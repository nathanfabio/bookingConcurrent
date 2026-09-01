-- Booking queries. M2 introduced the minimum needed to prove the schema
-- constraints; M4 adds the confirm flow's full conflict handling plus the
-- seat-map reads.

-- name: InsertConfirmedBooking :one
-- Plain insert: the partial unique index on (screening, seat) WHERE
-- status='confirmed' is the arbiter — a second confirmed booking for the
-- same seat fails with a uniqueness violation (ADR 0006).
INSERT INTO confirmed_bookings (session_id, screening_id, seat_row, seat_number, user_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetBookingBySession :one
SELECT * FROM confirmed_bookings WHERE session_id = $1;

-- name: SetBookingStatus :one
UPDATE confirmed_bookings
SET status = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ExistsConfirmedSeat :one
-- Pre-hold validation (CLAUDE.md §4 "rejected before touching Redis") and
-- the phantom-hold defense (ADR 0006): one indexed lookup.
SELECT EXISTS (
    SELECT 1 FROM confirmed_bookings
    WHERE screening_id = $1
      AND seat_row = $2
      AND seat_number = $3
      AND status = 'confirmed'
);

-- name: ListConfirmedSeatsByScreening :many
-- The seat map's authoritative "booked" layer (ADR 0006: availability is
-- computed Postgres-first). Served by the confirmed_bookings_screening_idx
-- plus the partial seat index, so it stays an index scan as the table grows.
SELECT seat_row, seat_number
FROM confirmed_bookings
WHERE screening_id = $1
  AND status = 'confirmed'
ORDER BY seat_row, seat_number;
