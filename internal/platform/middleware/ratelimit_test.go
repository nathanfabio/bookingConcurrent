package middleware

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"
)

func testRatePolicy() config.RateLimit {
	return config.RateLimit{
		General:    config.BucketPolicy{RequestsPerMinute: 120, Burst: 30},
		Auth:       config.BucketPolicy{RequestsPerMinute: 10, Burst: 2},
		Hold:       config.BucketPolicy{RequestsPerMinute: 30, Burst: 3},
		MaxClients: 2, IdleTTL: time.Minute,
	}
}

func rateHandler(limiter *RateLimiter) http.Handler {
	return limiter.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
}

func quotaRequest(handler http.Handler, method, path, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestRateLimitSharedAuthAndRefill(t *testing.T) {
	now := time.Unix(100, 0)
	l := NewRateLimiter(testRatePolicy(), func() time.Time { return now })
	h := rateHandler(l)
	for _, path := range []string{"/auth/register", "/auth/login"} {
		if w := quotaRequest(h, "POST", path, "192.0.2.1:1234"); w.Code != 204 {
			t.Fatalf("initial auth burst: %d", w.Code)
		}
	}
	for _, path := range []string{"/auth/refresh", "/auth/logout"} {
		w := quotaRequest(h, "POST", path, "192.0.2.1:9999")
		if w.Code != 429 || w.Header().Get("Retry-After") != "6" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("auth depleted: %d, headers %v", w.Code, w.Header())
		}
	}
	if w := quotaRequest(h, "POST", "/holds", "192.0.2.1:1234"); w.Code != 204 {
		t.Fatalf("auth quota must not deplete hold quota: %d", w.Code)
	}
	if w := quotaRequest(h, "POST", "/auth/login", "192.0.2.2:1234"); w.Code != 204 {
		t.Fatalf("second IP has its own quota: %d", w.Code)
	}
	now = now.Add(5500 * time.Millisecond)
	if w := quotaRequest(h, "POST", "/auth/login", "192.0.2.1:1234"); w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("fractional refill: %d, headers %v", w.Code, w.Header())
	}
	now = now.Add(500 * time.Millisecond)
	if w := quotaRequest(h, "POST", "/auth/login", "192.0.2.1:1234"); w.Code != 204 {
		t.Fatalf("retry must not delay refill: %d", w.Code)
	}
}

func TestRateLimitGeneralAndHold(t *testing.T) {
	policy := testRatePolicy()
	policy.General.Burst = 4
	l := NewRateLimiter(policy, time.Now)
	h := rateHandler(l)
	for range policy.Hold.Burst {
		if w := quotaRequest(h, "POST", "/holds", "192.0.2.1:1"); w.Code != 204 {
			t.Fatal("hold burst rejected")
		}
	}
	if w := quotaRequest(h, "POST", "/holds", "192.0.2.1:1"); w.Code != 429 {
		t.Fatal("hold burst exceeded")
	}
	// The denied hold must not debit the remaining general token.
	if w := quotaRequest(h, "GET", "/movies", "192.0.2.1:1"); w.Code != 204 {
		t.Fatal("denial consumed general quota")
	}
	if w := quotaRequest(h, "POST", "/auth/login", "192.0.2.1:1"); w.Code != 429 {
		t.Fatal("strict routes must also obey general quota")
	}
}

func TestRateLimitCapacityAndIdleEviction(t *testing.T) {
	now := time.Unix(100, 0)
	policy := testRatePolicy()
	l := NewRateLimiter(policy, func() time.Time { return now })
	h := rateHandler(l)
	for _, ip := range []string{"192.0.2.1:1", "192.0.2.2:1"} {
		quotaRequest(h, "GET", "/movies", ip)
	}
	w := quotaRequest(h, "GET", "/movies", "192.0.2.3:1")
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" || len(l.clients) != policy.MaxClients {
		t.Fatalf("capacity must reject without allocation: %d, headers=%v size=%d", w.Code, w.Header(), len(l.clients))
	}
	// Keep the first IP active. The second should become evictable first.
	now = now.Add(30 * time.Second)
	quotaRequest(h, "GET", "/movies", "192.0.2.1:1")
	now = now.Add(30 * time.Second)
	if w := quotaRequest(h, "GET", "/movies", "192.0.2.3:1"); w.Code != 204 {
		t.Fatalf("idle entry should be evicted at inclusive TTL: %d", w.Code)
	}
	if _, exists := l.clients[netip.MustParseAddr("192.0.2.2")]; exists {
		t.Error("idle entry survived")
	}
	if _, exists := l.clients[netip.MustParseAddr("192.0.2.1")]; !exists || len(l.clients) != policy.MaxClients {
		t.Error("active entry must be retained and size bounded")
	}
}

