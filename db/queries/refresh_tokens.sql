-- Refresh-token queries (ADR 0004). The rotation state machine itself is
-- documented on auth.RefreshTokenStore.Rotate; these are the SQL building
-- blocks it composes inside a transaction.

-- name: CreateRefreshToken :one
-- The row ID is minted by the application (uuid v4), not by the database:
-- both store implementations must agree on who assigns identity, and the
-- in-memory fake has no gen_random_uuid().
INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetRefreshTokenByHash :one
SELECT * FROM refresh_tokens WHERE token_hash = $1;

-- name: GetRefreshTokenByHashForUpdate :one
-- Row lock for Rotate's critical section. MUST run inside a transaction:
-- outside one the FOR UPDATE lock evaporates with the statement and the
-- read-decide-write is no longer atomic.
SELECT * FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE;

-- name: MarkRefreshTokenReplaced :exec
UPDATE refresh_tokens SET replaced_by = $2 WHERE id = $1;

-- name: RevokeRefreshTokenFamily :exec
-- Theft detection and logout both flow through here. Idempotent: rows
-- already revoked keep their original revoked_at.
UPDATE refresh_tokens
SET revoked_at = now()
WHERE family_id = $1 AND revoked_at IS NULL;
