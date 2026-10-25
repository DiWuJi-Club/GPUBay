package comfywf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// workflowsDir 是仓库里随平台发布的首批工作流目录（platform admin workflow import deploy/workflows 导入，规格 §9）。
const workflowsDir = "../../deploy/workflows"

// initialWorkflows 规格 §9 / D12 的六个首批工作流。
var initialWorkflows = []string{
	"flux-schnell-txt2img", "qwen-image-txt2img", "sdxl-img2img", "sdxl-txt2img", "sdxl-ultimate-upscale", "wan22-5b-ti2v",
}

// importWorkflowJSON 与导入器（ImportDir）读的 workflow.json 字段一致；未知字段报错（拼写错误不会被导入静默忽略）。
type importWorkflowJSON struct {
	Name        string   `json:"name"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
	Sort        int      `json:"sort"`
	Version     string   `json:"version"`
	Changelog   string   `json:"changelog"`
	ComfyUIMin  string   `json:"comfyui_min"`
}

// 目录里允许出现的文件（cover 至多一个）。
var workflowDirFiles = []string{"workflow.json", "graph.json", "api.json", "manifest.json"}
var coverFiles = map[string]string{"cover.png": "image/png", "cover.jpg": "image/jpeg", "cover.jpeg": "image/jpeg", "cover.webp": "image/webp"}

// 只存在于 UI 格式 graph 里的前端虚拟节点（API 格式里没有）。
var frontendOnlyNodes = map[string]bool{"Note": true, "MarkdownNote": true, "Reroute": true, "PrimitiveNode": true}

func TestWorkflowsDir(t *testing.T) {
	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}
	var slugs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			slugs = append(slugs, e.Name())
		}
	}
	for _, want := range initialWorkflows {
		if !slices.Contains(slugs, want) {
			t.Errorf("deploy/workflows/%s 不存在", want)
		}
	}
	if len(slugs) > MaxWorkflowsPerDeployment {
		// 下面的 BundleFiles 一次装全部目录；超过 8 个时改成分批断言。
		t.Fatalf("deploy/workflows 有 %d 个目录，超过单部署上限 %d", len(slugs), MaxWorkflowsPerDeployment)
	}
	var bundle []BundleWorkflow
	for _, slug := range slugs {
		t.Run(slug, func(t *testing.T) {
			if bw, ok := checkWorkflowDir(t, slug, filepath.Join(workflowsDir, slug)); ok {
				bundle = append(bundle, bw)
			}
		})
	}
	// 六个工作流装在同一个 ComfyUI 上：模型按 <dir>/<name> 去重、同名节点的 Git 地址与 commit 一致。
	t.Run("bundle", func(t *testing.T) {
		if len(bundle) != len(slugs) {
			t.Skip("有目录未通过校验")
		}
		files, err := BundleFiles(bundle)
		if err != nil {
			t.Fatalf("BundleFiles: %v", err)
		}
		if len(files) != len(bundle)+1 || files[0].Name != BundleManifestName {
			t.Fatalf("files = %d, [0] = %s", len(files), files[0].Name)
		}
		var bm bundleManifest
		if err := json.Unmarshal(files[0].Body, &bm); err != nil || len(bm.Workflows) != len(bundle) {
			t.Fatalf("manifest.json: %v, %d workflows", err, len(bm.Workflows))
		}
		// 同一目标位置的模型在各工作流里必须是同一份（sha256 / 大小一致），否则去重会让某个工作流拿到错的文件。
		seen := map[string]ModelSpec{}
		for _, w := range bundle {
			for _, md := range w.Manifest.Models {
				key := md.Dir + "/" + md.Name
				if prev, ok := seen[key]; ok && (prev.SHA256 != md.SHA256 || prev.SizeGB != md.SizeGB) {
					t.Errorf("%s：模型 %s 与其它工作流的同名模型 sha256 / 大小不同", w.Slug, key)
				}
				seen[key] = md
			}
		}
	})
}

func checkWorkflowDir(t *testing.T, slug, dir string) (BundleWorkflow, bool) {
	if !ValidSlug(slug) {
		t.Errorf("目录名 %q 不是合法 slug", slug)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cover := ""
	for _, e := range entries {
		name := e.Name()
		switch {
		case slices.Contains(workflowDirFiles, name):
		case coverFiles[name] != "":
			if cover != "" {
				t.Errorf("封面有两个：%s、%s", cover, name)
			}
			cover = name
		default:
			t.Errorf("多余的文件 %s（导入只认 %s 与 cover.png|jpg|jpeg|webp）", name, strings.Join(workflowDirFiles, " "))
		}
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return b
	}
	// D12：首批工作流都支持快速出图，四份文件都必需。
	wfRaw, graph, api, manRaw := read("workflow.json"), read("graph.json"), read("api.json"), read("manifest.json")

	var wf importWorkflowJSON
	if err := strictDecode(wfRaw, &wf); err != nil {
		t.Fatalf("workflow.json: %v", err)
	}
	checkWorkflowJSON(t, wf)

	var m Manifest
	if err := strictDecode(manRaw, &m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	// 导入器把 workflow.json 的 comfyui_min 合并进 manifest。
	if m.ComfyUIMin != "" && wf.ComfyUIMin != "" && m.ComfyUIMin != wf.ComfyUIMin {
		t.Errorf("comfyui_min 在 workflow.json（%s）与 manifest.json（%s）里不一致", wf.ComfyUIMin, m.ComfyUIMin)
	}
	if m.ComfyUIMin == "" {
		m.ComfyUIMin = wf.ComfyUIMin
	}
	if err := ValidateManifest(&m, graph, api); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
	if len(m.Inputs) == 0 {
		t.Error("manifest.inputs 为空（首批工作流都支持快速出图）")
	}
	if len(m.Outputs) == 0 {
		t.Error("manifest.outputs 为空")
	}
	if DownloadGB(m) <= 0 {
		t.Error("download_gb 应 > 0")
	}
	for i, md := range m.Models {
		// 仓库约定：首批工作流的模型都钉 sha256、两个来源都给（容器内按 MODEL_SOURCE 选、另一个兜底）。
		if md.SHA256 == "" {
			t.Errorf("models[%d] %s 缺 sha256", i, md.Name)
		}
		if md.ModelScope == nil || md.HF == nil {
			t.Errorf("models[%d] %s 应同时给 modelscope 与 hf 来源", i, md.Name)
		}
	}

	g := parseUIGraph(t, graph)
	if len(g.Nodes) < 3 {
		t.Errorf("graph.nodes 只有 %d 个", len(g.Nodes))
	}
	checkGraphLinks(t, g)
	apiNodes, err := parseAPIPrompt(api)
	if err != nil {
		t.Fatalf("api.json: %v", err)
	}
	checkAPIMatchesGraph(t, g, apiNodes)
	checkWidgetsMatchAPI(t, g, apiNodes)
	checkInputDefaults(t, m, apiNodes)

	// 只填图片、其余全用默认值也能生成 prompt（快速出图表单的「直接点生成」）。
	values := map[string]any{}
	for _, in := range m.Inputs {
		if in.Type == InputImage {
			values[in.Key] = "spark-quickrun/abcdef123456.png"
		}
	}
	prompt, _, err := BuildPrompt(api, m.Inputs, values, func() int64 { return 42 })
	if err != nil {
		t.Errorf("BuildPrompt(默认值): %v", err)
	} else if _, err := parseAPIPrompt(prompt); err != nil {
		t.Errorf("BuildPrompt 结果: %v", err)
	}

	if cover != "" {
		b := read(cover)
		if len(b) > MaxCoverBytes {
			t.Errorf("%s %d 字节，超过 %d", cover, len(b), MaxCoverBytes)
		}
		if ct := http.DetectContentType(b); ct != coverFiles[cover] {
			t.Errorf("%s 的实际类型是 %s", cover, ct)
		}
	}
	return BundleWorkflow{Slug: slug, Version: wf.Version, Graph: graph, Manifest: m}, !t.Failed()
}

func strictDecode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

// checkWorkflowJSON 规格 §2 的字段约束（name 1～48、summary ≤ 120、description ≤ 8000、分类、标签 ≤ 8 × 16、changelog ≤ 1000）。
func checkWorkflowJSON(t *testing.T, wf importWorkflowJSON) {
	t.Helper()
	runes := utf8.RuneCountInString
	if n := runes(strings.TrimSpace(wf.Name)); n < 1 || n > 48 {
		t.Errorf("name 长度 %d", n)
	}
	if n := runes(wf.Summary); n == 0 || n > 120 {
		t.Errorf("summary 长度 %d（应 1～120）", n)
	}
	if n := runes(wf.Description); n == 0 || n > 8000 {
		t.Errorf("description 长度 %d（应 1～8000）", n)
	}
	if !ValidCategory(wf.Category) {
		t.Errorf("category %q 不在 %v", wf.Category, Categories)
	}
	if len(wf.Tags) > 8 {
		t.Errorf("tags %d 项（≤ 8）", len(wf.Tags))
	}
	seen := map[string]bool{}
	for _, tag := range wf.Tags {
		if n := runes(tag); n < 1 || n > 16 || seen[tag] || strings.TrimSpace(tag) != tag {
			t.Errorf("tag %q 不合法（1～16 字符、不重复、无首尾空白）", tag)
		}
		seen[tag] = true
	}
	if !ValidVersion(wf.Version) {
		t.Errorf("version %q 不合法", wf.Version)
	}
	if n := runes(wf.Changelog); n > 1000 {
		t.Errorf("changelog 长度 %d", n)
	}
	if wf.ComfyUIMin == "" || !comfyMinRe.MatchString(wf.ComfyUIMin) {
		t.Errorf("comfyui_min %q", wf.ComfyUIMin)
	}
}

// ---- UI 格式 graph 的结构 ----

type uiInput struct {
	Name string `json:"name"`
	Link *int64 `json:"link"`
}

type uiOutput struct {
	Links []int64 `json:"links"`
}

type uiNode struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	Mode    int             `json:"mode"`
	Inputs  []uiInput       `json:"inputs"`
	Outputs []uiOutput      `json:"outputs"`
	Widgets json.RawMessage `json:"widgets_values"`
}

type uiLink struct {
	ID, From, FromSlot, To, ToSlot int64
}

type uiGraph struct {
	Nodes []uiNode
	Links []uiLink
	byID  map[int64]*uiNode
	link  map[int64]uiLink
}

func parseUIGraph(t *testing.T, raw []byte) *uiGraph {
	t.Helper()
	var g struct {
		Nodes []uiNode          `json:"nodes"`
		Links []json.RawMessage `json:"links"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("graph.json: %v", err)
	}
	out := &uiGraph{Nodes: g.Nodes, byID: map[int64]*uiNode{}, link: map[int64]uiLink{}}
	for i := range out.Nodes {
		n := &out.Nodes[i]
		if out.byID[n.ID] != nil {
			t.Errorf("graph 节点 id %d 重复", n.ID)
		}
		out.byID[n.ID] = n
	}
	for _, lr := range g.Links {
		// [link_id, from_node, from_slot, to_node, to_slot, type]
		var arr []any
		if err := json.Unmarshal(lr, &arr); err != nil || len(arr) != 6 {
			t.Fatalf("graph.links 项 %s 应为 6 元数组", lr)
		}
		var nums [5]int64
		for k := range 5 {
			f, ok := arr[k].(float64)
			if !ok {
				t.Fatalf("graph.links 项 %s 第 %d 位不是数字", lr, k)
			}
			nums[k] = int64(f)
		}
		l := uiLink{nums[0], nums[1], nums[2], nums[3], nums[4]}
		if _, dup := out.link[l.ID]; dup {
			t.Errorf("graph link id %d 重复", l.ID)
		}
		out.Links = append(out.Links, l)
		out.link[l.ID] = l
	}
	return out
}

