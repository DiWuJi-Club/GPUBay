# Spark Cloud 计划 9：Token 商城（按量计费的大模型 API）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用户在控制台创建 API Key，用 OpenAI 兼容接口（`https://<domain>/v1/chat/completions` 等）调用平台托管的大模型，按模型的输入 / 输出单价（分 / 百万 token）从钱包余额预付扣费；运营在后台维护模型与价格、把机器加入「Token 服务池」、按期望副本数自动在池内机器上拉起 vLLM 节点，节点健康检查与故障转移自动完成；最后在 LAN 上的 DGX Spark（192.168.1.221）完成单机验收。

**Architecture:** 不改 Agent 协议、不引入 New-API / LiteLLM。新包 `internal/llm`（模型目录、Key、账户、用量、结算、节点 Controller / Prober、路由表、限流）与 `internal/llmgw`（OpenAI 兼容网关，挂在用户 API 进程的 `/v1`）。节点 = `rental.PlaceSystem` 建的**系统实例**（系统账号持有、不计费不到期）+ 计划 7 `appdeploy` 部署的内部应用 `vllm-node`；网关经 `upstream.Pool.Dial` 到容器内 `127.0.0.1:8000` 转发。机器池 = 迁移建好的 `purpose='llm'`、`visible=false` 机组（计划 6 的分配天然不选它）。计费：每请求记 `llm_usage`（微分整数），清扫附加任务 `llm.Settle` 按用户汇总成一条 `wallet_ledger(llm_pay)`，`wallet.ApplyUpTo` 扣不足记欠费，欠费期间网关拒绝。

**Tech Stack:** Go 1.27（`~/.local/go/bin`）、pgx v5、chi v5、embedded-postgres（`dbtest.Main`）、`golang.org/x/crypto/ssh`（已有）；前端 Vue 3 + Element Plus + Vitest + Playwright 1.64。**不新增任何 Go 模块依赖、不新增 npm 依赖。**

**Spec:** `docs/superpowers/specs/2026-10-24-plan9-token-mall-design.md`（下称「规格」）。上位：计划 6 规格 §3 / §4.2、计划 7 规格 §5 / §5.4、计划 8 规格 §4.3 / §5。

**分支：** `feat/plan9-token-mall`（从 `main` 切出；**实施前先合入含计划 7、8 的 `main`**，核对 `migrations/00020_apps.sql`、`00021_referral.sql` 存在、`internal/appdeploy`、`internal/referral`、`wallet.ApplyUpTo` 可用——没有就停下来汇报，不要自己补）。

**本计划不包含（规格 §17）：** Token 套餐 / 赠送 / 阶梯价；Token 消费计入返利；独立 `api.` 子域；跨机并行 / 一机多模型 / 自动伸缩；洗机保留模型缓存；多副本共享限流；内容留存与审核；用量短信；Prometheus；`/v1/responses` 等其它端点；批量入池。

## 执行须知（写给每个子代理）

- 任务**按编号顺序串行执行，一个任务一个子代理**。子代理只看得到自己任务的文字：每个任务的 Interfaces 必须完整（消费什么、产出什么、精确签名），不写「同上」。
- 规格里的真机事实（§0）与裁定（§1.3）是约束，不是建议；与代码冲突时以代码为准并在任务汇报里写明。
- 测试机 192.168.1.221（`imac` 用户）**需要 root 的命令由协调人在运行时执行**，密码不进任何文件、脚本、提交（做法同计划 6 / 7 的执行须知：远端 `s()` 函数经 stdin 送给 `sudo -S -p ""`）。`git diff --cached | grep -i sudo_pw` 应为空。
- 开发 Mac 的 DNS 对 `*.sslip.io` 返回假地址：curl 加 `--resolve`，Playwright / Chrome 用 `--host-resolver-rules`。
- 测试机访问不了 Docker Hub；镜像 `spark/llm-serving:25.09` 已在机器上，以本地 tag 登记为 `LLM Serving`。
- 每个任务结束前必须实际运行并通过：`go vet ./... && go test ./...`；涉及前端的任务再加 `cd web && npm test && npm run build`；页面任务再加该任务列出的 Playwright 用例。证据是命令输出。
- 提交信息结尾空一行加 `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`。

## Global Constraints

- 仓库根目录 `/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud-p9`；所有命令前置 `export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH`。Go 模块名 `spark-cloud`。开发 Mac 没有 Docker；Go 测试用 embedded-postgres。
- **金额单位**：数据库与 API 里价格是「分 / 百万 token」（`input_price / output_price BIGINT`），单次费用 `cost_micro` 是**微分**（`prompt_tokens × input_price + completion_tokens × output_price`，embeddings 只算输入），结算把 `Σ cost_micro + carry_micro` 以 `10^6` 整除得「分」，余数回写 `carry_micro`。前端显示价格用「元 / 百万 token」两位小数（`price / 100`），费用用元 6 位小数或「< ¥0.01」。
- **系统账号**：迁移插入 `users(phone='system:llm', system=true, kyc_status='verified')`；`rental.Deps.SystemUserID`、`llm.Deps.SystemUserID` 由 `app.go` 用 `SELECT id FROM users WHERE system AND phone = 'system:llm'` 读出（找不到 → `Build` 报错）。
- **系统实例**（规格 §4）：`instances.system=true`、`expires_at NULL`、`pending_seconds 0`、`password_auth=false`、lease `active [now, now + SystemLeaseHorizon(10y))`、无订单 / 流水；`startLease` 对 `System` 不写 `expires_at`；`occupiedUntil` 对 `System` 返回 `lower + SystemLeaseHorizon`；`SuspendMachines / ResumeMachine` 对 `System` 只写 / 清 `suspended_at`；`RefundRelease / AdminExtend / AdminMigrate / cancelInstance / AdminReassign` 对 `System` 返回 `apperr.ErrInvalidState`；`ReleaseSystem` 允许 `creating / running / stopped`。
- **池机组**（规格 §3.3）：`machine_groups.purpose='llm'` 唯一一行（迁移写入「Token 服务池」，`visible=false`）；`fleet.UpdateGroup` 对它拒绝 `visible=true`、`purpose` 变更，`DeleteGroupIn` 拒绝；可建节点机器 = `group_id = 池 AND state='idle' AND agent_online AND NOT EXISTS 活动实例（creating/running/stopped/wiping）AND NOT EXISTS 相交的 reserved/active lease AND NOT EXISTS 非终态 llm_nodes`。
- **节点状态**：`provisioning / ready / unhealthy / draining / removing / removed / failed`；终态 `removed / failed`；唯一索引 `llm_nodes_machine_live_uniq (machine_id) WHERE state NOT IN ('removed','failed')`。
- **网关错误体**：`{"error":{"message","type","code"}}`；`type ∈ invalid_request_error / authentication_error / permission_error / insufficient_quota / rate_limit_error / server_error`；状态码与 code 见规格 §6.1。响应头 `X-Request-Id: req-<16 位 [a-z0-9]>`。
- **网关路径**：`/v1/models`（GET）、`/v1/chat/completions`、`/v1/completions`、`/v1/embeddings`（POST）、`OPTIONS /v1/*`；其它 404。`api.Server.requireJSONForWrites` 与 `SameSiteGuard` 对 `strings.HasPrefix(path, "/v1/")` 免检；`r.Mount("/v1", s.LLMGateway)` 在 `/api/v1` Route 之外。
- **限额默认**：RPM 60 / TPM 200,000 / 并发 8（按用户；`llm_accounts.*_limit` 非 NULL 时覆盖）；节点并发 `llm_models.max_concurrency`（默认 32）；请求体 2 MiB；上游非流式 body 8 MiB；总超时 10 分钟；首字节 120 秒；401 按 IP 60 次 / 分钟。
- **预算预检**（T2）：`available = balance − ceil(unsettled_micro/1e6) − inflight_estimate[user] − debt`；`est = ceil((len(body)/3 × input_price + max_out × output_price)/1e6)`，`max_out = min(请求 max_tokens | max_completion_tokens | 模型 default_max_tokens, 模型 max_output_tokens)`；要求 `available ≥ max(est, 1)`；`debt > 0` → 402。
- **结算**（规格 §7）：候选 `unsettled_micro > 0 OR debt > 0`；触发 `unsettled_micro ≥ 100×10^6` 或最早未结算 `created_at ≤ now − 10m`，或 `debt > 0 AND balance > 0`；`UPDATE llm_usage … WHERE settlement_id IS NULL AND id <= $max_id`；`ApplyUpTo(Entry{Amount: −(usage_amount + debt_amount), Kind: KindLLMPay, RefType: "llm_settlement", RefID: sid})`；`debt' = due − applied`；`llm_usage_daily` UPSERT；清理 `created_at < now − 90d AND settlement_id IS NOT NULL` 每轮 5000 行。
- **加锁顺序**（规格 §14，写进 `internal/llm/llm.go` 头注释）：记账 `llm_accounts(user) → llm_usage → llm_api_keys`；结算 `llm_accounts(user) → llm_usage → llm_settlements → llm_usage_daily → wallet_accounts(user)`；建节点 `machines(NO KEY UPDATE SKIP LOCKED) → instances / machine_leases → llm_nodes`；Controller / Prober 回写只锁 `llm_nodes` 行，对 appdeploy / rental 的调用在事务外。
- **告警种类**（文案稳定，不含数值）：`llm_pool_short`、`llm_node_unhealthy`、`llm_node_failed`、`llm_usage_record_failed`。**审计动作**：`llm.model.create / update / status / delete`、`llm.node.drain / rebuild / remove`、`llm.account.update`、`llm.key.create / update / revoke`（user actor）、`llm.key.admin_disable`。
- 时间一律 `clock.Clock`；月份 / 自然日按 `tz`（Asia/Shanghai）；错误用 `apperr`（控制台 / 后台）或 OpenAI 格式（`/v1`）；生产禁止 mock。
- 端到端规范沿用计划 5：用例标题以覆盖表 ID 开头、`data-testid` 命名 `页面-元素`、禁止 `waitForTimeout`、`workers: 1`；`web/e2e/coverage.json` 与覆盖表一致；现有 Playwright 用例必须保持全绿。

## Review Focus

1. **并发打穿余额后账能对上**：余额 ¥0.02、50 个并发请求，结算后 `debt = Σ usage_amount − Σ charged`、`wallet_accounts.balance ≥ 0`、每条 `llm_usage` 恰属于一次结算、`CheckInvariants` 七条全部成立 → Task 15 `TestLLMOverdraftSettlesIntoDebt`。
2. **流式请求的字节透传与计费**：注入 `stream_options.include_usage` 后客户端收到的 SSE 事件与 FakeVLLM 发出的逐字节一致（除注入）；末尾 usage 被解析入账；客户端中途断开按块数估算且 `estimated=true`；上游首字节后断开不收费 → Task 10 `TestGatewayStreamPassthroughAndUsage`、`TestGatewayClientAbortEstimates`、`TestGatewayUpstreamDropAfterFirstByteNotCharged`。
3. **系统实例不走任何计费 / 到期路径**：`PlaceSystem` 后推进 100 天 + 全部清扫任务 → 实例仍 running、无订单、无流水、无短信、无告警；`ReleaseSystem` 后机器 idle → Task 2 `TestSystemInstanceNeverExpiresOrBills`。
4. **池内机器永不被出租**：池里一台空闲在线机器，用户即时 / 预订 / 后台换机 / 改派都拿不到它；移出池后立刻能租 → Task 2 `TestPoolMachinesInvisibleToRental`。
5. **结算边界**：结算事务进行中并发插入的用量行留到下一次（`max_id`）；同一 `sid` 重放 `ApplyUpTo` 撞 `ErrDuplicate` 后事务整体回滚、`unsettled_micro` 不变 → Task 8 `TestSettleMaxIDBoundary`、`TestSettleReplayIsNoop`。

## 已做出的裁定（规格未写明或与代码核对后确定的细节）

