package rental_test

import (
	"testing"

	"spark-cloud/internal/catalog"
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
