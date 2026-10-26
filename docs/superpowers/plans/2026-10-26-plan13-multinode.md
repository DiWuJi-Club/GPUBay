# Spark Cloud 计划 13：多机模型（2 台 / 4 台集群）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 两台或四台 DGX Spark 组成一个整体跑一个大模型。A（优先）：Token 商城模型声明 `nodes_per_replica ∈ {1,2,4}`，一个副本 = 同一互联网络里的 N 台池内机器（Ray head + workers，vLLM `--distributed-executor-backend ray`，`TP × PP = N`），按组健康检查 / 故障转移，网关只路由 head，计费不变。B：用户下单「集群 2 台 / 4 台」，平台保证同一互联网络、成员互通且不破坏租户隔离，每个实例注入 `/etc/spark-cluster.json` 与环境变量，集群页一键部署多机 vLLM（`vllm-cluster`）。最后在 2～4 台真机上分「2 台以太网」「2 台 RDMA 直连」「4 台」三个检查点验收。

**Architecture:** 新纯包 `internal/clusterspec`（ClusterSpec / 网卡探测结构 / IP 规划 / `cluster.json`、`cluster.env` 渲染，平台与 Agent 共用）。Agent：协议加 `Hello.net / StateReport.net / InstanceState.cluster / CreateInstance.cluster / ConfigureCluster`、能力位 `features.Cluster`；新包 `internal/agent/clusternet`（接口 + fake）与 `clusternet/linux`（sysfs 探测、routed：veth + 宿主机 /32 路由 + `SPARK-CLU` 链；macvlan：CX-7 口 / VLAN 子接口；RDMA 设备与 RoCE GID）；执行器在 `docker start` 后建容器第二网卡 `spc0`。平台：fabric（`cluster_fabrics`）+ 机器网络字段（`fleet`）；实例集群 `instance_clusters`（`rental`：同 fabric 原子分配、`PlaceSystemCluster`、`SweepClusters` 级联 / 对齐 / 结束、整组续费 / 自动续费 / 释放）；集群部署 `app_cluster_deployments`（`appdeploy`：N 条成员部署，rank0 = head）；`llm` 节点组（`llm_node_members`，`node.instance_id` = head，网关零改动）。迁移 `00026_multinode.sql`。

**Tech Stack:** Go 1.27（`~/.local/go/bin`）、pgx v5、chi v5、embedded-postgres（`dbtest.Main`）、protoc 36.2 + protoc-gen-go v1.36.12 + protoc-gen-go-grpc v1.6.2（`~/.local/bin`，`make proto`）；前端 Vue 3 + Element Plus + Vitest + Playwright。**不新增任何 Go 模块依赖、不新增 npm 依赖**（`net/netip` 是标准库）。

**Spec:** `docs/superpowers/specs/2026-10-26-plan13-multinode-design.md`（下称「规格」；裁定 M1～M20 为负责人默认）。上位：计划 2（Agent）、计划 6 §4.2 / §6、计划 7 §5、计划 9 §3 / §5。

**分支：** 集成分支 `feat/plan13-multinode`（worktree `/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud-p13`，基于 `main` 234187d）。

**本计划不包含（规格 §17）：** 集群预订 / 指定机器 / 自动换机 / 扩缩容；集群溢价；SR-IOV、多轨、PFC / ECN；4 台无交换机环形；集群流量限速；模型分发；容器间 SSH 互信；管家集群工具；vLLM mp 多机。

## 执行须知（写给协调人与每个子代理）

- **并行执行**：任务按「任务依赖与并行组」分波；同一波内的任务互不依赖、文件范围不重叠（见「文件归属」），可同时派给不同子代理。**所有子代理只用 Opus（high），不用 Fable。**
- 每个任务一个独立 worktree：协调人在依赖任务都已合入集成分支后执行
  `git -C /Users/rifuyiri/Documents/claude/机房搭建/spark-cloud-p13 worktree add ../spark-cloud-p13-t<N> -b p13/t<N> feat/plan13-multinode`，子代理只在 `/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud-p13-t<N>` 里改文件、提交；完成后协调人按任务号顺序 `git merge --no-ff p13/t<N>` 进集成分支，合并后在集成分支跑 `go vet ./... && go test ./...`（涉及前端再加 `cd web && npm test && npm run build`），通过再派下一波。冲突只可能出现在「文件归属」标为共享的少数文件上，由协调人按两边意图合并。
- 子代理只看得到自己任务的文字：每个任务的 Interfaces 写全了消费什么、产出什么、精确签名，不写「同上」。规格里的真机事实（§0）与裁定（§1.3）是约束；与代码冲突时以代码为准并在汇报里写明。
- **功能必须实测通过才算完成**：每个任务结束前实际运行并通过 `go vet ./... && go test ./...`（以及任务列出的其它命令），证据是命令输出；不能只写代码。
- 测试机 192.168.1.221（`imac` 用户）与后续真机：需要 root 的命令由协调人经 `sudo -S -p ""` 从 stdin 送密码执行，**密码不进任何文件、脚本、提交**（`git diff --cached | grep -i sudo` 应为空）。真机任务（17～20）由协调人亲自或派一个子代理在协调人监督下执行。
- 大文件（模型、镜像）只在 DGX 上下载 / 传输：镜像用 `docker save spark/llm-serving:25.09 | ssh <dgx> docker load` 走内网；模型走 ModelScope；开发 Mac 不中转大文件。大陆网络：Go `GOPROXY=https://goproxy.cn,direct`，npm `https://registry.npmmirror.com/`，Docker 镜像站 `docker.m.daocloud.io`（只在需要拉基础镜像时）。
- 开发 Mac 的 DNS 对 `*.sslip.io` 返回假地址：curl 加 `--resolve`，Playwright / Chrome 用 `--host-resolver-rules`。
- 提交信息结尾空一行加 `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`（子代理按运行时系统提示给出的署名为准）。不 push。

## Global Constraints

- 所有命令前置 `export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH`；Go 模块名 `spark-cloud`；开发 Mac 没有 Docker，Go 测试用 embedded-postgres。
- **迁移编号固定 `migrations/00026_multinode.sql`**（00023 保留未用，goose 已允许乱序）；表 / 列 / 约束 / 索引名照抄规格 §2。
- **集群网络常量**（`internal/clusterspec`）：`ModeRouted = "routed"`、`ModeMacvlan = "macvlan"`、`IfName = "spc0"`、`DefaultPool = "10.210.0.0/16"`、`MemberOffset = 11`（成员 IP = 子网第 `11 + rank` 个地址）、网关 = 子网 `.1`、`MaxSize = 8`、`MasterPort = 29500`。宿主机侧网卡名：veth `sch` + 实例 ID 后 8 位、容器侧临时名 `scc` + 后 8 位、macvlan `scm` + 后 8 位、VLAN 子接口 `scv<vlan>`。iptables 链 `SPARK-CLU`；跳转 `DOCKER-USER -i sch+ -j SPARK-CLU`、`DOCKER-USER -o sch+ -j SPARK-CLU`、`INPUT -i sch+ -j SPARK-IN`。
- **集群规模**：服务层只接受 `{2, 4}`（`rental.ClusterSizes`）；表约束 `2..8`；`direct` fabric 只能组 2 台、最多挂 2 台机器。
- **fabric**：`kind ∈ lan | cx7`、`isolation ∈ routed | direct | vlan`、`lan ⇔ routed`、`rdma` 只能在 `cx7`；集群 `mode = routed`（lan）/ `macvlan`（cx7）；有效 RDMA = `fabric.rdma ∧ 全员 machines.cluster_rdma`。
- **能力位**：`features.SharedSlots = 1 << 0`（已有）、`features.Cluster = 1 << 1`（新）；机器入选集群要求 `machines.cluster_ready`（fleet 计算，已含能力位判断）。
- **集群容器参数**：`--cap-drop NET_RAW`、`--sysctl net.ipv4.ping_group_range=0 2147483647`；RDMA 时 `--device /dev/infiniband/rdma_cm --device /dev/infiniband/uverbsN`。
- **集群状态**：表 `active | released | failed`；派生 `forming | ready | degraded`（规格 §5.4）。集群部署表 `active | removed`；派生 `installing | running | failed | stopped | stale | removing | removed`（规格 §6.4）。角色 `head`（rank0）/ `worker`。
- **锁序**（规格 §14）：多成员路径一律「`instance_clusters` 行 `FOR UPDATE` → 成员实例按 ID 升序」；单实例路径不读写 `instance_clusters` 行；集群创建在机器行锁之后取 `pg_advisory_xact_lock(hashtext('cluster-alloc'))` 分配子网 / VLAN。
- **错误码**（`apperr`，规格 §10 文案原文）：`ErrClusterNotFound`（`cluster_not_found` 404）、`ErrClusterMember`（`cluster_member` 409）、`ErrClusterSizeInvalid`（`cluster_size_invalid` 400）、`ErrClusterBookingUnsupported`（`cluster_booking_unsupported` 400）、`ErrFabricNotFound`（404）、`ErrFabricInUse`（409）、`ErrFabricFull`（409）、`FabricInvalid(field, reason string) *Error`（`fabric_invalid` 400）、`ErrAppClusterRequired`（400）、`ErrAppClusterUnsupported`（400）、`AppClusterSize(min, max int) *Error`（`app_cluster_size` 400，「该应用支持 X～Y 台的集群」）、`ErrAppClusterExists`（409）、`ErrAppClusterManaged`（409）；`ClusterNoCapacity(n int) *Error`（`no_capacity` 409，「暂无可组成 N 台集群的空闲机器（需同一高速网络）」）。
- **告警种类**（文案稳定，不含数值）：`cluster_failed`（warning「集群 <id> 开通失败，已全额退款」）、`cluster_rdma_degraded`（warning「集群 <id> 的 RDMA 不可用，已退回以太网」）、`cluster_net_error`（warning「实例 <id> 的集群网络异常」）。**审计动作**：`fabric.create / update / delete`、`cluster.extend`、`cluster.resync`（admin）；`cluster.release`、`cluster.auto_renew`、`cluster.app_deploy / app_redeploy / app_stop / app_start / app_remove`（user / 系统）。全部加进 `web/shared/src/labels.ts` 的 `auditActions` / `alertKindLabel`。
- **指令**：`fleet.CmdConfigureCluster = "configure_cluster"`；`rental.ConfigureClusterCommandTimeout = 2 * time.Minute`；Agent 侧时限同值。
- **Token 商城常量**（`internal/llm/consts.go`）：`GroupRebuildAfter = 2 * time.Minute`、`GroupSuspendFailAfter = 10 * time.Minute`；`ProvisionTimeoutFor(ver appdeploy.Version) time.Duration = ver.Spec.TimeoutMinutes 分钟 + 30 分钟`（`TimeoutMinutes` 为 0 时按 `appdeploy.DefaultTimeoutMinutes`）。`appdeploy.MaxTimeoutMinutes = 480`。
- **保留变量兼容**：非集群部署的 `env` 文件必须与改动前**逐字节相同**；`N = 1` 的 Token 商城节点的 `NodeEnv` 必须与改动前逐字节相同（都有回归测试）。
- **e2e 覆盖表新增 ID**（规格 §16.3）：`C-CLU-01～10`、`A-FAB-01～02`、`A-MCH-11～12`、`A-CLU-01～03`、`A-INS-17`、`A-LLM-10～12`；`data-testid` 命名 `页面-元素`；禁止 `waitForTimeout`；`workers: 1`；现有用例全绿。
- 时间一律 `clock.Clock`；日期按 `internal/platform/tz`；生产禁止 mock。

## Review Focus

1. **租户隔离不被集群网络打破**：routed 模式 `SPARK-CLU` 只放行本机成员 ↔ 对端成员 IP 对、`INPUT -i sch+` 进 `SPARK-IN`、集群容器无 `NET_RAW`；macvlan 父接口无 IP 且不是 `lan_iface` → Task 2 `TestExpectedCluRules`、`TestApplyRefusesLanIface`、Task 3 `TestCreateClusterDropsNetRaw`、真机 CP-1 第 4 项 / CP-2 第 4 项。
2. **同 fabric 原子分配、没有半个集群**：两个并发的 4 台集群订单抢同一个 6 台 fabric → 恰一个成功（另一个换 fabric 或 `no_capacity`），任何时刻不存在成员跨 fabric 或成员数 ≠ size 的 active 集群 → Task 6 `TestClusterPlaceAtomicSameFabric`、`TestClusterPlaceConcurrent`。
3. **开通期中止的钱账**：rank1 Create 失败 → rank0 洗机且两单全额退、机器 0 不判故障、告警一条；`CheckClusterInvariants` 通过 → Task 10 `TestClusterAbortRefundsAll`、Task 13 `TestClusterAbortE2E`。
4. **Token 商城组语义**：成员实例没了 → 立即 failed 且释放全部成员；成员崩溃 → 2 分钟后整组 Redeploy；成员暂停 10 分钟 → failed；Router 不收成员暂停的组 → Task 9 `TestGroupMemberGoneFailsWholeGroup`、`TestGroupCrashRedeploysAll`、`TestRouterExcludesGroupWithSuspendedMember`。
5. **存量零扰动**：非集群部署的 `env` 字节不变（不触发重装）、`N = 1` 节点 `NodeEnv` 字节不变、单机分配在无 fabric 时与现状完全一致、旧 Agent 机器永不入选集群 → Task 7 `TestReservedEnvUnchangedForNonCluster`、Task 9 `TestPlatformNodeEnvSingleUnchanged`、Task 6 `TestOldAgentExcluded`。

## 已做出的裁定（规格未写明或与代码核对后确定的细节）

- **包依赖方向**：`clusterspec` 不依赖任何项目包；`fleet / agentgw / rental / appdeploy / llm / agent/*` 依赖它。`rental` 不依赖 `appdeploy`（集群部署的聚合由 `appdeploy` 读 `instances.cluster_id`）；`appdeploy` 依赖 `rental`（已有）；`llm` 通过 `RentalOps / AppOps` 小接口依赖二者（`app.go` 传真实服务；Task 9 只用 fake，**不要**加 `var _ RentalOps = (*rental.Service)(nil)` 之类的编译期断言，接口满足在 Task 13 装配时自然校验）。
- **ClusterSpec 的 Go 形态**只有一个：`clusterspec.Spec`；`fleet.CreateInstancePayload.Cluster *clusterspec.Spec`、`fleet.ConfigureClusterPayload{Cluster clusterspec.Spec}` 直接 JSON 存 `agent_commands.payload`；`agentgw` 把它转 proto，Agent 把 proto 转回 `clusterspec.Spec`。
- **网卡探测结构**同样只有 `clusterspec.NetInfo / Iface`；`machines.net_info` 存其 JSON；`fleet.StateReport` 增 `Net *clusterspec.NetInfo \`json:"-"\``（不进 `last_state_report`，单独 `RecordNetInfo`）。
- **`InstanceState.cluster`** 进 `fleet.InstanceState.Cluster`（`json:"cluster,omitempty"`），随 `last_state_report` 原样落库；`rental` 的视图从 `last_state_report` 读成员网络状态。
- **成员 rank 分配**：按机器 ID 升序给 0..N-1（下单与 `PlaceSystemCluster` 一致），所以 head 是 ID 最小的机器。
- **集群下单的 savepoint**：每个候选 fabric 一个外层 savepoint（`tx.Begin` 嵌套），内部原样调用 `placeOnMachines`；失败回滚该 savepoint 释放已加的机器行锁再试下一个。
- **用户没有停机 / 开机接口**（核对 `internal/api/server.go`）：成员「单独停机」不存在；成员停机只来自到期或失联。
- **`RenewCluster` 幂等**：子订单键 `key#<rank>`；重放按 `key#` 前缀查同一用户的 `renew` 订单。`checkIdempotencyKey` 已拒绝含 `#` 的键。
- **集群应用的 head 访问地址**：就是 rank0 实例的 8000 端口（计划 7 的私有端口 URL）；worker 不登记端口。
- **fake Agent 的网卡**：`fakeagent.Options.Net *clusterspec.NetInfo`（nil = 默认一块 `eth0` 有线网卡，IPv4 = Options.IPs[0]）；`Features` 默认 `SharedSlots | Cluster`。
- **`vllm-node 1.1.0` 与 `vllm-cluster 1.0.0` 两份 app.sh 的 Ray 段**用相同的标记注释包住（`# ---- spark-cluster-ray begin/end ----`），`apps_dir_test` 断言两段逐字相同。

