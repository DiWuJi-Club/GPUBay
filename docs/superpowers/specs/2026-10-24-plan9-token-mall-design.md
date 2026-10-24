# Spark Cloud — 计划 9：Token 商城（按量计费的大模型 API）设计文档

- 日期：2026-10-24
- 状态：待评审（负责人已授权由设计方裁定、不再逐项询问；代负责人作出的产品裁定集中列在 §1.3，可事后推翻）
- 上位文档：`2026-10-14-spark-cloud-mvp-design.md`（下称「总设计」）§2 `wallet_*`、路线图「Token 商城」；`2026-10-18-plan3-access-design.md`（下称「计划 3」）§5.3 上游连接池；`2026-10-19-plan6-booking-design.md`（下称「计划 6」）§3 机组、§4.2 候选机器；`2026-10-20-plan7-deploy-design.md`（下称「计划 7」）§5 部署机制、§5.4 钩子；`2026-10-20-plan8-referral-design.md`（下称「计划 8」）§4.3 `wallet.ApplyUpTo`、§5 清扫附加任务
- 范围：用户在控制台开通 API Key，以 OpenAI 兼容接口（`/v1/chat/completions`、`/v1/completions`、`/v1/embeddings`、`/v1/models`）调用平台托管的大模型；按模型分别对输入 / 输出 token 定价（元 / 百万 token），从统一钱包余额预付扣款；速率限制；用量日志与结算；控制台页面；后台模型 / 节点 / 机器池 / 用量管理；为 Token 服务保留一池机器并在其上自动拉起 vLLM 节点；健康检查与故障转移；测试与真机验收。
- 不在范围：见 §17。

## 0. 现状与约束（实现时不必再查）

**代码事实（`main` 上计划 1～6 已合并；计划 7、8 分别在 `feat/plan7-deploy`、`feat/plan8-referral`，本计划落在它们之后；分支 `feat/plan9-token-mall`）**
- 钱包：`wallet_accounts(user_id PK, balance ≥ 0)`、`wallet_ledger` 只追加，`kind` 的 CHECK 在计划 8 之后为 `recharge / admin_adjust / order_pay / refund / referral_rebate / referral_clawback`（约束名 `wallet_ledger_kind_check`，计划 8 的 `00021` 重建过一次）；`wallet.Apply(ctx, tx, Entry)` 余额不足返回 `apperr.ErrInsufficientBalance`、重复 `(kind, ref_type, ref_id)` 返回 `ErrDuplicate`；**计划 8 新增 `wallet.ApplyUpTo(ctx, tx, Entry) (applied, after int64, err error)`**：尽力扣款，扣款额取 `min(|amount|, balance)`，余额为 0 时不记流水。`wallet.ledgerKinds`（`admin.go`）、`web/shared/src/labels.ts` 的 `ledgerKindLabel` 都要同步加新类型。余额不可提现（计划 4）。
- 实例与机器：`instances.user_id NOT NULL`、`expires_at` 可空（creating 阶段为 NULL）、`pending_seconds`；`ExpireDue` 谓词 `state='running' AND suspended_at IS NULL AND expires_at <= $1`，`SendReminders` 也按 `expires_at <=`——**`expires_at IS NULL` 的 running 实例天然不会到期、不会提醒**；`AutoRenewDue` 要求 `auto_renew`。`machine_leases` 排他约束 + 计划 6 的 `machine_leases_period_nonempty`。机器行锁一律 `FOR NO KEY UPDATE`（计划 6 §14）。`fleet.MarkAllocated / Release / MarkFaulty`、`RaiseAlert / RaiseAlertOnce(ctx, q, fleet.Alert{Level, Kind, Message, MachineID?, InstanceID?})`。
- 机组（计划 6）：`machine_groups(name, region, description, sort, visible)`；`visible=false` 的机组**不在控制台展示、不能下单、不参与「不限机组」分配**（裁定 B9，`pickMachines` 的 `(m.group_id IS NULL OR g.visible)`），是「留给运维 / 大客户的池子」。`fleet.Service.{ListGroups, GetGroup, CreateGroup, UpdateGroup, DeleteGroupIn, AssignMachines, UnassignMachines, UpdateMachine}`，后台 `POST/DELETE /groups/{id}/machines`。
- 一键部署（计划 7，新包 `internal/appdeploy`）：`apps / app_versions / app_deployments / app_deploy_events`；`appdeploy.Service.{Create(CreateRequest{UserID, InstanceID, AppID, VersionID?, Env, IP}), Redeploy, Stop, Start, Remove, AdminForceRemove, Get, ListLive, LiveByInstances, Events, Log}`；`appdeploy.Catalog.{AppBySlug, LatestPublished, ListPublished, ImportDir, PublishVersion}`；Runner 每 5 秒经 `upstream.Pool.Exec` 在容器里驱动 `run.sh`，部署 `running` 时登记私有端口 `instance_ports(deployment_id)`，`health ∈ unknown / ok / unhealthy`，连续 3 次进程不在 → `failed(crashed)`；`apps.status ∈ draft / published / retired`，published 的应用在控制台市场页可见；`deploy/apps/<slug>/{app.json, spec.json, app.sh}` 由 `platform admin app import deploy/apps` 导入；vLLM 应用 1.0.4（`--enable-auto-tool-choice --tool-call-parser hermes`，`MODEL_ID` 是**枚举**、只有 4 个 Qwen 小模型，`API_KEY` 可选）；镜像 `spark/llm-serving:25.09`（名称 `LLM Serving`）含 vLLM、Open WebUI、JupyterLab；`rental.AppHooks`（`OnInstanceRunning / OnInstanceGone / OnInstanceMigrated`）由 appdeploy 实现；`rental.PlaceRequest / BatchRequest` 带 `AppID / AppEnv`。镜像环境经 `/run/spark-image.environ` 注入（新版 Agent）。
- 上游连接（计划 3 / 7）：`upstream.Pool.Dial(ctx, Target{InstanceID, LanIP, HostPubkey}, port) (net.Conn, error)` 打 `direct-tcpip` 到容器内 `127.0.0.1:<port>`，连接层错误是 `*upstream.DialError`；`Pool.Exec(...)`；每实例一条共享 `ssh.Client`，ingress 与 appdeploy Runner 共用同一个 Pool。ingress 对 WebSocket / SSE 用 `http.ResponseController` 清除读写期限、`FlushInterval: -1`（`internal/ingress/streams.go`），公网 `http.Server` 的 `WriteTimeout` 是 60 秒。
- 控制台 API（`internal/api/server.go`）：`/api/v1`，`requireUser` 组；匿名端点在 `httpx.NoStore + s.calendarRateLimit` 组（按 IP 每分钟 120 次，`ipLimiter`，计划 8 另有 `ipRateLimit(l)` 包装）；`requireJSONForWrites` 对支付回调路径 `notifyPathPrefix` 免检；`httpx.SameSiteGuard(PublicBaseURL, exempt, log)`；`writeError` 走 `httpx.WriteError`（`apperr` → JSON `{code, message}`）。后台 `adminapi`：`s.handle(g, method, pattern, roles, h)` 登记路由供 `permissions_test.go` 核对；角色表 `allRoles / opsRoles / financeRoles / superOnly`；`withStepUp(r, totp, op)`；审计 `audit.Record(ctx, tx, now, actor.Entry(action, targetType, targetID, diff))`。
- 身份：`users(phone UNIQUE, kind, kyc_status, status)`，`identity.Login` 的手机号正则 `^1[3-9]\d{9}$`；`identity.CanRent(ctx, userID)` = 未封禁且 `kyc_status='verified'`；计划 8 增 `last_login_*`、`user_devices`、`UserState`。
- 清扫：`rental.RunSweeper(ctx, interval, extra...)`，计划 8 后 `cmd/platform/main.go` 为 `a.Rental.RunSweeper(ctx, cfg.SweepInterval, a.Billing.Sweep, a.Referral.Settle)`；`e2ehooks` 的 `POST /hooks/sweep` 顺序调用；附加任务超时 2 分钟；`sweepEach` 模式；多副本靠咨询锁。
- 配置：计划 7 加了 `SPARK_APP_RUNNER on/off`（`config.loadApps`），后台 Runner 用 `pg_try_advisory_lock(hashtext('appdeploy'))`。
- 前端：控制台底部 / 侧栏 4 个入口（`ConsoleLayout.vue`：算力市场 / 我的实例 / 钱包 / 我的），路由在 `web/console/src/router.ts`；后台菜单 `web/admin/src/menu.ts`（`menu.spec.ts` 核对）；类型 `web/shared/src/types/*.ts`，中文映射 `labels.ts`（`ledgerKindLabel`、`alertKindLabel`、`auditActions`）。
- e2e：Playwright 单 worker、假时钟；e2e-server 内嵌 fake Agent 与 `sshtest` 假容器 sshd（`sshtest.Server.ServeHTTP(port, handler)` 可在假容器的某个端口挂 HTTP 处理器，`/hooks/ingress-upstream` 就是这样做的）；计划 7 钩子 `/hooks/apps/seed`、`/hooks/apps/tick`；计划 8 钩子 `/hooks/referral/config`。覆盖表前缀 `C-* / A-*`。
- 迁移编号：计划 7 `00020_apps.sql`，计划 8 `00021_referral.sql`；**本计划 `00022_llm.sql`**（带 `+goose Down`）；计划 10 / 11 / 12 预留 `00023 / 00024 / 00025`。
- 真机：一台 DGX Spark（192.168.1.221，`spark-3479`，GB10，128GB 统一内存，arm64，Ubuntu 24.04，Docker 29.6.2，CUDA 13），平台跑在 Mac 192.168.1.218（`SPARK_DOMAIN=192-168-1-218.sslip.io`，开发 Mac 的 DNS 对 `*.sslip.io` 返回假地址，curl 加 `--resolve`）；Docker Hub 不可达、nvcr.io 可达；镜像 `spark/llm-serving:25.09` 已在 221 上构建并以本地 tag 登记；验收时 `SPARK_AGENT_BANDWIDTH_MBPS=1000`；需要 root 的命令由协调人在运行时经 `sudo -S` 执行，密码不落盘。Qwen3-4B（7.6GB）在 221 上下载约 3 分钟、vLLM 启动约 2 分钟（计划 7 验收记录）。

**竞品（器灵算力站「Token 商城」）**：按模型列出输入 / 输出价格（元 / 百万 token），余额扣费，用户自建 API Key，OpenAI 兼容地址，控制台看用量与明细。

## 1. 架构与核心决策

```
 用户程序（OpenAI SDK / curl）                  控制台 /tokens                    后台 /llm
   Authorization: Bearer sk-spark-…              Key / 用量 / 价格                 模型 / 节点 / 机器池 / 用量
        │ https://<domain>/v1/…                          │                              │
        ▼                                                ▼                              ▼
┌──────────── llmgw（OpenAI 兼容网关，internal/llmgw，挂在用户 API 进程的 /v1）────────────┐
│ 鉴权（Key 哈希 → 用户；30 秒缓存）→ 账户门槛（实名 / 未封禁 / 无欠费 / 未停用）             │
│ → 限流（RPM / TPM / 并发，按用户）→ 预算预检（余额 − 未结算 − 在途估算 ≥ 本次估算）           │
│ → 选节点（模型的 ready 节点，同 Key 优先同节点，否则最少在途）→ 经 upstream.Pool.Dial 转发   │
│ → 流式：透传 SSE、注入 stream_options.include_usage、解析末尾 usage；非流式：解析 usage     │
│ → 写 llm_usage（token 数、价格快照、cost_micro）+ llm_accounts.unsettled_micro            │
│ 失败转移：首字节之前的连接 / 5xx 错误换节点重试一次；首字节之后不重试、不计费               │
└────────────────────────────────────────────────────────────────────────────────────────┘
        │                                                     ▲
        ▼                                                     │ 健康探测 GET /v1/models（每 10 秒）
┌──────────── llm（业务核心，internal/llm）───────────────────────────────────────────────┐
│ 模型目录 llm_models（slug、价格、上下文、期望副本数、节点参数）                             │
│ 节点 llm_nodes：provisioning → ready ⇄ unhealthy → draining → removed；failed             │
│ Controller（每 10 秒，咨询锁）：按期望副本数在「Token 服务池」机组的空闲机器上               │
│   PlaceSystem（系统实例，不计费不到期）→ appdeploy.Create(vllm-node) → 部署 running → ready │
│   缩容 / 下线：draining（等在途结束）→ rental.ReleaseSystem → 洗机 → 机器回到池内空闲        │
│ Settle（清扫附加任务，每分钟）：按用户汇总未结算用量 → 一条 wallet_ledger(llm_pay)           │
│   wallet.ApplyUpTo 扣款，不足部分记欠费（debt），欠费期间网关拒绝；余额补足后下轮先扣欠费    │
└────────────────────────────────────────────────────────────────────────────────────────┘
        │ 不改 Agent 协议：节点 = 系统账号持有的实例 + 计划 7 的 vllm-node 应用部署
        ▼
 机器池 = 一个 purpose='llm'、visible=false 的机组（不对外出租）；每台机器一个节点、一个模型
```

### 1.1 决策一览

