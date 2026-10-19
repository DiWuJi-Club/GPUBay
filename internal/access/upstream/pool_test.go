package upstream_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/access/upstream"
)

func signer(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type memStore struct {
	mu   sync.Mutex
	pubs map[string]string
}

func (m *memStore) SetSSHHostPubkeyIfEmpty(_ context.Context, id, pub string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pubs[id] != "" {
		return false, nil
	}
	m.pubs[id] = pub
	return true, nil
}

func (m *memStore) SSHHostPubkey(_ context.Context, id string) (string, error) {
	return m.get(id), nil
}

func (m *memStore) get(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pubs[id]
}

// waitFor 轮询 cond 直到为真或超时（sshtest 的连接摘除、池的 keepalive 丢弃都是异步的）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func httpGet(t *testing.T, p *upstream.Pool, tg upstream.Target, port int) (string, error) {
	t.Helper()
	conn, err := p.Dial(context.Background(), tg, port)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.0\r\nHost: x\r\n\r\n")
	b, _ := io.ReadAll(conn)
	parts := strings.SplitN(string(b), "\r\n\r\n", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("bad http response %q", b)
	}
	return parts[1], nil
}

func TestPoolSharesClientAndVerifiesHostKey(t *testing.T) {
	up := signer(t)
	srv, _ := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	defer srv.Close()
	stop := srv.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer stop()
	store := &memStore{pubs: map[string]string{}}
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: 300 * time.Millisecond}, store)
	defer p.Close()
	good := upstream.Target{InstanceID: "ins-aaaaaaaa", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	for i := 0; i < 3; i++ {
		if body, err := httpGet(t, p, good, 8888); err != nil || body != "ok" {
			t.Fatalf("get %d = %q %v", i, body, err)
		}
	}
	if p.Len() != 1 || srv.ConnCount() != 1 {
		t.Fatalf("pool=%d upstream conns=%d, want 1 shared connection", p.Len(), srv.ConnCount())
	}
	// 主机密钥不符 → DialError，且不建立连接
	bad := upstream.Target{InstanceID: "ins-bbbbbbbb", LanIP: "127.0.0.1", HostPubkey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	var de *upstream.DialError
	if _, err := httpGet(t, p, bad, 8888); !errors.As(err, &de) || de.InstanceID != "ins-bbbbbbbb" || !strings.Contains(err.Error(), "主机密钥") {
		t.Fatalf("mismatch = %v", err)
	}
	if p.Len() != 1 {
		t.Fatalf("mismatched host key pooled: len=%d", p.Len())
	}
	waitFor(t, "rejected conn removed upstream", func() bool { return srv.ConnCount() == 1 })
	// 存量实例（公钥为空）：首连信任并写回；之后用写回的公钥
	legacy := upstream.Target{InstanceID: "ins-cccccccc", LanIP: "127.0.0.1"}
	if body, err := httpGet(t, p, legacy, 8888); err != nil || body != "ok" {
		t.Fatalf("tofu = %q %v", body, err)
	}
	if store.get("ins-cccccccc") != srv.HostPubLine() {
		t.Fatalf("tofu stored %q", store.get("ins-cccccccc"))
	}
	// 未登记端口：拨号失败（不是 DialError）但连接保留
	if _, err := p.Dial(context.Background(), good, 9); err == nil || errors.As(err, &de) || p.Len() != 2 {
		t.Fatalf("port 9 = %v, len=%d", err, p.Len())
	}
	// 上游崩溃：旧连接坏掉，下一次 Dial 自动重建（不需要调用方 Drop）
	before := srv.ConnCount()
	srv.DropAll()
	if srv.ConnCount() != 0 {
		t.Fatalf("conns right after DropAll = %d", srv.ConnCount())
	}
	if body, err := httpGet(t, p, good, 8888); err != nil || body != "ok" {
		t.Fatalf("after upstream drop = %q %v", body, err)
	}
	if srv.ConnCount() != 1 || before != 2 {
		t.Fatalf("conns after rebuild = %d (before drop %d)", srv.ConnCount(), before)
	}
	// ins-cccccccc 的连接也被 DropAll 关掉了，没人再拨它：keepalive 协程发现连接结束后把它移出池
	waitFor(t, "dead legacy conn dropped from pool", func() bool { return p.Len() == 1 })
	// 登记地址变化（键含 lan_ip）：旧连接被丢弃，按新地址重拨；新地址不可达 → DialError
	moved := good
	moved.LanIP = "10.255.255.1"
	if _, err := httpGet(t, p, moved, 8888); !errors.As(err, &de) || de.InstanceID != "ins-aaaaaaaa" {
		t.Fatalf("moved lan ip = %v", err)
	}
	// ins-aaaaaaaa 的旧连接随地址变化丢弃、新地址没连上；ins-cccccccc 已因 DropAll 移出 → 池为空
	if p.Len() != 0 {
		t.Fatalf("after lan ip change len=%d", p.Len())
	}
	waitFor(t, "old 127.0.0.1 conn closed upstream", func() bool { return srv.ConnCount() == 0 })
	if _, err := httpGet(t, p, upstream.Target{InstanceID: "ins-aaaaaaaa", LanIP: "127.000.000.001", HostPubkey: good.HostPubkey}, 8888); err == nil {
		t.Fatal("unparseable lan ip accepted")
	}
	// 未带写回的公钥再拨（路由表还没刷新）：写回失败后复读登记值，与上游一致 → 接受
	if body, err := httpGet(t, p, legacy, 8888); err != nil || body != "ok" {
		t.Fatalf("second tofu = %q %v", body, err)
	}
	// 路由表带上写回的公钥后按固定公钥校验
	legacy.HostPubkey = store.get("ins-cccccccc")
	if body, err := httpGet(t, p, legacy, 8888); err != nil || body != "ok" {
		t.Fatalf("legacy redial = %q %v", body, err)
	}
	if p.Len() != 1 {
		t.Fatalf("after legacy redial len=%d", p.Len())
	}
	p.Drop("ins-cccccccc")
	if p.Len() != 0 {
		t.Fatalf("after drop len=%d", p.Len())
	}
	waitFor(t, "dropped conn closed upstream", func() bool { return srv.ConnCount() == 0 })
}

// 上游崩溃后池里可能还挂着已死的客户端（keepalive 协程尚未察觉）：Dial 必须当场丢弃并用新连接重试一次。
func TestPoolRetriesOnDeadClient(t *testing.T) {
	up := signer(t)
	srv, _ := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	defer srv.Close()
	stop := srv.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer stop()
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: time.Second}, &memStore{pubs: map[string]string{}})
	defer p.Close()
	tg := upstream.Target{InstanceID: "ins-ffffffff", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	for i := 0; i < 30; i++ {
		if body, err := httpGet(t, p, tg, 8888); err != nil || body != "ok" {
			t.Fatalf("round %d = %q %v", i, body, err)
		}
		if srv.ConnCount() != 1 || p.Len() != 1 {
			t.Fatalf("round %d: upstream conns=%d pool=%d", i, srv.ConnCount(), p.Len())
		}
		srv.DropAll()
	}
}

