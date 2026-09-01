package memory

import (
	"context"
	"sync"

	"github.com/google/uuid"

	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
)

// Compile-time proof the fake satisfies the port.
var _ appbooking.BookingStore = (*BookingStore)(nil)

// BookingStore is the in-memory fake of the booking.BookingStore port.
//
// Faithfulness notes (where it deliberately matches Postgres behavior):
//   - It reproduces the ARBITER under the mutex: at most one confirmed
//     booking per (screening, seat), at most one booking per session. That
//     is the partial unique index + the session unique index from
//     migrations/00003, re-expressed as map checks.
//   - It honors the PARTIAL half of the seat index: only rows whose status
//     is confirmed block a seat. A cancelled row falls out of the index in
//     Postgres and out of the blocking check here, so a cancelled seat
//     becomes bookable again. (The port has no cancel path yet — this
//     keeps the fake honest when one lands.)
//   - Confirm assigns identity (ID, ConfirmedAt) the way the database does:
//     callers pass a zero ID and get the store's row back.
//
// Concurrency: one mutex guards all maps, mirroring the atomicity the real
// transaction provides. That is what lets the race tests prove the port's
// exactly-one-winner contract without Postgres.
type BookingStore struct {
	mu        sync.Mutex
	clock     Clock
	bySession map[string]domain.Booking   // sessionID -> booking
	bySeat    map[string][]domain.Booking // seatKey -> all bookings for that seat (append-only)
}

// NewBookingStore builds an empty fake on the given clock.
func NewBookingStore(clock Clock) *BookingStore {
	return &BookingStore{
		clock:     clock,
		bySession: make(map[string]domain.Booking),
		bySeat:    make(map[string][]domain.Booking),
	}
}

// Confirm implements booking.BookingStore.
func (s *BookingStore) Confirm(ctx context.Context, b domain.Booking) (domain.Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Session uniqueness first (mirrors confirmed_bookings_session_unique):
	// one session can never produce two bookings, even for different seats.
	if _, exists := s.bySession[b.SessionID]; exists {
		return domain.Booking{}, domain.ErrSessionAlreadyConfirmed
	}

	// Seat arbiter (mirrors the PARTIAL confirmed_bookings_seat_unique):
	// only CONFIRMED rows block the seat.
	key := SeatKey(b.ScreeningID, b.Seat)
	for _, existing := range s.bySeat[key] {
		if existing.Status == domain.StatusConfirmed {
			return domain.Booking{}, domain.ErrSeatAlreadyBooked
		}
	}

	// The database assigns identity and timestamps.
	b.ID = uuid.NewString()
	b.Status = domain.StatusConfirmed
	b.ConfirmedAt = s.clock.Now()

	s.bySession[b.SessionID] = b
	s.bySeat[key] = append(s.bySeat[key], b)
	return b, nil
}

// GetBySession implements booking.BookingStore.
func (s *BookingStore) GetBySession(ctx context.Context, sessionID string) (domain.Booking, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.bySession[sessionID]
	if !ok {
		return domain.Booking{}, domain.ErrBookingNotFound
	}
	return b, nil
}

// SeatConfirmed implements booking.BookingStore.
func (s *BookingStore) SeatConfirmed(ctx context.Context, screeningID string, seat domain.Seat) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.bySeat[SeatKey(screeningID, seat)] {
		if existing.Status == domain.StatusConfirmed {
			return true, nil
		}
	}
	return false, nil
}

// ConfirmedSeats implements booking.BookingStore.
func (s *BookingStore) ConfirmedSeats(ctx context.Context, screeningID string) ([]domain.Seat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seats := make([]domain.Seat, 0)
	for _, bookings := range s.bySeat {
		for _, b := range bookings {
			if b.ScreeningID == screeningID && b.Status == domain.StatusConfirmed {
				seats = append(seats, b.Seat)
			}
		}
	}
	return seats, nil
}
