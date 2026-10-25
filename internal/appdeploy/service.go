// 部署服务：用户操作（创建 / 重新部署 / 停止 / 启动 / 卸载 / 日志 / 事件）与后台查询 / 强制卸载。
//
// 加锁顺序（规格 §14，所有路径一律）：instances 行 → app_deployments 行 → instance_ports。
//   - rental 钩子：在已持 instances 行锁（FOR UPDATE）的事务里锁 app_deployments 行。
//   - 用户操作：rental.LockOwned（instances FOR UPDATE）→ LockDeployment；后台强制卸载同序（instances FOR UPDATE → 部署）。
//   - Runner 回写：先取 instances 行 FOR KEY SHARE，再锁部署行。不能只锁部署行：
//     插入 instance_ports 的外键检查会对 instances 行取 KEY SHARE，与持实例 FOR UPDATE、等部署行锁的用户操作成环（死锁）。
//   - instance_ports 的插入 / 认领 / 删除都在持部署行锁之后（registerPorts 的前置条件）。
//
// 每个写 app_deployments 行的 UPDATE 都 row_version = row_version + 1：Runner 回写按 row_version 判断 exec 期间行是否被改过。

package appdeploy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/rental"
)

type Deps struct {
	Pool     *pgxpool.Pool
	Clock    clock.Clock
	Box      *secretbox.Box
	Log      *slog.Logger
	Catalog  *Catalog
	Images   *catalog.Service
	Rental   *rental.Service
	Fleet    *fleet.Service
	Upstream *upstream.Pool                           // 实时读日志；nil → 只返回 log_tail
	PortURL  func(instanceID string, port int) string // 端口访问 URL（app 传 ingressAuth.PortURL）；nil → URL 恒为空
	AppRoot  string                                   // 容器内应用根目录；空 = DefaultAppRoot
	// AppRootFor 按实例给出应用根目录（测试 / e2e 多实例共用一个假容器时各用各的目录）；nil → AppRoot。
	AppRootFor              func(instanceID string) string
	HFEndpoint, PipIndexURL string
	// SystemUserID 系统账号（users.system，计划 9）：只有它能部署内部应用；0 = 未配置，谁都不能部署内部应用。
	SystemUserID int64
	GitMirror    string // 注入 SPARK_GIT_MIRROR（计划 11，配置 SPARK_APP_GIT_MIRROR；空 = 直连）

	// 计划 11 扩展点（comfywf 实现、app.go 接线；appdeploy 不 import comfywf）。都可为 nil。

	// BundleExtra 给支持工作流的版本（Spec.SupportsWorkflows）追加部署包文件；apperr 错误按部署包生成失败处理（upload_failed），
	// 其余错误视为临时错误，部署留在原状态由 Runner 下一轮重试。
	BundleExtra func(ctx context.Context, d Deployment, ver Version) ([]BundleFile, error)
	// OnTransition 在 Runner 把部署写成 running（stepPoll）或 failed（markFailed）的同一回写事务里调用；d 是更新前的行，to 为目标状态。
	OnTransition func(ctx context.Context, tx pgx.Tx, d Deployment, to string, now time.Time) error
	// WorkflowViews 批量给出部署视图的 workflows 字段（按部署 ID）；只对 comfyui / 支持工作流的部署调用。
	WorkflowViews func(ctx context.Context, depIDs []string) (map[string][]json.RawMessage, error)
	// ResolveWorkflows 下单预装工作流时把工作流 ID 解析成 {工作流, 版本}（校验上架、去重、数量）；nil = 不支持预装工作流。
	// 约定：返回的 WorkflowID 互不相同（QueueInstall 仍防御性去重，保留第一个）。
	ResolveWorkflows func(ctx context.Context, q db.Querier, ids []int64) ([]WorkflowRef, error)
}

type Service struct {
	d Deps
}

// instanceSSHPort 是实例容器 sshd 的端口，与接入层（sshgw / ingress）同源：rental 按实例算；无 Rental（单测）→ 0 = 上游池默认端口。
func (s *Service) instanceSSHPort(instanceID string) int {
	if s.d.Rental == nil {
		return 0
	}
	return s.d.Rental.InstanceSSHPort(instanceID)
}

