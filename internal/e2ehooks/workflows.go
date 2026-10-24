package e2ehooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"

	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/comfywf"
	"spark-cloud/internal/comfywf/comfytest"
	"spark-cloud/internal/platform/apperr"
)

// 两个 fake 工作流（/hooks/workflows/seed，集成测试共用）：A 无自定义节点、有封面；B 带一个自定义节点 FakeNode。
// 两者都有 api_prompt 与快速出图参数 prompt / steps / seed，输出节点 "9"（SaveImage）。
const (
	WorkflowA     = "e2e-wf-a"
	WorkflowB     = "e2e-wf-b"
	FakeNodeName  = "FakeNode"
	FakeNodeType  = "FakeNodeType"
	FakeNodeGit   = "https://github.com/spark-e2e/FakeNode.git"
	FakeModelA    = "e2e-a.safetensors"
	FakeModelB    = "e2e-b.safetensors"
	FailDiskModel = "e2e-fail-disk.safetensors" // 名字含 fail-disk：容器里有 DiskFullMarker 时 fake install 汇报 "disk: …" 并退出 2
	// DiskFullMarker 是假容器 $SPARK_DATA_DIR（<实例根>/root/data）下的标记文件：存在时 fail-disk 模型安装失败，删掉后重新部署成功。
	DiskFullMarker = ".e2e-disk-full"
	fakeModelDir   = "checkpoints"
	fakeOutputNode = "9"
)

// FakeNodeCommit 是 FakeNode 钉死的 commit（40 个 a）。
var FakeNodeCommit = strings.Repeat("a", 40)

// FakeWorkflow 描述一个 fake 工作流：一个 checkpoints 模型（Model），可选一个自定义节点 FakeNode、可选封面（1×1 PNG）。
type FakeWorkflow struct {
	Slug, Name, Summary, Category string
	Model                         string
	SizeGB                        float64
	Node                          bool
	Cover                         bool
	Image                         bool // 再加一个 image 类型的快速出图参数 image（LoadImage 节点 "10"，必填）
	Sort                          int
}

// SeededWorkflow 是 /hooks/workflows/seed 与 /hooks/workflows/version 的返回项。
type SeededWorkflow struct {
	ID        int64  `json:"id"`
	Slug      string `json:"slug"`
	VersionID int64  `json:"version_id"`
	Version   string `json:"version"`
}

// FakeWorkflows 返回 A、B 的定义。
func FakeWorkflows() []FakeWorkflow {
	return []FakeWorkflow{
		{Slug: WorkflowA, Name: "E2E 文生图 A", Summary: "fake 工作流 A（无自定义节点）", Category: comfywf.CategoryTxt2Img,
			Model: FakeModelA, SizeGB: 2.5, Cover: true, Sort: 10},
		{Slug: WorkflowB, Name: "E2E 节点工作流 B", Summary: "fake 工作流 B（自定义节点 FakeNode）", Category: comfywf.CategoryUpscale,
			Model: FakeModelB, SizeGB: 1.2, Node: true, Sort: 20},
	}
}

// fakeGraph 是 UI 格式 graph：checkpoint 加载器（引用 model）+ SaveImage，Node 时再加一个 FakeNodeType 节点（cnr_id 非 comfy-core）。
func fakeGraph(fw FakeWorkflow) json.RawMessage {
	nodes := []map[string]any{
		{"id": 4, "type": "CheckpointLoaderSimple", "mode": 0, "properties": map[string]any{"cnr_id": "comfy-core"}, "widgets_values": []any{fw.Model}},
		{"id": 6, "type": "CLIPTextEncode", "mode": 0, "properties": map[string]any{"cnr_id": "comfy-core"}, "widgets_values": []any{"a cat"}},
		{"id": 9, "type": "SaveImage", "mode": 0, "properties": map[string]any{"cnr_id": "comfy-core"}, "widgets_values": []any{"ComfyUI"}},
	}
	if fw.Node {
		nodes = append(nodes, map[string]any{"id": 10, "type": FakeNodeType, "mode": 0, "properties": map[string]any{"cnr_id": strings.ToLower(FakeNodeName)},
			"widgets_values": []any{}})
	}
	b, _ := json.Marshal(map[string]any{"last_node_id": 10, "last_link_id": 0, "nodes": nodes, "links": []any{}, "version": 0.4})
	return b
}

