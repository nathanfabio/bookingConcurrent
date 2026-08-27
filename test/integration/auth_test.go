package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	domainuser "github.com/nathanfabio/bookingConcurrent/internal/domain/user"

	"github.com/google/uuid"
)

// uniqueEmail returns a collision-proof email so parallel test runs and the
// persistent test database never fight over a fixture.
func uniqueEmail(local string) string {
	return local + "-" + uuid.NewString()[:8] + "@example.com"
}

// TestUserRepoRoundTrip covers the UserRepo against real Postgres: create,
// case-insensitive lookups, not-found mapping, and the unique-index race
// surfaced as ErrEmailTaken.
func TestUserRepoRoundTrip(t *testing.T) {
	pool := connectPostgres(t)
	ctx := context.Background()
	users := postgresadapter.NewUserRepo(pool)

	email := uniqueEmail("repo")
	created, err := users.Create(ctx, domainuser.User{
		Email:        email,
		PasswordHash: "$argon2id$v=19$m=64,t=1,p=1$c2FsdA$aGFzaA",
		DisplayName:  "Repo User",
		Role:         domainuser.RoleCustomer,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" {
		t.Errorf("Create did not assign an ID")
	}
	if created.CreatedAt.IsZero() {
		t.Errorf("Create did not assign CreatedAt")
	}

	// Case-insensitive lookup: the lower(email) index means an
	// upper-cased form of the same address finds the user.
	got, err := users.GetByEmail(ctx, strings.ToUpper(email))
	if err != nil {
		t.Fatalf("GetByEmail(upper-cased): %v", err)
	}
	if got.ID != created.ID || got.DisplayName != "Repo User" {
		t.Errorf("GetByEmail = %+v, want user %+v", got, created)
	}

	byID, err := users.GetByID(ctx, created.ID)
	if err != nil || byID.Email != email {
		t.Errorf("GetByID = %+v, %v; want %s", byID, err, email)
	}

	if _, err := users.GetByEmail(ctx, uniqueEmail("missing")); !errors.Is(err, domainuser.ErrUserNotFound) {
		t.Errorf("GetByEmail(missing) = %v, want ErrUserNotFound", err)
	}
	if _, err := users.GetByID(ctx, uuid.NewString()); !errors.Is(err, domainuser.ErrUserNotFound) {
		t.Errorf("GetByID(missing) = %v, want ErrUserNotFound", err)
	}

	// The unique index turns a duplicate into the domain sentinel.
	_, err = users.Create(ctx, domainuser.User{Email: email, PasswordHash: "x", Role: domainuser.RoleCustomer})
	if !errors.Is(err, domainuser.ErrEmailTaken) {
		t.Errorf("duplicate Create = %v, want ErrEmailTaken", err)
	}
}

// TestUserRepoDuplicateEmailRace proves the port contract against the real
// unique index: many concurrent registrations of the same email yield
// exactly one winner, the rest ErrEmailTaken.
func TestUserRepoDuplicateEmailRace(t *testing.T) {
	pool := connectPostgres(t)
	ctx := context.Background()
	users := postgresadapter.NewUserRepo(pool)

	email := uniqueEmail("race")
	const n = 16

	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		taken   int
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, err := users.Create(ctx, domainuser.User{Email: email, PasswordHash: "x", Role: domainuser.RoleCustomer})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, domainuser.ErrEmailTaken):
				taken++
			default:
				t.Errorf("Create returned unexpected error %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
	if taken != n-1 {
		t.Errorf("ErrEmailTaken = %d, want %d", taken, n-1)
	}
}

// newTokenRow builds a refresh-token row with a fresh identity, for the
// repo lifecycle/race tests. userID must reference a real users row
// (refresh_tokens.user_id is a foreign key).
func newTokenRow(userID, familyID, tokenHash string, expiresAt time.Time) domainuser.RefreshToken {
	return domainuser.RefreshToken{
		ID:        uuid.NewString(),
		UserID:    userID,
		FamilyID:  familyID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
	}
}

// createTestUser inserts a throwaway user and returns its ID, so
// refresh-token rows have a real user to reference.
func createTestUser(t *testing.T, ctx context.Context, users *postgresadapter.UserRepo) string {
	t.Helper()
	u, err := users.Create(ctx, domainuser.User{
		Email:        uniqueEmail("tok"),
		PasswordHash: "x",
		Role:         domainuser.RoleCustomer,
	})
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	return u.ID
}

// TestRefreshTokenRepoLifecycle exercises the full rotation state machine
// against real Postgres: rotate, then replay the consumed token to trigger
// theft detection, which must revoke EVERY token in the family.
func TestRefreshTokenRepoLifecycle(t *testing.T) {
	pool := connectPostgres(t)
	ctx := context.Background()
	tokens := postgresadapter.NewRefreshTokenRepo(pool)
	users := postgresadapter.NewUserRepo(pool)
	userID := createTestUser(t, ctx, users)

	family := uuid.NewString()
	root := newTokenRow(userID, family, "hash-root-"+uuid.NewString(), time.Now().Add(time.Hour))
	if _, err := tokens.Create(ctx, root); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Rotate: the successor inherits user+family, the old row is replaced.
	next := domainuser.RefreshToken{
		ID:        uuid.NewString(),
		TokenHash: "hash-next-" + uuid.NewString(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	rotated, err := tokens.Rotate(ctx, root.TokenHash, next)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rotated.UserID != root.UserID || rotated.FamilyID != family {
		t.Errorf("Rotate stamped %+v, want user %s family %s", rotated, root.UserID, family)
	}
	old, err := tokens.GetByHash(ctx, root.TokenHash)
	if err != nil {
		t.Fatalf("GetByHash(root): %v", err)
	}
	if old.ReplacedBy != next.ID {
		t.Errorf("root.ReplacedBy = %q, want %s", old.ReplacedBy, next.ID)
	}

	// Replay the consumed root: theft → whole family revoked.
	_, err = tokens.Rotate(ctx, root.TokenHash, domainuser.RefreshToken{
		ID: uuid.NewString(), TokenHash: "hash-thief-" + uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour),
	})
	if !errors.Is(err, domainuser.ErrRefreshTokenReused) {
		t.Fatalf("replay of consumed token = %v, want ErrRefreshTokenReused", err)
	}
	for _, hash := range []string{root.TokenHash, next.TokenHash} {
		row, err := tokens.GetByHash(ctx, hash)
		if err != nil {
			t.Fatalf("GetByHash(%s): %v", hash, err)
		}
		if row.RevokedAt == nil {
			t.Errorf("token %s not revoked after theft; want family-wide revocation", hash)
		}
	}

	// The innocent successor is now dead too (its family is revoked).
	if _, err := tokens.Rotate(ctx, next.TokenHash, domainuser.RefreshToken{
		ID: uuid.NewString(), TokenHash: "hash-after-" + uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour),
	}); !errors.Is(err, domainuser.ErrRefreshTokenReused) {
		t.Errorf("successor after theft = %v, want ErrRefreshTokenReused", err)
	}
}

// TestRefreshTokenRepoExpiryAndUnknown: expiry is normal lifecycle (no
// family revocation); an unknown hash is ErrRefreshTokenUnknown.
func TestRefreshTokenRepoExpiryAndUnknown(t *testing.T) {
	pool := connectPostgres(t)
	ctx := context.Background()
	tokens := postgresadapter.NewRefreshTokenRepo(pool)
	users := postgresadapter.NewUserRepo(pool)
	userID := createTestUser(t, ctx, users)

	expired := newTokenRow(userID, uuid.NewString(), "hash-exp-"+uuid.NewString(), time.Now().Add(-time.Hour))
	if _, err := tokens.Create(ctx, expired); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := tokens.Rotate(ctx, expired.TokenHash, domainuser.RefreshToken{
		ID: uuid.NewString(), TokenHash: "hash-x-" + uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour),
	}); !errors.Is(err, domainuser.ErrRefreshTokenExpired) {
		t.Errorf("Rotate(expired) = %v, want ErrRefreshTokenExpired", err)
	}

	if _, err := tokens.Rotate(ctx, "hash-no-such", domainuser.RefreshToken{
		ID: uuid.NewString(), TokenHash: "hash-y-" + uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour),
	}); !errors.Is(err, domainuser.ErrRefreshTokenUnknown) {
		t.Errorf("Rotate(unknown) = %v, want ErrRefreshTokenUnknown", err)
	}
}

