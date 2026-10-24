package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/wallet"
)

// env 是 llm 包数据库测试的公共环境：嵌入式 PG、假时钟（dbtest.T0）、真实的 appdeploy.Catalog / catalog / wallet / identity，
// 以及库里预置的已发布内部应用 vllm-node（兼容镜像 = 新插入的「LLM Serving」）。
type env struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	clk    *clock.Fake
	box    *secretbox.Box
	fleet  *fakeFleet
	images *catalog.Service
	appCat *appdeploy.Catalog
	wallet *wallet.Service
	ids    *identity.Service
	svc    *llm.Service

	imageID      int64 // LLM Serving：vllm-node 当前版本的兼容镜像
	otherImageID int64 // 启用但不在 vllm-node 兼容列表里的镜像（种子 Ubuntu）
	appID        int64 // vllm-node
	versionID    int64 // vllm-node 最新已发布版本
	systemUserID int64 // 迁移插入的 system:llm
	poolGroupID  int64 // 迁移插入的「Token 服务池」
}

// fakeFleet 用真实 fleet.Service 满足 FleetOps；LLMPoolGroupID（Task 3 才加到 fleet）在这里用 SQL 补上。
// Task 3 合入后 fleet.Service 自带同名方法，外层方法优先，不冲突。
type fakeFleet struct {
	*fleet.Service
}