func NewService(d Deps) *Service {
	if d.AppRoot == "" {
		d.AppRoot = DefaultAppRoot
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Service{d: d}
}

// AppRoot 返回实例容器内的应用根目录。
func (s *Service) AppRoot(instanceID string) string {
	if s.d.AppRootFor != nil {
		return s.d.AppRootFor(instanceID)
	}
	return s.d.AppRoot
}

// ---- 创建 ----

type CreateRequest struct {
	UserID     int64
	InstanceID string
	AppID      int64
	VersionID  *int64 // nil = 最新已发布版本
	Env        map[string]string
	IP         string
}

// Create 在实例上新建一条 queued 部署（规格 §6.2「部署（新）」）。实例非 running 时 Runner 等它 running 再执行。
func (s *Service) Create(ctx context.Context, req CreateRequest) (Deployment, error) {
	now := s.d.Clock.Now()
	var out Deployment
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := rental.LockOwned(ctx, tx, req.UserID, req.InstanceID)
		if err != nil {
			return err
		}
		switch inst.State {
		case rental.StateReserved, rental.StateCreating, rental.StateRunning, rental.StateStopped:
		default:
			return apperr.ErrInvalidState
		}
		// 内部应用只有系统账号能部署，对其他用户按不存在处理
		isSystem := s.d.SystemUserID != 0 && req.UserID == s.d.SystemUserID
		app, ver, err := s.resolveVersion(ctx, tx, req.AppID, req.VersionID, isSystem)
		if err != nil {
			return err
		}
		if err := s.checkImage(ctx, tx, ver, inst.ImageID); err != nil {
			return err
		}
		var live int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM app_deployments WHERE instance_id = $1 AND state NOT IN ('removed', 'lost')`, inst.ID).Scan(&live); err != nil {
			return err
		}
		if live >= MaxAppsPerInstance {
			return apperr.ErrAppLimit
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_deployments WHERE instance_id = $1 AND app_id = $2 AND state NOT IN ('removed', 'lost'))`,
			inst.ID, app.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return apperr.ErrAppExists
		}
		if err := s.checkPorts(ctx, tx, inst.ID, ver.Spec.Ports, ""); err != nil {
			return err
		}
		plain, secret, err := ValidateEnv(ver.Spec, req.Env)
		if err != nil {
			return err
		}
		if out, err = s.insert(ctx, tx, inst, req.UserID, app, ver, plain, secret, now); err != nil {
			return err
		}
		if err := addEvent(ctx, tx, out.ID, "", StateQueued, "创建部署："+app.Name+" "+ver.Version, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: req.UserID, IP: req.IP}.Entry("instance.app_deploy", "instance", inst.ID,
			map[string]any{"deployment": out.ID, "app": app.Slug, "version_id": ver.ID}))
	})
	if db.IsUniqueViolation(err) { // 并发创建同一应用：app_deployments_live_uniq
		return Deployment{}, apperr.ErrAppExists
	}
	if err != nil {
		return Deployment{}, err
	}
	return out, nil
}

// checkPorts：spec 端口不得与实例现有端口（用户端口与其它部署已登记的）或其它非终态部署声明的端口冲突；
// 登记后总数（现有端口 ∪ 其它部署将登记的端口，再加本 spec 的）≤ 5。exclude 为重新部署时自己的 ID（自己的端口行不算占用）。
func (s *Service) checkPorts(ctx context.Context, q db.Querier, instanceID string, ports []PortSpec, exclude string) error {
	rows, err := q.Query(ctx, `SELECT port, deployment_id FROM instance_ports WHERE instance_id = $1`, instanceID)
	if err != nil {
		return err
	}
	taken := map[int]bool{}
	for rows.Next() {
		var port int
		var dep *string
		if err := rows.Scan(&port, &dep); err != nil {
			rows.Close()
			return err
		}
		if exclude == "" || dep == nil || *dep != exclude {
			taken[port] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	declared, err := livePortsExcept(ctx, q, instanceID, exclude)
	if err != nil {
		return err
	}
	for _, p := range declared {
		taken[p] = true
	}
	for _, p := range ports {
		if taken[p.Port] {
			return apperr.AppPortConflict(p.Port)
		}
	}
	if len(taken)+len(ports) > rental.MaxPortsPerInstance {
		return apperr.ErrPortLimit
	}
	return nil
}

// ---- 用户对已有部署的操作 ----

// userOp 先锁实例（校验归属）再锁部署行（须在该实例上，否则 not_found），在同一事务里执行 fn。
func (s *Service) userOp(ctx context.Context, userID int64, instanceID, depID string,
	fn func(tx pgx.Tx, inst rental.Instance, d Deployment, now time.Time) (Deployment, error)) (Deployment, error) {
	now := s.d.Clock.Now()
	var out Deployment
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := rental.LockOwned(ctx, tx, userID, instanceID)
		if err != nil {
			return err
		}
		d, err := LockDeployment(ctx, tx, depID)
		if err != nil {
			return err
		}
		if d.InstanceID != inst.ID {
			return apperr.ErrNotFound
		}
		out, err = fn(tx, inst, d, now)
		return err
	})
	if err != nil {
		return Deployment{}, err
	}
	return out, nil
}

