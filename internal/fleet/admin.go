package fleet

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type MachineFilter struct {
	State     string
	Online    *bool
	Rack      string
	GroupID   int64 // > 0：只看该机组
	Ungrouped bool  // 只看未分组机器（与 GroupID 互斥）
	FabricID  int64 // > 0：只看该集群网络（计划 13）
}

var machineStates = []string{StateIdle, StateAllocated, StateMaintenance, StateFaulty}

// ListMachines 按机柜、槽位排序分页返回机器。
func (s *Service) ListMachines(ctx context.Context, f MachineFilter, p paging.Page) ([]Machine, int, error) {
	var w db.Where
	if f.State != "" {
		if !slices.Contains(machineStates, f.State) {
			return nil, 0, apperr.BadRequest("机器状态不正确")
		}
		w.Add("state = ?", f.State)
	}
	if f.Online != nil {
		w.Add("agent_online = ?", *f.Online)
	}
	if f.Rack != "" {
		w.Add("rack = ?", f.Rack)
	}
	if f.GroupID > 0 {
		w.Add("group_id = ?", f.GroupID)
	}
	if f.Ungrouped {
		w.Add("group_id IS NULL")
	}
	if f.FabricID > 0 {
		w.Add("fabric_id = ?", f.FabricID)
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM machines WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.pool.Query(ctx, `SELECT `+machineCols+` FROM machines WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY rack, slot, id LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Machine, error) { return scanMachine(r) })
	return list, total, err
}

// RegisterAudited 后台单台登记：登记并写审计。
func (s *Service) RegisterAudited(ctx context.Context, actor audit.Actor, nm NewMachine) (Machine, error) {
	var m Machine
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if m, err = s.register(ctx, tx, nm); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.clock.Now(), actor.Entry("machine.register", "machine", strconv.FormatInt(m.ID, 10),
			map[string]any{"serial": m.Serial, "rack": m.Rack, "slot": m.Slot, "lan_ip": m.LanIP}))
	})
	return m, err
}

const MaxImportRows = 1000

type ImportRow struct {
	Line    int
	Machine NewMachine
}

type RowError struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// ImportError 表示批量导入中有行出错（整批未导入）。
type ImportError struct {
	Rows []RowError
}

func (e *ImportError) Error() string { return fmt.Sprintf("导入数据有 %d 行错误", len(e.Rows)) }

// Import 整批登记机器：每行在保存点内插入以收集全部行错误；任一行出错（含调用方预先发现的 preErrors）则整批回滚。
func (s *Service) Import(ctx context.Context, actor audit.Actor, rows []ImportRow, preErrors []RowError) ([]Machine, error) {
	if len(rows)+len(preErrors) == 0 {
		return nil, apperr.BadRequest("没有可导入的机器")
	}
	if len(rows)+len(preErrors) > MaxImportRows {
		return nil, apperr.BadRequest(fmt.Sprintf("单次最多导入 %d 台", MaxImportRows))
	}
	var out []Machine
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		rowErrs := append([]RowError(nil), preErrors...)
		for _, row := range rows {
			sp, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			m, err := s.register(ctx, sp, row.Machine)
			if err != nil {
				_ = sp.Rollback(ctx)
				ae := apperr.As(err)
				if ae == nil {
					return err
				}
				rowErrs = append(rowErrs, RowError{Line: row.Line, Message: ae.Message})
				continue
			}
			if err := sp.Commit(ctx); err != nil {
				return err
			}
			out = append(out, m)
		}
		if len(rowErrs) > 0 {
			sort.SliceStable(rowErrs, func(i, j int) bool { return rowErrs[i].Line < rowErrs[j].Line })
			return &ImportError{Rows: rowErrs}
		}
		serials := make([]string, len(out))
		for i, m := range out {
			serials[i] = m.Serial
		}
		return audit.Record(ctx, tx, s.clock.Now(), actor.Entry("machine.import", "machine", "",
			map[string]any{"count": len(out), "serials": serials}))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type MachineStats struct {
	ByState map[string]int `json:"by_state"`
	Online  int            `json:"online"`
	Total   int            `json:"total"`
}

func (s *Service) Stats(ctx context.Context) (MachineStats, error) {
	st := MachineStats{ByState: map[string]int{}}
	rows, err := s.pool.Query(ctx, `SELECT state, count(*), count(*) FILTER (WHERE agent_online) FROM machines GROUP BY state`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n, online int
		if err := rows.Scan(&state, &n, &online); err != nil {
			return st, err
		}
		st.ByState[state] = n
		st.Total += n
		st.Online += online
	}
	return st, rows.Err()
}

type AlertRow struct {
	ID         int64      `json:"id"`
	Level      string     `json:"level"`
	Kind       string     `json:"kind"`
	MachineID  *int64     `json:"machine_id"`
	InstanceID string     `json:"instance_id"`
	Message    string     `json:"message"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
	ResolvedBy *int64     `json:"resolved_by"`
}

// ListAlerts 按时间倒序分页返回告警；resolved 为 nil 表示不按处理状态筛选。
func (s *Service) ListAlerts(ctx context.Context, resolved *bool, p paging.Page) ([]AlertRow, int, error) {
	cond := "TRUE"
	if resolved != nil && *resolved {
		cond = "resolved_at IS NOT NULL"
	} else if resolved != nil {
		cond = "resolved_at IS NULL"
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE `+cond).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, level, kind, machine_id, instance_id, message, created_at, resolved_at, resolved_by
		FROM alerts WHERE `+cond+` ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`, p.Limit(), p.Offset())
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlertRow, error) {
		var a AlertRow
		err := r.Scan(&a.ID, &a.Level, &a.Kind, &a.MachineID, &a.InstanceID, &a.Message, &a.CreatedAt, &a.ResolvedAt, &a.ResolvedBy)
		return a, err
	})
	return list, total, err
}

// ResolveAlert 把告警标记为已处理；已处理过返回 invalid_state。
func (s *Service) ResolveAlert(ctx context.Context, actor audit.Actor, id int64) error {
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var resolvedAt *time.Time
		err := tx.QueryRow(ctx, `SELECT resolved_at FROM alerts WHERE id = $1 FOR UPDATE`, id).Scan(&resolvedAt)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if resolvedAt != nil {
			return apperr.ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `UPDATE alerts SET resolved_at = $2, resolved_by = $3 WHERE id = $1`, id, now, actor.AdminID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("alert.resolve", "alert", strconv.FormatInt(id, 10), nil))
	})
}
