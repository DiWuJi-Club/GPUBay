package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
)

// 计划 13 Task 11：控制台集群接口（规格 §10.1）。

// clusterFabric 建一个 lan fabric（routed），返回 ID。
func (e *testEnv) clusterFabric(name string) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO cluster_fabrics (name, kind, isolation, rdma, created_at, updated_at)
		VALUES ($1, 'lan', 'routed', false, $2, $2) RETURNING id`, name, e.clk.Now()).Scan(&id); err != nil {
		e.t.Fatalf("fabric: %v", err)
	}
	return id
}

// clusterMachines 新登记 n 台机器加入 lan fabric（新 Agent、网卡已上报、cluster_ready），lan_ip = 192.168.7.<10+k>。
func (e *testEnv) clusterMachines(fabricID int64, n int) []int64 {
	e.t.Helper()
	var ids []int64
	for k := 0; k < n; k++ {
		var seq int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM machines`).Scan(&seq); err != nil {
			e.t.Fatal(err)
		}
		ip := fmt.Sprintf("192.168.7.%d", 10+seq)
		id := dbtest.CreateMachine(e.t, e.pool, fmt.Sprintf("CLU-%d-%d", fabricID, seq))
		info, _ := json.Marshal(clusterspec.NetInfo{LanIface: "eth0", Ifaces: []clusterspec.Iface{
			{Name: "eth0", Up: true, MTU: 1500, IPv4: []string{ip + "/24"}, Driver: "r8169"}}})
		e.exec(`UPDATE machines SET fabric_id = $2, lan_ip = $3, agent_features = 3, net_info = $4, net_info_at = $5,
			cluster_ready = true, cluster_rdma = false, cluster_net = '192.168.7.0/24' WHERE id = $1`, id, fabricID, ip, info, e.clk.Now())
		ids = append(ids, id)
	}
	return ids
}

// clusterBody 是 n 台集群的即时下单请求体（1 天）。
func (e *testEnv) clusterBody(n int) map[string]any {
	return map[string]any{"product_id": dbtest.ProductID(e.t, e.pool), "image_id": dbtest.ImageID(e.t, e.pool), "count": n,
		"duration": "1d", "quantity": 1, "cluster": true}
}

// orderCluster 下一个 n 台集群，返回响应里的 cluster 视图。
func (e *testEnv) orderCluster(c *client, n int) map[string]any {
	e.t.Helper()
	code, out := c.do("POST", "/bookings", e.clusterBody(n))
	if code != 201 || out["cluster"] == nil {
		e.t.Fatalf("cluster booking = %d %v", code, out)
	}
	return out["cluster"].(map[string]any)
}

// memberIDs 集群视图里成员的实例 ID（按 rank）。
func memberIDs(cv map[string]any) []string {
	var ids []string
	for _, m := range cv["members"].([]any) {
		ids = append(ids, m.(map[string]any)["instance_id"].(string))
	}
	return ids
}

// runCluster 让集群全员开通成功、集群网络 ok，并跑一次 SweepClusters（首次就绪）。
func (e *testEnv) runCluster(cv map[string]any) {
	e.t.Helper()
	ctx := context.Background()
	for _, id := range memberIDs(cv) {
		mid := e.machineOf(id)
		cmds, err := e.fleet.PendingCommands(ctx, mid)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, cmd := range cmds {
			if err := e.rental.HandleCommandResult(ctx, cmd.ID, true, "ok"); err != nil {
				e.t.Fatal(err)
			}
		}
		if err := e.fleet.RecordStateReport(ctx, mid, fleet.StateReport{Instances: []fleet.InstanceState{{InstanceID: id, Phase: "running", Cluster: "ok"}}}); err != nil {
			e.t.Fatal(err)
		}
	}
	if _, err := e.rental.SweepClusters(ctx); err != nil {
		e.t.Fatal(err)
	}
}

// assertNoHiddenClusterFields：控制台的集群视图不带机器 ID、fabric ID、子网序号（规格 §10.1）。
func assertNoHiddenClusterFields(t *testing.T, v any) {
	t.Helper()
	raw := rawJSON(v)
	for _, k := range []string{`"machine_id"`, `"fabric_id"`, `"subnet_index"`, `"fabric_name"`, `"user_id"`, `"vlan"`} {
		if strings.Contains(raw, k) {
			t.Fatalf("cluster view leaks %s: %s", k, raw)
		}
	}
}