## 文件归属（并行时避免冲突）

| 文件 / 目录 | 归属任务 | 说明 |
|---|---|---|
| `migrations/00026_multinode.sql`、`internal/platform/db/migrate_multinode_test.go` | 1 | |
| `internal/clusterspec/**` | 1 | 之后只读 |
| `api/proto/agent/v1/agent.proto`、`internal/agentpb/**` | 1 | |
| `internal/platform/apperr/apperr.go` | 1 | 之后只读（新错误码全部在 Task 1 加） |
| `web/shared/src/types/cluster.ts`、`web/shared/src/labels.ts`、`web/shared/src/types/index.ts` | 1 | 14 / 15 只追加组件私有类型到各自目录 |
| `internal/agent/clusternet/clusternet.go`、`internal/agent/clusternet/fake/**` | 1 | |
| `internal/agent/clusternet/linux/**` | 2 | |
| `internal/agent/{executor,runtime,state,client,fakeagent}/**`、`internal/agent/netfilter/fake/**` | 3 | |
| `cmd/spark-agent/**`、`deploy/agent/**` | 4 | |
| `internal/fleet/**`（除 `commands.go` 的类型，见 1）、`internal/agentgw/**` | 5 | `fleet/commands.go`、`fleet/agent.go` 的 `InstanceState` 字段由 1 加 |
| `internal/rental/cluster_types.go`、`internal/rental/service.go` 的 `Instance` 字段与 `instanceCols` | 1 | `service.go` 是三方共享文件：1 改 `Instance` / `instanceCols` / `scanInstance`，6 改 `enqueueCreate`，10 改头注释锁序表——各改各的段落，合并时按段落取两边 |
| `internal/rental/cluster_place.go`、`cluster_view.go`、`cluster_spec.go`、`batch.go`、`alloc.go`、`system.go` | 6 | |
| `internal/rental/cluster_life.go`、`cluster_invariants.go`、`renew.go`、`autorenew.go`、`migrate.go`、`refund.go`、`admin_ops.go`、`results.go`、`sweeper.go`、`reconcile.go`、`service.go` 头注释 | 10 | |
| `internal/appdeploy/**`（`types.go` 的 `Spec.Cluster`、`spec.go` 校验、`consts.go`、`cluster_types.go` 由 1） | 7 | |
| `deploy/apps/vllm-node/**`、`deploy/apps/vllm-cluster/**`、`deploy/apps/README.md`、`internal/appdeploy/apps_dir_test.go` | 8 | `apps_dir_test.go` 例外归 8 |
| `internal/llm/**` | 9 | |
| `internal/api/**` | 11 | |
| `internal/adminapi/**` | 12 | |
| `internal/app/**`、`internal/platform/config/**`、`cmd/platform/**`、`cmd/e2e-server/**`、`internal/e2ehooks/**`、`README.md` | 13 | |
| `web/console/**` | 14 | |
| `web/admin/**` | 15 | |
| `web/e2e/**` | 16 | |
| `docs/superpowers/acceptance-plan13.md`、`followups-plan13.md`、`deploy/image/tests/**` | 17～20 | |

## 任务依赖与并行组

```
G0  : T1 基础
G1  : T2 Agent 宿主机网络 ┃ T3 Agent 执行器 ┃ T5 fleet+agentgw ┃ T6 rental 集群分配 ┃ T7 appdeploy 集群部署 ┃ T8 应用包 ┃ T9 llm 节点组      （都只依赖 T1）
G2  : T4 Agent 装配（T2、T3） ┃ T10 rental 集群生命周期（T6）
G3  : T11 控制台 API（T5、T6、T7、T10） ┃ T12 后台 API（T5、T6、T7、T9、T10）
G4  : T13 装配 + 钩子 + 集成测试（T3、T5～T12）
G5  : T14 控制台页面（T11；页面单测可与 T13 并行） ┃ T15 后台页面（T12；同左）
G6  : T16 Playwright + 覆盖表（T13、T14、T15）
G7  : T17 真机 CP-0 单机回归（T4、T13；可与 G5/G6 并行）
G8  : T18 真机 CP-1 两台以太网（T16、T17 + 负责人 2 台到位）→ T19 CP-2 两台 RDMA 直连（T18 + 直连线）→ T20 CP-3 四台 + 收尾（T19 + 4 台 / 交换机）
```
合并顺序：同一波内按任务号。A（Token 商城）所需的后端路径是 T1→T2/T3/T5/T6/T7/T8/T9→T4/T10→T12→T13，B 另需 T11、T14；真机 CP-1 先验 A 与 B 的以太网路径。

---
### Task 1: 基础——迁移 00026、`clusterspec`、协议、错误码、共享类型与接口占位

**Files:**
- Create: `migrations/00026_multinode.sql`、`internal/platform/db/migrate_multinode_test.go`、`internal/clusterspec/{spec.go,net.go,render.go,names.go,spec_test.go,net_test.go,render_test.go}`、`internal/clusterspec/testdata/{env-routed.golden,env-rdma.golden,json-routed.golden,json-rdma.golden}`、`internal/agent/clusternet/clusternet.go`、`internal/agent/clusternet/fake/{fake.go,fake_test.go}`、`internal/rental/cluster_types.go`、`internal/appdeploy/cluster_types.go`、`web/shared/src/types/cluster.ts`
- Modify: `api/proto/agent/v1/agent.proto`（`make proto` 重新生成 `internal/agentpb/*.pb.go`）、`internal/agentpb/agentpb_test.go`、`internal/agent/features/features.go`、`internal/platform/apperr/apperr.go`、`internal/fleet/commands.go`、`internal/fleet/agent.go`（`InstanceState.Cluster`、`StateReport.Net`）、`internal/rental/service.go`（`Instance.ClusterID / ClusterRank`、`instanceCols`、`scanInstance`）、`internal/appdeploy/types.go`（`Spec.Cluster`）、`internal/appdeploy/spec.go`（`ValidateSpec` 的 cluster 规则）、`internal/appdeploy/consts.go`（`MaxTimeoutMinutes = 480`）、`internal/appdeploy/spec_test.go`、`web/shared/src/labels.ts`、`web/shared/src/types/index.ts`、`web/shared/src/__tests__/labels.spec.ts`

**Interfaces:**
- Produces（迁移）：规格 §2 全部 SQL 原文 + Down（三条 `RAISE EXCEPTION`：「仍有运行中的集群，请先释放」「仍有多机节点组，请先把副本数调为 0」「仍有集群应用，请先卸载」，再逆序删除）。
- Produces（`internal/clusterspec`，纯包）：
  ```go
  const (ModeRouted = "routed"; ModeMacvlan = "macvlan"; IfName = "spc0"; DefaultPool = "10.210.0.0/16"; MemberOffset = 11; MaxSize = 8; MasterPort = 29500)
  type Member struct {
      Rank       int    `json:"rank"`
      InstanceID string `json:"instance_id"`
      IP         string `json:"ip"`
      HostIP     string `json:"host_ip,omitempty"` // routed：对端宿主机集群网卡 IPv4
  }
  type Spec struct {
      ClusterID string   `json:"cluster_id"`
      Version   int      `json:"version"`
      Size      int      `json:"size"`
      Rank      int      `json:"rank"`
      Mode      string   `json:"mode"`
      Iface     string   `json:"iface"`
      VLAN      int      `json:"vlan,omitempty"`
      MTU       int      `json:"mtu,omitempty"`
      Subnet    string   `json:"subnet"`
      RDMA      bool     `json:"rdma"`
      Members   []Member `json:"members"` // 含自己，按 rank 升序；缺位的 rank 不出现
  }
  type Iface struct {
      Name string `json:"name"`; MAC string `json:"mac"`; Up bool `json:"up"`; SpeedMbps int `json:"speed_mbps"`; MTU int `json:"mtu"`
      IPv4 []string `json:"ipv4"`; Driver string `json:"driver"`; PCI string `json:"pci"`; Wireless bool `json:"wireless"`
      RDMADevice string `json:"rdma_device,omitempty"`; RDMAActive bool `json:"rdma_active,omitempty"`; Uverbs string `json:"uverbs,omitempty"`; LinkLayer string `json:"link_layer,omitempty"`
  }
  type NetInfo struct { LanIface string `json:"lan_iface"`; Ifaces []Iface `json:"ifaces"` }
  type Runtime struct { HCA string `json:"hca,omitempty"`; GIDIndex int `json:"gid_index"`; RDMAActive bool `json:"rdma_active"` }
  func Subnet(pool netip.Prefix, index int) (netip.Prefix, error)   // pool 必须 /16，index 0..255 → 第 index 个 /24
  func MemberIP(subnet netip.Prefix, rank int) netip.Addr           // 子网起始 + MemberOffset + rank
  func GatewayIP(subnet netip.Prefix) netip.Addr                    // 子网起始 + 1
  func (s Spec) Validate() error  // ClusterID ^clu-[a-z0-9]{10}$、Version ≥ 1、2 ≤ Size ≤ MaxSize、0 ≤ Rank < Size、Mode 合法、Iface ^[A-Za-z0-9_.-]{1,15}$、VLAN 0 或 2..4094、MTU 0 或 1280..9216、Subnet 是 /24、
                                  // Members 按 rank 升序且唯一、每个 IP == MemberIP(subnet, rank)、含自己（rank == s.Rank）、routed 时每个对端 HostIP 是 IPv4、macvlan 时 HostIP 为空
  func (s Spec) Self() Member; func (s Spec) Head() (Member, bool); func (s Spec) Peers() []Member
  // ResolveIface 规格 §3.2：kind "lan" → configured 或 n.LanIface，须存在、Up、有 IPv4；hostIP = 该网卡是 LanIface 时取 lanIP，否则取其第一个 IPv4。
  // kind "cx7" → configured 或第一个 Driver == "mlx5_core" 且 Up 的网卡；须存在、Up、!Wireless、Name != n.LanIface；hostIP 为空。错误为中文可展示原因。
  func ResolveIface(n NetInfo, kind, configured, lanIP string) (Iface, string, error)
  func RenderJSON(s Spec, rt Runtime) []byte  // 规格 §3.6 的 /etc/spark-cluster.json（network = rdma 当 s.RDMA && rt.RDMAActive，否则 ethernet；rdma 为 null 或 {hca, gid_index}）
  func RenderEnv(s Spec, rt Runtime) []byte   // 规格 §3.6 的 KEY='value' 行，固定顺序，单引号按 ' → '\'' 转义，末尾换行
  func HostVeth(instanceID string) string     // "sch" + 实例 ID 后 8 位
  func TempName(instanceID string) string     // "scc" + 后 8 位
  func MacvlanName(instanceID string) string  // "scm" + 后 8 位
  func VLANDev(vlan int) string               // "scv" + vlan
  ```
- Produces（proto，规格 §4.1 原文）：`Hello.net = 5`、`StateReport.net = 3`、`InstanceState.cluster = 3`、`CreateInstance.cluster = 8`、`Command.configure_cluster = 15`、消息 `ConfigureCluster / ClusterSpec / ClusterMember / NetInfo / NetIface`。`features.Cluster uint32 = 1 << 1`（注释：Agent 支持 `CreateInstance.cluster` 与 `ConfigureCluster`；平台据 `machines.agent_features` 判定）。**本任务不改 `client.go` 的默认 features**（Task 3 改）。
- Produces（`apperr`）：Global Constraints「错误码」全部。
- Produces（`fleet`）：
  ```go
  const CmdConfigureCluster CommandType = "configure_cluster"
  type CreateInstancePayload struct { ImageRef string; SSHPort int; ShareSlots int; Cluster *clusterspec.Spec `json:"cluster,omitempty"` }
  type ConfigureClusterPayload struct { Cluster clusterspec.Spec `json:"cluster"` }
  type InstanceState struct { InstanceID string `json:"instance_id"`; Phase string `json:"phase"`; Cluster string `json:"cluster,omitempty"` }
  type StateReport struct { Instances []InstanceState `json:"instances"`; Images []string `json:"-"`; Net *clusterspec.NetInfo `json:"-"` }
  ```
- Produces（`rental/cluster_types.go` 与 `service.go`）：
  ```go
  const (ClusterActive = "active"; ClusterReleased = "released"; ClusterFailed = "failed"; ClusterForming = "forming"; ClusterReady = "ready"; ClusterDegraded = "degraded")
  var ClusterSizes = []int{2, 4}
  const ClusterAlignSlack = 60 * time.Second
  const ConfigureClusterCommandTimeout = 2 * time.Minute
  type Cluster struct {
      ID string `json:"id"`; UserID int64 `json:"user_id"`; System bool `json:"system"`; Size int `json:"size"`; FabricID int64 `json:"fabric_id"`
      Mode string `json:"mode"`; VLAN *int `json:"vlan"`; RDMA bool `json:"rdma"`; MTU int `json:"mtu"`; SubnetIndex int `json:"subnet_index"`; Version int `json:"version"`
      State string `json:"state"`; BatchID *string `json:"batch_id"`; ReadyAt *time.Time `json:"ready_at"`; CreatedAt time.Time `json:"created_at"`; UpdatedAt time.Time `json:"updated_at"`; EndedAt *time.Time `json:"ended_at"`
  }
  type ClusterMemberView struct {
      Rank int `json:"rank"`; InstanceID string `json:"instance_id"`; State string `json:"state"`; Suspended bool `json:"suspended"`
      MachineID int64 `json:"machine_id"`; MachineLabel string `json:"machine_label"`; IP string `json:"ip"`; NetStatus string `json:"net_status"`; ExpiresAt *time.Time `json:"expires_at"`
  }
  type ClusterView struct {
      Cluster
      Status string `json:"status"`            // forming / ready / degraded / released / failed
      Network string `json:"network"`          // rdma / ethernet / rdma_degraded
      FabricName string `json:"fabric_name"`; FabricKind string `json:"fabric_kind"`
      Subnet string `json:"subnet"`; HeadIP string `json:"head_ip"`
      Members []ClusterMemberView `json:"members"`
  }
  type ClusterAvail struct { Available int `json:"available"`; RDMA bool `json:"rdma"` }
  type ClusterFilter struct { State string; UserID, FabricID *int64; System *bool }
  type SystemClusterRequest struct { MachineIDs []int64; GroupID, ImageID int64; Note string }
  type SystemClusterResult struct { Cluster Cluster; Instances []Instance } // Instances[i].ClusterRank == i
  // Instance 增：ClusterID *string `json:"cluster_id"`; ClusterRank *int `json:"cluster_rank"`（instanceCols 末尾追加 cluster_id, cluster_rank）
  ```
