package ingress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
)

const (
	sessionCacheTTL  = 2 * time.Second
	sessionCacheMax  = 10000
	unreachThrottle  = 5 * time.Minute
	responseHeaderTO = 60 * time.Second
	maxIdlePerHost   = 8
	maxConnsPerHost  = 256 // 每个实例端口的并发上游通道上限：单个实例不能耗尽资源
	alertTimeout     = 5 * time.Second
)

// SessionChecker 查询控制台会话对应的用户（*identity.Service 满足）。
type SessionChecker interface {
	SessionUserID(ctx context.Context, hash string) (int64, error)
}

// Dialer 打开到容器内 127.0.0.1:<port> 的通道（*upstream.Pool 满足）。
type Dialer interface {
	Dial(ctx context.Context, t upstream.Target, port int) (net.Conn, error)
}

// Alerts 上报上游不可达（*rental.Service 满足）。
type Alerts interface {
	RaiseUpstreamUnreachable(ctx context.Context, instanceID string) error
}

// Server 是租户 Web 端口的反向代理：按 Host 分流，私有端口校验每实例 Cookie（绑定控制台会话），
// 经共享上游 SSH 连接的 direct-tcpip 通道转发。
type Server struct {
	auth   *Auth
	routes *Table
	sess   SessionChecker
	dialer Dialer
	alerts Alerts
	clk    clock.Clock
	log    *slog.Logger
	tr     *http.Transport
	proxy  *httputil.ReverseProxy

	mu          sync.Mutex
	sessCache   map[string]cachedSession
	lastUnreach map[string]time.Time
	dialedAt    map[string]string // 实例 → 最近一次转发所用的 lan_ip|sshd 端口|主机公钥；变化时丢弃 Transport 空闲连接
	streams     map[*stream]struct{}
	closing     bool // Shutdown 之后不再接受新的转发

	deadlineOnce sync.Once
}

type cachedSession struct {
	userID int64 // 0 = 会话无效
	until  time.Time
}

type ctxKey struct{}

type streamKey struct{}

