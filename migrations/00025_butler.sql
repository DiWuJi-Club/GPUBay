-- +goose Up
-- 计划 12：AI 管家对话 / 工具调用 / 用量 / 设置；Down 删表，不影响其它表。

CREATE TABLE butler_settings (
    id                 SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    enabled            BOOLEAN NOT NULL DEFAULT true,
    daily_token_limit  BIGINT  NOT NULL DEFAULT 300000 CHECK (daily_token_limit BETWEEN 10000 AND 100000000),
    max_turns          INT     NOT NULL DEFAULT 60     CHECK (max_turns BETWEEN 5 AND 1000),
    notice             TEXT    NOT NULL DEFAULT '',   -- 关闭时给用户看的说明（≤ 200 字）
    updated_at         TIMESTAMPTZ NOT NULL,
    updated_by         BIGINT                          -- admins.id；迁移写入的首行为 NULL
);
INSERT INTO butler_settings (id, enabled, updated_at) VALUES (1, true, now());

CREATE TABLE butler_conversations (
    id              TEXT PRIMARY KEY,                 -- 'cv-' + 12 位 [a-z0-9]
    user_id         BIGINT NOT NULL REFERENCES users (id),
    title           TEXT NOT NULL DEFAULT '',         -- 首条用户消息前 30 字
    status          TEXT NOT NULL DEFAULT 'idle' CHECK (status IN ('idle', 'running', 'awaiting_confirm')),
    run_token       TEXT NOT NULL DEFAULT '',         -- 本轮的随机令牌；running 时非空，回写时比对
    run_started_at  TIMESTAMPTZ,
    context         JSONB NOT NULL DEFAULT '{}',      -- {"instance_id": "ins-…"}（最近一次消息带的上下文）
    provider        TEXT NOT NULL,                    -- openai / anthropic / mock（创建时的配置，展示用）
    model           TEXT NOT NULL,
    user_turns      INT  NOT NULL DEFAULT 0,          -- 用户消息数（D2 上限）
    input_tokens    BIGINT NOT NULL DEFAULT 0,
    output_tokens   BIGINT NOT NULL DEFAULT 0,
    last_message_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX butler_conversations_user_idx ON butler_conversations (user_id, updated_at DESC);
CREATE INDEX butler_conversations_retention_idx ON butler_conversations (updated_at);

CREATE TABLE butler_messages (
    id              BIGSERIAL PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES butler_conversations (id) ON DELETE CASCADE,
    seq             INT  NOT NULL,                    -- 对话内递增，从 1 开始
    role            TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
    content         JSONB NOT NULL,                   -- §2.1 块数组
    tokens          INT  NOT NULL DEFAULT 0,          -- 该消息计入的 token（assistant：输出；user / tool：估算输入）
    created_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (conversation_id, seq)
);

CREATE TABLE butler_tool_calls (
    id              TEXT PRIMARY KEY,                 -- 'tc-' + 12 位
    conversation_id TEXT NOT NULL REFERENCES butler_conversations (id) ON DELETE CASCADE,
    message_id      BIGINT NOT NULL REFERENCES butler_messages (id) ON DELETE CASCADE,  -- 发起调用的 assistant 消息
    user_id         BIGINT NOT NULL REFERENCES users (id),
    provider_call_id TEXT NOT NULL,                   -- 模型给的 tool_call id（回填 tool_result 用）
    tool            TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('read', 'write')),
    args            JSONB NOT NULL,                   -- 已通过 schema 校验的参数（secret 值替换为 '***'）
    summary         TEXT NOT NULL DEFAULT '',         -- 确认卡文案（服务端渲染，≤ 500 字）
    quote_amount    BIGINT,                           -- 花钱操作的报价（分）；无则 NULL
    secret_env_enc  BYTEA,                            -- 写工具 env 里 secret 变量的真实值（JSON 的 secretbox 密文）；执行 / 拒绝 / 过期后置 NULL
    status          TEXT NOT NULL CHECK (status IN ('executed', 'failed', 'pending', 'approved', 'rejected', 'expired')),
    result          JSONB,                            -- 执行结果（≤ 16KB，已净化）或错误 {code, message}
    result_ref      TEXT NOT NULL DEFAULT '',         -- 产生的资源 ID（订单 / 实例 / 部署）
    expires_at      TIMESTAMPTZ,                      -- pending 时 = created_at + 10 分钟
    decided_at      TIMESTAMPTZ,
    executed_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX butler_tool_calls_conv_idx ON butler_tool_calls (conversation_id, created_at);
CREATE INDEX butler_tool_calls_user_idx ON butler_tool_calls (user_id, created_at DESC);
CREATE UNIQUE INDEX butler_tool_calls_pending_uniq ON butler_tool_calls (conversation_id) WHERE status = 'pending';

CREATE TABLE butler_usage (
    user_id        BIGINT NOT NULL REFERENCES users (id),
    day            DATE   NOT NULL,                   -- 东八区日期（tz.DayStart）
    requests       INT    NOT NULL DEFAULT 0,         -- LLM 请求次数
    input_tokens   BIGINT NOT NULL DEFAULT 0,
    output_tokens  BIGINT NOT NULL DEFAULT 0,
    tool_calls     INT    NOT NULL DEFAULT 0,
    failures       INT    NOT NULL DEFAULT 0,         -- LLM 失败次数（不计预算）
    PRIMARY KEY (user_id, day)
);
CREATE INDEX butler_usage_day_idx ON butler_usage (day);

-- +goose Down
DROP TABLE butler_usage, butler_tool_calls, butler_messages, butler_conversations, butler_settings;
