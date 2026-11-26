// External test package: these tests drive the Relay through the in-memory
// fakes (adapters/memory). Test files are exempt from the application-purity
// depguard rule for exactly this reason (CLAUDE.md §9).
package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nathanfabio/bookingConcurrent/internal/adapters/memory"
	appoutbox "github.com/nathanfabio/bookingConcurrent/internal/application/outbox"
)

// seedEvent builds a realistic pending event: a fresh UUID id, the one
// event type the system emits today, and a payload shaped like the writer's
// bookingConfirmedPayload (the relay must move it byte-identically — it
// never decodes, but the tests should carry wire-realistic bytes).
func seedEvent(t *testing.T, store *memory.OutboxStore) appoutbox.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"booking_id":   uuid.NewString(),
		"session_id":   uuid.NewString(),
		"screening_id": uuid.NewString(),
		"row":          "A",
		"number":       3,
		"user_id":      uuid.NewString(),
		"confirmed_at": time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	evt := appoutbox.Event{ID: uuid.NewString(), Type: "BookingConfirmed", Payload: payload}
	store.Seed(evt)
	return evt
}

func TestRelayPublishesAndMarksPending(t *testing.T) {
	ctx := context.Background()
	clock := memory.NewManualClock(time.Now())
	store := memory.NewOutboxStore(clock)
	pub := memory.NewEventPublisher()

	seeded := []appoutbox.Event{
		seedEvent(t, store), seedEvent(t, store), seedEvent(t, store),
	}
	relay := appoutbox.NewRelay(store, pub, 10)

	n, err := relay.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 3 {
		t.Errorf("published = %d, want 3", n)
	}

	// The envelope must arrive byte-identical: same ids, types, payloads,
	// in FIFO order.
	got := pub.Published()
	if len(got) != 3 {
		t.Fatalf("publisher recorded %d events, want 3", len(got))
	}
	for i, want := range seeded {
		if got[i].ID != want.ID || got[i].Type != want.Type {
			t.Errorf("event %d: got id=%q type=%q, want id=%q type=%q",
				i, got[i].ID, got[i].Type, want.ID, want.Type)
		}
		if string(got[i].Payload) != string(want.Payload) {
			t.Errorf("event %d: payload mutated in transit", i)
		}
	}

	// All rows marked; a second pass finds nothing and publishes nothing.
	if pending := store.PendingIDs(); len(pending) != 0 {
		t.Errorf("pending after relay = %v, want none", pending)
	}
	n, err = relay.RunOnce(ctx)
	if err != nil || n != 0 {
		t.Errorf("second RunOnce = (%d, %v), want (0, nil)", n, err)
	}
	if len(pub.Published()) != 3 {
		t.Errorf("second pass republished events: %d total, want 3", len(pub.Published()))
	}
}

func TestRelayStopsAtFirstPublishFailureMarksConfirmedPrefix(t *testing.T) {
	ctx := context.Background()
	store := memory.NewOutboxStore(memory.NewManualClock(time.Now()))
	pub := memory.NewEventPublisher()

	first := seedEvent(t, store)
	second := seedEvent(t, store)
	third := seedEvent(t, store)
	// The middle event poisons the batch: the relay must publish the first,
	// STOP (no hammering a broken broker), and still mark the confirmed
	// prefix — the first event is durable at the broker, leaving it pending
	// would republish it needlessly.
	pub.FailOn(second.ID, nil)

	relay := appoutbox.NewRelay(store, pub, 10)
	n, err := relay.RunOnce(ctx)
	if err == nil {
		t.Fatal("RunOnce with a failing publish must return an error")
	}
	if n != 1 {
		t.Errorf("published = %d, want 1 (the confirmed prefix)", n)
	}
	if ids := pub.PublishedIDs(); len(ids) != 1 || ids[0] != first.ID {
		t.Errorf("published ids = %v, want exactly [%s] (stop at first failure)", ids, first.ID)
	}
	if marked := store.PublishedIDs(); len(marked) != 1 || marked[0] != first.ID {
		t.Errorf("marked = %v, want exactly [%s]", marked, first.ID)
	}
	pending := store.PendingIDs()
	if len(pending) != 2 || pending[0] != second.ID || pending[1] != third.ID {
		t.Errorf("pending = %v, want [%s %s]", pending, second.ID, third.ID)
	}
}

