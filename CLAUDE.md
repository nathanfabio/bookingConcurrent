# CLAUDE.md

This file guides Claude Code when working in this repository. This is a production-grade concurrent cinema seat-booking system in Go, built from scratch, but deliberately designed around a small set of lessons drawn from studying a simpler reference implementation first: hexagonal architecture, Redis-based atomic locking (`SET NX` + TTL) for seat holds, and basic `net/http` handlers. This project keeps that core concurrency mechanism intact but builds a real system around it: persistence, auth, observability, messaging, payments, and testing.

**Primary goal of this repo: maximize learning value.** Every non-trivial decision should be traceable to a comment, ADR (Architecture Decision Record), or README section explaining *why*, not just *what*. Favor idiomatic, explicit Go over clever abstractions. When two approaches are both reasonable, pick the one that teaches more about production Go systems.

---

## 1. Architecture

Use **Hexagonal Architecture (Ports & Adapters)** from the start. Structure it like this:

```
cmd/
  api/                  → main.go, wiring only (composition root)
  worker/                → background consumer(s) for async jobs (hold-expiry sweeper, notifications)
internal/
  domain/                 → entities, value objects, domain errors, domain services (pure Go, zero infra imports)
    booking/
    movie/
    user/
    payment/
  application/            → use cases / services, orchestration, depends only on domain + ports
    booking/
    auth/
  ports/                   → interfaces the domain/application needs (BookingStore, UserStore, PaymentGateway, EventPublisher, ...)
  adapters/
    postgres/              → SQL persistence (users, movies, confirmed bookings, audit)
    redis/                  → hold/lock mechanism (SET NX + TTL) — KEEP, it's the right tool for this specific job
    http/                   → handlers, middleware, router wiring
    messaging/               → publisher/consumer (see §6)
    payment/                  → payment gateway adapter (see §7)
    email/ or notification/   → optional, for booking confirmation emails
  platform/
    config/                 → env-based config loading, validated at boot
    logger/                  → slog setup
    telemetry/                → OpenTelemetry tracing + metrics setup
    middleware/                → cross-cutting HTTP middleware (see §5)
migrations/                → SQL migration files (goose or golang-migrate)
docker/                     → Dockerfiles (multi-stage) for api and worker
docs/
  adr/                       → Architecture Decision Records, one per major decision
  api.md or openapi.yaml      → API contract
test/
  integration/                → integration tests requiring docker-compose services
  e2e/                          → end-to-end flow tests
```

**Rules to enforce throughout:**
- `internal/domain/**` must never import anything under `internal/adapters/**`. If you catch yourself importing `redis`, `sql`, or `net/http` there, stop — that logic belongs in application or adapters.
- Every port (interface) lives next to the domain/application code that needs it, not next to the adapter that implements it — the domain defines the contract, adapters conform to it.
- Prefer discovering interfaces from real second implementations (e.g., a Postgres store and a fake in-memory store for tests) over speculatively designing them. If only one implementation will ever exist for something, don't force an interface on it just for symmetry.

Write an ADR (`docs/adr/0001-hexagonal-architecture.md`, etc.) for each of these decisions as you make them: choice of Postgres vs staying Redis-only, choice of messaging broker, choice of payment gateway abstraction, choice of JWT vs session-based auth.

---

## 2. Persistence strategy — hybrid, deliberately

Don't move everything off Redis. Use each store for what it's actually good at, and document the reasoning in an ADR:

