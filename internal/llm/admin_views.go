package llm

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/tz"
)

// 后台（规格 §10.2）的只读汇总视图：模型节点计数、按天汇总、仪表盘卡片、用户详情的 Token 商城概况、Key 视图。

// NodeCounts 某模型的节点计数：Ready = ready 状态的节点数；Total = 非终态（不是 removed / failed）节点数。
type NodeCounts struct {
	Ready int `json:"ready"`
	Total int `json:"total"`
}

// AdminModelView 后台 GET /llm/models 的一行：模型全部字段 + 节点计数。
type AdminModelView struct {
	Model
	Nodes NodeCounts `json:"nodes"`
}

// AdminListModels 后台模型列表（全部状态，顺序同 ListModels）+ 每个模型的节点计数。
func (s *Service) AdminListModels(ctx context.Context) ([]AdminModelView, error) {
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT model_id, count(*) FILTER (WHERE state = $1)::int, count(*)::int
		FROM llm_nodes WHERE state <> ALL($2) GROUP BY model_id`, NodeReady, NodeTerminalStates)
	if err != nil {
		return nil, err
	}
	counts := map[int64]NodeCounts{}
	for rows.Next() {
		var id int64
		var c NodeCounts
		if err := rows.Scan(&id, &c.Ready, &c.Total); err != nil {
			rows.Close()
			return nil, err
		}
		counts[id] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AdminModelView, 0, len(models))
	for _, m := range models {
		out = append(out, AdminModelView{Model: m, Nodes: counts[m.ID]})
	}
	return out, nil
}

// 按天汇总的分组方式（GET /llm/usage/daily?group_by=）。
const (
	DailyByModel = "model"
	DailyByUser  = "user"
)

// AdminDailyRow 后台按天汇总的一行：Day 北京时间 YYYY-MM-DD；按模型时带 Model（slug），按用户时带 UserID / Phone。
type AdminDailyRow struct {
	Day              string `json:"day"`
	Model            string `json:"model,omitempty"`
	UserID           int64  `json:"user_id,omitempty"`
	Phone            string `json:"phone,omitempty"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CostMicro        int64  `json:"cost_micro"`
}

// AdminDailyUsage 全部用户按 (天, 模型) 或 (天, 用户) 汇总，日期升序；from、to 所在的北京时间自然日都包含在内。
// 口径同 DailyUsage：每天 = llm_usage_daily（已结算）+ 当天仍未结算的 llm_usage（usageDaySQL 日界）。groupBy 只能是 model / user。
func (s *Service) AdminDailyUsage(ctx context.Context, from, to time.Time, groupBy string) ([]AdminDailyRow, error) {
	var sel, join, group string
	switch groupBy {
	case DailyByModel:
		sel, join, group = `m.slug, 0::bigint, ''::text`, `JOIN llm_models m ON m.id = x.model_id`, `x.day, m.slug`
	case DailyByUser:
		sel, join, group = `''::text, x.user_id, u.phone`, `JOIN users u ON u.id = x.user_id`, `x.day, x.user_id, u.phone`
	default:
		return nil, apperr.BadRequest("group_by 只能是 model 或 user")
	}
	rows, err := s.d.Pool.Query(ctx, `WITH x AS (
			SELECT day, user_id, model_id, requests, prompt_tokens, completion_tokens, cost_micro
			FROM llm_usage_daily WHERE day BETWEEN $1::date AND $2::date
			UNION ALL
			SELECT `+usageDaySQL+`, user_id, model_id, 1, prompt_tokens, completion_tokens, cost_micro
			FROM llm_usage WHERE settlement_id IS NULL AND created_at >= $3 AND created_at < $4)
		SELECT to_char(x.day, 'YYYY-MM-DD'), `+sel+`, sum(x.requests)::bigint, sum(x.prompt_tokens)::bigint,
			sum(x.completion_tokens)::bigint, sum(x.cost_micro)::bigint
		FROM x `+join+`
		GROUP BY `+group+` ORDER BY `+group,
		cstDate(from), cstDate(to), tz.DayStart(from), tz.DayStart(to).Add(24*time.Hour))
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminDailyRow, error) {
		var d AdminDailyRow
		err := r.Scan(&d.Day, &d.Model, &d.UserID, &d.Phone, &d.Requests, &d.PromptTokens, &d.CompletionTokens, &d.CostMicro)
		return d, err
	})
	if list == nil && err == nil {
		list = []AdminDailyRow{}
	}
	return list, err
}

// SummaryNodes 仪表盘节点计数：Total 非终态节点数，Ready / Unhealthy 为对应状态数。
type SummaryNodes struct {
	Ready     int `json:"ready"`
	Total     int `json:"total"`
	Unhealthy int `json:"unhealthy"`
}

