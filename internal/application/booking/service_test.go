// External test package: the tests drive the Service through the in-memory
// fakes in adapters/memory, and that adapter imports this package for the
// ports — an in-package test file would close the loop into an import
// cycle. Everything exercised here is exported API anyway.
package booking_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/memory"
	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
	domainpayment "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
)

// epoch anchors every clock in these tests so expiry arithmetic is exact
// and comparable across subtests.
var epoch = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

const (
	testHoldTTL = 5 * time.Minute
	alice       = "user-alice"
	bob         = "user-bob"
)

// testEnv wires the Service over the shared in-memory fakes with one
// ManualClock driving both the hold store's expiry and the service's now().
type testEnv struct {
	clock      *memory.ManualClock
	holds      *memory.HoldStore
	bookings   *memory.BookingStore
	screenings *memory.ScreeningStore
	payments   *memory.PaymentStore
	svc        *appbooking.Service
}

func newTestEnv(t *testing.T, maxHolds int) *testEnv {
	t.Helper()
	clock := memory.NewManualClock(epoch)
	env := &testEnv{
		clock:      clock,
		holds:      memory.NewHoldStore(clock, maxHolds),
		bookings:   memory.NewBookingStore(clock),
		screenings: memory.NewScreeningStore(),
		payments:   memory.NewPaymentStore(clock),
	}
	env.svc = appbooking.NewService(env.holds, env.bookings, env.screenings, env.payments, testHoldTTL, clock.Now)
	return env
}

// seedScreening registers a 3x4 screening (rows A-C, seats 1-4) priced at
// 1200 cents (migration 00008 made price_cents NOT NULL — every fixture
// states its price, no silent defaults).
func (env *testEnv) seedScreening(id string) {
	env.screenings.Add(domainmovie.Screening{
		ID:          id,
		MovieID:     "movie-1",
		StartsAt:    epoch.Add(24 * time.Hour),
		Rows:        []string{"A", "B", "C"},
		SeatsPerRow: 4,
		PriceCents:  1200,
	})
}

// seedCapturedPayment satisfies the confirm gate (ADR 0008) the way a
// completed checkout would: the fake store gains a captured payment row for
// the session. Tests that exercise confirm SUCCESS call this first; tests
// that assert the gate itself deliberately do not.
func (env *testEnv) seedCapturedPayment(sessionID string) {
	env.payments.SeedCaptured(context.Background(), sessionID, "pi_fake_test-"+sessionID, 1200)
}

func seatA1() domain.Seat { return domain.Seat{Row: "A", Number: 1} }

// holdA1 is the happy-path fixture: alice holds A1 of the screening.
func (env *testEnv) holdA1(t *testing.T, screeningID string) domain.Hold {
	t.Helper()
	hold, err := env.svc.Hold(context.Background(), alice, screeningID, seatA1())
	if err != nil {
		t.Fatalf("setup hold: %v", err)
	}
	return hold
}

// failingReleaseStore decorates the fake hold store with an injectable
// Release failure — the test stand-in for "Redis was down right after the
// Postgres commit" (ADR 0006's cleanup-residue window).
type failingReleaseStore struct {
	inner *memory.HoldStore
	mu    sync.Mutex
	fail  bool
}

func (f *failingReleaseStore) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

func (f *failingReleaseStore) Hold(ctx context.Context, hold domain.Hold) error {
	return f.inner.Hold(ctx, hold)
}

func (f *failingReleaseStore) Release(ctx context.Context, sessionID, userID string) error {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail {
		return errors.New("redis: connection refused")
	}
	return f.inner.Release(ctx, sessionID, userID)
}

func (f *failingReleaseStore) Get(ctx context.Context, sessionID string) (*domain.Hold, error) {
	return f.inner.Get(ctx, sessionID)
}

func (f *failingReleaseStore) HeldSeats(ctx context.Context, screeningID string) ([]domain.Seat, error) {
	return f.inner.HeldSeats(ctx, screeningID)
}

