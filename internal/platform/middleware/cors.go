package middleware

import (
	"net/http"
	"strings"
)

// CORS checks an exact allowlist before downstream work and answers preflight
// without authentication or quota consumption. Origin is a browser policy,
// not authentication: non-browser clients may omit or forge it (ADR 0011).
func CORS(origins []string, writeDenied func(http.ResponseWriter, *http.Request)) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(origins))
	for _, origin := range origins {
		allowed[origin] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Vary is needed even without Origin: a shared cache must not reuse
			// a non-CORS response for an allowed browser origin, or vice versa.
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				w.Header().Add("Vary", "Access-Control-Request-Method")
				w.Header().Add("Vary", "Access-Control-Request-Headers")
			}
			values := r.Header.Values("Origin")
			if len(values) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			if len(values) != 1 || !allowed[values[0]] {
				writeDenied(w, r)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", values[0])
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After")
			if r.Method != http.MethodOptions || r.Header.Get("Access-Control-Request-Method") == "" {
				next.ServeHTTP(w, r)
				return
			}
			methods := r.Header.Values("Access-Control-Request-Method")
			if len(methods) != 1 || !corsMethod(methods[0]) || !corsHeaders(r.Header.Values("Access-Control-Request-Headers")) {
				writeDenied(w, r)
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID, Traceparent, Tracestate")
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

func corsMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete:
		return true
	default:
		return false
	}
}

func corsHeaders(values []string) bool {
	for _, value := range values {
		for _, header := range strings.Split(value, ",") {
			switch strings.ToLower(strings.TrimSpace(header)) {
			case "authorization", "content-type", "idempotency-key", "x-request-id", "traceparent", "tracestate":
			default:
				return false
			}
		}
	}
	return true
}
