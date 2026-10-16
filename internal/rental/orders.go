package rental

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/wallet"
)

type Order struct {
	ID             string           `json:"id"`
	UserID         int64            `json:"-"`
	Kind           string           `json:"kind"`
	InstanceID     string           `json:"instance_id"`
	ProductID      int64            `json:"product_id"`
	Duration       catalog.Duration `json:"duration"`
	Amount         int64            `json:"amount"`
	Status         string           `json:"status"`
	RefundedAmount int64            `json:"refunded_amount"`
	CreatedAt      time.Time        `json:"created_at"`
}

const orderCols = `id, user_id, kind, instance_id, product_id, duration, amount, status, refunded_amount, created_at`

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.UserID, &o.Kind, &o.InstanceID, &o.ProductID, &o.Duration, &o.Amount, &o.Status, &o.RefundedAmount, &o.CreatedAt)
	if db.IsNoRows(err) {
		return Order{}, apperr.ErrNotFound
	}
	return o, err
}

// charge 在事务内按当前价格扣款并创建订单（idemKey 为空表示不带幂等键）。实例行必须已存在（orders.instance_id 外键）。
func (s *Service) charge(ctx context.Context, tx pgx.Tx, userID int64, kind, instanceID string, productID int64, d catalog.Duration, idemKey string, now time.Time) (Order, error) {
	amount, err := s.d.Catalog.Quote(ctx, tx, productID, d)
	if err != nil {
		return Order{}, err
	}
	id := randx.ID("ord-", 12)
	remark := "新购 " + instanceID
	if kind == "renew" {
		remark = "续费 " + instanceID
	}
	if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{
		UserID: userID, Amount: -amount, Kind: wallet.KindOrderPay, RefType: "order", RefID: id, Remark: remark,
	}); err != nil {
		return Order{}, err
	}
	return scanOrder(tx.QueryRow(ctx, `INSERT INTO orders (id, user_id, kind, instance_id, product_id, duration, amount, status,
			idempotency_key, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'paid', NULLIF($8, ''), $9, $9) RETURNING `+orderCols,
		id, userID, kind, instanceID, productID, string(d), amount, idemKey, now))
}

func (s *Service) ListOrders(ctx context.Context, userID int64, limit int) ([]Order, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE user_id = $1 ORDER BY created_at DESC, id LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
}
