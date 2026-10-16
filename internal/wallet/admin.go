package wallet

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type LedgerFilter struct {
	UserID int64
	Kind   string
	From   *time.Time
	To     *time.Time // 不含
}

// AdminLedgerRow 是后台看到的流水（比用户端多用户与操作人）。
type AdminLedgerRow struct {
	LedgerRow
	UserID     int64  `json:"user_id"`
	OperatorID *int64 `json:"operator_id"`
}

var ledgerKinds = []string{string(KindRecharge), string(KindAdminAdjust), string(KindOrderPay), string(KindRefund)}

func (f LedgerFilter) where() (db.Where, error) {
	var w db.Where
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.Kind != "" {
		if !slices.Contains(ledgerKinds, f.Kind) {
			return w, apperr.BadRequest("流水类型不正确")
		}
		w.Add("kind = ?", f.Kind)
	}
	if f.From != nil {
		w.Add("created_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("created_at < ?", *f.To)
	}
	return w, nil
}

const adminLedgerCols = `id, amount, balance_after, kind, ref_type, ref_id, remark, created_at, user_id, operator_id`

func scanAdminLedger(row pgx.Row) (AdminLedgerRow, error) {
	var l AdminLedgerRow
	err := row.Scan(&l.ID, &l.Amount, &l.BalanceAfter, &l.Kind, &l.RefType, &l.RefID, &l.Remark, &l.CreatedAt, &l.UserID, &l.OperatorID)
	return l, err
}

func (s *Service) AdminListLedger(ctx context.Context, f LedgerFilter, p paging.Page) ([]AdminLedgerRow, int, error) {
	w, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.pool.Query(ctx, `SELECT `+adminLedgerCols+` FROM wallet_ledger WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY id DESC LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminLedgerRow, error) { return scanAdminLedger(r) })
	return list, total, err
}

// ExportLedger 按时间正序逐行回调；超过导出上限时在回调前返回 csvx.ErrTooLarge。
func (s *Service) ExportLedger(ctx context.Context, f LedgerFilter, fn func(AdminLedgerRow) error) error {
	w, err := f.where()
	if err != nil {
		return err
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return err
	}
	if total > csvx.MaxRows {
		return csvx.ErrTooLarge
	}
	rows, err := s.pool.Query(ctx, `SELECT `+adminLedgerCols+` FROM wallet_ledger WHERE `+w.SQL()+` ORDER BY id`, w.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		l, err := scanAdminLedger(rows)
		if err != nil {
			return err
		}
		if err := fn(l); err != nil {
			return err
		}
	}
	return rows.Err()
}
