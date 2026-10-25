package comfywf

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"spark-cloud/internal/platform/apperr"
)

func fixedRnd(v int64) func() int64 { return func() int64 { return v } }

// promptInputs 解出 prompt[node].inputs[input]（UseNumber，保留整数原文）。
func promptInput(t *testing.T, prompt json.RawMessage, node, input string) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(prompt))
	d.UseNumber()
	var p map[string]struct {
		ClassType string         `json:"class_type"`
		Inputs    map[string]any `json:"inputs"`
	}
	if err := d.Decode(&p); err != nil {
		t.Fatal(err)
	}
	n, ok := p[node]
	if !ok {
		t.Fatalf("node %s missing", node)
	}
	return n.Inputs[input]
}

func wantInputInvalid(t *testing.T, err error, label string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != "workflow_input_invalid" || !strings.HasPrefix(ae.Message, label+"：") {
		t.Fatalf("err = %v, want workflow_input_invalid for %q", err, label)
	}
}

func validManifest(t *testing.T) (Manifest, json.RawMessage) {
	t.Helper()
	m := okManifest()
	api := fixture(t, "api_sdxl.json")
	if err := ValidateManifest(&m, fixture(t, "graph_sdxl_ok.json"), api); err != nil {
		t.Fatal(err)
	}
	return m, api
}

func TestBuildPromptDefaults(t *testing.T) {
	m, api := validManifest(t)
	orig := append([]byte(nil), api...)
	prompt, seed, err := BuildPrompt(api, m.Inputs, map[string]any{"image": "spark-quickrun/abcdef012345.png"}, fixedRnd(42))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(api, orig) {
		t.Fatal("apiPrompt mutated")
	}
	if seed == nil || *seed != 42 {
		t.Fatalf("seed = %v", seed)
	}
	checks := map[[2]string]string{
		{"6", "text"}: "a photo of ...", {"7", "text"}: "", {"5", "width"}: "1024", {"3", "steps"}: "25", {"3", "cfg"}: "7",
		{"3", "sampler_name"}: "euler", {"3", "seed"}: "42", {"10", "image"}: "spark-quickrun/abcdef012345.png",
		{"3", "scheduler"}: "normal", {"4", "ckpt_name"}: "sd_xl_base_1.0.safetensors",
	}
	for k, want := range checks {
		got := promptInput(t, prompt, k[0], k[1])
		if s, ok := got.(json.Number); ok {
			got = s.String()
		}
		if got != want {
			t.Errorf("%s.%s = %v, want %v", k[0], k[1], got, want)
		}
	}
	// 连线原样保留。
	if l, ok := promptInput(t, prompt, "3", "model").([]any); !ok || l[0] != "4" {
		t.Fatalf("link = %v", promptInput(t, prompt, "3", "model"))
	}
	// _meta 等其它字段保留。
	if !bytes.Contains(prompt, []byte(`"_meta"`)) {
		t.Fatal("_meta dropped")
	}
}

