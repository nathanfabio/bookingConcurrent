package memory

import (
	"context"
	"sync"
	"time"

	appoutbox "github.com/nathanfabio/bookingConcurrent/internal/application/outbox"
)

// Compile-time proof the fake satisfies the port.
var _ appoutbox.OutboxStore = (*OutboxStore)(nil)

// OutboxStore is the in-memory fake of the outbox.OutboxStore port.
//
// Faithfulness notes (where it deliberately matches Postgres behavior):
//   - ListPending returns UNPUBLISHED events in insertion order, capped at
//     the limit — insertion order stands in for the SQL ORDER BY
//     (created_at, id), which is FIFO for rows written by sequential
//     transactions (the only order the relay's contract promises).
//   - MarkPublished stamps a published time, and TOLERATES already-marked
//     ids as no-ops, mirroring the SQL's "AND published_at IS NULL" guard:
//     a replayed mark after a republish is a normal path, not an error.
//   - Published rows never reappear in ListPending, so the relay's
//     "second tick publishes nothing" behavior is observable in tests.
//
// FailNextMark injects a one-shot mark failure — the lever for the
// at-least-once tests (mark fails after publish ⇒ the next tick republishes
// and consumers dedupe).
type OutboxStore struct {
	mu        sync.Mutex
	clock     Clock
	entries   []outboxEntry
	failMark  error // consumed by the next MarkPublished call
	failList  error // consumed by the next ListPending call
	markCalls int
}

type outboxEntry struct {
	evt         appoutbox.Event
	publishedAt *time.Time
}

// NewOutboxStore builds an empty fake on the given clock.
func NewOutboxStore(clock Clock) *OutboxStore {
	return &OutboxStore{clock: clock}
}

// Seed appends a pending event, mirroring the API-side InsertOutboxEvent
// that writes the row inside the confirm transaction.
func (s *OutboxStore) Seed(evt appoutbox.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, outboxEntry{evt: evt})
}

// FailNextMark makes the NEXT MarkPublished call fail once with err (and
// only that call), modeling a Postgres hiccup between publish and stamp.
func (s *OutboxStore) FailNextMark(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failMark = err
}

// FailNextList makes the NEXT ListPending call fail once with err.
func (s *OutboxStore) FailNextList(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failList = err
}

// MarkCalls counts MarkPublished invocations (including failed ones) so
// tests can pin "the relay skips the UPDATE when nothing was confirmed".
func (s *OutboxStore) MarkCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.markCalls
}

// PendingIDs returns the ids of events not yet marked published, in FIFO
// order — an assertion helper for tests.
func (s *OutboxStore) PendingIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, e := range s.entries {
		if e.publishedAt == nil {
			ids = append(ids, e.evt.ID)
		}
	}
	return ids
}

// PublishedIDs returns the ids of events marked published, in FIFO order.
func (s *OutboxStore) PublishedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, e := range s.entries {
		if e.publishedAt != nil {
			ids = append(ids, e.evt.ID)
		}
	}
	return ids
}

// ListPending implements outbox.OutboxStore.
func (s *OutboxStore) ListPending(ctx context.Context, limit int) ([]appoutbox.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failList != nil {
		err := s.failList
		s.failList = nil
		return nil, err
	}
	out := make([]appoutbox.Event, 0, limit)
	for _, e := range s.entries {
		if len(out) >= limit {
			break
		}
		if e.publishedAt == nil {
			out = append(out, e.evt)
		}
	}
	return out, nil
}

// MarkPublished implements outbox.OutboxStore. Unknown ids are ignored
// (they cannot exist in the real table either — the relay only marks ids it
// just listed), and already-published ids are no-ops.
func (s *OutboxStore) MarkPublished(ctx context.Context, eventIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markCalls++
	if s.failMark != nil {
		err := s.failMark
		s.failMark = nil
		return err
	}
	now := s.clock.Now()
	for _, id := range eventIDs {
		for i := range s.entries {
			if s.entries[i].evt.ID == id && s.entries[i].publishedAt == nil {
				s.entries[i].publishedAt = &now
			}
		}
	}
	return nil
}
