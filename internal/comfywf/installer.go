package comfywf

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/rental"
)

// comfyUISlug 是 ComfyUI 应用的 slug：只有它的部署能装工作流（与 appdeploy 视图的判断一致）。
const comfyUISlug = "comfyui"

// ---- 钩子（接到 appdeploy.Deps；不依赖 appdeploy.Service，先于它构造） ----

// Hooks 实现 appdeploy.Deps 的四个计划 11 扩展点：BundleExtra / OnTransition / WorkflowViews / ResolveWorkflows。
// 只读写 deployment_workflows 与工作流目录，不调 appdeploy.Service，所以可以在组装 appdeploy.Service 之前构造（见 Wire）。
type Hooks struct {
	pool *pgxpool.Pool
	cat  *Catalog
}

func NewHooks(pool *pgxpool.Pool, cat *Catalog) *Hooks { return &Hooks{pool: pool, cat: cat} }

// Wire 把四个钩子接到 appdeploy.Deps（在 appdeploy.NewService 之前调用）。
func (h *Hooks) Wire(d *appdeploy.Deps) {
	d.BundleExtra = h.BundleExtra
	d.OnTransition = h.OnTransition
	d.WorkflowViews = h.WorkflowViews
	d.ResolveWorkflows = h.ResolveWorkflows
}

// loadBundle 读部署的工作流集合及其版本内容（部署包用），按 slug 排序。
func loadBundle(ctx context.Context, q db.Querier, depID string) ([]BundleWorkflow, error) {
	rows, err := q.Query(ctx, `SELECT w.slug, v.version, v.graph, v.manifest FROM deployment_workflows dw
		JOIN comfy_workflows w ON w.id = dw.workflow_id JOIN comfy_workflow_versions v ON v.id = dw.workflow_version_id
		WHERE dw.deployment_id = $1 ORDER BY w.slug`, depID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (BundleWorkflow, error) {
		var w BundleWorkflow
		var graph, manifest []byte
		if err := r.Scan(&w.Slug, &w.Version, &graph, &manifest); err != nil {
			return w, err
		}
		w.Graph = compactJSON(graph)
		if err := decodeNumber(manifest, &w.Manifest); err != nil {
			return w, err
		}
		return w, nil
	})
}

// BundleExtra 实现 appdeploy.Deps.BundleExtra：支持工作流的版本一律给出 workflows/manifest.json（集合为空时 {"workflows":[]}，
// app.sh 据此删掉移除最后一个工作流后残留的 spark-*.json）与每个工作流的 workflows/<slug>.json；不支持的版本不追加文件。
func (h *Hooks) BundleExtra(ctx context.Context, d appdeploy.Deployment, ver appdeploy.Version) ([]appdeploy.BundleFile, error) {
	if !ver.Spec.SupportsWorkflows {
		return nil, nil
	}
	ws, err := loadBundle(ctx, h.pool, d.ID)
	if err != nil {
		return nil, err
	}
	return BundleFiles(ws)
}

// OnTransition 实现 appdeploy.Deps.OnTransition（每个部署的 running / failed 回写都会调，非 comfyui 部署没有行，是一条空 UPDATE）：
//   - running：pending / failed → installed（installed_at = now）。只在部署版本支持工作流时（否则包里没有 workflows/，什么也没装）。
//     failed 的也算：重新部署成功 = 集合里的全部工作流都装好了（规格 §2）。
//   - failed：pending → failed。
//
// d 是更新前的行（AppVersionID 即本次部署的版本）。
func (h *Hooks) OnTransition(ctx context.Context, tx pgx.Tx, d appdeploy.Deployment, to string, now time.Time) error {
	var err error
	switch to {
	case appdeploy.StateRunning:
		_, err = tx.Exec(ctx, `UPDATE deployment_workflows SET state = 'installed', installed_at = $2
			WHERE deployment_id = $1 AND state IN ('pending', 'failed')
			AND EXISTS (SELECT 1 FROM app_versions WHERE id = $3 AND COALESCE((spec->>'supports_workflows')::boolean, false))`,
			d.ID, now, d.AppVersionID)
	case appdeploy.StateFailed:
		_, err = tx.Exec(ctx, `UPDATE deployment_workflows SET state = 'failed' WHERE deployment_id = $1 AND state = 'pending'`, d.ID)
	}
	return err
}

