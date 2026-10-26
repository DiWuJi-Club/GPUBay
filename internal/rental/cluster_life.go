package rental

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/tz"
)

// 计划 13：集群生命周期（规格 §5.5～§5.8）。
// 成员口径（规格 §5.4）：在位成员 = ClusterMemberStates（creating / running / stopped），ClusterSpec、rank、续费 / 释放都按它；
// 未结束成员 = 在位 ∪ wiping（clusterUnfinishedStates），集群是否结束按它。
// 锁序：集群行 FOR UPDATE → 成员实例按 ID 升序 → 其后与单实例路径相同（lease → orders → wallet → agent_commands）。

// clusterUnfinishedStates 是「未结束成员」的实例状态。
var clusterUnfinishedStates = []string{StateCreating, StateRunning, StateStopped, StateWiping}

// clusterNetErrorPrefix：Agent 上报的 InstanceState.cluster 以它开头 = 成员集群网络异常。
const clusterNetErrorPrefix = "error:"

// AlertClusterFailed 用户集群开通期中止（成员失败 / 组建超时），全部成员已全额退款。
func AlertClusterFailed(clusterID string) string {
	return fmt.Sprintf("集群 %s 开通失败，已全额退款", clusterID)
}

// AlertClusterNetError 成员集群网络异常（集群网络导致的开通失败、configure_cluster 失败、巡检上报 error）。
func AlertClusterNetError(instanceID string) string {
	return fmt.Sprintf("实例 %s 的集群网络异常", instanceID)
}

func clusterNetAlert(machineID int64, instanceID string) fleet.Alert {
	return fleet.Alert{Level: "warning", Kind: "cluster_net_error", MachineID: machineID, InstanceID: instanceID,
		Message: AlertClusterNetError(instanceID)}
}

// lockUnfinishedMembers 按实例 ID 升序锁集群的未结束成员（在位 ∪ wiping）；调用方已持集群行锁。
func lockUnfinishedMembers(ctx context.Context, tx pgx.Tx, clusterID string) ([]Instance, error) {
	return collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
		WHERE cluster_id = $1 AND state = ANY($2) ORDER BY id FOR UPDATE`, clusterID, clusterUnfinishedStates))
}

func inPlace(insts []Instance) []Instance {
	return slices.DeleteFunc(slices.Clone(insts), func(i Instance) bool { return !slices.Contains(ClusterMemberStates, i.State) })
}

// byClusterRank 按 rank 排序（rank 相同按 ID）。
func byClusterRank(insts []Instance) []Instance {
	out := slices.Clone(insts)
	slices.SortFunc(out, func(a, b Instance) int {
		if d := rankOf(a) - rankOf(b); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func rankOf(i Instance) int {
	if i.ClusterRank == nil {
		return -1
	}
	return *i.ClusterRank
}

// memberNetStatus 在位成员在其机器最近一份 StateReport 里的集群网络状态（instance id → cluster 字段）。
func memberNetStatus(ctx context.Context, q db.Querier, clusterID string) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT i.id, `+clusterNetStatusSQL+` FROM instances i JOIN machines m ON m.id = i.machine_id
		WHERE i.cluster_id = $1 AND i.state = ANY($2)`, clusterID, ClusterMemberStates)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// clusterFormed：size 个 rank 都恰有一个在位成员，全员 running、未暂停，且没有成员上报集群网络 error（规格 §5.5 第 3 步）。
func clusterFormed(c Cluster, members []Instance, net map[string]string) bool {
	if len(members) != c.Size {
		return false
	}
	seen := make([]bool, c.Size)
	for _, m := range members {
		r := rankOf(m)
		if r < 0 || r >= c.Size || seen[r] || m.State != StateRunning || m.Suspended() || strings.HasPrefix(net[m.ID], clusterNetErrorPrefix) {
			return false
		}
		seen[r] = true
	}
	return true
}

