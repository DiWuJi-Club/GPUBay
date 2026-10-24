package llm_test

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

var nodeIDRe = regexp.MustCompile(`^node-[a-z0-9]{12}$`)

func TestControllerProvisionsToReplicas(t *testing.T) {
	ne := newNodeEnv(t)
	for _, s := range []string{"P1", "P2", "P3"} {
		ne.poolMachine(s)
	}
	m := ne.onlineModel("qwen3-4b", 2)
	rep := ne.tick()
	if rep.Provisioned != 2 || ne.rental.placeCount() != 2 {
		t.Fatalf("report %+v, PlaceSystem calls %d; want 2 / 2", rep, ne.rental.placeCount())
	}
	nodes := ne.nodesOf(m.ID)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d", len(nodes))
	}
	for _, n := range nodes {
		if n.State != llm.NodeProvisioning || n.InstanceID == nil || !nodeIDRe.MatchString(n.ID) {
			t.Fatalf("node %+v", n.Node)
		}
		if n.ModelSlug != "qwen3-4b" || n.MachineName == "" {
			t.Fatalf("row view %+v", n)
		}
		key, err := ne.svc.NodeKey(ne.ctx, n.ID)
		if err != nil || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(key) {
			t.Fatalf("NodeKey = %q, %v", key, err)
		}
		if got := ne.eventStates(n.ID); !slices.Equal(got, []string{">provisioning"}) {
			t.Fatalf("events %v", got)
		}
	}
	for _, req := range ne.rental.placed {
		if req.GroupID != ne.poolGroupID || req.ImageID != m.ImageID {
			t.Fatalf("PlaceSystem %+v", req)
		}
	}
	// 已满副本：不再建
	if rep := ne.tick(); rep.Provisioned != 0 || ne.rental.placeCount() != 2 {
		t.Fatalf("second tick %+v calls %d", rep, ne.rental.placeCount())
	}
	if ne.alertCount(llm.AlertPoolShort) != 0 {
		t.Fatal("unexpected pool-short alert")
	}

	// 每轮上限 MaxProvisionPerRound：再要 6 个，机器足够 → 第一轮 4、第二轮 2
	for _, s := range []string{"P4", "P5", "P6", "P7", "P8"} {
		ne.poolMachine(s)
	}
	m2 := ne.onlineModel("qwen3-8b", 5)
	ne.exec(`UPDATE llm_models SET replicas = 3 WHERE id = $1`, m.ID)
	if rep := ne.tick(); rep.Provisioned != llm.MaxProvisionPerRound {
		t.Fatalf("capped round %+v", rep)
	}
	if rep := ne.tick(); rep.Provisioned != 2 {
		t.Fatalf("second capped round %+v", rep)
	}
	if len(ne.nodesOf(m.ID)) != 3 || len(ne.nodesOf(m2.ID)) != 5 {
		t.Fatalf("nodes %d / %d", len(ne.nodesOf(m.ID)), len(ne.nodesOf(m2.ID)))
	}
	if ne.alertCount(llm.AlertPoolShort) != 0 {
		t.Fatal("the per-round cap is not a pool shortage")
	}
}

