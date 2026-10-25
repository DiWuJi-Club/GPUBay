package rental_test

import (
	"errors"
	"strconv"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
)

func TestAdminDeleteGroupChecksInstanceRefs(t *testing.T) {
	e := newEnv(t, 1)
	actor := audit.Actor{AdminID: 1, IP: "127.0.0.1"}
	g, err := e.fleet.CreateGroup(e.ctx, actor, fleet.GroupInput{Name: "A 区", Region: "一楼", Visible: true})
	if err != nil {
		t.Fatal(err)
	}
	_, inst := e.place(catalog.D1d)
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET group_id = $2 WHERE id = $1`, inst.ID, g.ID); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"reserved", "running", "stopped"} {
		if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = $2 WHERE id = $1`, inst.ID, state); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.AdminDeleteGroup(e.ctx, actor, g.ID); !errors.Is(err, apperr.ErrGroupNotEmpty) {
			t.Fatalf("delete with %s instance = %v, want group_not_empty", state, err)
		}
	}
	if counts, err := e.svc.ReservedCountsByGroup(e.ctx); err != nil || len(counts) != 0 {
		t.Fatalf("reserved counts while stopped = %v, %v", counts, err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'released' WHERE id = $1`, inst.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AdminDeleteGroup(e.ctx, actor, g.ID); err != nil {
		t.Fatalf("delete after release = %v", err)
	}
	if got := e.get(inst.ID); got.GroupID != nil {
		t.Fatalf("instance group_id = %v, want nil", *got.GroupID)
	}
	var diff string
	if err := e.pool.QueryRow(e.ctx, `SELECT diff::text FROM audit_logs WHERE action = 'group.delete' AND target_id = $1`,
		strconv.FormatInt(g.ID, 10)).Scan(&diff); err != nil || diff != `{"name": "A 区", "region": "一楼"}` {
		t.Fatalf("group.delete audit = %q, %v", diff, err)
	}
	if err := e.svc.AdminDeleteGroup(e.ctx, actor, g.ID); !errors.Is(err, apperr.ErrGroupNotFound) {
		t.Fatalf("delete twice = %v, want group_not_found", err)
	}
}

func TestReservedCountsByGroup(t *testing.T) {
	e := newEnv(t, 1)
	g, _ := e.fleet.CreateGroup(e.ctx, audit.Actor{AdminID: 1, IP: "127.0.0.1"}, fleet.GroupInput{Name: "A 区"})
	_, inst := e.place(catalog.D1d)
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'reserved', group_id = $2 WHERE id = $1`, inst.ID, g.ID); err != nil {
		t.Fatal(err)
	}
	if counts, err := e.svc.ReservedCountsByGroup(e.ctx); err != nil || len(counts) != 1 || counts[g.ID] != 1 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
}

// 计划 9：Token 服务池不能删除；锁住机组行后先判断，池里有未终态实例时也报「Token 服务池不能删除」而不是 group_not_empty。
func TestAdminDeleteGroupRejectsLLMPool(t *testing.T) {
	e := newEnv(t, 1)
	actor := audit.Actor{AdminID: 1, IP: "127.0.0.1"}
	poolID, err := e.fleet.LLMPoolGroupID(e.ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AdminDeleteGroup(e.ctx, actor, poolID); !errors.Is(err, fleet.ErrPoolNotDeletable) {
		t.Fatalf("delete empty pool = %v", err)
	}
	_, inst := e.place(catalog.D1d)
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET group_id = $2 WHERE id = $1`, inst.ID, poolID); err != nil {
		t.Fatal(err)
	}
	err = e.svc.AdminDeleteGroup(e.ctx, actor, poolID)
	if ae := apperr.As(err); !errors.Is(err, fleet.ErrPoolNotDeletable) || ae == nil || ae.Code != "invalid_state" || ae.Message != "Token 服务池不能删除" {
		t.Fatalf("delete busy pool = %v", err)
	}
	if got := e.get(inst.ID); got.GroupID == nil || *got.GroupID != poolID {
		t.Fatalf("instance group_id changed: %v", got.GroupID)
	}
}
