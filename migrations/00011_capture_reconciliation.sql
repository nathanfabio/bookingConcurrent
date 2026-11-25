-- +goose Up
-- Older writers and historical payments have no durable hold deadline.
ALTER TABLE payments ADD COLUMN hold_expires_at timestamptz;

CREATE VIEW unbooked_captures AS
SELECT p.id AS payment_id, p.booking_session_id, p.gateway, p.intent_id,
       p.amount_cents, p.currency, p.updated_at AS captured_at, p.hold_expires_at,
       CASE WHEN p.hold_expires_at IS NULL THEN 'unknown_deadline'
            WHEN p.hold_expires_at <= CURRENT_TIMESTAMP THEN 'deadline_passed'
            ELSE 'before_deadline' END AS review_state
FROM payments p
WHERE p.status = 'captured'
  AND NOT EXISTS (
      SELECT 1 FROM confirmed_bookings b WHERE b.session_id = p.booking_session_id
  );

-- +goose Down
DROP VIEW unbooked_captures;
ALTER TABLE payments DROP COLUMN hold_expires_at;
