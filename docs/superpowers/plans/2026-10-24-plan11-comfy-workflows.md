# Spark Cloud 计划 11：ComfyUI 工作流市场 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 后台维护带版本的 ComfyUI 工作流目录（工作流图 + 所需模型 + 所需自定义节点 + 可选的快速出图表单）；用户在控制台工作流市场一键把工作流安装到自己实例上的 ComfyUI（模型 / 节点在 Spark 上的实例容器内从 ModelScope / hf-mirror / Git 镜像下载），或开通实例时预装；已装工作流可更新 / 移除；极简「快速出图」表单经平台代理 ComfyUI API 出图；后台导入 / 版本 / 发布 / 统计；最后在 192.168.1.221 单机完成真机验收。

**Architecture:** 不加新的后台执行者。「安装工作流」= 改该 ComfyUI 部署的「应装集合」（新表 `deployment_workflows`）+ 计划 7 的 `appdeploy.Redeploy`：部署包多带 `workflows/manifest.json` 与 `workflows/<slug>.json`，comfyui 应用 1.1.0 的 `app.sh install` 按清单补模型 / 节点 / 工作流文件，`start` 重启 ComfyUI；进度、日志、失败、停机恢复、换机全部沿用计划 7。新包 `internal/comfywf`（目录 + 校验 + 安装 + 快速出图代理）通过 `appdeploy.Deps` 上的几个函数钩子（`BundleExtra / OnTransition / WorkflowViews / ResolveWorkflows`）接入，`appdeploy` 不 import `comfywf`。快速出图代理用 Runner 健康检查同款 `http.Transport{DialContext: Pool.Dial}`，只放行 `/prompt /history /queue /view /upload/image`。

**Tech Stack:** Go 1.27（`~/.local/go/bin`）、pgx v5、chi v5、embedded-postgres（`dbtest.Main`）；前端 Vue 3 + Element Plus + Vitest + Playwright（Chrome 通道）。**不新增任何 Go 模块依赖、不新增 npm 依赖。** 容器内脚本 POSIX sh + 镜像自带 `python3`。

**Spec:** `docs/superpowers/specs/2026-10-24-plan11-comfy-workflows-design.md`（裁定 D1～D16 为负责人默认）。上位：计划 7 规格与 `docs/superpowers/plans/2026-10-20-plan7-deploy.md`。

**前置：** 计划 7 已合并到 `main`。开工第一步把 `feat/plan11-comfy-workflows` rebase 到最新 `main`（`git rebase origin/main`），确认 `internal/appdeploy`、`deploy/apps/comfyui`、`web/console/src/components/AppsCard.vue` 都在。若计划 7 尚未合并，先停下来向负责人确认，不要在没有计划 7 代码的分支上实施。

## Global Constraints（所有任务通用）

- **不下载模型到开发 Mac。** 一切模型 / 节点下载只在 Spark 上的实例容器里发生（Task 8 核实地址、Task 16 验收）。单测 / 集成测试用假的 `modelscope / hf / git / pip` 可执行文件。
- **不碰测试 Spark**，除 Task 8（只做只读的目录核实，经一台临时实例）与 Task 16（验收）。
- TDD：先写失败的测试，再实现；每个任务结束 `make test`（Go）/ `npm test`（web）全绿、`go vet ./...` 干净、`npm run lint` 干净。提交信息中文或英文都可，格式同仓库惯例（`feat(comfywf): …` / `test(...)` / `docs: …`），每个任务至少一次提交。
- 常量集中在 `internal/comfywf/consts.go`：`MaxWorkflowsPerDeployment = 8`、`MaxGraphBytes = MaxAPIPromptBytes = 262144`、`MaxModelsPerVersion = 16`、`MaxNodesPerVersion = 8`、`MaxInputs = 24`、`MaxOutputs = 4`、`MaxRunsInFlight = 2`、`RunsPerHour = 120`、`MaxUploadBytes = 16 << 20`、`MaxViewBytes = 64 << 20`、`ProxyTimeout = 30s`、`ViewTimeout = 120s`、`RunWaitMax = 30m`、`MaxCoverBytes = 2 << 20`、`InstalledFilePrefix = "spark-"`、`ComfyPort = 8188`（只用于 fake / 测试默认，真实端口取部署 `spec.ports[0]`）、`DataDirName = "comfyui"`（容器内 `/root/data/comfyui`）。`appdeploy.MaxBundleBytes` 改为 `4 << 20`。
- 错误码在 `internal/platform/apperr/apperr.go` 新增（规格 §10）：`ErrWorkflowNotFound`（404）、`ErrWorkflowUnavailable`（409）、`ErrWorkflowExists`（409）、`ErrWorkflowLimit`（409）、`ErrWorkflowAppUnsupported`（409）、`ErrWorkflowVersionFrozen`（409）、`ErrWorkflowBusy`（429）、函数 `WorkflowVersionInvalid(path, reason string)`（400）、`WorkflowPromptRejected(msg string)`（422）、`WorkflowInputInvalid(label, reason string)`（400）。
- 审计动作：`workflow.create / workflow.update / workflow.delete / workflow.version.create / workflow.version.update / workflow.version.publish / instance.workflow_add / instance.workflow_update / instance.workflow_remove`；`labels.ts` 的 `auditActions` 同步。
- 所有对外 JSON 走视图结构体，不序列化带 `api_prompt` 本体的内部类型到控制台（只有后台版本详情接口返回它）。
- 容器内路径常量（`comfywf` 与 `app.sh` 两边一致）：`/root/data/comfyui/{models/<dir>,custom_nodes,user,input}`、工作流文件 `/root/data/comfyui/user/default/workflows/spark-<slug>.json`、部署包内 `workflows/manifest.json`、`workflows/<slug>.json`。测试里 `/root/data` 由 `SPARK_DATA_DIR` 覆盖（计划 7 已有）。
- 规格 §3.2 的正则集中在 `comfywf/manifest.go` 顶部，前端 `logic/workflows.ts` 复制同样的正则做即时校验（表单提示），后端为准。
- 前端：复用 `AppEnvForm.vue` 的表单渲染思路但**不改它**；新组件放 `web/console/src/components/`，纯函数放 `logic/workflows.ts` 并写 Vitest；`data-testid` 前缀 `wf-`。

## 文件结构

```
spark-cloud-p11/
├── migrations/00024_comfy_workflows.sql                                             # Task 1
├── deploy/apps/comfyui/{app.json,spec.json,app.sh}（升 1.1.0）                        # Task 7
├── deploy/workflows/README.md、deploy/workflows/<slug>/{workflow.json,graph.json,api.json,manifest.json,cover.png}  # Task 8
├── internal/
│   ├── comfywf/consts.go、types.go、manifest.go、builtin_nodes.go、hash.go、prompt.go、history.go、
│   │           catalog.go、importer.go、cover.go、installer.go、view.go、runs.go、client.go、comfytest/server.go、
│   │           testdata/…、*_test.go                                                     # Task 1、2、4、5、6
│   ├── appdeploy/types.go、spec.go、bundle.go、consts.go、service.go、deployments.go、runner.go、hooks.go、view.go、
│   │             runtime/run.sh、runtime/run_test.go、*_test.go                        # Task 3
│   ├── rental/apphooks.go、batch.go、place.go                                          # Task 3
│   ├── storage/uploads.go（workflow_cover 用途）                                        # Task 4
│   ├── api/server.go、workflows.go、booking.go、apps.go                                # Task 9
│   ├── adminapi/server.go、workflows.go、deployments.go、dashboard.go、permissions_test.go   # Task 10
│   ├── app/app.go、platform/config/config.go、platform/apperr/apperr.go、cmd/platform/{admin,workflow_import}.go   # Task 1、3、4、11
│   ├── app/workflows_e2e_test.go、e2ehooks/workflows.go、e2ehooks/apps.go（fake comfyui 升级）、cmd/e2e-server/main.go   # Task 11
├── web/shared/src/types/workflows.ts、labels.ts                                       # Task 1
├── web/console/src/views/{WorkflowsView,WorkflowDetailView}.vue、components/{WorkflowCard,WorkflowInstallDialog,WorkflowPicker,WorkflowsPanel,QuickRunDrawer,QuickRunForm}.vue、
│                     components/{RentDialog,AppsCard}.vue、logic/workflows.ts、router.ts、layouts/*（导航）、views/HelpView.vue   # Task 12、13
├── web/admin/src/{menu,router}.ts、views/{WorkflowsView,WorkflowDetailView,WorkflowVersionView,WorkflowInstallsView,DeploymentsView,DashboardView}.vue、
│                 components/WorkflowFormDialog.vue、logic/workflowManifest.ts         # Task 14
├── web/e2e/{helpers/workflows.ts,console/workflows.spec.ts,console/market.spec.ts,admin/workflows.spec.ts,admin/instances.spec.ts,coverage.json}   # Task 15
├── README.md（配置表加 SPARK_APP_GIT_MIRROR）                                           # Task 11
└── docs/superpowers/{acceptance-plan11.md,followups-plan11.md}                        # Task 16
```

## 任务依赖

