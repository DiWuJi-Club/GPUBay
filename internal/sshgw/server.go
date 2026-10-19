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
	serverVersion     = "SSH-2.0-SparkCloud"
	maxAuthTries      = 6
	handshakeTimeout  = 30 * time.Second
	upstreamTimeout   = 10 * time.Second // Config.UpstreamTimeout 的默认值
	rejectWait        = 2 * time.Second
	limiterSweepEvery = time.Minute
	acceptBackoffMin  = 5 * time.Millisecond
	acceptBackoffMax  = time.Second
	maxUnauthConns    = 256
	unauthWarnEvery   = time.Minute
)

type Store interface {
	Access(ctx context.Context, instanceID string) (rental.AccessInfo, error)
	SessionStates(ctx context.Context, ids []string) (map[string]rental.SessionState, error) // 周期扫描用的批量状态；不存在的 ID 不在结果里
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
	MaxUnauthConns         int           // 0 → 256：全进程握手中（未认证）连接上限，超出直接关闭（同 OpenSSH MaxStartups 的兜底）
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
	closing    atomic.Bool   // 网关主动断开中（扫描、drain、上游断开后复查）：上游随之结束不再复查
	upDead     chan struct{} // 上游底层连接读出错时关闭：早于 x/crypto 关闭上游通道
	verdict    chan struct{} // relay 判定完「上游断开是否因停机」（需要时已写好提示）后关闭
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

// disconnectWriteWait 是断开前写提示的最长等待：客户端不读数据时通道窗口可能为 0，Write 会一直阻塞。
const disconnectWriteWait = time.Second

// disconnect 向所有 session 通道的 stderr 写提示（最多等 disconnectWriteWait）后关闭两端连接。
// x/crypto 不支持带消息的 SSH_MSG_DISCONNECT，提示只能经通道 stderr 送达。
// 只有第一次调用生效：扫描、drain、上游断开复查可能同时触发，提示只写一次。
func (x *session) disconnect(msg string) {
	if !x.closing.CompareAndSwap(false, true) {
		return
	}
	x.mu.Lock()
	shells := append([]ssh.Channel(nil), x.shells...)
	x.mu.Unlock()
	written := make(chan struct{})
	go func() {
		defer close(written)
		for _, ch := range shells {
			ch.Stderr().Write([]byte("\r\n" + msg + "\r\n"))
		}
	}()
	t := time.NewTimer(disconnectWriteWait)
	select {
	case <-written:
	case <-t.C:
	}
	t.Stop()
	x.down.Close() // 关闭连接也让卡住的 Write 返回
	x.up.Close()
}

type Server struct {
	cfg    Config
	d      Deps
	sshCfg *ssh.ServerConfig
	lim    *limiter

	pending sync.Map // 远端地址 → *attempt：握手期间是否做过真实认证尝试、针对哪个实例

	mu          sync.Mutex
	sessions    map[*session]struct{}
	conns       map[net.Conn]struct{} // 全部已 accept 的底层连接（含握手中、拒绝中）：drain 超时后逐一关闭
	perInstance map[string]int
	lastUnreach map[string]time.Time // upstream_unreachable 告警的进程内节流（5 分钟）

	wg              sync.WaitGroup
	hostkeysDropped atomic.Int64
	unauth          atomic.Int64 // 握手中的连接数
	unauthWarned    atomic.Int64 // 上次「握手中连接已满」告警的 UnixNano（节流）
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
	if cfg.MaxUnauthConns <= 0 {
		cfg.MaxUnauthConns = maxUnauthConns
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
	s := &Server{cfg: cfg, d: d, sessions: map[*session]struct{}{}, conns: map[net.Conn]struct{}{}, perInstance: map[string]int{}, lastUnreach: map[string]time.Time{}}
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

// Serve 接受连接直到 ctx 结束；随后停止 accept，最多等 DrainTimeout 让会话自然结束，再强制关闭（含握手中的连接）。
// 运行期间每 10 秒扫描会话状态、每 60 秒刷新会话 last_seen_at、每分钟清扫限流表（runLoops，随 ctx 结束，Serve 返回前已退出）。
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	loopsDone := make(chan struct{})
	go func() { defer close(loopsDone); s.runLoops(ctx) }()
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		s.acceptLoop(ctx, lis)
	}()
	<-ctx.Done()
	lis.Close()
	<-accepted
	s.drain()
	<-loopsDone
	return nil
}

// acceptLoop 接受连接直到监听关闭或 ctx 结束；其它 Accept 错误（如 EMFILE）记录后退避重试（5ms 起翻倍，上限 1s，同 net/http）。
func (s *Server) acceptLoop(ctx context.Context, lis net.Listener) {
	var delay time.Duration
	for {
		nc, err := lis.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			if delay == 0 {
				delay = acceptBackoffMin
			} else if delay *= 2; delay > acceptBackoffMax {
				delay = acceptBackoffMax
			}
			s.d.Log.Error("ssh accept", "err", err, "retry_in", delay)
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			continue
		}
		delay = 0
		s.wg.Add(1)
		s.trackConn(nc, true) // 先登记再起 goroutine：drain 一定能看到它
		go s.handleConn(ctx, nc)
	}
}

