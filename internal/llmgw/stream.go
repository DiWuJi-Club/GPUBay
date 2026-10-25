package llmgw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"spark-cloud/internal/llm"
)

// maxEventBytes：缓冲里超过这么多字节仍没有结束空行时原样转发（不再等，避免无界缓冲；与非流式响应上限一致）。
const maxEventBytes = llm.MaxUpstreamBodyBytes

// drainAfterDone / drainTimeout：[DONE] 之后把上游剩余字节读掉（通常只有 chunked 结束块），让连接能复用。
const (
	drainAfterDone = 64 << 10
	drainTimeout   = 2 * time.Second
)

// sseScanner 按事件切分上游 SSE 流，返回每个事件的原始字节（含结束空行，用于逐字节透传）与其 data 字段
// （多行 data 以 "\n" 连接；没有 data 行为 nil）。行结束符按 SSE 规范认 CRLF、LF、CR 三种，事件以空行结束
// （"\n\n"、"\r\n\r\n"、"\n\r\n"、"\r\r" 等）。流末尾不完整的事件原样返回、data 为 nil。
type sseScanner struct {
	r   io.Reader
	buf []byte
	tmp []byte
	max int
	err error // 读错误（io.EOF = 正常结束）

	pos       int // buf 里下一个待检查的字节（跨 Read 续扫，不重扫）
	lineStart int // 当前行的起点
}

func newSSEScanner(r io.Reader) *sseScanner {
	return &sseScanner{r: r, tmp: make([]byte, 32<<10), max: maxEventBytes}
}

// Next 返回下一个事件；ok=false 表示流已结束（Err 区分正常结束与读错误）。
func (s *sseScanner) Next() (event []byte, data []byte, ok bool) {
	for {
		if end := s.scan(); end >= 0 {
			event = bytes.Clone(s.buf[:end])
			s.buf = s.buf[end:]
			s.pos, s.lineStart = 0, 0
			return event, eventData(event), true
		}
		if len(s.buf) > s.max || (s.err != nil && len(s.buf) > 0) {
			event, s.buf = s.buf, nil
			s.pos, s.lineStart = 0, 0
			return event, nil, true
		}
		if s.err != nil {
			return nil, nil, false
		}
		n, err := s.r.Read(s.tmp)
		s.buf = append(s.buf, s.tmp[:n]...)
		if err != nil {
			s.err = err
		}
	}
}

// scan 从 pos 续扫，找到结束空行时返回事件末尾（不含）的下标，否则 -1。
// 缓冲末尾的 CR 可能是被拆开的 CRLF：流未结束时等下一次读再判。
func (s *sseScanner) scan() int {
	b := s.buf
	for s.pos < len(b) {
		c := b[s.pos]
		if c != '\n' && c != '\r' {
			s.pos++
			continue
		}
		next := s.pos + 1
		if c == '\r' {
			if next >= len(b) {
				if s.err == nil {
					return -1
				}
			} else if b[next] == '\n' {
				next++
			}
		}
		if s.pos == s.lineStart { // 空行：事件结束
			return next
		}
		s.pos, s.lineStart = next, next
	}
	return -1
}

// Err 返回导致流结束的读错误；正常结束（EOF）为 nil。
func (s *sseScanner) Err() error {
	if errors.Is(s.err, io.EOF) {
		return nil
	}
	return s.err
}

// sseLines 按 CRLF / LF / CR 切行（不含结束符）。
func sseLines(b []byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' && b[i] != '\r' {
			continue
		}
		out = append(out, string(b[start:i]))
		if b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n' {
			i++
		}
		start = i + 1
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

// eventData 取事件里的 data 行（"data:" 后去掉一个可选空格）。
func eventData(ev []byte) []byte {
	var out []byte
	found := false
	for _, line := range sseLines(ev) {
		v, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		v = strings.TrimPrefix(v, " ")
		if found {
			out = append(out, '\n')
		}
		out = append(out, v...)
		found = true
	}
	if !found {
		return nil
	}
	return out
}

