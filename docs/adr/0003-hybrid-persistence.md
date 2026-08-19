# 3. Hybrid persistence: Redis for holds, Postgres for truth

Date: 2026-08-18
Status: Accepted

## Context

The reference implementation kept everything — including would-be permanent
records — in Redis, whose TTL semantics fit transient locks but not sales
records or user accounts. The rebuild needs two things Redis is bad at:
durability with transactional multi-row writes (a booking plus its event,
atomically), and queryable relational data (users, catalog, history). It
also needs the one thing Redis is *uniquely* good at for this domain: an
atomic conditional write that expires by itself (`SET NX EX`), which is
exactly the semantics of "this seat is spoken for during checkout."

## Decision

**Redis holds only concurrency state; Postgres owns everything durable.**

- *Redis*: the `seat:` claim (SET NX + TTL), the `session:` reverse
  lookup, and the per-user/global hold ZSETs. Nothing written here is a
  business record — it is coordination state, and its loss degrades to
  "seat becomes available again," which is a safe failure direction.
- *Postgres*: users, movies/screenings, confirmed bookings (append-only),
  payments, outbox, refresh tokens, audit log. The confirm path's commit
  point is a single Postgres transaction (booking row + outbox row); see
  ADR 0006 for the full consistency model.

**Tooling: goose for migrations, sqlc for queries, pgx for the driver.**

- *goose* over golang-migrate: it runs as a Go library (auto-migration at
  dev boot needs no subprocess or shipped binary), migrations are plain
  SQL with `+goose Up/Down`, and it reads from an embedded FS in the
  deployed binary. golang-migrate is fine too; the library embedding won
  the tie.
- *sqlc* over hand-written repository SQL (the option CLAUDE.md allows):
  the SQL is still written by hand in `db/queries/*.sql` and reviewed
  normally; sqlc generates the scanning boilerplate and makes column/type
  drift a *generation* error instead of a runtime surprise. Schema input
  for the generator is the same goose migrations the server runs, so the
  three artifacts (migrations, generated code, live schema) are pinned to
  one source. `make sqlc-check` fails CI if generated code is stale.
- *pgx* directly (pgxpool) rather than `database/sql`: first-class
  Postgres types (`TEXT[]` for seat rows, JSONB), tracing via otelpgx,
  and honest error types. The single exception is migrations, where goose
  speaks `database/sql` through pgx's stdlib shim — one driver, two APIs.
- *No ORM.* The SQL is the review artifact; a repository layer maps rows
  to domain types and contains no decisions.

**IDs are UUIDs** (`gen_random_uuid()`), assigned by the database. Booking
and session identifiers appear in URLs, logs, and events; sequential
integer IDs would enumerate the business and invite probing.

**Migration policy** (CLAUDE.md §2): dev auto-migrates at boot (a dev box
can never be silently behind); CI and production run
`go run ./cmd/migrate up` as an explicit step, so a schema change is a
visible deploy action.

**Email uniqueness is a functional index** (`UNIQUE (lower(email))`)
rather than CITEXT: the case-insensitivity rule is visible in the schema
and needs no extension.

## Consequences

- The two-store boundary is now a line with a rule: if a piece of state
  must survive a Redis flush, it lives in Postgres. Holds deliberately do
  not have to survive one.
- A crash during confirm can leave Redis and Postgres momentarily
  inconsistent; ADR 0006 defines which side wins how (Postgres, always)
  and what reconciles the residue.
- sqlc adds a generation step to the workflow; the trade is compile-time
  query safety for every future query, which matters more as the schema
  grows.
- The `confirmed_bookings` partial unique index
  (`WHERE status='confirmed'`) makes cancellation re-bookable; this was
  validated by schema tests in the same milestone, before any application
  code depends on it.
- Test databases are created on demand (`booking_test`) and never share
  the dev database, so integration tests and manual development cannot
  corrupt each other.
