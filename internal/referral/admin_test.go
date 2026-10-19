package referral_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/referral"
)

func (e *env) lastAudit() string {
	e.t.Helper()
	var action string
	if err := e.pool.QueryRow(e.ctx, `SELECT action FROM audit_logs ORDER BY id DESC LIMIT 1`).Scan(&action); err != nil {
		e.t.Fatal(err)
	}
	return action
}

// Review Focus 4：冻结期间到期返利不发放（继续累计），解冻后下一轮发放。
func TestFreezeBlocksReleaseUntilUnfreeze(t *testing.T) {
	e := newEnv(t)
	admin := audit.Actor{AdminID: 9, IP: "10.0.0.1"}
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.processPay(e.pay(b, "ord-1", 6900))
	if err := e.svc.Freeze(e.ctx, admin, a, "  可疑  "); err != nil || e.lastAudit() != "referral.freeze" {
		t.Fatalf("freeze = %v audit = %s", err, e.lastAudit())
	}
	var status, reason string
	e.pool.QueryRow(e.ctx, `SELECT status, frozen_reason FROM referral_accounts WHERE user_id = $1`, a).Scan(&status, &reason)
	if status != "frozen" || reason != "可疑" {
		t.Fatalf("account = %s %q", status, reason)
	}
	if err := e.svc.Freeze(e.ctx, admin, a, "again"); !errors.Is(err, apperr.ErrReferralBindingState) {
		t.Fatalf("double freeze = %v", err)
	}
	if err := e.svc.Freeze(e.ctx, admin, a, ""); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("empty reason = %v", err)
	}
	if err := e.svc.Freeze(e.ctx, admin, 999999, "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	// 冻结中：码不可绑定（LookupCode 无效）、到期不发放、新消费仍计提
	if valid, _, _ := e.svc.LookupCode(e.ctx, mustCode(e, a)); valid {
		t.Fatal("frozen code must be invalid")
	}
	e.clk.Advance(8 * 24 * time.Hour)
	e.processPay(e.pay(b, "ord-2", 6900))
	if n, _ := e.svc.Release(e.ctx); n != 0 || e.reward("ord-1")["status"] != "pending" || e.reward("ord-2")["reward"] != int64(690) {
		t.Fatalf("frozen: released=%d r1=%v r2=%v", n, e.reward("ord-1"), e.reward("ord-2"))
	}
	if err := e.svc.Unfreeze(e.ctx, admin, a); err != nil || e.lastAudit() != "referral.unfreeze" {
		t.Fatalf("unfreeze = %v", err)
	}
	if err := e.svc.Unfreeze(e.ctx, admin, a); !errors.Is(err, apperr.ErrReferralBindingState) {
		t.Fatalf("double unfreeze = %v", err)
	}
	if n, _ := e.svc.Release(e.ctx); n != 1 || e.reward("ord-1")["status"] != "settled" {
		t.Fatalf("after unfreeze released=%d", n)
	}
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
}

func mustCode(e *env, userID int64) string {
	e.t.Helper()
	a, err := e.svc.EnsureCode(e.ctx, userID)
	if err != nil {
		e.t.Fatal(err)
	}
	return a.Code
}

