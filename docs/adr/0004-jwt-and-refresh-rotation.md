# 4. Authentication: short-lived JWT access tokens + rotating refresh tokens

Date: 2026-08-27
Status: Accepted

## Context

The reference implementation trusted a `userID` from the request body —
anyone could book as anyone. CLAUDE.md §3 requires real authentication:
register/login endpoints, short-lived access tokens, and refresh tokens
stored server-side with rotation. Two shapes were on the table.

*Session-based*: an opaque session ID indexes a server-side record; every
request hits the store to resolve it. Strong revocation, but every
protected request needs a lookup, and the design goal here (a stateless
modular monolith whose workers and future services can verify identity
without sharing session state) points the other way.

*Pure stateless JWTs for both access and refresh*: nothing server-side at
all. Access tokens are fine this way — but a *refresh* token that is just
another JWT cannot be revoked before it expires. A stolen refresh token
would be good for its full 30-day lifetime, and logout would be a lie.

## Decision

**Split the difference: short-lived stateless JWT access tokens + rotating,
server-side, opaque refresh tokens.**

- **Access token**: an HS256 JWT, 15-minute TTL, verified on every request
  by the auth middleware. It is deliberately NOT revocable — the short TTL
  *is* the revocation story, and not implementing per-token revocation
  means not shipping a blacklist that pretends to be one.
- **Refresh token**: an opaque 256-bit random string, never a JWT. Only its
  SHA-256 hash is persisted (`migrations/00006`), so a database read leaks
  nothing usable. It lives in an httpOnly cookie (ADR 0007), not the JSON
  body, so JavaScript cannot read the long-lived credential.
- **Rotation + families**: each login opens a new token *family*; every
  refresh consumes the presented token and mints its successor in the same
  family. Presenting an already-consumed (or revoked) token is *theft*:
  the entire family is revoked before the error returns.
- **Claims**: `iss` (service name), `sub` (user id), `iat`, `exp`, and one
  custom claim `role`. No `jti` (we don't implement per-token revocation,
  so it would promise a feature we lack) and no `nbf` (clock-skew pain for
  zero gain in a single service). Validation pins HS256 only
  (`jwt.WithValidMethods`) and requires `exp` — this blocks `alg=none` and
  algorithm-confusion mechanically.

**Rotation is one atomic read-decide-write.** `Rotate` locks the token row
with `SELECT ... FOR UPDATE` inside a transaction, decides, then writes the
successor and marks the old row replaced. This is the Postgres mirror of
ADR 0002's lesson — "one atomic unit, no check-then-act gap" — applied to a
relational store instead of a Redis Lua script. Two concurrent refreshes of
the same token serialize on the lock: exactly one wins, and every other
caller observes the token already consumed, lands in the theft branch, and
revokes the family. The theft branch **commits** (the family revocation is
the point of that transaction); the expiry branch rolls back with no write.

**Expiry is not theft.** A token past its TTL returns `ErrRefreshTokenExpired`
without touching the family — letting a session lapse is normal lifecycle.
Only a *replaced* or *revoked* token being presented again is an attack.

## Consequences

- Register, login, and refresh return the access token in JSON and set the
  refresh cookie; `/auth/me` is the first protected route and reads the user
  id from the context the middleware populates — never from a request body.
- **Strict reuse detection kills a family on a benign double-submit**: two
  tabs (or a flaky network retry) refreshing at once look identical to
  theft, so the loser revokes everyone. Clients must serialize refresh
  calls. A grace window (accept the old token for N seconds after rotation)
  would soften this but re-opens a real theft window, so it is rejected.
- Login equalizes timing: an unknown email still runs one argon2 verify
  (ADR 0005), and both failure modes return the same sentinel, so responses
  cannot enumerate accounts.
- Every refresh failure clears the cookie client-side, so a dead session
  converges to "logged out" rather than retry-looping.
- JWTs are signed with a single shared secret (`AUTH_JWT_SECRET`); rotating
  it invalidates all access tokens at once. That is acceptable for a
  monolith; a key-set with overlap would be needed if multiple services
  minted tokens.