- **`llm` 与 `appdeploy` / `rental` 的依赖方向**：`llm` 依赖 `rental`、`appdeploy`、`fleet`、`wallet`（通过小接口 `RentalOps / AppOps / AppCatalog / FleetOps / WalletOps / UserLookup`，由 `app.go` 用真实 Service 满足；测试用 fake）。`rental` / `appdeploy` **不**依赖 `llm`：池机组移出前的节点检查放在 `adminapi.groups.go` 的 handler（调 `s.LLM.MachineHasLiveNode`）。
- **`rental.PlaceSystem` 的候选判定**在 rental 内（它知道 lease 与活动实例），接受 `GroupID` 参数并要求机器 `group_id` 等于它；`llm.Controller` 传池机组 ID。
- **节点密钥**：`randx` 生成 32 字节 → base64url 无填充（43 字符，满足 spec `^[A-Za-z0-9_-]{32,128}$`）；`secretbox` 存 `api_key_enc`；Controller 建部署时作为 `API_KEY` 传入（appdeploy 自己再加密一份——两处密文是同一明文，接受）。
- **网关与 Controller 同进程**：都在用户 API 进程（`cmd/platform`）里；`Router` 的在途计数是进程内 `sync.Map`，Controller 排空读它；e2e-server 关闭 Controller（`SPARK_LLM_CONTROLLER=off` 等价的 Options），用 `/hooks/llm/tick` 驱动。
- **`llm.Router` 的节点表刷新**：`Router.Run(ctx, 2s)` 由 `App.RunAccessLoops` 启动（与 ingress 路由表同处）；e2e / 单测直接调 `Refresh(ctx)`。
- **`http.Transport` 每节点一个**（key 为 node ID），`DialContext` 忽略 addr、调用 `Upstream.Dial(ctx, target, 8000)`；节点 `removed / failed` 时 `CloseIdleConnections` 并从 map 删除。
- **流式解析**：按 `\n\n` 切事件、取 `data:` 行；`[DONE]` 结束；JSON 解析失败的块照常透传、不计数。`choices[0].delta.content != ""` 或 `delta.reasoning_content != ""` 计一块；`usage != null` 的块记录 usage。
- **非流式 4xx 透传**：上游 400 / 404 / 422 原样返回（body 与状态码），用量行 `status='ok'`、`cost_micro=0`、`http_status=上游码`——用户请求错误既不收费也不是平台错误。
- **`GET /v1/models` 的 `created`**：模型 `created_at` 的 Unix 秒。
- **`/me` 的 `llm_month_cost`**：`llm_usage_daily` 本月 `Σ cost_micro / 1e6`（分，向下取整）+ 未结算 `unsettled_micro / 1e6`；0 时省略。
- **CSV 费用列**：元，6 位小数（`cost_micro / 1e8`，微分 → 元）。
- **`llm_usage.client_ip`**：`httpx.ClientIP(r)`。
- **FakeVLLM**（`internal/llmgw/llmtest`）：`http.Handler`；`GET /v1/models` 返回配置的 served 名单；`POST /v1/chat/completions`：非流式返回 `{choices:[{message:{content}}], usage:{prompt_tokens, completion_tokens, total_tokens}}`，流式按 `Chunks` 配置逐块发 `data:` 并在 `include_usage` 时发 usage 块；可配置 `Delay`、`FailFirstN(503)`、`DropAfterChunks`、`RequireKey`；记录收到的请求（body、Authorization）供断言。
- **控制台 `/tokens` 未登录**：模型 Tab 可见，其它 Tab 显示登录引导（不强制跳转，与 `/market` 一致）。
- **后台 `/llm` 路由 `ALL_ROLES`**，Tab 内按 `hasRole` 隐藏操作；接口层仍按规格 §10.2 的角色 403。
- **e2e 钩子 `/hooks/llm/node`**：直接插 `llm_nodes(ready)` + 用 `PlaceSystem` 建系统实例并让 fake Agent 完成 Create（复用 `/hooks/agent` 逻辑），不建部署；`/hooks/llm/upstream {port}` 在 sshtest 的该端口挂 `FakeVLLM`（默认 served = 全部 online 模型 slug）。

## 文件结构

```
spark-cloud/
├── migrations/00022_llm.sql                                            # Task 1
├── deploy/apps/vllm-node/{app.json,spec.json,app.sh}                   # Task 4
├── internal/
│   ├── platform/apperr/apperr.go                                       # Task 1
│   ├── wallet/wallet.go、admin.go                                      # Task 1
│   ├── llm/llm.go（Deps、Service、锁序注释）、consts.go、types.go、invariants.go   # Task 1
│   │       models.go、keys.go、authorize.go、accounts.go、usage.go、settle.go、limiter.go、router.go、controller.go、prober.go、admin.go、views.go、hooks.go  # Task 5～11
│   ├── llmgw/server.go、auth.go、proxy.go、stream.go、errors.go、llmtest/fake_vllm.go   # Task 10
│   ├── rental/system.go、service.go、results.go、leases.go、sweeper.go、admin_ops.go、migrate.go、cancel.go、reassign.go、alloc.go   # Task 2
│   ├── fleet/groups.go                                                 # Task 3
│   ├── appdeploy/catalog.go、service.go、importer.go、types.go         # Task 3
│   ├── identity/admin.go（Ban 拒绝系统用户、列表 include_system）        # Task 13
│   ├── api/llm.go、server.go                                           # Task 12
│   ├── adminapi/llm.go、server.go、groups.go、views.go、dashboard.go、users.go、permissions_test.go   # Task 13
│   ├── app/app.go、platform/config/config.go、cmd/platform/main.go     # Task 14
│   ├── app/llm_e2e_test.go、e2ehooks/hooks.go、e2ehooks/llm.go         # Task 15
├── web/shared/src/types/llm.ts、labels.ts、types/index.ts              # Task 1、17
├── web/console/src/{router.ts,layouts/ConsoleLayout.vue,logic/tokens.ts,views/TokensView.vue,components/tokens/{ModelsTab,KeysTab,UsageTab,LogsTab,QuickStartTab,CreateKeyDialog}.vue,views/{MarketView,WalletView,HelpView,AccountView}.vue}   # Task 17～19
├── web/admin/src/{menu.ts,router.ts,views/LlmView.vue,components/llm/{ModelsTab,ModelDrawer,NodesTab,NodeDrawer,AccountsTab,UsageTab}.vue,views/{UserDetailView,InstancesView,InstanceDetailView,MachineDetailView,DashboardView,LedgerView,CatalogView}.vue}   # Task 20～22
├── web/e2e/{helpers/llm.ts,console/tokens.spec.ts,admin/llm.spec.ts,admin/instances.spec.ts,admin/users.spec.ts,coverage.json}   # Task 23
├── README.md                                                           # Task 14、24
└── docs/superpowers/{acceptance-plan9.md,followups-plan9.md}           # Task 16、24
```

## 任务依赖

Task 1（迁移 + 骨架）→ 2（系统实例）→ 3（池机组 + internal 应用）→ 4（vllm-node 应用）→ 5（模型）→ 6（Key / Authorize）→ 7（账户 / 用量 / 记账）→ 8（结算）→ 9（Limiter / Router）→ 10（网关 + FakeVLLM）→ 11（Controller / Prober）→ 12（控制台 API）→ 13（后台 API）→ 14（装配 / 配置 / README）→ 15（集成测试 + 钩子）→ **16 真机检查点 A** → 17（控制台基础 + 模型 / 快速开始）→ 18（Keys Tab）→ 19（用量 / 日志 / 结算 + 其它页）→ 20（后台总览 + 模型）→ 21（后台节点 / 池）→ 22（后台账户 / 用量 + 其它页）→ 23（Playwright）→ **24 真机验收 B + 收尾**。按编号顺序执行，共 24 个任务。

---
### Task 1: 迁移 00022、`wallet.KindLLMPay`、错误码、`llm` 包骨架、前端类型与文案

**Files:**
- Create: `migrations/00022_llm.sql`、`internal/llm/llm.go`、`internal/llm/consts.go`、`internal/llm/types.go`、`internal/llm/invariants.go`、`internal/platform/db/migrate_llm_test.go`、`web/shared/src/types/llm.ts`
- Modify: `internal/platform/apperr/apperr.go`、`internal/wallet/wallet.go`（`KindLLMPay`）、`internal/wallet/admin.go`（`ledgerKinds`）、`web/shared/src/types/index.ts`、`web/shared/src/labels.ts`
- Test: `internal/platform/db/migrate_llm_test.go`（新建，结构仿 `migrate_booking_test.go` / 计划 7 的 `migrate_apps_test.go`）、`internal/wallet/admin_test.go`（追加 `llm_pay` 通过 `LedgerFilter`）、`web/shared/src/__tests__/labels.spec.ts`（追加）

