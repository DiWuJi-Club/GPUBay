package rental

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/tz"
)

// Renew 续费一个单位；见 RenewQuantity。
func (s *Service) Renew(ctx context.Context, userID int64, instanceID string, d catalog.Duration, idempotencyKey string) (Order, error) {
	return s.RenewQuantity(ctx, userID, instanceID, d, 1, idempotencyKey)
}

// RenewQuantity 续费 quantity 个单位（quantity 0 视为 1，受 90 天上限）：扣款建单后按实例当前状态延长时长。
// wiping/released/failed/cancelled 状态不允许续费且不扣款。idempotencyKey 非空时同一用户重复提交同一键只扣款一次。
func (s *Service) RenewQuantity(ctx context.Context, userID int64, instanceID string, d catalog.Duration, quantity int, idempotencyKey string) (Order, error) {
	if !d.Valid() {
		return Order{}, apperr.BadRequest("租期不正确")
	}
	if quantity == 0 {
		quantity = 1
	}
	if quantity < 1 {
		return Order{}, apperr.BadRequest("数量不正确")
	}
	if quantity > MaxQuantity(d) {
		return Order{}, apperr.ErrBookingTooLong
	}
	if err := checkIdempotencyKey(idempotencyKey); err != nil {
		return Order{}, err
	}
	now := s.d.Clock.Now()
	var ord Order
	// 同一用户并发下批次（先锁钱包再锁机器）与续费可能死锁（40P01）：整个事务重试一次
	err := withRetry(func() error {
		return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			ord = Order{}
			if idempotencyKey != "" {
				prev, found, err := lockIdempotencyKey(ctx, tx, userID, idempotencyKey)
				if err != nil {
					return err
				}
				if found {
					ord, err = replayRenew(prev, instanceID)
					return err
				}
			}
			inst, err := lockInstance(ctx, tx, instanceID)
			if err != nil {
				return err
			}
			if inst.UserID != userID {
				return apperr.ErrNotFound
			}
			if inst.System || (inst.State != StateReserved && inst.State != StateCreating && inst.State != StateRunning && inst.State != StateStopped) {
				return apperr.ErrInvalidState
			}
			ord, err = s.applyRenew(ctx, tx, inst, d, quantity, false, idempotencyKey, now,
				func(o Order) string { return "renew " + o.ID })
			return err
		})
	})
	if isIdempotencyViolation(err) {
		// 并发的重复请求已先提交：本事务已回滚，返回先提交的那笔订单。
		prev, found, ferr := findByIdempotencyKey(ctx, s.d.Pool, userID, idempotencyKey)
		if ferr != nil {
			return Order{}, ferr
		}
		if found {
			return replayRenew(prev, instanceID)
		}
	}
	if err != nil {
		return Order{}, err
	}
	return ord, nil
}

// applyRenew 续费的共用核心（手动续费与自动续费都走这里）：renewFits → 扣款建单 → addTime → 清自动续费失败标记 → 事件 → refreshLease → verifyLeaseFits。
// 调用方已锁住实例行并校验过状态；任一步失败调用方须回滚整个事务（或 savepoint），不会出现扣了款却没加时。
func (s *Service) applyRenew(ctx context.Context, tx pgx.Tx, inst Instance, d catalog.Duration, quantity int, auto bool,
	idempotencyKey string, now time.Time, event func(Order) string) (Order, error) {
	secs := seconds(d) * int64(quantity)
	if err := s.renewFits(ctx, tx, inst, secs, now); err != nil {
		return Order{}, err
	}
	if renewHook != nil {
		renewHook()
	}
	ord, err := s.charge(ctx, tx, inst.UserID, "renew", inst.ID, inst.ProductID, d, quantity, auto, nil, idempotencyKey, now)
	if err != nil {
		return Order{}, err
	}
	if err := s.addTime(ctx, tx, inst, secs, now); err != nil {
		return Order{}, err
	}
	if err := clearAutoRenewFailure(ctx, tx, inst, now); err != nil {
		return Order{}, err
	}
	if err := recordEvent(ctx, tx, inst.ID, inst.State, inst.State, event(ord), now); err != nil {
		return Order{}, err
	}
	if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
		return Order{}, err
	}
	if err := s.verifyLeaseFits(ctx, tx, inst.ID, now); err != nil {
		return Order{}, err
	}
	return ord, nil
}

// replayRenew 返回幂等键对应的已有续费订单；键被新购订单或其他实例的续费占用时报冲突。
func replayRenew(prev Order, instanceID string) (Order, error) {
	if prev.Kind != "renew" || prev.InstanceID != instanceID {
		return Order{}, errIdempotencyConflict
	}
	return prev, nil
}

