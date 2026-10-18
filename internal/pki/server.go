package pki

import (
	"crypto/tls"
	"sync"
	"time"

	"spark-cloud/internal/platform/clock"
)

// RenewBefore 是服务端证书自动重签的提前量。
const RenewBefore = 30 * 24 * time.Hour

// ServerCerts 持有网关当前的服务端证书并支持热替换；不落盘。
type ServerCerts struct {
	ca    *CA
	hosts []string
	clk   clock.Clock
	mu    sync.RWMutex
	cert  *tls.Certificate
}

func NewServerCerts(ca *CA, hosts []string, clk clock.Clock) (*ServerCerts, error) {
	s := &ServerCerts{ca: ca, hosts: hosts, clk: clk}
	cert, err := ca.IssueServer(hosts, clk.Now())
	if err != nil {
		return nil, err
	}
	s.cert = &cert
	return s, nil
}

// GetCertificate 供 tls.Config.GetCertificate 使用。
func (s *ServerCerts) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return s.Current(), nil
}

func (s *ServerCerts) Current() *tls.Certificate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cert
}

// Rotate 在剩余有效期不足 RenewBefore 时重签；返回是否重签。
func (s *ServerCerts) Rotate() (bool, error) {
	now := s.clk.Now()
	if s.Current().Leaf.NotAfter.Sub(now) >= RenewBefore {
		return false, nil
	}
	cert, err := s.ca.IssueServer(s.hosts, now)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	s.cert = &cert
	s.mu.Unlock()
	return true, nil
}
