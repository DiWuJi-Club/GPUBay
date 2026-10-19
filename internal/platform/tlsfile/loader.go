// Package tlsfile 从文件加载 TLS 证书并按 mtime 热加载：acme.sh 侧车或人工替换证书文件后，platform 不用重启。
package tlsfile

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"os"
	"sync"
	"time"
)

type Loader struct {
	CertFile string
	KeyFile  string
	Interval time.Duration    // 检查 mtime 的最小间隔，零值 1 分钟
	Now      func() time.Time // 零值 time.Now；app 传入 clock.Clock.Now
	// OnReloadError：连续失败次数恰好到 2 时回调一次（一轮连续失败只告警一次，成功加载后清零）；其余失败只记日志——
	// 证书与私钥分两次替换时，中间那次检查会看到不配对的一对，下一次就好了。可能在 TLS 握手路径上被调用，不应阻塞。
	// OnExpiring：CheckExpiry 发现剩余不足阈值时回调。
	OnReloadError func(err error)
	OnExpiring    func(notAfter time.Time)
	Log           *slog.Logger // 零值 slog.Default()

	reload    sync.Mutex // 串行化文件检查与重新加载：并发握手不会重复加载、重复回调
	mu        sync.Mutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	lastCheck time.Time
	failures  int // 连续失败的检查次数；> 0 时下次检查不看 mtime 直接重试
}

func (l *Loader) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Loader) interval() time.Duration {
	if l.Interval > 0 {
		return l.Interval
	}
	return time.Minute
}

func mtimes(certFile, keyFile string) (time.Time, time.Time, error) {
	cs, err := os.Stat(certFile)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	ks, err := os.Stat(keyFile)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return cs.ModTime(), ks.ModTime(), nil
}

// Load 首次加载（失败即返回错误，启动时调用）。
func (l *Loader) Load() error {
	l.reload.Lock()
	defer l.reload.Unlock()
	return l.load()
}

func (l *Loader) load() error {
	cm, km, err := mtimes(l.CertFile, l.KeyFile)
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(l.CertFile, l.KeyFile)
	if err != nil {
		return err
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return err
		}
	}
	l.mu.Lock()
	l.cert, l.certMod, l.keyMod, l.lastCheck, l.failures = &cert, cm, km, l.now(), 0
	l.mu.Unlock()
	return nil
}

// CheckNow 立即检查文件是否变化并重新加载；上次检查失败时不看 mtime 直接重试。失败保留旧证书，连续失败第 2 次时回调 OnReloadError。
func (l *Loader) CheckNow() {
	l.reload.Lock()
	defer l.reload.Unlock()
	l.mu.Lock()
	oldCM, oldKM, retry := l.certMod, l.keyMod, l.failures > 0
	l.lastCheck = l.now()
	l.mu.Unlock()
	cm, km, err := mtimes(l.CertFile, l.KeyFile)
	if err != nil {
		l.reloadFailed(err)
		return
	}
	if !retry && cm.Equal(oldCM) && km.Equal(oldKM) {
		return
	}
	if err := l.load(); err != nil {
		l.reloadFailed(err)
	}
}

func (l *Loader) reloadFailed(err error) {
	l.mu.Lock()
	l.failures++
	n := l.failures
	l.mu.Unlock()
	if n == 2 && l.OnReloadError != nil {
		l.OnReloadError(err)
		return
	}
	log := l.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("tls certificate reload failed, keeping previous certificate; will retry", "err", err, "consecutive_failures", n)
}

func (l *Loader) current() (*tls.Certificate, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cert, l.lastCheck
}

// GetCertificate 供 tls.Config 使用；每 Interval 最多检查一次文件。
func (l *Loader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert, last := l.current()
	if cert == nil {
		if err := l.Load(); err != nil {
			return nil, err
		}
		cert, _ = l.current()
		return cert, nil
	}
	if l.now().Sub(last) >= l.interval() {
		l.CheckNow()
		cert, _ = l.current()
	}
	return cert, nil
}

func (l *Loader) NotAfter() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cert == nil || l.cert.Leaf == nil {
		return time.Time{}
	}
	return l.cert.Leaf.NotAfter
}

func (l *Loader) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: l.GetCertificate}
}

// CheckExpiry：剩余有效期不足 warnWithin 时回调 OnExpiring。
func (l *Loader) CheckExpiry(warnWithin time.Duration) {
	na := l.NotAfter()
	if na.IsZero() || l.OnExpiring == nil {
		return
	}
	if na.Sub(l.now()) < warnWithin {
		l.OnExpiring(na)
	}
}

// RunExpiryCheck 每 every 检查一次到期（启动时先查一次），直到 ctx 结束。
// 每次检查前先 CheckNow：长时间没有 TLS 握手时也能拾取续期后的新文件，不误报到期。
func (l *Loader) RunExpiryCheck(ctx context.Context, every, warnWithin time.Duration) {
	l.CheckExpiry(warnWithin)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.CheckNow()
			l.CheckExpiry(warnWithin)
		}
	}
}