| # | 决策 | 结论 | 理由 |
|---|---|---|---|
| 1 | 网关选型：New-API / LiteLLM 还是自建 | **自建 Go 网关（`internal/llmgw`）+ 开源 vLLM 作为推理引擎**；不引入 New-API / LiteLLM | 两者都自带用户、令牌、余额、渠道四套概念，与我们的 `users / wallet / sessions / audit` 重复，要做双向同步（余额、封禁、实名）且多一个 Node / Python 进程与数据库；而我们需要的网关功能（鉴权、限流、计费、选节点、SSE 透传）不到两千行 Go，且能直接复用 `wallet.ApplyUpTo`、`upstream.Pool`、`apperr`、审计与 e2e 钩子。与此前「自研业务核心 + 开源组件」的方向一致 |
| 2 | 节点怎么跑 | **节点 = 系统账号持有的「系统实例」+ 计划 7 的 `vllm-node` 应用部署**；平台经已有的上游 SSH 连接池 `Dial` 到容器内 `127.0.0.1:8000` 转发请求 | 完全复用计划 2 / 3 / 7 的容器、镜像、模型下载、健康检查、日志、端口与排障；不加 Agent 指令、不在宿主机起第二个容器（计划 2 假设每机一实例）；ingress 已经证明经 SSH 通道转发 SSE / WebSocket 可行 |
| 3 | 机器池 | **池 = 一个 `purpose='llm'`、`visible=false` 的机组**（迁移建好「Token 服务池」）；运营用现有的机组 API 把机器加入 / 移出；池内机器不参与任何出租分配（计划 6 B9 已保证），Controller 只在「空闲 + 在线 + 无占用时段 + 无节点」的池内机器上建节点 | 不发明第二套「机器用途」字段；加入池 = 停止对外出租，现有租约到期后自然空出；移出池 = 节点下线后恢复出租 |
| 4 | 计费单位与精度 | 价格 = **分 / 百万 token**（后台填元，前端换算）；单次请求费用 `cost_micro = prompt_tokens × input_price + completion_tokens × output_price`（单位**微分**，整数、精确）；结算时按用户汇总，`floor((Σ cost_micro + carry) / 10^6)` 分入账、余数 `carry_micro` 带到下一次 | 每请求向上取整到分会把 ¥0.0003 的请求收成 ¥0.01；微分整数运算无浮点误差，总账与明细严格可对 |
| 5 | 扣款时机 | **预付 + 后结**：请求前做预算预检（余额 − 未结算 − 在途估算 ≥ 本次估算），请求后只记用量；清扫附加任务 `Settle` 每分钟把每个用户的未结算用量汇成**一条**钱包流水（`kind='llm_pay'`，`ref_type='llm_settlement'`）；未结算金额 ≥ ¥1 或最早未结算用量已超过 10 分钟才结算 | 每请求一条 `wallet_ledger` 会让用户的流水页被几千条几分钱的记录淹没，也让 `wallet_accounts` 行成为每秒几十次写的热点；预检保证了「预付」语义，汇总结算保证了可读的账本 |
| 6 | 余额不足的处理 | 预检失败 → 402；并发请求把余额打穿时，结算用 `wallet.ApplyUpTo` 扣到 0，差额记 `llm_accounts.debt`；**有欠费的用户一切请求 402**，充值后下一轮结算先扣欠费再恢复 | 钱包没有冻结 / 预授权（计划 6 B6），只能靠预检 + 欠费兜底；与计划 8 的欠账模式一致 |
| 7 | 失败请求是否收费 | 上游连接失败、上游 5xx、上游超时、首字节前的失败转移 → **不收费**；客户端中途断开（流式）→ 收已生成部分（按 SSE 块数估算输出、按请求字节估算输入，`estimated=true`）；正常完成 → 按上游返回的 `usage` 收费 | 平台过失不收钱；用户主动断开已经消耗了算力 |
| 8 | 限流 | 按**用户**（账户）限：RPM、TPM、并发；默认 60 / 200,000 / 8，后台可按用户改；每个 Key 可选**月消费上限**；按节点并发上限（`llm_models.max_concurrency`，默认 32）；都是进程内滑动窗口 | 一个用户多个 Key 共享配额，不然建 Key 就能绕过；节点并发上限防止 vLLM 排队过深拖垮 TTFT |
| 9 | 选节点与故障转移 | 模型的 `ready` 节点里**同一 Key 优先上次用过的节点**（vLLM 前缀缓存），该节点在途 ≥ 80% 上限时换最少在途的；首字节前的连接错误 / 502 / 503 换节点重试一次；节点连续 3 次探测失败 → `unhealthy`（不再分配）；10 分钟仍不健康 → Controller 重新部署，连续 3 次重建失败 → `failed` + 严重告警 | 对用户透明；对运维只在真的坏了时打扰 |
| 10 | 模型与 vLLM 应用 | 新增**内部应用** `vllm-node`（`apps.internal=true`，控制台不展示）：`MODEL_ID` 改为自由字符串、新增 `SERVED_MODEL_NAME / MAX_NUM_SEQS / QUANTIZATION / REASONING_PARSER / TOOL_CALL_PARSER`，`API_KEY` 必填；用户请求里的 `model` = `llm_models.slug`，节点以该 slug 作为 served-model-name，网关**不改写请求体** | 计划 7 的 vLLM 应用面向租户、模型是枚举且 API_KEY 可选；Token 服务需要任意 ModelScope 模型与强制鉴权。slug 直通让网关不解析 / 重写请求体 |
| 11 | 系统实例 | `instances.system=true`、`user_id` = 迁移写入的系统账号（`phone='system:llm'`，不可能登录）；`expires_at NULL`、不产生订单与流水、不到期、不提醒、不自动续费、不参与返利；lease 为 `[now, now + 10 年)`；后台实例列表显示「平台节点」标签，退款 / 延长 / 换机对它返回 `invalid_state` | 让计划 2 / 3 / 7 的全部机制原样工作，只在 rental 加一条「不计费」分支 |
| 12 | 用量数据 | `llm_usage` 每请求一行（**不存 prompt / 补全内容**，只有 token 数、价格快照、费用、状态、延迟）；保留 90 天；`llm_usage_daily` 按天 × 用户 × 模型汇总永久保留 | 隐私最小化；对账用汇总 |
| 13 | 接口地址 | `https://<SPARK_DOMAIN>/v1/…`（与控制台同域，路径不与 `/api/v1` 冲突）；不另起 `api.<domain>` 子域 | 不改 TLS 证书 SAN、不改 Host 分流 |
| 14 | 后台角色 | 模型与价格 **super**（与「价格与镜像」一致）；节点 / 机器池 / 账户限额与停用 **ops**；用量报表、结算、CSV **finance**；仪表盘卡片 all | 与现有权限矩阵一致 |

### 1.2 不做的替代方案（评审时不必再提）
- 「接 New-API / LiteLLM 做网关」：见 §1.1 第 1 条；放弃。
- 「Agent 新增 `run_service` 指令，在宿主机直接起 vLLM 容器」：改协议、破坏「每机一实例」的对账假设、健康 / 日志 / 端口都要重做；放弃。
- 「网关直连机器 `lan_ip:8000`」：容器只发布了 sshd 的 2222（计划 2 §4.4），其它端口不可达；放弃（走上游 SSH 通道，与 ingress 相同）。
- 「每请求一条钱包流水」：见 §1.1 第 5 条；放弃。
- 「预购 Token 套餐 / 充值赠送 / 阶梯价」：没有优惠券体系；放到遗留。
- 「把 Token 消费计入邀请返利」：计划 8 的返利基数是实付租用（`order_pay`），本计划的 `llm_pay` 不在扫描范围内；是否纳入由运营另定，放到遗留。
- 「在网关里做 tokenizer 精确计数」：vLLM 返回的 `usage` 已是权威；只在用户中断时估算。
- 「每台机器跑多个模型 / 一个模型跨多机」：GB10 单机 128GB，一机一模型足够；张量并行跨机不在 v1。

### 1.3 负责人裁定（默认，可推翻）

| # | 裁定 | 理由 |
|---|---|---|
| T1 | 价格以**分 / 百万 token** 存储（`input_price / output_price`），后台以「元 / 百万 token」填写、两位小数；改价**立即对之后的请求生效**，每条用量快照当时价格；不做价格版本表 | 价格是运营参数，变更有审计（`llm.model.update` 带 before / after），明细自带快照即可回溯 |
| T2 | 预算预检：`available = balance − ceil(unsettled_micro / 10^6) − inflight_estimate − debt`，须 `≥ max(本次估算, 1 分)`；本次估算 = `ceil((len(body)/3 × input_price + max_out × output_price) / 10^6)` 分，`max_out = min(请求 max_tokens 或 模型 default_max_tokens, 模型 max_output_tokens)`；`debt > 0` 直接 402 | 没有冻结机制，只能保守预检；`len(body)/3` 是中英混合文本的经验 token 密度，宁可高估 |
| T3 | 结算触发：未结算 ≥ **¥1**（`SettleMinMicro = 100 × 10^6`）或最早未结算用量已超过 **10 分钟**；有欠费且余额 > 0 时每轮都尝试扣欠费 | 一个活跃用户每小时最多几条流水；小额不积压超过 10 分钟 |
| T4 | 默认限额：每用户 **RPM 60 / TPM 200,000 / 并发 8**；节点并发默认 **32**；请求体 ≤ **2 MiB**；总超时 **10 分钟**、首字节超时 **120 秒**；超限 429 带 `Retry-After` | 单机 vLLM 对小模型每秒几十请求是常态，默认值既防滥用又不卡住正常开发；后台按用户可改 |
| T5 | API Key：格式 `sk-spark-` + 40 位 base62（`crypto/rand`），只存 `sha256` 十六进制、展示 `sk-spark-…` + 末 4 位；创建时**明文只返回一次**；每用户最多 **20 个**有效 Key；可设名称、允许的模型列表（空 = 全部）、月消费上限、到期时间；撤销不可恢复 | 与 OpenAI 的 Key 习惯一致；高熵随机串用 sha256 足够，不需要 bcrypt |
| T6 | 创建 Key 与调用接口都要求 `identity.CanRent`（已实名、未封禁）；封禁 / 撤销 / 停用在 **30 秒**内生效（网关的 Key 缓存 TTL） | 与下单同一门槛；Token 服务可被用于生成内容，实名是底线 |
| T7 | 模型 `status ∈ draft / online / offline`：只有 `online` 在控制台与 `/v1/models` 可见、可调用；`offline` 的期望副本按 0 处理（节点排空后释放）；删除只允许没有节点、没有用量的 `draft` | 下线不删历史 |
| T8 | 一台池内机器一个节点一个模型；期望副本数 `replicas` 由运营按模型设置（0～200）；池内空闲机器不足时节点保持缺额并告警 `llm_pool_short`（一次），不抢租用机器 | 不自动从租用池借机器，容量分配是运营决策 |
| T9 | 缩容与下线：节点进入 `draining` 后不再接新请求，等在途归零或 **60 秒**，然后 `rental.ReleaseSystem` → 洗机 → 机器回到池内空闲（数据盘上的模型缓存随洗机清除，下次建节点重新下载） | 洗机是现有租约结束的唯一路径，保留模型缓存要改 Agent 的 wipe 语义（遗留） |
| T10 | 节点不健康处理：连续 3 次探测失败（每 10 秒一次，5 秒超时）→ `unhealthy`、告警 `llm_node_unhealthy`（一次）；恢复一次探测成功即 `ready`；`unhealthy` 持续 **10 分钟** → `appdeploy.Redeploy`（重建计数 +1）；重建 **3 次**仍失败 → `failed`、严重告警 `llm_node_failed`，由运维处置（重建 / 移除）；部署 `failed` 或实例非 running（机器失联暂停）→ 立即 `unhealthy` | 自愈一轮，再坏就是硬件 / 镜像问题 |
| T11 | 流式响应**总是**带末尾 `usage` 块（网关注入 `stream_options.include_usage=true`），即使客户端没要求；帮助页注明 | 没有 usage 就没法计费；OpenAI 官方 SDK 对多出的 usage 块无害 |
| T12 | 客户端中断的流式请求：输出 token = 已转发的内容块数（vLLM 每块约一个 token）、输入 token = `len(body)/3`，`estimated=true`；上游错误 / 超时 / 首字节前失败一律 `cost_micro=0`（仍记用量行供排查） | 见 §1.1 第 7 条 |
| T13 | 节点 vLLM 以 `--api-key <节点密钥>` 启动（平台生成 32 字节随机、secretbox 存 `llm_nodes.api_key_enc`），网关转发时替换 `Authorization`；用户的 Key 永不到达节点 | 纵深防御：即使容器端口被意外公开也不可用 |
| T14 | `GET /v1/models` 需要鉴权（返回该 Key 允许的在线模型）；控制台 `GET /api/v1/llm/models` 匿名可见（市场页展示价格） | 与 OpenAI 行为一致；价格公开 |
| T15 | 速率限制与在途计数是**进程内**的；多副本部署时限额按副本各算一份（README 注明：网关建议单副本，或把限额按副本数折算） | 不引入 Redis；200 台规模下用户 API 进程本来就是单副本 |
| T16 | 新流水类型 `llm_pay`（中文「Token 消费」），`ref_type='llm_settlement'`、`ref_id=<结算 ID>`，备注「Token 消费 N 次请求 · 输入 X · 输出 Y token」；欠费补扣并入同一条流水 | 一条流水对应一次结算，用户点开能看到对应的请求区间 |
| T17 | 不发短信（欠费、Key 创建等都只在控制台提示）；不做站内信 | 与计划 7 / 8 一致 |
| T18 | 用户的用量明细保留 90 天（`llm_usage`），之后只剩按天汇总；后台 CSV 导出用量明细限 `csvx.MaxRows` | 明细表增长可控 |
| T19 | 支持的端点：`POST /v1/chat/completions`、`POST /v1/completions`、`POST /v1/embeddings`（仅 `kind='embedding'` 的模型，只计输入）、`GET /v1/models`；其它 `/v1/*` 返回 404 OpenAI 格式错误 | 覆盖 OpenAI SDK 的主流用法 |
| T20 | `/v1` 的错误体用 OpenAI 格式 `{"error":{"message","type","code"}}`，响应头 `X-Request-Id`；`/v1` 免除 `requireJSONForWrites` 与 `SameSiteGuard`（网关自己校验 `Content-Type`，Bearer 鉴权没有 CSRF 面），并开放 CORS（`*`，无凭据） | SDK 兼容；浏览器端应用可直接调用 |

### 1.4 实施裁定（2026-10-24～25 实施期间代负责人作出，可推翻）

