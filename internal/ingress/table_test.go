package ingress_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/ingress"
	"spark-cloud/internal/rental"
)

type memSource struct {
	mu     sync.Mutex
	routes []rental.IngressRoute
	err    error
	calls  int
}

func (m *memSource) ListIngressRoutes(context.Context) ([]rental.IngressRoute, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return append([]rental.IngressRoute(nil), m.routes...), m.err
}

func TestTableRefreshAndLookup(t *testing.T) {
	src := &memSource{routes: []rental.IngressRoute{
		{InstanceID: "ins-a1b2c3d4", UserID: 7, Port: 8888, Visibility: "private", State: rental.StateRunning, LanIP: "10.0.0.10", HostPubkey: "k"},
		{InstanceID: "ins-a1b2c3d4", UserID: 7, Port: 7860, Visibility: "public", State: rental.StateRunning, LanIP: "10.0.0.10", HostPubkey: "k"},
	}}
	tb := ingress.NewTable(src, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, ok := tb.Lookup("ins-a1b2c3d4", 8888); ok {
		t.Fatal("lookup before refresh succeeded")
	}
	if err := tb.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, ok := tb.Lookup("ins-a1b2c3d4", 7860)
	if !ok || r.Visibility != "public" || r.UserID != 7 {
		t.Fatalf("lookup = %+v %v", r, ok)
	}
	if _, ok := tb.Lookup("ins-a1b2c3d4", 9999); ok {
		t.Fatal("unknown port found")
	}
	// 刷新失败保留旧表
	src.mu.Lock()
	src.err = errors.New("db down")
	src.mu.Unlock()
	if err := tb.Refresh(context.Background()); err == nil {
		t.Fatal("refresh error swallowed")
	}
	if _, ok := tb.Lookup("ins-a1b2c3d4", 8888); !ok {
		t.Fatal("old routes dropped on failed refresh")
	}
	src.mu.Lock()
	src.err, src.routes = nil, nil
	src.mu.Unlock()
	tb.Refresh(context.Background())
	if len(tb.Routes()) != 0 {
		t.Fatal("removed routes still present")
	}
}

func TestTableRoutesIsCopy(t *testing.T) {
	src := &memSource{routes: []rental.IngressRoute{{InstanceID: "ins-a1b2c3d4", Port: 8888, Visibility: "private"}}}
	tb := ingress.NewTable(src, nil)
	if err := tb.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	rs := tb.Routes()
	rs[0].Visibility = "public"
	if r, _ := tb.Lookup("ins-a1b2c3d4", 8888); r.Visibility != "private" {
		t.Fatal("Routes() exposes internal state")
	}
	if tb.Routes()[0].Visibility != "private" {
		t.Fatal("Routes() exposes internal slice")
	}
}

// syncBuf 让 slog 在 Run 的 goroutine 写、测试 goroutine 读时无数据竞争。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestTableRunRefreshesLogsAndStops(t *testing.T) {
	src := &memSource{routes: []rental.IngressRoute{{InstanceID: "ins-a1b2c3d4", Port: 8888}}}
	var logs syncBuf
	tb := ingress.NewTable(src, slog.New(slog.NewTextHandler(&logs, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tb.Run(ctx, 5*time.Millisecond); close(done) }()

	waitFor(t, func() bool { _, ok := tb.Lookup("ins-a1b2c3d4", 8888); return ok })
	src.mu.Lock()
	src.err = errors.New("db down")
	src.mu.Unlock()
	waitFor(t, func() bool { return strings.Contains(logs.String(), "db down") })
	if _, ok := tb.Lookup("ins-a1b2c3d4", 8888); !ok {
		t.Fatal("Run dropped routes after failed refresh")
	}
	src.mu.Lock()
	src.err, src.routes = nil, []rental.IngressRoute{{InstanceID: "ins-zzzzzzzz", Port: 7860}}
	src.mu.Unlock()
	waitFor(t, func() bool { _, ok := tb.Lookup("ins-zzzzzzzz", 7860); return ok })
	if _, ok := tb.Lookup("ins-a1b2c3d4", 8888); ok {
		t.Fatal("stale route survived refresh")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on ctx cancel")
	}
}

// TestTableConcurrentAccess 在 -race 下验证读写并发安全。
func TestTableConcurrentAccess(t *testing.T) {
	src := &memSource{}
	tb := ingress.NewTable(src, nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					tb.Lookup("ins-a1b2c3d4", 8888)
					_ = tb.Routes()
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		src.mu.Lock()
		src.routes = []rental.IngressRoute{{InstanceID: "ins-a1b2c3d4", Port: 8000 + i%5}}
		src.mu.Unlock()
		if err := tb.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	_ = fmt.Sprint(tb.Routes())
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
