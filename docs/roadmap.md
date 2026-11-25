# Study roadmap

The completed baseline is M6b (commit f05a2e3): atomic Redis holds,
Postgres persistence, authentication, booking, sandbox payments, transactional
outbox, RabbitMQ consumer, and hold-expiry sweeper. CLAUDE.md remains the
project specification. Each next slice should be explained, tested, and
reviewed before beginning another.

## Current slice: durable confirmation retries

Recover a committed booking before reading Redis. Preserve ownership checks,
the live-hold/payment gate for first confirmation, and unique-index arbitration.
Cover cleanup, expiry, Redis-independent replay, concurrent cleanup, HTTP
responses, and durable-store failures. See ADR 0006.

## Proposed next slices

1. **HTTP protection:** configurable CORS and bounded rate limiting, with
   stricter auth limits. Define trusted-proxy behavior before using forwarded
   client IPs. Document single-process limiter semantics and memory bounds.
2. **Observability:** HTTP spans, request-correlated application logs, OTLP
   export and Jaeger in Compose; Prometheus request and business metrics.
   Use route templates for labels to avoid unbounded metric cardinality.
3. **End-to-end verification:** drive registration, login, hold, sandbox
   capture, confirmation and replay through HTTP. Require infrastructure in
   CI so unavailable services fail instead of silently skipping integration
   tests. Keep optional local integration execution convenient.
4. **Packaging and operations:** multi-stage Dockerfiles for API and worker,
   optional Compose app services, explicit deployment migrations, and a
   runbook for startup, readiness, shutdown and failure recovery.
5. **Payment and notification reliability:** make captured-but-unbooked
   payments visible and define compensation before adding real money.
   Replace claim-before-notify only when introducing a real notifier, using
   a documented delivery policy and provider idempotency where available.

Keep the modular monolith, visible SQL, standard-library HTTP and sandbox
gateway. Add a real payment provider only when it teaches a specific lesson
that the fake cannot cover. Update ADRs when changing established behavior.
