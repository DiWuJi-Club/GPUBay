package llm_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/tz"
	"spark-cloud/internal/wallet"
)

// lockedBuf 并发安全的日志缓冲（断言有 / 无 Error 日志）。
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// withLogBuf 把 e.svc 换成日志写入缓冲的新 Service。
func (e *env) withLogBuf() *lockedBuf {
	buf := &lockedBuf{}
	e.svc = e.newService(func(d *llm.Deps) { d.Log = slog.New(slog.NewTextHandler(buf, nil)) })
	return buf
}

// settleUser 一个实名用户 + 一把 Key，余额 fund 分（0 = 不充值）。返回用户 ID、Key ID、Key 明文。
func (e *env) settleUser(fund int64) (int64, int64, string) {
	e.t.Helper()
	u := e.verifiedUser()
	plain, k := e.createKey(u)
	if fund > 0 {
		e.fund(u, fund)
	}
	return u, k.ID, plain
}

// usageAt 记一条 ok 用量（created_at = at），费用直接给定（微分）。
func (e *env) usageAt(userID, keyID, modelID int64, at time.Time, p, c int, costMicro int64) {
	e.t.Helper()
	e.record(llm.Usage{RequestID: nextReqID(), UserID: userID, KeyID: keyID, ModelID: modelID, Endpoint: llm.EndpointChat,
		PromptTokens: p, CompletionTokens: c, InputPrice: 100, OutputPrice: 400, CostMicro: costMicro,
		Status: llm.UsageOK, HTTPStatus: 200, CreatedAt: at})
}

// spend 记一条当前时刻的 ok 用量（输入 10 / 输出 20 token）。
func (e *env) spend(userID, keyID, modelID, costMicro int64) {
	e.t.Helper()
	e.usageAt(userID, keyID, modelID, e.clk.Now(), 10, 20, costMicro)
}

func (e *env) settle() {
	e.t.Helper()
	if err := e.svc.Settle(e.ctx); err != nil {
		e.t.Fatalf("Settle: %v", err)
	}
}

type acctState struct{ unsettled, carry, debt int64 }

