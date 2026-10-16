-- +goose Up
CREATE TABLE recharges (
    id               TEXT PRIMARY KEY,                -- rch- + 12 位，同时作为渠道的商户订单号
    user_id          BIGINT NOT NULL REFERENCES users (id),
    channel          TEXT NOT NULL CHECK (channel IN ('wechat', 'alipay', 'manual')),
    amount           BIGINT NOT NULL CHECK (amount > 0),
    status           TEXT NOT NULL CHECK (status IN ('pending', 'paid', 'closed')),
    qr_code          TEXT NOT NULL DEFAULT '',
    channel_trade_no TEXT NOT NULL DEFAULT '',
    operator_id      BIGINT,                          -- 手动入账的管理员
    remark           TEXT NOT NULL DEFAULT '',
    expires_at       TIMESTAMPTZ NOT NULL,            -- 二维码失效时间
    paid_at          TIMESTAMPTZ,
    closed_at        TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX recharges_user_idx ON recharges (user_id, created_at DESC);
CREATE INDEX recharges_pending_idx ON recharges (created_at) WHERE status = 'pending';
CREATE INDEX recharges_created_idx ON recharges (created_at DESC);

-- +goose Down
DROP TABLE recharges;