func (f *fakeFleet) LLMPoolGroupID(ctx context.Context, q db.Querier) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM machine_groups WHERE purpose = 'llm'`).Scan(&id)
	return id, err
}

var phoneSeq atomic.Int64

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	f := fleet.NewService(pool, clk)
	imgs := catalog.NewService(pool).WithClock(clk)
	e := &env{t: t, ctx: context.Background(), pool: pool, clk: clk, box: box,
		fleet: &fakeFleet{f}, images: imgs,
		appCat: appdeploy.NewCatalog(pool, clk, f, imgs, slog.New(slog.NewTextHandler(io.Discard, nil))),
		wallet: wallet.NewService(pool, clk),
		ids:    identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: &sms.Mock{}, KYC: kyc.Mock{}, Box: box}),
	}
	e.otherImageID = dbtest.ImageID(t, pool)
	e.imageID = e.insertImage("LLM Serving", "spark/llm-serving:25.09", true)
	e.appID = e.queryInt(`INSERT INTO apps (slug, name, category, icon, status, internal, created_at, updated_at)
		VALUES ($1, 'vLLM 节点（平台内部）', 'llm', 'llm', 'published', true, $2, $2) RETURNING id`, llm.NodeAppSlug, dbtest.T0)
	e.versionID = e.addNodeVersion("1.0.0", "published", e.imageID)
	e.systemUserID = e.queryInt(`SELECT id FROM users WHERE system AND phone = $1`, llm.SystemUserPhone)
	e.poolGroupID = e.queryInt(`SELECT id FROM machine_groups WHERE purpose = 'llm'`)
	e.svc = e.newService(nil)
	return e
}

// newService 组装 llm.Service；mod 可覆盖个别依赖（换 fake）。RentalOps / AppOps / Upstream 由用到它们的任务自己填。
func (e *env) newService(mod func(*llm.Deps)) *llm.Service {
	d := llm.Deps{Pool: e.pool, Clock: e.clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Box: e.box,
		Wallet: e.wallet, Users: e.ids, AppCat: e.appCat, Images: e.images, Fleet: e.fleet, SystemUserID: e.systemUserID}
	if mod != nil {
		mod(&d)
	}
	return llm.NewService(d)
}

// vllmNodeSpec 按规格 §3.2 的变量表构造 vllm-node 的 spec（已过 ValidateSpec，与导入后入库的形态一致）。
func vllmNodeSpec(t *testing.T) appdeploy.Spec {
	t.Helper()
	i64 := func(v int64) *int64 { return &v }
	s := appdeploy.Spec{
		Ports:          []appdeploy.PortSpec{{Port: llm.NodePort, Label: "OpenAI API", Path: "/docs", HealthPath: "/health"}},
		TimeoutMinutes: 120,
		MinDataFreeGiB: 80,
		Env: []appdeploy.EnvSpec{
			{Name: "MODEL_ID", Type: "string", Required: true, Pattern: `^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`},
			{Name: "MODEL_SOURCE", Type: "enum", Options: []string{"modelscope", "hf-mirror"}, Default: "modelscope"},
			{Name: "SERVED_MODEL_NAME", Type: "string", Required: true, Pattern: `^[a-z0-9][a-z0-9.-]{1,63}$`},
			{Name: "MAX_MODEL_LEN", Type: "int", Min: i64(1024), Max: i64(1048576), Default: "32768"},
			{Name: "API_KEY", Type: "secret", Required: true, Pattern: `^[A-Za-z0-9_-]{32,128}$`},
			{Name: "GPU_MEMORY_UTILIZATION", Type: "string", Pattern: `^0\.[3-9][0-9]?$`, Default: "0.85"},
			{Name: "MAX_NUM_SEQS", Type: "int", Min: i64(1), Max: i64(512), Default: "64"},
			{Name: "QUANTIZATION", Type: "enum", Options: []string{"none", "fp8", "awq", "gptq"}, Default: "none"},
			{Name: "REASONING_PARSER", Type: "enum", Options: []string{"none", "qwen3", "deepseek_r1"}, Default: "none"},
			{Name: "TOOL_CALL_PARSER", Type: "enum", Options: []string{"none", "hermes"}, Default: "hermes"},
			{Name: "TASK", Type: "enum", Options: []string{"generate", "embed"}, Default: "generate"},
			{Name: "MODEL_SIZE_GB", Type: "int", Min: i64(1), Max: i64(2048), Default: "20"},
		},
	}
	if err := appdeploy.ValidateSpec(&s); err != nil {
		t.Fatalf("vllm-node spec: %v", err)
	}
	return s
}

// addNodeVersion 给 vllm-node 插一个版本（published 时 published_at = 当前假时钟，成为最新已发布版本）。
func (e *env) addNodeVersion(version, status string, imageIDs ...int64) int64 {
	e.t.Helper()
	spec := vllmNodeSpec(e.t)
	b, err := json.Marshal(spec)
	if err != nil {
		e.t.Fatal(err)
	}
	now := e.clk.Now()
	var publishedAt any
	if status == appdeploy.StatusPublished {
		publishedAt = now
	}
	return e.queryInt(`INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, published_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'true', $6, $7, $8, $8) RETURNING id`,
		e.appID, version, status, imageIDs, b, appdeploy.SpecHash(spec, "true"), publishedAt, now)
}

// insertImage 插一个镜像（enabled 可控）。
func (e *env) insertImage(name, ref string, enabled bool) int64 {
	e.t.Helper()
	return e.queryInt(`INSERT INTO images (name, ref, enabled, sort, created_at, updated_at) VALUES ($1, $2, $3, 100, $4, $4) RETURNING id`,
		name, ref, enabled, dbtest.T0)
}

func (e *env) admin() audit.Actor { return audit.Actor{AdminID: 1, IP: "127.0.0.1"} }

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) queryInt(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// createUser 插一个用户（手机号自动递增）；verified 决定实名状态。
func (e *env) createUser(verified bool) int64 {
	e.t.Helper()
	return dbtest.CreateUser(e.t, e.pool, fmt.Sprintf("139%08d", phoneSeq.Add(1)), verified)
}

func (e *env) verifiedUser() int64   { return e.createUser(true) }
func (e *env) unverifiedUser() int64 { return e.createUser(false) }

// fund 给用户加余额（分）。
func (e *env) fund(userID, amount int64) { dbtest.Fund(e.t, e.pool, userID, amount) }

// modelInput 一个能通过校验的 chat 模型输入（draft、0 副本）。
func (e *env) modelInput(slug string) llm.ModelInput {
	return llm.ModelInput{
		Slug: slug, Name: "Qwen3 4B", Description: "测试模型", Kind: llm.KindChat, Source: "modelscope", SourceID: "Qwen/Qwen3-4B",
		ImageID: e.imageID, ContextLen: 32768, MaxOutputTokens: 8192, DefaultMaxTokens: 4096,
		InputPrice: 100, OutputPrice: 400, NodeEnv: map[string]string{}, Replicas: 0, MaxConcurrency: 32,
		Features: []string{"tools"},
	}
}

// createModel 建一个 draft 模型；mods 在 modelInput 基础上改字段。
func (e *env) createModel(slug string, mods ...func(*llm.ModelInput)) llm.Model {
	e.t.Helper()
	in := e.modelInput(slug)
	for _, m := range mods {
		m(&in)
	}
	m, err := e.svc.CreateModel(e.ctx, e.admin(), in)
	if err != nil {
		e.t.Fatalf("CreateModel %s: %v", slug, err)
	}
	return m
}

// onlineModel 建一个 replicas 个副本、已上线的模型。
func (e *env) onlineModel(slug string, replicas int, mods ...func(*llm.ModelInput)) llm.Model {
	e.t.Helper()
	m := e.createModel(slug, append([]func(*llm.ModelInput){func(in *llm.ModelInput) { in.Replicas = replicas }}, mods...)...)
	m, err := e.svc.SetModelStatus(e.ctx, e.admin(), m.ID, llm.ModelOnline)
	if err != nil {
		e.t.Fatalf("SetModelStatus online %s: %v", slug, err)
	}
	return m
}

// poolMachine 插一台空闲在线机器并放进 Token 服务池。
func (e *env) poolMachine(serial string) int64 {
	e.t.Helper()
	id := dbtest.CreateMachine(e.t, e.pool, serial)
	e.exec(`UPDATE machines SET group_id = $2 WHERE id = $1`, id, e.poolGroupID)
	return id
}

// rawNode 直接插一个节点行（不建实例 / 部署），返回节点 ID。
func (e *env) rawNode(id string, modelID, machineID int64, state string) string {
	e.t.Helper()
	e.exec(`INSERT INTO llm_nodes (id, model_id, machine_id, state, api_key_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, '\x00', $5, $5)`, id, modelID, machineID, state, e.clk.Now())
	return id
}

var rawSeq atomic.Int64

// rawKey 直接插一把 active Key（哈希随机，不能用于鉴权），返回 ID。
func (e *env) rawKey(userID int64) int64 {
	e.t.Helper()
	n := rawSeq.Add(1)
	return e.queryInt(`INSERT INTO llm_api_keys (user_id, name, key_hash, key_hint, created_at, updated_at)
		VALUES ($1, 'raw', $2, 'sk-spark-…raw', $3, $3) RETURNING id`, userID, fmt.Sprintf("rawhash-%d", n), e.clk.Now())
}

// rawUsage 直接插一条未结算用量（status ok），返回 ID。
func (e *env) rawUsage(userID, keyID, modelID, costMicro int64) int64 {
	e.t.Helper()
	n := rawSeq.Add(1)
	return e.queryInt(`INSERT INTO llm_usage (request_id, user_id, key_id, model_id, endpoint, stream, prompt_tokens, completion_tokens,
		input_price, output_price, cost_micro, status, http_status, created_at)
		VALUES ($1, $2, $3, $4, 'chat', false, 10, 10, 100, 400, $5, 'ok', 200, $6) RETURNING id`,
		fmt.Sprintf("req-raw%013d", n), userID, keyID, modelID, costMicro, e.clk.Now())
}

// auditCount 某动作在某目标上的审计条数。
func (e *env) auditCount(action, targetID string) int {
	e.t.Helper()
	return int(e.queryInt(`SELECT count(*) FROM audit_logs WHERE action = $1 AND target_id = $2`, action, targetID))
}

// lastAuditDiff 某动作在某目标上的最新一条审计 diff。
func (e *env) lastAuditDiff(action, targetID string) map[string]any {
	e.t.Helper()
	var b []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT diff FROM audit_logs WHERE action = $1 AND target_id = $2 ORDER BY id DESC LIMIT 1`,
		action, targetID).Scan(&b); err != nil {
		e.t.Fatalf("audit %s %s: %v", action, targetID, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

// errCode 取 apperr 的 code（非 apperr 返回 ""）。
func errCode(err error) string {
	if ae := apperr.As(err); ae != nil {
		return ae.Code
	}
	return ""
}
