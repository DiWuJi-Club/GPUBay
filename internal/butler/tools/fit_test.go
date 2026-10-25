package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"spark-cloud/internal/appdeploy"
)

// 超限时降级字段而不是截字节：结果始终是 ≤ 上限的合法 JSON。

func TestGetAppManyEnumOptionsStaysValidJSON(t *testing.T) {
	f := newFakes()
	var env []appdeploy.EnvSpec
	for i := range 16 {
		var opts []string
		for j := range 32 {
			opts = append(opts, fmt.Sprintf("option-%02d-%02d-%s", i, j, strings.Repeat("x", 26))) // 40 字符
		}
		env = append(env, appdeploy.EnvSpec{Name: fmt.Sprintf("VAR_%02d", i), Type: "enum", Required: true, Options: opts,
			Help: strings.Repeat("帮助", 300)})
	}
	app := f.apps.catalog[0]
	app.Description = strings.Repeat("说明文字", 1000)
	app.Version.Env = env
	f.apps.catalog[0] = app
	res, err := run(t, NewRegistry(f.deps), "get_app", ownerID, `{"slug":"vllm"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) > largeLimit {
		t.Fatalf("len %d > %d", len(res.Content), largeLimit)
	}
	v := unwrapJSON(t, "get_app", ownerID, res.Content)
	envs := arr(t, v["version"], "env")
	if len(envs) != 16 {
		t.Fatalf("all variables must be kept: %d", len(envs))
	}
	for _, e := range envs {
		opts := e["options"].([]any)
		if len(opts) == 0 || len(opts) >= 32 || e["options_truncated"] != true || e["options_total"] != float64(32) || e["required"] != true {
			t.Fatalf("env = %v", e)
		}
		if !strings.HasPrefix(opts[0].(string), "option-") {
			t.Fatalf("first option = %v", opts[0])
		}
	}
	// 选项少的应用不带截断标记、非 enum 变量不带 options
	res, _ = run(t, NewRegistry(newFakes().deps), "get_app", ownerID, `{"slug":"vllm"}`)
	for _, e := range arr(t, unwrapJSON(t, "get_app", ownerID, res.Content)["version"], "env") {
		if _, has := e["options_truncated"]; has {
			t.Fatalf("unexpected truncation flag: %v", e)
		}
		if _, has := e["options"]; has {
			t.Fatalf("non-enum variable has options: %v", e)
		}
	}
}

func TestFitJSONGenericDegrade(t *testing.T) {
	c := Caller{UserID: ownerID}
	big := map[string]any{"title": strings.Repeat("长", 3000), "items": make([]any, 0), "n": 1}
	for i := range 500 {
		big["items"] = append(big["items"].([]any), map[string]any{"id": i, "note": strings.Repeat("n", 100)})
	}
	s, err := fitJSON("x", c, big, smallLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) > smallLimit {
		t.Fatalf("len %d", len(s))
	}
	v := unwrapJSON(t, "x", ownerID, s)
	if v["n"] != float64(1) || v["items_truncated"] != true || !strings.HasSuffix(v["title"].(string), truncMark) {
		t.Fatalf("v = %.300v", v)
	}
	// 键多到无法降级：固定的截断提示，仍是合法 JSON
	many := map[string]any{}
	for i := range 2000 {
		many[fmt.Sprintf("key_%04d", i)] = i
	}
	s, err = fitJSON("x", c, many, smallLimit)
	if err != nil {
		t.Fatal(err)
	}
	if v := unwrapJSON(t, "x", ownerID, s); v["truncated"] != true {
		t.Fatalf("v = %v", v)
	}
	// 放得下时原样输出
	s, _ = fitJSON("x", c, map[string]any{"a": "b"}, smallLimit)
	if s != "<untrusted_data source=\"x\" user=\"7\">\n{\"a\":\"b\"}\n</untrusted_data>" {
		t.Fatalf("s = %q", s)
	}
}

func TestHalveStringTerminates(t *testing.T) {
	s := strings.Repeat("中", 50)
	for range 20 {
		h := halveString(s)
		if !strings.HasSuffix(h, truncMark) || strings.Count(h, truncMark) != 1 {
			t.Fatalf("h = %q", h)
		}
		s = h
	}
	if s != truncMark {
		t.Fatalf("final = %q", s)
	}
}

// 日志全是需要转义的字符（`"\n` 转义后翻倍）时，按实测比例收紧，仍保留约一半预算的日志。
func TestDeploymentLogEscapeHeavy(t *testing.T) {
	f := newFakes()
	f.apps.logs["dep-vllm"] = appdeploy.LogResult{Log: strings.Repeat("\"\n", 20*1024), At: testNow} // 40KB
	res, err := run(t, NewRegistry(f.deps), "get_deployment_log", ownerID, `{"instance_id":"ins-run","deployment_id":"dep-vllm"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) > largeLimit {
		t.Fatalf("len %d > %d", len(res.Content), largeLimit)
	}
	v := unwrapJSON(t, "get_deployment_log", ownerID, res.Content)
	log := v["log"].(string)
	if n := strings.Count(log, "\"\n"); n*2 < 7800 {
		t.Fatalf("kept only %d bytes of log (content %d bytes)", n*2, len(res.Content))
	}
	if v["omitted_bytes"].(float64) <= 0 {
		t.Fatalf("omitted = %v", v["omitted_bytes"])
	}
}

func TestGetInstanceScheduledEnd(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	v := mustRun(t, r, "get_instance", `{"instance_id":"ins-res"}`)
	// 开始 2026-10-27 10:00 + 待开始 86400 秒
	if v["scheduled_end_at"] != "2026-10-28T10:00:00+08:00" {
		t.Fatalf("scheduled_end_at = %v", v["scheduled_end_at"])
	}
	if v := mustRun(t, r, "get_instance", `{"instance_id":"ins-run"}`); v["scheduled_end_at"] != nil {
		t.Fatalf("immediate instance scheduled_end_at = %v", v["scheduled_end_at"])
	}
}

func TestListOrdersClamp(t *testing.T) {
	f := newFakes()
	for i := range 30 {
		o := f.rental.orders[0]
		o.ID = fmt.Sprintf("ord-%02d", i)
		f.rental.orders = append(f.rental.orders, o)
	}
	tool, _ := NewRegistry(f.deps).Get("list_orders")
	// 绕过 schema 直接传超限的 limit：执行层仍钳到 20（超出 4KB 时再按大小截断）
	res, err := tool.Exec(context.Background(), Caller{UserID: ownerID}, json.RawMessage(`{"limit":50}`))
	if err != nil {
		t.Fatal(err)
	}
	m := unwrapJSON(t, "list_orders", ownerID, res.Content)["meta"].(map[string]any)
	if m["total"] != float64(20) {
		t.Fatalf("meta = %v", m)
	}
}
