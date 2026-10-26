package rental_test

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

func (e *env) lanIP(machineID int64) string {
	e.t.Helper()
	var ip string
	if err := e.pool.QueryRow(e.ctx, `SELECT lan_ip FROM machines WHERE id = $1`, machineID).Scan(&ip); err != nil {
		e.t.Fatal(err)
	}
	return ip
}

// byRank 按 ClusterRank 排序实例（rank 必须都已赋值）。
func byRank(t *testing.T, insts []rental.Instance) []rental.Instance {
	t.Helper()
	out := slices.Clone(insts)
	for _, i := range out {
		if i.ClusterID == nil || i.ClusterRank == nil {
			t.Fatalf("instance %s has no cluster", i.ID)
		}
	}
	slices.SortFunc(out, func(a, b rental.Instance) int { return *a.ClusterRank - *b.ClusterRank })
	return out
}

// assertClusterShape：实例数 = size、同一集群、全在 fabric 上、rank 按机器 ID 升序 0..N-1。
func assertClusterShape(t *testing.T, res rental.BatchResult, size int, fabricMachines []int64) {
	t.Helper()
	if res.Cluster == nil || res.Cluster.Size != size || len(res.Instances) != size || len(res.Orders) != size {
		t.Fatalf("cluster result = %+v", res)
	}
	ranked := byRank(t, res.Instances)
	for r, inst := range ranked {
		if *inst.ClusterID != res.Cluster.ID || *inst.ClusterRank != r {
			t.Fatalf("rank %d = %+v", r, inst)
		}
		if !slices.Contains(fabricMachines, inst.MachineID) {
			t.Fatalf("instance %s on machine %d outside fabric %v", inst.ID, inst.MachineID, fabricMachines)
		}
		if r > 0 && ranked[r-1].MachineID >= inst.MachineID {
			t.Fatalf("ranks not by machine id: %v", ranked)
		}
	}
}

