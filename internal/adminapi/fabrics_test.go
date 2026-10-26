package adminapi_test

import (
	"fmt"
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/rental"
)

// ---- 计划 13 集群测试夹具 ----

// lanNet：一块有线管理网卡 eth0，IPv4 = ip/24。
func lanNet(ip string) clusterspec.NetInfo {
	return clusterspec.NetInfo{LanIface: "eth0", Ifaces: []clusterspec.Iface{
		{Name: "eth0", MAC: "aa:bb:cc:00:00:01", Up: true, SpeedMbps: 10000, MTU: 1500, IPv4: []string{ip + "/24"}, Driver: "r8169"},
	}}
}

// createFabric 经接口新建 fabric，返回 ID。
func (c *client) createFabric(name, kind, isolation string) int64 {
	c.e.t.Helper()
	code, out := c.do("POST", "/fabrics", map[string]any{"name": name, "kind": kind, "isolation": isolation, "rdma": false, "mtu": 0,
		"vlan_min": nil, "vlan_max": nil, "description": ""})
	if code != 201 {
		c.e.t.Fatalf("create fabric %s = %d %v", name, code, out)
	}
	return int64(out["id"].(float64))
}

// clusterMachine 登记一台机器（lan_ip = 192.168.1.<k>）、经 PUT /machines/{id} 加入 fabric，再模拟新 Agent 的 Hello（能力位 3 + 网卡），
// 机器即 cluster_ready（cluster_net = 192.168.1.0/24）。
func (e *env) clusterMachine(c *client, serial string, fabricID int64, k int) int64 {
	e.t.Helper()
	id := e.machine(serial)
	ip := fmt.Sprintf("192.168.1.%d", k)
	e.exec(`UPDATE machines SET lan_ip = $2 WHERE id = $1`, id, ip)
	if code, out := c.do("PUT", "/machines/"+itoa(id), map[string]any{"fabric_id": fabricID}); code != 200 {
		e.t.Fatalf("join fabric = %d %v", code, out)
	}
	net := lanNet(ip)
	if err := e.fleet.RecordHello(e.ctx, id, 3, &net); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// userCluster 下一个 n 台用户集群，fake Agent 完成全部 Create，成员上报集群网络 ok，再跑一次 SweepClusters（就绪）。
func (e *env) userCluster(uid int64, n int) rental.ClusterView {
	e.t.Helper()
	res, err := e.rental.PlaceBatch(e.ctx, rental.BatchRequest{UserID: uid, ProductID: productIDOf(e), ImageID: imageIDOf(e), Count: n,
		Duration: "1d", Quantity: 1, Cluster: true})
	if err != nil {
		e.t.Fatalf("PlaceBatch cluster: %v", err)
	}
	e.ack()
	e.reportClusterOK(res.Instances)
	if _, err := e.rental.SweepClusters(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	v, err := e.rental.GetClusterView(e.ctx, res.Cluster.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// ---- 用例 ----

func TestFabricsCRUDRoles(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)
	body := map[string]any{"name": "机房以太网", "kind": "lan", "isolation": "routed", "rdma": false, "mtu": 0, "description": "A 区"}

	if code, out := fin.do("POST", "/fabrics", body); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance create = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/fabrics", map[string]any{"name": "x", "kind": "lan", "isolation": "direct"}); code != 400 || codeOf(out) != "fabric_invalid" {
		t.Fatalf("invalid fabric = %d %v", code, out)
	}
	code, out := ops.do("POST", "/fabrics", body)
	if code != 201 || out["name"] != "机房以太网" || out["kind"] != "lan" || out["isolation"] != "routed" || out["description"] != "A 区" {
		t.Fatalf("create = %d %v", code, out)
	}
	id := itoa(int64(out["id"].(float64)))
	if code, out := ops.do("POST", "/fabrics", body); code != 400 || codeOf(out) != "fabric_invalid" {
		t.Fatalf("duplicate name = %d %v", code, out)
	}

	vmin, vmax := 100, 199
	code, out = ops.do("PUT", "/fabrics/"+id, map[string]any{"name": "交换机 CX-7", "kind": "cx7", "isolation": "vlan", "rdma": true, "mtu": 9000,
		"vlan_min": vmin, "vlan_max": vmax, "description": ""})
	if code != 200 || out["kind"] != "cx7" || out["rdma"] != true || out["vlan_min"].(float64) != 100 || out["mtu"].(float64) != 9000 {
		t.Fatalf("update = %d %v", code, out)
	}
	if code, out := fin.do("PUT", "/fabrics/"+id, body); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance update = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/fabrics/999999", body); code != 404 || codeOf(out) != "fabric_not_found" {
		t.Fatalf("update missing = %d %v", code, out)
	}

	// 列表（全员可看）：机器数、活跃集群、网段
	m := e.machine("SN-F1")
	if code, out := ops.do("PUT", "/machines/"+itoa(m), map[string]any{"fabric_id": int64(mustAtoi(t, id))}); code != 200 {
		t.Fatalf("join = %d %v", code, out)
	}
	code, out = fin.do("GET", "/fabrics", nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	row := items[0].(map[string]any)
	if row["machine_count"].(float64) != 1 || row["active_clusters"].(float64) != 0 || row["cluster_nets"] == nil || row["name"] != "交换机 CX-7" {
		t.Fatalf("list row = %v", row)
	}

	// 有机器不能删；移出后删除
	if code, out := ops.do("DELETE", "/fabrics/"+id, nil); code != 409 || codeOf(out) != "fabric_in_use" {
		t.Fatalf("delete busy = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m), map[string]any{"fabric_id": nil}); code != 200 || out["fabric_id"] != nil {
		t.Fatalf("leave = %d %v", code, out)
	}
	if code, out := fin.do("DELETE", "/fabrics/"+id, nil); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance delete = %d %v", code, out)
	}
	if code, out := ops.do("DELETE", "/fabrics/"+id, nil); code != 200 {
		t.Fatalf("delete = %d %v", code, out)
	}
	if code, out := ops.do("DELETE", "/fabrics/"+id, nil); code != 404 || codeOf(out) != "fabric_not_found" {
		t.Fatalf("delete again = %d %v", code, out)
	}
	if code, out := ops.do("DELETE", "/fabrics/abc", nil); code != 400 {
		t.Fatalf("delete bad id = %d %v", code, out)
	}
	for _, action := range []string{"fabric.create", "fabric.update", "fabric.delete"} {
		if n := e.adminAudits(action, id); n != 1 {
			t.Errorf("audit %s = %d", action, n)
		}
	}
}

func TestMachineFabricUpdate(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	direct := ops.createFabric("直连", "cx7", "direct")
	m1, m2, m3 := e.machine("SN-D1"), e.machine("SN-D2"), e.machine("SN-D3")
	for _, m := range []int64{m1, m2} {
		if code, out := ops.do("PUT", "/machines/"+itoa(m), map[string]any{"fabric_id": direct, "cluster_iface": "enp1s0f1np1"}); code != 200 ||
			out["fabric_id"].(float64) != float64(direct) || out["cluster_iface"] != "enp1s0f1np1" || out["fabric_name"] != "直连" {
			t.Fatalf("join direct = %d %v", code, out)
		}
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m3), map[string]any{"fabric_id": direct}); code != 409 || codeOf(out) != "fabric_full" {
		t.Fatalf("third into direct = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m3), map[string]any{"fabric_id": 999999}); code != 404 || codeOf(out) != "fabric_not_found" {
		t.Fatalf("missing fabric = %d %v", code, out)
	}
	for _, bad := range []map[string]any{{"fabric_id": "1"}, {"fabric_id": 1.5}, {"fabric_id": -1}, {"cluster_iface": 3}, {"cluster_iface": "bad name!"},
		{"cluster_iface": "-eth0"}, {"cluster_iface": ".eth0"}} {
		if code, out := ops.do("PUT", "/machines/"+itoa(m3), bad); code != 400 {
			t.Fatalf("bad body %v = %d %v", bad, code, out)
		}
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m3), map[string]any{}); code != 400 {
		t.Fatalf("empty body = %d %v", code, out)
	}
	// 集群网卡单独可改（"" = 自动）
	if code, out := ops.do("PUT", "/machines/"+itoa(m1), map[string]any{"cluster_iface": ""}); code != 200 || out["cluster_iface"] != "" || out["fabric_id"] == nil {
		t.Fatalf("iface auto = %d %v", code, out)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'machine.update' AND target_id = $1
		AND diff->'after'->>'fabric_id' = $2 AND diff->'after'->>'cluster_iface' = 'enp1s0f1np1'`, itoa(m1), itoa(direct)); n != 1 {
		t.Fatalf("machine.update audit with fabric diff = %d", n)
	}

	// 机器上有集群成员：不能改 fabric / 集群网卡（invalid_state）
	lan := ops.createFabric("以太网", "lan", "routed")
	a := e.clusterMachine(ops, "SN-L1", lan, 11)
	e.clusterMachine(ops, "SN-L2", lan, 12)
	e.userCluster(e.renter("13600000011"), 2)
	if code, out := ops.do("PUT", "/machines/"+itoa(a), map[string]any{"fabric_id": nil}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("leave with member = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(a), map[string]any{"cluster_iface": "eth1"}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("iface with member = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(a), map[string]any{"display_name": "集群一号"}); code != 200 {
		t.Fatalf("rename member machine = %d %v", code, out)
	}
}

func TestMachineDetailNetInfo(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	lan := ops.createFabric("以太网", "lan", "routed")
	m := e.clusterMachine(ops, "SN-N1", lan, 21)
	plain := e.machine("SN-N2")

	code, out := ops.do("GET", "/machines/"+itoa(m), nil)
	if code != 200 {
		t.Fatalf("detail = %d %v", code, out)
	}
	mm := out["machine"].(map[string]any)
	fab, _ := out["fabric"].(map[string]any)
	if mm["cluster_ready"] != true || mm["cluster_rdma"] != false || mm["cluster_net"] != "192.168.1.0/24" || mm["cluster_reason"] != "" ||
		mm["agent_features"].(float64) != 3 || mm["net_info_at"] == nil || mm["fabric_name"] != "以太网" {
		t.Fatalf("machine cluster fields = %v", mm)
	}
	ifaces := mm["net_info"].(map[string]any)["ifaces"].([]any)
	if len(ifaces) != 1 || ifaces[0].(map[string]any)["name"] != "eth0" {
		t.Fatalf("net_info = %v", mm["net_info"])
	}
	if fab == nil || fab["id"].(float64) != float64(lan) || fab["kind"] != "lan" || fab["isolation"] != "routed" {
		t.Fatalf("fabric = %v", out["fabric"])
	}
	// 不在 fabric：fabric 为 null，未就绪原因非空 / 网卡为空
	code, out = ops.do("GET", "/machines/"+itoa(plain), nil)
	if code != 200 || out["fabric"] != nil || out["machine"].(map[string]any)["cluster_ready"] != false || out["machine"].(map[string]any)["net_info"] != nil {
		t.Fatalf("plain detail = %d %v", code, out)
	}

	// 列表：fabric_id 筛选与行字段
	code, out = ops.do("GET", "/machines?fabric_id="+itoa(lan), nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["id"].(float64) != float64(m) ||
		items[0].(map[string]any)["fabric_name"] != "以太网" || items[0].(map[string]any)["cluster_ready"] != true {
		t.Fatalf("list by fabric = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/machines?fabric_id=x", nil); code != 400 {
		t.Fatalf("bad fabric_id = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/machines", nil); code != 200 || len(out["items"].([]any)) != 2 {
		t.Fatalf("list all = %d %v", code, out)
	}
}
