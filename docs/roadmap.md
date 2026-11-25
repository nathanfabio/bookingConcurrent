# Study roadmap

The completed baseline is M6b (commit f05a2e3): atomic Redis holds,
Postgres persistence, authentication, booking, sandbox payments, transactional
outbox, RabbitMQ consumer, and hold-expiry sweeper. CLAUDE.md remains the
project specification. Each next slice should be explained, tested, and
reviewed before beginning another.

## Completed continuation slices

**Durable confirmation retries** (commit 1c029de):
recover a committed booking before reading Redis. Preserve ownership checks,
the live-hold/payment gate for first confirmation, and unique-index arbitration.
Cover cleanup, expiry, Redis-independent replay, concurrent cleanup, HTTP
responses, and durable-store failures. See ADR 0006.

**HTTP protection**: exact-origin CORS, bounded per-connection-IP token buckets
with general/auth/hold quotas, structured rejections and deterministic/race
tests. Proxy headers are deliberately ignored; edge limits are required for
per-client enforcement behind a proxy. See ADR 0011 and `.env.example`.

**Observability**: HTTP/Redis/Postgres/worker traces, OTLP/HTTP export and
Jaeger in Compose; correlated slog records; a private Prometheus registry
with HTTP, durable booking/capture and active-hold signals. Tests cover a
local OTLP receiver, sampling, export/scrape failures and concurrent counters.
See ADR 0012 for semantics and the outbox trace-context boundary.

**End-to-end verification**: `make test-e2e` runs a two-user HTTP checkout
against the API and worker with disposable infrastructure. It verifies the
ownership and payment gates, signed capture, confirmation replay, public
seat state and the worker notification. CI requires reachable services for
integration tests and runs the end-to-end journey separately.

## Proposed next slices

1. **Packaging and operations:** multi-stage Dockerfiles for API and worker,
   optional Compose app services, explicit deployment migrations, and a
   runbook for startup, readiness, shutdown and failure recovery.
2. **Payment and notification reliability:** make captured-but-unbooked
   payments visible and define compensation before adding real money.
   Replace claim-before-notify only when introducing a real notifier, using
   a documented delivery policy and provider idempotency where available.

Keep the modular monolith, visible SQL, standard-library HTTP and sandbox
gateway. Add a real payment provider only when it teaches a specific lesson
that the fake cannot cover. Update ADRs when changing established behavior.
