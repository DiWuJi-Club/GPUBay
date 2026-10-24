package comfywf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/rental"
)

// 快速出图（规格 §6）：经 Client 代理容器内 ComfyUI 的 /upload/image、/prompt、/history、/queue、/view，路径固定，
// 参数由平台拼（/view 的参数取自 history，不接受客户端传文件名）。运行记录只存元数据（comfy_workflow_runs）。

// QuickRunSubfolder 上传图片在 ComfyUI input 目录下的子目录。
const QuickRunSubfolder = "spark-quickrun"

var (
	// ErrUploadTooLarge 上传图片超过 MaxUploadBytes（与 storage.ErrFileTooLarge 同码，限额不同）。
	ErrUploadTooLarge = apperr.New("file_too_large", "图片不能超过 16MB", 413)
	// ErrUploadType 上传的不是 PNG / JPG / WebP（按内容判断；与 storage.ErrUnsupportedType 同码）。
	ErrUploadType = apperr.New("unsupported_file_type", "仅支持 PNG、JPG、WebP 图片", 415)
	// ErrFileTooLarge 输出文件超过 MaxViewBytes。
	ErrFileTooLarge = apperr.New("file_too_large", "输出文件超过 64MB，请在 ComfyUI 中查看", 413)

	errNotRunning   = apperr.New(apperr.ErrAppInvalidState.Code, "ComfyUI 未运行", 409)
	errNotInstalled = apperr.New(apperr.ErrAppInvalidState.Code, "工作流尚未安装完成", 409)
	errNoQuickRun   = apperr.BadRequest("该工作流不支持快速出图")
	errNoImageInput = apperr.BadRequest("该工作流没有图片输入")

	// promptIDRe ComfyUI 返回的 prompt_id（UUID；放宽到 token），会进 /history/{id} 路径与库。
	promptIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
)

const (
	// maxRejectMessage /prompt 拒绝时错误消息的截断长度（字符，规格 §6.1）。
	maxRejectMessage = 200
	// maxRefresh 在飞计数达到上限时，最多复查几个 queued 运行（没人轮询的已完成运行不应一直占名额）。
	maxRefresh = 4
)

// Runs 快速出图服务。
type Runs struct {
	pool   *pgxpool.Pool
	clk    clock.Clock
	cat    *Catalog
	apps   *appdeploy.Service
	client *Client
	log    *slog.Logger
	rnd    func() int64 // seed 随机源；nil = BuildPrompt 默认（测试注入）
}

func NewRuns(pool *pgxpool.Pool, clk clock.Clock, cat *Catalog, apps *appdeploy.Service, client *Client, log *slog.Logger) *Runs {
	if log == nil {
		log = slog.Default()
	}
	return &Runs{pool: pool, clk: clk, cat: cat, apps: apps, client: client, log: log}
}

// ---- 请求 / 响应 ----

type UploadRequest struct {
	UserID       int64
	InstanceID   string
	DeploymentID string
	WorkflowID   int64
	Body         io.Reader
	Size         int64 // 已知大小（multipart 头）；<= 0 = 未知，读取时限长
}

type UploadResult struct {
	Name string `json:"name"` // spark-quickrun/<12 位>.<png|jpg|webp>，作为 image 类型输入的值
}

type StartRequest struct {
	UserID       int64
	InstanceID   string
	DeploymentID string
	WorkflowID   int64
	Inputs       map[string]any
	IP           string
}

type GetRequest struct {
	UserID       int64
	InstanceID   string
	DeploymentID string
	WorkflowID   int64
	RunID        string
}

