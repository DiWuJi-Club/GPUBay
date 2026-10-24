package e2ehooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/butler"
	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/butler/llm/mock"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/tz"
)

// AI 管家钩子（计划 12，规格 §16.3 / D17）：
//
//	POST /hooks/butler/reply    把一轮脚本化回复放进 mock 提供方的队列（队列优先于规则表，一条用一次）
//	POST /hooks/butler/reset    清空尚未使用的队列（用例之间调用，免得上一个用例剩下的回复串到下一个）
//	POST /hooks/butler/usage    直接累加某用户某天的用量（测额度横幅 / 后台用量页）
//	POST /hooks/butler/settings 绕过后台改设置（走 Store.UpdateSettings 的同一校验，system 审计）并让设置缓存失效
//
// 请求体按字段严格解析：未知字段（拼错的键）→ 400，免得用例静默地没生效。

// maxChunkDelayMS 是 reply.chunk_delay_ms 的上限；mock 每 8 个字符（rune）一段、每段后停一次，
// 所以还限制整条回复的总停顿 ≤ maxTotalDelay（远小于单次模型请求超时 LLMTimeout 60 秒，否则这一轮会按模型超时失败）。
const (
	maxChunkDelayMS = 5000
	maxTotalDelay   = 30 * time.Second
	mockChunkRunes  = 8 // 与 mock.Chat 的分段一致
)

// decodeStrict 与 decode 相同，但拒绝未知字段与多余内容。
func decodeStrict[T any](body json.RawMessage) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, badRequest("请求参数格式不正确: " + err.Error())
	}
	if dec.More() {
		return v, badRequest("请求参数格式不正确: 多余的内容")
	}
	return v, nil
}

func (s *Server) butlerMock() (*mock.Mock, error) {
	if s.LLM == nil || s.App.Butler == nil {
		return nil, badRequest("AI 管家未启用（e2e-server 需 cfg.Butler = true 并注入 mock LLM）")
	}
	return s.LLM, nil
}

func (s *Server) butlerService() (*butler.Service, error) {
	if s.App.Butler == nil {
		return nil, badRequest("AI 管家未启用（cfg.Butler = false）")
	}
	return s.App.Butler, nil
}

type replyToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type replyUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// butlerReply：{text?, tool_calls?: [{name, args?}], error?: bool, usage?: {input_tokens, output_tokens}, chunk_delay_ms?}
// → 入队一条 mock.Reply；返回 {"queued": 队列长度}。
// error: true → 下一次 Chat 返回 *llm.UnavailableError（不能再带 text / tool_calls / usage）；否则 text 与 tool_calls 至少一个。
// args 必须是 JSON 对象，缺省 {}；工具名不校验（未知工具的处理也是可测的路径）。usage 缺省（或两项都为 0）→ mock 默认 1000 / 100。
func (s *Server) butlerReply(_ context.Context, body json.RawMessage) (any, error) {
	m, err := s.butlerMock()
	if err != nil {
		return nil, err
	}
	req, err := decodeStrict[struct {
		Text         string          `json:"text"`
		ToolCalls    []replyToolCall `json:"tool_calls"`
		Error        bool            `json:"error"`
		Usage        *replyUsage     `json:"usage"`
		ChunkDelayMS int             `json:"chunk_delay_ms"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.ChunkDelayMS < 0 || req.ChunkDelayMS > maxChunkDelayMS {
		return nil, badRequest(fmt.Sprintf("chunk_delay_ms 需在 0～%d 之间", maxChunkDelayMS))
	}
	r := mock.Reply{ChunkDelay: time.Duration(req.ChunkDelayMS) * time.Millisecond}
	if req.Error {
		if req.Text != "" || len(req.ToolCalls) > 0 || req.Usage != nil {
			return nil, badRequest("error: true 时不能再带 text / tool_calls / usage")
		}
		r.Err = &llm.UnavailableError{Status: 503, Cause: errors.New("e2e 钩子注入的模型不可用")}
		m.Enqueue(r)
		return map[string]int{"queued": m.Queued()}, nil
	}
	if req.Text == "" && len(req.ToolCalls) == 0 {
		return nil, badRequest("text 与 tool_calls 至少要有一个（或 error: true）")
	}
	if chunks := (utf8.RuneCountInString(req.Text) + mockChunkRunes - 1) / mockChunkRunes; time.Duration(chunks)*r.ChunkDelay > maxTotalDelay {
		return nil, badRequest(fmt.Sprintf("chunk_delay_ms × 分段数（每 %d 字一段）不能超过 %d 秒", mockChunkRunes, int(maxTotalDelay/time.Second)))
	}
	r.Text = req.Text
	for i, tc := range req.ToolCalls {
		if strings.TrimSpace(tc.Name) == "" {
			return nil, badRequest(fmt.Sprintf("tool_calls[%d].name 不能为空", i))
		}
		args := bytes.TrimSpace(tc.Args)
		if len(args) == 0 || string(args) == "null" {
			args = []byte(`{}`)
		}
		if args[0] != '{' || !json.Valid(args) {
			return nil, badRequest(fmt.Sprintf("tool_calls[%d].args 必须是 JSON 对象", i))
		}
		r.ToolCalls = append(r.ToolCalls, llm.ToolCall{Name: tc.Name, Args: json.RawMessage(args)})
	}
	if u := req.Usage; u != nil {
		if u.InputTokens < 0 || u.OutputTokens < 0 {
			return nil, badRequest("usage 的 token 数不能为负")
		}
		r.Usage = llm.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	}
	m.Enqueue(r)
	return map[string]int{"queued": m.Queued()}, nil
}

// butlerReset：清空 mock 队列；返回 {"queued": 0}。
func (s *Server) butlerReset(_ context.Context, body json.RawMessage) (any, error) {
	m, err := s.butlerMock()
	if err != nil {
		return nil, err
	}
	if _, err := decodeStrict[struct{}](body); err != nil {
		return nil, err
	}
	m.Reset()
	return map[string]int{"queued": m.Queued()}, nil
}

// butlerUsage：{user_id, day?: "YYYY-MM-DD"（北京日历日，缺省 = 假时钟的今天）, input_tokens?, output_tokens?, requests?, tool_calls?, failures?}
// → 累加到 butler_usage（与轮次记账同一 Store.AddUsage）；返回累加后的该行
// {"user_id","day","requests","input_tokens","output_tokens","tool_calls","failures"}。
func (s *Server) butlerUsage(ctx context.Context, body json.RawMessage) (any, error) {
	bt, err := s.butlerService()
	if err != nil {
		return nil, err
	}
	req, err := decodeStrict[struct {
		UserID       int64  `json:"user_id"`
		Day          string `json:"day"`
		InputTokens  int64  `json:"input_tokens"`
		OutputTokens int64  `json:"output_tokens"`
		Requests     int    `json:"requests"`
		ToolCalls    int    `json:"tool_calls"`
		Failures     int    `json:"failures"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.UserID <= 0 {
		return nil, badRequest("user_id 必须为正整数")
	}
	if req.InputTokens < 0 || req.OutputTokens < 0 || req.Requests < 0 || req.ToolCalls < 0 || req.Failures < 0 {
		return nil, badRequest("用量计数不能为负")
	}
	day := bt.DayOf(s.Clock.Now())
	if req.Day != "" {
		d, err := time.ParseInLocation("2006-01-02", req.Day, tz.CST)
		if err != nil {
			return nil, badRequest("day 必须是 YYYY-MM-DD")
		}
		day = d.UTC()
	}
	if _, err := s.App.Identity.GetUser(ctx, req.UserID); err != nil {
		if e := apperr.As(err); e != nil && e.Status == 404 {
			return nil, badRequest(fmt.Sprintf("用户 %d 不存在", req.UserID))
		}
		return nil, err
	}
	if err := bt.AddUsage(ctx, s.Pool, req.UserID, day, butler.Usage{Requests: req.Requests, InputTokens: req.InputTokens,
		OutputTokens: req.OutputTokens, ToolCalls: req.ToolCalls, Failures: req.Failures}); err != nil {
		return nil, err
	}
	dayStr := day.In(tz.CST).Format("2006-01-02")
	out := map[string]any{"user_id": req.UserID, "day": dayStr}
	var u butler.Usage
	err = s.Pool.QueryRow(ctx, `SELECT requests, input_tokens, output_tokens, tool_calls, failures FROM butler_usage
		WHERE user_id = $1 AND day = $2::date`, req.UserID, dayStr).Scan(&u.Requests, &u.InputTokens, &u.OutputTokens, &u.ToolCalls, &u.Failures)
	if err != nil && !db.IsNoRows(err) {
		return nil, err
	}
	out["requests"], out["input_tokens"], out["output_tokens"], out["tool_calls"], out["failures"] =
		u.Requests, u.InputTokens, u.OutputTokens, u.ToolCalls, u.Failures
	return out, nil
}

// butlerSettings：{defaults?: bool, enabled?, daily_token_limit?, max_turns?, notice?} → 以当前设置（defaults: true 时以迁移默认值）
// 为底合并给出的字段，经 Store.UpdateSettings 保存（同一校验，校验失败 → 400；审计 butler.settings，actor = system）并让本进程缓存失效；
// 返回保存后的 {"enabled","daily_token_limit","max_turns","notice"}。
func (s *Server) butlerSettings(ctx context.Context, body json.RawMessage) (any, error) {
	bt, err := s.butlerService()
	if err != nil {
		return nil, err
	}
	req, err := decodeStrict[struct {
		Defaults        bool    `json:"defaults"`
		Enabled         *bool   `json:"enabled"`
		DailyTokenLimit *int64  `json:"daily_token_limit"`
		MaxTurns        *int    `json:"max_turns"`
		Notice          *string `json:"notice"`
	}](body)
	if err != nil {
		return nil, err
	}
	cur := butler.DefaultSettings()
	if !req.Defaults {
		bt.InvalidateSettings() // 以库里的当前值为底，不用可能过时的缓存
		if cur, err = bt.Settings(ctx); err != nil {
			return nil, err
		}
	}
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if req.DailyTokenLimit != nil {
		cur.DailyTokenLimit = *req.DailyTokenLimit
	}
	if req.MaxTurns != nil {
		cur.MaxTurns = *req.MaxTurns
	}
	if req.Notice != nil {
		cur.Notice = *req.Notice
	}
	v, err := bt.UpdateSettings(ctx, audit.Actor{}, cur)
	if err != nil {
		if e := apperr.As(err); e != nil && e.Status == 400 {
			return nil, badRequest(e.Message)
		}
		return nil, err
	}
	bt.InvalidateSettings()
	return map[string]any{"enabled": v.Enabled, "daily_token_limit": v.DailyTokenLimit, "max_turns": v.MaxTurns, "notice": v.Notice}, nil
}
