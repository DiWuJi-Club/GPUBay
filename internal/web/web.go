// Package web 把前端构建产物 embed 进 platform 二进制，并以单页应用方式托管：
// 存在的文件直接返回（带哈希的 assets 长缓存），其余路径回退到 index.html（不缓存）。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist/console 与 dist/admin 由前端构建生成（gitignore）；dist/README.md 入库保证 embed 始终有文件。
// all: 前缀让 Vite 产出的 _ 开头文件（如 _plugin-vue_export-helper-*.js）也被嵌入；点文件由 NewHandler 拒绝。
//
//go:embed all:dist
var dist embed.FS

// CSP 是静态页面的内容安全策略：只允许同源脚本与请求；style-src 'unsafe-inline' 是 Element Plus 行内样式所需。
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
	"font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'self'"

// Console 托管用户控制台（挂在 :8080 的 /）。
func Console() http.Handler { return NewHandler(sub("dist/console"), "/") }

// Admin 托管管理后台（挂在 :8081 的 /admin/）。
func Admin() http.Handler { return NewHandler(sub("dist/admin"), "/admin/") }

func sub(dir string) fs.FS {
	s, err := fs.Sub(dist, dir) // 只校验路径格式；目录不存在时 Open 会失败，处理器返回 503
	if err != nil {
		panic(err)
	}
	return s
}

// NewHandler 返回单页应用处理器。prefix 是挂载前缀（"/" 或 "/admin/"），请求路径去掉前缀后在 fsys 中查找。
func NewHandler(fsys fs.FS, prefix string) http.Handler {
	files := http.FileServerFS(fsys)
	trim := strings.TrimSuffix(prefix, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, trim)), "/")
		if rel != "" && rel != "index.html" && !hasDotSegment(rel) {
			if st, err := fs.Stat(fsys, rel); err == nil && !st.IsDir() {
				if strings.HasPrefix(rel, "assets/") {
					h.Set("Cache-Control", "public, max-age=31536000, immutable") // 文件名带内容哈希
				} else {
					h.Set("Cache-Control", "public, max-age=3600") // favicon、brand/ 等
				}
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/" + rel
				files.ServeHTTP(w, r2)
				return
			}
		}
		index, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			h.Set("Content-Type", "text/plain; charset=utf-8")
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("前端尚未构建，请执行 make web\n"))
			return
		}
		h.Set("Cache-Control", "no-cache") // 发布新版本后刷新即生效
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(index)
	})
}

// hasDotSegment 报告路径中是否有以 . 开头的段（.env、.git/ 等），这类文件永不对外提供。
func hasDotSegment(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}