func TestControllerPoolShortAlertOnce(t *testing.T) {
	ne := newNodeEnv(t)
	free := ne.poolMachine("FREE")
	offline := ne.poolMachine("OFF")
	ne.exec(`UPDATE machines SET agent_online = false WHERE id = $1`, offline)
	leased := ne.poolMachine("LEASED") // 加入池时还有用户的预订租约：等待租约结束
	now := ne.clk.Now()
	ne.rentalLease(leased, "reserved", "reserved", now.Add(24*time.Hour), now.Add(48*time.Hour))
	ne.poolMachine("BUSY")
	busy := ne.queryInt(`SELECT id FROM machines WHERE serial = 'BUSY'`)
	ne.exec(`UPDATE machines SET state = 'maintenance' WHERE id = $1`, busy)
	other := ne.queryInt(`INSERT INTO machines (serial, lan_ip, product_id, state, agent_online, created_at, updated_at)
		SELECT 'NOTPOOL', '10.0.0.11', id, 'idle', true, $1, $1 FROM products WHERE code = 'spark-1' RETURNING id`, now)
	_ = other

	m := ne.onlineModel("qwen3-4b", 3)
	rep := ne.tick()
	if rep.Provisioned != 1 {
		t.Fatalf("report %+v", rep)
	}
	nodes := ne.nodesOf(m.ID)
	if len(nodes) != 1 || nodes[0].MachineID != free {
		t.Fatalf("nodes %+v", nodes)
	}
	if ne.alertCount(llm.AlertPoolShort) != 1 {
		t.Fatalf("pool short alerts = %d", ne.alertCount(llm.AlertPoolShort))
	}
	var msg, level string
	if err := ne.pool.QueryRow(ne.ctx, `SELECT message, level FROM alerts WHERE kind = $1`, llm.AlertPoolShort).Scan(&msg, &level); err != nil {
		t.Fatal(err)
	}
	if msg != "Token 服务池没有空闲机器，模型节点缺额" || level != "warning" {
		t.Fatalf("alert %q %q", level, msg)
	}
	ne.tick()
	ne.tick()
	if ne.alertCount(llm.AlertPoolShort) != 1 || ne.rental.placeCount() != 1 {
		t.Fatalf("alerts %d place calls %d after more ticks", ne.alertCount(llm.AlertPoolShort), ne.rental.placeCount())
	}

	// PlaceSystem 复核失败（并发被占）→ 跳过该机器，照样告警
	ne2 := newNodeEnv(t)
	ne2.poolMachine("X1")
	ne2.rental.placeErr = apperr.ErrMachineUnavailable
	ne2.onlineModel("qwen3-4b", 1)
	if rep := ne2.tick(); rep.Provisioned != 0 || ne2.alertCount(llm.AlertPoolShort) != 1 {
		t.Fatalf("unavailable machine: %+v alerts %d", rep, ne2.alertCount(llm.AlertPoolShort))
	}
}

func TestControllerAdvancesProvisioning(t *testing.T) {
	ne := newNodeEnv(t)
	ne.poolMachine("A1")
	m := ne.onlineModel("qwen3-4b", 1, func(in *llm.ModelInput) { in.NodeEnv = map[string]string{"MAX_NUM_SEQS": "32"} })
	ne.tick()
	n := ne.nodesOf(m.ID)[0]
	// 实例 creating：不建部署
	ne.tick()
	if len(ne.apps.createCalls()) != 0 {
		t.Fatal("Create called while instance creating")
	}
	ne.setInstance(*n.InstanceID, rental.StateRunning)

	// vllm-node 未标记 internal：告警、不建部署
	ne.exec(`UPDATE apps SET internal = false WHERE id = $1`, ne.appID)
	ne.tick()
	if len(ne.apps.createCalls()) != 0 || ne.alertCount(llm.AlertNodeFailed) != 1 {
		t.Fatalf("non-internal app: creates %d alerts %d", len(ne.apps.createCalls()), ne.alertCount(llm.AlertNodeFailed))
	}
	ne.exec(`UPDATE apps SET internal = true WHERE id = $1`, ne.appID)

	ne.tick()
	calls := ne.apps.createCalls()
	if len(calls) != 1 {
		t.Fatalf("Create calls = %d", len(calls))
	}
	c := calls[0]
	key, _ := ne.svc.NodeKey(ne.ctx, n.ID)
	if c.UserID != ne.systemUserID || c.InstanceID != *n.InstanceID || c.AppID != ne.appID || c.VersionID != nil || c.IP != "" {
		t.Fatalf("CreateRequest %+v", c)
	}
	want := map[string]string{"MODEL_ID": "Qwen/Qwen3-4B", "MODEL_SOURCE": "modelscope", "SERVED_MODEL_NAME": "qwen3-4b",
		"MAX_MODEL_LEN": "32768", "API_KEY": key, "TASK": "generate", "MAX_NUM_SEQS": "32"}
	if len(c.Env) != len(want) {
		t.Fatalf("env %v", c.Env)
	}
	for k, v := range want {
		if c.Env[k] != v {
			t.Fatalf("env[%s] = %q, want %q", k, c.Env[k], v)
		}
	}
	n2 := ne.node(n.ID)
	if n2.DeploymentID == nil || n2.State != llm.NodeProvisioning {
		t.Fatalf("after create %+v", n2)
	}
	dep := *n2.DeploymentID

	// 部署 queued / running 但 health unknown / 探测失败：仍 provisioning
	ne.tick()
	ne.setDeployment(dep, appdeploy.StateRunning, appdeploy.HealthUnknown, "")
	ne.tick()
	if ne.probe.count() != 0 {
		t.Fatal("probed before health ok")
	}
	ne.setDeployment(dep, appdeploy.StateRunning, appdeploy.HealthOK, "")
	ne.probe.set(errProbe)
	ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeProvisioning || got.LastProbeAt == nil {
		t.Fatalf("probe failed: %+v", got)
	}
	ne.probe.set(nil)
	ne.clk.Advance(time.Minute)
	ne.tick()
	got := ne.node(n.ID)
	if got.State != llm.NodeReady || got.LastReadyAt == nil || !got.LastReadyAt.Equal(ne.clk.Now()) || got.ProbeFailCount != 0 {
		t.Fatalf("ready: %+v", got)
	}
	if ne.probe.keys[len(ne.probe.keys)-1] != key {
		t.Fatal("probe did not use the node key")
	}
	if len(ne.apps.createCalls()) != 1 {
		t.Fatal("Create called again")
	}
	if ev := ne.eventStates(n.ID); !slices.Equal(ev, []string{">provisioning", "provisioning>ready"}) {
		t.Fatalf("events %v", ev)
	}
}