// auditOp 记用户侧部署操作的审计（只含部署 ID、应用 slug、版本 ID，不含变量值）。
func auditOp(ctx context.Context, q db.Querier, now time.Time, actor audit.Actor, action string, d Deployment) error {
	var slug string
	if err := q.QueryRow(ctx, `SELECT slug FROM apps WHERE id = $1`, d.AppID).Scan(&slug); err != nil {
		return err
	}
	return audit.Record(ctx, q, now, actor.Entry(action, "instance", d.InstanceID,
		map[string]any{"deployment": d.ID, "app": slug, "version_id": d.AppVersionID}))
}

type RedeployRequest struct {
	UserID                   int64
	InstanceID, DeploymentID string
	VersionID                *int64 // nil = 最新已发布版本
	Env                      map[string]string
	EnvSet                   bool // false：变量全部沿用；true：普通变量以 Env 为准，secret 未出现沿用、出现空串清除
	IP                       string
}

// mergeEnv 按重新部署的规则合并变量。沿用的旧值只进入目标版本仍以同类声明的变量：
// 旧 secret 只沿用到仍是 secret 的变量（变成普通变量后不得以明文写进 env 列）；旧普通值只沿用到仍是普通类型的变量。
func mergeEnv(spec Spec, oldPlain, oldSecret map[string]string, req map[string]string, envSet bool) map[string]string {
	isSecret := map[string]bool{}
	declared := map[string]bool{}
	for _, e := range spec.Env {
		declared[e.Name] = true
		isSecret[e.Name] = e.Type == "secret"
	}
	merged := map[string]string{}
	if envSet {
		for k, v := range req {
			merged[k] = v
		}
	} else {
		for k, v := range oldPlain {
			if declared[k] && !isSecret[k] {
				merged[k] = v
			}
		}
	}
	for k, v := range oldSecret {
		if !isSecret[k] {
			continue
		}
		if nv, given := merged[k]; given && envSet {
			if nv == "" {
				delete(merged, k) // 传空串 = 清除
			}
			continue
		}
		merged[k] = v
	}
	return merged
}

func portNumbers(ps []PortSpec) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = p.Port
	}
	return out
}

// Redeploy 重新部署（规格 §6.2）：running / stopped / failed → queued（可换版本、可改变量，attempt 不变）；
// 已是 queued → 只更新变量（EnvSet）并返回当前行，连点两次等价于一次。
func (s *Service) Redeploy(ctx context.Context, req RedeployRequest) (Deployment, error) {
	return s.userOp(ctx, req.UserID, req.InstanceID, req.DeploymentID, func(tx pgx.Tx, inst rental.Instance, d Deployment, now time.Time) (Deployment, error) {
		return s.RedeployInTx(ctx, tx, inst, d, req, now)
	})
}

