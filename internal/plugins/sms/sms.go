// Package sms 定义短信发送插件接口；生产实现（阿里云）在计划 4 中加入。
package sms

import (
	"context"
	"sync"
)

const (
	TemplateLoginCode        = "login_code"        // 参数：code
	TemplateExpiryReminder   = "expiry_reminder"   // 参数：instance、hours
	TemplateAutoRenewFailed  = "auto_renew_failed" // 参数：instance、reason
	TemplateBookingCancelled = "booking_cancelled" // 参数：instance、reason
)

type Sender interface {
	Send(ctx context.Context, phone, template string, params map[string]string) error
}

type Message struct {
	Phone    string
	Template string
	Params   map[string]string
}

// Mock 把短信记录在内存里，供开发与测试使用。
type Mock struct {
	mu   sync.Mutex
	sent []Message
}

func (m *Mock) Send(_ context.Context, phone, template string, params map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, Message{Phone: phone, Template: template, Params: params})
	return nil
}

func (m *Mock) Sent() []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Message(nil), m.sent...)
}
