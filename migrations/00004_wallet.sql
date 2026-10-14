-- +goose Up
CREATE TABLE wallet_accounts (
    user_id    BIGINT PRIMARY KEY REFERENCES users (id),
    balance    BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE wallet_ledger (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users (id),
    amount        BIGINT NOT NULL CHECK (amount <> 0),
    balance_after BIGINT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('recharge', 'admin_adjust', 'order_pay', 'refund')),
    ref_type      TEXT NOT NULL DEFAULT '',
    ref_id        TEXT NOT NULL DEFAULT '',
    operator_id   BIGINT,
    remark        TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX wallet_ledger_user_idx ON wallet_ledger (user_id, id DESC);
-- 同一业务单据同一类型只能记一次账（例如同一订单只能退款一次）
CREATE UNIQUE INDEX wallet_ledger_ref_uniq ON wallet_ledger (kind, ref_type, ref_id) WHERE ref_id <> '';

-- +goose Down
DROP TABLE wallet_ledger;
DROP TABLE wallet_accounts;
