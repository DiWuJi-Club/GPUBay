package llm

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/rental"
)

const nodeCols = `id, model_id, machine_id, instance_id, deployment_id, state, api_key_enc, probe_fail_count, rebuild_count,
	unhealthy_since, drain_since, last_probe_at, last_ready_at, error, created_at, updated_at, size, cluster_id, app_cluster_id`

// nodeColsN 同 nodeCols，带表别名 n.（联表查询用；扫描目标同 nodeDest）。
const nodeColsN = `n.id, n.model_id, n.machine_id, n.instance_id, n.deployment_id, n.state, n.api_key_enc, n.probe_fail_count,
	n.rebuild_count, n.unhealthy_since, n.drain_since, n.last_probe_at, n.last_ready_at, n.error, n.created_at, n.updated_at,
	n.size, n.cluster_id, n.app_cluster_id`

// nodeDest 是 nodeCols / nodeColsN 的扫描目标（extra 追加在后）。
func nodeDest(n *Node, extra ...any) []any {
	return append([]any{&n.ID, &n.ModelID, &n.MachineID, &n.InstanceID, &n.DeploymentID, &n.State, &n.APIKeyEnc, &n.ProbeFailCount,
		&n.RebuildCount, &n.UnhealthySince, &n.DrainSince, &n.LastProbeAt, &n.LastReadyAt, &n.Error, &n.CreatedAt, &n.UpdatedAt,
		&n.Size, &n.ClusterID, &n.AppClusterID}, extra...)
}

// maxNodeError llm_nodes.error 的长度上限（字符）。
const maxNodeError = 200

// orphanGrace 孤儿系统实例的宽限期：刚 PlaceSystem、节点行还没插入的实例不算孤儿。
const orphanGrace = 2 * time.Minute

// nodeLiveStates 是占机器的节点状态（非终态）；nodeActiveStates 是计入副本数的状态（规格 §5.2 第 1 步）。
var (
	nodeLiveStates   = []string{NodeProvisioning, NodeReady, NodeUnhealthy, NodeDraining, NodeRemoving}
	nodeActiveStates = []string{NodeProvisioning, NodeReady, NodeUnhealthy}
)

// instanceLive：系统实例仍占着机器、可以（需要）ReleaseSystem。wiping 已在释放链路上。
func instanceLive(state string) bool {
	return state == rental.StateCreating || state == rental.StateRunning || state == rental.StateStopped
}

// instanceGone：实例已结束（或正在洗机），节点不可能再用它。
func instanceGone(state string) bool {
	switch state {
	case rental.StateWiping, rental.StateReleased, rental.StateFailed, rental.StateCancelled:
		return true
	}
	return false
}

func scanNode(row pgx.Row) (Node, error) {
	var n Node
	err := row.Scan(nodeDest(&n)...)
	if db.IsNoRows(err) {
		return Node{}, apperr.ErrLLMNodeNotFound
	}
	return n, err
}

func collectNodes(rows pgx.Rows, err error) ([]Node, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Node, error) { return scanNode(r) })
}

// clipRunes 截到 n 个字符。
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func newNodeID() string { return randx.ID("node-", 12) }

// GetNodeRow 取节点行（不含视图字段）。不存在 → llm_node_not_found。
func (s *Service) getNode(ctx context.Context, q db.Querier, id string) (Node, error) {
	return scanNode(q.QueryRow(ctx, `SELECT `+nodeCols+` FROM llm_nodes WHERE id = $1`, id))
}

// errNodeStale：节点状态已不是调用方预期的（被并发改过），本次不写。
var errNodeStale = errors.New("llm: node state changed")

