package adminapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/httpx"
)

// fabricRoutes 集群网络（计划 13 规格 §3.1、§10.2）：全员可看；建 / 改 / 删 ops（审计 fabric.create / update / delete 在 fleet 里记）。
func (s *Server) fabricRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/fabrics", allRoles, s.handleListFabrics)
	s.handle(g, http.MethodPost, "/fabrics", opsRoles, s.handleCreateFabric)
	s.handle(g, http.MethodPut, "/fabrics/{id}", opsRoles, s.handleUpdateFabric)
	s.handle(g, http.MethodDelete, "/fabrics/{id}", opsRoles, s.handleDeleteFabric)
}

// fabricRequest POST / PUT /fabrics 的请求体（fleet.FabricInput 没有 JSON 标签）。vlan_min / vlan_max 只在 isolation = vlan 时给。
type fabricRequest struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Isolation   string `json:"isolation"`
	RDMA        bool   `json:"rdma"`
	MTU         int    `json:"mtu"`
	VLANMin     *int   `json:"vlan_min"`
	VLANMax     *int   `json:"vlan_max"`
	Description string `json:"description"`
}

func (q fabricRequest) input() fleet.FabricInput {
	return fleet.FabricInput{Name: q.Name, Kind: q.Kind, Isolation: q.Isolation, RDMA: q.RDMA, MTU: q.MTU,
		VLANMin: q.VLANMin, VLANMax: q.VLANMax, Description: q.Description}
}

// handleListFabrics {items:[{…fabric, machine_count, active_clusters, cluster_nets}]}（按 id）。
func (s *Server) handleListFabrics(w http.ResponseWriter, r *http.Request) {
	list, err := s.Fleet.ListFabrics(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleCreateFabric(w http.ResponseWriter, r *http.Request) {
	var req fabricRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.Fleet.CreateFabric(r.Context(), s.actor(r), req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, f)
}

func (s *Server) handleUpdateFabric(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req fabricRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.Fleet.UpdateFabric(r.Context(), s.actor(r), id, req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, f)
}

func (s *Server) handleDeleteFabric(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Fleet.DeleteFabric(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}
