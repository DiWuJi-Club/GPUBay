package ingress

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"spark-cloud/internal/rental"
)

// MaxRevokeInterval：访问权限变化后，已有的长连接（WebSocket / SSE / 长轮询）最迟在这么久内被断开，与 SSH 网关「10 秒内断开」一致。
// Run 的 interval 不应超过它。
const MaxRevokeInterval = 10 * time.Second

// stream 是一个正在转发中的请求（含升级后的 WebSocket）；记录建立时的身份，供 Sweep 复核。
type stream struct {
	instance string
	port     int
	authed   bool // 经私有端口 Cookie 校验进入
	userID   int64
	sessHash string
	cancel   context.CancelFunc

	mu      sync.Mutex
	conn    net.Conn // 被劫持的客户端连接（WebSocket）
	revoked bool
}

// revoke 取消请求 ctx（流式响应随之中断，ReverseProxy 也会关闭上游）并关闭被劫持的客户端连接。
func (st *stream) revoke() {
	st.cancel()
	st.mu.Lock()
	st.revoked = true
	c := st.conn
	st.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// trackingWriter 记录 ReverseProxy 劫持的连接；其余能力经 Unwrap 交给 http.ResponseController。
type trackingWriter struct {
	http.ResponseWriter
	st *stream
}

func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackingWriter) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *trackingWriter) Flush() { w.FlushError() }

func (w *trackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return c, brw, err
	}
	w.st.mu.Lock()
	w.st.conn = c
	revoked := w.st.revoked
	w.st.mu.Unlock()
	if revoked {
		c.Close()
	}
	return c, brw, nil
}

func (s *Server) addStream(st *stream) {
	s.mu.Lock()
	s.streams[st] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) removeStream(st *stream) {
	s.mu.Lock()
	delete(s.streams, st)
	s.mu.Unlock()
}

// ActiveStreams 返回正在转发中的请求数（含 WebSocket）。
func (s *Server) ActiveStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

// Run 每 interval（应 ≤ MaxRevokeInterval）执行一次 Sweep，直到 ctx 结束。
func (s *Server) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 || interval > MaxRevokeInterval {
		interval = MaxRevokeInterval
	}
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep 按当前路由表复核所有活动流，撤销已无权访问的；并清理已不在路由表中的实例的节流 / 目标记录。
func (s *Server) Sweep(ctx context.Context) {
	present := map[string]bool{}
	for _, r := range s.routes.Routes() {
		present[r.InstanceID] = true
	}
	now := s.clk.Now()
	s.mu.Lock()
	pruned := false
	for id := range s.dialedAt {
		if !present[id] {
			delete(s.dialedAt, id)
			pruned = true
		}
	}
	for id, last := range s.lastUnreach {
		if !present[id] || now.Sub(last) >= unreachThrottle {
			delete(s.lastUnreach, id)
		}
	}
	list := make([]*stream, 0, len(s.streams))
	for st := range s.streams {
		list = append(list, st)
	}
	s.mu.Unlock()
	if pruned {
		// 实例重新出现时可能已换机：不留经旧上游建立的空闲通道
		s.tr.CloseIdleConnections()
	}
	for _, st := range list {
		if reason := s.revokeReason(ctx, st); reason != "" {
			s.log.Info("ingress stream revoked", "instance", st.instance, "port", st.port, "reason", reason)
			st.revoke()
		}
	}
}

func (s *Server) revokeReason(ctx context.Context, st *stream) string {
	r, ok := s.routes.Lookup(st.instance, st.port)
	switch {
	case !ok:
		return "port removed"
	case r.State != rental.StateRunning || r.Suspended:
		return "instance not running"
	case r.Visibility == "public":
		return ""
	case !st.authed:
		return "port now private"
	case r.UserID != st.userID:
		return "owner changed"
	}
	uid, ok := s.sessionUser(ctx, st.sessHash)
	if ok && uid != st.userID { // 查询出错不撤销
		return "session ended"
	}
	return ""
}