func (e *env) acct(userID int64) acctState {
	e.t.Helper()
	var a acctState
	if err := e.pool.QueryRow(e.ctx, `SELECT unsettled_micro, carry_micro, debt FROM llm_accounts WHERE user_id = $1`, userID).
		Scan(&a.unsettled, &a.carry, &a.debt); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) balance(userID int64) int64 {
	e.t.Helper()
	b, err := e.wallet.Balance(e.ctx, userID)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

type payRow struct {
	id     int64
	amount int64
	refID  string
	remark string
}

// llmPays 用户的 llm_pay 流水，按 id 正序。
func (e *env) llmPays(userID int64) []payRow {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT id, amount, ref_id, remark FROM wallet_ledger
		WHERE user_id = $1 AND kind = 'llm_pay' AND ref_type = $2 ORDER BY id`, userID, llm.LedgerRefTypeSettlement)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []payRow
	for rows.Next() {
		var p payRow
		if err := rows.Scan(&p.id, &p.amount, &p.refID, &p.remark); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// settlementsOf 用户的结算记录（经 ListSettlements），按 id 正序。
func (e *env) settlementsOf(userID int64) []llm.Settlement {
	e.t.Helper()
	list, total, err := e.svc.ListSettlements(e.ctx, userID, paging.Page{Number: 1, Size: 100})
	if err != nil {
		e.t.Fatal(err)
	}
	if total != len(list) {
		e.t.Fatalf("ListSettlements total %d, len %d", total, len(list))
	}
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list
}

func (e *env) invariants() {
	e.t.Helper()
	if err := llm.CheckInvariants(e.ctx, e.pool); err != nil {
		e.t.Fatal(err)
	}
}

func TestSettleThresholds(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b")
	u, k, _ := e.settleUser(1000)
	start := e.clk.Now()

	// ¥0.5：未到 10 分钟不结
	e.spend(u, k, m.ID, 50_000_000)
	e.settle()
	e.clk.Advance(llm.SettleMaxAge - time.Second)
	e.settle()
	if n := len(e.settlementsOf(u)); n != 0 {
		t.Fatalf("settled before 10 minutes: %d settlements", n)
	}
	if a := e.acct(u); a.unsettled != 50_000_000 {
		t.Fatalf("unsettled = %d", a.unsettled)
	}
	// 恰好 10 分钟：结
	e.clk.Advance(time.Second)
	e.settle()
	ss := e.settlementsOf(u)
	if len(ss) != 1 {
		t.Fatalf("settlements = %d, want 1", len(ss))
	}
	s := ss[0]
	if s.Requests != 1 || s.PromptTokens != 10 || s.CompletionTokens != 20 || s.CostMicro != 50_000_000 ||
		s.UsageAmount != 50 || s.DebtAmount != 0 || s.Charged != 50 || s.WalletLedgerID == nil ||
		s.PeriodFrom == nil || !s.PeriodFrom.Equal(start) || s.PeriodTo == nil || !s.PeriodTo.Equal(start) {
		t.Fatalf("settlement = %+v", s)
	}
	pays := e.llmPays(u)
	if len(pays) != 1 || pays[0].amount != -50 || pays[0].refID != strconv.FormatInt(s.ID, 10) || pays[0].id != *s.WalletLedgerID ||
		pays[0].remark != "Token 消费 1 次请求 · 输入 10 · 输出 20 token" {
		t.Fatalf("ledger = %+v", pays)
	}
	if a := e.acct(u); a != (acctState{}) {
		t.Fatalf("account after settle = %+v", a)
	}
	if b := e.balance(u); b != 950 {
		t.Fatalf("balance = %d", b)
	}

	// ¥0.6 不结；再 ¥0.4 合计 ≥ ¥1 立即结
	e.spend(u, k, m.ID, 60_000_000)
	e.settle()
	if n := len(e.settlementsOf(u)); n != 1 {
		t.Fatalf("settled ¥0.6 immediately: %d", n)
	}
	e.spend(u, k, m.ID, 40_000_000)
	e.settle()
	ss = e.settlementsOf(u)
	if len(ss) != 2 || ss[1].Requests != 2 || ss[1].UsageAmount != 100 || ss[1].Charged != 100 {
		t.Fatalf("settlements = %+v", ss)
	}
	if pays := e.llmPays(u); len(pays) != 2 || pays[1].amount != -100 ||
		pays[1].remark != "Token 消费 2 次请求 · 输入 20 · 输出 40 token" {
		t.Fatalf("ledger = %+v", pays)
	}
	if b := e.balance(u); b != 850 {
		t.Fatalf("balance = %d", b)
	}

	// 只有 0 费用行（上游失败）的用户：10 分钟后也结算（全零结算、无流水），明细才能被标记、日后清理
	u2, k2, _ := e.settleUser(0)
	e.record(llm.Usage{RequestID: nextReqID(), UserID: u2, KeyID: k2, ModelID: m.ID, Endpoint: llm.EndpointChat,
		PromptTokens: 5, InputPrice: 100, OutputPrice: 400, CostMicro: 999, Status: llm.UsageUpstreamError, HTTPStatus: 502})
	e.settle()
	if n := len(e.settlementsOf(u2)); n != 0 {
		t.Fatalf("zero-cost row settled immediately: %d", n)
	}
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	ss = e.settlementsOf(u2)
	if len(ss) != 1 || ss[0].Requests != 1 || ss[0].PromptTokens != 5 || ss[0].CostMicro != 0 || ss[0].Charged != 0 ||
		ss[0].WalletLedgerID != nil {
		t.Fatalf("zero-cost settlement = %+v", ss)
	}
	if n := len(e.llmPays(u2)); n != 0 {
		t.Fatalf("zero-cost settlement wrote %d ledger rows", n)
	}
	if n := len(e.settlementsOf(u)); n != 2 {
		t.Fatalf("user without usage got a settlement: %d", n)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_usage WHERE settlement_id IS NULL`); n != 0 {
		t.Fatalf("%d unsettled usage rows", n)
	}
	e.invariants()
}

func TestSettleCarry(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b")
	u, k, _ := e.settleUser(100)
	carries := []int64{400_000, 800_000, 200_000}
	for i, want := range carries {
		e.spend(u, k, m.ID, 400_000)
		e.clk.Advance(llm.SettleMaxAge)
		e.settle()
		ss := e.settlementsOf(u)
		if len(ss) != i+1 {
			t.Fatalf("round %d: settlements = %d", i, len(ss))
		}
		s := ss[i]
		if a := e.acct(u); a.carry != want || a.unsettled != 0 || a.debt != 0 {
			t.Fatalf("round %d: account = %+v, want carry %d", i, a, want)
		}
		pays := e.llmPays(u)
		if i < 2 {
			if s.UsageAmount != 0 || s.Charged != 0 || s.WalletLedgerID != nil || len(pays) != 0 {
				t.Fatalf("round %d: settlement %+v, ledger %+v", i, s, pays)
			}
		} else if s.UsageAmount != 1 || s.Charged != 1 || s.WalletLedgerID == nil || len(pays) != 1 || pays[0].amount != -1 {
			t.Fatalf("round %d: settlement %+v, ledger %+v", i, s, pays)
		}
		e.invariants()
	}
	if b := e.balance(u); b != 99 {
		t.Fatalf("balance = %d", b)
	}
}