func TestBookingsClusterFlag(t *testing.T) {
	e := newTestEnv(t)
	e.clusterMachines(e.clusterFabric("L1"), 2)
	c, _ := e.fundedClient("13800001101")

	body := e.clusterBody(3)
	if code, out := c.do("POST", "/bookings", body); code != 400 || out["code"] != "cluster_size_invalid" {
		t.Fatalf("3 machines = %d %v", code, out)
	}
	body = e.clusterBody(2)
	body["start_at"] = dbtest.T0.Add(3 * time.Hour).Format(time.RFC3339)
	if code, out := c.do("POST", "/bookings", body); code != 400 || out["code"] != "cluster_booking_unsupported" {
		t.Fatalf("cluster booking with start_at = %d %v", code, out)
	}
	code, out := c.doH("POST", "/bookings", e.clusterBody(2), map[string]string{"Idempotency-Key": "clu-1"})
	if code != 201 {
		t.Fatalf("cluster order = %d %v", code, out)
	}
	cv := out["cluster"].(map[string]any)
	insts := out["instances"].([]any)
	if len(insts) != 2 || len(out["orders"].([]any)) != 2 || cv["size"].(float64) != 2 || cv["status"] != "forming" || cv["network"] != "ethernet" ||
		cv["fabric_kind"] != "lan" || !strings.HasPrefix(cv["id"].(string), "clu-") || len(cv["members"].([]any)) != 2 || cv["head_ip"] == "" {
		t.Fatalf("cluster order result = %v", out)
	}
	assertNoHiddenClusterFields(t, cv)
	for _, it := range insts {
		ic := it.(map[string]any)["cluster"].(map[string]any)
		if ic["id"] != cv["id"] || ic["size"].(float64) != 2 || ic["status"] != "forming" {
			t.Fatalf("member instance cluster = %v", ic)
		}
	}
	m0 := cv["members"].([]any)[0].(map[string]any)
	if m0["rank"].(float64) != 0 || m0["state"] != "creating" || m0["machine_label"] == "" || m0["ip"] != cv["head_ip"] || m0["net_status"] != "pending" {
		t.Fatalf("member 0 = %v", m0)
	}
	// 同键重放：同一个集群
	if code, again := c.doH("POST", "/bookings", e.clusterBody(2), map[string]string{"Idempotency-Key": "clu-1"}); code != 201 ||
		again["cluster"].(map[string]any)["id"] != cv["id"] {
		t.Fatalf("replay = %d %v", code, again)
	}
	// 批次查询也带集群
	if code, b := c.do("GET", "/bookings/"+out["batch"].(map[string]any)["id"].(string), nil); code != 200 || b["cluster"].(map[string]any)["id"] != cv["id"] {
		t.Fatalf("get batch = %d %v", code, b)
	}
	// 机器用完：no_capacity（集群文案）
	if code, out := c.do("POST", "/bookings", e.clusterBody(2)); code != 409 || out["code"] != "no_capacity" || !strings.Contains(out["message"].(string), "2 台集群") {
		t.Fatalf("no capacity = %d %v", code, out)
	}
	// 非集群下单的响应不带 cluster
	dbtest.CreateMachine(t, e.pool, "PLAIN-1")
	plain := map[string]any{"product_id": dbtest.ProductID(t, e.pool), "image_id": dbtest.ImageID(t, e.pool), "count": 1, "duration": "1d", "quantity": 1}
	if code, out := c.do("POST", "/bookings", plain); code != 201 || out["cluster"] != nil {
		t.Fatalf("plain booking = %d %v", code, out)
	} else if out["instances"].([]any)[0].(map[string]any)["cluster"] != nil {
		t.Fatalf("plain instance cluster = %v", out["instances"])
	}
}

func TestAvailabilityClusterSize(t *testing.T) {
	e := newTestEnv(t)
	e.clusterMachines(e.clusterFabric("L1"), 5)
	dbtest.CreateMachine(t, e.pool, "PLAIN-1") // 不在 fabric：不算集群
	anon := e.client()
	pid := itoa(dbtest.ProductID(t, e.pool))
	for _, tc := range []struct {
		size  string
		avail float64
	}{{"2", 2}, {"4", 1}} {
		code, out := anon.do("GET", "/availability?product_id="+pid+"&duration=1d&cluster_size="+tc.size, nil)
		if code != 200 || out["available"].(float64) != tc.avail || out["rdma"] != false || len(out) != 2 {
			t.Fatalf("cluster_size=%s = %d %v", tc.size, code, out)
		}
	}
	if code, out := anon.do("GET", "/availability?product_id="+pid+"&duration=1d&cluster_size=3", nil); code != 400 || out["code"] != "cluster_size_invalid" {
		t.Fatalf("cluster_size=3 = %d %v", code, out)
	}
	if code, out := anon.do("GET", "/availability?product_id="+pid+"&duration=1d&cluster_size=x", nil); code != 400 || out["code"] != "cluster_size_invalid" {
		t.Fatalf("cluster_size=x = %d %v", code, out)
	}
	start := dbtest.T0.Add(3 * time.Hour).Format(time.RFC3339)
	if code, out := anon.do("GET", "/availability?product_id="+pid+"&duration=1d&cluster_size=2&start_at="+start, nil); code != 400 || out["code"] != "cluster_booking_unsupported" {
		t.Fatalf("cluster_size with start_at = %d %v", code, out)
	}
	// 隐藏机组：与单机可用性同样 404
	g, err := e.fleet.CreateGroup(context.Background(), audit.Actor{AdminID: 1}, fleet.GroupInput{Name: "隐藏", Visible: false})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := anon.do("GET", "/availability?product_id="+pid+"&duration=1d&cluster_size=2&group_id="+itoa(g.ID), nil); code != 404 {
		t.Fatalf("hidden group = %d", code)
	}
	// 不带 cluster_size：原样
	if code, out := anon.do("GET", "/availability?product_id="+pid+"&duration=1d", nil); code != 200 || out["available"].(float64) != 6 || out["lower"] == nil {
		t.Fatalf("plain availability = %d %v", code, out)
	}
}

