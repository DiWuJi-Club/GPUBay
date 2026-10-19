package sshgw_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"spark-cloud/internal/rental"
	"spark-cloud/internal/sshgw"
)

type directMsg struct {
	Raddr string
	Rport uint32
	Laddr string
	Lport uint32
}

func TestLocalForwardPassesExtraDataVerbatim(t *testing.T) {
	e := newEnv(t)
	stop := e.upstream.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "lab") }))
	defer stop()
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	payload := ssh.Marshal(directMsg{Raddr: "127.0.0.1", Rport: 8888, Laddr: "203.0.113.7", Lport: 51515})
	ch, reqs, err := c.OpenChannel("direct-tcpip", payload)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	fmt.Fprint(ch, "GET / HTTP/1.0\r\nHost: x\r\n\r\n")
	body, _ := io.ReadAll(ch)
	if !strings.HasSuffix(string(body), "lab") {
		t.Fatalf("body = %q", body)
	}
	if got := e.upstream.LastDirectTCPIP(); !bytes.Equal(got, payload) {
		t.Fatalf("extra data rewritten: got %x want %x", got, payload)
	}
	// 上游拒绝（未登记端口）的原因原样回传
	_, _, err = c.OpenChannel("direct-tcpip", ssh.Marshal(directMsg{Raddr: "127.0.0.1", Rport: 9, Laddr: "x", Lport: 1}))
	var oce *ssh.OpenChannelError
	if !errors.As(err, &oce) || oce.Reason != ssh.ConnectionFailed || !strings.Contains(oce.Message, "refused") {
		t.Fatalf("rejected = %v", err)
	}
}

func TestRemoteForwardAndAgentForwarding(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	ln, err := c.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(conn, conn)
		conn.Close()
	}()
	back, err := net.Dial("tcp", e.upstream.ForwardAddr())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(back, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(back, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("remote forward echo = %q %v", buf, err)
	}
	back.Close()

	keyring := agent.NewKeyring()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	keyring.Add(agent.AddedKey{PrivateKey: priv})
	if err := agent.ForwardToAgent(c, keyring); err != nil {
		t.Fatal(err)
	}
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := agent.RequestAgentForwarding(sess); err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("agent")
	if err != nil || string(out) != "agent:1\n" {
		t.Fatalf("agent through gateway = %q %v", out, err)
	}
}

func TestSftpExitStatusAndStderr(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatal(err)
	}
	// 裁定 D2：RequestSubsystem 后不调用 Wait，读到 EOF 即可
	if out, err := io.ReadAll(stdout); err != nil || string(out) != "SFTP-FAKE\n" {
		t.Fatalf("sftp = %q %v", out, err)
	}
	sess.Close()
	var exitErr *ssh.ExitError
	if _, _, err := run(t, c, "exit 3"); !errors.As(err, &exitErr) || exitErr.ExitStatus() != 3 {
		t.Fatalf("exit status = %v", err)
	}
	if _, stderr, err := run(t, c, "stderr oops"); err != nil || stderr != "oops" {
		t.Fatalf("stderr = %q %v", stderr, err)
	}
	sess2, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	sess2.Stdin = strings.NewReader(strings.Repeat("x", 100000))
	var big bytes.Buffer
	sess2.Stdout = &big
	if err := sess2.Run("cat"); err != nil || big.Len() != 100000 {
		t.Fatalf("cat 100k = %d %v", big.Len(), err)
	}
	c.Close()
	waitFor(t, "closed", func() bool { _, closed, _ := e.store.snapshot(); return len(closed) == 1 })
	_, closed, _ := e.store.snapshot()
	if b := closed[1]; b[0] < 100000 || b[1] < 100000 {
		t.Fatalf("bytes up/down = %v", b)
	}
}

// rawClient 直接用 NewClientConn 连网关（可选经 via 中继），自己消费网关发来的全局请求（回 false），并记录类型。
type rawClient struct {
	*ssh.Client
	mu   sync.Mutex
	seen []string
}