// servedPastExpiryStore is a one-hold HoldStore stub that keeps serving the
// hold even after its domain expiry — the test stand-in for real Redis's
// ceiling-rounded TTL, which keeps a key alive up to a second PAST the
// domain ExpiresAt. That sliver is the only way the service's own
// ErrHoldExpired check (rather than the store's ErrHoldNotFound) can fire,
// so the branch gets its own stub instead of being untestable.
type servedPastExpiryStore struct {
	hold domain.Hold
}

var _ appbooking.HoldStore = servedPastExpiryStore{}

func (s servedPastExpiryStore) Hold(ctx context.Context, h domain.Hold) error { return nil }

func (s servedPastExpiryStore) Release(ctx context.Context, sessionID, userID string) error {
	return nil
}

func (s servedPastExpiryStore) Get(ctx context.Context, sessionID string) (*domain.Hold, error) {
	if sessionID == s.hold.SessionID {
		h := s.hold
		return &h, nil
	}
	return nil, domain.ErrHoldNotFound
}

func (s servedPastExpiryStore) HeldSeats(ctx context.Context, screeningID string) ([]domain.Seat, error) {
	return nil, nil
}

func TestServiceHold(t *testing.T) {
	const screeningID = "screening-1"

	t.Run("success", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)

		hold, err := env.svc.Hold(context.Background(), alice, screeningID, seatA1())
		if err != nil {
			t.Fatalf("Hold: %v", err)
		}
		if hold.SessionID == "" || hold.HoldToken == "" {
			t.Errorf("hold must carry a session ID and token: %+v", hold)
		}
		if hold.SessionID == hold.HoldToken {
			t.Error("session ID and hold token must be distinct values")
		}
		if hold.UserID != alice || hold.ScreeningID != screeningID || hold.Seat != seatA1() {
			t.Errorf("hold fields = %+v", hold)
		}
		// Expiry is now+TTL computed from the injected clock.
		want := epoch.Add(testHoldTTL)
		if !hold.ExpiresAt.Equal(want) {
			t.Errorf("ExpiresAt = %v, want %v", hold.ExpiresAt, want)
		}
		// The store actually holds the seat now.
		got, err := env.holds.Get(context.Background(), hold.SessionID)
		if err != nil || got.Seat != seatA1() {
			t.Errorf("store round-trip: got %+v, err %v", got, err)
		}
	})

	t.Run("unknown screening", func(t *testing.T) {
		env := newTestEnv(t, 4)
		_, err := env.svc.Hold(context.Background(), alice, "no-such-screening", seatA1())
		if !errors.Is(err, domainmovie.ErrScreeningNotFound) {
			t.Errorf("err = %v, want ErrScreeningNotFound", err)
		}
	})

	t.Run("seat out of range", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)

		cases := map[string]domain.Seat{
			"row not in grid":  {Row: "Z", Number: 1},
			"number too large": {Row: "A", Number: 5},
			"number zero-ish":  {Row: "B", Number: 0},
		}
		for name, seat := range cases {
			t.Run(name, func(t *testing.T) {
				_, err := env.svc.Hold(context.Background(), alice, screeningID, seat)
				if !errors.Is(err, domain.ErrSeatOutOfRange) {
					t.Errorf("err = %v, want ErrSeatOutOfRange", err)
				}
			})
		}
		// Rejection happened BEFORE Redis: nothing is held.
		held, err := env.holds.HeldSeats(context.Background(), screeningID)
		if err != nil || len(held) != 0 {
			t.Errorf("held seats after rejections = %v, err %v; want none", held, err)
		}
	})

	t.Run("already booked seat is rejected before holding", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		// A different session already confirmed A1 — the phantom-hold defense
		// must stop a new hold on the sold seat.
		_, err := env.bookings.Confirm(context.Background(), domain.Booking{
			SessionID: "someone-elses-session", ScreeningID: screeningID,
			Seat: seatA1(), UserID: bob, Status: domain.StatusConfirmed,
		})
		if err != nil {
			t.Fatalf("seed confirmed booking: %v", err)
		}

		_, err = env.svc.Hold(context.Background(), alice, screeningID, seatA1())
		if !errors.Is(err, domain.ErrSeatAlreadyBooked) {
			t.Errorf("err = %v, want ErrSeatAlreadyBooked", err)
		}
	})

	t.Run("held seat loses the race", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		env.holdA1(t, screeningID)

		_, err := env.svc.Hold(context.Background(), bob, screeningID, seatA1())
		if !errors.Is(err, domain.ErrSeatAlreadyHeld) {
			t.Errorf("err = %v, want ErrSeatAlreadyHeld", err)
		}
	})

	t.Run("hold limit", func(t *testing.T) {
		env := newTestEnv(t, 2)
		env.seedScreening(screeningID)
		env.holdA1(t, screeningID)
		if _, err := env.svc.Hold(context.Background(), alice, screeningID, domain.Seat{Row: "A", Number: 2}); err != nil {
			t.Fatalf("second hold: %v", err)
		}

		_, err := env.svc.Hold(context.Background(), alice, screeningID, domain.Seat{Row: "A", Number: 3})
		if !errors.Is(err, domain.ErrHoldLimitExceeded) {
			t.Errorf("err = %v, want ErrHoldLimitExceeded", err)
		}
	})
}

