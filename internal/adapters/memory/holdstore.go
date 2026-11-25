package memory

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	appholdexpiry "github.com/nathanfabio/bookingConcurrent/internal/application/holdexpiry"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
)

// Compile-time proof the fake satisfies BOTH ports over the same data —
// the same two-interface shape the Postgres PaymentRepo has: the booking
// service's HoldStore and the sweeper's ExpiredHoldStore.
var (
	_ appbooking.HoldStore           = (*HoldStore)(nil)
	_ appholdexpiry.ExpiredHoldStore = (*HoldStore)(nil)
)

// HoldStore is the in-memory fake of the booking.HoldStore and
// holdexpiry.ExpiredHoldStore ports.
//
// Faithfulness notes (where it deliberately matches Redis behavior):
//   - TTL expiry is SILENT and lazy: the first observation of an expired
//     session (Get, Release, HeldSeats, SweepExpired) purges the session
//     and seat entries — modeling Redis dropping the hash and seat key at
//     TTL — while the bookkeeping entries (the user/global ZSET
//     equivalents) LINGER until an explicit removal, exactly like Redis
//     ZSET members outliving their keys (ADR 0002). That lingering is what
//     makes the sweeper's GONE branch reachable in unit tests.
//   - Hold limits count lingering entries too — same as the in-script
//     ZCARD check against real Redis — so a silent expiry leaks a slot
//     until SweepExpired reclaims it (the user-visible effect the sweeper
//     exists for, ADR 0010).
//   - The global bookkeeping mirrors the Redis global ZSET's COMPOSITE
//     member format "userID:sessionID" (ADR 0010): after silent expiry the
//     session data — the only other place the userID lives — is gone, so a
//     sweeper driven by the global index needs the userID embedded in the
//     member to reclaim the per-user slot.
//   - SweepExpired is token-checked like the Redis sweepScript: a seat is
//     deleted only when its claim still carries the swept hold's token, so
//     sweeping an expired hold can never destroy a newer hold of the same
//     seat (the ADR 0002 guarantee).
//
// Concurrency: one mutex guards all maps. That is the point of the fake in
// the race tests — it proves the PORT's contract (exactly one winner) can
// be satisfied, while the integration tests prove real Redis satisfies it.
type HoldStore struct {
	mu          sync.Mutex
	clock       Clock
	maxHolds    int
	seats       map[string]*seatEntry          // seatKey -> claim
	sessions    map[string]*sessionEntry       // sessionID -> hold data
	userHolds   map[string]map[string]struct{} // userID -> sessionIDs (user ZSET)
	globalHolds map[string]time.Time           // "userID:sessionID" -> expiry (global ZSET member -> score)
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
		clock:       clock,
		maxHolds:    maxHolds,
		seats:       make(map[string]*seatEntry),
		sessions:    make(map[string]*sessionEntry),
		userHolds:   make(map[string]map[string]struct{}),
		globalHolds: make(map[string]time.Time),
	}
}

// SeatKey mirrors the Redis adapter's key scheme so tests can reason about
// both stores identically.
func SeatKey(screeningID string, seat domain.Seat) string {
	return "seat:" + screeningID + ":" + seat.Row + ":" + strconv.Itoa(seat.Number)
}

// GlobalMember mirrors the Redis adapter's composite global-ZSET member
// (ADR 0010), same convention as SeatKey.
func GlobalMember(userID, sessionID string) string {
	return userID + ":" + sessionID
}

// CountActiveHolds mirrors Redis's future-score gauge, excluding stale
// bookkeeping without mutating it. Redis scores have second precision.
func (s *HoldStore) CountActiveHolds(ctx context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int64
	for _, expires := range s.globalHolds {
		if expires.Unix() > now.Unix() {
			count++
		}
	}
	return count, nil
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
	s.globalHolds[GlobalMember(hold.UserID, hold.SessionID)] = hold.ExpiresAt
	return nil
}

