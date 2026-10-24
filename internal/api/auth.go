package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"spark-cloud/internal/identity"
)

func (s *Server) setSession(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: token, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) handleSendCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Identity.SendLoginCode(r.Context(), req.Phone, clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone      string `json:"phone"`
		Code       string `json:"code"`
		InviteCode string `json:"invite_code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	// X-Device-Id 原样传入（identity 只认 UUID 形态，其余视同未提供）；UA 由 Bind 清洗。
	token, res, err := s.Identity.LoginWith(r.Context(), identity.LoginRequest{Phone: req.Phone, Code: req.Code, InviteCode: sanitizeInviteCode(req.InviteCode),
		IP: clientIP(r), DeviceID: r.Header.Get("X-Device-Id"), UserAgent: r.UserAgent()})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.setSession(w, token, 30*24*3600)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.setSession(w, "", -1)
	if c, err := r.Cookie(s.cookieName()); err == nil {
		if err := s.Identity.Logout(r.Context(), c.Value); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	ok(w)
}

type meView struct {
	identity.User
	ReferralMonthSettled int64 `json:"referral_month_settled,omitempty"` // 计划 8：「我的」页入口小字；0 时省略
}

const touchSessionTimeout = 2 * time.Second

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	// 会话启动时补记设备与最近登录（计划 8 终审 I1，同设备 / 同 IP 判定用）：失败只记日志，/me 照常返回；
	// 短超时，数据库慢时不拖住登录态判断。
	tctx, cancel := context.WithTimeout(r.Context(), touchSessionTimeout)
	if err := s.Identity.TouchSession(tctx, u.ID, clientIP(r), r.Header.Get("X-Device-Id")); err != nil {
		s.Log.Error("touch session failed", "err", err, "user_id", u.ID)
	}
	cancel()
	v := meView{User: u}
	if s.Referral != nil {
		// 入口小字是锦上添花：查询失败只记日志、省略字段，/me 本身照常返回（前端靠它判断登录态）。
		if n, err := s.Referral.MonthSettledFor(r.Context(), u.ID); err != nil {
			s.Log.Error("referral month settled failed", "err", err, "user_id", u.ID)
		} else {
			v.ReferralMonthSettled = n
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleKYCPersonal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		IDNumber string `json:"id_number"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	u, err := s.Identity.SubmitPersonalKYC(r.Context(), currentUser(r).ID, req.Name, req.IDNumber)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleKYCEnterprise(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CompanyName string `json:"company_name"`
		LicenseNo   string `json:"license_no"`
		LicenseFile string `json:"license_file"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Identity.SubmitEnterpriseKYC(r.Context(), currentUser(r).ID, req.CompanyName, req.LicenseNo, req.LicenseFile); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}

// kycRecordView 是用户查看自己最近一条实名记录的视图：不含身份证号（含脱敏）与执照文件 key。
type kycRecordView struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	RealName  string    `json:"real_name"`
	LicenseNo string    `json:"license_no,omitempty"`
	Result    string    `json:"result"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// maskName 个人姓名只保留首字，其余以 * 代替。
func maskName(name string) string {
	r := []rune(name)
	if len(r) <= 1 {
		return name
	}
	return string(r[0]) + strings.Repeat("*", len(r)-1)
}

// handleMyKYC 返回用户最近一条实名记录（含驳回原因），没有记录时 record 为 null。
func (s *Server) handleMyKYC(w http.ResponseWriter, r *http.Request) {
	records, err := s.Identity.UserKYCRecords(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if len(records) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"record": nil})
		return
	}
	k := records[0]
	v := kycRecordView{ID: k.ID, Kind: k.Kind, RealName: k.RealName, LicenseNo: k.LicenseNo, Result: k.Result, Reason: k.Reason,
		CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt}
	if k.Kind == "personal" {
		v.RealName = maskName(k.RealName)
	}
	writeJSON(w, http.StatusOK, map[string]any{"record": v})
}
