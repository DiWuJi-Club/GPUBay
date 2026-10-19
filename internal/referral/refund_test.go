package referral_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/referral"
	"spark-cloud/internal/wallet"
)

func (e *env) refundTx(l referral.LedgerRowForTest) error {
	return db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error { return e.svc.ApplyRefundForTest(e.ctx, tx, e.clk.Now(), l) })
}

func (e *env) processRefund(ledgerID int64) error {
	e.t.Helper()
	return e.refundTx(e.ledgerOf(ledgerID))
}

// refundOK：processRefund 且必须成功。
func (e *env) refundOK(ledgerID int64) {
	e.t.Helper()
	if err := e.processRefund(ledgerID); err != nil {
		e.t.Fatalf("refund ledger %d: %v", ledgerID, err)
	}
}

// markSettled 直接把 reward 置为已发放并给邀请人入账（发放逻辑由 Task 9 测）。
func (e *env) markSettled(order string, inviter int64) {
	e.t.Helper()
	var reward int64
	if err := e.pool.QueryRow(e.ctx, `SELECT reward FROM referral_rewards WHERE order_id = $1`, order).Scan(&reward); err != nil {
		e.t.Fatal(err)
	}
	e.bindExec(`UPDATE referral_rewards SET status = 'settled', settled_amount = reward, settled_at = $2 WHERE order_id = $1`, order, e.clk.Now())
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := e.wallet.Apply(e.ctx, tx, wallet.Entry{UserID: inviter, Amount: reward, Kind: wallet.KindReferralRebate, RefType: "referral", RefID: order})
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// spend 让用户花掉余额（管理员调减；dbtest.Fund 不能扣减）。
func (e *env) spend(userID, amount int64) {
	e.t.Helper()
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := e.wallet.Apply(e.ctx, tx, wallet.Entry{UserID: userID, Amount: -amount, Kind: wallet.KindAdminAdjust, Remark: "test spend"})
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) invariantsOK() {
	e.t.Helper()
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		e.t.Fatal(err)
	}
}

