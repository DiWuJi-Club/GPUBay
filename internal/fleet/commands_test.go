package fleet_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
)

func TestCommandLifecycleIsIdempotent(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN001")

	id, err := svc.Enqueue(ctx, pool, mid, "ins-abc", fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: "img:1"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := svc.PendingCommands(ctx, mid)
	if err != nil || len(pending) != 1 || pending[0].ID != id || pending[0].Type != fleet.CmdCreateInstance {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	var p fleet.CreateInstancePayload
	if err := json.Unmarshal(pending[0].Payload, &p); err != nil || p.ImageRef != "img:1" {
		t.Fatalf("payload = %s, %v", pending[0].Payload, err)
	}

	cmd, applied, err := svc.CompleteCommand(ctx, pool, id, true, "ok")
	if err != nil || !applied || cmd.Status != "done" || cmd.InstanceID != "ins-abc" {
		t.Fatalf("complete = %+v applied=%v err=%v", cmd, applied, err)
	}
	cmd, applied, err = svc.CompleteCommand(ctx, pool, id, false, "late failure")
	if err != nil || applied || cmd.Status != "done" {
		t.Fatalf("second complete = %+v applied=%v err=%v", cmd, applied, err)
	}
	if pending, _ := svc.PendingCommands(ctx, mid); len(pending) != 0 {
		t.Fatalf("pending after done = %+v", pending)
	}
}

func TestCancelCommands(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN001")
	id, _ := svc.Enqueue(ctx, pool, mid, "ins-abc", fleet.CmdStartInstance, nil)
	_, _ = svc.Enqueue(ctx, pool, mid, "ins-other", fleet.CmdStopInstance, nil)

	if err := svc.CancelCommands(ctx, pool, "ins-abc", "superseded"); err != nil {
		t.Fatal(err)
	}
	pending, _ := svc.PendingCommands(ctx, mid)
	if len(pending) != 1 || pending[0].InstanceID != "ins-other" {
		t.Fatalf("pending = %+v", pending)
	}
	if _, applied, _ := svc.CompleteCommand(ctx, pool, id, true, "ok"); applied {
		t.Fatal("cancelled command must not be applied")
	}
}

func TestHasOpenCommand(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN001")
	id, _ := svc.Enqueue(ctx, pool, mid, "ins-abc", fleet.CmdStartInstance, nil)
	if ok, err := svc.HasOpenCommand(ctx, pool, "ins-abc", fleet.CmdStartInstance); err != nil || !ok {
		t.Fatalf("open = %v, %v", ok, err)
	}
	if ok, _ := svc.HasOpenCommand(ctx, pool, "ins-abc", fleet.CmdStopInstance); ok {
		t.Fatal("other type must not match")
	}
	if _, _, err := svc.CompleteCommand(ctx, pool, id, false, "boom"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.HasOpenCommand(ctx, pool, "ins-abc", fleet.CmdStartInstance); ok {
		t.Fatal("completed command must not be open")
	}
}

func TestCommandInstanceID(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN001")
	id, _ := svc.Enqueue(ctx, pool, mid, "ins-abc", fleet.CmdStartInstance, nil)
	if got, err := svc.CommandInstanceID(ctx, pool, id); err != nil || got != "ins-abc" {
		t.Fatalf("CommandInstanceID = %q, %v", got, err)
	}
	if _, err := svc.CommandInstanceID(ctx, pool, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown id = %v, want not found", err)
	}
}

func TestEnqueueUnknownMachineIsBadRequest(t *testing.T) {
	svc, pool, _ := newEnv(t)
	_, err := svc.Enqueue(context.Background(), pool, 9999, "", fleet.CmdWipeInstance, nil)
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" || ae.Message != "机器不存在" {
		t.Fatalf("want bad_request 机器不存在, got %v", err)
	}
}

func TestClaimPendingOnlyForOwnedConns(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	a := dbtest.CreateMachine(t, pool, "SN-A")
	b := dbtest.CreateMachine(t, pool, "SN-B")
	if err := svc.AttachAgentConn(ctx, a, "conn-a"); err != nil {
		t.Fatal(err)
	}
	if err := svc.AttachAgentConn(ctx, b, "conn-b"); err != nil {
		t.Fatal(err)
	}
	first, _ := svc.Enqueue(ctx, pool, a, "ins-aaaaaaaa", fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: "img:1"})
	clk.Advance(time.Second)
	second, _ := svc.Enqueue(ctx, pool, a, "ins-aaaaaaaa", fleet.CmdStopInstance, nil)
	_, _ = svc.Enqueue(ctx, pool, b, "ins-bbbbbbbb", fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: "img:1"})

	if got, err := svc.ClaimPending(ctx, nil); err != nil || got != nil {
		t.Fatalf("claim nil = %v, %v", got, err)
	}
	got, err := svc.ClaimPending(ctx, []string{"conn-a", "conn-zzz"})
	if err != nil || len(got) != 2 || got[0].ID != first || got[1].ID != second || got[0].ConnID != "conn-a" {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	if got[0].Status != "sent" || got[0].Attempts != 1 || got[0].SentAt == nil || !got[0].SentAt.Equal(clk.Now()) {
		t.Fatalf("claimed row = %+v", got[0])
	}
	if again, _ := svc.ClaimPending(ctx, []string{"conn-a"}); len(again) != 0 {
		t.Fatalf("second claim = %+v", again)
	}
	// 机器 b 的指令仍 pending；用 b 的旧连接 id 认领不到
	if pend, _ := svc.PendingCommands(ctx, b); len(pend) != 1 || pend[0].Status != "pending" {
		t.Fatalf("b pending = %+v", pend)
	}
	_ = svc.AttachAgentConn(ctx, b, "conn-b2")
	if got, _ := svc.ClaimPending(ctx, []string{"conn-b"}); len(got) != 0 {
		t.Fatalf("stale conn claimed %+v", got)
	}
	if got, _ := svc.ClaimPending(ctx, []string{"conn-b2"}); len(got) != 1 || got[0].ConnID != "conn-b2" {
		t.Fatalf("new conn claim = %+v", got)
	}
}

func TestSentCommandsTouchAndSentBefore(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	a := dbtest.CreateMachine(t, pool, "SN-A")
	_ = svc.AttachAgentConn(ctx, a, "c")
	id, _ := svc.Enqueue(ctx, pool, a, "ins-aaaaaaaa", fleet.CmdWipeInstance, nil)
	if _, err := svc.ClaimPending(ctx, []string{"c"}); err != nil {
		t.Fatal(err)
	}
	t0 := clk.Now()
	sent, err := svc.SentCommands(ctx, a)
	if err != nil || len(sent) != 1 || sent[0].ID != id {
		t.Fatalf("sent = %+v, %v", sent, err)
	}
	clk.Advance(time.Minute)
	if err := svc.TouchSent(ctx, []string{id, "nope"}); err != nil {
		t.Fatal(err)
	}
	sent, _ = svc.SentCommands(ctx, a)
	if !sent[0].SentAt.Equal(t0.Add(time.Minute)) || sent[0].Attempts != 1 {
		t.Fatalf("after touch = %+v", sent[0])
	}
	if got, _ := svc.SentBefore(ctx, t0); len(got) != 0 {
		t.Fatalf("sent before touch = %+v", got)
	}
	got, _ := svc.SentBefore(ctx, clk.Now())
	if len(got) != 1 || got[0].ID != id || got[0].Type != fleet.CmdWipeInstance {
		t.Fatalf("sent before now = %+v", got)
	}
	// 离线机器的 sent 指令不计时
	if _, err := svc.MarkOfflineStale(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.SentBefore(ctx, clk.Now()); len(got) != 0 {
		t.Fatalf("offline sent = %+v", got)
	}
	// 已完成的不再是 sent
	_, _ = svc.Heartbeat(ctx, a)
	if _, _, err := svc.CompleteCommand(ctx, pool, id, true, "ok"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.SentCommands(ctx, a); len(got) != 0 {
		t.Fatalf("done still sent: %+v", got)
	}
}

func TestHasAnyOpenCommandAndPullBroadcast(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	a := dbtest.CreateMachine(t, pool, "SN-A")
	b := dbtest.CreateMachine(t, pool, "SN-B")
	c := dbtest.CreateMachine(t, pool, "SN-C")
	_, _ = pool.Exec(ctx, `UPDATE machines SET agent_online = false WHERE id = $1`, c)
	if open, _ := svc.HasAnyOpenCommand(ctx, pool, "ins-aaaaaaaa"); open {
		t.Fatal("no commands yet")
	}
	id, _ := svc.Enqueue(ctx, pool, a, "ins-aaaaaaaa", fleet.CmdStartInstance, nil)
	if open, _ := svc.HasAnyOpenCommand(ctx, pool, "ins-aaaaaaaa"); !open {
		t.Fatal("pending start must count")
	}
	_, _, _ = svc.CompleteCommand(ctx, pool, id, false, "x")
	if open, _ := svc.HasAnyOpenCommand(ctx, pool, "ins-aaaaaaaa"); open {
		t.Fatal("failed command is not open")
	}
	n, err := svc.BroadcastPullImage(ctx, "nvcr.io/x/y:1")
	if err != nil || n != 2 {
		t.Fatalf("broadcast = %d, %v", n, err)
	}
	for _, mid := range []int64{a, b} {
		cmds, _ := svc.PendingCommands(ctx, mid)
		var p fleet.PullImagePayload
		if len(cmds) != 1 || cmds[0].Type != fleet.CmdPullImage || cmds[0].InstanceID != "" ||
			json.Unmarshal(cmds[0].Payload, &p) != nil || p.ImageRef != "nvcr.io/x/y:1" {
			t.Fatalf("machine %d pull = %+v", mid, cmds)
		}
	}
	if cmds, _ := svc.PendingCommands(ctx, c); len(cmds) != 0 {
		t.Fatalf("offline machine got pull: %+v", cmds)
	}
	if ids, _ := svc.OnlineMachineIDs(ctx); len(ids) != 2 || ids[0] != a || ids[1] != b {
		t.Fatalf("online ids = %v", ids)
	}
}
