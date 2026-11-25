# 0013. Package API, worker and migrations as separate processes

**Status** Accepted

## Context

Host binaries support rapid local iteration. A deployment also needs a
repeatable build and a visible decision about when schema changes run.

## Decision

Use one multi-stage Dockerfile with three named final targets. Each final
image runs a single Go executable as a numeric non-root user. A common build
stage uses the repository module checksums and compiles the embedded SQL.
An optional Compose overlay runs the migration job after Postgres is healthy.
API and worker startup depends on its successful exit. Production application
mode disables automatic migrations and catalog seeding.

## Consequences

All targets share the toolchain and build cache. Editing one executable can
trigger compilation of all three. Migration status and failures have their
own exit codes and logs. Independent releases still require schema changes
compatible with any processes that remain running. An empty packaged
installation requires catalog provisioning before it can sell seats.
