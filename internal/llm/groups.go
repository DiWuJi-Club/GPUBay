package llm

// 计划 13：多机节点组（规格 §7）。一个副本 = 同一 fabric、同一 cluster_net 的 N 台池机器（rental 系统集群），
// 集群部署 vllm-node（rank0 = Ray head + vLLM，其余 = worker）。llm_nodes 一行代表整组：machine_id / instance_id / deployment_id
// 指向 rank0，cluster_id / app_cluster_id 指向系统集群与集群部署；成员（含 rank0）在 llm_node_members。
//
// 故障规则（M12）：任一成员实例没了 / 非 running / 机器 faulty → 整组立即 failed 并释放全部成员（下一轮在别处另建）；
// 成员机器失联（实例暂停）→ 整组 unhealthy，GroupSuspendFailAfter 内未恢复 → failed；成员进程崩溃（集群部署 failed / 不健康）
// → unhealthy，GroupRebuildAfter 后整组 RedeployCluster（rebuild_count + 1，MaxRebuilds 次 → failed + 严重告警）。
// 与单机节点一样：对 rental / appdeploy 的调用都在事务外，节点回写只锁 llm_nodes 一行。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/rental"
)

// errClusterOpsMissing：Deps 没有装配 ClusterRental / ClusterApps（多机节点组不可用）。
var errClusterOpsMissing = errors.New("llm: cluster rental / app ops not configured")

// nodeMembers 组节点的全部成员行（含已 live = false 的），按 rank。
func (s *Service) nodeMembers(ctx context.Context, q db.Querier, nodeID string) ([]NodeMember, error) {
	rows, err := q.Query(ctx, `SELECT node_id, rank, machine_id, instance_id, live FROM llm_node_members WHERE node_id = $1 ORDER BY rank`, nodeID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (NodeMember, error) {
		var m NodeMember
		err := r.Scan(&m.NodeID, &m.Rank, &m.MachineID, &m.InstanceID, &m.Live)
		return m, err
	})
}

// liveMembers 组节点的 live 成员；数量须等于 size（rank 0..size-1，由主键与不变量 ⑧ 保证），否则 complete = false。
func (s *Service) liveMembers(ctx context.Context, n Node) (members []NodeMember, complete bool, err error) {
	all, err := s.nodeMembers(ctx, s.d.Pool, n.ID)
	if err != nil {
		return nil, false, err
	}
	for _, m := range all {
		if m.Live {
			members = append(members, m)
		}
	}
	return members, len(members) == n.Size, nil
}

// releaseCluster ReleaseSystemCluster（整组）；已无在位成员（nil）、集群不存在 / 不是系统集群都视为完成。
func (s *Service) releaseCluster(ctx context.Context, clusterID string) error {
	if s.d.ClusterRental == nil {
		return errClusterOpsMissing
	}
	err := s.d.ClusterRental.ReleaseSystemCluster(ctx, clusterID)
	if errors.Is(err, apperr.ErrInvalidState) || errors.Is(err, apperr.ErrNotFound) || errors.Is(err, apperr.ErrClusterNotFound) {
		return nil
	}
	return err
}

