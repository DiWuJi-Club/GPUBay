package butler

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/butler/butlertest"
	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/butler/llm/mock"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/randx"
)

func TestStatusAndProviderLabel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	st, err := e.svc.Status(ctx, e.owner)
	if err != nil || !st.Enabled || st.DayUsed != 0 || st.DayLimit != DefaultDailyTokenLimit || st.ProviderLabel != "平台模型" || st.Notice != "" {
		t.Fatalf("status = %+v err = %v", st, err)
	}
	e.svc.d.Provider = &funcProvider{name: "anthropic"}
	if e.svc.ProviderLabel() != "Claude" {
		t.Fatal("anthropic label")
	}
	e.svc.d.Provider = &funcProvider{name: "openai"}
	if e.svc.ProviderLabel() != "平台模型" {
		t.Fatal("openai label")
	}
}

func TestListLimitAndDeleteRunning(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		e.send("new", "对话")
		e.clk.Advance(time.Second)
	}
	if l, _ := e.svc.ListConversations(ctx, e.owner, 2); len(l) != 2 {
		t.Fatalf("limit 2 = %d", len(l))
	}
	if l, _ := e.svc.ListConversations(ctx, e.owner, 999); len(l) != 3 {
		t.Fatalf("limit 999 = %d", len(l))
	}
	c, _ := e.send("new", "运行中")
	if _, err := e.svc.pool.Exec(ctx, `UPDATE butler_conversations SET status = 'running', run_token = 'x', run_started_at = $2 WHERE id = $1`, c.ID, e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteConversation(ctx, e.owner, c.ID); codeOf(err) != "butler_busy" {
		t.Fatalf("delete running err = %v", err)
	}
}