func (r *rawClient) globals() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func (e *env) rawDial(t *testing.T, addr string) *rawClient {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, chans, reqs, err := ssh.NewClientConn(nc, addr, &ssh.ClientConfig{User: instA, Auth: []ssh.AuthMethod{ssh.Password("pw-correct-16chr")},
		HostKeyCallback: ssh.FixedHostKey(e.hostKey.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r := &rawClient{}
	go func() {
		for req := range reqs {
			r.mu.Lock()
			r.seen = append(r.seen, req.Type)
			r.mu.Unlock()
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}()
	noReqs := make(chan *ssh.Request)
	r.Client = ssh.NewClient(conn, chans, noReqs)
	t.Cleanup(func() { r.Close(); close(noReqs) })
	return r
}

// 默认拒绝：两个方向不在白名单的通道类型、用户发来的白名单外全局请求；hostkeys-00 被丢弃不到用户。
func TestPolicyRejectsAndDropsHostkeys(t *testing.T) {
	e := newEnv(t)
	c := e.rawDial(t, e.addr)
	var oce *ssh.OpenChannelError
	denied := []string{"x11", "direct-streamlocal@openssh.com", "tun@openssh.com", "forwarded-tcpip",
		"auth-agent@openssh.com", "forwarded-streamlocal@openssh.com", "session-bogus"}
	for _, typ := range denied {
		// sshtest 对不支持的类型也回 UnknownChannelType：必须用消息与上游记录区分「网关拒绝」与「上游拒绝」
		if _, _, err := c.OpenChannel(typ, nil); !errors.As(err, &oce) || oce.Reason != ssh.UnknownChannelType ||
			!strings.HasPrefix(oce.Message, "channel type not allowed") {
			t.Errorf("client %s = %v", typ, err)
		}
	}
	// 被转发的打开要等上游回应（上游先登记再回应），所以 OpenChannel 返回时若被转发必然已登记
	for _, typ := range e.upstream.ChannelOpens() {
		if slices.Contains(denied, typ) {
			t.Fatalf("denied channel %s reached upstream (%v)", typ, e.upstream.ChannelOpens())
		}
	}
	for _, typ := range []string{"streamlocal-forward@openssh.com", "cancel-streamlocal-forward@openssh.com", "bogus@example.com"} {
		if ok, _, err := c.SendRequest(typ, true, nil); ok || err != nil {
			t.Errorf("client global %s = %v %v", typ, ok, err)
		}
	}
	// 白名单内的照转：上游回 true
	if ok, _, err := c.SendRequest("keepalive@openssh.com", true, nil); !ok || err != nil {
		t.Fatalf("keepalive forwarded = %v %v", ok, err)
	}
	// 网关逐个同步转发全局请求：keepalive 已到上游，则此前被拒的若被转发也必然已到
	got := e.upstream.GlobalRequests()
	if !slices.Contains(got, "keepalive@openssh.com") {
		t.Fatalf("upstream global requests %v: keepalive missing", got)
	}
	for _, typ := range got {
		if strings.Contains(typ, "streamlocal") || typ == "bogus@example.com" {
			t.Fatalf("refused global request %s reached upstream (%v)", typ, got)
		}
	}

	out, err := func() (string, error) {
		s, err := c.NewSession()
		if err != nil {
			return "", err
		}
		defer s.Close()
		b, err := s.Output("hostkeys")
		return string(b), err
	}()
	if err != nil || out != "sent\n" {
		t.Fatalf("hostkeys exec = %q %v", out, err)
	}
	for _, typ := range []string{"x11", "forwarded-streamlocal@openssh.com", "direct-streamlocal@openssh.com", "tun@openssh.com",
		"session", "direct-tcpip", "bogus-type"} {
		if err := e.upstream.OpenToClient(typ, nil); !errors.As(err, &oce) || oce.Reason != ssh.Prohibited ||
			!strings.HasPrefix(oce.Message, "channel type not allowed") {
			t.Errorf("upstream %s = %v", typ, err)
		}
	}
	// 上游的全局请求由一个 goroutine 依次处理：keepalive 到了用户，之前的 hostkeys-00 必然已处理完
	if ok, err := e.upstream.SendGlobal("keepalive@openssh.com", true, nil); ok || err != nil { // 用户侧回 false
		t.Fatalf("upstream keepalive = %v %v", ok, err)
	}
	seen := c.globals()
	if !slices.Contains(seen, "keepalive@openssh.com") || slices.Contains(seen, "hostkeys-00@openssh.com") {
		t.Fatalf("client saw global requests %v", seen)
	}
	if n := sshgw.HostkeysDroppedForTest(e.srv); n != 1 {
		t.Fatalf("hostkeys dropped = %d", n)
	}
}

// 用户 → 容器方向：一个会话上未确认的通道打开数有上限，到上限后的新打开回 ResourceShortage，确认 / 拒绝后额度归还。
func TestPendingChannelOpensCapped(t *testing.T) {
	e := newEnv(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(conn, conn); conn.Close() }()
		}
	}()
	e.upstream.SetTarget(9002, echo.Addr().String())
	release := e.upstream.HoldDirect(9001)
	defer release()
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	n := sshgw.MaxPendingOpensForTest
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			ch, reqs, err := c.OpenChannel("direct-tcpip", ssh.Marshal(directMsg{Raddr: "127.0.0.1", Rport: 9001, Laddr: "127.0.0.1", Lport: uint32(40000 + i)}))
			if err == nil {
				go ssh.DiscardRequests(reqs)
				ch.Close()
			}
			results <- err
		}()
	}
	waitFor(t, "opens held at upstream", func() bool { return e.upstream.HeldDirect() == n })
	open9002 := func() error {
		ch, reqs, err := c.OpenChannel("direct-tcpip", ssh.Marshal(directMsg{Raddr: "127.0.0.1", Rport: 9002, Laddr: "127.0.0.1", Lport: 1}))
		if err != nil {
			return err
		}
		defer ch.Close()
		go ssh.DiscardRequests(reqs)
		ch.Write([]byte("ping"))
		buf := make([]byte, 4)
		if _, err := io.ReadFull(ch, buf); err != nil || string(buf) != "ping" {
			return fmt.Errorf("echo = %q %v", buf, err)
		}
		return nil
	}
	var oce *ssh.OpenChannelError
	if err := open9002(); !errors.As(err, &oce) || oce.Reason != ssh.ResourceShortage {
		t.Fatalf("open over cap = %v", err)
	}
	release()
	for i := 0; i < n; i++ {
		select {
		case <-results: // 目标未登记 → 被上游拒绝，也算有结果
		case <-time.After(5 * time.Second):
			t.Fatalf("held open %d never resolved", i)
		}
	}
	if err := open9002(); err != nil {
		t.Fatalf("open after release = %v", err)
	}
	if out, _, err := run(t, c, "echo ok"); err != nil || out != "ok\n" {
		t.Fatalf("session after release = %q %v", out, err)
	}
}

