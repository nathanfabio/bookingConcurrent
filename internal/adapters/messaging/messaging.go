// Package messaging is the RabbitMQ adapter (CLAUDE.md §6): the publisher
// behind the outbox relay's EventPublisher port and the consumer that feeds
// broker deliveries to the notification handler.
//
// This is the ONLY package that imports the AMQP client — .golangci.yml's
// depguard rule denies github.com/rabbitmq in domain and application code,
// so the broker's vocabulary (channels, confirms, acks, topology) never
// leaks past this boundary. The application layer sees two ports
// (application/outbox.EventPublisher) and one plain func signature
// (the consumer's handler callback).
//
// Topology ownership is deliberately NOT the textbook "consumers declare
// their own queues, publishers just publish". With a topic exchange, a
// publish to a routing key with no bound queue is silently discarded — so
// if the relay ever published before some consumer first declared the queue,
// the event would vanish WHILE the outbox row was marked published:
// permanent loss, contradicting the outbox's entire purpose (ADR 0009).
// Instead BOTH sides declare the FULL topology (exchange + queue + binding),
// idempotently, on every (re)connect, and the worker declares once at boot.
// Whoever starts first makes the other's declares no-ops.
package messaging

import (
	"errors"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// tracerName is the instrumentation scope every span in this package
// carries; the global tracer provider comes from platform/telemetry.
const tracerName = "internal/adapters/messaging"

// errSanitizedURL replaces URL-parse failures, whose messages echo the
// input string — and the broker URL carries credentials (CLAUDE.md §10).
// Constructors validate the URL with it so every LATER (runtime) dial error
// is network-level, contains no URL, and can be logged verbatim.
var errSanitizedURL = errors.New("messaging: RABBITMQ_URL is malformed (details suppressed — the URL carries credentials)")

// amqpTableCarrier adapts amqp.Table to the otel TextMapCarrier interface so
// the configured propagator (TraceContext, set up in platform/telemetry)
// can ride through the broker in message headers. This is what joins the
// relay's publish span and the consumer's process span into ONE trace —
// CLAUDE.md §8's "trace the message publish/consume path".
//
// Values round-trip as strings; the propagator only ever sets/gets strings,
// so the lossy amqp.Table (any-typed) is fine.
type amqpTableCarrier amqp.Table

func (c amqpTableCarrier) Get(key string) string {
	v, _ := c[key].(string)
	return v
}

func (c amqpTableCarrier) Set(key, value string) { c[key] = value }

func (c amqpTableCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// Topology names the broker objects this system uses. Values come from
// validated config (RABBITMQ_EXCHANGE / RABBITMQ_QUEUE) — never hardcoded
// here (CLAUDE.md §10); the defaults live in platform/config.
//
// BindingKey is the routing key the queue subscribes to. M6 binds the
// literal event type "BookingConfirmed" (ADR 0009's identity mapping: the
// relay publishes with routing key = event_type, so a type→key lookup table
// that could drift does not exist). A topic exchange is used anyway so a
// future consumer can bind wildcards (e.g. "Booking.*") without migrating
// the topology.
type Topology struct {
	Exchange   string
	Queue      string
	BindingKey string
}

// DeclareTopology idempotently creates the exchange, queue, and binding.
// RabbitMQ re-declares with IDENTICAL parameters are no-ops; a mismatched
// re-declare (someone renamed durability in config against a live broker)
// fails the channel — loudly, which is the correct outcome.
//
// Durability choices (ADR 0009):
//   - exchange durable, topic, not auto-delete: it outlives restarts and
//     routes by event type;
//   - queue durable, not exclusive, not auto-delete: events published while
//     the worker is down must survive — the outbox stops republishing once
//     rows are marked, so the broker is the ONLY copy at that point;
//   - messages persistent (publisher sets DeliveryMode): survives broker
//     restarts too.
func DeclareTopology(ch *amqp.Channel, t Topology) error {
	if err := ch.ExchangeDeclare(
		t.Exchange,
		amqp.ExchangeTopic,
		true,  // durable
		false, // auto-delete
		false, // internal
		false, // no-wait
		nil,
	); err != nil {
		return fmt.Errorf("messaging: declare exchange %q: %w", t.Exchange, err)
	}
	if _, err := ch.QueueDeclare(
		t.Queue,
		true,  // durable
		false, // auto-delete
		false, // exclusive
		false, // no-wait
		nil,
	); err != nil {
		return fmt.Errorf("messaging: declare queue %q: %w", t.Queue, err)
	}
	if err := ch.QueueBind(t.Queue, t.BindingKey, t.Exchange, false, nil); err != nil {
		return fmt.Errorf("messaging: bind %q to %q on %q: %w", t.Queue, t.BindingKey, t.Exchange, err)
	}
	return nil
}