| # | 裁定 | 理由 |
|---|---|---|
| I1 | 实施开始时计划 8 尚未合入：只摘了 `wallet.ApplyUpTo`（231613e、5dd1430）与返利流水类型常量；`llm.UserLookup` 只有 `CanRent`，用户状态 / 实名由 llm 包在 Key 查询里直接 JOIN `users` 读取。计划 7、8 合入 `main` 后已整体合并。 | 不提前引入计划 8 的大部分代码 |
| I2 | `00022` 重建的 `wallet_ledger_kind_check` 列出 7 种（含 `referral_*`），Down 恢复计划 8 的 6 种；Up 对系统账号与池机组按唯一键 upsert（Down 保留这两行）；Down 另在仍有未结束的系统实例时拒绝 | 与 00021 前后都兼容；往返可重放 |
| I3 | 不变量：第 4 条要求流水恰是本次结算的 `llm_pay`（`ref_id` = 结算 ID 十进制）；第 6 条「ready ⇒ 部署 running」只在 `deployment_id` 非空时核对（e2e 钩子的节点无部署），`removing` 节点允许实例已 released；第 2、5 条只在 90 天清理开始前严格成立；日桶统一为 `(created_at AT TIME ZONE 'Asia/Shanghai')::date`（`usageDaySQL`） | 与实现与测试场景一致 |
| I4 | 系统实例：`ReleaseSystem` 直接进入 wipe（不先 Stop，`beginWipe` 会取消在途指令，与 `Release` 一致）；wiping 中的系统实例 lease 上界按普通 `WipeBuffer`；后台改派到指定机器时拒绝池内机器；`PlaceSystem` 要求机组 `purpose='llm'`，允许已停用镜像 | 防止洗机失败时占机 10 年；池机器只给平台节点 |
| I5 | 内部应用：`PrepareInstall`（下单预装）也拒绝 internal 应用；importer 只在新建应用时写 `internal`，后台应用 API 不能改它；internal 校验在选版本之前（用户永远得到 `app_not_found`） | 防止用户经任何路径部署 `vllm-node` |
| I6 | `vllm-node`：`TASK` 也是平台保留变量（按模型 `kind` 填 `embed/generate`）；`node_env` 不接受 secret 类变量；节点密钥经环境变量 `VLLM_API_KEY` 传给 vLLM（不进 argv）；embed 节点不加工具 / 推理解析参数；`--task embed` 绑定 25.09 镜像的 vLLM 0.10.1；`MAX_MODEL_LEN` 有默认值 32768（平台总会传） | 纵深防御；版本差异写进注释 |
| I7 | 模型：`kind` 只在草稿时可改；`max_output_tokens` 缺省为 `min(8192, context_len)`；`node_env` 按固定的 `app_version_id`（为空则最新已发布版本）的 spec 校验；改价等不涉及节点参数的修改不重新校验目录 | 避免已上线模型类型与节点不符 |
| I8 | Key：后台「停用」就是普通 `disabled`（用户可自行启用，彻底停用请停用账户）且总写审计；不存在的 Key 不缓存（每 IP 401 限流兜底）；缓存失效带代际计数，进程内撤销立即生效；`InvalidateUser` 在调额度、结算改变欠费 / 未结算后调用 | 30 秒缓存窗口内尽量准确 |
| I9 | 用量：`request_id` 重复 → 整笔回滚（`ErrDuplicateUsage`，只记日志不告警）；`ok / client_abort` 之外的状态费用强制 0；账户视图费用向下取整到分、未结算向上取整 | 记账幂等；与结算口径一致 |
| I10 | 结算：候选账户按「到期条件」本身筛选（未结算 ≥ ¥1、或有超过 10 分钟的未结算行（含 0 费用行）、或欠费且余额 > 0），避免 500 个未到期账户占满名额；每次结算后 `InvalidateUser`；标记后把 `unsettled_micro` 重算为剩余未结算行之和（自愈漂移，不一致记 Error）；纯补扣欠费且一分没扣到时回滚，不留空结算行 | 防饿死；账实一致 |
| I11 | 限流 / 路由：TPM 估算在 `Acquire` 内夹到用户 TPM 上限；路由表只含在线模型、实例 running 且未暂停的 ready 节点；同 Key 粘滞按 (Key, 模型)；`PickAcquire` 在同一把锁内选节点并占在途 | 大请求不会永远 429；不超节点并发 |
| I12 | 网关：首字节超时只对流式请求生效（vLLM 非流式要生成完才发响应头，非流式只受 10 分钟总超时约束）；写期限 = 总期限 + 30 秒宽限，超时记 `upstream_timeout` 不收费；拨号超时也走失败转移；CORS 预检回显请求头（兼容 OpenAI JS SDK 的 `x-stainless-*`）；节点返回 401 → 502 `upstream_error` 并丢弃缓存的节点密钥；预算预检额外扣掉本进程在 Key 缓存加载之后记下的费用 | 修正规格 §6.1 第 8 步对非流式的误伤；减少透支 |
| I13 | Controller：排空先刷新路由表，且 `drain_since + 2 × RouterRefresh` 之后才相信「在途 0」；重建后的部署失败 / 超时回到 `unhealthy` 继续重建循环，满 3 次 `failed` + 严重告警；首次部署失败 → `failed` + 告警 `llm_node_failed`「节点部署失败，详情见后台节点页」（warning）；`Redeploy` 永久错误或部署已 removed / lost → `failed` + 严重告警；`vllm-node` 未标记 internal 时不建部署并告警；超过 2 分钟无节点引用的系统实例视为孤儿释放；后台「重建」`failed` 节点会新建节点行（模型须有缺额、同机器未被替换） | 自愈但不无限重试；崩溃恢复 |
| I14 | 池机组：机器有非终态节点或未结束的系统实例时，移出池 / 改机组 / 分配到别的机组一律 `llm_pool_machine_busy`，校验放在 fleet 事务内（锁机器行之后，按 SQL 读 `llm_nodes`，不 import llm） | 消除并发窗口 |
| I15 | 后台角色：`GET /llm/accounts`、`GET /llm/keys` 对 ops 与 finance 开放（规格表分别写 finance / ops）；改额度、停用 Key 仍只 ops；`GET /llm/accounts` 增加精确 `user_id` 筛选 | 两种角色都需要看账户 |
| I16 | 前端：控制台底部导航 5 项都加图标（声明已是传递依赖的 `@element-plus/icons-vue`，不新增安装）；钱包流水类型以标签着色并支持 `?ledger=<id>` 定位；Key 到期日按所选北京日期的 23:59:59 提交；admin 与 console 各自有微分格式化函数 | 一致的导航；从结算记录跳到流水 |
| I17 | e2e 覆盖表编号：计划 8 已占 `C-WAL-05`、`C-ACC-03`，本计划顺延（`C-WAL-06` 等） | 不冲突 |

## 2. 数据模型（迁移 `00022_llm.sql`，带 `+goose Down`）

```sql
-- 系统账号与系统实例
ALTER TABLE users ADD COLUMN system BOOLEAN NOT NULL DEFAULT false;      -- 平台内部账号：不能登录、不在用户列表默认筛选里
INSERT INTO users (phone, kind, kyc_status, status, system, created_at, updated_at)
VALUES ('system:llm', 'personal', 'verified', 'active', true, now(), now());   -- phone 不满足 ^1[3-9]\d{9}$，任何登录路径都到不了它
ALTER TABLE instances ADD COLUMN system BOOLEAN NOT NULL DEFAULT false;  -- 平台节点实例：无订单、expires_at NULL、不到期不提醒不自动续费
CREATE INDEX instances_system_live_idx ON instances (machine_id) WHERE system AND state IN ('creating', 'running', 'stopped', 'wiping');

-- 机器池：复用机组
ALTER TABLE machine_groups ADD COLUMN purpose TEXT NOT NULL DEFAULT 'rental' CHECK (purpose IN ('rental', 'llm'));
INSERT INTO machine_groups (name, region, description, sort, visible, purpose, created_at, updated_at)
VALUES ('Token 服务池', '', '平台自用：Token 商城推理节点所在机器，不对外出租', 1000, false, 'llm', now(), now());
CREATE UNIQUE INDEX machine_groups_llm_uniq ON machine_groups (purpose) WHERE purpose = 'llm';   -- 只有一个池

-- 内部应用（控制台不展示）
ALTER TABLE apps ADD COLUMN internal BOOLEAN NOT NULL DEFAULT false;

-- 模型目录
CREATE TABLE llm_models (
    id                 BIGSERIAL PRIMARY KEY,
    slug               TEXT NOT NULL UNIQUE,              -- 用户请求里的 model，^[a-z0-9][a-z0-9.-]{1,63}$；也是节点的 served-model-name
    name               TEXT NOT NULL,                     -- 展示名，1～64
    description        TEXT NOT NULL DEFAULT '',          -- ≤ 2000
    kind               TEXT NOT NULL CHECK (kind IN ('chat', 'embedding')),
    source             TEXT NOT NULL DEFAULT 'modelscope' CHECK (source IN ('modelscope', 'hf-mirror')),
    source_id          TEXT NOT NULL,                     -- ModelScope / HF 模型 ID，如 Qwen/Qwen3-32B
    image_id           BIGINT NOT NULL REFERENCES images (id),          -- 节点实例镜像（LLM Serving）
    app_version_id     BIGINT REFERENCES app_versions (id),             -- vllm-node 版本；NULL = 建节点时取最新 published
    context_len        INT  NOT NULL CHECK (context_len BETWEEN 1024 AND 1048576),   -- MAX_MODEL_LEN
    max_output_tokens  INT  NOT NULL DEFAULT 8192 CHECK (max_output_tokens BETWEEN 1 AND 262144),
    default_max_tokens INT  NOT NULL DEFAULT 4096,        -- 请求未带 max_tokens 时的预算估算值（不改写请求）
    input_price        BIGINT NOT NULL CHECK (input_price >= 0),        -- 分 / 百万 token
    output_price       BIGINT NOT NULL CHECK (output_price >= 0),
    node_env           JSONB NOT NULL DEFAULT '{}',       -- 传给 vllm-node 的附加变量（GPU_MEMORY_UTILIZATION、MAX_NUM_SEQS、QUANTIZATION、REASONING_PARSER、TOOL_CALL_PARSER）
    replicas           INT  NOT NULL DEFAULT 0 CHECK (replicas BETWEEN 0 AND 200),
    max_concurrency    INT  NOT NULL DEFAULT 32 CHECK (max_concurrency BETWEEN 1 AND 512),
    features           TEXT[] NOT NULL DEFAULT '{}',      -- 展示标签：tools / reasoning / vision / long-context
    status             TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'online', 'offline')),
    sort               INT  NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL
);

-- 节点
CREATE TABLE llm_nodes (
    id                TEXT PRIMARY KEY,                   -- 'node-' + 12 位 [a-z0-9]
    model_id          BIGINT NOT NULL REFERENCES llm_models (id),
    machine_id        BIGINT NOT NULL REFERENCES machines (id),
    instance_id       TEXT REFERENCES instances (id),     -- PlaceSystem 之后非空
    deployment_id     TEXT REFERENCES app_deployments (id),   -- appdeploy.Create 之后非空
    state             TEXT NOT NULL CHECK (state IN ('provisioning', 'ready', 'unhealthy', 'draining', 'removing', 'removed', 'failed')),
    api_key_enc       BYTEA NOT NULL,                     -- 节点 vLLM --api-key，secretbox
    probe_fail_count  INT  NOT NULL DEFAULT 0,
    rebuild_count     INT  NOT NULL DEFAULT 0,
    unhealthy_since   TIMESTAMPTZ,
    drain_since       TIMESTAMPTZ,
    last_probe_at     TIMESTAMPTZ,
    last_ready_at     TIMESTAMPTZ,
    error             TEXT NOT NULL DEFAULT '',           -- 最近错误（可展示，≤ 200）
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX llm_nodes_machine_live_uniq ON llm_nodes (machine_id) WHERE state NOT IN ('removed', 'failed');
CREATE INDEX llm_nodes_model_idx ON llm_nodes (model_id, state);

CREATE TABLE llm_node_events (
    id         BIGSERIAL PRIMARY KEY,
    node_id    TEXT NOT NULL REFERENCES llm_nodes (id),
    at         TIMESTAMPTZ NOT NULL,
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX llm_node_events_node_idx ON llm_node_events (node_id, id);

-- 用户账户（有 Key 或有用量才有行）
CREATE TABLE llm_accounts (
    user_id           BIGINT PRIMARY KEY REFERENCES users (id),
    unsettled_micro   BIGINT NOT NULL DEFAULT 0 CHECK (unsettled_micro >= 0),   -- Σ 未结算 cost_micro（与 llm_usage 一致，§14 不变量 3）
    carry_micro       BIGINT NOT NULL DEFAULT 0 CHECK (carry_micro BETWEEN 0 AND 999999),   -- 结算余数（微分）
    debt              BIGINT NOT NULL DEFAULT 0 CHECK (debt >= 0),              -- 欠费（分）
    rpm_limit         INT,                               -- NULL = 默认 60
    tpm_limit         INT,                               -- NULL = 默认 200000
    concurrency_limit INT,                               -- NULL = 默认 8
    blocked           BOOLEAN NOT NULL DEFAULT false,    -- 后台停用 Token 服务
    blocked_reason    TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);

CREATE TABLE llm_api_keys (
    id               BIGSERIAL PRIMARY KEY,
    user_id          BIGINT NOT NULL REFERENCES users (id),
    name             TEXT NOT NULL,                      -- 1～32
    key_hash         TEXT NOT NULL UNIQUE,               -- sha256 十六进制
    key_hint         TEXT NOT NULL,                      -- 'sk-spark-…' + 末 4 位
    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'revoked')),
    model_slugs      TEXT[] NOT NULL DEFAULT '{}',       -- 空 = 全部在线模型
    monthly_cap      BIGINT,                             -- 分；NULL = 不限
    month_ym         TEXT NOT NULL DEFAULT '',           -- 月消费计数所属月份（Asia/Shanghai，'2026-10'）
    month_cost_micro BIGINT NOT NULL DEFAULT 0,
    expires_at       TIMESTAMPTZ,
    last_used_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    revoked_at       TIMESTAMPTZ
);
CREATE INDEX llm_api_keys_user_idx ON llm_api_keys (user_id, created_at DESC);

-- 结算（一行 = 一条 llm_pay 流水，或金额为 0 的无流水结算）
CREATE TABLE llm_settlements (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT NOT NULL REFERENCES users (id),
    requests          INT    NOT NULL,
    prompt_tokens     BIGINT NOT NULL,
    completion_tokens BIGINT NOT NULL,
    cost_micro        BIGINT NOT NULL,                   -- 本次纳入的用量合计（微分）
    usage_amount      BIGINT NOT NULL CHECK (usage_amount >= 0),   -- 由用量折出的分 = floor((cost_micro + carry_before) / 1e6)
    debt_amount       BIGINT NOT NULL DEFAULT 0 CHECK (debt_amount >= 0),   -- 并入本次的历史欠费
    charged           BIGINT NOT NULL CHECK (charged >= 0),         -- 实际扣到（≤ usage_amount + debt_amount）
    wallet_ledger_id  BIGINT,                            -- charged = 0 时为 NULL
    period_from       TIMESTAMPTZ,                       -- 纳入用量的最早 / 最晚 created_at；纯扣欠费时为 NULL
    period_to         TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX llm_settlements_user_idx ON llm_settlements (user_id, id DESC);

-- 用量明细（不存内容）
CREATE TABLE llm_usage (
    id                BIGSERIAL PRIMARY KEY,
    request_id        TEXT NOT NULL UNIQUE,              -- 'req-' + 16 位；响应头 X-Request-Id
    user_id           BIGINT NOT NULL REFERENCES users (id),
    key_id            BIGINT NOT NULL REFERENCES llm_api_keys (id),
    model_id          BIGINT NOT NULL REFERENCES llm_models (id),
    node_id           TEXT REFERENCES llm_nodes (id),    -- 失败转移后为最终节点；未选到节点为 NULL
    endpoint          TEXT NOT NULL CHECK (endpoint IN ('chat', 'completions', 'embeddings')),
    stream            BOOLEAN NOT NULL,
    prompt_tokens     INT NOT NULL DEFAULT 0,
    completion_tokens INT NOT NULL DEFAULT 0,
    estimated         BOOLEAN NOT NULL DEFAULT false,
    input_price       BIGINT NOT NULL,                   -- 快照（分 / 百万）
    output_price      BIGINT NOT NULL,
    cost_micro        BIGINT NOT NULL CHECK (cost_micro >= 0),
    status            TEXT NOT NULL CHECK (status IN ('ok', 'client_abort', 'upstream_error', 'upstream_timeout', 'no_node')),
    http_status       INT NOT NULL,
    latency_ms        INT NOT NULL DEFAULT 0,
    ttft_ms           INT,                               -- 流式首字节
    client_ip         TEXT NOT NULL DEFAULT '',
    settlement_id     BIGINT REFERENCES llm_settlements (id),
    created_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX llm_usage_user_idx ON llm_usage (user_id, created_at DESC);
CREATE INDEX llm_usage_key_idx ON llm_usage (key_id, created_at DESC);
CREATE INDEX llm_usage_unsettled_idx ON llm_usage (user_id, id) WHERE settlement_id IS NULL;
CREATE INDEX llm_usage_created_idx ON llm_usage (created_at);

-- 按天汇总（永久）
CREATE TABLE llm_usage_daily (
    day               DATE   NOT NULL,                   -- Asia/Shanghai 自然日
    user_id           BIGINT NOT NULL REFERENCES users (id),
    model_id          BIGINT NOT NULL REFERENCES llm_models (id),
    requests          BIGINT NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    cost_micro        BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (day, user_id, model_id)
);
CREATE INDEX llm_usage_daily_day_idx ON llm_usage_daily (day);

-- 钱包：新流水类型
ALTER TABLE wallet_ledger DROP CONSTRAINT wallet_ledger_kind_check;
ALTER TABLE wallet_ledger ADD CONSTRAINT wallet_ledger_kind_check
    CHECK (kind IN ('recharge', 'admin_adjust', 'order_pay', 'refund', 'referral_rebate', 'referral_clawback', 'llm_pay'));
```

