package referral_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/referral"
)

// ledgerOf 读一条流水。
func (e *env) ledgerOf(ledgerID int64) referral.LedgerRowForTest {
	e.t.Helper()
	var l referral.LedgerRowForTest
	if err := e.pool.QueryRow(e.ctx, `SELECT id, user_id, kind, amount, ref_id, created_at FROM wallet_ledger WHERE id = $1`, ledgerID).
		Scan(&l.ID, &l.UserID, &l.Kind, &l.Amount, &l.RefID, &l.CreatedAt); err != nil {
		e.t.Fatal(err)
	}
	return l
}

func (e *env) accrueTx(l referral.LedgerRowForTest) error {
	return db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error { return e.svc.AccrueForTest(e.ctx, tx, e.clk.Now(), l) })
}

// processPay 读一条流水并在独立事务里计提（Task 10 之前的直通路径）。
func (e *env) processPay(ledgerID int64) error {
	e.t.Helper()
	return e.accrueTx(e.ledgerOf(ledgerID))
}

// accrueOK：processPay 且必须成功。
func (e *env) accrueOK(ledgerID int64) {
	e.t.Helper()
	if err := e.processPay(ledgerID); err != nil {
		e.t.Fatalf("accrue ledger %d: %v", ledgerID, err)
	}
}

