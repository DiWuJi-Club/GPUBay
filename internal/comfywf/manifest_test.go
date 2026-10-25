package comfywf

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"spark-cloud/internal/platform/apperr"
)

func fixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fp(v float64) *float64 { return &v }

const testCommit = "a5547db9e1d07d3318bb21e9e9c474f4c1e9c8df"

// okManifest 与 testdata/graph_sdxl_ok.json + api_sdxl.json 配套的合法清单（规格 §3.2 示例的形状）。
func okManifest() Manifest {
	return Manifest{
		ComfyUIMin: "0.3.60",
		Models: []ModelSpec{{
			Name: "sd_xl_base_1.0.safetensors", Dir: "checkpoints", SizeGB: 6.94,
			ModelScope: &RepoFile{Repo: "AI-ModelScope/stable-diffusion-xl-base-1.0", File: "sd_xl_base_1.0.safetensors"},
			HF:         &RepoFile{Repo: "stabilityai/stable-diffusion-xl-base-1.0", File: "sd_xl_base_1.0.safetensors"},
		}},
		Inputs: []InputSpec{
			{Key: "prompt", Label: "提示词", Type: InputTextarea, Node: "6", Input: "text", Default: "a photo of ...", MaxLen: 2000},
			{Key: "negative", Label: "反向提示词", Type: InputTextarea, Node: "7", Input: "text", Default: ""},
			{Key: "width", Label: "宽", Type: InputInt, Node: "5", Input: "width", Default: 1024.0, Min: fp(512), Max: fp(2048), Step: fp(64)},
			{Key: "height", Label: "高", Type: InputInt, Node: "5", Input: "height", Default: 1024.0, Min: fp(512), Max: fp(2048), Step: fp(64)},
			{Key: "steps", Label: "步数", Type: InputInt, Node: "3", Input: "steps", Default: 25.0, Min: fp(1), Max: fp(100)},
			{Key: "cfg", Label: "CFG", Type: InputFloat, Node: "3", Input: "cfg", Default: 7.0, Min: fp(1), Max: fp(30)},
			{Key: "sampler", Label: "采样器", Type: InputEnum, Node: "3", Input: "sampler_name", Default: "euler", Options: []string{"euler", "dpmpp_2m", "dpmpp_2m_sde"}},
			{Key: "seed", Label: "种子", Type: InputSeed, Node: "3", Input: "seed"},
			{Key: "image", Label: "参考图", Type: InputImage, Node: "10", Input: "image"},
		},
		Outputs: []OutputSpec{{Node: "9"}},
	}
}

func usduNode() NodeSpec {
	return NodeSpec{Name: "ComfyUI_UltimateSDUpscale", Git: "https://github.com/ssitu/ComfyUI_UltimateSDUpscale.git", Commit: testCommit,
		NodeTypes: []string{"UltimateSDUpscale", "UltimateSDUpscaleNoUpscale"}}
}

func upscaleModel() ModelSpec {
	return ModelSpec{Name: "4x-UltraSharp.pth", Dir: "upscale_models", SizeGB: 0.07,
		HF: &RepoFile{Repo: "lokCX/4x-Ultrasharp", File: "4x-UltraSharp.pth"}}
}

// wantInvalid 断言 err 是 workflow_version_invalid 且消息以 path 开头（path 为空只断言错误码）。
func wantInvalid(t *testing.T, err error, path string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != "workflow_version_invalid" {
		t.Fatalf("err = %v, want workflow_version_invalid at %q", err, path)
	}
	if path != "" && !strings.HasPrefix(ae.Message, path+"：") {
		t.Fatalf("message = %q, want prefix %q", ae.Message, path+"：")
	}
}

