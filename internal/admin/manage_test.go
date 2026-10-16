package admin_test

import (
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
)

func ptrRole(r admin.Role) *admin.Role { return &r }
func ptrStr(s string) *string          { return &s }

func TestListAndFind(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	e.create("alice", admin.RoleOps)
	list, err := e.svc.List(e.ctx)
	if err != nil || len(list) != 2 || list[0].Username != "root" || list[1].Role != admin.RoleOps {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if a, err := e.svc.FindByUsername(e.ctx, " Alice "); err != nil || a.Username != "alice" {
		t.Fatalf("find = %+v, %v", a, err)
	}
	if _, err := e.svc.FindByUsername(e.ctx, "nobody"); codeOf(err) != "not_found" {
		t.Fatalf("find missing = %v", err)
	}
}

func TestUpdateProtectsLastSuper(t *testing.T) {
	e := newEnv(t)
	root := e.create("root", admin.RoleSuper)
	actor := audit.Actor{AdminID: root.ID, IP: ip}
	if _, err := e.svc.Update(e.ctx, actor, root.ID, ptrRole(admin.RoleOps), nil); codeOf(err) != "last_super" {
		t.Fatalf("demote last super = %v", err)
	}
	if _, err := e.svc.Update(e.ctx, actor, root.ID, nil, ptrStr(admin.StatusDisabled)); codeOf(err) != "last_super" {
		t.Fatalf("disable last super = %v", err)
	}
	second := e.create("boss", admin.RoleSuper)
	a, err := e.svc.Update(e.ctx, actor, root.ID, ptrRole(admin.RoleOps), nil)
	if err != nil || a.Role != admin.RoleOps {
		t.Fatalf("demote with another super = %+v, %v", a, err)
	}
	if _, err := e.svc.Update(e.ctx, actor, second.ID, nil, ptrStr(admin.StatusDisabled)); codeOf(err) != "last_super" {
		t.Fatalf("disable the remaining super = %v", err)
	}
	if _, err := e.svc.Update(e.ctx, actor, root.ID, ptrRole(admin.Role("god")), nil); codeOf(err) != "bad_request" {
		t.Fatalf("bad role = %v", err)
	}
	if _, err := e.svc.Update(e.ctx, actor, root.ID, nil, ptrStr("gone")); codeOf(err) != "bad_request" {
		t.Fatalf("bad status = %v", err)
	}
	if _, err := e.svc.Update(e.ctx, actor, 9999, ptrRole(admin.RoleOps), nil); codeOf(err) != "not_found" {
		t.Fatalf("missing admin = %v", err)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = 'admin.update' AND actor_id = $1`, root.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("admin.update audit rows = %d, want 1", n)
	}
}

// 两个超级管理员同时互相降级，只能成功一个。
func TestConcurrentDemotionKeepsOneSuper(t *testing.T) {
	e := newEnv(t)
	a := e.create("root", admin.RoleSuper)
	b := e.create("boss", admin.RoleSuper)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []int64{a.ID, b.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.svc.Update(e.ctx, audit.Actor{}, id, ptrRole(admin.RoleOps), nil)
		}()
	}
	wg.Wait()
	var supers int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM admins WHERE role = 'super' AND status = 'active'`).Scan(&supers)
	if supers != 1 || (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("supers = %d, errs = %v", supers, errs)
	}
}

func TestDisableKillsSessionsAndBlocksLogin(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	ops := e.create("alice", admin.RoleOps)
	_, _, token := e.enroll("alice")
	if _, err := e.svc.Update(e.ctx, audit.Actor{}, ops.ID, nil, ptrStr(admin.StatusDisabled)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(e.ctx, token); codeOf(err) != "unauthorized" {
		t.Fatalf("session after disable = %v", err)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM admin_sessions WHERE admin_id = $1`, ops.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("sessions left = %d", n)
	}
	if _, err := e.svc.Login(e.ctx, "alice", password, ip); codeOf(err) != "invalid_credentials" {
		t.Fatalf("login while disabled = %v", err)
	}
	if _, err := e.svc.Update(e.ctx, audit.Actor{}, ops.ID, nil, ptrStr(admin.StatusActive)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Login(e.ctx, "alice", password, ip); err != nil {
		t.Fatalf("login after re-enable = %v", err)
	}
}

func TestResetTOTPForcesRebinding(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	ops := e.create("alice", admin.RoleOps)
	oldSecret, _, token := e.enroll("alice")
	if err := e.svc.ResetTOTP(e.ctx, audit.Actor{AdminID: 1, IP: ip}, ops.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(e.ctx, token); codeOf(err) != "unauthorized" {
		t.Fatalf("session after reset = %v", err)
	}
	e.clk.Advance(30 * time.Second)
	res, err := e.svc.Login(e.ctx, "alice", password, ip)
	if err != nil || !res.NeedTOTPSetup {
		t.Fatalf("login after reset = %+v, %v", res, err)
	}
	if _, _, err := e.svc.ConfirmTOTPSetup(e.ctx, res.Token, e.code(oldSecret, 0), ip); codeOf(err) != "invalid_totp" {
		t.Fatalf("old secret still accepted: %v", err)
	}
	var codes int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM admin_recovery_codes WHERE admin_id = $1`, ops.ID).Scan(&codes)
	if codes != 0 {
		t.Fatalf("recovery codes left = %d", codes)
	}
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, token := e.enroll("root")
	other := e.login("root", secret)
	sess, _ := e.svc.Authenticate(e.ctx, token)
	if err := e.svc.ChangePassword(e.ctx, sess, "wrong password!", "a brand new password", ip); codeOf(err) != "wrong_password" {
		t.Fatalf("wrong old password = %v", err)
	}
	if err := e.svc.ChangePassword(e.ctx, sess, password, "short", ip); codeOf(err) != "bad_request" {
		t.Fatalf("short new password = %v", err)
	}
	if err := e.svc.ChangePassword(e.ctx, sess, password, "a brand new password", ip); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(e.ctx, token); err != nil {
		t.Fatalf("current session must survive: %v", err)
	}
	if _, err := e.svc.Authenticate(e.ctx, other); codeOf(err) != "unauthorized" {
		t.Fatalf("other session must be revoked: %v", err)
	}
	if _, err := e.svc.Login(e.ctx, "root", password, ip); codeOf(err) != "invalid_credentials" {
		t.Fatalf("old password still works: %v", err)
	}
	if _, err := e.svc.Login(e.ctx, "root", "a brand new password", ip); err != nil {
		t.Fatalf("new password = %v", err)
	}
}

func TestEnrollmentWithoutSession(t *testing.T) {
	e := newEnv(t)
	a := e.create("root", admin.RoleSuper)
	uri, err := e.svc.EnrollTOTP(e.ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	uri2, _ := e.svc.EnrollTOTP(e.ctx, a.ID)
	if uri != uri2 {
		t.Fatal("EnrollTOTP must reuse the pending secret")
	}
	secret := secretFromURI(t, uri)
	if _, err := e.svc.ConfirmEnrollment(e.ctx, a.ID, "000000"); codeOf(err) != "invalid_totp" {
		t.Fatalf("wrong code = %v", err)
	}
	codes, err := e.svc.ConfirmEnrollment(e.ctx, a.ID, e.code(secret, 0))
	if err != nil || len(codes) != admin.RecoveryCodeCount {
		t.Fatalf("confirm = %v, %v", codes, err)
	}
	if _, err := e.svc.EnrollTOTP(e.ctx, a.ID); codeOf(err) != "invalid_state" {
		t.Fatalf("enroll after confirm = %v", err)
	}
	e.login("root", secret)
}
