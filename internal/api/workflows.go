package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/comfywf"
	"spark-cloud/internal/platform/apperr"
)

// ComfyUI 工作流市场（计划 11，规格 §4、§6.1、§10.1）。
//   - 市场三个接口匿名可访问，按 IP 限流（同 /apps）；详情不含 api_prompt 本体（Manifest 本身不带它）。
//   - /instances/{id}/apps/{dep}/workflows* 一律先校验归属：别人的实例 / 部署与不存在同样 404。
//   - 安装三个写接口与应用部署写操作共用每用户每小时的限流器（allowAppWrite）；运行的频率与在飞限制在 comfywf.Runs.Start 里（按库）。

const (
	// wfUploadSlack multipart 头部与边界的余量（上传体 = 文件 + 这些）。
	wfUploadSlack = 1 << 10
	// 快速出图接口的读写期限：公网服务器默认 ReadTimeout 30s / WriteTimeout 60s，上传大图、慢速下载输出文件、/prompt 慢响应需要更长。
	wfUploadReadDeadline = 2 * time.Minute
	wfRunWriteDeadline   = 2 * time.Minute
	wfFileWriteDeadline  = comfywf.ViewTimeout + 30*time.Second
	// coverMaxAge 封面的公共缓存时长（cover_url 带 ?v=，换封面 URL 随之变化）。
	coverMaxAge = "public, max-age=86400"
)

// ---- 市场（匿名） ----

