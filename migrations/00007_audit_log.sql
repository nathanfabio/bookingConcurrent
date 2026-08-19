-- +goose Up
-- Audit log: an append-only trail of consequential actions (CLAUDE.md §2).
-- Rows are never updated or deleted; entity_id is TEXT because the entity
-- may be a booking (UUID), a session (opaque string), or something else.
CREATE TABLE audit_log (
    id            BIGSERIAL PRIMARY KEY,
    actor_user_id UUID,
    action        TEXT NOT NULL,
    entity_type   TEXT NOT NULL,
    entity_id     TEXT NOT NULL,
    details       JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_entity_idx ON audit_log (entity_type, entity_id);
CREATE INDEX audit_log_actor_idx ON audit_log (actor_user_id);

-- +goose Down
DROP TABLE audit_log;
