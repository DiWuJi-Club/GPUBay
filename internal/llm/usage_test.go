package llm_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/tz"
)

func intp(v int) *int { return &v }

func TestCostAndEstimate(t *testing.T) {
	chat := llm.Model{Kind: llm.KindChat, InputPrice: 50, OutputPrice: 150, MaxOutputTokens: 8192, DefaultMaxTokens: 4096}
	emb := llm.Model{Kind: llm.KindEmbedding, InputPrice: 50, OutputPrice: 150, MaxOutputTokens: 8192, DefaultMaxTokens: 4096}
	costs := []struct {
		name  string
		m     llm.Model
		p, c  int
		micro int64
	}{
		{"chat", chat, 100, 50, 12_500},
		{"chat zero", chat, 0, 0, 0},
		{"chat output only", chat, 0, 7, 1_050},
		{"embedding ignores output", emb, 100, 50, 5_000},
		{"large", llm.Model{Kind: llm.KindChat, InputPrice: 100_000_000, OutputPrice: 100_000_000}, 1 << 30, 1 << 30,
			2 * (1 << 30) * 100_000_000},
	}
	for _, c := range costs {
		if got := llm.Cost(c.m, c.p, c.c); got != c.micro {
			t.Errorf("Cost %s = %d, want %d", c.name, got, c.micro)
		}
	}

	m := llm.Model{Kind: llm.KindChat, InputPrice: 100, OutputPrice: 400, MaxOutputTokens: 8192, DefaultMaxTokens: 4096}
	e := llm.Model{Kind: llm.KindEmbedding, InputPrice: 100, OutputPrice: 400, MaxOutputTokens: 8192, DefaultMaxTokens: 4096}
	ests := []struct {
		name    string
		m       llm.Model
		bodyLen int
		max     *int
		fen     int64
	}{
		// 1000 × 100 + 4096 × 400 = 1,738,400 微分 → 2 分
		{"default max_tokens", m, 3000, nil, 2},
		// 夹到 max_output 8192：100,000 + 3,276,800 = 3,376,800 → 4
		{"clamped to max_output", m, 3000, intp(100_000), 4},
		// 100,000 + 4,000 = 104,000 → 1（向上取整）
		{"small max_tokens ceil", m, 3000, intp(10), 1},
		// max_tokens ≤ 0 按模型默认
		{"zero max_tokens uses default", m, 3000, intp(0), 2},
		{"negative max_tokens uses default", m, 3000, intp(-5), 2},
		// bodyLen/3 整数除法：2 字节 → 0 token
		{"tiny body", m, 2, intp(1), 1},
		// embedding 只算输入：10000 × 100 = 1,000,000 → 恰好 1 分
		{"embedding exact", e, 30_000, intp(8192), 1},
		{"embedding ceil", e, 30_003, nil, 2},
		{"embedding empty", e, 2, nil, 0},
		// 最大请求体：699,050 × 100 + 8192 × 400 = 73,181,800 → 74
		{"max body", m, llm.MaxBodyBytes, intp(1 << 30), 74},
	}
	for _, c := range ests {
		if got := llm.EstimateFen(c.m, c.bodyLen, c.max); got != c.fen {
			t.Errorf("EstimateFen %s = %d, want %d", c.name, got, c.fen)
		}
	}
}

var reqSeq atomic.Int64

func nextReqID() string { return fmt.Sprintf("req-t%015d", reqSeq.Add(1)) }

// usageFor 一条 ok 的 chat 用量（价格按模型快照、费用按 Cost 算）。
func (e *env) usageFor(userID, keyID int64, m llm.Model, p, c int) llm.Usage {
	return llm.Usage{RequestID: nextReqID(), UserID: userID, KeyID: keyID, ModelID: m.ID, Endpoint: llm.EndpointChat,
		PromptTokens: p, CompletionTokens: c, InputPrice: m.InputPrice, OutputPrice: m.OutputPrice, CostMicro: llm.Cost(m, p, c),
		Status: llm.UsageOK, HTTPStatus: 200, LatencyMS: 120, ClientIP: "10.0.0.9", CreatedAt: e.clk.Now()}
}

// setClock 把假时钟推进到 t（只能向前）。
func (e *env) setClock(t time.Time) {
	e.t.Helper()
	d := t.Sub(e.clk.Now())
	if d < 0 {
		e.t.Fatalf("clock moving backwards to %v", t)
	}
	e.clk.Advance(d)
}

func (e *env) record(u llm.Usage) {
	e.t.Helper()
	if err := e.svc.RecordUsage(e.ctx, u); err != nil {
		e.t.Fatalf("RecordUsage %s: %v", u.RequestID, err)
	}
}

func (e *env) unsettled(userID int64) int64 {
	e.t.Helper()
	return e.queryInt(`SELECT unsettled_micro FROM llm_accounts WHERE user_id = $1`, userID)
}

type keyMonth struct {
	ym       string
	micro    int64
	lastUsed *time.Time
}

