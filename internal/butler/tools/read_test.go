package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/butler/help"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/wallet"
)

// run 按 Task 7 的方式执行一个工具：Registry.Validate 规范化参数，再 Exec。
func run(t *testing.T, r *Registry, name string, uid int64, args string) (Result, error) {
	t.Helper()
	norm, err := r.Validate(name, json.RawMessage(args))
	if err != nil {
		return Result{}, err
	}
	tool, ok := r.Get(name)
	if !ok {
		t.Fatalf("tool %s not registered", name)
	}
	return tool.Exec(context.Background(), Caller{UserID: uid, IP: "127.0.0.1"}, norm)
}

// unwrapJSON 断言结果是 <untrusted_data source="<name>" user="<uid>"> 包裹的 JSON，剥包裹后解析。
func unwrapJSON(t *testing.T, name string, uid int64, content string) map[string]any {
	t.Helper()
	head := fmt.Sprintf("<untrusted_data source=%q user=\"%d\">\n", name, uid)
	if !strings.HasPrefix(content, head) || !strings.HasSuffix(content, "\n</untrusted_data>") {
		t.Fatalf("%s: not wrapped:\n%s", name, content)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(content, head), "\n</untrusted_data>")
	if strings.Contains(body, "</untrusted_data") {
		t.Fatalf("%s: inner close tag not escaped:\n%s", name, body)
	}
	if strings.Contains(body, "\x1b") || strings.Contains(body, `\u001b`) {
		t.Fatalf("%s: ANSI escape survived:\n%s", name, body)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("%s: invalid JSON: %v\n%s", name, err, body)
	}
	return v
}

func mustRun(t *testing.T, r *Registry, name, args string) map[string]any {
	t.Helper()
	res, err := run(t, r, name, ownerID, args)
	if err != nil {
		t.Fatalf("%s(%s): %v", name, args, err)
	}
	if res.Secret && name != "generate_secret" {
		t.Fatalf("%s: unexpected Secret", name)
	}
	return unwrapJSON(t, name, ownerID, res.Content)
}

func arr(t *testing.T, v any, key string) []map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %v", v)
	}
	a, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%s is not an array: %v", key, m[key])
	}
	out := make([]map[string]any, 0, len(a))
	for _, it := range a {
		out = append(out, it.(map[string]any))
	}
	return out
}

