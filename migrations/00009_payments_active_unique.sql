-- +goose Up
-- One ACTIVE payment intent per hold session (M5, ADR 0008).
--
-- Migration 00005 left booking_session_id non-unique so "a superseding retry
-- can abandon the previous intent". This index refines that intent (pun
-- intended) into an enforceable rule: at most one non-terminal payment per
-- session, and superseding is an explicit two-step — fail the old row, then
-- insert the new one. Without it, a double-clicked "pay" button or two
-- racing CreateIntent calls would leave two live intents for one seat and
-- the confirm gate's EXISTS(captured) read could not tell which charge the
-- buyer meant.
--
-- 'failed' and 'refunded' are excluded (a dead intent must not block its
-- replacement). 'refunding' deliberately still BLOCKS: a refund in flight
-- must not race a fresh charge for the same seat. A future refunds
-- milestone may revisit the predicate — that is exactly what a partial
-- index migration is cheap for.
CREATE UNIQUE INDEX payments_active_session_unique
    ON payments (booking_session_id)
    WHERE status NOT IN ('failed', 'refunded');

-- +goose Down
DROP INDEX payments_active_session_unique;