func (e *env) keyMonth(keyID int64) keyMonth {
	e.t.Helper()
	var k keyMonth
	if err := e.pool.QueryRow(e.ctx, `SELECT month_ym, month_cost_micro, last_used_at FROM llm_api_keys WHERE id = $1`, keyID).
		Scan(&k.ym, &k.micro, &k.lastUsed); err != nil {
		e.t.Fatal(err)
	}
	return k
}

func TestRecordUsageUpdatesAccountAndKey(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b", func(in *llm.ModelInput) { in.InputPrice, in.OutputPrice = 50, 150 })
	u := e.verifiedUser()
	_, k := e.createKey(u)
	t0 := e.clk.Now()

	first := e.usageFor(u, k.ID, m, 100, 50)
	first.Stream, first.Estimated, first.TTFTMS = true, true, intp(35)
	e.record(first)
	if got := e.unsettled(u); got != 12_500 {
		t.Fatalf("unsettled = %d, want 12500", got)
	}
	km := e.keyMonth(k.ID)
	if km.ym != "2026-01" || km.micro != 12_500 || km.lastUsed == nil || !km.lastUsed.Equal(t0) {
		t.Fatalf("key = %+v", km)
	}
	// 行内容逐列落库
	var (
		stream, estimated     bool
		p, c, httpStatus, lat int
		ttft                  *int
		inP, outP, cost       int64
		status, endpoint, ip  string
		settlement            *int64
		created               time.Time
	)
	if err := e.pool.QueryRow(e.ctx, `SELECT stream, estimated, prompt_tokens, completion_tokens, http_status, latency_ms, ttft_ms,
		input_price, output_price, cost_micro, status, endpoint, client_ip, settlement_id, created_at FROM llm_usage WHERE request_id = $1`,
		first.RequestID).Scan(&stream, &estimated, &p, &c, &httpStatus, &lat, &ttft, &inP, &outP, &cost, &status, &endpoint, &ip,
		&settlement, &created); err != nil {
		t.Fatal(err)
	}
	if !stream || !estimated || p != 100 || c != 50 || httpStatus != 200 || lat != 120 || ttft == nil || *ttft != 35 ||
		inP != 50 || outP != 150 || cost != 12_500 || status != "ok" || endpoint != "chat" || ip != "10.0.0.9" ||
		settlement != nil || !created.Equal(t0) {
		t.Fatalf("row = %v %v %d %d %d %d %v %d %d %d %s %s %s %v %v", stream, estimated, p, c, httpStatus, lat, ttft, inP, outP, cost,
			status, endpoint, ip, settlement, created)
	}

	// 同月第二条：累加；CreatedAt 零值用当前时钟
	e.clk.Advance(time.Minute)
	second := e.usageFor(u, k.ID, m, 0, 0)
	second.CostMicro, second.CreatedAt = 1_000, time.Time{}
	e.record(second)
	if got := e.unsettled(u); got != 13_500 {
		t.Fatalf("unsettled = %d, want 13500", got)
	}
	if km := e.keyMonth(k.ID); km.micro != 13_500 || !km.lastUsed.Equal(e.clk.Now()) {
		t.Fatalf("key = %+v", km)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_usage WHERE request_id = $1 AND created_at = $2`, second.RequestID, e.clk.Now()); n != 1 {
		t.Fatal("zero CreatedAt not replaced by clock")
	}

	// request_id 重复：ErrDuplicateUsage，不重复计数
	dup := e.usageFor(u, k.ID, m, 100, 50)
	dup.RequestID = first.RequestID
	if err := e.svc.RecordUsage(e.ctx, dup); !errors.Is(err, llm.ErrDuplicateUsage) {
		t.Fatalf("duplicate err = %v", err)
	}
	if got := e.unsettled(u); got != 13_500 {
		t.Fatalf("unsettled after duplicate = %d", got)
	}
	if km := e.keyMonth(k.ID); km.micro != 13_500 {
		t.Fatalf("key after duplicate = %+v", km)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_usage WHERE user_id = $1`, u); n != 2 {
		t.Fatalf("usage rows = %d", n)
	}

	// 跨月（北京时间 2 月 1 日 00:00 = 1 月 31 日 16:00 UTC）：月计数重置后累加
	feb := time.Date(2026, 2, 1, 0, 0, 0, 0, tz.CST)
	e.setClock(feb)
	e.record(e.usageFor(u, k.ID, m, 10, 0)) // 500
	if km := e.keyMonth(k.ID); km.ym != "2026-02" || km.micro != 500 {
		t.Fatalf("key after month rollover = %+v", km)
	}
	if got := e.unsettled(u); got != 14_000 {
		t.Fatalf("unsettled = %d, want 14000（未结算不按月重置）", got)
	}
	// 迟到的 1 月用量：计入未结算，不污染 2 月的月计数，last_used_at 不倒退
	late := e.usageFor(u, k.ID, m, 20, 0) // 1000
	late.CreatedAt = feb.Add(-time.Second)
	e.record(late)
	if km := e.keyMonth(k.ID); km.ym != "2026-02" || km.micro != 500 || !km.lastUsed.Equal(feb) {
		t.Fatalf("key after late usage = %+v", km)
	}
	if got := e.unsettled(u); got != 15_000 {
		t.Fatalf("unsettled = %d, want 15000", got)
	}

	// 没有账户行的用户（Key 直接插库）：记账时插入账户行
	u2 := e.verifiedUser()
	k2 := e.rawKey(u2)
	if n := e.queryInt(`SELECT count(*) FROM llm_accounts WHERE user_id = $1`, u2); n != 0 {
		t.Fatal("precondition: no account row")
	}
	e.record(e.usageFor(u2, k2, m, 100, 50))
	if got := e.unsettled(u2); got != 12_500 {
		t.Fatalf("new account unsettled = %d", got)
	}
	if err := llm.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}

	// 输入校验
	bad := []func(*llm.Usage){
		func(x *llm.Usage) { x.RequestID = "" },
		func(x *llm.Usage) { x.UserID = 0 },
		func(x *llm.Usage) { x.CostMicro = -1 },
		func(x *llm.Usage) { x.PromptTokens = -1 },
		func(x *llm.Usage) { x.Status = "weird" },
	}
	for i, f := range bad {
		x := e.usageFor(u, k.ID, m, 1, 1)
		f(&x)
		if err := e.svc.RecordUsage(e.ctx, x); err == nil {
			t.Errorf("bad usage %d accepted", i)
		}
	}
	if got := e.unsettled(u); got != 15_000 {
		t.Fatalf("unsettled after rejected = %d", got)
	}
}

