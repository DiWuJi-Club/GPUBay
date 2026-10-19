package adminapi_test

import (
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/rental"
)

func TestInstanceListAndDetail(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	uid := e.renter("13600000001")
	inst := e.place(uid)
	e.ack()
	fin := e.loginAs("fin", admin.RoleFinance)
	code, out := fin.do("GET", "/instances?state=running&user_id="+itoa(uid), nil)
	if code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/instances?user_id=abc", nil); code != 400 {
		t.Fatalf("bad user_id = %d %v", code, out)
	}
	code, out = fin.do("GET", "/instances/"+inst.ID, nil)
	if code != 200 || out["instance"].(map[string]any)["state"] != "running" || len(out["events"].([]any)) != 2 ||
		len(out["commands"].([]any)) != 1 {
		t.Fatalf("detail = %d %v", code, out)
	}
	order := out["orders"].([]any)[0].(map[string]any)
	if order["user_id"].(float64) != float64(uid) || order["amount"].(float64) != 6900 {
		t.Fatalf("order view = %v", order)
	}
	if code, _ := fin.do("GET", "/instances/ins-missing", nil); code != 404 {
		t.Fatalf("missing = %d", code)
	}
}

func TestInstanceAfterSales(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	e.machine("SN2")
	uid := e.renter("13600000001")
	inst := e.place(uid)
	e.ack()
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)

	if code, out := ops.do("POST", "/instances/"+inst.ID+"/extend", map[string]any{"seconds": 3600, "reason": "补偿"}); code != 200 || out["state"] != "running" {
		t.Fatalf("extend = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/instances/"+inst.ID+"/extend", map[string]any{"seconds": 3600, "reason": "补偿"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance extend = %d %v", code, out)
	}
	code, out := ops.do("POST", "/instances/"+inst.ID+"/migrate", map[string]any{"reason": "GPU 报错"})
	if code != 200 {
		t.Fatalf("migrate = %d %v", code, out)
	}
	moved := out["instance"].(map[string]any)["id"].(string)
	e.ack()

	if code, out := ops.do("POST", "/instances/"+moved+"/refund-release", map[string]any{"amount": 100, "reason": "x"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops refund = %d %v", code, out)
	}
	code, out = fin.do("GET", "/instances/"+moved+"/refund-quote", nil)
	if code != 200 || out["max"].(float64) != 6900 || out["suggested"].(float64) <= 0 {
		t.Fatalf("quote = %d %v", code, out)
	}
	suggested := out["suggested"]
	// 参数错误的请求带着正确动态码也只得 400，且不打开二次验证窗口：紧接着的请求不带动态码仍要求动态码
	for _, bad := range []map[string]any{{"amount": suggested, "reason": " "}, {"amount": -1, "reason": "故障"}} {
		bad["totp"] = fin.totp()
		if code, out := fin.do("POST", "/instances/"+moved+"/refund-release", bad); code != 400 {
			t.Fatalf("refund %v = %d %v", bad, code, out)
		}
	}
	if code, out := fin.do("POST", "/instances/"+moved+"/refund-release", map[string]any{"amount": suggested, "reason": "故障"}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("refund without totp = %d %v", code, out)
	}
	code, out = fin.do("POST", "/instances/"+moved+"/refund-release", map[string]any{"amount": suggested, "reason": "故障", "totp": fin.totp()})
	if code != 200 || out["refunded"] != suggested {
		t.Fatalf("refund = %d %v", code, out)
	}
	if got, _ := e.rental.Get(e.ctx, moved); got.State != rental.StateWiping {
		t.Fatalf("state after refund = %s", got.State)
	}
	if code, out := ops.do("POST", "/instances/"+moved+"/rewipe", map[string]any{}); code != 200 {
		t.Fatalf("rewipe = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/instances/"+inst.ID+"/rewipe", map[string]any{}); code != 409 {
		t.Fatalf("rewipe released instance = %d %v", code, out)
	}
}
