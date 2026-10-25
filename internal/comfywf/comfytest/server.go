// Package comfytest 是一个假的 ComfyUI HTTP 服务（只实现快速出图代理用到的端点），供 comfywf 单测、集成测试与
// e2e-server 复用。Server 本身是 http.Handler：单测用 Start（httptest），e2e 把它挂到假容器的 8188 上。线程安全。
//
// 端点与真实 ComfyUI（v0.3.60）的响应形状一致：
//   - GET  /                    200 小 HTML 页（标题 Fake ComfyUI）
//   - GET  /system_stats        200 {}
//   - POST /prompt              记录提交；默认 200 {"prompt_id", "number", "node_errors": {}}；RejectPrompts / RejectMissingNode
//     后 400；缺 prompt → 400 no_prompt（node_errors 是数组，同真实 ComfyUI）
//   - GET  /history/{prompt_id} 按 SetHistory：排队 / 运行中 → {}；完成 → outputs + status.completed；出错 / 中断 → status_str = error。
//     条目里的 prompt 字段回显真实提交的 prompt 与 client_id
//   - GET  /queue               Running 的放 queue_running，Queued 的放 queue_pending（元素同真实形状 [number, id, prompt, extra, outputs]）
//   - GET  /view                只认某个 Done 条目里的文件（否则 404）；默认 1×1 PNG，Content-Type 按扩展名猜（同 mimetypes）、
//     带 Content-Disposition: filename="…"；SetView 可换成任意类型 / 大 body / 不带长度；200 且带长度时经 http.ServeContent
//     支持 Range（206 + Content-Range、Accept-Ranges: bytes，同 aiohttp FileResponse）
//   - POST /upload/image        multipart image + subfolder + overwrite → {name, subfolder, type: "input"}
package comfytest

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 运行状态（HistoryState.State）。
const (
	Queued  = "queued"
	Running = "running"
	Done    = "done"
	Error   = "error"
)

// File 一个输出文件（history outputs[node].images[] 的一项）。Type 为空 = "output"。
type File struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// HistoryState 某个 prompt 的状态。Files 只对 Done 有意义，Message / Interrupted 只对 Error 有意义。
type HistoryState struct {
	State       string
	Files       []File
	Message     string
	Interrupted bool // execution_interrupted（用户在 ComfyUI 里取消）而不是 execution_error
}

// QueuedState 已提交、尚未开始（history 为空，在 queue_pending 里）。
func QueuedState() HistoryState { return HistoryState{State: Queued} }

// RunningState 执行中（history 为空，在 queue_running 里）。
func RunningState() HistoryState { return HistoryState{State: Running} }

// DoneState 完成，输出为 files。
func DoneState(files ...File) HistoryState { return HistoryState{State: Done, Files: files} }

// ErrorState 执行失败（execution_error.exception_message = msg）。
func ErrorState(msg string) HistoryState { return HistoryState{State: Error, Message: msg} }

// InterruptedState 被中断（status_str = error，messages 里是 execution_interrupted）。
func InterruptedState() HistoryState { return HistoryState{State: Error, Interrupted: true} }

// PNG 一个 type = output 的图片输出。
func PNG(name string) File { return File{Filename: name, Type: "output"} }

// outputClasses 输出节点类型（挑 prompt 里的输出节点 id 用）。
var outputClasses = []string{"SaveImage", "SaveAnimatedWEBP", "SaveWEBM", "SaveVideo", "VHS_VideoCombine"}

func cloneValues(v url.Values) url.Values { return url.Values(http.Header(v).Clone()) }

func jsonOut(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
}

// DoneN 完成且有 n 个 PNG 输出（ComfyUI_00001_.png …）。
func DoneN(n int) HistoryState {
	files := make([]File, n)
	for i := range files {
		files[i] = PNG(fmt.Sprintf("ComfyUI_%05d_.png", i+1))
	}
	return DoneState(files...)
}

// Prompt 一次 POST /prompt 的提交。
type Prompt struct {
	ID       string          // 返回给调用方的 prompt_id
	ClientID string          // 请求里的 client_id（快速出图 = run id）
	Prompt   json.RawMessage // 请求里的 prompt 原文
}

// Upload 一次 POST /upload/image。
type Upload struct {
	Filename  string
	Subfolder string
	Type      string
	Overwrite string
	Size      int
}

// ViewConfig 配置 GET /view 的响应。Size > 0 时流式返回 Size 个字节（不占内存，忽略 Body）；NoLength 时不带 Content-Length。
type ViewConfig struct {
	ContentType string // 空 = image/png
	Body        []byte // 空 = 1×1 PNG
	Size        int64
	NoLength    bool
	Status      int // 0 = 200
}

