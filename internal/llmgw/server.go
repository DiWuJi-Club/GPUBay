// Package llmgw 是 Token 商城的 OpenAI 兼容网关（规格 §6）：/v1/models、/v1/chat/completions、/v1/completions、/v1/embeddings。
//
// 处理流程（规格 §6.1）：入口（X-Request-Id、CORS、期限）→ 401 的按 IP 限流 → 鉴权与账户门槛（llm.Authorize）→
// Content-Type / 请求体上限 / JSON 顶层字段 → Key 模型白名单 → 在线模型 → 端点与模型类型 → Key 月上限 → 用户限流（llm.Limiter）→
// 预算预检（余额 − 未结算 − 在途估算 − 欠费 ≥ 本次估算）→ 选节点（llm.Router）→ 转发（首字节前失败转移一次）→
// 非流式透传 / 流式逐事件透传并解析 usage → 记账（llm.RecordUsage）。
//
// 时间：业务时间（用量 created_at、Key 月份、限流窗口）一律取注入的 clock.Clock；
// 连接期限（SetWriteDeadline / SetReadDeadline）、超时、进程内缓存 TTL、latency_ms / ttft_ms 用真实单调时钟（time.Now / time.Since）——
// 它们作用于真实的套接字与计时，测试里的假时钟可以是静止的。
package llmgw

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/randx"
)

// ReadTimeout 入口给请求体上传的读期限（覆盖公网 http.Server 的 30 秒 ReadTimeout）；读完请求体后清除，见 setDeadlines。
const ReadTimeout = 60 * time.Second

// modelCacheTTL / transportGCEvery：在线模型表的进程内缓存时长、回收已下线节点 Transport 的间隔。
const (
	modelCacheTTL    = 2 * time.Second
	transportGCEvery = 30 * time.Second
)

// Config 网关参数；零值字段取 llm 包的默认值（MaxBodyBytes 2 MiB、TotalTimeout 10 分钟、TTFTTimeout 120 秒、AuthFailLimit 60）。
// Transport 非 nil 时用它给每个节点造 RoundTripper（单测直接指向 httptest.Server）；nil = 生产：每节点一个 http.Transport，
// DialContext 忽略地址、经 Upstream.Dial(ctx, node.Target, llm.NodePort) 走上游 SSH 通道。
type Config struct {
	MaxBodyBytes  int64
	TotalTimeout  time.Duration
	TTFTTimeout   time.Duration
	AuthFailLimit int
	Transport     func(node llm.RouteNode) http.RoundTripper
}

func (c Config) withDefaults() Config {
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = llm.MaxBodyBytes
	}
	if c.TotalTimeout <= 0 {
		c.TotalTimeout = llm.TotalTimeout
	}
	if c.TTFTTimeout <= 0 {
		c.TTFTTimeout = llm.TTFTTimeout
	}
	if c.AuthFailLimit <= 0 {
		c.AuthFailLimit = llm.AuthFailLimitPerIP
	}
	return c
}

// Server 是网关。并发安全；路由由 Router() 给出，由 api.Server 挂在 /v1。
type Server struct {
	svc     *llm.Service
	cfg     Config
	log     *slog.Logger
	clk     clock.Clock
	authLim *ipLimiter

	transports sync.Map // nodeID → http.RoundTripper
	nodeKeys   sync.Map // nodeID → string（节点密钥不变，解密一次）
	gcMu       sync.Mutex
	lastGC     time.Time

	models   modelCache
	balances balanceCache
}

func New(svc *llm.Service, cfg Config, clk clock.Clock, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if clk == nil {
		clk = clock.Real{}
	}
	cfg = cfg.withDefaults()
	return &Server{svc: svc, cfg: cfg, log: log, clk: clk, authLim: &ipLimiter{limit: cfg.AuthFailLimit}, lastGC: time.Now()}
}

