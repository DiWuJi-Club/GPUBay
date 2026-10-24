package sshtest_test

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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"spark-cloud/internal/access/sshtest"
)

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newServer(t *testing.T, up ssh.Signer) *sshtest.Server {
	t.Helper()
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func dial(t *testing.T, srv *sshtest.Server, signer ssh.Signer) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", srv.Addr(), &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(srv.Signer().PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func output(t *testing.T, c *ssh.Client, cmd string) (string, error) {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.Output(cmd)
	return string(out), err
}

func TestSessionExecShellSftp(t *testing.T) {
	up := newSigner(t)
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if !strings.HasPrefix(srv.HostPubLine(), "ssh-ed25519 ") {
		t.Fatalf("host pub = %q", srv.HostPubLine())
	}
	// 错误的客户端密钥被拒
	if _, err := ssh.Dial("tcp", srv.Addr(), &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(newSigner(t))}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second}); err == nil {
		t.Fatal("wrong key accepted")
	}
	c := dial(t, srv, up)
	if out, err := output(t, c, "echo hi"); err != nil || out != "hi\n" {
		t.Fatalf("echo = %q %v", out, err)
	}
	var exitErr *ssh.ExitError
	if _, err := output(t, c, "exit 3"); !errors.As(err, &exitErr) || exitErr.ExitStatus() != 3 {
		t.Fatalf("exit 3 = %v", err)
	}
	sess, _ := c.NewSession()
	sess.Stdin = strings.NewReader("abc")
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	if err := sess.Run("cat"); err != nil || stdout.String() != "abc" {
		t.Fatalf("cat = %q %v", stdout.String(), err)
	}
	sess.Close()
	sess, _ = c.NewSession()
	sess.Stderr = &stderr
	if err := sess.Run("stderr oops"); err != nil || stderr.String() != "oops" {
		t.Fatalf("stderr = %q %v", stderr.String(), err)
	}
	sess.Close()
	sess, _ = c.NewSession()
	sftpOut, _ := sess.StdoutPipe()
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(sftpOut); err != nil || string(b) != "SFTP-FAKE\n" {
		t.Fatalf("sftp = %q %v", b, err)
	}
	sess.Close()
	sess, _ = c.NewSession()
	in, _ := sess.StdinPipe()
	outp, _ := sess.StdoutPipe()
	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(in, "ls")
	buf := make([]byte, 64)
	n, _ := outp.Read(buf)
	if string(buf[:n]) != "shell:ls\n" {
		t.Fatalf("shell = %q", buf[:n])
	}
	in.Close()
	sess.Close()
	if srv.ConnCount() != 1 {
		t.Fatalf("conns = %d", srv.ConnCount())
	}
}

func TestDirectTCPIPAndForward(t *testing.T) {
	up := newSigner(t)
	srv := newServer(t, up)
	defer srv.Close()
	stop := srv.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "hello from 8888") }))
	defer stop()
	c := dial(t, srv, up)
	conn, err := c.Dial("tcp", "127.0.0.1:8888")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(conn, "GET / HTTP/1.0\r\nHost: x\r\n\r\n")
	body, _ := io.ReadAll(conn)
	if !strings.HasSuffix(string(body), "hello from 8888") {
		t.Fatalf("body = %q", body)
	}
	if _, err := c.Dial("tcp", "127.0.0.1:9"); err == nil {
		t.Fatal("unregistered port accepted")
	}
	if extra := srv.LastDirectTCPIP(); len(extra) == 0 {
		t.Fatal("direct-tcpip extra data not recorded")
	}
	ln, err := c.Listen("tcp", "127.0.0.1:0") // -R
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
	back, err := net.Dial("tcp", srv.ForwardAddr())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(back, "ping")
	buf := make([]byte, 4)
	io.ReadFull(back, buf)
	if string(buf) != "ping" {
		t.Fatalf("forwarded echo = %q", buf)
	}
	back.Close()
}

