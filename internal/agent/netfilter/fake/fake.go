package fake

import (
	"context"
	"sync"

	"spark-cloud/internal/agent/netfilter"
)

type Netfilter struct {
	mu       sync.Mutex
	rules    netfilter.Rules
	ensured  int
	limits   map[string]int
	limitN   int
	failNext map[string]error
}

var _ netfilter.Netfilter = (*Netfilter)(nil)

func New() *Netfilter { return &Netfilter{limits: map[string]int{}, failNext: map[string]error{}} }

func (n *Netfilter) FailNext(op string, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failNext[op] = err
}

func (n *Netfilter) EnsureRules(_ context.Context, r netfilter.Rules) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.failNext["rules"]; err != nil {
		delete(n.failNext, "rules")
		return err
	}
	n.rules = r
	n.ensured++
	return nil
}

func (n *Netfilter) Limit(_ context.Context, veth string, mbps int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.failNext["limit"]; err != nil {
		delete(n.failNext, "limit")
		return err
	}
	n.limits[veth] = mbps
	n.limitN++
	return nil
}

func (n *Netfilter) LimitApplied(_ context.Context, veth string) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.failNext["limitApplied"]; err != nil {
		delete(n.failNext, "limitApplied")
		return false, err
	}
	_, ok := n.limits[veth]
	return ok, nil
}

// Drop 模拟 veth 上的限速丢失（容器被外部重启、换了新 veth）。
func (n *Netfilter) Drop(veth string) { n.mu.Lock(); delete(n.limits, veth); n.mu.Unlock() }

// LimitCount 是 Limit 成功调用的累计次数。
func (n *Netfilter) LimitCount() int { n.mu.Lock(); defer n.mu.Unlock(); return n.limitN }

func (n *Netfilter) Rules() netfilter.Rules { n.mu.Lock(); defer n.mu.Unlock(); return n.rules }
func (n *Netfilter) EnsuredCount() int      { n.mu.Lock(); defer n.mu.Unlock(); return n.ensured }
func (n *Netfilter) Limits() map[string]int {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := map[string]int{}
	for k, v := range n.limits {
		out[k] = v
	}
	return out
}
