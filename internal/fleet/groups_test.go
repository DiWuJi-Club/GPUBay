package fleet_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
)

var opsActor = audit.Actor{AdminID: 1, IP: "127.0.0.1"}

func TestGroupCRUDAndAssign(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	g, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: " A 区 ", Region: "一楼", Visible: true})
	if err != nil || g.Name != "A 区" || g.Region != "一楼" || !g.Visible {
		t.Fatalf("create = %+v, %v", g, err)
	}
	if _, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "A 区"}); !errors.Is(err, apperr.ErrDuplicateGroupName) {
		t.Fatalf("duplicate name = %v", err)
	}
	if _, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: ""}); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("empty name = %v", err)
	}
	m1 := dbtest.CreateMachine(t, pool, "SN1")
	m2 := dbtest.CreateMachine(t, pool, "SN2")
	if err := svc.AssignMachines(ctx, opsActor, g.ID, []int64{m1, m2}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AssignMachines(ctx, opsActor, g.ID, []int64{m1, 999999}); apperr.As(err) == nil {
		t.Fatalf("unknown machine = %v", err)
	}
	rows, err := svc.ListGroups(ctx, false)
	if err != nil || len(rows) != 1 || rows[0].MachineCount != 2 || rows[0].IdleNow != 2 {
		t.Fatalf("list = %+v, %v", rows, err)
	}
	if err := svc.DeleteGroupIn(ctx, pool, g.ID); !errors.Is(err, apperr.ErrGroupNotEmpty) {
		t.Fatalf("delete non-empty = %v", err)
	}
	hidden, _ := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "留池", Visible: false})
	if vis, _ := svc.ListGroups(ctx, true); len(vis) != 1 || vis[0].ID != g.ID {
		t.Fatalf("visible only = %+v", vis)
	}
	// 展示名：唯一、≤ 32 字、三态改组
	name := "A-01"
	m, err := svc.UpdateMachine(ctx, opsActor, m1, fleet.MachineUpdate{DisplayName: &name})
	if err != nil || m.DisplayName != "A-01" || m.Label() != "A-01" || m.GroupID == nil || *m.GroupID != g.ID {
		t.Fatalf("update display = %+v, %v", m, err)
	}
	if _, err := svc.UpdateMachine(ctx, opsActor, m2, fleet.MachineUpdate{DisplayName: &name}); !errors.Is(err, apperr.ErrDuplicateDisplayName) {
		t.Fatalf("dup display = %v", err)
	}
	m, err = svc.UpdateMachine(ctx, opsActor, m2, fleet.MachineUpdate{SetGroup: true, GroupID: &hidden.ID})
	if err != nil || m.GroupID == nil || *m.GroupID != hidden.ID || m.Label() != "机器 #"+itoa(m2) {
		t.Fatalf("move group = %+v, %v", m, err)
	}
	m, err = svc.UpdateMachine(ctx, opsActor, m2, fleet.MachineUpdate{SetGroup: true})
	if err != nil || m.GroupID != nil {
		t.Fatalf("ungroup = %+v, %v", m, err)
	}
	bogus := int64(424242)
	if _, err := svc.UpdateMachine(ctx, opsActor, m2, fleet.MachineUpdate{SetGroup: true, GroupID: &bogus}); !errors.Is(err, apperr.ErrGroupNotFound) {
		t.Fatalf("unknown group = %v", err)
	}
	if err := svc.UnassignMachines(ctx, opsActor, g.ID, []int64{m1}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteGroupIn(ctx, pool, g.ID); err != nil {
		t.Fatalf("delete empty = %v", err)
	}
	if _, err := svc.GetGroup(ctx, pool, g.ID); !errors.Is(err, apperr.ErrGroupNotFound) {
		t.Fatalf("after delete = %v", err)
	}
	var audits int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action LIKE 'group.%' OR action = 'machine.update'`).Scan(&audits)
	if audits < 7 { // create×2 + assign + update(display)+update(move)+update(ungroup) + unassign
		t.Fatalf("audits = %d", audits)
	}
}

func TestMarkAllocatedRequiresIdleAndOnline(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	m, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN001", LanIP: "10.0.1.3", ProductID: pid})
	_ = svc.SetMaintenance(ctx, m.ID, false)
	if _, err := svc.MarkAllocated(ctx, pool, m.ID); !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("offline machine must not be allocated, got %v", err)
	}
	if _, err := svc.Heartbeat(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	got, err := svc.MarkAllocated(ctx, pool, m.ID)
	if err != nil || got.State != fleet.StateAllocated {
		t.Fatalf("allocate = %+v, %v", got, err)
	}
	if _, err := svc.MarkAllocated(ctx, pool, m.ID); !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("second allocate want no capacity, got %v", err)
	}
	if err := svc.Release(ctx, pool, m.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.IdleCount(ctx, pid); n != 1 {
		t.Fatalf("after release IdleCount = %d, want 1", n)
	}
}

func itoa(n int64) string { return fmt.Sprintf("%d", n) }
