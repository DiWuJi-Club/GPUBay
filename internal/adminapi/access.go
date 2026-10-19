package adminapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/rental"
)

func (s *Server) handleInstanceSSHSessions(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Rental.AdminListSSHSessions(r.Context(), chi.URLParam(r, "id"), p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func adminPathPort(r *http.Request) (int, error) {
	n, err := strconv.Atoi(chi.URLParam(r, "port"))
	if err != nil {
		return 0, apperr.ErrPortInvalid
	}
	return n, nil
}

func (s *Server) handleForcePortPrivate(w http.ResponseWriter, r *http.Request) {
	port, err := adminPathPort(r)
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
	id := chi.URLParam(r, "id")
	p, err := s.Rental.AdminForcePortPrivate(r.Context(), s.actor(r), id, port, req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.portViews(id, []rental.Port{p})[0])
}

func (s *Server) handleAdminDeletePort(w http.ResponseWriter, r *http.Request) {
	port, err := adminPathPort(r)
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
	if err := s.Rental.AdminDeletePort(r.Context(), s.actor(r), chi.URLParam(r, "id"), port, req.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}
