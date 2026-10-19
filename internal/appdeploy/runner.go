package appdeploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/rental"
)

// RunnerConfig 的零值即生产配置。
type RunnerConfig struct {
	PollInterval time.Duration // 零值 = PollInterval（5 秒）
	// AppRoot 容器内应用根目录；空 = Service 的设置（Deps.AppRootFor / Deps.AppRoot，默认 DefaultAppRoot），与「查看日志」一致。
	AppRoot string
	// AppRootFor 按实例给出应用根目录（测试 / e2e 多个实例共用一个假容器时各用各的目录）；非 nil 时优先于 AppRoot。
	AppRootFor    func(instanceID string) string
	HealthAddr    func(port int) string // run.sh 健康检查地址（测试注入）；nil = 127.0.0.1:<主端口>
	MaxConcurrent int                   // 每轮 exec 并发上限；零值 = MaxExecPerRound（16）
}

// RunnerStats 是测试 / 运维用的计数。
type RunnerStats struct{ PeakConcurrent int32 }

// Runner 是部署的后台执行者（规格 §5.3）：每轮拿会话级咨询锁，选出到点的部署，逐个用独立事务认领（事务 1），
// 在事务外经 upstream.Pool.Exec 操作容器里的 run.sh，再开事务回写（事务 2，按 row_version 乐观并发：
// exec 期间用户改过这一行就丢弃本轮结果）。
//
// exec 超时时 Pool.Exec 会 SIGKILL 会话，但容器里的命令仍可能在跑：每个步骤都按幂等、可与上一次重叠来写
// （deploy 先停旧进程；stop / remove 对已停 / 已删的目录也成功；状态按 seq 单调）。
type Runner struct {
	s    *Service
	up   *upstream.Pool
	cfg  RunnerConfig
	log  *slog.Logger
	cur  atomic.Int32
	peak atomic.Int32
	// beforeWriteback 测试钩子：exec 之后、回写事务之前调用（模拟 exec 期间的用户操作）。
	beforeWriteback func(Deployment)
}

func NewRunner(s *Service, up *upstream.Pool, cfg RunnerConfig) *Runner {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = PollInterval
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = MaxExecPerRound
	}
	return &Runner{s: s, up: up, cfg: cfg, log: s.d.Log}
}

func (r *Runner) Stats() RunnerStats { return RunnerStats{PeakConcurrent: r.peak.Load()} }

func (r *Runner) appRoot(instanceID string) string {
	switch {
	case r.cfg.AppRootFor != nil:
		return r.cfg.AppRootFor(instanceID)
	case r.cfg.AppRoot != "":
		return r.cfg.AppRoot
	}
	return r.s.AppRoot(instanceID)
}

// Run 每 PollInterval 一轮直到 ctx 结束；错误只记日志。
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.PollInterval)
	defer t.Stop()
	for {
		if _, _, err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("appdeploy runner tick", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// candidateCond 是 Runner 处理的部署（stopped / failed 不轮询：用户「启动 / 重新部署」或 rental 钩子把它们置回 queued）。
const candidateCond = `(state IN ('queued', 'installing', 'running', 'removing') OR (state = 'removed' AND cleanup_pending))`

// Tick 执行一轮：拿会话级咨询锁（拿不到 locked = false）→ 选候选 → 并发处理（≤ MaxConcurrent，同一实例 ≤ 3 个）→ 释放锁。
func (r *Runner) Tick(ctx context.Context) (int, bool, error) {
	conn, err := r.s.d.Pool.Acquire(ctx)
	if err != nil {
		return 0, false, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, advisoryLockName).Scan(&locked); err != nil {
		return 0, false, err
	}
	if !locked {
		return 0, false, nil
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, advisoryLockName) //nolint:errcheck
	now := r.s.d.Clock.Now()
	rows, err := r.s.d.Pool.Query(ctx, `SELECT id, instance_id FROM app_deployments
		WHERE next_poll_at <= $1 AND `+candidateCond+` ORDER BY next_poll_at, id LIMIT 50`, now)
	if err != nil {
		return 0, true, err
	}
	var ids []string
	perInst := map[string]int{}
	for rows.Next() {
		var id, inst string
		if err := rows.Scan(&id, &inst); err != nil {
			rows.Close()
			return 0, true, err
		}
		if perInst[inst] >= MaxAppsPerInstance {
			continue
		}
		perInst[inst]++
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, true, err
	}
	sem := make(chan struct{}, r.cfg.MaxConcurrent)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	processed := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { r.cur.Add(-1); <-sem }()
			r.enter()
			done, err := r.step(ctx, id, now)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("deployment %s: %w", id, err))
			} else if done {
				processed++
			}
		}(id)
	}
	wg.Wait()
	return processed, true, errors.Join(errs...)
}

