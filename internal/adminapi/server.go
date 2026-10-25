// Package adminapi 提供运营后台 REST API。与用户 API 同一进程、不同端口，只应监听内网地址。
package adminapi

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/agentgw"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/comfywf"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/referral"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/storage"
	"spark-cloud/internal/wallet"
	"spark-cloud/internal/web"
)

const (
	prefix        = "/admin/api/v1"
	sessionCookie = "sc_admin"
)

// cookieName：staging / prod 用 __Host- 前缀（必须 Secure、Path=/、无 Domain，子域无法覆写），dev 走 http 保持 sc_admin、Path=/admin/。
func (s *Server) cookieName() string {
	if s.SecureCookie {
		return "__Host-" + sessionCookie
	}
	return sessionCookie
}

// cookiePath：__Host- 前缀要求 Path=/；dev 仍限定在 /admin/，避免与同主机不同端口的控制台互相携带 Cookie。
func (s *Server) cookiePath() string {
	if s.SecureCookie {
		return "/"
	}
	return "/admin/"
}

type Server struct {
	Admins        *admin.Service
	Identity      *identity.Service
	Wallet        *wallet.Service
	Catalog       *catalog.Service
	Fleet         *fleet.Service
	Rental        *rental.Service
	Billing       *billing.Service
	Uploads       *storage.Service
	Gateway       *agentgw.Server    // 机器接入命令与撤销（计划 2）
	Referral      *referral.Service  // 计划 8：邀请返利（referralRoutes、用户详情 referral 区块、仪表盘本月返利净额）
	Apps          *appdeploy.Service // 应用部署（计划 7）：部署列表 / 详情 / 强制卸载、实例详情与仪表盘
	AppCatalog    *appdeploy.Catalog // 应用目录（计划 7）：应用、版本、发布、预拉取、覆盖率
	Workflows     *comfywf.Catalog   // ComfyUI 工作流目录（计划 11）：工作流、版本、发布、封面、安装列表、仪表盘数字
	Installer     *comfywf.Installer // 工作流安装（计划 11）：部署详情里的工作流集合
	Clock         clock.Clock
	Log           *slog.Logger
	SecureCookie  bool                                     // 生产环境为 true：Cookie 仅经 HTTPS 发送
	Static        http.Handler                             // 后台静态文件；nil 时使用 web.Admin()
	LoginLimit    int                                      // 同一 IP 每分钟登录相关请求上限；零值表示默认 20。只有端到端测试服务会调大它
	PortURL       func(instanceID string, port int) string // Web 入口端口地址（app 传 ingress.Auth.PortURL）；nil 时 url 为空
	PublicBaseURL string                                   // 后台对外地址（SPARK_ADMIN_PUBLIC_BASE_URL），用于同站防护的 Origin 比对；空表示有 Origin 就拒绝

	limiter *ipLimiter
	routes  []Route
}

// Route 是一个需要登录的后台接口及其允许的角色，供权限矩阵测试核对。
type Route struct {
	Method  string
	Pattern string
	Roles   []admin.Role
}

var (
	allRoles     = []admin.Role{admin.RoleSuper, admin.RoleOps, admin.RoleFinance}
	opsRoles     = []admin.Role{admin.RoleSuper, admin.RoleOps}
	financeRoles = []admin.Role{admin.RoleSuper, admin.RoleFinance}
	superOnly    = []admin.Role{admin.RoleSuper}
)

func (s *Server) Router() http.Handler {
	limit := s.LoginLimit
	if limit <= 0 {
		limit = loginLimit
	}
	s.limiter = newIPLimiter(limit, time.Minute, s.Clock)
	s.routes = nil
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// 后台只在内网监听，不使用 RealIP：任何内网客户端都能伪造 X-Forwarded-For，
	// 会绕过按 IP 的登录限流并伪造审计日志里的 IP。始终以连接的 RemoteAddr 为准。
	r.Use(httpx.Recoverer(s.Log), s.requireJSONForWrites, httpx.SameSiteGuard(s.PublicBaseURL, nil, s.Log))
	jsonNotFound := func(w http.ResponseWriter, req *http.Request) { s.writeError(w, req, apperr.ErrNotFound) }
	r.NotFound(jsonNotFound)
	r.Route(prefix, func(r chi.Router) {
		r.Use(httpx.NoStore) // API 响应一律不缓存；静态文件由 web 处理器自行设置 Cache-Control
		r.With(s.rateLimit).Post("/auth/login", s.handleLogin)
		r.With(s.rateLimit).Post("/auth/totp/setup", s.handleTOTPSetup)
		r.With(s.rateLimit).Post("/auth/totp", s.handleTOTP)
		r.Post("/auth/logout", s.handleLogout)
		r.Group(func(g chi.Router) {
			g.Use(s.requireAdmin)
			s.accountRoutes(g)
			s.machineRoutes(g)
			s.groupRoutes(g)
			s.bookingRoutes(g)
			s.instanceRoutes(g)
			s.userRoutes(g)
			s.financeRoutes(g)
			s.referralRoutes(g)
			s.catalogRoutes(g)
			s.appRoutes(g)
			if s.Workflows != nil { // 计划 11：没接工作流目录时不注册这些路由（404），仪表盘的工作流数字为 0
				s.workflowRoutes(g)
			}
		})
	})
	static := s.Static
	if static == nil {
		static = web.Admin()
	}
	redirectAdmin := func(w http.ResponseWriter, req *http.Request) { http.Redirect(w, req, "/admin/", http.StatusFound) }
	r.Get("/", redirectAdmin)
	r.Get("/admin", redirectAdmin)
	r.HandleFunc("/admin/api/*", jsonNotFound) // /admin/api/ 下非 v1 的路径不回退页面
	r.Get("/admin/", static.ServeHTTP)
	r.Get("/admin/*", static.ServeHTTP)
	return r
}

