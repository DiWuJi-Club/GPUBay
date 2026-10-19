package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"spark-cloud/internal/api"
)

// smsBurst 连发 31 次验证码请求（每小时每 IP 30 次），返回最后一次的状态码。
func smsBurst(t *testing.T, e *testEnv, headers func(i int) map[string]string) int {
	t.Helper()
	var last int
	for i := 0; i < 31; i++ {
		b, _ := json.Marshal(map[string]string{"phone": fmt.Sprintf("1380000%04d", i)})
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/auth/sms-code", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers(i) {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp.StatusCode
	}
	return last
}

// TrustProxy 下控制台客户端 IP 取 X-Forwarded-For 最右项：伪造 True-Client-IP / X-Real-IP / 最左项都换不了 IP（限流照样生效）。
func TestTrustProxyClientIPIgnoresForgedHeaders(t *testing.T) {
	trust := func(s *api.Server) { s.TrustProxy = true }
	for name, h := range map[string]func(int) map[string]string{
		"true-client-ip": func(i int) map[string]string {
			return map[string]string{"X-Forwarded-For": "203.0.113.5", "True-Client-IP": fmt.Sprintf("10.0.0.%d", i+1)}
		},
		"x-real-ip": func(i int) map[string]string {
			return map[string]string{"X-Forwarded-For": "203.0.113.5", "X-Real-IP": fmt.Sprintf("10.0.0.%d", i+1)}
		},
		"leftmost xff": func(i int) map[string]string {
			return map[string]string{"X-Forwarded-For": fmt.Sprintf("10.0.0.%d, 203.0.113.5", i+1)}
		},
	} {
		if last := smsBurst(t, newTestEnvWith(t, trust), h); last != 429 {
			t.Fatalf("%s: forged header changed client ip (31st = %d, want 429)", name, last)
		}
	}
	// 对照：代理写入的最右项不同 → 不同客户端，不限流
	if last := smsBurst(t, newTestEnvWith(t, trust), func(i int) map[string]string {
		return map[string]string{"X-Forwarded-For": fmt.Sprintf("6.6.6.6, 10.0.0.%d", i+1)}
	}); last != 200 {
		t.Fatalf("rightmost xff not used as client ip: 31st = %d", last)
	}
}
