package llmgw

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/llmgw/llmtest"
)

// 停机：宽限期内完成的流式请求照常计费、Shutdown 随之返回；期满仍在转发的流被取消——客户端收到 error 事件，
// 用量记 upstream_error、不收费；非流式回 503；Shutdown 之后的新请求 503。
func TestGatewayShutdownCancelsInflight(t *testing.T) {
	t.Run("finishes within grace", func(t *testing.T) {
		e := newGEnv(t)
		m := e.model("sd-ok", llm.KindChat)
		nodeID, _ := e.node(m, &llmtest.FakeVLLM{PromptTokens: 5, CompletionTokens: 5, Chunks: []string{"a", "b", "c", "d", "e"}, Delay: 40 * time.Millisecond}, "")
		_, key := e.userKey()
		resp := e.request(e.ctx, http.MethodPost, "/v1/chat/completions", key, "", chatBody("sd-ok", `"stream":true`))
		defer resp.Body.Close()
		waitFor(t, "request in flight", func() bool { return e.svc.Router().Inflight(nodeID) == 1 })
		start := time.Now()
		done := make(chan struct{})
		go func() { e.gw.Shutdown(e.ctx, 5*time.Second); close(done) }()
		got, _ := io.ReadAll(resp.Body)
		<-done
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("Shutdown waited %v for a request that finished early", d)
		}
		if !strings.Contains(string(got), "[DONE]") {
			t.Fatalf("stream not completed: %q", got)
		}
		row := e.usageRow(resp.Header.Get("X-Request-Id"))
		if row.Status != llm.UsageOK || row.CostMicro == 0 {
			t.Fatalf("usage row %+v", row)
		}
		r2, b2 := e.post("/v1/chat/completions", key, chatBody("sd-ok"))
		expectErr(t, r2, b2, http.StatusServiceUnavailable, codeServerOverloaded)
	})

	t.Run("stream cancelled after grace", func(t *testing.T) {
		e := newGEnv(t)
		m := e.model("sd-stream", llm.KindChat)
		chunks := make([]string, 100)
		for i := range chunks {
			chunks[i] = "块"
		}
		fake := &llmtest.FakeVLLM{PromptTokens: 50, CompletionTokens: 100, Chunks: chunks, Delay: 50 * time.Millisecond}
		e.node(m, fake, "")
		uid, key := e.userKey()
		resp := e.request(e.ctx, http.MethodPost, "/v1/chat/completions", key, "", chatBody("sd-stream", `"stream":true`))
		defer resp.Body.Close()
		rd := bufio.NewReader(resp.Body)
		if _, err := rd.ReadString('\n'); err != nil { // 至少收到第一个事件的首行
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { e.gw.Shutdown(e.ctx, 100*time.Millisecond); close(done) }()
		rest, _ := io.ReadAll(rd)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Shutdown did not return")
		}
		if !strings.Contains(string(rest), `"code":"upstream_error"`) || strings.Contains(string(rest), "[DONE]") {
			t.Fatalf("want an upstream_error event and no [DONE], got tail %q", rest)
		}
		row := e.usageRow(resp.Header.Get("X-Request-Id"))
		if row.Status != llm.UsageUpstreamError || row.CostMicro != 0 || row.HTTPStatus != http.StatusOK {
			t.Fatalf("usage row %+v", row)
		}
		if un := e.queryInt(`SELECT unsettled_micro FROM llm_accounts WHERE user_id = $1`, uid); un != 0 {
			t.Fatalf("unsettled_micro = %d", un)
		}
	})

	t.Run("non-stream cancelled after grace", func(t *testing.T) {
		e := newGEnv(t)
		m := e.model("sd-plain", llm.KindChat)
		nodeID, _ := e.node(m, &llmtest.FakeVLLM{PromptTokens: 5, CompletionTokens: 5, Delay: 10 * time.Second}, "")
		_, key := e.userKey()
		type res struct {
			resp *http.Response
			body []byte
		}
		ch := make(chan res, 1)
		go func() {
			r, b := e.post("/v1/chat/completions", key, chatBody("sd-plain"))
			ch <- res{r, b}
		}()
		waitFor(t, "request in flight", func() bool { return e.svc.Router().Inflight(nodeID) == 1 })
		e.gw.Shutdown(e.ctx, 50*time.Millisecond)
		var got res
		select {
		case got = <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("request not cancelled by Shutdown")
		}
		expectErr(t, got.resp, got.body, http.StatusServiceUnavailable, codeServerOverloaded)
		row := e.usageRow(got.resp.Header.Get("X-Request-Id"))
		if row.Status != llm.UsageUpstreamError || row.CostMicro != 0 || row.HTTPStatus != http.StatusServiceUnavailable {
			t.Fatalf("usage row %+v", row)
		}
	})
}
