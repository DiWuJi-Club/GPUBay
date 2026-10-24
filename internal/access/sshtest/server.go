// Package sshtest 是用 x/crypto/ssh 写的假「容器 sshd」：只接受平台上游公钥，支持 session（exec / shell / sftp）、
// direct-tcpip（按登记端口转发）、tcpip-forward 回连、agent 转发、hostkeys-00 推送，以及若干测试控制钩子。
// 供 sshgw / upstream / ingress 单元测试、Go 集成测试与 e2e-server 共用；生产二进制不引用它。
package sshtest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Options struct {
	AuthorizedKey ssh.PublicKey // 允许以 root 登录的公钥（平台上游公钥）
	HostKey       ssh.Signer    // nil → 生成 ed25519
	// Shell：exec 请求用本机 /bin/sh -c 真执行（工作目录 Root，HOME=Root）；false 时沿用回显模式（echo / exit / cat / …）。
	Shell bool
	// Root：Shell 模式的工作目录（充当容器的 /）；为空时自动建临时目录并在 Close 时删除。
	Root string
}

type Server struct {
	lis    net.Listener
	signer ssh.Signer
	cfg    *ssh.ServerConfig

	mu          sync.Mutex
	targets     map[int]string // direct-tcpip 目标端口 → 实际拨号地址
	lastDirect  []byte
	forwardAddr string
	conns       []*connEntry       // 每条已 accept 的原始连接（握手前即登记）
	extras      map[io.Closer]bool // 转发用的目标连接 / 监听器，Close 时一并关闭
	holds       map[int]*hold      // direct-tcpip 目标端口 → 挂起闸门（测试慢速通道打开）
	held        int                // 当前挂起中的 direct-tcpip 打开数
	globals     []string           // 收到的全局请求类型（按到达顺序）
	opens       []string           // 收到的通道打开类型（按到达顺序）
	closed      bool
	wg          sync.WaitGroup

	shell    bool
	root     string
	rootTemp bool            // root 是 New 建的临时目录，Close 时删除
	ctx      context.Context // Shell 模式命令的生命周期；Close 时取消
	cancel   context.CancelFunc
}

func New(o Options) (*Server, error) {
	if o.AuthorizedKey == nil {
		return nil, errors.New("sshtest: AuthorizedKey required")
	}
	signer := o.HostKey
	if signer == nil {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		if signer, err = ssh.NewSignerFromKey(priv); err != nil {
			return nil, err
		}
	}
	want := o.AuthorizedKey.Marshal()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == "root" && bytes.Equal(key.Marshal(), want) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("sshtest: key not authorized")
		},
	}
	cfg.AddHostKey(signer)
	ctx, cancel := context.WithCancel(context.Background())
	root, rootTemp := o.Root, false
	if o.Shell && root == "" {
		var err error
		if root, err = os.MkdirTemp("", "sshtest-root-"); err != nil {
			cancel()
			return nil, err
		}
		rootTemp = true
		// 解开符号链接（macOS 的 /var → /private/var），让 Root() 与命令里的 pwd 一致
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		if rootTemp {
			os.RemoveAll(root)
		}
		return nil, err
	}
	s := &Server{lis: lis, signer: signer, cfg: cfg, targets: map[int]string{}, extras: map[io.Closer]bool{}, holds: map[int]*hold{},
		shell: o.Shell, root: root, rootTemp: rootTemp, ctx: ctx, cancel: cancel}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

func (s *Server) Addr() string       { return s.lis.Addr().String() }
func (s *Server) Port() int          { return s.lis.Addr().(*net.TCPAddr).Port }
func (s *Server) Signer() ssh.Signer { return s.signer }

// Root 返回 Shell 模式的工作目录（非 Shell 模式为空）。
func (s *Server) Root() string { return s.root }
func (s *Server) HostPubLine() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.signer.PublicKey())))
}

type hold struct {
	ch   chan struct{}
	once sync.Once
}

func (h *hold) release() { h.once.Do(func() { close(h.ch) }) }

// HoldDirect 让到 port 的 direct-tcpip 打开挂起（不确认也不拒绝），直到调用返回的 release 或 Close。
func (s *Server) HoldDirect(port int) (release func()) {
	h := &hold{ch: make(chan struct{})}
	s.mu.Lock()
	s.holds[port] = h
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if s.holds[port] == h {
			delete(s.holds, port)
		}
		s.mu.Unlock()
		h.release()
	}
}

