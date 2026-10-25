package e2ehooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/app"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
)

// fakeAppScript 是全部 fake 应用版本共用的 app.sh：install 汇报一次下载进度后等 1 秒；start 起一个挂住的 sleep 当服务进程
// （不真的监听端口：run.sh 与 Runner 的健康检查经 AppHealthAddr / sshtest 的 direct-tcpip 目标打到测试起的 httptest）。
const fakeAppScript = `#!/bin/sh
# e2e / 集成测试用的 fake 应用（不下载、不起真服务）
case "$1" in
  install)
    spark_status download 50 "downloading $MODEL_ID"
    sleep 1
    ;;
  start)
    sleep 100000 </dev/null >/dev/null 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    ;;
esac
`

// fakeComfyScript 是 fake comfyui（1.1.0 语义：supports_workflows）的 app.sh：install 按部署包里的 workflows/manifest.json
// （平台生成的紧凑 JSON，按 '{' 拆行后逐行取字段，不依赖 python3）在 $SPARK_DATA_DIR/comfyui 下模拟安装——
// 每个 wf 复制为 user/default/workflows/spark-<slug>.json（目录里不在清单中的 spark-*.json 删掉，其它文件不动）、
// 每个模型 touch models/<dir>/<name>（名字含 fail-disk 且存在标记文件 $SPARK_DATA_DIR/.e2e-disk-full → spark_status 汇报 "disk: …"
// 并退出 2；删掉标记后重新部署即成功）、
// 每个节点 mkdir custom_nodes/<name> 并写 .spark-commit；已存在的模型 / commit 相同的节点跳过（日志 "skip model" / "skip node"）。
// start 同 fakeAppScript。
const fakeComfyScript = `#!/bin/sh
# e2e / 集成测试用的 fake ComfyUI（不下载、不 clone、不起真服务）
C="$SPARK_DATA_DIR/comfyui"
M="$SPARK_APP_DIR/workflows/manifest.json"
field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }
case "$1" in
  install)
    if [ "${MODEL_ID:-none}" != "none" ]; then
      spark_status download 50 "downloading $MODEL_ID"
    fi
    mkdir -p "$C/user/default/workflows" "$C/custom_nodes" "$C/models" || exit 1
    keep=" "
    n=0
    if [ -f "$M" ]; then
      tr '{' '\n' < "$M" > "$SPARK_APP_DIR/.manifest.lines"
      while IFS= read -r line; do
        case "$line" in
          *'"slug":'*)
            slug=$(printf '%s\n' "$line" | field slug)
            file=$(printf '%s\n' "$line" | field file)
            cp "$SPARK_APP_DIR/workflows/$file" "$C/user/default/workflows/spark-$slug.json" || exit 1
            keep="$keep spark-$slug.json "
            n=$((n + 1))
            echo "workflow $slug"
            ;;
          *'"name":'*'"dir":'*)
            name=$(printf '%s\n' "$line" | field name)
            dir=$(printf '%s\n' "$line" | field dir)
            case "$name" in
              *fail-disk*)
                if [ -e "$SPARK_DATA_DIR/.e2e-disk-full" ]; then
                  spark_status download 10 "disk: 数据盘空间不足，无法下载模型 $name"
                  echo "disk: no space for $name" >&2
                  exit 2
                fi
                ;;
            esac
            if [ -e "$C/models/$dir/$name" ]; then
              echo "skip model $dir/$name"
            else
              mkdir -p "$C/models/$dir" && touch "$C/models/$dir/$name" || exit 1
              spark_status download 40 "下载模型 $name"
              echo "model $dir/$name"
            fi
            ;;
          *'"name":'*'"commit":'*)
            name=$(printf '%s\n' "$line" | field name)
            commit=$(printf '%s\n' "$line" | field commit)
            if [ "$(cat "$C/custom_nodes/$name/.spark-commit" 2>/dev/null)" = "$commit" ]; then
              echo "skip node $name"
            else
              mkdir -p "$C/custom_nodes/$name" && printf '%s\n' "$commit" > "$C/custom_nodes/$name/.spark-commit" || exit 1
              spark_status install 75 "安装节点 $name"
              echo "node $name $commit"
            fi
            ;;
        esac
      done < "$SPARK_APP_DIR/.manifest.lines"
      rm -f "$SPARK_APP_DIR/.manifest.lines"
    fi
    for f in "$C/user/default/workflows"/spark-*.json; do
      [ -e "$f" ] || continue
      b=$(basename "$f")
      case "$keep" in
        *" $b "*) ;;
        *) rm -f "$f"; echo "removed workflow file $b" ;;
      esac
    done
    spark_status install 79 "工作流就绪：$n 个"
    sleep 1
    ;;
  start)
    sleep 100000 </dev/null >/dev/null 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    ;;
esac
`

