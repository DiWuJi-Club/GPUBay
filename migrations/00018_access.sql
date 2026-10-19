-- +goose Up
CREATE TABLE ssh_keys (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users (id),
    name         TEXT NOT NULL,
    public_key   TEXT NOT NULL,            -- "<type> <base64>"，无注释
    fingerprint  TEXT NOT NULL,            -- "SHA256:…"
    last_used_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (user_id, fingerprint)
);
CREATE INDEX ssh_keys_user_idx ON ssh_keys (user_id, created_at);

CREATE TABLE instance_ports (
    id             BIGSERIAL PRIMARY KEY,
    instance_id    TEXT NOT NULL REFERENCES instances (id),
    port           INT NOT NULL CHECK (port BETWEEN 1 AND 65535 AND port <> 22),
    visibility     TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'public')),
    forced_private BOOLEAN NOT NULL DEFAULT false,   -- 后台强制私有后用户不能改回公开
    note           TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    UNIQUE (instance_id, port)
);

CREATE TABLE ssh_sessions (
    id           BIGSERIAL PRIMARY KEY,
    instance_id  TEXT NOT NULL REFERENCES instances (id),
    user_id      BIGINT NOT NULL REFERENCES users (id),
    machine_id   BIGINT NOT NULL REFERENCES machines (id),
    auth_method  TEXT NOT NULL CHECK (auth_method IN ('password', 'publickey')),
    key_id       BIGINT REFERENCES ssh_keys (id) ON DELETE SET NULL,
    client_ip    TEXT NOT NULL,
    bytes_up     BIGINT NOT NULL DEFAULT 0,
    bytes_down   BIGINT NOT NULL DEFAULT 0,
    started_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,   -- 持有副本每 60 秒刷新；清扫器收尾超过 5 分钟未刷新的行
    ended_at     TIMESTAMPTZ
);
CREATE INDEX ssh_sessions_instance_idx ON ssh_sessions (instance_id, started_at DESC);
CREATE INDEX ssh_sessions_open_idx ON ssh_sessions (last_seen_at) WHERE ended_at IS NULL;

ALTER TABLE instances
    ADD COLUMN password_auth    BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN ssh_host_pubkey  TEXT NOT NULL DEFAULT '',
    ADD COLUMN ssh_host_key_enc BYTEA;                 -- 存量实例为 NULL；释放后清空

ALTER TABLE platform_pki ADD COLUMN ssh_host_key_enc BYTEA;  -- 网关主机密钥

-- +goose Down
ALTER TABLE platform_pki DROP COLUMN ssh_host_key_enc;
ALTER TABLE instances DROP COLUMN ssh_host_key_enc, DROP COLUMN ssh_host_pubkey, DROP COLUMN password_auth;
DROP TABLE ssh_sessions;
DROP TABLE instance_ports;
DROP TABLE ssh_keys;
