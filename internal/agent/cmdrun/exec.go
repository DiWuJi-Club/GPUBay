package cmdrun

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
)

type Exec struct{}

func (Exec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// 目标机 LANG=zh_CN.utf8，本地化输出会破坏解析（如 df 表头）；后者覆盖前者。
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
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
	return stdout.Bytes(), &CmdError{Cmd: Line(name, args...), ExitCode: code, Stderr: tail(msg)}
}
