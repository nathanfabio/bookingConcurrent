package observability_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/memory"
	"github.com/nathanfabio/bookingConcurrent/internal/adapters/observability"
	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	apppayment "github.com/nathanfabio/bookingConcurrent/internal/application/payment"
	domainbooking "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainpayment "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/telemetry"
)

func assertMetric(t *testing.T, m *telemetry.Metrics, line string) {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), line) {
		t.Fatalf("metric missing: %s (scrape status %d)", line, w.Code)
	}
}

func TestConcurrentWritesCountOnlyNewDurableOutcomes(t *testing.T) {
	ctx := context.Background()
	clock := memory.NewManualClock(time.Now())
	m := telemetry.NewMetrics(nil, time.Second)
	payments := &observability.Payments{PaymentStore: memory.NewPaymentStore(clock), Metrics: m}
	bookings := &observability.Bookings{BookingStore: memory.NewBookingStore(clock), Metrics: m}
	_, err := payments.InsertIntent(ctx, domainpayment.Payment{
		BookingSessionID: "session", IntentID: "pi_fake_test", Gateway: "fake", AmountCents: 1200, Currency: "brl",
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 50 {
		wg.Go(func() {
			<-start
			if _, _, err := payments.Capture(ctx, "pi_fake_test"); err != nil {
				t.Error(err)
			}
			_, err := bookings.Confirm(ctx, domainbooking.Booking{
				SessionID: "session", ScreeningID: "screening", Seat: domainbooking.Seat{Row: "A", Number: 1}, UserID: "alice",
			})
			if err != nil && !errors.Is(err, domainbooking.ErrSessionAlreadyConfirmed) {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	assertMetric(t, m, "booking_confirmations_total 1\n")
	assertMetric(t, m, "booking_payments_captured_total 1\n")
	// A recovered service replay never calls Confirm again.
	svc := appbooking.NewService(nil, bookings, nil, nil, time.Minute, clock.Now)
	if res, err := svc.Confirm(ctx, "alice", "session"); err != nil || res.Created {
		t.Fatalf("service replay: %+v %v", res, err)
	}
	assertMetric(t, m, "booking_confirmations_total 1\n")
	if _, _, err := payments.Capture(ctx, "missing"); !errors.Is(err, domainpayment.ErrPaymentNotFound) {
		t.Fatalf("wrapper changed error: %v", err)
	}
	assertMetric(t, m, "booking_payments_captured_total 1\n")
}

type captureGateway struct {
	apppayment.PaymentGateway
	err error
}

func (g captureGateway) CaptureIntent(context.Context, string, int) error { return g.err }

func TestGatewayCaptureOutcomes(t *testing.T) {
	m := telemetry.NewMetrics(nil, time.Second)
	for _, err := range []error{nil, domainpayment.ErrPaymentDeclined, errors.New("provider unavailable")} {
		gateway := &observability.Gateway{PaymentGateway: captureGateway{err: err}, Metrics: m}
		if got := gateway.CaptureIntent(context.Background(), "private-intent", 1200); !errors.Is(got, err) {
			t.Fatalf("gateway error = %v, want %v", got, err)
		}
	}
	for _, outcome := range []string{"success", "declined", "error"} {
		assertMetric(t, m, `booking_payment_capture_attempts_total{outcome="`+outcome+`"} 1`)
	}
}
