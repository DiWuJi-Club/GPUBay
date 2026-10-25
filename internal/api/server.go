// Package api 提供用户控制台使用的 REST API。
package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/butler"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/comfywf"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/ingress"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/referral"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/storage"
	"spark-cloud/internal/wallet"
	"spark-cloud/internal/web"
)

type Server struct {
	Identity         *identity.Service
	Wallet           *wallet.Service
	Catalog          *catalog.Service
	Fleet            *fleet.Service
	Rental           *rental.Service
	Uploads          *storage.Service
	Billing          *billing.Service
	Referral         *referral.Service // 计划 8：邀请返利；nil 时不注册 /referral 路由
	DevPay           bool              // 仅开发环境（mock 支付）为 true：注册模拟支付成功接口
	Clock            clock.Clock
	Log              *slog.Logger
	Domain           string        // 主域名，例如 spark.example.com
	SSHHost          string        // SSH 命令里的主机名：ssh.<domain>
	SSHPort          int           // SPARK_SSH_PUBLIC_PORT；非 22 时 SSH 命令带 -p；零值 = 22
	SecureCookie     bool          // 生产环境为 true：Cookie 仅经 HTTPS 发送
	TrustProxy       bool          // 仅当部署在会改写转发头的反向代理之后才设为 true
	TrustedProxyHops int           // TrustProxy 时自己的代理层数：客户端 IP 取 X-Forwarded-For 自右数第 N 项（零值 = 1）
	Static           http.Handler  // 控制台静态文件；nil 时使用 web.Console()
	PublicBaseURL    string        // 对外地址，用于同站防护的 Origin 比对
	Ingress          *ingress.Auth // Web 入口鉴权；nil 时不注册 authorize 端点
	AuthorizeLimit   int           // authorize 每用户每分钟上限；零值 60
	CalendarLimit    int           // 匿名机组 / 日历 / 可用性端点按 IP 每分钟上限；零值 120
	ReferralLimit    int           // 匿名邀请码查询按 IP 每分钟上限；零值 60
	// 计划 7：应用部署；nil 时不注册 /apps* 与 /instances/{id}/apps*，实例视图的 apps_summary 恒为空数组
	Apps      *appdeploy.Service
	AppsLimit int // 应用部署类写操作每用户每小时上限；零值 appdeploy.UserOpsPerHour（60）
	// 计划 9：Token 商城。LLM 为 nil 时不注册 /llm/*（/me 也不带 llm_month_cost）；
	// LLMGateway 为 nil 时不挂载 /v1（OpenAI 兼容网关，llmgw.Server.Router()），/v1/* 返回 JSON 404。
	LLM        *llm.Service
	LLMGateway http.Handler
	LLMBaseURL string // 控制台展示的网关地址；空 = PublicBaseURL + "/v1"
	// 计划 11：ComfyUI 工作流市场。Workflows nil 时不注册 /workflows*；Installer / Runs nil（或 Apps nil）时不注册
	// /instances/{id}/apps/{dep}/workflows* 的安装 / 快速出图接口。安装写接口与应用部署写操作共用 AppsLimit。
	Workflows *comfywf.Catalog
	Installer *comfywf.Installer
	Runs      *comfywf.Runs
	// 计划 12：AI 管家；nil 时不注册 /butler/*（前端按「未开放」处理）
	Butler      *butler.Service
	ButlerLimit int           // 发消息 + 确认每用户每分钟上限；零值 butler.SendRatePerMinute（20）
	ButlerPing  time.Duration // SSE 保活注释间隔；零值 15 秒（测试调小）

	authorizeLim  *userLimiter
	calendarLim   *ipLimiter
	referralLim   *ipLimiter
	appsLim       *userLimiter
	wfUploadLim   *userLimiter
	runAttemptLim *userLimiter
	coverLim      *ipLimiter
	butlerLim     *userLimiter
	deadlineOnce  sync.Once
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
	s.authorizeLim = &userLimiter{limit: limit, window: time.Minute}
	climit := s.CalendarLimit
	if climit <= 0 {
		climit = defaultCalendarLimit
	}
	s.calendarLim = &ipLimiter{limit: climit}
	rlimit := s.ReferralLimit
	if rlimit <= 0 {
		rlimit = defaultReferralLimit
	}
	s.referralLim = &ipLimiter{limit: rlimit}
	alimit := s.AppsLimit
	if alimit <= 0 {
		alimit = appdeploy.UserOpsPerHour
	}
	s.appsLim = &userLimiter{limit: alimit, window: time.Hour}
	s.wfUploadLim = &userLimiter{limit: comfywf.RunsPerHour, window: time.Hour}
	s.runAttemptLim = &userLimiter{limit: runAttemptsPerHour, window: time.Hour}
	s.coverLim = &ipLimiter{limit: coverIPLimit}
	blimit := s.ButlerLimit
	if blimit <= 0 {
		blimit = butler.SendRatePerMinute
	}
	s.butlerLim = &userLimiter{limit: blimit, window: time.Minute}
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	if s.TrustProxy {
		r.Use(httpx.ProxyRealIP(s.TrustedProxyHops)) // 与租户入口同一套取法；不看 X-Real-IP / True-Client-IP
	}
	// /v1 网关（Bearer 鉴权、无 Cookie，没有 CSRF 面）免同站防护与 JSON 要求，Content-Type 由网关自己校验（规格 T20）。
	r.Use(httpx.Recoverer(s.Log), s.requireJSONForWrites,
		httpx.SameSiteGuard(s.PublicBaseURL, func(req *http.Request) bool {
			return strings.HasPrefix(req.URL.Path, notifyPathPrefix) || isGatewayPath(req.URL.Path)
		}, s.Log))
	static := s.Static
	if static == nil {
		static = web.Console()
	}
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") || isGatewayPath(req.URL.Path) {
			s.writeError(w, req, apperr.ErrNotFound) // 未知 API 路径（含未挂载的 /v1）返回 JSON 404，不回退页面
			return
		}
		static.ServeHTTP(w, req)
	})
	if s.LLMGateway != nil {
		r.Mount("/v1", s.LLMGateway) // 在 /api/v1 之外；不加超时中间件（流式响应的写期限由网关自己延长）
	}
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/sms-code", s.handleSendCode)
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/logout", s.handleLogout)
		r.Get("/products", s.handleProducts)
		r.Get("/images", s.handleImages)
		r.Get("/pay/channels", s.handlePayChannels)
		r.Group(func(r chi.Router) {
			r.Use(httpx.NoStore, s.ipRateLimit(s.calendarLim))
			r.Get("/groups", s.handleGroups)
			r.Get("/groups/{id}/machines", s.handleGroupMachines)
			r.Get("/groups/{id}/calendar", s.handleGroupCalendar)
			r.Get("/machines/{id}/calendar", s.handleMachineCalendar)
			r.Get("/availability", s.handleAvailability)
			if s.Apps != nil {
				r.Get("/apps", s.handleAppCatalog)
				r.Get("/apps/{slug}", s.handleAppCatalogItem)
			}
			if s.LLM != nil {
				r.Get("/llm/models", s.handleLLMModels)
			}
			if s.Workflows != nil {
				r.Get("/workflows", s.handleWorkflowCatalog)
				r.Get("/workflows/{slug}", s.handleWorkflowDetail)
			}
		})
		if s.Workflows != nil {
			// 封面：公共长缓存（处理器设 Cache-Control），自己的按 IP 限流，不占列表 / 详情的额度
			r.With(s.coverRateLimit).Get("/workflows/{slug}/cover", s.handleWorkflowCover)
		}
		r.Post("/pay/notify/{channel}", s.handlePayNotify)
		if s.Ingress != nil {
			r.With(httpx.NoStore).Get("/ingress/authorize", s.handleIngressAuthorize) // 组外：未登录要 302 到登录页而不是 JSON 401
		}
		if s.Referral != nil {
			r.With(httpx.NoStore, s.ipRateLimit(s.referralLim)).Get("/referral/codes/{code}", s.handleReferralCode) // 匿名：登录页显示邀请人
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
			r.Post("/bookings", s.handleCreateBooking)
			r.Get("/bookings/{id}", s.handleBatch)
			r.Get("/instances/{id}/cancel-quote", s.handleCancelQuote)
			r.Post("/instances/{id}/cancel", s.handleCancel)
			r.Put("/instances/{id}/auto-renew", s.handleAutoRenew)
			r.Get("/instances/{id}/events", s.handleEvents)
			r.Get("/ssh-keys", s.handleListSSHKeys)
			r.Post("/ssh-keys", s.handleAddSSHKey)
			r.Delete("/ssh-keys/{id}", s.handleDeleteSSHKey)
			r.Post("/instances/{id}/password/reset", s.handleResetPassword)
			r.Put("/instances/{id}/access", s.handleSetAccess)
			r.Get("/instances/{id}/ports", s.handleListPorts)
			r.Post("/instances/{id}/ports", s.handleAddPort)
			r.Put("/instances/{id}/ports/{port}", s.handleUpdatePort)
			r.Delete("/instances/{id}/ports/{port}", s.handleDeletePort)
			r.Get("/instances/{id}/ssh-sessions", s.handleSSHSessions)
			if s.Apps != nil {
				r.Get("/instances/{id}/apps", s.handleInstanceApps)
				r.Post("/instances/{id}/apps", s.handleCreateApp)
				r.Get("/instances/{id}/apps/{dep}", s.handleInstanceApp)
				r.Post("/instances/{id}/apps/{dep}/redeploy", s.handleRedeployApp)
				r.Post("/instances/{id}/apps/{dep}/stop", s.handleStopApp)
				r.Post("/instances/{id}/apps/{dep}/start", s.handleStartApp)
				r.Delete("/instances/{id}/apps/{dep}", s.handleRemoveApp)
				r.Get("/instances/{id}/apps/{dep}/log", s.handleAppLog)
				r.Get("/instances/{id}/apps/{dep}/events", s.handleAppEvents)
			}
			if s.Apps != nil && s.Installer != nil {
				r.Get("/instances/{id}/apps/{dep}/workflows", s.handleDeploymentWorkflows)
				r.Post("/instances/{id}/apps/{dep}/workflows", s.handleAddWorkflow)
				if s.Workflows != nil {
					r.Get("/instances/{id}/apps/{dep}/workflows/{wid}", s.handleDeploymentWorkflow)
				}
				r.Put("/instances/{id}/apps/{dep}/workflows/{wid}", s.handleUpdateWorkflow)
				r.Delete("/instances/{id}/apps/{dep}/workflows/{wid}", s.handleRemoveWorkflow)
			}
			if s.Apps != nil && s.Runs != nil {
				r.Post("/instances/{id}/apps/{dep}/workflows/{wid}/uploads", s.handleWorkflowUpload)
				r.Post("/instances/{id}/apps/{dep}/workflows/{wid}/runs", s.handleStartRun)
				r.Get("/instances/{id}/apps/{dep}/workflows/{wid}/runs/{run}", s.handleGetRun)
				r.Get("/instances/{id}/apps/{dep}/workflows/{wid}/runs/{run}/files/{index}", s.handleRunFile)
			}
			r.Get("/recharges", s.handleRecharges)
			r.Post("/recharges", s.handleCreateRecharge)
			r.Get("/recharges/{id}", s.handleRecharge)
			if s.Referral != nil {
				r.Get("/referral/me", s.handleReferralMe)
				r.Get("/referral/invitees", s.handleReferralInvitees)
				r.Get("/referral/rewards", s.handleReferralRewards)
			}
			if s.Butler != nil {
				r.Get("/butler/status", s.handleButlerStatus)
				r.Get("/butler/conversations", s.handleButlerConversations)
				r.Get("/butler/conversations/{id}", s.handleButlerConversation)
				r.Delete("/butler/conversations/{id}", s.handleButlerDelete)
				r.Post("/butler/conversations/{id}/messages", s.handleButlerSend)                   // SSE；id 可为 new
				r.Post("/butler/conversations/{id}/tool-calls/{tc}/confirm", s.handleButlerConfirm) // SSE
			}
			if s.LLM != nil {
				r.Get("/llm/account", s.handleLLMAccount)
				r.Get("/llm/keys", s.handleLLMKeys)
				r.Post("/llm/keys", s.handleLLMCreateKey)
				r.Put("/llm/keys/{id}", s.handleLLMUpdateKey)
				r.Delete("/llm/keys/{id}", s.handleLLMRevokeKey)
				r.Get("/llm/usage", s.handleLLMUsage)
				r.Get("/llm/usage/daily", s.handleLLMDaily)
				r.Get("/llm/settlements", s.handleLLMSettlements)
			}
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
		if strings.HasPrefix(r.URL.Path, notifyPathPrefix) || isGatewayPath(r.URL.Path) { // 支付回调的格式由渠道决定，由插件自行验签解析；/v1 由网关回 OpenAI 格式错误
			next.ServeHTTP(w, r)
			return
		}
		want, msg := "application/json", "请求格式必须为 JSON"
		if r.URL.Path == uploadPath || (r.Method == http.MethodPost && isWorkflowUploadPath(r.URL.Path)) {
			want, msg = "multipart/form-data", "请使用 multipart/form-data 上传文件"
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), want) {
			s.writeError(w, r, apperr.BadRequest(msg))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isGatewayPath /v1 与 /v1/*（OpenAI 兼容网关）；/v1x 之类不算。
func isGatewayPath(p string) bool { return p == "/v1" || strings.HasPrefix(p, "/v1/") }

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