// checkGraphLinks links 与节点上的 inputs[].link / outputs[].links 互相对得上（ComfyUI 前端加载时依赖两边一致）。
func checkGraphLinks(t *testing.T, g *uiGraph) {
	t.Helper()
	for _, l := range g.Links {
		from, to := g.byID[l.From], g.byID[l.To]
		if from == nil || to == nil {
			t.Errorf("link %d 的端点节点不存在", l.ID)
			continue
		}
		if l.ToSlot >= int64(len(to.Inputs)) || to.Inputs[l.ToSlot].Link == nil || *to.Inputs[l.ToSlot].Link != l.ID {
			t.Errorf("link %d → 节点 %d 输入 %d 不一致", l.ID, l.To, l.ToSlot)
		}
		if l.FromSlot >= int64(len(from.Outputs)) || !slices.Contains(from.Outputs[l.FromSlot].Links, l.ID) {
			t.Errorf("link %d ← 节点 %d 输出 %d 不一致", l.ID, l.From, l.FromSlot)
		}
	}
	for _, n := range g.Nodes {
		for i, in := range n.Inputs {
			if in.Link == nil {
				continue
			}
			if l, ok := g.link[*in.Link]; !ok || l.To != n.ID || l.ToSlot != int64(i) {
				t.Errorf("节点 %d 输入 %s 的 link %d 不在 links 里或端点不符", n.ID, in.Name, *in.Link)
			}
		}
		for i, out := range n.Outputs {
			for _, id := range out.Links {
				if l, ok := g.link[id]; !ok || l.From != n.ID || l.FromSlot != int64(i) {
					t.Errorf("节点 %d 输出 %d 的 link %d 不在 links 里或端点不符", n.ID, i, id)
				}
			}
		}
	}
}