**Interfaces:**
- Produces：
  - 规格 §2 全部表、列、索引、种子行（系统账号、池机组）、`wallet_ledger_kind_check` 七种；Down 的两条 `RAISE EXCEPTION` 与按序删除。
  - `wallet.KindLLMPay Kind = "llm_pay"`；`ledgerKinds` 加入。
  - `apperr`：`ErrLLMModelNotFound`、`ErrLLMModelSlugExists`、`ErrLLMModelInUse`、`ErrLLMKeyNotFound`、`ErrLLMKeyLimit`、`ErrLLMNodeNotFound`、`ErrLLMNodeInvalidState`、`ErrLLMPoolMachineBusy`；函数 `LLMModelInvalid(field, reason string) *Error`（code `llm_model_invalid`）、`LLMKeyInvalid(field, reason string) *Error`、`LLMAccountInvalid(field, reason string) *Error`。文案见规格 §10.1。
  - `llm/consts.go`：规格 §13 全部常量（`DefaultRPM = 60`、`DefaultTPM = 200_000`、`DefaultConcurrency = 8`、`DefaultNodeConcurrency = 32`、`MaxBodyBytes = 2 << 20`、`MaxUpstreamBodyBytes = 8 << 20`、`TotalTimeout = 10 * time.Minute`、`TTFTTimeout = 120 * time.Second`、`KeyCacheTTL = 30 * time.Second`、`BalanceCacheTTL = 2 * time.Second`、`AuthFailLimitPerIP = 60`、`MaxKeysPerUser = 20`、`SettleMinMicro = 100_000_000`、`SettleMaxAge = 10 * time.Minute`、`UsageRetention = 90 * 24 * time.Hour`、`PurgeBatch = 5000`、`ControllerInterval = 10 * time.Second`、`ProbeInterval = 10 * time.Second`、`ProbeTimeout = 5 * time.Second`、`ProbeFailThreshold = 3`、`UnhealthyRebuildAfter = 10 * time.Minute`、`MaxRebuilds = 3`、`ProvisionTimeout = 150 * time.Minute`、`DrainTimeout = 60 * time.Second`、`MaxProvisionPerRound = 4`、`SystemLeaseHorizon = 10 * 365 * 24 * time.Hour`、`RouterRefresh = 2 * time.Second`、`NodePort = 8000`、`NodeAppSlug = "vllm-node"`、`SystemUserPhone = "system:llm"`、`MicroPerFen = 1_000_000`）；状态常量 `NodeProvisioning … NodeFailed`、`NodeTerminalStates`；`ModelDraft / ModelOnline / ModelOffline`；`KeyActive / KeyDisabled / KeyRevoked`；`UsageOK / UsageClientAbort / UsageUpstreamError / UsageUpstreamTimeout / UsageNoNode`；`EndpointChat / EndpointCompletions / EndpointEmbeddings`。
  - `llm/types.go`：`Model`、`Node`、`NodeEvent`、`Account`、`Key`、`Usage`、`Settlement`、`DailyRow` 结构体（字段 = 表列，`json` 标签 snake_case）。
  - `llm/llm.go`：`type Deps struct{ Pool *pgxpool.Pool; Clock clock.Clock; Log *slog.Logger; Box *secretbox.Box; Wallet WalletOps; Users UserLookup; Rental RentalOps; Apps AppOps; AppCat AppCatalog; Fleet FleetOps; Upstream *upstream.Pool; SystemUserID int64 }`，接口定义：
    ```go
    type WalletOps interface {
        Balance(ctx context.Context, userID int64) (int64, error)
        ApplyUpTo(ctx context.Context, tx pgx.Tx, e wallet.Entry) (applied, after int64, err error)
    }
    type UserLookup interface {
        CanRent(ctx context.Context, userID int64) error
        UserState(ctx context.Context, q db.Querier, id int64) (referral.UserState, error) // 计划 8 已有：Status / KYCStatus / Phone
    }
    type RentalOps interface {
        PlaceSystem(ctx context.Context, req rental.SystemPlaceRequest) (rental.Instance, error)
        ReleaseSystem(ctx context.Context, instanceID string) error
        Get(ctx context.Context, id string) (rental.Instance, error)
        UpstreamTarget(ctx context.Context, instanceID string) (upstream.Target, error) // Task 2 新增：lan_ip + 主机公钥
    }
    type AppOps interface {
        Create(ctx context.Context, req appdeploy.CreateRequest) (appdeploy.Deployment, error)
        Redeploy(ctx context.Context, req appdeploy.RedeployRequest) (appdeploy.Deployment, error)
        Get(ctx context.Context, depID string) (appdeploy.Deployment, error)
    }
    type AppCatalog interface {
        AppBySlug(ctx context.Context, slug string) (appdeploy.App, error)
        LatestPublished(ctx context.Context, q db.Querier, appID int64) (appdeploy.Version, error)
        GetVersion(ctx context.Context, id int64) (appdeploy.Version, error)
    }
    type FleetOps interface {
        LLMPoolGroupID(ctx context.Context, q db.Querier) (int64, error)
        Get(ctx context.Context, q db.Querier, id int64) (fleet.Machine, error)
        RaiseAlert(ctx context.Context, q db.Querier, a fleet.Alert) error
        RaiseAlertOnce(ctx context.Context, q db.Querier, a fleet.Alert) error
    }
    type Service struct{ d Deps; /* Task 6/9 加缓存、limiter、router */ }
    func NewService(d Deps) *Service
    ```
    头注释写规格 §14 的加锁顺序表。`rental.SystemPlaceRequest`、`UpstreamTarget` 在 Task 2 才有——本任务先在 `llm.go` 用注释占位、接口字段以 Task 2 的签名为准（Task 2 完成前 `llm` 包不编译 `RentalOps`：本任务把 `RentalOps` 接口放进 `llm.go` 但**先不引用 `rental` 包**，用 `type RentalOps interface{}` 占位并在 Task 2 替换；避免循环等待）。
  - `llm/invariants.go`：`func CheckInvariants(ctx context.Context, pool *pgxpool.Pool) error`——规格 §14 七条，每条一个 SQL，违背时返回带编号与明细的 error；本任务实现全部七条（表已就位）。
  - 前端 `types/llm.ts`：`LLMModelPublic`、`LLMAccount`、`LLMKey`、`LLMUsageRow`、`LLMDailyRow`、`LLMSettlement`、后台 `LLMModelAdmin`、`LLMNode`、`LLMNodeEvent`、`LLMPoolMachine`、`LLMAccountAdmin`、`LLMSummary`（字段与规格 §10 一致）；`labels.ts`：`ledgerKindLabel.llm_pay = 'Token 消费'`、`llmModelStatusLabel`（草稿 / 已上线 / 已下线）、`llmNodeStateLabel / llmNodeStateType`、`llmUsageStatusLabel`（成功 / 客户端中断 / 上游错误 / 上游超时 / 无可用节点）、`llmKeyStatusLabel`、`alertKindLabel` 加四种、`auditActions` 加 12 个。

- [ ] **Step 1: 写失败的迁移往返测试**

`TestMigration00022RoundTrip`：`UpTo(21)` → 造用户 / 机器 / 实例 / 一条 `order_pay` 流水 → `UpByOne` → 断言：系统账号存在且 `system`；池机组存在 `purpose='llm' AND NOT visible`；第二个 `purpose='llm'` 机组插入失败；`llm_nodes` 同机器两条非终态插入失败、`removed` 后可再插；`wallet_ledger` 可插 `llm_pay`；`Down` 在有 `llm_pay` 流水时失败（消息含「Token 消费流水」），删掉流水后在有 `ready` 节点时失败，清掉节点后成功；Down 后 `users.system` 列不存在、`wallet_ledger_kind_check` 拒绝 `llm_pay`、旧流水完好；再 `UpByOne` 成功。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/platform/db/ -run TestMigration00022 2>&1 | tail -3`
Expected: 失败（迁移文件不存在）。

- [ ] **Step 3: 写迁移、常量、类型、错误码、不变量、前端类型文案**

`00022_llm.sql` 按规格 §2 原文；文件头注释写 Down 的两个拒绝条件。`CheckInvariants` 的 7 条 SQL 用 `EXCEPT` / 聚合比较，返回 `fmt.Errorf("invariant %d: %s", n, detail)`。

- [ ] **Step 4: 运行测试并提交**

Run: `go vet ./... && go test ./internal/platform/db/ ./internal/wallet/ ./internal/llm/ 2>&1 | tail -5 && cd web && npm test 2>&1 | tail -3`
Expected: 全部通过。

```bash
git add migrations internal/llm internal/platform/apperr internal/wallet web/shared
git commit -m "feat(llm): migration 00022 (models, nodes, accounts, keys, usage, settlements, system user, llm pool group), llm_pay ledger kind, llm package skeleton and invariants

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 2: `rental` 系统实例：`PlaceSystem / ReleaseSystem`、`Instance.System`、生命周期分支、后台操作拒绝

**Files:**
- Create: `internal/rental/system.go`、`internal/rental/system_test.go`
- Modify: `internal/rental/service.go`（`Deps.SystemUserID`、`Instance.System`、头注释加 PlaceSystem / ReleaseSystem 锁序）、`internal/rental/queries.go`（`instanceCols` 加 `system`）、`internal/rental/results.go`（`startLease` 的 System 分支）、`internal/rental/leases.go`（`occupiedUntil`）、`internal/rental/sweeper.go`（`SuspendMachines / ResumeMachine`）、`internal/rental/admin_ops.go`、`internal/rental/migrate.go`、`internal/rental/cancel.go`、`internal/rental/reassign.go`、`internal/rental/refund.go`（`RefundRelease`）、`internal/rental/access.go`（`UpstreamTarget`）
- Test: `internal/rental/system_test.go`（新建）、`internal/rental/alloc_test.go`（追加池机器不可选）

**Interfaces:**
- Consumes: Task 1 的迁移列 `instances.system`、`machine_groups.purpose`；现有 `pickMachines`、`refreshLease`、`beginWipe`、`Fleet.MarkAllocated / Release`、`HostKeyGen`、`genPassword`。
- Produces：
  ```go
  type SystemPlaceRequest struct {
      MachineID int64
      GroupID   int64  // 机器必须属于该机组（池）
      ImageID   int64
      Note      string // 事件文案，如 "llm node node-xxxx qwen3-4b"
  }
  // PlaceSystem：在一台池内空闲机器上开通系统实例（不计费、不到期）。锁序 machines(NO KEY UPDATE) → instances / machine_leases → agent_commands。
  func (s *Service) PlaceSystem(ctx context.Context, req SystemPlaceRequest) (Instance, error)
  // ReleaseSystem：释放系统实例（creating / running / stopped 均可）：取消待执行指令、running 先 Stop 再 wipe（与 Release 相同链路），非 System 返回 ErrInvalidState。
  func (s *Service) ReleaseSystem(ctx context.Context, instanceID string) error
  // UpstreamTarget：供网关 / 探测拨号（lan_ip、主机公钥）；实例不存在 → ErrNotFound。
  func (s *Service) UpstreamTarget(ctx context.Context, instanceID string) (upstream.Target, error)
  const SystemLeaseHorizon = 10 * 365 * 24 * time.Hour
  ```
  `Instance.System bool`；`Deps.SystemUserID int64`（为 0 时 `PlaceSystem` 返回错误「未配置系统账号」）。
- 分支（Global Constraints「系统实例」一条）：`startLease` 中 `if inst.System { state=running; expires_at 保持 NULL; 不写 pending_seconds; 调 AppHooks.OnInstanceRunning }`；`occupiedUntil` 中 `if inst.System { return lower + SystemLeaseHorizon }`（`lower` 取自己 lease 的下界）；`SuspendMachines / ResumeMachine` 对 System 不算 `paused_remaining`；`RefundRelease / AdminExtend / AdminMigrate / cancelInstance / AdminReassign / SetAutoRenew / Renew` 对 System → `ErrInvalidState`；`PlaceSystem` 校验 `machines.group_id = req.GroupID AND state='idle' AND agent_online` + 无活动实例 + 无相交 lease，否则 `ErrMachineUnavailable`。
- `pickMachines` 不改（池机组 `visible=false` 已排除）；补回归测试。

- [ ] **Step 1: 写失败的测试**

`system_test.go`（用 `helpers_test.go` 的 `newEnv` + fake Agent 队列，造 `purpose='llm'` 机组由迁移给出，`SELECT id FROM machine_groups WHERE purpose='llm'`）：
- `TestPlaceSystemRequiresPoolIdleOnline`：机器不在池 / 非 idle / 离线 / 有活动实例 / 有未来 reserved lease → `ErrMachineUnavailable`；满足条件 → 实例 `System`、`UserID == SystemUserID`、`ExpiresAt == nil`、`PendingSeconds == 0`、`PasswordAuth == false`、lease `active` 且 `upper − lower ≈ 10y`、机器 `allocated`、`agent_commands` 有 Create、`orders` / `wallet_ledger` 为空。
- `TestSystemInstanceNeverExpiresOrBills`：Create 成功 → running、`expires_at` 仍 NULL；推进 100 天，每天跑 `Sweep`（含 `ExpireDue / SendReminders / AutoRenewDue / RefreshCappedLeases`）→ 仍 running、无订单、无流水、fake Notifier 无短信、无告警；`SuspendMachines([m])` → `suspended_at` 非空、`paused_remaining` 为 NULL；`ResumeMachine` → 清空；`expires_at` 仍 NULL。
- `TestReleaseSystemFromEachState`：creating（取消指令、直接 wipe）、running（Stop → wipe）、stopped（直接 wipe）三种起点 → `wiping` → fake Agent 完成 → `released`、机器 `idle`；对非 System 实例 → `ErrInvalidState`。
- `TestSystemInstanceRejectsAdminOps`：`RefundRelease / AdminExtend / AdminMigrate / cancelInstance（经 Cancel）/ AdminReassign / SetAutoRenew / Renew` 全部 `ErrInvalidState`。
- `TestPoolMachinesInvisibleToRental`：池里一台空闲在线机器，用户 `Place`（即时）→ `ErrNoCapacity`；`PlaceBatch` 预订 `GroupID=nil` → `no_capacity`；`GroupID=池` → `group_not_found`（不可见）；`AdminMigrate` 别的实例 → 不落到池机器；`UnassignMachines` 移出后 `Place` 成功。
- `TestUpstreamTarget`：返回 `Target{InstanceID, LanIP, HostPubkey}`。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/rental/ -run 'TestPlaceSystem|TestSystemInstance|TestReleaseSystem|TestPoolMachines|TestUpstreamTarget' 2>&1 | head -5`
Expected: 编译错误。

- [ ] **Step 3: 实现**

`system.go`：`PlaceSystem` 事务：`SELECT … FROM machines WHERE id=$1 FOR NO KEY UPDATE` → 条件复核（SQL 一次 `NOT EXISTS` × 2）→ 生成密码 / 主机密钥（复用 `Place` 的辅助函数）→ `INSERT instances (…, system=true, password_auth=false, expires_at NULL)` → `INSERT machine_leases (status='active', period=tstzrange(now, now+horizon))` → `Fleet.MarkAllocated` → `insertEvent(…, "system instance: "+note)` → `Enqueue(CreateInstance)`。`ReleaseSystem` 复用 `Release` 的内部函数（抽出 `releaseLocked(ctx, tx, inst)`，`Release` 与 `ReleaseSystem` 都调它；`Release` 保留用户校验与 running 要求）。

- [ ] **Step 4: 运行测试并提交**

Run: `go vet ./... && go test ./internal/rental/ ./internal/app/ 2>&1 | tail -5`
Expected: 全部通过（现有 rental / app 测试不受影响）。

```bash
git add internal/rental
git commit -m "feat(rental): system instances for platform nodes (PlaceSystem / ReleaseSystem, never expire or bill, admin ops rejected), UpstreamTarget

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 3: `fleet` 池机组校验与 `LLMPoolGroupID`；`appdeploy` 内部应用

