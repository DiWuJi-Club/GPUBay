package llmgw

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/llmgw/llmtest"
)

func TestGatewayAuthMatrix(t *testing.T) {
	e := newGEnv(t)
	m := e.model("qwen3-4b", llm.KindChat)
	e.node(m, &llmtest.FakeVLLM{}, "")
	now := e.clk.Now()
	fails := 0
	expect401 := func(name, key string) {
		t.Helper()
		resp, b := e.get("/v1/models", key)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401; %s", name, resp.StatusCode, b)
		}
		expectErr(t, resp, b, http.StatusUnauthorized, codeInvalidAPIKey)
		fails++
	}
	// 无头
	resp := e.request(e.ctx, http.MethodGet, "/v1/models", "", "", "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	expectErr(t, resp, b, http.StatusUnauthorized, codeInvalidAPIKey)
	fails++
	// 坏前缀 / 长度不对 / 不存在
	expect401("bad prefix", "sk-other-"+strings.Repeat("a", 40))
	expect401("bad length", "sk-spark-abc")
	expect401("bad charset", "sk-spark-"+strings.Repeat("-", 40))
	expect401("not found", "sk-spark-"+strings.Repeat("A", 40))

	u := e.user(1000)
	disabled, kd := e.key(u)
	e.exec(`UPDATE llm_api_keys SET status = 'disabled' WHERE id = $1`, kd.ID)
	expect401("disabled", disabled)
	revoked, kr := e.key(u)
	e.exec(`UPDATE llm_api_keys SET status = 'revoked', revoked_at = $2 WHERE id = $1`, kr.ID, now)
	expect401("revoked", revoked)
	expired, ke := e.key(u)
	e.exec(`UPDATE llm_api_keys SET expires_at = $2 WHERE id = $1`, ke.ID, now.Add(-time.Second))
	expect401("expired", expired)

	gate := func(name, setup string, status int, code string) {
		t.Helper()
		uid := e.user(1000)
		k, _ := e.key(uid)
		e.exec(setup, uid)
		resp, b := e.get("/v1/models", k)
		d := expectErr(t, resp, b, status, code)
		if d.Message == "" {
			t.Fatalf("%s: empty message", name)
		}
	}
	gate("banned", `UPDATE users SET status = 'banned' WHERE id = $1`, http.StatusForbidden, "account_banned")
	gate("kyc", `UPDATE users SET kyc_status = 'none' WHERE id = $1`, http.StatusForbidden, "kyc_required")
	gate("blocked", `UPDATE llm_accounts SET blocked = true, blocked_reason = '测试' WHERE user_id = $1`, http.StatusForbidden, "account_blocked")
	gate("debt", `UPDATE llm_accounts SET debt = 5 WHERE user_id = $1`, http.StatusPaymentRequired, codeInsufficientQuota)

	// 有效 Key 正常
	_, good := e.userKey()
	if resp, b := e.get("/v1/models", good); resp.StatusCode != http.StatusOK {
		t.Fatalf("good key: %d %s", resp.StatusCode, b)
	}
	// 同一 IP 第 61 次 401 → 429（在鉴权之前判断：有效 Key 也被拒）
	for fails < llm.AuthFailLimitPerIP {
		expect401("fill", "sk-spark-"+strings.Repeat("B", 40))
	}
	resp, b = e.get("/v1/models", "sk-spark-"+strings.Repeat("C", 40))
	expectErr(t, resp, b, http.StatusTooManyRequests, codeRateLimitExceeded)
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	resp, b = e.get("/v1/models", good)
	expectErr(t, resp, b, http.StatusTooManyRequests, codeRateLimitExceeded)
	if n := e.usageCount(); n != 0 {
		t.Fatalf("auth failures recorded %d usage rows", n)
	}
}

