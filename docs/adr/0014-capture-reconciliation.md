# 0014. Reconcile captures without a committed booking

**Status** Accepted for the sandbox. The refund execution policy below is a
prerequisite for introducing a gateway that moves money.

## Context

Capture and booking confirmation commit in separate operations. A capture
can succeed just before the Redis hold expires. The buyer can also leave
checkout after paying, or a signed capture can arrive after expiry. Reusing
the seat must remain safe. The payment record must survive all three cases.

## Decision

Freeze the hold deadline alongside the price when creating a payment intent.
Migration 00011 exposes captured payments with no booking for the session
through the Postgres view `unbooked_captures`. A single statement sees one
consistent snapshot of the two durable tables. The view also covers payments
created before the migration. Their original deadlines are unknown.

The operations owner reviews expired and unknown-deadline entries. The
prescribed compensation for a verified capture with a definitively unbooked,
closed checkout is a full refund in the original currency. A live checkout
can still confirm. An unknown deadline, an unavailable dependency, or a
single report snapshot requires investigation. A cancelled sale with an
existing booking has its own lifecycle and falls outside this report.

## Before enabling refunds with a real provider

1. Serialize confirmation and the refund decision on the same durable
   payment row. Both paths must lock it and recheck state inside their
   transaction. A confirmation that won first ends reconciliation. If a
   refund wins first, mark the payment refunding and prevent confirmation.
   Migration 00012 implements this lock in a before-insert trigger, so
   even API binaries deployed before this migration participate. The trigger
   acquires the payment lock until the booking and outbox transaction ends.
   The refund function locks first and then checks the booking in a separate
   READ COMMITTED statement with a fresh snapshot.
2. Commit one full-refund request keyed uniquely by payment ID in that
   transaction. Migration 00012 implements the sandbox authorization and
   persists the full amount, currency and reviewer. The
   worker submits the request using a stable provider
   idempotency key derived from that ID. Duplicate reviews, timeouts,
   process restarts and webhook replays reuse the same request.
3. Record refunded only after provider confirmation. On an ambiguous
   result, query the provider for the original request and reconcile the
   result before retrying. Escalate unresolved discrepancies to the
   operations owner. Audit the decision, amount and provider reference.

## Consequences

The report costs one database query per inspection and needs no polling
worker. The sandbox keeps the captured record for inspection. Reading the
view has no side effects. Captures can appear briefly during normal checkout
and disappear on booking commit. Detection time depends on how frequently
an operator inspects the report. Migration 00013 gives the fake gateway a receipt ledger in Postgres.
It shares the local database for deployment convenience and commits its
acceptance independently from the booking system. A repeated submission
with matching terms retrieves the original receipt, including after a
process restart. Changed terms cause an error. A real provider requires
verification of its own key retention, lookup and failure behavior.

Polling favors requests with the oldest attempt, including never-attempted
requests first. Concurrent workers can submit the same key. Once a receipt
returns, the worker locks the payment and atomically records the receipt,
completion time and refunded state. A repeated completion verifies the
committed reference. Recovery therefore works on either side of the local
commit without holding a database lock across the provider call.
