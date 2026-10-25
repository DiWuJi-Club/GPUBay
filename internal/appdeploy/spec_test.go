package appdeploy_test

import (
	"encoding/json"
	"strings"
	"testing"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/platform/apperr"
)

func i64(v int64) *int64 { return &v }

func goodSpec() appdeploy.Spec {
	return appdeploy.Spec{
		Ports: []appdeploy.PortSpec{{Port: 8000, Label: "API", Path: "/v1", HealthPath: "/health"}},
		Env: []appdeploy.EnvSpec{
			{Name: "MODEL_ID", Label: "模型", Type: "enum", Required: true, Options: []string{"Qwen/Qwen3-8B", "Qwen/Qwen3-4B"}, Default: "Qwen/Qwen3-8B"},
			{Name: "MAX_MODEL_LEN", Label: "最大上下文", Type: "int", Min: i64(1024), Max: i64(131072), Default: "32768"},
			{Name: "API_KEY", Label: "接口密钥", Type: "secret", Pattern: "^[A-Za-z0-9_-]{8,128}$"},
			{Name: "NOTE", Label: "备注", Type: "string"},
			{Name: "DEBUG", Label: "调试", Type: "bool", Default: "false"},
		},
		Links: []appdeploy.LinkSpec{{Label: "文档", Path: "/docs"}},
	}
}