func TestProductsClusters(t *testing.T) {
	e := newTestEnv(t)
	e.clusterMachines(e.clusterFabric("L1"), 4)
	code, out := e.client().do("GET", "/products", nil)
	if code != 200 {
		t.Fatalf("products = %d %v", code, out)
	}
	for _, it := range out["items"].([]any) {
		p := it.(map[string]any)
		cl, ok := p["clusters"].(map[string]any)
		if !ok || len(cl) != 2 {
			t.Fatalf("product %v: clusters = %v", p["id"], p["clusters"])
		}
		want2, want4 := 0.0, 0.0
		if p["id"].(float64) == float64(dbtest.ProductID(t, e.pool)) {
			want2, want4 = 2, 1
		}
		if cl["2"].(map[string]any)["available"].(float64) != want2 || cl["4"].(map[string]any)["available"].(float64) != want4 ||
			cl["2"].(map[string]any)["rdma"] != false {
			t.Fatalf("product %v clusters = %v", p["id"], cl)
		}
	}
}

func TestClustersListAndDetail(t *testing.T) {
	e := newTestEnv(t)
	e.clusterMachines(e.clusterFabric("L1"), 4)
	c, _ := e.fundedClient("13800001111")
	cv := e.orderCluster(c, 2)
	id := cv["id"].(string)
	e.runCluster(cv)

	code, out := c.do("GET", "/clusters", nil)
	if code != 200 || len(out["items"].([]any)) != 1 || out["items"].([]any)[0].(map[string]any)["id"] != id {
		t.Fatalf("list = %d %v", code, out)
	}
	assertNoHiddenClusterFields(t, out)
	code, d := c.do("GET", "/clusters/"+id, nil)
	if code != 200 || d["status"] != "ready" || d["size"].(float64) != 2 || d["subnet"] == "" || d["expires_at"] == nil || d["auto_renew"] != false ||
		d["auto_renew_duration"] != "1d" || d["created_at"] == nil || d["ready_at"] == nil || d["expires_mismatch"] != false {
		t.Fatalf("detail = %d %v", code, d)
	}
	if opts := d["renew_options"].([]any); len(opts) != 3 || opts[0].(map[string]any)["quantity_max"].(float64) < 1 {
		t.Fatalf("renew_options = %v", d["renew_options"])
	}
	if _, has := d["app"]; !has || d["app"] != nil {
		t.Fatalf("app = %v (want null)", d["app"])
	}
	for _, m := range d["members"].([]any) {
		if mm := m.(map[string]any); mm["state"] != "running" || mm["net_status"] != "ok" || mm["expires_at"] == nil {
			t.Fatalf("member = %v", mm)
		}
	}
	assertNoHiddenClusterFields(t, d)
	// 成员到期不一致（> 60 秒）→ expires_mismatch
	e.exec(`UPDATE instances SET expires_at = expires_at + interval '2 minutes' WHERE id = $1`, memberIDs(cv)[1])
	if _, d := c.do("GET", "/clusters/"+id, nil); d["expires_mismatch"] != true {
		t.Fatalf("mismatch = %v", d["expires_mismatch"])
	}

	// 别人的集群、不存在、系统集群：404 cluster_not_found
	other, _ := e.fundedClient("13800001112")
	for _, p := range []string{"/clusters/" + id, "/clusters/clu-nope000000"} {
		if code, out := other.do("GET", p, nil); code != 404 || out["code"] != "cluster_not_found" {
			t.Fatalf("other GET %s = %d %v", p, code, out)
		}
	}
	if _, out := other.do("GET", "/clusters", nil); len(out["items"].([]any)) != 0 {
		t.Fatalf("other list = %v", out)
	}
	for _, p := range []struct {
		method, path string
		body         map[string]any
	}{{"POST", "/clusters/" + id + "/renew", map[string]any{"duration": "1d"}}, {"PUT", "/clusters/" + id + "/auto-renew", map[string]any{"enabled": false}},
		{"POST", "/clusters/" + id + "/release", map[string]any{}}} {
		if code, out := other.doH(p.method, p.path, p.body, map[string]string{"Idempotency-Key": "x-1"}); code != 404 ||
			out["code"] != "cluster_not_found" {
			t.Fatalf("other %s %s = %d %v", p.method, p.path, code, out)
		}
	}
	e.exec(`UPDATE instance_clusters SET system = true WHERE id = $1`, id)
	if code, out := c.do("GET", "/clusters/"+id, nil); code != 404 || out["code"] != "cluster_not_found" {
		t.Fatalf("system cluster = %d %v", code, out)
	}
	if _, out := c.do("GET", "/clusters", nil); len(out["items"].([]any)) != 0 {
		t.Fatalf("system cluster listed: %v", out)
	}
	if code, _ := e.client().do("GET", "/clusters", nil); code != 401 {
		t.Fatalf("anonymous = %d", code)
	}
}

