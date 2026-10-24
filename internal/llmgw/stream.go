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

// maxEventBytes：缓冲里超过这么多字节仍没有事件分隔符时原样转发（不再等分隔符，避免无界缓冲）。
const maxEventBytes = 1 << 20

// sseScanner 按事件切分上游 SSE 流（以空行 "\n\n" 或 "\r\n\r\n" 结束），返回每个事件的原始字节（含分隔符，
// 用于逐字节透传）与其 data 字段（多行 data 以 "\n" 连接；没有 data 行为 nil）。流末尾不完整的事件原样返回、data 为 nil。
type sseScanner struct {
	r   io.Reader
	buf []byte
	tmp []byte
	err error // 读错误（io.EOF = 正常结束）
}

func newSSEScanner(r io.Reader) *sseScanner {
	return &sseScanner{r: r, tmp: make([]byte, 32<<10)}
}

// Next 返回下一个事件；ok=false 表示流已结束（Err 区分正常结束与读错误）。
func (s *sseScanner) Next() (event []byte, data []byte, ok bool) {
	for {
		if i, n := eventEnd(s.buf); i >= 0 {
			event = bytes.Clone(s.buf[:i+n])
			s.buf = s.buf[i+n:]
			return event, eventData(event), true
		}
		if len(s.buf) > maxEventBytes {
			event, s.buf = s.buf, nil
			return event, nil, true
		}
		if s.err != nil {
			if len(s.buf) > 0 {
				event, s.buf = s.buf, nil
				return event, nil, true
			}
			return nil, nil, false
		}
		n, err := s.r.Read(s.tmp)
		s.buf = append(s.buf, s.tmp[:n]...)
		if err != nil {
			s.err = err
		}
	}
}

// Err 返回导致流结束的读错误；正常结束（EOF）为 nil。
func (s *sseScanner) Err() error {
	if errors.Is(s.err, io.EOF) {
		return nil
	}
	return s.err
}

// eventEnd 返回第一个事件分隔符的位置与长度；没有返回 -1。
func eventEnd(b []byte) (int, int) {
	i := bytes.Index(b, []byte("\n\n"))
	j := bytes.Index(b, []byte("\r\n\r\n"))
	switch {
	case i < 0 && j < 0:
		return -1, 0
	case j >= 0 && (i < 0 || j < i):
		return j, 4
	default:
		return i, 2
	}
}

// eventData 取事件里的 data 行（"data:" 后去掉一个可选空格）。
func eventData(ev []byte) []byte {
	var out []byte
	found := false
	for _, line := range strings.Split(string(ev), "\n") {
		line = strings.TrimSuffix(line, "\r")
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
// 带非 null 的 usage → 返回它。不是合法 JSON 的块不计数。
func countChunk(data []byte) (content bool, u *usageJSON) {
	var ch struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
			Text string `json:"text"`
		} `json:"choices"`
		Usage *usageJSON `json:"usage"`
	}
	if json.Unmarshal(data, &ch) != nil {
		return false, nil
	}
	for _, c := range ch.Choices {
		if c.Delta.Content != "" || c.Delta.ReasoningContent != "" || c.Text != "" {
			content = true
			break
		}
	}
	return content, ch.Usage
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
// 客户端断开 → 取消上游、client_abort 按块数估算计费；上游首字节后断开 / 超时 → 写一个 error 事件后结束，不计费。
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
	if done || clientGone {
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
