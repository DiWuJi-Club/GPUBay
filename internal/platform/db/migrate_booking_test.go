package db_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/migrations"
)

// 迁移 00019 在带存量数据的库上 Up → Down → Up 往返（含遗留空时段的清理、cancelled 回退为 failed、reserved 时拒绝 Down）。
func TestMigration00019RoundTrip(t *testing.T) {
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
	scalar := func(q string, args ...any) string {
		t.Helper()
		var s sql.NullString
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s.String
	}
	version := func() int64 {
		t.Helper()
		v, err := p.GetDBVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cols := func(table, col string) string {
		return scalar(`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`, table, col)
	}

	if _, err := p.UpTo(ctx, 18); err != nil {
		t.Fatal(err)
	}
	// 00018 下的存量数据：一个运行中实例（正常时段 + 订单），一个立即失败的实例（遗留的空时段）
	exec(`INSERT INTO users (phone, kyc_status, created_at, updated_at) VALUES ('13800000000', 'verified', $1, $1)`, dbtest.T0)
	exec(`INSERT INTO machines (serial, lan_ip, product_id, state, agent_online, last_seen_at, created_at, updated_at)
		VALUES ('SN-OLD', '10.0.0.10', (SELECT id FROM products WHERE code = 'spark-1'), 'allocated', true, $1, $1, $1)`, dbtest.T0)
	for _, id := range []string{"ins-run", "ins-fail", "ins-c"} {
		state := map[string]string{"ins-run": "running", "ins-fail": "failed", "ins-c": "released"}[id]
		exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
			VALUES ($1, (SELECT id FROM users), (SELECT id FROM machines), (SELECT id FROM products WHERE code = 'spark-1'),
				(SELECT id FROM images ORDER BY id LIMIT 1), $2, '\x00', $3, $3)`, id, state, dbtest.T0)
	}
	exec(`INSERT INTO orders (id, user_id, kind, instance_id, product_id, duration, amount, status, created_at, updated_at)
		VALUES ('ord-old', (SELECT id FROM users), 'new', 'ins-run', (SELECT id FROM products WHERE code = 'spark-1'), '1d', 6900, 'paid', $1, $1)`, dbtest.T0)
	exec(`INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at) VALUES
		((SELECT id FROM machines), 'ins-run', tstzrange($1::timestamptz, $1::timestamptz + interval '31 hours'), 'active', $1, $1),
		((SELECT id FROM machines), 'ins-fail', tstzrange($1::timestamptz, $1::timestamptz), 'ended', $1, $1)`, dbtest.T0)

	if _, err := p.UpTo(ctx, 19); err != nil {
		t.Fatalf("up 19 on existing data: %v", err)
	}
	if got := scalar(`SELECT count(*) FROM machine_leases WHERE isempty(period)`); got != "0" {
		t.Fatalf("empty leases left = %s", got)
	}
	if got := scalar(`SELECT upper(period) - lower(period) FROM machine_leases WHERE instance_id = 'ins-fail'`); got != "00:00:01" {
		t.Fatalf("cleaned lease length = %s", got)
	}
	if got := scalar(`SELECT auto_renew_duration || '/' || reassign_count || '/' || auto_renew FROM instances WHERE id = 'ins-run'`); got != "1m/0/false" {
		t.Fatalf("instance defaults = %s", got)
	}
	if got := scalar(`SELECT quantity || '/' || auto || '/' || (batch_id IS NULL) FROM orders WHERE id = 'ord-old'`); got != "1/false/true" {
		t.Fatalf("order defaults = %s", got)
	}

	// 有 reserved 实例时 Down 拒绝，且整条 Down 回滚（版本仍为 19）
	exec(`UPDATE instances SET state = 'reserved', scheduled_start_at = $1 WHERE id = 'ins-c'`, dbtest.T0.Add(48*time.Hour))
	if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "仍有未开通的预订") {
		t.Fatalf("down with reserved = %v", err)
	}
	if v := version(); v != 19 {
		t.Fatalf("version after refused down = %d", v)
	}

	// cancelled 实例回退为 failed，其未结束的时段一并取消
	exec(`UPDATE instances SET state = 'cancelled', cancelled_at = $1 WHERE id = 'ins-c'`, dbtest.T0)
	exec(`INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ((SELECT id FROM machines), 'ins-c', tstzrange($1::timestamptz + interval '40 hours', $1::timestamptz + interval '50 hours'), 'reserved', $1, $1)`, dbtest.T0)
	exec(`INSERT INTO machine_groups (name, created_at, updated_at) VALUES ('A 区', $1, $1)`, dbtest.T0)
	exec(`UPDATE machines SET group_id = (SELECT id FROM machine_groups), display_name = 'A-01'`)
	if _, err := p.Down(ctx); err != nil {
		t.Fatalf("down: %v", err)
	}
	if v := version(); v != 18 {
		t.Fatalf("version after down = %d", v)
	}
	if got := scalar(`SELECT state FROM instances WHERE id = 'ins-c'`); got != "failed" {
		t.Fatalf("cancelled instance after down = %s", got)
	}
	if got := scalar(`SELECT status FROM machine_leases WHERE instance_id = 'ins-c'`); got != "cancelled" {
		t.Fatalf("cancelled instance lease after down = %s", got)
	}
	if cols("instances", "auto_renew") != "0" || cols("orders", "quantity") != "0" || cols("machines", "display_name") != "0" ||
		scalar(`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('machine_groups', 'rental_batches')`) != "0" {
		t.Fatal("down left 00019 schema behind")
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE instances SET state = 'cancelled' WHERE id = 'ins-c'`); err == nil {
		t.Fatal("old CHECK not restored")
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if v := version(); v < 19 || cols("instances", "auto_renew") != "1" {
		t.Fatalf("re-up version = %d", v)
	}
	if got := scalar(`SELECT count(*) FROM instances`); got != "3" {
		t.Fatalf("instances after round trip = %s", got)
	}
}