func TestSettleInsufficientBalanceCreatesDebt(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b")
	u, k, plain := e.settleUser(3)
	e.spend(u, k, m.ID, 10_000_000)
	e.clk.Advance(llm.SettleMaxAge)
	if st, code := e.authCode(e.svc, plain); st != 0 { // 结算前一刻进缓存
		t.Fatalf("authorize before debt: %d %s", st, code)
	}
	e.settle()
	ss := e.settlementsOf(u)
	if len(ss) != 1 || ss[0].UsageAmount != 10 || ss[0].DebtAmount != 0 || ss[0].Charged != 3 {
		t.Fatalf("settlements = %+v", ss)
	}
	if pays := e.llmPays(u); len(pays) != 1 || pays[0].amount != -3 {
		t.Fatalf("ledger = %+v", pays)
	}
	if a := e.acct(u); a.debt != 7 || a.unsettled != 0 {
		t.Fatalf("account = %+v", a)
	}
	if b := e.balance(u); b != 0 {
		t.Fatalf("balance = %d", b)
	}
	// 欠费后 Authorize 立即 402（结算提交后失效了缓存，不等 30 秒）
	if st, code := e.authCode(e.svc, plain); st != 402 || code != "insufficient_quota" {
		t.Fatalf("authorize after debt: %d %s", st, code)
	}
	// 余额 0、只有欠费：不结算
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	if n := len(e.settlementsOf(u)); n != 1 {
		t.Fatalf("settled debt with zero balance: %d", n)
	}
	// 没有钱包账户行的用户：欠下全部
	u2, k2, _ := e.settleUser(0)
	e.spend(u2, k2, m.ID, 120_000_000)
	e.settle()
	if a := e.acct(u2); a.debt != 120 {
		t.Fatalf("no wallet account: %+v", a)
	}
	if ss := e.settlementsOf(u2); len(ss) != 1 || ss[0].Charged != 0 || ss[0].WalletLedgerID != nil {
		t.Fatalf("no wallet account settlement: %+v", ss)
	}
	e.invariants()
}

