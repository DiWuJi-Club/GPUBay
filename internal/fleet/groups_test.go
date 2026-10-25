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
	// 迁移 00022 种子：不可见的「Token 服务池」（sort 1000，排在最后）
	if err != nil || len(rows) != 2 || rows[0].ID != g.ID || rows[0].MachineCount != 2 || rows[0].IdleNow != 2 || rows[1].Name != "Token 服务池" {
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

// 计划 9：迁移 00022 建好唯一的 Token 服务池（purpose='llm'、不可见）。
func TestLLMPoolGroupID(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	id, err := svc.LLMPoolGroupID(ctx, pool)
	if err != nil || id <= 0 {
		t.Fatalf("LLMPoolGroupID = %d, %v", id, err)
	}
	g, err := svc.GetGroup(ctx, pool, id)
	if err != nil || g.Purpose != fleet.GroupPurposeLLM || g.Visible || g.Name != "Token 服务池" {
		t.Fatalf("pool group = %+v, %v", g, err)
	}
	rows, err := svc.ListGroups(ctx, false, nil)
	if err != nil || len(rows) != 1 || rows[0].Purpose != fleet.GroupPurposeLLM {
		t.Fatalf("list = %+v, %v", rows, err)
	}
	if vis, err := svc.ListGroups(ctx, true, nil); err != nil || len(vis) != 0 {
		t.Fatalf("visible list must not contain pool: %+v, %v", vis, err)
	}
	other, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "A 区", Visible: true})
	if err != nil || other.Purpose != fleet.GroupPurposeRental {
		t.Fatalf("create = %+v, %v", other, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE machine_groups SET purpose = 'rental' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LLMPoolGroupID(ctx, pool); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing pool = %v, want ErrNotFound", err)
	}
}

func TestPoolGroupCannotBeVisibleOrDeleted(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	id, err := svc.LLMPoolGroupID(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	// 可以改描述、排序、地区（保持不可见、名字不变）
	g, err := svc.UpdateGroup(ctx, opsActor, id, fleet.GroupInput{Name: "Token 服务池", Description: "d", Sort: 5, Visible: false})
	if err != nil || g.Name != "Token 服务池" || g.Description != "d" || g.Sort != 5 || g.Visible || g.Purpose != fleet.GroupPurposeLLM {
		t.Fatalf("update = %+v, %v", g, err)
	}
	// 不能改名
	_, err = svc.UpdateGroup(ctx, opsActor, id, fleet.GroupInput{Name: "推理池", Visible: false})
	if ae := apperr.As(err); ae == nil || ae.Code != "invalid_state" || ae.Message != "Token 服务池不能改名" {
		t.Fatalf("rename = %v", err)
	}
	_, err = svc.UpdateGroup(ctx, opsActor, id, fleet.GroupInput{Name: "Token 服务池", Visible: true})
	if ae := apperr.As(err); ae == nil || ae.Code != "invalid_state" || ae.Message != "Token 服务池不能对用户可见" {
		t.Fatalf("visible = %v", err)
	}
	_, err = svc.UpdateGroup(ctx, opsActor, id, fleet.GroupInput{Name: "Token 服务池", Purpose: fleet.GroupPurposeRental})
	if ae := apperr.As(err); ae == nil || ae.Code != "invalid_state" {
		t.Fatalf("purpose change = %v", err)
	}
	if err := svc.DeleteGroupIn(ctx, pool, id); !errors.Is(err, apperr.ErrInvalidState) && (apperr.As(err) == nil || apperr.As(err).Code != "invalid_state") {
		t.Fatalf("delete pool = %v", err)
	}
	if g, err := svc.GetGroup(ctx, pool, id); err != nil || g.Visible || g.Purpose != fleet.GroupPurposeLLM || g.Name != "Token 服务池" {
		t.Fatalf("pool after rejected ops = %+v, %v", g, err)
	}
	// 普通机组不能改成池
	r, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "A 区", Visible: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateGroup(ctx, opsActor, r.ID, fleet.GroupInput{Name: "A 区", Purpose: fleet.GroupPurposeLLM}); apperr.As(err) == nil {
		t.Fatalf("rental -> llm = %v", err)
	}
	if g, err := svc.UpdateGroup(ctx, opsActor, r.ID, fleet.GroupInput{Name: "A 区", Purpose: fleet.GroupPurposeRental, Visible: false}); err != nil || g.Purpose != fleet.GroupPurposeRental || g.Visible {
		t.Fatalf("rental update = %+v, %v", g, err)
	}
}

func TestCreateGroupRejectsLLMPurpose(t *testing.T) {
	svc, _, _ := newEnv(t)
	ctx := context.Background()
	_, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "第二个池", Purpose: fleet.GroupPurposeLLM})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" || ae.Message != "不能创建 Token 服务池机组" {
		t.Fatalf("create llm = %v", err)
	}
	if _, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "x", Purpose: "weird"}); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("create weird purpose = %v", err)
	}
	g, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "A 区", Purpose: fleet.GroupPurposeRental, Visible: true})
	if err != nil || g.Purpose != fleet.GroupPurposeRental {
		t.Fatalf("create rental = %+v, %v", g, err)
	}
}

