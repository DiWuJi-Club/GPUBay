package rental

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

type RefundQuote struct {
	InstanceID       string `json:"instance_id"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	UnitOrderID      string `json:"unit_order_id"` // 计算单价所用的订单（最近一笔可退订单）
	Suggested        int64  `json:"suggested"`     // 建议退款金额（分）
	Max              int64  `json:"max"`           // 可退上限（分）
}

type refundable struct {
	id          string
	amount      int64
	refunded    int64
	refundCount int
	duration    catalog.Duration
}

// instanceChain 返回该实例以及通过换机链路追溯到的全部前身实例 ID。
func instanceChain(ctx context.Context, q db.Querier, id string) ([]string, error) {
	rows, err := q.Query(ctx, `WITH RECURSIVE chain (id) AS (
			SELECT $1::text
			UNION
			SELECT t.from_instance_id FROM instance_transfers t JOIN chain c ON t.to_instance_id = c.id
		) SELECT id FROM chain`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// refundableOrders 返回链路上仍可退的订单，从新到旧；lock 为 true 时加行锁（调用方须已锁实例）。
func refundableOrders(ctx context.Context, q db.Querier, chain []string, lock bool) ([]refundable, error) {
	sql := `SELECT id, amount, refunded_amount, refund_count, duration FROM orders
		WHERE instance_id = ANY($1) AND status IN ('paid', 'partially_refunded') ORDER BY created_at DESC, id DESC`
	if lock {
		sql += ` FOR UPDATE`
	}
	rows, err := q.Query(ctx, sql, chain)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (refundable, error) {
		var o refundable
		err := r.Scan(&o.id, &o.amount, &o.refunded, &o.refundCount, &o.duration)
		return o, err
	})
}

func remainingSeconds(inst Instance, now time.Time) int64 {
	switch inst.State {
	case StateRunning:
		return int64(inst.Remaining(now) / time.Second)
	case StateCreating, StateStopped:
		return inst.PendingSeconds
	}
	return 0
}

func quote(inst Instance, orders []refundable, now time.Time) RefundQuote {
	q := RefundQuote{InstanceID: inst.ID, RemainingSeconds: remainingSeconds(inst, now)}
	for _, o := range orders {
		q.Max += o.amount - o.refunded
	}
	if len(orders) > 0 {
		latest := orders[0]
		q.UnitOrderID = latest.id
		if secs := seconds(latest.duration); secs > 0 {
			q.Suggested = q.RemainingSeconds * latest.amount / secs
		}
	}
	if q.Suggested > q.Max {
		q.Suggested = q.Max
	}
	return q
}

// RefundQuote 计算退款建议金额与可退上限（不修改任何数据）。
func (s *Service) RefundQuote(ctx context.Context, instanceID string) (RefundQuote, error) {
	inst, err := s.Get(ctx, instanceID)
	if err != nil {
		return RefundQuote{}, err
	}
	chain, err := instanceChain(ctx, s.d.Pool, inst.ID)
	if err != nil {
		return RefundQuote{}, err
	}
	orders, err := refundableOrders(ctx, s.d.Pool, chain, false)
	if err != nil {
		return RefundQuote{}, err
	}
	return quote(inst, orders, s.d.Clock.Now()), nil
}

// RefundRelease 后台「退款并释放」：实例进入洗机，并按订单从新到旧退还 amount（分）到余额。返回实际退款金额。
// 加锁顺序：instance → agent_commands（beginWipe）→ orders → wallet。
func (s *Service) RefundRelease(ctx context.Context, actor audit.Actor, instanceID string, amount int64, reason string) (int64, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return 0, apperr.BadRequest("请填写退款原因")
	}
	if amount < 0 {
		return 0, apperr.BadRequest("退款金额不能为负数")
	}
	now := s.d.Clock.Now()
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if inst.State != StateCreating && inst.State != StateRunning && inst.State != StateStopped {
			return apperr.ErrInvalidState
		}
		chain, err := instanceChain(ctx, tx, inst.ID)
		if err != nil {
			return err
		}
		if err := s.beginWipe(ctx, tx, inst, "admin refund-release: "+reason, now); err != nil {
			return err
		}
		orders, err := refundableOrders(ctx, tx, chain, true)
		if err != nil {
			return err
		}
		q := quote(inst, orders, now) // inst 是加锁时读到的状态，beginWipe 不会改变它
		if amount > q.Max {
			return apperr.BadRequest(fmt.Sprintf("退款金额不能超过可退金额 %d.%02d 元", q.Max/100, q.Max%100))
		}
		parts, err := s.refundAcross(ctx, tx, inst.UserID, orders, amount, actor.AdminID, "退款并释放 "+inst.ID+"："+reason, now)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("instance.refund_release", "instance", inst.ID, map[string]any{
			"amount": amount, "reason": reason, "state": inst.State, "suggested": q.Suggested, "max": q.Max, "orders": parts,
		}))
	})
	if err != nil {
		return 0, err
	}
	return amount, nil
}

// refundAcross 按订单从新到旧（orders 须已按此顺序排列并加锁）退还 amount（分），
// 每笔订单记一条 refund 流水（ref_id = 订单号-r序号）并更新已退金额与状态。返回各订单的退款明细。
func (s *Service) refundAcross(ctx context.Context, tx pgx.Tx, userID int64, orders []refundable, amount, operatorID int64, remark string, now time.Time) ([]map[string]any, error) {
	left := amount
	var parts []map[string]any
	for _, o := range orders {
		if left == 0 {
			break
		}
		take := min(left, o.amount-o.refunded)
		if take <= 0 {
			continue
		}
		seq := o.refundCount + 1
		if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{
			UserID: userID, Amount: take, Kind: wallet.KindRefund, RefType: "order", RefID: fmt.Sprintf("%s-r%d", o.id, seq),
			OperatorID: operatorID, Remark: remark,
		}); err != nil {
			return nil, err
		}
		status := "partially_refunded"
		if o.refunded+take == o.amount {
			status = "refunded"
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET refunded_amount = refunded_amount + $2, refund_count = $3, status = $4, updated_at = $5
			WHERE id = $1`, o.id, take, seq, status, now); err != nil {
			return nil, err
		}
		parts = append(parts, map[string]any{"order_id": o.id, "amount": take})
		left -= take
	}
	return parts, nil
}