// Release 用户主动释放：立即进入洗机，已付费用不退。系统实例只能由 ReleaseSystem 释放。
func (s *Service) Release(ctx context.Context, userID int64, instanceID string) error {
	return s.release(ctx, instanceID, "user released", func(inst Instance) error {
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.System || (inst.State != StateRunning && inst.State != StateStopped) {
			return apperr.ErrInvalidState
		}
		return nil
	})
}

// addTime 按实例当前状态增加可用时长（续费与后台免费延长共用）：
// 运行中延长到期时间（已到期未清扫的从当前时刻起算；暂停中加到冻结的剩余时长）并重算提醒标记；
// 开通中或停机时计入待开始时长，停机且没有未完成的启动指令时下发启动；预订中计入待开始时长并推后 reserved 时段的上界。
func (s *Service) addTime(ctx context.Context, tx pgx.Tx, inst Instance, secs int64, now time.Time) error {
	var err error
	switch {
	case inst.State == StateReserved || (inst.Booked() && inst.State == StateCreating):
		// 预订（含已开通、尚未 running）的占用时段在下单时定死、refreshLease 不动：按新时长直接推后上界
		if _, err = tx.Exec(ctx, `UPDATE instances SET pending_seconds = pending_seconds + $2, updated_at = $3 WHERE id = $1`, inst.ID, secs, now); err != nil {
			return err
		}
		tag, uerr := tx.Exec(ctx, `UPDATE machine_leases SET period = tstzrange(lower(period), upper(period) + ($2::bigint * interval '1 second')), updated_at = $3
			WHERE id = (SELECT id FROM machine_leases WHERE instance_id = $1 AND status IN ('reserved', 'active') ORDER BY id DESC LIMIT 1)`, inst.ID, secs, now)
		if db.IsExclusionViolation(uerr) { // renewFits 之后同机紧随的预订才提交
			return errRenewBlockedGeneric
		}
		if uerr != nil {
			return uerr
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("addTime %s: booked lease not found", inst.ID)
		}
		return nil
	case inst.State == StateCreating:
		_, err = tx.Exec(ctx, `UPDATE instances SET pending_seconds = pending_seconds + $2, updated_at = $3 WHERE id = $1`, inst.ID, secs, now)
	case inst.State == StateRunning && inst.Suspended():
		var paused int64
		if inst.PausedRemaining != nil {
			paused = *inst.PausedRemaining
		}
		r24, r1 := reminderFlags(time.Duration(paused+secs) * time.Second)
		_, err = tx.Exec(ctx, `UPDATE instances SET paused_remaining = $2, reminded_24h = $3, reminded_1h = $4, updated_at = $5 WHERE id = $1`,
			inst.ID, paused+secs, r24, r1, now)
	case inst.State == StateRunning:
		if inst.ExpiresAt == nil { // 系统实例（expires_at NULL）：调用方本已拒绝，防御空指针
			return apperr.ErrInvalidState
		}
		base := *inst.ExpiresAt
		if base.Before(now) {
			base = now
		}
		expires := base.Add(time.Duration(secs) * time.Second)
		r24, r1 := reminderFlags(expires.Sub(now))
		_, err = tx.Exec(ctx, `UPDATE instances SET expires_at = $2, reminded_24h = $3, reminded_1h = $4, updated_at = $5 WHERE id = $1`,
			inst.ID, expires, r24, r1, now)
	case inst.State == StateStopped:
		if _, err = tx.Exec(ctx, `UPDATE instances SET pending_seconds = pending_seconds + $2, updated_at = $3 WHERE id = $1`, inst.ID, secs, now); err != nil {
			return err
		}
		var open bool // 已有未完成的启动指令则不重复下发；启动失败后再次加时会重新下发
		if open, err = s.d.Fleet.HasOpenCommand(ctx, tx, inst.ID, fleet.CmdStartInstance); err == nil && !open {
			_, err = s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdStartInstance, nil)
		}
	default:
		return apperr.ErrInvalidState
	}
	return err
}

// RenewOption 续费表单的一个档位：在可续时间与 90 天上限下最多能买的单位数（0 = 不可选）。
type RenewOption struct {
	Duration    catalog.Duration `json:"duration"`
	QuantityMax int              `json:"quantity_max"`
}

// RenewInfo 实例视图里的续费约束（规格 §7.1）。
type RenewInfo struct {
	RenewableUntil *time.Time    `json:"renewable_until"` // 同机下一条预订的 cap − Tail；无预订为 nil
	Options        []RenewOption `json:"renew_options"`
}

