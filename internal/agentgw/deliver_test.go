package agentgw_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"spark-cloud/internal/agentgw"
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
	if !strings.Contains(cr.GetHostKeyPem(), "OPENSSH PRIVATE KEY") {
		t.Fatalf("host_key_pem not delivered: %q", cr.GetHostKeyPem())
	}
	var payload string
	_ = e.pool.QueryRow(e.ctx, `SELECT payload::text FROM agent_commands WHERE id = $1`, cmdID).Scan(&payload)
	if strings.Contains(payload, "PRIVATE KEY") || strings.Contains(payload, "host_key") {
		t.Fatalf("private key leaked into agent_commands.payload: %s", payload)
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

func TestCreateCarriesConfiguredUpstreamSSHPort(t *testing.T) {
	e := newEnvWith(t, func(c *agentgw.Config) { c.SSHPort = 2223 })
	mid := e.machine("SN-002")
	_, cmdID := e.place(mid)
	a := e.connect(e.enroll(mid))
	a.hello("10.0.0.10")
	a.next(time.Second) // AgentConfig
	a.report()
	m := a.next(2 * time.Second)
	if cmd := m.GetCommand(); cmd == nil || cmd.GetId() != cmdID || cmd.GetCreate().GetSshPort() != 2223 {
		t.Fatalf("delivered = %v", m)
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

// 旧指令已认领（sent）但推送时流断了，期间又排入更新的指令：重连后必须按创建顺序先旧后新推送，
// 否则例如「到期 stop（sent）→ 续费 start（pending）」会被倒序执行，付费实例最终停机。
func TestRepushKeepsCreationOrderAcrossClaimedAndPending(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	_, older := e.place(mid)
	creds := e.enroll(mid)
	a := e.up(creds, mid)
	if m := a.next(2 * time.Second); m.GetCommand().GetId() != older {
		t.Fatalf("first delivery = %v", m)
	}
	_ = a.stream.CloseSend()
	e.waitFor("disconnected", func() bool { return !e.srv.Connected(mid) })
	e.clk.Advance(time.Minute)
	newer, err := e.fleet.Enqueue(e.ctx, e.pool, mid, "", fleet.CmdPullImage, fleet.PullImagePayload{ImageRef: "nvcr.io/x/y:3"})
	if err != nil {
		t.Fatal(err)
	}
	b := e.up(creds, mid)
	var got []string
	for range 2 {
		m := b.next(2 * time.Second)
		if m == nil {
			t.Fatalf("missing delivery, got %v", got)
		}
		got = append(got, m.GetCommand().GetId())
	}
	if got[0] != older || got[1] != newer {
		t.Fatalf("order = %v, want [%s %s]", got, older, newer)
	}
	if m := b.next(4 * tick); m != nil {
		t.Fatalf("unexpected extra delivery: %v", m)
	}
}

func TestDeliveryRecordPrunedOnResult(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	_, cmdID := e.place(mid)
	a := e.up(e.enroll(mid), mid)
	if m := a.next(2 * time.Second); m.GetCommand().GetId() != cmdID {
		t.Fatal("not delivered")
	}
	if _, ok := e.srv.DeliveryTick(cmdID); !ok {
		t.Fatal("delivery tick not recorded")
	}
	a.result(cmdID, true, "ok")
	e.waitFor("delivery record pruned", func() bool { _, ok := e.srv.DeliveryTick(cmdID); return !ok })
}

// 机器 A 的流发来机器 B 的指令结果：忽略（不中止 A 的流），B 的指令仍为 sent、实例状态不变。
func TestResultForAnotherMachinesCommandIgnored(t *testing.T) {
	e := newEnv(t)
	ma, mb := e.machine("SN-001"), e.machine("SN-002")
	a := e.up(e.enroll(ma), ma)
	b := e.up(e.enroll(mb), mb)
	_, ca := e.place(ma)
	instB, cb := e.place(mb)
	if got := a.next(2 * time.Second).GetCommand().GetId(); got != ca {
		t.Fatalf("A delivery = %s", got)
	}
	if got := b.next(2 * time.Second).GetCommand().GetId(); got != cb {
		t.Fatalf("B delivery = %s", got)
	}
	a.result(cb, false, "forged failure")
	before := e.srv.HeartbeatCount(ma)
	a.heartbeat(&agentpb.Metrics{Load1: 1})
	e.waitFor("heartbeat after forged result", func() bool { return e.srv.HeartbeatCount(ma) > before })
	if err := a.waitErr(100 * time.Millisecond); err != nil {
		t.Fatalf("A stream dropped: %v", err)
	}
	if c := e.command(cb); c.Status != "sent" || c.Result != "" {
		t.Fatalf("B command changed: %+v", c)
	}
	if inst, _ := e.rental.Get(e.ctx, instB); inst.State != rental.StateCreating {
		t.Fatalf("B instance state = %s", inst.State)
	}
	if _, ok := e.srv.DeliveryTick(cb); !ok {
		t.Fatal("B delivery record must be kept")
	}
	// B 自己的回报照常生效
	b.result(cb, true, "ok")
	e.waitFor("B running", func() bool { inst, _ := e.rental.Get(e.ctx, instB); return inst.State == rental.StateRunning })
}

// 网关处理回报失败（如数据库瞬时错误）：中止该流让 Agent 重连，重推后再次回报，指令最终完成。
func TestResultApplyFailureAbortsStreamForReplay(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	instID, cmdID := e.place(mid)
	creds := e.enroll(mid)
	a := e.up(creds, mid)
	if m := a.next(2 * time.Second); m.GetCommand().GetId() != cmdID {
		t.Fatalf("first delivery = %v", m)
	}
	// 故障注入：第一次把指令置为终态的 UPDATE 抛错（序列不随事务回滚，所以只失败一次）
	for _, q := range []string{
		`CREATE SEQUENCE inject_fail_seq`,
		`CREATE FUNCTION inject_fail() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN IF nextval('inject_fail_seq') = 1 THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER inject_fail BEFORE UPDATE ON agent_commands FOR EACH ROW
		 WHEN (NEW.status IN ('done', 'failed')) EXECUTE FUNCTION inject_fail()`,
	} {
		if _, err := e.pool.Exec(e.ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	a.result(cmdID, true, "ok")
	err := a.waitErr(2 * time.Second)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("stream must be aborted with Unavailable, got %v", err)
	}
	if c := e.command(cmdID); c.Status != "sent" {
		t.Fatalf("command after failed apply = %+v", c)
	}
	e.waitFor("disconnected", func() bool { return !e.srv.Connected(mid) })
	b := e.up(creds, mid)
	if m := b.next(2 * time.Second); m.GetCommand().GetId() != cmdID {
		t.Fatalf("re-push after reconnect = %v", m)
	}
	b.result(cmdID, true, "ok") // Agent 从 state.json 回放结果
	e.waitFor("running", func() bool { inst, _ := e.rental.Get(e.ctx, instID); return inst.State == rental.StateRunning })
	if c := e.command(cmdID); c.Status != "done" {
		t.Fatalf("command = %+v", c)
	}
}