func TestClusterRenewIdempotent(t *testing.T) {
	e := newTestEnv(t)
	e.clusterMachines(e.clusterFabric("L1"), 2)
	c, uid := e.fundedClient("13800001121")
	cv := e.orderCluster(c, 2)
	id := cv["id"].(string)
	e.runCluster(cv)
	_, before := c.do("GET", "/clusters/"+id, nil)
	balance := func() int64 {
		b, err := e.wallet.Balance(context.Background(), uid)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b0 := balance()

	if code, out := c.do("POST", "/clusters/"+id+"/renew", map[string]any{"duration": "1d"}); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("renew without key = %d %v", code, out)
	}
	code, out := c.doH("POST", "/clusters/"+id+"/renew", map[string]any{"duration": "1d", "quantity": 2}, map[string]string{"Idempotency-Key": "r-1"})
	if code != 200 || len(out["orders"].([]any)) != 2 || out["cluster"].(map[string]any)["id"] != id {
		t.Fatalf("renew = %d %v", code, out)
	}
	paid := b0 - balance()
	if paid != 2*2*6900 {
		t.Fatalf("paid = %d", paid)
	}
	exp0, _ := time.Parse(time.RFC3339, before["expires_at"].(string))
	exp1, _ := time.Parse(time.RFC3339, out["cluster"].(map[string]any)["expires_at"].(string))
	if exp1.Sub(exp0) != 48*time.Hour {
		t.Fatalf("expires %v → %v", exp0, exp1)
	}
	assertNoHiddenClusterFields(t, out["cluster"])
	o0 := out["orders"].([]any)[0].(map[string]any)["id"]
	code, again := c.doH("POST", "/clusters/"+id+"/renew", map[string]any{"duration": "1d", "quantity": 2}, map[string]string{"Idempotency-Key": "r-1"})
	if code != 200 || again["orders"].([]any)[0].(map[string]any)["id"] != o0 || b0-balance() != paid {
		t.Fatalf("replay = %d %v (paid %d)", code, again, b0-balance())
	}
	if code, out := c.doH("POST", "/clusters/"+id+"/renew", map[string]any{"duration": "1d"}, map[string]string{"Idempotency-Key": "bad#key"}); code != 400 {
		t.Fatalf("bad key = %d %v", code, out)
	}
	if code, out := c.doH("POST", "/clusters/"+id+"/renew", map[string]any{"duration": "9y"}, map[string]string{"Idempotency-Key": "r-2"}); code != 400 {
		t.Fatalf("bad duration = %d %v", code, out)
	}
}

func TestClusterAutoRenew(t *testing.T) {
	e := newTestEnv(t)
	e.clusterMachines(e.clusterFabric("L1"), 2)
	c, _ := e.fundedClient("13800001131")
	cv := e.orderCluster(c, 2)
	id := cv["id"].(string)
	code, out := c.do("PUT", "/clusters/"+id+"/auto-renew", map[string]any{"enabled": true, "duration": "1w"})
	if code != 200 || out["auto_renew"] != true || out["auto_renew_duration"] != "1w" || out["id"] != id {
		t.Fatalf("auto-renew on = %d %v", code, out)
	}
	assertNoHiddenClusterFields(t, out)
	for _, mid := range memberIDs(cv) {
		if _, inst := c.do("GET", "/instances/"+mid, nil); inst["auto_renew"] != true || inst["auto_renew_duration"] != "1w" {
			t.Fatalf("member %s = %v", mid, inst)
		}
	}
	if code, out := c.do("PUT", "/clusters/"+id+"/auto-renew", map[string]any{"enabled": false}); code != 200 || out["auto_renew"] != false || out["auto_renew_duration"] != "1w" {
		t.Fatalf("auto-renew off = %d %v", code, out)
	}
	if code, out := c.do("PUT", "/clusters/"+id+"/auto-renew", map[string]any{"enabled": true, "duration": "nope"}); code != 400 {
		t.Fatalf("bad duration = %d %v", code, out)
	}
}