- **Redis** — responsible for the *hold* mechanism only: `seat:{movieID}:{seatID}` with `SET NX EX`, and the `session:{sessionID}` reverse-lookup key. This is the concurrency-critical path and Redis is genuinely the right tool (atomic conditional writes + native TTL). Do not replace this with a database-based lock unless you also implement equivalent atomicity (`SELECT ... FOR UPDATE` + application-level expiry sweep) — and if you do, document the trade-off in an ADR, because it's a meaningfully different design.
- **PostgreSQL** — source of truth for everything durable: `users` (with hashed passwords), `movies`, `screenings`/`sessions` (real scheduling, not a hardcoded in-memory slice), `confirmed_bookings` (append-only once a hold is confirmed — this is the permanent sales record), `payments`, `audit_log`.
- Use `database/sql` with `pgx` as the driver, or `sqlc` for generated type-safe queries if you want an extra layer of production realism. Avoid a heavyweight ORM — keep SQL visible and reviewable, consistent with the project's existing preference for explicit code over magic.
- Migrations: use `golang-migrate` or `goose`, versioned files in `migrations/`, run automatically on boot in dev, and as an explicit step in CI/deploy for anything resembling production.
- When a hold is confirmed, write the confirmed booking to Postgres **within the same logical operation** that persists the Redis state change. Since these are two different stores, be explicit about the consistency model you're choosing (e.g., "Redis is confirmed first, Postgres write is the durable follow-up; if Postgres write fails, retry via outbox" — see §6). Write this down as an ADR; this is one of the more interesting production-realism problems in the whole project.

---

## 3. Authentication & user storage

Replace the "userID trusted from the request body" pattern entirely.

- `users` table: `id`, `email` (unique), `password_hash`, `created_at`, and whatever else is reasonable (name, role).
- **Password hashing**: use `bcrypt` (`golang.org/x/crypto/bcrypt`) with an appropriate cost factor, or `argon2id` if you want to go further — either is fine, but document the choice and cost parameters in an ADR since cost factor is itself a security/performance trade-off worth reasoning about explicitly.
- **Never** store or log plaintext passwords, even in debug logs. Add a lint/grep check or code review note reminding of this.
- JWT-based auth:
  - `POST /auth/register` — creates a user, hashes password, returns a token.
  - `POST /auth/login` — verifies credentials against the hash, returns a token.
  - Access tokens short-lived (e.g., 15 min); implement **refresh tokens** (longer-lived, stored server-side or as a rotating opaque token in Postgres) so the system has a realistic session lifecycle, not just a single long-lived JWT.
  - Auth middleware validates the JWT, injects the authenticated `userID` into `context.Context` — and **every** booking handler must read `userID` from context, never from the request body.
  - Every confirm/release operation on a booking session must compare `session.UserID != authenticatedUserID` and return a generic "not found" (404) rather than "forbidden" (403), to avoid leaking session existence to a user probing session IDs that aren't theirs. This check must exist from the first version of the code — don't ship a version that accepts a user identifier without validating it against the session owner.

---

## 4. Business rules to implement in `internal/domain` / `internal/application`

- Movie/seat validation: a hold request for a non-existent movie or an out-of-range seat must be rejected before touching Redis.
- Per-user hold limits (e.g., max N simultaneous held seats).
- `Booking.CanBeConfirmed()`-style domain methods for state-dependent rules (expired, wrong state transition, etc.) — keep pure, no I/O.
- A typed `Status` (`type Status string` + constants + `Valid()`) for booking state — never a bare `string` field with magic values scattered across the codebase.
- Consistent JSON field naming across all response DTOs — treat consistent `snake_case` JSON as a hard rule, and add a test that fails if a response struct lacks tags. Inconsistent naming between endpoints (e.g. one handler emitting `movieID` while every other emits `movie_id`) is a real, easy-to-introduce class of bug that breaks frontend integrations silently — it's worth a small automated check, not just review discipline.
- A real seat-map endpoint: `GET /movies/{id}/seats` should return the *full* seat grid (available + held + confirmed), computed server-side by cross-referencing screening geometry with current bookings. Don't leave that reconciliation to the frontend — the backend owns the source of truth for seat state and should hand back a complete, ready-to-render picture.

---

## 5. HTTP layer & middleware stack

Build a composable middleware chain (`internal/platform/middleware`), applied in this order (outermost first):

