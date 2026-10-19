package rental

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

const maxIdempotencyKey = 64

var errIdempotencyConflict = apperr.New("idempotency_conflict", "幂等键已被其他请求使用", 409)

func checkIdempotencyKey(key string) error {
	if len(key) > maxIdempotencyKey {
		return apperr.BadRequest("幂等键长度不能超过 64")
	}
	return nil
}

// lockIdempotencyKey 用事务级咨询锁串行化同一用户同一幂等键的请求，并返回此前用该键创建的订单。
// 咨询锁在事务内最先获取，之后才按「instance → agent_commands → orders → wallet」加行锁。
func lockIdempotencyKey(ctx context.Context, tx pgx.Tx, userID int64, key string) (Order, bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "idem:"+strconv.FormatInt(userID, 10)+":"+key); err != nil {
		return Order{}, false, err
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

// isIdempotencyViolation 判断是否为并发重复请求触发的幂等键唯一约束冲突。
func isIdempotencyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "orders_user_idempotency_uniq"
}