func TestClusterPlaceAtomicSameFabric(t *testing.T) {
	e := newEnv(t, 2) // 两台零散机器（无 fabric）
	fa := e.fabric("A", "lan", "routed", false)
	fb := e.fabric("B", "lan", "routed", false)
	e.lanMachines(fa, "192.168.1", 3)
	bm := e.lanMachines(fb, "192.168.2", 4)
	before := e.balance(e.userID)

	req := e.clusterReq(4)
	req.IdempotencyKey = "clu-k1"
	res, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	assertClusterShape(t, res, 4, bm)
	c := res.Cluster
	if c.FabricID != fb || c.Mode != clusterspec.ModeRouted || c.State != rental.ClusterActive || c.Status != rental.ClusterForming ||
		c.System || c.UserID != e.userID || c.BatchID == nil || *c.BatchID != res.Batch.ID || c.Version != 1 || c.VLAN != nil || c.RDMA ||
		c.Network != "ethernet" || c.FabricName != "B" || c.FabricKind != "lan" || c.Subnet != "10.210.0.0/24" || c.HeadIP != "10.210.0.11" {
		t.Fatalf("cluster = %+v", c)
	}
	if len(c.Members) != 4 {
		t.Fatalf("members = %+v", c.Members)
	}
	// 四张订单同一 batch、同价，钱包扣 4 × 单价
	var sum int64
	for _, o := range res.Orders {
		if o.Kind != "new" || o.BatchID == nil || *o.BatchID != res.Batch.ID || o.Amount != res.Orders[0].Amount {
			t.Fatalf("order = %+v", o)
		}
		sum += o.Amount
	}
	if sum != 4*res.Orders[0].Amount || e.balance(e.userID) != before-sum || sum == 0 {
		t.Fatalf("charged %d, balance %d → %d", sum, before, e.balance(e.userID))
	}
	// 四条 Create 载荷的 Spec 互为对端：routed、host_ip = 各机 lan_ip、IP = .11 + rank、Members 完全相同
	var first []clusterspec.Member
	for r, inst := range byRank(t, res.Instances) {
		p := e.createPayload(inst)
		if p.Cluster == nil {
			t.Fatalf("rank %d create without cluster spec", r)
		}
		s := *p.Cluster
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
		if s.ClusterID != c.ID || s.Rank != r || s.Size != 4 || s.Mode != clusterspec.ModeRouted || s.Iface != "eth0" || s.Subnet != "10.210.0.0/24" ||
			s.Version != 1 || s.RDMA || s.VLAN != 0 || len(s.Members) != 4 || p.SSHPort != testSSHBasePort || p.ShareSlots != 1 || p.ImageRef == "" {
			t.Fatalf("rank %d spec = %+v (payload %+v)", r, s, p)
		}
		for _, m := range s.Members {
			mi := byRank(t, res.Instances)[m.Rank]
			if m.InstanceID != mi.ID || m.HostIP != e.lanIP(mi.MachineID) || m.IP != fmt.Sprintf("10.210.0.%d", 11+m.Rank) {
				t.Fatalf("member = %+v", m)
			}
		}
		if first == nil {
			first = s.Members
		} else if !slices.Equal(first, s.Members) {
			t.Fatalf("members differ: %v vs %v", first, s.Members)
		}
	}
	// 库里同样
	if n := e.count(`SELECT count(*) FROM instances WHERE cluster_id = $1 AND batch_id = $2`, c.ID, res.Batch.ID); n != 4 {
		t.Fatalf("db members = %d", n)
	}
	// 幂等回放：同一批次、带集群视图，不再扣款
	again, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil || again.Batch.ID != res.Batch.ID || again.Cluster == nil || again.Cluster.ID != c.ID || len(again.Instances) != 4 {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if e.balance(e.userID) != before-sum || e.count(`SELECT count(*) FROM instance_clusters`) != 1 {
		t.Fatal("replay must not charge or create")
	}
	// 普通单机查询仍正常（GetBatch 带集群）
	got, err := e.svc.GetBatch(e.ctx, e.userID, res.Batch.ID)
	if err != nil || got.Cluster == nil || got.Cluster.ID != c.ID {
		t.Fatalf("GetBatch = %+v, %v", got, err)
	}
}

func TestClusterPlacePrefersRDMAThenTightest(t *testing.T) {
	t.Run("rdma first", func(t *testing.T) {
		e := newEnv(t, 0)
		fl := e.fabric("L", "lan", "routed", false)
		fr := e.fabric("R", "cx7", "vlan", true, 100, 110)
		lm := e.lanMachines(fl, "192.168.1", 2)
		rm := e.cx7Machines(fr, 3, true)
		res := e.placeCluster(2)
		assertClusterShape(t, res, 2, rm)
		c := res.Cluster
		if !c.RDMA || c.Network != "rdma" || c.Mode != clusterspec.ModeMacvlan || c.VLAN == nil || *c.VLAN != 100 {
			t.Fatalf("cluster = %+v", c)
		}
		// R 只剩 1 台：下一个 2 台集群落到 L
		res2 := e.placeCluster(2)
		assertClusterShape(t, res2, 2, lm)
		if res2.Cluster.RDMA || res2.Cluster.Network != "ethernet" || res2.Cluster.SubnetIndex != 1 {
			t.Fatalf("second = %+v", res2.Cluster)
		}
	})
	t.Run("tightest", func(t *testing.T) {
		e := newEnv(t, 0)
		f1 := e.fabric("F1", "lan", "routed", false)
		f2 := e.fabric("F2", "lan", "routed", false)
		e.lanMachines(f1, "192.168.1", 3)
		m2 := e.lanMachines(f2, "192.168.2", 2)
		assertClusterShape(t, e.placeCluster(2), 2, m2)
	})
	t.Run("rdma degraded", func(t *testing.T) {
		e := newEnv(t, 0)
		fr := e.fabric("R", "cx7", "vlan", true, 100, 110)
		rm := e.cx7Machines(fr, 1, true)
		rm = append(rm, e.cx7Machines(fr, 1, false)...)
		res := e.placeCluster(2)
		assertClusterShape(t, res, 2, rm)
		if res.Cluster.RDMA || res.Cluster.Network != "rdma_degraded" {
			t.Fatalf("cluster = %+v", res.Cluster)
		}
		if n := e.count(`SELECT count(*) FROM alerts WHERE kind = 'cluster_rdma_degraded' AND level = 'warning' AND message = $1`,
			rental.AlertClusterRDMADegraded(res.Cluster.ID)); n != 1 {
			t.Fatalf("degraded alerts = %d", n)
		}
		for _, inst := range res.Instances {
			if p := e.createPayload(inst); p.Cluster.RDMA {
				t.Fatal("spec must not ask for rdma")
			}
		}
	})
}

func TestClusterPlaceDirectOnlyTwo(t *testing.T) {
	e := newEnv(t, 0)
	fd := e.fabric("D", "cx7", "direct", true)
	dm := e.cx7Machines(fd, 4, true) // 绕过 fleet 的「direct 最多 2 台」，验证分配本身也只组 2 台
	if _, err := e.svc.PlaceBatch(e.ctx, e.clusterReq(4)); codeOf(err) != "no_capacity" {
		t.Fatalf("direct 4 = %v", err)
	}
	res := e.placeCluster(2)
	assertClusterShape(t, res, 2, dm)
	if res.Cluster.Mode != clusterspec.ModeMacvlan || res.Cluster.VLAN != nil || !res.Cluster.RDMA {
		t.Fatalf("cluster = %+v", res.Cluster)
	}
	for _, inst := range res.Instances {
		if s := e.createPayload(inst).Cluster; s.Iface != "enp1s0f1np1" || s.VLAN != 0 || s.Mode != clusterspec.ModeMacvlan {
			t.Fatalf("spec = %+v", s)
		}
	}
}

func TestClusterPlaceNoCapacity(t *testing.T) {
	e := newEnv(t, 2)
	f := e.fabric("A", "lan", "routed", false)
	fm := e.lanMachines(f, "192.168.1", 3)
	before := e.balance(e.userID)
	_, err := e.svc.PlaceBatch(e.ctx, e.clusterReq(4))
	ae := apperr.As(err)
	if ae == nil || ae.Code != "no_capacity" || ae.Message != apperr.ClusterNoCapacity(4).Message || ae.Status != 409 {
		t.Fatalf("err = %v", err)
	}
	if e.count(`SELECT count(*) FROM instances`) != 0 || e.count(`SELECT count(*) FROM instance_clusters`) != 0 ||
		e.count(`SELECT count(*) FROM rental_batches`) != 0 || e.count(`SELECT count(*) FROM orders`) != 0 || e.balance(e.userID) != before {
		t.Fatal("no_capacity must leave nothing behind")
	}
	for _, m := range fm {
		if e.machineState(m) != fleet.StateIdle {
			t.Fatal("machine left allocated")
		}
	}
}

func TestOldAgentExcluded(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	fm := e.lanMachines(f, "192.168.1", 4)
	// 旧 Agent（无能力位）：fleet 判为未就绪
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET agent_features = 0, cluster_ready = false, cluster_net = '' WHERE id = $1`, fm[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PlaceBatch(e.ctx, e.clusterReq(4)); codeOf(err) != "no_capacity" {
		t.Fatalf("4 with old agent = %v", err)
	}
	res := e.placeCluster(2)
	assertClusterShape(t, res, 2, fm[1:])
	av, err := e.svc.ClusterAvailability(e.ctx, e.productID, nil)
	if err != nil || av[4].Available != 0 || av[2].Available != 0 {
		t.Fatalf("availability = %v, %v", av, err)
	}
	// 单机订单照常能用旧 Agent 机器
	_, inst := e.place(catalog.D1d)
	if inst.MachineID != fm[0] || inst.ClusterID != nil {
		t.Fatalf("single = %+v", inst)
	}
}

func TestClusterPlaceValidation(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	fm := e.lanMachines(f, "192.168.1", 4)
	start := e.clock.Now().Add(48 * time.Hour).Truncate(time.Hour)
	app := int64(1)
	cases := []struct {
		name string
		mut  func(r *rental.BatchRequest)
		code string
		msg  string
	}{
		{"count 3", func(r *rental.BatchRequest) { r.Count = 3 }, "cluster_size_invalid", ""},
		{"count 1", func(r *rental.BatchRequest) { r.Count = 1 }, "cluster_size_invalid", ""},
		{"count 8", func(r *rental.BatchRequest) { r.Count = 8 }, "cluster_size_invalid", ""},
		{"booking", func(r *rental.BatchRequest) { r.StartAt = &start }, "cluster_booking_unsupported", ""},
		{"machine", func(r *rental.BatchRequest) { r.MachineID = &fm[0] }, "bad_request", "集群不能指定机器"},
		{"app", func(r *rental.BatchRequest) { r.AppID = &app }, "bad_request", "集群请在开通后到集群页部署应用"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := e.clusterReq(2)
			c.mut(&req)
			_, err := e.svc.PlaceBatch(e.ctx, req)
			ae := apperr.As(err)
			if ae == nil || ae.Code != c.code || (c.msg != "" && ae.Message != c.msg) {
				t.Fatalf("want %s %q, got %v", c.code, c.msg, err)
			}
		})
	}
	if e.count(`SELECT count(*) FROM instances`) != 0 {
		t.Fatal("rejected requests must not place")
	}
}

func TestClusterPlaceSplitsByNetwork(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	n1 := e.lanMachines(f, "192.168.1", 2)
	n2 := e.lanMachines(f, "192.168.2", 2)
	if _, err := e.svc.PlaceBatch(e.ctx, e.clusterReq(4)); codeOf(err) != "no_capacity" {
		t.Fatalf("4 across subnets = %v", err)
	}
	first := e.placeCluster(2)
	second := e.placeCluster(2)
	for _, res := range []rental.BatchResult{first, second} {
		a, b := res.Instances[0].MachineID, res.Instances[1].MachineID
		same := (slices.Contains(n1, a) && slices.Contains(n1, b)) || (slices.Contains(n2, a) && slices.Contains(n2, b))
		if !same {
			t.Fatalf("cluster %s spans subnets: %d, %d", res.Cluster.ID, a, b)
		}
		for _, inst := range res.Instances {
			for _, m := range e.createPayload(inst).Cluster.Members {
				if m.HostIP[:10] != e.lanIP(a)[:10] {
					t.Fatalf("host ip %s not in %s", m.HostIP, e.lanIP(a))
				}
			}
		}
	}
}

func TestClusterPlaceRetryResetsInstances(t *testing.T) {
	e := newEnv(t, 0)
	fa := e.fabric("A", "lan", "routed", false)
	fb := e.fabric("B", "lan", "routed", false)
	am := e.lanMachines(fa, "192.168.1", 2) // 空闲 2：排在 B（空闲 3）之前
	bm := e.lanMachines(fb, "192.168.2", 3)
	// 并发事务锁住 A 的一台：A 只挑到 1 台（回调已追加 1 个实例）→ 回滚 → B
	lock, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(e.ctx)
	if _, err := lock.Exec(e.ctx, `SELECT 1 FROM machines WHERE id = $1 FOR NO KEY UPDATE`, am[1]); err != nil {
		t.Fatal(err)
	}
	res := e.placeCluster(2)
	assertClusterShape(t, res, 2, bm)
	if n := e.count(`SELECT count(*) FROM instances WHERE user_id = $1`, e.userID); n != 2 {
		t.Fatalf("instances = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM instances WHERE machine_id = ANY($1)`, am); n != 0 {
		t.Fatalf("leftover on fabric A = %d", n)
	}
	if e.machineState(am[0]) != fleet.StateIdle || e.count(`SELECT count(*) FROM machine_leases WHERE machine_id = ANY($1)`, am) != 0 {
		t.Fatal("fabric A attempt not rolled back")
	}
	if e.count(`SELECT count(*) FROM orders WHERE user_id = $1`, e.userID) != 2 {
		t.Fatal("orders")
	}
}

