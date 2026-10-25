package e2ehooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/butler"
	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/tz"
)

// chat 直接调一次 mock（即 Butler 下一轮会拿到的回复）：返回拼起来的文本、工具调用、用量与错误。
func chat(t *testing.T, e *env) (string, []llm.ToolCall, *llm.Usage, error) {
	t.Helper()
	var text strings.Builder
	var calls []llm.ToolCall
	var usage *llm.Usage
	err := e.llm.Chat(context.Background(), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser,
		Blocks: []llm.Block{{Type: llm.BlockText, Text: "随便聊聊"}}}}}, func(ev llm.Event) {
		switch ev.Type {
		case llm.EvText:
			text.WriteString(ev.Text)
		case llm.EvTool:
			calls = append(calls, *ev.ToolCall)
		case llm.EvUsage:
			usage = ev.Usage
		}
	})
	return text.String(), calls, usage, err
}

// 管家未启用（cfg.Butler = false、没注入 mock）时四个钩子都是 400，而不是 nil 指针 500。
func TestButlerHooksRequireButler(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/hooks/butler/reply", "/hooks/butler/reset", "/hooks/butler/usage", "/hooks/butler/settings"} {
		if code, out := e.call("POST", path, map[string]any{"text": "x"}); code != 400 || !strings.Contains(out["error"].(string), "未启用") {
			t.Fatalf("%s = %d %v, want 400", path, code, out)
		}
	}
}

func TestButlerReplyHook(t *testing.T) {
	e := newButlerEnv(t)
	if e.a.Butler == nil || e.a.LLM != e.llm {
		t.Fatalf("Butler %v, LLM %v (want the injected mock %p)", e.a.Butler, e.a.LLM, e.llm)
	}
	for name, body := range map[string]any{
		"empty":            map[string]any{},
		"error with text":  map[string]any{"error": true, "text": "x"},
		"error with usage": map[string]any{"error": true, "usage": map[string]int{"input_tokens": 1}},
		"empty tool name":  map[string]any{"tool_calls": []map[string]any{{"name": " "}}},
		"args not object":  map[string]any{"tool_calls": []map[string]any{{"name": "get_wallet", "args": []int{1}}}},
		"negative usage":   map[string]any{"text": "a", "usage": map[string]int{"input_tokens": -1}},
		"negative delay":   map[string]any{"text": "a", "chunk_delay_ms": -1},
		"huge delay":       map[string]any{"text": "a", "chunk_delay_ms": 60000},
		"total delay":      map[string]any{"text": strings.Repeat("长", 8*31), "chunk_delay_ms": 1000}, // 31 段 × 1 秒 > 30 秒
		"unknown field":    map[string]any{"txt": "typo"},
		"unknown in tc":    map[string]any{"tool_calls": []map[string]any{{"name": "get_wallet", "arguments": map[string]any{}}}},
	} {
		if code, out := e.call("POST", "/hooks/butler/reply", body); code != 400 {
			t.Fatalf("%s: %d %v, want 400", name, code, out)
		}
	}
	if e.llm.Queued() != 0 {
		t.Fatalf("rejected replies were queued: %d", e.llm.Queued())
	}

	// 入队三条：文本 + 用量、工具调用（args 缺省 → {}；带 args 原样）、模型不可用
	if out := e.must("POST", "/hooks/butler/reply", map[string]any{"text": "钩子回复 <script>x</script>", "usage": map[string]int{"input_tokens": 6000, "output_tokens": 50}}); out["queued"] != float64(1) {
		t.Fatalf("queued = %v", out)
	}
	e.must("POST", "/hooks/butler/reply", map[string]any{"tool_calls": []map[string]any{{"name": "get_wallet"}, {"name": "get_instance", "args": map[string]string{"instance_id": "ins-abc"}}}})
	if out := e.must("POST", "/hooks/butler/reply", map[string]any{"error": true}); out["queued"] != float64(3) {
		t.Fatalf("queued = %v", out)
	}

	text, calls, usage, err := chat(t, e)
	if err != nil || text != "钩子回复 <script>x</script>" || len(calls) != 0 || usage == nil || usage.InputTokens != 6000 || usage.OutputTokens != 50 {
		t.Fatalf("reply 1 = %q %v %+v %v", text, calls, usage, err)
	}
	text, calls, _, err = chat(t, e)
	if err != nil || text != "" || len(calls) != 2 || calls[0].Name != "get_wallet" || string(calls[0].Args) != "{}" ||
		calls[1].Name != "get_instance" || string(calls[1].Args) != `{"instance_id":"ins-abc"}` || calls[0].ProviderCallID == "" {
		t.Fatalf("reply 2 = %q %+v %v", text, calls, err)
	}
	_, _, _, err = chat(t, e)
	var ue *llm.UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("reply 3 err = %v, want *llm.UnavailableError", err)
	}

	// reset 清空未用完的队列：之后回到规则表（「其它」→ 固定介绍文本）
	e.must("POST", "/hooks/butler/reply", map[string]any{"text": "不会被用到"})
	if out := e.must("POST", "/hooks/butler/reset", nil); out["queued"] != float64(0) || e.llm.Queued() != 0 {
		t.Fatalf("reset = %v, queued %d", out, e.llm.Queued())
	}
	if text, _, _, err := chat(t, e); err != nil || text == "不会被用到" || text == "" {
		t.Fatalf("after reset = %q %v, want the rule-table reply", text, err)
	}
	if code, _ := e.call("POST", "/hooks/butler/reset", map[string]any{"all": true}); code != 400 {
		t.Fatalf("reset with unknown field = %d, want 400", code)
	}
}

