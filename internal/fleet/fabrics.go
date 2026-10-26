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

// fabric 的类型与隔离方式（计划 13 规格 §3.1）。
const (
	FabricKindLAN = "lan" // 普通以太网：routed（veth + 宿主机路由）
	FabricKindCX7 = "cx7" // ConnectX-7：direct（两台直连）/ vlan（交换机 trunk）

	IsolationRouted = "routed"
	IsolationDirect = "direct"
	IsolationVLAN   = "vlan"

	// DirectFabricMaxMachines direct fabric 最多挂的机器数（两台 QSFP 直连）。
	DirectFabricMaxMachines = 2
)

// Fabric 集群网络：一组物理互通的机器（cluster_fabrics）。
type Fabric struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Isolation   string    `json:"isolation"`
	RDMA        bool      `json:"rdma"`
	MTU         int       `json:"mtu"`
	VLANMin     *int      `json:"vlan_min"`
	VLANMax     *int      `json:"vlan_max"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// FabricInput 建 / 改 fabric 的输入。
type FabricInput struct {
	Name, Kind, Isolation string
	RDMA                  bool
	MTU                   int
	VLANMin, VLANMax      *int
	Description           string
}

// FabricRow 列表行：fabric 加机器数、活跃集群数与成员机器的集群网段。
// ClusterNets 是该 fabric 下已就绪机器的 cluster_net 去重（升序）；lan fabric 出现多个网段时后台标黄
// （routed 只在同一网段内组队，规格 §3.2）。
type FabricRow struct {
	Fabric
	MachineCount   int      `json:"machine_count"`
	ActiveClusters int      `json:"active_clusters"`
	ClusterNets    []string `json:"cluster_nets"`
}

const fabricCols = `id, name, kind, isolation, rdma, mtu, vlan_min, vlan_max, description, created_at, updated_at`

func scanFabric(row pgx.Row) (Fabric, error) {
	var f Fabric
	err := row.Scan(&f.ID, &f.Name, &f.Kind, &f.Isolation, &f.RDMA, &f.MTU, &f.VLANMin, &f.VLANMax, &f.Description, &f.CreatedAt, &f.UpdatedAt)
	if db.IsNoRows(err) {
		return Fabric{}, apperr.ErrFabricNotFound
	}
	return f, err
}

var errFabricNameTaken = apperr.FabricInvalid("name", "已有同名集群网络")

func (in FabricInput) normalize() (FabricInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 32 {
		return in, apperr.FabricInvalid("name", "需为 1～32 个字符")
	}
	if hasControlChars(in.Name, false) {
		return in, apperr.FabricInvalid("name", "不能包含控制字符")
	}
	if utf8.RuneCountInString(in.Description) > 200 {
		return in, apperr.FabricInvalid("description", "最多 200 个字符")
	}
	if hasControlChars(in.Description, true) {
		return in, apperr.FabricInvalid("description", "不能包含控制字符")
	}
	switch in.Kind {
	case FabricKindLAN:
		if in.Isolation != IsolationRouted {
			return in, apperr.FabricInvalid("isolation", "普通以太网只能使用三层路由隔离")
		}
		if in.RDMA {
			return in, apperr.FabricInvalid("rdma", "只有 ConnectX-7 网络支持 RDMA")
		}
	case FabricKindCX7:
		if in.Isolation != IsolationDirect && in.Isolation != IsolationVLAN {
			return in, apperr.FabricInvalid("isolation", "ConnectX-7 网络只能使用直连或 VLAN 隔离")
		}
	default:
		return in, apperr.FabricInvalid("kind", "只能是 lan 或 cx7")
	}
	if in.MTU != 0 && (in.MTU < 1280 || in.MTU > 9216) {
		return in, apperr.FabricInvalid("mtu", "需为 0（继承网卡）或 1280～9216")
	}
	if in.Isolation == IsolationVLAN {
		if in.VLANMin == nil || in.VLANMax == nil {
			return in, apperr.FabricInvalid("vlan_min", "VLAN 隔离需填写 VLAN 范围")
		}
		if *in.VLANMin < 2 || *in.VLANMin > 4094 || *in.VLANMax < *in.VLANMin || *in.VLANMax > 4094 {
			return in, apperr.FabricInvalid("vlan_min", "VLAN 范围需在 2～4094 之间且起始不大于结束")
		}
	} else if in.VLANMin != nil || in.VLANMax != nil {
		return in, apperr.FabricInvalid("vlan_min", "只有 VLAN 隔离可以设置 VLAN 范围")
	}
	return in, nil
}

// ListFabrics 按 id 返回全部 fabric，附机器数、活跃集群数与就绪机器的网段。
func (s *Service) ListFabrics(ctx context.Context) ([]FabricRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+fabricCols+`,
			(SELECT count(*) FROM machines m WHERE m.fabric_id = f.id),
			(SELECT count(*) FROM instance_clusters c WHERE c.fabric_id = f.id AND c.state = 'active'),
			ARRAY(SELECT DISTINCT m.cluster_net FROM machines m WHERE m.fabric_id = f.id AND m.cluster_net <> '' ORDER BY 1)
		FROM cluster_fabrics f ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (FabricRow, error) {
		var f FabricRow
		err := r.Scan(&f.ID, &f.Name, &f.Kind, &f.Isolation, &f.RDMA, &f.MTU, &f.VLANMin, &f.VLANMax, &f.Description, &f.CreatedAt, &f.UpdatedAt,
			&f.MachineCount, &f.ActiveClusters, &f.ClusterNets)
		if f.ClusterNets == nil {
			f.ClusterNets = []string{}
		}
		return f, err
	})
}