func TestServiceConfirm(t *testing.T) {
	const screeningID = "screening-1"
	ctx := context.Background()

	t.Run("success commits and cleans up the hold", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		hold := env.holdA1(t, screeningID)
		env.seedCapturedPayment(hold.SessionID)

		res, err := env.svc.Confirm(ctx, alice, hold.SessionID)
		if err != nil {
			t.Fatalf("Confirm: %v", err)
		}
		if !res.Created {
			t.Error("first confirm must report Created=true")
		}
		b := res.Booking
		if b.ID == "" || b.SessionID != hold.SessionID || b.ScreeningID != screeningID ||
			b.Seat != seatA1() || b.UserID != alice || b.Status != domain.StatusConfirmed {
			t.Errorf("booking fields = %+v", b)
		}
		if !b.ConfirmedAt.Equal(epoch) {
			t.Errorf("ConfirmedAt = %v, want %v", b.ConfirmedAt, epoch)
		}
		// The best-effort cleanup ran: the hold is gone.
		if _, err := env.holds.Get(ctx, hold.SessionID); !errors.Is(err, domain.ErrHoldNotFound) {
			t.Errorf("hold after confirm: err = %v, want ErrHoldNotFound", err)
		}
	})

	t.Run("unknown session", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)

		_, err := env.svc.Confirm(ctx, alice, "no-such-session")
		if !errors.Is(err, domain.ErrHoldNotFound) {
			t.Errorf("err = %v, want ErrHoldNotFound", err)
		}
	})

	t.Run("not the owner", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		hold := env.holdA1(t, screeningID)
		env.seedCapturedPayment(hold.SessionID)

		_, err := env.svc.Confirm(ctx, bob, hold.SessionID)
		if !errors.Is(err, domain.ErrNotHoldOwner) {
			t.Errorf("err = %v, want ErrNotHoldOwner", err)
		}
		// The real owner's hold is undamaged.
		if _, err := env.svc.Confirm(ctx, alice, hold.SessionID); err != nil {
			t.Errorf("owner confirm after failed probe: %v", err)
		}
	})

	t.Run("expired hold is rejected at the store level", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		hold := env.holdA1(t, screeningID)

		// With the shared clock the fake stops serving the hold exactly at
		// the inclusive expiry boundary, so the store-level not-found is
		// what fires. The HTTP layer maps it to the same 404 as the
		// domain-level expiry below — one user-visible event, one response.
		env.clock.Advance(testHoldTTL)
		_, err := env.svc.Confirm(ctx, alice, hold.SessionID)
		if !errors.Is(err, domain.ErrHoldNotFound) {
			t.Errorf("err = %v, want ErrHoldNotFound", err)
		}
	})

	t.Run("expired hold still served by the store is rejected by the domain rule", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)

		// Models real Redis's ceiling-rounded TTL: the key is still there a
		// moment past the domain expiry. The service's own CanBeConfirmed
		// check must refuse it (ADR 0006).
		hold := domain.Hold{
			SessionID: "late-session", ScreeningID: screeningID, Seat: seatA1(),
			UserID: alice, HoldToken: "late-token", ExpiresAt: epoch.Add(testHoldTTL),
		}
		svc := appbooking.NewService(servedPastExpiryStore{hold: hold}, env.bookings, env.screenings, env.payments, testHoldTTL, env.clock.Now)

		env.clock.Advance(testHoldTTL) // exactly at the inclusive expiry
		_, err := svc.Confirm(ctx, alice, hold.SessionID)
		if !errors.Is(err, domain.ErrHoldExpired) {
			t.Errorf("err = %v, want ErrHoldExpired", err)
		}
	})

	t.Run("seat won by someone else surfaces the conflict", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		hold := env.holdA1(t, screeningID)
		env.seedCapturedPayment(hold.SessionID)

		// Interleaving: between hold and confirm, a DIFFERENT session
		// confirmed the same seat (as if the hold had expired and someone
		// else booked). No row exists for alice's session, so the seat
		// conflict must surface as-is.
		_, err := env.bookings.Confirm(ctx, domain.Booking{
			SessionID: "rival-session", ScreeningID: screeningID,
			Seat: seatA1(), UserID: bob, Status: domain.StatusConfirmed,
		})
		if err != nil {
			t.Fatalf("seed rival booking: %v", err)
		}

		_, err = env.svc.Confirm(ctx, alice, hold.SessionID)
		if !errors.Is(err, domain.ErrSeatAlreadyBooked) {
			t.Errorf("err = %v, want ErrSeatAlreadyBooked", err)
		}
	})

	t.Run("replay after cleanup failure recovers idempotently", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		failing := &failingReleaseStore{inner: env.holds}
		env.svc = appbooking.NewService(failing, env.bookings, env.screenings, env.payments, testHoldTTL, env.clock.Now)

		hold, err := env.svc.Hold(ctx, alice, screeningID, seatA1())
		if err != nil {
			t.Fatalf("Hold: %v", err)
		}
		env.seedCapturedPayment(hold.SessionID)
		failing.setFail(true) // Redis goes down right after the commit...
		first, err := env.svc.Confirm(ctx, alice, hold.SessionID)
		if err != nil || !first.Created {
			t.Fatalf("first confirm: created=%v err=%v (cleanup failure must not fail the request)", first.Created, err)
		}
		// ...so the hold is still alive and the client retries.
		if _, err := env.holds.Get(ctx, hold.SessionID); err != nil {
			t.Fatalf("hold should survive the failed cleanup: %v", err)
		}

		second, err := env.svc.Confirm(ctx, alice, hold.SessionID)
		if err != nil {
			t.Fatalf("replay confirm: %v", err)
		}
		if second.Created {
			t.Error("replay must report Created=false")
		}
		if second.Booking.ID != first.Booking.ID {
			t.Errorf("replay returned a different booking: %s vs %s", second.Booking.ID, first.Booking.ID)
		}
		// Still exactly one booking row for the seat.
		seats, err := env.bookings.ConfirmedSeats(ctx, screeningID)
		if err != nil || len(seats) != 1 {
			t.Errorf("confirmed seats = %v (err %v), want exactly 1", seats, err)
		}
	})

	t.Run("no captured payment is rejected at the gate", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		hold := env.holdA1(t, screeningID)

		// The gate ADR 0008 anchored confirm to: a live, owned hold with no
		// captured money must not produce a booking.
		_, err := env.svc.Confirm(ctx, alice, hold.SessionID)
		if !errors.Is(err, domainpayment.ErrPaymentNotCaptured) {
			t.Errorf("err = %v, want ErrPaymentNotCaptured", err)
		}
		seats, seatsErr := env.bookings.ConfirmedSeats(ctx, screeningID)
		if seatsErr != nil || len(seats) != 0 {
			t.Errorf("confirmed seats = %v (err %v), want none", seats, seatsErr)
		}

		// An intent that never captured does not satisfy the gate either.
		if _, insertErr := env.payments.InsertIntent(ctx, domainpayment.Payment{
			BookingSessionID: hold.SessionID,
			Gateway:          "fake",
			IntentID:         "pi_fake_uncaptured",
			AmountCents:      1200,
			Currency:         domainpayment.DefaultCurrency,
		}); insertErr != nil {
			t.Fatalf("seed uncaptured intent: %v", insertErr)
		}
		if _, err := env.svc.Confirm(ctx, alice, hold.SessionID); !errors.Is(err, domainpayment.ErrPaymentNotCaptured) {
			t.Errorf("err = %v, want ErrPaymentNotCaptured with an uncaptured intent", err)
		}

		// Capture flips the gate without any other change.
		if _, _, captureErr := env.payments.Capture(ctx, "pi_fake_uncaptured"); captureErr != nil {
			t.Fatalf("capture: %v", captureErr)
		}
		if _, err := env.svc.Confirm(ctx, alice, hold.SessionID); err != nil {
			t.Errorf("confirm after capture: %v", err)
		}
	})

	t.Run("expired hold beats captured payment", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		hold := env.holdA1(t, screeningID)
		env.seedCapturedPayment(hold.SessionID)

		// ADR 0006's hard NO, now pinned with money actually captured: the
		// expiry check runs BEFORE the payment gate, so the answer is the
		// hold sentinel, not a payment one — and no booking is written even
		// though the funds moved. The payment row is the audit trail; the
		// refund path is deferred ops work (ADR 0008).
		env.clock.Advance(testHoldTTL)
		_, err := env.svc.Confirm(ctx, alice, hold.SessionID)
		if !errors.Is(err, domain.ErrHoldNotFound) {
			t.Errorf("err = %v, want ErrHoldNotFound (fake stops serving at the boundary)", err)
		}
		seats, seatsErr := env.bookings.ConfirmedSeats(ctx, screeningID)
		if seatsErr != nil || len(seats) != 0 {
			t.Errorf("confirmed seats = %v (err %v), want none", seats, seatsErr)
		}
	})

	t.Run("payment gate infra error fails the confirm closed", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		hold := env.holdA1(t, screeningID)
		env.seedCapturedPayment(hold.SessionID)

		// "Postgres went down mid-confirm": the gate read fails, so confirm
		// must fail closed (no booking) with the error wrapped, not guess.
		svc := appbooking.NewService(env.holds, env.bookings, env.screenings,
			failingPaymentFinder{err: errors.New("connection refused")}, testHoldTTL, env.clock.Now)
		_, err := svc.Confirm(ctx, alice, hold.SessionID)
		if err == nil {
			t.Fatal("confirm must fail when the payment gate cannot read")
		}
		if !strings.Contains(err.Error(), "payment gate") {
			t.Errorf("err = %v, want a wrapped payment-gate failure", err)
		}
		seats, seatsErr := env.bookings.ConfirmedSeats(ctx, screeningID)
		if seatsErr != nil || len(seats) != 0 {
			t.Errorf("confirmed seats = %v (err %v), want none — the gate failure must not write", seats, seatsErr)
		}
	})
}