**Files:**
- Modify: `internal/fleet/groups.go`（`Group.Purpose`、`GroupRow.Purpose`、`LLMPoolGroupID`、`UpdateGroup / DeleteGroupIn` 校验）、`internal/fleet/groups_test.go`、`internal/appdeploy/types.go`（`App.Internal`）、`internal/appdeploy/catalog.go`（列、`ListPublished` 过滤、`AppInput.Internal`）、`internal/appdeploy/service.go`（`Create` 对 internal 的校验）、`internal/appdeploy/importer.go`（`appJSON.Internal`）、`internal/appdeploy/catalog_test.go`、`internal/appdeploy/service_test.go`、`internal/appdeploy/view.go`（`AdminView` / 后台应用行加 `internal`）
- Test: 上述 `_test.go`

**Interfaces:**
- Produces：
  ```go
  // fleet
  func (s *Service) LLMPoolGroupID(ctx context.Context, q db.Querier) (int64, error)  // purpose='llm' 的机组；不存在 → apperr.ErrNotFound
  // Group / GroupRow / GroupInput 增 Purpose string（GroupInput.Purpose 只在 Create 时可为 'rental'；'llm' 不能经 API 创建 → LLMModelInvalid 不对，这里用 apperr.BadRequest("不能创建 Token 服务池机组")）
  ```
  `UpdateGroup`：目标机组 `purpose='llm'` 且 `in.Visible == true` → `ErrInvalidState`（消息「Token 服务池不能对用户可见」）；`DeleteGroupIn` 对 `purpose='llm'` → `ErrInvalidState`。
  ```go
  // appdeploy
  type App struct{ …; Internal bool }
  // Catalog.ListPublished 过滤 internal；CatalogViews 同
  // Service.Create：app.Internal && req.UserID != s.d.SystemUserID → apperr.ErrAppNotFound
  // Deps 增 SystemUserID int64
  // importer：app.json 的 "internal": true
  ```
- 后台 `GET /groups` 返回 `purpose`（供池页识别）；控制台 `GET /groups` 不变（池不可见）。

- [ ] **Step 1: 写失败的测试**
- `fleet`：`TestLLMPoolGroupID`、`TestPoolGroupCannotBeVisibleOrDeleted`、`TestCreateGroupRejectsLLMPurpose`。
- `appdeploy`：`TestInternalAppHiddenFromPublished`、`TestInternalAppCreateOnlySystemUser`（普通用户 `app_not_found`、`SystemUserID` 成功）、`TestImportReadsInternal`（`apps_dir_test` 风格的临时目录）。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/fleet/ ./internal/appdeploy/ -run 'TestLLMPool|TestPoolGroup|TestCreateGroupRejects|TestInternalApp|TestImportReadsInternal' 2>&1 | head -5`

- [ ] **Step 3: 实现**（列加入 `groupCols` / `appCols`；校验放服务层）

- [ ] **Step 4: 运行测试并提交**

Run: `go vet ./... && go test ./internal/fleet/ ./internal/appdeploy/ ./internal/adminapi/ 2>&1 | tail -5`

```bash
git add internal/fleet internal/appdeploy
git commit -m "feat(fleet,appdeploy): llm pool group guards and lookup; internal apps hidden from users

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 4: 内部应用 `deploy/apps/vllm-node`

**Files:**
- Create: `deploy/apps/vllm-node/app.json`、`deploy/apps/vllm-node/spec.json`、`deploy/apps/vllm-node/app.sh`
- Modify: `deploy/apps/README.md`（新增一行与「internal」字段说明）、`internal/appdeploy/apps_dir_test.go`（目录校验自动覆盖新目录；追加断言 `vllm-node` 的 `internal=true`、`API_KEY` required、`SERVED_MODEL_NAME` 存在）

**Interfaces:**
- `app.json`：`{"name":"vLLM 节点（平台内部）","summary":"Token 商城推理节点，平台自动部署","description":"…","category":"llm","icon":"llm","sort":999,"internal":true,"version":"1.0.0","images":["LLM Serving"],"changelog":"初始版本"}`。
- `spec.json`：规格 §3.2 表（端口 8000 `health_path /health` `path /docs`；`timeout_minutes 120`；`min_data_free_gib 80`；变量 `MODEL_ID / MODEL_SOURCE / SERVED_MODEL_NAME / MAX_MODEL_LEN / API_KEY(secret, required, ^[A-Za-z0-9_-]{32,128}$) / GPU_MEMORY_UTILIZATION / MAX_NUM_SEQS / QUANTIZATION / REASONING_PARSER / TOOL_CALL_PARSER / TASK / MODEL_SIZE_GB(int 1~2000, default 20)`；`links` 同 vllm）。
- `app.sh`：基于 `deploy/apps/vllm/app.sh`（1.0.4）：`install` 下载（进度按 `MODEL_SIZE_GB`）；`start`：
  ```sh
  set -- vllm serve "$_dir" --host 127.0.0.1 --port "$SPARK_PORT" --served-model-name "$SERVED_MODEL_NAME" \
    --max-model-len "$MAX_MODEL_LEN" --gpu-memory-utilization "${GPU_MEMORY_UTILIZATION:-0.85}" \
    --max-num-seqs "${MAX_NUM_SEQS:-64}" --api-key "$API_KEY"
  [ "${TASK:-generate}" = embed ] && set -- "$@" --task embed
  [ "${QUANTIZATION:-none}" != none ] && set -- "$@" --quantization "$QUANTIZATION"
  [ "${REASONING_PARSER:-none}" != none ] && set -- "$@" --reasoning-parser "$REASONING_PARSER"
  [ "${TOOL_CALL_PARSER:-hermes}" != none ] && set -- "$@" --enable-auto-tool-choice --tool-call-parser "$TOOL_CALL_PARSER"
  ```
  日志轮转、`service.pid`、`tail.pid` 同 1.0.4；`stop` 同。

- [ ] **Step 1: 跑目录校验测试确认新目录被发现并失败**（先只建 `app.json`）
- [ ] **Step 2: 写三份文件**
- [ ] **Step 3: 运行并提交**

Run: `go test ./internal/appdeploy/ -run TestAppsDir 2>&1 | tail -3 && sh -n deploy/apps/vllm-node/app.sh`

```bash
git add deploy/apps
git commit -m "feat(apps): internal vllm-node app for Token mall nodes

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 5: `llm` 模型目录

**Files:**
- Create: `internal/llm/models.go`、`internal/llm/models_test.go`、`internal/llm/helpers_test.go`（`newEnv`：嵌入式 PG + 假时钟 + fake `AppCatalog`（返回 `vllm-node` 的 spec）+ fake `FleetOps`）
- Modify: `internal/llm/llm.go`（`RentalOps` 替换占位为 Task 2 的真实签名）

**Interfaces:**
```go
type ModelInput struct {
    Slug, Name, Description, Kind, Source, SourceID string
    ImageID int64; AppVersionID *int64
    ContextLen, MaxOutputTokens, DefaultMaxTokens int
    InputPrice, OutputPrice int64
    NodeEnv map[string]string
    Replicas, MaxConcurrency, Sort int
    Features []string
}
func (s *Service) CreateModel(ctx, actor audit.Actor, in ModelInput) (Model, error)       // 审计 llm.model.create
func (s *Service) UpdateModel(ctx, actor audit.Actor, id int64, in ModelInput) (Model, error)  // slug 不可改；审计 llm.model.update{before, after}
func (s *Service) SetModelStatus(ctx, actor audit.Actor, id int64, status string) (Model, error) // online 需 replicas ≥ 1；审计 llm.model.status
func (s *Service) DeleteModel(ctx, actor audit.Actor, id int64) error                    // draft 且无节点无用量；否则 ErrLLMModelInUse
func (s *Service) GetModel(ctx, id int64) (Model, error)
func (s *Service) ModelBySlug(ctx, q db.Querier, slug string) (Model, error)
func (s *Service) ListModels(ctx) ([]Model, error)              // 后台：全部
func (s *Service) ListOnlineModels(ctx) ([]Model, error)        // 控制台 / 网关
```
校验（`validateModelInput`）：规格 §3.1；`node_env` 白名单 = `vllm-node` 最新 published spec 的 env 名 − 平台保留五个；值经 `appdeploy.ValidateEnv(spec, env)`；`image_id ∈ version.ImageIDs` 且 enabled（经 fake / 真 `catalog`——`Deps` 增 `Images interface{ GetImage(ctx, q, id) (catalog.Image, error) }`）。

- [ ] **Step 1: 写失败的测试**：`TestCreateModelValidationMatrix`（slug 格式 / 重复 `llm_model_slug_exists` / source_id / 价格负数 / node_env 非白名单 / 保留变量 / image 不兼容 / context_len 范围）、`TestModelStatusMachine`（draft → online 需 replicas；offline；审计三条）、`TestDeleteModelConditions`、`TestUpdateModelAuditsPriceDiff`。
- [ ] **Step 2: 运行确认失败**
- [ ] **Step 3: 实现**
- [ ] **Step 4: 运行并提交**

```bash
git add internal/llm
git commit -m "feat(llm): model catalog with pricing, node parameters, status machine and audit

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 6: API Key 与 `Authorize` 缓存

**Files:**
- Create: `internal/llm/keys.go`、`internal/llm/authorize.go`、`internal/llm/keys_test.go`、`internal/llm/authorize_test.go`

**Interfaces:**
```go
type KeyInput struct{ Name string; ModelSlugs []string; MonthlyCap *int64; ExpiresAt *time.Time }
func GenerateKey() (plain, hash, hint string)       // plain = "sk-spark-" + 40 位 base62（crypto/rand）；hash = hex(sha256(plain))；hint = "sk-spark-…" + 末 4 位
func HashKey(plain string) string
func (s *Service) CreateKey(ctx, userID int64, in KeyInput, ip string) (plain string, k Key, err error)   // CanRent；≤ 20 个 active/disabled；审计 llm.key.create（user actor，diff 不含明文）；首次创建时插 llm_accounts
func (s *Service) UpdateKey(ctx, userID, id int64, in KeyUpdate, ip string) (Key, error)   // name / status(active|disabled) / model_slugs / monthly_cap
func (s *Service) RevokeKey(ctx, userID, id int64, ip string) error
func (s *Service) ListKeys(ctx, userID int64) ([]Key, error)                              // 不含 revoked 超过 30 天
func (s *Service) AdminListKeys(ctx, userID int64) ([]Key, error)
func (s *Service) AdminDisableKey(ctx, actor audit.Actor, id int64, reason string) error  // 审计 llm.key.admin_disable

type AuthContext struct {
    Key Key; UserID int64; Account Account
    UserStatus, KYCStatus string
    Limits Limits                      // 解析后的 RPM / TPM / Concurrency（默认或账户覆盖）
}
type AuthError struct{ Status int; Type, Code, Message string }   // 网关直接转 OpenAI 错误
func (e *AuthError) Error() string
// Authorize：按哈希取 Key → 用户状态 → 账户；结果缓存 KeyCacheTTL（sync.Map + 过期时间；撤销 / 停用 / 封禁最多 30 秒后生效）。
func (s *Service) Authorize(ctx, keyHash string, now time.Time) (AuthContext, error)
func (s *Service) InvalidateKey(hash string)          // 本进程内即时失效（控制台撤销时调用，顺手）
```
错误映射：无 / 不存在 / revoked / disabled / 过期 → `401 authentication_error invalid_api_key`；`banned` → `403 permission_error account_banned`；`kyc ≠ verified` → `403 permission_error kyc_required`；`blocked` → `403 permission_error account_blocked`（消息带 reason）；`debt > 0` → `402 insufficient_quota insufficient_quota`「账户有欠费 ¥X，请充值后再试」。月上限：`KeyAllowsModel(slug)`、`MonthCapExceeded(now)`（跨月按 `tz` 比较 `month_ym`）。

