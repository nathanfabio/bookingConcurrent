package user

import "time"

// RefreshToken is one row of the server-side session ledger
// (migrations/00006, ADR 0004). The presented credential itself is never
// stored: only TokenHash (SHA-256 hex of the opaque token), so a database
// read leaks nothing usable.
//
// Lifecycle state is carried explicitly instead of via a Status enum:
// a token is ACTIVE until it is replaced by rotation, revoked by
// logout/theft, or expires — and the three are mutually detectable from
// the row alone, which is what makes the reuse rule (ReuseIsTheft) pure.
type RefreshToken struct {
	ID        string
	UserID    string
	FamilyID  string
	TokenHash string
	ExpiresAt time.Time
	CreatedAt time.Time
	// RevokedAt is nil while the token has not been revoked. Logout and
	// theft detection set it on every live token of the family.
	RevokedAt *time.Time
	// ReplacedBy is "" until rotation mints the successor. A presented
	// token with ReplacedBy set means someone is replaying a credential
	// that has already been consumed — theft (ADR 0004).
	ReplacedBy string
}

// IsExpired reports whether the token's TTL has passed. The boundary is
// INCLUSIVE — now == ExpiresAt is expired — matching the "expires_at is
// the last instant the token is NOT valid" reading used by the stores.
func (t RefreshToken) IsExpired(now time.Time) bool {
	return !now.Before(t.ExpiresAt)
}

// IsRevoked reports whether the token (or its whole family) has been
// revoked by logout or theft detection.
func (t RefreshToken) IsRevoked() bool {
	return t.RevokedAt != nil
}

// IsReplaced reports whether rotation has already consumed this token in
// favor of a successor.
func (t RefreshToken) IsReplaced() bool {
	return t.ReplacedBy != ""
}

// IsActive reports whether the token may legally mint a successor right
// now. Both the Postgres Rotate transaction and the memory fake evaluate
// this predicate — it is the single shared definition of "active"
// (ADR 0004).
func (t RefreshToken) IsActive(now time.Time) bool {
	return !t.IsRevoked() && !t.IsReplaced() && !t.IsExpired(now)
}

// ReuseIsTheft encodes the ADR 0004 invariant: presenting a token that is
// already replaced OR revoked means someone is replaying a stolen or
// discarded credential — either way the whole family must be revoked.
// Expiry is deliberately NOT here: an expired token is normal lifecycle,
// not an attack.
func (t RefreshToken) ReuseIsTheft() bool {
	return t.IsReplaced() || t.IsRevoked()
}
