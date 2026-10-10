// External test package: these tests drive the Handler through the
// in-memory fakes (adapters/memory). Test files are exempt from the
// application-purity depguard rule for exactly this reason (CLAUDE.md §9).
package notification_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nathanfabio/bookingConcurrent/internal/adapters/memory"
	appnotification "github.com/nathanfabio/bookingConcurrent/internal/application/notification"
)

var confirmedAt = time.Date(2026, 10, 3, 18, 30, 0, 0, time.UTC)

// wireEvent builds a realistic delivery: a fresh event id and a payload
// encoded from the consumer's OWN struct — the same bytes the writer's
// bookingConfirmedPayload produces (both sides are snake_case-pinned by
// reflection tests; docs/api.md is the contract between them).
func wireEvent(t *testing.T) (eventID string, payload []byte, want appnotification.BookingConfirmed) {
	t.Helper()
	want = appnotification.BookingConfirmed{
		BookingID:   uuid.NewString(),
		SessionID:   uuid.NewString(),
		ScreeningID: uuid.NewString(),
		Row:         "C",
		Number:      7,
		UserID:      uuid.NewString(),
		ConfirmedAt: confirmedAt,
	}
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return uuid.NewString(), payload, want
}

func TestHandlerFirstDeliveryDecodesAndNotifies(t *testing.T) {
	ctx := context.Background()
	processed := memory.NewProcessedEvents()
	notifier := memory.NewNotifier()
	h := appnotification.NewHandler(processed, notifier)

	eventID, payload, want := wireEvent(t)
	if err := h.HandleBookingConfirmed(ctx, eventID, payload); err != nil {
		t.Fatalf("HandleBookingConfirmed: %v", err)
	}

	if processed.Count() != 1 || !processed.Has(eventID) {
		t.Errorf("claim table = %d rows (has=%v), want exactly the delivered event claimed",
			processed.Count(), processed.Has(eventID))
	}
	calls := notifier.Calls()
	if len(calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(calls))
	}
	// Every decoded field must survive the wire: a tag typo would silently
	// zero one of these.
	got := calls[0]
	if got != want {
		t.Errorf("decoded event = %+v, want %+v", got, want)
	}
}

