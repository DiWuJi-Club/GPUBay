package appdeploy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mvdan.cc/sh/v3/syntax"

	"spark-cloud/internal/appdeploy"
)

// appsDir 是仓库里随平台发布的初始应用目录（platform admin app import deploy/apps 导入）。
const appsDir = "../../deploy/apps"

// varRefRe 扫描脚本里引用的大写变量（$VAR / ${VAR}）；小写的脚本局部变量（_dir 等）不在检查范围内。
var varRefRe = regexp.MustCompile(`\$\{?([A-Z][A-Z0-9_]*)`)

// allowedVar 平台注入的保留变量与 sh 自带变量：不需要在 spec 里声明。
func allowedVar(name string) bool {
	switch {
	case strings.HasPrefix(name, "SPARK_"), strings.HasPrefix(name, "HF_"):
		return true
	}
	// PIP_CONSTRAINT：镜像 ENV（NVIDIA 镜像用它钉住自家编译的 torch），经 image.env 加载
	return slices.Contains([]string{"MODELSCOPE_CACHE", "PIP_INDEX_URL", "PIP_CONSTRAINT", "PATH", "HOME"}, name)
}

// importAppJSON 与 importer.go 的 appJSON 字段一致；未知字段报错（拼写错误不会被导入静默忽略）。
type importAppJSON struct {
	Name        string   `json:"name"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Icon        string   `json:"icon"`
	Sort        int      `json:"sort"`
	Internal    bool     `json:"internal"`
	Version     string   `json:"version"`
	Images      []string `json:"images"`
	Changelog   string   `json:"changelog"`
}

// 后台登记应用镜像时用的名称（images.name），导入按名称找镜像。
var knownImages = []string{"LLM Serving", "ComfyUI", "PyTorch"}

func TestAppsDir(t *testing.T) {
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		t.Fatalf("read %s: %v", appsDir, err)
	}
	var slugs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			slugs = append(slugs, e.Name())
		}
	}
	for _, want := range []string{"comfyui", "jupyterlab", "open-webui", "vllm", "vllm-node"} {
		if !slices.Contains(slugs, want) {
			t.Errorf("deploy/apps/%s 不存在", want)
		}
	}
	for _, slug := range slugs {
		t.Run(slug, func(t *testing.T) { checkAppDir(t, slug, filepath.Join(appsDir, slug)) })
	}
}

// TestVLLMNodeApp 计划 9 的内部应用 vllm-node：internal、平台填的变量齐全、节点密钥必填且是 secret。
func TestVLLMNodeApp(t *testing.T) {
	dir := filepath.Join(appsDir, "vllm-node")
	var meta importAppJSON
	raw, err := os.ReadFile(filepath.Join(dir, "app.json"))
	if err != nil || json.Unmarshal(raw, &meta) != nil {
		t.Fatalf("app.json: %v", err)
	}
	if !meta.Internal || meta.Category != "llm" || !slices.Equal(meta.Images, []string{"LLM Serving"}) {
		t.Errorf("app.json = %+v", meta)
	}
	for _, slug := range []string{"comfyui", "jupyterlab", "open-webui", "vllm"} {
		var m importAppJSON
		b, _ := os.ReadFile(filepath.Join(appsDir, slug, "app.json"))
		if json.Unmarshal(b, &m) != nil || m.Internal {
			t.Errorf("%s 不应是内部应用", slug)
		}
	}
	var spec appdeploy.Spec
	raw, err = os.ReadFile(filepath.Join(dir, "spec.json"))
	if err != nil || json.Unmarshal(raw, &spec) != nil {
		t.Fatalf("spec.json: %v", err)
	}
	if err := appdeploy.ValidateSpec(&spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].Port != 8000 || spec.Ports[0].HealthPath != "/health" || spec.Ports[0].Path != "/docs" ||
		spec.TimeoutMinutes != 120 || spec.MinDataFreeGiB != 80 {
		t.Errorf("spec = %+v", spec)
	}
	env := map[string]appdeploy.EnvSpec{}
	for _, e := range spec.Env {
		env[e.Name] = e
	}
	for _, name := range []string{"MODEL_ID", "MODEL_SOURCE", "SERVED_MODEL_NAME", "MAX_MODEL_LEN", "API_KEY", "GPU_MEMORY_UTILIZATION",
		"MAX_NUM_SEQS", "QUANTIZATION", "REASONING_PARSER", "TOOL_CALL_PARSER", "TASK", "MODEL_SIZE_GB"} {
		if _, ok := env[name]; !ok {
			t.Errorf("spec 缺少变量 %s", name)
		}
	}
	if k := env["API_KEY"]; k.Type != "secret" || !k.Required || k.Pattern != "^[A-Za-z0-9_-]{32,128}$" {
		t.Errorf("API_KEY = %+v", k)
	}
	if m := env["MODEL_ID"]; !m.Required || m.Type != "string" {
		t.Errorf("MODEL_ID = %+v", m)
	}
	if s := env["SERVED_MODEL_NAME"]; !s.Required || s.Pattern != "^[a-z0-9][a-z0-9.-]{1,63}$" {
		t.Errorf("SERVED_MODEL_NAME = %+v", s)
	}
	// 平台生成的节点密钥（32 字节 base64url 无填充，43 字符）能过校验；缺省值齐全时只需平台填的那几个
	key := strings.Repeat("aB3_-", 8) + "xyz"
	plain, secret, err := appdeploy.ValidateEnv(spec, map[string]string{"MODEL_ID": "Qwen/Qwen3-8B", "SERVED_MODEL_NAME": "qwen3-8b", "API_KEY": key})
	if err != nil || secret["API_KEY"] != key || plain["MODEL_SIZE_GB"] != "20" || plain["GPU_MEMORY_UTILIZATION"] != "0.85" || plain["TOOL_CALL_PARSER"] != "hermes" {
		t.Errorf("ValidateEnv = %v %v %v", plain, secret, err)
	}
	if _, _, err := appdeploy.ValidateEnv(spec, map[string]string{"MODEL_ID": "Qwen/Qwen3-8B", "SERVED_MODEL_NAME": "qwen3-8b"}); err == nil {
		t.Error("API_KEY 必填")
	}
	if _, _, err := appdeploy.ValidateEnv(spec, map[string]string{"MODEL_ID": "Qwen/Qwen3-8B", "SERVED_MODEL_NAME": "qwen3-8b", "API_KEY": "short-key-123"}); err == nil {
		t.Error("API_KEY 少于 32 字符必须拒绝")
	}
}

// runVLLMNodeStart 用假的 vllm 真跑 deploy/apps/vllm-node/app.sh start，返回 vllm 收到的参数与环境变量 VLLM_API_KEY。
func runVLLMNodeStart(t *testing.T, extraEnv ...string) (string, string) {
	t.Helper()
	appDir, bin := t.TempDir(), t.TempDir()
	argsFile := filepath.Join(appDir, "args")
	// 假 vllm 记下参数与它看到的 VLLM_API_KEY（节点密钥经环境变量传入，不在 argv 里）
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + appdeploy.ShellQuote(argsFile+".tmp") + "\nprintf '%s' \"${VLLM_API_KEY:-}\" > " +
		appdeploy.ShellQuote(filepath.Join(appDir, "envkey")) + "\nmv " + appdeploy.ShellQuote(argsFile+".tmp") + " " + appdeploy.ShellQuote(argsFile) + "\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "vllm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join(appsDir, "vllm-node", "app.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script, "start")
	cmd.Dir = appDir
	cmd.Env = append(append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SPARK_APP_DIR="+appDir, "SPARK_MODELS_DIR="+filepath.Join(appDir, "models"),
		"SPARK_PORT=8000", "MODEL_ID=Qwen/Qwen3-8B", "SERVED_MODEL_NAME=qwen3-8b", "MAX_MODEL_LEN=40960", "API_KEY=node-key-0123456789abcdefghijklmnop"), extraEnv...)
	outFile, err := os.Create(filepath.Join(appDir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	cmd.Stdout, cmd.Stderr = outFile, outFile
	err = cmd.Run()
	t.Cleanup(func() {
		for _, f := range []string{"service.pid", "tail.pid"} {
			if b, err := os.ReadFile(filepath.Join(appDir, f)); err == nil {
				exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
			}
		}
	})
	if err != nil {
		b, _ := os.ReadFile(filepath.Join(appDir, "log"))
		t.Fatalf("app.sh start: %v %s", err, b)
	}
	var args []string
	// 假 vllm 在后台启动后才写参数；机器负载高时 sh 启动可能要几秒，放宽到 15 秒
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		b, _ := os.ReadFile(argsFile)
		if args = strings.Fields(string(b)); len(args) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if !strings.HasPrefix(strings.Join(args, " "), "serve "+filepath.Join(appDir, "models", "modelscope", "Qwen/Qwen3-8B")+" ") {
		t.Errorf("vllm args = %v", args)
	}
	envKey, _ := os.ReadFile(filepath.Join(appDir, "envkey"))
	return strings.Join(args, " "), string(envKey)
}

func TestVLLMNodeStartArgs(t *testing.T) {
	const nodeKey = "node-key-0123456789abcdefghijklmnop"
	joined, envKey := runVLLMNodeStart(t)
	if envKey != nodeKey || strings.Contains(joined, nodeKey) || strings.Contains(joined, "--api-key") {
		t.Errorf("node key must reach vllm via VLLM_API_KEY only: env %q args %s", envKey, joined)
	}
	for _, want := range []string{"--host 127.0.0.1", "--port 8000", "--served-model-name qwen3-8b", "--max-model-len 40960",
		"--gpu-memory-utilization 0.85", "--max-num-seqs 64", "--enable-auto-tool-choice --tool-call-parser hermes"} {
		if !strings.Contains(joined, want) {
			t.Errorf("vllm args missing %q: %s", want, joined)
		}
	}
	for _, absent := range []string{"--task", "--quantization", "--reasoning-parser"} {
		if strings.Contains(joined, absent) {
			t.Errorf("vllm args should not have %q by default: %s", absent, joined)
		}
	}
	joined, _ = runVLLMNodeStart(t, "QUANTIZATION=fp8", "REASONING_PARSER=qwen3", "TOOL_CALL_PARSER=none", "GPU_MEMORY_UTILIZATION=0.6", "MAX_NUM_SEQS=16")
	for _, want := range []string{"--quantization fp8", "--reasoning-parser qwen3", "--gpu-memory-utilization 0.6", "--max-num-seqs 16"} {
		if !strings.Contains(joined, want) {
			t.Errorf("vllm args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--tool-call-parser") || strings.Contains(joined, "--enable-auto-tool-choice") || strings.Contains(joined, "--task") {
		t.Errorf("TOOL_CALL_PARSER=none must not enable tool calls; no --task for generate: %s", joined)
	}
	// 向量模型：--task embed，且不带推理内容 / 工具调用解析参数（即使变量设了）
	joined, _ = runVLLMNodeStart(t, "TASK=embed", "REASONING_PARSER=qwen3", "TOOL_CALL_PARSER=hermes")
	if !strings.Contains(joined, "--task embed") || strings.Contains(joined, "--reasoning-parser") || strings.Contains(joined, "--tool-call-parser") ||
		strings.Contains(joined, "--enable-auto-tool-choice") {
		t.Errorf("embed args = %s", joined)
	}
}

func checkAppDir(t *testing.T, slug, dir string) {
	if !appdeploy.ValidSlug(slug) {
		t.Errorf("目录名 %q 不是合法 slug", slug)
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return b
	}
	metaRaw, specRaw, scriptRaw := read("app.json"), read("spec.json"), read("app.sh")

	var meta importAppJSON
	dec := json.NewDecoder(bytes.NewReader(metaRaw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil {
		t.Fatalf("app.json: %v", err)
	}
	if meta.Name == "" || utf8.RuneCountInString(meta.Name) > 32 || utf8.RuneCountInString(meta.Summary) > 80 || utf8.RuneCountInString(meta.Description) > 4000 {
		t.Errorf("app.json: name 1～32 / summary ≤ 80 / description ≤ 4000 字符")
	}
	if !slices.Contains([]string{"llm", "image", "notebook", "tool"}, meta.Category) {
		t.Errorf("app.json: category %q", meta.Category)
	}
	if !slices.Contains([]string{"app", "llm", "chat", "image", "notebook", "tool", "code", "audio"}, meta.Icon) {
		t.Errorf("app.json: icon %q", meta.Icon)
	}
	if !appdeploy.ValidVersion(meta.Version) {
		t.Errorf("app.json: version %q", meta.Version)
	}
	if len(meta.Images) == 0 {
		t.Errorf("app.json: images 为空")
	}
	for _, img := range meta.Images {
		if !slices.Contains(knownImages, img) {
			t.Errorf("app.json: 镜像名 %q 不在约定的 %v 里", img, knownImages)
		}
	}

	var spec appdeploy.Spec
	sdec := json.NewDecoder(bytes.NewReader(specRaw))
	sdec.DisallowUnknownFields()
	if err := sdec.Decode(&spec); err != nil {
		t.Fatalf("spec.json: %v", err)
	}
	if err := appdeploy.ValidateSpec(&spec); err != nil {
		t.Fatalf("spec.json: %v", err)
	}

	script := string(scriptRaw)
	if len(scriptRaw) > appdeploy.MaxScriptBytes {
		t.Errorf("app.sh %d 字节超过 %d", len(scriptRaw), appdeploy.MaxScriptBytes)
	}
	if err := appdeploy.CheckShellSyntax(context.Background(), script); err != nil {
		t.Fatalf("app.sh: %v", err)
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(script), "app.sh")
	if err != nil {
		t.Fatalf("app.sh: %v", err)
	}

	// 子命令：install / start 必须有，start 分支必须写 service.pid
	branches := caseBranches(file)
	for _, sub := range []string{"install", "start"} {
		if _, ok := branches[sub]; !ok {
			t.Errorf("app.sh 没有 %s) 分支", sub)
		}
	}
	if !redirectsTo(branches["start"], "service.pid") {
		t.Errorf("app.sh 的 start 分支没有把 PID 写进 service.pid（> …/service.pid）")
	}

	// 引用的大写变量：spec 声明的、平台保留的、或脚本自己赋值的
	declared := map[string]bool{}
	for _, e := range spec.Env {
		declared[e.Name] = true
	}
	assigned := assignedVars(file)
	for _, m := range varRefRe.FindAllStringSubmatch(script, -1) {
		name := m[1]
		if !declared[name] && !assigned[name] && !allowedVar(name) {
			t.Errorf("app.sh 引用了 $%s：既不在 spec.env 里，也不是保留变量或脚本内赋值的变量", name)
		}
	}
}

// caseBranches 找出脚本里 case 语句各分支（模式 → 分支语句），用来检查 install / start / stop 子命令。
func caseBranches(file *syntax.File) map[string][]*syntax.Stmt {
	out := map[string][]*syntax.Stmt{}
	syntax.Walk(file, func(n syntax.Node) bool {
		if cc, ok := n.(*syntax.CaseClause); ok {
			for _, item := range cc.Items {
				for _, pat := range item.Patterns {
					if lit := pat.Lit(); lit != "" {
						out[lit] = append(out[lit], item.Stmts...)
					}
				}
			}
		}
		return true
	})
	return out
}

// redirectsTo 语句里有没有输出重定向（> / >> / >|）写到文件名以 name 结尾的文件。
func redirectsTo(stmts []*syntax.Stmt, name string) bool {
	found := false
	for _, st := range stmts {
		syntax.Walk(st, func(n syntax.Node) bool {
			if r, ok := n.(*syntax.Redirect); ok && (r.Op == syntax.RdrOut || r.Op == syntax.AppOut || r.Op == syntax.RdrClob) {
				var w strings.Builder
				if err := syntax.NewPrinter().Print(&w, r.Word); err == nil && strings.HasSuffix(strings.Trim(w.String(), `"'`), name) {
					found = true
				}
			}
			return !found
		})
	}
	return found
}