// mutateNode 节点行回写（规格 §14：只锁 llm_nodes(node) 一行）：FOR UPDATE 锁行 → fn 就地修改 n（须先复核状态，
// 不符返回 errNodeStale）→ 整行可变列写回、updated_at = now；状态变了写一条 llm_node_events（detail = fn 的返回值）。
// 返回写回后的行与是否写了；errNodeStale 不当错误（ok = false）。rental / appdeploy 调用不得放进 fn。
// 计划 13：状态进入终态（failed / removed）时同一事务里 llm_node_members.live = false（组的成员机器随之可再用）。
func (s *Service) mutateNode(ctx context.Context, id string, fn func(tx pgx.Tx, n *Node, now time.Time) (string, error)) (Node, bool, error) {
	var out Node
	err := withRetry(func() error {
		return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			cur, err := scanNode(tx.QueryRow(ctx, `SELECT `+nodeCols+` FROM llm_nodes WHERE id = $1 FOR UPDATE`, id))
			if err != nil {
				return err
			}
			now := s.d.Clock.Now()
			n := cur
			detail, err := fn(tx, &n, now)
			if err != nil {
				return err
			}
			n.Error = clipRunes(n.Error, maxNodeError)
			out, err = scanNode(tx.QueryRow(ctx, `UPDATE llm_nodes SET deployment_id = $2, state = $3, probe_fail_count = $4, rebuild_count = $5,
			unhealthy_since = $6, drain_since = $7, last_probe_at = $8, last_ready_at = $9, error = $10, updated_at = $11, app_cluster_id = $12
			WHERE id = $1 RETURNING `+nodeCols, id, n.DeploymentID, n.State, n.ProbeFailCount, n.RebuildCount, n.UnhealthySince,
				n.DrainSince, n.LastProbeAt, n.LastReadyAt, n.Error, now, n.AppClusterID))
			if err != nil {
				return err
			}
			if slices.Contains(NodeTerminalStates, n.State) && !slices.Contains(NodeTerminalStates, cur.State) {
				if _, err := tx.Exec(ctx, `UPDATE llm_node_members SET live = false WHERE node_id = $1 AND live`, id); err != nil {
					return err
				}
			}
			if n.State != cur.State {
				return insertNodeEvent(ctx, tx, id, cur.State, n.State, detail, now)
			}
			return nil
		})
	})
	if errors.Is(err, errNodeStale) {
		return Node{}, false, nil
	}
	if err != nil {
		return Node{}, false, err
	}
	return out, true, nil
}

func insertNodeEvent(ctx context.Context, q db.Querier, nodeID, from, to, detail string, at time.Time) error {
	_, err := q.Exec(ctx, `INSERT INTO llm_node_events (node_id, at, from_state, to_state, detail) VALUES ($1, $2, $3, $4, $5)`,
		nodeID, at, from, to, clipRunes(detail, 500))
	return err
}

// expect 返回 fn 的前置检查：状态须在 states 内，否则 errNodeStale。
func expect(n *Node, states ...string) error {
	for _, st := range states {
		if n.State == st {
			return nil
		}
	}
	return errNodeStale
}

// createNode 在机器上建节点（规格 §5.2 第 1 步，事务外调用）：生成节点 ID 与密钥 → rental.PlaceSystem（它锁机器并复核）
// → 插 llm_nodes(provisioning, instance_id) 与事件。插入失败（含唯一索引撞车）→ 释放刚建的系统实例。
// PlaceSystem 的 ErrMachineUnavailable 原样返回（调用方跳过该机器）。
// extra（可为 nil）在插入节点行的同一事务里执行（复核 / 审计）；它返回错误则整个事务回滚、实例释放。
func (s *Service) createNode(ctx context.Context, m Model, machineID, poolID int64, detail string,
	extra func(tx pgx.Tx, n Node, now time.Time) error) (Node, error) {
	id := newNodeID()
	_, enc, err := s.newNodeKey()
	if err != nil {
		return Node{}, err
	}
	inst, err := s.d.Rental.PlaceSystem(ctx, rental.SystemPlaceRequest{MachineID: machineID, GroupID: poolID, ImageID: m.ImageID,
		Note: "llm node " + id + " " + m.Slug})
	if err != nil {
		return Node{}, err
	}
	var out Node
	err = db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		now := s.d.Clock.Now()
		n, err := scanNode(tx.QueryRow(ctx, `INSERT INTO llm_nodes (id, model_id, machine_id, instance_id, state, api_key_enc, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7) RETURNING `+nodeCols, id, m.ID, machineID, inst.ID, NodeProvisioning, enc, now))
		if err != nil {
			return err
		}
		out = n
		if err := insertNodeEvent(ctx, tx, id, "", NodeProvisioning, detail, now); err != nil {
			return err
		}
		if extra != nil {
			return extra(tx, n, now)
		}
		return nil
	})
	if err != nil {
		if rerr := s.d.Rental.ReleaseSystem(ctx, inst.ID); rerr != nil {
			s.d.Log.Warn("llm: release system instance after node insert failure", "instance", inst.ID, "err", rerr)
		}
		if db.IsUniqueViolation(err) {
			return Node{}, apperr.ErrMachineUnavailable
		}
		return Node{}, err
	}
	return out, nil
}

// releaseNode 释放节点占的机器：组节点（size > 1）→ ReleaseSystemCluster（整组）；单机 → releaseInstance。
func (s *Service) releaseNode(ctx context.Context, n Node) error {
	if n.Size > 1 && n.ClusterID != nil {
		return s.releaseCluster(ctx, *n.ClusterID)
	}
	return s.releaseInstance(ctx, n.InstanceID)
}

