package appdeploy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"spark-cloud/internal/platform/apperr"
)

// CheckShellSyntax 在进程内按 POSIX sh 语法解析脚本（不执行、不起子进程；生产镜像里没有 shell），
// 失败返回 app_version_invalid，消息形如 `script：第 12 行: reached EOF without matching if with fi`，各平台一致。
// ctx 保留给调用方（解析是纯 CPU，64KB 上限内很快）。
func CheckShellSyntax(_ context.Context, script string) error {
	_, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(script), "")
	if err == nil {
		return nil
	}
	var pe syntax.ParseError
	var le syntax.LangError
	switch {
	case errors.As(err, &pe):
		return apperr.AppVersionInvalid("script", fmt.Sprintf("第 %d 行: %s", pe.Pos.Line(), pe.Text))
	case errors.As(err, &le):
		return apperr.AppVersionInvalid("script", fmt.Sprintf("第 %d 行: %s", le.Pos.Line(), strings.TrimPrefix(le.Error(), le.Pos.String()+": ")))
	}
	return apperr.AppVersionInvalid("script", err.Error())
}
