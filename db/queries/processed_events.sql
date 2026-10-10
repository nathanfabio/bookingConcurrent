-- Processed-event queries (migrations/00010). The claim that makes the
-- BookingConfirmed consumer idempotent under at-least-once delivery
-- (CLAUDE.md §6, ADR 0009): the consumer INSERTs the event_id BEFORE applying
-- the side effect; a conflict means the event was already processed and this
-- delivery is a duplicate to skip.

-- name: ClaimProcessedEvent :one
-- Atomic check-and-set. Returns the event_id row when THIS caller won the
-- claim; on conflict (already processed) it returns nothing, which surfaces
-- as pgx.ErrNoRows and maps to (claimed=false, nil error) in the repository.
INSERT INTO processed_events (event_id, event_type)
VALUES ($1, $2)
ON CONFLICT (event_id) DO NOTHING
RETURNING event_id;