func NewServer(auth *Auth, routes *Table, sessions SessionChecker, dialer Dialer, alerts Alerts, clk clock.Clock, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{auth: auth, routes: routes, sess: sessions, dialer: dialer, alerts: alerts, clk: clk, log: log,
		sessCache: map[string]cachedSession{}, lastUnreach: map[string]time.Time{}, dialedAt: map[string]string{}, streams: map[*stream]struct{}{}}
	s.tr = &http.Transport{
		// 出站地址是 http://<instance_id>:<port>：Transport 以 scheme+host 作为空闲连接池的键，实例之间绝不共用连接。
		DialContext: s.dial,
		Proxy:       nil,
		// 每个实例端口最多保留 8 条空闲通道
		MaxIdleConnsPerHost:   maxIdlePerHost,
		MaxConnsPerHost:       maxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: responseHeaderTO,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
	}
	s.proxy = &httputil.ReverseProxy{
		// Rewrite（而非 Director）：调用前已删除客户端带来的 Forwarded / X-Forwarded-For/Host/Proto 与逐跳头。
		Rewrite: s.rewrite,
		ModifyResponse: func(res *http.Response) error {
			rewriteSetCookies(res.Header)
			if st, ok := res.Request.Context().Value(streamKey{}).(*stream); ok &&
				(res.StatusCode == http.StatusSwitchingProtocols || strings.HasPrefix(strings.ToLower(res.Header.Get("Content-Type")), "text/event-stream")) {
				st.mu.Lock()
				st.streaming = true
				st.mu.Unlock()
			}
			return nil
		},
		Transport:     s.tr,
		FlushInterval: -1,
		ErrorHandler:  s.proxyError,
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, port, ok := ParseHost(r.Host, s.auth.IngressDomain)
	if !ok {
		WritePage(w, http.StatusNotFound, "页面不存在", PageNotFound, 0)
		return
	}
	// 平台保留路径：本地处理，绝不转发（回调的 query 里有票据）
	if r.URL.Path == "/.spark" || strings.HasPrefix(r.URL.Path, "/.spark/") {
		switch r.URL.Path {
		case "/.spark/callback":
			s.auth.HandleCallback(w, r)
		case "/.spark/logout":
			s.auth.HandleLogout(w, r)
		case "/.spark/healthz":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write([]byte("ok"))
		default:
			WritePage(w, http.StatusNotFound, "页面不存在", PageNotFound, 0)
		}
		return
	}
	route, ok := s.routes.Lookup(id, port)
	if !ok {
		WritePage(w, http.StatusNotFound, "端口未开放", PageNotOpen, 0)
		return
	}
	if route.State != rental.StateRunning || route.Suspended {
		WritePage(w, http.StatusServiceUnavailable, "暂时无法访问", NotRunningPage(route), 10)
		return
	}
	st := &stream{instance: id, port: port}
	if route.Visibility != "public" {
		// 兄弟租户（同站不同源）页面借用户的 Cookie 发起写请求或 WebSocket：直接拒绝
		if s.crossSiteBlocked(r, id, port) {
			WritePage(w, http.StatusForbidden, "无法访问", "跨站请求被拒绝", 0)
			return
		}
		claims, ok := s.auth.ValidCookieWith(r, id, port, func(c Claims) bool {
			return c.UserID == route.UserID && s.sessionValid(r.Context(), c.SessionHash, c.UserID)
		})
		if !ok {
			original := strings.TrimSuffix(s.auth.PortURL(id, port), "/") + r.URL.RequestURI()
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, s.auth.AuthorizeURL(original), http.StatusFound)
			return
		}
		st.authed, st.userID, st.sessHash = true, claims.UserID, claims.SessionHash
	}
	if s.isClosing() {
		WritePage(w, http.StatusServiceUnavailable, "暂时无法访问", PageShuttingDown, 10)
		return
	}
	s.noteTarget(route)
	// 长请求 / SSE / WebSocket 不受 http.Server 的 Read/WriteTimeout 约束（只清本请求的期限，控制台路径不受影响）
	rc := http.NewResponseController(w)
	if err := errors.Join(rc.SetReadDeadline(time.Time{}), rc.SetWriteDeadline(time.Time{})); err != nil {
		s.deadlineOnce.Do(func() { s.log.Warn("ingress cannot clear request deadlines", "err", err) })
	}
	// 登记为活动流：访问权限变化（端口改私有 / 删除、停机、登出）时由 Sweep 撤销
	ctx, cancel := context.WithCancel(context.WithValue(context.WithValue(r.Context(), ctxKey{}, route), streamKey{}, st))
	defer cancel()
	st.cancel = cancel
	if !s.addStream(st) { // 与 Shutdown 并发：没登记上就不转发
		WritePage(w, http.StatusServiceUnavailable, "暂时无法访问", PageShuttingDown, 10)
		return
	}
	defer s.removeStream(st)
	s.proxy.ServeHTTP(&trackingWriter{ResponseWriter: w, st: st}, r.WithContext(ctx))
}

// crossSiteBlocked：私有端口上，非 GET/HEAD/OPTIONS 请求若 Sec-Fetch-Site 为 same-site / cross-site 则拒绝；没有该头时退到 Origin：
// 带了且不等于本实例 origin 即拒绝，两者都没有放行（非浏览器客户端不带 Cookie）。
// WebSocket 升级（GET，但能读写）须同源：有 Sec-Fetch-Site → 只放 same-origin / none；没有它但有 Origin → 必须等于本实例 origin。
func (s *Server) crossSiteBlocked(r *http.Request, id string, port int) bool {
	sfs := r.Header.Get("Sec-Fetch-Site")
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		if sfs != "" {
			return sfs == "same-site" || sfs == "cross-site"
		}
		return s.foreignOrigin(r, id, port)
	}
	if !isUpgrade(r) {
		return false
	}
	if sfs != "" {
		return sfs != "same-origin" && sfs != "none"
	}
	return s.foreignOrigin(r, id, port)
}

