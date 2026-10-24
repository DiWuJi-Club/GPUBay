-- +goose Up
-- 计划 9：Token 商城。系统账号（users.system）与系统实例（instances.system）、Token 服务池（machine_groups.purpose='llm'）、
-- 内部应用（apps.internal）、模型 / 节点 / 账户 / API Key / 结算 / 用量明细 / 按天汇总、钱包流水类型 llm_pay。
-- Down 在以下情况拒绝：仍有 Token 消费流水（wallet_ledger kind='llm_pay'，流水只追加）；仍有非终态节点（请先在后台下线）。
-- Down 保留系统账号行与池机组行（前者仍无法登录，后者变成普通不可见机组）；因此 Up 对这两行按唯一键 upsert，往返可重放。

-- 系统账号与系统实例
ALTER TABLE users ADD COLUMN system BOOLEAN NOT NULL DEFAULT false;      -- 平台内部账号：不能登录、不在用户列表默认筛选里
-- phone 不满足 ^1[3-9]\d{9}$，任何登录路径都到不了它
INSERT INTO users (phone, kind, kyc_status, status, system, created_at, updated_at)
VALUES ('system:llm', 'personal', 'verified', 'active', true, now(), now())
ON CONFLICT (phone) DO UPDATE SET system = true, kyc_status = 'verified', status = 'active', updated_at = now();
ALTER TABLE instances ADD COLUMN system BOOLEAN NOT NULL DEFAULT false;  -- 平台节点实例：无订单、expires_at NULL、不到期不提醒不自动续费
CREATE INDEX instances_system_live_idx ON instances (machine_id) WHERE system AND state IN ('creating', 'running', 'stopped', 'wiping');

-- 机器池：复用机组
ALTER TABLE machine_groups ADD COLUMN purpose TEXT NOT NULL DEFAULT 'rental' CHECK (purpose IN ('rental', 'llm'));
INSERT INTO machine_groups (name, region, description, sort, visible, purpose, created_at, updated_at)
VALUES ('Token 服务池', '', '平台自用：Token 商城推理节点所在机器，不对外出租', 1000, false, 'llm', now(), now())
ON CONFLICT (name) DO UPDATE SET purpose = 'llm', visible = false, updated_at = now();
CREATE UNIQUE INDEX machine_groups_llm_uniq ON machine_groups (purpose) WHERE purpose = 'llm';   -- 只有一个池

-- 内部应用（控制台不展示）
ALTER TABLE apps ADD COLUMN internal BOOLEAN NOT NULL DEFAULT false;

-- 模型目录
CREATE TABLE llm_models (
    id                 BIGSERIAL PRIMARY KEY,
    slug               TEXT NOT NULL UNIQUE,              -- 用户请求里的 model，^[a-z0-9][a-z0-9.-]{1,63}$；也是节点的 served-model-name
    name               TEXT NOT NULL,                     -- 展示名，1～64
    description        TEXT NOT NULL DEFAULT '',          -- ≤ 2000
    kind               TEXT NOT NULL CHECK (kind IN ('chat', 'embedding')),
    source             TEXT NOT NULL DEFAULT 'modelscope' CHECK (source IN ('modelscope', 'hf-mirror')),
    source_id          TEXT NOT NULL,                     -- ModelScope / HF 模型 ID，如 Qwen/Qwen3-32B
    image_id           BIGINT NOT NULL REFERENCES images (id),          -- 节点实例镜像（LLM Serving）
    app_version_id     BIGINT REFERENCES app_versions (id),             -- vllm-node 版本；NULL = 建节点时取最新 published
    context_len        INT  NOT NULL CHECK (context_len BETWEEN 1024 AND 1048576),   -- MAX_MODEL_LEN
    max_output_tokens  INT  NOT NULL DEFAULT 8192 CHECK (max_output_tokens BETWEEN 1 AND 262144),
    default_max_tokens INT  NOT NULL DEFAULT 4096,        -- 请求未带 max_tokens 时的预算估算值（不改写请求）
    input_price        BIGINT NOT NULL CHECK (input_price >= 0),        -- 分 / 百万 token
    output_price       BIGINT NOT NULL CHECK (output_price >= 0),
    node_env           JSONB NOT NULL DEFAULT '{}',       -- 传给 vllm-node 的附加变量（GPU_MEMORY_UTILIZATION、MAX_NUM_SEQS、QUANTIZATION、REASONING_PARSER、TOOL_CALL_PARSER）
    replicas           INT  NOT NULL DEFAULT 0 CHECK (replicas BETWEEN 0 AND 200),
    max_concurrency    INT  NOT NULL DEFAULT 32 CHECK (max_concurrency BETWEEN 1 AND 512),
    features           TEXT[] NOT NULL DEFAULT '{}',      -- 展示标签：tools / reasoning / vision / long-context
    status             TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'online', 'offline')),
    sort               INT  NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL
);

