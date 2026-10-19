package api

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/platform/apperr"
)

// 应用一键部署（计划 7，规格 §10.1）。部署一律经 appdeploy 的视图输出（不序列化 Deployment：它带加密的敏感变量）；
// /instances/{id}/apps* 全部先校验实例归属，别人的实例与不存在同样 404。

const appEventsLimit = 50

// ---- 目录（匿名可访问，按 IP 限流）----

func (s *Server) handleAppCatalog(w http.ResponseWriter, r *http.Request) {
	items, err := s.Apps.CatalogViews(r.Context(), false)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(items))
}

func (s *Server) handleAppCatalogItem(w http.ResponseWriter, r *http.Request) {
	item, err := s.Apps.CatalogViewBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// ---- 实例上的部署 ----

// ownedDeployment 先校验实例归属（ownedInstance，不是主人 → not_found），再取部署（须在该实例上且属于该用户）。
// 只读接口用；写接口由 appdeploy 在事务内 LockOwned 校验。
func (s *Server) ownedDeployment(r *http.Request) (appdeploy.Deployment, error) {
	inst, err := s.ownedInstance(r)
	if err != nil {
		return appdeploy.Deployment{}, err
	}
	return s.Apps.GetForUser(r.Context(), currentUser(r).ID, inst.ID, chi.URLParam(r, "dep"))
}

// allowAppWrite 部署类写操作的每用户限流（规格 §6.2：每小时 60 次）；超限已写 rate_limited。
// 有意在解析请求体与业务校验之前计数：失败的请求（格式错、状态不允许、别人的实例）同样计入，
// 限的是写接口的调用频率，也防止用错误请求探测。读接口不计数。
func (s *Server) allowAppWrite(w http.ResponseWriter, r *http.Request) bool {
	if !s.appsLim.allow(currentUser(r).ID, s.Clock.Now()) {
		s.writeError(w, r, apperr.ErrRateLimited)
		return false
	}
	return true
}

func (s *Server) writeDeployment(w http.ResponseWriter, r *http.Request, status int, d appdeploy.Deployment, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views, err := s.Apps.Views(r.Context(), []appdeploy.Deployment{d})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, status, views[0])
}

func (s *Server) handleInstanceApps(w http.ResponseWriter, r *http.Request) {
	inst, err := s.ownedInstance(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	deps, err := s.Apps.ListLive(r.Context(), inst.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views, err := s.Apps.Views(r.Context(), deps)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(views))
}

func (s *Server) handleInstanceApp(w http.ResponseWriter, r *http.Request) {
	d, err := s.ownedDeployment(r)
	s.writeDeployment(w, r, http.StatusOK, d, err)
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	if !s.allowAppWrite(w, r) {
		return
	}
	var req struct {
		AppID     int64             `json:"app_id"`
		VersionID *int64            `json:"app_version_id"`
		Env       map[string]string `json:"env"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	d, err := s.Apps.Create(r.Context(), appdeploy.CreateRequest{UserID: currentUser(r).ID, InstanceID: chi.URLParam(r, "id"),
		AppID: req.AppID, VersionID: req.VersionID, Env: req.Env, IP: clientIP(r)})
	s.writeDeployment(w, r, http.StatusCreated, d, err)
}

func (s *Server) handleRedeployApp(w http.ResponseWriter, r *http.Request) {
	if !s.allowAppWrite(w, r) {
		return
	}
	var req struct {
		VersionID *int64          `json:"app_version_id"`
		Env       json.RawMessage `json:"env"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	// env 字段出现（且不是 null）才算改变量：不带 env = 全部沿用，"env": {} = 普通变量回到默认值、secret 沿用
	r2 := appdeploy.RedeployRequest{UserID: currentUser(r).ID, InstanceID: chi.URLParam(r, "id"), DeploymentID: chi.URLParam(r, "dep"),
		VersionID: req.VersionID, IP: clientIP(r), EnvSet: len(req.Env) > 0 && string(req.Env) != "null"}
	if r2.EnvSet {
		if err := json.Unmarshal(req.Env, &r2.Env); err != nil {
			s.writeError(w, r, apperr.BadRequest("env 格式不正确"))
			return
		}
	}
	d, err := s.Apps.Redeploy(r.Context(), r2)
	s.writeDeployment(w, r, http.StatusOK, d, err)
}

func (s *Server) handleStopApp(w http.ResponseWriter, r *http.Request) {
	if !s.allowAppWrite(w, r) {
		return
	}
	d, err := s.Apps.Stop(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), chi.URLParam(r, "dep"), clientIP(r))
	s.writeDeployment(w, r, http.StatusOK, d, err)
}

func (s *Server) handleStartApp(w http.ResponseWriter, r *http.Request) {
	if !s.allowAppWrite(w, r) {
		return
	}
	d, err := s.Apps.Start(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), chi.URLParam(r, "dep"), clientIP(r))
	s.writeDeployment(w, r, http.StatusOK, d, err)
}

func (s *Server) handleRemoveApp(w http.ResponseWriter, r *http.Request) {
	if !s.allowAppWrite(w, r) {
		return
	}
	d, err := s.Apps.Remove(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), chi.URLParam(r, "dep"), clientIP(r))
	s.writeDeployment(w, r, http.StatusOK, d, err)
}

func (s *Server) handleAppLog(w http.ResponseWriter, r *http.Request) {
	d, err := s.ownedDeployment(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	res, err := s.Apps.Log(r.Context(), currentUser(r).ID, d.InstanceID, d.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleAppEvents 最近 50 条事件，按 id 升序（时间顺序）。
func (s *Server) handleAppEvents(w http.ResponseWriter, r *http.Request) {
	d, err := s.ownedDeployment(r) // Events 本身不校验归属
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	events, err := s.Apps.Events(r.Context(), d.ID, appEventsLimit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	slices.Reverse(events)
	writeJSON(w, http.StatusOK, list(events))
}
