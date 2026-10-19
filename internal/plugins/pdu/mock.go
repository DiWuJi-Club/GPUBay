package pdu

import (
	"context"
	"fmt"
	"sync"
)

// Mock 记录调用，供开发与测试使用；Fail 非空时所有操作返回该错误。
type Mock struct {
	Fail  error
	mu    sync.Mutex
	calls []string
}

func (m *Mock) PowerCycle(_ context.Context, outlet int) error {
	if m.Fail != nil {
		return m.Fail
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, fmt.Sprintf("cycle:%d", outlet))
	return nil
}

func (m *Mock) Status(context.Context, int) (bool, error) {
	if m.Fail != nil {
		return false, m.Fail
	}
	return true, nil
}

func (m *Mock) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}