说明：
- `instances.system` 的实例 `user_id` = 系统账号；`rental.Instance` 增 `System bool`；后台实例视图增 `system`，前端显示「平台节点」。`users.system` 的用户在后台用户列表默认不显示（筛选 `include_system=true` 才出现），不能封禁（`invalid_state`）、不能调账。
- `machine_groups.purpose='llm'` 的机组：`visible` 必须为 `false`（`fleet.UpdateGroup` 校验，否则 `invalid_state`）、不能删除、不能改 `purpose`；迁移建好唯一的一个。`fleet.Service` 新增 `LLMPoolGroupID(ctx, q) (int64, error)`。
- `apps.internal=true`：`Catalog.ListPublished` 不返回；控制台 `GET /apps` 不可见；用户 `appdeploy.Create` 对 internal 应用返回 `app_not_found`（只有系统账号可以）；后台目录页显示「内部」标签。`deploy/apps/<slug>/app.json` 增字段 `internal`（importer 读取）。
- `llm_usage.key_id` 外键到 `llm_api_keys`：撤销 Key 不删行，明细仍可查。
- 价格、`default_max_tokens` 是预算估算与计费参数；`context_len` 传给节点 `MAX_MODEL_LEN`，网关不校验请求长度（上游 vLLM 自己返回 400）。
- **Down**：先 `DO $$ … IF EXISTS (SELECT 1 FROM wallet_ledger WHERE kind = 'llm_pay') THEN RAISE EXCEPTION '仍有 Token 消费流水，不能回滚' … $$`（与计划 8 同理：流水只追加）；再 `IF EXISTS (SELECT 1 FROM llm_nodes WHERE state NOT IN ('removed','failed')) THEN RAISE EXCEPTION '仍有运行中的节点，请先在后台下线' `；然后按外键顺序 `DROP TABLE llm_usage_daily, llm_usage, llm_settlements, llm_api_keys, llm_accounts, llm_node_events, llm_nodes, llm_models`；`apps` 删 `internal`；`machine_groups` 删索引与 `purpose`（池机组行保留，变成普通不可见机组）；`instances` 删索引与 `system`（系统实例若仍存在，Down 前面已因节点检查拒绝）；`users` 删 `system`（系统账号行保留，仍无法登录）；`wallet_ledger_kind_check` 恢复计划 8 的六种。

**存量数据**：无节点、无 Key、无用量；存量机组 `purpose='rental'`；存量 apps `internal=false`。

## 3. 模型目录与节点应用

### 3.1 模型（后台 super 维护）
- `slug` 创建后不可改（用户代码里写死了它）；`name / description / features / sort / 价格 / replicas / max_concurrency / default_max_tokens` 可随时改；`source / source_id / image_id / app_version_id / context_len / max_output_tokens / node_env` 改动**只影响之后新建的节点**，后台保存时提示「已有 N 个节点仍按旧参数运行，可逐个重建」。
- `status`：`draft`（后台可见）→ `online`（控制台与 `/v1/models` 可见；Controller 按 `replicas` 建节点）→ `offline`（不可调用；期望副本按 0）。`online` 要求 `replicas ≥ 1`（否则 `llm_model_invalid`，消息「上线前请设置至少 1 个副本」）。
- 删除：只允许 `draft` 且 `llm_nodes` / `llm_usage` 无引用；否则 `llm_model_in_use`。
- 校验：`slug ~ ^[a-z0-9][a-z0-9.-]{1,63}$`；`source_id ~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`；`node_env` 只接受 `vllm-node` spec 里声明且 **非 MODEL_ID / MODEL_SOURCE / SERVED_MODEL_NAME / MAX_MODEL_LEN / API_KEY** 的变量（这些由平台填），值经 `appdeploy.ValidateEnv` 校验；`image_id` 必须 enabled 且在 `vllm-node` 当前版本的 `image_ids` 内。

### 3.2 内部应用 `vllm-node`（`deploy/apps/vllm-node/`）
- `app.json`：`{"name": "vLLM 节点（平台内部）", "category": "llm", "icon": "llm", "internal": true, "version": "1.0.0", "images": ["LLM Serving"], …}`。
- `spec.json`：端口 `8000`（`health_path /health`，`path /docs`）、`timeout_minutes 120`、`min_data_free_gib 80`；变量：

| 变量 | 类型 | 说明 |
|---|---|---|
| `MODEL_ID` | string，`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`，必填 | 平台填 `source_id` |
| `MODEL_SOURCE` | enum `modelscope / hf-mirror` | 平台填 `source` |
| `SERVED_MODEL_NAME` | string，`^[a-z0-9][a-z0-9.-]{1,63}$`，必填 | 平台填 `slug`；vLLM `--served-model-name` |
| `MAX_MODEL_LEN` | int 1024～1048576 | 平台填 `context_len` |
| `API_KEY` | secret，`^[A-Za-z0-9_-]{32,128}$`，**必填** | 节点密钥（T13） |
| `GPU_MEMORY_UTILIZATION` | string `^0\.[3-9][0-9]?$`，默认 `0.85` | `node_env` 可覆盖 |
| `MAX_NUM_SEQS` | int 1～512，默认 64 | |
| `QUANTIZATION` | enum `none / fp8 / awq / gptq`，默认 `none` | `none` 不传参数 |
| `REASONING_PARSER` | enum `none / qwen3 / deepseek_r1`，默认 `none` | 非 none 传 `--reasoning-parser` |
| `TOOL_CALL_PARSER` | enum `none / hermes`，默认 `hermes` | 非 none 传 `--enable-auto-tool-choice --tool-call-parser` |
| `TASK` | enum `generate / embed`，默认 `generate` | `embed` 对应 `kind='embedding'`（`vllm serve --task embed`） |

- `app.sh`：以计划 7 的 `vllm/app.sh`（1.0.4）为基础：`install` 下载 `MODEL_ID`（进度估算改为按 `du` 增量，不再有模型大小常量表——未知模型按 `MODEL_SIZE_GB` 变量，缺省 20）；`start` 按上表拼参数，`--host 127.0.0.1 --port $SPARK_PORT --served-model-name "$SERVED_MODEL_NAME" --api-key "$API_KEY"`，日志轮转同 1.0.4。
- 由 `platform admin app import deploy/apps` 导入并在后台发布（与其它应用一样）；`Catalog.ListPublished` 过滤 `internal`。

### 3.3 池机组
- 迁移建好「Token 服务池」；后台机器池页就是该机组的机器列表 + 节点状态；加入 / 移出走现有 `POST/DELETE /groups/{id}/machines`（审计 `group.assign / unassign` 不变），但对 `purpose='llm'` 的机组：
  - 移出时若机器上有非终态节点 → `llm_pool_machine_busy`（409「机器上有运行中的节点，请先下线」）；
  - 加入带现有租约 / 实例的机器允许（租约结束后才会被 Controller 使用），页面显示「等待租约结束」。
- 可建节点的机器 = `machines.group_id = 池 AND state = 'idle' AND agent_online AND NOT EXISTS 活动实例 AND NOT EXISTS (machine_leases reserved/active 与 [now, now+10y) 相交) AND NOT EXISTS 非终态 llm_nodes`。

## 4. 系统实例（`rental` 的最小改动）

### 4.1 `PlaceSystem`
```go
type SystemPlaceRequest struct{ MachineID, ImageID int64; Note string }
func (s *Service) PlaceSystem(ctx context.Context, req SystemPlaceRequest) (Instance, error)
```
一个事务：锁机器行 `FOR NO KEY UPDATE` → 复核 §3.3 条件（不满足 → `ErrMachineUnavailable`）→ 插实例（`user_id = 系统账号`——`rental.Deps.SystemUserID` 由 `app.go` 从 `users WHERE system AND phone='system:llm'` 读出、`system = true`、`state = creating`、`pending_seconds = 0`、`expires_at NULL`、密码与主机密钥照常生成）→ 插 lease `active [now, now + SystemLeaseHorizon(10 年))` → `Fleet.MarkAllocated` → 事件 `"system instance for <note>"` → `Enqueue CreateInstance{image_ref}`。不查 `CanRent`、不建订单、不扣款、不调 `AppHooks.QueueInstall`（部署由 Controller 在 running 后创建）。

### 4.2 生命周期分支
| 位置 | 系统实例的行为 |
|---|---|
| `startLease`（Create 成功） | `state = running`，**不写 `expires_at`**（保持 NULL）；`refreshLease` 的 `occupiedUntil` 对 `system` 返回 `lower + SystemLeaseHorizon`（即不改）；照常调 `AppHooks.OnInstanceRunning`（节点重建 / 机器恢复时让部署回 queued） |
| `ExpireDue / SendReminders / AutoRenewDue` | 谓词天然不命中（`expires_at IS NULL`、`auto_renew=false`）；不改 |
| `SuspendMachines / ResumeMachine`（机器失联 / 恢复） | 只写 / 清 `suspended_at`，**不算 `paused_remaining`、不改 `expires_at`**（现有代码按 `expires_at − now` 算，对 NULL 要跳过）；节点侧据实例 `suspended_at` 置 `unhealthy` |
| `FailStuckCreating` / Create 失败 | 走现有 `failCreating`（`refundAll` 对无订单实例是空操作、机器 `faulty`、告警 `create_failed`）；节点 → `failed` |
| `ReleaseSystem(ctx, instanceID)`（新） | 与 `Release` 相同的状态机（running → 下发 Stop 后 `beginWipe`；creating → 取消指令并 wipe），但不校验用户、不要求 `state` 为 running（creating / stopped 也可）；由 Controller 调用 |
| 后台 `RefundRelease / AdminExtend / AdminMigrate / AdminReassign`、取消预订（`cancelInstance`）、用户 `Renew / SetAutoRenew / Cancel / Release` | `inst.System` → `ErrInvalidState`（用户路径本来就查不到系统实例，加判定只是防御） |
| 后台实例列表 / 详情 | 正常显示，`system=true`；筛选 `system=false` 默认隐藏？**不隐藏**（运维要看得到），标签「平台节点」 |
| 控制台 | 系统账号没有会话，看不到 |
| 接入层 | 系统实例的端口（appdeploy 登记的 8000）进路由表但只有系统账号能 authorize → 实际无人可访问；sshgw 对系统实例拒绝登录（`password_auth=false` 且无公钥——`PlaceSystem` 把 `password_auth` 置 false） |
| 对账 `reconcile.go` | 不变（容器真实存在） |
| 换机 `AdminMigrate` | 不允许（上表）；机器坏了由 Controller 建新节点 |

`rental.instanceView`（后台 `adminapi/views.go`）增 `system bool`；`instances` 列表筛选保持。

## 5. 节点生命周期（`llm.Controller`）

### 5.1 状态
```
provisioning ──(实例 running 且部署 running/health ok 且探测成功)──▶ ready ⇄ unhealthy（探测连续 3 次失败 / 部署非 running / 实例暂停）
     │                                                               │            │ 10 分钟 → Redeploy（rebuild_count+1）；3 次 → failed
     │  超时 ProvisionTimeout(150m) / 部署 failed / 实例 failed       ▼            ▼
     └──────────────────────────────────────────────────────────▶ failed      draining ──(在途 0 或 60s)──▶ removing ──(ReleaseSystem 已调用)──▶ removed
```
- 每次状态变化写 `llm_node_events`。
- `failed`：实例若仍存在则 `ReleaseSystem`（释放机器），`llm_nodes_machine_live_uniq` 把它排除在外；运维可「移除」（清理记录）或「重建」（新建一个节点行，不是复活）。

### 5.2 Controller 循环（每 `ControllerInterval = 10s`，`pg_try_advisory_lock(hashtext('llm-controller'))`，单副本工作）
每轮按顺序：
1. **期望对账**：对每个 `status='online'` 的模型：`want = replicas`，`have = count(nodes WHERE state IN (provisioning, ready, unhealthy))`。
   - `have < want`：挑 §3.3 可用机器（`ORDER BY id`，`FOR NO KEY UPDATE SKIP LOCKED`，一轮最多补 `MaxProvisionPerRound = 4` 个节点）→ 每个独立事务：生成节点密钥 → `PlaceSystem` → 插 `llm_nodes(provisioning)`。机器不够 → `RaiseAlertOnce(warning, llm_pool_short, "Token 服务池没有空闲机器，模型节点缺额")`（文案不含数值；现有告警都由后台人工「解决」，缺额补齐时**不自动解决**，与其它 `RaiseAlertOnce` 一致）。
   - `have > want`：按 `unhealthy` → `provisioning` → `ready`（`last_ready_at` 最早）的顺序挑 `have − want` 个 → `draining`。
   - `status ≠ online`：`want = 0`。
