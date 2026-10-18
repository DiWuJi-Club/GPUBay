package ingress

// UsedNonces 返回已用 nonce 表的大小（仅测试用）。
func UsedNonces(s *Signer) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.used)
}
