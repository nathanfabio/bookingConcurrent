//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

const buyers = 32

type attempt struct {
	status             int
	code, session      string
	latency, firstByte time.Duration
	err                error
}

// Measure through the final response byte. Decode and verify the body outside
// the request timer. firstByte includes network, scheduling and server time.
func attemptHold(ctx context.Context, req *http.Request, screening string, target seat) attempt {
	var a attempt
	start := time.Now()
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { a.firstByte = time.Since(start) }}
	resp, err := journeyClient.Do(req.WithContext(httptrace.WithClientTrace(ctx, trace))) //nolint:gosec // Runner-owned loopback API.
	if err != nil {
		a.err = err
		return a
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	a.latency = time.Since(start)
	if err != nil {
		a.err = err
		return a
	}
	var result struct {
		Code, Row string
		Number    int
		Session   string `json:"session_id"`
		Screening string `json:"screening_id"`
	}
	a.status = resp.StatusCode
	a.err = json.Unmarshal(body, &result)
	a.code, a.session = result.Code, result.Session
	if a.status == 201 && (a.session == "" || result.Screening != screening || result.Row != target.Row || result.Number != target.Number) {
		a.err = fmt.Errorf("invalid hold response for %s%d", target.Row, target.Number)
	}
	return a
}

func TestHTTPHoldContention(t *testing.T) {
	var tokens [buyers]string
	for i := range tokens {
		var auth struct {
			Token string `json:"access_token"`
		}
		request(t, "POST", "/auth/register", "", map[string]string{"email": fmt.Sprintf("buyer%d@example.test", i), "password": "sample-load-password"}, 201, &auth)
		if auth.Token == "" {
			t.Fatal("missing access token")
		}
		tokens[i] = auth.Token
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
	grid := func() []seat {
		var view struct{ Seats []seat }
		request(t, "GET", "/screenings/"+screening+"/seats", "", nil, 200, &view)
		return view.Seats
	}
	free := grid()
	if len(free) < buyers {
		t.Fatal("screening too small")
	}
	type sample struct {
		times              []time.Duration
		elapsed, ttfb      time.Duration
		created, conflicts int
	}
	var totals [2]sample
	for round := 0; round < 4; round++ {
		for step := 0; step < 2; step++ {
			mode := (round + step) % 2 // alternate which scenario gets the warmer pool
			out := &totals[mode]
			results := make([]attempt, buyers)
			var ready, done sync.WaitGroup
			start := make(chan struct{})
			targets := make([]seat, buyers)
			ready.Add(buyers)
			for i := range tokens {
				targets[i] = free[0]
				if mode == 1 {
					targets[i] = free[i]
				}
				if targets[i].Status != "available" {
					t.Fatal("expected a free seat")
				}
				data, err := json.Marshal(holdPayload(screening, targets[i]))
				if err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequestWithContext(t.Context(), "POST", "http://"+os.Getenv("HTTP_ADDR")+"/holds", bytes.NewReader(data)) //nolint:gosec // Runner supplies the loopback address of its isolated API.
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+tokens[i])
				done.Go(func() { ready.Done(); <-start; results[i] = attemptHold(t.Context(), req, screening, targets[i]) })
			}
			ready.Wait()
			begin := time.Now()
			close(start)
			done.Wait()
			out.elapsed += time.Since(begin)
			sessions := map[string]bool{}
			for _, r := range results {
				if r.err != nil {
					t.Fatal(r.err)
				}
				switch {
				case r.status == 201 && !sessions[r.session]:
					sessions[r.session] = true
					out.created++
				case r.status == 409 && r.code == "seat_taken" && mode == 0:
					out.conflicts++
				default:
					t.Fatalf("unexpected or duplicate result: HTTP %d, code=%q session=%q", r.status, r.code, r.session)
				}
				out.times = append(out.times, r.latency)
				out.ttfb += r.firstByte
			}
			want := 1
			if mode == 1 {
				want = buyers
			}
			if len(sessions) != want {
				t.Fatalf("got %d unique holds, want %d", len(sessions), want)
			}
			held := 0
			for _, s := range grid() {
				if s.Status == "held" {
					held++
				}
			}
			if held != want {
				t.Fatalf("seat map has %d held, want %d", held, want)
			}
			for i, r := range results {
				if r.status == 201 {
					request(t, "DELETE", "/holds/"+r.session, tokens[i], nil, 204, nil)
				}
			}
		}
	}
	for mode, s := range totals {
		slices.Sort(s.times)
		n := len(s.times)
		ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
		t.Logf("scenario=%s concurrency=%d requests=%d HTTP_201=%d HTTP_409=%d req/s=%.1f p50_ms=%.2f p95_ms=%.2f mean_first_byte_ms=%.2f", []string{"same-seat", "distinct-seats"}[mode], buyers, n, s.created, s.conflicts, float64(n)/s.elapsed.Seconds(), ms(s.times[(n*50+99)/100-1]), ms(s.times[(n*95+99)/100-1]), ms(s.ttfb/time.Duration(n)))
	}
}
