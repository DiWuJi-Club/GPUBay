package referral_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/referral"
)

func (e *env) watermark() time.Time {
	e.t.Helper()
	var w time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT watermark FROM referral_state WHERE id = 1`).Scan(&w); err != nil {
		e.t.Fatal(err)
	}
	return w
}

// ledgerAt 直接插一条 order 流水（任意 created_at，不动余额）；返回流水 id。
func (e *env) ledgerAt(userID int64, kind string, amount int64, refID string, at time.Time) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		VALUES ($1, $2, 0, $3, 'order', $4, '', $5) RETURNING id`, userID, amount, kind, refID, at).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) eventDetail(order, kind string) string {
	e.t.Helper()
	var d string
	if err := e.pool.QueryRow(e.ctx, `SELECT detail FROM referral_events WHERE order_id = $1 AND kind = $2 ORDER BY id LIMIT 1`, order, kind).Scan(&d); err != nil {
		e.t.Fatalf("event %s/%s: %v", order, kind, err)
	}
	return d
}

func TestDiscoverProcessesOnlyBoundUsersAndAdvancesWatermark(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	x := e.user("13900000009", true) // 未绑定
	e.bind(a, b)
	e.pay(b, "ord-b", 6900)
	e.pay(x, "ord-x", 6900)
	n, err := e.svc.Discover(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("discover = %d %v", n, err)
	}
	if e.reward("ord-b")["reward"] != int64(690) {
		t.Fatal("bound user's pay must accrue")
	}
	if len(e.events("ord-x")) != 0 {
		t.Fatal("unbound user's pay must be ignored")
	}
	if !e.watermark().Equal(e.clk.Now().Add(-2 * time.Minute)) {
		t.Fatalf("watermark = %v", e.watermark())
	}
	// 第二轮：无新行，水位继续推进；1 小时回看不重复处理
	e.clk.Advance(10 * time.Minute)
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 0 {
		t.Fatalf("second = %d %v", n, err)
	}
	if len(e.events("ord-b")) != 1 {
		t.Fatal("lookback must not double-process")
	}
	if !e.watermark().Equal(e.clk.Now().Add(-2 * time.Minute)) {
		t.Fatalf("watermark after second round = %v", e.watermark())
	}
	// 水位之前 1 小时以外的流水不再被看见（除非 Rescan）；在水位 − 1h 之内的「晚提交」流水会被看见
	e.ledgerAt(b, "order_pay", -100, "ord-old", e.clk.Now().Add(-3*time.Hour))
	e.orders.m["ord-old"] = fakeOrder{userID: b}
	e.ledgerAt(b, "order_pay", -100, "ord-late", e.clk.Now().Add(-30*time.Minute))
	e.orders.m["ord-late"] = fakeOrder{userID: b}
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 1 {
		t.Fatalf("late commit = %d %v", n, err)
	}
	if len(e.events("ord-old")) != 0 || len(e.events("ord-late")) != 1 {
		t.Fatalf("old=%v late=%v", e.events("ord-old"), e.events("ord-late"))
	}
	// Rescan 拨回 1 天：ord-old 被补上；已处理的不重复
	if _, err := e.svc.Rescan(e.ctx, audit.Actor{AdminID: 1}, 1); err != nil {
		t.Fatal(err)
	}
	if len(e.events("ord-old")) != 1 || len(e.events("ord-late")) != 1 || len(e.events("ord-b")) != 1 {
		t.Fatalf("after rescan old=%v late=%v b=%v", e.events("ord-old"), e.events("ord-late"), e.events("ord-b"))
	}
	var audits int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = 'referral.rescan' AND (diff->>'days')::int = 1`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("rescan audit rows = %d %v", audits, err)
	}
	for _, d := range []int{0, 31} {
		if _, err := e.svc.Rescan(e.ctx, audit.Actor{AdminID: 1}, d); err == nil {
			t.Fatalf("days %d must be rejected", d)
		}
	}
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverBacklogKeepsWatermark(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	for i := 0; i < referral.ScanBatch+3; i++ {
		e.pay(b, fmt.Sprintf("ord-%04d", i), 100)
	}
	restore := referral.SetDiscoverMaxPages(1) // 一轮只翻一页：模拟超过一轮页数上限的积压
	defer restore()
	w0 := e.watermark()
	n, err := e.svc.Discover(e.ctx)
	if err != nil || n != referral.ScanBatch || !e.watermark().Equal(w0) {
		t.Fatalf("full batch: n=%d err=%v watermark moved=%v", n, err, !e.watermark().Equal(w0))
	}
	n, err = e.svc.Discover(e.ctx)
	if err != nil || n != 3 || e.watermark().Equal(w0) {
		t.Fatalf("drain: n=%d err=%v watermark moved=%v", n, err, !e.watermark().Equal(w0))
	}
}

// Review Focus 1：同一批里退款流水排在它的扣款之后处理，即使 id 更小（人为造 id 顺序）。
func TestDiscoverProcessesPayBeforeRefundInSameBatch(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	// 先插退款（id 小），再插扣款（id 大）
	e.ledgerAt(b, "refund", 1000, "ord-1-r1", e.clk.Now())
	e.pay(b, "ord-1", 6900)
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 2 {
		t.Fatalf("n = %d %v", n, err)
	}
	if r := e.reward("ord-1"); r["reward"] != int64(590) || r["refunded"] != int64(1000) {
		t.Fatalf("reward = %v", r)
	}
	// 订单查不到的扣款：退款被延后；补上订单后下一轮两者都处理
	e.ledgerAt(b, "order_pay", -6900, "ord-2", e.clk.Now())
	e.ledgerAt(b, "refund", 500, "ord-2-r1", e.clk.Now())
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 0 || len(e.events("ord-2")) != 0 {
		t.Fatalf("ghost order: n=%d err=%v events=%v", n, err, e.events("ord-2"))
	}
	e.orders.m["ord-2"] = fakeOrder{userID: b}
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 2 || e.reward("ord-2")["reward"] != int64(640) {
		t.Fatalf("after order appears: n=%d err=%v reward=%v", n, err, e.reward("ord-2"))
	}
}

// 裁定 Q5：被跳过的行把水位钉在它的 created_at，1 小时回看过去之后它仍在扫描窗口里；
// 延后的退款（其扣款的订单暂时查不到）在 > 1 小时后仍会被处理，不会漏扣回。
func TestDiscoverKeepsWatermarkBehindSkippedRow(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	payAt := e.clk.Now()
	e.ledgerAt(b, "order_pay", -6900, "ord-1", payAt)
	e.clk.Advance(5 * time.Minute)
	refundAt := e.clk.Now()
	e.ledgerAt(b, "refund", 1000, "ord-1-r1", refundAt)
	e.clk.Advance(5 * time.Minute)
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 0 {
		t.Fatalf("first = %d %v", n, err)
	}
	if !e.watermark().Equal(payAt) {
		t.Fatalf("watermark = %v, want pinned at oldest skipped row %v", e.watermark(), payAt)
	}
	for i := 0; i < 2; i++ {
		e.clk.Advance(2 * time.Hour)
		if n, err := e.svc.Discover(e.ctx); err != nil || n != 0 {
			t.Fatalf("round %d = %d %v", i, n, err)
		}
		if !e.watermark().Equal(payAt) {
			t.Fatalf("round %d watermark = %v, want %v", i, e.watermark(), payAt)
		}
	}
	e.orders.m["ord-1"] = fakeOrder{userID: b}
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 2 {
		t.Fatalf("after order appears = %d %v", n, err)
	}
	if r := e.reward("ord-1"); r["reward"] != int64(590) || r["refunded"] != int64(1000) {
		t.Fatalf("reward = %v", r)
	}
	if !e.watermark().Equal(e.clk.Now().Add(-referral.ScanLag)) {
		t.Fatalf("watermark after drain = %v", e.watermark())
	}
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
}

// 裁定 Q5：处理失败（非跳过）的行同样钉住水位，Discover 返回错误；故障恢复后补上。
func TestDiscoverKeepsWatermarkBehindFailedRow(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	payAt := e.clk.Now()
	e.pay(b, "ord-1", 6900)
	e.orders.fail["ord-1"] = errors.New("orders db down")
	e.clk.Advance(10 * time.Minute)
	e.pay(b, "ord-2", 6900)
	n, err := e.svc.Discover(e.ctx)
	if err == nil || n != 1 {
		t.Fatalf("discover = %d %v, want 1 processed and an error", n, err)
	}
	if !e.watermark().Equal(payAt) {
		t.Fatalf("watermark = %v, want pinned at failed row %v", e.watermark(), payAt)
	}
	e.clk.Advance(3 * time.Hour)
	delete(e.orders.fail, "ord-1")
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 1 || e.reward("ord-1")["reward"] != int64(690) {
		t.Fatalf("after recovery = %d %v", n, err)
	}
	if !e.watermark().Equal(e.clk.Now().Add(-referral.ScanLag)) {
		t.Fatalf("watermark after recovery = %v", e.watermark())
	}
}

// 永久跳过：订单不存在 / 属于别人的扣款、找不到任何被绑定扣款的退款，在 SkipGrace 内按暂时跳过（钉水位、下轮重试），
// 过了 SkipGrace 写占位事件（扣款 void、退款 refund(0)），不再钉住水位；它们的退款随之走「无 reward」占位。
func TestDiscoverPermanentSkipWritesMarkerAfterGrace(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	t0 := e.clk.Now()
	e.ledgerAt(b, "order_pay", -6900, "ord-ghost", t0)   // 订单不存在
	e.ledgerAt(b, "refund", 1000, "ord-ghost-r1", t0)    // 其退款
	e.ledgerAt(b, "order_pay", -6900, "ord-foreign", t0) // 订单属于别人
	e.orders.m["ord-foreign"] = fakeOrder{userID: a}
	e.ledgerAt(b, "refund", 500, "ord-nopay", t0) // 没有任何扣款的退款
	e.clk.Advance(5 * time.Minute)                // 过了 ScanLag：水位由被跳过的行决定
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 0 {
		t.Fatalf("within grace = %d %v", n, err)
	}
	if !e.watermark().Equal(t0) {
		t.Fatalf("within grace watermark = %v, want %v", e.watermark(), t0)
	}
	e.clk.Advance(referral.SkipGrace - 6*time.Minute) // 仍差 1 分钟
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 0 || !e.watermark().Equal(t0) {
		t.Fatalf("just before grace = %d %v watermark=%v", n, err, e.watermark())
	}
	e.clk.Advance(time.Minute)
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 4 {
		t.Fatalf("after grace = %d %v", n, err)
	}
	if ev := e.events("ord-ghost"); len(ev) != 2 || ev[0] != "void" || ev[1] != "refund" || e.eventDetail("ord-ghost", "void") != "order not found" {
		t.Fatalf("ghost events = %v", ev)
	}
	if ev := e.events("ord-foreign"); len(ev) != 1 || ev[0] != "void" || e.eventDetail("ord-foreign", "void") != "order owner mismatch" {
		t.Fatalf("foreign events = %v", ev)
	}
	if ev := e.events("ord-nopay"); len(ev) != 1 || ev[0] != "refund" || e.eventDetail("ord-nopay", "refund") != "no order_pay" {
		t.Fatalf("no-pay refund events = %v", ev)
	}
	if e.rewardCount("ord-ghost")+e.rewardCount("ord-foreign") != 0 {
		t.Fatal("markers must not create rewards")
	}
	if !e.watermark().Equal(e.clk.Now().Add(-referral.ScanLag)) {
		t.Fatalf("watermark after markers = %v", e.watermark())
	}
	// 订单事后出现也不会再计提（已有占位）
	e.orders.m["ord-ghost"] = fakeOrder{userID: b}
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 0 {
		t.Fatalf("after markers = %d %v", n, err)
	}
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
}

// 退款的扣款还未处理（被绑定用户的 order_pay 仍在等）：过了 SkipGrace 也不写占位——扣款本身会钉住水位，
// 等它计提（或写占位）后退款再处理，避免漏扣回。
func TestRefundWaitsForPendingPayEvenAfterGrace(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	e.svc = referral.NewService(referral.Deps{Pool: e.pool, Clock: e.clk, Log: slog.New(slog.NewTextHandler(&buf, nil)), Wallet: e.wallet, Users: e.users, Orders: e.orders})
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	t0 := e.clk.Now()
	e.pay(b, "ord-1", 6900)
	e.orders.fail["ord-1"] = errors.New("orders db down")
	e.ledgerAt(b, "refund", 1000, "ord-1-r1", t0)
	e.clk.Advance(referral.SkipGrace + time.Hour)
	if n, err := e.svc.Discover(e.ctx); err == nil || n != 0 || len(e.events("ord-1")) != 0 {
		t.Fatalf("pay failing = %d %v events=%v", n, err, e.events("ord-1"))
	}
	if !strings.Contains(buf.String(), `level=ERROR msg="referral: refund past SkipGrace still waiting`) {
		t.Fatalf("past-grace refund waiting on its pay must log at Error: %q", buf.String())
	}
	delete(e.orders.fail, "ord-1")
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 2 {
		t.Fatalf("after recovery = %d %v", n, err)
	}
	if r := e.reward("ord-1"); r["reward"] != int64(590) || r["refunded"] != int64(1000) {
		t.Fatalf("reward = %v", r)
	}
}

