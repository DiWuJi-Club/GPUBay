package upstream_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
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
	// 未带写回的公钥再拨（路由表还没刷新）：不再首连信任，拒绝
	if _, err := httpGet(t, p, legacy, 8888); !errors.As(err, &de) {
		t.Fatalf("second tofu = %v", err)
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