1. **Panic recovery** — must be outermost so it can catch panics from everything else, including other middleware.
2. **Request ID** — generate/propagate a request ID, inject into context and response headers (`X-Request-ID`), used for log correlation and tracing.
3. **Structured logging** — log method, path, status, duration, request ID, and (once authenticated) userID, using `log/slog`.
4. **Tracing span start** (see §8).
5. **CORS** — configurable allowed origins via config, not hardcoded.
6. **Rate limiting** — per-IP and/or per-user token bucket (e.g., `golang.org/x/time/rate`), with sensible limits especially on `POST /hold` and `POST /auth/login` (brute-force protection).
7. **Auth** — only on protected routes; public routes (`/movies`, `/auth/*`, `/healthz`) skip it.

Also implement:
- `GET /healthz` (liveness) and `GET /readyz` (readiness — checks Redis and Postgres connectivity) — standard production endpoints that should exist from the first deployable version of the API.
- Consistent error responses: a single `errorResponse{Message string, Code string}` JSON shape, and a central place that maps domain errors (`ErrSeatAlreadyBooked`, `ErrSessionNotFound`, `ErrUnauthorized`, ...) to HTTP status codes. Treat "every error path must write a response" as a non-negotiable rule — a handler that logs an error server-side and returns without writing anything leaves the client with a misleading `200 OK` on an empty body, which is a surprisingly easy mistake to make repeatedly across handlers if there isn't a shared pattern for it. Consider a small linter/test that scans handlers for a `return` not preceded by a response write.
- Graceful shutdown: `http.Server` with `Shutdown(ctx)` wired to SIGTERM/SIGINT, draining in-flight requests before exit.

---

## 6. Messaging

Introduce an event-driven piece so the project also teaches decoupling, not just request/response:

- Broker: RabbitMQ (simplest realistic option) or Kafka if you want to lean into higher-throughput patterns — pick one, document why in an ADR.
- Publish a `BookingConfirmed` event when a hold is confirmed (after the Postgres write succeeds).
- At least one consumer reacting to it — e.g., a `worker` binary that sends a (stubbed/logged) confirmation email/notification, and/or updates a read-optimized "seat map" cache.
- Consider the **outbox pattern** for the Postgres-confirm → publish-event step, so the write and the publish don't silently diverge if the process crashes between them — this is a great, realistic distributed-systems lesson to bake in given the hybrid-store design in §2.
- Keep the consumer idempotent (dedupe by event/booking ID) since at-least-once delivery is the realistic default for most brokers.

---

## 7. Payment gateway integration

- Define a `PaymentGateway` port in `internal/ports` with a minimal interface (`CreatePaymentIntent`, `ConfirmPayment`, or similar), and an adapter implementing it against **Stripe's test mode** (or a clearly-labeled fake/sandbox adapter if no real account is available — either is fine, but never hit a real payment gateway from tests or CI).
- Booking confirmation flow becomes: hold → create payment intent → (webhook or client confirms) → payment confirmed → booking confirmed in Postgres → `BookingConfirmed` event published.
- Handle payment webhooks (`POST /webhooks/payment`) with signature verification — this is a realistic and valuable security lesson (verifying the webhook actually came from the payment provider).
- Never log full card data or raw webhook secrets. Store only what's needed (payment intent ID, status), never card details.

---

## 8. Observability

- **Logging**: `log/slog` with structured JSON output in non-dev environments, human-readable in dev (config-driven). Every log line in a request path should include the request ID.
- **Tracing**: OpenTelemetry, exporting to a local Jaeger or console exporter for dev (via `docker-compose`). Trace at minimum: incoming HTTP request, Redis calls, Postgres calls, and the message publish/consume path.
- **Metrics**: Prometheus-compatible `/metrics` endpoint — request count/duration by route and status, active holds gauge, booking confirmation counter, payment success/failure counter.

---

## 9. Testing strategy

Establish this pattern early and apply it consistently to every new port and use case added to the project:

