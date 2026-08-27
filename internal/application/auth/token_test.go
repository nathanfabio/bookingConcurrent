package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	user "github.com/nathanfabio/bookingConcurrent/internal/domain/user"
)

func testUser() user.User {
	return user.User{
		ID:    "user-123",
		Email: "alice@example.com",
		Role:  user.RoleCustomer,
	}
}

func TestTokenIssueValidateRoundTrip(t *testing.T) {
	issuer := NewTokenIssuer("test-secret-test-secret-test-sec", "booking-api", 15*time.Minute)

	token, err := issuer.IssueAccessToken(testUser())
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	userID, err := issuer.ValidateAccessToken(token)
	if err != nil {
		t.Fatalf("ValidateAccessToken: %v", err)
	}
	if userID != "user-123" {
		t.Errorf("ValidateAccessToken = %q, want user-123", userID)
	}
}

func TestTokenClaimsCarryRoleAndIssuer(t *testing.T) {
	issuer := NewTokenIssuer("test-secret-test-secret-test-sec", "booking-api", 15*time.Minute)
	token, err := issuer.IssueAccessToken(testUser())
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	parsed, _, err := jwt.NewParser().ParseUnverified(token, &Claims{})
	if err != nil {
		t.Fatalf("ParseUnverified: %v", err)
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok {
		t.Fatalf("claims are not *Claims")
	}
	if claims.Role != "customer" {
		t.Errorf("role claim = %q, want customer", claims.Role)
	}
	if claims.Issuer != "booking-api" {
		t.Errorf("iss claim = %q, want booking-api", claims.Issuer)
	}
	if claims.Subject != "user-123" {
		t.Errorf("sub claim = %q, want user-123", claims.Subject)
	}
}

func TestTokenValidationRejects(t *testing.T) {
	const secret = "test-secret-test-secret-test-sec"
	issuer := NewTokenIssuer(secret, "booking-api", 15*time.Minute)
	valid, err := issuer.IssueAccessToken(testUser())
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	t.Run("tampered signature", func(t *testing.T) {
		// Flip a character in the signature segment.
		parts := strings.Split(valid, ".")
		sig := []byte(parts[2])
		if sig[0] == 'A' {
			sig[0] = 'B'
		} else {
			sig[0] = 'A'
		}
		parts[2] = string(sig)
		tampered := strings.Join(parts, ".")
		if _, err := issuer.ValidateAccessToken(tampered); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("ValidateAccessToken(tampered) = %v, want ErrTokenInvalid", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		expired := NewTokenIssuer(secret, "booking-api", 15*time.Minute)
		now := time.Now()
		expired.now = func() time.Time { return now.Add(-time.Hour) }
		token, err := expired.IssueAccessToken(testUser())
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		if _, err := issuer.ValidateAccessToken(token); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("ValidateAccessToken(expired) = %v, want ErrTokenInvalid", err)
		}
	})

	t.Run("wrong issuer", func(t *testing.T) {
		other := NewTokenIssuer(secret, "another-service", 15*time.Minute)
		token, err := other.IssueAccessToken(testUser())
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		if _, err := issuer.ValidateAccessToken(token); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("ValidateAccessToken(wrong issuer) = %v, want ErrTokenInvalid", err)
		}
	})

	t.Run("wrong algorithm", func(t *testing.T) {
		// Forge a token signed with HS512: ValidateAccessToken must refuse
		// any algorithm other than HS256 (algorithm-confusion defense).
		claims := Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:    "booking-api",
				Subject:   "user-123",
				IssuedAt:  jwt.NewNumericDate(time.Now()),
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
			},
			Role: "customer",
		}
		forged, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString([]byte(secret))
		if err != nil {
			t.Fatalf("forge HS512 token: %v", err)
		}
		if _, err := issuer.ValidateAccessToken(forged); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("ValidateAccessToken(HS512) = %v, want ErrTokenInvalid", err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		if _, err := issuer.ValidateAccessToken("not.a.jwt"); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("ValidateAccessToken(malformed) = %v, want ErrTokenInvalid", err)
		}
	})
}
