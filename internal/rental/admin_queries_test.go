package rental_test

import (
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/rental"
)

func TestAdminListInstancesFilters(t *testing.T) {
	e := newEnv(t, 3)
	_, a := e.place(catalog.D1d)
	_, b := e.place(catalog.D1d)
	other := e.newUser("13600000099", startFunds)
	if _, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: other, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d}); err != nil {
		t.Fatal(err)
	}
	e.ack(true)
	if err := e.svc.Release(e.ctx, e.userID, a.ID); err != nil {
		t.Fatal(err)
	}
	all, total, err := e.svc.AdminListInstances(e.ctx, rental.InstanceFilter{}, paging.Page{})
	if err != nil || total != 3 || len(all) != 3 {
		t.Fatalf("all = %d, %v", total, err)
	}
	mine, total, _ := e.svc.AdminListInstances(e.ctx, rental.InstanceFilter{UserID: e.userID, State: rental.StateRunning}, paging.Page{})
	if total != 1 || mine[0].ID != b.ID {
		t.Fatalf("mine running = %+v", mine)
	}
	onMachine, total, _ := e.svc.AdminListInstances(e.ctx, rental.InstanceFilter{MachineID: a.MachineID}, paging.Page{})
	if total != 1 || onMachine[0].ID != a.ID {
		t.Fatalf("by machine = %+v", onMachine)
	}
	if _, _, err := e.svc.AdminListInstances(e.ctx, rental.InstanceFilter{State: "zombie"}, paging.Page{}); codeOf(err) != "bad_request" {
		t.Fatalf("bad state = %v", err)
	}
}

func TestInstanceEventsAndOrdersFollowMigration(t *testing.T) {
	e := newEnv(t, 2)
	ord, old := e.place(catalog.D1d)
	e.ack(true)
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "换机")
	if err != nil {
		t.Fatal(err)
	}
	events, err := e.svc.InstanceEvents(e.ctx, old.ID)
	if err != nil || len(events) != 3 || events[0].ToState != rental.StateCreating || events[2].ToState != rental.StateWiping {
		t.Fatalf("events = %+v, %v", events, err)
	}
	orders, err := e.svc.InstanceOrders(e.ctx, moved.ID)
	if err != nil || len(orders) != 1 || orders[0].ID != ord.ID {
		t.Fatalf("orders of migrated instance = %+v, %v", orders, err)
	}
	cmds, err := e.fleet.CommandsForInstance(e.ctx, old.ID, 10)
	if err != nil || len(cmds) != 2 || cmds[0].Type != "wipe_instance" {
		t.Fatalf("commands = %+v, %v", cmds, err)
	}
}

func TestAdminListOrdersAndExport(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(48 * time.Hour)
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w, ""); err != nil {
		t.Fatal(err)
	}
	list, total, err := e.svc.AdminListOrders(e.ctx, rental.OrderFilter{}, paging.Page{})
	if err != nil || total != 2 || list[0].Kind != "renew" || list[0].UserID != e.userID {
		t.Fatalf("all = %+v, %d, %v", list, total, err)
	}
	from := e.clock.Now().Add(-time.Hour)
	if _, total, _ := e.svc.AdminListOrders(e.ctx, rental.OrderFilter{From: &from}, paging.Page{}); total != 1 {
		t.Fatalf("from filter = %d", total)
	}
	if _, total, _ := e.svc.AdminListOrders(e.ctx, rental.OrderFilter{To: &from, Kind: "new", Status: "paid"}, paging.Page{}); total != 1 {
		t.Fatalf("to+kind+status filter = %d", total)
	}
	if _, _, err := e.svc.AdminListOrders(e.ctx, rental.OrderFilter{Kind: "gift"}, paging.Page{}); codeOf(err) != "bad_request" {
		t.Fatalf("bad kind = %v", err)
	}
	var kinds []string
	if err := e.svc.ExportOrders(e.ctx, rental.OrderFilter{}, func(o rental.Order) error { kinds = append(kinds, o.Kind); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 2 || kinds[0] != "new" {
		t.Fatalf("export order = %v (must be oldest first)", kinds)
	}
}

func TestExportOrdersTooLarge(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO orders (id, user_id, kind, instance_id, product_id, duration, amount, status, created_at, updated_at)
		SELECT 'ord-bulk-' || g, $1, 'renew', $2, $3, '1d', 1, 'paid', $4, $4 FROM generate_series(1, $5::int) g`,
		e.userID, inst.ID, e.productID, dbtest.T0, csvx.MaxRows); err != nil {
		t.Fatal(err)
	}
	called := false
	err := e.svc.ExportOrders(e.ctx, rental.OrderFilter{}, func(rental.Order) error { called = true; return nil })
	if !errors.Is(err, csvx.ErrTooLarge) || called {
		t.Fatalf("export = %v, called = %v", err, called)
	}
}

func TestDashboardStats(t *testing.T) {
	e := newEnv(t, 2)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // 计时从 T0 开始，到期 T0+24h，占用时段上界为 T0+31h（含 6 小时保留与 1 小时洗机缓冲）
	e.place(catalog.D1w)
	counts, err := e.svc.InstanceStateCounts(e.ctx)
	if err != nil || counts[rental.StateRunning] != 1 || counts[rental.StateCreating] != 1 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
	amount, err := e.svc.OrderAmount(e.ctx, dbtest.T0, dbtest.T0.Add(time.Hour))
	if err != nil || amount != 6900+41400 {
		t.Fatalf("order amount = %d, %v", amount, err)
	}
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	e.ack(true)
	now := dbtest.T0.Add(36 * time.Hour)
	occ, err := e.svc.DailyOccupancy(e.ctx, dbtest.T0, 2, now)
	if err != nil || len(occ) != 2 {
		t.Fatalf("occupancy = %v, %v", occ, err)
	}
	// 第一个实例在 T0 释放（时段为空）；第二个实例（1 周）在 e.ack 后开始运行，两天都被占用，第二天截止到 now
	if occ[0] != 86400 || occ[1] != 12*3600 {
		t.Fatalf("occupancy = %v, want [86400 43200]", occ)
	}
}
