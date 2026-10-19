package httpx

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

// ProxyRealIP 只在 SPARK_TRUST_PROXY 下使用（控制台与租户入口共用）：客户端 IP 取 X-Forwarded-For 自右数第 hops 项
// （hops = 我们自己的反向代理层数，最少 1；每层代理把它看到的对端追加到最右，更左的项都可能是客户端伪造的）。
// 该项可以是 IP 或 ip:port（[v6]:port）；不存在或解析不了时保留连接地址（解析不了时记一次 warn：多半是代理配置不对）。
// 不看 X-Real-IP / True-Client-IP 等其它头：代理不一定会清掉客户端自带的。
// RemoteAddr 保持 host:port 形式（httputil 的 SetXForwarded 只认这种形式）；不包装 ResponseWriter。
func ProxyRealIP(hops int) func(http.Handler) http.Handler {
	if hops < 1 {
		hops = 1
	}
	var warnOnce sync.Once
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, entry, ok := forwardedIP(r.Header.Values("X-Forwarded-For"), hops)
			if ip != nil {
				r.RemoteAddr = net.JoinHostPort(ip.String(), "0")
			} else if ok {
				warnOnce.Do(func() {
					slog.Default().Warn("X-Forwarded-For entry is not an IP; using connection address (check reverse proxy config)",
						"entry", entry, "trusted_proxy_hops", hops)
				})
			}
			next.ServeHTTP(w, r)
		})
	}
}

// forwardedIP 把所有 X-Forwarded-For 头按顺序拼起来，取自右数第 hops 项；ok 表示该项存在（ip 为 nil 即解析失败）。
func forwardedIP(values []string, hops int) (ip net.IP, entry string, ok bool) {
	var entries []string
	for _, v := range values {
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
	}
	if len(entries) < hops {
		return nil, "", false
	}
	entry = entries[len(entries)-hops]
	return parseForwarded(entry), entry, true
}

// parseForwarded 接受 IP、ip:port 与 [v6]:port（端口须是合法数字）。
func parseForwarded(s string) net.IP {
	if ip := net.ParseIP(s); ip != nil {
		return ip
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return net.IP(ap.Addr().Unmap().AsSlice())
	}
	return nil
}
