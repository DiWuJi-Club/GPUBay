-- +goose Up
CREATE TABLE users (
    id         BIGSERIAL PRIMARY KEY,
    phone      TEXT NOT NULL UNIQUE,
    kind       TEXT NOT NULL DEFAULT 'personal' CHECK (kind IN ('personal', 'enterprise')),
    kyc_status TEXT NOT NULL DEFAULT 'none' CHECK (kyc_status IN ('none', 'pending', 'verified', 'rejected')),
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'banned')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE sms_codes (
    id         BIGSERIAL PRIMARY KEY,
    phone      TEXT NOT NULL,
    code_hash  TEXT NOT NULL,
    ip         TEXT NOT NULL,
    attempts   INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);
CREATE INDEX sms_codes_phone_idx ON sms_codes (phone, created_at DESC);
CREATE INDEX sms_codes_ip_idx ON sms_codes (ip, created_at DESC);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

-- +goose Down
DROP TABLE sessions;
DROP TABLE sms_codes;
DROP TABLE users;
