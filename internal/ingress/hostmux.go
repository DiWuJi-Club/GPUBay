package ingress

import (
	"net/http"

	"spark-cloud/internal/platform/httpx"
)

// HostMux 按 Host 分流：实例域名（ins-<id>-<port>.<ingress_domain>）→ ingress，其它（<domain>、IP、ssh.<domain> 等）→ 控制台。
func HostMux(ingressDomain string, ingress, console http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := ParseHost(r.Host, ingressDomain); ok {
			ingress.ServeHTTP(w, r)
			return
		}
		console.ServeHTTP(w, r)
	})
}

// Front 是公网监听器的完整 Handler：HSTS → HostMux →（trustProxy 时 ProxyRealIP）→ ingress。
// 实例路径上的中间件都不包装 ResponseWriter，ingress 才能用 http.ResponseController 清除读写期限、劫持连接（WebSocket）。
// 控制台路由自己处理 RealIP（api.Server.TrustProxy）。
func Front(ingressDomain string, trustProxy bool, ingress, console http.Handler) http.Handler {
	if trustProxy {
		ingress = httpx.ProxyRealIP(ingress)
	}
	return httpx.HSTS(HostMux(ingressDomain, ingress, console))
}
