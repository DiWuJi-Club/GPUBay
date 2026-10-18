package sshgw_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/rental"
	"spark-cloud/internal/sshgw"
)

func TestPasswordLoginRelaysExecAndRecordsSession(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	out, _, err := run(t, c, "echo hello")
	if err != nil || out != "hello\n" {
		t.Fatalf("exec = %q %v", out, err)
	}
	c.Close()
	waitFor(t, "session closed", func() bool { _, closed, _ := e.store.snapshot(); return len(closed) == 1 })
	opened, closed, _ := e.store.snapshot()
	if len(opened) != 1 || opened[0].InstanceID != instA || opened[0].UserID != 7 || opened[0].AuthMethod != "password" || opened[0].KeyID != nil || opened[0].ClientIP != "127.0.0.1" {
		t.Fatalf("opened = %+v", opened)
	}
	if b := closed[1]; b[1] < int64(len("hello\n")) {
		t.Fatalf("bytes = %v", b)
	}
	if e.srv.Sessions() != 0 {
		t.Fatalf("sessions = %d", e.srv.Sessions())
	}
}

func TestPublicKeyLoginAndPasswordDisabled(t *testing.T) {
	e := newEnv(t)
	userKey := newSigner(t)
	k := e.keys.add(7, userKey.PublicKey())
	c := e.mustDial(instA, ssh.PublicKeys(userKey))
	if out, _, err := run(t, c, "echo k"); err != nil || out != "k\n" {
		t.Fatalf("pubkey exec = %q %v", out, err)
	}
	waitFor(t, "key touched", func() bool {
		e.keys.mu.Lock()
		defer e.keys.mu.Unlock()
		return len(e.keys.touched) == 1 && e.keys.touched[0] == k.ID
	})
	opened, _, _ := e.store.snapshot()
	if opened[0].AuthMethod != "publickey" || opened[0].KeyID == nil || *opened[0].KeyID != k.ID {
		t.Fatalf("opened = %+v", opened[0])
	}
	// 其他用户的钥匙不行
	if _, _, err := e.dial(instA, ssh.PublicKeys(newSigner(t))); err == nil {
		t.Fatal("unknown key accepted")
	}
	// 关闭密码登录后密码被拒、公钥仍通
	info := runningInfo(e, instA, 7, "pw-correct-16chr")
	info.Instance.PasswordAuth = false
	e.store.set(instA, info)
	if _, _, err := e.dial(instA, ssh.Password("pw-correct-16chr")); err == nil {
		t.Fatal("password accepted while disabled")
	}
	e.mustDial(instA, ssh.PublicKeys(userKey))
}

func TestBannerForUnknownUsernames(t *testing.T) {
	e := newEnv(t)
	for _, user := range []string{"root", "INS-AAAAAAAA", " ins-aaaaaaaa", "ins-aaaaaaaa@ssh", "ins-zzzzzzzz", "ins-aaaaaaa", ""} {
		_, banner, err := e.dial(user, ssh.Password("pw-correct-16chr"))
		if err == nil || banner != sshgw.BannerUnknown {
			t.Fatalf("user %q: banner=%q err=%v", user, banner, err)
		}
	}
	if _, banner, err := e.dial(instA, ssh.Password("wrong")); err == nil || banner != "" {
		t.Fatalf("wrong password: banner=%q err=%v", banner, err)
	}
}

func TestBannerByState(t *testing.T) {
	e := newEnv(t)
	retain := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	cases := []struct {
		name   string
		mutate func(*rental.AccessInfo)
		want   string
	}{
		{"suspended", func(i *rental.AccessInfo) { now := e.clk.Now(); i.Instance.SuspendedAt = &now }, sshgw.BannerSuspended},
		{"creating", func(i *rental.AccessInfo) { i.Instance.State = rental.StateCreating }, sshgw.BannerCreating},
		{"stopped", func(i *rental.AccessInfo) { i.Instance.State = rental.StateStopped; i.RetainUntil = &retain }, "实例已到期停机，数据保留至 2026-03-04 13:06，续费后可继续使用"},
		{"wiping", func(i *rental.AccessInfo) { i.Instance.State = rental.StateWiping }, sshgw.BannerReleased},
		{"released", func(i *rental.AccessInfo) { i.Instance.State = rental.StateReleased }, sshgw.BannerReleased},
		{"failed", func(i *rental.AccessInfo) { i.Instance.State = rental.StateFailed }, sshgw.BannerReleased},
	}
	for _, c := range cases {
		info := runningInfo(e, instA, 7, "pw-correct-16chr")
		c.mutate(&info)
		e.store.set(instA, info)
		if _, banner, err := e.dial(instA, ssh.Password("pw-correct-16chr")); err == nil || banner != c.want {
			t.Fatalf("%s: banner=%q err=%v", c.name, banner, err)
		}
	}
	// agent_online=false 但实例仍 running（5 分钟判定窗口内）：允许
	info := runningInfo(e, instA, 7, "pw-correct-16chr")
	info.AgentOnline = false
	e.store.set(instA, info)
	e.mustDial(instA, ssh.Password("pw-correct-16chr"))
}

