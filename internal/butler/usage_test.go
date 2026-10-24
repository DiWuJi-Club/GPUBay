package butler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/tz"
)

func pagingAll() paging.Page { return paging.Page{Number: 1, Size: 100} }

// cst 构造北京时间时刻（返回 UTC）。
func cst(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, tz.CST).UTC()
}

func TestAddUsageUpsert(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, s.pool, "13800000001", true)
	day := s.DayOf(s.clk.Now())
	if err := s.AddUsage(ctx, s.pool, uid, day, Usage{Requests: 1, InputTokens: 100, OutputTokens: 20, ToolCalls: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, s.pool, uid, day, Usage{UserID: 999, Requests: 2, InputTokens: 50, OutputTokens: 5, ToolCalls: 1, Failures: 1}); err != nil {
		t.Fatal(err)
	}
	// 事务里累加后回滚：不生效（AddUsage 用的是传入的 Querier）
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if err := s.AddUsage(ctx, tx, uid, day, Usage{Requests: 100, InputTokens: 100000}); err != nil {
			return err
		}
		return context.Canceled
	})
	if err != context.Canceled {
		t.Fatalf("withTx err = %v", err)
	}
	u, err := s.UsageToday(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if u.UserID != uid || !u.Day.Equal(day) || u.Requests != 3 || u.InputTokens != 150 || u.OutputTokens != 25 || u.ToolCalls != 3 || u.Failures != 1 {
		t.Fatalf("usage = %+v", u)
	}
	var rows int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM butler_usage`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d, %v", rows, err)
	}
	// 没有记录的用户：全零
	other := dbtest.CreateUser(t, s.pool, "13800000002", true)
	if u, err := s.UsageToday(ctx, other); err != nil || u.Requests != 0 || u.InputTokens != 0 || !u.Day.Equal(day) {
		t.Fatalf("empty usage = %+v, %v", u, err)
	}
}

func TestDayBoundaryCST(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, s.pool, "13800000001", true)
	// UTC 2026-03-01 15:59 = 北京时间 3 月 1 日 23:59；UTC 16:00 = 北京时间 3 月 2 日 00:00
	late := time.Date(2026, 3, 1, 15, 59, 0, 0, time.UTC)
	clk.Advance(late.Sub(clk.Now()))
	d1 := s.DayOf(clk.Now())
	if !d1.Equal(time.Date(2026, 2, 28, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("DayOf(15:59Z) = %v", d1)
	}
	if err := s.AddUsage(ctx, s.pool, uid, d1, Usage{Requests: 1, InputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	d2 := s.DayOf(clk.Now())
	if !d2.Equal(time.Date(2026, 3, 1, 16, 0, 0, 0, time.UTC)) || d2.Equal(d1) {
		t.Fatalf("DayOf(16:00Z) = %v", d2)
	}
	if u, _ := s.UsageToday(ctx, uid); u.Requests != 0 {
		t.Fatalf("new CST day must start at zero, got %+v", u)
	}
	if err := s.AddUsage(ctx, s.pool, uid, d2, Usage{Requests: 2, InputTokens: 20}); err != nil {
		t.Fatal(err)
	}
	// DATE 列存的是北京时间的日历日期
	rows, err := s.pool.Query(ctx, `SELECT day::text, requests FROM butler_usage ORDER BY day`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		Day string
		Req int
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (row{"2026-03-01", 1}) || got[1] != (row{"2026-03-02", 2}) {
		t.Fatalf("stored days = %+v", got)
	}
	// 读回的 Day 是该日北京时间 00:00
	days, err := s.UsageByDay(ctx, d1, d2)
	if err != nil || len(days) != 2 || !days[0].Day.Equal(d1) || !days[1].Day.Equal(d2) {
		t.Fatalf("UsageByDay = %+v, %v", days, err)
	}
}

// TestDailyBudget 即计划 Task 4 的 TestOverBudget（Review Focus 4：当日用量 ≥ 限额即超额，失败不计）。
func TestDailyBudget(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, s.pool, "13800000001", true)
	day := s.DayOf(s.clk.Now())
	if used, over, err := s.OverBudget(ctx, uid, 1000); err != nil || used != 0 || over {
		t.Fatalf("empty = %d %v %v", used, over, err)
	}
	if err := s.AddUsage(ctx, s.pool, uid, day, Usage{Requests: 1, InputTokens: 600, OutputTokens: 399}); err != nil {
		t.Fatal(err)
	}
	// 失败次数不计预算
	if err := s.AddUsage(ctx, s.pool, uid, day, Usage{Failures: 5}); err != nil {
		t.Fatal(err)
	}
	if used, over, err := s.OverBudget(ctx, uid, 1000); err != nil || used != 999 || over {
		t.Fatalf("999 = %d %v %v", used, over, err)
	}
	if err := s.AddUsage(ctx, s.pool, uid, day, Usage{OutputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if used, over, err := s.OverBudget(ctx, uid, 1000); err != nil || used != 1000 || !over {
		t.Fatalf("1000 = %d %v %v", used, over, err)
	}
	// 昨天的用量不算今天
	other := dbtest.CreateUser(t, s.pool, "13800000002", true)
	if err := s.AddUsage(ctx, s.pool, other, day.Add(-24*time.Hour), Usage{InputTokens: 5000}); err != nil {
		t.Fatal(err)
	}
	if used, over, err := s.OverBudget(ctx, other, 1000); err != nil || used != 0 || over {
		t.Fatalf("yesterday counted: %d %v %v", used, over, err)
	}
}

func TestUsageByDayUsersCount(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	u1 := dbtest.CreateUser(t, s.pool, "13800000001", true)
	u2 := dbtest.CreateUser(t, s.pool, "13800000002", true)
	u3 := dbtest.CreateUser(t, s.pool, "13800000003", true)
	d1 := cst(2026, 3, 1, 0, 0)
	d2 := cst(2026, 3, 2, 0, 0)
	d3 := cst(2026, 3, 3, 0, 0)
	add := func(uid int64, day time.Time, u Usage) {
		t.Helper()
		if err := s.AddUsage(ctx, s.pool, uid, day, u); err != nil {
			t.Fatal(err)
		}
	}
	add(u1, d1, Usage{Requests: 1, InputTokens: 100, OutputTokens: 10, ToolCalls: 1})
	add(u2, d1, Usage{Requests: 2, InputTokens: 200, OutputTokens: 20, Failures: 1})
	add(u1, d2, Usage{Requests: 1, InputTokens: 1, OutputTokens: 1})
	add(u2, d2, Usage{Requests: 1, InputTokens: 300, OutputTokens: 30, ToolCalls: 4})
	add(u3, d2, Usage{Requests: 3, InputTokens: 50, OutputTokens: 5})
	add(u3, d3, Usage{Requests: 9, InputTokens: 9})

	// from / to 取所在自然日、两端都含：d1 当日 12:00 ～ d2 当日 23:59
	days, err := s.UsageByDay(ctx, d1.Add(12*time.Hour), d2.Add(24*time.Hour-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := []DailyUsage{
		{Day: d1, Requests: 3, InputTokens: 300, OutputTokens: 30, ToolCalls: 1, Failures: 1, Users: 2},
		{Day: d2, Requests: 5, InputTokens: 351, OutputTokens: 36, ToolCalls: 4, Failures: 0, Users: 3},
	}
	if len(days) != len(want) {
		t.Fatalf("days = %+v", days)
	}
	for i := range want {
		if days[i] != want[i] {
			t.Errorf("day %d = %+v, want %+v", i, days[i], want[i])
		}
	}
	if empty, err := s.UsageByDay(ctx, cst(2025, 1, 1, 0, 0), cst(2025, 1, 2, 0, 0)); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty range = %#v, %v", empty, err)
	}

	// 按用户 Top：token 降序、分页、带手机号
	users, total, err := s.UsageByUser(ctx, d2.Add(5*time.Hour), paging.Page{Number: 1, Size: 2})
	if err != nil || total != 3 || len(users) != 2 {
		t.Fatalf("UsageByUser = %+v, %d, %v", users, total, err)
	}
	if users[0].UserID != u2 || users[0].Phone != "13800000002" || users[0].InputTokens != 300 || users[0].OutputTokens != 30 || users[0].ToolCalls != 4 ||
		users[1].UserID != u3 || users[1].Requests != 3 {
		t.Fatalf("top users = %+v", users)
	}
	page2, total, err := s.UsageByUser(ctx, d2, paging.Page{Number: 2, Size: 2})
	if err != nil || total != 3 || len(page2) != 1 || page2[0].UserID != u1 {
		t.Fatalf("page 2 = %+v, %d, %v", page2, total, err)
	}
}

func TestPruneUsage(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, s.pool, "13800000001", true)
	for _, d := range []int{1, 2, 3, 4} {
		if err := s.AddUsage(ctx, s.pool, uid, cst(2026, 3, d, 0, 0), Usage{Requests: d}); err != nil {
			t.Fatal(err)
		}
	}
	// before = 3 月 3 日北京时间 07:00（UTC 3 月 2 日 23:00）：删 1、2 日，保留 3、4 日
	n, err := s.PruneUsage(ctx, cst(2026, 3, 3, 7, 0))
	if err != nil || n != 2 {
		t.Fatalf("pruned = %d, %v", n, err)
	}
	days, err := s.UsageByDay(ctx, cst(2026, 1, 1, 0, 0), cst(2026, 12, 31, 0, 0))
	if err != nil || len(days) != 2 || days[0].Requests != 3 || days[1].Requests != 4 {
		t.Fatalf("remaining = %+v, %v", days, err)
	}
	if n, err := s.PruneUsage(ctx, cst(2026, 3, 3, 0, 0)); err != nil || n != 0 {
		t.Fatalf("second prune = %d, %v", n, err)
	}
}

func TestUserButlerStats(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, s.pool, "13800000001", true)
	other := dbtest.CreateUser(t, s.pool, "13800000002", true)
	now := clk.Now()
	since := now.Add(-30 * 24 * time.Hour)
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		id  string
		uid int64
	}{{"cv-a", uid}, {"cv-b", uid}, {"cv-c", other}} {
		mustExec(`INSERT INTO butler_conversations (id, user_id, provider, model, created_at, updated_at) VALUES ($1, $2, 'mock', 'mock', $3, $3)`, c.id, c.uid, now)
	}
	var msgA, msgC int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO butler_messages (conversation_id, seq, role, content, created_at) VALUES ('cv-a', 1, 'assistant', '[]', $1) RETURNING id`, now).Scan(&msgA); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `INSERT INTO butler_messages (conversation_id, seq, role, content, created_at) VALUES ('cv-c', 1, 'assistant', '[]', $1) RETURNING id`, now).Scan(&msgC); err != nil {
		t.Fatal(err)
	}
	tc := func(id, conv string, msg, user int64, kind, status string, at time.Time) {
		t.Helper()
		mustExec(`INSERT INTO butler_tool_calls (id, conversation_id, message_id, user_id, provider_call_id, tool, kind, args, status, created_at)
			VALUES ($1, $2, $3, $4, 'call', 'renew_instance', $5, '{}', $6, $7)`, id, conv, msg, user, kind, status, at)
	}
	tc("tc-1", "cv-a", msgA, uid, KindWrite, CallExecuted, now)
	tc("tc-2", "cv-a", msgA, uid, KindWrite, CallFailed, now.Add(-time.Hour))
	tc("tc-3", "cv-a", msgA, uid, KindWrite, CallRejected, now)                   // 未执行：不计
	tc("tc-4", "cv-a", msgA, uid, KindWrite, CallExpired, now)                    // 未执行：不计
	tc("tc-5", "cv-a", msgA, uid, KindRead, CallExecuted, now)                    // 读工具：不计
	tc("tc-6", "cv-a", msgA, uid, KindWrite, CallExecuted, since.Add(-time.Hour)) // 早于 since：不计
	tc("tc-7", "cv-c", msgC, other, KindWrite, CallExecuted, now)                 // 他人：不计

	if err := s.AddUsage(ctx, s.pool, uid, s.DayOf(now), Usage{InputTokens: 100, OutputTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, s.pool, uid, s.DayOf(since), Usage{InputTokens: 1000}); err != nil { // since 所在当日：计入
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, s.pool, uid, s.DayOf(since).Add(-24*time.Hour), Usage{InputTokens: 50000}); err != nil { // 更早：不计
		t.Fatal(err)
	}
	if err := s.AddUsage(ctx, s.pool, other, s.DayOf(now), Usage{InputTokens: 7777}); err != nil {
		t.Fatal(err)
	}
	st, err := s.UserButlerStats(ctx, uid, since)
	if err != nil {
		t.Fatal(err)
	}
	if st != (UserStats{Conversations: 2, Tokens30d: 1120, WriteCalls30d: 2}) {
		t.Fatalf("stats = %+v", st)
	}
	empty := dbtest.CreateUser(t, s.pool, "13800000003", true)
	if st, err := s.UserButlerStats(ctx, empty, since); err != nil || st != (UserStats{}) {
		t.Fatalf("empty stats = %+v, %v", st, err)
	}
}
