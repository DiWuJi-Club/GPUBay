package adminapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/httpx"
)

// llmRoutes Token 商城后台接口（计划 9 规格 §10.2）。角色：模型只读全员、改动 super（上下线另要 TOTP）；
// 机器池 / 节点 ops（移除另要 TOTP）；账户与 Key 列表 ops + finance，改账户 / 停用 Key ops；用量、汇总、结算 finance；仪表盘卡片全员。
func (s *Server) llmRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/llm/models", allRoles, s.handleLLMModels)
	s.handle(g, http.MethodPost, "/llm/models", superOnly, s.handleLLMCreateModel)
	s.handle(g, http.MethodPut, "/llm/models/{id}", superOnly, s.handleLLMUpdateModel)
	s.handle(g, http.MethodPost, "/llm/models/{id}/status", superOnly, s.handleLLMModelStatus)
	s.handle(g, http.MethodDelete, "/llm/models/{id}", superOnly, s.handleLLMDeleteModel)

	s.handle(g, http.MethodGet, "/llm/pool", opsRoles, s.handleLLMPool)
	s.handle(g, http.MethodGet, "/llm/nodes", opsRoles, s.handleLLMNodes)
	s.handle(g, http.MethodGet, "/llm/nodes/{id}", opsRoles, s.handleLLMNode)
	s.handle(g, http.MethodPost, "/llm/nodes/{id}/drain", opsRoles, s.handleLLMDrainNode)
	s.handle(g, http.MethodPost, "/llm/nodes/{id}/rebuild", opsRoles, s.handleLLMRebuildNode)
	s.handle(g, http.MethodPost, "/llm/nodes/{id}/remove", opsRoles, s.handleLLMRemoveNode)

	s.handle(g, http.MethodGet, "/llm/accounts", opsFinanceRoles, s.handleLLMAccounts)
	s.handle(g, http.MethodPut, "/llm/accounts/{user_id}", opsRoles, s.handleLLMUpdateAccount)
	s.handle(g, http.MethodGet, "/llm/keys", opsFinanceRoles, s.handleLLMKeys)
	s.handle(g, http.MethodPost, "/llm/keys/{id}/disable", opsRoles, s.handleLLMDisableKey)

	s.handle(g, http.MethodGet, "/llm/usage", financeRoles, s.handleLLMUsage)
	s.handle(g, http.MethodGet, "/llm/usage/export", financeRoles, s.handleLLMUsageExport)
	s.handle(g, http.MethodGet, "/llm/usage/daily", financeRoles, s.handleLLMUsageDaily)
	s.handle(g, http.MethodGet, "/llm/settlements", financeRoles, s.handleLLMSettlements)
	s.handle(g, http.MethodGet, "/llm/summary", allRoles, s.handleLLMSummary)
}

// ---- 模型 ----