- Produces（`appdeploy`）：
  ```go
  type ClusterSupport struct {
      Mode        string `json:"mode"`                    // optional / required
      MinSize     int    `json:"min_size"`
      MaxSize     int    `json:"max_size"`
      WorkerPorts bool   `json:"worker_ports,omitempty"`  // false：worker 部署不登记 instance_ports
  }
  // Spec 增 Cluster *ClusterSupport `json:"cluster,omitempty"`（nil 不改变既有 spec_hash）
  // ValidateSpec：Cluster 非 nil 时 Mode ∈ {optional, required}、2 ≤ MinSize ≤ MaxSize ≤ 8，否则 invalid("cluster…")
  const (ClusterModeOptional = "optional"; ClusterModeRequired = "required"; RoleHead = "head"; RoleWorker = "worker"; ClusterDepActive = "active"; ClusterDepRemoved = "removed")
  const (ClusterStatusInstalling = "installing"; ClusterStatusRunning = "running"; ClusterStatusFailed = "failed"; ClusterStatusStopped = "stopped"; ClusterStatusStale = "stale"; ClusterStatusRemoving = "removing"; ClusterStatusRemoved = "removed")
  type ClusterCreateRequest struct { UserID int64; ClusterID string; AppID int64; VersionID *int64; Env map[string]string; IP string }
  type ClusterRedeployRequest struct { UserID int64; ClusterDeploymentID string; VersionID *int64; Env map[string]string; EnvSet bool; IP string }
  type ClusterMemberDeployment struct { Rank int; Role string; InstanceID string; Deployment Deployment; Current bool /* 实例仍是集群当前成员 */ }
  type ClusterDeployment struct {
      ID, ClusterID string; UserID, AppID, AppVersionID int64; State string
      Status, Health, Phase string; Progress int; Members []ClusterMemberDeployment; CreatedAt, UpdatedAt time.Time
  }
  // Deployment 增 ClusterDeploymentID *string; ClusterRole string（列 cluster_deployment_id / cluster_role，deployment 的 scan 同步）
  ```
  `MaxTimeoutMinutes = 480`（`ValidateSpec` 的消息改为「需在 1～480 之间」）。
- Produces（`internal/agent/clusternet`）：
  ```go
  // Net 是 Agent 的集群数据面（规格 §3、§4.4）。实现：linux（Task 2）、fake（本任务）。
  type Net interface {
      Probe(ctx context.Context, lanIP string) (clusterspec.NetInfo, error)
      Devices(s clusterspec.Spec) ([]string, error) // RDMA 时 /dev/infiniband/rdma_cm 与 /dev/infiniband/uverbsN；非 RDMA 返回 nil
      Apply(ctx context.Context, instanceID string, pid int, s clusterspec.Spec) (clusterspec.Runtime, error)
      Check(ctx context.Context, instanceID string, pid int, s clusterspec.Spec) (bool, error)
      Teardown(ctx context.Context, instanceID string, s clusterspec.Spec, remaining []clusterspec.Spec) error
      SyncRules(ctx context.Context, routed map[string]clusterspec.Spec) error // 实例 ID → 本机 routed 成员 Spec
  }
  // ErrNoGID：RDMA 集群的 spc0 已配好，但宿主机 GID 表里找不到对应的 RoCE v2 GID（Apply 同时返回 Runtime{RDMAActive: false}）；
  // 调用方据此退回 TCP（NCCL_IB_DISABLE=1），不当作失败。Task 2 返回它、Task 3 用 errors.Is 判断。
  var ErrNoGID = errors.New("clusternet: RoCE GID not found")
  ```
  `fake.Net`：`func New() *Net`；字段（带锁的访问器）`Info clusterspec.NetInfo`、`Devs []string`、`FailApply / FailCheck / FailTeardown error`、`CheckResult bool`（默认 true）、`Runtime clusterspec.Runtime`；记录 `Applies() []ApplyCall{InstanceID string; PID int; Spec clusterspec.Spec}`、`Teardowns() []string`、`Rules() map[string]clusterspec.Spec`（最近一次 SyncRules）。
- Produces（前端 `types/cluster.ts`）：`ClusterMember`、`ClusterView`、`ClusterAvail`、`ClusterApp`（`{id, app_id, app_slug, app_name, version, status, health, phase, progress, members:[{rank, role, instance_id, state, health, progress, current}]}`）、`Fabric`、`FabricRow`、`NetIface`、`NetInfo`、`AdminCluster`、`LLMNodeMember`；`labels.ts`：`clusterStatusLabel`（组建中 / 就绪 / 降级 / 已释放 / 开通失败）与 `clusterStatusType`、`clusterNetworkLabel`（RDMA 高速互联 / 普通网络 / RDMA 不可用）、`fabricKindLabel`（普通以太网 / ConnectX-7）、`fabricIsolationLabel`（三层路由 / 直连 / VLAN 隔离）、`clusterAppStatusLabel`、`clusterRoleLabel`（主节点 / 工作节点）、`alertKindLabel` 加三种、`auditActions` 加 Global Constraints 全部动作。

- [ ] **Step 1: 写失败的测试**
  - `TestMigration00026RoundTrip`（仿 `migrate_llm_test.go`）：`UpTo(25)` → `UpByOne` → 断言约束：`cluster_fabrics` 的 kind / isolation / rdma / vlan 组合（`lan+direct`、`lan+rdma`、`vlan` 缺范围都失败）；两个 active 集群同 `subnet_index` 失败、一个改 `released` 后成功；同 fabric 同 vlan 两个 active 失败；`instances_cluster_pair`；`llm_models_parallel`（2 × 2 = 4 成功、2 × 1 ≠ 4 失败）；`llm_nodes_group`；`llm_node_members_machine_live_uniq`；Down 在有 active 集群时失败（消息含「运行中的集群」），清掉后成功；再 Up 成功。
  - `clusterspec`：`TestSubnetAndIPs`（index 0 → `10.210.0.0/24`、`MemberIP(·,3)` = `.14`、`GatewayIP` = `.1`、index 256 报错、非 /16 池报错）；`TestSpecValidate` 表驱动（每条规则一正一反）；`TestResolveIface` 表驱动（lan 默认 / lan 指定 / lan 网卡 down / cx7 自动选 mlx5 / cx7 指定无线被拒 / cx7 等于 lan 网卡被拒 / hostIP 取值）；`TestRenderGolden`（四个黄金文件；`-update` 旗标重写）；`TestRenderEnvQuoting`（成员 ID 含单引号时转义正确）；`TestNames`（长度 ≤ 15）。
  - `agentpb_test.go`：新消息 Marshal / Unmarshal 往返。
  - `appdeploy/spec_test.go`：`TestValidateSpecCluster`（合法 / mode 错 / min > max / max > 8 / 无 cluster 的 spec_hash 与改动前相同）、`TestValidateSpecTimeout480`。
  - `fake_test.go`：记录与失败注入。
  - `labels.spec.ts`：新映射齐全。
- [ ] **Step 2: 运行确认失败**
  Run: `go test ./internal/platform/db/ -run TestMigration00026 ./internal/clusterspec/ ./internal/appdeploy/ -run 'TestValidateSpec' 2>&1 | tail -5`
  Expected: 编译失败 / 测试失败。
- [ ] **Step 3: 实现**（迁移按规格原文；`make proto`；`rental.scanInstance` 增两个字段并核对所有 `instanceCols` 用法编译通过；`appdeploy` 的 deployment 列表与 scan 增两列）
- [ ] **Step 4: 运行测试并提交**
  Run: `make proto && git diff --stat internal/agentpb && go vet ./... && go test ./... 2>&1 | tail -15 && cd web && npm test 2>&1 | tail -3`
  Expected: 全部通过（现有测试不受影响）。
  ```bash
  git add migrations internal api web/shared
  git commit -m "feat(multinode): migration 00026, clusterspec package, agent protocol fields, error codes and shared types

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 2: Agent 宿主机网络实现（`clusternet/linux`：探测、routed、macvlan / VLAN、`SPARK-CLU`、RDMA GID）

**Files:**
- Create: `internal/agent/clusternet/linux/{linux.go,probe.go,routed.go,macvlan.go,rules.go,rdma.go,linux_test.go,probe_test.go,routed_test.go,macvlan_test.go,rules_test.go,rdma_test.go}`、`internal/agent/clusternet/linux/testdata/sysfs-221/**`（221 实况：`enP7s7`（r8169，down）、`wlP9s9`（wireless，up，`192.168.1.221`）、无 infiniband）、`testdata/sysfs-spark/**`（标准 Spark：`enP7s7` 有线 lan、4 个 mlx5 netdev `enp1s0f0np0 / enp1s0f1np1 / enP2p1s0f0np0 / enP2p1s0f1np1`，其中两个 carrier=1、对应 `rocep1s0f1` 等 RDMA 设备 `ports/1/state = 4: ACTIVE`、`link_layer = Ethernet`、`infiniband_verbs/uverbs0..3/ibdev`、`gids/0..5` 与 `gid_attrs/types/*`）
- Test: 上述 `_test.go`

**Interfaces:**
- Consumes: Task 1 的 `clusterspec.*`、`clusternet.Net`；现有 `cmdrun.Runner`、`cmdrun.Recorder`（`Stub(prefix, out, err)`、`Calls`、`Inputs`、`Lines()`）。
- Produces：
  ```go
  package linux
  type Options struct {
      SysRoot string                 // 默认 "/sys"；测试指向 testdata
      DevRoot string                 // 默认 "/dev"
      GIDWait time.Duration          // 默认 5s
      Log     *slog.Logger
  }
  type Net struct{ /* run cmdrun.Runner; opt Options */ }
  func New(run cmdrun.Runner, opt Options) *Net        // 实现 clusternet.Net
  func ExpectedCluRules(routed map[string]clusterspec.Spec) []string // `iptables -S SPARK-CLU` 的期望输出（含 -N 行），按实例 ID、对端 rank 排序
  const ChainClu = "SPARK-CLU"
  ```
- 行为（规格 §3.2～§3.4 的命令原文，每步幂等）：
  - `Probe`：只读 sysfs（`<SysRoot>/class/net/*`），网卡筛选与字段见规格 §3.2；`lan_iface` = IPv4 含 `lanIP` 的网卡（IPv4 用 `ip -4 -o addr show` 解析，命令输出可 stub）；`uverbs` 由 `<SysRoot>/class/infiniband_verbs/uverbs*/ibdev` 反查。
  - `Devices`：`s.RDMA` 时按 `s.Iface` 的探测结果返回 `[<DevRoot>/infiniband/rdma_cm, <DevRoot>/infiniband/<uverbs>]`，任一不存在 → 错误「RDMA 设备不可用」。
  - `Apply`：先校验 `s.Validate()`；`ModeRouted` 按规格 §3.3（veth、地址、容器路由、宿主机 /32、对端路由、`rp_filter=1`、关 IPv6）；`ModeMacvlan` 按 §3.4（VLAN 子接口 `scv<vlan>` 不存在才建、父接口 `ip -4 addr flush`、关 IPv6、MTU、macvlan bridge、移入 netns、改名 `spc0`、`A_r/24`、up）；**若 `s.Iface` 是持有本机 lan IP 的网卡且 mode = macvlan → 返回错误「集群网卡不能是管理网卡」，不执行任何命令**；RDMA 时查 GID（`<SysRoot>/class/infiniband/<hca>/ports/1/gids/<i>` 等于 `0000:0000:0000:0000:0000:ffff:<hex(A_r)>` 且 `gid_attrs/types/<i>` 为 `RoCE v2`），每 200ms 一次直到 `GIDWait`，找不到 → `Runtime{RDMAActive: false}` 与错误 `clusternet.ErrNoGID`（Task 1 定义；调用方据此写 `NCCL_IB_DISABLE=1`，状态 error 但不当失败处理）。
  - `Check`：`nsenter -t <pid> -n ip -o -4 addr show dev spc0` 含 `A_r`；routed 再查 `ip route show A_r/32` 指向 `sch<x>`。
  - `Teardown`：删对端 `/32` 路由（`ip route del A_k/32 via H_k`，不存在忽略）、`ip link del sch<x> / scm<x>`（不存在忽略）；`remaining` 里没有同一 `scv<vlan>` 的使用者时 `ip link del scv<vlan>`。
  - `SyncRules`：`iptables -S SPARK-CLU` 与 `ExpectedCluRules` 比对，不同则 `iptables-restore --noflush -w` 一次事务重建（同 `netfilter/iptables.syncChains` 的做法）；再保证三条跳转在各自链的第 1 位（同 `ensureFirstJump`；`DOCKER-USER`、`INPUT` 列不出 → 返回错误）。`ExpectedCluRules` 内容按规格 §3.3。

- [ ] **Step 1: 写失败的测试**
  - `TestProbe221`：夹具 → `LanIface = "wlP9s9"`、两块网卡（`enP7s7` `Up=false`、`wlP9s9` `Wireless=true`）、无 RDMA。
  - `TestProbeSparkCX7`：4 个 mlx5 网卡、`Driver = "mlx5_core"`、两个 `RDMAActive`、`Uverbs` 映射正确、`LinkLayer = "Ethernet"`；`lo / docker0 / veth* / spark0 / sch* / scm* / scv*` 被排除。
  - `TestApplyRoutedCommands`：`cmdrun.Recorder` 快照（`Lines()` 与期望多行字符串逐行相等），rank 1 / size 4 的对端路由 3 条。
  - `TestApplyMacvlanDirect`、`TestApplyMacvlanVLAN`（VLAN 子接口已存在时不重建：stub `ip link show scv120` 成功）、`TestApplyRefusesLanIface`（无任何 `ip link add` 调用）。
  - `TestExpectedCluRules`（两个本机实例 × 各自对端）、`TestSyncRulesNoopWhenMatching`、`TestSyncRulesRebuildsOnDrift`（`Inputs[0]` 是完整 restore 事务）、`TestJumpsFirst`。
  - `TestTeardownKeepsSharedVLAN`（remaining 里还有用 `scv120` 的 Spec → 不删）、`TestTeardownIgnoresMissing`。
  - `TestFindGID`（夹具 `gids/3` 匹配 → 3）、`TestGIDTimeout`（`GIDWait = 50ms` → `errors.Is(err, clusternet.ErrNoGID)`、`RDMAActive=false`）、`TestDevices`（RDMA / 非 RDMA / 设备缺失）、`TestCheck`。
- [ ] **Step 2: 运行确认失败**
  Run: `go test ./internal/agent/clusternet/linux/ 2>&1 | head -5`
- [ ] **Step 3: 实现**（`sysfs` 读取全部经 `filepath.Join(opt.SysRoot, …)`；命令全部经 `cmdrun.Runner`；不 import `executor`）
- [ ] **Step 4: 运行测试并提交**
  Run: `go vet ./... && go test ./internal/agent/... 2>&1 | tail -5 && GOOS=linux GOARCH=arm64 go build ./internal/agent/...`
  ```bash
  git add internal/agent/clusternet/linux
  git commit -m "feat(agent): cluster data plane on linux (sysfs probe, routed veth + SPARK-CLU, macvlan/VLAN on CX-7, RDMA devices and RoCE GID)

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 3: Agent 执行器集群支持（容器参数、`ConfigureCluster`、启动后建 `spc0`、巡检、洗机清理、上报、init.sh、client 协议）

