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
	rows, err := svc.ListGroups(ctx, false, nil)
	if err != nil || len(rows) != 1 || rows[0].MachineCount != 2 || rows[0].IdleNow != 2 {
		t.Fatalf("list = %+v, %v", rows, err)
	}
	if err := svc.DeleteGroupIn(ctx, pool, g.ID); !errors.Is(err, apperr.ErrGroupNotEmpty) {
		t.Fatalf("delete non-empty = %v", err)
	}
	hidden, _ := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "留池", Visible: false})
	if vis, _ := svc.ListGroups(ctx, true, nil); len(vis) != 1 || vis[0].ID != g.ID {
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
	// 失败的操作整笔回滚，不留审计
	for action, want := range map[string]int{"group.create": 2, "group.assign": 1, "machine.update": 3, "group.unassign": 1, "group.delete": 0} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = $1`, action).Scan(&n); err != nil || n != want {
			t.Fatalf("audit %s = %d (%v), want %d", action, n, err, want)
		}
	}
}

func TestGroupValidation(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	isBadRequest := func(err error) bool { return apperr.As(err) != nil && apperr.As(err).Code == "bad_request" }
	for _, in := range []fleet.GroupInput{
		{Name: "A\u200b区"},                 // 零宽空格（Cf）
		{Name: "A\x01区"},                   // 控制字符
		{Name: "A 区", Region: "一\n楼"},      // 位置标签不许换行
		{Name: "A 区", Description: "a\tb"}, // 描述只放过 \n
		{Name: "A 区", Sort: 1_000_001},
		{Name: "A 区", Sort: -1_000_001},
	} {
		if _, err := svc.CreateGroup(ctx, opsActor, in); !isBadRequest(err) {
			t.Fatalf("create %+v = %v, want bad_request", in, err)
		}
	}
	g, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "A 区", Description: "第一行\n第二行", Sort: 1_000_000})
	if err != nil || g.Description != "第一行\n第二行" || g.Sort != 1_000_000 {
		t.Fatalf("create = %+v, %v", g, err)
	}
	g2, _ := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "B 区"})
	m1 := dbtest.CreateMachine(t, pool, "SN1")
	m2 := dbtest.CreateMachine(t, pool, "SN2")
	m3 := dbtest.CreateMachine(t, pool, "SN3")

	// 分配：ID 必须为正、去重后最多 500 台；审计记去重后的列表与各机原机组
	if err := svc.AssignMachines(ctx, opsActor, g.ID, []int64{m1, 0}); !isBadRequest(err) {
		t.Fatalf("zero id = %v", err)
	}
	many := make([]int64, 501)
	for i := range many {
		many[i] = int64(i + 1)
	}
	if err := svc.AssignMachines(ctx, opsActor, g.ID, many); !isBadRequest(err) {
		t.Fatalf("501 ids = %v", err)
	}
	if err := svc.AssignMachines(ctx, opsActor, g2.ID, []int64{m2}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AssignMachines(ctx, opsActor, g.ID, []int64{m1, m2, m1, m2}); err != nil {
		t.Fatalf("duplicate ids = %v", err)
	}
	var diff string
	_ = pool.QueryRow(ctx, `SELECT diff::text FROM audit_logs WHERE action = 'group.assign' AND target_id = $1`, itoa(g.ID)).Scan(&diff)
	want := fmt.Sprintf(`{"machine_ids": [%d, %d], "previous_group_ids": {"%d": null, "%d": %d}}`, m1, m2, m1, m2, g2.ID)
	if diff != want {
		t.Fatalf("assign audit = %s, want %s", diff, want)
	}

	// 移出：机组不存在 → ErrGroupNotFound；审计只记实际移出的机器
	if err := svc.UnassignMachines(ctx, opsActor, 424242, []int64{m1}); !errors.Is(err, apperr.ErrGroupNotFound) {
		t.Fatalf("unassign unknown group = %v", err)
	}
	if err := svc.UnassignMachines(ctx, opsActor, g.ID, []int64{m1, m3}); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT diff::text FROM audit_logs WHERE action = 'group.unassign'`).Scan(&diff)
	if want := fmt.Sprintf(`{"machine_ids": [%d]}`, m1); diff != want {
		t.Fatalf("unassign audit = %s, want %s", diff, want)
	}

	// 展示名：不许控制 / 格式字符，不许冒充缺省「机器 #<id>」
	for _, name := range []string{"A\u202e01", "A\n01", "机器 #12", "机器 #" + itoa(m3)} {
		if _, err := svc.UpdateMachine(ctx, opsActor, m3, fleet.MachineUpdate{DisplayName: &name}); !isBadRequest(err) {
			t.Fatalf("display %q = %v, want bad_request", name, err)
		}
	}
	ok := "机器 #A1"
	if m, err := svc.UpdateMachine(ctx, opsActor, m3, fleet.MachineUpdate{DisplayName: &ok}); err != nil || m.DisplayName != ok {
		t.Fatalf("display %q = %+v, %v", ok, m, err)
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
