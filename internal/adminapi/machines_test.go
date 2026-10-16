package adminapi_test

import (
	"fmt"
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
)

func TestRegisterAndImportMachines(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	pid := dbtest.ProductID(t, e.pool)
	code, out := ops.do("POST", "/machines", map[string]any{"serial": "SN1", "rack": "A01", "slot": "01", "lan_ip": "10.0.1.1", "product_id": pid})
	if code != 201 || out["state"] != "maintenance" {
		t.Fatalf("register = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/machines", map[string]any{"serial": "SN1", "lan_ip": "10.0.1.9", "product_id": pid}); code != 409 || codeOf(out) != "duplicate_serial" {
		t.Fatalf("duplicate = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/pdus", map[string]any{"name": "PDU-A", "driver": "snmp", "address": "10.0.9.1", "community": "private"}); code != 201 || out["has_community"] != true {
		t.Fatalf("create pdu = %d %v", code, out)
	}

	bad := "\uFEFFserial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet\n" +
		"SN4,A01,04,10.0.1.4,spark-9,,\n" +
		"SN5,A01,05,bad-ip,spark-1,,\n" +
		"SN1,A01,06,10.0.1.6,spark-1,,\n" +
		"SN7,A01,08,10.0.1.8,spark-1,PDU-X,1\n" +
		"SN8,A01,09,10.0.1.9,spark-1,PDU-A,x\n" +
		"SN9,A01,10,10.0.1.10,spark-1,PDU-A,3\n"
	code, out = ops.do("POST", "/machines/import", map[string]string{"csv": bad})
	if code != 400 || codeOf(out) != "import_invalid" {
		t.Fatalf("bad import = %d %v", code, out)
	}
	var lines []int
	for _, it := range out["errors"].([]any) {
		lines = append(lines, int(it.(map[string]any)["line"].(float64)))
	}
	if fmt.Sprint(lines) != "[2 3 4 5 6]" {
		t.Fatalf("error lines = %v (%v)", lines, out["errors"])
	}
	if _, out := ops.do("GET", "/machines", nil); out["total"].(float64) != 1 {
		t.Fatalf("bad import must not import anything: %v", out)
	}

	good := "serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet\n" +
		"SN2,A01,02,10.0.1.2,spark-1,PDU-A,2\n" +
		"SN3,A01,03,10.0.1.3,spark-1,,\n"
	if code, out := ops.do("POST", "/machines/import", map[string]string{"csv": good}); code != 201 || len(out["items"].([]any)) != 2 {
		t.Fatalf("good import = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/machines/import", map[string]string{"csv": "sn,rack\nSN1,A"}); code != 400 || codeOf(out) != "bad_request" {
		t.Fatalf("bad header = %d %v", code, out)
	}
}

func TestMachineListDetailMaintenanceAndPowerCycle(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	busy := e.machine("SN-BUSY")
	e.machine("SN-IDLE")
	inst := e.place(e.renter("13600000001"))
	e.ack()

	if code, out := ops.do("GET", "/machines?state=allocated", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("allocated = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/machines?online=maybe", nil); code != 400 {
		t.Fatalf("bad online filter = %d %v", code, out)
	}
	code, out := ops.do("GET", fmt.Sprintf("/machines/%d", busy), nil)
	if code != 200 || out["current_instance"].(map[string]any)["id"] != inst.ID || out["machine"].(map[string]any)["serial"] != "SN-BUSY" {
		t.Fatalf("detail = %d %v", code, out)
	}
	if code, _ := ops.do("GET", "/machines/9999", nil); code != 404 {
		t.Fatalf("missing machine = %d", code)
	}
	idle := int64(0)
	_ = e.pool.QueryRow(e.ctx, `SELECT id FROM machines WHERE serial = 'SN-IDLE'`).Scan(&idle)
	if code, out := ops.do("POST", fmt.Sprintf("/machines/%d/maintenance", idle), map[string]bool{"on": true}); code != 200 || out["state"] != "maintenance" {
		t.Fatalf("maintenance on = %d %v", code, out)
	}
	if code, out := ops.do("POST", fmt.Sprintf("/machines/%d/maintenance", idle), map[string]bool{"on": false}); code != 200 || out["state"] != "idle" {
		t.Fatalf("maintenance off = %d %v", code, out)
	}
	if code, out := ops.do("POST", fmt.Sprintf("/machines/%d/power-cycle", busy), map[string]any{}); code != 400 {
		t.Fatalf("power-cycle without pdu = %d %v", code, out)
	}
	_, pdu := ops.do("POST", "/pdus", map[string]any{"name": "PDU-A", "driver": "mock", "address": "127.0.0.1"})
	pduID := int64(pdu["id"].(float64))
	if code, out := ops.do("PUT", fmt.Sprintf("/pdus/%d", pduID), map[string]any{"name": "PDU-A1", "driver": "mock", "address": "127.0.0.1"}); code != 200 || out["name"] != "PDU-A1" {
		t.Fatalf("update pdu = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/pdus", nil); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("list pdus = %d %v", code, out)
	}
	five := 5
	m, err := e.fleet.Register(e.ctx, fleet.NewMachine{Serial: "SN-PDU", LanIP: "10.0.1.50", ProductID: dbtest.ProductID(t, e.pool), PDUID: &pduID, PDUOutlet: &five})
	if err != nil {
		t.Fatal(err)
	}
	if code, out := ops.do("POST", fmt.Sprintf("/machines/%d/power-cycle", m.ID), map[string]any{}); code != 200 {
		t.Fatalf("power-cycle = %d %v", code, out)
	}
	if calls := e.pdu.Calls(); len(calls) != 1 || calls[0] != "cycle:5" {
		t.Fatalf("pdu calls = %v", calls)
	}

	fin := e.loginAs("fin", admin.RoleFinance)
	if code, out := fin.do("GET", "/machines", nil); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance machines = %d %v", code, out)
	}
}

func TestAlertsEndpoints(t *testing.T) {
	e := newEnv(t)
	fin := e.loginAs("fin", admin.RoleFinance)
	mid := e.machine("SN1")
	if err := e.fleet.RaiseAlert(e.ctx, e.pool, fleet.Alert{Level: "critical", Kind: "wipe_failed", MachineID: mid, Message: "x"}); err != nil {
		t.Fatal(err)
	}
	code, out := fin.do("GET", "/alerts?resolved=false", nil)
	if code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("alerts = %d %v", code, out)
	}
	id := int64(out["items"].([]any)[0].(map[string]any)["id"].(float64))
	if code, _ := fin.do("POST", fmt.Sprintf("/alerts/%d/resolve", id), map[string]any{}); code != 200 {
		t.Fatalf("resolve = %d", code)
	}
	if code, out := fin.do("POST", fmt.Sprintf("/alerts/%d/resolve", id), map[string]any{}); code != 409 {
		t.Fatalf("resolve twice = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/alerts?resolved=true", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("resolved alerts = %d %v", code, out)
	}
}
