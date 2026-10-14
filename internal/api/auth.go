package api

import "net/http"

func (s *Server) setSession(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: maxAge,
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
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	token, u, err := s.Identity.Login(r.Context(), req.Phone, req.Code)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.setSession(w, token, 30*24*3600)
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.setSession(w, "", -1)
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.Identity.Logout(r.Context(), c.Value); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	ok(w)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUser(r))
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
