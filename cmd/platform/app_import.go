package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
)

// appImport 实现 `platform admin app import <dir>`：把 deploy/apps 风格的目录导入为 draft 应用 / 版本；
// 每行输出 `slug version created|version-added`，跳过的（符号链接）输出 `slug skipped: 原因`。
func appImport(ctx context.Context, cat *appdeploy.Catalog, args []string, out io.Writer) error {
	if len(args) != 2 || args[0] != "import" {
		return errors.New("用法：platform admin app import <目录>")
	}
	res, err := cat.ImportDir(ctx, audit.Actor{}, args[1])
	for _, r := range res {
		if r.Skipped != "" {
			fmt.Fprintf(out, "%s skipped: %s\n", r.Slug, r.Skipped)
			continue
		}
		state := "version-added"
		if r.Created {
			state = "created"
		}
		fmt.Fprintf(out, "%s %s %s\n", r.Slug, r.Version, state)
	}
	return err
}