- [ ] **Step 1: 写失败的测试**：`TestGenerateKeyFormat`（前缀、长度、字母表、哈希 64 hex、hint）、`TestCreateKeyRequiresKYCAndLimit`（未实名 `kyc_required`、第 21 个 `llm_key_limit`、revoked 不计数、审计无明文）、`TestAuthorizeErrorMatrix`、`TestAuthorizeCacheTTL`（撤销后 29 秒仍通过、31 秒 401；`InvalidateKey` 立即 401）、`TestKeyModelFilterAndMonthlyCap`（跨月重置用假时钟到下月 1 日 00:00 Asia/Shanghai）。
- [ ] **Step 2～4**：确认失败 → 实现 → `go test ./internal/llm/` → 提交 `feat(llm): api keys and cached authorization`。

---
### Task 7: 账户、用量记账与价格计算

**Files:**
- Create: `internal/llm/accounts.go`、`internal/llm/usage.go`、`internal/llm/usage_test.go`

**Interfaces:**
```go
func Cost(m Model, promptTokens, completionTokens int) int64   // 微分；embedding 只算输入
func EstimateFen(m Model, bodyLen int, maxTokens *int) int64   // T2：ceil((bodyLen/3 × in + max_out × out) / 1e6)
type Usage struct {
    RequestID string; UserID, KeyID, ModelID int64; NodeID *string
    Endpoint string; Stream bool
    PromptTokens, CompletionTokens int; Estimated bool
    InputPrice, OutputPrice, CostMicro int64
    Status string; HTTPStatus, LatencyMS int; TTFTMS *int
    ClientIP string; At time.Time
}
// RecordUsage：llm_accounts(user) FOR UPDATE（无则插）→ INSERT llm_usage → unsettled_micro += cost → llm_api_keys SET last_used_at, month_cost_micro（month_ym ≠ 当月则重置后累加）
func (s *Service) RecordUsage(ctx, u Usage) error
func (s *Service) EnsureAccount(ctx, q db.Querier, userID int64) (Account, error)
func (s *Service) Account(ctx, userID int64) (AccountView, error)         // 控制台 /llm/account：debt、unsettled(分, ceil)、today/month cost（daily + 未结算）、limits、blocked、kyc_verified
func (s *Service) ListUsage(ctx, f UsageFilter, p paging.Page) ([]UsageRow, int, error)   // 控制台 / 后台共用；UserID 为 0 表示不过滤（后台）
func (s *Service) DailyUsage(ctx, userID int64, from, to time.Time) ([]DailyRow, error)
func (s *Service) ExportUsage(ctx, f UsageFilter, fn func(UsageRow) error) error            // csvx.MaxRows
func (s *Service) AdminUpdateAccount(ctx, actor audit.Actor, userID int64, in AccountUpdate) (Account, error)  // 限额 / blocked；审计 llm.account.update
func (s *Service) AdminListAccounts(ctx, f AccountFilter, p paging.Page) ([]AccountRow, int, error)
```

- [ ] **Step 1: 写失败的测试**：`TestCostAndEstimate`（表驱动：chat 100/50 @ 50/150 → 12,500；embedding 忽略输出；估算 ceil；max_tokens 超过 max_output 被夹）、`TestRecordUsageUpdatesAccountAndKey`（`unsettled_micro`、`last_used_at`、`month_cost_micro` 跨月重置）、`TestRecordUsageConcurrent50`（50 goroutine → 不变量 1）、`TestRecordUsageZeroCostStatuses`、`TestAccountViewAggregates`、`TestAdminUpdateAccountLimits`（null 恢复默认、blocked 需 reason）。
- [ ] **Step 2～4**：确认失败 → 实现 → 测试 → 提交 `feat(llm): accounts, usage recording and pricing`.

---
### Task 8: 结算 `llm.Settle` 与清理

**Files:**
- Create: `internal/llm/settle.go`、`internal/llm/settle_test.go`
- Modify: `internal/llm/helpers_test.go`（fake `WalletOps` 换成真 `wallet.Service`——结算必须测真实 `ApplyUpTo`）

**Interfaces:**
```go
// Settle：清扫附加任务。候选 llm_accounts WHERE unsettled_micro > 0 OR debt > 0 ORDER BY updated_at LIMIT 500；逐账户独立事务（40P01 / 40001 重试一次）；
// 然后 purge：DELETE llm_usage WHERE id IN (SELECT id … WHERE created_at < now − UsageRetention AND settlement_id IS NOT NULL LIMIT PurgeBatch)。
func (s *Service) Settle(ctx context.Context) error
func (s *Service) settleAccount(ctx context.Context, userID int64, now time.Time) (settled bool, err error)   // 规格 §7 伪代码逐行实现
func (s *Service) ListSettlements(ctx, userID int64, p paging.Page) ([]Settlement, int, error)
func (s *Service) AdminListSettlements(ctx, f SettlementFilter, p paging.Page) ([]Settlement, int, error)
```
备注格式：`"Token 消费 %d 次请求 · 输入 %d · 输出 %d token"`，含欠费时追加 `"（含欠费 ¥%.2f）"`；纯扣欠费：`"Token 消费欠费补扣"`。

- [ ] **Step 1: 写失败的测试**：`TestSettleThresholds`（¥0.5 未到 10 分钟不结；10 分钟后结；≥ ¥1 立即结）、`TestSettleCarry`（3 × 400,000 微分：前两次无流水、第三次 1 分 + carry 200,000；不变量 2）、`TestSettleInsufficientBalanceCreatesDebt`（余额 3 分、应扣 10 → 流水 −3、debt 7、不变量 3）、`TestSettleDebtRepaidOnRecharge`（充值后无新用量也扣欠费、一条流水、备注含「欠费补扣」）、`TestSettleMaxIDBoundary`（事务中用第二个连接插新用量 → 留到下次；用 `settleAccount` 的测试钩子在 UPDATE 前插入）、`TestSettleReplayIsNoop`（预先插一条 `ref_id = 下一个 sid` 的 llm_pay 流水 → `ApplyUpTo` `ErrDuplicate` → 事务回滚、`unsettled_micro` 不变、记 Error）、`TestSettleDailyAggregate`（跨 Asia/Shanghai 日界）、`TestPurgeOnlySettledOld`、`TestSettleManyAccountsAndInvariants`（20 用户随机用量 → `CheckInvariants`）。
- [ ] **Step 2～4**：确认失败 → 实现 → `go test ./internal/llm/ -run TestSettle` → 提交 `feat(llm): per-user usage settlement into one llm_pay ledger entry with carry and debt`.

---
### Task 9: `Limiter` 与 `Router`

**Files:**
- Create: `internal/llm/limiter.go`、`internal/llm/router.go`、`internal/llm/limiter_test.go`、`internal/llm/router_test.go`

**Interfaces:**
```go
type Limits struct{ RPM, TPM, Concurrency int }
type Limiter struct{ /* mu; per-user: hits []time.Time, tokens []tokenHit, inflight int, inflightEst int64 */ }
func NewLimiter() *Limiter
// Acquire：并发 / RPM / TPM（含 estTokens）三检；通过则 inflight+1、记 RPM；返回 release 函数（inflight−1）。失败返回 *LimitError{Code, RetryAfter}。
func (l *Limiter) Acquire(userID int64, lim Limits, estTokens int, now time.Time) (release func(), err error)
func (l *Limiter) AddTokens(userID int64, n int, now time.Time)         // 请求结束按实际 token 记 TPM
func (l *Limiter) ReserveEstimate(userID int64, fen int64) (release func())   // 预算在途估算
func (l *Limiter) InflightEstimate(userID int64) int64

type RouteNode struct{ ID string; ModelID int64; MaxConcurrency int; Target upstream.Target }
type Router struct{ /* nodes by model; inflight map[nodeID]*atomic.Int32; lastNode map[keyID]nodeID */ }
func NewRouter(s *Service) *Router
func (r *Router) Refresh(ctx context.Context) error        // SELECT ready 节点 JOIN 模型 max_concurrency JOIN 实例 lan_ip / host pubkey
func (r *Router) Run(ctx context.Context, every time.Duration)
// Pick：ErrNoNode（无 ready）/ ErrAllBusy（全满）；exclude 用于失败转移
func (r *Router) Pick(modelID, keyID int64, exclude map[string]bool) (RouteNode, error)
func (r *Router) Acquire(nodeID string) (release func())    // inflight ±1
func (r *Router) Inflight(nodeID string) int
func (r *Router) Remember(keyID int64, nodeID string)
```
`Pick` 策略：`lastNode[keyID]` 存在且 ready 且未被排除且 `inflight < 0.8 × max` → 它；否则最少在途且 `inflight < max`；都 ≥ max → `ErrAllBusy`。

- [ ] **Step 1: 写失败的测试**：`TestLimiterRPMWindow`（60 次通过、第 61 次拒绝且 `RetryAfter` ≈ 窗口剩余、滑过后恢复）、`TestLimiterTPMIncludesEstimate`、`TestLimiterConcurrencyRelease`、`TestLimiterEstimateReserve`、`TestRouterPrefersLastNodeUntil80Percent`、`TestRouterLeastInflightAndExclude`、`TestRouterNoNodeVsAllBusy`、`TestRouterRefreshDropsNonReady`。
- [ ] **Step 2～4**：确认失败 → 实现 → 测试 → 提交 `feat(llm): per-user limiter and node router`.

---
### Task 10: `llmtest.FakeVLLM` 与 `llmgw` 网关

**Files:**
- Create: `internal/llmgw/llmtest/fake_vllm.go`、`internal/llmgw/server.go`、`internal/llmgw/auth.go`、`internal/llmgw/proxy.go`、`internal/llmgw/stream.go`、`internal/llmgw/errors.go`、`internal/llmgw/server_test.go`、`internal/llmgw/stream_test.go`
- Modify: `internal/llm/llm.go`（`Service.Router() / Limiter()`、`NodeKey(ctx, nodeID) (string, error)` 解密节点密钥）

**Interfaces:**
```go
// llmtest
type FakeVLLM struct {
    Served []string; RequireKey string
    PromptTokens, CompletionTokens int; Chunks []string
    Delay time.Duration; FailFirstN int32 /*503*/; DropAfterChunks int /*>0 时发完 N 块断开*/
    mu sync.Mutex; Requests []RecordedRequest
}
func (f *FakeVLLM) ServeHTTP(w http.ResponseWriter, r *http.Request)
func (f *FakeVLLM) Recorded() []RecordedRequest   // Body []byte, Authorization string, Path string

// llmgw
type Config struct{ MaxBodyBytes int64; TotalTimeout, TTFTTimeout time.Duration; AuthFailLimit int; Transport func(node llm.RouteNode) http.RoundTripper /* nil = 经 Upstream.Dial */ }
type Server struct{ svc *llm.Service; cfg Config; log *slog.Logger; clk clock.Clock; authLim *ipLimiter; transports sync.Map }
func New(svc *llm.Service, cfg Config, clk clock.Clock, log *slog.Logger) *Server
func (g *Server) Router() http.Handler     // chi：OPTIONS/*、GET /models、POST /chat/completions、/completions、/embeddings、NotFound → openai 404
func writeOpenAIError(w http.ResponseWriter, status int, typ, code, msg string, reqID string)
```
处理流程逐条实现规格 §6.1 第 1～11 步（`handleCompletion(endpoint)` 一个函数，三端点共用）；`Transport` 可注入（单测直接指向 `httptest.Server`；生产 `DialContext` → `Upstream.Dial(ctx, node.Target, llm.NodePort)`）。`SetWriteDeadline / SetReadDeadline` 经 `http.NewResponseController(w)`（失败只记日志）。流式解析器 `stream.go`：`type sseScanner struct{…}`，`Next() (event []byte, data []byte, ok bool)`；`countChunk(data) (content bool, usage *usage)`。记账用 `svc.RecordUsage`，失败 → `RaiseAlertOnce(critical, llm_usage_record_failed, "Token 用量记账失败，详情见日志")`。

