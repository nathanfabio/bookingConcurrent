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