func TestPoolUnreachableAndNotRunningTargets(t *testing.T) {
	up := signer(t)
	store := &memStore{pubs: map[string]string{}}
	p := upstream.New(upstream.Config{Signer: up, Port: 1, DialTimeout: 300 * time.Millisecond}, store)
	defer p.Close()
	var de *upstream.DialError
	_, err := p.Dial(context.Background(), upstream.Target{InstanceID: "ins-dddddddd", LanIP: "127.0.0.1", HostPubkey: "x"}, 80)
	if !errors.As(err, &de) {
		t.Fatalf("unreachable = %v", err)
	}
	if _, err := p.Dial(context.Background(), upstream.Target{InstanceID: "ins-eeeeeeee", LanIP: "", HostPubkey: "x"}, 80); err == nil {
		t.Fatal("empty lan ip accepted")
	}
	if p.Len() != 0 {
		t.Fatalf("len=%d", p.Len())
	}
}

// 只 accept 不说 SSH 的监听：握手永远不完成。
func silentListener(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
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
	return ln.Addr().(*net.TCPAddr).Port
}

func TestPoolHandshakeDeadlineAndCtx(t *testing.T) {
	port := silentListener(t)
	p := upstream.New(upstream.Config{Signer: signer(t), Port: port, DialTimeout: 300 * time.Millisecond}, &memStore{pubs: map[string]string{}})
	defer p.Close()
	tg := upstream.Target{InstanceID: "ins-gggggggg", LanIP: "127.0.0.1", HostPubkey: "x"}
	start := time.Now()
	type res struct {
		err     error
		elapsed time.Duration
	}
	first, second := make(chan res, 1), make(chan res, 1)
	go func() {
		_, err := p.Dial(context.Background(), tg, 80)
		first <- res{err, time.Since(start)}
	}()
	time.Sleep(50 * time.Millisecond) // 第一个已持有该实例的锁、卡在握手
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := p.Dial(ctx, tg, 80)
		second <- res{err, time.Since(start)}
	}()
	for name, ch := range map[string]chan res{"first": first, "second": second} {
		select {
		case r := <-ch:
			if r.err == nil || r.elapsed > 2*time.Second {
				t.Fatalf("%s dial = %v after %v", name, r.err, r.elapsed)
			}
			if name == "first" {
				var de *upstream.DialError
				if !errors.As(r.err, &de) {
					t.Fatalf("handshake timeout not a DialError: %v", r.err)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s dial hung", name)
		}
	}
	// 握手超时很长时，ctx 取消也能打断握手
	p2 := upstream.New(upstream.Config{Signer: signer(t), Port: port, DialTimeout: time.Minute}, &memStore{pubs: map[string]string{}})
	defer p2.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err := p2.Dial(ctx, tg, 80)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("ctx-cancelled handshake = %v after %v", err, time.Since(start))
	}
	// 调用方放弃不是「上游不可达」：返回 ctx 错误而非 DialError（不应触发告警）
	var de *upstream.DialError
	if errors.As(err, &de) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx-cancelled handshake err = %v (want plain ctx error)", err)
	}
	// 冷拨号前 ctx 已取消：同样是 ctx 错误
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if _, err := p2.Dial(cctx, tg, 80); errors.As(err, &de) || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled dial err = %v (want plain ctx error)", err)
	}
	if p.Len() != 0 || p2.Len() != 0 {
		t.Fatalf("len = %d %d", p.Len(), p2.Len())
	}
}

