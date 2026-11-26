package payment

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nathanfabio/bookingConcurrent/internal/application/refund"
)

// SandboxRefundGateway stores provider-side receipts in their own transaction.
// A new adapter (or worker process) can recover an accepted refund by key.
type SandboxRefundGateway struct{ Pool *pgxpool.Pool }

func (g SandboxRefundGateway) Refund(ctx context.Context, r refund.Request) (string, error) {
	var ref string
	err := g.Pool.QueryRow(ctx, `INSERT INTO sandbox_refund_receipts AS accepted
		(idempotency_key,intent_id,amount_cents,currency) VALUES ($1,$2,$3,$4)
		ON CONFLICT (idempotency_key) DO UPDATE SET idempotency_key=EXCLUDED.idempotency_key
		WHERE (accepted.intent_id,accepted.amount_cents,accepted.currency) =
		      (EXCLUDED.intent_id,EXCLUDED.amount_cents,EXCLUDED.currency)
		RETURNING reference`, r.PaymentID, r.IntentID, r.AmountCents, r.Currency).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("sandbox refund: idempotency key reused with different terms")
	}
	if err != nil {
		return "", fmt.Errorf("sandbox refund submission: %w", err)
	}
	return ref, nil
}