// 钩子另建的两个镜像（ref 只是标签：e2e 不拉镜像）。
const (
	ImageLLMServing = "LLM Serving"
	ImageComfyUI    = "ComfyUI"
)

var fakeImageRefs = map[string]string{ImageLLMServing: "spark/llm-serving:e2e", ImageComfyUI: "spark/comfyui:e2e"}

// SeededApp 是 /hooks/apps/seed 的返回项。
type SeededApp struct {
	ID        int64  `json:"id"`
	Slug      string `json:"slug"`
	VersionID int64  `json:"version_id"`
	Port      int    `json:"port"`
}

func i64(v int64) *int64 { return &v }

// fakeApp 与 deploy/apps/<slug> 的端口 / 变量 / 链接 / 超时相同；不设 min_data_free_gib（开发机磁盘不一定有 50GB 空闲）。
type fakeApp struct {
	in     appdeploy.AppInput
	spec   appdeploy.Spec
	images []string // 兼容镜像（名称）；"" = 默认镜像（dbtest / 迁移种子的第一个启用镜像）
	// version / script 为空 = "1.0.0" / fakeAppScript
	version, script string
}

func fakeApps() []fakeApp {
	source := appdeploy.EnvSpec{Name: "MODEL_SOURCE", Label: "下载源", Type: "enum", Options: []string{"modelscope", "hf-mirror"}, Default: "modelscope"}
	return []fakeApp{
		{in: appdeploy.AppInput{Slug: "vllm", Name: "vLLM 推理服务", Summary: "OpenAI 兼容的大模型推理接口（Qwen3 等）", Category: "llm", Icon: "llm", Sort: 10},
			spec: appdeploy.Spec{
				Ports:          []appdeploy.PortSpec{{Port: 8000, Label: "OpenAI 兼容接口", Path: "/docs", HealthPath: "/health"}},
				TimeoutMinutes: 90,
				Env: []appdeploy.EnvSpec{
					{Name: "MODEL_ID", Label: "模型", Type: "enum", Required: true, Options: []string{"Qwen/Qwen3-8B", "Qwen/Qwen3-4B", "Qwen/Qwen2.5-7B-Instruct", "Qwen/Qwen3-14B"},
						Default: "Qwen/Qwen3-8B", Help: "ModelScope 模型 ID"},
					source,
					{Name: "MAX_MODEL_LEN", Label: "最大上下文", Type: "int", Min: i64(1024), Max: i64(131072), Default: "32768"},
					{Name: "GPU_MEMORY_UTILIZATION", Label: "显存占用比例", Type: "string", Pattern: `^0\.[3-9][0-9]?$`, Default: "0.7"},
					{Name: "API_KEY", Label: "接口密钥", Type: "secret", Pattern: `^[A-Za-z0-9_-]{8,128}$`, Help: "留空则接口不鉴权（仅建议私有端口）"},
				},
				Links: []appdeploy.LinkSpec{{Label: "接口文档", Path: "/docs"}, {Label: "模型列表", Path: "/v1/models"}},
			},
			images: []string{"", ImageLLMServing}},
		{in: appdeploy.AppInput{Slug: "open-webui", Name: "Open WebUI 对话界面", Summary: "类 ChatGPT 的网页对话界面", Category: "llm", Icon: "chat", Sort: 20},
			spec: appdeploy.Spec{
				Ports:          []appdeploy.PortSpec{{Port: 3000, Label: "对话界面", HealthPath: "/health"}},
				TimeoutMinutes: 15,
				Env: []appdeploy.EnvSpec{
					{Name: "OPENAI_API_BASE_URL", Label: "接口地址", Type: "string", Pattern: `^https?://[^\s]{1,200}$`, Default: "http://127.0.0.1:8000/v1"},
					{Name: "OPENAI_API_KEY", Label: "接口密钥", Type: "secret"},
					{Name: "WEBUI_SECRET_KEY", Label: "会话密钥", Type: "secret"},
				},
			},
			images: []string{"", ImageLLMServing}},
		{in: appdeploy.AppInput{Slug: "comfyui", Name: "ComfyUI 图像生成", Summary: "节点式工作流的文生图界面", Category: "image", Icon: "image", Sort: 30},
			spec: appdeploy.Spec{
				Ports:             []appdeploy.PortSpec{{Port: 8188, Label: "ComfyUI 界面", HealthPath: "/system_stats"}},
				TimeoutMinutes:    90,
				SupportsWorkflows: true, // 同 deploy/apps/comfyui 1.1.0
				Env: []appdeploy.EnvSpec{
					{Name: "MODEL_ID", Label: "模型", Type: "enum", Required: true,
						Options: []string{"none", "AI-ModelScope/stable-diffusion-xl-base-1.0", "AI-ModelScope/FLUX.1-schnell", "Qwen/Qwen-Image"},
						Default: "none"},
					source,
				},
			},
			images: []string{ImageComfyUI}, version: "1.1.0", script: fakeComfyScript},
		{in: appdeploy.AppInput{Slug: "jupyterlab", Name: "JupyterLab", Summary: "网页版 Python 开发环境", Category: "notebook", Icon: "notebook", Sort: 40},
			spec: appdeploy.Spec{
				Ports:          []appdeploy.PortSpec{{Port: 8888, Label: "JupyterLab", HealthPath: "/api"}},
				TimeoutMinutes: 10,
				Env: []appdeploy.EnvSpec{
					{Name: "JUPYTER_TOKEN", Label: "访问令牌", Type: "secret", Required: true, Pattern: `^[A-Za-z0-9]{16,64}$`},
				},
			},
			images: []string{"", ImageLLMServing, ImageComfyUI}},
		// 平台内部应用（Token 商城节点，计划 9）：与 deploy/apps/vllm-node 的端口 / 变量 / 超时相同；internal → 用户市场不可见，
		// 只有系统账号能部署。后台建模型（llm_models）要求它已发布、模型镜像在兼容列表内。
		{in: appdeploy.AppInput{Slug: "vllm-node", Name: "vLLM 节点（平台内部）", Summary: "Token 商城推理节点，平台自动部署", Category: "llm", Icon: "llm",
			Sort: 999, Internal: true},
			spec: appdeploy.Spec{
				Ports:          []appdeploy.PortSpec{{Port: 8000, Label: "OpenAI 兼容接口", Path: "/docs", HealthPath: "/health"}},
				TimeoutMinutes: 120,
				Env: []appdeploy.EnvSpec{
					{Name: "MODEL_ID", Label: "模型", Type: "string", Required: true, Pattern: `^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`},
					source,
					{Name: "SERVED_MODEL_NAME", Label: "对外模型名", Type: "string", Required: true, Pattern: `^[a-z0-9][a-z0-9.-]{1,63}$`},
					{Name: "MAX_MODEL_LEN", Label: "最大上下文", Type: "int", Min: i64(1024), Max: i64(1048576), Default: "32768"},
					{Name: "API_KEY", Label: "节点密钥", Type: "secret", Required: true, Pattern: `^[A-Za-z0-9_-]{32,128}$`},
					{Name: "GPU_MEMORY_UTILIZATION", Label: "显存占用比例", Type: "string", Pattern: `^0\.[3-9][0-9]?$`, Default: "0.85"},
					{Name: "MAX_NUM_SEQS", Label: "最大并发序列", Type: "int", Min: i64(1), Max: i64(512), Default: "64"},
					{Name: "TASK", Label: "任务类型", Type: "enum", Options: []string{"generate", "embed"}, Default: "generate"},
				},
				Links: []appdeploy.LinkSpec{{Label: "接口文档", Path: "/docs"}, {Label: "模型列表", Path: "/v1/models"}},
			},
			images: []string{"", ImageLLMServing}},
	}
}

