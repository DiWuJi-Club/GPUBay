package adminapi_test

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func productIDOf(e *env) int64 { return dbtest.ProductID(e.t, e.pool) }
func imageIDOf(e *env) int64   { return dbtest.ImageID(e.t, e.pool) }

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// reportClusterOK 模拟每台成员机器的 StateReport：实例 running、集群网络 ok。
func (e *env) reportClusterOK(insts []rental.Instance) {
	e.t.Helper()
	for _, i := range insts {
		if err := e.fleet.RecordStateReport(e.ctx, i.MachineID, fleet.StateReport{Instances: []fleet.InstanceState{
			{InstanceID: i.ID, Phase: fleet.PhaseRunning, Cluster: rental.NetStatusOK}}}); err != nil {
			e.t.Fatal(err)
		}
	}
}

// lanCluster 建 lan fabric + n 台就绪机器，并给新用户下一个就绪的 n 台集群。
func (e *env) lanCluster(ops *client, n int, phone string) (int64, int64, rental.ClusterView) {
	e.t.Helper()
	fab := ops.createFabric("以太网-"+phone, "lan", "routed")
	for k := 0; k < n; k++ {
		e.clusterMachine(ops, "SN-C"+phone+"-"+strconv.Itoa(k), fab, 30+k+int(fab)*10)
	}
	uid := e.renter(phone)
	return fab, uid, e.userCluster(uid, n)
}

// seedClusterApp 预置一个已发布的集群应用 vllm-cluster（spec 取仓库 deploy/apps/vllm-cluster/spec.json），兼容测试镜像。
func (e *env) seedClusterApp() int64 {
	e.t.Helper()
	raw, err := os.ReadFile("../../deploy/apps/vllm-cluster/spec.json")
	if err != nil {
		e.t.Fatal(err)
	}
	var spec appdeploy.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		e.t.Fatal(err)
	}
	if err := appdeploy.ValidateSpec(&spec); err != nil {
		e.t.Fatal(err)
	}
	b, _ := json.Marshal(spec)
	now := e.clk.Now()
	appID := e.qint(`INSERT INTO apps (slug, name, category, icon, status, created_at, updated_at)
		VALUES ('vllm-cluster', 'vLLM 多机', 'llm', 'llm', 'published', $1, $1) RETURNING id`, now)
	e.exec(`INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, published_at, created_at, updated_at)
		VALUES ($1, '1.0.0', 'published', $2, $3, 'true', $4, $5, $5, $5)`,
		appID, []int64{imageIDOf(e)}, b, appdeploy.SpecHash(spec, "true"), now)
	return appID
}

func TestAdminClustersListFilter(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)
	fab, uid, c := e.lanCluster(ops, 2, "13600000021")
	if c.Status != rental.ClusterReady {
		t.Fatalf("cluster status = %s", c.Status)
	}

	code, out := fin.do("GET", "/clusters", nil)
	items, _ := out["items"].([]any)
	if code != 200 || out["total"].(float64) != 1 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	row := items[0].(map[string]any)
	if row["id"] != c.ID || row["status"] != "ready" || row["state"] != "active" || row["fabric_id"].(float64) != float64(fab) ||
		row["user_id"].(float64) != float64(uid) || row["system"] != false || row["size"].(float64) != 2 || len(row["members"].([]any)) != 2 ||
		row["network"] != "ethernet" || row["subnet"] == "" {
		t.Fatalf("row = %v", row)
	}
	for q, want := range map[string]int{
		"?state=active": 1, "?state=released": 0, "?user_id=" + itoa(uid): 1, "?user_id=999999": 0,
		"?fabric_id=" + itoa(fab): 1, "?fabric_id=999999": 0, "?system=false": 1, "?system=true": 0, "?page=2": 0,
	} {
		code, out := fin.do("GET", "/clusters"+q, nil)
		if code != 200 || len(out["items"].([]any)) != want {
			t.Errorf("list %s = %d %v, want %d", q, code, out, want)
		}
	}
	for _, q := range []string{"?state=forming", "?system=yes", "?user_id=x", "?fabric_id=-1", "?page=0"} {
		if code, out := fin.do("GET", "/clusters"+q, nil); code != 400 {
			t.Errorf("bad %s = %d %v", q, code, out)
		}
	}
}