func TestOrderIDOf(t *testing.T) {
	for in, want := range map[string]string{"ord-abc": "ord-abc", "ord-abc-r1": "ord-abc", "ord-abc-r12": "ord-abc", "ord-abc-rx": "ord-abc-rx", "ord-r1-r2": "ord-r1"} {
		if got := referral.OrderIDOf(in); got != want {
			t.Errorf("OrderIDOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRefundPendingOnlyChangesNumbers(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))       // 690
	e.refundOK(e.refund(b, "ord-1-r1", 3000)) // 剩 3900 → 390
	r := e.reward("ord-1")
	if r["status"] != "pending" || r["reward"] != int64(390) || r["refunded"] != int64(3000) {
		t.Fatalf("after r1 = %v", r)
	}
	e.refundOK(e.refund(b, "ord-1-r2", 3900)) // 全退 → 0，仍 pending
	if r := e.reward("ord-1"); r["reward"] != int64(0) || r["status"] != "pending" || r["refunded"] != int64(6900) {
		t.Fatalf("after r2 = %v", r)
	}
	if ev := e.events("ord-1"); len(ev) != 3 || ev[1] != "refund" || ev[2] != "refund" {
		t.Fatalf("events = %v", ev)
	}
	if e.ledgerSum(a, "referral_clawback") != 0 {
		t.Fatal("pending refund must not touch wallet")
	}
	// 开通失败退款（ref_id 无后缀）同样处理
	e.accrueOK(e.pay(b, "ord-2", 6900))
	e.refundOK(e.refund(b, "ord-2", 6900))
	if r := e.reward("ord-2"); r["reward"] != int64(0) {
		t.Fatalf("refundAll = %v", r)
	}
	e.invariantsOK()
}

func TestRefundSettledClawsBackAndRecordsDebt(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.markSettled("ord-1", a) // A 余额 690
	// 退 3000：target 390，excess 300，余额够 → 扣 300
	e.refundOK(e.refund(b, "ord-1-r1", 3000))
	r := e.reward("ord-1")
	if r["status"] != "settled" || r["reward"] != int64(390) || r["clawed"] != int64(300) || r["settled"] != int64(690) {
		t.Fatalf("after r1 = %v", r)
	}
	if e.ledgerSum(a, "referral_clawback") != -300 || e.debt(a) != 0 {
		t.Fatalf("wallet = %d debt = %d", e.ledgerSum(a, "referral_clawback"), e.debt(a))
	}
	var refID, remark string
	var ledgerID, eventLedgerID int64
	if err := e.pool.QueryRow(e.ctx, `SELECT id, ref_id, remark FROM wallet_ledger WHERE kind = 'referral_clawback' ORDER BY id DESC LIMIT 1`).Scan(&ledgerID, &refID, &remark); err != nil {
		t.Fatal(err)
	}
	if refID != "ord-1-c1" || remark != "返利扣回 139****0001 订单 ord-1" {
		t.Fatalf("ledger = %q %q", refID, remark)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT wallet_ledger_id FROM referral_events WHERE order_id = 'ord-1' AND kind = 'clawback'`).Scan(&eventLedgerID); err != nil || eventLedgerID != ledgerID {
		t.Fatalf("clawback event wallet_ledger_id = %d, want %d (%v)", eventLedgerID, ledgerID, err)
	}
	// A 把余额花光（390 → 0），再退 2000：target = floor(1900×0.1) = 190，excess 200，余额 0 → 欠账 200
	e.spend(a, 390)
	e.refundOK(e.refund(b, "ord-1-r2", 2000))
	r = e.reward("ord-1")
	if r["reward"] != int64(190) || r["clawed"] != int64(500) || e.debt(a) != 200 {
		t.Fatalf("after r2 = %v debt = %d", r, e.debt(a))
	}
	if ev := e.events("ord-1"); len(ev) != 6 || ev[3] != "refund" || ev[4] != "debt_add" || ev[5] != "clawback" {
		t.Fatalf("events = %v", ev)
	}
	// 第三次：余额有 100，excess 190（全退），扣 100 欠 90 → 欠账 290；ref -c3
	dbtest.Fund(e.t, e.pool, a, 100)
	e.refundOK(e.refund(b, "ord-1-r3", 1900))
	if r := e.reward("ord-1"); r["reward"] != int64(0) || r["clawed"] != int64(690) || e.debt(a) != 290 {
		t.Fatalf("after r3 = %v debt = %d", r, e.debt(a))
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM wallet_ledger WHERE kind = 'referral_clawback' AND ref_id IN ('ord-1-c1', 'ord-1-c3')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("clawback ledger refs = %d (%v)", n, err)
	}
	e.invariantsOK()
	// 同一退款流水重放：无变化
	var lid int64
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM wallet_ledger WHERE ref_id = 'ord-1-r3'`).Scan(&lid); err != nil {
		t.Fatal(err)
	}
	e.refundOK(lid)
	if e.debt(a) != 290 || len(e.events("ord-1")) != 9 { // accrue, refund, clawback, refund, debt_add, clawback, refund, debt_add, clawback
		t.Fatalf("replay changed state: debt=%d events=%v", e.debt(a), e.events("ord-1"))
	}
	e.invariantsOK()
}

// 余额为 0 时 ApplyUpTo 不写钱包流水（返回 0），钱包唯一索引兜不住重放；幂等只能来自 referral_events 的
// source_ledger_id 与账户锁后的复查。顺序重放与并发重放都不得重复记欠账。
func TestRefundZeroBalanceReplayNoDoubleDebt(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.markSettled("ord-1", a)
	e.spend(a, 690) // 余额 0
	l := e.ledgerOf(e.refund(b, "ord-1-r1", 3000))
	if err := e.refundTx(l); err != nil {
		t.Fatal(err)
	}
	if e.debt(a) != 300 || e.ledgerSum(a, "referral_clawback") != 0 || e.reward("ord-1")["clawed"] != int64(300) {
		t.Fatalf("debt = %d clawback = %d reward = %v", e.debt(a), e.ledgerSum(a, "referral_clawback"), e.reward("ord-1"))
	}
	var lid *int64
	if err := e.pool.QueryRow(e.ctx, `SELECT wallet_ledger_id FROM referral_events WHERE order_id = 'ord-1' AND kind = 'clawback'`).Scan(&lid); err != nil || lid != nil {
		t.Fatalf("zero-applied clawback must have no wallet_ledger_id: %v %v", lid, err)
	}
	// 顺序重放
	if err := e.refundTx(l); err != nil {
		t.Fatal(err)
	}
	if e.debt(a) != 300 || len(e.events("ord-1")) != 4 {
		t.Fatalf("sequential replay: debt = %d events = %v", e.debt(a), e.events("ord-1"))
	}
	e.invariantsOK()

	// 并发重放一条新退款：两个事务都越过无锁的预检，在邀请人账户锁上排队；后者锁后复查看到已处理 → nil，不撞唯一索引。
	l2 := e.ledgerOf(e.refund(b, "ord-1-r2", 2000)) // target 190，excess 200
	hold, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(e.ctx)
	if _, err := hold.Exec(e.ctx, `SELECT 1 FROM referral_accounts WHERE user_id = $1 FOR UPDATE`, a); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- e.refundTx(l2) }()
	}
	e.waitLockWaiters(2)
	if err := hold.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent refund = %v", err)
		}
	}
	if e.debt(a) != 500 || e.reward("ord-1")["clawed"] != int64(500) || len(e.events("ord-1")) != 7 {
		t.Fatalf("concurrent replay: debt = %d reward = %v events = %v", e.debt(a), e.reward("ord-1"), e.events("ord-1"))
	}
	e.invariantsOK()
}

