package executor

import "time"

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
