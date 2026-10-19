-- +goose Up
-- 计划 6：机组、预订（reserved 实例 + reserved lease）、批次、自动续费。
-- Down 有损：cancelled 实例映射为 failed；仍有 reserved（已付款未退）实例时 Down 直接拒绝，请先在后台取消预订。

CREATE TABLE machine_groups (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,            -- 对用户展示，1～32 字符
    region      TEXT NOT NULL DEFAULT '',        -- 展示用位置标签，≤ 32 字符
    description TEXT NOT NULL DEFAULT '',        -- ≤ 200 字符
    sort        INT  NOT NULL DEFAULT 0,
    visible     BOOLEAN NOT NULL DEFAULT true,   -- false：控制台不展示，仅后台可见与分配
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);

ALTER TABLE machines
    ADD COLUMN group_id     BIGINT REFERENCES machine_groups (id),   -- NULL = 未分组
    ADD COLUMN display_name TEXT NOT NULL DEFAULT '';                  -- 对用户展示名，≤ 32 字符
CREATE INDEX machines_group_idx ON machines (group_id, id);
CREATE UNIQUE INDEX machines_display_name_uniq ON machines (display_name) WHERE display_name <> '';

CREATE TABLE rental_batches (
    id              TEXT PRIMARY KEY,            -- 'bat-' + 12 位
    user_id         BIGINT NOT NULL REFERENCES users (id),
    count           INT NOT NULL CHECK (count BETWEEN 1 AND 8),
    idempotency_key TEXT,
    created_at      TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX rental_batches_user_idempotency_uniq ON rental_batches (user_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

ALTER TABLE orders
    ADD COLUMN batch_id TEXT REFERENCES rental_batches (id),
    ADD COLUMN quantity INT NOT NULL DEFAULT 1 CHECK (quantity BETWEEN 1 AND 90),  -- 租期单位数：时长 = duration × quantity
    ADD COLUMN auto     BOOLEAN NOT NULL DEFAULT false;                              -- 自动续费产生的续费单
CREATE INDEX orders_batch_idx ON orders (batch_id) WHERE batch_id IS NOT NULL;

ALTER TABLE instances DROP CONSTRAINT instances_state_check;
ALTER TABLE instances ADD CONSTRAINT instances_state_check
    CHECK (state IN ('reserved', 'creating', 'running', 'stopped', 'wiping', 'released', 'failed', 'cancelled'));
ALTER TABLE instances
    ADD COLUMN scheduled_start_at       TIMESTAMPTZ,                 -- 预订开始时刻；即时订单为 NULL
    ADD COLUMN group_id                 BIGINT REFERENCES machine_groups (id),  -- 下单时选择的机组（改派范围）；NULL = 不限
    ADD COLUMN batch_id                 TEXT REFERENCES rental_batches (id),
    ADD COLUMN auto_renew               BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN auto_renew_duration      TEXT NOT NULL DEFAULT '1m' CHECK (auto_renew_duration IN ('1d', '1w', '1m')),
    ADD COLUMN auto_renew_failed_at     TIMESTAMPTZ,                 -- 最近一次自动续费失败（成功后清空）
    ADD COLUMN auto_renew_failed_reason TEXT NOT NULL DEFAULT '',    -- insufficient_balance / renew_blocked
    ADD COLUMN reassign_count           INT NOT NULL DEFAULT 0,      -- 改派次数
    ADD COLUMN provisioned_at           TIMESTAMPTZ,                 -- 预订开通（reserved → creating）时刻；即时订单为 NULL
    ADD COLUMN cancelled_at             TIMESTAMPTZ;
CREATE INDEX instances_reserved_idx ON instances (scheduled_start_at) WHERE state = 'reserved';
CREATE INDEX instances_auto_renew_idx ON instances (expires_at) WHERE state = 'running' AND auto_renew;
CREATE INDEX instances_batch_idx ON instances (batch_id) WHERE batch_id IS NOT NULL;
CREATE INDEX instances_group_idx ON instances (group_id) WHERE group_id IS NOT NULL;

-- 先清掉 occupiedUntil 回退为 now 产生的空时段（isempty 的 range 没有 lower，用 created_at 代替），否则 CHECK 建不起来
UPDATE machine_leases SET period = tstzrange(created_at, created_at + interval '1 second'), status = 'ended' WHERE isempty(period);
CREATE INDEX machine_leases_machine_period_idx ON machine_leases (machine_id, lower(period)) WHERE status IN ('reserved', 'active');
CREATE INDEX machine_leases_reserved_idx ON machine_leases (lower(period)) WHERE status = 'reserved';
ALTER TABLE machine_leases ADD CONSTRAINT machine_leases_period_nonempty CHECK (NOT isempty(period) AND lower(period) < upper(period));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM instances WHERE state = 'reserved') THEN
        RAISE EXCEPTION '仍有未开通的预订（已付款未退），请先在后台取消预订再回滚';
    END IF;
END $$;
-- +goose StatementEnd
UPDATE machine_leases SET status = 'cancelled' WHERE instance_id IN (SELECT id FROM instances WHERE state = 'cancelled') AND status IN ('reserved', 'active');
UPDATE instances SET state = 'failed' WHERE state = 'cancelled';
ALTER TABLE machine_leases DROP CONSTRAINT machine_leases_period_nonempty;
DROP INDEX machine_leases_reserved_idx;
DROP INDEX machine_leases_machine_period_idx;
DROP INDEX instances_group_idx;
DROP INDEX instances_batch_idx;
DROP INDEX instances_auto_renew_idx;
DROP INDEX instances_reserved_idx;
ALTER TABLE instances
    DROP COLUMN cancelled_at, DROP COLUMN provisioned_at, DROP COLUMN reassign_count, DROP COLUMN auto_renew_failed_reason,
    DROP COLUMN auto_renew_failed_at, DROP COLUMN auto_renew_duration, DROP COLUMN auto_renew, DROP COLUMN batch_id,
    DROP COLUMN group_id, DROP COLUMN scheduled_start_at;
ALTER TABLE instances DROP CONSTRAINT instances_state_check;
ALTER TABLE instances ADD CONSTRAINT instances_state_check
    CHECK (state IN ('creating', 'running', 'stopped', 'wiping', 'released', 'failed'));
DROP INDEX orders_batch_idx;
ALTER TABLE orders DROP COLUMN auto, DROP COLUMN quantity, DROP COLUMN batch_id;
DROP INDEX machines_display_name_uniq;
DROP INDEX machines_group_idx;
ALTER TABLE machines DROP COLUMN display_name, DROP COLUMN group_id;
DROP TABLE rental_batches;
DROP TABLE machine_groups;
