package butler

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/tz"
)

// DailyUsage 是某个东八区自然日的全站合计（后台用量总览）。
type DailyUsage struct {
	Day          time.Time // 该日北京时间 00:00（UTC 表示）
	Requests     int
	InputTokens  int64
	OutputTokens int64
	ToolCalls    int
	Failures     int
	Users        int // 当日有用量记录的用户数
}

// UserUsage 是某日单个用户的用量（后台用量页按用户 Top）。Phone 为完整手机号，脱敏由展示层决定。
type UserUsage struct {
	UserID       int64
	Phone        string
	Requests     int
	InputTokens  int64
	OutputTokens int64
	ToolCalls    int
	Failures     int
}

// UserStats 是后台用户详情里的管家统计。
type UserStats struct {
	Conversations int   // 现存对话数（不限时间；超过保留期的已被清扫）
	Tokens30d     int64 // since 所在自然日起的 input + output token
	WriteCalls30d int   // since 起用户确认执行过的写操作数（approved / executed / failed）
}

// DayOf 返回 t 所在东八区自然日的 00:00（UTC 表示），与 tz.DayStart 相同。
func (s *Store) DayOf(t time.Time) time.Time { return tz.DayStart(t) }

// dateParam 把时刻转成它在北京时间的日历日期 "2006-01-02"，SQL 里用 $n::date。
// 不能直接把 time.Time 传给 DATE 参数：pgx 取的是该值自身时区的年月日，
// 而 tz.DayStart 的结果是前一天 16:00 UTC，会差一天。
func dateParam(t time.Time) string { return t.In(tz.CST).Format("2006-01-02") }

// fromDate 把从 DATE 列读出的值（UTC 零点）转回该日北京时间 00:00 的 UTC 时刻。
func fromDate(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, tz.CST).UTC()
}

// UsageToday 返回用户今天（东八区）的用量；没有记录时返回全零。
func (s *Store) UsageToday(ctx context.Context, userID int64) (Usage, error) {
	day := s.DayOf(s.clk.Now())
	u := Usage{UserID: userID, Day: day}
	err := s.pool.QueryRow(ctx, `SELECT requests, input_tokens, output_tokens, tool_calls, failures
		FROM butler_usage WHERE user_id = $1 AND day = $2::date`, userID, dateParam(day)).
		Scan(&u.Requests, &u.InputTokens, &u.OutputTokens, &u.ToolCalls, &u.Failures)
	if db.IsNoRows(err) {
		return u, nil
	}
	return u, err
}

// AddUsage 把 d 的各计数累加到 (userID, day 所在自然日)；q 可以是事务，与轮次回写同事务提交。d.UserID / d.Day 被忽略。
func (s *Store) AddUsage(ctx context.Context, q db.Querier, userID int64, day time.Time, d Usage) error {
	_, err := q.Exec(ctx, `INSERT INTO butler_usage (user_id, day, requests, input_tokens, output_tokens, tool_calls, failures)
		VALUES ($1, $2::date, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, day) DO UPDATE SET
			requests = butler_usage.requests + EXCLUDED.requests,
			input_tokens = butler_usage.input_tokens + EXCLUDED.input_tokens,
			output_tokens = butler_usage.output_tokens + EXCLUDED.output_tokens,
			tool_calls = butler_usage.tool_calls + EXCLUDED.tool_calls,
			failures = butler_usage.failures + EXCLUDED.failures`,
		userID, dateParam(day), d.Requests, d.InputTokens, d.OutputTokens, d.ToolCalls, d.Failures)
	return err
}

// OverBudget 返回用户今天已用 token（input + output）及是否已达限额（used ≥ limit）；失败次数不计预算。
func (s *Store) OverBudget(ctx context.Context, userID int64, limit int64) (used int64, over bool, err error) {
	u, err := s.UsageToday(ctx, userID)
	if err != nil {
		return 0, false, err
	}
	used = u.InputTokens + u.OutputTokens
	return used, used >= limit, nil
}

// UsageByDay 返回 from 与 to 所在自然日之间（两端都含）每个有记录的日期的合计，按日期升序。
func (s *Store) UsageByDay(ctx context.Context, from, to time.Time) ([]DailyUsage, error) {
	rows, err := s.pool.Query(ctx, `SELECT day, sum(requests), sum(input_tokens), sum(output_tokens), sum(tool_calls), sum(failures), count(DISTINCT user_id)
		FROM butler_usage WHERE day >= $1::date AND day <= $2::date
		GROUP BY day ORDER BY day`, dateParam(from), dateParam(to))
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DailyUsage, error) {
		var d DailyUsage
		var day time.Time
		err := r.Scan(&day, &d.Requests, &d.InputTokens, &d.OutputTokens, &d.ToolCalls, &d.Failures, &d.Users)
		d.Day = fromDate(day)
		return d, err
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []DailyUsage{}
	}
	return out, nil
}

// UsageByUser 返回 day 所在自然日按 token（input + output）降序的用户用量分页，以及当日有记录的用户总数。
func (s *Store) UsageByUser(ctx context.Context, day time.Time, p paging.Page) ([]UserUsage, int, error) {
	p = p.Normalize()
	d := dateParam(day)
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM butler_usage WHERE day = $1::date`, d).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT u.user_id, us.phone, u.requests, u.input_tokens, u.output_tokens, u.tool_calls, u.failures
		FROM butler_usage u JOIN users us ON us.id = u.user_id
		WHERE u.day = $1::date
		ORDER BY u.input_tokens + u.output_tokens DESC, u.user_id
		LIMIT $2 OFFSET $3`, d, p.Limit(), p.Offset())
	if err != nil {
		return nil, 0, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (UserUsage, error) {
		var x UserUsage
		err := r.Scan(&x.UserID, &x.Phone, &x.Requests, &x.InputTokens, &x.OutputTokens, &x.ToolCalls, &x.Failures)
		return x, err
	})
	if err != nil {
		return nil, 0, err
	}
	if out == nil {
		out = []UserUsage{}
	}
	return out, total, nil
}

// UserButlerStats 返回用户的管家统计：现存对话数；since 所在自然日起的 token；since 起确认执行过的写操作数。
func (s *Store) UserButlerStats(ctx context.Context, userID int64, since time.Time) (UserStats, error) {
	var st UserStats
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM butler_conversations WHERE user_id = $1),
		(SELECT coalesce(sum(input_tokens + output_tokens), 0) FROM butler_usage WHERE user_id = $1 AND day >= $2::date),
		(SELECT count(*) FROM butler_tool_calls WHERE user_id = $1 AND kind = 'write'
			AND status IN ('approved', 'executed', 'failed') AND created_at >= $3)`,
		userID, dateParam(since), since).Scan(&st.Conversations, &st.Tokens30d, &st.WriteCalls30d)
	return st, err
}

// PruneUsage 删除 before 所在自然日之前（不含该日）的用量记录，返回删除行数。
func (s *Store) PruneUsage(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM butler_usage WHERE day < $1::date`, dateParam(before))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
