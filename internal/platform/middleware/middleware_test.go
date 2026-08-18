package middleware

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoveryCatchesPanicAndCallsWriter(t *testing.T) {
	var recovered any
	writeErr := func(w http.ResponseWriter, r *http.Request, rec any) {
		recovered = rec
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom handled","code":"internal_error"}`)
	}

	h := Recovery(writeErr)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if recovered != "kaboom" {
		t.Errorf("recovered = %v, want kaboom", recovered)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "boom handled") {
		t.Errorf("body = %q, want injected error payload", rec.Body.String())
	}
}

func TestRecoveryFallsBackWithoutWriter(t *testing.T) {
	h := Recovery(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestRecoveryDoesNotInterfereWithNormalRequests(t *testing.T) {
	h := Recovery(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418 passthrough", rec.Code)
	}
}

func TestRecoveryLogsPanicWithRequestID(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	h := RequestID(Recovery(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("logged-panic")
	})))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic-path", nil))

	out := buf.String()
	for _, want := range []string{"panic recovered", "logged-panic", "request_id", "/panic-path", "stack"} {
		if !strings.Contains(out, want) {
			t.Errorf("panic log missing %q; got: %s", want, out)
		}
	}
}

func TestLoggingRecordsStatusDurationAndRequestID(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	h := RequestID(Logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/things", nil))

	out := buf.String()
	for _, want := range []string{`"status":201`, `"method":"POST"`, `"path":"/things"`, "request_id", "duration_ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("access log missing %q; got: %s", want, out)
		}
	}
}

func TestLoggingImplicit200(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	h := Logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello") // no WriteHeader → implicit 200
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	if !strings.Contains(buf.String(), `"status":200`) {
		t.Errorf("implicit write should log status 200; got: %s", buf.String())
	}
}

func TestLoggingSkipsHealthEndpoints(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	h := Logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, path := range []string{"/healthz", "/readyz"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	if buf.Len() != 0 {
		t.Errorf("health endpoints should not be logged; got: %s", buf.String())
	}
}

func TestLoggingPanicsStillLog500AndPropagate(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	h := Logging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("downstream")
	}))

	propagated := false
	func() {
		defer func() {
			if recover() != nil {
				propagated = true
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}()

	if !propagated {
		t.Fatal("Logging must re-panic so the outer Recovery middleware can act")
	}
	if !strings.Contains(buf.String(), `"status":500`) {
		t.Errorf("panicked request should log status 500; got: %s", buf.String())
	}
}

func TestUserIDContextRoundTrip(t *testing.T) {
	ctx := ContextWithUserID(context.Background(), "user-42")
	uid, ok := UserIDFromContext(ctx)
	if !ok || uid != "user-42" {
		t.Errorf("UserIDFromContext = (%q, %v), want (user-42, true)", uid, ok)
	}
	if _, ok := UserIDFromContext(context.Background()); ok {
		t.Error("empty context must report no user")
	}
}
