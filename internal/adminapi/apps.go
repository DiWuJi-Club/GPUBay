package adminapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
)

// appRoutes 应用目录与部署排障（规格 §10.2，角色见 D8）：super 维护目录、发布要动态码；ops 预拉取与强制卸载；其余只读。
func (s *Server) appRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/apps", allRoles, s.handleListApps)
	s.handle(g, http.MethodPost, "/apps", superOnly, s.handleCreateApp)
	s.handle(g, http.MethodPut, "/apps/{id}", superOnly, s.handleUpdateApp)
	s.handle(g, http.MethodDelete, "/apps/{id}", superOnly, s.handleDeleteApp)
	s.handle(g, http.MethodGet, "/apps/{id}/versions", allRoles, s.handleListVersions)
	s.handle(g, http.MethodPost, "/apps/{id}/versions", superOnly, s.handleCreateVersion)
	s.handle(g, http.MethodPut, "/apps/{id}/versions/{vid}", superOnly, s.handleUpdateVersion)
	s.handle(g, http.MethodPost, "/apps/{id}/versions/{vid}/publish", superOnly, s.handlePublishVersion)
	s.handle(g, http.MethodPost, "/apps/{id}/versions/{vid}/prepull", opsRoles, s.handlePrepullVersion)
	s.handle(g, http.MethodGet, "/apps/{id}/versions/{vid}/script", allRoles, s.handleVersionScript)
	s.handle(g, http.MethodGet, "/deployments", allRoles, s.handleListDeployments)
	s.handle(g, http.MethodGet, "/deployments/{dep}", allRoles, s.handleDeployment)
	s.handle(g, http.MethodPost, "/deployments/{dep}/force-remove", opsRoles, s.handleForceRemoveDeployment)
}

// ---- 应用 ----

type appRequest struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Icon        string `json:"icon"`
	Sort        int    `json:"sort"`
	Status      string `json:"status"`
}

func (q appRequest) input() appdeploy.AppInput {
	return appdeploy.AppInput{Slug: q.Slug, Name: q.Name, Summary: q.Summary, Description: q.Description, Category: q.Category,
		Icon: q.Icon, Sort: q.Sort, Status: q.Status}
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	list, err := s.AppCatalog.ListAppsAdmin(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req appRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	a, err := s.AppCatalog.CreateApp(r.Context(), s.actor(r), req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, a)
}

// handleUpdateApp 整体更新展示信息与状态；slug 不可改（请求里的 slug 被忽略）。
func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req appRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	a, err := s.AppCatalog.UpdateApp(r.Context(), s.actor(r), id, req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.AppCatalog.DeleteApp(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

// ---- 版本 ----

// versionRequest 建 / 改版本的请求体。字段用指针区分「省略」与「空值」：
//   - POST：省略即零值（copy_from 时由被复制版本补齐 image_ids / spec / script）。
//   - PUT：省略的字段沿用库中当前值（读取时刻）。已发布版本只能改 changelog 或 status=retired，
//     所以只改说明 / 下线时只发 {changelog} 或 {status: "retired"} 即可；整版回传（含当前 status）也视为未改。
type versionRequest struct {
	Version   *string         `json:"version"`
	ImageIDs  *[]int64        `json:"image_ids"`
	Spec      *appdeploy.Spec `json:"spec"`
	Script    *string         `json:"script"`
	Changelog *string         `json:"changelog"`
	CopyFrom  *int64          `json:"copy_from"`
	Status    string          `json:"status"`
}

// input 把请求合成 VersionInput；base 非空时省略的字段取 base 的值。
func (q versionRequest) input(base *appdeploy.Version) appdeploy.VersionInput {
	var in appdeploy.VersionInput
	if base != nil {
		in = appdeploy.VersionInput{Version: base.Version, ImageIDs: base.ImageIDs, Spec: base.Spec, Script: base.Script, Changelog: base.Changelog}
	}
	if q.Version != nil {
		in.Version = *q.Version
	}
	if q.ImageIDs != nil {
		in.ImageIDs = *q.ImageIDs
	}
	if q.Spec != nil {
		in.Spec = *q.Spec
	}
	if q.Script != nil {
		in.Script = *q.Script
	}
	if q.Changelog != nil {
		in.Changelog = *q.Changelog
	}
	in.CopyFrom = q.CopyFrom
	in.Status = q.Status
	if base != nil && in.Status == base.Status {
		in.Status = "" // 回传当前状态 = 不改状态
	}
	return in
}

// versionView 是后台版本列表的一项（Version 不含脚本；脚本走单独接口）。
type versionView struct {
	appdeploy.Version
	Coverage []appdeploy.Coverage `json:"coverage"`
}

func validateOnly(r *http.Request) bool { return r.URL.Query().Get("validate") == "1" }

// appVersion 读应用 id 下的版本 vid（含脚本）；版本不属于该应用 → not_found。
func (s *Server) appVersion(r *http.Request) (int64, appdeploy.Version, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return 0, appdeploy.Version{}, err
	}
	vid, err := pathID(r, "vid")
	if err != nil {
		return 0, appdeploy.Version{}, err
	}
	v, err := s.AppCatalog.GetVersion(r.Context(), vid)
	if err != nil {
		return 0, appdeploy.Version{}, err
	}
	if v.AppID != id {
		return 0, appdeploy.Version{}, apperr.ErrNotFound
	}
	return id, v, nil
}

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, err := s.AppCatalog.GetApp(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	list, err := s.AppCatalog.ListVersions(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// 覆盖率只对草稿 / 已发布版本有意义（已下线的不再部署），一次批量算完
	active := make([]appdeploy.Version, 0, len(list))
	for _, v := range list {
		if v.Status != appdeploy.StatusRetired {
			active = append(active, v)
		}
	}
	cov, err := s.AppCatalog.VersionsCoverage(r.Context(), active)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]versionView, 0, len(list))
	for _, v := range list {
		c := cov[v.ID]
		if c == nil {
			c = []appdeploy.Coverage{}
		}
		views = append(views, versionView{Version: v, Coverage: c})
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(views))
}

// writeValidated 是 ?validate=1 的响应：只校验不保存。
func (s *Server) writeValidated(w http.ResponseWriter, r *http.Request, hash string, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "spec_hash": hash})
}

