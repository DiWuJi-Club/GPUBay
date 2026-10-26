package rental

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

// clusterCandidateSQL：可组成 n 台集群的 (fabric, cluster_net) 候选（规格 §5.1）。条件与 pickWhereSQL 相同（pickCondSQL，
// 调用方传 ClusterOnly 的 spec），再按 (fabric_id, cluster_net) 分组：routed 成员宿主机必须同一网段，cx7 的 cluster_net 恒为 'l2'。
// 排序：有效 RDMA 优先 → 剩余空闲最少（最贴合，减少碎片）→ fabric ID → 网段。direct fabric 只能组 2 台。
// 这里不能把只选 m.id 的 pickWhereSQL 当子查询（要分组列），所以单独写 FROM，共用 WHERE 片段。
var clusterCandidateSQL = `SELECT m.fabric_id, m.cluster_net, (f.rdma AND bool_and(m.cluster_rdma)) AS rdma, count(*)::int AS free
	FROM machines m JOIN cluster_fabrics f ON f.id = m.fabric_id LEFT JOIN machine_groups g ON g.id = m.group_id
	WHERE ` + pickCondSQL + ` AND m.cluster_ready
	GROUP BY m.fabric_id, m.cluster_net, f.rdma, f.isolation
	HAVING count(*) >= ` + fmt.Sprintf("$%d", pickArgCount+1) + `::int AND (f.isolation <> 'direct' OR ` + fmt.Sprintf("$%d", pickArgCount+1) + `::int = 2)
	ORDER BY rdma DESC, free ASC, m.fabric_id, m.cluster_net`

type clusterCandidate struct {
	FabricID   int64
	ClusterNet string
	RDMA       bool
	Free       int
}

func clusterCandidates(ctx context.Context, q db.Querier, spec pickSpec, n int) ([]clusterCandidate, error) {
	spec.ClusterOnly = true
	spec.FabricID, spec.ClusterNet = nil, nil
	rows, err := q.Query(ctx, clusterCandidateSQL, append(pickArgs(spec), n)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (clusterCandidate, error) {
		var c clusterCandidate
		err := r.Scan(&c.FabricID, &c.ClusterNet, &c.RDMA, &c.Free)
		return c, err
	})
}

// errClusterVLANExhausted：vlan fabric 的 VLAN 范围已被活动集群占满（换下一个候选 fabric）。
var errClusterVLANExhausted = errors.New("cluster vlan range exhausted")

// errClusterSubnetExhausted：集群网段池的 256 个 /24 已被活动集群占满。
var errClusterSubnetExhausted = errors.New("cluster subnet pool exhausted")

