// Package observability decorates application ports with operational metrics.
// Domain/use cases stay independent of Prometheus and OpenTelemetry; the
// composition root selects these wrappers around real stores or test fakes.
package observability

import (
	"context"
	"errors"

	appbooking "github.com/nathanfabio/bookingConcurrent/internal/application/booking"
	apppayment "github.com/nathanfabio/bookingConcurrent/internal/application/payment"
	domainbooking "github.com/nathanfabio/bookingConcurrent/internal/domain/booking"
	domainpayment "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/telemetry"
)

var (
	_ appbooking.BookingStore   = (*Bookings)(nil)
	_ apppayment.PaymentStore   = (*Payments)(nil)
	_ appbooking.PaymentFinder  = (*Payments)(nil)
	_ apppayment.PaymentGateway = (*Gateway)(nil)
)

type Bookings struct {
	appbooking.BookingStore
	Metrics *telemetry.Metrics
}

func (s *Bookings) Confirm(ctx context.Context, b domainbooking.Booking) (domainbooking.Booking, error) {
	row, err := s.BookingStore.Confirm(ctx, b)
	if err == nil {
		// Confirm's contract is a NEW booking + outbox commit or a conflict.
		// HTTP 200 recovery never inserts and cannot inflate this counter.
		s.Metrics.BookingConfirmed()
	}
	return row, err
}

type Payments struct {
	apppayment.PaymentStore
	Metrics *telemetry.Metrics
}

func (s *Payments) Capture(ctx context.Context, intentID string) (domainpayment.Payment, bool, error) {
	row, changed, err := s.PaymentStore.Capture(ctx, intentID)
	if err == nil && changed {
		s.Metrics.PaymentCaptured()
	}
	return row, changed, err
}

type Gateway struct {
	apppayment.PaymentGateway
	Metrics *telemetry.Metrics
}

func (g *Gateway) CaptureIntent(ctx context.Context, id string, cents int) error {
	err := g.PaymentGateway.CaptureIntent(ctx, id, cents)
	outcome := "success"
	if errors.Is(err, domainpayment.ErrPaymentDeclined) {
		outcome = "declined"
	} else if err != nil {
		outcome = "error"
	}
	g.Metrics.GatewayCapture(outcome)
	return err
}
