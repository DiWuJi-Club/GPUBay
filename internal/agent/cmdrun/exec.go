package cmdrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// DefaultTimeout 是调用方 ctx 没有 deadline 时单条命令的兜底时限：任何系统命令（docker、mount、tc…）都不能无限卡住。
// 可能更久的操作（docker pull、可能隐式拉取镜像的 docker create）由调用方给出显式 deadline。
const DefaultTimeout = 10 * time.Minute

// waitDelay：ctx 到期杀掉进程后，最多再等这么久让输出管道关闭（孙进程占着管道时 Wait 不至于跟着卡住）。
const waitDelay = 5 * time.Second

type Exec struct {
	// Backstop 覆盖 DefaultTimeout（测试用）；零值即 DefaultTimeout。
	Backstop time.Duration
}

func (e Exec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return e.RunInput(ctx, nil, name, args...)
}

func (e Exec) RunInput(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	limit := time.Duration(0)
	if _, ok := ctx.Deadline(); !ok {
		limit = e.Backstop
		if limit <= 0 {
			limit = DefaultTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = waitDelay
	// 目标机 LANG=zh_CN.utf8，本地化输出会破坏解析（如 df 表头）；后者覆盖前者。
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	msg := stderr.Bytes()
	if len(msg) == 0 {
		msg = []byte(err.Error())
	}
	ce := &CmdError{Cmd: Line(name, args...), ExitCode: code, Stderr: tail(msg)}
	// 被 ctx 终止时说清楚原因（否则只看到 "signal: killed"），并保留 ctx 错误供 errors.Is 判断。
	if cerr := ctx.Err(); cerr != nil {
		reason := "命令超时被终止"
		if limit > 0 && errors.Is(cerr, context.DeadlineExceeded) {
			reason = fmt.Sprintf("命令超时被终止（超过兜底时限 %v）", limit)
		} else if errors.Is(cerr, context.Canceled) {
			reason = "命令被取消"
		}
		ce.Stderr = tail([]byte(reason + ": " + string(msg)))
		ce.cause = cerr
	}
	return stdout.Bytes(), ce
}
