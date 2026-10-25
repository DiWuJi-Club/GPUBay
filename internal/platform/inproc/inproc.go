// Package inproc 提供进程内的 http.RoundTripper：请求不经网络，直接交给同一进程里的 http.Handler 处理。
//
// 用途：AI 管家（计划 12）默认经它调用本进程挂载的 Token 商城网关（计划 9 的 /v1），鉴权、限流、计费与外部调用完全相同，
// 只是省掉了回环连接。响应体经 io.Pipe 逐段交给调用方：处理器每写一段（SSE 事件 + Flush）调用方就能读到，
// 不会像 httptest.ResponseRecorder 那样等处理器返回才拿到全部内容。
//
// 语义尽量贴近 net/http 服务端：
//   - 处理器看到的请求：RemoteAddr = Transport.RemoteAddr（缺省 127.0.0.1:0）、Host = Transport.Host（缺省请求 URL 的主机）、
//     URL 只有 Path / RawQuery、RequestURI 已填、Proto HTTP/1.1。
//   - 处理器的请求 ctx 不带调用方 ctx 的任何值（与 net/http 服务端一样；调用方的 chi 路由状态等不会漏进处理器），
//     只继承取消与期限：调用方 ctx 取消 = 客户端断开，处理器的请求 ctx 随之取消，之后的 Write 返回错误。
//   - 调用方在处理器返回之前关闭响应体（如读到 SSE 的 [DONE] 就关）：处理器之后的 Write 立即失败；请求 ctx 再保留 CloseGrace，
//     处理器在此之前返回则不取消——网关在 [DONE] 之后读掉上游剩余字节、记账的收尾不被打断（与真实网络上服务端已写出的字节进了
//     内核缓冲、不会立刻发现断开相同）；期满仍未返回则取消。
//   - 处理器第一次 WriteHeader / Write（或返回）时 RoundTrip 返回响应头；之后再改 Header 不影响已返回的响应（与 net/http 相同）。
//   - 处理器返回时响应体读到 EOF；处理器 panic 时响应体读到错误（http.ErrAbortHandler 之外的 panic 记一条错误日志），进程不崩溃。
//   - 不支持 Hijack、读写期限（http.ResponseController 的 SetReadDeadline / SetWriteDeadline 返回 http.ErrNotSupported）。
//   - 管道无缓冲：处理器的 Write 阻塞到调用方读走为止。调用方不读（卡住）也不取消时，处理器一直阻塞在 Write 上——
//     没有 net/http 的写超时，处理器自己的停机（如网关 Shutdown 取消在途转发后写错误事件）也打断不了它；
//     只能靠调用方 ctx 的期限 / 取消解除（管家每次模型请求都有 LLMTimeout）。
package inproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
)

// Transport 把请求交给 Handler 处理的 http.RoundTripper。零值不可用：Handler 必填。并发安全。
type Transport struct {
	Handler    http.Handler
	Host       string       // 处理器看到的 r.Host；空 = 请求 URL 的 Host
	RemoteAddr string       // 处理器看到的 r.RemoteAddr；空 = "127.0.0.1:0"
	Log        *slog.Logger // 处理器 panic 时记日志；nil = slog.Default()
}

// DefaultRemoteAddr 处理器看到的默认对端地址（httpx.ClientIP → 127.0.0.1）。
const DefaultRemoteAddr = "127.0.0.1:0"

// ErrHandlerPanic 处理器 panic 时响应体（或 RoundTrip，若还没写响应头）返回的错误。
var ErrHandlerPanic = errors.New("inproc: handler panicked")

var errWriteAfterFinish = errors.New("inproc: write after handler returned")