func TestButlerUsageHook(t *testing.T) {
	e := newButlerEnv(t)
	ctx := context.Background()
	uid := e.must("POST", "/hooks/users", map[string]any{"phone": "13900000077"})["id"]
	today := dbtest.T0.In(tz.CST).Format("2006-01-02")

	out := e.must("POST", "/hooks/butler/usage", map[string]any{"user_id": uid, "input_tokens": 6000, "output_tokens": 500})
	if out["day"] != today || out["input_tokens"] != float64(6000) || out["output_tokens"] != float64(500) || out["requests"] != float64(0) {
		t.Fatalf("usage = %v (today %s)", out, today)
	}
	out = e.must("POST", "/hooks/butler/usage", map[string]any{"user_id": uid, "day": today, "input_tokens": 1000, "output_tokens": 0, "requests": 2, "failures": 1})
	if out["input_tokens"] != float64(7000) || out["requests"] != float64(2) || out["failures"] != float64(1) {
		t.Fatalf("accumulated usage = %v", out)
	}
	id := int64(uid.(float64))
	used, over, err := e.a.Butler.OverBudget(ctx, id, 7500)
	if err != nil || used != 7500 || !over {
		t.Fatalf("OverBudget = %d %v %v, want 7500 over", used, over, err)
	}
	// 别的日子单独一行，不影响今天
	if out := e.must("POST", "/hooks/butler/usage", map[string]any{"user_id": uid, "day": "2025-12-31", "input_tokens": 99}); out["day"] != "2025-12-31" || out["input_tokens"] != float64(99) {
		t.Fatalf("other day = %v", out)
	}
	if u, err := e.a.Butler.UsageToday(ctx, id); err != nil || u.InputTokens != 7000 {
		t.Fatalf("UsageToday = %+v %v", u, err)
	}

	for name, body := range map[string]any{
		"no user":       map[string]any{"input_tokens": 1},
		"zero user":     map[string]any{"user_id": 0, "input_tokens": 1},
		"unknown user":  map[string]any{"user_id": 987654, "input_tokens": 1},
		"bad day":       map[string]any{"user_id": uid, "day": "2026/01/02"},
		"negative":      map[string]any{"user_id": uid, "output_tokens": -5},
		"unknown field": map[string]any{"user_id": uid, "tokens": 5},
	} {
		if code, out := e.call("POST", "/hooks/butler/usage", body); code != 400 {
			t.Fatalf("%s: %d %v, want 400", name, code, out)
		}
	}
}

