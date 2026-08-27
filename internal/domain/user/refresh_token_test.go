package user

import (
	"testing"
	"time"
)

var epoch = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

// validToken returns a token that is active at epoch: it expires one hour
// later and has been neither revoked nor replaced.
func validToken() RefreshToken {
	return RefreshToken{
		ID:        "tok-1",
		UserID:    "user-1",
		FamilyID:  "fam-1",
		TokenHash: "hash-1",
		ExpiresAt: epoch.Add(time.Hour),
		CreatedAt: epoch.Add(-time.Hour),
	}
}

func TestRefreshTokenIsExpired(t *testing.T) {
	tok := validToken()
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"well before", epoch.Add(-time.Minute), false},
		{"one instant before", tok.ExpiresAt.Add(-time.Nanosecond), false},
		{"boundary is inclusive", tok.ExpiresAt, true},
		{"well after", epoch.Add(2 * time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tok.IsExpired(tc.now); got != tc.want {
				t.Errorf("IsExpired(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

func TestRefreshTokenIsActive(t *testing.T) {
	revoked := validToken()
	revokedAt := epoch
	revoked.RevokedAt = &revokedAt

	replaced := validToken()
	replaced.ReplacedBy = "tok-2"

	expired := validToken()
	expired.ExpiresAt = epoch.Add(-time.Second)

	cases := []struct {
		name string
		tok  RefreshToken
		want bool
	}{
		{"fresh token", validToken(), true},
		{"revoked", revoked, false},
		{"replaced", replaced, false},
		{"expired", expired, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tok.IsActive(epoch); got != tc.want {
				t.Errorf("IsActive = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRefreshTokenReuseIsTheft(t *testing.T) {
	revoked := validToken()
	revokedAt := epoch
	revoked.RevokedAt = &revokedAt

	replaced := validToken()
	replaced.ReplacedBy = "tok-2"

	both := validToken()
	both.RevokedAt = &revokedAt
	both.ReplacedBy = "tok-2"

	cases := []struct {
		name string
		tok  RefreshToken
		want bool
	}{
		{"active token is not theft", validToken(), false},
		{"revoked is theft", revoked, true},
		{"replaced is theft", replaced, true},
		{"revoked and replaced is theft", both, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tok.ReuseIsTheft(); got != tc.want {
				t.Errorf("ReuseIsTheft = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestExpiredIsNotTheft pins the ADR 0004 boundary: expiry is normal
// lifecycle. An expired-but-otherwise-clean token must NOT trigger family
// revocation — otherwise merely letting a session lapse would destroy the
// whole chain, punishing ordinary lifecycle instead of an attack.
func TestExpiredIsNotTheft(t *testing.T) {
	expired := validToken()
	expired.ExpiresAt = epoch.Add(-time.Second)
	if expired.ReuseIsTheft() {
		t.Errorf("expired token classified as theft; expiry must not revoke families")
	}
}