// fakeAPIPrompt 是 API 格式 prompt（最小的 txt2img 链路；Node 时图像先过 FakeNodeType 再保存）。
func fakeAPIPrompt(fw FakeWorkflow) json.RawMessage {
	images := []any{"8", 0}
	p := map[string]any{
		"3": map[string]any{"class_type": "KSampler", "inputs": map[string]any{"seed": 1, "steps": 20, "cfg": 7, "sampler_name": "euler",
			"scheduler": "normal", "denoise": 1, "model": []any{"4", 0}, "positive": []any{"6", 0}, "negative": []any{"7", 0}, "latent_image": []any{"5", 0}}},
		"4": map[string]any{"class_type": "CheckpointLoaderSimple", "inputs": map[string]any{"ckpt_name": fw.Model}},
		"5": map[string]any{"class_type": "EmptyLatentImage", "inputs": map[string]any{"width": 512, "height": 512, "batch_size": 1}},
		"6": map[string]any{"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "a cat", "clip": []any{"4", 1}}},
		"7": map[string]any{"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": "", "clip": []any{"4", 1}}},
		"8": map[string]any{"class_type": "VAEDecode", "inputs": map[string]any{"samples": []any{"3", 0}, "vae": []any{"4", 2}}},
	}
	if fw.Node {
		p["10"] = map[string]any{"class_type": FakeNodeType, "inputs": map[string]any{"images": []any{"8", 0}}}
		images = []any{"10", 0}
	}
	if fw.Image {
		p["11"] = map[string]any{"class_type": "LoadImage", "inputs": map[string]any{"image": "example.png"}}
	}
	p[fakeOutputNode] = map[string]any{"class_type": "SaveImage", "inputs": map[string]any{"filename_prefix": "ComfyUI", "images": images}}
	b, _ := json.Marshal(p)
	return b
}

func fp(v float64) *float64 { return &v }

func fakeManifest(fw FakeWorkflow) comfywf.Manifest {
	size := fw.SizeGB
	if size == 0 {
		size = 1
	}
	m := comfywf.Manifest{
		ComfyUIMin: "0.3.60",
		Models: []comfywf.ModelSpec{{Name: fw.Model, Dir: fakeModelDir, SizeGB: size,
			ModelScope: &comfywf.RepoFile{Repo: "spark-e2e/fake-models", File: fw.Model}}},
		Inputs: []comfywf.InputSpec{
			{Key: "prompt", Label: "提示词", Type: comfywf.InputTextarea, Node: "6", Input: "text", Default: "a cat", MaxLen: 500},
			{Key: "steps", Label: "步数", Type: comfywf.InputInt, Node: "3", Input: "steps", Default: 20.0, Min: fp(1), Max: fp(50), Step: fp(1)},
			{Key: "seed", Label: "种子", Type: comfywf.InputSeed, Node: "3", Input: "seed"},
		},
		Outputs: []comfywf.OutputSpec{{Node: fakeOutputNode}},
	}
	if fw.Image {
		m.Inputs = append(m.Inputs, comfywf.InputSpec{Key: "image", Label: "参考图", Type: comfywf.InputImage, Node: "11", Input: "image"})
	}
	if fw.Node {
		m.Nodes = []comfywf.NodeSpec{{Name: FakeNodeName, Git: FakeNodeGit, Commit: FakeNodeCommit, NodeTypes: []string{FakeNodeType}}}
	}
	return m
}

// latestSeeded 返回已存在工作流的最新已发布版本。
func latestSeeded(ctx context.Context, a *app.App, w comfywf.Workflow) (SeededWorkflow, error) {
	v, err := a.Workflows.LatestPublished(ctx, a.Fleet.Pool(), w.ID)
	if err != nil {
		return SeededWorkflow{}, fmt.Errorf("%s: %w", w.Slug, err)
	}
	return SeededWorkflow{ID: w.ID, Slug: w.Slug, VersionID: v.ID, Version: v.Version}, nil
}

// seedMu 串行化 fake 工作流的创建 / 发版（并发的 seed 钩子不会撞 slug 唯一键或版本号）。
var seedMu sync.Mutex

