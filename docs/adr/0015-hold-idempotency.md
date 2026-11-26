# 0015. Recover the original hold after a lost creation response

**Status** Accepted

## Decision

A caller may submit a canonical lowercase UUIDv7 in `Idempotency-Key` on
`POST /holds`. The UUID timestamp defines a 24-hour window for new allocations,
with one minute of future clock tolerance. Once that window closes, a missing binding
cannot allocate a seat, even after Redis has discarded its record. A live
existing hold can still be recovered. This bounds storage and
prevents an arbitrarily late retry from opening a new checkout. Generate a
fresh key for each new checkout and keep it across network retries.

The existing screening, geometry and confirmed-seat checks run first after
key validation. A confirmed seat returns its existing 409. In the atomic
Redis hold script, a successful request binds the authenticated user and
key to the screening, normalized seat, original session and deadline. A
live matching retry returns the original 201 response. A different target
or a closed session returns 409. Failed attempts do not reserve a key.
Release and expiry leave the binding intact. Redis retains it for at least
25 hours from creation or the initial hold lifetime, whichever is longer.

## Consequences

Redis stores one additional expiring record per successful keyed hold. A
keyed hold rounds its initial deadline up to Redis's existing whole-second
session precision. Retries keep that deadline. Calls without a key retain
their previous behavior. Redis must retain the binding during the retry
window. Losing or evicting transient Redis data can lose this guarantee.