// 50 个并发记账（两个用户、三把 Key 交错）：账户、Key 月计数与明细严格一致，不变量 1 成立。
func TestRecordUsageConcurrent50(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b")
	ua, ub := e.verifiedUser(), e.verifiedUser()
	_, ka1 := e.createKey(ua)
	_, ka2 := e.createKey(ua)
	kb := e.rawKey(ub) // ub 没有账户行：并发首次插入
	type job struct {
		user, key int64
		cost      int64
	}
	var jobs []job
	sums := map[int64]int64{}
	keySums := map[int64]int64{}
	for i := 0; i < 50; i++ {
		j := job{user: ua, key: ka1.ID, cost: int64(1000 + i)}
		switch i % 3 {
		case 1:
			j.key = ka2.ID
		case 2:
			j.user, j.key = ub, kb
		}
		jobs = append(jobs, j)
		sums[j.user] += j.cost
		keySums[j.key] += j.cost
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(jobs))
	for _, j := range jobs {
		u := e.usageFor(j.user, j.key, m, 1, 1)
		u.CostMicro = j.cost
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.svc.RecordUsage(e.ctx, u)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for user, want := range sums {
		if got := e.unsettled(user); got != want {
			t.Fatalf("user %d unsettled = %d, want %d", user, got, want)
		}
	}
	for key, want := range keySums {
		if got := e.keyMonth(key).micro; got != want {
			t.Fatalf("key %d month = %d, want %d", key, got, want)
		}
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_usage`); n != 50 {
		t.Fatalf("rows = %d", n)
	}
	if err := llm.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
}

// 规格 §6.4：status ≠ ok 且非 client_abort → cost_micro 一律记 0（仍记行、仍更新 last_used_at）；client_abort 按估算收费。
func TestRecordUsageZeroCostStatuses(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b")
	u := e.verifiedUser()
	_, k := e.createKey(u)
	// 终态节点：只为满足 node_id 外键（非终态节点会要求实例存在，见不变量 6）
	node := e.rawNode("node-000000000001", m.ID, e.poolMachine("SN-Z1"), llm.NodeRemoved)
	for _, st := range []string{llm.UsageUpstreamError, llm.UsageUpstreamTimeout, llm.UsageNoNode} {
		x := e.usageFor(u, k.ID, m, 100, 50)
		x.Status, x.HTTPStatus = st, 502
		if st != llm.UsageNoNode {
			x.NodeID = &node
		}
		e.record(x)
		if c := e.queryInt(`SELECT cost_micro FROM llm_usage WHERE request_id = $1`, x.RequestID); c != 0 {
			t.Fatalf("%s cost = %d", st, c)
		}
	}
	if got := e.unsettled(u); got != 0 {
		t.Fatalf("unsettled = %d", got)
	}
	if km := e.keyMonth(k.ID); km.micro != 0 || km.lastUsed == nil {
		t.Fatalf("key = %+v", km)
	}
	// 上游 4xx 透传：status ok、cost 0
	pass := e.usageFor(u, k.ID, m, 0, 0)
	pass.HTTPStatus, pass.NodeID = 400, &node
	e.record(pass)
	// 客户端中断：按估算收费
	abort := e.usageFor(u, k.ID, m, 300, 12)
	abort.Status, abort.Estimated, abort.Stream, abort.HTTPStatus, abort.NodeID = llm.UsageClientAbort, true, true, 200, &node
	e.record(abort)
	want := llm.Cost(m, 300, 12)
	if got := e.unsettled(u); got != want {
		t.Fatalf("unsettled = %d, want %d", got, want)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_usage WHERE user_id = $1`, u); n != 5 {
		t.Fatalf("rows = %d", n)
	}
	if err := llm.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
}

// insertSettled 插一条结算与它覆盖的一条已结算用量，并写对应的 llm_usage_daily（模拟 Task 8 的 Settle）。
func (e *env) insertSettled(userID, keyID, modelID int64, at time.Time, p, c int, costMicro int64) {
	e.t.Helper()
	sid := e.queryInt(`INSERT INTO llm_settlements (user_id, requests, prompt_tokens, completion_tokens, cost_micro, usage_amount, charged,
		created_at) VALUES ($1, 1, $2, $3, $4, 0, 0, $5) RETURNING id`, userID, p, c, costMicro, at)
	e.exec(`INSERT INTO llm_usage (request_id, user_id, key_id, model_id, endpoint, stream, prompt_tokens, completion_tokens,
		input_price, output_price, cost_micro, status, http_status, settlement_id, created_at)
		VALUES ($1, $2, $3, $4, 'chat', false, $5, $6, 100, 400, $7, 'ok', 200, $8, $9)`,
		nextReqID(), userID, keyID, modelID, p, c, costMicro, sid, at)
	e.exec(`INSERT INTO llm_usage_daily (day, user_id, model_id, requests, prompt_tokens, completion_tokens, cost_micro)
		VALUES ($1::date, $2, $3, 1, $4, $5, $6)
		ON CONFLICT (day, user_id, model_id) DO UPDATE SET requests = llm_usage_daily.requests + 1,
			prompt_tokens = llm_usage_daily.prompt_tokens + EXCLUDED.prompt_tokens,
			completion_tokens = llm_usage_daily.completion_tokens + EXCLUDED.completion_tokens,
			cost_micro = llm_usage_daily.cost_micro + EXCLUDED.cost_micro`,
		at.In(tz.CST).Format("2006-01-02"), userID, modelID, p, c, costMicro)
}

// usageFixture 北京时间 2026-01-15 12:00；已结算（daily）与未结算用量跨越日界与月界。
type usageFixture struct {
	m1, m2   llm.Model
	u        int64
	k1, k2   llm.Key
	now      time.Time
	todayCST time.Time
}

func newUsageFixture(e *env) usageFixture {
	f := usageFixture{now: time.Date(2026, 1, 15, 12, 0, 0, 0, tz.CST), todayCST: time.Date(2026, 1, 15, 0, 0, 0, 0, tz.CST)}
	f.m1 = e.createModel("qwen3-4b")
	f.m2 = e.createModel("qwen3-8b")
	f.u = e.verifiedUser()
	_, f.k1 = e.createKey(f.u)
	_, f.k2 = e.createKey(f.u)
	e.setClock(f.now)
	monthStart := time.Date(2026, 1, 1, 0, 0, 0, 0, tz.CST)
	// 已结算
	e.insertSettled(f.u, f.k1.ID, f.m1.ID, f.todayCST.Add(time.Hour), 100, 200, 2_500_000)      // 今天
	e.insertSettled(f.u, f.k1.ID, f.m1.ID, monthStart, 10, 20, 1_000_000)                       // 本月第一秒
	e.insertSettled(f.u, f.k1.ID, f.m1.ID, monthStart.Add(-time.Second), 1000, 1000, 9_000_000) // 上月最后一秒
	// 未结算
	rec := func(k llm.Key, m llm.Model, at time.Time, p, c int, cost int64) {
		x := e.usageFor(f.u, k.ID, m, p, c)
		x.CostMicro, x.CreatedAt = cost, at
		e.record(x)
	}
	rec(f.k1, f.m1, f.todayCST, 1, 2, 1_600_000)                   // 今天第一秒
	rec(f.k2, f.m2, f.now, 3, 4, 300_000)                          // 今天
	rec(f.k1, f.m1, f.todayCST.Add(-time.Second), 5, 6, 400_000)   // 昨天最后一秒
	rec(f.k2, f.m1, monthStart.Add(-time.Second), 7, 8, 5_000_000) // 上月：计入未结算，不计入本月
	return f
}

func TestAccountViewAggregates(t *testing.T) {
	e := newEnv(t)
	// 没有账户行：零值 + 默认限额；未实名 kyc_verified=false
	nu := e.unverifiedUser()
	v, err := e.svc.Account(e.ctx, nu)
	if err != nil {
		t.Fatal(err)
	}
	def := llm.Limits{RPM: llm.DefaultRPM, TPM: llm.DefaultTPM, Concurrency: llm.DefaultConcurrency}
	if v.Debt != 0 || v.Unsettled != 0 || v.TodayCost != 0 || v.MonthCost != 0 || v.MonthTokens != 0 || v.Limits != def ||
		v.Blocked || v.BlockedReason != "" || v.KYCVerified {
		t.Fatalf("empty view = %+v", v)
	}

	f := newUsageFixture(e)
	e.exec(`UPDATE llm_accounts SET debt = 37, rpm_limit = 120, blocked = true, blocked_reason = '涉嫌滥用' WHERE user_id = $1`, f.u)
	v, err = e.svc.Account(e.ctx, f.u)
	if err != nil {
		t.Fatal(err)
	}
	// 未结算 1.6M + 0.3M + 0.4M + 5M = 7.3M 微分 → ceil 8 分
	// 今天 2.5M（daily）+ 1.6M + 0.3M = 4.4M → floor 4 分
	// 本月 2.5M + 1.0M（daily）+ 1.6M + 0.3M + 0.4M = 5.8M → floor 5 分；上月的 9M / 5M 不计
	// 本月 token：daily 100+200 + 10+20，未结算 1+2 + 3+4 + 5+6 = 351
	if v.Debt != 37 || v.Unsettled != 8 || v.TodayCost != 4 || v.MonthCost != 5 || v.MonthTokens != 351 {
		t.Fatalf("view amounts = %+v", v)
	}
	if v.Limits != (llm.Limits{RPM: 120, TPM: llm.DefaultTPM, Concurrency: llm.DefaultConcurrency}) || !v.Blocked ||
		v.BlockedReason != "涉嫌滥用" || !v.KYCVerified {
		t.Fatalf("view = %+v", v)
	}
	b, _ := json.Marshal(v)
	for _, field := range []string{`"debt":37`, `"unsettled":8`, `"today_cost":4`, `"month_cost":5`, `"month_tokens":351`,
		`"limits":{"rpm":120,"tpm":200000,"concurrency":8}`, `"blocked":true`, `"blocked_reason":"涉嫌滥用"`, `"kyc_verified":true`} {
		if !strings.Contains(string(b), field) {
			t.Fatalf("json %s missing %s", b, field)
		}
	}
	// 未结算恰好整分：ceil 不进位
	e.exec(`UPDATE llm_usage SET cost_micro = 0 WHERE user_id = $1 AND settlement_id IS NULL`, f.u)
	e.exec(`UPDATE llm_usage SET cost_micro = 2000000 WHERE request_id = (SELECT min(request_id) FROM llm_usage WHERE user_id = $1 AND settlement_id IS NULL)`, f.u)
	e.exec(`UPDATE llm_accounts SET unsettled_micro = 2000000 WHERE user_id = $1`, f.u)
	if v, _ := e.svc.Account(e.ctx, f.u); v.Unsettled != 2 {
		t.Fatalf("exact unsettled = %d", v.Unsettled)
	}
}

func TestListUsageFiltersAndExport(t *testing.T) {
	e := newEnv(t)
	f := newUsageFixture(e)
	other := e.verifiedUser()
	ko := e.rawKey(other)
	failed := e.usageFor(other, ko, f.m2, 9, 9)
	failed.Status, failed.HTTPStatus = llm.UsageUpstreamError, 502
	e.record(failed)

	page := paging.Page{Number: 1, Size: 3}
	rows, total, err := e.svc.ListUsage(e.ctx, llm.UsageFilter{UserID: f.u}, page)
	if err != nil {
		t.Fatal(err)
	}
	if total != 7 || len(rows) != 3 {
		t.Fatalf("user list total=%d len=%d", total, len(rows))
	}
	// 新的在前；带模型 slug、Key 名称与手机号
	if !rows[0].CreatedAt.Equal(f.now) || rows[0].Model != "qwen3-8b" || rows[0].KeyName != f.k2.Name || rows[0].UserPhone == "" ||
		rows[0].KeyHint != f.k2.KeyHint {
		t.Fatalf("first row = %+v", rows[0])
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].CreatedAt.After(rows[i-1].CreatedAt) {
			t.Fatal("not newest first")
		}
	}
	// 第 3 页只剩 1 条
	if rows, _, _ := e.svc.ListUsage(e.ctx, llm.UsageFilter{UserID: f.u}, paging.Page{Number: 3, Size: 3}); len(rows) != 1 {
		t.Fatalf("page 3 len = %d", len(rows))
	}
	from, to := f.todayCST, f.todayCST.Add(24*time.Hour)
	count := func(fl llm.UsageFilter) int {
		t.Helper()
		_, n, err := e.svc.ListUsage(e.ctx, fl, paging.Page{})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	cases := []struct {
		name string
		f    llm.UsageFilter
		want int
	}{
		{"all users", llm.UsageFilter{}, 8},
		{"today window", llm.UsageFilter{UserID: f.u, From: &from, To: &to}, 3},
		{"to exclusive", llm.UsageFilter{UserID: f.u, To: &from}, 4},
		{"model id", llm.UsageFilter{UserID: f.u, ModelID: f.m2.ID}, 1},
		{"model slug", llm.UsageFilter{UserID: f.u, Model: "qwen3-4b"}, 6},
		{"key", llm.UsageFilter{UserID: f.u, KeyID: f.k2.ID}, 2},
		{"status", llm.UsageFilter{Status: llm.UsageUpstreamError}, 1},
		{"other user's key under my filter", llm.UsageFilter{UserID: f.u, KeyID: ko}, 0},
	}
	for _, c := range cases {
		if got := count(c.f); got != c.want {
			t.Errorf("%s: total = %d, want %d", c.name, got, c.want)
		}
	}
	if _, _, err := e.svc.ListUsage(e.ctx, llm.UsageFilter{Status: "weird"}, paging.Page{}); errCode(err) != "bad_request" {
		t.Fatalf("bad status err = %v", err)
	}

	// 导出：时间正序（id 正序）逐行回调
	var got []string
	if err := e.svc.ExportUsage(e.ctx, llm.UsageFilter{UserID: f.u, From: &from}, func(r llm.UsageRow) error {
		got = append(got, r.RequestID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] > got[1] || got[1] > got[2] {
		t.Fatalf("export = %v", got)
	}
	stop := errors.New("stop")
	if err := e.svc.ExportUsage(e.ctx, llm.UsageFilter{}, func(llm.UsageRow) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback err = %v", err)
	}
	// 超过 csvx.MaxRows：回调前返回 ErrTooLarge
	e.exec(`INSERT INTO llm_usage (request_id, user_id, key_id, model_id, endpoint, stream, input_price, output_price, cost_micro,
		status, http_status, created_at)
		SELECT 'req-bulk' || g, $1, $2, $3, 'chat', false, 0, 0, 0, 'ok', 200, $4 FROM generate_series(1, $5::int) g`,
		other, ko, f.m2.ID, e.clk.Now(), csvx.MaxRows)
	called := false
	if err := e.svc.ExportUsage(e.ctx, llm.UsageFilter{}, func(llm.UsageRow) error { called = true; return nil }); !errors.Is(err, csvx.ErrTooLarge) || called {
		t.Fatalf("too large err = %v called = %v", err, called)
	}

	// 时间窗口辅助：默认最近 7 天，超过 90 天 / 起止颠倒 → bad_request
	now := e.clk.Now()
	fr, tt, err := llm.UsageWindow(nil, nil, now)
	if err != nil || !tt.Equal(now) || !fr.Equal(now.Add(-7*24*time.Hour)) {
		t.Fatalf("default window = %v %v %v", fr, tt, err)
	}
	early := now.Add(-91 * 24 * time.Hour)
	if _, _, err := llm.UsageWindow(&early, nil, now); errCode(err) != "bad_request" {
		t.Fatalf("91 days err = %v", err)
	}
	ok90 := now.Add(-90 * 24 * time.Hour)
	if _, _, err := llm.UsageWindow(&ok90, nil, now); err != nil {
		t.Fatalf("90 days err = %v", err)
	}
	if _, _, err := llm.UsageWindow(&now, &early, now); errCode(err) != "bad_request" {
		t.Fatalf("reversed err = %v", err)
	}
	// 只给 to：from = to − 7 天
	if fr, tt, err := llm.UsageWindow(nil, &early, now); err != nil || !tt.Equal(early) || !fr.Equal(early.Add(-7*24*time.Hour)) {
		t.Fatalf("to-only window = %v %v %v", fr, tt, err)
	}
}

func TestDailyUsageMergesUnsettled(t *testing.T) {
	e := newEnv(t)
	f := newUsageFixture(e)
	rows, err := e.svc.DailyUsage(e.ctx, f.u, f.todayCST.Add(-24*time.Hour), f.now)
	if err != nil {
		t.Fatal(err)
	}
	type k struct {
		day   string
		model string
	}
	got := map[k]llm.DailyRow{}
	for _, r := range rows {
		got[k{r.Day.Format("2006-01-02"), r.ModelSlug}] = r
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	// 今天 m1：daily 1 条 2.5M + 未结算 1 条 1.6M
	if r := got[k{"2026-01-15", "qwen3-4b"}]; r.Requests != 2 || r.PromptTokens != 101 || r.CompletionTokens != 202 ||
		r.CostMicro != 4_100_000 || r.UserID != f.u || r.ModelID != f.m1.ID {
		t.Fatalf("today m1 = %+v", r)
	}
	if r := got[k{"2026-01-15", "qwen3-8b"}]; r.Requests != 1 || r.CostMicro != 300_000 {
		t.Fatalf("today m2 = %+v", r)
	}
	if r := got[k{"2026-01-14", "qwen3-4b"}]; r.Requests != 1 || r.CostMicro != 400_000 {
		t.Fatalf("yesterday = %+v", r)
	}
	// 日期升序
	if rows[0].Day.After(rows[len(rows)-1].Day) {
		t.Fatal("not ascending")
	}
	// userID 0 = 全部用户；跨月区间含上月最后一天（daily 9M + 未结算 5M 同一天同模型合并）
	all, err := e.svc.DailyUsage(e.ctx, 0, time.Date(2025, 12, 31, 0, 0, 0, 0, tz.CST), time.Date(2025, 12, 31, 23, 0, 0, 0, tz.CST))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Requests != 2 || all[0].CostMicro != 14_000_000 || all[0].Day.Format("2006-01-02") != "2025-12-31" {
		t.Fatalf("dec 31 = %+v", all)
	}
}

func TestAdminUpdateAccountLimits(t *testing.T) {
	e := newEnv(t)
	u := e.verifiedUser()
	target := fmt.Sprint(u)
	set := func(v int) llm.OptInt { return llm.OptInt{Set: true, Value: &v} }

	// 没有账户行：创建
	a, err := e.svc.AdminUpdateAccount(e.ctx, e.admin(), u, llm.AccountUpdate{RPMLimit: set(120)})
	if err != nil {
		t.Fatal(err)
	}
	if a.UserID != u || a.RPMLimit == nil || *a.RPMLimit != 120 || a.TPMLimit != nil || a.ConcurrencyLimit != nil || a.Blocked {
		t.Fatalf("account = %+v", a)
	}
	if e.auditCount("llm.account.update", target) != 1 {
		t.Fatal("audit missing")
	}
	diff := e.lastAuditDiff("llm.account.update", target)
	if diff["before"] == nil || diff["after"].(map[string]any)["rpm_limit"].(float64) != 120 {
		t.Fatalf("diff = %v", diff)
	}

	// 鉴权缓存立即失效：先缓存默认限额，改后马上生效
	plain, _ := e.createKey(u)
	ac, err := e.svc.Authorize(e.ctx, llm.HashKey(plain), e.clk.Now())
	if err != nil || ac.Limits.TPM != llm.DefaultTPM {
		t.Fatalf("authorize = %+v %v", ac, err)
	}
	if _, err := e.svc.AdminUpdateAccount(e.ctx, e.admin(), u, llm.AccountUpdate{TPMLimit: set(5000), ConcurrencyLimit: set(2)}); err != nil {
		t.Fatal(err)
	}
	ac, err = e.svc.Authorize(e.ctx, llm.HashKey(plain), e.clk.Now())
	if err != nil || ac.Limits != (llm.Limits{RPM: 120, TPM: 5000, Concurrency: 2}) {
		t.Fatalf("authorize after update = %+v %v", ac.Limits, err)
	}

	// null 恢复默认；未给的字段不变
	a, err = e.svc.AdminUpdateAccount(e.ctx, e.admin(), u, llm.AccountUpdate{RPMLimit: llm.OptInt{Set: true}})
	if err != nil {
		t.Fatal(err)
	}
	if a.RPMLimit != nil || a.TPMLimit == nil || *a.TPMLimit != 5000 || a.ConcurrencyLimit == nil || *a.ConcurrencyLimit != 2 {
		t.Fatalf("after null = %+v", a)
	}
	if a.Limits() != (llm.Limits{RPM: llm.DefaultRPM, TPM: 5000, Concurrency: 2}) {
		t.Fatalf("limits = %+v", a.Limits())
	}

	// JSON：出现的键才算 Set，null → 恢复默认
	var in llm.AccountUpdate
	if err := json.Unmarshal([]byte(`{"rpm_limit": null, "tpm_limit": 9000, "blocked": true, "blocked_reason": " 刷量 "}`), &in); err != nil {
		t.Fatal(err)
	}
	if !in.RPMLimit.Set || in.RPMLimit.Value != nil || !in.TPMLimit.Set || *in.TPMLimit.Value != 9000 || in.ConcurrencyLimit.Set ||
		in.Blocked == nil || !*in.Blocked || in.BlockedReason == nil {
		t.Fatalf("decoded = %+v", in)
	}
	if err := json.Unmarshal([]byte(`{"rpm_limit": "x"}`), &llm.AccountUpdate{}); err == nil {
		t.Fatal("string limit accepted")
	}

	// 校验
	yes, no := true, false
	empty, blank, long := "", "   ", strings.Repeat("长", 201)
	invalid := []struct {
		name  string
		in    llm.AccountUpdate
		field string
	}{
		{"rpm 0", llm.AccountUpdate{RPMLimit: set(0)}, "rpm_limit"},
		{"rpm too big", llm.AccountUpdate{RPMLimit: set(100_001)}, "rpm_limit"},
		{"tpm 0", llm.AccountUpdate{TPMLimit: set(0)}, "tpm_limit"},
		{"tpm too big", llm.AccountUpdate{TPMLimit: set(100_000_001)}, "tpm_limit"},
		{"concurrency 0", llm.AccountUpdate{ConcurrencyLimit: set(0)}, "concurrency_limit"},
		{"concurrency too big", llm.AccountUpdate{ConcurrencyLimit: set(513)}, "concurrency_limit"},
		{"blocked without reason", llm.AccountUpdate{Blocked: &yes}, "blocked_reason"},
		{"blocked empty reason", llm.AccountUpdate{Blocked: &yes, BlockedReason: &empty}, "blocked_reason"},
		{"blocked blank reason", llm.AccountUpdate{Blocked: &yes, BlockedReason: &blank}, "blocked_reason"},
		{"blocked long reason", llm.AccountUpdate{Blocked: &yes, BlockedReason: &long}, "blocked_reason"},
	}
	for _, c := range invalid {
		_, err := e.svc.AdminUpdateAccount(e.ctx, e.admin(), u, c.in)
		if errCode(err) != "llm_account_invalid" || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
	if e.auditCount("llm.account.update", target) != 3 {
		t.Fatalf("rejected updates audited: %d", e.auditCount("llm.account.update", target))
	}
	// 边界值可用
	if _, err := e.svc.AdminUpdateAccount(e.ctx, e.admin(), u, llm.AccountUpdate{RPMLimit: set(100_000), TPMLimit: set(100_000_000),
		ConcurrencyLimit: set(512)}); err != nil {
		t.Fatal(err)
	}

	// 停用：需原因（trim），网关立即 403；改原因不必重给 blocked；解除停用清空原因
	a, err = e.svc.AdminUpdateAccount(e.ctx, e.admin(), u, in)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Blocked || a.BlockedReason != "刷量" || *a.TPMLimit != 9000 || a.RPMLimit != nil {
		t.Fatalf("blocked = %+v", a)
	}
	if st, code := e.authCode(e.svc, plain); st != 403 || code != "account_blocked" {
		t.Fatalf("authorize blocked = %d %s", st, code)
	}
	reason := "批量刷量"
	if a, err = e.svc.AdminUpdateAccount(e.ctx, e.admin(), u, llm.AccountUpdate{BlockedReason: &reason}); err != nil || a.BlockedReason != reason {
		t.Fatalf("reason update = %+v %v", a, err)
	}
	if _, err := e.svc.AdminUpdateAccount(e.ctx, e.admin(), u, llm.AccountUpdate{BlockedReason: &blank}); errCode(err) != "llm_account_invalid" {
		t.Fatalf("blank reason while blocked err = %v", err)
	}
	a, err = e.svc.AdminUpdateAccount(e.ctx, e.admin(), u, llm.AccountUpdate{Blocked: &no, BlockedReason: &reason})
	if err != nil || a.Blocked || a.BlockedReason != "" {
		t.Fatalf("unblocked = %+v %v", a, err)
	}
	if st, _ := e.authCode(e.svc, plain); st != 0 {
		t.Fatalf("authorize after unblock = %d", st)
	}

	// 用户不存在
	if _, err := e.svc.AdminUpdateAccount(e.ctx, e.admin(), 999_999, llm.AccountUpdate{RPMLimit: set(10)}); errCode(err) != "not_found" {
		t.Fatalf("missing user err = %v", err)
	}
}

func TestAdminListAccounts(t *testing.T) {
	e := newEnv(t)
	f := newUsageFixture(e)
	e.exec(`UPDATE llm_accounts SET debt = 12 WHERE user_id = $1`, f.u)
	e.exec(`UPDATE users SET phone = '13800001111' WHERE id = $1`, f.u)
	ub := e.verifiedUser()
	e.exec(`UPDATE users SET phone = '13700002222' WHERE id = $1`, ub)
	yes := true
	reason := "测试"
	if _, err := e.svc.AdminUpdateAccount(e.ctx, e.admin(), ub, llm.AccountUpdate{Blocked: &yes, BlockedReason: &reason}); err != nil {
		t.Fatal(err)
	}
	rows, total, err := e.svc.AdminListAccounts(e.ctx, llm.AccountFilter{}, paging.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("total = %d rows = %+v", total, rows)
	}
	var ra llm.AccountRow
	for _, r := range rows {
		if r.UserID == f.u {
			ra = r
		}
	}
	// 本月 5.8M → 5 分；未结算 7.3M → 8 分；2 把 Key
	if ra.Phone == "" || ra.KeyCount != 2 || ra.MonthCost != 5 || ra.Unsettled != 8 || ra.Debt != 12 ||
		ra.Limits != (llm.Limits{RPM: llm.DefaultRPM, TPM: llm.DefaultTPM, Concurrency: llm.DefaultConcurrency}) || ra.Blocked {
		t.Fatalf("row = %+v", ra)
	}
	// 撤销的 Key 不计数
	e.exec(`UPDATE llm_api_keys SET status = 'revoked', revoked_at = $2 WHERE id = $1`, f.k2.ID, e.clk.Now())
	cnt := func(fl llm.AccountFilter) int {
		t.Helper()
		_, n, err := e.svc.AdminListAccounts(e.ctx, fl, paging.Page{})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	phone := ra.Phone
	cases := []struct {
		name string
		f    llm.AccountFilter
		want int
	}{
		{"debt", llm.AccountFilter{Debt: true}, 1},
		{"blocked", llm.AccountFilter{Blocked: &yes}, 1},
		{"phone", llm.AccountFilter{Q: phone}, 1},
		{"phone tail", llm.AccountFilter{Q: phone[7:]}, 1},
		{"phone middle", llm.AccountFilter{Q: "0000222"}, 1},
		{"no match", llm.AccountFilter{Q: "19900000000"}, 0},
	}
	for _, c := range cases {
		if got := cnt(c.f); got != c.want {
			t.Errorf("%s: total = %d, want %d", c.name, got, c.want)
		}
	}
	byID, _, _ := e.svc.AdminListAccounts(e.ctx, llm.AccountFilter{Q: fmt.Sprint(ub)}, paging.Page{})
	if !slices.ContainsFunc(byID, func(r llm.AccountRow) bool { return r.UserID == ub }) {
		t.Fatalf("by user id = %+v", byID)
	}
	rows, _, _ = e.svc.AdminListAccounts(e.ctx, llm.AccountFilter{Q: phone}, paging.Page{})
	if len(rows) != 1 || rows[0].KeyCount != 1 {
		t.Fatalf("after revoke = %+v", rows)
	}
	if _, _, err := e.svc.AdminListAccounts(e.ctx, llm.AccountFilter{Q: "abc"}, paging.Page{}); errCode(err) != "bad_request" {
		t.Fatalf("bad q err = %v", err)
	}
}