2. **推进 provisioning**：读实例与部署：
   - 实例 `creating` → 等；实例 `running` 且 `deployment_id IS NULL` → `appdeploy.Create(CreateRequest{UserID: 系统账号, InstanceID, AppID: vllm-node, VersionID: 模型的 app_version_id 或 nil, Env: {MODEL_ID, MODEL_SOURCE, SERVED_MODEL_NAME, MAX_MODEL_LEN, API_KEY, TASK} ∪ node_env, IP: ""})` → 记 `deployment_id`；
   - 部署 `running` 且 `health = ok` → 探测一次成功 → `ready`（`last_ready_at = now`，`probe_fail_count = 0`）；
   - 部署 `failed` 或实例 `failed / released / cancelled` → `failed`（`error` = 部署 `error_code` 中文 + message）；
   - `now − created_at > ProvisionTimeout(150m)` → `failed(timeout)`。
3. **unhealthy 处理**：`unhealthy_since + UnhealthyRebuildAfter(10m) ≤ now` 且 `rebuild_count < MaxRebuilds(3)` → `appdeploy.Redeploy(RedeployRequest{UserID: 系统账号, …, EnvSet: false})`、`rebuild_count + 1`、回 `provisioning`（保留 `instance_id / deployment_id`）；`rebuild_count ≥ 3` → `failed` + `RaiseAlert(critical, llm_node_failed, machine, instance, "节点 <id>（<slug>）多次重建失败")`。实例非 running（机器 faulty / 实例 failed）→ 直接 `failed`。
4. **draining → removing**：`inflight(node) == 0 OR drain_since + DrainTimeout(60s) ≤ now` → `ReleaseSystem` → `removing`；实例进入 `released`（洗机完成）→ `removed`。`inflight` 来自网关的进程内计数：Controller 与网关在同一进程（用户 API 进程）时直接读；不在同一进程时按 60 秒超时（README 注明）。
5. **removing 兜底**：实例 `released / failed` → `removed`。

### 5.3 健康探测（`llm.Prober`，与 Controller 同一 goroutine 组，每 `ProbeInterval = 10s`）
- 对 `ready / unhealthy / provisioning(部署 running)` 的节点并发（≤ 16）：经 `upstream.Pool.Dial(target, 8000)` 发 `GET /v1/models`（`Authorization: Bearer <节点密钥>`，5 秒超时）；2xx 且返回的 `data[].id` 含 `slug` → 成功。
- `ready` 连续 `ProbeFailThreshold = 3` 次失败 → `unhealthy`（`unhealthy_since = now`，`RaiseAlertOnce(warning, llm_node_unhealthy, machine, instance, "节点 <id>（<slug>）健康检查失败")`）；`unhealthy` 一次成功 → `ready`（`probe_fail_count = 0`，`unhealthy_since = NULL`）。
- 部署 `health = unhealthy` / `state ≠ running` 或实例 `suspended_at IS NOT NULL` 也算一次失败（不必等 3 次：部署 `failed` / `crashed` 立即 `unhealthy`）。
- 探测结果写 `last_probe_at`；网关的路由表（内存）每 2 秒从 `llm_nodes` 刷新一次 `ready` 节点列表（`llm.Router.Refresh`，与 ingress 路由表同节奏）。

### 5.4 与 rental / appdeploy 的衔接
- 机器失联 5 分钟 → `SuspendMachines` 置实例 `suspended_at` → 探测视为失败 → `unhealthy`；机器恢复 → `ResumeMachine` 清 `suspended_at` → appdeploy `OnInstanceRunning`… 注意 `ResumeMachine` 不触发 `OnInstanceRunning`（容器没停）；探测成功即 `ready`。
- 机器被运维置 `faulty` / 进入维护：`SetMachineMaintenance` 要求机器上没有未结束实例（现有规则）→ 对有节点的机器返回 `invalid_state`，页面提示「请先下线节点」。
- 后台「移出池」有节点 → 拒绝（§3.3）。
- 实例 `FailStuckCreating` → 节点 `failed` → Controller 下一轮按缺额在**另一台**机器建新节点（坏机器已 `faulty`）。

## 6. 网关（`internal/llmgw`）

### 6.1 请求处理流程（`POST /v1/chat/completions` 为例）
1. **入口**：`/v1/*` 挂在 `api.Server` 的路由（§10.3）；设置 `X-Request-Id = req-<16 位>`；CORS 预检直接 200；`Content-Type` 必须 `application/json`（否则 400 `invalid_request_error`）；`http.MaxBytesReader(2 MiB)`（超出 413 `request_too_large`）；`http.ResponseController.SetWriteDeadline(now + TotalTimeout(10m))`、`SetReadDeadline(now + 60s)`（公网服务器的 60 秒 WriteTimeout 对流式不够）。
2. **鉴权**：`Authorization: Bearer sk-spark-…` → `sha256` → `llm.KeyCache.Lookup(hash)`（30 秒 TTL，缓存 `{key, user status, kyc, account}`，未命中查库）；无 / 格式错 / 不存在 / `status ≠ active` / 已过期 → 401 `invalid_api_key`；按 IP 对 401 限流 60 次 / 分钟（复用 `ipLimiter`），超限 429。
3. **账户门槛**：用户 `banned` → 403 `account_banned`；`kyc_status ≠ verified` → 403 `kyc_required`；`llm_accounts.blocked` → 403 `account_blocked`（消息带原因）；`debt > 0` → 402 `insufficient_quota`（消息「账户有欠费 ¥X，请充值」）。
4. **解析**：读 body 为 JSON，取 `model`（必须字符串）、`stream`（bool）、`max_tokens` / `max_completion_tokens`、`stream_options`；其它字段不解析不校验。`model` 不在该 Key 的 `model_slugs`（非空时）→ 403 `model_not_allowed`；不是 `online` 模型 → 404 `model_not_found`；端点与 `kind` 不匹配（chat 模型调 embeddings 或反之）→ 400 `invalid_request_error`。
5. **限流**（`llm.Limiter`，按 `user_id`）：并发 `> concurrency_limit` → 429 `rate_limit_exceeded`（`Retry-After: 1`）；RPM 滑动窗口 → 429（`Retry-After` = 窗口剩余秒数）；TPM：过去 60 秒已计 token + 本次估算 `> tpm_limit` → 429。Key 的 `monthly_cap`：`month_cost_micro / 1e6 ≥ cap` → 429 `monthly_cap_exceeded`。
6. **预算预检**（T2）：读 `wallet.Balance`（2 秒进程内缓存）与 `llm_accounts`（随 Key 缓存）；`available < max(est, 1)` → 402 `insufficient_quota`（消息「余额不足，本次预估最多 ¥X，可用 ¥Y」）；通过则 `inflight_estimate[user] += est`（defer 减回）。
7. **选节点**（`llm.Router.Pick(modelID, keyID)`）：`ready` 节点中优先 `lastNode[keyID]`（且其在途 < 0.8 × `max_concurrency`），否则最少在途；所有节点在途都 ≥ `max_concurrency` → 503 `server_overloaded`（`Retry-After: 2`）；没有 `ready` 节点 → 503 `model_unavailable`（「模型暂无可用节点，请稍后再试」）。
8. **转发**：`http.Transport{DialContext: func → upstream.Pool.Dial(target, 8000)}`（每节点一个 Transport，`MaxIdleConnsPerHost 64`，`ResponseHeaderTimeout = TTFTTimeout(120s)`）；请求：方法 / 路径不变、body 为**改写后的 JSON**（只在流式且未带 `stream_options.include_usage` 时注入 `"stream_options":{"include_usage":true}`，其余字节不动——用 `json.RawMessage` 字段级补丁而非整体重编码：解析成 `map[string]json.RawMessage` 再编码，字段顺序变化无害）；头：`Content-Type`、`Accept`、`Authorization: Bearer <节点密钥>`、`X-Request-Id`；不转发客户端的其它头。
9. **失败转移**：`Dial` 错误（`*upstream.DialError`）/ 连接被拒 / 上游 502、503 且**尚未向客户端写任何字节** → 该节点 `probe_fail_count + 1`（≥ 3 → 由 Prober 处理）、`Router.Pick` 排除它再试**一次**；第二次仍失败 → 502 `upstream_error`，用量行 `status = upstream_error, cost_micro = 0`。
10. **响应**：
    - 非流式：读完上游 body（≤ 8 MiB）→ 透传状态码、`Content-Type`、body；2xx 时解析 `usage.prompt_tokens / completion_tokens`（embeddings 只有 prompt）；上游 4xx 原样透传且 `cost_micro = 0`、`status = ok`（用户请求错误不收费也不算平台错误）；上游 5xx → `upstream_error`。
    - 流式：`Content-Type: text/event-stream`、`X-Accel-Buffering: no`，逐 SSE 事件透传并 `Flush`；解析每个 `data:` 行：含 `choices[].delta.content` 或 `delta.reasoning_content` 非空的块计数 `chunks`；含 `usage` 的块记录 usage；`[DONE]` 结束。客户端断开（`r.Context()` 取消）→ 取消上游请求，`status = client_abort`，按 T12 估算。上游在首字节后断开 → `upstream_error`，`cost_micro = 0`，向客户端写一个 `data: {"error":…}` 事件后结束。
11. **记账**（事务，`llm.RecordUsage`）：`llm_accounts FOR UPDATE`（不存在则插入）→ 插 `llm_usage` → `unsettled_micro += cost_micro` → `llm_api_keys SET last_used_at, month_cost_micro（跨月重置）`；`Limiter.AddTokens(user, prompt + completion)`；`lastNode[keyID] = node`。记账失败只记日志（`llm: record usage failed`）并 `RaiseAlertOnce(critical, llm_usage_record_failed, …)`——**不向用户返回错误**（响应已经发出）。

### 6.2 `GET /v1/models`
鉴权 + 门槛后返回 `{"object":"list","data":[{"id": slug, "object":"model", "created": unix, "owned_by":"spark-cloud"}]}`，只含 `online` 且 Key 允许的模型（不要求有 ready 节点）。

### 6.3 错误格式
```json
{"error": {"message": "余额不足，本次预估最多 ¥0.12，可用 ¥0.03", "type": "insufficient_quota", "code": "insufficient_quota"}}
```
`type ∈ invalid_request_error / authentication_error / permission_error / insufficient_quota / rate_limit_error / server_error`；`code` 见 §6.1 各步。`apperr` 不用于 `/v1`。

### 6.4 用量日志的字段来源
| 字段 | 来源 |
|---|---|
| `prompt_tokens / completion_tokens` | 上游 `usage`；缺失（理论不会）或客户端中断 → 估算，`estimated = true` |
| `cost_micro` | `prompt × input_price + completion × output_price`（embeddings 只算输入）；`status ≠ ok` 且非 `client_abort` → 0 |
| `latency_ms / ttft_ms` | 网关计时 |
| `node_id` | 最终转发到的节点 |
| `http_status` | 返回给客户端的状态码 |

## 7. 结算（`llm.Settle`，清扫附加任务）

```
候选：llm_accounts WHERE unsettled_micro > 0 OR debt > 0（LIMIT 500，按 updated_at）
每个账户独立事务（40P01 / 40001 重试一次）：
  a  := llm_accounts FOR UPDATE
  oldest := min(created_at) of llm_usage WHERE user_id AND settlement_id IS NULL
  due_usage := (a.unsettled_micro >= SettleMinMicro) OR (oldest <= now − SettleMaxAge(10m))
  if !due_usage && !(a.debt > 0 && Wallet.Balance(user) > 0): 跳过
  插 llm_settlements(user, 0...) 得 sid
  if due_usage:
    UPDATE llm_usage SET settlement_id = sid WHERE user_id = $1 AND settlement_id IS NULL AND id <= $max_id  -- max_id 为锁后读到的最大 id，避免纳入事务中并发插入的行
      RETURNING cost_micro, prompt_tokens, completion_tokens, created_at, model_id, day
    sum := Σ cost_micro；校验 sum == a.unsettled_micro（不等 → 记 Error、以 sum 为准并把 unsettled_micro 置 0）
    total := sum + a.carry_micro；usage_amount := total / 1e6；carry' := total % 1e6
    UPSERT llm_usage_daily(day, user, model) += …
  debt_amount := a.debt
  due := usage_amount + debt_amount
  if due > 0:
    applied, _ := Wallet.ApplyUpTo(tx, Entry{UserID, Amount: −due, Kind: llm_pay, RefType: 'llm_settlement', RefID: sid, Remark: "Token 消费 N 次请求 · 输入 X · 输出 Y token"})
    debt' := due − applied
  UPDATE llm_settlements SET requests, tokens, cost_micro = sum, usage_amount, debt_amount, charged = applied, wallet_ledger_id, period_from/to
  UPDATE llm_accounts SET unsettled_micro = 0（或减 sum）, carry_micro = carry', debt = debt'
  debt' > 0 且 debt 之前为 0 → 事件日志 + 用户控制台顶部提示（由 /llm/account 返回 debt）
```
- 锁顺序：`llm_accounts(user) → llm_usage → llm_settlements → wallet_accounts(user)`；网关记账是 `llm_accounts(user) → llm_usage → llm_api_keys`，同序；rental / billing / referral 只锁 `wallet_accounts`（不碰 llm 表）→ 等待链不成环。
- `wallet_ledger_ref_uniq (kind, ref_type, ref_id)` 以 `sid` 唯一 → 重放不会重复扣款。
- **清理**：同一附加任务里 `DELETE FROM llm_usage WHERE created_at < now − 90d AND settlement_id IS NOT NULL LIMIT 5000`（每轮一批，`DELETE … WHERE id IN (SELECT … LIMIT)`）。
- 接入：`main.go` → `a.Rental.RunSweeper(ctx, cfg.SweepInterval, a.Billing.Sweep, a.Referral.Settle, a.LLM.Settle)`；`e2ehooks` 的 `sweep` 同步追加。

## 8. 与现有行为的交互

