-- +goose Up
-- Consumer-side idempotency (CLAUDE.md §6, ADR 0009). At-least-once delivery
-- means duplicates WILL arrive: the relay republishes after a crash between
-- broker-confirm and outbox mark, and the broker redelivers unacked messages
-- after a consumer restart. The BookingConfirmed consumer claims each event_id
-- here — INSERT .. ON CONFLICT DO NOTHING is the atomic check-and-set — and
-- applies its side effect only when the claim is new.
--
-- event_type is denormalized (the outbox has it too) because this table is
-- often the ONLY record of what a consumer actually saw; debugging "did we
-- process X?" should not require joining two tables.
--
-- Growth: one row per event, forever. Pruning old rows is an operational
-- concern recorded as an ADR 0009 deferral, not built now (CLAUDE.md §13).
CREATE TABLE processed_events (
    event_id     UUID PRIMARY KEY,
    event_type   TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE processed_events;
