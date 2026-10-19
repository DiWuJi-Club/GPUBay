package rental_test

import (
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

func (e *env) orderState(id string) (string, int64) {
	e.t.Helper()
	var status string
	var refunded int64
	if err := e.pool.QueryRow(e.ctx, `SELECT status, refunded_amount FROM orders WHERE id = $1`, id).Scan(&status, &refunded); err != nil {
		e.t.Fatal(err)
	}
	return status, refunded
}

func TestRefundQuoteRunning(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(6 * time.Hour)
	q, err := e.svc.RefundQuote(e.ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 剩余 18 小时 × 6900 分 / 86400 秒 = 5175 分
	if q.RemainingSeconds != 18*3600 || q.Suggested != 5175 || q.Max != 6900 {
		t.Fatalf("quote = %+v", q)
	}
}

func TestRefundQuoteUsesLatestOrderPrice(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	renew, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w, "")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(12 * time.Hour)
	q, _ := e.svc.RefundQuote(e.ctx, inst.ID)
	remaining := int64(8*24*3600 - 12*3600)
	if q.UnitOrderID != renew.ID || q.RemainingSeconds != remaining || q.Suggested != remaining*41400/(7*86400) || q.Max != 6900+41400 {
		t.Fatalf("quote = %+v", q)
	}
}

func TestRefundQuoteStoppedUsesPendingSeconds(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	expireAndStop(e)
	if q, _ := e.svc.RefundQuote(e.ctx, inst.ID); q.Suggested != 0 || q.Max != 6900 {
		t.Fatalf("stopped without renewal = %+v", q)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w, ""); err != nil {
		t.Fatal(err)
	}
	if q, _ := e.svc.RefundQuote(e.ctx, inst.ID); q.RemainingSeconds != 7*86400 || q.Suggested != 41400 {
		t.Fatalf("stopped with pending renewal = %+v", q)
	}
}

func TestRefundReleasePartialAcrossOrders(t *testing.T) {
	e := newEnv(t, 1)
	first, inst := e.place(catalog.D1d)
	e.ack(true)
	second, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, "")
	if err != nil {
		t.Fatal(err)
	}
	before := e.balance(e.userID)
	got, err := e.svc.RefundRelease(e.ctx, opsActor, inst.ID, 10000, "机器故障无法修复")
	if err != nil || got != 10000 {
		t.Fatalf("refund = %d, %v", got, err)
	}
	if e.balance(e.userID) != before+10000 || e.get(inst.ID).State != rental.StateWiping {
		t.Fatalf("balance=%d state=%s", e.balance(e.userID), e.get(inst.ID).State)
	}
	if s, r := e.orderState(second.ID); s != "refunded" || r != 6900 {
		t.Fatalf("newest order = %s %d", s, r)
	}
	if s, r := e.orderState(first.ID); s != "partially_refunded" || r != 3100 {
		t.Fatalf("older order = %s %d", s, r)
	}
	var refs []string
	rows, _ := e.pool.Query(e.ctx, `SELECT ref_id FROM wallet_ledger WHERE kind = 'refund' ORDER BY id`)
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		refs = append(refs, r)
	}
	rows.Close()
	if len(refs) != 2 || refs[0] != second.ID+"-r1" || refs[1] != first.ID+"-r1" {
		t.Fatalf("refund refs = %v", refs)
	}
	if e.auditCount("instance.refund_release") != 1 {
		t.Fatal("refund not audited")
	}
	e.ack(true)
	if e.get(inst.ID).State != rental.StateReleased {
		t.Fatal("not released after wipe")
	}
}

