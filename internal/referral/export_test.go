package referral

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// SetCodeGen 替换 EnsureCode 的码生成器（测试撞码重试）；返回恢复函数。
func SetCodeGen(f func() string) (restore func()) {
	old := newCode
	newCode = f
	return func() { newCode = old }
}

type LedgerRowForTest = ledgerRow

var ErrSkip = errSkip

func (s *Service) AccrueForTest(ctx context.Context, tx pgx.Tx, now time.Time, l ledgerRow) error {
	return s.accrue(ctx, tx, now, l)
}

func LedgerRowOf(id, userID int64, kind string, amount int64, refID string, at time.Time) ledgerRow {
	return ledgerRow{ID: id, UserID: userID, Kind: kind, Amount: amount, RefID: refID, CreatedAt: at}
}

func (s *Service) ApplyRefundForTest(ctx context.Context, tx pgx.Tx, now time.Time, l ledgerRow) error {
	return s.applyRefund(ctx, tx, now, l)
}
