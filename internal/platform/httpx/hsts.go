package httpx

import (
	"net"
	"net/http"
	"strings"
)

// HSTS 在 TLS 连接上加 Strict-Transport-Security（一年）；明文连接（dev、代理后）不加。
func HSTS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// ProxyRealIP 只在 SPARK_TRUST_PROXY 下使用：把 RemoteAddr 换成反向代理改写的 X-Forwarded-For 最左项（必须是合法 IP），
// 保持 host:port 形式（httputil 的 SetXForwarded 只认这种形式）。不看 X-Real-IP / True-Client-IP：代理不一定会清掉客户端自带的这两个头。
// 不包装 ResponseWriter。
func ProxyRealIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				r.RemoteAddr = net.JoinHostPort(ip.String(), "0")
			}
		}
		next.ServeHTTP(w, r)
	})
}
