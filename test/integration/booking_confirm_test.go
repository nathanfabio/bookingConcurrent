package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	"github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres/sqlcgen"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bookingFixture wires a BookingRepo over the shared test database with a
// fresh movie, screening, and N users, so each test starts from its own
// clean corner of the schema.
type bookingFixture struct {
	pool      *pgxpool.Pool
	repo      *postgresadapter.BookingRepo
	screening domainmovie.Screening
	users     []string
}

// queryRow runs a one-off assertion query against the fixture's pool (the
// outbox has no read query by design — its only reader is the M6 relay).
func (f *bookingFixture) queryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return f.pool.QueryRow(ctx, sql, args...)
}

func newBookingFixture(t *testing.T, nUsers int) *bookingFixture {
	t.Helper()
	pool := connectPostgres(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	movies := postgresadapter.NewMovieRepo(pool)
	screenings := postgresadapter.NewScreeningRepo(pool)

	movie, err := movies.Create(ctx, domainmovie.Movie{
		Title: "Confirm " + uuid.NewString()[:8], DurationMinutes: 90,
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

	users := make([]string, nUsers)
	for i := range users {
		u, err := q.CreateUser(ctx, sqlcgen.CreateUserParams{
			Email: uuid.NewString()[:8] + "@example.com", PasswordHash: "x",
		})
		if err != nil {
			t.Fatalf("create user %d: %v", i, err)
		}
		users[i] = u.ID
	}

	return &bookingFixture{
		pool:      pool,
		repo:      postgresadapter.NewBookingRepo(pool),
		screening: scr,
		users:     users,
	}
}

func (f *bookingFixture) booking(session, user string, row string, number int) domain.Booking {
	return domain.Booking{
		SessionID:   session,
		ScreeningID: f.screening.ID,
		Seat:        domain.Seat{Row: row, Number: number},
		UserID:      user,
		Status:      domain.StatusConfirmed,
	}
}

// TestConfirmWritesBookingAndOutboxAtomically proves the commit point of
// ADR 0003/0006: a successful Confirm persists the booking row AND exactly
// one BookingConfirmed outbox row, with the payload describing the row the
// database assigned.
func TestConfirmWritesBookingAndOutboxAtomically(t *testing.T) {
	f := newBookingFixture(t, 1)
	ctx := context.Background()

	session := uuid.NewString()
	created, err := f.repo.Confirm(ctx, f.booking(session, f.users[0], "A", 1))
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if created.ID == "" || created.ConfirmedAt.IsZero() {
		t.Errorf("database must assign identity: %+v", created)
	}

	// The outbox row landed in the same transaction.
	var count int
	if err := f.queryRow(ctx,
		"SELECT count(*) FROM outbox WHERE payload->>'session_id' = $1", session).Scan(&count); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("outbox rows for the session = %d, want exactly 1", count)
	}

	var eventType, payload string
	if err := f.queryRow(ctx,
		"SELECT event_type, payload::text FROM outbox WHERE payload->>'session_id' = $1", session).
		Scan(&eventType, &payload); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	if eventType != "BookingConfirmed" {
		t.Errorf("event_type = %q, want BookingConfirmed", eventType)
	}
	for _, want := range []string{created.ID, session, f.screening.ID, f.users[0], `"booking_id"`, `"confirmed_at"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload missing %q: %s", want, payload)
		}
	}
}

// TestConfirmConstraintDiscrimination pins the conflict mapping to the live
// schema: which UNIQUE index fired decides the sentinel, and the index
// NAMES are asserted here so a migration rename breaks the build instead of
// silently turning every conflict into a 500 in production.
func TestConfirmConstraintDiscrimination(t *testing.T) {
	f := newBookingFixture(t, 2)
	ctx := context.Background()
	q := sqlcgen.New(f.pool)

	session := uuid.NewString()
	if _, err := f.repo.Confirm(ctx, f.booking(session, f.users[0], "A", 1)); err != nil {
		t.Fatalf("first Confirm: %v", err)
	}

	// Seat conflict: a different session wants the same seat.
	_, err := f.repo.Confirm(ctx, f.booking(uuid.NewString(), f.users[1], "A", 1))
	if !errors.Is(err, domain.ErrSeatAlreadyBooked) {
		t.Errorf("seat conflict: err = %v, want ErrSeatAlreadyBooked", err)
	}
	// Session conflict: the same session wants a different seat.
	_, err = f.repo.Confirm(ctx, f.booking(session, f.users[0], "A", 2))
	if !errors.Is(err, domain.ErrSessionAlreadyConfirmed) {
		t.Errorf("session conflict: err = %v, want ErrSessionAlreadyConfirmed", err)
	}

	// Pin the raw constraint names behind the mapping.
	_, err = q.InsertConfirmedBooking(ctx, sqlcgen.InsertConfirmedBookingParams{
		SessionID: uuid.NewString(), ScreeningID: f.screening.ID,
		SeatRow: "A", SeatNumber: 1, UserID: f.users[1],
	})
	if name := uniqueConstraintName(err); name != "confirmed_bookings_seat_unique" {
		t.Errorf("seat conflict constraint = %q, want confirmed_bookings_seat_unique (err=%v)", name, err)
	}
	_, err = q.InsertConfirmedBooking(ctx, sqlcgen.InsertConfirmedBookingParams{
		SessionID: session, ScreeningID: f.screening.ID,
		SeatRow: "B", SeatNumber: 2, UserID: f.users[0],
	})
	if name := uniqueConstraintName(err); name != "confirmed_bookings_session_unique" {
		t.Errorf("session conflict constraint = %q, want confirmed_bookings_session_unique (err=%v)", name, err)
	}
}

// TestConcurrentConfirmExactlyOneWinner races many sessions at the same seat
// through the FULL repository path — transaction, arbiter, and outbox —
// against real Postgres. Exactly one contender may win, every other must
// see the seat sentinel, and the outbox must contain exactly one event:
// proof that "booking row + event row" stayed atomic under contention.
// Run with -race.
func TestConcurrentConfirmExactlyOneWinner(t *testing.T) {
	const contenders = 32
	f := newBookingFixture(t, contenders)
	ctx := context.Background()

	type outcome struct {
		booking domain.Booking
		err     error
	}
	results := make([]outcome, contenders)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			b, err := f.repo.Confirm(ctx, f.booking(uuid.NewString(), f.users[i], "A", 1))
			results[i] = outcome{booking: b, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	losers := 0
	for i, r := range results {
		switch {
		case r.err == nil:
			winners++
		case errors.Is(r.err, domain.ErrSeatAlreadyBooked):
			losers++
		default:
			t.Errorf("contender %d unexpected error: %v", i, r.err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	if losers != contenders-1 {
		t.Fatalf("losers = %d, want %d", losers, contenders-1)
	}

	// One sale, one event — even though 32 transactions raced.
	var events int
	if err := f.queryRow(ctx,
		"SELECT count(*) FROM outbox WHERE payload->>'screening_id' = $1", f.screening.ID).Scan(&events); err != nil {
		t.Fatalf("count outbox events: %v", err)
	}
	if events != 1 {
		t.Errorf("outbox events for the screening = %d, want 1", events)
	}
}

// TestBookingRepoReads covers the non-conflict halves of the port:
// GetBySession round-trip and not-found, SeatConfirmed, and ConfirmedSeats.
func TestBookingRepoReads(t *testing.T) {
	f := newBookingFixture(t, 2)
	ctx := context.Background()

	if _, err := f.repo.GetBySession(ctx, "no-such-session"); !errors.Is(err, domain.ErrBookingNotFound) {
		t.Errorf("unknown session: err = %v, want ErrBookingNotFound", err)
	}

	session := uuid.NewString()
	created, err := f.repo.Confirm(ctx, f.booking(session, f.users[0], "A", 1))
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	got, err := f.repo.GetBySession(ctx, session)
	if err != nil || got.ID != created.ID || got.Status != domain.StatusConfirmed {
		t.Errorf("GetBySession = %+v, err %v; want booking %s", got, err, created.ID)
	}

	ok, err := f.repo.SeatConfirmed(ctx, f.screening.ID, domain.Seat{Row: "A", Number: 1})
	if err != nil || !ok {
		t.Errorf("SeatConfirmed(booked) = %v, %v; want true", ok, err)
	}
	ok, err = f.repo.SeatConfirmed(ctx, f.screening.ID, domain.Seat{Row: "B", Number: 3})
	if err != nil || ok {
		t.Errorf("SeatConfirmed(free) = %v, %v; want false", ok, err)
	}

	if _, err := f.repo.Confirm(ctx, f.booking(uuid.NewString(), f.users[1], "B", 3)); err != nil {
		t.Fatalf("second Confirm: %v", err)
	}
	seats, err := f.repo.ConfirmedSeats(ctx, f.screening.ID)
	if err != nil || len(seats) != 2 {
		t.Fatalf("ConfirmedSeats = %v, err %v; want 2 seats", seats, err)
	}
}

func uniqueConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}