// HeldDirect 返回当前挂起中的 direct-tcpip 打开数。
func (s *Server) HeldDirect() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

// Close 停止监听并关闭全部连接。
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	for _, h := range s.holds {
		h.release()
	}
	s.mu.Unlock()
	s.lis.Close()
	s.DropAll()
	s.cancel() // 杀掉 Shell 模式下仍在跑的命令（整个进程组）
	s.wg.Wait()
	if s.rootTemp {
		os.RemoveAll(s.root)
	}
}

// SetTarget 让到容器内 127.0.0.1:<port> 的 direct-tcpip 转到 addr。
func (s *Server) SetTarget(port int, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets[port] = addr
}

// ServeHTTP 在随机地址起一个 httptest 服务并登记为 port 的目标；返回停止函数。
func (s *Server) ServeHTTP(port int, h http.Handler) (stop func()) {
	ts := httptest.NewServer(h)
	s.SetTarget(port, strings.TrimPrefix(ts.URL, "http://"))
	return func() {
		ts.Close()
		s.mu.Lock()
		delete(s.targets, port)
		s.mu.Unlock()
	}
}

func (s *Server) LastDirectTCPIP() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.lastDirect...)
}

// GlobalRequests 返回迄今收到的全局请求类型（按到达顺序；回复之前登记）。
func (s *Server) GlobalRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.globals...)
}

// ChannelOpens 返回迄今收到的通道打开类型（按到达顺序；接受 / 拒绝之前登记）。
func (s *Server) ChannelOpens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.opens...)
}

func (s *Server) ForwardAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forwardAddr
}

// ConnCount 返回当前存活的服务端连接数。原始连接在 accept 时就登记（早于握手完成），
// 所以客户端 Dial 返回时，该连接必然已被计入；握手失败或断开后由 handle 摘除。
func (s *Server) ConnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// OpenToClient 用最近一条已完成握手的连接向客户端发起 NewChannel（测试网关是否拒绝 x11 等类型）；
// 没有已就绪的连接时，等最近一条连接握手完成（卡住则超时）。
func (s *Server) OpenToClient(chType string, extra []byte) error {
	e, err := s.latestConn()
	if err != nil {
		return err
	}
	ch, reqs, err := e.sconn.OpenChannel(chType, extra)
	if err != nil {
		return err
	}
	go ssh.DiscardRequests(reqs)
	return ch.Close()
}

// SendGlobal 用最近一条已完成握手的连接向客户端发全局请求（测试网关的全局请求方向白名单）。
func (s *Server) SendGlobal(name string, wantReply bool, payload []byte) (bool, error) {
	e, err := s.latestConn()
	if err != nil {
		return false, err
	}
	ok, _, err := e.sconn.SendRequest(name, wantReply, payload)
	return ok, err
}

// latestConn 返回最近一条已完成握手的连接；没有已就绪的连接时，等最近一条连接握手完成（卡住则超时）。
func (s *Server) latestConn() (*connEntry, error) {
	s.mu.Lock()
	var e *connEntry
	for i := len(s.conns) - 1; i >= 0 && e == nil; i-- {
		select { // 不阻塞地检查是否就绪
		case <-s.conns[i].ready:
			if s.conns[i].sconn != nil {
				e = s.conns[i]
			}
		default:
		}
	}
	if e == nil && len(s.conns) > 0 {
		e = s.conns[len(s.conns)-1]
	}
	s.mu.Unlock()
	if e == nil {
		return nil, errors.New("sshtest: no connection")
	}
	select { // sconn 在 NewServerConn 返回后才有；等它就绪（握手卡住则超时）
	case <-e.ready:
	case <-time.After(2 * time.Second):
		return nil, errors.New("sshtest: connection not established")
	}
	if e.sconn == nil {
		return nil, errors.New("sshtest: handshake failed")
	}
	return e, nil
}

// DropAll 关闭全部服务端连接（含握手中的原始连接与转发目标连接），模拟容器 sshd 崩溃。
func (s *Server) DropAll() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	extras := s.extras
	s.extras = map[io.Closer]bool{}
	s.mu.Unlock()
	for _, e := range conns {
		e.nc.Close()
	}
	for c := range extras {
		c.Close()
	}
}

