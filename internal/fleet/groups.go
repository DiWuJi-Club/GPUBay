package fleet

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// Group 机组：机器的可选标签（规格 §3）。
type Group struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Region      string    `json:"region"`
	Description string    `json:"description"`
	Sort        int       `json:"sort"`
	Visible     bool      `json:"visible"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type GroupInput struct {
	Name        string
	Region      string
	Description string
	Sort        int
	Visible     bool
}

// GroupRow 列表行：机组加机器数与现空闲台数。
type GroupRow struct {
	Group
	MachineCount int `json:"machine_count"`
	IdleNow      int `json:"idle_now"`
}

const groupCols = `id, name, region, description, sort, visible, created_at, updated_at`

func scanGroup(row pgx.Row) (Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.Name, &g.Region, &g.Description, &g.Sort, &g.Visible, &g.CreatedAt, &g.UpdatedAt)
	if db.IsNoRows(err) {
		return Group{}, apperr.ErrGroupNotFound
	}
	return g, err
}

func (in GroupInput) normalize() (GroupInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Region = strings.TrimSpace(in.Region)
	in.Description = strings.TrimSpace(in.Description)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 32 {
		return in, apperr.BadRequest("机组名称需为 1～32 个字符")
	}
	if utf8.RuneCountInString(in.Region) > 32 {
		return in, apperr.BadRequest("位置标签最多 32 个字符")
	}
	if utf8.RuneCountInString(in.Description) > 200 {
		return in, apperr.BadRequest("描述最多 200 个字符")
	}
	return in, nil
}

// ListGroups 按 sort、id 返回机组（visibleOnly 只返回控制台可见的），附机器数与现空闲台数。
func (s *Service) ListGroups(ctx context.Context, visibleOnly bool) ([]GroupRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+groupCols+`,
			(SELECT count(*) FROM machines m WHERE m.group_id = g.id),
			(SELECT count(*) FROM machines m WHERE m.group_id = g.id AND m.state = 'idle' AND m.agent_online)
		FROM machine_groups g WHERE NOT $1 OR visible ORDER BY sort, id`, visibleOnly)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (GroupRow, error) {
		var g GroupRow
		err := r.Scan(&g.ID, &g.Name, &g.Region, &g.Description, &g.Sort, &g.Visible, &g.CreatedAt, &g.UpdatedAt, &g.MachineCount, &g.IdleNow)
		return g, err
	})
}

func (s *Service) GetGroup(ctx context.Context, q db.Querier, id int64) (Group, error) {
	return scanGroup(q.QueryRow(ctx, `SELECT `+groupCols+` FROM machine_groups WHERE id = $1`, id))
}

func (s *Service) CreateGroup(ctx context.Context, actor audit.Actor, in GroupInput) (Group, error) {
	in, err := in.normalize()
	if err != nil {
		return Group{}, err
	}
	now := s.clock.Now()
	var g Group
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		g, err = scanGroup(tx.QueryRow(ctx, `INSERT INTO machine_groups (name, region, description, sort, visible, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6) RETURNING `+groupCols, in.Name, in.Region, in.Description, in.Sort, in.Visible, now))
		if db.IsUniqueViolation(err) {
			return apperr.ErrDuplicateGroupName
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.create", "group", strconv.FormatInt(g.ID, 10),
			map[string]any{"name": g.Name, "region": g.Region, "visible": g.Visible}))
	})
	return g, err
}

func (s *Service) UpdateGroup(ctx context.Context, actor audit.Actor, id int64, in GroupInput) (Group, error) {
	in, err := in.normalize()
	if err != nil {
		return Group{}, err
	}
	now := s.clock.Now()
	var g Group
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := scanGroup(tx.QueryRow(ctx, `SELECT `+groupCols+` FROM machine_groups WHERE id = $1 FOR NO KEY UPDATE`, id))
		if err != nil {
			return err
		}
		g, err = scanGroup(tx.QueryRow(ctx, `UPDATE machine_groups SET name = $2, region = $3, description = $4, sort = $5, visible = $6, updated_at = $7
			WHERE id = $1 RETURNING `+groupCols, id, in.Name, in.Region, in.Description, in.Sort, in.Visible, now))
		if db.IsUniqueViolation(err) {
			return apperr.ErrDuplicateGroupName
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.update", "group", strconv.FormatInt(id, 10),
			map[string]any{"before": before, "after": g}))
	})
	return g, err
}

// DeleteGroupIn 删除没有机器的机组（实例引用由 rental.AdminDeleteGroup 在同一事务里先检查）。
func (s *Service) DeleteGroupIn(ctx context.Context, q db.Querier, id int64) error {
	var machines int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM machines WHERE group_id = $1`, id).Scan(&machines); err != nil {
		return err
	}
	if machines > 0 {
		return apperr.ErrGroupNotEmpty
	}
	tag, err := q.Exec(ctx, `DELETE FROM machine_groups WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.ErrGroupNotFound
	}
	return nil
}

// AssignMachines 把机器加入机组（已在其它机组的直接改组）；任一机器不存在整批失败。
func (s *Service) AssignMachines(ctx context.Context, actor audit.Actor, groupID int64, ids []int64) error {
	if len(ids) == 0 {
		return apperr.BadRequest("请选择机器")
	}
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.GetGroup(ctx, tx, groupID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE machines SET group_id = $1, updated_at = $3 WHERE id = ANY($2)`, groupID, ids, now)
		if err != nil {
			return err
		}
		if int(tag.RowsAffected()) != len(ids) {
			return apperr.BadRequest("有机器不存在")
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.assign", "group", strconv.FormatInt(groupID, 10), map[string]any{"machine_ids": ids}))
	})
}