func TestGatewayRequestValidation(t *testing.T) {
	e := newGEnv(t)
	chat := e.model("chat-a", llm.KindChat)
	emb := e.model("embed-a", llm.KindEmbedding)
	e.node(chat, &llmtest.FakeVLLM{}, "")
	e.node(emb, &llmtest.FakeVLLM{}, "")
	_, key := e.userKey()

	resp := e.request(e.ctx, http.MethodPost, "/v1/chat/completions", key, "text/plain", chatBody("chat-a"))
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	expectErr(t, resp, b, http.StatusBadRequest, codeInvalidRequest)
	// 带 charset 的 application/json 可以
	resp = e.request(e.ctx, http.MethodPost, "/v1/chat/completions", key, "application/json; charset=utf-8", chatBody("chat-a"))
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("json with charset: %d %s", resp.StatusCode, b)
	}

	big := `{"model":"chat-a","x":"` + strings.Repeat("a", 2<<20) + `"}`
	r2, b := e.post("/v1/chat/completions", key, big)
	expectErr(t, r2, b, http.StatusRequestEntityTooLarge, codeRequestTooLarge)

	for name, body := range map[string]string{
		"not json":     `{"model":`,
		"array":        `[1,2]`,
		"no model":     `{"messages":[]}`,
		"model number": `{"model":123}`,
		"model null":   `{"model":null}`,
		"stream str":   `{"model":"chat-a","stream":"yes"}`,
		"max_tokens":   `{"model":"chat-a","max_tokens":"10"}`,
	} {
		r, b := e.post("/v1/chat/completions", key, body)
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, r.StatusCode, b)
		}
		expectErr(t, r, b, http.StatusBadRequest, codeInvalidRequest)
	}

	r, b := e.post("/v1/chat/completions", key, chatBody("no-such-model"))
	expectErr(t, r, b, http.StatusNotFound, codeModelNotFound)
	// draft / offline 模型同样 404
	e.model("chat-draft", llm.KindChat, func(m map[string]any) { m["status"] = llm.ModelDraft })
	r, b = e.post("/v1/chat/completions", key, chatBody("chat-draft"))
	expectErr(t, r, b, http.StatusNotFound, codeModelNotFound)

	_, onlyChat := e.userKey(func(in *llm.KeyInput) { in.ModelSlugs = []string{"chat-a"} })
	r, b = e.post("/v1/embeddings", onlyChat, `{"model":"embed-a","input":"hi"}`)
	expectErr(t, r, b, http.StatusForbidden, codeModelNotAllowed)

	r, b = e.post("/v1/chat/completions", key, chatBody("embed-a"))
	expectErr(t, r, b, http.StatusBadRequest, codeInvalidRequest)
	r, b = e.post("/v1/embeddings", key, `{"model":"chat-a","input":"hi"}`)
	expectErr(t, r, b, http.StatusBadRequest, codeInvalidRequest)

	// 只有那一次成功的请求记了用量
	waitFor(t, "one usage row", func() bool { return e.usageCount() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := e.usageCount(); n != 1 {
		t.Fatalf("usage rows = %d, want 1 (rejected requests must not record)", n)
	}
}

func TestGatewayLimits(t *testing.T) {
	e := newGEnv(t)
	slow := e.model("slow", llm.KindChat)
	fast := e.model("fast", llm.KindChat)
	slowFake := &llmtest.FakeVLLM{Delay: 400 * time.Millisecond}
	e.node(slow, slowFake, "")
	e.node(fast, &llmtest.FakeVLLM{}, "")

	t.Run("concurrency", func(t *testing.T) {
		u, key := e.userKey()
		e.exec(`UPDATE llm_accounts SET concurrency_limit = 1 WHERE user_id = $1`, u)
		var wg sync.WaitGroup
		wg.Add(1)
		var firstStatus int
		go func() {
			defer wg.Done()
			r, _ := e.post("/v1/chat/completions", key, chatBody("slow"))
			firstStatus = r.StatusCode
		}()
		waitFor(t, "first request at node", func() bool { return slowFake.Count() == 1 })
		r, b := e.post("/v1/chat/completions", key, chatBody("fast"))
		expectErr(t, r, b, http.StatusTooManyRequests, codeRateLimitExceeded)
		if r.Header.Get("Retry-After") != "1" {
			t.Fatalf("Retry-After = %q, want 1", r.Header.Get("Retry-After"))
		}
		wg.Wait()
		if firstStatus != http.StatusOK {
			t.Fatalf("first request %d", firstStatus)
		}
	})

	t.Run("rpm", func(t *testing.T) {
		u, key := e.userKey()
		e.exec(`UPDATE llm_accounts SET rpm_limit = 2 WHERE user_id = $1`, u)
		for i := 0; i < 2; i++ {
			if r, b := e.post("/v1/chat/completions", key, chatBody("fast")); r.StatusCode != http.StatusOK {
				t.Fatalf("request %d: %d %s", i, r.StatusCode, b)
			}
		}
		r, b := e.post("/v1/chat/completions", key, chatBody("fast"))
		expectErr(t, r, b, http.StatusTooManyRequests, codeRateLimitExceeded)
		if ra := r.Header.Get("Retry-After"); ra == "" || ra == "0" {
			t.Fatalf("Retry-After = %q", ra)
		}
	})

	t.Run("monthly cap", func(t *testing.T) {
		u := e.user(100_000)
		capFen := int64(1)
		key, k := e.key(u, func(in *llm.KeyInput) { in.MonthlyCap = &capFen })
		e.exec(`UPDATE llm_api_keys SET month_ym = $2, month_cost_micro = 1000000 WHERE id = $1`, k.ID, llm.MonthOf(e.clk.Now()))
		r, b := e.post("/v1/chat/completions", key, chatBody("fast"))
		expectErr(t, r, b, http.StatusTooManyRequests, codeMonthlyCapExceeded)
	})
}

func TestGatewayBudgetPrecheck(t *testing.T) {
	e := newGEnv(t)
	m := e.model("budget", llm.KindChat)
	fake := &llmtest.FakeVLLM{Delay: 400 * time.Millisecond}
	e.node(m, fake, "")
	body := chatBody("budget")
	need := max(llm.EstimateFen(m, len(body), nil), 1)

	poor := e.user(0)
	pk, _ := e.key(poor)
	r, b := e.post("/v1/chat/completions", pk, body)
	d := expectErr(t, r, b, http.StatusPaymentRequired, codeInsufficientQuota)
	if !strings.Contains(d.Message, "余额不足") || !strings.Contains(d.Message, "可用 ¥0.00") {
		t.Fatalf("message = %q", d.Message)
	}

	rich := e.user(need)
	rk, _ := e.key(rich)
	if r, b := e.post("/v1/chat/completions", rk, body); r.StatusCode != http.StatusOK {
		t.Fatalf("enough balance: %d %s", r.StatusCode, b)
	}

	// 未结算用量也扣减可用额：unsettled 刚好吃掉一次估算
	unsettled := e.user(need)
	uk, _ := e.key(unsettled)
	e.exec(`UPDATE llm_accounts SET unsettled_micro = 1 WHERE user_id = $1`, unsettled)
	r, b = e.post("/v1/chat/completions", uk, body)
	expectErr(t, r, b, http.StatusPaymentRequired, codeInsufficientQuota)

	// 在途估算累加：余额只够一次，第一个在途时第二个 402
	one := e.user(2*need - 1)
	ok1, _ := e.key(one)
	var wg sync.WaitGroup
	wg.Add(1)
	before := fake.Count()
	var first int
	go func() {
		defer wg.Done()
		r, _ := e.post("/v1/chat/completions", ok1, body)
		first = r.StatusCode
	}()
	waitFor(t, "first request in flight", func() bool { return fake.Count() == before+1 })
	r, b = e.post("/v1/chat/completions", ok1, body)
	expectErr(t, r, b, http.StatusPaymentRequired, codeInsufficientQuota)
	wg.Wait()
	if first != http.StatusOK {
		t.Fatalf("first concurrent request: %d", first)
	}
	if got := e.svc.Limiter().InflightEstimate(one); got != 0 {
		t.Fatalf("inflight estimate after requests = %d, want 0", got)
	}
}

func TestGatewayNonStreamUsageAndPassthrough(t *testing.T) {
	e := newGEnv(t)
	m := e.model("ns-chat", llm.KindChat)
	fake := &llmtest.FakeVLLM{PromptTokens: 11, CompletionTokens: 7, Chunks: []string{"Hello", " world"}}
	nodeID, _ := e.node(m, fake, "")
	u, key := e.userKey()

	body := chatBody("ns-chat", `"temperature":0.2`)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom-Secret", "do-not-forward")
	req.Header.Set("Cookie", "sid=abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, got)
	}
	rec := fake.Recorded()
	if len(rec) != 1 {
		t.Fatalf("fake got %d requests", len(rec))
	}
	if !bytes.Equal(got, rec[0].Sent) {
		t.Fatalf("body not passed through:\n got %s\nwant %s", got, rec[0].Sent)
	}
	if !bytes.Equal(rec[0].Body, []byte(body)) {
		t.Fatalf("non-stream body rewritten: %s", rec[0].Body)
	}
	if rec[0].Path != "/v1/chat/completions" || !strings.HasPrefix(rec[0].Authorization, "Bearer nodekey-") {
		t.Fatalf("upstream path %q auth %q", rec[0].Path, rec[0].Authorization)
	}
	if rec[0].Header.Get("X-Custom-Secret") != "" || rec[0].Header.Get("Cookie") != "" {
		t.Fatalf("client headers forwarded: %v", rec[0].Header)
	}
	reqID := resp.Header.Get("X-Request-Id")
	if rec[0].Header.Get("X-Request-Id") != reqID {
		t.Fatalf("X-Request-Id not forwarded: %q vs %q", rec[0].Header.Get("X-Request-Id"), reqID)
	}
	row := e.usageRow(reqID)
	if row.UserID != u || nodeIDOf(row) != nodeID || row.HTTPStatus != 200 || row.Status != llm.UsageOK || row.Stream ||
		row.PromptTokens != 11 || row.CompletionTokens != 7 || row.Estimated || row.Endpoint != llm.EndpointChat {
		t.Fatalf("usage row %+v", row)
	}
	if want := int64(11*100 + 7*400); row.CostMicro != want {
		t.Fatalf("cost_micro = %d, want %d", row.CostMicro, want)
	}
	if row.InputPrice != 100 || row.OutputPrice != 400 || row.ClientIP != "127.0.0.1" || !row.CreatedAt.Equal(e.clk.Now()) {
		t.Fatalf("usage snapshot %+v", row)
	}
	if un := e.queryInt(`SELECT unsettled_micro FROM llm_accounts WHERE user_id = $1`, u); un != row.CostMicro {
		t.Fatalf("unsettled_micro = %d", un)
	}

	// 上游 400：原样透传，status ok、cost 0、http_status 400
	bad := e.model("ns-bad", llm.KindChat)
	badFake := &llmtest.FakeVLLM{ErrorStatus: http.StatusBadRequest}
	e.node(bad, badFake, "")
	resp2, got2 := e.post("/v1/chat/completions", key, chatBody("ns-bad"))
	if resp2.StatusCode != http.StatusBadRequest || !bytes.Equal(got2, badFake.Recorded()[0].Sent) {
		t.Fatalf("4xx passthrough: %d %s", resp2.StatusCode, got2)
	}
	row2 := e.usageRow(resp2.Header.Get("X-Request-Id"))
	if row2.Status != llm.UsageOK || row2.CostMicro != 0 || row2.HTTPStatus != 400 {
		t.Fatalf("4xx usage row %+v", row2)
	}

	// 上游 500：502 upstream_error、不收费
	five := e.model("ns-500", llm.KindChat)
	e.node(five, &llmtest.FakeVLLM{ErrorStatus: http.StatusInternalServerError}, "")
	resp3, got3 := e.post("/v1/chat/completions", key, chatBody("ns-500"))
	expectErr(t, resp3, got3, http.StatusBadGateway, codeUpstreamError)
	row3 := e.usageRow(resp3.Header.Get("X-Request-Id"))
	if row3.Status != llm.UsageUpstreamError || row3.CostMicro != 0 || row3.HTTPStatus != 502 {
		t.Fatalf("5xx usage row %+v", row3)
	}

	// embeddings：只计输入
	emb := e.model("ns-embed", llm.KindEmbedding)
	e.node(emb, &llmtest.FakeVLLM{PromptTokens: 9, CompletionTokens: 3}, "")
	resp4, got4 := e.post("/v1/embeddings", key, `{"model":"ns-embed","input":"你好"}`)
	if resp4.StatusCode != http.StatusOK || !strings.Contains(string(got4), `"embedding"`) {
		t.Fatalf("embeddings: %d %s", resp4.StatusCode, got4)
	}
	row4 := e.usageRow(resp4.Header.Get("X-Request-Id"))
	if row4.Endpoint != llm.EndpointEmbeddings || row4.PromptTokens != 9 || row4.CompletionTokens != 0 || row4.CostMicro != 900 {
		t.Fatalf("embeddings usage row %+v", row4)
	}

	// completions 端点
	resp5, got5 := e.post("/v1/completions", key, `{"model":"ns-chat","prompt":"hi"}`)
	if resp5.StatusCode != http.StatusOK || !strings.Contains(string(got5), `"text":"Hello world"`) {
		t.Fatalf("completions: %d %s", resp5.StatusCode, got5)
	}
	if row5 := e.usageRow(resp5.Header.Get("X-Request-Id")); row5.Endpoint != llm.EndpointCompletions || row5.CostMicro != 3900 {
		t.Fatalf("completions usage row %+v", row5)
	}
	if fake.Recorded()[1].Path != "/v1/completions" {
		t.Fatalf("completions path %q", fake.Recorded()[1].Path)
	}
}

func TestGatewayFailoverBeforeFirstByte(t *testing.T) {
	e := newGEnv(t)
	t.Run("503 then other node", func(t *testing.T) {
		m := e.model("fo-a", llm.KindChat)
		a := &llmtest.FakeVLLM{FailFirstN: 1}
		bf := &llmtest.FakeVLLM{PromptTokens: 3, CompletionTokens: 2}
		aID, _ := e.node(m, a, "node-aaaaaaaaaaa1")
		bID, _ := e.node(m, bf, "node-bbbbbbbbbbb1")
		_, key := e.userKey()
		resp, b := e.post("/v1/chat/completions", key, chatBody("fo-a"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d %s", resp.StatusCode, b)
		}
		if a.Count() != 1 || bf.Count() != 1 {
			t.Fatalf("node A got %d, B got %d", a.Count(), bf.Count())
		}
		if n := e.queryInt(`SELECT probe_fail_count FROM llm_nodes WHERE id = $1`, aID); n != 1 {
			t.Fatalf("A probe_fail_count = %d", n)
		}
		row := e.usageRow(resp.Header.Get("X-Request-Id"))
		if nodeIDOf(row) != bID || row.Status != llm.UsageOK || row.CostMicro != 3*100+2*400 {
			t.Fatalf("usage row %+v", row)
		}
		if a, b := e.svc.Router().Inflight(aID), e.svc.Router().Inflight(bID); a != 0 || b != 0 {
			t.Fatalf("router in-flight leaked: A %d B %d", a, b)
		}
	})
	t.Run("dial timeout then other node", func(t *testing.T) {
		m := e.model("fo-t", llm.KindChat)
		tID, _ := e.node(m, &llmtest.FakeVLLM{}, "node-ttttttttttt1")
		e.setNodeAddr(tID, dialTimeoutAddr)
		live := &llmtest.FakeVLLM{}
		uID, _ := e.node(m, live, "node-uuuuuuuuuuu1")
		_, key := e.userKey()
		resp, b := e.post("/v1/chat/completions", key, chatBody("fo-t"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d %s", resp.StatusCode, b)
		}
		if n := e.queryInt(`SELECT probe_fail_count FROM llm_nodes WHERE id = $1`, tID); n != 1 {
			t.Fatalf("timed-out node probe_fail_count = %d", n)
		}
		if row := e.usageRow(resp.Header.Get("X-Request-Id")); nodeIDOf(row) != uID {
			t.Fatalf("usage node %q", nodeIDOf(row))
		}
	})
	t.Run("connection refused then other node", func(t *testing.T) {
		m := e.model("fo-c", llm.KindChat)
		_, deadSrv := e.node(m, &llmtest.FakeVLLM{}, "node-ccccccccccc1")
		deadSrv.Close()
		live := &llmtest.FakeVLLM{}
		dID, _ := e.node(m, live, "node-ddddddddddd1")
		_, key := e.userKey()
		resp, b := e.post("/v1/chat/completions", key, chatBody("fo-c"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d %s", resp.StatusCode, b)
		}
		if n := e.queryInt(`SELECT probe_fail_count FROM llm_nodes WHERE id = 'node-ccccccccccc1'`); n != 1 {
			t.Fatalf("dead node probe_fail_count = %d", n)
		}
		if row := e.usageRow(resp.Header.Get("X-Request-Id")); nodeIDOf(row) != dID {
			t.Fatalf("usage node %q", nodeIDOf(row))
		}
	})
	t.Run("both 503", func(t *testing.T) {
		m := e.model("fo-b", llm.KindChat)
		x := &llmtest.FakeVLLM{FailFirstN: 100}
		y := &llmtest.FakeVLLM{FailFirstN: 100}
		e.node(m, x, "node-eeeeeeeeeee1")
		e.node(m, y, "node-fffffffffff1")
		_, key := e.userKey()
		resp, b := e.post("/v1/chat/completions", key, chatBody("fo-b"))
		expectErr(t, resp, b, http.StatusBadGateway, codeUpstreamError)
		if x.Count() != 1 || y.Count() != 1 {
			t.Fatalf("retried more than once: %d + %d", x.Count(), y.Count())
		}
		row := e.usageRow(resp.Header.Get("X-Request-Id"))
		if row.Status != llm.UsageUpstreamError || row.CostMicro != 0 || row.HTTPStatus != 502 {
			t.Fatalf("usage row %+v", row)
		}
	})
	t.Run("single node 503", func(t *testing.T) {
		m := e.model("fo-s", llm.KindChat)
		e.node(m, &llmtest.FakeVLLM{FailFirstN: 100}, "")
		_, key := e.userKey()
		resp, b := e.post("/v1/chat/completions", key, chatBody("fo-s"))
		expectErr(t, resp, b, http.StatusBadGateway, codeUpstreamError)
	})
}

func TestGatewayNoNodeAndBusy(t *testing.T) {
	e := newGEnv(t)
	e.model("no-node", llm.KindChat)
	_, key := e.userKey()
	resp, b := e.post("/v1/chat/completions", key, chatBody("no-node"))
	d := expectErr(t, resp, b, http.StatusServiceUnavailable, codeModelUnavailable)
	if d.Message != "模型暂无可用节点，请稍后再试" {
		t.Fatalf("message %q", d.Message)
	}
	row := e.usageRow(resp.Header.Get("X-Request-Id"))
	if row.Status != llm.UsageNoNode || row.HTTPStatus != 503 || row.NodeID != nil || row.CostMicro != 0 {
		t.Fatalf("no-node usage row %+v", row)
	}

	busy := e.model("busy", llm.KindChat, func(m map[string]any) { m["max_concurrency"] = 1 })
	fake := &llmtest.FakeVLLM{Delay: 400 * time.Millisecond}
	e.node(busy, fake, "")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.post("/v1/chat/completions", key, chatBody("busy"))
	}()
	waitFor(t, "first request at node", func() bool { return fake.Count() == 1 })
	_, key2 := e.userKey()
	resp, b = e.post("/v1/chat/completions", key2, chatBody("busy"))
	expectErr(t, resp, b, http.StatusServiceUnavailable, codeServerOverloaded)
	if resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	if row := e.usageRow(resp.Header.Get("X-Request-Id")); row.Status != llm.UsageNoNode || row.HTTPStatus != 503 {
		t.Fatalf("busy usage row %+v", row)
	}
	wg.Wait()
}

func TestGatewayModelsAndCORS(t *testing.T) {
	e := newGEnv(t)
	a := e.model("model-a", llm.KindChat)
	e.model("model-b", llm.KindChat)
	e.model("embed-x", llm.KindEmbedding)
	e.model("draft-z", llm.KindChat, func(m map[string]any) { m["status"] = llm.ModelDraft })
	_, all := e.userKey()
	_, onlyA := e.userKey(func(in *llm.KeyInput) { in.ModelSlugs = []string{"model-a"} })

	list := func(key string) modelsResponse {
		t.Helper()
		resp, b := e.get("/v1/models", key)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("models: %d %s", resp.StatusCode, b)
		}
		var out modelsResponse
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := list(all)
	ids := []string{}
	for _, d := range got.Data {
		ids = append(ids, d.ID)
		if d.Object != "model" || d.OwnedBy != "spark-cloud" {
			t.Fatalf("model object %+v", d)
		}
	}
	if got.Object != "list" || strings.Join(ids, ",") != "model-a,model-b,embed-x" {
		t.Fatalf("models = %+v", got)
	}
	if got.Data[0].Created != a.CreatedAt.Unix() {
		t.Fatalf("created = %d, want %d", got.Data[0].Created, a.CreatedAt.Unix())
	}
	if got := list(onlyA); len(got.Data) != 1 || got.Data[0].ID != "model-a" {
		t.Fatalf("filtered models = %+v", got)
	}

	// CORS 预检：无需鉴权
	req, _ := http.NewRequest(http.MethodOptions, e.srv.URL+"/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), "POST") ||
		resp.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	preflight := func(reqHeaders string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodOptions, e.srv.URL+"/v1/chat/completions", nil)
		req.Header.Set("Origin", "https://example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", reqHeaders)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Access-Control-Allow-Headers")
	}
	if got := preflight("authorization,x-stainless-os"); got != "authorization, x-stainless-os" {
		t.Fatalf("echoed allow headers = %q", got)
	}
	if got := preflight("authorization, bad header(), x"); got != defaultAllowHeaders || !strings.Contains(got, "X-Stainless-OS") ||
		!strings.Contains(got, "OpenAI-Organization") {
		t.Fatalf("fallback allow headers = %q", got)
	}

	// 未知路径：404 OpenAI 格式，也带 CORS 与 X-Request-Id
	r, b := e.get("/v1/no/such/path", all)
	expectErr(t, r, b, http.StatusNotFound, codeNotFound)
	if r.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("404 without CORS header")
	}
	if !regexp.MustCompile(`^req-[a-z0-9]{16}$`).MatchString(r.Header.Get("X-Request-Id")) {
		t.Fatalf("X-Request-Id = %q", r.Header.Get("X-Request-Id"))
	}
	r, b = e.get("/v1/chat/completions", all)
	expectErr(t, r, b, http.StatusMethodNotAllowed, codeMethodNotAllowed)
	r, b = e.post("/v1/files", all, `{}`)
	expectErr(t, r, b, http.StatusNotFound, codeNotFound)
}

// deadlineWriter 是支持 SetWriteDeadline / SetReadDeadline 的 ResponseWriter（经 http.ResponseController 调用）。
type deadlineWriter struct {
	*httptest.ResponseRecorder
	mu    sync.Mutex
	write []time.Time
	read  []time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.write = append(d.write, t)
	return nil
}

func (d *deadlineWriter) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.read = append(d.read, t)
	return nil
}

func TestGatewayDeadlinesExtended(t *testing.T) {
	e := newGEnv(t)
	m := e.model("dl", llm.KindChat)
	e.node(m, &llmtest.FakeVLLM{PromptTokens: 1, CompletionTokens: 1}, "")
	_, key := e.userKey()
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(chatBody("dl")))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	dw := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	before := time.Now()
	e.gw.Router().ServeHTTP(dw, req)
	if dw.Code != http.StatusOK {
		t.Fatalf("status %d %s", dw.Code, dw.Body.String())
	}
	dw.mu.Lock()
	defer dw.mu.Unlock()
	if len(dw.write) == 0 || dw.write[0].Before(before.Add(llm.TotalTimeout+WriteGrace)) {
		t.Fatalf("write deadlines %v, want ≥ now + TotalTimeout + WriteGrace", dw.write)
	}
	if len(dw.read) < 2 || dw.read[0].Before(before.Add(50*time.Second)) || !dw.read[len(dw.read)-1].IsZero() {
		t.Fatalf("read deadlines %v, want now + 60s then cleared after body read", dw.read)
	}
}