| 场景 | 行为 |
|---|---|
| 用户被封禁 | 30 秒内网关 403；已在途的请求完成并记账；结算照常扣 |
| 实名被撤销 / 从未实名 | 403 `kyc_required`；不能建 Key |
| 余额为 0 但无欠费 | 预检 402；不产生用量行 |
| 并发打穿余额 | 结算 `ApplyUpTo` 扣到 0，`debt` 记差额；之后全部 402 直到充值；充值后下一轮结算先扣欠费 |
| 后台调账 / 充值 | 不参与结算逻辑；只影响余额 |
| 邀请返利（计划 8） | `llm_pay` 的 `ref_type ≠ 'order'`，扫描不命中；不计返利（遗留） |
| 机器租用 | 池内机器对租用不可见（计划 6 B9）；`GET /groups` 不返回池；`pickMachines` 不选池内机器；`AdminMigrate / reassign` 指定池内机器 → 候选集不含 → `machine_unavailable` |
| 机器进入维护 | 有系统实例 → `invalid_state`（现有规则），页面提示先下线节点 |
| 机器失联 | 实例 `suspended_at` → 节点 `unhealthy`（不路由）；恢复后探测成功即 `ready`；超过 10 分钟不恢复 → 重建（`Redeploy`，机器仍失联则部署卡在 queued，Controller 继续等；`rebuild_count` 只在真正执行了 Redeploy 时 +1） |
| platform 重启 | 节点状态在库里；网关路由表 2 秒内重建；在途计数清零（`draining` 节点最多多等 60 秒）；Key 缓存冷启动 |
| 一键部署（计划 7）用户视角 | `vllm-node` 不在市场页；用户自己的 vLLM 应用照旧 |
| 后台用户列表 / 用户详情 | 系统账号默认隐藏；用户详情增「Token 商城」区块 |
| 后台流水页 | `llm_pay` 可筛、可导出；中文「Token 消费」 |
| 仪表盘 | 增 `llm` 卡片组：就绪节点 / 总节点、今日请求数、今日 token、今日收入（已结算 `usage_amount` 合计 + 未结算估算，分开显示） |

## 9. 控制台页面

### 9.1 入口
- `ConsoleLayout.vue` 导航加第 5 项 `Token 商城 → /tokens`（底部导航 5 项，图标 `ChatDotRound`）。
- 市场页 `/market` 顶部加一张横幅卡片「Token 商城 · 按量调用大模型 API，¥X 起 / 百万 token」→ `/tokens`（取最低 `input_price`）。

### 9.2 `/tokens`（需登录；未登录可看模型与价格，其余区块提示登录）
Tab：
1. **模型与价格**：卡片列表（`name`、`slug` 可复制、上下文 `context_len`、`features` 标签、输入 / 输出价（元 / 百万 token，两位小数）、状态「可用」/「暂无节点」）；顶部说明「按实际 token 计费，从钱包余额扣除，每次结算一条流水」。
2. **API Keys**：表格（名称、Key 提示、允许模型、月上限 / 本月已用、最近使用、到期、状态）；「创建 Key」对话框（名称必填、模型多选、月上限、到期）→ 成功后对话框**只显示一次**明文 Key + 复制按钮 + 「关闭后不再显示」；行操作：停用 / 启用、编辑（名称 / 模型 / 月上限）、撤销（二次确认）。未实名 → 创建按钮禁用并引导 `/kyc`。
3. **用量**：顶部统计（今日 / 本月消费、本月 token、未结算金额、欠费提示条（红，带「去充值」））；按天 × 模型表格（最近 30 天，`llm_usage_daily`）；「结算记录」子表（时间、请求数、token、金额、流水链接到 `/wallet`）。
4. **请求日志**：分页表（时间、模型、Key、端点、流式、输入 / 输出 token（估算带 `≈`）、费用、状态、延迟、`request_id` 可复制）；状态中文：成功 / 客户端中断 / 上游错误 / 上游超时 / 无可用节点。
5. **快速开始**：`base_url`（`https://<domain>/v1`）、curl 与 Python（openai SDK）示例（Key 用占位符 `sk-spark-xxxx`，模型用第一个在线模型）、说明：流式总带 usage 块、限额、错误码表、欠费规则。

### 9.3 其它
- `/wallet`：流水类型「Token 消费」标签（颜色同扣款）。
- `/help`：新增一节「Token 商城」（计费、结算、限额、Key 安全、常见错误）。
- `/account`：入口「Token 商城」小字「本月 ¥X」。

## 10. API

### 10.1 控制台（`/api/v1`）

错误码（`apperr`）新增：`llm_model_not_found`（404「模型不存在」）、`llm_model_slug_exists`（409「模型标识已存在」）、`llm_model_invalid`（400，消息按字段）、`llm_model_in_use`（409「模型仍有节点或用量记录，不能删除」）、`llm_key_not_found`（404「API Key 不存在」）、`llm_key_limit`（409「最多创建 20 个 API Key」）、`llm_key_invalid`（400，消息按字段）、`llm_node_not_found`（404）、`llm_node_invalid_state`（409「节点当前状态不允许该操作」）、`llm_pool_machine_busy`（409「机器上有运行中的节点，请先下线」）、`llm_account_invalid`（400）。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/llm/models` | 匿名（`calendarRateLimit` 组）：`{items:[{slug, name, description, kind, context_len, max_output_tokens, input_price, output_price, features, available}], base_url}`，只含 `online`；`available = ready 节点 > 0` |
| GET | `/llm/account` | `{debt, unsettled, today_cost, month_cost, month_tokens, limits:{rpm, tpm, concurrency}, blocked, blocked_reason, kyc_verified, base_url}`（金额单位分；`unsettled = ceil(unsettled_micro/1e6)`） |
| GET | `/llm/keys` | `{items:[{id, name, key_hint, status, model_slugs, monthly_cap, month_cost, expires_at, last_used_at, created_at}]}`（不含 revoked 超过 30 天的） |
| POST | `/llm/keys` | `{name, model_slugs?, monthly_cap?, expires_at?}` → 201 `{key:"sk-spark-…", item}`；要求 `CanRent`；上限 20（`llm_key_limit`）；审计 `llm.key.create`（user actor，不含明文） |
| PUT | `/llm/keys/{id}` | `{name?, status?(active/disabled), model_slugs?, monthly_cap?}`；审计 `llm.key.update` |
| DELETE | `/llm/keys/{id}` | 撤销（`revoked`，`revoked_at`）；审计 `llm.key.revoke` |
| GET | `/llm/usage?from=&to=&page=&model=&key_id=` | 分页明细 `{items:[{request_id, created_at, model, key_name, endpoint, stream, prompt_tokens, completion_tokens, estimated, cost_micro, status, http_status, latency_ms, ttft_ms}], total}`；默认最近 7 天，窗口 ≤ 90 天 |
| GET | `/llm/usage/daily?days=30` | `{items:[{day, model, requests, prompt_tokens, completion_tokens, cost_micro}]}` |
| GET | `/llm/settlements?page=` | `{items:[{id, created_at, requests, prompt_tokens, completion_tokens, usage_amount, debt_amount, charged, wallet_ledger_id, period_from, period_to}], total}` |
| GET | `/me` | 增 `llm_month_cost`（分，0 时省略） |

### 10.2 后台（`/admin/api/v1`）

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| GET | `/llm/models` | all | 全部模型 + 每个的节点计数 `{ready, total}` |
| POST | `/llm/models` | super | `{slug, name, description, kind, source, source_id, image_id, app_version_id?, context_len, max_output_tokens, default_max_tokens, input_price, output_price, node_env, replicas, max_concurrency, features, sort}`；审计 `llm.model.create` |
| PUT | `/llm/models/{id}` | super | 同上（不含 slug）；审计 `llm.model.update`（before / after；价格变化在 diff 里） |
| POST | `/llm/models/{id}/status` | super + TOTP | `{status, totp}`：`online` 要 `replicas ≥ 1`；审计 `llm.model.status` |
| DELETE | `/llm/models/{id}` | super | 条件见 §3.1；审计 `llm.model.delete` |
| GET | `/llm/pool` | ops | `{group_id, machines:[{machine, node?:{id, model_slug, state}, lease_until?}], free, total}` |
| GET | `/llm/nodes?model_id=&state=` | ops | 节点列表（含机器展示名、实例 ID、部署状态 / health / progress、`inflight`、最近错误） |
| GET | `/llm/nodes/{id}` | ops | 详情 + 最近 50 条 `llm_node_events` + 部署事件 |
| POST | `/llm/nodes/{id}/drain` | ops | `ready / unhealthy / provisioning → draining`；审计 `llm.node.drain` |
| POST | `/llm/nodes/{id}/rebuild` | ops | `ready / unhealthy → provisioning`（Redeploy，`rebuild_count` 不计入自动上限）；`failed` → 新建节点行（同机器若仍可用）；审计 `llm.node.rebuild` |
| POST | `/llm/nodes/{id}/remove` | ops + TOTP | 任何状态 → 立即 `ReleaseSystem` + `removing`（不等在途）；`{reason, totp}`；审计 `llm.node.remove` |
| GET | `/llm/accounts?q=&blocked=&debt=&page=` | finance | 每用户：手机号、Key 数、本月消费、未结算、欠费、限额、停用 |
| PUT | `/llm/accounts/{user_id}` | ops | `{rpm_limit?, tpm_limit?, concurrency_limit?, blocked?, blocked_reason?}`（null 恢复默认）；审计 `llm.account.update` |
| GET | `/llm/keys?user_id=` | ops | 某用户的 Key（只有 hint）|
| POST | `/llm/keys/{id}/disable` | ops | `{reason}`；审计 `llm.key.admin_disable` |
| GET | `/llm/usage?user_id=&model_id=&key_id=&status=&from=&to=&page=` | finance | 明细 |
| GET | `/llm/usage/export?…` | finance | CSV（请求 ID、时间、用户、模型、Key、端点、流式、输入 / 输出 token、估算、单价、费用（元，6 位小数）、状态、延迟、节点）；`csvx.MaxRows` |
| GET | `/llm/usage/daily?from=&to=&group_by=model\|user` | finance | 汇总 |
| GET | `/llm/settlements?user_id=&from=&to=&page=` | finance | 结算记录 |
| GET | `/llm/summary` | all | 仪表盘卡片数据：`nodes:{ready,total,unhealthy}`、`today:{requests, prompt_tokens, completion_tokens, settled_amount, unsettled_estimate}`、`debt_total`、`debt_users` |
| GET | `/users/{id}` | all | 响应增 `llm:{keys, month_cost, unsettled, debt, blocked}` |
| GET | `/users` | all | 增筛选 `include_system`（默认 false） |
| GET | `/instances*` | all | 实例视图增 `system`；筛选保持 |
| GET | `/ledger` | finance | `kind` 接受 `llm_pay` |
| GET | `/dashboard` | all | 增 `llm` 字段（= `/llm/summary` 的子集：`nodes_ready / nodes_total / today_requests / today_settled_amount`） |
| GET | `/apps` | all | 应用行增 `internal` |
| DELETE | `/groups/{id}/machines` | ops | 对池机组：机器有非终态节点 → `llm_pool_machine_busy` |
| PUT | `/groups/{id}` | ops | 池机组：`visible=true` 或 `purpose` 变更 → `invalid_state`；DELETE 池机组 → `invalid_state` |

### 10.3 OpenAI 兼容网关（`/v1`，同一进程、同一监听）
| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/models` | §6.2 |
| POST | `/v1/chat/completions` | §6.1 |
| POST | `/v1/completions` | 同 §6.1，`endpoint = completions` |
| POST | `/v1/embeddings` | 仅 `kind='embedding'`，只计输入 |
| OPTIONS | `/v1/*` | CORS 预检 |
| 其它 | `/v1/*` | 404 `{"error":{"type":"invalid_request_error","code":"not_found"}}` |

挂载：`api.Server` 增字段 `LLMGateway http.Handler`（非 nil 时 `r.Mount("/v1", s.LLMGateway)`）；`requireJSONForWrites` 与 `SameSiteGuard` 的 exempt 增加 `strings.HasPrefix(path, "/v1/")`；`ProxyRealIP` / `Recoverer` / `RequestID` 照常。配置 `SPARK_LLM_GATEWAY=off` 时不挂载（返回 404）。

## 11. 后台页面
- 菜单新增 `Token 商城 → /llm`（`ALL_ROLES`；页内 Tab 按角色显示）；`menu.spec.ts` 同步。
- `/llm` Tab：
  - **总览**（all）：节点就绪 / 总数、今日请求 / token / 收入、欠费用户数；按模型的节点卡片。
  - **模型**（all 看，super 改）：表格（slug、名称、类型、价格、上下文、副本 期望 / 就绪、状态）；新建 / 编辑抽屉（结构化表单；`node_env` 按 `vllm-node` spec 渲染成字段）；上下线（TOTP）；删除。
  - **节点与机器池**（ops）：机器池表（机器展示名 / 序列号 / 状态 / 在线 / 节点 / 租约到期）+「加入机器」对话框（选不在池里的机器，调用现有 `POST /groups/{id}/machines`）+「移出」；节点表（模型、机器、状态、健康、在途、部署进度、最近错误、重建次数）+ 行操作 排空 / 重建 / 移除（TOTP）；节点详情抽屉：事件时间线 + 部署日志（复用计划 7 的后台部署日志接口 `GET /deployments/{id}`）。
  - **账户**（ops / finance）：搜索、欠费筛选；编辑限额 / 停用（ops）；查看 Key（hint）并停用。
  - **用量**（finance）：筛选 + 明细表 + CSV；按天 / 按模型 / 按用户汇总切换；结算记录表。
- `/users/:id`：「Token 商城」区块（Key 数、本月消费、未结算、欠费、停用状态，链接到 `/llm?tab=accounts&user_id=`）。
- `/instances`：`system` 实例显示「平台节点」标签；详情页隐藏退款 / 延长 / 换机按钮。
- `/machines/:id`：机器属于池机组时显示「Token 服务池」与节点链接。
- `/ledger`：类型筛选加「Token 消费」。
- 仪表盘：「Token 商城」卡片（节点、今日请求、今日收入）。
- `/catalog`（应用目录，计划 7）：internal 应用显示标签。

## 12. 模块与接口

新包 `internal/llm`（业务核心）与 `internal/llmgw`（HTTP 网关）：

