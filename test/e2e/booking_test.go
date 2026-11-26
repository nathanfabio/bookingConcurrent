//go:build e2e

package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type seat struct {
	Row    string
	Number int
	Status string
}

var journeyClient = &http.Client{Timeout: 8 * time.Second}

type confirmedBooking struct {
	ID, Row, Status string
	Number          int
	Session         string `json:"session_id"`
	Screening       string `json:"screening_id"`
}

type checkoutFixture struct {
	Booking confirmedBooking
	Spare   seat
}

func request(t *testing.T, method, path, token string, body any, want int, result any) {
	t.Helper()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+os.Getenv("HTTP_ADDR")+path, bytes.NewReader(data)) //nolint:gosec // Target is the loopback API launched by the test runner.
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if path == "/webhooks/payment" {
		stamp := time.Now().Unix()
		mac := hmac.New(sha256.New, []byte(os.Getenv("PAYMENT_FAKE_WEBHOOK_SECRET")))
		_, _ = fmt.Fprintf(mac, "%d.%s", stamp, data)
		req.Header.Set("Payment-Signature", fmt.Sprintf("t=%d,v1=%x", stamp, mac.Sum(nil)))
	}
	resp, err := journeyClient.Do(req) //nolint:gosec // All requests exercise the runner-owned API.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		t.Fatalf("%s %s returned %d, want %d", method, path, resp.StatusCode, want)
	}
	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			t.Fatalf("%s response: %v", path, err)
		}
	}
}

func TestBookingJourney(t *testing.T) {
	account := func(email string) map[string]string {
		return map[string]string{"email": email, "password": "sample-e2e-password"}
	}
	var owner, other struct {
		Token string `json:"access_token"`
	}
	credentials := account("owner@example.test")
	request(t, "POST", "/auth/register", "", credentials, 201, nil)
	request(t, "POST", "/auth/login", "", credentials, 200, &owner)
	request(t, "POST", "/auth/register", "", account("other@example.test"), 201, &other)
	if owner.Token == "" || other.Token == "" || owner.Token == other.Token {
		t.Fatal("expected two distinct access tokens")
	}

	var catalog struct{ Movies []struct{ ID string } }
	request(t, "GET", "/movies", "", nil, 200, &catalog)
	if len(catalog.Movies) == 0 {
		t.Fatal("empty catalog")
	}
	var shows struct{ Screenings []struct{ ID string } }
	request(t, "GET", "/movies/"+catalog.Movies[0].ID+"/screenings", "", nil, 200, &shows)
	if len(shows.Screenings) == 0 {
		t.Fatal("empty screening list")
	}
	screening := shows.Screenings[0].ID
	seats := func() []seat {
		var grid struct{ Seats []seat }
		request(t, "GET", "/screenings/"+screening+"/seats", "", nil, 200, &grid)
		return grid.Seats
	}
	var selected, spare seat
	for _, s := range seats() {
		if s.Status == "available" {
			if selected.Row == "" {
				selected = s
				continue
			}
			spare = s
			break
		}
	}
	if spare.Row == "" {
		t.Fatal("need two available seats")
	}
	expectSeat := func(status string) {
		t.Helper()
		for _, s := range seats() {
			if s.Row == selected.Row && s.Number == selected.Number && s.Status == status {
				return
			}
		}
		t.Fatalf("expected %s%d to be %s", selected.Row, selected.Number, status)
	}
	var hold struct {
		Session string `json:"session_id"`
	}
	request(t, "POST", "/holds", owner.Token, map[string]any{"screening_id": screening, "row": selected.Row, "number": selected.Number}, 201, &hold)
	if hold.Session == "" {
		t.Fatal("missing session_id")
	}
	path := "/holds/" + hold.Session
	expectSeat("held")
	var rejection struct{ Code string }
	request(t, "DELETE", path, other.Token, nil, 404, &rejection)
	if rejection.Code != "not_found" {
		t.Fatalf("foreign release: %q", rejection.Code)
	}
	request(t, "POST", path+"/confirm", owner.Token, nil, 402, &rejection)
	if rejection.Code != "payment_required" {
		t.Fatalf("unpaid confirm: %q", rejection.Code)
	}

	var intent struct {
		ID     string `json:"intent_id"`
		Amount int    `json:"amount_cents"`
	}
	request(t, "POST", path+"/payment-intent", owner.Token, nil, 201, &intent)
	request(t, "POST", "/webhooks/payment", "", map[string]any{"kind": "payment.captured", "intent_id": intent.ID, "amount_cents": intent.Amount}, 200, nil)
	var booking, replay confirmedBooking
	request(t, "POST", path+"/confirm", owner.Token, nil, 201, &booking)
	if booking.ID == "" || booking.Session != hold.Session || booking.Screening != screening || booking.Row != selected.Row || booking.Number != selected.Number || booking.Status != "confirmed" {
		t.Fatalf("unexpected booking: %+v", booking)
	}
	expectSeat("booked")
	// Tell the runner which sale the API exposed while RabbitMQ was offline.
	fixture, err := json.Marshal(checkoutFixture{Booking: booking, Spare: spare})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("E2E_CHECKOUT"), fixture, 0600); err != nil { //nolint:gosec // Runner-owned temporary fixture.
		t.Fatal(err)
	}

	// The runner restarts the worker and restores RabbitMQ while we wait.
	// Replay with the original owner only after this booking is delivered.
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); {
		log, err := os.ReadFile(os.Getenv("E2E_WORKER_LOG")) //nolint:gosec // Runner supplies its own temporary log path.
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(log), "\n") {
			if strings.Contains(line, "booking confirmation notification (stub)") && strings.Contains(line, booking.ID) {
				request(t, "POST", path+"/confirm", owner.Token, nil, 200, &replay)
				if replay != booking {
					t.Fatalf("replay after recovery = %+v, want %+v", replay, booking)
				}
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("worker did not notify for booking %s", booking.ID)
}
