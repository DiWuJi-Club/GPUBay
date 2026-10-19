package rental

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
)

// SetExtraTaskTimeout 在测试中调小附加任务超时，返回恢复函数。
func SetExtraTaskTimeout(d time.Duration) func() {
	old := extraTaskTimeout
	extraTaskTimeout = d
	return func() { extraTaskTimeout = old }
}

// RefreshLease 在独立事务内对实例调用 refreshLease（测试直接驱动状态 → 时段同步）。
func (s *Service) RefreshLease(ctx context.Context, id string) error {
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error { return s.refreshLease(ctx, tx, id, s.d.Clock.Now()) })
}
