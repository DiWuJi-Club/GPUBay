package api

import (
	"errors"
	"net/http"
	"net/url"

	"spark-cloud/internal/identity"
	"spark-cloud/internal/ingress"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

const defaultAuthorizeLimit = 60 // 每用户每分钟

// handleIngressAuthorize：控制台域上的票据签发（浏览器整页跳转，所以回 HTML 页 / 302，不回 JSON）。
//   - target 不合法 → 400 页；未登录 → 登录页（redirect 回本端点）；
//   - 公开端口 → 不需要票据，302 回目标地址；
//   - 非主人（含实例 / 端口不存在）→ 403 页；主人但实例已进入终态 → 410 页；主人但端口未登记 → 404 页；
//   - 否则 → 302 到实例域名的 /.spark/callback（票据绑定当前会话，60 秒、单次）。
//
// 用 GET 是因为它是跨域整页跳转的一环（实例域名 302 过来）。GET 触发的唯一副作用是签发票据：票据只经 302 送到
// 从配置推导出的实例域名（不看请求 Host），绑定的是浏览器自己的会话和自己的实例，第三方页面诱导触发也拿不到票据内容。
func (s *Server) handleIngressAuthorize(w http.ResponseWriter, r *http.Request) {
	rawTarget := r.URL.Query().Get("target")
	t, err := s.Ingress.ParseTarget(rawTarget)
	if err != nil {
		ingress.WritePage(w, http.StatusBadRequest, "跳转目标不正确", ingress.PageBadTarget, 0)
		return
	}
	token := ""
	if c, err := r.Cookie(s.cookieName()); err == nil {
		token = c.Value
	}
	u, err := s.Identity.Authenticate(r.Context(), token)
	if errors.Is(err, apperr.ErrUnauthorized) {
		self := "/api/v1/ingress/authorize?target=" + url.QueryEscape(rawTarget)
		http.Redirect(w, r, s.Ingress.BaseURL.String()+"/login?redirect="+url.QueryEscape(self), http.StatusFound)
		return
	}
	if err != nil {
		s.writePageError(w, r, err)
		return
	}
	if !s.authorizeLim.allow(u.ID, s.Clock.Now()) {
		ingress.WritePage(w, http.StatusTooManyRequests, "操作过于频繁", apperr.ErrRateLimited.Message, 0)
		return
	}
	ports, err := s.Rental.ListPorts(r.Context(), t.InstanceID)
	if err != nil {
		s.writePageError(w, r, err)
		return
	}
	var port *rental.Port
	for i := range ports {
		if ports[i].Port == t.Port {
			port = &ports[i]
		}
	}
	if port != nil && port.Visibility == "public" {
		http.Redirect(w, r, s.Ingress.TargetURL(t), http.StatusFound)
		return
	}
	inst, err := s.Rental.GetForUser(r.Context(), u.ID, t.InstanceID)
	if errors.Is(err, apperr.ErrNotFound) {
		ingress.WritePage(w, http.StatusForbidden, "无法访问", ingress.PageForbidden, 0)
		return
	}
	if err != nil {
		s.writePageError(w, r, err)
		return
	}
	switch inst.State {
	case rental.StateReserved, rental.StateCreating, rental.StateRunning, rental.StateStopped:
	default:
		ingress.WritePage(w, http.StatusGone, "实例已释放", ingress.PageReleased, 0)
		return
	}
	if port == nil {
		ingress.WritePage(w, http.StatusNotFound, "端口未开放", ingress.PageNotOpen, 0)
		return
	}
	ticket := s.Ingress.Signer.Ticket(ingress.Claims{InstanceID: t.InstanceID, Port: t.Port, UserID: u.ID, SessionHash: identity.SessionHash(token)})
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, s.Ingress.CallbackURL(t, ticket), http.StatusFound)
}

// writePageError：业务错误（如封禁）→ 403 页；其余记日志后 500 页。
func (s *Server) writePageError(w http.ResponseWriter, r *http.Request, err error) {
	if ae := apperr.As(err); ae != nil {
		ingress.WritePage(w, http.StatusForbidden, "无法访问", ae.Message, 0)
		return
	}
	s.Log.Error("ingress authorize failed", "err", err, "path", r.URL.Path)
	ingress.WritePage(w, http.StatusInternalServerError, "服务暂时不可用", "请稍后重试", 0)
}