// createGroupNode 在指定的同 fabric 池机器上建组节点（规格 §7.3 第 2 步，事务外调用）：生成节点 ID 与密钥 →
// rental.PlaceSystemCluster（锁机器并复核，任一台不满足 → ErrMachineUnavailable，整组回滚）→ 一个事务插 llm_nodes
// （size N、rank0 的机器 / 实例、cluster_id）+ N 行 llm_node_members + 事件。插入失败（含唯一索引撞车）→ ReleaseSystemCluster。
func (s *Service) createGroupNode(ctx context.Context, m Model, machineIDs []int64, poolID int64, detail string) (Node, error) {
	if s.d.ClusterRental == nil {
		return Node{}, errClusterOpsMissing
	}
	id := newNodeID()
	_, enc, err := s.newNodeKey()
	if err != nil {
		return Node{}, err
	}
	res, err := s.d.ClusterRental.PlaceSystemCluster(ctx, rental.SystemClusterRequest{MachineIDs: machineIDs, GroupID: poolID,
		ImageID: m.ImageID, Note: "llm node " + id + " " + m.Slug})
	if err != nil {
		return Node{}, err
	}
	var out Node
	err = db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if len(res.Instances) != len(machineIDs) || len(res.Instances) < 2 {
			return fmt.Errorf("llm: PlaceSystemCluster returned %d instances for %d machines", len(res.Instances), len(machineIDs))
		}
		now := s.d.Clock.Now()
		head := res.Instances[0]
		n, err := scanNode(tx.QueryRow(ctx, `INSERT INTO llm_nodes (id, model_id, machine_id, instance_id, state, api_key_enc, size, cluster_id,
			created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9) RETURNING `+nodeCols,
			id, m.ID, head.MachineID, head.ID, NodeProvisioning, enc, len(res.Instances), res.Cluster.ID, now))
		if err != nil {
			return err
		}
		for rank, inst := range res.Instances {
			if _, err := tx.Exec(ctx, `INSERT INTO llm_node_members (node_id, rank, machine_id, instance_id) VALUES ($1, $2, $3, $4)`,
				id, rank, inst.MachineID, inst.ID); err != nil {
				return err
			}
		}
		out = n
		return insertNodeEvent(ctx, tx, id, "", NodeProvisioning, detail, now)
	})
	if err != nil {
		if rerr := s.releaseCluster(ctx, res.Cluster.ID); rerr != nil {
			s.d.Log.Warn("llm: release system cluster after group node insert failure", "cluster", res.Cluster.ID, "err", rerr)
		}
		if db.IsUniqueViolation(err) {
			return Node{}, apperr.ErrMachineUnavailable
		}
		return Node{}, err
	}
	return out, nil
}

// headDeploymentID 集群部署里 rank0（head）的成员部署 ID；没有 → nil。
func headDeploymentID(cd appdeploy.ClusterDeployment) *string {
	for _, m := range cd.Members {
		if m.Rank == 0 && m.Current {
			id := m.Deployment.ID
			return &id
		}
	}
	for _, m := range cd.Members {
		if m.Role == appdeploy.RoleHead {
			id := m.Deployment.ID
			return &id
		}
	}
	return nil
}

// clusterDeploymentError 集群部署失败的可展示原因：第一个失败成员的部署错误（带 rank），否则聚合状态。
func clusterDeploymentError(cd appdeploy.ClusterDeployment) string {
	for _, m := range cd.Members {
		if m.Deployment.State == appdeploy.StateFailed {
			return clipRunes("rank "+strconv.Itoa(m.Rank)+"："+deploymentError(m.Deployment), maxNodeError)
		}
	}
	return "集群部署" + cd.Status
}

// clusterDeploymentGone 集群部署已无法恢复（卸载中 / 已卸载 / 成员已换，需要重新部署）。
func clusterDeploymentGone(status string) bool {
	return status == appdeploy.ClusterStatusRemoving || status == appdeploy.ClusterStatusRemoved || status == appdeploy.ClusterStatusStale
}

// provisionTimeout 节点的建节点超时（ProvisionTimeoutFor）：版本取部署所用的版本（versionID 非 0），否则模型钉住的 / 最新已发布的
// vllm-node 版本；读不到版本时退回 ProvisionTimeout。
func (s *Service) provisionTimeout(ctx context.Context, m Model, versionID int64) time.Duration {
	var v appdeploy.Version
	var err error
	if versionID != 0 {
		v, err = s.d.AppCat.GetVersion(ctx, versionID)
	} else {
		v, err = s.nodeVersion(ctx, s.d.Pool, m.AppVersionID)
	}
	if err != nil {
		s.d.Log.Warn("llm: read vllm-node version for provision timeout; using default", "model", m.Slug, "err", err)
		return ProvisionTimeout
	}
	return ProvisionTimeoutFor(v)
}

func provisionTimeoutText(d time.Duration) string {
	return fmt.Sprintf("建节点超时（超过 %d 分钟）", int(d/time.Minute))
}

// ---- Controller：补组 ----

