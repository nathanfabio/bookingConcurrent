# 10. Hold-expiry sweeper: composite global index, token-checked Lua teardown

Date: 2026-10-10
Status: Accepted

## Context

ADR 0002 made Redis expiry the hold mechanism's safety net — seat and
session keys vanish on their own, so availability never waits on a cleanup
job — and documented the residue that choice leaves behind: expiry is
SILENT, so the bookkeeping ZSET members (`user:{userID}:holds`, which the
in-script `ZCARD` counts against `MAX_ACTIVE_HOLDS`, and the global
`holds:expiring` index) outlive the keys until an explicit `ZREM`. Every
expired hold therefore leaks one of the user's hold-limit slots and grows
the ZSETs forever. ADR 0002 called the sweeper "a required component (not
an optimization)" doing "token-checked cleanup", and deferred it to "the
milestone that introduces `cmd/worker`". ADR 0006 repeated the deferral,
adding the operational value: deterministic cleanup timing, the hold-limit
slot back sooner, and observability into expired-hold volume.

That milestone is this one: M6 introduced `cmd/worker` (ADR 0009). This ADR
records the two decisions the sweeper forced — how it finds WHOSE holds
expired, and how teardown stays safe against re-claimed seats.

## Decision

**1. The global `holds:expiring` ZSET member becomes composite:
`"userID:sessionID"`.** The sweeper is driven by the global ZSET because it
is the only bounded, score-ordered index of expiry (`ZRANGEBYSCORE -inf
now LIMIT 0 n`). But the residue that matters most — the per-user slot —
lives in `user:{userID}:holds`, and by sweep time the session hash, the
only other place the userID is recorded, has usually TTL-expired (hash,
seat key, and score all die at the same wall-clock moment; the sweeper
runs after). A global index of bare session IDs can therefore clean itself
but cannot reclaim user slots. Embedding the userID in the member makes
the global ZSET self-sufficient. Rejected alternatives:

- *Redis keyspace notifications* (get an event per expiry): fire-and-forget
  — notifications emitted while the worker is down are lost forever, which
  is exactly the outage the sweeper exists to survive. The ZSET is durable
  state; a pub/sub message is not.
- *An auxiliary `sweep:{sessionID} → userID` mapping key*: one more write
  per hold, and it needs its own TTL — reintroducing the same
  "which copy expired first" race one level down.
- *Scanning `user:*:holds`*: O(all users) per tick, unbounded, and SCAN
  over a keyspace is precisely the pattern the global ZSET was built to
  avoid (ADR 0002).

The user ZSET's member stays the bare sessionID (its KEY already names the
user). The split parses on the LAST colon: the sessionID is a server-minted
UUID and guaranteed colon-free, while a userID in principle is not — so the
rightmost segment is the only unambiguous sessionID. The schema change is
pinned by `TestGlobalHoldZSETMemberIsComposite`; no existing assertion
reads member values (only `ZCARD` counts), so the change is contained.

**2. Teardown is one token-checked Lua script with three outcomes.**
`SweepExpired` enumerates the expired score window, then runs `sweepScript`
per member — one atomic unit per hold, extending ADR 0002's
compare-and-delete rule to the sweeper as it promised:

- **GONE** (the normal case — the hash already TTL-expired): remove both
  ZSET members; the seat key is deliberately NOT touched, because it either
  expired too, or it belongs to a NEWER hold whose token protects it. A
  sweep must never delete a seat it cannot prove belongs to the swept hold.
- **SWEPT** (the hash outlived its score — the ceiling-rounded TTL keeps
  raw keys alive up to a second past `ExpiresAt`, ADR 0002's conservative
  direction): full teardown, with the seat key deleted ONLY if its value
  still equals this hold's token, then `DEL` hash and `ZREM` both members.
- **ALIVE** (score and hash disagree — unreachable via `holdScript`, which
  writes both from the same value; only hand-tampered data arrives here):
  skip and mutate nothing. Inconsistent state is never a license to delete
  live claims.

The boundary is INCLUSIVE (`score <= now.Unix()`) because the domain treats
a hold as expired AT `ExpiresAt` (`IsExpired: !now.Before(...)`), and the
score, the hash's `expires_at`, and the truncated-Unix time `Get` hands the
domain are all the same second — so sweep timing and `CanBeConfirmed` agree
to the second, and sweeping up to a second "early" in wall-clock terms is
indistinguishable at the API edge (expired, unknown, and foreign sessions
already answer the identical 404, ADR 0006).