func TestClusterPlaceConcurrent(t *testing.T) {
	for round := 0; round < 3; round++ {
		e := newEnv(t, 0)
		f := e.fabric("A", "lan", "routed", false)
		e.lanMachines(f, "192.168.1", 6)
		users := []int64{e.userID, e.newUser("13600000002", startFunds)}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		for i, uid := range users {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := e.clusterReq(4)
				req.UserID = uid
				_, errs[i] = e.svc.PlaceBatch(ctx, req)
			}()
		}
		wg.Wait()
		cancel()
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case codeOf(err) != "no_capacity":
				t.Fatalf("unexpected error %v", err)
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d succeeded (%v)", round, ok, errs)
		}
		// 任何 active 集群：在位成员数 = size、同 fabric
		if n := e.count(`SELECT count(*) FROM instance_clusters c WHERE c.state = 'active' AND (
				c.size <> (SELECT count(*) FROM instances i WHERE i.cluster_id = c.id AND i.state IN ('creating', 'running', 'stopped'))
				OR EXISTS (SELECT 1 FROM instances i JOIN machines m ON m.id = i.machine_id WHERE i.cluster_id = c.id AND m.fabric_id IS DISTINCT FROM c.fabric_id))`); n != 0 {
			t.Fatalf("broken clusters = %d", n)
		}
		if e.count(`SELECT count(*) FROM instances`) != 4 || e.count(`SELECT count(*) FROM instance_clusters`) != 1 {
			t.Fatal("exactly one cluster of 4")
		}
	}
}