func isWorkflowNotFound(err error) bool {
	return errors.Is(err, apperr.ErrWorkflowNotFound) || errors.Is(err, apperr.ErrNotFound)
}

// CreateFakeWorkflow 建并上架一个 fake 工作流、建并发布版本 1.0.0（系统操作人，直接调服务，不经 TOTP），可选封面。
// 幂等：同 slug 已存在 → 返回其最新已发布版本；自愈：已存在但没有已发布版本（例如上次在发布前失败）→ 发布最新的草稿，
// 没有草稿则按 fw 新建一个版本并发布；缺封面则补上；未上架则上架。
func CreateFakeWorkflow(ctx context.Context, a *app.App, fw FakeWorkflow) (SeededWorkflow, error) {
	seedMu.Lock()
	defer seedMu.Unlock()
	actor := audit.Actor{}
	w, err := a.Workflows.WorkflowBySlug(ctx, fw.Slug)
	switch {
	case isWorkflowNotFound(err):
		if w, err = a.Workflows.CreateWorkflow(ctx, actor, comfywf.WorkflowInput{Slug: fw.Slug, Name: fw.Name, Summary: fw.Summary,
			Description: "e2e 用的 fake 工作流：模型与节点不会真的下载。", Category: fw.Category, Tags: []string{"e2e"}, Sort: fw.Sort,
			Status: comfywf.StatusPublished}); err != nil {
			return SeededWorkflow{}, fmt.Errorf("create workflow %s: %w", fw.Slug, err)
		}
	case err != nil:
		return SeededWorkflow{}, err
	}
	if w.Status != comfywf.StatusPublished {
		if w, err = a.Workflows.UpdateWorkflow(ctx, actor, w.ID, comfywf.WorkflowInput{Name: w.Name, Summary: w.Summary, Description: w.Description,
			Category: w.Category, Tags: w.Tags, Sort: w.Sort, Status: comfywf.StatusPublished}); err != nil {
			return SeededWorkflow{}, fmt.Errorf("list workflow %s: %w", fw.Slug, err)
		}
	}
	if fw.Cover && !w.HasCover {
		if _, err := a.Workflows.SetCover(ctx, actor, w.ID, bytes.NewReader(comfytest.OnePixelPNG)); err != nil {
			return SeededWorkflow{}, fmt.Errorf("cover %s: %w", fw.Slug, err)
		}
	}
	if _, err := a.Workflows.LatestPublished(ctx, a.Fleet.Pool(), w.ID); err == nil {
		return latestSeeded(ctx, a, w)
	} else if !errors.Is(err, apperr.ErrWorkflowUnavailable) {
		return SeededWorkflow{}, err
	}
	vs, err := a.Workflows.ListVersions(ctx, w.ID) // 按创建倒序
	if err != nil {
		return SeededWorkflow{}, err
	}
	var draft *comfywf.Version
	for i := range vs {
		if vs[i].Status == comfywf.StatusDraft {
			draft = &vs[i]
			break
		}
	}
	if draft == nil {
		changelog := "首个版本"
		version := "1.0.0"
		if len(vs) > 0 {
			version, changelog = nextPatch(vs), "e2e 补发版本"
		}
		v, err := a.Workflows.CreateVersion(ctx, actor, w.ID, comfywf.VersionInput{Version: version, Graph: fakeGraph(fw), APIPrompt: fakeAPIPrompt(fw),
			Manifest: fakeManifest(fw), Changelog: &changelog})
		if err != nil {
			return SeededWorkflow{}, fmt.Errorf("create version %s: %w", fw.Slug, err)
		}
		draft = &v
	}
	v, err := a.Workflows.PublishVersion(ctx, actor, w.ID, draft.ID)
	if err != nil {
		return SeededWorkflow{}, fmt.Errorf("publish %s: %w", fw.Slug, err)
	}
	return SeededWorkflow{ID: w.ID, Slug: w.Slug, VersionID: v.ID, Version: v.Version}, nil
}

