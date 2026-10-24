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
	// ImageEnviron 容器内镜像环境文件（Agent 的 init.sh 落下，见 imageenv.go）；空 = DefaultImageEnviron（测试注入）。
	ImageEnviron string
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
	// execHook 测试钩子：非 nil 且返回错误时 exec 不执行命令、直接返回该错误（注入非连接层的 exec 失败）。
	execHook func(cmd string) error
}

func NewRunner(s *Service, up *upstream.Pool, cfg RunnerConfig) *Runner {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = PollInterval
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = MaxExecPerRound
	}
	if cfg.ImageEnviron == "" {
		cfg.ImageEnviron = DefaultImageEnviron
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
// 例外：failed 且 desired = stopped 是「容器里可能还有进程、待停止」的标记（见 failAndStop）。
// 注：这一项不在 app_deployments_poll_idx 的谓词里，候选查询因此可能不走该部分索引；部署表规模（实例数 × 3 + 历史）下可接受。
const candidateCond = `(state IN ('queued', 'installing', 'running', 'removing') OR (state = 'removed' AND cleanup_pending)
	OR (state = 'failed' AND desired = 'stopped'))`

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
	actNone       action = iota
	actWait              // 实例非 running / 暂停中：不 exec（在事务 1 里推后 next_poll_at）
	actGone              // 实例已消失：removed(instance_gone)
	actCleanup           // removed 且 cleanup_pending：清理停机时卸载留下的目录
	actRemove            // removing / desired removed：run.sh remove
	actDeploy            // queued：上传部署包 + run.sh deploy
	actStop              // installing / running 且 desired stopped：run.sh stop
	actPoll              // installing：读 status
	actHealth            // running：读 status + HTTP 健康检查
	actStopFailed        // failed 且 desired = stopped：补做失败后的 run.sh stop
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
	case d.State == StateFailed:
		if d.Desired == DesiredStopped {
			return actStopFailed
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
	case actStopFailed:
		return true, r.stepStopFailed(ctx, d, ut, now)
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
	if r.execHook != nil {
		if err := r.execHook(cmd); err != nil {
			return execResult{exit: -1}, err
		}
	}
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
		return r.alertUnreachable(ctx, tx, cur.InstanceID, tg.MachineID)
	})
}

// alertUnreachable 告警「实例的应用部署无法连接容器」（未处理前同一实例只一条）。
// 同一实例的几个部署往往同一轮一起失败、各自一个事务：RaiseAlertOnce 的「查重再插入」要按实例串行，否则各插一条。
func (r *Runner) alertUnreachable(ctx context.Context, tx pgx.Tx, instanceID string, machineID int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "app_deploy_unreachable:"+instanceID); err != nil {
		return err
	}
	return r.s.d.Fleet.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "warning", Kind: "app_deploy_unreachable", MachineID: machineID,
		InstanceID: instanceID, Message: rental.AlertAppDeployUnreachable(instanceID)})
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

// deployBusyExit 是 run.sh deploy 拿不到部署锁（上一次 deploy 的 exec 超时后仍在容器里跑）时的退出码。
const (
	deployBusyExit = 75
	// DeployBusyMax：一直 busy 超过 run.sh 陈旧锁时限（120 秒）的两倍 → failed(deploy_busy)（兜底，正常不会发生）。
	DeployBusyMax     = 2 * 120 * time.Second
	ErrCodeDeployBusy = "deploy_busy"
	phaseBusy         = "busy"
)

func init() { ErrorText[ErrCodeDeployBusy] = "上一次部署未结束" }