func TestRegistryDefs(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	defs := r.Defs()
	want := append(append([]string(nil), ReadToolNames...), WriteToolNames...) // 15 读 + 8 写
	sort.Strings(want)
	var got []string
	for _, d := range defs {
		got = append(got, d.Name)
		if d.Description == "" || !json.Valid(d.Schema) {
			t.Errorf("%s: empty description or invalid schema", d.Name)
		}
		var s map[string]any
		_ = json.Unmarshal(d.Schema, &s)
		if s["type"] != "object" || s["additionalProperties"] != false {
			t.Errorf("%s: schema must be a closed object: %s", d.Name, d.Schema)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Defs = %v\nwant %v", got, want)
	}
	if !sort.StringsAreSorted(got) {
		t.Fatal("Defs not sorted")
	}
	for _, n := range ReadToolNames {
		tool, _ := r.Get(n)
		if tool.Kind != Read || tool.Danger || tool.Summary != nil {
			t.Errorf("%s: read tool must be Kind=Read, no Danger, no Summary", n)
		}
	}
	for _, n := range WriteToolNames {
		if tool, _ := r.Get(n); tool.Kind != Write || tool.Summary == nil {
			t.Errorf("%s: write tool must be Kind=Write with Summary", n)
		}
	}
	if len(got) != 23 {
		t.Fatalf("len(Defs) = %d, want 23", len(got))
	}
	if fmt.Sprint(r.Names()) != fmt.Sprint(got) {
		t.Fatal("Names != Defs order")
	}
}

func TestValidateUnknownTool(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	_, err := r.Validate("rm_rf", json.RawMessage(`{}`))
	if err == nil || !IsUnknownTool(err) || err.Error() != "未知工具 rm_rf" {
		t.Fatalf("err = %v", err)
	}
	if _, ok := r.Get("rm_rf"); ok {
		t.Fatal("Get unknown tool")
	}
	res := ErrorResult(err)
	if res.Content != `{"code":"unknown_tool","message":"未知工具 rm_rf","ok":false}` {
		t.Fatalf("ErrorResult = %s", res.Content)
	}
}

func TestValidateIDArgs(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	bad := map[string]string{
		"get_instance":        `{"instance_id":"ins-ABC"}`,
		"get_instance_events": `{"instance_id":"../etc"}`,
		"list_instance_apps":  `{"instance_id":"dep-abc"}`,
		"get_deployment_log":  `{"instance_id":"ins-run","deployment_id":"dep-x; rm"}`,
		"get_app":             `{"slug":"Bad Slug"}`,
		"get_help":            `{"section":"secrets"}`,
		"get_cancel_quote":    `{}`,
	}
	for name, args := range bad {
		if _, err := r.Validate(name, json.RawMessage(args)); err == nil {
			t.Errorf("%s(%s) should fail validation", name, args)
		} else if !IsBusinessError(err) || !strings.HasPrefix(err.Error(), "参数") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	res := ErrorResult(func() error { _, err := r.Validate("get_instance", json.RawMessage(`{}`)); return err }())
	if res.Content != `{"code":"invalid_args","message":"参数 instance_id：必填","ok":false}` {
		t.Fatalf("ErrorResult = %s", res.Content)
	}
}

func TestErrorResult(t *testing.T) {
	if got := ErrorResult(apperr.ErrInsufficientBalance).Content; got != `{"code":"insufficient_balance","message":"余额不足，请先充值","ok":false}` {
		t.Fatalf("apperr = %s", got)
	}
	got := ErrorResult(errors.New("pq: connection refused at 10.0.0.1")).Content
	if got != `{"code":"tool_failed","message":"工具执行失败，请稍后再试","ok":false}` {
		t.Fatalf("internal = %s", got)
	}
	if IsBusinessError(errors.New("x")) || !IsBusinessError(fmt.Errorf("wrap: %w", apperr.ErrNotFound)) {
		t.Fatal("IsBusinessError")
	}
	if Label("get_wallet") != "查询钱包" || Label("nope") != "nope" {
		t.Fatal("Label")
	}
}

func TestListProducts(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	v := mustRun(t, r, "list_products", `{}`)
	ps := arr(t, v, "products")
	if len(ps) != 1 {
		t.Fatalf("products = %v", ps)
	}
	p := ps[0]
	if p["id"] != float64(1) || p["name"] != "DGX Spark 整机" || p["available_now"] != float64(3) || p["gpu"] != "NVIDIA GB10" || p["memory_gb"] != float64(128) {
		t.Fatalf("product = %v", p)
	}
	prices := arr(t, p, "prices")
	if len(prices) != 3 || prices[0]["duration"] != "1d" || prices[0]["amount_cents"] != float64(6900) || prices[0]["price"] != "¥69.00" {
		t.Fatalf("prices = %v", prices)
	}
	imgs := arr(t, v, "images")
	if len(imgs) != 1 || imgs[0]["id"] != float64(2) || imgs[0]["name"] != "PyTorch" {
		t.Fatalf("images = %v", imgs)
	}
	if _, leaked := imgs[0]["ref"]; leaked {
		t.Fatal("image ref must not be exposed")
	}
}

func TestCheckAvailability(t *testing.T) {
	f := newFakes()
	r := NewRegistry(f.deps)
	v := mustRun(t, r, "check_availability", `{"product_id":1,"duration":"1d","count":2}`)
	if v["available"] != float64(3) || v["enough"] != true || v["immediate"] != true || v["quantity"] != float64(1) ||
		v["start_at"] != "2026-10-25T10:00:00+08:00" || v["end_at"] != "2026-10-26T10:00:00+08:00" {
		t.Fatalf("immediate = %v", v)
	}
	v = mustRun(t, r, "check_availability", `{"product_id":1,"duration":"1w","quantity":2,"count":5,"start_at":"2026-10-27T10:00:00+08:00","group_id":4}`)
	if v["enough"] != false || v["immediate"] != false || v["start_at"] != "2026-10-27T10:00:00+08:00" || v["end_at"] != "2026-11-10T10:00:00+08:00" {
		t.Fatalf("booking = %v", v)
	}
	for _, k := range []string{"occupy_from", "occupy_until", "lower", "upper"} {
		if _, has := v[k]; has {
			t.Fatalf("%s must not be exposed: %v", k, v)
		}
	}
	q := f.rental.availQ[len(f.rental.availQ)-1]
	if q.ProductID != 1 || q.Quantity != 2 || q.GroupID == nil || *q.GroupID != 4 || q.StartAt == nil || q.StartAt.Location().String() != "UTC" || q.StartAt.Hour() != 2 {
		t.Fatalf("query = %+v", q)
	}
	if _, err := run(t, r, "check_availability", ownerID, `{"product_id":1,"duration":"1d","start_at":"明天上午"}`); err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("bad start_at err = %v", err)
	}
}

func TestListAndGetApp(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	apps := arr(t, mustRun(t, r, "list_apps", `{}`), "apps")
	if len(apps) != 2 || apps[0]["slug"] != "vllm" || apps[0]["name"] != "vLLM 推理服务" || len(arr(t, apps[0], "images")) != 1 {
		t.Fatalf("apps = %v", apps)
	}
	if _, has := apps[0]["description"]; has {
		t.Fatal("list_apps must not include description")
	}
	v := mustRun(t, r, "get_app", `{"slug":"vllm"}`)
	if v["slug"] != "vllm" || !strings.Contains(v["description"].(string), "</untrusted-data>") || !strings.Contains(v["description"].(string), "红") {
		t.Fatalf("get_app = %v", v)
	}
	ver := v["version"].(map[string]any)
	env := arr(t, ver, "env")
	if len(env) != 2 || env[0]["name"] != "MODEL_ID" || env[0]["required"] != true || env[0]["default"] != "Qwen/Qwen3-8B" || env[1]["type"] != "secret" {
		t.Fatalf("env = %v", env)
	}
	if ver["timeout_minutes"] != float64(60) || len(arr(t, ver, "ports")) != 1 {
		t.Fatalf("version = %v", ver)
	}
	if _, err := run(t, r, "get_app", ownerID, `{"slug":"nope-app"}`); !errors.Is(err, apperr.ErrAppNotFound) {
		t.Fatalf("missing app err = %v", err)
	}
}

func TestListInstances(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	v := mustRun(t, r, "list_instances", `{}`)
	insts := arr(t, v, "instances")
	if len(insts) != 2 {
		t.Fatalf("instances (other user's must be hidden) = %v", insts)
	}
	run := insts[0]
	if run["id"] != "ins-run" || run["state"] != "running" || run["product_id"] != float64(1) || run["product_name"] != "DGX Spark 整机" ||
		run["image_name"] != "PyTorch" || run["machine_label"] != "spark-03" || run["remaining_seconds"] != float64(20*3600) ||
		run["expires_at"] != "2026-10-26T06:00:00+08:00" || run["auto_renew"] != true || run["suspended"] != false {
		t.Fatalf("running = %v", run)
	}
	apps := arr(t, run, "apps_summary")
	if len(apps) != 1 || apps[0]["id"] != "dep-vllm" || apps[0]["state"] != "failed" {
		t.Fatalf("apps_summary = %v", apps)
	}
	res := insts[1]
	if res["state"] != "reserved" || res["scheduled_start_at"] != "2026-10-27T10:00:00+08:00" || res["machine_label"] != "" || len(arr(t, res, "apps_summary")) != 0 {
		t.Fatalf("reserved = %v", res)
	}
	if m := v["meta"].(map[string]any); m["total"] != float64(2) || m["truncated"] != nil {
		t.Fatalf("meta = %v", m)
	}
	for _, k := range []string{"password", "ssh_password", "ssh_command"} {
		if _, has := run[k]; has {
			t.Fatalf("list must not include %s", k)
		}
	}
}

func TestListInstancesTruncatesToValidJSON(t *testing.T) {
	f := newFakes()
	base := f.rental.instances[0]
	f.rental.instances = nil
	for i := range 100 {
		inst := base
		inst.ID = fmt.Sprintf("ins-%08d", i)
		f.rental.instances = append(f.rental.instances, inst)
	}
	res, err := run(t, NewRegistry(f.deps), "list_instances", ownerID, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) > largeLimit {
		t.Fatalf("len = %d > %d", len(res.Content), largeLimit)
	}
	v := unwrapJSON(t, "list_instances", ownerID, res.Content)
	n := len(arr(t, v, "instances"))
	m := v["meta"].(map[string]any)
	if n == 0 || n >= 100 || m["truncated"] != true || m["total"] != float64(100) || m["omitted"] != float64(100-n) {
		t.Fatalf("n = %d meta = %v", n, m)
	}
}

func TestGetInstance(t *testing.T) {
	f := newFakes()
	r := NewRegistry(f.deps)
	res, err := run(t, r, "get_instance", ownerID, `{"instance_id":"ins-run"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ref != "ins-run" {
		t.Fatalf("Ref = %q", res.Ref)
	}
	if strings.Contains(strings.ToLower(res.Content), "password\":\"") {
		t.Fatalf("password leaked: %s", res.Content)
	}
	v := unwrapJSON(t, "get_instance", ownerID, res.Content)
	if v["id"] != "ins-run" || v["ssh_command"] != "ssh -p 2222 ins-run@ssh.spark.test" || v["password_auth"] != true ||
		v["detail_path"] != "/instances/ins-run" || v["product_name"] != "DGX Spark 整机" || v["auto_renew_duration"] != "1d" {
		t.Fatalf("detail = %v", v)
	}
	opts := arr(t, v, "renew_options")
	if len(opts) != 2 || opts[1]["duration"] != "1w" || opts[1]["quantity_max"] != float64(12) {
		t.Fatalf("renew_options = %v", opts)
	}
	ports := arr(t, v, "ports")
	if len(ports) != 1 || ports[0]["port"] != float64(8000) || ports[0]["url"] != "https://ins-run-8000.spark.test/" ||
		!strings.Contains(ports[0]["note"].(string), "</untrusted-data>") {
		t.Fatalf("ports = %v", ports)
	}
	// 预订实例：无 ssh_command，有可取消截止
	v = mustRun(t, r, "get_instance", `{"instance_id":"ins-res"}`)
	if v["ssh_command"] != "" || v["cancellable_until"] != "2026-10-27T09:00:00+08:00" || len(arr(t, v, "renew_options")) != 1 {
		t.Fatalf("reserved = %v", v)
	}
	// 他人实例 → not_found
	if _, err := run(t, r, "get_instance", ownerID, `{"instance_id":"ins-other"}`); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user's instance err = %v", err)
	}
	// 业务服务的内部错误原样返回（调用方记 failed）
	f.rental.err = errors.New("db down")
	if _, err := run(t, r, "get_instance", ownerID, `{"instance_id":"ins-run"}`); err == nil || IsBusinessError(err) {
		t.Fatalf("internal err = %v", err)
	}
}

func TestGetInstanceEvents(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	v := mustRun(t, r, "get_instance_events", `{"instance_id":"ins-run","limit":1}`)
	evs := arr(t, v, "events")
	if len(evs) != 1 || evs[0]["to_state"] != "running" || evs[0]["reason_kind"] != "provisioned" || evs[0]["detail"] != "开通成功" {
		t.Fatalf("events = %v", evs)
	}
	if len(arr(t, mustRun(t, r, "get_instance_events", `{"instance_id":"ins-run"}`), "events")) != 2 {
		t.Fatal("default limit")
	}
	if _, err := run(t, r, "get_instance_events", ownerID, `{"instance_id":"ins-other"}`); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other err = %v", err)
	}
}

func TestListInstanceApps(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	v := mustRun(t, r, "list_instance_apps", `{"instance_id":"ins-run"}`)
	deps := arr(t, v, "deployments")
	if len(deps) != 2 || deps[0]["id"] != "dep-vllm" || deps[1]["id"] != "dep-old" {
		t.Fatalf("live deployments must come first: %v", deps)
	}
	d := deps[0]
	if d["state"] != "failed" || d["app_slug"] != "vllm" || d["app_name"] != "vLLM 推理服务" || d["error_code"] != "app_start_failed" ||
		d["message"] != "启动失败 CUDA OOM" || d["url"] != "https://ins-run-8000.spark.test/" {
		t.Fatalf("deployment = %v", d)
	}
	env := d["env"].(map[string]any)
	if !strings.Contains(env["MODEL_ID"].(string), "</untrusted-data>") {
		t.Fatalf("env = %v", env)
	}
	if se := d["secret_env"].(map[string]any); se["API_KEY"] != true {
		t.Fatalf("secret_env = %v", se)
	}
	if _, err := run(t, r, "list_instance_apps", ownerID, `{"instance_id":"ins-other"}`); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user's instance err = %v", err)
	}
}

func TestGetDeploymentLog(t *testing.T) {
	f := newFakes()
	r := NewRegistry(f.deps)
	res, err := run(t, r, "get_deployment_log", ownerID, `{"instance_id":"ins-run","deployment_id":"dep-vllm"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ref != "dep-vllm" {
		t.Fatalf("Ref = %q", res.Ref)
	}
	v := unwrapJSON(t, "get_deployment_log", ownerID, res.Content)
	if v["live"] != true || v["at"] != "2026-10-25T10:00:00+08:00" || v["omitted_bytes"] != float64(0) {
		t.Fatalf("log meta = %v", v)
	}
	log := v["log"].(string)
	wantHead := `<untrusted_data source="deployment_log" deployment="dep-vllm" instance="ins-run">` + "\n"
	if !strings.HasPrefix(log, wantHead) || !strings.HasSuffix(log, "\n</untrusted-data>") {
		t.Fatalf("inner wrap = %q", log)
	}
	if strings.Contains(log, "\x1b") || strings.Contains(log, "\x07") || strings.Contains(log, "[32m") {
		t.Fatalf("ANSI / control chars survived: %q", log)
	}
	if !strings.Contains(log, "loading model\nERROR </untrusted-data> CUDA out of memory") {
		t.Fatalf("log body = %q", log)
	}
	if _, err := run(t, r, "get_deployment_log", ownerID, `{"instance_id":"ins-other","deployment_id":"dep-other"}`); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other err = %v", err)
	}
	if _, err := run(t, r, "get_deployment_log", ownerID, `{"instance_id":"ins-run","deployment_id":"dep-vllm","tail_bytes":100000}`); err == nil {
		t.Fatal("tail_bytes > 16384 should fail validation")
	}
}

func TestGetDeploymentLogTail(t *testing.T) {
	f := newFakes()
	var b strings.Builder
	for i := 0; b.Len() < 64*1024; i++ {
		fmt.Fprintf(&b, "\x1b[33mline %05d \"quoted\" 中文日志\x1b[0m\n", i)
	}
	b.WriteString("LAST LINE")
	f.apps.logs["dep-vllm"] = appdeploy.LogResult{Log: b.String(), At: testNow}
	r := NewRegistry(f.deps)
	for _, tail := range []int{16384, 1000} {
		res, err := run(t, r, "get_deployment_log", ownerID, fmt.Sprintf(`{"instance_id":"ins-run","deployment_id":"dep-vllm","tail_bytes":%d}`, tail))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Content) > largeLimit {
			t.Fatalf("tail %d: len = %d", tail, len(res.Content))
		}
		v := unwrapJSON(t, "get_deployment_log", ownerID, res.Content)
		log := v["log"].(string)
		if !utf8.ValidString(log) || !strings.Contains(log, "LAST LINE") || !strings.Contains(log, "（前面已省略 ") || v["omitted_bytes"].(float64) <= 0 {
			t.Fatalf("tail %d: log = %.200q…", tail, log)
		}
		if tail == 1000 && len(log) > 1300 {
			t.Fatalf("tail 1000: log len %d", len(log))
		}
	}
}

func TestGetDeploymentEvents(t *testing.T) {
	f := newFakes()
	r := NewRegistry(f.deps)
	v := mustRun(t, r, "get_deployment_events", `{"instance_id":"ins-run","deployment_id":"dep-vllm"}`)
	evs := arr(t, v, "events")
	if len(evs) != 2 || evs[0]["to_state"] != "failed" || evs[0]["detail"] != "健康检查超时" || v["deployment_id"] != "dep-vllm" || v["state"] != "failed" {
		t.Fatalf("events = %v", v)
	}
	// 他人部署：GetForUser 拦下，Events 不被调用
	f.apps.eventsReq = nil
	if _, err := run(t, r, "get_deployment_events", ownerID, `{"instance_id":"ins-other","deployment_id":"dep-other"}`); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other err = %v", err)
	}
	if len(f.apps.eventsReq) != 0 {
		t.Fatal("Events called for another user's deployment")
	}
}

