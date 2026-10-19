package adminapi

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
)

// 部署一律经 appdeploy.AdminView 输出（不直接序列化 Deployment：它带 SecretEnvEnc）；敏感变量只给 secret_env = {NAME: true}，
// log_tail 在 Runner 回写时已按 secret 值脱敏。

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	appID, err := queryInt(r, "app_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	userID, err := queryInt(r, "user_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	list, total, err := s.Apps.AdminList(r.Context(), appdeploy.ListFilter{State: q.Get("state"), AppID: appID, UserID: userID,
		InstanceID: q.Get("instance_id")}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views, err := s.Apps.AdminViews(r.Context(), list)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(views, total))
}

func (s *Server) handleDeployment(w http.ResponseWriter, r *http.Request) {
	d, err := s.Apps.Get(r.Context(), chi.URLParam(r, "dep"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views, err := s.Apps.AdminViews(r.Context(), []appdeploy.Deployment{d})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	events, err := s.Apps.Events(r.Context(), d.ID, 100)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deployment": views[0], "events": events, "log_tail": d.LogTail})
}

// handleForceRemoveDeployment 平台强制卸载（ops，必须填原因）：事件「平台卸载：<原因>」，审计 app.deployment.force_remove 记在当前管理员名下。
func (s *Server) handleForceRemoveDeployment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		s.writeError(w, r, apperr.BadRequest("请填写原因"))
		return
	}
	d, err := s.Apps.AdminForceRemove(r.Context(), s.actor(r), chi.URLParam(r, "dep"), req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views, err := s.Apps.AdminViews(r.Context(), []appdeploy.Deployment{d})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, views[0])
}
