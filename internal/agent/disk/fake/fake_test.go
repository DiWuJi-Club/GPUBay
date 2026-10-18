package fake_test

import (
	"context"
	"errors"
	"testing"

	"spark-cloud/internal/agent/disk/fake"
)

func TestFakeDisk(t *testing.T) {
	d := fake.New(10 << 30)
	ctx := context.Background()
	if free, _ := d.FreeBytes(ctx); free != 10<<30 {
		t.Fatal("free")
	}
	if err := d.Create(ctx, "ins-a", 2); err != nil {
		t.Fatal(err)
	}
	if free, _ := d.FreeBytes(ctx); free != 8<<30 {
		t.Fatalf("free after create = %d", free)
	}
	if m, _ := d.Mounted(ctx, "ins-a"); !m {
		t.Fatal("not mounted after create")
	}
	d.Unmount("ins-a")
	if m, _ := d.Mounted(ctx, "ins-a"); m {
		t.Fatal("unmount ignored")
	}
	if err := d.Mount(ctx, "ins-a"); err != nil {
		t.Fatal(err)
	}
	if m, _ := d.Mounted(ctx, "ins-a"); !m {
		t.Fatal("mount")
	}
	d.FailNext("destroy", errors.New("busy"))
	if err := d.Destroy(ctx, "ins-a"); err == nil {
		t.Fatal("fail next")
	}
	if err := d.Destroy(ctx, "ins-a"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.Exists(ctx, "ins-a"); ok {
		t.Fatal("exists after destroy")
	}
	if free, _ := d.FreeBytes(ctx); free != 10<<30 {
		t.Fatal("free not restored")
	}
	if err := d.Mount(ctx, "ins-a"); err == nil {
		t.Fatal("mount missing image must fail")
	}
}