func TestValidateSpecMatrix(t *testing.T) {
	ok := goodSpec()
	if err := appdeploy.ValidateSpec(&ok); err != nil {
		t.Fatalf("good spec rejected: %v", err)
	}
	if ok.TimeoutMinutes != 60 || ok.Env[3].MaxLen != 256 {
		t.Fatalf("defaults not applied: %+v", ok)
	}
	// enum 选项按字符计：128 个汉字合法
	cjk := goodSpec()
	cjk.Env[0].MaxLen = 1024 // 128 个汉字 = 384 字节，须放得下（选项也要过 max_len）
	cjk.Env[0].Options = append(cjk.Env[0].Options, strings.Repeat("模", 128))
	if err := appdeploy.ValidateSpec(&cjk); err != nil {
		t.Fatalf("128-char option rejected: %v", err)
	}
	cases := map[string]struct {
		mut  func(s *appdeploy.Spec)
		path string
	}{
		"no ports": {func(s *appdeploy.Spec) { s.Ports = nil }, "ports"},
		"three ports": {func(s *appdeploy.Spec) {
			s.Ports = append(s.Ports, appdeploy.PortSpec{Port: 1}, appdeploy.PortSpec{Port: 2})
		}, "ports"},
		"port 22":          {func(s *appdeploy.Spec) { s.Ports[0].Port = 22 }, "ports[0].port"},
		"port 2222":        {func(s *appdeploy.Spec) { s.Ports[0].Port = 2222 }, "ports[0].port"},
		"port low":         {func(s *appdeploy.Spec) { s.Ports[0].Port = 1023 }, "ports[0].port"},
		"port dup":         {func(s *appdeploy.Spec) { s.Ports = append(s.Ports, appdeploy.PortSpec{Port: 8000}) }, "ports[1].port"},
		"path no slash":    {func(s *appdeploy.Spec) { s.Ports[0].Path = "v1" }, "ports[0].path"},
		"label long":       {func(s *appdeploy.Spec) { s.Ports[0].Label = strings.Repeat("x", 33) }, "ports[0].label"},
		"timeout high":     {func(s *appdeploy.Spec) { s.TimeoutMinutes = 181 }, "timeout_minutes"},
		"timeout neg":      {func(s *appdeploy.Spec) { s.TimeoutMinutes = -1 }, "timeout_minutes"},
		"disk high":        {func(s *appdeploy.Spec) { s.MinDataFreeGiB = 2049 }, "min_data_free_gib"},
		"env name lower":   {func(s *appdeploy.Spec) { s.Env[0].Name = "model" }, "env[0].name"},
		"env name SPARK_":  {func(s *appdeploy.Spec) { s.Env[0].Name = "SPARK_X" }, "env[0].name"},
		"env name PATH":    {func(s *appdeploy.Spec) { s.Env[0].Name = "PATH" }, "env[0].name"},
		"env name LD":      {func(s *appdeploy.Spec) { s.Env[0].Name = "LD_PRELOAD" }, "env[0].name"},
		"env dup":          {func(s *appdeploy.Spec) { s.Env[3].Name = "MODEL_ID" }, "env[3].name"},
		"env type":         {func(s *appdeploy.Spec) { s.Env[0].Type = "list" }, "env[0].type"},
		"enum no options":  {func(s *appdeploy.Spec) { s.Env[0].Options = nil }, "env[0].options"},
		"enum 33 options":  {func(s *appdeploy.Spec) { s.Env[0].Options = make([]string, 33) }, "env[0].options"},
		"enum bad default": {func(s *appdeploy.Spec) { s.Env[0].Default = "x" }, "env[0].default"},
		"int bad default":  {func(s *appdeploy.Spec) { s.Env[1].Default = "99" }, "env[1].default"},
		"int min>max":      {func(s *appdeploy.Spec) { s.Env[1].Min, s.Env[1].Max = i64(5), i64(1) }, "env[1].min"},
		"bad RE2":          {func(s *appdeploy.Spec) { s.Env[2].Pattern = "(" }, "env[2].pattern"},
		"pattern long":     {func(s *appdeploy.Spec) { s.Env[2].Pattern = strings.Repeat("a", 257) }, "env[2].pattern"},
		"max_len high":     {func(s *appdeploy.Spec) { s.Env[3].MaxLen = 1025 }, "env[3].max_len"},
		"17 env": {func(s *appdeploy.Spec) {
			for i := 0; i < 12; i++ {
				s.Env = append(s.Env, appdeploy.EnvSpec{Name: "V" + strings.Repeat("A", i+1), Type: "string"})
			}
		}, "env"},
		"help long":                   {func(s *appdeploy.Spec) { s.Env[0].Help = strings.Repeat("x", 201) }, "env[0].help"},
		"5 links":                     {func(s *appdeploy.Spec) { s.Links = make([]appdeploy.LinkSpec, 5) }, "links"},
		"link path":                   {func(s *appdeploy.Spec) { s.Links[0].Path = "docs" }, "links[0].path"},
		"enum option long":            {func(s *appdeploy.Spec) { s.Env[0].Options = append(s.Env[0].Options, strings.Repeat("模", 129)) }, "env[0].options"},
		"env name LD_ prefix":         {func(s *appdeploy.Spec) { s.Env[0].Name = "LD_AUDIT" }, "env[0].name"},
		"env name UID":                {func(s *appdeploy.Spec) { s.Env[0].Name = "UID" }, "env[0].name"},
		"env name EUID":               {func(s *appdeploy.Spec) { s.Env[0].Name = "EUID" }, "env[0].name"},
		"env name PPID":               {func(s *appdeploy.Spec) { s.Env[0].Name = "PPID" }, "env[0].name"},
		"env name SHELLOPTS":          {func(s *appdeploy.Spec) { s.Env[0].Name = "SHELLOPTS" }, "env[0].name"},
		"secret default":              {func(s *appdeploy.Spec) { s.Env[2].Default = "abcdefgh12" }, "env[2].default"},
		"enum option newline":         {func(s *appdeploy.Spec) { s.Env[0].Options = append(s.Env[0].Options, "a\nb") }, "env[0].options"},
		"enum option CR":              {func(s *appdeploy.Spec) { s.Env[0].Options = append(s.Env[0].Options, "a\rb") }, "env[0].options"},
		"enum option NUL":             {func(s *appdeploy.Spec) { s.Env[0].Options = append(s.Env[0].Options, "a\x00b") }, "env[0].options"},
		"enum option utf8":            {func(s *appdeploy.Spec) { s.Env[0].Options = append(s.Env[0].Options, "\xff") }, "env[0].options"},
		"enum option > max_len":       {func(s *appdeploy.Spec) { s.Env[0].MaxLen = 10 }, "env[0].options"},
		"secret pattern allows short": {func(s *appdeploy.Spec) { s.Env[2].Pattern = "^.{1,4}$" }, "env[2].pattern"},
	}
	for name, c := range cases {
		s := goodSpec()
		c.mut(&s)
		err := appdeploy.ValidateSpec(&s)
		ae := apperr.As(err)
		if ae == nil || ae.Code != "app_version_invalid" || !strings.HasPrefix(ae.Message, c.path+"：") {
			t.Errorf("%s: err = %v, want app_version_invalid at %s", name, err, c.path)
		}
	}
}

