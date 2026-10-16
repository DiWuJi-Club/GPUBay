// Package wallet 管理用户余额。所有变动先写只追加的流水，再更新余额。
package wallet

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
)

type Kind string

const (
	KindRecharge    Kind = "recharge"
	KindAdminAdjust Kind = "admin_adjust"
	KindOrderPay    Kind = "order_pay"
	KindRefund      Kind = "refund"
)

const MaxAdjust = 100_000_000 // 单次调账上限（分）

type Entry struct {
	UserID     int64
	Amount     int64 // 分；正数入账，负数扣款
	Kind       Kind
	RefType    string
	RefID      string
	OperatorID int64 // 管理员操作时填写
	Remark     string
}

type LedgerRow struct {
	ID           int64     `json:"id"`
	Amount       int64     `json:"amount"`
	BalanceAfter int64     `json:"balance_after"`
	Kind         Kind      `json:"kind"`
	RefType      string    `json:"ref_type"`
	RefID        string    `json:"ref_id"`
	Remark       string    `json:"remark"`
	CreatedAt    time.Time `json:"created_at"`
}

type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

func NewService(pool *pgxpool.Pool, clk clock.Clock) *Service {
	return &Service{pool: pool, clock: clk}
}

// Apply 在调用方事务内记一笔账并更新余额。
// 余额不足返回 ErrInsufficientBalance；同一 (kind, ref_type, ref_id) 重复记账返回 ErrDuplicate。
// 任何错误都要求调用方回滚事务。
func (s *Service) Apply(ctx context.Context, tx pgx.Tx, e Entry) (int64, error) {
	if e.Amount == 0 {
		return 0, apperr.BadRequest("金额不能为 0")
	}
	// 非管理员调整必须关联单据，否则唯一索引不生效，同一业务可能重复记账。
	if e.Kind != KindAdminAdjust && (e.RefType == "" || e.RefID == "") {
		return 0, apperr.BadRequest("缺少关联单据")
	}
	now := s.clock.Now()
	if _, err := tx.Exec(ctx, `INSERT INTO wallet_accounts (user_id, balance, updated_at) VALUES ($1, 0, $2)
		ON CONFLICT (user_id) DO NOTHING`, e.UserID, now); err != nil {
		return 0, err
	}
	var bal int64
	if err := tx.QueryRow(ctx, `SELECT balance FROM wallet_accounts WHERE user_id = $1 FOR UPDATE`, e.UserID).Scan(&bal); err != nil {
		return 0, err
	}
	after := bal + e.Amount
	if after < 0 {
		return 0, apperr.ErrInsufficientBalance
	}
	var operator *int64
	if e.OperatorID != 0 {
		operator = &e.OperatorID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, operator_id, remark, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.UserID, e.Amount, after, string(e.Kind), e.RefType, e.RefID, operator, e.Remark, now); err != nil {
		if db.IsUniqueViolation(err) {
			return 0, apperr.ErrDuplicate
		}
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE wallet_accounts SET balance = $2, updated_at = $3 WHERE user_id = $1`, e.UserID, after, now); err != nil {
		return 0, err
	}
	return after, nil
}

func (s *Service) Balance(ctx context.Context, userID int64) (int64, error) {
	var bal int64
	err := s.pool.QueryRow(ctx, `SELECT balance FROM wallet_accounts WHERE user_id = $1`, userID).Scan(&bal)
	if db.IsNoRows(err) {
		return 0, nil
	}
	return bal, err
}

// ListLedger 按时间倒序分页；beforeID 为 0 表示从最新开始。
func (s *Service) ListLedger(ctx context.Context, userID, beforeID int64, limit int) ([]LedgerRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if beforeID <= 0 {
		beforeID = 1<<63 - 1
	}
	rows, err := s.pool.Query(ctx, `SELECT id, amount, balance_after, kind, ref_type, ref_id, remark, created_at
		FROM wallet_ledger WHERE user_id = $1 AND id < $2 ORDER BY id DESC LIMIT $3`, userID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LedgerRow, error) {
		var l LedgerRow
		err := r.Scan(&l.ID, &l.Amount, &l.BalanceAfter, &l.Kind, &l.RefType, &l.RefID, &l.Remark, &l.CreatedAt)
		return l, err
	})
}

// AdminAdjust 管理员手动加减余额（线下转账入账、补偿、纠错），必须填写原因并写审计日志。
func (s *Service) AdminAdjust(ctx context.Context, operatorID, userID, amount int64, remark, ip string) (int64, error) {
	remark = strings.TrimSpace(remark)
	if remark == "" {
		return 0, apperr.BadRequest("请填写调整原因")
	}
	if amount == 0 || amount > MaxAdjust || amount < -MaxAdjust {
		return 0, apperr.BadRequest("调整金额需在 ±100 万元之间且不能为 0")
	}
	var after int64
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		after, err = s.Apply(ctx, tx, Entry{UserID: userID, Amount: amount, Kind: KindAdminAdjust, OperatorID: operatorID, Remark: remark})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.clock.Now(), audit.Entry{
			ActorType: "admin", ActorID: operatorID, Action: "wallet.adjust",
			TargetType: "user", TargetID: strconv.FormatInt(userID, 10),
			Diff: map[string]any{"amount": amount, "balance_after": after, "remark": remark}, IP: ip,
		})
	})
	if db.IsForeignKeyViolation(err) {
		return 0, apperr.ErrNotFound
	}
	return after, err
}