Task 1（迁移 + 骨架 + 错误码 + 前端类型）→ 2（清单校验 / prompt / history 纯逻辑）→ 3（`appdeploy` 扩展点）→ 4（目录服务 + 导入 + 封面）→ 5（安装服务 + 钩子 + 视图）→ 6（快速出图代理 + fake ComfyUI）→ 7（comfyui 1.1.0 脚本）→ 8（六个工作流文件，需在 Spark 容器内核实地址）→ 9（控制台 API）→ 10（后台 API）→ 11（组装 / 配置 / e2e 钩子 / 集成测试）→ 12（控制台市场页 / 安装 / RentDialog）→ 13（控制台实例详情 / 快速出图）→ 14（后台页面）→ 15（Playwright）→ **16 真机验收 + 记录**。按编号顺序执行，共 16 个任务。Task 8 可与 Task 5～7 并行（它主要是在 Spark 上核实与导出文件），但它的 `TestWorkflowsDir` 依赖 Task 2。

---
### Task 1: 迁移 00024、`comfywf` 包骨架、错误码、前端类型与文案

**Files:**
- Create: `migrations/00024_comfy_workflows.sql`、`internal/comfywf/consts.go`、`internal/comfywf/types.go`、`internal/platform/db/migrate_comfy_test.go`、`web/shared/src/types/workflows.ts`、`web/shared/src/__tests__/workflows-labels.spec.ts`
- Modify: `internal/platform/apperr/apperr.go`、`web/shared/src/types/index.ts`、`web/shared/src/labels.ts`

**Interfaces:**
- 表：规格 §2 四张表（`comfy_workflows`、`comfy_workflow_versions`、`deployment_workflows`、`comfy_workflow_runs`）与索引，`+goose Down` 逆序删表。
- `comfywf` 类型（字段 = 列）：`Workflow`、`Version{…; Graph json.RawMessage; APIPrompt json.RawMessage; Manifest Manifest; DownloadGB string; ManifestHash string}`、`Manifest{ComfyUIMin string; Models []ModelSpec; Nodes []NodeSpec; Inputs []InputSpec; Outputs []OutputSpec}`、`ModelSpec{Name, Dir string; SizeGB float64; ModelScope, HF *RepoFile; SHA256 string}`、`RepoFile{Repo, File string}`、`NodeSpec{Name, Git, Commit string; NodeTypes []string}`、`InputSpec{Key, Label, Type, Node, Input string; Default any; Min, Max, Step *float64; MaxLen int; Options []string}`、`OutputSpec{Node string}`、`DeploymentWorkflow{DeploymentID string; WorkflowID, VersionID int64; State string; AddedAt time.Time; InstalledAt *time.Time}`、`Run{ID, DeploymentID string; UserID, VersionID int64; PromptID string; Seed *int64; State string; CreatedAt time.Time; FinishedAt *time.Time}`。
- 常量：Global Constraints 一节；状态常量 `WFStatePending / Installed / Failed`、`RunQueued / RunDone / RunError`、分类列表 `Categories = []string{"txt2img","img2img","video","upscale","other"}`、模型目录白名单 `ModelDirs`、Git 主机白名单 `GitHosts = []string{"github.com","gitee.com","gitcode.com"}`。
- `apperr` 新增项见 Global Constraints。
- 前端 `types/workflows.ts`：`WorkflowCategory`、`WorkflowCatalogItem`、`WorkflowDetail`、`WorkflowManifestPublic`（不含 api_prompt）、`WorkflowInput`、`InstalledWorkflow`、`QuickRun{id, prompt_id, seed, state, files:[{index, filename, kind, url}], message}`；`labels.ts`：`workflowCategoryLabel`、`workflowStateLabel / Type`（待安装 / 已安装 / 安装失败）、`runStateLabel`、`workflowErrorLabel`、`formatGB(n)`（`6.9 GB` / `17 GB`）、`auditActions` 新增 9 项。

- [ ] **Step 1: 迁移往返测试**：仿 `migrate_apps_test.go`：`UpTo(23)`（若 00021～00023 尚不存在则 `UpTo` 到当前最大号）→ 插 users / machines / instances / apps / app_versions / app_deployments → `UpByOne` → 插工作流、版本（`UNIQUE (workflow_id, version)` 重复被拒）、`deployment_workflows`（同部署同工作流重复被拒；`state` 非法被拒）、`comfy_workflow_runs` → `Down` → 旧表仍在、新表不在 → `Up`。
- [ ] **Step 2: 写迁移文件**（规格 §2 原文，头注释写明 Down 不影响容器内文件）。
- [ ] **Step 3: `consts.go` / `types.go` / `apperr`**；`go build ./...`。
- [ ] **Step 4: 前端类型与文案 + Vitest**（`workflowCategoryLabel('video') === '视频'` 等；`formatGB(0.5) === '0.5 GB'`、`formatGB(17) === '17 GB'`）。
- [ ] **Step 5: 测试全绿，提交** `feat(comfywf): migration 00024, package skeleton, error codes, shared types`。

---
### Task 2: 清单校验、内置节点表、`manifest_hash`、容器内 manifest 生成、`BuildPrompt` / `ParseHistory`

**Files:**
- Create: `internal/comfywf/manifest.go`、`builtin_nodes.go`、`hash.go`、`bundle.go`、`prompt.go`、`history.go`、`manifest_test.go`、`prompt_test.go`、`history_test.go`、`testdata/graph_sdxl_ok.json`、`testdata/graph_missing_model.json`、`testdata/graph_custom_node.json`、`testdata/api_sdxl.json`、`testdata/history_{queued,done,error}.json`

**Interfaces:**
- `func ValidateManifest(m *Manifest, graph, apiPrompt json.RawMessage) error`：规范化（去空白、`SizeGB` 两位小数、`Dir` 小写）并按规格 §3.2 全部规则校验；错误为 `apperr.WorkflowVersionInvalid(path, reason)`，`path` 形如 `manifest.models[2].repo`、`graph.nodes[5]`、`manifest.inputs[1].node`。D16 交叉校验：`graphModelRefs(graph) []string`（按 `modelLoaderWidgets = map[string][]int{"CheckpointLoaderSimple": {0}, "UNETLoader": {0}, "VAELoader": {0}, "CLIPLoader": {0}, "DualCLIPLoader": {0, 1}, "TripleCLIPLoader": {0,1,2}, "LoraLoader": {0}, "LoraLoaderModelOnly": {0}, "UpscaleModelLoader": {0}, "ControlNetLoader": {0}, "CLIPVisionLoader": {0}}` 取 `widgets_values[i]` 字符串）每项须在 `models[].name`；`graphCustomTypes(graph)`：`properties.cnr_id` 存在且 ≠ `comfy-core` 或 `properties.aux_id` 存在、或 `type` 不在 `builtinNodeTypes` 的节点，其 `type` 须在某 `nodes[].node_types`。`graph` 形状：对象、`nodes` 数组、`links` 数组、≤ `MaxGraphBytes`；`api_prompt`：对象、每值含 `class_type` 字符串与 `inputs` 对象、≤ `MaxAPIPromptBytes`。
- `builtin_nodes.go`：`var builtinNodeTypes = map[string]bool{...}`，从 ComfyUI v0.3.60 的 `nodes.py` `NODE_CLASS_MAPPINGS` 与 `comfy_extras/*.py` 导出（实施时从 `/opt/ComfyUI` 的源码用一行 python 生成，列表写进文件并注明来源 commit；**不要凭记忆写**：在 Spark 容器里或从 GitHub 该 tag 的源码读）。另加常用前端虚拟节点 `Reroute / Note / MarkdownNote / PrimitiveNode`。
- `func ManifestHash(graph, apiPrompt json.RawMessage, m Manifest) string`：sha256(canonical(graph) + "\n" + canonical(manifest) + "\n" + canonical(api_prompt))，canonical = `json.Unmarshal` 到 `any` 再 `json.Marshal`（Go 的 map 序列化按键排序）。`func DownloadGB(m Manifest) float64`。
- `type BundleWorkflow struct{ Slug, Version string; Graph json.RawMessage; Manifest Manifest }`；`func BundleFiles(ws []BundleWorkflow) ([]appdeploy.BundleFile, error)`：生成 `workflows/manifest.json`（规格 §5.2 形状；模型按 `name` 去重——同名不同来源以先出现的为准；节点按 `name` 去重且 commit 不同 → 错误 `workflow_version_invalid`「节点 X 在两个工作流里 commit 不同」）与 `workflows/<slug>.json`（graph 原文）。输出稳定（按 slug 排序）。
- `func BuildPrompt(apiPrompt json.RawMessage, inputs []InputSpec, values map[string]any, rnd func() int64) (prompt json.RawMessage, seed *int64, err error)`：深拷贝；逐 input：缺值用 `Default`（`seed` 无默认 → 随机；其它无默认 → `WorkflowInputInvalid(label, "必填")`）；类型校验（`int` 整数且区间且 step 对齐（step 有时）；`float` 区间；`text/textarea` 长度与无 NUL；`enum` 在 options；`bool`；`seed` 整数 ≥ 0 或 -1 / 空 → 随机 `[0, 2^53)`；`image` 匹配 `^spark-quickrun/[a-z0-9]{12}\.(png|jpg|webp)$`）；多余 key → 错误；写入 `prompt[node].inputs[input]`。
- `type HistoryResult struct{ State string; Message string; Files []OutputFile }`、`type OutputFile struct{ Index int; Filename, Subfolder, Type, Kind string }`；`func ParseHistory(body []byte, promptID string, outputs []OutputSpec) (HistoryResult, error)`：空对象 / 无该 id → `State = ""`（调用方再查 queue）；`status.completed` → `done`，收集 `outputs[node].{images,gifs,videos}[]`（`Kind`：`images` → image，其余 video；`type` 必须是 `output`，否则跳过），`outputs` 非空时只取声明的节点；`status.status_str == "error"` → `error`，`Message` 取 `status.messages` 里 `execution_error` 项的 `exception_message`（截 500）。body ≤ 1MB 由调用方保证。