- **Unit tests** (`internal/domain`, `internal/application`): fast, no external dependencies — use in-memory fakes for every port (`BookingStore`, `UserStore`, `PaymentGateway`, `EventPublisher`). A well-designed port should always have at least one fake implementation exercised by unit tests, alongside the real adapter — that's the whole point of defining it as an interface rather than depending on a concrete store type directly.
- **Concurrency tests**: many goroutines racing to book the same seat, asserting exactly one success and (numGoroutines - 1) failures. Run these with `-race` in CI, always. Add an equivalent concurrency test for the payment/confirm path if relevant.
- **Integration tests** (`test/integration`): real Redis + real Postgres (via `docker-compose` or `testcontainers-go`), testing adapters against the real thing, not fakes. Include a lifecycle test (hold → expire via TTL → seat becomes available again) since that's a core, easy-to-regress behavior.
- **E2E tests** (`test/e2e`): spin up the whole stack, drive it via HTTP only, cover the full booking journey including auth and (stubbed) payment.
- Target meaningful coverage on `domain` and `application` layers particularly — thin adapters and wiring code matter less.
- Add `go test -race ./...` and `golangci-lint run` as required CI steps from day one.

---

## 10. Configuration & secrets

- All configuration via environment variables, loaded and **validated at startup** (fail fast with a clear error if something required is missing — don't let the process start in a broken state).
- No secrets committed to the repo. Provide a `.env.example` with placeholder values and clear comments.
- JWT signing key, DB credentials, Redis address, broker URL, payment gateway keys — all env-driven, never hardcoded. There should be no addresses, secrets, or connection strings literally written into `.go` source files anywhere in this repo.

---

## 11. Docker & local development

- Build a `docker-compose.yaml` covering all infra dependencies: Redis + a Redis-inspection UI (e.g. redis-commander) for visualizing hold keys/TTLs during development, Postgres, the message broker, and a tracing backend (Jaeger). Optionally add the API/worker themselves too, with a real multi-stage `Dockerfile` for each.
- Keep local dev ergonomic: `docker compose up -d` should bring up all infra dependencies; running `go run ./cmd/api` and `go run ./cmd/worker` directly on the host should still work without rebuilding images, for fast iteration — don't force the Go app itself into Docker for local dev unless there's a good reason.
- Add a `Makefile` or `justfile` with common tasks: `make up`, `make migrate`, `make test`, `make test-race`, `make lint`, `make run`.

---

## 12. Documentation expectations

- `README.md`: architecture diagram (even ASCII is fine), how to run locally, how to run tests, environment variables reference.
- `docs/adr/`: one file per significant decision, using a short standard ADR format (context, decision, consequences). Do not skip these — they are the main artifact that makes this project valuable as a *study* project, not just a working system.
- API contract: OpenAPI spec or a well-organized `docs/api.md`, kept in sync with actual handlers.
- Inline comments should explain *why*, especially anywhere a trade-off was made (e.g., why Redis stays for holds while Postgres owns confirmed bookings; why the outbox pattern is/isn't used; why a particular consistency model was chosen over a stronger one).

---

## 13. Non-goals / things to explicitly avoid

- Don't turn this into microservices. Keep it a well-organized modular monolith (`cmd/api`, `cmd/worker` as separate binaries is enough process separation to teach the concept without the operational overhead of real service-to-service networking).
- Don't add an API Gateway or service mesh — out of scope and unjustified at this scale.
- Don't introduce an ORM if it means hiding SQL that would otherwise be a good learning artifact.
- Don't gold-plate the payment integration into a full billing system — a minimal, correctly-verified webhook flow against a sandbox is enough.

---

## 14. Definition of done for any feature added in this repo

Before considering any slice of this project complete, confirm:
- [ ] Domain logic has no infra imports.
- [ ] New port has at least one fake implementation used in unit tests.
- [ ] Concurrency-sensitive paths have a test asserting correct behavior under parallel load, run with `-race`.
- [ ] Every error path returns an appropriate HTTP status and structured error body — no silent `200 OK` on failure.
- [ ] Structured logs include request ID; traces cover the new code path.
- [ ] Config values are env-driven, validated at boot, documented in `.env.example`.
- [ ] An ADR exists if a non-obvious architectural or trade-off decision was made.