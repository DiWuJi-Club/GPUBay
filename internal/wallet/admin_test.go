package wallet_test

import (
	"context"
	"testing"
	"time"

	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/wallet"
)

func TestAdminListLedgerAndExport(t *testing.T) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	svc := wallet.NewService(pool, clk)
	ctx := context.Background()
	a := dbtest.CreateUser(t, pool, "13700000001", false)
	b := dbtest.CreateUser(t, pool, "13700000002", false)
	if _, err := svc.AdminAdjust(ctx, 7, a, 1000, "补偿", ""); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	if _, err := svc.AdminAdjust(ctx, 7, b, 2000, "补偿", ""); err != nil {
		t.Fatal(err)
	}
	list, total, err := svc.AdminListLedger(ctx, wallet.LedgerFilter{}, paging.Page{})
	if err != nil || total != 2 || list[0].UserID != b || list[0].OperatorID == nil || *list[0].OperatorID != 7 {
		t.Fatalf("all = %+v, %d, %v", list, total, err)
	}
	if _, total, _ := svc.AdminListLedger(ctx, wallet.LedgerFilter{UserID: a, Kind: "admin_adjust"}, paging.Page{}); total != 1 {
		t.Fatalf("user filter = %d", total)
	}
	if _, _, err := svc.AdminListLedger(ctx, wallet.LedgerFilter{Kind: "gift"}, paging.Page{}); err == nil {
		t.Fatal("bad kind accepted")
	}
	var users []int64
	if err := svc.ExportLedger(ctx, wallet.LedgerFilter{}, func(r wallet.AdminLedgerRow) error { users = append(users, r.UserID); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0] != a {
		t.Fatalf("export = %v", users)
	}
}
