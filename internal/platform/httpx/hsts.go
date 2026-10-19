package httpx

import "net/http"

// HSTS 在 TLS 连接上加 Strict-Transport-Security（一年）；明文连接（dev、代理后）不加。
func HSTS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}