func TestControllerProvisionTimeoutFails(t *testing.T) {
	ne := newNodeEnv(t)
	for _, s := range []string{"T1", "T2", "T3"} {
		ne.poolMachine(s)
	}
	m := ne.onlineModel("qwen3-4b", 3)
	ne.tick()
	nodes := ne.nodesOf(m.ID)
	timeout, depFail, instFail := nodes[0], nodes[1], nodes[2]
	for _, n := range nodes[:2] {
		ne.setInstance(*n.InstanceID, rental.StateRunning)
	}
	ne.tick() // 两个建部署
	depID := *ne.node(depFail.ID).DeploymentID
	ne.setDeployment(depID, appdeploy.StateFailed, appdeploy.HealthUnknown, appdeploy.ErrCodeDisk)
	ne.exec(`UPDATE app_deployments SET message = '数据盘剩余 10 GiB' WHERE id = $1`, depID)
	ne.setInstance(*instFail.InstanceID, rental.StateFailed)
	rep := ne.tick()
	if rep.Failed != 2 {
		t.Fatalf("report %+v", rep)
	}
	if got := ne.node(depFail.ID); got.State != llm.NodeFailed || got.Error != "数据盘空间不足：数据盘剩余 10 GiB" {
		t.Fatalf("deployment failed node %+v", got)
	}
	if got := ne.node(instFail.ID); got.State != llm.NodeFailed || !strings.Contains(got.Error, "实例") {
		t.Fatalf("instance failed node %+v", got)
	}
	if !slices.Contains(ne.rental.releasedIDs(), *depFail.InstanceID) {
		t.Fatal("failed node's live instance not released")
	}

	// 剩下的一个：超过 150 分钟仍未就绪 → failed(timeout) + 释放
	ne.clk.Advance(llm.ProvisionTimeout - time.Minute)
	ne.tick()
	if ne.node(timeout.ID).State != llm.NodeProvisioning {
		t.Fatal("failed before timeout")
	}
	ne.clk.Advance(2 * time.Minute)
	ne.tick()
	got := ne.node(timeout.ID)
	if got.State != llm.NodeFailed || !strings.Contains(got.Error, "超时") {
		t.Fatalf("timeout node %+v", got)
	}
	if !slices.Contains(ne.rental.releasedIDs(), *timeout.InstanceID) {
		t.Fatal("timed-out node's instance not released")
	}
	if ev := ne.eventStates(timeout.ID); ev[len(ev)-1] != "provisioning>failed" {
		t.Fatalf("events %v", ev)
	}
}