// foreignOrigin：带了 Origin 且不等于本实例 origin（没有 Origin 放行）。
func (s *Server) foreignOrigin(r *http.Request, id string, port int) bool {
	origin := r.Header.Get("Origin")
	return origin != "" && origin != strings.TrimSuffix(s.auth.PortURL(id, port), "/")
}

func isUpgrade(r *http.Request) bool {
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// noteTarget：实例的 lan_ip / sshd 端口 / 主机公钥变化（换机、重建）时丢弃 Transport 的空闲连接，
// 避免复用经旧上游建立的通道（空闲连接按 http://<instance>:<port> 复用，看不到地址变化）。
func (s *Server) noteTarget(r rental.IngressRoute) {
	key := r.LanIP + "|" + strconv.Itoa(r.SSHPort) + "|" + strings.TrimSpace(r.HostPubkey)
	s.mu.Lock()
	prev, seen := s.dialedAt[r.InstanceID]
	s.dialedAt[r.InstanceID] = key
	s.mu.Unlock()
	if seen && prev != key {
		s.tr.CloseIdleConnections()
	}
}

// spoofableClientIPHeaders：客户端可以自带、租户应用可能误信为真实 IP 的头；转发前删除（真实 IP 只在 X-Forwarded-For 里）。
var spoofableClientIPHeaders = []string{"X-Real-Ip", "True-Client-Ip", "Cf-Connecting-Ip", "X-Client-Ip"}

func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	route := pr.In.Context().Value(ctxKey{}).(rental.IngressRoute)
	// 客户端自带的转发头一律不信（Rewrite 已删 X-Forwarded-For/Host/Proto，这里补删其余 X-Forwarded-* 与各种「客户端 IP」头）
	for k := range pr.Out.Header {
		if strings.HasPrefix(k, "X-Forwarded-") {
			delete(pr.Out.Header, k)
		}
	}
	for _, h := range spoofableClientIPHeaders {
		pr.Out.Header.Del(h)
	}
	pr.SetXForwarded()
	// 协议取对外地址的配置值：TLS 可能在前置代理终止，In.TLS 不可信
	pr.Out.Header.Set("X-Forwarded-Proto", s.auth.BaseURL.Scheme)
	pr.Out.URL.Scheme = "http"
	pr.Out.URL.Host = net.JoinHostPort(route.InstanceID, strconv.Itoa(route.Port))
	pr.Out.Host = pr.In.Host // 租户应用看到用户访问的域名
	stripIngressCookies(pr.Out.Header)
}

// stripIngressCookies 删除所有名为 sc_ingress 的 Cookie（租户应用不该拿到），其余 Cookie 原样保留（不重新编码）。
func stripIngressCookies(h http.Header) {
	var kept []string
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			name, _, _ := strings.Cut(p, "=")
			if strings.TrimSpace(name) == CookieName {
				continue
			}
			kept = append(kept, p)
		}
	}
	h.Del("Cookie")
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

// rewriteSetCookies 让上游设置的 Cookie 一律成为 host-only：删除 Domain 属性（不区分大小写，解析不了的值也删），
// 其余属性原样保留；名为 sc_ingress 的 Set-Cookie 整条丢弃（租户不能覆盖 / 伪造平台凭证）。
// 否则租户可用 Domain=<ingress 域> 把 Cookie 种到所有兄弟实例与平台域上。
func rewriteSetCookies(h http.Header) {
	vals := h.Values("Set-Cookie")
	if len(vals) == 0 {
		return
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		parts := strings.Split(v, ";")
		name, _, _ := strings.Cut(parts[0], "=")
		if strings.TrimSpace(name) == CookieName {
			continue
		}
		kept := []string{parts[0]}
		for _, a := range parts[1:] {
			k, _, _ := strings.Cut(a, "=")
			if strings.EqualFold(strings.TrimSpace(k), "domain") {
				continue
			}
			kept = append(kept, a)
		}
		out = append(out, strings.Join(kept, ";"))
	}
	h.Del("Set-Cookie")
	for _, v := range out {
		h.Add("Set-Cookie", v)
	}
}

