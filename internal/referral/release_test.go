package referral_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/referral"
)

// release 跑一轮 Release 且必须无错。
func (e *env) release() int {
	e.t.Helper()
	n, err := e.svc.Release(e.ctx)
	if err != nil {
		e.t.Fatalf("release: %v", err)
	}
	return n
}

// kycCheckFailed 写一条 identity 钩子失败时留下的审计（目标为该用户）。
func (e *env) kycCheckFailed(userID int64) {
	e.t.Helper()
	e.bindExec(`INSERT INTO audit_logs (actor_type, action, target_type, target_id, diff, created_at)
		VALUES ('system', 'referral.kyc_check_failed', 'user', $1, '{"kyc_record_id": 1}', $2)`, strconv.FormatInt(userID, 10), e.clk.Now())
}

func TestReleaseDueRewardsWithThresholds(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	bid := e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900)) // settle_after = T0 + 7d
	if n := e.release(); n != 0 {
		t.Fatalf("not due: %d", n)
	}
	e.clk.Advance(7*24*time.Hour - time.Second)
	if n := e.release(); n != 0 {
		t.Fatal("1s before settle_after must not release")
	}
	e.clk.Advance(time.Second)
	// 门槛：绑定 held
	e.bindExec(`UPDATE referral_bindings SET status = 'held' WHERE id = $1`, bid)
	if n := e.release(); n != 0 || e.reward("ord-1")["status"] != "pending" {
		t.Fatal("held binding must not release")
	}
	e.bindExec(`UPDATE referral_bindings SET status = 'active' WHERE id = $1`, bid)
	// 门槛：账户冻结
	e.bindExec(`UPDATE referral_accounts SET status = 'frozen' WHERE user_id = $1`, a)
	if n := e.release(); n != 0 {
		t.Fatal("frozen account must not release")
	}
	e.bindExec(`UPDATE referral_accounts SET status = 'active' WHERE user_id = $1`, a)
	// 门槛：邀请人未实名 / 封禁
	e.bindExec(`UPDATE users SET kyc_status = 'none' WHERE id = $1`, a)
	if n := e.release(); n != 0 {
		t.Fatal("unverified inviter must not release")
	}
	e.bindExec(`UPDATE users SET kyc_status = 'verified', status = 'banned' WHERE id = $1`, a)
	if n := e.release(); n != 0 {
		t.Fatal("banned inviter must not release")
	}
	e.bindExec(`UPDATE users SET status = 'active' WHERE id = $1`, a)
	if e.reward("ord-1")["status"] != "pending" || e.ledgerSum(a, "referral_rebate") != 0 {
		t.Fatalf("thresholds must leave the row untouched: %v", e.reward("ord-1"))
	}
	// 全部满足 → 发放
	if n := e.release(); n != 1 {
		t.Fatalf("release = %d", n)
	}
	r := e.reward("ord-1")
	if r["status"] != "settled" || r["settled"] != int64(690) || r["reward"] != int64(690) {
		t.Fatalf("after release = %v", r)
	}
	var amount int64
	var refType, refID, remark string
	if err := e.pool.QueryRow(e.ctx, `SELECT amount, ref_type, ref_id, remark FROM wallet_ledger WHERE user_id = $1 AND kind = 'referral_rebate'`, a).
		Scan(&amount, &refType, &refID, &remark); err != nil {
		t.Fatal(err)
	}
	if amount != 690 || refType != "referral" || refID != "ord-1" || remark != "邀请返利 139****0001 订单 ord-1" {
		t.Fatalf("ledger = %d %s %s %q", amount, refType, refID, remark)
	}
	if ev := e.events("ord-1"); ev[len(ev)-1] != "settle" {
		t.Fatalf("events = %v", ev)
	}
	// 再跑一次：没有候选
	if n := e.release(); n != 0 {
		t.Fatal("already settled must not release again")
	}
	e.invariantsOK()
}

