package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/memory"
	messagingadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/messaging"
	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	appnotification "github.com/nathanfabio/bookingConcurrent/internal/application/notification"
	appoutbox "github.com/nathanfabio/bookingConcurrent/internal/application/outbox"

	amqp "github.com/rabbitmq/amqp091-go"
)

// failFirstMark decorates the REAL outbox repo and fails exactly the first
// MarkPublished call — the integration-level lever for the relay's
// publish-then-mark crash window (ADR 0009): the broker has the event, the
// table says it doesn't, and the next tick MUST republish.
type failFirstMark struct {
	inner  appoutbox.OutboxStore
	failed bool
}

func (f *failFirstMark) ListPending(ctx context.Context, limit int) ([]appoutbox.Event, error) {
	return f.inner.ListPending(ctx, limit)
}

func (f *failFirstMark) MarkPublished(ctx context.Context, eventIDs []string) error {
	if !f.failed {
		f.failed = true
		return errors.New("injected mark failure (simulated crash window)")
	}
	return f.inner.MarkPublished(ctx, eventIDs)
}

// TestRelayPublishesPendingOutboxToBroker is the end-to-end proof of the
// outbox's second half: a real Confirm (booking + outbox row in one
// transaction, ADR 0006) → real OutboxRepo → real RabbitMQ Publisher with
// confirms → the durable queue carries the event with the outbox event_id
// as its MessageId → the row is stamped published → a second pass is quiet.
func TestRelayPublishesPendingOutboxToBroker(t *testing.T) {
	ctx := withRabbitContext(t)
	f := newBookingFixture(t, 1)
	conn := connectRabbit(t)
	top := testTopology(t, conn)
	resetMessagingTables(t, f.pool)
	drain := startDrain(t, conn, top.Queue)

	session := paidSession(t, f.pool)
	created, err := f.repo.Confirm(ctx, f.booking(session, f.users[0], "A", 2))
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	// The row starts pending (published_at NULL) — the state M4/M5 left
	// every confirmed booking in, by design.
	var eventID string
	if err := f.queryRow(ctx,
		"SELECT event_id FROM outbox WHERE payload->>'session_id' = $1 AND published_at IS NULL",
		session).Scan(&eventID); err != nil {
		t.Fatalf("read pending outbox row: %v", err)
	}

	publisher, err := messagingadapter.NewPublisher(rabbitURL(), top, "integration-test")
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer func() { _ = publisher.Close() }()
	relay := appoutbox.NewRelay(postgresadapter.NewOutboxRepo(f.pool), publisher, 10)

	published, err := relay.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if published != 1 {
		t.Errorf("published = %d, want 1", published)
	}

	// The row is now stamped — the confirm arrived before the mark.
	var marked bool
	if err := f.queryRow(ctx,
		"SELECT published_at IS NOT NULL FROM outbox WHERE event_id = $1", eventID).
		Scan(&marked); err != nil {
		t.Fatalf("re-read outbox row: %v", err)
	}
	if !marked {
		t.Error("published_at still NULL after a confirmed publish")
	}

	d := expectDelivery(t, drain, 5*time.Second)
	if d.MessageId != eventID {
		t.Errorf("delivery MessageId = %q, want the outbox event_id %q", d.MessageId, eventID)
	}
	if d.Type != appnotification.EventTypeBookingConfirmed {
		t.Errorf("delivery Type = %q, want %q", d.Type, appnotification.EventTypeBookingConfirmed)
	}
	if d.RoutingKey != appnotification.EventTypeBookingConfirmed {
		t.Errorf("routing key = %q, want identity mapping to the event type", d.RoutingKey)
	}
	if d.DeliveryMode != amqp.Persistent {
		t.Errorf("delivery mode = %d, want persistent (2)", d.DeliveryMode)
	}
	if d.ContentType != "application/json" {
		t.Errorf("content type = %q, want application/json", d.ContentType)
	}
	// Trace context must ride through the broker (CLAUDE.md §8: trace the
	// publish/consume path) — the propagator injects a traceparent header.
	if tp, _ := d.Headers["traceparent"].(string); tp == "" {
		t.Error("delivery carries no traceparent header; cross-broker tracing is broken")
	}
	// The payload is the writer's contract, byte-level: decode into the
	// CONSUMER's struct (the duplicated half) and check the identifiers.
	var evt appnotification.BookingConfirmed
	if err := json.Unmarshal(d.Body, &evt); err != nil {
		t.Fatalf("delivery body is not the contract JSON: %v (%s)", err, d.Body)
	}
	if evt.BookingID != created.ID || evt.SessionID != session ||
		evt.ScreeningID != f.screening.ID || evt.UserID != f.users[0] ||
		evt.Row != "A" || evt.Number != 2 {
		t.Errorf("decoded event = %+v, want booking %q session %q seat A2", evt, created.ID, session)
	}

	// Second pass: everything is stamped, nothing republishes.
	published, err = relay.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if published != 0 {
		t.Errorf("second pass published = %d, want 0", published)
	}
	expectNoDelivery(t, drain, 300*time.Millisecond)
}

