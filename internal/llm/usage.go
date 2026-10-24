package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/tz"
)

// 用量明细的查询窗口（规格 §10.1：默认最近 7 天，窗口 ≤ 90 天，与明细保留期一致）。
const (
	UsageDefaultWindow = 7 * 24 * time.Hour
	UsageMaxWindow     = 90 * 24 * time.Hour
)

// ErrDuplicateUsage RecordUsage 遇到已存在的 request_id：整个事务回滚、不重复计费（规格 §14 幂等）。
var ErrDuplicateUsage = errors.New("llm: duplicate usage request_id")

var usageStatuses = []string{UsageOK, UsageClientAbort, UsageUpstreamError, UsageUpstreamTimeout, UsageNoNode}

// Cost 单次请求费用（微分）= prompt × input_price + completion × output_price；embedding 只算输入（规格 §1.1 #4）。
// 价格上限 10^8 分 / 百万、token 数在 int32 内，乘积远小于 int64 上限，不做溢出保护。
func Cost(m Model, promptTokens, completionTokens int) int64 {
	c := int64(promptTokens) * m.InputPrice
	if m.Kind != KindEmbedding {
		c += int64(completionTokens) * m.OutputPrice
	}
	return c
}

// EstimateFen 预算预检的本次估算（分，规格 T2）：ceil((bodyLen/3 × input_price + max_out × output_price) / 10^6)。
// bodyLen/3 用整数除法（经验 token 密度，宁可高估的部分在 max_out）；max_out = min(maxTokens（nil 或 ≤ 0 时取模型
// default_max_tokens）, 模型 max_output_tokens)；embedding 没有输出部分。返回值可能为 0，「至少 1 分」由调用方取 max。
func EstimateFen(m Model, bodyLen int, maxTokens *int) int64 {
	micro := int64(bodyLen/3) * m.InputPrice
	if m.Kind != KindEmbedding {
		out := m.DefaultMaxTokens
		if maxTokens != nil && *maxTokens > 0 {
			out = *maxTokens
		}
		out = min(out, m.MaxOutputTokens)
		micro += int64(out) * m.OutputPrice
	}
	return ceilFen(micro)
}

// ceilFen 微分 → 分，向上取整（micro ≥ 0）。
func ceilFen(micro int64) int64 { return (micro + MicroPerFen - 1) / MicroPerFen }

// floorFen 微分 → 分，向下取整（与结算 floor(Σ / 10^6) 同口径）。
func floorFen(micro int64) int64 { return micro / MicroPerFen }

func validateUsage(u Usage) error {
	switch {
	case u.RequestID == "":
		return errors.New("llm: usage request_id is empty")
	case u.UserID <= 0 || u.KeyID <= 0 || u.ModelID <= 0:
		return fmt.Errorf("llm: usage %s: user / key / model id required", u.RequestID)
	case u.PromptTokens < 0 || u.CompletionTokens < 0 || u.CostMicro < 0 || u.InputPrice < 0 || u.OutputPrice < 0:
		return fmt.Errorf("llm: usage %s: negative tokens / price / cost", u.RequestID)
	case !slices.Contains(usageStatuses, u.Status):
		return fmt.Errorf("llm: usage %s: unknown status %q", u.RequestID, u.Status)
	}
	return nil
}

