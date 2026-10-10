// Command worker is the booking system's background process: the outbox
// relay (Postgres → RabbitMQ), the BookingConfirmed consumer (RabbitMQ →
// dedupe → notification stub), and — from the sweeper milestone — the
// hold-expiry sweeper (ADR 0002's deferred token-checked cleanup).
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
	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	appnotification "github.com/nathanfabio/bookingConcurrent/internal/application/notification"
	appoutbox "github.com/nathanfabio/bookingConcurrent/internal/application/outbox"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/logger"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/telemetry"
	platformworker "github.com/nathanfabio/bookingConcurrent/internal/platform/worker"
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
		Enabled:     cfg.Telemetry.Enabled,
		ServiceName: cfg.ServiceName,
		Environment: string(cfg.Env),
	})
	if err != nil {
		return fmt.Errorf("telemetry setup: %w", err)
	}

	// --- durable store: FAIL FAST ------------------------------------------------
	// The relay reads the outbox and the consumer writes processed_events —
	// without Postgres this process has no purpose, so a dead database is a
	// boot failure (same posture as cmd/api), not a retry loop.
	pool, err := postgresadapter.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	if cfg.Env == config.EnvDevelopment {
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
	consumer, err := messagingadapter.NewConsumer(cfg.Broker.URL, topology, cfg.Broker.Prefetch)
	if err != nil {
		return fmt.Errorf("consumer: %w", err)
	}

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
				published, err := relay.RunOnce(ctx)
				if published > 0 {
					log.Info("outbox relay published events", slog.Int("count", published))
				}
				if err != nil && ctx.Err() == nil {
					// LOG AND CONTINUE: relay failures are always retryable
					// (rows stay pending), so this Tick never ends on error.
					// The ctx check keeps shutdown from logging one last
					// spurious "failure" when the in-flight publish aborts.
					log.Error("outbox relay pass failed; rows stay pending, retrying next tick",
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

	log.Info("worker starting",
		"service", cfg.ServiceName,
		"env", string(cfg.Env),
		"exchange", cfg.Broker.Exchange,
		"queue", cfg.Broker.Queue,
		"relay_interval", cfg.Worker.RelayPollInterval.String(),
	)

	runErr := platformworker.Run(ctx, log, relayJob, consumerJob)

	// Flush spans regardless of exit path; a short deadline keeps shutdown
	// snappy without silently dropping the last traces (cmd/api's pattern).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if tErr := shutdownTelemetry(shutdownCtx); tErr != nil {
		log.Error("telemetry shutdown", "error", tErr)
	}

	// platform/worker.Run already normalizes clean cancellation to nil;
	// anything left is a real failure worth a non-zero exit.
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}