// OnePixelPNG 一张 1×1 透明 PNG。
var OnePixelPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

type entry struct {
	state      HistoryState
	outputNode string
	number     int
	prompt     json.RawMessage // 提交的 prompt 原文（SetHistory 建的条目为 {}）
	clientID   string
}

// reject 的种类。
const (
	rejectNone = iota
	rejectNode
	rejectMissingNode
)

// Server 假 ComfyUI。零值不可用，用 New。
type Server struct {
	mu         sync.Mutex
	prompts    []Prompt
	entries    map[string]*entry
	byClient   map[string]string
	rejectKind int
	reject     string
	uploads    []Upload
	uploadName string
	view       ViewConfig
	views      []url.Values
	requests   []string
}

func New() *Server {
	return &Server{entries: map[string]*entry{}, byClient: map[string]string{}}
}

// Start 新建 Server 并起一个 httptest.Server（调用方 Close）。
func Start() (*Server, *httptest.Server) {
	s := New()
	return s, httptest.NewServer(s)
}

// SetHistory 设置 prompt 的状态（未提交过的 id 也可设置，输出节点按 "9"）。
func (s *Server) SetHistory(promptID string, h HistoryState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[promptID]
	if e == nil {
		e = &entry{outputNode: "9", number: len(s.entries), prompt: json.RawMessage(`{}`)}
		s.entries[promptID] = e
	}
	h.Files = slices.Clone(h.Files)
	e.state = h
}

// Forget 删除 prompt 的历史（模拟 ComfyUI 重启：history 与队列都没有它）。
func (s *Server) Forget(promptID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, promptID)
}

// PromptIDForClient 按提交时的 client_id（run id）找 prompt_id；没有 → ""。
func (s *Server) PromptIDForClient(clientID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byClient[clientID]
}

// RejectPrompts 之后的 POST /prompt 返回 400 + node_errors（消息为 msg）；msg 为空恢复正常。
func (s *Server) RejectPrompts(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectKind, s.reject = rejectNode, msg
	if msg == "" {
		s.rejectKind = rejectNone
	}
}

// RejectMissingNode 之后的 POST /prompt 返回 400 missing_node_type（node_errors 为空，消息在 error.message / details，
// 同 ComfyUI 遇到未安装的自定义节点）；nodeType 为空恢复正常。
func (s *Server) RejectMissingNode(nodeType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectKind, s.reject = rejectMissingNode, nodeType
	if nodeType == "" {
		s.rejectKind = rejectNone
	}
}

// SetUploadName 之后 /upload/image 返回的 name 固定为 name（测试平台对返回名的校验）；空 = 原样返回上传的文件名。
func (s *Server) SetUploadName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploadName = name
}

// SetView 配置 /view 的响应。
func (s *Server) SetView(v ViewConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v.Body = append([]byte(nil), v.Body...)
	s.view = v
}

// Prompts 已收到的提交（副本）。
func (s *Server) Prompts() []Prompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.prompts)
}

// Uploads 已收到的上传（副本）。
func (s *Server) Uploads() []Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.uploads)
}

// Views 已收到的 /view 查询参数（副本）。
func (s *Server) Views() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]url.Values, len(s.views))
	for i, v := range s.views {
		out[i] = cloneValues(v)
	}
	return out
}

// maxRequests Requests 保留的最近请求数。
const maxRequests = 200

