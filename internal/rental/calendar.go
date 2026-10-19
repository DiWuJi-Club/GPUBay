package rental

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/tz"
)

const maxCalendarDays = 61

type AvailabilityQuery struct {
	ProductID int64
	GroupID   *int64
	MachineID *int64
	StartAt   *time.Time // nil = 立即
	Duration  catalog.Duration
	Quantity  int
}

type Availability struct {
	Available int       `json:"available"`
	Lower     time.Time `json:"lower"`
	Upper     time.Time `json:"upper"`
}

// VisibleGroup 控制台可用的机组：不存在或 visible=false 都按不存在处理。
func (s *Service) VisibleGroup(ctx context.Context, id int64) (fleet.Group, error) {
	g, err := s.d.Fleet.GetGroup(ctx, s.d.Pool, id)
	if err != nil {
		return fleet.Group{}, err
	}
	if !g.Visible {
		return fleet.Group{}, apperr.ErrGroupNotFound
	}
	return g, nil
}

// Availability 所选时段可租台数（只读预筛，下单以事务内结果为准）。
func (s *Service) Availability(ctx context.Context, q AvailabilityQuery) (Availability, error) {
	if !q.Duration.Valid() {
		return Availability{}, apperr.BadRequest("租期不正确")
	}
	if q.Quantity == 0 {
		q.Quantity = 1
	}
	if q.Quantity < 1 || q.Quantity > MaxQuantity(q.Duration) {
		return Availability{}, apperr.ErrBookingTooLong
	}
	if q.GroupID != nil {
		if _, err := s.VisibleGroup(ctx, *q.GroupID); err != nil {
			return Availability{}, err
		}
	}
	secs := seconds(q.Duration) * int64(q.Quantity)
	spec := pickSpec{ProductID: q.ProductID, GroupID: q.GroupID, MachineID: q.MachineID, Immediate: q.StartAt == nil}
	if spec.Immediate {
		spec.Lower, spec.Upper = immediatePeriod(s.d.Clock.Now(), secs)
	} else {
		spec.Lower, spec.Upper = bookingPeriod(q.StartAt.UTC(), secs)
	}
	n, err := countAvailable(ctx, s.d.Pool, spec)
	if err != nil {
		return Availability{}, err
	}
	return Availability{Available: n, Lower: spec.Lower, Upper: spec.Upper}, nil
}

type CalendarDay struct {
	Date  string `json:"date"`
	Free  int    `json:"free"`
	Total int    `json:"total"`
}

func calendarRange(from time.Time, days int) (time.Time, error) {
	if days < 1 || days > maxCalendarDays {
		return time.Time{}, apperr.BadRequest(fmt.Sprintf("days 需在 1～%d 之间", maxCalendarDays))
	}
	return tz.DayStart(from), nil
}

// GroupCalendar 机组日历：每一天（北京时间自然日）组内与任何 reserved / active 时段都不相交的机器数；total 只算 idle / allocated 机器。
func (s *Service) GroupCalendar(ctx context.Context, groupID int64, from time.Time, days int, visibleOnly bool) ([]CalendarDay, error) {
	if visibleOnly {
		if _, err := s.VisibleGroup(ctx, groupID); err != nil {
			return nil, err
		}
	} else if _, err := s.d.Fleet.GetGroup(ctx, s.d.Pool, groupID); err != nil {
		return nil, err
	}
	start, err := calendarRange(from, days)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `
		WITH d AS (SELECT generate_series($2::timestamptz, $2::timestamptz + ($3::int - 1) * interval '24 hours', interval '24 hours') AS day),
		     m AS (SELECT id FROM machines WHERE group_id = $1 AND state IN ('idle', 'allocated'))
		SELECT d.day,
		       count(m.id) FILTER (WHERE NOT EXISTS (SELECT 1 FROM machine_leases l WHERE l.machine_id = m.id
		                                           AND l.status IN ('reserved', 'active') AND l.period && tstzrange(d.day, d.day + interval '24 hours'))),
		       count(m.id)
		FROM d LEFT JOIN m ON true GROUP BY d.day ORDER BY d.day`, groupID, start, days)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CalendarDay, error) {
		var day time.Time
		var c CalendarDay
		err := r.Scan(&day, &c.Free, &c.Total)
		c.Date = day.In(tz.CST).Format("2006-01-02")
		return c, err
	})
}

type CalendarPeriod struct {
	Lower      time.Time `json:"lower"`
	Upper      time.Time `json:"upper"`
	Mine       bool      `json:"mine"`
	InstanceID string    `json:"instance_id,omitempty"`
	UserID     int64     `json:"user_id,omitempty"`
}

// MachineCalendar 机器在 [from 当天 00:00, + days) 内的 reserved / active 时段；非 admin 只标 mine，不透出别人的实例与用户。
func (s *Service) MachineCalendar(ctx context.Context, machineID, userID int64, from time.Time, days int, admin bool) ([]CalendarPeriod, error) {
	start, err := calendarRange(from, days)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT lower(l.period), upper(l.period), i.id, i.user_id
		FROM machine_leases l JOIN instances i ON i.id = l.instance_id
		WHERE l.machine_id = $1 AND l.status IN ('reserved', 'active') AND l.period && tstzrange($2::timestamptz, $2::timestamptz + $3::int * interval '24 hours')
		ORDER BY lower(l.period)`, machineID, start, days)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CalendarPeriod, error) {
		var p CalendarPeriod
		var inst string
		var uid int64
		if err := r.Scan(&p.Lower, &p.Upper, &inst, &uid); err != nil {
			return p, err
		}
		p.Mine = uid == userID && userID != 0
		if admin || p.Mine {
			p.InstanceID, p.UserID = inst, uid
		}
		return p, nil
	})
}

type MachineBrief struct {
	ID        int64      `json:"id"`
	Label     string     `json:"label"`
	BusyUntil *time.Time `json:"busy_until"` // 现有 / 未来时段的最晚结束；nil = 当前没有占用
}

// GroupMachines 机组内可租机器（idle / allocated）与展示名，供控制台指定机器。
func (s *Service) GroupMachines(ctx context.Context, groupID int64, visibleOnly bool) ([]MachineBrief, error) {
	if visibleOnly {
		if _, err := s.VisibleGroup(ctx, groupID); err != nil {
			return nil, err
		}
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT m.id, m.display_name,
			(SELECT max(upper(l.period)) FROM machine_leases l WHERE l.machine_id = m.id AND l.status IN ('reserved', 'active') AND upper(l.period) > $2)
		FROM machines m WHERE m.group_id = $1 AND m.state IN ('idle', 'allocated') ORDER BY m.display_name, m.id`, groupID, s.d.Clock.Now())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MachineBrief, error) {
		var b MachineBrief
		var name string
		if err := r.Scan(&b.ID, &name, &b.BusyUntil); err != nil {
			return b, err
		}
		b.Label = fleet.Machine{ID: b.ID, DisplayName: name}.Label()
		return b, nil
	})
}