func TestSettleRunsDiscoverThenRelease(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	if _, err := e.pool.Exec(e.ctx, `UPDATE referral_rate_versions SET hold_days = 0`); err != nil {
		t.Fatal(err)
	}
	e.pay(b, "ord-1", 6900)
	if err := e.svc.Settle(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.reward("ord-1")["status"] != "settled" || e.ledgerSum(a, "referral_rebate") != 690 {
		t.Fatal("hold_days=0 must accrue and release in the same Settle")
	}
}

func TestProcessRowConcurrentDuplicateIsBenign(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	id := e.pay(b, "ord-1", 6900)
	l := referral.LedgerRowOf(id, b, "order_pay", -6900, "ord-1", e.clk.Now())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.svc.ProcessRowForTest(e.ctx, l); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(e.events("ord-1")) != 1 {
		t.Fatalf("events = %v", e.events("ord-1"))
	}
}

// 裁定 Q3 兜底：撞 referral_rewards_pkey（23505，事务已失败）时回滚并视为已处理，不返回错误。
func TestProcessRowRewardPkeyConflictIsBenign(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	id := e.pay(b, "ord-1", 6900)
	l := referral.LedgerRowOf(id, b, "order_pay", -6900, "ord-1", e.clk.Now())
	if err := e.svc.ProcessRowForTest(e.ctx, l); err != nil {
		t.Fatal(err)
	}
	// 模拟「另一事务已写 reward 但本流水的事件不可见」：删掉事件，再处理同一行 → INSERT reward 撞主键
	if _, err := e.pool.Exec(e.ctx, `DELETE FROM referral_events WHERE source_ledger_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ProcessRowForTest(e.ctx, l); err != nil {
		t.Fatalf("pkey conflict must be benign, got %v", err)
	}
	if e.rewardCount("ord-1") != 1 {
		t.Fatal("reward must not be duplicated")
	}
}

// 裁定 Q10：Rescan 显式传入拨回后的水位；早于拨回读到旧水位的清扫轮结束时不会把水位改回去（按读到的水位比较后才更新），
// 一轮处理不完的积压之后的 Discover 继续补上。
func TestRescanNotUndoneByStaleDiscover(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	if _, err := e.svc.Discover(e.ctx); err != nil {
		t.Fatal(err)
	}
	stale := e.watermark() // 一个清扫轮在 Rescan 之前读到的水位
	old := e.clk.Now().Add(-5 * time.Hour)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		SELECT $1, -100, 0, 'order_pay', 'order', 'ord-old-' || g, '', $2 FROM generate_series(1, $3) g`, b, old, referral.ScanBatch+3); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= referral.ScanBatch+3; i++ {
		e.orders.m[fmt.Sprintf("ord-old-%d", i)] = fakeOrder{userID: b}
	}
	restore := referral.SetDiscoverMaxPages(1) // 积压超过一轮的页数上限
	defer restore()
	if _, err := e.svc.Rescan(e.ctx, audit.Actor{AdminID: 1}, 1); err != nil {
		t.Fatal(err)
	}
	target := e.clk.Now().Add(-24 * time.Hour)
	if !e.watermark().Equal(target) {
		t.Fatalf("after rescan with backlog watermark = %v, want %v", e.watermark(), target)
	}
	// 旧清扫轮结束：它的窗口里没有行，若无条件 SET 会把水位推回 now − 2m，剩下的 3 行就再也看不见
	if n, err := e.svc.DiscoverFromForTest(e.ctx, stale); err != nil || n != 0 {
		t.Fatalf("stale discover = %d %v", n, err)
	}
	if !e.watermark().Equal(target) {
		t.Fatalf("stale discover moved watermark to %v", e.watermark())
	}
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 3 {
		t.Fatalf("drain after rescan = %d %v", n, err)
	}
	var cnt int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM referral_events WHERE order_id LIKE 'ord-old-%'`).Scan(&cnt); err != nil || cnt != referral.ScanBatch+3 {
		t.Fatalf("old events = %d %v", cnt, err)
	}
}

// 水位被钉在 now − StuckAfter 之前（一直失败的行）→ 告警 referral_scan_stuck 一条（去重），提示重扫。
func TestDiscoverStuckRaisesOneAlert(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.pay(b, "ord-1", 6900)
	e.orders.fail["ord-1"] = errors.New("orders db down")
	alerts := func() (n int, msg string) {
		t.Helper()
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*), COALESCE(max(message), '') FROM alerts WHERE kind = $1 AND resolved_at IS NULL`, referral.AlertScanStuck).Scan(&n, &msg); err != nil {
			t.Fatal(err)
		}
		return n, msg
	}
	e.clk.Advance(referral.StuckAfter - time.Minute)
	if _, err := e.svc.Discover(e.ctx); err == nil {
		t.Fatal("failing row must surface an error")
	}
	if n, _ := alerts(); n != 0 {
		t.Fatalf("alerts before StuckAfter = %d", n)
	}
	e.clk.Advance(2 * time.Minute)
	for i := 0; i < 3; i++ {
		if _, err := e.svc.Discover(e.ctx); err == nil {
			t.Fatal("failing row must surface an error")
		}
	}
	if n, msg := alerts(); n != 1 || !strings.Contains(msg, "重扫") {
		t.Fatalf("stuck alerts = %d %q, want exactly one mentioning 重扫", n, msg)
	}
}

