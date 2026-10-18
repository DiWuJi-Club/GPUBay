package state

// SetMaxResultsForTest 调小结果保留上限，返回恢复函数。
func SetMaxResultsForTest(n int) (restore func()) {
	old := maxResults
	maxResults = n
	return func() { maxResults = old }
}
