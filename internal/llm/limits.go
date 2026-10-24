package llm

// Limits 类型定义在 limiter.go。

// Limits 账户限额：llm_accounts.*_limit 为 NULL 时用 DefaultRPM / DefaultTPM / DefaultConcurrency。
func (a Account) Limits() Limits {
	l := Limits{RPM: DefaultRPM, TPM: DefaultTPM, Concurrency: DefaultConcurrency}
	if a.RPMLimit != nil {
		l.RPM = *a.RPMLimit
	}
	if a.TPMLimit != nil {
		l.TPM = *a.TPMLimit
	}
	if a.ConcurrencyLimit != nil {
		l.Concurrency = *a.ConcurrencyLimit
	}
	return l
}
