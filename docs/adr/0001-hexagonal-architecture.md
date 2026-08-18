# 1. Hexagonal architecture, modular monolith, consumer-owned ports

Date: 2026-08-18
Status: Accepted

## Context

This project's stated purpose is to teach how a production Go system is
structured and why (CLAUDE.md preamble). Three shape decisions had to be
made before any code: process topology, internal layering, and where
interfaces live.

The reference implementation this project grows out of was a single binary
with a BookingStore interface and two implementations — already a small
hexagon. Its gaps (no auth, no durable records, swallowed errors) were
feature gaps, not consequences of its architecture. So the question was not
"does hexagonal work" but "how far do we take it without gold-plating."

## Decision

**Modular monolith, two binaries.** One module, two processes:
`cmd/api` (HTTP) and `cmd/worker` (background jobs). No service-to-service
networking, no API gateway, no mesh (CLAUDE.md §13). Two binaries is enough
process separation to teach async work and independent deployability
without paying microservice operational costs this project does not have.

**Hexagonal layering with hard, mechanically-enforced edges:**

- `internal/domain/**` — entities, value objects, domain errors, pure state
  rules. Zero imports from adapters, platform, drivers, transports, or
  observability. Enforced by depguard in CI (`.golangci.yml`), not by
  review discipline.
- `internal/application/**` — use cases and orchestration. Depends on
  domain and on ports; never on adapter implementations.
- `internal/adapters/**` — implementations: Redis hold store, Postgres
  repositories, HTTP handlers, messaging, payment gateways. Adapters do not
  import sibling adapters; when two adapters must cooperate, the
  application layer owns the interface and does the wiring.
- `internal/platform/**` — infrastructure plumbing (config, logger,
  telemetry, middleware, server lifecycle). Imports no adapters: where a
  middleware needs adapter behavior (e.g. writing the error body after a
  panic), the composition root injects it.
- `cmd/**` — composition roots only.

**Ports are consumer-owned.** An interface lives in the package that calls
it (application layer, occasionally domain), not in a central `internal/ports`
directory and not next to the adapter that implements it. CLAUDE.md §1's
directory diagram sketches a central ports package, but §1's own rule —
"every port lives next to the domain/application code that needs it" — is
the normative statement, and it is the idiomatic Go position ("accept
interfaces, return structs"). A consequence, deliberately embraced: an
interface exists only once a real need exists — typically one production
implementation plus an in-memory fake for tests. We do not pre-declare
interfaces for symmetry; CLAUDE.md explicitly warns against speculative
ports, and M7's `PaymentGateway` (justified by its two real implementations:
fake sandbox and Stripe) is the test case for that rule.

**Stdlib-first.** `net/http` with Go 1.22+ pattern routing, `log/slog`,
`crypto/rand`. A third-party dependency must earn its place (driver,
algorithm, or protocol we should not hand-roll — e.g. pgx, bcrypt, AMQP);
application frameworks do not.

## Consequences

- Dependency rule violations fail CI instead of surviving code review.
  Cost: depguard configuration must be maintained as packages are added.
- Use cases are testable with in-memory fakes and no network; every port is
  expected to have a fake exercised by unit tests (CLAUDE.md §9).
- Composition roots concentrate all wiring; adding a dependency is a
  visible diff in exactly one place per binary.
- Rejecting a central ports directory means two packages' needs may produce
  two small, different interfaces over the same store. That duplication is
  accepted: each interface stays minimal for its consumer, which is the
  point of consumer-driven contracts.
- The modular-monolith boundary makes a future service split possible
  (adapters and use cases would move largely intact) without committing to
  one now.
