package appdeploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"spark-cloud/internal/appdeploy"
)

func TestParseImageEnviron(t *testing.T) {
	raw := strings.Join([]string{
		"PATH=/usr/local/cuda/bin:/usr/bin:/bin", "CUDA_HOME=/usr/local/cuda", "HOSTNAME=abc", "HOME=/root", "PWD=/", "OLDPWD=/x",
		"SHLVL=1", "TERM=xterm", "_=/usr/bin/env", "SPARK_APP=evil", "SPARK_TEST_INIT_STAT=/x", "bad-name=1", "1X=2", "lower_ok=3", "NOEQ",
		"EMPTY=", "Q=it's", "ML=a\nb", "IFS=x", "PPID=1", "UID=0", "SHELLOPTS=x", "BASH_FUNC_f%%=() { :; }", "DUP=1", "DUP=2", "EQ=a=b", "=nameless",
	}, "\x00") + "\x00PARTIAL=trunc" // 截断在半个条目上：最后一段没有 NUL 结尾，丢弃
	got := appdeploy.ParseImageEnviron([]byte(raw))
	want := []appdeploy.EnvKV{{"PATH", "/usr/local/cuda/bin:/usr/bin:/bin"}, {"CUDA_HOME", "/usr/local/cuda"}, {"lower_ok", "3"},
		{"EMPTY", ""}, {"Q", "it's"}, {"ML", "a\nb"}, {"DUP", "1"}, {"EQ", "a=b"}}
	if !slices.Equal(got, want) {
		t.Fatalf("parsed = %q\nwant     %q", got, want)
	}
	if got := appdeploy.ParseImageEnviron(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty environ = %#v (want empty, non-nil: image.env still written)", got)
	}
}

func TestParseImageEnvironCaps(t *testing.T) {
	var b strings.Builder
	for i := 0; i < appdeploy.MaxImageEnvVars+50; i++ {
		b.WriteString("V" + strconv.Itoa(i) + "=x\x00")
	}
	if got := appdeploy.ParseImageEnviron([]byte(b.String())); len(got) != appdeploy.MaxImageEnvVars {
		t.Fatalf("count cap: %d", len(got))
	}
	// 单个值 + 引号展开后超过总量上限：跳过这一项，后面的照收
	big := "BIG=" + strings.Repeat("'", appdeploy.MaxImageEnvBytes/2) + "\x00SMALL=1\x00"
	got := appdeploy.ParseImageEnviron([]byte(big))
	if !slices.Equal(got, []appdeploy.EnvKV{{"SMALL", "1"}}) {
		t.Fatalf("size cap: %d entries, first %q", len(got), got[0].Name)
	}
	if n := len(appdeploy.RenderImageEnv(got)); n > appdeploy.MaxImageEnvBytes {
		t.Fatalf("rendered %d bytes", n)
	}
}

// image.env 由 sh 加载：引号、换行、等号原样还原。
func TestRenderImageEnvRoundTrip(t *testing.T) {
	kvs := []appdeploy.EnvKV{{"Q", "it's $(touch /tmp/x) `x` \\n"}, {"ML", "a\nb"}, {"lower_ok", "a=b"}, {"EMPTY", ""}}
	f := filepath.Join(t.TempDir(), "image.env")
	if err := os.WriteFile(f, appdeploy.RenderImageEnv(kvs), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kv := range kvs {
		out, err := exec.Command("/bin/sh", "-c", `. "$1"; eval "printf '%s' \"\$$2\""`, "sh", f, kv.Name).Output()
		if err != nil || string(out) != kv.Value {
			t.Fatalf("%s = %q %v, want %q", kv.Name, out, err, kv.Value)
		}
	}
}

// Runner 读镜像环境的命令在本机 sh 上真跑：base64（GNU 每 76 列换行）解回原文；文件不在时非零退出；超过上限截断后丢掉半个条目。
func TestImageEnvironCmdAndDecode(t *testing.T) {
	f := filepath.Join(t.TempDir(), "spark-image.environ")
	long := strings.Repeat("x", 300) // 让 base64 输出折行
	if err := os.WriteFile(f, []byte("PATH=/usr/local/cuda/bin:/usr/bin\x00LONG="+long+"\x00HOME=/root\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", appdeploy.ImageEnvironCmd(f)).Output()
	if err != nil {
		t.Fatal(err)
	}
	got, err := appdeploy.DecodeImageEnviron(out)
	if want := []appdeploy.EnvKV{{"PATH", "/usr/local/cuda/bin:/usr/bin"}, {"LONG", long}}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("decoded = %q %v", got, err)
	}
	// 手工折行的 base64（GNU base64 的输出形状）
	if got, err := appdeploy.DecodeImageEnviron([]byte("UEFUSD0v\nYmluAA==\n")); err != nil || !slices.Equal(got, []appdeploy.EnvKV{{"PATH", "/bin"}}) {
		t.Fatalf("wrapped = %q %v", got, err)
	}
	if _, err := appdeploy.DecodeImageEnviron([]byte("not base64!")); err == nil {
		t.Fatal("garbage accepted")
	}
	if err := exec.Command("/bin/sh", "-c", appdeploy.ImageEnvironCmd(f+".missing")).Run(); err == nil {
		t.Fatal("missing file must exit non-zero")
	}
	big := strings.Repeat("A=1\x00", appdeploy.MaxImageEnvironBytes/4) + "B=2\x00"
	os.WriteFile(f, []byte(big), 0o644)
	out, _ = exec.Command("/bin/sh", "-c", appdeploy.ImageEnvironCmd(f)).Output()
	if got, err := appdeploy.DecodeImageEnviron(out); err != nil || !slices.Equal(got, []appdeploy.EnvKV{{"A", "1"}}) {
		t.Fatalf("truncated = %q %v", got, err)
	}
}