func TestGetWallet(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	v := mustRun(t, r, "get_wallet", `{}`)
	if v["balance_cents"] != float64(13800) || v["balance"] != "¥138.00" {
		t.Fatalf("wallet = %v", v)
	}
	ledger := arr(t, v, "ledger")
	if len(ledger) != 2 || ledger[0]["amount_cents"] != float64(-6900) || ledger[0]["amount"] != "¥-69.00" || ledger[0]["kind"] != "rent" || ledger[0]["remark"] != "租用 </untrusted-data>" {
		t.Fatalf("ledger = %v", ledger)
	}
	v = mustRun(t, r, "get_wallet", `{"ledger_limit":0}`)
	if len(arr(t, v, "ledger")) != 0 {
		t.Fatal("ledger_limit 0")
	}
	res, err := run(t, r, "get_wallet", otherID, `{}`)
	if err != nil || unwrapJSON(t, "get_wallet", otherID, res.Content)["balance_cents"] != float64(999) {
		t.Fatalf("other user's wallet: %v %v", res, err)
	}
}

func TestWalletTruncatesToSmallLimit(t *testing.T) {
	f := newFakes()
	rows := []wallet.LedgerRow{}
	for i := range 20 {
		rows = append(rows, wallet.LedgerRow{ID: int64(i), Amount: 1, Kind: "rent", Remark: strings.Repeat("备注", 200), CreatedAt: testNow})
	}
	f.wallet.ledger[ownerID] = rows
	res, err := run(t, NewRegistry(f.deps), "get_wallet", ownerID, `{"ledger_limit":20}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) > smallLimit {
		t.Fatalf("len %d > %d", len(res.Content), smallLimit)
	}
	v := unwrapJSON(t, "get_wallet", ownerID, res.Content)
	if v["meta"].(map[string]any)["truncated"] != true || v["balance_cents"] != float64(13800) {
		t.Fatalf("v = %v", v)
	}
}

func TestListOrders(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	orders := arr(t, mustRun(t, r, "list_orders", `{}`), "orders")
	if len(orders) != 1 || orders[0]["id"] != "ord-1" || orders[0]["amount_cents"] != float64(6900) || orders[0]["amount"] != "¥69.00" || orders[0]["product_name"] != "DGX Spark 整机" || orders[0]["instance_id"] != "ins-run" {
		t.Fatalf("orders = %v", orders)
	}
}

func TestGetCancelQuote(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	v := mustRun(t, r, "get_cancel_quote", `{"instance_id":"ins-res"}`)
	if v["refund_cents"] != float64(6900) || v["refund"] != "¥69.00" || v["max_refund"] != "¥69.00" || v["percent"] != float64(100) || v["deadline"] != "2026-10-27T09:00:00+08:00" || v["max_refund_cents"] != float64(6900) {
		t.Fatalf("quote = %v", v)
	}
	if _, err := run(t, r, "get_cancel_quote", ownerID, `{"instance_id":"ins-run"}`); !errors.Is(err, apperr.ErrCancelNotAllowed) {
		t.Fatalf("running instance err = %v", err)
	}
	if _, err := run(t, r, "get_cancel_quote", ownerID, `{"instance_id":"ins-other"}`); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other err = %v", err)
	}
}

func TestGetHelp(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	secs := help.Sections()
	if len(secs) != 14 {
		t.Fatalf("sections = %v", secs)
	}
	for _, s := range secs {
		v := mustRun(t, r, "get_help", fmt.Sprintf(`{"section":%q}`, s))
		body, _ := help.Section(s)
		if v["section"] != s || v["content"] != body || body == "" {
			t.Fatalf("get_help(%s) = %v", s, v)
		}
	}
}

func TestGenerateSecret(t *testing.T) {
	r := NewRegistry(newFakes().deps)
	seen := map[string]bool{}
	for _, c := range []struct {
		args string
		n    int
	}{{`{}`, 32}, {`{"length":8}`, 8}, {`{"length":64}`, 64}} {
		res, err := run(t, r, "generate_secret", ownerID, c.args)
		if err != nil || !res.Secret {
			t.Fatalf("generate_secret: %v secret=%v", err, res.Secret)
		}
		s := unwrapJSON(t, "generate_secret", ownerID, res.Content)["secret"].(string)
		if len(s) != c.n || seen[s] {
			t.Fatalf("secret %q len %d want %d", s, len(s), c.n)
		}
		for _, ch := range s {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9') {
				t.Fatalf("secret char %q", ch)
			}
		}
		seen[s] = true
	}
	if _, err := r.Validate("generate_secret", json.RawMessage(`{"length":4}`)); err == nil {
		t.Fatal("length 4 should fail")
	}
}

// 所有读工具对他人资源一律 not_found（归属校验都在服务层 *ForUser 路径上）。
func TestReadToolsNeverExposeOtherUsers(t *testing.T) {
	f := newFakes()
	r := NewRegistry(f.deps)
	for name, args := range map[string]string{
		"get_instance": `{"instance_id":"ins-other"}`, "get_instance_events": `{"instance_id":"ins-other"}`,
		"list_instance_apps": `{"instance_id":"ins-other"}`, "get_cancel_quote": `{"instance_id":"ins-other"}`,
		"get_deployment_log":    `{"instance_id":"ins-other","deployment_id":"dep-other"}`,
		"get_deployment_events": `{"instance_id":"ins-other","deployment_id":"dep-other"}`,
		// 自己的实例 + 他人的部署 ID
		"get_deployment_events#2": `{"instance_id":"ins-run","deployment_id":"dep-other"}`,
	} {
		tool := strings.Split(name, "#")[0]
		if _, err := run(t, r, tool, ownerID, args); !errors.Is(err, apperr.ErrNotFound) {
			t.Errorf("%s(%s) err = %v, want not_found", tool, args, err)
		}
	}
}