-- 节点
CREATE TABLE llm_nodes (
    id                TEXT PRIMARY KEY,                   -- 'node-' + 12 位 [a-z0-9]
    model_id          BIGINT NOT NULL REFERENCES llm_models (id),
    machine_id        BIGINT NOT NULL REFERENCES machines (id),
    instance_id       TEXT REFERENCES instances (id),     -- PlaceSystem 之后非空
    deployment_id     TEXT REFERENCES app_deployments (id),   -- appdeploy.Create 之后非空
    state             TEXT NOT NULL CHECK (state IN ('provisioning', 'ready', 'unhealthy', 'draining', 'removing', 'removed', 'failed')),
    api_key_enc       BYTEA NOT NULL,                     -- 节点 vLLM --api-key，secretbox
    probe_fail_count  INT  NOT NULL DEFAULT 0,
    rebuild_count     INT  NOT NULL DEFAULT 0,
    unhealthy_since   TIMESTAMPTZ,
    drain_since       TIMESTAMPTZ,
    last_probe_at     TIMESTAMPTZ,
    last_ready_at     TIMESTAMPTZ,
    error             TEXT NOT NULL DEFAULT '',           -- 最近错误（可展示，≤ 200）
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX llm_nodes_machine_live_uniq ON llm_nodes (machine_id) WHERE state NOT IN ('removed', 'failed');
CREATE INDEX llm_nodes_model_idx ON llm_nodes (model_id, state);

CREATE TABLE llm_node_events (
    id         BIGSERIAL PRIMARY KEY,
    node_id    TEXT NOT NULL REFERENCES llm_nodes (id),
    at         TIMESTAMPTZ NOT NULL,
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX llm_node_events_node_idx ON llm_node_events (node_id, id);

-- 用户账户（有 Key 或有用量才有行）
CREATE TABLE llm_accounts (
    user_id           BIGINT PRIMARY KEY REFERENCES users (id),
    unsettled_micro   BIGINT NOT NULL DEFAULT 0 CHECK (unsettled_micro >= 0),   -- Σ 未结算 cost_micro（与 llm_usage 一致，§14 不变量 1）
    carry_micro       BIGINT NOT NULL DEFAULT 0 CHECK (carry_micro BETWEEN 0 AND 999999),   -- 结算余数（微分）
    debt              BIGINT NOT NULL DEFAULT 0 CHECK (debt >= 0),              -- 欠费（分）
    rpm_limit         INT,                               -- NULL = 默认 60
    tpm_limit         INT,                               -- NULL = 默认 200000
    concurrency_limit INT,                               -- NULL = 默认 8
    blocked           BOOLEAN NOT NULL DEFAULT false,    -- 后台停用 Token 服务
    blocked_reason    TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);

CREATE TABLE llm_api_keys (
    id               BIGSERIAL PRIMARY KEY,
    user_id          BIGINT NOT NULL REFERENCES users (id),
    name             TEXT NOT NULL,                      -- 1～32
    key_hash         TEXT NOT NULL UNIQUE,               -- sha256 十六进制
    key_hint         TEXT NOT NULL,                      -- 'sk-spark-…' + 末 4 位
    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'revoked')),
    model_slugs      TEXT[] NOT NULL DEFAULT '{}',       -- 空 = 全部在线模型
    monthly_cap      BIGINT,                             -- 分；NULL = 不限
    month_ym         TEXT NOT NULL DEFAULT '',           -- 月消费计数所属月份（Asia/Shanghai，'2026-10'）
    month_cost_micro BIGINT NOT NULL DEFAULT 0,
    expires_at       TIMESTAMPTZ,
    last_used_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    revoked_at       TIMESTAMPTZ
);
CREATE INDEX llm_api_keys_user_idx ON llm_api_keys (user_id, created_at DESC);

