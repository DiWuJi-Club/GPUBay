package rental

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

const (
	sweepBatch     = 200
	sweeperLockKey = 7301001 // pg_try_advisory_lock 的键，保证多个 platform 副本只有一个在清扫
)

// extraTaskTimeout 限制每个附加任务的执行时间，避免外部调用卡住后一直占着清扫咨询锁（测试可调小）。
var extraTaskTimeout = 2 * time.Minute

// RefreshCappedLeases 让停机、洗机中与失联暂停实例的占用上界随时间推进（并封顶于下一条预订）：
// 这些实例的 occupiedUntil 以 now 为基准，不刷新的话上界会「过期」，后来的预订会把机器当空闲挑走。
// sweepEach 的 pred 必须以 $1 引用参数，这里没有时间条件，用恒真的 created_at <= now 占位。
// 候选按 instances.updated_at 取前 sweepBatch 条，处理后把 updated_at 推到 now 轮转到队尾，
// 否则候选超过一批时总是同一批最旧的行被处理，其余实例的上界永远得不到刷新。
func (s *Service) RefreshCappedLeases(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	return s.sweepEach(ctx, `(state IN ('stopped', 'wiping') OR (state = 'running' AND suspended_at IS NOT NULL)) AND created_at <= $1`, now, "updated_at",
		func(tx pgx.Tx, inst Instance) error {
			if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE instances SET updated_at = $2 WHERE id = $1 AND updated_at < $2`, inst.ID, now)
			return err
		})
}

// ExpireDue 把已到期、未暂停的运行中实例转为停机并下发停止指令。
func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	return s.sweepEach(ctx, `state = 'running' AND suspended_at IS NULL AND expires_at <= $1`, now, "expires_at",
		func(tx pgx.Tx, inst Instance) error {
			if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'stopped', stopped_at = $2, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
				return err
			}
			if err := recordEvent(ctx, tx, inst.ID, StateRunning, StateStopped, "expired", now); err != nil {
				return err
			}
			if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
				return err
			}
			_, err := s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdStopInstance, nil)
			return err
		})
}

// WipeStale 洗掉停机超过保留期且没有待开始续费的实例。
func (s *Service) WipeStale(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	return s.sweepEach(ctx, `state = 'stopped' AND pending_seconds = 0 AND stopped_at <= $1`, now.Add(-RetainAfterStop), "stopped_at",
		func(tx pgx.Tx, inst Instance) error { return s.beginWipe(ctx, tx, inst, "retention expired", now) })
}

// sweepEach 先不加锁地选出满足 pred（以 $1 引用 arg）的候选实例，再逐个在独立事务内处理：
// 加锁时用 SKIP LOCKED 并重新校验 pred，已被其他清扫处理或状态已变的行直接跳过；
// 单行失败只回滚该行，错误汇总返回，其余行继续处理。返回成功处理的行数。
func (s *Service) sweepEach(ctx context.Context, pred string, arg any, orderBy string, fn func(pgx.Tx, Instance) error) (int, error) {
	return s.sweepEachAfter(ctx, pred, arg, orderBy, func(tx pgx.Tx, inst Instance) (func(), error) { return nil, fn(tx, inst) }, nil)
}

// sweepEachAfter 同 sweepEach，但回调可返回一个在该行事务提交后执行的动作（发短信等外部调用不进事务），
// 单行失败时先回调 onErr（用于在独立事务里告警）。单行事务遇 40P01 / 40001 重试一次（改派路径要求，规格 §14）。
func (s *Service) sweepEachAfter(ctx context.Context, pred string, arg any, orderBy string,
	fn func(pgx.Tx, Instance) (func(), error), onErr func(id string, err error)) (int, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT id FROM instances WHERE `+pred+` ORDER BY `+orderBy+` LIMIT $2`, arg, sweepBatch)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		var done bool
		var after func()
		err := withRetry(func() error {
			done, after = false, nil
			return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
				inst, err := scanInstance(tx.QueryRow(ctx, `SELECT `+instanceCols+` FROM instances
					WHERE id = $2 AND `+pred+` FOR UPDATE SKIP LOCKED`, arg, id))
				if errors.Is(err, apperr.ErrNotFound) {
					return nil
				}
				if err != nil {
					return err
				}
				after, err = fn(tx, inst)
				if err != nil {
					return err
				}
				done = true
				return nil
			})
		})
		if err != nil {
			if onErr != nil {
				onErr(id, err)
			}
			errs = append(errs, fmt.Errorf("instance %s: %w", id, err))
			continue
		}
		if after != nil {
			after()
		}
		if done {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// beginWipe 把实例转为 wiping：取消该实例未完成的指令，再下发洗机指令。
func (s *Service) beginWipe(ctx context.Context, tx pgx.Tx, inst Instance, reason string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'wiping', suspended_at = NULL, paused_remaining = NULL, updated_at = $2
		WHERE id = $1`, inst.ID, now); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, inst.ID, inst.State, StateWiping, reason, now); err != nil {
		return err
	}
	if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
		return err
	}
	if err := s.d.Fleet.CancelCommands(ctx, tx, inst.ID, "superseded by wipe"); err != nil {
		return err
	}
	_, err := s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdWipeInstance, nil)
	return err
}

// FailStuckCreating 把开通超时的实例判为失败并全额退款；预订实例改为改派（规格 §5.4）。
// 超时从预订的开通时刻（provisioned_at）起算，即时订单仍从 created_at 起算。
func (s *Service) FailStuckCreating(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	return s.sweepEachAfter(ctx, `state = 'creating' AND COALESCE(provisioned_at, created_at) <= $1`, now.Add(-CreateTimeout), "created_at",
		func(tx pgx.Tx, inst Instance) (func(), error) {
			if inst.Booked() {
				var after func()
				err := s.reassignAfterCreateFailure(ctx, tx, inst, "开通超时", now, &after)
				return after, err
			}
			return nil, s.failCreating(ctx, tx, inst, "开通超时", now)
		}, s.alertCancelFailed)
}

// SuspendMachines 冻结失联机器上运行中实例的剩余时长，暂停期间不计时、不会被判到期。
func (s *Service) SuspendMachines(ctx context.Context, machineIDs []int64) (int, error) {
	if len(machineIDs) == 0 {
		return 0, nil
	}
	now := s.d.Clock.Now()
	n := 0
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE machine_id = ANY($1) AND state = 'running' AND suspended_at IS NULL FOR UPDATE`, machineIDs))
		if err != nil {
			return err
		}
		for _, inst := range list {
			remaining := int64(inst.Remaining(now) / time.Second)
			if _, err := tx.Exec(ctx, `UPDATE instances SET suspended_at = $2, paused_remaining = $3, updated_at = $2 WHERE id = $1`,
				inst.ID, now, remaining); err != nil {
				return err
			}
			if err := recordEvent(ctx, tx, inst.ID, StateRunning, StateRunning, "machine offline: lease paused", now); err != nil {
				return err
			}
			if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
}

