package app_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/agent/fakeagent"
	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// twoReplicas 在同一个数据库上起两个 platform（两个网关监听）。
func twoReplicas(t *testing.T) (*flow, *app.App, string) {
	t.Helper()
	f := newFlow(t)
	b, addrB := buildApp(t, f.pool, f.clk)
	return f, b, addrB
}

// 旧 Agent（A 上）与被撤销的 Agent 都用 NoReconnect：否则它们退避后重连会把对方再顶掉，断言不稳定。
func noReconnect(o *fakeagent.Options) { o.NoReconnect = true }

func TestReplicaTakeoverAndRevoke(t *testing.T) {
	f, b, addrB := twoReplicas(t)
	mid, agA := f.enrollAgent("SN-001", true, noReconnect) // 连在 A
	agB, err := fakeagent.Start(f.ctx, fakeagent.Options{Addr: addrB, CAPEM: f.a.Gateway.CAPEM(), Creds: agA.Creds(), Stepped: true, NoReconnect: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agB.Stop)
	// 后连上的副本赢：A 在下一个 tick 发现归属不再是自己，关闭流
	// （客户端看到流结束时，A 的 Connect 已返回，DetachAgentConn 已执行完）
	f.waitFor("A aborted", func() bool { return !agA.Client.Connected() && !f.a.Gateway.Connected(mid) })
	if !b.Gateway.Connected(mid) {
		t.Fatal("B must own the stream")
	}
	m, _ := f.a.Fleet.Get(f.ctx, f.pool, mid)
	if !m.Connected {
		t.Fatal("A's detach cleared B's ownership")
	}
	// 撤销接入：B 上的旧流在下一个 tick 被断
	if err := f.a.Fleet.RevokeEnrollment(f.ctx, audit.Actor{AdminID: 1}, mid); err != nil {
		t.Fatal(err)
	}
	f.waitFor("B aborted after revoke", func() bool { return !agB.Client.Connected() && !b.Gateway.Connected(mid) })
	m, _ = f.a.Fleet.Get(f.ctx, f.pool, mid)
	if m.Enrolled || m.Connected {
		t.Fatalf("after revoke = %+v", m)
	}
}

// Agent 从 A 切到 B：A 已认领为 sent 但 Agent 尚未执行的指令、以及切换后新入队的指令，都须在 B 处理完首个报告后 ≤2 个 tick 内由 B 投递。
func TestSwitchDeliversWithinTwoTicks(t *testing.T) {
	f, b, addrB := twoReplicas(t)
	mid, agA := f.enrollAgent("SN-001", true, noReconnect)
	c := f.renter()
	id := f.order(c)
	createID := f.commandID(id, "create_instance")
	f.waitFor("A delivered", func() bool { return agA.Client.Received(createID) }) // A 认领为 sent，步进式 Agent 不执行
	// 切到 B：A 的流被顶掉（agA 不重连），B 通过「sent 补推」投递同一条指令
	agB, err := fakeagent.Start(f.ctx, fakeagent.Options{Addr: addrB, CAPEM: f.a.Gateway.CAPEM(), Creds: agA.Creds(), Stepped: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agB.Stop)
	// push 先发送、后记录 tick：Agent 收到后还要等 DeliveryTick 落表
	f.waitFor("B delivered", func() bool {
		_, ok := b.Gateway.DeliveryTick(createID)
		return agB.Client.Received(createID) && ok
	})
	ready, ok := b.Gateway.ReadyTick(mid)
	delivered, _ := b.Gateway.DeliveryTick(createID)
	if !ok {
		t.Fatal("ready tick missing")
	}
	if delivered-ready > 2 {
		t.Fatalf("re-push at tick %d, ready at %d: more than 2 ticks", delivered, ready)
	}
	if _, err := agB.Client.Step(f.ctx, nil); err != nil {
		t.Fatal(err)
	}
	f.waitState(id, rental.StateRunning)
	if agA.Runtime.Containers()[id].Exists {
		t.Fatal("A's agent must not have executed")
	}
	// 切换后新入队的指令（到期停机）由 B 在 ≤2 tick 内认领投递
	f.clk.Advance(24*time.Hour + time.Second)
	f.syncHeartbeat(mid, agB, true)
	before := b.Gateway.Ticks() // 入队前取 tick 号：界限量的是入队到投递的真实间隔
	f.sweep()
	stopID := f.commandID(id, "stop_instance")
	if stopID == "" {
		t.Fatal("stop not enqueued")
	}
	f.waitFor("stop delivered by B", func() bool {
		_, ok := b.Gateway.DeliveryTick(stopID)
		return agB.Client.Received(stopID) && ok
	})
	if tick, _ := b.Gateway.DeliveryTick(stopID); tick-before > 2 {
		t.Fatalf("stop delivered at tick %d, enqueued around %d", tick, before)
	}
	f.a.Gateway.TickNow(f.ctx) // 停机指令已是 sent 后 A 至少再完整跑一个 tick（TickNow 串行），否定断言才有意义
	if _, ok := f.a.Gateway.DeliveryTick(stopID); ok {
		t.Fatal("A must not deliver after losing ownership")
	}
}

// 并发 50 单抢 10 台不超卖：恰好 10 单成功，其余 no_capacity；10 个 Agent 全部开通成功。
func TestConcurrentOrdersDoNotOversell(t *testing.T) {
	f := newFlow(t)
	agents := map[int64]*fakeagent.Agent{}
	for i := 0; i < 10; i++ {
		mid, ag := f.enrollAgent("SN-"+string(rune('A'+i)), false)
		agents[mid] = ag
	}
	users := make([]int64, 50)
	for i := range users {
		users[i] = dbtest.CreateUser(t, f.pool, fmt.Sprintf("1370000%04d", i), true)
		dbtest.Fund(t, f.pool, users[i], 100000)
	}
	productID, imageID := dbtest.ProductID(t, f.pool), dbtest.ImageID(t, f.pool)
	var price int64
	if err := f.pool.QueryRow(f.ctx, `SELECT amount FROM prices WHERE product_id = $1 AND duration = '1d'`, productID).Scan(&price); err != nil {
		t.Fatal(err)
	}
	type result struct {
		uid int64
		err error
	}
	var wg sync.WaitGroup
	start := make(chan struct{}) // 50 个协程就位后一起下单
	results := make(chan result, 50)
	for _, uid := range users {
		wg.Add(1)
		go func(uid int64) {
			defer wg.Done()
			<-start
			_, _, err := f.a.Rental.Place(f.ctx, rental.PlaceRequest{UserID: uid, ProductID: productID, ImageID: imageID, Duration: catalog.D1d})
			results <- result{uid, err}
		}(uid)
	}
	close(start)
	wg.Wait()
	close(results)
	won := map[int64]bool{}
	capCount := 0
	for r := range results {
		switch {
		case r.err == nil:
			won[r.uid] = true
		case apperr.As(r.err) != nil && apperr.As(r.err).Code == apperr.ErrNoCapacity.Code:
			capCount++
		default:
			t.Fatalf("unexpected error: %v", r.err)
		}
	}
	if len(won) != 10 || capCount != 40 {
		t.Fatalf("ok=%d no_capacity=%d", len(won), capCount)
	}
	// 资金：赢家恰好扣一次价格，落空的余额不动；订单与支付流水各 10 条
	for _, uid := range users {
		var balance int64
		if err := f.pool.QueryRow(f.ctx, `SELECT balance FROM wallet_accounts WHERE user_id = $1`, uid).Scan(&balance); err != nil {
			t.Fatal(err)
		}
		want := int64(100000)
		if won[uid] {
			want -= price
		}
		if balance != want {
			t.Fatalf("user %d (won=%v) balance = %d, want %d", uid, won[uid], balance, want)
		}
	}
	var orders, pays int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM orders`).Scan(&orders)
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM wallet_ledger WHERE kind = 'order_pay'`).Scan(&pays)
	if orders != 10 || pays != 10 {
		t.Fatalf("orders = %d, order_pay ledger rows = %d", orders, pays)
	}
	f.waitFor("all running", func() bool {
		var n int
		_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM instances WHERE state = 'running'`).Scan(&n)
		return n == 10
	})
	// 每台机器恰好一个实例、一条开通指令，且由该机器自己的 Agent 收到
	rows, err := f.pool.Query(f.ctx, `SELECT machine_id, (array_agg(instance_id))[1], count(*) FROM agent_commands
		WHERE type = 'create_instance' GROUP BY machine_id`)
	if err != nil {
		t.Fatal(err)
	}
	creates := map[int64]string{}
	for rows.Next() {
		var mid int64
		var inst string
		var n int
		if err := rows.Scan(&mid, &inst, &n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("machine %d got %d create commands", mid, n)
		}
		creates[mid] = inst
	}
	rows.Close()
	if len(creates) != 10 {
		t.Fatalf("create commands spread over %d machines", len(creates))
	}
	for mid, inst := range creates {
		ag, ok := agents[mid]
		if !ok {
			t.Fatalf("create command for unknown machine %d", mid)
		}
		if !ag.Client.Received(f.commandID(inst, "create_instance")) || !ag.Runtime.Containers()[inst].Running {
			t.Fatalf("machine %d agent did not run instance %s", mid, inst)
		}
	}
}