// enter 计入一个并发步骤并更新峰值。
func (r *Runner) enter() {
	n := r.cur.Add(1)
	for {
		p := r.peak.Load()
		if n <= p || r.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

// target 是实例的连接目标快照（不加锁；过期由下一轮纠正）。
type target struct {
	State      string
	Suspended  bool
	LanIP      string
	HostPubkey string
	MachineID  int64
}

func (t target) upstream(id string) upstream.Target {
	return upstream.Target{InstanceID: id, LanIP: t.LanIP, HostPubkey: t.HostPubkey}
}

func readTarget(ctx context.Context, q db.Querier, instanceID string) (target, error) {
	var t target
	err := q.QueryRow(ctx, `SELECT i.state, i.suspended_at IS NOT NULL, m.lan_ip, i.ssh_host_pubkey, i.machine_id
		FROM instances i JOIN machines m ON m.id = i.machine_id WHERE i.id = $1`, instanceID).Scan(&t.State, &t.Suspended, &t.LanIP, &t.HostPubkey, &t.MachineID)
	if db.IsNoRows(err) {
		return target{State: rental.StateReleased}, nil
	}
	return t, err
}

func instanceGone(state string) bool {
	switch state {
	case rental.StateWiping, rental.StateReleased, rental.StateFailed, rental.StateCancelled:
		return true
	}
	return false
}

type action int

const (
	actNone    action = iota
	actWait           // 实例非 running / 暂停中：不 exec（在事务 1 里推后 next_poll_at）
	actGone           // 实例已消失：removed(instance_gone)
	actCleanup        // removed 且 cleanup_pending：清理停机时卸载留下的目录
	actRemove         // removing / desired removed：run.sh remove
	actDeploy         // queued：上传部署包 + run.sh deploy
	actStop           // installing / running 且 desired stopped：run.sh stop
	actPoll           // installing：读 status
	actHealth         // running：读 status + HTTP 健康检查
)

func decide(d Deployment, tg target) action {
	switch {
	case instanceGone(tg.State):
		return actGone
	case tg.State != rental.StateRunning || tg.Suspended:
		return actWait
	case d.State == StateRemoved:
		if d.CleanupPending {
			return actCleanup
		}
		return actNone
	case d.State == StateRemoving || d.Desired == DesiredRemoved:
		return actRemove
	case d.State == StateQueued:
		return actDeploy
	case d.Desired == DesiredStopped && (d.State == StateInstalling || d.State == StateRunning):
		return actStop
	case d.State == StateInstalling:
		return actPoll
	case d.State == StateRunning:
		return actHealth
	}
	return actNone
}

// step 处理一个部署：事务 1 认领（FOR UPDATE SKIP LOCKED 复核、推后 next_poll_at、row_version + 1；要部署时 attempt + 1，
// 在上传之前：卸载据 attempt 判断容器里是否可能有目录）→ exec（事务外）→ 事务 2 回写。返回是否真的处理了。
func (r *Runner) step(ctx context.Context, id string, now time.Time) (bool, error) {
	var d Deployment
	var tg target
	var act action
	err := db.WithTx(ctx, r.s.d.Pool, func(tx pgx.Tx) error {
		var err error
		if d, err = scanDeployment(tx.QueryRow(ctx, `SELECT `+depCols+` FROM app_deployments
			WHERE id = $1 AND next_poll_at <= $2 AND `+candidateCond+` FOR UPDATE SKIP LOCKED`, id, now)); err != nil {
			return err
		}
		if tg, err = readTarget(ctx, tx, d.InstanceID); err != nil {
			return err
		}
		act = decide(d, tg)
		if act == actWait { // 容器停着 / 机器失联：30 秒后再看；running 的部署健康未知（规格 §6.3）
			return tx.QueryRow(ctx, `UPDATE app_deployments SET next_poll_at = $2, last_polled_at = $3,
				health = CASE WHEN state = 'running' THEN 'unknown' ELSE health END, updated_at = $3, row_version = row_version + 1
				WHERE id = $1 RETURNING row_version`, d.ID, now.Add(NotRunningRetry), now).Scan(&d.RowVersion)
		}
		inc := 0
		if act == actDeploy {
			inc = 1
		}
		return tx.QueryRow(ctx, `UPDATE app_deployments SET next_poll_at = $2, last_polled_at = $3, attempt = attempt + $4, updated_at = $3,
			row_version = row_version + 1 WHERE id = $1 RETURNING attempt, row_version, updated_at`,
			d.ID, now.Add(r.cfg.PollInterval), now, inc).Scan(&d.Attempt, &d.RowVersion, &d.UpdatedAt)
	})
	if errors.Is(err, apperr.ErrNotFound) {
		return false, nil // 被别的轮次锁着、或状态已变
	}
	if err != nil {
		return false, err
	}
	ut := tg.upstream(d.InstanceID)
	switch act {
	case actWait:
		return true, nil
	case actGone:
		return true, r.stepGone(ctx, d, now)
	case actCleanup:
		return true, r.stepCleanup(ctx, d, ut, now)
	case actRemove:
		return true, r.stepRemove(ctx, d, ut, tg, now)
	case actDeploy:
		return true, r.stepDeploy(ctx, d, ut, tg, now)
	case actStop:
		return true, r.stepStop(ctx, d, ut, tg, now)
	case actPoll:
		return true, r.stepPoll(ctx, d, ut, tg, now)
	case actHealth:
		return true, r.stepHealth(ctx, d, ut, tg, now)
	}
	return false, nil
}

// writeback 事务 2。加锁顺序与用户操作 / rental 钩子一致：先实例行 FOR KEY SHARE，再部署行，再 instance_ports
// （registerPorts 插入端口时外键检查要对实例行取 KEY SHARE，只锁部署行会与「持实例 FOR UPDATE、等部署行」的事务成环）。
// row_version 与认领时不同（exec 期间用户停止 / 卸载 / 重新部署过）→ 丢弃本轮结果，下一轮按新状态处理。
func (r *Runner) writeback(ctx context.Context, d Deployment, fn func(tx pgx.Tx, cur Deployment) error) error {
	if r.beforeWriteback != nil {
		r.beforeWriteback(d)
	}
	return db.WithTx(ctx, r.s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM instances WHERE id = $1 FOR KEY SHARE`, d.InstanceID); err != nil {
			return err
		}
		cur, err := lockDeployment(ctx, tx, d.ID)
		if errors.Is(err, apperr.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.RowVersion != d.RowVersion {
			r.log.Info("appdeploy: stale writeback discarded", "deployment", d.ID, "row_version", d.RowVersion, "current", cur.RowVersion,
				"state", cur.State, "desired", cur.Desired)
			return nil
		}
		return fn(tx, cur)
	})
}

type execResult struct {
	stdout, stderr []byte
	exit           int
}

// exec 在容器里执行；连接层错误返回 *upstream.DialError（调用方走 unreachable 分支）。
func (r *Runner) exec(ctx context.Context, ut upstream.Target, cmd string, stdin []byte, timeout time.Duration) (execResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, errb, exit, err := r.up.Exec(ctx, ut, cmd, stdin, MaxLogTail+MaxStatusBytes+1024)
	return execResult{out, errb, exit}, err
}

func isDial(err error) bool {
	var de *upstream.DialError
	return errors.As(err, &de)
}

// ---- 容器内命令（目录只由 AppRoot 与 slug 拼出，仍按单引号转义） ----

func runSHCmd(dir, sub string) string { return "sh " + ShellQuote(dir+"/run.sh") + " " + sub }

// statusCmd：退出码取 run.sh status 的（3 = status.json 缺失；目录不在时 cd 失败也非零），不能被 tail 的覆盖。
func statusCmd(dir string) string {
	q := ShellQuote(dir)
	return "cd " + q + " && sh run.sh status; rc=$?; echo ---; tail -c " + strconv.Itoa(MaxLogTail) + " " + ShellQuote(dir+"/log") + " 2>/dev/null; exit $rc"
}

// removeCmd：run.sh 在 → run.sh remove（先停进程再删目录）；目录或 run.sh 已不在（用户删了、上次 remove 超时但已删完）→
// rm -rf 兜底，按已卸载处理（sh 对不存在的 run.sh 返回非零）。
func removeCmd(dir string) string {
	return "cd / && if [ -f " + ShellQuote(dir+"/run.sh") + " ]; then " + runSHCmd(dir, "remove") + "; else rm -rf " + ShellQuote(dir) + "; fi"
}

// ---- 文本整理 ----

// cleanText 去掉非法 UTF-8 与 NUL（PostgreSQL text 列不接受）。
func cleanText(s string) string { return strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\x00", "") }

// tidyLog：满 64KB 的尾部是从中间截的，丢掉不完整的第一行（可能是半个字符或半个 secret，脱敏认不出来）。
func tidyLog(s string) string {
	if len(s) >= MaxLogTail {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return cleanText(s)
}

func lastLine(b []byte) string {
	s := strings.TrimRight(string(b), "\n")
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return truncRunes(cleanText(s), MaxMessageLen)
}

func clampProgress(p int) int { return max(0, min(100, p)) }

// redactor 返回按该部署 secret 值脱敏的函数；解密失败时一律返回空串（宁缺勿漏）。
func (r *Runner) redactor(d Deployment) func(string) string {
	secrets, err := openSecrets(r.s.d.Box, d.SecretEnvEnc)
	if err != nil {
		r.log.Warn("appdeploy: cannot open secrets, dropping log and message", "deployment", d.ID, "err", err)
		return func(string) string { return "" }
	}
	return func(s string) string { return Redact(s, secrets) }
}

// ---- 步骤 ----

// unreachable 连续 DialError 计数；达到 UnreachableFailAfter → failed(unreachable) + 告警一次（卸载则仍标 removed）。
// undoAttempt：上传命令没发出去（DialError 只在命令送达前出现），认领时加的 attempt 退回。
func (r *Runner) unreachable(ctx context.Context, d Deployment, tg target, now time.Time, cause error, undoAttempt bool) error {
	r.log.Warn("appdeploy: container unreachable", "deployment", d.ID, "instance", d.InstanceID, "count", d.UnreachableCount+1, "err", cause)
	undo := 0
	if undoAttempt {
		undo = 1
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		n := cur.UnreachableCount + 1
		if _, err := tx.Exec(ctx, `UPDATE app_deployments SET unreachable_count = $2, attempt = GREATEST(attempt - $3, 0), next_poll_at = $4, updated_at = $5,
			row_version = row_version + 1 WHERE id = $1`, cur.ID, n, undo, now.Add(UnreachableRetry), now); err != nil {
			return err
		}
		if n < UnreachableFailAfter {
			return nil
		}
		if cur.Desired == DesiredRemoved { // 卸载时容器不可达：仍标 removed，目录留在数据盘
			return r.s.markRemoved(ctx, tx, cur, ErrCodeUnreachable, "容器不可达，目录未清理", now)
		}
		if err := r.s.markFailed(ctx, tx, cur, ErrCodeUnreachable, fmt.Sprintf("连续 %d 次无法连接容器", n), now); err != nil {
			return err
		}
		// 同一实例的几个部署往往同一轮一起失败、各自一个事务：RaiseAlertOnce 的「查重再插入」要按实例串行，否则各插一条
		msg := rental.AlertAppDeployUnreachable(cur.InstanceID)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "app_deploy_unreachable:"+cur.InstanceID); err != nil {
			return err
		}
		return r.s.d.Fleet.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "warning", Kind: "app_deploy_unreachable", MachineID: tg.MachineID,
			InstanceID: cur.InstanceID, Message: msg})
	})
}

// stepGone 实例已消失（rental 钩子的兜底）：非终态 → removed(instance_gone)；待清理的目录随实例一起没了 → 清标记。
func (r *Runner) stepGone(ctx context.Context, d Deployment, now time.Time) error {
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		if cur.State == StateRemoved {
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET cleanup_pending = false, updated_at = $2, row_version = row_version + 1 WHERE id = $1`, cur.ID, now)
			return err
		}
		return r.s.markRemoved(ctx, tx, cur, ErrCodeInstanceGone, ErrorText[ErrCodeInstanceGone], now)
	})
}