**Files:**
- Modify: `internal/agent/runtime/runtime.go`（`Spec.Devices / CapDrop / Sysctls`）、`internal/agent/runtime/docker/docker.go`（create 参数）、`internal/agent/runtime/docker/docker_test.go`、`internal/agent/runtime/fake/fake.go`（记录新字段）、`internal/agent/state/store.go`（`Instance.Cluster / ClusterRuntime`）、`internal/agent/state/store_test.go`、`internal/agent/executor/{executor.go,ops.go,sshdir.go}`、新建 `internal/agent/executor/cluster.go`、`internal/agent/executor/cluster_test.go`、`internal/agent/executor/executor_test.go`（追加）、`internal/agent/client/client.go`（Hello / StateReport 的 `net`、`InstanceState.cluster`、`toCommand` 的 `configure_cluster` 与 `create.cluster`、默认 features）、`internal/agent/client/client_test.go`、`internal/agent/fakeagent/fakeagent.go`（`Options.Net`、`Options.ClusterNet *clusternetfake.Net`、默认 features）、`internal/agent/fakeagent/fakeagent_test.go`
- Test: 上述 `_test.go`

**Interfaces:**
- Consumes: Task 1 的 `clusterspec`、`clusternet.Net`、`clusternet/fake`、proto 新字段、`features.Cluster`。
- Produces：
  ```go
  // runtime
  type Spec struct{ …; Devices []string; CapDrop []string; Sysctls map[string]string }   // docker：--device X、--cap-drop X、--sysctl k=v（按 key 排序），在 --cap-add 之后、--network 之前
  // state
  type Instance struct{ …; Cluster *clusterspec.Spec `json:"cluster,omitempty"`; ClusterRuntime *clusterspec.Runtime `json:"cluster_runtime,omitempty"` }
  // executor
  type Command struct{ …; Cluster *clusterspec.Spec }      // create_instance 的 cluster；configure_cluster 的目标 Spec（ClusterID 为空 = 退出集群）
  type Deps struct{ …; Cluster clusternet.Net }             // nil = 不支持集群
  type InstanceState struct{ InstanceID, Phase, Cluster string }   // Cluster：""、"ok"、"pending"、"error: …"（≤ 200 字节，合法 UTF-8）
  func (e *Executor) ClusterCapable() bool                  // Deps.Cluster != nil
  func (e *Executor) NetInfo(ctx context.Context) *clusterspec.NetInfo   // Deps.Cluster.Probe(ctx, cfg.LanIP)；出错或不支持 → nil；受 reportTimeout 约束
  const configureClusterTimeout = 2 * time.Minute           // commandTimeout("configure_cluster")
  // client
  // Hello.Features 默认：SharedSlots | (Cluster 当 ex.ClusterCapable())；Config.Features 非 nil 时照用（测试模拟旧 Agent）
  // Hello.Net 与每份 StateReport.Net = ex.NetInfo(ctx)（nil 不发）；InstanceState.Cluster 原样转发
  // toCommand：Create 的 cluster → executor.Command.Cluster；ConfigureCluster → Type "configure_cluster"、Cluster
  // fakeagent
  type Options struct{ …; Net *clusterspec.NetInfo; ClusterNet *clusternetfake.Net }   // ClusterNet nil → 新建一个；Net nil → 一块 eth0（IPv4 = IPs[0] 或 10.0.0.10，Up，有线）
  func (a *Agent) ClusterNet() *clusternetfake.Net
  ```
- 行为（规格 §4.3）：
  - `create`：`Cluster != nil` → `Spec.Validate()` 失败或 `Deps.Cluster == nil` → 失败；`runtime.Spec.CapDrop = ["NET_RAW"]`、`Sysctls = {"net.ipv4.ping_group_range": "0 2147483647"}`、`Devices = Net.Devices(spec)`（出错 → 失败）；写 `cluster.json / cluster.env`（`clusterspec.RenderJSON / RenderEnv`，`Runtime{}`）到 `ssh/<id>/`（`writeFileAtomic`，0644）；`state.Instance.Cluster = spec`；容器已存在但缺少期望的 cap-drop / 设备（`Inspect` 不比较这些，**以 state 里有无 Cluster 判定**：首次带集群的 create 若容器已存在且 state 无 Cluster → 删掉重建）。
  - `startChecked` 末尾（`Limit` 成功之后）：`inst.Cluster != nil` → `Net.Apply` → 成功：`ClusterRuntime = rt`、按 rt 重写 `cluster.env`、`clusterStatus[id] = ok`；`errors.Is(err, clusternet.ErrNoGID)`（Task 1 定义，Task 2 的 linux 实现返回）→ 写 `NCCL_IB_DISABLE=1`、状态 `error: 未找到 RoCE GID，已退回 TCP`、不算失败；其它错误：**create 调用链**（`e.creating[id]` 标记）→ 停容器并返回错误；**其它调用链** → 状态 `error: …`、返回 nil。routed 实例 Apply 后 `Net.SyncRules(本机全部 routed 且 Desired=running 的 Spec)`。
  - `configure_cluster`：`checkID`；本地无该实例 → 成功（忽略）；`spec.ClusterID == ""` → `Net.Teardown` + 删两个文件 + 清 `Cluster / ClusterRuntime` + `SyncRules`；`spec.Version < 本地 Version` → 成功（忽略）；否则存 state、重写文件；容器运行中 → `Net.Apply`（同上的状态处理）+ `SyncRules`；`spec.RDMA && !old.RDMA` → 状态 `error: RDMA 变更需重建实例`（文件按 `RDMA=false` 渲染）。
  - `refreshRules` 内新增 `ensureClusters`（`opMu.TryLock`，同 `ensureLimits`）：对 Desired=running 且有 Cluster 的实例 `Inspect` 拿 pid → `Net.Check` 为 false → 重新 Apply；之后 `SyncRules`。
  - `wipe`：删容器之后 `Net.Teardown(id, spec, 其余实例的 Spec)`（失败计入洗机校验失败），删 ssh 目录（文件随之删除）。
  - `Report`：`InstanceState.Cluster`：无 Cluster `""`；容器未运行 `pending`；`clusterStatus[id]`（内存，Agent 重启后为空 → 视为 `pending` 直到第一次 Apply）。
  - `initScript`：规格 §3.6 的片段插在 `exec /usr/sbin/sshd` 之前。

- [ ] **Step 1: 写失败的测试**（fake runtime + fake disk + fake netfilter + `clusternetfake.Net`）
  - `TestCreateClusterDropsNetRaw`：fake runtime 记录的 `Spec.CapDrop == ["NET_RAW"]`、sysctl、RDMA 时 `Devices`；非集群 create 三项为空（`docker_test` 另断言命令行快照：非集群命令行与改动前逐字相同）。
  - `TestCreateClusterWritesFilesAndApplies`：`ssh/<id>/cluster.json / cluster.env` 内容 = `RenderJSON / RenderEnv`；`Applies()` 一次；`Rules()` 含该实例（routed）。
  - `TestCreateClusterApplyFailureFailsCreate`（`FailApply` → create 失败、容器被停）、`TestStartApplyFailureKeepsRunning`（start 指令路径 → 成功、`Report` 为 `error: …`）、`TestNoGIDFallsBackToTCP`（env 含 `NCCL_IB_DISABLE='1'`）。
  - `TestConfigureClusterVersioning`（旧版本忽略、新版本 Apply、清空 Teardown + 删文件）、`TestConfigureClusterStoppedInstance`（只写文件不 Apply）、`TestConfigureClusterRDMAFlip`。
  - `TestEnsureClustersReapplies`（`CheckResult=false` → 下一轮巡检 Apply）、`TestWipeTearsDownCluster`、`TestRecoverReappliesCluster`、`TestReportClusterField`、`TestInitScriptHasClusterSnippet`、`TestStateRoundTripCluster`（旧状态文件无字段读出 nil）。
  - client：`TestHelloCarriesNetAndClusterFeature`（`ClusterCapable` 为真时 features = 3）、`TestOldAgentFeatures`（`Config.Features = new(uint32)` → 0）、`TestToCommandConfigureCluster`、`TestStateReportNetAndCluster`。
  - fakeagent：`TestFakeAgentReportsNetInfo`。
- [ ] **Step 2: 运行确认失败**
  Run: `go test ./internal/agent/... 2>&1 | grep -E 'FAIL|cannot' | head`
- [ ] **Step 3: 实现**（`cluster.go` 放 `applyCluster / configureCluster / ensureClusters / clusterFiles`；`create` 用 `e.creating` 集合区分调用链）
- [ ] **Step 4: 运行测试并提交**
  Run: `go vet ./... && go test ./internal/agent/... ./internal/agentgw/ ./internal/app/ 2>&1 | tail -8`
  Expected: 全部通过（`internal/app` 的现有集成测试用 fakeagent，必须仍然绿）。
  ```bash
  git add internal/agent
  git commit -m "feat(agent): cluster members in executor (cap-drop NET_RAW, RDMA devices, spc0 after start, ConfigureCluster, periodic repair, wipe teardown, net info and cluster status in reports)

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 4: Agent 装配与自检

**Files:**
- Modify: `cmd/spark-agent/main.go`（`clusternet/linux` 装配进 `executor.Deps.Cluster`）、`cmd/spark-agent/selfcheck.go`（「集群网卡」一节）、`cmd/spark-agent/main_test.go`、`deploy/agent/install.sh`（安装后提示：CX-7 口勿配 IP、`ip_forward`）、`deploy/agent/install_test.sh`（如有断言则同步）
- Test: `cmd/spark-agent/main_test.go`

**Interfaces:**
- Consumes: Task 2 `linux.New(cmdrun.Runner, linux.Options{Log})`；Task 3 `executor.Deps.Cluster`。
- Produces：`--fake` 时 `Deps.Cluster = clusternetfake.New()`，否则 `linux.New(runner, linux.Options{Log: log})`。`selfcheck` 新增输出行（`ok:` / `warn:` 前缀与现有一致）：每块物理网卡 `iface <name> up=<bool> speed=<n> driver=<d> rdma=<dev|-> active=<bool>`；`warn: 未发现 ConnectX-7 网卡，集群将只能使用普通以太网`；CX-7 口有 IPv4 → `warn: <iface> 上有地址 <ip>，作为集群网卡时 Agent 会清除`；`/proc/sys/net/ipv4/ip_forward != 1` → `warn`；本机地址或路由落在 `10.210.0.0/16` → `warn: 与集群网段冲突`。selfcheck 不因这些 warn 失败（集群是可选能力）。

- [ ] **Step 1: 写失败的测试**：`TestSelfcheckClusterSection`（`cmdrun.Recorder` + 临时 sysroot 拷贝 Task 2 的 `sysfs-221` 夹具 → 输出含「未发现 ConnectX-7」）；`TestRunAgentWiresCluster`（`--fake` 下 Hello features = 3）。
- [ ] **Step 2: 运行确认失败**：`go test ./cmd/spark-agent/ 2>&1 | tail -3`
- [ ] **Step 3: 实现**
- [ ] **Step 4: 运行并提交**
  Run: `go vet ./... && go test ./cmd/spark-agent/ ./internal/agent/... && make agent && sh deploy/agent/install_test.sh`
  ```bash
  git add cmd/spark-agent deploy/agent
  git commit -m "feat(agent): wire linux cluster network into spark-agent, selfcheck cluster NIC section

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 5: fleet 集群网络（fabric、机器网络字段、就绪判定）+ agentgw 协议转换

**Files:**
- Create: `internal/fleet/fabrics.go`、`internal/fleet/fabrics_test.go`、`internal/fleet/clusternet.go`（`evalClusterNet`、`RecordHello`、`RecordNetInfo`）、`internal/fleet/clusternet_test.go`
- Modify: `internal/fleet/machines.go`（`Machine` 新字段、`machineCols`、`scanMachine`）、`internal/fleet/groups.go`（`MachineUpdate` 与 `UpdateMachine`）、`internal/fleet/admin.go`（`MachineFilter.FabricID`、列表列）、`internal/fleet/machines_test.go`、`internal/agentgw/{connect.go,convert.go,convert_test.go,connect_test.go}`
- Test: 上述

**Interfaces:**
- Consumes: Task 1 的迁移列、`clusterspec`、proto、`fleet.CreateInstancePayload.Cluster`、`ConfigureClusterPayload`、`CmdConfigureCluster`、`features.Cluster`。
- Produces：
  ```go
  type Fabric struct {
      ID int64 `json:"id"`; Name string `json:"name"`; Kind string `json:"kind"`; Isolation string `json:"isolation"`; RDMA bool `json:"rdma"`; MTU int `json:"mtu"`
      VLANMin *int `json:"vlan_min"`; VLANMax *int `json:"vlan_max"`; Description string `json:"description"`; CreatedAt time.Time `json:"created_at"`; UpdatedAt time.Time `json:"updated_at"`
  }
  type FabricInput struct { Name, Kind, Isolation string; RDMA bool; MTU int; VLANMin, VLANMax *int; Description string }
  type FabricRow struct { Fabric; MachineCount int `json:"machine_count"`; ActiveClusters int `json:"active_clusters"` }
  func (s *Service) ListFabrics(ctx context.Context) ([]FabricRow, error)
  func (s *Service) GetFabric(ctx context.Context, q db.Querier, id int64) (Fabric, error)          // 不存在 → ErrFabricNotFound
  func (s *Service) CreateFabric(ctx context.Context, actor audit.Actor, in FabricInput) (Fabric, error)   // 审计 fabric.create
  func (s *Service) UpdateFabric(ctx context.Context, actor audit.Actor, id int64, in FabricInput) (Fabric, error) // 有 active 集群时 kind/isolation/vlan 变化 → ErrFabricInUse；之后重算该 fabric 全部机器的 cluster_ready/rdma；审计 fabric.update（before/after）
  func (s *Service) DeleteFabric(ctx context.Context, actor audit.Actor, id int64) error   // 有机器或 active 集群 → ErrFabricInUse；审计 fabric.delete
  // Machine 增：FabricID *int64 `json:"fabric_id"`; ClusterIface string `json:"cluster_iface"`; AgentFeatures uint32 `json:"agent_features"`;
  //            NetInfo *clusterspec.NetInfo `json:"net_info"`; NetInfoAt *time.Time `json:"net_info_at"`; ClusterReady bool `json:"cluster_ready"`; ClusterRDMA bool `json:"cluster_rdma"`; ClusterReason string `json:"cluster_reason"`（不是列：scan 后用 evalClusterNet 现算）
  type MachineUpdate struct{ …; SetFabric bool; FabricID *int64; ClusterIface *string }
  // UpdateMachine：SetFabric / ClusterIface 变化时，机器上有 cluster_id 非空的活动实例（creating/running/stopped/wiping）→ ErrInvalidState（「机器上有集群成员，不能修改集群网络」）；
  //   加入 direct fabric 后该 fabric 机器数 > 2 → ErrFabricFull；FabricID 不存在 → ErrFabricNotFound；同事务重算 cluster_ready / cluster_rdma；审计 diff 带 fabric_id / cluster_iface
  func (s *Service) RecordHello(ctx context.Context, machineID int64, features uint32, net *clusterspec.NetInfo) error  // 写 agent_features（与 net_info，若非 nil）并重算
  func (s *Service) RecordNetInfo(ctx context.Context, machineID int64, net clusterspec.NetInfo) error   // 写 net_info / net_info_at 并重算（JSON 与现值相同只更新 net_info_at）
  // evalClusterNet（未导出，规格 §3.2）：(ready, rdma bool, reason string)，reason 中文：「未加入集群网络」「Agent 不支持集群（请升级）」「尚未上报网卡」「<ResolveIface 的错误>」
  ```