func remoteIP(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// attempt 记录一条连接在握手期间的认证尝试（只由 password / publickey 回调写入，Banner 不算）。
// 只有凭据真正被校验（实例可登录）或用户名查不到实例时才算尝试；实例存在但停机 / 暂停 / 开通中 / 关闭了密码登录时不算。
type attempt struct {
	mu       sync.Mutex
	tried    bool
	instance string // 最近一次查到的实例 ID；查不到的用户名不计入实例失败

	// connLookup 的握手期缓存（含解密后的实例密码）：握手结束即清空
	cached     bool
	cachedUser string
	cachedInfo rental.AccessInfo
	cachedOK   bool
}

// dropCache 清掉握手期缓存，不让解密后的密码在会话期间留在内存里。
func (a *attempt) dropCache() {
	a.mu.Lock()
	a.cached, a.cachedUser, a.cachedInfo, a.cachedOK = false, "", rental.AccessInfo{}, false
	a.mu.Unlock()
}

// noteAttempt 在 password / publickey 回调里调用：found 表示用户名对应的实例存在。
func (s *Server) noteAttempt(c ssh.ConnMetadata, found bool) {
	v, ok := s.pending.Load(c.RemoteAddr().String())
	if !ok {
		return
	}
	a := v.(*attempt)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tried = true
	if found {
		a.instance = c.User()
	}
}

func (s *Server) handleConn(ctx context.Context, nc net.Conn) {
	defer s.wg.Done()
	defer s.trackConn(nc, false)
	ip := remoteIP(nc.RemoteAddr())
	// 全进程握手中连接上限：超出直接关闭，不计暴力破解失败（还没有任何认证尝试）
	if s.unauth.Add(1) > int64(s.cfg.MaxUnauthConns) {
		s.unauth.Add(-1)
		nc.Close()
		s.warnUnauthFull()
		return
	}
	if !s.lim.acquireConn(ip) {
		s.unauth.Add(-1)
		nc.Close()
		return
	}
	defer s.lim.releaseConn(ip)
	key := nc.RemoteAddr().String()
	att := &attempt{}
	s.pending.Store(key, att)
	defer s.pending.Delete(key)
	nc.SetDeadline(time.Now().Add(handshakeTimeout))
	down, chans, reqs, err := ssh.NewServerConn(nc, s.sshCfg)
	att.dropCache()
	s.unauth.Add(-1) // 握手结束（成功或失败）：已认证会话不占名额
	if err != nil {
		nc.Close()
		att.mu.Lock()
		tried, instance := att.tried, att.instance
		att.mu.Unlock()
		if tried { // 裸 TCP、版本串 / 密钥交换失败、只拿了 Banner 的连接都不计
			s.authFailed(ip, instance)
		}
		return
	}
	nc.SetDeadline(time.Time{})
	defer down.Close()
	s.serve(ctx, down, chans, reqs, ip)
}

// warnUnauthFull 记录握手中连接已满（每分钟最多一条，避免被洪泛刷屏）。
func (s *Server) warnUnauthFull() {
	now := time.Now().UnixNano()
	last := s.unauthWarned.Load()
	if now-last < int64(unauthWarnEvery) || !s.unauthWarned.CompareAndSwap(last, now) {
		return
	}
	s.d.Log.Warn("ssh unauthenticated connection limit reached, closing new connections", "limit", s.cfg.MaxUnauthConns)
}

// authFailed：一条做过认证尝试的连接关闭时仍未认证成功 → 按 IP 计 1 次；instance 非空（实例存在）时也按实例计 1 次。
func (s *Server) authFailed(ip, instance string) {
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
	// 认证用的是握手期缓存：这里重新查库复核（状态可能在握手期间变了）
	info, ok := s.lookup(instanceID)
	if !ok || !allowed(info) {
		msg := BannerReleased
		if ok {
			msg = stateBanner(info)
		}
		reject(msg)
		return
	}
	if method == "password" && !info.Instance.PasswordAuth { // 握手期间关闭了密码登录
		reject(MsgPasswordAuthOff)
		return
	}
	dialCtx, cancelDial := context.WithCancel(ctx)
	go func() { down.Wait(); cancelDial() }() // 用户在拨号期间断开：中止拨号
	upDead := make(chan struct{})
	up, upChans, upReqs, err := s.dialUpstream(dialCtx, info, upDead)
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
	sess := &session{instanceID: instanceID, userID: userID, ip: ip, down: down, up: up, upDead: upDead, verdict: make(chan struct{})}
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
// dead 在建立后的底层连接首次读出错时关闭（见 deathConn）。
func (s *Server) dialUpstream(ctx context.Context, info rental.AccessInfo, dead chan struct{}) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	if net.ParseIP(info.MachineLanIP) == nil { // 不拨任何不是 IP 的地址（空值、主机名）
		return nil, nil, nil, fmt.Errorf("机器内网地址无效 %q", info.MachineLanIP)
	}
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
	up, chans, reqs, err := ssh.NewClientConn(&deathConn{Conn: nc, dead: dead}, addr, cfg)
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

// deathConn 在 Read 首次出错时关闭 dead。x/crypto 读出错后才关闭全部通道、再让 Wait 返回，
// 所以通道看到 EOF 时 dead 一定已关闭：pipeChannels 据此区分「容器正常结束通道」与「上游连接断了」。
type deathConn struct {
	net.Conn
	dead chan struct{}
	once sync.Once
}

func (c *deathConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		c.once.Do(func() { close(c.dead) })
	}
	return n, err
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
