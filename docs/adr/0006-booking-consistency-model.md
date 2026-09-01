# 6. Booking consistency model: Postgres commits, Redis follows

Date: 2026-09-01
Status: Accepted

## Context

A confirmed booking touches two stores at once: the durable sales record
lives in Postgres, the hold it grew out of lives in Redis. Two stores means
a window in which they can disagree — a process can crash mid-confirm, the
durable write can fail after the hold is released, or Redis can hiccup
after the commit lands. ADR 0003 chose the split and deferred this exact
question: *"A crash during confirm can leave Redis and Postgres momentarily
inconsistent; ADR 0006 defines which side wins how (Postgres, always) and
what reconciles the residue."* This is that ADR.

The schema already encodes half the answer (migrations/00003): the PARTIAL
unique index `confirmed_bookings_seat_unique` (at most one confirmed
booking per screening+seat) and the plain unique index
`confirmed_bookings_session_unique` (one booking per hold session). Both
were proven by schema tests in M2. This ADR documents how the confirm flow
uses them and how the two stores stay reconciled around them.

## Decision

**Postgres is the source of truth; Redis is a fast path that follows.**

1. **The commit point is ONE Postgres transaction: booking row + outbox
   row.** `BookingRepo.Confirm` inserts the confirmed booking and its
   `BookingConfirmed` outbox event in a single transaction (completing
   ADR 0003's commit-point definition). Either both land or neither does;
   there is no state where the sale is recorded but the event is not
   queued. The outbox relay that publishes the event is M6 — until then
   rows accumulate with `published_at NULL`, which is the outbox doing its
   job.

2. **Conflicts are arbitrated by the schema, not by application checks.**
   The confirm is a plain `INSERT`; on SQLSTATE 23505 the repository reads
   WHICH unique index fired (`confirmed_bookings_seat_unique` →
   `ErrSeatAlreadyBooked`, `confirmed_bookings_session_unique` →
   `ErrSessionAlreadyConfirmed`). Any other constraint name is logged and
   surfaced as a generic failure — never guessed at, and pinned by
   integration tests asserting the literal index names.

3. **Confirm is idempotent per session.** On either conflict sentinel the
   use case looks the session up (`GetBySession`): an existing confirmed
   row owned by the caller is returned as success (HTTP 200 vs 201 on the
   first confirm), not an error. Recovery runs for BOTH sentinels because
   an idempotent replay violates both unique indexes at once and which one
   Postgres reports first is not guaranteed. No row for the session means
   the seat index fired: someone else owns the seat, and the conflict is
   surfaced as-is.

4. **Redis cleanup is best-effort and runs AFTER the commit, never
   before.** Releasing the hold before the durable write would strand the
   buyer if Postgres then failed — the wrong failure direction. After a
   successful commit, cleanup failure is logged and the request still
   succeeds; the residue is safe-direction:
   - *seat key*: blocks nothing — the pre-hold check rejects holds on
     confirmed seats, and the seat map shows `booked` regardless;
   - *session key*: a replayed confirm still recovers idempotently via
     `GetBySession` (idempotency survives Redis loss);
   - *ZSET bookkeeping*: costs the user one hold-limit slot until TTL
     expiry — the only user-visible residue.

5. **Availability is computed Postgres-first, Redis second.** The seat map
   renders `booked > held > available`: a seat with a confirmed Postgres
   booking is booked even if a stale hold still lingers in Redis. The
   phantom-hold defense works the same way on the write path: a
   pre-hold `ExistsConfirmedSeat` check stops the common case (one indexed
   lookup), but it is a fast path, NOT the arbiter — a seat can be
   confirmed between the check and the claim, and the partial unique index
   re-checks at confirm time. A hold on an already-booked seat is
   therefore harmless: its confirm is rejected, and it costs at most one
   hold-limit slot until TTL.

6. **The seat map route is screening-scoped: `GET /screenings/{id}/seats`,
   not CLAUDE.md §4's literal `GET /movies/{id}/seats`.** This is a
   deliberate correction, not an oversight: the schema scopes ALL seat
   state to screenings (`confirmed_bookings.screening_id`, the Redis key
   scheme of ADR 0002, migrations/00002), because seat A1 at Tuesday's
   showing and A1 at Friday's are independent. A movie-level seat map
   would need a screening selector bolted on, which is exactly the
   indirection the schema exists to avoid. `GET /movies` and
   `GET /movies/{id}/screenings` (public reads) provide the discovery
   path to screening IDs.

## Consequences

- **Correctness does not depend on the expiry sweeper.** Redis's own TTL
  expiry — both passive (checked on next access) and Redis's internal
  active expiration cycle — removes hold state on its own, and
  Postgres-first availability means stale residue cannot change what a
  client is told. The sweeper, when it lands with `cmd/worker`, adds
  operational value: deterministic cleanup timing, the hold-limit slot
  back sooner, and observability into expired-hold volume. It is deferred
  to that milestone deliberately rather than bolted onto `cmd/api` now and
  moved later. Until then, ZSET bookkeeping accumulates stale members
  after silent expiry — behavior pinned by
  `TestHoldExpiresAndSeatBecomesAvailableAgain`.
- **Known race windows, all safe-direction.** (1) Confirm vs expiry
  boundary: `CanBeConfirmed` can pass a sub-second before the Postgres
  commit crosses `ExpiresAt` — the ceiling-rounded TTL design already
  chose "Redis errs conservative, domain rule exact"; M5 must re-anchor
  this to payment capture. (2) Confirm racing the owner's explicit
  release: both operations required ownership of the same hold, so the
  committed sale is legitimate. (3) The phantom-hold window between
  pre-check and `SET NX`, bounded by the arbiter as described above.
- **Expired holds are indistinguishable from unknown or foreign ones at
  the API edge.** `ErrHoldExpired`, `ErrHoldNotFound`, and
  `ErrNotHoldOwner` all map to the byte-identical 404 `not_found`:
  otherwise the same user-visible event would get two different responses
  depending on sub-second timing (Redis key gone vs domain check firing),
  and session probing would leak more than CLAUDE.md §3 allows.
- **The constraint-name mapping is a schema dependency.** Renaming either
  unique index in a future migration changes conflict semantics; the
  integration tests assert the names so that rename breaks the build
  instead of production.
- **Tests that prove this model:** `TestConcurrentConfirmExactlyOneWinner`
  (32 transactions race one seat through the full repository path — one
  winner, one outbox event), `TestConfirmConstraintDiscrimination`,
  `TestConfirmWritesBookingAndOutboxAtomically`,
  `TestServiceConcurrentConfirmSameSessionOneCreated`, and the lifecycle
  tests over real Redis + Postgres.