// Router 返回网关的 chi 路由（路径相对 /v1）：GET /models、POST /chat/completions、/completions、/embeddings；
// OPTIONS 任意路径 → 200（CORS 预检，在入口中间件里处理）；其它 → OpenAI 格式 404 / 405。
func (g *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(g.entry)
	r.Get("/models", g.handleModels)
	r.Post("/chat/completions", g.handleCompletion(llm.EndpointChat))
	r.Post("/completions", g.handleCompletion(llm.EndpointCompletions))
	r.Post("/embeddings", g.handleCompletion(llm.EndpointEmbeddings))
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusNotFound, codeNotFound, "接口不存在")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "不支持该请求方法")
	})
	return r
}

// newRequestID 'req-' + 16 位 [a-z0-9]（llm_usage.request_id、X-Request-Id）。
func newRequestID() string { return randx.ID("req-", 16) }

// entry 是入口中间件：X-Request-Id、CORS（每个 /v1 响应都带；无凭据）、连接期限；OPTIONS 直接 200。
func (g *Server) entry(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Request-Id", newRequestID())
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusOK)
			return
		}
		g.setDeadlines(w)
		next.ServeHTTP(w, r)
	})
}

// setDeadlines 把本连接的写期限延到 now + TotalTimeout（公网 http.Server 的 60 秒 WriteTimeout 对流式不够），
// 读期限延到 now + ReadTimeout（请求体上传）。失败（如 ResponseRecorder 不支持）只记日志。
func (g *Server) setDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	now := time.Now()
	if err := rc.SetWriteDeadline(now.Add(g.cfg.TotalTimeout)); err != nil {
		g.logDeadline(err)
	}
	if err := rc.SetReadDeadline(now.Add(ReadTimeout)); err != nil {
		g.logDeadline(err)
	}
}

// clearReadDeadline 读完请求体后清除读期限：net/http 在请求体读到 EOF 后会在后台读连接（探测客户端断开），
// 读期限一到后台读就报超时并取消请求 ctx——长时间的生成会被误判成客户端断开。
func (g *Server) clearReadDeadline(w http.ResponseWriter) {
	if err := http.NewResponseController(w).SetReadDeadline(time.Time{}); err != nil {
		g.logDeadline(err)
	}
}

func (g *Server) logDeadline(err error) {
	if errors.Is(err, http.ErrNotSupported) {
		g.log.Debug("llmgw: response writer does not support deadlines", "err", err)
		return
	}
	g.log.Warn("llmgw: set connection deadline failed", "err", err)
}

// ---- 在线模型缓存（≤ 2 秒） ----

type modelCache struct {
	mu     sync.Mutex
	at     time.Time
	list   []llm.Model
	bySlug map[string]llm.Model
}

// onlineModels 返回在线模型（sort, id 序）与 slug 索引；缓存 modelCacheTTL。
func (g *Server) onlineModels(ctx context.Context) ([]llm.Model, map[string]llm.Model, error) {
	c := &g.models
	c.mu.Lock()
	if c.bySlug != nil && time.Since(c.at) < modelCacheTTL {
		list, by := c.list, c.bySlug
		c.mu.Unlock()
		return list, by, nil
	}
	c.mu.Unlock()
	list, err := g.svc.ListOnlineModels(ctx)
	if err != nil {
		return nil, nil, err
	}
	by := make(map[string]llm.Model, len(list))
	for _, m := range list {
		by[m.Slug] = m
	}
	c.mu.Lock()
	c.list, c.bySlug, c.at = list, by, time.Now()
	c.mu.Unlock()
	return list, by, nil
}

// flushCaches 仅测试：清空在线模型与余额缓存。
func (g *Server) flushCaches() {
	g.models.mu.Lock()
	g.models.bySlug = nil
	g.models.mu.Unlock()
	g.balances.m.Clear()
}

// ---- 余额缓存（每用户 BalanceCacheTTL） ----

type balanceCache struct{ m sync.Map } // userID → balanceEntry

type balanceEntry struct {
	balance int64
	at      time.Time
}