func TestRefundReleaseLimits(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	for _, c := range []struct {
		amount int64
		reason string
	}{{6901, "x"}, {-1, "x"}, {100, " "}} {
		if _, err := e.svc.RefundRelease(e.ctx, opsActor, inst.ID, c.amount, c.reason); codeOf(err) != "bad_request" {
			t.Fatalf("refund(%d, %q) = %v", c.amount, c.reason, err)
		}
	}
	if e.get(inst.ID).State != rental.StateRunning || e.balance(e.userID) != startFunds-6900 {
		t.Fatal("rejected refund must not change anything")
	}
	if _, err := e.svc.RefundRelease(e.ctx, opsActor, inst.ID, 0, "用户要求立即释放"); err != nil {
		t.Fatal(err)
	}
	if e.get(inst.ID).State != rental.StateWiping || e.balance(e.userID) != startFunds-6900 {
		t.Fatal("zero refund should only release")
	}
	if _, err := e.svc.RefundRelease(e.ctx, opsActor, inst.ID, 100, "again"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("refund while wiping = %v", err)
	}
}

func TestRefundReleaseCreating(t *testing.T) {
	e := newEnv(t, 1)
	ord, inst := e.place(catalog.D1d)
	if _, err := e.svc.RefundRelease(e.ctx, opsActor, inst.ID, 6900, "开通卡住，全额退"); err != nil {
		t.Fatal(err)
	}
	if s, r := e.orderState(ord.ID); s != "refunded" || r != 6900 || e.balance(e.userID) != startFunds {
		t.Fatalf("order=%s %d balance=%d", s, r, e.balance(e.userID))
	}
	if n := countPending(e, e.machines[0], fleet.CmdCreateInstance); n != 0 {
		t.Fatalf("create command still pending: %d", n)
	}
}

func TestRefundAfterMigrationUsesOriginalOrders(t *testing.T) {
	e := newEnv(t, 2)
	ord, old := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(12 * time.Hour)
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "换机")
	if err != nil {
		t.Fatal(err)
	}
	q, err := e.svc.RefundQuote(e.ctx, moved.ID)
	if err != nil || q.Max != 6900 || q.UnitOrderID != ord.ID || q.RemainingSeconds != 12*3600 || q.Suggested != 3450 {
		t.Fatalf("quote on migrated instance = %+v, %v", q, err)
	}
	if _, err := e.svc.RefundRelease(e.ctx, opsActor, moved.ID, 3450, "换机后仍故障"); err != nil {
		t.Fatal(err)
	}
	if s, r := e.orderState(ord.ID); s != "partially_refunded" || r != 3450 {
		t.Fatalf("original order = %s %d", s, r)
	}
}

func TestCreateFailureRecordsRefundedAmount(t *testing.T) {
	e := newEnv(t, 1)
	ord, _ := e.place(catalog.D1d)
	e.ack(false)
	if s, r := e.orderState(ord.ID); s != "refunded" || r != 6900 {
		t.Fatalf("order = %s %d", s, r)
	}
}

