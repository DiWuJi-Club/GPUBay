package rental_test

import (
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/rental"
)

func TestCreateSuccessStartsLeaseAtRunningTime(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.clock.Advance(2 * time.Minute) // 开机耗时不计入租期
	e.ack(true)

	got := e.get(inst.ID)
	want := e.clock.Now().Add(24 * time.Hour)
	if got.State != rental.StateRunning || got.PendingSeconds != 0 || got.ExpiresAt == nil || !got.ExpiresAt.Equal(want) {
		t.Fatalf("instance = %+v, want running until %v", got, want)
	}
	if got.Remaining(e.clock.Now()) != 24*time.Hour {
		t.Fatalf("remaining = %v", got.Remaining(e.clock.Now()))
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(want.Add(rental.RetainAfterStop + rental.WipeBuffer)) {
		t.Fatalf("lease upper = %v, want %v", upper, want.Add(rental.RetainAfterStop+rental.WipeBuffer))
	}
}

func TestDuplicateAckIsIgnored(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	cmds := e.ack(true)
	if err := e.svc.HandleCommandResult(e.ctx, cmds[0].ID, false, "late failure"); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.State != rental.StateRunning {
		t.Fatalf("state = %s, want running", got.State)
	}
	if got := e.balance(e.userID); got != startFunds-6900 {
		t.Fatalf("balance = %d, no refund expected", got)
	}
}

func TestCreateFailureRefundsAndQuarantinesMachine(t *testing.T) {
	e := newEnv(t, 1)
	ord, inst := e.place(catalog.D1d)
	e.clock.Advance(time.Minute) // 时段需非空，否则 lower/upper 为 NULL
	e.ack(false)

	if got := e.get(inst.ID); got.State != rental.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	if got := e.balance(e.userID); got != startFunds {
		t.Fatalf("balance = %d, want full refund", got)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateFaulty {
		t.Fatalf("machine = %s, want faulty", got)
	}
	if _, _, status := e.lease(inst.ID); status != "ended" {
		t.Fatalf("lease status = %s, want ended", status)
	}
	orders, _ := e.svc.ListOrders(e.ctx, e.userID, 10)
	if len(orders) != 1 || orders[0].ID != ord.ID || orders[0].Status != "refunded" {
		t.Fatalf("orders = %+v", orders)
	}
	var alerts int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = 'create_failed'`).Scan(&alerts)
	if alerts != 1 {
		t.Fatalf("alerts = %d", alerts)
	}
}

func TestUnknownCommandID(t *testing.T) {
	e := newEnv(t, 1)
	if err := e.svc.HandleCommandResult(e.ctx, "00000000-0000-0000-0000-000000000000", true, ""); err == nil {
		t.Fatal("unknown command must return error")
	}
}

// 并发：Agent 回报启动成功 与 用户释放 同时发生，不能死锁，最终必须处于洗机。
func TestConcurrentAckAndReleaseDoNotDeadlock(t *testing.T) {
	const n = 20
	e := newEnv(t, n)
	var ids []string
	for i := 0; i < n; i++ {
		_, inst := e.place(catalog.D1d)
		ids = append(ids, inst.ID)
	}
	e.ack(true)
	expireAndStop(e)
	for _, id := range ids {
		if _, err := e.svc.Renew(e.ctx, e.userID, id, catalog.D1d, ""); err != nil {
			t.Fatal(err)
		}
	}
	starts := map[string]string{}
	for _, mid := range e.machines {
		cmds, _ := e.fleet.PendingCommands(e.ctx, mid)
		for _, c := range cmds {
			if c.Type == fleet.CmdStartInstance {
				starts[c.InstanceID] = c.ID
			}
		}
	}
	if len(starts) != n {
		t.Fatalf("start commands = %d", len(starts))
	}
	for _, id := range ids {
		var wg sync.WaitGroup
		gate := make(chan struct{})
		var ackErr, relErr error
		wg.Add(2)
		go func() { defer wg.Done(); <-gate; ackErr = e.svc.HandleCommandResult(e.ctx, starts[id], true, "ok") }()
		go func() { defer wg.Done(); <-gate; relErr = e.svc.Release(e.ctx, e.userID, id) }()
		close(gate)
		wg.Wait()
		if ackErr != nil || relErr != nil {
			t.Fatalf("instance %s: ack err = %v, release err = %v", id, ackErr, relErr)
		}
		if got := e.get(id); got.State != rental.StateWiping {
			t.Fatalf("instance %s state = %s, want wiping", id, got.State)
		}
	}
}