// nextPatch：已有版本里形如 X.Y.Z 的最大者把 Z + 1（都不是这种形状 → 1.0.<版本数>）。
func nextPatch(vs []comfywf.Version) string {
	best, found := [3]int{}, false
	for _, v := range vs {
		var x [3]int
		if n, err := fmt.Sscanf(v.Version, "%d.%d.%d", &x[0], &x[1], &x[2]); err != nil || n != 3 || fmt.Sprintf("%d.%d.%d", x[0], x[1], x[2]) != v.Version {
			continue
		}
		if !found || x[0] > best[0] || (x[0] == best[0] && (x[1] > best[1] || (x[1] == best[1] && x[2] > best[2]))) {
			best, found = x, true
		}
	}
	if !found {
		return fmt.Sprintf("1.0.%d", len(vs))
	}
	return fmt.Sprintf("%d.%d.%d", best[0], best[1], best[2]+1)
}

// SeedWorkflows 幂等地建 A、B（按此顺序返回）。
func SeedWorkflows(ctx context.Context, a *app.App) ([]SeededWorkflow, error) {
	out := []SeededWorkflow{}
	for _, fw := range FakeWorkflows() {
		s, err := CreateFakeWorkflow(ctx, a, fw)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// PublishFakeVersion 以工作流最新已发布版本的内容（graph / api_prompt / manifest 不变）建并发布新版本 version
// （空 = 已有 X.Y.Z 版本中最大者的 Z + 1）；changelog 空 = "e2e 新版本 <version>"。
func PublishFakeVersion(ctx context.Context, a *app.App, slug, version, changelog string) (SeededWorkflow, error) {
	seedMu.Lock()
	defer seedMu.Unlock()
	w, err := a.Workflows.WorkflowBySlug(ctx, slug)
	if err != nil {
		return SeededWorkflow{}, err
	}
	cur, err := a.Workflows.LatestPublished(ctx, a.Fleet.Pool(), w.ID)
	if err != nil {
		return SeededWorkflow{}, err
	}
	if version == "" {
		vs, err := a.Workflows.ListVersions(ctx, w.ID)
		if err != nil {
			return SeededWorkflow{}, err
		}
		version = nextPatch(vs)
	}
	if changelog == "" {
		changelog = "e2e 新版本 " + version
	}
	actor := audit.Actor{}
	v, err := a.Workflows.CreateVersion(ctx, actor, w.ID, comfywf.VersionInput{Version: version, Graph: cur.Graph, APIPrompt: cur.APIPrompt,
		Manifest: cur.Manifest, Changelog: &changelog})
	if err != nil {
		return SeededWorkflow{}, fmt.Errorf("create version %s %s: %w", slug, version, err)
	}
	if v, err = a.Workflows.PublishVersion(ctx, actor, w.ID, v.ID); err != nil {
		return SeededWorkflow{}, fmt.Errorf("publish %s %s: %w", slug, version, err)
	}
	return SeededWorkflow{ID: w.ID, Slug: w.Slug, VersionID: v.ID, Version: v.Version}, nil
}

// ---- 钩子 ----

// serveComfy 让假容器 sshd 把所有实例 127.0.0.1:8188 的 direct-tcpip 转到进程内 fake ComfyUI（首次调用时起 httptest）。
// /hooks/apps/seed 会把 8188 登记到通用的 fake 应用服务，所以它之后要再调一次（appsSeed 已经这样做）。
func (s *Server) serveComfy() error {
	if s.Comfy == nil || s.Upstream == nil {
		return errors.New("fake ComfyUI / upstream not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.comfyTS == nil {
		s.comfyTS = httptest.NewServer(s.Comfy)
	}
	s.Upstream.SetTarget(comfywf.ComfyPort, strings.TrimPrefix(s.comfyTS.URL, "http://"))
	return nil
}

// workflowsSeed：POST /hooks/workflows/seed {} → {workflows: [{id, slug, version_id, version}]}（A、B，幂等）。
func (s *Server) workflowsSeed(ctx context.Context, _ json.RawMessage) (any, error) {
	if err := s.serveComfy(); err != nil {
		return nil, err
	}
	ws, err := SeedWorkflows(ctx, s.App)
	if err != nil {
		return nil, err
	}
	return map[string]any{"workflows": ws}, nil
}

// workflowsVersion：POST /hooks/workflows/version {slug?: 默认 A, version?: 默认 1.0.<n>, changelog?} → {id, slug, version_id, version}。
func (s *Server) workflowsVersion(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Slug      string `json:"slug"`
		Version   string `json:"version"`
		Changelog string `json:"changelog"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Slug == "" {
		req.Slug = WorkflowA
	}
	out, err := PublishFakeVersion(ctx, s.App, req.Slug, req.Version, req.Changelog)
	if errors.Is(err, apperr.ErrNotFound) || errors.Is(err, apperr.ErrWorkflowNotFound) {
		return nil, badRequest("工作流 " + req.Slug + " 不存在（先调 /hooks/workflows/seed）")
	}
	return out, err
}

// comfyHistory：POST /hooks/comfy/history {prompt_id | run_id, state: queued|running|done|error, files?: 0～8（done，默认 1）,
// message?（error，默认 "fake error"）} → {prompt_id, state}。run_id 是快速出图运行的 id（= 提交时的 client_id）。
func (s *Server) comfyHistory(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		PromptID string `json:"prompt_id"`
		RunID    string `json:"run_id"`
		State    string `json:"state"`
		Files    *int   `json:"files"`
		Message  string `json:"message"`
	}](body)
	if err != nil {
		return nil, err
	}
	if s.Comfy == nil {
		return nil, errors.New("fake ComfyUI not configured")
	}
	id := req.PromptID
	if id == "" && req.RunID != "" {
		if id = s.Comfy.PromptIDForClient(req.RunID); id == "" {
			// fake 重置过（/hooks/comfy/reset）：按运行记录找 prompt_id
			_ = s.Pool.QueryRow(ctx, `SELECT prompt_id FROM comfy_workflow_runs WHERE id = $1`, req.RunID).Scan(&id)
		}
	}
	if id == "" {
		return nil, badRequest("需要 prompt_id 或已提交过的 run_id")
	}
	var h comfytest.HistoryState
	switch req.State {
	case comfytest.Queued:
		h = comfytest.QueuedState()
	case comfytest.Running:
		h = comfytest.RunningState()
	case comfytest.Done:
		n := 1
		if req.Files != nil {
			n = *req.Files
		}
		if n < 0 || n > 8 {
			return nil, badRequest("files 必须在 0～8 之间")
		}
		h = comfytest.DoneN(n)
	case comfytest.Error:
		msg := req.Message
		if msg == "" {
			msg = "fake error"
		}
		h = comfytest.ErrorState(msg)
	default:
		return nil, badRequest("state 只能是 queued / running / done / error")
	}
	s.Comfy.SetHistory(id, h)
	return map[string]string{"prompt_id": id, "state": req.State}, nil
}

// comfyPrompts：GET /hooks/comfy/prompts → {prompts: [{id, client_id, prompt}]}（fake ComfyUI 收到的全部 /prompt 提交，按顺序）。
func (s *Server) comfyPrompts(context.Context, json.RawMessage) (any, error) {
	if s.Comfy == nil {
		return nil, errors.New("fake ComfyUI not configured")
	}
	type item struct {
		ID       string          `json:"id"`
		ClientID string          `json:"client_id"`
		Prompt   json.RawMessage `json:"prompt"`
	}
	out := []item{}
	for _, p := range s.Comfy.Prompts() {
		out = append(out, item{ID: p.ID, ClientID: p.ClientID, Prompt: p.Prompt})
	}
	return map[string]any{"prompts": out}, nil
}

// comfyReject：POST /hooks/comfy/reject {message} → 之后的 /prompt 返回 400 node_errors（消息 message）；message 为空恢复正常。
func (s *Server) comfyReject(_ context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Message string `json:"message"`
	}](body)
	if err != nil {
		return nil, err
	}
	if s.Comfy == nil {
		return nil, errors.New("fake ComfyUI not configured")
	}
	s.Comfy.RejectPrompts(req.Message)
	return map[string]bool{"ok": true}, nil
}

// comfyReset：POST /hooks/comfy/reset → 清空 fake ComfyUI 的全部状态（提交、历史、拒绝设置）。
func (s *Server) comfyReset(context.Context, json.RawMessage) (any, error) {
	if s.Comfy == nil {
		return nil, errors.New("fake ComfyUI not configured")
	}
	s.Comfy.Reset()
	return map[string]bool{"ok": true}, nil
}