// assignedVars 脚本里赋值过的变量名（NAME=…、export NAME=…、for NAME in …）。
func assignedVars(file *syntax.File) map[string]bool {
	out := map[string]bool{}
	syntax.Walk(file, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.Assign:
			if x.Name != nil && !x.Naked { // export NAME（无 =）不算赋值
				out[x.Name.Value] = true
			}
		case *syntax.WordIter:
			if x.Name != nil {
				out[x.Name.Value] = true
			}
		}
		return true
	})
	return out
}

// TestVLLMStartArgs 用假的 vllm 真跑 deploy/apps/vllm/app.sh start：开了工具调用（验收缺陷 A-2，Open WebUI 默认带
// tool_choice=auto）；每次启动先把旧 vllm.log 改名为 vllm.log.1（O-3：并入部署日志的 tail 只有本次运行的输出）；
// 「打开」指向 /docs（A-4：vLLM 根路径 404）。
func TestVLLMStartArgs(t *testing.T) {
	appDir, bin := t.TempDir(), t.TempDir()
	argsFile := filepath.Join(appDir, "args")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + appdeploy.ShellQuote(argsFile) + "\necho fresh-run\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "vllm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(appDir, "vllm.log"), []byte("previous-run\n"), 0o644)
	script, err := filepath.Abs(filepath.Join(appsDir, "vllm", "app.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script, "start")
	cmd.Dir = appDir
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SPARK_APP_DIR="+appDir, "SPARK_MODELS_DIR="+filepath.Join(appDir, "models"),
		"SPARK_PORT=8000", "MODEL_ID=Qwen/Qwen3-4B", "API_KEY=abcdefgh")
	// start 把 vllm.log 的 tail 并进 stdout（run.sh 的部署日志）且留在后台：输出写文件，不能等管道关闭
	outFile, err := os.Create(filepath.Join(appDir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	cmd.Stdout, cmd.Stderr = outFile, outFile
	err = cmd.Run()
	t.Cleanup(func() {
		for _, f := range []string{"service.pid", "tail.pid"} {
			if b, err := os.ReadFile(filepath.Join(appDir, f)); err == nil {
				exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
			}
		}
	})
	if err != nil {
		b, _ := os.ReadFile(filepath.Join(appDir, "log"))
		t.Fatalf("app.sh start: %v %s", err, b)
	}
	var args []string
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) { // 新建的可执行文件首次启动可能被系统扫描拖慢数秒
		b, _ := os.ReadFile(argsFile)
		if args = strings.Fields(string(b)); len(args) > 0 || time.Now().After(deadline) {
			break
		}
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"serve " + filepath.Join(appDir, "models", "modelscope", "Qwen/Qwen3-4B"), "--host 127.0.0.1", "--port 8000",
		"--served-model-name Qwen/Qwen3-4B", "--enable-auto-tool-choice", "--tool-call-parser hermes", "--api-key abcdefgh"} {
		if !strings.Contains(joined, want) {
			t.Errorf("vllm args missing %q: %s", want, joined)
		}
	}
	if old, _ := os.ReadFile(filepath.Join(appDir, "vllm.log.1")); string(old) != "previous-run\n" {
		t.Errorf("vllm.log.1 = %q", old)
	}
	if cur, _ := os.ReadFile(filepath.Join(appDir, "vllm.log")); strings.Contains(string(cur), "previous-run") {
		t.Errorf("vllm.log still has the previous run: %q", cur)
	}
	var spec appdeploy.Spec
	raw, _ := os.ReadFile(filepath.Join(appsDir, "vllm", "spec.json"))
	if err := json.Unmarshal(raw, &spec); err != nil || spec.Ports[0].Path != "/docs" {
		t.Errorf("vllm ports[0].path = %+v %v", spec.Ports, err)
	}
	df, _ := os.ReadFile("../../deploy/image/apps/llm-serving/Dockerfile")
	if !regexp.MustCompile(`ARG OPEN_WEBUI_VERSION=0\.12\.3\b`).Match(df) || !bytes.Contains(df, []byte(`"open-webui==${OPEN_WEBUI_VERSION}"`)) {
		t.Error("llm-serving Dockerfile must pin Open WebUI to the version verified on the Spark (0.12.3)")
	}
}

// TestOpenWebUIOfflineEmbedding 用假的 modelscope / open-webui 真跑 deploy/apps/open-webui/app.sh：
// install 从 ModelScope 下载向量模型（只要 safetensors 与配置），start 带 OFFLINE_MODE=True 并把 RAG_EMBEDDING_MODEL
// 指向本地目录——Open WebUI 默认启动时经 hf-mirror 拉 all-MiniLM-L6-v2，被重定向到 HF 的 xet CDN 后卡死在健康检查（验收 A 复测）。
// 下载失败不阻塞部署：install 照常结束，start 仍是离线模式（对话可用，只是知识库不可用）。
func TestOpenWebUIOfflineEmbedding(t *testing.T) {
	script, err := filepath.Abs(filepath.Join(appsDir, "open-webui", "app.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		downloadOK bool
	}{{"download ok", true}, {"download fails", false}} {
		t.Run(tc.name, func(t *testing.T) {
			appDir, bin, models := t.TempDir(), t.TempDir(), t.TempDir()
			msArgs, envFile := filepath.Join(appDir, "ms-args"), filepath.Join(appDir, "webui-env")
			ms := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + appdeploy.ShellQuote(msArgs) + "\n"
			if tc.downloadOK {
				ms += "while [ $# -gt 0 ]; do if [ \"$1\" = --local-dir ]; then mkdir -p \"$2\" && : > \"$2/model.safetensors\"; fi; shift; done\n"
			} else {
				ms += "exit 1\n"
			}
			webui := "#!/bin/sh\nenv > " + appdeploy.ShellQuote(envFile) + "\nexec sleep 30\n"
			for name, body := range map[string]string{"modelscope": ms, "open-webui": webui, "spark_status": "#!/bin/sh\nexit 0\n"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			env := append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SPARK_APP_DIR="+appDir, "SPARK_MODELS_DIR="+models,
				"SPARK_PORT=3000", "OPEN_WEBUI_BIN="+filepath.Join(bin, "open-webui"), "OPENAI_API_BASE_URL=http://127.0.0.1:8000/v1", "OPENAI_API_KEY=", "HF_ENDPOINT=")
			install := exec.Command("/bin/sh", script, "install")
			install.Dir, install.Env = appDir, env
			if out, err := install.CombinedOutput(); err != nil {
				t.Fatalf("install: %v %s", err, out)
			}
			args, _ := os.ReadFile(msArgs)
			embedDir := filepath.Join(models, "modelscope", "sentence-transformers/all-MiniLM-L6-v2")
			for _, want := range []string{"download\nsentence-transformers/all-MiniLM-L6-v2\n", "--local-dir\n" + embedDir + "\n", "--exclude\nonnx/*\nopenvino/*\n*.bin\n"} {
				if !strings.Contains(string(args), want) {
					t.Errorf("modelscope args missing %q:\n%s", want, args)
				}
			}

			start := exec.Command("/bin/sh", script, "start")
			start.Dir, start.Env = appDir, env
			t.Cleanup(func() {
				if b, err := os.ReadFile(filepath.Join(appDir, "service.pid")); err == nil {
					exec.Command("kill", strings.TrimSpace(string(b))).Run() //nolint:errcheck
				}
			})
			if out, err := start.CombinedOutput(); err != nil {
				t.Fatalf("start: %v %s", err, out)
			}
			var got string
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) { // 新建的可执行文件首次启动可能被系统扫描拖慢数秒
				b, _ := os.ReadFile(envFile)
				if got = string(b); strings.Contains(got, "OFFLINE_MODE=") || time.Now().After(deadline) {
					break
				}
			}
			if !strings.Contains(got, "\nOFFLINE_MODE=True\n") && !strings.HasPrefix(got, "OFFLINE_MODE=True\n") {
				t.Errorf("open-webui started without OFFLINE_MODE=True")
			}
			hasEmbed := strings.Contains(got, "RAG_EMBEDDING_MODEL="+embedDir+"\n")
			if hasEmbed != tc.downloadOK {
				t.Errorf("RAG_EMBEDDING_MODEL=<local dir> set = %v, want %v", hasEmbed, tc.downloadOK)
			}
			if strings.Contains(got, "\nHF_ENDPOINT=") {
				t.Errorf("empty HF_ENDPOINT should be unset")
			}
		})
	}
}
