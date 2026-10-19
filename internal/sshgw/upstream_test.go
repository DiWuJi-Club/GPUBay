package sshgw_test

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/sshgw"
)

// silentUpstream 只 accept TCP、从不说 SSH；closed 在网关一侧关闭连接后收到信号。
func silentUpstream(t *testing.T) (port int, closed <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan struct{}, 16)
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
			go func() { io.Copy(io.Discard, c); ch <- struct{}{} }()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
	})
	return ln.Addr().(*net.TCPAddr).Port, ch
}

func TestUpstreamSilentHandshakeTimesOut(t *testing.T) {
	port, _ := silentUpstream(t)
	e := newEnvWith(t, func(c *sshgw.Config) { c.UpstreamPort = port; c.UpstreamTimeout = 300 * time.Millisecond })
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	type result struct {
		stderr string
		err    error
	}
	res := make(chan result, 1)
	go func() { _, se, err := run(t, c, "echo x"); res <- result{se, err} }()
	select {
	case r := <-res:
		if r.err == nil || !strings.Contains(r.stderr, sshgw.MsgUpstreamUnreachable) {
			t.Fatalf("silent upstream: err=%v stderr=%q", r.err, r.stderr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("silent upstream: no answer within 2s (handshake deadline 300ms)")
	}
	waitFor(t, "unreachable alert", func() bool { _, _, a := e.store.snapshot(); return len(a) == 1 && a[0] == "unreachable:"+instA })
}

func TestUpstreamDialAbortedWhenClientLeaves(t *testing.T) {
	port, closed := silentUpstream(t)
	e := newEnvWith(t, func(c *sshgw.Config) { c.UpstreamPort = port; c.UpstreamTimeout = time.Minute })
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	time.Sleep(100 * time.Millisecond) // 网关已在拨上游、卡在握手
	c.Close()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream handshake not aborted after client left")
	}
	time.Sleep(50 * time.Millisecond)
	if _, _, a := e.store.snapshot(); len(a) != 0 {
		t.Fatalf("client leaving must not raise alerts: %v", a)
	}
}

func TestUpstreamHostKeyTOFU(t *testing.T) {
	e := newEnv(t)
	info := runningInfo(e, instA, 7, "pw-correct-16chr")
	info.Instance.SSHHostPubkey = ""
	e.store.set(instA, info)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if out, _, err := run(t, c, "echo tofu"); err != nil || out != "tofu\n" {
		t.Fatalf("tofu exec = %q %v", out, err)
	}
	if got := e.store.hostPub(instA); got != e.upstream.HostPubLine() {
		t.Fatalf("host key not written back: %q", got)
	}
}

func TestUpstreamHostKeyTOFURaceLoser(t *testing.T) {
	e := newEnv(t)
	blank := func() {
		info := runningInfo(e, instA, 7, "pw-correct-16chr")
		info.Instance.SSHHostPubkey = ""
		e.store.set(instA, info)
	}
	// 其它副本抢先写回了同一把公钥：复读一致 → 接受
	blank()
	e.store.mu.Lock()
	e.store.tofuRace = e.upstream.HostPubLine()
	e.store.mu.Unlock()
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if out, _, err := run(t, c, "echo same"); err != nil || out != "same\n" {
		t.Fatalf("race loser, same key: %q %v", out, err)
	}
	// 其它副本写回的是另一把公钥：复读不一致 → 拒绝
	blank()
	e.store.mu.Lock()
	e.store.tofuRace = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey())))
	e.store.mu.Unlock()
	c = e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if _, stderr, err := run(t, c, "echo other"); err == nil || !strings.Contains(stderr, sshgw.MsgUpstreamUnreachable) {
		t.Fatalf("race loser, other key: err=%v stderr=%q", err, stderr)
	}
}

func TestUpstreamHostKeyMismatch(t *testing.T) {
	e := newEnv(t)
	info := runningInfo(e, instA, 7, "pw-correct-16chr")
	info.Instance.SSHHostPubkey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey())))
	e.store.set(instA, info)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if _, stderr, err := run(t, c, "echo x"); err == nil || !strings.Contains(stderr, sshgw.MsgUpstreamUnreachable) {
		t.Fatalf("mismatched host key: err=%v stderr=%q", err, stderr)
	}
}

// 裁定 D24：上游发起的通道（-R 的 forwarded-tcpip）里，用户 → 容器 仍计为 up。
func TestByteCountersOnUpstreamInitiatedChannel(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	ln, err := c.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const toUser, fromUser = 1000, 50000
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		io.ReadFull(conn, make([]byte, toUser))
		conn.Write(bytes.Repeat([]byte("u"), fromUser))
		conn.Close()
	}()
	var addr string
	waitFor(t, "remote forward", func() bool { addr = e.upstream.ForwardAddr(); return addr != "" })
	in, err := net.Dial("tcp", addr) // 容器内进程连到 -R 端口
	if err != nil {
		t.Fatal(err)
	}
	in.Write(bytes.Repeat([]byte("c"), toUser))
	got, _ := io.ReadAll(in)
	in.Close()
	if len(got) != fromUser {
		t.Fatalf("container side read %d bytes, want %d", len(got), fromUser)
	}
	c.Close()
	waitFor(t, "session closed", func() bool { _, closed, _ := e.store.snapshot(); return len(closed) == 1 })
	_, closed, _ := e.store.snapshot()
	if b := closed[1]; b[0] != fromUser || b[1] != toUser {
		t.Fatalf("bytes up/down = %v, want [%d %d]", b, fromUser, toUser)
	}
}

