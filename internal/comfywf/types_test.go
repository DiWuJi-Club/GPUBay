package comfywf

import (
	"encoding/json"
	"testing"
)

// 常量与规格 §13 / 计划 Global Constraints 一致。
func TestConsts(t *testing.T) {
	if MaxWorkflowsPerDeployment != 8 || MaxGraphBytes != 262144 || MaxAPIPromptBytes != 262144 || MaxModelsPerVersion != 16 ||
		MaxNodesPerVersion != 8 || MaxInputs != 24 || MaxOutputs != 4 || MaxRunsInFlight != 2 || RunsPerHour != 120 ||
		MaxUploadBytes != 16<<20 || MaxViewBytes != 64<<20 || MaxCoverBytes != 2<<20 || ComfyPort != 8188 {
		t.Fatal("numeric constants differ from spec §13")
	}
	if ProxyTimeout.Seconds() != 30 || ViewTimeout.Seconds() != 120 || RunWaitMax.Minutes() != 30 {
		t.Fatal("timeouts differ from spec §13")
	}
	if InstalledFilePrefix != "spark-" || DataDirName != "comfyui" {
		t.Fatal("path constants differ")
	}
	if len(Categories) != 5 || Categories[0] != CategoryTxt2Img || Categories[4] != CategoryOther {
		t.Fatalf("Categories = %v", Categories)
	}
	if len(ModelDirs) != 13 || !ValidModelDir("checkpoints") || !ValidModelDir("vae_approx") || ValidModelDir("custom_nodes") {
		t.Fatalf("ModelDirs = %v", ModelDirs)
	}
	if len(GitHosts) != 3 || !ValidGitHost("gitee.com") || ValidGitHost("gitlab.com") {
		t.Fatalf("GitHosts = %v", GitHosts)
	}
	if !ValidCategory("video") || ValidCategory("llm") {
		t.Fatal("ValidCategory")
	}
}

// manifest 的 JSON 字段名与规格 §3.2 示例一致（往返不丢字段）。
func TestManifestJSON(t *testing.T) {
	src := `{"comfyui_min":"0.3.60",
	 "models":[{"name":"sd_xl_base_1.0.safetensors","dir":"checkpoints","size_gb":6.9,
	   "modelscope":{"repo":"AI-ModelScope/stable-diffusion-xl-base-1.0","file":"sd_xl_base_1.0.safetensors"},
	   "hf":{"repo":"stabilityai/stable-diffusion-xl-base-1.0","file":"sd_xl_base_1.0.safetensors"},"sha256":""}],
	 "nodes":[{"name":"ComfyUI_UltimateSDUpscale","git":"https://github.com/ssitu/ComfyUI_UltimateSDUpscale.git",
	   "commit":"0123456789abcdef0123456789abcdef01234567","node_types":["UltimateSDUpscale"]}],
	 "inputs":[{"key":"width","label":"宽","type":"int","node":"5","input":"width","default":1024,"min":512,"max":2048,"step":64},
	   {"key":"sampler","label":"采样器","type":"enum","node":"3","input":"sampler_name","default":"euler","options":["euler"]},
	   {"key":"prompt","label":"提示词","type":"textarea","node":"6","input":"text","max_len":2000}],
	 "outputs":[{"node":"9"}]}`
	var m Manifest
	if err := json.Unmarshal([]byte(src), &m); err != nil {
		t.Fatal(err)
	}
	if m.ComfyUIMin != "0.3.60" || len(m.Models) != 1 || m.Models[0].SizeGB != 6.9 || m.Models[0].ModelScope == nil ||
		m.Models[0].HF.Repo != "stabilityai/stable-diffusion-xl-base-1.0" || m.Nodes[0].Commit == "" || len(m.Nodes[0].NodeTypes) != 1 ||
		*m.Inputs[0].Min != 512 || *m.Inputs[0].Step != 64 || m.Inputs[1].Options[0] != "euler" || m.Inputs[2].MaxLen != 2000 ||
		m.Outputs[0].Node != "9" {
		t.Fatalf("decoded = %+v", m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Manifest
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(back)
	if string(again) != string(out) {
		t.Fatalf("round trip differs:\n%s\n%s", out, again)
	}
	// 空清单序列化为空数组而不是 null（前端 / app.sh 不必判空）
	empty, _ := json.Marshal(Manifest{})
	if string(empty) != `{"models":[],"nodes":[],"inputs":[],"outputs":[]}` {
		t.Fatalf("empty manifest = %s", empty)
	}
}
