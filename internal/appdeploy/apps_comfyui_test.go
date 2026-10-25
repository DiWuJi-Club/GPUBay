//go:build !windows

package appdeploy_test

// deploy/apps/comfyui/app.sh（1.1.0，计划 11 Task 7）用假的 modelscope / hf / git / python3（含 python3 -m pip）真跑 install / start：
// 临时目录充当容器的 /root/data，部署包里的 workflows/ 由 comfywf.BundleFiles 生成（与平台下发的形状一致）。
//
// python3：app.sh 用镜像自带的 python3 解析 manifest.json（`python3 -I - <清单> <目录表>`），start 用它跑 `python3 main.py …`。
// 测试在 PATH 最前放一个假 python3：第一个参数是 main.py 时记录参数并 sleep（充当 ComfyUI 服务进程），其余调用 exec 本机真
// python3（manifest 解析、run.sh 在没有 setsid 的 macOS 上用的 os.setsid 包装）。脚本本身不为测试分支。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/appdeploy/runtime"
	"spark-cloud/internal/comfywf"
)

const (
	usduCommit = "0123456789abcdef0123456789abcdef01234567"
	usduGit    = "https://github.com/ssitu/ComfyUI_UltimateSDUpscale.git"
	testMirror = "https://gh-proxy.com/"
)

// 假命令：调用逐行记进 $FAKE_CALLS（参数用空格连接）。
const fakeDownloader = `#!/bin/sh
# modelscope / hf 的替身：<cmd> download <repo> <file> --local-dir <dir> → 写 <dir>/<file>，内容 "fake:<repo>/<file>"。
# 只认 --local-dir 这一种写法（真机核实过的拼写），其它参数报错；FAKE_DL_FAIL=<命令名> 时该命令下载失败
printf '%s %s\n' "$(basename "$0")" "$*" >> "$FAKE_CALLS"
[ "$1" = download ] || exit 64
[ "${FAKE_DL_FAIL:-}" = "$(basename "$0")" ] && { echo "download failed" >&2; exit 1; }
repo=$2; file=$3; dir=
shift 3
while [ $# -gt 0 ]; do
  case "$1" in
    --local-dir) dir=$2; shift ;;
    *) echo "unknown argument $1" >&2; exit 66 ;;
  esac
  shift
done
[ -n "$dir" ] || exit 65
mkdir -p "$(dirname "$dir/$file")"
printf 'fake:%s/%s\n' "$repo" "$file" > "$dir/$file"
`

const fakeGit = `#!/bin/sh
printf 'git %s\n' "$*" >> "$FAKE_CALLS"
dir=.
cfg=
while :; do
  case "$1" in
    -C) dir=$2; shift 2 ;;
    -c) cfg="$cfg $2"; shift 2 ;;
    *) break ;;
  esac
done
case "$1" in
  clone)
    shift
    while [ $# -gt 2 ]; do shift; done
    url=$1; dst=$2
    if [ "${FAKE_GIT_FAIL_MIRROR:-}" = clone ]; then case "$url" in *gh-proxy*) echo "fatal: mirror down" >&2; exit 128 ;; esac; fi
    mkdir -p "$dst/.git"
    name=$(basename "$url" .git)
    if [ -d "${FAKE_GIT_SRC:-/nonexistent}/$name" ]; then cp -R "$FAKE_GIT_SRC/$name/." "$dst/"; fi
    ;;
  checkout)
    for a in "$@"; do
      if [ -n "${FAKE_GIT_BAD_COMMIT:-}" ] && [ "$a" = "$FAKE_GIT_BAD_COMMIT" ]; then echo "fatal: reference is not a tree: $a" >&2; exit 128; fi
    done
    ;;
  submodule)
    if [ "${FAKE_GIT_FAIL_MIRROR:-}" = submodule ]; then case "$cfg" in *gh-proxy*) echo "fatal: mirror down" >&2; exit 1 ;; esac; fi
    mkdir -p "$dir/repositories/ultimate_sd_upscale"
    ;;
  *) exit 64 ;;
esac
`

// fakePyPip 假 python3 里 `python3 -m pip …` 的部分：list 输出一份已装包（约束文件应只挑出 torch / numpy / nvidia-* 等），
// install 记录参数，FAKE_PIP_FAIL=1 时失败。
const fakePyPip = `if [ "${1:-}" = -m ] && [ "${2:-}" = pip ]; then
  shift 2
  printf 'pip %s\n' "$*" >> "$FAKE_CALLS"
  case "${1:-}" in
    list) printf 'foo==1.0\ntorch==2.9.0a0+50eac811a6.nv25.9\nnumpy==1.26.4\nnvidia-cudnn-cu12==9.1.0\nTransformers==4.56.0\ntorchvisionx==1\n' ;;
    install) [ "${FAKE_PIP_FAIL:-}" = 1 ] && exit 1 ;;
  esac
  exit 0
fi
`

type comfyBox struct {
	t                     *testing.T
	root, app, bin, calls string // root 充当 /root/data；app = <root>/.spark-apps/comfyui
	comfy                 string // 充当 /opt/ComfyUI
	env                   map[string]string
}

// comfyFakeBin 写假命令（含 spark_status）到一个目录，供同一个父测试的全部子用例共用：macOS 第一次执行新建的可执行文件要做
// 安全检查（每个文件数秒），每个用例各建一套会让测试慢上几分钟。spark_status 按 $SPARK_APP_DIR 找 run.sh，与 run.sh 生成的一致。
func comfyFakeBin(t *testing.T) string {
	t.Helper()
	realPy, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	bin := t.TempDir()
	fakePy := "#!/bin/sh\n" + fakePyPip + "if [ \"${1:-}\" = main.py ]; then\n  pwd > \"$FAKE_PY_ARGS.cwd\"\n  printf '%s\\n' \"$@\" > \"$FAKE_PY_ARGS\"\n  echo fresh-run\n  exec sleep 30\nfi\nexec " +
		appdeploy.ShellQuote(realPy) + " \"$@\"\n"
	sparkStatus := "#!/bin/sh\nDIR=$(dirname \"$0\"); [ -n \"${SPARK_APP_DIR:-}\" ] && DIR=$SPARK_APP_DIR\nexec sh \"$DIR/run.sh\" _status \"${1:-}\" \"${2:-}\" \"${3:-}\"\n"
	for name, body := range map[string]string{"modelscope": fakeDownloader, "hf": fakeDownloader, "git": fakeGit,
		"python3": fakePy, "spark_status": sparkStatus} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 预热（并行触发上面说的首次执行检查）：带一个不会被当成正常调用的参数，结果不管
	var wg sync.WaitGroup
	for _, name := range []string{"modelscope", "hf", "git", "python3", "spark_status"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exec.Command(filepath.Join(bin, name), "--spark-warmup").Run() //nolint:errcheck
		}()
	}
	wg.Wait()
	return bin
}

