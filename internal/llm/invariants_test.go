package llm_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// 七条不变量：空库成立；一组自洽的数据成立；逐条破坏后报出对应编号，恢复后重新成立。
func TestCheckInvariants(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := llm.CheckInvariants(ctx, pool); err != nil {
		t.Fatalf("empty db: %v", err)
	}

	t0 := dbtest.T0
	u := dbtest.CreateUser(t, pool, "13700000001", true)
	dbtest.Fund(t, pool, u, 1000)
	img := dbtest.ImageID(t, pool)
	prod := dbtest.ProductID(t, pool)
	m := dbtest.CreateMachine(t, pool, "SN-LLM-1")
	var sys int64
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE system AND phone = $1`, llm.SystemUserPhone).Scan(&sys); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE machines SET group_id = (SELECT id FROM machine_groups WHERE purpose = 'llm') WHERE id = $1`, m)
	exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, system, created_at, updated_at)
		VALUES ('ins-sys00001', $1, $2, $3, $4, 'running', 'x', true, $5, $5)`, sys, m, prod, img, t0)
	exec(`INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, 'ins-sys00001', tstzrange($2::timestamptz, $2::timestamptz + interval '1 hour'), 'active', $2, $2)`, m, t0)
	exec(`INSERT INTO apps (slug, name, category, status, internal, created_at, updated_at) VALUES ('vllm-node', 'vLLM 节点', 'llm', 'published', true, $1, $1)`, t0)
	exec(`INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, created_at, updated_at)
		VALUES ((SELECT id FROM apps WHERE slug = 'vllm-node'), '1.0.0', 'published', ARRAY[$1::bigint], '{"ports":[{"port":8000}]}', 'true', 'h', $2, $2)`, img, t0)
	exec(`INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ('dep-000000000001', 'ins-sys00001', $1, (SELECT id FROM apps WHERE slug = 'vllm-node'), (SELECT id FROM app_versions), 'running', 8000, 'h', $2, $2, $2)`, sys, t0)
	exec(`INSERT INTO llm_models (slug, name, kind, source_id, image_id, context_len, input_price, output_price, status, replicas, created_at, updated_at)
		VALUES ('qwen3-4b', 'Qwen3 4B', 'chat', 'Qwen/Qwen3-4B', $1, 32768, 100, 400, 'online', 1, $2, $2)`, img, t0)
	exec(`INSERT INTO llm_nodes (id, model_id, machine_id, instance_id, deployment_id, state, api_key_enc, created_at, updated_at)
		VALUES ('node-000000000001', (SELECT id FROM llm_models), $1, 'ins-sys00001', 'dep-000000000001', 'ready', '\x00', $2, $2)`, m, t0)
	// 一个 removing 节点：实例已 released，不算违背
	m2 := dbtest.CreateMachine(t, pool, "SN-LLM-2")
	exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, system, created_at, updated_at)
		VALUES ('ins-sys00002', $1, $2, $3, $4, 'released', 'x', true, $5, $5)`, sys, m2, prod, img, t0)
	exec(`INSERT INTO llm_nodes (id, model_id, machine_id, instance_id, state, api_key_enc, created_at, updated_at)
		VALUES ('node-000000000002', (SELECT id FROM llm_models), $1, 'ins-sys00002', 'removing', '\x00', $2, $2)`, m2, t0)

	exec(`INSERT INTO llm_api_keys (user_id, name, key_hash, key_hint, created_at, updated_at) VALUES ($1, 'k', 'h1', 'sk-spark-…abcd', $2, $2)`, u, t0)
	// 一次结算：用量 2.5 分 → usage_amount 2、carry 0.5 分、扣到 2 分
	exec(`INSERT INTO llm_settlements (id, user_id, requests, prompt_tokens, completion_tokens, cost_micro, usage_amount, debt_amount, charged, created_at)
		VALUES (1, $1, 1, 1000, 6000, 2500000, 2, 0, 2, $2)`, u, t0)
	exec(`UPDATE wallet_accounts SET balance = balance - 2 WHERE user_id = $1`, u)
	exec(`INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, created_at)
		VALUES ($1, -2, 998, 'llm_pay', 'llm_settlement', '1', $2)`, u, t0)
	exec(`UPDATE llm_settlements SET wallet_ledger_id = (SELECT id FROM wallet_ledger WHERE kind = 'llm_pay') WHERE id = 1`)
	usage := `INSERT INTO llm_usage (request_id, user_id, key_id, model_id, node_id, endpoint, stream, prompt_tokens, completion_tokens,
		input_price, output_price, cost_micro, status, http_status, settlement_id, created_at)
		VALUES ($1, $2, (SELECT id FROM llm_api_keys), (SELECT id FROM llm_models), 'node-000000000001', 'chat', false, $3, $4, 100, 400, $5, 'ok', 200, $6, $7)`
	exec(usage, "req-0000000000000001", u, 1000, 6000, 2500000, int64(1), t0)
	exec(usage, "req-0000000000000002", u, 1000, 500, 300000, nil, t0)
	exec(`INSERT INTO llm_accounts (user_id, unsettled_micro, carry_micro, debt, created_at, updated_at) VALUES ($1, 300000, 500000, 0, $2, $2)`, u, t0)
	// T0 = 2026-01-01 00:00 UTC = 北京时间 2026-01-01 08:00
	exec(`INSERT INTO llm_usage_daily (day, user_id, model_id, requests, prompt_tokens, completion_tokens, cost_micro)
		VALUES ('2026-01-01', $1, (SELECT id FROM llm_models), 1, 1000, 6000, 2500000)`, u)
	if err := llm.CheckInvariants(ctx, pool); err != nil {
		t.Fatalf("consistent data: %v", err)
	}

	cases := []struct {
		n           int
		break_, fix string
		alsoAllowed []int // 同一破坏必然连带的其它编号
	}{
		{1, `UPDATE llm_accounts SET unsettled_micro = 1`, `UPDATE llm_accounts SET unsettled_micro = 300000`, nil},
		{2, `UPDATE llm_accounts SET carry_micro = 0`, `UPDATE llm_accounts SET carry_micro = 500000`, nil},
		{3, `UPDATE llm_accounts SET debt = 5`, `UPDATE llm_accounts SET debt = 0`, nil},
		{3, `UPDATE wallet_ledger SET amount = -3 WHERE kind = 'llm_pay'`, `UPDATE wallet_ledger SET amount = -2 WHERE kind = 'llm_pay'`, []int{4}},
		{4, `UPDATE llm_settlements SET wallet_ledger_id = NULL`, `UPDATE llm_settlements SET wallet_ledger_id = (SELECT id FROM wallet_ledger WHERE kind = 'llm_pay')`, nil},
		{4, `UPDATE llm_settlements SET debt_amount = 0, usage_amount = 1, charged = 2`, `UPDATE llm_settlements SET usage_amount = 2`, []int{2, 3}},
		{5, `UPDATE llm_usage_daily SET requests = 2`, `UPDATE llm_usage_daily SET requests = 1`, nil},
		{5, `DELETE FROM llm_usage_daily`, `INSERT INTO llm_usage_daily (day, user_id, model_id, requests, prompt_tokens, completion_tokens, cost_micro)
			VALUES ('2026-01-01', (SELECT id FROM users WHERE phone = '13700000001'), (SELECT id FROM llm_models), 1, 1000, 6000, 2500000)`, nil},
		{6, `UPDATE instances SET system = false WHERE id = 'ins-sys00001'`, `UPDATE instances SET system = true WHERE id = 'ins-sys00001'`, []int{7}},
		{6, `UPDATE instances SET state = 'failed' WHERE id = 'ins-sys00001'`, `UPDATE instances SET state = 'running' WHERE id = 'ins-sys00001'`, nil},
		{6, `UPDATE app_deployments SET state = 'failed'`, `UPDATE app_deployments SET state = 'running'`, nil},
		{6, `UPDATE llm_nodes SET state = 'draining' WHERE id = 'node-000000000002'`, `UPDATE llm_nodes SET state = 'removing' WHERE id = 'node-000000000002'`, nil},
	}
	check := func(n int, allowed []int) {
		t.Helper()
		err := llm.CheckInvariants(ctx, pool)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("invariant %d:", n)) {
			t.Fatalf("want invariant %d, got %v", n, err)
		}
		for i := 1; i <= 7; i++ {
			if i == n || slices.Contains(allowed, i) {
				continue
			}
			if strings.Contains(err.Error(), fmt.Sprintf("invariant %d:", i)) {
				t.Fatalf("breaking invariant %d also reported invariant %d: %v", n, i, err)
			}
		}
	}
	for _, c := range cases {
		exec(c.break_)
		check(c.n, c.alsoAllowed)
		exec(c.fix)
		if err := llm.CheckInvariants(ctx, pool); err != nil {
			t.Fatalf("after fixing %q: %v", c.break_, err)
		}
	}

	// 7：池内机器上有节点时又出现非系统实例的预订 lease
	exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ('ins-usr00001', $1, $2, $3, $4, 'reserved', 'x', $5, $5)`, u, m, prod, img, t0)
	exec(`INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, 'ins-usr00001', tstzrange($2::timestamptz + interval '2 hours', $2::timestamptz + interval '3 hours'), 'reserved', $2, $2)`, m, t0)
	check(7, nil)
	exec(`UPDATE machine_leases SET status = 'cancelled' WHERE instance_id = 'ins-usr00001'`)
	if err := llm.CheckInvariants(ctx, pool); err != nil {
		t.Fatalf("after cancelling lease: %v", err)
	}
}
