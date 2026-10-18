// Package api 提供用户控制台使用的 REST API。
package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/billing"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/ingress"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/storage"
	"spark-cloud/internal/wallet"
	"spark-cloud/internal/web"
)

type Server struct {
	Identity       *identity.Service
	Wallet         *wallet.Service
	Catalog        *catalog.Service
	Fleet          *fleet.Service
	Rental         *rental.Service
	Uploads        *storage.Service
	Billing        *billing.Service
	DevPay         bool // 仅开发环境（mock 支付）为 true：注册模拟支付成功接口
	Clock          clock.Clock
	Log            *slog.Logger
	Domain         string        // 用于生成 SSH 命令，例如 spark.example.com
	SecureCookie   bool          // 生产环境为 true：Cookie 仅经 HTTPS 发送
	TrustProxy     bool          // 仅当部署在会改写转发头的反向代理之后才设为 true
	Static         http.Handler  // 控制台静态文件；nil 时使用 web.Console()
	PublicBaseURL  string        // 对外地址，用于同站防护的 Origin 比对
	Ingress        *ingress.Auth // Web 入口鉴权；nil 时不注册 authorize 端点
	AuthorizeLimit int           // authorize 每用户每分钟上限；零值 60

	authorizeLim *authorizeLimiter
}

const sessionCookie = "sc_session"

// cookieName：staging / prod 用 __Host- 前缀（浏览器拒绝子域用 Domain 属性覆写），dev 走 http 无法用前缀。
func (s *Server) cookieName() string {
	if s.SecureCookie {
		return "__Host-" + sessionCookie
	}
	return sessionCookie
}

type userCtxKey struct{}

func (s *Server) Router() http.Handler {
	limit := s.AuthorizeLimit
	if limit <= 0 {
		limit = defaultAuthorizeLimit
	}
	s.authorizeLim = &authorizeLimiter{limit: limit}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	if s.TrustProxy {
		r.Use(middleware.RealIP)
	}
	r.Use(httpx.Recoverer(s.Log), s.requireJSONForWrites,
		httpx.SameSiteGuard(s.PublicBaseURL, func(req *http.Request) bool { return strings.HasPrefix(req.URL.Path, notifyPathPrefix) }, s.Log))
	static := s.Static
	if static == nil {
		static = web.Console()
	}
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			s.writeError(w, req, apperr.ErrNotFound) // 未知 API 路径返回 JSON 404，不回退页面
			return
		}
		static.ServeHTTP(w, req)
	})
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/sms-code", s.handleSendCode)
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/logout", s.handleLogout)
		r.Get("/products", s.handleProducts)
		r.Get("/images", s.handleImages)
		r.Get("/pay/channels", s.handlePayChannels)
		r.Post("/pay/notify/{channel}", s.handlePayNotify)
		if s.Ingress != nil {
			r.With(httpx.NoStore).Get("/ingress/authorize", s.handleIngressAuthorize) // 组外：未登录要 302 到登录页而不是 JSON 401
		}

		r.Group(func(r chi.Router) {
			r.Use(httpx.NoStore, s.requireUser)
			r.Get("/me", s.handleMe)
			r.Post("/uploads", s.handleUpload)
			r.Get("/kyc", s.handleMyKYC)
			r.Post("/kyc/personal", s.handleKYCPersonal)
			r.Post("/kyc/enterprise", s.handleKYCEnterprise)
			r.Get("/wallet", s.handleWallet)
			r.Get("/wallet/ledger", s.handleLedger)
			r.Get("/orders", s.handleOrders)
			r.Post("/orders", s.handlePlaceOrder)
			r.Get("/instances", s.handleInstances)
			r.Get("/instances/{id}", s.handleInstance)
			r.Get("/instances/{id}/password", s.handlePassword)
			r.Post("/instances/{id}/renew", s.handleRenew)
			r.Post("/instances/{id}/release", s.handleRelease)
			r.Get("/recharges", s.handleRecharges)
			r.Post("/recharges", s.handleCreateRecharge)
			r.Get("/recharges/{id}", s.handleRecharge)
			if s.DevPay {
				r.Post("/dev/recharges/{id}/pay", s.handleDevPay)
			}
		})
	})
	return r
}

// requireJSONForWrites 要求写请求使用 application/json（上传接口要求 multipart/form-data）。
// 浏览器跨站表单无法伪造 JSON 类型，兼作 CSRF 防护；同站（兄弟子域）与跨站的写请求由 SameSiteGuard 拦截（SameSite Cookie 对兄弟子域无效）。
func (s *Server) requireJSONForWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, notifyPathPrefix) { // 支付回调的格式由渠道决定，由插件自行验签解析
			next.ServeHTTP(w, r)
			return
		}
		want, msg := "application/json", "请求格式必须为 JSON"
		if r.URL.Path == uploadPath {
			want, msg = "multipart/form-data", "请使用 multipart/form-data 上传文件"
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), want) {
			s.writeError(w, r, apperr.BadRequest(msg))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie(s.cookieName()); err == nil {
			token = c.Value
		}
		u, err := s.Identity.Authenticate(r.Context(), token)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey{}, u)))
	})
}

func currentUser(r *http.Request) identity.User {
	return r.Context().Value(userCtxKey{}).(identity.User)
}
