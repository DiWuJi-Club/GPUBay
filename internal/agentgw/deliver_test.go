package agentgw_test

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// place 下单并返回实例 id 与 create 指令 id。
func (e *env) place(mid int64) (string, string) {
	e.t.Helper()
	uid := dbtest.CreateUser(e.t, e.pool, "1360000"+itoa(mid), true)
	dbtest.Fund(e.t, e.pool, uid, 1_000_000)
	_, inst, err := e.rental.Place(e.ctx, rental.PlaceRequest{UserID: uid, ProductID: dbtest.ProductID(e.t, e.pool), ImageID: dbtest.ImageID(e.t, e.pool), Duration: catalog.D1d})
	if err != nil {
		e.t.Fatal(err)
	}
	cmds, _ := e.fleet.PendingCommands(e.ctx, mid)
	return inst.ID, cmds[0].ID
}

func itoa(n int64) string { return fmt.Sprintf("%04d", n) }

func (e *env) command(id string) fleet.Command {
	e.t.Helper()
	var instanceID string
	_ = e.pool.QueryRow(e.ctx, `SELECT instance_id FROM agent_commands WHERE id = $1`, id).Scan(&instanceID)
	cmds, err := e.fleet.CommandsForInstance(e.ctx, instanceID, 50)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cmds {
		if c.ID == id {
			return c
		}
	}
	e.t.Fatalf("command %s not found", id)
	return fleet.Command{}
}

func TestDeliveryWaitsForFirstStateReport(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	instID, cmdID := e.place(mid)
	a := e.connect(e.enroll(mid))
	a.hello("10.0.0.10")
	if cfg := a.next(time.Second); cfg.GetConfig() == nil {
		t.Fatal("no config")
	}
	if m := a.next(6 * tick); m != nil { // 没有 StateReport 之前不投递
		t.Fatalf("delivered before report: %v", m)
	}
	if c := e.command(cmdID); c.Status != "pending" {
		t.Fatalf("status before report = %s", c.Status)
	}
	a.report()
	m := a.next(2 * time.Second)
	cmd := m.GetCommand()
	if cmd == nil || cmd.GetId() != cmdID || cmd.GetInstanceId() != instID {
		t.Fatalf("delivered = %v", m)
	}
	cr := cmd.GetCreate()
	if cr.GetImageRef() == "" || cr.GetUpstreamPubkey() != "ssh-ed25519 AAAAtest" || cr.GetBandwidthMbps() != 200 || cr.GetSshPort() != 2222 || cr.GetDataSizeGib() != 2048 {
		t.Fatalf("create body = %v", cr)
	}
	c := e.command(cmdID)
	if c.Status != "sent" || c.Attempts != 1 || c.SentAt == nil {
		t.Fatalf("claimed = %+v", c)
	}
	if _, ok := e.srv.DeliveryTick(cmdID); !ok {
		t.Fatal("delivery tick not recorded")
	}
	// 结果 → 实例 running
	a.result(cmdID, true, "ok")
	e.waitFor("running", func() bool { inst, _ := e.rental.Get(e.ctx, instID); return inst.State == rental.StateRunning })
	if m := a.next(4 * tick); m != nil {
		t.Fatalf("nothing else should be delivered: %v", m)
	}
}

func TestSentCommandsRepushedOnReconnect(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	_, cmdID := e.place(mid)
	creds := e.enroll(mid)
	a := e.up(creds, mid)
	if m := a.next(2 * time.Second); m.GetCommand().GetId() != cmdID {
		t.Fatalf("first delivery = %v", m)
	}
	first := e.command(cmdID)
	if m := a.next(4 * tick); m != nil {
		t.Fatalf("must not re-push to the same stream: %v", m)
	}
	_ = a.stream.CloseSend()
	e.waitFor("disconnected", func() bool { return !e.srv.Connected(mid) })
	e.clk.Advance(time.Minute)
	b := e.up(creds, mid)
	if m := b.next(2 * time.Second); m.GetCommand().GetId() != cmdID {
		t.Fatalf("re-push after reconnect = %v", m)
	}
	again := e.command(cmdID)
	if again.Attempts != 1 || !again.SentAt.After(*first.SentAt) || again.Status != "sent" {
		t.Fatalf("after re-push = %+v (first %+v)", again, first)
	}
}

