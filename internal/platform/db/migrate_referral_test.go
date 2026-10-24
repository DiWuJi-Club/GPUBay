package db_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/migrations"
)

// 迁移 00021 在带存量数据的库上 Up → Down → Up 往返；有 referral_* 流水时 Down 拒绝。
// 基线取 UpTo(20)：计划 7 的 00020 合入前后都成立（没有 00020 时基线是 19），版本断言相对基线，不写死最新版本。
func TestMigration00021RoundTrip(t *testing.T) {
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
	if _, err := p.UpTo(ctx, 20); err != nil {
		t.Fatal(err)
	}
	base := version()
	if base < 19 || base >= 21 {
		t.Fatalf("baseline version = %d", base)
	}
	exec(`INSERT INTO users (phone, kyc_status, created_at, updated_at) VALUES ('13800000000', 'verified', $1, $1)`, dbtest.T0)
	exec(`INSERT INTO wallet_accounts (user_id, balance, updated_at) VALUES ((SELECT id FROM users), 100, $1)`, dbtest.T0)
	exec(`INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		VALUES ((SELECT id FROM users), 100, 100, 'recharge', 'recharge', 'rch-1', '', $1)`, dbtest.T0)

	if _, err := p.UpTo(ctx, 21); err != nil {
		t.Fatalf("up 21: %v", err)
	}
	if v := version(); v != 21 {
		t.Fatalf("version after up 21 = %d", v)
	}
	if got := scalar(`SELECT rate_bp || '/' || max_per_invitee || '/' || max_per_inviter_30d || '/' || hold_days || '/' || period_days FROM referral_rate_versions`); got != "1000/50000/500000/7/365" {
		t.Fatalf("seed rate version = %s", got)
	}
	if got := scalar(`SELECT watermark::date::text FROM referral_state WHERE id = 1`); got != "2000-01-01" {
		t.Fatalf("watermark = %s", got)
	}
	if got := scalar(`SELECT last_login_ip || '/' || (last_login_at IS NULL) FROM users`); got != "/true" {
		t.Fatalf("users new cols = %s", got)
	}
	if got := scalar(`SELECT count(*) FROM pg_indexes WHERE indexname IN ('wallet_ledger_order_scan_idx', 'kyc_records_fingerprint_idx', 'referral_events_source_uniq')`); got != "3" {
		t.Fatalf("indexes = %s", got)
	}
	// 新类型可写；未知类型仍被 CHECK 拒绝
	exec(`INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		VALUES ((SELECT id FROM users), 10, 110, 'referral_rebate', 'referral', 'ord-x', '', $1)`, dbtest.T0)
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		VALUES ((SELECT id FROM users), 10, 120, 'bogus', 'x', 'y', '', $1)`, dbtest.T0); err == nil {
		t.Fatal("bogus kind accepted")
	}
	// 有 referral 流水时 Down 拒绝且版本不变
	if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "仍有邀请返利流水") {
		t.Fatalf("down with referral ledger = %v", err)
	}
	if v := version(); v != 21 {
		t.Fatalf("version after refused down = %d", v)
	}
	exec(`DELETE FROM wallet_ledger WHERE kind = 'referral_rebate'`)
	if _, err := p.Down(ctx); err != nil {
		t.Fatalf("down: %v", err)
	}
	if v := version(); v != base {
		t.Fatalf("version after down = %d, want %d", v, base)
	}
	if got := scalar(`SELECT count(*) FROM information_schema.tables WHERE table_name LIKE 'referral_%' OR table_name = 'user_devices'`); got != "0" {
		t.Fatalf("down left tables: %s", got)
	}
	if got := scalar(`SELECT count(*) FROM information_schema.columns WHERE (table_name = 'users' AND column_name IN ('last_login_ip', 'last_login_at')) OR (table_name = 'kyc_records' AND column_name = 'fingerprint')`); got != "0" {
		t.Fatalf("down left columns: %s", got)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		VALUES ((SELECT id FROM users), 10, 110, 'referral_rebate', 'referral', 'ord-y', '', $1)`, dbtest.T0); err == nil {
		t.Fatal("old CHECK not restored")
	}
	if _, err := p.UpTo(ctx, 21); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := scalar(`SELECT count(*) FROM referral_rate_versions`); got != "1" {
		t.Fatalf("rate versions after round trip = %s", got)
	}
}