func TestFailureCountingIsPerConnection(t *testing.T) {
	e := newEnv(t)
	sshgw.SetLimitsForTest(e.srv, 3, 100, 32)
	// agent 里 4 把陌生钥匙 + 正确密码：PublicKeyCallback 失败 4 次，但连接最终成功 → 不计失败
	keys := []ssh.Signer{newSigner(t), newSigner(t), newSigner(t), newSigner(t)}
	c, _, err := e.dial(instA, ssh.PublicKeys(keys...))
	if err == nil {
		c.Close()
		t.Fatal("unknown keys accepted")
	}
	// 服务端在读到 EOF 后才计数，客户端 Dial 返回时未必已计入：一律等待
	fails := func(n int) {
		waitFor(t, fmt.Sprintf("fail count %d", n), func() bool { return sshgw.FailCountForTest(e.srv, "127.0.0.1") == n })
	}
	fails(1)
	c2, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{User: instA, Auth: []ssh.AuthMethod{ssh.PublicKeys(keys...), ssh.Password("pw-correct-16chr")},
		HostKeyCallback: ssh.FixedHostKey(e.hostKey.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
	time.Sleep(100 * time.Millisecond)
	fails(1) // 成功的连接不计
	// 密码错 5 次（同一连接内重试）只计 1
	e.dial(instA, ssh.RetryableAuthMethod(ssh.Password("wrong"), 5))
	fails(2)
	e.dial(instA, ssh.Password("wrong"))
	waitFor(t, "ip blocked", func() bool { return sshgw.BlockedForTest(e.srv, "127.0.0.1") })
	if _, _, err := e.dial(instA, ssh.Password("pw-correct-16chr")); err == nil {
		t.Fatal("blocked ip could connect")
	}
	e.clk.Advance(10*time.Minute + time.Second)
	e.mustDial(instA, ssh.Password("pw-correct-16chr"))
}

func TestInstanceBruteforceAlertOnce(t *testing.T) {
	e := newEnv(t)
	sshgw.SetLimitsForTest(e.srv, 100, 2, 32)
	for i := 0; i < 4; i++ {
		e.dial(instA, ssh.Password("wrong"))
	}
	waitFor(t, "alert raised", func() bool { _, _, alerts := e.store.snapshot(); return len(alerts) >= 1 })
	time.Sleep(100 * time.Millisecond)
	_, _, alerts := e.store.snapshot()
	if len(alerts) != 1 || alerts[0] != "bruteforce:"+instA {
		t.Fatalf("alerts = %v", alerts)
	}
}

func TestIPKeyAndConcurrentConnCap(t *testing.T) {
	if sshgw.IPKeyForTest("203.0.113.9") != "203.0.113.9" || sshgw.IPKeyForTest("2001:db8:1:2::1") != sshgw.IPKeyForTest("2001:db8:1:2:ffff::2") ||
		sshgw.IPKeyForTest("2001:db8:1:2::1") == sshgw.IPKeyForTest("2001:db8:1:3::1") || !strings.HasSuffix(sshgw.IPKeyForTest("2001:db8::1"), "/64") {
		t.Fatal("ipKey: IPv6 must be keyed by /64, IPv4 by address")
	}
	e := newEnv(t)
	sshgw.SetLimitsForTest(e.srv, 100, 100, 2)
	a := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	b := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if _, _, err := e.dial(instA, ssh.Password("pw-correct-16chr")); err == nil {
		t.Fatal("third concurrent connection from same ip accepted")
	}
	a.Close()
	waitFor(t, "slot freed", func() bool { _, _, err := e.dial(instA, ssh.Password("pw-correct-16chr")); return err == nil })
	b.Close()
}

func TestSessionCapAndUpstreamUnreachable(t *testing.T) {
	e := newEnvWith(t, func(c *sshgw.Config) { c.MaxSessionsPerInstance = 1 })
	first := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if _, _, err := run(t, first, "echo one"); err != nil {
		t.Fatal(err)
	}
	second := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	_, stderr, err := run(t, second, "echo two")
	if err == nil || !strings.Contains(stderr, sshgw.MsgTooManySessions) {
		t.Fatalf("second session: err=%v stderr=%q", err, stderr)
	}
	first.Close()
	waitFor(t, "slot released", func() bool { return e.srv.Sessions() == 0 })
	// 上游拨不通（端口 1 没人监听）：提示 + 告警（进程内 5 分钟节流，连两次只告警一次）
	bad := newEnvWith(t, func(c *sshgw.Config) { c.UpstreamPort = 1 })
	for i := 0; i < 2; i++ {
		c := bad.mustDial(instA, ssh.Password("pw-correct-16chr"))
		_, stderr, err = run(t, c, "echo x")
		if err == nil || !strings.Contains(stderr, sshgw.MsgUpstreamUnreachable) {
			t.Fatalf("unreachable: err=%v stderr=%q", err, stderr)
		}
	}
	_, _, alerts := bad.store.snapshot()
	if len(alerts) != 1 || alerts[0] != "unreachable:"+instA {
		t.Fatalf("alerts = %v", alerts)
	}
}
