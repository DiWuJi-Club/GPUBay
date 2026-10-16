package rental

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type InstanceFilter struct {
	State     string
	UserID    int64
	MachineID int64
}

var instanceStates = []string{StateCreating, StateRunning, StateStopped, StateWiping, StateReleased, StateFailed}

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