```go
// internal/llm
type Deps struct {
    Pool     *pgxpool.Pool
    Clock    clock.Clock
    Log      *slog.Logger
    Box      *secretbox.Box
    Wallet   WalletOps        // Balance / ApplyUpTo
    Users    UserLookup       // CanRent(ctx, id) error；UserState(ctx, q, id)（计划 8 已有）
    Rental   RentalOps        // PlaceSystem / ReleaseSystem / Get(instance) / UpstreamTarget(instanceID) (upstream.Target, error)
    Apps     AppOps           // Create / Redeploy / Get（appdeploy.Service）
    AppCat   AppCatalog       // AppBySlug / LatestPublished / GetVersion（appdeploy.Catalog）
    Images   ImageLookup      // GetImage(ctx, q, id)（catalog.Service，校验模型镜像）
    Fleet    FleetOps         // LLMPoolGroupID / Get(machine) / RaiseAlert / RaiseAlertOnce
    Upstream *upstream.Pool   // 探测与转发
    SystemUserID int64        // app.go 从 users 读出
}
func NewService(d Deps) *Service
// 模型 / Key / 账户 / 用量（控制台 + 后台）
func (s *Service) ListOnlineModels(ctx) ([]ModelView, error)
func (s *Service) CreateKey(ctx, userID int64, in KeyInput, ip string) (plain string, k Key, err error)
func (s *Service) UpdateKey / RevokeKey / ListKeys(...)
func (s *Service) Account(ctx, userID) (AccountView, error)
func (s *Service) ListUsage / DailyUsage / ListSettlements(...)
func (s *Service) Admin*(...)                                          // §10.2
// 网关用
func (s *Service) Authorize(ctx, keyHash string, now time.Time) (AuthContext, error)   // 带 30 秒缓存
func (s *Service) RecordUsage(ctx, u Usage) error
func (s *Service) Router() *Router                                     // ready 节点表（2 秒刷新）；Pick / Release / Inflight
func (s *Service) Limiter() *Limiter
// 后台任务
func (s *Service) Settle(ctx) error                                    // 清扫附加任务
func NewController(s *Service) *Controller; func (c *Controller) Run(ctx)   // 对账 + 探测
func (s *Service) CheckInvariants(ctx, pool) error                     // §14

// internal/llmgw
type Config struct{ MaxBodyBytes int64; TotalTimeout, TTFTTimeout time.Duration; AuthFailLimit int }
func New(svc *llm.Service, cfg Config, log *slog.Logger) *Server
func (g *Server) Router() http.Handler
```
- `rental`：`SystemPlaceRequest / PlaceSystem / ReleaseSystem`、`Instance.System`、`Deps.SystemUserID`、各分支（§4.2）；`admin_queries` 的 `InstanceFilter` 不变。
- `fleet`：`Group.Purpose`、`LLMPoolGroupID`、`UpdateGroup / DeleteGroupIn` 对池的校验；`AssignMachines / UnassignMachines` 不变（移出前的节点检查在 `adminapi` 的 handler 里调 `LLM.MachineHasLiveNode`）。
- `appdeploy`：`App.Internal`、`ListPublished` 过滤、`Create` 对 internal 应用的用户校验、importer 读 `internal`。
- `wallet`：`KindLLMPay`、`ledgerKinds`。
- `identity`：`UserState` 已有；后台用户列表 `include_system`；`Ban` 对 system 用户 `ErrInvalidState`。
- `api`：`llm.go`（控制台端点）、`server.go`（`LLM *llm.Service`、`LLMGateway http.Handler`、exempt、Mount）。
- `adminapi`：`llm.go`、`permissions_test.go`、`groups.go` 的池校验、`views.go` 的 `system`、`dashboard.go`、`users.go`。
- `app.go`：装配、`SystemUserID` 查询、`LLMGateway`；`main.go`：`Settle` 入清扫、Controller goroutine（`cfg.LLMController`）。
- `e2ehooks`：`sweep` 调 `LLM.Settle`；新增 `POST /hooks/llm/upstream {port}`（在 sshtest 假容器上挂假 vLLM）、`POST /hooks/llm/node {model_id, machine_id}`（直接造 `ready` 节点，跳过 provisioning）、`POST /hooks/llm/tick`（跑一轮 Controller + Prober）。
- `internal/llmgw/llmtest`：`FakeVLLM`（`http.Handler`：`/v1/models`、`/v1/chat/completions` 流式 / 非流式带 usage、可注入延迟 / 错误 / 断开），Go 集成测试与 e2e 共用。

## 13. 配置与常量

| 项 | 值 | 说明 |
|---|---|---|
| `DefaultRPM / DefaultTPM / DefaultConcurrency` | 60 / 200,000 / 8 | T4，按用户 |
| `DefaultNodeConcurrency` | 32 | `llm_models.max_concurrency` 默认 |
| `MaxBodyBytes` | 2 MiB | |
| `MaxUpstreamBodyBytes` | 8 MiB | 非流式响应 |
| `TotalTimeout / TTFTTimeout` | 10m / 120s | |
| `KeyCacheTTL / BalanceCacheTTL` | 30s / 2s | |
| `AuthFailLimitPerIP` | 60 / 分钟 | 401 限流 |
| `MaxKeysPerUser` | 20 | |
| `SettleMinMicro / SettleMaxAge` | 100 × 10^6（¥1）/ 10m | T3 |
| `UsageRetention` | 90 天 | T18 |
| `ControllerInterval / ProbeInterval / ProbeTimeout` | 10s / 10s / 5s | |
| `ProbeFailThreshold` | 3 | |
| `UnhealthyRebuildAfter / MaxRebuilds` | 10m / 3 | T10 |
| `ProvisionTimeout` | 150m | 含 120 分钟部署超时 |
| `DrainTimeout` | 60s | T9 |
| `MaxProvisionPerRound` | 4 | 每轮最多建节点数 |
| `SystemLeaseHorizon` | 10 年 | 系统实例 lease |
| `RouterRefresh` | 2s | 节点表刷新 |
| `SPARK_LLM_GATEWAY` | 环境变量 `on / off`，默认 on | off 时不挂 `/v1` |
| `SPARK_LLM_CONTROLLER` | 环境变量 `on / off`，默认 on | 与 `SPARK_APP_RUNNER` 同模式；e2e-server 关闭，用钩子驱动 |

常量在 `internal/llm/consts.go`；价格、限额、副本数是数据库里的运营参数。

## 14. 并发与正确性

- **资金只经 `wallet.ApplyUpTo`**（结算）；网关从不写钱包。每次结算一个唯一 `ref_id`（结算 ID），`wallet_ledger_ref_uniq` 兜底重放。
- **加锁顺序**（写进 `internal/llm/llm.go` 头注释）：

  | 路径 | 顺序 |
  |---|---|
  | 网关记账 `RecordUsage` | `llm_accounts(user) FOR UPDATE` → 插 `llm_usage` → `llm_api_keys(key)` |
  | 结算 `Settle` | `llm_accounts(user) FOR UPDATE` → `llm_usage`（UPDATE 标记） → `llm_settlements` → `llm_usage_daily` → `wallet_accounts(user)` |
  | 建节点 | `machines(FOR NO KEY UPDATE SKIP LOCKED)` → 插 `instances` / `machine_leases`（`PlaceSystem`）→ 插 `llm_nodes` |
  | Controller 推进 / Prober 回写 | `llm_nodes(node) FOR UPDATE` → [appdeploy / rental 调用在事务外] |
  | 后台模型 / Key / 账户操作 | 单表行锁 |
  | rental / billing / referral | 不碰 llm 表 |

  `llm_accounts` 之后只锁更细的行与 `wallet_accounts`，而其它模块只锁 `wallet_accounts` → 不成环。Controller 对 appdeploy / rental 的调用在事务外（它们各自开事务），`llm_nodes` 行锁不与 `instances` 行锁嵌套。
- **不变量**（`llm.CheckInvariants`，集成测试与真机验收后执行）：
  1. 每个用户：`llm_accounts.unsettled_micro == Σ llm_usage.cost_micro WHERE settlement_id IS NULL`。
  2. 每个用户：`Σ llm_settlements.usage_amount × 10^6 + carry_micro == Σ llm_usage.cost_micro WHERE settlement_id IS NOT NULL`。
  3. 每个用户：`Σ |wallet_ledger(llm_pay).amount| == Σ llm_settlements.charged`；`debt == Σ (usage_amount + debt_amount − charged) − Σ debt_amount`（即 `debt == Σ usage_amount − Σ charged`）。
  4. 每条结算：`charged ≤ usage_amount + debt_amount`；`wallet_ledger_id IS NULL ⇔ charged = 0`。
  5. `llm_usage_daily` 各列 == 已结算 `llm_usage` 按 (day, user, model) 的聚合（90 天内可核对）。
  6. 每台机器最多一个非终态节点（唯一索引）；每个非终态节点的实例非终态且 `system=true`；`ready` 节点的部署 `running`。
  7. 池内机器没有非系统实例的活动 lease 与节点同时存在（节点 lease 是系统实例的）。
- **估算 vs 实际**：预检用估算，记账用实际；并发可打穿 → 欠费兜底（§7）。
- **幂等**：`request_id` 唯一；记账失败只告警不重试（避免双记）；结算的 `max_id` 边界避免纳入事务期间新插的行。
- **时钟**：全部 `clock.Clock`；月份与自然日按 `Asia/Shanghai`（`internal/platform/tz`）。
- **重试**：结算单账户 / 节点事务遇 `40P01 / 40001` 重试一次。

## 15. 迁移与兼容
1. 迁移 00022：无存量回填；`users.system` 行与池机组由迁移写入。合并前核对 `main` 已含 00020 / 00021（goose 不接受乱序）。
2. 发布顺序：platform 单独发布（不改 Agent 协议；节点依赖计划 7 的新版 Agent 与 `image.env`，计划 7 上线时已满足）。
3. `vllm-node` 应用：上线后 `platform admin app import deploy/apps` 导入并发布；镜像 `LLM Serving` 已登记（计划 7）。
4. 回滚：Down 在有 `llm_pay` 流水或活动节点时拒绝；先在后台把所有模型 `offline`、等节点排空再 Down。
5. 多副本：网关限额按副本各算（T15）；Controller / Settle 咨询锁单副本。

## 16. 测试与验收

### 16.1 单元（Go，嵌入式 PG，假时钟）
- 迁移：Up / Down 往返；Down 在有 `llm_pay` 流水或活动节点时拒绝；池机组唯一索引。
- `wallet`：`KindLLMPay` 通过 `LedgerFilter`；`ApplyUpTo` 已有测试不变。
- `rental`：`PlaceSystem` 条件矩阵（非池机组 / 非 idle / 离线 / 有活动实例 / 有重叠 lease → `machine_unavailable`）；成功后 `system=true`、无订单、lease 10 年、`password_auth=false`；`startLease` 不写 `expires_at`；`ExpireDue / SendReminders / AutoRenewDue` 不命中；`Suspend / Resume` 不碰 `expires_at`；`ReleaseSystem` 从 running / creating / stopped 都进 wiping，洗机后机器 idle；`RefundRelease / AdminExtend / AdminMigrate` 对系统实例 `invalid_state`；`pickMachines` 不选池内机器（即时与预订）。
- `fleet`：池机组 `visible=true` / 改 `purpose` / 删除 → `invalid_state`；`LLMPoolGroupID`。
- `appdeploy`：`internal` 应用不在 `ListPublished`；用户 `Create` 对 internal → `app_not_found`；系统账号可以；importer 读 `internal`。
- `llm/models`：校验矩阵（slug、source_id、node_env 白名单、image 兼容、online 要求 replicas ≥ 1）；删除条件；价格更新审计 diff。
- `llm/keys`：生成格式与熵、哈希、hint、20 上限、`CanRent` 门槛、撤销后 `Authorize` 失败、过期、`disabled`、`model_slugs` 过滤、月上限跨月重置（Asia/Shanghai）。
- `llm/authorize`：缓存 TTL（撤销后 30 秒内旧结果、之后失效）、封禁 / 未实名 / blocked / debt 的错误码。
- `llm/limiter`：RPM 边界、TPM 含估算、并发释放、后台限额覆盖、`Retry-After` 计算。
- `llm/router`：优先同 Key 节点、80% 切换、最少在途、排除节点、无节点 / 全满的区分；2 秒刷新。
- `llm/usage`：`cost_micro` 计算（含 embeddings 只算输入、估算路径、失败 0）；`RecordUsage` 更新 `unsettled_micro` 与 Key 月消费；并发 50 次记账后不变量 1 成立。
- `llm/settle`：阈值（¥1 / 10 分钟）、`carry_micro` 进位（例：3 次 400,000 微分 → 第 3 次结算 1 分、carry 200,000）、`ApplyUpTo` 余额不足记欠费、欠费补扣并入同一流水、无用量仅欠费的结算、`max_id` 边界（结算中并发插入的行留到下次）、`llm_usage_daily` 聚合、90 天清理只删已结算、重放（同 `sid` 第二次 `ApplyUpTo` 撞 `ErrDuplicate` → 事务回滚无副作用）。
- `llm/controller`：期望对账（补 / 缩）、`MaxProvisionPerRound`、机器不足告警一次、provisioning 推进（用 fake appdeploy / rental 接口）、超时 failed、unhealthy 10 分钟重建、3 次 failed + 告警、draining 60 秒、`offline` 模型缩到 0。
- `llm/prober`：3 次失败 → unhealthy、一次成功恢复、部署 failed 立即 unhealthy、实例暂停视为失败。
- `llmgw`：用 `llmtest.FakeVLLM` 作上游（直接 `httptest`，不经 SSH）：鉴权错误码矩阵、`Content-Type` / 413、`model` 校验、流式注入 `include_usage` 且字节透传、非流式 usage 解析、上游 4xx 透传不收费、上游 5xx → 502 + 转移一次、首字节后断开不重试、客户端中断估算、`X-Request-Id`、CORS 预检、`/v1/models`、`SetWriteDeadline` 被调用（用自定义 ResponseWriter 断言）。
- `api` / `adminapi`：路由、角色矩阵（`permissions_test.go` 加全部 `/llm/*`）、TOTP 要求、`include_system`、池机组移出 busy、`/me` 字段、CSV 列。
- `identity`：`Ban` 系统用户 → `invalid_state`；系统用户 `Login` 不可达（手机号正则）。