// RoundTrip 实现 http.RoundTripper。按约定总会关闭请求体（处理器返回后）。
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.Handler == nil {
		closeBody(req)
		return nil, errors.New("inproc: nil Handler")
	}
	if req.URL == nil {
		closeBody(req)
		return nil, errors.New("inproc: nil Request.URL")
	}
	ctx, cancel := handlerContext(req.Context())
	sreq := req.Clone(ctx)
	sreq.URL = &url.URL{Path: req.URL.Path, RawPath: req.URL.RawPath, RawQuery: req.URL.RawQuery}
	sreq.RequestURI = req.URL.RequestURI()
	sreq.Host = t.Host
	if sreq.Host == "" {
		sreq.Host = req.URL.Host
	}
	sreq.RemoteAddr = t.RemoteAddr
	if sreq.RemoteAddr == "" {
		sreq.RemoteAddr = DefaultRemoteAddr
	}
	sreq.Proto, sreq.ProtoMajor, sreq.ProtoMinor = "HTTP/1.1", 1, 1
	sreq.Close = false
	if sreq.Body == nil {
		sreq.Body = http.NoBody
	}
	if req.ContentLength == 0 && req.Body != nil && req.Body != http.NoBody {
		sreq.ContentLength = -1 // 客户端未知长度：与服务端读到的 chunked 请求一致
	}

	pr, pw := io.Pipe()
	w := &writer{header: http.Header{}, pw: pw, headerDone: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer closeBody(req)
		defer func() {
			if rec := recover(); rec != nil {
				if rec != http.ErrAbortHandler { //nolint:errorlint // 与 net/http 约定一致：ErrAbortHandler 只中断、不记日志
					log := t.Log
					if log == nil {
						log = slog.Default()
					}
					log.Error("inproc: handler panic", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()),
						"method", sreq.Method, "path", sreq.URL.Path)
				}
				w.finish(ErrHandlerPanic)
				return
			}
			w.finish(nil)
		}()
		t.Handler.ServeHTTP(w, sreq)
	}()

	// 调用方取消 ctx（含 http.Client 超时）：像 net/http 断开连接一样关闭管道——调用方的 Read 立即返回 ctx 错误，
	// 处理器阻塞中的 Write 返回错误（处理器同时从请求 ctx 看到取消）。处理器返回后这个 goroutine 退出。
	go func() {
		select {
		case <-req.Context().Done():
			_ = pw.CloseWithError(req.Context().Err()) // 写端关闭：调用方读完已写出的内容后得到 ctx 错误，处理器的 Write 得到 io.ErrClosedPipe
		case <-ctx.Done():
			_ = pw.CloseWithError(ctx.Err())
		case <-done:
		}
	}()
	select {
	case <-w.headerDone:
	case <-req.Context().Done():
		// 响应头之前客户端放弃：取消处理器、让它之后的写立即失败；不等处理器返回
		cancel()
		_ = pw.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
	if w.panicked {
		cancel()
		_ = pr.Close()
		return nil, ErrHandlerPanic
	}
	resp := &http.Response{
		Status:        strconv.Itoa(w.status) + " " + http.StatusText(w.status),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.sent,
		Body:          &body{pr: pr, cancel: cancel, done: done},
		ContentLength: -1,
		Request:       req,
	}
	if n, err := strconv.ParseInt(w.sent.Get("Content-Length"), 10, 64); err == nil && n >= 0 {
		resp.ContentLength = n
	}
	if req.Method == http.MethodHead {
		resp.Body = http.NoBody
		go func() { <-done; cancel() }()
		_ = pr.Close()
	}
	return resp, nil
}

// handlerContext 返回处理器的请求 ctx：与 net/http 服务端一样不继承调用方 ctx 的任何值，只继承取消（context.AfterFunc）
// 与期限（调用方 ctx 有 deadline 时照搬）。调用方的值不能漏进处理器：调用方常常本身就在一个 chi 处理器里（管家的控制台 SSE），
// 它 ctx 里的 chi.RouteContext 会让处理器里的 chi 路由按外层的路由状态匹配（全部 404），外层处理器返回后 chi 还会回收复用
// 这个 RouteContext，而处理器（如网关收尾记账）可能仍在用它 → 数据竞争。返回的 cancel 取消处理器 ctx 并解除与调用方 ctx 的关联。
func handlerContext(parent context.Context) (context.Context, context.CancelFunc) {
	base, cancelDeadline := context.Background(), context.CancelFunc(func() {})
	if dl, ok := parent.Deadline(); ok {
		base, cancelDeadline = context.WithDeadline(base, dl)
	}
	ctx, cancel := context.WithCancel(base)
	stop := context.AfterFunc(parent, cancel)
	return ctx, func() { stop(); cancel(); cancelDeadline() }
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// writer 是处理器看到的 http.ResponseWriter（+ http.Flusher）。
type writer struct {
	mu         sync.Mutex
	header     http.Header // 处理器可改的头
	sent       http.Header // WriteHeader 时的快照（返回给调用方）
	status     int
	wrote      bool // 已确定响应头
	finished   bool
	panicked   bool // 写响应头之前 panic
	pw         *io.PipeWriter
	headerDone chan struct{}
}

func (w *writer) Header() http.Header { return w.header }

// WriteHeader 确定响应头。1xx（101 之外）忽略；重复调用忽略。
func (w *writer) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeHeaderLocked(code)
}

