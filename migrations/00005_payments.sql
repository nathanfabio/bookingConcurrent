-- +goose Up
-- Payments: a state machine over gateway intents (CLAUDE.md §7).
--
-- Deliberately stores ONLY the intent ID, status, and amounts — never card
-- data or raw gateway secrets. status transitions are enforced in the
-- domain (internal/domain/payment), not just by the CHECK here: the CHECK
-- keeps bad writes out of the table, the state machine keeps bad sequences
-- out of the flow (e.g. duplicate webhooks become no-ops).
--
-- booking_session_id binds an intent to the hold session it pays for, so a
-- superseding retry can abandon the previous intent deterministically.
CREATE TABLE payments (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_session_id TEXT NOT NULL,
    gateway            TEXT NOT NULL,
    intent_id          TEXT NOT NULL,
    status             TEXT NOT NULL,
    amount_cents       INTEGER NOT NULL,
    currency           TEXT NOT NULL DEFAULT 'usd',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT payments_gateway_known CHECK (gateway IN ('fake', 'stripe')),
    CONSTRAINT payments_status_known CHECK (status IN
        ('intent', 'authorized', 'captured', 'refunding', 'refunded', 'failed')),
    CONSTRAINT payments_amount_nonnegative CHECK (amount_cents >= 0)
);

CREATE UNIQUE INDEX payments_intent_unique ON payments (intent_id);
CREATE INDEX payments_session_idx ON payments (booking_session_id);

-- +goose Down
DROP TABLE payments;