// seedImages 返回镜像名 → id：""（默认镜像，不改名不改动）、LLM Serving、ComfyUI（不存在则新建，已存在则复用）。
// LLM Serving 另建而不是把默认镜像改名：整轮 Playwright 共用一个数据库，改名会让之后断言镜像名称的用例（A-INS 等）失败。
func seedImages(ctx context.Context, a *app.App) (map[string]int64, error) {
	imgs, err := a.Catalog.AdminImages(ctx)
	if err != nil {
		return nil, err
	}
	ids := map[string]int64{}
	for _, img := range imgs {
		if _, ours := fakeImageRefs[img.Name]; ours {
			if ids[img.Name] == 0 && img.Ref == fakeImageRefs[img.Name] {
				ids[img.Name] = img.ID
			}
			continue
		}
		if img.Enabled && ids[""] == 0 {
			ids[""] = img.ID
		}
	}
	if ids[""] == 0 {
		return nil, errors.New("no enabled default image")
	}
	for i, name := range []string{ImageLLMServing, ImageComfyUI} {
		if ids[name] != 0 {
			continue
		}
		img, err := a.Catalog.CreateImage(ctx, audit.Actor{}, catalog.ImageInput{Name: name, Ref: fakeImageRefs[name],
			Description: "e2e 应用镜像（fake）", Enabled: true, Sort: 1000 + i})
		if err != nil {
			return nil, fmt.Errorf("create image %s: %w", name, err)
		}
		ids[name] = img.ID
	}
	return ids, nil
}

