package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/butler"
	"spark-cloud/internal/platform/apperr"
)

// AI 管家（计划 12，规格 §5.4 / §6.2 / §10.1）。业务全部在 butler.Service：本文件只做鉴权后的参数解析、限流与 SSE 输出。
// 两个 SSE 端点（发消息 / 确认卡）在流建立之前的错误（校验、关闭、额度、busy、轮数上限、not_found、确认失效）
// 以普通 JSON 错误返回；流建立后（start 事件已写出）问题只以 SSE error 事件报告（由 Service 推送）。

// defaultButlerPing 是 SSE 保活注释间隔（规格 §5.4）。
const defaultButlerPing = 15 * time.Second

// sseWriter 把 butler.Sink 的事件写成 SSE 帧（event: <name>\ndata: <json>\n\n）。
// 首个 Emit 时才写响应头与 200（start），之前 handler 仍可返回 JSON 错误；每帧 Flush；
// 写失败（客户端断开）后 dead，之后的事件静默丢弃，轮次照常跑完并落库。Emit 与保活 ping 并发，由 mu 串行。
type sseWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController // Flush 经它逐层 Unwrap，不要求 w 本身实现 http.Flusher
	mu      sync.Mutex
	dead    bool
	started bool
	closed  bool // handler 即将返回：之后不得再碰 w
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

// start 写响应头、200 与首个注释帧。调用方持有 mu。
func (s *sseWriter) start() {
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	h.Set("Connection", "keep-alive")
	h.Del("Content-Length")
	s.w.WriteHeader(http.StatusOK)
	s.write([]byte(": ok\n\n"))
}

// write 写一帧并 Flush；失败 → dead。调用方持有 mu。
func (s *sseWriter) write(b []byte) {
	if s.dead {
		return
	}
	if _, err := s.w.Write(b); err != nil {
		s.dead = true
		return
	}
	if err := s.rc.Flush(); err != nil {
		s.dead = true
	}
}

// Emit 实现 butler.Sink。
func (s *sseWriter) Emit(event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		data, _ = json.Marshal(butler.ErrorEvent{Code: "internal", Message: "服务器内部错误，请稍后重试"})
		event = butler.EvError
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if !s.started {
		s.start()
	}
	frame := make([]byte, 0, len(event)+len(data)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	s.write(frame)
}

// ping 写保活注释；流尚未建立时不写（不能抢在 JSON 错误之前发 200）。
func (s *sseWriter) ping() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.started {
		return
	}
	s.write([]byte(": ping\n\n"))
}

// close 标记 handler 结束；返回流是否已建立。
func (s *sseWriter) close() (started bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.started
}

func (s *Server) butlerPing() time.Duration {
	if s.ButlerPing > 0 {
		return s.ButlerPing
	}
	return defaultButlerPing
}

// streamButler 以 SSE 跑完一轮（run 即 Send / Confirm）：清除本请求的读写期限（轮次最长 RunTimeout，超过服务器 WriteTimeout），
// 期间每 ButlerPing 一条保活注释；run 在流建立前返回的错误写 JSON。无论正常返回还是 panic，退出前保活协程都已停止、
// sseWriter 已关闭，之后不会再写 w。流建立后 panic：记日志并以 http.ErrAbortHandler 中断连接（Recoverer 原样上抛），
// 不让 Recoverer 把 500 JSON 写进事件流；流建立前 panic 照常交给 Recoverer 返回 500 JSON。
func (s *Server) streamButler(w http.ResponseWriter, r *http.Request, run func(sink butler.Sink) error) {
	rc := http.NewResponseController(w)
	if err := errors.Join(rc.SetReadDeadline(time.Time{}), rc.SetWriteDeadline(time.Time{})); err != nil {
		s.Log.Warn("butler: cannot clear request deadlines", "err", err)
	}
	sw := newSSEWriter(w)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(s.butlerPing())
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sw.ping()
			}
		}
	}()
	var once sync.Once
	started := false
	cleanup := func() {
		once.Do(func() {
			close(stop)
			wg.Wait()
			started = sw.close()
		})
	}
	defer func() {
		rec := recover()
		cleanup()
		if rec == nil {
			return
		}
		if started && rec != http.ErrAbortHandler { //nolint:errorlint // 与 net/http 约定一致
			s.Log.Error("butler: panic after stream start", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()),
				"request_id", middleware.GetReqID(r.Context()), "path", r.URL.Path)
			panic(http.ErrAbortHandler)
		}
		panic(rec)
	}()
	err := run(sw)
	cleanup()
	switch {
	case !started && err != nil && errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// 客户端在流建立前就断开了：没有人读响应，也不是服务端错误
	case !started && err != nil:
		s.writeError(w, r, err)
	case !started:
		// 不应发生（成功的 Send / Confirm 总会先推 start）：给一个空流，前端按「中断」刷新详情
		sw.mu.Lock()
		sw.start()
		sw.mu.Unlock()
	case err != nil:
		// 流已建立（轮次已落库），只剩读回对话失败：记日志，前端按流结束处理
		s.Log.Error("butler: stream finished with error", "err", err, "path", r.URL.Path)
	}
}

