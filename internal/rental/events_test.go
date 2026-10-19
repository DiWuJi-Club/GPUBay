package rental_test

import (
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/dbtest"
)

func TestUserEventsWhitelistAndMasking(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(3 * time.Hour)
	req := e.batchReq(1, &start)
	req.MachineID = &e.machines[0]
	res, _ := e.svc.PlaceBatch(e.ctx, req)
	inst := res.Instances[0]
	e.setMachine(e.machines[0], "maintenance", true)
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	_, _ = e.svc.ProvisionDue(e.ctx) // 改派到机器 1
	e.ack(true)
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatal(err)
	}
	events, err := e.svc.UserEvents(e.ctx, e.userID, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		if ev.Detail == "" {
			t.Fatalf("empty detail: %+v", ev)
		}
		for _, id := range e.machines {
			if strings.Contains(ev.Detail, itoa64(id)) && ev.Kind == "reassigned" {
				t.Fatalf("machine id leaked: %+v", ev)
			}
		}
	}
	want := []string{"renew", "created", "provision", "reassigned", "order"} // 新到旧：开通链路先记 reassigned 再记 scheduled start
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if events[4].Detail != "预订下单，2026-01-01 11:00 开始" || events[3].Detail != "已为您更换机器" || events[2].Detail != "开始开通" {
		t.Fatalf("details = %+v", events)
	}
	if _, err := e.svc.UserEvents(e.ctx, e.userID+1, inst.ID); err == nil {
		t.Fatal("other user must get not_found")
	}
}
