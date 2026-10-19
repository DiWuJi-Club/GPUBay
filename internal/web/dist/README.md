# 前端构建产物目录

`console/` 与 `admin/` 由 `make web`（`cd web && npm run build`）生成并被 `internal/web` embed 进二进制，已 gitignore。
本文件入库是为了保证 `//go:embed dist` 在未构建时也有文件可嵌入；未构建时访问页面会返回 503「前端尚未构建，请执行 make web」。