// releaseInstance 实例仍占机器时 ReleaseSystem；已在释放 / 已结束（invalid_state）、不存在都视为完成。
func (s *Service) releaseInstance(ctx context.Context, instanceID *string) error {
	if instanceID == nil {
		return nil
	}
	err := s.d.Rental.ReleaseSystem(ctx, *instanceID)
	if errors.Is(err, apperr.ErrInvalidState) || errors.Is(err, apperr.ErrNotFound) {
		return nil
	}
	return err
}

// ---- 后台操作（规格 §10.2）----

func nodeAudit(ctx context.Context, tx pgx.Tx, now time.Time, actor audit.Actor, action string, n Node, from string, extra map[string]any) error {
	diff := map[string]any{"model_id": n.ModelID, "machine_id": n.MachineID, "from": from, "to": n.State}
	for k, v := range extra {
		diff[k] = v
	}
	return audit.Record(ctx, tx, now, actor.Entry(action, "llm_node", n.ID, diff))
}

// AdminDrainNode 排空：ready / unhealthy / provisioning → draining（drain_since = now）；Controller 等在途归零或 60 秒后释放。
// 其它状态 → llm_node_invalid_state。审计 llm.node.drain。
func (s *Service) AdminDrainNode(ctx context.Context, actor audit.Actor, id string) (Node, error) {
	n, ok, err := s.mutateNode(ctx, id, func(tx pgx.Tx, n *Node, now time.Time) (string, error) {
		if expect(n, NodeReady, NodeUnhealthy, NodeProvisioning) != nil {
			return "", apperr.ErrLLMNodeInvalidState
		}
		from := n.State
		n.State, n.DrainSince = NodeDraining, &now
		return "后台排空", nodeAudit(ctx, tx, now, actor, "llm.node.drain", *n, from, nil)
	})
	if err == nil && !ok {
		err = apperr.ErrLLMNodeInvalidState
	}
	return n, err
}

// AdminRebuildNode 重建（规格 §10.2）：
//   - ready / unhealthy → appdeploy.Redeploy（变量沿用）→ provisioning；rebuild_count 不变（人工重建不计入自动上限）；
//   - failed → 在同一台机器上新建节点行（不是复活旧行），机器不可用（实例未释放完 / 已出租 / 离线等）→ llm_node_invalid_state；
//     返回新节点；
//   - 其它 → llm_node_invalid_state。审计 llm.node.rebuild。
//
// 组节点（计划 13 §7.6）：ready / unhealthy → RedeployCluster（整组，人工重建不计入自动上限）；failed → llm_node_invalid_state
// （多机节点组失败后由系统在别处自动另建）。
func (s *Service) AdminRebuildNode(ctx context.Context, actor audit.Actor, id string) (Node, error) {
	cur, err := s.getNode(ctx, s.d.Pool, id)
	if err != nil {
		return Node{}, err
	}
	if cur.Size > 1 {
		if cur.State == NodeReady || cur.State == NodeUnhealthy {
			return s.adminRedeployGroup(ctx, actor, cur)
		}
		return Node{}, apperr.ErrLLMNodeInvalidState
	}
	switch cur.State {
	case NodeReady, NodeUnhealthy:
		return s.adminRedeploy(ctx, actor, cur)
	case NodeFailed:
		return s.adminRecreate(ctx, actor, cur)
	}
	return Node{}, apperr.ErrLLMNodeInvalidState
}

func (s *Service) adminRedeploy(ctx context.Context, actor audit.Actor, cur Node) (Node, error) {
	if cur.InstanceID == nil || cur.DeploymentID == nil {
		return Node{}, apperr.ErrLLMNodeInvalidState
	}
	m, err := s.GetModel(ctx, cur.ModelID)
	if err != nil {
		return Node{}, err
	}
	// 事务外调用 appdeploy（它自己锁 instances → app_deployments），再锁节点行复核状态
	if _, err := s.d.Apps.Redeploy(ctx, appdeploy.RedeployRequest{UserID: s.d.SystemUserID, InstanceID: *cur.InstanceID,
		DeploymentID: *cur.DeploymentID, VersionID: m.AppVersionID, EnvSet: false}); err != nil {
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
		return "后台重建（重新部署）", nodeAudit(ctx, tx, now, actor, "llm.node.rebuild", *n, from, nil)
	})
	if err == nil && !ok {
		err = apperr.ErrLLMNodeInvalidState
	}
	return n, err
}