func (r *Runner) stepDeploy(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	root := r.appRoot(d.InstanceID)
	bundle, app, err := r.s.bundleFor(ctx, d, root, r.cfg.HealthAddr)
	var be *bundleError
	if errors.As(err, &be) {
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			return r.s.markFailed(ctx, tx, cur, ErrCodeUploadFailed, "部署包生成失败："+be.Error(), now)
		})
	}
	if err != nil {
		return err
	}
	dir := AppDir(root, app.Slug)
	res, err := r.exec(ctx, ut, UploadCommand(dir), bundle, ExecUploadTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err, true)
	}
	if err != nil {
		return err
	}
	if res.exit != 0 {
		msg := lastLine(res.stderr)
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			return r.s.markFailed(ctx, tx, cur, ErrCodeUploadFailed, "上传失败："+msg, now)
		})
	}
	res, err = r.exec(ctx, ut, runSHCmd(dir, "deploy"), nil, ExecStatusTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err, false)
	}
	if err != nil {
		return err
	}
	if res.exit != 0 {
		code, msg := ErrCodeScriptFailed, lastLine(res.stderr)
		if strings.HasPrefix(msg, "disk:") {
			code = ErrCodeDisk
		}
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error { return r.s.markFailed(ctx, tx, cur, code, msg, now) })
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'installing', phase = 'prepare', progress = 0, message = '', health = 'unknown', error_code = '',
			started_at = $2, finished_at = NULL, status_seq = 0, unreachable_count = 0, crash_count = 0, next_poll_at = $3, updated_at = $2,
			row_version = row_version + 1 WHERE id = $1`, cur.ID, now, now.Add(r.cfg.PollInterval)); err != nil {
			return err
		}
		return addEvent(ctx, tx, cur.ID, cur.State, StateInstalling, fmt.Sprintf("开始部署（第 %d 次）", cur.Attempt), now)
	})
}

// statusMissing 是 status 命令非零退出（status.json 缺失 / 目录被删）时的消息（规格 §6.3）。
const statusMissing = "状态文件缺失"

// failCode 把容器内 failed 状态映射成错误码。
func failCode(st Status) string {
	switch {
	case strings.HasPrefix(st.Message, "disk:"):
		return ErrCodeDisk
	case strings.HasPrefix(st.Message, "timeout"):
		return ErrCodeTimeout
	case st.Phase == "health" && strings.Contains(st.Message, "进程已退出"):
		return ErrCodeHealthFailed
	}
	return ErrCodeScriptFailed
}

func (r *Runner) stepPoll(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	app, ver, err := r.s.appAndVersion(ctx, d)
	if err != nil {
		return err
	}
	dir := AppDir(r.appRoot(d.InstanceID), app.Slug)
	res, err := r.exec(ctx, ut, statusCmd(dir), nil, ExecStatusTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err, false)
	}
	if err != nil {
		return err
	}
	if res.exit != 0 {
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			return r.s.markFailed(ctx, tx, cur, ErrCodeScriptFailed, statusMissing, now)
		})
	}
	st, _, rawTail, perr := ParseStatusOutput(res.stdout)
	if perr != nil {
		r.log.Warn("appdeploy: bad status output", "deployment", d.ID, "err", perr)
		return nil
	}
	if st.Seq < d.StatusSeq {
		r.log.Info("appdeploy: status seq went back, ignored", "deployment", d.ID, "seq", st.Seq, "have", d.StatusSeq)
		return nil
	}
	red := r.redactor(d)
	logTail, msg, phase := red(tidyLog(rawTail)), truncRunes(red(cleanText(st.Message)), MaxMessageLen), truncRunes(cleanText(st.Phase), 64)
	minutes := ver.Spec.TimeoutMinutes
	if minutes <= 0 {
		minutes = DefaultTimeoutMinutes
	}
	if st.State == StateInstalling && d.StartedAt != nil && now.Sub(*d.StartedAt) > time.Duration(minutes)*time.Minute {
		// 超时：先停掉容器里的 _bg / 服务（失败也照样标 failed：之后的 deploy / remove 会再停一次）
		if res, err := r.exec(ctx, ut, runSHCmd(dir, "stop"), nil, ExecStatusTimeout); err != nil || res.exit != 0 {
			r.log.Warn("appdeploy: stop after timeout failed", "deployment", d.ID, "exit", res.exit, "err", err)
		}
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET log_tail = $2, status_seq = $3, phase = $4, row_version = row_version + 1 WHERE id = $1`,
				cur.ID, logTail, st.Seq, phase); err != nil {
				return err
			}
			return r.s.markFailed(ctx, tx, cur, ErrCodeTimeout, fmt.Sprintf("超过 %d 分钟未完成", minutes), now)
		})
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		switch st.State {
		case StateInstalling:
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET phase = $2, progress = $3, message = $4, log_tail = $5, status_seq = $6, unreachable_count = 0,
				next_poll_at = $7, updated_at = $8, row_version = row_version + 1 WHERE id = $1`,
				cur.ID, phase, clampProgress(st.Progress), msg, logTail, st.Seq, now.Add(r.cfg.PollInterval), now)
			return err
		case StateRunning:
			code, err := registerPorts(ctx, tx, cur, ver.Spec.Ports, app.Name, now)
			if err != nil {
				return err
			}
			if code != "" {
				return r.s.markFailed(ctx, tx, cur, code, ErrorText[code], now)
			}
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'running', phase = $2, progress = 100, message = $3, health = 'ok', error_code = '',
				log_tail = $4, status_seq = $5, unreachable_count = 0, crash_count = 0, finished_at = $6, next_poll_at = $7, updated_at = $6,
				row_version = row_version + 1 WHERE id = $1`, cur.ID, phase, msg, logTail, st.Seq, now, now.Add(HealthInterval)); err != nil {
				return err
			}
			return addEvent(ctx, tx, cur.ID, cur.State, StateRunning, "部署完成", now)
		case StateFailed:
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET log_tail = $2, status_seq = $3, phase = $4, row_version = row_version + 1 WHERE id = $1`,
				cur.ID, logTail, st.Seq, phase); err != nil {
				return err
			}
			return r.s.markFailed(ctx, tx, cur, failCode(st), msg, now)
		case StateStopped: // 容器内已被停掉（不是经平台）：对齐，用户可再「启动」
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'stopped', health = 'unknown', log_tail = $2, status_seq = $3, unreachable_count = 0,
				next_poll_at = $4, updated_at = $5, row_version = row_version + 1 WHERE id = $1`, cur.ID, logTail, st.Seq, now.Add(24*time.Hour), now); err != nil {
				return err
			}
			return addEvent(ctx, tx, cur.ID, cur.State, StateStopped, "容器内已停止", now)
		}
		r.log.Warn("appdeploy: unknown container state", "deployment", cur.ID, "state", st.State)
		return nil
	})
}

