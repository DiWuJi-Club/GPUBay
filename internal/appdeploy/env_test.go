package appdeploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/platform/apperr"
)

func TestValidateEnvMatrix(t *testing.T) {
	s := goodSpec()
	if err := appdeploy.ValidateSpec(&s); err != nil {
		t.Fatal(err)
	}
	plain, secret, err := appdeploy.ValidateEnv(s, map[string]string{"API_KEY": "abcdefgh", "MAX_MODEL_LEN": "4096"})
	if err != nil {
		t.Fatal(err)
	}
	if plain["MODEL_ID"] != "Qwen/Qwen3-8B" || plain["MAX_MODEL_LEN"] != "4096" || plain["DEBUG"] != "false" || plain["NOTE"] != "" {
		t.Fatalf("plain = %v", plain)
	}
	if _, has := plain["NOTE"]; has {
		t.Fatal("unset optional var without default must be absent")
	}
	if secret["API_KEY"] != "abcdefgh" || len(secret) != 1 {
		t.Fatalf("secret = %v", secret)
	}
	if _, has := plain["API_KEY"]; has {
		t.Fatal("secret leaked into plain")
	}
	bad := map[string]struct {
		in   map[string]string
		code string
		msg  string
	}{
		"extra key":        {map[string]string{"FOO": "1"}, "app_env_invalid", "FOO"},
		"newline":          {map[string]string{"NOTE": "a\nb"}, "app_env_invalid", "备注"},
		"nul":              {map[string]string{"NOTE": "a\x00b"}, "app_env_invalid", "备注"},
		"bad utf8":         {map[string]string{"NOTE": "\xff"}, "app_env_invalid", "备注"},
		"too long":         {map[string]string{"NOTE": strings.Repeat("x", 257)}, "app_env_invalid", "备注"},
		"int not number":   {map[string]string{"MAX_MODEL_LEN": "big"}, "app_env_invalid", "最大上下文"},
		"int out of range": {map[string]string{"MAX_MODEL_LEN": "1"}, "app_env_invalid", "最大上下文"},
		"bool":             {map[string]string{"DEBUG": "yes"}, "app_env_invalid", "调试"},
		"enum":             {map[string]string{"MODEL_ID": "Qwen/Other"}, "app_env_invalid", "模型"},
		"pattern":          {map[string]string{"API_KEY": "has space!"}, "app_env_invalid", "接口密钥"},
		"secret short":     {map[string]string{"API_KEY": "abc"}, "app_env_invalid", "接口密钥"},
	}
	for name, c := range bad {
		_, _, err := appdeploy.ValidateEnv(s, c.in)
		ae := apperr.As(err)
		if ae == nil || ae.Code != c.code || !strings.Contains(ae.Message, c.msg) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// pattern 是全匹配（规格 §3.3）：未锚定的正则也不能只匹配一部分
	s3 := goodSpec()
	s3.Env[3].Pattern = "[a-z]+"
	if err := appdeploy.ValidateSpec(&s3); err != nil {
		t.Fatal(err)
	}
	if _, _, err := appdeploy.ValidateEnv(s3, map[string]string{"NOTE": "abc; rm"}); apperr.As(err) == nil || apperr.As(err).Code != "app_env_invalid" {
		t.Fatalf("partial pattern match accepted: %v", err)
	}
	if plain, _, err := appdeploy.ValidateEnv(s3, map[string]string{"NOTE": "abc"}); err != nil || plain["NOTE"] != "abc" {
		t.Fatalf("full pattern match rejected: %v %v", plain, err)
	}
	// required 且无默认 → app_env_required
	s2 := goodSpec()
	s2.Env[0].Default = ""
	if err := appdeploy.ValidateSpec(&s2); err != nil {
		t.Fatal(err)
	}
	_, _, err = appdeploy.ValidateEnv(s2, nil)
	if ae := apperr.As(err); ae == nil || ae.Code != "app_env_required" || !strings.Contains(ae.Message, "模型") {
		t.Fatalf("required = %v", err)
	}
}

// 未声明的变量名来自用户输入：回显进错误消息前截到 64 字节，且不切断 UTF-8 字符。
func TestValidateEnvTruncatesUnknownName(t *testing.T) {
	sp := goodSpec()
	if err := appdeploy.ValidateSpec(&sp); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{strings.Repeat("A", 5000), strings.Repeat("模", 30)} {
		_, _, err := appdeploy.ValidateEnv(sp, map[string]string{name: "x"})
		ae := apperr.As(err)
		if ae == nil || ae.Code != "app_env_invalid" || len(ae.Message) > 64+len("…：不是该应用声明的变量") || !utf8.ValidString(ae.Message) ||
			!strings.Contains(ae.Message, "…") {
			t.Fatalf("unknown name %d bytes → %v", len(name), err)
		}
	}
	if _, _, err := appdeploy.ValidateEnv(sp, map[string]string{"SHORT": "x"}); apperr.As(err) == nil || apperr.As(err).Message != "SHORT：不是该应用声明的变量" {
		t.Fatalf("short name = %v", err)
	}
}

func TestRenderEnvEscapes(t *testing.T) {
	kvs := []appdeploy.EnvKV{{Name: "NOTE", Value: `'; touch /tmp/pwned; '`}, {Name: "B", Value: "a'b$(id)`id` \\ \"q\""}}
	got := string(appdeploy.RenderEnv([]appdeploy.EnvKV{{Name: "SPARK_APP", Value: "vllm"}}, kvs))
	want := "export SPARK_APP='vllm'\nexport NOTE=''\\''; touch /tmp/pwned; '\\'''\nexport B='a'\\''b$(id)`id` \\ \"q\"'\n"
	if got != want {
		t.Fatalf("render =\n%s\nwant\n%s", got, want)
	}
	// 纵深防御：名字不合规（格式错、保留名、SPARK_ 前缀、LD_ 前缀）的行不写进 env
	got2 := string(appdeploy.RenderEnv(
		[]appdeploy.EnvKV{{Name: "SPARK_APP", Value: "vllm"}, {Name: "bad;name", Value: "x"}},
		[]appdeploy.EnvKV{{Name: "PATH", Value: "/evil"}, {Name: "X=1; touch /tmp/p; Y", Value: "x"}, {Name: "SPARK_PORT", Value: "1"},
			{Name: "LD_AUDIT", Value: "x"}, {Name: "UID", Value: "0"}, {Name: "lower", Value: "x"}, {Name: "OK", Value: "v"}}))
	if want2 := "export SPARK_APP='vllm'\nexport OK='v'\n"; got2 != want2 {
		t.Fatalf("invalid names rendered:\n%s", got2)
	}
	// 真用 /bin/sh 加载：值原样回来，没有执行任何命令
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(got), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "pwned")
	kvs2 := []appdeploy.EnvKV{{Name: "NOTE", Value: "'; touch " + marker + "; '"}}
	os.WriteFile(filepath.Join(dir, "env2"), appdeploy.RenderEnv(nil, kvs2), 0o600)
	cmd := exec.Command("/bin/sh", "-c", `. ./env2 && printf '%s' "$NOTE"`)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil || string(out) != kvs2[0].Value {
		t.Fatalf("sh load = %q %v", out, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("injected command executed")
	}
}