- [ ] **Step 1: 测试先行**：`manifest_test.go` 表驱动矩阵（每条规则至少一个反例 + 一个正例，含 D16 三个 fixture）；`ManifestHash` 键序不敏感；`BundleFiles` 快照 + 去重 + commit 冲突；`prompt_test.go` 每种类型、随机种子注入 `rnd`、原 `apiPrompt` 字节不变；`history_test.go` 三个 fixture（从真实 ComfyUI 响应形状手写）。
- [ ] **Step 2: 实现**；`builtin_nodes.go` 的生成方式写在文件头注释（命令行）。
- [ ] **Step 3: 测试全绿，提交** `feat(comfywf): manifest validation, hash, bundle files, prompt builder, history parser`。

---
### Task 3: `appdeploy` 扩展点（`Spec.SupportsWorkflows`、`BundleInput.Extra`、4MiB、`UploadCommand`、`SPARK_GIT_MIRROR`、`Deps` 钩子、`RedeployInTx`、`run.sh install_key`、`PrepareInstall` 带 `workflowIDs`、换机复制集合）

**Files:**
- Modify: `internal/appdeploy/types.go`、`spec.go`、`bundle.go`、`consts.go`、`service.go`、`deployments.go`、`runner.go`、`hooks.go`、`view.go`、`runtime/run.sh`、`runtime/run_test.go`、`bundle_test.go`、`spec_test.go`、`service_test.go`、`runner_test.go`、`hooks_test.go`；`internal/rental/apphooks.go`、`batch.go`、`place.go`、`apphooks_test.go`；`internal/platform/config/config.go`（`AppGitMirror`，env `SPARK_APP_GIT_MIRROR`，默认 `https://gh-proxy.com/`，允许空；非空时必须以 `https://` 开头并以 `/` 结尾）、`config_test.go`

**Interfaces:**
- `Spec.SupportsWorkflows bool json:"supports_workflows,omitempty"`；`ValidateSpec` 不加约束（bool 即可）。`CatalogView` / 控制台 `GET /apps` 的 `version` 带 `supports_workflows`。
- `type BundleFile struct{ Name string; Mode int64; Body []byte }`；`BundleInput.Extra []BundleFile`；`Bundle` 校验 `Name`（`^[a-z0-9][a-z0-9/._-]{0,127}$`，不含 `..` 段，不与固定 5 个文件名重复），追加写入；`MaxBundleBytes = 4 << 20`。
- `UploadCommand(dir)`：`mkdir -p <q> && chmod 700 <q> && rm -rf <q>/workflows && tar -x -C <q> -f - && …`（`<q>/workflows` 同样 `ShellQuote`）。
- `ReservedEnv` 末尾加 `{"SPARK_GIT_MIRROR", in.GitMirror}`；`BundleInput.GitMirror string`；`Deps.GitMirror string`；`bundleFor` 传入。
- `Deps` 新增（都 nil 安全）：
  - `BundleExtra func(ctx context.Context, d Deployment, ver Version) ([]BundleFile, error)` —— `bundleFor` 在 `ver.Spec.SupportsWorkflows && BundleExtra != nil` 时调用，错误包成 `bundleError`（计划 7 对 bundleError 的处理 = `failed(upload_failed)`，消息带原因）。
  - `OnTransition func(ctx context.Context, tx pgx.Tx, d Deployment, to string, now time.Time) error` —— Runner 在回写 `running`（`stepPoll` 的 `StateRunning` 分支，`registerPorts` 之后、`addEvent` 之前）与 `markFailed` 里调用（`markFailed` 已是所有 failed 写入的汇聚点，确认后在那里调一次）。
  - `WorkflowViews func(ctx context.Context, depIDs []string) (map[string][]json.RawMessage, error)` —— `views()` 对 `app_slug == "comfyui"` 或 `spec.SupportsWorkflows` 的部署批量取，`View.Workflows []json.RawMessage json:"workflows"`（非 nil 空切片序列化为 `[]`）。
  - `ResolveWorkflows func(ctx context.Context, q db.Querier, ids []int64) ([]WorkflowRef, error)`；`type WorkflowRef struct{ WorkflowID, VersionID int64 }`。
- `func LockDeployment(ctx, q db.Querier, id string) (Deployment, error)` = 导出现有 `lockDeployment`（内部调用点改名）。
- `func (s *Service) RedeployInTx(ctx context.Context, tx pgx.Tx, inst rental.Instance, d Deployment, req RedeployRequest, now time.Time) (Deployment, error)`：从现 `Redeploy` 闭包体中提出（`actor` 由 `req.UserID / req.IP` 构造）；`Redeploy` 改为 `userOp` 里调它。行为不变：既有 `service_test.go` 全过。
- `rental.AppHooks.PrepareInstall(ctx, q, appID, imageID int64, env map[string]string, workflowIDs []int64)`；`rental.BatchRequest / PlaceRequest` 加 `AppWorkflowIDs []int64`；`batch.go` 两处调用点传入；`AppWorkflowIDs` 非空且 `AppID == nil` → `apperr.BadRequest("预装工作流需要同时选择 ComfyUI 应用")`（与现有 `AppEnv` 无 `AppID` 的检查并列）。`appdeploy.PrepareInstall`：`workflowIDs` 非空时 `!ver.Spec.SupportsWorkflows` → `ErrWorkflowAppUnsupported`；`ResolveWorkflows == nil` → `ErrWorkflowAppUnsupported`；结果放进 `installPlan.Workflows []WorkflowRef`；`QueueInstall` 在插入部署后 `INSERT INTO deployment_workflows (deployment_id, workflow_id, workflow_version_id, state, added_at) VALUES (…, 'pending', now)`，并追加事件「预装工作流 N 个」。
- `OnInstanceMigrated`：复制部署行后 `INSERT INTO deployment_workflows (deployment_id, workflow_id, workflow_version_id, state, added_at) SELECT $new, workflow_id, workflow_version_id, 'pending', $now FROM deployment_workflows WHERE deployment_id = $old`。
- `run.sh`：`install_key() { printf '%s %s %s' "$SPARK_SPEC_HASH" "$(cksum < "$DIR/env" …)" "$(cksum < "$DIR/workflows/manifest.json" 2>/dev/null | awk '{print $1 "-" $2}')"; }`——**注意**：没有 manifest 时第三段为空串，原两段式 `.installed` 内容与新三段式不相等 → 存量部署在升级 platform 后第一次恢复会重跑一次 `install`（幂等、秒过）。接受此行为，在 `followups-plan11.md` 记一句；或者兼容：读到两段式旧值且 manifest 不存在时视为相等（**选兼容**：`_installed` 去掉尾部空格后与 `_key` 去掉尾部空格后比较）。
- `run.sh` `cmd_bg_deploy` 的 install 失败分支（现在是 `fail_exit "${_ph:-install}" 0 "install 失败，退出码 $_rc" 0 "$_rc"`）：先 `_msg=$(sed -n 's/.*"message":"\(disk:[^"]*\)".*/\1/p' "$DIR/status.json")`，非空则用 `_msg` 作为 fail_exit 的消息（规格 D7：`app.sh` 的磁盘预检失败要映射成错误码 `disk`，`failCode` 看的是 message 前缀）。`run_test.go` 加用例：fake `app.sh install` 先 `spark_status download 10 "disk: 剩余 1GiB"` 再 `exit 2` → `status.json.message` 以 `disk:` 开头、`state = failed`。

- [ ] **Step 1: 测试先行**：`bundle_test.go`（Extra 路径非法 / `..` / 重名 / 超 4MiB / 快照含 `workflows/x.json`；`UploadCommand` 快照；`ReservedEnv` 含 `SPARK_GIT_MIRROR`）；`spec_test.go`（`supports_workflows` 往返）；`service_test.go`（`RedeployInTx` 等价、`bundleFor` 调 `BundleExtra` 且其错误变 `upload_failed`）；`runner_test.go`（`OnTransition` 在 running / failed 各被调一次，参数正确）；`hooks_test.go` + `rental/apphooks_test.go`（`PrepareInstall` 带 ids 的三种结果；`QueueInstall` 写集合；换机复制集合）；`run_test.go`（`install_key` 随 manifest 变化；旧两段式 `.installed` 仍被认为匹配）；`config_test.go`（`SPARK_APP_GIT_MIRROR` 默认 / 空 / 非法）。
- [ ] **Step 2: 实现**，保持计划 7 既有测试全绿。
- [ ] **Step 3: `go test ./internal/appdeploy/... ./internal/rental/... ./internal/platform/config/...`，提交** `feat(appdeploy): extension points for workflow bundles (Extra, BundleExtra, OnTransition, RedeployInTx, SPARK_GIT_MIRROR)`。

