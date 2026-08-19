-- +goose Up
-- Movies are the catalog; screenings are the bookable showings. Holds and
-- bookings are scoped to a SCREENING, not a movie: seat A1 at Tuesday's
-- showing and seat A1 at Friday's showing are independent (see ADR 0002
-- key scheme and docs/api.md).
CREATE TABLE movies (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title            TEXT NOT NULL,
    synopsis         TEXT NOT NULL DEFAULT '',
    duration_minutes INTEGER NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT movies_title_nonempty CHECK (length(trim(title)) > 0),
    CONSTRAINT movies_duration_positive CHECK (duration_minutes > 0)
);

CREATE TABLE screenings (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    movie_id        UUID NOT NULL REFERENCES movies (id),
    starts_at       TIMESTAMPTZ NOT NULL,
    -- Explicit row labels ({A,B,C,...}) instead of a row count: real venues
    -- skip letters (no row I) and sometimes use multi-letter rows, so the
    -- geometry is stored, not derived.
    rows            TEXT[] NOT NULL,
    seats_per_row   INTEGER NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT screenings_rows_nonempty CHECK (array_length(rows, 1) > 0),
    CONSTRAINT screenings_seats_positive CHECK (seats_per_row > 0)
);

CREATE INDEX screenings_movie_id_idx ON screenings (movie_id);
CREATE INDEX screenings_starts_at_idx ON screenings (starts_at);

-- +goose Down
DROP TABLE screenings;
DROP TABLE movies;
