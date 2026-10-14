// Package api 提供用户控制台使用的 REST API。
package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

type Server struct {
	Identity     *identity.Service
	Wallet       *wallet.Service
	Catalog      *catalog.Service
	Fleet        *fleet.Service
	Rental       *rental.Service
	Clock        clock.Clock
	Log          *slog.Logger
	Domain       string // 用于生成 SSH 命令，例如 spark.example.com
	SecureCookie bool   // 生产环境为 true：Cookie 仅经 HTTPS 发送
	TrustProxy   bool   // 仅当部署在会改写转发头的反向代理之后才设为 true
}

const sessionCookie = "sc_session"

type userCtxKey struct{}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	if s.TrustProxy {
		r.Use(middleware.RealIP)
	}
	r.Use(middleware.Recoverer, s.requireJSONForWrites)
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/sms-code", s.handleSendCode)
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/logout", s.handleLogout)
		r.Get("/products", s.handleProducts)
		r.Get("/images", s.handleImages)

		r.Group(func(r chi.Router) {
			r.Use(noStore, s.requireUser)
			r.Get("/me", s.handleMe)
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
		})
	})
	return r
}

// requireJSONForWrites 要求所有写请求使用 application/json；浏览器跨站表单无法伪造该类型，兼作 CSRF 防护。
func (s *Server) requireJSONForWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions &&
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			s.writeError(w, r, apperr.BadRequest("请求格式必须为 JSON"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie(sessionCookie); err == nil {
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

// noStore 禁止缓存已登录接口的响应（含密码、余额等敏感数据）。
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