// fakeIndexHTML 是 GET / 的页面（真实 ComfyUI 是前端 SPA）。
const fakeIndexHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Fake ComfyUI</title></head><body><h1>Fake ComfyUI</h1></body></html>`

// Requests 已收到的请求（"METHOD /path"，不含查询串；只保留最近 maxRequests 条）。
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Reset 清空全部状态与配置。
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts, s.uploads, s.views, s.requests = nil, nil, nil, nil
	s.entries, s.byClient = map[string]*entry{}, map[string]string{}
	s.rejectKind, s.reject, s.uploadName, s.view = rejectNone, "", "", ViewConfig{}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	if len(s.requests) > maxRequests { // e2e-server 长时间运行（健康检查每轮打 /system_stats）：只留最近的
		s.requests = slices.Clone(s.requests[len(s.requests)-maxRequests:])
	}
	s.mu.Unlock()
	switch p := r.URL.Path; {
	case p == "/" && r.Method == http.MethodGet: // 「打开 ComfyUI」经 ingress 打开的页面
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, fakeIndexHTML)
	case p == "/system_stats" && r.Method == http.MethodGet:
		jsonOut(w, http.StatusOK)
		io.WriteString(w, `{"system":{"comfyui_version":"0.3.60-fake"},"devices":[]}`)
	case p == "/prompt" && r.Method == http.MethodPost:
		s.prompt(w, r)
	case strings.HasPrefix(p, "/history/") && r.Method == http.MethodGet:
		s.history(w, strings.TrimPrefix(p, "/history/"))
	case p == "/queue" && r.Method == http.MethodGet:
		s.queue(w)
	case p == "/view" && r.Method == http.MethodGet:
		s.serveView(w, r)
	case p == "/upload/image" && r.Method == http.MethodPost:
		s.upload(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) prompt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt   json.RawMessage `json:"prompt"`
		ClientID string          `json:"client_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		jsonOut(w, http.StatusBadRequest)
		io.WriteString(w, `{"error":{"type":"invalid_prompt","message":"Cannot parse prompt","details":"","extra_info":{}},"node_errors":[]}`)
		return
	}
	if len(req.Prompt) == 0 || string(req.Prompt) == "null" {
		jsonOut(w, http.StatusBadRequest)
		io.WriteString(w, `{"error":{"type":"no_prompt","message":"No prompt provided","details":"No prompt provided","extra_info":{}},"node_errors":[]}`)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejectKind == rejectMissingNode {
		body, _ := json.Marshal(map[string]any{
			"error": map[string]any{"type": "missing_node_type",
				"message": fmt.Sprintf("Node '%s' not found. The custom node may not be installed.", s.reject),
				"details": "Node ID '#12'", "extra_info": map[string]any{"node_id": "12", "class_type": s.reject, "node_title": s.reject}},
			"node_errors": map[string]any{},
		})
		jsonOut(w, http.StatusBadRequest)
		w.Write(body)
		return
	}
	if s.rejectKind == rejectNode {
		body, _ := json.Marshal(map[string]any{
			"error": map[string]any{"type": "prompt_outputs_failed_validation", "message": "Prompt outputs failed validation", "details": "", "extra_info": map[string]any{}},
			"node_errors": map[string]any{"4": map[string]any{
				"errors":            []any{map[string]any{"type": "value_not_in_list", "message": s.reject, "details": "", "extra_info": map[string]any{}}},
				"dependent_outputs": []string{"9"}, "class_type": "CheckpointLoaderSimple"}},
		})
		jsonOut(w, http.StatusBadRequest)
		w.Write(body)
		return
	}
	id := uuid()
	n := len(s.prompts)
	s.prompts = append(s.prompts, Prompt{ID: id, ClientID: req.ClientID, Prompt: append(json.RawMessage(nil), req.Prompt...)})
	s.entries[id] = &entry{state: QueuedState(), outputNode: outputNode(req.Prompt), number: n,
		prompt: append(json.RawMessage(nil), req.Prompt...), clientID: req.ClientID}
	if req.ClientID != "" {
		s.byClient[req.ClientID] = id
	}
	jsonOut(w, http.StatusOK)
	fmt.Fprintf(w, `{"prompt_id":%q,"number":%d,"node_errors":{}}`, id, n)
}

