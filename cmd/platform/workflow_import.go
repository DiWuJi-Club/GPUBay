package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/comfywf"
)

const workflowImportUsage = "用法：platform admin workflow import <目录>"

// workflowImport 实现 `platform admin workflow import <dir>`：把 deploy/workflows 风格的目录导入为 draft 工作流 / 版本（不发布）；
// 每行输出 `slug version created|version-added`，跳过的（符号链接）输出 `slug skipped: 原因`，失败的输出 `slug error: 原因`
// （其它目录照常导入，最后以非零退出）。
func workflowImport(ctx context.Context, cat *comfywf.Catalog, args []string, out io.Writer) error {
	if len(args) != 2 || args[0] != "import" {
		return errors.New(workflowImportUsage)
	}
	res, err := cat.ImportDir(ctx, audit.Actor{}, args[1])
	for _, r := range res {
		switch {
		case r.Skipped != "":
			fmt.Fprintf(out, "%s skipped: %s\n", r.Slug, r.Skipped)
		case r.Err != nil:
			fmt.Fprintf(out, "%s error: %s\n", r.Slug, r.Err)
		case r.Created:
			fmt.Fprintf(out, "%s %s created\n", r.Slug, r.Version)
		default:
			fmt.Fprintf(out, "%s %s version-added\n", r.Slug, r.Version)
		}
	}
	return err
}
