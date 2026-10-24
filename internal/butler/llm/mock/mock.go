// Package mock 是进程内的确定性 llm.Provider（规格 §3.4），供 Go 测试、集成测试与 e2e 使用；生产环境由 config 校验禁止。
// 回复来源按优先级：队列（Enqueue，一条用一次；e2e 钩子 /hooks/butler/reply 写入）→ 规则（rules.go 的 Decide）。
package mock

import (
	"context"
	"fmt"
	"sync"

	"spark-cloud/internal/butler/llm"
)

// Reply 是 mock 的一轮回复。Err 非 nil → Chat 不发任何事件、直接返回该错误（钩子 error: true 用 *llm.UnavailableError）。
type Reply struct {
	Text      string
	ToolCalls []llm.ToolCall
	Usage     llm.Usage // 全零 → 默认 {1000, 100}
	Err       error
}

// DefaultUsage 是 Reply.Usage 未设置时上报的用量。
var DefaultUsage = llm.Usage{InputTokens: 1000, OutputTokens: 100}

const textChunkRunes = 8

// Mock 实现 llm.Provider；并发安全。
type Mock struct {
	mu    sync.Mutex
	queue []Reply
	seq   int64
	calls int
	last  *llm.Request
}

func New() *Mock { return &Mock{} }

func (m *Mock) Name() string  { return "mock" }
func (m *Mock) Model() string { return "mock" }

// Enqueue 追加一条回复；队列非空时 Chat 优先取队首。
func (m *Mock) Enqueue(r Reply) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue = append(m.queue, r)
}

// Queued 返回队列里尚未使用的回复数。
func (m *Mock) Queued() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.queue)
}

// Calls 返回 Chat 被调用的次数（含返回错误的调用）。
func (m *Mock) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// LastRequest 返回最近一次 Chat 收到的请求；从未调用 → ok = false。
func (m *Mock) LastRequest() (llm.Request, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		return llm.Request{}, false
	}
	return *m.last, true
}

// next 记录本次调用并取出回复：队列优先，否则走规则；给缺 ProviderCallID 的调用编号 "mock-N"。
func (m *Mock) next(req llm.Request) Reply {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	cp := req
	cp.Messages = append([]llm.Message(nil), req.Messages...)
	cp.Tools = append([]llm.ToolDef(nil), req.Tools...)
	m.last = &cp
	var r Reply
	if len(m.queue) > 0 {
		r = m.queue[0]
		m.queue = m.queue[1:]
	} else {
		r = Decide(req)
	}
	calls := make([]llm.ToolCall, len(r.ToolCalls))
	for i, c := range r.ToolCalls {
		if c.ProviderCallID == "" {
			m.seq++
			c.ProviderCallID = fmt.Sprintf("mock-%d", m.seq)
		}
		if len(c.Args) == 0 {
			c.Args = []byte(`{}`)
		}
		calls[i] = c
	}
	r.ToolCalls = calls
	return r
}

// Chat 按 llm.Provider 约定流式发出回复：文本每 8 rune 一段 EvText → EvTool* → EvUsage → EvDone。
// ctx 取消时立即返回 ctx.Err()（之后不再 emit）。
func (m *Mock) Chat(ctx context.Context, req llm.Request, emit func(llm.Event)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r := m.next(req)
	if r.Err != nil {
		return r.Err
	}
	send := func(e llm.Event) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		emit(e)
		return nil
	}
	runes := []rune(r.Text)
	for i := 0; i < len(runes); i += textChunkRunes {
		end := min(i+textChunkRunes, len(runes))
		if err := send(llm.Event{Type: llm.EvText, Text: string(runes[i:end])}); err != nil {
			return err
		}
	}
	for i := range r.ToolCalls {
		c := r.ToolCalls[i]
		if err := send(llm.Event{Type: llm.EvTool, ToolCall: &c}); err != nil {
			return err
		}
	}
	u := r.Usage
	if u == (llm.Usage{}) {
		u = DefaultUsage
	}
	if err := send(llm.Event{Type: llm.EvUsage, Usage: &u}); err != nil {
		return err
	}
	stop := llm.StopEndTurn
	if len(r.ToolCalls) > 0 {
		stop = llm.StopToolUse
	}
	return send(llm.Event{Type: llm.EvDone, StopReason: stop})
}
