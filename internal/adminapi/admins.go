package adminapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/platform/httpx"
)

func (s *Server) accountRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/me", allRoles, s.handleMe)
	s.handle(g, http.MethodPost, "/me/password", allRoles, s.handleChangePassword)
	s.handle(g, http.MethodGet, "/admins", superOnly, s.handleListAdmins)
	s.handle(g, http.MethodPost, "/admins", superOnly, s.handleCreateAdmin)
	s.handle(g, http.MethodPut, "/admins/{id}", superOnly, s.handleUpdateAdmin)
	s.handle(g, http.MethodPost, "/admins/{id}/reset-totp", superOnly, s.handleResetTOTP)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, currentSession(r).Admin)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Admins.ChangePassword(r.Context(), currentSession(r), req.Old, req.New, httpx.ClientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleListAdmins(w http.ResponseWriter, r *http.Request) {
	list, err := s.Admins.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleCreateAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
		TOTP     string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.stepUp(r, req.TOTP); err != nil {
		s.writeError(w, r, err)
		return
	}
	a, err := s.Admins.Create(r.Context(), s.actor(r), req.Username, req.Password, admin.Role(req.Role))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, a)
}

func (s *Server) handleUpdateAdmin(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Role   *string `json:"role"`
		Status *string `json:"status"`
		TOTP   string  `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.stepUp(r, req.TOTP); err != nil {
		s.writeError(w, r, err)
		return
	}
	var role *admin.Role
	if req.Role != nil {
		v := admin.Role(*req.Role)
		role = &v
	}
	a, err := s.Admins.Update(r.Context(), s.actor(r), id, role, req.Status)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}

func (s *Server) handleResetTOTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		TOTP string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.stepUp(r, req.TOTP); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Admins.ResetTOTP(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}
