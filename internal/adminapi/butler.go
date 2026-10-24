package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/butler"
	"spark-cloud/internal/butler/tools"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/platform/tz"
)

// butlerRoutes AI 管家（计划 12，D8）：设置与用量全员可看，只有 super 改设置（不需动态码）；对话内容只给 ops。
// Butler == nil（平台未启用管家）时一条都不注册。
func (s *Server) butlerRoutes(g chi.Router) {
	if s.Butler == nil {
		return
	}
	s.handle(g, http.MethodGet, "/butler/settings", allRoles, s.handleButlerSettings)
	s.handle(g, http.MethodPut, "/butler/settings", superOnly, s.handleButlerUpdateSettings)
	s.handle(g, http.MethodGet, "/butler/usage", allRoles, s.handleButlerUsage)
	s.handle(g, http.MethodGet, "/butler/usage/users", allRoles, s.handleButlerUsageUsers)
	s.handle(g, http.MethodGet, "/butler/conversations", opsRoles, s.handleButlerConversations)
	s.handle(g, http.MethodGet, "/butler/conversations/{id}", opsRoles, s.handleButlerConversation)
}

// butlerMaxRangeDays 是用量总览一次最多查询的天数（两端含），与用量保留期一致。
const butlerMaxRangeDays = butler.UsageRetentionDays

// ---- 设置 ----

type butlerSettingsView struct {
	Enabled         bool      `json:"enabled"`
	DailyTokenLimit int64     `json:"daily_token_limit"`
	MaxTurns        int       `json:"max_turns"`
	Notice          string    `json:"notice"`
	UpdatedAt       time.Time `json:"updated_at"`
	UpdatedBy       *int64    `json:"updated_by"`
}

func toButlerSettingsView(v butler.Settings) butlerSettingsView {
	return butlerSettingsView{Enabled: v.Enabled, DailyTokenLimit: v.DailyTokenLimit, MaxTurns: v.MaxTurns, Notice: v.Notice,
		UpdatedAt: v.UpdatedAt, UpdatedBy: v.UpdatedBy}
}

func (s *Server) handleButlerSettings(w http.ResponseWriter, r *http.Request) {
	v, err := s.Butler.Settings(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toButlerSettingsView(v))
}

func (s *Server) handleButlerUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled         *bool   `json:"enabled"`
		DailyTokenLimit *int64  `json:"daily_token_limit"`
		MaxTurns        *int    `json:"max_turns"`
		Notice          *string `json:"notice"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Enabled == nil || req.DailyTokenLimit == nil || req.MaxTurns == nil || req.Notice == nil {
		s.writeError(w, r, apperr.BadRequest("enabled、daily_token_limit、max_turns、notice 都必须提供"))
		return
	}
	// 校验与审计 butler.settings（diff {old, new}）都在 Store.UpdateSettings 里，同一事务
	v, err := s.Butler.UpdateSettings(r.Context(), s.actor(r), butler.Settings{Enabled: *req.Enabled,
		DailyTokenLimit: *req.DailyTokenLimit, MaxTurns: *req.MaxTurns, Notice: *req.Notice})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toButlerSettingsView(v))
}

// ---- 用量 ----

// butlerCounts 是用量计数（按日合计 / 按用户 / 合计共用）；cost_cents 按 Server 注入的单价估算。
type butlerCounts struct {
	Requests     int   `json:"requests"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	ToolCalls    int   `json:"tool_calls"`
	Failures     int   `json:"failures"`
	CostCents    int64 `json:"cost_cents"`
}

type butlerDailyView struct {
	Day string `json:"day"`
	butlerCounts
	Users int `json:"users"`
}

type butlerUserUsageView struct {
	UserID int64  `json:"user_id"`
	Phone  string `json:"phone"`
	butlerCounts
}

// butlerCost 估算费用（分）= round((输入 × 输入单价 + 输出 × 输出单价) / 1e6)，单价单位为分 / 百万 token，四舍五入（half-up）。
func (s *Server) butlerCost(in, out int64) int64 {
	return (in*int64(s.LLMPriceInCentsPerM) + out*int64(s.LLMPriceOutCentsPerM) + 500_000) / 1_000_000
}

func (s *Server) butlerCounts(requests int, in, out int64, toolCalls, failures int) butlerCounts {
	return butlerCounts{Requests: requests, InputTokens: in, OutputTokens: out, ToolCalls: toolCalls, Failures: failures,
		CostCents: s.butlerCost(in, out)}
}