func TestHandlerDuplicateDeliverySkipped(t *testing.T) {
	ctx := context.Background()
	processed := memory.NewProcessedEvents()
	notifier := memory.NewNotifier()
	h := appnotification.NewHandler(processed, notifier)

	eventID, payload, _ := wireEvent(t)
	for i := range 3 {
		if err := h.HandleBookingConfirmed(ctx, eventID, payload); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if notifier.Attempts() != 1 {
		t.Errorf("notifier attempts = %d, want 1 (duplicates must skip the side effect)", notifier.Attempts())
	}
	if processed.Count() != 1 {
		t.Errorf("claim rows = %d, want 1", processed.Count())
	}
}

// TestConcurrentDuplicateDeliveriesNotifyOnce is the unit-level
// exactly-once-EFFECT proof: at-least-once delivery means the broker can
// hand the same event_id to many in-flight goroutines at once; the claim's
// atomic check-and-set (mutex here, ON CONFLICT in Postgres — pinned
// against the real database in test/integration) must admit exactly one.
func TestConcurrentDuplicateDeliveriesNotifyOnce(t *testing.T) {
	ctx := context.Background()
	processed := memory.NewProcessedEvents()
	notifier := memory.NewNotifier()
	h := appnotification.NewHandler(processed, notifier)

	eventID, payload, _ := wireEvent(t)

	const goroutines = 32
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			errs[i] = h.HandleBookingConfirmed(ctx, eventID, payload)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	if notifier.Attempts() != 1 {
		t.Errorf("notifier attempts = %d, want exactly 1 under %d racing deliveries",
			notifier.Attempts(), goroutines)
	}
	if processed.Count() != 1 {
		t.Errorf("claim rows = %d, want 1", processed.Count())
	}
}

func TestHandlerMalformedEventIDIsAckedNotRetried(t *testing.T) {
	ctx := context.Background()
	processed := memory.NewProcessedEvents()
	notifier := memory.NewNotifier()
	h := appnotification.NewHandler(processed, notifier)

	_, payload, _ := wireEvent(t)
	// A non-UUID id can never become processable: redelivery would spin
	// forever, and the UUID-typed claim column would reject it anyway.
	if err := h.HandleBookingConfirmed(ctx, "not-a-uuid", payload); err != nil {
		t.Fatalf("malformed id must ack (return nil), got %v", err)
	}
	if processed.Count() != 0 || notifier.Attempts() != 0 {
		t.Errorf("malformed id touched claim(%d)/notifier(%d), want neither",
			processed.Count(), notifier.Attempts())
	}
}

func TestHandlerMalformedPayloadIsAckedWithoutClaim(t *testing.T) {
	ctx := context.Background()
	processed := memory.NewProcessedEvents()
	notifier := memory.NewNotifier()
	h := appnotification.NewHandler(processed, notifier)

	// Decode BEFORE claim: garbage must not burn a claim row.
	if err := h.HandleBookingConfirmed(ctx, uuid.NewString(), []byte(`{"booking_id":`)); err != nil {
		t.Fatalf("malformed payload must ack (return nil), got %v", err)
	}
	if processed.Count() != 0 || notifier.Attempts() != 0 {
		t.Errorf("malformed payload touched claim(%d)/notifier(%d), want neither",
			processed.Count(), notifier.Attempts())
	}
}

func TestHandlerClaimErrorIsTransientAndRetriable(t *testing.T) {
	ctx := context.Background()
	processed := memory.NewProcessedEvents()
	notifier := memory.NewNotifier()
	h := appnotification.NewHandler(processed, notifier)

	eventID, payload, _ := wireEvent(t)
	processed.FailNextClaim(errors.New("postgres hiccup"))

	// An errored claim must NOT be treated as a duplicate: the error
	// surfaces so the broker nacks+requeues.
	if err := h.HandleBookingConfirmed(ctx, eventID, payload); err == nil {
		t.Fatal("claim failure must propagate (nack+requeue), got nil")
	}
	if notifier.Attempts() != 0 {
		t.Errorf("notifier ran despite a failed claim (attempts=%d)", notifier.Attempts())
	}

	// The redelivery claims successfully and notifies exactly once.
	if err := h.HandleBookingConfirmed(ctx, eventID, payload); err != nil {
		t.Fatalf("redelivery after transient claim failure: %v", err)
	}
	if notifier.Attempts() != 1 {
		t.Errorf("notifier attempts after redelivery = %d, want 1", notifier.Attempts())
	}
}

// TestHandlerNotifyFailureLosesNotification pins the documented AT-MOST-ONCE
// consequence of claim-then-notify (ADR 0009): the claim survives the notify
// failure, so the redelivery is skipped as a duplicate and the notifier is
// never invoked again. Correct for the M6 log stub; a real notifier must
// invert the order or be idempotent itself — this test exists so that change
// is a deliberate act, not a silent regression.
func TestHandlerNotifyFailureLosesNotification(t *testing.T) {
	ctx := context.Background()
	processed := memory.NewProcessedEvents()
	notifier := memory.NewNotifier()
	h := appnotification.NewHandler(processed, notifier)

	eventID, payload, _ := wireEvent(t)
	notifier.FailNext(1, errors.New("smtp down"))

	if err := h.HandleBookingConfirmed(ctx, eventID, payload); err == nil {
		t.Fatal("notify failure must propagate (honest nack), got nil")
	}
	if notifier.Attempts() != 1 {
		t.Fatalf("attempts = %d, want 1 (the failed call)", notifier.Attempts())
	}

	// Redelivery: the claim row exists, so the handler skips — the
	// notification is LOST, by design, for a stub side effect.
	if err := h.HandleBookingConfirmed(ctx, eventID, payload); err != nil {
		t.Fatalf("redelivery after notify failure: %v", err)
	}
	if notifier.Attempts() != 1 {
		t.Errorf("attempts after redelivery = %d, want STILL 1 (claim-then-notify is at-most-once)",
			notifier.Attempts())
	}
	if notifier.CallCount() != 0 {
		t.Errorf("recorded calls = %d, want 0 (the notification never succeeded)", notifier.CallCount())
	}
}