func TestGatewayTimeouts(t *testing.T) {
	e := newGEnv(t, func(c *Config) { c.TTFTTimeout = 150 * time.Millisecond; c.TotalTimeout = 1500 * time.Millisecond })
	m := e.model("to-chat", llm.KindChat)
	slowHeaders := &llmtest.FakeVLLM{HeaderDelay: 400 * time.Millisecond, PromptTokens: 1, CompletionTokens: 1, Chunks: []string{"x"}}
	e.node(m, slowHeaders, "")
	_, key := e.userKey()

	// 非流式不受首字节超时约束（vLLM 生成完才发响应头），只受总超时
	if r, b := e.post("/v1/chat/completions", key, chatBody("to-chat")); r.StatusCode != http.StatusOK {
		t.Fatalf("non-stream with slow headers: %d %s", r.StatusCode, b)
	}
	// 流式：首字节超时 → 504 upstream_timeout、不收费
	r, b := e.post("/v1/chat/completions", key, chatBody("to-chat", `"stream":true`))
	expectErr(t, r, b, http.StatusGatewayTimeout, codeUpstreamTimeout)
	if row := e.usageRow(r.Header.Get("X-Request-Id")); row.Status != llm.UsageUpstreamTimeout || row.CostMicro != 0 || row.HTTPStatus != 504 {
		t.Fatalf("ttft usage row %+v", row)
	}

	// 流式进行中总超时：写期限多留 WriteGrace，error 事件仍写得出去；upstream_timeout 不收费
	long := e.model("to-long", llm.KindChat)
	chunks := make([]string, 50)
	for i := range chunks {
		chunks[i] = "z"
	}
	e.node(long, &llmtest.FakeVLLM{Chunks: chunks, Delay: 100 * time.Millisecond}, "")
	r, got := e.post("/v1/chat/completions", key, chatBody("to-long", `"stream":true`))
	if r.StatusCode != http.StatusOK || !strings.Contains(string(got), `"code":"upstream_timeout"`) {
		t.Fatalf("total timeout stream: %d tail %q", r.StatusCode, got[max(0, len(got)-200):])
	}
	if row := e.usageRow(r.Header.Get("X-Request-Id")); row.Status != llm.UsageUpstreamTimeout || row.CostMicro != 0 {
		t.Fatalf("total timeout usage row %+v", row)
	}
}

