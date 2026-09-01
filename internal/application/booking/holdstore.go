// Package booking contains the booking use cases and the ports they depend
// on. Per ADR 0001, ports live here — next to the code that calls them —
// not in a central ports package and not next to the adapters that
// implement them.
package booking

import (
	"context"

	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
)

// HoldStore is the port for the transient seat-hold state (CLAUDE.md §2).
//
// Implementations must make each operation ATOMIC with respect to
// concurrent callers — two Hold calls for the same seat must yield exactly
// one success, and Release must never delete state belonging to a different
// hold of the same seat. The Redis adapter achieves this with Lua scripts
// (ADR 0002); the in-memory fake achieves it with a mutex.
//
// All implementations surface the same domain errors from
// internal/domain/booking so use cases can stay implementation-agnostic.
type HoldStore interface {
	// Hold atomically claims the seat for hold.SessionID if and only if:
	// the seat is not currently held, the user is under their hold limit,
	// and no conflicting hold bookkeeping exists. The claim self-expires at
	// hold.ExpiresAt (the store's TTL mechanism).
	//
	// Errors: domain.ErrSeatAlreadyHeld, domain.ErrHoldLimitExceeded.
	Hold(ctx context.Context, hold domain.Hold) error

	// Release atomically gives up the hold for sessionID, but only if the
	// caller is its owner AND the seat key still belongs to this hold
	// (compare-and-delete on the hold token — a late release must never
	// delete a seat some other hold has since acquired).
	//
	// Errors: domain.ErrHoldNotFound (unknown or expired session),
	// domain.ErrNotHoldOwner.
	Release(ctx context.Context, sessionID, userID string) error

	// Get returns the live hold for sessionID.
	//
	// Errors: domain.ErrHoldNotFound for unknown or expired sessions.
	// Ownership checks are the caller's job (compare Hold.UserID).
	Get(ctx context.Context, sessionID string) (*domain.Hold, error)

	// HeldSeats returns every seat currently held for screeningID. It backs
	// the seat map's "held" layer (ADR 0006).
	//
	// SNAPSHOT SEMANTICS: seats acquired or released while the enumeration
	// runs may or may not appear. That is fine by design — the seat map is
	// advisory (a rendering of "right now"), and the atomicity that matters
	// lives in Hold/Confirm, not here. Implementations must never fail the
	// whole enumeration over a single corrupt entry: skip it and move on.
	HeldSeats(ctx context.Context, screeningID string) ([]domain.Seat, error)
}