// widgetNames UI 格式 widgets_values 的顺序（= ComfyUI v0.3.60 INPUT_TYPES 里 widget 输入的顺序，连线输入不占位）。
// "@ctl" 是前端给 seed 加的 control_after_generate 小部件，"@upload" 是 LoadImage 的上传按钮：两者只在 UI 格式里。
// 新工作流用到表外的节点类型时在这里补上（顺序以 ComfyUI 源码的 INPUT_TYPES 为准）。
var widgetNames = map[string][]string{
	"KSampler":                {"seed", "@ctl", "steps", "cfg", "sampler_name", "scheduler", "denoise"},
	"CheckpointLoaderSimple":  {"ckpt_name"},
	"EmptyLatentImage":        {"width", "height", "batch_size"},
	"EmptySD3LatentImage":     {"width", "height", "batch_size"},
	"CLIPTextEncode":          {"text"},
	"VAEDecode":               {},
	"VAEEncode":               {},
	"SaveImage":               {"filename_prefix"},
	"LoadImage":               {"image", "@upload"},
	"ImageScaleToTotalPixels": {"upscale_method", "megapixels"},
	"UNETLoader":              {"unet_name", "weight_dtype"},
	"CLIPLoader":              {"clip_name", "type", "device"},
	"VAELoader":               {"vae_name"},
	"ModelSamplingAuraFlow":   {"shift"},
	"ModelSamplingSD3":        {"shift"},
	"Wan22ImageToVideoLatent": {"width", "height", "length", "batch_size"},
	"CreateVideo":             {"fps"},
	"SaveVideo":               {"filename_prefix", "format", "codec"},
	"UpscaleModelLoader":      {"model_name"},
	"UltimateSDUpscale": {"upscale_by", "seed", "@ctl", "steps", "cfg", "sampler_name", "scheduler", "denoise", "mode_type",
		"tile_width", "tile_height", "mask_blur", "tile_padding", "seam_fix_mode", "seam_fix_denoise", "seam_fix_width",
		"seam_fix_mask_blur", "seam_fix_padding", "force_uniform_tiles", "tiled_decode", "batch_size"},
}

