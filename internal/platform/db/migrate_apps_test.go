package db_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/migrations"
)

// 迁移 00020：带存量数据 Up → Down → Up 往返；Down 后旧表 / 旧列不受影响。
func TestMigration00020RoundTrip(t *testing.T) {
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
	if _, err := p.UpTo(ctx, 19); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO users (phone, kind, status, created_at, updated_at) VALUES ('13800000001', 'personal', 'active', now(), now())`)
	exec(`INSERT INTO machines (serial, rack, slot, lan_ip, product_id, state, created_at, updated_at) VALUES ('SN1', 'A', '1', '10.0.0.1', 1, 'idle', now(), now())`)
	exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ('ins-aaaaaaaa', 1, 1, 1, 1, 'running', 'x', now(), now())`)
	exec(`INSERT INTO instance_ports (instance_id, port, visibility, note, created_at, updated_at) VALUES ('ins-aaaaaaaa', 8888, 'private', 'old', now(), now())`)
	if _, err := p.UpByOne(ctx); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO apps (slug, name, category, status, created_at, updated_at) VALUES ('vllm', 'vLLM', 'llm', 'published', now(), now())`)
	exec(`INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, created_at, updated_at)
		VALUES (1, '1', 'published', '{1}', '{"ports":[{"port":8000}]}', 'true', 'h', now(), now())`)
	exec(`INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ('dep-000000000001', 'ins-aaaaaaaa', 1, 1, 1, 'queued', 8000, 'h', now(), now(), now())`)
	exec(`UPDATE instance_ports SET deployment_id = 'dep-000000000001' WHERE instance_id = 'ins-aaaaaaaa'`)
	exec(`UPDATE machines SET cached_images = '{"spark/llm-serving:25.09"}', cached_images_at = now() WHERE id = 1`)
	// 同一实例同一应用只能有一条非终态部署
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ('dep-000000000002', 'ins-aaaaaaaa', 1, 1, 1, 'running', 8000, 'h', now(), now(), now())`); !db.IsUniqueViolation(err) {
		t.Fatalf("second live deployment of same app: err = %v, want unique violation", err)
	}
	exec(`INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ('dep-000000000003', 'ins-aaaaaaaa', 1, 1, 1, 'removed', 8000, 'h', now(), now(), now())`)
	if n := count(`SELECT count(*) FROM app_deployments WHERE unreachable_count = 0 AND crash_count = 0`); n != 2 {
		t.Fatalf("counters default = %d rows", n)
	}
	// version：乐观并发版本号，默认 0，NOT NULL
	if n := count(`SELECT count(*) FROM app_deployments WHERE row_version = 0`); n != 2 {
		t.Fatalf("version default = %d rows", n)
	}
	exec(`UPDATE app_deployments SET row_version = row_version + 1 WHERE id = 'dep-000000000001'`)
	if n := count(`SELECT row_version FROM app_deployments WHERE id = 'dep-000000000001'`); n != 1 {
		t.Fatalf("version after bump = %d", n)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE app_deployments SET row_version = NULL WHERE id = 'dep-000000000001'`); err == nil {
		t.Fatal("NULL version accepted")
	}
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM instance_ports WHERE instance_id = 'ins-aaaaaaaa'`); n != 1 {
		t.Fatalf("ports after down = %d", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('apps', 'app_versions', 'app_deployments', 'app_deploy_events')`); n != 0 {
		t.Fatalf("tables after down = %d", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'machines' AND column_name IN ('cached_images', 'cached_images_at')`); n != 0 {
		t.Fatalf("machine columns after down = %d", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'instance_ports' AND column_name = 'deployment_id'`); n != 0 {
		t.Fatalf("instance_ports.deployment_id after down = %d", n)
	}
	if _, err := p.UpByOne(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM apps`); n != 0 {
		t.Fatalf("apps after re-up = %d", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'app_deployments' AND column_name = 'row_version' AND is_nullable = 'NO' AND column_default = '0'`); n != 1 {
		t.Fatalf("app_deployments.row_version after re-up = %d", n)
	}
}
