package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/middleware"
)

func TestHTTPProtectionChain(t *testing.T) {
	const origin = "https://cinema.example"
	var validations, calls int
	protected := middleware.Auth(func(ctx context.Context, token string) (string, error) {
		validations++
		return "alice", nil
	}, WriteUnauthorized)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
	}))
	policy := config.RateLimit{
		General:    config.BucketPolicy{RequestsPerMinute: 120, Burst: 30},
		Auth:       config.BucketPolicy{RequestsPerMinute: 10, Burst: 1},
		Hold:       config.BucketPolicy{RequestsPerMinute: 30, Burst: 1},
		MaxClients: 100, IdleTTL: time.Minute,
	}
	limiter := middleware.NewRateLimiter(policy, func() time.Time { return time.Unix(100, 0) })
	// This is the same outer order as cmd/api, with the real error writers.
	handler := protected
	handler = limiter.Wrap(handler, WriteRateLimited)
	handler = middleware.CORS([]string{origin}, WriteCORSRejected)(handler)
	handler = middleware.Logging(handler)
	handler = middleware.RequestID(handler)
	handler = middleware.Recovery(WriteInternalError)(handler)
	request := func(method, origin, token string, preflight bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/holds", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("Origin", origin)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if preflight {
			r.Header.Set("Access-Control-Request-Method", "POST")
			r.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Header().Get("X-Request-ID") == "" {
			t.Fatal("every outcome must carry a request ID")
		}
		return w
	}
	assertError := func(w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		var body errorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != status || body.Code != code || body.Message == "" {
			t.Fatalf("got %d %s; want %d/%s", w.Code, w.Body.String(), status, code)
		}
	}
	assertError(request("POST", "https://evil.example", "valid", false), 403, CodeCORSRejected)
	// Preflight is never authenticated or charged; repeated preflights must
	// not prevent the first actual request, even with a burst of one.
	for range 3 {
		if w := request("OPTIONS", origin, "", true); w.Code != 204 || w.Body.Len() != 0 {
			t.Fatalf("preflight: %d %s", w.Code, w.Body.String())
		}
	}
	if validations != 0 || calls != 0 {
		t.Fatal("CORS must precede auth and handlers")
	}
	w := request("POST", origin, "", false)
	assertError(w, 401, CodeUnauthorized)
	if w.Header().Get("Access-Control-Allow-Origin") != origin {
		t.Fatal("browser must be able to read auth error")
	}
	w = request("POST", origin, "valid", false)
	assertError(w, 429, CodeRateLimited)
	if w.Header().Get("Access-Control-Allow-Origin") != origin || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("browser-visible throttling headers missing: %v", w.Header())
	}
	if validations != 0 || calls != 0 {
		t.Fatal("rate limit must reject before JWT validation and handler work")
	}
	// Auth policy must also apply to the public password-hashing endpoints.
	r := httptest.NewRequest("POST", "/auth/login", nil)
	r.RemoteAddr = "192.0.2.2:1234"
	publicCalls := 0
	public := limiter.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicCalls++
		w.WriteHeader(200)
	}), WriteRateLimited)
	public.ServeHTTP(httptest.NewRecorder(), r)
	w = httptest.NewRecorder()
	public.ServeHTTP(w, r)
	assertError(w, 429, CodeRateLimited)
	if publicCalls != 1 {
		t.Fatalf("public auth handler calls=%d, want 1", publicCalls)
	}
}