- agentgw：`Connect` 收到 Hello 后调 `Fleet.RecordHello(ctx, m.ID, hello.GetFeatures(), toNetInfo(hello.GetNet()))`（替代此前不读 features 的现状；`Fleet` 依赖接口加该方法）；`handleReport`：`rep.Net != nil` → `RecordNetInfo`；`toStateReport` 带 `InstanceState.Cluster`；`toProtoCommand`：Create 载荷 `Cluster != nil` → `CreateInstance.Cluster = toProtoSpec(*p.Cluster)`；`CmdConfigureCluster` → `Command_ConfigureCluster{ConfigureCluster: {Cluster: toProtoSpec(p.Cluster)}}`（`Cluster.ClusterID == ""` 时发空 Spec）。转换函数 `toProtoSpec / fromProtoNet` 放 `convert.go`，表驱动测试往返。

- [ ] **Step 1: 写失败的测试**
  - `TestFabricCRUDValidation`（kind/isolation 组合、vlan 范围、名称唯一、删除占用 → in_use）、`TestDirectFabricMaxTwo`、`TestUpdateFabricRecomputesMachines`、`TestUpdateFabricBlockedByActiveCluster`（直接 SQL 插一个 active 集群）。
  - `TestEvalClusterNetMatrix`（无 fabric / 旧 Agent / 无 net / lan 正常 / lan 网卡 down / cx7 自动 / cx7 指定无线 / cx7 = lan 网卡 / rdma 判定 fabric.rdma × rdma_active × uverbs）。
  - `TestRecordHelloAndNetInfo`（就绪列随之变化；相同 JSON 只刷时间）、`TestUpdateMachineFabricGuard`（有集群成员 → invalid_state）。
  - agentgw：`TestConvertClusterSpecRoundTrip`、`TestConfigureClusterCommandToProto`、`TestHelloRecordsFeaturesAndNet`、`TestReportRecordsNetAndClusterState`（`last_state_report` JSON 含 `"cluster":"ok"`）。
- [ ] **Step 2: 运行确认失败**：`go test ./internal/fleet/ ./internal/agentgw/ 2>&1 | tail -5`
- [ ] **Step 3: 实现**
- [ ] **Step 4: 运行并提交**
  Run: `go vet ./... && go test ./internal/fleet/ ./internal/agentgw/ ./internal/rental/ ./internal/adminapi/ 2>&1 | tail -6`
  ```bash
  git add internal/fleet internal/agentgw
  git commit -m "feat(fleet,agentgw): cluster fabrics, machine NIC probe and cluster readiness, agent features recorded, cluster spec conversion

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 6: rental 集群分配（实体、ClusterSpec 构造、`PlaceBatch` 集群分支、`PlaceSystemCluster / ReleaseSystemCluster`、可用性、视图）

**Files:**
- Create: `internal/rental/cluster_place.go`、`internal/rental/cluster_spec.go`、`internal/rental/cluster_view.go`、`internal/rental/cluster_place_test.go`、`internal/rental/cluster_spec_test.go`、`internal/rental/cluster_view_test.go`
- Modify: `internal/rental/batch.go`（`BatchRequest.Cluster`、`BatchResult.Cluster`、`validateBatch`、集群分支）、`internal/rental/alloc.go`（`pickSpec.FabricID / ClusterOnly`、`pickWhereSQL` 增两个条件参数、`pickOrderSQL` 首键 `(m.fabric_id IS NOT NULL)`）、`internal/rental/system.go`（抽出 `placeSystemOn(tx, machineID, groupID, imageID, note, cluster *clusterRef)` 供单机与集群共用）、`internal/rental/service.go`（`enqueueCreate` 增可选 Spec 参数：`enqueueCreateSpec(ctx, q, inst, ref, spec *clusterspec.Spec)`，原函数调用它传 nil）、`internal/rental/helpers_test.go`（造 fabric / 就绪机器的辅助函数）、`internal/rental/alloc_test.go`（追加）
- Test: 上述

**Interfaces:**
- Consumes: Task 1（迁移列、`rental.Cluster*` 类型、`clusterspec`、`fleet.CreateInstancePayload.Cluster`、`apperr` 集群错误码、`ClusterSizes`）。测试里机器的 `fabric_id / cluster_ready / cluster_rdma / net_info / agent_features` 用 SQL 直接设置（不依赖 Task 5）。
- Produces：
  ```go
  type BatchRequest struct{ …; Cluster bool }
  type BatchResult struct{ …; Cluster *ClusterView }
  // pickSpec 增 FabricID *int64; ClusterOnly bool（ClusterOnly：m.fabric_id IS NOT NULL AND m.cluster_ready）
  func (s *Service) PlaceSystemCluster(ctx context.Context, req SystemClusterRequest) (SystemClusterResult, error)
  func (s *Service) ReleaseSystemCluster(ctx context.Context, clusterID string) error   // 非系统集群 → ErrInvalidState；已无活动成员 → nil
  func (s *Service) ClusterSpecFor(ctx context.Context, q db.Querier, clusterID string, rank int) (clusterspec.Spec, error)
  func (s *Service) GetClusterView(ctx context.Context, id string) (ClusterView, error)                 // 不存在 → ErrClusterNotFound
  func (s *Service) ClusterForUser(ctx context.Context, userID int64, id string) (ClusterView, error)  // 别人的 / 系统集群 → ErrClusterNotFound
  func (s *Service) ListClustersForUser(ctx context.Context, userID int64) ([]ClusterView, error)       // active + ended_at 在 30 天内，新的在前
  func (s *Service) AdminListClusters(ctx context.Context, f ClusterFilter, p paging.Page) ([]ClusterView, int, error)
  func (s *Service) ClusterAvailability(ctx context.Context, productID int64, groupID *int64) (map[int]ClusterAvail, error)  // 键 2、4
  func (s *Service) SubnetPool() netip.Prefix   // Deps.ClusterSubnetPool，零值 → clusterspec.DefaultPool
  // Deps 增 ClusterSubnetPool netip.Prefix
  ```
- 行为：规格 §5.1～§5.4。要点：
  - `validateBatch`：`Cluster` 时 `Count ∉ ClusterSizes` → `ErrClusterSizeInvalid`；`StartAt != nil` → `ErrClusterBookingUnsupported`；`MachineID != nil` → `BadRequest("集群不能指定机器")`。
  - 集群分支：fabric 候选 SQL（规格 §5.1，基于 `pickWhereSQL` 的即时分支 + `ClusterOnly`）→ 逐 fabric savepoint + `placeOnMachines(spec{FabricID}, N, 原回调)`；都失败 → `apperr.ClusterNoCapacity(N)`；成功 → `createCluster(ctx, tx, …)`（`cluster-alloc` 咨询锁、最小空闲 `subnet_index`、`vlan` fabric 取最小空闲 VLAN、`mode`、`rdma`、`mtu`、`batch_id`、按机器 ID 升序 `UPDATE instances SET cluster_id, cluster_rank`）→ 原流程 charge / 事件；`enqueueCreateSpec(…, ClusterSpecFor(…, rank))`。有效 RDMA 为假而 fabric.rdma 为真 → 同事务 `RaiseAlertOnce(warning, cluster_rdma_degraded, …)`。
  - `PlaceSystemCluster`：规格 §5.2；`MachineIDs` 长度 ∉ ClusterSizes → `ErrClusterSizeInvalid`；任一台条件不满足 / fabric 不同 / `cluster_ready=false` → `ErrMachineUnavailable`。
  - `ClusterSpecFor`：规格 §5.3；成员 `HostIP` 用 `clusterspec.ResolveIface(net_info, fabric.kind, machines.cluster_iface, machines.lan_ip)`；`Spec.Validate()` 失败 → 返回错误（不下发坏 Spec）。
  - 视图：`Status` 按规格 §5.4（`NetStatus` 从 `machines.last_state_report` 的 `instances[].cluster` 取）；`Network`：`rdma` / `ethernet` / `rdma_degraded`（fabric.rdma 且集群 rdma 为假）；`HeadIP` = `MemberIP(subnet, 0)`。
  - 单机即时分配排序首键 `(m.fabric_id IS NOT NULL)`；无 fabric 时与现状一致。

- [ ] **Step 1: 写失败的测试**
  - `TestClusterPlaceAtomicSameFabric`：fabric A 3 台、fabric B 4 台（lan）、零散 2 台无 fabric → 4 台集群全部落在 B，rank 按机器 ID，四张订单同一 `batch_id`、钱包扣 4 × 单价、四条 Create 载荷的 Spec 互为对端（routed、`host_ip` = 各机 lan_ip）。
  - `TestClusterPlacePrefersRDMAThenTightest`、`TestClusterPlaceDirectOnlyTwo`、`TestClusterPlaceNoCapacity`（消息为 `ClusterNoCapacity(4)`）、`TestOldAgentExcluded`（`cluster_ready=false` 不入选）、`TestClusterPlaceValidation`（count 3、预订、指定机器）。
  - `TestClusterPlaceConcurrent`：6 台的 fabric，两个 goroutine 同时下 4 台集群 → 恰一个成功、另一个 `no_capacity`，任何 active 集群成员数 = size、同 fabric。
  - `TestClusterSubnetAndVLANAllocation`（顺序分配、释放后复用、并发 20 个 2 台集群子网互不相同）。
  - `TestSingleOrdersAvoidFabricMachines`（有非 fabric 空闲机时单机订单不选 fabric 机器；全是 fabric 机器时照常选）。
  - `TestPlaceSystemClusterAtomic`（一台不满足 → 全部回滚、无实例无集群；成功 → 系统实例、lease 10 年、集群 `system=true`）、`TestReleaseSystemCluster`。
  - `TestClusterSpecForMacvlanVLAN`（vlan、iface 自动选 mlx5、无 host_ip、rdma 判定）、`TestClusterSpecForMissingRank`。
  - `TestClusterViewStatus`（forming / ready / degraded 三态，用 `last_state_report` JSON 构造网络状态）、`TestClusterForUserIsolation`、`TestClusterAvailability`。
- [ ] **Step 2: 运行确认失败**：`go test ./internal/rental/ -run 'TestCluster|TestOldAgent|TestSingleOrders|TestPlaceSystemCluster|TestReleaseSystemCluster' 2>&1 | head -5`
- [ ] **Step 3: 实现**
- [ ] **Step 4: 运行并提交**
  Run: `go vet ./... && go test ./internal/rental/ ./internal/app/ 2>&1 | tail -5`
  Expected: 全部通过（现有 rental / app 测试不受影响）。
  ```bash
  git add internal/rental
  git commit -m "feat(rental): instance clusters with same-fabric atomic placement, system clusters for llm node groups, cluster spec builder, availability and views

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 7: appdeploy 集群部署

**Files:**
- Create: `internal/appdeploy/cluster.go`、`internal/appdeploy/cluster_status.go`、`internal/appdeploy/cluster_test.go`、`internal/appdeploy/cluster_status_test.go`
- Modify: `internal/appdeploy/service.go`（`Create` 对 `required` 应用 → `ErrAppClusterRequired`；成员部署的 `Redeploy / Stop / Start / Remove` 用户入口 → `ErrAppClusterManaged`，内部集群操作走未导出路径）、`internal/appdeploy/bundle.go`（`BundleInput.ClusterRole / ClusterDeploymentID`，`ReservedEnv` 只在非空时追加两行）、`internal/appdeploy/runner.go`（worker 且 `!Spec.Cluster.WorkerPorts` 时 running 不登记端口；bundle 传角色）、`internal/appdeploy/hooks.go`（`OnInstanceMigrated` 保留 `cluster_deployment_id / cluster_role`）、`internal/appdeploy/view.go`（视图 `cluster_role`、应用目录视图 `cluster`）、`internal/appdeploy/catalog.go`（`ListPublished` 结果带 `Spec.Cluster` 摘要，供控制台区分）
- Test: 上述 + `bundle_test.go`、`runner_test.go`、`service_test.go`（追加）

**Interfaces:**
- Consumes: Task 1（表、`ClusterSupport`、`ClusterCreateRequest` 等类型、错误码）；成员由 SQL `SELECT … FROM instances WHERE cluster_id = $1 AND state IN ('creating','running','stopped') ORDER BY cluster_rank, id` 读取，集群行 `SELECT … FROM instance_clusters WHERE id = $1 FOR UPDATE`（不调用 rental 的集群函数，避免与 Task 6 并行时互相等待）。
- Produces：
  ```go
  func (s *Service) CreateCluster(ctx context.Context, req ClusterCreateRequest) (ClusterDeployment, error)
  func (s *Service) GetClusterDeployment(ctx context.Context, id string) (ClusterDeployment, error)      // 不存在 → ErrAppNotFound 同类的 app_deployment_not_found（沿用现有部署不存在错误）
  func (s *Service) LiveClusterDeployment(ctx context.Context, clusterID string) (*ClusterDeployment, error) // 无 → nil, nil
  func (s *Service) ClusterDeploymentForUser(ctx context.Context, userID int64, clusterID, id string) (ClusterDeployment, error)
  func (s *Service) RedeployCluster(ctx context.Context, req ClusterRedeployRequest) (ClusterDeployment, error)
  func (s *Service) StopCluster(ctx context.Context, userID int64, id, ip string) (ClusterDeployment, error)
  func (s *Service) StartCluster(ctx context.Context, userID int64, id, ip string) (ClusterDeployment, error)
  func (s *Service) RemoveCluster(ctx context.Context, userID int64, id, ip string) (ClusterDeployment, error)
  func AggregateCluster(members []ClusterMemberDeployment) (status, health, phase string, progress int)   // 规格 §6.4，表驱动可测
  ```
