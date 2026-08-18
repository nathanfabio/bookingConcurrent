package booking

import (
	"errors"
	"testing"
	"time"
)

func TestStatusValid(t *testing.T) {
	valid := []Status{StatusConfirmed, StatusCancelled}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("status %q should be valid", s)
		}
	}
	invalid := []Status{"", "pending", "CONFIRMED", "confirmed "}
	for _, s := range invalid {
		if s.Valid() {
			t.Errorf("status %q should be invalid", s)
		}
	}
}

func TestNewSeatValid(t *testing.T) {
	cases := []struct {
		row    string
		number int
	}{
		{"A", 1},
		{"a", 7},    // lowercased
		{" A ", 7},  // trimmed
		{"AA", 12},  // multi-char row
		{"ABC", 99}, // 3-char row, the max
	}
	for _, tc := range cases {
		seat, err := NewSeat(tc.row, tc.number)
		if err != nil {
			t.Errorf("NewSeat(%q, %d) unexpected error: %v", tc.row, tc.number, err)
			continue
		}
		if seat.Number != tc.number {
			t.Errorf("NewSeat(%q, %d).Number = %d", tc.row, tc.number, seat.Number)
		}
	}
}

func TestNewSeatInvalid(t *testing.T) {
	cases := []struct {
		row    string
		number int
	}{
		{"", 1},     // empty row
		{"ABCD", 1}, // row too long
		{"A1", 1},   // digit in row
		{"-", 1},    // punctuation
		{"A", 0},    // zero seat number
		{"A", -3},   // negative seat number
	}
	for _, tc := range cases {
		if _, err := NewSeat(tc.row, tc.number); err == nil {
			t.Errorf("NewSeat(%q, %d) should fail", tc.row, tc.number)
		}
	}
}

func TestHoldExpiryAndConfirmability(t *testing.T) {
	expires := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	h := Hold{ExpiresAt: expires}

	if h.IsExpired(expires.Add(-time.Second)) {
		t.Error("hold should be live one second before expiry")
	}
	if h.CanBeConfirmed(expires.Add(-time.Second)) != true {
		t.Error("live hold must be confirmable")
	}

	// Expiry is inclusive: at exactly ExpiresAt the hold is gone. The Redis
	// TTL behaves the same way (a key at TTL 0 is dead), so the pure rule
	// must agree or the fake and the real store would diverge.
	if !h.IsExpired(expires) {
		t.Error("hold should be expired exactly at ExpiresAt")
	}
	if h.CanBeConfirmed(expires) {
		t.Error("expired hold must not be confirmable")
	}
	if !h.IsExpired(expires.Add(time.Hour)) {
		t.Error("hold should be expired after ExpiresAt")
	}
}

func TestBookingCanBeCancelled(t *testing.T) {
	confirmed := Booking{Status: StatusConfirmed}
	if !confirmed.CanBeCancelled() {
		t.Error("confirmed booking must be cancellable")
	}
	cancelled := Booking{Status: StatusCancelled}
	if cancelled.CanBeCancelled() {
		t.Error("cancelled booking must not be cancellable twice")
	}
}

func TestDomainErrorsAreDistinctAndStable(t *testing.T) {
	errs := []error{ErrSeatAlreadyHeld, ErrHoldLimitExceeded, ErrHoldNotFound, ErrNotHoldOwner}
	for i, a := range errs {
		for j, b := range errs {
			if i != j && errors.Is(a, b) {
				t.Errorf("errors %d and %d must be distinct", i, j)
			}
		}
		if a == nil || a.Error() == "" {
			t.Errorf("error %d must have a message", i)
		}
	}
}

func validHold(expires time.Time) Hold {
	return Hold{
		SessionID:   "session-1",
		ScreeningID: "screening-1",
		Seat:        Seat{Row: "A", Number: 1},
		UserID:      "user-1",
		HoldToken:   "token-1",
		ExpiresAt:   expires,
	}
}

func TestHoldValidateAcceptsWellFormed(t *testing.T) {
	now := time.Now()
	if err := validHold(now.Add(time.Minute)).Validate(now); err != nil {
		t.Errorf("well-formed hold rejected: %v", err)
	}
}

func TestHoldValidateRejectsMalformed(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Minute)

	cases := map[string]func(h *Hold){
		"missing session":    func(h *Hold) { h.SessionID = "" },
		"missing screening":  func(h *Hold) { h.ScreeningID = "" },
		"missing user":       func(h *Hold) { h.UserID = "" },
		"missing token":      func(h *Hold) { h.HoldToken = "" },
		"missing seat row":   func(h *Hold) { h.Seat.Row = "" },
		"bad seat number":    func(h *Hold) { h.Seat.Number = 0 },
		"zero expiry":        func(h *Hold) { h.ExpiresAt = time.Time{} },
		"past expiry":        func(h *Hold) { h.ExpiresAt = now.Add(-time.Second) },
		"expiry exactly now": func(h *Hold) { h.ExpiresAt = now },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := validHold(future)
			mutate(&h)
			if err := h.Validate(now); err == nil {
				t.Errorf("Validate accepted hold with %s", name)
			}
		})
	}
}
