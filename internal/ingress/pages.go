package ingress

import (
	"fmt"
	"html"
	"net/http"

	"spark-cloud/internal/rental"
)

const (
	PageNotOpen       = "该端口未开放"
	PageNotFound      = "该地址不存在"
	PageUpstreamFmt   = "无法连接到实例端口，请确认服务已在容器内监听 %d"
	PageForbidden     = "无权访问该实例"
	PageBadTarget     = "跳转目标不正确"
	PageSuspended     = "实例所在机器暂时失联，计时已暂停，请稍后重试"
	PageCreating      = "实例正在开通，请稍后再试"
	PageStopped       = "实例已到期停机，续费后可访问"
	PageReleased      = "实例已释放"
	PageShuttingDown  = "平台正在重启，请稍后刷新"
	PageTicketInvalid = "登录票据无效或已过期，请重新访问"
)

// NotRunningPage 按实例状态给出 503 页文案。
func NotRunningPage(r rental.IngressRoute) string {
	switch {
	case r.State == rental.StateRunning && r.Suspended:
		return PageSuspended
	case r.State == rental.StateCreating:
		return PageCreating
	case r.State == rental.StateStopped:
		return PageStopped
	default:
		return PageReleased
	}
}

const pageTpl = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>%s</title>%s
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font-family:system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center;background:#f5f7fa;color:#303133}
main{background:#fff;padding:32px 40px;border-radius:8px;box-shadow:0 2px 12px rgba(0,0,0,.08);max-width:520px}h1{font-size:18px;margin:0 0 12px}p{margin:0;color:#606266;line-height:1.7}</style></head>
<body><main><h1>%s</h1><p>%s</p></main></body></html>`

// WritePage 输出一个简单的状态页；refreshSeconds > 0 时自动刷新（停机 / 失联页用）。
// title / body 一律 HTML 转义，调用方可以安全地放入 Host、路径等请求内容。
func WritePage(w http.ResponseWriter, status int, title, body string, refreshSeconds int) {
	refresh := ""
	if refreshSeconds > 0 {
		refresh = fmt.Sprintf(`<meta http-equiv="refresh" content="%d">`, refreshSeconds)
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprintf(w, pageTpl, html.EscapeString(title), refresh, html.EscapeString(title), html.EscapeString(body))
}