func (s *Server) handleWorkflowCatalog(w http.ResponseWriter, r *http.Request) {
	items, err := s.Workflows.ListPublished(r.Context(), r.URL.Query().Get("category"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if items == nil {
		items = []comfywf.CatalogItem{}
	}
	writeJSON(w, http.StatusOK, list(items))
}

func (s *Server) handleWorkflowDetail(w http.ResponseWriter, r *http.Request) {
	d, err := s.Workflows.DetailBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// coverETag 由封面文件键派生（哈希前 16 位十六进制），不暴露文件键本身。
func coverETag(key string) string {
	sum := sha256.Sum256([]byte(key))
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// etagMatches If-None-Match（逗号分隔、可带 W/ 前缀、或 *）里有 etag。
func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == etag {
			return true
		}
	}
	return false
}

func (s *Server) handleWorkflowCover(w http.ResponseWriter, r *http.Request) {
	up, body, err := s.Workflows.OpenCover(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer body.Close()
	h := w.Header()
	etag := coverETag(up.Key)
	h.Set("Cache-Control", coverMaxAge) // 覆盖组中间件的 no-store
	h.Set("ETag", etag)
	h.Set("X-Content-Type-Options", "nosniff")
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", up.ContentType)
	if up.Size > 0 {
		h.Set("Content-Length", strconv.FormatInt(up.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		io.Copy(w, body)
	}
}

// ---- 安装 ----

// workflowIDParam 解析 {wid}；不是正整数 → workflow_not_found（与不存在相同）。
func workflowIDParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "wid"), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.ErrWorkflowNotFound
	}
	return id, nil
}

// installResult 是安装三个写接口的响应：{deployment: 部署视图（同 GET /instances/{id}/apps 的一项）, workflows: [Installed]}。
func (s *Server) writeInstallResult(w http.ResponseWriter, r *http.Request, status int, res comfywf.Result, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views, err := s.Apps.Views(r.Context(), []appdeploy.Deployment{res.Deployment})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ws := res.Workflows
	if ws == nil {
		ws = []comfywf.Installed{}
	}
	writeJSON(w, status, map[string]any{"deployment": views[0], "workflows": ws})
}

func (s *Server) handleDeploymentWorkflows(w http.ResponseWriter, r *http.Request) {
	d, err := s.ownedDeployment(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ws, err := s.Installer.ListInstalled(r.Context(), d.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if ws == nil {
		ws = []comfywf.Installed{}
	}
	writeJSON(w, http.StatusOK, list(ws))
}

func (s *Server) handleAddWorkflow(w http.ResponseWriter, r *http.Request) {
	if !s.allowAppWrite(w, r) {
		return
	}
	var req struct {
		WorkflowID int64  `json:"workflow_id"`
		VersionID  *int64 `json:"version_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	res, err := s.Installer.Add(r.Context(), comfywf.AddRequest{UserID: currentUser(r).ID, InstanceID: chi.URLParam(r, "id"),
		DeploymentID: chi.URLParam(r, "dep"), WorkflowID: req.WorkflowID, VersionID: req.VersionID, IP: clientIP(r)})
	s.writeInstallResult(w, r, http.StatusCreated, res, err)
}

func (s *Server) handleUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	if !s.allowAppWrite(w, r) {
		return
	}
	wid, err := workflowIDParam(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		VersionID *int64 `json:"version_id"`
	}
	if err := decodeOptionalJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	res, err := s.Installer.Update(r.Context(), comfywf.UpdateRequest{UserID: currentUser(r).ID, InstanceID: chi.URLParam(r, "id"),
		DeploymentID: chi.URLParam(r, "dep"), WorkflowID: wid, VersionID: req.VersionID, IP: clientIP(r)})
	s.writeInstallResult(w, r, http.StatusOK, res, err)
}

func (s *Server) handleRemoveWorkflow(w http.ResponseWriter, r *http.Request) {
	if !s.allowAppWrite(w, r) {
		return
	}
	wid, err := workflowIDParam(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	res, err := s.Installer.Remove(r.Context(), comfywf.RemoveRequest{UserID: currentUser(r).ID, InstanceID: chi.URLParam(r, "id"),
		DeploymentID: chi.URLParam(r, "dep"), WorkflowID: wid, IP: clientIP(r)})
	s.writeInstallResult(w, r, http.StatusOK, res, err)
}

// decodeOptionalJSON 同 decodeJSON，但允许空请求体（PUT 不带 version_id = 最新版本）。
func decodeOptionalJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return apperr.BadRequest("请求参数格式不正确")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return decodeJSON(r, v)
}

// ---- 快速出图 ----

// isWorkflowUploadPath：POST /api/v1/instances/{id}/apps/{dep}/workflows/{wid}/uploads（要求 multipart/form-data）。
func isWorkflowUploadPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/api/v1/instances/")
	if !ok {
		return false
	}
	seg := strings.Split(rest, "/")
	return len(seg) == 6 && seg[1] == "apps" && seg[3] == "workflows" && seg[5] == "uploads" && seg[0] != "" && seg[2] != "" && seg[4] != ""
}

// extendDeadlines 放宽本请求的读 / 写期限（零值 = 不改）；不支持（测试 ResponseWriter）时忽略。
func extendDeadlines(w http.ResponseWriter, read, write time.Duration) {
	rc := http.NewResponseController(w)
	now := time.Now()
	if read > 0 {
		_ = rc.SetReadDeadline(now.Add(read))
	}
	if write > 0 {
		_ = rc.SetWriteDeadline(now.Add(write))
	}
}

func (s *Server) runRequest(r *http.Request) (comfywf.GetRequest, error) {
	wid, err := workflowIDParam(r)
	if err != nil {
		return comfywf.GetRequest{}, err
	}
	return comfywf.GetRequest{UserID: currentUser(r).ID, InstanceID: chi.URLParam(r, "id"), DeploymentID: chi.URLParam(r, "dep"),
		WorkflowID: wid, RunID: chi.URLParam(r, "run")}, nil
}

// fillFileURLs 填 RunView.Files[].URL = /api/v1/instances/{id}/apps/{dep}/workflows/{wid}/runs/{run}/files/{index}。
func fillFileURLs(v *comfywf.RunView, req comfywf.GetRequest) {
	if v.Files == nil {
		v.Files = []comfywf.FileView{}
	}
	base := "/api/v1/instances/" + req.InstanceID + "/apps/" + req.DeploymentID + "/workflows/" + strconv.FormatInt(req.WorkflowID, 10) +
		"/runs/" + v.ID + "/files/"
	for i := range v.Files {
		v.Files[i].URL = base + strconv.Itoa(v.Files[i].Index)
	}
}

// maxBytesErrReader 把 http.MaxBytesReader 的超限错误换成 file_too_large（请求体超过 16MB + 头部余量）。
type maxBytesErrReader struct{ r io.Reader }

func (m maxBytesErrReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		err = comfywf.ErrUploadTooLarge
	}
	return n, err
}

// allowWorkflowUpload 上传的每用户限流（与运行同额：每小时 RunsPerHour）；超限已写 rate_limited。
func (s *Server) allowWorkflowUpload(w http.ResponseWriter, r *http.Request) bool {
	if !s.wfUploadLim.allow(currentUser(r).ID, s.Clock.Now()) {
		s.writeError(w, r, apperr.ErrRateLimited)
		return false
	}
	return true
}

func (s *Server) handleWorkflowUpload(w http.ResponseWriter, r *http.Request) {
	if !s.allowWorkflowUpload(w, r) {
		return
	}
	wid, err := workflowIDParam(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	extendDeadlines(w, wfUploadReadDeadline, wfUploadReadDeadline)
	r.Body = http.MaxBytesReader(w, r.Body, comfywf.MaxUploadBytes+wfUploadSlack)
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
			s.writeError(w, r, comfywf.ErrUploadTooLarge)
			return
		}
		if err != nil {
			s.writeError(w, r, apperr.BadRequest("上传内容格式不正确"))
			return
		}
		if part.FormName() != "file" {
			continue
		}
		res, err := s.Runs.Upload(r.Context(), comfywf.UploadRequest{UserID: currentUser(r).ID, InstanceID: chi.URLParam(r, "id"),
			DeploymentID: chi.URLParam(r, "dep"), WorkflowID: wid, Body: maxBytesErrReader{part}})
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, res)
		return
	}
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	req, err := s.runRequest(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	extendDeadlines(w, 0, wfRunWriteDeadline)
	var body struct {
		Inputs json.RawMessage `json:"inputs"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	// 数字保留原文（json.Number），不经 float64 舍入：大整数（种子上限 2^53-1 附近）按原值校验
	var inputs map[string]any
	if len(body.Inputs) > 0 && string(body.Inputs) != "null" {
		dec := json.NewDecoder(bytes.NewReader(body.Inputs))
		dec.UseNumber()
		if err := dec.Decode(&inputs); err != nil {
			s.writeError(w, r, apperr.BadRequest("inputs 格式不正确"))
			return
		}
	}
	if inputs == nil {
		inputs = map[string]any{}
	}
	v, err := s.Runs.Start(r.Context(), comfywf.StartRequest{UserID: req.UserID, InstanceID: req.InstanceID, DeploymentID: req.DeploymentID,
		WorkflowID: req.WorkflowID, Inputs: inputs, IP: clientIP(r)})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	fillFileURLs(&v, req)
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	req, err := s.runRequest(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.Runs.Get(r.Context(), req)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	fillFileURLs(&v, req)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleRunFile(w http.ResponseWriter, r *http.Request) {
	req, err := s.runRequest(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil || index < 0 {
		s.writeError(w, r, apperr.ErrNotFound)
		return
	}
	extendDeadlines(w, 0, wfFileWriteDeadline)
	f, err := s.Runs.OpenFile(r.Context(), req, index)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer f.Body.Close()
	f.SetHeaders(w.Header()) // 覆盖组中间件的 no-store
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f.Body); err != nil {
		// 响应头已发出，只能中断连接（客户端看到截断的响应）。客户端中途断开很常见（关掉预览），只记 Debug
		lvl := slog.LevelDebug
		if errors.Is(err, comfywf.ErrFileTooLarge) {
			lvl = slog.LevelWarn
		}
		s.Log.Log(r.Context(), lvl, "workflow run file copy aborted", "run", req.RunID, "index", index, "err", err)
		panic(http.ErrAbortHandler)
	}
}
