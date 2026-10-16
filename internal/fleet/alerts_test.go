package fleet_test

import (
	"context"
	"testing"

	"spark-cloud/internal/fleet"
)

func TestRaiseAlertOnce(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	a := fleet.Alert{Level: "critical", Kind: "recharge_amount_mismatch", Message: "充值单 rch-1 金额不符"}
	for i := 0; i < 3; i++ {
		if err := svc.RaiseAlertOnce(ctx, pool, a); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'recharge_amount_mismatch'`).Scan(&n)
	if n != 1 {
		t.Fatalf("alerts = %d, want 1", n)
	}
	if _, err := pool.Exec(ctx, `UPDATE alerts SET resolved_at = created_at`); err != nil {
		t.Fatal(err)
	}
	if err := svc.RaiseAlertOnce(ctx, pool, a); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'recharge_amount_mismatch'`).Scan(&n)
	if n != 2 {
		t.Fatalf("after resolve alerts = %d, want 2", n)
	}
}