// RedeployInTx 是 Redeploy 的事务内部分（计划 11：comfywf 改工作流集合后在同一事务里重新部署）。
// 调用方须已按加锁顺序持实例行锁（rental.LockOwned）与部署行锁（LockDeployment）；d 不在 inst 上 → not_found。
// 审计操作人由 req.UserID / req.IP 构造。行为与 Redeploy 相同。
//
// 注意：d 为 queued 且 !req.EnvSet 时什么都不做，**不** bump row_version（Runner 可能正拿着旧输入在打包 / exec）。
// 调用方若改了部署包的其它输入（如工作流集合），须另调 TouchDeployment，Runner 才会丢弃本轮、按新输入重新打包。
// VersionID 为 nil 表示「最新已发布版本」而不是「不换」；要保持当前版本须传 &d.AppVersionID（该版本须仍是 published）。
func (s *Service) RedeployInTx(ctx context.Context, tx pgx.Tx, inst rental.Instance, d Deployment, req RedeployRequest, now time.Time) (Deployment, error) {
	if d.InstanceID != inst.ID {
		return Deployment{}, apperr.ErrNotFound
	}
	actor := audit.Actor{UserID: req.UserID, IP: req.IP}
	oldSecret, err := openSecrets(s.d.Box, d.SecretEnvEnc)
	if err != nil {
		return Deployment{}, err
	}
	cur, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionCols+` FROM app_versions WHERE id = $1`, d.AppVersionID))
	if err != nil {
		return Deployment{}, err
	}
	switch d.State {
	case StateQueued:
		if !req.EnvSet {
			return d, nil
		}
		plain, secret, err := ValidateEnv(cur.Spec, mergeEnv(cur.Spec, d.Env, oldSecret, req.Env, true))
		if err != nil {
			return Deployment{}, err
		}
		out, err := scanDeployment(tx.QueryRow(ctx, `UPDATE app_deployments SET env = $2, secret_env_enc = $3, updated_at = $4, row_version = row_version + 1
			WHERE id = $1 RETURNING `+depCols, d.ID, mustJSON(plain), sealSecrets(s.d.Box, secret), now))
		if err != nil {
			return Deployment{}, err
		}
		return out, auditOp(ctx, tx, now, actor, "instance.app_redeploy", out)
	case StateRunning, StateStopped, StateFailed:
	default:
		return Deployment{}, apperr.ErrAppInvalidState
	}
	// 显式指定部署自己的当前版本 = 原样重新部署，即使该版本已下线（计划 11：改工作流集合后不换应用版本）；
	// 其它显式版本仍须已发布（pickVersion）。
	ver := cur
	if req.VersionID == nil || *req.VersionID != d.AppVersionID {
		if ver, err = s.pickVersion(ctx, tx, d.AppID, req.VersionID); err != nil {
			return Deployment{}, err
		}
	}
	if err := s.checkImage(ctx, tx, ver, inst.ImageID); err != nil {
		return Deployment{}, err
	}
	if !slices.Equal(portNumbers(cur.Spec.Ports), portNumbers(ver.Spec.Ports)) {
		if err := s.checkPorts(ctx, tx, inst.ID, ver.Spec.Ports, d.ID); err != nil {
			return Deployment{}, err
		}
		name, err := appName(ctx, tx, d.AppID)
		if err != nil {
			return Deployment{}, err
		}
		// 新版本端口变了：旧端口行删除，新版本 running 时重新登记
		if err := deletePortsBecause(ctx, tx, d, "重新部署应用 "+name+"（端口变更）", now, actor); err != nil {
			return Deployment{}, err
		}
	}
	plain, secret, err := ValidateEnv(ver.Spec, mergeEnv(ver.Spec, d.Env, oldSecret, req.Env, req.EnvSet))
	if err != nil {
		return Deployment{}, err
	}
	out, err := scanDeployment(tx.QueryRow(ctx, `UPDATE app_deployments SET state = 'queued', desired = 'running', app_version_id = $2, spec_hash = $3, port = $4,
		env = $5, secret_env_enc = $6, phase = '', progress = 0, message = '', health = 'unknown', error_code = '', crash_count = 0, unreachable_count = 0,
		next_poll_at = $7, finished_at = NULL, updated_at = $7, row_version = row_version + 1 WHERE id = $1 RETURNING `+depCols,
		d.ID, ver.ID, ver.SpecHash, ver.Spec.Ports[0].Port, mustJSON(plain), sealSecrets(s.d.Box, secret), now))
	if err != nil {
		return Deployment{}, err
	}
	if err := addEvent(ctx, tx, d.ID, d.State, StateQueued, "重新部署 v"+ver.Version, now); err != nil {
		return Deployment{}, err
	}
	return out, auditOp(ctx, tx, now, actor, "instance.app_redeploy", out)
}