// RunView 运行的对外视图（POST / GET …/runs[/{run}]）。Files[].URL 由 api 层按路由填。
type RunView struct {
	ID         string     `json:"id"`
	PromptID   string     `json:"prompt_id"`
	Seed       *int64     `json:"seed"`
	State      string     `json:"state"` // queued / running / done / error
	Message    string     `json:"message"`
	Files      []FileView `json:"files"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

type FileView struct {
	Index    int    `json:"index"`
	Filename string `json:"filename"`
	Kind     string `json:"kind"` // image / video
	URL      string `json:"url"`
}

// File 是 OpenFile 的结果：调用方 SetHeaders 后 io.Copy(w, Body)，最后 Close。
// Body 已限长：读到第 MaxViewBytes+1 个字节时返回 ErrFileTooLarge（此时响应头已发出，调用方只能中断连接）。
// 响应头全部由平台生成（Content-Type 按扩展名，不透传容器的 Content-Type）。
type File struct {
	Body        io.ReadCloser
	ContentType string
	Length      int64 // -1 = 未知
	Filename    string
	Disposition string // inline; filename=…（mime.FormatMediaType，非 ASCII 用 RFC 2231 编码）
}

// SetHeaders 写下载响应头：Content-Type、Content-Length（已知时）、X-Content-Type-Options、Content-Disposition、Cache-Control。
func (f *File) SetHeaders(h http.Header) {
	h.Set("Content-Type", f.ContentType)
	if f.Length >= 0 {
		h.Set("Content-Length", strconv.FormatInt(f.Length, 10))
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", f.Disposition)
	h.Set("Cache-Control", "private, max-age=3600")
}

// ---- 公共校验 ----

// runTarget 部署的连接目标。
type runTarget struct {
	dep       appdeploy.Deployment
	up        upstream.Target
	port      int
	reachable bool // 实例 running 且未暂停、部署 running
}

// target 归属（不是主人 / 不在该实例上 → not_found）+ 连接目标；reachable 由调用方决定是否要求。
func (r *Runs) target(ctx context.Context, userID int64, instanceID, depID string) (runTarget, error) {
	d, err := r.apps.GetForUser(ctx, userID, instanceID, depID)
	if err != nil {
		return runTarget{}, err
	}
	var state, lanIP, hostPub string
	var suspended bool
	err = r.pool.QueryRow(ctx, `SELECT i.state, i.suspended_at IS NOT NULL, m.lan_ip, i.ssh_host_pubkey
		FROM instances i JOIN machines m ON m.id = i.machine_id WHERE i.id = $1`, d.InstanceID).Scan(&state, &suspended, &lanIP, &hostPub)
	if db.IsNoRows(err) {
		return runTarget{}, apperr.ErrNotFound
	}
	if err != nil {
		return runTarget{}, err
	}
	return runTarget{
		dep:       d,
		up:        upstream.Target{InstanceID: d.InstanceID, LanIP: lanIP, HostPubkey: hostPub},
		port:      d.Port, // = spec.ports[0].port（创建 / 重新部署时写入）
		reachable: state == rental.StateRunning && !suspended && d.State == appdeploy.StateRunning,
	}, nil
}

// runnable 归属 + ComfyUI 在运行 + 该工作流在部署上且已安装 → 已安装的版本。
func (r *Runs) runnable(ctx context.Context, userID int64, instanceID, depID string, wfID int64) (runTarget, Version, error) {
	rt, err := r.target(ctx, userID, instanceID, depID)
	if err != nil {
		return runTarget{}, Version{}, err
	}
	var vid int64
	var state string
	err = r.pool.QueryRow(ctx, `SELECT workflow_version_id, state FROM deployment_workflows WHERE deployment_id = $1 AND workflow_id = $2`,
		depID, wfID).Scan(&vid, &state)
	if db.IsNoRows(err) {
		return runTarget{}, Version{}, apperr.ErrWorkflowNotFound
	}
	if err != nil {
		return runTarget{}, Version{}, err
	}
	if !rt.reachable {
		return runTarget{}, Version{}, errNotRunning
	}
	if state != WFStateInstalled {
		return runTarget{}, Version{}, errNotInstalled
	}
	ver, err := r.cat.GetVersion(ctx, vid)
	if err != nil {
		return runTarget{}, Version{}, err
	}
	if nullJSON(ver.APIPrompt) {
		return runTarget{}, Version{}, errNoQuickRun
	}
	return rt, ver, nil
}

// ---- 上传 ----

var uploadExts = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp"}

// Upload 把图片传到 ComfyUI 的 input/spark-quickrun/，文件名由平台生成（不透传用户文件名），返回作为 image 输入的值。
func (r *Runs) Upload(ctx context.Context, req UploadRequest) (UploadResult, error) {
	rt, ver, err := r.runnable(ctx, req.UserID, req.InstanceID, req.DeploymentID, req.WorkflowID)
	if err != nil {
		return UploadResult{}, err
	}
	if !slices.ContainsFunc(ver.Manifest.Inputs, func(in InputSpec) bool { return in.Type == InputImage }) {
		return UploadResult{}, errNoImageInput
	}
	if req.Size > MaxUploadBytes {
		return UploadResult{}, ErrUploadTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, MaxUploadBytes+1))
	if err != nil {
		return UploadResult{}, err
	}
	if len(data) > MaxUploadBytes {
		return UploadResult{}, ErrUploadTooLarge
	}
	ctype := http.DetectContentType(data)
	ext, ok := uploadExts[ctype]
	if !ok || len(data) == 0 {
		return UploadResult{}, ErrUploadType
	}
	name := randx.ID("", 12) + "." + ext
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	ph := textproto.MIMEHeader{}
	ph.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image"; filename=%q`, name))
	ph.Set("Content-Type", ctype)
	part, err := mw.CreatePart(ph)
	if err != nil {
		return UploadResult{}, err
	}
	part.Write(data)
	for k, v := range map[string]string{"subfolder": QuickRunSubfolder, "overwrite": "true", "type": "input"} {
		if err := mw.WriteField(k, v); err != nil {
			return UploadResult{}, err
		}
	}
	if err := mw.Close(); err != nil {
		return UploadResult{}, err
	}
	status, body, err := r.client.call(ctx, rt.up, rt.port, http.MethodPost, "/upload/image", &buf, mw.FormDataContentType(), maxJSONBytes)
	if err != nil {
		return UploadResult{}, err
	}
	if status != http.StatusOK {
		return UploadResult{}, badUpstream("/upload/image 返回 %d", status)
	}
	var res struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return UploadResult{}, badUpstream("/upload/image 响应不是 JSON")
	}
	full := res.Subfolder + "/" + res.Name
	if !quickImageRe.MatchString(full) {
		return UploadResult{}, badUpstream("/upload/image 返回的文件名不合法")
	}
	return UploadResult{Name: full}, nil
}