// RecordUsage 网关记账（规格 §6.1 第 11 步，一个事务，加锁顺序 §14）：
// llm_accounts(user) FOR UPDATE（无则插）→ 插 llm_usage → unsettled_micro += cost_micro → llm_api_keys 更新
// last_used_at（只前进）与 month_cost_micro（按 CreatedAt 的北京时间月份：month_ym 是更早的月份则重置后累加；
// 迟到的上月用量不计入已翻月的 Key 月计数）。
//
// status ≠ ok 且非 client_abort 的行 cost_micro 一律记 0（规格 §6.4）。request_id 已存在 → ErrDuplicateUsage、不改任何数据。
// 不使 Authorize 缓存失效：网关缓存的 month_cost_micro / unsettled 最多落后 KeyCacheTTL（月上限与预检本来就允许被并发打穿，
// 欠费兜底）。llm_usage_daily 只由结算写。
func (s *Service) RecordUsage(ctx context.Context, u Usage) error {
	if err := validateUsage(u); err != nil {
		return err
	}
	now := s.d.Clock.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	if u.Status != UsageOK && u.Status != UsageClientAbort {
		u.CostMicro = 0
	}
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := s.EnsureAccount(ctx, tx, u.UserID); err != nil {
			return err
		}
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO llm_usage (request_id, user_id, key_id, model_id, node_id, endpoint, stream,
				prompt_tokens, completion_tokens, estimated, input_price, output_price, cost_micro, status, http_status,
				latency_ms, ttft_ms, client_ip, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
			ON CONFLICT (request_id) DO NOTHING RETURNING id`,
			u.RequestID, u.UserID, u.KeyID, u.ModelID, u.NodeID, u.Endpoint, u.Stream,
			u.PromptTokens, u.CompletionTokens, u.Estimated, u.InputPrice, u.OutputPrice, u.CostMicro, u.Status, u.HTTPStatus,
			u.LatencyMS, u.TTFTMS, u.ClientIP, u.CreatedAt).Scan(&id)
		if db.IsNoRows(err) {
			return ErrDuplicateUsage
		}
		if err != nil {
			return err
		}
		if u.CostMicro > 0 {
			if _, err := tx.Exec(ctx, `UPDATE llm_accounts SET unsettled_micro = unsettled_micro + $2, updated_at = $3 WHERE user_id = $1`,
				u.UserID, u.CostMicro, now); err != nil {
				return err
			}
		}
		ym := MonthOf(u.CreatedAt)
		tag, err := tx.Exec(ctx, `UPDATE llm_api_keys SET last_used_at = GREATEST(last_used_at, $3),
				month_cost_micro = CASE WHEN month_ym = $4 THEN month_cost_micro + $5
					WHEN month_ym < $4 THEN $5 ELSE month_cost_micro END,
				month_ym = GREATEST(month_ym, $4)
			WHERE id = $1 AND user_id = $2`, u.KeyID, u.UserID, u.CreatedAt, ym, u.CostMicro)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("llm: usage %s: key %d does not belong to user %d", u.RequestID, u.KeyID, u.UserID)
		}
		return nil
	})
}

// UsageFilter 用量明细筛选（控制台 / 后台共用）。零值字段不过滤：UserID 0 = 全部用户（后台）；
// Model 按 slug（控制台 ?model=），ModelID 按 ID（后台 ?model_id=）；时间 [From, To)。窗口默认值 / 上限见 UsageWindow（接口层调用）。
type UsageFilter struct {
	UserID  int64
	ModelID int64
	Model   string
	KeyID   int64
	Status  string
	From    *time.Time
	To      *time.Time
}

// UsageRow 明细行 = llm_usage 一行 + 联表的模型 slug、Key 名称 / hint、用户手机号（后台用；控制台视图自行裁剪）。
type UsageRow struct {
	Usage
	Model     string `json:"model"`
	KeyName   string `json:"key_name"`
	KeyHint   string `json:"key_hint"`
	UserPhone string `json:"user_phone"`
}

// UsageWindow 解析明细查询窗口：to 缺省 = now，from 缺省 = to − 7 天；from ≥ to 或跨度 > 90 天 → bad_request。
func UsageWindow(from, to *time.Time, now time.Time) (time.Time, time.Time, error) {
	t := now
	if to != nil {
		t = *to
	}
	f := t.Add(-UsageDefaultWindow)
	if from != nil {
		f = *from
	}
	if !f.Before(t) {
		return time.Time{}, time.Time{}, apperr.BadRequest("开始时间须早于结束时间")
	}
	if t.Sub(f) > UsageMaxWindow {
		return time.Time{}, time.Time{}, apperr.BadRequest("查询时间范围最长 90 天")
	}
	return f, t, nil
}

func (f UsageFilter) where() (db.Where, error) {
	var w db.Where
	if f.UserID > 0 {
		w.Add("g.user_id = ?", f.UserID)
	}
	if f.ModelID > 0 {
		w.Add("g.model_id = ?", f.ModelID)
	}
	if f.Model != "" {
		w.Add("m.slug = ?", f.Model)
	}
	if f.KeyID > 0 {
		w.Add("g.key_id = ?", f.KeyID)
	}
	if f.Status != "" {
		if !slices.Contains(usageStatuses, f.Status) {
			return w, apperr.BadRequest("用量状态不正确")
		}
		w.Add("g.status = ?", f.Status)
	}
	if f.From != nil {
		w.Add("g.created_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("g.created_at < ?", *f.To)
	}
	return w, nil
}

const usageRowFrom = ` FROM llm_usage g
	JOIN llm_models m ON m.id = g.model_id
	JOIN llm_api_keys k ON k.id = g.key_id
	JOIN users u ON u.id = g.user_id`

const usageRowCols = `g.id, g.request_id, g.user_id, g.key_id, g.model_id, g.node_id, g.endpoint, g.stream, g.prompt_tokens,
	g.completion_tokens, g.estimated, g.input_price, g.output_price, g.cost_micro, g.status, g.http_status, g.latency_ms, g.ttft_ms,
	g.client_ip, g.settlement_id, g.created_at, m.slug, k.name, k.key_hint, u.phone`

func scanUsageRow(row pgx.Row) (UsageRow, error) {
	var r UsageRow
	u := &r.Usage
	err := row.Scan(&u.ID, &u.RequestID, &u.UserID, &u.KeyID, &u.ModelID, &u.NodeID, &u.Endpoint, &u.Stream, &u.PromptTokens,
		&u.CompletionTokens, &u.Estimated, &u.InputPrice, &u.OutputPrice, &u.CostMicro, &u.Status, &u.HTTPStatus, &u.LatencyMS, &u.TTFTMS,
		&u.ClientIP, &u.SettlementID, &u.CreatedAt, &r.Model, &r.KeyName, &r.KeyHint, &r.UserPhone)
	return r, err
}

// ListUsage 用量明细分页，新的在前（created_at DESC, id DESC）。返回本页与总数。
func (s *Service) ListUsage(ctx context.Context, f UsageFilter, p paging.Page) ([]UsageRow, int, error) {
	w, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*)`+usageRowFrom+` WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.d.Pool.Query(ctx, `SELECT `+usageRowCols+usageRowFrom+` WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY g.created_at DESC, g.id DESC LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (UsageRow, error) { return scanUsageRow(r) })
	if list == nil && err == nil {
		list = []UsageRow{}
	}
	return list, total, err
}

