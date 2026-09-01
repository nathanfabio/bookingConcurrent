package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	"github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres/sqlcgen"
	redisadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/redis"
	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"

	"github.com/google/uuid"
)

// TestBookingLifecycle drives the FULL application service against real
// Redis and real Postgres: hold → seat map held → confirm → seat map booked
// → phantom defense → release. This is the end-to-end proof that the two
// stores cooperate under ADR 0006's consistency model.
func TestBookingLifecycle(t *testing.T) {
	pool := connectPostgres(t)
	client := connectRedis(t)
	ctx := context.Background()

	movies := postgresadapter.NewMovieRepo(pool)
	screenings := postgresadapter.NewScreeningRepo(pool)
	bookings := postgresadapter.NewBookingRepo(pool)
	holds := redisadapter.NewHoldStore(client, testHoldTTL, 10)
	svc := appbooking.NewService(holds, bookings, screenings, testHoldTTL, time.Now)

	movie, err := movies.Create(ctx, domainmovie.Movie{
		Title: "Lifecycle " + uuid.NewString()[:8], DurationMinutes: 100,
	})
	if err != nil {
		t.Fatalf("create movie: %v", err)
	}
	scr, err := screenings.Create(ctx, domainmovie.Screening{
		MovieID: movie.ID, StartsAt: time.Now().Add(time.Hour),
		Rows: []string{"A", "B"}, SeatsPerRow: 3,
	})
	if err != nil {
		t.Fatalf("create screening: %v", err)
	}
	user, err := sqlcgen.New(pool).CreateUser(ctx, sqlcgen.CreateUserParams{
		Email: uuid.NewString()[:8] + "@example.com", PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	seatA1 := domain.Seat{Row: "A", Number: 1}

	// 1. Hold: the seat map flips A1 to held.
	hold, err := svc.Hold(ctx, user.ID, scr.ID, seatA1)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	status := seatStatus(t, svc, ctx, scr.ID, seatA1)
	if status != domain.SeatStatusHeld {
		t.Errorf("seat A1 after hold = %q, want held", status)
	}

	// 2. Confirm: Postgres commits, Redis is cleaned up, the map flips to
	// booked — Postgres-first, even though the cleanup also happened.
	res, err := svc.Confirm(ctx, user.ID, hold.SessionID)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !res.Created || res.Booking.Seat != seatA1 {
		t.Errorf("confirm result = %+v", res)
	}
	status = seatStatus(t, svc, ctx, scr.ID, seatA1)
	if status != domain.SeatStatusBooked {
		t.Errorf("seat A1 after confirm = %q, want booked", status)
	}

	// 3. Phantom-hold defense end-to-end: the seat is sold, so a new hold
	// is rejected before touching Redis.
	if _, err := svc.Hold(ctx, user.ID, scr.ID, seatA1); !errors.Is(err, domain.ErrSeatAlreadyBooked) {
		t.Errorf("hold on booked seat: err = %v, want ErrSeatAlreadyBooked", err)
	}

	// 4. Replay of the confirm is idempotent only while the hold survives;
	// after the cleanup released it, the session is simply unknown — the
	// same generic 404 the domain promises.
	if _, err := svc.Confirm(ctx, user.ID, hold.SessionID); !errors.Is(err, domain.ErrHoldNotFound) {
		t.Errorf("confirm after cleanup: err = %v, want ErrHoldNotFound", err)
	}

	// 5. Release on a fresh hold frees the seat again.
	hold2, err := svc.Hold(ctx, user.ID, scr.ID, domain.Seat{Row: "B", Number: 2})
	if err != nil {
		t.Fatalf("Hold B2: %v", err)
	}
	if err := svc.Release(ctx, user.ID, hold2.SessionID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	status = seatStatus(t, svc, ctx, scr.ID, domain.Seat{Row: "B", Number: 2})
	if status != domain.SeatStatusAvailable {
		t.Errorf("seat B2 after release = %q, want available", status)
	}
}

// TestBookingLifecycleExpiry proves the lifecycle against real TTL decay:
// an expired hold cannot be confirmed, and its seat becomes claimable
// again — with NO sweeper involved. Redis's own TTL does the work; the
// sweeper milestone will only make the bookkeeping cleanup deterministic
// (ADR 0006).
func TestBookingLifecycleExpiry(t *testing.T) {
	pool := connectPostgres(t)
	client := connectRedis(t)
	ctx := context.Background()

	movies := postgresadapter.NewMovieRepo(pool)
	screenings := postgresadapter.NewScreeningRepo(pool)
	bookings := postgresadapter.NewBookingRepo(pool)

	movie, err := movies.Create(ctx, domainmovie.Movie{
		Title: "Expiry " + uuid.NewString()[:8], DurationMinutes: 100,
	})
	if err != nil {
		t.Fatalf("create movie: %v", err)
	}
	scr, err := screenings.Create(ctx, domainmovie.Screening{
		MovieID: movie.ID, StartsAt: time.Now().Add(time.Hour),
		Rows: []string{"A"}, SeatsPerRow: 2,
	})
	if err != nil {
		t.Fatalf("create screening: %v", err)
	}
	user, err := sqlcgen.New(pool).CreateUser(ctx, sqlcgen.CreateUserParams{
		Email: uuid.NewString()[:8] + "@example.com", PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Short-TTL service: 1s holds, mirroring the hold-store lifecycle test.
	shortHolds := redisadapter.NewHoldStore(client, 1*time.Second, 10)
	svc := appbooking.NewService(shortHolds, bookings, screenings, 1*time.Second, time.Now)

	seat := domain.Seat{Row: "A", Number: 1}
	hold, err := svc.Hold(ctx, user.ID, scr.ID, seat)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	time.Sleep(2200 * time.Millisecond) // TTL 1s + scheduler margin

	if _, err := svc.Confirm(ctx, user.ID, hold.SessionID); !errors.Is(err, domain.ErrHoldNotFound) && !errors.Is(err, domain.ErrHoldExpired) {
		t.Errorf("confirm after expiry: err = %v, want ErrHoldNotFound/ErrHoldExpired", err)
	}

	// The seat is claimable again — expiry alone reconciled it.
	if _, err := svc.Hold(ctx, user.ID, scr.ID, seat); err != nil {
		t.Errorf("re-hold after expiry: %v", err)
	}
}

func seatStatus(t *testing.T, svc *appbooking.Service, ctx context.Context, screeningID string, seat domain.Seat) domain.SeatStatus {
	t.Helper()
	view, err := svc.SeatMap(ctx, screeningID)
	if err != nil {
		t.Fatalf("SeatMap: %v", err)
	}
	for _, st := range view.Seats {
		if st.Seat == seat {
			return st.Status
		}
	}
	t.Fatalf("seat %s%d missing from seat map", seat.Row, seat.Number)
	return ""
}