func TestControllerScaleDownDrainsWorstFirst(t *testing.T) {
	ne := newNodeEnv(t)
	for _, s := range []string{"S1", "S2", "S3", "S4"} {
		ne.poolMachine(s)
	}
	m := ne.onlineModel("qwen3-4b", 4)
	ne.tick()
	nodes := ne.nodesOf(m.ID)
	unhealthy, prov, oldReady, newReady := nodes[0].ID, nodes[1].ID, nodes[2].ID, nodes[3].ID
	t0 := ne.clk.Now()
	ne.exec(`UPDATE llm_nodes SET state = 'unhealthy', unhealthy_since = $2 WHERE id = $1`, unhealthy, t0)
	ne.exec(`UPDATE llm_nodes SET state = 'ready', last_ready_at = $2 WHERE id = $1`, oldReady, t0.Add(-time.Hour))
	ne.exec(`UPDATE llm_nodes SET state = 'ready', last_ready_at = $2 WHERE id = $1`, newReady, t0.Add(-time.Minute))
	// 在途占住，排空的节点留在 draining 便于断言
	for _, n := range nodes {
		defer ne.svc.Router().Acquire(n.ID)()
	}
	ne.exec(`UPDATE llm_models SET replicas = 2 WHERE id = $1`, m.ID)
	if rep := ne.tick(); rep.Drained != 2 {
		t.Fatalf("report %+v", rep)
	}
	if ne.node(unhealthy).State != llm.NodeDraining || ne.node(prov).State != llm.NodeDraining {
		t.Fatal("unhealthy and provisioning should drain first")
	}
	ne.exec(`UPDATE llm_models SET replicas = 1 WHERE id = $1`, m.ID)
	ne.tick()
	if ne.node(oldReady).State != llm.NodeDraining || ne.node(newReady).State != llm.NodeReady {
		t.Fatalf("oldest-ready should drain: old %s new %s", ne.node(oldReady).State, ne.node(newReady).State)
	}
	if d := ne.node(oldReady).DrainSince; d == nil || !d.Equal(ne.clk.Now()) {
		t.Fatalf("drain_since %v", d)
	}
	if ne.rental.placeCount() != 4 {
		t.Fatal("scale-down must not provision")
	}
}

func TestControllerDrainWaitsInflightThenReleases(t *testing.T) {
	ne := newNodeEnv(t)
	ne.poolMachine("D1")
	ne.poolMachine("D2")
	m := ne.onlineModel("qwen3-4b", 2)
	ne.tick()
	nodes := ne.bringReady(m.ID)
	a, b := nodes[0], nodes[1]
	relA := ne.svc.Router().Acquire(a.ID)
	relB := ne.svc.Router().Acquire(b.ID)
	defer relB()
	ne.exec(`UPDATE llm_models SET replicas = 0 WHERE id = $1`, m.ID)
	ne.tick()
	if ne.node(a.ID).State != llm.NodeDraining || ne.node(b.ID).State != llm.NodeDraining {
		t.Fatal("both should drain")
	}
	ne.clk.Advance(30 * time.Second)
	ne.tick()
	if len(ne.rental.releasedIDs()) != 0 || ne.node(a.ID).State != llm.NodeDraining {
		t.Fatal("released with inflight before timeout")
	}
	relA() // a 在途归零
	ne.tick()
	if got := ne.node(a.ID); got.State != llm.NodeRemoving || !slices.Equal(ne.rental.releasedIDs(), []string{*a.InstanceID}) {
		t.Fatalf("a: %s released %v", got.State, ne.rental.releasedIDs())
	}
	if ne.node(b.ID).State != llm.NodeDraining {
		t.Fatal("b still has inflight")
	}
	ne.clk.Advance(31 * time.Second) // drain_since + 61s：超时，不等在途
	ne.tick()
	if ne.node(b.ID).State != llm.NodeRemoving || len(ne.rental.releasedIDs()) != 2 {
		t.Fatalf("b after drain timeout: %s", ne.node(b.ID).State)
	}
	// 洗机中：仍 removing；洗完 released → removed
	ne.tick()
	if ne.node(a.ID).State != llm.NodeRemoving {
		t.Fatal("removed while wiping")
	}
	ne.setInstance(*a.InstanceID, rental.StateReleased)
	if rep := ne.tick(); rep.Removed != 1 {
		t.Fatalf("report %+v", rep)
	}
	if ne.node(a.ID).State != llm.NodeRemoved {
		t.Fatal("a not removed")
	}
	want := []string{">provisioning", "provisioning>ready", "ready>draining", "draining>removing", "removing>removed"}
	if ev := ne.eventStates(a.ID); !slices.Equal(ev, want) {
		t.Fatalf("events %v", ev)
	}
	ne.setInstance(*b.InstanceID, rental.StateFailed)
	ne.tick()
	if ne.node(b.ID).State != llm.NodeRemoved {
		t.Fatal("b not removed after instance failed")
	}
}