// TestRefreshTokenRepoConcurrentRotate is the FOR UPDATE proof on real
// Postgres: N concurrent Rotates of the same token yield exactly one
// success; every loser observes reuse and the family is revoked.
func TestRefreshTokenRepoConcurrentRotate(t *testing.T) {
	pool := connectPostgres(t)
	ctx := context.Background()
	tokens := postgresadapter.NewRefreshTokenRepo(pool)
	users := postgresadapter.NewUserRepo(pool)
	userID := createTestUser(t, ctx, users)

	root := newTokenRow(userID, uuid.NewString(), "hash-crace-"+uuid.NewString(), time.Now().Add(time.Hour))
	if _, err := tokens.Create(ctx, root); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const n = 16
	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		reused  int
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			next := domainuser.RefreshToken{
				ID:        uuid.NewString(),
				TokenHash: "hash-crace-next-" + uuid.NewString(),
				ExpiresAt: time.Now().Add(time.Hour),
			}
			_, err := tokens.Rotate(ctx, root.TokenHash, next)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, domainuser.ErrRefreshTokenReused):
				reused++
			default:
				t.Errorf("Rotate returned unexpected error %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1 (FOR UPDATE must serialize the rotation)", winners)
	}
	if reused != n-1 {
		t.Errorf("reuse errors = %d, want %d", reused, n-1)
	}
	after, err := tokens.GetByHash(ctx, root.TokenHash)
	if err != nil {
		t.Fatalf("GetByHash(root): %v", err)
	}
	if after.RevokedAt == nil {
		t.Errorf("family not revoked after concurrent reuse")
	}
}

