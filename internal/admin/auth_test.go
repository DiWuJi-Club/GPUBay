package admin_test

import (
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
)

func TestCreateValidatesAndAudits(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		username, password string
		role               admin.Role
		want               string
	}{
		{"ab", password, admin.RoleSuper, "bad_request"},
		{"1root", password, admin.RoleSuper, "bad_request"},
		{"root", "short", admin.RoleSuper, "bad_request"},
		{"root", password, admin.Role("god"), "bad_request"},
	}
	for _, c := range cases {
		if _, err := e.svc.Create(e.ctx, audit.Actor{}, c.username, c.password, c.role); codeOf(err) != c.want {
			t.Fatalf("Create(%q, role %q) = %v, want %s", c.username, c.role, err, c.want)
		}
	}
	a := e.create(" Root ", admin.RoleSuper)
	if a.Username != "root" || a.Role != admin.RoleSuper || a.Status != admin.StatusActive || a.TOTPConfirmed {
		t.Fatalf("admin = %+v", a)
	}
	if _, err := e.svc.Create(e.ctx, audit.Actor{}, "root", password, admin.RoleOps); codeOf(err) != "duplicate_username" {
		t.Fatalf("duplicate = %v", err)
	}
	var actorType, action string
	if err := e.pool.QueryRow(e.ctx, `SELECT actor_type, action FROM audit_logs WHERE target_type = 'admin'`).Scan(&actorType, &action); err != nil {
		t.Fatal(err)
	}
	if actorType != "system" || action != "admin.create" {
		t.Fatalf("audit = %s %s", actorType, action)
	}
	var hash string
	if err := e.pool.QueryRow(e.ctx, `SELECT password_hash FROM admins WHERE id = $1`, a.ID).Scan(&hash); err != nil || !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("password hash = %q, %v", hash, err)
	}
}

func TestFirstLoginRequiresTOTPSetup(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	res, err := e.svc.Login(e.ctx, "root", password, ip)
	if err != nil || !res.NeedTOTPSetup || !strings.HasPrefix(res.OTPAuthURI, "otpauth://totp/Spark%20Cloud:root?") || res.Token == "" {
		t.Fatalf("login = %+v, %v", res, err)
	}
	// 密码阶段的会话不能访问后台，也不能跳过绑定直接验证动态码
	if _, err := e.svc.Authenticate(e.ctx, res.Token); codeOf(err) != "unauthorized" {
		t.Fatalf("authenticate pending = %v", err)
	}
	if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, "123456", ip); codeOf(err) != "totp_setup_required" {
		t.Fatalf("verify before setup = %v", err)
	}
	// 再次登录拿到的是同一个待绑定密钥
	res2, err := e.svc.Login(e.ctx, "root", password, ip)
	if err != nil || res2.OTPAuthURI != res.OTPAuthURI {
		t.Fatalf("second login uri changed: %v", err)
	}
	if _, _, err := e.svc.ConfirmTOTPSetup(e.ctx, res.Token, "000000", ip); codeOf(err) != "invalid_totp" {
		t.Fatalf("wrong setup code = %v", err)
	}

	secret, codes, token := e.enroll("root")
	if len(codes) != admin.RecoveryCodeCount || secret == "" {
		t.Fatalf("codes = %v", codes)
	}
	sess, err := e.svc.Authenticate(e.ctx, token)
	if err != nil || sess.Admin.Username != "root" || !sess.Admin.TOTPConfirmed || sess.Admin.LastLoginIP != ip {
		t.Fatalf("session = %+v, %v", sess, err)
	}
	e.clk.Advance(30 * time.Second)
	if res, err := e.svc.Login(e.ctx, "root", password, ip); err != nil || res.NeedTOTPSetup || res.OTPAuthURI != "" {
		t.Fatalf("login after setup = %+v, %v", res, err)
	}
}

func TestVerifyTOTPWindowAndReplay(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, _ := e.enroll("root")
	// 绑定时用掉了时间片 S；快进到 S+2，使 S+1 仍在窗口内且未被使用
	e.clk.Advance(60 * time.Second)

	res, _ := e.svc.Login(e.ctx, "root", password, ip)
	if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, e.code(secret, -2), ip); codeOf(err) != "invalid_totp" {
		t.Fatalf("code two steps old = %v", err)
	}
	code := e.code(secret, -1)
	if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, code, ip); err != nil {
		t.Fatalf("code one step old = %v", err)
	}
	// 同一动态码不能再用于另一次登录
	res2, _ := e.svc.Login(e.ctx, "root", password, ip)
	if _, err := e.svc.VerifyTOTP(e.ctx, res2.Token, code, ip); codeOf(err) != "totp_used" {
		t.Fatalf("replayed code = %v", err)
	}
	if _, err := e.svc.VerifyTOTP(e.ctx, res2.Token, e.code(secret, 0), ip); err != nil {
		t.Fatalf("fresh code = %v", err)
	}
	// 已激活的会话不能再走第二步
	if _, err := e.svc.VerifyTOTP(e.ctx, res2.Token, e.code(secret, 1), ip); codeOf(err) != "unauthorized" {
		t.Fatalf("verify on active session = %v", err)
	}
}