// TestRelayRepublishOnMarkFailureYieldsOneNotification is THE marquee test
// of the messaging milestone: it walks the full at-least-once → exactly-
// once-EFFECT pipeline against real Postgres and a real broker.
//
//  1. Confirm writes booking + outbox row (one transaction).
//  2. Relay pass #1 publishes (broker confirms) but the mark FAILS —
//     the crash window ADR 0009 says must be safe.
//  3. Relay pass #2 republishes the same event (a duplicate, by design).
//  4. BOTH deliveries go through the real Handler + real ProcessedEventRepo:
//     the claim table admits exactly one, the notifier fires exactly once.
func TestRelayRepublishOnMarkFailureYieldsOneNotification(t *testing.T) {
	ctx := withRabbitContext(t)
	f := newBookingFixture(t, 1)
	conn := connectRabbit(t)
	top := testTopology(t, conn)
	resetMessagingTables(t, f.pool)
	drain := startDrain(t, conn, top.Queue)

	session := paidSession(t, f.pool)
	if _, err := f.repo.Confirm(ctx, f.booking(session, f.users[0], "B", 1)); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	publisher, err := messagingadapter.NewPublisher(rabbitURL(), top, "integration-test")
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer func() { _ = publisher.Close() }()
	store := &failFirstMark{inner: postgresadapter.NewOutboxRepo(f.pool)}
	relay := appoutbox.NewRelay(store, publisher, 10)

	// Pass #1: published to the broker, mark fails, RunOnce reports failure
	// and 0 completed rows.
	if n, err := relay.RunOnce(ctx); err == nil {
		t.Fatalf("pass #1 must surface the injected mark failure, got (n=%d, nil)", n)
	}
	// Pass #2: the row is still pending, so the relay republishes it.
	n, err := relay.RunOnce(ctx)
	if err != nil {
		t.Fatalf("pass #2: %v", err)
	}
	if n != 1 {
		t.Errorf("pass #2 published = %d, want 1", n)
	}

	// Two deliveries now sit on the queue — same event, same MessageId.
	d1 := expectDelivery(t, drain, 5*time.Second)
	d2 := expectDelivery(t, drain, 5*time.Second)
	if d1.MessageId == "" || d1.MessageId != d2.MessageId {
		t.Fatalf("deliveries are not duplicates of one event: %q vs %q", d1.MessageId, d2.MessageId)
	}

	notifier := memory.NewNotifier()
	handler := appnotification.NewHandler(
		postgresadapter.NewProcessedEventRepo(f.pool), notifier)

	if err := handler.HandleBookingConfirmed(ctx, d1.MessageId, d1.Body); err != nil {
		t.Errorf("first delivery: %v", err)
	}
	if err := handler.HandleBookingConfirmed(ctx, d2.MessageId, d2.Body); err != nil {
		t.Errorf("duplicate delivery must be skipped, not errored: %v", err)
	}

	if calls := notifier.CallCount(); calls != 1 {
		t.Errorf("notifier fired %d times for one logical event, want exactly 1", calls)
	} else if got := notifier.Calls()[0]; got.SessionID != session {
		t.Errorf("notified session = %q, want %q", got.SessionID, session)
	}
	var claims int
	if err := f.queryRow(ctx, "SELECT count(*) FROM processed_events").Scan(&claims); err != nil {
		t.Fatalf("count processed_events: %v", err)
	}
	if claims != 1 {
		t.Errorf("processed_events rows = %d, want 1 (the claim is the dedupe)", claims)
	}
}

