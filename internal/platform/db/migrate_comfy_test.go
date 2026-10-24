package db_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/migrations"
)

// 迁移 00024（计划 11 工作流市场）：带存量数据 Up → Down → Up 往返；Down 后计划 7 的表不受影响。
// 00021～00023 留给其它计划，可能尚不存在：先 UpTo 到 00024 之前的最大号，再 UpByOne 恰好落到 00024。
func TestMigration00024RoundTrip(t *testing.T) {
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
	var prev int64
	for _, s := range p.ListSources() {
		if s.Version < 24 && s.Version > prev {
			prev = s.Version
		}
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
	isCheck := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23514"
	}
	if _, err := p.UpTo(ctx, prev); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO users (phone, kind, status, created_at, updated_at) VALUES ('13800000001', 'personal', 'active', now(), now())`)
	exec(`INSERT INTO machines (serial, rack, slot, lan_ip, product_id, state, created_at, updated_at) VALUES ('SN1', 'A', '1', '10.0.0.1', 1, 'idle', now(), now())`)
	exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ('ins-aaaaaaaa', 1, 1, 1, 1, 'running', 'x', now(), now())`)
	exec(`INSERT INTO apps (slug, name, category, status, created_at, updated_at) VALUES ('comfyui', 'ComfyUI', 'image', 'published', now(), now())`)
	exec(`INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, created_at, updated_at)
		VALUES (1, '1.0.0', 'published', '{1}', '{"ports":[{"port":8188}]}', 'true', 'h', now(), now())`)
	exec(`INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ('dep-000000000001', 'ins-aaaaaaaa', 1, 1, 1, 'running', 8188, 'h', now(), now(), now())`)

	res, err := p.UpByOne(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source.Version != 24 {
		t.Fatalf("UpByOne applied %d, want 24", res.Source.Version)
	}

	exec(`INSERT INTO comfy_workflows (slug, name, category, created_at, updated_at) VALUES ('sdxl-txt2img', 'SDXL 文生图', 'txt2img', now(), now())`)
	if n := count(`SELECT count(*) FROM comfy_workflows WHERE status = 'draft' AND tags = '{}' AND sort = 0 AND cover_key IS NULL AND summary = ''`); n != 1 {
		t.Fatalf("workflow defaults = %d rows", n)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO comfy_workflows (slug, name, category, created_at, updated_at) VALUES ('bad-cat', 'x', 'llm', now(), now())`); !isCheck(err) {
		t.Fatalf("bad category: err = %v, want check violation", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO comfy_workflows (slug, name, category, created_at, updated_at) VALUES ('sdxl-txt2img', 'dup', 'other', now(), now())`); !db.IsUniqueViolation(err) {
		t.Fatalf("duplicate slug: err = %v, want unique violation", err)
	}
	exec(`INSERT INTO comfy_workflow_versions (workflow_id, version, graph, api_prompt, manifest, manifest_hash, created_at, updated_at)
		VALUES (1, '1.0.0', '{"nodes":[],"links":[]}', '{"3":{"class_type":"KSampler","inputs":{}}}', '{"models":[]}', 'h', now(), now())`)
	exec(`INSERT INTO comfy_workflow_versions (workflow_id, version, graph, manifest, manifest_hash, created_at, updated_at)
		VALUES (1, '1.1.0', '{"nodes":[],"links":[]}', '{}', 'h', now(), now())`)
	if n := count(`SELECT count(*) FROM comfy_workflow_versions WHERE status = 'draft' AND download_gb = 0 AND changelog = '' AND published_at IS NULL`); n != 2 {
		t.Fatalf("version defaults = %d rows", n)
	}
	if n := count(`SELECT count(*) FROM comfy_workflow_versions WHERE api_prompt IS NULL`); n != 1 {
		t.Fatalf("nullable api_prompt = %d rows", n)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO comfy_workflow_versions (workflow_id, version, graph, manifest, manifest_hash, created_at, updated_at)
		VALUES (1, '1.0.0', '{}', '{}', 'h', now(), now())`); !db.IsUniqueViolation(err) {
		t.Fatalf("duplicate (workflow_id, version): err = %v, want unique violation", err)
	}

	exec(`INSERT INTO deployment_workflows (deployment_id, workflow_id, workflow_version_id, added_at) VALUES ('dep-000000000001', 1, 1, now())`)
	if n := count(`SELECT count(*) FROM deployment_workflows WHERE state = 'pending' AND installed_at IS NULL`); n != 1 {
		t.Fatalf("deployment_workflows defaults = %d rows", n)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO deployment_workflows (deployment_id, workflow_id, workflow_version_id, added_at)
		VALUES ('dep-000000000001', 1, 2, now())`); !db.IsUniqueViolation(err) {
		t.Fatalf("same workflow twice on a deployment: err = %v, want unique violation", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE deployment_workflows SET state = 'removed'`); !isCheck(err) {
		t.Fatalf("bad deployment_workflows.state: err = %v, want check violation", err)
	}
	exec(`UPDATE deployment_workflows SET state = 'installed', installed_at = now()`)

	exec(`INSERT INTO comfy_workflow_runs (id, deployment_id, user_id, workflow_version_id, prompt_id, seed, created_at)
		VALUES ('run-000000000001', 'dep-000000000001', 1, 1, '0b5c3a52-5d8a-4b5e-9a39-1c1f5a8a0c11', 42, now())`)
	if n := count(`SELECT count(*) FROM comfy_workflow_runs WHERE state = 'queued' AND finished_at IS NULL`); n != 1 {
		t.Fatalf("run defaults = %d rows", n)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE comfy_workflow_runs SET state = 'running'`); !isCheck(err) {
		t.Fatalf("bad run state: err = %v, want check violation", err)
	}
	if n := count(`SELECT count(*) FROM pg_indexes WHERE indexname IN ('comfy_workflow_versions_wf_idx', 'deployment_workflows_wf_idx', 'comfy_workflow_runs_dep_idx', 'comfy_workflow_runs_user_idx')`); n != 4 {
		t.Fatalf("indexes = %d", n)
	}

	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('comfy_workflows', 'comfy_workflow_versions', 'deployment_workflows', 'comfy_workflow_runs')`); n != 0 {
		t.Fatalf("tables after down = %d", n)
	}
	if n := count(`SELECT count(*) FROM app_deployments WHERE id = 'dep-000000000001'`); n != 1 {
		t.Fatalf("app_deployments after down = %d", n)
	}
	if n := count(`SELECT count(*) FROM apps`); n != 1 {
		t.Fatalf("apps after down = %d", n)
	}

	if _, err := p.UpByOne(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM comfy_workflows`); n != 0 {
		t.Fatalf("workflows after re-up = %d", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('comfy_workflows', 'comfy_workflow_versions', 'deployment_workflows', 'comfy_workflow_runs')`); n != 4 {
		t.Fatalf("tables after re-up = %d", n)
	}
}
