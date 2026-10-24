package comfywf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"spark-cloud/internal/platform/apperr"
)

// 规格 §3.2 的正则（前端 web/console/src/logic/workflows.ts 复制同样的正则做即时校验，以这里为准）。
var (
	slugRe       = regexp.MustCompile(`^[a-z][a-z0-9-]{1,47}$`)
	versionRe    = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.-]{0,31}$`)
	comfyMinRe   = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	modelNameRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	repoRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	fileSegRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	sha256Re     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	nodeNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	gitPathRe    = regexp.MustCompile(`^/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(\.git)?$`)
	commitRe     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	nodeTypeRe   = regexp.MustCompile(`^[A-Za-z0-9_.: -]{1,64}$`)
	inputKeyRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	quickImageRe = regexp.MustCompile(`^spark-quickrun/[a-z0-9]{12}\.(png|jpg|webp)$`)
	// subgraphIDRe 前端子图定义的 id 是 UUID；只有这种形状的 type 才当作子图实例。
	subgraphIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

const (
	maxModelFile  = 256
	maxRepoLen    = 128
	maxLabelRunes = 32
	defaultMaxLen = 1000
	maxMaxLen     = 4000
	maxOptions    = 32
	maxOptionLen  = 64
	maxNodeTypes  = 64
	maxImageInput = 2
	// maxSeed 种子上限（不含）：2^53，浏览器 JSON 数字也能精确表示。
	maxSeed = int64(1) << 53
)

// ValidSlug 工作流 slug（容器内文件名 spark-<slug>.json，创建后不可改）。
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

// ValidVersion 工作流版本号。
func ValidVersion(s string) bool { return versionRe.MatchString(s) }

// modelLoaderWidgets D16：UI 格式 graph 里模型加载节点的 widgets_values 中模型文件名所在位置。
var modelLoaderWidgets = map[string][]int{
	"CheckpointLoaderSimple": {0}, "UNETLoader": {0}, "VAELoader": {0}, "CLIPLoader": {0}, "DualCLIPLoader": {0, 1},
	"TripleCLIPLoader": {0, 1, 2}, "LoraLoader": {0}, "LoraLoaderModelOnly": {0}, "UpscaleModelLoader": {0},
	"ControlNetLoader": {0}, "CLIPVisionLoader": {0},
}

// modelLoaderInputs 同一批节点在 API 格式 api_prompt 里的模型文件名入参（快速出图提交的就是它，一并校验）。
var modelLoaderInputs = map[string][]string{
	"CheckpointLoaderSimple": {"ckpt_name"}, "UNETLoader": {"unet_name"}, "VAELoader": {"vae_name"}, "CLIPLoader": {"clip_name"},
	"DualCLIPLoader": {"clip_name1", "clip_name2"}, "TripleCLIPLoader": {"clip_name1", "clip_name2", "clip_name3"},
	"LoraLoader": {"lora_name"}, "LoraLoaderModelOnly": {"lora_name"}, "UpscaleModelLoader": {"model_name"},
	"ControlNetLoader": {"control_net_name"}, "CLIPVisionLoader": {"clip_name"},
}

// modelLoaderDirs 各模型加载节点读取的 models/<dir>（manifest.models[].dir 须与之相符，否则装好了 ComfyUI 也找不到）。
var modelLoaderDirs = map[string][]string{
	"CheckpointLoaderSimple": {"checkpoints"}, "UNETLoader": {"diffusion_models", "unet"}, "VAELoader": {"vae"},
	"CLIPLoader": {"text_encoders", "clip"}, "DualCLIPLoader": {"text_encoders", "clip"}, "TripleCLIPLoader": {"text_encoders", "clip"},
	"LoraLoader": {"loras"}, "LoraLoaderModelOnly": {"loras"}, "UpscaleModelLoader": {"upscale_models"},
	"ControlNetLoader": {"controlnet"}, "CLIPVisionLoader": {"clip_vision"},
}

// modelSet 已声明的模型（文件名 → 下标）。
type modelSet struct {
	idx   map[string]int
	specs []ModelSpec
}

// ref 校验 loader 节点引用的模型文件 name：须已声明（错误指向 path），目录须与 loader 相符（错误指向该模型的 dir）。
func (ms modelSet) ref(path, who, loader, name string) error {
	i, ok := ms.idx[name]
	if !ok {
		return invalid(path, who+" 引用的模型 "+name+" 不在 manifest.models 里")
	}
	if dirs := modelLoaderDirs[loader]; len(dirs) > 0 && !slices.Contains(dirs, ms.specs[i].Dir) {
		return invalid(fmt.Sprintf("manifest.models[%d].dir", i),
			fmt.Sprintf("模型 %s 的目录应为 %s（%s 的 %s 引用了它）", name, strings.Join(dirs, " 或 "), path, loader))
	}
	return nil
}

// isLoaderInput api_prompt 里 class_type 的 input 是不是模型文件名入参。
func isLoaderInput(classType, input string) bool {
	return slices.Contains(modelLoaderInputs[classType], input)
}

// outputClassTypes manifest.outputs[].node 允许的 class_type（规格 §3.2）。
var outputClassTypes = map[string]bool{"SaveImage": true, "SaveAnimatedWEBP": true, "SaveWEBM": true, "VHS_VideoCombine": true, "SaveVideo": true}

func invalid(path, reason string) error { return apperr.WorkflowVersionInvalid(path, reason) }

// apiNode 是 api_prompt 的一项（API 格式：{"<node_id>": {"class_type": …, "inputs": {…}}}）。
type apiNode struct {
	ClassType string
	Inputs    map[string]any
}

// ValidateManifest 规范化 m（去空白、size_gb 两位小数、dir / sha256 / commit 小写、text 的 max_len 默认 1000）并按规格 §3.2
// 校验 manifest、graph（UI 格式）与 api_prompt（API 格式，nil / "null" = 不支持快速出图），含 D16 的交叉校验。
// 错误为 apperr.WorkflowVersionInvalid(path, reason)。
func ValidateManifest(m *Manifest, graph, apiPrompt json.RawMessage) error {
	normalize(m)
	if m.ComfyUIMin != "" && !comfyMinRe.MatchString(m.ComfyUIMin) {
		return invalid("manifest.comfyui_min", "格式应为 0.3.60")
	}
	models, err := validateModels(m.Models)
	if err != nil {
		return err
	}
	customTypes, err := validateNodes(m.Nodes)
	if err != nil {
		return err
	}
	api, err := parseAPIPrompt(apiPrompt)
	if err != nil {
		return err
	}
	if err := validateGraph(graph, models, customTypes); err != nil {
		return err
	}
	if err := checkAPIRefs(api, models, customTypes); err != nil {
		return err
	}
	if api == nil {
		if len(m.Inputs) > 0 {
			return invalid("manifest.inputs", "没有 api_prompt 时不能定义快速出图参数")
		}
		if len(m.Outputs) > 0 {
			return invalid("manifest.outputs", "没有 api_prompt 时不能定义输出节点")
		}
		return nil
	}
	if err := validateInputs(m.Inputs, api, models); err != nil {
		return err
	}
	return validateOutputs(m.Outputs, api)
}

func normalize(m *Manifest) {
	m.ComfyUIMin = strings.TrimSpace(m.ComfyUIMin)
	for i := range m.Models {
		md := &m.Models[i]
		md.Name = strings.TrimSpace(md.Name)
		md.Dir = strings.ToLower(strings.TrimSpace(md.Dir))
		md.SizeGB = round2(md.SizeGB)
		md.SHA256 = strings.ToLower(strings.TrimSpace(md.SHA256))
		for _, rf := range []*RepoFile{md.ModelScope, md.HF} {
			if rf != nil {
				rf.Repo, rf.File = strings.TrimSpace(rf.Repo), strings.TrimSpace(rf.File)
			}
		}
	}
	for i := range m.Nodes {
		n := &m.Nodes[i]
		n.Name, n.Git = strings.TrimSpace(n.Name), strings.TrimSpace(n.Git)
		n.Commit = strings.ToLower(strings.TrimSpace(n.Commit))
		for j := range n.NodeTypes {
			n.NodeTypes[j] = strings.TrimSpace(n.NodeTypes[j])
		}
	}
	for i := range m.Inputs {
		in := &m.Inputs[i]
		in.Key, in.Label, in.Type = strings.TrimSpace(in.Key), strings.TrimSpace(in.Label), strings.TrimSpace(in.Type)
		in.Node, in.Input = strings.TrimSpace(in.Node), strings.TrimSpace(in.Input)
		for j := range in.Options {
			in.Options[j] = strings.TrimSpace(in.Options[j])
		}
		// UseNumber 解出的 json.Number 统一成 float64（与 encoding/json 默认一致；manifest_hash 不随解码方式变）。
		if n, ok := in.Default.(json.Number); ok {
			if f, err := n.Float64(); err == nil {
				in.Default = f
			}
		}
		if (in.Type == InputText || in.Type == InputTextarea) && in.MaxLen == 0 {
			in.MaxLen = defaultMaxLen
		}
	}
	for i := range m.Outputs {
		m.Outputs[i].Node = strings.TrimSpace(m.Outputs[i].Node)
	}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// validateModels 返回声明的模型集合。
func validateModels(models []ModelSpec) (modelSet, error) {
	ms, err := validateModelList(models)
	return modelSet{idx: ms, specs: models}, err
}

func validateModelList(models []ModelSpec) (map[string]int, error) {
	if len(models) > MaxModelsPerVersion {
		return nil, invalid("manifest.models", fmt.Sprintf("最多 %d 个模型", MaxModelsPerVersion))
	}
	names := map[string]int{}
	for i, md := range models {
		p := fmt.Sprintf("manifest.models[%d]", i)
		if !modelNameRe.MatchString(md.Name) {
			return nil, invalid(p+".name", "文件名只能含字母、数字、. _ -，不含 /，≤ 128 字符")
		}
		if _, dup := names[md.Name]; dup {
			return nil, invalid(p+".name", "模型 "+md.Name+" 重复")
		}
		names[md.Name] = i
		if !ValidModelDir(md.Dir) {
			return nil, invalid(p+".dir", "目录只能是 "+strings.Join(ModelDirs, " / "))
		}
		if md.SizeGB < 0.01 || md.SizeGB > 200 {
			return nil, invalid(p+".size_gb", "大小应在 0.01～200 GB")
		}
		if md.ModelScope == nil && md.HF == nil {
			return nil, invalid(p, "modelscope 与 hf 至少给一个")
		}
		for _, src := range []struct {
			key string
			rf  *RepoFile
		}{{"modelscope", md.ModelScope}, {"hf", md.HF}} {
			if src.rf == nil {
				continue
			}
			if len(src.rf.Repo) > maxRepoLen || !repoRe.MatchString(src.rf.Repo) {
				return nil, invalid(p+"."+src.key+".repo", fmt.Sprintf("仓库应形如 owner/name，≤ %d 字符", maxRepoLen))
			}
			if !validRepoFile(src.rf.File) {
				return nil, invalid(p+"."+src.key+".file", "应为仓库内相对路径（每段以字母或数字开头，不含 ..，≤ 256 字符）")
			}
		}
		if md.SHA256 != "" && !sha256Re.MatchString(md.SHA256) {
			return nil, invalid(p+".sha256", "应为空或 64 位十六进制")
		}
	}
	return names, nil
}

func validRepoFile(f string) bool {
	if f == "" || len(f) > maxModelFile || strings.Contains(f, "..") {
		return false
	}
	for _, seg := range strings.Split(f, "/") {
		if !fileSegRe.MatchString(seg) {
			return false
		}
	}
	return true
}

// validateNodes 返回所有声明的 node_types。
func validateNodes(nodes []NodeSpec) (map[string]bool, error) {
	if len(nodes) > MaxNodesPerVersion {
		return nil, invalid("manifest.nodes", fmt.Sprintf("最多 %d 个自定义节点", MaxNodesPerVersion))
	}
	names, types := map[string]bool{}, map[string]bool{}
	for i, n := range nodes {
		p := fmt.Sprintf("manifest.nodes[%d]", i)
		if !nodeNameRe.MatchString(n.Name) {
			return nil, invalid(p+".name", "目录名只能含字母、数字、. _ -，≤ 64 字符")
		}
		if names[n.Name] {
			return nil, invalid(p+".name", "节点 "+n.Name+" 重复")
		}
		names[n.Name] = true
		if reason := checkGitURL(n.Git); reason != "" {
			return nil, invalid(p+".git", reason)
		}
		if !commitRe.MatchString(n.Commit) {
			return nil, invalid(p+".commit", "必须是 40 位十六进制 commit")
		}
		if len(n.NodeTypes) < 1 || len(n.NodeTypes) > maxNodeTypes {
			return nil, invalid(p+".node_types", fmt.Sprintf("应有 1～%d 项", maxNodeTypes))
		}
		for j, t := range n.NodeTypes {
			if !nodeTypeRe.MatchString(t) {
				return nil, invalid(fmt.Sprintf("%s.node_types[%d]", p, j), "节点类型只能含字母、数字、_ . : - 与空格，≤ 64 字符")
			}
			types[t] = true
		}
	}
	return types, nil
}

func checkGitURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || !strings.HasPrefix(s, "https://") || u.Scheme != "https" || u.Opaque != "" {
		return "必须是 https:// 地址"
	}
	if u.User != nil || u.Port() != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(s, "#") {
		return "不能带用户名、端口、查询参数或片段"
	}
	if !ValidGitHost(u.Host) {
		return "Git 主机只能是 " + strings.Join(GitHosts, " / ")
	}
	if u.RawPath != "" || !gitPathRe.MatchString(u.Path) {
		return "路径应形如 /owner/repo(.git)"
	}
	for _, seg := range strings.Split(strings.TrimPrefix(u.Path, "/"), "/") {
		if strings.HasPrefix(seg, ".") {
			return "路径的每一段不能以 . 开头"
		}
	}
	return ""
}

// parseAPIPrompt 校验 api_prompt 形状；nil / 空 / "null" → (nil, nil)。
func parseAPIPrompt(raw json.RawMessage) (map[string]apiNode, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	if len(raw) > MaxAPIPromptBytes {
		return nil, invalid("api_prompt", fmt.Sprintf("不能超过 %d KB", MaxAPIPromptBytes/1024))
	}
	var top map[string]json.RawMessage
	if err := decodeNumber(trimmed, &top); err != nil || top == nil {
		return nil, invalid("api_prompt", "应为 API 格式的 JSON 对象（ComfyUI「导出 (API)」）")
	}
	if len(top) == 0 {
		return nil, invalid("api_prompt", "至少要有一个节点")
	}
	out := make(map[string]apiNode, len(top))
	for _, id := range sortedKeys(top) {
		p := "api_prompt." + id
		var n map[string]json.RawMessage
		if err := decodeNumber(top[id], &n); err != nil || n == nil {
			return nil, invalid(p, "节点应为 {class_type, inputs} 对象")
		}
		var ct string
		if err := json.Unmarshal(n["class_type"], &ct); err != nil || strings.TrimSpace(ct) == "" {
			return nil, invalid(p+".class_type", "缺少 class_type")
		}
		var inputs map[string]any
		if err := decodeNumber(n["inputs"], &inputs); err != nil || inputs == nil {
			return nil, invalid(p+".inputs", "inputs 应为对象")
		}
		out[id] = apiNode{ClassType: ct, Inputs: inputs}
	}
	return out, nil
}

// checkAPIRefs D16 在 api_prompt 上的同构校验：模型加载入参须已声明、非内置 class_type 须在 node_types 里。
func checkAPIRefs(api map[string]apiNode, models modelSet, customTypes map[string]bool) error {
	for _, id := range sortedKeys(api) {
		n := api[id]
		if !builtinNodeTypes[n.ClassType] && !customTypes[n.ClassType] {
			return invalid("api_prompt."+id+".class_type", "节点类型 "+n.ClassType+" 不是内置节点，须在 manifest.nodes[].node_types 里声明")
		}
		for _, in := range modelLoaderInputs[n.ClassType] {
			if s, ok := n.Inputs[in].(string); ok && s != "" {
				if err := models.ref("api_prompt."+id+".inputs."+in, n.ClassType, n.ClassType, s); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateGraph 校验 UI 格式 graph 的形状与 D16 交叉校验（含 definitions.subgraphs 里的子图节点）。
func validateGraph(raw json.RawMessage, models modelSet, customTypes map[string]bool) error {
	if len(raw) > MaxGraphBytes {
		return invalid("graph", fmt.Sprintf("不能超过 %d KB", MaxGraphBytes/1024))
	}
	var g struct {
		Nodes       json.RawMessage `json:"nodes"`
		Links       json.RawMessage `json:"links"`
		Definitions *struct {
			Subgraphs []struct {
				ID    string          `json:"id"`
				Nodes json.RawMessage `json:"nodes"`
			} `json:"subgraphs"`
		} `json:"definitions"`
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, &g) != nil {
		return invalid("graph", "应为 ComfyUI 保存的工作流 JSON 对象（UI 格式）")
	}
	if !isJSONArray(g.Nodes) {
		return invalid("graph.nodes", "缺少 nodes 数组")
	}
	if !isJSONArray(g.Links) {
		return invalid("graph.links", "缺少 links 数组")
	}
	subgraphs := map[string]bool{}
	if g.Definitions != nil {
		for _, sg := range g.Definitions.Subgraphs {
			if subgraphIDRe.MatchString(sg.ID) {
				subgraphs[sg.ID] = true
			}
		}
	}
	if err := checkGraphNodes("graph.nodes", g.Nodes, models, customTypes, subgraphs); err != nil {
		return err
	}
	if g.Definitions != nil {
		for i, sg := range g.Definitions.Subgraphs {
			p := fmt.Sprintf("graph.definitions.subgraphs[%d].nodes", i)
			if !isJSONArray(sg.Nodes) {
				return invalid(p, "子图缺少 nodes 数组")
			}
			if err := checkGraphNodes(p, sg.Nodes, models, customTypes, subgraphs); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkGraphNodes(path string, raw json.RawMessage, models modelSet, customTypes, subgraphs map[string]bool) error {
	var nodes []json.RawMessage
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return invalid(path, "nodes 应为数组")
	}
	for i, nr := range nodes {
		p := fmt.Sprintf("%s[%d]", path, i)
		var n struct {
			ID         any             `json:"id"`
			Type       string          `json:"type"`
			Mode       any             `json:"mode"`
			Properties map[string]any  `json:"properties"`
			Widgets    json.RawMessage `json:"widgets_values"`
		}
		if t := bytes.TrimSpace(nr); len(t) == 0 || t[0] != '{' || json.Unmarshal(t, &n) != nil || n.Type == "" {
			return invalid(p, "节点缺少 type")
		}
		// 静音（mode 2）/ 旁路（mode 4）的节点 ComfyUI 不执行，不做 D16 校验（官方 Qwen-Image 模板里就有旁路的 LoRA 加载器）。
		if m, ok := toInt(n.Mode); ok && (m == 2 || m == 4) {
			continue
		}
		label := fmt.Sprintf("节点 #%v %s", n.ID, n.Type)
		if isCustomGraphNode(n.Type, n.Properties, subgraphs) && !customTypes[n.Type] {
			return invalid(p, label+" 不是内置节点，须在 manifest.nodes[].node_types 里声明")
		}
		for _, s := range loaderWidgetModels(n.Type, n.Widgets) {
			if err := models.ref(p, label, n.Type, s); err != nil {
				return err
			}
		}
	}
	return nil
}

// loaderWidgetModels 模型加载节点 widgets_values 里的模型文件名：数组按位置（modelLoaderWidgets）；对象（部分前端 /
// 节点包的写法）按入参名（modelLoaderInputs）；其它形状、非字符串、空串跳过。
func loaderWidgetModels(typ string, raw json.RawMessage) []string {
	var out []string
	t := bytes.TrimSpace(raw)
	switch {
	case len(t) == 0:
	case t[0] == '[':
		var wv []any
		if json.Unmarshal(t, &wv) == nil {
			for _, k := range modelLoaderWidgets[typ] {
				if k < len(wv) {
					if s, ok := wv[k].(string); ok && s != "" {
						out = append(out, s)
					}
				}
			}
		}
	case t[0] == '{':
		var wv map[string]any
		if json.Unmarshal(t, &wv) == nil {
			for _, name := range modelLoaderInputs[typ] {
				if s, ok := wv[name].(string); ok && s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// isCustomGraphNode D16：properties.cnr_id 存在且 ≠ comfy-core、或 properties.aux_id 存在、或 type 不是内置节点（也不是子图实例）。
func isCustomGraphNode(typ string, props map[string]any, subgraphs map[string]bool) bool {
	if cnr, ok := props["cnr_id"]; ok && cnr != "comfy-core" {
		return true
	}
	if _, ok := props["aux_id"]; ok {
		return true
	}
	return !builtinNodeTypes[typ] && !subgraphs[typ]
}

func validateInputs(inputs []InputSpec, api map[string]apiNode, models modelSet) error {
	if len(inputs) > MaxInputs {
		return invalid("manifest.inputs", fmt.Sprintf("最多 %d 个参数", MaxInputs))
	}
	keys, targets, images := map[string]bool{}, map[[2]string]bool{}, 0
	for i, in := range inputs {
		p := fmt.Sprintf("manifest.inputs[%d]", i)
		if !inputKeyRe.MatchString(in.Key) {
			return invalid(p+".key", "只能是小写字母开头的 a-z0-9_，≤ 32 字符")
		}
		if keys[in.Key] {
			return invalid(p+".key", "参数 "+in.Key+" 重复")
		}
		keys[in.Key] = true
		if in.Label == "" || utf8.RuneCountInString(in.Label) > maxLabelRunes {
			return invalid(p+".label", fmt.Sprintf("名称必填，≤ %d 字符", maxLabelRunes))
		}
		if !slices.Contains(InputTypes, in.Type) {
			return invalid(p+".type", "类型只能是 "+strings.Join(InputTypes, " / "))
		}
		node, ok := api[in.Node]
		if !ok {
			return invalid(p+".node", "节点 "+in.Node+" 不在 api_prompt 里")
		}
		cur, ok := node.Inputs[in.Input]
		if !ok {
			return invalid(p+".input", "节点 "+in.Node+" 没有输入 "+in.Input)
		}
		if _, isLink := cur.([]any); isLink {
			return invalid(p+".input", "节点 "+in.Node+" 的 "+in.Input+" 是连线，不能作为参数")
		}
		t := [2]string{in.Node, in.Input}
		if targets[t] {
			return invalid(p+".input", "节点 "+in.Node+" 的 "+in.Input+" 已被其它参数使用")
		}
		targets[t] = true
		if !currentValueMatches(in.Type, cur) {
			return invalid(p+".input", "节点 "+in.Node+" 的 "+in.Input+" 现值类型与参数类型 "+in.Type+" 不符")
		}
		// 模型文件名入参只能做成 enum，选项都必须是声明的模型（否则快速出图能让 ComfyUI 去读清单外的文件）。
		if isLoaderInput(node.ClassType, in.Input) {
			if in.Type != InputEnum {
				return invalid(p+".type", node.ClassType+" 的 "+in.Input+" 是模型文件名，只能用 enum 类型")
			}
			for j, o := range in.Options {
				if err := models.ref(fmt.Sprintf("%s.options[%d]", p, j), "选项", node.ClassType, o); err != nil {
					return err
				}
			}
		}
		if in.Type == InputImage {
			if images++; images > maxImageInput {
				return invalid("manifest.inputs", fmt.Sprintf("image 类型最多 %d 项", maxImageInput))
			}
		}
		if err := validateInputAttrs(p, in); err != nil {
			return err
		}
	}
	return nil
}

func currentValueMatches(typ string, cur any) bool {
	switch typ {
	case InputText, InputTextarea, InputEnum, InputImage:
		_, ok := cur.(string)
		return ok
	case InputInt, InputFloat:
		_, ok := toFloat(cur)
		return ok
	case InputSeed:
		_, ok := toInt(cur)
		return ok
	case InputBool:
		_, ok := cur.(bool)
		return ok
	}
	return false
}

// validateInputAttrs 校验与类型相关的属性（min / max / step / max_len / options / default）；不适用的属性必须不填。
func validateInputAttrs(p string, in InputSpec) error {
	numeric := in.Type == InputInt || in.Type == InputFloat
	text := in.Type == InputText || in.Type == InputTextarea
	if !numeric {
		switch {
		case in.Min != nil:
			return invalid(p+".min", in.Type+" 类型不能设 min")
		case in.Max != nil:
			return invalid(p+".max", in.Type+" 类型不能设 max")
		case in.Step != nil:
			return invalid(p+".step", in.Type+" 类型不能设 step")
		}
	}
	if !text && in.MaxLen != 0 {
		return invalid(p+".max_len", in.Type+" 类型不能设 max_len")
	}
	if in.Type != InputEnum && in.Options != nil {
		return invalid(p+".options", in.Type+" 类型不能设 options")
	}
	switch in.Type {
	case InputInt, InputFloat:
		if in.Min == nil {
			return invalid(p+".min", "数值参数必须设 min")
		}
		if in.Max == nil {
			return invalid(p+".max", "数值参数必须设 max")
		}
		if *in.Min > *in.Max {
			return invalid(p+".min", "min 不能大于 max")
		}
		if in.Type == InputInt {
			if !isIntegral(*in.Min) {
				return invalid(p+".min", "int 参数的 min 必须是整数")
			}
			if !isIntegral(*in.Max) {
				return invalid(p+".max", "int 参数的 max 必须是整数")
			}
		}
		if in.Step != nil {
			if *in.Step <= 0 || (in.Type == InputInt && !isIntegral(*in.Step)) {
				return invalid(p+".step", "step 必须为正（int 参数为正整数）")
			}
		}
	case InputText, InputTextarea:
		if in.MaxLen < 1 || in.MaxLen > maxMaxLen {
			return invalid(p+".max_len", fmt.Sprintf("max_len 应在 1～%d", maxMaxLen))
		}
	case InputEnum:
		if len(in.Options) < 1 || len(in.Options) > maxOptions {
			return invalid(p+".options", fmt.Sprintf("选项应有 1～%d 项", maxOptions))
		}
		seen := map[string]bool{}
		for j, o := range in.Options {
			if o == "" || utf8.RuneCountInString(o) > maxOptionLen || seen[o] {
				return invalid(fmt.Sprintf("%s.options[%d]", p, j), fmt.Sprintf("选项不能为空、不能重复，≤ %d 字符", maxOptionLen))
			}
			seen[o] = true
		}
	case InputImage:
		if in.Default != nil {
			return invalid(p+".default", "image 参数不能有默认值")
		}
		return nil
	}
	if in.Default == nil {
		return nil
	}
	if in.Type == InputSeed {
		if v, ok := toInt(in.Default); !ok || v < -1 || v >= maxSeed {
			return invalid(p+".default", "种子默认值应为 -1（随机）或 0～2^53-1 的整数")
		}
		return nil
	}
	if _, err := coerceValue(in, in.Default); err != "" {
		return invalid(p+".default", "默认值"+err)
	}
	return nil
}

// coerceValue 按参数类型校验并转换一个值（不含 seed 的随机与 image 以外的特殊处理）；第二个返回值为空表示合法，
// 否则是错误原因（如「必须是整数」）。
func coerceValue(in InputSpec, v any) (any, string) {
	switch in.Type {
	case InputText, InputTextarea:
		s, ok := v.(string)
		if !ok {
			return nil, "必须是文本"
		}
		maxLen := in.MaxLen
		if maxLen == 0 {
			maxLen = defaultMaxLen
		}
		if utf8.RuneCountInString(s) > maxLen {
			return nil, fmt.Sprintf("不能超过 %d 字", maxLen)
		}
		if strings.ContainsRune(s, 0) || !utf8.ValidString(s) {
			return nil, "含非法字符"
		}
		return s, ""
	case InputInt:
		n, ok := toInt(v)
		if !ok {
			return nil, "必须是整数"
		}
		if in.Min != nil && float64(n) < *in.Min || in.Max != nil && float64(n) > *in.Max {
			return nil, fmt.Sprintf("应在 %s～%s", fmtNum(in.Min), fmtNum(in.Max))
		}
		// step 从 min 起对齐（(v - min) % step == 0，如 Wan22 的 length：min 1、step 4 → 1、5、…、121）；无 min 时按 v % step。
		if in.Step != nil && *in.Step > 0 {
			base := 0.0
			if in.Min != nil {
				base = *in.Min
			}
			if math.Mod(float64(n)-base, *in.Step) != 0 {
				return nil, fmt.Sprintf("必须是 %s 加 %s 的整数倍", fmtNum(&base), fmtNum(in.Step))
			}
		}
		return n, ""
	case InputFloat:
		f, ok := toFloat(v)
		if !ok {
			return nil, "必须是数字"
		}
		if in.Min != nil && f < *in.Min || in.Max != nil && f > *in.Max {
			return nil, fmt.Sprintf("应在 %s～%s", fmtNum(in.Min), fmtNum(in.Max))
		}
		return f, ""
	case InputBool:
		b, ok := v.(bool)
		if !ok {
			return nil, "必须是布尔值"
		}
		return b, ""
	case InputEnum:
		s, ok := v.(string)
		if !ok || !slices.Contains(in.Options, s) {
			return nil, "不是可选值"
		}
		return s, ""
	case InputImage:
		s, ok := v.(string)
		if !ok || !quickImageRe.MatchString(s) {
			return nil, "请先上传图片"
		}
		return s, ""
	}
	return nil, "类型未知"
}

func fmtNum(p *float64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprint(*p)
}

func validateOutputs(outputs []OutputSpec, api map[string]apiNode) error {
	if len(outputs) > MaxOutputs {
		return invalid("manifest.outputs", fmt.Sprintf("最多 %d 个输出节点", MaxOutputs))
	}
	seen := map[string]bool{}
	for i, o := range outputs {
		p := fmt.Sprintf("manifest.outputs[%d].node", i)
		n, ok := api[o.Node]
		if !ok {
			return invalid(p, "节点 "+o.Node+" 不在 api_prompt 里")
		}
		if !outputClassTypes[n.ClassType] {
			return invalid(p, "节点 "+o.Node+" 的类型 "+n.ClassType+" 不是保存图片 / 视频的节点")
		}
		if seen[o.Node] {
			return invalid(p, "输出节点 "+o.Node+" 重复")
		}
		seen[o.Node] = true
	}
	return nil
}

// ---- 小工具 ----

func isJSONArray(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '['
}

func isIntegral(f float64) bool { return f == math.Trunc(f) && !math.IsInf(f, 0) && !math.IsNaN(f) }

// toFloat 接受 JSON 解出的数字（float64 / json.Number）与 Go 整数。
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// toInt 接受整数值的数字（1024、1024.0、json.Number("1024")）；超出 int64 或带小数 → false。
func toInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		f, err := n.Float64()
		if err != nil || !isIntegral(f) || math.Abs(f) >= 1<<63 {
			return 0, false
		}
		return int64(f), true
	}
	f, ok := toFloat(v)
	if !ok || !isIntegral(f) || math.Abs(f) >= 1<<63 {
		return 0, false
	}
	return int64(f), true
}

func decodeNumber(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareNodeID)
	return keys
}

// compareNodeID 节点 id 排序：都是十进制数字时按数值（"9" < "10"），否则按字典序。
func compareNodeID(a, b string) int {
	if isDigits(a) && isDigits(b) {
		ta, tb := strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(ta) != len(tb) {
			return len(ta) - len(tb)
		}
		if c := strings.Compare(ta, tb); c != 0 {
			return c
		}
	}
	return strings.Compare(a, b) // 数值相同（"010" 与 "10"）按原文定序，结果稳定
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
