package rental

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

// ClusterListWindow：用户「我的集群」列出已结束集群的时长（ended_at 在此之内）。
const ClusterListWindow = 30 * 24 * time.Hour

const clusterCols = `id, user_id, system, size, fabric_id, mode, vlan, rdma, mtu, subnet_index, version, state, batch_id,
	ready_at, created_at, updated_at, ended_at`

func scanCluster(row pgx.Row) (Cluster, error) {
	var c Cluster
	err := row.Scan(&c.ID, &c.UserID, &c.System, &c.Size, &c.FabricID, &c.Mode, &c.VLAN, &c.RDMA, &c.MTU, &c.SubnetIndex,
		&c.Version, &c.State, &c.BatchID, &c.ReadyAt, &c.CreatedAt, &c.UpdatedAt, &c.EndedAt)
	if db.IsNoRows(err) {
		return Cluster{}, apperr.ErrClusterNotFound
	}
	return c, err
}

func collectClusters(rows pgx.Rows, err error) ([]Cluster, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Cluster, error) { return scanCluster(r) })
}

// lockCluster 锁集群行（多成员路径的第一把锁，规格 §14：集群行 FOR UPDATE → 成员实例按 ID 升序）。不存在 → ErrClusterNotFound。
func lockCluster(ctx context.Context, tx pgx.Tx, id string) (Cluster, error) {
	return scanCluster(tx.QueryRow(ctx, `SELECT `+clusterCols+` FROM instance_clusters WHERE id = $1 FOR UPDATE`, id))
}

