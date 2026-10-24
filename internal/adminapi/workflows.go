package adminapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/comfywf"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/storage"
)

// workflowRoutes ComfyUI 工作流目录（计划 11 规格 §10.2，角色见 D6）：全部角色可读；super 维护目录与版本；
// 发布版本 = super + 动态码。没有 ops 专属操作（强制移除工作流不做：ops 用计划 7 的强制卸载应用）。
func (s *Server) workflowRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/workflows", allRoles, s.handleListWorkflows)
	s.handle(g, http.MethodPost, "/workflows", superOnly, s.handleCreateWorkflow)
	s.handle(g, http.MethodGet, "/workflows/{id}", allRoles, s.handleWorkflow)
	s.handle(g, http.MethodPut, "/workflows/{id}", superOnly, s.handleUpdateWorkflow)
	s.handle(g, http.MethodDelete, "/workflows/{id}", superOnly, s.handleDeleteWorkflow)
	s.handle(g, http.MethodGet, "/workflows/{id}/cover", allRoles, s.handleWorkflowCover)
	s.handle(g, http.MethodPost, "/workflows/{id}/cover", superOnly, s.handleUploadWorkflowCover)
	s.handle(g, http.MethodGet, "/workflows/{id}/installs", allRoles, s.handleWorkflowInstalls)
	s.handle(g, http.MethodGet, "/workflows/{id}/versions", allRoles, s.handleListWorkflowVersions)
	s.handle(g, http.MethodPost, "/workflows/{id}/versions", superOnly, s.handleCreateWorkflowVersion)
	s.handle(g, http.MethodGet, "/workflows/{id}/versions/{vid}", allRoles, s.handleWorkflowVersion)
	s.handle(g, http.MethodPut, "/workflows/{id}/versions/{vid}", superOnly, s.handleUpdateWorkflowVersion)
	s.handle(g, http.MethodPost, "/workflows/{id}/versions/{vid}/publish", superOnly, s.handlePublishWorkflowVersion)
}

// coverUploadRe 是封面上传接口的路径：它收 multipart/form-data，requireJSONForWrites 放行，由处理器自己要求 multipart
// （角色校验在处理器之前，无权角色照样得到 403）。
var coverUploadRe = regexp.MustCompile(`^` + prefix + `/workflows/[^/]+/cover$`)

func isCoverUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && coverUploadRe.MatchString(r.URL.Path)
}

// ---- 工作流 ----

// workflowView 是后台的工作流视图：Workflow（不含 cover_key）+ 后台预览封面的地址（无封面为 null）。
type workflowView struct {
	comfywf.Workflow
	CoverURL *string `json:"cover_url"`
}

// workflowRowView 是后台工作流列表的一行：workflowView + 版本数、在用安装数、近 7 日快速出图次数。
type workflowRowView struct {
	workflowView
	Versions     int `json:"versions"`
	InstallsLive int `json:"installs_live"`
	Runs7d       int `json:"runs_7d"`
}

// adminCoverURL 是后台取封面的地址：/admin/api/v1/workflows/{id}/cover，带控制台 cover_url 同一个 ?v= 版本参数（换封面后地址改变）。
func adminCoverURL(w comfywf.Workflow) *string {
	pub := comfywf.CoverURL(w)
	if pub == nil {
		return nil
	}
	u := fmt.Sprintf("%s/workflows/%d/cover", prefix, w.ID)
	if i := strings.IndexByte(*pub, '?'); i >= 0 {
		u += (*pub)[i:]
	}
	return &u
}

func viewOfWorkflow(w comfywf.Workflow) workflowView {
	return workflowView{Workflow: w, CoverURL: adminCoverURL(w)}
}

type workflowRequest struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
	Sort        int      `json:"sort"`
	Status      string   `json:"status"`
}

func (q workflowRequest) input() comfywf.WorkflowInput {
	return comfywf.WorkflowInput{Slug: q.Slug, Name: q.Name, Summary: q.Summary, Description: q.Description, Category: q.Category,
		Tags: q.Tags, Sort: q.Sort, Status: q.Status}
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	list, err := s.Workflows.ListAdmin(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]workflowRowView, 0, len(list))
	for _, row := range list {
		views = append(views, workflowRowView{workflowView: viewOfWorkflow(row.Workflow), Versions: row.Versions,
			InstallsLive: row.InstallsLive, Runs7d: row.Runs7d})
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(views))
}

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req workflowRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	wf, err := s.Workflows.CreateWorkflow(r.Context(), s.actor(r), req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, viewOfWorkflow(wf))
}

func (s *Server) handleWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	wf, err := s.Workflows.GetWorkflow(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewOfWorkflow(wf))
}

