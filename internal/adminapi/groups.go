package adminapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
)

func (s *Server) groupRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/groups", allRoles, s.handleListGroups)
	s.handle(g, http.MethodPost, "/groups", opsRoles, s.handleCreateGroup)
	s.handle(g, http.MethodPut, "/groups/{id}", opsRoles, s.handleUpdateGroup)
	s.handle(g, http.MethodDelete, "/groups/{id}", opsRoles, s.handleDeleteGroup)
	s.handle(g, http.MethodPost, "/groups/{id}/machines", opsRoles, s.handleAssignMachines)
	s.handle(g, http.MethodDelete, "/groups/{id}/machines", opsRoles, s.handleUnassignMachines)
	s.handle(g, http.MethodPut, "/machines/{id}", opsRoles, s.handleUpdateMachine)
}

type groupRequest struct {
	Name        string `json:"name"`
	Region      string `json:"region"`
	Description string `json:"description"`
	Sort        int    `json:"sort"`
	Visible     *bool  `json:"visible"`
	Purpose     string `json:"purpose"` // 可选：建组只能是 rental；改组空 = 不改（池的用途不可改，规格 §3.3）
}

func (q groupRequest) input() fleet.GroupInput {
	visible := true
	if q.Visible != nil {
		visible = *q.Visible
	}
	return fleet.GroupInput{Name: q.Name, Region: q.Region, Description: q.Description, Sort: q.Sort, Visible: visible, Purpose: q.Purpose}
}

// groupView 列表行：fleet.GroupRow 加未开通预订数（rental 统计）。
type groupView struct {
	fleet.GroupRow
	ReservedUpcoming int `json:"reserved_upcoming"`
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Fleet.ListGroups(r.Context(), false, nil)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	counts, err := s.Rental.ReservedCountsByGroup(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]groupView, 0, len(rows))
	for _, g := range rows {
		out = append(out, groupView{GroupRow: g, ReservedUpcoming: counts[g.ID]})
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(out))
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req groupRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	g, err := s.Fleet.CreateGroup(r.Context(), s.actor(r), req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, g)
}

func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req groupRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	g, err := s.Fleet.UpdateGroup(r.Context(), s.actor(r), id, req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, g)
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Rental.AdminDeleteGroup(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

type machineIDsRequest struct {
	MachineIDs []int64 `json:"machine_ids"`
}

func (s *Server) handleAssignMachines(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req machineIDsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	// 加入别的机组 = 移出 Token 服务池：池里有节点的机器不能被挪走
	if err := s.checkPoolLeave(r.Context(), req.MachineIDs, nil, &id); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Fleet.AssignMachines(r.Context(), s.actor(r), id, req.MachineIDs); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleUnassignMachines(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req machineIDsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.checkPoolLeave(r.Context(), req.MachineIDs, &id, nil); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Fleet.UnassignMachines(r.Context(), s.actor(r), id, req.MachineIDs); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

// handleUpdateMachine 只认 group_id（键存在即改组，null = 移出）与 display_name 两个字段。
func (s *Server) handleUpdateMachine(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var raw map[string]any
	if err := httpx.DecodeJSON(r, &raw); err != nil {
		s.writeError(w, r, err)
		return
	}
	var in fleet.MachineUpdate
	if v, ok := raw["group_id"]; ok {
		in.SetGroup = true
		if v != nil {
			f, isNum := v.(float64)
			if !isNum || f <= 0 || f != float64(int64(f)) {
				s.writeError(w, r, apperr.BadRequest("group_id 必须是正整数或 null"))
				return
			}
			gid := int64(f)
			in.GroupID = &gid
		}
	}
	if v, ok := raw["display_name"]; ok {
		str, isStr := v.(string)
		if !isStr {
			s.writeError(w, r, apperr.BadRequest("display_name 必须是字符串"))
			return
		}
		in.DisplayName = &str
	}
	if !in.SetGroup && in.DisplayName == nil {
		s.writeError(w, r, apperr.BadRequest("请提供 group_id 或 display_name"))
		return
	}
	if in.SetGroup {
		if err := s.checkPoolLeave(r.Context(), []int64{id}, nil, in.GroupID); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	m, err := s.Fleet.UpdateMachine(r.Context(), s.actor(r), id, in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}
