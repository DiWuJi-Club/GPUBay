package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/tz"
)

// Token 商城控制台接口（规格 §10.1）。金额单位：价格「分 / 百万 token」，cost_micro 微分，其余金额分。

// llmDailyMaxDays /llm/usage/daily 的 days 上限（与明细窗口同为 90 天）。
const llmDailyMaxDays = 90

// llmBaseURL 控制台展示的 OpenAI 兼容网关地址（https://<domain>/v1）。
func (s *Server) llmBaseURL() string {
	if s.LLMBaseURL != "" {
		return s.LLMBaseURL
	}
	return s.PublicBaseURL + "/v1"
}

func (s *Server) handleLLMModels(w http.ResponseWriter, r *http.Request) {
	items, err := s.LLM.PublicModels(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "base_url": s.llmBaseURL()})
}

// llmAccountView 在 llm.AccountView 上加网关地址。
type llmAccountView struct {
	llm.AccountView
	BaseURL string `json:"base_url"`
}

func (s *Server) handleLLMAccount(w http.ResponseWriter, r *http.Request) {
	v, err := s.LLM.Account(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, llmAccountView{AccountView: v, BaseURL: s.llmBaseURL()})
}

func (s *Server) handleLLMKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.LLM.ListKeys(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	now := s.Clock.Now()
	out := make([]llm.KeyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, llm.NewKeyView(k, now))
	}
	writeJSON(w, http.StatusOK, list(out))
}

func (s *Server) handleLLMCreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string   `json:"name"`
		ModelSlugs []string `json:"model_slugs"`
		MonthlyCap *int64   `json:"monthly_cap"`
		ExpiresAt  *string  `json:"expires_at"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	in := llm.KeyInput{Name: req.Name, ModelSlugs: req.ModelSlugs, MonthlyCap: req.MonthlyCap}
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			s.writeError(w, r, apperr.LLMKeyInvalid("expires_at", "到期时间格式不正确"))
			return
		}
		t = t.UTC()
		in.ExpiresAt = &t
	}
	plain, k, err := s.LLM.CreateKey(r.Context(), currentUser(r).ID, in, clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": plain, "item": llm.NewKeyView(k, s.Clock.Now())})
}

// optInt64 PUT 的三态整数：键不出现 = 不改；null = 清空；数字 = 设置。
type optInt64 struct {
	Set   bool
	Value *int64
}

func (o *optInt64) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// llmKeyID 路径里的 Key ID；非正整数按不存在处理。
func llmKeyID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.ErrLLMKeyNotFound
	}
	return id, nil
}

func (s *Server) handleLLMUpdateKey(w http.ResponseWriter, r *http.Request) {
	id, err := llmKeyID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Name       *string   `json:"name"`
		Status     *string   `json:"status"`
		ModelSlugs *[]string `json:"model_slugs"`
		MonthlyCap optInt64  `json:"monthly_cap"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	in := llm.KeyUpdate{Name: req.Name, Status: req.Status, ModelSlugs: req.ModelSlugs}
	if req.MonthlyCap.Set {
		if req.MonthlyCap.Value == nil {
			in.ClearMonthlyCap = true
		} else {
			in.MonthlyCap = req.MonthlyCap.Value
		}
	}
	k, err := s.LLM.UpdateKey(r.Context(), currentUser(r).ID, id, in, clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, llm.NewKeyView(k, s.Clock.Now()))
}

func (s *Server) handleLLMRevokeKey(w http.ResponseWriter, r *http.Request) {
	id, err := llmKeyID(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.LLM.RevokeKey(r.Context(), currentUser(r).ID, id, clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}

// parseLLMDate 解析 YYYY-MM-DD（北京时间）或 RFC3339；endOfDay 时日期取次日零点（to 含当天）。空 → nil。
func parseLLMDate(name, v string, endOfDay bool) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", v, tz.CST); err == nil {
		if endOfDay {
			t = t.AddDate(0, 0, 1)
		}
		t = t.UTC()
		return &t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		t = t.UTC()
		return &t, nil
	}
	return nil, apperr.BadRequest(name + " 的格式应为 2006-01-02")
}

func (s *Server) handleLLMUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := paging.Parse(q)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	from, err := parseLLMDate("from", q.Get("from"), false)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	to, err := parseLLMDate("to", q.Get("to"), true)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, t, err := llm.UsageWindow(from, to, s.Clock.Now())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	filter := llm.UsageFilter{UserID: currentUser(r).ID, Model: q.Get("model"), From: &f, To: &t}
	if v := q.Get("key_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			s.writeError(w, r, apperr.BadRequest("key_id 必须是正整数"))
			return
		}
		filter.KeyID = id
	}
	rows, total, err := s.LLM.ListUsage(r.Context(), filter, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]llm.UsageRowView, 0, len(rows))
	for _, x := range rows {
		out = append(out, llm.NewUsageRowView(x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total": total})
}

// handleLLMDaily 最近 days 个北京时间自然日（含今天）按天 × 模型汇总。
func (s *Server) handleLLMDaily(w http.ResponseWriter, r *http.Request) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > llmDailyMaxDays {
			s.writeError(w, r, apperr.BadRequest("days 必须是 1～90 之间的整数"))
			return
		}
		days = n
	}
	now := s.Clock.Now()
	from := now.In(tz.CST).AddDate(0, 0, -(days - 1))
	rows, err := s.LLM.DailyUsage(r.Context(), currentUser(r).ID, from, now)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]llm.DailyView, 0, len(rows))
	for _, d := range rows {
		out = append(out, llm.NewDailyView(d))
	}
	writeJSON(w, http.StatusOK, list(out))
}

func (s *Server) handleLLMSettlements(w http.ResponseWriter, r *http.Request) {
	p, err := paging.Parse(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rows, total, err := s.LLM.ListSettlements(r.Context(), currentUser(r).ID, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]llm.SettlementView, 0, len(rows))
	for _, x := range rows {
		out = append(out, llm.NewSettlementView(x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "total": total})
}
