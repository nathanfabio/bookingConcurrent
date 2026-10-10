package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Delivery is the adapter-owned view of one broker message: exactly the
// three things the notification handler needs. The AMQP type never crosses
// this boundary (depguard denies it in application code, and the handler
// signature takes primitives — see application/notification).
type Delivery struct {
	// EventID is the publisher's MessageId: the outbox event_id UUID and
	// the consumer's dedupe key. Empty means a foreign producer bypassed the
	// relay — such deliveries are discarded (see Consume).
	EventID   string
	EventType string
	Body      []byte
}

// redeliverDelay is the anti-spin pause after a nack+requeue. Without it, a
// handler failing on an infrastructure outage (Postgres down ⇒ every claim
// errors) would redeliver instantly in a hot loop, burning CPU and flooding
// logs; the delay degrades that to slow polling. Small enough that genuine
// transient blips recover almost immediately.
const redeliverDelay = 500 * time.Millisecond

// Consumer subscribes to the notification queue and feeds each delivery to
// a handler. It owns the ack/nack policy (ADR 0009):
//
//   - handler returns nil  → Ack (done, permanently);
//   - handler returns err  → Nack(requeue) + anti-spin delay: the failure is
//     treated as transient and the broker redelivers — safe because the
//     handler's claim table makes redelivery idempotent;
//   - delivery without a MessageId → Ack after logging loudly: there is no
//     dedupe key, so it can never be processed safely, and requeueing would
//     spin forever. A dead-letter queue would be the production upgrade
//     (deferred, CLAUDE.md §13);
//   - ctx cancellation → clean shutdown: cancel the subscription and return
//     nil. In-flight unacked deliveries are requeued by the broker when the
//     channel closes — dedupe makes that safe.
//
// Consume DIALS on every call and returns when the connection dies or ctx
// is canceled; the worker's consumer job owns the retry loop with capped
// backoff. One call = one connection lifetime keeps reconnection logic out
// of this type entirely.
type Consumer struct {
	url      string
	top      Topology
	prefetch int
}

// NewConsumer builds the consumer from validated config. No network I/O;
// the URL is validated like the publisher's (see errSanitizedURL).
func NewConsumer(url string, top Topology, prefetch int) (*Consumer, error) {
	if _, err := amqp.ParseURI(url); err != nil {
		return nil, errSanitizedURL
	}
	return &Consumer{url: url, top: top, prefetch: prefetch}, nil
}

// Consume blocks until ctx is canceled (returns nil) or the broker
// connection fails (returns an error for the worker to retry).
func (c *Consumer) Consume(ctx context.Context, handler func(context.Context, Delivery) error) error {
	conn, err := dialContext(ctx, c.url)
	if err != nil {
		return fmt.Errorf("messaging: consumer dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("messaging: consumer channel: %w", err)
	}
	defer func() { _ = ch.Close() }()

	// Prefetch bounds unacked in-flight deliveries: with requeue-on-failure
	// semantics, an unbounded prefetch would let one consumer hoard the
	// queue during an outage.
	if err := ch.Qos(c.prefetch, 0, false); err != nil {
		return fmt.Errorf("messaging: set prefetch: %w", err)
	}
	if err := DeclareTopology(ch, c.top); err != nil {
		return err
	}

	tag := "consumer-" + uuid.NewString()[:8]
	deliveries, err := ch.Consume(c.top.Queue, tag,
		false, // autoAck: NEVER — the ack is the handler's success signal
		false, // exclusive
		false, // no-local
		false, // no-wait
		nil)
	if err != nil {
		return fmt.Errorf("messaging: subscribe to %q: %w", c.top.Queue, err)
	}
	connClosed := make(chan *amqp.Error, 1)
	conn.NotifyClose(connClosed)

	slog.InfoContext(ctx, "consumer subscribed",
		slog.String("queue", c.top.Queue),
		slog.String("exchange", c.top.Exchange),
		slog.Int("prefetch", c.prefetch))

	for {
		select {
		case <-ctx.Done():
			// Clean shutdown: stop the subscription so the broker requeues
			// anything in flight, then let the deferred closes run.
			if err := ch.Cancel(tag, false); err != nil {
				slog.WarnContext(ctx, "consumer cancel on shutdown failed",
					slog.String("error", err.Error()))
			}
			return nil
		case amqpErr, ok := <-connClosed:
			if !ok || amqpErr == nil {
				return errors.New("messaging: consumer connection closed")
			}
			return fmt.Errorf("messaging: consumer connection lost: %w", amqpErr)
		case d, ok := <-deliveries:
			if !ok {
				// The library closes this channel when the channel dies;
				// treat it like a connection loss and let the worker redial.
				return errors.New("messaging: delivery channel closed")
			}
			c.handleOne(ctx, handler, d)
		}
	}
}

// handleOne applies the ack/nack policy to a single delivery. Errors from
// Ack/Nack themselves are logged, not returned: a failed ack means the
// broker redelivers, which the handler's dedupe absorbs — there is no
// better recovery available from here.
//
// The trace context injected by the publisher is extracted from the message
// headers FIRST, so this span (and everything the handler does under it)
// joins the relay's publish trace — one end-to-end trace across the broker.
func (c *Consumer) handleOne(ctx context.Context, handler func(context.Context, Delivery) error, d amqp.Delivery) {
	ctx, span := otel.Tracer(tracerName).Start(
		otel.GetTextMapPropagator().Extract(ctx, amqpTableCarrier(d.Headers)),
		"process "+d.Type,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.operation.name", "process"),
			attribute.String("messaging.destination.name", c.top.Queue),
			attribute.String("messaging.message.id", d.MessageId),
		))
	defer span.End()

	if d.MessageId == "" {
		// No dedupe key ⇒ cannot ever be processed safely (see the type
		// doc). Ack to discard; the loud log is the audit trail.
		slog.ErrorContext(ctx, "discarding delivery without a message id (no dedupe key)",
			slog.String("queue", c.top.Queue),
			slog.String("amqp_type", d.Type))
		span.SetStatus(codes.Error, "missing message id")
		c.ack(ctx, d, "discard")
		return
	}
	if handler == nil {
		c.ack(ctx, d, "no handler")
		return
	}

	if err := handler(ctx, Delivery{EventID: d.MessageId, EventType: d.Type, Body: d.Body}); err != nil {
		slog.ErrorContext(ctx, "delivery handler failed; nacking for redelivery",
			slog.String("event_id", d.MessageId),
			slog.String("error", err.Error()))
		span.RecordError(err)
		span.SetStatus(codes.Error, "handler")
		if nackErr := d.Nack(false, true); nackErr != nil {
			slog.WarnContext(ctx, "nack failed; broker will redeliver when the channel closes",
				slog.String("event_id", d.MessageId),
				slog.String("error", nackErr.Error()))
		}
		// Anti-spin: give the transient failure a moment before the broker
		// hands the message back. Ctx-aware so shutdown is never delayed.
		select {
		case <-time.After(redeliverDelay):
		case <-ctx.Done():
		}
		return
	}
	c.ack(ctx, d, "handled")
}

func (c *Consumer) ack(ctx context.Context, d amqp.Delivery, why string) {
	if err := d.Ack(false); err != nil {
		slog.WarnContext(ctx, "ack failed after "+why+"; broker will redeliver (dedupe makes this safe)",
			slog.String("event_id", d.MessageId),
			slog.String("error", err.Error()))
	}
}
