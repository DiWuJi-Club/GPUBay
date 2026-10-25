package db_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/migrations"
)

// 迁移 00022：带存量数据 Up → Down（两个拒绝条件）→ Up 往返。
// UpTo(21)：计划 8 的 00021 不存在时 goose 停在 20，存在时停在 21；之后 UpByOne 都落到 22。
func TestMigration00022RoundTrip(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", dbtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	upByOne := func() {
		t.Helper()
		r, err := p.UpByOne(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r.Source.Version != 22 {
			t.Fatalf("UpByOne applied version %d, want 22", r.Source.Version)
		}
	}
	if _, err := p.UpTo(ctx, 21); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO users (phone, kind, status, created_at, updated_at) VALUES ('13800000001', 'personal', 'active', now(), now())`)
	exec(`INSERT INTO machines (serial, rack, slot, lan_ip, product_id, state, created_at, updated_at) VALUES ('SN1', 'A', '1', '10.0.0.1', 1, 'idle', now(), now())`)
	exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ('ins-aaaaaaaa', 1, 1, 1, 1, 'running', 'x', now(), now())`)
	exec(`INSERT INTO wallet_accounts (user_id, balance, updated_at) VALUES (1, 900, now())`)
	exec(`INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, created_at)
		VALUES (1, -100, 900, 'order_pay', 'order', 'ord-1', now())`)
	upByOne()

	// 系统账号
	if n := count(`SELECT count(*) FROM users WHERE phone = 'system:llm' AND system AND kyc_status = 'verified' AND status = 'active'`); n != 1 {
		t.Fatalf("system user = %d", n)
	}
	if n := count(`SELECT count(*) FROM users WHERE NOT system`); n != 1 {
		t.Fatalf("non-system users = %d", n)
	}
	if n := count(`SELECT count(*) FROM instances WHERE NOT system`); n != 1 {
		t.Fatalf("instances.system default: %d", n)
	}
	// 池机组：唯一一个，不可见
	if n := count(`SELECT count(*) FROM machine_groups WHERE purpose = 'llm' AND NOT visible AND name = 'Token 服务池'`); n != 1 {
		t.Fatalf("pool group = %d", n)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO machine_groups (name, purpose, visible, created_at, updated_at) VALUES ('池2', 'llm', false, now(), now())`); !db.IsUniqueViolation(err) {
		t.Fatalf("second llm group: err = %v, want unique violation", err)
	}
	exec(`INSERT INTO machine_groups (name, created_at, updated_at) VALUES ('普通', now(), now())`)
	if n := count(`SELECT count(*) FROM machine_groups WHERE name = '普通' AND purpose = 'rental'`); n != 1 {
		t.Fatalf("purpose default = %d", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'apps' AND column_name = 'internal'`); n != 1 {
		t.Fatalf("apps.internal = %d", n)
	}
	// 节点：同一机器最多一个非终态节点
	exec(`INSERT INTO llm_models (slug, name, kind, source_id, image_id, context_len, input_price, output_price, created_at, updated_at)
		VALUES ('qwen3-4b', 'Qwen3 4B', 'chat', 'Qwen/Qwen3-4B', 1, 32768, 100, 400, now(), now())`)
	exec(`INSERT INTO llm_nodes (id, model_id, machine_id, state, api_key_enc, created_at, updated_at)
		VALUES ('node-000000000001', 1, 1, 'provisioning', '\x00', now(), now())`)
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO llm_nodes (id, model_id, machine_id, state, api_key_enc, created_at, updated_at)
		VALUES ('node-000000000002', 1, 1, 'ready', '\x00', now(), now())`); !db.IsUniqueViolation(err) {
		t.Fatalf("second live node on machine: err = %v, want unique violation", err)
	}
	exec(`UPDATE llm_nodes SET state = 'removed' WHERE id = 'node-000000000001'`)
	exec(`INSERT INTO llm_nodes (id, model_id, machine_id, state, api_key_enc, created_at, updated_at)
		VALUES ('node-000000000002', 1, 1, 'ready', '\x00', now(), now())`)
	// 钱包：llm_pay 与计划 8 的两种都可插
	exec(`INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, created_at)
		VALUES (1, -10, 890, 'llm_pay', 'llm_settlement', '1', now())`)
	exec(`INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, created_at)
		VALUES (1, 5, 895, 'referral_rebate', 'referral', 'ord-x', now())`)
	exec(`DELETE FROM wallet_ledger WHERE kind = 'referral_rebate'`)

	// Down：有 llm_pay 流水 → 拒绝
	if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "Token 消费流水") {
		t.Fatalf("down with llm_pay ledger: err = %v", err)
	}
	exec(`DELETE FROM wallet_ledger WHERE kind = 'llm_pay'`)
	// Down：有非终态节点 → 拒绝
	if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "运行中的节点") {
		t.Fatalf("down with ready node: err = %v", err)
	}
	exec(`UPDATE llm_nodes SET state = 'failed' WHERE id = 'node-000000000002'`)
	// Down：节点已终态但系统实例仍未释放 → 拒绝
	exec(`UPDATE instances SET system = true WHERE id = 'ins-aaaaaaaa'`)
	if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "平台节点实例") {
		t.Fatalf("down with live system instance: err = %v", err)
	}
	exec(`UPDATE instances SET system = false WHERE id = 'ins-aaaaaaaa'`)
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'system'`); n != 0 {
		t.Fatalf("users.system after down = %d", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.columns WHERE (table_name = 'instances' AND column_name = 'system')
		OR (table_name = 'machine_groups' AND column_name = 'purpose') OR (table_name = 'apps' AND column_name = 'internal')`); n != 0 {
		t.Fatalf("columns after down = %d", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_name LIKE 'llm\_%'`); n != 0 {
		t.Fatalf("llm tables after down = %d", n)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, created_at)
		VALUES (1, -10, 890, 'llm_pay', 'llm_settlement', '2', now())`); err == nil {
		t.Fatal("llm_pay accepted after down")
	}
	// Down 恢复的是计划 8（00021）的六种：两种返利流水仍可插
	exec(`INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, created_at)
		VALUES (1, 5, 895, 'referral_rebate', 'referral', 'ord-y', now()), (1, -5, 890, 'referral_clawback', 'referral', 'ord-y', now())`)
	exec(`DELETE FROM wallet_ledger WHERE kind IN ('referral_rebate', 'referral_clawback')`)
	if n := count(`SELECT count(*) FROM wallet_ledger WHERE kind = 'order_pay' AND ref_id = 'ord-1' AND amount = -100`); n != 1 {
		t.Fatalf("old ledger after down = %d", n)
	}
	// 系统账号与池机组行保留
	if n := count(`SELECT count(*) FROM users WHERE phone = 'system:llm' AND status = 'banned'`); n != 1 {
		t.Fatalf("banned system user row after down = %d", n)
	}
	if n := count(`SELECT count(*) FROM users WHERE phone = '13800000001' AND status = 'active'`); n != 1 {
		t.Fatalf("ordinary user touched by down = %d", n)
	}
	if n := count(`SELECT count(*) FROM machine_groups WHERE name = 'Token 服务池' AND NOT visible`); n != 1 {
		t.Fatalf("pool group row after down = %d", n)
	}

	// 再 Up：两行按唯一键 upsert，不重复
	upByOne()
	if n := count(`SELECT count(*) FROM users WHERE phone = 'system:llm' AND system AND status = 'active'`); n != 1 {
		t.Fatalf("system user after re-up = %d", n)
	}
	if n := count(`SELECT count(*) FROM machine_groups WHERE purpose = 'llm' AND NOT visible`); n != 1 {
		t.Fatalf("pool group after re-up = %d", n)
	}
	exec(`INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, created_at)
		VALUES (1, -10, 890, 'llm_pay', 'llm_settlement', '3', now())`)
}
