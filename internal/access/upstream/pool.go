package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Store interface {
	SetSSHHostPubkeyIfEmpty(ctx context.Context, id, pub string) (bool, error)
}

type Config struct {
	Signer      ssh.Signer
	Port        int           // 容器 sshd 在机器上的端口
	DialTimeout time.Duration // 零值 10s
	Keepalive   time.Duration // 零值 30s；连续 2 次无回应即关闭
	Log         *slog.Logger
}

// Target 是路由表给出的拨号目标。
type Target struct {
	InstanceID string
	LanIP      string
	HostPubkey string // 为空 → 首连信任并写回
}

// DialError 表示到达不了容器 sshd（拨号失败、握手失败、主机密钥不符）；端口未监听不算。
type DialError struct {
	InstanceID string
	Err        error
}

func (e *DialError) Error() string { return "上游 " + e.InstanceID + ": " + e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

type entry struct {
	client *ssh.Client
	lanIP  string
	stop   chan struct{}
	once   sync.Once // dropEntry 可能被 keepalive 与 Dial 并发调用
}

type Pool struct {
	cfg   Config
	store Store
	mu    sync.Mutex
	locks map[string]*sync.Mutex // 每实例一把：并发首次拨号只建一条
	conns map[string]*entry
}

func New(cfg Config, store Store) *Pool {
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.Keepalive <= 0 {
		cfg.Keepalive = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Pool{cfg: cfg, store: store, locks: map[string]*sync.Mutex{}, conns: map[string]*entry{}}
}

func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// Dial 返回一条到容器内 127.0.0.1:<port> 的通道（net.Conn）。
// 共享连接已坏（上游重启、keepalive 尚未察觉）时当场丢弃，并用新连接重试一次。
func (p *Pool) Dial(ctx context.Context, t Target, port int) (net.Conn, error) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var c *ssh.Client
		if c, err = p.client(ctx, t); err != nil {
			return nil, err
		}
		var conn net.Conn
		if conn, err = c.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
			return conn, nil
		}
		var oce *ssh.OpenChannelError
		if errors.As(err, &oce) { // 上游拒绝通道：连接本身完好
			return nil, fmt.Errorf("容器内端口 %d 未监听: %w", port, err)
		}
		p.dropClient(t.InstanceID, c) // 连接本身坏了：丢弃后重建
	}
	return nil, &DialError{InstanceID: t.InstanceID, Err: err}
}

func (p *Pool) client(ctx context.Context, t Target) (*ssh.Client, error) {
	if net.ParseIP(t.LanIP) == nil {
		return nil, &DialError{InstanceID: t.InstanceID, Err: fmt.Errorf("机器内网地址无效 %q", t.LanIP)}
	}
	p.mu.Lock()
	l := p.locks[t.InstanceID]
	if l == nil {
		l = &sync.Mutex{}
		p.locks[t.InstanceID] = l
	}
	p.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	p.mu.Lock()
	e := p.conns[t.InstanceID]
	p.mu.Unlock()
	if e != nil && e.lanIP == t.LanIP {
		return e.client, nil
	}
	if e != nil {
		p.Drop(t.InstanceID) // 登记地址变了
	}
	addr := net.JoinHostPort(t.LanIP, strconv.Itoa(p.cfg.Port))
	tofu := func(pub string) error {
		ok, err := p.store.SetSSHHostPubkeyIfEmpty(ctx, t.InstanceID, pub)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("主机公钥已由其它副本写回，请重试")
		}
		p.cfg.Log.Warn("upstream host key trusted on first use", "instance", t.InstanceID, "fingerprint", fingerprintOf(pub))
		return nil
	}
	d := net.Dialer{Timeout: p.cfg.DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, &DialError{InstanceID: t.InstanceID, Err: err}
	}
	cc, chans, reqs, err := ssh.NewClientConn(nc, addr, ClientConfig(p.cfg.Signer, HostKeyCallback(t.HostPubkey, tofu), p.cfg.DialTimeout))
	if err != nil {
		nc.Close()
		return nil, &DialError{InstanceID: t.InstanceID, Err: err}
	}
	client := ssh.NewClient(cc, chans, reqs)
	e = &entry{client: client, lanIP: t.LanIP, stop: make(chan struct{})}
	p.mu.Lock()
	p.conns[t.InstanceID] = e
	p.mu.Unlock()
	go p.keepalive(t.InstanceID, e)
	return client, nil
}

// keepalive 每 Keepalive 发一次 keepalive@openssh.com，连续 2 次无回应（或连接已关）即丢弃。
func (p *Pool) keepalive(id string, e *entry) {
	t := time.NewTicker(p.cfg.Keepalive)
	defer t.Stop()
	misses := 0
	closed := make(chan struct{})
	go func() { e.client.Wait(); close(closed) }()
	for {
		select {
		case <-e.stop:
			return
		case <-closed:
			p.dropEntry(id, e)
			return
		case <-t.C:
			res := make(chan bool, 1)
			go func() { _, _, err := e.client.SendRequest("keepalive@openssh.com", true, nil); res <- err == nil }()
			select {
			case ok := <-res:
				if ok {
					misses = 0
					continue
				}
			case <-time.After(10 * time.Second):
			}
			if misses++; misses >= 2 {
				p.cfg.Log.Warn("upstream keepalive failed, dropping connection", "instance", id)
				p.dropEntry(id, e)
				return
			}
		}
	}
}

// Drop 关闭并移除某实例的共享连接（实例离开 running、地址变化、连接损坏）。
func (p *Pool) Drop(instanceID string) {
	p.mu.Lock()
	e := p.conns[instanceID]
	p.mu.Unlock()
	if e != nil {
		p.dropEntry(instanceID, e)
	}
}

// dropClient 只在池里仍是 c 时丢弃：并发 Dial 已重建的新连接不受影响。
func (p *Pool) dropClient(id string, c *ssh.Client) {
	p.mu.Lock()
	e := p.conns[id]
	p.mu.Unlock()
	if e != nil && e.client == c {
		p.dropEntry(id, e)
	}
}

func (p *Pool) dropEntry(id string, e *entry) {
	p.mu.Lock()
	if p.conns[id] == e {
		delete(p.conns, id)
	}
	p.mu.Unlock()
	e.once.Do(func() { close(e.stop) })
	e.client.Close()
}

func (p *Pool) Close() {
	p.mu.Lock()
	all := p.conns
	p.conns = map[string]*entry{}
	p.mu.Unlock()
	for id, e := range all {
		p.dropEntry(id, e)
	}
}
