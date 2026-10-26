package appdeploy

import (
	"context"
	"errors"
	"slices"
	"time"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/rental"
)

// 集群部署的聚合状态（规格 §6.4）与读取。

// AggregateCluster 按成员部署聚合集群部署的状态（规格 §6.4，按顺序取第一条命中的）：
//
//	任一 failed → failed；任一 removing → removing；全部 removed / lost → removed；全部 running 且 health ok → running；
//	全部 stopped → stopped；有成员所在实例已不是集群当前成员（换机 / 成员没了）→ stale；其余 → installing。
//
// health：全员 ok → ok，有 unhealthy → unhealthy，否则 unknown。phase 取 head（rank0）的阶段，progress 取成员最小值。
// 没有成员 → removed（不会发生：集群部署至少有建时的成员部署）。
func AggregateCluster(members []ClusterMemberDeployment) (status, health, phase string, progress int) {
	if len(members) == 0 {
		return ClusterStatusRemoved, HealthUnknown, "", 0
	}
	all := func(ok func(m ClusterMemberDeployment) bool) bool {
		for _, m := range members {
			if !ok(m) {
				return false
			}
		}
		return true
	}
	anyOf := func(ok func(m ClusterMemberDeployment) bool) bool {
		return slices.ContainsFunc(members, ok)
	}
	progress = 100
	for _, m := range members {
		progress = min(progress, m.Deployment.Progress)
		if m.Role == RoleHead && phase == "" {
			phase = m.Deployment.Phase
		}
	}
	progress = max(progress, 0)
	switch {
	case all(func(m ClusterMemberDeployment) bool { return m.Deployment.Health == HealthOK }):
		health = HealthOK
	case anyOf(func(m ClusterMemberDeployment) bool { return m.Deployment.Health == HealthUnhealthy }):
		health = HealthUnhealthy
	default:
		health = HealthUnknown
	}
	switch {
	case anyOf(func(m ClusterMemberDeployment) bool { return m.Deployment.State == StateFailed }):
		status = ClusterStatusFailed
	case anyOf(func(m ClusterMemberDeployment) bool { return m.Deployment.State == StateRemoving }):
		status = ClusterStatusRemoving
	case all(func(m ClusterMemberDeployment) bool { return !m.Deployment.Live() }):
		status = ClusterStatusRemoved
	case all(func(m ClusterMemberDeployment) bool {
		return m.Deployment.State == StateRunning && m.Deployment.Health == HealthOK
	}):
		status = ClusterStatusRunning
	case all(func(m ClusterMemberDeployment) bool { return m.Deployment.State == StateStopped }):
		status = ClusterStatusStopped
	case anyOf(func(m ClusterMemberDeployment) bool { return !m.Current }):
		status = ClusterStatusStale
	default:
		status = ClusterStatusInstalling
	}
	return status, health, phase, progress
}

// acdCols 与 scanClusterDep 的字段顺序一致。
const acdCols = "id, cluster_id, user_id, app_id, app_version_id, state, created_at, updated_at"

func scanClusterDep(row interface{ Scan(...any) error }) (ClusterDeployment, error) {
	var c ClusterDeployment
	err := row.Scan(&c.ID, &c.ClusterID, &c.UserID, &c.AppID, &c.AppVersionID, &c.State, &c.CreatedAt, &c.UpdatedAt)
	if db.IsNoRows(err) {
		return ClusterDeployment{}, apperr.ErrNotFound
	}
	return c, err
}

// memberRef 是集群部署成员部署所在实例的集群信息。
type memberRef struct {
	rank    int  // instances.cluster_rank；实例已不在集群里（不应发生）为 -1
	current bool // 实例仍是集群当前（在位）成员
}

