package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres/sqlcgen"
	appnotification "github.com/nathanfabio/bookingConcurrent/internal/application/notification"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Compile-time proof the adapter satisfies the port.
var _ appnotification.ProcessedEvents = (*ProcessedEventRepo)(nil)

// ProcessedEventRepo implements notification.ProcessedEvents against
// Postgres (migrations/00010). The whole idempotency guarantee is ONE
// statement — INSERT .. ON CONFLICT (event_id) DO NOTHING RETURNING — so
// the primary key is the arbiter of "exactly one winner per event_id" under
// any interleaving, the same schema-as-arbiter posture the booking and
// payment repos take (ADR 0006, ADR 0008).
type ProcessedEventRepo struct {
	q *sqlcgen.Queries
}

// NewProcessedEventRepo builds the repository over the shared pool.
func NewProcessedEventRepo(pool *pgxpool.Pool) *ProcessedEventRepo {
	return &ProcessedEventRepo{q: sqlcgen.New(pool)}
}

// Claim implements notification.ProcessedEvents: true when THIS call
// inserted the row (first delivery wins), false when the conflict branch
// fired (a duplicate). pgx surfaces "RETURNING produced no row" as
// ErrNoRows, which is the duplicate signal — not an error.
func (r *ProcessedEventRepo) Claim(ctx context.Context, eventID, eventType string) (bool, error) {
	_, err := r.q.ClaimProcessedEvent(ctx, sqlcgen.ClaimProcessedEventParams{
		EventID:   eventID,
		EventType: eventType,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("notification: claim %s: %w", eventID, err)
	}
	return true, nil
}
