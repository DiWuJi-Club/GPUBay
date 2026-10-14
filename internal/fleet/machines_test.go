package fleet_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func newEnv(t *testing.T) (*fleet.Service, *pgxpool.Pool, *clock.Fake) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	return fleet.NewService(pool, clk), pool, clk
}

func allocate(ctx context.Context, pool *pgxpool.Pool, svc *fleet.Service, productID int64) (fleet.Machine, error) {
	var m fleet.Machine
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		m, err = svc.Allocate(ctx, tx, productID)
		return err
	})
	return m, err
}

func TestRegisterValidatesAndRejectsDuplicates(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)

	m, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN001", Rack: "A01", Slot: "03", LanIP: "10.0.1.3", ProductID: pid})
	if err != nil {
		t.Fatal(err)
	}
	if m.State != fleet.StateMaintenance || m.AgentOnline {
		t.Fatalf("new machine = %+v", m)
	}
	_, err = svc.Register(ctx, fleet.NewMachine{Serial: "SN001", LanIP: "10.0.1.4", ProductID: pid})
	if ae := apperr.As(err); ae == nil || ae.Code != "duplicate_serial" {
		t.Fatalf("want duplicate_serial, got %v", err)
	}
	_, err = svc.Register(ctx, fleet.NewMachine{Serial: "SN002", LanIP: "not-an-ip", ProductID: pid})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
}

func TestMaintenanceTransitions(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	m, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN001", LanIP: "10.0.1.3", ProductID: dbtest.ProductID(t, pool)})

	if err := svc.SetMaintenance(ctx, m.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, pool, m.ID); got.State != fleet.StateIdle {
		t.Fatalf("state = %s, want idle", got.State)
	}
	if err := svc.SetMaintenance(ctx, m.ID, false); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("idle->idle want invalid state, got %v", err)
	}
	if err := svc.SetMaintenance(ctx, m.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMaintenance(ctx, 9999, true); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing machine want not found, got %v", err)
	}
}

func TestAllocateRequiresIdleAndOnline(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	m, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN001", LanIP: "10.0.1.3", ProductID: pid})
	_ = svc.SetMaintenance(ctx, m.ID, false)

	if _, err := allocate(ctx, pool, svc, pid); !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("offline machine must not be allocated, got %v", err)
	}
	if _, err := svc.Heartbeat(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.IdleCount(ctx, pid); n != 1 {
		t.Fatalf("IdleCount = %d, want 1", n)
	}
	got, err := allocate(ctx, pool, svc, pid)
	if err != nil || got.ID != m.ID || got.State != fleet.StateAllocated {
		t.Fatalf("allocate = %+v, %v", got, err)
	}
	if _, err := allocate(ctx, pool, svc, pid); !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("second allocate want no capacity, got %v", err)
	}
	if err := svc.Release(ctx, pool, m.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.IdleCount(ctx, pid); n != 1 {
		t.Fatalf("after release IdleCount = %d, want 1", n)
	}
}

func TestConcurrentAllocationNeverDoubleAssigns(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	for i := 0; i < 10; i++ {
		dbtest.CreateMachine(t, pool, fmt.Sprintf("SN%03d", i))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[int64]int{}
	noCap := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := allocate(ctx, pool, svc, pid)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				got[m.ID]++
			case errors.Is(err, apperr.ErrNoCapacity):
				noCap++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(got) != 10 || noCap != 90 {
		t.Fatalf("distinct machines = %d, no capacity = %d", len(got), noCap)
	}
	for id, n := range got {
		if n != 1 {
			t.Fatalf("machine %d allocated %d times", id, n)
		}
	}
}

func TestHeartbeatAndOfflineDetection(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN001")

	if came, err := svc.Heartbeat(ctx, id); err != nil || came {
		t.Fatalf("heartbeat on online machine: came=%v err=%v", came, err)
	}
	clk.Advance(5*time.Minute + time.Second)
	offline, err := svc.MarkOfflineStale(ctx, 5*time.Minute)
	if err != nil || len(offline) != 1 || offline[0] != id {
		t.Fatalf("offline = %v, %v", offline, err)
	}
	if again, _ := svc.MarkOfflineStale(ctx, 5*time.Minute); len(again) != 0 {
		t.Fatalf("already offline machines reported again: %v", again)
	}
	if came, err := svc.Heartbeat(ctx, id); err != nil || !came {
		t.Fatalf("heartbeat after offline: came=%v err=%v", came, err)
	}
	if _, err := svc.Heartbeat(ctx, 9999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown machine want not found, got %v", err)
	}
}

func TestMarkFaultyAndAlert(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN001")
	if err := svc.MarkFaulty(ctx, pool, id); err != nil {
		t.Fatal(err)
	}
	if m, _ := svc.Get(ctx, pool, id); m.State != fleet.StateFaulty {
		t.Fatalf("state = %s", m.State)
	}
	if err := svc.RaiseAlert(ctx, pool, fleet.Alert{Level: "critical", Kind: "wipe_failed", MachineID: id, Message: "disk error"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RaiseAlert(ctx, pool, fleet.Alert{Level: "warning", Kind: "system", Message: "no machine"}); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alerts`).Scan(&n)
	if n != 2 {
		t.Fatalf("alerts = %d", n)
	}
}

func TestOfflineMachineIDsAndOnlineAmong(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	a := dbtest.CreateMachine(t, pool, "SN001")
	b := dbtest.CreateMachine(t, pool, "SN002")

	if got, err := svc.OnlineAmong(ctx, nil); err != nil || got != nil {
		t.Fatalf("empty input = %v, %v", got, err)
	}
	if got, _ := svc.OfflineMachineIDs(ctx); len(got) != 0 {
		t.Fatalf("offline = %v", got)
	}
	clk.Advance(6 * time.Minute)
	if _, err := svc.Heartbeat(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkOfflineStale(ctx, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.OfflineMachineIDs(ctx); err != nil || len(got) != 1 || got[0] != a {
		t.Fatalf("offline = %v, %v; want [%d]", got, err, a)
	}
	if got, err := svc.OnlineAmong(ctx, []int64{a, b}); err != nil || len(got) != 1 || got[0] != b {
		t.Fatalf("online among = %v, %v; want [%d]", got, err, b)
	}
}
