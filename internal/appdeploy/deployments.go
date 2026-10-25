package appdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy/runtime"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/rental"
)

// depCols 与 scanDeployment 的字段顺序一致（含乐观并发列 row_version）。
const depCols = "id, instance_id, user_id, app_id, app_version_id, state, desired, phase, progress, message, health, error_code, env, secret_env_enc, " +
	"port, log_tail, status_seq, attempt, spec_hash, next_poll_at, last_polled_at, cleanup_pending, unreachable_count, crash_count, row_version, " +
	"started_at, finished_at, created_at, updated_at"

// scanDeployment 扫描一行部署；没有行 → apperr.ErrNotFound（Runner 据此判断部署已不存在）。
func scanDeployment(row pgx.Row) (Deployment, error) {
	var d Deployment
	var env []byte
	err := row.Scan(&d.ID, &d.InstanceID, &d.UserID, &d.AppID, &d.AppVersionID, &d.State, &d.Desired, &d.Phase, &d.Progress, &d.Message,
		&d.Health, &d.ErrorCode, &env, &d.SecretEnvEnc, &d.Port, &d.LogTail, &d.StatusSeq, &d.Attempt, &d.SpecHash, &d.NextPollAt,
		&d.LastPolledAt, &d.CleanupPending, &d.UnreachableCount, &d.CrashCount, &d.RowVersion, &d.StartedAt, &d.FinishedAt, &d.CreatedAt, &d.UpdatedAt)
	if db.IsNoRows(err) {
		return Deployment{}, apperr.ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	d.Env = map[string]string{}
	if len(env) > 0 {
		if err := json.Unmarshal(env, &d.Env); err != nil {
			return Deployment{}, fmt.Errorf("app_deployments %s env: %w", d.ID, err)
		}
	}
	return d, nil
}

func collectDeployments(rows pgx.Rows, err error) ([]Deployment, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Deployment, error) { return scanDeployment(r) })
}

// LockDeployment 锁部署行（FOR UPDATE）；不存在 → apperr.ErrNotFound。用户操作与 rental 钩子须先持有实例行锁（规格 §14 加锁顺序）；
// 计划 11 的 comfywf 在 rental.LockOwned 之后调用它。
func LockDeployment(ctx context.Context, q db.Querier, id string) (Deployment, error) {
	return scanDeployment(q.QueryRow(ctx, `SELECT `+depCols+` FROM app_deployments WHERE id = $1 FOR UPDATE`, id))
}

// TouchDeployment 只把部署行 row_version + 1、updated_at = now（不改状态）；不存在 → apperr.ErrNotFound。
// 用途（计划 11）：改了部署包的输入却不改行本身的调用方（comfywf 对 queued 部署只改工作流集合）必须调用它——
// Runner 在事务外打包并 exec，回写时按 row_version 丢弃期间被改过的行，下一轮按新输入重新打包；
// 否则旧包装进容器，OnTransition(running) 还会把没进包的 pending 集合标成 installed。调用方应已持部署行锁。
func TouchDeployment(ctx context.Context, q db.Querier, id string, now time.Time) error {
	tag, err := q.Exec(ctx, `UPDATE app_deployments SET row_version = row_version + 1, updated_at = $2 WHERE id = $1`, id, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.ErrNotFound
	}
	return nil
}

// addEvent 记一条部署事件（detail 须已脱敏、不含变量值）。
func addEvent(ctx context.Context, q db.Querier, depID, from, to, detail string, at time.Time) error {
	_, err := q.Exec(ctx, `INSERT INTO app_deploy_events (deployment_id, at, from_state, to_state, detail) VALUES ($1, $2, $3, $4, $5)`,
		depID, at, from, to, detail)
	return err
}

