package rental_test

import (
	"strings"
	"sync"
	"testing"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

func (e *env) placeKey(key string) (rental.Order, rental.Instance, error) {
	return e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID,
		Duration: catalog.D1d, IdempotencyKey: key})
}

func TestPlaceSameIdempotencyKeyChargesOnce(t *testing.T) {
	e := newEnv(t, 2)
	ord1, inst1, err := e.placeKey("k-1")
	if err != nil {
		t.Fatal(err)
	}
	ord2, inst2, err := e.placeKey("k-1")
	if err != nil {
		t.Fatal(err)
	}
	if ord1.ID != ord2.ID || inst1.ID != inst2.ID {
		t.Fatalf("retry returned order %s/%s instance %s/%s", ord1.ID, ord2.ID, inst1.ID, inst2.ID)
	}
	if b := e.balance(e.userID); b != startFunds-6900 {
		t.Fatalf("balance = %d, want charged once", b)
	}
	if list, _ := e.svc.ListForUser(e.ctx, e.userID); len(list) != 1 {
		t.Fatalf("instances = %d, want 1", len(list))
	}
	// 不同的键是新订单
	if ord3, _, err := e.placeKey("k-2"); err != nil || ord3.ID == ord1.ID {
		t.Fatalf("different key = %+v, %v", ord3, err)
	}
}

func TestConcurrentPlaceSameIdempotencyKey(t *testing.T) {
	e := newEnv(t, 10)
	var wg sync.WaitGroup
	var mu sync.Mutex
	orders, insts := map[string]bool{}, map[string]bool{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ord, inst, err := e.placeKey("same-key")
			if err != nil {
				t.Errorf("Place: %v", err)
				return
			}
			mu.Lock()
			orders[ord.ID], insts[inst.ID] = true, true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(orders) != 1 || len(insts) != 1 {
		t.Fatalf("orders = %v, instances = %v; want exactly one", orders, insts)
	}
	if b := e.balance(e.userID); b != startFunds-6900 {
		t.Fatalf("balance = %d, want charged once", b)
	}
}

func TestRenewIdempotencyKey(t *testing.T) {
	e := newEnv(t, 2)
	_, a := e.place(catalog.D1d)
	_, b := e.place(catalog.D1d)
	e.ack(true)
	r1, err := e.svc.Renew(e.ctx, e.userID, a.ID, catalog.D1d, "renew-1")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.svc.Renew(e.ctx, e.userID, a.ID, catalog.D1d, "renew-1")
	if err != nil || r2.ID != r1.ID {
		t.Fatalf("retry = %+v, %v; want order %s", r2, err, r1.ID)
	}
	if bal := e.balance(e.userID); bal != startFunds-3*6900 {
		t.Fatalf("balance = %d, want renewal charged once", bal)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, b.ID, catalog.D1d, "renew-1"); apperr.As(err) == nil ||
		apperr.As(err).Code != "idempotency_conflict" || apperr.As(err).Status != 409 {
		t.Fatalf("key reused for other instance = %v, want idempotency_conflict", err)
	}
	if _, _, err := e.placeKey("renew-1"); apperr.As(err) == nil || apperr.As(err).Code != "idempotency_conflict" {
		t.Fatalf("renew key reused for place = %v, want idempotency_conflict", err)
	}
	if bal := e.balance(e.userID); bal != startFunds-3*6900 {
		t.Fatalf("balance = %d after conflicts", bal)
	}
}

func TestIdempotencyKeyTooLong(t *testing.T) {
	e := newEnv(t, 1)
	if _, _, err := e.placeKey(strings.Repeat("x", 65)); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("long key = %v, want bad_request", err)
	}
}