func TestPoolConcurrentFirstDialSingleConn(t *testing.T) {
	up := signer(t)
	srv, _ := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	defer srv.Close()
	stop := srv.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer stop()
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: 2 * time.Second}, &memStore{pubs: map[string]string{}})
	defer p.Close()
	tg := upstream.Target{InstanceID: "ins-hhhhhhhh", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Dial(context.Background(), tg, 8888)
			if err == nil {
				c.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if srv.ConnCount() != 1 || p.Len() != 1 {
		t.Fatalf("upstream conns=%d pool=%d, want exactly 1", srv.ConnCount(), p.Len())
	}
}

func TestPoolKeyAwareEntries(t *testing.T) {
	up := signer(t)
	srv, _ := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	defer srv.Close()
	stop := srv.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer stop()
	store := &memStore{pubs: map[string]string{}}
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: time.Second}, store)
	defer p.Close()
	// 首连信任建立的连接：路由仍为空公钥 → 复用；路由带上写回的同一公钥 → 复用
	tg := upstream.Target{InstanceID: "ins-iiiiiiii", LanIP: "127.0.0.1"}
	for _, key := range []string{"", "", srv.HostPubLine()} {
		tg.HostPubkey = key
		if body, err := httpGet(t, p, tg, 8888); err != nil || body != "ok" {
			t.Fatalf("key %q = %q %v", key, body, err)
		}
		if srv.ConnCount() != 1 || p.Len() != 1 {
			t.Fatalf("key %q: upstream conns=%d pool=%d", key, srv.ConnCount(), p.Len())
		}
	}
	// 路由公钥变了（实例重建了主机密钥）：丢弃旧连接，按新公钥重拨；与上游不符 → DialError
	tg.HostPubkey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	var de *upstream.DialError
	if _, err := httpGet(t, p, tg, 8888); !errors.As(err, &de) || !strings.Contains(err.Error(), "主机密钥") {
		t.Fatalf("changed key = %v", err)
	}
	if p.Len() != 0 {
		t.Fatalf("len after key change = %d", p.Len())
	}
	waitFor(t, "old conn closed upstream", func() bool { return srv.ConnCount() == 0 })
}

