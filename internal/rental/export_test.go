package rental

import "time"

// SetExtraTaskTimeout 在测试中调小附加任务超时，返回恢复函数。
func SetExtraTaskTimeout(d time.Duration) func() {
	old := extraTaskTimeout
	extraTaskTimeout = d
	return func() { extraTaskTimeout = old }
}
