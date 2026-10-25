package llmgw

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/llmgw/llmtest"
)

func TestSSEScanner(t *testing.T) {
	in := "data: {\"a\":1}\n\n: keepalive\n\nevent: x\ndata: line1\ndata: line2\n\ndata:[DONE]\r\n\r\ndata: partial"
	sc := newSSEScanner(iotest.OneByteReader(strings.NewReader(in)))
	type ev struct{ raw, data string }
	var got []ev
	for {
		e, d, ok := sc.Next()
		if !ok {
			break
		}
		got = append(got, ev{string(e), string(d)})
	}
	want := []ev{
		{"data: {\"a\":1}\n\n", `{"a":1}`},
		{": keepalive\n\n", ""},
		{"event: x\ndata: line1\ndata: line2\n\n", "line1\nline2"},
		{"data:[DONE]\r\n\r\n", "[DONE]"},
		{"data: partial", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}
	if sc.Err() != nil {
		t.Fatalf("Err = %v", sc.Err())
	}
	var all strings.Builder
	for _, e := range got {
		all.WriteString(e.raw)
	}
	if all.String() != in {
		t.Fatal("concatenated events differ from input")
	}
	// 其它空行形态：LF + CRLF、CR CR、CRLF 被拆在两次读之间（OneByteReader）
	in2 := "data: a\n\r\ndata: b\r\rdata: c\r\n\r\n"
	sc = newSSEScanner(iotest.OneByteReader(strings.NewReader(in2)))
	var got2 []string
	for {
		e, d, ok := sc.Next()
		if !ok {
			break
		}
		got2 = append(got2, string(e)+"|"+string(d))
	}
	if want := []string{"data: a\n\r\n|a", "data: b\r\r|b", "data: c\r\n\r\n|c"}; !reflect.DeepEqual(got2, want) {
		t.Fatalf("blank-line variants:\n got %q\nwant %q", got2, want)
	}
	// 大事件：> 32 KiB（跨多次读）与 > 1 MiB 都完整解析
	for _, size := range []int{40 << 10, 1<<20 + 123} {
		payload := `{"choices":[{"delta":{"content":"` + strings.Repeat("y", size) + `"}}]}`
		raw := "data: " + payload + "\n\n" + "data: [DONE]\n\n"
		sc := newSSEScanner(strings.NewReader(raw))
		ev, d, ok := sc.Next()
		if !ok || string(ev) != "data: "+payload+"\n\n" || string(d) != payload {
			t.Fatalf("size %d: event len %d data len %d", size, len(ev), len(d))
		}
		if c, _ := countChunk(d); !c {
			t.Fatalf("size %d: content not counted", size)
		}
		if _, d, ok := sc.Next(); !ok || string(d) != "[DONE]" {
			t.Fatalf("size %d: [DONE] lost", size)
		}
	}
	// 超过上限仍没有空行：原样转发、不解析
	over := "data: " + strings.Repeat("q", 100) + "\n\n"
	sc = newSSEScanner(iotest.OneByteReader(strings.NewReader(over)))
	sc.max = 50
	var relayed []byte
	first, d, ok := sc.Next()
	if !ok || d != nil || len(first) != 51 {
		t.Fatalf("over-limit event: ok %v len %d data %q", ok, len(first), d)
	}
	relayed = append(relayed, first...)
	for {
		part, _, ok := sc.Next()
		if !ok {
			break
		}
		relayed = append(relayed, part...)
	}
	if string(relayed) != over {
		t.Fatal("over-limit bytes not relayed intact")
	}
	sc = newSSEScanner(iotest.TimeoutReader(strings.NewReader("data: x\n\n")))
	if _, _, ok := sc.Next(); !ok {
		t.Fatal("first event lost")
	}
	if _, _, ok := sc.Next(); ok || sc.Err() == nil {
		t.Fatal("read error not reported")
	}
}

func TestCountChunk(t *testing.T) {
	for _, c := range []struct {
		data    string
		content bool
		usage   *usageJSON
	}{
		{`{"choices":[{"delta":{"role":"assistant","content":""}}]}`, false, nil},
		{`{"choices":[{"delta":{"content":null}}]}`, false, nil},
		{`{"choices":[{"delta":{"content":"hi"}}]}`, true, nil},
		{`{"choices":[{"delta":{"reasoning_content":"think"}}]}`, true, nil},
		{`{"choices":[{"text":"x"}]}`, true, nil},
		{`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}`, false, &usageJSON{3, 4}},
		{`{"choices":[{"delta":{}}],"usage":null}`, false, nil},
		{`{"choices":{"bad":true},"usage":{"prompt_tokens":1,"completion_tokens":2}}`, false, &usageJSON{1, 2}},
		{`{"choices":[{"delta":{"content":"x"}}],"usage":"bad"}`, true, nil},
		{`not json`, false, nil},
	} {
		content, u := countChunk([]byte(c.data))
		if content != c.content || !reflect.DeepEqual(u, c.usage) {
			t.Fatalf("%s: content %v usage %+v", c.data, content, u)
		}
	}
}

func TestUpstreamBodyInjection(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"stream_options":{"continuous_usage_stats":false}}`)
	p, msg := parseRequest(body)
	if msg != "" {
		t.Fatal(msg)
	}
	out, err := upstreamBody(body, p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	so := got["stream_options"].(map[string]any)
	if so["include_usage"] != true || so["continuous_usage_stats"] != false || got["model"] != "m" {
		t.Fatalf("injected body %s", out)
	}
	for _, b := range []string{`{"model":"m","stream":false}`, `{"model":"m"}`, `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`} {
		p, _ := parseRequest([]byte(b))
		out, _ := upstreamBody([]byte(b), p)
		if string(out) != b {
			t.Fatalf("body rewritten without need: %s → %s", b, out)
		}
	}
}

func TestCheckOutputBoundsAndMaxTokensInjection(t *testing.T) {
	m := llm.Model{Kind: llm.KindChat, MaxOutputTokens: 100}
	// 都没给：注入 max_tokens，与 include_usage 注入叠加
	body := []byte(`{"model":"m","stream":true,"n":3,"best_of":5}`)
	p, _ := parseRequest(body)
	if msg := checkOutputBounds(&p, m); msg != "" || p.injectMaxTokens != 100 || p.fanout != 5 {
		t.Fatalf("msg %q inject %d fanout %d", msg, p.injectMaxTokens, p.fanout)
	}
	out, err := upstreamBody(body, p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["max_tokens"] != float64(100) || got["stream_options"].(map[string]any)["include_usage"] != true || got["n"] != float64(3) {
		t.Fatalf("patched body %s", out)
	}
	// 给了任一字段：不注入；null 视为未给
	for _, b := range []string{`{"model":"m","max_tokens":100}`, `{"model":"m","max_completion_tokens":1}`, `{"model":"m","max_tokens":0}`} {
		p, _ := parseRequest([]byte(b))
		if msg := checkOutputBounds(&p, m); msg != "" || p.injectMaxTokens != 0 || p.fanout != 1 {
			t.Fatalf("%s: msg %q inject %d fanout %d", b, msg, p.injectMaxTokens, p.fanout)
		}
		if out, _ := upstreamBody([]byte(b), p); string(out) != b {
			t.Fatalf("%s rewritten: %s", b, out)
		}
	}
	p, _ = parseRequest([]byte(`{"model":"m","max_tokens":null,"n":null}`))
	if msg := checkOutputBounds(&p, m); msg != "" || p.injectMaxTokens != 100 || p.fanout != 1 {
		t.Fatalf("null fields: msg %q inject %d fanout %d", msg, p.injectMaxTokens, p.fanout)
	}
	for b, want := range map[string]string{
		`{"model":"m","max_tokens":101}`:                           "max_tokens 不能超过该模型的最大输出 100",
		`{"model":"m","max_tokens":5,"max_completion_tokens":101}`: "max_completion_tokens 不能超过该模型的最大输出 100",
		`{"model":"m","n":9}`:                                      "n 不能超过 8",
		`{"model":"m","best_of":9}`:                                "best_of 不能超过 8",
		`{"model":"m","best_of":1.5}`:                              "best_of 必须是整数",
	} {
		p, _ := parseRequest([]byte(b))
		if msg := checkOutputBounds(&p, m); msg != want {
			t.Fatalf("%s: msg %q, want %q", b, msg, want)
		}
	}
}

// readEvents 读完 SSE 响应体，返回原始字节与内容块数。
func contentChunks(raw []byte) int {
	n := 0
	sc := newSSEScanner(bytes.NewReader(raw))
	for {
		_, d, ok := sc.Next()
		if !ok {
			return n
		}
		if c, _ := countChunk(d); c {
			n++
		}
	}
}

func TestGatewayStreamPassthroughAndUsage(t *testing.T) {
	e := newGEnv(t)
	m := e.model("st-chat", llm.KindChat)
	fake := &llmtest.FakeVLLM{PromptTokens: 20, CompletionTokens: 5, Chunks: []string{"一", "二", "三", "四", "五"}}
	nodeID, _ := e.node(m, fake, "")
	_, key := e.userKey()

	body := chatBody("st-chat", `"stream":true`, `"temperature":0.5`, `"max_tokens":64`)
	resp := e.request(e.ctx, http.MethodPost, "/v1/chat/completions", key, "", body)
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d err %v %s", resp.StatusCode, err, got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type %q", ct)
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("stream headers %v", resp.Header)
	}
	rec := fake.Recorded()
	if len(rec) != 1 {
		t.Fatalf("fake got %d requests", len(rec))
	}
	if !bytes.Equal(got, rec[0].Sent) {
		t.Fatalf("stream bytes differ:\n got %q\nwant %q", got, rec[0].Sent)
	}
	if contentChunks(got) != 5 || !strings.HasSuffix(string(got), "data: [DONE]\n\n") {
		t.Fatalf("stream content %q", got)
	}
	// 注入 stream_options.include_usage，其它字段不变
	var sent, orig map[string]any
	if err := json.Unmarshal(rec[0].Body, &sent); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(body), &orig)
	so, _ := sent["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("include_usage not injected: %s", rec[0].Body)
	}
	delete(sent, "stream_options")
	if !reflect.DeepEqual(sent, orig) {
		t.Fatalf("other fields changed:\n got %v\nwant %v", sent, orig)
	}
	row := e.usageRow(resp.Header.Get("X-Request-Id"))
	if row.Status != llm.UsageOK || !row.Stream || row.PromptTokens != 20 || row.CompletionTokens != 5 || row.Estimated ||
		row.CostMicro != 20*100+5*400 || nodeIDOf(row) != nodeID || row.HTTPStatus != 200 {
		t.Fatalf("usage row %+v", row)
	}
	if row.TTFTMS == nil || *row.TTFTMS < 0 {
		t.Fatalf("ttft_ms = %v", row.TTFTMS)
	}

	// 客户端自己带了 include_usage=true：请求体原样转发
	body2 := chatBody("st-chat", `"stream":true`, `"stream_options":{"include_usage":true}`, `"max_completion_tokens":32`)
	resp2, got2 := e.post("/v1/chat/completions", key, body2)
	if resp2.StatusCode != http.StatusOK || !bytes.Equal(fake.Recorded()[1].Body, []byte(body2)) {
		t.Fatalf("body rewritten: %s", fake.Recorded()[1].Body)
	}
	if !bytes.Equal(got2, fake.Recorded()[1].Sent) {
		t.Fatal("second stream bytes differ")
	}
	// [DONE] 之后读完上游剩余字节：两次流式请求复用同一条上游连接
	if n := e.nodeConns(nodeID); n != 1 {
		t.Fatalf("upstream connections = %d, want 1 (connection reused after [DONE])", n)
	}

	// 流式请求遇上游 4xx：按非流式透传
	bad := e.model("st-bad", llm.KindChat)
	e.node(bad, &llmtest.FakeVLLM{ErrorStatus: http.StatusUnprocessableEntity}, "")
	resp3, _ := e.post("/v1/chat/completions", key, chatBody("st-bad", `"stream":true`))
	if resp3.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("stream 4xx: %d", resp3.StatusCode)
	}
	if row := e.usageRow(resp3.Header.Get("X-Request-Id")); row.Status != llm.UsageOK || row.CostMicro != 0 || row.HTTPStatus != 422 {
		t.Fatalf("stream 4xx usage %+v", row)
	}
}

func TestGatewayClientAbortEstimates(t *testing.T) {
	e := newGEnv(t)
	m := e.model("ab-chat", llm.KindChat)
	chunks := make([]string, 20)
	for i := range chunks {
		chunks[i] = "块"
	}
	fake := &llmtest.FakeVLLM{PromptTokens: 50, CompletionTokens: 20, Chunks: chunks, Delay: 50 * time.Millisecond}
	e.node(m, fake, "")
	_, key := e.userKey()
	body := chatBody("ab-chat", `"stream":true`)

	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	resp := e.request(ctx, http.MethodPost, "/v1/chat/completions", key, "", body)
	reqID := resp.Header.Get("X-Request-Id")
	rd := bufio.NewReader(resp.Body)
	seen := 0
	var ev []byte
	for seen < 2 {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		ev = append(ev, line...)
		if string(line) == "\n" {
			if c, _ := countChunk(eventData(ev)); c {
				seen++
			}
			ev = nil
		}
	}
	cancel()
	resp.Body.Close()

	row := e.usageRow(reqID)
	if row.Status != llm.UsageClientAbort || row.CompletionTokens != 2 || !row.Estimated || row.PromptTokens != len(body)/3 {
		t.Fatalf("usage row %+v (body len %d)", row, len(body))
	}
	if want := llm.Cost(m, len(body)/3, 2); row.CostMicro != want || want == 0 {
		t.Fatalf("cost_micro = %d, want %d", row.CostMicro, want)
	}
	// 上游请求随之取消：假节点不会把 20 块都发完
	time.Sleep(200 * time.Millisecond)
	if n := contentChunks(fake.Recorded()[0].Sent); n >= 20 {
		t.Fatalf("upstream not cancelled: sent %d chunks", n)
	}
}

func TestGatewayUpstreamDropAfterFirstByteNotCharged(t *testing.T) {
	e := newGEnv(t)
	m := e.model("drop-chat", llm.KindChat)
	fake := &llmtest.FakeVLLM{PromptTokens: 10, CompletionTokens: 5, Chunks: []string{"a", "b", "c", "d", "e"}, DropAfterChunks: 2}
	e.node(m, fake, "")
	_, key := e.userKey()
	resp, got := e.post("/v1/chat/completions", key, chatBody("drop-chat", `"stream":true`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if n := contentChunks(got); n != 2 {
		t.Fatalf("client got %d chunks: %q", n, got)
	}
	sent := fake.Recorded()[0].Sent
	if !bytes.HasPrefix(got, sent) {
		t.Fatalf("relayed bytes are not the upstream prefix:\n got %q\nsent %q", got, sent)
	}
	tail := string(got[len(sent):])
	if !strings.HasPrefix(tail, `data: {"error":`) || !strings.Contains(tail, `"code":"upstream_error"`) || !strings.HasSuffix(tail, "\n\n") {
		t.Fatalf("missing error event, tail %q", tail)
	}
	row := e.usageRow(resp.Header.Get("X-Request-Id"))
	if row.Status != llm.UsageUpstreamError || row.CostMicro != 0 {
		t.Fatalf("usage row %+v", row)
	}
	if un := e.queryInt(`SELECT unsettled_micro FROM llm_accounts WHERE user_id = $1`, row.UserID); un != 0 {
		t.Fatalf("unsettled_micro = %d", un)
	}
}