---
### Task 4: 工作流目录服务（CRUD、版本、发布、封面、`ImportDir`、`platform admin workflow import`）

**Files:**
- Create: `internal/comfywf/catalog.go`、`importer.go`、`cover.go`、`catalog_test.go`、`importer_test.go`、`testdata/import/sdxl-demo/{workflow.json,graph.json,api.json,manifest.json,cover.png}`；`cmd/platform/workflow_import.go`、`workflow_import_test.go`
- Modify: `internal/storage/uploads.go`（`PurposeWorkflowCover = "workflow_cover"`：允许 `userID = 0`，类型 PNG / JPG / WebP（加 `image/webp` → `webp`），上限 `MaxCoverBytes`；`Save` 的「每用户每日 20 次」对 `userID = 0` 不计）、`uploads_test.go`；`cmd/platform/admin.go`（子命令分发 `workflow import <dir>`）

**Interfaces:**
- `type Catalog struct{…}`；`func NewCatalog(pool *pgxpool.Pool, clk clock.Clock, uploads *storage.Service, log *slog.Logger) *Catalog`。
- `WorkflowInput{Slug, Name, Summary, Description, Category string; Tags []string; Sort int; Status string}`；`CreateWorkflow / UpdateWorkflow / DeleteWorkflow / GetWorkflow / WorkflowBySlug / ListAdmin(ctx) ([]WorkflowRow, error)`（带 `Versions int; InstallsLive int; Runs7d int`）、`ListPublished(ctx, category string) ([]CatalogItem, error)`（含最新 published 版本摘要与 `Installs`）、`DetailBySlug`。规范化与校验规则同规格 §2 注释（slug 正则、长度、分类、标签）。审计 `workflow.*`。
- `VersionInput{Version string; Graph, APIPrompt json.RawMessage; Manifest Manifest; Changelog string; CopyFrom *int64; Status string}`；`CreateVersion / UpdateVersion / ValidateVersion(ctx, wfID, in) (ValidateResult{ManifestHash string; DownloadGB float64; Warnings []string}, error) / PublishVersion / GetVersion / ListVersions / LatestPublished(ctx, q, wfID)`；published 的 `UpdateVersion` 只接受 `Changelog` / `Status = retired`，其余 → `ErrWorkflowVersionFrozen`。`PublishVersion` 重新校验、写 `manifest_hash / download_gb / published_at`、审计 `workflow.version.publish{manifest_hash}`；TOTP 在 adminapi 层（Task 10）。
- `SetCover(ctx, actor, wfID int64, r io.Reader) (Workflow, error)`：`uploads.Save(ctx, 0, PurposeWorkflowCover, r)` → 更新 `cover_key`（旧 key 调 `store.Delete`，失败只记日志）；`OpenCover(ctx, slug) (storage.Upload, io.ReadCloser, error)`。
- `ImportDir(ctx, actor, dir) ([]ImportResult, error)`：每个子目录读 `workflow.json`（`{"name","summary","description","category","tags","sort","version","changelog","comfyui_min"}`）、`graph.json`、`api.json`（可缺）、`manifest.json`（`comfyui_min` 来自 workflow.json 时合并）、`cover.(png|jpg|jpeg|webp)`（可缺）；slug 不存在建 draft 工作流 + draft 版本并设封面，存在只加版本（版本号重复 → 错误，继续其它目录）；符号链接跳过（同 `appdeploy/importer.go`）；输出 `slug version created|version-added`。
- `platform admin workflow import <dir>`：仿 `app_import.go`。

- [ ] **Step 1: 测试先行**（嵌入式 PG）：CRUD 与规则、slug 不可改、删除条件、draft 保存也校验、frozen、retired、复制为新版本、`LatestPublished` 按 `published_at`、`ListPublished` 的 `Installs` 计数（手工插 `deployment_workflows` + 部署行）、封面类型 / 大小 / 替换删旧、`ImportDir` 三种结果；`storage` 新用途测试。
- [ ] **Step 2: 实现**。
- [ ] **Step 3: 测试全绿，提交** `feat(comfywf): workflow catalog, versions, publish, cover, directory import`。

---
### Task 5: 安装服务（Add / Update / Remove、状态钩子、视图、`ResolveWorkflows`、`BundleExtra`）

**Files:**
- Create: `internal/comfywf/installer.go`、`view.go`、`installer_test.go`
- Modify: `internal/comfywf/catalog.go`（`LatestPublishedSupported` 等辅助若需要）

**Interfaces:**
- `type Installer struct{…}`；`func NewInstaller(pool, clk, cat *Catalog, apps *appdeploy.Service, appsCat *appdeploy.Catalog, log) *Installer`。
- `type AddRequest struct{ UserID int64; InstanceID, DeploymentID string; WorkflowID int64; VersionID *int64; IP string }`；`Add(ctx, AddRequest) (Result, error)`、`Update(ctx, UpdateRequest{…同 Add})`、`Remove(ctx, RemoveRequest{UserID, InstanceID, DeploymentID string; WorkflowID int64; IP})`；`type Result struct{ Deployment appdeploy.Deployment; Workflows []Installed }`。流程 = 规格 §5.3（`rental.LockOwned` → `appdeploy.LockDeployment` → 校验 → 写集合 → 事件 → `queued` 时不重排，否则 `apps.RedeployInTx(…, RedeployRequest{UserID, InstanceID, DeploymentID, VersionID: targetVer, EnvSet: false, IP}, now)`）。应用识别：`appsCat.GetApp(d.AppID).Slug == "comfyui"`，否则 `ErrWorkflowAppUnsupported`。目标应用版本：当前版本 `Spec.SupportsWorkflows` → `nil`（不换）；否则 `appsCat.ListVersions` 中 `published` 且 `SupportsWorkflows` 的 `published_at` 最新者 → 其 ID；无 → `ErrWorkflowAppUnsupported`。
- `ListInstalled(ctx, depID) ([]Installed, error)`；`type Installed struct{ WorkflowID int64 json:"workflow_id"; Slug, Name, Category string; VersionID int64; Version string; State string; LatestVersionID *int64; LatestVersion string; UpdateAvailable bool; QuickRun bool; AddedAt time.Time; InstalledAt *time.Time }`。
- 钩子实现（接到 `appdeploy.Deps`，在 Task 11 组装）：
  - `BundleExtra(ctx, d, ver)`：`SELECT dw.workflow_id, w.slug, v.version, v.graph, v.manifest FROM deployment_workflows dw JOIN … WHERE deployment_id = $1 ORDER BY w.slug` → `BundleFiles(...)`；集合为空返回 `nil, nil`（包里不带 workflows 目录；`UploadCommand` 的 `rm -rf` 保证旧目录清掉）。
  - `OnTransition(ctx, tx, d, to, now)`：`to == running` → `UPDATE deployment_workflows SET state='installed', installed_at=$2 WHERE deployment_id=$1 AND state='pending'`；`to == failed` → `pending → failed`；其它不动。
  - `WorkflowViews(ctx, depIDs)`：批量 `ListInstalled` 序列化为 `json.RawMessage`。
  - `ResolveWorkflows(ctx, q, ids)`：去重、≤ 8（否则 `ErrWorkflowLimit`）、每个 published 且有 published 版本（否则 `ErrWorkflowNotFound` / `ErrWorkflowUnavailable`）→ `[]appdeploy.WorkflowRef`。

- [ ] **Step 1: 测试先行**（嵌入式 PG，`appdeploy` 用计划 7 测试里的 `newService` 风格搭起来，部署行直接 SQL 插）：允许状态矩阵（queued 只写集合、running / stopped / failed 变 queued 且 `row_version + 1`、installing / removing → `app_invalid_state`）、`workflow_exists / limit / not_found / unavailable`、非 comfyui 部署 → `app_unsupported`、1.0.0（`SupportsWorkflows=false`）部署 Add 后 `app_version_id` 变为支持的最新版且 `spec_hash` 变、事件文案、审计三种动作、`Update` 换版本、`Remove` 对 queued 只删行、`OnTransition` 两条、`BundleExtra` 输出含 `workflows/manifest.json` + 两个 json 且集合空时 nil、`ResolveWorkflows` 四种结果、`ListInstalled` 的 `UpdateAvailable / QuickRun`。
- [ ] **Step 2: 实现**。
- [ ] **Step 3: 测试全绿，提交** `feat(comfywf): installer (add/update/remove), transition hook, installed views`。

---
### Task 6: 快速出图代理（客户端、上传、运行、查询、文件）与 fake ComfyUI 测试服务器

