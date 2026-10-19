package executor

import (
	"context"
	"time"
)

// SetReportTimeoutForTest 调短 Report 的采集上限。
func SetReportTimeoutForTest(e *Executor, d time.Duration) { e.reportTimeout = d }

// SetRecoverStepTimeoutForTest 调短 Recover / 重试中每个实例挂载与启动的上限。
func SetRecoverStepTimeoutForTest(e *Executor, d time.Duration) { e.recoverStepTimeout = d }

// SetRecoverGraceForTest 调短待重试实例报 unknown 的宽限期。
func SetRecoverGraceForTest(e *Executor, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recoverGrace = d
}

// RetryRecoverPendingForTest 直接跑一轮恢复重试。
func RetryRecoverPendingForTest(e *Executor, ctx context.Context) { e.retryRecoverPending(ctx) }

// EnsureLimitsForTest 直接跑一轮限速巡检。
func EnsureLimitsForTest(e *Executor, ctx context.Context) { e.ensureLimits(ctx) }

// SetRulesIntervalForTest 调短规则刷新（及恢复重试）的周期，返回恢复函数；必须在 Run 之前调用。
func SetRulesIntervalForTest(e *Executor, d time.Duration) (restore func()) {
	old := e.rulesInterval
	e.rulesInterval = d
	return func() { e.rulesInterval = old }
}

// LockOpsForTest 占住 opMu，模拟有实例指令正在执行；返回释放函数。
func LockOpsForTest(e *Executor) (unlock func()) {
	e.opMu.Lock()
	return e.opMu.Unlock
}

// RecoverPendingForTest 返回当前待重试启动的实例 ID。
func RecoverPendingForTest(e *Executor) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.recoverPending))
	for id := range e.recoverPending {
		ids = append(ids, id)
	}
	return ids
}
