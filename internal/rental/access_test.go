package rental_test

import (
	"testing"

	"spark-cloud/internal/catalog"
)

func TestInstanceAccessColumnsDefaults(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	got := e.get(inst.ID)
	if !got.PasswordAuth {
		t.Fatalf("password_auth default = %v, want true", got.PasswordAuth)
	}
	var portsCount, keysCount, sessCount int
	if err := e.pool.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM instance_ports), (SELECT count(*) FROM ssh_keys), (SELECT count(*) FROM ssh_sessions)`).
		Scan(&portsCount, &keysCount, &sessCount); err != nil {
		t.Fatalf("new tables missing: %v", err)
	}
	var hasCol bool
	if err := e.pool.QueryRow(e.ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'platform_pki' AND column_name = 'ssh_host_key_enc')`).Scan(&hasCol); err != nil || !hasCol {
		t.Fatalf("platform_pki.ssh_host_key_enc missing: %v", err)
	}
	// 端口范围与 22 由 CHECK 约束兜底（业务层另有校验）
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instance_ports (instance_id, port, created_at, updated_at) VALUES ($1, 22, now(), now())`, inst.ID); err == nil {
		t.Fatal("port 22 accepted by CHECK")
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instance_ports (instance_id, port, created_at, updated_at) VALUES ($1, 70000, now(), now())`, inst.ID); err == nil {
		t.Fatal("port 70000 accepted by CHECK")
	}
}
