package middleware

import (
	"container/list"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"
)

// RateLimiter owns bounded per-peer buckets for ONE API process. Forwarded
// headers are deliberately ignored; trusting them requires a separate proxy
// trust policy. A mutex makes refill/check/debit atomic across all buckets.
// No goroutine or timer needs shutdown (ADR 0011).
type RateLimiter struct {
	mu      sync.Mutex
	policy  config.RateLimit
	now     func() time.Time
	clients map[netip.Addr]*list.Element
	recent  list.List // front = most recent; back = earliest safe eviction
}

type clientQuota struct {
	ip       netip.Addr
	lastSeen time.Time
	tokens   [3]float64 // general, auth, hold; fixed-size, no path-based keys
}

// NewRateLimiter requires a policy validated by config.Load. The injected
// clock makes refill and eviction tests deterministic without sleeping.
func NewRateLimiter(policy config.RateLimit, now func() time.Time) *RateLimiter {
	return &RateLimiter{policy: policy, now: now, clients: make(map[netip.Addr]*list.Element)}
}

// Wrap rejects before auth, hashing, or application I/O. Health probes are
// exempt only for GET/HEAD; successful CORS preflights terminate outside it.
func (l *RateLimiter) Wrap(next http.Handler, writeRejected func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbe(r) {
			next.ServeHTTP(w, r)
			return
		}
		if wait := l.allow(peerIP(r.RemoteAddr), quotaClass(r)); wait > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(wait.Seconds())), 10))
			w.Header().Set("Cache-Control", "no-store")
			writeRejected(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func peerIP(remote string) netip.Addr {
	if peer, err := netip.ParseAddrPort(remote); err == nil {
		return peer.Addr().Unmap().WithZone("")
	}
	// net/http supplies IP:port. Malformed/custom transports share one
	// bucket instead of creating attacker-controlled string keys.
	return netip.Addr{}
}

func quotaClass(r *http.Request) int {
	if r.Method == http.MethodPost {
		switch r.URL.Path {
		case "/auth/register", "/auth/login", "/auth/refresh", "/auth/logout":
			return 1
		case "/holds":
			return 2
		}
	}
	return 0
}

// allow consumes all applicable tokens or none. Retrying a rejected request
// therefore cannot push its next available token further into the future.
func (l *RateLimiter) allow(ip netip.Addr, class int) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	policies := [3]config.BucketPolicy{l.policy.General, l.policy.Auth, l.policy.Hold}
	element := l.clients[ip]
	if element == nil {
		// Evict only idle entries, never active ones: evicting an active
		// attacker would grant a fresh burst when it comes back. TTL is
		// validated to exceed the slowest full-bucket refill time.
		for oldest := l.recent.Back(); oldest != nil; oldest = l.recent.Back() {
			client := oldest.Value.(*clientQuota)
			if now.Sub(client.lastSeen) < l.policy.IdleTTL {
				break
			}
			delete(l.clients, client.ip)
			l.recent.Remove(oldest)
		}
		if len(l.clients) >= l.policy.MaxClients {
			oldest := l.recent.Back().Value.(*clientQuota)
			return max(time.Nanosecond, l.policy.IdleTTL-now.Sub(oldest.lastSeen))
		}
		client := &clientQuota{ip: ip, lastSeen: now}
		for i, policy := range policies {
			client.tokens[i] = float64(policy.Burst)
		}
		element = l.recent.PushFront(client)
		l.clients[ip] = element
	}
	client := element.Value.(*clientQuota)
	elapsed := max(0, now.Sub(client.lastSeen).Minutes())
	for i, policy := range policies {
		client.tokens[i] = min(float64(policy.Burst), client.tokens[i]+elapsed*float64(policy.RequestsPerMinute))
	}
	client.lastSeen = now
	l.recent.MoveToFront(element)
	var wait time.Duration
	for _, i := range []int{0, class} {
		if client.tokens[i] < 1 {
			seconds := (1 - client.tokens[i]) * 60 / float64(policies[i].RequestsPerMinute)
			wait = max(wait, time.Duration(math.Ceil(seconds*float64(time.Second))))
		}
	}
	if wait > 0 {
		return wait
	}
	client.tokens[0]--
	if class != 0 {
		client.tokens[class]--
	}
	return 0
}
