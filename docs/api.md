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

## Development seed credentials

`make run` seeds a development user (documented non-secret, dev only):

- email: `dev@example.com`
- password: `booking-dev-password`

## Not yet implemented

- Rate limiting on `/auth/login` (brute-force protection, CLAUDE.md §5) —
  arrives with the middleware hardening milestone.
- Booking endpoints (hold/confirm/release/seat-map) — M4.
