package rental

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type InstanceFilter struct {
	State     string
	UserID    int64
	MachineID int64
}

var instanceStates = []string{StateReserved, StateCreating, StateRunning, StateStopped, StateWiping, StateReleased, StateFailed, StateCancelled}

// AdminListInstances 按创建时间倒序分页返回实例。
func (s *Service) AdminListInstances(ctx context.Context, f InstanceFilter, p paging.Page) ([]Instance, int, error) {
	var w db.Where
	if f.State != "" {
		if !slices.Contains(instanceStates, f.State) {
			return nil, 0, apperr.BadRequest("实例状态不正确")
		}
		w.Add("state = ?", f.State)
	}
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.MachineID > 0 {
		w.Add("machine_id = ?", f.MachineID)
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM instances WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	list, err := collectInstances(s.d.Pool.Query(ctx, `SELECT `+instanceCols+` FROM instances WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...))
	return list, total, err
}

type Event struct {
	ID        int64     `json:"id"`
	FromState string    `json:"from_state"`
	ToState   string    `json:"to_state"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

// InstanceEvents 返回实例的状态变化记录（旧到新）。
func (s *Service) InstanceEvents(ctx context.Context, id string) ([]Event, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT id, from_state, to_state, reason, at FROM instance_events
		WHERE instance_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.FromState, &e.ToState, &e.Reason, &e.At)
		return e, err
	})
}

// InstanceOrders 返回实例及其换机前身的全部订单（新到旧）。
func (s *Service) InstanceOrders(ctx context.Context, id string) ([]Order, error) {
	chain, err := instanceChain(ctx, s.d.Pool, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE instance_id = ANY($1) ORDER BY created_at DESC, id`, chain)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
}

type OrderFilter struct {
	UserID int64
	Kind   string // new / renew
	Status string // paid / refunded / partially_refunded
	From   *time.Time
	To     *time.Time // 不含
	// BatchID 一次多台的批次号（精确匹配）
	BatchID string
}

func (f OrderFilter) where() (db.Where, error) {
	var w db.Where
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.Kind != "" {
		if f.Kind != "new" && f.Kind != "renew" {
			return w, apperr.BadRequest("订单类型不正确")
		}
		w.Add("kind = ?", f.Kind)
	}
	if f.Status != "" {
		if !slices.Contains([]string{"paid", "refunded", "partially_refunded"}, f.Status) {
			return w, apperr.BadRequest("订单状态不正确")
		}
		w.Add("status = ?", f.Status)
	}
	if f.From != nil {
		w.Add("created_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("created_at < ?", *f.To)
	}
	if f.BatchID != "" {
		w.Add("batch_id = ?", f.BatchID)
	}
	return w, nil
}

// AdminListOrders 按下单时间倒序分页返回订单。
func (s *Service) AdminListOrders(ctx context.Context, f OrderFilter, p paging.Page) ([]Order, int, error) {
	w, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.d.Pool.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
	return list, total, err
}

// ExportOrders 按下单时间正序逐行回调；超过导出上限时在回调前返回 csvx.ErrTooLarge。
func (s *Service) ExportOrders(ctx context.Context, f OrderFilter, fn func(Order) error) error {
	w, err := f.where()
	if err != nil {
		return err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return err
	}
	if total > csvx.MaxRows {
		return csvx.ErrTooLarge
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE `+w.SQL()+` ORDER BY created_at, id`, w.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return err
		}
		if err := fn(o); err != nil {
			return err
		}
	}
	return rows.Err()
}

// InstanceStateCounts 按状态统计实例数量。
func (s *Service) InstanceStateCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT state, count(*) FROM instances GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

// OrderAmount 返回 [from, to) 内下单金额之和（分，未扣除退款）。
func (s *Service) OrderAmount(ctx context.Context, from, to time.Time) (int64, error) {
	var sum int64
	err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0) FROM orders WHERE created_at >= $1 AND created_at < $2`, from, to).Scan(&sum)
	return sum, err
}

// DailyOccupancy 返回从 from 起连续 days 天、每天被租用的机器总秒数（machine_leases 中 active / ended 时段与当天的交集，截止到 now）。
func (s *Service) DailyOccupancy(ctx context.Context, from time.Time, days int, now time.Time) ([]float64, error) {
	rows, err := s.d.Pool.Query(ctx, `
		SELECT COALESCE(sum(GREATEST(EXTRACT(EPOCH FROM
				LEAST(upper(l.period), d.day + interval '24 hours', $3::timestamptz) - GREATEST(lower(l.period), d.day)), 0)), 0)::float8
		FROM generate_series($1::timestamptz, $1::timestamptz + ($2::int - 1) * interval '24 hours', interval '24 hours') AS d(day)
		LEFT JOIN machine_leases l ON l.status IN ('active', 'ended') AND l.period && tstzrange(d.day, d.day + interval '24 hours')
		GROUP BY d.day ORDER BY d.day`, from, days, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[float64])
}