- [ ] **Step 1: 写失败的测试**（`server_test.go` 用 `llm` 的 `newEnv` 造模型 / 节点行（直接 SQL 插 `ready` 节点与系统实例占位）、Key，`Config.Transport` 指向 `httptest.NewServer(fake)`）：
- `TestGatewayAuthMatrix`：无头 / 坏前缀 / 不存在 / disabled / revoked / 过期 → 401 `invalid_api_key`；封禁 403 `account_banned`；未实名 403 `kyc_required`；blocked 403；欠费 402；第 61 次 401 同 IP → 429。
- `TestGatewayRequestValidation`：非 JSON Content-Type 400；body 2 MiB + 1 → 413；`model` 缺 / 非字符串 400；未知模型 404 `model_not_found`；Key 不允许 403 `model_not_allowed`；embedding 模型调 chat 400。
- `TestGatewayLimits`：并发 1 + fake 延迟 → 429；RPM 2 → 第 3 次 429 带 `Retry-After`；月上限 1 分 → 429 `monthly_cap_exceeded`。
- `TestGatewayBudgetPrecheck`：余额 0 → 402 消息含「余额不足」；余额够 → 通过；在途估算累加（两并发、余额只够一个 → 第二个 402）。
- `TestGatewayNonStreamUsageAndPassthrough`：200 透传 body 字节；`llm_usage` 一行 `cost_micro` 正确、`node_id`、`http_status 200`、`X-Request-Id` 头与行一致；上游 400 → 透传 400、`cost_micro 0`、`status ok`。
- `TestGatewayStreamPassthroughAndUsage`：fake 发 5 块 + usage + `[DONE]`；客户端收到字节 == fake 发出字节；请求体被注入 `stream_options.include_usage`（fake 记录）且其它字段不变；usage 入账；`ttft_ms` 非空；`Content-Type: text/event-stream`。
- `TestGatewayClientAbortEstimates`：fake 每块延迟 50ms，客户端读 2 块后取消 → `status client_abort`、`completion_tokens 2`、`estimated true`、`prompt_tokens == len(body)/3`。
- `TestGatewayUpstreamDropAfterFirstByteNotCharged`：`DropAfterChunks 2` → 客户端收到 2 块 + error 事件；`status upstream_error`、`cost_micro 0`。
- `TestGatewayFailoverBeforeFirstByte`：两节点，节点 A fake `FailFirstN 1`（503）→ 请求 200、A 的 `probe_fail_count 1`、用量 `node_id == B`；两节点都 503 → 502 `upstream_error`、`cost_micro 0`。
- `TestGatewayNoNodeAndBusy`：无 ready → 503 `model_unavailable`；`max_concurrency 1` + 在途 → 503 `server_overloaded` `Retry-After: 2`。
- `TestGatewayModelsAndCORS`：`GET /v1/models` 列表 / 过滤；`OPTIONS` 200 带 `Access-Control-Allow-Origin: *`；未知路径 404 OpenAI 格式。
- `TestGatewayDeadlinesExtended`：自定义 `ResponseWriter` 实现 `SetWriteDeadline`，断言被调用且 ≥ now + 9 分钟。
- `TestFakeVLLMBehaves`（`llmtest` 自测）。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/llmgw/... 2>&1 | head -5`

- [ ] **Step 3: 实现**（文件分工见 Files；`errors.go` 的 code → type 映射表）

- [ ] **Step 4: 运行测试并提交**

Run: `go vet ./... && go test ./internal/llmgw/... ./internal/llm/ 2>&1 | tail -5`

```bash
git add internal/llmgw internal/llm
git commit -m "feat(llmgw): OpenAI-compatible gateway (auth, limits, budget precheck, routing, SSE passthrough with usage, failover, usage recording) and FakeVLLM

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 11: 节点 `Controller` 与 `Prober`

**Files:**
- Create: `internal/llm/nodes.go`（节点行读写、事件、`AdminDrain / AdminRebuild / AdminRemove / ListNodes / GetNode / NodeEvents / PoolView / MachineHasLiveNode`）、`internal/llm/controller.go`、`internal/llm/prober.go`、`internal/llm/controller_test.go`、`internal/llm/prober_test.go`
- Modify: `internal/llm/helpers_test.go`（fake `RentalOps` / `AppOps` 记录调用并可设实例 / 部署状态）

**Interfaces:**
```go
type Controller struct{ s *Service; probe *Prober }
func NewController(s *Service) *Controller
func (c *Controller) Run(ctx context.Context)             // 每 ControllerInterval：pg_try_advisory_lock(hashtext('llm-controller')) → Tick → probe.Tick
func (c *Controller) Tick(ctx context.Context, now time.Time) (TickReport, error)   // 规格 §5.2 第 1～5 步；TickReport{Provisioned, Drained, Failed, Removed int}
type Prober struct{ s *Service; probe func(ctx, node Node, target upstream.Target, key string) error /* 可注入 */ }
func (p *Prober) Tick(ctx context.Context, now time.Time) error   // 规格 §5.3；并发 ≤ 16
// 后台操作
func (s *Service) AdminDrainNode(ctx, actor audit.Actor, id string) (Node, error)
func (s *Service) AdminRebuildNode(ctx, actor audit.Actor, id string) (Node, error)      // ready/unhealthy → Redeploy → provisioning；failed → 新建节点行（同机器可用时）
func (s *Service) AdminRemoveNode(ctx, actor audit.Actor, id, reason string) (Node, error)   // 立即 ReleaseSystem → removing
func (s *Service) MachineHasLiveNode(ctx, machineID int64) (bool, error)
func (s *Service) PoolView(ctx) (PoolView, error)        // 池机组机器 + 节点 + 租约到期
func (s *Service) ListNodes(ctx, f NodeFilter) ([]NodeRow, error)   // 含 inflight（Router）、部署状态 / health / progress
```
建节点事务：`SELECT m.id FROM machines m WHERE <可建条件> ORDER BY id LIMIT $n FOR NO KEY UPDATE SKIP LOCKED` 在 **rental.PlaceSystem 之外**先选机器 ID（只读），然后对每台：生成密钥 → `Rental.PlaceSystem`（它自己锁机器并复核）→ 成功后 `INSERT llm_nodes(provisioning, instance_id)`；`PlaceSystem` 返回 `ErrMachineUnavailable` 则跳过该机器。节点事件每次状态变化写入。`Redeploy` 用 `appdeploy.RedeployRequest{UserID: SystemUserID, InstanceID, DeploymentID, EnvSet: false}`。

- [ ] **Step 1: 写失败的测试**（fake rental：`PlaceSystem` 返回 creating 实例并可被测试「推进」到 running；fake apps：`Create` 返回 queued 部署并可推进）：`TestControllerProvisionsToReplicas`（replicas 2、3 台池机器 → 2 个 provisioning、`PlaceSystem` 调 2 次；`MaxProvisionPerRound` 4）、`TestControllerPoolShortAlertOnce`、`TestControllerAdvancesProvisioning`（实例 running → `Create` 被调且 Env 含五个平台变量 + node_env；部署 running + health ok + 探测成功 → ready）、`TestControllerProvisionTimeoutFails`、`TestControllerScaleDownDrainsWorstFirst`、`TestControllerDrainWaitsInflightThenReleases`（Router 在途 1 → 不释放；归零或 60 秒 → `ReleaseSystem` → removing；实例 released → removed）、`TestControllerOfflineModelScalesToZero`、`TestControllerUnhealthyRebuildThenFailed`（10 分钟 → Redeploy、`rebuild_count 1`；3 次后 failed + 严重告警 + `ReleaseSystem`）、`TestProberThresholds`（3 次失败 → unhealthy + 告警一次；一次成功恢复；部署 failed 立即 unhealthy；实例 suspended 视为失败）、`TestAdminNodeOps`（drain / rebuild / remove 状态机、`llm_node_invalid_state`、审计）、`TestMachineHasLiveNode`。
- [ ] **Step 2～4**：确认失败 → 实现 → `go test ./internal/llm/` → 提交 `feat(llm): node controller (reconcile, provision, drain, rebuild) and health prober`.

---
### Task 12: 控制台 API 与 `/v1` 挂载

**Files:**
- Create: `internal/api/llm.go`、`internal/api/llm_test.go`
- Modify: `internal/api/server.go`（字段 `LLM *llm.Service`、`LLMGateway http.Handler`；`/llm/models` 进匿名限流组；`requireUser` 组内 `/llm/*`；`r.Mount("/v1", s.LLMGateway)`；`requireJSONForWrites` 与 `SameSiteGuard` 的 `/v1/` exempt）、`internal/api/auth.go`（`/me` 的 `llm_month_cost`）、`internal/llm/views.go`（`ModelPublicView / AccountView / KeyView / UsageRowView / SettlementView`）

**Interfaces:**
- 路由与响应字段 = 规格 §10.1；`POST /llm/keys` 201 `{key, item}`；`DELETE /llm/keys/{id}` 200 `{ok}`；`GET /llm/usage` 默认 7 天、窗口 > 90 天 → 400。
- `s.LLM == nil` 时不注册 `/llm/*`（现有 api 测试不传）；`s.LLMGateway == nil` 时不 Mount。

- [ ] **Step 1: 写失败的测试**：`TestLLMModelsAnonymous`、`TestLLMKeysFlow`（创建 201 明文一次、列表 hint、更新、撤销、第 21 个 409、未实名 403）、`TestLLMAccountAndUsage`（造用量 + 结算 → account / usage / daily / settlements 字段）、`TestMeIncludesLLMMonthCost`、`TestV1MountedAndExempt`（`POST /v1/chat/completions` 带 `Origin: https://evil` 与 `Content-Type: application/json` 到达网关（401 OpenAI 格式而非 403）；`Content-Type: text/plain` → 400 OpenAI 格式而非 `bad_request`）。
- [ ] **Step 2～4**：确认失败 → 实现 → `go test ./internal/api/` → 提交 `feat(api): Token mall console endpoints and /v1 gateway mount`.

---
### Task 13: 后台 API、角色矩阵、审计

**Files:**
- Create: `internal/adminapi/llm.go`、`internal/adminapi/llm_test.go`
- Modify: `internal/adminapi/server.go`（`LLM *llm.Service`、`llmRoutes(g)`）、`internal/adminapi/groups.go`（池机组移出前 `MachineHasLiveNode` → `ErrLLMPoolMachineBusy`；`GET /groups` 带 `purpose`）、`internal/adminapi/views.go`（`instanceView.System`）、`internal/adminapi/dashboard.go`（`llm` 字段）、`internal/adminapi/users.go`（`include_system`、详情 `llm` 块；`Ban` 系统用户 `invalid_state`——在 `identity.Ban` 里判 `users.system`）、`internal/adminapi/finance.go`（`/ledger` kind 已由 `ledgerKinds` 放行，核对）、`internal/adminapi/permissions_test.go`、`internal/identity/admin.go`、`internal/llm/views.go`（`AdminModelView`（含节点计数）、`NodeRow`、`PoolView`、`AccountRow`、`Summary`）、`internal/appdeploy/view.go`（后台应用行 `internal`）

**Interfaces:** 规格 §10.2 全部路径与角色；`POST /llm/models/{id}/status`、`POST /llm/nodes/{id}/remove` 走 `withStepUp`；CSV 用现有 `csvExport`（列见规格）。

- [ ] **Step 1: 写失败的测试**：`permissions_test.go` 表加全部 `/llm/*`；`TestAdminLLMModelsCRUD`（super；ops 403；状态需 TOTP）；`TestAdminLLMPoolAndNodes`（池视图；移出有节点机器 409；drain / rebuild / remove（TOTP））；`TestAdminLLMAccountsUsageExport`（finance；CSV 首行列名、费用 6 位小数）；`TestAdminUsersHideSystem`（默认不含、`include_system=true` 含；封禁系统用户 409）；`TestAdminInstanceViewSystem`；`TestDashboardLLM`。
- [ ] **Step 2～4**：确认失败 → 实现 → `go test ./internal/adminapi/ ./internal/identity/` → 提交 `feat(adminapi): Token mall admin endpoints (models, pool, nodes, accounts, usage, settlements, summary), role matrix and audit`.

