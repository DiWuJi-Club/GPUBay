package llm

// MissingToolResult 是配对修补时补上的工具结果内容（错误结果）。
const MissingToolResult = "（未执行）"

// CallID 取 tool_use / tool_result 块的配对 ID：模型给的调用 ID 优先，历史里缺失时退回本系统的 tool_call_id。
func CallID(b Block) string {
	if b.ProviderCallID != "" {
		return b.ProviderCallID
	}
	return b.ToolCallID
}

// PairToolCalls 防御性修补历史中的 tool_use / tool_result 配对（适配器发送前调用，两家 API 都会对未配对的调用报 400）：
//   - assistant 消息里的每个 tool_use，必须在紧随其后的 tool 消息里得到回答；缺失的补一个错误结果 MissingToolResult，
//     插在下一条 user / assistant 消息之前（或历史末尾）；
//   - tool_result 的 ID 不属于紧邻的上一条 assistant 消息（或重复回答）→ 丢弃；丢空的 tool 消息整条丢弃。
//
// 不修改入参；返回新切片。
func PairToolCalls(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	var pending []string          // 上一条 assistant 的 tool_use ID（按出现顺序）
	answered := map[string]bool{} // pending 中已回答的
	open := map[string]bool{}     // pending 的集合
	flush := func() {
		var fill []Block
		for _, id := range pending {
			if !answered[id] {
				fill = append(fill, Block{Type: BlockToolResult, ProviderCallID: id, OK: false, Content: MissingToolResult})
			}
		}
		if len(fill) > 0 {
			out = append(out, Message{Role: RoleTool, Blocks: fill})
		}
		pending, answered, open = nil, map[string]bool{}, map[string]bool{}
	}
	for _, m := range msgs {
		switch m.Role {
		case RoleTool:
			var keep []Block
			for _, b := range m.Blocks {
				if b.Type != BlockToolResult {
					continue
				}
				id := CallID(b)
				if !open[id] || answered[id] {
					continue
				}
				answered[id] = true
				keep = append(keep, b)
			}
			if len(keep) > 0 {
				out = append(out, Message{Role: RoleTool, Blocks: keep})
			}
		case RoleAssistant:
			flush()
			for _, b := range m.Blocks {
				if b.Type == BlockToolUse {
					if id := CallID(b); !open[id] {
						open[id] = true
						pending = append(pending, id)
					}
				}
			}
			out = append(out, m)
		default:
			flush()
			out = append(out, m)
		}
	}
	flush()
	return out
}
