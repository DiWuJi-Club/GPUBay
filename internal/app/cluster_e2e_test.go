package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/admin"
	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// 计划 13 的集成测试（规格 §16.2）：真实 app.Build（控制台 / 后台 API、Agent 网关、rental / appdeploy / llm）+ 嵌入式 PG + 假时钟 +
// e2ehooks（与 cmd/e2e-server 相同的钩子：步进式 fake Agent 经真实网关接入，执行器带 fake 集群数据面；sshtest shell 模式充当全部
// 实例的容器 sshd；fake 应用的健康检查 / Web 打到一个 httptest）。钩子经 HTTP 调用，同时验证 Task 16（Playwright）要用的钩子。
// 每个用例结束时核对 rental.CheckClusterInvariants、llm.CheckInvariants 与应用部署一致性。

type cluEnv struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	clk   *clock.Fake
	a     *app.App
	hooks *httptest.Server
	ts    *httptest.Server // 控制台 API + /v1
	adm   *httptest.Server // 后台
	root  string           // 假容器根目录（<root>/<实例>/root/data 是实例的应用根）
	apps  map[string]e2ehooks.SeededApp

	phones atomic.Int64
}

func newCluEnv(t *testing.T) *cluEnv {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	t.Cleanup(func() { assertConsistent(t, pool) })
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
	root := t.TempDir()
	up, err := sshtest.New(sshtest.Options{AuthorizedKey: upSigner.PublicKey(), HostKey: hostSigner, Shell: true, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { up.Close(); e2ehooks.StopFakeApps(root) })
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "spark-app-ok|path=%s", r.URL.Path)
	}))
	t.Cleanup(health.Close)
	healthAddr := strings.TrimPrefix(health.URL, "http://")
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir(),
		AgentHosts: []string{"127.0.0.1"}, AgentDataSizeGiB: 2048, AgentBandwidthMbps: 200, AgentDNSServers: []string{"223.5.5.5"},
		UpstreamSSHKeyFile: keyFile, UpstreamSSHPort: up.Port(), LLMGateway: true}
	log := discardLog()
	kycSwitch := &e2ehooks.SwitchableKYC{}
	a, err := app.Build(cfg, pool, log, app.Options{Clock: clk, KYC: kycSwitch, DisableRateLimits: true, AgentTick: 20 * time.Millisecond,
		HostKeyGen:    func() (string, []byte, error) { return hostPub, hostPEM, nil },
		AppHealthAddr: func(int) string { return healthAddr },
		AppRootFor:    func(id string) string { return filepath.Join(root, id, "root", "data") }})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gwCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = a.Gateway.Serve(gwCtx, lis) }()
	hs := &e2ehooks.Server{App: a, Pool: pool, Clock: clk, KYC: kycSwitch, Log: log, AgentAddr: lis.Addr().String(), Upstream: up, HealthAddr: healthAddr}
	e := &cluEnv{t: t, ctx: ctx, pool: pool, clk: clk, a: a, root: root, apps: map[string]e2ehooks.SeededApp{}}
	e.hooks = httptest.NewServer(hs.Handler())
	t.Cleanup(func() { e.hooks.Close(); hs.Close(); cancel(); <-done; a.Upstream.Close() })
	e.ts = httptest.NewServer(a.Handler)
	t.Cleanup(e.ts.Close)
	e.adm = httptest.NewServer(a.AdminHandler)
	t.Cleanup(e.adm.Close)
	for _, it := range e.hook("/hooks/apps/seed", nil)["apps"].([]any) {
		m := it.(map[string]any)
		e.apps[m["slug"].(string)] = e2ehooks.SeededApp{ID: int64(m["id"].(float64)), Slug: m["slug"].(string), VersionID: int64(m["version_id"].(float64))}
	}
	// 最后注册、最先执行：每个用例结束时集群 ①～⑤ 与 llm ①～⑩ 不变量都成立
	t.Cleanup(func() {
		if err := rental.CheckClusterInvariants(context.Background(), pool); err != nil {
			t.Errorf("cluster invariants: %v", err)
		}
		if err := llm.CheckInvariants(context.Background(), pool); err != nil {
			t.Errorf("llm invariants: %v", err)
		}
	})
	return e
}