func (s *Service) adminRecreate(ctx context.Context, actor audit.Actor, cur Node) (Node, error) {
	m, err := s.GetModel(ctx, cur.ModelID)
	if err != nil {
		return Node{}, err
	}
	// 预检（不调 rental）：模型在线、仍有缺额、旧节点未被替换过
	if err := s.checkRecreate(ctx, s.d.Pool, m, cur, ""); err != nil {
		return Node{}, err
	}
	poolID, err := s.d.Fleet.LLMPoolGroupID(ctx, s.d.Pool)
	if err != nil {
		return Node{}, err
	}
	n, err := s.createNode(ctx, m, cur.MachineID, poolID, "后台重建（替换节点 "+cur.ID+"）", func(tx pgx.Tx, n Node, now time.Time) error {
		// 锁旧节点行复核（并发的两次重建只有一次成功），替换关系记进旧节点的事件，审计与新节点同一事务
		old, err := scanNode(tx.QueryRow(ctx, `SELECT `+nodeCols+` FROM llm_nodes WHERE id = $1 FOR UPDATE`, cur.ID))
		if err != nil {
			return err
		}
		if old.State != NodeFailed {
			return apperr.ErrLLMNodeInvalidState
		}
		if err := s.checkRecreate(ctx, tx, m, old, n.ID); err != nil {
			return err
		}
		if err := insertNodeEvent(ctx, tx, old.ID, NodeFailed, NodeFailed, "已由节点 "+n.ID+" 替换", now); err != nil {
			return err
		}
		return nodeAudit(ctx, tx, now, actor, "llm.node.rebuild", n, NodeFailed, map[string]any{"replaces": old.ID})
	})
	if errors.Is(err, apperr.ErrMachineUnavailable) {
		return Node{}, apperr.ErrLLMNodeInvalidState
	}
	if err != nil {
		return Node{}, err
	}
	return n, nil
}

// checkRecreate failed 节点能否在原机器上重建：模型 online；计入副本的节点数（不含刚插入的 newID）< replicas；
// 该机器上没有比旧节点更新的其它节点（旧节点已被替换过，或机器已被别的节点用过）。不满足 → llm_node_invalid_state。
func (s *Service) checkRecreate(ctx context.Context, q db.Querier, m Model, old Node, newID string) error {
	if m.Status != ModelOnline {
		return apperr.ErrLLMNodeInvalidState
	}
	var active int
	var replaced bool
	if err := q.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM llm_nodes WHERE model_id = $1 AND state = ANY($2) AND id <> $3),
		EXISTS (SELECT 1 FROM llm_nodes WHERE machine_id = $4 AND id NOT IN ($5, $3) AND created_at >= $6)`,
		m.ID, nodeActiveStates, newID, old.MachineID, old.ID, old.CreatedAt).Scan(&active, &replaced); err != nil {
		return err
	}
	if active >= m.Replicas || replaced {
		return apperr.ErrLLMNodeInvalidState
	}
	return nil
}

// AdminRemoveNode 移除（规格 §10.2）：provisioning / ready / unhealthy / draining → removing 并立即 ReleaseSystem（不等在途）；
// failed → removed（清理记录；实例若仍在则一并释放）。removing / removed → llm_node_invalid_state。reason 必填（≤ 200 字符）。
// 组节点释放全部成员（ReleaseSystemCluster）。
// 审计 llm.node.remove{reason}。ReleaseSystem 失败只记日志：节点已是 removing，Controller 每轮重试释放。
func (s *Service) AdminRemoveNode(ctx context.Context, actor audit.Actor, id, reason string) (Node, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > maxAdminReason {
		return Node{}, apperr.BadRequest("请填写原因（最多 200 个字符）")
	}
	n, ok, err := s.mutateNode(ctx, id, func(tx pgx.Tx, n *Node, now time.Time) (string, error) {
		from := n.State
		switch n.State {
		case NodeProvisioning, NodeReady, NodeUnhealthy, NodeDraining:
			n.State = NodeRemoving
		case NodeFailed:
			n.State = NodeRemoved
		default:
			return "", apperr.ErrLLMNodeInvalidState
		}
		return "后台移除：" + reason, nodeAudit(ctx, tx, now, actor, "llm.node.remove", *n, from, map[string]any{"reason": reason})
	})
	if err != nil {
		return Node{}, err
	}
	if !ok {
		return Node{}, apperr.ErrLLMNodeInvalidState
	}
	if err := s.releaseNode(ctx, n); err != nil {
		s.d.Log.Warn("llm: admin remove node: release system instance failed; controller will retry", "node", n.ID, "err", err)
	}
	return n, nil
}

// MachineHasLiveNode 机器上有非终态节点、是非终态组节点的成员（llm_node_members.live），
// 或有未结束的系统实例（节点 failed 后实例尚未释放完）→ true。
// 池机组移出机器前由 adminapi 调用（llm_pool_machine_busy）。
func (s *Service) MachineHasLiveNode(ctx context.Context, machineID int64) (bool, error) {
	var busy bool
	err := s.d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llm_nodes WHERE machine_id = $1 AND state <> ALL($2))
		OR EXISTS (SELECT 1 FROM llm_node_members WHERE machine_id = $1 AND live)
		OR EXISTS (SELECT 1 FROM instances WHERE machine_id = $1 AND system AND state IN ('creating', 'running', 'stopped', 'wiping'))`,
		machineID, NodeTerminalStates).Scan(&busy)
	return busy, err
}

