package llmgw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/httpx"
)

// statusClientClosed 客户端在网关返回之前断开（非流式）时记入 llm_usage.http_status 的值（nginx 惯例 499）。
const statusClientClosed = 499

// recordTimeout 记账事务的上限（记账 ctx 脱离请求 ctx，客户端断开后也要记上）。
const recordTimeout = 10 * time.Second

// upstreamPath 端点 → 节点 vLLM 的路径（与客户端路径相同；不依赖挂载前缀）。
var upstreamPath = map[string]string{
	llm.EndpointChat:        "/v1/chat/completions",
	llm.EndpointCompletions: "/v1/completions",
	llm.EndpointEmbeddings:  "/v1/embeddings",
}

// call 是一次已通过鉴权、校验、限流与预检、进入选节点阶段的请求。
type call struct {
	g        *Server
	w        http.ResponseWriter
	r        *http.Request
	reqID    string
	ac       llm.AuthContext
	model    llm.Model
	endpoint string
	p        parsedRequest
	body     []byte // 客户端原始请求体（估算 prompt 用）
	upBody   []byte // 转发给节点的请求体（可能注入了 stream_options）
	ip       string

	start     time.Time // 单调时钟：latency_ms / ttft_ms
	createdAt time.Time // 业务时钟：llm_usage.created_at
}

// outcome 是一次请求的记账结果。
type outcome struct {
	node              *llm.RouteNode
	status            string // llm.Usage*
	httpStatus        int
	prompt, completed int
	estimated         bool
	charge            bool // false → cost_micro 0（上游出错 / 4xx 透传）
	ttft              *int
}

func isJSONContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

// handleCompletion 规格 §6.1：三个 POST 端点共用。
func (g *Server) handleCompletion(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		createdAt := g.clk.Now()
		reqID := w.Header().Get("X-Request-Id")
		ac, ok := g.authenticate(w, r)
		if !ok {
			return
		}
		// Content-Type 在鉴权之后检查（规格 §6.1 把它写在第 1 步）：未鉴权的请求一律先回 401，保留这个顺序。
		if !isJSONContentType(r.Header.Get("Content-Type")) {
			writeErr(w, http.StatusBadRequest, codeInvalidRequest, "Content-Type 必须是 application/json")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.cfg.MaxBodyBytes))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeErr(w, http.StatusRequestEntityTooLarge, codeRequestTooLarge,
					fmt.Sprintf("请求体超过上限 %d MiB", g.cfg.MaxBodyBytes>>20))
				return
			}
			writeErr(w, http.StatusBadRequest, codeInvalidRequest, "读取请求体失败")
			return
		}
		g.clearReadDeadline(w)
		p, msg := parseRequest(body)
		if msg != "" {
			writeErr(w, http.StatusBadRequest, codeInvalidRequest, msg)
			return
		}
		if endpoint == llm.EndpointEmbeddings {
			p.stream = false // embeddings 没有流式
		}
		if !ac.Key.KeyAllowsModel(p.model) {
			writeErr(w, http.StatusForbidden, codeModelNotAllowed, fmt.Sprintf("该 API Key 不允许调用模型 %s", p.model))
			return
		}
		_, bySlug, err := g.onlineModels(r.Context())
		if err != nil {
			g.log.Error("llmgw: list online models failed", "err", err, "request_id", reqID)
			writeErr(w, http.StatusInternalServerError, codeInternal, "服务内部错误，请稍后再试")
			return
		}
		m, ok := bySlug[p.model]
		if !ok {
			writeErr(w, http.StatusNotFound, codeModelNotFound, fmt.Sprintf("模型 %s 不存在或未上线", p.model))
			return
		}
		if (endpoint == llm.EndpointEmbeddings) != (m.Kind == llm.KindEmbedding) {
			if m.Kind == llm.KindEmbedding {
				writeErr(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("模型 %s 是向量模型，请调用 /v1/embeddings", m.Slug))
			} else {
				writeErr(w, http.StatusBadRequest, codeInvalidRequest, fmt.Sprintf("模型 %s 不是向量模型，不能调用 /v1/embeddings", m.Slug))
			}
			return
		}
		now := g.clk.Now()
		if ac.Key.MonthCapExceeded(now) {
			writeErr(w, http.StatusTooManyRequests, codeMonthlyCapExceeded, "该 API Key 本月消费已达上限")
			return
		}
		maxOut := 0
		if m.Kind != llm.KindEmbedding {
			maxOut = m.DefaultMaxTokens
			if p.maxTokens != nil && *p.maxTokens > 0 {
				maxOut = *p.maxTokens
			}
			maxOut = min(maxOut, m.MaxOutputTokens)
		}
		lim := g.svc.Limiter()
		releaseLimit, err := lim.Acquire(ac.UserID, ac.Limits, len(body)/3+maxOut, now)
		if err != nil {
			var le *llm.LimitError
			if errors.As(err, &le) {
				setRetryAfter(w, le.RetryAfter)
				writeErr(w, http.StatusTooManyRequests, le.Code, le.Message)
				return
			}
			writeErr(w, http.StatusInternalServerError, codeInternal, "服务内部错误，请稍后再试")
			return
		}
		defer releaseLimit()

		// 预算预检（规格 T2）：available = balance − ceil(unsettled/1e6) − 在途估算 − debt ≥ max(est, 1)
		bal, err := g.balance(r.Context(), ac.UserID)
		if err != nil {
			g.log.Error("llmgw: read wallet balance failed", "err", err, "request_id", reqID, "user_id", ac.UserID)
			writeErr(w, http.StatusInternalServerError, codeInternal, "服务内部错误，请稍后再试")
			return
		}
		need := max(llm.EstimateFen(m, len(body), p.maxTokens), 1)
		// 缓存的 unsettled_micro 最多落后 KeyCacheTTL：补上装载之后本进程新记的用量
		unsettled := ac.Account.UnsettledMicro + g.recorded.since(ac.UserID, ac.LoadSeq)
		budget := bal - (unsettled+llm.MicroPerFen-1)/llm.MicroPerFen - ac.Account.Debt
		releaseEst, avail, ok := lim.TryReserveEstimate(ac.UserID, need, budget)
		if !ok {
			writeErr(w, http.StatusPaymentRequired, codeInsufficientQuota,
				fmt.Sprintf("余额不足，本次预估最多 ¥%s，可用 ¥%s", yuan(need), yuan(max(avail, 0))))
			return
		}
		defer releaseEst()

		upBody, err := upstreamBody(body, p)
		if err != nil {
			writeErr(w, http.StatusBadRequest, codeInvalidRequest, "stream_options 必须是对象")
			return
		}
		c := &call{g: g, w: w, r: r, reqID: reqID, ac: ac, model: m, endpoint: endpoint, p: p, body: body, upBody: upBody,
			ip: httpx.ClientIP(r), start: start, createdAt: createdAt}
		c.run()
	}
}

