package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nathanfabio/bookingConcurrent/internal/application/refund"
)

type RefundRepo struct{ Pool *pgxpool.Pool }

// Touch the oldest attempts first so a failing request cannot starve a backlog.
// Overlapping passes may retry a request once this statement commits. The
// provider key and the guarded local completion make that overlap safe.
func (r RefundRepo) NextBatch(ctx context.Context, limit int) ([]refund.Request, error) {
	rows, err := r.Pool.Query(ctx, `WITH candidates AS (
		SELECT r.payment_id,p.intent_id FROM refund_requests r JOIN payments p ON p.id=r.payment_id
		WHERE r.completed_at IS NULL AND p.status='refunding' AND p.gateway='fake'
		ORDER BY r.last_attempt_at NULLS FIRST,r.requested_at,r.payment_id
		LIMIT $1 FOR UPDATE OF r SKIP LOCKED
	) UPDATE refund_requests r SET last_attempt_at=clock_timestamp()
	FROM candidates c WHERE r.payment_id=c.payment_id
	RETURNING r.payment_id,c.intent_id,r.amount_cents,r.currency`, limit)
	if err != nil {
		return nil, fmt.Errorf("select pending refunds: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[refund.Request])
}

func (r RefundRepo) Complete(ctx context.Context, id, reference string) error {
	if reference == "" {
		return errors.New("refund completion requires a provider reference")
	}
	return pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, "SELECT status FROM payments WHERE id=$1 FOR UPDATE", id).Scan(&status); err != nil {
			return err
		}
		if status == "refunded" {
			var matches bool
			if err := tx.QueryRow(ctx, "SELECT provider_reference=$2 FROM refund_requests WHERE payment_id=$1", id, reference).Scan(&matches); err != nil {
				return err
			}
			if matches {
				return nil
			}
			return errors.New("refund receipt differs from committed reference")
		}
		if status != "refunding" {
			return fmt.Errorf("cannot complete refund in payment state %s", status)
		}
		tag, err := tx.Exec(ctx, `UPDATE refund_requests SET provider_reference=$2,completed_at=clock_timestamp()
			WHERE payment_id=$1 AND completed_at IS NULL`, id, reference)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("pending refund request missing")
		}
		_, err = tx.Exec(ctx, "UPDATE payments SET status='refunded',updated_at=clock_timestamp() WHERE id=$1", id)
		return err
	})
}
