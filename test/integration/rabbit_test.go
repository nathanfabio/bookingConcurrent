package integration

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	messagingadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/messaging"
	appnotification "github.com/nathanfabio/bookingConcurrent/internal/application/notification"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/telemetry"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
)

// rabbitURL returns the broker URL for tests: RABBITMQ_URL when set (CI
// pins its own), else the compose dev default. Test files are exempt from
// the no-connection-strings lint guard — these are throwaway local
// credentials, same posture as pgTestConfig.
func rabbitURL() string {
	if v := os.Getenv("RABBITMQ_URL"); v != "" {
		return v
	}
	return "amqp://booking:booking-dev-password@localhost:5672/booking"
}

// connectRabbit dials the test broker or skips the test — the same
// infra-optional pattern as connectPostgres and connectRedis, so
// `go test ./...` stays green on a laptop without `make up`.
//
// It also installs the REAL telemetry pipeline (spans to io.Discard):
// production configures the global tracer provider and the TraceContext
// propagator through telemetry.Setup at boot, and the publisher's header
// injection only writes a traceparent when a sampled span context exists —
// with otel's default no-op provider it silently would not, and the
// cross-broker tracing assertion would test nothing.
func connectRabbit(t *testing.T) *amqp.Connection {
	t.Helper()

	shutdown, err := telemetry.Setup(context.Background(), telemetry.Options{
		Enabled:     true,
		ServiceName: "integration-test",
		Environment: "test",
		Writer:      io.Discard,
	})
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	conn, err := amqp.Dial(rabbitURL())
	if err != nil {
		infraUnavailable(t, "RabbitMQ not reachable — start infra with `make up`: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// testTopology builds a FULLY ISOLATED topology for one test: a unique
// exchange + queue per test, declared through the production DeclareTopology
// code path (so the adapter's declare logic is what's under test). Isolation
// matters twice over: tests never consume each other's events, and a dev
// worker running against the same broker never sees test traffic (its
// consumer sits on the production queue, bound to the production exchange).
// Cleanup deletes both objects best-effort.
func testTopology(t *testing.T, conn *amqp.Connection) messagingadapter.Topology {
	t.Helper()
	id := uuid.NewString()[:8]
	top := messagingadapter.Topology{
		Exchange:   "test." + id + ".events",
		Queue:      "test." + id + ".notifications",
		BindingKey: appnotification.EventTypeBookingConfirmed,
	}

	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("open channel for topology: %v", err)
	}
	defer func() { _ = ch.Close() }()
	if err := messagingadapter.DeclareTopology(ch, top); err != nil {
		t.Fatalf("declare test topology: %v", err)
	}

	t.Cleanup(func() {
		dch, err := conn.Channel()
		if err != nil {
			return // connection already gone; broker vhost cleanup is manual
		}
		defer func() { _ = dch.Close() }()
		_, _ = dch.QueueDelete(top.Queue, false, false, false)
		_ = dch.ExchangeDelete(top.Exchange, false, false)
	})
	return top
}

// startDrain subscribes to the queue for the REST of the test and returns
// the delivery channel. ONE channel per test is load-bearing: with autoAck
// and no Qos limit the broker eagerly pushes the whole queue to the first
// consumer, so a helper that opened a fresh channel per expected delivery
// would silently DESTROY every message after the first when its channel
// closed (server-side auto-acked, client-side buffered, never read).
func startDrain(t *testing.T, conn *amqp.Connection, queue string) <-chan amqp.Delivery {
	t.Helper()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("open consume channel: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	deliveries, err := ch.Consume(queue, "", true, false, false, false, nil)
	if err != nil {
		t.Fatalf("subscribe to %q: %v", queue, err)
	}
	return deliveries
}

// expectDelivery waits for one delivery on a drain started by startDrain.
// Publisher confirms mean a confirmed publish is already queued, so a
// timeout here is a real failure, not flakiness.
func expectDelivery(t *testing.T, deliveries <-chan amqp.Delivery, timeout time.Duration) amqp.Delivery {
	t.Helper()
	select {
	case d, ok := <-deliveries:
		if !ok {
			t.Fatal("delivery channel closed before a message arrived")
		}
		return d
	case <-time.After(timeout):
		t.Fatalf("no delivery within %s", timeout)
		return amqp.Delivery{} // unreachable; keeps the compiler happy
	}
}

// expectNoDelivery fails if a delivery arrives within the window — the
// "nothing republished" assertion. The window is deliberately short: it
// guards against a second publish, not against broker latency.
func expectNoDelivery(t *testing.T, deliveries <-chan amqp.Delivery, window time.Duration) {
	t.Helper()
	select {
	case d := <-deliveries:
		t.Errorf("unexpected delivery (message_id=%q) within %s — expected a quiet queue", d.MessageId, window)
	case <-time.After(window):
	}
}

// resetMessagingTables truncates the two tables the messaging pipeline
// owns end-to-end. The test database is shared and long-lived, so WITHOUT
// this the relay — which is global by design (ListPending has no scope
// parameter; production has exactly one outbox) — would publish the whole
// accumulated backlog of previous runs' confirms, and the dedupe assertions
// would count other tests' rows. Truncating at test start gives each relay
// test the empty-table precondition production starts from; safe because
// integration tests run sequentially and every other test asserts on its
// own rows immediately after writing them.
func resetMessagingTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `TRUNCATE outbox, processed_events`); err != nil {
		t.Fatalf("truncate messaging tables: %v", err)
	}
}

// withRabbitContext is a convenience for the sub-tests: a background ctx
// with a hard timeout so a hung broker fails the test instead of the CI job.
func withRabbitContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
