-- +goose Up
CREATE TABLE platform_pki (
    id         SMALLINT PRIMARY KEY CHECK (id = 1),
    ca_cert    BYTEA NOT NULL,   -- 根 CA 证书 DER
    ca_key_enc BYTEA NOT NULL,   -- secretbox 加密的 PKCS#8 私钥 DER
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE machine_enroll_tokens (
    id         BIGSERIAL PRIMARY KEY,
    machine_id BIGINT NOT NULL REFERENCES machines (id),
    token_hash BYTEA NOT NULL UNIQUE,  -- 令牌明文的 SHA-256
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    issued_by  BIGINT NOT NULL DEFAULT 0,  -- 签发管理员 id，0 为系统
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX machine_enroll_tokens_open_idx ON machine_enroll_tokens (machine_id) WHERE used_at IS NULL;

ALTER TABLE machines
    ADD COLUMN agent_version      TEXT NOT NULL DEFAULT '',
    ADD COLUMN last_metrics       JSONB,
    ADD COLUMN last_state_report  JSONB,
    ADD COLUMN agent_connected_at TIMESTAMPTZ,
    ADD COLUMN state_reported_at  TIMESTAMPTZ,
    ADD COLUMN agent_conn_id      TEXT;  -- 当前持有该机器流的网关连接 id（uuid）

ALTER TABLE agent_commands ADD COLUMN sent_at TIMESTAMPTZ;
CREATE INDEX agent_commands_sent_idx ON agent_commands (sent_at) WHERE status = 'sent';

-- +goose Down
DROP INDEX agent_commands_sent_idx;
ALTER TABLE agent_commands DROP COLUMN sent_at;
ALTER TABLE machines
    DROP COLUMN agent_conn_id, DROP COLUMN state_reported_at, DROP COLUMN agent_connected_at,
    DROP COLUMN last_state_report, DROP COLUMN last_metrics, DROP COLUMN agent_version;
DROP TABLE machine_enroll_tokens;
DROP TABLE platform_pki;
