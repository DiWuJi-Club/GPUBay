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
	failNext map[string]error
}

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
	return nil
}

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
