package rental_test

import (
	"errors"
	"sync"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

func str(s string) *string { return &s }

func TestPortCRUDAndLimits(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	p, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8888, "", "JupyterLab", "1.1.1.1")
	if err != nil || p.Port != 8888 || p.Visibility != "private" || p.ForcedPrivate || p.Note != "JupyterLab" {
		t.Fatalf("add = %+v %v", p, err)
	}
	for _, bad := range []int{0, 22, 65536, -1} {
		if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, bad, "private", "", ""); !errors.Is(err, apperr.ErrPortInvalid) {
			t.Fatalf("port %d = %v", bad, err)
		}
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8888, "public", "", ""); !errors.Is(err, apperr.ErrPortExists) {
		t.Fatalf("dup = %v", err)
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 80, "weird", "", ""); err == nil {
		t.Fatal("bad visibility accepted")
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 81, "private", string(make([]rune, 33)), ""); err == nil {
		t.Fatal("33-char note accepted")
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID+1, inst.ID, 82, "private", "", ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user = %v", err)
	}
	for port := 8001; port <= 8004; port++ {
		if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, port, "private", "", ""); err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8005, "private", "", ""); !errors.Is(err, apperr.ErrPortLimit) {
		t.Fatalf("6th = %v", err)
	}
	list, _ := e.svc.ListPorts(e.ctx, inst.ID)
	if len(list) != rental.MaxPortsPerInstance || list[0].Port != 8001 || list[4].Port != 8888 {
		t.Fatalf("list = %+v", list)
	}
	up, err := e.svc.UpdatePort(e.ctx, e.userID, inst.ID, 8888, str("public"), nil, "")
	if err != nil || up.Visibility != "public" || up.Note != "JupyterLab" {
		t.Fatalf("update = %+v %v", up, err)
	}
	if _, err := e.svc.UpdatePort(e.ctx, e.userID, inst.ID, 9999, str("public"), nil, ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("update missing = %v", err)
	}
	if err := e.svc.DeletePort(e.ctx, e.userID, inst.ID, 8001, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePort(e.ctx, e.userID, inst.ID, 8001, ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("delete twice = %v", err)
	}
	var audits int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE actor_type = 'user' AND action IN ('instance.port_add', 'instance.port_update', 'instance.port_delete')`).Scan(&audits)
	if audits != 7 {
		t.Fatalf("audit = %d", audits)
	}
}

func TestPortForcedPrivateAndAdminDelete(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8888, "public", "", ""); err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{AdminID: 3, IP: "10.0.0.1"}
	if _, err := e.svc.AdminForcePortPrivate(e.ctx, actor, inst.ID, 8888, ""); err == nil {
		t.Fatal("empty reason accepted")
	}
	p, err := e.svc.AdminForcePortPrivate(e.ctx, actor, inst.ID, 8888, "钓鱼页面")
	if err != nil || p.Visibility != "private" || !p.ForcedPrivate {
		t.Fatalf("force = %+v %v", p, err)
	}
	if _, err := e.svc.UpdatePort(e.ctx, e.userID, inst.ID, 8888, str("public"), nil, ""); !errors.Is(err, apperr.ErrPortForcedPrivate) {
		t.Fatalf("user re-public = %v", err)
	}
	if _, err := e.svc.UpdatePort(e.ctx, e.userID, inst.ID, 8888, nil, str("still mine"), ""); err != nil { // 改备注仍允许
		t.Fatal(err)
	}
	if err := e.svc.AdminDeletePort(e.ctx, actor, inst.ID, 8888, "abuse"); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.svc.ListPorts(e.ctx, inst.ID); len(list) != 0 {
		t.Fatalf("list = %+v", list)
	}
	var kinds []string
	rows, _ := e.pool.Query(e.ctx, `SELECT action FROM audit_logs WHERE actor_type = 'admin' AND actor_id = 3 ORDER BY id`)
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		kinds = append(kinds, a)
	}
	rows.Close()
	if len(kinds) != 2 || kinds[0] != "instance.port_force_private" || kinds[1] != "instance.port_delete" {
		t.Fatalf("audit = %v", kinds)
	}
}

func TestIngressRoutesAndMigrateCopy(t *testing.T) {
	e := newEnv(t, 2)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8888, "public", "lab", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 7860, "private", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminForcePortPrivate(e.ctx, audit.Actor{AdminID: 1}, inst.ID, 8888, "x"); err != nil {
		t.Fatal(err)
	}
	routes, err := e.svc.ListIngressRoutes(e.ctx)
	if err != nil || len(routes) != 2 {
		t.Fatalf("routes = %+v %v", routes, err)
	}
	r := routes[0]
	if r.InstanceID != inst.ID || r.UserID != e.userID || r.Port != 7860 || r.Visibility != "private" || r.State != rental.StateRunning || r.Suspended ||
		r.LanIP != "10.0.0.10" || r.HostPubkey == "" {
		t.Fatalf("route = %+v", r)
	}
	moved, err := e.svc.AdminMigrate(e.ctx, audit.Actor{AdminID: 1}, inst.ID, "move")
	if err != nil {
		t.Fatal(err)
	}
	copied, _ := e.svc.ListPorts(e.ctx, moved.ID)
	if len(copied) != 2 || copied[1].Port != 8888 || !copied[1].ForcedPrivate || copied[0].Note != "" || copied[1].Note != "lab" {
		t.Fatalf("copied = %+v", copied)
	}
	e.ack(true) // 旧实例 wipe → released，新实例 create → running
	routes, _ = e.svc.ListIngressRoutes(e.ctx)
	for _, r := range routes {
		if r.InstanceID == inst.ID {
			t.Fatalf("released instance still routed: %+v", r)
		}
	}
	if len(routes) != 2 || routes[0].InstanceID != moved.ID {
		t.Fatalf("routes after migrate = %+v", routes)
	}
}

func TestPortNoteControlChars(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8080, "private", "a\x00b", ""); err == nil {
		t.Fatal("NUL in note accepted")
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8080, "private", "ok", ""); err != nil {
		t.Fatal(err)
	}
	bad := "x\x07"
	if _, err := e.svc.UpdatePort(e.ctx, e.userID, inst.ID, 8080, nil, &bad, ""); err == nil {
		t.Fatal("control char in update accepted")
	}
}

func TestPortOwnerAndState(t *testing.T) {
	e := newEnv(t, 2)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8888, "private", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdatePort(e.ctx, e.userID+1, inst.ID, 8888, str("public"), nil, ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("update other user = %v", err)
	}
	if err := e.svc.DeletePort(e.ctx, e.userID+1, inst.ID, 8888, ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("delete other user = %v", err)
	}
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	e.ack(true) // wipe -> released
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 9000, "private", "", ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("add on released = %v", err)
	}
}

func TestPortLimitConcurrent(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	for port := 8001; port <= 8004; port++ {
		if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, port, "private", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.svc.AddPort(e.ctx, e.userID, inst.ID, 9001+i, "private", "", "")
		}()
	}
	wg.Wait()
	ok, limit := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, apperr.ErrPortLimit):
			limit++
		default:
			t.Fatalf("unexpected err %v", err)
		}
	}
	if list, _ := e.svc.ListPorts(e.ctx, inst.ID); ok != 1 || limit != 2 || len(list) != rental.MaxPortsPerInstance {
		t.Fatalf("ok=%d limit=%d count=%d", ok, limit, len(list))
	}
}
