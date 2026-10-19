package adminapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/referral"
)

// referralRoutes 邀请返利（计划 8）。finance 查看与审核（撤销需动态码）；改费率与重扫只给 super 且需动态码。
// 需要动态码的接口先解析、校验参数再二次验证：参数错误不消耗动态码，也不打开验证窗口（withStepUp）。
func (s *Server) referralRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/referral/summary", financeRoles, s.handleReferralSummary)
	s.handle(g, http.MethodGet, "/referral/config", financeRoles, s.handleReferralConfig)
	s.handle(g, http.MethodPost, "/referral/config", superOnly, s.handleReferralSetConfig)
	s.handle(g, http.MethodGet, "/referral/inviters", financeRoles, s.handleReferralInviters)
	s.handle(g, http.MethodGet, "/referral/inviters/{id}", financeRoles, s.handleReferralInviter)
	s.handle(g, http.MethodPost, "/referral/inviters/{id}/freeze", financeRoles, s.handleReferralFreeze)
	s.handle(g, http.MethodPost, "/referral/inviters/{id}/unfreeze", financeRoles, s.handleReferralUnfreeze)
	s.handle(g, http.MethodGet, "/referral/bindings", financeRoles, s.handleReferralBindings)
	s.handle(g, http.MethodGet, "/referral/bindings/export", financeRoles, s.handleReferralExportBindings)
	s.handle(g, http.MethodPost, "/referral/bindings/{id}/approve", financeRoles, s.handleReferralApprove)
	s.handle(g, http.MethodPost, "/referral/bindings/{id}/revoke", financeRoles, s.handleReferralRevoke)
	s.handle(g, http.MethodGet, "/referral/rewards", financeRoles, s.handleReferralRewards)
	s.handle(g, http.MethodGet, "/referral/rewards/export", financeRoles, s.handleReferralExportRewards)
	s.handle(g, http.MethodPost, "/referral/rescan", superOnly, s.handleReferralRescan)
}

func (s *Server) handleReferralSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.Referral.Summary(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sum)
}

func (s *Server) handleReferralConfig(w http.ResponseWriter, r *http.Request) {
	cur, err := s.Referral.CurrentConfig(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	versions, err := s.Referral.ListRateVersions(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"current": cur, "versions": versions})
}