func TestButlerSettingsHook(t *testing.T) {
	e := newButlerEnv(t)
	ctx := context.Background()
	if st, err := e.a.Butler.Settings(ctx); err != nil || !st.Enabled { // 先把缓存热起来：钩子必须让它失效
		t.Fatalf("settings = %+v %v", st, err)
	}
	out := e.must("POST", "/hooks/butler/settings", map[string]any{"enabled": false, "notice": "  维护中  "})
	if out["enabled"] != false || out["notice"] != "维护中" || out["daily_token_limit"] != float64(300000) || out["max_turns"] != float64(60) {
		t.Fatalf("settings = %v", out)
	}
	if st, err := e.a.Butler.Status(ctx, 1); err != nil || st.Enabled || st.Notice != "维护中" {
		t.Fatalf("status after hook = %+v %v (cache not invalidated?)", st, err)
	}
	// 只改给出的字段，其余保持
	out = e.must("POST", "/hooks/butler/settings", map[string]any{"daily_token_limit": 10000})
	if out["enabled"] != false || out["notice"] != "维护中" || out["daily_token_limit"] != float64(10000) {
		t.Fatalf("partial settings = %v", out)
	}
	if st, _ := e.a.Butler.Settings(ctx); st.DailyTokenLimit != 10000 {
		t.Fatalf("limit = %d", st.DailyTokenLimit)
	}
	for name, body := range map[string]any{
		"max_turns 0":   map[string]any{"max_turns": 0},
		"limit too low": map[string]any{"daily_token_limit": 1},
		"notice ctrl":   map[string]any{"notice": "a\x01b"},
		"wrong type":    map[string]any{"enabled": "no"},
		"unknown field": map[string]any{"enable": false},
	} {
		if code, out := e.call("POST", "/hooks/butler/settings", body); code != 400 {
			t.Fatalf("%s: %d %v, want 400", name, code, out)
		}
	}
	// defaults: true → 迁移默认值（用例之间复位）
	out = e.must("POST", "/hooks/butler/settings", map[string]any{"defaults": true})
	if out["enabled"] != true || out["notice"] != "" || out["daily_token_limit"] != float64(300000) || out["max_turns"] != float64(60) {
		t.Fatalf("defaults = %v", out)
	}
	audits := func() int {
		var n int
		if err := e.a.Fleet.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'butler.settings' AND actor_type = 'system'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := audits(); n != 3 {
		t.Fatalf("system butler.settings audits = %d, want 3", n)
	}
	// 合并后与当前设置相同：不保存、不写审计（每个用例开始前的 {defaults: true} 不刷审计页），返回值仍是当前设置
	for _, body := range []map[string]any{{"defaults": true}, {"notice": "  "}, {"enabled": true, "max_turns": 60}, {}} {
		out = e.must("POST", "/hooks/butler/settings", body)
		if out["enabled"] != true || out["notice"] != "" || out["daily_token_limit"] != float64(300000) || out["max_turns"] != float64(60) {
			t.Fatalf("no-op %v = %v", body, out)
		}
	}
	if n := audits(); n != 3 {
		t.Fatalf("no-op settings wrote audits: %d, want 3", n)
	}
	// 缓存过时也以库里为准：别处改了库（绕过缓存）后，相同的请求仍会保存
	if _, err := e.a.Fleet.Pool().Exec(ctx, `UPDATE butler_settings SET enabled = false`); err != nil {
		t.Fatal(err)
	}
	out = e.must("POST", "/hooks/butler/settings", map[string]any{"defaults": true})
	if out["enabled"] != true || audits() != 4 {
		t.Fatalf("after external change: %v audits=%d, want enabled and 4 audits", out, audits())
	}
}

// /hooks/sweep 在管家开启时也跑 Butler.Sweep，与 cmd/platform 的清扫器一致：假时钟越过 ConfirmTTL 后 pending → expired、对话回 idle。
func TestSweepHookRunsButlerSweep(t *testing.T) {
	e := newButlerEnv(t)
	ctx := context.Background()
	pool := e.a.Fleet.Pool()
	uid := int64(e.must("POST", "/hooks/users", map[string]any{"phone": "13900000078"})["id"].(float64))
	now := e.clk.Now()
	content, _ := json.Marshal([]llm.Block{{Type: llm.BlockToolUse, ToolCallID: "tc-sweep0000001", ProviderCallID: "mock-1",
		Name: "release_instance", Args: json.RawMessage(`{"instance_id":"ins-gone"}`)}})
	var msgID int64
	if _, err := pool.Exec(ctx, `INSERT INTO butler_conversations (id, user_id, title, status, provider, model, created_at, updated_at)
		VALUES ('cv-sweep0000001', $1, '释放', 'awaiting_confirm', 'mock', 'mock', $2, $2)`, uid, now); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO butler_messages (conversation_id, seq, role, content, created_at)
		VALUES ('cv-sweep0000001', 1, 'assistant', $1, $2) RETURNING id`, content, now).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO butler_tool_calls (id, conversation_id, message_id, user_id, provider_call_id, tool, kind, args, summary, status, expires_at, created_at)
		VALUES ('tc-sweep0000001', 'cv-sweep0000001', $1, $2, 'mock-1', 'release_instance', 'write', '{"instance_id":"ins-gone"}', '释放实例', 'pending', $3, $4)`,
		msgID, uid, now.Add(butler.ConfirmTTL), now); err != nil {
		t.Fatal(err)
	}
	state := func() (call, conv string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT t.status, c.status FROM butler_tool_calls t JOIN butler_conversations c ON c.id = t.conversation_id
			WHERE t.id = 'tc-sweep0000001'`).Scan(&call, &conv); err != nil {
			t.Fatal(err)
		}
		return call, conv
	}
	e.must("POST", "/hooks/sweep", map[string]any{"heartbeat": false})
	if call, conv := state(); call != "pending" || conv != "awaiting_confirm" {
		t.Fatalf("before TTL: %s / %s", call, conv)
	}
	e.must("POST", "/hooks/clock/advance", map[string]int{"seconds": int(butler.ConfirmTTL/time.Second) + 60})
	e.must("POST", "/hooks/sweep", map[string]any{"heartbeat": false})
	if call, conv := state(); call != "expired" || conv != "idle" {
		t.Fatalf("after TTL: %s / %s, want expired / idle", call, conv)
	}
}