func TestClusterRelease(t *testing.T) {
	e := newTestEnv(t)
	e.clusterMachines(e.clusterFabric("L1"), 2)
	c, _ := e.fundedClient("13800001141")
	cv := e.orderCluster(c, 2)
	id := cv["id"].(string)
	if code, out := c.do("POST", "/clusters/"+id+"/release", map[string]any{}); code != 409 || out["code"] != "invalid_state" {
		t.Fatalf("release while creating = %d %v", code, out)
	}
	e.runCluster(cv)
	if code, out := c.do("POST", "/clusters/"+id+"/release", map[string]any{}); code != 200 || out["ok"] != true {
		t.Fatalf("release = %d %v", code, out)
	}
	for _, mid := range memberIDs(cv) {
		if _, inst := c.do("GET", "/instances/"+mid, nil); inst["state"] != "wiping" {
			t.Fatalf("member %s after release = %v", mid, inst["state"])
		}
	}
	if code, out := c.do("POST", "/clusters/"+id+"/release", map[string]any{}); code != 409 {
		t.Fatalf("release twice = %d %v", code, out)
	}
	if n := e.countRows(`SELECT count(*) FROM audit_logs WHERE action = 'cluster.release' AND target_id = $1`, id); n != 1 {
		t.Fatalf("audit cluster.release = %d", n)
	}
}

func TestInstanceMemberGuards(t *testing.T) {
	e := newTestEnv(t)
	e.clusterMachines(e.clusterFabric("L1"), 2)
	c, _ := e.fundedClient("13800001151")
	cv := e.orderCluster(c, 2)
	e.runCluster(cv)
	mid := memberIDs(cv)[1]
	for _, p := range []struct {
		method, path string
		body         map[string]any
	}{{"POST", "/instances/" + mid + "/renew", map[string]any{"duration": "1d"}}, {"POST", "/instances/" + mid + "/release", map[string]any{}},
		{"PUT", "/instances/" + mid + "/auto-renew", map[string]any{"enabled": true, "duration": "1d"}}} {
		code, out := c.doH(p.method, p.path, p.body, map[string]string{"Idempotency-Key": "g-1"})
		if code != 409 || out["code"] != "cluster_member" {
			t.Fatalf("%s %s = %d %v", p.method, p.path, code, out)
		}
	}
}

func TestInstanceViewCluster(t *testing.T) {
	e := newTestEnv(t)
	e.clusterMachines(e.clusterFabric("L1"), 2)
	c, _ := e.fundedClient("13800001161")
	cv := e.orderCluster(c, 2)
	e.runCluster(cv)
	ids := memberIDs(cv)
	code, inst := c.do("GET", "/instances/"+ids[1], nil)
	ic, ok := inst["cluster"].(map[string]any)
	if code != 200 || !ok || ic["id"] != cv["id"] || ic["size"].(float64) != 2 || ic["rank"].(float64) != 1 || ic["status"] != "ready" || len(ic) != 4 {
		t.Fatalf("instance cluster = %d %v", code, inst["cluster"])
	}
	_, list := c.do("GET", "/instances", nil)
	for _, it := range list["items"].([]any) {
		if it.(map[string]any)["cluster"].(map[string]any)["id"] != cv["id"] {
			t.Fatalf("list item cluster = %v", it)
		}
	}
	// 非成员：cluster 为 null
	other := e.client()
	plain := other.runningInstance(t, e, "13800001162")
	if _, v := other.do("GET", "/instances/"+plain, nil); v["cluster"] != nil {
		t.Fatalf("plain instance cluster = %v", v["cluster"])
	} else if _, has := v["cluster"]; !has {
		t.Fatalf("plain instance has no cluster key: %v", v)
	}
}