func TestCreateFailureAfterMigrationRefundsTransferredTime(t *testing.T) {
	e := newEnv(t, 2)
	ord, old := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(12 * time.Hour)
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "换机")
	if err != nil {
		t.Fatal(err)
	}
	before := e.balance(e.userID)
	e.ack(false) // 新实例开通失败（同时原实例洗机失败，仅把原机器标为故障）
	if got := e.balance(e.userID); got != before+3450 {
		t.Fatalf("balance = %d, want %d", got, before+3450)
	}
	if s, r := e.orderState(ord.ID); s != "partially_refunded" || r != 3450 {
		t.Fatalf("original order = %s %d", s, r)
	}
	if e.get(moved.ID).State != rental.StateFailed {
		t.Fatalf("migrated instance state = %s", e.get(moved.ID).State)
	}
	var ref, remark string
	if err := e.pool.QueryRow(e.ctx, `SELECT ref_id, remark FROM wallet_ledger WHERE kind = 'refund'`).Scan(&ref, &remark); err != nil {
		t.Fatal(err)
	}
	if ref != ord.ID+"-r1" || remark != "开通失败退款 "+moved.ID {
		t.Fatalf("refund ledger = %q %q", ref, remark)
	}
	var alerts int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = 'create_failed' AND instance_id = $1`, moved.ID).Scan(&alerts); err != nil {
		t.Fatal(err)
	}
	if alerts != 1 {
		t.Fatalf("create_failed alerts = %d", alerts)
	}
}

func TestCreateFailureAfterMigrationWithRenewalRefundsBoth(t *testing.T) {
	e := newEnv(t, 2)
	ord, old := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(12 * time.Hour)
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "换机")
	if err != nil {
		t.Fatal(err)
	}
	renew, err := e.svc.Renew(e.ctx, e.userID, moved.ID, catalog.D1d, "")
	if err != nil {
		t.Fatal(err)
	}
	e.ack(false)
	// 续费全额退回，转入的 12 小时退 3450 分
	if got, want := e.balance(e.userID), int64(startFunds-6900-6900+6900+3450); got != want {
		t.Fatalf("balance = %d, want %d", got, want)
	}
	if s, r := e.orderState(ord.ID); s != "partially_refunded" || r != 3450 {
		t.Fatalf("original order = %s %d", s, r)
	}
	if s, r := e.orderState(renew.ID); s != "refunded" || r != 6900 {
		t.Fatalf("renewal order = %s %d", s, r)
	}
}

func TestCreateFailureAfterMigrationDoesNotRefundFreeExtension(t *testing.T) {
	e := newEnv(t, 2)
	ord, old := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(12 * time.Hour)
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "换机")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, moved.ID, 30*86400, "补偿"); err != nil {
		t.Fatal(err)
	}
	before := e.balance(e.userID)
	e.ack(false)
	// 赠送的 30 天不能按钱退，只退转入的 12 小时
	if got := e.balance(e.userID); got != before+3450 {
		t.Fatalf("balance = %d, want %d", got, before+3450)
	}
	if s, r := e.orderState(ord.ID); s != "partially_refunded" || r != 3450 {
		t.Fatalf("original order = %s %d", s, r)
	}
}

func TestRefundQuoteUsesQuantity(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	renew, err := e.svc.RenewQuantity(e.ctx, e.userID, inst.ID, catalog.D1d, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(12 * time.Hour)
	q, err := e.svc.RefundQuote(e.ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	remaining := int64(4*86400 - 12*3600)
	// 单价按最新订单：20700 分 / (3 × 86400 秒)
	if q.UnitOrderID != renew.ID || q.RemainingSeconds != remaining || q.Suggested != remaining*20700/(3*86400) || q.Max != 6900+20700 {
		t.Fatalf("quote = %+v", q)
	}
}

// refundAll 的 ownSecs 必须按 duration × quantity 计：新实例自己的 3 单位续费若只算 1 天，
// pending − ownSecs 会多出 2 天，被当成「转入时长」按前身订单单价多退。
func TestCreateFailureAfterMigrationCountsOwnQuantity(t *testing.T) {
	e := newEnv(t, 2)
	ord, old := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(12 * time.Hour)
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "换机")
	if err != nil {
		t.Fatal(err)
	}
	renew, err := e.svc.RenewQuantity(e.ctx, e.userID, moved.ID, catalog.D1d, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	// 换机记录的秒数是退款上限，正常数据下它恰好遮住 ownSecs 的错误；把它调大（模拟上限不起作用），
	// 让结果只取决于 pending − ownSecs：正确 = 12h，漏乘 quantity = 12h + 2d。
	if _, err := e.pool.Exec(e.ctx, `UPDATE instance_transfers SET seconds = seconds + 2 * 86400 WHERE to_instance_id = $1`, moved.ID); err != nil {
		t.Fatal(err)
	}
	before := e.balance(e.userID)
	e.ack(false)
	// 自己的续费单全退 20700；前身只退转入的 12 小时 = 3450
	if got := e.balance(e.userID); got != before+20700+3450 {
		t.Fatalf("balance = %d, want %d", got, before+20700+3450)
	}
	if s, r := e.orderState(renew.ID); s != "refunded" || r != 20700 {
		t.Fatalf("renew order = %s %d", s, r)
	}
	if s, r := e.orderState(ord.ID); s != "partially_refunded" || r != 3450 {
		t.Fatalf("predecessor order = %s %d", s, r)
	}
}
