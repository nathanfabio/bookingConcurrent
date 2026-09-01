package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres/sqlcgen"
	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Compile-time proof the adapters satisfy the ports they back.
var (
	_ appbooking.BookingStore   = (*BookingRepo)(nil)
	_ appbooking.ScreeningStore = (*ScreeningRepo)(nil)
)

// Constraint names from migrations/00003_confirmed_bookings.sql. These are
// the schema artifacts the confirm conflict mapping branches on, so they are
// pinned here (and asserted against the live database in the integration
// tests) rather than buried inline. Renaming an index in a migration is a
// legitimate change; silently turning every booking conflict into a 500 is
// not, so an unrecognized constraint is logged loudly and surfaced as a
// generic error instead of being guessed at.
const (
	constraintSeatUnique    = "confirmed_bookings_seat_unique"
	constraintSessionUnique = "confirmed_bookings_session_unique"
)

// bookingConfirmedEventType labels the outbox row written atomically with a
// confirmed booking. The relay in cmd/worker (M6) publishes it to the broker.
const bookingConfirmedEventType = "BookingConfirmed"

// BookingRepo implements booking.BookingStore against Postgres. It holds the
// pool directly (like RefreshTokenRepo) because Confirm composes the booking
// insert and the outbox insert inside ONE transaction — the commit point of
// the whole flow (ADR 0003, ADR 0006).
type BookingRepo struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewBookingRepo builds the repository over the shared pool.
func NewBookingRepo(pool *pgxpool.Pool) *BookingRepo {
	return &BookingRepo{pool: pool, q: sqlcgen.New(pool)}
}

// bookingConfirmedPayload is the JSON written to the outbox alongside the
// booking row. Its shape is defined now because the M6 relay (and its
// consumers) read it; snake_case is enforced by a reflection test in this
// package. Keep field names stable — they are part of the event contract.
type bookingConfirmedPayload struct {
	BookingID   string    `json:"booking_id"`
	SessionID   string    `json:"session_id"`
	ScreeningID string    `json:"screening_id"`
	Row         string    `json:"row"`
	Number      int       `json:"number"`
	UserID      string    `json:"user_id"`
	ConfirmedAt time.Time `json:"confirmed_at"`
}

// Confirm atomically writes the confirmed booking and its BookingConfirmed
// outbox row. The two land or roll back together; there is no intermediate
// state where the sale is recorded but the event is not queued (ADR 0003's
// commit point, ADR 0006's consistency model).
//
// Conflicts are surfaced as domain sentinels by reading WHICH unique index
// fired: the seat arbiter means someone else owns the seat, the session
// index means this session already confirmed (an idempotent replay the use
// case recovers from).
func (r *BookingRepo) Confirm(ctx context.Context, b domain.Booking) (domain.Booking, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Booking{}, fmt.Errorf("booking: confirm: begin: %w", err)
	}
	// Commit is the explicit success path; anything else rolls back.
	defer func() { _ = tx.Rollback(ctx) }()

	q := r.q.WithTx(tx)

	row, err := q.InsertConfirmedBooking(ctx, sqlcgen.InsertConfirmedBookingParams{
		SessionID:   b.SessionID,
		ScreeningID: b.ScreeningID,
		SeatRow:     b.Seat.Row,
		SeatNumber:  int32(b.Seat.Number),
		UserID:      b.UserID,
	})
	if err != nil {
		if conflict := bookingConflictError(ctx, err); conflict != nil {
			return domain.Booking{}, conflict
		}
		return domain.Booking{}, fmt.Errorf("booking: confirm: insert: %w", err)
	}

	// The booking row is the source of truth for the event payload: use the
	// database-assigned id and confirmed_at, not the caller's inputs. The
	// event id is minted here because it is a per-transaction identity — one
	// committed transaction produces exactly one event (M6 consumers dedupe
	// on it).
	payload, err := json.Marshal(bookingConfirmedPayload{
		BookingID:   row.ID,
		SessionID:   row.SessionID,
		ScreeningID: row.ScreeningID,
		Row:         row.SeatRow,
		Number:      int(row.SeatNumber),
		UserID:      row.UserID,
		ConfirmedAt: ts(row.ConfirmedAt),
	})
	if err != nil {
		return domain.Booking{}, fmt.Errorf("booking: confirm: encode outbox payload: %w", err)
	}
	if err := q.InsertOutboxEvent(ctx, sqlcgen.InsertOutboxEventParams{
		EventID:   uuid.NewString(),
		EventType: bookingConfirmedEventType,
		Payload:   payload,
	}); err != nil {
		return domain.Booking{}, fmt.Errorf("booking: confirm: outbox: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Booking{}, fmt.Errorf("booking: confirm: commit: %w", err)
	}
	return bookingFromRow(row), nil
}

// GetBySession implements booking.BookingStore.
func (r *BookingRepo) GetBySession(ctx context.Context, sessionID string) (domain.Booking, error) {
	row, err := r.q.GetBookingBySession(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Booking{}, domain.ErrBookingNotFound
	}
	if err != nil {
		return domain.Booking{}, fmt.Errorf("booking: get by session: %w", err)
	}
	return bookingFromRow(row), nil
}

// SeatConfirmed implements booking.BookingStore.
func (r *BookingRepo) SeatConfirmed(ctx context.Context, screeningID string, seat domain.Seat) (bool, error) {
	exists, err := r.q.ExistsConfirmedSeat(ctx, sqlcgen.ExistsConfirmedSeatParams{
		ScreeningID: screeningID,
		SeatRow:     seat.Row,
		SeatNumber:  int32(seat.Number),
	})
	if err != nil {
		return false, fmt.Errorf("booking: seat confirmed: %w", err)
	}
	return exists, nil
}

// ConfirmedSeats implements booking.BookingStore.
func (r *BookingRepo) ConfirmedSeats(ctx context.Context, screeningID string) ([]domain.Seat, error) {
	rows, err := r.q.ListConfirmedSeatsByScreening(ctx, screeningID)
	if err != nil {
		return nil, fmt.Errorf("booking: confirmed seats: %w", err)
	}
	seats := make([]domain.Seat, 0, len(rows))
	for _, row := range rows {
		seats = append(seats, domain.Seat{Row: row.SeatRow, Number: int(row.SeatNumber)})
	}
	return seats, nil
}

// bookingConflictError maps a uniqueness violation from the confirm insert to
// the domain sentinel for the index that fired, or returns nil when err is
// not a recognized booking conflict (leaving the caller to wrap it as a
// generic failure). An unrecognized constraint name on a 23505 is logged
// loudly: it means the schema changed and this mapping drifted, and guessing
// a sentinel would corrupt the conflict signal.
func bookingConflictError(ctx context.Context, err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return nil
	}
	switch pgErr.ConstraintName {
	case constraintSeatUnique:
		return domain.ErrSeatAlreadyBooked
	case constraintSessionUnique:
		return domain.ErrSessionAlreadyConfirmed
	default:
		slog.ErrorContext(ctx, "confirm insert hit an unrecognized unique constraint",
			slog.String("constraint", pgErr.ConstraintName),
			slog.String("sqlstate", pgErr.Code))
		return nil
	}
}

func bookingFromRow(row sqlcgen.ConfirmedBooking) domain.Booking {
	return domain.Booking{
		ID:          row.ID,
		SessionID:   row.SessionID,
		ScreeningID: row.ScreeningID,
		Seat:        domain.Seat{Row: row.SeatRow, Number: int(row.SeatNumber)},
		UserID:      row.UserID,
		Status:      domain.Status(row.Status),
		ConfirmedAt: ts(row.ConfirmedAt),
	}
}
