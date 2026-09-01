# API Contract

Base URL (local dev): `http://localhost:8080`

Every response body is JSON. Field names are `snake_case` everywhere — this
is enforced by tests (CLAUDE.md §4), not convention.

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
{ "screenings": [ { "id": "…", "movie_id": "…", "starts_at": "…", "rows": ["A","B","C","D","E","F"], "seats_per_row": 10, "created_at": "…" } ] }
```

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

Turn the caller's live hold into the durable booking. The commit point is
one Postgres transaction — booking row + `BookingConfirmed` outbox row
(ADR 0003/0006); the Redis hold is released best-effort afterwards.

Response `201` (first confirm) or `200` (idempotent replay — the same
booking returned again):

```json
{ "id": "…", "session_id": "…", "screening_id": "…", "row": "A", "number": 1, "status": "confirmed", "confirmed_at": "…" }
```

Errors: `404 not_found` (unknown/foreign/expired session),
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

## Development seed credentials

`make run` seeds a development user (documented non-secret, dev only):

- email: `dev@example.com`
- password: `booking-dev-password`

It also seeds two movies with screenings (geometry A–F × 10) so the catalog
and seat-map endpoints have data to browse.

## Not yet implemented

- Rate limiting on `/auth/login` and `POST /holds` (brute-force / abuse
  protection, CLAUDE.md §5) — arrives with the middleware hardening
  milestone.
- Payments (hold → payment intent → capture → confirm, CLAUDE.md §7) —
  M5 plugs into the confirm path this milestone built.
- Hold-expiry sweeper (`cmd/worker`) — deferred; correctness does not
  depend on it (ADR 0006).
- Outbox relay / `BookingConfirmed` consumer — M6.