### 16.2 集成（Go，`internal/app`，进程内 fake Agent + `sshtest` 假容器 + `FakeVLLM`）
- **全链路**：后台建模型 `qwen3-test`（¥0.5 / ¥1.5 每百万，replicas 1，online）→ 池里加 1 台 fake 机器 → Controller tick：`PlaceSystem` → fake Agent 完成 Create → 实例 running → tick：`appdeploy.Create` → `/hooks/apps/tick` 把部署推到 running（`sshtest` shell 模式）→ 在 `sshtest` 的 8000 端口挂 `FakeVLLM` → tick：探测成功 → `ready`。用户 A（实名、余额 ¥10）建 Key → `POST /v1/chat/completions`（非流式，FakeVLLM 返回 usage 100/50）→ 200、`llm_usage` 一行 `cost_micro = 100×50 + 50×150 = 12,500`、`unsettled_micro = 12,500`；流式请求 → SSE 带 usage 块、第二行；推进 10 分钟 + sweep → 一条 `llm_pay`（`usage_amount = 0`，因为 25,000 微分 < 1 分 → 实际 `charged 0`、`carry 25,000`、无钱包流水）；再发 100 次 → 结算 ≥ ¥1 → 钱包流水 `-X`、`CheckInvariants` 通过。
- **余额打穿与欠费**：余额 ¥0.02、50 个并发请求（每个估算 1 分）→ 部分 402、部分成功；结算后 `debt > 0`、余额 0；之后请求 402「欠费」；充值 ¥1 → 下一轮结算扣欠费 → 请求恢复。
- **限流**：RPM 2 → 第 3 次 429 且 `Retry-After`；并发 1 + FakeVLLM 延迟 → 第二个并发 429；Key 月上限 1 分 → 超后 429。
- **故障转移**：2 个节点；FakeVLLM A 对首个请求返回 503 → 请求成功（由 B 处理）、A `probe_fail_count 1`；A 连续 3 次探测失败 → `unhealthy`、告警一条、路由只剩 B；A 恢复 → `ready`；推进 10 分钟不恢复 → Redeploy → `rebuild_count 1`。
- **排空**：replicas 1 → 0：节点 `draining`（在途 1 个请求挂着）→ 请求结束 → `ReleaseSystem` → fake Agent Stop / Wipe → `removed`、机器 `idle`；机器随即可被普通 `POST /orders` 租到？**不能**（仍在池里，不可见机组）；移出池后可以。
- **移出池被拒**：有节点时 `DELETE /groups/{pool}/machines` → 409 `llm_pool_machine_busy`。
- **封禁**：封禁 A → 30 秒内 403；撤销 Key → 401。
- **一致性**：所有用例结束调用 `CheckInvariants`。

### 16.3 端到端（Playwright）
钩子：`/hooks/llm/upstream {port}`、`/hooks/llm/node {model_id, machine_id}`、`/hooks/llm/tick`、`/hooks/sweep` 含 `Settle`；后台模型用真实 API 建。
覆盖表新增（登记到 `coverage.json`）：
- `C-TOK-01`～`10`：导航入口与市场横幅；模型卡片价格与「暂无节点」/「可用」；未实名创建 Key 禁用并引导；创建 Key 只显示一次明文并可复制；停用 / 启用 / 撤销；用量 Tab 今日 / 本月与按天表（经钩子造一次调用 + sweep）；欠费提示条与「去充值」；请求日志分页与状态中文；结算记录链接到钱包流水「Token 消费」；快速开始的 `base_url` 与示例含在线模型 slug。
- `C-WAL-05`（若计划 8 已占用则顺延）：流水类型「Token 消费」筛选。
- `C-ACC-04`：我的页入口与本月金额。
- `A-LLM-01`～`09`：模型新建（super）/ 编辑价格有审计 / 上线需 TOTP 且 replicas ≥ 1 / 删除条件；机器池加入与移出（有节点被拒）；节点列表状态与操作（排空 / 重建 / 移除 TOTP）；账户限额编辑与停用；用量筛选与 CSV 下载；结算记录；仪表盘卡片；`finance` 看不到节点操作按钮、`ops` 看不到价格编辑。
- `A-INS-14`：实例列表「平台节点」标签、详情无退款 / 延长 / 换机。
- `A-USR-08`：用户详情「Token 商城」区块；用户列表默认无系统账号。
- 现有用例保持全绿；`menu.spec.ts` 加 `llm` 菜单。

### 16.4 真机验收清单（192.168.1.221，一台机器，全部通过才算完成）
平台跑在 Mac（配置同计划 7 验收：`SPARK_DOMAIN=192-168-1-218.sslip.io`，`SPARK_AGENT_BANDWIDTH_MBPS=1000`，`SPARK_APP_PIP_INDEX_URL` 同前）；Agent 用计划 7 的版本（含 `image.env`）；镜像 `spark/llm-serving:25.09` 本地 tag 已登记为 `LLM Serving`。只有一台机器：验收期间它在池里，不能同时做租用。curl 一律 `--resolve 192-168-1-218.sslip.io:8080:192.168.1.218`（或按实际端口 / TLS）。需要 root 的步骤由协调人执行。

1. **导入与发布 `vllm-node`**：`platform admin app import deploy/apps` 输出 `vllm-node 1.0.0 created`；后台应用目录显示「内部」标签并发布（动态码）；控制台市场页「一键部署」**没有**它。
2. **建模型与池**：后台 `/llm` 新建 `qwen3-4b`（名称 Qwen3-4B，`Qwen/Qwen3-4B`，LLM Serving，上下文 32768，输入 ¥0.50 / 输出 ¥1.50 每百万，replicas 1，并发 32）→ 上线（TOTP）；机器池加入机器 1；池页显示机器「空闲 · 无节点」。控制台 `/tokens` 模型卡片显示价格与「暂无节点」。
3. **节点拉起**：Controller 在 10 秒内建节点（`provisioning`，实例 `ins-…` 为「平台节点」，后台实例列表可见、无订单）；实例 running 后出现部署 `vllm-node`，进度随下载推进；**≤ 15 分钟** 节点 `ready`（后台节点页绿色，探测时间刷新）；`nvidia-smi` 看到 vLLM；控制台模型卡片变「可用」。后台机器详情显示「Token 服务池」。
4. **Key 与调用**：控制台用户（已实名、余额 ≥ ¥5）创建 Key `test`（允许模型留空）→ 明文只显示一次；`curl https://…/v1/models -H "Authorization: Bearer sk-spark-…"` 列出 `qwen3-4b`；非流式 `POST /v1/chat/completions {"model":"qwen3-4b","messages":[{"role":"user","content":"用一句话介绍 DGX Spark"}],"max_tokens":64}` → 200、`usage` 非空、响应头 `X-Request-Id`；流式（`"stream":true`）→ SSE、末尾有 `usage` 块与 `[DONE]`；用 OpenAI Python SDK（`base_url=https://…/v1`）跑一次流式对话成功。控制台请求日志出现 3 条，token 数与 curl 返回一致，费用 = token × 单价。
5. **结算**：连续调用直到未结算 ≥ ¥1（或等 10 分钟）→ 下一轮清扫后钱包流水出现「Token 消费」一条、金额 = floor(Σ 微分 / 1e6)、备注含请求数；控制台结算记录链接到该流水；`llm.CheckInvariants`（`go test ./internal/llm -run TestInvariantsAgainstDevDB`，连开发库只读）通过。
6. **限流**：后台把该用户 RPM 改为 2 → 第 3 次请求 429 `rate_limit_exceeded` 带 `Retry-After`；改回。Key 月上限设为 ¥0.01 → 429 `monthly_cap_exceeded`；取消上限。
7. **余额与欠费**：后台调账把余额调到 ¥0.00 → 请求 402 `insufficient_quota`「余额不足」；调到 ¥0.01、同时发 10 个 `max_tokens=512` 的请求 → 部分成功，结算后后台账户页显示欠费 > 0、流水一条扣到 0；再请求 → 402「账户有欠费」；充值（mock）¥5 → 下一轮清扫扣欠费（流水备注含「含欠费」）→ 请求恢复。
8. **Key 管理**：停用 Key → 30 秒内 401；启用恢复；撤销 → 401 且列表显示「已撤销」；再建 Key 只允许模型 `other` → 调 `qwen3-4b` 403 `model_not_allowed`。
9. **故障与恢复**：在 221 上 `docker exec <容器> sh -c 'kill $(cat /root/data/.spark-apps/vllm-node/service.pid)'`（杀 vLLM 进程）→ 1 分钟内节点 `unhealthy`、告警 `llm_node_unhealthy` 一条、请求 503 `model_unavailable`；部署 `failed(crashed)` → Controller 10 分钟后（或后台立即「重建」）Redeploy → 跳过下载（`skip install`）→ ≤ 5 分钟 `ready`、请求恢复。协调人 `sudo systemctl stop spark-agent` 5 分钟 → 实例暂停 → 节点 `unhealthy`；`start` 后 ≤ 1 分钟恢复 `ready`（不重建）。
10. **排空与释放**：模型 replicas 改 0（或下线）→ 节点 `draining` → `removing`（实例进入洗机）→ `removed`；机器回到 `idle`、仍在池内（市场页「现在可租」不含它）；移出池 → 市场页可租 → 下 1 天单成功（随后释放）。期间后台 `DELETE /groups/{pool}/machines` 在节点存在时返回 409。
11. **安全**：节点容器内 `curl 127.0.0.1:8000/v1/models` 无鉴权 → 401（vLLM `--api-key` 生效）；用户 Key 直接打 `https://ins-…-8000.<ingress 域>/v1/models` → 302 到登录（私有端口、系统账号）；平台日志与数据库里没有任何 prompt 内容（`llm_usage` 无内容列）；后台部署详情里 `API_KEY` 显示 `***`。
12. **对账无噪声**：全过程告警只有第 9 步预期的 `llm_node_unhealthy`、`container_down`（如有）、`machine_offline`；`CheckInvariants` 通过；`llm_usage` 行数 = 控制台日志条数 = 后台用量条数。

验收记录写到 `docs/superpowers/acceptance-plan9.md`，格式同计划 7。

## 17. 范围与遗留

**本计划做**：§2～§16 全部。

**上线前核对（写入 `followups-plan9.md`）**：首批模型清单与定价（运营）；池机器数量；`SPARK_LLM_GATEWAY / SPARK_LLM_CONTROLLER` 在多副本部署下的配置（网关建议单副本或折算限额）；帮助页与控制台的计费说明文案（法务）；`vllm-node` 的 NGC 标签复核（同计划 7）；默认限额是否调整。

**明确不做（遗留清单）**
- 预购 Token 套餐、充值赠送、阶梯 / 包月价、按用户差异化定价、优惠券。
- Token 消费计入邀请返利（计划 8 的基数只有 `order_pay`）。
- `api.<domain>` 独立子域；按 Key 的 IP 白名单；Key 的只读 / 作用域细分。
- 跨机张量并行、一机多模型、按负载自动伸缩副本（现为运营手动设 replicas）、按时段的节点预热。
- 洗机时保留模型缓存（需要改 Agent 的 wipe 语义；计划 7 遗留「机器级共享模型缓存」同题）。
- 多副本共享限流（Redis）；全局 / 按模型的排队与优先级。
- 日志内容留存、内容安全审核、敏感词过滤；按请求的审计导出。
- 用量预警短信 / 邮件、余额低于阈值提醒；站内信。
- Prometheus 指标导出（请求数、TTFT、吞吐）；节点 GPU 利用率采集（Agent 已有 `last_metrics`，可后续接入）。
- `/v1/responses`、`/v1/audio/*`、`/v1/images/*`、`/v1/files`、批处理接口、函数调用之外的扩展字段校验。
- 后台「一键把 N 台空闲机器加入池」、按机组批量入池。
- 节点级日志实时流（复用计划 7 的日志尾部）。

## 18. 任务拆分（供实施计划引用，24 项）
1. 迁移 00022、`wallet.KindLLMPay`、`apperr` 错误码、`llm` 包骨架（类型、常量、状态、`CheckInvariants` 七条）、前端类型与文案。
2. `rental` 系统实例：`PlaceSystem / ReleaseSystem`、`Instance.System`、生命周期分支、后台操作拒绝、`pickMachines` 回归；单测。
3. `fleet` 池机组（`purpose`、`LLMPoolGroupID`、校验）、`appdeploy.internal`（目录过滤、Create 校验、importer）；单测。
4. `deploy/apps/vllm-node`（`app.json / spec.json / app.sh`）+ `apps_dir_test` 校验。
5. `llm` 模型目录（CRUD、校验、状态机、审计）；单测。
6. `llm` API Key（生成 / 哈希 / 列表 / 更新 / 撤销 / 月上限）与 `Authorize` 缓存；单测。
7. `llm` 账户 / 用量 / 记账（`RecordUsage`、`unsettled_micro`、Key 月消费）、价格计算；单测。
8. `llm.Settle`（阈值、进位、`ApplyUpTo`、欠费、daily 聚合、清理）+ 清扫接入；单测。
9. `llm.Limiter`、`llm.Router`（节点表刷新、Pick 策略、在途计数）；单测。
10. `llmtest.FakeVLLM` + `llmgw` 网关（鉴权 / 门槛 / 解析 / 限流 / 预检 / 转发 / 流式 / 转移 / 记账 / 错误格式 / CORS / 期限）；单测。
11. `llm.Controller`（对账、建节点、推进、unhealthy 重建、排空、removing）+ `Prober`；单测（fake 接口）。
12. 控制台 API（`/llm/*`、`/me` 字段、`/v1` 挂载与 exempt）；单测。
13. 后台 API（模型 / 池 / 节点 / 账户 / Key / 用量 / 导出 / 结算 / summary / users / dashboard / ledger / groups 池校验 / instances `system`）+ 角色矩阵 + 审计。
14. `app.go` 装配（`SystemUserID`、`llm.Service`、`llmgw`、Controller）、`config`（两个开关）、`main.go`（Settle 入清扫、Controller goroutine）、README 配置表与部署注意。
15. Go 集成测试（§16.2 全部）+ e2e 钩子（`/hooks/llm/*`、`sweep`）。
16. 真机检查点 A（后端 + API）：§16.4 第 1～9 项用 curl / 后台 API 验证，记录到 `acceptance-plan9.md`。
17. 控制台：共享类型、`logic/tokens.ts`（金额 / token 格式化、示例代码生成）、导航与市场横幅、`/tokens` 模型与快速开始 Tab。
18. 控制台：API Keys Tab（创建一次性展示、编辑、停用、撤销）。
19. 控制台：用量 / 请求日志 / 结算记录 Tab、欠费提示、钱包标签、帮助页、我的页入口。
20. 后台：菜单、`/llm` 总览 + 模型 Tab（结构化表单、TOTP 上下线）。
21. 后台：节点与机器池 Tab（加入 / 移出、节点操作、详情抽屉）、机器详情 / 实例标签。
22. 后台：账户 / 用量 / 结算 Tab（CSV）、用户详情区块、仪表盘卡片、流水筛选、应用目录「内部」标签。
23. Playwright 用例（§16.3）、钩子、`coverage.json`、`menu.spec.ts`。
24. 真机验收 B（§16.4 第 10～12 项 + 页面复核）+ 验收记录 + `followups-plan9.md` + `make check` 全量。