// placeCluster 是 PlaceBatch 集群分支的挑机器（规格 §5.1）：逐个 (fabric, cluster_net) 候选开一个 savepoint，
// 先 reset（清空回调追加的实例列表：上一个候选失败时回调已追加过、但随 savepoint 回滚了的实例），
// 再原样调用 placeOnMachines（锁 / 排他冲突换机 / 补取）；不足 n 台 → 回滚该 savepoint（放掉本次加的机器行锁）试下一个。
// 成功 → 同一 savepoint 里 createCluster + assignRanks（成员按机器 ID 升序给 rank）。都不行 → ClusterNoCapacity(n)。
// insts 是回调追加的实例列表（成功时其 ClusterID / ClusterRank 已回填）。
func (s *Service) placeCluster(ctx context.Context, tx pgx.Tx, spec pickSpec, n int, userID int64, batchID *string, now time.Time,
	insts *[]Instance, place func(sp pgx.Tx, machineID int64) error) (Cluster, error) {
	cands, err := clusterCandidates(ctx, tx, spec, n)
	if err != nil {
		return Cluster{}, err
	}
	for _, cand := range cands {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return Cluster{}, err
		}
		*insts = (*insts)[:0]
		s2 := spec
		s2.FabricID, s2.ClusterNet, s2.ClusterOnly = &cand.FabricID, &cand.ClusterNet, true
		// 锁序 fabric → machine（与 UpdateFabric / UpdateMachine 一致）：先 FOR SHARE 锁 fabric 行，改 fabric 与开通串行化
		err = shareLockFabric(ctx, sp, cand.FabricID)
		var ids []int64
		if err == nil {
			ids, err = placeOnMachines(ctx, sp, s2, n, place)
		}
		if err == nil && len(ids) != n {
			err = fmt.Errorf("placeOnMachines returned %d machines, want %d", len(ids), n)
		}
		var c Cluster
		if err == nil {
			c, err = s.createCluster(ctx, sp, clusterParams{UserID: userID, FabricID: cand.FabricID, MachineIDs: ids, BatchID: batchID, Now: now})
		}
		if err == nil {
			err = assignRanks(ctx, sp, c, *insts, now)
		}
		if err != nil {
			_ = sp.Rollback(ctx)
			if codeIs(err, "no_capacity") || errors.Is(err, errClusterVLANExhausted) || errors.Is(err, apperr.ErrFabricNotFound) {
				continue
			}
			if errors.Is(err, errClusterSubnetExhausted) {
				break
			}
			return Cluster{}, err
		}
		if err := sp.Commit(ctx); err != nil {
			return Cluster{}, err
		}
		return c, nil
	}
	*insts = (*insts)[:0]
	return Cluster{}, apperr.ClusterNoCapacity(n)
}

