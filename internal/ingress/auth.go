package ingress

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"spark-cloud/internal/platform/clock"
)

const CookieName = "sc_ingress"

// Target 是用户最初想访问的实例地址。
type Target struct {
	InstanceID string
	Port       int
	Path       string // 含 query，以 / 开头
}

// Auth 负责票据跳转与每实例 Cookie；所有对外 URL 从 SPARK_PUBLIC_BASE_URL 推导，不信任请求里的 Host。
type Auth struct {
	Signer        *Signer
	BaseURL       *url.URL // scheme + host[:port]
	IngressDomain string
	SecureCookie  bool
}

var errBadTarget = errors.New("ingress: bad target")

func NewAuth(secret []byte, publicBaseURL, ingressDomain string, secureCookie bool, clk clock.Clock) (*Auth, error) {
	u, err := url.Parse(publicBaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("ingress: bad public base url %q", publicBaseURL)
	}
	return &Auth{Signer: NewSigner(secret, clk), BaseURL: &url.URL{Scheme: u.Scheme, Host: u.Host}, IngressDomain: strings.ToLower(ingressDomain), SecureCookie: secureCookie}, nil
}

// effectivePort 返回 URL 的端口，未写时按 scheme 取默认端口（显式写出默认端口与省略等价）。
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// PortURL：scheme://ins-<id>-<port>.<ingress_domain>[:base 端口]/
func (a *Auth) PortURL(instanceID string, port int) string {
	host := fmt.Sprintf("%s-%d.%s", instanceID, port, a.IngressDomain)
	if p := a.BaseURL.Port(); p != "" {
		host = net.JoinHostPort(host, p)
	}
	return (&url.URL{Scheme: a.BaseURL.Scheme, Host: host, Path: "/"}).String()
}

func (a *Auth) AuthorizeURL(target string) string {
	return a.BaseURL.String() + "/api/v1/ingress/authorize?target=" + url.QueryEscape(target)
}

// TargetURL 返回 Target 对应的完整地址（PortURL + Path）。
func (a *Auth) TargetURL(t Target) string {
	return strings.TrimSuffix(a.PortURL(t.InstanceID, t.Port), "/") + t.Path
}

func (a *Auth) CallbackURL(t Target, ticket string) string {
	u, _ := url.Parse(a.PortURL(t.InstanceID, t.Port))
	u.Path = "/.spark/callback"
	u.RawQuery = url.Values{"ticket": {ticket}, "next": {t.Path}}.Encode()
	return u.String()
}

// ParseTarget 校验 authorize 的 target：scheme 与端口等于对外地址推导值、host 是本平台的实例域名、无用户名密码段。
func (a *Auth) ParseTarget(raw string) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != a.BaseURL.Scheme || u.User != nil || u.Host == "" || u.Opaque != "" {
		return Target{}, errBadTarget
	}
	if effectivePort(u) != effectivePort(a.BaseURL) {
		return Target{}, errBadTarget
	}
	id, port, ok := ParseHost(u.Host, a.IngressDomain)
	if !ok {
		return Target{}, errBadTarget
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return Target{InstanceID: id, Port: port, Path: path}, nil
}

// SafeNext 与控制台 safeRedirect 同规则：以 / 开头、不以 // 开头、不含反斜杠、解析后无 scheme / host；否则回 /。
// url.Parse 拒绝控制字符（浏览器会删掉 Tab / 换行，"/\t/evil" 会变成 "//evil"）。
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, `\`) {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

func (a *Auth) setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: a.SecureCookie, SameSite: http.SameSiteLaxMode})
}

// ValidCookie 逐枚验证同名 Cookie（兄弟子域的租户页面可用 Domain 属性塞同名 Cookie），任一验签通过且实例 / 端口匹配即可。
func (a *Auth) ValidCookie(r *http.Request, instanceID string, port int) (Claims, bool) {
	return a.ValidCookieWith(r, instanceID, port, nil)
}

// ValidCookieWith 同 ValidCookie，accept 非 nil 时还须通过（例如校验 Cookie 绑定的控制台会话仍有效）。
// 逐枚检查：前一枚被拒不会遮住后面有效的那枚。
func (a *Auth) ValidCookieWith(r *http.Request, instanceID string, port int, accept func(Claims) bool) (Claims, bool) {
	for _, c := range r.CookiesNamed(CookieName) {
		claims, err := a.Signer.VerifyCookie(c.Value)
		if err != nil || claims.InstanceID != instanceID || claims.Port != port {
			continue
		}
		if accept == nil || accept(claims) {
			return claims, true
		}
	}
	return Claims{}, false
}

// sameInstanceOrigin：Sec-Fetch-Site 为 same-origin / none，或（无该头时）Origin 等于本实例的 origin（从配置推导 scheme 与端口）。
func (a *Auth) sameInstanceOrigin(r *http.Request, instanceID string, port int) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		return sfs == "same-origin" || sfs == "none"
	}
	origin := r.Header.Get("Origin")
	return origin != "" && origin == strings.TrimSuffix(a.PortURL(instanceID, port), "/")
}

// HandleCallback：验票据（必须与当前 Host 的实例 / 端口一致）→ 发 Cookie → 302 到 next。
func (a *Auth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer") // 票据在 URL 里，不外传
	id, port, ok := ParseHost(r.Host, a.IngressDomain)
	if !ok {
		WritePage(w, http.StatusNotFound, "页面不存在", PageNotFound, 0)
		return
	}
	claims, err := a.Signer.VerifyTicket(r.URL.Query().Get("ticket"))
	if err != nil || claims.InstanceID != id || claims.Port != port {
		WritePage(w, http.StatusForbidden, "无法访问", PageTicketInvalid, 0)
		return
	}
	a.setCookie(w, a.Signer.Cookie(claims), int(CookieTTL/time.Second))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, SafeNext(r.URL.Query().Get("next")), http.StatusFound)
}

// HandleLogout：POST 清 Cookie（GET 可被任意页面触发，拒绝）。
// 只接受本实例页面发起的请求：有 Sec-Fetch-Site → 只放 same-origin / none；没有它 → Origin 必须等于本实例 origin；否则 403，不动 Cookie。
func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	id, port, ok := ParseHost(r.Host, a.IngressDomain)
	if !ok {
		WritePage(w, http.StatusNotFound, "页面不存在", PageNotFound, 0)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		WritePage(w, http.StatusMethodNotAllowed, "方法不允许", "请用 POST 退出", 0)
		return
	}
	if !a.sameInstanceOrigin(r, id, port) {
		WritePage(w, http.StatusForbidden, "无法访问", "退出请求必须来自本实例页面", 0)
		return
	}
	a.setCookie(w, "", -1)
	WritePage(w, http.StatusOK, "已退出", "已清除此实例的访问凭证", 0)
}
