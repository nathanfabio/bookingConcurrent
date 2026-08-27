# 7. Refresh-token transport: httpOnly cookie, access token in JSON

Date: 2026-08-27
Status: Accepted

## Context

A session has two credentials with very different blast radii. The access
token is short-lived (15 min); the refresh token is long-lived (30 days) and
can mint unlimited access tokens. How each is stored on the client decides
which attack steals which.

The natural alternative — return both tokens in JSON and let the frontend
hold the refresh token in `localStorage`/memory — has a sharp edge: anything
in `localStorage` is readable by any JavaScript on the page. One XSS hole
anywhere in the frontend then yields the long-lived credential and a
persistent session hijack.

## Decision

**Refresh token in an httpOnly cookie; access token in the JSON body.**

The refresh token is the one that needs the cookie treatment, because it is
long-lived and high-blast-radius. Setting it `HttpOnly` means JavaScript —
malicious or not — cannot read it, so an XSS payload can exfiltrate at most
the in-memory access token: fifteen minutes of damage, not a persistent
session. The access token stays in JSON and client memory, exactly as
planned.

Cookie attributes:

| Attribute | Value | Why |
|-----------|-------|-----|
| `HttpOnly` | always | Removes JS access — the core XSS defense |
| `Secure` | production only | Dev runs plain-http localhost; prod requires TLS |
| `SameSite` | `Lax` | CSRF protection, below |
| `Path` | `/auth` | Cookie only travels to the endpoints that use it |
| `Domain` | unset | Host-only; never leaks to subdomains |
| `MaxAge` | refresh TTL | Cookie death lines up with row expiry |

**Why SameSite=Lax is sufficient CSRF protection for `/auth/refresh`.**
HttpOnly buys the XSS defense but introduces the classic counter-risk: the
browser now attaches the credential automatically, so a malicious site could
try to make the victim's browser POST to `/auth/refresh` (CSRF). `SameSite=Lax`
closes this: the browser omits the cookie on **cross-site** requests that are
not top-level navigations — and both state-changing endpoints
(`POST /auth/refresh`, `POST /auth/logout`) are POSTs, which a cross-site
page cannot trigger with the cookie attached. Same-site requests (our own
frontend) are unaffected. `Strict` would also work but is rejected because it
withholds the cookie even on same-site top-level navigation, which buys
nothing here and breaks legitimate flows the day we add one. A synchronizer
CSRF token is the stronger guarantee, but it is redundant once Lax blocks the
only cross-site vector these POST endpoints expose.

**Path=/auth scoping** keeps the cookie out of every other request: the
credential only travels to register/login/refresh/logout/me, shrinking the
surface on which it could be logged, mirrored, or leaked.

## Consequences

- The XSS-vs-CSRF trade is explicit: we trade away automatic-attach CSRF risk
  (mitigated by SameSite=Lax) to eliminate JS-readable long-lived credentials
  (which have no equivalent mitigation short of not storing them client-side).
- **Documented limitation — cross-origin SPA:** SameSite=Lax only protects
  same-site requests. If a frontend is ever served from a *different origin*
  than the API (e.g. `app.example.com` calling `api.example.com`, or a dev
  server on another port), the browser treats those calls as cross-site and
  withholds a Lax cookie on POST. That deployment would need
  `SameSite=None; Secure` plus a real CSRF token, and CORS with credentials.
  M3 assumes a same-origin frontend; that constraint is recorded here so the
  next person doesn't hit it by surprise.
- Every refresh failure and logout clears the cookie (`MaxAge=-1`), so
  clients converge on "no session" rather than retrying a dead credential.
- The response JSON deliberately never contains the refresh token, so there
  is no second path by which script could capture it.
