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

// 用户事件只扫描最近 200 条原始事件；未知的自动续费失败原因有兜底文案。
func TestUserEventsScanLimitAndUnknownAutoRenewReason(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	noise := func(n int) {
		if _, err := e.pool.Exec(e.ctx, `INSERT INTO instance_events (instance_id, from_state, to_state, reason, at)
			SELECT $1, 'running', 'running', 'internal noise', $2 FROM generate_series(1, $3)`, inst.ID, e.clock.Now(), n); err != nil {
			t.Fatal(err)
		}
	}
	noise(150)
	if evs, err := e.svc.UserEvents(e.ctx, e.userID, inst.ID); err != nil || len(evs) != 1 || evs[0].Kind != "order" {
		t.Fatalf("within limit = %+v, %v", evs, err)
	}
	noise(100) // 「下单」事件落到最近 200 条之外
	if evs, err := e.svc.UserEvents(e.ctx, e.userID, inst.ID); err != nil || len(evs) != 0 {
		t.Fatalf("beyond limit = %+v, %v", evs, err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instance_events (instance_id, from_state, to_state, reason, at)
		VALUES ($1, 'running', 'running', 'auto renew failed: something_new', $2)`, inst.ID, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if evs, err := e.svc.UserEvents(e.ctx, e.userID, inst.ID); err != nil || len(evs) != 1 || evs[0].Detail != "自动续费失败：其他原因" {
		t.Fatalf("unknown auto renew reason = %+v, %v", evs, err)
	}
}