- 行为：规格 §6.2～§6.6。要点：锁序 集群行 → 成员实例（ID 升序）→ `app_deployments`；系统集群只允许 `SystemUserID`；`CreateCluster` 的 `app_cluster_exists` 由唯一索引兜底（并发 → 唯一冲突映射为 `ErrAppClusterExists`）；`RedeployCluster` 时若有成员实例不是当前成员（换机），为当前成员中缺部署的 rank 补建 queued 部署、把非当前成员的部署标 `removed`；审计动作 `cluster.app_*`。保留变量追加顺序固定：`SPARK_CLUSTER_ROLE`、`SPARK_CLUSTER_DEPLOYMENT`，位于 `SPARK_GIT_MIRROR` 之后。

- [ ] **Step 1: 写失败的测试**
  - `TestCreateClusterValidation`（不是 owner、集群非 active、成员数 ≠ size、size 越界 → `AppClusterSize`、已有集群部署 → exists、`mode` 无 → unsupported、镜像不兼容、某成员已有该应用）、`TestCreateClusterRoles`（N 条部署、rank0 head、env 相同、事件 / 审计各一）、`TestCreateClusterConcurrent`（两个并发 → 一个 exists）。
  - `TestRequiredAppRejectsSingleCreate`、`TestMemberDeploymentUserOpsManaged`（`Redeploy / Stop / Start / Remove` → `app_cluster_managed`；`Get / Log / Events` 正常）。
  - `TestReservedEnvUnchangedForNonCluster`（`ReservedEnv` 对 `ClusterRole == ""` 的输入与改动前的硬编码期望逐项相同；`RenderEnv` 字节相同）、`TestReservedEnvClusterRole`。
  - `TestWorkerPortsNotRegistered`（runner 把 worker 推到 running → `instance_ports` 无该部署端口；head 有）。
  - `TestAggregateClusterTable`（规格 §6.4 每一行一个用例）。
  - `TestRedeployClusterAfterMigration`（模拟成员换机：新实例 queued 部署保留角色 → 聚合 stale → RedeployCluster 后非当前成员部署 removed、全员 queued）、`TestStopStartRemoveCluster`。
- [ ] **Step 2: 运行确认失败**：`go test ./internal/appdeploy/ -run 'Cluster|ReservedEnv|WorkerPorts|RequiredApp|MemberDeployment' 2>&1 | head -5`
- [ ] **Step 3: 实现**
- [ ] **Step 4: 运行并提交**
  Run: `go vet ./... && go test ./internal/appdeploy/... ./internal/app/ ./internal/api/ 2>&1 | tail -5`
  ```bash
  git add internal/appdeploy
  git commit -m "feat(appdeploy): cluster deployments (one deployment per member, head/worker roles, aggregate status, group redeploy/stop/start/remove)

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 8: 应用包 `vllm-node 1.1.0`（多机角色）与 `vllm-cluster 1.0.0`

**Files:**
- Modify: `deploy/apps/vllm-node/{app.json,spec.json,app.sh}`（版本 1.1.0）、`deploy/apps/README.md`（新行与「集群应用」一节）、`internal/appdeploy/apps_dir_test.go`
- Create: `deploy/apps/vllm-cluster/{app.json,spec.json,app.sh}`（注意：部署包只有 `app.json / spec.json / app.sh` 三个固定文件（`bundle.go`），worker 的健康服务由 `app.sh` 内的 heredoc 在运行时生成 `$SPARK_APP_DIR/health.py`，不要另建文件）

**Interfaces:**
- Consumes: Task 1 的 `Spec.Cluster`（`ValidateSpec` 接受 `cluster` 键）、`MaxTimeoutMinutes = 480`；运行时变量 `SPARK_CLUSTER_ROLE`（Task 7 注入）与 `/.spark/cluster.env`（Task 3 写）。
- Produces：
  - `vllm-node/spec.json` 1.1.0：1.0.0 全部 + `cluster: {"mode": "optional", "min_size": 2, "max_size": 4}`、`timeout_minutes: 360`、变量 `TENSOR_PARALLEL_SIZE`、`PIPELINE_PARALLEL_SIZE`（int 1～8，默认 1）——总数 14 ≤ 16。`app.json` `version 1.1.0`，changelog「支持 2 / 4 台多机（Ray）」。
  - `vllm-cluster`：`app.json` `{"name":"vLLM 多机推理","summary":"在 2～4 台集群上部署一个大模型（OpenAI 兼容接口）","category":"llm","icon":"llm","sort":15,"version":"1.0.0","images":["LLM Serving"],"changelog":"初始版本"}`；`spec.json` 规格 §6.7 变量表、`cluster: {"mode":"required","min_size":2,"max_size":4}`、端口 8000（`/docs`、`/health`）、`timeout_minutes 360`、`min_data_free_gib 100`、`links` 同 vllm。
  - `app.sh`（两个文件的 `# ---- spark-cluster-ray begin ----` 到 `# ---- spark-cluster-ray end ----` 之间逐字相同）：规格 §6.7 的 head / worker / stop 流程；head 的等待脚本：
    ```sh
    wait_nodes() { # wait_nodes <N> <超时秒>：Ray 里存活且带 GPU 的节点数达到 N
      python3 - "$1" "$2" <<'PY'
    import sys, time, ray
    n, deadline = int(sys.argv[1]), time.time() + int(sys.argv[2])
    ray.init(address="auto", logging_level="ERROR")
    while time.time() < deadline:
        alive = [x for x in ray.nodes() if x.get("Alive") and x.get("Resources", {}).get("GPU", 0) >= 1]
        if len(alive) >= n: sys.exit(0)
        time.sleep(5)
    sys.exit(1)
    PY
    }
    ```
    worker 健康服务（`health.py`，heredoc 生成）：`http.server` 监听 `127.0.0.1:$SPARK_PORT`，`GET /health` → 遍历 `/proc/*/comm` 有 `raylet` 时 200，否则 503；其它路径 404。
  - `vllm-cluster` 的 `TENSOR_PARALLEL_SIZE` 为空时取 `$SPARK_CLUSTER_SIZE`；`TP × PP != SPARK_CLUSTER_SIZE` → `spark_status install 0 "TP×PP 必须等于集群台数"` 并退出 2。
- 测试（`apps_dir_test.go` 追加）：`TestVLLMNodeClusterSpec`（1.1.0、cluster optional、两个新变量、internal 仍为 true）、`TestVLLMClusterSpec`（required、min 2 max 4、8000 健康 `/health`）、`TestClusterRaySectionIdentical`（两段逐字相同）、`TestClusterScriptsReferenceClusterEnv`（脚本含 `. /.spark/cluster.env`、`--distributed-executor-backend ray`、`--object-store-memory`）。现有 `TestAppsDir` 自动覆盖新目录（POSIX 语法、变量声明检查）。

- [ ] **Step 1: 写失败的测试**（先只改 `app.json` 版本号，跑 `TestAppsDir` 看到新断言失败）
- [ ] **Step 2: 写 / 改三份文件 ×2**
- [ ] **Step 3: 运行并提交**
  Run: `go test ./internal/appdeploy/ -run 'TestAppsDir|TestVLLM|TestCluster' 2>&1 | tail -3 && sh -n deploy/apps/vllm-node/app.sh && sh -n deploy/apps/vllm-cluster/app.sh`
  ```bash
  git add deploy/apps internal/appdeploy/apps_dir_test.go
  git commit -m "feat(apps): vllm-node 1.1.0 with Ray head/worker roles, new public vllm-cluster app

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 9: llm 多机节点组（模型字段、Controller、Prober、Router、后台操作、池视图、不变量）

**Files:**
- Create: `internal/llm/groups.go`（组的建 / 推进 / 不健康 / 释放）、`internal/llm/groups_test.go`、`internal/llm/groups_helpers_test.go`（fake 的集群方法）
- Modify: `internal/llm/{llm.go,consts.go,types.go,models.go,models_test.go,nodes.go,nodes_test.go,controller.go,controller_test.go,prober.go,prober_test.go,router.go,router_test.go,invariants.go,invariants_test.go,admin_views.go,helpers_test.go,nodes_helpers_test.go}`
- Test: 上述

**Interfaces:**
- Consumes: Task 1 的迁移列、`rental.SystemClusterRequest / SystemClusterResult / Cluster`、`appdeploy.ClusterCreateRequest / ClusterRedeployRequest / ClusterDeployment / ClusterStatus*`。**只用 fake 实现测试**（rental / appdeploy 的真实方法在 Task 6 / 7）。
- Produces：
  ```go
  // RentalOps 增：
  PlaceSystemCluster(ctx context.Context, req rental.SystemClusterRequest) (rental.SystemClusterResult, error)
  ReleaseSystemCluster(ctx context.Context, clusterID string) error
  // AppOps 增：
  CreateCluster(ctx context.Context, req appdeploy.ClusterCreateRequest) (appdeploy.ClusterDeployment, error)
  GetClusterDeployment(ctx context.Context, id string) (appdeploy.ClusterDeployment, error)
  LiveClusterDeployment(ctx context.Context, clusterID string) (*appdeploy.ClusterDeployment, error)
  RedeployCluster(ctx context.Context, req appdeploy.ClusterRedeployRequest) (appdeploy.ClusterDeployment, error)
  // Model / ModelInput 增 NodesPerReplica, TensorParallel, PipelineParallel int（json nodes_per_replica / tensor_parallel / pipeline_parallel）
  // Node 增 Size int; ClusterID *string; AppClusterID *string（json size / cluster_id / app_cluster_id）
  type NodeMember struct{ NodeID string; Rank int; MachineID int64; InstanceID string; Live bool }
  const GroupRebuildAfter = 2 * time.Minute; const GroupSuspendFailAfter = 10 * time.Minute
  func ProvisionTimeoutFor(v appdeploy.Version) time.Duration
  // NodeReservedEnv 增 "TENSOR_PARALLEL_SIZE", "PIPELINE_PARALLEL_SIZE"；PlatformNodeEnv 仅 NodesPerReplica > 1 时填这两项
  // NodeRow 增 Members []NodeMemberRow `json:"members"`（size = 1 为空数组）、AppCluster *NodeAppCluster `json:"app_cluster"`
  type NodeMemberRow struct{ Rank int `json:"rank"`; MachineID int64 `json:"machine_id"`; MachineName string `json:"machine_name"`; InstanceID string `json:"instance_id"`; InstanceState string `json:"instance_state"`; Suspended bool `json:"suspended"`; Deployment *NodeDeployment `json:"deployment"` }
  type NodeAppCluster struct{ ID string `json:"id"`; Status string `json:"status"`; Health string `json:"health"`; Progress int `json:"progress"` }
  // PoolView 增 Fabrics []PoolFabric `json:"fabrics"`；PoolMachine 增 FabricID *int64 `json:"fabric_id"`、FabricName string `json:"fabric_name"`；PoolNode 增 Rank, Size int
  type PoolFabric struct{ FabricID int64 `json:"fabric_id"`; Name, Kind string; RDMA bool; Total, Free int; Groups map[string]int `json:"groups"` } // "2"/"4" → 可组副本数
  ```
- 行为：规格 §7.1～§7.7。要点：
  - 模型校验与零值补齐（`TensorParallel` 默认 = N、`PipelineParallel` 默认 1）；修改 N 时有非终态节点 → `LLMModelInvalid("nodes_per_replica", "请先把副本数调为 0 并等节点全部释放，再修改每副本机器数")`；`checkNodeParams` 对 N > 1 校验版本 `Spec.Cluster != nil && MaxSize >= N`。
  - Controller：`reconcile` 的缺额按 N 降序处理；`candidateMachines` 排序 `(m.fabric_id IS NOT NULL), m.id`；`candidateFabrics` SQL（`candidateCond` + `m.cluster_ready` + 能力位 + 排除 `llm_node_members WHERE live` 的机器，`GROUP BY m.fabric_id HAVING count(*) >= N`，`direct` 只给 2）；`createGroupNode`、组推进 / unhealthy / draining / removing / 孤儿 / `failNode` 按规格 §7.3；`ProvisionTimeoutFor`（组与单机都改用它，单机按 `vllm-node` 版本取）。
  - `mutateNode`：状态进入 `NodeTerminalStates` 时同事务 `UPDATE llm_node_members SET live = false WHERE node_id = $1`。
  - Prober / Router：规格 §7.4 / §7.5。
  - 后台：`AdminDrainNode / AdminRemoveNode` 对组释放全部成员；`AdminRebuildNode` 组 `ready / unhealthy` → `RedeployCluster`，组 `failed` → `ErrLLMNodeInvalidState`；`MachineHasLiveNode` 含成员；`ListNodes / GetNode` 带成员与集群部署聚合；`PoolView` 按 fabric。
  - 不变量 ⑧～⑩ 追加到 `CheckInvariants`。

- [ ] **Step 1: 写失败的测试**（fake RentalOps / AppOps 增集群方法与可注入状态；假时钟）
  - 模型：`TestModelParallelValidation`（N ∉ {1,2,4}、TP×PP ≠ N、embedding N 2、版本不支持集群、改 N 有节点被拒 / 无节点允许）、`TestPlatformNodeEnvSingleUnchanged`（N = 1 输出与改动前硬编码 map 相同）、`TestPlatformNodeEnvGroup`。
  - 建组：`TestProvisionGroupPicksFabricWithN`（两个 fabric 3 台 / 2 台，N=2 → 选 2 台的（最贴合））、`TestProvisionLargerNFirst`、`TestSingleNodePrefersNonFabric`、`TestProvisionGroupShortAlert`、`TestCreateGroupNodeInsertFailureReleases`。
  - 推进：`TestAdvanceGroupWaitsAllRunning`、`TestAdvanceGroupCreatesClusterDeployment`（env 含 TP/PP）、`TestAdvanceGroupClaimsExisting`、`TestAdvanceGroupPromotesViaHeadProbe`、`TestAdvanceGroupMemberGoneFails`、`TestAdvanceGroupTimeout`（`ProvisionTimeoutFor`）。
  - unhealthy：`TestGroupMemberGoneFailsWholeGroup`（`ReleaseSystemCluster` 被调一次、成员 `live=false`）、`TestGroupMachineFaultyFails`、`TestGroupSuspendedTenMinutesFails`（9 分钟仍 unhealthy、10 分钟 failed）、`TestGroupCrashRedeploysAll`（2 分钟后 `RedeployCluster`、rebuild_count 1；3 次后 failed + critical）。
  - `TestDrainGroupReleasesAll`、`TestRemovingGroupDoneWhenAllEnded`、`TestOrphansIgnoreGroupMembers`。
  - Prober：`TestProberGroupMemberSuspendedCountsFailure`、`TestProberGroupDeploymentFailedImmediate`。
  - Router：`TestRouterExcludesGroupWithSuspendedMember`（真实 SQL：用 helpers 造 instances 与 members）。
  - 后台：`TestAdminRebuildGroup`、`TestAdminRemoveGroup`、`TestPoolViewFabrics`、`TestMachineHasLiveNodeMember`。
  - 不变量：`TestInvariantsGroup`（成员缺一行 / rank 不连续 / 终态仍 live 各报对应编号）。
  - 回归：`go test ./internal/llm/` 现有用例全部仍通过（N = 1 路径不变）。
- [ ] **Step 2: 运行确认失败**：`go test ./internal/llm/ -run 'Group|Parallel|PlatformNodeEnv|Fabric|NonFabric|LargerN' 2>&1 | head -5`
- [ ] **Step 3: 实现**
- [ ] **Step 4: 运行并提交**
  Run: `go vet ./... && go test ./internal/llm/... ./internal/llmgw/... 2>&1 | tail -5`
  ```bash
  git add internal/llm
  git commit -m "feat(llm): multi-node replicas (node groups on one fabric, Ray head routing, group health, failover and rebuild, pool by fabric)

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 10: rental 集群生命周期（级联中止、对齐、整组续费 / 自动续费 / 释放 / 延长 / 重新下发、成员守卫、换机 / 退款、指令结果、提醒、不变量）