// ---- 运行 ----

// Start 提交一次运行：校验表单（BuildPrompt）→ 频率（每用户每小时 RunsPerHour）与在飞（每部署 MaxRunsInFlight）限制 →
// POST /prompt {prompt, client_id: run id} → 记 comfy_workflow_runs。
func (r *Runs) Start(ctx context.Context, req StartRequest) (RunView, error) {
	rt, ver, err := r.runnable(ctx, req.UserID, req.InstanceID, req.DeploymentID, req.WorkflowID)
	if err != nil {
		return RunView{}, err
	}
	prompt, seed, err := BuildPrompt(ver.APIPrompt, ver.Manifest.Inputs, req.Inputs, r.rnd)
	if err != nil {
		return RunView{}, err
	}
	now := r.clk.Now()
	var hourly int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM comfy_workflow_runs WHERE user_id = $1 AND created_at > $2`,
		req.UserID, now.Add(-time.Hour)).Scan(&hourly); err != nil {
		return RunView{}, err
	}
	if hourly >= RunsPerHour {
		return RunView{}, apperr.ErrRateLimited
	}
	if err := r.checkInFlight(ctx, rt, now); err != nil {
		return RunView{}, err
	}
	runID := randx.ID("run-", 12)
	reqBody, err := json.Marshal(struct {
		Prompt   json.RawMessage `json:"prompt"`
		ClientID string          `json:"client_id"`
	}{prompt, runID})
	if err != nil {
		return RunView{}, err
	}
	status, body, err := r.client.call(ctx, rt.up, rt.port, http.MethodPost, "/prompt", bytes.NewReader(reqBody), "application/json", maxJSONBytes)
	if err != nil {
		return RunView{}, err
	}
	if status == http.StatusBadRequest {
		return RunView{}, apperr.WorkflowPromptRejected(rejectMessage(body))
	}
	if status != http.StatusOK {
		return RunView{}, badUpstream("/prompt 返回 %d", status)
	}
	var res struct {
		PromptID string `json:"prompt_id"`
	}
	if err := json.Unmarshal(body, &res); err != nil || !promptIDRe.MatchString(res.PromptID) {
		return RunView{}, badUpstream("/prompt 响应缺少合法的 prompt_id")
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO comfy_workflow_runs (id, deployment_id, user_id, workflow_version_id, prompt_id, seed, state, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'queued', $7)`, runID, rt.dep.ID, req.UserID, ver.ID, res.PromptID, seed, now); err != nil {
		return RunView{}, err
	}
	return RunView{ID: runID, PromptID: res.PromptID, Seed: seed, State: RunQueued, Files: []FileView{}, CreatedAt: now}, nil
}