// checkWidgetsMatchAPI 启用节点的 widgets_values 与 api.json 的同名输入值一一相等（ComfyUI 界面里打开运行与快速出图跑的是同一组参数），
// 且 api 里没有 widgets 之外的非连线输入。
func checkWidgetsMatchAPI(t *testing.T, g *uiGraph, api map[string]apiNode) {
	t.Helper()
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if !activeNode(n) {
			continue
		}
		id := strconv.FormatInt(n.ID, 10)
		an, ok := api[id]
		if !ok {
			continue // checkAPIMatchesGraph 已报
		}
		names, ok := widgetNames[n.Type]
		if !ok {
			t.Errorf("节点 %s 的类型 %s 不在 widgetNames 表里，请补上它的 widget 顺序", id, n.Type)
			continue
		}
		var wv []any
		if t0 := bytes.TrimSpace(n.Widgets); len(t0) > 0 && string(t0) != "null" {
			if err := decodeNumber(t0, &wv); err != nil {
				t.Errorf("节点 %s widgets_values 应为数组：%v", id, err)
				continue
			}
		}
		if len(wv) != len(names) {
			t.Errorf("节点 %s（%s）widgets_values 有 %d 项，应为 %d 项 %v", id, n.Type, len(wv), len(names), names)
			continue
		}
		widgetInputs := map[string]bool{}
		for k, name := range names {
			switch name {
			case "@ctl":
				if s, _ := wv[k].(string); !slices.Contains([]string{"fixed", "increment", "decrement", "randomize"}, s) {
					t.Errorf("节点 %s 的 control_after_generate = %v", id, wv[k])
				}
				continue
			case "@upload":
				if wv[k] != "image" {
					t.Errorf("节点 %s 的上传小部件 = %v", id, wv[k])
				}
				continue
			}
			widgetInputs[name] = true
			got, ok := an.Inputs[name]
			if !ok {
				t.Errorf("api 节点 %s 缺输入 %s（graph 里是 %v）", id, name, wv[k])
			} else if !sameJSONValue(wv[k], got) {
				t.Errorf("节点 %s.%s：graph %v ≠ api %v", id, name, wv[k], got)
			}
		}
		for name, v := range an.Inputs {
			if _, isLink := v.([]any); !isLink && !widgetInputs[name] {
				t.Errorf("api 节点 %s 的输入 %s 在 graph 的 widgets_values 里没有对应项", id, name)
			}
		}
	}
}

