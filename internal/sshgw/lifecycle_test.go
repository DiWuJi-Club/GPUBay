package sshgw_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
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

// 批量扫描：一轮只查一次状态、不调 Access；查询出错谁也不踢；查不到的实例断开，其它实例不受影响。
func TestScanBatchedStoreErrorKeepsMissingDisconnects(t *testing.T) {
	const instB = "ins-bbbbbbbb"
	e := newEnv(t)
	e.store.set(instB, runningInfo(e, instB, 8, "pw-other-16chars"))
	a1 := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	a2 := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	b := e.mustDial(instB, ssh.Password("pw-other-16chars"))
	for _, c := range []*ssh.Client{a1, a2, b} {
		if _, _, err := run(t, c, "echo 1"); err != nil {
			t.Fatal(err)
		}
	}
	e.store.mu.Lock()
	accessed, queries := e.store.accessed, e.store.stateQueries
	e.store.mu.Unlock()
	e.srv.ScanNow(context.Background())
	e.store.mu.Lock()
	if e.store.accessed != accessed || e.store.stateQueries != queries+1 {
		t.Fatalf("scan: access calls %d→%d, state queries %d→%d", accessed, e.store.accessed, queries, e.store.stateQueries)
	}
	e.store.mu.Unlock()
	if e.srv.Sessions() != 3 {
		t.Fatalf("sessions = %d", e.srv.Sessions())
	}
	// 持续数据库故障：多轮扫描都不踢（即便实例此时已不存在）
	e.store.mu.Lock()
	delete(e.store.infos, instB)
	e.store.mu.Unlock()
	e.store.setFailFor(errors.New("conn reset"), 3)
	for i := 0; i < 3; i++ {
		e.srv.ScanNow(context.Background())
	}
	time.Sleep(100 * time.Millisecond)
	if e.srv.Sessions() != 3 {
		t.Fatal("store error kicked sessions")
	}
	// 故障恢复：查不到的 instB 断开，instA 的两条保留
	e.srv.ScanNow(context.Background())
	waitFor(t, "instB session closed", func() bool { return e.srv.Sessions() == 2 })
	closed := make(chan struct{})
	go func() { b.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("instB client not closed")
	}
	if _, _, err := run(t, a1, "echo still"); err != nil {
		t.Fatalf("instA session hurt: %v", err)
	}
}

// 客户端不读数据、通道窗口耗尽时，断开提示写不出去：最多等 1 秒后照样断开。
func TestDisconnectBoundedWhenClientWindowFull(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	sess, _ := c.NewSession()
	if _, err := sess.StdoutPipe(); err != nil { // 永不读取：客户端不再放大窗口
		t.Fatal(err)
	}
	if _, err := sess.StderrPipe(); err != nil {
		t.Fatal(err)
	}
	in, _ := sess.StdinPipe()
	if err := sess.Start("cat"); err != nil {
		t.Fatal(err)
	}
	var written atomic.Int64
	go func() { // cat 把输入原样回显，直到各级窗口全部耗尽、写入阻塞
		buf := make([]byte, 32<<10)
		for {
			n, err := in.Write(buf)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	// 等写入停滞（窗口已满）
	last := int64(-1)
	waitFor(t, "stdin stalls", func() bool {
		time.Sleep(200 * time.Millisecond)
		cur := written.Load()
		stalled := cur == last && cur > 0
		last = cur
		return stalled
	})
	info := runningInfo(e, instA, 7, "pw-correct-16chr")
	info.Instance.State = rental.StateStopped
	e.store.set(instA, info)
	start := time.Now()
	go e.srv.ScanNow(context.Background()) // 异步：写入若无上限，测试在下面超时失败而不是卡死
	waitFor(t, "sessions 0", func() bool { return e.srv.Sessions() == 0 })
	if d := time.Since(start); d < 900*time.Millisecond || d > 3*time.Second {
		t.Fatalf("disconnect took %v (want ≈1s: the stderr write must block and be bounded)", d)
	}
}

// flakyListener 前 n 次 Accept 返回错误（模拟 EMFILE），之后正常。
type flakyListener struct {
	net.Listener
	mu   sync.Mutex
	left int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.left > 0 {
		l.left--
		l.mu.Unlock()
		return nil, errors.New("accept: too many open files")
	}
	l.mu.Unlock()
	return l.Listener.Accept()
}

func TestAcceptErrorsAreRetried(t *testing.T) {
	e := newEnvLis(t, nil, func(l net.Listener) net.Listener { return &flakyListener{Listener: l, left: 4} })
	// 先确认 4 次错误都被记录并重试（Accept 出错即退出的实现在这里失败，而不是让下面的拨号卡住）
	waitFor(t, "4 accept errors retried", func() bool { return strings.Count(e.logs.String(), "ssh accept") == 4 })
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if out, _, err := run(t, c, "echo ok"); err != nil || strings.TrimSpace(out) != "ok" {
		t.Fatalf("run = %q %v", out, err)
	}
	if n := strings.Count(e.logs.String(), "ssh accept"); n != 4 {
		t.Fatalf("accept error logs = %d, want 4", n)
	}
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