// seedClusterApp 建一个只能集群部署的应用（required，2～4 台），已上架。
func (e *testEnv) seedClusterApp(mode string) (appdeploy.App, appdeploy.Version) {
	e.t.Helper()
	ctx := context.Background()
	actor := audit.Actor{AdminID: 1}
	slug := "vllm-cluster-" + mode
	a, err := e.appCat.CreateApp(ctx, actor, appdeploy.AppInput{Slug: slug, Name: "集群 " + mode, Summary: "多机", Description: "多机推理",
		Category: "llm", Icon: "llm", Status: appdeploy.StatusPublished})
	if err != nil {
		e.t.Fatal(err)
	}
	spec := vllmSpec()
	spec.Cluster = &appdeploy.ClusterSupport{Mode: mode, MinSize: 2, MaxSize: 4}
	v, err := e.appCat.CreateVersion(ctx, actor, a.ID, appdeploy.VersionInput{Version: "1.0.0", ImageIDs: []int64{dbtest.ImageID(e.t, e.pool)},
		Spec: spec, Script: appScript})
	if err != nil {
		e.t.Fatal(err)
	}
	if v, _, err = e.appCat.PublishVersion(ctx, actor, a.ID, v.ID); err != nil {
		e.t.Fatal(err)
	}
	return a, v
}

func TestAppCatalogClusterField(t *testing.T) {
	e := newAppsTestEnv(t, nil)
	e.seedApps()
	e.seedClusterApp(appdeploy.ClusterModeRequired)
	code, out := e.client().do("GET", "/apps", nil)
	if code != 200 {
		t.Fatalf("apps = %d %v", code, out)
	}
	found := false
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		cl, has := m["version"].(map[string]any)["cluster"]
		if m["slug"] == "vllm-cluster-required" {
			found = true
			c := cl.(map[string]any)
			if c["mode"] != "required" || c["min_size"].(float64) != 2 || c["max_size"].(float64) != 4 {
				t.Fatalf("cluster field = %v", c)
			}
		} else if has {
			t.Fatalf("non-cluster app %v has cluster = %v", m["slug"], cl)
		}
	}
	if !found {
		t.Fatal("cluster app not listed")
	}
	if _, one := e.client().do("GET", "/apps/vllm-cluster-required", nil); one["version"].(map[string]any)["cluster"].(map[string]any)["mode"] != "required" {
		t.Fatalf("catalog item cluster = %v", one["version"])
	}
}

