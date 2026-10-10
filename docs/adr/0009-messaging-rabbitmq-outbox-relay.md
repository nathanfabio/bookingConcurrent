# 9. Messaging: RabbitMQ, outbox relay semantics, idempotent consumer

Date: 2026-10-10
Status: Accepted

## Context

CLAUDE.md §6 requires an event-driven piece so the project teaches decoupling,
not just request/response: publish `BookingConfirmed` when a hold is
confirmed, run at least one consumer reacting to it, consider the outbox
pattern for the write→publish step, and keep the consumer idempotent because
at-least-once delivery is the realistic default.

The writing half has existed since M4: `BookingRepo.Confirm` inserts the
booking row and its `BookingConfirmed` outbox row in ONE Postgres transaction
(ADR 0006's commit point), and every confirm since then has been accumulating
rows with `published_at NULL` — the outbox buffering exactly as designed.
Migration 00004 pinned the shape: `event_id UUID` (the consumers' dedupe
key), `event_type`, JSONB payload, and a partial index over the pending set.
RabbitMQ has been sitting in compose.yaml since M0 ("not consumed by code
until M6"), and `RABBITMQ_URL` has been validated at boot since then.

This ADR is the one CLAUDE.md §1 explicitly demands ("choice of messaging
broker") plus the two decisions the outbox forces: what "published" means
(the relay's delivery semantics) and what "processed exactly once" means
(the consumer's idempotency).

## Decision

**1. RabbitMQ over Kafka.** Both satisfy §6; RabbitMQ wins on fit, not on
throughput. The workload is a trickle of sale events needing per-message
durability controls — publisher confirms ("the broker accepted THIS
message") and manual acks ("the consumer finished THIS message") — which are
exactly RabbitMQ's native primitives. Kafka's partitioned log shines for
replay and high-volume streams; replay is precisely what the outbox table
already provides (it IS the durable, ordered, replayable log — Postgres is
better at it than a broker here), so Kafka's core advantage is redundant in
this architecture while its operational surface is larger. It is also
already in compose with a management UI for development (§11), and one more
reason that is easy to underrate: `amqp091-go` fits in
`internal/adapters/messaging` without pulling a heavyweight client into the
modular monolith (§13).

**2. Topology: one durable topic exchange, one durable queue per consuming
service, routing key = event type, identity.** The relay publishes with
routing key `"BookingConfirmed"` — the literal `event_type` value, no
type→key mapping table. A mapping is a second source of truth that can
drift, and drift fails in one of two nasty ways: fail-on-unknown stalls the
relay (a poison row blocks the batch head forever), map-to-default silently
misroutes. Identity cannot drift. The exchange is a topic anyway, so a
future consumer can bind `"Booking.*"` wildcards without re-plumbing.
Everything is durable and messages are persistent: events published while
the worker is down must survive, because once the relay marks a row
published, the broker holds the ONLY copy.

**3. Both sides declare the FULL topology on every (re)connect.** This
deviates from the textbook "consumers declare their queues, publishers just
publish", deliberately: with a topic exchange, a publish whose routing key
matches no binding is silently DISCARDED — and still publisher-confirmed.
If the relay ever ran before a consumer first declared `booking.notifications`,
events would vanish while their outbox rows were marked published: permanent
loss, contradicting the outbox's entire purpose. So `DeclareTopology`
(exchange + queue + binding, idempotent) runs at worker boot, inside every
publisher (re)dial, and inside every consumer (re)dial — whoever starts
first is right, and a parameter mismatch against a live broker fails the
declare loudly instead of forking the topology.

**4. Relay semantics: publish-then-mark, per-message confirms, at-least-once.**
One `RunOnce` pass: `ListPending` (FIFO by `(created_at, id)`, bounded by
`WORKER_RELAY_BATCH_SIZE`) → publish each event sequentially, awaiting its
publisher confirm → stop at the first failure → mark the CONFIRMED PREFIX
published → return. The ordering is the whole safety argument: marking
before publishing would LOSE events on a crash in between (unrecoverable),
while publishing before marking can only DUPLICATE them (routine, absorbed
by decision 5). Confirm-awaiting is what makes `published_at` mean "the
broker durably accepted this" rather than "we handed bytes to a socket".
Partial-failure marking matters too: when the broker dies mid-batch, the
confirmed prefix is durable and gets stamped, so the next tick republishes
only the remainder.

Single relay instance, deliberately — the same posture ADR 0002 takes for
Redis. Two relays polling one table would each publish every pending row;
dedupe makes that safe-but-wasteful. The documented scale-out path is
`SELECT ... FOR UPDATE SKIP LOCKED` in `ListPending` (partitioning the
pending set), not built now (§13). Polling on a ticker
(`WORKER_RELAY_POLL_INTERVAL`, default 5s) rather than LISTEN/NOTIFY:
stateless, restart-proof, and seconds of publish latency are invisible next
to a five-minute hold TTL.

**5. Consumer idempotency: claim-before-side-effect in Postgres.**
Migration 00010's `processed_events` table (event_id PK) is the arbiter, in
the same schema-as-arbiter posture as the booking seat index (ADR 0006) and
the payment capture guard (ADR 0008): `INSERT .. ON CONFLICT DO NOTHING
RETURNING` admits exactly one winner per event_id under any interleaving —
proven by `TestProcessedEventClaimConcurrentExactlyOneWinner` (32 racing
claims against real Postgres). The handler's order is decode → claim →
notify, and its ack taxonomy is: deterministic garbage (unparseable id or
payload) is logged loudly and ACKED (redelivery cannot fix malformed input;
the same ack-don't-retry policy the payment webhook uses, ADR 0008);
duplicates are debug-logged and acked; transient failures (claim store,
notifier) error → NACK+requeue behind a 500ms anti-spin delay, so a dead
database degrades to slow polling instead of a hot redelivery loop.

**The sharp edge, named:** claim-then-notify makes the SIDE EFFECT
at-most-once. If notify fails (or the process crashes) after the claim, the
redelivery finds the claim row and skips — that notification is gone. For
M6's notifier (a structured log line, `adapters/notification.LogNotifier`)
this is the correct trade: a lost log line is trivia, while the alternative
order (notify-then-claim) double-notifies on every redelivery race. A real
email/SMS sender must either be idempotent itself, carry provider-side
dedupe keys, or invert the order as a deliberate act —
`TestHandlerNotifyFailureLosesNotification` pins the current behavior so
that inversion cannot happen by accident.

**6. Worker supervision and the boot-policy asymmetry.** `platform/worker`
owns mechanics (jobs under one cancellable context, first failure cancels
siblings, clean cancel exits 0, `Tick` runs immediately then per interval);
`cmd/worker` owns policy. Postgres failure at boot is FATAL (without the
outbox and claim table the process has no purpose); broker failure is
RETRIABLE with capped backoff — the outbox exists precisely so broker
outages are survivable, and a worker that crash-looped on a broker blip
would teach the opposite lesson. Relay errors are logged and swallowed per
tick (rows stay pending); consumer connection loss redials with 1s→30s
backoff. Panics are not recovered: a panicking job is a bug, and crashing
hands the supervisor a full stack trace instead of a zombie.

**7. Trace context rides the messages.** The publisher injects the active
trace context into AMQP headers via the global propagator; the consumer
extracts it before starting its `process` span, so relay-publish and
consumer-process spans join one trace (CLAUDE.md §8's publish/consume
requirement). Known limitation, documented rather than solved: the relay's
trace does NOT join the originating HTTP confirm request's trace — that
link would need the traceparent stored as an outbox column, and the
correlation that matters operationally (event_id) is already on every span
and log line.

**8. The consumer is not a port.** The messaging `Consumer` has exactly one
implementation and one caller, so per CLAUDE.md §1 ("don't force an
interface just for symmetry") the worker wires it concretely; its handler
callback takes primitives, and `application/notification` never imports
`application/outbox` or the adapter — the composition root adapts with a
closure (the `PaymentFinder` precedent for keeping application packages
mutually independent). The FIVE ports that do exist (`EventPublisher`,
`OutboxStore`, `ProcessedEvents`, `Notifier`, and the sweeper's
`ExpiredHoldStore`) each have a memory fake exercised by unit tests (§9).

## Consequences

- A committed booking ALWAYS eventually emits its event: the transaction
  guarantees it is queued, confirms + publish-then-mark guarantee it is not
  marked until durable, and the queue's durability covers broker restarts.
  The failure modes that remain all produce duplicates, never losses.
- Duplicates are a normal occurrence (relay republish after a mark failure,
  broker redelivery after a consumer restart) — every consumer of every
  future event type inherits the obligation to dedupe on `event_id`. The
  claim table is generic; new consumers reuse it.
- `processed_events` and published `outbox` rows grow forever; pruning is an
  operational task, deferred (§13) and noted here so it is not forgotten.
- The API's `/readyz` deliberately does NOT check the broker (wired that way
  since M4): the outbox is the buffer that lets the API keep taking bookings
  while RabbitMQ is down. Symmetrically, worker liveness does not depend on
  the broker either.
- Broker credentials travel in `RABBITMQ_URL`, env-driven like everything
  else (§10); the adapter rejects a malformed URL at construction with a
  sanitized error, because URL-parse failures echo their input — and runtime
  network errors, which never contain the URL, are logged verbatim.
- Tests that prove this model: unit — `TestRelayMarkFailureRepublishesNextTick`
  (at-least-once), `TestRelayStopsAtFirstPublishFailureMarksConfirmedPrefix`,
  `TestConcurrentDuplicateDeliveriesNotifyOnce`,
  `TestHandlerNotifyFailureLosesNotification` (at-most-once edge);
  integration against real Postgres + RabbitMQ —
  `TestRelayPublishesPendingOutboxToBroker`,
  `TestRelayRepublishOnMarkFailureYieldsOneNotification` (the marquee:
  injected mark failure → duplicate delivery → exactly one notification and
  one claim row), `TestConsumerDeliversToHandlerAndAcks`,
  `TestProcessedEventClaimConcurrentExactlyOneWinner`.
