package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	user "github.com/nathanfabio/bookingConcurrent/internal/domain/user"
)

// Session is the outcome of a successful register, login, or refresh:
// everything the HTTP layer needs to answer the client. RefreshToken is
// the OPAQUE plaintext credential — it never leaves this struct except
// into the response (JSON sets it, the transport puts it in an httpOnly
// cookie; ADR 0007). Only its SHA-256 hash is persisted (ADR 0004).
type Session struct {
	AccessToken  string
	ExpiresIn    time.Duration // access-token TTL, surfaced as expires_in
	RefreshToken string
	User         user.User
}

// Service orchestrates the auth use cases (CLAUDE.md §3). It depends only
// on ports — UserStore and RefreshTokenStore — so unit tests run against
// the in-memory fakes and production runs against Postgres.
type Service struct {
	users      UserStore
	tokens     RefreshTokenStore
	hasher     *Argon2Hasher
	issuer     *TokenIssuer
	accessTTL  time.Duration
	refreshTTL time.Duration

	// dummyHash is a pre-computed argon2id hash of a random password.
	// Login against an UNKNOWN email still runs one Verify against it so
	// both failure paths burn the same CPU time: response latency must not
	// enumerate which emails are registered (ADR 0005).
	dummyHash string
}

// NewService wires the use case. It panics if the dummy hash cannot be
// computed at startup: like middleware.newUUID, an entropy-source failure
// means the process cannot serve auth traffic honestly, and failing at
// boot beats failing per-request.
func NewService(users UserStore, tokens RefreshTokenStore, hasher *Argon2Hasher, issuer *TokenIssuer, accessTTL, refreshTTL time.Duration) *Service {
	dummy := make([]byte, 32)
	if _, err := rand.Read(dummy); err != nil {
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	dummyHash, err := hasher.Hash(base64.RawURLEncoding.EncodeToString(dummy))
	if err != nil {
		panic("auth: dummy hash: " + err.Error())
	}
	return &Service{
		users:      users,
		tokens:     tokens,
		hasher:     hasher,
		issuer:     issuer,
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		dummyHash:  dummyHash,
	}
}

// Register creates an account and immediately opens a session for it
// (CLAUDE.md §3). Every new registration starts a FRESH token family
// (ADR 0004); role is forced to customer — no request may mint an admin.
func (s *Service) Register(ctx context.Context, email, password, displayName string) (Session, error) {
	normalizedEmail, err := user.ValidateEmail(email)
	if err != nil {
		return Session{}, err
	}
	if err := user.ValidatePassword(password); err != nil {
		return Session{}, err
	}
	if password == normalizedEmail {
		// Cross-field rule: NIST SP 800-63B — the password must not be the
		// identifier. Checked here rather than in the domain validator
		// because it needs both values.
		return Session{}, fmt.Errorf("user: password must not equal the email: %w", user.ErrValidation)
	}
	name, err := user.ValidateDisplayName(displayName)
	if err != nil {
		return Session{}, err
	}

	hash, err := s.hasher.Hash(password)
	if err != nil {
		return Session{}, fmt.Errorf("auth: register: %w", err)
	}

	created, err := s.users.Create(ctx, user.User{
		Email:        normalizedEmail,
		PasswordHash: hash,
		DisplayName:  name,
		Role:         user.RoleCustomer,
	})
	if err != nil {
		return Session{}, err // ErrEmailTaken or infra
	}
	return s.newSession(ctx, created)
}

// Login verifies credentials and opens a session. Unknown email and wrong
// password are indistinguishable by design: both run one argon2id verify
// and return the SAME sentinel (CLAUDE.md §3).
func (s *Service) Login(ctx context.Context, email, password string) (Session, error) {
	normalizedEmail, err := user.ValidateEmail(email)
	if err != nil {
		// A malformed email is still "invalid credentials": returning a
		// validation error here would let a prober distinguish bad input
		// from a real account check.
		s.burnDummyVerify(password)
		return Session{}, user.ErrInvalidCredentials
	}

	u, err := s.users.GetByEmail(ctx, normalizedEmail)
	if err != nil {
		if errors.Is(err, user.ErrUserNotFound) {
			s.burnDummyVerify(password)
			return Session{}, user.ErrInvalidCredentials
		}
		return Session{}, err // infra
	}

	ok, err := s.hasher.Verify(u.PasswordHash, password)
	if err != nil || !ok {
		return Session{}, user.ErrInvalidCredentials
	}
	return s.newSession(ctx, u)
}

// Refresh consumes the presented refresh token and mints its successor in
// the SAME family (rotation, ADR 0004). Replaying an already-consumed or
// revoked token revokes the entire family before returning the error —
// that is theft detection, and the caller cannot opt out of it.
func (s *Service) Refresh(ctx context.Context, presentedToken string) (Session, error) {
	if presentedToken == "" {
		return Session{}, user.ErrRefreshTokenUnknown
	}

	newToken, newHash, err := newOpaqueToken()
	if err != nil {
		return Session{}, fmt.Errorf("auth: refresh: %w", err)
	}
	next := user.RefreshToken{
		ID:        uuid.NewString(),
		TokenHash: newHash,
		ExpiresAt: time.Now().Add(s.refreshTTL),
		// UserID and FamilyID deliberately left empty: Rotate stamps them
		// from the row it locks, so this service never reads token state
		// outside the store's critical section.
	}

	rotated, err := s.tokens.Rotate(ctx, hashToken(presentedToken), next)
	if err != nil {
		return Session{}, err // Unknown / Expired / Reused (family revoked)
	}

	u, err := s.users.GetByID(ctx, rotated.UserID)
	if err != nil {
		// refresh_tokens.user_id is an FK with ON DELETE CASCADE, so a
		// live token row guarantees a live user; only infra errors land
		// here.
		return Session{}, err
	}

	access, err := s.issuer.IssueAccessToken(u)
	if err != nil {
		return Session{}, fmt.Errorf("auth: refresh: %w", err)
	}
	return Session{
		AccessToken:  access,
		ExpiresIn:    s.accessTTL,
		RefreshToken: newToken,
		User:         u,
	}, nil
}

// Logout revokes the whole token family of the presented refresh token,
// closing every session in the chain (the legitimate one AND any stolen
// copies). Idempotent by design: an unknown or already-dead token still
// logs out successfully — the client's goal state is "no session", and we
// are already there.
func (s *Service) Logout(ctx context.Context, presentedToken string) error {
	if presentedToken == "" {
		return nil
	}
	row, err := s.tokens.GetByHash(ctx, hashToken(presentedToken))
	if err != nil {
		if errors.Is(err, user.ErrRefreshTokenUnknown) {
			return nil // nothing to revoke
		}
		return err // infra
	}
	return s.tokens.RevokeFamily(ctx, row.FamilyID)
}

// Me returns the authenticated user. The userID comes from the verified
// access token (via middleware → context), NEVER from a request body
// (CLAUDE.md §3).
func (s *Service) Me(ctx context.Context, userID string) (user.User, error) {
	return s.users.GetByID(ctx, userID)
}

// newSession mints the access token and the first refresh token of a
// fresh family (register/login path).
func (s *Service) newSession(ctx context.Context, u user.User) (Session, error) {
	access, err := s.issuer.IssueAccessToken(u)
	if err != nil {
		return Session{}, fmt.Errorf("auth: issue access token: %w", err)
	}
	token, hash, err := newOpaqueToken()
	if err != nil {
		return Session{}, fmt.Errorf("auth: mint refresh token: %w", err)
	}
	now := time.Now()
	row := user.RefreshToken{
		ID:        uuid.NewString(),
		UserID:    u.ID,
		FamilyID:  uuid.NewString(),
		TokenHash: hash,
		ExpiresAt: now.Add(s.refreshTTL),
	}
	if _, err := s.tokens.Create(ctx, row); err != nil {
		return Session{}, fmt.Errorf("auth: store refresh token: %w", err)
	}
	return Session{
		AccessToken:  access,
		ExpiresIn:    s.accessTTL,
		RefreshToken: token,
		User:         u,
	}, nil
}

// burnDummyVerify spends one argon2id verification so the unknown-email
// path takes the same time as the wrong-password path (timing
// equalization, ADR 0005). The result is ignored on purpose.
func (s *Service) burnDummyVerify(password string) {
	_, _ = s.hasher.Verify(s.dummyHash, password)
}

// newOpaqueToken returns a fresh refresh credential: 32 bytes of
// crypto/rand, base64url-encoded (256 bits of entropy — brute-forcing is
// not the threat model; guessing is), plus the SHA-256 hex digest that is
// the ONLY form persisted (migrations/00006).
func newOpaqueToken() (token, tokenHash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

// hashToken is the one-way step between the credential the client holds
// and the row the server stores: SHA-256 hex. A database read therefore
// leaks nothing usable.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
