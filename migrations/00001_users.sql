-- +goose Up
-- Users: the durable identity record (CLAUDE.md §3).
--
-- email is stored as plain TEXT with a functional unique index on
-- lower(email) rather than CITEXT: the index makes the case-insensitive
-- uniqueness rule visible in the schema, and it works without enabling an
-- extension.
CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL,
    -- bcrypt output. The hash is self-describing (algorithm + cost + salt),
    -- which is why the column is a plain string and rotation/verification
    -- need no extra metadata columns.
    password_hash TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT 'customer',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT users_email_nonempty CHECK (length(trim(email)) > 0),
    CONSTRAINT users_role_known CHECK (role IN ('customer', 'admin'))
);

CREATE UNIQUE INDEX users_email_unique ON users (lower(email));

-- +goose Down
DROP TABLE users;
