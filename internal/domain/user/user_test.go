package user

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateEmail(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"alice@example.com", "alice@example.com"},
		{"  Alice@Example.COM ", "alice@example.com"}, // trim + lowercase
		{"a@b.co", "a@b.co"},
		{"first.last@sub.domain.org", "first.last@sub.domain.org"},
	}
	for _, tc := range valid {
		got, err := ValidateEmail(tc.in)
		if err != nil {
			t.Errorf("ValidateEmail(%q) = unexpected error %v, want %q", tc.in, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("ValidateEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	invalid := map[string]string{
		"empty":            "",
		"whitespace only":  "   ",
		"no at sign":       "alice.example.com",
		"two at signs":     "al@ice@example.com",
		"leading at":       "@example.com",
		"empty domain":     "alice@",
		"domain no dot":    "alice@localhost",
		"leading dot":      "alice@.example.com",
		"trailing dot":     "alice@example.com.",
		"too long overall": strings.Repeat("a", 250) + "@b.co",
		"local too long":   strings.Repeat("a", 65) + "@b.co",
	}
	for name, in := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateEmail(in); err == nil {
				t.Errorf("ValidateEmail(%q) = no error, want validation failure", in)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name    string
		pw      string
		wantErr bool
	}{
		{"9 chars rejected", "123456789", true},
		{"10 chars accepted", "1234567890", false},
		{"long accepted", strings.Repeat("x", 128), false},
		{"129 rejected", strings.Repeat("x", 129), true},
		{"empty rejected", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.pw)
			if tc.wantErr && err == nil {
				t.Errorf("ValidatePassword = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidatePassword = %v, want nil", err)
			}
		})
	}
}

func TestValidateDisplayName(t *testing.T) {
	got, err := ValidateDisplayName("  Ada Lovelace  ")
	if err != nil {
		t.Fatalf("ValidateDisplayName = unexpected error %v", err)
	}
	if got != "Ada Lovelace" {
		t.Errorf("ValidateDisplayName = %q, want trimmed %q", got, "Ada Lovelace")
	}

	if _, err := ValidateDisplayName(""); err != nil {
		t.Errorf("ValidateDisplayName(\"\") = %v, want nil (empty allowed)", err)
	}

	if _, err := ValidateDisplayName(strings.Repeat("é", 101)); err == nil {
		t.Errorf("ValidateDisplayName(101 runes) = nil, want error")
	}
}

func TestDomainErrorsAreDistinctAndStable(t *testing.T) {
	errs := []error{
		ErrValidation,
		ErrUserNotFound,
		ErrEmailTaken,
		ErrInvalidCredentials,
		ErrRefreshTokenUnknown,
		ErrRefreshTokenExpired,
		ErrRefreshTokenReused,
	}
	for i := range errs {
		if errs[i].Error() == "" {
			t.Errorf("sentinel %d has empty message", i)
		}
		for j := range errs {
			if i == j {
				continue
			}
			if errors.Is(errs[i], errs[j]) {
				t.Errorf("sentinels %d and %d are not distinct", i, j)
			}
		}
	}
}