// Stop 停止（installing / running）：desired = stopped，由 Runner 执行 run.sh stop。
func (s *Service) Stop(ctx context.Context, userID int64, instanceID, depID, ip string) (Deployment, error) {
	return s.userOp(ctx, userID, instanceID, depID, func(tx pgx.Tx, _ rental.Instance, d Deployment, now time.Time) (Deployment, error) {
		if d.State != StateInstalling && d.State != StateRunning {
			return Deployment{}, apperr.ErrAppInvalidState
		}
		out, err := scanDeployment(tx.QueryRow(ctx, `UPDATE app_deployments SET desired = 'stopped', next_poll_at = $2, updated_at = $2, row_version = row_version + 1
			WHERE id = $1 RETURNING `+depCols, d.ID, now))
		if err != nil {
			return Deployment{}, err
		}
		if err := addEvent(ctx, tx, d.ID, d.State, d.State, "请求停止", now); err != nil {
			return Deployment{}, err
		}
		return out, auditOp(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}, "instance.app_stop", out)
	})
}

// Start 启动（stopped）：desired = running、state = queued，Runner 重新执行 deploy（.installed 匹配时跳过安装）。
func (s *Service) Start(ctx context.Context, userID int64, instanceID, depID, ip string) (Deployment, error) {
	return s.userOp(ctx, userID, instanceID, depID, func(tx pgx.Tx, _ rental.Instance, d Deployment, now time.Time) (Deployment, error) {
		if d.State != StateStopped {
			return Deployment{}, apperr.ErrAppInvalidState
		}
		out, err := scanDeployment(tx.QueryRow(ctx, `UPDATE app_deployments SET state = 'queued', desired = 'running', phase = '', progress = 0, message = '',
			health = 'unknown', error_code = '', crash_count = 0, unreachable_count = 0, next_poll_at = $2, finished_at = NULL, updated_at = $2,
			row_version = row_version + 1 WHERE id = $1 RETURNING `+depCols, d.ID, now))
		if err != nil {
			return Deployment{}, err
		}
		if err := addEvent(ctx, tx, d.ID, d.State, StateQueued, "启动", now); err != nil {
			return Deployment{}, err
		}
		return out, auditOp(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}, "instance.app_start", out)
	})
}

// Remove 卸载（任何非终态）。已是 removing → 返回当前行。
func (s *Service) Remove(ctx context.Context, userID int64, instanceID, depID, ip string) (Deployment, error) {
	return s.userOp(ctx, userID, instanceID, depID, func(tx pgx.Tx, inst rental.Instance, d Deployment, now time.Time) (Deployment, error) {
		if d.State == StateRemoving {
			return d, nil
		}
		actor := audit.Actor{UserID: userID, IP: ip}
		out, err := s.remove(ctx, tx, inst.State, d, "", actor, now)
		if err != nil {
			return Deployment{}, err
		}
		return out, auditOp(ctx, tx, now, actor, "instance.app_remove", out)
	})
}

// remove 是用户卸载与后台强制卸载的共同部分（调用方持实例与部署行锁）：
//   - 实例不是 running（停机 / 未开通 / 已消失）→ 直接 removed，端口立即删除；实例停机且部署执行过（容器里可能有目录）时
//     cleanup_pending = true，实例恢复 running 后 Runner 删除目录。
//   - 实例 running（含暂停中）→ 一律 removing / desired removed，由 Runner 执行 run.sh remove（目录不存在也算成功）后删端口、置 removed；
//     暂停中的实例 Runner 等恢复后再执行。queued 且 attempt = 0 也不走捷径：Runner 可能正在上传部署包。
//
// detail 非空时作为事件文案（后台：「平台卸载：<原因>」）。actor 是端口删除审计的操作人。
func (s *Service) remove(ctx context.Context, tx pgx.Tx, instState string, d Deployment, detail string, actor audit.Actor, now time.Time) (Deployment, error) {
	if !d.Live() {
		return Deployment{}, apperr.ErrAppInvalidState
	}
	if instState != rental.StateRunning {
		fresh := d.State == StateQueued && d.Attempt == 0
		// 只有停机保留数据的实例容器里可能有目录；未开通 / 已消失的实例没有可清理的东西
		cleanup := !fresh && instState == rental.StateStopped
		if detail == "" {
			detail = "已卸载"
			if cleanup {
				detail = "已卸载（实例停机中，应用文件会在实例下次启动时清理）"
			}
		}
		if err := s.setRemoved(ctx, tx, d, "", detail, cleanup, now, actor); err != nil {
			return Deployment{}, err
		}
		return LockDeployment(ctx, tx, d.ID)
	}
	if detail == "" {
		detail = "卸载中"
	}
	out, err := scanDeployment(tx.QueryRow(ctx, `UPDATE app_deployments SET state = 'removing', desired = 'removed', next_poll_at = $2, updated_at = $2,
		row_version = row_version + 1 WHERE id = $1 RETURNING `+depCols, d.ID, now))
	if err != nil {
		return Deployment{}, err
	}
	return out, addEvent(ctx, tx, d.ID, d.State, StateRemoving, detail, now)
}

