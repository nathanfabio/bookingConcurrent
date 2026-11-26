# API Contract

Base URL (local dev): `http://localhost:8080`

Application response bodies are JSON; `/metrics` uses Prometheus text and
204 responses have no body. JSON field names are `snake_case` — this is
enforced by tests (CLAUDE.md §4), not convention.

## Error envelope

Every error path writes the same shape; there is no silent `200 OK` on
failure.

```json
{ "message": "invalid email or password", "code": "unauthorized" }
```

`code` is the stable, machine-readable half (clients branch on it);
`message` is for humans and may change.

| code | meaning |
|------|---------|
| `internal_error` | something went wrong server-side; nothing is revealed |
| `not_found` | resource does not exist (or you may not know it exists) |
| `unauthorized` | authentication required, or credentials/tokens rejected |
| `validation_error` | request body failed validation |
| `conflict` | registration conflicts with an existing account |
| `seat_taken` | a live HOLD owns the seat — may free up when the hold expires |
| `seat_booked` | a CONFIRMED booking owns the seat — it will not free up on its own |
| `hold_limit_exceeded` | the user already holds the maximum number of seats (`MAX_ACTIVE_HOLDS`) |
| `payment_required` | no captured payment for the session — start (or finish) checkout |
| `payment_declined` | the gateway refused the capture — a superseding intent can be minted |
| `webhook_invalid` | provider webhook failed verification — ONE opaque code for every failure mode (ADR 0008) |
| `cors_rejected` | 403: Origin is not allowed, or preflight method/headers are unsupported |
| `rate_limited` | 429: per-IP quota exhausted or bounded client table full; retry after Retry-After seconds |

## Browser access and request limits (ADR 0011)

Set `CORS_ALLOWED_ORIGINS` to comma-separated exact origins, for example
`http://localhost:5173,http://localhost:8080`. No wildcard, trailing slash,
path or opaque `null`. Empty means any request carrying Origin is rejected
with 403, including same-origin browser POSTs; list your own API origin too
when needed. Clients/webhooks without Origin continue normally. Origin is
browser policy and does not replace authentication.

Allowed browser origins receive credential support. Preflight OPTIONS with
Origin and Access-Control-Request-Method returns empty 204 for GET, HEAD,
POST or DELETE and these requested headers: Authorization, Content-Type,
Idempotency-Key, X-Request-ID, Traceparent, Tracestate. It requires no Bearer token and consumes
no quota. Responses vary by Origin; preflight also varies by requested method
and headers. X-Request-ID and Retry-After are exposed to browser JavaScript.
Cookies retain SameSite=Lax (ADR 0007); enabling CORS does not enable refresh
cookies on cross-site requests.

Rate limits are continuous token buckets per connection IP and API process:

| Scope | Refill per minute | Maximum burst |
|-------|-------------------|---------------|
| General | 120 | 30 |
| POST /auth/register, /auth/login, /auth/refresh, /auth/logout (shared) | 10 | 5 |
| POST /holds | 30 | 5 |

Auth/hold requests also consume the general quota. Failed requests count;
throttling happens before auth or application work. GET/HEAD /healthz,
/readyz and /metrics are exempt. Payment routes/webhooks use general quota; successful
preflight and CORS-denied requests terminate before rate limiting.

Exhaustion returns `429`, `Cache-Control: no-store`, `Retry-After` (positive
whole seconds), and the JSON error envelope with code `rate_limited`.
Do not retry before that delay; a delay is an estimate, not a reservation.
No future tokens are consumed by a denial. Quotas and bursts are configurable
in `.env.example`. The client table is capped (default 10,000 IPs); when full,
new IPs receive 429 until an idle entry can be reclaimed (default 10 minutes).

Forwarded/X-Forwarded-For/X-Real-IP headers are ignored. Behind a reverse
proxy, clients share its connection-IP quota; enforce per-client limits at
the trusted edge. State resets on restart and each replica has its own limits.

## Authentication model (ADR 0004, 0005, 0007)

- **Access token**: short-lived HS256 JWT, returned in the JSON body as
  `access_token`. Send it as `Authorization: Bearer <token>`.
- **Refresh token**: long-lived opaque credential, delivered **only** as an
  `httpOnly` cookie named `refresh_token` (never in a JSON body). The client
  does not read or manage it — the browser attaches it automatically to
  `/auth/*` requests.
