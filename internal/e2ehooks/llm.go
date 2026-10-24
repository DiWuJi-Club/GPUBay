package e2ehooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"spark-cloud/internal/app"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/llmgw/llmtest"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

// Token 商城（计划 9）的 e2e 钩子：
//   - POST /hooks/llm/upstream：在假容器 sshd 的某个端口（默认 8000 = llm.NodePort）挂一个 llmtest.FakeVLLM；
//   - POST /hooks/llm/node：在池内机器上直接造一个 ready 节点（PlaceSystem + fake Agent 完成 Create + 插节点行，不建部署）；
//   - POST /hooks/llm/tick：跑一轮 Controller.Tick + Prober.Tick（探测经 Upstream.Dial 真走 sshtest），再刷新路由表。
// 所有 e2e 实例共用同一个假容器：同一端口上的 FakeVLLM 服务全部节点。

// 默认的假节点响应：usage 100 / 50（规格 §16.2 的例子）、三个内容块。
var (
	defaultFakePrompt     = 100
	defaultFakeCompletion = 50
	defaultFakeChunks     = []string{"你好", "，", "世界"}
)

// SeedLLMNode 在 machineID（须在 Token 服务池、空闲、Agent 在线、没有实例）上直接造一个 ready 节点（/hooks/llm/node 与集成测试共用）：
// rental.PlaceSystem（池机组、模型镜像）→ completeCreate 让 Agent 完成 Create 指令 → 实例须已 running → llm.SeedReadyNode
// （随机节点密钥、无部署）→ 刷新路由表。PlaceSystem 之后任何一步失败都释放刚建的系统实例。
// 机器不可用 → apperr.ErrMachineUnavailable；模型不存在 → apperr.ErrLLMModelNotFound。
func SeedLLMNode(ctx context.Context, a *app.App, modelID, machineID int64,
	completeCreate func(ctx context.Context, instanceID string) error) (llm.Node, error) {
	m, err := a.LLM.GetModel(ctx, modelID)
	if err != nil {
		return llm.Node{}, err
	}
	poolID, err := a.Fleet.LLMPoolGroupID(ctx, a.Fleet.Pool())
	if err != nil {
		return llm.Node{}, err
	}
	inst, err := a.Rental.PlaceSystem(ctx, rental.SystemPlaceRequest{MachineID: machineID, GroupID: poolID, ImageID: m.ImageID,
		Note: "e2e node " + m.Slug})
	if err != nil {
		return llm.Node{}, err
	}
	fail := func(err error) (llm.Node, error) {
		if rerr := a.Rental.ReleaseSystem(context.WithoutCancel(ctx), inst.ID); rerr != nil && !errors.Is(rerr, apperr.ErrInvalidState) {
			err = errors.Join(err, fmt.Errorf("release system instance %s: %w", inst.ID, rerr))
		}
		return llm.Node{}, err
	}
	if err := completeCreate(ctx, inst.ID); err != nil {
		return fail(err)
	}
	got, err := a.Rental.Get(ctx, inst.ID)
	if err != nil {
		return fail(err)
	}
	if got.State != rental.StateRunning {
		return fail(fmt.Errorf("system instance %s is %s after create, want running", inst.ID, got.State))
	}
	n, err := a.LLM.SeedReadyNode(ctx, modelID, machineID, inst.ID)
	if err != nil {
		return fail(err)
	}
	if err := a.LLM.Router().Refresh(ctx); err != nil {
		return llm.Node{}, fmt.Errorf("refresh llm router: %w", err)
	}
	return n, nil
}