// butlerDayParam 解析日期参数（YYYY-MM-DD 按北京时间，也接受 RFC3339），返回所在东八区自然日的 00:00；空值返回 def。
func butlerDayParam(name, v string, def time.Time) (time.Time, error) {
	t, err := parseTimeParam(name, v, false)
	if err != nil {
		return time.Time{}, err
	}
	if t == nil {
		return def, nil
	}
	return tz.DayStart(*t), nil
}

func cstDate(t time.Time) string { return t.In(tz.CST).Format("2006-01-02") }

func (s *Server) handleButlerUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	to, err := butlerDayParam("to", q.Get("to"), tz.DayStart(s.Clock.Now()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	from, err := butlerDayParam("from", q.Get("from"), to.AddDate(0, 0, -29))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if from.After(to) {
		s.writeError(w, r, apperr.BadRequest("from 不能晚于 to"))
		return
	}
	// 按日历日计天数（东八区无夏令时，整日为 24 小时）
	if days := int(to.Sub(from).Hours()/24) + 1; days > butlerMaxRangeDays {
		s.writeError(w, r, apperr.BadRequest("查询区间不能超过 "+strconv.Itoa(butlerMaxRangeDays)+" 天"))
		return
	}
	rows, err := s.Butler.UsageByDay(r.Context(), from, to)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]butlerDailyView, 0, len(rows))
	var t butlerCounts
	for _, d := range rows {
		items = append(items, butlerDailyView{Day: cstDate(d.Day), Users: d.Users,
			butlerCounts: s.butlerCounts(d.Requests, d.InputTokens, d.OutputTokens, d.ToolCalls, d.Failures)})
		t.Requests += d.Requests
		t.InputTokens += d.InputTokens
		t.OutputTokens += d.OutputTokens
		t.ToolCalls += d.ToolCalls
		t.Failures += d.Failures
	}
	t.CostCents = s.butlerCost(t.InputTokens, t.OutputTokens) // 按合计 token 算，不是各行取整后相加
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"from": cstDate(from), "to": cstDate(to),
		"price_in_cents_per_m": s.LLMPriceInCentsPerM, "price_out_cents_per_m": s.LLMPriceOutCentsPerM,
		"items": items, "totals": t,
	})
}

func (s *Server) handleButlerUsageUsers(w http.ResponseWriter, r *http.Request) {
	day, err := butlerDayParam("day", r.URL.Query().Get("day"), tz.DayStart(s.Clock.Now()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rows, total, err := s.Butler.UsageByUser(r.Context(), day, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]butlerUserUsageView, 0, len(rows))
	for _, u := range rows {
		items = append(items, butlerUserUsageView{UserID: u.UserID, Phone: u.Phone,
			butlerCounts: s.butlerCounts(u.Requests, u.InputTokens, u.OutputTokens, u.ToolCalls, u.Failures)})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"day": cstDate(day), "items": items, "total": total})
}

// ---- 对话审阅（ops，只读） ----