**3. Bounded, isolated, anti-clog.** One sweep reclaims at most
`WORKER_SWEEP_BATCH_SIZE` members per tick (`WORKER_SWEEP_INTERVAL`,
default 30s — holds live minutes, so slot-reclaim latency of seconds is
invisible); a pass that hits the limit leaves the remainder to the next
tick rather than growing an inner loop. Per-member script failures are
logged and skipped (`HeldSeats`' philosophy: one bad member never fails the
batch), and UNPARSEABLE members are logged loudly and `ZREM`'d — a poison
member would otherwise re-enter every `LIMIT` window forever. That discard
path doubles as the upgrade route for legacy bare-sessionID members
written before this change.

**4. The sweeper gets its own port and package, and the fake gets more
faithful.** `ExpiredHoldStore` lives in `internal/application/holdexpiry`
(its consumer), NOT on `booking.HoldStore` — the booking service never
sweeps, and per CLAUDE.md §1 an interface grows the methods its consumers
need. Two real implementations exist (the Redis adapter and the memory
fake), so the interface is discovered, not speculative. The `now` clock is
caller-supplied (adapters stay clock-free, mirroring the hold flow). The
memory fake was upgraded to model the residue faithfully: composite global
bookkeeping, and LAZY PURGE on observation — an expired session vanishes
from `Get`/`Release`/`HeldSeats` (Redis's TTL) while its bookkeeping
lingers (Redis's ZSETs), which is what makes the GONE branch and the
slot-leak-then-reclaim story testable without Redis.

## Consequences

- **Correctness still does not depend on the sweeper** (ADR 0006 stands):
  Redis TTL frees seats and Postgres-first availability means residue
  cannot change what a client is told. What the sweeper adds is user-facing
  fairness (hold-limit slots return on the sweep interval instead of
  lingering until... nothing), bounded memory (both ZSETs stay
  proportional to LIVE holds), and the expired-hold count in the worker's
  logs. ADR 0012 adds a separate active-holds gauge using `ZCOUNT` over
  FUTURE scores in the global ZSET; `ZCARD` would include expired residue
  and is not a valid active-holds count.
- **The deploy seam, honestly:** legacy bare-sessionID members get
  discarded by the anti-clog path, but their `user:{id}:holds` entries are
  unrecoverable — those users lose a hold-limit slot until the entries are
  manually removed. Holds are transient and this system is pre-production,
  so the upgrade instruction is: flush the dev/test Redis DB when deploying
  this change. A production rollout would need a one-off reconciliation
  pass (scan user ZSETs, `EXISTS session:{id}` each member, `ZREM` the
  orphans) — recorded here, deliberately not built (§13).
- **No new race class.** Sweep-vs-confirm at the expiry boundary is ADR
  0006's already-legitimate window: a confirm that committed a microsecond
  before the sweep wins (the sale stands; the sweep's release counterpart
  is best-effort-after-commit and finds `NOT_FOUND`), and a confirm after
  the sweep answers the identical 404. Sweep-vs-rehold is the token check
  (`TestSweepTokenCheckProtectsNewerHold`, extending
  `TestLateReleaseCannotDestroyANewerHold`).
- **Tests that prove this model:** unit (memory fake + ManualClock) —
  `TestSweepReclaimsExpiredHoldSlot`, `TestSweepLeavesLiveHoldsAlone`,
  `TestSweepBoundaryAtExpiresAt`, `TestSweepHonorsLimit`,
  `TestSweepGoneBranchReclaimsBookkeeping`,
  `TestSweepTokenCheckProtectsNewerHold`,
  `TestSweepOncePropagatesStoreError`; integration against real Redis —
  `TestGlobalHoldZSETMemberIsComposite`,
  `TestSweepReclaimsExpiredHoldSlot`,
  `TestSweepTokenCheckProtectsNewerHold`,
  `TestSweepTeardownWhenSessionStillPresent` (SWEPT),
  `TestSweepSkipsAliveHoldWithStaleScore` (ALIVE),
  `TestSweepHonorsLimit`, `TestSweepDiscardsUnparseableMember`. The
  existing hold race/lifecycle suites stay green across the script change —
  they assert counts, not member formats.
