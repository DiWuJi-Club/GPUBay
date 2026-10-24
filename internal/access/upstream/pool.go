package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Store 是首连信任写回所需的实例存储（rental.Service 实现）。
type Store interface {
	SetSSHHostPubkeyIfEmpty(ctx context.Context, id, pub string) (bool, error)
	SSHHostPubkey(ctx context.Context, id string) (string, error) // 写回竞争失败后复读
}

// ErrClosed 表示池已关闭（平台退出中）；不是 DialError，调用方不应据此告警。
var ErrClosed = errors.New("upstream: 连接池已关闭")

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
	client  *ssh.Client
	lanIP   string
	hostPub string // 建立连接时校验通过的主机公钥（固定公钥或首连信任的公钥）
	stop    chan struct{}
	once    sync.Once // dropEntry 可能被 keepalive 与 Dial 并发调用
}

type Pool struct {
	cfg    Config
	store  Store
	mu     sync.Mutex
	locks  map[string]chan struct{} // 每实例一个单槽信号量：并发首次拨号只建一条，等待可被 ctx 打断
	conns  map[string]*entry
	closed bool
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
	return &Pool{cfg: cfg, store: store, locks: map[string]chan struct{}{}, conns: map[string]*entry{}}
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
		if conn, err = c.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
			return conn, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) { // 调用方放弃：共享连接不受影响
			return nil, err
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
	want := strings.TrimSpace(t.HostPubkey)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	l := p.locks[t.InstanceID]
	if l == nil {
		l = make(chan struct{}, 1)
		p.locks[t.InstanceID] = l
	}
	p.mu.Unlock()
	select {
	case l <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("等待上游连接: %w", ctx.Err())
	}
	defer func() { <-l }()
	p.mu.Lock()
	e := p.conns[t.InstanceID]
	p.mu.Unlock()
	// 路由公钥为空（存量实例、路由表未刷新）时沿用连接上已校验的公钥
	if e != nil && e.lanIP == t.LanIP && (want == "" || want == e.hostPub) {
		return e.client, nil
	}
	if e != nil {
		p.dropEntry(t.InstanceID, e) // 登记地址或主机公钥变了
	}
	addr := net.JoinHostPort(t.LanIP, strconv.Itoa(p.cfg.Port))
	trusted := want
	tofu := func(pub string) error {
		ok, err := p.store.SetSSHHostPubkeyIfEmpty(ctx, t.InstanceID, pub)
		if err != nil {
			return err
		}
		if !ok { // 其它副本抢先写回：复读，一致即接受
			cur, err := p.store.SSHHostPubkey(ctx, t.InstanceID)
			if err != nil {
				return err
			}
			if strings.TrimSpace(cur) != pub {
				return fmt.Errorf("上游主机密钥不匹配：其它副本已登记 %s，实际 %s", fingerprintOf(cur), fingerprintOf(pub))
			}
			trusted = pub
			return nil
		}
		trusted = pub
		p.cfg.Log.Warn("upstream host key trusted on first use", "instance", t.InstanceID, "fingerprint", fingerprintOf(pub))
		return nil
	}
	d := net.Dialer{Timeout: p.cfg.DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil { // 调用方放弃，不是上游不可达
			return nil, ctxErr
		}
		return nil, &DialError{InstanceID: t.InstanceID, Err: err}
	}
	// 握手有截止时间，且 ctx 取消即关闭底层连接：卡死的 sshd 不会一直占着该实例的锁
	nc.SetDeadline(time.Now().Add(p.cfg.DialTimeout))
	stopAfter := context.AfterFunc(ctx, func() { nc.Close() })
	cfg := ClientConfig(p.cfg.Signer, HostKeyCallback(want, tofu), p.cfg.DialTimeout)
	cfg.HostKeyAlgorithms = HostKeyAlgorithms(want)
	cc, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	if !stopAfter() && err == nil { // ctx 已取消、底层连接已被关闭
		cc.Close()
		err = ctx.Err()
	}
	if err != nil {
		nc.Close()
		if ctxErr := ctx.Err(); ctxErr != nil { // 调用方放弃，不是上游不可达
			return nil, ctxErr
		}
		return nil, &DialError{InstanceID: t.InstanceID, Err: err}
	}
	nc.SetDeadline(time.Time{})
	client := ssh.NewClient(cc, chans, reqs)
	e = &entry{client: client, lanIP: t.LanIP, hostPub: trusted, stop: make(chan struct{})}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		client.Close()
		return nil, ErrClosed
	}
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

// Close 关闭全部连接；之后 Dial 一律返回 ErrClosed。
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	all := p.conns
	p.conns = map[string]*entry{}
	p.mu.Unlock()
	for id, e := range all {
		p.dropEntry(id, e)
	}
}

// limitedBuf 只保留前 max 字节，其余丢弃（exec 输出截断，不报错）。
type limitedBuf struct {
	buf bytes.Buffer
	max int
}