func TestGatewayBudgetCountsRecentUsage(t *testing.T) {
	e := newGEnv(t)
	// 每个输出 token 1 分；请求 max_tokens 1 → 预估 1 分；上游实际 5 个输出 token → 5 分
	m := e.model("bud-recent", llm.KindChat, func(f map[string]any) { f["input_price"] = 0; f["output_price"] = 1_000_000 })
	e.node(m, &llmtest.FakeVLLM{CompletionTokens: 5, Chunks: []string{"x"}}, "")
	u := e.user(5)
	key, _ := e.key(u)
	body := chatBody("bud-recent", `"max_tokens":1`)
	r, b := e.post("/v1/chat/completions", key, body)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("first: %d %s", r.StatusCode, b)
	}
	if row := e.usageRow(r.Header.Get("X-Request-Id")); row.CostMicro != 5_000_000 {
		t.Fatalf("first cost %d", row.CostMicro)
	}
	// Key 缓存里的 unsettled 仍是 0；本进程刚记的 5 分要扣掉 → 可用 0 < 1
	r, b = e.post("/v1/chat/completions", key, body)
	d := expectErr(t, r, b, http.StatusPaymentRequired, codeInsufficientQuota)
	if !strings.Contains(d.Message, "可用 ¥0.00") {
		t.Fatalf("message %q", d.Message)
	}
	// 缓存重新装载后（unsettled 已含这 5 分）不重复扣减
	e.svc.InvalidateUser(u)
	e.exec(`UPDATE llm_accounts SET unsettled_micro = 0 WHERE user_id = $1`, u) // 模拟已结算：余额另行扣过
	r, b = e.post("/v1/chat/completions", key, body)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("after reload: %d %s", r.StatusCode, b)
	}
}