func (s *Server) handleCreateVersion(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req versionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Status != "" && req.Status != appdeploy.StatusDraft {
		s.writeError(w, r, apperr.BadRequest("新版本只能是草稿；发布请使用发布操作"))
		return
	}
	in := req.input(nil)
	if validateOnly(r) {
		hash, err := s.AppCatalog.ValidateVersion(r.Context(), id, in)
		s.writeValidated(w, r, hash, err)
		return
	}
	v, err := s.AppCatalog.CreateVersion(r.Context(), s.actor(r), id, in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, v)
}

func (s *Server) handleUpdateVersion(w http.ResponseWriter, r *http.Request) {
	id, cur, err := s.appVersion(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req versionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.CopyFrom != nil {
		s.writeError(w, r, apperr.BadRequest("copy_from 只用于新建版本"))
		return
	}
	in := req.input(&cur)
	if validateOnly(r) { // 与真正修改同一规则：已发布版本改冻结字段 / 已下线版本 → app_version_frozen
		hash, err := s.AppCatalog.ValidateVersionUpdate(r.Context(), id, cur.ID, in)
		s.writeValidated(w, r, hash, err)
		return
	}
	v, err := s.AppCatalog.UpdateVersion(r.Context(), s.actor(r), id, cur.ID, in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

// handlePublishVersion 发布草稿（super + 动态码）：先确认版本存在且是草稿，明显无法发布的请求不消耗动态码；
// 发布成功后才打开二次验证窗口（withStepUp）。
func (s *Server) handlePublishVersion(w http.ResponseWriter, r *http.Request) {
	id, cur, err := s.appVersion(r)
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
	if cur.Status != appdeploy.StatusDraft {
		s.writeError(w, r, apperr.ErrInvalidState)
		return
	}
	var out appdeploy.Version
	var machines int
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		out, machines, err = s.AppCatalog.PublishVersion(r.Context(), s.actor(r), id, cur.ID)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"version": out, "prepull_machines": machines})
}

func (s *Server) handlePrepullVersion(w http.ResponseWriter, r *http.Request) {
	id, cur, err := s.appVersion(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	n, err := s.AppCatalog.Prepull(r.Context(), s.actor(r), id, cur.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"machines": n})
}

// handleVersionScript 返回脚本原文（只在后台可见，控制台从不返回脚本；读取不审计）。
func (s *Server) handleVersionScript(w http.ResponseWriter, r *http.Request) {
	_, v, err := s.appVersion(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"script": v.Script})
}