// 余额充足时的重放：钱包唯一索引（-c<n>）与 referral_events 的复查都在；顺序重放与并发重放都只扣一次。
func TestRefundNonZeroBalanceReplayClawsOnce(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.markSettled("ord-1", a)
	dbtest.Fund(e.t, e.pool, a, 1000) // 余额 1690
	l := e.ledgerOf(e.refund(b, "ord-1-r1", 3000))
	if err := e.refundTx(l); err != nil {
		t.Fatal(err)
	}
	if e.ledgerSum(a, "referral_clawback") != -300 || e.debt(a) != 0 || len(e.events("ord-1")) != 3 {
		t.Fatalf("first: clawback = %d debt = %d events = %v", e.ledgerSum(a, "referral_clawback"), e.debt(a), e.events("ord-1"))
	}
	// 顺序重放
	if err := e.refundTx(l); err != nil {
		t.Fatal(err)
	}
	if e.ledgerSum(a, "referral_clawback") != -300 || e.reward("ord-1")["clawed"] != int64(300) || len(e.events("ord-1")) != 3 {
		t.Fatalf("sequential replay: clawback = %d reward = %v events = %v", e.ledgerSum(a, "referral_clawback"), e.reward("ord-1"), e.events("ord-1"))
	}
	e.invariantsOK()

	// 并发重放一条新退款：target 190，excess 200
	l2 := e.ledgerOf(e.refund(b, "ord-1-r2", 2000))
	hold, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(e.ctx) }()
	if _, err := hold.Exec(e.ctx, `SELECT 1 FROM referral_accounts WHERE user_id = $1 FOR UPDATE`, a); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- e.refundTx(l2) }()
	}
	e.waitLockWaiters(2)
	if err := hold.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent refund = %v", err)
		}
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM wallet_ledger WHERE kind = 'referral_clawback' AND ref_id = 'ord-1-c2'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("-c2 rows = %d %v", n, err)
	}
	if e.ledgerSum(a, "referral_clawback") != -500 || e.debt(a) != 0 || e.reward("ord-1")["clawed"] != int64(500) || len(e.events("ord-1")) != 5 {
		t.Fatalf("concurrent replay: clawback = %d debt = %d reward = %v events = %v", e.ledgerSum(a, "referral_clawback"), e.debt(a), e.reward("ord-1"), e.events("ord-1"))
	}
	e.invariantsOK()
}