// 计划 9 规格 §3.3：池里有未结束系统实例或非终态节点的机器不能离开池（移出、改组、加入别的机组），检查在机组事务里、机器行锁之后。
func TestPoolMachineWithLiveNodeCannotLeave(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	poolID, err := svc.LLMPoolGroupID(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "A 区", Visible: true})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	withNode := dbtest.CreateMachine(t, pool, "SN-NODE")
	withInst := dbtest.CreateMachine(t, pool, "SN-INST")
	free := dbtest.CreateMachine(t, pool, "SN-FREE")
	if err := svc.AssignMachines(ctx, opsActor, poolID, []int64{withNode, withInst, free}); err != nil {
		t.Fatal(err)
	}
	var modelID int64
	if err := pool.QueryRow(ctx, `INSERT INTO llm_models (slug, name, kind, source_id, image_id, context_len, input_price, output_price, created_at, updated_at)
		VALUES ('qwen3-4b', 'Qwen3 4B', 'chat', 'Qwen/Qwen3-4B', $1, 32768, 100, 400, $2, $2) RETURNING id`, dbtest.ImageID(t, pool), dbtest.T0).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO llm_nodes (id, model_id, machine_id, state, api_key_enc, created_at, updated_at)
		VALUES ('node-a', $1, $2, 'removing', '\x00', $3, $3)`, modelID, withNode, dbtest.T0)
	exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, password_auth, system, created_at, updated_at)
		SELECT 'ins-sys', u.id, m.id, m.product_id, $2, 'wiping', '\x00', false, true, $3, $3 FROM machines m, users u
		WHERE m.id = $1 AND u.system`, withInst, dbtest.ImageID(t, pool), dbtest.T0)

	for _, id := range []int64{withNode, withInst} {
		if err := svc.UnassignMachines(ctx, opsActor, poolID, []int64{id, free}); !errors.Is(err, apperr.ErrLLMPoolMachineBusy) {
			t.Fatalf("unassign %d = %v", id, err)
		}
		if _, err := svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetGroup: true}); !errors.Is(err, apperr.ErrLLMPoolMachineBusy) {
			t.Fatalf("ungroup %d = %v", id, err)
		}
		if _, err := svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetGroup: true, GroupID: &other.ID}); !errors.Is(err, apperr.ErrLLMPoolMachineBusy) {
			t.Fatalf("regroup %d = %v", id, err)
		}
		if err := svc.AssignMachines(ctx, opsActor, other.ID, []int64{id}); !errors.Is(err, apperr.ErrLLMPoolMachineBusy) {
			t.Fatalf("assign elsewhere %d = %v", id, err)
		}
		// 留在池里的操作照常
		if _, err := svc.UpdateMachine(ctx, opsActor, id, fleet.MachineUpdate{SetGroup: true, GroupID: &poolID}); err != nil {
			t.Fatalf("keep in pool %d = %v", id, err)
		}
		if err := svc.AssignMachines(ctx, opsActor, poolID, []int64{id}); err != nil {
			t.Fatalf("reassign to pool %d = %v", id, err)
		}
	}
	// 被拒的批量操作整体回滚：空闲机器仍在池里
	if m, _ := svc.Get(ctx, pool, free); m.GroupID == nil || *m.GroupID != poolID {
		t.Fatalf("free machine group = %v", m.GroupID)
	}
	// 从别的机组移出时不看池（池机器不受影响）
	if err := svc.UnassignMachines(ctx, opsActor, other.ID, []int64{withNode}); err != nil {
		t.Fatalf("unassign from other group = %v", err)
	}
	// 终态节点 / 已结束实例不再阻止
	exec(`UPDATE llm_nodes SET state = 'removed' WHERE id = 'node-a'`)
	exec(`UPDATE instances SET state = 'released' WHERE id = 'ins-sys'`)
	if err := svc.UnassignMachines(ctx, opsActor, poolID, []int64{withNode, withInst, free}); err != nil {
		t.Fatalf("unassign after node removed = %v", err)
	}
	if err := svc.AssignMachines(ctx, opsActor, other.ID, []int64{withNode}); err != nil {
		t.Fatalf("assign after = %v", err)
	}
}