// modelRequest POST / PUT /llm/models 的请求体（llm.ModelInput 没有 JSON 标签）。PUT 时 slug 被忽略（创建后不可改）。
type modelRequest struct {
	Slug             string            `json:"slug"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Kind             string            `json:"kind"`
	Source           string            `json:"source"`
	SourceID         string            `json:"source_id"`
	ImageID          int64             `json:"image_id"`
	AppVersionID     *int64            `json:"app_version_id"`
	ContextLen       int               `json:"context_len"`
	MaxOutputTokens  int               `json:"max_output_tokens"`
	DefaultMaxTokens int               `json:"default_max_tokens"`
	InputPrice       int64             `json:"input_price"`
	OutputPrice      int64             `json:"output_price"`
	NodeEnv          map[string]string `json:"node_env"`
	Replicas         int               `json:"replicas"`
	MaxConcurrency   int               `json:"max_concurrency"`
	Features         []string          `json:"features"`
	Sort             int               `json:"sort"`
}

func (q modelRequest) input() llm.ModelInput {
	return llm.ModelInput{Slug: q.Slug, Name: q.Name, Description: q.Description, Kind: q.Kind, Source: q.Source, SourceID: q.SourceID,
		ImageID: q.ImageID, AppVersionID: q.AppVersionID, ContextLen: q.ContextLen, MaxOutputTokens: q.MaxOutputTokens,
		DefaultMaxTokens: q.DefaultMaxTokens, InputPrice: q.InputPrice, OutputPrice: q.OutputPrice, NodeEnv: q.NodeEnv,
		Replicas: q.Replicas, MaxConcurrency: q.MaxConcurrency, Sort: q.Sort, Features: q.Features}
}

func (s *Server) handleLLMModels(w http.ResponseWriter, r *http.Request) {
	list, err := s.LLM.AdminListModels(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleLLMCreateModel(w http.ResponseWriter, r *http.Request) {
	var req modelRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.LLM.CreateModel(r.Context(), s.actor(r), req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, m)
}

func (s *Server) handleLLMUpdateModel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req modelRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.LLM.UpdateModel(r.Context(), s.actor(r), id, req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

// handleLLMModelStatus 上下线：super + 动态码（withStepUp）。
func (s *Server) handleLLMModelStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Status string `json:"status"`
		TOTP   string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	var m llm.Model
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		m, err = s.LLM.SetModelStatus(r.Context(), s.actor(r), id, req.Status)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

func (s *Server) handleLLMDeleteModel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.LLM.DeleteModel(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

// ---- 机器池与节点 ----

func (s *Server) handleLLMPool(w http.ResponseWriter, r *http.Request) {
	v, err := s.LLM.PoolView(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) handleLLMNodes(w http.ResponseWriter, r *http.Request) {
	mid, err := queryInt(r, "model_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, err := s.LLM.ListNodes(r.Context(), llm.NodeFilter{ModelID: mid, State: r.URL.Query().Get("state")})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleLLMNode(w http.ResponseWriter, r *http.Request) {
	d, err := s.LLM.GetNode(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (s *Server) handleLLMDrainNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.LLM.AdminDrainNode(r.Context(), s.actor(r), chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, n)
}

func (s *Server) handleLLMRebuildNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.LLM.AdminRebuildNode(r.Context(), s.actor(r), chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, n)
}

// handleLLMRemoveNode 移除节点：ops + 动态码（withStepUp），原因必填。
func (s *Server) handleLLMRemoveNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
		TOTP   string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	var n llm.Node
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		n, err = s.LLM.AdminRemoveNode(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.Reason)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, n)
}

// checkPoolLeave 机组变更前调用：ids 中当前在 Token 服务池、且将离开池（target 不是池）的机器上有非终态节点或未结束的系统实例
// → apperr.ErrLLMPoolMachineBusy（规格 §3.3）。from 非 nil 时只处理从该机组移出的情形（DELETE /groups/{id}/machines）。
// LLM 未装配或没有池机组时不检查。检查在机组事务之外（裁定：rental / fleet 不依赖 llm）。
func (s *Server) checkPoolLeave(ctx context.Context, ids []int64, from, target *int64) error {
	if s.LLM == nil || len(ids) == 0 {
		return nil
	}
	poolID, err := s.Fleet.LLMPoolGroupID(ctx, s.Fleet.Pool())
	if errors.Is(err, apperr.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if (from != nil && *from != poolID) || (target != nil && *target == poolID) {
		return nil
	}
	for _, id := range ids {
		m, err := s.Fleet.Get(ctx, s.Fleet.Pool(), id)
		if errors.Is(err, apperr.ErrNotFound) {
			continue // 不存在的机器交给机组操作本身处理
		}
		if err != nil {
			return err
		}
		if m.GroupID == nil || *m.GroupID != poolID {
			continue
		}
		busy, err := s.LLM.MachineHasLiveNode(ctx, id)
		if err != nil {
			return err
		}
		if busy {
			return apperr.ErrLLMPoolMachineBusy
		}
	}
	return nil
}

// ---- 账户与 Key ----

func (s *Server) handleLLMAccounts(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	uid, err := queryInt(r, "user_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	blocked, err := parseBoolParam(q.Get("blocked"), "blocked")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	debt, err := parseBoolParam(q.Get("debt"), "debt")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f := llm.AccountFilter{Q: q.Get("q"), UserID: uid, Blocked: blocked, Debt: debt != nil && *debt}
	list, total, err := s.LLM.AdminListAccounts(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleLLMUpdateAccount(w http.ResponseWriter, r *http.Request) {
	uid, err := pathID(r, "user_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req llm.AccountUpdate
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	a, err := s.LLM.AdminUpdateAccount(r.Context(), s.actor(r), uid, req)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}

func (s *Server) handleLLMKeys(w http.ResponseWriter, r *http.Request) {
	uid, err := queryInt(r, "user_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if uid == 0 {
		s.writeError(w, r, apperr.BadRequest("请提供 user_id"))
		return
	}
	list, err := s.LLM.AdminKeyViews(r.Context(), uid)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleLLMDisableKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.LLM.AdminDisableKey(r.Context(), s.actor(r), id, req.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

// ---- 用量、汇总、结算 ----

// llmWindow 解析 from / to（北京时间日期，to 含当天；或 RFC3339）并套用明细窗口规则：默认最近 7 天、跨度 > 90 天 → 400。
func (s *Server) llmWindow(r *http.Request) (time.Time, time.Time, error) {
	from, to, err := parseRange(r)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return llm.UsageWindow(from, to, s.Clock.Now())
}

func (s *Server) llmUsageFilter(r *http.Request) (llm.UsageFilter, error) {
	var f llm.UsageFilter
	var err error
	if f.UserID, err = queryInt(r, "user_id"); err != nil {
		return f, err
	}
	if f.ModelID, err = queryInt(r, "model_id"); err != nil {
		return f, err
	}
	if f.KeyID, err = queryInt(r, "key_id"); err != nil {
		return f, err
	}
	f.Status = r.URL.Query().Get("status")
	from, to, err := s.llmWindow(r)
	if err != nil {
		return f, err
	}
	f.From, f.To = &from, &to
	return f, nil
}

func (s *Server) handleLLMUsage(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.llmUsageFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.LLM.ListUsage(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

var llmUsageStatusText = map[string]string{
	llm.UsageOK: "成功", llm.UsageClientAbort: "客户端中断", llm.UsageUpstreamError: "上游错误",
	llm.UsageUpstreamTimeout: "上游超时", llm.UsageNoNode: "无可用节点",
}

// microYuan 微分 → 元，6 位小数（1 元 = 10^8 微分；第 7 位起四舍五入），整数运算。
func microYuan(micro int64) string {
	sign := ""
	if micro < 0 {
		sign, micro = "-", -micro
	}
	u := (micro + 50) / 100 // 10^-6 元
	return fmt.Sprintf("%s%d.%06d", sign, u/1_000_000, u%1_000_000)
}

func (s *Server) handleLLMUsageExport(w http.ResponseWriter, r *http.Request) {
	f, err := s.llmUsageFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	header := []string{"请求ID", "时间", "用户", "模型", "Key", "端点", "流式", "输入token", "输出token", "估算",
		"输入单价(元/百万token)", "输出单价(元/百万token)", "费用(元)", "状态", "延迟(ms)", "节点"}
	s.csvExport(w, r, "llm-usage", header, func(write func([]string) error) error {
		return s.LLM.ExportUsage(r.Context(), f, func(u llm.UsageRow) error {
			yes := func(b bool) string {
				if b {
					return "是"
				}
				return "否"
			}
			status := llmUsageStatusText[u.Status]
			if status == "" {
				status = u.Status
			}
			node := ""
			if u.NodeID != nil {
				node = *u.NodeID
			}
			return write([]string{u.RequestID, csvx.Time(u.CreatedAt), u.UserPhone, u.Model, u.KeyName + " " + u.KeyHint, u.Endpoint,
				yes(u.Stream), strconv.Itoa(u.PromptTokens), strconv.Itoa(u.CompletionTokens), yes(u.Estimated),
				csvx.Yuan(u.InputPrice), csvx.Yuan(u.OutputPrice), microYuan(u.CostMicro), status, strconv.Itoa(u.LatencyMS), node})
		})
	})
}

func (s *Server) handleLLMUsageDaily(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.llmWindow(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	group := r.URL.Query().Get("group_by")
	if group == "" {
		group = llm.DailyByModel
	}
	// 窗口是 [from, to)；按天汇总取两端所在的自然日（含），所以末端用 to 之前的最后一刻
	list, err := s.LLM.AdminDailyUsage(r.Context(), from, to.Add(-time.Nanosecond), group)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleLLMSettlements(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	uid, err := queryInt(r, "user_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	from, to, err := parseRange(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.LLM.AdminListSettlements(r.Context(), llm.SettlementFilter{UserID: uid, From: from, To: to}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleLLMSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.LLM.AdminSummary(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sum)
}