func TestApproveAndRevokeStateMachine(t *testing.T) {
	e := newEnv(t)
	admin := audit.Actor{AdminID: 9}
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	bid := e.bind(a, b)
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET status = 'held', hold_reason = 'same_device' WHERE id = $1`, bid)
	e.processPay(e.pay(b, "ord-1", 6900))
	e.clk.Advance(8 * 24 * time.Hour)
	if n, _ := e.svc.Release(e.ctx); n != 0 {
		t.Fatal("held must not release")
	}
	// approve：held → active，下一轮发放
	if err := e.svc.Approve(e.ctx, admin, bid, "核实为家人"); err != nil || e.lastAudit() != "referral.approve" {
		t.Fatalf("approve = %v", err)
	}
	st, hold, _ := e.bindingRow(b)
	if st != "active" || hold != "" {
		t.Fatalf("after approve = %s %q", st, hold)
	}
	if err := e.svc.Approve(e.ctx, admin, bid, "again"); !errors.Is(err, apperr.ErrReferralBindingState) {
		t.Fatalf("approve active = %v", err)
	}
	if err := e.svc.Approve(e.ctx, admin, 999999, "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("approve unknown = %v", err)
	}
	if n, _ := e.svc.Release(e.ctx); n != 1 {
		t.Fatal("approved must release")
	}
	// revoke：已发放的 ord-1 不动；待发放的 ord-2 作废；voided = 690
	e.processPay(e.pay(b, "ord-2", 6900))
	voided, err := e.svc.Revoke(e.ctx, admin, bid, "刷单")
	if err != nil || voided != 690 || e.lastAudit() != "referral.revoke" {
		t.Fatalf("revoke = %d %v", voided, err)
	}
	if e.reward("ord-1")["status"] != "settled" || e.reward("ord-2")["status"] != "void" {
		t.Fatalf("after revoke r1=%v r2=%v", e.reward("ord-1"), e.reward("ord-2"))
	}
	if st, hold, _ := e.bindingRow(b); st != "revoked" || hold != "刷单" {
		t.Fatalf("binding = %s %q", st, hold)
	}
	if _, err := e.svc.Revoke(e.ctx, admin, bid, "again"); !errors.Is(err, apperr.ErrReferralBindingState) {
		t.Fatalf("double revoke = %v", err)
	}
	if _, err := e.svc.Revoke(e.ctx, admin, bid, " "); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("empty reason = %v", err)
	}
	// 撤销后的消费：void 占位，不计提；撤销前已发放订单的退款仍扣回
	e.processPay(e.pay(b, "ord-3", 6900))
	if ev := e.events("ord-3"); len(ev) != 1 || ev[0] != "void" {
		t.Fatalf("post-revoke pay = %v", ev)
	}
	e.processRefund(e.refund(b, "ord-1-r1", 6900))
	if e.ledgerSum(a, "referral_clawback") != -690 {
		t.Fatalf("clawback after revoke = %d", e.ledgerSum(a, "referral_clawback"))
	}
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
}

// reviewOf 读绑定的复核字段。
func (e *env) reviewOf(bindingID int64) (by *int64, at *time.Time, note string) {
	e.t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT reviewed_by, reviewed_at, review_note FROM referral_bindings WHERE id = $1`, bindingID).Scan(&by, &at, &note); err != nil {
		e.t.Fatal(err)
	}
	return
}

// Review Focus 4：撤销把该绑定的 pending 全部作废（返回作废金额、写 void 事件），已发放的不动（不扣回、余额不变）；
// held 绑定同样可撤销；撤销后不再发放。
func TestRevokeVoidsPendingKeepsSettled(t *testing.T) {
	e := newEnv(t)
	admin := audit.Actor{AdminID: 9, IP: "10.0.0.1"}
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	c := e.user("13900000002", true)
	bid := e.bind(a, b)
	bidC := e.bind(a, c)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.clk.Advance(8 * 24 * time.Hour)
	if n := e.release(); n != 1 || e.reward("ord-1")["status"] != "settled" {
		t.Fatalf("setup release = %d %v", n, e.reward("ord-1"))
	}
	e.accrueOK(e.pay(b, "ord-2", 6900))  // 690
	e.accrueOK(e.pay(b, "ord-3", 12000)) // 1200
	e.accrueOK(e.pay(c, "ord-c", 6900))  // 另一绑定，不受影响
	balance, _ := e.wallet.Balance(e.ctx, a)
	if _, err := e.svc.Revoke(e.ctx, admin, 999999, "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("revoke unknown = %v", err)
	}
	voided, err := e.svc.Revoke(e.ctx, admin, bid, "  刷单  ")
	if err != nil || voided != 690+1200 {
		t.Fatalf("revoke = %d %v", voided, err)
	}
	r1 := e.reward("ord-1")
	if r1["status"] != "settled" || r1["settled"] != int64(690) || r1["clawed"] != int64(0) {
		t.Fatalf("settled reward touched: %v", r1)
	}
	if got, _ := e.wallet.Balance(e.ctx, a); got != balance || e.ledgerSum(a, "referral_clawback") != 0 {
		t.Fatalf("revoke must not claw back settled: balance %d → %d", balance, got)
	}
	for _, o := range []string{"ord-2", "ord-3"} {
		if ev := e.events(o); e.reward(o)["status"] != "void" || len(ev) != 2 || ev[1] != "void" {
			t.Fatalf("%s: %v events=%v", o, e.reward(o), ev)
		}
	}
	if e.reward("ord-c")["status"] != "pending" {
		t.Fatalf("other binding touched: %v", e.reward("ord-c"))
	}
	if st, hold, _ := e.bindingRow(b); st != "revoked" || hold != "刷单" {
		t.Fatalf("binding = %s %q", st, hold)
	}
	if by, at, note := e.reviewOf(bid); by == nil || *by != 9 || at == nil || !at.Equal(e.clk.Now()) || note != "刷单" {
		t.Fatalf("review = %v %v %q", by, at, note)
	}
	var actorType, diff string
	if err := e.pool.QueryRow(e.ctx, `SELECT actor_type, diff::text FROM audit_logs WHERE action = 'referral.revoke' AND target_type = 'referral_binding' AND target_id = $1`,
		strconv.FormatInt(bid, 10)).Scan(&actorType, &diff); err != nil || actorType != "admin" || !strings.Contains(diff, `"voided": 1890`) {
		t.Fatalf("audit = %s %s %v", actorType, diff, err)
	}
	e.clk.Advance(8 * 24 * time.Hour)
	if n := e.release(); n != 1 || e.reward("ord-c")["status"] != "settled" || e.reward("ord-2")["status"] != "void" {
		t.Fatalf("after revoke release = %d", n)
	}
	// held 绑定也可撤销
	e.bindExec(`UPDATE referral_bindings SET status = 'held', hold_reason = 'same_device' WHERE id = $1`, bidC)
	e.accrueOK(e.pay(c, "ord-c2", 6900))
	if voided, err := e.svc.Revoke(e.ctx, admin, bidC, "同设备"); err != nil || voided != 690 || e.reward("ord-c2")["status"] != "void" {
		t.Fatalf("revoke held = %d %v", voided, err)
	}
	e.invariantsOK()
}

