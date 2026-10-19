// Package cmdrun 封装 Agent 对系统命令的调用：生产用 Exec，测试用 Recorder 做命令快照。
package cmdrun

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

type Runner interface {
	// Run 执行命令并返回 stdout；非零退出返回 *CmdError。
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// RunInput 同 Run，但把 stdin 喂给子进程（如 iptables-restore 的事务输入）。
	RunInput(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
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
	cause    error  // 命令被 ctx 终止时的 ctx 错误（context.DeadlineExceeded / Canceled）
}

func (e *CmdError) Error() string { return fmt.Sprintf("%s: exit %d: %s", e.Cmd, e.ExitCode, e.Stderr) }

func (e *CmdError) Unwrap() error { return e.cause }

// tail 先把非法 UTF-8 替换成 U+FFFD，再取末尾最多 500 字节（切点落在 rune 边界上），保证结果是合法 UTF-8（proto3 string 要求）。
func tail(b []byte) string {
	s := strings.ToValidUTF8(string(b), "\uFFFD")
	if len(s) > stderrTail {
		cut := len(s) - stderrTail
		for cut < len(s) && !utf8.RuneStart(s[cut]) {
			cut++
		}
		s = s[cut:]
	}
	return s
}
