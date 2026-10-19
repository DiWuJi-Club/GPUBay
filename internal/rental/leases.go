package rental

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// WipeBuffer 是预留给洗机的时长，计入机器占用时段。
const WipeBuffer = time.Hour

// occupiedUntil 估算实例对机器的占用结束时间（machine_leases 的上界）。
// 子项目 2 的档期预订会据此判断机器何时空闲。任何分支都至少 now + 1 秒（时段不能为空）。
func occupiedUntil(inst Instance, now time.Time) time.Time {
	pending := time.Duration(inst.PendingSeconds) * time.Second
	var end time.Time
	switch inst.State {
	case StateCreating:
		end = now.Add(CreateTimeout + pending + Tail)
	case StateRunning:
		if inst.Suspended() && inst.PausedRemaining != nil {
			end = now.Add(time.Duration(*inst.PausedRemaining)*time.Second + Tail)
		} else if inst.ExpiresAt != nil {
			end = inst.ExpiresAt.Add(Tail)
		}
	case StateStopped:
		if pending > 0 {
			end = now.Add(CreateTimeout + pending + Tail)
		} else if inst.StoppedAt != nil {
			end = inst.StoppedAt.Add(Tail)
		}
	case StateWiping:
		end = now.Add(WipeBuffer)
	}
	if !end.After(now) {
		end = now.Add(time.Second)
	}
	return end
}

// openLease 为新实例写入一条 active 占用时段；与已有时段重叠视为无可用机器。
func openLease(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'active', $3, $3)`, inst.MachineID, inst.ID, now, occupiedUntil(inst, now))
	if db.IsExclusionViolation(err) {
		return apperr.ErrNoCapacity
	}
	return err
}

// refreshLease 在实例状态变化后同步占用时段：结束的实例关闭时段，其余按 occupiedUntil 更新上界。
// 上界至少为下界 + 1 秒：时段不能为空（machine_leases_period_nonempty）。
func (s *Service) refreshLease(ctx context.Context, tx pgx.Tx, id string, now time.Time) error {
	inst, err := lockInstance(ctx, tx, id)
	if err != nil {
		return err
	}
	end, status := occupiedUntil(inst, now), "active"
	if inst.State == StateReleased || inst.State == StateFailed {
		end, status = now, "ended"
	}
	_, err = tx.Exec(ctx, `UPDATE machine_leases
		SET period = tstzrange(lower(period), GREATEST(lower(period) + interval '1 second', $2::timestamptz)), status = $3, updated_at = $4
		WHERE instance_id = $1 AND status = 'active'`, id, end, status, now)
	if db.IsExclusionViolation(err) {
		return apperr.ErrNoCapacity
	}
	return err
}
