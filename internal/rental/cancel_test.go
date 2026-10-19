package rental_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func (e *env) book(start time.Time) rental.Instance {
	e.t.Helper()
	res, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start))
	if err != nil {
		e.t.Fatalf("book: %v", err)
	}
	return res.Instances[0]
}

func (e *env) auditCountOf(action string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestCancelQuoteTiers(t *testing.T) {
	e := newEnv(t, 2)
	near := e.book(dbtest.T0.Add(3 * time.Hour)) // 距开始 3h：冷静期内全退，之后 50%
	q, err := e.svc.CancelQuote(e.ctx, e.userID, near.ID)
	if err != nil || q.Percent != 100 || q.Refund != 6900 || q.Max != 6900 || !q.Deadline.Equal(dbtest.T0.Add(2*time.Hour)) {
		t.Fatalf("cool-off quote = %+v, %v", q, err)
	}
	e.clock.Advance(16 * time.Minute)
	if q, _ = e.svc.CancelQuote(e.ctx, e.userID, near.ID); q.Percent != 50 || q.Refund != 3450 {
		t.Fatalf("late quote = %+v", q)
	}
	far := e.book(dbtest.T0.Add(40 * time.Hour)) // 距开始 > 24h：全退
	e.clock.Advance(16 * time.Minute)
	if q, _ = e.svc.CancelQuote(e.ctx, e.userID, far.ID); q.Percent != 100 || q.Refund != 6900 {
		t.Fatalf("far quote = %+v", q)
	}
	if _, err := e.svc.CancelQuote(e.ctx, e.userID+1, far.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user = %v", err)
	}
}

func TestUserCancelRefundsAndClosesLease(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	e.clock.Advance(20 * time.Minute) // 过了冷静期，距开始 2h40m → 50%
	got, err := e.svc.Cancel(e.ctx, e.userID, inst.ID, "1.2.3.4")
	if err != nil || got.State != rental.StateCancelled || got.CancelledAt == nil {
		t.Fatalf("cancel = %+v, %v", got, err)
	}
	if b := e.balance(e.userID); b != startFunds-6900+3450 {
		t.Fatalf("balance = %d", b)
	}
	if _, _, status := e.lease(inst.ID); status != "cancelled" {
		t.Fatalf("lease status = %s", status)
	}
	var keyEnc []byte
	_ = e.pool.QueryRow(e.ctx, `SELECT ssh_host_key_enc FROM instances WHERE id = $1`, inst.ID).Scan(&keyEnc)
	if keyEnc != nil {
		t.Fatal("host key must be cleared")
	}
	if e.auditCountOf("booking.cancel") != 1 {
		t.Fatal("audit missing")
	}
	events, _ := e.svc.InstanceEvents(e.ctx, inst.ID)
	if last := events[len(events)-1]; last.ToState != rental.StateCancelled || last.Reason != "user cancel, refund 3450" {
		t.Fatalf("event = %+v", last)
	}
	if _, err := e.svc.Cancel(e.ctx, e.userID, inst.ID, ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("cancel twice = %v", err)
	}
	// 机器可以被别的预订再次使用
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, ptr(dbtest.T0.Add(3*time.Hour)))); err != nil {
		t.Fatalf("rebook after cancel = %v", err)
	}
}

func TestCancelNotAllowedAfterProvisionTime(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	e.clock.Advance(2 * time.Hour) // 正好到开通时刻（S − 1h）
	if _, err := e.svc.Cancel(e.ctx, e.userID, inst.ID, ""); !errors.Is(err, apperr.ErrCancelNotAllowed) {
		t.Fatalf("at deadline = %v", err)
	}
	if e.get(inst.ID).State != rental.StateReserved {
		t.Fatal("must stay reserved")
	}
}

func TestCancelRefundsPreStartRenewalNewestFirst(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	renew, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.PendingSeconds != 2*86400 {
		t.Fatalf("pending after pre-start renew = %d", got.PendingSeconds)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(3*time.Hour + 48*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper after renew = %v", upper)
	}
	e.clock.Advance(20 * time.Minute) // 50%：13800 × 50% = 6900，先退最新的续费单
	if _, err := e.svc.Cancel(e.ctx, e.userID, inst.ID, ""); err != nil {
		t.Fatal(err)
	}
	if status, refunded := e.orderState(renew.ID); status != "refunded" || refunded != 6900 {
		t.Fatalf("renew order = %s %d", status, refunded)
	}
	if b := e.balance(e.userID); b != startFunds-13800+6900 {
		t.Fatalf("balance = %d", b)
	}
}

func TestAdminCancelBookingFullRefund(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	e.clock.Advance(20 * time.Minute)
	if _, err := e.svc.AdminCancelBooking(e.ctx, audit.Actor{AdminID: 9}, inst.ID, ""); apperr.As(err) == nil {
		t.Fatalf("empty reason = %v", err)
	}
	refunded, err := e.svc.AdminCancelBooking(e.ctx, audit.Actor{AdminID: 9}, inst.ID, "机房断电")
	if err != nil || refunded != 6900 {
		t.Fatalf("admin cancel = %d, %v", refunded, err)
	}
	if b := e.balance(e.userID); b != startFunds {
		t.Fatalf("balance = %d", b)
	}
	if e.auditCountOf("booking.admin_cancel") != 1 || !slices.Contains(e.notifier.Calls(), "booking_cancelled:"+inst.ID+":平台取消") {
		t.Fatalf("audit / sms missing: %v", e.notifier.Calls())
	}
	if _, err := e.svc.AdminCancelBooking(e.ctx, audit.Actor{AdminID: 9}, inst.ID, "again"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("cancel twice = %v", err)
	}
}

func TestCancelQuantityBookingRefundsPercentOfPaid(t *testing.T) {
	e := newEnv(t, 1)
	req := e.batchReq(1, ptr(dbtest.T0.Add(3*time.Hour)))
	req.Quantity = 3
	res, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	inst := res.Instances[0]
	e.clock.Advance(20 * time.Minute) // 50%：3 × 6900 × 50% = 10350
	if q, err := e.svc.CancelQuote(e.ctx, e.userID, inst.ID); err != nil || q.Max != 3*6900 || q.Refund != 10350 {
		t.Fatalf("quote = %+v, %v", q, err)
	}
	if _, err := e.svc.Cancel(e.ctx, e.userID, inst.ID, ""); err != nil {
		t.Fatal(err)
	}
	if b := e.balance(e.userID); b != startFunds-3*6900+10350 {
		t.Fatalf("balance = %d", b)
	}
}
