package butler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/butler/butlertest"
	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/butler/llm/mock"
	"spark-cloud/internal/platform/paging"
)

func TestSweepExpiresPending(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, cf := e.pendingRenew()
	e.clk.Advance(9 * time.Minute)
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM butler_tool_calls WHERE id = $1 AND status = 'pending'`, cf.ID); n != 1 {
		t.Fatal("未到期就过期了")
	}
	e.clk.Advance(2 * time.Minute)
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	conv, msgs, pending, err := e.svc.GetConversation(ctx, e.owner, c.ID)
	if err != nil || conv.Status != ConvIdle || pending != nil {
		t.Fatalf("conv = %+v pending = %v err = %v", conv, pending, err)
	}
	last := msgs[len(msgs)-1]
	if last.Role != RoleTool || last.Content[0].ToolCallID != cf.ID || last.Content[0].OK ||
		!strings.Contains(last.Content[0].Content, `"code":"expired"`) || !strings.Contains(last.Content[0].Content, "操作已过期未执行") {
		t.Fatalf("last = %+v", last)
	}
	// 再扫一次不重复追加
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(e.msgs(c.ID)); n != len(msgs) {
		t.Fatalf("messages %d → %d", len(msgs), n)
	}
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestSweepStaleRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, _ := e.send("new", "你好")
	if _, err := e.svc.pool.Exec(ctx, `UPDATE butler_conversations SET status = 'running', run_token = 'tok', run_started_at = $2 WHERE id = $1`,
		c.ID, e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * time.Minute)
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if conv, _ := e.svc.getConv(ctx, e.svc.pool, e.owner, c.ID); conv.Status != ConvRunning {
		t.Fatal("2 分钟就被接管了")
	}
	e.clk.Advance(2 * time.Minute)
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	conv, _ := e.svc.getConv(ctx, e.svc.pool, e.owner, c.ID)
	if conv.Status != ConvIdle || conv.RunToken != "" {
		t.Fatalf("conv = %+v", conv)
	}
	msgs := e.msgs(c.ID)
	if last := msgs[len(msgs)-1]; last.Role != RoleAssistant || last.Content[0].Text != TextInterrupted {
		t.Fatalf("last = %+v", last)
	}
	// 被接管后可以继续发消息
	e.send(c.ID, "重试")
	butlertest.CheckInvariants(t, e.svc.pool)
}

func TestSweepRetentionAndUsage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	old, _ := e.send("new", "旧对话")
	if _, err := e.svc.pool.Exec(ctx, `UPDATE butler_conversations SET updated_at = $2 WHERE id = $1`, old.ID, e.clk.Now().AddDate(0, 0, -91)); err != nil {
		t.Fatal(err)
	}
	fresh, _ := e.send("new", "新对话")
	if _, err := e.svc.pool.Exec(ctx, `UPDATE butler_conversations SET updated_at = $2 WHERE id = $1`, fresh.ID, e.clk.Now().AddDate(0, 0, -89)); err != nil {
		t.Fatal(err)
	}
	oldDay := e.clk.Now().AddDate(0, 0, -401)
	if err := e.svc.AddUsage(ctx, e.svc.pool, e.owner, e.svc.DayOf(oldDay), Usage{Requests: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AddUsage(ctx, e.svc.pool, e.owner, e.svc.DayOf(e.clk.Now().AddDate(0, 0, -399)), Usage{Requests: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM butler_conversations WHERE id = $1`, old.ID); n != 0 {
		t.Fatal("90 天前的对话没删")
	}
	if n := e.count(`SELECT count(*) FROM butler_messages WHERE conversation_id = $1`, old.ID); n != 0 {
		t.Fatal("消息没级联删")
	}
	if n := e.count(`SELECT count(*) FROM butler_conversations WHERE id = $1`, fresh.ID); n != 1 {
		t.Fatal("89 天的对话被删了")
	}
	if n := e.count(`SELECT count(*) FROM butler_usage`); n != 2 { // 今天 + 399 天前
		t.Fatalf("usage rows = %d", n)
	}
}