// provisionGroups 给 N > 1 的模型补组（规格 §7.3 第 1 步）：每补一组重新列候选 (fabric, cluster_net)（最贴合在前），
// 取其中 ID 最小的 N 台 → createGroupNode；ErrMachineUnavailable → 换下一个候选。budget 按组计。
// 返回 short = 候选用完仍有缺额（调用方告警 llm_pool_short）。
func (c *Controller) provisionGroups(ctx context.Context, now time.Time, d deficit, poolID int64, budget *int, rep *TickReport,
	note func(string, error)) (short bool) {
	size := d.m.NodesPerReplica
	if c.s.d.ClusterRental == nil {
		note("provision groups for "+d.m.Slug, errClusterOpsMissing)
		return false
	}
	for k := 0; k < d.n && *budget > 0; k++ {
		cands, err := c.s.candidateFabrics(ctx, poolID, now, size)
		if err != nil {
			note("candidate fabrics", err)
			return false
		}
		placed := false
		for _, fc := range cands {
			_, err := c.s.createGroupNode(ctx, d.m, fc.MachineIDs[:size], poolID, fmt.Sprintf("补副本：模型 %s（%d 台节点组）", d.m.Slug, size))
			if errors.Is(err, apperr.ErrMachineUnavailable) {
				continue
			}
			if err != nil { // 与机器无关的失败：本模型本轮不再补
				note("create group node for "+d.m.Slug, err)
				return false
			}
			placed = true
			rep.Provisioned++
			*budget--
			break
		}
		if !placed {
			return true
		}
	}
	return false
}

// ---- Controller：推进 provisioning（组）----

// advanceGroup 规格 §7.3 第 3 步：任一成员实例不存在 / 已结束 → failed（不可重建）；集群部署 failed → failProvisioning（可重建），
// 卸载 / 换成员 → 不可重建；超时（ProvisionTimeoutFor）→ 可重建；有成员非 running → 等；全员 running 且没有集群部署 → CreateCluster；
// 集群部署 running ∧ ok → tryPromote（探测 head）。
func (c *Controller) advanceGroup(ctx context.Context, now time.Time, cn ctlNode, m Model, rep *TickReport) error {
	n := cn.Node
	if c.s.d.ClusterApps == nil {
		return errClusterOpsMissing
	}
	members, complete, err := c.s.liveMembers(ctx, n)
	if err != nil {
		return err
	}
	if !complete {
		return c.failProvisioning(ctx, n, "节点组成员不完整", false, rep)
	}
	allRunning := true
	for _, mb := range members {
		inst, err := c.s.d.Rental.Get(ctx, mb.InstanceID)
		if errors.Is(err, apperr.ErrNotFound) {
			return c.failProvisioning(ctx, n, memberText(mb, "实例不存在"), false, rep)
		}
		if err != nil {
			return err
		}
		if instanceGone(inst.State) {
			return c.failProvisioning(ctx, n, memberText(mb, instanceGoneText(inst.State)), false, rep)
		}
		if inst.State != rental.StateRunning {
			allRunning = false
		}
	}
	var cd *appdeploy.ClusterDeployment
	if n.AppClusterID != nil {
		d, err := c.s.d.ClusterApps.GetClusterDeployment(ctx, *n.AppClusterID)
		if err != nil {
			return err
		}
		cd = &d
		switch {
		case d.Status == appdeploy.ClusterStatusFailed:
			return c.failProvisioning(ctx, n, clusterDeploymentError(d), true, rep)
		case clusterDeploymentGone(d.Status):
			return c.failProvisioning(ctx, n, "集群部署已失效（"+d.Status+"）", false, rep)
		}
	}
	var verID int64
	if cd != nil {
		verID = cd.AppVersionID
	}
	if timeout := c.s.provisionTimeout(ctx, m, verID); now.Sub(cn.since) > timeout {
		return c.failProvisioning(ctx, n, provisionTimeoutText(timeout), true, rep)
	}
	if !allRunning {
		return nil // creating：等 Agent 建好全部成员的容器
	}
	if cd == nil {
		return c.createClusterDeployment(ctx, n, m, rep)
	}
	if cd.Status == appdeploy.ClusterStatusRunning && cd.Health == appdeploy.HealthOK {
		_, err := c.probe.tryPromote(ctx, n, cn.slug)
		return err
	}
	return nil
}

func memberText(mb NodeMember, what string) string {
	return "成员 rank " + strconv.Itoa(mb.Rank) + "：" + what
}