// SweepClusters 清扫活动集群（规格 §5.5；Sweep 里排在 FailTimedOutCommands 之后）。每个集群一个独立事务，
// 锁序 集群行 → 未结束成员（ID 升序）：
//  1. 用户集群组建期（ready_at 为空）：有成员 failed / cancelled、在位成员已全部离开、或 created_at + ClusterFormTimeout 已到仍未就绪
//     → 其余在位成员洗机、全部成员（含换机前旧实例）的订单全额退款、集群 failed、告警 cluster_failed（一次）；机器不判故障。
//  2. 首次就绪（clusterFormed）→ ready_at = now（系统集群同样置）；用户集群此刻把全员到期对齐到最晚的一个。之后不再对齐。
//  3. 没有未结束成员 → ready_at 为空 failed、否则 released（子网 / VLAN 随之释放）。
//
// 返回状态有变化的集群数。
func (s *Service) SweepClusters(ctx context.Context) (int, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT id FROM instance_clusters WHERE state = 'active' ORDER BY created_at, id LIMIT $1`, sweepBatch)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		var changed bool
		err := withRetry(func() error {
			changed = false
			return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
				var err error
				changed, err = s.sweepCluster(ctx, tx, id, s.d.Clock.Now())
				return err
			})
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("cluster %s: %w", id, err))
			continue
		}
		if changed {
			n++
		}
	}
	return n, errors.Join(errs...)
}

func (s *Service) sweepCluster(ctx context.Context, tx pgx.Tx, id string, now time.Time) (bool, error) {
	c, err := lockCluster(ctx, tx, id)
	if err != nil || c.State != ClusterActive {
		return false, err
	}
	unfinished, err := lockUnfinishedMembers(ctx, tx, id)
	if err != nil {
		return false, err
	}
	members := inPlace(unfinished)
	net, err := memberNetStatus(ctx, tx, id)
	if err != nil {
		return false, err
	}
	formed := clusterFormed(c, members, net)
	if !c.System && c.ReadyAt == nil {
		var gone bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instances WHERE cluster_id = $1 AND state IN ('failed', 'cancelled'))`, id).Scan(&gone); err != nil {
			return false, err
		}
		reason := ""
		switch {
		case gone:
			reason = "member failed"
		case len(members) == 0:
			reason = "no members left"
		case formed:
		case !now.Before(c.CreatedAt.Add(ClusterFormTimeout)):
			reason = "formation timeout"
		}
		if reason != "" {
			return true, s.abortCluster(ctx, tx, c, members, reason, now)
		}
	}
	if c.ReadyAt == nil && formed {
		if _, err := tx.Exec(ctx, `UPDATE instance_clusters SET ready_at = $2, updated_at = $2 WHERE id = $1`, id, now); err != nil {
			return false, err
		}
		if !c.System {
			if err := s.alignCluster(ctx, tx, members, now); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if len(unfinished) == 0 {
		state := ClusterReleased
		if c.ReadyAt == nil {
			state = ClusterFailed
		}
		_, err := tx.Exec(ctx, `UPDATE instance_clusters SET state = $2, ended_at = $3, updated_at = $3 WHERE id = $1`, id, state, now)
		return err == nil, err
	}
	return false, nil
}

// abortCluster 用户集群开通期中止（规格 §5.5 第 1 步）：在位成员洗机（beginWipe 已取消其未完成指令）→ 全部成员订单全额退款
// → 集群 failed → 告警 cluster_failed（一次）。机器不判故障（失败成员已按自己的原因处理过）。
func (s *Service) abortCluster(ctx context.Context, tx pgx.Tx, c Cluster, members []Instance, reason string, now time.Time) error {
	for _, inst := range members {
		if err := s.beginWipe(ctx, tx, inst, "cluster aborted: "+reason, now); err != nil {
			return err
		}
	}
	if err := s.refundClusterAll(ctx, tx, c, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE instance_clusters SET state = 'failed', ended_at = $2, updated_at = $2 WHERE id = $1`, c.ID, now); err != nil {
		return err
	}
	return s.d.Fleet.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "warning", Kind: "cluster_failed", Message: AlertClusterFailed(c.ID)})
}

// refundClusterAll 退还集群全部实例（cluster_id 指向本集群的，含已失败成员与换机前的旧实例）全部订单尚未退还的部分。
// 订单行在成员实例行锁之后加锁（与单实例退款同序）。
func (s *Service) refundClusterAll(ctx context.Context, tx pgx.Tx, c Cluster, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id FROM instances WHERE cluster_id = $1 ORDER BY id`, c.ID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		orders, err := refundableOrders(ctx, tx, []string{id}, true)
		if err != nil {
			return err
		}
		var total int64
		for _, o := range orders {
			total += o.amount - o.refunded
		}
		if total <= 0 {
			continue
		}
		if _, err := s.refundAcross(ctx, tx, c.UserID, orders, total, 0, "集群开通失败退款 "+id, now); err != nil {
			return err
		}
	}
	return nil
}

// alignCluster 首次就绪时把全员 expires_at 拉到其中最晚的一个（平台让出开通先后差的几分钟，规格 M8）。
// 被同机下一条预订封顶时按封顶值（不补偿：对齐本身是赠送）。
func (s *Service) alignCluster(ctx context.Context, tx pgx.Tx, members []Instance, now time.Time) error {
	var latest time.Time
	for _, m := range members {
		if m.ExpiresAt != nil && m.ExpiresAt.After(latest) {
			latest = *m.ExpiresAt
		}
	}
	if latest.IsZero() {
		return nil
	}
	for _, m := range members {
		if m.ExpiresAt == nil || !m.ExpiresAt.Before(latest) {
			continue
		}
		r24, r1 := reminderFlags(latest.Sub(now))
		if _, err := tx.Exec(ctx, `UPDATE instances SET expires_at = $2, reminded_24h = $3, reminded_1h = $4, updated_at = $5
			WHERE id = $1`, m.ID, latest, r24, r1, now); err != nil {
			return err
		}
		if err := s.refreshLease(ctx, tx, m.ID, now); err != nil {
			return err
		}
		if _, err := capExpiryToLease(ctx, tx, m.ID, latest, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, m.ID, m.State, m.State, "cluster aligned to "+tz.Format(latest), now); err != nil {
			return err
		}
	}
	return nil
}

// lockUserCluster 锁用户自己的活动集群与其在位成员（按 ID 升序）。别人的 / 系统集群 → ErrClusterNotFound；已结束 → ErrInvalidState。
func lockUserCluster(ctx context.Context, tx pgx.Tx, userID int64, clusterID string) (Cluster, []Instance, error) {
	c, err := lockCluster(ctx, tx, clusterID)
	if err != nil {
		return Cluster{}, nil, err
	}
	if c.UserID != userID || c.System {
		return Cluster{}, nil, apperr.ErrClusterNotFound
	}
	if c.State != ClusterActive {
		return Cluster{}, nil, apperr.ErrInvalidState
	}
	members, err := lockClusterMembers(ctx, tx, clusterID)
	if err != nil {
		return Cluster{}, nil, err
	}
	if len(members) == 0 {
		return Cluster{}, nil, apperr.ErrInvalidState
	}
	return c, members, nil
}

// clusterRenewKey 是集群续费第 rank 个成员子订单的幂等键（key 为空 = 不幂等）。
func clusterRenewKey(key string, rank int) string {
	if key == "" {
		return ""
	}
	return fmt.Sprintf("%s#%d", key, rank)
}

// findClusterRenewOrders 返回用户以 key# 为前缀的子订单（按子键序号）。
func findClusterRenewOrders(ctx context.Context, q db.Querier, userID int64, key string) ([]Order, error) {
	prefix := key + "#"
	rows, err := q.Query(ctx, `SELECT `+orderCols+` FROM orders
		WHERE user_id = $1 AND left(idempotency_key, length($2)) = $2
		ORDER BY substring(idempotency_key FROM length($2) + 1), id`, userID, prefix)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
}

// replayClusterRenew：子订单必须都是本集群成员（或其换机后继无关：续费单只挂在续费时的成员上）的续费单，否则幂等键冲突。
func replayClusterRenew(ctx context.Context, q db.Querier, prev []Order, clusterID string) ([]Order, error) {
	ids := make([]string, 0, len(prev))
	for _, o := range prev {
		if o.Kind != "renew" {
			return nil, errIdempotencyConflict
		}
		ids = append(ids, o.InstanceID)
	}
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM instances WHERE id = ANY($1) AND cluster_id = $2`, ids, clusterID).Scan(&n); err != nil {
		return nil, err
	}
	if n != len(slices.Compact(slices.Sorted(slices.Values(ids)))) {
		return nil, errIdempotencyConflict
	}
	return prev, nil
}

// RenewCluster 整组续费（规格 §5.7）：幂等咨询锁 → 锁集群 → 在位成员（ID 升序）→ 全员 renewFits → 按 rank 逐个 charge（子键 key#<rank>）+ addTime。
// 每个成员加同样的时长（保持原有差距）；任何一个失败整笔回滚（余额不足报 insufficient_balance，带本次总价）。
// 重放：命中 key#* 的续费单 → 原样返回。返回的订单按 rank 排列。
func (s *Service) RenewCluster(ctx context.Context, userID int64, clusterID string, d catalog.Duration, quantity int, idempotencyKey string) ([]Order, error) {
	if !d.Valid() {
		return nil, apperr.BadRequest("租期不正确")
	}
	if quantity == 0 {
		quantity = 1
	}
	if quantity < 1 {
		return nil, apperr.BadRequest("数量不正确")
	}
	if quantity > MaxQuantity(d) {
		return nil, apperr.ErrBookingTooLong
	}
	if err := checkIdempotencyKey(idempotencyKey); err != nil {
		return nil, err
	}
	now := s.d.Clock.Now()
	var out []Order
	err := withRetry(func() error {
		return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			out = nil
			if idempotencyKey != "" {
				if err := lockIdempotency(ctx, tx, userID, idempotencyKey); err != nil {
					return err
				}
				if _, found, err := findBatchByKey(ctx, tx, userID, idempotencyKey); err != nil {
					return err
				} else if found {
					return errIdempotencyConflict
				}
				if _, found, err := findByIdempotencyKey(ctx, tx, userID, idempotencyKey); err != nil {
					return err
				} else if found {
					return errIdempotencyConflict
				}
				prev, err := findClusterRenewOrders(ctx, tx, userID, idempotencyKey)
				if err != nil {
					return err
				}
				if len(prev) > 0 {
					out, err = replayClusterRenew(ctx, tx, prev, clusterID)
					return err
				}
			}
			_, members, err := lockUserCluster(ctx, tx, userID, clusterID)
			if err != nil {
				return err
			}
			secs := seconds(d) * int64(quantity)
			var total int64
			for _, m := range members {
				if m.State != StateCreating && m.State != StateRunning && m.State != StateStopped {
					return apperr.ErrInvalidState
				}
				if err := s.renewFits(ctx, tx, m, secs, now); err != nil {
					return err
				}
				unit, err := s.d.Catalog.Quote(ctx, tx, m.ProductID, d)
				if err != nil {
					return err
				}
				total += unit * int64(quantity)
			}
			for _, m := range byClusterRank(members) {
				ord, err := s.applyRenew(ctx, tx, m, d, quantity, false, clusterRenewKey(idempotencyKey, rankOf(m)), now,
					func(o Order) string { return "cluster renew " + o.ID })
				if codeIs(err, "insufficient_balance") {
					return apperr.New("insufficient_balance", fmt.Sprintf("余额不足：集群续费共需 %d.%02d 元，请先充值", total/100, total%100), 402)
				}
				if err != nil {
					return err
				}
				out = append(out, ord)
			}
			return nil
		})
	})
	if idempotencyKey != "" && isIdempotencyViolation(err) {
		prev, ferr := findClusterRenewOrders(ctx, s.d.Pool, userID, idempotencyKey)
		if ferr != nil {
			return nil, ferr
		}
		if len(prev) > 0 {
			return replayClusterRenew(ctx, s.d.Pool, prev, clusterID)
		}
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetClusterAutoRenew 全员同设自动续费开关与档位（清掉上次的失败标记），审计 cluster.auto_renew。
func (s *Service) SetClusterAutoRenew(ctx context.Context, userID int64, clusterID string, enabled bool, d catalog.Duration, ip string) (ClusterView, error) {
	if !d.Valid() && (enabled || d != "") {
		return ClusterView{}, apperr.BadRequest("自动续费档位不正确")
	}
	now := s.d.Clock.Now()
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		_, members, err := lockUserCluster(ctx, tx, userID, clusterID)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.ID)
		}
		if _, err := tx.Exec(ctx, `UPDATE instances SET auto_renew = $2, auto_renew_duration = COALESCE(NULLIF($3, ''), auto_renew_duration),
			auto_renew_failed_at = NULL, auto_renew_failed_reason = '', updated_at = $4 WHERE id = ANY($1)`, ids, enabled, string(d), now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("cluster.auto_renew", "cluster", clusterID,
			map[string]any{"enabled": enabled, "duration": d, "instances": ids}))
	})
	if err != nil {
		return ClusterView{}, err
	}
	return s.GetClusterView(ctx, clusterID)
}

// ReleaseCluster 用户整组释放：全员走 Release 的同一链路（立即洗机，已付费用不退），审计 cluster.release。
// 与单实例 Release 一致，只能释放 running / stopped 的成员：有成员仍在开通 → ErrInvalidState。
func (s *Service) ReleaseCluster(ctx context.Context, userID int64, clusterID, ip string) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		_, members, err := lockUserCluster(ctx, tx, userID, clusterID)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(members))
		for _, m := range members {
			if m.State != StateRunning && m.State != StateStopped {
				return apperr.ErrInvalidState
			}
			ids = append(ids, m.ID)
		}
		for _, m := range members {
			if err := s.beginWipe(ctx, tx, m, "user released cluster "+clusterID, now); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("cluster.release", "cluster", clusterID,
			map[string]any{"instances": ids}))
	})
}

// AdminExtendCluster 后台免费延长整组：全员 renewFits 通过后逐个 addTime（同 AdminExtend），审计 cluster.extend。系统集群 → ErrInvalidState。
func (s *Service) AdminExtendCluster(ctx context.Context, actor audit.Actor, clusterID string, secs int64, reason string) (ClusterView, error) {
	reason = strings.TrimSpace(reason)
	if secs < MinExtendSeconds || secs > MaxExtendSeconds {
		return ClusterView{}, apperr.BadRequest("补偿时长需在 1 分钟到 30 天之间")
	}
	if reason == "" {
		return ClusterView{}, apperr.BadRequest("请填写补偿原因")
	}
	now := s.d.Clock.Now()
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		c, err := lockCluster(ctx, tx, clusterID)
		if err != nil {
			return err
		}
		if c.System || c.State != ClusterActive {
			return apperr.ErrInvalidState
		}
		members, err := lockClusterMembers(ctx, tx, clusterID)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return apperr.ErrInvalidState
		}
		for _, m := range members {
			if err := s.renewFits(ctx, tx, m, secs, now); err != nil {
				return err
			}
		}
		ids := make([]string, 0, len(members))
		for _, m := range members {
			if err := s.addTime(ctx, tx, m, secs, now); err != nil {
				return err
			}
			if err := clearAutoRenewFailure(ctx, tx, m, now); err != nil {
				return err
			}
			if err := recordEvent(ctx, tx, m.ID, m.State, m.State, fmt.Sprintf("admin extend cluster +%ds: %s", secs, reason), now); err != nil {
				return err
			}
			if err := s.refreshLease(ctx, tx, m.ID, now); err != nil {
				return err
			}
			if err := s.verifyLeaseFits(ctx, tx, m.ID, now); err != nil {
				return err
			}
			ids = append(ids, m.ID)
		}
		return audit.Record(ctx, tx, now, actor.Entry("cluster.extend", "cluster", clusterID,
			map[string]any{"seconds": secs, "reason": reason, "instances": ids}))
	})
	if err != nil {
		return ClusterView{}, err
	}
	return s.GetClusterView(ctx, clusterID)
}

// AdminResyncCluster 排障：version + 1 并向全部在位成员下发 configure_cluster（新 Spec），审计 cluster.resync。
func (s *Service) AdminResyncCluster(ctx context.Context, actor audit.Actor, clusterID string) (ClusterView, error) {
	now := s.d.Clock.Now()
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		c, err := lockCluster(ctx, tx, clusterID)
		if err != nil {
			return err
		}
		if c.State != ClusterActive {
			return apperr.ErrInvalidState
		}
		if _, err := lockClusterMembers(ctx, tx, clusterID); err != nil {
			return err
		}
		version, n, err := s.bumpClusterVersion(ctx, tx, clusterID, "", now)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("cluster.resync", "cluster", clusterID,
			map[string]any{"version": version, "members": n}))
	})
	if err != nil {
		return ClusterView{}, err
	}
	return s.GetClusterView(ctx, clusterID)
}

// bumpClusterVersion 成员变更后：集群 version + 1，并向除 skipInstanceID 外的每个在位成员下发 configure_cluster（ClusterSpecFor 新 Spec）。
// 调用方已持集群行锁（及需要的成员行锁）。某成员 Spec 构造失败 → 跳过它并告警 cluster_net_error。返回新版本与下发条数。
func (s *Service) bumpClusterVersion(ctx context.Context, tx pgx.Tx, clusterID, skipInstanceID string, now time.Time) (int, int, error) {
	var version int
	if err := tx.QueryRow(ctx, `UPDATE instance_clusters SET version = version + 1, updated_at = $2 WHERE id = $1 RETURNING version`,
		clusterID, now).Scan(&version); err != nil {
		return 0, 0, err
	}
	rows, err := tx.Query(ctx, `SELECT id, machine_id, cluster_rank FROM instances WHERE cluster_id = $1 AND state = ANY($2) ORDER BY cluster_rank, id`,
		clusterID, ClusterMemberStates)
	if err != nil {
		return 0, 0, err
	}
	type member struct {
		id        string
		machineID int64
		rank      int
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (member, error) {
		var m member
		err := r.Scan(&m.id, &m.machineID, &m.rank)
		return m, err
	})
	if err != nil {
		return 0, 0, err
	}
	n := 0
	for _, m := range list {
		if m.id == skipInstanceID {
			continue
		}
		spec, err := s.ClusterSpecFor(ctx, tx, clusterID, m.rank)
		if err != nil {
			// 构造不出该成员的 Spec（网卡信息缺失等）：不下发坏 Spec、不阻断后台操作，告警由运维处理后 resync
			s.d.Log.Warn("cluster spec", "cluster", clusterID, "rank", m.rank, "err", err)
			if err := s.d.Fleet.RaiseAlertOnce(ctx, tx, clusterNetAlert(m.machineID, m.id)); err != nil {
				return 0, 0, err
			}
			continue
		}
		if _, err := s.d.Fleet.Enqueue(ctx, tx, m.machineID, m.id, fleet.CmdConfigureCluster, fleet.ConfigureClusterPayload{Cluster: spec}); err != nil {
			return 0, 0, err
		}
		n++
	}
	return version, n, nil
}

// clusterAutoRenewCandSQL：自动续费候选集群（规格 §5.7）——用户集群、active、已就绪、在位成员全是 running 且都开了自动续费、
// 未暂停成员的最早到期 ≤ now + AutoRenewLead（全员暂停则不选）、每个成员的失败退避都已过（同 autoRenewPred）。
var clusterAutoRenewCandSQL = fmt.Sprintf(`SELECT c.id FROM instance_clusters c JOIN instances i ON i.cluster_id = c.id AND i.state IN ('creating', 'running', 'stopped')
	WHERE c.state = 'active' AND NOT c.system AND c.ready_at IS NOT NULL
	GROUP BY c.id, c.created_at
	HAVING bool_and(i.state = 'running') AND bool_and(i.auto_renew)
	   AND min(i.expires_at) FILTER (WHERE i.suspended_at IS NULL) <= $1::timestamptz + interval '%d seconds'
	   AND bool_and(i.auto_renew_failed_at IS NULL OR i.auto_renew_failed_at <= $1::timestamptz
	       - CASE WHEN i.auto_renew_failed_reason = 'renew_blocked' THEN interval '%d seconds' ELSE interval '%d seconds' END)
	ORDER BY min(i.expires_at) FILTER (WHERE i.suspended_at IS NULL), c.id LIMIT $2`,
	int(AutoRenewLead/time.Second), int(AutoRenewRetryBlocked/time.Second), int(AutoRenewRetry/time.Second))

// clusterAutoRenewDue 在锁内复核候选条件。
func clusterAutoRenewDue(members []Instance, now time.Time) bool {
	if len(members) == 0 {
		return false
	}
	var earliest *time.Time
	for _, m := range members {
		if m.State != StateRunning || !m.AutoRenew {
			return false
		}
		if m.AutoRenewFailedAt != nil {
			retry := AutoRenewRetry
			if m.AutoRenewFailedReason == "renew_blocked" {
				retry = AutoRenewRetryBlocked
			}
			if m.AutoRenewFailedAt.After(now.Add(-retry)) {
				return false
			}
		}
		if !m.Suspended() && m.ExpiresAt != nil && (earliest == nil || m.ExpiresAt.Before(*earliest)) {
			earliest = m.ExpiresAt
		}
	}
	return earliest != nil && !earliest.After(now.Add(AutoRenewLead))
}

// AutoRenewClustersDue 集群自动续费（规格 §5.7；Sweep 里紧跟 AutoRenewDue）：每个集群一个事务，锁集群 → 成员；
// 档位从 rank 最小成员的档位起降档，取全员都放得下的最大档；savepoint 里全员 charge(auto) + addTime（暂停成员加到冻结的剩余时长上）。
// 任一失败 → 回滚 savepoint，全员写失败标记；首次失败只对 rank 最小的成员写事件、发一条短信（提交后发）。返回续费成功的集群数。
func (s *Service) AutoRenewClustersDue(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	rows, err := s.d.Pool.Query(ctx, clusterAutoRenewCandSQL, now, sweepBatch)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		var after func()
		var renewed bool
		err := withRetry(func() error {
			after, renewed = nil, false
			return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
				var err error
				renewed, after, err = s.autoRenewCluster(ctx, tx, id, now)
				return err
			})
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("cluster %s: %w", id, err))
			continue
		}
		if after != nil {
			after()
		}
		if renewed {
			n++
		}
	}
	return n, errors.Join(errs...)
}

func (s *Service) autoRenewCluster(ctx context.Context, tx pgx.Tx, id string, now time.Time) (bool, func(), error) {
	c, err := lockCluster(ctx, tx, id)
	if err != nil || c.State != ClusterActive || c.System || c.ReadyAt == nil {
		return false, nil, err
	}
	members, err := lockClusterMembers(ctx, tx, id)
	if err != nil || !clusterAutoRenewDue(members, now) {
		return false, nil, err
	}
	ranked := byClusterRank(members)
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, nil, err
	}
	failReason := ""
	d, err := s.clusterAutoRenewDuration(ctx, sp, ranked, now)
	if err == nil && d == "" {
		failReason = "renew_blocked"
	} else if err == nil {
		for _, m := range ranked {
			if _, err = s.applyRenew(ctx, sp, m, d, 1, true, "", now,
				func(o Order) string { return fmt.Sprintf("cluster auto renew %s (%s)", o.ID, d) }); err != nil {
				break
			}
		}
	}
	switch {
	case errors.Is(err, apperr.ErrInsufficientBalance) || codeIs(err, "insufficient_balance"):
		failReason = "insufficient_balance"
	case codeIs(err, "renew_blocked"):
		failReason = "renew_blocked"
	case err != nil:
		return false, nil, errors.Join(err, sp.Rollback(ctx))
	}
	if failReason == "" {
		return true, nil, sp.Commit(ctx)
	}
	if err := sp.Rollback(ctx); err != nil {
		return false, nil, err
	}
	first := ranked[0]
	for _, m := range ranked[1:] { // 其余成员只写标记（不写事件、不发短信）
		if _, err := tx.Exec(ctx, `UPDATE instances SET auto_renew_failed_at = $2, auto_renew_failed_reason = $3, updated_at = $2 WHERE id = $1`,
			m.ID, now, failReason); err != nil {
			return false, nil, err
		}
	}
	// 「首次失败」按 rank 最小的成员判断（全员标记同时写、同时清）
	after, err := s.markAutoRenewFailed(ctx, tx, first, failReason, now)
	return false, after, err
}

// clusterAutoRenewDuration 从 rank 最小成员的档位起按 1m → 1w → 1d 降档，取全员都放得下的第一个；都放不下返回空串。
func (s *Service) clusterAutoRenewDuration(ctx context.Context, q db.Querier, ranked []Instance, now time.Time) (catalog.Duration, error) {
tiers:
	for _, d := range autoRenewTiers(ranked[0].AutoRenewDuration) {
		for _, m := range ranked {
			err := s.renewFits(ctx, q, m, seconds(d), now)
			if codeIs(err, "renew_blocked") {
				continue tiers
			}
			if err != nil {
				return "", err
			}
		}
		return d, nil
	}
	return "", nil
}

// peekClusterID 不加锁读实例的 cluster_id（多成员路径先据此锁集群行，再锁实例并复核）。实例不存在 → ErrNotFound。
func peekClusterID(ctx context.Context, q db.Querier, instanceID string) (*string, error) {
	var cid *string
	err := q.QueryRow(ctx, `SELECT cluster_id FROM instances WHERE id = $1`, instanceID).Scan(&cid)
	if db.IsNoRows(err) {
		return nil, apperr.ErrNotFound
	}
	return cid, err
}

// lockMemberCluster 成员的后台操作（换机 / 退款释放）：先锁集群行，再锁实例并复核 cluster_id（规格 §14）。
// 非成员返回 nil 集群与已锁的实例（与原单实例路径相同）。
func lockMemberCluster(ctx context.Context, tx pgx.Tx, instanceID string) (*Cluster, Instance, error) {
	cid, err := peekClusterID(ctx, tx, instanceID)
	if err != nil {
		return nil, Instance{}, err
	}
	var c *Cluster
	if cid != nil {
		cl, err := lockCluster(ctx, tx, *cid)
		if err != nil {
			return nil, Instance{}, err
		}
		c = &cl
	}
	inst, err := lockInstance(ctx, tx, instanceID)
	if err != nil {
		return nil, Instance{}, err
	}
	if (inst.ClusterID == nil) != (c == nil) || (c != nil && *inst.ClusterID != c.ID) {
		return nil, Instance{}, fmt.Errorf("instance %s cluster changed while locking", instanceID)
	}
	return c, inst, nil
}