// ---- 后台查询 ----

// NodeFilter 节点列表筛选：ModelID 0 = 全部；State "" = 除 removed 外全部，"all" = 含 removed，其它 = 该状态。
type NodeFilter struct {
	ModelID int64
	State   string
}

// NodeDeployment 节点部署的摘要（appdeploy 读不到时整个为 null）。
type NodeDeployment struct {
	State    string `json:"state"`
	Health   string `json:"health"`
	Phase    string `json:"phase"`
	Progress int    `json:"progress"`
}

// NodeRow 后台节点列表 / 详情的一行：节点字段 + 模型 slug、机器展示名、部署摘要、网关在途数。
// 计划 13：组节点带 Members（按 rank，含 rank0；单机节点为空数组）与集群部署聚合 AppCluster（读不到 / 还没建为 null）；
// 组节点的 Deployment 是 head（rank0）的成员部署。
type NodeRow struct {
	Node
	ModelSlug   string          `json:"model_slug"`
	MachineName string          `json:"machine_name"`
	Deployment  *NodeDeployment `json:"deployment"`
	Inflight    int             `json:"inflight"`
	Members     []NodeMemberRow `json:"members"`
	AppCluster  *NodeAppCluster `json:"app_cluster"`
}

// NodeMemberRow 组节点的一个成员（后台节点列表 / 详情）。Deployment 为该成员在集群部署里的成员部署（读不到为 null）。
type NodeMemberRow struct {
	Rank          int             `json:"rank"`
	MachineID     int64           `json:"machine_id"`
	MachineName   string          `json:"machine_name"`
	InstanceID    string          `json:"instance_id"`
	InstanceState string          `json:"instance_state"`
	Suspended     bool            `json:"suspended"`
	Deployment    *NodeDeployment `json:"deployment"`
}

// NodeAppCluster 组节点的集群部署聚合（appdeploy 规格 §6.4）。
type NodeAppCluster struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Health   string `json:"health"`
	Progress int    `json:"progress"`
}

// NodeDetail GET /llm/nodes/{id}：节点 + 最近 50 条节点事件 + 部署事件（新的在前）。
type NodeDetail struct {
	Node             NodeRow           `json:"node"`
	Events           []NodeEvent       `json:"events"`
	DeploymentEvents []appdeploy.Event `json:"deployment_events"`
}

const nodeRowSelect = `SELECT ` + nodeColsN + `,
	md.slug, COALESCE(NULLIF(m.display_name, ''), m.serial)
	FROM llm_nodes n JOIN llm_models md ON md.id = n.model_id JOIN machines m ON m.id = n.machine_id`

func (s *Service) collectNodeRows(ctx context.Context, rows pgx.Rows, err error) ([]NodeRow, error) {
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (NodeRow, error) {
		var x NodeRow
		err := r.Scan(nodeDest(&x.Node, &x.ModelSlug, &x.MachineName)...)
		x.Members = []NodeMemberRow{}
		return x, err
	})
	if err != nil {
		return nil, err
	}
	if err := s.loadMemberRows(ctx, out); err != nil {
		return nil, err
	}
	for i := range out {
		s.decorateNodeRow(ctx, &out[i])
	}
	return out, nil
}