func TestSettleDebtRepaidOnRecharge(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b")

	// A：欠 7 分 → 充值 → 无新用量也扣欠费，一条流水「欠费补扣」
	a, ka, plainA := e.settleUser(3)
	e.spend(a, ka, m.ID, 10_000_000)
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	if st, _ := e.authCode(e.svc, plainA); st != 402 {
		t.Fatalf("authorize in debt: %d", st)
	}
	e.fund(a, 100)
	e.settle()
	ss := e.settlementsOf(a)
	if len(ss) != 2 {
		t.Fatalf("settlements = %+v", ss)
	}
	s := ss[1]
	if s.Requests != 0 || s.CostMicro != 0 || s.UsageAmount != 0 || s.DebtAmount != 7 || s.Charged != 7 ||
		s.PeriodFrom != nil || s.PeriodTo != nil || s.WalletLedgerID == nil {
		t.Fatalf("debt repayment settlement = %+v", s)
	}
	pays := e.llmPays(a)
	if len(pays) != 2 || pays[1].amount != -7 || pays[1].remark != "Token 消费欠费补扣" || pays[1].refID != strconv.FormatInt(s.ID, 10) {
		t.Fatalf("ledger = %+v", pays)
	}
	if st := e.acct(a); st.debt != 0 {
		t.Fatalf("account = %+v", st)
	}
	if b := e.balance(a); b != 93 {
		t.Fatalf("balance = %d", b)
	}
	if st, code := e.authCode(e.svc, plainA); st != 0 {
		t.Fatalf("authorize after repayment: %d %s", st, code)
	}

	// B：欠费 + 未到期的小额用量 → 只扣欠费，用量留到期后单独结
	b, kb, _ := e.settleUser(2)
	e.spend(b, kb, m.ID, 5_000_000)
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	e.fund(b, 100)
	e.spend(b, kb, m.ID, 4_000_000)
	e.settle()
	pays = e.llmPays(b)
	if len(pays) != 2 || pays[1].amount != -3 || pays[1].remark != "Token 消费欠费补扣" {
		t.Fatalf("B ledger = %+v", pays)
	}
	if st := e.acct(b); st.debt != 0 || st.unsettled != 4_000_000 {
		t.Fatalf("B account = %+v", st)
	}
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	pays = e.llmPays(b)
	if len(pays) != 3 || pays[2].amount != -4 || pays[2].remark != "Token 消费 1 次请求 · 输入 10 · 输出 20 token" {
		t.Fatalf("B ledger = %+v", pays)
	}

	// C：欠 ¥1.23 + 新用量 4 分 → 并入同一条流水，备注带欠费
	c, kc, _ := e.settleUser(2)
	e.spend(c, kc, m.ID, 125_000_000)
	e.settle()
	if st := e.acct(c); st.debt != 123 {
		t.Fatalf("C account = %+v", st)
	}
	e.fund(c, 1000)
	e.spend(c, kc, m.ID, 4_000_000)
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	pays = e.llmPays(c)
	if len(pays) != 2 || pays[1].amount != -127 || pays[1].remark != "Token 消费 1 次请求 · 输入 10 · 输出 20 token（含欠费 ¥1.23）" {
		t.Fatalf("C ledger = %+v", pays)
	}
	ss = e.settlementsOf(c)
	if s := ss[1]; s.UsageAmount != 4 || s.DebtAmount != 123 || s.Charged != 127 {
		t.Fatalf("C settlement = %+v", s)
	}

	// D：充值不够还清 → 部分扣、剩余继续欠
	d, kd, _ := e.settleUser(3)
	e.spend(d, kd, m.ID, 10_000_000)
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	e.fund(d, 4)
	e.settle()
	if st := e.acct(d); st.debt != 3 {
		t.Fatalf("D account = %+v", st)
	}
	if bal := e.balance(d); bal != 0 {
		t.Fatalf("D balance = %d", bal)
	}
	e.invariants()
}

func TestSettleMaxIDBoundary(t *testing.T) {
	e := newEnv(t)
	logs := e.withLogBuf()
	m := e.createModel("qwen3-4b")
	u, k, _ := e.settleUser(1000)
	e.spend(u, k, m.ID, 60_000_000)
	e.spend(u, k, m.ID, 60_000_000)
	var lateID int64
	llm.SetSettleBeforeMarkHook(e.svc, func(userID int64) {
		if userID == u && lateID == 0 {
			// 另一个连接（自动提交）在 max_id 读出之后插入一行：不持有账户锁，所以不会被结算事务阻塞
			lateID = e.rawUsage(u, k, m.ID, 30_000_000)
		}
	})
	e.settle()
	if lateID == 0 {
		t.Fatal("hook not called")
	}
	var sid *int64
	if err := e.pool.QueryRow(e.ctx, `SELECT settlement_id FROM llm_usage WHERE id = $1`, lateID).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	if sid != nil {
		t.Fatalf("row inserted after max_id was settled into %d", *sid)
	}
	ss := e.settlementsOf(u)
	if len(ss) != 1 || ss[0].Requests != 2 || ss[0].CostMicro != 120_000_000 || ss[0].Charged != 120 {
		t.Fatalf("settlements = %+v", ss)
	}
	// 计数自愈：unsettled_micro 重算为剩余未结算明细之和（这一行没走 RecordUsage，与预期 0 不符 → 记 Error）
	if a := e.acct(u); a.unsettled != 30_000_000 {
		t.Fatalf("account = %+v", a)
	}
	if !strings.Contains(logs.String(), "unsettled_micro mismatch") {
		t.Fatalf("mismatch not logged:\n%s", logs.String())
	}
	e.invariants()
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	if err := e.pool.QueryRow(e.ctx, `SELECT settlement_id FROM llm_usage WHERE id = $1`, lateID).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	ss = e.settlementsOf(u)
	if len(ss) != 2 || sid == nil || *sid != ss[1].ID || ss[1].Requests != 1 || ss[1].Charged != 30 {
		t.Fatalf("second settlement = %+v (late row sid %v)", ss, sid)
	}
	e.invariants()
}

