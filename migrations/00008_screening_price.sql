-- +goose Up
-- Ticket pricing (M5, CLAUDE.md §7): payments.amount_cents needs a source of
-- truth, and per-screening pricing is the realistic one (matinees vs
-- premieres). The column is added with a DEFAULT so the ALTER can backfill
-- existing rows in one statement, then the DEFAULT is DROPPED: an implicit
-- price silently applied to a new screening is exactly the "silent default
-- for a deployment-specific value" config.go refuses to allow, expressed as
-- schema. Inserts must state the price from M5 on.
ALTER TABLE screenings ADD COLUMN price_cents INTEGER NOT NULL DEFAULT 1500;
ALTER TABLE screenings ALTER COLUMN price_cents DROP DEFAULT;

-- > 0, not >= 0: a zero-amount payment intent is a gateway edge case with
-- no business meaning for a cinema ticket (see domain/payment.Payment).
ALTER TABLE screenings ADD CONSTRAINT screenings_price_positive CHECK (price_cents > 0);

-- +goose Down
ALTER TABLE screenings DROP CONSTRAINT screenings_price_positive;
ALTER TABLE screenings DROP COLUMN price_cents;
