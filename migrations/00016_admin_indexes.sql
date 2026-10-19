-- +goose Up
ALTER TABLE alerts ADD COLUMN resolved_by BIGINT;  -- 处理告警的管理员
CREATE INDEX alerts_created_idx ON alerts (created_at DESC);
CREATE INDEX machines_state_idx ON machines (state, rack, slot);
CREATE INDEX instances_machine_idx ON instances (machine_id, created_at DESC);
CREATE INDEX instances_created_idx ON instances (created_at DESC);
CREATE INDEX orders_created_idx ON orders (created_at DESC);
CREATE INDEX wallet_ledger_created_idx ON wallet_ledger (created_at DESC);
CREATE INDEX audit_logs_created_idx ON audit_logs (created_at DESC);
CREATE INDEX audit_logs_actor_idx ON audit_logs (actor_id, created_at DESC);
CREATE INDEX users_created_idx ON users (created_at DESC);

-- +goose Down
DROP INDEX users_created_idx;
DROP INDEX audit_logs_actor_idx;
DROP INDEX audit_logs_created_idx;
DROP INDEX wallet_ledger_created_idx;
DROP INDEX orders_created_idx;
DROP INDEX instances_created_idx;
DROP INDEX instances_machine_idx;
DROP INDEX machines_state_idx;
DROP INDEX alerts_created_idx;
ALTER TABLE alerts DROP COLUMN resolved_by;
