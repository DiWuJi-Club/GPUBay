package rental_test

import (
	"testing"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/rental"
)

func TestBookingColumnsDefaults(t *testing.T) {
	e := newEnv(t, 1)
	ord, inst := e.place(catalog.D1d)
	got := e.get(inst.ID)
	if got.Booked() || got.ScheduledStartAt != nil || got.GroupID != nil || got.BatchID != nil || got.AutoRenew ||
		!got.AutoRenewDuration.Valid() || got.ReassignCount != 0 || got.ProvisionedAt != nil || got.CancelledAt != nil {
		t.Fatalf("defaults = %+v", got)
	}
	// 列默认值（Task 4 起 Place 会显式写入首单档位，所以默认值用一行不带该列的裸插入来验）
	var def string
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ('ins-defaults', $1, $2, $3, $4, 'released', $5, $6, $6) RETURNING auto_renew_duration`,
		e.userID, e.machines[0], e.productID, e.imageID, []byte("x"), e.clock.Now()).Scan(&def); err != nil || def != "1m" {
		t.Fatalf("auto_renew_duration default = %q, %v", def, err)
	}
	if ord.Quantity != 1 || ord.BatchID != nil || ord.Auto {
		t.Fatalf("order defaults = %+v", ord)
	}
	// 新状态能写入；旧状态照常
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'reserved' WHERE id = $1`, inst.ID); err != nil {
		t.Fatalf("reserved rejected by CHECK: %v", err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'cancelled' WHERE id = $1`, inst.ID); err != nil {
		t.Fatalf("cancelled rejected by CHECK: %v", err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'bogus' WHERE id = $1`, inst.ID); err == nil {
		t.Fatal("bogus state accepted")
	}
	var tables int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('machine_groups', 'rental_batches')`).Scan(&tables); err != nil || tables != 2 {
		t.Fatalf("new tables = %d, %v", tables, err)
	}
	// 空时段被 CHECK 拒绝
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange(now(), now()), 'ended', now(), now())`, e.machines[0], inst.ID); err == nil {
		t.Fatal("empty period accepted")
	}
	// display_name 非空时唯一
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET display_name = 'A-01' WHERE id = $1`, e.machines[0]); err != nil {
		t.Fatal(err)
	}
	m2 := e.newMachine("SN-DUP")
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET display_name = 'A-01' WHERE id = $1`, m2); err == nil {
		t.Fatal("duplicate display_name accepted")
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET display_name = '' WHERE id = $1`, m2); err != nil {
		t.Fatalf("empty display_name must not collide: %v", err)
	}
}

func TestMaxQuantity(t *testing.T) {
	cases := map[catalog.Duration]int{catalog.D1d: 90, catalog.D1w: 12, catalog.D1m: 3, catalog.Duration("x"): 0}
	for d, want := range cases {
		if got := rental.MaxQuantity(d); got != want {
			t.Fatalf("MaxQuantity(%s) = %d, want %d", d, got, want)
		}
	}
}
