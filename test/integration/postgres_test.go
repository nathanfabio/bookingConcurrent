package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	"github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres/sqlcgen"
	"github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainmovie "github.com/nathanfabio/bookingConcurrent/internal/domain/movie"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	_ "github.com/jackc/pgx/v5/stdlib" // maintenance connection via database/sql
)

// pgTestConfig builds a Postgres config for tests from env vars, defaulting
// to the compose values. The TEST database is separate from the dev one and
// is created on demand, so tests never touch development data.
func pgTestConfig(t *testing.T) config.Postgres {
	t.Helper()
	cfg := config.Postgres{
		Host:     envOr("POSTGRES_HOST", "localhost"),
		Port:     5433,
		User:     envOr("POSTGRES_USER", "booking"),
		Password: envOr("POSTGRES_PASSWORD", "booking-dev-password"),
		Database: envOr("TEST_POSTGRES_DB", "booking_test"),
		SSLMode:  "disable",
	}
	if v := os.Getenv("POSTGRES_PORT"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &cfg.Port); err != nil {
			t.Fatalf("POSTGRES_PORT=%q is not an integer", v)
		}
	}
	// Defense against SQL injection through env config: the database name
	// is interpolated into CREATE DATABASE below, so constrain its charset.
	for _, r := range cfg.Database {
		if !isTestDBChar(r) {
			t.Fatalf("TEST_POSTGRES_DB %q must be [a-z0-9_]+", cfg.Database)
		}
	}
	return cfg
}

func isTestDBChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// connectPostgres ensures the test database exists, applies migrations, and
// returns a ready pool — or skips the test when Postgres is unreachable.
func connectPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	cfg := pgTestConfig(t)

	// Maintenance connection to guarantee the test database exists.
	maint := cfg
	maint.Database = "postgres"
	db, err := sql.Open("pgx", maint.DSN())
	if err != nil {
		t.Fatalf("open maintenance connection: %v", err)
	}
	defer func() { _ = db.Close() }()

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		t.Skipf("Postgres not reachable at %s:%d — start infra with `make up`: %v",
			cfg.Host, cfg.Port, err)
	}

	var exists bool
	err = db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", cfg.Database).
		Scan(&exists)
	if err != nil {
		t.Fatalf("check database existence: %v", err)
	}
	if !exists {
		// The name is charset-validated above; identifiers cannot be
		// parameterized in DDL.
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, cfg.Database)); err != nil {
			t.Fatalf("create test database: %v", err)
		}
	}

	if _, err := postgresadapter.MigrateUp(ctx, cfg); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	pool, err := postgresadapter.NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestMigrationsAreIdempotent(t *testing.T) {
	cfg := pgTestConfig(t)
	// connectPostgres already migrated once; a second pass must apply zero.
	_ = connectPostgres(t)

	applied, err := postgresadapter.MigrateUp(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second MigrateUp: %v", err)
	}
	if applied != 0 {
		t.Errorf("second migration pass applied %d migrations, want 0", applied)
	}
}

func TestMovieAndScreeningRepositoryRoundTrip(t *testing.T) {
	pool := connectPostgres(t)
	ctx := context.Background()
	movies := postgresadapter.NewMovieRepo(pool)
	screenings := postgresadapter.NewScreeningRepo(pool)

	title := "Integration Feature " + uuid.NewString()[:8]
	created, err := movies.Create(ctx, domainmovie.Movie{
		Title: title, Synopsis: "test movie", DurationMinutes: 100,
	})
	if err != nil {
		t.Fatalf("create movie: %v", err)
	}
	if created.ID == "" {
		t.Error("movie ID should be assigned by the database")
	}

	fetched, err := movies.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get movie: %v", err)
	}
	if fetched.Title != title || fetched.DurationMinutes != 100 {
		t.Errorf("round trip mismatch: %+v", fetched)
	}

	scr, err := screenings.Create(ctx, domainmovie.Screening{
		MovieID:     created.ID,
		StartsAt:    time.Now().Add(24 * time.Hour).Truncate(time.Minute),
		Rows:        []string{"A", "B", "C"},
		SeatsPerRow: 4,
		PriceCents:  1500,
	})
	if err != nil {
		t.Fatalf("create screening: %v", err)
	}

	// Geometry must survive the trip through TEXT[] and back — this is the
	// data the hold validation and seat map rely on.
	got, err := screenings.Get(ctx, scr.ID)
	if err != nil {
		t.Fatalf("get screening: %v", err)
	}
	if got.SeatCount() != 12 {
		t.Errorf("SeatCount = %d, want 12", got.SeatCount())
	}
	if !got.HasSeat(seat(t, "B", 3)) || got.HasSeat(seat(t, "D", 1)) {
		t.Error("screening geometry wrong after round trip")
	}

	byMovie, err := screenings.ListByMovie(ctx, created.ID)
	if err != nil || len(byMovie) != 1 {
		t.Fatalf("ListByMovie = %d screenings, err %v; want 1", len(byMovie), err)
	}

	if _, err := movies.Get(ctx, uuid.NewString()); !errors.Is(err, domainmovie.ErrMovieNotFound) {
		t.Errorf("unknown movie: err = %v, want ErrMovieNotFound", err)
	}
	if _, err := screenings.Get(ctx, uuid.NewString()); !errors.Is(err, domainmovie.ErrScreeningNotFound) {
		t.Errorf("unknown screening: err = %v, want ErrScreeningNotFound", err)
	}
}

