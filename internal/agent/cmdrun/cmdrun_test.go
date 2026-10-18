package cmdrun_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"spark-cloud/internal/agent/cmdrun"
)

func TestExecCapturesStdoutAndStderrTail(t *testing.T) {
	ctx := context.Background()
	out, err := cmdrun.Exec{}.Run(ctx, "sh", "-c", "echo hello; echo oops >&2; exit 3")
	var ce *cmdrun.CmdError
	if !errors.As(err, &ce) || ce.ExitCode != 3 || strings.TrimSpace(ce.Stderr) != "oops" || string(out) != "hello\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if ce.Error() != "sh -c echo hello; echo oops >&2; exit 3: exit 3: oops\n" {
		t.Fatalf("message = %q", ce.Error())
	}
	long := strings.Repeat("x", 600)
	_, err = cmdrun.Exec{}.Run(ctx, "sh", "-c", "echo "+long+" >&2; exit 1")
	if !errors.As(err, &ce) || len(ce.Stderr) != 500 || !strings.HasSuffix(ce.Error(), "x\n") { // 末尾 500 字节（含换行）
		t.Fatalf("tail = %d", len(ce.Stderr))
	}
	if _, err := (cmdrun.Exec{}).Run(ctx, "definitely-not-a-command-xyz"); !errors.As(err, &ce) || ce.ExitCode != -1 {
		t.Fatalf("missing command = %v", err)
	}
	if out, err := (cmdrun.Exec{}).Run(ctx, "sh", "-c", "exit 0"); err != nil || len(out) != 0 {
		t.Fatalf("ok = %q %v", out, err)
	}
}

func TestRecorderStubsAndSnapshots(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("docker inspect", `{"Id":"x"}`, nil)
	r.Stub("docker inspect --type container missing", "", &cmdrun.CmdError{Cmd: "docker inspect", ExitCode: 1, Stderr: "No such object"})
	out, err := r.Run(context.Background(), "docker", "inspect", "--type", "container", "ins-1")
	if err != nil || string(out) != `{"Id":"x"}` {
		t.Fatalf("stub = %q %v", out, err)
	}
	if _, err := r.Run(context.Background(), "docker", "inspect", "--type", "container", "missing"); err == nil {
		t.Fatal("more specific stub must win")
	}
	if out, err := r.Run(context.Background(), "tc", "qdisc", "show"); err != nil || len(out) != 0 {
		t.Fatalf("unstubbed = %q %v", out, err)
	}
	if r.Lines() != "docker inspect --type container ins-1\ndocker inspect --type container missing\ntc qdisc show" {
		t.Fatalf("lines = %q", r.Lines())
	}
	r.Reset()
	if len(r.Calls) != 0 {
		t.Fatal("reset")
	}
}

func TestStderrTailIsValidUTF8(t *testing.T) {
	ctx := context.Background()
	var ce *cmdrun.CmdError
	// 600 字节的多字节字符，500 不是 3 的倍数，切点会落在 rune 中间
	_, err := (cmdrun.Exec{}).Run(ctx, "sh", "-c", "printf '%s' \"a"+strings.Repeat("中", 200)+"\" >&2; exit 1")
	if !errors.As(err, &ce) || !utf8.ValidString(ce.Stderr) || len(ce.Stderr) > 500 || !strings.HasSuffix(ce.Stderr, "中") || !utf8.ValidString(ce.Error()) {
		t.Fatalf("multibyte tail = %d %q", len(ce.Stderr), ce.Stderr)
	}
	_, err = (cmdrun.Exec{}).Run(ctx, "sh", "-c", "printf '\\377\\376abc\\200' >&2; exit 1")
	if !errors.As(err, &ce) || !utf8.ValidString(ce.Stderr) || !strings.Contains(ce.Stderr, "abc") {
		t.Fatalf("invalid bytes = %q", ce.Stderr)
	}
	_, err = (cmdrun.Exec{}).Run(ctx, "sh", "-c", "i=0; while [ $i -lt 400 ]; do printf '\\377'; i=$((i+1)); done >&2; exit 1")
	if !errors.As(err, &ce) || !utf8.ValidString(ce.Stderr) || len(ce.Stderr) > 500 {
		t.Fatalf("invalid long = %d", len(ce.Stderr))
	}
	// 命令名含非法 UTF-8 的回退路径
	_, err = (cmdrun.Exec{}).Run(ctx, "no-such-\xff-cmd")
	if !errors.As(err, &ce) || !utf8.ValidString(ce.Stderr) {
		t.Fatalf("fallback = %q", ce.Stderr)
	}
}
