package fleet_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func newEnv(t *testing.T) (*fleet.Service, *pgxpool.Pool, *clock.Fake) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	return fleet.NewService(pool, clk), pool, clk
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

// MarkOfflineStale 不等别的事务锁着的机器行（SKIP LOCKED）：多行 UPDATE 不定序加锁会与锁机器行的事务死锁。
// 被跳过的机器在锁释放后的下一轮判定。
func TestMarkOfflineStaleSkipsLockedRows(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	a := dbtest.CreateMachine(t, pool, "SN-A")
	b := dbtest.CreateMachine(t, pool, "SN-B")
	clk.Advance(5*time.Minute + time.Second)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT id FROM machines WHERE id = $1 FOR UPDATE`, b); err != nil {
		t.Fatal(err)
	}
	quick, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	offline, err := svc.MarkOfflineStale(quick, 5*time.Minute)
	if err != nil || len(offline) != 1 || offline[0] != a {
		t.Fatalf("with B locked: offline = %v, %v; want [%d] without waiting", offline, err, a)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if offline, err = svc.MarkOfflineStale(ctx, 5*time.Minute); err != nil || len(offline) != 1 || offline[0] != b {
		t.Fatalf("after unlock: offline = %v, %v; want [%d]", offline, err, b)
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

func TestRegisterUnknownProductIsBadRequest(t *testing.T) {
	svc, _, _ := newEnv(t)
	_, err := svc.Register(context.Background(), fleet.NewMachine{Serial: "SN009", LanIP: "10.0.1.9", ProductID: 9999})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" || ae.Message != "商品不存在" {
		t.Fatalf("want bad_request 商品不存在, got %v", err)
	}
}