func TestSpecHashIgnoresKeyOrder(t *testing.T) {
	// 同一 spec 的两份 JSON（如 JSONB 回读后键序改变），顶层与嵌套对象的键序都不同。
	docA := `{"ports":[{"port":8000,"label":"API","path":"/v1","health_path":"/health"}],"timeout_minutes":60,` +
		`"env":[{"name":"MODEL_ID","type":"enum","options":["a","b"],"default":"a","min":1}],"links":[{"label":"文档","path":"/docs"}]}`
	docB := `{"links":[{"path":"/docs","label":"文档"}],"env":[{"min":1,"default":"a","options":["a","b"],"type":"enum","name":"MODEL_ID"}],` +
		`"timeout_minutes":60,"ports":[{"health_path":"/health","path":"/v1","label":"API","port":8000}]}`
	var a, b appdeploy.Spec
	if err := json.Unmarshal([]byte(docA), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(docB), &b); err != nil {
		t.Fatal(err)
	}
	if appdeploy.SpecHash(a, "x") != appdeploy.SpecHash(b, "x") {
		t.Fatal("same spec with different key order hashes differently")
	}
	if appdeploy.SpecHash(a, "x") == appdeploy.SpecHash(a, "y") {
		t.Fatal("script change not in hash")
	}
	// 真实改动：默认值、端口、选项顺序都要改变 hash
	c := docB
	for _, mut := range []struct{ from, to string }{
		{`"default":"a"`, `"default":"b"`},
		{`"port":8000`, `"port":8001`},
		{`["a","b"]`, `["b","a"]`},
	} {
		var m appdeploy.Spec
		if err := json.Unmarshal([]byte(strings.Replace(c, mut.from, mut.to, 1)), &m); err != nil {
			t.Fatal(err)
		}
		if appdeploy.SpecHash(m, "x") == appdeploy.SpecHash(a, "x") {
			t.Fatalf("spec change %s → %s not in hash", mut.from, mut.to)
		}
	}
	if h := appdeploy.SpecHash(a, "x"); len(h) != 64 {
		t.Fatalf("hash = %q", h)
	}
}

// 计划 11：supports_workflows 往返；false 时省略（既有版本的 spec JSON 与 spec_hash 不变），true 时改变 hash；ValidateSpec 不加约束。
func TestSpecSupportsWorkflows(t *testing.T) {
	s := goodSpec()
	if err := appdeploy.ValidateSpec(&s); err != nil {
		t.Fatal(err)
	}
	before := appdeploy.SpecHash(s, "x")
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "supports_workflows") {
		t.Fatalf("false must be omitted: %s", b)
	}
	s.SupportsWorkflows = true
	if err := appdeploy.ValidateSpec(&s); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(s)
	if !strings.Contains(string(b), `"supports_workflows":true`) {
		t.Fatalf("spec json = %s", b)
	}
	var back appdeploy.Spec
	if err := json.Unmarshal(b, &back); err != nil || !back.SupportsWorkflows {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if appdeploy.SpecHash(s, "x") == before {
		t.Fatal("supports_workflows must change spec_hash")
	}
	s.SupportsWorkflows = false
	if appdeploy.SpecHash(s, "x") != before {
		t.Fatal("false changed spec_hash of existing versions")
	}
}