// UnassignMachines 把机器移出机组（只影响当前确实在该组的机器）。
func (s *Service) UnassignMachines(ctx context.Context, actor audit.Actor, groupID int64, ids []int64) error {
	if len(ids) == 0 {
		return apperr.BadRequest("请选择机器")
	}
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE machines SET group_id = NULL, updated_at = $3 WHERE id = ANY($2) AND group_id = $1`, groupID, ids, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.unassign", "group", strconv.FormatInt(groupID, 10), map[string]any{"machine_ids": ids}))
	})
}

// MachineUpdate 后台可改的机器字段：SetGroup=true 时按 GroupID 改组（nil = 移出机组）；DisplayName 非 nil 时改展示名。
type MachineUpdate struct {
	SetGroup    bool
	GroupID     *int64
	DisplayName *string
}

// UpdateMachine 修改机组 / 展示名（计划 4 遗留的「修改已登记机器」只开放这两项）。
func (s *Service) UpdateMachine(ctx context.Context, actor audit.Actor, id int64, in MachineUpdate) (Machine, error) {
	var name string
	if in.DisplayName != nil {
		name = strings.TrimSpace(*in.DisplayName)
		if utf8.RuneCountInString(name) > 32 {
			return Machine{}, apperr.BadRequest("展示名最多 32 个字符")
		}
	}
	now := s.clock.Now()
	var out Machine
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := scanMachine(tx.QueryRow(ctx, `SELECT `+machineCols+` FROM machines WHERE id = $1 FOR NO KEY UPDATE`, id))
		if err != nil {
			return err
		}
		groupID, display := before.GroupID, before.DisplayName
		if in.SetGroup {
			groupID = in.GroupID
			if groupID != nil {
				if _, err := s.GetGroup(ctx, tx, *groupID); err != nil {
					return err
				}
			}
		}
		if in.DisplayName != nil {
			display = name
		}
		out, err = scanMachine(tx.QueryRow(ctx, `UPDATE machines SET group_id = $2, display_name = $3, updated_at = $4 WHERE id = $1 RETURNING `+machineCols,
			id, groupID, display, now))
		if db.IsUniqueViolation(err) && db.ConstraintName(err) == "machines_display_name_uniq" {
			return apperr.ErrDuplicateDisplayName
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("machine.update", "machine", strconv.FormatInt(id, 10), map[string]any{
			"before": map[string]any{"group_id": before.GroupID, "display_name": before.DisplayName},
			"after":  map[string]any{"group_id": out.GroupID, "display_name": out.DisplayName},
		}))
	})
	return out, err
}
