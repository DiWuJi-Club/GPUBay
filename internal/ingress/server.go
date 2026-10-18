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
	dialedAt    map[string]string // 实例 → 最近一次转发所用的 lan_ip|主机公钥；变化时丢弃 Transport 空闲连接
}

type cachedSession struct {
	userID int64 // 0 = 会话无效
	until  time.Time
}

type ctxKey struct{}

func NewServer(auth *Auth, routes *Table, sessions SessionChecker, dialer Dialer, alerts Alerts, clk clock.Clock, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{auth: auth, routes: routes, sess: sessions, dialer: dialer, alerts: alerts, clk: clk, log: log,
		sessCache: map[string]cachedSession{}, lastUnreach: map[string]time.Time{}, dialedAt: map[string]string{}}
	s.tr = &http.Transport{
		// 出站地址是 http://<instance_id>:<port>：Transport 以 scheme+host 作为空闲连接池的键，实例之间绝不共用连接。
		DialContext: s.dial,
		Proxy:       nil,
		// 每个实例端口最多保留 8 条空闲通道
		MaxIdleConnsPerHost:   maxIdlePerHost,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: responseHeaderTO,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
	}
	s.proxy = &httputil.ReverseProxy{
		// Rewrite（而非 Director）：调用前已删除客户端带来的 Forwarded / X-Forwarded-For/Host/Proto 与逐跳头。
		Rewrite:       s.rewrite,
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
	if route.Visibility != "public" {
		// 兄弟租户（同站不同源）页面借用户的 Cookie 发起写请求或 WebSocket：直接拒绝
		if s.crossSiteBlocked(r, id, port) {
			WritePage(w, http.StatusForbidden, "无法访问", "跨站请求被拒绝", 0)
			return
		}
		_, ok := s.auth.ValidCookieWith(r, id, port, func(c Claims) bool {
			return c.UserID == route.UserID && s.sessionValid(r.Context(), c.SessionHash, c.UserID)
		})
		if !ok {
			original := strings.TrimSuffix(s.auth.PortURL(id, port), "/") + r.URL.RequestURI()
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, s.auth.AuthorizeURL(original), http.StatusFound)
			return
		}
	}
	s.noteTarget(route)
	// 长请求 / SSE / WebSocket 不受 http.Server 的 Read/WriteTimeout 约束（只清本请求的期限，控制台路径不受影响）
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Time{})
	rc.SetWriteDeadline(time.Time{})
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, route)))
}

// crossSiteBlocked：私有端口上，非 GET/HEAD/OPTIONS 请求若 Sec-Fetch-Site 为 same-site / cross-site 则拒绝（无该头放行：非浏览器客户端不带 Cookie）；
// WebSocket 升级（GET，但能读写）还须同源：有 Sec-Fetch-Site → 只放 same-origin / none；没有它但有 Origin → 必须等于本实例 origin。
func (s *Server) crossSiteBlocked(r *http.Request, id string, port int) bool {
	sfs := r.Header.Get("Sec-Fetch-Site")
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return sfs == "same-site" || sfs == "cross-site"
	}
	if !isUpgrade(r) {
		return false
	}
	if sfs != "" {
		return sfs != "same-origin" && sfs != "none"
	}
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

// noteTarget：实例的 lan_ip / 主机公钥变化（换机、重建）时丢弃 Transport 的空闲连接，
// 避免复用经旧上游建立的通道（空闲连接按 http://<instance>:<port> 复用，看不到地址变化）。
func (s *Server) noteTarget(r rental.IngressRoute) {
	key := r.LanIP + "|" + strings.TrimSpace(r.HostPubkey)
	s.mu.Lock()
	prev, seen := s.dialedAt[r.InstanceID]
	s.dialedAt[r.InstanceID] = key
	s.mu.Unlock()
	if seen && prev != key {
		s.tr.CloseIdleConnections()
	}
}

func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	route := pr.In.Context().Value(ctxKey{}).(rental.IngressRoute)
	// 客户端自带的转发头一律不信（Rewrite 已删 X-Forwarded-For/Host/Proto，这里补删其余 X-Forwarded-* 与 X-Real-IP）
	for k := range pr.Out.Header {
		if strings.HasPrefix(k, "X-Forwarded-") {
			delete(pr.Out.Header, k)
		}
	}
	pr.Out.Header.Del("X-Real-Ip")
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
	return s.dialer.Dial(ctx, upstream.Target{InstanceID: route.InstanceID, LanIP: route.LanIP, HostPubkey: route.HostPubkey}, route.Port)
}

// sessionValid：Cookie 绑定的控制台会话仍有效（登出即失效）；确定的结果缓存 2 秒，查询出错（数据库故障）不缓存并视为无效。
func (s *Server) sessionValid(ctx context.Context, hash string, userID int64) bool {
	now := s.clk.Now()
	s.mu.Lock()
	if c, ok := s.sessCache[hash]; ok && now.Before(c.until) {
		s.mu.Unlock()
		return c.userID != 0 && c.userID == userID
	}
	s.mu.Unlock()
	uid, err := s.sess.SessionUserID(ctx, hash)
	if err != nil {
		if !errors.Is(err, apperr.ErrUnauthorized) && !errors.Is(err, apperr.ErrBanned) {
			if ctx.Err() == nil {
				s.log.Error("ingress session lookup failed", "err", err)
			}
			return false
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
	return uid != 0 && uid == userID
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