func (e *env) rewardCount(order string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM referral_rewards WHERE order_id = $1`, order).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// waitLockWaiters 等到本测试库里有 n 个后端在等行锁。
func (e *env) waitLockWaiters(n int) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
			e.t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("lock waiters = %d, want %d", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAccrueUsesVersionAtPayTimeAndFloors(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	// 版本 2 在 T0+1h 生效：15%
	e.bindExec(`INSERT INTO referral_rate_versions (rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, created_at) VALUES (1500, 50000, 500000, 3, 365, $1, $1)`, e.clk.Now().Add(time.Hour))
	l1 := e.pay(b, "ord-1", 6901) // 10%：690.1 → 690
	e.clk.Advance(2 * time.Hour)
	l2 := e.pay(b, "ord-2", 6901) // 15%：1035.15 → 1035，hold 3 天
	// 计提时刻晚于两个版本：仍按付款时刻的版本
	if err := e.processPay(l1); err != nil {
		t.Fatal(err)
	}
	if err := e.processPay(l2); err != nil {
		t.Fatal(err)
	}
	r1, r2 := e.reward("ord-1"), e.reward("ord-2")
	if r1["reward"] != int64(690) || r1["cap"] != int64(690) || r1["status"] != "pending" || !r1["settle_after"].(time.Time).Equal(e.clk.Now().Add(-2*time.Hour+7*24*time.Hour)) {
		t.Fatalf("r1 = %v", r1)
	}
	if r2["reward"] != int64(1035) || !r2["settle_after"].(time.Time).Equal(e.clk.Now().Add(3*24*time.Hour)) {
		t.Fatalf("r2 = %v", r2)
	}
	var bp1, bp2 int
	var amt int64
	if err := e.pool.QueryRow(e.ctx, `SELECT (SELECT rate_bp FROM referral_rewards WHERE order_id = 'ord-1'), (SELECT rate_bp FROM referral_rewards WHERE order_id = 'ord-2'), (SELECT order_amount FROM referral_rewards WHERE order_id = 'ord-1')`).
		Scan(&bp1, &bp2, &amt); err != nil || bp1 != 1000 || bp2 != 1500 || amt != 6901 {
		t.Fatalf("rate_bp = %d %d amount = %d (%v)", bp1, bp2, amt, err)
	}
	if ev := e.events("ord-1"); len(ev) != 1 || ev[0] != "accrue" {
		t.Fatalf("events = %v", ev)
	}
	// 幂等：同一流水再处理 → 已有事件，返回 nil、不重复建行
	if err := e.processPay(l1); err != nil {
		t.Fatalf("reprocess = %v", err)
	}
	if ev := e.events("ord-1"); len(ev) != 1 {
		t.Fatalf("events after reprocess = %v", ev)
	}
}

func TestAccrueCapsPerInviteeAndPerInviter30d(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	c := e.user("13900000002", true)
	e.bind(a, b)
	e.bind(a, c)
	// 被邀请人上限 1000 分，邀请人 30 天上限 1500 分
	e.bindExec(`UPDATE referral_rate_versions SET max_per_invitee = 1000, max_per_inviter_30d = 1500`)
	e.accrueOK(e.pay(b, "ord-b1", 6900)) // 690
	e.accrueOK(e.pay(b, "ord-b2", 6900)) // room_invitee 310 → 310，capped 380
	e.accrueOK(e.pay(b, "ord-b3", 6900)) // room 0 → reward 0，capped 690
	if e.reward("ord-b2")["reward"] != int64(310) || e.reward("ord-b2")["cap"] != int64(310) || e.reward("ord-b3")["reward"] != int64(0) {
		t.Fatalf("b2/b3 = %v %v", e.reward("ord-b2"), e.reward("ord-b3"))
	}
	if ev := e.events("ord-b2"); len(ev) != 2 || ev[0] != "accrue" || ev[1] != "capped" {
		t.Fatalf("b2 events = %v", ev)
	}
	var cappedAmt int64
	if err := e.pool.QueryRow(e.ctx, `SELECT amount FROM referral_events WHERE order_id = 'ord-b3' AND kind = 'capped'`).Scan(&cappedAmt); err != nil || cappedAmt != 690 {
		t.Fatalf("b3 capped = %d %v", cappedAmt, err)
	}
	// 邀请人 30 天：已占用 pending 1000；c 的 690 → room_inviter 500 → 500
	e.accrueOK(e.pay(c, "ord-c1", 6900))
	if e.reward("ord-c1")["reward"] != int64(500) {
		t.Fatalf("c1 = %v", e.reward("ord-c1"))
	}
	// 把 b1 标记为 10 天前发放（仍在 30 天窗口内）：净额 690 占用；pending 310 + 0 + 500 = 810 → room 0
	e.bindExec(`UPDATE referral_rewards SET status = 'settled', settled_amount = 690, settled_at = $1 WHERE order_id = 'ord-b1'`, e.clk.Now().Add(-10*24*time.Hour))
	e.accrueOK(e.pay(c, "ord-c2", 6900))
	if e.reward("ord-c2")["reward"] != int64(0) {
		t.Fatalf("c2 = %v", e.reward("ord-c2"))
	}
	// 推进 31 天：b1 滑出窗口，room = 1500 − 810 = 690 → 全额；但 c 的被邀请人上限 1000 − 500 − 0 = 500 → 500
	e.clk.Advance(31 * 24 * time.Hour)
	e.accrueOK(e.pay(c, "ord-c3", 6900))
	if e.reward("ord-c3")["reward"] != int64(500) {
		t.Fatalf("c3 = %v", e.reward("ord-c3"))
	}
}

func TestAccrueBookingPeriodOverRevokedAndMissingOrder(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	bid := e.bind(a, b)
	// 预订：开始时刻在 10 天后 > 7 天 → settle_after = 开始时刻
	start := e.clk.Now().Add(10 * 24 * time.Hour)
	e.accrueOK(e.payAt(b, "ord-book", 6900, &start))
	if !e.reward("ord-book")["settle_after"].(time.Time).Equal(start) {
		t.Fatalf("booking settle_after = %v", e.reward("ord-book")["settle_after"])
	}
	// 预订开始在 2 天后 < 7 天 → settle_after = 付款 + 7 天
	soon := e.clk.Now().Add(2 * 24 * time.Hour)
	e.accrueOK(e.payAt(b, "ord-soon", 6900, &soon))
	if !e.reward("ord-soon")["settle_after"].(time.Time).Equal(e.clk.Now().Add(7 * 24 * time.Hour)) {
		t.Fatal("soon booking must use hold_days")
	}
	// 返利期结束：rebate_until 改到过去 → capped(period_over)，不建 reward
	e.bindExec(`UPDATE referral_bindings SET rebate_until = $2 WHERE id = $1`, bid, e.clk.Now().Add(-time.Second))
	e.accrueOK(e.pay(b, "ord-late", 6900))
	if e.rewardCount("ord-late") != 0 || len(e.events("ord-late")) != 1 || e.events("ord-late")[0] != "capped" {
		t.Fatalf("period over: rewards=%d events=%v", e.rewardCount("ord-late"), e.events("ord-late"))
	}
	e.bindExec(`UPDATE referral_bindings SET rebate_until = $2 WHERE id = $1`, bid, e.clk.Now().Add(365*24*time.Hour))
	// 撤销的绑定：void 占位
	e.bindExec(`UPDATE referral_bindings SET status = 'revoked' WHERE id = $1`, bid)
	e.accrueOK(e.pay(b, "ord-revoked", 6900))
	if ev := e.events("ord-revoked"); len(ev) != 1 || ev[0] != "void" || e.rewardCount("ord-revoked") != 0 {
		t.Fatalf("revoked = %v", ev)
	}
	e.bindExec(`UPDATE referral_bindings SET status = 'held' WHERE id = $1`, bid)
	// held 仍计提
	e.accrueOK(e.pay(b, "ord-held", 6900))
	if e.reward("ord-held")["reward"] != int64(690) {
		t.Fatal("held binding must still accrue")
	}
	// 订单查不到：跳过、不写事件（errSkip，事务回滚）
	l := e.pay(b, "ord-ghost", 6900)
	delete(e.orders.m, "ord-ghost")
	if err := e.processPay(l); !errors.Is(err, referral.ErrSkip) {
		t.Fatalf("ghost = %v", err)
	}
	if len(e.events("ord-ghost")) != 0 || e.rewardCount("ord-ghost") != 0 {
		t.Fatal("ghost order must not write events")
	}
	// 订单属于别人：同样跳过
	l = e.pay(b, "ord-other", 6900)
	e.orders.m["ord-other"] = fakeOrder{userID: a}
	if err := e.processPay(l); !errors.Is(err, referral.ErrSkip) || len(e.events("ord-other")) != 0 {
		t.Fatalf("other owner = %v %v", err, e.events("ord-other"))
	}
	// 非绑定用户：什么也不写
	x := e.user("13900000009", true)
	if err := e.processPay(e.pay(x, "ord-x", 6900)); err != nil || len(e.events("ord-x")) != 0 || e.rewardCount("ord-x") != 0 {
		t.Fatalf("unbound = %v", err)
	}
}

// 两个事务并发处理同一条 order_pay：都在邀请人账户锁上排队；锁后复查使后者看到已处理并返回 nil，
// 而不是撞 referral_rewards_pkey。
func TestAccrueConcurrentDuplicateIsBenign(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	l := e.ledgerOf(e.pay(b, "ord-1", 6900))

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
		go func() { errs <- e.accrueTx(l) }()
	}
	e.waitLockWaiters(2)
	if err := hold.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent accrue = %v", err)
		}
	}
	if ev := e.events("ord-1"); len(ev) != 1 || ev[0] != "accrue" || e.rewardCount("ord-1") != 1 {
		t.Fatalf("events = %v rewards = %d", ev, e.rewardCount("ord-1"))
	}
}

// 实名事务先持 users(被邀请人) FOR UPDATE 再锁 referral_accounts(邀请人)；计提必须先对 users 取 KEY SHARE
// 再锁账户（规格 §14），否则插 referral_rewards 的外键检查会形成 accounts → users 的反向等待而死锁。
func TestAccrueLocksUsersBeforeAccount(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	l := e.ledgerOf(e.pay(b, "ord-1", 6900))

	kyc, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer kyc.Rollback(e.ctx)
	if _, err := kyc.Exec(e.ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, b); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.accrueTx(l) }()
	e.waitLockWaiters(1)
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	if _, err := kyc.Exec(ctx, `SELECT 1 FROM referral_accounts WHERE user_id = $1 FOR UPDATE`, a); err != nil {
		t.Fatalf("kyc side lock account: %v", err)
	}
	if err := kyc.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("accrue = %v", err)
	}
	if e.reward("ord-1")["reward"] != int64(690) {
		t.Fatalf("reward = %v", e.reward("ord-1"))
	}
}

// accrue 只接受 order_pay 且金额为负的流水；其他行是调用方的错误，不能静默写事件。
func TestAccrueRejectsNonOrderPayRow(t *testing.T) {
	e := newEnv(t)
	for _, l := range []referral.LedgerRowForTest{
		referral.LedgerRowOf(1, 1, "refund", 100, "ord-1", e.clk.Now()),
		referral.LedgerRowOf(1, 1, "order_pay", 100, "ord-1", e.clk.Now()),
	} {
		if err := e.accrueTx(l); err == nil || errors.Is(err, referral.ErrSkip) {
			t.Fatalf("%+v: want error, got %v", l, err)
		}
	}
}

// R2 边界：created_at == rebate_until 已过返利期（capped period_over）；rebate_until − 1ns 仍计提。
func TestAccrueRebateUntilBoundary(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	bid := e.bind(a, b)
	e.clk.Advance(time.Hour)
	l := e.ledgerOf(e.pay(b, "ord-eq", 6900))
	e.bindExec(`UPDATE referral_bindings SET rebate_until = $2 WHERE id = $1`, bid, l.CreatedAt)
	if err := e.accrueTx(l); err != nil {
		t.Fatal(err)
	}
	if ev := e.events("ord-eq"); len(ev) != 1 || ev[0] != "capped" || e.rewardCount("ord-eq") != 0 {
		t.Fatalf("created_at == rebate_until: events = %v", ev)
	}
	var detail string
	if err := e.pool.QueryRow(e.ctx, `SELECT detail FROM referral_events WHERE order_id = 'ord-eq'`).Scan(&detail); err != nil || detail != "period_over" {
		t.Fatalf("detail = %q (%v)", detail, err)
	}
	l2 := e.ledgerOf(e.pay(b, "ord-before", 6900))
	l2.CreatedAt = l.CreatedAt.Add(-time.Nanosecond)
	if err := e.accrueTx(l2); err != nil {
		t.Fatal(err)
	}
	if e.reward("ord-before")["reward"] != int64(690) {
		t.Fatalf("rebate_until − 1ns must accrue: %v", e.reward("ord-before"))
	}
}

// void 的 reward 不占被邀请人上限：作废后空出的额度下次计提可用。
func TestAccrueVoidRewardFreesInviteeRoom(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.bindExec(`UPDATE referral_rate_versions SET max_per_invitee = 1000`)
	e.accrueOK(e.pay(b, "ord-1", 6900)) // 690
	e.accrueOK(e.pay(b, "ord-2", 6900)) // room 310
	if e.reward("ord-2")["reward"] != int64(310) {
		t.Fatalf("ord-2 = %v", e.reward("ord-2"))
	}
	e.bindExec(`UPDATE referral_rewards SET status = 'void' WHERE order_id = 'ord-1'`)
	e.accrueOK(e.pay(b, "ord-3", 6900)) // room 1000 − 310 = 690
	if e.reward("ord-3")["reward"] != int64(690) {
		t.Fatalf("void must free room: ord-3 = %v", e.reward("ord-3"))
	}
}

// 跳过时的错误日志区分「订单不存在」与「订单属于别人」，后者带上订单主人。
func TestAccrueSkipLogDistinguishesMissingAndForeignOrder(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	svc := referral.NewService(referral.Deps{Pool: e.pool, Clock: e.clk, Log: slog.New(slog.NewTextHandler(&buf, nil)), Wallet: e.wallet, Users: e.users, Orders: e.orders})
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	accrue := func(l referral.LedgerRowForTest) error {
		return db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error { return svc.AccrueForTest(e.ctx, tx, e.clk.Now(), l) })
	}
	l := e.ledgerOf(e.pay(b, "ord-ghost", 6900))
	delete(e.orders.m, "ord-ghost")
	if err := accrue(l); !errors.Is(err, referral.ErrSkip) || !strings.Contains(buf.String(), "order not found") {
		t.Fatalf("ghost: %v log=%q", err, buf.String())
	}
	buf.Reset()
	l = e.ledgerOf(e.pay(b, "ord-other", 6900))
	e.orders.m["ord-other"] = fakeOrder{userID: a}
	if err := accrue(l); !errors.Is(err, referral.ErrSkip) || !strings.Contains(buf.String(), "not the order owner") ||
		!strings.Contains(buf.String(), fmt.Sprintf("owner=%d", a)) {
		t.Fatalf("other owner: %v log=%q", err, buf.String())
	}
}
