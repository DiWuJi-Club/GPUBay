package comfywf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/platform/apperr"
)

// DialFunc 拨到实例容器内 127.0.0.1:<port>；生产是 upstream.Pool.Dial（与 Runner 健康检查同一条 SSH 通道）。
type DialFunc func(ctx context.Context, t upstream.Target, port int) (net.Conn, error)

// Client 是到容器内 ComfyUI 的 HTTP 客户端（快速出图代理）。每次请求一条新通道（DisableKeepAlives），不跟随重定向；
// 容器内进程不可信：响应头 ≤ 64KB，响应体由调用方限长读取。
type Client struct {
	dial DialFunc
}

// NewClient 经 upstream.Pool 拨号。
func NewClient(up *upstream.Pool) *Client { return &Client{dial: up.Dial} }

// NewClientWithDialer 用指定的拨号函数（测试直连 httptest / comfytest）。
func NewClientWithDialer(dial DialFunc) *Client { return &Client{dial: dial} }

const (
	// maxJSONBytes 容器内 JSON 响应（/prompt、/history、/upload/image）的上限（规格 §6.1）。
	maxJSONBytes = 1 << 20
	// maxQueueBytes /queue 的上限：queue_pending 里带着每个任务的完整 prompt（各 ≤ 256KB），1MB 太紧；超出按「排队中」处理。
	maxQueueBytes = 4 << 20
	// maxHeaderBytes 容器内响应头上限。
	maxHeaderBytes = 64 << 10
)

// errBadUpstream 容器内 ComfyUI 的响应无法使用（非预期状态码、不是 JSON、超长、字段不合法）。沿用 upstream_unreachable 码，
// 状态 502：前端对这两种情况的处理相同（稍后重试 / 打开 ComfyUI 查看）。
var errBadUpstream = apperr.New(apperr.ErrUpstreamUnreachable.Code, "ComfyUI 返回了无法处理的响应", 502)

func badUpstream(format string, args ...any) error {
	return fmt.Errorf("%w (%s)", errBadUpstream, fmt.Sprintf(format, args...))
}

// do 发一个请求到容器内 127.0.0.1:<port>（req 的 URL 只取路径与查询串）。返回的响应由调用方关闭 Body。
// 传输层错误（拨号失败、端口未监听、超时、连接中断）一律 → apperr.ErrUpstreamUnreachable（包装原因）；
// 调用方的 ctx 被取消 → ctx.Err()。
func (c *Client) do(ctx context.Context, t upstream.Target, port int, req *http.Request) (*http.Response, error) {
	hc := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: maxHeaderBytes,
			DisableCompression:     true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return c.dial(ctx, t, port)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req.URL.Scheme, req.URL.Host, req.Host = "http", "127.0.0.1:"+strconv.Itoa(port), "127.0.0.1:"+strconv.Itoa(port)
	resp, err := hc.Do(req.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", apperr.ErrUpstreamUnreachable, err)
	}
	return resp, nil
}

// call 发请求并读完响应体（≤ limit，超出 → errBadUpstream）；超时 ProxyTimeout。
func (c *Client) call(ctx context.Context, t upstream.Target, port int, method, path string, body io.Reader, contentType string, limit int64) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ProxyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.do(ctx, t, port, req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := readLimited(resp.Body, limit)
	if err != nil {
		if errors.Is(err, errBadUpstream) {
			return resp.StatusCode, nil, err
		}
		return resp.StatusCode, nil, fmt.Errorf("%w: 读取响应: %v", apperr.ErrUpstreamUnreachable, err)
	}
	return resp.StatusCode, b, nil
}

// readLimited 读完 r，超过 limit 字节 → errBadUpstream。
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, badUpstream("响应超过 %d 字节", limit)
	}
	return b, nil
}
