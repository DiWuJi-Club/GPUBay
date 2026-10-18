// Package sshtest 是用 x/crypto/ssh 写的假「容器 sshd」：只接受平台上游公钥，支持 session（exec / shell / sftp）、
// direct-tcpip（按登记端口转发）、tcpip-forward 回连、agent 转发、hostkeys-00 推送，以及若干测试控制钩子。
// 供 sshgw / upstream / ingress 单元测试、Go 集成测试与 e2e-server 共用；生产二进制不引用它。
package sshtest

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Options struct {
	AuthorizedKey ssh.PublicKey // 允许以 root 登录的公钥（平台上游公钥）
	HostKey       ssh.Signer    // nil → 生成 ed25519
}

type Server struct {
	lis    net.Listener
	signer ssh.Signer
	cfg    *ssh.ServerConfig

	mu          sync.Mutex
	targets     map[int]string // direct-tcpip 目标端口 → 实际拨号地址
	lastDirect  []byte
	forwardAddr string
	conns       []*ssh.ServerConn
	closed      bool
	wg          sync.WaitGroup
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
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{lis: lis, signer: signer, cfg: cfg, targets: map[int]string{}}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

func (s *Server) Addr() string       { return s.lis.Addr().String() }
func (s *Server) Port() int          { return s.lis.Addr().(*net.TCPAddr).Port }
func (s *Server) Signer() ssh.Signer { return s.signer }
func (s *Server) HostPubLine() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.signer.PublicKey())))
}

// Close 停止监听并关闭全部连接。
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.lis.Close()
	s.DropAll()
	s.wg.Wait()
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

func (s *Server) ForwardAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forwardAddr
}

func (s *Server) ConnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// OpenToClient 用最近一条连接向客户端发起 NewChannel（测试网关是否拒绝 x11 等类型）。
func (s *Server) OpenToClient(chType string, extra []byte) error {
	s.mu.Lock()
	var c *ssh.ServerConn
	if n := len(s.conns); n > 0 {
		c = s.conns[n-1]
	}
	s.mu.Unlock()
	if c == nil {
		return errors.New("sshtest: no connection")
	}
	ch, reqs, err := c.OpenChannel(chType, extra)
	if err != nil {
		return err
	}
	go ssh.DiscardRequests(reqs)
	return ch.Close()
}

// DropAll 关闭全部服务端连接（模拟容器 sshd 崩溃）。
func (s *Server) DropAll() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		nc, err := s.lis.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(nc)
		}()
	}
}

type session struct {
	srv            *Server
	conn           *ssh.ServerConn
	agentRequested bool
}

func (s *Server) handle(nc net.Conn) {
	sconn, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		nc.Close()
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		sconn.Close()
		return
	}
	s.conns = append(s.conns, sconn)
	s.mu.Unlock()
	defer func() {
		sconn.Close()
		s.mu.Lock()
		for i, c := range s.conns {
			if c == sconn {
				s.conns = append(s.conns[:i], s.conns[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()
	go s.globalRequests(sconn, reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			ch, chReqs, err := nc.Accept()
			if err != nil {
				continue
			}
			go (&session{srv: s, conn: sconn}).run(ch, chReqs)
		case "direct-tcpip":
			s.directTCPIP(nc)
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
	go pipe(ch, dst)
}

// pipe 双向拷贝；任一方向结束即关闭两端。
func pipe(ch ssh.Channel, c net.Conn) {
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
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	for req := range reqs {
		switch req.Type {
		case "tcpip-forward":
			var m tcpipForwardMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				req.Reply(false, nil)
				continue
			}
			l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(m.Port))))
			if err != nil {
				req.Reply(false, nil)
				continue
			}
			listeners = append(listeners, l)
			port := uint32(l.Addr().(*net.TCPAddr).Port)
			s.mu.Lock()
			s.forwardAddr = l.Addr().String()
			s.mu.Unlock()
			if m.Port == 0 {
				req.Reply(true, ssh.Marshal(struct{ Port uint32 }{port}))
			} else {
				req.Reply(true, nil)
			}
			go func(l net.Listener, bind string, port uint32) {
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
					go pipe(ch, c)
				}
			}(l, m.Addr, port)
		case "cancel-tcpip-forward":
			for _, l := range listeners {
				l.Close()
			}
			listeners = nil
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
			x.exit(ch, x.exec(ch, m.Command))
			return
		case "shell":
			req.Reply(true, nil)
			sc := bufio.NewScanner(ch)
			for sc.Scan() {
				fmt.Fprintf(ch, "shell:%s\n", sc.Text())
			}
			x.exit(ch, 0)
			return
		case "subsystem":
			var m subsystemMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil || m.Name != "sftp" {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			fmt.Fprint(ch, "SFTP-FAKE\n")
			x.exit(ch, 0)
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (x *session) exit(ch ssh.Channel, status uint32) {
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
}

func (x *session) exec(ch ssh.Channel, cmd string) uint32 {
	switch {
	case strings.HasPrefix(cmd, "echo "):
		fmt.Fprint(ch, strings.TrimPrefix(cmd, "echo ")+"\n")
	case strings.HasPrefix(cmd, "exit "):
		n, _ := strconv.Atoi(strings.TrimPrefix(cmd, "exit "))
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
		keys, err := agent.NewClient(ach).List()
		ach.Close()
		if err != nil {
			fmt.Fprintf(ch, "agent:error %v\n", err)
			return 1
		}
		fmt.Fprintf(ch, "agent:%d\n", len(keys))
	case cmd == "hostkeys":
		x.conn.SendRequest("hostkeys-00@openssh.com", false, x.srv.signer.PublicKey().Marshal())
		fmt.Fprint(ch, "sent\n")
	default:
		fmt.Fprintf(ch, "exec:%s\n", cmd)
	}
	return 0
}
