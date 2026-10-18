package adminapi

import (
	"net/http"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
)

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: token, Path: s.cookiePath(), MaxAge: maxAge,
		HttpOnly: true, Secure: s.SecureCookie, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	res, err := s.Admins.Login(r.Context(), req.Username, req.Password, httpx.ClientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.setSessionCookie(w, res.Token, int(admin.SessionTTL/time.Second))
	next := "totp"
	if res.NeedTOTPSetup {
		next = "totp_setup"
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"next": next, "otpauth_uri": res.OTPAuthURI})
}

func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	a, codes, err := s.Admins.ConfirmTOTPSetup(r.Context(), s.sessionToken(r), req.Code, httpx.ClientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"admin": a, "recovery_codes": codes})
}

func (s *Server) handleTOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code         string `json:"code"`
		RecoveryCode string `json:"recovery_code"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if (req.Code == "") == (req.RecoveryCode == "") {
		s.writeError(w, r, apperr.BadRequest("请填写动态码或恢复码（二选一）"))
		return
	}
	var a admin.Admin
	var err error
	if req.Code != "" {
		a, err = s.Admins.VerifyTOTP(r.Context(), s.sessionToken(r), req.Code, httpx.ClientIP(r))
	} else {
		a, err = s.Admins.VerifyRecoveryCode(r.Context(), s.sessionToken(r), req.RecoveryCode, httpx.ClientIP(r))
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"admin": a})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := s.sessionToken(r); token != "" {
		if err := s.Admins.Logout(r.Context(), token); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	s.setSessionCookie(w, "", -1)
	httpx.OK(w)
}