// Approve 必须写 reviewed_at：发放把 kyc_check_failed 审计晚于 COALESCE(reviewed_at, bound_at) 视为未解决，
// 不写则通过后的下一轮又被搁置。端到端：Release 搁置 → Approve → 下一轮 Release 发放。
func TestApproveResolvesKYCCheckHold(t *testing.T) {
	e := newEnv(t)
	admin := audit.Actor{AdminID: 9}
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	bid := e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.clk.Advance(time.Second)
	e.kycCheckFailed(b)
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 0 {
		t.Fatalf("unresolved kyc check must hold: %d", n)
	}
	if st, hold, _ := e.bindingRow(b); st != "held" || hold != "kyc_check_failed" {
		t.Fatalf("binding = %s %q", st, hold)
	}
	if err := e.svc.Approve(e.ctx, admin, bid, "  人工核实非同一人  "); err != nil {
		t.Fatal(err)
	}
	if by, at, note := e.reviewOf(bid); by == nil || *by != 9 || at == nil || !at.Equal(e.clk.Now()) || note != "人工核实非同一人" {
		t.Fatalf("review = %v %v %q", by, at, note)
	}
	if n := e.release(); n != 1 || e.reward("ord-1")["status"] != "settled" || e.ledgerSum(a, "referral_rebate") != 690 {
		t.Fatalf("approved binding must release: n=%d %v", n, e.reward("ord-1"))
	}
	if st, _, _ := e.bindingRow(b); st != "active" {
		t.Fatalf("binding re-held after approve: %s", st)
	}
	e.invariantsOK()
}

// Review Focus 4：发放在 referral_accounts 锁内复核账户状态——预筛时还是 active、冻结在发放等锁期间提交，也不发。
func TestFreezeCommittedWhileReleaseWaitsBlocksPayment(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.clk.Advance(8 * 24 * time.Hour)

	// 模拟进行中的 Freeze 事务：持账户锁、已改 frozen、未提交（预筛读到的仍是 active）。
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `SELECT 1 FROM referral_accounts WHERE user_id = $1 FOR UPDATE`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `UPDATE referral_accounts SET status = 'frozen', frozen_reason = 'x' WHERE user_id = $1`, a); err != nil {
		t.Fatal(err)
	}
	type res struct {
		n   int
		err error
	}
	done := make(chan res, 1)
	go func() {
		n, err := e.svc.Release(e.ctx)
		done <- res{n, err}
	}()
	e.waitLockWaiters(1)
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.n != 0 || e.reward("ord-1")["status"] != "pending" || e.ledgerSum(a, "referral_rebate") != 0 {
		t.Fatalf("release during freeze = %d %v %v", r.n, r.err, e.reward("ord-1"))
	}
}

