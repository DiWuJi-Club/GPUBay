package rental

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

const maxIdempotencyKey = 64

var errIdempotencyConflict = apperr.New("idempotency_conflict", "幂等键已被其他请求使用", 409)

// checkIdempotencyKey：长度 ≤ 64，且不能含 '#'（批次子订单的键是 "<key>#<序号>"，用户若能提交含 # 的键就可能撞上别人的子单键）。
func checkIdempotencyKey(key string) error {
	if len(key) > maxIdempotencyKey {
		return apperr.BadRequest("幂等键长度不能超过 64")
	}
	if strings.Contains(key, "#") {
		return apperr.BadRequest("幂等键不能包含 #")
	}
	return nil
}

// lockIdempotency 取事务级咨询锁串行化同一用户同一幂等键的请求。咨询锁在事务内最先获取，之后才按头注释的顺序加行锁。
func lockIdempotency(ctx context.Context, tx pgx.Tx, userID int64, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "idem:"+strconv.FormatInt(userID, 10)+":"+key)
	return err
}

// lockIdempotencyKey（单台下单与续费用）：加锁后先查批次——键若被 POST /bookings 用过即冲突——再查订单。
func lockIdempotencyKey(ctx context.Context, tx pgx.Tx, userID int64, key string) (Order, bool, error) {
	if err := lockIdempotency(ctx, tx, userID, key); err != nil {
		return Order{}, false, err
	}
	if _, found, err := findBatchByKey(ctx, tx, userID, key); err != nil {
		return Order{}, false, err
	} else if found {
		return Order{}, false, errIdempotencyConflict
	}
	return findByIdempotencyKey(ctx, tx, userID, key)
}

func findByIdempotencyKey(ctx context.Context, q db.Querier, userID int64, key string) (Order, bool, error) {
	ord, err := scanOrder(q.QueryRow(ctx, `SELECT `+orderCols+` FROM orders WHERE user_id = $1 AND idempotency_key = $2`, userID, key))
	if errors.Is(err, apperr.ErrNotFound) {
		return Order{}, false, nil
	}
	if err != nil {
		return Order{}, false, err
	}
	return ord, true, nil
}

func findBatchByKey(ctx context.Context, q db.Querier, userID int64, key string) (Batch, bool, error) {
	var b Batch
	err := q.QueryRow(ctx, `SELECT id, user_id, count, created_at FROM rental_batches WHERE user_id = $1 AND idempotency_key = $2`, userID, key).
		Scan(&b.ID, &b.UserID, &b.Count, &b.CreatedAt)
	if db.IsNoRows(err) {
		return Batch{}, false, nil
	}
	if err != nil {
		return Batch{}, false, err
	}
	return b, true, nil
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// isIdempotencyViolation 判断是否为并发重复请求触发的幂等键唯一约束冲突（订单）。
func isIdempotencyViolation(err error) bool {
	return pgCode(err) == "23505" && db.ConstraintName(err) == "orders_user_idempotency_uniq"
}

// isBatchIdempotencyViolation 判断是否为并发重复请求触发的幂等键唯一约束冲突（批次）。
func isBatchIdempotencyViolation(err error) bool {
	return pgCode(err) == "23505" && db.ConstraintName(err) == "rental_batches_user_idempotency_uniq"
}

// withRetry 对死锁检测（40P01）与序列化失败（40001）把整个事务重试一次；其它错误原样返回。
func withRetry(fn func() error) error {
	err := fn()
	if c := pgCode(err); c == "40P01" || c == "40001" {
		err = fn()
	}
	return err
}
