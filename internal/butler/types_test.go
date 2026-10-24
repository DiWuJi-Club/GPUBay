package butler

import (
	"encoding/json"
	"testing"
	"time"

	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/platform/apperr"
)

// 内容块的 JSON 形态是 butler_messages.content 的落库格式：空字段省略，往返不丢信息。
func TestBlockJSON(t *testing.T) {
	cases := []struct {
		b    Block
		want string
	}{
		{Block{Type: BlockText, Text: "你好"}, `{"type":"text","text":"你好"}`},
		{Block{Type: BlockToolUse, ToolCallID: "tc-1", ProviderCallID: "call_1", Name: "list_instances", Args: json.RawMessage(`{}`)},
			`{"type":"tool_use","tool_call_id":"tc-1","provider_call_id":"call_1","name":"list_instances","args":{}}`},
		{Block{Type: BlockToolResult, ToolCallID: "tc-1", ProviderCallID: "call_1", OK: true, Content: "结果"},
			`{"type":"tool_result","tool_call_id":"tc-1","provider_call_id":"call_1","ok":true,"content":"结果"}`},
		{Block{Type: BlockToolResult, ToolCallID: "tc-2", Content: "失败"}, `{"type":"tool_result","tool_call_id":"tc-2","content":"失败"}`},
		{Block{Type: BlockContext, InstanceID: "ins-abc"}, `{"type":"context","instance_id":"ins-abc"}`},
	}
	for _, tc := range cases {
		got, err := json.Marshal(tc.b)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("marshal %+v = %s, want %s", tc.b, got, tc.want)
		}
		var back Block
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatal(err)
		}
		again, _ := json.Marshal(back)
		if string(again) != tc.want {
			t.Errorf("round trip = %s, want %s", again, tc.want)
		}
	}
	// butler.Block 与 llm.Block 是同一类型（别名），常量值一致
	var _ llm.Block = Block{}
	if BlockText != "text" || BlockToolUse != "tool_use" || BlockToolResult != "tool_result" || BlockContext != "context" ||
		RoleUser != "user" || RoleAssistant != "assistant" || RoleTool != "tool" {
		t.Fatal("block / role constants changed")
	}
}

// 常量与迁移 00025 的 CHECK / DEFAULT、规格 §7 保持一致。
func TestConstants(t *testing.T) {
	if MaxUserChars != 2000 || MaxIterations != 8 || MaxToolResultBytes != 16384 || MaxSmallResultBytes != 4096 ||
		MaxHistoryMessages != 40 || MaxPromptTokens != 60000 || MaxConcurrentRuns != 32 || RetentionDays != 90 ||
		UsageRetentionDays != 400 || SweepDeleteBatch != 200 || DefaultMaxOutputTokens != 2048 || SendRatePerMinute != 20 ||
		ListLimit != 50 || TitleRunes != 30 || LLMFailAlertAfter != 5 {
		t.Fatal("limit constants changed")
	}
	if LLMTimeout != 60*time.Second || RunTimeout != 120*time.Second || ReadToolTimeout != 15*time.Second ||
		WriteToolTimeout != 30*time.Second || ConfirmTTL != 10*time.Minute || StaleRunAfter != 3*time.Minute {
		t.Fatal("timeouts changed")
	}
	if s := DefaultSettings(); !s.Enabled || s.DailyTokenLimit != 300000 || s.MaxTurns != 60 || s.Notice != "" || s.UpdatedBy != nil {
		t.Fatalf("default settings = %+v", s)
	}
	for _, s := range []string{ConvIdle, ConvRunning, ConvAwaitingConfirm} {
		if s != "idle" && s != "running" && s != "awaiting_confirm" {
			t.Fatalf("conversation status %q", s)
		}
	}
	got := []string{CallExecuted, CallFailed, CallPending, CallApproved, CallRejected, CallExpired}
	want := []string{"executed", "failed", "pending", "approved", "rejected", "expired"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call status %d = %q, want %q", i, got[i], want[i])
		}
	}
	if AuditToolExecute != "butler.tool_execute" || AuditSettings != "butler.settings" || AlertLLMDown != "butler_llm_unavailable" {
		t.Fatal("audit / alert names changed")
	}
}

func TestDisabledErr(t *testing.T) {
	if err := disabledErr("  "); err != apperr.ErrButlerDisabled {
		t.Fatalf("empty notice = %v", err)
	}
	e := apperr.As(disabledErr("系统维护中，预计 18:00 恢复"))
	if e == nil || e.Code != "butler_disabled" || e.Status != 503 || e.Message != "AI 管家暂未开放：系统维护中，预计 18:00 恢复" {
		t.Fatalf("with notice = %+v", e)
	}
	if apperr.ErrButlerDisabled.Message != "AI 管家暂未开放" {
		t.Fatal("shared error mutated")
	}
}