// loadMemberRows 给组节点的行装上成员（llm_node_members 全部行，含已 live = false 的：失败节点也能看到当时的成员）。
func (s *Service) loadMemberRows(ctx context.Context, list []NodeRow) error {
	idx := map[string]int{}
	var ids []string
	for i, x := range list {
		if x.Size > 1 {
			idx[x.ID] = i
			ids = append(ids, x.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT mm.node_id, mm.rank, mm.machine_id, COALESCE(NULLIF(m.display_name, ''), m.serial),
		mm.instance_id, COALESCE(i.state, ''), i.suspended_at IS NOT NULL
		FROM llm_node_members mm JOIN machines m ON m.id = mm.machine_id LEFT JOIN instances i ON i.id = mm.instance_id
		WHERE mm.node_id = ANY($1) ORDER BY mm.node_id, mm.rank`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID string
		var r NodeMemberRow
		if err := rows.Scan(&nodeID, &r.Rank, &r.MachineID, &r.MachineName, &r.InstanceID, &r.InstanceState, &r.Suspended); err != nil {
			return err
		}
		x := &list[idx[nodeID]]
		x.Members = append(x.Members, r)
	}
	return rows.Err()
}

// decorateNodeRow 补部署摘要（读失败忽略）与在途数；组节点另补集群部署聚合与各成员的成员部署。
func (s *Service) decorateNodeRow(ctx context.Context, x *NodeRow) {
	x.Inflight = s.router.Inflight(x.ID)
	if x.Size > 1 && x.AppClusterID != nil && s.d.ClusterApps != nil {
		if cd, err := s.d.ClusterApps.GetClusterDeployment(ctx, *x.AppClusterID); err == nil {
			x.AppCluster = &NodeAppCluster{ID: cd.ID, Status: cd.Status, Health: cd.Health, Progress: cd.Progress}
			for i := range x.Members {
				for _, md := range cd.Members {
					if md.InstanceID == x.Members[i].InstanceID {
						d := md.Deployment
						x.Members[i].Deployment = &NodeDeployment{State: d.State, Health: d.Health, Phase: d.Phase, Progress: d.Progress}
					}
				}
			}
		}
	}
	if x.DeploymentID == nil || s.d.Apps == nil {
		return
	}
	d, err := s.d.Apps.Get(ctx, *x.DeploymentID)
	if err != nil {
		return
	}
	x.Deployment = &NodeDeployment{State: d.State, Health: d.Health, Phase: d.Phase, Progress: d.Progress}
}

// ListNodes 后台节点列表（按模型、创建时间），最多 500 行。
func (s *Service) ListNodes(ctx context.Context, f NodeFilter) ([]NodeRow, error) {
	var w db.Where
	if f.ModelID != 0 {
		w.Add("n.model_id = ?", f.ModelID)
	}
	switch f.State {
	case "":
		w.Add("n.state <> ?", NodeRemoved)
	case "all":
	default:
		w.Add("n.state = ?", f.State)
	}
	rows, err := s.d.Pool.Query(ctx, nodeRowSelect+` WHERE `+w.SQL()+` ORDER BY n.model_id, n.created_at, n.id LIMIT 500`, w.Args...)
	return s.collectNodeRows(ctx, rows, err)
}

// GetNode 节点详情：行 + 最近 50 条节点事件 + 部署事件（最近 50 条；读失败为空）。
func (s *Service) GetNode(ctx context.Context, id string) (NodeDetail, error) {
	rows, err := s.d.Pool.Query(ctx, nodeRowSelect+` WHERE n.id = $1`, id)
	list, err := s.collectNodeRows(ctx, rows, err)
	if err != nil {
		return NodeDetail{}, err
	}
	if len(list) == 0 {
		return NodeDetail{}, apperr.ErrLLMNodeNotFound
	}
	out := NodeDetail{Node: list[0], DeploymentEvents: []appdeploy.Event{}}
	if out.Events, err = s.NodeEvents(ctx, id, 50); err != nil {
		return NodeDetail{}, err
	}
	if dep := out.Node.DeploymentID; dep != nil && s.d.Apps != nil {
		if evs, err := s.d.Apps.Events(ctx, *dep, 50); err == nil && evs != nil {
			out.DeploymentEvents = evs
		}
	}
	return out, nil
}

// NodeEvents 节点事件，新的在前；limit <= 0 → 50。
func (s *Service) NodeEvents(ctx context.Context, id string, limit int) ([]NodeEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT id, node_id, at, from_state, to_state, detail FROM llm_node_events
		WHERE node_id = $1 ORDER BY id DESC LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (NodeEvent, error) {
		var e NodeEvent
		err := r.Scan(&e.ID, &e.NodeID, &e.At, &e.FromState, &e.ToState, &e.Detail)
		return e, err
	})
}

// PoolMachineInfo 机器池表的机器列。
type PoolMachineInfo struct {
	ID          int64  `json:"id"`
	Serial      string `json:"serial"`
	DisplayName string `json:"display_name"`
	State       string `json:"state"`
	AgentOnline bool   `json:"agent_online"`
}

// PoolNode 机器上的非终态节点（组节点：本机是其成员，Rank 为本机的 rank，Size 为组的台数；单机节点 Rank 0、Size 1）。
type PoolNode struct {
	ID        string `json:"id"`
	ModelSlug string `json:"model_slug"`
	State     string `json:"state"`
	Rank      int    `json:"rank"`
	Size      int    `json:"size"`
}

// PoolMachine 机器池的一行；LeaseUntil = 机器上非系统实例 reserved / active 租约的最晚结束时刻（「等待租约结束」）。
// FabricID / FabricName：机器所属的集群网络（计划 13；无 → null / ""）。
type PoolMachine struct {
	Machine    PoolMachineInfo `json:"machine"`
	Node       *PoolNode       `json:"node,omitempty"`
	LeaseUntil *time.Time      `json:"lease_until,omitempty"`
	FabricID   *int64          `json:"fabric_id"`
	FabricName string          `json:"fabric_name"`
}

// PoolFabric 机器池按集群网络的汇总（计划 13 §7.6）：Total = 池内属于该 fabric 的机器数，Free = 其中当前可建节点的；
// Groups["2"] / Groups["4"] = 用空闲且集群就绪（cluster_ready、Agent 支持集群）的机器按 (fabric, cluster_net) 能组出的副本数
// （direct 隔离只组 2 台）。
type PoolFabric struct {
	FabricID int64          `json:"fabric_id"`
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	RDMA     bool           `json:"rdma"`
	Total    int            `json:"total"`
	Free     int            `json:"free"`
	Groups   map[string]int `json:"groups"`
}

// PoolView GET /llm/pool：池机组的机器 + 节点 + 租约到期；Free = 当前可建节点的机器数（规格 §3.3），Total = 池内机器数；
// Fabrics 按 fabric ID 升序（池内没有 fabric 机器时为空数组）。
type PoolView struct {
	GroupID  int64         `json:"group_id"`
	Machines []PoolMachine `json:"machines"`
	Free     int           `json:"free"`
	Total    int           `json:"total"`
	Fabrics  []PoolFabric  `json:"fabrics"`
}

// candidateCond 可建节点的机器（规格 §3.3），参数 $1 = 池机组 ID、$2 = now、$3 = now + SystemLeaseHorizon。
// 计划 13：非终态组节点的成员机器（llm_node_members.live）也不是候选。
const candidateCond = `m.group_id = $1 AND m.state = 'idle' AND m.agent_online
	AND NOT EXISTS (SELECT 1 FROM instances i WHERE i.machine_id = m.id AND i.state IN ('creating', 'running', 'stopped', 'wiping'))
	AND NOT EXISTS (SELECT 1 FROM machine_leases l WHERE l.machine_id = m.id AND l.status IN ('reserved', 'active')
		AND l.period && tstzrange($2::timestamptz, $3::timestamptz))
	AND NOT EXISTS (SELECT 1 FROM llm_nodes ln WHERE ln.machine_id = m.id AND ln.state NOT IN ('removed', 'failed'))
	AND NOT EXISTS (SELECT 1 FROM llm_node_members lm WHERE lm.machine_id = m.id AND lm.live)`

// groupCond 在 candidateCond 之上：机器能进节点组（fleet 判定集群网络就绪，且 Agent 声明了 features.Cluster 位）。
const groupCond = candidateCond + ` AND m.fabric_id IS NOT NULL AND m.cluster_ready AND (m.agent_features & 2) <> 0`

// candidateMachines 只读地选出可建（单机）节点的机器 ID；PlaceSystem 在锁下复核。
// 排序 (m.fabric_id IS NOT NULL), m.id：优先用不属于任何 fabric 的机器，把能组队的机器留给多机节点组（M13）；无 fabric 时与计划 9 相同。
func (s *Service) candidateMachines(ctx context.Context, poolID int64, now time.Time, limit int) ([]int64, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT m.id FROM machines m WHERE `+candidateCond+` ORDER BY (m.fabric_id IS NOT NULL), m.id LIMIT $4`,
		poolID, now, now.Add(SystemLeaseHorizon), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// fabricCandidate 一个能建 N 台组的 (fabric, cluster_net)：MachineIDs 为其中空闲就绪机器（ID 升序）。
type fabricCandidate struct {
	FabricID   int64
	ClusterNet string
	MachineIDs []int64
}

// candidateFabrics 只读地列出能建 n 台组的 (fabric, cluster_net)（规格 §7.3 第 1 步）：池内空闲且集群就绪的机器按
// (fabric_id, cluster_net) 分组、count ≥ n、direct 隔离只给 n = 2；按 count ASC（最贴合）、fabric_id、cluster_net 排序。
// PlaceSystemCluster 在锁下复核。
func (s *Service) candidateFabrics(ctx context.Context, poolID int64, now time.Time, n int) ([]fabricCandidate, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT m.fabric_id, m.cluster_net, array_agg(m.id ORDER BY m.id)
		FROM machines m JOIN cluster_fabrics f ON f.id = m.fabric_id
		WHERE `+groupCond+`
		GROUP BY m.fabric_id, m.cluster_net, f.isolation
		HAVING count(*) >= $4 AND (f.isolation <> 'direct' OR $4 = 2)
		ORDER BY count(*), m.fabric_id, m.cluster_net`, poolID, now, now.Add(SystemLeaseHorizon), n)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (fabricCandidate, error) {
		var c fabricCandidate
		err := r.Scan(&c.FabricID, &c.ClusterNet, &c.MachineIDs)
		return c, err
	})
}

func (s *Service) PoolView(ctx context.Context) (PoolView, error) {
	poolID, err := s.d.Fleet.LLMPoolGroupID(ctx, s.d.Pool)
	if err != nil {
		return PoolView{}, err
	}
	now := s.d.Clock.Now()
	// 机器上的节点：组成员（live 成员行）优先，否则以本机为 machine_id 的非终态节点（单机节点）
	rows, err := s.d.Pool.Query(ctx, `SELECT m.id, m.serial, m.display_name, m.state, m.agent_online,
		n.id, md.slug, n.state, COALESCE(mm.rank, 0), n.size,
		(SELECT max(upper(l.period)) FROM machine_leases l JOIN instances i ON i.id = l.instance_id
			WHERE l.machine_id = m.id AND l.status IN ('reserved', 'active') AND NOT i.system),
		(`+candidateCond+`),
		(`+groupCond+`),
		m.fabric_id, COALESCE(f.name, ''), COALESCE(f.kind, ''), COALESCE(f.rdma, false), COALESCE(f.isolation, ''), m.cluster_net
		FROM machines m
		LEFT JOIN cluster_fabrics f ON f.id = m.fabric_id
		LEFT JOIN llm_node_members mm ON mm.machine_id = m.id AND mm.live
		LEFT JOIN llm_nodes n ON (mm.node_id IS NOT NULL AND n.id = mm.node_id)
			OR (mm.node_id IS NULL AND n.machine_id = m.id AND n.state NOT IN ('removed', 'failed'))
		LEFT JOIN llm_models md ON md.id = n.model_id
		WHERE m.group_id = $1 ORDER BY m.id`, poolID, now, now.Add(SystemLeaseHorizon))
	if err != nil {
		return PoolView{}, err
	}
	out := PoolView{GroupID: poolID, Machines: []PoolMachine{}, Fabrics: []PoolFabric{}}
	type netKey struct {
		fabric int64
		net    string
	}
	fabrics := map[int64]*PoolFabric{}
	isolation := map[int64]string{}
	groupFree := map[netKey]int{}
	for rows.Next() {
		var pm PoolMachine
		var nodeID, slug, state *string
		var rank, size *int
		var free, groupable, rdma bool
		var kind, iso, clusterNet string
		if err := rows.Scan(&pm.Machine.ID, &pm.Machine.Serial, &pm.Machine.DisplayName, &pm.Machine.State, &pm.Machine.AgentOnline,
			&nodeID, &slug, &state, &rank, &size, &pm.LeaseUntil, &free, &groupable,
			&pm.FabricID, &pm.FabricName, &kind, &rdma, &iso, &clusterNet); err != nil {
			rows.Close()
			return PoolView{}, err
		}
		if nodeID != nil {
			pm.Node = &PoolNode{ID: *nodeID, ModelSlug: *slug, State: *state, Rank: *rank, Size: *size}
		}
		if free {
			out.Free++
		}
		if pm.FabricID != nil {
			f := fabrics[*pm.FabricID]
			if f == nil {
				f = &PoolFabric{FabricID: *pm.FabricID, Name: pm.FabricName, Kind: kind, RDMA: rdma, Groups: map[string]int{"2": 0, "4": 0}}
				fabrics[*pm.FabricID] = f
				isolation[*pm.FabricID] = iso
			}
			f.Total++
			if free {
				f.Free++
			}
			if groupable {
				groupFree[netKey{*pm.FabricID, clusterNet}]++
			}
		}
		out.Machines = append(out.Machines, pm)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PoolView{}, err
	}
	out.Total = len(out.Machines)
	for k, cnt := range groupFree {
		f := fabrics[k.fabric]
		f.Groups["2"] += cnt / 2
		if isolation[k.fabric] != "direct" {
			f.Groups["4"] += cnt / 4
		}
	}
	for _, id := range slices.Sorted(maps.Keys(fabrics)) {
		out.Fabrics = append(out.Fabrics, *fabrics[id])
	}
	return out, nil
}
