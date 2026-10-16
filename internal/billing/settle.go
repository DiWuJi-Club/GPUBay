package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/plugins/pay"
	"spark-cloud/internal/wallet"
)

var (
	errUnknownTrade   = errors.New("billing: 未知的商户订单号或渠道不符")
	errAmountMismatch = errors.New("billing: 支付金额与充值单不符")
)

// Settle 按支付结果入账；回调、主动查询、开发环境模拟支付共用。重复调用只入账一次。
// 未支付的结果忽略；金额不符写告警且不入账；已关闭的单收到支付成功照常入账并写告警。
func (s *Service) Settle(ctx context.Context, channel string, n pay.Notification) error {
	if !n.Paid {
		return nil
	}
	now := s.d.Clock.Now()
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		r, err := scanRecharge(tx.QueryRow(ctx, `SELECT `+rechargeCols+` FROM recharges WHERE id = $1 FOR UPDATE`, n.OutTradeNo))
		if errors.Is(err, apperr.ErrNotFound) || (err == nil && r.Channel != channel) {
			bizErr = fmt.Errorf("%w: %s via %s", errUnknownTrade, n.OutTradeNo, channel)
			return nil
		}
		if err != nil {
			return err
		}
		if r.Status == StatusPaid {
			return nil
		}
		if n.Amount != r.Amount {
			bizErr = fmt.Errorf("%w: %s want %d got %d", errAmountMismatch, r.ID, r.Amount, n.Amount)
			return s.d.Alerts.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "critical", Kind: "recharge_amount_mismatch",
				Message: fmt.Sprintf("充值单 %s 金额 %d 分，渠道通知 %d 分（渠道单号 %s），未入账，请人工核对",
					r.ID, r.Amount, n.Amount, n.ChannelTradeNo)})
		}
		if _, err := tx.Exec(ctx, `UPDATE recharges SET status = 'paid', channel_trade_no = $2, paid_at = $3, updated_at = $3
			WHERE id = $1`, r.ID, n.ChannelTradeNo, now); err != nil {
			return err
		}
		if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{UserID: r.UserID, Amount: r.Amount, Kind: wallet.KindRecharge,
			RefType: "recharge", RefID: r.ID, Remark: channelName(r.Channel) + "充值"}); err != nil {
			return err
		}
		if r.Status == StatusClosed {
			return s.d.Alerts.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "warning", Kind: "recharge_paid_after_close",
				Message: fmt.Sprintf("充值单 %s 关闭后收到支付成功（%d 分，渠道单号 %s），已照常入账，请核对", r.ID, r.Amount, n.ChannelTradeNo)})
		}
		return nil
	})
	if err != nil {
		return err
	}
	return bizErr
}

// HandleNotify 处理渠道的异步回调：验签 → 入账 → 按渠道格式回复。验签失败只记日志（不写库、不写告警，避免被刷）。
func (s *Service) HandleNotify(ctx context.Context, channel string, w http.ResponseWriter, r *http.Request) {
	p, ok := s.d.Providers[channel]
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, err := p.ParseNotify(r)
	if err != nil {
		s.d.Log.Warn("pay notify rejected", "channel", channel, "err", err)
		p.NotifyAck(w, false)
		return
	}
	if err := s.Settle(ctx, channel, n); err != nil {
		s.d.Log.Error("pay notify settle failed", "channel", channel, "out_trade_no", n.OutTradeNo, "err", err)
		p.NotifyAck(w, false)
		return
	}
	p.NotifyAck(w, true)
}

type pendingRef struct {
	id      string
	channel string
}

func (s *Service) pending(ctx context.Context, pred string, args ...any) ([]pendingRef, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT id, channel FROM recharges
		WHERE status = 'pending' AND channel <> 'manual' AND `+pred+` ORDER BY created_at LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (pendingRef, error) {
		var p pendingRef
		err := r.Scan(&p.id, &p.channel)
		return p, err
	})
}

// Sweep 主动查询创建 1 分钟～2 小时仍未支付的单（回调可能丢失），关闭创建超过 2 小时的单。
func (s *Service) Sweep(ctx context.Context) error {
	now := s.d.Clock.Now()
	var errs []error
	due, err := s.pending(ctx, `created_at <= $1 AND created_at > $2`, now.Add(-QueryAfter), now.Add(-CloseAfter))
	if err != nil {
		errs = append(errs, fmt.Errorf("list due: %w", err))
	}
	for _, r := range due {
		if _, err := s.queryAndSettle(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	stale, err := s.pending(ctx, `created_at <= $1`, now.Add(-CloseAfter))
	if err != nil {
		errs = append(errs, fmt.Errorf("list stale: %w", err))
	}
	for _, r := range stale {
		// 关单前最后查一次，避免关掉刚付款的单
		settled, err := s.queryAndSettle(ctx, r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if settled {
			continue
		}
		if p := s.d.Providers[r.channel]; p != nil {
			if err := p.Close(ctx, r.id); err != nil {
				s.d.Log.Warn("pay close failed", "recharge", r.id, "channel", r.channel, "err", err)
			}
		}
		if _, err := s.d.Pool.Exec(ctx, `UPDATE recharges SET status = 'closed', closed_at = $2, updated_at = $2
			WHERE id = $1 AND status = 'pending'`, r.id, now); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", r.id, err))
		}
	}
	return errors.Join(errs...)
}

// queryAndSettle 向渠道查询一笔单，已支付则入账；渠道已下线（未配置）时返回 false。
func (s *Service) queryAndSettle(ctx context.Context, r pendingRef) (bool, error) {
	p := s.d.Providers[r.channel]
	if p == nil {
		return false, nil
	}
	n, err := p.Query(ctx, r.id)
	if err != nil {
		return false, fmt.Errorf("query %s: %w", r.id, err)
	}
	if !n.Paid {
		return false, nil
	}
	n.OutTradeNo = r.id
	if err := s.Settle(ctx, r.channel, n); err != nil {
		return false, fmt.Errorf("settle %s: %w", r.id, err)
	}
	return true, nil
}
