package butler

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"spark-cloud/internal/butler/llm/mock"
)

func textMsg(seq int, role, text string) Message {
	return Message{Seq: seq, Role: role, Content: []Block{{Type: BlockText, Text: text}}}
}

func useMsg(seq int, id string) Message {
	return Message{Seq: seq, Role: RoleAssistant, Content: []Block{{Type: BlockToolUse, ToolCallID: id, ProviderCallID: "p-" + id, Name: "get_wallet", Args: []byte(`{}`)}}}
}

func resMsg(seq int, id, content string) Message {
	return Message{Seq: seq, Role: RoleTool, Content: []Block{{Type: BlockToolResult, ToolCallID: id, ProviderCallID: "p-" + id, OK: true, Content: content}}}
}

func seqs(msgs []Message) []int {
	out := make([]int, len(msgs))
	for i, m := range msgs {
		out[i] = m.Seq
	}
	return out
}

func TestHistoryKeepsFirstUser(t *testing.T) {
	var msgs []Message
	for i := 1; i <= 41; i++ {
		role := RoleUser
		if i%2 == 0 {
			role = RoleAssistant
		}
		msgs = append(msgs, textMsg(i, role, "消息"))
	}
	out := trimHistory(msgs)
	if len(out) != 41 || out[0].Seq != 1 || out[1].Seq != 2 {
		t.Fatalf("41 条：seqs = %v", seqs(out))
	}
	msgs = append(msgs, textMsg(42, RoleAssistant, "x"), textMsg(43, RoleUser, "y"))
	out = trimHistory(msgs)
	if len(out) != MaxHistoryMessages+1 || out[0].Seq != 1 || out[1].Seq != 4 || out[len(out)-1].Seq != 43 {
		t.Fatalf("43 条：seqs = %v", seqs(out))
	}
	// 输入未被修改
	if len(msgs) != 43 {
		t.Fatal("input modified")
	}
}

func TestHistoryKeepsToolPairs(t *testing.T) {
	// seq 1 user，2..3 填充，之后 assistant(tool_use) + tool(result) 成对；窗口恰好从一条 tool 消息开始 → 丢掉它
	msgs := []Message{textMsg(1, RoleUser, "第一条")}
	seq := 2
	for len(msgs) < 45 {
		msgs = append(msgs, useMsg(seq, "tc"+itoa(seq)), resMsg(seq+1, "tc"+itoa(seq), "结果"))
		seq += 2
	}
	msgs = append(msgs, textMsg(seq, RoleUser, "最新"))
	out := trimHistory(msgs)
	if out[0].Seq != 1 || out[1].Role == RoleTool {
		t.Fatalf("seqs = %v roles[1] = %s", seqs(out), out[1].Role)
	}
	// 每个 tool 消息前面都是它的 assistant
	for i, m := range out {
		if m.Role == RoleTool && (i == 0 || out[i-1].Role != RoleAssistant || out[i-1].Content[0].ToolCallID != m.Content[0].ToolCallID) {
			t.Fatalf("tool 消息 %d 没有配对的 tool_use: %v", m.Seq, seqs(out))
		}
	}
	if len(out) > MaxHistoryMessages+1 {
		t.Fatalf("len = %d", len(out))
	}
}

func TestHistoryTokenBudget(t *testing.T) {
	big := strings.Repeat("日志", 19500) // ≈ 19500 token
	msgs := []Message{textMsg(1, RoleUser, "第一条")}
	for i := 0; i < 4; i++ {
		s := 2 + i*2
		msgs = append(msgs, useMsg(s, "tc"+itoa(s)), resMsg(s+1, "tc"+itoa(s), big))
	}
	msgs = append(msgs, textMsg(10, RoleUser, "最新"))
	out := trimHistory(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("省略工具结果就够了，不该丢消息: %v", seqs(out))
	}
	omitted := 0
	for _, m := range out {
		for _, b := range m.Content {
			if b.Type == BlockToolResult && b.Content == OmittedContent {
				omitted++
			}
		}
	}
	// 总 ≈ 78k > 历史预算 54k（60k − 6k 预留）：从旧到新省略，省到 ≤ 54k 为止 → 恰好省略最旧的 2 条
	if omitted != 2 || out[2].Content[0].Content != OmittedContent || out[4].Content[0].Content != OmittedContent ||
		out[6].Content[0].Content != big || out[8].Content[0].Content != big {
		t.Fatalf("omitted = %d", omitted)
	}
	if msgs[2].Content[0].Content != big {
		t.Fatal("input modified")
	}
	// 用户文本本身就超：丢旧消息，保留首条 user 与最后一条
	huge := strings.Repeat("长", 100000)
	msgs2 := []Message{textMsg(1, RoleUser, "第一条"), textMsg(2, RoleAssistant, huge), textMsg(3, RoleUser, huge[:30000*3]), useMsg(4, "a"), resMsg(5, "a", "r"), textMsg(6, RoleUser, "最新")}
	out = trimHistory(msgs2)
	if out[0].Seq != 1 || out[len(out)-1].Seq != 6 || len(out) != 5 {
		t.Fatalf("seqs = %v", seqs(out))
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// 集成：41 条历史时模型请求里仍有第一条 user 消息。
func TestHistoryInRequest(t *testing.T) {
	e := newEnv(t)
	c, _ := e.send("new", "最早的问题")
	for i := 0; i < 21; i++ {
		e.mock.Enqueue(mock.Reply{Text: "答"})
		e.send(c.ID, "后续")
	}
	req, _ := e.mock.LastRequest()
	if len(req.Messages) != MaxHistoryMessages+1 || req.Messages[0].Blocks[0].Text != "最早的问题" {
		t.Fatalf("messages = %d first = %+v", len(req.Messages), req.Messages[0])
	}
	if n := len(e.msgs(c.ID)); n != 44 {
		t.Fatalf("stored = %d", n)
	}
	_ = context.Background()
}
