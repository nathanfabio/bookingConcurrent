package memory

import (
	"context"
	"errors"
	"sync"

	appnotification "github.com/nathanfabio/bookingConcurrent/internal/application/notification"
)

// Compile-time proof the fake satisfies the port.
var _ appnotification.ProcessedEvents = (*ProcessedEvents)(nil)

// ErrClaimFailed is the default error FailNextClaim injects.
var ErrClaimFailed = errors.New("memory: claim failed (injected)")

// ProcessedEvents is the in-memory fake of the notification.ProcessedEvents
// port.
//
// Faithfulness notes (where it deliberately matches Postgres behavior):
//   - The mutex-guarded map insert IS the INSERT .. ON CONFLICT DO NOTHING:
//     exactly one concurrent Claim for a given event_id returns true, the
//     rest return false. That is what lets the handler's race test prove
//     exactly-once-effect without Postgres.
//   - A FAILED claim records nothing — mirroring the SQL transaction that
//     rolls back — so the redelivery after a transient error still claims
//     successfully.
type ProcessedEvents struct {
	mu       sync.Mutex
	seen     map[string]string // event_id -> event_type
	failNext error             // consumed by the next Claim call
}

// NewProcessedEvents builds an empty fake.
func NewProcessedEvents() *ProcessedEvents {
	return &ProcessedEvents{seen: make(map[string]string)}
}

// FailNextClaim makes the NEXT Claim call fail once with err (or
// ErrClaimFailed when nil), recording nothing.
func (s *ProcessedEvents) FailNextClaim(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		err = ErrClaimFailed
	}
	s.failNext = err
}

// Count returns how many event ids have been claimed.
func (s *ProcessedEvents) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// Has reports whether eventID was claimed.
func (s *ProcessedEvents) Has(eventID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.seen[eventID]
	return ok
}

// Claim implements notification.ProcessedEvents.
func (s *ProcessedEvents) Claim(ctx context.Context, eventID, eventType string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return false, err
	}
	if _, exists := s.seen[eventID]; exists {
		return false, nil
	}
	s.seen[eventID] = eventType
	return true, nil
}