func TestSettleUnsettledDriftHealed(t *testing.T) {
	e := newEnv(t)
	logs := e.withLogBuf()
	u, _, _ := e.settleUser(1000)
	// 账上 ¥1.5 未结算，但没有任何明细
	e.exec(`UPDATE llm_accounts SET unsettled_micro = 150000000 WHERE user_id = $1`, u)
	e.settle()
	if a := e.acct(u); a != (acctState{}) {
		t.Fatalf("account after heal = %+v", a)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_settlements`); n != 0 {
		t.Fatalf("heal wrote %d settlements", n)
	}
	if n := strings.Count(logs.String(), "unsettled_micro drift"); n != 1 {
		t.Fatalf("drift logged %d times:\n%s", n, logs.String())
	}
	// 校正后不再是候选：再跑一轮不再记 Error
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	if n := strings.Count(logs.String(), "level=ERROR"); n != 1 {
		t.Fatalf("errors logged %d times:\n%s", n, logs.String())
	}
	if b := e.balance(u); b != 1000 {
		t.Fatalf("balance = %d", b)
	}
	e.invariants()
}

func TestSettleDebtOnlyDrainedBalanceLeavesNoRow(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b")
	u, k, _ := e.settleUser(3)
	e.spend(u, k, m.ID, 10_000_000)
	e.clk.Advance(llm.SettleMaxAge)
	e.settle() // 欠 7
	e.fund(u, 5)
	// 结算读到余额 5 之后、ApplyUpTo 锁钱包之前，另一笔扣款把余额扣光
	llm.SetSettleBeforeMarkHook(e.svc, func(userID int64) {
		if userID == u {
			if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
				_, err := e.wallet.Apply(e.ctx, tx, wallet.Entry{UserID: u, Amount: -5, Kind: wallet.KindOrderPay,
					RefType: "test", RefID: "drain"})
				return err
			}); err != nil {
				t.Fatalf("drain: %v", err)
			}
		}
	})
	e.settle()
	llm.SetSettleBeforeMarkHook(e.svc, nil)
	if ss := e.settlementsOf(u); len(ss) != 1 {
		t.Fatalf("drained debt-only settlement left rows: %+v", ss)
	}
	if a := e.acct(u); a.debt != 7 {
		t.Fatalf("account = %+v", a)
	}
	e.invariants()
	// 再充值后正常扣
	e.fund(u, 10)
	e.settle()
	if ss := e.settlementsOf(u); len(ss) != 2 || ss[1].Charged != 7 || ss[1].DebtAmount != 7 {
		t.Fatalf("settlements = %+v", ss)
	}
	e.invariants()
}

func TestAdminListSettlements(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b")
	a, ka, _ := e.settleUser(10_000)
	b, kb, _ := e.settleUser(10_000)
	t0 := e.clk.Now()
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	e.spend(a, ka, m.ID, 100_000_000)
	e.settle() // a@t0
	e.setClock(at(time.Hour))
	e.spend(a, ka, m.ID, 100_000_000)
	e.spend(b, kb, m.ID, 100_000_000)
	e.settle() // a@1h, b@1h
	e.setClock(at(2 * time.Hour))
	e.spend(b, kb, m.ID, 100_000_000)
	e.settle() // b@2h

	list := func(f llm.SettlementFilter, p paging.Page) ([]llm.Settlement, int) {
		t.Helper()
		l, total, err := e.svc.AdminListSettlements(e.ctx, f, p)
		if err != nil {
			t.Fatal(err)
		}
		return l, total
	}
	desc := func(l []llm.Settlement) string {
		var parts []string
		for i, x := range l {
			if i > 0 && x.ID > l[i-1].ID {
				t.Fatalf("not newest first: %+v", l)
			}
			parts = append(parts, fmt.Sprintf("%d@%s", x.UserID, x.CreatedAt.Sub(t0)))
		}
		return strings.Join(parts, " ")
	}
	all, total := list(llm.SettlementFilter{}, paging.Page{Number: 1, Size: 20})
	if total != 4 || len(all) != 4 {
		t.Fatalf("all: total %d, %s", total, desc(all))
	}
	// [From, To)：1h 两条在内，2h 那条不在
	from, to := at(time.Hour), at(2*time.Hour)
	got, total := list(llm.SettlementFilter{From: &from, To: &to}, paging.Page{Number: 1, Size: 20})
	if total != 2 || len(got) != 2 || got[0].CreatedAt.Sub(t0) != time.Hour || got[1].CreatedAt.Sub(t0) != time.Hour {
		t.Fatalf("window: total %d, %s", total, desc(got))
	}
	got, total = list(llm.SettlementFilter{UserID: b, From: &from}, paging.Page{Number: 1, Size: 20})
	if total != 2 || len(got) != 2 || got[0].UserID != b || got[0].CreatedAt.Sub(t0) != 2*time.Hour {
		t.Fatalf("user b from 1h: total %d, %s", total, desc(got))
	}
	got, total = list(llm.SettlementFilter{UserID: a, To: &from}, paging.Page{Number: 1, Size: 20})
	if total != 1 || len(got) != 1 || got[0].UserID != a || !got[0].CreatedAt.Equal(t0) {
		t.Fatalf("user a before 1h: total %d, %s", total, desc(got))
	}
	// 分页：总数不受页大小影响，第 2 页是更早的
	p1, total := list(llm.SettlementFilter{UserID: a}, paging.Page{Number: 1, Size: 1})
	p2, _ := list(llm.SettlementFilter{UserID: a}, paging.Page{Number: 2, Size: 1})
	if total != 2 || len(p1) != 1 || len(p2) != 1 || p1[0].ID <= p2[0].ID {
		t.Fatalf("paging: total %d, %s / %s", total, desc(p1), desc(p2))
	}
	if l, total := list(llm.SettlementFilter{UserID: a, From: &to}, paging.Page{Number: 1, Size: 20}); total != 0 || l == nil || len(l) != 0 {
		t.Fatalf("empty: total %d, %v", total, l)
	}
}

func TestSettleReplayIsNoop(t *testing.T) {
	e := newEnv(t)
	logs := e.withLogBuf()
	m := e.createModel("qwen3-4b")
	u, k, _ := e.settleUser(1000)
	other, ko, _ := e.settleUser(1000)
	e.spend(u, k, m.ID, 150_000_000)
	// 预先占住下一个结算 ID 的流水（模拟重放）
	next := e.queryInt(`SELECT CASE WHEN is_called THEN last_value + 1 ELSE last_value END FROM llm_settlements_id_seq`)
	fake := e.queryInt(`INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		VALUES ($1, -1, 0, 'llm_pay', $2, $3, 'replay', $4) RETURNING id`, u, llm.LedgerRefTypeSettlement, strconv.FormatInt(next, 10), e.clk.Now())
	if err := e.svc.Settle(e.ctx); err != nil {
		t.Fatalf("Settle returned %v; per-account failures must only be logged", err)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_settlements`); n != 0 {
		t.Fatalf("settlements after duplicate = %d", n)
	}
	if a := e.acct(u); a != (acctState{unsettled: 150_000_000}) {
		t.Fatalf("account changed: %+v", a)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_usage WHERE settlement_id IS NOT NULL`); n != 0 {
		t.Fatalf("%d usage rows marked", n)
	}
	if b := e.balance(u); b != 1000 {
		t.Fatalf("balance = %d", b)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_usage_daily`); n != 0 {
		t.Fatalf("daily rows = %d", n)
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("no error logged:\n%s", logs.String())
	}

	// 其它账户不受影响：撞重复的账户只影响自己
	e.exec(`DELETE FROM wallet_ledger WHERE id = $1`, fake)
	e.spend(other, ko, m.ID, 150_000_000)
	e.settle()
	if ss := e.settlementsOf(u); len(ss) != 1 || ss[0].ID == next || ss[0].Charged != 150 {
		t.Fatalf("settlements after replay = %+v", ss)
	}
	if ss := e.settlementsOf(other); len(ss) != 1 || ss[0].Charged != 150 {
		t.Fatalf("other settlements = %+v", ss)
	}
	e.invariants()
}

