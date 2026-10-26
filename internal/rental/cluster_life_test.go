package rental_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/rental"
)

// ---- 集群生命周期测试辅助 ----

// openCmd 返回实例在机器上未完成的某类指令（没有 → 测试失败）。
func (e *env) openCmd(machineID int64, instanceID string, t fleet.CommandType) fleet.Command {
	e.t.Helper()
	cmds, err := e.fleet.PendingCommands(e.ctx, machineID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cmds {
		if c.Type == t && c.InstanceID == instanceID {
			return c
		}
	}
	e.t.Fatalf("no open %s for %s on machine %d", t, instanceID, machineID)
	return fleet.Command{}
}

// finish 回报实例某类未完成指令的结果。
func (e *env) finish(inst rental.Instance, t fleet.CommandType, ok bool, msg string) {
	e.t.Helper()
	c := e.openCmd(inst.MachineID, inst.ID, t)
	if err := e.svc.HandleCommandResult(e.ctx, c.ID, ok, msg); err != nil {
		e.t.Fatalf("HandleCommandResult(%s %s): %v", t, inst.ID, err)
	}
}

func (e *env) runMembers(insts ...rental.Instance) {
	e.t.Helper()
	for _, inst := range insts {
		e.finish(inst, fleet.CmdCreateInstance, true, "ok")
	}
}

func (e *env) sweepClusters() int {
	e.t.Helper()
	n, err := e.svc.SweepClusters(e.ctx)
	if err != nil {
		e.t.Fatalf("SweepClusters: %v", err)
	}
	return n
}

// readyCluster 下单 n 台集群、全员开通、清扫一轮使其就绪；返回按 rank 排序的成员。
func (e *env) readyCluster(n int) (rental.BatchResult, []rental.Instance) {
	e.t.Helper()
	res := e.placeCluster(n)
	ranked := byRank(e.t, res.Instances)
	e.runMembers(ranked...)
	if e.sweepClusters() < 1 {
		e.t.Fatal("cluster not swept to ready")
	}
	if v := e.clusterView(res.Cluster.ID); v.ReadyAt == nil {
		e.t.Fatalf("cluster not ready: %+v", v.Cluster)
	}
	for i := range ranked {
		ranked[i] = e.get(ranked[i].ID)
	}
	return res, ranked
}

func (e *env) checkInvariants() {
	e.t.Helper()
	if err := rental.CheckClusterInvariants(e.ctx, e.pool); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) unitPrice(d catalog.Duration) int64 {
	e.t.Helper()
	p, err := catalog.NewService(e.pool).Quote(e.ctx, e.pool, e.productID, d)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) configurePayload(inst rental.Instance) fleet.ConfigureClusterPayload {
	e.t.Helper()
	c := e.openCmd(inst.MachineID, inst.ID, fleet.CmdConfigureCluster)
	var p fleet.ConfigureClusterPayload
	if err := json.Unmarshal(c.Payload, &p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func orderFullyRefunded(e *env, instanceID string) bool {
	return e.count(`SELECT count(*) FROM orders WHERE instance_id = $1 AND (refunded_amount <> amount OR status <> 'refunded')`, instanceID) == 0 &&
		e.count(`SELECT count(*) FROM orders WHERE instance_id = $1`, instanceID) > 0
}

// ---- SweepClusters ----

func TestClusterAbortRefundsAll(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res := e.placeCluster(2)
	r := byRank(t, res.Instances)
	e.runMembers(r[0])
	e.finish(r[1], fleet.CmdCreateInstance, false, "docker: image pull failed")
	if e.get(r[1].ID).State != rental.StateFailed || e.machineState(r[1].MachineID) != fleet.StateFaulty {
		t.Fatal("rank1 should fail and its machine be faulty")
	}
	if n := e.sweepClusters(); n != 1 {
		t.Fatalf("swept %d", n)
	}
	if e.get(r[0].ID).State != rental.StateWiping || countPending(e, r[0].MachineID, fleet.CmdWipeInstance) != 1 {
		t.Fatal("rank0 should be wiping")
	}
	for _, inst := range r {
		if !orderFullyRefunded(e, inst.ID) {
			t.Fatalf("orders of %s not fully refunded", inst.ID)
		}
	}
	if e.balance(e.userID) != startFunds {
		t.Fatalf("balance %d, want %d", e.balance(e.userID), startFunds)
	}
	if e.machineState(r[0].MachineID) == fleet.StateFaulty {
		t.Fatal("rank0 machine must not be faulty")
	}
	v := e.clusterView(res.Cluster.ID)
	if v.State != rental.ClusterFailed || v.EndedAt == nil || v.Status != rental.ClusterFailed {
		t.Fatalf("cluster = %+v", v.Cluster)
	}
	if e.alerts("cluster_failed") != 1 || e.count(`SELECT count(*) FROM alerts WHERE kind = 'cluster_failed' AND message = $1`, rental.AlertClusterFailed(v.ID)) != 1 {
		t.Fatal("want one cluster_failed alert")
	}
	if e.count(`SELECT count(*) FROM instance_events WHERE instance_id = $1 AND reason = 'cluster aborted: member failed'`, r[0].ID) != 1 {
		t.Fatal("abort event missing")
	}
	// 再跑一轮无副作用
	if n := e.sweepClusters(); n != 0 || e.alerts("cluster_failed") != 1 || e.balance(e.userID) != startFunds {
		t.Fatalf("second sweep n=%d", n)
	}
	e.checkInvariants()
}

func TestClusterReadyAndAlign(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res := e.placeCluster(2)
	r := byRank(t, res.Instances)
	e.runMembers(r[0])
	if n := e.sweepClusters(); n != 0 {
		t.Fatalf("half running swept %d", n)
	}
	e.clock.Advance(3 * time.Minute)
	e.runMembers(r[1])
	late := *e.get(r[1].ID).ExpiresAt
	if !e.get(r[0].ID).ExpiresAt.Before(late) {
		t.Fatal("precondition: rank0 expires earlier")
	}
	if n := e.sweepClusters(); n != 1 {
		t.Fatalf("ready sweep %d", n)
	}
	v := e.clusterView(res.Cluster.ID)
	if v.ReadyAt == nil || !v.ReadyAt.Equal(e.clock.Now()) {
		t.Fatalf("ready_at = %v", v.ReadyAt)
	}
	for _, inst := range r {
		if got := e.get(inst.ID); !got.ExpiresAt.Equal(late) {
			t.Fatalf("%s expires %v, want %v", inst.ID, got.ExpiresAt, late)
		}
		_, upper, _ := e.lease(inst.ID)
		if !upper.Equal(late.Add(rental.Tail)) {
			t.Fatalf("lease upper %v", upper)
		}
	}
	if e.count(`SELECT count(*) FROM instance_events WHERE instance_id = $1 AND reason LIKE 'cluster aligned to %'`, r[0].ID) != 1 ||
		e.count(`SELECT count(*) FROM instance_events WHERE instance_id = $1 AND reason LIKE 'cluster aligned to %'`, r[1].ID) != 0 {
		t.Fatal("align events")
	}
	if n := e.sweepClusters(); n != 0 {
		t.Fatalf("idle sweep %d", n)
	}
	e.checkInvariants()
}

func TestClusterNoRealignAfterSuspend(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	_, r := e.readyCluster(2)
	exp0, exp1 := *r[0].ExpiresAt, *r[1].ExpiresAt
	if _, err := e.svc.SuspendMachines(e.ctx, []int64{r[1].MachineID}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Hour)
	if _, err := e.svc.ResumeMachine(e.ctx, r[1].MachineID, []string{r[1].ID}); err != nil {
		t.Fatal(err)
	}
	if got := *e.get(r[1].ID).ExpiresAt; !got.Equal(exp1.Add(2 * time.Hour)) {
		t.Fatalf("rank1 expires %v", got)
	}
	e.sweepClusters()
	if got := *e.get(r[0].ID).ExpiresAt; !got.Equal(exp0) {
		t.Fatalf("rank0 realigned to %v", got)
	}
	if got := *e.get(r[1].ID).ExpiresAt; !got.Equal(exp1.Add(2 * time.Hour)) {
		t.Fatalf("rank1 changed to %v", got)
	}
}

func TestClusterFormTimeoutAborts(t *testing.T) {
	t.Run("suspended member", func(t *testing.T) {
		e := newEnv(t, 0)
		f := e.fabric("A", "lan", "routed", false)
		e.lanMachines(f, "192.168.1", 2)
		res := e.placeCluster(2)
		r := byRank(t, res.Instances)
		e.runMembers(r...)
		if _, err := e.svc.SuspendMachines(e.ctx, []int64{r[1].MachineID}); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(rental.ClusterFormTimeout - time.Second)
		if n := e.sweepClusters(); n != 0 || e.clusterView(res.Cluster.ID).State != rental.ClusterActive {
			t.Fatal("aborted before timeout")
		}
		e.clock.Advance(time.Second)
		if n := e.sweepClusters(); n != 1 {
			t.Fatalf("swept %d", n)
		}
		if v := e.clusterView(res.Cluster.ID); v.State != rental.ClusterFailed {
			t.Fatalf("state %s", v.State)
		}
		for _, inst := range r {
			if e.get(inst.ID).State != rental.StateWiping || !orderFullyRefunded(e, inst.ID) || e.machineState(inst.MachineID) == fleet.StateFaulty {
				t.Fatalf("member %s", inst.ID)
			}
		}
		if e.balance(e.userID) != startFunds || e.alerts("cluster_failed") != 1 {
			t.Fatal("refund / alert")
		}
		e.checkInvariants()
	})
	t.Run("member refund-released", func(t *testing.T) {
		e := newEnv(t, 0)
		f := e.fabric("A", "lan", "routed", false)
		e.lanMachines(f, "192.168.1", 2)
		res := e.placeCluster(2)
		r := byRank(t, res.Instances)
		e.runMembers(r[0])
		if _, err := e.svc.RefundRelease(e.ctx, audit.Actor{AdminID: 1}, r[1].ID, 0, "bad machine"); err != nil {
			t.Fatal(err)
		}
		e.finish(r[1], fleet.CmdWipeInstance, true, "ok")
		e.clock.Advance(rental.ClusterFormTimeout)
		if n := e.sweepClusters(); n != 1 || e.clusterView(res.Cluster.ID).State != rental.ClusterFailed {
			t.Fatal("not aborted")
		}
		if !orderFullyRefunded(e, r[0].ID) || !orderFullyRefunded(e, r[1].ID) || e.balance(e.userID) != startFunds {
			t.Fatal("not fully refunded")
		}
		e.checkInvariants()
	})
}

func TestClusterNetCreateFailureNotFaulty(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res := e.placeCluster(2)
	r := byRank(t, res.Instances)
	e.runMembers(r[0])
	e.finish(r[1], fleet.CmdCreateInstance, false, "cluster-net: veth add failed")
	if e.get(r[1].ID).State != rental.StateFailed || e.machineState(r[1].MachineID) == fleet.StateFaulty {
		t.Fatal("cluster-net failure must fail the instance without faulty machine")
	}
	if e.alerts("cluster_net_error") != 1 || e.alerts("create_failed") != 0 {
		t.Fatal("want cluster_net_error alert only")
	}
	if !orderFullyRefunded(e, r[1].ID) {
		t.Fatal("failed member refunded")
	}
	if e.sweepClusters() != 1 || e.clusterView(res.Cluster.ID).State != rental.ClusterFailed {
		t.Fatal("cluster aborted")
	}
	// 失败成员的机器：洗机完成后回到库存
	e.finish(r[1], fleet.CmdWipeInstance, true, "ok")
	if e.machineState(r[1].MachineID) != fleet.StateIdle {
		t.Fatalf("machine %s", e.machineState(r[1].MachineID))
	}
	e.finish(r[0], fleet.CmdWipeInstance, true, "ok")
	if e.machineState(r[0].MachineID) != fleet.StateIdle || e.balance(e.userID) != startFunds {
		t.Fatal("rank0 machine / balance")
	}
	e.checkInvariants()
}

func TestSystemClusterReadyAndEnd(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	pm := e.lanMachines(f, "192.168.1", 4)
	pool := e.toPool(pm...)
	sys, err := e.svc.PlaceSystemCluster(e.ctx, rental.SystemClusterRequest{MachineIDs: pm[:2], GroupID: pool, ImageID: e.imageID, Note: "n"})
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(rental.ClusterFormTimeout + time.Minute) // 系统集群组建期不中止
	if n := e.sweepClusters(); n != 0 {
		t.Fatalf("system forming swept %d", n)
	}
	e.runMembers(sys.Instances...)
	if n := e.sweepClusters(); n != 1 {
		t.Fatalf("swept %d", n)
	}
	v := e.clusterView(sys.Cluster.ID)
	if v.ReadyAt == nil || v.State != rental.ClusterActive {
		t.Fatalf("system cluster = %+v", v.Cluster)
	}
	for _, inst := range sys.Instances {
		if e.get(inst.ID).ExpiresAt != nil {
			t.Fatal("system members never expire")
		}
	}
	if err := e.svc.ReleaseSystemCluster(e.ctx, sys.Cluster.ID); err != nil {
		t.Fatal(err)
	}
	e.finish(sys.Instances[0], fleet.CmdWipeInstance, true, "ok")
	if e.sweepClusters() != 0 {
		t.Fatal("still a wiping member")
	}
	e.finish(sys.Instances[1], fleet.CmdWipeInstance, true, "ok")
	if e.sweepClusters() != 1 {
		t.Fatal("end not swept")
	}
	if v := e.clusterView(sys.Cluster.ID); v.State != rental.ClusterReleased || v.EndedAt == nil {
		t.Fatalf("state %s", v.State)
	}
	// 从未就绪的系统集群结束 → failed（不告警、无订单）
	sys2, err := e.svc.PlaceSystemCluster(e.ctx, rental.SystemClusterRequest{MachineIDs: pm[2:], GroupID: pool, ImageID: e.imageID, Note: "n2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ReleaseSystemCluster(e.ctx, sys2.Cluster.ID); err != nil {
		t.Fatal(err)
	}
	for _, inst := range sys2.Instances {
		e.finish(inst, fleet.CmdWipeInstance, true, "ok")
	}
	if e.sweepClusters() != 1 || e.clusterView(sys2.Cluster.ID).State != rental.ClusterFailed || e.alerts("cluster_failed") != 0 {
		t.Fatal("never-ready system cluster should end failed without alert")
	}
	e.checkInvariants()
}

func TestClusterEndsWhenAllReleased(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res, r := e.readyCluster(2)
	if err := e.svc.ReleaseCluster(e.ctx, e.userID, res.Cluster.ID, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if e.sweepClusters() != 0 || e.clusterView(res.Cluster.ID).State != rental.ClusterActive {
		t.Fatal("wiping members keep the cluster active")
	}
	e.finish(r[0], fleet.CmdWipeInstance, true, "ok")
	if e.sweepClusters() != 0 {
		t.Fatal("one member still wiping")
	}
	e.finish(r[1], fleet.CmdWipeInstance, true, "ok")
	if e.sweepClusters() != 1 {
		t.Fatal("end")
	}
	if v := e.clusterView(res.Cluster.ID); v.State != rental.ClusterReleased || v.EndedAt == nil {
		t.Fatalf("state %s", v.State)
	}
	// 子网可被新集群复用
	if again := e.placeCluster(2); again.Cluster.SubnetIndex != res.Cluster.SubnetIndex {
		t.Fatalf("subnet %d not reused", again.Cluster.SubnetIndex)
	}
	e.checkInvariants()
}

// ---- 成员守卫 ----

func TestClusterMemberGuards(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	_, r := e.readyCluster(2)
	id := r[0].ID
	if _, err := e.svc.Renew(e.ctx, e.userID, id, catalog.D1d, ""); codeOf(err) != "cluster_member" {
		t.Fatalf("Renew = %v", err)
	}
	if _, err := e.svc.RenewQuantity(e.ctx, e.userID, id, catalog.D1d, 2, ""); codeOf(err) != "cluster_member" {
		t.Fatalf("RenewQuantity = %v", err)
	}
	if _, err := e.svc.SetAutoRenew(e.ctx, e.userID, id, true, catalog.D1d, ""); codeOf(err) != "cluster_member" {
		t.Fatalf("SetAutoRenew = %v", err)
	}
	if err := e.svc.Release(e.ctx, e.userID, id); codeOf(err) != "cluster_member" {
		t.Fatalf("Release = %v", err)
	}
	if _, err := e.svc.AdminExtend(e.ctx, audit.Actor{AdminID: 1}, id, 3600, "x"); codeOf(err) != "cluster_member" {
		t.Fatalf("AdminExtend = %v", err)
	}
	// 别人的实例仍是 not_found（不泄露成员身份）
	other := e.newUser("13600000099", 0)
	if err := e.svc.Release(e.ctx, other, id); codeOf(err) != "not_found" {
		t.Fatalf("other Release = %v", err)
	}
	// 访问类设置允许
	if _, err := e.svc.ResetPassword(e.ctx, e.userID, id, ""); err != nil {
		t.Fatalf("ResetPassword = %v", err)
	}
	if e.get(id).State != rental.StateRunning || e.count(`SELECT count(*) FROM orders WHERE kind = 'renew'`) != 0 {
		t.Fatal("guards must not change anything")
	}
}

// ---- 整组续费 ----

func TestRenewClusterAllOrNothing(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	price := e.unitPrice(catalog.D1d)
	poor := e.newUser("13600000002", 3*price) // 两台新购 + 只够一台续费
	req := e.clusterReq(2)
	req.UserID = poor
	res, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	r := byRank(t, res.Instances)
	e.runMembers(r...)
	e.sweepClusters()
	before := []time.Time{*e.get(r[0].ID).ExpiresAt, *e.get(r[1].ID).ExpiresAt}
	_, err = e.svc.RenewCluster(e.ctx, poor, res.Cluster.ID, catalog.D1d, 1, "rk")
	if codeOf(err) != "insufficient_balance" || !strings.Contains(err.Error(), "元") {
		t.Fatalf("RenewCluster = %v", err)
	}
	if e.count(`SELECT count(*) FROM orders WHERE kind = 'renew'`) != 0 || e.balance(poor) != price {
		t.Fatal("nothing must be charged")
	}
	for i, inst := range r {
		if !e.get(inst.ID).ExpiresAt.Equal(before[i]) {
			t.Fatal("nothing must be extended")
		}
	}
	if _, err := e.svc.RenewCluster(e.ctx, e.userID, res.Cluster.ID, catalog.D1d, 1, ""); codeOf(err) != "cluster_not_found" {
		t.Fatalf("other user = %v", err)
	}
}

func TestRenewClusterIdempotent(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res, r := e.readyCluster(2)
	price := e.unitPrice(catalog.D1d)
	bal := e.balance(e.userID)
	first, err := e.svc.RenewCluster(e.ctx, e.userID, res.Cluster.ID, catalog.D1d, 2, "renew-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].InstanceID != r[0].ID || first[1].InstanceID != r[1].ID || first[0].Kind != "renew" || first[0].Quantity != 2 {
		t.Fatalf("orders = %+v", first)
	}
	if e.balance(e.userID) != bal-4*price {
		t.Fatalf("charged %d", bal-e.balance(e.userID))
	}
	for i, inst := range r {
		if got := *e.get(inst.ID).ExpiresAt; !got.Equal(r[i].ExpiresAt.Add(48 * time.Hour)) {
			t.Fatalf("%s expires %v", inst.ID, got)
		}
	}
	again, err := e.svc.RenewCluster(e.ctx, e.userID, res.Cluster.ID, catalog.D1d, 2, "renew-1")
	if err != nil || len(again) != 2 || again[0].ID != first[0].ID || again[1].ID != first[1].ID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if e.balance(e.userID) != bal-4*price || e.count(`SELECT count(*) FROM orders WHERE kind = 'renew'`) != 2 {
		t.Fatal("replay must not charge")
	}
	// 同一键用在单实例续费 / 别的集群 → 冲突
	if _, err := e.svc.Renew(e.ctx, e.userID, r[0].ID, catalog.D1d, "renew-1#0"); codeOf(err) != "bad_request" {
		t.Fatalf("# key = %v", err)
	}
}

func TestRenewClusterBlocked(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res, r := e.readyCluster(2)
	// rank1 的机器在到期后不久有一条预订（只放得下几小时，续 1 天放不下）
	e.reserveRaw(r[1].MachineID, r[1].ExpiresAt.Add(rental.Tail+rental.PrepLead+3*time.Hour), 86400)
	bal := e.balance(e.userID)
	if _, err := e.svc.RenewCluster(e.ctx, e.userID, res.Cluster.ID, catalog.D1d, 1, ""); codeOf(err) != "renew_blocked" {
		t.Fatalf("RenewCluster = %v", err)
	}
	if e.balance(e.userID) != bal || !e.get(r[0].ID).ExpiresAt.Equal(*r[0].ExpiresAt) {
		t.Fatal("blocked renew must not touch anyone")
	}
}

// ---- 自动续费 / 提醒 ----

func TestAutoRenewClustersDue(t *testing.T) {
	// units < 0：用默认用户（资金充足）；否则新建一个只有 units 个 1 天单价的用户
	setup := func(t *testing.T, units int64) (*env, rental.BatchResult, []rental.Instance, int64) {
		e := newEnv(t, 0)
		f := e.fabric("A", "lan", "routed", false)
		e.lanMachines(f, "192.168.1", 2)
		uid := e.userID
		if units >= 0 {
			uid = e.newUser("13600000003", units*e.unitPrice(catalog.D1d))
		}
		req := e.clusterReq(2)
		req.UserID = uid
		res, err := e.svc.PlaceBatch(e.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		r := byRank(t, res.Instances)
		e.runMembers(r...)
		e.sweepClusters()
		if _, err := e.svc.SetClusterAutoRenew(e.ctx, uid, res.Cluster.ID, true, catalog.D1d, ""); err != nil {
			t.Fatal(err)
		}
		if e.count(`SELECT count(*) FROM audit_logs WHERE action = 'cluster.auto_renew'`) != 1 {
			t.Fatal("audit")
		}
		for i := range r {
			r[i] = e.get(r[i].ID)
			if !r[i].AutoRenew || r[i].AutoRenewDuration != catalog.D1d {
				t.Fatalf("member %d auto renew not set", i)
			}
		}
		return e, res, r, uid
	}
	t.Run("success", func(t *testing.T) {
		e, _, r, uid := setup(t, -1)
		bal := e.balance(uid)
		e.clock.Advance(24*time.Hour - rental.AutoRenewLead - time.Minute)
		if n, err := e.svc.AutoRenewClustersDue(e.ctx); err != nil || n != 0 {
			t.Fatalf("early = %d, %v", n, err)
		}
		e.clock.Advance(time.Minute)
		if n, err := e.svc.AutoRenewClustersDue(e.ctx); err != nil || n != 1 {
			t.Fatalf("due = %d, %v", n, err)
		}
		for _, inst := range r {
			if got := *e.get(inst.ID).ExpiresAt; !got.Equal(inst.ExpiresAt.Add(24 * time.Hour)) {
				t.Fatalf("%s expires %v", inst.ID, got)
			}
			if e.count(`SELECT count(*) FROM instance_events WHERE instance_id = $1 AND reason LIKE 'cluster auto renew %'`, inst.ID) != 1 {
				t.Fatal("one event per member")
			}
		}
		if e.balance(uid) != bal-2*e.unitPrice(catalog.D1d) || e.count(`SELECT count(*) FROM orders WHERE kind = 'renew' AND auto`) != 2 {
			t.Fatal("charge")
		}
		if n, _ := e.svc.AutoRenewClustersDue(e.ctx); n != 0 {
			t.Fatal("renewed twice")
		}
	})
	t.Run("insufficient", func(t *testing.T) {
		e, _, r, uid := setup(t, 2)
		e.clock.Advance(24*time.Hour - rental.AutoRenewLead)
		if n, err := e.svc.AutoRenewClustersDue(e.ctx); err != nil || n != 0 {
			t.Fatalf("due = %d, %v", n, err)
		}
		for _, inst := range r {
			got := e.get(inst.ID)
			if got.AutoRenewFailedAt == nil || got.AutoRenewFailedReason != "insufficient_balance" || !got.ExpiresAt.Equal(*inst.ExpiresAt) {
				t.Fatalf("member %+v", got)
			}
		}
		calls := e.notifier.Calls()
		if len(calls) != 1 || calls[0] != "auto_renew_failed:"+r[0].ID+":余额不足" {
			t.Fatalf("sms = %v", calls)
		}
		if e.count(`SELECT count(*) FROM orders WHERE kind = 'renew'`) != 0 || e.balance(uid) != 0 {
			t.Fatal("nothing charged")
		}
		e.clock.Advance(rental.AutoRenewRetry)
		if _, err := e.svc.AutoRenewClustersDue(e.ctx); err != nil {
			t.Fatal(err)
		}
		if len(e.notifier.Calls()) != 1 {
			t.Fatal("sms only on first failure")
		}
	})
	t.Run("suspended member", func(t *testing.T) {
		e, _, r, _ := setup(t, -1)
		e.clock.Advance(time.Hour)
		if _, err := e.svc.SuspendMachines(e.ctx, []int64{r[1].MachineID}); err != nil {
			t.Fatal(err)
		}
		paused := *e.get(r[1].ID).PausedRemaining
		e.clock.Advance(23*time.Hour - rental.AutoRenewLead)
		if n, err := e.svc.AutoRenewClustersDue(e.ctx); err != nil || n != 1 {
			t.Fatalf("due = %d, %v", n, err)
		}
		if got := *e.get(r[0].ID).ExpiresAt; !got.Equal(r[0].ExpiresAt.Add(24 * time.Hour)) {
			t.Fatalf("rank0 expires %v", got)
		}
		if got := *e.get(r[1].ID).PausedRemaining; got != paused+86400 {
			t.Fatalf("rank1 paused %d, want %d", got, paused+86400)
		}
	})
}

func TestAutoRenewDueSkipsMembers(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res, _ := e.readyCluster(2)
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET auto_renew = true WHERE cluster_id = $1`, res.Cluster.ID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(24*time.Hour - rental.AutoRenewLead)
	if n, err := e.svc.AutoRenewDue(e.ctx); err != nil || n != 0 {
		t.Fatalf("AutoRenewDue = %d, %v", n, err)
	}
	if e.count(`SELECT count(*) FROM orders WHERE kind = 'renew'`) != 0 {
		t.Fatal("members must not be renewed one by one")
	}
}

func TestRemindersOncePerCluster(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 4)
	_, r := e.readyCluster(4)
	e.clock.Advance(23*time.Hour + 10*time.Minute)
	n, err := e.svc.SendReminders(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("SendReminders = %d, %v", n, err)
	}
	calls := e.notifier.Calls()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], r[0].ID+":") {
		t.Fatalf("calls = %v", calls)
	}
	for _, inst := range r {
		if got := e.get(inst.ID); !got.Reminded1h || !got.Reminded24h {
			t.Fatalf("member %s flags", inst.ID)
		}
	}
	if n, _ := e.svc.SendReminders(e.ctx); n != 0 || len(e.notifier.Calls()) != 1 {
		t.Fatal("second round must not send")
	}
}

// ---- 集群级接口 ----

func TestReleaseCluster(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 4)
	forming := e.placeCluster(2)
	if err := e.svc.ReleaseCluster(e.ctx, e.userID, forming.Cluster.ID, ""); codeOf(err) != "invalid_state" {
		t.Fatalf("creating members = %v", err)
	}
	res, r := e.readyCluster(2)
	other := e.newUser("13600000004", 0)
	if err := e.svc.ReleaseCluster(e.ctx, other, res.Cluster.ID, ""); codeOf(err) != "cluster_not_found" {
		t.Fatalf("other = %v", err)
	}
	bal := e.balance(e.userID)
	if err := e.svc.ReleaseCluster(e.ctx, e.userID, res.Cluster.ID, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	for _, inst := range r {
		if e.get(inst.ID).State != rental.StateWiping {
			t.Fatal("member not wiping")
		}
	}
	if e.balance(e.userID) != bal || e.count(`SELECT count(*) FROM audit_logs WHERE action = 'cluster.release' AND target_id = $1`, res.Cluster.ID) != 1 {
		t.Fatal("no refund / audit")
	}
	if err := e.svc.ReleaseCluster(e.ctx, e.userID, res.Cluster.ID, ""); codeOf(err) != "invalid_state" {
		t.Fatalf("second release = %v", err)
	}
}

func TestAdminExtendCluster(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res, r := e.readyCluster(2)
	if _, err := e.svc.AdminExtendCluster(e.ctx, audit.Actor{AdminID: 1}, res.Cluster.ID, 30, "x"); codeOf(err) != "bad_request" {
		t.Fatalf("too short = %v", err)
	}
	v, err := e.svc.AdminExtendCluster(e.ctx, audit.Actor{AdminID: 1}, res.Cluster.ID, 3600, "补偿")
	if err != nil || v.ID != res.Cluster.ID {
		t.Fatal(err)
	}
	for _, inst := range r {
		if got := *e.get(inst.ID).ExpiresAt; !got.Equal(inst.ExpiresAt.Add(time.Hour)) {
			t.Fatalf("expires %v", got)
		}
	}
	if e.count(`SELECT count(*) FROM audit_logs WHERE action = 'cluster.extend' AND target_id = $1`, res.Cluster.ID) != 1 ||
		e.count(`SELECT count(*) FROM orders WHERE kind = 'renew'`) != 0 {
		t.Fatal("audit / no orders")
	}
}

func TestAdminResyncCluster(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res, r := e.readyCluster(2)
	v, err := e.svc.AdminResyncCluster(e.ctx, audit.Actor{AdminID: 1}, res.Cluster.ID)
	if err != nil || v.Version != 2 {
		t.Fatalf("resync = %+v, %v", v.Cluster, err)
	}
	for rank, inst := range r {
		if countPending(e, inst.MachineID, fleet.CmdConfigureCluster) != 1 {
			t.Fatal("one configure_cluster per member")
		}
		p := e.configurePayload(inst)
		if p.Cluster.Version != 2 || p.Cluster.Rank != rank || p.Cluster.ClusterID != res.Cluster.ID || len(p.Cluster.Members) != 2 {
			t.Fatalf("payload %+v", p.Cluster)
		}
	}
	if e.count(`SELECT count(*) FROM audit_logs WHERE action = 'cluster.resync'`) != 1 {
		t.Fatal("audit")
	}
}

// ---- 换机 / 退款 ----

func TestMigrateClusterMember(t *testing.T) {
	e := newEnv(t, 1) // 一台不在 fabric 的空闲机器：换机不能选它
	loose := e.machines[0]
	f := e.fabric("A", "lan", "routed", false)
	fm := e.lanMachines(f, "192.168.1", 3)
	other := e.fabric("B", "lan", "routed", false)
	e.lanMachines(other, "192.168.1", 1) // 别的 fabric 的空闲机器也不能选
	res, r := e.readyCluster(2)
	moved, err := e.svc.AdminMigrate(e.ctx, audit.Actor{AdminID: 1}, r[1].ID, "nic broken")
	if err != nil {
		t.Fatal(err)
	}
	if moved.ClusterID == nil || *moved.ClusterID != res.Cluster.ID || *moved.ClusterRank != 1 || !slices.Contains(fm, moved.MachineID) ||
		moved.MachineID == loose || moved.MachineID == r[0].MachineID || moved.MachineID == r[1].MachineID {
		t.Fatalf("moved = %+v", moved)
	}
	if e.get(r[1].ID).State != rental.StateWiping {
		t.Fatal("old member wiping")
	}
	v := e.clusterView(res.Cluster.ID)
	if v.Version != 2 || len(v.Members) != 2 || v.Members[1].InstanceID != moved.ID {
		t.Fatalf("view = %+v", v)
	}
	p := e.createPayload(moved)
	if p.Cluster == nil || p.Cluster.Version != 2 || p.Cluster.Rank != 1 || len(p.Cluster.Members) != 2 || p.Cluster.Members[1].InstanceID != moved.ID {
		t.Fatalf("create spec = %+v", p.Cluster)
	}
	cp := e.configurePayload(r[0])
	if cp.Cluster.Version != 2 || cp.Cluster.Rank != 0 || cp.Cluster.Members[1].InstanceID != moved.ID || cp.Cluster.Members[1].HostIP != e.lanIP(moved.MachineID) {
		t.Fatalf("configure spec = %+v", cp.Cluster)
	}
	if countPending(e, moved.MachineID, fleet.CmdConfigureCluster) != 0 {
		t.Fatal("new member gets the spec in Create, not configure_cluster")
	}
	e.runMembers(moved)
	e.checkInvariants()
	// fabric 内已无空闲机器（loose 与 B 的机器不算）→ no_capacity，全部回滚
	if _, err := e.svc.AdminMigrate(e.ctx, audit.Actor{AdminID: 1}, r[0].ID, "again"); codeOf(err) != "no_capacity" {
		t.Fatalf("no fabric capacity = %v", err)
	}
	if e.get(r[0].ID).State != rental.StateRunning || e.clusterView(res.Cluster.ID).Version != 2 {
		t.Fatal("failed migrate must roll back")
	}
}

func TestRefundReleaseClusterMember(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res, r := e.readyCluster(2)
	if _, err := e.svc.RefundRelease(e.ctx, audit.Actor{AdminID: 1}, r[1].ID, 100, "bad"); err != nil {
		t.Fatal(err)
	}
	if e.get(r[1].ID).State != rental.StateWiping {
		t.Fatal("member wiping")
	}
	v := e.clusterView(res.Cluster.ID)
	if v.Version != 2 || len(v.Members) != 1 || v.Status != rental.ClusterDegraded || v.State != rental.ClusterActive {
		t.Fatalf("view = %+v", v)
	}
	cp := e.configurePayload(r[0])
	if cp.Cluster.Version != 2 || len(cp.Cluster.Members) != 1 || cp.Cluster.Members[0].InstanceID != r[0].ID || cp.Cluster.Size != 2 {
		t.Fatalf("configure spec = %+v", cp.Cluster)
	}
	if e.sweepClusters() != 0 {
		t.Fatal("ready cluster with a released member stays active")
	}
	e.checkInvariants()
}

// ---- 指令结果 / 对账 ----

func TestConfigureClusterResultAndTimeout(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	res, r := e.readyCluster(2)
	if rental.CommandTimeout(fleet.CmdConfigureCluster) != rental.ConfigureClusterCommandTimeout {
		t.Fatal("timeout table")
	}
	if _, err := e.svc.AdminResyncCluster(e.ctx, audit.Actor{AdminID: 1}, res.Cluster.ID); err != nil {
		t.Fatal(err)
	}
	e.finish(r[0], fleet.CmdConfigureCluster, true, "ok")
	if e.alerts("cluster_net_error") != 0 {
		t.Fatal("success has no side effect")
	}
	if _, err := e.svc.AdminResyncCluster(e.ctx, audit.Actor{AdminID: 1}, res.Cluster.ID); err != nil {
		t.Fatal(err)
	}
	e.finish(r[0], fleet.CmdConfigureCluster, false, "cluster-net: route replace failed")
	if e.alerts("cluster_net_error") != 1 || e.machineState(r[0].MachineID) != fleet.StateAllocated || e.get(r[0].ID).State != rental.StateRunning {
		t.Fatal("failure only alerts")
	}
	// rank1 的 configure_cluster（两条在途）下发后 2 分钟未回报 → 判失败 → 告警
	if err := e.fleet.AttachAgentConn(e.ctx, r[1].MachineID, "c1"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := e.fleet.ClaimPending(e.ctx, []string{"c1"}); err != nil || len(claimed) != 2 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	e.clock.Advance(rental.ConfigureClusterCommandTimeout - time.Second)
	e.heartbeatAll()
	mustCount(t, firstOf(e.svc.FailTimedOutCommands(e.ctx)), nil, 0, "before timeout")
	e.clock.Advance(time.Second)
	mustCount(t, firstOf(e.svc.FailTimedOutCommands(e.ctx)), nil, 2, "at timeout")
	if e.count(`SELECT count(*) FROM alerts WHERE kind = 'cluster_net_error' AND instance_id = $1`, r[1].ID) != 1 {
		t.Fatal("timeout alert once")
	}
}

func TestReconcileClusterErrorAlert(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	e.lanMachines(f, "192.168.1", 2)
	_, r := e.readyCluster(2)
	rep := fleet.StateReport{Instances: []fleet.InstanceState{{InstanceID: r[0].ID, Phase: fleet.PhaseRunning, Cluster: "ok"}}}
	if err := e.svc.Reconcile(e.ctx, r[0].MachineID, rep); err != nil {
		t.Fatal(err)
	}
	if e.alerts("cluster_net_error") != 0 {
		t.Fatal("ok is not an error")
	}
	rep.Instances[0].Cluster = "error: peer 10.210.0.12 unreachable"
	for i := 0; i < 2; i++ {
		if err := e.svc.Reconcile(e.ctx, r[0].MachineID, rep); err != nil {
			t.Fatal(err)
		}
	}
	if e.count(`SELECT count(*) FROM alerts WHERE kind = 'cluster_net_error' AND message = $1`, rental.AlertClusterNetError(r[0].ID)) != 1 {
		t.Fatal("one alert")
	}
	if e.get(r[0].ID).State != rental.StateRunning {
		t.Fatal("state unchanged")
	}
}