// yuan 分 → 「12.34」。
func yuan(fen int64) string { return fmt.Sprintf("%d.%02d", fen/100, fen%100) }

// run 选节点、转发（首字节前失败转移一次）、响应与记账。
func (c *call) run() {
	g := c.g
	// 与入口的写期限同一截止时刻（写期限另加 WriteGrace）：总超时先到，还来得及写错误并记 upstream_timeout
	ctx, cancel := context.WithDeadline(c.r.Context(), g.requestDeadline(c.r))
	defer cancel()
	// 停机宽限期满时取消转发（cause = errShutdown，见 Server.Shutdown）
	ctx, cancelShutdown := context.WithCancelCause(ctx)
	defer cancelShutdown(nil)
	defer context.AfterFunc(g.stopCtx, func() { cancelShutdown(errShutdown) })()
	router := g.svc.Router()
	exclude := map[string]bool{}
	var failed *llm.RouteNode
	// attempt 一次选节点 + 转发；返回 true 表示请求已处理完。节点在途名额用 defer 归还（panic 也不泄漏）。
	attempt := func() bool {
		node, release, err := router.PickAcquire(c.model.ID, c.ac.Key.ID, exclude)
		if err != nil {
			if failed != nil {
				// 失败转移时没有别的可用节点
				c.fail(outcome{node: failed, status: llm.UsageUpstreamError}, http.StatusBadGateway, codeUpstreamError, "上游节点出错，请稍后再试")
			} else {
				c.noNode(err)
			}
			return true
		}
		defer release()
		up, err := c.send(ctx, node)
		if c.failover(ctx, up, err) {
			g.noteFailure(node.ID, up, err, c.reqID)
			exclude[node.ID] = true
			failed = &node
			return false
		}
		c.respond(ctx, node, up, err)
		return true
	}
	for i := 0; i < 2; i++ {
		if attempt() {
			return
		}
	}
	c.fail(outcome{node: failed, status: llm.UsageUpstreamError}, http.StatusBadGateway, codeUpstreamError, "上游节点出错，请稍后再试")
}