// failingPaymentFinder is a PaymentFinder whose read always fails — the
// test stand-in for the payments table being unreachable mid-confirm.
type failingPaymentFinder struct{ err error }

var _ appbooking.PaymentFinder = failingPaymentFinder{}

func (f failingPaymentFinder) HasCapturedPayment(ctx context.Context, sessionID string) (bool, error) {
	return false, f.err
}

// TestServiceConcurrentCaptureConfirmExactlyOneBooking is the §9
// concurrency proof for the payment/confirm path: many goroutines race
// Confirm while the capture lands mid-race. Before the capture every racer
// must be rejected at the gate; after it, exactly one may create the
// booking and the rest are replays or hold-gone. Run with -race.
func TestServiceConcurrentCaptureConfirmExactlyOneBooking(t *testing.T) {
	const screeningID = "screening-1"
	const contenders = 32

	env := newTestEnv(t, 4)
	env.seedScreening(screeningID)
	hold := env.holdA1(t, screeningID)

	type outcome struct {
		res appbooking.ConfirmResult
		err error
	}
	results := make([]outcome, contenders)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := env.svc.Confirm(context.Background(), alice, hold.SessionID)
			results[i] = outcome{res: res, err: err}
		}(i)
	}
	close(start)
	// The capture lands while the racers are in flight — some see the gate
	// closed, some see it open; which is which depends on interleaving, and
	// that is the point.
	env.seedCapturedPayment(hold.SessionID)
	wg.Wait()

	created, replayed, gone, gated := 0, 0, 0, 0
	var bookingID string
	for i, r := range results {
		switch {
		case r.err == nil && r.res.Created:
			created++
		case r.err == nil && !r.res.Created:
			replayed++
		case errors.Is(r.err, domain.ErrHoldNotFound):
			gone++
		case errors.Is(r.err, domainpayment.ErrPaymentNotCaptured):
			gated++
		default:
			t.Errorf("goroutine %d unexpected outcome: created=%v err=%v", i, r.res.Created, r.err)
			continue
		}
		if r.err == nil {
			if bookingID == "" {
				bookingID = r.res.Booking.ID
			} else if r.res.Booking.ID != bookingID {
				t.Errorf("goroutine %d got booking %s, want %s", i, r.res.Booking.ID, bookingID)
			}
		}
	}
	if created != 1 {
		t.Errorf("created = %d, want exactly 1 (gated=%d replayed=%d gone=%d)", created, gated, replayed, gone)
	}
	if gated == 0 {
		// Not strictly guaranteed by interleaving, but 32 racers against a
		// mid-race capture should reliably produce some gate rejections; if
		// this ever flakes, the assertion to keep is created == 1.
		t.Log("note: no racer hit the closed gate — capture landed before all confirms")
	}
	seats, err := env.bookings.ConfirmedSeats(context.Background(), screeningID)
	if err != nil || len(seats) != 1 {
		t.Fatalf("confirmed seats = %v (err %v), want exactly 1", seats, err)
	}
}

