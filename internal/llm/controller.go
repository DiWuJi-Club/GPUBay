package llm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

// controllerLockName 咨询锁名：多副本时只有一个 Controller 工作（pg_try_advisory_lock(hashtext('llm-controller'))）。
const controllerLockName = "llm-controller"

// 告警文案（稳定，不含数值）。
const (
	poolShortMessage    = "Token 服务池没有空闲机器，模型节点缺额"
	nodeAppNotInternal  = "内部应用 " + NodeAppSlug + " 未标记为平台内部应用，已暂停创建节点部署"
	provisionTimeoutMsg = "建节点超时（超过 150 分钟）"
)

// Controller 节点生命周期控制循环（规格 §5.2）。对 rental / appdeploy 的调用都在事务外；节点回写只锁 llm_nodes 一行。
type Controller struct {
	s     *Service
	probe *Prober
}

func NewController(s *Service) *Controller { return &Controller{s: s, probe: NewProber(s)} }

// Prober 返回 Controller 内的探测器（测试注入探测函数；e2e 钩子单独跑一轮探测）。
func (c *Controller) Prober() *Prober { return c.probe }

// TickReport 一轮的动作计数：Provisioned 新建节点、Drained 转 draining、Failed 转 failed、Removed 转 removed、
// Orphans 释放的孤儿系统实例（没有非终态节点引用）。
type TickReport struct {
	Provisioned int `json:"provisioned"`
	Drained     int `json:"drained"`
	Failed      int `json:"failed"`
	Removed     int `json:"removed"`
	Orphans     int `json:"orphans"`
}

func (r TickReport) zero() bool { return r == TickReport{} }

// Run 每 ControllerInterval 一轮（ProbeInterval 相同，同一循环里先 Tick 再探测）直到 ctx 结束；错误只记日志。
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(ControllerInterval)
	defer t.Stop()
	for {
		if _, err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
			c.s.d.Log.Error("llm controller round", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce 在专用连接上拿会话级咨询锁（拿不到 → false，别的副本在工作）→ Tick → Prober.Tick → 释放锁。
func (c *Controller) RunOnce(ctx context.Context) (bool, error) {
	conn, err := c.s.d.Pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, controllerLockName).Scan(&locked); err != nil {
		return false, err
	}
	if !locked {
		return false, nil
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, controllerLockName) //nolint:errcheck
	rep, err := c.Tick(ctx, c.s.d.Clock.Now())
	if !rep.zero() {
		c.s.d.Log.Info("llm controller tick", "provisioned", rep.Provisioned, "drained", rep.Drained, "failed", rep.Failed,
			"removed", rep.Removed, "orphans", rep.Orphans)
	}
	perr := c.probe.Tick(ctx, c.s.d.Clock.Now())
	return true, errors.Join(err, perr)
}

// ctlNode 一个非终态节点 + 模型 slug + 本轮 provisioning 的起点（最近一次进入 provisioning 的事件时刻；重建后重新计时）。
type ctlNode struct {
	Node
	slug  string
	since time.Time
}

func (c *Controller) liveNodes(ctx context.Context) ([]ctlNode, error) {
	rows, err := c.s.d.Pool.Query(ctx, `SELECT n.id, n.model_id, n.machine_id, n.instance_id, n.deployment_id, n.state, n.api_key_enc,
		n.probe_fail_count, n.rebuild_count, n.unhealthy_since, n.drain_since, n.last_probe_at, n.last_ready_at, n.error, n.created_at,
		n.updated_at, md.slug,
		COALESCE((SELECT max(e.at) FROM llm_node_events e WHERE e.node_id = n.id AND e.to_state = $2), n.created_at)
		FROM llm_nodes n JOIN llm_models md ON md.id = n.model_id
		WHERE n.state <> ALL($1) ORDER BY n.id`, NodeTerminalStates, NodeProvisioning)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ctlNode, error) {
		var x ctlNode
		n := &x.Node
		err := r.Scan(&n.ID, &n.ModelID, &n.MachineID, &n.InstanceID, &n.DeploymentID, &n.State, &n.APIKeyEnc, &n.ProbeFailCount,
			&n.RebuildCount, &n.UnhealthySince, &n.DrainSince, &n.LastProbeAt, &n.LastReadyAt, &n.Error, &n.CreatedAt, &n.UpdatedAt,
			&x.slug, &x.since)
		return x, err
	})
}

