-- Outbox queries (migrations/00004). The outbox is the transactional
-- bridge between "booking committed" and "event published": the
-- BookingConfirmed event is written in the SAME Postgres transaction as the
-- booking row (ADR 0003's commit point), and the relay in cmd/worker
-- publishes it to the broker afterwards (M6). Until then, rows simply
-- accumulate with published_at NULL — that buffering IS the point.

-- name: InsertOutboxEvent :exec
-- Written atomically with the booking row; the caller supplies event_id,
-- event_type, and the JSON payload.
INSERT INTO outbox (event_id, event_type, payload)
VALUES ($1, $2, $3);