func TestBuildPromptValues(t *testing.T) {
	m, api := validManifest(t)
	vals := map[string]any{
		"prompt": "a cat", "negative": "dog", "width": 768.0, "height": json.Number("1536"), "steps": 30, "cfg": 4.5,
		"sampler": "dpmpp_2m", "seed": 123456789012345.0, "image": "spark-quickrun/0123456789ab.webp",
	}
	prompt, seed, err := BuildPrompt(api, m.Inputs, vals, func() int64 { t.Fatal("rnd called"); return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if seed == nil || *seed != 123456789012345 {
		t.Fatalf("seed = %v", seed)
	}
	for k, want := range map[[2]string]string{{"6", "text"}: "a cat", {"7", "text"}: "dog", {"5", "width"}: "768", {"5", "height"}: "1536",
		{"3", "steps"}: "30", {"3", "cfg"}: "4.5", {"3", "sampler_name"}: "dpmpp_2m", {"3", "seed"}: "123456789012345"} {
		got := promptInput(t, prompt, k[0], k[1])
		if n, ok := got.(json.Number); ok {
			got = n.String()
		}
		if got != want {
			t.Errorf("%s.%s = %v, want %v", k[0], k[1], got, want)
		}
	}
}

func TestBuildPromptSeed(t *testing.T) {
	m, api := validManifest(t)
	img := "spark-quickrun/abcdef012345.jpg"
	for _, v := range []any{nil, -1.0, "", json.Number("-1")} {
		vals := map[string]any{"image": img, "seed": v}
		_, seed, err := BuildPrompt(api, m.Inputs, vals, fixedRnd(7))
		if err != nil || seed == nil || *seed != 7 {
			t.Fatalf("seed %v: %v %v", v, seed, err)
		}
	}
	// rnd 返回值折进 [0, 2^53)。
	_, seed, err := BuildPrompt(api, m.Inputs, map[string]any{"image": img}, fixedRnd(-5))
	if err != nil || *seed < 0 || *seed >= 1<<53 {
		t.Fatalf("seed = %v, %v", *seed, err)
	}
	_, seed, err = BuildPrompt(api, m.Inputs, map[string]any{"image": img}, nil) // nil rnd → 内置随机
	if err != nil || *seed < 0 || *seed >= 1<<53 {
		t.Fatalf("seed = %v, %v", seed, err)
	}
	// 种子 0 合法。
	_, seed, _ = BuildPrompt(api, m.Inputs, map[string]any{"image": img, "seed": 0.0}, fixedRnd(9))
	if *seed != 0 {
		t.Fatalf("seed = %d", *seed)
	}
	// 数字字符串也接受。
	for v, want := range map[string]int64{"-1": 7, "123": 123, " 5 ": 5} {
		_, seed, err := BuildPrompt(api, m.Inputs, map[string]any{"image": img, "seed": v}, fixedRnd(7))
		if err != nil || *seed != want {
			t.Fatalf("seed %q: %v %v", v, seed, err)
		}
	}
	for _, v := range []any{-2.0, 1.5, "abc", "1.5", "-2", float64(1 << 53), true} {
		_, _, err := BuildPrompt(api, m.Inputs, map[string]any{"image": img, "seed": v}, fixedRnd(1))
		wantInputInvalid(t, err, "种子")
	}
	// 默认种子（非 -1）用默认值。
	in := append([]InputSpec(nil), m.Inputs...)
	in[7].Default = 99.0
	_, seed, _ = BuildPrompt(api, in, map[string]any{"image": img}, fixedRnd(1))
	if *seed != 99 {
		t.Fatalf("seed = %d", *seed)
	}
	// 没有种子类输入 → seed = nil。
	_, seed, err = BuildPrompt(api, m.Inputs[:2], nil, fixedRnd(1))
	if err != nil || seed != nil {
		t.Fatalf("seed = %v, %v", seed, err)
	}
}

func TestBuildPromptInvalid(t *testing.T) {
	m, api := validManifest(t)
	img := "spark-quickrun/abcdef012345.png"
	cases := []struct {
		name  string
		vals  map[string]any
		label string
	}{
		{"image required", map[string]any{}, "参考图"},
		{"image bad name", map[string]any{"image": "../../etc/passwd"}, "参考图"},
		{"image other folder", map[string]any{"image": "input/abcdef012345.png"}, "参考图"},
		{"image bad ext", map[string]any{"image": "spark-quickrun/abcdef012345.gif"}, "参考图"},
		{"image upper", map[string]any{"image": "spark-quickrun/ABCDEF012345.png"}, "参考图"},
		{"image not string", map[string]any{"image": 1.0}, "参考图"},
		{"unknown key", map[string]any{"image": img, "lora": "x"}, "lora"},
		{"text too long", map[string]any{"image": img, "negative": strings.Repeat("字", 1001)}, "反向提示词"},
		{"text NUL", map[string]any{"image": img, "prompt": "a\x00"}, "提示词"},
		{"text not string", map[string]any{"image": img, "prompt": 3.0}, "提示词"},
		{"int fractional", map[string]any{"image": img, "steps": 2.5}, "步数"},
		{"int below", map[string]any{"image": img, "steps": 0.0}, "步数"},
		{"int above", map[string]any{"image": img, "steps": 101}, "步数"},
		{"int unaligned", map[string]any{"image": img, "width": 1000.0}, "宽"},
		{"int string", map[string]any{"image": img, "width": "1024"}, "宽"},
		{"float above", map[string]any{"image": img, "cfg": 30.5}, "CFG"},
		{"float not number", map[string]any{"image": img, "cfg": "7"}, "CFG"},
		{"enum not in options", map[string]any{"image": img, "sampler": "ddim"}, "采样器"},
		{"enum not string", map[string]any{"image": img, "sampler": 1.0}, "采样器"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := BuildPrompt(api, m.Inputs, c.vals, fixedRnd(1))
			wantInputInvalid(t, err, c.label)
		})
	}
	// 无默认的非种子输入缺值 → 必填。
	in := append([]InputSpec(nil), m.Inputs...)
	in[0].Default = nil
	_, _, err := BuildPrompt(api, in, map[string]any{"image": img}, fixedRnd(1))
	wantInputInvalid(t, err, "提示词")
	if !strings.Contains(err.Error(), "必填") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildPromptBool(t *testing.T) {
	api := json.RawMessage(`{"1":{"class_type":"X","inputs":{"on":false,"n":1}}}`)
	in := []InputSpec{{Key: "on", Label: "开关", Type: InputBool, Node: "1", Input: "on", Default: false}}
	p, _, err := BuildPrompt(api, in, map[string]any{"on": true}, nil)
	if err != nil || promptInput(t, p, "1", "on") != true {
		t.Fatalf("p = %s, err = %v", p, err)
	}
	p, _, _ = BuildPrompt(api, in, nil, nil)
	if promptInput(t, p, "1", "on") != false {
		t.Fatalf("p = %s", p)
	}
	_, _, err = BuildPrompt(api, in, map[string]any{"on": "true"}, nil)
	wantInputInvalid(t, err, "开关")
	// 目标节点 / 输入不存在（清单未校验时的兜底）。
	_, _, err = BuildPrompt(api, []InputSpec{{Key: "x", Label: "X", Type: InputBool, Node: "2", Input: "on", Default: true}}, nil, nil)
	if err == nil {
		t.Fatal("missing node accepted")
	}
	_, _, err = BuildPrompt(json.RawMessage(`[`), in, nil, nil)
	if err == nil {
		t.Fatal("bad api accepted")
	}
}

// step 从 min 起对齐：Wan22ImageToVideoLatent 的 length（min 1、step 4）→ 121 合法、120 不合法。
func TestBuildPromptStepFromMin(t *testing.T) {
	api := json.RawMessage(`{"55":{"class_type":"Wan22ImageToVideoLatent","inputs":{"width":1280,"height":704,"length":121,"batch_size":1,"vae":["39",0]}}}`)
	in := []InputSpec{{Key: "length", Label: "帧数", Type: InputInt, Node: "55", Input: "length", Default: 121.0, Min: fp(1), Max: fp(241), Step: fp(4)}}
	m := Manifest{Inputs: in}
	if err := ValidateManifest(&m, graphWith(), api); err != nil {
		t.Fatal(err)
	}
	for _, v := range []float64{1, 5, 121, 241} {
		if _, _, err := BuildPrompt(api, in, map[string]any{"length": v}, nil); err != nil {
			t.Fatalf("%v: %v", v, err)
		}
	}
	for _, v := range []float64{120, 4, 0, 245} {
		_, _, err := BuildPrompt(api, in, map[string]any{"length": v}, nil)
		wantInputInvalid(t, err, "帧数")
	}
}