// ExportUsage 后台 CSV：按 id 正序逐行回调；超过 csvx.MaxRows 时在回调前返回 csvx.ErrTooLarge（规格 T18）。
func (s *Service) ExportUsage(ctx context.Context, f UsageFilter, fn func(UsageRow) error) error {
	w, err := f.where()
	if err != nil {
		return err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*)`+usageRowFrom+` WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return err
	}
	if total > csvx.MaxRows {
		return csvx.ErrTooLarge
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT `+usageRowCols+usageRowFrom+` WHERE `+w.SQL()+` ORDER BY g.id`, w.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanUsageRow(rows)
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// cstDate 北京时间日期字符串（与 llm_usage_daily.day 比较时 ::date）。
func cstDate(t time.Time) string { return t.In(tz.CST).Format("2006-01-02") }

// monthStart 北京时间当月 1 日 00:00（UTC）。
func monthStart(t time.Time) time.Time {
	l := t.In(tz.CST)
	return time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, tz.CST).UTC()
}

// DailyUsage 按天 × 用户 × 模型汇总，日期升序；from、to 所在的北京时间自然日都包含在内。
// 每天 = llm_usage_daily（已结算）+ 当天仍未结算的 llm_usage（Asia/Shanghai 日界），所以今天的数据不必等结算。
// userID 0 = 全部用户（后台；按模型 / 用户再汇总由接口层做）。ModelSlug 联表带出。
func (s *Service) DailyUsage(ctx context.Context, userID int64, from, to time.Time) ([]DailyRow, error) {
	rows, err := s.d.Pool.Query(ctx, `WITH x AS (
			SELECT day, user_id, model_id, requests, prompt_tokens, completion_tokens, cost_micro
			FROM llm_usage_daily
			WHERE day BETWEEN $1::date AND $2::date AND ($3::bigint = 0 OR user_id = $3)
			UNION ALL
			SELECT (created_at AT TIME ZONE 'Asia/Shanghai')::date, user_id, model_id, 1, prompt_tokens, completion_tokens, cost_micro
			FROM llm_usage
			WHERE settlement_id IS NULL AND created_at >= $4 AND created_at < $5 AND ($3::bigint = 0 OR user_id = $3))
		SELECT x.day, x.user_id, x.model_id, m.slug, sum(x.requests)::bigint, sum(x.prompt_tokens)::bigint,
			sum(x.completion_tokens)::bigint, sum(x.cost_micro)::bigint
		FROM x JOIN llm_models m ON m.id = x.model_id
		GROUP BY x.day, x.user_id, x.model_id, m.slug
		ORDER BY x.day, x.user_id, x.model_id`,
		cstDate(from), cstDate(to), userID, tz.DayStart(from), tz.DayStart(to).Add(24*time.Hour))
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DailyRow, error) {
		var d DailyRow
		err := r.Scan(&d.Day, &d.UserID, &d.ModelID, &d.ModelSlug, &d.Requests, &d.PromptTokens, &d.CompletionTokens, &d.CostMicro)
		return d, err
	})
	if list == nil && err == nil {
		list = []DailyRow{}
	}
	return list, err
}
