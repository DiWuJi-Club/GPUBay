// Package sshgw 是自研 SSH 网关：用户名 = 实例 ID，密码 / 公钥在此校验，认证通过后用平台上游私钥登录容器 sshd 并透传通道。
package sshgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
)

const (
	serverVersion    = "SSH-2.0-SparkCloud"
	maxAuthTries     = 6
	handshakeTimeout = 30 * time.Second
	upstreamTimeout  = 10 * time.Second // Config.UpstreamTimeout 的默认值
	rejectWait       = 2 * time.Second
)

type Store interface {
	Access(ctx context.Context, instanceID string) (rental.AccessInfo, error)
	SetSSHHostPubkeyIfEmpty(ctx context.Context, id, pub string) (bool, error)
	SSHHostPubkey(ctx context.Context, id string) (string, error) // 首连信任写回竞争失败后复读
	OpenSSHSession(ctx context.Context, st rental.SSHSessionStart) (int64, error)
	TouchSSHSessions(ctx context.Context, ids []int64) error
	CloseSSHSession(ctx context.Context, id, up, down int64) error
	RaiseSSHBruteforce(ctx context.Context, instanceID string) error
	RaiseUpstreamUnreachable(ctx context.Context, instanceID string) error
}

type Keys interface {
	FindSSHKey(ctx context.Context, userID int64, fingerprint string) (identity.SSHKey, error)
	TouchSSHKey(ctx context.Context, id int64) error
}

type Config struct {
	HostKey                ssh.Signer
	Upstream               ssh.Signer
	UpstreamPort           int
	UpstreamTimeout        time.Duration // 0 → 10s：拨上游 TCP + SSH 握手的总时限
	MaxSessionsPerInstance int           // 0 → 16
	MaxConnsPerIP          int           // 0 → 32
	DrainTimeout           time.Duration // 0 → 30s
	KeepaliveInterval      time.Duration // 0 → 60s；测试调短（必须经 Config 传入，Serve 之后不可改）
	KeepaliveTimeout       time.Duration // 0 → 15s
	DisableRateLimits      bool
}

type Deps struct {
	Store Store
	Keys  Keys
	Clock clock.Clock
	Log   *slog.Logger
}

// session 是一条已认证并连上上游的用户连接。
type session struct {
	id         int64
	instanceID string
	userID     int64
	ip         string
	down       *ssh.ServerConn
	up         ssh.Conn
	bytesUp    atomic.Int64   // 客户端 → 容器
	bytesDown  atomic.Int64   // 容器 → 客户端
	pipes      sync.WaitGroup // 通道拷贝 goroutine：全部结束后字节数才是终值
	mu         sync.Mutex
	shells     []ssh.Channel // 已打开的 session 通道：强制断开时在这里写提示
}

func (x *session) addShell(ch ssh.Channel) {
	x.mu.Lock()
	x.shells = append(x.shells, ch)
	x.mu.Unlock()
}

func (x *session) removeShell(ch ssh.Channel) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for i, c := range x.shells {
		if c == ch {
			x.shells = append(x.shells[:i], x.shells[i+1:]...)
			return
		}
	}
}

// disconnect 向所有 session 通道写提示后关闭两端连接。
func (x *session) disconnect(msg string) {
	x.mu.Lock()
	shells := append([]ssh.Channel(nil), x.shells...)
	x.mu.Unlock()
	for _, ch := range shells {
		ch.Stderr().Write([]byte("\r\n" + msg + "\r\n"))
	}
	x.down.Close()
	x.up.Close()
}

type Server struct {
	cfg    Config
	d      Deps
	sshCfg *ssh.ServerConfig
	lim    *limiter

	pending sync.Map // 远端地址 → 握手期间最近一次出现的用户名（失败连接按实例计数用）

	mu          sync.Mutex
	sessions    map[*session]struct{}
	perInstance map[string]int
	lastUnreach map[string]time.Time // upstream_unreachable 告警的进程内节流（5 分钟）

	wg              sync.WaitGroup
	hostkeysDropped atomic.Int64
}

