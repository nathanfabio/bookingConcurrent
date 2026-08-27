package memory

import (
	"context"
	"sync"
	"time"

	appauth "github.com/nathanfabio/bookingConcurrent/internal/application/auth"
	user "github.com/nathanfabio/bookingConcurrent/internal/domain/user"
)

// Compile-time proof the fake satisfies the port.
var _ appauth.RefreshTokenStore = (*RefreshTokenStore)(nil)

// RefreshTokenStore is the in-memory fake of the auth.RefreshTokenStore
// port.
//
// Faithfulness notes (where it deliberately matches Postgres behavior):
//   - Rotate implements the ADR 0004 decision table under the mutex: the
//     mutex is the fake's stand-in for SELECT ... FOR UPDATE. Both stores
//     guarantee exactly one concurrent Rotate for the same token wins, and
//     every loser observes the token already replaced — i.e. theft, which
//     revokes the whole family.
//   - Expiry is checked against the injected clock, mirroring Postgres
//     comparing expires_at to its own now(). Tests advance a ManualClock
//     instead of sleeping — the same technique the HoldStore fake uses for
//     Redis TTLs.
//   - Theft revocation is persisted before the error returns, exactly like
//     the Postgres transaction COMMITs the family revocation.
type RefreshTokenStore struct {
	mu     sync.Mutex
	clock  Clock
	byHash map[string]*user.RefreshToken
}

// NewRefreshTokenStore builds an empty fake.
func NewRefreshTokenStore(clock Clock) *RefreshTokenStore {
	return &RefreshTokenStore{
		clock:  clock,
		byHash: make(map[string]*user.RefreshToken),
	}
}

// Create implements auth.RefreshTokenStore. The row's ID is minted by the
// caller (the Service), matching the port contract.
func (s *RefreshTokenStore) Create(ctx context.Context, t user.RefreshToken) (user.RefreshToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := t
	s.byHash[t.TokenHash] = &stored
	return stored, nil
}

// GetByHash implements auth.RefreshTokenStore.
func (s *RefreshTokenStore) GetByHash(ctx context.Context, tokenHash string) (user.RefreshToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.byHash[tokenHash]
	if !ok {
		return user.RefreshToken{}, user.ErrRefreshTokenUnknown
	}
	return *row, nil
}

// Rotate implements auth.RefreshTokenStore. The whole read-decide-write is
// one critical section, so concurrent callers are serialized and see each
// other's effects — the port's atomicity contract.
func (s *RefreshTokenStore) Rotate(ctx context.Context, tokenHash string, next user.RefreshToken) (user.RefreshToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()

	old, ok := s.byHash[tokenHash]
	if !ok {
		return user.RefreshToken{}, user.ErrRefreshTokenUnknown
	}

	// Reuse (revoked or already replaced) is checked BEFORE expiry: a
	// replaced token that has also lapsed is still theft (ADR 0004).
	if old.ReuseIsTheft() {
		s.revokeFamilyLocked(old.FamilyID, now)
		return user.RefreshToken{}, user.ErrRefreshTokenReused
	}
	if old.IsExpired(now) {
		// Normal lifecycle: no revocation, the family stays usable.
		return user.RefreshToken{}, user.ErrRefreshTokenExpired
	}

	// Active: stamp the successor with the locked row's identity, persist
	// it, and mark the old token consumed.
	next.UserID = old.UserID
	next.FamilyID = old.FamilyID
	stored := next
	s.byHash[next.TokenHash] = &stored
	old.ReplacedBy = next.ID
	return stored, nil
}

// RevokeFamily implements auth.RefreshTokenStore. Idempotent.
func (s *RefreshTokenStore) RevokeFamily(ctx context.Context, familyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokeFamilyLocked(familyID, s.clock.Now())
	return nil
}

// revokeFamilyLocked marks every not-yet-revoked token of the family
// revoked. Callers must hold the mutex.
func (s *RefreshTokenStore) revokeFamilyLocked(familyID string, now time.Time) {
	for _, row := range s.byHash {
		if row.FamilyID == familyID && row.RevokedAt == nil {
			t := now
			row.RevokedAt = &t
		}
	}
}
