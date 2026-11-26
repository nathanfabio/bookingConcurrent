package postgres

import (
	"context"
	"fmt"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres/sqlcgen"
	appoutbox "github.com/nathanfabio/bookingConcurrent/internal/application/outbox"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Compile-time proof the adapter satisfies the port.
var _ appoutbox.OutboxStore = (*OutboxRepo)(nil)

// OutboxRepo implements outbox.OutboxStore against Postgres
// (migrations/00004). It is the relay's ONLY reader of the outbox table;
// the writer half is BookingRepo.Confirm, which inserts the row inside the
// confirm transaction (ADR 0006's commit point).
//
// No transactions here: ListPending is one indexed scan (the partial
// outbox_pending_idx) and MarkPublished is one guarded UPDATE. The relay's
// publish-then-mark ordering provides the crash safety (ADR 0009), so the
// store needs no locking of its own — a single relay instance is the
// documented deployment shape.
type OutboxRepo struct {
	q *sqlcgen.Queries
}

// NewOutboxRepo builds the repository over the shared pool.
func NewOutboxRepo(pool *pgxpool.Pool) *OutboxRepo {
	return &OutboxRepo{q: sqlcgen.New(pool)}
}

// ListPending implements outbox.OutboxStore. The BIGSERIAL id stays inside
// sqlcgen; the port's contract is keyed by event_id alone.
func (r *OutboxRepo) ListPending(ctx context.Context, limit int) ([]appoutbox.Event, error) {
	rows, err := r.q.ListPendingOutboxEvents(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("outbox: list pending: %w", err)
	}
	events := make([]appoutbox.Event, 0, len(rows))
	for _, row := range rows {
		events = append(events, appoutbox.Event{
			ID:      row.EventID,
			Type:    row.EventType,
			Payload: row.Payload,
		})
	}
	return events, nil
}

// MarkPublished implements outbox.OutboxStore. The SQL carries the
// "AND published_at IS NULL" guard, so a replayed or concurrent mark is a
// no-op, not an error.
func (r *OutboxRepo) MarkPublished(ctx context.Context, eventIDs []string) error {
	if len(eventIDs) == 0 {
		return nil
	}
	if err := r.q.MarkOutboxEventsPublished(ctx, eventIDs); err != nil {
		return fmt.Errorf("outbox: mark published: %w", err)
	}
	return nil
}

func (r *OutboxRepo) InspectDelivery(ctx context.Context, id string) (appoutbox.Delivery, error) {
	row, err := r.q.InspectOutboxDelivery(ctx, id)
	if err != nil {
		return appoutbox.Delivery{}, fmt.Errorf("inspect outbox event %s: %w", id, err)
	}
	return appoutbox.Delivery{Event: appoutbox.Event{ID: row.EventID, Type: row.EventType, Payload: row.Payload}, Published: row.Published, Processed: row.Processed}, nil
}
