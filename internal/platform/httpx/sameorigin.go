package httpx

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"spark-cloud/internal/platform/apperr"
)

// SameSiteGuard 防止同站（兄弟子域，例如租户 Web 页面）向控制台 / 后台提交写请求：SameSite Cookie 只区分跨站不区分兄弟子域。
// 规则（非 GET / HEAD / OPTIONS）：有 Sec-Fetch-Site → 只放行 same-origin / none；没有它但有 Origin → 必须等于 publicBaseURL 的 origin
// （publicBaseURL 为空时无法比对，一律拒绝）；两个头都没有 → 放行（非浏览器客户端；浏览器跨源 POST 一定带 Origin）。
// exempt 返回 true 的请求（例如支付渠道的异步回调，天然跨站）不做检查。
func SameSiteGuard(publicBaseURL string, exempt func(*http.Request) bool, log *slog.Logger) func(http.Handler) http.Handler {
	origin := ""
	if u, err := url.Parse(publicBaseURL); err == nil && u.Scheme != "" && u.Host != "" {
		origin = strings.ToLower(u.Scheme + "://" + u.Host)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions || (exempt != nil && exempt(r)) {
				next.ServeHTTP(w, r)
				return
			}
			if sfs := strings.ToLower(r.Header.Get("Sec-Fetch-Site")); sfs != "" {
				if sfs == "same-origin" || sfs == "none" {
					next.ServeHTTP(w, r)
					return
				}
				log.Warn("same-site guard rejected request", "sec_fetch_site", sfs, "origin", r.Header.Get("Origin"), "path", r.URL.Path)
				WriteError(log, w, r, apperr.ErrForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				if origin != "" && strings.ToLower(strings.TrimSuffix(o, "/")) == origin {
					next.ServeHTTP(w, r)
					return
				}
				log.Warn("same-site guard rejected request", "origin", o, "path", r.URL.Path)
				WriteError(log, w, r, apperr.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