// TestRefreshTokenRepoRevokeFamilyIsolation: revoking one family leaves
// another untouched.
func TestRefreshTokenRepoRevokeFamilyIsolation(t *testing.T) {
	pool := connectPostgres(t)
	ctx := context.Background()
	tokens := postgresadapter.NewRefreshTokenRepo(pool)
	users := postgresadapter.NewUserRepo(pool)
	userA := createTestUser(t, ctx, users)
	userB := createTestUser(t, ctx, users)

	famA := uuid.NewString()
	famB := uuid.NewString()
	hashA := "hash-iso-a-" + uuid.NewString()
	hashB := "hash-iso-b-" + uuid.NewString()
	if _, err := tokens.Create(ctx, newTokenRow(userA, famA, hashA, time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("Create A: %v", err)
	}
	if _, err := tokens.Create(ctx, newTokenRow(userB, famB, hashB, time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("Create B: %v", err)
	}

	if err := tokens.RevokeFamily(ctx, famA); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}

	a, err := tokens.GetByHash(ctx, hashA)
	if err != nil {
		t.Fatalf("GetByHash(A): %v", err)
	}
	b, err := tokens.GetByHash(ctx, hashB)
	if err != nil {
		t.Fatalf("GetByHash(B): %v", err)
	}
	if a.RevokedAt == nil {
		t.Errorf("family A token not revoked")
	}
	if b.RevokedAt != nil {
		t.Errorf("family B token revoked; revocation leaked across families")
	}

	// Idempotent: revoking again is a no-op, not an error.
	if err := tokens.RevokeFamily(ctx, famA); err != nil {
		t.Errorf("second RevokeFamily = %v, want nil (idempotent)", err)
	}
}