// SummaryToday 北京时间今天：请求 / token（已结算的 llm_usage_daily 今天 + 今天仍未结算的明细）、
// SettledAmount = 今天创建的结算单实扣金额之和（分，即今日收入）、UnsettledEstimate = 今天未结算明细的费用（分，向上取整）。
type SummaryToday struct {
	Requests          int64 `json:"requests"`
	PromptTokens      int64 `json:"prompt_tokens"`
	CompletionTokens  int64 `json:"completion_tokens"`
	SettledAmount     int64 `json:"settled_amount"`
	UnsettledEstimate int64 `json:"unsettled_estimate"`
}

// Summary 后台 GET /llm/summary（仪表盘卡片）。DebtTotal 全部账户欠费之和（分），DebtUsers 欠费 > 0 的用户数。
type Summary struct {
	Nodes     SummaryNodes `json:"nodes"`
	Today     SummaryToday `json:"today"`
	DebtTotal int64        `json:"debt_total"`
	DebtUsers int          `json:"debt_users"`
}

// AdminSummary 仪表盘卡片数据（今天按 Asia/Shanghai）。
func (s *Service) AdminSummary(ctx context.Context) (Summary, error) {
	var out Summary
	now := s.d.Clock.Now()
	ds := tz.DayStart(now)
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = $1)::int, count(*)::int,
			count(*) FILTER (WHERE state = $2)::int
		FROM llm_nodes WHERE state <> ALL($3)`, NodeReady, NodeUnhealthy, NodeTerminalStates).Scan(
		&out.Nodes.Ready, &out.Nodes.Total, &out.Nodes.Unhealthy); err != nil {
		return Summary{}, err
	}
	var unsettledMicro int64
	if err := s.d.Pool.QueryRow(ctx, `WITH d AS (
			SELECT COALESCE(sum(requests), 0) AS req, COALESCE(sum(prompt_tokens), 0) AS pt, COALESCE(sum(completion_tokens), 0) AS ct
			FROM llm_usage_daily WHERE day = $1::date),
		     g AS (
			SELECT count(*) AS req, COALESCE(sum(prompt_tokens), 0) AS pt, COALESCE(sum(completion_tokens), 0) AS ct,
				COALESCE(sum(cost_micro), 0) AS cost
			FROM llm_usage WHERE settlement_id IS NULL AND created_at >= $2 AND created_at < $3)
		SELECT (d.req + g.req)::bigint, (d.pt + g.pt)::bigint, (d.ct + g.ct)::bigint, g.cost::bigint,
			(SELECT COALESCE(sum(charged), 0) FROM llm_settlements WHERE created_at >= $2 AND created_at < $3)::bigint
		FROM d, g`, cstDate(now), ds, ds.Add(24*time.Hour)).Scan(
		&out.Today.Requests, &out.Today.PromptTokens, &out.Today.CompletionTokens, &unsettledMicro, &out.Today.SettledAmount); err != nil {
		return Summary{}, err
	}
	out.Today.UnsettledEstimate = ceilFen(unsettledMicro)
	if err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE(sum(debt), 0)::bigint, count(*) FILTER (WHERE debt > 0)::int FROM llm_accounts`).Scan(
		&out.DebtTotal, &out.DebtUsers); err != nil {
		return Summary{}, err
	}
	return out, nil
}

// UserLLM 后台用户详情的「Token 商城」区块（金额单位分）：Keys = 非 revoked 的 Key 数；MonthCost / Unsettled / Debt / Blocked 口径同 AccountView。
type UserLLM struct {
	Keys      int   `json:"keys"`
	MonthCost int64 `json:"month_cost"`
	Unsettled int64 `json:"unsettled"`
	Debt      int64 `json:"debt"`
	Blocked   bool  `json:"blocked"`
}

// AdminUserLLM 用户的 Token 商城概况（没有账户行时全零，不插行）。用户不存在 → apperr.ErrNotFound。
func (s *Service) AdminUserLLM(ctx context.Context, userID int64) (UserLLM, error) {
	a, err := s.Account(ctx, userID)
	if err != nil {
		return UserLLM{}, err
	}
	out := UserLLM{MonthCost: a.MonthCost, Unsettled: a.Unsettled, Debt: a.Debt, Blocked: a.Blocked}
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*)::int FROM llm_api_keys WHERE user_id = $1 AND status <> $2`,
		userID, KeyRevoked).Scan(&out.Keys); err != nil {
		return UserLLM{}, err
	}
	return out, nil
}

// AdminKeyView 后台 GET /llm/keys 的一行：Key 全部字段（只有 hint）+ 当前（北京时间）月的消费（分，向下取整）。
type AdminKeyView struct {
	Key
	MonthCost int64 `json:"month_cost"`
}

// AdminKeyViews 某用户的全部 Key（含已撤销，新的在前）及本月消费。
func (s *Service) AdminKeyViews(ctx context.Context, userID int64) ([]AdminKeyView, error) {
	keys, err := s.AdminListKeys(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := s.d.Clock.Now()
	out := make([]AdminKeyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, AdminKeyView{Key: k, MonthCost: floorFen(k.CurrentMonthCostMicro(now))})
	}
	return out, nil
}
