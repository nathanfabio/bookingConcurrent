package auth_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/memory"
	appauth "github.com/nathanfabio/bookingConcurrent/internal/application/auth"
	user "github.com/nathanfabio/bookingConcurrent/internal/domain/user"
)

// newTestService wires the Service over in-memory fakes with CHEAP argon2
// parameters — production params would make the suite take seconds per
// hash. See ADR 0005 / hasher_test.go.
func newTestService(t *testing.T, clock memory.Clock) (*appauth.Service, *memory.UserStore, *memory.RefreshTokenStore) {
	t.Helper()
	users := memory.NewUserStore()
	tokens := memory.NewRefreshTokenStore(clock)
	hasher := appauth.NewArgon2Hasher(appauth.Argon2Params{
		MemoryKiB: 64, Time: 1, Parallelism: 1, KeyLen: 32, SaltLen: 16,
	})
	issuer := appauth.NewTokenIssuer("test-secret-test-secret-test-sec", "booking-api", 15*time.Minute)
	svc := appauth.NewService(users, tokens, hasher, issuer, 15*time.Minute, 720*time.Hour)
	return svc, users, tokens
}

func TestServiceRegister(t *testing.T) {
	svc, users, _ := newTestService(t, memory.NewManualClock(time.Now()))
	ctx := context.Background()

	sess, err := svc.Register(ctx, "Alice@Example.COM", "averylongpassword", "  Ada  ")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if sess.AccessToken == "" || sess.RefreshToken == "" {
		t.Errorf("Register returned empty tokens: %+v", sess)
	}
	if sess.User.Email != "alice@example.com" {
		t.Errorf("stored email = %q, want normalized lowercase", sess.User.Email)
	}
	if sess.User.DisplayName != "Ada" {
		t.Errorf("display name = %q, want trimmed Ada", sess.User.DisplayName)
	}
	if sess.User.Role != user.RoleCustomer {
		t.Errorf("role = %q, want customer", sess.User.Role)
	}

	// The stored hash must be argon2id PHC, never the plaintext password.
	stored, err := users.GetByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if !strings.HasPrefix(stored.PasswordHash, "$argon2id$") {
		t.Errorf("stored hash %q is not an argon2id PHC string", stored.PasswordHash)
	}
	if strings.Contains(stored.PasswordHash, "averylongpassword") {
		t.Errorf("stored hash contains the plaintext password")
	}
}

func TestServiceRegisterValidation(t *testing.T) {
	svc, _, _ := newTestService(t, memory.NewManualClock(time.Now()))
	ctx := context.Background()

	cases := map[string]struct{ email, password string }{
		"bad email":         {"not-an-email", "averylongpassword"},
		"short password":    {"a@b.co", "short"},
		"password is email": {"a@b.co", "a@b.co"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Register(ctx, tc.email, tc.password, "")
			if err == nil {
				t.Errorf("Register(%q,%q) = nil error, want validation failure", tc.email, tc.password)
			}
		})
	}
}

func TestServiceRegisterDuplicateEmail(t *testing.T) {
	svc, _, _ := newTestService(t, memory.NewManualClock(time.Now()))
	ctx := context.Background()

	if _, err := svc.Register(ctx, "dup@example.com", "averylongpassword", ""); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// Case variant must still collide (case-insensitive uniqueness).
	_, err := svc.Register(ctx, "DUP@example.com", "anotherlongpassword", "")
	if !errors.Is(err, user.ErrEmailTaken) {
		t.Errorf("second Register = %v, want ErrEmailTaken", err)
	}
}

func TestServiceLogin(t *testing.T) {
	svc, _, _ := newTestService(t, memory.NewManualClock(time.Now()))
	ctx := context.Background()

	if _, err := svc.Register(ctx, "bob@example.com", "averylongpassword", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}

	sess, err := svc.Login(ctx, "BOB@example.com", "averylongpassword")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sess.AccessToken == "" || sess.RefreshToken == "" {
		t.Errorf("Login returned empty tokens")
	}
	if sess.User.Email != "bob@example.com" {
		t.Errorf("Login user email = %q, want bob@example.com", sess.User.Email)
	}
}

// TestServiceLoginFailureIndistinguishable: wrong password and unknown
// email must return the SAME sentinel so responses cannot enumerate
// accounts (CLAUDE.md §3).
func TestServiceLoginFailureIndistinguishable(t *testing.T) {
	svc, _, _ := newTestService(t, memory.NewManualClock(time.Now()))
	ctx := context.Background()

	if _, err := svc.Register(ctx, "carol@example.com", "averylongpassword", ""); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, wrongPassword := svc.Login(ctx, "carol@example.com", "wrong-password-here")
	_, unknownEmail := svc.Login(ctx, "nobody@example.com", "averylongpassword")

	if !errors.Is(wrongPassword, user.ErrInvalidCredentials) {
		t.Errorf("wrong password = %v, want ErrInvalidCredentials", wrongPassword)
	}
	if !errors.Is(unknownEmail, user.ErrInvalidCredentials) {
		t.Errorf("unknown email = %v, want ErrInvalidCredentials", unknownEmail)
	}
}

