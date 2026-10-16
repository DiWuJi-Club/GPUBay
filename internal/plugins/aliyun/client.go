// Package aliyun 实现阿里云 RPC 风格 API（签名版本 1.0，HMAC-SHA1）的最小客户端，供短信与号码百科插件使用。
package aliyun

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Client struct {
	Endpoint        string // 例如 https://dysmsapi.aliyuncs.com
	AccessKeyID     string
	AccessKeySecret string
	HTTP            *http.Client     // 为空时使用 10 秒超时的默认客户端
	Now             func() time.Time // 测试可替换
	Nonce           func() string    // 测试可替换
}

// Error 是阿里云以非 200 状态返回的错误。
type Error struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *Error) Error() string {
	return fmt.Sprintf("aliyun %d %s: %s (request %s)", e.Status, e.Code, e.Message, e.RequestID)
}

var defaultHTTP = &http.Client{Timeout: 10 * time.Second}

// Call 以 GET 调用 action，并把 JSON 响应解码到 out。
func (c *Client) Call(ctx context.Context, action, version string, params map[string]string, out any) error {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	nonce := uuid.NewString
	if c.Nonce != nil {
		nonce = c.Nonce
	}
	q := map[string]string{
		"AccessKeyId":      c.AccessKeyID,
		"Action":           action,
		"Format":           "JSON",
		"RegionId":         "cn-hangzhou",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   nonce(),
		"SignatureVersion": "1.0",
		"Timestamp":        now().UTC().Format("2006-01-02T15:04:05Z"),
		"Version":          version,
	}
	for k, v := range params {
		q[k] = v
	}
	query := canonicalQuery(q) + "&Signature=" + percentEncode(Sign(c.AccessKeySecret, http.MethodGet, q))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.Endpoint, "/")+"/?"+query, nil)
	if err != nil {
		return err
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("aliyun %s: %w", action, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("aliyun %s: read response: %w", action, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Code, Message, RequestId string }
		_ = json.Unmarshal(body, &e)
		return &Error{Status: resp.StatusCode, Code: e.Code, Message: e.Message, RequestID: e.RequestId}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("aliyun %s: decode response: %w", action, err)
	}
	return nil
}

// percentEncode 按阿里云要求编码：空格为 %20，* 为 %2A，~ 不编码。
func percentEncode(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "*", "%2A")
	return strings.ReplaceAll(e, "%7E", "~")
}

func canonicalQuery(q map[string]string) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = percentEncode(k) + "=" + percentEncode(q[k])
	}
	return strings.Join(parts, "&")
}

// Sign 计算 RPC 签名：Base64(HMAC-SHA1(secret + "&", METHOD + "&%2F&" + percentEncode(规范化参数)))。
func Sign(secret, method string, params map[string]string) string {
	stringToSign := method + "&" + percentEncode("/") + "&" + percentEncode(canonicalQuery(params))
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
