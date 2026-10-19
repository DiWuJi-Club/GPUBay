package rental

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/fleet"
)

// reassign 把预订换到同机组（instances.group_id；nil 不限）另一台机器（规格 §5.4）：
// 新 lease 以 reserved 插入（同一 period）、旧 lease cancelled、实例换机器并 reassign_count+1、写事件。
// needOnline：开通时刻的改派要求 idle AND agent_online；后台提前改派只要求时段不冲突。
// 成功时就地更新 *inst 与 *lease；没有候选返回 false（不改任何数据）。
func (s *Service) reassign(ctx context.Context, tx pgx.Tx, inst *Instance, lease *leaseRow, reason string, needOnline bool, now time.Time) (bool, error) {
	spec := pickSpec{ProductID: inst.ProductID, GroupID: inst.GroupID, Lower: lease.Lower, Upper: lease.Upper,
		Immediate: needOnline, Exclude: []int64{inst.MachineID}}
	var newLease, newMachine int64
	// placeOnMachines 遇到被短暂锁住的候选会退避重挑（合计 ≤ 约 240ms），期间一直持有调用方锁住的实例行与 lease 行；有界，可接受。
	_, err := placeOnMachines(ctx, tx, spec, 1, func(sp pgx.Tx, mid int64) error {
		id, err := insertLease(ctx, sp, mid, inst.ID, lease.Lower, lease.Upper, "reserved", now)
		if err != nil {
			return err
		}
		newLease, newMachine = id, mid
		return nil
	})
	if codeIs(err, "no_capacity") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'cancelled', updated_at = $2 WHERE id = $1`, lease.ID, now); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET machine_id = $2, reassign_count = reassign_count + 1, updated_at = $3 WHERE id = $1`,
		inst.ID, newMachine, now); err != nil {
		return false, err
	}
	if err := recordEvent(ctx, tx, inst.ID, inst.State, inst.State, fmt.Sprintf("reassigned %d -> %d: %s", inst.MachineID, newMachine, reason), now); err != nil {
		return false, err
	}
	inst.MachineID, inst.ReassignCount = newMachine, inst.ReassignCount+1
	lease.ID, lease.MachineID, lease.Status = newLease, newMachine, "reserved"
	return true, nil
}

// reassignAfterCreateFailure 预订实例 Create 失败 / 超时（规格 §5.4 末段），代替 failCreating：
// 机器 faulty + create_failed 告警 + 取消指令（与 failCreating 相同），但不改 pending_seconds、不清主机密钥、不退款、lease 不置 ended。
// 有候选 → 换机器继续 creating（主机密钥重生成、provisioned_at = now）；
// 无候选且未过宽限期 → 回 reserved（provisioned_at 清空，lease 回 reserved，钉在故障机器上等下一轮 ProvisionDue）；
// 过了宽限期 → 平台取消（失败包成 cancelFailure；短信经 *after 在提交后发）。
// 加锁顺序：instance（调用方）→ [agent_commands] → 自己的 lease → 旧机器（MarkFaulty）→ 候选 machines SKIP LOCKED → agent_commands。
func (s *Service) reassignAfterCreateFailure(ctx context.Context, tx pgx.Tx, inst Instance, reason string, now time.Time, after *func()) error {
	old := inst.MachineID
	lease, err := lockLease(ctx, tx, inst.ID)
	if err != nil {
		return err
	}
	if err := s.d.Fleet.MarkFaulty(ctx, tx, old); err != nil {
		return err
	}
	if err := s.d.Fleet.CancelCommands(ctx, tx, inst.ID, "instance create failed"); err != nil {
		return err
	}
	if err := s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "create_failed", MachineID: old, InstanceID: inst.ID, Message: reason}); err != nil {
		return err
	}
	moved, err := s.reassign(ctx, tx, &inst, &lease, "create failed", true, now)
	if err != nil {
		return err
	}
	if moved {
		hostPub, hostPEM, err := s.d.HostKeyGen()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE instances SET ssh_host_pubkey = $2, ssh_host_key_enc = $3, provisioned_at = $4, updated_at = $4 WHERE id = $1`,
			inst.ID, hostPub, s.d.Box.Seal(hostPEM), now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'active', updated_at = $2 WHERE id = $1`, lease.ID, now); err != nil {
			return err
		}
		if _, err := s.d.Fleet.MarkAllocated(ctx, tx, inst.MachineID); err != nil {
			return err
		}
		ref, err := s.d.Catalog.ImageRef(ctx, tx, inst.ImageID)
		if err != nil {
			return err
		}
		_, err = s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: ref})
		return err
	}
	if now.Before(inst.ScheduledStartAt.Add(BookingGrace)) {
		if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'reserved', provisioned_at = NULL, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'reserved', updated_at = $2 WHERE id = $1`, lease.ID, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, inst.ID, StateCreating, StateReserved, "create failed, waiting for machine", now); err != nil {
			return err
		}
		return s.d.Fleet.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "warning", Kind: "booking_at_risk", MachineID: old, InstanceID: inst.ID, Message: AlertBookingAtRisk(inst.ID)})
	}
	sms, err := s.cancelUnfulfilled(ctx, tx, inst, old, "unfulfilled: create failed past grace", now)
	if err != nil {
		return err
	}
	*after = sms
	return nil
}