func TestPoolTOFURaceLoser(t *testing.T) {
	up := signer(t)
	srv, _ := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	defer srv.Close()
	stop := srv.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer stop()
	// 其它副本已写回同一公钥（路由表还是旧的空公钥）：复读一致 → 接受
	store := &memStore{pubs: map[string]string{"ins-jjjjjjjj": srv.HostPubLine(), "ins-kkkkkkkk": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: time.Second}, store)
	defer p.Close()
	if body, err := httpGet(t, p, upstream.Target{InstanceID: "ins-jjjjjjjj", LanIP: "127.0.0.1"}, 8888); err != nil || body != "ok" {
		t.Fatalf("race loser same key = %q %v", body, err)
	}
	// 写回的是别的公钥 → DialError
	var de *upstream.DialError
	if _, err := httpGet(t, p, upstream.Target{InstanceID: "ins-kkkkkkkk", LanIP: "127.0.0.1"}, 8888); !errors.As(err, &de) {
		t.Fatalf("race loser different key = %v", err)
	}
	if p.Len() != 1 {
		t.Fatalf("len = %d", p.Len())
	}
}

func TestPoolCloseIsFinal(t *testing.T) {
	up := signer(t)
	srv, _ := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	defer srv.Close()
	stop := srv.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer stop()
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: time.Second}, &memStore{pubs: map[string]string{}})
	tg := upstream.Target{InstanceID: "ins-llllllll", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	if body, err := httpGet(t, p, tg, 8888); err != nil || body != "ok" {
		t.Fatalf("before close = %q %v", body, err)
	}
	p.Close()
	var de *upstream.DialError
	_, err := p.Dial(context.Background(), tg, 8888)
	if err == nil || errors.As(err, &de) || !errors.Is(err, upstream.ErrClosed) {
		t.Fatalf("dial after close = %v", err)
	}
	if p.Len() != 0 {
		t.Fatalf("len after close = %d", p.Len())
	}
	waitFor(t, "upstream conns closed", func() bool { return srv.ConnCount() == 0 })
}

func TestHostKeyAlgorithms(t *testing.T) {
	cases := map[string][]string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA": {ssh.KeyAlgoED25519},
		"":        nil,
		"garbage": nil,
	}
	for line, want := range cases {
		if got := upstream.HostKeyAlgorithms(line); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("HostKeyAlgorithms(%q) = %v, want %v", line, got, want)
		}
	}
}