// deployBusy：上一次 deploy 还没结束 → 保持 queued，30 秒后再试。phase = busy、started_at 记「从何时开始等」，
// 等了超过 DeployBusyMax 就 failed(deploy_busy)（并按失败后停进程处理）。
// attempt：上传已经成功（目录与带 secret 的 env 已在容器里），不能退回 0，否则停机时卸载不会置 cleanup_pending；
// 但也不让每次 busy 重试累加：退回本轮的 +1，下限 1。
func (r *Runner) deployBusy(ctx context.Context, d Deployment, ut upstream.Target, dir string, now time.Time) error {
	if d.Phase == phaseBusy && d.StartedAt != nil && now.Sub(*d.StartedAt) > DeployBusyMax {
		undo := func(tx pgx.Tx, cur Deployment) error { // 本轮没有启动任何东西：同样退回本轮的 +1
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET attempt = GREATEST(attempt - 1, 1), row_version = row_version + 1 WHERE id = $1`, cur.ID)
			return err
		}
		return r.failAndStop(ctx, d, ut, dir, ErrCodeDeployBusy, fmt.Sprintf("上一次部署超过 %d 分钟仍未结束", int(DeployBusyMax/time.Minute)), now, undo)
	}
	r.log.Info("appdeploy: previous deploy still running in container, retry later", "deployment", d.ID)
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		_, err := tx.Exec(ctx, `UPDATE app_deployments SET attempt = GREATEST(attempt - 1, 1), next_poll_at = $2, updated_at = $3,
			phase = 'busy', message = '上一次部署仍在进行，稍后重试', started_at = CASE WHEN phase = 'busy' AND started_at IS NOT NULL THEN started_at ELSE $3 END,
			row_version = row_version + 1 WHERE id = $1`, cur.ID, now.Add(UnreachableRetry), now)
		return err
	})
}

// readImageEnv 读容器的镜像环境（验收缺陷 A-1，见 imageenv.go）。连接层错误原样返回（与上传一样按 unreachable 处理，
// 反正上传也发不出去）；其余失败（文件不在：旧版 Agent 的 init.sh 不落文件、实例升级 Agent 后还没重启过；命令超时；
// 输出不是 base64）记 warn 并返回 nil：照常部署，只是包里不带 image.env——镜像环境缺失只影响依赖它的应用，
// 不应让所有部署都失败；目录里上一次部署留下的 image.env 保留（同一实例的镜像不会变）。
func (r *Runner) readImageEnv(ctx context.Context, d Deployment, ut upstream.Target) ([]EnvKV, error) {
	res, err := r.exec(ctx, ut, imageEnvironCmd(r.cfg.ImageEnviron), nil, ExecStatusTimeout)
	if isDial(err) {
		return nil, err
	}
	if err == nil && res.exit != 0 {
		err = fmt.Errorf("exit %d", res.exit)
	}
	var kvs []EnvKV
	if err == nil {
		kvs, err = DecodeImageEnviron(res.stdout)
	}
	if err != nil {
		r.log.Warn("appdeploy: cannot read image environment, deploying without image.env", "deployment", d.ID, "instance", d.InstanceID,
			"path", r.cfg.ImageEnviron, "err", err)
		return nil, nil
	}
	return kvs, nil
}

func (r *Runner) stepDeploy(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	root := r.appRoot(d.InstanceID)
	imageEnv, err := r.readImageEnv(ctx, d, ut)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err, true)
	}
	bundle, app, err := r.s.bundleFor(ctx, d, root, r.cfg.HealthAddr, imageEnv)
	dir := AppDir(root, app.Slug)
	var be *bundleError
	if errors.As(err, &be) { // 重新部署时旧服务可能还在跑：一并停掉
		return r.failAndStop(ctx, d, ut, dir, ErrCodeUploadFailed, "部署包生成失败："+be.Error(), now, nil)
	}
	if err != nil {
		return err
	}
	res, err := r.exec(ctx, ut, UploadCommand(dir), bundle, ExecUploadTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err, true)
	}
	if err != nil {
		return err
	}
	if res.exit != 0 {
		return r.failAndStop(ctx, d, ut, dir, ErrCodeUploadFailed, "上传失败："+lastLine(res.stderr), now, nil)
	}
	res, err = r.exec(ctx, ut, runSHCmd(dir, "deploy"), nil, ExecStatusTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err, false)
	}
	if err != nil {
		return err
	}
	if res.exit == deployBusyExit {
		return r.deployBusy(ctx, d, ut, dir, now)
	}
	if res.exit != 0 {
		code, msg := ErrCodeScriptFailed, lastLine(res.stderr)
		if strings.HasPrefix(msg, "disk:") {
			code = ErrCodeDisk
		}
		return r.failAndStop(ctx, d, ut, dir, code, msg, now, nil)
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

// ---- 失败后停进程 ----
//
// 标 failed 时容器里可能还有进程（超时的 _bg、端口冲突时已起来的服务、重新部署失败时的旧服务、status.json 被删但服务还在）：
// failAndStop 在同一事务里标 failed 并置 desired = stopped（failed 行上的「待停止」标记，Runner 候选之一），提交后立即
// best-effort run.sh stop；成功（或目录 / run.sh 已不在，无可停）→ desired 改回 running、next_poll_at 推后 24 小时；
// exec 失败 → 标记保留，30 秒后 Runner 重试（actStopFailed）。用户「重新部署」把 desired 置回 running、「卸载」置为 removed，
// 都会接管这一行（deploy / remove 本身也会先停进程）。

// failAndStop 见上。before 非 nil 时在标 failed 之前于同一事务内执行（写 log_tail 等）。
func (r *Runner) failAndStop(ctx context.Context, d Deployment, ut upstream.Target, dir, code, msg string, now time.Time,
	before func(tx pgx.Tx, cur Deployment) error) error {
	var rv int
	committed := false
	err := r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		if before != nil {
			if err := before(tx, cur); err != nil {
				return err
			}
		}
		if err := r.markFailedStopping(ctx, tx, cur, code, msg, now, &rv); err != nil {
			return err
		}
		committed = true
		return nil
	})
	if err != nil || !committed {
		return err
	}
	d.RowVersion = rv
	return r.stopFailed(ctx, d, ut, dir, now)
}

// markFailedStopping = markFailed + 「待停止」标记（desired = stopped）；rv 收到提交后的 row_version。
func (r *Runner) markFailedStopping(ctx context.Context, tx pgx.Tx, cur Deployment, code, msg string, now time.Time, rv *int) error {
	if err := r.s.markFailed(ctx, tx, cur, code, msg, now); err != nil {
		return err
	}
	return tx.QueryRow(ctx, `UPDATE app_deployments SET desired = 'stopped', unreachable_count = 0, next_poll_at = $2, row_version = row_version + 1
		WHERE id = $1 RETURNING row_version`, cur.ID, now.Add(r.cfg.PollInterval)).Scan(rv)
}

// stopFailed 对带「待停止」标记的 failed 部署执行 run.sh stop（幂等），成功后清标记。
func (r *Runner) stopFailed(ctx context.Context, d Deployment, ut upstream.Target, dir string, now time.Time) error {
	res, err := r.exec(ctx, ut, runSHCmd(dir, "stop"), nil, ExecStatusTimeout)
	if isDial(err) {
		// 连不上容器：与其它步骤一样计 unreachable_count；到阈值放弃（清标记）并告警，不再无限重试
		r.log.Warn("appdeploy: stop after failure: container unreachable", "deployment", d.ID, "count", d.UnreachableCount+1, "err", err)
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			n := cur.UnreachableCount + 1
			if n < UnreachableFailAfter {
				_, err := tx.Exec(ctx, `UPDATE app_deployments SET unreachable_count = $2, next_poll_at = $3, row_version = row_version + 1 WHERE id = $1`,
					cur.ID, n, now.Add(UnreachableRetry))
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET unreachable_count = $2, desired = 'running', next_poll_at = $3, updated_at = $4,
				row_version = row_version + 1 WHERE id = $1`, cur.ID, n, now.Add(24*time.Hour), now); err != nil {
				return err
			}
			if err := addEvent(ctx, tx, cur.ID, cur.State, cur.State, "无法连接容器，未能停止容器内的进程", now); err != nil {
				return err
			}
			var machineID int64
			if err := tx.QueryRow(ctx, `SELECT machine_id FROM instances WHERE id = $1`, cur.InstanceID).Scan(&machineID); err != nil {
				return err
			}
			return r.alertUnreachable(ctx, tx, cur.InstanceID, machineID)
		})
	}
	if err != nil {
		r.log.Warn("appdeploy: stop after failure did not run, retry later", "deployment", d.ID, "err", err)
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET next_poll_at = $2, row_version = row_version + 1 WHERE id = $1`, cur.ID, now.Add(UnreachableRetry))
			return err
		})
	}
	if res.exit != 0 { // 目录 / run.sh 已不在：没有可停的进程
		r.logStderr(d, "appdeploy: run.sh stop after failure exited non-zero", res)
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		_, err := tx.Exec(ctx, `UPDATE app_deployments SET desired = 'running', unreachable_count = 0, next_poll_at = $2, updated_at = $3, row_version = row_version + 1 WHERE id = $1`,
			cur.ID, now.Add(24*time.Hour), now)
		return err
	})
}

func (r *Runner) stepStopFailed(ctx context.Context, d Deployment, ut upstream.Target, now time.Time) error {
	app, err := r.s.d.Catalog.GetApp(ctx, d.AppID)
	if err != nil {
		return err
	}
	return r.stopFailed(ctx, d, ut, AppDir(r.appRoot(d.InstanceID), app.Slug), now)
}

// logStderr 记一条带 stderr 的警告：只记脱敏后的最后一行与总长度（stderr 可能含变量值）。
func (r *Runner) logStderr(d Deployment, msg string, res execResult) {
	r.log.Warn(msg, "deployment", d.ID, "exit", res.exit, "stderr_last", r.redactor(d)(lastLine(res.stderr)), "stderr_len", len(res.stderr))
}

// ---- 轮询 ----

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
	minutes := ver.Spec.TimeoutMinutes
	if minutes <= 0 {
		minutes = DefaultTimeoutMinutes
	}
	timedOut := d.StartedAt != nil && now.Sub(*d.StartedAt) > time.Duration(minutes)*time.Minute
	timeoutMsg := fmt.Sprintf("超过 %d 分钟未完成", minutes)
	res, err := r.exec(ctx, ut, statusCmd(dir), nil, ExecStatusTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err, false)
	}
	if err != nil {
		if timedOut { // 状态读不出来（命令超时等）也不能让超时失效
			return r.failAndStop(ctx, d, ut, dir, ErrCodeTimeout, timeoutMsg, now, nil)
		}
		return err
	}
	if res.exit != 0 {
		return r.failAndStop(ctx, d, ut, dir, ErrCodeScriptFailed, statusMissing, now, nil)
	}
	st, _, rawTail, perr := ParseStatusOutput(res.stdout)
	fresh := perr == nil && st.Seq >= d.StatusSeq
	red := r.redactor(d)
	logTail := red(tidyLog(rawTail))
	msg, phase := truncRunes(red(cleanText(st.Message)), MaxMessageLen), truncRunes(red(cleanText(st.Phase)), 64)
	// 超时先于「解析失败 / seq 回退就不改状态」判断：status.json 被写坏或回退时超时照样生效。
	// 只有新鲜且已离开 installing 的状态（running / failed / stopped）按状态处理。
	if timedOut && (!fresh || st.State == StateInstalling) {
		var before func(tx pgx.Tx, cur Deployment) error
		if fresh {
			before = func(tx pgx.Tx, cur Deployment) error {
				_, err := tx.Exec(ctx, `UPDATE app_deployments SET log_tail = $2, status_seq = $3, phase = $4, row_version = row_version + 1 WHERE id = $1`,
					cur.ID, logTail, st.Seq, phase)
				return err
			}
		}
		return r.failAndStop(ctx, d, ut, dir, ErrCodeTimeout, timeoutMsg, now, before)
	}
	if perr != nil {
		r.log.Warn("appdeploy: bad status output", "deployment", d.ID, "err", perr)
		return nil
	}
	if !fresh {
		r.log.Info("appdeploy: status seq went back, ignored", "deployment", d.ID, "seq", st.Seq, "have", d.StatusSeq)
		return nil
	}
	var rv int
	stopAfter := false
	err = r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
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
			if code != "" { // 端口登记不了：服务已在容器里跑起来，标 failed 后停掉
				if _, err := tx.Exec(ctx, `UPDATE app_deployments SET log_tail = $2, status_seq = $3, row_version = row_version + 1 WHERE id = $1`,
					cur.ID, logTail, st.Seq); err != nil {
					return err
				}
				stopAfter = true
				return r.markFailedStopping(ctx, tx, cur, code, ErrorText[code], now, &rv)
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
	if err != nil || !stopAfter {
		return err
	}
	d.RowVersion = rv
	return r.stopFailed(ctx, d, ut, dir, now)
}

func (r *Runner) stepHealth(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	app, ver, err := r.s.appAndVersion(ctx, d)
	if err != nil {
		return err
	}
	dir := AppDir(r.appRoot(d.InstanceID), app.Slug)
	res, err := r.exec(ctx, ut, statusCmd(dir), nil, ExecStatusTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err, false)
	}
	if err != nil { // 非连接层错误（命令超时、上游拒绝执行）：按健康检查周期退避，健康未知
		if werr := r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET health = 'unknown', next_poll_at = $2, row_version = row_version + 1 WHERE id = $1`,
				cur.ID, now.Add(HealthInterval))
			return err
		}); werr != nil {
			return werr
		}
		return err
	}
	if res.exit != 0 {
		return r.failAndStop(ctx, d, ut, dir, ErrCodeScriptFailed, statusMissing, now, nil)
	}
	st, alive, rawTail, perr := ParseStatusOutput(res.stdout)
	if perr != nil {
		r.log.Warn("appdeploy: bad status output", "deployment", d.ID, "err", perr)
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET health = 'unknown', next_poll_at = $2, updated_at = $3, row_version = row_version + 1 WHERE id = $1`,
				cur.ID, now.Add(HealthInterval), now)
			return err
		})
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
		r.logStderr(d, "appdeploy: run.sh stop failed, treating as stopped", res)
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
		r.logStderr(d, "appdeploy: remove exited non-zero, marking removed", res)
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
			r.logStderr(d, "appdeploy: cleanup exited non-zero", res)
		}
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		_, err := tx.Exec(ctx, `UPDATE app_deployments SET cleanup_pending = false, updated_at = $2, row_version = row_version + 1 WHERE id = $1`, cur.ID, now)
		return err
	})
}
