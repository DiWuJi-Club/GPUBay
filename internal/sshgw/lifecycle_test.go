package sshgw_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/rental"
	"spark-cloud/internal/sshgw"
)

func TestScanDisconnectsStoppedButKeepsSuspended(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	sess, _ := c.NewSession()
	var stderr strings.Builder
	sess.Stderr = &stderr
	in, _ := sess.StdinPipe()
	if err := sess.Start("cat"); err != nil {
		t.Fatal(err)
	}
	// 暂停：不断
	info := runningInfo(e, instA, 7, "pw-correct-16chr")
	now := e.clk.Now()
	info.Instance.SuspendedAt = &now
	e.store.set(instA, info)
	e.srv.ScanNow(context.Background())
	time.Sleep(100 * time.Millisecond)
	if e.srv.Sessions() != 1 {
		t.Fatal("suspended instance's session was disconnected")
	}
	// 数据库瞬时错误：不是终态，不能踢会话
	e.store.setFailNext(errors.New("db timeout"))
	e.srv.ScanNow(context.Background())
	time.Sleep(100 * time.Millisecond)
	if e.srv.Sessions() != 1 {
		t.Fatal("db error kicked the session")
	}
	// 停机：10 秒扫描断开并提示
	info = runningInfo(e, instA, 7, "pw-correct-16chr")
	info.Instance.State = rental.StateStopped
	e.store.set(instA, info)
	e.srv.ScanNow(context.Background())
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("session not closed after stop")
	}
	in.Close()
	if !strings.Contains(stderr.String(), sshgw.MsgDisconnected) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	waitFor(t, "sessions 0", func() bool { return e.srv.Sessions() == 0 })
}

func TestScanDisconnectsMissingInstance(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if _, _, err := run(t, c, "echo 1"); err != nil {
		t.Fatal(err)
	}
	// 持续数据库故障：多轮扫描都不踢
	e.store.setFailFor(errors.New("conn reset"), 3)
	for i := 0; i < 3; i++ {
		e.srv.ScanNow(context.Background())
	}
	time.Sleep(100 * time.Millisecond)
	if e.srv.Sessions() != 1 {
		t.Fatal("persistent db error kicked the session")
	}
	e.store.mu.Lock()
	delete(e.store.infos, instA)
	e.store.mu.Unlock()
	e.srv.ScanNow(context.Background())
	waitFor(t, "sessions 0", func() bool { return e.srv.Sessions() == 0 })
}

func TestTouchRefreshesAllSessions(t *testing.T) {
	e := newEnv(t)
	a := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	b := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if _, _, err := run(t, a, "echo 1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, b, "echo 2"); err != nil {
		t.Fatal(err)
	}
	e.srv.TouchNow(context.Background())
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if len(e.store.touched) != 1 || len(e.store.touched[0]) != 2 {
		t.Fatalf("touched = %v", e.store.touched)
	}
}

func TestDrainClosesRemainingSessions(t *testing.T) {
	e := newEnvWith(t, func(c *sshgw.Config) { c.DrainTimeout = 200 * time.Millisecond })
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	sess, _ := c.NewSession()
	var stderr strings.Builder
	sess.Stderr = &stderr
	in, _ := sess.StdinPipe() // 不接 stdin 时客户端立即发 EOF，cat 马上退出
	defer in.Close()
	if err := sess.Start("cat"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 3*time.Second {
		t.Fatalf("drain took %v", d)
	}
	if err := sess.Wait(); err == nil {
		t.Fatalf("session survived drain: stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "网关正在重启") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// 握手中的连接（未认证，握手超时 30 秒）也必须在 drain 时限内被关闭，不能拖住 Serve 返回。
func TestDrainClosesConnectionsMidHandshake(t *testing.T) {
	e := newEnvWith(t, func(c *sshgw.Config) { c.DrainTimeout = 200 * time.Millisecond })
	raw, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// 只读到服务端版本串，然后什么都不发：停在握手中
	buf := make([]byte, 64)
	raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := raw.Read(buf); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve blocked by a connection mid-handshake")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("drain took %v", d)
	}
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, err := raw.Read(buf); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatal("handshaking connection was not closed")
			}
			break
		}
	}
}