// Tick 执行规格 §5.2 第 1～5 步，外加孤儿系统实例回收。单节点失败记日志并合并进返回的错误，不影响其它节点。
func (c *Controller) Tick(ctx context.Context, now time.Time) (TickReport, error) {
	var rep TickReport
	var errs []error
	note := func(what string, err error) {
		if err != nil {
			c.s.d.Log.Error("llm controller: "+what, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", what, err))
		}
	}
	models, err := c.s.ListModels(ctx)
	if err != nil {
		return rep, err
	}
	modelByID := make(map[int64]Model, len(models))
	for _, m := range models {
		modelByID[m.ID] = m
	}
	poolID, err := c.s.d.Fleet.LLMPoolGroupID(ctx, c.s.d.Pool)
	if err != nil {
		return rep, err
	}
	nodes, err := c.liveNodes(ctx)
	if err != nil {
		return rep, err
	}
	// 1. 期望对账
	c.reconcile(ctx, now, models, nodes, poolID, &rep, note)

	// 2～5. 逐节点推进（重读：第 1 步的排空 / 新建要反映出来）
	if nodes, err = c.liveNodes(ctx); err != nil {
		return rep, errors.Join(append(errs, err)...)
	}
	for _, n := range nodes {
		m := modelByID[n.ModelID]
		switch n.State {
		case NodeProvisioning:
			note("advance "+n.ID, c.advance(ctx, now, n, m, &rep))
		case NodeUnhealthy:
			note("unhealthy "+n.ID, c.handleUnhealthy(ctx, now, n, m, &rep))
		case NodeDraining:
			note("drain "+n.ID, c.handleDraining(ctx, now, n))
		case NodeRemoving:
			note("remove "+n.ID, c.handleRemoving(ctx, n, &rep))
		}
	}
	note("orphans", c.releaseOrphans(ctx, now, &rep))
	return rep, errors.Join(errs...)
}

// ---- 第 1 步：期望对账 ----

type deficit struct {
	m Model
	n int
}

func (c *Controller) reconcile(ctx context.Context, now time.Time, models []Model, nodes []ctlNode, poolID int64, rep *TickReport,
	note func(string, error)) {
	active := map[int64][]ctlNode{}
	for _, n := range nodes {
		switch n.State {
		case NodeProvisioning, NodeReady, NodeUnhealthy:
			active[n.ModelID] = append(active[n.ModelID], n)
		}
	}
	var deficits []deficit
	for _, m := range models {
		want := 0
		if m.Status == ModelOnline {
			want = m.Replicas
		}
		have := active[m.ID]
		switch {
		case len(have) > want:
			detail := "缩容排空"
			if m.Status != ModelOnline {
				detail = "模型已下线，排空"
			}
			for _, v := range drainVictims(have, len(have)-want) {
				_, ok, err := c.s.mutateNode(ctx, v.ID, func(_ pgx.Tx, n *Node, now time.Time) (string, error) {
					if err := expect(n, v.State); err != nil {
						return "", err
					}
					n.State, n.DrainSince = NodeDraining, &now
					return detail, nil
				})
				note("drain "+v.ID, err)
				if ok {
					rep.Drained++
				}
			}
		case len(have) < want:
			deficits = append(deficits, deficit{m, want - len(have)})
		}
	}
	if len(deficits) > 0 {
		note("provision", c.provision(ctx, now, deficits, poolID, rep, note))
	}
}

// drainVictims 缩容挑选顺序：unhealthy（最早不健康的先）→ provisioning（最新建的先）→ ready（last_ready_at 最早的先）。
func drainVictims(nodes []ctlNode, k int) []ctlNode {
	rank := map[string]int{NodeUnhealthy: 0, NodeProvisioning: 1, NodeReady: 2}
	at := func(t *time.Time) time.Time { // nil 排最前
		if t == nil {
			return time.Time{}
		}
		return *t
	}
	out := append([]ctlNode(nil), nodes...)
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if rank[x.State] != rank[y.State] {
			return rank[x.State] < rank[y.State]
		}
		var tx, ty time.Time
		switch x.State {
		case NodeUnhealthy:
			tx, ty = at(x.UnhealthySince), at(y.UnhealthySince)
		case NodeProvisioning: // 最新建的先排空（进度最少）
			tx, ty = y.CreatedAt, x.CreatedAt
		default:
			tx, ty = at(x.LastReadyAt), at(y.LastReadyAt)
		}
		if !tx.Equal(ty) {
			return tx.Before(ty)
		}
		return x.ID < y.ID
	})
	return out[:k]
}