// TestConsumerDeliversToHandlerAndAcks exercises the messaging.Consumer
// adapter itself against the real broker: dial, Qos, declare, subscribe,
// deliver to the handler, ACK, and shut down cleanly on ctx cancel.
func TestConsumerDeliversToHandlerAndAcks(t *testing.T) {
	ctx := withRabbitContext(t)
	f := newBookingFixture(t, 1)
	conn := connectRabbit(t)
	top := testTopology(t, conn)
	resetMessagingTables(t, f.pool)

	session := paidSession(t, f.pool)
	if _, err := f.repo.Confirm(ctx, f.booking(session, f.users[0], "C", 3)); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	publisher, err := messagingadapter.NewPublisher(rabbitURL(), top, "integration-test")
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer func() { _ = publisher.Close() }()
	relay := appoutbox.NewRelay(postgresadapter.NewOutboxRepo(f.pool), publisher, 10)
	if _, err := relay.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	consumer, err := messagingadapter.NewConsumer(rabbitURL(), top, 10)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	notifier := memory.NewNotifier()
	handler := appnotification.NewHandler(
		postgresadapter.NewProcessedEventRepo(f.pool), notifier)

	consumeCtx, cancelConsume := context.WithCancel(ctx)
	consumeErr := make(chan error, 1)
	var once sync.Once
	handled := make(chan struct{})
	go func() {
		consumeErr <- consumer.Consume(consumeCtx, func(ctx context.Context, d messagingadapter.Delivery) error {
			err := handler.HandleBookingConfirmed(ctx, d.EventID, d.Body)
			if err == nil {
				once.Do(func() { close(handled) })
			}
			return err
		})
	}()

	select {
	case <-handled:
	case <-time.After(10 * time.Second):
		t.Fatal("consumer never delivered the event to the handler")
	}
	if notifier.CallCount() != 1 {
		t.Errorf("notifier calls = %d, want 1", notifier.CallCount())
	}

	// The handler succeeded, so the message must end up ACKed: poll the
	// queue depth (passive declare) until it drains. An unacked message
	// would sit there as "unacked" and reappear after the channel closes.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ch, err := conn.Channel()
		if err != nil {
			t.Fatalf("channel for queue inspection: %v", err)
		}
		q, err := ch.QueueDeclarePassive(top.Queue, true, false, false, false, nil)
		_ = ch.Close()
		if err != nil {
			t.Fatalf("passive declare: %v", err)
		}
		if q.Messages == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue still holds %d message(s) — the consumer never acked", q.Messages)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Clean shutdown: cancel → Consume returns nil, not an error.
	cancelConsume()
	select {
	case err := <-consumeErr:
		if err != nil {
			t.Errorf("Consume after cancel = %v, want nil (clean shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Consume did not return after context cancellation")
	}

	// The claim row is the durable trace of the side effect.
	var claims int
	if err := f.queryRow(ctx, "SELECT count(*) FROM processed_events").Scan(&claims); err != nil {
		t.Fatalf("count processed_events: %v", err)
	}
	if claims != 1 {
		t.Errorf("processed_events rows = %d, want 1", claims)
	}
}
