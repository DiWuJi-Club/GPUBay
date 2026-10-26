package rental

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

// SystemLeaseHorizon 系统实例的占用时段长度（规格 §4.1）：lease active [now, now + 10 年)，等价于「不到期」；
// occupiedUntil 对系统实例返回 lower + SystemLeaseHorizon，上界不随时间变化。
const SystemLeaseHorizon = 10 * 365 * 24 * time.Hour

// SystemPlaceRequest 在一台池内机器上开通系统实例（平台节点）。
type SystemPlaceRequest struct {
	MachineID int64
	GroupID   int64 // 机器必须属于该机组（池）
	ImageID   int64
	Note      string // 事件文案，如 "llm node node-xxxx qwen3-4b"
}

var errNoSystemUser = errors.New("未配置系统账号")

// PlaceSystem 在一台池内空闲机器上开通系统实例（不计费、不到期，规格 §4.1）：
// 锁机器行（FOR NO KEY UPDATE，等待：调用方点名的机器不该被悄悄跳过）→ 复核机组 / idle / 在线 / 无活动实例 / 无相交 lease
// → 插实例（system、password_auth=false、pending_seconds=0、expires_at NULL）→ 插 lease active [now, now + SystemLeaseHorizon)
// → 机器 allocated → 事件 → Create 指令。不查 CanRent、不建订单、不扣款、不调 AppHooks.QueueInstall（部署由调用方在 running 后创建）。
// 条件不满足 → ErrMachineUnavailable。锁序 machines → instances / machine_leases → agent_commands。
func (s *Service) PlaceSystem(ctx context.Context, req SystemPlaceRequest) (Instance, error) {
	if s.d.SystemUserID == 0 {
		return Instance{}, errNoSystemUser
	}
	var out Instance
	err := withRetry(func() error {
		return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			inst, ref, err := s.placeSystemOn(ctx, tx, req, s.d.Clock.Now(), nil)
			if err != nil {
				return err
			}
			// 与其它开通路径同一形态的 Create 指令（计划 10：显式 ssh_port、share_slots = 1）
			if err := s.enqueueCreate(ctx, tx, inst, ref); err != nil {
				return err
			}
			out = inst
			return nil
		})
	})
	return out, err
}

// clusterRef 是系统集群成员的集群归属（PlaceSystemCluster 插实例时直接写 cluster_id / cluster_rank）。
type clusterRef struct {
	ID   string
	Rank int
}