func (r *Runner) stepHealth(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	app, ver, err := r.s.appAndVersion(ctx, d)
	if err != nil {
		return err
	}
	res, err := r.exec(ctx, ut, statusCmd(AppDir(r.appRoot(d.InstanceID), app.Slug)), nil, ExecStatusTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err, false)
	}
	if err != nil {
		return err
	}
	if res.exit != 0 {
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			return r.s.markFailed(ctx, tx, cur, ErrCodeScriptFailed, statusMissing, now)
		})
	}
	st, alive, rawTail, perr := ParseStatusOutput(res.stdout)
	if perr != nil {
		r.log.Warn("appdeploy: bad status output", "deployment", d.ID, "err", perr)
		return nil
	}
	logTail := r.redactor(d)(tidyLog(rawTail))
	seq := max(st.Seq, d.StatusSeq)
	healthy := alive
	if alive && len(ver.Spec.Ports) > 0 && ver.Spec.Ports[0].HealthPath != "" {
		healthy = r.httpHealth(ctx, ut, ver.Spec.Ports[0].Port, ver.Spec.Ports[0].HealthPath)
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		switch {
		case !alive && cur.CrashCount+1 >= CrashedAfter:
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET log_tail = $2, status_seq = $3, row_version = row_version + 1 WHERE id = $1`, cur.ID, logTail, seq); err != nil {
				return err
			}
			return r.s.markFailed(ctx, tx, cur, ErrCodeCrashed, ErrorText[ErrCodeCrashed], now)
		case !alive:
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET health = 'unhealthy', crash_count = crash_count + 1, log_tail = $2, status_seq = $3, unreachable_count = 0,
				next_poll_at = $4, updated_at = $5, row_version = row_version + 1 WHERE id = $1`, cur.ID, logTail, seq, now.Add(HealthInterval), now)
			return err
		default:
			h := HealthUnhealthy
			if healthy {
				h = HealthOK
			}
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET health = $2, crash_count = 0, log_tail = $3, status_seq = $4, unreachable_count = 0,
				next_poll_at = $5, updated_at = $6, row_version = row_version + 1 WHERE id = $1`, cur.ID, h, logTail, seq, now.Add(HealthInterval), now)
			return err
		}
	})
}

// httpHealth 经 Pool.Dial 请求容器内 127.0.0.1:<port><path>，只看状态码 2xx；响应体最多读 4KB。
func (r *Runner) httpHealth(ctx context.Context, ut upstream.Target, port int, path string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return r.up.Dial(ctx, ut, port) }}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+path, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.CopyN(io.Discard, resp.Body, 4096) //nolint:errcheck
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (r *Runner) stepStop(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	app, err := r.s.d.Catalog.GetApp(ctx, d.AppID)
	if err != nil {
		return err
	}
	res, err := r.exec(ctx, ut, runSHCmd(AppDir(r.appRoot(d.InstanceID), app.Slug), "stop"), nil, ExecStatusTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err, false)
	}
	if err != nil {
		return err
	}
	if res.exit != 0 { // 目录 / run.sh 已不在：没有可停的进程
		r.log.Warn("appdeploy: run.sh stop failed, treating as stopped", "deployment", d.ID, "exit", res.exit, "stderr", lastLine(res.stderr))
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'stopped', health = 'unknown', phase = '', message = '', unreachable_count = 0, crash_count = 0,
			next_poll_at = $2, updated_at = $3, row_version = row_version + 1 WHERE id = $1`, cur.ID, now.Add(24*time.Hour), now); err != nil {
			return err
		}
		return addEvent(ctx, tx, cur.ID, cur.State, StateStopped, "已停止", now)
	})
}

