package rental

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// 计划 13（T12）：后台集群详情 / 仪表盘用的只读查询。

// ClusterEvent 是集群某个成员实例（含换机前的旧成员）的一条状态变化。
type ClusterEvent struct {
	ID         int64     `json:"id"`
	InstanceID string    `json:"instance_id"`
	FromState  string    `json:"from_state"`
	ToState    string    `json:"to_state"`
	Detail     string    `json:"detail"`
	At         time.Time `json:"at"`
}

// ClusterEvents 返回集群全部成员实例（cluster_id = id，含已离开的）的最近 limit 条实例事件，按时间倒序合并；limit <= 0 → 50。
func (s *Service) ClusterEvents(ctx context.Context, clusterID string, limit int) ([]ClusterEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT e.id, e.instance_id, e.from_state, e.to_state, e.reason, e.at
		FROM instance_events e JOIN instances i ON i.id = e.instance_id
		WHERE i.cluster_id = $1 ORDER BY e.at DESC, e.id DESC LIMIT $2`, clusterID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ClusterEvent, error) {
		var e ClusterEvent
		err := r.Scan(&e.ID, &e.InstanceID, &e.FromState, &e.ToState, &e.Detail, &e.At)
		return e, err
	})
}

// ClusterCounts 仪表盘「集群」卡片：活跃集群数及其派生状态分布（用户集群与平台节点组都算）。
type ClusterCounts struct {
	Active   int `json:"active"`
	Forming  int `json:"forming"`
	Ready    int `json:"ready"`
	Degraded int `json:"degraded"`
}

// AdminClusterCounts 统计 state = active 的集群（派生状态与 ClusterView.Status 同一口径）。
func (s *Service) AdminClusterCounts(ctx context.Context) (ClusterCounts, error) {
	list, err := collectClusters(s.d.Pool.Query(ctx, `SELECT `+clusterCols+` FROM instance_clusters WHERE state = 'active' ORDER BY id`))
	if err != nil {
		return ClusterCounts{}, err
	}
	views, err := s.clusterViews(ctx, list)
	if err != nil {
		return ClusterCounts{}, err
	}
	var out ClusterCounts
	for _, v := range views {
		out.Active++
		switch v.Status {
		case ClusterForming:
			out.Forming++
		case ClusterReady:
			out.Ready++
		case ClusterDegraded:
			out.Degraded++
		}
	}
	return out, nil
}
