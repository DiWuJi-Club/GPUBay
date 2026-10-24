package butler

import (
	"context"
	"strings"

	"spark-cloud/internal/butler/llm"
)

// OmittedContent 是历史超出 token 上限时替换旧工具结果的内容。
const OmittedContent = "（已省略）"

// msgTokens 估算一条消息的 token（文本 + 工具参数 + 工具结果，每条另加固定开销）。
func msgTokens(m Message) int {
	var b strings.Builder
	for _, x := range m.Content {
		b.WriteString(x.Text)
		b.Write(x.Args)
		b.WriteString(x.Content)
	}
	return llm.EstimateTokens(b.String())
}

// trimHistory 按「已做出的裁定·历史裁剪」裁剪 seq 升序的历史（输入不被修改）：
//  1. 取最近 MaxHistoryMessages 条；窗口开头的 tool 消息（它们的 tool_use 在窗口外）一起丢掉，保证 tool_use / tool_result 成对；
//  2. 对话的第一条 user 消息不在窗口里时放到最前面；
//  3. 估算 token > MaxPromptTokens 时，从旧到新把 tool_result 的 content 换成「（已省略）」；
//  4. 仍超时从旧到新丢整条（第一条 user 消息与最后一条消息不丢；丢 assistant 时连同紧随其后的 tool 消息一起丢）。
func trimHistory(msgs []Message) []Message {
	if len(msgs) == 0 {
		return []Message{}
	}
	var first *Message
	for i := range msgs {
		if msgs[i].Role == RoleUser {
			first = &msgs[i]
			break
		}
	}
	window := msgs[max(0, len(msgs)-MaxHistoryMessages):]
	for len(window) > 1 && window[0].Role == RoleTool {
		window = window[1:]
	}
	out := make([]Message, 0, len(window)+1)
	if first != nil && first.Seq < window[0].Seq {
		out = append(out, *first)
	}
	out = append(out, window...)

	total := 0
	for _, m := range out {
		total += msgTokens(m)
	}
	if total <= MaxPromptTokens {
		return out
	}
	// 3. 省略旧工具结果（复制块，不改调用方的切片）
	for i := 0; i < len(out) && total > MaxPromptTokens; i++ {
		if out[i].Role != RoleTool {
			continue
		}
		blocks := append([]Block(nil), out[i].Content...)
		changed := false
		for k := range blocks {
			if blocks[k].Type == BlockToolResult && blocks[k].Content != OmittedContent {
				blocks[k].Content = OmittedContent
				changed = true
			}
		}
		if changed {
			before := msgTokens(out[i])
			out[i].Content = blocks
			total += msgTokens(out[i]) - before
		}
	}
	// 4. 丢整条
	lo := 0
	if len(out) > 0 && out[0].Role == RoleUser {
		lo = 1
	}
	for total > MaxPromptTokens {
		i := lo
		if i >= len(out)-1 {
			break
		}
		j := i + 1
		if out[i].Role == RoleAssistant {
			for j < len(out) && out[j].Role == RoleTool {
				j++
			}
		}
		if j >= len(out) {
			break
		}
		for _, m := range out[i:j] {
			total -= msgTokens(m)
		}
		out = append(out[:i], out[j:]...)
	}
	return out
}

// toLLM 把落库消息转为模型消息；secrets 里的 tool_call_id（本轮 generate_secret 的结果）用内存里的明文替换落库的 ***。
// context 块原样带上（适配器忽略它；mock 规则读它）。
func toLLM(msgs []Message, secrets map[string]string) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]Block, len(m.Content))
		copy(blocks, m.Content)
		for k := range blocks {
			if blocks[k].Type == BlockToolResult {
				if v, ok := secrets[blocks[k].ToolCallID]; ok {
					blocks[k].Content = v
				}
			}
		}
		out = append(out, llm.Message{Role: m.Role, Blocks: blocks})
	}
	return out
}

// loadHistory 读取并裁剪本轮要发给模型的历史。
func (s *Service) loadHistory(ctx context.Context, convID string, secrets map[string]string) ([]llm.Message, error) {
	msgs, err := s.historyWindow(ctx, convID, MaxHistoryMessages)
	if err != nil {
		return nil, err
	}
	return toLLM(trimHistory(msgs), secrets), nil
}