**Files:**
- Create: `internal/rental/cluster_life.go`、`internal/rental/cluster_invariants.go`、`internal/rental/cluster_life_test.go`、`internal/rental/cluster_invariants_test.go`
- Modify: `internal/rental/renew.go`（`Renew / RenewQuantity / Release` 守卫）、`internal/rental/autorenew.go`（`SetAutoRenew` 守卫、`AutoRenewDue` 排除成员）、`internal/rental/admin_ops.go`（`AdminExtend` 守卫）、`internal/rental/migrate.go`（成员换机）、`internal/rental/refund.go`（成员退款释放后下发）、`internal/rental/results.go`（`configure_cluster` 结果）、`internal/rental/sweeper.go`（`FailTimedOutCommands` 超时表、任务顺序加 `AutoRenewClustersDue`、`SweepClusters`、`SendReminders` 去重）、`internal/rental/reconcile.go`（`cluster` 状态告警）、`internal/rental/service.go`（头注释锁序表）
- Test: 上述 + 追加到 `renew_test.go`、`autorenew_test.go`、`migrate_test.go`、`refund_test.go`、`sweeper_test.go`、`reconcile_test.go`

**Interfaces:**
- Consumes: Task 6 的 `ClusterSpecFor`、`enqueueCreateSpec`、`GetClusterView`、`placeOnMachines` 的 `FabricID / ClusterOnly`；Task 1 的错误码与常量。
- Produces：
  ```go
  func (s *Service) RenewCluster(ctx context.Context, userID int64, clusterID string, d catalog.Duration, quantity int, idempotencyKey string) ([]Order, error)
  func (s *Service) SetClusterAutoRenew(ctx context.Context, userID int64, clusterID string, enabled bool, d catalog.Duration, ip string) (ClusterView, error)
  func (s *Service) ReleaseCluster(ctx context.Context, userID int64, clusterID, ip string) error
  func (s *Service) AdminExtendCluster(ctx context.Context, actor audit.Actor, clusterID string, seconds int64, reason string) (ClusterView, error)
  func (s *Service) AdminResyncCluster(ctx context.Context, actor audit.Actor, clusterID string) (ClusterView, error)
  func (s *Service) SweepClusters(ctx context.Context) (int, error)
  func (s *Service) AutoRenewClustersDue(ctx context.Context) (int, error)
  func CheckClusterInvariants(ctx context.Context, pool *pgxpool.Pool) error   // 规格 §5.9 五条，违背返回 "cluster invariant N: …"
  ```
- 行为：规格 §5.5～§5.8。要点：
  - 守卫：`Renew / RenewQuantity / SetAutoRenew / Release`（用户）与 `AdminExtend` 对 `inst.ClusterID != nil` → `ErrClusterMember`（在锁实例之后判断）。
  - `SweepClusters` 三步（`sweepEach` 模式、每集群独立事务、锁序 集群 → 成员）；中止时退款用 `refundAll`、`beginWipe`，机器不 `MarkFaulty`；对齐时 `refreshLease`。
  - `RenewCluster`：幂等前缀 `key#`；全员 `renewFits` 后再逐个 `charge + addTime`；任何一个失败整笔回滚。
  - `AutoRenewClustersDue`：规格 §5.7；短信只发 rank 最小成员。
  - `SendReminders`：成员只对 rank 最小的活动成员发，其余成员同一事务置 `reminded_*`。
  - `AdminMigrate`：成员 → 先锁集群行（读实例拿 `cluster_id`，锁集群，再 `lockInstance` 并复核 `cluster_id` 未变）；候选 `pickSpec{Immediate, FabricID: 集群.fabric_id, ClusterOnly: true}`；新实例 `cluster_id / cluster_rank` 继承；集群 `version + 1`；新实例 `enqueueCreateSpec`（新 Spec）；其余活动成员 `Enqueue(CmdConfigureCluster, ConfigureClusterPayload{ClusterSpecFor(rank)})`。非成员路径不变。
  - `RefundRelease`：成员 → 锁序 集群 → 实例；释放后 `version + 1` 并对其余活动成员下发 ConfigureCluster。
  - `HandleCommandResult`：`configure_cluster` 成功 → 只完成指令；失败 → `RaiseAlertOnce(warning, cluster_net_error, machine, instance, "实例 <id> 的集群网络异常")`。`FailTimedOutCommands` 的超时表加 `configure_cluster → ConfigureClusterCommandTimeout`。
  - `Reconcile`：报告里实例 `Cluster` 以 `error:` 开头 → 同一告警（一次）。
  - Sweep 顺序：`… → AutoRenewDue → AutoRenewClustersDue → ExpireDue → WipeStale → FailStuckCreating → FailTimedOutCommands → SweepClusters → ReapSSHSessions → SendReminders`。

- [ ] **Step 1: 写失败的测试**（用 Task 6 的辅助函数造 2 / 4 台集群）
  - `TestClusterAbortRefundsAll`：rank1 Create 失败（`HandleCommandResult(false)`）→ `SweepClusters` → rank0 `wiping`、两张订单 `refunded_amount = amount`、rank1 机器 faulty、rank0 机器不 faulty、集群 `failed`、告警 `cluster_failed` 一条；再跑一轮无副作用。
  - `TestClusterReadyAndAlign`（两成员相差 3 分钟 running → 对齐到晚的、`ready_at` 置位）、`TestClusterRealignAfterSuspend`、`TestClusterEndsWhenAllReleased`（子网可被新集群复用）。
  - `TestClusterMemberGuards`（四个用户操作 + `AdminExtend` → `cluster_member`；改密码 / 端口仍可）。
  - `TestRenewClusterAllOrNothing`（余额只够一台 → 两台都没续、无订单）、`TestRenewClusterIdempotent`（同键重放返回同两单）、`TestRenewClusterBlocked`（一个成员被后续预订挡住 → `renew_blocked`）。
  - `TestAutoRenewClustersDue`（成功：全员到期同时后移、一条事件；失败：全员标失败、短信一条）、`TestAutoRenewDueSkipsMembers`、`TestRemindersOncePerCluster`。
  - `TestReleaseCluster`、`TestAdminExtendCluster`、`TestAdminResyncCluster`（版本 + 1、每个活动成员一条 `configure_cluster`）。
  - `TestMigrateClusterMember`（新机器同 fabric、rank 不变、版本 2、新 Create 带新 Spec、对端收到 ConfigureCluster；fabric 内无空闲 → `no_capacity`）、`TestRefundReleaseClusterMember`。
  - `TestConfigureClusterResultAndTimeout`、`TestReconcileClusterErrorAlert`、`TestCheckClusterInvariants`（每条一个反例）。