- Refresh tokens **rotate** on every use; reusing a consumed token revokes
  the whole session family (theft detection, ADR 0004).

## Endpoints

### `GET /healthz` — liveness

Always `200` if the process is up (does not check dependencies).

### `GET /readyz` — readiness

`200` when Redis and Postgres are reachable, else `503` with a per-check
status map.

### `GET /metrics` — Prometheus exposition (ADR 0012)

Public monitoring endpoint (no JWT), with Prometheus text format rather than
JSON. GET/HEAD scrapes bypass rate limits and HTTP tracing/access logs.
Deploy behind the intended monitoring network; no credentials are exposed.

Exports `booking_http_requests_total` and
`booking_http_request_duration_seconds` with method/route-template/status
labels, plus booking confirmation, durable payment capture, gateway capture
attempt, Go and process metrics. Requests rejected before routing share
`route="unmatched"`. Metrics never label by user, session, request or trace ID.

`booking_active_holds` reads future expiry scores from the shared Redis index
on each scrape. `booking_active_holds_collection_success=0` and an absent
active gauge indicate Redis read failure, while other metrics remain
available. This gauge is global across API replicas: do not sum it. Counters
are per-process, reset on restart, and are not a replacement for durable
sales/payment records. Scrape concurrency is capped at two.

```bash
curl -s localhost:8080/metrics
```

HTTP responses continue returning X-Request-ID. Request/application logs
also contain trace_id/span_id when tracing provides a valid span context.
Incoming valid traceparent/tracestate join the caller's trace. HTTP spans
use route templates and exclude raw paths, query strings and credentials.

### `POST /auth/register`

Creates a user and opens a session.

Request:

```json
{ "email": "alice@example.com", "password": "averylongpassword", "display_name": "Ada" }
```

- `password`: 10–128 chars. `display_name`: optional (≤100 chars).

Response `201`:

```json
{
  "access_token": "eyJhbGciOi...",
  "token_type": "Bearer",
  "expires_in": 900,
  "user": { "id": "…", "email": "alice@example.com", "display_name": "Ada", "role": "customer" }
}
```

Plus `Set-Cookie: refresh_token=…; Path=/auth; HttpOnly; SameSite=Lax`
(and `Secure` in production). Errors: `400 validation_error`,
`409 conflict` (email already registered).

```bash
curl -i -X POST localhost:8080/auth/register \
  -d '{"email":"alice@example.com","password":"averylongpassword","display_name":"Ada"}'
```

### `POST /auth/login`

Request: `{ "email": "alice@example.com", "password": "…" }`

Response `200`: same shape as register (new access token + fresh refresh
cookie). Wrong password and unknown email return **byte-identical**
`401 unauthorized` bodies (`"invalid email or password"`) — this endpoint
does not reveal which accounts exist.

```bash
curl -i -X POST localhost:8080/auth/login \
  -d '{"email":"dev@example.com","password":"booking-dev-password"}'
```

### `POST /auth/refresh`

Consumes the refresh cookie and rotates it. No request body; the browser
sends the `refresh_token` cookie.

Response `200`: new `access_token` + a **new** `refresh_token` cookie. The
old cookie is now dead — replaying it returns `401` and revokes the family.

Every failure (`401`) also clears the cookie, so a dead session converges
to logged-out instead of retrying.

```bash
curl -i -X POST localhost:8080/auth/refresh -b cookies.txt -c cookies.txt
```

### `POST /auth/logout`

Revokes the entire session family and clears the cookie. Always `204` (no
body), idempotent — logging out with no cookie is still `204`.

```bash
curl -i -X POST localhost:8080/auth/logout -b cookies.txt
```

### `GET /auth/me` — *protected*

Returns the authenticated user. Requires `Authorization: Bearer <access>`.
The user id comes from the verified token, never a request parameter.

Response `200`: `{ "id": "…", "email": "…", "display_name": "…", "role": "customer" }`.
Missing/invalid/expired token → `401 unauthorized`.

```bash
curl -i localhost:8080/auth/me -H "Authorization: Bearer $ACCESS_TOKEN"
```

## Catalog & seat state (public reads)

Browsing never needs auth. Seat state is scoped to a SCREENING — seat A1 at
Tuesday's showing and seat A1 at Friday's are independent (ADR 0002 key
scheme, ADR 0006).