// Q4：欠账经真实的退款扣回路径产生（余额不足 → debt_add），不用裸 UPDATE，不变量全程成立。
func TestReleaseZeroVoidsAndDebtOffset(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	// 造欠账 300：ord-s 发放 690 → 邀请人花光余额 → 退款 3000（应得 390，多出 300 扣不到）
	e.accrueOK(e.pay(b, "ord-s", 6900))
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 1 {
		t.Fatalf("settle ord-s = %d", n)
	}
	e.spend(a, 690)
	e.refundOK(e.refund(b, "ord-s-r1", 3000))
	if e.debt(a) != 300 {
		t.Fatalf("debt = %d", e.debt(a))
	}
	e.invariantsOK()
	// 全退 → 0 → 到期 void
	e.accrueOK(e.pay(b, "ord-0", 6900))
	e.refundOK(e.refund(b, "ord-0-r1", 6900))
	// ord-1 发放 690：抵 300、入账 390
	e.accrueOK(e.pay(b, "ord-1", 6900))
	// ord-2 reward 100（改 cap 模拟；与 min(cap, 剩余 × 费率) 一致）：欠账已清 → 全额入账
	e.accrueOK(e.pay(b, "ord-2", 6900))
	e.bindExec(`UPDATE referral_rewards SET reward = 100, reward_cap = 100 WHERE order_id = 'ord-2'`)
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 3 {
		t.Fatalf("release = %d", n)
	}
	if r := e.reward("ord-0"); r["status"] != "void" || r["settled"] != int64(0) {
		t.Fatalf("zero reward = %v", r)
	}
	if ev := e.events("ord-0"); ev[len(ev)-1] != "void" {
		t.Fatalf("ord-0 events = %v", ev)
	}
	// 顺序按 settle_after, order_id：ord-1 先（抵 300，入 390，欠 0），ord-2 后（欠 0 → 全额入 100）
	if r := e.reward("ord-1"); r["status"] != "settled" || r["settled"] != int64(690) {
		t.Fatalf("ord-1 = %v", r)
	}
	if e.ledgerSum(a, "referral_rebate") != 690+390+100 || e.debt(a) != 0 {
		t.Fatalf("wallet = %d debt = %d", e.ledgerSum(a, "referral_rebate"), e.debt(a))
	}
	if ev := e.events("ord-1"); len(ev) < 2 || ev[len(ev)-2] != "debt_offset" || ev[len(ev)-1] != "settle" {
		t.Fatalf("ord-1 events = %v", ev)
	}
	var offset int64
	if err := e.pool.QueryRow(e.ctx, `SELECT amount FROM referral_events WHERE order_id = 'ord-1' AND kind = 'debt_offset'`).Scan(&offset); err != nil || offset != 300 {
		t.Fatalf("debt_offset amount = %d %v", offset, err)
	}
	e.invariantsOK()

	// 欠账 > 返利：余额花光后 ord-1、ord-2 全退 → 扣不到，欠 690 + 100 = 790；ord-3 的 690 全部抵欠，钱包不入账
	e.spend(a, 390+100) // 余额：+690 −690 +390 +100
	e.refundOK(e.refund(b, "ord-1-r1", 6900))
	e.refundOK(e.refund(b, "ord-2-r1", 6900))
	if e.debt(a) != 790 {
		t.Fatalf("debt = %d", e.debt(a))
	}
	e.accrueOK(e.pay(b, "ord-3", 6900))
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 1 {
		t.Fatalf("release ord-3 = %d", n)
	}
	if r := e.reward("ord-3"); r["status"] != "settled" || r["settled"] != int64(690) || e.debt(a) != 100 || e.ledgerSum(a, "referral_rebate") != 1180 {
		t.Fatalf("debt > reward: %v debt=%d wallet=%d", r, e.debt(a), e.ledgerSum(a, "referral_rebate"))
	}
	var lid *int64
	if err := e.pool.QueryRow(e.ctx, `SELECT wallet_ledger_id FROM referral_events WHERE order_id = 'ord-3' AND kind = 'settle'`).Scan(&lid); err != nil || lid != nil {
		t.Fatalf("fully offset settle must have no wallet_ledger_id: %v %v", lid, err)
	}
	e.invariantsOK()
}