func (s *Server) handleReferralSetConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RateBP           int    `json:"rate_bp"`
		MaxPerInvitee    int64  `json:"max_per_invitee"`
		MaxPerInviter30d int64  `json:"max_per_inviter_30d"`
		HoldDays         int    `json:"hold_days"`
		PeriodDays       int    `json:"period_days"`
		EffectiveFrom    string `json:"effective_from"`
		Note             string `json:"note"`
		TOTP             string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	in := referral.RateInput{RateBP: req.RateBP, MaxPerInvitee: req.MaxPerInvitee, MaxPerInviter30d: req.MaxPerInviter30d,
		HoldDays: req.HoldDays, PeriodDays: req.PeriodDays, Note: req.Note}
	if req.EffectiveFrom != "" {
		t, err := time.Parse(time.RFC3339, req.EffectiveFrom)
		if err != nil {
			s.writeError(w, r, apperr.BadRequest("生效时间格式应为 RFC3339"))
			return
		}
		in.EffectiveFrom = &t
	}
	if err := in.Validate(s.Clock.Now()); err != nil {
		s.writeError(w, r, err)
		return
	}
	var v referral.RateVersion
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		v, err = s.Referral.AddRateVersion(r.Context(), s.actor(r), in)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) handleReferralInviters(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	list, total, err := s.Referral.AdminListInviters(r.Context(),
		referral.InviterFilter{Q: q.Get("q"), Status: q.Get("status"), Flag: q.Get("flag"), Sort: q.Get("sort")}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleReferralInviter(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	d, err := s.Referral.AdminInviter(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (s *Server) handleReferralFreeze(w http.ResponseWriter, r *http.Request) {
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
	if err := s.Referral.Freeze(r.Context(), s.actor(r), id, req.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleReferralUnfreeze(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Referral.Unfreeze(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func bindingFilter(r *http.Request) (referral.BindingFilter, error) {
	inviter, err := queryInt(r, "inviter_id")
	if err != nil {
		return referral.BindingFilter{}, err
	}
	invitee, err := queryInt(r, "invitee_id")
	if err != nil {
		return referral.BindingFilter{}, err
	}
	from, to, err := parseRange(r)
	if err != nil {
		return referral.BindingFilter{}, err
	}
	q := r.URL.Query()
	return referral.BindingFilter{InviterID: inviter, InviteeID: invitee, Status: q.Get("status"), Flag: q.Get("flag"), From: from, To: to}, nil
}

func (s *Server) handleReferralBindings(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := bindingFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Referral.AdminListBindings(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleReferralExportBindings(w http.ResponseWriter, r *http.Request) {
	f, err := bindingFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.csvExport(w, r, "referral-bindings", []string{"绑定ID", "邀请人ID", "被邀请人ID", "邀请码", "状态", "标记", "绑定时间", "返利截止", "IP", "设备"},
		func(write func([]string) error) error {
			return s.Referral.ExportBindings(r.Context(), f, func(b referral.Binding) error {
				return write([]string{strconv.FormatInt(b.ID, 10), strconv.FormatInt(b.InviterID, 10), strconv.FormatInt(b.InviteeID, 10), b.Code, b.Status,
					strings.Join(b.RiskFlags, " "), csvx.Time(b.BoundAt), csvx.Time(b.RebateUntil), b.IP, b.DeviceID})
			})
		})
}

func (s *Server) handleReferralApprove(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Referral.Approve(r.Context(), s.actor(r), id, req.Note); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleReferralRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
		TOTP   string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := referral.CheckRevokeReason(req.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	var voided int64
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		voided, err = s.Referral.Revoke(r.Context(), s.actor(r), id, req.Reason)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"voided_amount": voided})
}

func rewardFilter(r *http.Request) (referral.RewardFilter, error) {
	inviter, err := queryInt(r, "inviter_id")
	if err != nil {
		return referral.RewardFilter{}, err
	}
	invitee, err := queryInt(r, "invitee_id")
	if err != nil {
		return referral.RewardFilter{}, err
	}
	from, to, err := parseRange(r)
	if err != nil {
		return referral.RewardFilter{}, err
	}
	return referral.RewardFilter{InviterID: inviter, InviteeID: invitee, Status: r.URL.Query().Get("status"), From: from, To: to}, nil
}

func (s *Server) handleReferralRewards(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := rewardFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Referral.AdminListRewards(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleReferralExportRewards(w http.ResponseWriter, r *http.Request) {
	f, err := rewardFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.csvExport(w, r, "referral-rewards", []string{"订单号", "邀请人ID", "被邀请人ID", "消费(元)", "退款(元)", "费率(%)", "应得(元)", "状态", "发放时间", "已扣回(元)", "费率版本"},
		func(write func([]string) error) error {
			return s.Referral.ExportRewards(r.Context(), f, func(v referral.AdminReward) error {
				return write([]string{v.OrderID, strconv.FormatInt(v.InviterID, 10), strconv.FormatInt(v.InviteeID, 10), csvx.Yuan(v.OrderAmount), csvx.Yuan(v.RefundedAmount),
					strconv.FormatFloat(float64(v.RateBP)/100, 'f', 2, 64), csvx.Yuan(v.RewardAmt), v.Status, csvx.TimePtr(v.SettledAt), csvx.Yuan(v.ClawedAmount),
					strconv.FormatInt(v.RateVersionID, 10)})
			})
		})
}

// handleReferralRescan 把扫描水位拨回 days 天并立刻跑一轮发现 + 发放（Rescan 以拨回后的水位显式调用发现，裁定 Q10）。
// 拨回提交之后的失败（failed 行数、incomplete）不算操作失败：返回 200 与 RescanResult，验证窗口照常打开。
func (s *Server) handleReferralRescan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days int    `json:"days"`
		TOTP string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := referral.CheckRescanDays(req.Days); err != nil {
		s.writeError(w, r, err)
		return
	}
	var res referral.RescanResult
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		res, err = s.Referral.Rescan(r.Context(), s.actor(r), req.Days)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