// allowButler 发消息 / 确认的每用户限流（规格 §6.2）；在解析请求体之前计数。超限已写 rate_limited。
func (s *Server) allowButler(w http.ResponseWriter, r *http.Request) bool {
	if !s.butlerLim.allow(currentUser(r).ID, s.Clock.Now()) {
		s.writeError(w, r, apperr.ErrRateLimited)
		return false
	}
	return true
}

type butlerStatusView struct {
	butler.StatusView
	IngressDomain string `json:"ingress_domain"` // 前端链接白名单：https://ins-*.<ingress_domain>
}

func (s *Server) handleButlerStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.Butler.Status(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v := butlerStatusView{StatusView: st}
	if s.Ingress != nil {
		v.IngressDomain = s.Ingress.IngressDomain
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleButlerConversations(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n <= 0 {
			s.writeError(w, r, apperr.BadRequest("limit 必须是正整数"))
			return
		}
		limit = n // 超过 ListLimit 由 Service 收到 ListLimit
	}
	convs, err := s.Butler.ListConversations(r.Context(), currentUser(r).ID, limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]butler.ConversationBrief, 0, len(convs))
	for _, c := range convs {
		items = append(items, c.Brief())
	}
	writeJSON(w, http.StatusOK, list(items))
}

func (s *Server) handleButlerConversation(w http.ResponseWriter, r *http.Request) {
	d, err := s.Butler.Detail(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleButlerDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Butler.DeleteConversation(r.Context(), currentUser(r).ID, chi.URLParam(r, "id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}

func (s *Server) handleButlerSend(w http.ResponseWriter, r *http.Request) {
	if !s.allowButler(w, r) {
		return
	}
	var body struct {
		Text    string          `json:"text"`
		Context *butler.Context `json:"context"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	req := butler.SendRequest{UserID: currentUser(r).ID, IP: clientIP(r), ConversationID: chi.URLParam(r, "id"), Text: body.Text}
	if body.Context != nil {
		req.Context = *body.Context
	}
	s.streamButler(w, r, func(sink butler.Sink) error {
		_, err := s.Butler.Send(r.Context(), req, sink)
		return err
	})
}

func (s *Server) handleButlerConfirm(w http.ResponseWriter, r *http.Request) {
	if !s.allowButler(w, r) {
		return
	}
	var body struct {
		Approve *bool `json:"approve"`
	}
	if err := decodeJSON(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	if body.Approve == nil {
		s.writeError(w, r, apperr.BadRequest("缺少 approve"))
		return
	}
	uid, ip := currentUser(r).ID, clientIP(r)
	convID, tcID := chi.URLParam(r, "id"), chi.URLParam(r, "tc")
	s.streamButler(w, r, func(sink butler.Sink) error {
		_, err := s.Butler.Confirm(r.Context(), uid, ip, convID, tcID, *body.Approve, sink)
		return err
	})
}