func TestClusterAppsFlow(t *testing.T) {
	e := newAppsTestEnv(t, nil)
	apps := e.seedApps()
	capp, cver := e.seedClusterApp(appdeploy.ClusterModeRequired)
	e.clusterMachines(e.clusterFabric("L1"), 2)
	c, _ := e.fundedClient("13800001171")
	cv := e.orderCluster(c, 2)
	id := cv["id"].(string)
	e.runCluster(cv)
	ids := memberIDs(cv)
	base := "/clusters/" + id + "/apps"

	if code, out := c.do("GET", base, nil); code != 200 || out != nil {
		t.Fatalf("no app = %d %v", code, out)
	}
	// required 应用不能单实例部署；非集群应用不能集群部署
	if code, out := c.do("POST", "/instances/"+ids[0]+"/apps", map[string]any{"app_id": capp.ID}); code != 400 || out["code"] != "app_cluster_required" {
		t.Fatalf("single deploy of required app = %d %v", code, out)
	}
	if code, out := c.do("POST", base, map[string]any{"app_id": apps.vllm.ID}); code != 400 || out["code"] != "app_cluster_unsupported" {
		t.Fatalf("cluster deploy of plain app = %d %v", code, out)
	}
	code, av := c.do("POST", base, map[string]any{"app_id": capp.ID, "version_id": cver.ID, "env": map[string]string{"API_KEY": "abcdefgh"}})
	if code != 201 {
		t.Fatalf("deploy = %d %v", code, av)
	}
	acd := av["id"].(string)
	if !strings.HasPrefix(acd, "acd-") || av["app_id"].(float64) != float64(capp.ID) || av["app_slug"] != "vllm-cluster-required" || av["app_name"] != "集群 required" ||
		av["version_id"].(float64) != float64(cver.ID) || av["version"] != "1.0.0" || av["status"] != "installing" || av["url"] != nil || av["state"] != "active" {
		t.Fatalf("cluster app = %v", av)
	}
	ms := av["members"].([]any)
	if len(ms) != 2 {
		t.Fatalf("members = %v", ms)
	}
	for i, m := range ms {
		mm := m.(map[string]any)
		role := "worker"
		if i == 0 {
			role = "head"
		}
		if mm["rank"].(float64) != float64(i) || mm["role"] != role || mm["instance_id"] != ids[i] || mm["state"] != "queued" || mm["current"] != true ||
			mm["deployment_id"] == "" || mm["health"] == nil || mm["progress"] == nil {
			t.Fatalf("member %d = %v", i, mm)
		}
	}
	if se := av["secret_env"].(map[string]any); se["API_KEY"] != true {
		t.Fatalf("secret_env = %v", av["secret_env"])
	}
	if strings.Contains(rawJSON(av), "abcdefgh") {
		t.Fatal("cluster app view leaks secret")
	}
	if code, out := c.do("POST", base, map[string]any{"app_id": capp.ID}); code != 409 || out["code"] != "app_cluster_exists" {
		t.Fatalf("second deploy = %d %v", code, out)
	}
	if code, out := c.do("GET", base, nil); code != 200 || out["id"] != acd || len(out["members"].([]any)) != 2 {
		t.Fatalf("get = %d %v", code, out)
	}
	if code, out := c.do("GET", "/clusters/"+id, nil); code != 200 || out["app"].(map[string]any)["id"] != acd {
		t.Fatalf("detail app = %d %v", code, out["app"])
	}
	// 成员的应用视图带 cluster_role；成员部署的单实例写接口 → app_cluster_managed
	headDep := ms[0].(map[string]any)["deployment_id"].(string)
	if _, one := c.do("GET", "/instances/"+ids[0]+"/apps/"+headDep, nil); one["cluster_role"] != "head" || one["cluster_deployment_id"] != acd {
		t.Fatalf("member deployment view = %v", one)
	}
	dbase := "/instances/" + ids[0] + "/apps/" + headDep
	for _, p := range []struct{ method, path string }{{"POST", dbase + "/redeploy"}, {"POST", dbase + "/stop"}, {"POST", dbase + "/start"}, {"DELETE", dbase}} {
		if code, out := c.do(p.method, p.path, map[string]any{}); code != 409 || out["code"] != "app_cluster_managed" {
			t.Fatalf("%s %s = %d %v", p.method, p.path, code, out)
		}
	}

	// head 登记端口后有 url
	e.exec(`UPDATE app_deployments SET state = 'running', health = 'ok', progress = 100 WHERE cluster_deployment_id = $1`, acd)
	e.exec(`INSERT INTO instance_ports (instance_id, port, note, deployment_id, created_at, updated_at) VALUES ($1, 8000, 'vllm', $2, now(), now())`, ids[0], headDep)
	code, out := c.do("GET", base, nil)
	if code != 200 || out["status"] != "running" || out["health"] != "ok" || out["url"] == nil || !strings.Contains(out["url"].(string), ids[0]+"-8000.spark.test") {
		t.Fatalf("running = %d %v", code, out)
	}
	// 别人：404
	other, _ := e.fundedClient("13800001172")
	for _, p := range []struct{ method, path string }{{"GET", base}, {"POST", base}, {"POST", base + "/" + acd + "/stop"}, {"DELETE", base + "/" + acd}} {
		if code, out := other.do(p.method, p.path, map[string]any{"app_id": capp.ID}); code != 404 {
			t.Fatalf("other %s %s = %d %v", p.method, p.path, code, out)
		}
	}
	if code, out := c.do("POST", "/clusters/"+id+"/apps/acd-nope/stop", map[string]any{}); code != 404 {
		t.Fatalf("unknown acd = %d %v", code, out)
	}

	if code, out := c.do("POST", base+"/"+acd+"/stop", map[string]any{}); code != 200 || out["id"] != acd {
		t.Fatalf("stop = %d %v", code, out)
	}
	e.exec(`UPDATE app_deployments SET state = 'stopped' WHERE cluster_deployment_id = $1`, acd)
	if code, out := c.do("POST", base+"/"+acd+"/start", map[string]any{}); code != 200 || out["members"].([]any)[0].(map[string]any)["state"] != "queued" {
		t.Fatalf("start = %d %v", code, out)
	}
	e.exec(`UPDATE app_deployments SET state = 'failed' WHERE cluster_deployment_id = $1`, acd)
	code, out = c.do("POST", base+"/"+acd+"/redeploy", map[string]any{"env": map[string]string{"MODEL_ID": "Qwen/Qwen3-4B"}})
	if code != 200 || out["env"].(map[string]any)["MODEL_ID"] != "Qwen/Qwen3-4B" || out["secret_env"].(map[string]any)["API_KEY"] != true {
		t.Fatalf("redeploy env = %d %v", code, out)
	}
	e.exec(`UPDATE app_deployments SET state = 'failed' WHERE cluster_deployment_id = $1`, acd)
	code, out = c.do("POST", base+"/"+acd+"/redeploy", map[string]any{"version_id": cver.ID})
	if code != 200 || out["env"].(map[string]any)["MODEL_ID"] != "Qwen/Qwen3-4B" {
		t.Fatalf("redeploy keep env = %d %v", code, out)
	}
	e.exec(`UPDATE app_deployments SET state = 'failed' WHERE cluster_deployment_id = $1`, acd)
	code, out = c.do("POST", base+"/"+acd+"/redeploy", map[string]any{"env_set": true})
	if code != 200 || out["env"].(map[string]any)["MODEL_ID"] != "Qwen/Qwen3-8B" {
		t.Fatalf("redeploy env_set without env = %d %v", code, out)
	}
	e.exec(`UPDATE app_deployments SET state = 'installing' WHERE cluster_deployment_id = $1`, acd)
	if code, out := c.do("POST", base+"/"+acd+"/redeploy", map[string]any{}); code != 409 || out["code"] != "app_invalid_state" {
		t.Fatalf("redeploy installing = %d %v", code, out)
	}
	e.exec(`UPDATE app_deployments SET state = 'running' WHERE cluster_deployment_id = $1`, acd)
	if code, out := c.do("DELETE", base+"/"+acd, nil); code != 200 || out["status"] != "removing" {
		t.Fatalf("remove = %d %v", code, out)
	}
	if n := e.countRows(`SELECT count(*) FROM audit_logs WHERE action LIKE 'cluster.app_%' AND target_id = $1`, id); n < 5 {
		t.Fatalf("cluster app audits = %d", n)
	}
}