// renewBase 加时前的到期基准：新上界 = base + secs + Tail（规格 §7.1 表）。
func renewBase(inst Instance, now time.Time) time.Time {
	pending := time.Duration(inst.PendingSeconds) * time.Second
	switch {
	case inst.State == StateRunning && inst.Suspended() && inst.PausedRemaining != nil: // 与 occupiedUntil 一致
		return inst.resumeBase(now).Add(time.Duration(*inst.PausedRemaining) * time.Second)
	case inst.State == StateRunning: // 含暂停但缺 paused_remaining：occupiedUntil 同样按 expires_at 估算
		if inst.ExpiresAt != nil && inst.ExpiresAt.After(now) {
			return *inst.ExpiresAt
		}
		return now
	case inst.Booked() && (inst.State == StateReserved || inst.State == StateCreating):
		return inst.ScheduledStartAt.Add(pending)
	default: // 即时 creating / stopped：与 occupiedUntil 的估算一致
		return now.Add(CreateTimeout + pending)
	}
}

// renewInfo 读取同机下一条预订并算出各档位最多能买的单位数（不加锁；q 可以是事务）。
func (s *Service) renewInfo(ctx context.Context, q db.Querier, inst Instance, now time.Time) (RenewInfo, error) {
	var lower *time.Time
	if err := q.QueryRow(ctx, `SELECT lower(period) FROM machine_leases WHERE instance_id = $1 AND status IN ('reserved', 'active') ORDER BY id DESC LIMIT 1`, inst.ID).Scan(&lower); err != nil && !db.IsNoRows(err) {
		return RenewInfo{}, err
	}
	info := RenewInfo{}
	if lower != nil {
		nextLower, ok, err := nextReservedLower(ctx, q, inst.MachineID, inst.ID, *lower)
		if err != nil {
			return RenewInfo{}, err
		}
		if ok {
			until := nextLower.Add(-Tail)
			info.RenewableUntil = &until
		}
	}
	base := renewBase(inst, now)
	for _, d := range []catalog.Duration{catalog.D1d, catalog.D1w, catalog.D1m} {
		qmax := MaxQuantity(d)
		if info.RenewableUntil != nil {
			room := info.RenewableUntil.Sub(base)
			if room < d.Length() {
				qmax = 0
			} else if n := int(room / d.Length()); n < qmax {
				qmax = n
			}
		}
		info.Options = append(info.Options, RenewOption{Duration: d, QuantityMax: qmax})
	}
	return info, nil
}

// RenewInfoFor 实例视图用。
func (s *Service) RenewInfoFor(ctx context.Context, inst Instance) (RenewInfo, error) {
	return s.renewInfo(ctx, s.d.Pool, inst, s.d.Clock.Now())
}

// renewFits 所有加时路径共用：加 secs 秒后的到期不得越过同机下一条预订（规格 §7.1）。
func (s *Service) renewFits(ctx context.Context, q db.Querier, inst Instance, secs int64, now time.Time) error {
	info, err := s.renewInfo(ctx, q, inst, now)
	if err != nil {
		return err
	}
	if info.RenewableUntil == nil || !renewBase(inst, now).Add(time.Duration(secs)*time.Second).After(*info.RenewableUntil) {
		return nil
	}
	nextStart := info.RenewableUntil.Add(Tail + PrepLead)
	return apperr.New("renew_blocked", fmt.Sprintf("该机器自 %s 起已有预订，本次最多可续至 %s", tz.Format(nextStart), tz.Format(*info.RenewableUntil)), 409)
}

var errRenewBlockedGeneric = apperr.New("renew_blocked", "该机器已有后续预订，无法续费", 409)

// renewHook 测试钩子：在 renewFits 通过后、扣款前调用（制造「并发预订刚提交」的竞态）；生产为 nil。
var renewHook func()

// verifyLeaseFits 加时并 refreshLease 之后复核：lease 上界若被同机新提交的预订封顶（小于按新状态算出的 occupiedUntil），
// 说明 renewFits 读到的 cap 已过期——返回 renew_blocked 让事务回滚，不能既扣款又让到期越过别人已付的预订。
func (s *Service) verifyLeaseFits(ctx context.Context, tx pgx.Tx, id string, now time.Time) error {
	inst, err := lockInstance(ctx, tx, id)
	if err != nil {
		return err
	}
	// 预订（reserved / 已开通未 running）的 lease 由 addTime 直接推后，撞约束时已经报错；refreshLease 不动它，
	// occupiedUntil 的即时估算也不适用，复核会误报
	if inst.Booked() && (inst.State == StateReserved || inst.State == StateCreating) {
		return nil
	}
	var lower, upper time.Time
	if err := tx.QueryRow(ctx, `SELECT lower(period), upper(period) FROM machine_leases WHERE instance_id = $1 AND status = 'active' ORDER BY id DESC LIMIT 1`, id).Scan(&lower, &upper); err != nil {
		return err
	}
	if upper.Before(occupiedUntil(inst, lower, now)) {
		return errRenewBlockedGeneric
	}
	return nil
}
