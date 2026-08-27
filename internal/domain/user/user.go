// Package user holds the identity domain model: users and their refresh
// tokens (CLAUDE.md §3).
//
// Everything here is pure Go: no I/O, no infrastructure imports, no
// goroutines. State-dependent rules — token expiry, replacement, theft —
// live here as methods; anything that needs a store lives in the
// application layer (internal/application/auth).
package user

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Role is a user's authorization level. The schema (migrations/00001)
// constrains it to these two values; M3 enforces nothing on Role beyond
// storing it — it travels in the access token so future admin endpoints
// don't need a token-format migration (ADR 0004).
type Role string

const (
	RoleCustomer Role = "customer"
	RoleAdmin    Role = "admin"
)

// Domain errors surfaced by the auth stores and use cases. Mapped to HTTP
// statuses centrally in the HTTP adapter (CLAUDE.md §5); identity
// (errors.Is) is the contract, messages are for humans.
var (
	// ErrValidation wraps every input-shape failure from the validators
	// below (and the cross-field checks in the use case). The HTTP layer
	// maps it to 400 without needing to know which rule fired.
	ErrValidation = errors.New("validation failed")
	// ErrUserNotFound: no user matches the lookup (email or ID).
	ErrUserNotFound = errors.New("user: user not found")
	// ErrEmailTaken: registration lost a race to the unique lower(email)
	// index (migrations/00001).
	ErrEmailTaken = errors.New("user: email already registered")
	// ErrInvalidCredentials: login failed. Deliberately ONE sentinel for
	// both unknown email and wrong password so the response cannot leak
	// which of the two happened (CLAUDE.md §3's no-enumeration rule).
	ErrInvalidCredentials = errors.New("user: invalid credentials")
	// ErrRefreshTokenUnknown: no refresh-token row matches the presented
	// token's hash.
	ErrRefreshTokenUnknown = errors.New("user: refresh token not recognized")
	// ErrRefreshTokenExpired: the token row exists but its TTL has passed.
	// Expiry is normal lifecycle — unlike reuse, it does NOT revoke the
	// family (ADR 0004).
	ErrRefreshTokenExpired = errors.New("user: refresh token expired")
	// ErrRefreshTokenReused: a token that was ALREADY replaced or revoked
	// was presented again — token theft. By the time a caller observes this
	// error the ENTIRE token family has been revoked by the store
	// (ADR 0004).
	ErrRefreshTokenReused = errors.New("user: refresh token reused; token family revoked")
)

// User is a registered account (migrations/00001). PasswordHash holds an
// argon2id PHC string (ADR 0005) — never a plaintext password, and the
// value must never reach a log line (scripts/lint-guards.sh enforces).
type User struct {
	ID           string
	Email        string
	PasswordHash string
	DisplayName  string
	Role         Role
	CreatedAt    time.Time
}

// Email limits follow the RFC 5321 envelope caps rather than any attempt
// at full RFC 5322 parsing — shape-checking is deliberately all we do
// (see ValidateEmail).
const (
	emailMaxTotal = 254
	emailMaxLocal = 64

	// Password policy is length-only on purpose: NIST SP 800-63B found
	// composition rules (digits! symbols!) degrade real-world security by
	// pushing users toward predictable substitutions. The 128-character cap
	// doubles as a DoS bound on argon2id input.
	passwordMinLen = 10
	passwordMaxLen = 128

	displayNameMaxRunes = 100
)

// ValidateEmail normalizes and shape-checks an email address, returning the
// canonical form (trimmed, lowercased) used for lookups and storage.
//
// This is intentionally a SHAPE check, not an RFC 5322 parser and not
// ownership verification: one '@', a local part of 1-64 chars, and a
// domain containing at least one dot. The only durable fix for "does this
// mailbox exist" is sending mail to it, which this system doesn't do.
func ValidateEmail(email string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return "", fmt.Errorf("user: email is required: %w", ErrValidation)
	}
	if len(e) > emailMaxTotal {
		return "", fmt.Errorf("user: email exceeds %d characters: %w", emailMaxTotal, ErrValidation)
	}
	at := strings.IndexByte(e, '@')
	if at <= 0 || at != strings.LastIndexByte(e, '@') {
		return "", fmt.Errorf("user: email must contain exactly one '@': %w", ErrValidation)
	}
	local, domain := e[:at], e[at+1:]
	if len(local) > emailMaxLocal {
		return "", fmt.Errorf("user: email local part exceeds %d characters: %w", emailMaxLocal, ErrValidation)
	}
	if domain == "" || !strings.Contains(domain, ".") {
		return "", fmt.Errorf("user: email domain is malformed: %w", ErrValidation)
	}
	if domain[0] == '.' || domain[len(domain)-1] == '.' {
		return "", fmt.Errorf("user: email domain is malformed: %w", ErrValidation)
	}
	return e, nil
}

// ValidatePassword enforces the length-only policy (NIST SP 800-63B).
// Callers that also know the email should additionally reject
// password == email; that cross-field rule needs both values, so it lives
// in the use case, not here.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < passwordMinLen {
		return fmt.Errorf("user: password must be at least %d characters: %w", passwordMinLen, ErrValidation)
	}
	if len(password) > passwordMaxLen {
		return fmt.Errorf("user: password exceeds %d characters: %w", passwordMaxLen, ErrValidation)
	}
	return nil
}

// ValidateDisplayName trims and bounds a display name. Empty is allowed —
// a user is not required to tell us what to call them.
func ValidateDisplayName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if utf8.RuneCountInString(n) > displayNameMaxRunes {
		return "", fmt.Errorf("user: display name exceeds %d characters: %w", displayNameMaxRunes, ErrValidation)
	}
	return n, nil
}