func TestServiceRelease(t *testing.T) {
	const screeningID = "screening-1"
	ctx := context.Background()

	t.Run("success frees the seat", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		hold := env.holdA1(t, screeningID)

		if err := env.svc.Release(ctx, alice, hold.SessionID); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if _, err := env.holds.Get(ctx, hold.SessionID); !errors.Is(err, domain.ErrHoldNotFound) {
			t.Errorf("hold after release: err = %v, want ErrHoldNotFound", err)
		}
		// Someone else can claim the seat immediately.
		if _, err := env.svc.Hold(ctx, bob, screeningID, seatA1()); err != nil {
			t.Errorf("re-hold after release: %v", err)
		}
	})

	t.Run("unknown session", func(t *testing.T) {
		env := newTestEnv(t, 4)
		err := env.svc.Release(ctx, alice, "no-such-session")
		if !errors.Is(err, domain.ErrHoldNotFound) {
			t.Errorf("err = %v, want ErrHoldNotFound", err)
		}
	})

	t.Run("not the owner", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)
		hold := env.holdA1(t, screeningID)

		if err := env.svc.Release(ctx, bob, hold.SessionID); !errors.Is(err, domain.ErrNotHoldOwner) {
			t.Errorf("err = %v, want ErrNotHoldOwner", err)
		}
		// Alice's hold survived bob's release attempt.
		if _, err := env.holds.Get(ctx, hold.SessionID); err != nil {
			t.Errorf("hold after foreign release: %v", err)
		}
	})
}