// livePortsExcept 返回实例上除 exclude 外所有非终态部署在 spec 里声明的端口（含尚未登记到 instance_ports 的）。
func livePortsExcept(ctx context.Context, q db.Querier, instanceID, exclude string) ([]int, error) {
	rows, err := q.Query(ctx, `SELECT (p->>'port')::int FROM app_deployments d JOIN app_versions v ON v.id = d.app_version_id,
		jsonb_array_elements(v.spec->'ports') p WHERE d.instance_id = $1 AND d.id <> $2 AND d.state NOT IN ('removed', 'lost')`, instanceID, exclude)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// mustJSON 普通变量写 env 列；nil → {}。
func mustJSON(m map[string]string) []byte {
	if m == nil {
		m = map[string]string{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err) // map[string]string 不会失败
	}
	return b
}

// truncRunes 截到 n 个字符（message 列上限）。
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// pickVersion：versionID 给定 → 该应用下的已发布版本（否则 app_not_found）；nil → 最新已发布版本（没有 → app_unavailable）。
func (s *Service) pickVersion(ctx context.Context, q db.Querier, appID int64, versionID *int64) (Version, error) {
	if versionID == nil {
		return s.d.Catalog.LatestPublished(ctx, q, appID)
	}
	v, err := scanVersion(q.QueryRow(ctx, `SELECT `+versionCols+` FROM app_versions WHERE id = $1 AND app_id = $2 AND status = 'published'`, *versionID, appID))
	if errors.Is(err, apperr.ErrNotFound) {
		return Version{}, apperr.ErrAppNotFound
	}
	return v, err
}

// resolveVersion 新建部署用：应用须已上架（published），版本见 pickVersion。
func (s *Service) resolveVersion(ctx context.Context, q db.Querier, appID int64, versionID *int64) (App, Version, error) {
	app, err := scanApp(q.QueryRow(ctx, `SELECT `+appCols+` FROM apps WHERE id = $1 AND status = 'published'`, appID))
	if errors.Is(err, apperr.ErrNotFound) {
		return App{}, Version{}, apperr.ErrAppNotFound
	}
	if err != nil {
		return App{}, Version{}, err
	}
	ver, err := s.pickVersion(ctx, q, appID, versionID)
	if err != nil {
		return App{}, Version{}, err
	}
	return app, ver, nil
}

// checkImage 实例镜像须在版本的兼容镜像列表里，否则 app_image_mismatch（消息含所需镜像与当前镜像的名称）。
func (s *Service) checkImage(ctx context.Context, q db.Querier, ver Version, imageID int64) error {
	if slices.Contains(ver.ImageIDs, imageID) {
		return nil
	}
	ids := append(slices.Clone(ver.ImageIDs), imageID)
	rows, err := q.Query(ctx, `SELECT id, name FROM images WHERE id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	names := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		names[id] = name
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	need := make([]string, 0, len(ver.ImageIDs))
	for _, id := range ver.ImageIDs {
		if n, ok := names[id]; ok {
			need = append(need, n)
		}
	}
	have := names[imageID]
	if have == "" {
		have = fmt.Sprintf("#%d", imageID)
	}
	return apperr.AppImageMismatch(need, have)
}

// cancelStaleCleanup：同一实例上同一应用的旧部署若还等着清理目录（停机时卸载），新部署会用同一个目录：
// 取消清理，免得 Runner 删掉新部署的文件。每条新建部署的路径（创建 / 预装 / 换机复制）在插入前调用。
func cancelStaleCleanup(ctx context.Context, q db.Querier, instanceID string, appID int64, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE app_deployments SET cleanup_pending = false, updated_at = $3, row_version = row_version + 1
		WHERE instance_id = $1 AND app_id = $2 AND state = 'removed' AND cleanup_pending`, instanceID, appID, now)
	return err
}

// insert 插入一条 queued 部署（attempt 0，next_poll_at = now），先取消同一应用旧部署的待清理（cancelStaleCleanup）。
func (s *Service) insert(ctx context.Context, q db.Querier, inst rental.Instance, userID int64, app App, ver Version, plain, secret map[string]string, now time.Time) (Deployment, error) {
	if err := cancelStaleCleanup(ctx, q, inst.ID, app.ID, now); err != nil {
		return Deployment{}, err
	}
	id := randx.ID("dep-", 12)
	return scanDeployment(q.QueryRow(ctx, `INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, desired, env, secret_env_enc, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'queued', 'running', $6, $7, $8, $9, $10, $10, $10) RETURNING `+depCols,
		id, inst.ID, userID, app.ID, ver.ID, mustJSON(plain), sealSecrets(s.d.Box, secret), ver.Spec.Ports[0].Port, ver.SpecHash, now))
}

// appName 取应用名（端口备注 / 审计 detail 用）。
func appName(ctx context.Context, q db.Querier, appID int64) (string, error) {
	var name string
	err := q.QueryRow(ctx, `SELECT name FROM apps WHERE id = $1`, appID).Scan(&name)
	return name, err
}

// markFailed 把部署置为 failed（Runner / 钩子用；调用方持部署行锁）：next_poll_at 推后 24 小时（failed 不在 Runner 候选里，只为索引整洁）。
// msg 在这里按该部署的全部 secret 值脱敏（先脱敏再截断，避免截断留下 secret 前缀）；解密失败时丢弃 msg（宁缺勿漏）。
// 所有写 failed 的路径都经这里：Deps.OnTransition(failed) 在此于同一事务里调用一次。
func (s *Service) markFailed(ctx context.Context, q pgx.Tx, cur Deployment, code, msg string, now time.Time) error {
	secrets, err := openSecrets(s.d.Box, cur.SecretEnvEnc)
	if err != nil {
		s.d.Log.Warn("app deploy markFailed: cannot open secrets, dropping message", "deployment", cur.ID, "err", err)
		msg = ""
	}
	msg = truncRunes(Redact(msg, secrets), MaxMessageLen)
	if _, err := q.Exec(ctx, `UPDATE app_deployments SET state = 'failed', error_code = $2, message = $3, health = 'unknown', finished_at = $4,
		next_poll_at = $5, updated_at = $4, row_version = row_version + 1 WHERE id = $1`, cur.ID, code, msg, now, now.Add(24*time.Hour)); err != nil {
		return err
	}
	if err := s.onTransition(ctx, q, cur, StateFailed, now); err != nil {
		return err
	}
	detail := ErrorText[code]
	if detail == "" {
		detail = code
	}
	if msg != "" {
		detail += "：" + msg
	}
	return addEvent(ctx, q, cur.ID, cur.State, StateFailed, detail, now)
}

// onTransition 调 Deps.OnTransition（nil 安全）。
func (s *Service) onTransition(ctx context.Context, tx pgx.Tx, cur Deployment, to string, now time.Time) error {
	if s.d.OnTransition == nil {
		return nil
	}
	if err := s.d.OnTransition(ctx, tx, cur, to, now); err != nil {
		// 回写随之回滚、下一轮重试；一直失败时部署卡在原状态，必须让人看见
		s.d.Log.Error("appdeploy: OnTransition failed, writeback rolled back", "deployment", cur.ID, "to", to, "err", err)
		return fmt.Errorf("appdeploy: OnTransition(%s → %s): %w", cur.ID, to, err)
	}
	return nil
}

// markRemoved 把部署置为 removed（不再需要清理容器目录：实例已消失，或 Runner 已执行 remove），删除它登记的端口（以部署主人审计）并记事件。
func (s *Service) markRemoved(ctx context.Context, q db.Querier, cur Deployment, code, detail string, now time.Time) error {
	return s.setRemoved(ctx, q, cur, code, detail, false, now, audit.Actor{UserID: cur.UserID})
}

// setRemoved 是 markRemoved 与用户 / 后台卸载共用的终态写入。
func (s *Service) setRemoved(ctx context.Context, q db.Querier, cur Deployment, code, detail string, cleanup bool, now time.Time, actor audit.Actor) error {
	if _, err := q.Exec(ctx, `UPDATE app_deployments SET state = 'removed', desired = 'removed', error_code = $2, finished_at = $3, cleanup_pending = $4,
		health = 'unknown', updated_at = $3, row_version = row_version + 1 WHERE id = $1`, cur.ID, code, now, cleanup); err != nil {
		return err
	}
	name, err := appName(ctx, q, cur.AppID)
	if err != nil {
		return err
	}
	if err := deletePorts(ctx, q, cur, name, now, actor); err != nil {
		return err
	}
	return addEvent(ctx, q, cur.ID, cur.State, StateRemoved, detail, now)
}

// ---- Runner 用 ----

// bundleError 部署包无法生成（版本 / 变量数据问题，或敏感变量无法解密；重试无用）：Runner 把部署标为 upload_failed。
type bundleError struct{ err error }

// Error 给 apperr 只用其文案（不带错误码），其余原样。
func (e *bundleError) Error() string {
	var ae *apperr.Error
	if errors.As(e.err, &ae) {
		return ae.Message
	}
	return e.err.Error()
}
func (e *bundleError) Unwrap() error { return e.err }

// appAndVersion 取部署的应用与版本（含脚本）。
func (s *Service) appAndVersion(ctx context.Context, d Deployment) (App, Version, error) {
	app, err := s.d.Catalog.GetApp(ctx, d.AppID)
	if err != nil {
		return App{}, Version{}, fmt.Errorf("app %d: %w", d.AppID, err)
	}
	ver, err := s.d.Catalog.GetVersion(ctx, d.AppVersionID)
	if err != nil {
		return App{}, Version{}, fmt.Errorf("app version %d: %w", d.AppVersionID, err)
	}
	return app, ver, nil
}

// bundleFor 生成部署的部署包（run.sh + app.sh + spec.json + env，imageEnv 非 nil 时加 image.env；
// 版本支持工作流且接了 Deps.BundleExtra 时追加它给出的文件：其 apperr 错误（内容问题，如 workflow_version_invalid）
// 是 bundleError，其余（库连不上等临时错误）原样返回，Runner 下一轮重试，不把部署标失败）。
// healthAddr 非 nil 时覆盖 run.sh 的健康检查地址（测试）。
func (s *Service) bundleFor(ctx context.Context, d Deployment, appRoot string, healthAddr func(port int) string, imageEnv []EnvKV) ([]byte, App, error) {
	app, ver, err := s.appAndVersion(ctx, d)
	if err != nil {
		return nil, App{}, err
	}
	secret, err := openSecrets(s.d.Box, d.SecretEnvEnc)
	if err != nil {
		return nil, app, &bundleError{err}
	}
	in := BundleInput{Slug: app.Slug, InstanceID: d.InstanceID, AppRoot: appRoot, SpecHash: d.SpecHash, Spec: ver.Spec, Script: ver.Script,
		Env: d.Env, Secret: secret, HFEndpoint: s.d.HFEndpoint, PipIndexURL: s.d.PipIndexURL, GitMirror: s.d.GitMirror, ImageEnv: imageEnv}
	if ver.Spec.SupportsWorkflows && s.d.BundleExtra != nil {
		if in.Extra, err = s.d.BundleExtra(ctx, d, ver); err != nil {
			var ae *apperr.Error
			if errors.As(err, &ae) {
				return nil, app, &bundleError{err}
			}
			return nil, app, fmt.Errorf("bundle extra: %w", err)
		}
	}
	if healthAddr != nil && len(ver.Spec.Ports) > 0 {
		in.HealthAddr = healthAddr(ver.Spec.Ports[0].Port)
	}
	b, err := Bundle(in, runtime.RunSH)
	if err != nil {
		return nil, app, &bundleError{err}
	}
	return b, app, nil
}
