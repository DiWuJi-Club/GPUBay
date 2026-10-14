package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/rental"
)

type instanceView struct {
	ID               string     `json:"id"`
	State            string     `json:"state"`
	ProductID        int64      `json:"product_id"`
	ImageID          int64      `json:"image_id"`
	Suspended        bool       `json:"suspended"`
	ExpiresAt        *time.Time `json:"expires_at"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	PendingSeconds   int64      `json:"pending_seconds"`
	RetainUntil      *time.Time `json:"retain_until"` // 停机后数据保留截止时间
	SSHCommand       string     `json:"ssh_command"`
	CreatedAt        time.Time  `json:"created_at"`
}

func (s *Server) view(i rental.Instance) instanceView {
	now := s.Clock.Now()
	v := instanceView{
		ID: i.ID, State: i.State, ProductID: i.ProductID, ImageID: i.ImageID, Suspended: i.Suspended(),
		ExpiresAt: i.ExpiresAt, RemainingSeconds: int64(i.Remaining(now) / time.Second),
		PendingSeconds: i.PendingSeconds, CreatedAt: i.CreatedAt,
	}
	if i.State == rental.StateRunning {
		v.SSHCommand = fmt.Sprintf("ssh %s@ssh.%s", i.ID, s.Domain)
	}
	if i.State == rental.StateStopped && i.StoppedAt != nil && i.PendingSeconds == 0 {
		t := i.StoppedAt.Add(rental.RetainAfterStop)
		v.RetainUntil = &t
	}
	return v
}

func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProductID int64  `json:"product_id"`
		Duration  string `json:"duration"`
		ImageID   int64  `json:"image_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	ord, inst, err := s.Rental.Place(r.Context(), rental.PlaceRequest{
		UserID: currentUser(r).ID, ProductID: req.ProductID, ImageID: req.ImageID, Duration: catalog.Duration(req.Duration),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"order": ord, "instance": s.view(inst)})
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := s.Rental.ListOrders(r.Context(), currentUser(r).ID, 50)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(orders))
}

func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := s.Rental.ListForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]instanceView, 0, len(insts))
	for _, i := range insts {
		views = append(views, s.view(i))
	}
	writeJSON(w, http.StatusOK, list(views))
}

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := s.Rental.GetForUser(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(inst))
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	pw, err := s.Rental.Password(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"password": pw})
}

func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Duration string `json:"duration"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	ord, err := s.Rental.Renew(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), catalog.Duration(req.Duration))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": ord})
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	if err := s.Rental.Release(r.Context(), currentUser(r).ID, chi.URLParam(r, "id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}