// Release implements booking.HoldStore with the same compare-and-delete on
// the hold token as the Redis Lua script.
func (s *HoldStore) Release(ctx context.Context, sessionID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.sessions[sessionID]
	if !ok {
		return domain.ErrHoldNotFound
	}
	if !s.clock.Now().Before(entry.hold.ExpiresAt) {
		// Observed expired: purge the keys Redis's TTL would have dropped,
		// leave the bookkeeping for the sweeper (faithful residue).
		s.purgeExpiredLocked(entry.hold)
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
	delete(s.globalHolds, GlobalMember(userID, sessionID))
	return nil
}

// Get implements booking.HoldStore.
func (s *HoldStore) Get(ctx context.Context, sessionID string) (*domain.Hold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.sessions[sessionID]
	if !ok {
		return nil, domain.ErrHoldNotFound
	}
	if !s.clock.Now().Before(entry.hold.ExpiresAt) {
		s.purgeExpiredLocked(entry.hold)
		return nil, domain.ErrHoldNotFound
	}
	h := entry.hold
	return &h, nil
}

// HeldSeats implements booking.HoldStore. It enumerates live sessions for
// the screening — the fake's equivalent of SCANing seat:{screeningID}:*
// keys in Redis (the seat and session entries always share a lifetime, so
// both views agree). Snapshot semantics, same as the port contract.
func (s *HoldStore) HeldSeats(ctx context.Context, screeningID string) ([]domain.Seat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()

	seats := make([]domain.Seat, 0)
	// Deleting from a map while ranging over it is safe in Go — the lazy
	// purge below cannot corrupt the walk.
	for _, entry := range s.sessions {
		if !now.Before(entry.hold.ExpiresAt) {
			s.purgeExpiredLocked(entry.hold)
			continue
		}
		if entry.hold.ScreeningID == screeningID {
			seats = append(seats, entry.hold.Seat)
		}
	}
	return seats, nil
}

// SweepExpired implements holdexpiry.ExpiredHoldStore: the fake's twin of
// the Redis sweepScript. Driven by the global (composite-member) index like
// the real one: for each member expired at-or-before now, tear down the
// session data if it is still observable (SWEPT — token-checked) or just
// reclaim the bookkeeping (GONE — the lazy purge already modeled the TTL),
// bounded by limit.
func (s *HoldStore) SweepExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	swept := 0
	for member, expiry := range s.globalHolds {
		if swept >= limit {
			break
		}
		if now.Before(expiry) {
			continue // still live — the ALIVE guard of the Lua script
		}
		userID, sessionID, ok := splitGlobalMember(member)
		if !ok {
			// Anti-clog, mirroring the Redis adapter: an unparseable member
			// is discarded loudly rather than re-entering every window.
			// Unreachable in the fake (Hold always writes composites) but
			// kept honest with the real adapter's contract.
			delete(s.globalHolds, member)
			continue
		}
		if entry, exists := s.sessions[sessionID]; exists {
			// SWEPT branch: session data still here — token-checked teardown
			// so a newer hold of the same seat is never destroyed.
			key := SeatKey(entry.hold.ScreeningID, entry.hold.Seat)
			if e, ok := s.seats[key]; ok && e.token == entry.hold.HoldToken {
				delete(s.seats, key)
			}
			delete(s.sessions, sessionID)
		}
		// GONE branch lands here too: bookkeeping is all that remains.
		delete(s.userHolds[userID], sessionID)
		delete(s.globalHolds, member)
		swept++
	}
	return swept, nil
}

// purgeExpiredLocked models Redis's silent TTL drop for an OBSERVED-expired
// hold: the session entry and (if also expired) its seat entry disappear,
// while the ZSET bookkeeping lingers for the sweeper — the exact residue
// ADR 0002 describes. A seat entry whose expiry is in the FUTURE belongs to
// a newer hold (re-claimed after this one expired) and is never touched.
func (s *HoldStore) purgeExpiredLocked(hold domain.Hold) {
	delete(s.sessions, hold.SessionID)
	key := SeatKey(hold.ScreeningID, hold.Seat)
	if e, ok := s.seats[key]; ok && !s.clock.Now().Before(e.expiresAt) {
		delete(s.seats, key)
	}
}

// splitGlobalMember is the fake's copy of the Redis adapter's parse: split
// on the LAST colon, because the sessionID (server-minted UUID) is
// guaranteed colon-free while a userID in principle is not.
func splitGlobalMember(member string) (userID, sessionID string, ok bool) {
	i := strings.LastIndex(member, ":")
	if i <= 0 || i == len(member)-1 {
		return "", "", false
	}
	return member[:i], member[i+1:], true
}