func TestControllerOfflineModelScalesToZero(t *testing.T) {
	ne := newNodeEnv(t)
	ne.poolMachine("O1")
	ne.poolMachine("O2")
	ne.poolMachine("O3")
	m := ne.onlineModel("qwen3-4b", 2)
	ne.tick()
	nodes := ne.bringReady(m.ID)
	if _, err := ne.svc.SetModelStatus(ne.ctx, ne.admin(), m.ID, llm.ModelOffline); err != nil {
		t.Fatal(err)
	}
	rep := ne.tick()
	if rep.Drained != 2 || rep.Provisioned != 0 {
		t.Fatalf("report %+v", rep)
	}
	for _, n := range nodes {
		if got := ne.node(n.ID); got.State != llm.NodeRemoving { // 在途 0：同一轮排空即释放
			t.Fatalf("node %s state %s", n.ID, got.State)
		}
		ne.setInstance(*n.InstanceID, rental.StateReleased)
	}
	if len(ne.rental.releasedIDs()) != 2 {
		t.Fatalf("released %v", ne.rental.releasedIDs())
	}
	ne.tick()
	if ne.countState(m.ID, llm.NodeRemoved) != 0 || len(ne.nodesOf(m.ID)) != 0 { // removed 不在默认列表里
		t.Fatalf("nodes left %+v", ne.nodesOf(m.ID))
	}
	rows, _ := ne.svc.ListNodes(ne.ctx, llm.NodeFilter{ModelID: m.ID, State: llm.NodeRemoved})
	if len(rows) != 2 {
		t.Fatalf("removed rows %d", len(rows))
	}
	ne.tick()
	if ne.rental.placeCount() != 2 {
		t.Fatal("offline model provisioned")
	}
}

func TestControllerUnhealthyRebuildThenFailed(t *testing.T) {
	ne := newNodeEnv(t)
	ne.poolMachine("U1")
	m := ne.onlineModel("qwen3-4b", 1)
	ne.tick()
	n := ne.bringReady(m.ID)[0]
	dep := *n.DeploymentID

	makeUnhealthy := func() {
		t.Helper()
		ne.probe.set(errProbe)
		for i := 0; i < llm.ProbeFailThreshold; i++ {
			ne.probeTick()
		}
		if got := ne.node(n.ID); got.State != llm.NodeUnhealthy {
			t.Fatalf("expected unhealthy, got %s", got.State)
		}
	}
	for round := 1; round <= llm.MaxRebuilds; round++ {
		makeUnhealthy()
		ne.clk.Advance(llm.UnhealthyRebuildAfter - time.Second)
		ne.tick()
		if len(ne.apps.redeployCalls()) != round-1 {
			t.Fatal("redeployed before 10 minutes")
		}
		ne.clk.Advance(time.Second)
		ne.tick()
		calls := ne.apps.redeployCalls()
		if len(calls) != round {
			t.Fatalf("round %d: redeploy calls %d", round, len(calls))
		}
		r := calls[round-1]
		if r.UserID != ne.systemUserID || r.InstanceID != *n.InstanceID || r.DeploymentID != dep || r.EnvSet {
			t.Fatalf("RedeployRequest %+v", r)
		}
		got := ne.node(n.ID)
		if got.State != llm.NodeProvisioning || got.RebuildCount != round || got.UnhealthySince != nil {
			t.Fatalf("round %d after redeploy %+v", round, got)
		}
		// 重建后 provisioning 重新计时：不会因为节点早就建好而立即超时
		ne.setDeployment(dep, appdeploy.StateRunning, appdeploy.HealthOK, "")
		ne.probe.set(nil)
		ne.tick()
		if ne.node(n.ID).State != llm.NodeReady {
			t.Fatalf("round %d: not ready after rebuild", round)
		}
	}
	makeUnhealthy()
	ne.clk.Advance(llm.UnhealthyRebuildAfter)
	rep := ne.tick()
	if rep.Failed != 1 || len(ne.apps.redeployCalls()) != llm.MaxRebuilds {
		t.Fatalf("report %+v redeploys %d", rep, len(ne.apps.redeployCalls()))
	}
	if got := ne.node(n.ID); got.State != llm.NodeFailed {
		t.Fatalf("state %s", got.State)
	}
	var level, msg, inst string
	if err := ne.pool.QueryRow(ne.ctx, `SELECT level, message, instance_id FROM alerts WHERE kind = $1`, llm.AlertNodeFailed).Scan(&level, &msg, &inst); err != nil {
		t.Fatal(err)
	}
	if level != "critical" || msg != "节点 "+n.ID+"（qwen3-4b）多次重建失败" || inst != *n.InstanceID {
		t.Fatalf("alert %s %q %s", level, msg, inst)
	}
	if !slices.Contains(ne.rental.releasedIDs(), *n.InstanceID) {
		t.Fatal("failed node instance not released")
	}
	// 缺额：下一轮在别的机器上补（这里没有别的机器 → 告警缺额）
	ne.tick()
	if ne.alertCount(llm.AlertPoolShort) != 1 {
		t.Fatal("expected pool short after failure")
	}
}