// placeSystemOn 是 PlaceSystem 与 PlaceSystemCluster 共用的单机开通（不入队 Create：集群成员的 Spec 要等全员插好才能构造）：
// 锁机器行并复核条件 → 插系统实例（cluster 非空时带 cluster_id / cluster_rank）→ lease → allocated → 事件。
// 返回实例与镜像地址。条件不满足 → ErrMachineUnavailable。
func (s *Service) placeSystemOn(ctx context.Context, tx pgx.Tx, req SystemPlaceRequest, now time.Time, cluster *clusterRef) (Instance, string, error) {
	var productID int64
	// 机组必须就是 Token 服务池（purpose='llm'）：调用方传错机组也不能把普通机组的机器拿去建节点
	err := tx.QueryRow(ctx, `SELECT product_id FROM machines WHERE id = $1 AND group_id = $2 AND state = 'idle' AND agent_online
		AND EXISTS (SELECT 1 FROM machine_groups g WHERE g.id = $2 AND g.purpose = 'llm')
		FOR NO KEY UPDATE`, req.MachineID, req.GroupID).Scan(&productID)
	if db.IsNoRows(err) {
		return Instance{}, "", apperr.ErrMachineUnavailable
	}
	if err != nil {
		return Instance{}, "", err
	}
	// 加锁之后单独一条语句复核（读已提交：新语句的快照能看到等锁期间别人提交的实例 / lease）
	lower, upper := now, now.Add(SystemLeaseHorizon)
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instances
			WHERE machine_id = $1 AND state IN ('creating', 'running', 'stopped', 'wiping'))
		OR EXISTS (SELECT 1 FROM machine_leases
			WHERE machine_id = $1 AND status IN ('reserved', 'active') AND period && tstzrange($2::timestamptz, $3::timestamptz))`,
		req.MachineID, lower, upper).Scan(&busy); err != nil {
		return Instance{}, "", err
	}
	if busy {
		return Instance{}, "", apperr.ErrMachineUnavailable
	}
	ref, err := s.d.Catalog.ImageRef(ctx, tx, req.ImageID)
	if err != nil {
		return Instance{}, "", err
	}
	hostPub, hostPEM, err := s.d.HostKeyGen()
	if err != nil {
		return Instance{}, "", err
	}
	if _, err := s.d.Fleet.MarkAllocated(ctx, tx, req.MachineID); err != nil {
		return Instance{}, "", err
	}
	var clusterID *string
	var clusterRank *int
	if cluster != nil {
		clusterID, clusterRank = &cluster.ID, &cluster.Rank
	}
	id := randx.ID("ins-", 8)
	inst, err := scanInstance(tx.QueryRow(ctx, `INSERT INTO instances
		(id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, password_auth, ssh_host_pubkey, ssh_host_key_enc,
		 group_id, system, cluster_id, cluster_rank, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'creating', 0, $6, false, $7, $8, $9, true, $10, $11, $12, $12) RETURNING `+instanceCols,
		id, s.d.SystemUserID, req.MachineID, productID, req.ImageID, s.d.Box.Seal([]byte(randx.Password(16))), hostPub, s.d.Box.Seal(hostPEM),
		req.GroupID, clusterID, clusterRank, now))
	if err != nil {
		return Instance{}, "", err
	}
	if _, err := insertLease(ctx, tx, req.MachineID, inst.ID, lower, upper, "active", now); err != nil {
		if db.IsExclusionViolation(err) { // 复核之后别的事务才提交的时段（理论上被机器行锁挡住，兜底）
			return Instance{}, "", apperr.ErrMachineUnavailable
		}
		return Instance{}, "", err
	}
	if err := recordEvent(ctx, tx, inst.ID, "", StateCreating, "system instance for "+req.Note, now); err != nil {
		return Instance{}, "", err
	}
	return inst, ref, nil
}

// ListLiveSystemInstances 全部未结束（creating / running / stopped / wiping）的系统实例，按 id 排序；
// 供 llm Controller 对账（找出没有对应节点的孤儿系统实例）。走 instances_system_live_idx 部分索引。
func (s *Service) ListLiveSystemInstances(ctx context.Context) ([]Instance, error) {
	return collectInstances(s.d.Pool.Query(ctx, `SELECT `+instanceCols+` FROM instances
		WHERE system AND state IN ('creating', 'running', 'stopped', 'wiping') ORDER BY id`))
}

// ReleaseSystem 释放系统实例（creating / running / stopped 均可，规格 §4.2）：与 Release 走同一链路（beginWipe：
// 取消未完成的指令——creating 时即待执行的 Create——再下发洗机）；不校验用户。非系统实例 → ErrInvalidState。
func (s *Service) ReleaseSystem(ctx context.Context, instanceID string) error {
	return s.release(ctx, instanceID, "system released", func(inst Instance) error {
		if !inst.System {
			return apperr.ErrInvalidState
		}
		switch inst.State {
		case StateCreating, StateRunning, StateStopped:
			return nil
		}
		return apperr.ErrInvalidState
	})
}

// release 是 Release 与 ReleaseSystem 共用的释放事务：锁实例 → check（用户 / 状态校验）→ beginWipe。
func (s *Service) release(ctx context.Context, instanceID, reason string, check func(Instance) error) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if err := check(inst); err != nil {
			return err
		}
		return s.beginWipe(ctx, tx, inst, reason, now)
	})
}