func TestServiceSeatMap(t *testing.T) {
	const screeningID = "screening-1"
	ctx := context.Background()

	t.Run("precedence booked over held over available", func(t *testing.T) {
		env := newTestEnv(t, 4)
		env.seedScreening(screeningID)

		// alice holds A1; bob's session already confirmed B2.
		if _, err := env.svc.Hold(ctx, alice, screeningID, seatA1()); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		if _, err := env.bookings.Confirm(ctx, domain.Booking{
			SessionID: "bobs-session", ScreeningID: screeningID,
			Seat: domain.Seat{Row: "B", Number: 2}, UserID: bob, Status: domain.StatusConfirmed,
		}); err != nil {
			t.Fatalf("seed confirmed booking: %v", err)
		}
		// A conflicting stale hold on the BOOKED seat must not demote it:
		// Postgres wins (ADR 0006).
		if err := env.holds.Hold(ctx, domain.Hold{
			SessionID: "stale-session", ScreeningID: screeningID,
			Seat: domain.Seat{Row: "B", Number: 2}, UserID: "user-stale",
			HoldToken: "stale-token", ExpiresAt: epoch.Add(testHoldTTL),
		}); err != nil {
			t.Fatalf("seed stale hold: %v", err)
		}

		view, err := env.svc.SeatMap(ctx, screeningID)
		if err != nil {
			t.Fatalf("SeatMap: %v", err)
		}
		if len(view.Seats) != view.Screening.SeatCount() {
			t.Fatalf("seat count = %d, want %d", len(view.Seats), view.Screening.SeatCount())
		}

		want := map[domain.Seat]domain.SeatStatus{
			{Row: "A", Number: 1}: domain.SeatStatusHeld,
			{Row: "B", Number: 2}: domain.SeatStatusBooked,
		}
		for _, st := range view.Seats {
			expected, constrained := want[st.Seat]
			if !constrained {
				expected = domain.SeatStatusAvailable
			}
			if st.Status != expected {
				t.Errorf("seat %s%d = %q, want %q", st.Seat.Row, st.Seat.Number, st.Status, expected)
			}
		}
		// Row-major ordering from the screening grid.
		if view.Seats[0].Seat != (domain.Seat{Row: "A", Number: 1}) ||
			view.Seats[len(view.Seats)-1].Seat != (domain.Seat{Row: "C", Number: 4}) {
			t.Errorf("grid ordering wrong: first=%+v last=%+v", view.Seats[0].Seat, view.Seats[len(view.Seats)-1].Seat)
		}
	})

	t.Run("unknown screening", func(t *testing.T) {
		env := newTestEnv(t, 4)
		_, err := env.svc.SeatMap(ctx, "no-such-screening")
		if !errors.Is(err, domainmovie.ErrScreeningNotFound) {
			t.Errorf("err = %v, want ErrScreeningNotFound", err)
		}
	})
}