func shellServer(t *testing.T, up ssh.Signer) *sshtest.Server {
	t.Helper()
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey(), Shell: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestPoolExec(t *testing.T) {
	up := signer(t)
	srv := shellServer(t, up)
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: time.Second}, &memStore{pubs: map[string]string{}})
	defer p.Close()
	tg := upstream.Target{InstanceID: "ins-exec0001", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	ctx := context.Background()
	out, errb, exit, err := p.Exec(ctx, tg, "echo hi; echo oops >&2", nil, 1024)
	if err != nil || exit != 0 || string(out) != "hi\n" || string(errb) != "oops\n" {
		t.Fatalf("exec = %q %q %d %v", out, errb, exit, err)
	}
	if _, _, exit, err := p.Exec(ctx, tg, "exit 3", nil, 1024); err != nil || exit != 3 {
		t.Fatalf("exit 3 = %d %v", exit, err)
	}
	// stdin 传递（部署包上传就是这条路径）
	out, _, exit, err = p.Exec(ctx, tg, "cat > f && wc -c < f", []byte("12345"), 1024)
	if err != nil || exit != 0 || strings.TrimSpace(string(out)) != "5" {
		t.Fatalf("stdin = %q %d %v", out, exit, err)
	}
	// 截断
	out, errb, exit, err = p.Exec(ctx, tg, "head -c 5000 /dev/zero | tr '\\0' x; head -c 3000 /dev/zero | tr '\\0' y >&2", nil, 100)
	if err != nil || exit != 0 || len(out) != 100 || len(errb) != 100 {
		t.Fatalf("truncate = %d %d %d %v", len(out), len(errb), exit, err)
	}
	if p.Len() != 1 || srv.ConnCount() != 1 {
		t.Fatalf("pool=%d conns=%d, want one shared connection", p.Len(), srv.ConnCount())
	}
	// 不可达 → DialError
	var de *upstream.DialError
	if _, _, _, err := p.Exec(ctx, upstream.Target{InstanceID: "ins-exec0002", LanIP: "10.255.255.1", HostPubkey: "x"}, "true", nil, 10); !errors.As(err, &de) {
		t.Fatalf("unreachable = %v", err)
	}
	p.Close()
	if _, _, _, err := p.Exec(ctx, tg, "true", nil, 10); !errors.Is(err, upstream.ErrClosed) {
		t.Fatalf("closed = %v", err)
	}
}

func TestPoolExecTimeoutKeepsConnection(t *testing.T) {
	up := signer(t)
	srv := shellServer(t, up)
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: time.Second}, &memStore{pubs: map[string]string{}})
	defer p.Close()
	tg := upstream.Target{InstanceID: "ins-exec0003", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, _, exit, err := p.Exec(ctx, tg, "echo partial; sleep 10; echo late", nil, 1024)
	var de *upstream.DialError
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &de) || exit != -1 || string(out) != "partial\n" || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout = %q %d %v after %v", out, exit, err, time.Since(start))
	}
	// 共享连接仍可用
	if out, _, exit, err := p.Exec(context.Background(), tg, "echo again", nil, 64); err != nil || exit != 0 || string(out) != "again\n" {
		t.Fatalf("after timeout = %q %d %v", out, exit, err)
	}
	if p.Len() != 1 || srv.ConnCount() != 1 {
		t.Fatalf("pool=%d conns=%d", p.Len(), srv.ConnCount())
	}
}

func TestPoolExecRetriesDeadClient(t *testing.T) {
	up := signer(t)
	srv := shellServer(t, up)
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: time.Second}, &memStore{pubs: map[string]string{}})
	defer p.Close()
	tg := upstream.Target{InstanceID: "ins-exec0004", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	for i := 0; i < 5; i++ {
		if out, _, exit, err := p.Exec(context.Background(), tg, "echo ok", nil, 64); err != nil || exit != 0 || string(out) != "ok\n" {
			t.Fatalf("round %d = %q %d %v", i, out, exit, err)
		}
		srv.DropAll() // 上游崩溃：下一次 Exec 发现连接已死，重建一次
	}
}

// 命令不读 stdin 就以 0 退出：Wait 报 stdin 拷贝错误，但命令已执行过——不得当作连接错误重跑，也不得丢弃共享连接。
func TestPoolExecUnreadStdinRunsOnce(t *testing.T) {
	up := signer(t)
	srv := shellServer(t, up)
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: time.Second}, &memStore{pubs: map[string]string{}})
	defer p.Close()
	tg := upstream.Target{InstanceID: "ins-exec0005", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	if out, errb, exit, err := p.Exec(context.Background(), tg, "echo run >> runs", make([]byte, 8<<20), 64); err != nil || exit != 0 {
		t.Fatalf("exec = %q %q %d %v", out, errb, exit, err)
	}
	if out, _, exit, err := p.Exec(context.Background(), tg, "wc -l < runs", nil, 64); err != nil || exit != 0 || strings.TrimSpace(string(out)) != "1" {
		t.Fatalf("runs = %q %d %v, want the command executed once", out, exit, err)
	}
	if p.Len() != 1 || srv.ConnCount() != 1 {
		t.Fatalf("pool=%d conns=%d, want the shared connection kept", p.Len(), srv.ConnCount())
	}
}