func TestWrongPasswordLocksAccount(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	if _, err := e.svc.Login(e.ctx, "nobody", password, ip); codeOf(err) != "invalid_credentials" {
		t.Fatalf("unknown user = %v", err)
	}
	for i := 0; i < admin.MaxFailures; i++ {
		if _, err := e.svc.Login(e.ctx, "root", "wrong password!", ip); codeOf(err) != "invalid_credentials" {
			t.Fatalf("attempt %d = %v", i+1, err)
		}
	}
	if _, err := e.svc.Login(e.ctx, "root", password, ip); codeOf(err) != "account_locked" {
		t.Fatalf("after 5 failures = %v", err)
	}
	e.clk.Advance(admin.LockDuration)
	if _, err := e.svc.Login(e.ctx, "root", password, ip); err != nil {
		t.Fatalf("after lock expiry = %v", err)
	}
}

func TestTOTPFailuresCountTowardLock(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, _ := e.enroll("root")
	e.clk.Advance(30 * time.Second)
	res, _ := e.svc.Login(e.ctx, "root", password, ip)
	for i := 0; i < admin.MaxFailures; i++ {
		if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, "000000", ip); codeOf(err) != "invalid_totp" {
			t.Fatalf("attempt %d = %v", i+1, err)
		}
	}
	if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, e.code(secret, 0), ip); codeOf(err) != "account_locked" {
		t.Fatalf("correct code while locked = %v", err)
	}
	if _, err := e.svc.Login(e.ctx, "root", password, ip); codeOf(err) != "account_locked" {
		t.Fatalf("password while locked = %v", err)
	}
}

func TestSuccessfulLoginResetsFailures(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, _ := e.enroll("root")
	for i := 0; i < admin.MaxFailures-1; i++ {
		_, _ = e.svc.Login(e.ctx, "root", "wrong password!", ip)
	}
	e.login("root", secret)
	for i := 0; i < admin.MaxFailures-1; i++ {
		if _, err := e.svc.Login(e.ctx, "root", "wrong password!", ip); codeOf(err) != "invalid_credentials" {
			t.Fatalf("attempt %d after reset = %v", i+1, err)
		}
	}
}

func TestRecoveryCodeIsSingleUse(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	_, codes, _ := e.enroll("root")
	res, _ := e.svc.Login(e.ctx, "root", password, ip)
	if _, err := e.svc.VerifyRecoveryCode(e.ctx, res.Token, " "+strings.ToUpper(codes[0])+" ", ip); err != nil {
		t.Fatalf("recovery code = %v", err)
	}
	res2, _ := e.svc.Login(e.ctx, "root", password, ip)
	if _, err := e.svc.VerifyRecoveryCode(e.ctx, res2.Token, codes[0], ip); codeOf(err) != "invalid_totp" {
		t.Fatalf("reused recovery code = %v", err)
	}
	if _, err := e.svc.VerifyRecoveryCode(e.ctx, res2.Token, codes[1], ip); err != nil {
		t.Fatalf("second recovery code = %v", err)
	}
}

func TestPendingSessionExpires(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, _ := e.enroll("root")
	e.clk.Advance(30 * time.Second)
	res, _ := e.svc.Login(e.ctx, "root", password, ip)
	e.clk.Advance(admin.PendingTTL + time.Second)
	if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, e.code(secret, 0), ip); codeOf(err) != "unauthorized" {
		t.Fatalf("expired pending session = %v", err)
	}
}

func TestSessionIdleTimeout(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	_, _, token := e.enroll("root")
	e.clk.Advance(admin.IdleTimeout - time.Minute)
	if _, err := e.svc.Authenticate(e.ctx, token); err != nil {
		t.Fatalf("before idle timeout = %v", err)
	}
	e.clk.Advance(admin.IdleTimeout - time.Minute) // 上一次访问刷新了空闲计时
	if _, err := e.svc.Authenticate(e.ctx, token); err != nil {
		t.Fatalf("activity should extend idle timeout: %v", err)
	}
	e.clk.Advance(admin.IdleTimeout + time.Second)
	if _, err := e.svc.Authenticate(e.ctx, token); codeOf(err) != "unauthorized" {
		t.Fatalf("after idle timeout = %v", err)
	}
}

