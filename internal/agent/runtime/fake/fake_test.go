package fake_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/agent/runtime"
	"spark-cloud/internal/agent/runtime/fake"
)

func TestFakeRuntimeLifecycle(t *testing.T) {
	rt := fake.New()
	ctx := context.Background()
	spec := runtime.Spec{InstanceID: "ins-a", ImageRef: "img:1", DataDir: "/mnt/ins-a"}
	if err := rt.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := rt.Create(ctx, spec); err == nil {
		t.Fatal("duplicate create must conflict")
	}
	c, _ := rt.Inspect(ctx, "ins-a")
	if !c.Exists || c.Running || c.Image != "img:1" || c.Label != "ins-a" || !c.DataMounted || c.DataSource != "/mnt/ins-a" {
		t.Fatalf("created = %+v", c)
	}
	_ = rt.Start(ctx, "ins-a")
	if c, _ = rt.Inspect(ctx, "ins-a"); !c.Running {
		t.Fatal("not running")
	}
	rt.Exit("ins-a")
	if c, _ = rt.Inspect(ctx, "ins-a"); c.Running {
		t.Fatal("exit ignored")
	}
	rt.SetDataMounted("ins-a", false)
	if c, _ = rt.Inspect(ctx, "ins-a"); c.DataMounted || c.DataSource != "" {
		t.Fatal("data mount flag")
	}
	rt.SetDataSource("ins-a", "/elsewhere")
	if c, _ = rt.Inspect(ctx, "ins-a"); !c.DataMounted || c.DataSource != "/elsewhere" {
		t.Fatalf("data source = %+v", c)
	}
	rt.FailNext("start", errors.New("boom"))
	if err := rt.Start(ctx, "ins-a"); err == nil || err.Error() != "boom" {
		t.Fatalf("fail next = %v", err)
	}
	if err := rt.Start(ctx, "ins-a"); err != nil {
		t.Fatal("FailNext must be one-shot")
	}
	if err := rt.Stop(ctx, "nope"); err != nil {
		t.Fatal("stop missing ok")
	}
	list, _ := rt.List(ctx)
	if len(list) != 1 || list[0].Label != "ins-a" {
		t.Fatalf("list = %+v", list)
	}
	if v, _ := rt.VethOf(ctx, "ins-a"); v != "veth0abc" {
		t.Fatalf("veth = %s", v)
	}
	_ = rt.Pull(ctx, "img:2")
	if p := rt.Pulled(); len(p) != 1 || p[0] != "img:2" {
		t.Fatalf("pulled = %v", p)
	}
	_ = rt.Remove(ctx, "ins-a")
	if c, _ = rt.Inspect(ctx, "ins-a"); c.Exists {
		t.Fatal("not removed")
	}
}

func TestFakeNetworksAndPublishIPs(t *testing.T) {
	rt := fake.New()
	ctx := context.Background()
	_ = rt.Create(ctx, runtime.Spec{InstanceID: "ins-a", ImageRef: "img:1", LanIP: "192.168.1.221", DataDir: "/mnt/ins-a"})
	c, _ := rt.Inspect(ctx, "ins-a")
	if len(c.Networks) != 1 || c.Networks[0] != "spark0" || len(c.PublishIPs) != 1 || c.PublishIPs[0] != "192.168.1.221" {
		t.Fatalf("created = %+v", c)
	}
	rt.SetNetworks("ins-a", nil)
	if c, _ = rt.Inspect(ctx, "ins-a"); len(c.Networks) != 0 {
		t.Fatalf("networks = %v", c.Networks)
	}
	// 没有网络挂接时 docker start 成功但容器里没有网卡
	_ = rt.Start(ctx, "ins-a")
	if _, err := rt.VethOf(ctx, "ins-a"); err == nil {
		t.Fatal("VethOf must fail for a container started without networks")
	}
	_ = rt.Stop(ctx, "ins-a")
	if err := rt.ConnectNetwork(ctx, "ins-a", "spark0"); err != nil {
		t.Fatal(err)
	}
	// 与 docker 一致：已挂接时再次 connect 报错（endpoint already exists）
	if err := rt.ConnectNetwork(ctx, "ins-a", "spark0"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second connect = %v", err)
	}
	if c, _ = rt.Inspect(ctx, "ins-a"); len(c.Networks) != 1 {
		t.Fatalf("networks = %v", c.Networks)
	}
	if rt.ConnectNetwork(ctx, "ins-missing", "spark0") == nil {
		t.Fatal("connect on missing container must fail")
	}
	want := []string{"start ins-a", "veth ins-a", "stop ins-a", "connect ins-a", "connect ins-a", "connect ins-missing"}
	if got := rt.Ops(); !slices.Equal(got[len(got)-len(want):], want) {
		t.Fatalf("ops = %v", got)
	}
}

// Block 让 List / Inspect 一直阻塞到 ctx 结束（模拟 dockerd 卡死），返回 ctx 的错误。
func TestFakeBlockWaitsForContext(t *testing.T) {
	rt := fake.New()
	rt.Block("list", true)
	rt.Block("inspect", true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := rt.List(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("list = %v", err)
	}
	if _, err := rt.Inspect(ctx, "ins-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inspect = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("block ignored ctx")
	}
	rt.Block("list", false)
	if _, err := rt.List(context.Background()); err != nil {
		t.Fatal(err)
	}
}
