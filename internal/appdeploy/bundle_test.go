package appdeploy_test

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"spark-cloud/internal/appdeploy"
)

func TestBundleContents(t *testing.T) {
	s := goodSpec()
	if err := appdeploy.ValidateSpec(&s); err != nil {
		t.Fatal(err)
	}
	in := appdeploy.BundleInput{Slug: "vllm", InstanceID: "ins-aaaaaaaa", AppRoot: "/root/data", SpecHash: "abc", Spec: s, Script: "echo hi\n",
		Env: map[string]string{"MODEL_ID": "Qwen/Qwen3-8B"}, Secret: map[string]string{"API_KEY": "abcdefgh"},
		HFEndpoint: "https://hf-mirror.com", PipIndexURL: "https://pypi.tuna.tsinghua.edu.cn/simple", GitMirror: "https://gh-proxy.com/"}
	b, err := appdeploy.Bundle(in, []byte("#!/bin/sh\n"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	var names []string
	files := map[string]string{}
	modes := map[string]int64{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		files[h.Name] = string(body)
		modes[h.Name] = h.Mode
	}
	if strings.Join(names, ",") != "run.sh,app.sh,spec.json,env" {
		t.Fatalf("entries = %v", names)
	}
	if modes["run.sh"] != 0o755 || modes["env"] != 0o600 || modes["app.sh"] != 0o644 {
		t.Fatalf("modes = %v", modes)
	}
	env := files["env"]
	for _, line := range []string{
		"export SPARK_APP='vllm'", "export SPARK_APP_DIR='/root/data/.spark-apps/vllm'", "export SPARK_DATA_DIR='/root/data'", "export SPARK_PORT='8000'",
		"export SPARK_PORT_2=''", "export SPARK_INSTANCE_ID='ins-aaaaaaaa'", "export SPARK_MODELS_DIR='/root/data/models'", "export SPARK_SPEC_HASH='abc'",
		"export SPARK_TIMEOUT_S='3600'", "export SPARK_HEALTH_PATH='/health'", "export SPARK_MIN_FREE_GIB='0'", "export SPARK_HEALTH_ADDR='127.0.0.1:8000'",
		"export HF_ENDPOINT='https://hf-mirror.com'", "export MODELSCOPE_CACHE='/root/data/models/modelscope'", "export HF_HOME='/root/data/models/hf'",
		"export PIP_INDEX_URL='https://pypi.tuna.tsinghua.edu.cn/simple'", "export SPARK_GIT_MIRROR='https://gh-proxy.com/'",
		"export MODEL_ID='Qwen/Qwen3-8B'", "export API_KEY='abcdefgh'",
	} {
		if !strings.Contains(env, line+"\n") {
			t.Errorf("env missing %q:\n%s", line, env)
		}
	}
	if !strings.Contains(files["spec.json"], `"port":8000`) {
		t.Fatalf("spec.json = %s", files["spec.json"])
	}
	// 可重复：同样输入同样字节（mtime 固定）
	b2, _ := appdeploy.Bundle(in, []byte("#!/bin/sh\n"))
	if !bytes.Equal(b, b2) {
		t.Fatal("bundle not deterministic")
	}
	for _, bad := range []struct{ env, secret map[string]string }{
		{env: map[string]string{"PATH": "/evil"}},
		{env: map[string]string{"SPARK_APP": "x"}},
		{env: map[string]string{"LD_AUDIT": "x"}},
		{env: map[string]string{"X; touch /tmp/p": "x"}},
		{secret: map[string]string{"bad-name": "abcdefgh"}},
		{secret: map[string]string{"IFS": "abcdefgh"}},
	} {
		badIn := in
		badIn.Env, badIn.Secret = bad.env, bad.secret
		if _, err := appdeploy.Bundle(badIn, []byte("#!/bin/sh\n")); err == nil {
			t.Errorf("bundle accepted bad env name: %v %v", bad.env, bad.secret)
		}
	}
	noPorts := in
	noPorts.Spec.Ports = nil
	if _, err := appdeploy.Bundle(noPorts, []byte("#!/bin/sh\n")); err == nil {
		t.Fatal("spec without ports accepted")
	}
	if appdeploy.MaxBundleBytes != 4<<20 {
		t.Fatalf("MaxBundleBytes = %d, want 4MiB (plan 11 D2)", appdeploy.MaxBundleBytes)
	}
	in.Script = strings.Repeat("x", appdeploy.MaxBundleBytes)
	if _, err := appdeploy.Bundle(in, []byte("#!/bin/sh\n")); err == nil {
		t.Fatal("oversized bundle accepted")
	}
	if got := appdeploy.AppDir("/tmp/x", "open-webui"); got != "/tmp/x/.spark-apps/open-webui" {
		t.Fatalf("AppDir = %q", got)
	}
}

// 读到了镜像环境（哪怕是空的）→ 包里带 image.env（0600）；名字不合规的行不写出（纵深防御）。
func TestBundleImageEnv(t *testing.T) {
	s := goodSpec()
	in := appdeploy.BundleInput{Slug: "vllm", InstanceID: "ins-aaaaaaaa", AppRoot: "/root/data", SpecHash: "abc", Spec: s, Script: "echo hi\n",
		ImageEnv: []appdeploy.EnvKV{{"CUDA_HOME", "/usr/local/cuda"}, {"Q", "it's"}, {"X; touch /tmp/p", "x"}}}
	files := map[string]string{}
	modes := map[string]int64{}
	var names []string
	b, err := appdeploy.Bundle(in, []byte("#!/bin/sh\n"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		files[h.Name], modes[h.Name] = string(body), h.Mode
	}
	if strings.Join(names, ",") != "run.sh,app.sh,spec.json,env,image.env" || modes["image.env"] != 0o600 {
		t.Fatalf("entries = %v modes = %v", names, modes)
	}
	if got, want := files["image.env"], "export CUDA_HOME='/usr/local/cuda'\nexport Q='it'\\''s'\n"; got != want {
		t.Fatalf("image.env = %q, want %q", got, want)
	}
	in.ImageEnv = []appdeploy.EnvKV{}
	if b, err = appdeploy.Bundle(in, []byte("#!/bin/sh\n")); err != nil || !bytes.Contains(b, []byte("image.env")) {
		t.Fatalf("empty image env must still write image.env: %v", err)
	}
}

func TestUploadCommand(t *testing.T) {
	got := appdeploy.UploadCommand(appdeploy.AppDir("/root/data", "vllm"))
	want := "mkdir -p '/root/data/.spark-apps/vllm' && chmod 700 '/root/data/.spark-apps/vllm' && rm -rf '/root/data/.spark-apps/vllm/workflows'" +
		" && tar -x -C '/root/data/.spark-apps/vllm' -f - && chmod 600 '/root/data/.spark-apps/vllm/env'" +
		" && { [ ! -e '/root/data/.spark-apps/vllm/image.env' ] || chmod 600 '/root/data/.spark-apps/vllm/image.env'; }"
	if got != want {
		t.Fatalf("UploadCommand =\n%s\nwant\n%s", got, want)
	}
}

// 真跑上传命令：有 / 没有 image.env 都成功退出，env 与 image.env 都是 0600（tar 不保留权限时也由 chmod 补上）。
func TestUploadCommandRuns(t *testing.T) {
	s := goodSpec()
	in := appdeploy.BundleInput{Slug: "vllm", InstanceID: "ins-aaaaaaaa", AppRoot: "/root/data", SpecHash: "abc", Spec: s, Script: "echo hi\n"}
	for _, tc := range []struct {
		name     string
		imageEnv []appdeploy.EnvKV
		stale    bool // 目录里已有上一次的 image.env
	}{{"without", nil, false}, {"with", []appdeploy.EnvKV{{"CUDA_HOME", "/usr/local/cuda"}}, false}, {"stale", nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), ".spark-apps", "vllm")
			if tc.stale {
				os.MkdirAll(dir, 0o700)
				os.WriteFile(filepath.Join(dir, "image.env"), []byte("export A='1'\n"), 0o644)
			}
			in.ImageEnv = tc.imageEnv
			b, err := appdeploy.Bundle(in, []byte("#!/bin/sh\n"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-c", "umask 022; "+appdeploy.UploadCommand(dir))
			cmd.Stdin = bytes.NewReader(b)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("upload: %v %s", err, out)
			}
			for _, f := range []string{"env", "image.env"} {
				st, err := os.Stat(filepath.Join(dir, f))
				if f == "image.env" && tc.imageEnv == nil && !tc.stale {
					if err == nil {
						t.Fatal("image.env created without image environment")
					}
					continue
				}
				if err != nil || st.Mode().Perm() != 0o600 {
					t.Fatalf("%s: %v %v", f, st, err)
				}
			}
		})
	}
}

// readTar 返回 tar 里的条目名（按顺序）、内容与权限位。
func readTar(t *testing.T, b []byte) ([]string, map[string]string, map[string]int64) {
	t.Helper()
	var names []string
	files, modes := map[string]string{}, map[string]int64{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names, files, modes
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		files[h.Name], modes[h.Name] = string(body), h.Mode
	}
}

// 计划 11：Extra 追加在固定文件之后（顺序不变、Mode 原样、0 = 0644）；名字非法 / 含 .. / 与固定文件或彼此重名 → 错误；总大小仍受 4MiB 限制。
func TestBundleExtra(t *testing.T) {
	in := appdeploy.BundleInput{Slug: "comfyui", InstanceID: "ins-aaaaaaaa", AppRoot: "/root/data", SpecHash: "abc", Spec: goodSpec(), Script: "echo hi\n",
		Extra: []appdeploy.BundleFile{
			{Name: "workflows/manifest.json", Mode: 0o644, Body: []byte(`{"workflows":[]}`)},
			{Name: "workflows/sdxl-txt2img.json", Body: []byte(`{"nodes":[]}`)},
		}}
	b, err := appdeploy.Bundle(in, []byte("#!/bin/sh\n"))
	if err != nil {
		t.Fatal(err)
	}
	names, files, modes := readTar(t, b)
	if strings.Join(names, ",") != "run.sh,app.sh,spec.json,env,workflows/manifest.json,workflows/sdxl-txt2img.json" {
		t.Fatalf("entries = %v", names)
	}
	if files["workflows/sdxl-txt2img.json"] != `{"nodes":[]}` || modes["workflows/manifest.json"] != 0o644 || modes["workflows/sdxl-txt2img.json"] != 0o644 {
		t.Fatalf("extra files = %v %v", files, modes)
	}
	if b2, _ := appdeploy.Bundle(in, []byte("#!/bin/sh\n")); !bytes.Equal(b, b2) {
		t.Fatal("bundle with extra not deterministic")
	}
	for _, name := range []string{
		"", "Workflows/x.json", "/etc/passwd", "../x", "workflows/../../x", "workflows/./x", "workflows//x", "workflows/", ".installed",
		"workflows/x y.json", "workflows/" + strings.Repeat("a", 120), // 超过 128
		"run.sh", "app.sh", "spec.json", "env", "image.env", "env/x", "workflows/manifest.json", // 与固定文件 / 前一个 Extra 重名
	} {
		bad := in
		bad.Extra = append(slices.Clone(in.Extra[:1]), appdeploy.BundleFile{Name: name, Body: []byte("x")})
		if _, err := appdeploy.Bundle(bad, []byte("#!/bin/sh\n")); err == nil {
			t.Errorf("extra name %q accepted", name)
		}
	}
	big := in
	big.Extra = []appdeploy.BundleFile{{Name: "workflows/big.json", Body: bytes.Repeat([]byte("x"), appdeploy.MaxBundleBytes)}}
	if _, err := appdeploy.Bundle(big, []byte("#!/bin/sh\n")); err == nil || !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("oversized extra = %v", err)
	}
	// 3.5MiB 的工作流文件放得下（D2：8 × 256KB 远小于 4MiB）
	big.Extra[0].Body = bytes.Repeat([]byte("x"), 3<<20+512<<10)
	if _, err := appdeploy.Bundle(big, []byte("#!/bin/sh\n")); err != nil {
		t.Fatalf("3.5MiB extra rejected: %v", err)
	}
}