func TestSettleDailyAggregate(t *testing.T) {
	e := newEnv(t)
	m1 := e.createModel("qwen3-4b")
	m2 := e.createModel("qwen3-8b")
	u, k, _ := e.settleUser(10_000)
	d15 := time.Date(2026, 1, 15, 23, 59, 59, 0, tz.CST)
	d16 := time.Date(2026, 1, 16, 0, 0, 0, 0, tz.CST)
	e.setClock(d16.Add(time.Hour))
	e.usageAt(u, k, m1.ID, d15, 10, 20, 1_000_000)
	e.usageAt(u, k, m1.ID, d16, 10, 20, 2_000_000)
	e.usageAt(u, k, m2.ID, d16.Add(30*time.Minute), 10, 20, 3_000_000)
	e.record(llm.Usage{RequestID: nextReqID(), UserID: u, KeyID: k, ModelID: m1.ID, Endpoint: llm.EndpointChat,
		PromptTokens: 5, InputPrice: 100, OutputPrice: 400, Status: llm.UsageNoNode, HTTPStatus: 503, CreatedAt: d16})
	e.settle()

	type day struct {
		day                  string
		model                int64
		requests, p, c, cost int64
	}
	daily := func() []day {
		rows, err := e.pool.Query(e.ctx, `SELECT day::text, model_id, requests, prompt_tokens, completion_tokens, cost_micro
			FROM llm_usage_daily WHERE user_id = $1 ORDER BY day, model_id`, u)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []day
		for rows.Next() {
			var d day
			if err := rows.Scan(&d.day, &d.model, &d.requests, &d.p, &d.c, &d.cost); err != nil {
				t.Fatal(err)
			}
			out = append(out, d)
		}
		return out
	}
	want := []day{
		{"2026-01-15", m1.ID, 1, 10, 20, 1_000_000},
		{"2026-01-16", m1.ID, 2, 15, 20, 2_000_000},
		{"2026-01-16", m2.ID, 1, 10, 20, 3_000_000},
	}
	if got := daily(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("daily = %v, want %v", got, want)
	}
	ss := e.settlementsOf(u)
	if len(ss) != 1 || ss[0].Requests != 4 || !ss[0].PeriodFrom.Equal(d15) || !ss[0].PeriodTo.Equal(d16.Add(30*time.Minute)) {
		t.Fatalf("settlements = %+v", ss)
	}
	// 第二次结算累加到已有的 (day, user, model) 行
	e.spend(u, k, m1.ID, 4_000_000)
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	want[1] = day{"2026-01-16", m1.ID, 3, 25, 40, 6_000_000}
	if got := daily(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("daily after second settlement = %v, want %v", got, want)
	}
	e.invariants()
}

