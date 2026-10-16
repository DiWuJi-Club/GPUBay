package fleet_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
)

func TestListMachinesFilters(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	dbtest.CreateMachine(t, pool, "SN-IDLE") // idle、在线、rack 为空
	if _, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN-A1", Rack: "A01", LanIP: "10.0.1.1", ProductID: pid}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN-A2", Rack: "A01", LanIP: "10.0.1.2", ProductID: pid}); err != nil {
		t.Fatal(err)
	}
	all, total, err := svc.ListMachines(ctx, fleet.MachineFilter{}, paging.Page{Number: 1, Size: 2})
	if err != nil || total != 3 || len(all) != 2 {
		t.Fatalf("all = %d/%d, %v", len(all), total, err)
	}
	on := true
	idle, total, _ := svc.ListMachines(ctx, fleet.MachineFilter{State: "idle", Online: &on}, paging.Page{})
	if total != 1 || idle[0].Serial != "SN-IDLE" {
		t.Fatalf("idle online = %+v", idle)
	}
	rack, total, _ := svc.ListMachines(ctx, fleet.MachineFilter{Rack: "A01", State: "maintenance"}, paging.Page{})
	if total != 2 || rack[0].Serial != "SN-A1" {
		t.Fatalf("rack = %+v", rack)
	}
	if _, _, err := svc.ListMachines(ctx, fleet.MachineFilter{State: "broken"}, paging.Page{}); codeOf(err) != "bad_request" {
		t.Fatalf("bad state = %v", err)
	}
	stats, err := svc.Stats(ctx)
	if err != nil || stats.Total != 3 || stats.Online != 1 || stats.ByState["maintenance"] != 2 || stats.ByState["idle"] != 1 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
}

func TestImportIsAllOrNothing(t *testing.T) {
	svc, pool, _ := newEnv(t)
	configurePDU(t, svc, true)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	p, _ := svc.CreatePDU(ctx, ops, snmpInput("PDU-A"))
	two := 2
	rows := []fleet.ImportRow{
		{Line: 2, Machine: fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: pid}},
		{Line: 3, Machine: fleet.NewMachine{Serial: "SN2", LanIP: "10.0.1.2", ProductID: pid, PDUID: &p.ID, PDUOutlet: &two}},
	}
	bad := append(rows,
		fleet.ImportRow{Line: 4, Machine: fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.3", ProductID: pid}},                                // 同批重复
		fleet.ImportRow{Line: 5, Machine: fleet.NewMachine{Serial: "SN3", LanIP: "10.0.1.4", ProductID: pid, PDUID: &p.ID, PDUOutlet: &two}}, // 插座已占用
		fleet.ImportRow{Line: 6, Machine: fleet.NewMachine{Serial: "SN4", LanIP: "bad", ProductID: pid}},
	)
	_, err := svc.Import(ctx, ops, bad, []fleet.RowError{{Line: 7, Message: "商品 spark-9 不存在"}})
	var ie *fleet.ImportError
	if !errors.As(err, &ie) || len(ie.Rows) != 4 || ie.Rows[0].Line != 4 || ie.Rows[3].Line != 7 {
		t.Fatalf("import errors = %+v (%v)", ie, err)
	}
	if _, total, _ := svc.ListMachines(ctx, fleet.MachineFilter{}, paging.Page{}); total != 0 {
		t.Fatalf("partial import: %d machines", total)
	}
	got, err := svc.Import(ctx, ops, rows, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("import = %d, %v", len(got), err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'machine.import' AND (diff->>'count')::int = 2`).Scan(&n)
	if n != 1 {
		t.Fatalf("import audit rows = %d", n)
	}
	if _, err := svc.Import(ctx, ops, nil, nil); codeOf(err) != "bad_request" {
		t.Fatalf("empty import = %v", err)
	}
}

func TestRegisterAudited(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	m, err := svc.RegisterAudited(ctx, ops, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: dbtest.ProductID(t, pool)})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'machine.register' AND target_id = $1`, strconv.FormatInt(m.ID, 10)).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
}

func TestAlertsListAndResolve(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN1")
	for _, kind := range []string{"a", "b", "c"} {
		if err := svc.RaiseAlert(ctx, pool, fleet.Alert{Level: "warning", Kind: kind, MachineID: mid, Message: kind}); err != nil {
			t.Fatal(err)
		}
	}
	open := false
	list, total, err := svc.ListAlerts(ctx, &open, paging.Page{})
	if err != nil || total != 3 || list[0].MachineID == nil || *list[0].MachineID != mid {
		t.Fatalf("open alerts = %+v, %d, %v", list, total, err)
	}
	if err := svc.ResolveAlert(ctx, ops, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResolveAlert(ctx, ops, list[0].ID); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("resolve twice = %v", err)
	}
	if err := svc.ResolveAlert(ctx, ops, 9999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("resolve missing = %v", err)
	}
	done := true
	resolved, total, _ := svc.ListAlerts(ctx, &done, paging.Page{})
	if total != 1 || resolved[0].ResolvedBy == nil || *resolved[0].ResolvedBy != ops.AdminID || resolved[0].ResolvedAt == nil {
		t.Fatalf("resolved = %+v", resolved)
	}
	if _, total, _ := svc.ListAlerts(ctx, nil, paging.Page{}); total != 3 {
		t.Fatalf("all alerts = %d", total)
	}
}
