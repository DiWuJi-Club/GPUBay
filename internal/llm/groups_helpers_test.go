package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/rental"
)

// 计划 13：fake rental / appdeploy 的集群方法（落库，满足 llm.ClusterRentalOps / llm.ClusterAppOps）与建组用的辅助函数。
// 真实实现在 rental（Task 6）/ appdeploy（Task 7）；这里只模拟 llm 依赖的可观察行为。

var (
	_ llm.ClusterRentalOps = (*fakeRental)(nil)
	_ llm.ClusterAppOps    = (*fakeApps)(nil)
)

// PlaceSystemCluster：按机器 ID 升序锁机器并复核（空闲在线、在池内、cluster_ready、同 fabric 同 cluster_net）→ 插系统集群
// （最小空闲 subnet_index）→ 每台一个系统实例（cluster_id / rank）+ active lease + 机器 allocated。任一台不满足 → ErrMachineUnavailable。
func (f *fakeRental) PlaceSystemCluster(ctx context.Context, req rental.SystemClusterRequest) (rental.SystemClusterResult, error) {
	f.mu.Lock()
	f.clusterPlaced = append(f.clusterPlaced, req)
	perr, after := f.placeClusterErr, f.afterPlaceCluster
	if fn := f.placeClusterErrFn; perr == nil && fn != nil {
		perr = fn(len(f.clusterPlaced))
	}
	f.mu.Unlock()
	if perr != nil {
		return rental.SystemClusterResult{}, perr
	}
	if !slices.Contains(rental.ClusterSizes, len(req.MachineIDs)) {
		return rental.SystemClusterResult{}, apperr.ErrClusterSizeInvalid
	}
	ids := slices.Sorted(slices.Values(req.MachineIDs))
	now := f.e.clk.Now()
	cid := randx.ID("clu-", 10)
	var instIDs []string
	err := db.WithTx(ctx, f.e.pool, func(tx pgx.Tx) error {
		var fabric int64
		var net string
		for i, mid := range ids {
			var ok bool
			var fid *int64
			var cnet string
			if err := tx.QueryRow(ctx, `SELECT state = 'idle' AND agent_online AND group_id = $2 AND cluster_ready
				AND NOT EXISTS (SELECT 1 FROM instances WHERE machine_id = $1 AND state IN ('creating', 'running', 'stopped', 'wiping'))
				AND NOT EXISTS (SELECT 1 FROM machine_leases WHERE machine_id = $1 AND status IN ('reserved', 'active') AND upper(period) > $3),
				fabric_id, cluster_net
				FROM machines WHERE id = $1 FOR NO KEY UPDATE`, mid, req.GroupID, now).Scan(&ok, &fid, &cnet); err != nil {
				return err
			}
			if !ok || fid == nil || (i > 0 && (*fid != fabric || cnet != net)) {
				return apperr.ErrMachineUnavailable
			}
			fabric, net = *fid, cnet
		}
		if _, err := tx.Exec(ctx, `INSERT INTO instance_clusters (id, user_id, system, size, fabric_id, mode, rdma, mtu, subnet_index, state,
			created_at, updated_at)
			SELECT $1, $2, true, $3, f.id, CASE WHEN f.kind = 'lan' THEN 'routed' ELSE 'macvlan' END, false, f.mtu,
				(SELECT min(g) FROM generate_series(0, 255) g WHERE g NOT IN (SELECT subnet_index FROM instance_clusters WHERE state = 'active')),
				'active', $5, $5
			FROM cluster_fabrics f WHERE f.id = $4`, cid, f.e.systemUserID, len(ids), fabric, now); err != nil {
			return err
		}
		for rank, mid := range ids {
			id := randx.ID("ins-", 8)
			instIDs = append(instIDs, id)
			if _, err := tx.Exec(ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, password_auth,
				ssh_host_pubkey, group_id, system, cluster_id, cluster_rank, created_at, updated_at)
				SELECT $1, $2, m.id, m.product_id, $3, 'creating', '\x00', false, 'ssh-ed25519 AAAA', $4, true, $7, $8, $5, $5
				FROM machines m WHERE m.id = $6`, id, f.e.systemUserID, req.ImageID, req.GroupID, now, mid, cid, rank); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
				VALUES ($1, $2, tstzrange($3, $4), 'active', $3, $3)`, mid, id, now, now.Add(rental.SystemLeaseHorizon)); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE machines SET state = 'allocated' WHERE id = $1`, mid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return rental.SystemClusterResult{}, err
	}
	res := rental.SystemClusterResult{Cluster: rental.Cluster{ID: cid, UserID: f.e.systemUserID, System: true, Size: len(ids),
		State: rental.ClusterActive, CreatedAt: now, UpdatedAt: now}}
	for _, id := range instIDs {
		inst, err := f.Get(ctx, id)
		if err != nil {
			return rental.SystemClusterResult{}, err
		}
		res.Instances = append(res.Instances, inst)
	}
	if after != nil {
		after(cid)
	}
	return res, nil
}

// ReleaseSystemCluster：在位成员（creating / running / stopped）→ wiping；集群不存在 → ErrClusterNotFound，非系统集群 → ErrInvalidState。
func (f *fakeRental) ReleaseSystemCluster(ctx context.Context, clusterID string) error {
	f.mu.Lock()
	f.clustersReleased = append(f.clustersReleased, clusterID)
	rerr := f.releaseClusterErr
	f.mu.Unlock()
	if rerr != nil {
		return rerr
	}
	var system bool
	err := f.e.pool.QueryRow(ctx, `SELECT system FROM instance_clusters WHERE id = $1`, clusterID).Scan(&system)
	if db.IsNoRows(err) {
		return apperr.ErrClusterNotFound
	}
	if err != nil {
		return err
	}
	if !system {
		return apperr.ErrInvalidState
	}
	_, err = f.e.pool.Exec(ctx, `UPDATE instances SET state = 'wiping' WHERE cluster_id = $1 AND state IN ('creating', 'running', 'stopped')`, clusterID)
	return err
}

func (f *fakeRental) clusterPlaceCalls() []rental.SystemClusterRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rental.SystemClusterRequest(nil), f.clusterPlaced...)
}

func (f *fakeRental) releasedClusters() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.clustersReleased...)
}

// CreateCluster：集群已有 active 集群部署 → ErrAppClusterExists；否则插 app_cluster_deployments + 每个在位成员一条 queued 部署
// （rank0 head、其余 worker）。
func (f *fakeApps) CreateCluster(ctx context.Context, req appdeploy.ClusterCreateRequest) (appdeploy.ClusterDeployment, error) {
	f.mu.Lock()
	f.clusterCreates = append(f.clusterCreates, req)
	f.mu.Unlock()
	ver := f.e.versionID
	if req.VersionID != nil {
		ver = *req.VersionID
	}
	id := randx.ID("acd-", 12)
	now := f.e.clk.Now()
	envJSON, _ := json.Marshal(req.Env)
	err := db.WithTx(ctx, f.e.pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_cluster_deployments WHERE cluster_id = $1 AND state = 'active')`,
			req.ClusterID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return apperr.ErrAppClusterExists
		}
		if _, err := tx.Exec(ctx, `INSERT INTO app_cluster_deployments (id, cluster_id, user_id, app_id, app_version_id, state, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'active', $6, $6)`, id, req.ClusterID, req.UserID, req.AppID, ver, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, env, port, spec_hash,
			next_poll_at, created_at, updated_at, cluster_deployment_id, cluster_role)
			SELECT 'dep-' || substr(md5(random()::text || i.id), 1, 12), i.id, $2, $3, $4, 'queued', $5, 8000, 'x', $6, $6, $6, $1,
				CASE WHEN i.cluster_rank = 0 THEN 'head' ELSE 'worker' END
			FROM instances i WHERE i.cluster_id = $7 AND i.state IN ('creating', 'running', 'stopped')`,
			id, req.UserID, req.AppID, ver, envJSON, now, req.ClusterID)
		return err
	})
	if err != nil {
		return appdeploy.ClusterDeployment{}, err
	}
	return f.GetClusterDeployment(ctx, id)
}

// GetClusterDeployment：行 + 成员部署（按 rank）+ 简化的聚合（appdeploy 规格 §6.4 的顺序）。
func (f *fakeApps) GetClusterDeployment(ctx context.Context, id string) (appdeploy.ClusterDeployment, error) {
	var cd appdeploy.ClusterDeployment
	err := f.e.pool.QueryRow(ctx, `SELECT id, cluster_id, user_id, app_id, app_version_id, state, created_at, updated_at
		FROM app_cluster_deployments WHERE id = $1`, id).Scan(&cd.ID, &cd.ClusterID, &cd.UserID, &cd.AppID, &cd.AppVersionID, &cd.State,
		&cd.CreatedAt, &cd.UpdatedAt)
	if db.IsNoRows(err) {
		return cd, apperr.ErrNotFound
	}
	if err != nil {
		return cd, err
	}
	rows, err := f.e.pool.Query(ctx, `SELECT d.id, i.cluster_rank, d.cluster_role, d.instance_id,
		i.cluster_id IS NOT DISTINCT FROM $2 AND i.state IN ('creating', 'running', 'stopped')
		FROM app_deployments d JOIN instances i ON i.id = d.instance_id
		WHERE d.cluster_deployment_id = $1 ORDER BY i.cluster_rank, d.created_at, d.id`, id, cd.ClusterID)
	if err != nil {
		return cd, err
	}
	type mrow struct {
		dep, role, inst string
		rank            int
		current         bool
	}
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (mrow, error) {
		var m mrow
		err := r.Scan(&m.dep, &m.rank, &m.role, &m.inst, &m.current)
		return m, err
	})
	if err != nil {
		return cd, err
	}
	for _, m := range ms {
		d, err := f.Get(ctx, m.dep)
		if err != nil {
			return cd, err
		}
		cd.Members = append(cd.Members, appdeploy.ClusterMemberDeployment{Rank: m.rank, Role: m.role, InstanceID: m.inst, Deployment: d,
			Current: m.current})
	}
	cd.Status, cd.Health, cd.Phase, cd.Progress = fakeAggregate(cd)
	return cd, nil
}

func fakeAggregate(cd appdeploy.ClusterDeployment) (status, health, phase string, progress int) {
	if cd.State == appdeploy.ClusterDepRemoved {
		return appdeploy.ClusterStatusRemoved, appdeploy.HealthUnknown, "", 0
	}
	count := func(pred func(appdeploy.ClusterMemberDeployment) bool) int {
		n := 0
		for _, m := range cd.Members {
			if pred(m) {
				n++
			}
		}
		return n
	}
	all := len(cd.Members)
	progress = 100
	for _, m := range cd.Members {
		progress = min(progress, m.Deployment.Progress)
		if m.Rank == 0 {
			phase = m.Deployment.Phase
		}
	}
	health = appdeploy.HealthUnknown
	switch {
	case all > 0 && count(func(m appdeploy.ClusterMemberDeployment) bool { return m.Deployment.Health == appdeploy.HealthOK }) == all:
		health = appdeploy.HealthOK
	case count(func(m appdeploy.ClusterMemberDeployment) bool {
		return m.Deployment.Health == appdeploy.HealthUnhealthy
	}) > 0:
		health = appdeploy.HealthUnhealthy
	}
	st := func(s ...string) func(appdeploy.ClusterMemberDeployment) bool {
		return func(m appdeploy.ClusterMemberDeployment) bool { return slices.Contains(s, m.Deployment.State) }
	}
	switch {
	case count(st(appdeploy.StateFailed)) > 0:
		status = appdeploy.ClusterStatusFailed
	case count(st(appdeploy.StateRemoving)) > 0:
		status = appdeploy.ClusterStatusRemoving
	case all > 0 && count(st(appdeploy.StateRemoved, appdeploy.StateLost)) == all:
		status = appdeploy.ClusterStatusRemoved
	case all > 0 && count(st(appdeploy.StateRunning)) == all && health == appdeploy.HealthOK:
		status = appdeploy.ClusterStatusRunning
	case all > 0 && count(st(appdeploy.StateStopped)) == all:
		status = appdeploy.ClusterStatusStopped
	case count(func(m appdeploy.ClusterMemberDeployment) bool { return !m.Current }) > 0:
		status = appdeploy.ClusterStatusStale
	default:
		status = appdeploy.ClusterStatusInstalling
	}
	return status, health, phase, progress
}

func (f *fakeApps) LiveClusterDeployment(ctx context.Context, clusterID string) (*appdeploy.ClusterDeployment, error) {
	var id string
	err := f.e.pool.QueryRow(ctx, `SELECT id FROM app_cluster_deployments WHERE cluster_id = $1 AND state = 'active'`, clusterID).Scan(&id)
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cd, err := f.GetClusterDeployment(ctx, id)
	return &cd, err
}

// RedeployCluster：成员部署 running / stopped / failed → queued（其余不动）。
func (f *fakeApps) RedeployCluster(ctx context.Context, req appdeploy.ClusterRedeployRequest) (appdeploy.ClusterDeployment, error) {
	f.mu.Lock()
	f.clusterRedeploys = append(f.clusterRedeploys, req)
	rerr := f.clusterRedeployErr
	f.mu.Unlock()
	if rerr != nil {
		return appdeploy.ClusterDeployment{}, rerr
	}
	if _, err := f.e.pool.Exec(ctx, `UPDATE app_deployments SET state = 'queued', health = 'unknown'
		WHERE cluster_deployment_id = $1 AND state IN ('running', 'stopped', 'failed')`, req.ClusterDeploymentID); err != nil {
		return appdeploy.ClusterDeployment{}, err
	}
	return f.GetClusterDeployment(ctx, req.ClusterDeploymentID)
}

func (f *fakeApps) clusterCreateCalls() []appdeploy.ClusterCreateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]appdeploy.ClusterCreateRequest(nil), f.clusterCreates...)
}

func (f *fakeApps) clusterRedeployCalls() []appdeploy.ClusterRedeployRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]appdeploy.ClusterRedeployRequest(nil), f.clusterRedeploys...)
}

// ---- 建组的辅助 ----

var fabricSeq atomic.Int64

// fabric 插一个集群网络（isolation = vlan 时 VLAN 范围 100～199）。
func (e *env) fabric(kind, isolation string) int64 {
	e.t.Helper()
	var vmin, vmax any
	if isolation == "vlan" {
		vmin, vmax = 100, 199
	}
	return e.queryInt(`INSERT INTO cluster_fabrics (name, kind, isolation, vlan_min, vlan_max, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6) RETURNING id`, fmt.Sprintf("fab-%d", fabricSeq.Add(1)), kind, isolation, vmin, vmax, e.clk.Now())
}

// fabricMachine 插一台池机器并挂到 fabric：集群就绪、Agent 支持集群（features = SharedSlots | Cluster）、cluster_net = net。
func (e *env) fabricMachine(serial string, fabricID int64, net string) int64 {
	e.t.Helper()
	id := e.poolMachine(serial)
	e.exec(`UPDATE machines SET fabric_id = $2, cluster_ready = true, agent_features = 3, cluster_net = $3 WHERE id = $1`, id, fabricID, net)
	return id
}

// clusterVersion 给 vllm-node 插一个支持 2～maxSize 台集群的已发布版本（仓库 spec，cluster.max_size = maxSize、timeout_minutes = timeout）。
func (e *env) clusterVersion(version string, maxSize, timeout int) int64 {
	e.t.Helper()
	spec := vllmNodeSpec(e.t)
	spec.Cluster = &appdeploy.ClusterSupport{Mode: appdeploy.ClusterModeOptional, MinSize: 2, MaxSize: maxSize}
	spec.TimeoutMinutes = timeout
	if err := appdeploy.ValidateSpec(&spec); err != nil {
		e.t.Fatalf("cluster spec: %v", err)
	}
	return e.addNodeVersionSpec(version, appdeploy.StatusPublished, spec, e.imageID)
}

// plainVersion 给 vllm-node 插一个不支持集群的已发布版本（仓库 spec 去掉 cluster 段，模拟 1.0.0）。
func (e *env) plainVersion(version string) int64 {
	e.t.Helper()
	spec := vllmNodeSpec(e.t)
	spec.Cluster = nil
	return e.addNodeVersionSpec(version, appdeploy.StatusPublished, spec, e.imageID)
}

// groupModel 建一个 nodes_per_replica = n、钉住 versionID、replicas 个副本的在线模型。
func (e *env) groupModel(slug string, n, replicas int, versionID int64, mods ...func(*llm.ModelInput)) llm.Model {
	e.t.Helper()
	return e.onlineModel(slug, replicas, append([]func(*llm.ModelInput){func(in *llm.ModelInput) {
		in.NodesPerReplica, in.AppVersionID = n, &versionID
	}}, mods...)...)
}

// members 节点的全部成员行（按 rank）。
func (e *env) members(nodeID string) []llm.NodeMember {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT node_id, rank, machine_id, instance_id, live FROM llm_node_members WHERE node_id = $1 ORDER BY rank`, nodeID)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (llm.NodeMember, error) {
		var m llm.NodeMember
		err := r.Scan(&m.NodeID, &m.Rank, &m.MachineID, &m.InstanceID, &m.Live)
		return m, err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// setClusterMembers 把集群部署的全部成员部署置为 state / health。
func (e *env) setClusterMembers(acdID, state, health string) {
	e.t.Helper()
	e.exec(`UPDATE app_deployments SET state = $2, health = $3 WHERE cluster_deployment_id = $1`, acdID, state, health)
}

// memberDeployment 某成员实例在集群部署里的成员部署 ID。
func (e *env) memberDeployment(acdID, instanceID string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM app_deployments WHERE cluster_deployment_id = $1 AND instance_id = $2`,
		acdID, instanceID).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// bringGroupReady 让组节点走完 provisioning：全员 running → CreateCluster → 成员部署 running/ok → 探测 head 成功 → ready。
func (ne *nodeEnv) bringGroupReady(nodeID string) llm.Node {
	ne.t.Helper()
	for _, m := range ne.members(nodeID) {
		ne.setInstance(m.InstanceID, rental.StateRunning)
	}
	ne.tick()
	n := ne.node(nodeID)
	if n.AppClusterID == nil {
		ne.t.Fatalf("group %s has no cluster deployment after tick (error %q)", nodeID, n.Error)
	}
	ne.setClusterMembers(*n.AppClusterID, appdeploy.StateRunning, appdeploy.HealthOK)
	ne.probe.set(nil)
	ne.tick()
	if n = ne.node(nodeID); n.State != llm.NodeReady {
		ne.t.Fatalf("group %s state %s (error %q)", nodeID, n.State, n.Error)
	}
	return n
}

// readyGroup 建 fabric（lan）+ size 台机器 + N = size 的模型（1 副本）并带到 ready；返回模型与节点。
func (ne *nodeEnv) readyGroup(size int) (llm.Model, llm.Node) {
	ne.t.Helper()
	f := ne.fabric("lan", "routed")
	for i := 0; i < size; i++ {
		ne.fabricMachine(fmt.Sprintf("G%d", i), f, "192.168.1.0/24")
	}
	m := ne.groupModel("qwen3-235b", size, 1, ne.clusterVersion("1.1.0", 4, 360))
	ne.tick()
	nodes := ne.nodesOf(m.ID)
	if len(nodes) != 1 || nodes[0].Size != size {
		ne.t.Fatalf("nodes %+v", nodes)
	}
	return m, ne.bringGroupReady(nodes[0].ID)
}

// suspend / resume 成员实例（模拟机器失联暂停）。
func (e *env) suspend(instanceID string) {
	e.t.Helper()
	e.exec(`UPDATE instances SET suspended_at = $2 WHERE id = $1`, instanceID, e.clk.Now())
}

func (e *env) since(d time.Duration) time.Time { return e.clk.Now().Add(-d) }
