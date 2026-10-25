package rental

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

const (
	MinExtendSeconds = 60
	MaxExtendSeconds = 30 * 24 * 3600
)

// AdminExtend 后台免费补偿时长：不产生订单，按实例状态加时，写实例事件与审计。
func (s *Service) AdminExtend(ctx context.Context, actor audit.Actor, instanceID string, seconds int64, reason string) (Instance, error) {
	reason = strings.TrimSpace(reason)
	if seconds < MinExtendSeconds || seconds > MaxExtendSeconds {
		return Instance{}, apperr.BadRequest("补偿时长需在 1 分钟到 30 天之间")
	}
	if reason == "" {
		return Instance{}, apperr.BadRequest("请填写补偿原因")
	}
	now := s.d.Clock.Now()
	var out Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if inst.System { // 系统实例不计时（expires_at NULL），补偿时长无意义
			return apperr.ErrInvalidState
		}
		if err := s.renewFits(ctx, tx, inst, seconds, now); err != nil {
			return err
		}
		if err := s.addTime(ctx, tx, inst, seconds, now); err != nil {
			return err
		}
		if err := clearAutoRenewFailure(ctx, tx, inst, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, inst.ID, inst.State, inst.State, fmt.Sprintf("admin extend +%ds: %s", seconds, reason), now); err != nil {
			return err
		}
		if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
			return err
		}
		if err := s.verifyLeaseFits(ctx, tx, inst.ID, now); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, now, actor.Entry("instance.extend", "instance", inst.ID,
			map[string]any{"seconds": seconds, "reason": reason, "state": inst.State})); err != nil {
			return err
		}
		out, err = lockInstance(ctx, tx, inst.ID)
		return err
	})
	return out, err
}

// AdminRewipe 对洗机中（通常是洗机失败）的实例取消未完成的指令并重新下发洗机。机器为 faulty 时也会下发，
// 由运维确认机器已修复后操作；洗机成功后机器保持 faulty，需运维结束维护才回到库存。
func (s *Service) AdminRewipe(ctx context.Context, actor audit.Actor, instanceID string) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if inst.State != StateWiping {
			return apperr.ErrInvalidState
		}
		if err := s.d.Fleet.CancelCommands(ctx, tx, inst.ID, "superseded by admin rewipe"); err != nil {
			return err
		}
		if _, err := s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdWipeInstance, nil); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, inst.ID, StateWiping, StateWiping, "admin rewipe", now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("instance.rewipe", "instance", inst.ID,
			map[string]any{"machine_id": inst.MachineID}))
	})
}

// SetMachineMaintenance 让机器进入或结束维护；结束维护（回到库存）前要求机器上没有未结束的实例。
func (s *Service) SetMachineMaintenance(ctx context.Context, actor audit.Actor, machineID int64, on bool) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if !on {
			var busy bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instances
				WHERE machine_id = $1 AND state IN ('creating', 'running', 'stopped', 'wiping'))`, machineID).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return apperr.New("invalid_state", "该机器上还有未结束的实例，不能结束维护", 409)
			}
		}
		if err := s.d.Fleet.SetMaintenanceIn(ctx, tx, machineID, on); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("machine.maintenance", "machine", strconv.FormatInt(machineID, 10),
			map[string]any{"on": on}))
	})
}
