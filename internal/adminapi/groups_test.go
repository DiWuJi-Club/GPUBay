package adminapi_test

import (
	"testing"

	"spark-cloud/internal/admin"
)

func TestGroupAPI(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)
	m1 := e.machine("SN1")
	m2 := e.machine("SN2")

	code, out := ops.do("POST", "/groups", map[string]any{"name": "A 区", "region": "一楼", "visible": true})
	if code != 201 || out["name"] != "A 区" {
		t.Fatalf("create = %d %v", code, out)
	}
	gid := int64(out["id"].(float64))
	if code, out := ops.do("POST", "/groups", map[string]any{"name": "A 区"}); code != 409 || codeOf(out) != "duplicate_group_name" {
		t.Fatalf("dup = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/groups", map[string]any{"name": "B"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance create = %d %v", code, out)
	}
	if code, _ := ops.do("POST", "/groups/"+itoa(gid)+"/machines", map[string]any{"machine_ids": []int64{m1, m2}}); code != 200 {
		t.Fatalf("assign = %d", code)
	}
	code, out = fin.do("GET", "/groups", nil)
	items := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	row := items[0].(map[string]any)
	if row["machine_count"].(float64) != 2 || row["idle_now"].(float64) != 2 || row["reserved_upcoming"].(float64) != 0 {
		t.Fatalf("row = %v", row)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m1), map[string]any{"display_name": "A-01"}); code != 200 || out["display_name"] != "A-01" {
		t.Fatalf("update machine = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m2), map[string]any{"display_name": "A-01"}); code != 409 || codeOf(out) != "duplicate_display_name" {
		t.Fatalf("dup display = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/groups/"+itoa(gid), map[string]any{"name": "A 区", "region": "二楼", "visible": false, "sort": 5}); code != 200 || out["region"] != "二楼" || out["visible"] != false {
		t.Fatalf("update group = %d %v", code, out)
	}
	if code, out := ops.do("DELETE", "/groups/"+itoa(gid), map[string]any{}); code != 409 || codeOf(out) != "group_not_empty" {
		t.Fatalf("delete non-empty = %d %v", code, out)
	}
	if code, _ := ops.do("DELETE", "/groups/"+itoa(gid)+"/machines", map[string]any{"machine_ids": []int64{m1, m2}}); code != 200 {
		t.Fatalf("unassign = %d", code)
	}
	if code, _ := ops.do("DELETE", "/groups/"+itoa(gid), map[string]any{}); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if code, out := ops.do("GET", "/groups", nil); code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("after delete = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m1), map[string]any{"group_id": gid}); code != 404 || codeOf(out) != "group_not_found" {
		t.Fatalf("assign to deleted = %d %v", code, out)
	}
}
