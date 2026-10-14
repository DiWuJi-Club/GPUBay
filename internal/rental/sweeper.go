package rental

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/db"
)

const (
	sweepBatch     = 200
	sweeperLockKey = 7301001 // pg_try_advisory_lock 的键，保证多个 platform 副本只有一个在清扫
)

// ExpireDue 把已到期、未暂停的运行中实例转为停机并下发停止指令。
func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	n := 0
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE state = 'running' AND suspended_at IS NULL AND expires_at <= $1
			ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now, sweepBatch))
		if err != nil {
			return err
		}
		for _, inst := range list {
			if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'stopped', stopped_at = $2, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
				return err
			}
			if err := recordEvent(ctx, tx, inst.ID, StateRunning, StateStopped, "expired", now); err != nil {
				return err
			}
			if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
				return err
			}
			if _, err := s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdStopInstance, nil); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
}

// WipeStale 洗掉停机超过保留期且没有待开始续费的实例。
func (s *Service) WipeStale(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	n := 0
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE state = 'stopped' AND pending_seconds = 0 AND stopped_at <= $1
			ORDER BY stopped_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now.Add(-RetainAfterStop), sweepBatch))
		if err != nil {
			return err
		}
		for _, inst := range list {
			if err := s.beginWipe(ctx, tx, inst, "retention expired", now); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
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

// FailStuckCreating 把开通超时的实例判为失败并全额退款。
func (s *Service) FailStuckCreating(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	n := 0
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE state = 'creating' AND created_at <= $1
			ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now.Add(-CreateTimeout), sweepBatch))
		if err != nil {
			return err
		}
		for _, inst := range list {
			if err := s.failCreating(ctx, tx, inst, "开通超时", now); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
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

// ResumeMachine 机器恢复在线后补回暂停的时长。
func (s *Service) ResumeMachine(ctx context.Context, machineID int64) (int, error) {
	now := s.d.Clock.Now()
	n := 0
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE machine_id = $1 AND state = 'running' AND suspended_at IS NOT NULL FOR UPDATE`, machineID))
		if err != nil {
			return err
		}
		for _, inst := range list {
			var remaining time.Duration
			if inst.PausedRemaining != nil {
				remaining = time.Duration(*inst.PausedRemaining) * time.Second
			}
			r24, r1 := reminderFlags(remaining)
			if _, err := tx.Exec(ctx, `UPDATE instances SET expires_at = $2, suspended_at = NULL, paused_remaining = NULL,
					reminded_24h = reminded_24h OR $3, reminded_1h = reminded_1h OR $4, updated_at = $5 WHERE id = $1`,
				inst.ID, now.Add(remaining), r24, r1, now); err != nil {
				return err
			}
			if err := recordEvent(ctx, tx, inst.ID, StateRunning, StateRunning, "machine online: lease resumed", now); err != nil {
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
	if _, err := s.SendReminders(ctx); err != nil {
		errs = append(errs, fmt.Errorf("reminders: %w", err))
	}
	return errors.Join(errs...)
}

// resumeOnline 恢复所有已在线但仍处于暂停状态的实例所在机器。
func (s *Service) resumeOnline(ctx context.Context) error {
	rows, err := s.d.Pool.Query(ctx, `SELECT DISTINCT machine_id FROM instances WHERE state = 'running' AND suspended_at IS NOT NULL`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	online, err := s.d.Fleet.OnlineAmong(ctx, ids)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range online {
		if _, err := s.ResumeMachine(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("machine %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// RunSweeper 每隔 interval 执行一次 Sweep，直到 ctx 结束。
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	s.sweepOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

func (s *Service) sweepOnce(ctx context.Context) {
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
}