func TestRateLimitCannotResetActiveQuotaAtCapacity(t *testing.T) {
	policy := testRatePolicy()
	policy.MaxClients = 1
	now := time.Unix(100, 0)
	l := NewRateLimiter(policy, func() time.Time { return now })
	h := rateHandler(l)
	for range policy.Auth.Burst {
		quotaRequest(h, "POST", "/auth/login", "192.0.2.1:1")
	}
	for range 20 {
		quotaRequest(h, "POST", "/auth/login", "192.0.2.2:1")
	}
	if w := quotaRequest(h, "POST", "/auth/login", "192.0.2.1:1"); w.Code != 429 {
		t.Fatal("capacity pressure reset active quota")
	}
	now = now.Add(policy.IdleTTL)
	if w := quotaRequest(h, "POST", "/auth/login", "192.0.2.2:1"); w.Code != 204 {
		t.Fatal("idle capacity should be reclaimable")
	}
}

func TestRateLimitPeerIdentityAndSpoofedHeaders(t *testing.T) {
	policy := testRatePolicy()
	policy.General.Burst = 1
	l := NewRateLimiter(policy, func() time.Time { return time.Unix(100, 0) })
	h := rateHandler(l)
	if quotaRequest(h, "GET", "/movies", "192.0.2.1:1").Code != 204 {
		t.Fatal("initial burst rejected")
	}
	r := httptest.NewRequest("GET", "/movies", nil)
	r.RemoteAddr = "[::ffff:192.0.2.1]:9999"
	r.Header.Set("X-Forwarded-For", "192.0.2.2")
	r.Header.Set("X-Real-IP", "192.0.2.3")
	r.Header.Set("Forwarded", "for=192.0.2.4")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 429 || len(l.clients) != 1 {
		t.Fatalf("mapped IPv4/port/forwarded headers must not change identity: %d, size=%d", w.Code, len(l.clients))
	}
	if peerIP("[2001:db8::1]:1") != peerIP("[2001:0db8:0:0:0:0:0:1]:2") {
		t.Error("IPv6 forms must canonicalize")
	}
	if peerIP("invalid") != peerIP("also-invalid") {
		t.Error("malformed peers must share a bucket")
	}
}

func TestRateLimitProbesAndMethodScope(t *testing.T) {
	policy := testRatePolicy()
	policy.General.Burst = 1
	l := NewRateLimiter(policy, time.Now)
	h := rateHandler(l)
	for _, method := range []string{"GET", "HEAD"} {
		for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
			if quotaRequest(h, method, path, "192.0.2.1:1").Code != 204 {
				t.Fatal("probe must bypass limit")
			}
		}
	}
	if len(l.clients) != 0 {
		t.Fatal("probes must not allocate quotas")
	}
	quotaRequest(h, "POST", "/healthz", "192.0.2.1:1")
	if quotaRequest(h, "GET", "/movies", "192.0.2.1:1").Code != 429 {
		t.Fatal("non-probe methods must consume general quota")
	}
}

func TestRateLimitConcurrentBurst(t *testing.T) {
	policy := testRatePolicy()
	policy.Auth.Burst = 5
	l := NewRateLimiter(policy, func() time.Time { return time.Unix(100, 0) })
	h := rateHandler(l)
	var accepted atomic.Int32
	var rejected atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 100 {
		wg.Go(func() {
			<-start
			w := quotaRequest(h, "POST", "/auth/login", "192.0.2.1:1")
			switch w.Code {
			case 204:
				accepted.Add(1)
			case 429:
				rejected.Add(1)
				if seconds, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || seconds <= 0 {
					t.Errorf("invalid Retry-After: %q", w.Header().Get("Retry-After"))
				}
			default:
				t.Errorf("unexpected status: %d", w.Code)
			}
		})
	}
	close(start)
	wg.Wait()
	if accepted.Load() != 5 || rejected.Load() != 95 {
		t.Fatalf("accepted=%d rejected=%d, want 5/95", accepted.Load(), rejected.Load())
	}
}
