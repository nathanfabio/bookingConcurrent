package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORS(t *testing.T) {
	const origin = "https://cinema.example"
	tests := []struct {
		name      string
		method    string
		origin    string
		requested string
		headers   []string
		status    int
		called    bool
		allow     bool
	}{
		{name: "CLI without origin", method: "POST", status: 202, called: true},
		{name: "allowed browser", method: "POST", origin: origin, status: 202, called: true, allow: true},
		{name: "foreign origin", method: "POST", origin: "https://evil.example", status: 403},
		{name: "lookalike suffix", method: "POST", origin: origin + ".evil.example", status: 403},
		{name: "opaque origin", method: "POST", origin: "null", status: 403},
		{name: "different port", method: "GET", origin: origin + ":8443", status: 403},
		{name: "different scheme", method: "GET", origin: "http://cinema.example", status: 403},
		{name: "preflight", method: "OPTIONS", origin: origin, requested: "POST", headers: []string{"authorization, Content-Type", "X-Request-ID, Traceparent, Tracestate"}, status: 204, allow: true},
		{name: "preflight without headers", method: "OPTIONS", origin: origin, requested: "DELETE", status: 204, allow: true},
		{name: "preflight unsupported method", method: "OPTIONS", origin: origin, requested: "PUT", status: 403, allow: true},
		{name: "method case sensitive", method: "OPTIONS", origin: origin, requested: "post", status: 403, allow: true},
		{name: "preflight forbidden header", method: "OPTIONS", origin: origin, requested: "POST", headers: []string{"Authorization, X-Admin"}, status: 403, allow: true},
		{name: "preflight empty header token", method: "OPTIONS", origin: origin, requested: "POST", headers: []string{"Authorization,"}, status: 403, allow: true},
		{name: "ordinary options", method: "OPTIONS", origin: origin, status: 202, called: true, allow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.Header().Add("Vary", "Accept-Encoding")
				w.WriteHeader(http.StatusAccepted)
			})
			handler := CORS([]string{origin}, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			})(next)
			r := httptest.NewRequest(tt.method, "/holds", nil)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.requested != "" {
				r.Header.Set("Access-Control-Request-Method", tt.requested)
			}
			for _, header := range tt.headers {
				r.Header.Add("Access-Control-Request-Headers", header)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.status || called != tt.called {
				t.Fatalf("status=%d called=%t, want %d/%t", w.Code, called, tt.status, tt.called)
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); (got == origin) != tt.allow {
				t.Errorf("allow-origin=%q, allowed=%t", got, tt.allow)
			}
			vary := strings.Join(w.Header().Values("Vary"), ",")
			if !strings.Contains(vary, "Origin") || (called && !strings.Contains(vary, "Accept-Encoding")) {
				t.Errorf("Vary must preserve cache separation: %q", vary)
			}
			if tt.allow && w.Header().Get("Access-Control-Allow-Credentials") != "true" {
				t.Error("allowed origin must support credentials")
			}
			if tt.status == http.StatusNoContent {
				if w.Body.Len() != 0 || w.Header().Get("Access-Control-Allow-Methods") == "" || w.Header().Get("Access-Control-Allow-Headers") == "" {
					t.Fatal("preflight must return headers and an empty body")
				}
			}
			if tt.method == "OPTIONS" && (!strings.Contains(vary, "Access-Control-Request-Method") || !strings.Contains(vary, "Access-Control-Request-Headers")) {
				t.Errorf("preflight cache dimensions missing: %q", vary)
			}
		})
	}
}

func TestCORSRejectsAmbiguousOriginAndMethod(t *testing.T) {
	for _, header := range []string{"Origin", "Access-Control-Request-Method"} {
		t.Run(header, func(t *testing.T) {
			handler := CORS([]string{"https://cinema.example"}, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("must not reach handler") }))
			r := httptest.NewRequest("OPTIONS", "/holds", nil)
			r.Header.Set("Origin", "https://cinema.example")
			r.Header.Set("Access-Control-Request-Method", "POST")
			r.Header.Add(header, r.Header.Get(header))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("duplicate %s: %d, want 403", header, w.Code)
			}
		})
	}
}

func TestCORSEmptyAllowlist(t *testing.T) {
	handler := CORS(nil, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202) }))
	for _, origin := range []string{"", "https://cinema.example"} {
		r := httptest.NewRequest("POST", "/auth/login", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		want := 202
		if origin != "" {
			want = 403
		}
		if w.Code != want {
			t.Errorf("origin %q: %d, want %d", origin, w.Code, want)
		}
	}
}