// 发放流水重放保护：行被（人工 / 异常）改回 pending 再发 → 已有 referral/<order> 流水，不再入账，仍置 settled 并引用原流水。
func TestReleaseReplayDoesNotDoubleCredit(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 1 {
		t.Fatalf("release = %d", n)
	}
	var ledgerID int64
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM wallet_ledger WHERE kind = 'referral_rebate' AND ref_id = 'ord-1'`).Scan(&ledgerID); err != nil {
		t.Fatal(err)
	}
	e.bindExec(`UPDATE referral_rewards SET status = 'pending', settled_amount = 0, settled_at = NULL WHERE order_id = 'ord-1'`)
	if n := e.release(); n != 1 {
		t.Fatalf("replay release = %d", n)
	}
	if e.ledgerSum(a, "referral_rebate") != 690 || e.reward("ord-1")["status"] != "settled" || e.reward("ord-1")["settled"] != int64(690) {
		t.Fatalf("replay = wallet %d reward %v", e.ledgerSum(a, "referral_rebate"), e.reward("ord-1"))
	}
	rows, err := e.pool.Query(e.ctx, `SELECT wallet_ledger_id FROM referral_events WHERE order_id = 'ord-1' AND kind = 'settle' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) != 2 || ids[0] != ledgerID || ids[1] != ledgerID {
		t.Fatalf("settle events ledger ids = %v %v, want both %d", ids, err, ledgerID)
	}
	e.invariantsOK()
}

