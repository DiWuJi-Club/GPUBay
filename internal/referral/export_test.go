package referral

// SetCodeGen 替换 EnsureCode 的码生成器（测试撞码重试）；返回恢复函数。
func SetCodeGen(f func() string) (restore func()) {
	old := newCode
	newCode = f
	return func() { newCode = old }
}