// hook 调一个 e2e 钩子（须 200），返回 JSON 对象。
func (e *cluEnv) hook(path string, body any) map[string]any {
	e.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(e.hooks.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		e.t.Fatalf("hook %s %v = %d %v", path, body, resp.StatusCode, out)
	}
	return out
}

func (e *cluEnv) agents() { e.t.Helper(); e.hook("/hooks/agent", nil) }
func (e *cluEnv) agentOn(mid int64) {
	e.t.Helper()
	e.hook("/hooks/agent", map[string]any{"machine_id": mid})
}
func (e *cluEnv) sweep()                  { e.t.Helper(); e.hook("/hooks/sweep", nil) }
func (e *cluEnv) appsTick(n int)          { e.t.Helper(); e.hook("/hooks/apps/tick", map[string]any{"n": n}) }
func (e *cluEnv) llmTick() map[string]any { e.t.Helper(); return e.hook("/hooks/llm/tick", nil) }
func (e *cluEnv) clusterNet(mid int64, b map[string]any) {
	e.t.Helper()
	b["machine_id"] = mid
	e.hook("/hooks/agent/cluster-net", b)
}

// fabric 经钩子建一个 fabric（kind lan / cx7），返回 ID。
func (e *cluEnv) fabric(kind string, extra map[string]any) int64 {
	e.t.Helper()
	body := map[string]any{"kind": kind}
	for k, v := range extra {
		body[k] = v
	}
	return int64(e.hook("/hooks/fabrics", body)["id"].(float64))
}

// machines 经钩子登记 n 台机器、接入 fake Agent 并加入 fabric；断言全部 cluster_ready。extra 可带 group_id 等。
func (e *cluEnv) machines(n int, fabricID int64, netPreset string, extra map[string]any) []int64 {
	e.t.Helper()
	body := map[string]any{"count": n, "fabric_id": fabricID, "net": netPreset}
	for k, v := range extra {
		body[k] = v
	}
	out := e.hook("/hooks/machines", body)
	var ids []int64
	for i, id := range out["ids"].([]any) {
		if !out["cluster_ready"].([]any)[i].(bool) {
			e.t.Fatalf("machine %v not cluster_ready: %v", id, out)
		}
		ids = append(ids, int64(id.(float64)))
	}
	return ids
}