// checkInFlight 在飞（queued 且未超 RunWaitMax）≥ MaxRunsInFlight → ErrWorkflowBusy。达到上限时先复查这些运行的 history
// （没人轮询的已结束运行就此落成终态），再数一次。软限制：并发提交可能略超（规格 §14）。
func (r *Runs) checkInFlight(ctx context.Context, rt runTarget, now time.Time) error {
	ids, err := r.inFlight(ctx, rt.dep.ID, now)
	if err != nil || len(ids) < MaxRunsInFlight {
		return err
	}
	for _, run := range ids[:min(len(ids), maxRefresh)] {
		ver, err := r.cat.GetVersion(ctx, run.VersionID)
		if err != nil {
			return err
		}
		h, err := r.history(ctx, rt, run.PromptID, ver.Manifest.Outputs)
		if err != nil {
			if errors.Is(err, apperr.ErrUpstreamUnreachable) {
				return err
			}
			continue // 响应不可用：这一个不复查
		}
		if h.State == RunDone || h.State == RunError {
			if _, err := r.finish(ctx, run.ID, h.State, now); err != nil {
				return err
			}
		}
	}
	if ids, err = r.inFlight(ctx, rt.dep.ID, now); err != nil {
		return err
	}
	if len(ids) >= MaxRunsInFlight {
		return apperr.ErrWorkflowBusy
	}
	return nil
}

func (r *Runs) inFlight(ctx context.Context, depID string, now time.Time) ([]Run, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+runCols+` FROM comfy_workflow_runs WHERE deployment_id = $1 AND state = 'queued' AND created_at > $2
		ORDER BY created_at, id`, depID, now.Add(-RunWaitMax))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// rejectMessage 取 /prompt 400 响应里第一条节点错误（按节点 id 排序）的 message（+ details），没有则取 error.message；截 200 字符。
func rejectMessage(body []byte) string {
	type item struct {
		Message string `json:"message"`
		Details string `json:"details"`
	}
	var res struct {
		Error      json.RawMessage            `json:"error"`
		NodeErrors map[string]json.RawMessage `json:"node_errors"`
	}
	msg := ""
	if json.Unmarshal(body, &res) == nil {
		for _, id := range sortedKeys(res.NodeErrors) {
			var ne struct {
				Errors []item `json:"errors"`
			}
			if json.Unmarshal(res.NodeErrors[id], &ne) == nil && len(ne.Errors) > 0 && strings.TrimSpace(ne.Errors[0].Message) != "" {
				msg = joinDetails(ne.Errors[0].Message, ne.Errors[0].Details)
				break
			}
		}
		if msg == "" {
			var e item
			if json.Unmarshal(res.Error, &e) == nil {
				msg = joinDetails(e.Message, e.Details)
			} else {
				var s string
				if json.Unmarshal(res.Error, &s) == nil {
					msg = s
				}
			}
		}
	}
	msg = strings.TrimSpace(stripControl(msg))
	if msg == "" {
		msg = "工作流校验失败"
	}
	return truncateRunes(msg, maxRejectMessage)
}

func joinDetails(msg, details string) string {
	msg, details = strings.TrimSpace(msg), strings.TrimSpace(details)
	if details == "" || msg == "" {
		return msg
	}
	return msg + "：" + details
}

// stripControl 去掉控制字符（容器给的文本进错误消息）。
func stripControl(s string) string {
	return strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return ' '
		}
		return c
	}, strings.ToValidUTF8(s, ""))
}

// ---- 查询 ----

const runCols = `id, deployment_id, user_id, workflow_version_id, prompt_id, seed, state, created_at, finished_at`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var run Run
	err := row.Scan(&run.ID, &run.DeploymentID, &run.UserID, &run.VersionID, &run.PromptID, &run.Seed, &run.State, &run.CreatedAt, &run.FinishedAt)
	if db.IsNoRows(err) {
		return Run{}, apperr.ErrNotFound
	}
	return run, err
}