// fillClusterDeployment 读成员部署并聚合（q 可以是事务：集群操作在同一事务里返回改后的结果）。成员的取法（规格 §6.4 / §6.6）：
//   - 非终态的成员部署全部计入；
//   - lost（换机时旧实例上的部署）计入，作为「需要重新部署」的标记，直到 RedeployCluster 把它们改成 removed；
//   - removed 只在它所在 rank 没有「当前成员上的非终态部署」时计入（成员没了且未补上 → stale；全部卸载 → removed），
//     每个 rank 只取最新的一条。
//
// 成员按 rank、当前成员在前、创建时间排序。
func (s *Service) fillClusterDeployment(ctx context.Context, q db.Querier, c ClusterDeployment) (ClusterDeployment, error) {
	deps, err := collectDeployments(q.Query(ctx, `SELECT `+depCols+` FROM app_deployments WHERE cluster_deployment_id = $1 ORDER BY created_at, id`, c.ID))
	if err != nil {
		return ClusterDeployment{}, err
	}
	ids := make([]string, 0, len(deps))
	for _, d := range deps {
		ids = append(ids, d.InstanceID)
	}
	refs := map[string]memberRef{}
	rows, err := q.Query(ctx, `SELECT id, COALESCE(cluster_rank, -1), cluster_id IS NOT DISTINCT FROM $2 AND state = ANY($3)
		FROM instances WHERE id = ANY($1)`, ids, c.ClusterID, rental.ClusterMemberStates)
	if err != nil {
		return ClusterDeployment{}, err
	}
	for rows.Next() {
		var id string
		var r memberRef
		if err := rows.Scan(&id, &r.rank, &r.current); err != nil {
			rows.Close()
			return ClusterDeployment{}, err
		}
		refs[id] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ClusterDeployment{}, err
	}
	covered := map[int]bool{} // rank 上有当前成员的非终态部署
	for _, d := range deps {
		if r := refs[d.InstanceID]; d.Live() && r.current {
			covered[r.rank] = true
		}
	}
	newestRemoved := map[int]int{} // rank → deps 下标（deps 按创建时间升序，后者覆盖前者）
	for i, d := range deps {
		if r := refs[d.InstanceID]; d.State == StateRemoved && !covered[r.rank] {
			newestRemoved[r.rank] = i
		}
	}
	c.Members = []ClusterMemberDeployment{}
	for i, d := range deps {
		r := refs[d.InstanceID]
		switch {
		case d.Live(), d.State == StateLost:
		case d.State == StateRemoved && !covered[r.rank] && newestRemoved[r.rank] == i:
		default:
			continue
		}
		c.Members = append(c.Members, ClusterMemberDeployment{Rank: r.rank, Role: d.ClusterRole, InstanceID: d.InstanceID, Deployment: d, Current: r.current})
	}
	slices.SortStableFunc(c.Members, func(a, b ClusterMemberDeployment) int {
		if a.Rank != b.Rank {
			return a.Rank - b.Rank
		}
		if a.Current != b.Current {
			if a.Current {
				return -1
			}
			return 1
		}
		return a.Deployment.CreatedAt.Compare(b.Deployment.CreatedAt)
	})
	c.Status, c.Health, c.Phase, c.Progress = AggregateCluster(c.Members)
	return c, nil
}

func (s *Service) loadClusterDeployment(ctx context.Context, q db.Querier, id string) (ClusterDeployment, error) {
	c, err := scanClusterDep(q.QueryRow(ctx, `SELECT `+acdCols+` FROM app_cluster_deployments WHERE id = $1`, id))
	if err != nil {
		return ClusterDeployment{}, err
	}
	return s.fillClusterDeployment(ctx, q, c)
}

// GetClusterDeployment 读一条集群部署（含成员与聚合状态，不校验归属：后台 / llm 用）；不存在 → apperr.ErrNotFound（与部署不存在相同）。
func (s *Service) GetClusterDeployment(ctx context.Context, id string) (ClusterDeployment, error) {
	return s.loadClusterDeployment(ctx, s.d.Pool, id)
}

// LiveClusterDeployment 集群上 active 的集群部署（每个集群至多一条）；没有 → nil, nil。
func (s *Service) LiveClusterDeployment(ctx context.Context, clusterID string) (*ClusterDeployment, error) {
	c, err := scanClusterDep(s.d.Pool.QueryRow(ctx, `SELECT `+acdCols+` FROM app_cluster_deployments WHERE cluster_id = $1 AND state = 'active'`, clusterID))
	if errors.Is(err, apperr.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err = s.fillClusterDeployment(ctx, s.d.Pool, c)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ClusterDeploymentForUser 只返回该用户、该集群上的集群部署；不是主人或不在该集群上 → apperr.ErrNotFound。
func (s *Service) ClusterDeploymentForUser(ctx context.Context, userID int64, clusterID, id string) (ClusterDeployment, error) {
	c, err := s.GetClusterDeployment(ctx, id)
	if err != nil {
		return ClusterDeployment{}, err
	}
	if c.UserID != userID || c.ClusterID != clusterID {
		return ClusterDeployment{}, apperr.ErrNotFound
	}
	return c, nil
}

// finalizeClusterDeployments：rental 钩子（OnInstanceGone）把成员部署置终态后，若某集群部署已没有任何非终态成员部署
// （集群开通期中止 / 全员洗机），把它置为 removed，免得已结束的集群上留着 active 的集群部署。
// 锁序：调用方已持实例行与部署行锁；这里最后锁 app_cluster_deployments 行（与集群操作「集群 → 实例 → 部署 → 集群部署」同序）。
func finalizeClusterDeployments(ctx context.Context, q db.Querier, ids []string, now time.Time) error {
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		var state string
		if err := q.QueryRow(ctx, `SELECT state FROM app_cluster_deployments WHERE id = $1 FOR UPDATE`, id).Scan(&state); err != nil {
			return err
		}
		if state != ClusterDepActive {
			continue
		}
		if _, err := q.Exec(ctx, `UPDATE app_cluster_deployments SET state = 'removed', updated_at = $2 WHERE id = $1
			AND NOT EXISTS (SELECT 1 FROM app_deployments WHERE cluster_deployment_id = $1 AND state NOT IN ('removed', 'lost'))`, id, now); err != nil {
			return err
		}
	}
	return nil
}
