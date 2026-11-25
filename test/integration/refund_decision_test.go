package integration

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	domain "github.com/nathanfabio/bookingConcurrent/internal/domain/payment"
)

func TestRefundDecisionEligibility(t *testing.T) {
	f := newPaymentFixture(t)
	ctx := t.Context()
	review := func(id, want string) {
		t.Helper()
		var got string
		if err := f.pool.QueryRow(ctx, "SELECT request_sandbox_refund($1)", id).Scan(&got); err != nil || got != want {
			t.Fatalf("review = %q, %v; want %q", got, err, want)
		}
	}
	review(uuid.NewString(), "payment_not_found")
	for _, tc := range []struct {
		state    string
		deadline time.Time
	}{
		{"unknown_deadline", time.Time{}}, {"before_deadline", time.Now().Add(time.Hour)}, {"requested", time.Now().Add(-time.Hour)},
	} {
		p := f.intent(uuid.NewString())
		p.HoldExpiresAt = tc.deadline
		row, err := f.repo.InsertIntent(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		review(row.ID, "not_captured")
		if _, _, err = f.repo.Capture(ctx, row.IntentID); err != nil {
			t.Fatal(err)
		}
		review(row.ID, tc.state)
		if tc.state == "requested" {
			review(row.ID, "already_requested")
			b := newBookingFixture(t, 1)
			_, err = b.repo.Confirm(ctx, b.booking(row.BookingSessionID, b.users[0], "A", 1))
			if !errors.Is(err, domain.ErrPaymentNotCaptured) {
				t.Fatalf("confirm after authorization: %v", err)
			}
		}
	}
	// Cover the sale-first ordering deterministically, regardless of which
	// goroutine wins in the contention test.
	b := newBookingFixture(t, 1)
	session := paidSession(t, b.pool)
	if _, err := b.repo.Confirm(ctx, b.booking(session, b.users[0], "A", 1)); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := b.pool.QueryRow(ctx, "SELECT id FROM payments WHERE booking_session_id=$1", session).Scan(&id); err != nil {
		t.Fatal(err)
	}
	review(id, "booking_exists")

}

func TestConcurrentConfirmAndRefundDecision(t *testing.T) {
	f := newBookingFixture(t, 1)
	ctx := t.Context()
	payments := postgresadapter.NewPaymentRepo(f.pool)
	session := uuid.NewString()
	p, err := payments.InsertIntent(ctx, domain.Payment{BookingSessionID: session, Gateway: "fake", IntentID: "pi_fake_" + session,
		AmountCents: 1500, Currency: "usd", HoldExpiresAt: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = payments.Capture(ctx, p.IntentID); err != nil {
		t.Fatal(err)
	}

	// Model a confirmation already past its live-hold check when review starts.
	start := make(chan struct{})
	var wg sync.WaitGroup
	var confirmErr error
	wg.Go(func() { <-start; _, confirmErr = f.repo.Confirm(ctx, f.booking(session, f.users[0], "A", 1)) })
	results := make([]string, 12)
	for i := range results {
		wg.Go(func() {
			<-start
			if err := f.pool.QueryRow(ctx, "SELECT request_sandbox_refund($1)", p.ID).Scan(&results[i]); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	var status string
	var requests, bookings, events int
	if err := f.pool.QueryRow(ctx, `SELECT status,
		(SELECT count(*) FROM refund_requests WHERE payment_id=p.id AND amount_cents=p.amount_cents AND currency=p.currency),
		(SELECT count(*) FROM confirmed_bookings WHERE session_id=p.booking_session_id),
		(SELECT count(*) FROM outbox WHERE payload->>'session_id'=p.booking_session_id)
		FROM payments p WHERE id=$1`, p.ID).Scan(&status, &requests, &bookings, &events); err != nil {
		t.Fatal(err)
	}
	created := 0
	for _, outcome := range results {
		if confirmErr == nil && outcome != "booking_exists" {
			t.Fatalf("review after sale: %q", outcome)
		}
		switch outcome {
		case "requested":
			created++
		case "already_requested", "booking_exists":
		default:
			t.Fatalf("unexpected outcome %q", outcome)
		}
	}
	if confirmErr == nil {
		if bookings != 1 || events != 1 || requests != 0 || status != "captured" {
			t.Fatalf("sale: bookings=%d events=%d requests=%d status=%s", bookings, events, requests, status)
		}
	} else if !errors.Is(confirmErr, domain.ErrPaymentNotCaptured) || bookings != 0 || events != 0 || requests != 1 || created != 1 || status != "refunding" {
		t.Fatalf("refund: confirm=%v bookings=%d events=%d requests=%d created=%d status=%s", confirmErr, bookings, events, requests, created, status)
	}
}