func TestPurgeOnlySettledOld(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b")
	u, k, _ := e.settleUser(10_000)
	t0 := e.clk.Now()
	e.spend(u, k, m.ID, 1_000_000) // A、B：T0、已结算
	e.spend(u, k, m.ID, 1_000_000)
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	sid := e.settlementsOf(u)[0].ID

	e.setClock(t0.Add(89 * 24 * time.Hour))
	e.spend(u, k, m.ID, 1_000_000) // C：89 天前、已结算
	cID := e.queryInt(`SELECT max(id) FROM llm_usage`)
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()

	e.setClock(t0.Add(llm.UsageRetention + time.Hour))
	e.usageAt(u, k, m.ID, t0, 10, 20, 1_000_000) // D：迟到记账，created_at = T0、未结算
	dID := e.queryInt(`SELECT max(id) FROM llm_usage`)
	e.spend(u, k, m.ID, 1_000_000) // E：现在、未结算
	eID := e.queryInt(`SELECT max(id) FROM llm_usage`)

	n, err := llm.Purge(e.ctx, e.svc)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("purged %d, want 2", n)
	}
	ids := func() string {
		rows, err := e.pool.Query(e.ctx, `SELECT id FROM llm_usage ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, strconv.FormatInt(id, 10))
		}
		return strings.Join(out, ",")
	}
	if got, want := ids(), fmt.Sprintf("%d,%d,%d", cID, dID, eID); got != want {
		t.Fatalf("remaining usage ids = %s, want %s", got, want)
	}

	// 每轮最多 PurgeBatch 行
	e.exec(`INSERT INTO llm_usage (request_id, user_id, key_id, model_id, endpoint, stream, input_price, output_price, cost_micro,
			status, http_status, settlement_id, created_at)
		SELECT 'req-bulk' || lpad(g::text, 12, '0'), $1, $2, $3, 'chat', false, 100, 400, 0, 'ok', 200, $4, $5
		FROM generate_series(1, $6::int) g`, u, k, m.ID, sid, t0, llm.PurgeBatch+1)
	for i, want := range []int64{llm.PurgeBatch, 1, 0} {
		n, err := llm.Purge(e.ctx, e.svc)
		if err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("purge round %d: %d rows, want %d", i, n, want)
		}
	}

	// Settle 本身也清理：D 已超过 10 分钟 → 结算（连同 E）；D 满 90 天在同一轮里被删，C、E 保留
	e.settle()
	if got, want := ids(), fmt.Sprintf("%d,%d", cID, eID); got != want {
		t.Fatalf("after Settle remaining usage ids = %s, want %s", got, want)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_usage WHERE settlement_id IS NULL`); n != 0 {
		t.Fatalf("%d usage rows unsettled", n)
	}
	if a := e.acct(u); a.unsettled != 0 {
		t.Fatalf("account = %+v", a)
	}
}

