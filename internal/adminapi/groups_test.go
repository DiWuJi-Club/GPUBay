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
	m3 := e.machine("SN3") // 未分组
	if code, out := ops.do("GET", "/machines?group_id="+itoa(gid)+"&page=1&page_size=50", nil); code != 200 || out["total"].(float64) != 2 {
		t.Fatalf("machines in group = %d %v", code, out)
	}
	code, out = ops.do("GET", "/machines?group_id=none&page_size=50", nil)
	if code != 200 || out["total"].(float64) != 1 || out["items"].([]any)[0].(map[string]any)["id"].(float64) != float64(m3) {
		t.Fatalf("ungrouped machines = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/machines?group_id=none&rack=NOPE", nil); code != 200 || out["total"].(float64) != 0 {
		t.Fatalf("ungrouped + rack = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/machines?group_id=abc", nil); code != 400 || codeOf(out) != "bad_request" {
		t.Fatalf("bad group_id = %d %v", code, out)
	}
	code, out = fin.do("GET", "/groups", nil)
	items := out["items"].([]any)
	// 迁移 00022 种子：不可见的「Token 服务池」（sort 1000，排在最后）
	if code != 200 || len(items) != 2 || items[1].(map[string]any)["name"] != "Token 服务池" {
		t.Fatalf("list = %d %v", code, out)
	}
	row := items[0].(map[string]any)
	poolRow := items[1].(map[string]any)
	if row["purpose"] != "rental" || poolRow["purpose"] != "llm" || poolRow["visible"] != false {
		t.Fatalf("purpose: %v / %v", row, poolRow)
	}
	poolID := itoa(int64(poolRow["id"].(float64)))
	// 计划 9：池机组不能对用户可见、不能改名、不能改用途、不能删除；不能经接口再建一个池
	if code, out := ops.do("PUT", "/groups/"+poolID, map[string]any{"name": "Token 服务池", "visible": true}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("pool visible = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/groups/"+poolID, map[string]any{"name": "Token 服务池", "visible": false, "purpose": "rental"}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("pool purpose = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/groups/"+poolID, map[string]any{"name": "推理池", "visible": false}); code != 409 || codeOf(out) != "invalid_state" ||
		out["message"] != "Token 服务池不能改名" {
		t.Fatalf("pool rename = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/groups/"+poolID, map[string]any{"name": "Token 服务池", "visible": false, "sort": 1000, "description": "推理节点"}); code != 200 || out["purpose"] != "llm" {
		t.Fatalf("pool edit = %d %v", code, out)
	}
	if code, out := ops.do("DELETE", "/groups/"+poolID, map[string]any{}); code != 409 || codeOf(out) != "invalid_state" {
		t.Fatalf("pool delete = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/groups", map[string]any{"name": "第二池", "purpose": "llm"}); code != 400 || codeOf(out) != "bad_request" {
		t.Fatalf("create llm group = %d %v", code, out)
	}
	if row["machine_count"].(float64) != 2 || row["idle_now"].(float64) != 2 || row["reserved_upcoming"].(float64) != 0 {
		t.Fatalf("row = %v", row)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m1), map[string]any{}); code != 400 || codeOf(out) != "bad_request" {
		t.Fatalf("empty machine update = %d %v", code, out)
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
	if code, out := ops.do("GET", "/groups", nil); code != 200 || len(out["items"].([]any)) != 1 { // 只剩种子「Token 服务池」
		t.Fatalf("after delete = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m1), map[string]any{"group_id": gid}); code != 404 || codeOf(out) != "group_not_found" {
		t.Fatalf("assign to deleted = %d %v", code, out)
	}
}
