-- +goose Up
CREATE TABLE admins (
    id              BIGSERIAL PRIMARY KEY,
    username        TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    totp_secret_enc BYTEA,                    -- AES-GCM 加密的 TOTP 密钥；未绑定或已重置时为 NULL
    totp_confirmed  BOOLEAN NOT NULL DEFAULT false,
    role            TEXT NOT NULL CHECK (role IN ('super', 'ops', 'finance')),
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    failed_attempts INT NOT NULL DEFAULT 0,
    locked_until    TIMESTAMPTZ,
    last_login_at   TIMESTAMPTZ,
    last_login_ip   TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE admin_recovery_codes (
    id        BIGSERIAL PRIMARY KEY,
    admin_id  BIGINT NOT NULL REFERENCES admins (id),
    code_hash TEXT NOT NULL,
    used_at   TIMESTAMPTZ
);
CREATE INDEX admin_recovery_codes_admin_idx ON admin_recovery_codes (admin_id);

-- stage=password_ok：密码已通过、等待动态码（5 分钟）；stage=active：已登录（绝对 12 小时，空闲 2 小时）
CREATE TABLE admin_sessions (
    token_hash   TEXT PRIMARY KEY,
    admin_id     BIGINT NOT NULL REFERENCES admins (id),
    stage        TEXT NOT NULL CHECK (stage IN ('password_ok', 'active')),
    ip           TEXT NOT NULL DEFAULT '',
    stepup_at    TIMESTAMPTZ,                 -- 最近一次高风险操作二次验证通过的时间
    created_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX admin_sessions_admin_idx ON admin_sessions (admin_id);

-- 已使用的动态码时间片，防止同一动态码被重复使用；只保留最近 10 分钟
CREATE TABLE admin_totp_used (
    admin_id BIGINT NOT NULL REFERENCES admins (id),
    step     BIGINT NOT NULL,
    used_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (admin_id, step)
);

-- +goose Down
DROP TABLE admin_totp_used;
DROP TABLE admin_sessions;
DROP TABLE admin_recovery_codes;
DROP TABLE admins;
