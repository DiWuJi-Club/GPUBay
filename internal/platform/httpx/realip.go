package httpx

import (
	"net"
	"net/http"
	"strings"
)

// ProxyRealIP 只在 SPARK_TRUST_PROXY 下使用（控制台与租户入口共用）：客户端 IP 取 X-Forwarded-For 自右数第 hops 项
// （hops = 我们自己的反向代理层数，最少 1；每层代理把它看到的对端追加到最右，更左的项都可能是客户端伪造的）。
// 该项不存在或不是合法 IP 时保留连接地址。不看 X-Real-IP / True-Client-IP 等其它头：代理不一定会清掉客户端自带的。
// RemoteAddr 保持 host:port 形式（httputil 的 SetXForwarded 只认这种形式）；不包装 ResponseWriter。
func ProxyRealIP(hops int) func(http.Handler) http.Handler {
	if hops < 1 {
		hops = 1
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip := forwardedIP(r.Header.Values("X-Forwarded-For"), hops); ip != nil {
				r.RemoteAddr = net.JoinHostPort(ip.String(), "0")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// forwardedIP 把所有 X-Forwarded-For 头按顺序拼起来，取自右数第 hops 项。
func forwardedIP(values []string, hops int) net.IP {
	var entries []string
	for _, v := range values {
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
	}
	if len(entries) < hops {
		return nil
	}
	return net.ParseIP(entries[len(entries)-hops])
}