func baseView(run Run) RunView {
	return RunView{ID: run.ID, PromptID: run.PromptID, Seed: run.Seed, State: run.State, Files: []FileView{}, CreatedAt: run.CreatedAt,
		FinishedAt: run.FinishedAt}
}

// Get 查询运行状态（规格 §6.1）：history 有结果 → done / error（同步落库）；没有 → 查 /queue 区分 running / queued。
// 终态的运行仍查一次 history 取输出文件；查不到（ComfyUI 重启、连不上、实例未运行）只返回缓存的状态。
func (r *Runs) Get(ctx context.Context, req GetRequest) (RunView, error) {
	rt, err := r.target(ctx, req.UserID, req.InstanceID, req.DeploymentID)
	if err != nil {
		return RunView{}, err
	}
	v, _, err := r.get(ctx, rt, req)
	return v, err
}

// get 是 Get 的主体；另外返回 history 里的输出文件（只在 done 且本次读到 history 时非空，OpenFile 用）。
func (r *Runs) get(ctx context.Context, rt runTarget, req GetRequest) (RunView, []OutputFile, error) {
	run, err := scanRun(r.pool.QueryRow(ctx, `SELECT `+qualify(runCols, "r")+` FROM comfy_workflow_runs r
		JOIN comfy_workflow_versions v ON v.id = r.workflow_version_id
		WHERE r.id = $1 AND r.deployment_id = $2 AND r.user_id = $3 AND v.workflow_id = $4`, req.RunID, rt.dep.ID, req.UserID, req.WorkflowID))
	if err != nil {
		return RunView{}, nil, err
	}
	view := baseView(run)
	terminal := run.State != RunQueued
	if !rt.reachable {
		if terminal {
			return view, nil, nil
		}
		return RunView{}, nil, errNotRunning
	}
	ver, err := r.cat.GetVersion(ctx, run.VersionID)
	if err != nil {
		return RunView{}, nil, err
	}
	h, err := r.history(ctx, rt, run.PromptID, ver.Manifest.Outputs)
	if err != nil {
		if terminal {
			r.log.Info("comfywf: history unavailable for finished run", "run", run.ID, "err", err)
			return view, nil, nil
		}
		return RunView{}, nil, err
	}
	switch {
	case h.State == "" && terminal: // history 已丢（ComfyUI 重启）：只给缓存状态
		return view, nil, nil
	case h.State == "":
		if r.queueRunning(ctx, rt, run.PromptID) {
			view.State = RunRunning
		}
		return view, nil, nil
	}
	if !terminal {
		fin, err := r.finish(ctx, run.ID, h.State, r.clk.Now())
		if err != nil {
			return RunView{}, nil, err
		}
		view.State, view.FinishedAt = h.State, fin
	}
	if view.State == RunError && h.State == RunError {
		view.Message = h.Message
	}
	if view.State == RunDone && h.State == RunDone {
		for _, f := range h.Files {
			view.Files = append(view.Files, FileView{Index: f.Index, Filename: f.Filename, Kind: f.Kind})
		}
		return view, h.Files, nil
	}
	return view, nil, nil
}

// finish queued → 终态（只改仍是 queued 的行；并发轮询以先写者为准），返回 finished_at。
func (r *Runs) finish(ctx context.Context, runID, state string, now time.Time) (*time.Time, error) {
	var fin *time.Time
	err := r.pool.QueryRow(ctx, `UPDATE comfy_workflow_runs SET state = $2, finished_at = $3 WHERE id = $1 AND state = 'queued' RETURNING finished_at`,
		runID, state, now).Scan(&fin)
	if db.IsNoRows(err) {
		err = r.pool.QueryRow(ctx, `SELECT finished_at FROM comfy_workflow_runs WHERE id = $1`, runID).Scan(&fin)
	}
	return fin, err
}