- [ ] **Step 2: 运行确认失败**：`go test ./internal/rental/ -run 'TestCluster|TestRenewCluster|TestAutoRenewCluster|TestReleaseCluster|TestAdmin.*Cluster|TestMigrateClusterMember|TestRefundReleaseClusterMember|TestConfigureCluster|TestReconcileCluster|TestReminders|TestAutoRenewDueSkips|TestCheckCluster' 2>&1 | head -5`
- [ ] **Step 3: 实现**
- [ ] **Step 4: 运行并提交**
  Run: `go vet ./... && go test ./internal/rental/ ./internal/app/ ./internal/billing/ 2>&1 | tail -5`
  ```bash
  git add internal/rental
  git commit -m "feat(rental): cluster lifecycle (abort with full refunds, expiry alignment, group renew/auto-renew/release/extend/resync, member guards, migrate/refund members, configure_cluster results, invariants)

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 11: 控制台 API

**Files:**
- Create: `internal/api/clusters.go`、`internal/api/clusters_test.go`
- Modify: `internal/api/server.go`（路由）、`internal/api/booking.go`（`cluster` 字段、响应 `cluster`）、`internal/api/market.go`（`/products` 的 `clusters`、`/availability?cluster_size=`）、`internal/api/instances.go`（实例视图 `cluster`、成员守卫错误透传）、`internal/api/apps.go`（应用视图 `cluster`、`cluster_role`；单实例「添加应用」列表排除 `required`）、`internal/api/view_internal_test.go`、`internal/api/booking_test.go`、`internal/api/apps_test.go`

**Interfaces:**
- Consumes: Task 6 / 10 的 rental 集群函数、Task 7 的 appdeploy 集群函数、Task 5 无（控制台不展示 fabric 细节，只用 `ClusterView.Network / FabricKind`）。
- Produces：规格 §10.1 全部端点与字段（JSON 字段名照抄）。`ClusterAppView`：`{id, app_id, app_slug, app_name, version_id, version, status, health, phase, progress, url (head 端口 URL，可为空), members:[{rank, role, instance_id, state, health, progress, current}]}`。渲染 `ClusterView` 时对用户隐藏 `machine_id`（只给 `machine_label`）、`fabric_id`、`subnet_index`。`POST /clusters/{id}/renew` 要求 `Idempotency-Key`（与 `/instances/{id}/renew` 相同的中间件 / 校验）。所有 `/clusters/*` 在 `requireUser` 组；`/availability` 与 `/products` 仍匿名组。

- [ ] **Step 1: 写失败的测试**（`api_test.go` 的测试服务器 + fake Agent 造集群；或直接用 rental 辅助函数造数据）：`TestBookingsClusterFlag`（2 台成功、3 台 400、带 start_at 400）、`TestAvailabilityClusterSize`、`TestProductsClusters`、`TestClustersListAndDetail`（他人集群 404、系统集群 404、字段不含 machine_id）、`TestClusterRenewIdempotent`、`TestClusterAutoRenew`、`TestClusterRelease`、`TestInstanceMemberGuards`（三个接口 409 `cluster_member`）、`TestInstanceViewCluster`、`TestClusterAppsFlow`（部署 / 查询 / redeploy / stop / start / 删除；成员部署单实例接口 409 `app_cluster_managed`）、`TestAppCatalogClusterField`。
- [ ] **Step 2: 运行确认失败**：`go test ./internal/api/ -run 'Cluster|Availability|Products' 2>&1 | head -5`
- [ ] **Step 3: 实现**
- [ ] **Step 4: 运行并提交**
  Run: `go vet ./... && go test ./internal/api/ ./internal/app/ 2>&1 | tail -5`
  ```bash
  git add internal/api
  git commit -m "feat(api): console cluster endpoints (order, availability, detail, renew/auto-renew/release, cluster apps), member guards

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 12: 后台 API

**Files:**
- Create: `internal/adminapi/fabrics.go`、`internal/adminapi/fabrics_test.go`、`internal/adminapi/clusters.go`、`internal/adminapi/clusters_test.go`
- Modify: `internal/adminapi/server.go`（路由与角色）、`internal/adminapi/machines.go`（`PUT /machines/{id}` 新字段、`GET /machines/{id}` 新字段、列表列与筛选）、`internal/adminapi/instances.go`（实例详情 `cluster` 区块、`extend` 的 `cluster_member` 透传）、`internal/adminapi/llm.go`（模型字段、`/llm/pool` fabrics、节点成员）、`internal/adminapi/dashboard.go`（`clusters` 计数）、`internal/adminapi/views.go`、`internal/adminapi/permissions_test.go`、`internal/adminapi/machines_test.go`、`internal/adminapi/llm_test.go`

**Interfaces:**
- Consumes: Task 5 的 fleet fabric 与机器字段；Task 6 / 10 的 rental 集群函数；Task 7 的集群部署；Task 9 的 llm 字段与视图。
- Produces：规格 §10.2 全部端点；角色：`GET /fabrics`、`GET /clusters*`、`GET /dashboard` 为 `allRoles`；`POST/PUT/DELETE /fabrics*`、`PUT /machines/{id}`、`POST /clusters/{id}/extend`、`POST /clusters/{id}/resync` 为 `opsRoles`（不要求 TOTP，与现有 `extend` 一致）；`/llm/*` 维持计划 9 的角色。`permissions_test.go` 登记全部新路由。`GET /clusters/{id}` 响应：`{cluster: ClusterView（含 machine_id、fabric_id）, app: ClusterDeployment|null, events: 最近 50 条成员实例事件（合并按时间倒序）}`。

- [ ] **Step 1: 写失败的测试**：`TestFabricsCRUDRoles`（finance 403 写、ops 成功、审计三种）、`TestMachineFabricUpdate`（direct 满 → 409 `fabric_full`、有成员 → 409）、`TestMachineDetailNetInfo`、`TestAdminClustersListFilter`、`TestAdminClusterDetail`、`TestAdminClusterExtendResync`、`TestInstanceDetailClusterBlock`、`TestInstanceExtendMember409`、`TestLLMModelParallelFields`、`TestLLMPoolFabrics`、`TestLLMNodeMembers`、`TestDashboardClusters`、`permissions_test` 表驱动通过。
- [ ] **Step 2: 运行确认失败**：`go test ./internal/adminapi/ -run 'Fabric|Cluster|NetInfo|Parallel|PoolFabrics|NodeMembers|Dashboard|Permissions' 2>&1 | head -5`
- [ ] **Step 3: 实现**
- [ ] **Step 4: 运行并提交**
  Run: `go vet ./... && go test ./internal/adminapi/ 2>&1 | tail -5`
  ```bash
  git add internal/adminapi
  git commit -m "feat(adminapi): fabrics, machine NIC and fabric settings, clusters list/detail/extend/resync, llm multi-node fields and pool by fabric

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 13: 装配、清扫接入、e2e 钩子与 Go 集成测试

**Files:**
- Modify: `internal/platform/config/config.go`（`ClusterSubnetPool netip.Prefix` ← `SPARK_CLUSTER_SUBNET_POOL`，默认 `10.210.0.0/16`，必须 /16、不与 `10.200.0.0/24` 重叠）、`internal/platform/config/config_test.go`、`internal/app/app.go`（`rental.Deps.ClusterSubnetPool`；`llm.Deps` 的 `RentalOps / AppOps` 自然满足新接口；`agentgw` 的 Fleet 依赖带 `RecordHello / RecordNetInfo`）、`cmd/platform/main.go`（清扫顺序由 rental 内部完成，确认无需改；如 `RunSweeper` 外挂任务列表需要则追加）、`internal/e2ehooks/hooks.go`（`/hooks/machines` 增 `fabric_id`、`net`、`old_agent`）、新建 `internal/e2ehooks/clusters.go`（`POST /hooks/fabrics {name, kind, isolation, rdma, vlan_min?, vlan_max?}` 直接经 `Fleet.CreateFabric` 建 fabric，返回 id）、`internal/e2ehooks/hooks_test.go`、`README.md`（配置表加 `SPARK_CLUSTER_SUBNET_POOL`；部署注意：先升级 Agent、CX-7 口勿配 IP、fabric 登记步骤）
- Create: `internal/app/cluster_e2e_test.go`、`internal/app/llm_multinode_e2e_test.go`

**Interfaces:**
- Consumes: 全部后端任务；`fakeagent.Options.Net / ClusterNet`（Task 3）。
- Produces：钩子 `/hooks/machines` 新参数：`fabric_id`（登记后 `UpdateMachine{SetFabric}`）、`net`（`"lan"`：一块 `eth0` 有线；`"cx7"`：`eth0` + `enp1s0f1np1`（mlx5_core，up，无 RDMA）；`"cx7-rdma"`：同上且 `rdma_device=rocep1s0f1, rdma_active, uverbs=uverbs0`）、`old_agent`（Features = SharedSlots）。集成测试名（规格 §16.2）：`TestClusterOrderE2E`、`TestClusterAbortE2E`、`TestClusterMemberMigrateE2E`、`TestLLMTwoNodeGroupE2E`、`TestLLMGroupCrashRebuildE2E`；每个用例结束调用 `rental.CheckClusterInvariants` 与（llm 用例）`llm.CheckInvariants`。

- [ ] **Step 1: 写失败的测试**（集成测试与 config 测试）
- [ ] **Step 2: 运行确认失败**：`go test ./internal/app/ -run 'Cluster|MultiNode|TwoNode|GroupCrash' 2>&1 | head -5`
- [ ] **Step 3: 实现装配与钩子；修复集成中暴露的问题（只改归属本任务的文件；若问题在别的任务文件里，最小修复并在汇报中逐条列出）**
- [ ] **Step 4: 运行并提交**
  Run: `go vet ./... && go test ./... 2>&1 | tail -15`
  Expected: 全量通过。
  ```bash
  git add internal/app internal/platform/config internal/e2ehooks cmd README.md
  git commit -m "feat(app): wire multi-node clusters, e2e hooks for fabrics and NIC presets, cluster and llm multi-node integration tests

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 14: 控制台页面

**Files:**
- Create: `web/console/src/views/ClusterDetailView.vue`、`web/console/src/components/clusters/{ClusterCard.vue,ClusterMembers.vue,ClusterConnectInfo.vue,ClusterRenewDialog.vue,ClusterAppCard.vue,ClusterAppDeployDialog.vue}`、`web/console/src/logic/cluster.ts`、`web/console/src/logic/__tests__/cluster.spec.ts`、`web/console/src/views/__tests__/ClusterDetailView.spec.ts`
- Modify: `web/console/src/router.ts`（`/clusters/:id`）、`web/console/src/components/RentDialog.vue`（租用方式）、`web/console/src/logic/rent.ts`（集群总价与参数）、`web/console/src/views/InstancesView.vue`（集群折叠）、`web/console/src/views/InstanceDetailView.vue`（成员横幅、按钮置灰）、`web/console/src/components/AppsCard.vue`（集群部署成员只读、`required` 应用不出现在添加列表）、`web/console/src/views/HelpView.vue`（「多机集群」一节）

**Interfaces:**
- Consumes: Task 1 前端类型 / 文案；Task 11 的 API（字段照规格 §10.1）。
- Produces：`logic/cluster.ts`：`clusterTotalPrice(unit, size, quantity)`、`clusterStatusBadge(view)`、`connectSnippets(view) → {env, torchrun, ray}`（示例文本，含 `. /.spark/cluster.env`、`torchrun --nnodes $NNODES --node-rank $NODE_RANK --master-addr $MASTER_ADDR --master-port 29500`）、`defaultTP(size)`、`groupInstancesByCluster(instances)`。`data-testid`：`rent-mode`、`rent-cluster-network`、`rent-cluster-available`、`instances-cluster-card`、`cluster-status`、`cluster-members`、`cluster-connect`、`cluster-renew`、`cluster-auto-renew`、`cluster-release`、`cluster-app-deploy`、`cluster-app-status`、`cluster-app-members`、`instance-cluster-banner`。

- [ ] **Step 1: 写失败的单测**：`cluster.spec.ts`（总价、分组、片段含关键变量、TP 默认）、`ClusterDetailView.spec.ts`（mock http：成员表、降级提示、stale 提示、按钮状态）、`RentDialog` 的集群模式测试（追加到现有 `__tests__`）。
- [ ] **Step 2: 运行确认失败**：`cd web && npx vitest run console 2>&1 | tail -5`
- [ ] **Step 3: 实现**
- [ ] **Step 4: 运行并提交**
  Run: `cd web && npm test 2>&1 | tail -3 && npm run build 2>&1 | tail -3`
  ```bash
  git add web/console
  git commit -m "feat(console): cluster ordering, cluster detail page with members, connection info, renew/release and cluster apps

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 15: 后台页面

**Files:**
- Create: `web/admin/src/views/{FabricsView.vue,ClustersView.vue,ClusterDetailView.vue}`、`web/admin/src/components/{FabricDrawer.vue,MachineNetCard.vue}`、`web/admin/src/views/__tests__/{FabricsView.spec.ts,ClusterDetailView.spec.ts}`
- Modify: `web/admin/src/menu.ts`（「集群网络」`/fabrics`、「集群」`/clusters`）、`web/admin/src/__tests__/menu.spec.ts`（或现有位置）、`web/admin/src/router.ts`、`web/admin/src/views/MachineDetailView.vue`（`MachineNetCard`）、`web/admin/src/views/MachinesView.vue`（fabric 列、就绪 / RDMA 徽标、筛选）、`web/admin/src/views/InstanceDetailView.vue`（集群区块、延长 / 换机提示）、`web/admin/src/components/llm/{ModelDrawer.vue,NodesTab.vue,NodeDrawer.vue,AddPoolMachineDialog.vue}`、`web/admin/src/views/LlmView.vue`（池按 fabric）、`web/admin/src/views/DashboardView.vue`

**Interfaces:**
- Consumes: Task 1 类型 / 文案；Task 12 API。
- Produces：`data-testid`：`fabrics-table`、`fabric-create`、`fabric-drawer-kind`、`fabric-delete`、`machine-net-table`、`machine-fabric-select`、`machine-iface-select`、`machine-cluster-ready`、`clusters-table`、`cluster-detail-members`、`cluster-extend`、`cluster-resync`、`instance-cluster-block`、`llm-model-nodes-per-replica`、`llm-model-tp`、`llm-model-pp`、`llm-pool-fabrics`、`llm-node-members`。表单联动：`kind = lan` 时 isolation 固定 routed、RDMA 禁用；`cx7` 时 isolation ∈ direct / vlan，vlan 时显示范围输入；模型抽屉 TP × PP 必须等于 N，否则保存按钮禁用并提示。

- [ ] **Step 1: 写失败的单测**（`FabricsView.spec.ts` 联动与角色、`ClusterDetailView.spec.ts`、`menu.spec.ts` 两个新菜单、`ModelDrawer` 的 TP×PP 校验追加）
- [ ] **Step 2: 运行确认失败**：`cd web && npx vitest run admin 2>&1 | tail -5`
- [ ] **Step 3: 实现**
- [ ] **Step 4: 运行并提交**
  Run: `cd web && npm test 2>&1 | tail -3 && npm run build 2>&1 | tail -3`
  ```bash
  git add web/admin
  git commit -m "feat(admin): fabrics and clusters pages, machine NIC card, instance cluster block, llm multi-node model and pool views

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 16: Playwright 用例与覆盖表

**Files:**
- Create: `web/e2e/helpers/clusters.ts`、`web/e2e/console/clusters.spec.ts`、`web/e2e/admin/fabrics.spec.ts`、`web/e2e/admin/clusters.spec.ts`
- Modify: `web/e2e/coverage.json`（规格 §16.3 的 ID）、`web/e2e/admin/machines.spec.ts`（A-MCH-11～12）、`web/e2e/admin/instances.spec.ts`（A-INS-17）、`web/e2e/admin/llm.spec.ts`（A-LLM-10～12）、`web/e2e/helpers/hooks.ts`（`fabrics / machines` 新参数）

**Interfaces:**
- Consumes: Task 13 钩子（`/hooks/fabrics`、`/hooks/machines {count, fabric_id, net, old_agent}`、`/hooks/agent`、`/hooks/apps/seed`、`/hooks/apps/tick`、`/hooks/sweep`、`/hooks/llm/tick`、`/hooks/llm/upstream`）；Task 14 / 15 的 `data-testid`。
- Produces：`helpers/clusters.ts`：`seedFabric(kind, opts) → id`、`seedClusterMachines(count, fabricId, net)`、`orderCluster(page, size)`、`completeAgents()`。每个用例标题以覆盖表 ID 开头；`C-CLU-08 / 09` 用 `vllm-cluster`（`/hooks/apps/seed` 让 fake 应用走完 install / health）；`A-LLM-11 / 12` 用 `/hooks/llm/tick` 推进 2 台组（`/hooks/llm/upstream` 在 head 端口挂 FakeVLLM）。

- [ ] **Step 1: 写用例（先跑确认失败）**：`cd web && npx playwright test console/clusters.spec.ts admin/fabrics.spec.ts admin/clusters.spec.ts 2>&1 | tail -5`
- [ ] **Step 2: 修通（只改 `web/e2e/**`；页面缺 testid 时在汇报中列出并做最小修复）**
- [ ] **Step 3: 全量**
  Run: `make e2e 2>&1 | tail -15`（含 `npm run e2e:coverage` 核对）
  Expected: 全部通过、覆盖表一致。
  ```bash
  git add web/e2e
  git commit -m "test(e2e): cluster ordering, cluster apps, fabrics, admin clusters and llm multi-node coverage

  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  ```

---
### Task 17: 真机检查点 CP-0——单机回归（192.168.1.221）

**Files:**
- Create: `docs/superpowers/acceptance-plan13.md`（格式同 `acceptance-plan9.md`：环境、版本、每项步骤 / 命令 / 实际输出摘录 / 结论）、`deploy/image/tests/nccl_allreduce.py`（torch.distributed NCCL all-reduce，参数 `--size-mb`、`--iters`，打印 algbw / busbw；CP-1 起使用）

**步骤**（规格 §16.4 CP-0 全部 4 项）：
- [ ] `make agent` → 拷到 221 → 协调人 `sudo` 安装（`deploy/agent/install.sh` 同计划 2 的升级流程）→ `sudo spark-agent selfcheck` 输出存档。
- [ ] 平台（开发 Mac，`make run` 配置同计划 9 验收）迁移到 00026；后台机器详情网卡表、能力位截图 / 文本存档。
- [ ] 普通实例全流程、`sudo iptables -S SPARK-FWD`、`SPARK-IN`、`SPARK-CLU` 存档；N = 1 Token 商城节点抽查（模型 Qwen3-4B 已在 221 上缓存则复用）。
- [ ] 单台 fabric 下的集群选项「不可用」、cx7 判定「未找到 CX-7 网卡」。
- [ ] 发现的问题：在集成分支修复（派子代理，按文件归属）、回归后再记录通过。
- [ ] 提交：`git add docs/superpowers/acceptance-plan13.md deploy/image/tests && git commit -m "docs(acceptance): plan 13 CP-0 single-machine regression on 221 …"`

---
### Task 18: 真机检查点 CP-1——两台以太网（`lan` fabric）

**前置**：负责人准备清单第 1、2、5、6、8 项完成（2 台在同一内网、Agent 已装、镜像已 load、后台已登记）。协调人先 `ssh` 两台核对：`docker images | grep llm-serving`、`ip -br addr`、`sudo spark-agent selfcheck`。

**步骤**（规格 §16.4 CP-1 全部 9 项，逐项记录命令与输出）：
- [ ] 1～2 建 fabric、下单、扣款与到期对齐。
- [ ] 3 集群信息与互通（`ping`、任意端口 HTTP）。
- [ ] 4 隔离四项（宿主 22、网关、raw socket、第三方实例）+ 宿主机 `iptables -S SPARK-CLU`、`ip route | grep 10.210`。
- [ ] 5 `nccl_allreduce.py` 两节点（`NCCL_DEBUG=INFO`），记录 busbw。
- [ ] 6 `vllm-cluster`（`Qwen/Qwen3-8B` TP 2；再 PP 2）——模型在两台 DGX 上从 ModelScope 下载；`ray status`；吞吐。
- [ ] 7 失联恢复与 `spc0` 自愈。
- [ ] 8 续费 / 成员 409 / 释放与宿主机清理。
- [ ] 9 Token 商城 2 台组全套（含杀 raylet、停 Agent 10 分钟、`llm_pool_short`、恢复后新组）、两个不变量函数（`go test ./internal/llm -run TestInvariantsAgainstDevDB` 与对应的 rental 只读检查，连开发库）。
- [ ] 问题修复与回归同 Task 17；更新验收记录并提交。

---
### Task 19: 真机检查点 CP-2——两台 RDMA 直连（`cx7 + direct + rdma`）

**前置**：负责人准备清单第 3（QSFP 直连线）、4 项完成；两台 `rdma link` 显示直连口 `ACTIVE`。

**步骤**（规格 §16.4 CP-2 全部 6 项）：
- [ ] 1 网卡表 RDMA 判定、建 fabric。
- [ ] 2 容器内 `/dev/infiniband`、`ibv_devinfo`、GID 核对（镜像缺 rdma-core 用户态 → 在 `deploy/image` 的 LLM Serving Dockerfile 加 `rdma-core ibverbs-utils`，**在 DGX 上**重建镜像、两台各自 load，记录到验收文档与 followups）。
- [ ] 3 NCCL `NET/IB … RoCE` 与 busbw 对比 CP-1。
- [ ] 4 隔离（父接口无地址、访问不到宿主、raw socket 失败）。
- [ ] 5 `vllm-cluster` 更大模型与 Token 商城 2 台组走 RDMA。
- [ ] 6（可选）拔线测试。
- [ ] 问题修复、回归、记录、提交。

---
### Task 20: 真机检查点 CP-3——四台 + 收尾

**前置**：负责人准备清单第 1（4 台）、3（交换机 trunk，无交换机则按 `lan` 做）、6、7 项完成。

**步骤**（规格 §16.4 CP-3 全部 4 项）：
- [ ] 1 VLAN fabric：两个 2 台集群互相隔离、再 4 台集群互通（无交换机时：`lan` fabric 4 台互通 + 两个 2 台集群互不可达）。
- [ ] 2 4 台才能装下的模型（`Qwen/Qwen3-235B-A22B-Instruct-2507-FP8`，TP 4 或 TP 2 × PP 2；DeepSeek 4-bit 为加分项）经 `vllm-cluster` 部署、chat 正确，记录下载 / 加载 / TTFT / 吞吐。
- [ ] 3 Token 商城 `nodes_per_replica 4` 同模型、网关调用、停成员 → unhealthy → 10 分钟 failed。
- [ ] 4 收尾：告警核对、两个不变量函数、`make check` 全绿；写 `docs/superpowers/followups-plan13.md`（规格 §17 遗留 + 验收中发现的未修项）；更新 `README.md` 的集群部署说明（如验收中有修订）。
- [ ] 提交：`git add docs README.md && git commit -m "docs(acceptance): plan 13 CP-3 four-node acceptance, followups …"`
