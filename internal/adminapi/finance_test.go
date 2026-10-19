package adminapi_test

import (
	"io"
	"strings"
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
)

func TestFinanceListsAndExports(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	uid := e.renter("13600000001")
	e.place(uid)
	if _, err := e.billing.ManualRecharge(e.ctx, audit.Actor{AdminID: 1}, uid, 10000, "=HYPERLINK(\"x\")"); err != nil {
		t.Fatal(err)
	}
	fin := e.loginAs("fin", admin.RoleFinance)
	code, out := fin.do("GET", "/orders?user_id="+itoa(uid), nil)
	if code != 200 || out["total"].(float64) != 1 || out["items"].([]any)[0].(map[string]any)["user_id"].(float64) != float64(uid) {
		t.Fatalf("orders = %d %v", code, out)
	}
	if code, _ := fin.do("GET", "/orders?kind=gift", nil); code != 400 {
		t.Fatalf("bad kind = %d", code)
	}
	if code, out := fin.do("GET", "/orders?from=2026-01-02", nil); code != 200 || out["total"].(float64) != 0 {
		t.Fatalf("from next day = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/orders?to=2026-01-01", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("to same day (inclusive) = %d %v", code, out)
	}
	if code, _ := fin.do("GET", "/orders?from=2026-01-02&to=2026-01-01", nil); code != 400 {
		t.Fatalf("inverted range = %d", code)
	}
	if code, _ := fin.do("GET", "/orders?from=yesterday", nil); code != 400 {
		t.Fatalf("bad time = %d", code)
	}
	if code, out := fin.do("GET", "/ledger?user_id="+itoa(uid)+"&kind=order_pay", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("ledger = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/recharges?channel=manual", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("recharges = %d %v", code, out)
	}

	resp, _ := fin.send("GET", "/orders/export", nil)
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), `attachment; filename="orders-`) ||
		!strings.HasPrefix(text, "\xEF\xBB\xBF订单号,") || !strings.Contains(text, ",69.00,") || strings.Count(text, "\n") != 2 {
		t.Fatalf("orders csv = %d %v\n%s", resp.StatusCode, resp.Header, text)
	}
	resp, _ = fin.send("GET", "/recharges/export", nil)
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `'=HYPERLINK(`) || !strings.Contains(string(body), ",100.00,") {
		t.Fatalf("recharges csv must escape formulas and format yuan:\n%s", body)
	}
	resp, _ = fin.send("GET", "/ledger/export?kind=refund", nil)
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.Count(string(body), "\n") != 1 {
		t.Fatalf("empty ledger csv must still have a header:\n%s", body)
	}

	ops := e.loginAs("ops", admin.RoleOps)
	if code, out := ops.do("GET", "/orders", nil); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops orders = %d %v", code, out)
	}
}
