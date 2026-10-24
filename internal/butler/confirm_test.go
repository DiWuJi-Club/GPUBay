package butler

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/butler/butlertest"
	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/butler/llm/mock"
)

// pendingRenew 让模型提议续费 ins-run 1 天，返回对话与确认卡。
func (e *env) pendingRenew() (Conversation, ConfirmView) {
	e.t.Helper()
	e.mock.Enqueue(mock.Reply{ToolCalls: []llm.ToolCall{call("renew_instance", `{"instance_id": "ins-run", "duration": "1d"}`)}})
	c, sink := e.send("new", "帮我续费一天")
	cf := sink.find(EvConfirm)
	if len(cf) != 1 || c.Status != ConvAwaitingConfirm {
		e.t.Fatalf("no confirm card: %v", sink.names())
	}
	return c, cf[0].(ConfirmEvent).ToolCall
}

func (e *env) confirm(convID, tcID string, approve bool) (Conversation, *sliceSink, error) {
	sink := &sliceSink{}
	c, err := e.svc.Confirm(context.Background(), e.owner, "1.2.3.4", convID, tcID, approve, sink)
	return c, sink, err
}

func TestConfirmApproveExecutes(t *testing.T) {
	e := newEnv(t)
	c, cf := e.pendingRenew()
	if !strings.Contains(cf.Summary, "续费实例 ins-run") || cf.QuoteAmount == nil || *cf.QuoteAmount != 6900 || cf.Danger {
		t.Fatalf("card = %+v", cf)
	}
	calls := e.mock.Calls()
	c2, sink, err := e.confirm(c.ID, cf.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	// start → tool(ok, id = 卡片 id) → 模型收尾（规则 1）→ done
	if got := sink.names(); !slices.Equal(got, []string{EvStart, EvTool, EvDelta, EvUsage, EvDone}) {
		t.Fatalf("events = %v", got)
	}
	ev := sink.find(EvTool)[0].(ToolEvent)
	if ev.ID != cf.ID || ev.Status != "ok" || ev.Name != "renew_instance" || ev.Label != "续费实例" {
		t.Fatalf("tool event = %+v", ev)
	}
	if e.mock.Calls() != calls+1 || c2.Status != ConvIdle || !strings.Contains(sink.text(), "续费成功") {
		t.Fatalf("calls = %d status = %s text = %q", e.mock.Calls(), c2.Status, sink.text())
	}
	if !slices.Equal(e.rental.renewCalls, []string{"butler:" + cf.ID}) {
		t.Fatalf("renew calls = %v", e.rental.renewCalls)
	}
	var status, ref string
	var result []byte
	var executed *time.Time
	if err := e.svc.pool.QueryRow(context.Background(), `SELECT status, result_ref, result, executed_at FROM butler_tool_calls WHERE id = $1`, cf.ID).
		Scan(&status, &ref, &result, &executed); err != nil {
		t.Fatal(err)
	}
	if status != CallExecuted || ref != "ins-run" || executed == nil || !strings.Contains(string(result), `"order_id": "ord-renew"`) && !strings.Contains(string(result), `"order_id":"ord-renew"`) {
		t.Fatalf("row = %s %s %s", status, ref, result)
	}
	// 审计一条
	var diff []byte
	var actorType string
	var actorID int64
	if err := e.svc.pool.QueryRow(context.Background(), `SELECT actor_type, actor_id, diff FROM audit_logs WHERE action = 'butler.tool_execute'
		AND target_type = 'butler_tool_call' AND target_id = $1`, cf.ID).Scan(&actorType, &actorID, &diff); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	_ = json.Unmarshal(diff, &d)
	if actorType != "user" || actorID != e.owner || d["tool"] != "renew_instance" || d["status"] != CallExecuted || d["result_ref"] != "ins-run" ||
		d["conversation_id"] != c.ID || d["error_code"] != "" {
		t.Fatalf("audit = %s %d %v", actorType, actorID, d)
	}
	// tool 消息落库
	msgs := e.msgs(c.ID)
	var found bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if m.Role == RoleTool && b.ToolCallID == cf.ID && b.OK {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("没有执行结果的 tool 消息")
	}
	if u, _ := e.svc.UsageToday(context.Background(), e.owner); u.ToolCalls != 1 {
		t.Fatalf("usage tool_calls = %d", u.ToolCalls)
	}
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestConfirmBusinessFailure(t *testing.T) {
	e := newEnv(t)
	e.rental.renewErr = errInsufficient
	c, cf := e.pendingRenew()
	_, sink, err := e.confirm(c.ID, cf.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	ev := sink.find(EvTool)[0].(ToolEvent)
	if ev.ID != cf.ID || ev.Status != "error" || ev.Message != "余额不足，请先充值" {
		t.Fatalf("tool event = %+v", ev)
	}
	if n := e.count(`SELECT count(*) FROM butler_tool_calls WHERE id = $1 AND status = 'failed' AND result->>'code' = 'insufficient_balance'`, cf.ID); n != 1 {
		t.Fatal("没有记 failed")
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE target_id = $1 AND diff->>'error_code' = 'insufficient_balance' AND diff->>'status' = 'failed'`, cf.ID); n != 1 {
		t.Fatal("失败也要审计")
	}
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestConfirmReject(t *testing.T) {
	e := newEnv(t)
	c, cf := e.pendingRenew()
	c2, sink, err := e.confirm(c.ID, cf.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.find(EvTool)) != 0 || !strings.Contains(sink.text(), "用户拒绝了这次操作") || c2.Status != ConvIdle {
		t.Fatalf("events = %v text = %q", sink.names(), sink.text())
	}
	if e.rental.renewN() != 0 || e.count(`SELECT count(*) FROM audit_logs WHERE action = 'butler.tool_execute'`) != 0 {
		t.Fatal("拒绝后执行了")
	}
	if n := e.count(`SELECT count(*) FROM butler_tool_calls WHERE id = $1 AND status = 'rejected' AND decided_at IS NOT NULL`, cf.ID); n != 1 {
		t.Fatal("没有标 rejected")
	}
	msgs := e.msgs(c.ID)
	var content string
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == BlockToolResult && b.ToolCallID == cf.ID {
				content = b.Content
			}
		}
	}
	var r struct {
		OK      bool   `json:"ok"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(content), &r) != nil || r.OK || r.Code != "rejected" || r.Message != "用户拒绝了该操作" {
		t.Fatalf("rejected result = %s", content)
	}
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestConfirmIdempotent(t *testing.T) {
	e := newEnv(t)
	c, cf := e.pendingRenew()
	if _, _, err := e.confirm(c.ID, cf.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.confirm(c.ID, cf.ID, true); codeOf(err) != "butler_confirm_invalid" {
		t.Fatalf("second approve err = %v", err)
	}
	if _, _, err := e.confirm(c.ID, cf.ID, false); codeOf(err) != "butler_confirm_invalid" {
		t.Fatalf("reject after approve err = %v", err)
	}
	if e.rental.renewN() != 1 || e.count(`SELECT count(*) FROM audit_logs WHERE action = 'butler.tool_execute'`) != 1 {
		t.Fatalf("renew calls = %d", e.rental.renewN())
	}
	// 不属于该对话的 tc → invalid
	if _, _, err := e.confirm(c.ID, "tc-nope", true); codeOf(err) != "butler_confirm_invalid" {
		t.Fatalf("unknown tc err = %v", err)
	}
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestConfirmExpired(t *testing.T) {
	e := newEnv(t)
	c, cf := e.pendingRenew()
	e.clk.Advance(11 * time.Minute)
	_, sink, err := e.confirm(c.ID, cf.ID, true)
	if codeOf(err) != "butler_confirm_invalid" || len(sink.events) != 0 {
		t.Fatalf("err = %v events = %v", err, sink.names())
	}
	if e.rental.renewN() != 0 {
		t.Fatal("过期后执行了")
	}
	var status, cstatus string
	_ = e.svc.pool.QueryRow(context.Background(), `SELECT t.status, c.status FROM butler_tool_calls t JOIN butler_conversations c ON c.id = t.conversation_id
		WHERE t.id = $1`, cf.ID).Scan(&status, &cstatus)
	if status != CallExpired || cstatus != ConvIdle {
		t.Fatalf("status = %s conv = %s", status, cstatus)
	}
	msgs := e.msgs(c.ID)
	last := msgs[len(msgs)-1]
	if last.Role != RoleTool || !strings.Contains(last.Content[0].Content, `"code":"expired"`) || !strings.Contains(last.Content[0].Content, "操作已过期未执行") {
		t.Fatalf("last = %+v", last)
	}
	// 对话仍可继续
	e.send(c.ID, "好吧")
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestNewMessageExpiresPending(t *testing.T) {
	e := newEnv(t)
	c, cf := e.pendingRenew()
	e.mock.Enqueue(mock.Reply{Text: "好的，那就不续了。"})
	c2, _ := e.send(c.ID, "算了不续了")
	if c2.Status != ConvIdle {
		t.Fatalf("status = %s", c2.Status)
	}
	if n := e.count(`SELECT count(*) FROM butler_tool_calls WHERE id = $1 AND status = 'expired' AND secret_env_enc IS NULL`, cf.ID); n != 1 {
		t.Fatal("pending 没有作废")
	}
	// 回填「用户未确认」在新 user 消息之前，模型能看到
	req, _ := e.mock.LastRequest()
	var saw bool
	for i, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Type == BlockToolResult && b.ToolCallID == cf.ID {
				saw = strings.Contains(b.Content, `"code":"expired"`) && strings.Contains(b.Content, "用户未确认，操作未执行") &&
					req.Messages[i+1].Role == RoleUser
			}
		}
	}
	if !saw {
		t.Fatalf("history = %+v", req.Messages)
	}
	if _, _, err := e.confirm(c.ID, cf.ID, true); codeOf(err) != "butler_confirm_invalid" {
		t.Fatalf("confirm after new message err = %v", err)
	}
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestConfirmOtherUser(t *testing.T) {
	e := newEnv(t)
	c, cf := e.pendingRenew()
	_, err := e.svc.Confirm(context.Background(), e.other, "", c.ID, cf.ID, true, &sliceSink{})
	if codeOf(err) != "not_found" {
		t.Fatalf("err = %v", err)
	}
	if e.rental.renewN() != 0 {
		t.Fatal("他人确认执行了")
	}
}

func TestSecretEnvLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// mock 规则：部署 jupyter → list_instances → get_app → generate_secret → deploy_app（同一轮，用生成的密钥）
	c, sink := e.send("new", "在我的实例上部署 jupyter")
	cfs := sink.find(EvConfirm)
	if len(cfs) != 1 {
		t.Fatalf("events = %v", sink.names())
	}
	cf := cfs[0].(ConfirmEvent).ToolCall
	if cf.Tool != "deploy_app" {
		t.Fatalf("card = %+v", cf)
	}
	var args struct {
		Env map[string]string `json:"env"`
	}
	_ = json.Unmarshal(cf.Args, &args)
	if args.Env["TOKEN"] != "***" {
		t.Fatalf("card args = %s", cf.Args)
	}
	// 卡片 summary 显示真实密钥；summary 列存 ***
	var stored string
	var enc []byte
	_ = e.svc.pool.QueryRow(ctx, `SELECT summary, secret_env_enc FROM butler_tool_calls WHERE id = $1`, cf.ID).Scan(&stored, &enc)
	if len(enc) == 0 || !strings.Contains(stored, "***") {
		t.Fatalf("stored summary = %q enc = %d", stored, len(enc))
	}
	secret := ""
	for _, line := range strings.Split(cf.Summary, "\n") {
		if strings.Contains(line, "TOKEN") || strings.Contains(line, "访问令牌") {
			secret = line
		}
	}
	if secret == "" || strings.Contains(secret, "***") {
		t.Fatalf("confirm summary = %q", cf.Summary)
	}
	_, _, pending, err := e.svc.GetConversation(ctx, e.owner, c.ID)
	if err != nil || pending == nil || pending.Summary != cf.Summary || pending.SecretEnvEnc != nil {
		t.Fatalf("pending = %+v err = %v", pending, err)
	}
	if v := e.svc.ConfirmViewOf(*pending); v.ExpiresIn != int64(ConfirmTTL/time.Second) || v.Danger {
		t.Fatalf("view = %+v", v)
	}
	// 落库的消息里没有明文
	var real string
	if e.apps.created != nil {
		t.Fatal("确认前就部署了")
	}
	if _, _, err := e.confirm(c.ID, cf.ID, true); err != nil {
		t.Fatal(err)
	}
	if len(e.apps.created) != 1 {
		t.Fatalf("created = %d", len(e.apps.created))
	}
	real = e.apps.created[0].Env["TOKEN"]
	if len(real) != 32 || real == "***" || !strings.Contains(cf.Summary, real) {
		t.Fatalf("Create env TOKEN = %q", real)
	}
	if n := e.count(`SELECT count(*) FROM butler_tool_calls WHERE id = $1 AND secret_env_enc IS NULL AND status = 'executed'`, cf.ID); n != 1 {
		t.Fatal("执行后 secret_env_enc 没有清空")
	}
	if n := e.count(`SELECT count(*) FROM butler_messages WHERE content::text LIKE '%' || $1 || '%'`, real); n != 0 {
		t.Fatal("密钥明文落库了（消息）")
	}
	if n := e.count(`SELECT count(*) FROM butler_tool_calls WHERE args::text LIKE '%' || $1 || '%' OR summary LIKE '%' || $1 || '%' OR coalesce(result::text, '') LIKE '%' || $1 || '%'`, real); n != 0 {
		t.Fatal("密钥明文落库了（工具调用）")
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE diff::text LIKE '%' || $1 || '%'`, real); n != 0 {
		t.Fatal("密钥明文进了审计")
	}
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestDetailStatuses(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	stat := func(convID, tcID string) (string, *ConfirmView) {
		d, err := e.svc.Detail(ctx, e.owner, convID)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range d.Messages {
			for _, b := range m.Content {
				if b.Type == "tool" && b.ID == tcID {
					return b.Status, d.PendingToolCall
				}
			}
		}
		t.Fatalf("no tool block %s", tcID)
		return "", nil
	}
	c, cf := e.pendingRenew()
	if st, p := stat(c.ID, cf.ID); st != "pending" || p == nil || p.ID != cf.ID || p.ExpiresIn != 600 {
		t.Fatalf("pending: %s %+v", st, p)
	}
	e.clk.Advance(30 * time.Second)
	if _, p := stat(c.ID, cf.ID); p.ExpiresIn != 570 {
		t.Fatalf("expires_in = %d", p.ExpiresIn)
	}
	if _, _, err := e.confirm(c.ID, cf.ID, false); err != nil {
		t.Fatal(err)
	}
	if st, p := stat(c.ID, cf.ID); st != "rejected" || p != nil {
		t.Fatalf("rejected: %s %+v", st, p)
	}
	c2, cf2 := e.pendingRenew()
	if _, _, err := e.confirm(c2.ID, cf2.ID, true); err != nil {
		t.Fatal(err)
	}
	if st, _ := stat(c2.ID, cf2.ID); st != "ok" {
		t.Fatalf("executed: %s", st)
	}
	c3, cf3 := e.pendingRenew()
	e.mock.Enqueue(mock.Reply{Text: "好"})
	e.send(c3.ID, "不要了")
	if st, _ := stat(c3.ID, cf3.ID); st != "expired" {
		t.Fatalf("expired: %s", st)
	}
	d, _ := e.svc.Detail(ctx, e.owner, c3.ID)
	b, _ := json.Marshal(d)
	for _, k := range []string{`"conversation":{"id":"` + c3.ID + `"`, `"pending_tool_call":null`, `"context":{}`, `"messages":[{"id":`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("detail json missing %s: %s", k, b)
		}
	}
	if _, err := e.svc.Detail(ctx, e.other, c3.ID); codeOf(err) != "not_found" {
		t.Fatalf("other detail err = %v", err)
	}
}