// AdminForceRemove 后台强制卸载（任何非终态，同 Remove）；事件「平台卸载：<原因>」，审计只记部署 ID 与原因。
func (s *Service) AdminForceRemove(ctx context.Context, actor audit.Actor, depID, reason string) (Deployment, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Deployment{}, apperr.BadRequest("请填写原因")
	}
	now := s.d.Clock.Now()
	var out Deployment
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var instanceID string
		if err := tx.QueryRow(ctx, `SELECT instance_id FROM app_deployments WHERE id = $1`, depID).Scan(&instanceID); err != nil {
			if db.IsNoRows(err) {
				return apperr.ErrNotFound
			}
			return err
		}
		// 先实例后部署（与用户操作同序；instance_id 不会变，未加锁读取无妨）；后台不校验归属，也不拒绝已释放的实例
		var instState string
		if err := tx.QueryRow(ctx, `SELECT state FROM instances WHERE id = $1 FOR UPDATE`, instanceID).Scan(&instState); err != nil {
			return err
		}
		d, err := LockDeployment(ctx, tx, depID)
		if err != nil {
			return err
		}
		if d.State == StateRemoving {
			out = d
		} else if out, err = s.remove(ctx, tx, instState, d, "平台卸载："+reason, actor, now); err != nil { // 端口删除审计记在后台操作人名下
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("app.deployment.force_remove", "app_deployment", d.ID,
			map[string]any{"deployment": d.ID, "reason": reason}))
	})
	if err != nil {
		return Deployment{}, err
	}
	return out, nil
}

// ---- 查询 ----

func (s *Service) Get(ctx context.Context, depID string) (Deployment, error) {
	return scanDeployment(s.d.Pool.QueryRow(ctx, `SELECT `+depCols+` FROM app_deployments WHERE id = $1`, depID))
}

// GetForUser 只返回该用户实例上的部署；不是主人或不在该实例上 → not_found。
func (s *Service) GetForUser(ctx context.Context, userID int64, instanceID, depID string) (Deployment, error) {
	d, err := s.Get(ctx, depID)
	if err != nil {
		return Deployment{}, err
	}
	if d.UserID != userID || d.InstanceID != instanceID {
		return Deployment{}, apperr.ErrNotFound
	}
	return d, nil
}

// ListForInstance 实例上的全部部署（含终态），按创建时间。
func (s *Service) ListForInstance(ctx context.Context, instanceID string) ([]Deployment, error) {
	return collectDeployments(s.d.Pool.Query(ctx, `SELECT `+depCols+` FROM app_deployments WHERE instance_id = $1 ORDER BY created_at, id`, instanceID))
}

// ListLive 实例上的非终态部署，按创建时间。
func (s *Service) ListLive(ctx context.Context, instanceID string) ([]Deployment, error) {
	return collectDeployments(s.d.Pool.Query(ctx, `SELECT `+depCols+` FROM app_deployments
		WHERE instance_id = $1 AND state NOT IN ('removed', 'lost') ORDER BY created_at, id`, instanceID))
}

