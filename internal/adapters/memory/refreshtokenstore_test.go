package memory

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	user "github.com/nathanfabio/bookingConcurrent/internal/domain/user"
)

var tokenEpoch = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

// activeRow returns a token row that is active at the given clock time.
func activeRow(hash, familyID string, expiresAt time.Time) user.RefreshToken {
	return user.RefreshToken{
		ID:        "id-" + hash,
		UserID:    "user-1",
		FamilyID:  familyID,
		TokenHash: hash,
		ExpiresAt: expiresAt,
		CreatedAt: tokenEpoch,
	}
}

func TestRefreshTokenStoreCreateAndGetByHash(t *testing.T) {
	ctx := context.Background()
	s := NewRefreshTokenStore(NewManualClock(tokenEpoch))

	row := activeRow("hash-a", "fam-1", tokenEpoch.Add(time.Hour))
	if _, err := s.Create(ctx, row); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.GetByHash(ctx, "hash-a")
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got.ID != row.ID || got.FamilyID != "fam-1" {
		t.Errorf("GetByHash = %+v, want row %+v", got, row)
	}

	if _, err := s.GetByHash(ctx, "missing"); !errors.Is(err, user.ErrRefreshTokenUnknown) {
		t.Errorf("GetByHash(missing) = %v, want ErrRefreshTokenUnknown", err)
	}
}

