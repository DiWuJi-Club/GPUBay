-- +goose Up
-- 换机记录：原实例剩余时长转移到新实例，原实例的已付订单通过此表关联到新实例
CREATE TABLE instance_transfers (
    id               BIGSERIAL PRIMARY KEY,
    from_instance_id TEXT NOT NULL UNIQUE REFERENCES instances (id),
    to_instance_id   TEXT NOT NULL UNIQUE REFERENCES instances (id),
    seconds          BIGINT NOT NULL,
    reason           TEXT NOT NULL,
    admin_id         BIGINT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE instance_transfers;