// 容器 → 用户方向（-R 的 forwarded-tcpip 等）：用户迟迟不回应的打开同样有上限，与用户方向分开计数。
func TestPendingUpstreamOpensCapped(t *testing.T) {
	e := newEnv(t)
	c := e.rawDial(t, e.addr)
	if out, _, err := run(t, c.Client, "echo up"); err != nil || out != "up\n" { // 上游连接已建立
		t.Fatalf("exec = %q %v", out, err)
	}
	n := sshgw.MaxPendingOpensForTest
	// 用户侧收下 forwarded-tcpip 打开但先不回应；及时取走，免得 x/crypto 的内部缓冲塞满卡住整条连接
	incoming := c.HandleChannelOpen("forwarded-tcpip")
	held := make(chan ssh.NewChannel, n+8)
	go func() {
		for nc := range incoming {
			held <- nc
		}
	}()
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() { results <- e.upstream.OpenToClient("forwarded-tcpip", nil) }()
	}
	waitFor(t, "opens held at client", func() bool { return len(held) == n })
	var oce *ssh.OpenChannelError
	over := make(chan error, 1)
	go func() { over <- e.upstream.OpenToClient("forwarded-tcpip", nil) }()
	select { // 没有上限时这次打开会和前面的一样挂在用户侧：限时，失败而不是卡死
	case err := <-over:
		if !errors.As(err, &oce) || oce.Reason != ssh.ResourceShortage || !strings.HasPrefix(oce.Message, "too many pending") {
			t.Fatalf("upstream open over cap = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream open over cap was not rejected (reached the client and is pending)")
	}
	if len(held) != n {
		t.Fatalf("open over cap reached client: %d held", len(held))
	}
	// 用户方向的额度不受影响
	if out, _, err := run(t, c.Client, "echo still"); err != nil || out != "still\n" {
		t.Fatalf("client-direction open while upstream direction capped = %q %v", out, err)
	}
	for i := 0; i < n; i++ {
		(<-held).Reject(ssh.ConnectionFailed, "held")
	}
	for i := 0; i < n; i++ {
		select {
		case err := <-results:
			if !errors.As(err, &oce) || oce.Reason != ssh.ConnectionFailed || oce.Message != "held" {
				t.Fatalf("held open result = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("held open %d never resolved", i)
		}
	}
	go func() { // 只确认、不主动关（由 OpenToClient 关）：用户先关会让上游的 Close 返回 EOF
		nc := <-held
		if _, reqs, err := nc.Accept(); err == nil {
			go ssh.DiscardRequests(reqs)
		}
	}()
	if err := e.upstream.OpenToClient("forwarded-tcpip", nil); err != nil {
		t.Fatalf("upstream open after release = %v", err)
	}
}

func TestUpstreamDropEndsSession(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stderr syncBuffer
	sess.Stderr = &stderr
	in, _ := sess.StdinPipe()
	out, _ := sess.StdoutPipe()
	if err := sess.Start("cat"); err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(in, "live")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(out, buf); err != nil || string(buf) != "live" {
		t.Fatalf("cat echo = %q %v", buf, err)
	}
	e.upstream.DropAll()
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("session ended without error after upstream drop")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end after upstream drop")
	}
	if strings.Contains(stderr.String(), sshgw.MsgDisconnected) { // 实例仍 running：上游故障不是停机，不提示停机
		t.Fatalf("stderr = %q", stderr.String())
	}
	waitFor(t, "session record closed", func() bool { _, closed, _ := e.store.snapshot(); return len(closed) == 1 && e.srv.Sessions() == 0 })
	_, closed, _ := e.store.snapshot()
	if closed[1][0] != 4 || closed[1][1] != 4 {
		t.Fatalf("bytes = %v", closed[1])
	}
}

// 到期停机时 agent 立即停容器，上游连接在 10 秒扫描之前就断了：网关复查状态，已停机则先写提示再断开（§10.4.9）。
func TestUpstreamDropAfterStopNotifies(t *testing.T) {
	for _, state := range []string{rental.StateStopped, rental.StateReleased, "missing"} {
		t.Run(state, func(t *testing.T) {
			e := newEnv(t)
			c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
			sess, err := c.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			var stderr syncBuffer
			sess.Stderr = &stderr
			in, _ := sess.StdinPipe()
			out, _ := sess.StdoutPipe()
			if err := sess.Start("cat"); err != nil {
				t.Fatal(err)
			}
			fmt.Fprint(in, "live")
			buf := make([]byte, 4)
			if _, err := io.ReadFull(out, buf); err != nil || string(buf) != "live" {
				t.Fatalf("cat echo = %q %v", buf, err)
			}
			if state == "missing" {
				e.store.mu.Lock()
				delete(e.store.infos, instA)
				e.store.mu.Unlock()
			} else {
				info := runningInfo(e, instA, 7, "pw-correct-16chr")
				info.Instance.State = state
				e.store.set(instA, info)
			}
			e.upstream.DropAll()
			done := make(chan error, 1)
			go func() { done <- sess.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("session did not end after upstream drop")
			}
			if !strings.Contains(stderr.String(), sshgw.MsgDisconnected) {
				t.Fatalf("stderr = %q", stderr.String())
			}
			waitFor(t, "session record closed", func() bool { _, closed, _ := e.store.snapshot(); return len(closed) == 1 && e.srv.Sessions() == 0 })
		})
	}
}

func TestLargeOutputNotTruncated(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.Output("big 4194304") // 上游写满 4 MiB 后立即退出：尾部不能被 Close 截断
	if err != nil || len(out) != 4194304 {
		t.Fatalf("big output = %d bytes, err %v", len(out), err)
	}
	if bytes.Count(out, []byte("x")) != len(out) {
		t.Fatal("big output contains bytes other than 'x'")
	}
	sess2, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess2.Close()
	var stdout, stderr bytes.Buffer
	sess2.Stdout, sess2.Stderr = &stdout, &stderr
	if err := sess2.Run("bigerr 1048576"); err != nil || stderr.Len() != 1048576 || stdout.Len() != 0 {
		t.Fatalf("big stderr = %d bytes (stdout %d), err %v", stderr.Len(), stdout.Len(), err)
	}
	if bytes.Count(stderr.Bytes(), []byte("x")) != stderr.Len() {
		t.Fatal("big stderr contains bytes other than 'x'")
	}
}

// freezer 是客户端与网关之间的 TCP 中继，可随时停止转发（模拟客户端网络断掉但 TCP 未关闭）。
type freezer struct {
	lis    net.Listener
	frozen atomic.Bool
	mu     sync.Mutex
	conns  []net.Conn
}

func newFreezer(t *testing.T, target string) *freezer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &freezer{lis: lis}
	go func() {
		for {
			a, err := lis.Accept()
			if err != nil {
				return
			}
			b, err := net.Dial("tcp", target)
			if err != nil {
				a.Close()
				continue
			}
			f.mu.Lock()
			f.conns = append(f.conns, a, b)
			f.mu.Unlock()
			pump := func(dst, src net.Conn) {
				buf := make([]byte, 32*1024)
				for {
					n, err := src.Read(buf)
					if err != nil {
						return
					}
					for f.frozen.Load() {
						time.Sleep(10 * time.Millisecond)
					}
					if _, err := dst.Write(buf[:n]); err != nil {
						return
					}
				}
			}
			go pump(a, b)
			go pump(b, a)
		}
	}()
	t.Cleanup(func() {
		lis.Close()
		f.frozen.Store(false) // 让卡在冻结循环里的 pump 退出
		f.mu.Lock()
		for _, c := range f.conns {
			c.Close()
		}
		f.mu.Unlock()
	})
	return f
}