func TestValidateManifestOK(t *testing.T) {
	m := okManifest()
	m.ComfyUIMin = " 0.3.60 "
	m.Models[0].Dir = " Checkpoints"
	m.Models[0].SizeGB = 6.938
	m.Models[0].SHA256 = strings.Repeat("AB", 32)
	m.Inputs[0].Label = " 提示词 "
	if err := ValidateManifest(&m, fixture(t, "graph_sdxl_ok.json"), fixture(t, "api_sdxl.json")); err != nil {
		t.Fatal(err)
	}
	// 规范化：去空白、dir 小写、size_gb 两位小数、sha256 小写、text max_len 默认 1000。
	if m.ComfyUIMin != "0.3.60" || m.Models[0].Dir != "checkpoints" || m.Models[0].SizeGB != 6.94 ||
		m.Models[0].SHA256 != strings.Repeat("ab", 32) || m.Inputs[0].Label != "提示词" || m.Inputs[1].MaxLen != 1000 || m.Inputs[0].MaxLen != 2000 {
		t.Fatalf("not normalized: %+v", m)
	}
	// 无 api_prompt：inputs / outputs 为空即可。
	m2 := Manifest{Models: okManifest().Models}
	if err := ValidateManifest(&m2, fixture(t, "graph_sdxl_ok.json"), nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateManifest(&m2, fixture(t, "graph_sdxl_ok.json"), json.RawMessage("null")); err != nil {
		t.Fatal(err)
	}
}

// D16 的三个 fixture。
func TestValidateManifestD16(t *testing.T) {
	ok := okManifest()
	if err := ValidateManifest(&ok, fixture(t, "graph_sdxl_ok.json"), nil); err == nil {
		t.Fatal("inputs without api_prompt accepted")
	}
	plain := func() Manifest { return Manifest{Models: okManifest().Models} }

	// 缺模型：graph 的 LoraLoader 引用了没声明的 lora（graph_missing_model.json 第 10 项 = nodes[9]）。
	m := plain()
	err := ValidateManifest(&m, fixture(t, "graph_missing_model.json"), nil)
	wantInvalid(t, err, "graph.nodes[9]")
	if !strings.Contains(err.Error(), "add-detail-xl.safetensors") {
		t.Fatalf("err = %v", err)
	}
	m = plain()
	m.Models = append(m.Models, ModelSpec{Name: "add-detail-xl.safetensors", Dir: "loras", SizeGB: 0.2,
		ModelScope: &RepoFile{Repo: "a/b", File: "add-detail-xl.safetensors"}})
	if err := ValidateManifest(&m, fixture(t, "graph_missing_model.json"), nil); err != nil {
		t.Fatal(err)
	}

	// 自定义节点：UltimateSDUpscale（cnr_id = comfyui_ultimatesdupscale），还引用了 4x-UltraSharp.pth。
	m = plain()
	m.Models = append(m.Models, upscaleModel())
	err = ValidateManifest(&m, fixture(t, "graph_custom_node.json"), nil)
	wantInvalid(t, err, "graph.nodes[9]")
	if !strings.Contains(err.Error(), "UltimateSDUpscale") {
		t.Fatalf("err = %v", err)
	}
	m.Nodes = []NodeSpec{usduNode()}
	if err := ValidateManifest(&m, fixture(t, "graph_custom_node.json"), nil); err != nil {
		t.Fatal(err)
	}
	// 声明了节点但缺 upscale 模型。
	m = plain()
	m.Nodes = []NodeSpec{usduNode()}
	err = ValidateManifest(&m, fixture(t, "graph_custom_node.json"), nil)
	wantInvalid(t, err, "graph.nodes[5]")
	if !strings.Contains(err.Error(), "4x-UltraSharp.pth") {
		t.Fatalf("err = %v", err)
	}
}

// graphWith 用给定节点拼一个最小 graph。
func graphWith(nodes ...string) json.RawMessage {
	return json.RawMessage(`{"last_node_id":9,"nodes":[` + strings.Join(nodes, ",") + `],"links":[],"version":0.4}`)
}

func TestGraphRefs(t *testing.T) {
	models := []ModelSpec{
		{Name: "a.safetensors", Dir: "clip", SizeGB: 1, HF: &RepoFile{Repo: "x/y", File: "a.safetensors"}},
		{Name: "b.safetensors", Dir: "text_encoders", SizeGB: 1, HF: &RepoFile{Repo: "x/y", File: "b.safetensors"}},
		{Name: "v.safetensors", Dir: "vae", SizeGB: 1, HF: &RepoFile{Repo: "x/y", File: "v.safetensors"}},
		{Name: "u.safetensors", Dir: "diffusion_models", SizeGB: 1, HF: &RepoFile{Repo: "x/y", File: "u.safetensors"}},
		{Name: "c.safetensors", Dir: "checkpoints", SizeGB: 1, HF: &RepoFile{Repo: "x/y", File: "c.safetensors"}},
	}
	cases := []struct {
		name  string
		graph json.RawMessage
		nodes []NodeSpec
		path  string // "" = 通过
	}{
		{"dual clip both declared", graphWith(`{"id":1,"type":"DualCLIPLoader","properties":{"cnr_id":"comfy-core"},"widgets_values":["a.safetensors","b.safetensors","flux","default"]}`), nil, ""},
		{"dual clip second missing", graphWith(`{"id":1,"type":"DualCLIPLoader","widgets_values":["a.safetensors","z.safetensors","flux"]}`), nil, "graph.nodes[0]"},
		{"triple clip third missing", graphWith(`{"id":1,"type":"TripleCLIPLoader","widgets_values":["a.safetensors","b.safetensors","t5.safetensors"]}`), nil, "graph.nodes[0]"},
		{"widgets_values object checked", graphWith(`{"id":1,"type":"CheckpointLoaderSimple","widgets_values":{"ckpt_name":"zzz"}}`), nil, "graph.nodes[0]"},
		{"empty model name skipped", graphWith(`{"id":1,"type":"VAELoader","widgets_values":[""]}`), nil, ""},
		{"vae missing", graphWith(`{"id":1,"type":"VAELoader","widgets_values":["ae.safetensors"]}`), nil, "graph.nodes[0]"},
		{"frontend virtual nodes", graphWith(`{"id":1,"type":"Reroute"}`, `{"id":2,"type":"Note","widgets_values":["x"]}`, `{"id":3,"type":"MarkdownNote"}`, `{"id":4,"type":"PrimitiveNode"}`), nil, ""},
		{"unknown type without cnr_id", graphWith(`{"id":1,"type":"KSampler"}`, `{"id":2,"type":"MysteryNode"}`), nil, "graph.nodes[1]"},
		{"builtin name but foreign cnr_id", graphWith(`{"id":1,"type":"KSampler","properties":{"cnr_id":"comfyui-impact-pack"}}`), nil, "graph.nodes[0]"},
		{"builtin name with aux_id", graphWith(`{"id":1,"type":"KSampler","properties":{"aux_id":"someone/fork"}}`), nil, "graph.nodes[0]"},
		{"custom declared", graphWith(`{"id":1,"type":"UltimateSDUpscale","properties":{"cnr_id":"comfyui_ultimatesdupscale"}}`), []NodeSpec{usduNode()}, ""},
		{"muted node skipped", graphWith(`{"id":1,"type":"LoraLoaderModelOnly","mode":2,"widgets_values":["x.safetensors",1]}`), nil, ""},
		{"bypassed node skipped", graphWith(`{"id":1,"type":"LoraLoaderModelOnly","mode":4,"widgets_values":["x.safetensors",1]}`, `{"id":2,"type":"Foo","mode":4}`), nil, ""},
		{"active mode 0 checked", graphWith(`{"id":1,"type":"LoraLoaderModelOnly","mode":0,"widgets_values":["x.safetensors",1]}`), nil, "graph.nodes[0]"},
		{"incomplete nodes tolerated", graphWith(`{"id":1,"type":"KSampler","properties":{}}`, `{"id":2,"type":"VAELoader","widgets_values":null}`,
			`{"id":3,"type":"VAELoader","widgets_values":[5]}`, `{"id":4,"type":"CLIPLoader","widgets_values":"a"}`, `{"id":5,"type":"VAELoader","properties":null,"widgets_values":[]}`), nil, ""},
		{"node without type", graphWith(`{"id":1}`), nil, "graph.nodes[0]"},
		{"node not object", graphWith(`7`), nil, "graph.nodes[0]"},
		{"subgraph instance + inner nodes", json.RawMessage(`{"nodes":[{"id":1,"type":"0b7c5a3e-1111-4c2d-8e9f-123456789abc"}],"links":[],
			"definitions":{"subgraphs":[{"id":"0b7c5a3e-1111-4c2d-8e9f-123456789abc","nodes":[{"id":5,"type":"VAELoader","widgets_values":["v.safetensors"]}]}]}}`), nil, ""},
		{"subgraph id not uuid", json.RawMessage(`{"nodes":[{"id":1,"type":"KSampler"},{"id":2,"type":"s1"}],"links":[],
			"definitions":{"subgraphs":[{"id":"s1","nodes":[]}]}}`), nil, "graph.nodes[1]"},
		{"widgets_values object by input name", graphWith(`{"id":1,"type":"VAELoader","widgets_values":{"vae_name":"zz.safetensors"}}`), nil, "graph.nodes[0]"},
		{"widgets_values object declared", graphWith(`{"id":1,"type":"DualCLIPLoader","widgets_values":{"clip_name1":"a.safetensors","clip_name2":"b.safetensors","type":"flux"}}`), nil, ""},
		{"model wrong dir for loader", graphWith(`{"id":1,"type":"KSampler"}`, `{"id":2,"type":"VAELoader","widgets_values":["a.safetensors"]}`), nil, "manifest.models[0].dir"},
		{"unet in diffusion_models ok", graphWith(`{"id":1,"type":"UNETLoader","widgets_values":["u.safetensors","default"]}`), nil, ""},
		{"lora in checkpoints dir", graphWith(`{"id":1,"type":"LoraLoaderModelOnly","widgets_values":["c.safetensors",1]}`), nil, "manifest.models[4].dir"},
		{"subgraph inner missing model", json.RawMessage(`{"nodes":[],"links":[],
			"definitions":{"subgraphs":[{"id":"0b7c5a3e-1111-4c2d-8e9f-123456789abc","nodes":[{"id":5,"type":"KSampler"},{"id":6,"type":"VAELoader","widgets_values":["zz.safetensors"]}]}]}}`), nil, "graph.definitions.subgraphs[0].nodes[1]"},
		{"subgraph inner custom", json.RawMessage(`{"nodes":[],"links":[],
			"definitions":{"subgraphs":[{"id":"0b7c5a3e-1111-4c2d-8e9f-123456789abc","nodes":[{"id":5,"type":"Foo"}]}]}}`), nil, "graph.definitions.subgraphs[0].nodes[0]"},
		{"graph not object", json.RawMessage(`[]`), nil, "graph"},
		{"graph invalid json", json.RawMessage(`{"nodes":`), nil, "graph"},
		{"graph empty", nil, nil, "graph"},
		{"graph nodes missing", json.RawMessage(`{"links":[]}`), nil, "graph.nodes"},
		{"graph links not array", json.RawMessage(`{"nodes":[],"links":{}}`), nil, "graph.links"},
		{"graph too big", json.RawMessage(`{"nodes":[],"links":[],"x":"` + strings.Repeat("a", MaxGraphBytes) + `"}`), nil, "graph"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Manifest{Models: append([]ModelSpec(nil), models...), Nodes: c.nodes}
			err := ValidateManifest(&m, c.graph, nil)
			if c.path == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantInvalid(t, err, c.path)
		})
	}
}

