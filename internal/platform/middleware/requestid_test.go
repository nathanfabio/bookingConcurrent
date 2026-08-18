package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDGeneratesWhenAbsent(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if seen == "" {
		t.Fatal("expected a generated request ID in context")
	}
	if len(seen) != 36 {
		t.Errorf("generated ID %q should be UUID-shaped (36 chars)", seen)
	}
	if got := rec.Header().Get(Header); got != seen {
		t.Errorf("response header %q = %q, want %q", Header, got, seen)
	}
}

func TestRequestIDPropagatesAcceptableClientID(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(Header, "abc-123_DEF.456")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "abc-123_DEF.456" {
		t.Errorf("request ID = %q, want client-supplied value", seen)
	}
}

func TestRequestIDRejectsMaliciousClientID(t *testing.T) {
	bad := []string{
		"line1\nline2",           // log injection
		"a b",                    // space
		strings.Repeat("x", 129), // too long
		"id\r\nInjected: header", // CRLF smuggling
	}
	for _, id := range bad {
		var seen string
		h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = RequestIDFromContext(r.Context())
		}))
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set(Header, id)
		h.ServeHTTP(httptest.NewRecorder(), req)

		if seen == id {
			t.Errorf("malicious ID %q was propagated; it should have been regenerated", id)
		}
		if seen == "" {
			t.Errorf("a fresh ID should have been generated to replace %q", id)
		}
	}
}

func TestRequestIDDerivesFromTraceparent(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != traceID {
		t.Errorf("request ID = %q, want trace ID %q from traceparent", seen, traceID)
	}
}

func TestRequestIDPrefersExplicitHeaderOverTraceparent(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(Header, "explicit-id")
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "explicit-id" {
		t.Errorf("request ID = %q, want explicit X-Request-ID to win", seen)
	}
}

func TestTraceIDFromTraceparentRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":              "",
		"too few parts":      "00-abc-01",
		"too many parts":     "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
		"version ff":         "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"all-zero trace id":  "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"short trace id":     "00-4bf92f-00f067aa0ba902b7-01",
		"non-hex trace id":   "00-zbf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"bad version length": "0-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}
	for name, header := range cases {
		if got := traceIDFromTraceparent(header); got != "" {
			t.Errorf("%s: traceIDFromTraceparent(%q) = %q, want empty", name, header, got)
		}
	}
}