// noNode 规格 §6.1 第 7 步：没有 ready 节点 → 503 model_unavailable；全部满载 → 503 server_overloaded（Retry-After 2）。
func (c *call) noNode(err error) {
	if errors.Is(err, llm.ErrAllBusy) {
		setRetryAfter(c.w, 2*time.Second)
		c.fail(outcome{status: llm.UsageNoNode}, http.StatusServiceUnavailable, codeServerOverloaded, "模型繁忙，请稍后再试")
		return
	}
	c.fail(outcome{status: llm.UsageNoNode}, http.StatusServiceUnavailable, codeModelUnavailable, "模型暂无可用节点，请稍后再试")
}

// fail 写 OpenAI 错误并记账（cost 0）。
func (c *call) fail(o outcome, status int, code, msg string) {
	writeErr(c.w, status, code, msg)
	o.httpStatus = status
	c.record(o)
}

// upstreamResp 是一次转发的结果：resp 为上游响应（err == nil 时），cancel 取消这次上游请求，timedOut 报告首字节超时。
type upstreamResp struct {
	resp     *http.Response
	cancel   context.CancelFunc
	ttftHit  *atomic.Bool
	internal bool // 网关自身错误（如节点密钥解密失败），不失败转移
}

// send 把请求转发给节点：方法 / 路径不变，头只带 Content-Type、Accept、Authorization（节点密钥）、X-Request-Id。
//
// 首字节超时 TTFTTimeout 只用于流式请求（生产流式 Transport 的 ResponseHeaderTimeout + 这里的计时器兜底，注入的 Transport 也生效）。
// 与规格 §6.1 第 8 步不同：vLLM 的非流式响应要等整段生成完才发响应头，对它套 120 秒首字节超时会把长生成误杀；
// 非流式只受总超时（TotalTimeout）约束。
func (c *call) send(ctx context.Context, node llm.RouteNode) (upstreamResp, error) {
	g := c.g
	key, err := g.nodeKey(ctx, node.ID)
	if err != nil {
		return upstreamResp{internal: true}, fmt.Errorf("node key %s: %w", node.ID, err)
	}
	upCtx, upCancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(upCtx, http.MethodPost, "http://"+node.ID+upstreamPath[c.endpoint], bytes.NewReader(c.upBody))
	if err != nil {
		upCancel()
		return upstreamResp{internal: true}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a := c.r.Header.Get("Accept"); a != "" {
		req.Header.Set("Accept", a)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Request-Id", c.reqID)
	hit := &atomic.Bool{}
	var timer *time.Timer
	if c.p.stream {
		timer = time.AfterFunc(g.cfg.TTFTTimeout, func() {
			hit.Store(true)
			upCancel()
		})
	}
	resp, err := g.transport(node, c.p.stream).RoundTrip(req)
	if timer != nil && !timer.Stop() && err == nil {
		// 计时器恰在响应头到达时触发：上游 ctx 已取消，按首字节超时处理
		resp.Body.Close()
		resp, err = nil, context.DeadlineExceeded
	}
	if err != nil {
		upCancel()
		return upstreamResp{cancel: upCancel, ttftHit: hit}, err
	}
	return upstreamResp{resp: resp, cancel: upCancel, ttftHit: hit}, nil
}

// failover 规格 §6.1 第 9 步：尚未向客户端写任何字节，且是连接层错误（建连失败——包括拨号超时 / 连接被拒 / 复用的连接被对端关闭）
// 或上游 502 / 503 → 换节点。客户端已断开、总超时已到、首字节超时、网关内部错误不换。
func (c *call) failover(ctx context.Context, up upstreamResp, err error) bool {
	if c.r.Context().Err() != nil || ctx.Err() != nil || up.internal {
		return false
	}
	if err == nil {
		s := up.resp.StatusCode
		return s == http.StatusBadGateway || s == http.StatusServiceUnavailable
	}
	if up.ttftHit != nil && up.ttftHit.Load() {
		return false
	}
	// 建连失败（含 SSH 拨号超时）先判：它也是 net.Error Timeout，但节点不可达就该换节点
	var df *dialFailure
	var de *upstream.DialError
	var oe *net.OpError
	switch {
	case errors.As(err, &df), errors.As(err, &de):
		return true
	case errors.As(err, &oe) && oe.Op == "dial":
		return true
	case isTimeout(err):
		return false
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	return false
}

// noteFailure 失败转移：关闭上游响应、给节点记一次失败（probe_fail_count + 1）。
func (g *Server) noteFailure(nodeID string, up upstreamResp, err error, reqID string) {
	if up.resp != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(up.resp.Body, 64<<10))
		up.resp.Body.Close()
		g.log.Warn("llmgw: upstream unavailable, failing over", "node_id", nodeID, "status", up.resp.StatusCode, "request_id", reqID)
	} else {
		g.log.Warn("llmgw: upstream connect failed, failing over", "node_id", nodeID, "err", err, "request_id", reqID)
	}
	if up.cancel != nil {
		up.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := g.svc.NoteNodeFailure(ctx, nodeID); e != nil {
		g.log.Error("llmgw: note node failure failed", "node_id", nodeID, "err", e)
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// respond 处理不再失败转移的转发结果。
func (c *call) respond(ctx context.Context, node llm.RouteNode, up upstreamResp, err error) {
	defer func() {
		if up.cancel != nil {
			up.cancel()
		}
	}()
	n := node
	if err != nil {
		c.transportError(ctx, &n, up, err)
		return
	}
	if c.p.stream && up.resp.StatusCode == http.StatusOK {
		c.stream(ctx, &n, up)
		return
	}
	c.nonStream(ctx, &n, up)
}

// transportError 没拿到上游响应：客户端断开 → client_abort；停机取消 → 503（upstream_error，不收费）；超时 → 504 upstream_timeout；其它 → 502 upstream_error。
func (c *call) transportError(ctx context.Context, node *llm.RouteNode, up upstreamResp, err error) {
	switch {
	case c.r.Context().Err() != nil:
		c.record(c.abortOutcome(node, statusClientClosed, 0, nil))
	case shutDown(ctx):
		setRetryAfter(c.w, 2*time.Second)
		c.fail(outcome{node: node, status: llm.UsageUpstreamError}, http.StatusServiceUnavailable, codeServerOverloaded, "服务正在重启，本次请求不收费，请重试")
	case up.ttftHit != nil && up.ttftHit.Load(), isTimeout(err), ctx.Err() != nil:
		c.fail(outcome{node: node, status: llm.UsageUpstreamTimeout}, http.StatusGatewayTimeout, codeUpstreamTimeout, "上游节点响应超时，请稍后再试")
	default:
		if up.internal {
			c.g.log.Error("llmgw: forward failed", "err", err, "request_id", c.reqID)
		} else {
			c.g.log.Warn("llmgw: upstream request failed", "node_id", node.ID, "err", err, "request_id", c.reqID)
		}
		c.fail(outcome{node: node, status: llm.UsageUpstreamError}, http.StatusBadGateway, codeUpstreamError, "上游节点出错，请稍后再试")
	}
}

// abortOutcome 客户端中断（规格 T12）：输入 = len(body)/3、输出 = 已转发内容块数，estimated，照常计费。
func (c *call) abortOutcome(node *llm.RouteNode, httpStatus, chunks int, ttft *int) outcome {
	completion := chunks
	if c.model.Kind == llm.KindEmbedding {
		completion = 0
	}
	return outcome{node: node, status: llm.UsageClientAbort, httpStatus: httpStatus, prompt: len(c.body) / 3,
		completed: completion, estimated: true, charge: true, ttft: ttft}
}

// usageJSON 是上游响应 / SSE 块里的 usage。
type usageJSON struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// nonStream 规格 §6.1 第 10 步（非流式，及流式请求但上游非 200）：读完上游 body（≤ 8 MiB）。
// 2xx → 透传并按 usage 计费；4xx → 原样透传、status ok、cost 0；其它 → 502 upstream_error。
func (c *call) nonStream(ctx context.Context, node *llm.RouteNode, up upstreamResp) {
	resp := up.resp
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, llm.MaxUpstreamBodyBytes+1))
	if err != nil {
		c.transportError(ctx, node, up, err)
		return
	}
	if len(b) > llm.MaxUpstreamBodyBytes {
		c.g.log.Warn("llmgw: upstream body too large", "node_id", node.ID, "request_id", c.reqID)
		c.fail(outcome{node: node, status: llm.UsageUpstreamError}, http.StatusBadGateway, codeUpstreamError, "上游响应过大")
		return
	}
	s := resp.StatusCode
	switch {
	case s == http.StatusUnauthorized:
		// 节点拒绝了节点密钥：平台故障（密钥不一致），不是用户的错——丢掉缓存的密钥、502，不透传
		c.g.nodeKeys.Delete(node.ID)
		c.g.log.Error("llmgw: node rejected node key", "node_id", node.ID, "request_id", c.reqID)
		c.fail(outcome{node: node, status: llm.UsageUpstreamError}, http.StatusBadGateway, codeUpstreamError, "上游节点出错，请稍后再试")
	case s >= 200 && s < 300:
		o := outcome{node: node, status: llm.UsageOK, httpStatus: s, charge: true}
		var parsed struct {
			Usage *usageJSON `json:"usage"`
		}
		if json.Unmarshal(b, &parsed) == nil && parsed.Usage != nil {
			o.prompt, o.completed = parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens
		} else {
			// 上游没给 usage（理论不会）：按字节估算
			o.prompt, o.completed, o.estimated = len(c.body)/3, len(b)/3, true
		}
		if c.model.Kind == llm.KindEmbedding {
			o.completed = 0
		}
		c.passthrough(resp, b)
		c.record(o)
		c.g.svc.Router().Remember(c.ac.Key.ID, node.ID)
	case s >= 400 && s < 500:
		c.passthrough(resp, b)
		c.record(outcome{node: node, status: llm.UsageOK, httpStatus: s})
	default:
		c.g.log.Warn("llmgw: upstream error status", "node_id", node.ID, "status", s, "request_id", c.reqID)
		c.fail(outcome{node: node, status: llm.UsageUpstreamError}, http.StatusBadGateway, codeUpstreamError, "上游节点出错，请稍后再试")
	}
}

// passthrough 原样写回上游的状态码、Content-Type 与 body。
func (c *call) passthrough(resp *http.Response, b []byte) {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	c.w.Header().Set("Content-Type", ct)
	c.w.WriteHeader(resp.StatusCode)
	_, _ = c.w.Write(b)
}

// record 写 llm_usage（规格 §6.1 第 11 步、§6.4）并把实际 token 记入 TPM 窗口。记账失败只记日志 + 严重告警，不影响响应。
func (c *call) record(o outcome) {
	g := c.g
	var cost int64
	if o.charge {
		cost = llm.Cost(c.model, o.prompt, o.completed)
	}
	u := llm.Usage{RequestID: c.reqID, UserID: c.ac.UserID, KeyID: c.ac.Key.ID, ModelID: c.model.ID, Endpoint: c.endpoint,
		Stream: c.p.stream, PromptTokens: o.prompt, CompletionTokens: o.completed, Estimated: o.estimated,
		InputPrice: c.model.InputPrice, OutputPrice: c.model.OutputPrice, CostMicro: cost, Status: o.status,
		HTTPStatus: o.httpStatus, LatencyMS: int(time.Since(c.start).Milliseconds()), TTFTMS: o.ttft, ClientIP: c.ip,
		CreatedAt: c.createdAt}
	if o.node != nil {
		id := o.node.ID
		u.NodeID = &id
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.r.Context()), recordTimeout)
	defer cancel()
	if err := g.svc.RecordUsage(ctx, u); err != nil {
		if errors.Is(err, llm.ErrDuplicateUsage) {
			g.log.Warn("llmgw: duplicate usage request_id", "request_id", c.reqID)
		} else {
			g.log.Error("llm: record usage failed", "err", err, "request_id", c.reqID, "user_id", u.UserID, "key_id", u.KeyID,
				"model_id", u.ModelID, "status", u.Status, "prompt_tokens", u.PromptTokens, "completion_tokens", u.CompletionTokens,
				"cost_micro", u.CostMicro)
			// 告警用自己的短超时 ctx：记账可能正是因为超时失败的
			actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
			if e := g.svc.AlertUsageRecordFailed(actx); e != nil {
				g.log.Error("llmgw: raise usage record alert failed", "err", e)
			}
			acancel()
		}
	} else if u.Status == llm.UsageOK || u.Status == llm.UsageClientAbort {
		// 记账后取装载序号：序号 ≥ 它的缓存条目装载时这笔还没提交（或在边界上），预检要另扣
		g.recorded.add(u.UserID, g.svc.AuthLoadSeq(), u.CostMicro)
	}
	g.svc.Limiter().AddTokens(c.ac.UserID, o.prompt+o.completed, g.clk.Now())
}
