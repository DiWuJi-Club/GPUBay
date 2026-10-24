package ingress_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/ingress"
	"spark-cloud/internal/rental"
)

type pubStore struct {
	mu   sync.Mutex
	pubs map[string]string
}

func (m *pubStore) SetSSHHostPubkeyIfEmpty(_ context.Context, id, pub string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pubs[id] != "" {
		return false, nil
	}
	m.pubs[id] = pub
	return true, nil
}

func (m *pubStore) SSHHostPubkey(_ context.Context, id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pubs[id], nil
}

// 经真实 upstream.Pool + sshtest（假容器 sshd）走 direct-tcpip：HTTP、WebSocket、端口未监听、sshd 崩溃后自动重建、lan_ip 变化、池关闭。
func TestIngressThroughUpstreamPool(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	up, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	stop := srv.ServeHTTP(8888, http.HandlerFunc(wsEchoHandler))
	defer stop()
	pool := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: 300 * time.Millisecond}, &pubStore{pubs: map[string]string{}})
	defer pool.Close()

	e := newEnvWith(t, pool)
	const id = "ins-a1b2c3d4"
	h := id + "-8888.spark.test"
	route := rental.IngressRoute{InstanceID: id, UserID: 7, Port: 8888, Visibility: "public", State: rental.StateRunning, LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	e.addRoute(route)
	e.addRoute(rental.IngressRoute{InstanceID: id, UserID: 7, Port: 7000, Visibility: "public", State: rental.StateRunning, LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()})

	for i := 0; i < 3; i++ {
		if r := e.get(h, "/"); r.StatusCode != 200 || body(r) != "plain" {
			t.Fatalf("get %d = %d", i, r.StatusCode)
		}
	}
	if srv.ConnCount() != 1 || pool.Len() != 1 {
		t.Fatalf("upstream conns=%d pool=%d, want one shared ssh connection", srv.ConnCount(), pool.Len())
	}
	// WebSocket 经 direct-tcpip 端到端，等待超过 ingress 的 Write/ReadTimeout
	wsRoundTrip(t, e.host, h, 1200*time.Millisecond)

	// 容器内端口未监听：上游拒绝通道 → 502 页，不告警，共享连接保留
	r := e.get(id+"-7000.spark.test", "/")
	if b := body(r); r.StatusCode != 502 || !strings.Contains(b, fmt.Sprintf(ingress.PageUpstreamFmt, 7000)) {
		t.Fatalf("not listening = %d %q", r.StatusCode, b)
	}
	if e.alerts.count() != 0 || srv.ConnCount() != 1 {
		t.Fatalf("not listening: alerts=%d conns=%d", e.alerts.count(), srv.ConnCount())
	}

	// 容器 sshd 崩溃：下一次请求自动重建连接
	srv.DropAll()
	if r := e.get(h, "/"); r.StatusCode != 200 || body(r) != "plain" {
		t.Fatalf("after crash = %d", r.StatusCode)
	}
	waitUntil(t, "rebuilt single upstream conn", func() bool { return srv.ConnCount() == 1 && pool.Len() == 1 })
	if e.alerts.count() != 0 {
		t.Fatalf("alerts after recoverable crash = %d", e.alerts.count())
	}

	// 实例换机：lan_ip 变为不可达地址 → 旧连接（含 Transport 空闲连接）不再复用，502 + upstream_unreachable
	e.src.mu.Lock()
	for i := range e.src.routes {
		e.src.routes[i].LanIP = "10.255.255.1"
	}
	e.src.mu.Unlock()
	e.table.Refresh(context.Background())
	r = e.get(h, "/")
	if b := body(r); r.StatusCode != 502 || !strings.Contains(b, "无法连接到实例端口") {
		t.Fatalf("moved to unreachable lan ip = %d %q", r.StatusCode, b)
	}
	if e.alerts.count() != 1 {
		t.Fatalf("alerts after lan ip change = %d", e.alerts.count())
	}
	waitUntil(t, "old upstream conn closed", func() bool { return srv.ConnCount() == 0 })

	// 换回可达地址：恢复
	e.src.mu.Lock()
	for i := range e.src.routes {
		e.src.routes[i].LanIP = "127.0.0.1"
	}
	e.src.mu.Unlock()
	e.table.Refresh(context.Background())
	if r := e.get(h, "/"); r.StatusCode != 200 || body(r) != "plain" {
		t.Fatalf("back on reachable ip = %d", r.StatusCode)
	}
	waitUntil(t, "one upstream conn again", func() bool { return srv.ConnCount() == 1 })

	// 平台退出（池已关闭）：502，不告警
	pool.Close()
	waitUntil(t, "pool conns closed", func() bool { return srv.ConnCount() == 0 })
	if r := e.get(h, "/"); r.StatusCode != 502 {
		t.Fatalf("closed pool = %d", r.StatusCode)
	}
	if e.alerts.count() != 1 {
		t.Fatalf("alerts after pool close = %d", e.alerts.count())
	}
}

// 路由带容器 sshd 端口（rental.IngressRoute.SSHPort）：经真实连接池按该端口拨，不用池的默认端口；端口变化后不复用旧的空闲连接。
func TestIngressDialsRouteSSHPort(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	up, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	alt, err := srv.ListenAlso(0)
	if err != nil {
		t.Fatal(err)
	}
	stop := srv.ServeHTTP(8888, http.HandlerFunc(wsEchoHandler))
	defer stop()
	// 池的默认端口无人监听：只有路由上的端口生效才连得上
	pool := upstream.New(upstream.Config{Signer: up, Port: 1, DialTimeout: 300 * time.Millisecond}, &pubStore{pubs: map[string]string{}})
	defer pool.Close()
	e := newEnvWith(t, pool)
	const id = "ins-a1b2c3d4"
	h := id + "-8888.spark.test"
	e.addRoute(rental.IngressRoute{InstanceID: id, UserID: 7, Port: 8888, Visibility: "public", State: rental.StateRunning,
		LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine(), SSHPort: alt})
	if r := e.get(h, "/"); r.StatusCode != 200 || body(r) != "plain" {
		t.Fatalf("via route ssh port = %d", r.StatusCode)
	}
	// 端口改为 0 → 池默认端口（不可达）：502，且不能复用经旧端口建立的空闲连接
	e.src.mu.Lock()
	for i := range e.src.routes {
		e.src.routes[i].SSHPort = 0
	}
	e.src.mu.Unlock()
	e.table.Refresh(context.Background())
	if r := e.get(h, "/"); r.StatusCode != 502 {
		t.Fatalf("fallback to pool port = %d", r.StatusCode)
	}
}
