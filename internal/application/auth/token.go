package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	user "github.com/nathanfabio/bookingConcurrent/internal/domain/user"
)

// ErrTokenInvalid covers every way an access token can fail validation
// (bad signature, expired, wrong issuer, wrong algorithm, malformed).
// Callers deliberately cannot tell WHICH reason failed — the HTTP layer
// maps all of them to one generic 401 (CLAUDE.md §3, ADR 0004).
var ErrTokenInvalid = errors.New("auth: access token invalid or expired")

// Claims is the access-token claim set (ADR 0004). Only registered claims
// plus role: access tokens are short-lived on purpose, so there is no jti
// (per-token revocation is not a feature we implement — the TTL IS the
// revocation story) and no nbf (clock-skew pain for zero gain in a
// single-service system).
type Claims struct {
	jwt.RegisteredClaims
	Role string `json:"role"`
}

// TokenIssuer signs and validates HS256 access JWTs. It is a concrete
// type, not an interface — one implementation, exercised directly in
// tests.
type TokenIssuer struct {
	secret    []byte
	issuer    string
	accessTTL time.Duration
	now       func() time.Time // injectable for expiry tests
}

// NewTokenIssuer builds an issuer. secret is the raw signing key
// (cfg.Auth.JWTSecret), issuer identifies this deployment in the iss
// claim so tokens minted elsewhere are rejected, not just expired.
func NewTokenIssuer(secret, issuer string, accessTTL time.Duration) *TokenIssuer {
	return &TokenIssuer{
		secret:    []byte(secret),
		issuer:    issuer,
		accessTTL: accessTTL,
		now:       time.Now,
	}
}

// IssueAccessToken signs a fresh access token for u, expiring after the
// configured TTL. The user's Role travels in the token so future admin
// endpoints need no token-format migration (ADR 0004); M3 enforces nothing
// with it.
func (i *TokenIssuer) IssueAccessToken(u user.User) (string, error) {
	now := i.now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.issuer,
			Subject:   u.ID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.accessTTL)),
		},
		Role: string(u.Role),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.secret)
}

// ValidateAccessToken checks signature, algorithm, expiry, and issuer,
// and returns the authenticated user ID (the sub claim) — the ONLY
// identity downstream handlers may trust (CLAUDE.md §3).
//
// Every failure path collapses into ErrTokenInvalid so response bodies
// cannot reveal why a token was rejected. The token value itself is never
// logged (scripts/lint-guards.sh).
func (i *TokenIssuer) ValidateAccessToken(accessToken string) (string, error) {
	token, err := jwt.ParseWithClaims(accessToken, &Claims{},
		func(t *jwt.Token) (any, error) {
			// Pin the algorithm: blocks alg=none and algorithm-confusion
			// (e.g. a forged RS256 header) mechanically instead of by
			// review discipline.
			if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				return nil, fmt.Errorf("auth: unexpected signing method %v", t.Method.Alg())
			}
			return i.secret, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(i.issuer),
		jwt.WithTimeFunc(i.now),
	)
	if err != nil || !token.Valid {
		return "", ErrTokenInvalid
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || claims.Subject == "" {
		return "", ErrTokenInvalid
	}
	return claims.Subject, nil
}