// provision 补缺额：只读选候选机器（ORDER BY id），逐台 createNode（PlaceSystem 在锁下复核，不可用就跳过）；
// 一轮最多 MaxProvisionPerRound 个。候选用完仍有缺额（不是被每轮上限截住）→ RaiseAlertOnce(llm_pool_short)。
func (c *Controller) provision(ctx context.Context, now time.Time, deficits []deficit, poolID int64, rep *TickReport,
	note func(string, error)) error {
	total := 0
	for _, d := range deficits {
		total += d.n
	}
	budget := min(total, MaxProvisionPerRound)
	cands, err := c.s.candidateMachines(ctx, poolID, now, budget+MaxProvisionPerRound) // 多取几台，抵消并发被占的
	if err != nil {
		return err
	}
	ci, short := 0, false
models:
	for _, d := range deficits {
		for k := 0; k < d.n; k++ {
			if budget == 0 {
				break models
			}
			placed := false
			for ci < len(cands) {
				mid := cands[ci]
				ci++
				_, err := c.s.createNode(ctx, d.m, mid, poolID, fmt.Sprintf("补副本：模型 %s", d.m.Slug))
				if errors.Is(err, apperr.ErrMachineUnavailable) {
					continue
				}
				if err != nil {
					// 与机器无关的失败（镜像、库错误等）：这台机器留给下一个模型，本模型本轮不再补
					ci--
					note("create node for "+d.m.Slug, err)
					continue models
				}
				placed = true
				rep.Provisioned++
				budget--
				break
			}
			if !placed {
				short = true
				break models
			}
		}
	}
	if short {
		return c.s.d.Fleet.RaiseAlertOnce(ctx, c.s.d.Pool, fleet.Alert{Level: "warning", Kind: AlertPoolShort, Message: poolShortMessage})
	}
	return nil
}

// ---- 失败 ----

// failNode expectState → failed（error = detail），alert 非空则同一事务里告警；随后（事务外）释放仍占机器的系统实例。
func (c *Controller) failNode(ctx context.Context, n Node, expectState, detail string, alert *fleet.Alert, rep *TickReport) error {
	_, ok, err := c.s.mutateNode(ctx, n.ID, func(tx pgx.Tx, x *Node, _ time.Time) (string, error) {
		if err := expect(x, expectState); err != nil {
			return "", err
		}
		x.State, x.Error = NodeFailed, detail
		if alert != nil {
			return detail, c.s.d.Fleet.RaiseAlert(ctx, tx, *alert)
		}
		return detail, nil
	})
	if err != nil || !ok {
		return err
	}
	rep.Failed++
	return c.s.releaseInstance(ctx, n.InstanceID)
}

// instanceGoneText 实例结束的可展示原因。
func instanceGoneText(state string) string {
	switch state {
	case rental.StateFailed:
		return "实例创建失败或已失效"
	case rental.StateWiping, rental.StateReleased:
		return "实例已释放"
	case rental.StateCancelled:
		return "实例已取消"
	}
	return "实例状态异常（" + state + "）"
}

// ---- 第 2 步：推进 provisioning ----

func (c *Controller) advance(ctx context.Context, now time.Time, cn ctlNode, m Model, rep *TickReport) error {
	n := cn.Node
	if n.InstanceID == nil {
		return c.failNode(ctx, n, NodeProvisioning, "节点没有实例", nil, rep)
	}
	inst, err := c.s.d.Rental.Get(ctx, *n.InstanceID)
	if errors.Is(err, apperr.ErrNotFound) {
		return c.failNode(ctx, n, NodeProvisioning, "实例不存在", nil, rep)
	}
	if err != nil {
		return err
	}
	if instanceGone(inst.State) {
		return c.failNode(ctx, n, NodeProvisioning, instanceGoneText(inst.State), nil, rep)
	}
	var dep *appdeploy.Deployment
	if n.DeploymentID != nil {
		d, err := c.s.d.Apps.Get(ctx, *n.DeploymentID)
		if err != nil {
			return err
		}
		dep = &d
		if d.State == appdeploy.StateFailed || d.State == appdeploy.StateRemoved || d.State == appdeploy.StateLost {
			return c.failNode(ctx, n, NodeProvisioning, deploymentError(d), nil, rep)
		}
	}
	if now.Sub(cn.since) > ProvisionTimeout {
		return c.failNode(ctx, n, NodeProvisioning, provisionTimeoutMsg, nil, rep)
	}
	if inst.State != rental.StateRunning {
		return nil // creating：等 Agent 建好容器
	}
	if dep == nil {
		return c.createDeployment(ctx, n, m)
	}
	if dep.State == appdeploy.StateRunning && dep.Health == appdeploy.HealthOK {
		_, err := c.probe.tryPromote(ctx, n, cn.slug)
		return err
	}
	return nil
}

