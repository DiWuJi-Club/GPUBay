package audit_test

import (
	"context"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func TestList(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	entries := []struct {
		at time.Time
		e  audit.Entry
	}{
		{dbtest.T0, audit.Actor{AdminID: 1, IP: "a"}.Entry("machine.register", "machine", "1", map[string]int{"x": 1})},
		{dbtest.T0.Add(time.Hour), audit.Actor{AdminID: 2, IP: "b"}.Entry("instance.extend", "instance", "ins-1", nil)},
		{dbtest.T0.Add(2 * time.Hour), audit.Actor{AdminID: 1, IP: "a"}.Entry("instance.extend", "instance", "ins-2", nil)},
	}
	for _, it := range entries {
		if err := audit.Record(ctx, pool, it.at, it.e); err != nil {
			t.Fatal(err)
		}
	}
	all, total, err := audit.List(ctx, pool, audit.Filter{}, paging.Page{})
	if err != nil || total != 3 || all[0].TargetID != "ins-2" || all[2].Action != "machine.register" || string(all[2].Diff) != `{"x": 1}` {
		t.Fatalf("all = %+v, %d, %v", all, total, err)
	}
	if list, total, _ := audit.List(ctx, pool, audit.Filter{ActorID: 1, Action: "instance.extend"}, paging.Page{}); total != 1 || list[0].TargetID != "ins-2" {
		t.Fatalf("actor+action = %+v", list)
	}
	from, to := dbtest.T0.Add(30*time.Minute), dbtest.T0.Add(90*time.Minute)
	if list, total, _ := audit.List(ctx, pool, audit.Filter{From: &from, To: &to}, paging.Page{}); total != 1 || list[0].TargetID != "ins-1" {
		t.Fatalf("time range = %+v", list)
	}
	if _, total, _ := audit.List(ctx, pool, audit.Filter{TargetType: "instance", TargetID: "ins-1"}, paging.Page{}); total != 1 {
		t.Fatalf("target = %d", total)
	}
}