---
### Task 14: 装配、配置、启动、README

**Files:**
- Modify: `internal/app/app.go`（读 `SystemUserID`；`rental.Deps.SystemUserID`、`appdeploy.Deps.SystemUserID`；`llm.NewService`（`Rental: r, Apps: apps, AppCat: appCat, Fleet: f, Wallet: w, Users: ids, Upstream: upstreamPool, Images: c`）；`llmgw.New`；`api.Server.LLM / LLMGateway`（`cfg.LLMGateway` 为 false 时 nil）；`adminapi.Server.LLM`；`App.LLM *llm.Service`、`App.LLMController *llm.Controller`；`RunAccessLoops` 启动 `LLM.Router().Run(ctx, RouterRefresh)`）、`internal/platform/config/config.go`（`LLMGateway, LLMController bool`；`loadLLM(or)`：`SPARK_LLM_GATEWAY` / `SPARK_LLM_CONTROLLER` `on/off` 默认 on，其它值报错；`FillLLMDefaults`）、`internal/platform/config/config_test.go`、`cmd/platform/main.go`（`RunSweeper(…, a.Billing.Sweep, a.Referral.Settle, a.LLM.Settle)`；`cfg.LLMController` → `go a.LLMController.Run(ctx)`，off 时 `log.Warn`）、`cmd/e2e-server/main.go`（Controller 关、Options）、`README.md`（配置表两行；「部署注意」：网关单副本 / 限额折算、`vllm-node` 导入发布、池机组说明、`/v1` 地址）

- [ ] **Step 1: 写失败的测试**：`config_test.go` 两个开关解析；`internal/app` 现有 `Build` 测试通过且 `a.LLM != nil`；`TestBuildFailsWithoutSystemUser`（删掉系统账号行 → `Build` 报错含「系统账号」）。
- [ ] **Step 2～4**：确认失败 → 实现 → `go vet ./... && go test ./internal/app/ ./internal/platform/config/ ./cmd/...` → 提交 `feat(app): wire llm service, gateway and controller; config switches; README`.

---
### Task 15: Go 集成测试与 e2e 钩子

**Files:**
- Create: `internal/app/llm_e2e_test.go`、`internal/e2ehooks/llm.go`
- Modify: `internal/e2ehooks/hooks.go`（注册 `/hooks/llm/upstream`、`/hooks/llm/node`、`/hooks/llm/tick`；`sweep` 追加 `LLM.Settle`）、`internal/app/export_test.go`（暴露需要的内部）

**Interfaces:**
- 钩子：`POST /hooks/llm/upstream {port, served?: []string, prompt_tokens?, completion_tokens?, chunks?: []string, fail_first_n?, delay_ms?}` → 在 `sshtest` 该端口挂 `FakeVLLM`（替换旧的）；`POST /hooks/llm/node {model_id, machine_id}` → `PlaceSystem` + fake Agent 完成 Create + 插 `llm_nodes(ready)`（`api_key_enc` 随机）+ `Router.Refresh` → `{node_id, instance_id}`；`POST /hooks/llm/tick` → `Controller.Tick` + `Prober.Tick`（探测函数经 `Upstream.Dial` 真走 sshtest）→ `TickReport`。
- 集成测试（`internal/app`，`newE2E` 环境 + `sshtest` shell 模式 + FakeVLLM 挂 8000）：规格 §16.2 七组：`TestLLMFullChain`、`TestLLMOverdraftSettlesIntoDebt`、`TestLLMRateLimits`、`TestLLMFailoverAndUnhealthy`、`TestLLMDrainReleasesMachine`（含「仍在池内不可租、移出后可租」）、`TestLLMUnassignBusyMachineRejected`、`TestLLMBanAndRevoke`；每个结束 `llm.CheckInvariants`。

- [ ] **Step 1: 写测试（会失败：钩子 / 辅助不存在）**
- [ ] **Step 2: 实现钩子与辅助**
- [ ] **Step 3: 运行并提交**

Run: `go vet ./... && go test ./internal/app/ -run TestLLM -count=1 2>&1 | tail -10 && go test ./... 2>&1 | tail -3`

```bash
git add internal/app internal/e2ehooks
git commit -m "test(llm): end-to-end Go integration (chain, overdraft, limits, failover, drain, guards) and e2e hooks

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 16: 真机检查点 A（192.168.1.221）——后端 + API

**Files:**
- Create: `docs/superpowers/acceptance-plan9.md`（检查点 A 部分）

**步骤**（规格 §16.4 第 1～9 项，用 curl / 后台 API / 控制台 API，不依赖本计划的页面）：
- [ ] 构建平台（`make web && go build ./cmd/platform`），迁移开发库到 00022，启动（`SPARK_SWEEP_INTERVAL=1m`、`SPARK_AGENT_BANDWIDTH_MBPS=1000`、其余同计划 7 验收）。Agent 用计划 7 版本（含 `image.env`），不重装。
- [ ] `platform admin app import deploy/apps` → `vllm-node 1.0.0 created`；后台 API 发布（TOTP）；控制台 `GET /api/v1/apps` 不含它。
- [ ] 后台 API 建模型 `qwen3-4b`（规格 §16.4 第 2 项参数）→ 上线；`POST /groups/{池}/machines {machine_ids:[1]}`；`GET /admin/api/v1/llm/pool` 显示机器空闲无节点。
- [ ] 等 Controller：`GET /llm/nodes` 出现 provisioning → 实例 running → 部署进度 → `ready`（记录各阶段时间，目标 ≤ 15 分钟）；`nvidia-smi` 有 vLLM；`GET /api/v1/llm/models` `available=true`。
- [ ] 控制台 API 建 Key；`curl /v1/models`、非流式、流式（记录响应 usage 与 `X-Request-Id`）；OpenAI Python SDK 一次流式对话（Mac 上 `pip install openai` 若未装）；`GET /api/v1/llm/usage` 三条。
- [ ] 结算：循环调用到未结算 ≥ ¥1 或等 10 分钟 → 钱包流水 `llm_pay`；`go test ./internal/llm -run TestInvariantsAgainstDevDB`（Task 15 已加该只读测试，用 `SPARK_DATABASE_URL` 连开发库）。
- [ ] 限流（RPM 2、月上限 ¥0.01）、余额 / 欠费（调账 0 → 402；¥0.01 + 10 并发 → 欠费 → 充值恢复）、Key 停用 / 撤销 / 模型白名单。
- [ ] 故障：容器内杀 vLLM 进程 → unhealthy + 告警 + 503 → 后台「重建」→ ready（记录耗时，`skip install`）；协调人停 Agent 5 分钟 → unhealthy → 启 Agent → ready 不重建。
- [ ] 记录：每项通过 / 失败、时间、关键输出、缺陷表（编号 `A-n`、严重度、根因、处置）；页面类问题留到检查点 B。**不要修改真机上的其它东西**，不留测试数据之外的痕迹；验收结束保持节点 ready（检查点 B 要用）。

```bash
git add docs/superpowers/acceptance-plan9.md
git commit -m "docs: plan 9 real-machine checkpoint A (backend + API) on 192.168.1.221

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 17: 控制台基础：类型、`logic/tokens.ts`、导航与市场横幅、`/tokens` 模型 / 快速开始 Tab

**Files:**
- Create: `web/console/src/logic/tokens.ts`、`web/console/src/logic/__tests__/tokens.spec.ts`、`web/console/src/views/TokensView.vue`、`web/console/src/components/tokens/ModelsTab.vue`、`web/console/src/components/tokens/QuickStartTab.vue`
- Modify: `web/console/src/router.ts`（`/tokens`，`meta.title 'Token 商城'`，不要求 auth）、`web/console/src/layouts/ConsoleLayout.vue`（第 5 项）、`web/console/src/views/MarketView.vue`（横幅卡片 `market-tokens-banner`，取 `GET /llm/models` 最低 `input_price`）、`web/shared/src/format.ts`（`formatPricePerM(fen) → '¥0.50'`、`formatMicroFen(micro) → '¥0.0125' | '< ¥0.01'`、`formatTokens(n) → '12.3K'`）

**Interfaces（`logic/tokens.ts`）:**
```ts
export function pricePerMillionYuan(fen: number): string            // 50 → '0.50'
export function costYuan(costMicro: number): string                 // 12500 → '0.000125'；显示用 formatMicroFen
export function curlExample(baseUrl: string, model: string): string
export function pythonExample(baseUrl: string, model: string): string
export function keyHintMask(hint: string): string
export const llmErrorHelp: Record<string, string>                   // code → 中文说明（快速开始的错误码表）
```
页面：`TokensView.vue` 用 `el-tabs`（`tokens-tab-models / keys / usage / logs / quickstart`），未登录只渲染 models 与 quickstart，其它 Tab 显示 `GuideDialog` 风格的登录引导；`ModelsTab` 卡片 `tokens-model-card-<slug>`（名称、slug 复制、上下文、features 标签、价格两行、状态标签 `tokens-model-available`）；`QuickStartTab` 显示 `base_url`（复制）、curl / Python 代码块（复制）、说明列表（流式 usage、限额、欠费）。

- [ ] **Step 1: 写失败的 Vitest**（`tokens.spec.ts`：格式化、示例含 base_url 与 model、`ConsoleLayout` 导航含 Token 商城）
- [ ] **Step 2～4**：实现 → `cd web && npm test && npm run build` → 提交 `feat(console): Token mall entry, models and quick start`.

---
### Task 18: 控制台 API Keys Tab

**Files:**
- Create: `web/console/src/components/tokens/KeysTab.vue`、`web/console/src/components/tokens/CreateKeyDialog.vue`、`web/console/src/logic/__tests__/keys.spec.ts`
- Modify: `web/console/src/logic/tokens.ts`（`validateKeyInput`、`monthlyCapYuanToFen`）

**行为：** 表格列（名称、Key、允许模型（空 = 全部）、月上限 / 本月已用、最近使用、到期、状态）；`tokens-key-create` 按钮：未实名禁用 + tooltip + 链接 `/kyc`；对话框表单（名称必填 ≤ 32、模型多选、月上限元、到期日期）→ 成功后切换为「只显示一次」面板（`tokens-key-plain`、复制按钮 `tokens-key-copy`、警示文案），关闭需二次确认；行操作停用 / 启用（`PUT status`）、编辑（名称 / 模型 / 月上限）、撤销（`ElMessageBox.confirm`）。错误 `llm_key_limit` 文案直显。

- [ ] **Step 1～4**：Vitest（校验、单位换算）→ 实现 → `npm test && npm run build` → 提交 `feat(console): API key management with one-time secret display`.

---
### Task 19: 控制台用量 / 日志 / 结算 Tab、欠费提示、钱包标签、帮助页、我的页

**Files:**
- Create: `web/console/src/components/tokens/UsageTab.vue`、`web/console/src/components/tokens/LogsTab.vue`
- Modify: `web/console/src/views/TokensView.vue`（顶部欠费 / 停用提示条 `tokens-debt-alert`，带「去充值」→ `/wallet/recharge`）、`web/console/src/views/WalletView.vue`（`llm_pay` 标签色同扣款）、`web/console/src/views/HelpView.vue`（「Token 商城」一节）、`web/console/src/views/AccountView.vue`（入口 + `llm_month_cost`）

**行为：** `UsageTab`：统计卡（今日 / 本月消费、本月 token、未结算、欠费）；按天 × 模型表（`GET /llm/usage/daily?days=30`）；结算记录表（`GET /llm/settlements`，`wallet_ledger_id` 链接 `/wallet?ledger=<id>`）。`LogsTab`：筛选（模型、Key、日期）+ 分页表（`GET /llm/usage`），估算 token 前缀 `≈`，状态标签中文，`request_id` 复制。

