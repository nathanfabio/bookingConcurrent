-- Outbox queries (migrations/00004). The outbox is the transactional
-- bridge between "booking committed" and "event published": the
-- BookingConfirmed event is written in the SAME Postgres transaction as the
-- booking row (ADR 0003's commit point), and the relay in cmd/worker
-- publishes it to the broker afterwards (ADR 0009's delivery semantics:
-- publish-then-mark, at-least-once, consumers dedupe on event_id).
--
-- Two writers, two readers, no overlap: the API's Confirm writes rows
-- (InsertOutboxEvent); only the relay reads pending rows and stamps
-- published_at. If the broker is down, rows simply accumulate with
-- published_at NULL — that buffering IS the point.

-- name: InsertOutboxEvent :exec
-- Written atomically with the booking row; the caller supplies event_id,
-- event_type, and the JSON payload.
INSERT INTO outbox (event_id, event_type, payload)
VALUES ($1, $2, $3);

-- name: ListPendingOutboxEvents :many
-- The relay's work queue. ORDER BY (created_at, id): created_at alone ties
-- within one transaction batch, and the BIGSERIAL id breaks ties into strict
-- FIFO. The partial index outbox_pending_idx (WHERE published_at IS NULL)
-- serves this scan no matter how much published history accumulates.
-- The BIGSERIAL id deliberately does not leave the repository: the relay's
-- contract is keyed by event_id alone.
SELECT event_id, event_type, payload
FROM outbox
WHERE published_at IS NULL
ORDER BY created_at, id
LIMIT $1;

-- name: MarkOutboxEventsPublished :exec
-- Stamped AFTER the broker confirmed the publishes (never before — marking
-- first would lose events on a crash between mark and publish, the unsafe
-- direction; republishing a confirmed event is merely a duplicate, which
-- consumers dedupe). The published_at IS NULL guard keeps a concurrent or
-- replayed relay from re-stamping history.
UPDATE outbox
SET published_at = now()
WHERE event_id = ANY($1::uuid[])
  AND published_at IS NULL;
