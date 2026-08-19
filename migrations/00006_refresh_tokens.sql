-- +goose Up
-- Refresh tokens: rotating, opaque, stored HASHED (CLAUDE.md §3).
--
-- The server never stores the token itself: it stores SHA-256(token), so a
-- database read leaks nothing usable. Rotation works through family_id:
-- every refresh mints a new token in the same family and marks the old row
-- replaced. If a token that was ALREADY replaced is presented again, that
-- is token theft — the whole family gets revoked (revoke_family in M3).
CREATE TABLE refresh_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id   UUID NOT NULL,
    token_hash  TEXT NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    replaced_by UUID REFERENCES refresh_tokens (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX refresh_tokens_hash_unique ON refresh_tokens (token_hash);
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);

-- +goose Down
DROP TABLE refresh_tokens;
