package referral_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/referral"
)

func (e *env) doBind(req referral.BindRequest) referral.BindResult {
	e.t.Helper()
	var res referral.BindResult
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		res, err = e.svc.Bind(e.ctx, tx, req)
		return err
	}); err != nil {
		e.t.Fatalf("bind: %v", err)
	}
	return res
}

func (e *env) bindingRow(invitee int64) (status, hold string, flags []string) {
	e.t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT status, hold_reason, risk_flags FROM referral_bindings WHERE invitee_id = $1`, invitee).Scan(&status, &hold, &flags); err != nil {
		e.t.Fatalf("binding row: %v", err)
	}
	return
}

func TestBindHappyPathAndReasons(t *testing.T) {
	e := newEnv(t)
	inviter := e.user("13812345678", true)
	a, _ := e.svc.EnsureCode(e.ctx, inviter)
	now := e.clk.Now()
	b := e.user("13900000001", false)
	res := e.doBind(referral.BindRequest{Code: " " + a.Code + " ", InviteeID: b, IP: "1.1.1.1", UserAgent: "ua", Now: now})
	if !res.Bound || res.Reason != "ok" || res.Inviter != "138****5678" {
		t.Fatalf("res = %+v", res)
	}
	status, hold, flags := e.bindingRow(b)
	if status != "active" || hold != "" || len(flags) != 0 {
		t.Fatalf("row = %s %q %v", status, hold, flags)
	}
	var until time.Time
	var ver int64
	e.pool.QueryRow(e.ctx, `SELECT rebate_until, rate_version_id FROM referral_bindings WHERE invitee_id = $1`, b).Scan(&until, &ver)
	if !until.Equal(now.Add(365*24*time.Hour)) || ver == 0 {
		t.Fatalf("rebate_until = %v ver = %d", until, ver)
	}
	var action, actorType string
	e.pool.QueryRow(e.ctx, `SELECT action, actor_type FROM audit_logs ORDER BY id DESC LIMIT 1`).Scan(&action, &actorType)
	if action != "referral.bind" || actorType != "user" {
		t.Fatalf("audit = %s %s", action, actorType)
	}
	// 同一被邀请人再绑：invalid_code，不报错
	if r := e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b, Now: now}); r.Bound || r.Reason != "invalid_code" {
		t.Fatalf("rebind = %+v", r)
	}
	// 无效码 / 垃圾码
	for _, c := range []string{"ZZZZZZZZ", "", "abc", "!!!!!!!!"} {
		if r := e.doBind(referral.BindRequest{Code: c, InviteeID: e.user("1390000"+c[:min(4, len(c))]+"0", false), Now: now}); r.Bound || r.Reason != "invalid_code" {
			t.Fatalf("code %q = %+v", c, r)
		}
	}
	// 冻结 / 封禁 → inviter_frozen
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET status = 'frozen' WHERE user_id = $1`, inviter)
	if r := e.doBind(referral.BindRequest{Code: a.Code, InviteeID: e.user("13900000002", false), Now: now}); r.Bound || r.Reason != "inviter_frozen" {
		t.Fatalf("frozen = %+v", r)
	}
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET status = 'active' WHERE user_id = $1`, inviter)
	e.pool.Exec(e.ctx, `UPDATE users SET status = 'banned' WHERE id = $1`, inviter)
	if r := e.doBind(referral.BindRequest{Code: a.Code, InviteeID: e.user("13900000003", false), Now: now}); r.Bound || r.Reason != "inviter_frozen" {
		t.Fatalf("banned = %+v", r)
	}
	// 绑定用绑定时刻的版本：改版本后再绑的人 rebate_until 不同，旧绑定不变
	e.pool.Exec(e.ctx, `UPDATE users SET status = 'active' WHERE id = $1`, inviter)
	e.pool.Exec(e.ctx, `INSERT INTO referral_rate_versions (rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, created_at) VALUES (1000, 1, 1, 7, 30, $1, $1)`, now)
	c := e.user("13900000004", false)
	e.doBind(referral.BindRequest{Code: a.Code, InviteeID: c, Now: now})
	var untilC time.Time
	e.pool.QueryRow(e.ctx, `SELECT rebate_until FROM referral_bindings WHERE invitee_id = $1`, c).Scan(&untilC)
	if !untilC.Equal(now.Add(30 * 24 * time.Hour)) {
		t.Fatalf("new version period = %v", untilC)
	}
	e.pool.QueryRow(e.ctx, `SELECT rebate_until FROM referral_bindings WHERE invitee_id = $1`, b).Scan(&until)
	if !until.Equal(now.Add(365 * 24 * time.Hour)) {
		t.Fatal("old binding period changed")
	}
}

func TestBindRiskFlagsMatrix(t *testing.T) {
	e := newEnv(t)
	inviter := e.user("13812345678", true)
	a, _ := e.svc.EnsureCode(e.ctx, inviter)
	now := e.clk.Now()
	e.pool.Exec(e.ctx, `UPDATE users SET last_login_ip = '8.8.8.8', last_login_at = $2 WHERE id = $1`, inviter, now.Add(-23*time.Hour))
	e.users.devices[inviter] = map[string]bool{"dev-a": true}

	// same_ip（24h 内）→ 只标记，active
	b1 := e.user("13900000011", false)
	e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b1, IP: "8.8.8.8", Now: now})
	if st, _, flags := e.bindingRow(b1); st != "active" || len(flags) != 1 || flags[0] != "same_ip" {
		t.Fatalf("same_ip = %s %v", st, flags)
	}
	// 同 IP 但超过 24h → 不标记
	e.pool.Exec(e.ctx, `UPDATE users SET last_login_at = $2 WHERE id = $1`, inviter, now.Add(-25*time.Hour))
	b2 := e.user("13900000012", false)
	e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b2, IP: "8.8.8.8", Now: now})
	if _, _, flags := e.bindingRow(b2); len(flags) != 0 {
		t.Fatalf("stale ip = %v", flags)
	}
	// same_device → held
	b3 := e.user("13900000013", false)
	res := e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b3, DeviceID: "dev-a", Now: now})
	if st, hold, flags := e.bindingRow(b3); !res.Bound || st != "held" || hold != "same_device" || flags[0] != "same_device" {
		t.Fatalf("same_device = %+v %s %q %v", res, st, hold, flags)
	}
	// burst：已有 3 条，再造到第 19 条不标记，第 20 条标记
	for i := 4; i <= 19; i++ {
		u := e.user(fmt.Sprintf("1390000%04d", 100+i), false) // 与 b1..b3、b20、b21 不重号（preflight Q2）
		e.doBind(referral.BindRequest{Code: a.Code, InviteeID: u, Now: now})
		if _, _, flags := e.bindingRow(u); len(flags) != 0 {
			t.Fatalf("binding #%d flagged %v", i, flags)
		}
	}
	b20 := e.user("13900000120", false)
	e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b20, Now: now})
	if st, _, flags := e.bindingRow(b20); st != "active" || len(flags) != 1 || flags[0] != "burst" {
		t.Fatalf("burst = %s %v", st, flags)
	}
	// 24 小时后的绑定不再算 burst
	e.clk.Advance(25 * time.Hour)
	b21 := e.user("13900000121", false)
	e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b21, Now: e.clk.Now()})
	if _, _, flags := e.bindingRow(b21); len(flags) != 0 {
		t.Fatalf("after window = %v", flags)
	}
	_ = dbtest.T0
}