// history GET /history/{prompt_id}（≤ 1MB）→ ParseHistory。
func (r *Runs) history(ctx context.Context, rt runTarget, promptID string, outputs []OutputSpec) (HistoryResult, error) {
	status, body, err := r.client.call(ctx, rt.up, rt.port, http.MethodGet, "/history/"+url.PathEscape(promptID), nil, "", maxJSONBytes)
	if err != nil {
		return HistoryResult{}, err
	}
	if status != http.StatusOK {
		return HistoryResult{}, badUpstream("/history 返回 %d", status)
	}
	h, err := ParseHistory(body, promptID, outputs)
	if err != nil {
		return HistoryResult{}, badUpstream("%v", err)
	}
	return h, nil
}

// queueRunning GET /queue：prompt_id 在 queue_running 里（元素 [number, prompt_id, …]）。任何错误按「不在运行」处理。
func (r *Runs) queueRunning(ctx context.Context, rt runTarget, promptID string) bool {
	status, body, err := r.client.call(ctx, rt.up, rt.port, http.MethodGet, "/queue", nil, "", maxQueueBytes)
	if err != nil || status != http.StatusOK {
		return false
	}
	var q struct {
		Running []json.RawMessage `json:"queue_running"`
	}
	if json.Unmarshal(body, &q) != nil {
		return false
	}
	for _, it := range q.Running {
		var item []json.RawMessage
		if json.Unmarshal(it, &item) != nil || len(item) < 2 {
			continue
		}
		var id string
		if json.Unmarshal(item[1], &id) == nil && id == promptID {
			return true
		}
	}
	return false
}

// ---- 输出文件 ----

var viewTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif",
	".apng": "image/apng", ".bmp": "image/bmp",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime", ".mkv": "video/x-matroska",
	".avi": "video/x-msvideo",
}

// OpenFile 代理第 index 个输出文件：GET /view?filename=&subfolder=&type=output（参数全部取自 history）。
// 运行未完成 / index 越界 / 容器里文件不在 → not_found；Content-Length > MaxViewBytes → ErrFileTooLarge。
// 返回的 Body 超时 ViewTimeout，调用方必须 Close。
func (r *Runs) OpenFile(ctx context.Context, req GetRequest, index int) (*File, error) {
	rt, err := r.target(ctx, req.UserID, req.InstanceID, req.DeploymentID)
	if err != nil {
		return nil, err
	}
	if !rt.reachable {
		if _, _, err := r.get(ctx, rt, req); err != nil { // 归属优先：别人的运行 → not_found
			return nil, err
		}
		return nil, errNotRunning
	}
	_, files, err := r.get(ctx, rt, req)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(files) {
		return nil, apperr.ErrNotFound
	}
	f := files[index]
	ctype, ok := viewTypes[strings.ToLower(path.Ext(f.Filename))]
	if !ok {
		return nil, apperr.ErrNotFound
	}
	q := url.Values{"filename": {f.Filename}, "subfolder": {f.Subfolder}, "type": {f.Type}}
	vctx, cancel := context.WithTimeout(ctx, ViewTimeout)
	hreq, err := http.NewRequestWithContext(vctx, http.MethodGet, "/view?"+q.Encode(), nil)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := r.client.do(vctx, rt.up, rt.port, hreq)
	if err != nil {
		cancel()
		return nil, err
	}
	fail := func(err error) (*File, error) {
		resp.Body.Close()
		cancel()
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fail(apperr.ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		return fail(badUpstream("/view 返回 %d", resp.StatusCode))
	case resp.ContentLength > MaxViewBytes:
		return fail(ErrFileTooLarge)
	}
	disp := mime.FormatMediaType("inline", map[string]string{"filename": f.Filename})
	if disp == "" {
		disp = "inline"
	}
	return &File{
		Body:        &limitedBody{r: resp.Body, left: MaxViewBytes, closeFn: func() error { err := resp.Body.Close(); cancel(); return err }},
		ContentType: ctype,
		Length:      resp.ContentLength,
		Filename:    f.Filename,
		Disposition: disp,
	}, nil
}

// limitedBody 最多读 left 字节；还有更多时返回 ErrFileTooLarge。
type limitedBody struct {
	r       io.Reader
	left    int64
	closeFn func() error
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		n, err := b.r.Read(one[:])
		if n > 0 {
			return 0, ErrFileTooLarge
		}
		if err == nil {
			return 0, nil
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.closeFn() }