// SeedApps 创建并发布（系统操作人，钩子直接调服务，不经 TOTP）四个用户应用与平台内部应用 vllm-node 的 fake 版本，并让假容器 sshd 把每个应用端口的
// direct-tcpip 转到 healthAddr（Runner 的 HTTP 健康检查与 ingress 都打到它）。幂等：已存在同 slug 的应用直接返回其最新已发布版本。
// 集成测试（internal/app）与 /hooks/apps/seed 共用。
func SeedApps(ctx context.Context, a *app.App, up *sshtest.Server, healthAddr string) ([]SeededApp, error) {
	imgIDs, err := seedImages(ctx, a)
	if err != nil {
		return nil, err
	}
	out := make([]SeededApp, 0, len(fakeApps()))
	for _, fa := range fakeApps() {
		port := fa.spec.Ports[0].Port
		if up != nil && healthAddr != "" {
			up.SetTarget(port, healthAddr)
		}
		existing, err := a.AppCatalog.AppBySlug(ctx, fa.in.Slug)
		if err == nil {
			v, err := a.AppCatalog.LatestPublished(ctx, a.Fleet.Pool(), existing.ID)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", fa.in.Slug, err)
			}
			out = append(out, SeededApp{ID: existing.ID, Slug: existing.Slug, VersionID: v.ID, Port: v.Spec.Ports[0].Port})
			continue
		}
		if !errors.Is(err, apperr.ErrNotFound) {
			return nil, err
		}
		in := fa.in
		in.Status = appdeploy.StatusPublished
		ap, err := a.AppCatalog.CreateApp(ctx, audit.Actor{}, in)
		if err != nil {
			return nil, fmt.Errorf("create app %s: %w", fa.in.Slug, err)
		}
		version, script := fa.version, fa.script
		if version == "" {
			version, script = "1.0.0", fakeAppScript
		}
		vin := appdeploy.VersionInput{Version: version, Spec: fa.spec, Script: script, Changelog: "e2e fake"}
		for _, name := range fa.images {
			vin.ImageIDs = append(vin.ImageIDs, imgIDs[name])
		}
		v, err := a.AppCatalog.CreateVersion(ctx, audit.Actor{}, ap.ID, vin)
		if err != nil {
			return nil, fmt.Errorf("create version %s: %w", fa.in.Slug, err)
		}
		if v, _, err = a.AppCatalog.PublishVersion(ctx, audit.Actor{}, ap.ID, v.ID); err != nil {
			return nil, fmt.Errorf("publish %s: %w", fa.in.Slug, err)
		}
		out = append(out, SeededApp{ID: ap.ID, Slug: ap.Slug, VersionID: v.ID, Port: port})
	}
	return out, nil
}