func (r *Runner) stepRemove(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	app, err := r.s.d.Catalog.GetApp(ctx, d.AppID)
	if err != nil {
		return err
	}
	res, err := r.exec(ctx, ut, removeCmd(AppDir(r.appRoot(d.InstanceID), app.Slug)), nil, ExecUploadTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err, false)
	}
	if err != nil {
		return err
	}
	detail := "已卸载"
	if res.exit != 0 {
		r.log.Warn("appdeploy: remove exited non-zero, marking removed", "deployment", d.ID, "exit", res.exit, "stderr", lastLine(res.stderr))
		detail = fmt.Sprintf("已卸载（容器内清理返回 %d）", res.exit)
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		return r.s.markRemoved(ctx, tx, cur, "", detail, now)
	})
}

// stepCleanup 清理停机时卸载留下的目录（实例恢复 running 后）：同一实例上同一应用已有非终态部署时目录归它，只清标记；
// 否则 run.sh remove（停掉可能还在的进程再删目录，不只是 rm -rf）。exec 失败 30 秒后重试。
func (r *Runner) stepCleanup(ctx context.Context, d Deployment, ut upstream.Target, now time.Time) error {
	var live bool
	if err := r.s.d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_deployments WHERE instance_id = $1 AND app_id = $2 AND id <> $3
		AND state NOT IN ('removed', 'lost'))`, d.InstanceID, d.AppID, d.ID).Scan(&live); err != nil {
		return err
	}
	if live {
		r.log.Info("appdeploy: cleanup skipped, app redeployed on the instance", "deployment", d.ID)
	} else {
		app, err := r.s.d.Catalog.GetApp(ctx, d.AppID)
		if err != nil {
			return err
		}
		res, err := r.exec(ctx, ut, removeCmd(AppDir(r.appRoot(d.InstanceID), app.Slug)), nil, ExecUploadTimeout)
		if err != nil {
			r.log.Warn("appdeploy: cleanup failed, retry later", "deployment", d.ID, "err", err)
			return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
				_, err := tx.Exec(ctx, `UPDATE app_deployments SET next_poll_at = $2, row_version = row_version + 1 WHERE id = $1`, cur.ID, now.Add(UnreachableRetry))
				return err
			})
		}
		if res.exit != 0 {
			r.log.Warn("appdeploy: cleanup exited non-zero", "deployment", d.ID, "exit", res.exit, "stderr", lastLine(res.stderr))
		}
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		_, err := tx.Exec(ctx, `UPDATE app_deployments SET cleanup_pending = false, updated_at = $2, row_version = row_version + 1 WHERE id = $1`, cur.ID, now)
		return err
	})
}