func New(cfg Config, d Deps) (*Server, error) {
	if cfg.HostKey == nil || cfg.Upstream == nil || cfg.UpstreamPort <= 0 {
		return nil, errors.New("sshgw: HostKey, Upstream and UpstreamPort are required")
	}
	if d.Store == nil || d.Keys == nil || d.Clock == nil {
		return nil, errors.New("sshgw: missing deps")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if cfg.MaxSessionsPerInstance <= 0 {
		cfg.MaxSessionsPerInstance = 16
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 30 * time.Second
	}
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = upstreamTimeout
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = keepaliveInterval
	}
	if cfg.KeepaliveTimeout <= 0 {
		cfg.KeepaliveTimeout = keepaliveTimeout
	}
	s := &Server{cfg: cfg, d: d, sessions: map[*session]struct{}{}, perInstance: map[string]int{}, lastUnreach: map[string]time.Time{}}
	s.lim = newLimiter(d.Clock, cfg.DisableRateLimits, cfg.MaxConnsPerIP)
	s.sshCfg = &ssh.ServerConfig{
		ServerVersion:     serverVersion,
		MaxAuthTries:      maxAuthTries,
		BannerCallback:    s.banner,
		PasswordCallback:  s.passwordCallback,
		PublicKeyCallback: s.publicKeyCallback,
	}
	s.sshCfg.AddHostKey(cfg.HostKey)
	return s, nil
}

// Sessions 返回当前已建立（已连上上游）的会话数。
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// Serve 接受连接直到 ctx 结束；随后停止 accept，最多等 DrainTimeout 让会话自然结束，再强制关闭。
// 每 10 秒扫描会话状态、每 60 秒刷新会话 last_seen_at（Task 11 的 runLoops）。
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	loopCtx, stopLoops := context.WithCancel(ctx)
	defer stopLoops()
	go s.runLoops(loopCtx)
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			nc, err := lis.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go s.handleConn(ctx, nc)
		}
	}()
	<-ctx.Done()
	lis.Close()
	<-accepted
	s.drain()
	return nil
}