type connEntry struct {
	nc    net.Conn
	sconn *ssh.ServerConn // ready 关闭后读取；握手失败为 nil
	ready chan struct{}
}

// track 登记需要随 Close 关闭的辅助资源；服务器已关闭则立即关闭并返回 false。
func (s *Server) track(c io.Closer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return false
	}
	s.extras[c] = true
	return true
}

func (s *Server) untrack(c io.Closer) {
	s.mu.Lock()
	delete(s.extras, c)
	s.mu.Unlock()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		nc, err := s.lis.Accept()
		if err != nil {
			return
		}
		e := &connEntry{nc: nc, ready: make(chan struct{})}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			nc.Close()
			return
		}
		s.conns = append(s.conns, e)
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.handle(e)
		}()
	}
}

type session struct {
	srv            *Server
	conn           *ssh.ServerConn
	agentRequested bool
}

func (s *Server) handle(e *connEntry) {
	defer func() {
		e.nc.Close()
		s.mu.Lock()
		for i, c := range s.conns {
			if c == e {
				s.conns = append(s.conns[:i], s.conns[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()
	sconn, chans, reqs, err := ssh.NewServerConn(e.nc, s.cfg)
	if err != nil {
		close(e.ready)
		return
	}
	e.sconn = sconn
	close(e.ready)
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.globalRequests(sconn, reqs) }()
	for nc := range chans {
		s.mu.Lock()
		s.opens = append(s.opens, nc.ChannelType())
		s.mu.Unlock()
		switch nc.ChannelType() {
		case "session":
			ch, chReqs, err := nc.Accept()
			if err != nil {
				continue
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); (&session{srv: s, conn: sconn}).run(ch, chReqs) }()
		case "direct-tcpip": // 异步：挂起或慢速的目标不挡同一连接上的其它通道
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.directTCPIP(nc) }()
		default:
			nc.Reject(ssh.UnknownChannelType, "sshtest: unsupported channel "+nc.ChannelType())
		}
	}
}

type channelOpenDirectMsg struct {
	Raddr string
	Rport uint32
	Laddr string
	Lport uint32
}

func (s *Server) directTCPIP(nc ssh.NewChannel) {
	extra := nc.ExtraData()
	s.mu.Lock()
	s.lastDirect = append([]byte(nil), extra...)
	s.mu.Unlock()
	var msg channelOpenDirectMsg
	if err := ssh.Unmarshal(extra, &msg); err != nil {
		nc.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
		return
	}
	s.mu.Lock()
	h := s.holds[int(msg.Rport)]
	if h != nil {
		s.held++
	}
	s.mu.Unlock()
	if h != nil {
		<-h.ch
		s.mu.Lock()
		s.held--
		s.mu.Unlock()
	}
	s.mu.Lock()
	target, ok := s.targets[int(msg.Rport)]
	s.mu.Unlock()
	if !ok {
		nc.Reject(ssh.ConnectionFailed, fmt.Sprintf("connect to %s:%d refused", msg.Raddr, msg.Rport))
		return
	}
	dst, err := net.Dial("tcp", target)
	if err != nil {
		nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		dst.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	s.startPipe(ch, dst)
}

// startPipe 在 wg 内双向拷贝 ch 与 c；任一方向结束后做半关闭，两个方向都结束才关闭两端。
// c 登记为 extra，Close/DropAll 会关闭它，因此 pipe goroutine 不会在 Close 之后残留。
func (s *Server) startPipe(ch ssh.Channel, c net.Conn) {
	if !s.track(c) {
		ch.Close()
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		done := make(chan struct{}, 2)
		go func() { io.Copy(ch, c); ch.CloseWrite(); done <- struct{}{} }()
		go func() {
			io.Copy(c, ch)
			if tc, ok := c.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
			done <- struct{}{}
		}()
		<-done
		<-done
		ch.Close()
		c.Close()
		s.untrack(c)
	}()
}

type tcpipForwardMsg struct {
	Addr string
	Port uint32
}

type forwardedTCPIPMsg struct {
	Addr     string
	Port     uint32
	OrigAddr string
	OrigPort uint32
}

func (s *Server) globalRequests(sconn *ssh.ServerConn, reqs <-chan *ssh.Request) {
	listeners := map[uint32]net.Listener{} // 实际监听端口 → 监听器
	// 连接结束：关闭其全部监听，并清掉指向它们的 ForwardAddr
	defer func() {
		for _, l := range listeners {
			addr := l.Addr().String()
			l.Close()
			s.untrack(l)
			s.mu.Lock()
			if s.forwardAddr == addr {
				s.forwardAddr = ""
			}
			s.mu.Unlock()
		}
	}()
	for req := range reqs {
		s.mu.Lock()
		s.globals = append(s.globals, req.Type)
		s.mu.Unlock()
		switch req.Type {
		case "tcpip-forward":
			var m tcpipForwardMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				req.Reply(false, nil)
				continue
			}
			l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(m.Port))))
			if err != nil || !s.track(l) {
				if err == nil {
					l.Close()
				}
				req.Reply(false, nil)
				continue
			}
			port := uint32(l.Addr().(*net.TCPAddr).Port)
			listeners[port] = l
			s.mu.Lock()
			s.forwardAddr = l.Addr().String()
			s.mu.Unlock()
			if m.Port == 0 {
				req.Reply(true, ssh.Marshal(struct{ Port uint32 }{port}))
			} else {
				req.Reply(true, nil)
			}
			s.wg.Add(1)
			go func(l net.Listener, bind string, port uint32) {
				defer s.wg.Done()
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					ra := c.RemoteAddr().(*net.TCPAddr)
					ch, chReqs, err := sconn.OpenChannel("forwarded-tcpip", ssh.Marshal(forwardedTCPIPMsg{Addr: bind, Port: port, OrigAddr: ra.IP.String(), OrigPort: uint32(ra.Port)}))
					if err != nil {
						c.Close()
						continue
					}
					go ssh.DiscardRequests(chReqs)
					s.startPipe(ch, c)
				}
			}(l, m.Addr, port)
		case "cancel-tcpip-forward":
			var m tcpipForwardMsg
			var l net.Listener
			ok := false
			if err := ssh.Unmarshal(req.Payload, &m); err == nil {
				l, ok = listeners[m.Port]
			}
			if !ok {
				req.Reply(false, nil)
				continue
			}
			addr := l.Addr().String()
			l.Close()
			s.untrack(l)
			delete(listeners, m.Port)
			s.mu.Lock()
			if s.forwardAddr == addr {
				s.forwardAddr = ""
			}
			s.mu.Unlock()
			req.Reply(true, nil)
		case "keepalive@openssh.com":
			req.Reply(true, nil)
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

type execMsg struct{ Command string }
type subsystemMsg struct{ Name string }

// serviceReqs 在 exec/shell/subsystem 启动后继续处理通道请求（window-change / signal / env 回 true，其余回 false）。
func serviceReqs(reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.WantReply {
			switch req.Type {
			case "window-change", "signal", "env":
				req.Reply(true, nil)
			default:
				req.Reply(false, nil)
			}
		}
	}
}

