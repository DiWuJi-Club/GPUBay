-- +goose Up
CREATE TABLE machines (
    id               BIGSERIAL PRIMARY KEY,
    serial           TEXT NOT NULL UNIQUE,
    rack             TEXT NOT NULL DEFAULT '',
    slot             TEXT NOT NULL DEFAULT '',
    lan_ip           TEXT NOT NULL,
    product_id       BIGINT NOT NULL REFERENCES products (id),
    state            TEXT NOT NULL DEFAULT 'maintenance'
                     CHECK (state IN ('idle', 'allocated', 'maintenance', 'faulty')),
    agent_online     BOOLEAN NOT NULL DEFAULT false,
    last_seen_at     TIMESTAMPTZ,
    cert_fingerprint TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX machines_alloc_idx ON machines (product_id, id) WHERE state = 'idle' AND agent_online;

CREATE TABLE agent_commands (
    id          TEXT PRIMARY KEY,
    machine_id  BIGINT NOT NULL REFERENCES machines (id),
    instance_id TEXT NOT NULL DEFAULT '',
    type        TEXT NOT NULL,
    payload     JSONB NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'done', 'failed')),
    result      TEXT NOT NULL DEFAULT '',
    attempts    INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX agent_commands_open_idx ON agent_commands (machine_id, created_at) WHERE status IN ('pending', 'sent');
CREATE INDEX agent_commands_instance_idx ON agent_commands (instance_id) WHERE status IN ('pending', 'sent');

CREATE TABLE alerts (
    id          BIGSERIAL PRIMARY KEY,
    level       TEXT NOT NULL CHECK (level IN ('warning', 'critical')),
    kind        TEXT NOT NULL,
    machine_id  BIGINT REFERENCES machines (id),
    instance_id TEXT NOT NULL DEFAULT '',
    message     TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ
);
CREATE INDEX alerts_open_idx ON alerts (created_at DESC) WHERE resolved_at IS NULL;

-- +goose Down
DROP TABLE alerts;
DROP TABLE agent_commands;
DROP TABLE machines;
