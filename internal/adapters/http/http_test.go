package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteErrorEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusConflict, "seat_taken", "seat is already held")

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Code != "seat_taken" || body.Message != "seat is already held" {
		t.Errorf("unexpected envelope: %+v", body)
	}
}

func TestWriteErrorFieldsAreSnakeCase(t *testing.T) {
	// The reference implementation broke a frontend by drifting between
	// movieID and movie_id. Pin the envelope field names here so any
	// rename is deliberate.
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusBadRequest, "x", "y")

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"message", "code"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("error envelope missing snake_case field %q; got keys %v", key, raw)
		}
	}
}

func TestWriteInternalErrorRevealsNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteInternalError(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "secret-panic-value")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); len(body) == 0 {
		t.Error("expected a JSON body even for internal errors")
	}
	var parsed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("body must be valid JSON: %v", err)
	}
	if parsed["code"] != CodeInternal {
		t.Errorf("code = %v, want %s", parsed["code"], CodeInternal)
	}
}

func TestHealthzAlwaysOK(t *testing.T) {
	rec := httptest.NewRecorder()
	HealthzHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (liveness must not depend on downstreams)", rec.Code)
	}
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Status != "ok" {
		t.Errorf("body = %s, err = %v", rec.Body.String(), err)
	}
}

func TestReadyzNoChecksIsReady(t *testing.T) {
	rec := httptest.NewRecorder()
	ReadyzHandler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with zero registered checks", rec.Code)
	}
}

func TestReadyzReportsFailingDependency(t *testing.T) {
	checks := []ReadinessCheck{
		{Name: "redis", Check: func(ctx context.Context) error { return nil }},
		{Name: "postgres", Check: func(ctx context.Context) error { return errors.New("connection refused") }},
	}

	rec := httptest.NewRecorder()
	ReadyzHandler(checks).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}

	var body readyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body.Status != "unavailable" {
		t.Errorf("status field = %q, want unavailable", body.Status)
	}
	if body.Checks["redis"] != "ok" {
		t.Errorf("redis check = %q, want ok", body.Checks["redis"])
	}
	if body.Checks["postgres"] != "connection refused" {
		t.Errorf("postgres check = %q, want the error surfaced", body.Checks["postgres"])
	}
}

func TestReadyzRunsAllChecksEvenAfterFailure(t *testing.T) {
	ran := 0
	checks := []ReadinessCheck{
		{Name: "first", Check: func(ctx context.Context) error { return errors.New("down") }},
		{Name: "second", Check: func(ctx context.Context) error { ran++; return nil }},
	}

	rec := httptest.NewRecorder()
	ReadyzHandler(checks).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if ran != 1 {
		t.Errorf("second check ran %d times, want 1 — operators need the full picture", ran)
	}
}
