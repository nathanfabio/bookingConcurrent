// Package outbox contains the relay use case and the ports it depends on.
// Per ADR 0001, ports live here — next to the code that calls them — not in
// a central ports package and not next to the adapters that implement them.
//
// The outbox pattern (CLAUDE.md §6, ADR 0006, ADR 0009): the API's confirm
// flow writes the BookingConfirmed event into the outbox table in the SAME
// Postgres transaction as the booking row, so "sale recorded" and "event
// queued" can never diverge. THIS package is the other half — the relay that
// lifts queued events out of Postgres and hands them to the broker.
//
// The relay is deliberately payload-agnostic: it moves opaque envelopes
// (id, type, JSON bytes) and never decodes them. Decoding belongs to
// consumers (application/notification), which keeps this package decoupled
// from every event schema the system will ever grow.
package outbox

import "context"

// Event is the transport envelope the relay moves from the outbox table to
// the broker. ID is the outbox row's event_id (a UUID minted by the writing
// transaction) and travels as the AMQP MessageId — it is the consumers'
// idempotency key under at-least-once delivery. Type is the event_type
// column ("BookingConfirmed") and doubles as the routing key (ADR 0009's
// identity mapping: a type→key lookup table could drift; identity cannot).
// Payload is the raw JSONB bytes; the relay never parses them.
type Event struct {
	ID      string
	Type    string
	Payload []byte
}

// EventPublisher is the port to the message broker (CLAUDE.md §6). The
// RabbitMQ adapter implements it with publisher confirms; the in-memory fake
// records publishes for unit tests.
type EventPublisher interface {
	// Publish hands the event to the broker. The contract is durability,
	// not fire-and-forget: nil means the broker has ACCEPTED RESPONSIBILITY
	// for the message (for RabbitMQ: the publisher confirm arrived after a
	// persistent publish to a durable exchange with a durable binding).
	// An error means the event was NOT durably published and the outbox row
	// must stay pending for the next relay tick.
	//
	// Implementations may cause DUPLICATE deliveries (a confirm that timed
	// out client-side may still have landed broker-side); exactly-once
	// EFFECT is the consumer's job (dedupe on Event.ID).
	Publish(ctx context.Context, evt Event) error
}

// OutboxStore is the port over the outbox table (migrations/00004). The
// Postgres adapter implements it; the in-memory fake backs unit tests.
type OutboxStore interface {
	// ListPending returns up to limit unpublished events (published_at IS
	// NULL), oldest first — FIFO by (created_at, id) so one transaction's
	// events keep their commit order. The store's BIGSERIAL primary key
	// deliberately does not escape: everything downstream is keyed by the
	// event_id UUID.
	ListPending(ctx context.Context, limit int) ([]Event, error)

	// MarkPublished stamps published_at on the given event ids, completing
	// the relay cycle. It must tolerate ids that are already published
	// (a replayed mark is a no-op, not an error) because at-least-once
	// republishing is a normal path, not a bug.
	//
	// Failing to mark after a successful publish is SAFE — the next tick
	// republishes and consumers dedupe. The reverse order (mark, then
	// publish) would LOSE events on a crash in between, which is why the
	// relay is publish-then-mark (ADR 0009).
	MarkPublished(ctx context.Context, eventIDs []string) error
}
