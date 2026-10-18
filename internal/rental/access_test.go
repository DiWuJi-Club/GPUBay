package rental_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/rental"
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

func TestPlaceGeneratesHostKeyAndResetPassword(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	got := e.get(inst.ID)
	if !strings.HasPrefix(got.SSHHostPubkey, "ssh-ed25519 ") || strings.ContainsAny(got.SSHHostPubkey, "\n") {
		t.Fatalf("host pubkey = %q", got.SSHHostPubkey)
	}
	pem, err := e.svc.HostKeyPEM(e.ctx, inst.ID)
	if err != nil || !strings.Contains(pem, "OPENSSH PRIVATE KEY") {
		t.Fatalf("pem = %q %v", pem, err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(pem))
	if err != nil || strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) != got.SSHHostPubkey {
		t.Fatalf("pem does not match pubkey: %v", err)
	}
	if _, err := e.svc.HostKeyPEM(e.ctx, "ins-nope0000"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	old, _ := e.svc.Password(e.ctx, e.userID, inst.ID)
	pw, err := e.svc.ResetPassword(e.ctx, e.userID, inst.ID, "9.9.9.9")
	if err != nil || len(pw) != 16 || pw == old {
		t.Fatalf("reset = %q %v (old %q)", pw, err, old)
	}
	if now, _ := e.svc.Password(e.ctx, e.userID, inst.ID); now != pw {
		t.Fatalf("stored password = %q, want %q", now, pw)
	}
	if _, err := e.svc.ResetPassword(e.ctx, e.userID+1, inst.ID, ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user reset = %v", err)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE actor_type = 'user' AND action = 'instance.password_reset' AND target_id = $1`, inst.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("audit = %d", n)
	}
}

func TestFailedAndReleasedClearHostKey(t *testing.T) {
	e := newEnv(t, 2)
	_, failed := e.place(catalog.D1d)
	e.ack(false) // create 失败 → failed
	if pem, err := e.svc.HostKeyPEM(e.ctx, failed.ID); err != nil || pem != "" {
		t.Fatalf("failed instance pem = %q %v", pem, err)
	}
	_, inst := e.place(catalog.D1d)
	e.ack(true) // running
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	e.ack(true) // wipe → released
	if st := e.get(inst.ID).State; st != rental.StateReleased {
		t.Fatalf("state = %s", st)
	}
	if pem, err := e.svc.HostKeyPEM(e.ctx, inst.ID); err != nil || pem != "" {
		t.Fatalf("released pem = %q %v", pem, err)
	}
	if _, err := e.svc.ResetPassword(e.ctx, e.userID, inst.ID, ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("reset released = %v", err)
	}
}

func TestPasswordAuthToggleRequiresKey(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if _, err := e.svc.SetPasswordAuth(e.ctx, e.userID, inst.ID, false, ""); !errors.Is(err, apperr.ErrPasswordAuthRequiresKey) {
		t.Fatalf("disable without key = %v", err)
	}
	e.keys.n = 1
	got, err := e.svc.SetPasswordAuth(e.ctx, e.userID, inst.ID, false, "1.1.1.1")
	if err != nil || got.PasswordAuth {
		t.Fatalf("disable = %+v %v", got, err)
	}
	e.keys.n = 0 // 删光钥匙后仍可重新打开密码登录
	if got, err = e.svc.SetPasswordAuth(e.ctx, e.userID, inst.ID, true, ""); err != nil || !got.PasswordAuth {
		t.Fatalf("enable = %+v %v", got, err)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = 'instance.password_auth' AND target_id = $1`, inst.ID).Scan(&n)
	if n != 2 {
		t.Fatalf("audit = %d", n)
	}
}

func TestMigrateCopiesPasswordAuthAndNewHostKey(t *testing.T) {
	e := newEnv(t, 2)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.keys.n = 1
	if _, err := e.svc.SetPasswordAuth(e.ctx, e.userID, inst.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	moved, err := e.svc.AdminMigrate(e.ctx, audit.Actor{AdminID: 1}, inst.ID, "bad gpu")
	if err != nil {
		t.Fatal(err)
	}
	got := e.get(moved.ID)
	if got.PasswordAuth || got.SSHHostPubkey == "" || got.SSHHostPubkey == e.get(inst.ID).SSHHostPubkey {
		t.Fatalf("moved = %+v", got)
	}
}

func TestAccessInfoAndTOFU(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	info, err := e.svc.Access(e.ctx, inst.ID)
	if err != nil || info.Instance.State != rental.StateRunning || len(info.Password) != 16 || info.MachineLanIP != "10.0.0.10" ||
		!info.AgentOnline || info.MachineSerial != "SN000" || info.RetainUntil != nil || info.Instance.SSHHostPubkey == "" {
		t.Fatalf("access = %+v %v", info, err)
	}
	if _, err := e.svc.Access(e.ctx, "ins-nope0000"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	// 存量实例：公钥为空时首连写回，之后不再覆盖
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET ssh_host_pubkey = '' WHERE id = $1`, inst.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.svc.SetSSHHostPubkeyIfEmpty(e.ctx, inst.ID, "ssh-ed25519 AAAAfirst"); err != nil || !ok {
		t.Fatalf("tofu first = %v %v", ok, err)
	}
	if ok, err := e.svc.SetSSHHostPubkeyIfEmpty(e.ctx, inst.ID, "ssh-ed25519 AAAAsecond"); err != nil || ok {
		t.Fatalf("tofu second = %v %v", ok, err)
	}
	if e.get(inst.ID).SSHHostPubkey != "ssh-ed25519 AAAAfirst" {
		t.Fatal("tofu overwritten")
	}
	// 到期停机后 RetainUntil 有值
	e.clock.Advance(25 * time.Hour)
	if _, err := e.svc.ExpireDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	info, _ = e.svc.Access(e.ctx, inst.ID)
	if info.Instance.State != rental.StateStopped || info.RetainUntil == nil || !info.RetainUntil.Equal(e.clock.Now().Add(rental.RetainAfterStop)) {
		t.Fatalf("stopped access = %+v", info)
	}
}

func TestSSHSessionsLifecycleAndReap(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	id, err := e.svc.OpenSSHSession(e.ctx, rental.SSHSessionStart{InstanceID: inst.ID, UserID: e.userID, MachineID: e.machines[0], AuthMethod: "password", ClientIP: "203.0.113.9"})
	if err != nil || id == 0 {
		t.Fatalf("open = %d %v", id, err)
	}
	if n, _ := e.svc.OpenSSHSessionCount(e.ctx); n != 1 {
		t.Fatalf("open count = %d", n)
	}
	e.clock.Advance(time.Minute)
	if err := e.svc.TouchSSHSessions(e.ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.ListSSHSessions(e.ctx, inst.ID, 20)
	if err != nil || len(list) != 1 || !list[0].LastSeenAt.Equal(e.clock.Now()) || list[0].EndedAt != nil {
		t.Fatalf("list = %+v %v", list, err)
	}
	if err := e.svc.CloseSSHSession(e.ctx, id, 100, 2000); err != nil {
		t.Fatal(err)
	}
	list, _ = e.svc.ListSSHSessions(e.ctx, inst.ID, 20)
	if list[0].EndedAt == nil || list[0].BytesUp != 100 || list[0].BytesDown != 2000 {
		t.Fatalf("closed = %+v", list[0])
	}
	// 崩溃残留：open 后 5 分钟没刷新 → 清扫收尾 ended_at = last_seen_at
	stale, _ := e.svc.OpenSSHSession(e.ctx, rental.SSHSessionStart{InstanceID: inst.ID, UserID: e.userID, MachineID: e.machines[0], AuthMethod: "publickey", ClientIP: "203.0.113.9"})
	seen := e.clock.Now()
	e.clock.Advance(rental.SSHSessionStaleAfter + time.Second)
	closed, deleted, err := e.svc.ReapSSHSessions(e.ctx)
	if err != nil || closed != 1 || deleted != 0 {
		t.Fatalf("reap = %d %d %v", closed, deleted, err)
	}
	page, total, err := e.svc.AdminListSSHSessions(e.ctx, inst.ID, paging.Page{Number: 1, Size: 10})
	if err != nil || total != 2 || page[0].ID != stale || page[0].EndedAt == nil || !page[0].EndedAt.Equal(seen) {
		t.Fatalf("admin list = %+v %d %v", page, total, err)
	}
	e.clock.Advance(rental.SSHSessionRetention)
	if _, deleted, _ := e.svc.ReapSSHSessions(e.ctx); deleted != 2 {
		t.Fatalf("retention deleted = %d", deleted)
	}
	if n, _ := e.svc.OpenSSHSessionCount(e.ctx); n != 0 {
		t.Fatalf("open count after reap = %d", n)
	}
}

func TestAccessAlertsAreStableAndOnce(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	for i := 0; i < 2; i++ {
		if err := e.svc.RaiseSSHBruteforce(e.ctx, inst.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.RaiseUpstreamUnreachable(e.ctx, inst.ID); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind IN ('ssh_bruteforce', 'upstream_unreachable')`).Scan(&n)
	if n != 2 {
		t.Fatalf("alerts = %d, want 2 (deduplicated)", n)
	}
	var msg string
	_ = e.pool.QueryRow(e.ctx, `SELECT message FROM alerts WHERE kind = 'upstream_unreachable'`).Scan(&msg)
	if msg != rental.AlertUpstreamUnreachable("SN000", inst.ID) || msg != "机器 SN000 上实例 "+inst.ID+" 的 SSH 上游无法连接" {
		t.Fatalf("message = %q", msg)
	}
}