**Files:**
- Create: `internal/comfywf/client.go`、`runs.go`、`runs_test.go`、`comfytest/server.go`（导出，供 e2e-server 与集成测试复用）

**Interfaces:**
- `type Client struct{ up *upstream.Pool }`；`func (c *Client) do(ctx, t upstream.Target, port int, req *http.Request, limit int64) (*http.Response, error)`：`http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(...) { return c.up.Dial(ctx, t, port) }}}`，超时由 ctx；`*upstream.DialError` → `apperr.ErrUpstreamUnreachable`（**新增**：`New("upstream_unreachable", "无法连接实例内的 ComfyUI", 503)`；`apperr` 目前没有这个码，ingress 用的是自己的 502 页）；响应体用 `io.LimitReader(limit+1)` 读，超出 → 413。
- `type Runs struct{…}`；`func NewRuns(pool, clk, box?, cat *Catalog, apps *appdeploy.Service, client *Client, log) *Runs`。
  - `Upload(ctx, UploadRequest{UserID; InstanceID, DeploymentID string; WorkflowID int64; Body io.Reader; Size int64}) (UploadResult{Name string}, error)`：部署归属与 running 校验（`apps.GetForUser` + 实例 running/非暂停，否则 `ErrAppInvalidState`）；读 ≤ 16MB，`http.DetectContentType` ∈ png / jpeg / webp（否则 `storage.ErrUnsupportedType` 同款 415）；文件名 `randx` 12 位 + 扩展；multipart 字段 `image` + `subfolder=spark-quickrun` + `overwrite=true` → `POST /upload/image` → 返回 `spark-quickrun/<name>`（以 ComfyUI 返回的 `name` 为准校验与白名单正则一致）。
  - `Start(ctx, StartRequest{UserID; InstanceID, DeploymentID string; WorkflowID int64; Inputs map[string]any; IP}) (RunView, error)`：归属 / running；`deployment_workflows` 里该工作流 `installed`（否则 `ErrAppInvalidState`「工作流尚未安装完成」）；版本 `APIPrompt` 非空（否则 `apperr.BadRequest("该工作流不支持快速出图")`）；在飞计数 `SELECT count(*) FROM comfy_workflow_runs WHERE deployment_id=$1 AND state='queued' AND created_at > $now - 30m` ≥ 2 → `ErrWorkflowBusy`；`BuildPrompt`；`POST /prompt {"prompt":…, "client_id": run_id}`；400 → 解析 `node_errors`（取第一项 `errors[0].message` 或 `error.message`）→ `WorkflowPromptRejected`；200 → `INSERT comfy_workflow_runs`。
  - `Get(ctx, GetRequest{UserID; InstanceID, DeploymentID, RunID string}) (RunView, error)`：归属（run.user_id、deployment_id）→ 若 `state != queued` 直接返回缓存的终态（`files` 需要重新从 history 取：终态仍查一次 history，失败回落只给 state）；`GET /history/{prompt_id}` → `ParseHistory`；`State == ""` → `GET /queue`，`queue_running` 里有 `prompt_id`（元素 `[number, prompt_id, …]`）→ `running`，否则 `queued`；`done / error` → 更新行 `state / finished_at`。`RunView{ID, PromptID string; Seed *int64; State string; Message string; Files []FileView{Index int; Filename, Kind, URL string}; CreatedAt, FinishedAt}`，`URL` 由调用方（api 层）按路由拼。
  - `OpenFile(ctx, GetRequest, index int) (*http.Response, error)`：先 `Get` 拿 `OutputFile`（`index` 越界 404）→ `GET /view?filename=&subfolder=&type=output`（`url.Values` 编码）→ 返回响应（调用方透传 `Content-Type / Content-Length`，`io.Copy` 带 `MaxViewBytes` 上限：先看 `Content-Length` > 上限 → 413，没有长度则 `LimitReader`）。
- `comfytest.Server`：`httptest.Server` 实现 `/system_stats`（200 `{}`）、`/prompt`（记录 `Prompts []json.RawMessage`，可配置返回 400 的 `node_errors`，返回 `prompt_id = uuid-ish`）、`/history/{id}`（按 `SetHistory(id, HistoryState)`：`Queued / Running / Done{Files} / Error{Msg}` 生成真实形状 JSON）、`/queue`（Running 的 id 放 `queue_running`）、`/view`（返回 1×1 PNG 字节或配置的大 body，`Content-Type: image/png`）、`/upload/image`（返回 `{name, subfolder:"spark-quickrun", type:"input"}`，记录收到的文件名与大小）。线程安全。

- [ ] **Step 1: 测试先行**：用 `sshtest`（计划 7 的 `SetTarget` 把 8188 指到 `comfytest.Server`）或直接把 `Client` 的拨号函数注入（加 `Client.dial` 可替换字段，测试里直连 httptest）——**选后者**（更快，不起 sshd）：`NewClientWithDialer(func(ctx, t, port) (net.Conn, error))`。用例：Upload 类型 / 大小 / 返回名；Start 的 busy、未安装、无 api_prompt、`node_errors` → 422、成功写行且 prompt 里 inputs 正确、seed 随机；Get 三种状态与终态更新；OpenFile 透传与 413、index 越界；`DialError` → 503；非主人 404。
- [ ] **Step 2: 实现**。
- [ ] **Step 3: 测试全绿，提交** `feat(comfywf): quick-run proxy (upload/prompt/history/view) and fake ComfyUI test server`。

---
### Task 7: `deploy/apps/comfyui` 1.1.0（清单安装、`extra_model_paths.yaml`、数据盘目录）

**Files:**
- Modify: `deploy/apps/comfyui/app.json`（`version: 1.1.0`、changelog）、`spec.json`（`supports_workflows: true`，`MODEL_ID.options` 前加 `none` 且 `default: none`，`help` 补一句「选 none 只装工作流清单里的模型」）、`app.sh`
- Create: `deploy/apps/comfyui/app_test.sh`？——**不**：测试放 Go：`internal/appdeploy/apps_comfyui_test.go`（用 `testdata/fakebin/{modelscope,hf,git,pip,df}` 假命令 + 临时目录当 `/root/data`，`sh app.sh install/start` 真跑；仿 `runtime/run_test.go` 的做法）
- Modify: `deploy/apps/README.md`（1.1.0 的目录约定、`supports_workflows`、manifest 行格式）

**Interfaces（`app.sh` 1.1.0）：**
- 顶部沿用 1.0.0（`set -eu`、HF_ENDPOINT 处理、TQDM 关闭）。目录：`CDATA=$SPARK_DATA_DIR/comfyui`，`mkdir -p $CDATA/{models,custom_nodes,user,input} $SPARK_DATA_DIR/comfyui-output`。
- `install`：
  1. `MODEL_ID != none` → 1.0.0 的预置模型逻辑，`link_models` 目标改为 `$CDATA/models/<sub>/`。
  2. `[ -f "$SPARK_APP_DIR/workflows/manifest.json" ]` → `wf_install`：
     - `wf_lines() { python3 - "$SPARK_APP_DIR/workflows/manifest.json" <<'PY' … PY }`：输出 `model name dir size_gb sha256|- ms_repo|- ms_file|- hf_repo|- hf_file|-`（按 name 去重）、`node name git commit`（去重）、`wf slug file`。
     - 模型循环：`[ -e "$CDATA/models/$dir/$name" ] && { log "模型 $name 已存在，跳过"; continue; }`；选源：`MODEL_SOURCE=hf-mirror` 且 `hf_repo != -` → hf，否则 ms（都有则按 MODEL_SOURCE；只有一个用那个）；`free_gib`（同 run.sh 的 `df -Pk` 写法，可被 `SPARK_TEST_DF_FREE_GIB` 覆盖）`< size_gb + 5` → `printf 'disk: 数据盘剩余 %sGiB，模型 %s 需要约 %sGiB\n' >&2; exit 2`（run.sh 把 `disk:` 前缀映射为 `disk`——**注意** run.sh 的 `failCode` 看的是 `status.json.message`：所以在 exit 前先 `spark_status download <pct> "disk: …"`，让 message 带前缀）；下载到 `$SPARK_MODELS_DIR/<ms|hf>/<repo>/<file>`（`fetch` 同 1.0.0，`modelscope download --model <repo> <file> --local_dir <dir>` / `hf download <repo> <file> --local-dir <dir>`）；`sha256 != -` → `sha256sum` 比对，不符删文件退出 1；`ln -sfn <下载路径> "$CDATA/models/$dir/$name"`；进度 `spark_status download $((5 + i*65/n)) "下载模型 $name（$i/$n，约 ${size}GB）"` + 1.0.0 的 `progress_loop`（对当前仓库目录）。
     - 节点循环：`[ "$(cat "$CDATA/custom_nodes/$name/.spark-commit" 2>/dev/null)" = "$commit" ] && continue`；`tmp=$CDATA/custom_nodes/.tmp-$name; rm -rf "$tmp"`；`git clone --filter=blob:none "${SPARK_GIT_MIRROR:-}$git" "$tmp" || git clone --filter=blob:none "$git" "$tmp"`；`git -C "$tmp" checkout --quiet "$commit"`；`[ -f "$tmp/requirements.txt" ] && pip install -r "$tmp/requirements.txt"`；`rm -rf "$CDATA/custom_nodes/$name" && mv "$tmp" "$CDATA/custom_nodes/$name" && printf '%s\n' "$commit" > "$CDATA/custom_nodes/$name/.spark-commit"`；`spark_status install $((70 + j*8/m)) "安装节点 $name"`。
     - 工作流文件：`wfdir=$CDATA/user/default/workflows; mkdir -p`；对每个 `wf`：`cp "$SPARK_APP_DIR/workflows/$file" "$wfdir/spark-$slug.json"`；然后 `for f in "$wfdir"/spark-*.json` 不在清单 slug 集合里的 `rm -f`。
     - `spark_status install 79 "工作流就绪：N 个"`。
  3. 没有 manifest 时也执行第 2 步的「删多余 `spark-*.json`」（集合被清空的情形）。