func TestAgentHostkeysAndControl(t *testing.T) {
	up := newSigner(t)
	srv := newServer(t, up)
	defer srv.Close()
	c := dial(t, srv, up)
	keyring := agent.NewKeyring()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	if err := agent.ForwardToAgent(c, keyring); err != nil {
		t.Fatal(err)
	}
	sess, _ := c.NewSession()
	if err := agent.RequestAgentForwarding(sess); err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("agent")
	if err != nil || string(out) != "agent:1\n" {
		t.Fatalf("agent = %q %v", out, err)
	}
	sess.Close()
	if out, err := output(t, c, "agent"); err != nil || out != "agent:none\n" {
		t.Fatalf("agent without request = %q %v", out, err)
	}
	if out, err := output(t, c, "hostkeys"); err != nil || out != "sent\n" {
		t.Fatalf("hostkeys = %q %v", out, err)
	}
	// 服务端主动向客户端开通道：x/crypto 客户端对未注册类型回 UnknownChannelType
	err = srv.OpenToClient("x11", nil)
	var oce *ssh.OpenChannelError
	if !errors.As(err, &oce) || oce.Reason != ssh.UnknownChannelType {
		t.Fatalf("x11 to client = %v", err)
	}
	srv.DropAll()
	// 服务端已关连接：NewSession 或 Output 其一必然出错（不能用 output() 辅助）
	sess, err = c.NewSession()
	if err != nil {
		return
	}
	if _, err = sess.Output("echo after-drop"); err == nil {
		t.Fatal("session survived DropAll")
	}
}

func TestCloseWithStuckHandshake(t *testing.T) {
	srv := newServer(t, newSigner(t))
	raw, err := net.Dial("tcp", srv.Addr()) // 不发任何 SSH 数据，握手永远卡住
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	deadline := time.Now().Add(2 * time.Second)
	for srv.ConnCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ConnCount() != 1 {
		t.Fatalf("conns = %d", srv.ConnCount())
	}
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on stuck handshake")
	}
}

func TestWindowChangeMidShell(t *testing.T) {
	up := newSigner(t)
	srv := newServer(t, up)
	defer srv.Close()
	c := dial(t, srv, up)
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	in, _ := sess.StdinPipe()
	outp, _ := sess.StdoutPipe()
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(in, "a")
	buf := make([]byte, 64)
	if n, _ := outp.Read(buf); string(buf[:n]) != "shell:a\n" {
		t.Fatalf("shell = %q", buf[:n])
	}
	ok, err := sess.SendRequest("window-change", true, ssh.Marshal(struct{ W, H, X, Y uint32 }{100, 40, 0, 0}))
	if err != nil || !ok {
		t.Fatalf("window-change = %v %v", ok, err)
	}
	if ok, err := sess.SendRequest("bogus", true, nil); err != nil || ok {
		t.Fatalf("bogus = %v %v", ok, err)
	}
}

func TestCancelForward(t *testing.T) {
	up := newSigner(t)
	srv := newServer(t, up)
	defer srv.Close()
	c := dial(t, srv, up)
	ln, err := c.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := srv.ForwardAddr()
	if addr == "" {
		t.Fatal("no forward addr")
	}
	// 取消一个不存在的端口：false
	type fwd struct {
		Addr string
		Port uint32
	}
	if ok, _, err := c.SendRequest("cancel-tcpip-forward", true, ssh.Marshal(fwd{"127.0.0.1", 1})); err != nil || ok {
		t.Fatalf("cancel unknown = %v %v", ok, err)
	}
	if err := ln.Close(); err != nil { // 发送 cancel-tcpip-forward
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for srv.ForwardAddr() != "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ForwardAddr() != "" {
		t.Fatalf("forward addr not cleared: %q", srv.ForwardAddr())
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Fatal("listener still open after cancel")
	}
}

