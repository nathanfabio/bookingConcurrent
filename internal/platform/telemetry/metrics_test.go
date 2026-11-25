package telemetry

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, metrics *Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("scrape: %d %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func TestMetricsRegistryAndFiniteLabels(t *testing.T) {
	m := NewMetrics(nil, time.Second)
	m.RecordHTTP("UNTRUSTED-METHOD", "/holds/{sessionID}/confirm", 201, 25*time.Millisecond)
	m.BookingConfirmed()
	m.PaymentCaptured()
	m.GatewayCapture("success")
	m.GatewayCapture("declined")
	m.GatewayCapture("arbitrary-provider-error")
	body := scrape(t, m)
	for _, want := range []string{
		`booking_http_requests_total{method="OTHER",route="/holds/{sessionID}/confirm",status="201"} 1`,
		`booking_http_request_duration_seconds_count{method="OTHER",route="/holds/{sessionID}/confirm",status="201"} 1`,
		`booking_confirmations_total 1`, `booking_payments_captured_total 1`,
		`booking_payment_capture_attempts_total{outcome="success"} 1`,
		`booking_payment_capture_attempts_total{outcome="declined"} 1`,
		`booking_payment_capture_attempts_total{outcome="error"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, "UNTRUSTED-METHOD") || strings.Contains(body, "arbitrary-provider-error") {
		t.Error("unbounded caller input became a label")
	}
	// A second private registry is safe and starts fresh.
	if body := scrape(t, NewMetrics(nil, time.Second)); !strings.Contains(body, "booking_confirmations_total 0") {
		t.Error("private registry leaked prior counts")
	}
}

func TestActiveHoldsScrapeDoesNotLieOnFailure(t *testing.T) {
	fail := false
	m := NewMetrics(func(ctx context.Context, now time.Time) (int64, error) {
		if _, ok := ctx.Deadline(); !ok || now.IsZero() {
			t.Error("scrape must bound I/O and pass its observation time")
		}
		if fail {
			return 0, errors.New("redis unavailable")
		}
		return 3, nil
	}, time.Second)
	body := scrape(t, m)
	if !strings.Contains(body, "booking_active_holds 3") || !strings.Contains(body, "booking_active_holds_collection_success 1") {
		t.Fatal("successful gauge missing")
	}
	fail = true
	body = scrape(t, m)
	if strings.Contains(body, "\nbooking_active_holds ") || !strings.Contains(body, "booking_active_holds_collection_success 0") {
		t.Fatal("failed gauge must be absent, not zero or stale")
	}
	if !strings.Contains(body, "booking_confirmations_total") {
		t.Fatal("Redis failure must not hide unrelated metrics")
	}
}

func TestActiveHoldsCollectionDeadline(t *testing.T) {
	m := NewMetrics(func(ctx context.Context, now time.Time) (int64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}, time.Millisecond)
	if body := scrape(t, m); !strings.Contains(body, "booking_active_holds_collection_success 0") {
		t.Fatal("deadline must produce a failed collection indicator")
	}
}
