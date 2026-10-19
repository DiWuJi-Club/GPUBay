package rental

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// pickSpec 描述一次分配的候选集（规格 §4.2）。
type pickSpec struct {
	ProductID int64
	GroupID   *int64 // nil = 不限机组：未分组机器 + 可见机组
	MachineID *int64 // 指定机器（仍受机组条件约束）
	Lower     time.Time
	Upper     time.Time
	Immediate bool // 即时分支：要求 idle AND agent_online；预订分支允许 allocated 与离线
	Exclude   []int64
}

// pickWhereSQL：候选 = 商品匹配 ∩ 机组条件 ∩ 状态允许 ∩ 与 reserved / active 时段无重叠。
const pickWhereSQL = `SELECT m.id FROM machines m
	LEFT JOIN machine_groups g ON g.id = m.group_id
	WHERE m.product_id = $1
	  AND (($2::bigint IS NULL AND (m.group_id IS NULL OR g.visible)) OR m.group_id = $2::bigint)
	  AND ($3::bigint IS NULL OR m.id = $3::bigint)
	  AND NOT (m.id = ANY($6::bigint[]))
	  AND (CASE WHEN $7::boolean THEN m.state = 'idle' AND m.agent_online ELSE m.state IN ('idle', 'allocated') END)
	  AND NOT EXISTS (SELECT 1 FROM machine_leases l
	                  WHERE l.machine_id = m.id AND l.status IN ('reserved', 'active') AND l.period && tstzrange($4::timestamptz, $5::timestamptz))`

// pickOrderSQL：「最紧贴前一档期的机器优先」（减少碎片），再按 id。计数不需要排序，countAvailable 不带它。
const pickOrderSQL = `
	ORDER BY (SELECT max(upper(l.period)) FROM machine_leases l
	          WHERE l.machine_id = m.id AND l.status IN ('reserved', 'active', 'ended') AND upper(l.period) <= $4::timestamptz) DESC NULLS LAST,
	         m.id`

func pickArgs(spec pickSpec) []any {
	exclude := spec.Exclude
	if exclude == nil {
		exclude = []int64{}
	}
	return []any{spec.ProductID, spec.GroupID, spec.MachineID, spec.Lower, spec.Upper, exclude, spec.Immediate}
}

