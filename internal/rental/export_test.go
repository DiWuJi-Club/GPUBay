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

// 计划 6：导出内部分配函数供 rental_test 包直接测试。
type PickSpec = pickSpec

var (
	PickMachines    = pickMachines
	PlaceOnMachines = placeOnMachines
	ImmediatePeriod = immediatePeriod
	BookingPeriod   = bookingPeriod
	CountAvailable  = countAvailable
	InsertLease     = insertLease
)

// SetRenewHookForTest 设置 renewFits 之后的钩子，返回恢复函数。
func SetRenewHookForTest(f func()) func() {
	renewHook = f
	return func() { renewHook = nil }
}

// SetLockRetryHookForTest 设置 pickLocked 决定重挑时（退避前）的钩子，返回恢复函数。
func SetLockRetryHookForTest(f func()) func() {
	lockRetryHook = f
	return func() { lockRetryHook = nil }
}

// AutoRenewTiers 导出自动续费的降档序列。
var AutoRenewTiers = autoRenewTiers