// TestRelayMarkFailureRepublishesNextTick is the unit-level at-least-once
// proof: the broker confirms, the mark fails, the rows stay pending, and the
// NEXT tick publishes them AGAIN. Duplicates are the contract; consumers
// dedupe (handler tests pin the other half).
func TestRelayMarkFailureRepublishesNextTick(t *testing.T) {
	ctx := context.Background()
	store := memory.NewOutboxStore(memory.NewManualClock(time.Now()))
	pub := memory.NewEventPublisher()

	first := seedEvent(t, store)
	second := seedEvent(t, store)
	store.FailNextMark(errors.New("postgres hiccup"))

	relay := appoutbox.NewRelay(store, pub, 10)

	n, err := relay.RunOnce(ctx)
	if err == nil {
		t.Fatal("RunOnce must surface the mark failure")
	}
	if n != 0 {
		t.Errorf("published = %d, want 0 (nothing completed the publish+mark cycle)", n)
	}
	if pending := store.PendingIDs(); len(pending) != 2 {
		t.Fatalf("pending after failed mark = %v, want both rows still pending", pending)
	}

	// Next tick: republish everything (the broker sees duplicates) and the
	// mark succeeds this time.
	n, err = relay.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if n != 2 {
		t.Errorf("second pass published = %d, want 2", n)
	}
	ids := pub.PublishedIDs()
	want := []string{first.ID, second.ID, first.ID, second.ID}
	if len(ids) != len(want) {
		t.Fatalf("published ids = %v, want %v (each event published twice)", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("published ids = %v, want %v", ids, want)
		}
	}
	if pending := store.PendingIDs(); len(pending) != 0 {
		t.Errorf("pending after second pass = %v, want none", pending)
	}
}

func TestRelayEmptyPendingIsNoop(t *testing.T) {
	ctx := context.Background()
	store := memory.NewOutboxStore(memory.NewManualClock(time.Now()))
	pub := memory.NewEventPublisher()
	relay := appoutbox.NewRelay(store, pub, 10)

	n, err := relay.RunOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("RunOnce on empty outbox = (%d, %v), want (0, nil)", n, err)
	}
	if store.MarkCalls() != 0 {
		t.Errorf("MarkPublished called %d times on an empty batch, want 0 (no pointless UPDATE)", store.MarkCalls())
	}
	if len(pub.Published()) != 0 {
		t.Errorf("publisher saw %d events, want 0", len(pub.Published()))
	}
}

func TestRelayAllPublishesFailSkipsMark(t *testing.T) {
	ctx := context.Background()
	store := memory.NewOutboxStore(memory.NewManualClock(time.Now()))
	pub := memory.NewEventPublisher()
	seedEvent(t, store)
	seedEvent(t, store)
	pub.FailAll(nil) // the broker is down

	relay := appoutbox.NewRelay(store, pub, 10)
	n, err := relay.RunOnce(ctx)
	if err == nil {
		t.Fatal("RunOnce must surface the publish failure")
	}
	if n != 0 {
		t.Errorf("published = %d, want 0", n)
	}
	if store.MarkCalls() != 0 {
		t.Errorf("MarkPublished called with an empty confirmed prefix (calls=%d), want 0", store.MarkCalls())
	}
	if pending := store.PendingIDs(); len(pending) != 2 {
		t.Errorf("pending = %v, want both rows to retry next tick", pending)
	}
}

func TestRelayRespectsBatchLimit(t *testing.T) {
	ctx := context.Background()
	store := memory.NewOutboxStore(memory.NewManualClock(time.Now()))
	pub := memory.NewEventPublisher()
	for range 5 {
		seedEvent(t, store)
	}

	relay := appoutbox.NewRelay(store, pub, 2)
	n, err := relay.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Errorf("published = %d, want 2 (batch limit)", n)
	}
	if pending := store.PendingIDs(); len(pending) != 3 {
		t.Errorf("pending = %d, want 3", len(pending))
	}

	// The remaining rows drain over subsequent ticks, FIFO.
	for tick := range 2 {
		if _, err := relay.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce tick %d: %v", tick, err)
		}
	}
	if pending := store.PendingIDs(); len(pending) != 0 {
		t.Errorf("pending after draining = %v, want none", pending)
	}
	if len(pub.Published()) != 5 {
		t.Errorf("total published = %d, want 5", len(pub.Published()))
	}
}

func TestRelayListFailurePropagates(t *testing.T) {
	ctx := context.Background()
	store := memory.NewOutboxStore(memory.NewManualClock(time.Now()))
	pub := memory.NewEventPublisher()
	store.FailNextList(errors.New("postgres down"))

	relay := appoutbox.NewRelay(store, pub, 10)
	if _, err := relay.RunOnce(ctx); err == nil {
		t.Fatal("RunOnce must surface a ListPending failure")
	}
	if len(pub.Published()) != 0 {
		t.Errorf("publisher saw events despite a list failure")
	}
}

func TestManualRepublicationEligibility(t *testing.T) {
	event := seedEvent(t, memory.NewOutboxStore(memory.NewManualClock(time.Now())))
	for _, tc := range []struct {
		name                 string
		published, processed bool
		want                 string
	}{
		{"normal relay still responsible", false, false, ""},
		{"reviewed loss", true, false, "republished"},
		{"claim already recorded", true, true, "already_processed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := memory.NewEventPublisher()
			d := appoutbox.Delivery{Event: event, Published: tc.published, Processed: tc.processed}
			got, err := d.Republish(t.Context(), pub)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("result=%q, err=%v", got, err)
			}
			sent := pub.Published()
			if got == "republished" {
				if len(sent) != 1 || sent[0].ID != event.ID || sent[0].Type != event.Type || string(sent[0].Payload) != string(event.Payload) {
					t.Fatalf("unexpected envelope: %+v", sent)
				}
			} else if len(sent) != 0 {
				t.Fatal("ineligible event was published")
			}
		})
	}
}
