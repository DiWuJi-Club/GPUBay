package fleet

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	Purpose     string    `json:"purpose"` // rental（普通机组）/ llm（Token 服务池，唯一、不可见，规格 §3.3）
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// 机组用途（machine_groups.purpose）。
const (
	GroupPurposeRental = "rental"
	GroupPurposeLLM    = "llm"
)

// GroupInput 建 / 改机组的输入。Purpose：建组时空 = rental，只能是 rental（池由迁移建好，不能经接口创建）；
// 改组时空 = 不改，非空且与现值不同 → 拒绝（用途不可修改）。
type GroupInput struct {
	Name        string
	Region      string
	Description string
	Sort        int
	Visible     bool
	Purpose     string
}

var (
	errCreateLLMGroup   = apperr.BadRequest("不能创建 Token 服务池机组")
	errPoolVisible      = apperr.New(apperr.ErrInvalidState.Code, "Token 服务池不能对用户可见", apperr.ErrInvalidState.Status)
	errPurposeImmutable = apperr.New(apperr.ErrInvalidState.Code, "机组用途不能修改", apperr.ErrInvalidState.Status)
	errPoolRename       = apperr.New(apperr.ErrInvalidState.Code, "Token 服务池不能改名", apperr.ErrInvalidState.Status)
	// ErrPoolNotDeletable Token 服务池不能删除（rental.AdminDeleteGroup 锁住机组行后先判断，早于 group_not_empty）。
	ErrPoolNotDeletable = apperr.New(apperr.ErrInvalidState.Code, "Token 服务池不能删除", apperr.ErrInvalidState.Status)
)

// GroupRow 列表行：机组加机器数与现空闲台数。
type GroupRow struct {
	Group
	MachineCount int `json:"machine_count"`
	IdleNow      int `json:"idle_now"`
}

const groupCols = `id, name, region, description, sort, visible, purpose, created_at, updated_at`

func scanGroup(row pgx.Row) (Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.Name, &g.Region, &g.Description, &g.Sort, &g.Visible, &g.Purpose, &g.CreatedAt, &g.UpdatedAt)
	if db.IsNoRows(err) {
		return Group{}, apperr.ErrGroupNotFound
	}
	return g, err
}

// 机组排序值范围、一次分配机器的上限。
const (
	maxGroupSort   = 1_000_000
	maxAssignBatch = 500
)

// defaultLabelRe 匹配缺省展示名「机器 #<id>」；后台设置的展示名不能冒充它。
var defaultLabelRe = regexp.MustCompile(`^机器 #\d+$`)

// hasControlChars 报告 s 是否含控制字符或格式字符（Cc / Cf，如零宽字符、方向控制符）；allowNewline 时放过 \n。
func hasControlChars(s string, allowNewline bool) bool {
	for _, r := range s {
		if allowNewline && r == '\n' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
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
	if hasControlChars(in.Name, false) || hasControlChars(in.Region, false) || hasControlChars(in.Description, true) {
		return in, apperr.BadRequest("名称、位置标签与描述不能包含控制字符")
	}
	if in.Sort < -maxGroupSort || in.Sort > maxGroupSort {
		return in, apperr.BadRequest("排序值需在 -1000000～1000000 之间")
	}
	return in, nil
}

// normalizeMachineIDs 去重并校验机器 ID 列表：非空、均为正数、去重后不超过 maxAssignBatch 台。
func normalizeMachineIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, apperr.BadRequest("请选择机器")
	}
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, apperr.BadRequest("机器 ID 无效")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) > maxAssignBatch {
		return nil, apperr.BadRequest("一次最多选择 500 台机器")
	}
	return out, nil
}

// ListGroups 按 sort、id 返回机组（visibleOnly 只返回控制台可见的），附机器数与现空闲台数。
// productID 非 nil 时两个计数只算该商品的机器（机组可跨商品，规格 §3.1；终审 M1）。
func (s *Service) ListGroups(ctx context.Context, visibleOnly bool, productID *int64) ([]GroupRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+groupCols+`,
			(SELECT count(*) FROM machines m WHERE m.group_id = g.id AND ($2::bigint IS NULL OR m.product_id = $2)),
			(SELECT count(*) FROM machines m WHERE m.group_id = g.id AND ($2::bigint IS NULL OR m.product_id = $2) AND m.state = 'idle' AND m.agent_online)
		FROM machine_groups g WHERE NOT $1 OR visible ORDER BY sort, id`, visibleOnly, productID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (GroupRow, error) {
		var g GroupRow
		err := r.Scan(&g.ID, &g.Name, &g.Region, &g.Description, &g.Sort, &g.Visible, &g.Purpose, &g.CreatedAt, &g.UpdatedAt, &g.MachineCount, &g.IdleNow)
		return g, err
	})
}

