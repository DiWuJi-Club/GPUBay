package wallet_test

import (
	"context"
	"testing"
	"time"

	"spark-cloud/internal/platform/apperr"
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
	if n := countKind(t, svc, wallet.KindLLMPay); n != 0 {
		t.Fatalf("llm_pay filter before = %d", n)
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
	// 计划 9：llm_pay 可作为筛选类型
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Apply(ctx, tx, wallet.Entry{UserID: a, Amount: -10, Kind: wallet.KindLLMPay, RefType: "llm_settlement", RefID: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countKind(t, svc, wallet.KindLLMPay); n != 1 {
		t.Fatalf("llm_pay filter = %d", n)
	}
}

func countKind(t *testing.T, svc *wallet.Service, k wallet.Kind) int {
	t.Helper()
	_, total, err := svc.AdminListLedger(context.Background(), wallet.LedgerFilter{Kind: string(k)}, paging.Page{})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func TestLedgerFilterAcceptsReferralKinds(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13800000077", true)
	if _, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: 500, Kind: wallet.KindReferralRebate, RefType: "referral", RefID: "ord-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -200, Kind: wallet.KindReferralClawback, RefType: "referral", RefID: "ord-a-c1"}); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]int{"referral_rebate": 1, "referral_clawback": 1, "order_pay": 0} {
		list, total, err := svc.AdminListLedger(ctx, wallet.LedgerFilter{UserID: uid, Kind: kind}, paging.Page{Number: 1, Size: 10})
		if err != nil || total != want || len(list) != want {
			t.Fatalf("kind %s: %d %d %v", kind, total, len(list), err)
		}
	}
	if _, _, err := svc.AdminListLedger(ctx, wallet.LedgerFilter{Kind: "bogus"}, paging.Page{Number: 1, Size: 10}); apperr.As(err) == nil {
		t.Fatalf("bogus kind must be rejected, got %v", err)
	}
}