// TestServiceRefreshRotates covers the happy path plus the FULL theft
// chain: refreshing rotates the token; replaying the OLD token revokes the
// whole family, which kills the NEW token too.
func TestServiceRefreshRotates(t *testing.T) {
	svc, _, _ := newTestService(t, memory.NewManualClock(time.Now()))
	ctx := context.Background()

	first, err := svc.Register(ctx, "dave@example.com", "averylongpassword", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	second, err := svc.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Errorf("Refresh did not rotate the token")
	}
	if second.AccessToken == "" {
		t.Errorf("Refresh returned empty access token")
	}
	if second.User.ID != first.User.ID {
		t.Errorf("Refresh changed user: %q -> %q", first.User.ID, second.User.ID)
	}

	// Replay the OLD (now consumed) token: theft.
	_, err = svc.Refresh(ctx, first.RefreshToken)
	if !errors.Is(err, user.ErrRefreshTokenReused) {
		t.Fatalf("replay of old token = %v, want ErrRefreshTokenReused", err)
	}

	// The theft must have revoked the family, killing the NEW token too.
	// It now presents as a revoked token → reuse.
	if _, err := svc.Refresh(ctx, second.RefreshToken); !errors.Is(err, user.ErrRefreshTokenReused) {
		t.Errorf("new token after theft = %v, want ErrRefreshTokenReused (family revoked)", err)
	}
}

// TestServiceRefreshExpired: an expired token is normal lifecycle — it
// fails WITHOUT revoking the family, so a fresh login still works.
func TestServiceRefreshExpired(t *testing.T) {
	start := time.Now()
	clock := memory.NewManualClock(start)
	svc, _, _ := newTestService(t, clock)
	ctx := context.Background()

	sess, err := svc.Register(ctx, "erin@example.com", "averylongpassword", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	clock.Advance(721 * time.Hour) // past the 720h refresh TTL

	_, err = svc.Refresh(ctx, sess.RefreshToken)
	if !errors.Is(err, user.ErrRefreshTokenExpired) {
		t.Fatalf("Refresh(expired) = %v, want ErrRefreshTokenExpired", err)
	}

	// Rewind so the fresh session below isn't born expired.
	clock.Set(start)

	// Family must NOT be revoked: a new login opens a working session.
	fresh, err := svc.Login(ctx, "erin@example.com", "averylongpassword")
	if err != nil {
		t.Fatalf("Login after expiry: %v", err)
	}
	if _, err := svc.Refresh(ctx, fresh.RefreshToken); err != nil {
		t.Errorf("Refresh after fresh login = %v, want success (family not revoked)", err)
	}
}

func TestServiceRefreshUnknownToken(t *testing.T) {
	svc, _, _ := newTestService(t, memory.NewManualClock(time.Now()))
	ctx := context.Background()

	for _, tok := range []string{"", "no-such-token"} {
		if _, err := svc.Refresh(ctx, tok); !errors.Is(err, user.ErrRefreshTokenUnknown) {
			t.Errorf("Refresh(%q) = %v, want ErrRefreshTokenUnknown", tok, err)
		}
	}
}

// TestServiceLogout: logout revokes the family; it is idempotent for
// unknown/already-dead tokens.
func TestServiceLogout(t *testing.T) {
	svc, _, _ := newTestService(t, memory.NewManualClock(time.Now()))
	ctx := context.Background()

	sess, err := svc.Register(ctx, "frank@example.com", "averylongpassword", "")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := svc.Logout(ctx, sess.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	// The revoked token can no longer refresh.
	if _, err := svc.Refresh(ctx, sess.RefreshToken); err == nil {
		t.Errorf("Refresh after logout = nil, want failure")
	}

	// Idempotent: logging out again (now unknown/revoked) must not error.
	if err := svc.Logout(ctx, sess.RefreshToken); err != nil {
		t.Errorf("second Logout = %v, want nil (idempotent)", err)
	}
	if err := svc.Logout(ctx, "never-existed"); err != nil {
		t.Errorf("Logout(unknown) = %v, want nil (idempotent)", err)
	}
	if err := svc.Logout(ctx, ""); err != nil {
		t.Errorf("Logout(empty) = %v, want nil", err)
	}
}

func TestServiceMe(t *testing.T) {
	svc, _, _ := newTestService(t, memory.NewManualClock(time.Now()))
	ctx := context.Background()

	sess, err := svc.Register(ctx, "grace@example.com", "averylongpassword", "Grace")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := svc.Me(ctx, sess.User.ID)
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if got.Email != "grace@example.com" || got.DisplayName != "Grace" {
		t.Errorf("Me = %+v, want grace@example.com / Grace", got)
	}

	if _, err := svc.Me(ctx, "no-such-user"); !errors.Is(err, user.ErrUserNotFound) {
		t.Errorf("Me(unknown) = %v, want ErrUserNotFound", err)
	}
}

// TestServiceConcurrentRegisterSameEmail: the port contract under -race —
// many goroutines race to register the same email, exactly one wins.
func TestServiceConcurrentRegisterSameEmail(t *testing.T) {
	svc, _, _ := newTestService(t, memory.NewManualClock(time.Now()))
	ctx := context.Background()

	const n = 32
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
			_, err := svc.Register(ctx, "race@example.com", "averylongpassword", "")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, user.ErrEmailTaken):
				taken++
			default:
				t.Errorf("Register returned unexpected error %v", err)
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