func TestOwnershipMismatchAbortsStreamAndStopsDelivery(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	a := e.up(e.enroll(mid), mid)
	// 模拟另一副本接管：数据库归属换成别的连接 id
	if err := e.fleet.AttachAgentConn(e.ctx, mid, "other-replica"); err != nil {
		t.Fatal(err)
	}
	_, cmdID := e.place(mid)
	if err := a.waitErr(2 * time.Second); status.Code(err) != codes.Aborted {
		t.Fatalf("stream = %v", err)
	}
	if c := e.command(cmdID); c.Status != "pending" {
		t.Fatalf("command must stay pending for the other replica, got %s", c.Status)
	}
	m, _ := e.fleet.Get(e.ctx, e.pool, mid)
	if !m.Connected { // 归属是别人的，本副本断流时不能清
		t.Fatal("other replica's conn id was cleared")
	}
}

func TestRevokeAbortsStream(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	a := e.up(e.enroll(mid), mid)
	if err := e.fleet.RevokeEnrollment(e.ctx, audit.Actor{AdminID: 1}, mid); err != nil {
		t.Fatal(err)
	}
	if err := a.waitErr(2 * time.Second); status.Code(err) != codes.Aborted {
		t.Fatalf("stream = %v", err)
	}
	e.waitFor("conn cleared", func() bool { return !e.srv.Connected(mid) })
}

func TestLateResultIgnoredAndPullDelivered(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	instID, cmdID := e.place(mid)
	a := e.up(e.enroll(mid), mid)
	if m := a.next(2 * time.Second); m.GetCommand().GetId() != cmdID {
		t.Fatal("not delivered")
	}
	// 平台先把 create 判超时
	if err := e.rental.HandleCommandResult(e.ctx, cmdID, false, "timeout after 15m0s"); err != nil {
		t.Fatal(err)
	}
	a.result(cmdID, true, "late")
	before := e.srv.HeartbeatCount(mid)
	a.heartbeat(&agentpb.Metrics{})
	e.waitFor("hb", func() bool { return e.srv.HeartbeatCount(mid) > before })
	if inst, _ := e.rental.Get(e.ctx, instID); inst.State != rental.StateFailed {
		t.Fatalf("late ack revived instance: %s", inst.State)
	}
	// 机器已 faulty 但仍在线：pull_image 广播照常投递并完成
	if _, err := e.fleet.BroadcastPullImage(e.ctx, "nvcr.io/x/y:2"); err != nil {
		t.Fatal(err)
	}
	m := a.next(2 * time.Second)
	if m.GetCommand().GetPull().GetImageRef() != "nvcr.io/x/y:2" || m.GetCommand().GetInstanceId() != "" {
		t.Fatalf("pull = %v", m)
	}
	a.result(m.GetCommand().GetId(), true, "pulled")
	e.waitFor("pull done", func() bool {
		var st string
		_ = e.pool.QueryRow(e.ctx, `SELECT status FROM agent_commands WHERE id = $1`, m.GetCommand().GetId()).Scan(&st)
		return st == "done"
	})
}

func TestTwoMachinesGetOnlyTheirOwnCommands(t *testing.T) {
	e := newEnv(t)
	m1, m2 := e.machine("SN-001"), e.machine("SN-002")
	a1 := e.up(e.enroll(m1), m1)
	a2 := e.up(e.enroll(m2), m2)
	_, c1 := e.place(m1)
	_, c2 := e.place(m2)
	got1, got2 := a1.next(2*time.Second).GetCommand().GetId(), a2.next(2*time.Second).GetCommand().GetId()
	if got1 != c1 || got2 != c2 {
		t.Fatalf("routing: %s/%s want %s/%s", got1, got2, c1, c2)
	}
}
