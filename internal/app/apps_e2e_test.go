package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/app"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/sms"
)

// 应用一键部署的集成测试（规格 §16.2）：真实 app.Build（控制台 API、ingress、rental 钩子、Runner）+ 嵌入式 PG + 假时钟 +
// sshtest shell 模式充当所有实例的容器 sshd（每实例一个应用根目录 <Root>/<实例 ID>/root/data）+ 一个 httptest 充当所有 fake 应用的服务。
// Agent 用直接回报指令结果的方式模拟（与 TestAccessLifecycle 相同），Runner 由测试逐轮 Tick。

const appsPhone = "13800007777"

// testGitMirror 是集成测试配置的 SPARK_APP_GIT_MIRROR：只注入支持工作流的版本（comfyui 1.1.0）的 env。
const testGitMirror = "https://mirror.test/"

type appsEnv struct {
	t          *testing.T
	ctx        context.Context
	pool       *pgxpool.Pool
	clk        *clock.Fake
	a          *app.App
	up         *sshtest.Server
	ts         *httptest.Server
	jar        *cookiejar.Jar
	c          *client
	machines   []int64
	img        int64
	healthAddr string
	appRootFor func(string) string
	apps       map[string]e2ehooks.SeededApp
}

func newAppsEnv(t *testing.T, machines int) *appsEnv {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	t.Cleanup(func() { assertConsistent(t, pool); assertWorkflowsConsistent(t, pool) }) // 最后注册的先执行：在停进程、关服务之前检查
	clk := clock.NewFake(dbtest.T0)
	keyFile := upstreamKeyFile(t)
	upSigner, err := pki.LoadUpstreamSigner(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	hostPub, hostPEM, err := pki.GenerateInstanceHostKey()
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.ParsePrivateKey(hostPEM)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir() // 显式 Root：sshtest.Close（不可达用例里提前关）之后宿主机上仍能找到 run.sh 去停进程
	up, err := sshtest.New(sshtest.Options{AuthorizedKey: upSigner.PublicKey(), HostKey: hostSigner, Shell: true, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	// run.sh 用 setsid 把 _bg 与 fake 服务（sleep 100000）移出了 sshtest 的进程组：Close 杀不到，结束时逐个 run.sh stop
	t.Cleanup(func() { up.Close(); e2ehooks.StopFakeApps(root) })
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "spark-app-ok|path=%s", r.URL.Path)
	}))
	t.Cleanup(health.Close)
	e := &appsEnv{t: t, ctx: ctx, pool: pool, clk: clk, up: up, healthAddr: strings.TrimPrefix(health.URL, "http://")}
	e.appRootFor = func(id string) string { return filepath.Join(root, id, "root", "data") }
	cfg := config.Config{UpstreamSSHKeyFile: keyFile, Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key,
		SMSDriver: "mock", KYCDriver: "mock", UpstreamSSHPort: up.Port(), AppGitMirror: testGitMirror}
	e.a, err = app.Build(cfg, pool, discardLog(), app.Options{Clock: clk, SMS: &sms.Mock{}, DisableRateLimits: true,
		HostKeyGen:    func() (string, []byte, error) { return hostPub, hostPEM, nil },
		AppHealthAddr: func(int) string { return e.healthAddr },
		AppRootFor:    e.appRootFor})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.a.Upstream.Close)
	e.ts = httptest.NewServer(e.a.Handler)
	t.Cleanup(e.ts.Close)
	for i := 0; i < machines; i++ {
		mid := dbtest.CreateMachine(t, pool, fmt.Sprintf("APP%03d", i+1))
		e.exec(`UPDATE machines SET lan_ip = '127.0.0.1' WHERE id = $1`, mid) // 网关 / Runner 拨 lan_ip:UpstreamSSHPort = sshtest
		e.machines = append(e.machines, mid)
	}
	e.img = dbtest.ImageID(t, pool)
	seeded, err := e2ehooks.SeedApps(ctx, e.a, up, e.healthAddr)
	if err != nil {
		t.Fatal(err)
	}
	e.apps = map[string]e2ehooks.SeededApp{}
	for _, s := range seeded {
		e.apps[s.Slug] = s
	}
	e.jar, _ = cookiejar.New(nil)
	e.c = &client{t: t, base: e.ts.URL + "/api/v1", http: &http.Client{Jar: e.jar}}
	e.c.do("POST", "/auth/sms-code", map[string]string{"phone": appsPhone})
	e.c.do("POST", "/auth/login", map[string]string{"phone": appsPhone, "code": "000000"})
	e.c.do("POST", "/kyc/personal", map[string]string{"name": "赵六", "id_number": "110101199001011234"})
	rch := e.c.do("POST", "/recharges", map[string]any{"amount": 1_000_000, "channel": "wechat"})
	e.c.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{})
	return e
}