// createDeployment 实例 running 且还没有部署 → appdeploy.Create(vllm-node, 系统账号, NodeEnv)。vllm-node 未标记 internal
// → 告警（一次）且不建部署（它会出现在用户应用市场，配置有误）。
func (c *Controller) createDeployment(ctx context.Context, n Node, m Model) error {
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
	dep, err := c.s.d.Apps.Create(ctx, appdeploy.CreateRequest{UserID: c.s.d.SystemUserID, InstanceID: *n.InstanceID, AppID: app.ID,
		VersionID: m.AppVersionID, Env: NodeEnv(m, string(key)), IP: ""})
	if err != nil {
		msg := "创建部署失败"
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
		if err := expect(x, NodeProvisioning); err != nil || x.DeploymentID != nil {
			return "", errNodeStale
		}
		x.DeploymentID, x.Error = &dep.ID, ""
		return "", nil
	})
	return err
}

// ---- 第 3 步：unhealthy ----

func (c *Controller) handleUnhealthy(ctx context.Context, now time.Time, cn ctlNode, m Model, rep *TickReport) error {
	n := cn.Node
	if n.InstanceID == nil {
		return c.failNode(ctx, n, NodeUnhealthy, "节点没有实例", nil, rep)
	}
	inst, err := c.s.d.Rental.Get(ctx, *n.InstanceID)
	if errors.Is(err, apperr.ErrNotFound) {
		return c.failNode(ctx, n, NodeUnhealthy, "实例不存在", nil, rep)
	}
	if err != nil {
		return err
	}
	if instanceGone(inst.State) {
		return c.failNode(ctx, n, NodeUnhealthy, instanceGoneText(inst.State), nil, rep)
	}
	if mach, err := c.s.d.Fleet.Get(ctx, c.s.d.Pool, n.MachineID); err != nil {
		return err
	} else if mach.State == fleet.StateFaulty {
		return c.failNode(ctx, n, NodeUnhealthy, "机器故障", nil, rep)
	}
	if n.UnhealthySince == nil || now.Before(n.UnhealthySince.Add(UnhealthyRebuildAfter)) {
		return nil
	}
	if n.DeploymentID == nil {
		return c.failNode(ctx, n, NodeUnhealthy, "节点没有部署，无法重建", nil, rep)
	}
	if n.RebuildCount >= MaxRebuilds {
		return c.failNode(ctx, n, NodeUnhealthy, "多次重建仍不健康", &fleet.Alert{Level: "critical", Kind: AlertNodeFailed,
			MachineID: n.MachineID, InstanceID: *n.InstanceID, Message: "节点 " + n.ID + "（" + cn.slug + "）多次重建失败"}, rep)
	}
	dep, err := c.s.d.Apps.Get(ctx, *n.DeploymentID)
	if err != nil {
		return err
	}
	if dep.State == appdeploy.StateQueued || dep.State == appdeploy.StateInstalling {
		// 部署已在重新安装（如机器恢复后 OnInstanceRunning 置回 queued）：不再 Redeploy、不计重建次数，按 provisioning 等待（超时兜底）
		_, _, err := c.s.mutateNode(ctx, n.ID, func(_ pgx.Tx, x *Node, _ time.Time) (string, error) {
			if err := expect(x, NodeUnhealthy); err != nil {
				return "", err
			}
			x.State, x.ProbeFailCount, x.UnhealthySince = NodeProvisioning, 0, nil
			return "部署正在重新安装，等待就绪", nil
		})
		return err
	}
	if _, err := c.s.d.Apps.Redeploy(ctx, appdeploy.RedeployRequest{UserID: c.s.d.SystemUserID, InstanceID: *n.InstanceID,
		DeploymentID: *n.DeploymentID, VersionID: m.AppVersionID, EnvSet: false}); err != nil {
		return fmt.Errorf("redeploy: %w", err) // 没有真正执行：rebuild_count 不变，下一轮再试
	}
	_, _, err = c.s.mutateNode(ctx, n.ID, func(_ pgx.Tx, x *Node, _ time.Time) (string, error) {
		if err := expect(x, NodeUnhealthy); err != nil {
			return "", err
		}
		x.RebuildCount++
		x.State, x.ProbeFailCount, x.UnhealthySince = NodeProvisioning, 0, nil
		return fmt.Sprintf("持续不健康，自动重建（第 %d 次）", x.RebuildCount), nil
	})
	return err
}