// llmUpstream 在假容器 sshd 的 port（缺省 8000）挂一个 FakeVLLM（替换该端口上之前的任何服务，含 /hooks/ingress-upstream 起的）：
//
//	{port?, served?: [], prompt_tokens?, completion_tokens?, chunks?: [], fail_first_n?, delay_ms?, require_key?}
//
// served 缺省 = 当前全部 online 模型的 slug（/v1/models 的返回与 POST 的 model 校验都按它）；prompt_tokens / completion_tokens
// 缺省 100 / 50；chunks 缺省 ["你好","，","世界"]；fail_first_n：前 N 个 POST 回 503；delay_ms：非流式响应前 / 每个流式块前的等待；
// require_key 缺省 ""（接受任意 Bearer：节点密钥是钩子 / Controller 随机生成的，FakeVLLM 不校验它）。
// 非 /v1/* 的路径（如 Runner 的 /health 之外的应用页面）在配置了 HealthAddr 时转给 fake 应用服务，
// 让 /hooks/apps/seed 登记在同一端口（8000 = vllm）的 ingress / 健康检查用例不受影响；/health 一律 200。
func (s *Server) llmUpstream(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Port             int       `json:"port"`
		Served           *[]string `json:"served"`
		PromptTokens     *int      `json:"prompt_tokens"`
		CompletionTokens *int      `json:"completion_tokens"`
		Chunks           *[]string `json:"chunks"`
		FailFirstN       int32     `json:"fail_first_n"`
		DelayMS          int       `json:"delay_ms"`
		RequireKey       string    `json:"require_key"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Port == 0 {
		req.Port = llm.NodePort
	}
	switch {
	case req.Port < 0 || req.Port > 65535:
		return nil, badRequest("port 不正确")
	case req.FailFirstN < 0:
		return nil, badRequest("fail_first_n 不能为负数")
	case req.DelayMS < 0 || req.DelayMS > 600_000:
		return nil, badRequest("delay_ms 需在 0～600000 之间")
	case req.PromptTokens != nil && *req.PromptTokens < 0, req.CompletionTokens != nil && *req.CompletionTokens < 0:
		return nil, badRequest("token 数不能为负数")
	}
	if s.Upstream == nil {
		return nil, errors.New("fake upstream not configured")
	}
	fake := &llmtest.FakeVLLM{PromptTokens: defaultFakePrompt, CompletionTokens: defaultFakeCompletion, Chunks: defaultFakeChunks,
		FailFirstN: req.FailFirstN, Delay: time.Duration(req.DelayMS) * time.Millisecond, RequireKey: req.RequireKey}
	if req.Served != nil {
		fake.Served = *req.Served
	} else {
		models, err := s.App.LLM.ListOnlineModels(ctx)
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			fake.Served = append(fake.Served, m.Slug)
		}
	}
	if fake.Served == nil {
		fake.Served = []string{}
	}
	if req.PromptTokens != nil {
		fake.PromptTokens = *req.PromptTokens
	}
	if req.CompletionTokens != nil {
		fake.CompletionTokens = *req.CompletionTokens
	}
	if req.Chunks != nil {
		fake.Chunks = *req.Chunks
	}
	var h http.Handler = fake
	if s.HealthAddr != "" {
		appSrv := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: s.HealthAddr})
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" || strings.HasPrefix(r.URL.Path, "/v1/") {
				fake.ServeHTTP(w, r)
				return
			}
			appSrv.ServeHTTP(w, r)
		})
	}
	// 与 ingressUpstream 同一张表、同一把锁：同端口的替换不会交错
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev := s.upstreamStops[req.Port]; prev != nil {
		prev()
	}
	if s.upstreamStops == nil {
		s.upstreamStops = map[int]func(){}
	}
	s.upstreamStops[req.Port] = s.Upstream.ServeHTTP(req.Port, h)
	return map[string]any{"ok": true, "port": req.Port, "served": fake.Served}, nil
}

// llmNode 在池内机器上直接造一个 ready 节点（规格 §16.3；计划 9 裁定：不建部署）：{model_id, machine_id} → {node_id, instance_id}。
// fake Agent 经 /hooks/agent 同一逻辑完成 Create（机器没有 fake Agent 时先接入）。
func (s *Server) llmNode(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		ModelID   int64 `json:"model_id"`
		MachineID int64 `json:"machine_id"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.ModelID <= 0 || req.MachineID <= 0 {
		return nil, badRequest("model_id 与 machine_id 必填")
	}
	n, err := SeedLLMNode(ctx, s.App, req.ModelID, req.MachineID, func(ctx context.Context, _ string) error {
		_, err := s.agent(ctx, json.RawMessage(fmt.Sprintf(`{"machine_id":%d}`, req.MachineID)))
		return err
	})
	switch {
	case errors.Is(err, apperr.ErrMachineUnavailable):
		return nil, badRequest("机器不能建节点：须在 Token 服务池内、空闲、Agent 在线，且没有实例、租约或节点")
	case errors.Is(err, apperr.ErrLLMModelNotFound):
		return nil, badRequest("模型不存在")
	case err != nil:
		return nil, err
	}
	return map[string]any{"node_id": n.ID, "instance_id": *n.InstanceID}, nil
}

// llmTick 跑一轮节点 Controller（与 Controller.RunOnce 相同：先 Tick 再探测，但不拿咨询锁——e2e-server 不启动 Controller），
// 然后刷新路由表（不必等 2 秒的定时刷新）。{probe?: bool（缺省 true）}。
// 返回 TickReport 各计数 + error（单节点的失败合并成一行文本；"" = 全部成功）：
//
//	{"provisioned":0,"drained":0,"failed":0,"removed":0,"orphans":0,"error":""}
//
// 注意：/hooks/llm/node 造的节点没有部署，每轮探测都对它计一次失败——连续 3 轮（probe 为 true）后转 unhealthy，再下一轮
// Controller 把它置 failed 并释放实例。只想推进排空 / 移除时传 {"probe": false}。
func (s *Server) llmTick(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Probe *bool `json:"probe"`
	}](body)
	if err != nil {
		return nil, err
	}
	s.llmMu.Lock()
	defer s.llmMu.Unlock()
	rep, terr := s.App.LLMController.Tick(ctx, s.Clock.Now())
	var perr error
	if req.Probe == nil || *req.Probe {
		perr = s.App.LLMController.Prober().Tick(ctx, s.Clock.Now())
	}
	if err := s.App.LLM.Router().Refresh(ctx); err != nil {
		return nil, fmt.Errorf("refresh llm router: %w", err)
	}
	msg := ""
	if err := errors.Join(terr, perr); err != nil {
		msg = strings.ReplaceAll(err.Error(), "\n", "; ")
	}
	return struct {
		llm.TickReport
		Error string `json:"error"`
	}{rep, msg}, nil
}