func TestAdminListsSummaryAndExports(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	x := e.user("13700000000", true)
	b := e.user("13900000001", true)
	c := e.user("13900000002", false)
	e.bind(a, b)
	cb := e.bind(a, c)
	e.bindExec(`UPDATE referral_bindings SET status = 'held', hold_reason = 'same_device', risk_flags = '{same_ip,same_device}' WHERE id = $1`, cb)
	e.bind(x, e.user("13900000003", false))
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.accrueOK(e.pay(b, "ord-2", 6900))
	e.clk.Advance(8 * 24 * time.Hour)
	if n := e.release(); n != 2 {
		t.Fatalf("release = %d", n)
	}
	e.refundOK(e.refund(b, "ord-2-r1", 6900)) // 扣回 690：A 1380 → 690
	e.spend(a, 690)                           // 花光再退一次 → 欠账 690
	e.refundOK(e.refund(b, "ord-1-r1", 6900))

	sum, err := e.svc.Summary(e.ctx)
	if err != nil || sum.Pending != 0 || sum.SettledNet != 0 || sum.Debt != 690 || sum.HeldCount != 1 || sum.MonthSettled != 1380 {
		t.Fatalf("summary = %+v %v", sum, err)
	}
	net, err := e.svc.MonthSettledNet(e.ctx)
	if err != nil || net != 1380-690 {
		t.Fatalf("month net = %d %v", net, err)
	}
	list, total, err := e.svc.AdminListInviters(e.ctx, referral.InviterFilter{}, paging.Page{Number: 1, Size: 10})
	if err != nil || total != 2 || list[0].UserID != a || list[0].Invitees != 2 || list[0].HeldCount != 1 || list[0].Debt != 690 || list[0].Phone != "13812345678" {
		t.Fatalf("inviters = %+v %d %v", list, total, err)
	}
	if _, total, _ := e.svc.AdminListInviters(e.ctx, referral.InviterFilter{Q: "5678"}, paging.Page{Number: 1, Size: 10}); total != 1 {
		t.Fatal("q by phone tail")
	}
	if l, total, _ := e.svc.AdminListInviters(e.ctx, referral.InviterFilter{Q: strconv.FormatInt(x, 10)}, paging.Page{Number: 1, Size: 10}); total != 1 || l[0].UserID != x {
		t.Fatal("q by id")
	}
	if _, total, _ := e.svc.AdminListInviters(e.ctx, referral.InviterFilter{Flag: "same_device"}, paging.Page{Number: 1, Size: 10}); total != 1 {
		t.Fatal("flag filter")
	}
	if l, _, err := e.svc.AdminListInviters(e.ctx, referral.InviterFilter{Sort: "invitees"}, paging.Page{Number: 1, Size: 10}); err != nil || l[0].UserID != a {
		t.Fatalf("sort invitees = %+v %v", l, err)
	}
	for _, f := range []referral.InviterFilter{{Sort: "bogus"}, {Status: "bogus"}, {Flag: "bogus"}} {
		if _, _, err := e.svc.AdminListInviters(e.ctx, f, paging.Page{Number: 1, Size: 10}); err == nil {
			t.Fatalf("bogus filter %+v accepted", f)
		}
	}
	det, err := e.svc.AdminInviter(e.ctx, a)
	if err != nil || det.Phone != "13812345678" || len(det.Bindings) != 2 || len(det.Rewards) != 2 || len(det.Events) < 6 {
		t.Fatalf("detail = %+v %v", det, err)
	}
	if _, err := e.svc.AdminInviter(e.ctx, b); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("non-inviter = %v", err)
	}
	held, total, _ := e.svc.AdminListBindings(e.ctx, referral.BindingFilter{Status: "held"}, paging.Page{Number: 1, Size: 10})
	if total != 1 || held[0].InviteeID != c || held[0].InviteePhone != "13900000002" || len(held[0].RiskFlags) != 2 || held[0].KYCCheckUnresolved {
		t.Fatalf("held = %+v", held)
	}
	if _, _, err := e.svc.AdminListBindings(e.ctx, referral.BindingFilter{Status: "bogus"}, paging.Page{Number: 1, Size: 10}); err == nil {
		t.Fatal("bogus binding status accepted")
	}
	var rows []referral.Binding
	if err := e.svc.ExportBindings(e.ctx, referral.BindingFilter{InviterID: a}, func(b referral.Binding) error { rows = append(rows, b); return nil }); err != nil || len(rows) != 2 {
		t.Fatalf("export bindings = %d %v", len(rows), err)
	}
	rw, total, _ := e.svc.AdminListRewards(e.ctx, referral.RewardFilter{InviteeID: b, Status: "settled"}, paging.Page{Number: 1, Size: 10})
	if total != 2 || rw[0].InviterPhone != "13812345678" || rw[0].InviteePhone != "13900000001" || rw[0].ClawedAmount != 690 {
		t.Fatalf("rewards = %+v", rw)
	}
	var exported []referral.AdminReward
	if err := e.svc.ExportRewards(e.ctx, referral.RewardFilter{InviterID: a}, func(v referral.AdminReward) error { exported = append(exported, v); return nil }); err != nil || len(exported) != 2 {
		t.Fatalf("export rewards = %d %v", len(exported), err)
	}
	us, err := e.svc.UserSummary(e.ctx, b)
	if err != nil || us.Inviter == nil || us.Inviter.UserID != a || us.Inviter.BindingStatus != "active" || us.Account != nil {
		t.Fatalf("user summary invitee = %+v %v", us, err)
	}
	us, _ = e.svc.UserSummary(e.ctx, a)
	if us.Inviter != nil || us.Account == nil || us.Account.Invitees != 2 || us.Account.Debt != 690 {
		t.Fatalf("user summary inviter = %+v", us)
	}
}

