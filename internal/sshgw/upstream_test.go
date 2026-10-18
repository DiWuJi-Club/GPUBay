package sshgw_test

import (
	"bytes"
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
