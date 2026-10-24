package rental

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type BookingFilter struct {
	State     string // reserved（缺省）/ cancelled
	UserID    int64
	GroupID   int64
	MachineID int64
	From      *time.Time // scheduled_start_at >=
	To        *time.Time // scheduled_start_at <（不含）
}

// AdminListBookings 后台预订列表：只含预订实例（scheduled_start_at 非空），按开始时间排序。
func (s *Service) AdminListBookings(ctx context.Context, f BookingFilter, p paging.Page) ([]Instance, int, error) {
	if f.State == "" {
		f.State = StateReserved
	}
	if !slices.Contains([]string{StateReserved, StateCancelled}, f.State) {
		return nil, 0, apperr.BadRequest("预订状态只能是 reserved 或 cancelled")
	}
	var w db.Where
	w.Add("scheduled_start_at IS NOT NULL")
	w.Add("state = ?", f.State)
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.GroupID > 0 {
		w.Add("group_id = ?", f.GroupID)
	}
	if f.MachineID > 0 {
		w.Add("machine_id = ?", f.MachineID)
	}
	if f.From != nil {
		w.Add("scheduled_start_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("scheduled_start_at < ?", *f.To)
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM instances WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	list, err := collectInstances(s.d.Pool.Query(ctx, `SELECT `+instanceCols+` FROM instances WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY scheduled_start_at, id LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...))
	return list, total, err
}

// UpcomingBookings 从 from 当天（北京时间）起连续 days 天、每天开始的未开通预订台数。
func (s *Service) UpcomingBookings(ctx context.Context, from time.Time, days int) ([]int, error) {
	start, err := calendarRange(from, days)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `
		SELECT (SELECT count(*) FROM instances i WHERE i.state = 'reserved'
		          AND i.scheduled_start_at >= d.day AND i.scheduled_start_at < d.day + interval '24 hours')::int
		FROM generate_series($1::timestamptz, $1::timestamptz + ($2::int - 1) * interval '24 hours', interval '24 hours') AS d(day) ORDER BY d.day`, start, days)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// MachineStates 批量读取机器状态（预订列表里标出钉在 faulty / maintenance 机器上的预订，便于宽限期前手动改派）。
func (s *Service) MachineStates(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT id, state FROM machines WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st string
		if err := rows.Scan(&id, &st); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// UpcomingBookingsForMachine 机器上未开通预订的台数（进入维护时提示）。
func (s *Service) UpcomingBookingsForMachine(ctx context.Context, machineID int64) (int, error) {
	var n int
	err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM instances WHERE machine_id = $1 AND state = 'reserved'`, machineID).Scan(&n)
	return n, err
}

// AdminReassign 后台提前改派：machineID 为 nil 时同机组自动挑（不要求在线）；指定时目标必须空闲 / 已分配且时段无冲突（任意机组）。
// 加锁顺序：instance → lease → machines（自动挑选 SKIP LOCKED；指定机器 NO KEY UPDATE 等待）；整个事务遇 40P01 / 40001 重试一次。
func (s *Service) AdminReassign(ctx context.Context, actor audit.Actor, id string, machineID *int64, reason string) (Instance, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Instance{}, apperr.BadRequest("请填写改派原因")
	}
	now := s.d.Clock.Now()
	var out Instance
	err := withRetry(func() error {
		return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			inst, err := lockInstance(ctx, tx, id)
			if err != nil {
				return err
			}
			if inst.System || inst.State != StateReserved || !inst.Booked() {
				return apperr.ErrInvalidState
			}
			lease, err := lockLease(ctx, tx, inst.ID)
			if err != nil {
				return err
			}
			from, fromGroup := inst.MachineID, inst.GroupID
			if machineID == nil {
				moved, err := s.reassign(ctx, tx, &inst, &lease, "admin: "+reason, false, now)
				if err != nil {
					return err
				}
				if !moved {
					return apperr.ErrMachineUnavailable
				}
			} else if err := s.reassignTo(ctx, tx, &inst, &lease, *machineID, "admin: "+reason, now); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, now, actor.Entry("booking.reassign", "instance", inst.ID,
				map[string]any{"from_machine": from, "to_machine": inst.MachineID, "from_group": fromGroup, "to_group": inst.GroupID, "reason": reason})); err != nil {
				return err
			}
			out, err = lockInstance(ctx, tx, inst.ID)
			return err
		})
	})
	return out, err
}

// reassignTo 改派到指定机器：机器存在、同商品、idle / allocated、与 lease 时段无冲突（排他约束兜底）。
// 与自动挑选不对称（有意）：不要求 agent_online（提前改派，开通时再看），也允许 visible=false 机组的机器（运维留池）；后台页面的说明文字同此。
// 例外（计划 9）：Token 服务池（purpose='llm'）的机器不接受改派——池机器只给平台节点用。
// 目标机器行锁 FOR NO KEY UPDATE（等待而非 SKIP LOCKED：运维点名的机器不该因并发分配被悄悄跳过）；
// 实例的 group_id 改为目标机器的机组（与 AdminMigrate 一致），之后的自动改派与展示都以新机组为准。
func (s *Service) reassignTo(ctx context.Context, tx pgx.Tx, inst *Instance, lease *leaseRow, target int64, reason string, now time.Time) error {
	if target == inst.MachineID {
		return apperr.ErrMachineUnavailable
	}
	var groupID *int64
	err := tx.QueryRow(ctx, `SELECT m.group_id FROM machines m WHERE m.id = $1 AND m.product_id = $2 AND m.state IN ('idle', 'allocated')
			AND NOT EXISTS (SELECT 1 FROM machine_groups g WHERE g.id = m.group_id AND g.purpose = 'llm')
			AND NOT EXISTS (SELECT 1 FROM machine_leases l WHERE l.machine_id = m.id AND l.status IN ('reserved', 'active')
			                AND l.period && tstzrange($3::timestamptz, $4::timestamptz))
			FOR NO KEY UPDATE OF m`,
		target, inst.ProductID, lease.Lower, lease.Upper).Scan(&groupID)
	if db.IsNoRows(err) {
		return apperr.ErrMachineUnavailable
	}
	if err != nil {
		return err
	}
	newLease, err := insertLease(ctx, tx, target, inst.ID, lease.Lower, lease.Upper, "reserved", now)
	if db.IsExclusionViolation(err) { // 并发事务刚提交的时段：整个事务随错误回滚
		return apperr.ErrMachineUnavailable
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'cancelled', updated_at = $2 WHERE id = $1`, lease.ID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET machine_id = $2, group_id = $3, reassign_count = reassign_count + 1, updated_at = $4 WHERE id = $1`,
		inst.ID, target, groupID, now); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, inst.ID, inst.State, inst.State, fmt.Sprintf("reassigned %d -> %d: %s", inst.MachineID, target, reason), now); err != nil {
		return err
	}
	inst.MachineID, inst.GroupID, inst.ReassignCount = target, groupID, inst.ReassignCount+1
	lease.ID, lease.MachineID = newLease, target
	return nil
}

// MachineLeases 机器从 from 起 days 天内的全部 reserved / active 时段（后台视图，含实例与用户）；没有时为空数组。
func (s *Service) MachineLeases(ctx context.Context, machineID int64, from time.Time, days int) ([]CalendarPeriod, error) {
	list, err := s.MachineCalendar(ctx, machineID, 0, from, days, true)
	if list == nil && err == nil {
		list = []CalendarPeriod{}
	}
	return list, err
}
