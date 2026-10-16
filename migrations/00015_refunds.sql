-- +goose Up
-- 订单已退金额与退款次数：支持「退款并释放」按订单部分退款
ALTER TABLE orders
    ADD COLUMN refunded_amount BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN refund_count INT NOT NULL DEFAULT 0;
UPDATE orders SET refunded_amount = amount, refund_count = 1 WHERE status = 'refunded';
ALTER TABLE orders ADD CONSTRAINT orders_refunded_amount_chk CHECK (refunded_amount >= 0 AND refunded_amount <= amount);

-- +goose Down
ALTER TABLE orders DROP CONSTRAINT orders_refunded_amount_chk, DROP COLUMN refund_count, DROP COLUMN refunded_amount;
