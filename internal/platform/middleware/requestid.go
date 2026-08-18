// Package middleware provides the HTTP middleware chain (CLAUDE.md §5).
//
// Chain order, outermost first: Recovery → RequestID → Logging → (tracing,
// CORS, rate limiting in later milestones) → Auth → handler. Order matters:
// Recovery must be outermost to catch panics from everything else, and
// RequestID must run before Logging so every log line can carry the ID.
//
// This package deliberately imports no adapter code. Where a middleware
// needs adapter behavior (e.g. writing a structured error after a panic),
// the composition root injects it — platform must not depend on adapters.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// Header is the canonical request ID header, read inbound and written
// outbound so clients can correlate their own logs with ours.
const Header = "X-Request-ID"

type ctxKey int

const (
	requestIDKey ctxKey = iota
	userIDKey
)

// RequestID ensures every request has an ID: an acceptable inbound X-Request-ID
// is propagated, otherwise the trace ID from a valid W3C `traceparent`
// header is used (so distributed traces and logs share one identifier),
// otherwise a fresh UUIDv4 is minted.
//
// The ID is placed in the context (for logging/tracing) and echoed in the
// response header (for clients).
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(Header))
		if id == "" {
			id = traceIDFromTraceparent(r.Header.Get("traceparent"))
		}
		if id == "" {
			id = newUUID()
		}
		w.Header().Set(Header, id)
		next.ServeHTTP(w, r.WithContext(ContextWithRequestID(r.Context(), id)))
	})
}

// ContextWithRequestID returns a context carrying the request ID.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request ID, or "" if none was set.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// ContextWithUserID returns a context carrying the authenticated user ID.
// Set by the auth middleware (M3); read by logging and handlers.
func ContextWithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// UserIDFromContext returns the authenticated user ID if present.
func UserIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(userIDKey).(string)
	return id, ok
}

// sanitizeRequestID accepts client-supplied IDs only when they are safe to
// echo into logs, headers, and traces: 1-128 chars of [A-Za-z0-9._-].
// Anything else is discarded (and regenerated), which blocks log injection
// and header-smuggling via crafted IDs.
func sanitizeRequestID(id string) string {
	if id == "" || len(id) > 128 {
		return ""
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
		default:
			return ""
		}
	}
	return id
}

// traceIDFromTraceparent extracts the trace ID from a W3C traceparent
// header ("version-traceid-parentid-flags") when it is well-formed.
// Malformed headers yield "" so we never propagate garbage IDs.
func traceIDFromTraceparent(h string) string {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) != 4 {
		return ""
	}
	version, traceID := parts[0], parts[1]
	if len(version) != 2 || version == "ff" {
		return ""
	}
	if len(traceID) != 32 || !isHex(traceID) || traceID == strings.Repeat("0", 32) {
		return ""
	}
	return traceID
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// newUUID returns a random UUIDv4. crypto/rand is used (not math/rand):
// request IDs appear in logs and support conversations, so predictability
// would be a (small but real) weakness.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the platform has no functioning entropy
		// source; no ID we could invent would be safe to use.
		panic("middleware: crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}
