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
		if err := postgresadapter.SeedDev(ctx, pool); err != nil {
			return fmt.Errorf("dev seed: %w", err)
		}
	}

	// --- transient hold store ----------------------------------------------------
	redisClient := redisadapter.NewClient(cfg.Redis)
	defer func() { _ = redisClient.Close() }()

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

	// Middleware chain, applied innermost-first so the OUTER order matches
	// CLAUDE.md §5: recovery → request ID → logging → handler.
	// (Tracing, CORS, rate limiting, and auth join in later milestones.)
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