// TestConfirmedBookingsArbiter proves the schema backstop of the
// consistency model (ADR 0006): at most one CONFIRMED booking per seat,
// cancellations free the seat, and one session can never produce two
// bookings. These are Postgres-enforced invariants — the application layer
// gets them even if its own checks have a hole.
func TestConfirmedBookingsArbiter(t *testing.T) {
	pool := connectPostgres(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	movies := postgresadapter.NewMovieRepo(pool)
	screenings := postgresadapter.NewScreeningRepo(pool)

	movie, err := movies.Create(ctx, domainmovie.Movie{Title: "Arbiter " + uuid.NewString()[:8], DurationMinutes: 90})
	if err != nil {
		t.Fatalf("create movie: %v", err)
	}
	scr, err := screenings.Create(ctx, domainmovie.Screening{
		MovieID: movie.ID, StartsAt: time.Now().Add(time.Hour),
		Rows: []string{"A"}, SeatsPerRow: 2, PriceCents: 1500,
	})
	if err != nil {
		t.Fatalf("create screening: %v", err)
	}

	alice, err := q.CreateUser(ctx, sqlcgen.CreateUserParams{
		Email: "alice-" + uuid.NewString()[:8] + "@example.com", PasswordHash: "x", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := q.CreateUser(ctx, sqlcgen.CreateUserParams{
		Email: "bob-" + uuid.NewString()[:8] + "@example.com", PasswordHash: "x", DisplayName: "Bob",
	})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}

	sessionA := uuid.NewString()
	bookingA, err := q.InsertConfirmedBooking(ctx, sqlcgen.InsertConfirmedBookingParams{
		SessionID: sessionA, ScreeningID: scr.ID, SeatRow: "A", SeatNumber: 1, UserID: alice.ID,
	})
	if err != nil {
		t.Fatalf("alice confirms seat A1: %v", err)
	}

	// 1. UNIQUE(screening,row,seat) WHERE status='confirmed' — Bob cannot
	// confirm the same seat; the database says no with a uniqueness error.
	_, err = q.InsertConfirmedBooking(ctx, sqlcgen.InsertConfirmedBookingParams{
		SessionID: uuid.NewString(), ScreeningID: scr.ID, SeatRow: "A", SeatNumber: 1, UserID: bob.ID,
	})
	if !isUniqueViolation(err) {
		t.Errorf("second confirmed booking for same seat: err = %v, want unique violation", err)
	}

	// 2. One booking per session: re-confirming Alice's session on another
	// seat must also fail (session uniqueness is the idempotency anchor).
	_, err = q.InsertConfirmedBooking(ctx, sqlcgen.InsertConfirmedBookingParams{
		SessionID: sessionA, ScreeningID: scr.ID, SeatRow: "A", SeatNumber: 2, UserID: alice.ID,
	})
	if !isUniqueViolation(err) {
		t.Errorf("second booking for same session: err = %v, want unique violation", err)
	}

	// 3. ExistsConfirmedSeat agrees with the table state.
	exists, err := q.ExistsConfirmedSeat(ctx, sqlcgen.ExistsConfirmedSeatParams{
		ScreeningID: scr.ID, SeatRow: "A", SeatNumber: 1,
	})
	if err != nil || !exists {
		t.Errorf("ExistsConfirmedSeat = %v, %v; want true", exists, err)
	}

	// 4. Cancel frees the seat: the partial index excludes cancelled rows,
	// so Bob can now book A1. Without the partial index this would fail —
	// the first cancellation would brick the seat forever.
	if _, err := q.SetBookingStatus(ctx, sqlcgen.SetBookingStatusParams{
		ID: bookingA.ID, Status: "cancelled",
	}); err != nil {
		t.Fatalf("cancel alice's booking: %v", err)
	}
	exists, err = q.ExistsConfirmedSeat(ctx, sqlcgen.ExistsConfirmedSeatParams{
		ScreeningID: scr.ID, SeatRow: "A", SeatNumber: 1,
	})
	if err != nil || exists {
		t.Errorf("ExistsConfirmedSeat after cancel = %v, %v; want false", exists, err)
	}
	if _, err := q.InsertConfirmedBooking(ctx, sqlcgen.InsertConfirmedBookingParams{
		SessionID: uuid.NewString(), ScreeningID: scr.ID, SeatRow: "A", SeatNumber: 1, UserID: bob.ID,
	}); err != nil {
		t.Errorf("rebooking a cancelled seat must succeed: %v", err)
	}

	// 5. Append-only record: the cancelled row is still there.
	kept, err := q.GetBookingBySession(ctx, sessionA)
	if err != nil || kept.Status != "cancelled" {
		t.Errorf("cancelled booking must remain in the table: %+v, %v", kept, err)
	}
}

// TestUsersEmailCaseInsensitive pins the lower(email) unique index: the
// same address in different cases is one user.
func TestUsersEmailCaseInsensitive(t *testing.T) {
	pool := connectPostgres(t)
	ctx := context.Background()
	q := sqlcgen.New(pool)

	base := uuid.NewString()[:8] + "@example.com"
	if _, err := q.CreateUser(ctx, sqlcgen.CreateUserParams{
		Email: "First-" + base, PasswordHash: "x",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := q.CreateUser(ctx, sqlcgen.CreateUserParams{
		Email: "FIRST-" + base, PasswordHash: "x",
	})
	if !isUniqueViolation(err) {
		t.Errorf("duplicate email differing only by case: err = %v, want unique violation", err)
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func seat(t *testing.T, row string, number int) booking.Seat {
	t.Helper()
	s, err := booking.NewSeat(row, number)
	if err != nil {
		t.Fatalf("bad test seat: %v", err)
	}
	return s
}