// 游标翻页：本轮前面 ScanBatch+ 行都被跳过（订单暂查不到），后面的新流水仍在同一轮被处理；水位钉在最早的跳过行。
func TestDiscoverPagesPastStuckRows(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	t0 := e.clk.Now()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		SELECT $1, -100, 0, 'order_pay', 'order', 'ord-ghost-' || g, '', $2 FROM generate_series(1, $3) g`, b, t0, referral.ScanBatch+3); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(10 * time.Minute)
	e.pay(b, "ord-new", 6900)
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 1 || e.reward("ord-new")["reward"] != int64(690) {
		t.Fatalf("discover = %d %v; new row must not starve behind skipped rows", n, err)
	}
	if !e.watermark().Equal(t0) {
		t.Fatalf("watermark = %v, want pinned at %v", e.watermark(), t0)
	}
	// 页数上限：翻满仍有下一页 → 水位不动（下轮继续）
	restore := referral.SetDiscoverMaxPages(1)
	defer restore()
	if _, err := e.pool.Exec(e.ctx, `UPDATE referral_state SET watermark = '2000-01-01' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 0 {
		t.Fatalf("single page = %d %v", n, err)
	}
	if w := e.watermark(); w.Year() != 2000 {
		t.Fatalf("full last page must keep watermark, got %v", w)
	}
}

