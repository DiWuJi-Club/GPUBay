-- +goose Up
CREATE TABLE instances (
    id               TEXT PRIMARY KEY,
    user_id          BIGINT NOT NULL REFERENCES users (id),
    machine_id       BIGINT NOT NULL REFERENCES machines (id),
    product_id       BIGINT NOT NULL REFERENCES products (id),
    image_id         BIGINT NOT NULL REFERENCES images (id),
    state            TEXT NOT NULL
                     CHECK (state IN ('creating', 'running', 'stopped', 'wiping', 'released', 'failed')),
    pending_seconds  BIGINT NOT NULL DEFAULT 0,  -- 已付款但尚未开始计时的秒数
    expires_at       TIMESTAMPTZ,
    stopped_at       TIMESTAMPTZ,
    suspended_at     TIMESTAMPTZ,                -- 机器失联、暂停计时的开始时间
    paused_remaining BIGINT,                     -- 暂停时冻结的剩余秒数
    ssh_password_enc BYTEA NOT NULL,
    reminded_24h     BOOLEAN NOT NULL DEFAULT false,
    reminded_1h      BOOLEAN NOT NULL DEFAULT false,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX instances_user_idx ON instances (user_id, created_at DESC);
CREATE INDEX instances_state_idx ON instances (state, expires_at);
-- 一台机器同一时间只能被一个未结束的实例占用
CREATE UNIQUE INDEX instances_machine_active_uniq ON instances (machine_id)
    WHERE state IN ('creating', 'running', 'stopped', 'wiping');

CREATE TABLE instance_events (
    id          BIGSERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances (id),
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    at          TIMESTAMPTZ NOT NULL
);
CREATE INDEX instance_events_instance_idx ON instance_events (instance_id, id);

CREATE TABLE orders (
    id          TEXT PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users (id),
    kind        TEXT NOT NULL CHECK (kind IN ('new', 'renew')),
    instance_id TEXT NOT NULL REFERENCES instances (id),
    product_id  BIGINT NOT NULL REFERENCES products (id),
    duration    TEXT NOT NULL CHECK (duration IN ('1d', '1w', '1m')),
    amount      BIGINT NOT NULL CHECK (amount > 0),
    status      TEXT NOT NULL CHECK (status IN ('paid', 'refunded', 'partially_refunded')),
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX orders_user_idx ON orders (user_id, created_at DESC);
CREATE INDEX orders_instance_idx ON orders (instance_id);

-- 机器占用时段：同一台机器的 reserved/active 时段不得重叠（子项目 2 的档期预订基于此表）
CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE TABLE machine_leases (
    id          BIGSERIAL PRIMARY KEY,
    machine_id  BIGINT NOT NULL REFERENCES machines (id),
    instance_id TEXT NOT NULL REFERENCES instances (id),
    period      TSTZRANGE NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('reserved', 'active', 'ended', 'cancelled')),
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    EXCLUDE USING gist (machine_id WITH =, period WITH &&) WHERE (status IN ('reserved', 'active'))
);
CREATE INDEX machine_leases_instance_idx ON machine_leases (instance_id) WHERE status = 'active';

-- +goose Down
DROP TABLE machine_leases;
DROP TABLE orders;
DROP TABLE instance_events;
DROP TABLE instances;