func (w *writer) writeHeaderLocked(code int) {
	if w.wrote || w.finished {
		return
	}
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		return
	}
	if code < 100 || code > 999 {
		panic(fmt.Sprintf("invalid WriteHeader code %v", code))
	}
	w.status = code
	w.sent = w.header.Clone()
	w.wrote = true
	close(w.headerDone)
}

// Write 写响应体；阻塞到调用方读走（io.Pipe 无缓冲），调用方断开后返回错误。
func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.finished {
		w.mu.Unlock()
		return 0, errWriteAfterFinish
	}
	if !w.wrote {
		if w.header.Get("Content-Type") == "" && len(p) > 0 && w.header.Get("Transfer-Encoding") == "" {
			w.header.Set("Content-Type", http.DetectContentType(p))
		}
		w.writeHeaderLocked(http.StatusOK)
	}
	w.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	return w.pw.Write(p)
}

// Flush 实现 http.Flusher：管道无缓冲，Write 返回时调用方已读到，这里只需确定响应头。
func (w *writer) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wrote {
		w.writeHeaderLocked(http.StatusOK)
	}
}

// finish 处理器返回（或 panic）：没写过响应头时补 200（panic 时不补），然后关闭管道（panic → 调用方读到错误）。
func (w *writer) finish(err error) {
	w.mu.Lock()
	if !w.wrote {
		if err != nil {
			w.panicked = true
			w.wrote = true
			close(w.headerDone)
		} else {
			w.writeHeaderLocked(http.StatusOK)
		}
	}
	w.finished = true
	w.mu.Unlock()
	if err != nil {
		_ = w.pw.CloseWithError(err)
		return
	}
	_ = w.pw.Close()
}

// CloseGrace：调用方在处理器返回之前关闭响应体后，处理器的请求 ctx 最多再保持这么久才取消（处理器先返回则不取消）。
// 真实网络上客户端读到 [DONE] 就关连接时，服务端已写出的字节进了内核缓冲、ctx 不会立刻取消，网关得以在 [DONE] 之后
// 读掉上游剩余字节（llmgw 的 drainTimeout 2 秒）、让节点连接复用；这里给同样的余量。之后处理器再 Write 立即失败
// （管道读端已关），所以仍在生成的流照样马上发现客户端断开、取消上游。
const CloseGrace = 2 * time.Second

// errClientClosed 调用方关闭响应体之后处理器再 Write 得到的错误（相当于连接已断开）。
var errClientClosed = errors.New("inproc: client closed the response body")

// body 是返回给调用方的响应体。Close 早于 EOF = 客户端断开：处理器之后的 Write 失败，请求 ctx 在 CloseGrace 内随处理器返回释放、否则取消。
type body struct {
	pr     *io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (b *body) Read(p []byte) (int, error) { return b.pr.Read(p) }

func (b *body) Close() error {
	b.once.Do(func() {
		_ = b.pr.CloseWithError(errClientClosed) // 处理器之后的 Write 返回 errClientClosed；调用方之后的 Read 返回 io.ErrClosedPipe
		select {
		case <-b.done: // 处理器已返回：正常读完（或没读完但内容已全部写出）
			b.cancel()
			return
		default:
		}
		// 处理器还没返回：已写出的内容调用方都读到了（管道无缓冲），处理器多半在收尾（如网关在 [DONE] 后读掉上游剩余字节、记账）。
		// 给它 CloseGrace 自然结束，期满仍未返回按客户端断开取消 ctx。
		go func() {
			t := time.NewTimer(CloseGrace)
			defer t.Stop()
			select {
			case <-b.done:
			case <-t.C:
			}
			b.cancel()
		}()
	})
	return nil
}