// 重放保护先于抵欠账：有欠账时把已发放的行改回 pending 再发 → 不再抵欠、不再入账（含全额抵欠、没有发放流水的行）。
func TestReleaseReplayWithDebtSkipsOffset(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	// 欠 300：ord-s 发放后花光余额再退 3000
	e.accrueOK(e.pay(b, "ord-s", 6900))
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 1 {
		t.Fatalf("settle ord-s = %d", n)
	}
	e.spend(a, 690)
	e.refundOK(e.refund(b, "ord-s-r1", 3000))
	// ord-1 返利 100（改 cap）：全额抵欠（欠 200），没有发放流水；ord-2 690：抵 200、入 490
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.bindExec(`UPDATE referral_rewards SET reward = 100, reward_cap = 100 WHERE order_id = 'ord-1'`)
	e.accrueOK(e.pay(b, "ord-2", 6900))
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 2 || e.debt(a) != 0 || e.ledgerSum(a, "referral_rebate") != 690+490 {
		t.Fatalf("n=%d debt=%d wallet=%d", n, e.debt(a), e.ledgerSum(a, "referral_rebate"))
	}
	e.invariantsOK()
	// 再造欠账 300，然后把 ord-1、ord-2 改回 pending 重放
	e.accrueOK(e.pay(b, "ord-3", 6900))
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 1 {
		t.Fatalf("settle ord-3 = %d", n)
	}
	e.spend(a, 490+690) // 余额：+690 −690 +490 +690
	e.refundOK(e.refund(b, "ord-3-r1", 3000))
	if e.debt(a) != 300 {
		t.Fatalf("debt = %d", e.debt(a))
	}
	e.bindExec(`UPDATE referral_rewards SET status = 'pending', settled_amount = 0, settled_at = NULL WHERE order_id IN ('ord-1', 'ord-2')`)
	if n := e.release(); n != 2 {
		t.Fatalf("replay = %d", n)
	}
	if e.debt(a) != 300 || e.ledgerSum(a, "referral_rebate") != 690+490+690 {
		t.Fatalf("replay must not offset or credit: debt=%d wallet=%d", e.debt(a), e.ledgerSum(a, "referral_rebate"))
	}
	for _, o := range []string{"ord-1", "ord-2"} {
		if r := e.reward(o); r["status"] != "settled" {
			t.Fatalf("%s = %v", o, r)
		}
		var offsets int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM referral_events WHERE order_id = $1 AND kind = 'debt_offset'`, o).Scan(&offsets); err != nil || offsets != 1 {
			t.Fatalf("%s debt_offset events = %d %v", o, offsets, err)
		}
	}
	e.invariantsOK()
}

// 每页 ReleaseBatch（200）行，一轮内按 (settle_after, order_id) 游标翻页；限一页时退化为每轮 200 行。
func TestReleaseBatchLimit(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	for i := 0; i < 205; i++ {
		e.accrueOK(e.pay(b, fmt.Sprintf("ord-%03d", i), 100))
	}
	e.clk.Advance(7 * 24 * time.Hour)
	restore := referral.SetReleaseMaxPages(1)
	if n := e.release(); n != 200 {
		t.Fatalf("one page = %d", n)
	}
	restore()
	for i := 205; i < 410; i++ {
		e.accrueOK(e.pay(b, fmt.Sprintf("ord-%03d", i), 100))
	}
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 210 { // 5 + 205，两页
		t.Fatalf("paged round = %d", n)
	}
	if e.ledgerSum(a, "referral_rebate") != 410*10 {
		t.Fatalf("wallet = %d", e.ledgerSum(a, "referral_rebate"))
	}
	e.invariantsOK()
}

// 饿死（预筛）：排在前面的 201 行邀请人未实名，SQL 预筛直接排除；即便每轮只一页，后面可发的行本轮就发。
func TestReleasePrefilterSkipsUnpayableRows(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	x := e.user("13700000001", true)
	y := e.user("13700000002", true)
	e.bind(x, y)
	e.bind(a, b)
	for i := 0; i < 201; i++ {
		e.accrueOK(e.pay(y, fmt.Sprintf("ord-x%03d", i), 100))
	}
	e.bindExec(`UPDATE users SET kyc_status = 'none' WHERE id = $1`, x)
	e.clk.Advance(time.Second)
	e.accrueOK(e.pay(b, "ord-a", 6900)) // settle_after 晚 1s，排在 201 行之后
	e.clk.Advance(7 * 24 * time.Hour)
	defer referral.SetReleaseMaxPages(1)()
	if n := e.release(); n != 1 || e.reward("ord-a")["status"] != "settled" {
		t.Fatalf("payable row starved: n=%d %v", n, e.reward("ord-a"))
	}
	if e.reward("ord-x000")["status"] != "pending" {
		t.Fatal("unverified inviter must stay pending")
	}
}

// 饿死（翻页）：预筛放行但锁内跳过的行（kyc_check_failed 把绑定转 held，同绑定其余 200 行随即跳过）占满第一页，
// 游标翻到下一页，可发的行本轮就发。
func TestReleasePagesPastSkippedRows(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	x := e.user("13700000001", true)
	y := e.user("13700000002", true)
	bidY := e.bind(x, y)
	e.bind(a, b)
	for i := 0; i < 201; i++ {
		e.accrueOK(e.pay(y, fmt.Sprintf("ord-x%03d", i), 100))
	}
	e.clk.Advance(time.Second)
	e.kycCheckFailed(y)
	e.accrueOK(e.pay(b, "ord-a", 6900))
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 1 || e.reward("ord-a")["status"] != "settled" {
		t.Fatalf("payable row starved: n=%d %v", n, e.reward("ord-a"))
	}
	if status, _, _ := e.bindingRow(y); status != "held" || e.autoHoldAudits(bidY) != 1 {
		t.Fatalf("binding y = %s audits = %d", status, e.autoHoldAudits(bidY))
	}
}

// 预筛只是优化：锁内复核仍是权威。绕过预筛直接跑一行，门槛不满足时不动。
func TestReleaseOneRechecksGatesUnderLock(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	bid := e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.clk.Advance(7 * 24 * time.Hour)
	one := func() bool {
		t.Helper()
		var done bool
		if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
			var err error
			done, err = e.svc.ReleaseOneForTest(e.ctx, tx, e.clk.Now(), "ord-1", a, b)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return done
	}
	for _, c := range []struct{ set, reset string }{
		{`UPDATE referral_bindings SET status = 'held' WHERE id = $1`, `UPDATE referral_bindings SET status = 'active' WHERE id = $1`},
		{`UPDATE referral_accounts SET status = 'frozen' WHERE user_id = (SELECT inviter_id FROM referral_bindings WHERE id = $1)`, `UPDATE referral_accounts SET status = 'active' WHERE user_id = (SELECT inviter_id FROM referral_bindings WHERE id = $1)`},
		{`UPDATE users SET kyc_status = 'rejected' WHERE id = (SELECT inviter_id FROM referral_bindings WHERE id = $1)`, `UPDATE users SET kyc_status = 'verified' WHERE id = (SELECT inviter_id FROM referral_bindings WHERE id = $1)`},
		{`UPDATE users SET status = 'banned' WHERE id = (SELECT inviter_id FROM referral_bindings WHERE id = $1)`, `UPDATE users SET status = 'active' WHERE id = (SELECT inviter_id FROM referral_bindings WHERE id = $1)`},
	} {
		e.bindExec(c.set, bid)
		if one() || e.reward("ord-1")["status"] != "pending" {
			t.Fatalf("%s: must skip under lock", c.set)
		}
		e.bindExec(c.reset, bid)
	}
	if !one() || e.reward("ord-1")["status"] != "settled" {
		t.Fatalf("all gates pass: %v", e.reward("ord-1"))
	}
}

// 指纹按邀请人在一轮内缓存：同一邀请人的多行只读一次。
type countingUsers struct {
	referral.UserLookup
	fpCalls int
}

func (c *countingUsers) VerifiedFingerprints(ctx context.Context, q db.Querier, ids []int64) (map[int64][]string, error) {
	c.fpCalls++
	return c.UserLookup.VerifiedFingerprints(ctx, q, ids)
}

func TestReleaseCachesFingerprintsPerInviter(t *testing.T) {
	e := newEnv(t)
	cu := &countingUsers{UserLookup: e.users}
	svc := referral.NewService(referral.Deps{Pool: e.pool, Clock: e.clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Wallet: e.wallet, Users: cu, Orders: e.orders})
	a := e.user("13812345678", true)
	e.users.fps[a] = []string{"fp-a"}
	for i, phone := range []string{"13900000001", "13900000002", "13900000003"} {
		u := e.user(phone, true)
		e.users.fps[u] = []string{fmt.Sprintf("fp-%d", i)}
		e.bind(a, u)
		e.accrueOK(e.pay(u, fmt.Sprintf("ord-%d", i), 6900))
	}
	e.clk.Advance(7 * 24 * time.Hour)
	if n, err := svc.Release(e.ctx); err != nil || n != 3 {
		t.Fatalf("release = %d %v", n, err)
	}
	if cu.fpCalls != 1 {
		t.Fatalf("VerifiedFingerprints calls = %d, want 1 per inviter per round", cu.fpCalls)
	}
}

// 控制者裁决（纵深防御）：实名钩子可能静默失败，发放前按全部 verified 指纹复查同身份——
// 被邀请人与邀请人、或与邀请人其它未撤销被邀请人同指纹 → 按实名路径同样撤销（same_identity），待发放作废，不付款。
func TestReleaseRechecksSameIdentity(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	c := e.user("13900000002", true)
	x := e.user("13700000001", true)
	y := e.user("13700000002", true)
	bidB := e.bind(a, b)
	e.bind(a, c)
	bidY := e.bind(x, y)
	e.users.fps[a] = []string{"fp-a"}
	e.users.fps[c] = []string{"fp-c"}
	e.users.fps[b] = []string{"fp-b-ent", "fp-c"} // b 的企业实名与兄弟 c 同人
	e.users.fps[x] = []string{"fp-x"}
	e.users.fps[y] = []string{"fp-y", "fp-x"} // y 与邀请人 x 同人
	e.accrueOK(e.pay(b, "ord-b", 6900))
	e.accrueOK(e.pay(b, "ord-b2", 6900))
	e.accrueOK(e.pay(c, "ord-c", 6900))
	e.accrueOK(e.pay(y, "ord-y", 6900))
	e.clk.Advance(7 * 24 * time.Hour)
	// ord-b：与兄弟 c 撞 → 撤销 b 的绑定（ord-b、ord-b2 作废，计 1 行）；ord-b2 已无 pending → 不计；
	// ord-c：b 已撤销、不再参与比较 → 照常发放；ord-y：与邀请人 x 撞 → 撤销（计 1 行）。
	if n := e.release(); n != 3 {
		t.Fatalf("release = %d", n)
	}
	for _, o := range []string{"ord-b", "ord-b2", "ord-y"} {
		if r := e.reward(o); r["status"] != "void" || r["settled"] != int64(0) {
			t.Fatalf("%s = %v", o, r)
		}
	}
	if r := e.reward("ord-c"); r["status"] != "settled" || r["settled"] != int64(690) {
		t.Fatalf("ord-c = %v", r)
	}
	if e.ledgerSum(a, "referral_rebate") != 690 || e.ledgerSum(x, "referral_rebate") != 0 {
		t.Fatalf("wallet a = %d x = %d", e.ledgerSum(a, "referral_rebate"), e.ledgerSum(x, "referral_rebate"))
	}
	for invitee, bid := range map[int64]int64{b: bidB, y: bidY} {
		status, hold, flags := e.bindingRow(invitee)
		if status != "revoked" || hold != "same_identity" || !slices.Contains(flags, "same_identity") {
			t.Fatalf("binding %d = %s %s %v", bid, status, hold, flags)
		}
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = 'referral.auto_revoke' AND actor_type = 'system' AND target_type = 'referral_binding' AND target_id = $1`,
			strconv.FormatInt(bid, 10)).Scan(&n); err != nil || n != 1 {
			t.Fatalf("auto_revoke audit for binding %d = %d %v", bid, n, err)
		}
	}
	if ev := e.events("ord-b"); ev[len(ev)-1] != "void" {
		t.Fatalf("ord-b events = %v", ev)
	}
	e.invariantsOK()
}

