package comfywf

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
)

// Installed 是部署上的一个工作流（控制台 / 后台视图，前端 InstalledWorkflow，web/shared/src/types/workflows.ts）。
// 不含 graph / api_prompt 本体；QuickRun = 该版本带 api_prompt。
type Installed struct {
	WorkflowID      int64      `json:"workflow_id"`
	Slug            string     `json:"slug"`
	Name            string     `json:"name"`
	Category        string     `json:"category"`
	VersionID       int64      `json:"version_id"`
	Version         string     `json:"version"`
	State           string     `json:"state"` // pending / installed / failed
	LatestVersionID *int64     `json:"latest_version_id"`
	LatestVersion   string     `json:"latest_version"`
	UpdateAvailable bool       `json:"update_available"`
	QuickRun        bool       `json:"quick_run"`
	AddedAt         time.Time  `json:"added_at"`
	InstalledAt     *time.Time `json:"installed_at"`
}

// listInstalled 批量读多个部署的工作流集合，按部署分组、组内按 slug 排序。
//   - 「最新版本」= 工作流仍上架（published）时 published_at 最新的已发布版本（与 Catalog.LatestPublished 同序）；工作流下线后为空。
//   - UpdateAvailable：最新版本不是已装版本，且比已装版本发布得晚（已装的是更新的已下线版本时不提示「更新」成旧版本）。
func listInstalled(ctx context.Context, q db.Querier, depIDs []string) (map[string][]Installed, error) {
	out := map[string][]Installed{}
	if len(depIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT dw.deployment_id, dw.workflow_id, w.slug, w.name, w.category, dw.workflow_version_id, v.version, dw.state,
		lv.id, COALESCE(lv.version, ''), COALESCE(lv.id <> v.id AND (v.published_at IS NULL OR lv.published_at > v.published_at), false),
		v.api_prompt IS NOT NULL, dw.added_at, dw.installed_at
		FROM deployment_workflows dw
		JOIN comfy_workflows w ON w.id = dw.workflow_id
		JOIN comfy_workflow_versions v ON v.id = dw.workflow_version_id
		LEFT JOIN LATERAL (SELECT l.id, l.version, l.published_at FROM comfy_workflow_versions l
			WHERE l.workflow_id = w.id AND l.status = 'published' AND w.status = 'published'
			ORDER BY l.published_at DESC, l.id DESC LIMIT 1) lv ON true
		WHERE dw.deployment_id = ANY($1)
		ORDER BY dw.deployment_id, w.slug`, depIDs)
	if err != nil {
		return nil, err
	}
	type row struct {
		dep string
		Installed
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.dep, &x.WorkflowID, &x.Slug, &x.Name, &x.Category, &x.VersionID, &x.Version, &x.State,
			&x.LatestVersionID, &x.LatestVersion, &x.UpdateAvailable, &x.QuickRun, &x.AddedAt, &x.InstalledAt)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	for _, x := range list {
		out[x.dep] = append(out[x.dep], x.Installed)
	}
	return out, nil
}

// ListInstalled 返回部署上的工作流集合（按 slug）；没有 = 空切片（非 nil）。
func (h *Hooks) ListInstalled(ctx context.Context, depID string) ([]Installed, error) {
	m, err := listInstalled(ctx, h.pool, []string{depID})
	if err != nil {
		return nil, err
	}
	if ws := m[depID]; ws != nil {
		return ws, nil
	}
	return []Installed{}, nil
}

// WorkflowViews 实现 appdeploy.Deps.WorkflowViews：批量给出部署视图的 workflows 字段（Installed 的 JSON）。
// 没有工作流的部署不在结果里（appdeploy 自己补 []）。
func (h *Hooks) WorkflowViews(ctx context.Context, depIDs []string) (map[string][]json.RawMessage, error) {
	m, err := listInstalled(ctx, h.pool, depIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]json.RawMessage, len(m))
	for dep, ws := range m {
		raws := make([]json.RawMessage, 0, len(ws))
		for _, w := range ws {
			b, err := json.Marshal(w)
			if err != nil {
				return nil, err
			}
			raws = append(raws, b)
		}
		out[dep] = raws
	}
	return out, nil
}