// pickMachines 返回最多 limit 台候选机器；lock 为 true 时锁行（SKIP LOCKED：并发分配互不等待；跳过的机器可能其实空闲，分配走 pickLocked 重挑）。
func pickMachines(ctx context.Context, q db.Querier, spec pickSpec, limit int, lock bool) ([]int64, error) {
	sql := pickWhereSQL + pickOrderSQL + ` LIMIT $8`
	if lock {
		sql += ` FOR NO KEY UPDATE OF m SKIP LOCKED` // 不用 FOR UPDATE：与外键插入的 KEY SHARE 互斥
	}
	rows, err := q.Query(ctx, sql, append(pickArgs(spec), limit)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// countAvailable 返回候选台数（只读，供可用性查询与错误消息）。
func countAvailable(ctx context.Context, q db.Querier, spec pickSpec) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM (`+pickWhereSQL+`) x`, pickArgs(spec)...).Scan(&n)
	return n, err
}

// noCapacity：即时分支保持 ErrNoCapacity（消息与错误值不变，前端引导与现有测试依赖它）；预订分支带台数。
func noCapacity(spec pickSpec, got int) error {
	if spec.Immediate {
		return apperr.ErrNoCapacity
	}
	if got == 0 {
		return apperr.New("no_capacity", "该时段暂无可用机器", 409)
	}
	return apperr.New("no_capacity", fmt.Sprintf("该时段只有 %d 台可用，请减少数量或更换时段", got), 409)
}

// placeOnMachines 挑出 want 台机器，对每台在 savepoint 内执行 fn（插实例 + lease [+ 标记 allocated]）：
// 排他约束冲突（并发事务刚提交了重叠时段，预筛的快照没看到）→ 回滚 savepoint、剔除这台、补取；最多补取 3 轮。
// 机器行锁（NO KEY UPDATE，SKIP LOCKED）在挑选用的 savepoint 里取、挑选结束后 RELEASE 归外层事务，fn 的 savepoint 回滚不释放它，并发批次不会互相等待。
// 挑到的台数不足时先排除「被别的事务短暂锁住而跳过」的情况（见 pickLocked），再判定不足。
// 不足 want 台 → 返回 no_capacity，调用方回滚整个事务（没有部分成功）。
func placeOnMachines(ctx context.Context, tx pgx.Tx, spec pickSpec, want int, fn func(sp pgx.Tx, machineID int64) error) ([]int64, error) {
	var done []int64
	tried := append([]int64(nil), spec.Exclude...)
	lockRetries := maxLockRetries
	for round := 0; round <= 3 && len(done) < want; round++ {
		s := spec
		s.Exclude = tried
		ids, err := pickLocked(ctx, tx, s, want-len(done), &lockRetries)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			break
		}
		for _, mid := range ids {
			tried = append(tried, mid)
			sp, err := tx.Begin(ctx)
			if err != nil {
				return nil, err
			}
			if err := fn(sp, mid); err != nil {
				_ = sp.Rollback(ctx)
				if db.IsExclusionViolation(err) {
					continue
				}
				return nil, err
			}
			if err := sp.Commit(ctx); err != nil {
				return nil, err
			}
			done = append(done, mid)
		}
	}
	if len(done) < want {
		return nil, noCapacity(spec, len(done))
	}
	return done, nil
}

// maxLockRetries：一次分配里因「空闲机器被别的事务短暂锁住」而重挑的总次数上限。
const maxLockRetries = 3

// lockRetryBackoff 重挑前的退避：20～80ms 随机抖动，让争同一批机器的事务错开。
func lockRetryBackoff() time.Duration {
	return 20*time.Millisecond + rand.N(61*time.Millisecond)
}

// pickLocked 加锁挑最多 need 台（pickMachines，锁序与 SKIP LOCKED 不变），挑选在一个 savepoint 里做。
// SKIP LOCKED 会跳过别的事务只是短暂锁住、该时段其实空闲的机器：挑到不足 need 台时，用同一 pickWhereSQL 不加锁计数，
// 计数够 need 台 → 回滚这个 savepoint（放掉刚加的行锁：两个多台批次各攥一部分、互相等到重试用完就都失败了）、
// 退避后重挑，消耗 *retries；计数也不够或 *retries 用完 → 释放 savepoint（行锁归外层事务）并返回已挑到的，由调用方判定不足。
func pickLocked(ctx context.Context, tx pgx.Tx, spec pickSpec, need int, retries *int) ([]int64, error) {
	for {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return nil, err
		}
		ids, err := pickMachines(ctx, sp, spec, need, true)
		if err != nil {
			_ = sp.Rollback(ctx)
			return nil, err
		}
		if len(ids) < need && *retries > 0 {
			n, err := countAvailable(ctx, sp, spec)
			if err != nil {
				_ = sp.Rollback(ctx)
				return nil, err
			}
			if n >= need {
				*retries--
				if err := sp.Rollback(ctx); err != nil {
					return nil, err
				}
				if lockRetryHook != nil {
					lockRetryHook()
				}
				// 退避在外层事务里睡，调用方已持有的锁不放：下单 / 预订路径持有每用户咨询锁（idem:、booking:），
				// 开通与改派路径持有实例行锁与 lease 行锁（及旧机器行锁）。总时长有界（maxLockRetries × ≤80ms ≈ ≤240ms），可接受。
				if err := sleepCtx(ctx, lockRetryBackoff()); err != nil {
					return nil, err
				}
				continue
			}
		}
		if err := sp.Commit(ctx); err != nil {
			return nil, err
		}
		return ids, nil
	}
}

// lockRetryHook 测试钩子：pickLocked 决定重挑、放掉本次行锁之后、退避之前调用；生产为 nil。
var lockRetryHook func()

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// immediatePeriod 即时订单的占用时段：开通估算 + 租期 + 尾段（规格 §4.1）。
func immediatePeriod(now time.Time, secs int64) (time.Time, time.Time) {
	return now, now.Add(CreateTimeout + time.Duration(secs)*time.Second + Tail)
}

// bookingPeriod 预订的占用时段：开始前 1 小时开通，到期后 7 小时尾段。
func bookingPeriod(start time.Time, secs int64) (time.Time, time.Time) {
	return start.Add(-PrepLead), start.Add(time.Duration(secs)*time.Second + Tail)
}

// AvailableNow 此刻可以立即租用 1 天的机器数（市场页展示；按最短租期探测，和下单时的校验一致）。
func (s *Service) AvailableNow(ctx context.Context, productID int64) (int, error) {
	spec := pickSpec{ProductID: productID, Immediate: true}
	spec.Lower, spec.Upper = immediatePeriod(s.d.Clock.Now(), 86400)
	return countAvailable(ctx, s.d.Pool, spec)
}