// createClusterDeployment 全员 running 且还没有集群部署 → CreateCluster(系统账号, 集群, vllm-node, NodeEnv)；
// app_cluster_exists（上一轮建成但回写失败）→ 认领集群上现有的 active 集群部署。回写 app_cluster_id 与 head 的成员部署。
func (c *Controller) createClusterDeployment(ctx context.Context, n Node, m Model, rep *TickReport) error {
	if n.ClusterID == nil {
		return c.failProvisioning(ctx, n, "节点组没有集群", false, rep)
	}
	app, err := c.s.d.AppCat.AppBySlug(ctx, NodeAppSlug)
	if err != nil {
		return err
	}
	if !app.Internal {
		return c.s.d.Fleet.RaiseAlertOnce(ctx, c.s.d.Pool, fleet.Alert{Level: "critical", Kind: AlertNodeFailed, Message: nodeAppNotInternal})
	}
	key, err := c.s.d.Box.Open(n.APIKeyEnc)
	if err != nil {
		return fmt.Errorf("open node key: %w", err)
	}
	cd, err := c.s.d.ClusterApps.CreateCluster(ctx, appdeploy.ClusterCreateRequest{UserID: c.s.d.SystemUserID, ClusterID: *n.ClusterID,
		AppID: app.ID, VersionID: m.AppVersionID, Env: NodeEnv(m, string(key)), IP: ""})
	if errors.Is(err, apperr.ErrAppClusterExists) {
		live, lerr := c.s.d.ClusterApps.LiveClusterDeployment(ctx, *n.ClusterID)
		if lerr != nil {
			return lerr
		}
		if live == nil || live.AppID != app.ID {
			return c.failProvisioning(ctx, n, "集群上已有其它集群部署，无法认领", false, rep)
		}
		cd, err = *live, nil
	}
	if err != nil {
		msg := "创建集群部署失败"
		if ae := apperr.As(err); ae != nil {
			msg += "：" + ae.Message
		}
		_, _, werr := c.s.mutateNode(ctx, n.ID, func(_ pgx.Tx, x *Node, _ time.Time) (string, error) {
			if err := expect(x, NodeProvisioning); err != nil {
				return "", err
			}
			x.Error = msg
			return "", nil
		})
		return errors.Join(err, werr)
	}
	_, _, err = c.s.mutateNode(ctx, n.ID, func(_ pgx.Tx, x *Node, _ time.Time) (string, error) {
		if err := expect(x, NodeProvisioning); err != nil || x.AppClusterID != nil {
			return "", errNodeStale
		}
		x.AppClusterID, x.DeploymentID, x.Error = &cd.ID, headDeploymentID(cd), ""
		return "", nil
	})
	return err
}

// ---- Controller：unhealthy（组）----