func remoteIP(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

func (s *Server) noteUser(c ssh.ConnMetadata) { s.pending.Store(c.RemoteAddr().String(), c.User()) }

func (s *Server) handleConn(ctx context.Context, nc net.Conn) {
	defer s.wg.Done()
	ip := remoteIP(nc.RemoteAddr())
	if !s.lim.acquireConn(ip) {
		nc.Close()
		return
	}
	defer s.lim.releaseConn(ip)
	key := nc.RemoteAddr().String()
	s.pending.Store(key, "")
	defer s.pending.Delete(key)
	nc.SetDeadline(time.Now().Add(handshakeTimeout))
	down, chans, reqs, err := ssh.NewServerConn(nc, s.sshCfg)
	if err != nil {
		user, _ := s.pending.Load(key)
		s.authFailed(ip, user.(string))
		nc.Close()
		return
	}
	nc.SetDeadline(time.Time{})
	defer down.Close()
	s.serve(ctx, down, chans, reqs, ip)
}

// authFailed：一条连接关闭时仍未认证成功 → 按 IP 与实例各计 1 次。
func (s *Server) authFailed(ip, user string) {
	instance := ""
	if instanceIDRe.MatchString(user) {
		instance = user
	}
	blocked, alert := s.lim.failed(ip, instance)
	// 只记录合法的实例 ID：用户名是攻击者可控的任意文本，用户也可能把密码误填进用户名
	s.d.Log.Warn("ssh auth failed", "ip", ip, "instance", instance, "ip_blocked", blocked)
	if alert {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		if err := s.d.Store.RaiseSSHBruteforce(ctx, instance); err != nil {
			s.d.Log.Error("raise ssh_bruteforce", "instance", instance, "err", err)
		}
	}
}

func (s *Server) acquireInstance(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perInstance[id] >= s.cfg.MaxSessionsPerInstance {
		return false
	}
	s.perInstance[id]++
	return true
}

func (s *Server) releaseInstance(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perInstance[id] <= 1 {
		delete(s.perInstance, id)
	} else {
		s.perInstance[id]--
	}
}

// serve：认证通过后的一条连接。会话上限、再次状态校验、拨上游、记录会话、中继、收尾。
// ctx 是 Serve 的 ctx：只约束拨上游（网关退出或用户断开时中止拨号），不约束已建立的会话（由 drain 处理）。
func (s *Server) serve(ctx context.Context, down *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, ip string) {
	ext := down.Permissions.Extensions
	instanceID := ext["instance"]
	userID, _ := strconv.ParseInt(ext["user_id"], 10, 64)
	keyID, _ := strconv.ParseInt(ext["key_id"], 10, 64)
	method := ext["method"]
	// 全局请求（reqs）正常路径由 relay 按白名单转发给上游；拒绝路径里必须有人消费并回 false，
	// 否则 OpenSSH 客户端发的 no-more-sessions@openssh.com（WantReply）会把它卡住
	reject := func(msg string) {
		go ssh.DiscardRequests(reqs)
		rejectWithMessage(chans, msg)
	}
	if !s.acquireInstance(instanceID) {
		reject(MsgTooManySessions)
		return
	}
	defer s.releaseInstance(instanceID)
	info, ok := s.lookup(instanceID)
	if !ok || !allowed(info) {
		msg := BannerReleased
		if ok {
			msg = stateBanner(info)
		}
		reject(msg)
		return
	}
	dialCtx, cancelDial := context.WithCancel(ctx)
	go func() { down.Wait(); cancelDial() }() // 用户在拨号期间断开：中止拨号
	up, upChans, upReqs, err := s.dialUpstream(dialCtx, info)
	cancelled := dialCtx.Err() != nil
	cancelDial()
	if err != nil {
		if cancelled { // 用户已离开或网关退出：不是上游故障，不告警
			s.d.Log.Info("ssh upstream dial aborted", "instance", instanceID, "err", err)
			reject(MsgUpstreamUnreachable)
			return
		}
		s.d.Log.Warn("ssh upstream dial failed", "instance", instanceID, "err", err)
		s.alertUnreachable(instanceID)
		reject(MsgUpstreamUnreachable)
		return
	}
	sess := &session{instanceID: instanceID, userID: userID, ip: ip, down: down, up: up}
	rctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	start := rental.SSHSessionStart{InstanceID: instanceID, UserID: userID, MachineID: info.Instance.MachineID, AuthMethod: method, ClientIP: ip}
	if method == "publickey" && keyID != 0 {
		start.KeyID = &keyID
		if err := s.d.Keys.TouchSSHKey(rctx, keyID); err != nil {
			s.d.Log.Error("touch ssh key", "key", keyID, "err", err)
		}
	}
	if sess.id, err = s.d.Store.OpenSSHSession(rctx, start); err != nil {
		s.d.Log.Error("open ssh session record", "instance", instanceID, "err", err)
	}
	cancel()
	s.mu.Lock()
	s.sessions[sess] = struct{}{}
	s.mu.Unlock()
	started := time.Now()
	s.d.Log.Info("ssh session open", "instance", instanceID, "user", userID, "method", method, "ip", ip)
	s.relay(sess, down, chans, reqs, up, upChans, upReqs)
	sess.pipes.Wait() // 两端连接已关闭，全部通道随之结束；等拷贝把字节数加完再落库
	s.mu.Lock()
	delete(s.sessions, sess)
	s.mu.Unlock()
	if sess.id != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		if err := s.d.Store.CloseSSHSession(ctx, sess.id, sess.bytesUp.Load(), sess.bytesDown.Load()); err != nil {
			s.d.Log.Error("close ssh session record", "session", sess.id, "err", err)
		}
		cancel()
	}
	s.d.Log.Info("ssh session close", "instance", instanceID, "user", userID, "ip", ip, "duration", time.Since(started).Round(time.Second),
		"bytes_up", sess.bytesUp.Load(), "bytes_down", sess.bytesDown.Load())
}