// ResolveWorkflows 实现 appdeploy.Deps.ResolveWorkflows（下单预装）：去重（保持顺序）、≤ 8（workflow_limit）、
// 每个工作流已上架（workflow_not_found）且有已发布版本（workflow_unavailable，取 published_at 最新的），
// 整组能生成部署包（同名节点 commit 冲突 → workflow_version_invalid）。空 → nil。
func (h *Hooks) ResolveWorkflows(ctx context.Context, q db.Querier, ids []int64) ([]appdeploy.WorkflowRef, error) {
	uniq := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !slices.Contains(uniq, id) {
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return nil, nil
	}
	if len(uniq) > MaxWorkflowsPerDeployment {
		return nil, apperr.ErrWorkflowLimit
	}
	refs := make([]appdeploy.WorkflowRef, 0, len(uniq))
	bundle := make([]BundleWorkflow, 0, len(uniq))
	for _, id := range uniq {
		w, v, err := h.cat.installable(ctx, q, id, nil, apperr.ErrWorkflowNotFound)
		if err != nil {
			return nil, err
		}
		refs = append(refs, appdeploy.WorkflowRef{WorkflowID: w.ID, VersionID: v.ID})
		bundle = append(bundle, BundleWorkflow{Slug: w.Slug, Version: v.Version, Graph: v.Graph, Manifest: v.Manifest})
	}
	if _, err := BundleFiles(bundle); err != nil {
		return nil, err
	}
	return refs, nil
}

// ---- 安装服务 ----

// Installer 改 ComfyUI 部署上的工作流集合（规格 §5.3）：加入 / 换版本 / 移除后经 appdeploy.Service.RedeployInTx 重新部署
// （queued 的部署只改集合并 row_version + 1）。
type Installer struct {
	pool    *pgxpool.Pool
	clock   clock.Clock
	cat     *Catalog
	apps    *appdeploy.Service
	appsCat *appdeploy.Catalog
	hooks   *Hooks
	log     *slog.Logger
}

func NewInstaller(pool *pgxpool.Pool, clk clock.Clock, cat *Catalog, apps *appdeploy.Service, appsCat *appdeploy.Catalog, log *slog.Logger) *Installer {
	if log == nil {
		log = slog.Default()
	}
	return &Installer{pool: pool, clock: clk, cat: cat, apps: apps, appsCat: appsCat, hooks: NewHooks(pool, cat), log: log}
}

// AddRequest：VersionID nil = 最新已发布版本。
type AddRequest struct {
	UserID                   int64
	InstanceID, DeploymentID string
	WorkflowID               int64
	VersionID                *int64
	IP                       string
}

// UpdateRequest：VersionID nil = 最新已发布版本。
type UpdateRequest struct {
	UserID                   int64
	InstanceID, DeploymentID string
	WorkflowID               int64
	VersionID                *int64
	IP                       string
}

type RemoveRequest struct {
	UserID                   int64
	InstanceID, DeploymentID string
	WorkflowID               int64
	IP                       string
}

// Result 是集合修改后的部署（已重新排队，或 queued 时只 row_version + 1）与其工作流集合。
type Result struct {
	Deployment appdeploy.Deployment
	Workflows  []Installed
}

// ListInstalled 返回部署上的工作流集合（按 slug）；没有 = 空切片。
func (i *Installer) ListInstalled(ctx context.Context, depID string) ([]Installed, error) {
	return i.hooks.ListInstalled(ctx, depID)
}

// installable 取可安装的 {工作流, 版本}：工作流须已上架（否则 notPublished）；vid 给定 → 须是该工作流的版本（否则 workflow_not_found）
// 且已发布（否则 workflow_unavailable）；nil → published_at 最新的已发布版本（没有 → workflow_unavailable）。
func (c *Catalog) installable(ctx context.Context, q db.Querier, wfID int64, vid *int64, notPublished error) (Workflow, Version, error) {
	w, err := scanWorkflow(q.QueryRow(ctx, `SELECT `+wfCols+` FROM comfy_workflows WHERE id = $1`, wfID))
	if errors.Is(err, apperr.ErrNotFound) {
		return Workflow{}, Version{}, apperr.ErrWorkflowNotFound
	}
	if err != nil {
		return Workflow{}, Version{}, err
	}
	if w.Status != StatusPublished {
		return Workflow{}, Version{}, notPublished
	}
	if vid == nil {
		v, err := c.LatestPublished(ctx, q, wfID)
		return w, v, err
	}
	v, err := c.version(ctx, q, wfID, *vid, false)
	if errors.Is(err, apperr.ErrNotFound) {
		return Workflow{}, Version{}, apperr.ErrWorkflowNotFound
	}
	if err != nil {
		return Workflow{}, Version{}, err
	}
	if v.Status != StatusPublished {
		return Workflow{}, Version{}, apperr.ErrWorkflowUnavailable
	}
	return w, v, nil
}