func TestAdminQueries(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, _ = e.pendingRenew()
	e.clk.Advance(time.Minute)
	e.mock.Enqueue(mock.Reply{ToolCalls: []llm.ToolCall{call("get_wallet", `{}`)}})
	e.mock.Enqueue(mock.Reply{Text: "余额 ¥138.00"})
	c2, err := e.svc.Send(ctx, SendRequest{UserID: e.other, ConversationID: "new", Text: "余额"}, &sliceSink{})
	if err != nil {
		t.Fatal(err)
	}
	rows, total, err := e.svc.AdminListConversations(ctx, ConvFilter{}, paging.Page{})
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("rows = %d total = %d err = %v", len(rows), total, err)
	}
	if rows[0].ID != c2.ID || rows[0].Phone != "13800005678" || rows[0].Messages != 4 || rows[0].WriteCalls != 0 ||
		rows[1].WriteCalls != 1 || rows[1].Messages != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	rows, total, _ = e.svc.AdminListConversations(ctx, ConvFilter{UserID: e.owner}, paging.Page{})
	if total != 1 || rows[0].UserID != e.owner {
		t.Fatalf("by user = %+v", rows)
	}
	from := e.clk.Now().Add(time.Hour)
	if _, total, _ = e.svc.AdminListConversations(ctx, ConvFilter{From: &from}, paging.Page{}); total != 0 {
		t.Fatalf("from filter total = %d", total)
	}
	conv, msgs, calls, err := e.svc.AdminConversation(ctx, c2.ID)
	if err != nil || conv.ID != c2.ID || len(msgs) != 4 || len(calls) != 1 || calls[0].Kind != KindRead || calls[0].SecretEnvEnc != nil {
		t.Fatalf("admin conv = %+v %d %d err = %v", conv, len(msgs), len(calls), err)
	}
	if _, _, _, err := e.svc.AdminConversation(ctx, "cv-nope"); codeOf(err) != "not_found" {
		t.Fatalf("err = %v", err)
	}
	req, tok, fail, err := e.svc.Counts(ctx, e.clk.Now())
	if err != nil || req != 3 || tok != 3300 || fail != 0 {
		t.Fatalf("counts = %d %d %d %v", req, tok, fail, err)
	}
	// 视图
	views := MessageViews(msgs, calls, conv.Status)
	if views[1].Content[0].Type != "tool" || views[1].Content[0].Status != "ok" || views[1].Content[0].Label != "查询钱包" ||
		views[2].Content[0].Type != BlockToolResult || strings.Contains(views[2].Content[0].Summary, "<untrusted_data") {
		t.Fatalf("views = %+v", views)
	}
	av := AdminMessageViews(msgs, calls, conv.Status)
	if b := av[2].Content[0]; b.OK == nil || !*b.OK || b.Suspicious == nil || *b.Suspicious {
		t.Fatalf("admin views = %+v", av[2])
	}
	b, _ := json.Marshal(views[0])
	if !strings.Contains(string(b), `"content":[{"type":"text","text":"余额"}]`) {
		t.Fatalf("json = %s", b)
	}
}

func TestResultHelpers(t *testing.T) {
	wrapped := "<untrusted_data source=\"x\" user=\"1\">\n{\"ok\":false,\"code\":\"app_exists\",\"message\":\"已部署\"}\n</untrusted_data>"
	if StripWrapper(wrapped) != `{"ok":false,"code":"app_exists","message":"已部署"}` {
		t.Fatalf("strip = %q", StripWrapper(wrapped))
	}
	if c, ok := resultError(wrapped); !ok || c != "app_exists" {
		t.Fatalf("resultError = %q %v", c, ok)
	}
	if _, ok := resultError(`{"ok":true}`); ok {
		t.Fatal("ok result treated as error")
	}
	if got := string(resultJSON("not json")); got != `"not json"` {
		t.Fatalf("resultJSON = %s", got)
	}
	if CallErrorCode(json.RawMessage(`{"ok":false,"code":"x"}`)) != "x" || CallResultSummary(nil) != "" {
		t.Fatal("call helpers")
	}
	long := strings.Repeat("长", 2000)
	if s := ResultSummary(long); len(s) > SummaryBytes+4 || !strings.HasSuffix(s, "…") {
		t.Fatalf("summary len = %d", len(s))
	}
}