// 待审核队列标出每条绑定是否有晚于 COALESCE(reviewed_at, bound_at) 的未解决 referral.kyc_check_failed 审计；
// Approve 把这一事实写进审计 diff。绑定之前的失败不算。
func TestHeldQueueShowsUnresolvedKYCCheck(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	c := e.user("13900000002", true)
	e.kycCheckFailed(c) // 绑定之前：与绑定无关
	e.clk.Advance(time.Second)
	bb := e.bind(a, b)
	cb := e.bind(a, c)
	e.bindExec(`UPDATE referral_bindings SET status = 'held', hold_reason = 'kyc_check_failed' WHERE id = ANY($1)`, []int64{bb, cb})
	e.clk.Advance(time.Second)
	e.kycCheckFailed(b)

	held, _, err := e.svc.AdminListBindings(e.ctx, referral.BindingFilter{Status: "held"}, paging.Page{Number: 1, Size: 10})
	if err != nil || len(held) != 2 {
		t.Fatalf("held = %+v %v", held, err)
	}
	got := map[int64]bool{}
	for _, h := range held {
		got[h.ID] = h.KYCCheckUnresolved
	}
	if !got[bb] || got[cb] {
		t.Fatalf("kyc_check_unresolved = %v (want %d true, %d false)", got, bb, cb)
	}
	diffOf := func(id int64) string {
		var d string
		if err := e.pool.QueryRow(e.ctx, `SELECT diff::text FROM audit_logs WHERE action = 'referral.approve' AND target_id = $1`, strconv.FormatInt(id, 10)).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	e.clk.Advance(time.Second)
	if err := e.svc.Approve(e.ctx, audit.Actor{AdminID: 9}, bb, "核实"); err != nil {
		t.Fatal(err)
	}
	if d := diffOf(bb); !strings.Contains(d, `"kyc_check_unresolved": true`) {
		t.Fatalf("approve diff = %s", d)
	}
	if err := e.svc.Approve(e.ctx, audit.Actor{AdminID: 9}, cb, "核实"); err != nil {
		t.Fatal(err)
	}
	if d := diffOf(cb); !strings.Contains(d, `"kyc_check_unresolved": false`) {
		t.Fatalf("approve diff = %s", d)
	}
	// 通过后 reviewed_at 晚于该审计：不再显示未解决
	list, _, _ := e.svc.AdminListBindings(e.ctx, referral.BindingFilter{InviteeID: b}, paging.Page{Number: 1, Size: 10})
	if len(list) != 1 || list[0].KYCCheckUnresolved {
		t.Fatalf("after approve = %+v", list)
	}
}