// LiveByInstances 一次查询返回多个实例的非终态部署（列表页摘要）。
func (s *Service) LiveByInstances(ctx context.Context, ids []string) (map[string][]Deployment, error) {
	out := map[string][]Deployment{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := collectDeployments(s.d.Pool.Query(ctx, `SELECT `+depCols+` FROM app_deployments
		WHERE instance_id = ANY($1) AND state NOT IN ('removed', 'lost') ORDER BY created_at, id`, ids))
	if err != nil {
		return nil, err
	}
	for _, d := range list {
		out[d.InstanceID] = append(out[d.InstanceID], d)
	}
	return out, nil
}

// Events 部署事件，新的在前；limit <= 0 → 100。
func (s *Service) Events(ctx context.Context, depID string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT id, deployment_id, at, from_state, to_state, detail FROM app_deploy_events
		WHERE deployment_id = $1 ORDER BY id DESC LIMIT $2`, depID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.DeploymentID, &e.At, &e.FromState, &e.ToState, &e.Detail)
		return e, err
	})
}

var deploymentStates = []string{StateQueued, StateInstalling, StateRunning, StateStopped, StateFailed, StateRemoving, StateRemoved, StateLost}

type ListFilter struct {
	State      string
	AppID      int64
	UserID     int64
	InstanceID string
}

// AdminList 后台部署列表（新的在前）与总数。
func (s *Service) AdminList(ctx context.Context, f ListFilter, p paging.Page) ([]Deployment, int, error) {
	var w db.Where
	if f.State != "" {
		if !slices.Contains(deploymentStates, f.State) {
			return nil, 0, apperr.BadRequest("部署状态不正确")
		}
		w.Add("state = ?", f.State)
	}
	if f.AppID > 0 {
		w.Add("app_id = ?", f.AppID)
	}
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.InstanceID != "" {
		w.Add("instance_id = ?", f.InstanceID)
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM app_deployments WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	list, err := collectDeployments(s.d.Pool.Query(ctx, `SELECT `+depCols+` FROM app_deployments WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...))
	return list, total, err
}

// Counts 仪表盘数字：部署中（queued / installing）、运行中、最近 24 小时内失败（finished_at ≥ now − 24h）的部署数。
func (s *Service) Counts(ctx context.Context, now time.Time) (installing, running, failed24h int, err error) {
	err = s.d.Pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE state IN ('queued', 'installing')),
		count(*) FILTER (WHERE state = 'running'),
		count(*) FILTER (WHERE state = 'failed' AND finished_at >= $1)
		FROM app_deployments WHERE state IN ('queued', 'installing', 'running') OR (state = 'failed' AND finished_at >= $1)`, now.Add(-24*time.Hour)).
		Scan(&installing, &running, &failed24h)
	return installing, running, failed24h, err
}

// ---- 日志 ----

type LogResult struct {
	Log  string    `json:"log"`
	Live bool      `json:"live"` // true = 刚从容器读取；false = 数据库里 Runner 上次回写的 log_tail
	At   time.Time `json:"at"`
}

// Log 返回部署日志（规格 §6.2「查看日志」）：实例 running 且未暂停时从容器实时读一次（tail -c 64KB，脱敏），失败或不可读时返回 log_tail。
// 终态部署不实时读：同一应用重新部署后目录属于新的部署。
func (s *Service) Log(ctx context.Context, userID int64, instanceID, depID string) (LogResult, error) {
	d, err := s.GetForUser(ctx, userID, instanceID, depID)
	if err != nil {
		return LogResult{}, err
	}
	res := LogResult{Log: d.LogTail, At: d.UpdatedAt}
	if d.LastPolledAt != nil {
		res.At = *d.LastPolledAt
	}
	if s.d.Upstream == nil || !d.Live() {
		return res, nil
	}
	var state, slug string
	var suspended bool
	t := upstream.Target{InstanceID: d.InstanceID, Port: s.instanceSSHPort(d.InstanceID)}
	if err := s.d.Pool.QueryRow(ctx, `SELECT i.state, i.suspended_at IS NOT NULL, m.lan_ip, i.ssh_host_pubkey, a.slug
		FROM instances i JOIN machines m ON m.id = i.machine_id JOIN apps a ON a.id = $2 WHERE i.id = $1`, d.InstanceID, d.AppID).
		Scan(&state, &suspended, &t.LanIP, &t.HostPubkey, &slug); err != nil {
		return LogResult{}, err
	}
	if state != rental.StateRunning || suspended {
		return res, nil
	}
	secrets, err := openSecrets(s.d.Box, d.SecretEnvEnc)
	if err != nil {
		return LogResult{}, err
	}
	ectx, cancel := context.WithTimeout(ctx, ExecStatusTimeout)
	defer cancel()
	out, _, exit, err := s.d.Upstream.Exec(ectx, t, "tail -c "+fmt.Sprint(MaxLogTail)+" "+ShellQuote(AppDir(s.AppRoot(d.InstanceID), slug)+"/log"), nil, MaxLogTail)
	if err != nil || exit != 0 {
		s.d.Log.Debug("app log live read failed", "deployment", d.ID, "exit", exit, "err", err)
		return res, nil
	}
	return LogResult{Log: Redact(string(out), secrets), Live: true, At: s.d.Clock.Now()}, nil
}