// outputNode 取 prompt 里第一个输出节点（按 id 数值序）；没有 → "9"。
func outputNode(raw json.RawMessage) string {
	var p map[string]struct {
		ClassType string `json:"class_type"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return "9"
	}
	ids := make([]string, 0, len(p))
	for id, n := range p {
		if slices.Contains(outputClasses, n.ClassType) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "9"
	}
	slices.SortFunc(ids, func(a, b string) int {
		ai, aerr := strconv.Atoi(a)
		bi, berr := strconv.Atoi(b)
		if aerr == nil && berr == nil {
			return ai - bi
		}
		return strings.Compare(a, b)
	})
	return ids[0]
}

func (s *Server) history(w http.ResponseWriter, id string) {
	s.mu.Lock()
	e := s.entries[id]
	var st HistoryState
	var cp entry
	if e != nil {
		cp = *e
		st = e.state
		st.Files = slices.Clone(st.Files)
	}
	node := cp.outputNode
	s.mu.Unlock()
	jsonOut(w, http.StatusOK)
	if e == nil || (st.State != Done && st.State != Error) {
		io.WriteString(w, `{}`)
		return
	}
	images := make([]File, len(st.Files))
	for i, f := range st.Files {
		if f.Type == "" {
			f.Type = "output"
		}
		images[i] = f
	}
	item := map[string]any{
		"prompt": cp.queueItem(id),
		"meta":   map[string]any{},
	}
	if st.State == Done {
		outputs := map[string]any{}
		if len(images) > 0 {
			outputs[node] = map[string]any{"images": images}
		}
		item["outputs"] = outputs
		item["status"] = map[string]any{"status_str": "success", "completed": true, "messages": []any{
			[]any{"execution_start", map[string]any{"prompt_id": id}},
			[]any{"execution_success", map[string]any{"prompt_id": id}},
		}}
	} else {
		last := []any{"execution_error", map[string]any{"prompt_id": id, "node_id": "3", "node_type": "KSampler",
			"exception_message": st.Message, "exception_type": "RuntimeError", "traceback": []string{}}}
		if st.Interrupted {
			last = []any{"execution_interrupted", map[string]any{"prompt_id": id, "node_id": "3", "node_type": "KSampler", "executed": []string{}}}
		}
		item["outputs"] = map[string]any{}
		item["status"] = map[string]any{"status_str": "error", "completed": false, "messages": []any{
			[]any{"execution_start", map[string]any{"prompt_id": id}}, last,
		}}
	}
	body, _ := json.Marshal(map[string]any{id: item})
	w.Write(body)
}

func (s *Server) queue(w http.ResponseWriter) {
	s.mu.Lock()
	running, pending := []any{}, []any{}
	for id, e := range s.entries {
		item := e.queueItem(id)
		switch e.state.State {
		case Running:
			running = append(running, item)
		case Queued:
			pending = append(pending, item)
		}
	}
	s.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"queue_running": running, "queue_pending": pending})
	jsonOut(w, http.StatusOK)
	w.Write(body)
}

// queueItem 真实 ComfyUI 的队列项 / history 条目 prompt 字段：[number, prompt_id, prompt, extra_data, outputs_to_execute]。
func (e entry) queueItem(id string) []any {
	prompt := e.prompt
	if len(prompt) == 0 {
		prompt = json.RawMessage(`{}`)
	}
	return []any{e.number, id, prompt, map[string]any{"client_id": e.clientID}, []string{e.outputNode}}
}

// hasOutput 文件是否出现在某个 Done 条目里（真实 ComfyUI 的 /view 只能取到磁盘上已有的文件）。
func (s *Server) hasOutput(q url.Values) bool {
	f := File{Filename: q.Get("filename"), Subfolder: q.Get("subfolder"), Type: cmp.Or(q.Get("type"), "output")}
	for _, e := range s.entries {
		if e.state.State != Done {
			continue
		}
		for _, x := range e.state.Files {
			if x.Type == "" {
				x.Type = "output"
			}
			if x == f {
				return true
			}
		}
	}
	return false
}

func (s *Server) serveView(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	s.views = append(s.views, cloneValues(q))
	v := s.view
	found := s.hasOutput(q)
	s.mu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	name := q.Get("filename")
	w.Header().Set("Content-Type", cmp.Or(v.ContentType, mime.TypeByExtension(path.Ext(name)), "application/octet-stream"))
	w.Header().Set("Content-Disposition", fmt.Sprintf("filename=%q", name))
	status := v.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status == http.StatusOK && !v.NoLength {
		// 同真实 ComfyUI（aiohttp FileResponse）：支持单段 / 多段 Range（206 + Content-Range，Accept-Ranges: bytes）
		body := v.Body
		if len(body) == 0 {
			body = OnePixelPNG
		}
		var rs io.ReadSeeker = bytes.NewReader(body)
		if v.Size > 0 {
			rs = io.NewSectionReader(zeros{}, 0, v.Size)
		}
		http.ServeContent(w, r, "", time.Time{}, rs)
		return
	}
	if v.Size > 0 {
		if !v.NoLength {
			w.Header().Set("Content-Length", strconv.FormatInt(v.Size, 10))
		}
		w.WriteHeader(status)
		io.CopyN(w, zeros{}, v.Size)
		return
	}
	body := v.Body
	if len(body) == 0 {
		body = OnePixelPNG
	}
	if !v.NoLength {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	if v.NoLength {
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // 先发头，强制 chunked
		}
	}
	w.Write(body)
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func (zeros) ReadAt(p []byte, _ int64) (int, error) { return zeros{}.Read(p) }

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "bad multipart", http.StatusBadRequest)
		return
	}
	f, hdr, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "no image", http.StatusBadRequest)
		return
	}
	defer f.Close()
	n, _ := io.Copy(io.Discard, f)
	up := Upload{Filename: hdr.Filename, Subfolder: r.FormValue("subfolder"), Type: cmp.Or(r.FormValue("type"), "input"),
		Overwrite: r.FormValue("overwrite"), Size: int(n)}
	s.mu.Lock()
	s.uploads = append(s.uploads, up)
	name := cmp.Or(s.uploadName, up.Filename)
	s.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"name": name, "subfolder": up.Subfolder, "type": up.Type})
	jsonOut(w, http.StatusOK)
	w.Write(body)
}

// uuid 随机 UUID v4 字符串（ComfyUI 的 prompt_id 形状）。
func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