// Review Focus 3：计提后费率版本改为 20%，退款重算仍按存的 10% 与 reward_cap。
func TestRefundUsesStoredRateNotCurrentVersion(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.bindExec(`INSERT INTO referral_rate_versions (rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, created_at) VALUES (2000, 50000, 500000, 7, 365, $1, $1)`, e.clk.Now())
	e.clk.Advance(time.Hour)
	e.refundOK(e.refund(b, "ord-1-r1", 3000))
	if r := e.reward("ord-1"); r["reward"] != int64(390) {
		t.Fatalf("must use stored 10%%: %v", r)
	}
	// reward_cap 也按存的：被邀请人上限把 ord-2 的 1380 压到 610，退款后取 min(reward_cap, 剩余 × 存的 20%)
	e.bindExec(`UPDATE referral_rate_versions SET max_per_invitee = 1000`)
	e.accrueOK(e.pay(b, "ord-2", 6900)) // 版本 2（20%）：raw 1380，room 1000 − 390 = 610 → 610
	e.accrueOK(e.pay(b, "ord-3", 6900)) // room 1000 − 390 − 610 = 0 → 0
	e.refundOK(e.refund(b, "ord-2-r1", 3000))
	if r := e.reward("ord-2"); r["cap"] != int64(610) || r["reward"] != int64(610) { // min(610, 3900×20%=780)
		t.Fatalf("ord-2 after r1 = %v", r)
	}
	e.refundOK(e.refund(b, "ord-2-r2", 2000))
	if r := e.reward("ord-2"); r["reward"] != int64(380) { // min(610, 1900×20%)
		t.Fatalf("ord-2 after r2 = %v", r)
	}
	e.invariantsOK()
}

// Review Focus 1：退款先于 / 没有计提。
func TestRefundWithoutRewardDeferredOrPlaceholdered(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	bid := e.bind(a, b)
	// 没有任何事件的订单：延后（errSkip），不写事件
	e.pay(b, "ord-1", 6900)
	l := e.refund(b, "ord-1-r1", 1000)
	if err := e.processRefund(l); !errors.Is(err, referral.ErrSkip) {
		t.Fatalf("want skip, got %v", err)
	}
	if len(e.events("ord-1")) != 0 {
		t.Fatal("deferred refund must not write events")
	}
	// 返利期后的订单只有 capped 占位 → 退款写 refund(0) 占位
	e.bindExec(`UPDATE referral_bindings SET rebate_until = $2 WHERE id = $1`, bid, e.clk.Now().Add(-time.Second))
	e.accrueOK(e.pay(b, "ord-2", 6900))
	rl := e.refund(b, "ord-2-r1", 1000)
	e.refundOK(rl)
	if ev := e.events("ord-2"); len(ev) != 2 || ev[0] != "capped" || ev[1] != "refund" {
		t.Fatalf("placeholder = %v", ev)
	}
	e.refundOK(rl) // 占位同样幂等
	if ev := e.events("ord-2"); len(ev) != 2 {
		t.Fatalf("placeholder replay = %v", ev)
	}
	// 撤销绑定前已发放的订单，撤销后退款仍扣回
	e.bindExec(`UPDATE referral_bindings SET rebate_until = $2 WHERE id = $1`, bid, e.clk.Now().Add(365*24*time.Hour))
	e.accrueOK(e.pay(b, "ord-3", 6900))
	e.markSettled("ord-3", a)
	e.bindExec(`UPDATE referral_bindings SET status = 'revoked' WHERE id = $1`, bid)
	e.refundOK(e.refund(b, "ord-3-r1", 6900))
	if r := e.reward("ord-3"); r["clawed"] != int64(690) || e.ledgerSum(a, "referral_clawback") != -690 {
		t.Fatalf("revoked binding refund = %v wallet = %d", r, e.ledgerSum(a, "referral_clawback"))
	}
	// 撤销后的订单只有 void 占位 → 退款写 refund(0) 占位
	e.accrueOK(e.pay(b, "ord-4", 6900))
	e.refundOK(e.refund(b, "ord-4-r1", 6900))
	if ev := e.events("ord-4"); len(ev) != 2 || ev[0] != "void" || ev[1] != "refund" {
		t.Fatalf("void placeholder = %v", ev)
	}
}