func TestConfirmPrecheckNoSlot(t *testing.T) {
	e := newEnv(t)
	c, cf := e.pendingRenew()
	if _, _, err := e.confirm(c.ID, cf.ID, false); err != nil {
		t.Fatal(err)
	}
	// 用户名额被占时，已失效的确认仍直接返回 confirm_invalid（不报 busy）
	if err := e.svc.acquire(e.owner); err != nil {
		t.Fatal(err)
	}
	defer e.svc.release(e.owner)
	if _, _, err := e.confirm(c.ID, cf.ID, true); codeOf(err) != "butler_confirm_invalid" {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := e.confirm("cv-nope", cf.ID, true); codeOf(err) != "not_found" {
		t.Fatalf("missing conv err = %v", err)
	}
}

func TestStoredTextMasksSecrets(t *testing.T) {
	e := newEnv(t)
	var secret string
	e.svc.d.Provider = &funcProvider{fn: func(n int, req llm.Request, emit func(llm.Event)) error {
		if n == 1 {
			emitCalls(emit, call("generate_secret", `{}`))
			return nil
		}
		last := req.Messages[len(req.Messages)-1]
		secret = secretValue(last.Blocks[0].Content)
		emitText(emit, "密钥是 "+secret+"，请记下。")
		return nil
	}}
	c, sink := e.send("new", "生成密钥")
	if len(secret) != 32 || !strings.Contains(sink.text(), secret) {
		t.Fatalf("delta 应含明文: %q", sink.text())
	}
	msgs := e.msgs(c.ID)
	if got := msgs[len(msgs)-1].Content[0].Text; got != "密钥是 ***，请记下。" {
		t.Fatalf("stored = %q", got)
	}
	if n := e.count(`SELECT count(*) FROM butler_messages WHERE content::text LIKE '%' || $1 || '%'`, secret); n != 0 {
		t.Fatal("明文落库")
	}
}

func TestContinuationMasksCardSecrets(t *testing.T) {
	e := newEnv(t)
	c, sink := e.send("new", "在我的实例上部署 jupyter") // mock 规则：生成密钥 → deploy_app 确认卡
	cf := sink.find(EvConfirm)[0].(ConfirmEvent).ToolCall
	e.svc.d.Provider = &funcProvider{fn: func(n int, req llm.Request, emit func(llm.Event)) error {
		e.apps.mu.Lock()
		tok := e.apps.created[0].Env["TOKEN"]
		e.apps.mu.Unlock()
		emitText(emit, "部署已提交，令牌 "+tok)
		return nil
	}}
	_, sink2, err := e.confirm(c.ID, cf.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	tok := e.apps.created[0].Env["TOKEN"]
	if !strings.Contains(sink2.text(), tok) {
		t.Fatalf("delta = %q", sink2.text())
	}
	msgs := e.msgs(c.ID)
	if got := msgs[len(msgs)-1].Content[0].Text; got != "部署已提交，令牌 ***" {
		t.Fatalf("stored = %q", got)
	}
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestToolsDisabled(t *testing.T) {
	e := newEnv(t)
	e.svc.d.ToolsEnabled = false
	e.mock.Enqueue(mock.Reply{ToolCalls: []llm.ToolCall{call("list_instances", `{}`)}})
	e.mock.Enqueue(mock.Reply{Text: "只能聊天"})
	c, _ := e.send("new", "我的实例")
	req, _ := e.mock.LastRequest()
	if len(req.Tools) != 0 {
		t.Fatalf("tools = %d", len(req.Tools))
	}
	msgs := e.msgs(c.ID)
	if !strings.Contains(msgs[2].Content[0].Content, `"code":"unknown_tool"`) {
		t.Fatalf("result = %s", msgs[2].Content[0].Content)
	}
	if n := e.count(`SELECT count(*) FROM butler_tool_calls`); n != 0 {
		t.Fatalf("rows = %d", n)
	}
}

func TestRunTimeout(t *testing.T) {
	e := newEnv(t)
	e.svc.runTimeout = 40 * time.Millisecond
	e.mock.Enqueue(mock.Reply{Text: strings.Repeat("很长的回答", 20), ChunkDelay: 20 * time.Millisecond})
	c, sink := e.send("new", "慢慢说")
	if d := sink.find(EvDone); len(d) != 1 || !strings.HasSuffix(sink.text(), TextRunTimeout) {
		t.Fatalf("events = %v text = %q", sink.names(), sink.text())
	}
	msgs := e.msgs(c.ID)
	if got := msgs[len(msgs)-1].Content[0].Text; !strings.HasSuffix(got, TextRunTimeout) || c.Status != ConvIdle {
		t.Fatalf("stored = %q status = %s", got, c.Status)
	}
	if u, _ := e.svc.UsageToday(context.Background(), e.owner); u.Failures != 0 {
		t.Fatalf("整轮超时不算模型失败: %+v", u)
	}
	if e.svc.failStreak != 0 {
		t.Fatal("streak")
	}
}

func TestRunPanicRecovered(t *testing.T) {
	e := newEnv(t)
	e.svc.d.Provider = &funcProvider{fn: func(int, llm.Request, func(llm.Event)) error { panic("boom") }}
	c, sink := e.send("new", "你好")
	if ev := sink.find(EvError); len(ev) != 1 || ev[0].(ErrorEvent).Code != "internal" || c.Status != ConvIdle {
		t.Fatalf("events = %v status = %s", sink.names(), c.Status)
	}
	if !strings.Contains(e.logs.String(), "panic: boom") {
		t.Fatal("panic 没有记日志")
	}
}

func TestStaleDuringTools(t *testing.T) {
	e := newEnv(t)
	c, _ := e.send("new", "第一条")
	before := len(e.msgs(c.ID))
	e.hook.before = func() {
		if e.mock.Queued() == 1 { // 只在发读工具的这次调用前接管
			_, _ = e.svc.pool.Exec(context.Background(), `UPDATE butler_conversations SET run_token = 'x', status = 'idle' WHERE id = $1`, c.ID)
		}
	}
	e.mock.Enqueue(mock.Reply{ToolCalls: []llm.ToolCall{call("get_wallet", `{}`)}})
	_, sink := e.send(c.ID, "查余额")
	e.hook.before = nil
	if n := len(e.msgs(c.ID)); n != before+1 {
		t.Fatalf("messages = %d (before %d)", n, before)
	}
	if len(sink.find(EvTool)) != 0 || e.count(`SELECT count(*) FROM butler_tool_calls`) != 0 {
		t.Fatalf("events = %v", sink.names())
	}
	if u, _ := e.svc.UsageToday(context.Background(), e.owner); u.ToolCalls != 1 {
		t.Fatalf("读工具执行了就计数: %+v", u)
	}
	if strings.Count(e.logs.String(), "butler: stale run discarded") != 1 {
		t.Fatal("stale log")
	}
}

func TestStaleDuringExecute(t *testing.T) {
	e := newEnv(t)
	c, cf := e.pendingRenew()
	e.rental.onRenew = func() { // 执行期间被清扫器接管
		_, _ = e.svc.pool.Exec(context.Background(), `UPDATE butler_conversations SET run_token = '', status = 'idle' WHERE id = $1`, c.ID)
	}
	calls := e.mock.Calls()
	_, sink, err := e.confirm(c.ID, cf.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if e.mock.Calls() != calls || len(sink.find(EvTool)) != 0 {
		t.Fatalf("接管后不应续跑: calls %d→%d events %v", calls, e.mock.Calls(), sink.names())
	}
	if n := e.count(`SELECT count(*) FROM butler_tool_calls WHERE id = $1 AND status = 'executed' AND secret_env_enc IS NULL`, cf.ID); n != 1 {
		t.Fatal("调用行没有记执行结果")
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE target_id = $1`, cf.ID); n != 1 {
		t.Fatal("没有审计")
	}
	var found bool
	for _, m := range e.msgs(c.ID) {
		for _, b := range m.Content {
			found = found || (b.Type == BlockToolResult && b.ToolCallID == cf.ID && b.OK)
		}
	}
	if !found {
		t.Fatal("事实结果的 tool 消息没有落库")
	}
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestSweepInterruptsApproved(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, cf := e.pendingRenew()
	// 模拟：用户确认后副本在执行中崩溃
	if _, err := e.svc.pool.Exec(ctx, `UPDATE butler_tool_calls SET status = 'approved', decided_at = $2, secret_env_enc = '\x01' WHERE id = $1`, cf.ID, e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.pool.Exec(ctx, `UPDATE butler_conversations SET status = 'running', run_token = 'tok', run_started_at = $2 WHERE id = $1`, c.ID, e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	butlertest.CheckInvariants(t, e.svc.pool) // running 中的 approved 合法
	e.clk.Advance(4 * time.Minute)
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM butler_tool_calls WHERE id = $1 AND status = 'failed' AND secret_env_enc IS NULL
		AND result->>'code' = 'interrupted'`, cf.ID); n != 1 {
		t.Fatal("approved 调用没有标 interrupted")
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE target_id = $1 AND diff->>'status' = 'unknown'`, cf.ID); n != 1 {
		t.Fatal("没有 unknown 审计")
	}
	msgs := e.msgs(c.ID)
	if msgs[len(msgs)-2].Content[0].ToolCallID != cf.ID || msgs[len(msgs)-1].Content[0].Text != TextInterrupted {
		t.Fatalf("messages = %+v", msgs[len(msgs)-2:])
	}
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestConfirmRacesSweeperExpiry(t *testing.T) {
	for i := 0; i < 3; i++ {
		e := newEnv(t)
		c, cf := e.pendingRenew()
		e.clk.Advance(11 * time.Minute)
		var wg sync.WaitGroup
		var cerr, serr error
		wg.Add(2)
		go func() { defer wg.Done(); _, _, cerr = e.confirm(c.ID, cf.ID, true) }()
		go func() { defer wg.Done(); serr = e.svc.Sweep(context.Background()) }()
		wg.Wait()
		if codeOf(cerr) != "butler_confirm_invalid" || serr != nil {
			t.Fatalf("confirm err = %v sweep err = %v", cerr, serr)
		}
		if e.rental.renewN() != 0 {
			t.Fatal("过期后执行了")
		}
		n := 0
		for _, m := range e.msgs(c.ID) {
			for _, b := range m.Content {
				if b.Type == BlockToolResult && b.ToolCallID == cf.ID {
					n++
				}
			}
		}
		if n != 1 {
			t.Fatalf("expired results = %d", n)
		}
		butlertest.CheckInvariants(t, e.svc.pool)
	}
}

func TestSweepRetentionBatchKeepsRunning(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	old := e.clk.Now().AddDate(0, 0, -100)
	for i := 0; i < SweepDeleteBatch+5; i++ {
		if _, err := e.svc.pool.Exec(ctx, `INSERT INTO butler_conversations (id, user_id, provider, model, created_at, updated_at)
			VALUES ($1, $2, 'mock', 'mock', $3, $3)`, randx.ID("cv-", 12), e.owner, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.pool.Exec(ctx, `INSERT INTO butler_conversations (id, user_id, status, run_token, run_started_at, provider, model, created_at, updated_at)
		VALUES ('cv-running', $1, 'running', 'tok', $2, 'mock', 'mock', $3, $3)`, e.owner, e.clk.Now(), old); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM butler_conversations`); n != 6 { // 205 − 200 + running
		t.Fatalf("after first sweep = %d", n)
	}
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM butler_conversations`); n != 1 || e.count(`SELECT count(*) FROM butler_conversations WHERE id = 'cv-running'`) != 1 {
		t.Fatalf("after second sweep = %d", n)
	}
}

func TestDetailApprovedRunning(t *testing.T) {
	msgs := []Message{useMsg(1, "tc-a"), useMsg(2, "tc-r")}
	calls := []ToolCall{{ID: "tc-a", Kind: KindWrite, Status: CallApproved}}
	v := MessageViews(msgs, calls, ConvRunning)
	if v[0].Content[0].Status != "running" || v[1].Content[0].Status != "running" {
		t.Fatalf("views = %+v", v)
	}
	v = MessageViews(msgs, nil, ConvIdle)
	if v[1].Content[0].Status != "error" {
		t.Fatalf("read without result idle = %s", v[1].Content[0].Status)
	}
	b, _ := json.Marshal(v[0])
	if !strings.Contains(string(b), `"type":"tool"`) {
		t.Fatal(string(b))
	}
}

// Shutdown：运行中的轮次按整轮超时同一路径收尾（落 TextShutdown、idle、不计模型失败），Shutdown 等它结束才返回；之后不接新轮次。
func TestShutdownInterruptsRunsAndRejectsNew(t *testing.T) {
	e := newEnv(t)
	e.mock.Enqueue(mock.Reply{Text: strings.Repeat("很长的回答", 40), ChunkDelay: 20 * time.Millisecond})
	sink := &sliceSink{}
	type res struct {
		c   Conversation
		err error
	}
	done := make(chan res, 1)
	go func() {
		c, err := e.svc.Send(context.Background(), SendRequest{UserID: e.owner, IP: "1.2.3.4", ConversationID: "new", Text: "慢慢说"}, sink)
		done <- res{c, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(sink.find(EvDelta)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no delta before shutdown")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.svc.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	var r res
	select {
	case r = <-done: // Shutdown 返回时轮次必须已经结束
	default:
		t.Fatal("Shutdown returned before the run finished")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.HasSuffix(sink.text(), TextShutdown) || len(sink.find(EvDone)) != 1 {
		t.Fatalf("events = %v text = %q", sink.names(), sink.text())
	}
	msgs := e.msgs(r.c.ID)
	if got := msgs[len(msgs)-1].Content[0].Text; !strings.HasSuffix(got, TextShutdown) || r.c.Status != ConvIdle {
		t.Fatalf("stored = %q status = %s", got, r.c.Status)
	}
	if u, _ := e.svc.UsageToday(context.Background(), e.owner); u.Failures != 0 || e.svc.failStreak != 0 {
		t.Fatalf("停机不算模型失败: %+v streak %d", u, e.svc.failStreak)
	}
	// 停机后不接新轮次；再次 Shutdown 立即返回
	_, err := e.svc.Send(context.Background(), SendRequest{UserID: e.owner, ConversationID: "new", Text: "还在吗"}, NopSink{})
	if ae := apperr.As(err); ae == nil || ae.Code != apperr.ErrButlerBusy.Code {
		t.Fatalf("send after shutdown = %v, want butler_busy", err)
	}
	if err := e.svc.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// Shutdown 的 ctx 到期而轮次还没收尾（例如卡在不响应 ctx 的调用里）→ 返回 ctx.Err()，不无限等待。
func TestShutdownHonorsContext(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.acquire(e.owner); err != nil { // 模拟一个永远不结束的轮次
		t.Fatal(err)
	}
	defer e.svc.release(e.owner)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := e.svc.Shutdown(ctx); err != context.DeadlineExceeded {
		t.Fatalf("Shutdown = %v, want DeadlineExceeded", err)
	}
}
