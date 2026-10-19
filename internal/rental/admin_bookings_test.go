package rental_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func TestAdminReassignToMachineAndAuto(t *testing.T) {
	e := newEnv(t, 3)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	actor := audit.Actor{AdminID: 9}
	if _, err := e.svc.AdminReassign(e.ctx, actor, inst.ID, nil, " "); codeOf(err) != "bad_request" {
		t.Fatalf("empty reason = %v", err)
	}
	same := inst.MachineID
	if _, err := e.svc.AdminReassign(e.ctx, actor, inst.ID, &same, "x"); !errors.Is(err, apperr.ErrMachineUnavailable) {
		t.Fatalf("same machine = %v", err)
	}
	var others []int64
	for _, m := range e.machines {
		if m != inst.MachineID {
			others = append(others, m)
		}
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET state = 'maintenance' WHERE id = $1`, others[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminReassign(e.ctx, actor, inst.ID, &others[0], "x"); !errors.Is(err, apperr.ErrMachineUnavailable) {
		t.Fatalf("maintenance target = %v", err)
	}
	got, err := e.svc.AdminReassign(e.ctx, actor, inst.ID, &others[1], "换机柜")
	if err != nil || got.MachineID != others[1] || got.ReassignCount != 1 {
		t.Fatalf("reassign to = %+v, %v", got, err)
	}
	if _, _, status := e.lease(inst.ID); status != "reserved" {
		t.Fatalf("lease status = %s", status)
	}
	if ev := e.lastEvent(inst.ID); !strings.HasPrefix(ev.Reason, "reassigned ") || !strings.Contains(ev.Reason, "admin: 换机柜") {
		t.Fatalf("event = %q", ev.Reason)
	}
	// 自动：唯一另一台在线可用机器 = 原来那台（others[0] 在维护）
	got, err = e.svc.AdminReassign(e.ctx, actor, inst.ID, nil, "自动")
	if err != nil || got.MachineID != same || got.ReassignCount != 2 {
		t.Fatalf("auto reassign = %+v, %v", got, err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET state = 'maintenance' WHERE id = $1`, others[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminReassign(e.ctx, actor, inst.ID, nil, "没得换"); !errors.Is(err, apperr.ErrMachineUnavailable) {
		t.Fatalf("auto without candidate = %v", err)
	}
	if n := e.auditCountOf("booking.reassign"); n != 2 {
		t.Fatalf("audits = %d", n)
	}
	other := e.book(dbtest.T0.Add(10 * 24 * time.Hour))
	if _, err := e.svc.Cancel(e.ctx, e.userID, other.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminReassign(e.ctx, actor, other.ID, nil, "x"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("cancelled booking = %v", err)
	}
}

// 指定机器改派：实例的 group_id 改为目标机器的机组（之后的自动改派只在新机组里挑）；
// 目标机器行锁 FOR NO KEY UPDATE：别的事务锁着目标机器时等待（不是跳过后报 machine_unavailable），锁释放后成功。
func TestAdminReassignToTakesTargetGroupAndLocksTarget(t *testing.T) {
	e := newEnv(t, 3)
	ga, gh := e.group("A", true), e.group("留池", false)
	e.setGroup(e.machines[0], ga)
	e.setGroup(e.machines[1], ga)
	e.setGroup(e.machines[2], gh)
	start := dbtest.T0.Add(3 * time.Hour)
	req := e.batchReq(1, &start)
	req.GroupID = &ga
	res, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	inst := res.Instances[0]
	target := e.machines[2]
	actor := audit.Actor{AdminID: 9}

	lockTx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(e.ctx, `SELECT id FROM machines WHERE id = $1 FOR NO KEY UPDATE`, target); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(e.ctx, 300*time.Millisecond)
	_, err = e.svc.AdminReassign(ctx, actor, inst.ID, &target, "换机柜")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		_ = lockTx.Rollback(e.ctx)
		t.Fatalf("reassign to a locked target must wait, got %v", err)
	}
	if err := lockTx.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.MachineID != inst.MachineID || got.ReassignCount != 0 {
		t.Fatalf("timed-out reassign changed the instance: %+v", got)
	}

	got, err := e.svc.AdminReassign(e.ctx, actor, inst.ID, &target, "换机柜")
	if err != nil || got.MachineID != target || got.GroupID == nil || *got.GroupID != gh {
		t.Fatalf("reassign to hidden-group machine = %+v, %v", got, err)
	}
	if g := e.get(inst.ID).GroupID; g == nil || *g != gh {
		t.Fatalf("stored group_id = %v", g)
	}
	// 新机组里没有别的机器：自动改派无候选（旧机组 A 的两台不再是候选）
	if _, err := e.svc.AdminReassign(e.ctx, actor, inst.ID, nil, "自动"); !errors.Is(err, apperr.ErrMachineUnavailable) {
		t.Fatalf("auto reassign after group change = %v", err)
	}
}

// 后台改派整个事务遇死锁（40P01）/ 序列化失败（40001）重试一次（规格 §14）。
func TestAdminReassignRetriesOnDeadlockAndSerialization(t *testing.T) {
	for _, code := range []string{"40P01", "40001"} {
		t.Run(code, func(t *testing.T) {
			e := newEnv(t, 2)
			inst := e.book(dbtest.T0.Add(3 * time.Hour))
			// 审计写入时第一次抛出 code（序列不随事务回滚，第二次放行）
			if _, err := e.pool.Exec(e.ctx, `CREATE SEQUENCE fail_once;
				CREATE FUNCTION fail_once() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					IF nextval('fail_once') = 1 THEN RAISE EXCEPTION 'injected' USING ERRCODE = '`+code+`'; END IF;
					RETURN NEW;
				END $$;
				CREATE TRIGGER fail_once BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION fail_once();`); err != nil {
				t.Fatal(err)
			}
			got, err := e.svc.AdminReassign(e.ctx, audit.Actor{AdminID: 9}, inst.ID, nil, "重试")
			if err != nil || got.ReassignCount != 1 || got.MachineID == inst.MachineID {
				t.Fatalf("reassign = %+v, %v", got, err)
			}
			var calls int64
			if err := e.pool.QueryRow(e.ctx, `SELECT last_value FROM fail_once`).Scan(&calls); err != nil || calls != 2 {
				t.Fatalf("attempts = %d, %v", calls, err)
			}
			if n := e.auditCountOf("booking.reassign"); n != 1 {
				t.Fatalf("audits = %d", n)
			}
		})
	}
}

// 已取消的预订不能再重置密码、切换密码登录或改端口。
func TestCancelledBookingRejectsAccessChanges(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	if _, err := e.svc.Cancel(e.ctx, e.userID, inst.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResetPassword(e.ctx, e.userID, inst.ID, ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("reset password = %v", err)
	}
	if _, err := e.svc.SetPasswordAuth(e.ctx, e.userID, inst.ID, true, ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("password auth = %v", err)
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 9000, "private", "", ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("add port = %v", err)
	}
	if got := e.get(inst.ID); got.State != rental.StateCancelled {
		t.Fatalf("state = %s", got.State)
	}
}