// dialUpstream 用平台上游私钥连容器 sshd，主机密钥固定为 instances.ssh_host_pubkey（为空时首连信任写回）。
// TCP 拨号与 SSH 握手共用 UpstreamTimeout 截止时间，且 ctx 结束即中止（ClientConfig.Timeout 不覆盖已建立 TCP 上的握手）。
func (s *Server) dialUpstream(ctx context.Context, info rental.AccessInfo) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	addr := net.JoinHostPort(info.MachineLanIP, strconv.Itoa(s.cfg.UpstreamPort))
	timeout := s.cfg.UpstreamTimeout
	d := net.Dialer{Timeout: timeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, nil, err
	}
	id := info.Instance.ID
	tofu := func(pub string) error {
		tctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		ok, err := s.d.Store.SetSSHHostPubkeyIfEmpty(tctx, id, pub)
		if err != nil {
			return err
		}
		if !ok { // 其它副本抢先写回：复读，一致才接受
			cur, err := s.d.Store.SSHHostPubkey(tctx, id)
			if err != nil {
				return err
			}
			if strings.TrimSpace(cur) != pub {
				return fmt.Errorf("上游主机密钥不匹配：其它副本已登记 %s，实际 %s", fingerprintLine(cur), fingerprintLine(pub))
			}
			return nil
		}
		s.d.Log.Warn("upstream host key trusted on first use", "instance", id, "fingerprint", fingerprintLine(pub))
		return nil
	}
	nc.SetDeadline(time.Now().Add(timeout))
	stopAfter := context.AfterFunc(ctx, func() { nc.Close() })
	cfg := upstream.ClientConfig(s.cfg.Upstream, upstream.HostKeyCallback(info.Instance.SSHHostPubkey, tofu), timeout)
	cfg.HostKeyAlgorithms = upstream.HostKeyAlgorithms(info.Instance.SSHHostPubkey)
	up, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	if !stopAfter() && err == nil { // ctx 已结束、底层连接已被关闭
		up.Close()
		err = ctx.Err()
	}
	if err != nil {
		nc.Close()
		return nil, nil, nil, err
	}
	nc.SetDeadline(time.Time{})
	return up, chans, reqs, nil
}

// fingerprintLine 返回 authorized_keys 一行的 SHA256 指纹；无法解析时返回占位文本。
func fingerprintLine(line string) string {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return "(无法解析)"
	}
	return ssh.FingerprintSHA256(pub)
}

// alertUnreachable 进程内按实例 5 分钟节流后 RaiseAlertOnce。
func (s *Server) alertUnreachable(instanceID string) {
	now := s.d.Clock.Now()
	s.mu.Lock()
	if last, ok := s.lastUnreach[instanceID]; ok && now.Sub(last) < 5*time.Minute {
		s.mu.Unlock()
		return
	}
	s.lastUnreach[instanceID] = now
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	if err := s.d.Store.RaiseUpstreamUnreachable(ctx, instanceID); err != nil {
		s.d.Log.Error("raise upstream_unreachable", "instance", instanceID, "err", err)
	}
}

// rejectWithMessage：等客户端打开第一个 session 通道并发出 shell / exec / subsystem（共计最多 rejectWait），
// 把提示写到 stderr，退出码 1，然后结束。非 session 通道一律拒绝。
// 必须等到启动请求再写：否则通道在客户端发 exec 之前就关了，客户端只看到 EOF、看不到提示。
func rejectWithMessage(chans <-chan ssh.NewChannel, msg string) {
	timer := time.NewTimer(rejectWait)
	defer timer.Stop()
	for {
		select {
		case nc, ok := <-chans:
			if !ok {
				return
			}
			if nc.ChannelType() != "session" {
				nc.Reject(ssh.Prohibited, msg)
				continue
			}
			ch, reqs, err := nc.Accept()
			if err != nil {
				return
			}
			started := make(chan struct{})
			go func() {
				var once sync.Once
				for req := range reqs {
					start := req.Type == "shell" || req.Type == "exec" || req.Type == "subsystem"
					if req.WantReply {
						req.Reply(start || req.Type == "pty-req" || req.Type == "env", nil)
					}
					if start {
						once.Do(func() { close(started) })
					}
				}
			}()
			select {
			case <-started:
			case <-timer.C:
			}
			ch.Stderr().Write([]byte(msg + "\r\n"))
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
			ch.Close()
			return
		case <-timer.C:
			return
		}
	}
}

// runLoops：Task 11 填充扫描与刷新；本任务只等待 ctx 结束。
func (s *Server) runLoops(ctx context.Context) { <-ctx.Done() }

// drain：最多等 DrainTimeout 让会话自然结束，然后强制断开剩余会话。
func (s *Server) drain() {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(s.cfg.DrainTimeout):
	}
	s.mu.Lock()
	all := make([]*session, 0, len(s.sessions))
	for x := range s.sessions {
		all = append(all, x)
	}
	s.mu.Unlock()
	for _, x := range all {
		x.disconnect("网关正在重启，连接已断开")
	}
	<-done
}