-- 结算（一行 = 一条 llm_pay 流水，或金额为 0 的无流水结算）
CREATE TABLE llm_settlements (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT NOT NULL REFERENCES users (id),
    requests          INT    NOT NULL,
    prompt_tokens     BIGINT NOT NULL,
    completion_tokens BIGINT NOT NULL,
    cost_micro        BIGINT NOT NULL,                   -- 本次纳入的用量合计（微分）
    usage_amount      BIGINT NOT NULL CHECK (usage_amount >= 0),   -- 由用量折出的分 = floor((cost_micro + carry_before) / 1e6)
    debt_amount       BIGINT NOT NULL DEFAULT 0 CHECK (debt_amount >= 0),   -- 并入本次的历史欠费
    charged           BIGINT NOT NULL CHECK (charged >= 0),         -- 实际扣到（≤ usage_amount + debt_amount）
    wallet_ledger_id  BIGINT,                            -- charged = 0 时为 NULL
    period_from       TIMESTAMPTZ,                       -- 纳入用量的最早 / 最晚 created_at；纯扣欠费时为 NULL
    period_to         TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX llm_settlements_user_idx ON llm_settlements (user_id, id DESC);

-- 用量明细（不存内容）
CREATE TABLE llm_usage (
    id                BIGSERIAL PRIMARY KEY,
    request_id        TEXT NOT NULL UNIQUE,              -- 'req-' + 16 位；响应头 X-Request-Id
    user_id           BIGINT NOT NULL REFERENCES users (id),
    key_id            BIGINT NOT NULL REFERENCES llm_api_keys (id),
    model_id          BIGINT NOT NULL REFERENCES llm_models (id),
    node_id           TEXT REFERENCES llm_nodes (id),    -- 失败转移后为最终节点；未选到节点为 NULL
    endpoint          TEXT NOT NULL CHECK (endpoint IN ('chat', 'completions', 'embeddings')),
    stream            BOOLEAN NOT NULL,
    prompt_tokens     INT NOT NULL DEFAULT 0,
    completion_tokens INT NOT NULL DEFAULT 0,
    estimated         BOOLEAN NOT NULL DEFAULT false,
    input_price       BIGINT NOT NULL,                   -- 快照（分 / 百万）
    output_price      BIGINT NOT NULL,
    cost_micro        BIGINT NOT NULL CHECK (cost_micro >= 0),
    status            TEXT NOT NULL CHECK (status IN ('ok', 'client_abort', 'upstream_error', 'upstream_timeout', 'no_node')),
    http_status       INT NOT NULL,
    latency_ms        INT NOT NULL DEFAULT 0,
    ttft_ms           INT,                               -- 流式首字节
    client_ip         TEXT NOT NULL DEFAULT '',
    settlement_id     BIGINT REFERENCES llm_settlements (id),
    created_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX llm_usage_user_idx ON llm_usage (user_id, created_at DESC);
CREATE INDEX llm_usage_key_idx ON llm_usage (key_id, created_at DESC);
CREATE INDEX llm_usage_unsettled_idx ON llm_usage (user_id, id) WHERE settlement_id IS NULL;
CREATE INDEX llm_usage_created_idx ON llm_usage (created_at);

-- 按天汇总（永久）
CREATE TABLE llm_usage_daily (
    day               DATE   NOT NULL,                   -- Asia/Shanghai 自然日
    user_id           BIGINT NOT NULL REFERENCES users (id),
    model_id          BIGINT NOT NULL REFERENCES llm_models (id),
    requests          BIGINT NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    cost_micro        BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (day, user_id, model_id)
);
CREATE INDEX llm_usage_daily_day_idx ON llm_usage_daily (day);

-- 钱包：新流水类型（同时列出计划 8 的两种，00021 是否已执行都成立）
ALTER TABLE wallet_ledger DROP CONSTRAINT wallet_ledger_kind_check;
ALTER TABLE wallet_ledger ADD CONSTRAINT wallet_ledger_kind_check
    CHECK (kind IN ('recharge', 'admin_adjust', 'order_pay', 'refund', 'referral_rebate', 'referral_clawback', 'llm_pay'));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM wallet_ledger WHERE kind = 'llm_pay') THEN
        RAISE EXCEPTION '仍有 Token 消费流水，不能回滚';
    END IF;
    IF EXISTS (SELECT 1 FROM llm_nodes WHERE state NOT IN ('removed', 'failed')) THEN
        RAISE EXCEPTION '仍有运行中的节点，请先在后台下线';
    END IF;
END $$;
-- +goose StatementEnd
ALTER TABLE wallet_ledger DROP CONSTRAINT wallet_ledger_kind_check;
ALTER TABLE wallet_ledger ADD CONSTRAINT wallet_ledger_kind_check
    CHECK (kind IN ('recharge', 'admin_adjust', 'order_pay', 'refund', 'referral_rebate', 'referral_clawback'));
DROP TABLE llm_usage_daily;
DROP TABLE llm_usage;
DROP TABLE llm_settlements;
DROP TABLE llm_api_keys;
DROP TABLE llm_accounts;
DROP TABLE llm_node_events;
DROP TABLE llm_nodes;
DROP TABLE llm_models;
ALTER TABLE apps DROP COLUMN internal;
-- 池机组行保留，变成普通不可见机组
DROP INDEX machine_groups_llm_uniq;
ALTER TABLE machine_groups DROP COLUMN purpose;
-- 系统实例若仍存在，前面已因节点检查拒绝
DROP INDEX instances_system_live_idx;
ALTER TABLE instances DROP COLUMN system;
-- 系统账号行保留（phone 不满足登录正则，仍无法登录）
ALTER TABLE users DROP COLUMN system;
