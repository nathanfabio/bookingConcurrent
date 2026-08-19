-- +goose Up
-- Transactional outbox (CLAUDE.md §6). A BookingConfirmed event is written
-- HERE, in the SAME Postgres transaction as the booking row, and a relay in
-- cmd/worker publishes it to the broker afterwards. That is what makes
-- "booking committed" and "event published" impossible to silently diverge
-- on crash: the transaction either contains both rows or neither.
--
-- event_id is the consumer's idempotency key (at-least-once delivery means
-- duplicates WILL happen; consumers dedupe on it — M6).
CREATE TABLE outbox (
    id           BIGSERIAL PRIMARY KEY,
    event_id     UUID NOT NULL,
    event_type   TEXT NOT NULL,
    payload      JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NULL until the relay publishes; the partial index below keeps the
    -- pending set cheap to scan no matter how much history accumulates.
    published_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX outbox_event_id_unique ON outbox (event_id);
CREATE INDEX outbox_pending_idx ON outbox (created_at) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE outbox;
