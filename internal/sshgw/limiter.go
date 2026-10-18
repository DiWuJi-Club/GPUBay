package sshgw

import (
	"net"
	"sync"
	"time"

	"spark-cloud/internal/platform/clock"
)

const (
	failWindow       = 10 * time.Minute
	blockFor         = 10 * time.Minute
	defaultIPFails   = 10
	defaultInstFails = 30
	defaultMaxConns  = 32
)

// limiter 进程内限流：按来源 IP（IPv6 取 /64）计并发连接与失败连接数，按实例计失败连接数。
type limiter struct {
	mu        sync.Mutex
	clk       clock.Clock
	disabled  bool
	ipFails   int
	instFails int
	maxConns  int
	conns     map[string]int
	fails     map[string][]time.Time
	instF     map[string][]time.Time
	blocked   map[string]time.Time
}

func newLimiter(clk clock.Clock, disabled bool, maxConns int) *limiter {
	if maxConns <= 0 {
		maxConns = defaultMaxConns
	}
	return &limiter{clk: clk, disabled: disabled, ipFails: defaultIPFails, instFails: defaultInstFails, maxConns: maxConns,
		conns: map[string]int{}, fails: map[string][]time.Time{}, instF: map[string][]time.Time{}, blocked: map[string]time.Time{}}
}

// ipKey：IPv4 原样；IPv6 取 /64 前缀（轮换地址绕不过封禁）。
func ipKey(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if p.To4() != nil {
		return p.String()
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func prune(ts []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > failWindow {
		i++
	}
	return ts[i:]
}

// acquireConn 在新连接到来时调用：被封禁或并发超限返回 false。
func (l *limiter) acquireConn(ip string) bool {
	if l.disabled {
		return true
	}
	k := ipKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	if until, ok := l.blocked[k]; ok {
		if now.Before(until) {
			return false
		}
		delete(l.blocked, k)
	}
	if l.conns[k] >= l.maxConns {
		return false
	}
	l.conns[k]++
	return true
}

func (l *limiter) releaseConn(ip string) {
	if l.disabled {
		return
	}
	k := ipKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conns[k] <= 1 {
		delete(l.conns, k)
	} else {
		l.conns[k]--
	}
}

// failed 记录一条「关闭时仍未认证成功」的连接；返回该 IP 是否因此被封禁、该实例是否刚达到告警阈值。
func (l *limiter) failed(ip, instance string) (blocked, alert bool) {
	if l.disabled {
		return false, false
	}
	k := ipKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	l.fails[k] = append(prune(l.fails[k], now), now)
	if len(l.fails[k]) >= l.ipFails {
		l.blocked[k] = now.Add(blockFor)
		l.fails[k] = nil
		blocked = true
	}
	if instance != "" {
		l.instF[instance] = append(prune(l.instF[instance], now), now)
		if len(l.instF[instance]) == l.instFails {
			alert = true
		}
	}
	return blocked, alert
}

func (l *limiter) failCount(ip string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(prune(l.fails[ipKey(ip)], l.clk.Now()))
}

func (l *limiter) isBlocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.blocked[ipKey(ip)]
	return ok && l.clk.Now().Before(until)
}