// CommandTimeout 返回指令类型的超时值。
func CommandTimeout(t fleet.CommandType) time.Duration {
	switch t {
	case fleet.CmdCreateInstance:
		return CreateCommandTimeout
	case fleet.CmdStartInstance:
		return StartCommandTimeout
	case fleet.CmdStopInstance:
		return StopCommandTimeout
	case fleet.CmdWipeInstance:
		return WipeCommandTimeout
	}
	return PullCommandTimeout
}

// FailTimedOutCommands 把在线机器上超过类型超时仍未回报的 sent 指令判为失败，交给状态机。
func (s *Service) FailTimedOutCommands(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	cmds, err := s.d.Fleet.SentBefore(ctx, now.Add(-StartCommandTimeout)) // 最短的超时值
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, c := range cmds {
		timeout := CommandTimeout(c.Type)
		if c.SentAt == nil || now.Sub(*c.SentAt) < timeout {
			continue
		}
		if err := s.HandleCommandResult(ctx, c.ID, false, "timeout after "+timeout.String()); err != nil {
			errs = append(errs, fmt.Errorf("command %s: %w", c.ID, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// ResumeMachine 机器恢复在线后，只对容器确实在运行（runningIDs）的暂停实例补回时长并重算提醒标记
// （到期 = resumeBase + 冻结的剩余时长：预订实例在开始时刻前恢复，仍到 S + 已付时长）；
// 其余暂停实例保持暂停并告警 container_down，由管理员换机或退款。
func (s *Service) ResumeMachine(ctx context.Context, machineID int64, runningIDs []string) (int, error) {
	now := s.d.Clock.Now()
	n := 0
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE machine_id = $1 AND state = 'running' AND suspended_at IS NOT NULL ORDER BY id FOR UPDATE`, machineID))
		if err != nil || len(list) == 0 {
			return err
		}
		m, err := s.d.Fleet.Get(ctx, tx, machineID)
		if err != nil {
			return err
		}
		for _, inst := range list {
			if !slices.Contains(runningIDs, inst.ID) {
				if err := s.d.Fleet.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "critical", Kind: "container_down",
					MachineID: machineID, InstanceID: inst.ID, Message: AlertContainerDown(m.Serial, inst.ID)}); err != nil {
					return err
				}
				continue
			}
			var remaining time.Duration
			if inst.PausedRemaining != nil {
				remaining = time.Duration(*inst.PausedRemaining) * time.Second
			}
			expires := inst.resumeBase(now).Add(remaining)
			r24, r1 := reminderFlags(expires.Sub(now))
			if _, err := tx.Exec(ctx, `UPDATE instances SET expires_at = $2, suspended_at = NULL, paused_remaining = NULL,
					reminded_24h = $3, reminded_1h = $4, updated_at = $5 WHERE id = $1`,
				inst.ID, expires, r24, r1, now); err != nil {
				return err
			}
			if err := recordEvent(ctx, tx, inst.ID, StateRunning, StateRunning, "machine online: lease resumed", now); err != nil {
				return err
			}
			if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
				return err
			}
			short, err := capExpiryToLease(ctx, tx, inst.ID, expires, now)
			if err != nil {
				return err
			}
			// 故障停机是平台原因：恢复后被下一条预订封顶吃掉的时长按单价补偿（同迟开通补偿）
			if short > 0 {
				if err := s.compensateShortfall(ctx, tx, inst, int64(short/time.Second), "故障恢复补偿 ", "resume capped compensated", now); err != nil {
					return err
				}
			}
			n++
		}
		return nil
	})
	return n, err
}

// capExpiryToLease 恢复后 refreshLease 可能把上界封顶在同机下一条预订的下界：到期随之封顶为上界 − Tail（规格 §4.3），
// 不能让到期越过别人已付的预订。返回被封顶吃掉的时长（没有封顶为 0）。
func capExpiryToLease(ctx context.Context, tx pgx.Tx, id string, expires, now time.Time) (time.Duration, error) {
	var upper time.Time
	if err := tx.QueryRow(ctx, `SELECT upper(period) FROM machine_leases WHERE instance_id = $1 AND status = 'active' ORDER BY id DESC LIMIT 1`, id).Scan(&upper); err != nil {
		if db.IsNoRows(err) {
			return 0, nil
		}
		return 0, err
	}
	capped := upper.Add(-Tail)
	if !expires.After(capped) {
		return 0, nil
	}
	r24, r1 := reminderFlags(capped.Sub(now))
	_, err := tx.Exec(ctx, `UPDATE instances SET expires_at = $2, reminded_24h = $3, reminded_1h = $4, updated_at = $5 WHERE id = $1`, id, capped, r24, r1, now)
	return expires.Sub(capped), err
}

type reminder struct {
	userID    int64
	id        string
	remaining time.Duration
}

// SendReminders 对剩余不足 24 小时、1 小时的实例各发一次短信提醒（短信在事务提交后发送）。
func (s *Service) SendReminders(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	var due []reminder
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE state = 'running' AND suspended_at IS NULL AND (
				(NOT reminded_24h AND expires_at <= $1) OR (NOT reminded_1h AND expires_at <= $2))
			ORDER BY expires_at LIMIT $3 FOR UPDATE SKIP LOCKED`, now.Add(24*time.Hour), now.Add(time.Hour), sweepBatch))
		if err != nil {
			return err
		}
		for _, inst := range list {
			remaining := inst.ExpiresAt.Sub(now)
			if _, err := tx.Exec(ctx, `UPDATE instances SET reminded_24h = true, reminded_1h = reminded_1h OR $2, updated_at = $3
				WHERE id = $1`, inst.ID, remaining <= time.Hour, now); err != nil {
				return err
			}
			due = append(due, reminder{userID: inst.UserID, id: inst.ID, remaining: remaining})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, r := range due {
		if err := s.d.Notifier.NotifyExpiry(ctx, r.userID, r.id, r.remaining); err != nil {
			s.d.Log.Warn("expiry reminder failed", "instance", r.id, "err", err)
		}
	}
	return len(due), nil
}

// Sweep 执行一轮完整清扫：离线检测与暂停、到期停机、保留期洗机、开通超时、到期提醒。
func (s *Service) Sweep(ctx context.Context) error {
	var errs []error
	offline, err := s.d.Fleet.MarkOfflineStale(ctx, OfflineTimeout)
	if err != nil {
		errs = append(errs, fmt.Errorf("mark offline: %w", err))
	}
	for _, id := range offline {
		if err := s.d.Fleet.RaiseAlert(ctx, s.d.Pool, fleet.Alert{Level: "warning", Kind: "machine_offline",
			MachineID: id, Message: "Agent 失联超过 5 分钟"}); err != nil {
			errs = append(errs, fmt.Errorf("alert offline: %w", err))
		}
	}
	// 每轮都按当前状态对账，修复上一轮可能漏掉的暂停/恢复。
	if down, err := s.d.Fleet.OfflineMachineIDs(ctx); err != nil {
		errs = append(errs, fmt.Errorf("list offline: %w", err))
	} else if _, err := s.SuspendMachines(ctx, down); err != nil {
		errs = append(errs, fmt.Errorf("suspend: %w", err))
	}
	if err := s.resumeOnline(ctx); err != nil {
		errs = append(errs, fmt.Errorf("resume: %w", err))
	}
	if _, err := s.ExpireDue(ctx); err != nil {
		errs = append(errs, fmt.Errorf("expire: %w", err))
	}
	if _, err := s.WipeStale(ctx); err != nil {
		errs = append(errs, fmt.Errorf("wipe stale: %w", err))
	}
	if _, err := s.FailStuckCreating(ctx); err != nil {
		errs = append(errs, fmt.Errorf("creating timeout: %w", err))
	}
	if _, err := s.FailTimedOutCommands(ctx); err != nil {
		errs = append(errs, fmt.Errorf("command timeout: %w", err))
	}
	if _, _, err := s.ReapSSHSessions(ctx); err != nil {
		errs = append(errs, fmt.Errorf("ssh sessions: %w", err))
	}
	if _, err := s.SendReminders(ctx); err != nil {
		errs = append(errs, fmt.Errorf("reminders: %w", err))
	}
	return errors.Join(errs...)
}

// resumeOnline 清扫器兜底：对已在线、且本次连接之后上报过 StateReport 的机器，按报告里 running 的实例恢复计时。
func (s *Service) resumeOnline(ctx context.Context) error {
	rows, err := s.d.Pool.Query(ctx, `SELECT DISTINCT machine_id FROM instances WHERE state = 'running' AND suspended_at IS NOT NULL ORDER BY machine_id`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	reports, err := s.d.Fleet.ReportsFor(ctx, ids)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		rep, ok := reports[id]
		if !ok {
			continue
		}
		if _, err := s.ResumeMachine(ctx, id, rep.RunningIDs()); err != nil {
			errs = append(errs, fmt.Errorf("machine %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// RunSweeper 每隔 interval 执行一次 Sweep 及附加任务（例如充值补查与关单），直到 ctx 结束。
// 多个 platform 副本中只有拿到咨询锁的一个会执行。附加任务出错只记日志，不影响后续任务。
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration, extra ...func(context.Context) error) {
	s.sweepOnce(ctx, extra)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOnce(ctx, extra)
		}
	}
}

func (s *Service) sweepOnce(ctx context.Context, extra []func(context.Context) error) {
	conn, err := s.d.Pool.Acquire(ctx)
	if err != nil {
		s.d.Log.Error("sweeper acquire conn", "err", err)
		return
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, sweeperLockKey).Scan(&locked); err != nil {
		s.d.Log.Error("sweeper advisory lock", "err", err)
		return
	}
	if !locked {
		return
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, sweeperLockKey) //nolint:errcheck
	if err := s.Sweep(ctx); err != nil {
		s.d.Log.Error("sweep failed", "err", err)
	}
	for i, task := range extra {
		tctx, cancel := context.WithTimeout(ctx, extraTaskTimeout)
		err := task(tctx)
		cancel()
		if err != nil {
			s.d.Log.Error("sweep task failed", "task", i, "err", err)
		}
	}
}
