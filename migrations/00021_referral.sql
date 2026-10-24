-- +goose Up
-- 计划 8：邀请返利。费率版本只增不改；返利 / 扣回是两种新的钱包流水类型；user_devices 与 users.last_login_* 供风险标记；kyc_records.fingerprint 供同身份判定。
-- Down 在仍有 referral_rebate / referral_clawback 流水时拒绝（流水只追加，改类型就是篡改账目）。

CREATE TABLE referral_rate_versions (
    id                   BIGSERIAL PRIMARY KEY,
    rate_bp              INT    NOT NULL CHECK (rate_bp BETWEEN 0 AND 5000),      -- 万分比：1000 = 10%
    max_per_invitee      BIGINT NOT NULL CHECK (max_per_invitee >= 0),           -- 单个被邀请人累计返利上限（分）
    max_per_inviter_30d  BIGINT NOT NULL CHECK (max_per_inviter_30d >= 0),       -- 单个邀请人 30 天发放上限（分）
    hold_days            INT    NOT NULL CHECK (hold_days BETWEEN 0 AND 90),      -- 发放延迟
    period_days          INT    NOT NULL CHECK (period_days BETWEEN 1 AND 3650),  -- 返利期（绑定起）
    effective_from       TIMESTAMPTZ NOT NULL,
    note                 TEXT NOT NULL DEFAULT '',
    created_by           BIGINT,                                                  -- 管理员；迁移写入的首版为 NULL
    created_at           TIMESTAMPTZ NOT NULL
);
CREATE INDEX referral_rate_versions_effective_idx ON referral_rate_versions (effective_from DESC);
INSERT INTO referral_rate_versions (rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, note, created_at)
VALUES (1000, 50000, 500000, 7, 365, '2000-01-01', '初始版本（规格 R1～R3、R5）', now());

CREATE TABLE referral_accounts (
    user_id       BIGINT PRIMARY KEY REFERENCES users (id),
    code          TEXT NOT NULL UNIQUE,                                   -- 8 位 Crockford Base32（无 I L O U）
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen')),
    frozen_reason TEXT NOT NULL DEFAULT '',
    frozen_by     BIGINT,
    frozen_at     TIMESTAMPTZ,
    debt          BIGINT NOT NULL DEFAULT 0 CHECK (debt >= 0),            -- 扣回时余额不足形成的欠账（分）
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE referral_bindings (
    id              BIGSERIAL PRIMARY KEY,
    inviter_id      BIGINT NOT NULL REFERENCES users (id),
    invitee_id      BIGINT NOT NULL UNIQUE REFERENCES users (id),
    code            TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'held', 'revoked')),
    risk_flags      TEXT[] NOT NULL DEFAULT '{}',                         -- same_ip / same_device / burst / same_identity
    hold_reason     TEXT NOT NULL DEFAULT '',
    rate_version_id BIGINT NOT NULL REFERENCES referral_rate_versions (id),
    bound_at        TIMESTAMPTZ NOT NULL,
    rebate_until    TIMESTAMPTZ NOT NULL,                                 -- bound_at + period_days（按绑定时版本）
    ip              TEXT NOT NULL DEFAULT '',
    device_id       TEXT NOT NULL DEFAULT '',
    user_agent      TEXT NOT NULL DEFAULT '',                             -- ≤ 200 字符
    reviewed_by     BIGINT,
    reviewed_at     TIMESTAMPTZ,
    review_note     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX referral_bindings_inviter_idx ON referral_bindings (inviter_id, bound_at DESC);
CREATE INDEX referral_bindings_status_idx  ON referral_bindings (status, bound_at DESC) WHERE status = 'held';