// Routes 返回 Router 注册的全部需要登录的接口。
func (s *Server) Routes() []Route { return append([]Route(nil), s.routes...) }

// handle 注册一个需要登录的接口，并在处理器之前校验角色。
func (s *Server) handle(g chi.Router, method, pattern string, roles []admin.Role, h http.HandlerFunc) {
	s.routes = append(s.routes, Route{Method: method, Pattern: prefix + pattern, Roles: roles})
	g.With(s.requireRole(roles)).MethodFunc(method, pattern, h)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(s.Log, w, r, err)
}

// requireJSONForWrites 要求写请求使用 application/json；配合 SameSite=Strict Cookie 防止跨站请求。
// 例外：工作流封面上传要求 multipart/form-data。浏览器跨站表单可以发 multipart，所以这条路由还要求请求带 Origin 或
// Sec-Fetch-Site（浏览器的 fetch / XHR 一定带其一），两个都没有 → 403；带了的由随后的 SameSiteGuard 比对。
func (s *Server) requireJSONForWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isCoverUpload(r) {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				s.writeError(w, r, apperr.BadRequest("请使用 multipart/form-data 上传文件"))
				return
			}
			if r.Header.Get("Origin") == "" && r.Header.Get("Sec-Fetch-Site") == "" {
				s.Log.Warn("cover upload without Origin / Sec-Fetch-Site rejected", "path", r.URL.Path)
				s.writeError(w, r, apperr.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions &&
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			s.writeError(w, r, apperr.BadRequest("请求格式必须为 JSON"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type sessionKey struct{}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.Admins.Authenticate(r.Context(), s.sessionToken(r))
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess)))
	})
}

func (s *Server) requireRole(roles []admin.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !slices.Contains(roles, currentSession(r).Admin.Role) {
				s.writeError(w, r, apperr.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func currentSession(r *http.Request) admin.Session {
	return r.Context().Value(sessionKey{}).(admin.Session)
}

func (s *Server) sessionToken(r *http.Request) string {
	if c, err := r.Cookie(s.cookieName()); err == nil {
		return c.Value
	}
	return ""
}

// actor 返回当前请求的审计操作人。
func (s *Server) actor(r *http.Request) audit.Actor {
	return audit.Actor{AdminID: currentSession(r).Admin.ID, IP: httpx.ClientIP(r)}
}

// withStepUp 高风险操作：先校验动态码（或 5 分钟窗口），再执行 op；只有 op 成功且本次带了动态码时才打开验证窗口。
// 参数错误、业务失败的请求即使带着正确的动态码也不会打开窗口（否则随后的敏感操作可以免动态码）。
func (s *Server) withStepUp(r *http.Request, code string, op func() error) error {
	sess := currentSession(r)
	if err := s.Admins.StepUp(r.Context(), sess, code); err != nil {
		return err
	}
	if err := op(); err != nil {
		return err
	}
	if admin.NormalizeCode(code) == "" { // 窗口内免码的操作不延长窗口
		return nil
	}
	// 操作已提交：记录窗口失败只影响下一次是否要再输动态码，不能把成功的操作报成失败
	if err := s.Admins.RecordStepUp(r.Context(), sess); err != nil {
		s.Log.Warn("record step-up window failed", "admin", sess.Admin.ID, "err", err)
	}
	return nil
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.BadRequest("ID 不正确")
	}
	return id, nil
}

func pageOf(r *http.Request) (paging.Page, error) { return paging.Parse(r.URL.Query()) }
