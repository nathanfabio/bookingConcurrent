// Package booking holds the domain model for seat holds and confirmed
// bookings (CLAUDE.md §4).
//
// Everything here is pure Go: no I/O, no infrastructure imports, no
// goroutines. State-dependent business rules live here as methods; anything
// that needs a store lives in the application layer.
package booking

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Domain errors returned by hold/booking operations. The HTTP layer maps
// these to status codes centrally; the identity (errors.Is) is what callers
// branch on, so the messages are for humans and logs.
var (
	// ErrSeatAlreadyHeld: the seat is currently held by someone (SET NX lost).
	ErrSeatAlreadyHeld = errors.New("booking: seat is already held")
	// ErrHoldLimitExceeded: the user already holds the maximum number of seats.
	ErrHoldLimitExceeded = errors.New("booking: hold limit exceeded")
	// ErrHoldNotFound: no live hold exists for this session (expired,
	// released, or never existed). Deliberately indistinguishable at the
	// HTTP layer from "someone else's hold" (CLAUDE.md §3).
	ErrHoldNotFound = errors.New("booking: hold not found")
	// ErrNotHoldOwner: a hold exists for this session but belongs to a
	// different user.
	ErrNotHoldOwner = errors.New("booking: not the owner of this hold")
)

// Status is the typed state of a confirmed booking. A bare string with magic
// values scattered through the codebase is exactly what CLAUDE.md §4
// forbids, hence the type + constants + Valid().
type Status string

const (
	// StatusConfirmed: payment captured, seat permanently sold.
	StatusConfirmed Status = "confirmed"
	// StatusCancelled: confirmed, then refunded/released after the fact.
	// The row is kept (append-only sales record) with its status flipped —
	// the partial unique index in Postgres only enforces uniqueness among
	// 'confirmed' rows, so a cancelled seat becomes bookable again.
	StatusCancelled Status = "cancelled"
)

// Valid reports whether s is a known booking status.
func (s Status) Valid() bool {
	switch s {
	case StatusConfirmed, StatusCancelled:
		return true
	default:
		return false
	}
}

// Seat identifies one physical seat for one screening: a row label plus a
// seat number within the row. Screening geometry (which rows/numbers exist)
// is validated by the use case against the screening, not here — Seat only
// enforces its own shape.
type Seat struct {
	Row    string
	Number int
}

// NewSeat validates and builds a Seat. Row labels are 1-3 letters
// (multi-char rows like "AA" stay representable); numbers start at 1.
func NewSeat(row string, number int) (Seat, error) {
	row = strings.ToUpper(strings.TrimSpace(row))
	if row == "" || len(row) > 3 {
		return Seat{}, fmt.Errorf("booking: invalid seat row %q (1-3 letters)", row)
	}
	for _, r := range row {
		if r < 'A' || r > 'Z' {
			return Seat{}, fmt.Errorf("booking: invalid seat row %q (letters only)", row)
		}
	}
	if number < 1 {
		return Seat{}, fmt.Errorf("booking: invalid seat number %d (must be >= 1)", number)
	}
	return Seat{Row: row, Number: number}, nil
}

// Hold is a transient claim on a seat while the buyer completes checkout.
//
// It is deliberately a value with no behavior beyond pure time/ownership
// checks: creating, expiring, and releasing holds involves I/O and belongs
// to the HoldStore adapter + use cases, not to this type.
type Hold struct {
	// SessionID is the opaque handle the client uses from now on. It maps to
	// exactly one seat (the reverse-lookup key from the reference design).
	SessionID string
	// ScreeningID scopes the hold: seat A1 for Tuesday's showing and seat A1
	// for Friday's are independent claims.
	ScreeningID string
	Seat        Seat
	UserID      string
	// HoldToken is a secret-per-hold UUID stored as the seat key's value.
	// Release and the expiry sweeper compare it before deleting anything,
	// which is what stops a stale operation from one hold deleting state
	// that now belongs to a different hold of the same seat (see ADR 0002).
	HoldToken string
	ExpiresAt time.Time
}

// IsExpired reports whether the hold's TTL window has passed at time now.
// In Redis the keys evaporate on their own; this method exists for the
// in-memory fake and for pure business checks that must not trust the store.
func (h Hold) IsExpired(now time.Time) bool {
	return !now.Before(h.ExpiresAt)
}

// Validate checks that a hold is well-formed and still has a future expiry
// window. Use cases call this before touching any store: a hold that is
// malformed or already expired at creation time is a programming or
// clock-skew error and must never reach Redis.
func (h Hold) Validate(now time.Time) error {
	if h.SessionID == "" {
		return errors.New("booking: hold requires a session ID")
	}
	if h.ScreeningID == "" {
		return errors.New("booking: hold requires a screening ID")
	}
	if h.UserID == "" {
		return errors.New("booking: hold requires a user ID")
	}
	if h.HoldToken == "" {
		return errors.New("booking: hold requires a hold token")
	}
	if h.Seat.Row == "" || h.Seat.Number < 1 {
		return errors.New("booking: hold requires a valid seat")
	}
	if h.ExpiresAt.IsZero() || !now.Before(h.ExpiresAt) {
		return errors.New("booking: hold expiry must be in the future")
	}
	return nil
}

// CanBeConfirmed encodes the state-dependent rule for confirmation
// (CLAUDE.md §4): a hold is confirmable only inside its TTL window. The
// expired-hold case is a hard business NO — payment already authorized or
// not, an expired hold cannot become a booking (see ADR 0006). Ownership
// and payment checks are use-case concerns, not this method's.
func (h Hold) CanBeConfirmed(now time.Time) bool {
	return !h.IsExpired(now)
}

// Booking is the durable sales record created when a hold is confirmed.
// Append-only: it is never deleted, only transitioned (e.g. to cancelled).
type Booking struct {
	ID          string
	SessionID   string
	ScreeningID string
	Seat        Seat
	UserID      string
	Status      Status
	ConfirmedAt time.Time
}

// CanBeCancelled: only a confirmed booking can be cancelled. Kept here (not
// in a use case) because it is a pure state-transition rule.
func (b Booking) CanBeCancelled() bool {
	return b.Status == StatusConfirmed
}
