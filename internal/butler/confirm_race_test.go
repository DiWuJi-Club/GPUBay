package butler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/butler/butlertest"
	"spark-cloud/internal/platform/apperr"
)

// 跨副本双击确认（Review Focus 2）：两个 Service（各自的进程内运行名额互不相见）共用一个库，同时确认同一张卡。
// 测试先在另一个事务里锁住对话行，让两个 Confirm 都通过只读预检、都拿到本副本的运行名额，再一起排队等行锁；
// 放开后只有一个执行（renew 恰调一次、审计一条），另一个看到 approved / running → butler_confirm_invalid。
func TestConfirmRaceAcrossReplicas(t *testing.T) {
	e := newEnv(t)
	c, cf := e.pendingRenew()
	replicaB := NewService(e.svc.d)
	ctx := context.Background()

	hold, err := e.svc.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx) //nolint:errcheck // 已提交时为 no-op
	if _, err := hold.Exec(ctx, `SELECT 1 FROM butler_conversations WHERE id = $1 FOR UPDATE`, c.ID); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		conv Conversation
		sink *sliceSink
		err  error
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	for i, svc := range []*Service{e.svc, replicaB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink := &sliceSink{}
			conv, err := svc.Confirm(ctx, e.owner, "1.2.3.4", c.ID, cf.ID, true, sink)
			results[i] = outcome{conv, sink, err}
		}()
	}
	// 两个 Confirm 都在等对话行锁（都已过预检）
	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()`) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("confirms never queued on the conversation lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	ok, invalid := 0, 0
	for _, r := range results {
		switch {
		case r.err == nil:
			ok++
			if r.conv.Status != ConvIdle || len(r.sink.find(EvTool)) != 1 {
				t.Fatalf("winner = %s events %v", r.conv.Status, r.sink.names())
			}
		case errors.Is(r.err, apperr.ErrButlerConfirmInvalid):
			invalid++
			if len(r.sink.names()) != 0 {
				t.Fatalf("loser emitted %v", r.sink.names())
			}
		default:
			t.Fatalf("unexpected error %v", r.err)
		}
	}
	if ok != 1 || invalid != 1 {
		t.Fatalf("ok %d invalid %d, want 1 / 1", ok, invalid)
	}
	if n := e.rental.renewN(); n != 1 {
		t.Fatalf("renew executed %d times", n)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'butler.tool_execute' AND target_id = $1`, cf.ID); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM butler_tool_calls WHERE id = $1 AND status = 'executed'`, cf.ID); n != 1 {
		t.Fatal("call not executed exactly once")
	}
	butlertest.CheckInvariants(t, e.svc.pool)
}