### `GET /movies`

The catalog, ordered by title.

Response `200`:

```json
{ "movies": [ { "id": "…", "title": "The Concurrency Menace", "synopsis": "…", "duration_minutes": 112, "created_at": "…" } ] }
```

### `GET /movies/{movieID}/screenings`

One movie's showings, chronological. Unknown movie → `404 not_found`
(distinct from a movie with no screenings, which is an empty list).

Response `200`:

```json
{ "screenings": [ { "id": "…", "movie_id": "…", "starts_at": "…", "rows": ["A","B","C","D","E","F"], "seats_per_row": 10, "price_cents": 1450, "created_at": "…" } ] }
```

`price_cents` is what a payment intent for this screening will freeze
(M5, ADR 0008) — clients see the price before holding.

### `GET /screenings/{screeningID}/seats` — seat map

The FULL seat grid, computed server-side (CLAUDE.md §4) with
Postgres-first precedence (ADR 0006): a seat with a confirmed booking is
`booked` even if a stale hold lingers in Redis. Each seat's status is one
of `available`, `held`, `booked`.

**Route note:** CLAUDE.md §4 names `GET /movies/{id}/seats`; the real
scheduling model scopes seat state to screenings, so the map lives under
the screening instead. Deliberate correction, recorded in ADR 0006.

Response `200`:

```json
{
  "screening_id": "…", "movie_id": "…", "starts_at": "…",
  "rows": ["A","B"], "seats_per_row": 3,
  "seats": [ { "row": "A", "number": 1, "status": "held" }, { "row": "A", "number": 2, "status": "available" } ]
}
```

Unknown screening → `404 not_found`.

```bash
curl -i localhost:8080/screenings/$SCREENING_ID/seats
```

## Booking (protected)

All three endpoints require `Authorization: Bearer <access>`. The user ID
always comes from the verified token — never from a request body or path
(CLAUDE.md §3). Confirm/release on a session that is unknown, expired, or
someone else's answer the SAME `404 {"message":"hold not found","code":"not_found"}`,
so session IDs cannot be probed (ADR 0006).

### `POST /holds`

Claim a seat for checkout.

To recover from a lost response, generate a **lowercase UUIDv7** before the
first attempt and send it in the optional `Idempotency-Key` header.
Repeat the same authenticated request with that key. A live replay returns
201 with the original session ID and deadline, including at the user's hold
limit. The initial keyed deadline rounds up to a whole second.

A fresh allocation requires a key less than 24 hours old (up to one minute
of clock skew into the future). A malformed, repeated or future-dated
header returns 400 `validation_error`. A key bound to a different valid
seat returns 409 `hold_key_conflict`. After release or expiry the same
request returns 409 `hold_key_closed`; a confirmed seat returns 409
`seat_booked` from the durable pre-check. Use a fresh key for a new
checkout. These semantics assume Redis retains the binding (ADR 0015).

Request:

```json
{ "screening_id": "…", "row": "A", "number": 1 }
```

Validation runs before Redis is touched (CLAUDE.md §4): unknown screening →
`404`; seat outside the grid → `400 validation_error`; already-confirmed
seat → `409 seat_booked` (phantom-hold defense, ADR 0006).

Response `201`:

```json
{ "session_id": "…", "screening_id": "…", "row": "A", "number": 1, "expires_at": "…" }
```

