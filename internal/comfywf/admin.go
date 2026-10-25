package comfywf

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/paging"
)

// InstallRow 是后台「安装列表」的一行（GET /admin/api/v1/workflows/{id}/installs）：
// 部署上该工作流的一行 deployment_workflows，含已卸载 / 失联部署的历史行（DeploymentState 区分）。
// Phone 是部署所属用户的手机号（后台其它列表同样给完整号码）。
type InstallRow struct {
	DeploymentID    string     `json:"deployment_id"`
	DeploymentState string     `json:"deployment_state"`
	InstanceID      string     `json:"instance_id"`
	UserID          int64      `json:"user_id"`
	Phone           string     `json:"phone"`
	VersionID       int64      `json:"version_id"`
	Version         string     `json:"version"`
	State           string     `json:"state"` // pending / installed / failed
	AddedAt         time.Time  `json:"added_at"`
	InstalledAt     *time.Time `json:"installed_at"`
}

// ListInstalls 分页返回工作流的安装列表（按 added_at 倒序）与总数；工作流不存在 → workflow_not_found。
func (c *Catalog) ListInstalls(ctx context.Context, wfID int64, p paging.Page) ([]InstallRow, int, error) {
	if _, err := c.GetWorkflow(ctx, wfID); err != nil {
		return nil, 0, err
	}
	p = p.Normalize()
	var total int
	if err := c.pool.QueryRow(ctx, `SELECT count(*) FROM deployment_workflows WHERE workflow_id = $1`, wfID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := c.pool.Query(ctx, `SELECT dw.deployment_id, d.state, d.instance_id, d.user_id, u.phone, dw.workflow_version_id, v.version,
		dw.state, dw.added_at, dw.installed_at
		FROM deployment_workflows dw
		JOIN app_deployments d ON d.id = dw.deployment_id
		JOIN users u ON u.id = d.user_id
		JOIN comfy_workflow_versions v ON v.id = dw.workflow_version_id
		WHERE dw.workflow_id = $1
		ORDER BY dw.added_at DESC, dw.deployment_id LIMIT $2 OFFSET $3`, wfID, p.Limit(), p.Offset())
	if err != nil {
		return nil, 0, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (InstallRow, error) {
		var x InstallRow
		err := r.Scan(&x.DeploymentID, &x.DeploymentState, &x.InstanceID, &x.UserID, &x.Phone, &x.VersionID, &x.Version,
			&x.State, &x.AddedAt, &x.InstalledAt)
		return x, err
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []InstallRow{}
	}
	return items, total, nil
}

// AdminCounts 是后台仪表盘的两个数字：在用安装数（口径同 liveInstallsCond，全部工作流合计）与近 24 小时快速出图次数。
func (c *Catalog) AdminCounts(ctx context.Context, now time.Time) (installs, runs24h int, err error) {
	err = c.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM deployment_workflows dw JOIN app_deployments d ON d.id = dw.deployment_id WHERE `+liveInstallsCond+`),
		(SELECT count(*) FROM comfy_workflow_runs WHERE created_at > $1)`, now.Add(-24*time.Hour)).Scan(&installs, &runs24h)
	return installs, runs24h, err
}