func TestRefreshTokenStoreRotateDecisionTable(t *testing.T) {
	ctx := context.Background()
	ttl := time.Hour

	cases := []struct {
		name    string
		seed    func(clock *ManualClock) *RefreshTokenStore
		present string
		wantErr error
	}{
		{
			name: "row 1: unknown token",
			seed: func(clock *ManualClock) *RefreshTokenStore {
				return NewRefreshTokenStore(clock)
			},
			present: "no-such-hash",
			wantErr: user.ErrRefreshTokenUnknown,
		},
		{
			name: "row 2: revoked token is reuse",
			seed: func(clock *ManualClock) *RefreshTokenStore {
				s := NewRefreshTokenStore(clock)
				_, _ = s.Create(ctx, activeRow("h-revoked", "fam-2", tokenEpoch.Add(ttl)))
				_ = s.RevokeFamily(ctx, "fam-2")
				return s
			},
			present: "h-revoked",
			wantErr: user.ErrRefreshTokenReused,
		},
		{
			name: "row 3: replaced token is reuse",
			seed: func(clock *ManualClock) *RefreshTokenStore {
				s := NewRefreshTokenStore(clock)
				_, _ = s.Create(ctx, activeRow("h-old", "fam-3", tokenEpoch.Add(ttl)))
				// First rotation consumes h-old.
				if _, err := s.Rotate(ctx, "h-old", activeRow("h-new", "", tokenEpoch.Add(ttl))); err != nil {
					t.Fatalf("setup rotate: %v", err)
				}
				return s
			},
			present: "h-old",
			wantErr: user.ErrRefreshTokenReused,
		},
		{
			name: "row 4: expired token is not theft",
			seed: func(clock *ManualClock) *RefreshTokenStore {
				s := NewRefreshTokenStore(clock)
				_, _ = s.Create(ctx, activeRow("h-expired", "fam-4", tokenEpoch.Add(ttl)))
				clock.Advance(2 * ttl) // let it lapse
				return s
			},
			present: "h-expired",
			wantErr: user.ErrRefreshTokenExpired,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := NewManualClock(tokenEpoch)
			s := tc.seed(clock)
			_, err := s.Rotate(ctx, tc.present, activeRow("h-next", "", tokenEpoch.Add(ttl)))
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Rotate = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestRefreshTokenStoreRotateActive is row 5: a live token rotates, the
// successor inherits user+family, and the old row is marked replaced.
func TestRefreshTokenStoreRotateActive(t *testing.T) {
	ctx := context.Background()
	s := NewRefreshTokenStore(NewManualClock(tokenEpoch))
	ttl := time.Hour

	if _, err := s.Create(ctx, activeRow("h-1", "fam-1", tokenEpoch.Add(ttl))); err != nil {
		t.Fatalf("Create: %v", err)
	}
	next := user.RefreshToken{
		ID:        "id-h-2",
		TokenHash: "h-2",
		ExpiresAt: tokenEpoch.Add(2 * ttl),
		// UserID/FamilyID empty on purpose — Rotate stamps them.
	}
	got, err := s.Rotate(ctx, "h-1", next)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if got.UserID != "user-1" || got.FamilyID != "fam-1" {
		t.Errorf("Rotate stamped %+v, want user-1/fam-1", got)
	}

	old, err := s.GetByHash(ctx, "h-1")
	if err != nil {
		t.Fatalf("GetByHash(h-1): %v", err)
	}
	if old.ReplacedBy != "id-h-2" {
		t.Errorf("old.ReplacedBy = %q, want id-h-2", old.ReplacedBy)
	}
}

// TestRefreshTokenStoreReuseRevokesFamily proves the theft branch: reusing
// a consumed token revokes EVERY token in the family — including the
// innocent successor.
func TestRefreshTokenStoreReuseRevokesFamily(t *testing.T) {
	ctx := context.Background()
	s := NewRefreshTokenStore(NewManualClock(tokenEpoch))
	ttl := time.Hour

	_, _ = s.Create(ctx, activeRow("h-1", "fam-1", tokenEpoch.Add(ttl)))
	if _, err := s.Rotate(ctx, "h-1", activeRow("h-2", "", tokenEpoch.Add(ttl))); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Replay the consumed h-1: theft.
	if _, err := s.Rotate(ctx, "h-1", activeRow("h-x", "", tokenEpoch.Add(ttl))); !errors.Is(err, user.ErrRefreshTokenReused) {
		t.Fatalf("Rotate(h-1 again) = %v, want ErrRefreshTokenReused", err)
	}

	// The whole family — including the legitimate h-2 — must now be revoked.
	for _, hash := range []string{"h-1", "h-2"} {
		row, err := s.GetByHash(ctx, hash)
		if err != nil {
			t.Fatalf("GetByHash(%s): %v", hash, err)
		}
		if row.RevokedAt == nil {
			t.Errorf("token %s not revoked after theft; want family-wide revocation", hash)
		}
	}
}

// TestRefreshTokenStoreRevokeFamilyIsolation: revoking one family leaves
// another family untouched.
func TestRefreshTokenStoreRevokeFamilyIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewRefreshTokenStore(NewManualClock(tokenEpoch))

	_, _ = s.Create(ctx, activeRow("h-a", "fam-a", tokenEpoch.Add(time.Hour)))
	_, _ = s.Create(ctx, activeRow("h-b", "fam-b", tokenEpoch.Add(time.Hour)))

	if err := s.RevokeFamily(ctx, "fam-a"); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}

	a, _ := s.GetByHash(ctx, "h-a")
	b, _ := s.GetByHash(ctx, "h-b")
	if a.RevokedAt == nil {
		t.Errorf("family A token not revoked")
	}
	if b.RevokedAt != nil {
		t.Errorf("family B token revoked; revocation leaked across families")
	}
}

// TestRefreshTokenStoreRotateRace proves the atomicity contract under
// -race: many concurrent Rotates of the same token yield exactly one
// winner; every other caller observes reuse and the family is revoked.
func TestRefreshTokenStoreRotateRace(t *testing.T) {
	ctx := context.Background()
	s := NewRefreshTokenStore(NewManualClock(tokenEpoch))
	ttl := time.Hour

	if _, err := s.Create(ctx, activeRow("h-root", "fam-1", tokenEpoch.Add(ttl))); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const n = 32
	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		reused  int
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			next := user.RefreshToken{
				ID:        "id-next-" + strconv.Itoa(i),
				TokenHash: "h-next-" + strconv.Itoa(i),
				ExpiresAt: tokenEpoch.Add(ttl),
			}
			_, err := s.Rotate(ctx, "h-root", next)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, user.ErrRefreshTokenReused):
				reused++
			default:
				t.Errorf("Rotate returned unexpected error %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
	if reused != n-1 {
		t.Errorf("reuse errors = %d, want %d", reused, n-1)
	}
	root, err := s.GetByHash(ctx, "h-root")
	if err != nil {
		t.Fatalf("GetByHash(h-root): %v", err)
	}
	if root.RevokedAt == nil {
		t.Errorf("family not revoked after concurrent reuse")
	}
}