// handleUpdateWorkflow 整体更新展示信息、排序与状态；slug 不可改（为空或等于原值，否则 bad_request）；
// status 省略（空）= 保持当前状态（不会因为漏传把已上架的工作流打回草稿）。
func (s *Server) handleUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req workflowRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Status == "" {
		cur, err := s.Workflows.GetWorkflow(r.Context(), id)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		req.Status = cur.Status
	}
	wf, err := s.Workflows.UpdateWorkflow(r.Context(), s.actor(r), id, req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewOfWorkflow(wf))
}

// handleDeleteWorkflow 只能删除没有任何版本的草稿（其余 invalid_state，改用 status = retired 下线）。
func (s *Server) handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Workflows.DeleteWorkflow(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

// ---- 封面 ----

// coverMultipartOverhead 是封面上传请求体在文件本身之外留给 multipart 头部与边界的余量。
const coverMultipartOverhead = 64 << 10

// handleUploadWorkflowCover 上传封面（multipart/form-data，字段 file；PNG / JPG / WebP ≤ 2MB，D13）；
// 超限 413 file_too_large、类型不对 415 unsupported_file_type。审计 workflow.update（detail cover）由 Catalog 记录。
func (s *Server) handleUploadWorkflowCover(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		s.writeError(w, r, apperr.BadRequest("请使用 multipart/form-data 上传，文件字段名为 file"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, comfywf.MaxCoverBytes+coverMultipartOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		s.writeError(w, r, apperr.BadRequest("请使用 multipart/form-data 上传，文件字段名为 file"))
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			s.writeError(w, r, apperr.BadRequest("缺少文件字段 file"))
			return
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, r, storage.ErrCoverTooLarge)
			return
		}
		if err != nil {
			s.writeError(w, r, apperr.BadRequest("上传内容格式不正确"))
			return
		}
		if part.FormName() != "file" {
			continue
		}
		wf, err := s.Workflows.SetCover(r.Context(), s.actor(r), id, part)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, viewOfWorkflow(wf))
		return
	}
}

// handleWorkflowCover 后台预览封面（任意状态的工作流）；没有封面 → 404 not_found。
func (s *Server) handleWorkflowCover(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	up, rc, err := s.Workflows.OpenCoverByID(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer rc.Close()
	h := w.Header()
	h.Set("Content-Type", up.ContentType)
	h.Set("Content-Length", strconv.FormatInt(up.Size, 10))
	h.Set("Content-Disposition", "inline")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		s.Log.Warn("serve workflow cover aborted", "workflow", id, "err", err)
	}
}

// ---- 安装列表 ----

func (s *Server) handleWorkflowInstalls(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items, total, err := s.Workflows.ListInstalls(r.Context(), id, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(items, total))
}

// ---- 版本 ----

// maxWorkflowVersionBody 是建 / 改版本请求体的上限：graph 与 api_prompt 各 ≤ 256KB（紧凑形式，由校验把关），
// 给带缩进的原文与清单留足余量。
const maxWorkflowVersionBody = 2 << 20

// wfVersionRequest 建 / 改工作流版本的请求体。字段用指针 / RawMessage 区分「省略」与「给了值」：
//   - POST：省略即零值（copy_from 时由被复制版本补齐 graph / api_prompt / manifest；api_prompt 显式 null = 不要快速出图）。
//   - PUT 草稿：省略的字段沿用库中当前值（读取时刻），所以可以只发要改的字段；api_prompt 显式 null = 去掉快速出图。
//   - PUT 已发布：只能改 changelog 或 status=retired；给了的其它字段须与当前内容相同（否则 workflow_version_frozen）。
//   - changelog 省略 = 不改（例如只下线不会清空更新说明）；给空串 = 清空。
type wfVersionRequest struct {
	Version   *string           `json:"version"`
	Graph     json.RawMessage   `json:"graph"`
	APIPrompt json.RawMessage   `json:"api_prompt"`
	Manifest  *comfywf.Manifest `json:"manifest"`
	Changelog *string           `json:"changelog"`
	CopyFrom  *int64            `json:"copy_from"`
	Status    string            `json:"status"`
}

// input 合成 VersionInput；base 非空时（草稿 PUT）省略的 version / graph / api_prompt / manifest 取 base 的值。
func (q wfVersionRequest) input(base *comfywf.Version) comfywf.VersionInput {
	in := comfywf.VersionInput{Graph: q.Graph, APIPrompt: q.APIPrompt, Changelog: q.Changelog, CopyFrom: q.CopyFrom, Status: q.Status}
	if base != nil {
		in.Version, in.Manifest = base.Version, base.Manifest
		if q.Graph == nil {
			in.Graph = base.Graph
		}
		if q.APIPrompt == nil {
			in.APIPrompt = base.APIPrompt
		}
	}
	if q.Version != nil {
		in.Version = *q.Version
	}
	if q.Manifest != nil {
		in.Manifest = *q.Manifest
	}
	return in
}