func TestSessionAbsoluteExpiry(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	_, _, token := e.enroll("root")
	for elapsed := time.Duration(0); elapsed < admin.SessionTTL-time.Hour; elapsed += time.Hour {
		e.clk.Advance(time.Hour)
		if _, err := e.svc.Authenticate(e.ctx, token); err != nil {
			t.Fatalf("at %v: %v", elapsed+time.Hour, err)
		}
	}
	e.clk.Advance(time.Hour)
	if _, err := e.svc.Authenticate(e.ctx, token); codeOf(err) != "unauthorized" {
		t.Fatalf("after 12h = %v", err)
	}
}

func TestStepUp(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, token := e.enroll("root")
	sess, _ := e.svc.Authenticate(e.ctx, token)
	if err := e.svc.StepUp(e.ctx, sess, ""); codeOf(err) != "totp_required" {
		t.Fatalf("no code, no prior step-up = %v", err)
	}
	e.clk.Advance(30 * time.Second)
	if err := e.svc.StepUp(e.ctx, sess, "000000"); codeOf(err) != "invalid_totp" {
		t.Fatalf("wrong code = %v", err)
	}
	if err := e.svc.StepUp(e.ctx, sess, e.code(secret, 0)); err != nil {
		t.Fatalf("right code = %v", err)
	}
	// 只校验不开窗口：窗口由 RecordStepUp 在操作成功后打开
	sess, _ = e.svc.Authenticate(e.ctx, token)
	if sess.StepUpAt != nil {
		t.Fatal("StepUp must not record the window by itself")
	}
	if err := e.svc.StepUp(e.ctx, sess, ""); codeOf(err) != "totp_required" {
		t.Fatalf("verified but not recorded = %v", err)
	}
	if err := e.svc.RecordStepUp(e.ctx, sess); err != nil {
		t.Fatal(err)
	}
	sess, _ = e.svc.Authenticate(e.ctx, token)
	if sess.StepUpAt == nil {
		t.Fatal("StepUpAt not recorded")
	}
	e.clk.Advance(admin.StepUpWindow - time.Second)
	sess, _ = e.svc.Authenticate(e.ctx, token)
	if err := e.svc.StepUp(e.ctx, sess, ""); err != nil {
		t.Fatalf("within window = %v", err)
	}
	e.clk.Advance(2 * time.Second)
	sess, _ = e.svc.Authenticate(e.ctx, token)
	if err := e.svc.StepUp(e.ctx, sess, ""); codeOf(err) != "totp_required" {
		t.Fatalf("after window = %v", err)
	}
}

// 动态码只校验不开窗口：操作失败后管理员重输同一个码是正常行为，不能计入失败次数而把自己锁住；但用过的码也绝不能再次通过。
func TestStepUpReusedCodeIsNotAFailure(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, token := e.enroll("root")
	sess, _ := e.svc.Authenticate(e.ctx, token)
	e.clk.Advance(30 * time.Second)
	code := e.code(secret, 0)
	if err := e.svc.StepUp(e.ctx, sess, code); err != nil {
		t.Fatalf("first use = %v", err)
	}
	for i := 0; i < admin.MaxFailures+1; i++ {
		if err := e.svc.StepUp(e.ctx, sess, code); codeOf(err) != "totp_used" {
			t.Fatalf("reuse %d = %v", i+1, err)
		}
	}
	// 重用不计失败：再错 MaxFailures-1 次仍未锁定
	for i := 0; i < admin.MaxFailures-1; i++ {
		if err := e.svc.StepUp(e.ctx, sess, "000000"); codeOf(err) != "invalid_totp" {
			t.Fatalf("wrong %d = %v", i+1, err)
		}
	}
	e.clk.Advance(30 * time.Second)
	if err := e.svc.StepUp(e.ctx, sess, e.code(secret, 0)); err != nil {
		t.Fatalf("next code after reuses = %v", err)
	}
}

func TestStepUpWrongCodesLockAccount(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, token := e.enroll("root")
	sess, _ := e.svc.Authenticate(e.ctx, token)
	e.clk.Advance(30 * time.Second)
	for i := 0; i < admin.MaxFailures; i++ {
		if err := e.svc.StepUp(e.ctx, sess, "000000"); codeOf(err) != "invalid_totp" {
			t.Fatalf("wrong %d = %v", i+1, err)
		}
	}
	if err := e.svc.StepUp(e.ctx, sess, e.code(secret, 0)); codeOf(err) != "account_locked" {
		t.Fatalf("right code while locked = %v", err)
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	_, _, token := e.enroll("root")
	if err := e.svc.Logout(e.ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(e.ctx, token); codeOf(err) != "unauthorized" {
		t.Fatalf("after logout = %v", err)
	}
	if _, err := e.svc.Authenticate(e.ctx, ""); codeOf(err) != "unauthorized" {
		t.Fatalf("empty token = %v", err)
	}
}

func TestTOTPSecretIsEncrypted(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, _ := e.enroll("root")
	var enc []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT totp_secret_enc FROM admins WHERE username = 'root'`).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc), secret) {
		t.Fatal("TOTP secret stored in plaintext")
	}
}