func TestGatewayNodeKeyRejected(t *testing.T) {
	e := newGEnv(t)
	m := e.model("nk-chat", llm.KindChat)
	fake := &llmtest.FakeVLLM{}
	id, _ := e.node(m, fake, "")
	var good []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT api_key_enc FROM llm_nodes WHERE id = $1`, id).Scan(&good); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE llm_nodes SET api_key_enc = $2 WHERE id = $1`, id, e.box.Seal([]byte("wrong-node-key")))
	_, key := e.userKey()
	r, b := e.post("/v1/chat/completions", key, chatBody("nk-chat"))
	expectErr(t, r, b, http.StatusBadGateway, codeUpstreamError)
	if row := e.usageRow(r.Header.Get("X-Request-Id")); row.Status != llm.UsageUpstreamError || row.CostMicro != 0 {
		t.Fatalf("usage row %+v", row)
	}
	if _, ok := e.gw.nodeKeys.Load(id); ok {
		t.Fatal("rejected node key still cached")
	}
	// 密钥修正后下一次请求重新解密、成功
	e.exec(`UPDATE llm_nodes SET api_key_enc = $2 WHERE id = $1`, id, good)
	if r, b := e.post("/v1/chat/completions", key, chatBody("nk-chat")); r.StatusCode != http.StatusOK {
		t.Fatalf("after fix: %d %s", r.StatusCode, b)
	}
}