// decodeVersionRequest 解析版本请求体（上限 maxWorkflowVersionBody，未知字段报错；数字按 json.Number 解出，与导入一致）。
func decodeVersionRequest(w http.ResponseWriter, r *http.Request) (wfVersionRequest, error) {
	var req wfVersionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWorkflowVersionBody))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return req, apperr.BadRequest(fmt.Sprintf("请求内容不能超过 %dMB", maxWorkflowVersionBody>>20))
		}
		return req, apperr.BadRequest("请求参数格式不正确")
	}
	return req, nil
}

// workflowAndVersion 解析路径里的 {id} / {vid}：工作流不存在 → workflow_not_found；版本不属于它 → not_found。
func (s *Server) workflowAndVersion(r *http.Request) (int64, comfywf.Version, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return 0, comfywf.Version{}, err
	}
	vid, err := pathID(r, "vid")
	if err != nil {
		return 0, comfywf.Version{}, err
	}
	if _, err := s.Workflows.GetWorkflow(r.Context(), id); err != nil {
		return 0, comfywf.Version{}, err
	}
	v, err := s.Workflows.WorkflowVersion(r.Context(), id, vid)
	if err != nil {
		return 0, comfywf.Version{}, err
	}
	return id, v, nil
}

// writeWorkflowValidated 是 ?validate=1 的响应：只校验不保存。
func (s *Server) writeWorkflowValidated(w http.ResponseWriter, r *http.Request, res comfywf.ValidateResult, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	warnings := res.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "manifest_hash": res.ManifestHash, "download_gb": res.DownloadGB,
		"warnings": warnings})
}

// handleListWorkflowVersions 版本表（不含 graph / api_prompt / manifest 本体），按创建倒序。
func (s *Server) handleListWorkflowVersions(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, err := s.Workflows.GetWorkflow(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	list, err := s.Workflows.ListVersionRows(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

// handleWorkflowVersion 版本详情：唯一返回 graph / api_prompt / manifest 全文的接口（只在后台）。读取不审计。
func (s *Server) handleWorkflowVersion(w http.ResponseWriter, r *http.Request) {
	_, v, err := s.workflowAndVersion(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) handleCreateWorkflowVersion(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	req, err := decodeVersionRequest(w, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Status != "" && req.Status != comfywf.StatusDraft {
		s.writeError(w, r, apperr.BadRequest("新版本只能是草稿；发布请使用发布操作"))
		return
	}
	in := req.input(nil)
	in.Status = ""
	if validateOnly(r) {
		res, err := s.Workflows.ValidateVersion(r.Context(), id, in)
		s.writeWorkflowValidated(w, r, res, err)
		return
	}
	v, err := s.Workflows.CreateVersion(r.Context(), s.actor(r), id, in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, v)
}

func (s *Server) handleUpdateWorkflowVersion(w http.ResponseWriter, r *http.Request) {
	id, cur, err := s.workflowAndVersion(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	req, err := decodeVersionRequest(w, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.CopyFrom != nil {
		s.writeError(w, r, apperr.BadRequest("copy_from 只用于新建版本"))
		return
	}
	var base *comfywf.Version
	if cur.Status == comfywf.StatusDraft {
		base = &cur
	}
	in := req.input(base)
	if validateOnly(r) { // 与真正修改同一规则：已发布版本改冻结字段 / 已下线版本 → workflow_version_frozen
		res, err := s.Workflows.ValidateVersionUpdate(r.Context(), id, cur.ID, in)
		s.writeWorkflowValidated(w, r, res, err)
		return
	}
	v, err := s.Workflows.UpdateVersion(r.Context(), s.actor(r), id, cur.ID, in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

// handlePublishWorkflowVersion 发布草稿（super + 动态码，D6）：先确认版本存在且是草稿，明显无法发布的请求不消耗动态码；
// 发布成功后才打开二次验证窗口（withStepUp）。校验失败 → workflow_version_invalid（不打开窗口）。
func (s *Server) handlePublishWorkflowVersion(w http.ResponseWriter, r *http.Request) {
	id, cur, err := s.workflowAndVersion(r)
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
	if cur.Status != comfywf.StatusDraft {
		s.writeError(w, r, apperr.ErrInvalidState)
		return
	}
	var out comfywf.Version
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		out, err = s.Workflows.PublishVersion(r.Context(), s.actor(r), id, cur.ID)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
