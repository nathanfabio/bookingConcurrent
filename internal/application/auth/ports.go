// Package auth contains the authentication use cases (register, login,
// refresh, logout) and the ports they depend on (CLAUDE.md §3, ADR 0004).
// Per ADR 0001, ports live here — next to the code that calls them — not
// in a central ports package and not next to the adapters that implement
// them.
package auth

import (
	"context"

	user "github.com/nathanfabio/bookingConcurrent/internal/domain/user"
)

// UserStore is the port for durable user records (migrations/00001).
//
// Email uniqueness is case-insensitive and enforced by the store (the
// Postgres unique index on lower(email); the in-memory fake mirrors it).
// Create must therefore be safe under concurrent registration of the same
// email — exactly one Create succeeds, the rest return user.ErrEmailTaken.
type UserStore interface {
	// Create persists a new user. The ID and CreatedAt are assigned by the
	// store if left zero on u.
	//
	// Errors: user.ErrEmailTaken.
	Create(ctx context.Context, u user.User) (user.User, error)

	// GetByEmail matches case-insensitively.
	//
	// Errors: user.ErrUserNotFound.
	GetByEmail(ctx context.Context, email string) (user.User, error)

	// GetByID loads one user by primary key.
	//
	// Errors: user.ErrUserNotFound.
	GetByID(ctx context.Context, id string) (user.User, error)
}

// RefreshTokenStore is the port for the rotating refresh-token ledger
// (migrations/00006, ADR 0004).
//
// The atomicity contract is the interesting part and mirrors HoldStore's
// (ADR 0002): Rotate must behave as one indivisible read-decide-write. Two
// concurrent Rotates presenting the same token must yield exactly one
// success; every loser must observe the token already consumed and report
// it as reuse, revoking the whole family. The Postgres adapter achieves
// this with SELECT ... FOR UPDATE inside a transaction; the in-memory fake
// achieves it with a mutex.
type RefreshTokenStore interface {
	// Create persists a new token row. All fields — including ID — are
	// minted by the caller (the Service) so both implementations agree on
	// who assigns identity.
	Create(ctx context.Context, t user.RefreshToken) (user.RefreshToken, error)

	// GetByHash loads one token row by its SHA-256 hash. Used by Logout to
	// find which family a presented token belongs to.
	//
	// Errors: user.ErrRefreshTokenUnknown.
	GetByHash(ctx context.Context, tokenHash string) (user.RefreshToken, error)

	// Rotate atomically swaps the token identified by tokenHash for next.
	// next arrives with UserID and FamilyID empty: the store stamps them
	// from the row it locks, so the caller never reads state outside the
	// critical section.
	//
	// Semantics, evaluated on the locked row (ADR 0004 decision table):
	//   - no row            -> user.ErrRefreshTokenUnknown
	//   - revoked/replaced  -> revoke the ENTIRE family, then return
	//     user.ErrRefreshTokenReused (the revocation is the point of the
	//     transaction, so it must be persisted, not rolled back)
	//   - expired           -> user.ErrRefreshTokenExpired (normal
	//     lifecycle: no revocation)
	//   - active            -> persist next, mark the old row replaced by
	//     next.ID, return next
	//
	// Errors: user.ErrRefreshTokenUnknown, user.ErrRefreshTokenExpired,
	// user.ErrRefreshTokenReused.
	Rotate(ctx context.Context, tokenHash string, next user.RefreshToken) (user.RefreshToken, error)

	// RevokeFamily marks every not-yet-revoked token in familyID revoked.
	// Idempotent: revoking an already-revoked family is a no-op.
	RevokeFamily(ctx context.Context, familyID string) error
}
