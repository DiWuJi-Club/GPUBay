package appdeploy

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"spark-cloud/internal/platform/apperr"
)

// CheckShellSyntax 用 `/bin/sh -n` 检查脚本语法（不执行），5 秒超时；
// 失败返回 app_version_invalid，消息为 sh 输出的第一行（去掉临时文件路径），形如 `line 12: Syntax error: ...`。
func CheckShellSyntax(ctx context.Context, script string) error {
	f, err := os.CreateTemp("", "spark-app-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return err
	}
	f.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/sh", "-n", f.Name()).CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	msg = strings.TrimPrefix(msg, f.Name()+": ")
	if msg == "" {
		msg = err.Error()
	}
	return apperr.AppVersionInvalid("script", msg)
}
