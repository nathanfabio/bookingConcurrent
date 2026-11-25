# ADR 0011: HTTP protection — exact CORS origins and bounded local quotas

Status: Accepted

## Context

The API already authenticates booking writes, but authentication does not
bound request volume. Password hashing is intentionally expensive (ADR 0005),
and holds reach Redis/Postgres. Browser clients also need an explicit CORS
contract for Bearer requests and refresh cookies (ADR 0007).

The middleware must reject before expensive work, preserve the JSON error
contract, and avoid an unbounded IP-to-limiter map. A proxy's forwarded IP
headers are client input unless a trusted proxy policy says otherwise.

## Decision

Use the standard library for both middlewares; there is no new dependency
or external store. The order is recovery → request ID → logging → CORS →
rate limiting → per-route authentication → handler. HTTP tracing will be
inserted between logging and CORS in the observability slice.

### CORS

`CORS_ALLOWED_ORIGINS` is a comma-separated exact allowlist of serialized
HTTP(S) origins: scheme, host, optional port. Configuration rejects wildcards,
opaque `null`, credentials, paths, queries and fragments. An empty list denies
all requests carrying Origin, including same-origin browser POSTs; deployments
must explicitly list the API's own origin if needed. Requests without Origin
(CLI clients and provider webhooks) continue normally. We deliberately do not
infer trust from Host or forwarded headers.

Allowed origins receive an exact `Access-Control-Allow-Origin` and
`Access-Control-Allow-Credentials: true`. Preflight accepts GET, HEAD, POST,
DELETE and only Authorization, Content-Type, X-Request-ID, Traceparent and
Tracestate request headers. It returns an empty 204 before authentication or
quota consumption. Ordinary OPTIONS requests continue to routing. Preflight
does not assert that a particular endpoint exists; the actual request still
passes through routing and authentication.

Reject unknown/ambiguous origins or unsupported preflight method/headers
with `403 cors_rejected` before downstream work. Vary includes Origin on
all responses and the requested method/header dimensions on OPTIONS.
Expose X-Request-ID and Retry-After so browser clients can read them, including
on downstream authentication or throttling errors. See the
[Fetch CORS protocol](https://fetch.spec.whatwg.org/#http-cors-protocol).

CORS is browser policy, not authentication: non-browser clients can forge or
omit Origin. Credential support does not change SameSite=Lax cookies; a truly
cross-site SPA still needs a separate cookie/CSRF design (ADR 0007).

### Rate limiting

One entry per canonical connection IP contains three continuously refilled
token buckets. An accepted request consumes general quota plus its specific
quota, if any. All checks and debits happen under one mutex; a denied request
consumes no tokens and reserves no future capacity. Default policy:

| Bucket | Refill per minute | Initial/maximum burst | Scope |
|--------|-------------------|-----------------------|-------|
| General | 120 | 30 | All requests except GET/HEAD probes and CORS-terminated requests |
| Auth | 10 | 5 | POST /auth/register, /auth/login, /auth/refresh, /auth/logout combined |
| Hold | 30 | 5 | POST /holds |

Failed authentication and invalid hold requests count too, because throttling
runs before authentication and validation. GET /auth/me and payment routes
(including webhooks) use the general bucket. GET/HEAD /healthz and /readyz
bypass allocation and limiting so probe traffic does not alter availability.

Return `429 rate_limited`, `Cache-Control: no-store`, and Retry-After in whole
seconds rounded up. The wait reflects the slowest depleted applicable bucket.
This follows [RFC 6585 §4](https://www.rfc-editor.org/rfc/rfc6585#section-4).

Key by net/http's RemoteAddr, ignore Forwarded, X-Forwarded-For and X-Real-IP,
remove ports and normalize IPv4-mapped IPv6 addresses. Malformed transport
addresses share one fallback bucket. Never key by raw path, user-submitted
identity, or arbitrary header values.

Bound entries with `RATE_LIMIT_MAX_CLIENTS` (default 10,000). A recency list
allows lazy eviction when a new client arrives; no background goroutine is
needed. Evict only entries idle for `RATE_LIMIT_IDLE_TTL` (default 10 minutes).
Validate TTL >= every burst's full refill time so eviction never grants tokens
earlier than natural refill. Active IPs are never evicted to make space. If
full, reject a new IP with 429 and a retry estimate until the oldest entry can
expire; later activity may extend that estimate. Map and list updates share
the same mutex, so capacity remains bounded under concurrent arrivals.

## Consequences

- State is local, resets on restart, and scales independently per replica.
  This is protection for a single-process deployment, not a distributed quota
  or an exact fixed-window "N requests per minute" guarantee.
- Behind a reverse proxy, clients share the proxy's quota. Enforce per-client
  limits at the trusted edge until an explicit proxy allowlist and forwarded
  chain parser are implemented. Never fix this by blindly reading XFF.
- Shared NAT clients share quotas. Distributed attackers and IPv6 address
  rotation can exhaust the bounded table; fail-closed admission protects
  memory but may deny new legitimate IPs. Operational tuning is required.
- There is no cross-IP limit on concurrent password hashes. Internet-facing
  deployment may additionally need edge controls and bounded expensive-work
  concurrency. No account lockout is introduced, avoiding easy account denial
  by submitting somebody else's email repeatedly.
- A single mutex favors understandable atomicity. Sharding or a distributed
  limiter should follow measured contention or deployment needs, with a new
  ADR. The current educational scope does not justify extra infrastructure.
- Tests cover deterministic refill, Retry-After rounding, independent/shared
  quotas, bounded admission and idle eviction, spoofed forwarded headers,
  canonical IP identity, concurrent bursts, and middleware/error ordering.