CREATE TABLE referral_rewards (
    order_id         TEXT PRIMARY KEY,                                    -- 不加外键：referral 不依赖 rental 的表
    binding_id       BIGINT NOT NULL REFERENCES referral_bindings (id),
    inviter_id       BIGINT NOT NULL REFERENCES users (id),
    invitee_id       BIGINT NOT NULL REFERENCES users (id),
    rate_version_id  BIGINT NOT NULL REFERENCES referral_rate_versions (id),
    rate_bp          INT    NOT NULL,
    order_amount     BIGINT NOT NULL CHECK (order_amount > 0),            -- order_pay 流水的 |amount|
    refunded_amount  BIGINT NOT NULL DEFAULT 0 CHECK (refunded_amount >= 0 AND refunded_amount <= order_amount),
    reward_cap       BIGINT NOT NULL CHECK (reward_cap >= 0),             -- 计提时两项上限允许的最大返利
    reward           BIGINT NOT NULL CHECK (reward >= 0),                 -- 当前应得 = min(reward_cap, floor((order_amount − refunded_amount) × rate_bp / 10000))
    status           TEXT NOT NULL CHECK (status IN ('pending', 'settled', 'void')),
    settle_after     TIMESTAMPTZ NOT NULL,
    settled_amount   BIGINT NOT NULL DEFAULT 0 CHECK (settled_amount >= 0),  -- 发放时的应得（含被欠账抵掉的部分）
    clawed_amount    BIGINT NOT NULL DEFAULT 0 CHECK (clawed_amount >= 0 AND clawed_amount <= settled_amount),
    settled_at       TIMESTAMPTZ,
    paid_at          TIMESTAMPTZ NOT NULL,                                -- order_pay 流水的 created_at
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX referral_rewards_pending_idx ON referral_rewards (settle_after) WHERE status = 'pending';
CREATE INDEX referral_rewards_inviter_idx ON referral_rewards (inviter_id, paid_at DESC);
CREATE INDEX referral_rewards_binding_idx ON referral_rewards (binding_id);

CREATE TABLE referral_events (
    id               BIGSERIAL PRIMARY KEY,
    order_id         TEXT NOT NULL,
    binding_id       BIGINT NOT NULL REFERENCES referral_bindings (id),
    kind             TEXT NOT NULL CHECK (kind IN ('accrue', 'refund', 'settle', 'clawback', 'debt_add', 'debt_offset', 'void', 'capped')),
    source_ledger_id BIGINT,                                              -- accrue / refund 来自哪条 wallet_ledger
    wallet_ledger_id BIGINT,                                              -- settle / clawback 写出的 wallet_ledger
    amount           BIGINT NOT NULL,
    detail           TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX referral_events_source_uniq ON referral_events (source_ledger_id) WHERE source_ledger_id IS NOT NULL;
CREATE INDEX referral_events_order_idx ON referral_events (order_id, id);

CREATE TABLE referral_state (
    id        INT PRIMARY KEY CHECK (id = 1),
    watermark TIMESTAMPTZ NOT NULL
);
-- 不能用 now()：测试与 e2e 用假时钟，流水 created_at 可能早于真实时间；首轮扫描 JOIN referral_bindings 为空，代价可忽略
INSERT INTO referral_state VALUES (1, '2000-01-01');

ALTER TABLE wallet_ledger DROP CONSTRAINT wallet_ledger_kind_check;
ALTER TABLE wallet_ledger ADD CONSTRAINT wallet_ledger_kind_check
    CHECK (kind IN ('recharge', 'admin_adjust', 'order_pay', 'refund', 'referral_rebate', 'referral_clawback'));
CREATE INDEX wallet_ledger_order_scan_idx ON wallet_ledger (created_at, id) WHERE ref_type = 'order';

ALTER TABLE users ADD COLUMN last_login_ip TEXT NOT NULL DEFAULT '', ADD COLUMN last_login_at TIMESTAMPTZ;
CREATE TABLE user_devices (
    user_id       BIGINT NOT NULL REFERENCES users (id),
    device_id     TEXT   NOT NULL,                                        -- 控制台生成的 UUID（localStorage）
    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, device_id)
);
CREATE INDEX user_devices_device_idx ON user_devices (device_id);
ALTER TABLE kyc_records ADD COLUMN fingerprint TEXT NOT NULL DEFAULT '';  -- HMAC，只对 verified 记录写
CREATE INDEX kyc_records_fingerprint_idx ON kyc_records (fingerprint) WHERE fingerprint <> '';

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM wallet_ledger WHERE kind IN ('referral_rebate', 'referral_clawback')) THEN
        RAISE EXCEPTION '仍有邀请返利流水，不能回滚';
    END IF;
END $$;
-- +goose StatementEnd
DROP INDEX kyc_records_fingerprint_idx;
ALTER TABLE kyc_records DROP COLUMN fingerprint;
DROP TABLE user_devices;
ALTER TABLE users DROP COLUMN last_login_at, DROP COLUMN last_login_ip;
DROP INDEX wallet_ledger_order_scan_idx;
ALTER TABLE wallet_ledger DROP CONSTRAINT wallet_ledger_kind_check;
ALTER TABLE wallet_ledger ADD CONSTRAINT wallet_ledger_kind_check
    CHECK (kind IN ('recharge', 'admin_adjust', 'order_pay', 'refund'));
DROP TABLE referral_state;
DROP TABLE referral_events;
DROP TABLE referral_rewards;
DROP TABLE referral_bindings;
DROP TABLE referral_accounts;
DROP TABLE referral_rate_versions;
