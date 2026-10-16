package wallet_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/wallet"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func newEnv(t *testing.T) (*wallet.Service, *pgxpool.Pool) {
	pool := dbtest.New(t)
	return wallet.NewService(pool, clock.NewFake(dbtest.T0)), pool
}

func apply(ctx context.Context, pool *pgxpool.Pool, svc *wallet.Service, e wallet.Entry) (int64, error) {
	var after int64
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		after, err = svc.Apply(ctx, tx, e)
		return err
	})
	return after, err
}

func TestCreditDebitAndLedger(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000001", true)

	if after, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: 10000, Kind: wallet.KindRecharge, RefType: "recharge", RefID: "r1"}); err != nil || after != 10000 {
		t.Fatalf("credit: %d, %v", after, err)
	}
	if after, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -3000, Kind: wallet.KindOrderPay, RefType: "order", RefID: "o1"}); err != nil || after != 7000 {
		t.Fatalf("debit: %d, %v", after, err)
	}
	bal, _ := svc.Balance(ctx, uid)
	if bal != 7000 {
		t.Fatalf("balance = %d", bal)
	}
	rows, err := svc.ListLedger(ctx, uid, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Amount != -3000 || rows[0].BalanceAfter != 7000 || rows[1].Amount != 10000 {
		t.Fatalf("ledger = %+v", rows)
	}
	older, _ := svc.ListLedger(ctx, uid, rows[0].ID, 10)
	if len(older) != 1 || older[0].ID != rows[1].ID {
		t.Fatalf("paging = %+v", older)
	}
}

func TestBalanceOfNewUserIsZero(t *testing.T) {
	svc, pool := newEnv(t)
	uid := dbtest.CreateUser(t, pool, "13700000002", true)
	if bal, err := svc.Balance(context.Background(), uid); err != nil || bal != 0 {
		t.Fatalf("balance = %d, %v", bal, err)
	}
}

func TestInsufficientBalanceLeavesNoTrace(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000003", true)
	dbtest.Fund(t, pool, uid, 500)

	_, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -501, Kind: wallet.KindOrderPay, RefType: "order", RefID: "o1"})
	if !errors.Is(err, apperr.ErrInsufficientBalance) {
		t.Fatalf("want insufficient balance, got %v", err)
	}
	bal, _ := svc.Balance(ctx, uid)
	rows, _ := svc.ListLedger(ctx, uid, 0, 10)
	if bal != 500 || len(rows) != 1 {
		t.Fatalf("balance = %d, ledger rows = %d", bal, len(rows))
	}
}

func TestDuplicateRefRejected(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000004", true)
	e := wallet.Entry{UserID: uid, Amount: 100, Kind: wallet.KindRefund, RefType: "order", RefID: "o1"}
	if _, err := apply(ctx, pool, svc, e); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(ctx, pool, svc, e); !errors.Is(err, apperr.ErrDuplicate) {
		t.Fatalf("want duplicate, got %v", err)
	}
	if bal, _ := svc.Balance(ctx, uid); bal != 100 {
		t.Fatalf("balance = %d, want 100", bal)
	}
}

func TestConcurrentDebitsNeverOverdraw(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000005", true)
	dbtest.Fund(t, pool, uid, 1000)

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -100, Kind: wallet.KindOrderPay, RefType: "order", RefID: string(rune('a' + i))})
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !errors.Is(err, apperr.ErrInsufficientBalance) {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	bal, _ := svc.Balance(ctx, uid)
	if ok != 10 || bal != 0 {
		t.Fatalf("succeeded = %d, balance = %d; want 10, 0", ok, bal)
	}
}

func TestAdminAdjustRequiresRemarkAndAudits(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000006", true)

	if _, err := svc.AdminAdjust(ctx, 7, uid, 5000, " ", "10.0.0.1"); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
	after, err := svc.AdminAdjust(ctx, 7, uid, 5000, "对公转账 #123", "10.0.0.1")
	if err != nil || after != 5000 {
		t.Fatalf("adjust = %d, %v", after, err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'wallet.adjust' AND actor_id = 7`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
}

func TestNonAdminEntriesRequireRef(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000007", true)
	dbtest.Fund(t, pool, uid, 1000)

	for _, kind := range []wallet.Kind{wallet.KindOrderPay, wallet.KindRefund, wallet.KindRecharge} {
		_, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -100, Kind: kind})
		if e := apperr.As(err); e == nil || e.Code != "bad_request" {
			t.Fatalf("kind %s with empty ref: want bad_request, got %v", kind, err)
		}
	}
	if bal, _ := svc.Balance(ctx, uid); bal != 1000 {
		t.Fatalf("balance = %d, want 1000", bal)
	}
}

func TestAdminAdjustLimitsAndUnknownUser(t *testing.T) {
	pool := dbtest.New(t)
	svc := wallet.NewService(pool, clock.NewFake(dbtest.T0))
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000001", false)
	for _, amount := range []int64{0, wallet.MaxAdjust + 1, -wallet.MaxAdjust - 1} {
		if _, err := svc.AdminAdjust(ctx, 1, uid, amount, "x", ""); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
			t.Fatalf("amount %d = %v", amount, err)
		}
	}
	if _, err := svc.AdminAdjust(ctx, 1, 9999, 100, "x", ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown user = %v", err)
	}
}

func TestAdminAdjustRejectsIdenticalRepeat(t *testing.T) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	svc := wallet.NewService(pool, clk)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000009", true)

	if _, err := svc.AdminAdjust(ctx, 7, uid, 5000, "补偿", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdminAdjust(ctx, 7, uid, 5000, " 补偿 ", ""); apperr.As(err) == nil || apperr.As(err).Code != "duplicate" {
		t.Fatalf("identical repeat = %v, want duplicate", err)
	}
	if _, err := svc.AdminAdjust(ctx, 7, uid, 3000, "补偿", ""); err != nil {
		t.Fatalf("different amount = %v", err)
	}
	clk.Advance(61 * time.Second)
	if _, err := svc.AdminAdjust(ctx, 7, uid, 5000, "补偿", ""); err != nil {
		t.Fatalf("repeat after 61s = %v", err)
	}
	if bal, _ := svc.Balance(ctx, uid); bal != 13000 {
		t.Fatalf("balance = %d, want 13000", bal)
	}
}
