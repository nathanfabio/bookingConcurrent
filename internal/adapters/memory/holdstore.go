package memory

import (
	"context"
	"strconv"
	"sync"
	"time"

	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
)

// Compile-time proof the fake satisfies the port.
var _ appbooking.HoldStore = (*HoldStore)(nil)

// HoldStore is the in-memory fake of the booking.HoldStore port.
//
// Faithfulness notes (where it deliberately matches Redis behavior):
//   - TTL expiry is silent: expired entries are invisible to Get/Release
//     and the seat becomes claimable again, but bookkeeping entries
//     (the user/global ZSET equivalents) linger until an explicit removal,
//     exactly like Redis ZSETs do after their member keys expire. The
//     expiry sweeper (M4) is what reconciles them in both stores.
//   - Hold limits therefore count lingering entries too — same as the
//     in-script ZCARD check against real Redis.
//
// Concurrency: one mutex guards all maps. That is the point of the fake in
// the race tests — it proves the PORT's contract (exactly one winner) can
// be satisfied, while the integration tests prove real Redis satisfies it.
type HoldStore struct {
	mu         sync.Mutex
	clock      Clock
	maxHolds   int
	seats      map[string]*seatEntry          // seatKey -> claim
	sessions   map[string]*sessionEntry       // sessionID -> hold data
	userHolds  map[string]map[string]struct{} // userID -> live sessionIDs bookkeeping
	globalSize int
}

type seatEntry struct {
	token     string
	expiresAt time.Time
}

type sessionEntry struct {
	hold domain.Hold
}

// NewHoldStore builds a fake with the given clock and per-user hold limit.
func NewHoldStore(clock Clock, maxHolds int) *HoldStore {
	return &HoldStore{
		clock:     clock,
		maxHolds:  maxHolds,
		seats:     make(map[string]*seatEntry),
		sessions:  make(map[string]*sessionEntry),
		userHolds: make(map[string]map[string]struct{}),
	}
}

// SeatKey mirrors the Redis adapter's key scheme so tests can reason about
// both stores identically.
func SeatKey(screeningID string, seat domain.Seat) string {
	return "seat:" + screeningID + ":" + seat.Row + ":" + strconv.Itoa(seat.Number)
}

// Hold implements booking.HoldStore.
func (s *HoldStore) Hold(ctx context.Context, hold domain.Hold) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()

	// Limit check first, mirroring the in-script ZCARD: stale bookkeeping
	// counts until swept, same as Redis.
	if len(s.userHolds[hold.UserID]) >= s.maxHolds {
		return domain.ErrHoldLimitExceeded
	}

	key := SeatKey(hold.ScreeningID, hold.Seat)
	if e, ok := s.seats[key]; ok && now.Before(e.expiresAt) {
		return domain.ErrSeatAlreadyHeld
	}

	s.seats[key] = &seatEntry{token: hold.HoldToken, expiresAt: hold.ExpiresAt}
	s.sessions[hold.SessionID] = &sessionEntry{hold: hold}
	if s.userHolds[hold.UserID] == nil {
		s.userHolds[hold.UserID] = make(map[string]struct{})
	}
	s.userHolds[hold.UserID][hold.SessionID] = struct{}{}
	s.globalSize++
	return nil
}

// Release implements booking.HoldStore with the same compare-and-delete on
// the hold token as the Redis Lua script.
func (s *HoldStore) Release(ctx context.Context, sessionID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()

	entry, ok := s.sessions[sessionID]
	if !ok || !now.Before(entry.hold.ExpiresAt) {
		return domain.ErrHoldNotFound
	}
	if entry.hold.UserID != userID {
		return domain.ErrNotHoldOwner
	}

	key := SeatKey(entry.hold.ScreeningID, entry.hold.Seat)
	if e, ok := s.seats[key]; ok && e.token == entry.hold.HoldToken {
		// Still our claim — clear it. If the token differs, this seat
		// already belongs to a later hold; leave it alone.
		delete(s.seats, key)
	}
	delete(s.sessions, sessionID)
	delete(s.userHolds[userID], sessionID)
	s.globalSize--
	return nil
}

// Get implements booking.HoldStore.
func (s *HoldStore) Get(ctx context.Context, sessionID string) (*domain.Hold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.sessions[sessionID]
	if !ok || !s.clock.Now().Before(entry.hold.ExpiresAt) {
		return nil, domain.ErrHoldNotFound
	}
	h := entry.hold
	return &h, nil
}
