// Package butlertest 是 AI 管家的测试辅助（集成测试与 butler 包测试共用）。
package butlertest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

type block struct {
	Type       string `json:"type"`
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Content    string `json:"content"`
}

// CheckInvariants 检查管家表的一致性（规格 §14），在测试结束（所有轮次已结束）时调用：
//   - awaiting_confirm 的对话恰有 1 条 pending 调用；idle 的对话没有 pending；
//   - running 的对话 run_token 非空；非 running 的对话 run_token 为空；
//   - 每个 tool_use 块都有对应的 tool_result（pending / approved 的写调用除外）；
//   - secret_env_enc 只出现在 pending / approved 的调用上；
//   - generate_secret 的成功结果落库为 {"secret":"***"}（明文不落库）。
func CheckInvariants(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT c.id, c.status, c.run_token,
		(SELECT count(*) FROM butler_tool_calls t WHERE t.conversation_id = c.id AND t.status = 'pending')
		FROM butler_conversations c`)
	if err != nil {
		t.Fatalf("invariants: %v", err)
	}
	type conv struct {
		id, status, token string
		pending           int
	}
	var convs []conv
	for rows.Next() {
		var c conv
		if err := rows.Scan(&c.id, &c.status, &c.token, &c.pending); err != nil {
			t.Fatalf("invariants: %v", err)
		}
		convs = append(convs, c)
	}
	rows.Close()
	for _, c := range convs {
		switch c.status {
		case "awaiting_confirm":
			if c.pending != 1 {
				t.Errorf("invariant: 对话 %s awaiting_confirm 但有 %d 条 pending", c.id, c.pending)
			}
		case "idle":
			if c.pending != 0 {
				t.Errorf("invariant: 对话 %s idle 但有 %d 条 pending", c.id, c.pending)
			}
		}
		if (c.status == "running") != (c.token != "") {
			t.Errorf("invariant: 对话 %s status = %s, run_token 非空 = %v", c.id, c.status, c.token != "")
		}
	}

	var bad int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM butler_tool_calls WHERE secret_env_enc IS NOT NULL AND status NOT IN ('pending', 'approved')`).Scan(&bad); err != nil {
		t.Fatalf("invariants: %v", err)
	}
	if bad != 0 {
		t.Errorf("invariant: %d 条已结束的工具调用仍有 secret_env_enc", bad)
	}

	open := map[string]bool{} // pending / approved：允许没有 tool_result
	crow, err := pool.Query(ctx, `SELECT id FROM butler_tool_calls WHERE status IN ('pending', 'approved')`)
	if err != nil {
		t.Fatalf("invariants: %v", err)
	}
	for crow.Next() {
		var id string
		_ = crow.Scan(&id)
		open[id] = true
	}
	crow.Close()

	mrows, err := pool.Query(ctx, `SELECT conversation_id, role, content FROM butler_messages ORDER BY conversation_id, seq`)
	if err != nil {
		t.Fatalf("invariants: %v", err)
	}
	uses := map[string]string{} // tool_call_id → 工具名
	answered := map[string]bool{}
	results := map[string]block{}
	for mrows.Next() {
		var convID, role string
		var raw []byte
		if err := mrows.Scan(&convID, &role, &raw); err != nil {
			t.Fatalf("invariants: %v", err)
		}
		var blocks []block
		if err := json.Unmarshal(raw, &blocks); err != nil {
			t.Errorf("invariant: 消息内容不是块数组: %v", err)
			continue
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				uses[b.ToolCallID] = b.Name
			case "tool_result":
				answered[b.ToolCallID] = true
				results[b.ToolCallID] = b
			}
		}
	}
	mrows.Close()
	for id, name := range uses {
		if !answered[id] && !open[id] {
			t.Errorf("invariant: tool_use %s (%s) 没有 tool_result", id, name)
		}
		if r, ok := results[id]; ok && name == "generate_secret" && r.OK && r.Content != `{"secret":"***"}` {
			t.Errorf("invariant: generate_secret 结果 %s 落库了明文", id)
		}
	}
}