// user 经钩子建一个已实名、余额 balance 分的用户并在控制台登录。
func (e *cluEnv) user(balance int64) (*client, int64) {
	e.t.Helper()
	phone := fmt.Sprintf("1360000%04d", e.phones.Add(1))
	u := e.hook("/hooks/users", map[string]any{"phone": phone, "verified": true, "balance": balance})
	jar, _ := cookiejar.New(nil)
	c := &client{t: e.t, base: e.ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	c.do("POST", "/auth/sms-code", map[string]string{"phone": phone})
	c.do("POST", "/auth/login", map[string]string{"phone": phone, "code": "000000"})
	return c, int64(u["id"].(float64))
}

// callH 发请求（可带请求头），返回状态码与响应体（不因非 2xx 失败）。
func callH(c *client, method, path string, body any, hdr map[string]string) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// orderCluster 经控制台即时下单一个 n 台集群（1 天），返回响应（instances / orders / cluster）。
func (e *cluEnv) orderCluster(c *client, n int) map[string]any {
	e.t.Helper()
	products := c.do("GET", "/products", nil)["items"].([]any)
	images := c.do("GET", "/images", nil)["items"].([]any)
	code, out := callH(c, "POST", "/bookings", map[string]any{"product_id": products[0].(map[string]any)["id"], "image_id": images[0].(map[string]any)["id"],
		"count": n, "duration": "1d", "quantity": 1, "cluster": true}, nil)
	if code != 201 || out["cluster"] == nil {
		e.t.Fatalf("cluster order = %d %v", code, out)
	}
	return out
}

func (e *cluEnv) scalar(dst any, sql string, args ...any) {
	e.t.Helper()
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(dst); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *cluEnv) inst(id string) rental.Instance {
	e.t.Helper()
	inst, err := e.a.Rental.Get(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return inst
}

// members 集群视图里成员的实例 ID（按 rank）。
func members(cv map[string]any) []string {
	var ids []string
	for _, m := range cv["members"].([]any) {
		ids = append(ids, m.(map[string]any)["instance_id"].(string))
	}
	return ids
}

// pendingOf 机器上未完成的指令（按创建顺序）。
func (e *cluEnv) pendingOf(mid int64) []fleet.Command {
	e.t.Helper()
	cmds, err := e.a.Fleet.PendingCommands(e.ctx, mid)
	if err != nil {
		e.t.Fatal(err)
	}
	return cmds
}

// createSpec 机器上实例 instID 的待执行 Create 指令里的集群 Spec（必须有）。
func (e *cluEnv) createSpec(mid int64, instID string) clusterspec.Spec {
	e.t.Helper()
	for _, c := range e.pendingOf(mid) {
		if c.Type != fleet.CmdCreateInstance || c.InstanceID != instID {
			continue
		}
		var p fleet.CreateInstancePayload
		if err := json.Unmarshal(c.Payload, &p); err != nil {
			e.t.Fatal(err)
		}
		if p.Cluster == nil {
			e.t.Fatalf("create %s has no cluster spec: %s", instID, c.Payload)
		}
		return *p.Cluster
	}
	e.t.Fatalf("no pending create for %s on machine %d: %+v", instID, mid, e.pendingOf(mid))
	return clusterspec.Spec{}
}

// clusterApp 轮询集群应用视图，每轮一次 /hooks/apps/tick，直到 cond 成立（最多 80 轮）。
func (e *cluEnv) clusterApp(c *client, cid string, cond func(map[string]any) bool) map[string]any {
	e.t.Helper()
	var v map[string]any
	for i := 0; i < 80; i++ {
		_, out := callH(c, "GET", "/clusters/"+cid+"/apps", nil, nil)
		v = out
		if v != nil && cond(v) {
			return v
		}
		e.appsTick(1)
	}
	e.t.Fatalf("cluster app never reached the wanted state: %v", v)
	return nil
}

func appRunning(v map[string]any) bool { return v["status"] == "running" && v["health"] == "ok" }

// alerts 某类告警的条数。
func (e *cluEnv) alerts(kind string) int {
	e.t.Helper()
	var n int
	e.scalar(&n, `SELECT count(*) FROM alerts WHERE kind = $1`, kind)
	return n
}

// adminLogin 建一个 super 管理员、后台登录并绑定 TOTP。
func (e *cluEnv) adminLogin() *client {
	e.t.Helper()
	if _, err := e.a.Admins.Create(e.ctx, audit.Actor{}, "root", "correct horse battery", admin.RoleSuper); err != nil {
		e.t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	ac := &client{t: e.t, base: e.adm.URL + "/admin/api/v1", http: &http.Client{Jar: jar}}
	login := ac.do("POST", "/auth/login", map[string]string{"username": "root", "password": "correct horse battery"})
	u, _ := url.Parse(login["otpauth_uri"].(string))
	e.clk.Advance(30 * time.Second)
	code, _ := admin.TOTPCode(u.Query().Get("secret"), admin.TOTPStep(e.clk.Now()))
	ac.do("POST", "/auth/totp/setup", map[string]string{"code": code})
	return ac
}

// ==== 用例 ====

// 用户集群全链路（规格 §16.2 第 1 条）：lan fabric + 4 台 fake 机器 → 下单 2 台集群 → 两张订单、一个集群、两条 Create 的 Spec 互为对端 →
// fake Agent 完成 → 清扫后 ready、到期对齐 → 控制台详情 → 部署 vllm-cluster → 聚合 running、head 端口登记、worker 未登记 →
// 整组续费两单（同键重放）→ 释放 → 洗机完成后 released、机器回到空闲。
func TestClusterOrderE2E(t *testing.T) {
	e := newCluEnv(t)
	fab := e.fabric("lan", nil)
	mids := e.machines(4, fab, e2ehooks.NetLAN, nil)
	c, uid := e.user(10_000_000)

	products := c.do("GET", "/products", nil)["items"].([]any)
	pid := products[0].(map[string]any)["id"]
	if cl := products[0].(map[string]any)["clusters"].(map[string]any); cl["2"].(map[string]any)["available"].(float64) != 2 ||
		cl["4"].(map[string]any)["available"].(float64) != 1 || cl["2"].(map[string]any)["rdma"] != false {
		t.Fatalf("product clusters = %v", cl)
	}
	if av := c.do("GET", fmt.Sprintf("/availability?product_id=%v&cluster_size=2", pid), nil); av["available"].(float64) != 2 {
		t.Fatalf("availability = %v", av)
	}

	out := e.orderCluster(c, 2)
	cv := out["cluster"].(map[string]any)
	cid := cv["id"].(string)
	if len(out["orders"].([]any)) != 2 || cv["status"] != "forming" || cv["network"] != "ethernet" || cv["fabric_kind"] != "lan" {
		t.Fatalf("order = %v", out)
	}
	ids := members(cv)
	var batchOrders int
	e.scalar(&batchOrders, `SELECT count(*) FROM orders WHERE batch_id = (SELECT batch_id FROM instance_clusters WHERE id = $1) AND kind = 'new'`, cid)
	if batchOrders != 2 {
		t.Fatalf("batch orders = %d", batchOrders)
	}
	// 两条 Create 的 Spec：同一集群、各自 rank、成员互为对端（routed：host_ip 是 lan_ip，成员 IP = 子网 .11 + rank）
	var specs []clusterspec.Spec
	for r, id := range ids {
		inst := e.inst(id)
		if inst.ClusterID == nil || *inst.ClusterID != cid || inst.ClusterRank == nil || *inst.ClusterRank != r {
			t.Fatalf("member %d = %+v", r, inst)
		}
		sp := e.createSpec(inst.MachineID, id)
		if sp.ClusterID != cid || sp.Rank != r || sp.Size != 2 || sp.Mode != clusterspec.ModeRouted || sp.Iface != "eth0" || len(sp.Members) != 2 {
			t.Fatalf("rank %d spec = %+v", r, sp)
		}
		specs = append(specs, sp)
	}
	for k := 0; k < 2; k++ {
		if specs[0].Members[k] != specs[1].Members[k] || specs[0].Members[k].InstanceID != ids[k] || specs[0].Members[k].HostIP != "127.0.0.1" {
			t.Fatalf("specs disagree on member %d: %+v vs %+v", k, specs[0].Members, specs[1].Members)
		}
	}
	headIP := specs[0].Members[0].IP

	// fake Agent 执行 Create（fake 集群数据面 Apply）→ 清扫：首次就绪 + 到期对齐
	e.agents()
	for _, id := range ids {
		if st := e.inst(id).State; st != rental.StateRunning {
			t.Fatalf("member %s = %s", id, st)
		}
	}
	e.sweep()
	view := c.do("GET", "/clusters/"+cid, nil)
	if view["status"] != "ready" || view["head_ip"] != headIP || view["ready_at"] == nil || view["expires_mismatch"] != false || view["auto_renew"] != false {
		t.Fatalf("cluster after ready = %v", view)
	}
	for _, m := range view["members"].([]any) {
		if m := m.(map[string]any); m["net_status"] != "ok" || m["state"] != "running" || m["expires_at"] != view["expires_at"] {
			t.Fatalf("member view = %v (cluster expires %v)", m, view["expires_at"])
		}
	}
	if list := c.do("GET", "/clusters", nil)["items"].([]any); len(list) != 1 || list[0].(map[string]any)["id"] != cid {
		t.Fatalf("cluster list = %v", list)
	}
	if ic := c.do("GET", "/instances/"+ids[1], nil)["cluster"].(map[string]any); ic["id"] != cid || ic["rank"].(float64) != 1 || ic["status"] != "ready" {
		t.Fatalf("member instance cluster = %v", ic)
	}
	// 成员的单实例续费 / 释放被拒
	if code, o := callH(c, "POST", "/instances/"+ids[0]+"/release", map[string]any{}, nil); code != 409 || o["code"] != "cluster_member" {
		t.Fatalf("member release = %d %v", code, o)
	}

	// 部署集群应用 vllm-cluster：两条成员部署（head + worker）→ 聚合 running；head 端口登记、worker 不登记
	vc := e.apps["vllm-cluster"]
	code, dep := callH(c, "POST", "/clusters/"+cid+"/apps", map[string]any{"app_id": vc.ID, "env": map[string]string{"MODEL_ID": "Qwen/Qwen3-32B"}}, nil)
	if code != 201 || len(dep["members"].([]any)) != 2 {
		t.Fatalf("deploy cluster app = %d %v", code, dep)
	}
	app := e.clusterApp(c, cid, appRunning)
	if app["url"] == nil || app["app_slug"] != "vllm-cluster" {
		t.Fatalf("cluster app = %v", app)
	}
	for _, m := range app["members"].([]any) {
		m := m.(map[string]any)
		wantRole := map[float64]string{0: "head", 1: "worker"}[m["rank"].(float64)]
		if m["role"] != wantRole || m["state"] != "running" || m["instance_id"] != ids[int(m["rank"].(float64))] {
			t.Fatalf("app member = %v", m)
		}
	}
	var headPorts, workerPorts int
	e.scalar(&headPorts, `SELECT count(*) FROM instance_ports WHERE instance_id = $1 AND port = 8000`, ids[0])
	e.scalar(&workerPorts, `SELECT count(*) FROM instance_ports WHERE instance_id = $1`, ids[1])
	if headPorts != 1 || workerPorts != 0 {
		t.Fatalf("ports head %d worker %d", headPorts, workerPorts)
	}
	// 单实例接口改不了成员部署
	headDep := app["members"].([]any)[0].(map[string]any)["deployment_id"].(string)
	if code, o := callH(c, "POST", "/instances/"+ids[0]+"/apps/"+headDep+"/stop", map[string]any{}, nil); code != 409 || o["code"] != "app_cluster_managed" {
		t.Fatalf("member deployment stop = %d %v", code, o)
	}

	// 整组续费：两张 renew 订单，每个成员加同样的时长；同键重放返回同一批订单
	before := e.inst(ids[0]).ExpiresAt
	code, ren := callH(c, "POST", "/clusters/"+cid+"/renew", map[string]any{"duration": "1d", "quantity": 1}, map[string]string{"Idempotency-Key": "clu-renew-1"})
	if code != 200 || len(ren["orders"].([]any)) != 2 {
		t.Fatalf("renew = %d %v", code, ren)
	}
	if _, again := callH(c, "POST", "/clusters/"+cid+"/renew", map[string]any{"duration": "1d", "quantity": 1}, map[string]string{"Idempotency-Key": "clu-renew-1"}); rawJSON(again["orders"]) != rawJSON(ren["orders"]) {
		t.Fatalf("renew replay = %v, want %v", again["orders"], ren["orders"])
	}
	if after := e.inst(ids[0]).ExpiresAt; after == nil || before == nil || after.Sub(*before) != 24*time.Hour {
		t.Fatalf("expires %v → %v", before, after)
	}
	var renewOrders int
	e.scalar(&renewOrders, `SELECT count(*) FROM orders WHERE user_id = $1 AND kind = 'renew'`, uid)
	if renewOrders != 2 {
		t.Fatalf("renew orders = %d", renewOrders)
	}

	// 释放：全员洗机 → 集群 released、机器回到空闲
	if code, o := callH(c, "POST", "/clusters/"+cid+"/release", map[string]any{}, nil); code != 200 {
		t.Fatalf("release = %d %v", code, o)
	}
	e.agents()
	e.sweep()
	for _, id := range ids {
		if st := e.inst(id).State; st != rental.StateReleased {
			t.Fatalf("member %s after release = %s", id, st)
		}
	}
	if view := c.do("GET", "/clusters/"+cid, nil); view["status"] != "released" || view["ended_at"] == nil {
		t.Fatalf("cluster after release = %v", view)
	}
	for _, mid := range mids {
		if m, err := e.a.Fleet.Get(e.ctx, e.pool, mid); err != nil || m.State != "idle" {
			t.Fatalf("machine %d after release = %+v %v", mid, m.State, err)
		}
	}
}

// 开通期中止（规格 §16.2 第 2 条，Review Focus 3）：rank1 的集群网络 Apply 失败（"cluster-net: " 前缀）→ rank1 failed 并退款、
// 不判机器故障 → 下一轮清扫 rank0 洗机、两单全额退、集群 failed、告警 cluster_failed 恰一条 → 机器都回到空闲。
func TestClusterAbortE2E(t *testing.T) {
	e := newCluEnv(t)
	fab := e.fabric("lan", nil)
	mids := e.machines(2, fab, e2ehooks.NetLAN, nil)
	c, uid := e.user(10_000_000)
	balance0, err := e.a.Wallet.Balance(e.ctx, uid)
	if err != nil {
		t.Fatal(err)
	}

	out := e.orderCluster(c, 2)
	cid := out["cluster"].(map[string]any)["id"].(string)
	ids := members(out["cluster"].(map[string]any))
	rank1 := e.inst(ids[1]).MachineID
	e.clusterNet(rank1, map[string]any{"fail_apply": "spc0 建立失败（fake）"})
	e.agents()
	if st := e.inst(ids[0]).State; st != rental.StateRunning {
		t.Fatalf("rank0 = %s", st)
	}
	if st := e.inst(ids[1]).State; st != rental.StateFailed {
		t.Fatalf("rank1 = %s", st)
	}
	e.sweep()
	if view := c.do("GET", "/clusters/"+cid, nil); view["status"] != "failed" || view["ended_at"] == nil {
		t.Fatalf("cluster after abort = %v", view)
	}
	if st := e.inst(ids[0]).State; st != rental.StateWiping {
		t.Fatalf("rank0 after abort = %s", st)
	}
	var unrefunded int
	e.scalar(&unrefunded, `SELECT count(*) FROM orders WHERE user_id = $1 AND refunded_amount <> amount`, uid)
	if unrefunded != 0 {
		t.Fatalf("%d orders not fully refunded", unrefunded)
	}
	if b, _ := e.a.Wallet.Balance(e.ctx, uid); b != balance0 {
		t.Fatalf("balance %d, want %d (full refund)", b, balance0)
	}
	if n := e.alerts("cluster_failed"); n != 1 {
		t.Fatalf("cluster_failed alerts = %d", n)
	}
	e.sweep() // 再扫一轮也只有一条告警
	if n := e.alerts("cluster_failed"); n != 1 {
		t.Fatalf("cluster_failed alerts after 2nd sweep = %d", n)
	}
	e.clusterNet(rank1, map[string]any{"fail_apply": ""})
	e.agents() // rank0 洗机；rank1 的容器由对账清掉
	e.sweep()
	e.agents()
	for _, mid := range mids {
		m, err := e.a.Fleet.Get(e.ctx, e.pool, mid)
		if err != nil || m.State != "idle" {
			t.Fatalf("machine %d after abort = %s %v（集群网络失败不判机器故障）", mid, m.State, err)
		}
	}
	if st := e.inst(ids[0]).State; st != rental.StateReleased {
		t.Fatalf("rank0 after wipe = %s", st)
	}
}

// 成员换机（规格 §16.2 第 3 条）：后台对 rank1 migrate → 新实例同 fabric、rank 1、集群版本 2、rank0 收到 configure_cluster(v2)、
// 新 Create 带 v2 Spec、集群应用 stale → fake Agent 完成 → 集群重新 ready → 重新部署后 running。
func TestClusterMemberMigrateE2E(t *testing.T) {
	e := newCluEnv(t)
	fab := e.fabric("lan", nil)
	e.machines(3, fab, e2ehooks.NetLAN, nil)
	other := e.fabric("lan", nil)
	e.machines(1, other, e2ehooks.NetLAN, nil) // 另一 fabric 的空闲机器不能接手
	c, _ := e.user(10_000_000)
	out := e.orderCluster(c, 2)
	cid := out["cluster"].(map[string]any)["id"].(string)
	ids := members(out["cluster"].(map[string]any))
	e.agents()
	e.sweep()
	vc := e.apps["vllm-cluster"]
	if code, dep := callH(c, "POST", "/clusters/"+cid+"/apps", map[string]any{"app_id": vc.ID, "env": map[string]string{"MODEL_ID": "Qwen/Qwen3-32B"}}, nil); code != 201 {
		t.Fatalf("deploy = %d %v", code, dep)
	}
	app := e.clusterApp(c, cid, appRunning)
	cdID := app["id"].(string)

	ac := e.adminLogin()
	mv := ac.do("POST", "/instances/"+ids[1]+"/migrate", map[string]any{"reason": "rank1 机器维修"})["instance"].(map[string]any)
	newID := mv["id"].(string)
	ni := e.inst(newID)
	if ni.ClusterID == nil || *ni.ClusterID != cid || ni.ClusterRank == nil || *ni.ClusterRank != 1 {
		t.Fatalf("migrated instance = %+v", ni)
	}
	if m, _ := e.a.Fleet.Get(e.ctx, e.pool, ni.MachineID); m.FabricID == nil || *m.FabricID != fab {
		t.Fatalf("new machine fabric = %v, want %d", m.FabricID, fab)
	}
	var version int
	e.scalar(&version, `SELECT version FROM instance_clusters WHERE id = $1`, cid)
	if version != 2 {
		t.Fatalf("cluster version = %d", version)
	}
	// 旧 rank1 已进入洗机（离开在位成员）、新实例 creating 在位：v2 Spec 的成员是 rank0 + 新 rank1
	if sp := e.createSpec(ni.MachineID, newID); sp.Version != 2 || sp.Rank != 1 || len(sp.Members) != 2 || sp.Members[0].InstanceID != ids[0] ||
		sp.Members[1].InstanceID != newID {
		t.Fatalf("new create spec = %+v", sp)
	}
	rank0 := e.inst(ids[0]).MachineID
	var cfg *fleet.ConfigureClusterPayload
	for _, cmd := range e.pendingOf(rank0) {
		if cmd.Type == fleet.CmdConfigureCluster && cmd.InstanceID == ids[0] {
			var p fleet.ConfigureClusterPayload
			if err := json.Unmarshal(cmd.Payload, &p); err != nil {
				t.Fatal(err)
			}
			cfg = &p
		}
	}
	if cfg == nil || cfg.Cluster.Version != 2 || cfg.Cluster.Rank != 0 || len(cfg.Cluster.Members) != 2 || cfg.Cluster.Members[1].InstanceID != newID {
		t.Fatalf("rank0 configure_cluster = %+v (pending %+v)", cfg, e.pendingOf(rank0))
	}
	if v := c.do("GET", "/clusters/"+cid+"/apps", nil); v["status"] != "stale" {
		t.Fatalf("cluster app after migrate = %v", v)
	}
	if v := ac.do("GET", "/clusters/"+cid, nil)["cluster"].(map[string]any); v["version"].(float64) != 2 {
		t.Fatalf("admin cluster view = %v", v)
	}

	e.agents() // 新实例 Create、旧实例洗机、rank0 configure_cluster
	e.sweep()
	if st := e.inst(newID).State; st != rental.StateRunning {
		t.Fatalf("new member = %s", st)
	}
	if st := e.inst(ids[1]).State; st != rental.StateReleased {
		t.Fatalf("old member = %s", st)
	}
	view := c.do("GET", "/clusters/"+cid, nil)
	if view["status"] != "ready" || len(view["members"].([]any)) != 2 || members(view)[1] != newID {
		t.Fatalf("cluster after migrate = %v", view)
	}
	if code, o := callH(c, "POST", "/clusters/"+cid+"/apps/"+cdID+"/redeploy", map[string]any{}, nil); code != 200 {
		t.Fatalf("redeploy = %d %v", code, o)
	}
	app = e.clusterApp(c, cid, func(v map[string]any) bool {
		if !appRunning(v) {
			return false
		}
		for _, m := range v["members"].([]any) {
			if m := m.(map[string]any); m["rank"].(float64) == 1 && m["instance_id"] != newID {
				return false
			}
		}
		return true
	})
	if app["id"] != cdID {
		t.Fatalf("redeployed app = %v", app)
	}
}

func rawJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// Task 16（Playwright）要用的 cx7 路径：vlan + RDMA fabric 上 4 台 cx7-rdma 机器 → 4 台集群（macvlan、VLAN 100、RDMA）→ ready、network rdma；
// direct fabric 上 2 台 cx7（无 RDMA）→ 2 台集群 → ready、network ethernet；旧 Agent 机器不入选；成员网络巡检异常 → degraded。
func TestClusterCX7FabricsE2E(t *testing.T) {
	e := newCluEnv(t)
	vlan := e.fabric("cx7", map[string]any{"isolation": "vlan", "rdma": true, "mtu": 9000})
	direct := e.fabric("cx7", nil)
	e.machines(4, vlan, e2ehooks.NetCX7RDMA, nil)
	e.machines(2, direct, e2ehooks.NetCX7, nil)
	e.hook("/hooks/machines", map[string]any{"count": 1, "fabric_id": vlan, "net": "cx7-rdma", "old_agent": true})
	if st, out := callH(&client{t: t, base: e.hooks.URL, http: http.DefaultClient}, "POST", "/hooks/machines", map[string]any{"count": 1, "fabric_id": direct, "net": "cx7"}, nil); st != 400 {
		t.Fatalf("third machine on a direct fabric = %d %v", st, out)
	}
	c, _ := e.user(20_000_000)
	products := c.do("GET", "/products", nil)["items"].([]any)
	cl := products[0].(map[string]any)["clusters"].(map[string]any)
	if cl["4"].(map[string]any)["available"].(float64) != 1 || cl["4"].(map[string]any)["rdma"] != true || cl["2"].(map[string]any)["available"].(float64) != 3 {
		t.Fatalf("product clusters = %v", cl)
	}

	four := e.orderCluster(c, 4)["cluster"].(map[string]any)
	if four["network"] != "rdma" || four["fabric_kind"] != "cx7" {
		t.Fatalf("4-node cluster = %v", four)
	}
	ids4 := members(four)
	for r, id := range ids4 {
		sp := e.createSpec(e.inst(id).MachineID, id)
		if sp.Mode != clusterspec.ModeMacvlan || sp.Iface != "enp1s0f1np1" || sp.VLAN != 100 || sp.MTU != 9000 || !sp.RDMA || sp.Rank != r || len(sp.Members) != 4 {
			t.Fatalf("rank %d spec = %+v", r, sp)
		}
	}
	two := e.orderCluster(c, 2)["cluster"].(map[string]any)
	if two["network"] != "ethernet" {
		t.Fatalf("2-node cluster = %v", two)
	}
	for _, id := range members(two) {
		if sp := e.createSpec(e.inst(id).MachineID, id); sp.Mode != clusterspec.ModeMacvlan || sp.VLAN != 0 || sp.RDMA {
			t.Fatalf("direct spec = %+v", sp)
		}
	}
	// vlan fabric 剩下的旧 Agent 机器不入选：凑不出第三个集群
	if code, out := callH(c, "POST", "/bookings", map[string]any{"product_id": products[0].(map[string]any)["id"], "image_id": dbtest.ImageID(t, e.pool),
		"count": 2, "duration": "1d", "quantity": 1, "cluster": true}, nil); code != 409 || out["code"] != "no_capacity" {
		t.Fatalf("third cluster = %d %v", code, out)
	}
	e.agents()
	e.sweep()
	for _, cv := range []map[string]any{four, two} {
		v := c.do("GET", "/clusters/"+cv["id"].(string), nil)
		if v["status"] != "ready" {
			t.Fatalf("cluster %s = %v", cv["id"], v)
		}
	}
	// 成员集群网络巡检异常（/hooks/agent/cluster-net check_ok=false）→ 降级、告警 cluster_net_error；恢复后 ready
	mid := e.inst(members(two)[1]).MachineID
	e.clusterNet(mid, map[string]any{"check_ok": false})
	if v := c.do("GET", "/clusters/"+two["id"].(string), nil); v["status"] != "degraded" {
		t.Fatalf("cluster with net error = %v", v)
	}
	if e.alerts("cluster_net_error") != 1 {
		t.Fatalf("cluster_net_error alerts = %d", e.alerts("cluster_net_error"))
	}
	e.clusterNet(mid, map[string]any{"check_ok": true})
	if v := c.do("GET", "/clusters/"+two["id"].(string), nil); v["status"] != "ready" {
		t.Fatalf("cluster after net recovered = %v", v)
	}
}

// 装配：rental 的集群网段池取 SPARK_CLUSTER_SUBNET_POOL（config.Load 给默认 10.210.0.0/16）；Config 字面量零值时 rental 退回 clusterspec.DefaultPool。
func TestBuildWiresClusterSubnetPool(t *testing.T) {
	pool := dbtest.New(t)
	for _, tc := range []struct{ cfg, want string }{{"10.77.0.0/16", "10.77.0.0/16"}, {"", clusterspec.DefaultPool}} {
		cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir(),
			UpstreamSSHKeyFile: upstreamKeyFile(t)}
		if tc.cfg != "" {
			cfg.ClusterSubnetPool = netip.MustParsePrefix(tc.cfg)
		}
		a, err := app.Build(cfg, pool, discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)})
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Rental.SubnetPool().String(); got != tc.want {
			t.Fatalf("cfg %q: rental subnet pool = %s, want %s", tc.cfg, got, tc.want)
		}
		a.Upstream.Close()
	}
}