func TestReservedEnvGitMirror(t *testing.T) {
	in := appdeploy.BundleInput{Slug: "comfyui", AppRoot: "/root/data", Spec: goodSpec(), GitMirror: "https://gh-proxy.com/"}
	kvs := appdeploy.ReservedEnv(in)
	if last := kvs[len(kvs)-1]; last.Name != "SPARK_GIT_MIRROR" || last.Value != "https://gh-proxy.com/" {
		t.Fatalf("last reserved = %+v", last)
	}
	in.GitMirror = ""
	kvs = appdeploy.ReservedEnv(in)
	if last := kvs[len(kvs)-1]; last.Name != "SPARK_GIT_MIRROR" || last.Value != "" {
		t.Fatalf("direct = %+v", last)
	}
	// 用户变量不能冒充它
	if _, err := appdeploy.Bundle(appdeploy.BundleInput{Slug: "comfyui", AppRoot: "/root/data", Spec: goodSpec(), Env: map[string]string{"SPARK_GIT_MIRROR": "x"}},
		[]byte("#!/bin/sh\n")); err == nil {
		t.Fatal("user env SPARK_GIT_MIRROR accepted")
	}
}

// 真跑上传命令：上一次留下的 workflows/ 先删掉（移除的工作流不残留），本次包里的写出；不带工作流的包也清掉旧目录。
func TestUploadCommandReplacesWorkflows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".spark-apps", "comfyui")
	os.MkdirAll(filepath.Join(dir, "workflows"), 0o700)
	os.WriteFile(filepath.Join(dir, "workflows", "old.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, ".installed"), []byte("keep\n"), 0o644)
	in := appdeploy.BundleInput{Slug: "comfyui", InstanceID: "ins-aaaaaaaa", AppRoot: "/root/data", SpecHash: "abc", Spec: goodSpec(), Script: "echo hi\n",
		Extra: []appdeploy.BundleFile{{Name: "workflows/manifest.json", Mode: 0o644, Body: []byte(`{}`)}, {Name: "workflows/new.json", Mode: 0o644, Body: []byte(`{"a":1}`)}}}
	upload := func(in appdeploy.BundleInput) {
		t.Helper()
		b, err := appdeploy.Bundle(in, []byte("#!/bin/sh\n"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", appdeploy.UploadCommand(dir))
		cmd.Stdin = bytes.NewReader(b)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("upload: %v %s", err, out)
		}
	}
	upload(in)
	if _, err := os.Stat(filepath.Join(dir, "workflows", "old.json")); err == nil {
		t.Fatal("stale workflow file survived upload")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "workflows", "new.json")); string(got) != `{"a":1}` {
		t.Fatalf("new.json = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ".installed")); string(got) != "keep\n" {
		t.Fatal("upload touched files outside workflows/")
	}
	in.Extra = nil
	upload(in)
	if _, err := os.Stat(filepath.Join(dir, "workflows")); err == nil {
		t.Fatal("workflows/ kept after a bundle without workflows")
	}
}