// void 的 reward 退款只记 refunded_amount，reward 不动；累计退款超过订单额（数据异常）时夹到订单额。
func TestRefundVoidRewardAndClamp(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.bindExec(`UPDATE referral_rewards SET status = 'void' WHERE order_id = 'ord-1'`)
	e.refundOK(e.refund(b, "ord-1-r1", 3000))
	if r := e.reward("ord-1"); r["status"] != "void" || r["reward"] != int64(690) || r["refunded"] != int64(3000) {
		t.Fatalf("void after refund = %v", r)
	}
	// void 行不再欠也不再付：refund 事件金额记 0，而不是累计差额
	var amt int64
	if err := e.pool.QueryRow(e.ctx, `SELECT amount FROM referral_events WHERE order_id = 'ord-1' AND kind = 'refund'`).Scan(&amt); err != nil || amt != 0 {
		t.Fatalf("void refund event amount = %d %v", amt, err)
	}
	e.accrueOK(e.pay(b, "ord-2", 6900))
	e.refundOK(e.refund(b, "ord-2-r1", 5000))
	e.refundOK(e.refund(b, "ord-2-r2", 5000))
	if r := e.reward("ord-2"); r["refunded"] != int64(6900) || r["reward"] != int64(0) {
		t.Fatalf("clamped = %v", r)
	}
}

func TestApplyRefundRejectsNonRefundRow(t *testing.T) {
	e := newEnv(t)
	for _, l := range []referral.LedgerRowForTest{
		referral.LedgerRowOf(1, 1, "order_pay", 100, "ord-1", e.clk.Now()),
		referral.LedgerRowOf(1, 1, "refund", -100, "ord-1-r1", e.clk.Now()),
	} {
		if err := e.refundTx(l); err == nil || errors.Is(err, referral.ErrSkip) {
			t.Fatalf("%+v: want error, got %v", l, err)
		}
	}
}

// CheckInvariants 对每条不变量都能查出破坏（在回滚的事务里制造破坏）。
func TestCheckInvariantsDetectsViolations(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.markSettled("ord-1", a)
	e.refundOK(e.refund(b, "ord-1-r1", 3000)) // settled 690，clawed 300
	e.accrueOK(e.pay(b, "ord-2", 6900))       // pending 690
	e.invariantsOK()
	for _, c := range []struct{ name, sql string }{
		{"inv1", `UPDATE referral_accounts SET debt = debt + 1`},
		{"inv2", `UPDATE referral_rewards SET reward = reward + 1 WHERE order_id = 'ord-1'`},
		{"inv2", `UPDATE referral_rewards SET settled_amount = 1 WHERE order_id = 'ord-2'`},
		{"inv2", `UPDATE referral_rewards SET status = 'void' WHERE order_id = 'ord-1'`},
		{"inv3", `UPDATE referral_rewards SET refunded_amount = refunded_amount + 1 WHERE order_id = 'ord-1'`},
		{"inv4 unprocessed order_pay", `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at) SELECT invitee_id, -1, 0, 'order_pay', 'order', 'ord-x', '', now() FROM referral_bindings`},
		{"inv4 unprocessed refund", `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at) SELECT invitee_id, 1, 0, 'refund', 'order', 'ord-2-r1', '', now() FROM referral_bindings`},
		{"inv4 event kind", `UPDATE referral_events SET kind = 'settle' WHERE kind = 'accrue' AND order_id = 'ord-2'`},
		{"inv6 reward formula", `UPDATE referral_rewards SET reward = reward - 1 WHERE order_id = 'ord-2'`},
		{"inv6 reward formula", `UPDATE referral_rewards SET reward_cap = 1 WHERE order_id = 'ord-2'`},
		{"inv6 reward formula", `UPDATE referral_rewards SET refunded_amount = 0 WHERE order_id = 'ord-1'`},
		{"inv5 per-invitee", `UPDATE referral_rate_versions SET max_per_invitee = 1`},
		{"inv5 per-inviter", `UPDATE referral_rate_versions SET max_per_inviter_30d = 1`},
	} {
		tx, err := e.pool.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(e.ctx, c.sql); err != nil {
			if rerr := tx.Rollback(e.ctx); rerr != nil {
				t.Errorf("rollback: %v", rerr)
			}
			t.Fatalf("%s: %v", c.sql, err)
		}
		err = e.svc.CheckInvariants(e.ctx, tx)
		if rerr := tx.Rollback(e.ctx); rerr != nil {
			t.Fatalf("%s: rollback: %v", c.sql, rerr)
		}
		if err == nil || !strings.Contains(err.Error(), c.name) {
			t.Errorf("%s: want violation %q, got %v", c.sql, c.name, err)
		}
	}
	e.invariantsOK()
}