// GetFabric 不存在 → ErrFabricNotFound。
func (s *Service) GetFabric(ctx context.Context, q db.Querier, id int64) (Fabric, error) {
	return scanFabric(q.QueryRow(ctx, `SELECT `+fabricCols+` FROM cluster_fabrics WHERE id = $1`, id))
}

// lockFabric 锁 fabric 行（NO KEY UPDATE：不挡外键 KEY SHARE）。锁序：fabric → machines（UpdateFabric、UpdateMachine 一致）。
func lockFabric(ctx context.Context, tx pgx.Tx, id int64) (Fabric, error) {
	return scanFabric(tx.QueryRow(ctx, `SELECT `+fabricCols+` FROM cluster_fabrics WHERE id = $1 FOR NO KEY UPDATE`, id))
}

func fabricAudit(f Fabric) map[string]any {
	return map[string]any{"name": f.Name, "kind": f.Kind, "isolation": f.Isolation, "rdma": f.RDMA, "mtu": f.MTU,
		"vlan_min": f.VLANMin, "vlan_max": f.VLANMax, "description": f.Description}
}

// CreateFabric 新建 fabric；审计 fabric.create。
func (s *Service) CreateFabric(ctx context.Context, actor audit.Actor, in FabricInput) (Fabric, error) {
	in, err := in.normalize()
	if err != nil {
		return Fabric{}, err
	}
	now := s.clock.Now()
	var f Fabric
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		f, err = scanFabric(tx.QueryRow(ctx, `INSERT INTO cluster_fabrics (name, kind, isolation, rdma, mtu, vlan_min, vlan_max, description, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9) RETURNING `+fabricCols,
			in.Name, in.Kind, in.Isolation, in.RDMA, in.MTU, in.VLANMin, in.VLANMax, in.Description, now))
		if db.IsUniqueViolation(err) {
			return errFabricNameTaken
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("fabric.create", "fabric", strconv.FormatInt(f.ID, 10), fabricAudit(f)))
	})
	return f, err
}

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// UpdateFabric 修改 fabric：有 active 集群时 kind / isolation / VLAN 范围变化 → ErrFabricInUse；改成 direct 而机器数 > 2 → ErrFabricFull。
// 同事务重算该 fabric 全部机器的 cluster_ready / cluster_rdma / cluster_net；审计 fabric.update（before / after）。
func (s *Service) UpdateFabric(ctx context.Context, actor audit.Actor, id int64, in FabricInput) (Fabric, error) {
	in, err := in.normalize()
	if err != nil {
		return Fabric{}, err
	}
	now := s.clock.Now()
	var f Fabric
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := lockFabric(ctx, tx, id)
		if err != nil {
			return err
		}
		var machines, active int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM machines WHERE fabric_id = $1),
				(SELECT count(*) FROM instance_clusters WHERE fabric_id = $1 AND state = 'active')`, id).Scan(&machines, &active); err != nil {
			return err
		}
		structural := in.Kind != before.Kind || in.Isolation != before.Isolation ||
			!eqIntPtr(in.VLANMin, before.VLANMin) || !eqIntPtr(in.VLANMax, before.VLANMax)
		if structural && active > 0 {
			return apperr.ErrFabricInUse
		}
		if in.Isolation == IsolationDirect && machines > DirectFabricMaxMachines {
			return apperr.ErrFabricFull
		}
		f, err = scanFabric(tx.QueryRow(ctx, `UPDATE cluster_fabrics SET name = $2, kind = $3, isolation = $4, rdma = $5, mtu = $6,
				vlan_min = $7, vlan_max = $8, description = $9, updated_at = $10
			WHERE id = $1 RETURNING `+fabricCols,
			id, in.Name, in.Kind, in.Isolation, in.RDMA, in.MTU, in.VLANMin, in.VLANMax, in.Description, now))
		if db.IsUniqueViolation(err) {
			return errFabricNameTaken
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id FROM machines WHERE fabric_id = $1 ORDER BY id FOR NO KEY UPDATE`, id)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		for _, mid := range ids {
			if err := s.recomputeClusterNet(ctx, tx, mid); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, now, actor.Entry("fabric.update", "fabric", strconv.FormatInt(id, 10),
			map[string]any{"before": fabricAudit(before), "after": fabricAudit(f)}))
	})
	return f, err
}

// DeleteFabric 删除 fabric：有机器或 active 集群（或被历史集群引用）→ ErrFabricInUse；审计 fabric.delete。
func (s *Service) DeleteFabric(ctx context.Context, actor audit.Actor, id int64) error {
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		f, err := lockFabric(ctx, tx, id)
		if err != nil {
			return err
		}
		var busy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM machines WHERE fabric_id = $1)
				OR EXISTS (SELECT 1 FROM instance_clusters WHERE fabric_id = $1 AND state = 'active')`, id).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return apperr.ErrFabricInUse
		}
		// 已结束的集群仍以外键引用 fabric（历史记录不删）：同样按占用处理。
		if _, err := tx.Exec(ctx, `DELETE FROM cluster_fabrics WHERE id = $1`, id); err != nil {
			if db.IsForeignKeyViolation(err) {
				return apperr.ErrFabricInUse
			}
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("fabric.delete", "fabric", strconv.FormatInt(id, 10), fabricAudit(f)))
	})
}
