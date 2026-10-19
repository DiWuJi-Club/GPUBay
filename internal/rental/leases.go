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

// leaseRow 是实例当前的占用时段。
type leaseRow struct {
	ID        int64
	MachineID int64
	Lower     time.Time
	Upper     time.Time
	Status    string
}

// lockLease 锁定并返回实例当前（reserved / active）的 lease；调用方须已锁实例。没有 → ErrNotFound。
func lockLease(ctx context.Context, tx pgx.Tx, instanceID string) (leaseRow, error) {
	var l leaseRow
	err := tx.QueryRow(ctx, `SELECT id, machine_id, lower(period), upper(period), status FROM machine_leases
		WHERE instance_id = $1 AND status IN ('reserved', 'active') ORDER BY id DESC LIMIT 1 FOR UPDATE`, instanceID).
		Scan(&l.ID, &l.MachineID, &l.Lower, &l.Upper, &l.Status)
	if db.IsNoRows(err) {
		return leaseRow{}, apperr.ErrNotFound
	}
	return l, err
}

// insertLease 插入一条时段，返回原始错误（排他冲突由调用方用 db.IsExclusionViolation 判断）。
func insertLease(ctx context.Context, q db.Querier, machineID int64, instanceID string, lower, upper time.Time, status string, now time.Time) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), $5, $6, $6) RETURNING id`, machineID, instanceID, lower, upper, status, now).Scan(&id)
	return id, err
}

// nextReservedLower 返回机器上其它实例的 reserved 时段中、下界不早于 after 的最小下界（封顶值 cap）；没有则 ok=false。
func nextReservedLower(ctx context.Context, q db.Querier, machineID int64, exclude string, after time.Time) (time.Time, bool, error) {
	var cap *time.Time
	if err := q.QueryRow(ctx, `SELECT min(lower(period)) FROM machine_leases
		WHERE machine_id = $1 AND status = 'reserved' AND instance_id <> $2 AND lower(period) >= $3`, machineID, exclude, after).Scan(&cap); err != nil {
		return time.Time{}, false, err
	}
	if cap == nil {
		return time.Time{}, false, nil
	}
	return *cap, true, nil
}

// occupiedUntil 估算实例对机器的占用结束时间（machine_leases 的上界），任何分支都至少 now + 1 秒（时段不能为空）。
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
	_, err := insertLease(ctx, tx, inst.MachineID, inst.ID, now, occupiedUntil(inst, now), "active", now)
	if db.IsExclusionViolation(err) {
		return apperr.ErrNoCapacity
	}
	return err
}

// refreshLease 在实例状态变化后同步占用时段：结束的实例关闭时段，其余按 occupiedUntil 更新上界，
// 但上界封顶于同机下一条 reserved 时段的下界（规格 §4.3）。
// 预订实例在 reserved 与首次开通的 creating 期间时段固定（下单时定死），不改写。
// 封顶值按语句快照计算：并发事务正在插入、尚未提交的预订看不到，UPDATE 会等它提交后撞排他约束。
// 所以 UPDATE 放在保存点里，冲突时回滚保存点重试一次（新语句的快照能看到已提交的预订，封顶生效）；仍冲突 → ErrNoCapacity。
func (s *Service) refreshLease(ctx context.Context, tx pgx.Tx, id string, now time.Time) error {
	inst, err := lockInstance(ctx, tx, id)
	if err != nil {
		return err
	}
	if inst.Booked() && (inst.State == StateReserved || inst.State == StateCreating) {
		return nil
	}
	desired, status := occupiedUntil(inst, now), "active"
	if inst.State == StateReleased || inst.State == StateFailed || inst.State == StateCancelled {
		desired, status = now, "ended"
	}
	for attempt := 0; ; attempt++ {
		err = updateLeaseCapped(ctx, tx, id, desired, status, now)
		if !db.IsExclusionViolation(err) {
			return err
		}
		if attempt == 1 {
			return apperr.ErrNoCapacity
		}
	}
}

// updateLeaseCapped 在保存点内把实例的 active 时段改为 [lower, min(desired, cap))（至少 1 秒）；结果与现值相同则不写。
func updateLeaseCapped(ctx context.Context, tx pgx.Tx, id string, desired time.Time, status string, now time.Time) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	// cap 按 lease 自己的 machine_id 算（不用 inst.MachineID）：改派过渡期 lease 与实例的机器可能短暂不一致
	_, err = sp.Exec(ctx, `WITH own AS (
			SELECT id, machine_id, lower(period) AS lo FROM machine_leases
			WHERE instance_id = $1 AND status = 'active' ORDER BY id DESC LIMIT 1 FOR UPDATE
		), cap AS (
			SELECT min(lower(l.period)) AS c FROM machine_leases l, own
			WHERE l.machine_id = own.machine_id AND l.status = 'reserved' AND l.instance_id <> $1 AND lower(l.period) >= own.lo
		), target AS (
			SELECT own.id, tstzrange(own.lo, GREATEST(own.lo + interval '1 second',
				LEAST($2::timestamptz, COALESCE((SELECT c FROM cap), $2::timestamptz)))) AS p
			FROM own
		)
		UPDATE machine_leases m
		SET period = target.p, status = $3, updated_at = $4
		FROM target WHERE m.id = target.id AND (m.period IS DISTINCT FROM target.p OR m.status IS DISTINCT FROM $3)`, id, desired, status, now)
	if err != nil {
		_ = sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}