func TestBuiltinNodeTypes(t *testing.T) {
	for _, n := range []string{"KSampler", "CheckpointLoaderSimple", "SaveImage", "LoadImage", "SaveVideo", "SaveWEBM", "SaveAnimatedWEBP",
		"UNETLoader", "DualCLIPLoader", "EmptySD3LatentImage", "Wan22ImageToVideoLatent", "ImageUpscaleWithModel", "PrimitiveString",
		"Reroute", "Note", "MarkdownNote", "PrimitiveNode"} {
		if !builtinNodeTypes[n] {
			t.Errorf("%s not builtin", n)
		}
	}
	for _, n := range []string{"UltimateSDUpscale", "VHS_VideoCombine", "MinimaxSubjectToVideoNode", ""} {
		if builtinNodeTypes[n] {
			t.Errorf("%s should not be builtin", n)
		}
	}
	if len(builtinNodeTypes) != 489 {
		t.Errorf("len = %d, want 485 + 4", len(builtinNodeTypes))
	}
}

// TestValidateManifestRules 每条规则至少一个反例（正例见 TestValidateManifestOK 与各 mutate 后的边界值）。
func TestValidateManifestRules(t *testing.T) {
	type mut func(m *Manifest)
	cases := []struct {
		name string
		mut  mut
		path string // "" = 通过
	}{
		// comfyui_min
		{"comfyui_min empty ok", func(m *Manifest) { m.ComfyUIMin = "" }, ""},
		{"comfyui_min bad", func(m *Manifest) { m.ComfyUIMin = "v0.3" }, "manifest.comfyui_min"},
		// models
		{"models none rejects graph ref", func(m *Manifest) { m.Models = nil }, "graph.nodes[6]"}, // graph 引用了 checkpoint
		{"models too many", func(m *Manifest) {
			for i := 0; i < MaxModelsPerVersion; i++ {
				m.Models = append(m.Models, ModelSpec{Name: "m" + string(rune('a'+i)) + ".bin", Dir: "loras", SizeGB: 1, HF: &RepoFile{Repo: "a/b", File: "x"}})
			}
		}, "manifest.models"},
		{"models 16 ok", func(m *Manifest) {
			for i := 0; i < MaxModelsPerVersion-1; i++ {
				m.Models = append(m.Models, ModelSpec{Name: "m" + string(rune('a'+i)) + ".bin", Dir: "loras", SizeGB: 1, HF: &RepoFile{Repo: "a/b", File: "x"}})
			}
		}, ""},
		{"model name slash", func(m *Manifest) { m.Models[0].Name = "sdxl/sd_xl_base_1.0.safetensors" }, "manifest.models[0].name"},
		{"model name leading dot", func(m *Manifest) { m.Models[0].Name = ".hidden" }, "manifest.models[0].name"},
		{"model name too long", func(m *Manifest) { m.Models[0].Name = strings.Repeat("a", 129) }, "manifest.models[0].name"},
		{"model name dup", func(m *Manifest) { m.Models = append(m.Models, m.Models[0]) }, "manifest.models[1].name"},
		{"model dir bad", func(m *Manifest) { m.Models[0].Dir = "custom_nodes" }, "manifest.models[0].dir"},
		{"model size low", func(m *Manifest) { m.Models[0].SizeGB = 0.004 }, "manifest.models[0].size_gb"},
		{"model size high", func(m *Manifest) { m.Models[0].SizeGB = 200.01 }, "manifest.models[0].size_gb"},
		{"model size 200 ok", func(m *Manifest) { m.Models[0].SizeGB = 200 }, ""},
		{"model no source", func(m *Manifest) { m.Models[0].ModelScope = nil; m.Models[0].HF = nil }, "manifest.models[0]"},
		{"model hf only ok", func(m *Manifest) { m.Models[0].ModelScope = nil }, ""},
		{"model repo bad", func(m *Manifest) { m.Models[0].ModelScope.Repo = "stable-diffusion" }, "manifest.models[0].modelscope.repo"},
		{"model repo too long", func(m *Manifest) { m.Models[0].HF.Repo = "a/" + strings.Repeat("b", 127) }, "manifest.models[0].hf.repo"},
		{"model repo 128 ok", func(m *Manifest) { m.Models[0].HF.Repo = "a/" + strings.Repeat("b", 126) }, ""},
		{"model repo 3 parts", func(m *Manifest) { m.Models[0].HF.Repo = "a/b/c" }, "manifest.models[0].hf.repo"},
		{"model file dotdot", func(m *Manifest) { m.Models[0].HF.File = "a/../b.safetensors" }, "manifest.models[0].hf.file"},
		{"model file dotdot inside", func(m *Manifest) { m.Models[0].HF.File = "a..b" }, "manifest.models[0].hf.file"},
		{"model file abs", func(m *Manifest) { m.Models[0].HF.File = "/etc/passwd" }, "manifest.models[0].hf.file"},
		{"model file space", func(m *Manifest) { m.Models[0].HF.File = "a b" }, "manifest.models[0].hf.file"},
		{"model file too long", func(m *Manifest) { m.Models[0].HF.File = strings.Repeat("a/", 128) + "b" }, "manifest.models[0].hf.file"},
		{"model file subdir ok", func(m *Manifest) { m.Models[0].HF.File = "split_files/text_encoders/clip_l.safetensors" }, ""},
		{"model sha bad", func(m *Manifest) { m.Models[0].SHA256 = "abc" }, "manifest.models[0].sha256"},
		// nodes
		{"node ok", func(m *Manifest) { m.Nodes = []NodeSpec{usduNode()} }, ""},
		{"node gitee ok", func(m *Manifest) { n := usduNode(); n.Git = "https://gitee.com/a/b"; m.Nodes = []NodeSpec{n} }, ""},
		{"nodes too many", func(m *Manifest) {
			for i := 0; i <= MaxNodesPerVersion; i++ {
				n := usduNode()
				n.Name = "n" + string(rune('a'+i))
				m.Nodes = append(m.Nodes, n)
			}
		}, "manifest.nodes"},
		{"node name bad", func(m *Manifest) { n := usduNode(); n.Name = "../x"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].name"},
		{"node name dup", func(m *Manifest) { m.Nodes = []NodeSpec{usduNode(), usduNode()} }, "manifest.nodes[1].name"},
		{"node git http", func(m *Manifest) { n := usduNode(); n.Git = "http://github.com/a/b.git"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].git"},
		{"node git host", func(m *Manifest) { n := usduNode(); n.Git = "https://gitlab.com/a/b.git"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].git"},
		{"node git userinfo", func(m *Manifest) { n := usduNode(); n.Git = "https://u:p@github.com/a/b.git"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].git"},
		{"node git port", func(m *Manifest) { n := usduNode(); n.Git = "https://github.com:8443/a/b.git"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].git"},
		{"node git query", func(m *Manifest) { n := usduNode(); n.Git = "https://github.com/a/b.git?x=1"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].git"},
		{"node git path", func(m *Manifest) { n := usduNode(); n.Git = "https://github.com/a/b/c"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].git"},
		{"node git userinfo host", func(m *Manifest) {
			n := usduNode()
			n.Git = "https://github.com@evil.com/a/b.git"
			m.Nodes = []NodeSpec{n}
		}, "manifest.nodes[0].git"},
		{"node git host suffix", func(m *Manifest) {
			n := usduNode()
			n.Git = "https://github.com.evil.com/a/b.git"
			m.Nodes = []NodeSpec{n}
		}, "manifest.nodes[0].git"},
		{"node git uppercase scheme", func(m *Manifest) { n := usduNode(); n.Git = "HTTPS://github.com/a/b.git"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].git"},
		{"node git dot segment", func(m *Manifest) { n := usduNode(); n.Git = "https://github.com/../b.git"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].git"},
		{"node git dot repo", func(m *Manifest) { n := usduNode(); n.Git = "https://github.com/a/.."; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].git"},
		{"node git leading dot", func(m *Manifest) {
			n := usduNode()
			n.Git = "https://github.com/a/.hidden.git"
			m.Nodes = []NodeSpec{n}
		}, "manifest.nodes[0].git"},
		{"node git single dot", func(m *Manifest) { n := usduNode(); n.Git = "https://github.com/./b"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].git"},
		{"node git quote", func(m *Manifest) { n := usduNode(); n.Git = "https://github.com/a/b'c"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].git"},
		{"node commit short", func(m *Manifest) { n := usduNode(); n.Commit = "a5547db"; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].commit"},
		{"node types empty", func(m *Manifest) { n := usduNode(); n.NodeTypes = nil; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].node_types"},
		{"node types too many", func(m *Manifest) {
			n := usduNode()
			for len(n.NodeTypes) <= 64 {
				n.NodeTypes = append(n.NodeTypes, "T")
			}
			m.Nodes = []NodeSpec{n}
		}, "manifest.nodes[0].node_types"},
		{"node type bad char", func(m *Manifest) { n := usduNode(); n.NodeTypes = []string{"A|B"}; m.Nodes = []NodeSpec{n} }, "manifest.nodes[0].node_types[0]"},
		{"node type with space ok", func(m *Manifest) {
			n := usduNode()
			n.NodeTypes = []string{"Image Resize", "pkg.Node:v2"}
			m.Nodes = []NodeSpec{n}
		}, ""},
		// inputs
		{"inputs too many", func(m *Manifest) {
			for i := 0; len(m.Inputs) <= MaxInputs; i++ {
				m.Inputs = append(m.Inputs, InputSpec{Key: "k" + string(rune('a'+i)), Label: "x", Type: InputText, Node: "6", Input: "text"})
			}
		}, "manifest.inputs"},
		{"input key bad", func(m *Manifest) { m.Inputs[0].Key = "Prompt" }, "manifest.inputs[0].key"},
		{"input key dup", func(m *Manifest) { m.Inputs[1].Key = "prompt" }, "manifest.inputs[1].key"},
		{"input label empty", func(m *Manifest) { m.Inputs[0].Label = " " }, "manifest.inputs[0].label"},
		{"input label long", func(m *Manifest) { m.Inputs[0].Label = strings.Repeat("字", 33) }, "manifest.inputs[0].label"},
		{"input label 32 ok", func(m *Manifest) { m.Inputs[0].Label = strings.Repeat("字", 32) }, ""},
		{"input type bad", func(m *Manifest) { m.Inputs[0].Type = "string" }, "manifest.inputs[0].type"},
		{"input node missing", func(m *Manifest) { m.Inputs[0].Node = "99" }, "manifest.inputs[0].node"},
		{"input input missing", func(m *Manifest) { m.Inputs[0].Input = "prompt" }, "manifest.inputs[0].input"},
		{"input is link", func(m *Manifest) {
			m.Inputs[0] = InputSpec{Key: "prompt", Label: "x", Type: InputText, Node: "6", Input: "clip"}
		}, "manifest.inputs[0].input"},
		{"input same target twice", func(m *Manifest) {
			m.Inputs = append(m.Inputs, InputSpec{Key: "prompt2", Label: "x", Type: InputText, Node: "6", Input: "text"})
		}, "manifest.inputs[9].input"},
		{"text current not string", func(m *Manifest) {
			m.Inputs[0] = InputSpec{Key: "prompt", Label: "x", Type: InputText, Node: "3", Input: "steps"}
		}, "manifest.inputs[0].input"},
		{"text max_len too big", func(m *Manifest) { m.Inputs[0].MaxLen = 4001 }, "manifest.inputs[0].max_len"},
		{"text max_len negative", func(m *Manifest) { m.Inputs[0].MaxLen = -1 }, "manifest.inputs[0].max_len"},
		{"text default too long", func(m *Manifest) { m.Inputs[0].MaxLen = 3 }, "manifest.inputs[0].default"},
		{"text default not string", func(m *Manifest) { m.Inputs[0].Default = 3.0 }, "manifest.inputs[0].default"},
		{"text default NUL", func(m *Manifest) { m.Inputs[0].Default = "a\x00b" }, "manifest.inputs[0].default"},
		{"text with min", func(m *Manifest) { m.Inputs[0].Min = fp(1) }, "manifest.inputs[0].min"},
		{"int no min", func(m *Manifest) { m.Inputs[2].Min = nil }, "manifest.inputs[2].min"},
		{"int no max", func(m *Manifest) { m.Inputs[2].Max = nil }, "manifest.inputs[2].max"},
		{"int min > max", func(m *Manifest) { m.Inputs[2].Min = fp(4096) }, "manifest.inputs[2].min"},
		{"int min fractional", func(m *Manifest) { m.Inputs[4].Min = fp(1.5) }, "manifest.inputs[4].min"},
		{"int step zero", func(m *Manifest) { m.Inputs[2].Step = fp(0) }, "manifest.inputs[2].step"},
		{"int step fractional", func(m *Manifest) { m.Inputs[2].Step = fp(0.5) }, "manifest.inputs[2].step"},
		{"int min not multiple of step ok", func(m *Manifest) { m.Inputs[2].Min = fp(1); m.Inputs[2].Default = 1025.0 }, ""},
		{"int default unaligned from min", func(m *Manifest) { m.Inputs[2].Min = fp(1) }, "manifest.inputs[2].default"},
		{"int default out of range", func(m *Manifest) { m.Inputs[2].Default = 4096.0 }, "manifest.inputs[2].default"},
		{"int default fractional", func(m *Manifest) { m.Inputs[4].Default = 2.5 }, "manifest.inputs[4].default"},
		{"int default unaligned", func(m *Manifest) { m.Inputs[2].Default = 1000.0 }, "manifest.inputs[2].default"},
		{"int default absent ok", func(m *Manifest) { m.Inputs[2].Default = nil }, ""},
		{"int default go int ok", func(m *Manifest) { m.Inputs[2].Default = 1024 }, ""},
		{"int current not number", func(m *Manifest) {
			m.Inputs[2] = InputSpec{Key: "width", Label: "x", Type: InputInt, Node: "6", Input: "text", Min: fp(1), Max: fp(2)}
		}, "manifest.inputs[2].input"},
		{"int with options", func(m *Manifest) { m.Inputs[2].Options = []string{"a"} }, "manifest.inputs[2].options"},
		{"float default out of range", func(m *Manifest) { m.Inputs[5].Default = 0.5 }, "manifest.inputs[5].default"},
		{"float no min", func(m *Manifest) { m.Inputs[5].Min = nil }, "manifest.inputs[5].min"},
		{"float with max_len", func(m *Manifest) { m.Inputs[5].MaxLen = 3 }, "manifest.inputs[5].max_len"},
		{"enum no options", func(m *Manifest) { m.Inputs[6].Options = nil }, "manifest.inputs[6].options"},
		{"enum too many options", func(m *Manifest) {
			for len(m.Inputs[6].Options) <= 32 {
				m.Inputs[6].Options = append(m.Inputs[6].Options, "o"+strings.Repeat("x", len(m.Inputs[6].Options)))
			}
		}, "manifest.inputs[6].options"},
		{"enum option long", func(m *Manifest) { m.Inputs[6].Options = append(m.Inputs[6].Options, strings.Repeat("a", 65)) }, "manifest.inputs[6].options[3]"},
		{"enum option empty", func(m *Manifest) { m.Inputs[6].Options = append(m.Inputs[6].Options, "") }, "manifest.inputs[6].options[3]"},
		{"enum option dup", func(m *Manifest) { m.Inputs[6].Options = append(m.Inputs[6].Options, "euler") }, "manifest.inputs[6].options[3]"},
		{"enum default not in options", func(m *Manifest) { m.Inputs[6].Default = "ddim" }, "manifest.inputs[6].default"},
		{"seed current not integer", func(m *Manifest) { m.Inputs[7].Node = "12"; m.Inputs[7].Input = "scale_by" }, "manifest.inputs[7].input"},
		{"seed on float", func(m *Manifest) { m.Inputs[7].Input = "denoise"; m.Inputs[5].Input = "cfg" }, ""}, // denoise = 1（整数值）可当种子位
		{"seed on text", func(m *Manifest) {
			m.Inputs = []InputSpec{{Key: "seed", Label: "x", Type: InputSeed, Node: "6", Input: "text"}}
		}, "manifest.inputs[0].input"},
		{"seed default -1 ok", func(m *Manifest) { m.Inputs[7].Default = -1.0 }, ""},
		{"seed default -2", func(m *Manifest) { m.Inputs[7].Default = -2.0 }, "manifest.inputs[7].default"},
		{"seed default huge", func(m *Manifest) { m.Inputs[7].Default = 1e17 }, "manifest.inputs[7].default"},
		{"image default", func(m *Manifest) { m.Inputs[8].Default = "spark-quickrun/abcdefabcdef.png" }, "manifest.inputs[8].default"},
		{"image two ok", func(m *Manifest) {
			m.Inputs = append(m.Inputs, InputSpec{Key: "image2", Label: "x", Type: InputImage, Node: "13", Input: "image"})
		}, ""},
		{"image too many", func(m *Manifest) {
			m.Inputs = append(m.Inputs, InputSpec{Key: "image2", Label: "x", Type: InputImage, Node: "13", Input: "image"},
				InputSpec{Key: "image3", Label: "x", Type: InputImage, Node: "14", Input: "image"})
		}, "manifest.inputs"},
		{"image current not string", func(m *Manifest) { m.Inputs[8].Node = "12"; m.Inputs[8].Input = "count" }, "manifest.inputs[8].input"},
		{"bool default not bool", func(m *Manifest) {
			m.Inputs = append(m.Inputs, InputSpec{Key: "flag", Label: "x", Type: InputBool, Node: "12", Input: "enabled", Default: "yes"})
		}, "manifest.inputs[9].default"},
		{"bool current not bool", func(m *Manifest) {
			m.Inputs = append(m.Inputs, InputSpec{Key: "flag", Label: "x", Type: InputBool, Node: "12", Input: "count"})
		}, "manifest.inputs[9].input"},
		{"bool ok", func(m *Manifest) {
			m.Inputs = append(m.Inputs, InputSpec{Key: "flag", Label: "x", Type: InputBool, Node: "12", Input: "enabled", Default: true})
		}, ""},
		// 模型加载入参只能用 enum，且选项都是声明的模型
		{"loader input as text", func(m *Manifest) {
			m.Inputs = append(m.Inputs, InputSpec{Key: "ckpt", Label: "模型", Type: InputText, Node: "4", Input: "ckpt_name"})
		}, "manifest.inputs[9].type"},
		{"loader input as textarea", func(m *Manifest) {
			m.Inputs = append(m.Inputs, InputSpec{Key: "ckpt", Label: "模型", Type: InputTextarea, Node: "4", Input: "ckpt_name"})
		}, "manifest.inputs[9].type"},
		{"loader enum undeclared option", func(m *Manifest) {
			m.Inputs = append(m.Inputs, InputSpec{Key: "ckpt", Label: "模型", Type: InputEnum, Node: "4", Input: "ckpt_name",
				Options: []string{"sd_xl_base_1.0.safetensors", "other.safetensors"}})
		}, "manifest.inputs[9].options[1]"},
		{"loader enum declared ok", func(m *Manifest) {
			m.Inputs = append(m.Inputs, InputSpec{Key: "ckpt", Label: "模型", Type: InputEnum, Node: "4", Input: "ckpt_name",
				Default: "sd_xl_base_1.0.safetensors", Options: []string{"sd_xl_base_1.0.safetensors"}})
		}, ""},
		// outputs
		{"outputs empty ok", func(m *Manifest) { m.Outputs = nil }, ""},
		{"output not in api", func(m *Manifest) { m.Outputs[0].Node = "99" }, "manifest.outputs[0].node"},
		{"output wrong class", func(m *Manifest) { m.Outputs[0].Node = "11" }, "manifest.outputs[0].node"},
		{"output dup", func(m *Manifest) { m.Outputs = append(m.Outputs, m.Outputs[0]) }, "manifest.outputs[1].node"},
		{"outputs too many", func(m *Manifest) {
			for len(m.Outputs) <= MaxOutputs {
				m.Outputs = append(m.Outputs, OutputSpec{Node: "9"})
			}
		}, "manifest.outputs"},
	}
	api := apiWithExtras(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := okManifest()
			c.mut(&m)
			err := ValidateManifest(&m, fixture(t, "graph_sdxl_ok.json"), api)
			if c.path == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantInvalid(t, err, c.path)
		})
	}
}

// apiWithExtras = api_sdxl.json + 带 bool / 数值入参的节点 "12" 与两个 LoadImage "13" "14"。
func apiWithExtras(t *testing.T) json.RawMessage {
	var p map[string]any
	if err := json.Unmarshal(fixture(t, "api_sdxl.json"), &p); err != nil {
		t.Fatal(err)
	}
	p["12"] = map[string]any{"class_type": "ImageScaleBy", "inputs": map[string]any{"enabled": false, "count": 2, "scale_by": 1.5, "image": []any{"8", 0}}}
	p["13"] = map[string]any{"class_type": "LoadImage", "inputs": map[string]any{"image": "a.png"}}
	p["14"] = map[string]any{"class_type": "LoadImage", "inputs": map[string]any{"image": "b.png"}}
	b, _ := json.Marshal(p)
	return b
}

func TestValidateAPIPrompt(t *testing.T) {
	plain := func() Manifest { return Manifest{Models: okManifest().Models} }
	cases := []struct {
		name string
		api  string
		path string
	}{
		{"not object", `[1]`, "api_prompt"},
		{"invalid json", `{"3":`, "api_prompt"},
		{"empty object", `{}`, "api_prompt"},
		{"node not object", `{"3":1}`, "api_prompt.3"},
		{"no class_type", `{"3":{"inputs":{}}}`, "api_prompt.3.class_type"},
		{"class_type not string", `{"3":{"class_type":1,"inputs":{}}}`, "api_prompt.3.class_type"},
		{"inputs not object", `{"3":{"class_type":"KSampler","inputs":[]}}`, "api_prompt.3.inputs"},
		{"custom class undeclared", `{"3":{"class_type":"UltimateSDUpscale","inputs":{}}}`, "api_prompt.3.class_type"},
		{"model undeclared", `{"4":{"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"other.safetensors"}}}`, "api_prompt.4.inputs.ckpt_name"},
		{"lora undeclared", `{"4":{"class_type":"LoraLoader","inputs":{"lora_name":"x.safetensors","model":["1",0]}}}`, "api_prompt.4.inputs.lora_name"},
		{"too big", `{"3":{"class_type":"KSampler","inputs":{"x":"` + strings.Repeat("a", MaxAPIPromptBytes) + `"}}}`, "api_prompt"},
		{"model wrong dir", `{"4":{"class_type":"VAELoader","inputs":{"vae_name":"sd_xl_base_1.0.safetensors"}}}`, "manifest.models[0].dir"},
		{"ok", `{"4":{"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"sd_xl_base_1.0.safetensors"}}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := plain()
			err := ValidateManifest(&m, fixture(t, "graph_sdxl_ok.json"), json.RawMessage(c.api))
			if c.path == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantInvalid(t, err, c.path)
		})
	}
}

func TestValidSlugVersion(t *testing.T) {
	for _, s := range []string{"sdxl-txt2img", "ab", "a" + strings.Repeat("b", 47)} {
		if !ValidSlug(s) {
			t.Errorf("%q rejected", s)
		}
	}
	for _, s := range []string{"a", "Sdxl", "1abc", "a_b", "a" + strings.Repeat("b", 48), "../x"} {
		if ValidSlug(s) {
			t.Errorf("%q accepted", s)
		}
	}
	if !ValidVersion("1.0.0") || !ValidVersion("2024-10-rc1") || ValidVersion(".1") || ValidVersion("1 0") {
		t.Fatal("ValidVersion")
	}
}
