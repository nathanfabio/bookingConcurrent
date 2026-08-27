package middleware

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// okValidate accepts exactly "good-token" and maps it to a fixed user ID —
// enough to exercise the middleware without a real JWT library.
func okValidate(ctx context.Context, token string) (string, error) {
	if token == "good-token" {
		return "user-99", nil
	}
	return "", errors.New("bad token")
}

func recordingUnauthorized(calls *int) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.WriteHeader(http.StatusUnauthorized)
	}
}

func TestAuthMiddlewareRejects(t *testing.T) {
	cases := map[string]string{
		"missing header":       "",
		"wrong scheme":         "Basic abc123",
		"empty bearer":         "Bearer ",
		"whitespace bearer":    "Bearer    ",
		"invalid token":        "Bearer wrong-token",
		"lowercase scheme ok?": "bearer wrong-token", // still a bad token
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			reached := false
			h := Auth(okValidate, recordingUnauthorized(&calls))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
			}))

			req := httptest.NewRequest(http.MethodGet, "/protected", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if reached {
				t.Errorf("handler reached despite rejected request")
			}
			if calls != 1 {
				t.Errorf("writeUnauthorized called %d times, want 1", calls)
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestAuthMiddlewareInjectsUserID(t *testing.T) {
	calls := 0
	var seen string
	h := Auth(okValidate, recordingUnauthorized(&calls))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, ok := UserIDFromContext(r.Context())
		if !ok {
			t.Errorf("UserIDFromContext reported no user inside the handler")
		}
		seen = uid
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if calls != 0 {
		t.Errorf("writeUnauthorized called %d times, want 0", calls)
	}
	if seen != "user-99" {
		t.Errorf("userID in context = %q, want user-99", seen)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestAuthSchemeIsCaseInsensitive: RFC 7235 schemes are case-insensitive.
func TestAuthSchemeIsCaseInsensitive(t *testing.T) {
	calls := 0
	reached := false
	h := Auth(okValidate, recordingUnauthorized(&calls))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "BEARER good-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !reached || calls != 0 {
		t.Errorf("case-insensitive scheme rejected: reached=%v calls=%d", reached, calls)
	}
}

// TestLoggingRecordsUserIDFromAuth proves the §5 tension is resolved:
// Logging wraps Auth (auth is INNER), yet the access log still carries the
// user ID — via the recorder side channel, since the context cannot flow
// back outward.
func TestLoggingRecordsUserIDFromAuth(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	calls := 0
	// Chain order, outermost first: Logging → Auth → handler. This is the
	// production order (CLAUDE.md §5): logging runs outside auth.
	h := Logging(Auth(okValidate, recordingUnauthorized(&calls))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, `"user_id":"user-99"`) {
		t.Errorf("access log missing user_id from the auth side channel; got: %s", out)
	}
}

// TestLoggingOmitsUserIDWhenUnauthenticated: public requests still log
// cleanly, with no user_id attribute.
func TestLoggingOmitsUserIDWhenUnauthenticated(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	h := Logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/public", nil))

	if strings.Contains(buf.String(), "user_id") {
		t.Errorf("unauthenticated request log should have no user_id; got: %s", buf.String())
	}
}
