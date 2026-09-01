// Command api is the booking service's HTTP entrypoint.
//
// This file is the composition root and nothing else: it loads config,
// builds dependencies, wires middleware in the documented order, and hands
// control to the server package. Business logic never lives here — that
// keeps the wiring readable and the use cases testable without HTTP.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	httpadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/http"
	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	redisadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/redis"
	appauth "github.com/nathanfabio/bookingConcurrent/internal/application/auth"
	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	appcatalog "github.com/nathanfabio/bookingConcurrent/internal/application/catalog"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/logger"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/middleware"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/server"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/telemetry"
)

func main() {
	if err := run(); err != nil {
		// Config errors land here before a logger exists; everything after
		// logger setup logs through slog and returns nil on clean shutdown.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Fail fast: a misconfigured process must not boot (CLAUDE.md §10).
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logger.New(cfg.Env, cfg.Log.Level)
	ctx := logger.WithContext(context.Background(), log)

	// SIGTERM/SIGINT cancel the context; server.Run drains and exits.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.Options{
		Enabled:     cfg.Telemetry.Enabled,
		ServiceName: cfg.ServiceName,
		Environment: string(cfg.Env),
	})
	if err != nil {
		return fmt.Errorf("telemetry setup: %w", err)
	}

	// --- durable store ---------------------------------------------------------
	pool, err := postgresadapter.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	// --- auth (M3) -------------------------------------------------------------
	// The argon2id hasher serves both the dev seed (below) and the live
	// service. Production parameters — login latency is a feature, not a
	// bug (ADR 0005). The issuer signs/valids the short-lived JWT access
	// tokens; refresh tokens never pass through it (ADR 0004).
	hasher := appauth.NewArgon2Hasher(appauth.DefaultArgon2Params)
	issuer := appauth.NewTokenIssuer(cfg.Auth.JWTSecret, cfg.ServiceName, cfg.Auth.AccessTokenTTL)
	authService := appauth.NewService(
		postgresadapter.NewUserRepo(pool),
		postgresadapter.NewRefreshTokenRepo(pool),
		hasher, issuer,
		cfg.Auth.AccessTokenTTL, cfg.Auth.RefreshTokenTTL,
	)
	secureCookies := cfg.Env.IsProduction()

	if cfg.Env == config.EnvDevelopment {
		// Dev-only convenience (CLAUDE.md §2): auto-migrate at boot so a dev
		// box is never silently behind. CI/production migrate explicitly via
		// `go run ./cmd/migrate up`.
		applied, err := postgresadapter.MigrateUp(ctx, cfg.Postgres)
		if err != nil {
			return fmt.Errorf("auto-migrate: %w", err)
		}
		if applied > 0 {
			log.Info("dev auto-migration applied migrations", "count", applied)
		}
		if err := postgresadapter.SeedDev(ctx, pool, hasher.Hash); err != nil {
			return fmt.Errorf("dev seed: %w", err)
		}
	}

	// --- transient hold store ----------------------------------------------------
	redisClient := redisadapter.NewClient(cfg.Redis)
	defer func() { _ = redisClient.Close() }()

	// --- booking + catalog (M4) ---------------------------------------------------
	// Holds live in Redis (Lua-atomic, TTL-bound, ADR 0002); confirmed
	// bookings live in Postgres, where the partial unique index is the
	// arbiter (ADR 0006). The service glues the two under the consistency
	// model ADR 0006 spells out: Postgres commits first, Redis cleanup is
	// best-effort after.
	holdStore := redisadapter.NewHoldStore(redisClient, cfg.Redis.HoldTTL, cfg.Booking.MaxActiveHolds)
	bookingService := appbooking.NewService(
		holdStore,
		postgresadapter.NewBookingRepo(pool),
		postgresadapter.NewScreeningRepo(pool),
		cfg.Redis.HoldTTL, time.Now,
	)
	catalogService := appcatalog.NewService(
		postgresadapter.NewMovieRepo(pool),
		postgresadapter.NewScreeningRepo(pool),
	)

	// --- routes ------------------------------------------------------------------
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpadapter.HealthzHandler())
	// Readiness checks the two stores this process cannot live without.
	// The broker is deliberately NOT here (from M6 onward): the outbox is
	// precisely the buffer that lets the API keep taking bookings while the
	// broker is down, so failing readiness on broker trouble would be wrong.
	mux.Handle("GET /readyz", httpadapter.ReadyzHandler([]httpadapter.ReadinessCheck{
		{Name: "postgres", Check: pool.Ping},
		{Name: "redis", Check: func(ctx context.Context) error {
			return redisClient.Ping(ctx).Err()
		}},
	}))

	// --- auth routes (M3) --------------------------------------------------------
	// register/login/refresh/logout are PUBLIC: they are how you obtain or
	// recover credentials. /auth/me is the one PROTECTED route — it wraps
	// the handler in the auth middleware so the user ID comes from the
	// verified token, never the request body (CLAUDE.md §3). Auth is
	// applied per route; the booking routes below use the same wrapper.
	validate := func(ctx context.Context, token string) (string, error) {
		return issuer.ValidateAccessToken(token)
	}
	authMiddleware := middleware.Auth(validate, httpadapter.WriteUnauthorized)

	mux.Handle("POST /auth/register", httpadapter.RegisterHandler(authService, cfg.Auth.RefreshTokenTTL, secureCookies))
	mux.Handle("POST /auth/login", httpadapter.LoginHandler(authService, cfg.Auth.RefreshTokenTTL, secureCookies))
	mux.Handle("POST /auth/refresh", httpadapter.RefreshHandler(authService, cfg.Auth.RefreshTokenTTL, secureCookies))
	mux.Handle("POST /auth/logout", httpadapter.LogoutHandler(authService, secureCookies))
	mux.Handle("GET /auth/me", authMiddleware(httpadapter.MeHandler(authService)))

	// --- catalog + booking routes (M4) -------------------------------------------
	// PUBLIC reads: browsing the catalog and the seat map never needs auth —
	// availability is browse-before-login data. The seat map is
	// screening-scoped because seat state only exists per screening
	// (ADR 0006 records the deliberate deviation from CLAUDE.md §4's
	// /movies/{id}/seats wording).
	mux.Handle("GET /movies", httpadapter.ListMoviesHandler(catalogService))
	mux.Handle("GET /movies/{movieID}/screenings", httpadapter.ListScreeningsHandler(catalogService))
	mux.Handle("GET /screenings/{screeningID}/seats", httpadapter.SeatMapHandler(bookingService))

	// PROTECTED writes: the user ID comes from the verified token via the
	// auth middleware — NEVER from a request body or path (CLAUDE.md §3).
	// Confirm/release on someone else's session answer the same 404 as an
	// unknown one, so session IDs cannot be probed.
	mux.Handle("POST /holds", authMiddleware(httpadapter.HoldHandler(bookingService)))
	mux.Handle("POST /holds/{sessionID}/confirm", authMiddleware(httpadapter.ConfirmHandler(bookingService)))
	mux.Handle("DELETE /holds/{sessionID}", authMiddleware(httpadapter.ReleaseHandler(bookingService)))

	// Middleware chain, applied innermost-first so the OUTER order matches
	// CLAUDE.md §5: recovery → request ID → logging → handler.
	// Auth is applied per-route above (M3); tracing, CORS, and rate
	// limiting join in later milestones.
	var handler http.Handler = mux
	handler = middleware.Logging(handler)
	handler = middleware.RequestID(handler)
	handler = middleware.Recovery(httpadapter.WriteInternalError)(handler)

	log.Info("api starting",
		"addr", cfg.HTTP.Addr,
		"env", string(cfg.Env),
		"service", cfg.ServiceName,
	)

	err = server.Run(ctx, log, handler, server.Options{
		Addr:            cfg.HTTP.Addr,
		ReadTimeout:     cfg.HTTP.ReadTimeout,
		WriteTimeout:    cfg.HTTP.WriteTimeout,
		ShutdownTimeout: cfg.HTTP.ShutdownTimeout,
	})

	// Flush spans regardless of exit path; a short deadline keeps shutdown
	// snappy without silently dropping the last traces.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if tErr := shutdownTelemetry(shutdownCtx); tErr != nil {
		log.Error("telemetry shutdown", "error", tErr)
	}

	return err
}
