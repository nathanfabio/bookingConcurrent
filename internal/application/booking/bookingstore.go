package booking

import (
	"context"

	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
)

// BookingStore is the port for the durable sales record — the confirmed
// bookings and the confirm-time checks around them (CLAUDE.md §2, ADR 0003).
//
// The Postgres implementation is the arbiter of last resort: the partial
// unique index on (screening, seat) WHERE status='confirmed' is what makes
// "exactly one confirmed booking per seat" true under any interleaving
// (ADR 0006). Implementations surface the domain sentinels below so use
// cases stay implementation-agnostic.
type BookingStore interface {
	// Confirm atomically persists a confirmed booking AND its
	// BookingConfirmed outbox row as ONE unit — the commit point of the
	// whole booking flow (ADR 0003: "booking row + outbox row" in a single
	// Postgres transaction; ADR 0006: the full consistency model). Either
	// both land or neither does; the implementation owns the mechanism
	// (a transaction in Postgres, a mutex-guarded pair of maps in the
	// fake), and the outbox row is NOT a separate port concern on purpose:
	// publishing it is M6's relay job, writing it is part of committing.
	//
	// b arrives with ID empty (the store's database assigns identity, same
	// as every other Postgres row) and Status domain.StatusConfirmed.
	//
	// Errors: domain.ErrSeatAlreadyBooked (another booking already holds the
	// seat's confirmed slot — lost the race to the partial unique index),
	// domain.ErrSessionAlreadyConfirmed (this session already produced a
	// booking row — an idempotent replay; the use case recovers by reading
	// the existing row back).
	Confirm(ctx context.Context, b domain.Booking) (domain.Booking, error)

	// GetBySession returns the booking a hold session produced. It is the
	// recovery half of confirm idempotency (ADR 0006): after a uniqueness
	// conflict, the use case looks the session up to distinguish "I already
	// confirmed this" from "someone else won the seat".
	//
	// Errors: domain.ErrBookingNotFound when the session never confirmed.
	GetBySession(ctx context.Context, sessionID string) (domain.Booking, error)

	// SeatConfirmed reports whether a CONFIRMED booking already occupies the
	// seat. This is the pre-hold phantom-hold defense (ADR 0006): one
	// indexed lookup before a hold touches Redis, so the common case never
	// burns a hold slot on an already-sold seat. It is a FAST PATH, not the
	// arbiter — a seat can become confirmed between this check and the
	// hold, and the partial unique index re-checks at confirm time.
	SeatConfirmed(ctx context.Context, screeningID string, seat domain.Seat) (bool, error)

	// ConfirmedSeats returns every seat with a CONFIRMED booking for the
	// screening. It is the seat map's authoritative "booked" layer
	// (ADR 0006: availability is computed Postgres-first). Ordering is not
	// part of the contract; the seat map renders from the screening's own
	// grid.
	ConfirmedSeats(ctx context.Context, screeningID string) ([]domain.Seat, error)
}