// approveForTest 模拟后台「通过」（Task 11 Approve：held → active，写 reviewed_at）。
func (e *env) approveForTest(bindingID int64) {
	e.t.Helper()
	e.bindExec(`UPDATE referral_bindings SET status = 'active', hold_reason = '', reviewed_at = $2, updated_at = $2 WHERE id = $1 AND status = 'held'`, bindingID, e.clk.Now())
}

func (e *env) autoHoldAudits(bindingID int64) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = 'referral.auto_hold' AND actor_type = 'system' AND target_type = 'referral_binding' AND target_id = $1`,
		strconv.FormatInt(bindingID, 10)).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// 控制者裁决：被邀请人或邀请人有未解决的 referral.kyc_check_failed 审计（创建晚于绑定的 reviewed_at，NULL 视为从未复核）→
// 绑定转 held（hold_reason=kyc_check_failed，审计 referral.auto_hold），返利保持 pending；后台通过（写 reviewed_at）后下一轮照常发放，
// 同一条审计不再触发。
func TestReleaseHoldsOnUnresolvedKYCCheckFailure(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	c := e.user("13900000002", true)
	other := e.user("13900000009", true)
	e.kycCheckFailed(other) // 无关用户的失败不影响
	e.kycCheckFailed(c)     // 绑定之前的失败与该绑定无关（created_at ≤ bound_at）
	e.clk.Advance(time.Second)
	bid := e.bind(a, b)
	bidC := e.bind(a, c)
	e.accrueOK(e.pay(c, "ord-c", 6900))
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.clk.Advance(time.Second)
	e.kycCheckFailed(b)
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 1 || e.reward("ord-1")["status"] != "pending" || e.ledgerSum(a, "referral_rebate") != 690 {
		t.Fatalf("invitee kyc_check_failed must hold ord-1 only: n=%d ord-1=%v", n, e.reward("ord-1"))
	}
	if r := e.reward("ord-c"); r["status"] != "settled" || e.autoHoldAudits(bidC) != 0 {
		t.Fatalf("pre-binding failure must not hold: %v audits=%d", r, e.autoHoldAudits(bidC))
	}
	if status, hold, _ := e.bindingRow(b); status != "held" || hold != "kyc_check_failed" || e.autoHoldAudits(bid) != 1 {
		t.Fatalf("binding = %s %s audits = %d", status, hold, e.autoHoldAudits(bid))
	}
	if ev := e.events("ord-1"); len(ev) != 1 {
		t.Fatalf("hold must not write reward events: %v", ev)
	}
	// 搁置中：再跑不发放、不重复审计
	if n := e.release(); n != 0 || e.autoHoldAudits(bid) != 1 {
		t.Fatalf("held: n = %d audits = %d", n, e.autoHoldAudits(bid))
	}
	e.approveForTest(bid)
	if n := e.release(); n != 1 || e.reward("ord-1")["status"] != "settled" {
		t.Fatalf("approved binding must release: %v", e.reward("ord-1"))
	}
	if status, _, _ := e.bindingRow(b); status != "active" || e.autoHoldAudits(bid) != 1 {
		t.Fatalf("resolved audit must not re-hold: %s audits = %d", status, e.autoHoldAudits(bid))
	}
	// 邀请人一侧：新的失败审计晚于 reviewed_at → 再次搁置
	e.accrueOK(e.pay(b, "ord-2", 6900))
	e.clk.Advance(time.Second)
	e.kycCheckFailed(a)
	e.clk.Advance(7 * 24 * time.Hour)
	if n := e.release(); n != 0 || e.reward("ord-2")["status"] != "pending" {
		t.Fatalf("inviter kyc_check_failed must hold: %v", e.reward("ord-2"))
	}
	if status, hold, _ := e.bindingRow(b); status != "held" || hold != "kyc_check_failed" || e.autoHoldAudits(bid) != 2 {
		t.Fatalf("binding = %s %s audits = %d", status, hold, e.autoHoldAudits(bid))
	}
	e.approveForTest(bid)
	if n := e.release(); n != 1 || e.reward("ord-2")["status"] != "settled" {
		t.Fatalf("approved binding must release: %v", e.reward("ord-2"))
	}
	e.invariantsOK()
}

// 实名事务先持 users(本人) FOR UPDATE 再锁 referral_accounts；发放必须先对 users(邀请人, 被邀请人) 取 KEY SHARE 再锁账户，
// 否则写 wallet_ledger 的外键检查会形成 accounts → users 的反向等待而死锁。
func TestReleaseLocksUsersBeforeAccount(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.clk.Advance(7 * 24 * time.Hour)

	kyc, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = kyc.Rollback(e.ctx) }()
	if _, err := kyc.Exec(e.ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, a); err != nil {
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
	// NOWAIT：Release 此刻不能持有账户锁（它应停在 users 上）。不能只看最终成功——死锁时 Release 是先等待的一方、
	// 会被选为牺牲者，withRetry 重试后照样成功，掩盖反向加锁。
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	if _, err := kyc.Exec(ctx, `SELECT 1 FROM referral_accounts WHERE user_id = $1 FOR UPDATE NOWAIT`, a); err != nil {
		t.Fatalf("kyc side lock account: %v", err)
	}
	if err := kyc.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || r.n != 1 {
		t.Fatalf("release = %d %v", r.n, r.err)
	}
	if e.ledgerSum(a, "referral_rebate") != 690 {
		t.Fatalf("wallet = %d", e.ledgerSum(a, "referral_rebate"))
	}
}

// 两轮 Release 并发：在邀请人账户锁上排队，后者锁后 SKIP LOCKED 复核看到已 settled → 不重复入账。
func TestReleaseConcurrentRoundsPayOnce(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.clk.Advance(7 * 24 * time.Hour)
	hold, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(e.ctx) }()
	if _, err := hold.Exec(e.ctx, `SELECT 1 FROM referral_accounts WHERE user_id = $1 FOR UPDATE`, a); err != nil {
		t.Fatal(err)
	}
	type res struct {
		n   int
		err error
	}
	out := make(chan res, 2)
	for range 2 {
		go func() {
			n, err := e.svc.Release(e.ctx)
			out <- res{n, err}
		}()
	}
	e.waitLockWaiters(2)
	if err := hold.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	total := 0
	for range 2 {
		r := <-out
		if r.err != nil {
			t.Fatalf("concurrent release: %v", r.err)
		}
		total += r.n
	}
	if total != 1 || e.ledgerSum(a, "referral_rebate") != 690 || len(e.events("ord-1")) != 2 {
		t.Fatalf("total = %d wallet = %d events = %v", total, e.ledgerSum(a, "referral_rebate"), e.events("ord-1"))
	}
	e.invariantsOK()
}
