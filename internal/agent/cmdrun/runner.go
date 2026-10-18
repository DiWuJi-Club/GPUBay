// Package cmdrun 封装 Agent 对系统命令的调用：生产用 Exec，测试用 Recorder 做命令快照。
package cmdrun

import (
	"context"
	"fmt"
	"strings"
)

type Runner interface {
	// Run 执行命令并返回 stdout；非零退出返回 *CmdError。
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Line 组出可读的命令行（不做 shell 转义，只用于日志、错误与快照）。
func Line(name string, args ...string) string {
	if len(args) == 0 {
		return name
	}
	return name + " " + strings.Join(args, " ")
}

const stderrTail = 500

type CmdError struct {
	Cmd      string
	ExitCode int
	Stderr   string // 末尾最多 500 字节
}

func (e *CmdError) Error() string { return fmt.Sprintf("%s: exit %d: %s", e.Cmd, e.ExitCode, e.Stderr) }

func tail(b []byte) string {
	if len(b) > stderrTail {
		b = b[len(b)-stderrTail:]
	}
	return string(b)
}
