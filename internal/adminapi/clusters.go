package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/rental"
)

// clusterRoutes 实例集群（计划 13 规格 §10.2）：列表 / 详情全员可看；延长、重新下发 ops（不要求动态码，与实例 extend 一致）。
// 换机仍走 POST /instances/{id}/migrate（成员的候选机器由 rental 限定在同一 fabric / 集群网段）。
func (s *Server) clusterRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/clusters", allRoles, s.handleListClusters)
	s.handle(g, http.MethodGet, "/clusters/{id}", allRoles, s.handleCluster)
	s.handle(g, http.MethodPost, "/clusters/{id}/extend", opsRoles, s.handleExtendCluster)
	s.handle(g, http.MethodPost, "/clusters/{id}/resync", opsRoles, s.handleResyncCluster)
}

// handleListClusters GET /clusters?state=&user_id=&fabric_id=&system=&page=&page_size= → {items:[ClusterView], total}（新的在前）。
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	f := rental.ClusterFilter{State: q.Get("state")}
	uid, err := queryInt(r, "user_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	fid, err := queryInt(r, "fabric_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if f.System, err = parseBoolParam(q.Get("system"), "system"); err != nil {
		s.writeError(w, r, err)
		return
	}
	if uid > 0 {
		f.UserID = &uid
	}
	if fid > 0 {
		f.FabricID = &fid
	}
	list, total, err := s.Rental.AdminListClusters(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

// clusterEventLimit 集群详情合并的成员实例事件条数。
const clusterEventLimit = 50

// handleCluster GET /clusters/{id} → {cluster: ClusterView, app: 集群应用 | null, events: 最近 50 条成员实例事件（新的在前）}。
func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	v, err := s.Rental.GetClusterView(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	cd, err := s.Apps.LiveClusterDeployment(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var app *clusterAppView
	if cd != nil {
		if app, err = s.clusterAppView(ctx, *cd); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	events, err := s.Rental.ClusterEvents(ctx, id, clusterEventLimit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if events == nil {
		events = []rental.ClusterEvent{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"cluster": v, "app": app, "events": events})
}

// handleExtendCluster POST /clusters/{id}/extend {seconds, reason} → ClusterView；审计 cluster.extend（rental 记）。
func (s *Server) handleExtendCluster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seconds int64  `json:"seconds"`
		Reason  string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.Rental.AdminExtendCluster(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.Seconds, req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

// handleResyncCluster POST /clusters/{id}/resync（无请求体字段）→ ClusterView（version + 1，向在位成员重新下发 configure_cluster）；审计 cluster.resync。
func (s *Server) handleResyncCluster(w http.ResponseWriter, r *http.Request) {
	v, err := s.Rental.AdminResyncCluster(r.Context(), s.actor(r), chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

// clusterAppMemberView 集群应用的一个成员部署：角色、聚合用的状态摘要与完整后台部署视图（读不到为 null）。
type clusterAppMemberView struct {
	Rank         int                  `json:"rank"`
	Role         string               `json:"role"`
	InstanceID   string               `json:"instance_id"`
	Current      bool                 `json:"current"`
	DeploymentID string               `json:"deployment_id"`
	State        string               `json:"state"`
	Health       string               `json:"health"`
	Phase        string               `json:"phase"`
	Progress     int                  `json:"progress"`
	Deployment   *appdeploy.AdminView `json:"deployment"`
}

// clusterAppView 后台的集群应用（字段与控制台 ClusterAppView 对齐，另带 cluster_id / user_id / state / 时间与成员的完整部署视图）。
type clusterAppView struct {
	ID        string                 `json:"id"`
	ClusterID string                 `json:"cluster_id"`
	UserID    int64                  `json:"user_id"`
	AppID     int64                  `json:"app_id"`
	AppSlug   string                 `json:"app_slug"`
	AppName   string                 `json:"app_name"`
	VersionID int64                  `json:"version_id"`
	Version   string                 `json:"version"`
	State     string                 `json:"state"`
	Status    string                 `json:"status"`
	Health    string                 `json:"health"`
	Phase     string                 `json:"phase"`
	Progress  int                    `json:"progress"`
	URL       *string                `json:"url"` // head（rank0 当前成员）的访问地址；端口不在时为 null
	Members   []clusterAppMemberView `json:"members"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

func (s *Server) clusterAppView(ctx context.Context, cd appdeploy.ClusterDeployment) (*clusterAppView, error) {
	deps := make([]appdeploy.Deployment, len(cd.Members))
	for i, m := range cd.Members {
		deps[i] = m.Deployment
	}
	views, err := s.Apps.AdminViews(ctx, deps)
	if err != nil {
		return nil, err
	}
	out := &clusterAppView{ID: cd.ID, ClusterID: cd.ClusterID, UserID: cd.UserID, AppID: cd.AppID, VersionID: cd.AppVersionID,
		State: cd.State, Status: cd.Status, Health: cd.Health, Phase: cd.Phase, Progress: cd.Progress,
		Members: make([]clusterAppMemberView, 0, len(cd.Members)), CreatedAt: cd.CreatedAt, UpdatedAt: cd.UpdatedAt}
	for i, m := range cd.Members {
		d := m.Deployment
		mv := clusterAppMemberView{Rank: m.Rank, Role: m.Role, InstanceID: m.InstanceID, Current: m.Current, DeploymentID: d.ID,
			State: d.State, Health: d.Health, Phase: d.Phase, Progress: d.Progress, Deployment: &views[i]}
		out.Members = append(out.Members, mv)
		if out.AppSlug == "" || (m.Rank == 0 && m.Current) {
			out.AppSlug, out.AppName, out.Version = views[i].AppSlug, views[i].AppName, views[i].Version
		}
		if m.Rank == 0 && m.Current && out.URL == nil {
			u, err := s.Apps.PortURLFor(ctx, d)
			if err != nil {
				return nil, err
			}
			if u != "" {
				out.URL = &u
			}
		}
	}
	return out, nil
}

// instanceClusterView 实例详情的 cluster 区块：所属集群的视图 + 本实例的 rank。
type instanceClusterView struct {
	rental.ClusterView
	Rank int `json:"rank"`
}

// instanceCluster 成员实例 → 集群区块；非成员 → nil（JSON null）。
func (s *Server) instanceCluster(ctx context.Context, inst rental.Instance) (*instanceClusterView, error) {
	if inst.ClusterID == nil {
		return nil, nil
	}
	v, err := s.Rental.GetClusterView(ctx, *inst.ClusterID)
	if err != nil {
		return nil, err
	}
	out := &instanceClusterView{ClusterView: v}
	if inst.ClusterRank != nil {
		out.Rank = *inst.ClusterRank
	}
	return out, nil
}
