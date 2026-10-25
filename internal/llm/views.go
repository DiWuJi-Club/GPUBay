package llm

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// 控制台（/api/v1/llm/*）的视图类型（规格 §10.1）。后台视图在 admin_views.go。
// 控制台视图只给用户自己的数据，且不透出 user_id、节点、client_ip、Key 哈希等内部字段。

// ModelPublicView GET /llm/models 的条目（只含 online 模型；匿名可见）。Available = 至少一个 ready 节点。
type ModelPublicView struct {
	Slug            string   `json:"slug"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Kind            string   `json:"kind"`
	ContextLen      int      `json:"context_len"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	InputPrice      int64    `json:"input_price"`
	OutputPrice     int64    `json:"output_price"`
	Features        []string `json:"features"`
	Available       bool     `json:"available"`
}

// PublicModels 控制台模型目录：online 模型（按 sort, id）+ 是否有 ready 节点（直接数 llm_nodes，不依赖 Router 的内存表）。
func (s *Service) PublicModels(ctx context.Context) ([]ModelPublicView, error) {
	models, err := s.ListOnlineModels(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT DISTINCT n.model_id FROM llm_nodes n JOIN llm_models m ON m.id = n.model_id
		WHERE n.state = $1 AND m.status = $2`, NodeReady, ModelOnline)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	ready := make(map[int64]bool, len(ids))
	for _, id := range ids {
		ready[id] = true
	}
	out := make([]ModelPublicView, 0, len(models))
	for _, m := range models {
		features := m.Features
		if features == nil {
			features = []string{}
		}
		out = append(out, ModelPublicView{
			Slug: m.Slug, Name: m.Name, Description: m.Description, Kind: m.Kind, ContextLen: m.ContextLen,
			MaxOutputTokens: m.MaxOutputTokens, InputPrice: m.InputPrice, OutputPrice: m.OutputPrice,
			Features: features, Available: ready[m.ID],
		})
	}
	return out, nil
}

// KeyView 控制台 Key 条目：只有 hint；MonthCost = 本月（北京时间）已计消费，分，向下取整。
type KeyView struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	KeyHint    string     `json:"key_hint"`
	Status     string     `json:"status"`
	ModelSlugs []string   `json:"model_slugs"`
	MonthlyCap *int64     `json:"monthly_cap"`
	MonthCost  int64      `json:"month_cost"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// NewKeyView 把 Key 裁成控制台视图；now 决定 month_cost 是否属于当月（跨月为 0）。
func NewKeyView(k Key, now time.Time) KeyView {
	slugs := k.ModelSlugs
	if slugs == nil {
		slugs = []string{}
	}
	return KeyView{
		ID: k.ID, Name: k.Name, KeyHint: k.KeyHint, Status: k.Status, ModelSlugs: slugs, MonthlyCap: k.MonthlyCap,
		MonthCost: floorFen(k.CurrentMonthCostMicro(now)), ExpiresAt: k.ExpiresAt, LastUsedAt: k.LastUsedAt, CreatedAt: k.CreatedAt,
	}
}

// UsageRowView 控制台请求日志条目（cost_micro 为微分）。
type UsageRowView struct {
	RequestID        string    `json:"request_id"`
	CreatedAt        time.Time `json:"created_at"`
	Model            string    `json:"model"`
	KeyName          string    `json:"key_name"`
	KeyHint          string    `json:"key_hint"`
	Endpoint         string    `json:"endpoint"`
	Stream           bool      `json:"stream"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	Estimated        bool      `json:"estimated"`
	CostMicro        int64     `json:"cost_micro"`
	Status           string    `json:"status"`
	HTTPStatus       int       `json:"http_status"`
	LatencyMS        int       `json:"latency_ms"`
	TTFTMS           *int      `json:"ttft_ms"`
}

func NewUsageRowView(r UsageRow) UsageRowView {
	return UsageRowView{
		RequestID: r.RequestID, CreatedAt: r.CreatedAt, Model: r.Model, KeyName: r.KeyName, KeyHint: r.KeyHint,
		Endpoint: r.Endpoint, Stream: r.Stream, PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
		Estimated: r.Estimated, CostMicro: r.CostMicro, Status: r.Status, HTTPStatus: r.HTTPStatus, LatencyMS: r.LatencyMS, TTFTMS: r.TTFTMS,
	}
}

// DailyView 控制台按天 × 模型汇总（day 为北京时间 YYYY-MM-DD）。
type DailyView struct {
	Day              string `json:"day"`
	Model            string `json:"model"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CostMicro        int64  `json:"cost_micro"`
}

// NewDailyView DailyRow.Day 来自 DATE 列（UTC 零点），直接格式化即为该自然日。
func NewDailyView(d DailyRow) DailyView {
	return DailyView{Day: d.Day.Format("2006-01-02"), Model: d.ModelSlug, Requests: d.Requests, PromptTokens: d.PromptTokens,
		CompletionTokens: d.CompletionTokens, CostMicro: d.CostMicro}
}

// SettlementView 控制台结算记录（金额单位分）。
type SettlementView struct {
	ID               int64      `json:"id"`
	CreatedAt        time.Time  `json:"created_at"`
	Requests         int        `json:"requests"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	UsageAmount      int64      `json:"usage_amount"`
	DebtAmount       int64      `json:"debt_amount"`
	Charged          int64      `json:"charged"`
	WalletLedgerID   *int64     `json:"wallet_ledger_id"`
	PeriodFrom       *time.Time `json:"period_from"`
	PeriodTo         *time.Time `json:"period_to"`
}

func NewSettlementView(x Settlement) SettlementView {
	return SettlementView{
		ID: x.ID, CreatedAt: x.CreatedAt, Requests: x.Requests, PromptTokens: x.PromptTokens, CompletionTokens: x.CompletionTokens,
		UsageAmount: x.UsageAmount, DebtAmount: x.DebtAmount, Charged: x.Charged, WalletLedgerID: x.WalletLedgerID,
		PeriodFrom: x.PeriodFrom, PeriodTo: x.PeriodTo,
	}
}