func (s *Service) GetGroup(ctx context.Context, q db.Querier, id int64) (Group, error) {
	return scanGroup(q.QueryRow(ctx, `SELECT `+groupCols+` FROM machine_groups WHERE id = $1`, id))
}

// LLMPoolGroupID 返回 Token 服务池（purpose='llm'，迁移建好的唯一一个）的 ID；不存在 → apperr.ErrNotFound。
func (s *Service) LLMPoolGroupID(ctx context.Context, q db.Querier) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM machine_groups WHERE purpose = 'llm'`).Scan(&id)
	if db.IsNoRows(err) {
		return 0, apperr.ErrNotFound
	}
	return id, err
}

func (s *Service) CreateGroup(ctx context.Context, actor audit.Actor, in GroupInput) (Group, error) {
	in, err := in.normalize()
	if err != nil {
		return Group{}, err
	}
	switch in.Purpose {
	case "", GroupPurposeRental:
	case GroupPurposeLLM:
		return Group{}, errCreateLLMGroup
	default:
		return Group{}, apperr.BadRequest("机组用途无效")
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
		if in.Purpose != "" && in.Purpose != before.Purpose {
			return errPurposeImmutable
		}
		if before.Purpose == GroupPurposeLLM && in.Visible {
			return errPoolVisible
		}
		if before.Purpose == GroupPurposeLLM && in.Name != before.Name {
			return errPoolRename
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

// DeleteGroupIn 删除没有机器的机组（实例引用由 rental.AdminDeleteGroup 在同一事务里先检查）；Token 服务池不能删除。
func (s *Service) DeleteGroupIn(ctx context.Context, q db.Querier, id int64) error {
	var purpose string
	var machines int
	err := q.QueryRow(ctx, `SELECT purpose, (SELECT count(*) FROM machines WHERE group_id = $1) FROM machine_groups WHERE id = $1`, id).Scan(&purpose, &machines)
	if db.IsNoRows(err) {
		return apperr.ErrGroupNotFound
	}
	if err != nil {
		return err
	}
	if purpose == GroupPurposeLLM {
		return ErrPoolNotDeletable
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
// 审计记录每台机器原来的机组（previous_group_ids：机器 ID → 原 group_id，null = 原未分组）。
func (s *Service) AssignMachines(ctx context.Context, actor audit.Actor, groupID int64, ids []int64) error {
	ids, err := normalizeMachineIDs(ids)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.GetGroup(ctx, tx, groupID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, group_id FROM machines WHERE id = ANY($1) ORDER BY id FOR NO KEY UPDATE`, ids)
		if err != nil {
			return err
		}
		previous := map[string]*int64{}
		for rows.Next() {
			var id int64
			var prev *int64
			if err := rows.Scan(&id, &prev); err != nil {
				rows.Close()
				return err
			}
			previous[strconv.FormatInt(id, 10)] = prev
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(previous) != len(ids) {
			return apperr.BadRequest("有机器不存在")
		}
		if err := poolLeaveBusy(ctx, tx, ids, &groupID); err != nil {
			return err
		}
		// 机组在 GetGroup 之后被并发删除时外键检查失败，按机组不存在处理。
		if _, err := tx.Exec(ctx, `UPDATE machines SET group_id = $1, updated_at = $3 WHERE id = ANY($2)`, groupID, ids, now); err != nil {
			if db.IsForeignKeyViolation(err) {
				return apperr.ErrGroupNotFound
			}
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.assign", "group", strconv.FormatInt(groupID, 10),
			map[string]any{"machine_ids": ids, "previous_group_ids": previous}))
	})
}