// Settle 给发现与发放各自的超时：发现卡住（订单查询挂起）时发放照常进行，水位不推进。
func TestSettleReleaseRunsWhenDiscoverHangs(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	if _, err := e.pool.Exec(e.ctx, `UPDATE referral_rate_versions SET hold_days = 0`); err != nil {
		t.Fatal(err)
	}
	e.pay(b, "ord-1", 6900)
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 1 {
		t.Fatalf("discover = %d %v", n, err)
	}
	w0 := e.watermark()
	e.clk.Advance(10 * time.Minute)
	e.pay(b, "ord-2", 6900)
	e.orders.hang["ord-2"] = true
	restore := referral.SetSettleTimeouts(200*time.Millisecond, 10*time.Second)
	defer restore()
	ctx, cancel := context.WithTimeout(e.ctx, 3*time.Second) // 清扫器给整个附加任务的总超时
	defer cancel()
	err := e.svc.Settle(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("settle = %v, want discover deadline", err)
	}
	if e.reward("ord-1")["status"] != "settled" || e.ledgerSum(a, "referral_rebate") != 690 {
		t.Fatalf("release must run despite a hung discover: %v", e.reward("ord-1"))
	}
	if !e.watermark().Equal(w0) {
		t.Fatalf("timed-out discover moved watermark %v → %v", w0, e.watermark())
	}
}

