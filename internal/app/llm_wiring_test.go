package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/app"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
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

// RunAccessLoops 启动 Token 节点表刷新：新插入的 ready 节点不经手动 Refresh 就能被选中。
func TestRunAccessLoopsRefreshesLLMRouter(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key,
		SMSDriver: "mock", KYCDriver: "mock", LLMGateway: true}
	clk := clock.NewFake(dbtest.T0)
	a, err := app.Build(cfg, pool, discardLog(), app.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	imageID, productID := dbtest.ImageID(t, pool), dbtest.ProductID(t, pool)
	var sys, modelID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE system AND phone = $1`, llm.SystemUserPhone).Scan(&sys); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO llm_models (slug, name, kind, source_id, image_id, context_len, max_output_tokens, default_max_tokens,
			input_price, output_price, max_concurrency, status, created_at, updated_at)
		VALUES ('loop-chat', 'loop-chat', $1, 'Qwen/Qwen3-4B', $2, 32768, 8192, 4096, 100, 400, 32, $3, $4, $4) RETURNING id`,
		llm.KindChat, imageID, llm.ModelOnline, dbtest.T0).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { a.RunAccessLoops(loopCtx); close(done) }()
	defer func() { cancel(); <-done }()

	mach := dbtest.CreateMachine(t, pool, "SN-LOOP-1")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, system, created_at, updated_at)
			VALUES ('ins-loop00001', $1, $2, $3, $4, 'running', 'x', true, $5, $5)`, []any{sys, mach, productID, imageID, dbtest.T0}},
		{`INSERT INTO llm_nodes (id, model_id, machine_id, instance_id, state, api_key_enc, created_at, updated_at)
			VALUES ('node-loop00001', $1, $2, 'ins-loop00001', 'ready', $3, $4, $4)`, []any{modelID, mach, box.Seal([]byte("k")), dbtest.T0}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * llm.RouterRefresh)
	for {
		n, err := a.LLM.Router().Pick(modelID, 0, nil)
		if err == nil && n.ID == "node-loop00001" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("router never picked up the node: %+v %v", n, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