func (x *session) run(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "pty-req", "env", "window-change":
			req.Reply(true, nil)
		case "auth-agent-req@openssh.com":
			x.agentRequested = true
			req.Reply(true, nil)
		case "exec":
			var m execMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			go serviceReqs(reqs)
			x.finish(ch, x.exec(ch, m.Command))
			return
		case "shell":
			req.Reply(true, nil)
			go serviceReqs(reqs)
			sc := bufio.NewScanner(ch)
			for sc.Scan() {
				fmt.Fprintf(ch, "shell:%s\n", sc.Text())
			}
			x.finish(ch, 0)
			return
		case "subsystem":
			var m subsystemMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil || m.Name != "sftp" {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			go serviceReqs(reqs)
			fmt.Fprint(ch, "SFTP-FAKE\n")
			x.finish(ch, 0)
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

// finish 按 OpenSSH 的顺序收尾：数据已写完 → exit-status → EOF → （调用方 defer）Close。
func (x *session) finish(ch ssh.Channel, status uint32) {
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	ch.CloseWrite()
}

func (x *session) exec(ch ssh.Channel, cmd string) uint32 {
	if x.srv.shell {
		return x.srv.runShell(ch, cmd)
	}
	switch { // 原回显模式不变
	case strings.HasPrefix(cmd, "echo "):
		fmt.Fprint(ch, strings.TrimPrefix(cmd, "echo ")+"\n")
	case strings.HasPrefix(cmd, "exit "):
		n, err := strconv.Atoi(strings.TrimPrefix(cmd, "exit "))
		if err != nil || n < 0 || n > 255 {
			return 255
		}
		return uint32(n)
	case cmd == "cat":
		io.Copy(ch, ch)
	case strings.HasPrefix(cmd, "stderr "):
		fmt.Fprint(ch.Stderr(), strings.TrimPrefix(cmd, "stderr "))
	case cmd == "agent":
		if !x.agentRequested {
			fmt.Fprint(ch, "agent:none\n")
			return 0
		}
		ach, areqs, err := x.conn.OpenChannel("auth-agent@openssh.com", nil)
		if err != nil {
			fmt.Fprintf(ch, "agent:error %v\n", err)
			return 1
		}
		go ssh.DiscardRequests(areqs)
		type listRes struct {
			keys []*agent.Key
			err  error
		}
		resc := make(chan listRes, 1)
		go func() { k, err := agent.NewClient(ach).List(); resc <- listRes{k, err} }()
		var keys []*agent.Key
		select {
		case r := <-resc:
			keys, err = r.keys, r.err
		case <-time.After(5 * time.Second):
			err = errors.New("agent list timeout")
		}
		ach.Close()
		if err != nil {
			fmt.Fprintf(ch, "agent:error %v\n", err)
			return 1
		}
		fmt.Fprintf(ch, "agent:%d\n", len(keys))
	case cmd == "hostkeys":
		x.conn.SendRequest("hostkeys-00@openssh.com", false, ssh.Marshal(struct{ K []byte }{x.srv.signer.PublicKey().Marshal()}))
		fmt.Fprint(ch, "sent\n")
	case strings.HasPrefix(cmd, "big "):
		n, _ := strconv.Atoi(strings.TrimPrefix(cmd, "big "))
		writeN(ch, n)
	case strings.HasPrefix(cmd, "bigerr "):
		n, _ := strconv.Atoi(strings.TrimPrefix(cmd, "bigerr "))
		writeN(ch.Stderr(), n)
	default:
		fmt.Fprintf(ch, "exec:%s\n", cmd)
	}
	return 0
}

// runShell 用 /bin/sh -c 真执行命令：stdin / stdout / stderr 与通道对接，返回退出码；服务器 Close 时命令被杀（ctx）。
// 命令跑在独立进程组里，取消时整组 SIGKILL：只杀 /bin/sh 的话，fork 出来的子进程（Linux dash 下的 sleep、
// 未重定向的后台进程）仍占着 stdout 管道，Wait 等不到 EOF，Close 就卡住。WaitDelay 兜底逃出进程组的孙进程。
func (s *Server) runShell(ch ssh.Channel, cmd string) uint32 {
	c := exec.CommandContext(s.ctx, "/bin/sh", "-c", cmd)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 2 * time.Second
	c.Dir = s.root
	c.Env = append(os.Environ(), "HOME="+s.root, "SPARK_TEST_ROOT="+s.root)
	c.Stdout, c.Stderr = ch, ch.Stderr()
	stdin, err := c.StdinPipe()
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "sshtest: %v\n", err)
		return 255
	}
	if err := c.Start(); err != nil {
		fmt.Fprintf(ch.Stderr(), "sshtest: %v\n", err)
		return 127
	}
	go func() { io.Copy(stdin, ch); stdin.Close() }()
	err = c.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() { // 被信号杀：128+信号
			return 128 + uint32(ws.Signal())
		}
		return uint32(ee.ExitCode())
	default:
		return 255
	}
}

// writeN 写 n 个 'x'（64 KiB 一块），命令返回后 session 立即 exit：用来验证网关不截断尾部数据。
func writeN(w io.Writer, n int) {
	block := []byte(strings.Repeat("x", 65536))
	for n > 0 {
		k := len(block)
		if n < k {
			k = n
		}
		if _, err := w.Write(block[:k]); err != nil {
			return
		}
		n -= k
	}
}
