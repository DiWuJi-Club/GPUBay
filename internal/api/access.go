package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

type portView struct {
	Port          int       `json:"port"`
	Visibility    string    `json:"visibility"`
	ForcedPrivate bool      `json:"forced_private"`
	Note          string    `json:"note"`
	URL           string    `json:"url"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Server) portView(instanceID string, p rental.Port) portView {
	url := ""
	if s.Ingress != nil {
		url = s.Ingress.PortURL(instanceID, p.Port)
	}
	return portView{Port: p.Port, Visibility: p.Visibility, ForcedPrivate: p.ForcedPrivate, Note: p.Note, URL: url, CreatedAt: p.CreatedAt}
}

func (s *Server) handleListSSHKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Identity.ListSSHKeys(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(keys))
}

func (s *Server) handleAddSSHKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	k, err := s.Identity.AddSSHKey(r.Context(), currentUser(r).ID, req.Name, req.PublicKey, clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, k)
}

func (s *Server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, r, apperr.BadRequest("ID 不正确"))
		return
	}
	if err := s.Identity.DeleteSSHKey(r.Context(), currentUser(r).ID, id, clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	pw, err := s.Rental.ResetPassword(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"password": pw})
}

func (s *Server) handleSetAccess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PasswordAuth bool `json:"password_auth"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	inst, err := s.Rental.SetPasswordAuth(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), req.PasswordAuth, clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.viewOne(r.Context(), inst)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ownedInstance 校验实例属于当前用户（只读接口用；写接口由 rental 在事务内校验）。
func (s *Server) ownedInstance(r *http.Request) (rental.Instance, error) {
	return s.Rental.GetForUser(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
}

func (s *Server) handleListPorts(w http.ResponseWriter, r *http.Request) {
	inst, err := s.ownedInstance(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ports, err := s.Rental.ListPorts(r.Context(), inst.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]portView, 0, len(ports))
	for _, p := range ports {
		out = append(out, s.portView(inst.ID, p))
	}
	writeJSON(w, http.StatusOK, list(out))
}

func pathPort(r *http.Request) (int, error) {
	n, err := strconv.Atoi(chi.URLParam(r, "port"))
	if err != nil {
		return 0, apperr.ErrPortInvalid
	}
	return n, nil
}

func (s *Server) handleAddPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port       int    `json:"port"`
		Visibility string `json:"visibility"`
		Note       string `json:"note"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	p, err := s.Rental.AddPort(r.Context(), currentUser(r).ID, id, req.Port, req.Visibility, req.Note, clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.portView(id, p))
}

func (s *Server) handleUpdatePort(w http.ResponseWriter, r *http.Request) {
	port, err := pathPort(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Visibility *string `json:"visibility"`
		Note       *string `json:"note"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	p, err := s.Rental.UpdatePort(r.Context(), currentUser(r).ID, id, port, req.Visibility, req.Note, clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.portView(id, p))
}

func (s *Server) handleDeletePort(w http.ResponseWriter, r *http.Request) {
	port, err := pathPort(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Rental.DeletePort(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), port, clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}

func (s *Server) handleSSHSessions(w http.ResponseWriter, r *http.Request) {
	inst, err := s.ownedInstance(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sessions, err := s.Rental.ListSSHSessions(r.Context(), inst.ID, 20)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	type view struct {
		AuthMethod string     `json:"auth_method"`
		ClientIP   string     `json:"client_ip"`
		StartedAt  time.Time  `json:"started_at"`
		EndedAt    *time.Time `json:"ended_at"`
	}
	out := make([]view, 0, len(sessions))
	for _, x := range sessions {
		out = append(out, view{AuthMethod: x.AuthMethod, ClientIP: x.ClientIP, StartedAt: x.StartedAt, EndedAt: x.EndedAt})
	}
	writeJSON(w, http.StatusOK, list(out))
}
