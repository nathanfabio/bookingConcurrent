-- +goose Up
-- Confirmed bookings: the permanent, append-only sales record (CLAUDE.md §2).
--
-- Two constraints carry the consistency model (ADR 0006):
--
-- 1. confirmed_bookings_seat_unique — the arbiter of last resort. At most
--    one CONFIRMED booking may exist per (screening, seat). It is PARTIAL:
--    cancelled rows fall out of the index, so refunding/cancelling a
--    booking makes the seat bookable again instead of bricking it forever.
--
-- 2. confirmed_bookings_session_unique — one booking per hold session.
--    This is what makes confirm idempotent: on a conflict, the use case
--    looks up the existing row by session_id and distinguishes "I already
--    confirmed this" from "someone else won the seat".
CREATE TABLE confirmed_bookings (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id   TEXT NOT NULL,
    screening_id UUID NOT NULL REFERENCES screenings (id),
    seat_row     TEXT NOT NULL,
    seat_number  INTEGER NOT NULL,
    user_id      UUID NOT NULL REFERENCES users (id),
    status       TEXT NOT NULL DEFAULT 'confirmed',
    confirmed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT confirmed_bookings_seat_valid CHECK (seat_number > 0 AND length(trim(seat_row)) > 0),
    CONSTRAINT confirmed_bookings_status_known CHECK (status IN ('confirmed', 'cancelled'))
);

CREATE UNIQUE INDEX confirmed_bookings_seat_unique
    ON confirmed_bookings (screening_id, seat_row, seat_number)
    WHERE status = 'confirmed';

CREATE UNIQUE INDEX confirmed_bookings_session_unique
    ON confirmed_bookings (session_id);

CREATE INDEX confirmed_bookings_user_idx ON confirmed_bookings (user_id);
CREATE INDEX confirmed_bookings_screening_idx ON confirmed_bookings (screening_id);

-- +goose Down
DROP TABLE confirmed_bookings;
