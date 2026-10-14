// Package migrations 内嵌所有 goose SQL 迁移文件。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