// dial 是 Transport.DialContext：路由随请求 ctx 传入（Transport 拨号保留请求 ctx 的值）；缺失时按地址查路由表。
func (s *Server) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	route, ok := ctx.Value(ctxKey{}).(rental.IngressRoute)
	if !ok {
		id, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, _ := strconv.Atoi(portStr)
		if route, ok = s.routes.Lookup(id, port); !ok {
			return nil, fmt.Errorf("ingress: route %s vanished", addr)
		}
	}
	return s.dialer.Dial(ctx, upstream.Target{InstanceID: route.InstanceID, LanIP: route.LanIP, HostPubkey: route.HostPubkey, Port: route.SSHPort}, route.Port)
}

// sessionValid：Cookie 绑定的控制台会话仍有效且属于 userID（登出即失效）。查询出错（数据库故障）视为无效。
func (s *Server) sessionValid(ctx context.Context, hash string, userID int64) bool {
	uid, ok := s.sessionUser(ctx, hash)
	return ok && uid != 0 && uid == userID
}

// sessionUser 返回会话对应的用户（0 = 会话无效 / 已登出 / 被封禁）；确定的结果缓存 2 秒。
// ok=false 表示查询出错（不缓存）：请求路径据此拒绝，撤销扫描据此不动已有连接。
func (s *Server) sessionUser(ctx context.Context, hash string) (int64, bool) {
	now := s.clk.Now()
	s.mu.Lock()
	if c, ok := s.sessCache[hash]; ok && now.Before(c.until) {
		s.mu.Unlock()
		return c.userID, true
	}
	s.mu.Unlock()
	uid, err := s.sess.SessionUserID(ctx, hash)
	if err != nil {
		if !errors.Is(err, apperr.ErrUnauthorized) && !errors.Is(err, apperr.ErrBanned) {
			if ctx.Err() == nil {
				s.log.Error("ingress session lookup failed", "err", err)
			}
			return 0, false
		}
		uid = 0
	}
	s.mu.Lock()
	if len(s.sessCache) >= sessionCacheMax {
		for k, c := range s.sessCache {
			if !now.Before(c.until) {
				delete(s.sessCache, k)
			}
		}
	}
	s.sessCache[hash] = cachedSession{userID: uid, until: now.Add(sessionCacheTTL)}
	s.mu.Unlock()
	return uid, true
}

// proxyError：只有 *upstream.DialError（到不了容器 sshd）告警；客户端放弃、池关闭、容器端口未监听都不告警。
// 不记录 URL / query（票据之类的敏感参数不进日志）。
func (s *Server) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	route, _ := r.Context().Value(ctxKey{}).(rental.IngressRoute)
	var de *upstream.DialError
	var oce *ssh.OpenChannelError
	switch {
	case r.Context().Err() != nil || errors.Is(err, context.Canceled):
		s.log.Debug("ingress client gone", "instance", route.InstanceID, "port", route.Port, "err", err)
	case errors.As(err, &de):
		s.log.Warn("ingress upstream unreachable", "instance", route.InstanceID, "port", route.Port, "err", err)
		s.alertUnreachable(route.InstanceID)
	case errors.As(err, &oce):
		s.log.Info("ingress upstream port not listening", "instance", route.InstanceID, "port", route.Port)
	case errors.Is(err, upstream.ErrClosed):
		s.log.Info("ingress upstream pool closed", "instance", route.InstanceID, "port", route.Port)
	default:
		s.log.Warn("ingress upstream error", "instance", route.InstanceID, "port", route.Port, "err", err)
	}
	WritePage(w, http.StatusBadGateway, "无法连接", fmt.Sprintf(PageUpstreamFmt, route.Port), 0)
}

func (s *Server) alertUnreachable(instanceID string) {
	now := s.clk.Now()
	s.mu.Lock()
	if last, ok := s.lastUnreach[instanceID]; ok && now.Sub(last) < unreachThrottle {
		s.mu.Unlock()
		return
	}
	s.lastUnreach[instanceID] = now
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), alertTimeout)
	defer cancel()
	if err := s.alerts.RaiseUpstreamUnreachable(ctx, instanceID); err != nil {
		s.log.Error("raise upstream_unreachable", "instance", instanceID, "err", err)
	}
}
