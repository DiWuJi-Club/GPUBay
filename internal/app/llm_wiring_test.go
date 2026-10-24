package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spark-cloud/internal/app"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
)

// app.Build 装配 Token 商城：LLM 服务与 Controller 非 nil；cfg.LLMGateway 为 true 时 /v1 挂网关（无 Key → OpenAI 格式 401），
// 为 false 时不挂（/v1/* 是 API 的 JSON 404）；结算可直接跑。
func TestBuildWiresLLM(t *testing.T) {
	ctx := context.Background()
	for _, gateway := range []bool{true, false} {
		pool := dbtest.New(t)
		cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key,
			SMSDriver: "mock", KYCDriver: "mock", LLMGateway: gateway}
		a, err := app.Build(cfg, pool, discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)})
		if err != nil {
			t.Fatal(err)
		}
		if a.LLM == nil || a.LLMController == nil {
			t.Fatalf("App.LLM = %v, App.LLMController = %v; both must be set", a.LLM, a.LLMController)
		}
		if err := a.LLM.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if err := a.LLM.Router().Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "http://spark.test/v1/models", nil)
		rec := httptest.NewRecorder()
		a.Handler.ServeHTTP(rec, req)
		var body struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if gateway {
			if rec.Code != http.StatusUnauthorized || body.Error.Type != "authentication_error" {
				t.Fatalf("gateway on: GET /v1/models = %d %s, want 401 authentication_error", rec.Code, rec.Body)
			}
		} else if rec.Code != http.StatusNotFound || body.Error.Type == "authentication_error" {
			t.Fatalf("gateway off: GET /v1/models = %d %s, want API 404", rec.Code, rec.Body)
		}
	}
}

// 系统账号（users.system，迁移 00022 写入）缺失时 Build 拒绝启动：系统实例 / 内部应用 / 节点都挂在它名下。
func TestBuildFailsWithoutSystemUser(t *testing.T) {
	pool := dbtest.New(t)
	if _, err := pool.Exec(context.Background(), `UPDATE users SET system = false WHERE phone = $1`, llm.SystemUserPhone); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key,
		SMSDriver: "mock", KYCDriver: "mock"}
	if _, err := app.Build(cfg, pool, discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)}); err == nil || !strings.Contains(err.Error(), "系统账号") {
		t.Fatalf("Build without system user: err = %v, want 系统账号 error", err)
	}
}
