package rental

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// CancelQuote 取消预订的退款预览（规格 §5.5，裁定 B4）。
type CancelQuote struct {
	Refund   int64     `json:"refund"`
	Percent  int       `json:"percent"`
	Deadline time.Time `json:"deadline"` // 可取消的截止时刻 = 开始 − PrepLead
	Max      int64     `json:"max"`      // 已付未退总额
}

// cancelPercent：下单 15 分钟内全退（冷静期）；之后距开始 ≥ 24h 全退，否则 50%。
func cancelPercent(inst Instance, now time.Time) int {
	if now.Before(inst.CreatedAt.Add(CancelCoolOff)) {
		return 100
	}
	if inst.ScheduledStartAt != nil && inst.ScheduledStartAt.Sub(now) >= FreeCancelBefore {
		return 100
	}
	return LateCancelRefundPercent
}

// CancelQuote 不修改任何数据；只对 reserved 实例有意义。
func (s *Service) CancelQuote(ctx context.Context, userID int64, id string) (CancelQuote, error) {
	inst, err := s.GetForUser(ctx, userID, id)
	if err != nil {
		return CancelQuote{}, err
	}
	if inst.State != StateReserved || !inst.Booked() {
		return CancelQuote{}, apperr.ErrInvalidState
	}
	orders, err := refundableOrders(ctx, s.d.Pool, []string{inst.ID}, false)
	if err != nil {
		return CancelQuote{}, err
	}
	now := s.d.Clock.Now()
	pct := cancelPercent(inst, now)
	max := quote(inst, orders, now).Max // 退款基数沿用 refund.go 的 quote（已含数量与多笔订单），不另写价格公式
	return CancelQuote{Refund: max * int64(pct) / 100, Percent: pct, Deadline: inst.ScheduledStartAt.Add(-PrepLead), Max: max}, nil
}

// Cancel 用户取消预订：只在开通时刻（开始前 1 小时）之前允许；退款比例见 cancelPercent。
func (s *Service) Cancel(ctx context.Context, userID int64, id, ip string) (Instance, error) {
	now := s.d.Clock.Now()
	var out Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.State != StateReserved || !inst.Booked() {
			return apperr.ErrInvalidState
		}
		if !now.Before(inst.ScheduledStartAt.Add(-PrepLead)) {
			return apperr.ErrCancelNotAllowed
		}
		pct := cancelPercent(inst, now)
		refund, err := s.cancelInstance(ctx, tx, inst, pct, 0, "user cancel", "取消预订 "+inst.ID, now)
		if err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("booking.cancel", "instance", inst.ID,
			map[string]any{"refund": refund, "percent": pct})); err != nil {
			return err
		}
		out, err = lockInstance(ctx, tx, inst.ID)
		return err
	})
	return out, err
}

// AdminCancelBooking 后台取消预订（ops + 动态码由 API 层校验）：全额退款；提交后发短信。
func (s *Service) AdminCancelBooking(ctx context.Context, actor audit.Actor, id, reason string) (int64, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return 0, apperr.BadRequest("请填写取消原因")
	}
	now := s.d.Clock.Now()
	var refund int64
	var userID int64
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.State != StateReserved || !inst.Booked() {
			return apperr.ErrInvalidState
		}
		userID = inst.UserID
		refund, err = s.cancelInstance(ctx, tx, inst, 100, actor.AdminID, "admin cancel: "+reason, "后台取消预订 "+inst.ID+"："+reason, now)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("booking.admin_cancel", "instance", inst.ID, map[string]any{"reason": reason, "refund": refund}))
	})
	if err != nil {
		return 0, err
	}
	if err := s.d.Notifier.NotifyBookingCancelled(ctx, userID, id, "平台取消"); err != nil {
		s.d.Log.Warn("booking cancelled sms failed", "instance", id, "err", err)
	}
	return refund, nil
}

// cancelInstance 把 reserved（或改派失败后的 creating）实例取消：lease 与实例置 cancelled、清主机私钥、按比例退款、写事件。
// 调用方已持实例锁。返回实际退款金额。
func (s *Service) cancelInstance(ctx context.Context, tx pgx.Tx, inst Instance, percent int, operatorID int64, reason, remark string, now time.Time) (int64, error) {
	if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'cancelled', updated_at = $2
		WHERE instance_id = $1 AND status IN ('reserved', 'active')`, inst.ID, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'cancelled', cancelled_at = $2, ssh_host_key_enc = NULL, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
		return 0, err
	}
	if err := s.d.Fleet.CancelCommands(ctx, tx, inst.ID, "booking cancelled"); err != nil {
		return 0, err
	}
	orders, err := refundableOrders(ctx, tx, []string{inst.ID}, true)
	if err != nil {
		return 0, err
	}
	amount := quote(inst, orders, now).Max * int64(percent) / 100
	if _, err := s.refundAcross(ctx, tx, inst.UserID, orders, amount, operatorID, remark, now); err != nil {
		return 0, err
	}
	return amount, recordEvent(ctx, tx, inst.ID, inst.State, StateCancelled, fmt.Sprintf("%s, refund %d", reason, amount), now)
}

// cancelPlatform 平台原因取消（无法履约、用户失去租用资格）：全额退；短信由调用方在事务提交后发。
func (s *Service) cancelPlatform(ctx context.Context, tx pgx.Tx, inst Instance, reason string, now time.Time) (int64, error) {
	return s.cancelInstance(ctx, tx, inst, 100, 0, reason, "平台取消预订 "+inst.ID, now)
}