// TestServiceConcurrentConfirmSameSessionOneCreated races one hold's confirm
// from many goroutines — the application-level mirror of the DB arbiter
// race. Whatever interleaving happens, the store must end with exactly one
// booking row, at most one caller may see Created=true, and every other
// outcome is either the idempotent replay (same booking ID) or "hold gone"
// because the winner's best-effort cleanup already ran. Run with -race.
func TestServiceConcurrentConfirmSameSessionOneCreated(t *testing.T) {
	const screeningID = "screening-1"
	const contenders = 32

	env := newTestEnv(t, 4)
	env.seedScreening(screeningID)
	hold := env.holdA1(t, screeningID)
	env.seedCapturedPayment(hold.SessionID)

	type outcome struct {
		res appbooking.ConfirmResult
		err error
	}
	results := make([]outcome, contenders)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := env.svc.Confirm(context.Background(), alice, hold.SessionID)
			results[i] = outcome{res: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	created := 0
	replayed := 0
	gone := 0
	// Every successful outcome — created or replayed — must agree on ONE
	// booking ID, no matter the interleaving.
	var bookingID string
	for i, r := range results {
		switch {
		case r.err == nil && r.res.Created:
			created++
		case r.err == nil && !r.res.Created:
			replayed++
		case errors.Is(r.err, domain.ErrHoldNotFound):
			gone++
		default:
			t.Errorf("goroutine %d unexpected outcome: created=%v err=%v", i, r.res.Created, r.err)
			continue
		}
		if r.err == nil {
			if bookingID == "" {
				bookingID = r.res.Booking.ID
			} else if r.res.Booking.ID != bookingID {
				t.Errorf("goroutine %d got booking %s, want %s", i, r.res.Booking.ID, bookingID)
			}
		}
	}
	if created > 1 {
		t.Errorf("%d goroutines reported Created=true, want at most 1", created)
	}
	if created == 0 && replayed == 0 {
		t.Error("no goroutine confirmed — the booking must exist")
	}

	// Exactly one durable booking, regardless of interleaving.
	seats, err := env.bookings.ConfirmedSeats(context.Background(), screeningID)
	if err != nil || len(seats) != 1 {
		t.Fatalf("confirmed seats = %v (err %v), want exactly 1", seats, err)
	}
	stored, err := env.bookings.GetBySession(context.Background(), hold.SessionID)
	if err != nil {
		t.Fatalf("GetBySession after race: %v", err)
	}
	if bookingID != "" && stored.ID != bookingID {
		t.Errorf("stored booking %s, want the winner %s", stored.ID, bookingID)
	}
}