- `start`：写 `$SPARK_APP_DIR/extra_model_paths.yaml`（规格 §5.2 的 YAML，`base_path: $CDATA/`）；`link_models`（若 `MODEL_ID != none`）；`cd /opt/ComfyUI && python3 main.py --listen 127.0.0.1 --port "$SPARK_PORT" --extra-model-paths-config "$SPARK_APP_DIR/extra_model_paths.yaml" --user-directory "$CDATA/user" --input-directory "$CDATA/input" --output-directory "$SPARK_DATA_DIR/comfyui-output" </dev/null >>"$SPARK_APP_DIR/comfyui.log" 2>&1 &`；旧 `comfyui.log` 先轮转为 `.1`（计划 7 O-3 的做法）；写 `service.pid`。
- 所有变量引号包裹；`read -r` 读行；不用 bash 语法（`TestAppsDir` 的 POSIX 解析会拦）。

- [ ] **Step 1: 测试先行**（`internal/appdeploy/apps_comfyui_test.go`，`//go:build !windows`）：假命令记录调用到 `$SPARK_TEST_ROOT/calls.log`；`modelscope / hf` 假命令按参数 `touch` 目标文件；`git clone` 假命令 `mkdir -p <dst>/.git` 并按环境 `FAKE_GIT_FAIL_MIRROR=1` 时对含 `gh-proxy` 的 URL 失败；`git checkout` 对 `FAKE_GIT_BAD_COMMIT` 失败；`pip` 记录。用例：两个工作流共用模型只下一次；已存在跳过；`MODEL_SOURCE=hf-mirror` 但只有 ms 来源 → 用 modelscope；镜像失败回退直连（calls.log 两次 clone）；checkout 失败非零；`SPARK_TEST_DF_FREE_GIB=1` → 非零且 status.json message 以 `disk:` 开头；多余 `spark-x.json` 被删、`my.json` 保留；`extra_model_paths.yaml` 快照；`start` 命令行含三个目录参数（假 `python3`? ——`start` 测试用 `PATH` 前置的假 `python3` 记录参数并 `sleep`，然后杀掉）。`TestAppsDir` 继续通过（脚本引用的大写变量都在 spec / 保留变量 / 脚本内赋值里：`apps_dir_test.go` 的 `allowedVar` 按 `SPARK_` / `HF_` 前缀放行，`SPARK_GIT_MIRROR` 不需要改名单；脚本里自己赋值的 `CDATA` 等小写 / 已赋值变量也不需要）。
- [ ] **Step 2: 实现 `app.sh` / `spec.json` / `app.json` / README**。
- [ ] **Step 3: 测试全绿，提交** `feat(apps): comfyui 1.1.0 installs workflow manifests, data-disk model/node/user dirs`。

---
### Task 8: 六个工作流目录（`deploy/workflows/*`）、README、`TestWorkflowsDir`

**Files:**
- Create: `deploy/workflows/README.md`、`deploy/workflows/{sdxl-txt2img,sdxl-img2img,flux-schnell-txt2img,qwen-image-txt2img,wan22-5b-ti2v,sdxl-ultimate-upscale}/{workflow.json,graph.json,api.json,manifest.json,cover.png}`、`internal/comfywf/workflows_dir_test.go`

**步骤说明：** 这是唯一在实施期间要用到 Spark 的任务，且只做**只读核实与导出**：
- 在 221 上用现有平台开一台 ComfyUI 实例（镜像 `spark/comfyui:25.09`，应用 comfyui 1.0.0 或 1.1.0，`MODEL_ID=none` 若 1.1.0 已导入），SSH 进容器：
  - 核实 ModelScope 文件存在（不下载）：`modelscope download --model <repo> <file> --local_dir /tmp/x --dry-run` 若无 `--dry-run`，用 `python3 -c "from modelscope.hub.api import HubApi; print([f['Path'] for f in HubApi().get_model_files('<repo>', recursive=True)])"` 列目录；hf-mirror 用 `curl -sI https://hf-mirror.com/<repo>/resolve/main/<file>` 看 200/302 与 `Content-Length`（填 `size_gb`）。
  - 在容器里 `git ls-remote https://gh-proxy.com/https://github.com/ssitu/ComfyUI_UltimateSDUpscale.git HEAD` 取 commit；用 `python3` 读该仓库 `nodes.py` 的 `NODE_CLASS_MAPPINGS` 键（可 `git clone --depth 1` 到 `/tmp`，这是 KB 级代码不是模型）→ `node_types`。
  - 从 `/opt/ComfyUI` 导出内置节点列表（Task 2 的 `builtin_nodes.go` 若还没生成，在这里生成：`python3 -c "import sys; sys.path.insert(0,'/opt/ComfyUI'); import nodes; nodes.init_extra_nodes(); print('\n'.join(sorted(nodes.NODE_CLASS_MAPPINGS)))"`，无 GPU 依赖；若 import 需要 torch 则在该容器里本来就有）。
  - 启动该实例的 ComfyUI（不下模型也能起），在浏览器里打开官方模板（`default`、`image2image`、`flux_schnell`、`qwen_image`、`wan2.2_ti2v_5B`），把模型文件名改成清单里的 `name`，分别「Save」与「Save (API Format)」导出 → `graph.json` / `api.json`；放大修复的图手工搭（LoadImage → UpscaleModelLoader + UltimateSDUpscale → SaveImage）。文件拷回仓库（KB 级）。
  - 结束后释放该实例。
- 封面：用占位图（例如用 ImageMagick / python PIL 在 Mac 上生成 512×288 带文字的 PNG，≤ 200KB）；不要求真实出图（真机验收后可替换）。
- `workflow.json` 字段表写进 README（规格 §9）。

**Interfaces:**
- `TestWorkflowsDir`：遍历 `deploy/workflows/*/`：四个必需文件存在；`workflow.json` 字段合法（slug = 目录名、category、tags、version）；`manifest.json` 过 `ValidateManifest(graph, api)`（含 D16）；`api.json` 存在时 `inputs` 非空且每个 `inputs[].node/input` 命中；`graph.json` 的 `nodes` ≥ 3；封面存在时为 PNG/JPG/WebP 且 ≤ 2MB；`download_gb` 与规格表大致相符（只断言 > 0）。

- [ ] **Step 1: 写 `TestWorkflowsDir`（先对空目录失败）**。
- [ ] **Step 2: 在 Spark 实例容器内核实地址、导出 JSON、钉 commit；写六个目录与 README**。核实结果（每个模型的实际 repo / file / 大小、节点 commit、不可用的来源）记在 README 末尾的「核实记录（2026-10-xx，221）」。
- [ ] **Step 3: 测试全绿，提交** `feat(workflows): six initial ComfyUI workflows (sdxl/flux/qwen-image/wan2.2/upscale) with manifests`。

---
### Task 9: 控制台 API（§10.1）+ 订单 `app_workflow_ids`

**Files:**
- Create: `internal/api/workflows.go`、`workflows_test.go`
- Modify: `internal/api/server.go`（`Workflows *comfywf.Catalog; Installer *comfywf.Installer; Runs *comfywf.Runs`，三者 nil 时不注册路由；路由见下）、`booking.go`（`AppWorkflowIDs []int64 json:"app_workflow_ids"` 透传到 `rental.BatchRequest / PlaceRequest`）、`apps.go`（写操作限流器复用于 workflows 写接口；新增 `runsLim`：每用户每小时 120）、`booking_test.go`

