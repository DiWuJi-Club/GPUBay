-- +goose Up
CREATE TABLE audit_logs (
    id          BIGSERIAL PRIMARY KEY,
    actor_type  TEXT NOT NULL CHECK (actor_type IN ('admin', 'user', 'system')),
    actor_id    BIGINT NOT NULL DEFAULT 0,
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL DEFAULT '',
    target_id   TEXT NOT NULL DEFAULT '',
    diff        JSONB NOT NULL DEFAULT '{}',
    ip          TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX audit_logs_target_idx ON audit_logs (target_type, target_id);

-- +goose Down
DROP TABLE audit_logs;