func TestKeepaliveClosesFrozenClient(t *testing.T) {
	e := newEnvWith(t, func(c *sshgw.Config) {
		c.KeepaliveInterval, c.KeepaliveTimeout = 100*time.Millisecond, 400*time.Millisecond
	})
	fz := newFreezer(t, e.addr)
	c := e.rawDial(t, fz.lis.Addr().String())
	if out, _, err := run(t, c.Client, "echo alive"); err != nil || out != "alive\n" {
		t.Fatalf("before freeze = %q %v", out, err)
	}
	// 健康连接不被误杀：等网关发出的保活超过连续未回应上限（每次都被回应，计数清零）
	count := func() int {
		n := 0
		for _, typ := range c.globals() {
			if typ == "keepalive@openssh.com" {
				n++
			}
		}
		return n
	}
	waitFor(t, "several keepalives answered", func() bool { return count() >= 6 })
	if e.srv.Sessions() != 1 {
		t.Fatalf("healthy session closed by keepalive: %d", e.srv.Sessions())
	}
	if out, _, err := run(t, c.Client, "echo still"); err != nil || out != "still\n" {
		t.Fatalf("after keepalives = %q %v", out, err)
	}
	fz.frozen.Store(true)
	waitFor(t, "frozen client dropped", func() bool { return e.srv.Sessions() == 0 })
	waitFor(t, "session record closed", func() bool { _, closed, _ := e.store.snapshot(); return len(closed) == 1 })
}