`session_id` is the handle for confirm/release. The hold token is
deliberately NOT returned — it is the server-internal compare-and-delete
secret (ADR 0002). Errors: `400 validation_error`, `404 not_found`,
`409 seat_taken` (someone's hold got there first), `409 seat_booked`,
`409 hold_limit_exceeded`.

```bash
curl -i -X POST localhost:8080/holds \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -d '{"screening_id":"'"$SCREENING_ID"'","row":"A","number":1}'
```

### `POST /holds/{sessionID}/confirm`

Turn the caller's live hold into the durable booking. Since M5 the confirm
is **payment-gated** (ADR 0008): a captured payment must exist for the
session, or the answer is `402 payment_required`. The commit point is one
Postgres transaction — booking row + `BookingConfirmed` outbox row
(ADR 0003/0006); the Redis hold is released best-effort afterwards.

Response `201` (first confirm) or `200` (idempotent replay — the same
booking returned again):

Replay reads the committed booking from Postgres and works after the Redis
hold has been released or expired, including when Redis is unavailable.
Only the owner can recover it. A first confirmation still requires a live
hold and captured payment; retrying never creates another booking or event.

```json
{ "id": "…", "session_id": "…", "screening_id": "…", "row": "A", "number": 1, "status": "confirmed", "confirmed_at": "…" }
```

Errors: `404 not_found` (unknown/foreign/expired unconfirmed session — the expiry
hard-NO fires even with money captured, ADR 0006/0008),
`402 payment_required` (no captured payment yet),
`409 seat_booked` (the seat lost a race to another session's booking).

```bash
curl -i -X POST localhost:8080/holds/$SESSION_ID/confirm \
  -H "Authorization: Bearer $ACCESS_TOKEN"
```

### `DELETE /holds/{sessionID}`

Give up the hold without confirming. `204`, no body. Errors:
`404 not_found` (unknown/foreign/expired session).

```bash
curl -i -X DELETE localhost:8080/holds/$SESSION_ID \
  -H "Authorization: Bearer $ACCESS_TOKEN"
```

## Payments (M5, protected unless noted)

The checkout flow (CLAUDE.md §7, ADR 0008): hold → payment intent →
capture (client confirm OR provider webhook) → booking confirm. The M5
gateway is the clearly-labeled in-process fake sandbox
(`PAYMENT_PROVIDER=fake`); `stripe` has a config slot but fails at boot
until the Stripe milestone. Sandbox intent IDs are recognizable
(`pi_fake_…`) so fake money is never mistaken for real.

### `POST /holds/{sessionID}/payment-intent`

Freeze the screening's price into a gateway intent for the caller's live
hold. Idempotent: an existing active intent for the session is returned
again, not re-minted (one active intent per session is enforced by the
`payments_active_session_unique` partial index, migration 00009).

Response `201` (created) or `200` (idempotent replay):

```json
{
  "payment_id": "…", "session_id": "…", "intent_id": "pi_fake_…",
  "amount_cents": 1450, "currency": "usd", "status": "intent",
  "hold_expires_at": "…"
}
```

`hold_expires_at` is the pay-by deadline: after it, capture still records
provider truth, but confirm refuses the booking (ADR 0006's hard NO).
There is no `client_secret` field — the fake has none, and inventing one
would be dishonest API design (ADR 0008).

Errors: `404 not_found` (unknown/foreign/expired session — byte-identical,
no probing through payment routes either).

```bash
curl -i -X POST localhost:8080/holds/$SESSION_ID/payment-intent \
  -H "Authorization: Bearer $ACCESS_TOKEN"
```

### `POST /holds/{sessionID}/payment-intent/confirm`

The client-confirm capture — the sandbox equivalent of Stripe's
client-side `paymentIntent.confirm()`, and the dev trigger for "the buyer
paid". The production-shaped path IS the fast path; there is no backdoor
route. The hold is re-checked BEFORE the gateway is called, so this
endpoint never charges an expired hold.

Response `200` (first capture and replay alike — capture is idempotent):

```json
{ "payment_id": "…", "intent_id": "pi_fake_…", "status": "captured", "amount_cents": 1450, "currency": "usd", "updated_at": "…" }
```

Errors: `404 not_found` (unknown/foreign/expired session),
`402 payment_required` (no active intent — create one first),
`402 payment_declined` (gateway refused; the intent is marked failed and a
superseding intent can be minted).

```bash
curl -i -X POST localhost:8080/holds/$SESSION_ID/payment-intent/confirm \
  -H "Authorization: Bearer $ACCESS_TOKEN"
```

### `POST /webhooks/payment` — public, signature-verified

The provider callback path. PUBLIC: the signature IS its authentication —
a provider cannot present a user JWT, so this route is deliberately not
wrapped in the auth middleware.

Verification (ADR 0008): header `Payment-Signature: t=<unix>,v1=<hex>`
where `v1 = HMAC-SHA256(secret, "<t>.<rawBody>")` over the EXACT request
bytes, ±300s replay tolerance, constant-time compare. ANY verification
failure answers one opaque `400 webhook_invalid` — which check failed is
never revealed. Every signature-valid event answers
`200 { "received": true }`, including duplicates, unknown intents, and
unhandled kinds (ack policy: retrying them cannot change the outcome).

Event body (fake provider's wire format):

```json
{ "kind": "payment.captured", "intent_id": "pi_fake_…", "amount_cents": 1450 }
```

`kind` is `payment.captured` or `payment.failed`. The amount is
cross-checked against the frozen intent amount; a mismatch fails the
payment and is logged loudly (provider numbers are input, not truth).

Signing a delivery by hand against the dev server (secret =
`PAYMENT_FAKE_WEBHOOK_SECRET` from `.env`):

```bash
BODY='{"kind":"payment.captured","intent_id":"'"$INTENT_ID"'","amount_cents":1450}'
T=$(date +%s)
V1=$(printf '%s.%s' "$T" "$BODY" \
  | openssl dgst -sha256 -hmac "$PAYMENT_FAKE_WEBHOOK_SECRET" -hex | awk '{print $NF}')
curl -i -X POST localhost:8080/webhooks/payment \
  -H "Payment-Signature: t=$T,v1=$V1" -d "$BODY"
```

## Events (M6) — broker contract, not HTTP

Confirmed bookings emit one `BookingConfirmed` event through the
transactional outbox (ADR 0006 wrote it, ADR 0009 publishes it):

```
confirm tx (booking + outbox row) → relay (cmd/worker, every
WORKER_RELAY_POLL_INTERVAL) → exchange "booking.events" (durable topic)
→ routing key "BookingConfirmed" → queue "booking.notifications"
(durable) → consumer → processed_events claim (dedupe) → notification stub
```

Transport envelope (AMQP):

| Property    | Value                                              |
|-------------|----------------------------------------------------|
| exchange    | `RABBITMQ_EXCHANGE` (default `booking.events`, topic, durable) |
| routing key | the event type, identity-mapped (`BookingConfirmed`) |
| queue       | `RABBITMQ_QUEUE` (default `booking.notifications`, durable, bound to the routing key) |
| message_id  | the outbox row's `event_id` UUID — the consumers' dedupe key |
| type        | `BookingConfirmed`                                  |
| content     | `application/json`, persistent delivery mode        |
| headers     | W3C `traceparent` (publish and consume spans join one trace) |
| semantics   | at-least-once: duplicates are normal, consumers MUST dedupe on `message_id` |

Payload (snake_case, pinned by reflection tests on BOTH the writer —
`bookingConfirmedPayload` in `internal/adapters/postgres` — and the
consumer — `BookingConfirmed` in `internal/application/notification`;
changing a field means changing this table and both structs):

| Field          | Type     | Meaning                                        |
|----------------|----------|------------------------------------------------|
| `booking_id`   | string   | the database-assigned confirmed booking id      |
| `session_id`   | string   | the hold session that produced the booking      |
| `screening_id` | string   | the screening the seat belongs to               |
| `row`          | string   | seat row letter                                 |
| `number`       | int      | seat number within the row                      |
| `user_id`      | string   | the buyer                                       |
| `confirmed_at` | RFC 3339 | when the booking committed (database clock)     |

```json
{
  "booking_id": "0e11…",
  "session_id": "5b4a…",
  "screening_id": "aa90…",
  "row": "C",
  "number": 7,
  "user_id": "c3d2…",
  "confirmed_at": "2026-10-10T21:14:02.481Z"
}
```

The M6 consumer side effect is a structured log line
(`adapters/notification.LogNotifier`). Its idempotency is the
`processed_events` claim table (migration 00010): exactly one winner per
`event_id` under any interleaving, proven by
`TestProcessedEventClaimConcurrentExactlyOneWinner`.

## Development seed credentials

`make run` seeds a development user (documented non-secret, dev only):

- email: `dev@example.com`
- password: `booking-dev-password`

It also seeds two movies with screenings (geometry A–F × 10, prices 1450
and 1250 cents) so the catalog, seat-map, and payment endpoints have data
to browse.

## Not yet implemented

- Rate limiting on `/auth/login` and `POST /holds` (brute-force / abuse
  protection, CLAUDE.md §5) — arrives with the middleware hardening
  milestone.
- Stripe test-mode adapter, refunds, and auto-refund of the
  captured-after-expiry window — ADR 0008's explicit deferrals.
