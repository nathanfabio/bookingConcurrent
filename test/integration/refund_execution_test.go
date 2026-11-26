package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	paymentadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/payment"
	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	"github.com/nathanfabio/bookingConcurrent/internal/application/refund"
)

// Simulate losing the provider's response after the refund has committed.
type lostRefundResponse struct{ provider refund.Gateway }

func (g lostRefundResponse) Refund(ctx context.Context, r refund.Request) (string, error) {
	if _, err := g.provider.Refund(ctx, r); err != nil {
		return "", err
	}
	return "", errors.New("connection lost after acceptance")
}

func TestRefundExecutionRecoversAcceptedReceipt(t *testing.T) {
	ctx := t.Context()
	// Poll the entire queue, exactly as the worker does. Give this test a
	// private database so neither existing pending rows nor other suites interfere.
	admin := connectPostgres(t)
	name := "refund_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Setenv("TEST_POSTGRES_DB", name)
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Error(err)
		}
	})
	f := newPaymentFixture(t)
	p := f.intent(uuid.NewString())
	p.HoldExpiresAt = time.Now().Add(-time.Hour)
	row, err := f.repo.InsertIntent(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.repo.Capture(ctx, p.IntentID); err != nil {
		t.Fatal(err)
	}
	var decision string
	if err = f.pool.QueryRow(ctx, "SELECT request_sandbox_refund($1)", row.ID).Scan(&decision); err != nil || decision != "requested" {
		t.Fatalf("authorization: %s %v", decision, err)
	}

	run := refund.Processor{Store: postgresadapter.RefundRepo{Pool: f.pool}, Gateway: lostRefundResponse{paymentadapter.SandboxRefundGateway{Pool: f.pool}}, BatchSize: 1}
	if n, err := run.RunOnce(ctx); err == nil || n != 0 {
		t.Fatalf("lost reply: completed=%d err=%v", n, err)
	}
	var ref string
	if err = f.pool.QueryRow(ctx, `SELECT s.reference FROM sandbox_refund_receipts s
		JOIN refund_requests r ON r.payment_id=s.idempotency_key JOIN payments p ON p.id=r.payment_id
		WHERE p.id=$1 AND p.status='refunding' AND r.completed_at IS NULL
		AND s.intent_id=p.intent_id AND s.amount_cents=p.amount_cents AND s.currency=p.currency`, row.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}

	// Fresh connections and adapter instances simulate independent restarted workers.
	restarted := connectPostgres(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			next := refund.Processor{Store: postgresadapter.RefundRepo{Pool: restarted}, Gateway: paymentadapter.SandboxRefundGateway{Pool: restarted}, BatchSize: 1}
			if _, err := next.RunOnce(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var receipts, completed int
	err = f.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM sandbox_refund_receipts),
		(SELECT count(*) FROM refund_requests r JOIN payments p ON p.id=r.payment_id
		 WHERE p.id=$1 AND p.status='refunded' AND r.completed_at IS NOT NULL AND r.provider_reference=$2)`, row.ID, ref).Scan(&receipts, &completed)
	if err != nil || receipts != 1 || completed != 1 {
		t.Fatalf("receipts=%d completed=%d err=%v", receipts, completed, err)
	}
	gateway := paymentadapter.SandboxRefundGateway{Pool: restarted}
	if _, err := gateway.Refund(ctx, refund.Request{PaymentID: row.ID, IntentID: p.IntentID, AmountCents: p.AmountCents + 1, Currency: p.Currency}); err == nil {
		t.Fatal("accepted replay with changed amount")
	}
	if n, err := run.RunOnce(ctx); n != 0 || err != nil {
		t.Fatalf("completed refund polled again: %d, %v", n, err)
	}
}