func TestSettleManyAccountsAndInvariants(t *testing.T) {
	e := newEnv(t)
	logs := e.withLogBuf()
	m1 := e.createModel("qwen3-4b")
	m2 := e.createModel("qwen3-8b")
	r := rand.New(rand.NewPCG(9, 8))
	const nUsers = 20
	type user struct{ id, key int64 }
	users := make([]user, nUsers)
	for i := range users {
		fund := int64(0)
		if i%4 != 0 {
			fund = int64(r.IntN(300))
		}
		users[i].id, users[i].key, _ = e.settleUser(fund)
	}
	statuses := []string{llm.UsageOK, llm.UsageOK, llm.UsageOK, llm.UsageClientAbort, llm.UsageUpstreamError, llm.UsageNoNode}
	for round := 0; round < 8; round++ {
		// 预先生成本轮用量（rand 不并发安全）
		plans := make([][]llm.Usage, nUsers)
		for i, us := range users {
			for j := r.IntN(9); j > 0; j-- {
				mid := m1.ID
				if r.IntN(2) == 0 {
					mid = m2.ID
				}
				cost := int64(r.IntN(30_000_000))
				if r.IntN(10) == 0 {
					cost = 150_000_000
				}
				plans[i] = append(plans[i], llm.Usage{RequestID: nextReqID(), UserID: us.id, KeyID: us.key, ModelID: mid,
					Endpoint: llm.EndpointChat, PromptTokens: r.IntN(5000), CompletionTokens: r.IntN(2000), InputPrice: 100,
					OutputPrice: 400, CostMicro: cost, Status: statuses[r.IntN(len(statuses))], HTTPStatus: 200,
					CreatedAt: e.clk.Now().Add(-time.Duration(r.IntN(600)) * time.Second)})
			}
		}
		// 记账与两个并发 Settle（多副本无咨询锁时的最坏情况）同时进行
		type walletOp struct {
			user   int64
			amount int64
		}
		var wops []walletOp
		for j := r.IntN(10); j > 0; j-- {
			amt := int64(1 + r.IntN(200))
			if r.IntN(3) == 0 {
				amt = -int64(1 + r.IntN(50)) // 其它业务扣款（余额不足就算了）
			}
			wops = append(wops, walletOp{users[r.IntN(nUsers)].id, amt})
		}
		var wg sync.WaitGroup
		errs := make(chan error, nUsers+8)
		// 钱包写入方（充值 / 扣款，只锁 wallet_accounts）与清理和结算并发
		wg.Add(2)
		go func() {
			defer wg.Done()
			for _, op := range wops {
				err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
					kind := wallet.KindRecharge
					if op.amount < 0 {
						kind = wallet.KindOrderPay
					}
					_, err := e.wallet.Apply(e.ctx, tx, wallet.Entry{UserID: op.user, Amount: op.amount, Kind: kind,
						RefType: "test", RefID: nextReqID()})
					return err
				})
				if err != nil && !errors.Is(err, apperr.ErrInsufficientBalance) {
					errs <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 3 {
				if _, err := llm.Purge(e.ctx, e.svc); err != nil {
					errs <- err
					return
				}
			}
		}()
		for i := range plans {
			wg.Add(1)
			go func(us []llm.Usage) {
				defer wg.Done()
				for _, u := range us {
					if err := e.svc.RecordUsage(e.ctx, u); err != nil {
						errs <- err
						return
					}
				}
			}(plans[i])
		}
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 3 {
					if err := e.svc.Settle(e.ctx); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		e.invariants()
		e.clk.Advance(time.Duration(r.IntN(12)) * time.Minute)
	}
	e.clk.Advance(llm.SettleMaxAge)
	e.settle()
	if n := e.queryInt(`SELECT count(*) FROM llm_usage WHERE settlement_id IS NULL`); n != 0 {
		t.Fatalf("%d usage rows still unsettled", n)
	}
	if n := e.queryInt(`SELECT count(*) FROM wallet_accounts WHERE balance < 0`); n != 0 {
		t.Fatalf("%d negative balances", n)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_accounts WHERE debt > 0`); n == 0 {
		t.Fatal("no user ended in debt; test does not exercise ApplyUpTo shortfall")
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_settlements WHERE debt_amount > 0 AND charged > 0`); n == 0 {
		t.Fatal("no debt repayment happened")
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("errors logged:\n%s", logs.String())
	}
	e.invariants()
}