// lockClusterMembers 按实例 ID 升序锁集群的在位成员（ClusterMemberStates）；调用方已持集群行锁。
func lockClusterMembers(ctx context.Context, tx pgx.Tx, clusterID string) ([]Instance, error) {
	return collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
		WHERE cluster_id = $1 AND state = ANY($2) ORDER BY id FOR UPDATE`, clusterID, ClusterMemberStates))
}

// clusterNetStatusSQL：成员在其机器最近一份 StateReport 里的集群网络状态（instances[].cluster；未上报为空串）。
const clusterNetStatusSQL = `COALESCE((SELECT x->>'cluster' FROM jsonb_array_elements(
		CASE WHEN jsonb_typeof(m.last_state_report->'instances') = 'array' THEN m.last_state_report->'instances' ELSE '[]'::jsonb END) x
	WHERE x->>'instance_id' = i.id LIMIT 1), '')`

// NetStatusOK 是成员集群网络就绪时 Agent 上报的 InstanceState.cluster。
const NetStatusOK = "ok"

// clusterView 组装集群视图（规格 §5.4）：活动集群列在位成员；已结束集群每个 rank 列最后一个实例（展示用）。
func (s *Service) clusterView(ctx context.Context, q db.Querier, c Cluster) (ClusterView, error) {
	v := ClusterView{Cluster: c, Members: []ClusterMemberView{}}
	var fabricRDMA bool
	if err := q.QueryRow(ctx, `SELECT name, kind, rdma FROM cluster_fabrics WHERE id = $1`, c.FabricID).
		Scan(&v.FabricName, &v.FabricKind, &fabricRDMA); err != nil {
		return ClusterView{}, err
	}
	subnet, err := s.clusterSubnet(c.SubnetIndex)
	if err != nil {
		return ClusterView{}, err
	}
	v.Subnet = subnet.String()
	v.HeadIP = clusterspec.MemberIP(subnet, 0).String()
	switch {
	case c.RDMA:
		v.Network = "rdma"
	case fabricRDMA:
		v.Network = "rdma_degraded"
	default:
		v.Network = "ethernet"
	}
	where, args := `i.cluster_id = $1 AND i.state = ANY($2)`, []any{c.ID, ClusterMemberStates}
	if c.State != ClusterActive {
		where, args = `i.cluster_id = $1 AND i.id = (SELECT j.id FROM instances j WHERE j.cluster_id = i.cluster_id AND j.cluster_rank = i.cluster_rank
			ORDER BY j.created_at DESC, j.id DESC LIMIT 1)`, []any{c.ID}
	}
	rows, err := q.Query(ctx, `SELECT i.cluster_rank, i.id, i.state, i.suspended_at IS NOT NULL, m.id,
			COALESCE(NULLIF(m.display_name, ''), '机器 #' || m.id), `+clusterNetStatusSQL+`, i.expires_at
		FROM instances i JOIN machines m ON m.id = i.machine_id
		WHERE `+where+` ORDER BY i.cluster_rank, i.id`, args...)
	if err != nil {
		return ClusterView{}, err
	}
	members, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ClusterMemberView, error) {
		var m ClusterMemberView
		err := r.Scan(&m.Rank, &m.InstanceID, &m.State, &m.Suspended, &m.MachineID, &m.MachineLabel, &m.NetStatus, &m.ExpiresAt)
		m.IP = clusterspec.MemberIP(subnet, m.Rank).String()
		return m, err
	})
	if err != nil {
		return ClusterView{}, err
	}
	v.Members = members
	v.Status = clusterStatus(c, members)
	return v, nil
}

// clusterStatus 派生集群状态（规格 §5.4）：failed / released 原样；ready_at 为空 → forming；
// size 个 rank 都有 running、未暂停、集群网络 ok 的在位成员 → ready；其余 → degraded。
func clusterStatus(c Cluster, members []ClusterMemberView) string {
	if c.State != ClusterActive {
		return c.State
	}
	if c.ReadyAt == nil {
		return ClusterForming
	}
	ok := make([]bool, c.Size)
	for _, m := range members {
		if m.Rank >= 0 && m.Rank < c.Size && m.State == StateRunning && !m.Suspended && m.NetStatus == NetStatusOK {
			ok[m.Rank] = true
		}
	}
	if slices.Contains(ok, false) {
		return ClusterDegraded
	}
	return ClusterReady
}

// GetClusterView 返回集群视图（后台 / 内部用，不校验归属）。不存在 → ErrClusterNotFound。
func (s *Service) GetClusterView(ctx context.Context, id string) (ClusterView, error) {
	c, err := scanCluster(s.d.Pool.QueryRow(ctx, `SELECT `+clusterCols+` FROM instance_clusters WHERE id = $1`, id))
	if err != nil {
		return ClusterView{}, err
	}
	return s.clusterView(ctx, s.d.Pool, c)
}

// ClusterForUser 返回用户自己的集群；别人的 / 系统集群 → ErrClusterNotFound。
func (s *Service) ClusterForUser(ctx context.Context, userID int64, id string) (ClusterView, error) {
	c, err := scanCluster(s.d.Pool.QueryRow(ctx, `SELECT `+clusterCols+` FROM instance_clusters
		WHERE id = $1 AND user_id = $2 AND NOT system`, id, userID))
	if err != nil {
		return ClusterView{}, err
	}
	return s.clusterView(ctx, s.d.Pool, c)
}

// ListClustersForUser 返回用户的集群：active + 最近 ClusterListWindow 内结束的，新的在前。
func (s *Service) ListClustersForUser(ctx context.Context, userID int64) ([]ClusterView, error) {
	list, err := collectClusters(s.d.Pool.Query(ctx, `SELECT `+clusterCols+` FROM instance_clusters
		WHERE user_id = $1 AND NOT system AND (state = 'active' OR ended_at >= $2)
		ORDER BY created_at DESC, id DESC`, userID, s.d.Clock.Now().Add(-ClusterListWindow)))
	if err != nil {
		return nil, err
	}
	return s.clusterViews(ctx, list)
}

var clusterStates = []string{ClusterActive, ClusterReleased, ClusterFailed}

// AdminListClusters 后台集群列表（新的在前，分页）。
func (s *Service) AdminListClusters(ctx context.Context, f ClusterFilter, p paging.Page) ([]ClusterView, int, error) {
	var w db.Where
	if f.State != "" {
		if !slices.Contains(clusterStates, f.State) {
			return nil, 0, apperr.BadRequest("集群状态不正确")
		}
		w.Add("state = ?", f.State)
	}
	if f.UserID != nil {
		w.Add("user_id = ?", *f.UserID)
	}
	if f.FabricID != nil {
		w.Add("fabric_id = ?", *f.FabricID)
	}
	if f.System != nil {
		w.Add("system = ?", *f.System)
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM instance_clusters WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	list, err := collectClusters(s.d.Pool.Query(ctx, `SELECT `+clusterCols+` FROM instance_clusters WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...))
	if err != nil {
		return nil, 0, err
	}
	views, err := s.clusterViews(ctx, list)
	return views, total, err
}

func (s *Service) clusterViews(ctx context.Context, list []Cluster) ([]ClusterView, error) {
	out := make([]ClusterView, 0, len(list))
	for _, c := range list {
		v, err := s.clusterView(ctx, s.d.Pool, c)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