// ---- 第 4、5 步：排空与移除 ----

// handleDraining 在途归零或 drain_since + DrainTimeout ≤ now → ReleaseSystem → removing。
func (c *Controller) handleDraining(ctx context.Context, now time.Time, cn ctlNode) error {
	n := cn.Node
	inflight := c.s.router.Inflight(n.ID)
	timedOut := n.DrainSince == nil || !now.Before(n.DrainSince.Add(DrainTimeout))
	if inflight > 0 && !timedOut {
		return nil
	}
	if err := c.s.releaseInstance(ctx, n.InstanceID); err != nil {
		return err
	}
	detail := "在途请求已归零，释放机器"
	if inflight > 0 {
		detail = "排空超时，释放机器"
	}
	_, _, err := c.s.mutateNode(ctx, n.ID, func(_ pgx.Tx, x *Node, _ time.Time) (string, error) {
		if err := expect(x, NodeDraining); err != nil {
			return "", err
		}
		x.State = NodeRemoving
		return detail, nil
	})
	return err
}

// handleRemoving 实例已结束（released / failed / cancelled / 不存在）→ removed；实例仍占机器 → 重试 ReleaseSystem；wiping → 等洗机。
func (c *Controller) handleRemoving(ctx context.Context, cn ctlNode, rep *TickReport) error {
	n := cn.Node
	done := n.InstanceID == nil
	if !done {
		inst, err := c.s.d.Rental.Get(ctx, *n.InstanceID)
		switch {
		case errors.Is(err, apperr.ErrNotFound):
			done = true
		case err != nil:
			return err
		case instanceLive(inst.State):
			return c.s.releaseInstance(ctx, n.InstanceID)
		case inst.State != rental.StateWiping:
			done = true
		}
	}
	if !done {
		return nil
	}
	_, ok, err := c.s.mutateNode(ctx, n.ID, func(_ pgx.Tx, x *Node, _ time.Time) (string, error) {
		if err := expect(x, NodeRemoving); err != nil {
			return "", err
		}
		x.State = NodeRemoved
		return "机器已释放", nil
	})
	if ok {
		rep.Removed++
	}
	return err
}

// releaseOrphans 没有非终态节点引用、建立超过 orphanGrace 的系统实例（节点 failed 后释放失败、建节点插行失败后释放失败等）→ ReleaseSystem。
// 先列实例再读引用：列表之后才插入的节点行一定能在引用集里看到。
func (c *Controller) releaseOrphans(ctx context.Context, now time.Time, rep *TickReport) error {
	insts, err := c.s.d.Rental.ListLiveSystemInstances(ctx)
	if err != nil || len(insts) == 0 {
		return err
	}
	rows, err := c.s.d.Pool.Query(ctx, `SELECT instance_id FROM llm_nodes WHERE state <> ALL($1) AND instance_id IS NOT NULL`, NodeTerminalStates)
	if err != nil {
		return err
	}
	refs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	used := make(map[string]bool, len(refs))
	for _, id := range refs {
		used[id] = true
	}
	var errs []error
	for _, inst := range insts {
		if used[inst.ID] || !instanceLive(inst.State) || now.Sub(inst.CreatedAt) < orphanGrace {
			continue
		}
		c.s.d.Log.Warn("llm controller: releasing orphan system instance", "instance", inst.ID, "machine", inst.MachineID, "state", inst.State)
		id := inst.ID
		if err := c.s.releaseInstance(ctx, &id); err != nil {
			errs = append(errs, fmt.Errorf("orphan %s: %w", inst.ID, err))
			continue
		}
		rep.Orphans++
	}
	return errors.Join(errs...)
}
