package rental_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func TestPlaceAllocatesChargesAndEnqueuesCreate(t *testing.T) {
	e := newEnv(t, 1)
	ord, inst := e.place(catalog.D1d)

	if ord.Amount != 6900 || ord.Kind != "new" || ord.Status != "paid" || ord.InstanceID != inst.ID {
		t.Fatalf("order = %+v", ord)
	}
	if inst.State != rental.StateCreating || inst.PendingSeconds != 86400 || inst.ExpiresAt != nil {
		t.Fatalf("instance = %+v", inst)
	}
	if got := e.balance(e.userID); got != startFunds-6900 {
		t.Fatalf("balance = %d", got)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateAllocated {
		t.Fatalf("machine state = %s", got)
	}
	cmds, _ := e.fleet.PendingCommands(e.ctx, e.machines[0])
	if len(cmds) != 1 || cmds[0].Type != fleet.CmdCreateInstance || cmds[0].InstanceID != inst.ID {
		t.Fatalf("commands = %+v", cmds)
	}
	var p fleet.CreateInstancePayload
	_ = json.Unmarshal(cmds[0].Payload, &p)
	if p.ImageRef != "registry.spark.internal/base/cuda:ubuntu24.04" {
		t.Fatalf("payload = %+v", p)
	}
	orders, _ := e.svc.ListOrders(e.ctx, e.userID, 10)
	if len(orders) != 1 || orders[0].ID != ord.ID {
		t.Fatalf("orders = %+v", orders)
	}
	pw, err := e.svc.Password(e.ctx, e.userID, inst.ID)
	if err != nil || len(pw) != 16 {
		t.Fatalf("password = %q, %v", pw, err)
	}
	// 占用时段：下单时刻起，至「开通超时 + 租期 + 6 小时保留 + 1 小时洗机」
	lower, upper, status := e.lease(inst.ID)
	wantUpper := dbtest.T0.Add(rental.CreateTimeout + 24*time.Hour + rental.RetainAfterStop + rental.WipeBuffer)
	if !lower.Equal(dbtest.T0) || !upper.Equal(wantUpper) || status != "active" {
		t.Fatalf("lease = [%v, %v) %s, want [%v, %v) active", lower, upper, status, dbtest.T0, wantUpper)
	}
}

func TestPlaceRequiresKYC(t *testing.T) {
	e := newEnv(t, 1)
	uid := dbtest.CreateUser(t, e.pool, "13600000099", false)
	dbtest.Fund(t, e.pool, uid, startFunds)
	_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: uid, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
	if !errors.Is(err, apperr.ErrKYCRequired) {
		t.Fatalf("want kyc required, got %v", err)
	}
}

func TestPlaceInsufficientBalanceKeepsMachineIdle(t *testing.T) {
	e := newEnv(t, 1)
	poor := e.newUser("13600000002", 100)
	_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: poor, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
	if !errors.Is(err, apperr.ErrInsufficientBalance) {
		t.Fatalf("want insufficient balance, got %v", err)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateIdle {
		t.Fatalf("machine state = %s, want idle", got)
	}
	if list, _ := e.svc.ListForUser(e.ctx, poor); len(list) != 0 {
		t.Fatalf("instances = %+v", list)
	}
	if got := e.balance(poor); got != 100 {
		t.Fatalf("balance = %d", got)
	}
}

func TestPlaceNoCapacityDoesNotCharge(t *testing.T) {
	e := newEnv(t, 1)
	e.place(catalog.D1d)
	_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
	if !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("want no capacity, got %v", err)
	}
	if got := e.balance(e.userID); got != startFunds-6900 {
		t.Fatalf("balance = %d, want charged once", got)
	}
}

func TestPlaceRejectsInvalidDurationAndImage(t *testing.T) {
	e := newEnv(t, 1)
	_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: "2h"})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("invalid duration want bad_request, got %v", err)
	}
	_, _, err = e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: 99999, Duration: catalog.D1d})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("invalid image want bad_request, got %v", err)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateIdle {
		t.Fatalf("machine state = %s, want idle", got)
	}
}

func TestConcurrentPlaceForLastMachines(t *testing.T) {
	const machines, buyers = 10, 100
	e := newEnv(t, machines)
	users := make([]int64, buyers)
	for i := range users {
		users[i] = e.newUser(fmt.Sprintf("135%08d", i), startFunds)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := map[int64]int64{} // 用户 → 分到的机器
	for _, uid := range users {
		wg.Add(1)
		go func(uid int64) {
			defer wg.Done()
			_, inst, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: uid, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won[uid] = inst.MachineID
			} else if !errors.Is(err, apperr.ErrNoCapacity) {
				t.Errorf("unexpected error: %v", err)
			}
		}(uid)
	}
	wg.Wait()
	if len(won) != machines {
		t.Fatalf("winners = %d, want %d", len(won), machines)
	}
	distinct := map[int64]bool{}
	for _, mid := range won {
		distinct[mid] = true
	}
	if len(distinct) != machines {
		t.Fatalf("winners share machines: %v", won)
	}
	for _, uid := range users {
		want := int64(startFunds)
		if _, ok := won[uid]; ok {
			want -= 6900
		}
		if got := e.balance(uid); got != want {
			t.Fatalf("user %d balance = %d, want %d", uid, got, want)
		}
	}
}

func TestInstancesAreIsolatedPerUser(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	other := e.newUser("13600000003", 0)
	if _, err := e.svc.GetForUser(e.ctx, other, inst.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("GetForUser other = %v", err)
	}
	if _, err := e.svc.Password(e.ctx, other, inst.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("Password other = %v", err)
	}
	if list, _ := e.svc.ListForUser(e.ctx, e.userID); len(list) != 1 || list[0].ID != inst.ID {
		t.Fatalf("ListForUser = %+v", list)
	}
}
