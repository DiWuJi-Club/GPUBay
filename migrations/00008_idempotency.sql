-- +goose Up
-- 下单/续费的幂等键：同一用户同一键只会生成一笔订单（防双击与客户端重试重复扣款）
ALTER TABLE orders ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX orders_user_idempotency_uniq ON orders (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- +goose Down
DROP INDEX orders_user_idempotency_uniq;
ALTER TABLE orders DROP COLUMN idempotency_key;
