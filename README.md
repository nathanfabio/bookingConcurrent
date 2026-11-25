# bookingConcurrent
A software of simultaneous reservations

This software is designed to handle multiple reservations at the same time, ensuring that users can book their desired slots without conflicts. It is ideal for businesses that require efficient management of appointments, events, or services.

## It was created for a study and I'm still working on it.

Two binaries, one hexagonal codebase: domain/application own the contracts,
adapters implement them, and `cmd/` wires dependencies (see `docs/adr/`).

- `cmd/api` — HTTP service: auth, catalog, seat map, holds, payments.
- `cmd/worker` — background jobs: the transactional-outbox relay, the
  `BookingConfirmed` consumer (ADR 0009), and the hold-expiry sweeper
  (ADR 0010).

### Running locally

```bash
cp .env.example .env      # then edit (secrets are generated, never committed)
make up                   # Redis + Postgres + RabbitMQ + Jaeger + UIs
make migrate              # explicit migration (dev also auto-migrates at boot)
make run                  # the API on :8080
make run-worker           # the background worker (separate terminal)
make test-race            # unit + integration tests, -race (infra optional:
                          # integration tests skip when it is unreachable)
```

### Event pipeline (M6, ADR 0009)

A confirmed booking and its event commit in ONE Postgres transaction; the
worker's relay publishes it to RabbitMQ only after the broker confirms, and
the consumer dedupes on the event id — at-least-once delivery, exactly-once
effect:

```
POST /holds/{id}/confirm
   └─ tx: confirmed_bookings + outbox row ──┐ (ADR 0006 commit point)
                                            ▼
                       cmd/worker: relay (poll, publish, confirm, mark)
                                            ▼
                    exchange booking.events ──► queue booking.notifications
                                            ▼
                       consumer: processed_events claim ──► notify (log stub)
```

Environment reference: `.env.example` (every variable is validated at boot —
the process refuses to start half-configured). API contract: `docs/api.md`.
Decisions and their reasoning: `docs/adr/`.

Continuation plan and reviewable study slices: [docs/roadmap.md](docs/roadmap.md).

### HTTP protection

The API applies CORS before local per-IP rate limiting, then authenticates
protected routes. Set `CORS_ALLOWED_ORIGINS` in `.env` for browser clients
(exact origins, no trailing slash). Empty rejects all requests carrying
Origin; CLI/webhook requests without Origin continue normally.

General, auth and hold token buckets have configurable refill/burst settings
and a bounded IP table. Throttled requests return JSON `429 rate_limited`
with `Retry-After`. Limits apply per API process to the connection IP;
forwarded IP headers are ignored. Behind a proxy, enforce per-client quotas
at that trusted edge. See [ADR 0011](docs/adr/0011-http-protection.md) for
the trade-offs and [the API contract](docs/api.md) for defaults.

### Observability

`make up` starts Jaeger at [localhost:16686](http://localhost:16686). The API
and worker export OTLP/HTTP traces by default, with distinct service names.
An existing `.env` that still has the old port **4317** must change to
`OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318` (the HTTP receiver).
Set `OTEL_TRACES_EXPORTER=stdout` for newline-delimited JSON spans instead,
or `OTEL_ENABLED=false` to disable tracing while keeping logs/metrics.

With the API and infrastructure running:

```bash
curl -s -H 'X-Request-ID: study-trace-1' localhost:8080/movies
curl -s localhost:8080/metrics
```

Look for service `booking-api` in Jaeger and `study-trace-1` in the logs.
HTTP spans contain Redis/Postgres children; worker publish/consume traces
propagate through RabbitMQ. The HTTP trace ends at the outbox commit:
trace context is not yet persisted in outbox metadata, so relay ticks start
separate traces. Request/application logs include request ID and valid
trace/span IDs; authenticated request logs also carry user ID.

`/metrics` exposes Prometheus request counts/duration, new booking commits,
durable captures, gateway capture outcomes and active holds. Routes are
templates, not path IDs. The active-holds gauge reads future Redis expiry
scores at scrape time; a failed read omits that gauge and exports
`booking_active_holds_collection_success 0`. It is shared state: do not sum
it across API replicas. Counters reset on restart and are operational
signals, not the financial ledger. No Prometheus server is bundled yet.

Jaeger stores dev traces in memory; restarting it loses them. Trace export
is bounded and best-effort, and Jaeger is not an API readiness dependency.
The worker emits traces/logs but has no metrics listener in this slice.
See [ADR 0012](docs/adr/0012-observability.md) for sampling, metric semantics,
privacy choices and known boundaries.
