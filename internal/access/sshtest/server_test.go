package sshtest_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
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
	srv, _ := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
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
	srv, _ := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
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
	// 服务端已关连接：NewSession 或 Output 其一必然出错（不能用 output() 辅助，它在 NewSession 出错时直接 t.Fatal）
	if sess, err := c.NewSession(); err == nil {
		if _, err = sess.Output("echo after-drop"); err == nil {
			t.Fatal("session survived DropAll")
		}
	}
}