// setChange 在事务里改集合（含事件与审计）；changed = false 表示无事可做（不重新部署、不改部署行）。
type setChange func(tx pgx.Tx, inst rental.Instance, d appdeploy.Deployment, now time.Time) (changed bool, err error)

// mutate 是 Add / Update / Remove 的公共流程（规格 §5.3，加锁顺序同 appdeploy 用户操作）：
// rental.LockOwned → appdeploy.LockDeployment（须在该实例上）→ 应用须是 comfyui（workflow_app_unsupported）→
// 状态 queued / running / stopped / failed（否则 app_invalid_state）→ 定目标应用版本 → fn 改集合 →
// 集合里 failed 的行回到 pending（都随这次部署重装）→ queued：只 row_version + 1；否则 RedeployInTx 换到目标版本、排队。
//
// needSupport（Add / Update）：目标版本必须支持工作流（queued 部署不换版本，当前版本就得支持）；
// 为 false（Remove）时找不到支持的版本也照删，只是不重新部署（包里本来就没有工作流）。
func (i *Installer) mutate(ctx context.Context, userID int64, instanceID, depID, ip string, needSupport bool, fn setChange) (Result, error) {
	now := i.clock.Now()
	var out appdeploy.Deployment
	err := db.WithTx(ctx, i.pool, func(tx pgx.Tx) error {
		inst, err := rental.LockOwned(ctx, tx, userID, instanceID)
		if err != nil {
			return err
		}
		d, err := appdeploy.LockDeployment(ctx, tx, depID)
		if err != nil {
			return err
		}
		if d.InstanceID != inst.ID {
			return apperr.ErrNotFound
		}
		app, err := i.appsCat.GetApp(ctx, d.AppID)
		if err != nil {
			return err
		}
		if app.Slug != comfyUISlug {
			return apperr.ErrWorkflowAppUnsupported
		}
		switch d.State {
		case appdeploy.StateQueued, appdeploy.StateRunning, appdeploy.StateStopped, appdeploy.StateFailed:
		default:
			return apperr.ErrAppInvalidState
		}
		redeploy := d.State != appdeploy.StateQueued
		var target int64
		if redeploy {
			target, err = i.targetVersion(ctx, d)
			if errors.Is(err, apperr.ErrWorkflowAppUnsupported) && !needSupport {
				redeploy, err = false, nil
			}
		} else if needSupport {
			err = i.requireSupport(ctx, d.AppVersionID)
		}
		if err != nil {
			return err
		}
		changed, err := fn(tx, inst, d, now)
		if err != nil || !changed {
			out = d
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE deployment_workflows SET state = 'pending' WHERE deployment_id = $1 AND state = 'failed'`, d.ID); err != nil {
			return err
		}
		if !redeploy {
			out, err = touchDeployment(ctx, tx, d.ID, now)
			return err
		}
		out, err = i.apps.RedeployInTx(ctx, tx, inst, d, appdeploy.RedeployRequest{UserID: userID, InstanceID: instanceID, DeploymentID: depID,
			VersionID: &target, EnvSet: false, IP: ip}, now)
		return err
	})
	if err != nil {
		return Result{}, err
	}
	ws, err := i.ListInstalled(ctx, out.ID)
	if err != nil {
		return Result{}, err
	}
	return Result{Deployment: out, Workflows: ws}, nil
}

// touchDeployment：queued 部署的集合变了 → row_version + 1。Runner 在事务外按 queued 行生成部署包，
// 行版本变了它就丢弃这一轮（包里是旧集合），下一轮按新集合重新打包（规格 §14）。
func touchDeployment(ctx context.Context, tx pgx.Tx, id string, now time.Time) (appdeploy.Deployment, error) {
	if err := appdeploy.TouchDeployment(ctx, tx, id, now); err != nil {
		return appdeploy.Deployment{}, err
	}
	return appdeploy.LockDeployment(ctx, tx, id)
}

// requireSupport：版本 spec.supports_workflows 为假 → workflow_app_unsupported。
func (i *Installer) requireSupport(ctx context.Context, versionID int64) error {
	v, err := i.appsCat.GetVersion(ctx, versionID)
	if err != nil {
		return err
	}
	if !v.Spec.SupportsWorkflows {
		return apperr.ErrWorkflowAppUnsupported
	}
	return nil
}

// targetVersion 定重新部署用的应用版本（规格 §3.4 / §5.3 第 3 步）：当前版本支持工作流且仍是已发布 → 不换（显式传当前版本 ID：
// RedeployRequest.VersionID 为 nil 意味着「最新已发布」）；否则（不支持，或支持但已下线）取 published 且支持工作流的版本里
// published_at 最新的；没有 → workflow_app_unsupported。
func (i *Installer) targetVersion(ctx context.Context, d appdeploy.Deployment) (int64, error) {
	cur, err := i.appsCat.GetVersion(ctx, d.AppVersionID)
	if err != nil {
		return 0, err
	}
	if cur.Spec.SupportsWorkflows && cur.Status == StatusPublished {
		return cur.ID, nil
	}
	vers, err := i.appsCat.ListVersions(ctx, d.AppID)
	if err != nil {
		return 0, err
	}
	var best *appdeploy.Version
	for k := range vers {
		v := &vers[k]
		if v.Status != StatusPublished || !v.Spec.SupportsWorkflows || v.PublishedAt == nil {
			continue
		}
		if best == nil || v.PublishedAt.After(*best.PublishedAt) || (v.PublishedAt.Equal(*best.PublishedAt) && v.ID > best.ID) {
			best = v
		}
	}
	if best == nil {
		return 0, apperr.ErrWorkflowAppUnsupported
	}
	return best.ID, nil
}

// addEvent 记一条部署事件（状态不变：集合修改本身不改部署状态；重新部署的事件由 RedeployInTx 另记）。
func addEvent(ctx context.Context, tx pgx.Tx, d appdeploy.Deployment, detail string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO app_deploy_events (deployment_id, at, from_state, to_state, detail) VALUES ($1, $2, $3, $3, $4)`,
		d.ID, now, d.State, detail)
	return err
}

// checkSet：改过的集合要能生成部署包（同名节点 commit / Git 冲突 → workflow_version_invalid），否则 Runner 才发现、部署 upload_failed。
func checkSet(ctx context.Context, tx pgx.Tx, depID string) error {
	ws, err := loadBundle(ctx, tx, depID)
	if err != nil {
		return err
	}
	_, err = BundleFiles(ws)
	return err
}

func (i *Installer) audit(ctx context.Context, tx pgx.Tx, userID int64, ip, action string, inst rental.Instance, now time.Time, diff map[string]any) error {
	return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry(action, "instance", inst.ID, diff))
}

// Add 把工作流加入部署的集合（pending）并重新部署（规格 §5.3）。
// 错误：workflow_not_found（不存在 / 未上架 / 版本不属于它）、workflow_unavailable（没有 / 指定的不是已发布版本）、workflow_exists、
// workflow_limit、workflow_app_unsupported、workflow_version_invalid（与集合里的节点冲突）、app_invalid_state，以及 RedeployInTx 的错误。
func (i *Installer) Add(ctx context.Context, req AddRequest) (Result, error) {
	return i.mutate(ctx, req.UserID, req.InstanceID, req.DeploymentID, req.IP, true,
		func(tx pgx.Tx, inst rental.Instance, d appdeploy.Deployment, now time.Time) (bool, error) {
			w, v, err := i.cat.installable(ctx, tx, req.WorkflowID, req.VersionID, apperr.ErrWorkflowNotFound)
			if err != nil {
				return false, err
			}
			var n int
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(bool_or(workflow_id = $2), false) FROM deployment_workflows WHERE deployment_id = $1`,
				d.ID, w.ID).Scan(&n, &exists); err != nil {
				return false, err
			}
			if exists {
				return false, apperr.ErrWorkflowExists
			}
			if n >= MaxWorkflowsPerDeployment {
				return false, apperr.ErrWorkflowLimit
			}
			if _, err := tx.Exec(ctx, `INSERT INTO deployment_workflows (deployment_id, workflow_id, workflow_version_id, state, added_at)
				VALUES ($1, $2, $3, 'pending', $4)`, d.ID, w.ID, v.ID, now); err != nil {
				return false, err
			}
			if err := checkSet(ctx, tx, d.ID); err != nil {
				return false, err
			}
			if err := addEvent(ctx, tx, d, "添加工作流 "+w.Name+" v"+v.Version, now); err != nil {
				return false, err
			}
			return true, i.audit(ctx, tx, req.UserID, req.IP, "instance.workflow_add", inst, now, map[string]any{
				"deployment": d.ID, "workflow_id": w.ID, "slug": w.Slug, "version_id": v.ID, "version": v.Version})
		})
}

// installedRow 读集合里的一行（FOR UPDATE；部署行已锁，这里只为语义清楚）；不在集合里 → workflow_not_found。
func installedRow(ctx context.Context, tx pgx.Tx, depID string, wfID int64) (DeploymentWorkflow, string, string, error) {
	var r DeploymentWorkflow
	var name, version string
	err := tx.QueryRow(ctx, `SELECT dw.deployment_id, dw.workflow_id, dw.workflow_version_id, dw.state, dw.added_at, dw.installed_at, w.name, v.version
		FROM deployment_workflows dw JOIN comfy_workflows w ON w.id = dw.workflow_id JOIN comfy_workflow_versions v ON v.id = dw.workflow_version_id
		WHERE dw.deployment_id = $1 AND dw.workflow_id = $2 FOR UPDATE OF dw`, depID, wfID).
		Scan(&r.DeploymentID, &r.WorkflowID, &r.VersionID, &r.State, &r.AddedAt, &r.InstalledAt, &name, &version)
	if db.IsNoRows(err) {
		return r, "", "", apperr.ErrWorkflowNotFound
	}
	return r, name, version, err
}

// Update 把集合里的工作流换到指定 / 最新已发布版本（pending）并重新部署。工作流已下线 → workflow_unavailable；
// 不在集合里 → workflow_not_found。目标就是当前版本：已装 / 待装 → 无操作（返回现状）；安装失败 → 当作重试（回到 pending 并重新部署）。
func (i *Installer) Update(ctx context.Context, req UpdateRequest) (Result, error) {
	return i.mutate(ctx, req.UserID, req.InstanceID, req.DeploymentID, req.IP, true,
		func(tx pgx.Tx, inst rental.Instance, d appdeploy.Deployment, now time.Time) (bool, error) {
			cur, _, curVer, err := installedRow(ctx, tx, d.ID, req.WorkflowID)
			if err != nil {
				return false, err
			}
			w, v, err := i.cat.installable(ctx, tx, req.WorkflowID, req.VersionID, apperr.ErrWorkflowUnavailable)
			if err != nil {
				return false, err
			}
			if v.ID == cur.VersionID && cur.State != WFStateFailed {
				return false, nil
			}
			if _, err := tx.Exec(ctx, `UPDATE deployment_workflows SET workflow_version_id = $3, state = 'pending', installed_at = NULL
				WHERE deployment_id = $1 AND workflow_id = $2`, d.ID, w.ID, v.ID); err != nil {
				return false, err
			}
			if err := checkSet(ctx, tx, d.ID); err != nil {
				return false, err
			}
			detail := "更新工作流 " + w.Name + " v" + curVer + " → v" + v.Version
			if v.ID == cur.VersionID {
				detail = "重新安装工作流 " + w.Name + " v" + v.Version
			}
			if err := addEvent(ctx, tx, d, detail, now); err != nil {
				return false, err
			}
			return true, i.audit(ctx, tx, req.UserID, req.IP, "instance.workflow_update", inst, now, map[string]any{
				"deployment": d.ID, "workflow_id": w.ID, "slug": w.Slug, "version_id": v.ID, "version": v.Version,
				"from_version_id": cur.VersionID, "from_version": curVer})
		})
}

// Remove 把工作流移出集合并重新部署（容器内删 spark-<slug>.json，模型与节点保留，D8）；queued 只删行。
// 不在集合里 → workflow_not_found。部署版本不支持工作流且没有可换的支持版本时只删行（不重新部署）。
func (i *Installer) Remove(ctx context.Context, req RemoveRequest) (Result, error) {
	return i.mutate(ctx, req.UserID, req.InstanceID, req.DeploymentID, req.IP, false,
		func(tx pgx.Tx, inst rental.Instance, d appdeploy.Deployment, now time.Time) (bool, error) {
			cur, name, curVer, err := installedRow(ctx, tx, d.ID, req.WorkflowID)
			if err != nil {
				return false, err
			}
			var slug string
			if err := tx.QueryRow(ctx, `DELETE FROM deployment_workflows dw USING comfy_workflows w
				WHERE dw.deployment_id = $1 AND dw.workflow_id = $2 AND w.id = dw.workflow_id RETURNING w.slug`, d.ID, req.WorkflowID).Scan(&slug); err != nil {
				return false, err
			}
			if err := addEvent(ctx, tx, d, "移除工作流 "+name, now); err != nil {
				return false, err
			}
			return true, i.audit(ctx, tx, req.UserID, req.IP, "instance.workflow_remove", inst, now, map[string]any{
				"deployment": d.ID, "workflow_id": req.WorkflowID, "slug": slug, "version_id": cur.VersionID, "version": curVer})
		})
}