// sameJSONValue 数字按数值比较（1 与 1.0 相等），其余按值比较。
func sameJSONValue(a, b any) bool {
	if fa, ok := toFloat(a); ok {
		fb, ok := toFloat(b)
		return ok && fa == fb
	}
	return a == b
}

func activeNode(n *uiNode) bool {
	return n != nil && n.Mode != 2 && n.Mode != 4 && !frontendOnlyNodes[n.Type]
}

// checkAPIMatchesGraph api.json 是 graph.json 的「导出 (API)」：节点集合（去掉静音 / 旁路 / 前端虚拟节点）与类型相同，
// 连线一一对应（[源节点 id, 输出序号]）。
func checkAPIMatchesGraph(t *testing.T, g *uiGraph, api map[string]apiNode) {
	t.Helper()
	active := map[string]*uiNode{}
	for i := range g.Nodes {
		if n := &g.Nodes[i]; activeNode(n) {
			active[strconv.FormatInt(n.ID, 10)] = n
		}
	}
	for id, an := range api {
		gn := active[id]
		if gn == nil {
			t.Errorf("api 节点 %s（%s）在 graph 里不存在或未启用", id, an.ClassType)
			continue
		}
		if gn.Type != an.ClassType {
			t.Errorf("api 节点 %s 类型 %s，graph 里是 %s", id, an.ClassType, gn.Type)
		}
	}
	for id, gn := range active {
		an, ok := api[id]
		if !ok {
			t.Errorf("graph 节点 %s（%s）不在 api 里", id, gn.Type)
			continue
		}
		linked := 0
		for _, in := range gn.Inputs {
			if in.Link == nil {
				continue
			}
			l := g.link[*in.Link]
			if !activeNode(g.byID[l.From]) {
				continue // 来自旁路 / 虚拟节点的连线不进 API（如 Wan2.2 旁路的 LoadImage）
			}
			linked++
			want := fmt.Sprintf(`["%d",%d]`, l.From, l.FromSlot)
			got, _ := json.Marshal(an.Inputs[in.Name])
			if string(got) != want {
				t.Errorf("api 节点 %s 输入 %s = %s，graph 连线是 %s", id, in.Name, got, want)
			}
		}
		apiLinks := 0
		for name, v := range an.Inputs {
			if arr, isLink := v.([]any); isLink {
				apiLinks++
				if len(arr) != 2 {
					t.Errorf("api 节点 %s 输入 %s 连线格式应为 [id, slot]", id, name)
					continue
				}
				if src, _ := arr[0].(string); api[src].ClassType == "" {
					t.Errorf("api 节点 %s 输入 %s 连到不存在的节点 %v", id, name, arr[0])
				}
			}
		}
		if apiLinks != linked {
			t.Errorf("api 节点 %s 有 %d 条连线，graph 里有 %d 条", id, apiLinks, linked)
		}
	}
}

// checkInputDefaults 参数默认值与 api.json 的现值一致（ComfyUI 界面里打开与快速出图默认出同样的图）；种子与图片没有默认值。
func checkInputDefaults(t *testing.T, m Manifest, api map[string]apiNode) {
	t.Helper()
	for _, in := range m.Inputs {
		cur := api[in.Node].Inputs[in.Input]
		switch in.Type {
		case InputSeed, InputImage:
			if in.Default != nil {
				t.Errorf("参数 %s 不应设默认值（种子默认随机、图片必须上传）", in.Key)
			}
			continue
		}
		if in.Default == nil {
			t.Errorf("参数 %s 缺默认值", in.Key)
			continue
		}
		if a, ok := toFloat(in.Default); ok {
			if b, ok := toFloat(cur); !ok || a != b {
				t.Errorf("参数 %s 默认值 %v ≠ api 现值 %v", in.Key, in.Default, cur)
			}
		} else if in.Default != cur {
			t.Errorf("参数 %s 默认值 %v ≠ api 现值 %v", in.Key, in.Default, cur)
		}
	}
}