func (g *Server) balance(ctx context.Context, userID int64) (int64, error) {
	if v, ok := g.balances.m.Load(userID); ok {
		if e := v.(balanceEntry); time.Since(e.at) < llm.BalanceCacheTTL {
			return e.balance, nil
		}
	}
	b, err := g.svc.WalletBalance(ctx, userID)
	if err != nil {
		return 0, err
	}
	g.balances.m.Store(userID, balanceEntry{balance: b, at: time.Now()})
	return b, nil
}

// ---- 节点密钥与 Transport ----

func (g *Server) nodeKey(ctx context.Context, nodeID string) (string, error) {
	if v, ok := g.nodeKeys.Load(nodeID); ok {
		return v.(string), nil
	}
	k, err := g.svc.NodeKey(ctx, nodeID)
	if err != nil {
		return "", err
	}
	g.nodeKeys.Store(nodeID, k)
	return k, nil
}

// dialFailure 标记「经 Upstream.Dial 建连失败」（失败转移判定用）。
type dialFailure struct{ err error }

func (e *dialFailure) Error() string { return "llmgw: dial upstream: " + e.err.Error() }
func (e *dialFailure) Unwrap() error { return e.err }

// transport 取（或建）节点的 RoundTripper；顺带按 transportGCEvery 回收已不在路由表里的节点。
func (g *Server) transport(node llm.RouteNode) http.RoundTripper {
	g.maybeGCTransports()
	if v, ok := g.transports.Load(node.ID); ok {
		return v.(http.RoundTripper)
	}
	var rt http.RoundTripper
	if g.cfg.Transport != nil {
		rt = g.cfg.Transport(node)
	} else {
		target, pool := node.Target, g.svc.Upstream()
		rt = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, err := pool.Dial(ctx, target, llm.NodePort)
				if err != nil {
					return nil, &dialFailure{err: err}
				}
				return c, nil
			},
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: g.cfg.TTFTTimeout,
			DisableCompression:    true,
		}
	}
	v, _ := g.transports.LoadOrStore(node.ID, rt)
	return v.(http.RoundTripper)
}

// maybeGCTransports 节点掉出路由表（removed / failed / draining / unhealthy）后关闭其空闲连接并删除 Transport；
// 节点恢复 ready 时下次请求重新创建。在途请求持有的 Transport 不受影响。
func (g *Server) maybeGCTransports() {
	g.gcMu.Lock()
	if time.Since(g.lastGC) < transportGCEvery {
		g.gcMu.Unlock()
		return
	}
	g.lastGC = time.Now()
	g.gcMu.Unlock()
	router := g.svc.Router()
	g.transports.Range(func(k, v any) bool {
		if id := k.(string); !router.HasNode(id) {
			if ci, ok := v.(interface{ CloseIdleConnections() }); ok {
				ci.CloseIdleConnections()
			}
			g.transports.Delete(id)
		}
		return true
	})
}

// ---- 401 的按 IP 限流 ----

// ipLimiter 记每个 IP 最近一分钟内的 401 次数；达到 limit 后该 IP 的请求在鉴权前即 429。
type ipLimiter struct {
	mu    sync.Mutex
	limit int
	hits  map[string][]time.Time
}

func (l *ipLimiter) prune(ip string, now time.Time) []time.Time {
	ts := l.hits[ip]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= time.Minute {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.hits, ip)
	} else {
		l.hits[ip] = ts
	}
	return ts
}

// blocked 报告该 IP 是否已达上限；是则返回最早一次滑出窗口的剩余时间。
func (l *ipLimiter) blocked(ip string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		return false, 0
	}
	ts := l.prune(ip, now)
	if len(ts) < l.limit {
		return false, 0
	}
	return true, ts[len(ts)-l.limit].Add(time.Minute).Sub(now)
}

// fail 记一次 401；顺手清掉一个已过期的其它 IP（摊销 O(1)，map 不会无界增长）。
func (l *ipLimiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	ts := l.prune(ip, now)
	l.hits[ip] = append(ts, now)
	for k, v := range l.hits {
		if k != ip && (len(v) == 0 || now.Sub(v[len(v)-1]) >= time.Minute) {
			delete(l.hits, k)
			break
		}
	}
}
