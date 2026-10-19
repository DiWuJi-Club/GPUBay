-- +goose Up
CREATE TABLE uploads (
    key          TEXT PRIMARY KEY,               -- yyyy/mm/<32 位随机>.<ext>，即本地磁盘上的相对路径
    user_id      BIGINT NOT NULL REFERENCES users (id),
    size         BIGINT NOT NULL,
    content_type TEXT NOT NULL,
    purpose      TEXT NOT NULL CHECK (purpose IN ('license')),
    created_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX uploads_user_idx ON uploads (user_id, created_at DESC);

-- +goose Down
DROP TABLE uploads;
