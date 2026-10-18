package agentgw

import "sync"

type registry struct {
	mu        sync.Mutex
	byMachine map[int64]*conn
	byConn    map[string]*conn
}

func newRegistry() *registry {
	return &registry{byMachine: map[int64]*conn{}, byConn: map[string]*conn{}}
}

type conn struct{} // Task 9 填充

func (r *registry) abortAll(err error) {}
