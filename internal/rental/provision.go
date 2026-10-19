package rental

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/tz"
)

// cancelFailure 标记「平台取消本身失败」（例如退款流水冲突）：调用方据此在独立事务里告警，否则只有日志可见。
type cancelFailure struct{ err error }

func (c cancelFailure) Error() string { return "booking cancel failed: " + c.err.Error() }
func (c cancelFailure) Unwrap() error { return c.err }

// alertCancelFailed 在平台取消失败（err 链里有 cancelFailure）时，于独立事务（取消事务已回滚）里告警 booking_cancel_failed；
// 其它错误不做任何事。签名与 sweepEachAfter 的 onErr 一致。
func (s *Service) alertCancelFailed(id string, err error) {
	var cf cancelFailure
	if id == "" || !errors.As(err, &cf) {
		return
	}
	s.d.Log.Error("booking cancel failed", "instance", id, "err", err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // 调用方的 ctx 可能已取消，告警仍要写进去
	defer cancel()
	if aerr := s.d.Fleet.RaiseAlertOnce(ctx, s.d.Pool, fleet.Alert{Level: "critical", Kind: "booking_cancel_failed", InstanceID: id,
		Message: AlertBookingCancelFailed(id)}); aerr != nil {
		s.d.Log.Error("raise booking_cancel_failed", "instance", id, "err", aerr)
	}
}

// ProvisionDue 到点开通（规格 §5.2）：开始前 PrepLead 的预订实例逐个开通或改派；开始后仍无机器超过 BookingGrace 则平台取消。
// 谓词命中部分索引 instances_reserved_idx (scheduled_start_at) WHERE state = 'reserved'。
// 返回值 n 是「本轮处理过的行」：开通、改派、取消，以及「无候选仍在宽限期内等待」的行都计入；测试按状态断言，不按 n。
func (s *Service) ProvisionDue(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	return s.sweepEachAfter(ctx, `state = 'reserved' AND scheduled_start_at <= $1`, now.Add(PrepLead), "scheduled_start_at",
		func(tx pgx.Tx, inst Instance) (func(), error) { return s.provision(ctx, tx, inst, now) },
		s.alertCancelFailed)
}

// provision 对一个到点的预订实例（调用方已锁实例）：复核租用资格 → 机器可用则开通，否则同机组改派；
// 都不行：宽限期内保持 reserved 并告警，过了宽限期平台取消。返回事务提交后要执行的动作（发短信）。
func (s *Service) provision(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) (func(), error) {
	if err := s.d.Users.CanRent(ctx, inst.UserID); err != nil {
		if apperr.As(err) == nil {
			return nil, err
		}
		if _, cerr := s.cancelPlatform(ctx, tx, inst, "cancelled: user cannot rent", now); cerr != nil {
			return nil, cancelFailure{cerr}
		}
		return s.smsCancelled(inst, "账号不可租用"), nil
	}
	lease, err := lockLease(ctx, tx, inst.ID)
	if err != nil {
		return nil, err
	}
	ready, err := s.machineReady(ctx, tx, inst.MachineID)
	if err != nil {
		return nil, err
	}
	if !ready {
		moved, err := s.reassign(ctx, tx, &inst, &lease, "machine unavailable at start", true, now)
		if err != nil {
			return nil, err
		}
		if !moved {
			if now.Before(inst.ScheduledStartAt.Add(BookingGrace)) {
				return nil, s.d.Fleet.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "warning", Kind: "booking_at_risk",
					MachineID: inst.MachineID, InstanceID: inst.ID, Message: AlertBookingAtRisk(inst.ID)})
			}
			return s.cancelUnfulfilled(ctx, tx, inst, inst.MachineID, "unfulfilled: no machine by grace", now)
		}
	}
	return nil, s.startProvision(ctx, tx, inst, lease, now)
}

// cancelUnfulfilled 过了宽限期仍无机器：平台取消（全退）+ booking_unfulfilled 告警；失败包成 cancelFailure。返回提交后发短信的动作。
func (s *Service) cancelUnfulfilled(ctx context.Context, tx pgx.Tx, inst Instance, machineID int64, reason string, now time.Time) (func(), error) {
	if _, err := s.cancelPlatform(ctx, tx, inst, reason, now); err != nil {
		return nil, cancelFailure{err}
	}
	if err := s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "booking_unfulfilled",
		MachineID: machineID, InstanceID: inst.ID, Message: AlertBookingUnfulfilled(inst.ID)}); err != nil {
		return nil, cancelFailure{err}
	}
	return s.smsCancelled(inst, "无法安排机器"), nil
}

func (s *Service) smsCancelled(inst Instance, reason string) func() {
	return func() {
		if err := s.d.Notifier.NotifyBookingCancelled(context.Background(), inst.UserID, inst.ID, reason); err != nil {
			s.d.Log.Warn("booking cancelled sms failed", "instance", inst.ID, "err", err)
		}
	}
}

// machineReady 锁机器行（NO KEY UPDATE：随后要插 agent_commands，FOR UPDATE 会与其外键 KEY SHARE 冲突）并判断能否现在开通：空闲、在线、没有活动实例。
func (s *Service) machineReady(ctx context.Context, tx pgx.Tx, machineID int64) (bool, error) {
	var state string
	var online bool
	err := tx.QueryRow(ctx, `SELECT state, agent_online FROM machines WHERE id = $1 FOR NO KEY UPDATE`, machineID).Scan(&state, &online)
	if db.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state != fleet.StateIdle || !online {
		return false, nil
	}
	var busy bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instances WHERE machine_id = $1 AND state IN ('creating', 'running', 'stopped', 'wiping'))`, machineID).Scan(&busy)
	return !busy, err
}

// startProvision：机器 allocated、实例 creating（provisioned_at = now）、lease active、入队 Create（规格 §5.2 第 3 步）。
func (s *Service) startProvision(ctx context.Context, tx pgx.Tx, inst Instance, lease leaseRow, now time.Time) error {
	if _, err := s.d.Fleet.MarkAllocated(ctx, tx, inst.MachineID); err != nil {
		return err
	}
	ref, err := s.d.Catalog.ImageRef(ctx, tx, inst.ImageID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'creating', provisioned_at = $2, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'active', updated_at = $2 WHERE id = $1`, lease.ID, now); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, inst.ID, StateReserved, StateCreating, "scheduled start "+tz.Format(*inst.ScheduledStartAt), now); err != nil {
		return err
	}
	_, err = s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: ref})
	return err
}
