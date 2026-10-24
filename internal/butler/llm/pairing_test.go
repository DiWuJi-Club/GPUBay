package llm

import (
	"reflect"
	"testing"
)

func roles(ms []Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Role
	}
	return out
}

func resultIDs(m Message) []string {
	var out []string
	for _, b := range m.Blocks {
		out = append(out, CallID(b))
	}
	return out
}

func TestPairToolCalls(t *testing.T) {
	text := func(role, s string) Message { return Message{Role: role, Blocks: []Block{{Type: BlockText, Text: s}}} }
	use := func(ids ...string) Message {
		m := Message{Role: RoleAssistant}
		for _, id := range ids {
			m.Blocks = append(m.Blocks, Block{Type: BlockToolUse, ToolCallID: "tc-" + id, ProviderCallID: id, Name: "get_wallet"})
		}
		return m
	}
	res := func(ids ...string) Message {
		m := Message{Role: RoleTool}
		for _, id := range ids {
			m.Blocks = append(m.Blocks, Block{Type: BlockToolResult, ProviderCallID: id, OK: true, Content: "ok"})
		}
		return m
	}
	in := []Message{
		res("orphan"),        // 没有前置 assistant → 丢弃
		text(RoleUser, "hi"), //
		use("a", "b"),        // b 未回答 → 补错误结果
		res("a", "zzz"),      // zzz 不属于上一条 assistant → 丢弃
		res("a"),             // 重复回答 → 丢弃（整条空 → 丢）
		use("c"),             // c 的结果在 user 之后 → 补错误结果、后面的丢弃
		text(RoleUser, "继续"), //
		res("c"),             //
		use("d"),             // 历史末尾未回答 → 补
	}
	orig := len(in[3].Blocks)
	out := PairToolCalls(in)
	if len(in[3].Blocks) != orig {
		t.Fatal("不应修改入参")
	}
	want := []string{RoleUser, RoleAssistant, RoleTool, RoleTool, RoleAssistant, RoleTool, RoleUser, RoleAssistant, RoleTool}
	if got := roles(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	if got := resultIDs(out[2]); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("out[2] = %v", got)
	}
	check := func(m Message, id string) {
		t.Helper()
		if len(m.Blocks) != 1 {
			t.Fatalf("blocks = %+v", m.Blocks)
		}
		b := m.Blocks[0]
		if b.Type != BlockToolResult || CallID(b) != id || b.OK || b.Content != MissingToolResult {
			t.Fatalf("filler = %+v", b)
		}
	}
	check(out[3], "b")
	check(out[5], "c")
	check(out[8], "d")

	// 正常历史原样返回
	ok := []Message{text(RoleUser, "x"), use("a", "b"), res("a"), res("b"), text(RoleAssistant, "done")}
	if got := PairToolCalls(ok); !reflect.DeepEqual(got, ok) {
		t.Fatalf("正常历史被改动: %+v", got)
	}
	// ProviderCallID 缺失时用 ToolCallID 配对
	fb := []Message{{Role: RoleAssistant, Blocks: []Block{{Type: BlockToolUse, ToolCallID: "tc-1"}}}, {Role: RoleTool, Blocks: []Block{{Type: BlockToolResult, ToolCallID: "tc-1", OK: true}}}}
	if got := PairToolCalls(fb); !reflect.DeepEqual(got, fb) {
		t.Fatalf("回退 ID 配对失败: %+v", got)
	}
}
