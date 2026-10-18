package fake_test

import (
	"context"
	"errors"
	"testing"

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
	if !c.Exists || c.Running || c.Image != "img:1" || c.Label != "ins-a" || !c.DataMounted {
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
	if c, _ = rt.Inspect(ctx, "ins-a"); c.DataMounted {
		t.Fatal("data mount flag")
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