type butlerConvView struct {
	ID            string     `json:"id"`
	UserID        int64      `json:"user_id"`
	Phone         string     `json:"phone"`
	Title         string     `json:"title"`
	Status        string     `json:"status"`
	Provider      string     `json:"provider"`
	Model         string     `json:"model"`
	Messages      int        `json:"messages"`
	UserTurns     int        `json:"user_turns"`
	InputTokens   int64      `json:"input_tokens"`
	OutputTokens  int64      `json:"output_tokens"`
	WriteCalls    int        `json:"write_calls"`
	LastMessageAt *time.Time `json:"last_message_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func toButlerConvView(x butler.AdminConvRow) butlerConvView {
	c := x.Conversation
	return butlerConvView{ID: c.ID, UserID: c.UserID, Phone: x.Phone, Title: c.Title, Status: c.Status, Provider: c.Provider,
		Model: c.Model, Messages: x.Messages, UserTurns: c.UserTurns, InputTokens: c.InputTokens, OutputTokens: c.OutputTokens,
		WriteCalls: x.WriteCalls, LastMessageAt: c.LastMessageAt, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

// butlerToolCallView 是详情的工具调用表一行：args / summary 为落库的 *** 版本；从不输出 secret_env_enc。
type butlerToolCallView struct {
	ID            string          `json:"id"`
	MessageID     int64           `json:"message_id"`
	Tool          string          `json:"tool"`
	Label         string          `json:"label"`
	Kind          string          `json:"kind"`
	Args          json.RawMessage `json:"args"`
	Summary       string          `json:"summary"`
	QuoteAmount   *int64          `json:"quote_amount"`
	Status        string          `json:"status"`
	ResultSummary string          `json:"result_summary"`
	ErrorCode     string          `json:"error_code"`
	ResultRef     string          `json:"result_ref"`
	ExpiresAt     *time.Time      `json:"expires_at"`
	DecidedAt     *time.Time      `json:"decided_at"`
	ExecutedAt    *time.Time      `json:"executed_at"`
	CreatedAt     time.Time       `json:"created_at"`
}

// compactJSON 去掉 JSONB 读出时自带的空白（{"a": 1} → {"a":1}）；不是合法 JSON 时原样返回。
func compactJSON(raw json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	if len(raw) == 0 || json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}

func toButlerToolCallView(c butler.ToolCall) butlerToolCallView {
	args := compactJSON(c.Args)
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	result := compactJSON(c.Result)
	return butlerToolCallView{ID: c.ID, MessageID: c.MessageID, Tool: c.Tool, Label: tools.Label(c.Tool), Kind: c.Kind,
		Args: args, Summary: c.Summary, QuoteAmount: c.QuoteAmount, Status: c.Status,
		ResultSummary: butler.CallResultSummary(result), ErrorCode: butler.CallErrorCode(result), ResultRef: c.ResultRef,
		ExpiresAt: c.ExpiresAt, DecidedAt: c.DecidedAt, ExecutedAt: c.ExecutedAt, CreatedAt: c.CreatedAt}
}

func (s *Server) handleButlerConversations(w http.ResponseWriter, r *http.Request) {
	var f butler.ConvFilter
	if v := r.URL.Query().Get("user_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			s.writeError(w, r, apperr.BadRequest("user_id 不正确"))
			return
		}
		f.UserID = id
	}
	from, to, err := parseRange(r) // updated_at ∈ [from, to)，to 为日期时含当天
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f.From, f.To = from, to
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rows, total, err := s.Butler.AdminListConversations(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]butlerConvView, 0, len(rows))
	for _, x := range rows {
		items = append(items, toButlerConvView(x))
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(items, total))
}

func (s *Server) handleButlerConversation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := r.Context()
	row, err := s.Butler.AdminConvRowOf(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	conv, msgs, calls, err := s.Butler.AdminConversation(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// 两次读之间对话可能有新进展：头部的状态与计数以同时读出的消息 / 调用为准
	row.Conversation, row.Messages, row.WriteCalls = conv, len(msgs), 0
	for _, c := range calls {
		if c.Kind == butler.KindWrite {
			row.WriteCalls++
		}
	}
	views := make([]butlerToolCallView, 0, len(calls))
	for _, c := range calls {
		views = append(views, toButlerToolCallView(c))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"conversation": struct {
			butlerConvView
			Context butler.Context `json:"context"`
		}{toButlerConvView(row), conv.Context},
		// 消息块：tool 的 status 见 butler.AdminMessageViews（写调用按 tool_calls.status），tool_result 带 ok 与 suspicious
		"messages":   butler.AdminMessageViews(msgs, calls, conv.Status),
		"tool_calls": views,
	})
}

// ---- 用户详情 / 仪表盘增量（Butler == nil 时不加字段） ----

type butlerUserStatsView struct {
	Conversations int   `json:"conversations"`
	Tokens30d     int64 `json:"tokens_30d"`
	WriteCalls30d int   `json:"write_calls_30d"`
}

// addButlerUserStats 给 GET /users/{id} 的响应加 butler（近 30 天）。
func (s *Server) addButlerUserStats(ctx context.Context, userID int64, out map[string]any) error {
	if s.Butler == nil {
		return nil
	}
	st, err := s.Butler.UserButlerStats(ctx, userID, s.Clock.Now().AddDate(0, 0, -30))
	if err != nil {
		return err
	}
	out["butler"] = butlerUserStatsView{Conversations: st.Conversations, Tokens30d: st.Tokens30d, WriteCalls30d: st.WriteCalls30d}
	return nil
}

// addButlerDashboard 给 GET /dashboard 的响应加今天（东八区）的全站管家请求数、token（输入 + 输出）与失败数。
func (s *Server) addButlerDashboard(ctx context.Context, now time.Time, out map[string]any) error {
	if s.Butler == nil {
		return nil
	}
	requests, tokens, failures, err := s.Butler.Counts(ctx, now)
	if err != nil {
		return err
	}
	out["butler_requests_today"], out["butler_tokens_today"], out["butler_failures_today"] = requests, tokens, failures
	return nil
}
