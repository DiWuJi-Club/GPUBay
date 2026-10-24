package comfywf

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestManifestHash(t *testing.T) {
	g1 := json.RawMessage(`{"nodes":[{"id":1,"type":"KSampler"}],"links":[],"version":0.4}`)
	g2 := json.RawMessage("{\n  \"version\": 0.4,\n  \"links\": [],\n  \"nodes\": [{\"type\": \"KSampler\", \"id\": 1}]\n}")
	a1 := json.RawMessage(`{"3":{"class_type":"KSampler","inputs":{"seed":156680208700286,"cfg":7}}}`)
	a2 := json.RawMessage(`{ "3": { "inputs": { "cfg": 7, "seed": 156680208700286 }, "class_type": "KSampler" } }`)
	m := okManifest()
	h := ManifestHash(g1, a1, m)
	if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		t.Fatalf("hash = %q", h)
	}
	if ManifestHash(g2, a2, m) != h {
		t.Fatal("hash depends on key order / whitespace")
	}
	m2 := okManifest()
	m2.Models[0].SizeGB = 7
	if ManifestHash(g1, a1, m2) == h {
		t.Fatal("manifest change not reflected")
	}
	if ManifestHash(g1, json.RawMessage(`{"3":{"class_type":"KSampler","inputs":{"seed":156680208700287,"cfg":7}}}`), m) == h {
		t.Fatal("api_prompt change not reflected (number precision)")
	}
	if ManifestHash(g1, nil, m) == h || ManifestHash(g1, nil, m) != ManifestHash(g1, json.RawMessage("null"), m) {
		t.Fatal("nil api_prompt handling")
	}
	// nil 切片与空切片等价（Manifest.MarshalJSON 统一成 []）。
	e1, e2 := Manifest{}, Manifest{Models: []ModelSpec{}, Nodes: []NodeSpec{}, Inputs: []InputSpec{}, Outputs: []OutputSpec{}}
	if ManifestHash(g1, nil, e1) != ManifestHash(g1, nil, e2) {
		t.Fatal("nil vs empty slices")
	}
}

func TestDownloadGB(t *testing.T) {
	m := Manifest{Models: []ModelSpec{{SizeGB: 6.94}, {SizeGB: 0.07}, {SizeGB: 9.79}}}
	if got := DownloadGB(m); got != 16.8 {
		t.Fatalf("DownloadGB = %v", got)
	}
	if DownloadGB(Manifest{}) != 0 {
		t.Fatal("empty")
	}
}

func bundleSDXL() BundleWorkflow {
	return BundleWorkflow{Slug: "sdxl-txt2img", Version: "1.0.0", Graph: json.RawMessage(`{"nodes":[],"links":[]}`), Manifest: okManifest()}
}

func bundleUpscale() BundleWorkflow {
	m := Manifest{Models: []ModelSpec{
		{Name: "sd_xl_base_1.0.safetensors", Dir: "checkpoints", SizeGB: 6.94, HF: &RepoFile{Repo: "other/repo", File: "sd_xl_base_1.0.safetensors"}},
		upscaleModel(),
	}, Nodes: []NodeSpec{usduNode()}}
	return BundleWorkflow{Slug: "sdxl-upscale", Version: "1.0.1", Graph: json.RawMessage("{\n \"nodes\": [], \"links\": []\n}\n"), Manifest: m}
}