// 终审 B-2：成员集群网络状态在控制台归一为 ok / pending / error + 固定原因；Agent 原始错误（命令行、VLAN、网卡名、地址）不出控制台。
func TestClusterNetStatusConsoleNormalized(t *testing.T) {
	e := newTestEnv(t)
	e.clusterMachines(e.clusterFabric("L1"), 2)
	c, _ := e.fundedClient("13800001171")
	cv := e.orderCluster(c, 2)
	id := cv["id"].(string)
	// 开通中（机器未上报）：pending
	for _, m := range cv["members"].([]any) {
		if mm := m.(map[string]any); mm["net_status"] != "pending" || mm["net_reason"] != "" {
			t.Fatalf("creating member = %v", mm)
		}
	}
	e.runCluster(cv)
	ids := memberIDs(cv)
	raw := "error: ip link add link enP7s7 name scv1234 type vlan id 1234: exit status 2: RTNETLINK answers: File exists; " +
		"iptables -w -A SPARK-CLU -s 10.210.3.12/32 -d 192.168.7.10 -j ACCEPT"
	if err := e.fleet.RecordStateReport(context.Background(), e.machineOf(ids[1]), fleet.StateReport{Instances: []fleet.InstanceState{
		{InstanceID: ids[1], Phase: "running", Cluster: raw}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.fleet.RecordStateReport(context.Background(), e.machineOf(ids[0]), fleet.StateReport{Instances: []fleet.InstanceState{
		{InstanceID: ids[0], Phase: "running", Cluster: "pending"}}}); err != nil {
		t.Fatal(err)
	}
	_, list := c.do("GET", "/clusters", nil)
	_, detail := c.do("GET", "/clusters/"+id, nil)
	_, inst := c.do("GET", "/instances/"+ids[1], nil)
	_, insts := c.do("GET", "/instances", nil)
	for name, v := range map[string]any{"list": list, "detail": detail, "instance": inst, "instances": insts} {
		s := rawJSON(v)
		for _, leak := range []string{"ip link", "vlan", "scv1234", "enP7s7", "iptables", "SPARK-CLU", "192.168.7.", "RTNETLINK", "exit status"} {
			if strings.Contains(s, leak) {
				t.Fatalf("%s leaks %q: %s", name, leak, s)
			}
		}
	}
	if detail["status"] != "degraded" {
		t.Fatalf("status = %v", detail["status"])
	}
	ms := detail["members"].([]any)
	m0, m1 := ms[0].(map[string]any), ms[1].(map[string]any)
	if m0["net_status"] != "pending" || m0["net_reason"] != "" {
		t.Fatalf("member 0 = %v", m0)
	}
	if m1["net_status"] != "error" || m1["net_reason"] != "集群网络异常，平台已告警处理" {
		t.Fatalf("member 1 = %v", m1)
	}
}

// 终审 B-12：/clusters 列表里某个集群组装失败只跳过该项（记日志），不让整表 500。
func TestClustersListSkipsBrokenItem(t *testing.T) {
	e := newTestEnv(t)
	e.clusterMachines(e.clusterFabric("L1"), 4)
	c, _ := e.fundedClient("13800001181")
	good := e.orderCluster(c, 2)
	bad := e.orderCluster(c, 2)
	e.runCluster(good)
	e.runCluster(bad)
	// 让 bad 的一个成员实例读不到（归属错乱）：控制台视图组装失败
	_, otherUID := e.fundedClient("13800001182")
	e.exec(`UPDATE instances SET user_id = $2 WHERE id = $1`, memberIDs(bad)[1], otherUID)
	code, out := c.do("GET", "/clusters", nil)
	if code != 200 {
		t.Fatalf("list = %d %v", code, out)
	}
	items := out["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != good["id"] {
		t.Fatalf("items = %v", items)
	}
}