func TestControllerUnhealthyEdgeCases(t *testing.T) {
	ne := newNodeEnv(t)
	ne.poolMachine("E1")
	ne.poolMachine("E2")
	m := ne.onlineModel("qwen3-4b", 2)
	ne.tick()
	nodes := ne.bringReady(m.ID)
	a, b := nodes[0], nodes[1]
	ne.exec(`UPDATE llm_nodes SET state = 'unhealthy', unhealthy_since = $1`, ne.clk.Now())
	// a：部署已被置回 queued（机器恢复后重装）→ 不 Redeploy、不计次数，转 provisioning 等
	ne.setDeployment(*a.DeploymentID, appdeploy.StateQueued, appdeploy.HealthUnknown, "")
	// b：实例已失效 → 直接 failed
	ne.setInstance(*b.InstanceID, rental.StateFailed)
	ne.clk.Advance(llm.UnhealthyRebuildAfter)
	ne.tick()
	if got := ne.node(a.ID); got.State != llm.NodeProvisioning || got.RebuildCount != 0 || len(ne.apps.redeployCalls()) != 0 {
		t.Fatalf("a %+v redeploys %d", got, len(ne.apps.redeployCalls()))
	}
	if got := ne.node(b.ID); got.State != llm.NodeFailed {
		t.Fatalf("b %s", got.State)
	}
}

func TestControllerReleasesOrphanSystemInstances(t *testing.T) {
	ne := newNodeEnv(t)
	mach := ne.poolMachine("R1")
	inst, err := ne.rental.PlaceSystem(ne.ctx, rental.SystemPlaceRequest{MachineID: mach, GroupID: ne.poolGroupID, ImageID: ne.imageID})
	if err != nil {
		t.Fatal(err)
	}
	ne.tick()
	if len(ne.rental.releasedIDs()) != 0 {
		t.Fatal("orphan released inside grace period")
	}
	ne.clk.Advance(3 * time.Minute)
	if rep := ne.tick(); rep.Orphans != 1 || !slices.Equal(ne.rental.releasedIDs(), []string{inst.ID}) {
		t.Fatalf("report %+v released %v", rep, ne.rental.releasedIDs())
	}
}

func TestControllerRunOnceTakesAdvisoryLock(t *testing.T) {
	ne := newNodeEnv(t)
	ne.poolMachine("L1")
	ne.onlineModel("qwen3-4b", 1)
	conn, err := ne.pool.Acquire(ne.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ne.ctx, `SELECT pg_advisory_lock(hashtext('llm-controller'))`); err != nil {
		t.Fatal(err)
	}
	if ran, err := ne.ctl.RunOnce(ne.ctx); ran || err != nil || ne.rental.placeCount() != 0 {
		t.Fatalf("ran %v err %v while another holder has the lock", ran, err)
	}
	if _, err := conn.Exec(ne.ctx, `SELECT pg_advisory_unlock(hashtext('llm-controller'))`); err != nil {
		t.Fatal(err)
	}
	if ran, err := ne.ctl.RunOnce(context.Background()); !ran || err != nil || ne.rental.placeCount() != 1 {
		t.Fatalf("ran %v err %v place %d", ran, err, ne.rental.placeCount())
	}
}
