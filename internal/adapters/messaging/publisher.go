package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	appoutbox "github.com/nathanfabio/bookingConcurrent/internal/application/outbox"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Compile-time proof the adapter satisfies the port.
var _ appoutbox.EventPublisher = (*Publisher)(nil)

// Publisher publishes outbox events to RabbitMQ with PUBLISHER CONFIRMS:
// Publish returns nil only after the broker has acknowledged durable
// responsibility for the message (the outbox.EventPublisher contract). An
// unconfirmed publish — dial failure, nack, timed-out confirm — returns an
// error so the relay leaves the row pending and retries next tick.
//
// RECONNECTION is lazy and lives inside Publish: an amqp091 channel never
// self-heals after a broker restart or network blip, so every publish first
// checks the channel and re-dials (+ re-enables confirms + re-declares the
// topology, which a fresh broker may have lost) when it is gone. Without
// this the relay would go permanently deaf after one blip.
//
// Concurrency: one mutex serializes dial and publish. The relay publishes
// sequentially by design (ADR 0009), so there is no throughput cost, and
// holding the lock across the confirm wait keeps a reconnect from swapping
// the channel underneath an in-flight publish.
type Publisher struct {
	url   string
	appID string
	top   Topology

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

// NewPublisher builds the publisher from validated config. It performs NO
// network I/O — dialing is the worker's boot-retry loop's job (EnsureReady)
// and, after that, lazy reconnection's. A constructor that fails when the
// broker is briefly down would turn every worker start into a crash-loop,
// exactly what the outbox exists to prevent.
//
// It DOES reject a structurally invalid broker URL (errSanitizedURL): that
// is a configuration error which no amount of retrying fixes, and catching
// it here means every later dial error is network-level and safe to log
// verbatim (see dialContext).
func NewPublisher(url string, top Topology, appID string) (*Publisher, error) {
	if _, err := amqp.ParseURI(url); err != nil {
		return nil, errSanitizedURL
	}
	return &Publisher{url: url, appID: appID, top: top}, nil
}

// EnsureReady establishes the connection, channel, confirm mode, and
// topology, or verifies the existing ones are live. The worker calls it at
// boot inside a retry loop (broker-down must not fail-fast the worker —
// ADR 0009's boot-policy asymmetry) and Publish calls it lazily thereafter.
func (p *Publisher) EnsureReady(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensureLocked(ctx)
}

// Publish implements outbox.EventPublisher: persistent delivery to the
// durable exchange, routing key = event type (identity mapping, ADR 0009),
// MessageId = the outbox event_id (the consumers' dedupe key), and a
// blocking wait for the broker's confirm.
//
// The active trace context is injected into the message headers so the
// consumer's span joins this publish's trace (see amqpTableCarrier).
func (p *Publisher) Publish(ctx context.Context, evt appoutbox.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	ctx, span := otel.Tracer(tracerName).Start(ctx, "publish "+evt.Type,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.operation.name", "publish"),
			attribute.String("messaging.destination.name", p.top.Exchange),
			attribute.String("messaging.message.id", evt.ID),
		))
	defer span.End()

	if err := p.ensureLocked(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "connect")
		return err
	}

	headers := amqp.Table{}
	otel.GetTextMapPropagator().Inject(ctx, amqpTableCarrier(headers))

	// mandatory=false: the topology is declared on this very channel before
	// any publish, so the binding always exists; a basic.return path would
	// add handling for a state that cannot occur without a config change —
	// and a config change against a live broker fails the (re)declare above.
	dc, err := p.ch.PublishWithDeferredConfirmWithContext(
		ctx, p.top.Exchange, evt.Type, false, false,
		amqp.Publishing{
			Headers:      headers,
			MessageId:    evt.ID,
			Type:         evt.Type,
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Timestamp:    time.Now(),
			AppId:        p.appID,
			Body:         evt.Payload,
		})
	if err != nil {
		err = fmt.Errorf("messaging: publish %s: %w", evt.ID, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish")
		return err
	}

	confirmed, err := dc.WaitContext(ctx)
	if err != nil {
		// Ctx canceled mid-wait (shutdown) or the channel died before the
		// confirm arrived. The publish MAY still have landed broker-side —
		// unknowable from here. That is the safe direction: the row stays
		// pending, the next tick republishes, and the consumer dedupes.
		err = fmt.Errorf("messaging: await confirm for %s: %w", evt.ID, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "confirm")
		return err
	}
	if !confirmed {
		err := fmt.Errorf("messaging: broker nacked %s (refused durable responsibility)", evt.ID)
		span.RecordError(err)
		span.SetStatus(codes.Error, "nack")
		return err
	}
	return nil
}

// Close tears down the channel and connection. Safe to call on a publisher
// that never dialed.
func (p *Publisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var errs []error
	if p.ch != nil {
		errs = append(errs, p.ch.Close())
		p.ch = nil
	}
	if p.conn != nil {
		errs = append(errs, p.conn.Close())
		p.conn = nil
	}
	return errors.Join(errs...)
}

// ensureLocked (re)establishes conn → channel → confirm mode → topology.
// Caller holds p.mu. Idempotent: a live channel short-circuits.
func (p *Publisher) ensureLocked(ctx context.Context) error {
	if p.ch != nil && !p.ch.IsClosed() {
		return nil
	}
	// Tear down the corpse of the previous connection so a half-dead
	// conn never outlives its channel.
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn, p.ch = nil, nil
	}

	conn, err := dialContext(ctx, p.url)
	if err != nil {
		return fmt.Errorf("messaging: publisher dial: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("messaging: publisher channel: %w", err)
	}
	// Confirm mode BEFORE the first publish: it is what upgrades Publish
	// from fire-and-forget to "nil means durable".
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return fmt.Errorf("messaging: enable publisher confirms: %w", err)
	}
	if err := DeclareTopology(ch, p.top); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return err
	}
	slog.InfoContext(ctx, "publisher connected to broker",
		slog.String("exchange", p.top.Exchange))
	p.conn, p.ch = conn, ch
	return nil
}

// dialContext makes amqp.Dial interruptible: the library's dial has its own
// 30s network timeout, and a worker shutting down during a broker outage
// should not have to wait it out. On ctx cancellation the dialing goroutine
// is left to finish into a buffered channel (it cannot be canceled mid-TCP)
// and its result is discarded — acceptable because the only caller context
// that cancels is process shutdown.
//
// Credential-safety note: runtime dial errors (DNS, refused, auth, timeout)
// never echo the URL — only amqp.ParseURI failures can, via url.Parse — and
// the constructors already rejected unparseable URLs with a sanitized error.
// So wrapping the library error verbatim here is safe AND keeps operator
// logs debuggable (refused vs auth vs DNS are different outages).
func dialContext(ctx context.Context, url string) (*amqp.Connection, error) {
	type dialResult struct {
		conn *amqp.Connection
		err  error
	}
	done := make(chan dialResult, 1)
	go func() {
		conn, err := amqp.Dial(url)
		done <- dialResult{conn, err}
	}()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("messaging: dial canceled: %w", ctx.Err())
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("messaging: dial: %w", r.err)
		}
		return r.conn, nil
	}
}
