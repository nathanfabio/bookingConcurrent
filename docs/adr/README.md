# ADR Index

Every non-obvious architectural decision in this repo gets one ADR
(CLAUDE.md §12). Format: Context / Decision / Consequences, kept short.
A superseded ADR keeps its file and gains a "Status: superseded by NNNN" line —
the history is part of the learning value.

| # | Title | Status |
|---|-------|--------|
| 0001 | [Hexagonal architecture, modular monolith, consumer-owned ports](0001-hexagonal-architecture.md) | Accepted |
| 0002 | [Hold atomicity: Lua scripts, hold tokens, single-instance Redis](0002-hold-atomicity-lua.md) | Accepted |
| 0003 | [Hybrid persistence: Redis for holds, Postgres for truth](0003-hybrid-persistence.md) | Accepted |
| 0004 | [Authentication: short-lived JWT access tokens + rotating refresh tokens](0004-jwt-and-refresh-rotation.md) | Accepted |
| 0005 | [Password hashing: argon2id with explicit, reasoned parameters](0005-argon2id-password-hashing.md) | Accepted |
| 0007 | [Refresh-token transport: httpOnly cookie, access token in JSON](0007-refresh-cookie-transport.md) | Accepted |

**Numbering note:** 0006 is intentionally absent. It is already cited in code
and migrations as the *booking consistency model* (which Postgres side wins,
phantom-hold defense) and will be written when that slice lands (M4/M5). The
gap is reserved, not a mistake.
