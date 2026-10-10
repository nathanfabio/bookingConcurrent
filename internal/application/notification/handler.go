package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// Handler is the BookingConfirmed consumer use case. It depends only on
// ports (ProcessedEvents, Notifier), so unit tests run against the
// in-memory fakes and production runs against Postgres + whatever notifier
// the composition root selects.
type Handler struct {
	processed ProcessedEvents
	notifier  Notifier
}

// NewHandler wires the consumer.
func NewHandler(processed ProcessedEvents, notifier Notifier) *Handler {
	return &Handler{processed: processed, notifier: notifier}
}

// HandleBookingConfirmed processes ONE broker delivery. It takes primitives
// (not an outbox.Event or a broker delivery type) on purpose: this package
// must not import application/outbox or the messaging adapter — the worker
// adapts with a closure, and the dependency graph stays acyclic and
// consumer-owned (the PaymentFinder precedent in application/booking).
//
// The return value is the broker's ack/nack signal: nil = ack (the delivery
// is done with, permanently), error = nack+requeue (something transient
// failed; redeliver and try again). That mapping drives the error taxonomy:
//
//   - DETERMINISTIC GARBAGE (unparseable event id, unparseable payload)
//     returns nil after logging loudly. Redelivery cannot fix malformed
//     input, so nack+requeue would spin forever — the same ack-don't-retry
//     policy the payment webhook uses for unverifiable bodies (ADR 0008).
//   - DUPLICATE deliveries (claim lost) return nil: this is at-least-once
//     working as designed, logged at debug, not an error.
//   - TRANSIENT failures (claim store down, notifier failure) return the
//     error so the broker redelivers.
//
// IDEMPOTENCY ORDER — claim BEFORE notify, and its sharp edge: if Notify
// fails (or the process crashes) AFTER a successful claim, the redelivery
// finds the claim row and skips, so the notification is LOST — the side
// effect is at-most-once even though delivery is at-least-once. For the M6
// stub (a log line) that is the correct trade: a lost log line is trivia,
// and the alternative (notify-then-claim) would double-notify on every
// redelivery race. A real notifier (email!) must either be idempotent
// itself, carry provider-side dedupe keys, or deliberately invert the order
// and accept bounded duplicates. handler_test.go pins this behavior so
// inverting it later is a conscious act, not a silent regression. (ADR 0009.)
func (h *Handler) HandleBookingConfirmed(ctx context.Context, eventID string, payload []byte) error {
	// The claim table's event_id column is UUID-typed: a non-UUID id would
	// fail at the database, and no amount of redelivery fixes that. Validate
	// here so garbage is acked loudly instead of hot-looping the store.
	if _, err := uuid.Parse(eventID); err != nil {
		slog.ErrorContext(ctx, "discarding delivery with malformed event id",
			slog.String("event_id", eventID))
		return nil
	}

	var evt BookingConfirmed
	if err := json.Unmarshal(payload, &evt); err != nil {
		// Decode BEFORE claim: garbage must not burn a claim row, or a
		// corrected replay of the same event id (if that were ever possible)
		// would be skipped as a "duplicate".
		slog.ErrorContext(ctx, "discarding BookingConfirmed delivery with malformed payload",
			slog.String("event_id", eventID),
			slog.String("error", err.Error()))
		return nil
	}

	claimed, err := h.processed.Claim(ctx, eventID, EventTypeBookingConfirmed)
	if err != nil {
		return fmt.Errorf("notification: claim %s: %w", eventID, err)
	}
	if !claimed {
		slog.DebugContext(ctx, "duplicate delivery skipped",
			slog.String("event_id", eventID),
			slog.String("event_type", EventTypeBookingConfirmed))
		return nil
	}

	if err := h.notifier.NotifyBookingConfirmed(ctx, evt); err != nil {
		// Claimed but not notified — see the IDEMPOTENCY ORDER doc: this
		// notification will NOT be retried on redelivery. Return the error
		// anyway: the nack is honest bookkeeping (broker redelivery stats,
		// logs), and the duplicate it causes is skipped by the claim.
		return fmt.Errorf("notification: notify %s: %w", eventID, err)
	}
	return nil
}