// StopFakeApps 在宿主机上对假容器里每个应用目录（<root>/<实例>/root/data/.spark-apps/<slug>）执行 run.sh stop：
// run.sh 用 setsid 把 _bg 与服务进程移出了 sshtest 的进程组，sshtest.Close 杀不到它们（fake 服务是 sleep 100000）。
func StopFakeApps(root string) {
	if root == "" {
		return
	}
	scripts, _ := filepath.Glob(filepath.Join(root, "*", "root", "data", appdeploy.AppDirName, "*", "run.sh"))
	var wg sync.WaitGroup
	for _, s := range scripts {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			exec.CommandContext(ctx, "sh", s, "stop").Run() //nolint:errcheck
		})
	}
	wg.Wait()
}

func (s *Server) appsSeed(ctx context.Context, _ json.RawMessage) (any, error) {
	if s.Upstream == nil || s.HealthAddr == "" {
		return nil, errors.New("fake upstream / health server not configured")
	}
	apps, err := SeedApps(ctx, s.App, s.Upstream, s.HealthAddr)
	if err != nil {
		return nil, err
	}
	if s.Comfy != nil { // comfyui 的 8188 改指 fake ComfyUI（它也回 /system_stats，健康检查照常）
		if err := s.serveComfy(); err != nil {
			return nil, err
		}
	}
	return map[string]any{"apps": apps}, nil
}

// tickPause 是每轮 Runner 之前的真实等待：让容器里的脚本（后台 _bg）推进。
const tickPause = 200 * time.Millisecond

// appsTick 执行 n 轮（默认 1，最多 200）Runner：每轮先推进假时钟 PollInterval（让 next_poll_at 到点）并真实等 200ms，再 Tick。
func (s *Server) appsTick(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		N *int `json:"n"`
	}](body)
	if err != nil {
		return nil, err
	}
	n := 1
	if req.N != nil {
		n = *req.N
	}
	if n < 1 || n > 200 {
		return nil, badRequest("n 必须在 1～200 之间")
	}
	processed := 0
	for i := 0; i < n; i++ {
		s.Clock.Advance(appdeploy.PollInterval)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(tickPause):
		}
		k, locked, err := s.App.AppRunner.Tick(ctx)
		if err != nil { // 与 Runner.Run 一样只记日志：单个部署的 exec 出错由下一轮重试，不让整轮钩子失败
			s.Log.Warn("apps tick", "round", i+1, "err", err)
		}
		if !locked {
			return nil, errors.New("runner advisory lock held elsewhere")
		}
		processed += k
	}
	return map[string]int{"rounds": n, "processed": processed}, nil
}