// countChunk 解析一个 data 块：choices 里任一 delta.content / delta.reasoning_content（completions 端点是 text）非空 → 计一块；
// 带非 null 的 usage → 返回它。choices 与 usage 分开解码：一边类型不对不影响另一边。不是合法 JSON 的块不计数。
// n > 1 时一个块里多个 choice 也只计一块（估算只在客户端中断时用，且 vLLM 对 n > 1 本来就分块发送；保留）。
func countChunk(data []byte) (content bool, u *usageJSON) {
	var ch struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
			Text string `json:"text"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &ch) == nil {
		for _, c := range ch.Choices {
			if c.Delta.Content != "" || c.Delta.ReasoningContent != "" || c.Text != "" {
				content = true
				break
			}
		}
	}
	var us struct {
		Usage *usageJSON `json:"usage"`
	}
	if json.Unmarshal(data, &us) == nil {
		u = us.Usage
	}
	return content, u
}

// firstByteReader 记下第一次读到数据的时刻（ttft）。
type firstByteReader struct {
	r     io.Reader
	first time.Time
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 && f.first.IsZero() {
		f.first = time.Now()
	}
	return n, err
}

// streamErrorEvent 上游在首字节后出错时写给客户端的最后一个事件。
func streamErrorEvent(code, msg string) []byte {
	return []byte("data: " + string(errorJSON(typeServer, code, msg)) + "\n\n")
}

// stream 规格 §6.1 第 10 步（流式）：逐 SSE 事件透传并 Flush；统计内容块、取末尾 usage；[DONE] 结束。
// 客户端断开 → 取消上游、client_abort 按块数估算计费；停机取消 → error 事件、upstream_error 不收费；
// 总超时 → error 事件，已交付过内容块则按已交付的计费（status ok、estimated），否则 upstream_timeout 不收费；
// 上游首字节后断开 → error 事件、upstream_error 不收费（平台故障，规格 §1.1 #7）。
func (c *call) stream(ctx context.Context, node *llm.RouteNode, up upstreamResp) {
	resp := up.resp
	defer resp.Body.Close()
	h := c.w.Header()
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		h.Set("Content-Type", ct)
	} else {
		h.Set("Content-Type", "text/event-stream")
	}
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	c.w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(c.w)
	_ = rc.Flush()

	fr := &firstByteReader{r: resp.Body}
	sc := newSSEScanner(fr)
	chunks := 0
	var usage *usageJSON
	done, clientGone := false, false
	for {
		ev, data, ok := sc.Next()
		if !ok {
			break
		}
		if _, err := c.w.Write(ev); err != nil {
			clientGone = true
			break
		}
		if err := rc.Flush(); err != nil {
			clientGone = true
			break
		}
		if data == nil {
			continue
		}
		if string(bytes.TrimSpace(data)) == "[DONE]" {
			done = true
			break
		}
		content, u := countChunk(data)
		if content {
			chunks++
		}
		if u != nil {
			usage = u
		}
	}
	var ttft *int
	if !fr.first.IsZero() {
		ms := int(fr.first.Sub(c.start).Milliseconds())
		ttft = &ms
	}
	switch {
	case done:
		// [DONE] 之后读掉剩余字节（有上限、有时限），连接回到池里复用
		t := time.AfterFunc(drainTimeout, up.cancel)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainAfterDone))
		t.Stop()
	case clientGone:
		up.cancel() // 不再读上游：取消请求（vLLM 随之停止生成）
	}
	switch {
	case done || (sc.Err() == nil && usage != nil && !clientGone):
		o := outcome{node: node, status: llm.UsageOK, httpStatus: http.StatusOK, charge: true, ttft: ttft}
		if usage != nil {
			o.prompt, o.completed = usage.PromptTokens, usage.CompletionTokens
		} else {
			// 正常结束但没有 usage（理论不会：网关总是注入 include_usage）：按 T12 的方式估算
			o.prompt, o.completed, o.estimated = len(c.body)/3, chunks, true
		}
		c.record(o)
		c.g.svc.Router().Remember(c.ac.Key.ID, node.ID)
	case clientGone || c.r.Context().Err() != nil:
		o := c.abortOutcome(node, http.StatusOK, chunks, ttft)
		if usage != nil {
			o.prompt, o.completed, o.estimated = usage.PromptTokens, usage.CompletionTokens, false
		}
		c.record(o)
	case shutDown(ctx):
		c.g.log.Warn("llmgw: stream cancelled by shutdown", "node_id", node.ID, "request_id", c.reqID)
		c.writeStreamError(codeUpstreamError, "服务正在重启，本次请求不收费，请重试")
		c.record(outcome{node: node, status: llm.UsageUpstreamError, httpStatus: http.StatusOK, ttft: ttft})
	case (ctx.Err() != nil || isTimeout(sc.Err())) && chunks > 0:
		// 总超时前已经交付了内容：按已交付的计费，status ok、estimated（裁定修订 I12；输入 / 输出优先取已解析的 usage，
		// 否则与客户端中断同样估算：len(body)/3、内容块数）。块数为 0 仍是 upstream_timeout、不收费
		c.g.log.Warn("llmgw: upstream stream hit total timeout after content", "node_id", node.ID, "chunks", chunks, "request_id", c.reqID)
		c.writeStreamError(codeUpstreamTimeout, "请求超过总时长上限，已按已输出的内容计费")
		o := outcome{node: node, status: llm.UsageOK, httpStatus: http.StatusOK, prompt: len(c.body) / 3, completed: chunks,
			estimated: true, charge: true, ttft: ttft}
		if usage != nil {
			o.prompt, o.completed = usage.PromptTokens, usage.CompletionTokens
		}
		c.record(o)
	case ctx.Err() != nil || isTimeout(sc.Err()):
		c.g.log.Warn("llmgw: upstream stream timed out", "node_id", node.ID, "request_id", c.reqID)
		c.writeStreamError(codeUpstreamTimeout, "上游节点响应超时，本次请求不收费")
		c.record(outcome{node: node, status: llm.UsageUpstreamTimeout, httpStatus: http.StatusOK, ttft: ttft})
	default:
		c.g.log.Warn("llmgw: upstream stream dropped", "node_id", node.ID, "err", sc.Err(), "request_id", c.reqID)
		c.writeStreamError(codeUpstreamError, "上游连接中断，本次请求不收费")
		c.record(outcome{node: node, status: llm.UsageUpstreamError, httpStatus: http.StatusOK, ttft: ttft})
	}
}

func (c *call) writeStreamError(code, msg string) {
	if _, err := c.w.Write(streamErrorEvent(code, msg)); err == nil {
		_ = http.NewResponseController(c.w).Flush()
	}
}