func TestClusterSubnetAndVLANAllocation(t *testing.T) {
	t.Run("sequential and reuse", func(t *testing.T) {
		e := newEnv(t, 0)
		f := e.fabric("A", "lan", "routed", false)
		e.lanMachines(f, "192.168.1", 6)
		var ids []string
		for i := 0; i < 3; i++ {
			res := e.placeCluster(2)
			if res.Cluster.SubnetIndex != i || res.Cluster.Subnet != fmt.Sprintf("10.210.%d.0/24", i) {
				t.Fatalf("cluster %d subnet = %d", i, res.Cluster.SubnetIndex)
			}
			ids = append(ids, res.Cluster.ID)
		}
		if _, err := e.pool.Exec(e.ctx, `UPDATE instance_clusters SET state = 'released', ended_at = $2 WHERE id = $1`, ids[1], e.clock.Now()); err != nil {
			t.Fatal(err)
		}
		// 子网要等成员全部结束才释放（中止的集群可能还有成员在洗机）
		if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'released' WHERE cluster_id = $1`, ids[1]); err != nil {
			t.Fatal(err)
		}
		e.lanMachines(f, "192.168.1", 2)
		if res := e.placeCluster(2); res.Cluster.SubnetIndex != 1 {
			t.Fatalf("reuse subnet = %d", res.Cluster.SubnetIndex)
		}
	})
	t.Run("pool", func(t *testing.T) {
		e := newEnv(t, 0)
		if e.svc.SubnetPool().String() != clusterspec.DefaultPool {
			t.Fatalf("default pool = %s", e.svc.SubnetPool())
		}
		custom := rental.NewService(rental.Deps{Pool: e.pool, Clock: e.clock, ClusterSubnetPool: netip.MustParsePrefix("10.77.0.0/16")})
		if custom.SubnetPool().String() != "10.77.0.0/16" {
			t.Fatalf("custom pool = %s", custom.SubnetPool())
		}
	})
	t.Run("vlan", func(t *testing.T) {
		e := newEnv(t, 0)
		f := e.fabric("V", "cx7", "vlan", false, 100, 101)
		e.cx7Machines(f, 6, false)
		for _, want := range []int{100, 101} {
			res := e.placeCluster(2)
			if res.Cluster.VLAN == nil || *res.Cluster.VLAN != want {
				t.Fatalf("vlan = %v, want %d", res.Cluster.VLAN, want)
			}
			for _, inst := range res.Instances {
				if s := e.createPayload(inst).Cluster; s.VLAN != want {
					t.Fatalf("spec vlan = %d", s.VLAN)
				}
			}
		}
		// VLAN 用尽：还有 2 台空闲也组不成
		_, err := e.svc.PlaceBatch(e.ctx, e.clusterReq(2))
		if ae := apperr.As(err); ae == nil || ae.Code != "no_capacity" || ae.Message != apperr.ClusterNoCapacity(2).Message {
			t.Fatalf("vlan exhausted = %v", err)
		}
		if e.count(`SELECT count(*) FROM instances WHERE state = 'creating'`) != 4 {
			t.Fatal("exhausted attempt must roll back")
		}
	})
	t.Run("concurrent 20", func(t *testing.T) {
		e := newEnv(t, 0)
		f := e.fabric("A", "lan", "routed", false)
		ms := e.lanMachines(f, "192.168.1", 40)
		pool := e.toPool(ms...)
		var wg sync.WaitGroup
		results := make([]rental.SystemClusterResult, 20)
		errs := make([]error, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = e.svc.PlaceSystemCluster(e.ctx, rental.SystemClusterRequest{MachineIDs: ms[2*i : 2*i+2], GroupID: pool, ImageID: e.imageID, Note: "c"})
			}()
		}
		wg.Wait()
		seen := map[int]bool{}
		for i := range results {
			if errs[i] != nil {
				t.Fatalf("cluster %d: %v", i, errs[i])
			}
			if seen[results[i].Cluster.SubnetIndex] {
				t.Fatalf("duplicate subnet %d", results[i].Cluster.SubnetIndex)
			}
			seen[results[i].Cluster.SubnetIndex] = true
		}
		if len(seen) != 20 || !seen[0] || !seen[19] {
			t.Fatalf("subnets = %v", seen)
		}
	})
}

func TestPlaceSystemClusterAtomic(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	fm := e.lanMachines(f, "192.168.1", 3)
	other := e.lanMachines(f, "192.168.2", 1)[0] // 同 fabric、不同网段
	loose := e.newMachine("LOOSE")               // 不在 fabric
	pool := e.toPool(append(append(slices.Clone(fm), other), loose)...)
	outside := e.lanMachines(f, "192.168.1", 1)[0] // 在 fabric、不在池
	req := func(ids ...int64) rental.SystemClusterRequest {
		return rental.SystemClusterRequest{MachineIDs: ids, GroupID: pool, ImageID: e.imageID, Note: "llm node node-x m"}
	}
	if _, err := e.svc.PlaceSystemCluster(e.ctx, req(fm...)); codeOf(err) != "cluster_size_invalid" {
		t.Fatalf("3 machines = %v", err)
	}
	notReady := fm[2]
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET cluster_ready = false WHERE id = $1`, notReady); err != nil {
		t.Fatal(err)
	}
	for name, ids := range map[string][]int64{
		"loose":     {fm[0], loose},
		"other net": {fm[0], other},
		"outside":   {fm[0], outside},
		"not ready": {fm[0], notReady},
		"duplicate": {fm[0], fm[0]},
	} {
		if _, err := e.svc.PlaceSystemCluster(e.ctx, req(ids...)); codeOf(err) != "machine_unavailable" {
			t.Fatalf("%s = %v", name, err)
		}
	}
	if e.count(`SELECT count(*) FROM instances`) != 0 || e.count(`SELECT count(*) FROM instance_clusters`) != 0 || e.count(`SELECT count(*) FROM machine_leases`) != 0 {
		t.Fatal("failed attempts must roll back")
	}
	for _, m := range []int64{fm[0], fm[1], loose, other} {
		if e.machineState(m) != fleet.StateIdle {
			t.Fatalf("machine %d not idle", m)
		}
	}

	res, err := e.svc.PlaceSystemCluster(e.ctx, req(fm[1], fm[0])) // 乱序传入，rank 仍按机器 ID
	if err != nil {
		t.Fatal(err)
	}
	c := res.Cluster
	if !c.System || c.UserID != e.systemID || c.BatchID != nil || c.Size != 2 || c.State != rental.ClusterActive || c.FabricID != f || len(res.Instances) != 2 {
		t.Fatalf("result = %+v", res)
	}
	for r, inst := range res.Instances {
		if inst.MachineID != fm[r] || *inst.ClusterRank != r || *inst.ClusterID != c.ID || !inst.System || inst.UserID != e.systemID ||
			inst.ExpiresAt != nil || inst.State != rental.StateCreating {
			t.Fatalf("instance %d = %+v", r, inst)
		}
		lower, upper, status := e.lease(inst.ID)
		if status != "active" || upper.Sub(lower) != rental.SystemLeaseHorizon {
			t.Fatalf("lease = %v %v %s", lower, upper, status)
		}
		if e.machineState(inst.MachineID) != fleet.StateAllocated {
			t.Fatal("machine not allocated")
		}
		p := e.createPayload(inst)
		if p.Cluster == nil || p.Cluster.Rank != r || p.Cluster.ClusterID != c.ID || len(p.Cluster.Members) != 2 || p.SSHPort != testSSHBasePort {
			t.Fatalf("payload = %+v", p)
		}
	}
	if e.count(`SELECT count(*) FROM orders`) != 0 {
		t.Fatal("system cluster must not create orders")
	}
	// 系统集群不出现在用户视图里
	if _, err := e.svc.ClusterForUser(e.ctx, e.systemID, c.ID); codeOf(err) != "cluster_not_found" {
		t.Fatalf("ClusterForUser(system) = %v", err)
	}
	if v, err := e.svc.GetClusterView(e.ctx, c.ID); err != nil || !v.System || len(v.Members) != 2 {
		t.Fatalf("GetClusterView = %+v, %v", v, err)
	}
}

func TestReleaseSystemCluster(t *testing.T) {
	e := newEnv(t, 0)
	f := e.fabric("A", "lan", "routed", false)
	pm := e.lanMachines(f, "192.168.1", 2)
	pool := e.toPool(pm...)
	um := e.lanMachines(f, "192.168.1", 2)
	sys, err := e.svc.PlaceSystemCluster(e.ctx, rental.SystemClusterRequest{MachineIDs: pm, GroupID: pool, ImageID: e.imageID, Note: "n"})
	if err != nil {
		t.Fatal(err)
	}
	user := e.placeCluster(2)
	assertClusterShape(t, user, 2, um)

	if err := e.svc.ReleaseSystemCluster(e.ctx, user.Cluster.ID); codeOf(err) != "invalid_state" {
		t.Fatalf("user cluster = %v", err)
	}
	if err := e.svc.ReleaseSystemCluster(e.ctx, "clu-nonexisten"); codeOf(err) != "cluster_not_found" {
		t.Fatalf("missing = %v", err)
	}
	if err := e.svc.ReleaseSystemCluster(e.ctx, sys.Cluster.ID); err != nil {
		t.Fatal(err)
	}
	for _, inst := range sys.Instances {
		if e.get(inst.ID).State != rental.StateWiping || countPending(e, inst.MachineID, fleet.CmdWipeInstance) != 1 ||
			countPending(e, inst.MachineID, fleet.CmdCreateInstance) != 0 {
			t.Fatalf("member %s not wiping", inst.ID)
		}
	}
	// 已无在位成员 → nil，不重复下发
	if err := e.svc.ReleaseSystemCluster(e.ctx, sys.Cluster.ID); err != nil {
		t.Fatal(err)
	}
	for _, inst := range sys.Instances {
		if countPending(e, inst.MachineID, fleet.CmdWipeInstance) != 1 {
			t.Fatal("second release must be a no-op")
		}
	}
	for _, inst := range user.Instances {
		if e.get(inst.ID).State != rental.StateCreating {
			t.Fatal("user cluster untouched")
		}
	}
}

// 端到端口径：机器的就绪列由 fleet（CreateFabric / UpdateMachine / RecordHello）算出，rental 照样能组集群、Spec 的 host_ip 与 lan_ip 一致。
func TestClusterPlaceWithFleetReadiness(t *testing.T) {
	e := newEnv(t, 0)
	fab, err := e.fleet.CreateFabric(e.ctx, audit.Actor{AdminID: 1}, fleet.FabricInput{Name: "lab", Kind: "lan", Isolation: "routed"})
	if err != nil {
		t.Fatal(err)
	}
	var ms []int64
	for k := 0; k < 2; k++ {
		ip := fmt.Sprintf("192.168.9.%d", 20+k)
		id := e.newMachine(fmt.Sprintf("FLT-%d", k))
		if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET lan_ip = $2 WHERE id = $1`, id, ip); err != nil {
			t.Fatal(err)
		}
		if _, err := e.fleet.UpdateMachine(e.ctx, audit.Actor{AdminID: 1}, id, fleet.MachineUpdate{SetFabric: true, FabricID: &fab.ID}); err != nil {
			t.Fatal(err)
		}
		n := lanNetInfo(ip)
		n.LanIface = "" // Hello 在 AgentConfig 之前：平台按 lan_ip 认管理网卡
		if err := e.fleet.RecordHello(e.ctx, id, 3, &n); err != nil {
			t.Fatal(err)
		}
		ms = append(ms, id)
	}
	res := e.placeCluster(2)
	assertClusterShape(t, res, 2, ms)
	for _, inst := range res.Instances {
		for _, m := range e.createPayload(inst).Cluster.Members {
			if m.HostIP != e.lanIP(byRank(t, res.Instances)[m.Rank].MachineID) {
				t.Fatalf("member = %+v", m)
			}
		}
	}
	// 旧 Agent（能力位 0）重连 → 不就绪 → 不再入选
	if err := e.fleet.RecordHello(e.ctx, ms[0], 0, nil); err != nil {
		t.Fatal(err)
	}
	var ready bool
	if err := e.pool.QueryRow(e.ctx, `SELECT cluster_ready FROM machines WHERE id = $1`, ms[0]).Scan(&ready); err != nil || ready {
		t.Fatalf("old agent ready = %v, %v", ready, err)
	}
}