func (l *limitedBuf) Write(p []byte) (int, error) {
	n := len(p) // 报告全部写入：截断不能让 io.Copy 以 ErrShortWrite 失败
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		l.buf.Write(p)
	}
	return n, nil
}

// execCloseGrace 是 ctx 到期关闭 session 后等上游确认的上限；超时说明上游已不回应，丢弃共享连接让 Wait 返回。
const execCloseGrace = 2 * time.Second

// isConnErr 判断错误是否来自传输层（连接已断 / 写失败），区别于上游正常的拒绝回复。
func isConnErr(err error) bool {
	var ne net.Error
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.As(err, &ne)
}

// Exec 在容器里执行一条命令（复用共享 ssh.Client 上的一个 session）。命令至多执行一次：
//   - 远端非零退出：exit = 退出码，err = nil；退出码缺失：exit = -1，err 非 DialError。
//   - NewSession 遇连接层错误（命令尚未发出）：丢弃连接重试一次，再失败返回 *DialError。
//   - 上游拒绝 session 或 exec：普通错误，共享连接保留（丢弃会断掉该实例的全部入口隧道）。
//   - Start 遇连接层错误：丢弃连接，返回普通错误，不重试（exec 可能已送达）。
//   - ctx 到期：尽力发 SIGKILL 并关闭 session，返回已收到的输出与 ctx.Err()；共享连接保留，
//     除非上游在 execCloseGrace 内不回应关闭（此时丢弃连接，保证 Exec 及时返回）。
//
// stdout / stderr 各截断到 maxOutput 字节（<= 0 视为 1 MiB）。
func (p *Pool) Exec(ctx context.Context, t Target, cmd string, stdin []byte, maxOutput int) (stdout, stderr []byte, exit int, err error) {
	if maxOutput <= 0 {
		maxOutput = 1 << 20
	}
	var c *ssh.Client
	var sess *ssh.Session
	for attempt := 0; ; attempt++ {
		if c, err = p.client(ctx, t); err != nil {
			return nil, nil, -1, err
		}
		if sess, err = c.NewSession(); err == nil {
			break
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, -1, ctxErr
		}
		var oce *ssh.OpenChannelError
		if errors.As(err, &oce) { // 上游拒绝 session：连接本身完好
			return nil, nil, -1, fmt.Errorf("上游拒绝 session: %w", err)
		}
		// 其余 NewSession 错误都来自传输层：写失败，或等确认时连接断开
		// （x/crypto 此时报 "unexpected packet in response to channel open: <nil>"，不是 io.EOF）
		p.dropClient(t.InstanceID, c)
		if attempt > 0 {
			return nil, nil, -1, &DialError{InstanceID: t.InstanceID, Err: err}
		}
	}
	out, errb := &limitedBuf{max: maxOutput}, &limitedBuf{max: maxOutput}
	sess.Stdin, sess.Stdout, sess.Stderr = bytes.NewReader(stdin), out, errb
	type runResult struct {
		started bool
		err     error
	}
	done := make(chan runResult, 1)
	go func() {
		if err := sess.Start(cmd); err != nil {
			done <- runResult{err: err}
			return
		}
		done <- runResult{started: true, err: sess.Wait()}
	}()
	select {
	case r := <-done:
		sess.Close()
		if !r.started {
			if isConnErr(r.err) {
				p.dropClient(t.InstanceID, c)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, nil, -1, ctxErr
			}
			if isConnErr(r.err) {
				return nil, nil, -1, fmt.Errorf("发送命令时上游连接中断（命令可能已执行）: %w", r.err)
			}
			return nil, nil, -1, fmt.Errorf("上游拒绝执行命令: %w", r.err)
		}
		var ee *ssh.ExitError
		var em *ssh.ExitMissingError
		switch {
		case errors.As(r.err, &ee):
			return out.buf.Bytes(), errb.buf.Bytes(), ee.ExitStatus(), nil
		case errors.As(r.err, &em):
			return out.buf.Bytes(), errb.buf.Bytes(), -1, fmt.Errorf("远端未返回退出码: %w", r.err)
		}
		// r.err 为 nil，或远端已以 0 退出、只是没读完 stdin（Wait 报 stdin 拷贝错误）：都按退出码 0 返回
		return out.buf.Bytes(), errb.buf.Bytes(), 0, nil
	case <-ctx.Done():
		sess.Signal(ssh.SIGKILL) // 尽力而为：容器 sshd 不一定支持 signal
		sess.Close()             // 只关这个 session；共享连接保留
		grace := time.NewTimer(execCloseGrace)
		defer grace.Stop()
		select {
		case <-done:
		case <-grace.C:
			p.dropClient(t.InstanceID, c) // 上游对关闭无回应：丢弃连接，Wait 随之返回
			<-done
		}
		return out.buf.Bytes(), errb.buf.Bytes(), -1, ctx.Err()
	}
}
