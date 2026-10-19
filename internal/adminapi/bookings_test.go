package adminapi_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func (e *env) book(uid int64, start time.Time) rental.Instance {
	e.t.Helper()
	res, err := e.rental.PlaceBatch(e.ctx, rental.BatchRequest{UserID: uid, ProductID: dbtest.ProductID(e.t, e.pool), ImageID: dbtest.ImageID(e.t, e.pool),
		Count: 1, Duration: catalog.D1d, Quantity: 1, StartAt: &start})
	if err != nil {
		e.t.Fatalf("book: %v", err)
	}
	return res.Instances[0]
}

func TestBookingAdminAPI(t *testing.T) {
	e := newEnv(t)
	m1 := e.machine("SN1")
	m2 := e.machine("SN2")
	uid := e.renter("13600000001")
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(uid, start)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)

	code, out := fin.do("GET", "/bookings", nil)
	if code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	row := out["items"].([]any)[0].(map[string]any)
	if row["id"] != inst.ID || row["state"] != "reserved" || row["scheduled_start_at"] == nil || row["scheduled_end_at"] == nil {
		t.Fatalf("row = %v", row)
	}
	if end, _ := time.Parse(time.RFC3339, row["scheduled_end_at"].(string)); !end.Equal(start.Add(24*time.Hour)) || row["machine_state"] != "idle" || row["batch_id"] == nil {
		t.Fatalf("row end / machine_state / batch = %v", row)
	}
	if code, out := fin.do("GET", "/bookings?state=reserved&user_id=&group_id=&machine_id=&from=&to=&page=1&page_size=20", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("list with empty filters = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/bookings?user_id="+itoa(uid+1), nil); code != 200 || out["total"].(float64) != 0 {
		t.Fatalf("list other user = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/bookings?state=cancelled", nil); code != 200 || out["total"].(float64) != 0 {
		t.Fatalf("cancelled list = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/bookings?state=running", nil); code != 400 {
		t.Fatalf("bad state = %d %v", code, out)
	}
	code, out = fin.do("GET", "/bookings/upcoming?days=7", nil)
	if code != 200 || len(out["days"].([]any)) != 7 || out["days"].([]any)[0].(map[string]any)["count"].(float64) != 1 {
		t.Fatalf("upcoming = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/bookings/upcoming?days=0", nil); code != 400 {
		t.Fatalf("upcoming days=0 = %d %v", code, out)
	}
	code, out = fin.do("GET", "/dashboard", nil)
	if code != 200 || out["bookings_upcoming_7d"].(float64) != 1 || out["instances"].(map[string]any)["reserved"].(float64) != 1 {
		t.Fatalf("dashboard = %d %v", code, out)
	}
	code, out = ops.do("GET", "/machines/"+itoa(inst.MachineID), nil)
	leases := out["leases"].([]any)
	if code != 200 || len(leases) != 1 || leases[0].(map[string]any)["instance_id"] != inst.ID || out["upcoming_bookings"].(float64) != 1 {
		t.Fatalf("machine detail = %d %v", code, out)
	}
	if out["group"] != nil {
		t.Fatalf("ungrouped machine group = %v", out["group"])
	}
	// 改派：指定机器
	target := m2
	if inst.MachineID == m2 {
		target = m1
	}
	code, out = ops.do("GET", "/machines/"+itoa(target), nil)
	if l, ok := out["leases"].([]any); code != 200 || !ok || len(l) != 0 || out["upcoming_bookings"].(float64) != 0 {
		t.Fatalf("idle machine detail must have leases [] = %d %v", code, out)
	}
	code, out = ops.do("POST", "/instances/"+inst.ID+"/reassign", map[string]any{"machine_id": target, "reason": "换机柜"})
	if code != 200 || out["machine_id"].(float64) != float64(target) || out["reassign_count"].(float64) != 1 {
		t.Fatalf("reassign = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/machines/"+itoa(target)+"/calendar?days=7", nil); code != 200 || len(out["periods"].([]any)) != 1 {
		t.Fatalf("calendar = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/instances/"+inst.ID+"/reassign", map[string]any{"machine_id": target, "reason": "再来"}); code != 409 || codeOf(out) != "machine_unavailable" {
		t.Fatalf("reassign to busy machine = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/instances/"+inst.ID+"/reassign", map[string]any{"reason": "  "}); code != 400 {
		t.Fatalf("reassign without reason = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/instances/"+inst.ID+"/reassign", map[string]any{"reason": "自动"}); code != 200 || out["reassign_count"].(float64) != 2 {
		t.Fatalf("auto reassign = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/instances/"+inst.ID+"/reassign", map[string]any{"reason": "x"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance reassign = %d %v", code, out)
	}
	// 进入维护时的提示数：对预订当前所在的机器（自动改派后要重读）
	cur, _ := e.rental.Get(e.ctx, inst.ID)
	code, out = ops.do("POST", "/machines/"+itoa(cur.MachineID)+"/maintenance", map[string]any{"on": true})
	if code != 200 || out["state"] != "maintenance" || out["upcoming_bookings"].(float64) != 1 {
		t.Fatalf("maintenance = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/bookings", nil); code != 200 || out["items"].([]any)[0].(map[string]any)["machine_state"] != "maintenance" {
		t.Fatalf("machine_state after maintenance = %d %v", code, out)
	}
	// 订单按批次筛选与 CSV
	batchID := ""
	_ = e.pool.QueryRow(e.ctx, `SELECT batch_id FROM instances WHERE id = $1`, inst.ID).Scan(&batchID)
	if code, out := fin.do("GET", "/orders?batch_id="+batchID, nil); code != 200 || out["total"].(float64) != 1 || out["items"].([]any)[0].(map[string]any)["quantity"].(float64) != 1 {
		t.Fatalf("orders by batch = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/orders?batch_id=nope", nil); code != 200 || out["total"].(float64) != 0 {
		t.Fatalf("orders by other batch = %d %v", code, out)
	}
	resp, _ := fin.send("GET", "/orders/export?batch_id="+batchID, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("export = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	raw, _ := io.ReadAll(resp.Body)
	csvText := string(raw)
	if !strings.HasPrefix(csvText, "\xEF\xBB\xBF订单号,用户ID,类型,实例,租期,数量,金额(元),已退(元),状态,批次,自动续费,下单时间\n") ||
		!strings.Contains(csvText, ",1d,1,69.00,0.00,paid,"+batchID+",,") || strings.Count(csvText, "\n") != 2 {
		t.Fatalf("csv = %q", csvText)
	}
	// 取消预订：需要动态码；全额退款；审计
	if code, out := ops.do("POST", "/instances/"+inst.ID+"/cancel-booking", map[string]any{"reason": "机房断电"}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("cancel without totp = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/instances/"+inst.ID+"/cancel-booking", map[string]any{"reason": "机房断电", "totp": fin.totp()}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance cancel = %d %v", code, out)
	}
	code, out = ops.do("POST", "/instances/"+inst.ID+"/cancel-booking", map[string]any{"reason": "机房断电", "totp": ops.totp()})
	if code != 200 || out["refunded"].(float64) != 6900 {
		t.Fatalf("cancel = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/bookings?state=cancelled", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("cancelled list after = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/machines/"+itoa(cur.MachineID), nil); code != 200 || out["current_instance"] != nil || out["upcoming_bookings"].(float64) != 0 {
		t.Fatalf("cancelled booking must not be current_instance = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/dashboard", nil); code != 200 || out["instances"].(map[string]any)["cancelled"].(float64) != 1 || out["bookings_upcoming_7d"].(float64) != 0 {
		t.Fatalf("dashboard after cancel = %d %v", code, out)
	}
	var audits int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action IN ('booking.reassign', 'booking.admin_cancel')`).Scan(&audits)
	if audits != 3 {
		t.Fatalf("audits = %d", audits)
	}
}

// 预订实例（reserved）可以由后台补偿加时。
func TestAdminExtendReservedBooking(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	inst := e.book(e.renter("13600000001"), dbtest.T0.Add(3*time.Hour))
	ops := e.loginAs("ops", admin.RoleOps)
	code, out := ops.do("POST", "/instances/"+inst.ID+"/extend", map[string]any{"seconds": 3600, "reason": "补偿"})
	if code != 200 || out["state"] != "reserved" || out["pending_seconds"].(float64) != 86400+3600 {
		t.Fatalf("extend reserved = %d %v", code, out)
	}
}