func TestAdminClusterDetail(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)
	fab, uid, c := e.lanCluster(ops, 2, "13600000031")

	code, out := fin.do("GET", "/clusters/"+c.ID, nil)
	if code != 200 {
		t.Fatalf("detail = %d %v", code, out)
	}
	cv := out["cluster"].(map[string]any)
	members := cv["members"].([]any)
	if cv["id"] != c.ID || cv["fabric_id"].(float64) != float64(fab) || cv["fabric_name"] == "" || len(members) != 2 || out["app"] != nil {
		t.Fatalf("detail = %v", out)
	}
	m0 := members[0].(map[string]any)
	if m0["rank"].(float64) != 0 || m0["machine_id"].(float64) != float64(c.Members[0].MachineID) || m0["net_status"] != "ok" ||
		m0["state"] != "running" || m0["ip"] != c.HeadIP || m0["machine_label"] == "" {
		t.Fatalf("member 0 = %v", m0)
	}
	evs := out["events"].([]any)
	if len(evs) < 4 { // 每个成员至少 → creating、→ running 两条
		t.Fatalf("events = %v", evs)
	}
	var last time.Time
	for i, x := range evs {
		ev := x.(map[string]any)
		at, _ := time.Parse(time.RFC3339Nano, ev["at"].(string))
		if ev["instance_id"] == "" || (i > 0 && at.After(last)) {
			t.Fatalf("events not merged newest first: %v", evs)
		}
		last = at
	}
	if code, out := fin.do("GET", "/clusters/clu-missing000", nil); code != 404 || codeOf(out) != "cluster_not_found" {
		t.Fatalf("missing = %d %v", code, out)
	}

	// 集群应用：部署后详情带 app（聚合状态 + 成员角色）
	appID := e.seedClusterApp()
	cd, err := e.apps.CreateCluster(e.ctx, appdeploy.ClusterCreateRequest{UserID: uid, ClusterID: c.ID, AppID: appID,
		Env: map[string]string{"MODEL_ID": "Qwen/Qwen3-0.6B"}, IP: "1.2.3.4"})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	code, out = fin.do("GET", "/clusters/"+c.ID, nil)
	app, _ := out["app"].(map[string]any)
	if code != 200 || app == nil || app["id"] != cd.ID || app["app_slug"] != "vllm-cluster" || app["version"] != "1.0.0" ||
		app["status"] != cd.Status || app["state"] != "active" || app["user_id"].(float64) != float64(uid) {
		t.Fatalf("detail with app = %d %v", code, out)
	}
	am := app["members"].([]any)
	if len(am) != 2 {
		t.Fatalf("app members = %v", am)
	}
	head := am[0].(map[string]any)
	if head["rank"].(float64) != 0 || head["role"] != "head" || head["current"] != true || head["instance_id"] != c.Members[0].InstanceID ||
		head["deployment_id"] == "" || head["deployment"].(map[string]any)["app_slug"] != "vllm-cluster" {
		t.Fatalf("head member = %v", head)
	}
	if am[1].(map[string]any)["role"] != "worker" {
		t.Fatalf("worker member = %v", am[1])
	}

	// 成员部署不能单条强制卸载；整组强制卸载（ops）
	if code, out := ops.do("POST", "/deployments/"+head["deployment_id"].(string)+"/force-remove", map[string]any{"reason": "违规"}); code != 409 || codeOf(out) != "app_cluster_managed" {
		t.Fatalf("force remove member = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/clusters/"+c.ID+"/app/force-remove", map[string]any{"reason": "违规"}); code != 403 {
		t.Fatalf("finance force remove = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/clusters/"+c.ID+"/app/force-remove", map[string]any{"reason": " "}); code != 400 {
		t.Fatalf("force remove without reason = %d %v", code, out)
	}
	code, out = ops.do("POST", "/clusters/"+c.ID+"/app/force-remove", map[string]any{"reason": "违规"})
	if code != 200 || out["id"] != cd.ID || out["state"] != "removed" {
		t.Fatalf("force remove cluster app = %d %v", code, out)
	}
	if n := e.adminAudits("cluster.app_force_remove", c.ID); n != 1 {
		t.Fatalf("audit = %d", n)
	}
	if code, out := ops.do("POST", "/clusters/"+c.ID+"/app/force-remove", map[string]any{"reason": "违规"}); code != 404 || codeOf(out) != "not_found" {
		t.Fatalf("force remove again = %d %v", code, out)
	}
}

func TestAdminClusterExtendResync(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)
	_, _, c := e.lanCluster(ops, 2, "13600000041")
	before := map[string]time.Time{}
	for _, m := range c.Members {
		before[m.InstanceID] = *m.ExpiresAt
	}

	if code, out := fin.do("POST", "/clusters/"+c.ID+"/extend", map[string]any{"seconds": 3600, "reason": "故障补偿"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance extend = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/clusters/"+c.ID+"/extend", map[string]any{"seconds": 10, "reason": "x"}); code != 400 {
		t.Fatalf("extend too short = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/clusters/"+c.ID+"/extend", map[string]any{"seconds": 3600, "reason": " "}); code != 400 {
		t.Fatalf("extend without reason = %d %v", code, out)
	}
	code, out := ops.do("POST", "/clusters/"+c.ID+"/extend", map[string]any{"seconds": 3600, "reason": "故障补偿"})
	if code != 200 || out["id"] != c.ID {
		t.Fatalf("extend = %d %v", code, out)
	}
	for _, x := range out["members"].([]any) {
		m := x.(map[string]any)
		at, _ := time.Parse(time.RFC3339Nano, m["expires_at"].(string))
		if !at.Equal(before[m["instance_id"].(string)].Add(time.Hour)) {
			t.Fatalf("member %v expires %v, before %v", m["instance_id"], at, before[m["instance_id"].(string)])
		}
	}
	if code, out := ops.do("POST", "/clusters/clu-missing000/extend", map[string]any{"seconds": 3600, "reason": "x"}); code != 404 || codeOf(out) != "cluster_not_found" {
		t.Fatalf("extend missing = %d %v", code, out)
	}

	if code, out := fin.do("POST", "/clusters/"+c.ID+"/resync", map[string]any{}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance resync = %d %v", code, out)
	}
	cmdsBefore := e.count(`SELECT count(*) FROM agent_commands WHERE type = 'configure_cluster'`)
	code, out = ops.do("POST", "/clusters/"+c.ID+"/resync", map[string]any{})
	if code != 200 || out["version"].(float64) != float64(c.Version+1) {
		t.Fatalf("resync = %d %v", code, out)
	}
	if n := e.count(`SELECT count(*) FROM agent_commands WHERE type = 'configure_cluster'`) - cmdsBefore; n != 2 {
		t.Fatalf("configure_cluster enqueued = %d", n)
	}
	for _, action := range []string{"cluster.extend", "cluster.resync"} {
		if n := e.adminAudits(action, c.ID); n != 1 {
			t.Errorf("audit %s = %d", action, n)
		}
	}
}

func TestInstanceDetailClusterBlock(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)
	_, _, c := e.lanCluster(ops, 2, "13600000051")
	member := c.Members[1]

	code, out := fin.do("GET", "/instances/"+member.InstanceID, nil)
	if code != 200 {
		t.Fatalf("detail = %d %v", code, out)
	}
	inst := out["instance"].(map[string]any)
	cl, _ := out["cluster"].(map[string]any)
	if inst["cluster_id"] != c.ID || inst["cluster_rank"].(float64) != 1 || cl == nil || cl["id"] != c.ID || cl["rank"].(float64) != 1 ||
		cl["size"].(float64) != 2 || cl["status"] != "ready" || len(cl["members"].([]any)) != 2 {
		t.Fatalf("instance cluster block = %v / %v", inst, out["cluster"])
	}
	// 列表行同样带 cluster_id / cluster_rank
	code, out = fin.do("GET", "/instances?user_id="+itoa(c.UserID), nil)
	if code != 200 || out["items"].([]any)[0].(map[string]any)["cluster_id"] != c.ID {
		t.Fatalf("list = %d %v", code, out)
	}

	// 非成员：cluster 为 null，cluster_id / cluster_rank 为 null
	e.machine("SN-PLAIN")
	single := e.place(e.renter("13600000052"))
	e.ack()
	code, out = fin.do("GET", "/instances/"+single.ID, nil)
	if code != 200 || out["cluster"] != nil || out["instance"].(map[string]any)["cluster_id"] != nil || out["instance"].(map[string]any)["cluster_rank"] != nil {
		t.Fatalf("single detail = %d %v", code, out)
	}
}

func TestInstanceExtendMember409(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	_, _, c := e.lanCluster(ops, 2, "13600000061")
	code, out := ops.do("POST", "/instances/"+c.Members[0].InstanceID+"/extend", map[string]any{"seconds": 3600, "reason": "补偿"})
	if code != 409 || codeOf(out) != "cluster_member" {
		t.Fatalf("member extend = %d %v", code, out)
	}
}

func TestDashboardClusters(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)
	code, out := fin.do("GET", "/dashboard", nil)
	if code != 200 {
		t.Fatalf("dashboard = %d %v", code, out)
	}
	if cl := out["clusters"].(map[string]any); cl["active"].(float64) != 0 || cl["ready"].(float64) != 0 || cl["degraded"].(float64) != 0 {
		t.Fatalf("empty clusters = %v", cl)
	}
	_, _, c := e.lanCluster(ops, 2, "13600000071")
	// 第二个集群：成员网络 error → 组建期不就绪（forming）
	fab2 := ops.createFabric("以太网二", "lan", "routed")
	e.clusterMachine(ops, "SN-X1", fab2, 201)
	e.clusterMachine(ops, "SN-X2", fab2, 202)
	res, err := e.rental.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.renter("13600000072"), ProductID: productIDOf(e), ImageID: imageIDOf(e),
		Count: 2, Duration: "1d", Quantity: 1, Cluster: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = res
	// 第一个集群的 rank1 掉成 error → degraded
	m := c.Members[1]
	if err := e.fleet.RecordStateReport(e.ctx, m.MachineID, fleet.StateReport{Instances: []fleet.InstanceState{
		{InstanceID: m.InstanceID, Phase: fleet.PhaseRunning, Cluster: "error: link down"}}}); err != nil {
		t.Fatal(err)
	}
	code, out = fin.do("GET", "/dashboard", nil)
	cl := out["clusters"].(map[string]any)
	if code != 200 || cl["active"].(float64) != 2 || cl["ready"].(float64) != 0 || cl["degraded"].(float64) != 1 || cl["forming"].(float64) != 1 {
		t.Fatalf("clusters = %d %v", code, cl)
	}
}

// 终审 B-2：后台集群视图保留 Agent 上报的原始集群网络状态（控制台只给归一化值）。
func TestAdminClusterNetStatusRaw(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	_, _, c := e.lanCluster(ops, 2, "13600000091")
	raw := "error: ip link add link enP7s7 name scv1234 type vlan id 1234: exit status 2"
	m := c.Members[1]
	if err := e.fleet.RecordStateReport(e.ctx, m.MachineID, fleet.StateReport{Instances: []fleet.InstanceState{
		{InstanceID: m.InstanceID, Phase: fleet.PhaseRunning, Cluster: raw}}}); err != nil {
		t.Fatal(err)
	}
	code, out := ops.do("GET", "/clusters/"+c.ID, nil)
	if code != 200 {
		t.Fatalf("detail = %d %v", code, out)
	}
	m1 := out["cluster"].(map[string]any)["members"].([]any)[1].(map[string]any)
	if m1["net_status"] != raw {
		t.Fatalf("admin member 1 net_status = %v", m1["net_status"])
	}
}