// shareLockFabric FOR SHARE 锁 fabric 行（开通集群在锁机器之前调用）：与 UpdateFabric / UpdateMachine 的 FOR NO KEY UPDATE 互斥，
// 多个开通之间不互斥。fabric 已被删除 → ErrFabricNotFound。全部路径的锁序都是 fabric → machine（fleet 里重算就绪只读 fabric、不加锁）。
func shareLockFabric(ctx context.Context, tx pgx.Tx, fabricID int64) error {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM cluster_fabrics WHERE id = $1 FOR SHARE`, fabricID).Scan(&id)
	if db.IsNoRows(err) {
		return apperr.ErrFabricNotFound
	}
	return err
}

type clusterParams struct {
	UserID     int64
	System     bool
	FabricID   int64
	MachineIDs []int64 // 成员机器（已锁）；有效 RDMA = fabric.rdma ∧ 它们的 cluster_rdma 全真
	BatchID    *string
	Now        time.Time
}

// clusterLiveSQL：集群 c 仍占用子网 / VLAN——active，或已结束（开通期中止置 failed）但仍有未结束成员（含 wiping）：
// 离线成员重连后 Agent 会按旧 Spec 恢复 spc0 / VLAN，直到洗机完成才拆掉（规格 §5.5 第 4 步）。$1 / $4 = clusterUnfinishedStates。
const clusterLiveSQL = `(c.state = 'active' OR EXISTS (SELECT 1 FROM instances i WHERE i.cluster_id = c.id AND i.state = ANY($1)))`
const clusterLiveSQL4 = `(c.state = 'active' OR EXISTS (SELECT 1 FROM instances i WHERE i.cluster_id = c.id AND i.state = ANY($4)))`

// createCluster 插一行 active 的 instance_clusters（规格 §5.1）：在机器行锁之后取 cluster-alloc 咨询锁（串行化子网 / VLAN 分配），
// subnet_index = 最小的未被占用（clusterLiveSQL）的 0..255；vlan fabric 取 [vlan_min, vlan_max] 中本 fabric 最小未占用的 VLAN；
// mode = routed（lan）/ macvlan（cx7）；rdma = fabric.rdma ∧ 全员 cluster_rdma；mtu = fabric.mtu。
// fabric 要 RDMA 而有效 RDMA 为假 → 同事务告警 cluster_rdma_degraded（一次）。
func (s *Service) createCluster(ctx context.Context, tx pgx.Tx, p clusterParams) (Cluster, error) {
	var kind, isolation string
	var fabricRDMA bool
	var mtu int
	var vlanMin, vlanMax *int
	if err := tx.QueryRow(ctx, `SELECT kind, isolation, rdma, mtu, vlan_min, vlan_max FROM cluster_fabrics WHERE id = $1`, p.FabricID).
		Scan(&kind, &isolation, &fabricRDMA, &mtu, &vlanMin, &vlanMax); err != nil {
		if db.IsNoRows(err) {
			return Cluster{}, apperr.ErrFabricNotFound
		}
		return Cluster{}, err
	}
	var allRDMA bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(bool_and(cluster_rdma), false) FROM machines WHERE id = ANY($1)`, p.MachineIDs).Scan(&allRDMA); err != nil {
		return Cluster{}, err
	}
	rdma := fabricRDMA && allRDMA
	mode := clusterspec.ModeRouted
	if kind == "cx7" {
		mode = clusterspec.ModeMacvlan
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('cluster-alloc'))`); err != nil {
		return Cluster{}, err
	}
	var subnet int
	err := tx.QueryRow(ctx, `SELECT g FROM generate_series(0, 255) g
		WHERE NOT EXISTS (SELECT 1 FROM instance_clusters c WHERE c.subnet_index = g AND `+clusterLiveSQL+`) ORDER BY g LIMIT 1`,
		clusterUnfinishedStates).Scan(&subnet)
	if db.IsNoRows(err) {
		return Cluster{}, errClusterSubnetExhausted
	}
	if err != nil {
		return Cluster{}, err
	}
	var vlan *int
	if isolation == "vlan" && vlanMin != nil && vlanMax != nil {
		var v int
		err := tx.QueryRow(ctx, `SELECT v FROM generate_series($1::int, $2::int) v
			WHERE NOT EXISTS (SELECT 1 FROM instance_clusters c WHERE c.fabric_id = $3 AND c.vlan = v AND `+clusterLiveSQL4+`) ORDER BY v LIMIT 1`,
			*vlanMin, *vlanMax, p.FabricID, clusterUnfinishedStates).Scan(&v)
		if db.IsNoRows(err) {
			return Cluster{}, errClusterVLANExhausted
		}
		if err != nil {
			return Cluster{}, err
		}
		vlan = &v
	}
	c, err := scanCluster(tx.QueryRow(ctx, `INSERT INTO instance_clusters
		(id, user_id, system, size, fabric_id, mode, vlan, rdma, mtu, subnet_index, version, state, batch_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1, 'active', $11, $12, $12) RETURNING `+clusterCols,
		randx.ID("clu-", 10), p.UserID, p.System, len(p.MachineIDs), p.FabricID, mode, vlan, rdma, mtu, subnet, p.BatchID, p.Now))
	if err != nil {
		return Cluster{}, err
	}
	if fabricRDMA && !rdma {
		if err := s.d.Fleet.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "warning", Kind: "cluster_rdma_degraded",
			Message: AlertClusterRDMADegraded(c.ID)}); err != nil {
			return Cluster{}, err
		}
	}
	return c, nil
}

// AlertClusterRDMADegraded 集群 fabric 要 RDMA 但成员探测不可用（开通时判定），退回以太网。
func AlertClusterRDMADegraded(clusterID string) string {
	return fmt.Sprintf("集群 %s 的 RDMA 不可用，已退回以太网", clusterID)
}

// assignRanks 按机器 ID 升序给成员 rank 0..N-1 并写 instances.cluster_id / cluster_rank；insts 原地回填。
func assignRanks(ctx context.Context, tx pgx.Tx, c Cluster, insts []Instance, now time.Time) error {
	if len(insts) != c.Size {
		return fmt.Errorf("cluster %s: %d members, want %d", c.ID, len(insts), c.Size)
	}
	order := make([]int, len(insts))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return int(insts[a].MachineID - insts[b].MachineID) })
	for rank, idx := range order {
		if _, err := tx.Exec(ctx, `UPDATE instances SET cluster_id = $2, cluster_rank = $3, updated_at = $4 WHERE id = $1`,
			insts[idx].ID, c.ID, rank, now); err != nil {
			return err
		}
		id, r := c.ID, rank
		insts[idx].ClusterID, insts[idx].ClusterRank = &id, &r
	}
	return nil
}

// enqueueClusterCreate 给实例入队 Create：集群成员带 ClusterSpec（须在全部成员都写好 cluster_id / rank 之后调用），非成员同 enqueueCreate。
func (s *Service) enqueueClusterCreate(ctx context.Context, tx pgx.Tx, inst Instance, imageRef string) error {
	if inst.ClusterID == nil || inst.ClusterRank == nil {
		return s.enqueueCreate(ctx, tx, inst, imageRef)
	}
	spec, err := s.ClusterSpecFor(ctx, tx, *inst.ClusterID, *inst.ClusterRank)
	if err != nil {
		return err
	}
	return s.enqueueCreateSpec(ctx, tx, inst, imageRef, &spec)
}

// PlaceSystemCluster 在指定的池内机器上开通一个系统集群（Token 商城节点组，规格 §5.2）：
// 按机器 ID 升序 FOR NO KEY UPDATE（等待）→ 每台复核 PlaceSystem 的条件 + 已加入 fabric 且 cluster_ready + 同一 fabric、同一 cluster_net
// → createCluster(system) → 每台插系统实例（cluster_id / rank 按机器 ID 升序）+ lease 10 年 + allocated + 事件 → 带 Spec 的 Create。
// 机器数 ∉ ClusterSizes → ErrClusterSizeInvalid；任一台不满足 → ErrMachineUnavailable，整个事务回滚。
func (s *Service) PlaceSystemCluster(ctx context.Context, req SystemClusterRequest) (SystemClusterResult, error) {
	if s.d.SystemUserID == 0 {
		return SystemClusterResult{}, errNoSystemUser
	}
	if !slices.Contains(ClusterSizes, len(req.MachineIDs)) {
		return SystemClusterResult{}, apperr.ErrClusterSizeInvalid
	}
	ids := slices.Clone(req.MachineIDs)
	slices.Sort(ids)
	if len(slices.Compact(slices.Clone(ids))) != len(ids) {
		return SystemClusterResult{}, apperr.ErrMachineUnavailable
	}
	var out SystemClusterResult
	err := withRetry(func() error {
		out = SystemClusterResult{}
		return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			now := s.d.Clock.Now()
			var fabricID int64
			var clusterNet string
			// 锁序 fabric → machine：先按（未加锁读到的）首台机器的 fabric FOR SHARE，下面锁机器后复核每台都在这个 fabric
			var want *int64
			if err := tx.QueryRow(ctx, `SELECT fabric_id FROM machines WHERE id = $1`, ids[0]).Scan(&want); err != nil {
				if db.IsNoRows(err) {
					return apperr.ErrMachineUnavailable
				}
				return err
			}
			if want == nil {
				return apperr.ErrMachineUnavailable
			}
			if err := shareLockFabric(ctx, tx, *want); err != nil {
				if errors.Is(err, apperr.ErrFabricNotFound) {
					return apperr.ErrMachineUnavailable
				}
				return err
			}
			for i, mid := range ids {
				var fid int64
				var cnet string
				err := tx.QueryRow(ctx, `SELECT fabric_id, cluster_net FROM machines
					WHERE id = $1 AND group_id = $2 AND state = 'idle' AND agent_online AND fabric_id IS NOT NULL AND cluster_ready
					  AND EXISTS (SELECT 1 FROM machine_groups g WHERE g.id = $2 AND g.purpose = 'llm')
					FOR NO KEY UPDATE`, mid, req.GroupID).Scan(&fid, &cnet)
				if db.IsNoRows(err) {
					return apperr.ErrMachineUnavailable
				}
				if err != nil {
					return err
				}
				if i == 0 {
					fabricID, clusterNet = fid, cnet
				}
				if fid != *want || fid != fabricID || cnet != clusterNet { // 首台在加锁前被移到别的 fabric：不在已锁的 fabric 上
					return apperr.ErrMachineUnavailable
				}
			}
			c, err := s.createCluster(ctx, tx, clusterParams{UserID: s.d.SystemUserID, System: true, FabricID: fabricID, MachineIDs: ids, Now: now})
			if errors.Is(err, errClusterVLANExhausted) || errors.Is(err, errClusterSubnetExhausted) {
				return apperr.ClusterNoCapacity(len(ids))
			}
			if err != nil {
				return err
			}
			refs := make([]string, len(ids))
			for rank, mid := range ids {
				inst, ref, err := s.placeSystemOn(ctx, tx, SystemPlaceRequest{MachineID: mid, GroupID: req.GroupID, ImageID: req.ImageID, Note: req.Note},
					now, &clusterRef{ID: c.ID, Rank: rank})
				if err != nil {
					return err
				}
				out.Instances = append(out.Instances, inst)
				refs[rank] = ref
			}
			for rank, inst := range out.Instances {
				if err := s.enqueueClusterCreate(ctx, tx, inst, refs[rank]); err != nil {
					return err
				}
			}
			out.Cluster = c
			return nil
		})
	})
	if err != nil {
		return SystemClusterResult{}, err
	}
	return out, nil
}

// ReleaseSystemCluster 释放系统集群的全部在位成员（规格 §5.2）：锁集群行 → 按实例 ID 升序锁在位成员 → 每个 beginWipe
// （与 ReleaseSystem 同链路）。集群行的结束（released / failed）由 SweepClusters 在成员都结束后处理。
// 不存在 → ErrClusterNotFound；非系统集群 → ErrInvalidState；已无在位成员 → nil。
func (s *Service) ReleaseSystemCluster(ctx context.Context, clusterID string) error {
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		now := s.d.Clock.Now()
		c, err := lockCluster(ctx, tx, clusterID)
		if err != nil {
			return err
		}
		if !c.System {
			return apperr.ErrInvalidState
		}
		members, err := lockClusterMembers(ctx, tx, clusterID)
		if err != nil {
			return err
		}
		for _, inst := range members {
			if err := s.beginWipe(ctx, tx, inst, "system cluster released", now); err != nil {
				return err
			}
		}
		return nil
	})
}

// ClusterAvailability 返回此刻（按最短租期 1 天探测，与 AvailableNow 一致）可组成的集群数（规格 §5.1）：
// 键为 ClusterSizes；available = Σ_(fabric, cluster_net) ⌊free / n⌋（direct 只计入 2 台）；rdma = 有能组成 n 台的有效 RDMA 候选。
// groupID 为 nil = 未分组 + 可见机组；非 nil 只看该机组（不校验可见性，调用方负责）。
func (s *Service) ClusterAvailability(ctx context.Context, productID int64, groupID *int64) (map[int]ClusterAvail, error) {
	spec := pickSpec{ProductID: productID, GroupID: groupID, Immediate: true}
	spec.Lower, spec.Upper = immediatePeriod(s.d.Clock.Now(), 86400)
	out := make(map[int]ClusterAvail, len(ClusterSizes))
	for _, n := range ClusterSizes {
		cands, err := clusterCandidates(ctx, s.d.Pool, spec, n)
		if err != nil {
			return nil, err
		}
		var a ClusterAvail
		for _, c := range cands {
			a.Available += c.Free / n
			a.RDMA = a.RDMA || c.RDMA
		}
		out[n] = a
	}
	return out, nil
}
