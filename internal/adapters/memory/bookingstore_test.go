package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
)

func newBookingFixture(t *testing.T) (*BookingStore, *ManualClock) {
	t.Helper()
	clock := NewManualClock(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	return NewBookingStore(clock), clock
}

func confirmedBooking(session, screening, user string, seat domain.Seat) domain.Booking {
	return domain.Booking{
		SessionID:   session,
		ScreeningID: screening,
		Seat:        seat,
		UserID:      user,
		Status:      domain.StatusConfirmed,
	}
}

func TestFakeConfirmArbiter(t *testing.T) {
	store, _ := newBookingFixture(t)
	ctx := context.Background()
	seat := domain.Seat{Row: "A", Number: 1}

	first, err := store.Confirm(ctx, confirmedBooking("session-1", "screening-1", "alice", seat))
	if err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	if first.ID == "" || !first.Status.Valid() {
		t.Errorf("store must assign identity: %+v", first)
	}

	// Same seat, different session: the seat arbiter refuses.
	_, err = store.Confirm(ctx, confirmedBooking("session-2", "screening-1", "bob", seat))
	if !errors.Is(err, domain.ErrSeatAlreadyBooked) {
		t.Errorf("second seat confirm: err = %v, want ErrSeatAlreadyBooked", err)
	}

	// Same session, DIFFERENT seat: the session index refuses — one session
	// can never produce two bookings.
	_, err = store.Confirm(ctx, confirmedBooking("session-1", "screening-1", "alice", domain.Seat{Row: "B", Number: 2}))
	if !errors.Is(err, domain.ErrSessionAlreadyConfirmed) {
		t.Errorf("second session confirm: err = %v, want ErrSessionAlreadyConfirmed", err)
	}

	// Same seat in a DIFFERENT screening is independent.
	if _, err := store.Confirm(ctx, confirmedBooking("session-3", "screening-2", "carol", seat)); err != nil {
		t.Errorf("same seat other screening: %v", err)
	}
}

func TestFakeGetBySession(t *testing.T) {
	store, _ := newBookingFixture(t)
	ctx := context.Background()

	if _, err := store.GetBySession(ctx, "unknown"); !errors.Is(err, domain.ErrBookingNotFound) {
		t.Errorf("unknown session: err = %v, want ErrBookingNotFound", err)
	}

	want, err := store.Confirm(ctx, confirmedBooking("session-1", "screening-1", "alice", domain.Seat{Row: "A", Number: 1}))
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	got, err := store.GetBySession(ctx, "session-1")
	if err != nil || got.ID != want.ID {
		t.Errorf("GetBySession = %+v, err %v; want booking %s", got, err, want.ID)
	}
}

func TestFakeSeatConfirmedAndConfirmedSeats(t *testing.T) {
	store, _ := newBookingFixture(t)
	ctx := context.Background()

	ok, err := store.SeatConfirmed(ctx, "screening-1", domain.Seat{Row: "A", Number: 1})
	if err != nil || ok {
		t.Errorf("empty store: SeatConfirmed = %v, err %v; want false", ok, err)
	}

	if _, err := store.Confirm(ctx, confirmedBooking("session-1", "screening-1", "alice", domain.Seat{Row: "A", Number: 1})); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	ok, err = store.SeatConfirmed(ctx, "screening-1", domain.Seat{Row: "A", Number: 1})
	if err != nil || !ok {
		t.Errorf("booked seat: SeatConfirmed = %v, err %v; want true", ok, err)
	}

	seats, err := store.ConfirmedSeats(ctx, "screening-1")
	if err != nil || len(seats) != 1 || seats[0] != (domain.Seat{Row: "A", Number: 1}) {
		t.Errorf("ConfirmedSeats = %v, err %v; want [A1]", seats, err)
	}
	// Another screening stays empty.
	seats, err = store.ConfirmedSeats(ctx, "screening-2")
	if err != nil || len(seats) != 0 {
		t.Errorf("other screening ConfirmedSeats = %v, err %v; want none", seats, err)
	}
}

// TestFakeConcurrentConfirmOneWinner races many sessions at the fake
// arbiter for ONE seat — the confirm-path sibling of
// TestFakeConcurrentRaceOneWinner for holds. Exactly one confirm may win;
// every other contender must see the seat arbiter refuse. Run with -race;
// the integration suite repeats this against real Postgres.
func TestFakeConcurrentConfirmOneWinner(t *testing.T) {
	store, _ := newBookingFixture(t)
	const contenders = 64

	var (
		mu      sync.Mutex
		winners int
		losers  int
		other   []error
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			session := fmt.Sprintf("session-%d", i)
			_, err := store.Confirm(context.Background(),
				confirmedBooking(session, "screening-1", "user", domain.Seat{Row: "A", Number: 1}))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, domain.ErrSeatAlreadyBooked):
				losers++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
	if losers != contenders-1 {
		t.Errorf("losers = %d, want %d", losers, contenders-1)
	}
	if len(other) != 0 {
		t.Errorf("unexpected errors: %v", other)
	}
}
