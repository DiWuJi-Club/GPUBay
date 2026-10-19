package adminapi_test

import (
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/dbtest"
)

func TestDashboard(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	e.machine("SN2")
	uid := e.renter("13600000001")
	e.place(uid)
	e.ack()
	if _, err := e.billing.ManualRecharge(e.ctx, audit.Actor{AdminID: 1}, uid, 5000, "转账"); err != nil {
		t.Fatal(err)
	}
	ops := e.loginAs("ops", admin.RoleOps)
	code, out := ops.do("GET", "/dashboard", nil)
	if code != 200 {
		t.Fatalf("dashboard = %d %v", code, out)
	}
	machines := out["machines"].(map[string]any)
	instances := out["instances"].(map[string]any)
	util := out["utilization"].([]any)
	if machines["total"].(float64) != 2 || instances["running"].(float64) != 1 ||
		out["order_today"].(float64) != 6900 || out["order_month"].(float64) != 6900 ||
		out["recharge_today"].(float64) != 5000 || out["recharge_month"].(float64) != 5000 || len(util) != 30 {
		t.Fatalf("dashboard = %v", out)
	}
	today := util[29].(map[string]any)
	// T0 = 北京时间 2026-01-01 08:00；登录等操作把时钟推进了 1 分钟，今天已过去约 8 小时，2 台机器中 1 台在租
	if today["date"] != dbtest.T0.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02") || today["rate"].(float64) <= 0 || today["rate"].(float64) > 0.5 {
		t.Fatalf("today utilization = %v", today)
	}
}