func TestUpstreamInvalidLanIPNotDialed(t *testing.T) {
	e := newEnv(t)
	info := runningInfo(e, instA, 7, "pw-correct-16chr")
	info.MachineLanIP = ""
	info.Instance.SSHHostPubkey = ""
	e.store.set(instA, info)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if _, stderr, err := run(t, c, "echo x"); err == nil || !strings.Contains(stderr, sshgw.MsgUpstreamUnreachable) {
		t.Fatalf("empty lan ip: err=%v stderr=%q", err, stderr)
	}
	if n := e.upstream.ConnCount(); n != 0 {
		t.Fatalf("gateway dialed with empty lan ip: %d upstream conns", n)
	}
	if got := e.store.hostPub(instA); got != "" {
		t.Fatalf("tofu wrote %q with empty lan ip", got)
	}
}

// 一个迟迟得不到确认的 -L 通道不能挡住同一连接上的其它通道与已开的 shell。
func TestSlowChannelOpenDoesNotBlockOthers(t *testing.T) {
	e := newEnv(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	e.upstream.SetTarget(9002, echo.Addr().String())
	release := e.upstream.HoldDirect(9001)
	defer release()

	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	sh, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	stdin, _ := sh.StdinPipe()
	stdout, _ := sh.StdoutPipe()
	if err := sh.Shell(); err != nil {
		t.Fatal(err)
	}
	roundTrip := func(line string) {
		t.Helper()
		done := make(chan string, 1)
		go func() {
			buf := make([]byte, len("shell:"+line+"\n"))
			io.ReadFull(stdout, buf)
			done <- string(buf)
		}()
		stdin.Write([]byte(line + "\n"))
		select {
		case got := <-done:
			if got != "shell:"+line+"\n" {
				t.Fatalf("shell echo = %q", got)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("shell round trip %q blocked", line)
		}
	}
	roundTrip("a")

	pending := make(chan error, 1)
	go func() {
		conn, err := c.Dial("tcp", "127.0.0.1:9001")
		if err == nil {
			conn.Close()
		}
		pending <- err
	}()
	waitFor(t, "first -L held at upstream", func() bool { return e.upstream.HeldDirect() == 1 })

	roundTrip("b")
	second := make(chan error, 1)
	go func() {
		conn, err := c.Dial("tcp", "127.0.0.1:9002")
		if err != nil {
			second <- err
			return
		}
		defer conn.Close()
		conn.Write([]byte("ping"))
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
			second <- errors.New("echo mismatch: " + string(buf))
			return
		}
		second <- nil
	}()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second -L: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second -L blocked behind the pending one")
	}
	select {
	case <-pending:
		t.Fatal("first -L should still be pending")
	default:
	}
	release()
	select {
	case <-pending: // 放行后目标未登记 → 被拒，也算有结果
	case <-time.After(3 * time.Second):
		t.Fatal("first -L never resolved after release")
	}
}

// 全局请求方向白名单：容器发来的 tcpip-forward 不转给用户，保活照转。
func TestUpstreamGlobalRequestsDirectional(t *testing.T) {
	e := newEnv(t)
	nc, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(nc, e.addr, &ssh.ClientConfig{User: instA, Auth: []ssh.AuthMethod{ssh.Password("pw-correct-16chr")},
		HostKeyCallback: ssh.FixedHostKey(e.hostKey.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	go func() {
		for r := range reqs {
			mu.Lock()
			seen = append(seen, r.Type)
			mu.Unlock()
			if r.WantReply {
				r.Reply(true, nil)
			}
		}
	}()
	noReqs := make(chan *ssh.Request)
	c := ssh.NewClient(cc, chans, noReqs)
	t.Cleanup(func() { c.Close(); close(noReqs) })
	if out, _, err := run(t, c, "echo up"); err != nil || out != "up\n" { // 上游已连通
		t.Fatalf("exec = %q %v", out, err)
	}
	if ok, err := e.upstream.SendGlobal("tcpip-forward", true, ssh.Marshal(struct {
		Addr string
		Port uint32
	}{"127.0.0.1", 0})); ok || err != nil {
		t.Fatalf("upstream tcpip-forward relayed: ok=%v err=%v", ok, err)
	}
	if ok, err := e.upstream.SendGlobal("keepalive@openssh.com", true, nil); !ok || err != nil {
		t.Fatalf("upstream keepalive: ok=%v err=%v", ok, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "keepalive@openssh.com" {
		t.Fatalf("client saw global requests %v", seen)
	}
}