**Interfaces（路由）：**
- 组外匿名（IP 限流同 `/apps`）：`GET /workflows`（`?category=`）、`GET /workflows/{slug}`、`GET /workflows/{slug}/cover`（`Cache-Control: public, max-age=86400`，`ETag = cover_key`）。
- `requireUser` 组内：`GET /instances/{id}/apps/{dep}/workflows`；`POST …/workflows`（`{workflow_id, version_id?}` → 201 `{deployment: View, workflows: []}`）；`PUT …/workflows/{wid}`；`DELETE …/workflows/{wid}`；`POST …/workflows/{wid}/uploads`（multipart `file`，`r.Body = http.MaxBytesReader(16MB+1KB)`）；`POST …/workflows/{wid}/runs`；`GET …/workflows/{wid}/runs/{run}`；`GET …/workflows/{wid}/runs/{run}/files/{index}`。
- 视图：`comfywf.CatalogItem` 直接 JSON（字段 §10.1）；`cover_url` = `/api/v1/workflows/<slug>/cover`（有 `cover_key` 时）；详情 `version.manifest` 用 `ManifestPublic`（不含 api_prompt）。`RunView.Files[].URL` = `/api/v1/instances/{id}/apps/{dep}/workflows/{wid}/runs/{run}/files/{index}`。
- 错误：`writeError` 已按 `apperr` 输出；`Runs.OpenFile` 的响应用 `io.CopyN`。

- [ ] **Step 1: 测试先行**（`api_test.go` 的测试服务器 + Task 6 的 `comfytest` + `NewClientWithDialer`）：路由与匿名限流；详情不含 `api_prompt` 字段（JSON 字符串里 `grep` 不到 `class_type`）；订单 / 预订体 `app_workflow_ids` 到达 `PrepareInstall`（用 fake hooks 断言）、无 `app_id` 时 400；安装三接口对非主人 404、写限流 61 次 → 429；runs 四接口全流程（上传 → 运行 → 轮询 → 文件字节透传、`Content-Type`）；运行限流 121 次 → 429。
- [ ] **Step 2: 实现**。
- [ ] **Step 3: 测试全绿，提交** `feat(api): workflow market, install, quick-run endpoints; app_workflow_ids on orders`。

---
### Task 10: 后台 API（§10.2）+ 角色矩阵 + 审计

**Files:**
- Create: `internal/adminapi/workflows.go`、`workflows_test.go`
- Modify: `internal/adminapi/server.go`（`Workflows *comfywf.Catalog; Installer *comfywf.Installer` 字段与路由）、`deployments.go`（`GET /deployments/{dep}` 响应加 `workflows`）、`dashboard.go`（`workflow_installs`、`workflow_runs_24h`）、`permissions_test.go`、`dashboard_test.go`

**Interfaces：** 规格 §10.2 表。`publish` 走既有 `s.withStepUp(r, req.TOTP, func() error {…})`（`adminapi/apps.go` 第 303 行一带的用法，请求体 `{"totp": "..."}`）；`validate=1` 的 POST / PUT 只调 `ValidateVersion / ValidateVersionUpdate` 返回 `{ok:true, manifest_hash, download_gb, warnings}`；封面 `POST /workflows/{id}/cover` multipart `file` ≤ 2MB；`GET /workflows/{id}/installs?page=` 用 `paging.Page`，返回 `{items:[{deployment_id, instance_id, user_id, phone?, version, state, added_at, installed_at}], total}`。角色：读 all；写 super；无 ops 专属操作（强制移除工作流不做：ops 用计划 7 的强制卸载应用即可）。

- [ ] **Step 1: 测试先行**：角色矩阵加入 `permissions_test.go` 的表；CRUD / 版本 / validate / publish 需 TOTP / frozen / 封面 / installs 分页 / 部署详情含 workflows / 仪表盘两个数字。
- [ ] **Step 2: 实现**。
- [ ] **Step 3: 测试全绿，提交** `feat(adminapi): workflow catalog, versions, publish (TOTP), cover, installs, dashboard counters`。

---
### Task 11: `app.go` 组装、配置、README；e2e 钩子与 fake ComfyUI；集成测试

**Files:**
- Modify: `internal/app/app.go`（构造 `comfywf.Catalog / Installer / Runs`，接 `appdeploy.Deps.{BundleExtra, OnTransition, WorkflowViews, ResolveWorkflows, GitMirror}`，注入 api / adminapi；`admin` 子命令 `workflow import`）、`internal/platform/config/config.go`（已在 Task 3）、`README.md`（配置表 `SPARK_APP_GIT_MIRROR`；部署注意：计划 7 合并后导入 comfyui 1.1.0 与 `deploy/workflows`）、`cmd/e2e-server/main.go`、`internal/e2ehooks/apps.go`（fake comfyui 版本 `spec.supports_workflows = true`，fake `app.sh install` 读 manifest：对每个 `wf` 复制到 `$SPARK_TEST_ROOT/comfyui/user/default/workflows/spark-<slug>.json`，每个模型 `touch models/<dir>/<name>`（名字含 `fail-disk` 时输出 `disk:` 并退出 2），每个节点 `mkdir custom_nodes/<name>` + `.spark-commit`）、`internal/e2ehooks/hooks.go`
- Create: `internal/e2ehooks/workflows.go`（`POST /hooks/workflows/seed` → 两个 fake 工作流 A（无节点、有封面、api_prompt + inputs：prompt/steps/seed）与 B（一个节点 `FakeNode` commit 全 `a`、`node_types: ["FakeNodeType"]`，graph 里有该自定义节点）返回 `{workflows:[{id, slug, version_id}]}`，幂等；`POST /hooks/comfy/history {prompt_id|run_id, state: queued|running|done|error, files?: n}` 控制 `comfytest.Server`；e2e-server 把 `comfytest` 的 handler 挂到假容器的 8188 上：`Upstream.ServeHTTP(8188, handler)`（`sshtest.Server.ServeHTTP(port, h) (stop func())` 已存在，`/hooks/ingress-upstream` 就是用它；所有实例共用一台假容器，所以 8188 对所有实例都指到同一个 fake ComfyUI））、`internal/app/workflows_e2e_test.go`（集成测试，规格 §16.2 全链路 + 失败链路 + 快速出图 + 一致性检查）

- [ ] **Step 1: 集成测试先行**（仿 `internal/app/apps_e2e_test.go`）：按规格 §16.2 顺序写断言（集合状态、假容器目录文件、部署包内容可经 `sshtest` 的根目录直接检查 `workflows/manifest.json`、`skip install` 日志、换机复制、失败链路、快速出图三次第三次 429）。
- [ ] **Step 2: 组装与钩子实现**；`SPARK_APP_RUNNER=off` 下 e2e-server 由 `/hooks/apps/tick` 推进。
- [ ] **Step 3: `make test` 全绿（含既有 e2e 集成），提交** `feat(app): wire comfywf, e2e hooks for workflows and fake ComfyUI, integration test`。

---
### Task 12: 控制台：工作流市场页、详情页、安装弹窗、`RentDialog` 预装工作流、导航、帮助页

**Files:**
- Create: `web/console/src/views/WorkflowsView.vue`、`WorkflowDetailView.vue`、`components/WorkflowCard.vue`、`WorkflowInstallDialog.vue`、`WorkflowPicker.vue`、`logic/workflows.ts`、`logic/__tests__/workflows.spec.ts`
- Modify: `router.ts`（`/workflows`、`/workflows/:slug`，匿名可看）、`layouts/ConsoleLayout.vue`（顶栏导航在「算力市场」后加「工作流市场」）、`components/RentDialog.vue`（选 ComfyUI 应用时显示「预装工作流」多选（`WorkflowPicker` 内嵌模式），`app_workflow_ids` 进请求体与幂等键派生参数；支持 `props.presetWorkflowId`）、`logic/rent.ts`（幂等键派生加 `appWorkflowIds`）、`views/MarketView.vue`（无改动或只加入口链接）、`views/HelpView.vue`、`api.ts`（请求函数）、`components.d.ts`

**Interfaces（`logic/workflows.ts` 纯函数，Vitest 覆盖）：**
- `filterWorkflows(items, {category, query})`、`sortWorkflows(items)`（`sort` 升序、再 `name`）、`totalDownloadGB(items, selectedIds)`、`installTargets(instances) → [{instanceId, instanceName, deploymentId, state, installedCount}]`（从 `apps_summary` 里挑 `comfyui`——`apps_summary` 只有 `id/name/state`，没有 slug：**改用** `GET /instances/{id}/apps` 的 `app_slug`；弹窗打开时对用户实例并行请求，≤ 20 台）、`canInstallTo(target) → {ok, reason}`（state ∈ queued/running/stopped/failed 且 installedCount < 8）、`installConfirmText(workflow)`（含「重启 ComfyUI」与下载量）、`coverOrPlaceholder(item)`。
- `WorkflowInstallDialog`：props `workflow`；列目标；「安装」→ `POST …/workflows` → 成功跳转实例详情并高亮工作流子区块；「新开一台实例」→ `emit('rent', workflow.id)` → 父页面打开 `RentDialog`（`presetAppSlug = 'comfyui'`、`presetEnv = {MODEL_ID: 'none'}`、`presetWorkflowId`）。未登录点「一键安装」→ 跳 `/login?redirect=`。

- [ ] **Step 1: Vitest 先行**（纯函数全部用例 + `rent.ts` 幂等键含 `appWorkflowIds`）。
- [ ] **Step 2: 页面与组件**；`data-testid`：`wf-card-<slug>`、`wf-category-<c>`、`wf-install-btn`、`wf-install-dialog`、`wf-target-<dep>`、`wf-rent-new`、`rent-workflows`、`rent-confirm-workflows`。
- [ ] **Step 3: `npm test`、`npm run lint`、`npm run build` 通过，提交** `feat(console): workflow market, detail, install dialog, preinstall workflows in RentDialog`。

