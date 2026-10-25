package appdeploy

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/rental"
)

// rental 钩子（规格 §5.4 / §6.4）。都在 rental 已持实例行锁（FOR UPDATE）的事务里调用，这里再锁部署行、再动 instance_ports
// （service.go 头部的加锁顺序）。钩子只处理平台自己写进去的数据，不因用户数据失败：用户输入只在 PrepareInstall（下单扣款之前）校验。

var _ rental.AppHooks = (*Service)(nil)

// lockLive 锁实例上的非终态部署（按创建时间）。
func lockLive(ctx context.Context, tx pgx.Tx, instanceID string) ([]Deployment, error) {
	return collectDeployments(tx.Query(ctx, `SELECT `+depCols+` FROM app_deployments
		WHERE instance_id = $1 AND state NOT IN ('removed', 'lost') ORDER BY created_at, id FOR UPDATE`, instanceID))
}

// OnInstanceRunning：installing / running / stopped 且 desired=running 的部署统一回 queued（容器停过，进程必然不在；deploy 幂等）。
// installing 也回 queued（裁定 P1）：安装中途停机，容器里的 _bg 已经没了，不回 queued 就只能等 timeout。
// failed 与用户停止的（desired=stopped 的 stopped）不动。其余仍需 Runner 处理的行（queued / removing / desired=stopped 的
// installing·running，以及停机时卸载、等清理目录的 removed AND cleanup_pending）把 next_poll_at 提到 now，下一轮就处理。
func (s *Service) OnInstanceRunning(ctx context.Context, tx pgx.Tx, instanceID string, now time.Time) error {
	deps, err := lockLive(ctx, tx, instanceID)
	if err != nil {
		return err
	}
	for _, d := range deps {
		if d.Desired != DesiredRunning || (d.State != StateInstalling && d.State != StateRunning && d.State != StateStopped) {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'queued', phase = '', progress = 0, message = '', health = 'unknown', error_code = '',
			unreachable_count = 0, crash_count = 0, next_poll_at = $2, finished_at = NULL, updated_at = $2, row_version = row_version + 1
			WHERE id = $1`, d.ID, now); err != nil {
			return err
		}
		if err := addEvent(ctx, tx, d.ID, d.State, StateQueued, "实例已启动，正在恢复应用", now); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE app_deployments SET next_poll_at = $2, updated_at = $2, row_version = row_version + 1
		WHERE instance_id = $1 AND next_poll_at > $2 AND (state IN ('queued', 'installing', 'running', 'removing') OR (state = 'removed' AND cleanup_pending))`,
		instanceID, now)
	return err
}

// OnInstanceGone：非终态部署 → removed(instance_gone)，事件 detail = reason；端口行按 deployment_id 删除（与卸载一致，§14 一致性检查）。
// 实例的容器 / 数据盘随之消失：停机时卸载、还等着清理目录的 removed 行也清掉 cleanup_pending。
func (s *Service) OnInstanceGone(ctx context.Context, tx pgx.Tx, instanceID, reason string, now time.Time) error {
	deps, err := lockLive(ctx, tx, instanceID)
	if err != nil {
		return err
	}
	for _, d := range deps {
		if err := s.markRemoved(ctx, tx, d, ErrCodeInstanceGone, reason, now); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE app_deployments SET cleanup_pending = false, updated_at = $2, row_version = row_version + 1
		WHERE instance_id = $1 AND state = 'removed' AND cleanup_pending`, instanceID, now)
	return err
}

// OnInstanceMigrated（规格 §6.4）：旧实例非终态部署 → lost（事件「实例已更换机器」），旧端口行不再指向它们（deployment_id 置 NULL，裁定 P7）。
// state ≠ failed 且 desired ≠ removed 的复制到新实例（attempt 0，同版本 / 变量 / 密文 / 端口 / spec_hash，连同工作流集合，置 pending）：
//   - desired = running → queued，新实例 running 后部署；
//   - desired = stopped → stopped / stopped（用户停的不自动启动；「启动」走 Start → queued 的完整部署）。
//
// 新实例的端口行已由 AdminMigrate 复制（deployment_id NULL），复制过去的部署 running 时由 Runner 认领；
// 不复制的部署（failed / 正在卸载）在旧实例上占的端口，在新实例上对应的复制行删除（否则成了无主的孤儿端口，
// 之后在新实例上重新部署该应用会撞 port_conflict）。
func (s *Service) OnInstanceMigrated(ctx context.Context, tx pgx.Tx, oldID, newID string, now time.Time) error {
	deps, err := lockLive(ctx, tx, oldID)
	if err != nil {
		return err
	}
	for _, d := range deps {
		copied := d.State != StateFailed && d.Desired != DesiredRemoved
		if !copied {
			if err := s.dropCopiedPorts(ctx, tx, d, newID, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'lost', health = 'unknown', finished_at = $2, updated_at = $2, row_version = row_version + 1
			WHERE id = $1`, d.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE instance_ports SET deployment_id = NULL, updated_at = $2 WHERE deployment_id = $1`, d.ID, now); err != nil {
			return err
		}
		if err := addEvent(ctx, tx, d.ID, d.State, StateLost, "实例已更换机器", now); err != nil {
			return err
		}
		if !copied {
			continue
		}
		if err := cancelStaleCleanup(ctx, tx, newID, d.AppID, now); err != nil {
			return err
		}
		state, desired, detail := StateQueued, DesiredRunning, "从 "+oldID+" 迁移，模型需重新下载"
		if d.Desired == DesiredStopped {
			state, desired, detail = StateStopped, DesiredStopped, "从 "+oldID+" 迁移（已停止，启动时重新下载模型）"
		}
		nd, err := scanDeployment(tx.QueryRow(ctx, `INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, desired, env, secret_env_enc, port, spec_hash, next_poll_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12, $12) RETURNING `+depCols,
			randx.ID("dep-", 12), newID, d.UserID, d.AppID, d.AppVersionID, state, desired, mustJSON(d.Env), d.SecretEnvEnc, d.Port, d.SpecHash, now))
		if err != nil {
			return err
		}
		if err := addEvent(ctx, tx, nd.ID, "", state, detail, now); err != nil {
			return err
		}
		// 计划 11 D14：工作流集合随部署复制，全部回到 pending（新容器里要重新装）
		if _, err := tx.Exec(ctx, `INSERT INTO deployment_workflows (deployment_id, workflow_id, workflow_version_id, state, added_at)
			SELECT $1, workflow_id, workflow_version_id, 'pending', $3 FROM deployment_workflows WHERE deployment_id = $2`, nd.ID, d.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// dropCopiedPorts 删除新实例上「复制自旧部署 d 所占端口」的行（d 不迁移时用）；以部署主人审计 instance.port_delete。
func (s *Service) dropCopiedPorts(ctx context.Context, tx pgx.Tx, d Deployment, newID string, now time.Time) error {
	rows, err := tx.Query(ctx, `DELETE FROM instance_ports WHERE instance_id = $1 AND deployment_id IS NULL
		AND port IN (SELECT port FROM instance_ports WHERE deployment_id = $2) RETURNING port`, newID, d.ID)
	if err != nil {
		return err
	}
	ports, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		return nil
	}
	name, err := appName(ctx, tx, d.AppID)
	if err != nil {
		return err
	}
	for _, p := range ports {
		if err := audit.Record(ctx, tx, now, audit.Actor{UserID: d.UserID}.Entry("instance.port_delete", "instance", newID,
			map[string]any{"port": p, "deployment": d.ID, "reason": "换机：应用 " + name + " 未迁移"})); err != nil {
			return err
		}
	}
	return nil
}

// installPlan 是 PrepareInstall 校验通过的预装计划（变量已补默认值并拆成普通 / 敏感）。
type installPlan struct {
	app       App
	ver       Version
	plain     map[string]string
	secret    map[string]string
	Workflows []WorkflowRef // 计划 11：预装的工作流集合（ResolveWorkflows 的结果）
}

// dedupeRefs 按 WorkflowID 去重（保留第一个）。ResolveWorkflows 约定已去重；这里防御，免得重复让开通事务撞主键整单失败。
func dedupeRefs(refs []WorkflowRef) []WorkflowRef {
	seen := map[int64]bool{}
	out := make([]WorkflowRef, 0, len(refs))
	for _, r := range refs {
		if !seen[r.WorkflowID] {
			seen[r.WorkflowID] = true
			out = append(out, r)
		}
	}
	return out
}

// PrepareInstall 下单预装校验：published 的非内部应用的最新 published 版本、镜像兼容、变量；workflowIDs 非空时（计划 11）
// 应用须是 comfyui、版本须支持工作流且接了 Deps.ResolveWorkflows（否则 workflow_app_unsupported），由它解析出集合。错误都是 4xx 的 apperr
// （app_not_found / app_unavailable / app_image_mismatch / app_env_* / workflow_*），rental 在扣款前整单失败。
func (s *Service) PrepareInstall(ctx context.Context, q db.Querier, appID, imageID int64, env map[string]string, workflowIDs []int64) (rental.InstallPlan, error) {
	// 内部应用不能随下单预装（系统实例由 llm.Controller 直接 Create）
	app, ver, err := s.resolveVersion(ctx, q, appID, nil, false)
	if err != nil {
		return nil, err
	}
	if err := s.checkImage(ctx, q, ver, imageID); err != nil {
		return nil, err
	}
	plain, secret, err := ValidateEnv(ver.Spec, env)
	if err != nil {
		return nil, err
	}
	plan := &installPlan{app: app, ver: ver, plain: plain, secret: secret}
	if len(workflowIDs) > 0 {
		if app.Slug != comfyUISlug || !ver.Spec.SupportsWorkflows || s.d.ResolveWorkflows == nil {
			return nil, apperr.ErrWorkflowAppUnsupported
		}
		refs, err := s.d.ResolveWorkflows(ctx, q, workflowIDs)
		if err != nil {
			return nil, err
		}
		plan.Workflows = dedupeRefs(refs)
	}
	return plan, nil
}

// QueueInstall 为刚插入的实例写一条 queued 部署（实例 running 后 Runner 执行），以实例主人审计 instance.app_deploy；
// 计划带工作流时写入 deployment_workflows（pending）并记事件「预装工作流 N 个」。
func (s *Service) QueueInstall(ctx context.Context, tx pgx.Tx, inst rental.Instance, plan rental.InstallPlan, now time.Time) error {
	p, ok := plan.(*installPlan)
	if !ok || p == nil {
		return fmt.Errorf("appdeploy: QueueInstall got %T, want a plan from PrepareInstall", plan)
	}
	d, err := s.insert(ctx, tx, inst, inst.UserID, p.app, p.ver, p.plain, p.secret, now)
	if err != nil {
		return err
	}
	if err := addEvent(ctx, tx, d.ID, "", StateQueued, "预装："+p.app.Name+" "+p.ver.Version+"，实例开通后自动部署", now); err != nil {
		return err
	}
	if len(p.Workflows) > 0 {
		for _, w := range p.Workflows {
			if _, err := tx.Exec(ctx, `INSERT INTO deployment_workflows (deployment_id, workflow_id, workflow_version_id, state, added_at)
				VALUES ($1, $2, $3, 'pending', $4)`, d.ID, w.WorkflowID, w.VersionID, now); err != nil {
				return err
			}
		}
		if err := addEvent(ctx, tx, d.ID, StateQueued, StateQueued, fmt.Sprintf("预装工作流 %d 个", len(p.Workflows)), now); err != nil {
			return err
		}
	}
	detail := map[string]any{"deployment": d.ID, "app": p.app.Slug, "version_id": p.ver.ID, "preinstall": true}
	if len(p.Workflows) > 0 {
		ids := make([]int64, len(p.Workflows))
		for i, w := range p.Workflows {
			ids[i] = w.WorkflowID
		}
		detail["workflow_ids"] = ids
	}
	return audit.Record(ctx, tx, now, audit.Actor{UserID: inst.UserID}.Entry("instance.app_deploy", "instance", inst.ID, detail))
}