func (e *appsEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

// call 发控制台请求，返回状态码与响应体（不因非 2xx 失败）。
func (e *appsEnv) call(method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.c.http.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// agent 模拟全部机器的 Agent：心跳并成功执行全部待处理指令（含发布版本时广播的预拉取）。
func (e *appsEnv) agent() {
	e.t.Helper()
	for _, mid := range e.machines {
		if _, err := e.a.Fleet.Heartbeat(e.ctx, mid); err != nil {
			e.t.Fatal(err)
		}
		cmds, err := e.a.Fleet.PendingCommands(e.ctx, mid)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, cmd := range cmds {
			if err := e.a.Rental.HandleCommandResult(e.ctx, cmd.ID, true, "ok"); err != nil {
				e.t.Fatal(err)
			}
		}
	}
}

// order 下单（可带预装应用），返回实例视图。
func (e *appsEnv) order(extra map[string]any) map[string]any {
	e.t.Helper()
	body := map[string]any{"product_id": dbtest.ProductID(e.t, e.pool), "duration": "1d", "image_id": e.img}
	for k, v := range extra {
		body[k] = v
	}
	return e.c.do("POST", "/orders", body)["instance"].(map[string]any)
}

func (e *appsEnv) tick() int {
	e.t.Helper()
	n, locked, err := e.a.AppRunner.Tick(e.ctx)
	if err != nil || !locked {
		e.t.Fatalf("Tick = %d locked %v err %v", n, locked, err)
	}
	return n
}

func (e *appsEnv) dep(id string) appdeploy.Deployment {
	e.t.Helper()
	d, err := e.a.Apps.Get(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// until 逐轮（推进 PollInterval、真实等 50ms 让容器里的脚本推进、Tick）直到 ok，最多 maxRounds 轮。
func (e *appsEnv) until(id, what string, maxRounds int, ok func(appdeploy.Deployment) bool) appdeploy.Deployment {
	e.t.Helper()
	d := e.dep(id)
	stalled := false
	for i := 0; i < maxRounds && !ok(d); i++ {
		e.clk.Advance(appdeploy.PollInterval)
		// 部署中且上一轮没有进展时多等一会儿真实时间：假时钟每轮走 30 秒，机器繁忙时（新写的脚本首次 exec 可能要数秒）
		// 只等 50ms 会让 10 分钟的部署超时在脚本真正跑完之前到点
		if stalled && (d.State == appdeploy.StateQueued || d.State == appdeploy.StateInstalling) {
			time.Sleep(500 * time.Millisecond)
		} else {
			time.Sleep(50 * time.Millisecond)
		}
		e.tick()
		prev := d
		d = e.dep(id)
		stalled = d.State == prev.State && d.Phase == prev.Phase && d.Progress == prev.Progress && d.Message == prev.Message
	}
	if !ok(d) {
		e.t.Fatalf("deployment %s: %s not reached: state %s phase %q error %q message %q\nlog:\n%s", id, what, d.State, d.Phase, d.ErrorCode, d.Message, d.LogTail)
	}
	return d
}

func (e *appsEnv) untilState(id, state string) appdeploy.Deployment {
	e.t.Helper()
	return e.until(id, state, 200, func(d appdeploy.Deployment) bool { return d.State == state })
}

func (e *appsEnv) view(instID, depID string) map[string]any {
	e.t.Helper()
	return e.c.do("GET", "/instances/"+instID+"/apps/"+depID, nil)
}

type portRow struct {
	Port       int
	Visibility string
	Deployment string
}

func (e *appsEnv) ports(instID string) []portRow {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT port, visibility, coalesce(deployment_id, '') FROM instance_ports WHERE instance_id = $1 ORDER BY port`, instID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []portRow
	for rows.Next() {
		var r portRow
		if err := rows.Scan(&r.Port, &r.Visibility, &r.Deployment); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e *appsEnv) appDir(instID, slug string) string {
	return appdeploy.AppDir(e.appRootFor(instID), slug)
}

// containerStatus 读假容器里的 status.json。
func (e *appsEnv) containerStatus(instID, slug string) map[string]any {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.appDir(instID, slug), "status.json"))
	if err != nil {
		return nil
	}
	var st map[string]any
	_ = json.Unmarshal(b, &st)
	return st
}

// ingressGet 经 Web 入口访问实例私有端口：authorize（控制台登录 Cookie）→ callback 拿 sc_ingress → 带它请求。
func (e *appsEnv) ingressGet(instID string, port int, path string) (int, string) {
	e.t.Helper()
	host := fmt.Sprintf("%s-%d.spark.test", instID, port)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	tsURL, _ := url.Parse(e.ts.URL)
	get := func(h, p string, cookies []*http.Cookie) *http.Response {
		req, _ := http.NewRequest("GET", e.ts.URL+p, nil)
		req.Host = h
		for _, ck := range cookies {
			req.AddCookie(ck)
		}
		resp, err := noFollow.Do(req)
		if err != nil {
			e.t.Fatal(err)
		}
		return resp
	}
	// 控制台会话是在 127.0.0.1 上登录的：带到 spark.test（浏览器里是同一个域）
	r := get("spark.test", "/api/v1/ingress/authorize?target="+url.QueryEscape("http://"+host+":8080"+path), e.jar.Cookies(tsURL))
	r.Body.Close()
	cb, _ := url.Parse(r.Header.Get("Location"))
	if r.StatusCode != 302 || cb.Path != "/.spark/callback" {
		e.t.Fatalf("authorize = %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	r = get(host, "/.spark/callback?"+cb.RawQuery, nil)
	r.Body.Close()
	var ingressCookie *http.Cookie
	for _, ck := range r.Cookies() {
		if ck.Name == "sc_ingress" {
			ingressCookie = ck
		}
	}
	if ingressCookie == nil {
		e.t.Fatalf("callback = %d, no sc_ingress cookie", r.StatusCode)
	}
	r = get(host, path, []*http.Cookie{ingressCookie})
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

// publishVersion 给已有应用加一个已发布版本（同兼容镜像；spec 以最新版本为底，可改）。
func (e *appsEnv) publishVersion(slug, version, script string, tweak func(*appdeploy.Spec)) int64 {
	e.t.Helper()
	base, err := e.a.AppCatalog.GetVersion(e.ctx, e.apps[slug].VersionID)
	if err != nil {
		e.t.Fatal(err)
	}
	spec := base.Spec
	if tweak != nil {
		tweak(&spec)
	}
	actor := audit.Actor{AdminID: 1}
	v, err := e.a.AppCatalog.CreateVersion(e.ctx, actor, e.apps[slug].ID, appdeploy.VersionInput{Version: version, ImageIDs: base.ImageIDs, Spec: spec, Script: script})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := e.a.AppCatalog.PublishVersion(e.ctx, actor, e.apps[slug].ID, v.ID); err != nil {
		e.t.Fatal(err)
	}
	return v.ID
}

func (e *appsEnv) alertCount(kind string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, kind).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// assertConsistent 是规格 §14 的一致性检查：removed / lost 的部署没有端口行指向它；非终态部署的实例非终态；
// 每个实例非终态部署 ≤ 3；running 部署的主端口在 instance_ports 里且归它。
func assertConsistent(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	checks := []struct{ what, sql string }{
		{"port rows pointing at removed / lost deployments", `SELECT p.instance_id || ':' || p.port || ' → ' || d.id || ' ' || d.state
			FROM instance_ports p JOIN app_deployments d ON d.id = p.deployment_id WHERE d.state IN ('removed', 'lost')`},
		{"live deployments on gone instances", `SELECT d.id || ' ' || d.state || ' on ' || i.id || ' ' || i.state
			FROM app_deployments d JOIN instances i ON i.id = d.instance_id
			WHERE d.state NOT IN ('removed', 'lost') AND i.state IN ('wiping', 'released', 'failed', 'cancelled')`},
		{"instances with more than 3 live deployments", `SELECT instance_id || ' ' || count(*) FROM app_deployments
			WHERE state NOT IN ('removed', 'lost') GROUP BY instance_id HAVING count(*) > 3`},
		{"running deployments without their main port row", `SELECT d.id || ' port ' || d.port FROM app_deployments d WHERE d.state = 'running'
			AND NOT EXISTS (SELECT 1 FROM instance_ports p WHERE p.instance_id = d.instance_id AND p.port = d.port AND p.deployment_id = d.id)`},
	}
	for _, c := range checks {
		rows, err := pool.Query(ctx, c.sql)
		if err != nil {
			t.Errorf("consistency %s: %v", c.what, err)
			continue
		}
		var bad []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Errorf("consistency %s: %v", c.what, err)
			}
			bad = append(bad, s)
		}
		rows.Close()
		if len(bad) > 0 {
			t.Errorf("consistency (§14): %s: %v", c.what, bad)
		}
	}
}

// 全链路：下单预装 vLLM → 开通 → queued → installing（进度 / 阶段 / 日志）→ running → 端口登记 → 经 ingress 访问 →
// 停止（端口与 url 保留）→ 启动 → 到期停机（Runner 不 exec、健康未知）→ 续费 → 自动回 queued → running（跳过安装）→ 释放 → removed。
func TestAppsFullLifecycle(t *testing.T) {
	e := newAppsEnv(t, 1)
	vllm := e.apps["vllm"]
	inst := e.order(map[string]any{"app_id": vllm.ID, "app_env": map[string]string{"API_KEY": "abcdefgh12"}})
	id := inst["id"].(string)
	sum := inst["apps_summary"].([]any)
	if inst["state"] != "creating" || len(sum) != 1 || sum[0].(map[string]any)["state"] != "queued" {
		t.Fatalf("order = state %v apps_summary %v", inst["state"], sum)
	}
	depID := sum[0].(map[string]any)["id"].(string)
	// 实例还在开通：Runner 不 exec，只推后
	opens := len(e.up.ChannelOpens())
	e.tick()
	if d := e.dep(depID); d.State != appdeploy.StateQueued || len(e.up.ChannelOpens()) != opens {
		t.Fatalf("tick while creating: %s, channel opens %d → %d", d.State, opens, len(e.up.ChannelOpens()))
	}
	e.agent()
	if st := e.c.do("GET", "/instances/"+id, nil)["state"]; st != "running" {
		t.Fatalf("instance = %v", st)
	}
	// installing：download 阶段 50%，日志尾部带脚本的进度行
	d := e.until(depID, "installing / download", 200, func(d appdeploy.Deployment) bool {
		return d.State == appdeploy.StateInstalling && d.Phase == "download"
	})
	if d.Progress != 50 || !strings.Contains(d.LogTail, "downloading Qwen/Qwen3-8B") || d.Attempt != 1 {
		t.Fatalf("installing = progress %d attempt %d log %q", d.Progress, d.Attempt, d.LogTail)
	}
	if strings.Contains(d.LogTail, "abcdefgh12") {
		t.Fatal("secret in log tail")
	}
	e.untilState(depID, appdeploy.StateRunning)
	if env, err := os.ReadFile(filepath.Join(e.appDir(id, "vllm"), "env")); err != nil || strings.Contains(string(env), "SPARK_GIT_MIRROR") {
		t.Fatalf("vllm env has SPARK_GIT_MIRROR (or unreadable %v):\n%s", err, env)
	}
	if got := e.ports(id); len(got) != 1 || got[0] != (portRow{8000, "private", depID}) {
		t.Fatalf("instance_ports = %+v", got)
	}
	v := e.view(id, depID)
	if v["state"] != "running" || v["health"] != "ok" || !strings.Contains(fmt.Sprint(v["url"]), id+"-8000.") {
		t.Fatalf("view = %v", v)
	}
	if err := e.a.Routes.Refresh(e.ctx); err != nil {
		t.Fatal(err)
	}
	if code, body := e.ingressGet(id, 8000, "/docs"); code != 200 || !strings.Contains(body, "spark-app-ok|path=/docs") {
		t.Fatalf("ingress = %d %q", code, body)
	}

	// 停止：端口行与 url 保留
	e.c.do("POST", "/instances/"+id+"/apps/"+depID+"/stop", map[string]any{})
	e.untilState(depID, appdeploy.StateStopped)
	if got := e.ports(id); len(got) != 1 || got[0].Deployment != depID {
		t.Fatalf("ports after stop = %+v", got)
	}
	if v := e.view(id, depID); v["state"] != "stopped" || v["url"] == nil {
		t.Fatalf("stopped view = %v", v)
	}
	if st := e.containerStatus(id, "vllm"); st["state"] != "stopped" {
		t.Fatalf("container status after stop = %v", st)
	}
	e.c.do("POST", "/instances/"+id+"/apps/"+depID+"/start", map[string]any{})
	if d := e.dep(depID); d.State != appdeploy.StateQueued {
		t.Fatalf("after start = %s", d.State)
	}
	e.untilState(depID, appdeploy.StateRunning)

	// 到期停机：Runner 不 exec（容器停了），running 部署健康未知
	e.clk.Advance(25 * time.Hour)
	e.agent() // 心跳
	if err := e.a.Rental.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.agent() // 执行停机
	if st := e.c.do("GET", "/instances/"+id, nil)["state"]; st != "stopped" {
		t.Fatalf("expired instance = %v", st)
	}
	opens = len(e.up.ChannelOpens())
	for i := 0; i < 3; i++ {
		e.clk.Advance(appdeploy.NotRunningRetry)
		e.tick()
	}
	d = e.dep(depID)
	if n := len(e.up.ChannelOpens()); n != opens || d.State != appdeploy.StateRunning || d.Health != appdeploy.HealthUnknown {
		t.Fatalf("while instance stopped: channel opens %d → %d, state %s health %s", opens, n, d.State, d.Health)
	}
	// 续费 → 实例 running → 钩子把部署置回 queued → 重新 deploy，.installed 匹配跳过安装
	e.c.do("POST", "/instances/"+id+"/renew", map[string]string{"duration": "1d"})
	e.agent()
	if d := e.dep(depID); d.State != appdeploy.StateQueued {
		t.Fatalf("after renew = %s, want queued", d.State)
	}
	d = e.untilState(depID, appdeploy.StateRunning)
	if !strings.Contains(d.LogTail, "skip install") || strings.Contains(d.LogTail, "downloading") {
		t.Fatalf("redeploy after renew log = %q", d.LogTail)
	}

	// 释放 → removed(instance_gone)，端口行随部署删除
	e.c.do("POST", "/instances/"+id+"/release", map[string]any{})
	d = e.dep(depID)
	if d.State != appdeploy.StateRemoved || d.ErrorCode != appdeploy.ErrCodeInstanceGone {
		t.Fatalf("after release = %s %s", d.State, d.ErrorCode)
	}
	for _, p := range e.ports(id) {
		if p.Deployment != "" {
			t.Fatalf("port rows still owned after release: %+v", e.ports(id))
		}
	}
	assertConsistent(t, e.pool)
}

// 失败链路：install 退出 3 → failed(script_failed) 且日志带脚本输出 → 重新部署到好版本 → running；
// 超时（假时钟）→ failed(timeout) 且容器里的 _bg 进程组被停掉；容器 sshd 不可达 → 第 10 轮 failed(unreachable) + 告警恰一条。
func TestAppsFailureAndRedeploy(t *testing.T) {
	e := newAppsEnv(t, 1)
	id := e.order(nil)["id"].(string)
	e.agent()
	vllm := e.apps["vllm"]
	bad := e.publishVersion("vllm", "1.0.1", "#!/bin/sh\ncase \"$1\" in\n  install) echo \"boom: model download refused\"; exit 3 ;;\n  start) exit 0 ;;\nesac\n", nil)
	created := e.c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": vllm.ID, "app_version_id": bad, "env": map[string]string{}})
	depID := created["id"].(string)
	d := e.untilState(depID, appdeploy.StateFailed)
	if d.ErrorCode != appdeploy.ErrCodeScriptFailed || !strings.Contains(d.LogTail, "boom: model download refused") {
		t.Fatalf("failed = %s log %q", d.ErrorCode, d.LogTail)
	}
	if v := e.view(id, depID); v["error_code"] != "script_failed" {
		t.Fatalf("failed view = %v", v)
	}
	e.c.do("POST", "/instances/"+id+"/apps/"+depID+"/redeploy", map[string]any{"app_version_id": vllm.VersionID})
	d = e.untilState(depID, appdeploy.StateRunning)
	if d.AppVersionID != vllm.VersionID || d.ErrorCode != "" {
		t.Fatalf("redeployed = version %d error %q", d.AppVersionID, d.ErrorCode)
	}

	// 超时：timeout_minutes 1、install 睡 30 秒；容器里进入 install 后假时钟推进 61 秒
	slow := e.publishVersion("open-webui", "1.0.1", "#!/bin/sh\ncase \"$1\" in\n  install) sleep 30 ;;\n  start) exit 0 ;;\nesac\n",
		func(s *appdeploy.Spec) { s.TimeoutMinutes = 1 })
	webui := e.c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": e.apps["open-webui"].ID, "app_version_id": slow, "env": map[string]string{}})["id"].(string)
	e.untilState(webui, appdeploy.StateInstalling)
	pgid := 0
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := e.containerStatus(id, "open-webui")
		b, _ := os.ReadFile(filepath.Join(e.appDir(id, "open-webui"), ".pgid"))
		pgid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if st != nil && st["phase"] == "install" && pgid > 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pgid <= 1 || syscall.Kill(-pgid, 0) != nil {
		t.Fatalf("install not running in container (pgid %d, status %v)", pgid, e.containerStatus(id, "open-webui"))
	}
	e.clk.Advance(61 * time.Second)
	e.tick()
	d = e.dep(webui)
	if d.State != appdeploy.StateFailed || d.ErrorCode != appdeploy.ErrCodeTimeout {
		t.Fatalf("after timeout = %s %s %q", d.State, d.ErrorCode, d.Message)
	}
	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Fatalf("_bg process group %d still alive after timeout", pgid)
	}
	// 容器里不再是 installing：run.sh stop 时 _bg 的 TERM 陷阱写 failed，随后 stop 写 stopped
	if st := e.containerStatus(id, "open-webui"); st["state"] != "failed" && st["state"] != "stopped" {
		t.Fatalf("container status after timeout = %v", st)
	}
	if out, _ := exec.Command("sh", filepath.Join(e.appDir(id, "open-webui"), "run.sh"), "status").Output(); !strings.Contains(string(out), "alive=0") {
		t.Fatalf("container still alive after timeout: %s", out)
	}

	// 不可达：关掉假容器 sshd（之后的连接被拒）→ running 的 vLLM 每轮 DialError，第 10 轮 failed(unreachable)，告警只一条
	e.up.Close()
	for i := 1; i <= appdeploy.UnreachableFailAfter; i++ {
		e.clk.Advance(appdeploy.UnreachableRetry)
		e.tick()
		d = e.dep(depID)
		if i < appdeploy.UnreachableFailAfter && (d.State != appdeploy.StateRunning || d.UnreachableCount != i) {
			t.Fatalf("round %d: %s unreachable_count %d", i, d.State, d.UnreachableCount)
		}
	}
	if d.State != appdeploy.StateFailed || d.ErrorCode != appdeploy.ErrCodeUnreachable {
		t.Fatalf("after %d unreachable rounds = %s %s", appdeploy.UnreachableFailAfter, d.State, d.ErrorCode)
	}
	if n := e.alertCount("app_deploy_unreachable"); n != 1 {
		t.Fatalf("app_deploy_unreachable alerts = %d, want 1", n)
	}
}

// 换机：running 部署 → AdminMigrate → 旧部署 lost、新实例上 queued → 新实例开通 → running，认领复制过来的端口行（不重复插入）。
// 同时（复审裁定）：用户停止的应用迁移后仍是 stopped（新实例开通不启动它，端口行保留待启动后认领）；failed 的应用不迁移，
// 复制到新实例的它的端口行被删掉（不留孤儿端口挡住以后再装）。
func TestAppsMigrate(t *testing.T) {
	e := newAppsEnv(t, 2)
	inst := e.order(map[string]any{"app_id": e.apps["vllm"].ID})
	id := inst["id"].(string)
	oldDep := inst["apps_summary"].([]any)[0].(map[string]any)["id"].(string)
	oldLab := e.c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": e.apps["jupyterlab"].ID,
		"env": map[string]string{"JUPYTER_TOKEN": "abcdefghijklmnop1234"}})["id"].(string)
	bad := e.publishVersion("open-webui", "1.0.1", "#!/bin/sh\ncase \"$1\" in install) exit 3 ;; esac\n", nil)
	oldWebui := e.c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": e.apps["open-webui"].ID, "app_version_id": bad})["id"].(string)
	e.agent()
	e.untilState(oldDep, appdeploy.StateRunning)
	e.untilState(oldLab, appdeploy.StateRunning)
	e.untilState(oldWebui, appdeploy.StateFailed)
	// 模拟「running 过后崩溃成 failed」：端口行仍归它（install 就失败的部署从没登记过端口）
	e.exec(`INSERT INTO instance_ports (instance_id, port, created_at, updated_at, deployment_id) VALUES ($1, 3000, now(), now(), $2)`, id, oldWebui)
	e.c.do("POST", "/instances/"+id+"/apps/"+oldLab+"/stop", map[string]any{})
	e.untilState(oldLab, appdeploy.StateStopped)

	moved, err := e.a.Rental.AdminMigrate(e.ctx, audit.Actor{AdminID: 1}, id, "bad gpu")
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{oldDep, oldLab, oldWebui} {
		if d := e.dep(old); d.State != appdeploy.StateLost {
			t.Fatalf("old deployment %s = %s, want lost", old, d.State)
		}
	}
	live, err := e.a.Apps.ListLive(e.ctx, moved.ID)
	if err != nil || len(live) != 2 {
		t.Fatalf("new instance deployments = %+v %v", live, err)
	}
	var newDep, newLab string
	for _, d := range live {
		switch {
		case d.AppID == e.apps["vllm"].ID && d.State == appdeploy.StateQueued && d.Desired == appdeploy.DesiredRunning:
			newDep = d.ID
		case d.AppID == e.apps["jupyterlab"].ID && d.State == appdeploy.StateStopped && d.Desired == appdeploy.DesiredStopped:
			newLab = d.ID
		default:
			t.Fatalf("unexpected migrated deployment %+v", d)
		}
	}
	if newDep == "" || newLab == "" {
		t.Fatalf("migrated deployments = %+v", live)
	}
	want := []portRow{{8000, "private", ""}, {8888, "private", ""}} // 3000（failed 的 open-webui）没有跟过来
	if got := e.ports(moved.ID); !slices.Equal(got, want) {
		t.Fatalf("copied port rows before claim = %+v, want %+v", got, want)
	}
	e.agent() // 新实例开通、旧实例洗机
	if st := e.c.do("GET", "/instances/"+moved.ID, nil)["state"]; st != "running" {
		t.Fatalf("new instance = %v", st)
	}
	d := e.untilState(newDep, appdeploy.StateRunning)
	if !strings.Contains(d.LogTail, "downloading") { // 新机器上的新目录：重新安装
		t.Fatalf("new deployment log = %q", d.LogTail)
	}
	if lab := e.dep(newLab); lab.State != appdeploy.StateStopped {
		t.Fatalf("stopped app started by the new instance: %s", lab.State)
	}
	want = []portRow{{8000, "private", newDep}, {8888, "private", ""}}
	if got := e.ports(moved.ID); !slices.Equal(got, want) {
		t.Fatalf("new instance ports = %+v, want %+v", got, want)
	}
	// 用户再启动 → 认领保留的 8888（不重复插入）
	e.c.do("POST", "/instances/"+moved.ID+"/apps/"+newLab+"/start", map[string]any{})
	e.untilState(newLab, appdeploy.StateRunning)
	want = []portRow{{8000, "private", newDep}, {8888, "private", newLab}}
	if got := e.ports(moved.ID); !slices.Equal(got, want) {
		t.Fatalf("new instance ports after start = %+v, want %+v", got, want)
	}
}

// 同一实例三个应用同时 queued：一轮内并发部署（峰值并发 ≥ 2），都 running，端口 8000 / 3000 / 8888 各归各的部署；第四个 409 app_limit。
func TestAppsConcurrentOnOneInstance(t *testing.T) {
	e := newAppsEnv(t, 1)
	inst := e.order(map[string]any{"app_id": e.apps["vllm"].ID})
	id := inst["id"].(string)
	deps := map[string]string{"vllm": inst["apps_summary"].([]any)[0].(map[string]any)["id"].(string)}
	// 实例开通中就可以加应用
	deps["open-webui"] = e.c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": e.apps["open-webui"].ID})["id"].(string)
	deps["jupyterlab"] = e.c.do("POST", "/instances/"+id+"/apps", map[string]any{"app_id": e.apps["jupyterlab"].ID,
		"env": map[string]string{"JUPYTER_TOKEN": "abcdefghijklmnop1234"}})["id"].(string)
	e.agent()
	if n := e.tick(); n != 3 {
		t.Fatalf("first tick processed %d, want 3", n)
	}
	for slug, dep := range deps {
		if d := e.dep(dep); d.State != appdeploy.StateInstalling {
			t.Fatalf("%s after one tick = %s", slug, d.State)
		}
	}
	if p := e.a.AppRunner.Stats().PeakConcurrent; p < 2 {
		t.Fatalf("PeakConcurrent = %d, want ≥ 2", p)
	}
	for _, dep := range deps {
		e.untilState(dep, appdeploy.StateRunning)
	}
	want := []portRow{{3000, "private", deps["open-webui"]}, {8000, "private", deps["vllm"]}, {8888, "private", deps["jupyterlab"]}}
	if got := e.ports(id); !slices.Equal(got, want) {
		t.Fatalf("ports = %+v, want %+v", got, want)
	}
	// 第四个（另建一个兼容默认镜像的应用，免得先撞上镜像不兼容）
	actor := audit.Actor{AdminID: 1}
	extra, err := e.a.AppCatalog.CreateApp(e.ctx, actor, appdeploy.AppInput{Slug: "extra-tool", Name: "Extra", Category: "tool", Status: appdeploy.StatusPublished})
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.a.AppCatalog.CreateVersion(e.ctx, actor, extra.ID, appdeploy.VersionInput{Version: "1.0.0", ImageIDs: []int64{e.img},
		Spec: appdeploy.Spec{Ports: []appdeploy.PortSpec{{Port: 7000}}}, Script: "#!/bin/sh\nexit 0\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.a.AppCatalog.PublishVersion(e.ctx, actor, extra.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	if code, out := e.call("POST", "/instances/"+id+"/apps", map[string]any{"app_id": extra.ID}); code != 409 || out["code"] != "app_limit" {
		t.Fatalf("fourth app = %d %v, want 409 app_limit", code, out)
	}
}

// 多副本：两个 Runner 并发 Tick，每轮恰一个拿到咨询锁并处理到点的部署；另一条连接长持该锁时两者都跳过，释放后恢复。
func TestAppsTwoRunnersOneWorks(t *testing.T) {
	e := newAppsEnv(t, 1)
	inst := e.order(map[string]any{"app_id": e.apps["vllm"].ID})
	dep := inst["apps_summary"].([]any)[0].(map[string]any)["id"].(string)
	e.agent()
	e.untilState(dep, appdeploy.StateRunning)
	r1 := e.a.AppRunner
	r2 := appdeploy.NewRunner(e.a.Apps, e.a.Upstream, appdeploy.RunnerConfig{AppRootFor: e.appRootFor, HealthAddr: func(int) string { return e.healthAddr }})
	type res struct {
		n      int
		locked bool
		err    error
	}
	for round := 1; round <= 20; round++ {
		e.clk.Advance(appdeploy.HealthInterval) // running 部署每轮到点做一次健康检查（exec + HTTP），持锁的一方要花几十毫秒
		start := make(chan struct{})
		out := make([]res, 2)
		var wg sync.WaitGroup
		for i, r := range []*appdeploy.Runner{r1, r2} {
			wg.Go(func() {
				<-start
				n, locked, err := r.Tick(e.ctx)
				out[i] = res{n, locked, err}
			})
		}
		close(start)
		wg.Wait()
		if out[0].err != nil || out[1].err != nil || out[0].locked == out[1].locked {
			t.Fatalf("round %d: r1 %+v r2 %+v, want exactly one locked", round, out[0], out[1])
		}
		if w := out[0]; (w.locked && w.n != 1) || (!w.locked && out[1].n != 1) {
			t.Fatalf("round %d: winner processed r1 %+v r2 %+v, want 1", round, out[0], out[1])
		}
	}
	if d := e.dep(dep); d.State != appdeploy.StateRunning || d.Health != appdeploy.HealthOK {
		t.Fatalf("after 20 rounds = %s %s", d.State, d.Health)
	}
	// 另一条连接（另一个副本）持锁：两个 Runner 都跳过
	conn, err := e.pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(e.ctx, `SELECT pg_advisory_lock(hashtext('appdeploy'))`); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(appdeploy.HealthInterval)
	for i, r := range []*appdeploy.Runner{r1, r2} {
		if n, locked, err := r.Tick(e.ctx); err != nil || locked || n != 0 {
			t.Fatalf("runner %d while lock held elsewhere = %d %v %v", i+1, n, locked, err)
		}
	}
	// 持锁者「停掉」（释放锁）→ 接管
	if _, err := conn.Exec(e.ctx, `SELECT pg_advisory_unlock(hashtext('appdeploy'))`); err != nil {
		t.Fatal(err)
	}
	if n, locked, err := r2.Tick(e.ctx); err != nil || !locked || n != 1 {
		t.Fatalf("r2 after release = %d %v %v", n, locked, err)
	}
	if n, locked, err := r1.Tick(e.ctx); err != nil || !locked || n != 0 {
		t.Fatalf("r1 after release = %d %v %v", n, locked, err)
	}
}
