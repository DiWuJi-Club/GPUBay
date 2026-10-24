package llm

import (
	"context"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/rental"
)

// SeedReadyNode 仅供 e2e 钩子（internal/e2ehooks 的 /hooks/llm/node，计划 9 裁定「直接插 ready、不建部署」）与集成测试：
// 在 machineID 上一个已 running 的系统实例上直接插一个 ready 节点（deployment_id NULL、last_ready_at = now、随机节点密钥加密存储）
// 并写一条事件。生产路径（Controller / 后台）不调用它。
//
// 这种节点没有部署：Prober 跳过部署检查、直接经上游做 HTTP 探测，所以它的健康跟随假容器上的 FakeVLLM
// （探测失败照常计数 → unhealthy；恢复 → ready）。但它不能重建：后台「重建」返回 llm_node_invalid_state，
// unhealthy 之后 Controller 因没有部署可 Redeploy 而把它置为 failed 并释放实例。
// 实例不是该机器上 running 的系统实例 → apperr.ErrInvalidState；机器上已有非终态节点 → apperr.ErrMachineUnavailable。
func (s *Service) SeedReadyNode(ctx context.Context, modelID, machineID int64, instanceID string) (Node, error) {
	if _, err := s.GetModel(ctx, modelID); err != nil {
		return Node{}, err
	}
	_, enc, err := s.newNodeKey()
	if err != nil {
		return Node{}, err
	}
	var out Node
	err = db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instances WHERE id = $1 AND machine_id = $2 AND system AND state = $3)`,
			instanceID, machineID, rental.StateRunning).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return apperr.ErrInvalidState
		}
		now := s.d.Clock.Now()
		n, err := scanNode(tx.QueryRow(ctx, `INSERT INTO llm_nodes (id, model_id, machine_id, instance_id, state, api_key_enc, last_ready_at,
				created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $7) RETURNING `+nodeCols, newNodeID(), modelID, machineID, instanceID, NodeReady, enc, now))
		if err != nil {
			return err
		}
		out = n
		return insertNodeEvent(ctx, tx, n.ID, "", NodeReady, "e2e 钩子直接创建（无部署）", now)
	})
	if db.IsUniqueViolation(err) {
		return Node{}, apperr.ErrMachineUnavailable
	}
	if err != nil {
		return Node{}, err
	}
	return out, nil
}