// 日志：宽限期内 Warn、过了宽限期 Error；失败行逐行记一次日志，Discover 返回的错误只是汇总（不重复每行错误）。
func TestDiscoverSkipLogLevelsAndErrorSummary(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	e.svc = referral.NewService(referral.Deps{Pool: e.pool, Clock: e.clk, Log: slog.New(slog.NewTextHandler(&buf, nil)), Wallet: e.wallet, Users: e.users, Orders: e.orders})
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.ledgerAt(b, "order_pay", -6900, "ord-ghost", e.clk.Now())
	if _, err := e.svc.Discover(e.ctx); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "order not found") || strings.Contains(out, "level=ERROR") {
		t.Fatalf("within grace log = %q, want WARN only", out)
	}
	buf.Reset()
	e.clk.Advance(referral.SkipGrace)
	if n, err := e.svc.Discover(e.ctx); err != nil || n != 1 {
		t.Fatalf("after grace = %d %v", n, err)
	}
	if out := buf.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "void placeholder") {
		t.Fatalf("after grace log = %q, want ERROR", out)
	}
	buf.Reset()
	e.pay(b, "ord-fail", 6900)
	e.orders.fail["ord-fail"] = errors.New("orders db down")
	_, err := e.svc.Discover(e.ctx)
	if err == nil || strings.Contains(err.Error(), "orders db down") || !strings.Contains(err.Error(), "1 ledger rows failed") {
		t.Fatalf("discover error = %v, want a summary without the per-row error", err)
	}
	if c := strings.Count(buf.String(), "orders db down"); c != 1 {
		t.Fatalf("per-row error logged %d times: %q", c, buf.String())
	}
}
