//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

func loadCheckout(t *testing.T) checkoutFixture {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("E2E_CHECKOUT")) //nolint:gosec // Runner-owned checkout in a private temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	var f checkoutFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.Booking.ID == "" || f.Spare.Row == "" {
		t.Fatal("incomplete checkout fixture")
	}
	return f
}

func login(t *testing.T, email string) string {
	t.Helper()
	var auth struct {
		Token string `json:"access_token"`
	}
	request(t, "POST", "/auth/login", "", map[string]string{"email": email, "password": "sample-e2e-password"}, 200, &auth)
	if auth.Token == "" {
		t.Fatal("missing access token")
	}
	return auth.Token
}

func holdPayload(screening string, s seat) map[string]any {
	return map[string]any{"screening_id": screening, "row": s.Row, "number": s.Number}
}

func TestRedisUnavailable(t *testing.T) {
	f := loadCheckout(t)
	request(t, "GET", "/healthz", "", nil, 200, nil)
	var readiness struct {
		Status string
		Checks map[string]string
	}
	request(t, "GET", "/readyz", "", nil, 503, &readiness)
	if readiness.Status != "unavailable" || readiness.Checks["postgres"] != "ok" || readiness.Checks["redis"] == "" || readiness.Checks["redis"] == "ok" {
		t.Fatalf("unexpected readiness: %+v", readiness)
	}

	owner := login(t, "owner@example.test")
	other := login(t, "other@example.test")
	path := "/holds/" + f.Booking.Session + "/confirm"
	var replay confirmedBooking
	request(t, "POST", path, owner, nil, 200, &replay)
	if replay != f.Booking {
		t.Fatalf("owner replay = %+v, want %+v", replay, f.Booking)
	}

	var denied struct{ Code string }
	request(t, "POST", path, other, nil, 404, &denied)
	if denied.Code != "not_found" {
		t.Fatalf("foreign replay: %q", denied.Code)
	}
	var unavailable struct{ Code string }
	request(t, "POST", "/holds", owner, holdPayload(f.Booking.Screening, f.Spare), 500, &unavailable)
	if unavailable.Code != "internal_error" {
		t.Fatalf("unavailable hold: %q", unavailable.Code)
	}
}

func TestRedisRecovered(t *testing.T) {
	f := loadCheckout(t)
	owner := login(t, "owner@example.test")
	var grid struct{ Seats []seat }
	request(t, "GET", "/screenings/"+f.Booking.Screening+"/seats", "", nil, 200, &grid)
	sold := seat{Row: f.Booking.Row, Number: f.Booking.Number, Status: "booked"}
	if !slices.Contains(grid.Seats, sold) || !slices.Contains(grid.Seats, f.Spare) {
		t.Fatalf("expected booked seat %+v and available seat %+v after recovery", sold, f.Spare)
	}
	var hold struct {
		Session string `json:"session_id"`
	}
	request(t, "POST", "/holds", owner, holdPayload(f.Booking.Screening, f.Spare), 201, &hold)
	if hold.Session == "" {
		t.Fatal("no session after recovery")
	}
	request(t, "DELETE", "/holds/"+hold.Session, owner, nil, 204, nil)
}