// poolLeaveBusy 机组变更（计划 9 规格 §3.3）：ids 中当前在 Token 服务池（purpose='llm'）、且将离开池（target 为 nil 或不是池）的机器上
// 有未结束的系统实例或非终态节点（llm_nodes，按 SQL 读，不依赖 llm 包）→ apperr.ErrLLMPoolMachineBusy。
// 调用方须已在本事务里对这些机器行 FOR NO KEY UPDATE：rental.PlaceSystem 也先锁机器行并复核 group_id，
// 所以检查与改组之间不会有新的系统实例 / 节点插进来。
func poolLeaveBusy(ctx context.Context, tx pgx.Tx, ids []int64, target *int64) error {
	if len(ids) == 0 {
		return nil
	}
	var busy bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM machines m JOIN machine_groups g ON g.id = m.group_id AND g.purpose = 'llm'
		WHERE m.id = ANY($1) AND ($2::bigint IS NULL OR g.id <> $2)
			AND (EXISTS (SELECT 1 FROM instances i WHERE i.machine_id = m.id AND i.system
					AND i.state IN ('creating', 'running', 'stopped', 'wiping'))
				OR EXISTS (SELECT 1 FROM llm_nodes n WHERE n.machine_id = m.id AND n.state NOT IN ('removed', 'failed'))))`,
		ids, target).Scan(&busy)
	if err != nil {
		return err
	}
	if busy {
		return apperr.ErrLLMPoolMachineBusy
	}
	return nil
}

// UnassignMachines 把机器移出机组（只影响当前确实在该组的机器；审计只记实际移出的）。
func (s *Service) UnassignMachines(ctx context.Context, actor audit.Actor, groupID int64, ids []int64) error {
	ids, err := normalizeMachineIDs(ids)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.GetGroup(ctx, tx, groupID); err != nil {
			return err
		}
		// 先锁住确实在该组的机器（与 rental.PlaceSystem 同一把机器行锁），再查池内节点
		rows, err := tx.Query(ctx, `SELECT id FROM machines WHERE id = ANY($2) AND group_id = $1 ORDER BY id FOR NO KEY UPDATE`, groupID, ids)
		if err != nil {
			return err
		}
		inGroup, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		if err := poolLeaveBusy(ctx, tx, inGroup, nil); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `UPDATE machines SET group_id = NULL, updated_at = $3 WHERE id = ANY($2) AND group_id = $1 RETURNING id`, groupID, ids, now)
		if err != nil {
			return err
		}
		removed, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		if removed == nil {
			removed = []int64{}
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.unassign", "group", strconv.FormatInt(groupID, 10), map[string]any{"machine_ids": removed}))
	})
}

// MachineUpdate 后台可改的机器字段：SetGroup=true 时按 GroupID 改组（nil = 移出机组）；DisplayName 非 nil 时改展示名；
// 计划 13：SetFabric=true 时按 FabricID 改集群网络（nil = 移出）；ClusterIface 非 nil 时改集群网卡（"" = 自动）。
type MachineUpdate struct {
	SetGroup    bool
	GroupID     *int64
	DisplayName *string

	SetFabric    bool
	FabricID     *int64
	ClusterIface *string
}

// errClusterMemberOnMachine 机器上有集群成员实例时不能改集群网络（规格 §3.1）。
var errClusterMemberOnMachine = apperr.New(apperr.ErrInvalidState.Code, "机器上有集群成员，不能修改集群网络", apperr.ErrInvalidState.Status)

// clusterIfaceRe 集群网卡名：Linux 网卡名规则（≤ 15 字符）。
var clusterIfaceRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

func eqInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// UpdateMachine 修改机组 / 展示名 / 集群网络（fabric 与集群网卡）。
// 集群网络有变化时：机器上有 cluster_id 非空的活动实例（creating / running / stopped / wiping）→ invalid_state；
// 加入 direct fabric 后该 fabric 机器数 > 2 → ErrFabricFull；FabricID 不存在 → ErrFabricNotFound；同事务重算就绪判定。
// 锁序 fabric → machine（与 UpdateFabric 一致）：目标 fabric 行先锁，加入 direct fabric 的并发请求由它串行化。
func (s *Service) UpdateMachine(ctx context.Context, actor audit.Actor, id int64, in MachineUpdate) (Machine, error) {
	var name string
	if in.DisplayName != nil {
		name = strings.TrimSpace(*in.DisplayName)
		if utf8.RuneCountInString(name) > 32 {
			return Machine{}, apperr.BadRequest("展示名最多 32 个字符")
		}
		if hasControlChars(name, false) {
			return Machine{}, apperr.BadRequest("展示名不能包含控制字符")
		}
		if defaultLabelRe.MatchString(name) {
			return Machine{}, apperr.BadRequest("展示名不能使用「机器 #编号」格式")
		}
	}
	var iface string
	if in.ClusterIface != nil {
		iface = strings.TrimSpace(*in.ClusterIface)
		if iface != "" && !clusterIfaceRe.MatchString(iface) {
			return Machine{}, apperr.BadRequest("集群网卡名称不正确")
		}
	}
	now := s.clock.Now()
	var out Machine
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if in.SetFabric && in.FabricID != nil {
			if _, err := lockFabric(ctx, tx, *in.FabricID); err != nil {
				return err
			}
		}
		before, err := scanMachine(tx.QueryRow(ctx, `SELECT `+machineCols+` FROM machines WHERE id = $1 FOR NO KEY UPDATE`, id))
		if err != nil {
			return err
		}
		groupID, display := before.GroupID, before.DisplayName
		fabricID, clusterIface := before.FabricID, before.ClusterIface
		if in.SetGroup {
			groupID = in.GroupID
			if groupID != nil {
				if _, err := s.GetGroup(ctx, tx, *groupID); err != nil {
					return err
				}
			}
			if err := poolLeaveBusy(ctx, tx, []int64{id}, groupID); err != nil {
				return err
			}
		}
		if in.DisplayName != nil {
			display = name
		}
		if in.SetFabric {
			fabricID = in.FabricID
		}
		if in.ClusterIface != nil {
			clusterIface = iface
		}
		netChanged := !eqInt64Ptr(fabricID, before.FabricID) || clusterIface != before.ClusterIface
		if netChanged {
			var member bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instances WHERE machine_id = $1 AND cluster_id IS NOT NULL
					AND state IN ('creating', 'running', 'stopped', 'wiping'))`, id).Scan(&member); err != nil {
				return err
			}
			if member {
				return errClusterMemberOnMachine
			}
		}
		out, err = scanMachine(tx.QueryRow(ctx, `UPDATE machines SET group_id = $2, display_name = $3, fabric_id = $4, cluster_iface = $5, updated_at = $6
			WHERE id = $1 RETURNING `+machineCols, id, groupID, display, fabricID, clusterIface, now))
		if db.IsUniqueViolation(err) && db.ConstraintName(err) == "machines_display_name_uniq" {
			return apperr.ErrDuplicateDisplayName
		}
		if db.IsForeignKeyViolation(err) && db.ConstraintName(err) == "machines_fabric_id_fkey" {
			return apperr.ErrFabricNotFound
		}
		if db.IsForeignKeyViolation(err) { // 目标机组在 GetGroup 之后被并发删除
			return apperr.ErrGroupNotFound
		}
		if err != nil {
			return err
		}
		if netChanged {
			if out.fabric != nil && fabricID != nil && !eqInt64Ptr(fabricID, before.FabricID) {
				var isolation string
				var n int
				if err := tx.QueryRow(ctx, `SELECT isolation, (SELECT count(*) FROM machines WHERE fabric_id = $1) FROM cluster_fabrics WHERE id = $1`,
					*fabricID).Scan(&isolation, &n); err != nil {
					return err
				}
				if isolation == IsolationDirect && n > DirectFabricMaxMachines {
					return apperr.ErrFabricFull
				}
			}
			if err := s.recomputeClusterNet(ctx, tx, id); err != nil {
				return err
			}
			if out, err = s.Get(ctx, tx, id); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, now, actor.Entry("machine.update", "machine", strconv.FormatInt(id, 10), map[string]any{
			"before": map[string]any{"group_id": before.GroupID, "display_name": before.DisplayName, "fabric_id": before.FabricID, "cluster_iface": before.ClusterIface},
			"after":  map[string]any{"group_id": out.GroupID, "display_name": out.DisplayName, "fabric_id": out.FabricID, "cluster_iface": out.ClusterIface},
		}))
	})
	return out, err
}
