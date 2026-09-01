# 2. Hold atomicity: Lua scripts, hold tokens, single-instance Redis

Date: 2026-08-18
Status: Accepted

## Context

A seat hold touches four keys that must stay consistent with each other:
the seat claim (`seat:{screeningID}:{row}:{num}`), the session
reverse-lookup (`session:{sessionID}`), the per-user holds ZSET
(`user:{userID}:holds`, used for the hold limit and sweep enumeration),
and the global holds ZSET (`holds:expiring`, used by the expiry sweeper
and later the active-holds metric). Two correctness requirements shape
the mechanism:

1. **Mutual exclusion under concurrency.** N simultaneous hold attempts for
   one seat must yield exactly one winner. `SET key token NX EX ttl` gives
   this for the seat key alone — but the session and ZSET writes must be
   conditional on winning, or losers leave garbage behind.
2. **Safe teardown.** Release (and later the expiry sweeper) must never
   delete a seat claim that now belongs to a *different* hold: between one
   hold's TTL expiry and a late release arriving, another user may have
   acquired the seat.

The reference implementation used plain `SET NX` + a second unconditional
write; that design can leave a dangling session key pointing at a seat the
client never won.

## Decision

**Every multi-key hold operation is a Redis Lua script**
(`internal/adapters/redis/holdstore.go`). Redis executes a script as one
atomic unit against all other clients, with real branching inside:

- `holdScript`: check `ZCARD(userZSET) >= maxHolds` → `LIMIT`; else
  `SET seat token NX EX ttl` → on success write the session hash and both
  ZSET entries → `OK`; on NX failure → `TAKEN`. The limit check is inside
  the script on purpose: a check-then-act from Go lets a burst of parallel
  requests all pass the check before any of them books (the limit becomes
  advisory exactly when it is under pressure).
- `releaseScript`: read the session hash; `NOT_FOUND` if gone, `NOT_OWNER`
  if the user does not match; else delete the seat key **only if its value
  still equals this hold's token** (compare-and-delete), then remove the
  session and ZSET entries.

**Rejected alternative — MULTI/EXEC or pipelining.** A transaction cannot
branch on intermediate results: `EXEC` queues commands blind, so the Go
code would have to check the `SET NX` outcome *after* executing and
compensate (delete the session key it just wrote) on loss. That leaves a
real window where `session:{id}` exists pointing at a seat the client
never owned — reachable state, since release/confirm look the session up
first — and costs two round trips to teach the wrong lesson. Lua is one
round trip with genuine conditional logic.

**Hold tokens.** Every hold mints a UUID stored as the seat key's *value*
and in the session hash. Teardown paths compare it before deleting.
Without the token, the sequence "A's hold expires → B acquires the seat →
A's client releases late" deletes B's seat key. The hold-expiry sweeper
(the milestone that introduces `cmd/worker`) follows the same rule.

**Sessions are stored as Redis hashes**, not JSON blobs, so Lua can read
individual fields with `HGET` (Redis has no JSON parsing in scripts) and
so redis-commander renders them field-by-field during development.

**Bookkeeping outlives keys, by design.** Redis expiry is silent: when a
hold's TTL lapses, the seat/session keys vanish but the ZSET members
remain until an explicit `ZREM`. The hold-limit count therefore includes
stale entries until the hold-expiry sweeper (the milestone that introduces
`cmd/worker`) reconciles them. This is the
"reconcilable state" choice from ADR 0006's subject area: a ZSET's
membership can be audited against reality (does the session still exist?)
and repaired; a counter that drifts on missed decrements contains no
information to reconstruct itself from. Until the sweeper lands, the
residue is harmless — seat availability is computed Postgres-first, and
Redis's own TTL expiry cleans up the claim keys (ADR 0006).

**Single-instance Redis, deliberately.** The scripts touch multiple keys,
which Redis Cluster would require to share a hash tag
(`{screeningID}`-style) or would simply refuse. Holds are low-volume,
small, and transient; one adequately-provisioned Redis instance (or a
replicated setup that fails over whole) is the right scale for this
system. The constraint is documented here so nobody is surprised later.

## Consequences

- Exactly-one-winner is guaranteed by Redis's single-threaded script
  execution and proven by the race test (`test/integration`, `-race`).
- Caveat worth knowing: a Lua script is atomic with respect to
  *interleaving* but not all-or-nothing on *runtime errors* — commands
  before the error have applied. The scripts are therefore kept short,
  type-simple, and their error paths tested.
- Teardown safety no longer depends on timing luck; it depends on token
  comparison, which is testable (`TestLateReleaseCannotDestroyANewerHold`).
- The sweeper becomes a required component (not an optimization): without
  it, per-user ZSETs accumulate stale members. It lands with the milestone
  that introduces `cmd/worker`, doing token-checked cleanup. Correctness
  does not wait on it — Redis's TTL frees the seats, and availability is
  computed Postgres-first (ADR 0006).
- Moving to Redis Cluster later means re-keying with hash tags — a
  contained change, but a real one.
