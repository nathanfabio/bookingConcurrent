-- +goose Up
ALTER TABLE refund_requests
    ADD COLUMN last_attempt_at timestamptz,
    ADD COLUMN completed_at timestamptz,
    ADD COLUMN provider_reference text UNIQUE,
    ADD CONSTRAINT refund_completion_pair CHECK ((completed_at IS NULL) = (provider_reference IS NULL));

CREATE INDEX pending_refund_order ON refund_requests
    (last_attempt_at NULLS FIRST, requested_at, payment_id) WHERE completed_at IS NULL;

-- The sandbox provider's own ledger. It commits independently of the local
-- request and deliberately has no foreign key back into the booking system.
CREATE TABLE sandbox_refund_receipts (
    idempotency_key uuid PRIMARY KEY,
    intent_id text UNIQUE NOT NULL,
    amount_cents integer NOT NULL CHECK (amount_cents >= 0),
    currency text NOT NULL,
    reference text UNIQUE NOT NULL DEFAULT ('re_fake_' || gen_random_uuid()::text),
    accepted_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- +goose Down
DROP TABLE sandbox_refund_receipts;
DROP INDEX pending_refund_order;
ALTER TABLE refund_requests DROP COLUMN provider_reference, DROP COLUMN completed_at, DROP COLUMN last_attempt_at;