func newComfyBox(t *testing.T, bin string) *comfyBox {
	t.Helper()
	root := t.TempDir()
	b := &comfyBox{t: t, root: root, app: appdeploy.AppDir(root, "comfyui"), bin: bin,
		calls: filepath.Join(root, "calls.log"), comfy: filepath.Join(root, "opt-comfyui")}
	for _, d := range []string{b.app, filepath.Join(b.comfy, "input"), filepath.Join(root, "gitsrc", "ComfyUI_UltimateSDUpscale")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile(filepath.Join(appsDir, "comfyui", "app.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		filepath.Join(b.app, "app.sh"):                                                 string(script),
		filepath.Join(b.app, "run.sh"):                                                 string(runtime.RunSH),
		filepath.Join(b.comfy, "input", "example.png"):                                 "png",
		filepath.Join(root, "image-constraint.txt"):                                    "torch==2.4.0\n",
		filepath.Join(root, "gitsrc", "ComfyUI_UltimateSDUpscale", "requirements.txt"): "numpy\n",
		filepath.Join(root, "gitsrc", "ComfyUI_UltimateSDUpscale", ".gitmodules"):      "[submodule \"repositories/ultimate_sd_upscale\"]\n",
		filepath.Join(root, "gitsrc", "ComfyUI_UltimateSDUpscale", "__init__.py"):      "",
	} {
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b.env = map[string]string{
		"PATH":                   b.bin + ":" + os.Getenv("PATH"),
		"SPARK_APP_DIR":          b.app,
		"SPARK_DATA_DIR":         root,
		"SPARK_MODELS_DIR":       filepath.Join(root, "models"),
		"SPARK_PORT":             "8188",
		"SPARK_GIT_MIRROR":       testMirror,
		"SPARK_TEST_DF_FREE_GIB": "500", // 开发机的真实剩余空间不参与判断
		"SPARK_TEST_COMFY_DIR":   b.comfy,
		"MODEL_ID":               "none",
		"MODEL_SOURCE":           "modelscope",
		"HF_ENDPOINT":            "https://hf-mirror.com",
		"FAKE_CALLS":             b.calls,
		"FAKE_GIT_SRC":           filepath.Join(root, "gitsrc"),
		"FAKE_PY_ARGS":           filepath.Join(root, "py-args"),
		// 镜像 ENV 的 pip 约束（NVIDIA 镜像有）；第二个路径不存在，应被跳过
		"PIP_CONSTRAINT": filepath.Join(root, "image-constraint.txt") + " " + filepath.Join(root, "missing.txt"),
	}
	t.Cleanup(func() {
		if raw, err := os.ReadFile(filepath.Join(b.app, "service.pid")); err == nil {
			exec.Command("kill", strings.TrimSpace(string(raw))).Run() //nolint:errcheck
		}
	})
	return b
}

// bundle 把 BundleFiles 生成的 workflows/ 写进应用目录（先清空，同平台 UploadCommand 的 rm -rf workflows）。
func (b *comfyBox) bundle(ws ...comfywf.BundleWorkflow) {
	b.t.Helper()
	os.RemoveAll(filepath.Join(b.app, "workflows"))
	files, err := comfywf.BundleFiles(ws)
	if err != nil {
		b.t.Fatal(err)
	}
	for _, f := range files {
		p := filepath.Join(b.app, f.Name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, f.Body, 0o644); err != nil {
			b.t.Fatal(err)
		}
	}
}

func (b *comfyBox) environ() []string {
	out := os.Environ()
	for k, v := range b.env {
		out = append(out, k+"="+v)
	}
	return out
}

// run 执行 `sh app.sh <sub>`（工作目录 = 应用目录，同 run.sh）；输出写文件（progress_loop 的 sleep 子进程不会卡住管道）。
func (b *comfyBox) run(sub string) (string, int) {
	b.t.Helper()
	outPath := filepath.Join(b.root, "out-"+sub+".txt")
	f, err := os.Create(outPath)
	if err != nil {
		b.t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", filepath.Join(b.app, "app.sh"), sub)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = b.app, b.environ(), f, f
	err = cmd.Run()
	f.Close()
	out, _ := os.ReadFile(outPath)
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		b.t.Fatalf("app.sh %s: %v", sub, err)
	}
	return string(out), code
}

func (b *comfyBox) callLines() []string {
	raw, _ := os.ReadFile(b.calls)
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func (b *comfyBox) countCalls(prefix string) int {
	n := 0
	for _, l := range b.callLines() {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func (b *comfyBox) resetCalls() { os.Remove(b.calls) }

func (b *comfyBox) status() map[string]any {
	b.t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.app, "status.json"))
	if err != nil {
		b.t.Fatalf("status.json: %v", err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		b.t.Fatalf("status.json %q: %v", raw, err)
	}
	return st
}

func (b *comfyBox) cdata(parts ...string) string {
	return filepath.Join(append([]string{b.root, "comfyui"}, parts...)...)
}

func sdxlModel() comfywf.ModelSpec {
	return comfywf.ModelSpec{Name: "sd_xl_base_1.0.safetensors", Dir: "checkpoints", SizeGB: 6.94,
		ModelScope: &comfywf.RepoFile{Repo: "AI-ModelScope/stable-diffusion-xl-base-1.0", File: "sd_xl_base_1.0.safetensors"},
		HF:         &comfywf.RepoFile{Repo: "stabilityai/stable-diffusion-xl-base-1.0", File: "sd_xl_base_1.0.safetensors"}}
}

func upscaleModelHFOnly() comfywf.ModelSpec {
	return comfywf.ModelSpec{Name: "4x-UltraSharp.pth", Dir: "upscale_models", SizeGB: 0.07,
		HF: &comfywf.RepoFile{Repo: "Kim2091/UltraSharp", File: "4x-UltraSharp.pth"}}
}

func t5ModelMSOnly() comfywf.ModelSpec {
	return comfywf.ModelSpec{Name: "t5xxl_fp8_e4m3fn.safetensors", Dir: "text_encoders", SizeGB: 4.89,
		ModelScope: &comfywf.RepoFile{Repo: "AI-ModelScope/flux_text_encoders", File: "split/t5xxl_fp8_e4m3fn.safetensors"}}
}

func wfTxt2Img() comfywf.BundleWorkflow {
	return comfywf.BundleWorkflow{Slug: "sdxl-txt2img", Version: "1.0.0", Graph: json.RawMessage(`{"nodes":[],"links":[],"id":"txt2img"}`),
		Manifest: comfywf.Manifest{Models: []comfywf.ModelSpec{sdxlModel()}}}
}

func wfUpscale() comfywf.BundleWorkflow {
	return comfywf.BundleWorkflow{Slug: "sdxl-upscale", Version: "1.0.0", Graph: json.RawMessage(`{"nodes":[],"links":[],"id":"upscale"}`),
		Manifest: comfywf.Manifest{Models: []comfywf.ModelSpec{sdxlModel(), upscaleModelHFOnly()},
			Nodes: []comfywf.NodeSpec{{Name: "ComfyUI_UltimateSDUpscale", Git: usduGit, Commit: usduCommit, NodeTypes: []string{"UltimateSDUpscale"}}}}}
}

func readLink(t *testing.T, p string) string {
	t.Helper()
	got, err := os.Readlink(p)
	if err != nil {
		t.Fatalf("readlink %s: %v", p, err)
	}
	return got
}

// TestComfyApp 子用例共用一套假命令（见 comfyFakeBin）。
func TestComfyApp(t *testing.T) {
	bin := comfyFakeBin(t)
	for _, c := range []struct {
		name string
		fn   func(*testing.T, string)
	}{
		{"InstallWorkflows", comfyInstallWorkflows},
		{"InstallSourceFallback", comfyInstallSourceFallback},
		{"InstallGitMirrorFallback", comfyInstallGitMirrorFallback},
		{"InstallNoGitMirror", comfyInstallNoGitMirror},
		{"InstallNodeFailures", comfyInstallNodeFailures},
		{"InstallNodeCommitChange", comfyInstallNodeCommitChange},
		{"InstallDiskPrecheck", comfyInstallDiskPrecheck},
		{"InstallSHA256", comfyInstallSHA256},
		{"InstallNoManifest", comfyInstallNoManifest},
		{"InstallRejectsBadManifest", comfyInstallRejectsBadManifest},
		{"InstallPresetModel", comfyInstallPresetModel},
		{"Start", comfyStart},
		{"InstallLeftoverTmp", comfyInstallLeftoverTmp},
		{"InstallDanglingLink", comfyInstallDanglingLink},
		{"InstallSourceRetry", comfyInstallSourceRetry},
		{"InstallProgressMonotonic", comfyInstallProgressMonotonic},
		{"InstallReusesLegacyModels", comfyInstallReusesLegacyModels},
	} {
		t.Run(c.name, func(t *testing.T) { c.fn(t, bin) })
	}
}

// 两个工作流共用 SDXL：只下一次；只有 hf 来源的放大模型用 hf；节点经镜像 clone、checkout、子模块、pip；工作流文件就位，
// 多余的 spark-*.json 删掉、用户自己的文件保留；再跑一次全部跳过。
func comfyInstallWorkflows(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	b.bundle(wfUpscale(), wfTxt2Img())
	wfdir := b.cdata("user", "default", "workflows")
	os.MkdirAll(wfdir, 0o755)
	os.WriteFile(filepath.Join(wfdir, "spark-old.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(wfdir, "my.json"), []byte("{}"), 0o644)

	out, code := b.run("install")
	if code != 0 {
		t.Fatalf("install = %d:\n%s\ncalls:\n%s", code, out, strings.Join(b.callLines(), "\n"))
	}
	models := filepath.Join(b.root, "models")
	if n := b.countCalls("modelscope download AI-ModelScope/stable-diffusion-xl-base-1.0 sd_xl_base_1.0.safetensors --local-dir " +
		filepath.Join(models, "modelscope", "AI-ModelScope/stable-diffusion-xl-base-1.0")); n != 1 {
		t.Errorf("sdxl downloaded %d times; calls:\n%s", n, strings.Join(b.callLines(), "\n"))
	}
	if n := b.countCalls("hf download Kim2091/UltraSharp 4x-UltraSharp.pth --local-dir " + filepath.Join(models, "hf", "Kim2091/UltraSharp")); n != 1 {
		t.Errorf("upscale model via hf: %d; calls:\n%s", n, strings.Join(b.callLines(), "\n"))
	}
	if n := b.countCalls("modelscope ") + b.countCalls("hf "); n != 2 {
		t.Errorf("downloads = %d, want 2", n)
	}
	if got := readLink(t, b.cdata("models", "checkpoints", "sd_xl_base_1.0.safetensors")); got !=
		filepath.Join(models, "modelscope", "AI-ModelScope/stable-diffusion-xl-base-1.0", "sd_xl_base_1.0.safetensors") {
		t.Errorf("sdxl link -> %s", got)
	}
	if got := readLink(t, b.cdata("models", "upscale_models", "4x-UltraSharp.pth")); got != filepath.Join(models, "hf", "Kim2091/UltraSharp", "4x-UltraSharp.pth") {
		t.Errorf("upscale link -> %s", got)
	}

	// 节点：镜像 clone 一次（临时目录在 custom_nodes 外），checkout 指定 commit，子模块经镜像（insteadOf），
	// requirements.txt 用 python3 -m pip 装且带约束文件
	node := b.cdata("custom_nodes", "ComfyUI_UltimateSDUpscale")
	tmp := b.cdata(".tmp-nodes", "ComfyUI_UltimateSDUpscale")
	if n := b.countCalls("git clone --quiet --no-checkout " + testMirror + usduGit + " " + tmp); n != 1 || b.countCalls("git clone") != 1 {
		t.Errorf("clone calls:\n%s", strings.Join(b.callLines(), "\n"))
	}
	if b.countCalls("git -C "+tmp+" checkout --quiet "+usduCommit) != 1 {
		t.Errorf("checkout calls:\n%s", strings.Join(b.callLines(), "\n"))
	}
	if b.countCalls("git -C "+tmp+" -c url."+testMirror+"https://github.com/.insteadOf=https://github.com/ submodule update --init --recursive") != 1 ||
		b.countCalls("git -C "+tmp+" submodule") != 0 {
		t.Errorf("submodule calls:\n%s", strings.Join(b.callLines(), "\n"))
	}
	constraints := filepath.Join(b.app, ".pip-constraints.txt")
	if b.countCalls("pip install -c "+constraints+" -r "+tmp+"/requirements.txt") != 1 || b.countCalls("pip list --format=freeze") != 1 {
		t.Errorf("pip calls:\n%s", strings.Join(b.callLines(), "\n"))
	}
	// 约束 = 镜像的 PIP_CONSTRAINT 文件 + 已装的 torch / numpy / transformers / nvidia-*（大小写不敏感），别的包不钉
	if c, _ := os.ReadFile(constraints); string(c) != "torch==2.4.0\n\ntorch==2.9.0a0+50eac811a6.nv25.9\nnumpy==1.26.4\nnvidia-cudnn-cu12==9.1.0\nTransformers==4.56.0\n" {
		t.Errorf(".pip-constraints.txt = %q", c)
	}
	if _, err := os.Stat(b.cdata(".tmp-nodes")); !os.IsNotExist(err) {
		t.Errorf(".tmp-nodes left behind: %v", err)
	}
	if c, _ := os.ReadFile(filepath.Join(node, ".spark-commit")); string(c) != usduCommit+"\n" {
		t.Errorf(".spark-commit = %q", c)
	}
	if _, err := os.Stat(filepath.Join(node, "repositories", "ultimate_sd_upscale")); err != nil {
		t.Errorf("submodule dir missing: %v", err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("tmp clone left behind: %v", err)
	}

	// 工作流文件
	for slug, id := range map[string]string{"sdxl-txt2img": "txt2img", "sdxl-upscale": "upscale"} {
		got, err := os.ReadFile(filepath.Join(wfdir, "spark-"+slug+".json"))
		if err != nil || !strings.Contains(string(got), `"id":"`+id+`"`) {
			t.Errorf("spark-%s.json = %q %v", slug, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(wfdir, "spark-old.json")); !os.IsNotExist(err) {
		t.Error("spark-old.json not removed")
	}
	if _, err := os.Stat(filepath.Join(wfdir, "my.json")); err != nil {
		t.Error("user's my.json removed")
	}
	if st := b.status(); st["message"] != "工作流就绪：2 个" || st["progress"] != float64(79) || st["phase"] != "install" {
		t.Errorf("status = %v", st)
	}
	for _, d := range append([]string{"custom_nodes", "input", "user"}, comfywf.ModelDirs...) {
		dir := d
		if slices.Contains(comfywf.ModelDirs, d) {
			dir = filepath.Join("models", d)
		}
		if fi, err := os.Stat(b.cdata(dir)); err != nil || !fi.IsDir() {
			t.Errorf("dir %s missing", dir)
		}
	}
	if fi, err := os.Stat(filepath.Join(b.root, "comfyui-output")); err != nil || !fi.IsDir() {
		t.Error("comfyui-output missing")
	}

	// 幂等：再跑一次不下载、不 clone、不 pip
	b.resetCalls()
	out, code = b.run("install")
	if code != 0 {
		t.Fatalf("second install = %d:\n%s", code, out)
	}
	if raw, _ := os.ReadFile(b.calls); len(raw) != 0 {
		t.Errorf("second install made calls:\n%s", raw)
	}
	if !strings.Contains(out, "已存在，跳过") {
		t.Errorf("second install output:\n%s", out)
	}

	// 移除一个工作流：它的文件删掉，模型与节点保留（D8）
	b.bundle(wfTxt2Img())
	if out, code = b.run("install"); code != 0 {
		t.Fatalf("third install = %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(wfdir, "spark-sdxl-upscale.json")); !os.IsNotExist(err) {
		t.Error("spark-sdxl-upscale.json not removed")
	}
	if _, err := os.Stat(filepath.Join(node, ".spark-commit")); err != nil {
		t.Error("node removed with its workflow (D8 keeps it)")
	}
	if _, err := os.Lstat(b.cdata("models", "upscale_models", "4x-UltraSharp.pth")); err != nil {
		t.Error("model removed with its workflow (D8 keeps it)")
	}
	if st := b.status(); st["message"] != "工作流就绪：1 个" {
		t.Errorf("status = %v", st)
	}
}

// MODEL_SOURCE=hf-mirror：两个来源都有的走 hf，只有 ModelScope 来源的走 modelscope（仓库内子路径保留）。
func comfyInstallSourceFallback(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	b.env["MODEL_SOURCE"] = "hf-mirror"
	b.bundle(comfywf.BundleWorkflow{Slug: "flux-dev", Version: "1.0.0", Graph: json.RawMessage(`{"nodes":[],"links":[]}`),
		Manifest: comfywf.Manifest{Models: []comfywf.ModelSpec{sdxlModel(), t5ModelMSOnly()}}})
	if out, code := b.run("install"); code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	models := filepath.Join(b.root, "models")
	if b.countCalls("hf download stabilityai/stable-diffusion-xl-base-1.0 sd_xl_base_1.0.safetensors ") != 1 ||
		b.countCalls("modelscope download AI-ModelScope/flux_text_encoders split/t5xxl_fp8_e4m3fn.safetensors ") != 1 {
		t.Errorf("calls:\n%s", strings.Join(b.callLines(), "\n"))
	}
	if got := readLink(t, b.cdata("models", "text_encoders", "t5xxl_fp8_e4m3fn.safetensors")); got !=
		filepath.Join(models, "modelscope", "AI-ModelScope/flux_text_encoders", "split", "t5xxl_fp8_e4m3fn.safetensors") {
		t.Errorf("t5 link -> %s", got)
	}
	// 目标已是普通文件（用户手工放的）也算已存在
	b.resetCalls()
	os.Remove(b.cdata("models", "checkpoints", "sd_xl_base_1.0.safetensors"))
	os.WriteFile(b.cdata("models", "checkpoints", "sd_xl_base_1.0.safetensors"), []byte("mine"), 0o644)
	if out, code := b.run("install"); code != 0 || b.countCalls("hf ") != 0 {
		t.Fatalf("install = %d:\n%s\ncalls: %v", code, out, b.callLines())
	}
}

// 经镜像的任一步失败 → 清掉临时目录，整套（clone + checkout + 子模块）直连重来；成功安装。
func comfyInstallGitMirrorFallback(t *testing.T, bin string) {
	for _, tc := range []struct {
		failAt     string
		wantSubs   int // 子模块调用次数（镜像 clone 失败时镜像那一轮没走到子模块）
		wantChecks int
	}{{"clone", 1, 1}, {"submodule", 2, 2}} {
		t.Run(tc.failAt, func(t *testing.T) {
			b := newComfyBox(t, bin)
			b.env["FAKE_GIT_FAIL_MIRROR"] = tc.failAt
			b.bundle(wfUpscale())
			if out, code := b.run("install"); code != 0 {
				t.Fatalf("install = %d:\n%s", code, out)
			}
			tmp := b.cdata(".tmp-nodes", "ComfyUI_UltimateSDUpscale")
			var clones, subs []string
			for _, l := range b.callLines() {
				if strings.HasPrefix(l, "git clone") {
					clones = append(clones, l)
				}
				if strings.Contains(l, " submodule ") {
					subs = append(subs, l)
				}
			}
			if len(clones) != 2 || clones[0] != "git clone --quiet --no-checkout "+testMirror+usduGit+" "+tmp ||
				clones[1] != "git clone --quiet --no-checkout "+usduGit+" "+tmp {
				t.Errorf("clones = %q", clones)
			}
			if len(subs) != tc.wantSubs || subs[len(subs)-1] != "git -C "+tmp+" submodule update --init --recursive" ||
				(tc.wantSubs == 2 && !strings.Contains(subs[0], "insteadOf")) {
				t.Errorf("submodule calls = %q", subs)
			}
			if n := b.countCalls("git -C " + tmp + " checkout"); n != tc.wantChecks {
				t.Errorf("checkouts = %d", n)
			}
			if _, err := os.Stat(b.cdata("custom_nodes", "ComfyUI_UltimateSDUpscale", ".spark-commit")); err != nil {
				t.Error("node not installed")
			}
		})
	}
}

// 没配镜像（SPARK_GIT_MIRROR 为空）：直连 clone 一次，子模块不带 insteadOf。
func comfyInstallNoGitMirror(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	b.env["SPARK_GIT_MIRROR"] = ""
	b.bundle(wfUpscale())
	if out, code := b.run("install"); code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	if b.countCalls("git clone") != 1 || b.countCalls("git clone --quiet --no-checkout "+usduGit+" ") != 1 ||
		b.countCalls("git -C") != 2 || strings.Contains(strings.Join(b.callLines(), "\n"), "insteadOf") {
		t.Errorf("calls:\n%s", strings.Join(b.callLines(), "\n"))
	}
}

// checkout 失败 / pip 失败 → 非零退出，节点不就位、不写 .spark-commit、临时目录清掉。
func comfyInstallNodeFailures(t *testing.T, bin string) {
	for _, tc := range []struct{ name, key, val, wantCall string }{
		{"checkout", "FAKE_GIT_BAD_COMMIT", usduCommit, "git -C"},
		{"pip", "FAKE_PIP_FAIL", "1", "pip install"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newComfyBox(t, bin)
			b.env[tc.key] = tc.val
			b.bundle(wfUpscale())
			out, code := b.run("install")
			if code == 0 {
				t.Fatalf("install succeeded:\n%s", out)
			}
			if b.countCalls(tc.wantCall) == 0 {
				t.Errorf("calls:\n%s", strings.Join(b.callLines(), "\n"))
			}
			if _, err := os.Stat(b.cdata("custom_nodes", "ComfyUI_UltimateSDUpscale")); !os.IsNotExist(err) {
				t.Errorf("node dir present after failure: %v", err)
			}
			if _, err := os.Stat(b.cdata(".tmp-nodes", "ComfyUI_UltimateSDUpscale")); !os.IsNotExist(err) {
				t.Errorf("tmp clone left behind: %v", err)
			}
			if !strings.Contains(out, "ComfyUI_UltimateSDUpscale") {
				t.Errorf("output does not name the node:\n%s", out)
			}
		})
	}
}

// 已装节点 commit 变了 → 重新 clone 并替换；commit 相同 → 跳过。
func comfyInstallNodeCommitChange(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	node := b.cdata("custom_nodes", "ComfyUI_UltimateSDUpscale")
	os.MkdirAll(node, 0o755)
	os.WriteFile(filepath.Join(node, ".spark-commit"), []byte(strings.Repeat("a", 40)+"\n"), 0o644)
	os.WriteFile(filepath.Join(node, "stale.py"), nil, 0o644)
	b.bundle(wfUpscale())
	if out, code := b.run("install"); code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	if b.countCalls("git clone") != 1 {
		t.Errorf("calls:\n%s", strings.Join(b.callLines(), "\n"))
	}
	if _, err := os.Stat(filepath.Join(node, "stale.py")); !os.IsNotExist(err) {
		t.Error("old node dir not replaced")
	}
	if c, _ := os.ReadFile(filepath.Join(node, ".spark-commit")); string(c) != usduCommit+"\n" {
		t.Errorf(".spark-commit = %q", c)
	}
}

// 磁盘预检（D7）：经 run.sh deploy 真跑，install 非零退出，部署 failed、消息以 disk: 开头（run.sh 沿用），退出码 2，不建软链。
func comfyInstallDiskPrecheck(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	b.env["SPARK_TEST_DF_FREE_GIB"] = "1"
	b.env["SPARK_SPEC_HASH"] = "hash-1"
	b.bundle(wfTxt2Img())
	cmd := exec.Command("/bin/sh", "run.sh", "deploy")
	cmd.Dir, cmd.Env = b.app, b.environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy: %v %s", err, out)
	}
	var st map[string]any
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		st = b.status()
		if st["state"] == "failed" || time.Now().After(deadline) {
			break
		}
	}
	msg, _ := st["message"].(string)
	if st["state"] != "failed" || !strings.HasPrefix(msg, "disk: ") || !strings.Contains(msg, "sd_xl_base_1.0.safetensors") ||
		st["exit_code"] != float64(2) || st["phase"] != "download" {
		log, _ := os.ReadFile(filepath.Join(b.app, "log"))
		t.Fatalf("status = %v\nlog:\n%s", st, log)
	}
	if b.countCalls("modelscope") != 0 {
		t.Error("downloaded despite disk precheck failure")
	}
	if _, err := os.Lstat(b.cdata("models", "checkpoints", "sd_xl_base_1.0.safetensors")); !os.IsNotExist(err) {
		t.Error("link created")
	}
}

// sha256：不符 → 删掉下载的文件、非零退出；相符 → 软链就位。
func comfyInstallSHA256(t *testing.T, bin string) {
	m := sdxlModel()
	sum := sha256.Sum256([]byte("fake:" + m.ModelScope.Repo + "/" + m.ModelScope.File + "\n"))
	dl := func(b *comfyBox) string {
		return filepath.Join(b.root, "models", "modelscope", m.ModelScope.Repo, m.ModelScope.File)
	}
	wf := func(sha string) comfywf.BundleWorkflow {
		w := wfTxt2Img()
		mm := sdxlModel()
		mm.SHA256 = sha
		w.Manifest.Models = []comfywf.ModelSpec{mm}
		return w
	}

	b := newComfyBox(t, bin)
	b.bundle(wf(strings.Repeat("0", 64)))
	out, code := b.run("install")
	if code == 0 || !strings.Contains(out, "sha256") {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	if _, err := os.Stat(dl(b)); !os.IsNotExist(err) {
		t.Error("mismatching file not deleted")
	}
	if _, err := os.Lstat(b.cdata("models", "checkpoints", m.Name)); !os.IsNotExist(err) {
		t.Error("link created for mismatching file")
	}

	b = newComfyBox(t, bin)
	b.bundle(wf(hex.EncodeToString(sum[:])))
	if out, code := b.run("install"); code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	if got := readLink(t, b.cdata("models", "checkpoints", m.Name)); got != dl(b) {
		t.Errorf("link -> %s", got)
	}
}

// 包里没有 manifest（集合清空 / 旧平台）：平台放的 spark-*.json 全删，用户文件保留；MODEL_ID=none 不下任何模型。
func comfyInstallNoManifest(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	wfdir := b.cdata("user", "default", "workflows")
	os.MkdirAll(wfdir, 0o755)
	os.WriteFile(filepath.Join(wfdir, "spark-x.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(wfdir, "my.json"), []byte("{}"), 0o644)
	if out, code := b.run("install"); code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	if raw, _ := os.ReadFile(b.calls); len(raw) != 0 {
		t.Errorf("calls:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(wfdir, "spark-x.json")); !os.IsNotExist(err) {
		t.Error("spark-x.json not removed")
	}
	if _, err := os.Stat(filepath.Join(wfdir, "my.json")); err != nil {
		t.Error("my.json removed")
	}
	// 空集合的 manifest（{"workflows":[]}）同样
	os.WriteFile(filepath.Join(wfdir, "spark-y.json"), []byte("{}"), 0o644)
	b.bundle()
	if out, code := b.run("install"); code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(wfdir, "spark-y.json")); !os.IsNotExist(err) {
		t.Error("spark-y.json not removed")
	}
	if st := b.status(); st["message"] != "工作流就绪：0 个" {
		t.Errorf("status = %v", st)
	}
}

// 清单里的字段在 sh 里按空白切分、拼进路径：解析器再校验一遍，不合规的整份拒绝（平台已校验，这里是兜底）。
func comfyInstallRejectsBadManifest(t *testing.T, bin string) {
	for name, body := range map[string]string{
		"model path": `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[{"name":"../x","dir":"checkpoints","size_gb":1,"sha256":"","hf":{"repo":"a/b","file":"x"}}],"nodes":[]}]}`,
		"model dir":  `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[{"name":"x","dir":"bin","size_gb":1,"sha256":"","hf":{"repo":"a/b","file":"x"}}],"nodes":[]}]}`,
		"no source":  `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[{"name":"x","dir":"vae","size_gb":1,"sha256":""}],"nodes":[]}]}`,
		"repo space": `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[{"name":"x","dir":"vae","size_gb":1,"sha256":"","hf":{"repo":"a/b c","file":"x"}}],"nodes":[]}]}`,
		"git host":   `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[],"nodes":[{"name":"n","git":"https://evil.example/a/b","commit":"` + usduCommit + `"}]}]}`,
		"commit":     `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[],"nodes":[{"name":"n","git":"https://github.com/a/b","commit":"main"}]}]}`,
		// 同平台 checkGitURL：路径段不能以 . 开头
		"git dot owner": `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[],"nodes":[{"name":"n","git":"https://github.com/.a/b","commit":"` + usduCommit + `"}]}]}`,
		"git dot repo":  `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[],"nodes":[{"name":"n","git":"https://github.com/a/..","commit":"` + usduCommit + `"}]}]}`,
		"slug":          `{"workflows":[{"slug":"../a","version":"1","file":"../a.json","models":[],"nodes":[]}]}`,
		"file":          `{"workflows":[{"slug":"a-b","version":"1","file":"other.json","models":[],"nodes":[]}]}`,
		"not json":      `{"workflows":[`,
		"top array":     `[]`,
		"models obj":    `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":{"a":1},"nodes":[]}]}`,
		"model item":    `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[1],"nodes":[]}]}`,
		"source str":    `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[{"name":"x","dir":"vae","size_gb":1,"sha256":"","hf":"a/b"}],"nodes":[]}]}`,
		"node item":     `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[],"nodes":["x"]}]}`,
		"wf item":       `{"workflows":["a-b"]}`,
		"node commit":   `{"workflows":[{"slug":"a-b","version":"1","file":"a-b.json","models":[],"nodes":[{"name":"n","git":"https://github.com/a/b","commit":"` + usduCommit + `"}]},{"slug":"c-d","version":"1","file":"c-d.json","models":[],"nodes":[{"name":"n","git":"https://github.com/a/b","commit":"` + strings.Repeat("b", 40) + `"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			b := newComfyBox(t, bin)
			os.MkdirAll(filepath.Join(b.app, "workflows"), 0o755)
			os.WriteFile(filepath.Join(b.app, "workflows", "manifest.json"), []byte(body), 0o644)
			os.WriteFile(filepath.Join(b.app, "workflows", "a-b.json"), []byte("{}"), 0o644)
			out, code := b.run("install")
			if code == 0 || !strings.Contains(out, "manifest.json") || strings.Contains(out, "Traceback") {
				t.Fatalf("install = %d:\n%s", code, out)
			}
			if raw, _ := os.ReadFile(b.calls); len(raw) != 0 {
				t.Errorf("calls:\n%s", raw)
			}
		})
	}
}

// 目标位置没有、但同名文件已在旧位置（数据盘的旧名目录 unet / clip，或 1.0.0 的 link_models 软链到的 /opt/ComfyUI/models/<子目录>）：
// 软链到它（解析到最终文件）而不重新下载；清单带 sha256 时先校验，不符就照常下载。
func comfyInstallReusesLegacyModels(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	cache := func(repo, file string) string {
		p := filepath.Join(b.root, "models", "modelscope", repo, file)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("fake:"+repo+"/"+file+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	link := func(target, at string) {
		os.MkdirAll(filepath.Dir(at), 0o755)
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}
	}
	real := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// 1.1.0 预置 FLUX（数据盘 models/unet）、1.0.0 预置 FLUX 的 clip（/opt/ComfyUI/models/clip）、1.0.0 预置 SDXL（/opt/ComfyUI/models/checkpoints）
	flux := cache("AI-ModelScope/FLUX.1-schnell", "flux1-schnell.safetensors")
	link(flux, b.cdata("models", "unet", "flux1-schnell.safetensors"))
	t5 := cache("AI-ModelScope/flux_text_encoders", "t5xxl_fp16.safetensors")
	link(t5, filepath.Join(b.comfy, "models", "clip", "t5xxl_fp16.safetensors"))
	sdxl := cache("AI-ModelScope/stable-diffusion-xl-base-1.0", "sd_xl_base_1.0.safetensors")
	link(sdxl, filepath.Join(b.comfy, "models", "checkpoints", "sd_xl_base_1.0.safetensors"))
	// sha256 不符的旧文件：不复用
	vaeOld := filepath.Join(b.comfy, "models", "vae", "ae.safetensors")
	os.MkdirAll(filepath.Dir(vaeOld), 0o755)
	os.WriteFile(vaeOld, []byte("something else\n"), 0o644)
	vaeSum := sha256.Sum256([]byte("fake:Comfy-Org/flux-vae/ae.safetensors\n"))
	t5Sum := sha256.Sum256([]byte("fake:AI-ModelScope/flux_text_encoders/t5xxl_fp16.safetensors\n"))

	ms := func(name, dir, repo, file, sha string) comfywf.ModelSpec {
		return comfywf.ModelSpec{Name: name, Dir: dir, SizeGB: 1, SHA256: sha, ModelScope: &comfywf.RepoFile{Repo: repo, File: file}}
	}
	b.bundle(comfywf.BundleWorkflow{Slug: "flux-txt2img", Version: "1.0.0", Graph: json.RawMessage(`{"nodes":[]}`),
		Manifest: comfywf.Manifest{Models: []comfywf.ModelSpec{
			ms("flux1-schnell.safetensors", "diffusion_models", "Comfy-Org/flux1-schnell", "flux1-schnell.safetensors", ""),
			ms("t5xxl_fp16.safetensors", "text_encoders", "Comfy-Org/t5", "t5xxl_fp16.safetensors", hex.EncodeToString(t5Sum[:])),
			ms("sd_xl_base_1.0.safetensors", "checkpoints", "Comfy-Org/sdxl", "sd_xl_base_1.0.safetensors", ""),
			ms("ae.safetensors", "vae", "Comfy-Org/flux-vae", "ae.safetensors", hex.EncodeToString(vaeSum[:])),
		}}})
	out, code := b.run("install")
	if code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	for dst, want := range map[string]string{
		b.cdata("models", "diffusion_models", "flux1-schnell.safetensors"): real(flux),
		b.cdata("models", "text_encoders", "t5xxl_fp16.safetensors"):       real(t5),
		b.cdata("models", "checkpoints", "sd_xl_base_1.0.safetensors"):     real(sdxl),
		b.cdata("models", "vae", "ae.safetensors"):                         filepath.Join(b.root, "models", "modelscope", "Comfy-Org/flux-vae", "ae.safetensors"),
	} {
		if got := readLink(t, dst); got != want {
			t.Errorf("%s -> %s, want %s", dst, got, want)
		}
	}
	if calls := b.callLines(); b.countCalls("modelscope download") != 1 || b.countCalls("modelscope download Comfy-Org/flux-vae ae.safetensors ") != 1 {
		t.Errorf("calls:\n%s", strings.Join(calls, "\n"))
	}
	if !strings.Contains(out, "已在") {
		t.Errorf("no reuse log:\n%s", out)
	}
}

// MODEL_ID 选了预置模型：1.0.0 的下载逻辑，软链改到数据盘 comfyui/models/<子目录>。
func comfyInstallPresetModel(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	b.env["MODEL_ID"] = "AI-ModelScope/stable-diffusion-xl-base-1.0"
	if out, code := b.run("install"); code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	if b.countCalls("modelscope download AI-ModelScope/stable-diffusion-xl-base-1.0 sd_xl_base_1.0.safetensors ") != 1 {
		t.Errorf("calls:\n%s", strings.Join(b.callLines(), "\n"))
	}
	if got := readLink(t, b.cdata("models", "checkpoints", "sd_xl_base_1.0.safetensors")); got !=
		filepath.Join(b.root, "models", "modelscope", "AI-ModelScope/stable-diffusion-xl-base-1.0", "sd_xl_base_1.0.safetensors") {
		t.Errorf("link -> %s", got)
	}
}

// start：extra_model_paths.yaml 覆盖全部模型目录（unet / clip 并进 diffusion_models / text_encoders），main.py 带三个数据盘目录参数，
// comfyui.log 轮转，example.png 拷进输入目录，service.pid 是服务进程。
func comfyStart(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	os.WriteFile(filepath.Join(b.app, "comfyui.log"), []byte("previous-run\n"), 0o644)
	// 1.0.0 留在镜像层用户目录里的工作流：第一次启动拷到数据盘
	os.MkdirAll(filepath.Join(b.comfy, "user", "default", "workflows"), 0o755)
	os.WriteFile(filepath.Join(b.comfy, "user", "default", "workflows", "old.json"), []byte("{}"), 0o644)
	out, code := b.run("start")
	if code != 0 {
		t.Fatalf("start = %d:\n%s", code, out)
	}
	cdata := b.cdata()
	wantYAML := "# 由 app.sh start 生成（每次启动覆盖）：模型、自定义节点放在数据盘\n" +
		"spark:\n" +
		"  base_path: '" + cdata + "/'\n" +
		"  is_default: true\n" +
		"  checkpoints: models/checkpoints/\n" +
		"  diffusion_models: |\n    models/diffusion_models/\n    models/unet/\n" +
		"  text_encoders: |\n    models/text_encoders/\n    models/clip/\n" +
		"  vae: models/vae/\n" +
		"  loras: models/loras/\n" +
		"  upscale_models: models/upscale_models/\n" +
		"  controlnet: models/controlnet/\n" +
		"  clip_vision: models/clip_vision/\n" +
		"  embeddings: models/embeddings/\n" +
		"  style_models: models/style_models/\n" +
		"  vae_approx: models/vae_approx/\n" +
		"  custom_nodes: custom_nodes/\n"
	yaml, _ := os.ReadFile(filepath.Join(b.app, "extra_model_paths.yaml"))
	if string(yaml) != wantYAML {
		t.Errorf("extra_model_paths.yaml =\n%s\nwant\n%s", yaml, wantYAML)
	}
	for _, d := range comfywf.ModelDirs {
		if !strings.Contains(string(yaml), "models/"+d+"/") {
			t.Errorf("yaml misses models/%s/", d)
		}
	}

	var args []string
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		raw, _ := os.ReadFile(b.env["FAKE_PY_ARGS"])
		if args = strings.Split(strings.TrimRight(string(raw), "\n"), "\n"); len(raw) > 0 || time.Now().After(deadline) {
			break
		}
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"main.py --listen 127.0.0.1 --port 8188",
		"--extra-model-paths-config " + filepath.Join(b.app, "extra_model_paths.yaml"),
		"--user-directory " + filepath.Join(cdata, "user"),
		"--input-directory " + filepath.Join(cdata, "input"),
		"--output-directory " + filepath.Join(b.root, "comfyui-output")} {
		if !strings.Contains(joined, want) {
			t.Errorf("main.py args missing %q: %s", want, joined)
		}
	}
	if cwd, _ := os.ReadFile(b.env["FAKE_PY_ARGS"] + ".cwd"); strings.TrimSpace(string(cwd)) != b.comfy {
		if real, _ := filepath.EvalSymlinks(b.comfy); strings.TrimSpace(string(cwd)) != real {
			t.Errorf("main.py cwd = %q, want %s", cwd, b.comfy)
		}
	}
	for _, d := range []string{"user", "input", "custom_nodes"} {
		if fi, err := os.Stat(filepath.Join(cdata, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s missing", d)
		}
	}
	if png, _ := os.ReadFile(filepath.Join(cdata, "input", "example.png")); string(png) != "png" {
		t.Errorf("example.png = %q", png)
	}
	if old, _ := os.ReadFile(filepath.Join(b.app, "comfyui.log.1")); string(old) != "previous-run\n" {
		t.Errorf("comfyui.log.1 = %q", old)
	}
	raw, _ := os.ReadFile(filepath.Join(b.app, "service.pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if pid <= 1 || syscall.Kill(pid, 0) != nil {
		t.Fatalf("service.pid = %q not alive", raw)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		cur, _ := os.ReadFile(filepath.Join(b.app, "comfyui.log"))
		if strings.Contains(string(cur), "fresh-run") || time.Now().After(deadline) {
			if strings.Contains(string(cur), "previous-run") || !strings.Contains(string(cur), "fresh-run") {
				t.Errorf("comfyui.log = %q", cur)
			}
			break
		}
	}

	if _, err := os.Stat(filepath.Join(cdata, "user", "default", "workflows", "old.json")); err != nil {
		t.Errorf("1.0.0 user dir not migrated: %v", err)
	}

	// 用户已有的 example.png 不覆盖；迁移只做一次；再次 start 也不报错
	os.Remove(filepath.Join(cdata, "user", "default", "workflows", "old.json"))
	os.WriteFile(filepath.Join(cdata, "input", "example.png"), []byte("mine"), 0o644)
	exec.Command("kill", strconv.Itoa(pid)).Run() //nolint:errcheck
	if out, code := b.run("start"); code != 0 {
		t.Fatalf("second start = %d:\n%s", code, out)
	}
	if png, _ := os.ReadFile(filepath.Join(cdata, "input", "example.png")); string(png) != "mine" {
		t.Errorf("example.png overwritten: %q", png)
	}
	if _, err := os.Stat(filepath.Join(cdata, "user", "default", "workflows", "old.json")); !os.IsNotExist(err) {
		t.Error("user dir migrated twice")
	}
}

// spec.json：supports_workflows、MODEL_ID 默认 none（D15）；app.json 1.1.0。
func TestComfyAppSpec(t *testing.T) {
	var spec appdeploy.Spec
	raw, _ := os.ReadFile(filepath.Join(appsDir, "comfyui", "spec.json"))
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.SupportsWorkflows {
		t.Error("supports_workflows not set")
	}
	var model *appdeploy.EnvSpec
	for i := range spec.Env {
		if spec.Env[i].Name == "MODEL_ID" {
			model = &spec.Env[i]
		}
	}
	if model == nil || len(model.Options) == 0 || model.Options[0] != "none" || model.Default != "none" {
		t.Errorf("MODEL_ID = %+v", model)
	}
	var meta struct{ Version string }
	raw, _ = os.ReadFile(filepath.Join(appsDir, "comfyui", "app.json"))
	if err := json.Unmarshal(raw, &meta); err != nil || meta.Version != "1.1.0" {
		t.Errorf("app.json version = %q %v", meta.Version, err)
	}
}

// 上次中断留下的临时目录（旧版的 custom_nodes/.tmp-*、新版的 .tmp-nodes/*）在装节点前清掉。
func comfyInstallLeftoverTmp(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	for _, d := range []string{b.cdata("custom_nodes", ".tmp-ComfyUI_Old"), b.cdata(".tmp-nodes", "Stale")} {
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "x.py"), nil, 0o644)
	}
	b.bundle(wfUpscale())
	if out, code := b.run("install"); code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	for _, d := range []string{b.cdata("custom_nodes", ".tmp-ComfyUI_Old"), b.cdata(".tmp-nodes")} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", d, err)
		}
	}
	if _, err := os.Stat(b.cdata("custom_nodes", "ComfyUI_UltimateSDUpscale", ".spark-commit")); err != nil {
		t.Error("node not installed")
	}
}

// 目标是失效的软链（缓存被清掉）→ 不算已存在，重新下载并改链。
func comfyInstallDanglingLink(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	dst := b.cdata("models", "checkpoints", "sd_xl_base_1.0.safetensors")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.Symlink(filepath.Join(b.root, "gone", "sd_xl_base_1.0.safetensors"), dst); err != nil {
		t.Fatal(err)
	}
	b.bundle(wfTxt2Img())
	if out, code := b.run("install"); code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	if b.countCalls("modelscope download ") != 1 {
		t.Errorf("calls:\n%s", strings.Join(b.callLines(), "\n"))
	}
	if got := readLink(t, dst); got != filepath.Join(b.root, "models", "modelscope", "AI-ModelScope/stable-diffusion-xl-base-1.0", "sd_xl_base_1.0.safetensors") {
		t.Errorf("link -> %s", got)
	}
}

// 所选来源下载失败、另一个来源存在 → 换它再试；只有一个来源时直接失败。
func comfyInstallSourceRetry(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	b.env["FAKE_DL_FAIL"] = "modelscope"
	b.bundle(wfTxt2Img())
	if out, code := b.run("install"); code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	if b.countCalls("modelscope download ") != 1 || b.countCalls("hf download stabilityai/stable-diffusion-xl-base-1.0 ") != 1 {
		t.Errorf("calls:\n%s", strings.Join(b.callLines(), "\n"))
	}
	if got := readLink(t, b.cdata("models", "checkpoints", "sd_xl_base_1.0.safetensors")); got !=
		filepath.Join(b.root, "models", "hf", "stabilityai/stable-diffusion-xl-base-1.0", "sd_xl_base_1.0.safetensors") {
		t.Errorf("link -> %s", got)
	}

	b = newComfyBox(t, bin)
	b.env["FAKE_DL_FAIL"] = "modelscope"
	b.bundle(comfywf.BundleWorkflow{Slug: "t5-only", Version: "1.0.0", Graph: json.RawMessage(`{"nodes":[],"links":[]}`),
		Manifest: comfywf.Manifest{Models: []comfywf.ModelSpec{t5ModelMSOnly()}}})
	out, code := b.run("install")
	if code == 0 || !strings.Contains(out, "t5xxl_fp8_e4m3fn.safetensors") || b.countCalls("hf ") != 0 {
		t.Fatalf("install = %d:\n%s\ncalls: %v", code, out, b.callLines())
	}
}

// 预置模型 + 工作流清单都有：预置模型占 5～35、工作流模型 35～70、节点 70～78、最后 79，进度不回退；中间不报 78。
// sha256 校验前也有一条进度。
func comfyInstallProgressMonotonic(t *testing.T, bin string) {
	b := newComfyBox(t, bin)
	b.env["MODEL_ID"] = "AI-ModelScope/FLUX.1-schnell"
	w := wfUpscale()
	m := upscaleModelHFOnly()
	sum := sha256.Sum256([]byte("fake:" + m.HF.Repo + "/" + m.HF.File + "\n"))
	m.SHA256 = hex.EncodeToString(sum[:])
	w.Manifest.Models = []comfywf.ModelSpec{sdxlModel(), m}
	b.bundle(w)
	if out, code := b.run("install"); code != 0 {
		t.Fatalf("install = %d:\n%s", code, out)
	}
	raw, _ := os.ReadFile(filepath.Join(b.app, "log"))
	var pcts []int
	var msgs []string
	for _, l := range strings.Split(string(raw), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || f[0] != "[spark_status]" {
			continue
		}
		p, err := strconv.Atoi(strings.TrimSuffix(f[2], "%"))
		if err != nil {
			t.Fatalf("log line %q", l)
		}
		pcts = append(pcts, p)
		msgs = append(msgs, strings.Join(f[3:], " "))
	}
	if len(pcts) < 5 || pcts[0] != 5 || pcts[len(pcts)-1] != 79 {
		t.Fatalf("progress = %v %q", pcts, msgs)
	}
	for i := 1; i < len(pcts); i++ {
		if pcts[i] < pcts[i-1] || pcts[i] == 78 {
			t.Fatalf("progress not monotonic / reported 78: %v %q", pcts, msgs)
		}
	}
	joined := strings.Join(msgs, "|")
	for _, want := range []string{"预置模型就绪", "下载模型 sd_xl_base_1.0.safetensors（1/2", "校验 sha256：4x-UltraSharp.pth", "安装节点 ComfyUI_UltimateSDUpscale"} {
		if !strings.Contains(joined, want) {
			t.Errorf("status messages missing %q: %q", want, msgs)
		}
	}
	// 预置模型之后的第一条工作流模型进度从 35 起
	for i, msg := range msgs {
		if strings.HasPrefix(msg, "下载模型 sd_xl_base") && pcts[i] != 35 {
			t.Errorf("first workflow model at %d%%, want 35", pcts[i])
		}
	}
}
