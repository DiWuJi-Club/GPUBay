package billing

import (
	"context"
	"fmt"
	"slices"
	"time"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/plugins/pay"
)

type RechargeFilter struct {
	UserID  int64
	Channel string
	Status  string
	From    *time.Time
	To      *time.Time // 不含
}

func (f RechargeFilter) where() (db.Where, error) {
	var w db.Where
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.Channel != "" {
		if !slices.Contains([]string{pay.ChannelWechat, pay.ChannelAlipay, ChannelManual}, f.Channel) {
			return w, apperr.BadRequest("充值渠道不正确")
		}
		w.Add("channel = ?", f.Channel)
	}
	if f.Status != "" {
		if !slices.Contains([]string{StatusPending, StatusPaid, StatusClosed}, f.Status) {
			return w, apperr.BadRequest("充值状态不正确")
		}
		w.Add("status = ?", f.Status)
	}
	if f.From != nil {
		w.Add("created_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("created_at < ?", *f.To)
	}
	return w, nil
}

func (s *Service) AdminList(ctx context.Context, f RechargeFilter, p paging.Page) ([]Recharge, int, error) {
	w, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM recharges WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	list, err := collectRecharges(s.d.Pool.Query(ctx, `SELECT `+rechargeCols+` FROM recharges WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...))
	return list, total, err
}

// Export 按创建时间正序逐行回调；超过导出上限时在回调前返回 csvx.ErrTooLarge。
func (s *Service) Export(ctx context.Context, f RechargeFilter, fn func(Recharge) error) error {
	w, err := f.where()
	if err != nil {
		return err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM recharges WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return err
	}
	if total > csvx.MaxRows {
		return csvx.ErrTooLarge
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT `+rechargeCols+` FROM recharges WHERE `+w.SQL()+` ORDER BY created_at, id`, w.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRecharge(rows)
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}
