package api

import (
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/platform/paging"
)

const defaultReferralLimit = 60 // 匿名查码按 IP 每分钟

// maxInviteCodeLen 是登录请求里 invite_code 的字节上限：8 位码加少量粘贴带入的空白足够；
// 超长或非法 UTF-8 视为无效码（不截断——截断可能把垃圾变成有效码），注册照常进行。
const maxInviteCodeLen = 32

// invalidInviteCode 是格式错误的 invite_code 的替身：非空（响应仍带 invite.reason），且 NormalizeCode 必然拒绝。
const invalidInviteCode = "!"

// sanitizeInviteCode：空串原样（= 未带码）；格式错误换成必然无效的替身，从不报错。
func sanitizeInviteCode(c string) string {
	if c == "" {
		return ""
	}
	if len(c) > maxInviteCodeLen || !utf8.ValidString(c) {
		return invalidInviteCode
	}
	return c
}

func (s *Server) referralRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.referralLim.allow(clientIP(r), s.Clock.Now()) {
			s.writeError(w, r, apperr.ErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleReferralCode 匿名查码（登录页显示「通过 138****1234 的邀请注册」）：只返回有效与否和脱敏手机号，
// 不存在 / 冻结 / 码主封禁一律 {valid:false, inviter:""}，不区分原因。
func (s *Server) handleReferralCode(w http.ResponseWriter, r *http.Request) {
	valid, inviter, err := s.Referral.LookupCode(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": valid, "inviter": inviter})
}

func (s *Server) handleReferralMe(w http.ResponseWriter, r *http.Request) {
	me, err := s.Referral.Me(r.Context(), currentUser(r).ID, s.PublicBaseURL)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, me)
}

func (s *Server) handleReferralInvitees(w http.ResponseWriter, r *http.Request) {
	p, err := paging.Parse(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Referral.Invitees(r.Context(), currentUser(r).ID, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleReferralRewards(w http.ResponseWriter, r *http.Request) {
	p, err := paging.Parse(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Referral.Rewards(r.Context(), currentUser(r).ID, r.URL.Query().Get("status"), p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, httpx.Page(list, total))
}
