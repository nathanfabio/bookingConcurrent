// Command worker is the booking system's background process: the outbox
// relay (Postgres → RabbitMQ), the BookingConfirmed consumer (RabbitMQ →
// dedupe → notification stub), and the hold-expiry sweeper (ADR 0002's
// deferred token-checked cleanup, landed in ADR 0010).
//
// Like cmd/api, this file is the composition root and nothing else: config,
// dependencies, wiring, supervision. The job MECHANICS live in platform/
// worker; the error POLICY per job lives in the closures below, because
// "what should happen when this job fails" is a wiring decision, not a
// library one.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	messagingadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/messaging"
	notificationadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/notification"
	paymentadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/payment"
	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	redisadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/redis"
	appholdexpiry "github.com/nathanfabio/bookingConcurrent/internal/application/holdexpiry"
	appnotification "github.com/nathanfabio/bookingConcurrent/internal/application/notification"
	appoutbox "github.com/nathanfabio/bookingConcurrent/internal/application/outbox"
	"github.com/nathanfabio/bookingConcurrent/internal/application/refund"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/logger"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/telemetry"
	platformworker "github.com/nathanfabio/bookingConcurrent/internal/platform/worker"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
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
	args := os.Args[1:]
	withoutConsumer := len(args) == 1 && args[0] == "--without-consumer"
	var replayID string
	if len(args) == 2 && args[0] == "republish" {
		id, err := uuid.Parse(args[1])
		if err != nil {
			return fmt.Errorf("invalid event UUID: %w", err)
		}
		replayID = id.String()
	} else if len(args) > 0 && !withoutConsumer {
		return errors.New("usage: worker [--without-consumer | republish EVENT_UUID]")
	}
	// Fail fast: a misconfigured process must not boot (CLAUDE.md §10).
	// config.Load is SHARED with the API, so the worker also validates
	// HTTP_ADDR / auth / payment settings it never uses — the accepted
	// trade-off documented on config.Worker (one validated config beats
	// per-binary loaders at this scale).
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logger.New(cfg.Env, cfg.Log.Level)
	// Application and adapter code logs through slog.*Context, which routes
	// to the DEFAULT logger — make the configured one (env-driven level and
	// format) the process default, or every log line below silently bypasses
	// LOG_LEVEL and the dev/prod format switch.
	slog.SetDefault(log)
	ctx := logger.WithContext(context.Background(), log)

	// SIGTERM/SIGINT cancel the context; platform/worker.Run drains the
	// jobs and exits 0 (the orchestrator's "clean stop" signal).
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.Setup(ctx, telemetry.Options{
		Enabled:       cfg.Telemetry.Enabled,
		ServiceName:   cfg.ServiceName,
		Environment:   string(cfg.Env),
		Exporter:      cfg.Telemetry.Exporter,
		Endpoint:      cfg.Telemetry.OTLPEndpoint,
		SampleRatio:   &cfg.Telemetry.SampleRatio,
		ExportTimeout: cfg.Telemetry.ExportTimeout,
	})
	if err != nil {
		return fmt.Errorf("telemetry setup: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			log.Error("telemetry shutdown", "error", err)
		}
	}()

	// --- durable store: FAIL FAST ------------------------------------------------
	// The relay reads the outbox and the consumer writes processed_events —
	// without Postgres this process has no purpose, so a dead database is a
	// boot failure (same posture as cmd/api), not a retry loop.
	pool, err := postgresadapter.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	if cfg.Env == config.EnvDevelopment && replayID == "" {
		// Dev-only convenience, mirroring cmd/api: auto-migrate so a dev box
		// is never silently behind. Goose's version table makes a concurrent
		// api+worker first boot benign (whoever wins the migration, the other
		// sees it applied — idempotency pinned by TestMigrationsAreIdempotent).
		// The worker does NOT seed: browsing data is the API's dev concern.
		applied, err := postgresadapter.MigrateUp(ctx, cfg.Postgres)
		if err != nil {
			return fmt.Errorf("auto-migrate: %w", err)
		}
		if applied > 0 {
			log.Info("dev auto-migration applied migrations", "count", applied)
		}
	}

	// --- broker: RETRY, NEVER FAIL FAST -------------------------------------------
	// The boot-policy asymmetry is the outbox's whole point (ADR 0009): a
	// broker outage is survivable — events buffer in Postgres and publish
	// when it returns. A worker that crash-looped on a broker blip would
	// teach the opposite lesson. Both jobs below therefore treat the broker
	// as eventually-available: the publisher reconnects lazily inside
	// Publish, and the consumer job retries with capped backoff.
	topology := messagingadapter.Topology{
		Exchange: cfg.Broker.Exchange,
		Queue:    cfg.Broker.Queue,
		// Identity routing (ADR 0009): the queue binds the literal event
		// type the relay publishes as its routing key.
		BindingKey: appnotification.EventTypeBookingConfirmed,
	}
	publisher, err := messagingadapter.NewPublisher(cfg.Broker.URL, topology, cfg.ServiceName)
	if err != nil {
		return fmt.Errorf("publisher: %w", err) // malformed URL: a config error, no retry helps
	}
	defer func() { _ = publisher.Close() }()
	if replayID != "" {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		ctx, span := otel.Tracer("cmd/worker").Start(ctx, "manual outbox republication")
		defer span.End()
		delivery, err := postgresadapter.NewOutboxRepo(pool).InspectDelivery(ctx, replayID)
		if err != nil {
			return err
		}
		result, err := delivery.Republish(ctx, publisher)
		if err != nil {
			return err
		}
		log.InfoContext(ctx, "manual outbox republication", "event_id", replayID, "result", result)
		fmt.Println(result)
		return nil
	}
	consumer, err := messagingadapter.NewConsumer(cfg.Broker.URL, topology, cfg.Broker.Prefetch)
	if err != nil {
		return fmt.Errorf("consumer: %w", err)
	}

	// --- transient hold store (sweeper) ------------------------------------------------
	// Same constructor as the API, but the worker only ever calls
	// SweepExpired through the holdexpiry port: Redis TTL keeps
	// availability correct on its own (ADR 0006) — the sweeper reconciles
	// the bookkeeping ZSETs so hold-limit slots come back deterministically
	// instead of lingering until the next hold attempt (ADR 0002's deferred
	// "required component", ADR 0010).
	redisClient := redisadapter.NewClient(cfg.Redis)
	defer func() { _ = redisClient.Close() }()
	holdStore := redisadapter.NewHoldStore(redisClient, cfg.Redis.HoldTTL, cfg.Booking.MaxActiveHolds)
	sweeper := appholdexpiry.NewSweeper(holdStore, cfg.Worker.SweepBatchSize, time.Now)

	// --- jobs -----------------------------------------------------------------------
	relay := appoutbox.NewRelay(
		postgresadapter.NewOutboxRepo(pool),
		publisher,
		cfg.Worker.RelayBatchSize,
	)
	// The notification handler takes primitives (eventID, payload) so the
	// application packages never import each other; this closure is the
	// adapter between the messaging adapter's Delivery and the use case.
	handler := appnotification.NewHandler(
		postgresadapter.NewProcessedEventRepo(pool),
		notificationadapter.NewLogNotifier(),
	)

	relayJob := platformworker.Job{
		Name: "outbox-relay",
		Run: func(ctx context.Context) error {
			return platformworker.Tick(ctx, cfg.Worker.RelayPollInterval, func(ctx context.Context) error {
				ctx, span := otel.Tracer("cmd/worker").Start(ctx, "outbox relay")
				defer span.End()
				published, err := relay.RunOnce(ctx)
				if published > 0 {
					log.InfoContext(ctx, "outbox relay published events", slog.Int("count", published))
				}
				if err != nil && ctx.Err() == nil {
					span.SetStatus(codes.Error, "outbox relay failed")
					// LOG AND CONTINUE: relay failures are always retryable
					// (rows stay pending), so this Tick never ends on error.
					// The ctx check keeps shutdown from logging one last
					// spurious "failure" when the in-flight publish aborts.
					log.ErrorContext(ctx, "outbox relay pass failed; rows stay pending, retrying next tick",
						slog.String("error", err.Error()))
				}
				return nil
			})
		},
	}

	consumerJob := platformworker.Job{
		Name: "booking-consumer",
		Run: func(ctx context.Context) error {
			// Consume dials per call and returns on connection loss; this
			// loop is the retry policy, with capped exponential backoff so a
			// long outage degrades to a slow poll instead of a dial storm.
			const maxBackoff = 30 * time.Second
			backoff := time.Second
			for {
				started := time.Now()
				err := consumer.Consume(ctx, func(ctx context.Context, d messagingadapter.Delivery) error {
					return handler.HandleBookingConfirmed(ctx, d.EventID, d.Body)
				})
				if err == nil {
					return nil // clean shutdown (ctx canceled)
				}
				if ctx.Err() != nil {
					return nil // died BECAUSE of shutdown — still clean
				}
				// A connection that stayed up for a while before dying was
				// healthy: restart the backoff ladder instead of resuming
				// at 30s for a blip hours later.
				if time.Since(started) > time.Minute {
					backoff = time.Second
				}
				log.Error("consumer lost; reconnecting",
					slog.String("error", err.Error()),
					slog.Duration("backoff", backoff))
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, maxBackoff)
			}
		},
	}

	sweeperJob := platformworker.Job{
		Name: "hold-expiry-sweeper",
		Run: func(ctx context.Context) error {
			return platformworker.Tick(ctx, cfg.Worker.SweepInterval, func(ctx context.Context) error {
				ctx, span := otel.Tracer("cmd/worker").Start(ctx, "holds sweep")
				defer span.End()
				// LOG AND CONTINUE, same policy as the relay: a failed
				// sweep changes nothing durable — the members stay in the
				// score window and the next tick retries. When a pass hits
				// the limit, the remainder simply waits for the next tick;
				// an inner drain loop would blur the interval's meaning.
				swept, err := sweeper.SweepOnce(ctx)
				_ = swept // SweepOnce already logs non-zero counts
				if err != nil && ctx.Err() == nil {
					span.SetStatus(codes.Error, "hold expiry sweep failed")
					log.ErrorContext(ctx, "hold-expiry sweep failed; members stay queued, retrying next tick",
						slog.String("error", err.Error()))
				}
				return nil
			})
		},
	}

	processor := refund.Processor{
		Store:     postgresadapter.RefundRepo{Pool: pool},
		Gateway:   paymentadapter.SandboxRefundGateway{Pool: pool},
		BatchSize: cfg.Worker.RefundBatchSize,
	}
	refundJob := platformworker.Job{Name: "sandbox-refunds", Run: func(ctx context.Context) error {
		return platformworker.Tick(ctx, cfg.Worker.RefundPollInterval, func(ctx context.Context) error {
			ctx, span := otel.Tracer("cmd/worker").Start(ctx, "execute sandbox refunds")
			defer span.End()
			n, err := processor.RunOnce(ctx)
			if n > 0 {
				log.InfoContext(ctx, "sandbox refunds completed", "count", n)
			}
			if err != nil && ctx.Err() == nil {
				span.SetStatus(codes.Error, "refund execution failed")
				log.ErrorContext(ctx, "refund pass incomplete", "error", err)
			}
			return nil
		})
	}}

	log.Info("worker starting",
		"service", cfg.ServiceName,
		"env", string(cfg.Env),
		"exchange", cfg.Broker.Exchange,
		"queue", cfg.Broker.Queue,
		"relay_interval", cfg.Worker.RelayPollInterval.String(),
		"sweep_interval", cfg.Worker.SweepInterval.String(),
	)

	jobs := []platformworker.Job{relayJob, sweeperJob, refundJob}
	if !withoutConsumer {
		jobs = append(jobs, consumerJob)
	}
	runErr := platformworker.Run(ctx, log, jobs...)

	// platform/worker.Run already normalizes clean cancellation to nil;
	// anything left is a real failure worth a non-zero exit.
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}
