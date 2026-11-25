package booking_test

import (
	"context"
	"errors"
	"testing"

	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
)

// A first-read miss models a concurrent commit between the durable lookup
// and Redis cleanup. The second lookup observes the now-committed booking.
type firstReadMissStore struct {
	appbooking.BookingStore
	reads int
}

func (s *firstReadMissStore) GetBySession(ctx context.Context, id string) (domain.Booking, error) {
	s.reads++
	if s.reads == 1 {
		return domain.Booking{}, domain.ErrBookingNotFound
	}
	return s.BookingStore.GetBySession(ctx, id)
}

func TestConfirmRecoversConcurrentCleanup(t *testing.T) {
	env := newTestEnv(t, 4)
	env.seedScreening("screening-1")
	hold := env.holdA1(t, "screening-1")
	env.seedCapturedPayment(hold.SessionID)
	first, err := env.svc.Confirm(context.Background(), alice, hold.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	store := &firstReadMissStore{BookingStore: env.bookings}
	svc := appbooking.NewService(env.holds, store, nil, nil, testHoldTTL, env.clock.Now)
	replay, err := svc.Confirm(context.Background(), alice, hold.SessionID)
	if err != nil || replay.Created || replay.Booking != first.Booking {
		t.Fatalf("replay: %+v, %v; want %+v", replay, err, first.Booking)
	}
}

type unavailableBookingStore struct {
	appbooking.BookingStore
	err error
}

func (s unavailableBookingStore) GetBySession(context.Context, string) (domain.Booking, error) {
	return domain.Booking{}, s.err
}

func TestConfirmDurableLookupFailure(t *testing.T) {
	failure := errors.New("database unavailable")
	svc := appbooking.NewService(nil, unavailableBookingStore{err: failure}, nil, nil, testHoldTTL, nil)
	if _, err := svc.Confirm(context.Background(), alice, "session"); !errors.Is(err, failure) {
		t.Fatalf("error = %v, want durable lookup failure", err)
	}
}
