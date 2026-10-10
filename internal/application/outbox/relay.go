package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Relay publishes pending outbox events to the broker. It is the M6 half of
// the outbox pattern (ADR 0006 wrote the rows; this moves them).
//
// Delivery semantics (ADR 0009): PUBLISH-THEN-MARK, at-least-once.
//
//	A crash (or mark failure) between the broker's confirm and the outbox
//	stamp leaves the row pending, so the next tick republishes it — a
//	duplicate, which consumers dedupe on event_id. The reverse order would
//	lose events on a crash between mark and publish, and a lost sale event
//	is unrecoverable while a duplicate is routine. When in doubt, duplicate.
//
// Single-instance, deliberately (the same posture ADR 0002 takes for Redis):
// two relays polling the same table would each publish the same pending rows.
// The dedupe contract makes that safe-but-wasteful; the documented scale-out
// path is SELECT ... FOR UPDATE SKIP LOCKED in ListPending, which partitions
// the pending set across relays. Not built now — one worker is the deployment
// shape (CLAUDE.md §13: no gold-plating).
//
// The relay is sequential within a batch and awaits each publisher confirm.
// Booking-confirmation volume is a handful of events per second at most;
// per-message confirms are the simplest design that never marks an
// unconfirmed publish, and batching confirms is an ADR 0009 deferral.
type Relay struct {
	store     OutboxStore
	publisher EventPublisher
	batchSize int
}

// NewRelay wires the relay. batchSize comes from validated config
// (WORKER_RELAY_BATCH_SIZE) and bounds the work of one RunOnce call.
func NewRelay(store OutboxStore, publisher EventPublisher, batchSize int) *Relay {
	return &Relay{store: store, publisher: publisher, batchSize: batchSize}
}

// RunOnce performs ONE relay pass: list a pending batch, publish it in FIFO
// order, mark what the broker confirmed. It returns the number of events
// durably published and marked.
//
// Partial-failure behavior is the load-bearing detail: publishing stops at
// the first error (the broker is likely down or the channel broke — hammering
// the rest of the batch would just generate more errors), but the CONFIRMED
// PREFIX is still marked. Those events are durably with the broker; leaving
// them pending would republish all of them needlessly. The remaining rows
// stay pending and retry on the next tick.
//
// RunOnce never loops on its own — the worker owns the ticker (platform/
// worker.Tick) so this stays a pure, testable single step and application
// code never grows a timer.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	pending, err := r.store.ListPending(ctx, r.batchSize)
	if err != nil {
		return 0, fmt.Errorf("outbox: relay: list pending: %w", err)
	}
	if len(pending) == 0 {
		return 0, nil
	}

	var (
		confirmed  []string
		publishErr error
	)
	for _, evt := range pending {
		if err := r.publisher.Publish(ctx, evt); err != nil {
			publishErr = fmt.Errorf("outbox: relay: publish %s (%s): %w", evt.ID, evt.Type, err)
			break
		}
		confirmed = append(confirmed, evt.ID)
	}

	// Mark the confirmed prefix even when publishing failed midway — the
	// prefix is durable at the broker, and marking is what stops the next
	// tick from republishing it. If nothing was confirmed there is nothing
	// to mark; skip the UPDATE entirely.
	if len(confirmed) > 0 {
		if err := r.store.MarkPublished(ctx, confirmed); err != nil {
			// Safe direction (see OutboxStore.MarkPublished): these events
			// republish next tick and consumers dedupe. Join with any publish
			// error so the caller sees both.
			markErr := fmt.Errorf("outbox: relay: mark %d published: %w", len(confirmed), err)
			publishErr = errors.Join(publishErr, markErr)
			// The mark failed, so nothing is BOTH published and stamped:
			// report 0 to keep the count meaning "rows the relay completed".
			return 0, publishErr
		}
	}

	if publishErr != nil {
		slog.WarnContext(ctx, "outbox relay pass incomplete; remaining rows retry next tick",
			slog.Int("published", len(confirmed)),
			slog.Int("batch", len(pending)))
		return len(confirmed), publishErr
	}
	return len(confirmed), nil
}
