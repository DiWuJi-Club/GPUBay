package sshgw

func SetLimitsForTest(s *Server, ipFails, instFails, maxConns int) {
	s.lim.mu.Lock()
	defer s.lim.mu.Unlock()
	s.lim.ipFails, s.lim.instFails, s.lim.maxConns = ipFails, instFails, maxConns
}

func FailCountForTest(s *Server, ip string) int { return s.lim.failCount(ip) }
func BlockedForTest(s *Server, ip string) bool  { return s.lim.isBlocked(ip) }

func HostkeysDroppedForTest(s *Server) int64 { return s.hostkeysDropped.Load() }

// IPKeyForTest 暴露限流键（IPv6 取 /64）。
func IPKeyForTest(ip string) string { return ipKey(ip) }

// SweepForTest 按当前（假）时钟执行一次限流表清扫。
func SweepForTest(s *Server) { s.lim.sweep(s.d.Clock.Now()) }

// LimiterSizesForTest 返回限流三张表（IP 失败、实例失败、封禁）的键数。
func LimiterSizesForTest(s *Server) (fails, instF, blocked int) { return s.lim.sizes() }

const MaxPendingOpensForTest = maxPendingOpens