- [ ] **Step 1～4**：Vitest（金额 / token 格式、估算标记）→ 实现 → `npm test && npm run build` → 提交 `feat(console): usage, request logs, settlements, debt banner, help and wallet labels`.

---
### Task 20: 后台菜单、`/llm` 总览与模型 Tab

**Files:**
- Create: `web/admin/src/views/LlmView.vue`、`web/admin/src/components/llm/ModelsTab.vue`、`web/admin/src/components/llm/ModelDrawer.vue`、`web/admin/src/logic/llm.ts`、`web/admin/src/logic/__tests__/llm.spec.ts`
- Modify: `web/admin/src/menu.ts`（`{ key: 'llm', path: '/llm', label: 'Token 商城', roles: ALL_ROLES }`）、`web/admin/src/router.ts`、`web/admin/src/__tests__/menu.spec.ts`

**行为：** `LlmView` Tab（`llm-tab-overview / models / nodes / accounts / usage`，按 `hasRole` 显示）；总览卡片（`GET /llm/summary`）+ 按模型节点卡；模型表 + 抽屉表单（结构化：基本 / 来源与镜像 / 价格（元 / 百万）/ 节点参数（按 `GET /apps/{vllm-node}` 的 spec 渲染 `node_env` 字段）/ 副本与并发）；状态切换走 `TotpDialog`；删除确认。保存 `source / image / context_len / node_env` 变化时提示「已有 N 个节点仍按旧参数运行」。

- [ ] **Step 1～4**：Vitest（元 ↔ 分换算、表单校验、menu.spec）→ 实现 → `npm test && npm run build` → 提交 `feat(admin): Token mall overview and model management`.

---
### Task 21: 后台节点与机器池 Tab、机器详情 / 实例标签

**Files:**
- Create: `web/admin/src/components/llm/NodesTab.vue`、`web/admin/src/components/llm/NodeDrawer.vue`、`web/admin/src/components/llm/AddPoolMachineDialog.vue`
- Modify: `web/admin/src/views/MachineDetailView.vue`（池标签 + 节点链接）、`web/admin/src/views/InstancesView.vue`（「平台节点」标签）、`web/admin/src/views/InstanceDetailView.vue`（`system` 时隐藏退款 / 延长 / 换机）、`web/admin/src/logic/instanceOps.ts`（`canRefund/canExtend/canMigrate` 加 `!system`）

**行为：** 机器池表（`GET /llm/pool`）+「加入机器」对话框（列出不在池的机器，调用 `POST /groups/{pool}/machines`）+ 移出（409 文案直显）；节点表（`GET /llm/nodes`，10 秒自动刷新）+ 行操作排空 / 重建 / 移除（TOTP + 原因）；抽屉：节点事件时间线、部署状态与日志（复用计划 7 后台部署详情接口）。

- [ ] **Step 1～4**：Vitest（`instanceOps` 对 system 的判定）→ 实现 → `npm test && npm run build` → 提交 `feat(admin): node and machine pool management, platform-node instance labels`.

---
### Task 22: 后台账户 / 用量 Tab、用户详情区块、仪表盘、流水筛选、应用目录标签

**Files:**
- Create: `web/admin/src/components/llm/AccountsTab.vue`、`web/admin/src/components/llm/UsageTab.vue`
- Modify: `web/admin/src/views/UserDetailView.vue`（「Token 商城」区块）、`web/admin/src/views/UsersView.vue`（`include_system` 开关，默认关）、`web/admin/src/views/DashboardView.vue`（卡片）、`web/admin/src/views/LedgerView.vue`（类型选项）、`web/admin/src/views/CatalogView.vue` 或计划 7 的应用目录页（「内部」标签）、`web/admin/src/logic/csv.ts`（如需）

**行为：** 账户表（搜索、欠费 / 停用筛选；编辑限额对话框（空 = 默认）、停用（原因必填）；展开行显示 Key 列表与停用按钮）；用量 Tab（筛选 + 明细 + 按天 / 模型 / 用户汇总切换 + CSV 下载 + 结算记录子表）。

- [ ] **Step 1～4**：Vitest（限额表单空值语义）→ 实现 → `npm test && npm run build` → 提交 `feat(admin): accounts, usage and settlements, user detail block, dashboard card`.

---
### Task 23: Playwright 用例、钩子辅助、覆盖表

**Files:**
- Create: `web/e2e/helpers/llm.ts`（`seedLLMModel(admin api)`、`createPoolMachineAndNode()`（`/hooks/machines` + `/groups/{pool}/machines` + `/hooks/llm/upstream` + `/hooks/llm/node`）、`callChat(key, model, stream?)`（直接 `request.post('/v1/chat/completions')`）、`sweep()`）、`web/e2e/console/tokens.spec.ts`、`web/e2e/admin/llm.spec.ts`
- Modify: `web/e2e/admin/instances.spec.ts`（`A-INS-14`）、`web/e2e/admin/users.spec.ts`（`A-USR-08`）、`web/e2e/console/wallet.spec.ts`（`C-WAL-05` 或顺延编号）、`web/e2e/console/account.spec.ts`（`C-ACC-04`）、`web/e2e/coverage.json`、`web/playwright.config.ts`（如需顺序说明）

**用例：** 规格 §16.3 的 `C-TOK-01`～`10`、`A-LLM-01`～`09`、`A-INS-14`、`A-USR-08`、`C-WAL-05`、`C-ACC-04`（编号若与计划 8 已登记的冲突则顺延并在覆盖表注明）。改时钟的用例（结算 10 分钟）放在时钟推进顺序的说明位置；每个用例结束把节点 drain 或直接 `/hooks/llm/node` 的实例释放，避免影响后续租用用例的机器数。

- [ ] **Step 1: 写用例（会失败：testid 不存在或钩子未接）**
- [ ] **Step 2: 补 testid / 钩子细节**
- [ ] **Step 3: 运行并提交**

Run: `cd web && npm run e2e:coverage && npx playwright test e2e/console/tokens.spec.ts e2e/admin/llm.spec.ts e2e/admin/instances.spec.ts e2e/admin/users.spec.ts 2>&1 | tail -15 && npx playwright test 2>&1 | tail -5`
Expected: 新用例全绿、全量全绿、覆盖表核对通过。

```bash
git add web/e2e web/console web/admin
git commit -m "test(e2e): Token mall console and admin coverage (C-TOK, A-LLM, A-INS-14, A-USR-08)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 24: 真机验收 B（192.168.1.221）+ 验收记录 + 遗留清单 + `make check`

**Files:**
- Modify: `docs/superpowers/acceptance-plan9.md`（检查点 B）、`README.md`（复核）
- Create: `docs/superpowers/followups-plan9.md`

**步骤：**
- [ ] 用检查点 A 遗留的环境（节点 ready）重新构建含前端的平台并重启；用 Playwright 驱动真实 Chrome（`--host-resolver-rules`）走控制台 `/tokens` 全部 Tab（创建 Key、调用、日志、用量、结算记录）与后台 `/llm` 全部 Tab；截图留在验收 scratchpad。
- [ ] 规格 §16.4 第 10 项（replicas 0 → draining → removed → 机器 idle 仍在池 → 移出 → 市场页可租 → 下单成功后释放）、第 11 项（容器内无鉴权 401；用户 Key 打 ingress 域 302；库里无内容列；后台 secret `***`）、第 12 项（告警只有预期；`CheckInvariants`；三处计数一致）。
- [ ] 记录格式同 `acceptance-plan7.md`：环境、每项结论、缺陷表（`B-n`）、观察（`O-n`）、与计划 7 验收一样写明哪些只能由 e2e / 集成覆盖（多节点故障转移、并发打穿）。
- [ ] `followups-plan9.md`：规格 §17 的「上线前核对」与「明确不做」、验收缺陷的处置状态、与计划 7 / 8 遗留的交叉引用（洗机保留模型缓存、返利基数）。
- [ ] `make check` 全量通过（`go vet && go test ./... && sh deploy/agent/install_test.sh && web-test && e2e`）；输出贴进记录。
- [ ] 验收后把真机恢复到可出租状态：模型 offline、节点 removed、机器移出池（记录里写明）。

```bash
git add docs README.md
git commit -m "docs: plan 9 real-machine acceptance B, followups, README review

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
## 上线前需要核对
- 首批模型清单与定价、池机器数量（运营）；`vllm-node` 导入并发布；`LLM Serving` 镜像在 Harbor（计划 7 遗留）。
- 多副本：网关进程内限流按副本各算，建议用户 API 单副本或按副本数折算限额；Controller / Settle 咨询锁单副本（README）。
- `SPARK_DOMAIN` 即 `/v1` 的对外主机名；TLS 证书覆盖它（已是控制台域）。
- 帮助页 / 控制台计费文案（法务）；流式总带 usage 块的说明。
- 公网 `http.Server.WriteTimeout 60s` 对 `/v1` 由 `SetWriteDeadline` 覆盖；若前面有反向代理（nginx / caddy），其 `proxy_read_timeout` 需 ≥ 10 分钟且关闭缓冲（`X-Accel-Buffering: no` 已带）。

## 规格列出的遗留（本计划不做）
- Token 套餐 / 赠送 / 阶梯价 / 差异化定价 / 优惠券；Token 消费计入返利。
- `api.<domain>` 子域；Key 的 IP 白名单与作用域。
- 跨机并行、一机多模型、自动伸缩、预热；洗机保留模型缓存。
- 多副本共享限流；排队与优先级。
- 内容留存 / 审核；用量短信；Prometheus / GPU 利用率；`/v1/responses` 等端点；批量入池；节点日志实时流。

## 自检记录（写计划时完成）
- 24 个任务与规格 §18 一一对应；每个任务的 Files / Interfaces / 测试名 / 命令齐全。
- 真实名称核对：`wallet.ApplyUpTo`（计划 8 `internal/wallet/wallet.go:97`）、`appdeploy.Service.Create / Redeploy / Get`、`appdeploy.CreateRequest / RedeployRequest`、`appdeploy.Catalog.AppBySlug / LatestPublished / GetVersion / ListPublished / ImportDir`、`appdeploy.ValidateEnv`、`upstream.Pool.Dial / Exec`、`upstream.Target{InstanceID, LanIP, HostPubkey}`、`upstream.DialError`、`fleet.Service.ListGroups / UpdateGroup / DeleteGroupIn / AssignMachines / UnassignMachines / MarkAllocated / Release / MarkFaulty / RaiseAlert / RaiseAlertOnce`、`fleet.Alert`、`rental.Service.Place / PlaceBatch / Release / AdminExtend / AdminMigrate / Get / RunSweeper / Sweep`、`rental.Deps / Instance / AppHooks`、`identity.CanRent / UserState`、`referral.UserState`、`api.Server.calendarRateLimit / ipLimiter / ipRateLimit / requireUser / requireJSONForWrites`、`adminapi.handle / withStepUp / csvExport`、`audit.Actor.Entry / audit.Record`、`httpx.ClientIP / WriteError / NoStore / SameSiteGuard`、`csvx.MaxRows`、`tz`、`clock.Clock`、`secretbox.Box`、`randx`、`sshtest.Server.ServeHTTP`、`e2ehooks` 现有钩子名、`config.loadApps` 模式、`cmd/platform/main.go` 的 `RunSweeper(…, a.Billing.Sweep, a.Referral.Settle)`。
- 与计划 6 的衔接：池机组 `visible=false` 已被 `pickMachines` 排除（规格 §4.2 `(m.group_id IS NULL OR g.visible)`），本计划只加 `purpose` 列与校验；系统实例的 lease 与排他约束、`machine_leases_period_nonempty` 兼容（有界 10 年）。
- 与计划 7 的衔接：节点部署走用户路径 `appdeploy.Create`（系统账号为 owner），不新增 Runner 分支；`apps.internal` 是唯一的目录改动；`image.env` 依赖新版 Agent（计划 7 已要求）。
- 与计划 8 的衔接：`wallet_ledger_kind_check` 在 00021 基础上加一种；Down 的拒绝条件同模式；`llm_pay` 的 `ref_type ≠ 'order'`，返利扫描不受影响。