func TestRejects(t *testing.T) {
	up := newSigner(t)
	srv := newServer(t, up)
	defer srv.Close()
	if _, err := ssh.Dial("tcp", srv.Addr(), &ssh.ClientConfig{User: "bob", Auth: []ssh.AuthMethod{ssh.PublicKeys(up)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second}); err == nil {
		t.Fatal("non-root user accepted")
	}
	c := dial(t, srv, up)
	if ok, _, err := c.SendRequest("bogus@example.com", true, nil); err != nil || ok {
		t.Fatalf("unknown global request = %v %v", ok, err)
	}
	if ok, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil || !ok {
		t.Fatalf("keepalive = %v %v", ok, err)
	}
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestSubsystem("netconf"); err == nil {
		t.Fatal("non-sftp subsystem accepted")
	}
}

func TestOpenToClientSkipsUnestablishedConn(t *testing.T) {
	up := newSigner(t)
	srv := newServer(t, up)
	defer srv.Close()
	dial(t, srv, up)
	raw, err := net.Dial("tcp", srv.Addr()) // 更新的一条原始连接，握手永远不完成
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	deadline := time.Now().Add(2 * time.Second)
	for srv.ConnCount() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	err = srv.OpenToClient("x11", nil)
	var oce *ssh.OpenChannelError
	if !errors.As(err, &oce) || oce.Reason != ssh.UnknownChannelType {
		t.Fatalf("x11 to client = %v, want UnknownChannelType via established conn", err)
	}
}

func TestForwardAddrClearedOnDrop(t *testing.T) {
	up := newSigner(t)
	srv := newServer(t, up)
	defer srv.Close()
	c := dial(t, srv, up)
	if _, err := c.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if srv.ForwardAddr() == "" {
		t.Fatal("no forward addr")
	}
	srv.DropAll()
	deadline := time.Now().Add(2 * time.Second)
	for srv.ForwardAddr() != "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.ForwardAddr() != "" {
		t.Fatalf("forward addr not cleared after drop: %q", srv.ForwardAddr())
	}
}

func TestShellModeExec(t *testing.T) {
	up := newSigner(t)
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey(), Shell: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	root := srv.Root()
	if root == "" {
		t.Fatal("no root")
	}
	c := dial(t, srv, up)
	// 真执行：工作目录是 Root，HOME 也是
	if out, err := output(t, c, `pwd; printf '%s\n' "$HOME"`); err != nil || out != root+"\n"+root+"\n" {
		t.Fatalf("pwd = %q %v", out, err)
	}
	// 退出码透传
	var exitErr *ssh.ExitError
	if _, err := output(t, c, "exit 7"); !errors.As(err, &exitErr) || exitErr.ExitStatus() != 7 {
		t.Fatalf("exit 7 = %v", err)
	}
	// stdin → 文件（部署包上传路径）
	sess, _ := c.NewSession()
	sess.Stdin = strings.NewReader("payload")
	if err := sess.Run("mkdir -p sub && cat > sub/f"); err != nil {
		t.Fatal(err)
	}
	sess.Close()
	if b, _ := os.ReadFile(filepath.Join(root, "sub", "f")); string(b) != "payload" {
		t.Fatalf("file = %q", b)
	}
	// stderr 分流
	sess, _ = c.NewSession()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	if err := sess.Run("echo out; echo err >&2"); err != nil || stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Fatalf("split = %q %q %v", stdout.String(), stderr.String(), err)
	}
	sess.Close()
	// 关闭服务器杀掉仍在跑的命令（sleep 不会残留）
	sess, _ = c.NewSession()
	done := make(chan error, 1)
	go func() { done <- sess.Run("sleep 30") }()
	time.Sleep(100 * time.Millisecond)
	srv.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("sleep finished normally")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command survived Close")
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("temp root not removed on Close")
	}
}

// 后台子进程继承 stdout 时，Close 必须杀掉整个进程组：只杀 /bin/sh 的话子进程一直占着 stdout 管道，
// Wait 等不到 EOF，Close 卡住（Linux dash 下 fork 出来的 sleep 就是这样）。
func TestShellModeCloseKillsBackgroundChild(t *testing.T) {
	up := newSigner(t)
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey(), Shell: true, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c := dial(t, srv, up)
	sess, _ := c.NewSession()
	out, _ := sess.StdoutPipe()
	if err := sess.Start("sleep 30 & echo $!; wait"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("pid line = %q", line)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	closed := make(chan struct{})
	start := time.Now()
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a background child holding stdout")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %v (waited for WaitDelay instead of killing the process group)", d)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) { // 被杀后由 init 收尸，短暂停留为僵尸
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("background child %d survived Close", pid)
	}
}