---
### Task 13: 控制台：实例详情工作流子区块、快速出图抽屉

**Files:**
- Create: `web/console/src/components/WorkflowsPanel.vue`、`QuickRunDrawer.vue`、`QuickRunForm.vue`、`logic/quickrun.ts`、`logic/__tests__/quickrun.spec.ts`
- Modify: `components/AppsCard.vue`（`app_slug === 'comfyui'` 的行下方渲染 `WorkflowsPanel`，传 `deployment`；部署 installing 时面板显示「正在安装工作流…」）、`logic/apps.ts`（视图类型加 `workflows`）、`api.ts`

**Interfaces（`logic/quickrun.ts`）：**
- `defaultValues(inputs) → Record<string, any>`（seed 为空 = 随机）、`validateValues(inputs, values) → {ok, errors: {key: msg}}`（与后端规则一致：区间、step、长度、enum、image 必填（有 image 输入且无默认时））、`runStatusText(run) → '排队中' | '生成中' | '完成（12 秒）' | '失败：…'`、`pollDelayMs = 2000`、`pollDeadlineMs = 30 * 60 * 1000`、`fileKind(file)`。
- `WorkflowsPanel`：每行：名称、`v<版本>`、`有新版本` 标签、状态标签（`workflowStateLabel`）、按钮「打开 ComfyUI」（`deployment.url`）、「快速出图」（`quick_run && deployment.state === 'running'`）、更多菜单：更新（确认文案：重启 + 新版本 changelog）、移除（确认：「移除后模型与节点保留在 /root/data/comfyui」）；底部「添加工作流」→ `WorkflowPicker`（灰显已装；选中 → 确认文案 → `POST`）；`workflows.length >= 8` 时隐藏按钮并显示 `wf-limit` 文案；安装 / 更新 / 移除成功后 `emit('refresh')` 让 `AppsCard` 重新拉部署列表（部署会变 queued → 进度条沿用计划 7 的渲染）。
- `QuickRunDrawer`：左表单（`QuickRunForm` 按 `inputs` 渲染，`image` 用 `el-upload` 自定义 `http-request` 调 `…/uploads`，预览缩略图）；「运行」→ `POST …/runs` → 轮询 `GET …/runs/{id}`（2 秒，30 分钟上限）→ `files` 渲染 `<img>` / `<video controls>`（`url` 直接作为 src，同源带 Cookie）、每张下方「下载」（`<a download>`）；显示「本次种子：N」；错误用后端 message；`workflow_busy` 提示稍后再试。
- `data-testid`：`wf-panel`、`wf-row-<slug>`、`wf-state-<slug>`、`wf-quickrun-<slug>`、`wf-update-<slug>`、`wf-remove-<slug>`、`wf-add`、`wf-limit`、`qr-drawer`、`qr-input-<key>`、`qr-run`、`qr-status`、`qr-file-<n>`、`qr-seed`。

- [ ] **Step 1: Vitest 先行**（`quickrun.ts` 全部；`apps.ts` 视图解析含 `workflows`）。
- [ ] **Step 2: 组件实现**。
- [ ] **Step 3: `npm test` / lint / build，提交** `feat(console): installed workflows panel and quick-run drawer`。

---
### Task 14: 后台：工作流列表 / 详情 / 版本编辑页 / 安装列表、部署详情、仪表盘

**Files:**
- Create: `web/admin/src/views/WorkflowsView.vue`、`WorkflowDetailView.vue`、`WorkflowVersionView.vue`、`WorkflowInstallsView.vue`、`components/WorkflowFormDialog.vue`、`logic/workflowManifest.ts`、`logic/__tests__/workflowManifest.spec.ts`
- Modify: `menu.ts`、`router.ts`（`/workflows`、`/workflows/:id`、`/workflows/:id/versions/:vid`、`/workflows/:id/installs`，all 角色）、`__tests__/menu.spec.ts`、`views/DeploymentsView.vue`（抽屉加「工作流」表）、`views/DashboardView.vue`、`web/shared/src/types/admin.ts`

**Interfaces（`logic/workflowManifest.ts`）：** 结构化表单 ↔ JSON 的互转（`manifestToForm / formToManifest`）、前端即时校验（同 §3.2 正则，返回字段路径）、`parseGraphFile(text) → {ok, nodes: n, bytes}`、`parseApiFile(text) → {ok, nodes: [{id, class_type, inputs: string[]}]}`（供 inputs 表的 node / input 下拉选择：只列非连线的 input）、`downloadGB(models)`。
- `WorkflowVersionView`：左：版本号、changelog、`graph.json` 文件框（粘贴 / `<input type=file>` 读文本，显示节点数与大小）、`api.json` 文件框；中：manifest 结构化表单（模型表、节点表、输入表（node / input 下拉来自 api.json 解析）、输出表）+「查看 JSON」；右上：「校验」（`?validate=1`，显示错误（字段路径）与 warnings）、「保存」、「发布」（TOTP 弹窗显示 `download_gb`）。published 只读（changelog / 退役除外）。
- `WorkflowsView`：表格 + 新建 / 编辑弹窗（含封面上传 `el-upload`，预览）。
- 仪表盘：两个新数字卡。

- [ ] **Step 1: Vitest 先行**（`workflowManifest.ts`、`menu.spec.ts` 表）。
- [ ] **Step 2: 页面实现**；`data-testid` 前缀 `adm-wf-`。
- [ ] **Step 3: `npm test` / lint / build，提交** `feat(admin): workflow catalog pages, version editor with manifest form, installs, dashboard`。

---
### Task 15: Playwright 用例与 `coverage.json`

**Files:**
- Create: `web/e2e/helpers/workflows.ts`（`seedWorkflows()`、`setComfyHistory()`、`installWorkflowViaApi()`）、`web/e2e/console/workflows.spec.ts`、`web/e2e/admin/workflows.spec.ts`
- Modify: `web/e2e/console/market.spec.ts`（`C-MKT-13`）、`web/e2e/admin/instances.spec.ts`（`A-INS-15`）、`web/e2e/coverage.json`、`web/e2e/__tests__/coverage.spec.ts`（若按前缀校验需加 `C-WF` / `A-WF`）

**用例（规格 §16.3）：** `C-WF-01`～`12`、`C-MKT-13`、`A-WF-01`～`07`、`A-INS-15`。安装后用 `/hooks/apps/tick` 推进到 running（fake comfyui 的 `app.sh` 在 Task 11 已支持 manifest）；快速出图用 `/hooks/comfy/history` 把状态推到 `done`，断言 `qr-file-0` 的 `<img>` 加载成功（`naturalWidth > 0`）。

- [ ] **Step 1: 写用例并登记 ID**。
- [ ] **Step 2: `npm run e2e` 全绿（单 worker）**。
- [ ] **Step 3: 提交** `test(e2e): workflow market, install, quick-run, admin workflow pages (C-WF-01..12, A-WF-01..07)`。

---
### Task 16: 真机验收（§16.4）与 `acceptance-plan11.md`、`followups-plan11.md`

**Files:**
- Create: `docs/superpowers/acceptance-plan11.md`、`docs/superpowers/followups-plan11.md`
- Modify: 验收中发现的缺陷按计划 7 的做法修复并记录（提交信息带 `(acceptance X-n)`）

**步骤：**
- [ ] **Step 1: 环境**：Mac 上 `platform` 用本分支构建（配置同计划 7 验收，`SPARK_AGENT_BANDWIDTH_MBPS=1000`，`SPARK_APP_GIT_MIRROR` 默认）；Agent 与镜像沿用计划 7 验收产物（确认 221 上 `spark/comfyui:25.09` 存在、Agent 版本含 `image.env` 修复）；迁移到 00024。
- [ ] **Step 2: 按 §16.4 第 1～10 项逐项执行**，每项记录时间线、截图（scratchpad `shots/`）、容器内核对命令与输出、接口响应。模型全部在实例容器内下载（在记录里写明 Mac 未下载任何模型）。sudo 步骤请负责人执行。
- [ ] **Step 3: 缺陷表**（编号 `W-n`，现象 / 复现 / 原因 / 严重度 / 修复提交）与观察（`O-n`）；修复后复测对应项。
- [ ] **Step 4: `followups-plan11.md`**：上线前核对（Git 镜像可达性、六个工作流在生产跑一遍、Wan2.2 下载时间、封面版权、Task 3 的 `install_key` 兼容说明）、规格遗留清单、验收观察。
- [ ] **Step 5: 释放验收实例；提交** `docs: plan 11 real-machine acceptance record on 192.168.1.221, followups-plan11`。

---

## 验收标准（整个计划完成的定义）
- `make test`、`go vet ./...`、`npm test`、`npm run lint`、`npm run e2e` 全绿。
- `acceptance-plan11.md` 的 §16.4 十项全部「通过」或「部分通过且缺陷已修并复测」。
- 分支可 fast-forward / 无冲突合并到 `main`（已 rebase）。
