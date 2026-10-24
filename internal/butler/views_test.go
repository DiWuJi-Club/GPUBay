package butler

import (
	"encoding/json"
	"testing"
)

// 各类型块的必有字段即使为空也要输出（前端按类型直接取字段）。
func TestViewBlocksAlwaysSerializeRequiredFields(t *testing.T) {
	ok, sus := false, false
	cases := []struct {
		v    any
		want string
	}{
		{ViewBlock{Type: BlockText}, `{"type":"text","text":""}`},
		{ViewBlock{Type: "tool", ID: "tc-1", Name: "get_wallet"}, `{"type":"tool","id":"tc-1","name":"get_wallet","label":"","status":""}`},
		{ViewBlock{Type: BlockToolResult, ToolCallID: "tc-1"}, `{"type":"tool_result","tool_call_id":"tc-1","summary":""}`},
		{AdminBlock{Type: BlockText}, `{"type":"text","text":""}`},
		{AdminBlock{Type: "tool", ID: "tc-1", Name: "x", Label: "y", Status: "ok"}, `{"type":"tool","id":"tc-1","name":"x","label":"y","status":"ok"}`},
		{AdminBlock{Type: BlockToolResult, ToolCallID: "tc-1", OK: &ok, Suspicious: &sus}, `{"type":"tool_result","tool_call_id":"tc-1","ok":false,"summary":"","suspicious":false}`},
		{AdminBlock{Type: BlockToolResult, ToolCallID: "tc-2"}, `{"type":"tool_result","tool_call_id":"tc-2","ok":false,"summary":"","suspicious":false}`},
		{AdminBlock{Type: BlockContext, InstanceID: "ins-a"}, `{"type":"context","instance_id":"ins-a"}`},
		{[]ViewBlock{{Type: BlockText, Text: "hi"}}, `[{"type":"text","text":"hi"}]`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("marshal %#v = %s, want %s", c.v, b, c.want)
		}
	}
}
