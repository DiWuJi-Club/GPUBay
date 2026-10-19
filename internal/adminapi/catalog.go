package adminapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/httpx"
)

func (s *Server) catalogRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/products", allRoles, s.handleAdminProducts)
	s.handle(g, http.MethodPut, "/products/{id}/prices/{duration}", superOnly, s.handleSetPrice)
	s.handle(g, http.MethodGet, "/images", allRoles, s.handleAdminImages)
	s.handle(g, http.MethodPost, "/images", superOnly, s.handleCreateImage)
	s.handle(g, http.MethodPut, "/images/{id}", superOnly, s.handleUpdateImage)
	s.handle(g, http.MethodGet, "/audit", allRoles, s.handleAudit)
	s.handle(g, http.MethodGet, "/dashboard", allRoles, s.handleDashboard)
}

func (s *Server) handleAdminProducts(w http.ResponseWriter, r *http.Request) {
	list, err := s.Catalog.AdminProducts(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleSetPrice(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Amount  int64  `json:"amount"`
		Enabled bool   `json:"enabled"`
		TOTP    string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	var p any
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		p, err = s.Catalog.SetPrice(r.Context(), s.actor(r), id, catalog.Duration(chi.URLParam(r, "duration")), req.Amount, req.Enabled)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (s *Server) handleAdminImages(w http.ResponseWriter, r *http.Request) {
	list, err := s.Catalog.AdminImages(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

type imageRequest struct {
	Name        string `json:"name"`
	Ref         string `json:"ref"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Sort        int    `json:"sort"`
}

func (q imageRequest) input() catalog.ImageInput {
	return catalog.ImageInput{Name: q.Name, Ref: q.Ref, Description: q.Description, Enabled: q.Enabled, Sort: q.Sort}
}

func (s *Server) handleCreateImage(w http.ResponseWriter, r *http.Request) {
	var req imageRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	img, err := s.Catalog.CreateImage(r.Context(), s.actor(r), req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.broadcastPull(r, img, nil)
	httpx.WriteJSON(w, http.StatusCreated, img)
}

// broadcastPull 镜像新建启用、由停用转启用、或启用中改 ref 时向所有在线机器预拉取；失败不阻塞上架。
func (s *Server) broadcastPull(r *http.Request, img catalog.AdminImage, prev *catalog.AdminImage) {
	if !img.Enabled || (prev != nil && prev.Enabled && prev.Ref == img.Ref) {
		return
	}
	if n, err := s.Fleet.BroadcastPullImage(r.Context(), img.Ref); err != nil {
		s.Log.Warn("broadcast pull_image failed", "image", img.ID, "err", err)
	} else {
		s.Log.Info("pull_image broadcast", "image", img.ID, "machines", n)
	}
}

func (s *Server) handleUpdateImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req imageRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	var prev *catalog.AdminImage
	if all, err := s.Catalog.AdminImages(r.Context()); err == nil {
		for k := range all {
			if all[k].ID == id {
				prev = &all[k]
			}
		}
	}
	img, err := s.Catalog.UpdateImage(r.Context(), s.actor(r), id, req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.broadcastPull(r, img, prev)
	httpx.WriteJSON(w, http.StatusOK, img)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	actorID, err := queryInt(r, "actor_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	from, to, err := parseRange(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	list, total, err := audit.List(r.Context(), s.Fleet.Pool(), audit.Filter{ActorID: actorID, Action: q.Get("action"),
		TargetType: q.Get("target_type"), TargetID: q.Get("target_id"), From: from, To: to}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	admins, err := s.Admins.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	names := make(map[int64]string, len(admins))
	for _, a := range admins {
		names[a.ID] = a.Username
	}
	views := make([]auditView, 0, len(list))
	for _, l := range list {
		v := auditView{Log: l}
		if l.ActorType == "admin" {
			v.ActorName = names[l.ActorID]
		}
		views = append(views, v)
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(views, total))
}

// auditView 在审计记录上补操作人用户名（admin 之外的 actor 或已不存在的管理员为空串）。
type auditView struct {
	audit.Log
	ActorName string `json:"actor_name"`
}