func TestBundleFiles(t *testing.T) {
	// 输入顺序打乱，输出按 slug 排序。
	files, err := BundleFiles([]BundleWorkflow{bundleUpscale(), bundleSDXL()})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[0].Name != "workflows/manifest.json" || files[1].Name != "workflows/sdxl-txt2img.json" ||
		files[2].Name != "workflows/sdxl-upscale.json" {
		t.Fatalf("files = %+v", files)
	}
	for _, f := range files {
		if f.Mode != 0o644 {
			t.Fatalf("%s mode %o", f.Name, f.Mode)
		}
	}
	// graph 原文（字节不变）。
	if string(files[2].Body) != "{\n \"nodes\": [], \"links\": []\n}\n" {
		t.Fatalf("graph body = %q", files[2].Body)
	}
	// 快照：规格 §5.2 形状；同名模型以先出现的为准（sdxl-upscale 的 checkpoint 改用 sdxl-txt2img 的来源）；节点不带 node_types。
	want := `{"workflows":[` +
		`{"slug":"sdxl-txt2img","version":"1.0.0","file":"sdxl-txt2img.json","models":[` +
		`{"name":"sd_xl_base_1.0.safetensors","dir":"checkpoints","size_gb":6.94,` +
		`"modelscope":{"repo":"AI-ModelScope/stable-diffusion-xl-base-1.0","file":"sd_xl_base_1.0.safetensors"},` +
		`"hf":{"repo":"stabilityai/stable-diffusion-xl-base-1.0","file":"sd_xl_base_1.0.safetensors"},"sha256":""}],"nodes":[]},` +
		`{"slug":"sdxl-upscale","version":"1.0.1","file":"sdxl-upscale.json","models":[` +
		`{"name":"sd_xl_base_1.0.safetensors","dir":"checkpoints","size_gb":6.94,` +
		`"modelscope":{"repo":"AI-ModelScope/stable-diffusion-xl-base-1.0","file":"sd_xl_base_1.0.safetensors"},` +
		`"hf":{"repo":"stabilityai/stable-diffusion-xl-base-1.0","file":"sd_xl_base_1.0.safetensors"},"sha256":""},` +
		`{"name":"4x-UltraSharp.pth","dir":"upscale_models","size_gb":0.07,"hf":{"repo":"lokCX/4x-Ultrasharp","file":"4x-UltraSharp.pth"},"sha256":""}],` +
		`"nodes":[{"name":"ComfyUI_UltimateSDUpscale","git":"https://github.com/ssitu/ComfyUI_UltimateSDUpscale.git","commit":"` + testCommit + `"}]}` +
		`]}` + "\n"
	if string(files[0].Body) != want {
		t.Fatalf("manifest.json =\n%s\nwant\n%s", files[0].Body, want)
	}
	// 可重复。
	again, _ := BundleFiles([]BundleWorkflow{bundleSDXL(), bundleUpscale()})
	if string(again[0].Body) != want {
		t.Fatal("output depends on input order")
	}
}

func TestBundleFilesEmpty(t *testing.T) {
	// 集合为空也给出空清单：app.sh 据此删掉 user/default/workflows 里残留的 spark-*.json（移除最后一个工作流时）。
	files, err := BundleFiles(nil)
	if err != nil || len(files) != 1 || files[0].Name != "workflows/manifest.json" || string(files[0].Body) != `{"workflows":[]}`+"\n" {
		t.Fatalf("files = %+v, err = %v", files, err)
	}
}

func TestBundleFilesNodes(t *testing.T) {
	a, b := bundleUpscale(), bundleUpscale()
	b.Slug = "sdxl-upscale-2"
	// 同名同 commit：去重通过（两个工作流各自列出）。
	if _, err := BundleFiles([]BundleWorkflow{a, b}); err != nil {
		t.Fatal(err)
	}
	b.Manifest.Nodes = []NodeSpec{usduNode()}
	b.Manifest.Nodes[0].Commit = strings.Repeat("0", 40)
	_, err := BundleFiles([]BundleWorkflow{a, b})
	wantInvalid(t, err, "")
	if !strings.Contains(err.Error(), "ComfyUI_UltimateSDUpscale") || !strings.Contains(err.Error(), "commit 不同") {
		t.Fatalf("err = %v", err)
	}
	b.Manifest.Nodes[0].Commit = testCommit
	b.Manifest.Nodes[0].Git = "https://gitee.com/mirror/ComfyUI_UltimateSDUpscale.git"
	_, err = BundleFiles([]BundleWorkflow{a, b})
	wantInvalid(t, err, "")
	if !strings.Contains(err.Error(), "Git 地址不同") {
		t.Fatalf("err = %v", err)
	}
}

func TestBundleFilesInvalid(t *testing.T) {
	bad := []func(w *BundleWorkflow){
		func(w *BundleWorkflow) { w.Slug = "../etc" },
		func(w *BundleWorkflow) { w.Version = "" },
		func(w *BundleWorkflow) { w.Graph = nil },
		func(w *BundleWorkflow) { w.Graph = json.RawMessage(`{"nodes":`) },
	}
	for i, f := range bad {
		w := bundleSDXL()
		f(&w)
		if _, err := BundleFiles([]BundleWorkflow{w}); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if _, err := BundleFiles([]BundleWorkflow{bundleSDXL(), bundleSDXL()}); err == nil {
		t.Error("duplicate slug accepted")
	}
	many := make([]BundleWorkflow, 0, MaxWorkflowsPerDeployment+1)
	for i := 0; i <= MaxWorkflowsPerDeployment; i++ {
		w := bundleSDXL()
		w.Slug = "wf-" + string(rune('a'+i))
		many = append(many, w)
	}
	if _, err := BundleFiles(many); err == nil {
		t.Error("more than MaxWorkflowsPerDeployment accepted")
	}
}
