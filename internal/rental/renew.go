package rental

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
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
	if quantity < 1 || quantity > MaxQuantity(d) {
		return Order{}, apperr.ErrBookingTooLong
	}
	if err := checkIdempotencyKey(idempotencyKey); err != nil {
		return Order{}, err
	}
	now := s.d.Clock.Now()
	secs := seconds(d) * int64(quantity)
	var ord Order
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
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
		if inst.State != StateReserved && inst.State != StateCreating && inst.State != StateRunning && inst.State != StateStopped {
			return apperr.ErrInvalidState
		}
		if ord, err = s.charge(ctx, tx, userID, "renew", inst.ID, inst.ProductID, d, quantity, false, nil, idempotencyKey, now); err != nil {
			return err
		}
		if err := s.addTime(ctx, tx, inst, secs, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, inst.ID, inst.State, inst.State, "renew "+ord.ID, now); err != nil {
			return err
		}
		if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
			return err
		}
		return nil // T9: verifyLeaseFits(ctx, tx, inst.ID, now)
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

// replayRenew 返回幂等键对应的已有续费订单；键被新购订单或其他实例的续费占用时报冲突。
func replayRenew(prev Order, instanceID string) (Order, error) {
	if prev.Kind != "renew" || prev.InstanceID != instanceID {
		return Order{}, errIdempotencyConflict
	}
	return prev, nil
}

// Release 用户主动释放：立即进入洗机，已付费用不退。
func (s *Service) Release(ctx context.Context, userID int64, instanceID string) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.State != StateRunning && inst.State != StateStopped {
			return apperr.ErrInvalidState
		}
		return s.beginWipe(ctx, tx, inst, "user released", now)
	})
}

// addTime 按实例当前状态增加可用时长（续费与后台免费延长共用）：
// 运行中延长到期时间（已到期未清扫的从当前时刻起算；暂停中加到冻结的剩余时长）并重算提醒标记；
// 开通中或停机时计入待开始时长，停机且没有未完成的启动指令时下发启动；预订中计入待开始时长并推后 reserved 时段的上界。
func (s *Service) addTime(ctx context.Context, tx pgx.Tx, inst Instance, secs int64, now time.Time) error {
	var err error
	switch {
	case inst.State == StateReserved:
		_, err = tx.Exec(ctx, `UPDATE instances SET pending_seconds = pending_seconds + $2, updated_at = $3 WHERE id = $1`, inst.ID, secs, now)
		if err == nil { // 预订的占用时段按新时长推后（refreshLease 对 reserved 不动，这里直接改）
			_, err = tx.Exec(ctx, `UPDATE machine_leases SET period = tstzrange(lower(period), upper(period) + ($2::bigint * interval '1 second')), updated_at = $3
				WHERE instance_id = $1 AND status = 'reserved'`, inst.ID, secs, now)
		}
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