// handleUnhealthyGroup 规格 §7.3 第 4 步（M12）。
func (c *Controller) handleUnhealthyGroup(ctx context.Context, now time.Time, cn ctlNode, m Model, rep *TickReport) error {
	n := cn.Node
	if c.s.d.ClusterApps == nil {
		return errClusterOpsMissing
	}
	members, complete, err := c.s.liveMembers(ctx, n)
	if err != nil {
		return err
	}
	if !complete {
		return c.failNode(ctx, n, NodeUnhealthy, "节点组成员不完整", nil, rep)
	}
	suspended := false
	for _, mb := range members {
		inst, err := c.s.d.Rental.Get(ctx, mb.InstanceID)
		if errors.Is(err, apperr.ErrNotFound) {
			return c.failNode(ctx, n, NodeUnhealthy, memberText(mb, "实例不存在"), nil, rep)
		}
		if err != nil {
			return err
		}
		// 成员实例非 running（已结束、stopped、creating…）→ 整组立即 failed；running 但暂停（机器失联）走失联计时
		if inst.State != rental.StateRunning {
			return c.failNode(ctx, n, NodeUnhealthy, memberText(mb, instanceGoneText(inst.State)), nil, rep)
		}
		if inst.SuspendedAt != nil {
			suspended = true
		}
		mach, err := c.s.d.Fleet.Get(ctx, c.s.d.Pool, mb.MachineID)
		if err != nil {
			return err
		}
		if mach.State == fleet.StateFaulty {
			return c.failNode(ctx, n, NodeUnhealthy, memberText(mb, "机器故障"), nil, rep)
		}
	}
	if n.AppClusterID == nil {
		return c.failNode(ctx, n, NodeUnhealthy, "节点没有部署，无法重建", nil, rep)
	}
	cd, err := c.s.d.ClusterApps.GetClusterDeployment(ctx, *n.AppClusterID)
	if err != nil {
		return err
	}
	if clusterDeploymentGone(cd.Status) {
		return c.failNode(ctx, n, NodeUnhealthy, "集群部署已失效（"+cd.Status+"）", nil, rep)
	}
	if n.UnhealthySince == nil {
		return nil
	}
	if suspended { // 成员失联：Ray 集群已缺节点，重新部署也起不来；等恢复，超时整组 failed
		if !now.Before(n.UnhealthySince.Add(GroupSuspendFailAfter)) {
			return c.failNode(ctx, n, NodeUnhealthy, "成员机器失联", nil, rep)
		}
		return nil
	}
	if now.Before(n.UnhealthySince.Add(GroupRebuildAfter)) {
		return nil
	}
	critical := func(detail, what string) error {
		return c.failNode(ctx, n, NodeUnhealthy, detail, &fleet.Alert{Level: "critical", Kind: AlertNodeFailed,
			MachineID: n.MachineID, InstanceID: deref(n.InstanceID), Message: "节点 " + n.ID + "（" + cn.slug + "）" + what}, rep)
	}
	if n.RebuildCount >= MaxRebuilds {
		return critical("多次重建仍不健康", "多次重建失败")
	}
	if cd.Status == appdeploy.ClusterStatusInstalling {
		// 集群部署已在（重新）安装：不再 Redeploy，按 provisioning 等待（超时兜底）；规则同单机节点
		_, _, err := c.s.mutateNode(ctx, n.ID, func(_ pgx.Tx, x *Node, _ time.Time) (string, error) {
			if err := expect(x, NodeUnhealthy); err != nil {
				return "", err
			}
			if x.RebuildCount > 0 {
				x.RebuildCount++
			}
			x.State, x.ProbeFailCount, x.UnhealthySince = NodeProvisioning, 0, nil
			return "集群部署正在重新安装，等待就绪", nil
		})
		return err
	}
	nd, err := c.s.d.ClusterApps.RedeployCluster(ctx, appdeploy.ClusterRedeployRequest{UserID: c.s.d.SystemUserID,
		ClusterDeploymentID: *n.AppClusterID, VersionID: m.AppVersionID, EnvSet: false})
	if err != nil {
		if errors.Is(err, apperr.ErrAppInvalidState) { // 有成员部署正在安装 / 卸载：暂时性，下一轮再试（不计重建次数）
			return nil
		}
		if ae := apperr.As(err); ae != nil { // 其它业务拒绝：重试也不会好
			return critical(clipRunes("重建失败："+ae.Message, maxNodeError), "重建失败")
		}
		return fmt.Errorf("redeploy cluster: %w", err) // 暂时性错误：rebuild_count 不变，下一轮再试
	}
	_, _, err = c.s.mutateNode(ctx, n.ID, func(_ pgx.Tx, x *Node, _ time.Time) (string, error) {
		if err := expect(x, NodeUnhealthy); err != nil {
			return "", err
		}
		x.RebuildCount++
		x.State, x.ProbeFailCount, x.UnhealthySince = NodeProvisioning, 0, nil
		if head := headDeploymentID(nd); head != nil {
			x.DeploymentID = head
		}
		return fmt.Sprintf("持续不健康，整组自动重新部署（第 %d 次）", x.RebuildCount), nil
	})
	return err
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---- Controller：removing（组）----

// handleRemovingGroup 全部成员实例已结束（released / failed / cancelled / 不存在）→ removed；有成员仍占机器 → 重试
// ReleaseSystemCluster；有成员 wiping → 等洗机。
func (c *Controller) handleRemovingGroup(ctx context.Context, cn ctlNode, rep *TickReport) error {
	n := cn.Node
	members, err := c.s.nodeMembers(ctx, c.s.d.Pool, n.ID)
	if err != nil {
		return err
	}
	wiping := false
	for _, mb := range members {
		inst, err := c.s.d.Rental.Get(ctx, mb.InstanceID)
		switch {
		case errors.Is(err, apperr.ErrNotFound):
		case err != nil:
			return err
		case instanceLive(inst.State):
			return c.s.releaseNode(ctx, n)
		case inst.State == rental.StateWiping:
			wiping = true
		}
	}
	if wiping {
		return nil
	}
	_, ok, err := c.s.mutateNode(ctx, n.ID, func(_ pgx.Tx, x *Node, _ time.Time) (string, error) {
		if err := expect(x, NodeRemoving); err != nil {
			return "", err
		}
		x.State = NodeRemoved
		return "节点组机器已全部释放", nil
	})
	if ok {
		rep.Removed++
	}
	return err
}

// ---- Prober（组）----

// observeGroup 规格 §7.4：逐成员读实例——不存在 / 结束 → vFailNow；非 running 或已暂停 → vFail（provisioning 为 vSkip）；
// 再读集群部署聚合（failed / removed / stale → vFailNow，非 running / health 非 ok → vFail；provisioning 都是 vSkip）；最后探测 head。
func (p *Prober) observeGroup(ctx context.Context, n Node, slug string) observation {
	s := p.s
	prov := n.State == NodeProvisioning
	skipOr := func(v verdict, detail string) observation {
		if prov {
			return observation{vSkip, ""}
		}
		return observation{v, detail}
	}
	members, complete, err := s.liveMembers(ctx, n)
	if err != nil {
		s.d.Log.Warn("llm prober: read group members", "node", n.ID, "err", err)
		return observation{vSkip, ""}
	}
	if !complete || n.InstanceID == nil {
		return observation{vFailNow, "节点组成员不完整"}
	}
	for _, mb := range members {
		inst, err := s.d.Rental.Get(ctx, mb.InstanceID)
		if errors.Is(err, apperr.ErrNotFound) {
			return observation{vFailNow, memberText(mb, "实例不存在")}
		}
		if err != nil {
			s.d.Log.Warn("llm prober: read member instance", "node", n.ID, "instance", mb.InstanceID, "err", err)
			return observation{vSkip, ""}
		}
		switch {
		case instanceGone(inst.State):
			return observation{vFailNow, memberText(mb, "实例已结束（"+inst.State+"）")}
		case inst.State != rental.StateRunning:
			return skipOr(vFail, memberText(mb, "实例未运行（"+inst.State+"）"))
		case inst.SuspendedAt != nil:
			return skipOr(vFail, memberText(mb, "机器失联，实例已暂停"))
		}
	}
	if n.AppClusterID == nil {
		if prov || s.d.ClusterApps == nil {
			return observation{vSkip, ""}
		}
		return p.probeNode(ctx, n, slug) // 只有 e2e 钩子直接造的 ready 组节点没有集群部署
	}
	if s.d.ClusterApps == nil {
		return observation{vSkip, ""}
	}
	cd, err := s.d.ClusterApps.GetClusterDeployment(ctx, *n.AppClusterID)
	if err != nil {
		s.d.Log.Warn("llm prober: read cluster deployment", "node", n.ID, "err", err)
		return observation{vSkip, ""}
	}
	switch {
	case cd.Status == appdeploy.ClusterStatusFailed:
		return skipOr(vFailNow, clusterDeploymentError(cd)) // provisioning → failed 由 Controller 负责
	case clusterDeploymentGone(cd.Status):
		return skipOr(vFailNow, "集群部署已失效（"+cd.Status+"）")
	case cd.Status != appdeploy.ClusterStatusRunning:
		return skipOr(vFail, "集群部署未运行（"+cd.Status+"）")
	case cd.Health != appdeploy.HealthOK:
		return skipOr(vFail, "集群部署健康检查未通过")
	}
	return p.probeNode(ctx, n, slug)
}

// ---- 后台 ----

// adminRedeployGroup 组 ready / unhealthy → RedeployCluster（变量沿用）→ provisioning；rebuild_count 不变。审计 llm.node.rebuild。
func (s *Service) adminRedeployGroup(ctx context.Context, actor audit.Actor, cur Node) (Node, error) {
	if cur.AppClusterID == nil || s.d.ClusterApps == nil {
		return Node{}, apperr.ErrLLMNodeInvalidState
	}
	m, err := s.GetModel(ctx, cur.ModelID)
	if err != nil {
		return Node{}, err
	}
	cd, err := s.d.ClusterApps.RedeployCluster(ctx, appdeploy.ClusterRedeployRequest{UserID: s.d.SystemUserID,
		ClusterDeploymentID: *cur.AppClusterID, VersionID: m.AppVersionID, EnvSet: false})
	if err != nil {
		if errors.Is(err, apperr.ErrAppInvalidState) {
			return Node{}, apperr.ErrLLMNodeInvalidState
		}
		return Node{}, err
	}
	n, ok, err := s.mutateNode(ctx, cur.ID, func(tx pgx.Tx, n *Node, now time.Time) (string, error) {
		if err := expect(n, NodeReady, NodeUnhealthy); err != nil {
			return "", err
		}
		from := n.State
		n.State, n.ProbeFailCount, n.UnhealthySince, n.Error = NodeProvisioning, 0, nil, ""
		if head := headDeploymentID(cd); head != nil {
			n.DeploymentID = head
		}
		return "后台重建（整组重新部署）", nodeAudit(ctx, tx, now, actor, "llm.node.rebuild", *n, from, map[string]any{"size": n.Size})
	})
	if err == nil && !ok {
		err = apperr.ErrLLMNodeInvalidState
	}
	return n, err
}
