# Spark Cloud — 计划 13：多机模型（2 台 / 4 台集群）设计文档

- 日期：2026-10-26
- 状态：待评审（负责人已授权由设计方裁定、不再逐项询问；代负责人作出的产品裁定集中列在 §1.3，可事后推翻）
- 上位文档：`2026-10-14-spark-cloud-mvp-design.md`（下称「总设计」）；`2026-10-18-plan2-agent-design.md`（下称「计划 2」）§3 协议、§4.2 容器规格、§4.4 网络隔离；`2026-10-19-plan6-booking-design.md`（下称「计划 6」）§4.2 候选机器、§6 一次多台；`2026-10-20-plan7-deploy-design.md`（下称「计划 7」）§5 部署机制；`2026-10-24-plan9-token-mall-design.md`（下称「计划 9」）§3 节点应用、§5 节点生命周期；`2026-10-24-plan10-shared-machine-design.md`（下称「计划 10」，`share_slots` 与 `Hello.features`）
- 范围：两部分都做。**A（优先）Token 商城多机模型**：模型声明 `nodes_per_replica ∈ {1, 2, 4}`，一个副本 = N 台池内机器组成的节点组（同一互联网络、原子分配），Ray head + workers、vLLM `--distributed-executor-backend ray`、`TP × PP = N`；按组健康检查与故障转移；网关只路由到 head；计费不变。**B 租机用户的集群**：下单可选「集群 2 台 / 4 台」，平台保证同一互联网络并让成员互通（不破坏租户隔离），每个实例注入集群信息（`/etc/spark-cluster.json` 与环境变量），一键部署多机 vLLM（集群应用）。两部分共用同一套「集群网络（fabric）+ 实例集群 + 集群部署」基础设施。
- 不在范围：见 §17。

## 0. 现状与约束（实现时不必再查）

**代码事实（`main` 234187d，计划 1～12 已合并，计划 10 共享机搁置；分支 `feat/plan13-multinode`）**
- 容器（`internal/agent/runtime/docker/docker.go`）：`docker create --gpus all --network spark0 -p <lan_ip>:<ssh_port>:22 --cap-add IPC_LOCK --cap-add SYS_NICE --cap-add SYS_PTRACE --ulimit memlock=-1:-1 --entrypoint /.spark/init.sh`，**root 但非 privileged**（Docker 默认能力集，含 `NET_RAW`、不含 `NET_ADMIN`——容器里改不了 IP / 路由 / 网卡）。网桥 `spark0`（`10.200.0.0/24`，`enable_icc=false`，NAT 出网）。`/.spark` 是宿主机 `/var/lib/spark-agent/ssh/<id>/` 的**只读目录挂载**，里面有 `authorized_keys`、`hostkeys/`、`init.sh`（`executor/sshdir.go` 的 `initScript`，每次 create / start / 恢复都重写）。
- 隔离（`internal/agent/netfilter/iptables/iptables.go`）：`DOCKER-USER -i spark0 -j SPARK-FWD`：已建立放行 → 白名单放行 → `PrivateCIDRs`（10/8、172.16/12、192.168/16、100.64/10、169.254/16、224/4）DROP → 其余放行；`INPUT -i spark0 -j SPARK-IN`：已建立放行、其余 DROP。限速 `tc` 只挂在 eth0 的宿主机 veth 上（`VethOf` 按 `eth0@ifN` 找）。规则每 60 秒巡检（`executor.refreshRules` → `EnsureRules` + `ensureLimits`）。
- 执行器（`internal/agent/executor`）：实例指令单队列 + `opMu` 串行；`create → startChecked`（挂载校验 → docker start → `VethOf` → `Limit`，限速失败停容器 fail closed）；`Recover` 宿主机重启后按 `state.json` 恢复；`wipe` 删容器 / 数据盘 / ssh 目录后校验。状态文件 `state.Instance{ID, Desired, ImageRef, LanIP, SSHPort, BandwidthMbps, DataSizeGiB, ShareSlots}`。
- 协议（`api/proto/agent/v1/agent.proto`，`make proto` 用 `~/.local/bin` 的 protoc 36.2 重新生成，产物入库 `internal/agentpb`）：`Hello{agent_version, hostname, ips, features}`（`features` bit0 = `SharedSlots`，平台目前不读）；`StateReport{instances, images}` 每 60 秒；`Command{create/start/stop/wipe/pull}`；`CreateInstance{image_ref, upstream_pubkey, bandwidth_mbps, ssh_port, data_size_gib, host_key_pem, share_slots}`。平台侧 `fleet.CreateInstancePayload{ImageRef, SSHPort, ShareSlots}` 存 `agent_commands.payload`，`agentgw.toProtoCommand` 转协议；`fleet.RecordStateReport` 把报告原文写 `machines.last_state_report`（JSON）。旧 Agent 忽略未知字段——**新能力必须靠 `features` 位门控**，不能指望旧 Agent 报错。
- 下单（计划 6，`internal/rental/batch.go`）：`PlaceBatch(BatchRequest{…, Count ≤ 8, StartAt?, GroupID?, MachineID?, AppID?})` 一个事务挑 N 台（`placeOnMachines` + `pickLocked`，`FOR NO KEY UPDATE SKIP LOCKED`，排他约束冲突换机）、N 张订单（`charge` 每台一单，`Wallet.Apply`）、N 条 Create 指令，全成或全败；`rental_batches` 一行。**没有「同一网络」的概念**：N 台可以落在任意机器上。用户侧没有停机 / 开机接口（只有续费、释放、自动续费、取消预订）。
- 实例生命周期：`HandleCommandResult`（Create 成功 → `startLease`，失败 → `failCreating`：实例 failed、`refundAll`、机器 faulty、告警 `create_failed`）；`AdminMigrate`（锁序 原实例 → 新机器 → 指令，复制端口与部署、`beginWipe(old)`）；`RefundRelease`；`ExpireDue / AutoRenewDue / SendReminders / FailStuckCreating / FailTimedOutCommands` 在 `rental.Sweep`；加锁顺序写在 `internal/rental/service.go` 头注释。
- Token 商城（计划 9，`internal/llm`）：节点 = 一台池内机器上的系统实例（`rental.PlaceSystem`）+ 内部应用 `vllm-node 1.0.0` 部署；`llm_nodes(machine_id, instance_id, deployment_id, state …)`，`llm_nodes_machine_live_uniq`；Controller（10 秒，咨询锁 `llm-controller`）：对账 `replicas` → `createNode` → `advance`（实例 running → `appdeploy.Create` → 部署 running + health ok → 探测 → ready）→ `handleUnhealthy`（10 分钟 → `Redeploy`，3 次 → failed）→ `handleDraining / handleRemoving` → `releaseOrphans`；`Prober` 经 `upstream.Pool.Dial(target, 8000)` 打 `GET /v1/models`；`Router.Refresh` 每 2 秒从 `llm_nodes ⋈ instances(running, 未暂停)` 建路由表，网关只按 `node.instance_id` 拨号。计划 9 §1.2 / §17 明确「跨机张量并行不在 v1」——本计划解除。
- 一键部署（计划 7，`internal/appdeploy`）：一条 `app_deployments` = 一个实例上的一个应用；Runner 经 SSH 在容器里跑内置 `run.sh`（`deploy → install → start → 健康检查`），`Spec{Ports(1～2), TimeoutMinutes ≤ 180, MinDataFreeGiB, Env(≤ 16), Links}`；健康检查经上游拨号到容器内 `127.0.0.1:<Ports[0]>`；保留变量 `ReservedEnv`（`env` 文件的 cksum 是 `install_key` 的一部分——**给存量部署多加一行保留变量会让它们全部重跑 install**，计划 11 的 `SPARK_GIT_MIRROR` 因此只对新版本注入）。
- 迁移：下一个可用编号 **00026**（00023 保留未用；goose 已允许乱序）。
- 覆盖表（`web/e2e/coverage.json`）当前最大编号：`C-MKT-13`、`C-INS-07`、`C-DET-26`、`C-APP-11`；`A-MCH-10`、`A-INS-16`、`A-LLM-09`、`A-GRP-05`。

**已核实的真机事实（2026-10-26，测试机 192.168.1.221）**
- 镜像 `spark/llm-serving:25.09`：vLLM 0.10.1.1+nv25.09、torch 2.9.0a0、NCCL 2.27.7、Ray 2.49.2。**vLLM 0.10.1 多机只能走 Ray**（`--nnodes / --node-rank` 的 mp 多机是更新版本才有）：head `ray start --head --port=6379`，worker `ray start --address=<head>:6379 --block`，然后在 head 上 `vllm serve --distributed-executor-backend ray`。NCCL / Gloo 要 `NCCL_SOCKET_IFNAME / GLOO_SOCKET_IFNAME` 指向集群网卡；RDMA 时还要 `/dev/infiniband` 与 `NCCL_IB_HCA`。
- **221 没有 ConnectX-7**（PCI 无 15b3 设备）：只有 Realtek 有线 `enP7s7`（未插线）与 WiFi `wlP9s9`（当前上网用的就是 WiFi）；`/dev/infiniband` 只有 `rdma_cm`、`ibv_devices` 为空。标准 DGX Spark 有 CX-7（2 × QSFP），两台可直连，4 台需要交换机。
- 推论（设计约束）：集群网卡与 RDMA 能力必须是**每台机器的探测结果 + 后台配置**；没有 CX-7 时退回普通以太网（NCCL 走 TCP socket），性能告警但能跑；**WiFi 客户端模式不能挂 macvlan（一个无线关联只认一个 MAC）**，所以「普通以太网」模式不能用二层桥接方案，只能走宿主机三层路由（§3.3）。

## 1. 架构与核心决策

```
                       后台「集群网络」：fabric（lan / cx7-direct / cx7-vlan）+ 机器归属 + 集群网卡
                                         │                        ▲ Agent 每 60 秒上报网卡探测（含 RDMA）
                                         ▼                        │
┌──────────────── rental：实例集群 instance_clusters（同 fabric、原子分配、/24 子网、rank）────────────────┐
│  用户：PlaceBatch{Cluster: true, Count 2|4} → 同 fabric 挑 N 台 → N 单 N 实例 + 1 个集群               │
│  平台：PlaceSystemCluster(N 台池内机器) → N 个系统实例 + 1 个系统集群                                   │
│  Create 指令带 ClusterSpec（成员、rank、IP、模式）；成员变更 → ConfigureCluster 推给全部成员            │
│  SweepClusters：开通期任一成员失败 → 整组中止全额退款；全员 running → 到期对齐；全员结束 → 集群结束      │
└────────────────────────────────────────────────────────────────────────────────────────────────────┘
        │ ClusterSpec                                                  │ instances.cluster_id / rank
        ▼                                                              ▼
┌──── spark-agent：集群数据面（spc0 第二网卡）──────┐      ┌──── appdeploy：集群部署 app_cluster_deployments ──┐
│ routed（lan）：veth + 宿主机 /32 路由 + SPARK-CLU │      │ 一次在 N 个成员上各建一条部署：rank0=head，其余=worker │
│ macvlan（cx7）：CX-7 口 / VLAN 子接口上的 macvlan │      │ 保留变量 SPARK_CLUSTER_ROLE；整组重新部署 / 停 / 卸载   │
│ RDMA：--device /dev/infiniband/* + GID 探测       │      │ vllm-node 1.1.0（平台内部）/ vllm-cluster（用户）       │
│ 写 /.spark/cluster.{json,env} → /etc/spark-cluster.json │  └──────────────────────────────────────────────────┘
└──────────────────────────────────────────────────┘                    ▲
                                                                         │ CreateCluster / RedeployCluster
┌──── llm（Token 商城）──────────────────────────────────────────────────┴───────────────────────────────┐
│ 模型 nodes_per_replica N、TP×PP=N；节点 = 节点组（llm_node_members），node.instance_id = rank0（head）       │
│ provisioning：PlaceSystemCluster → 全员 running → CreateCluster(vllm-node) → 聚合 running → 探测 head → ready │
│ 任一成员实例没了 / 机器 faulty / 失联 10 分钟 → 整组 failed 释放、另起一组；进程崩溃 → 2 分钟后整组重新部署   │
│ Router 只收「全员 running 未暂停」的组，网关只拨 head（不变）；计费不变                                       │
└────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

### 1.1 决策一览

| # | 决策 | 结论 | 理由 |
|---|---|---|---|
| 1 | 「同一互联」的表达 | 新实体 **fabric（集群网络）**：后台登记，三种 `lan`（普通以太网，三层路由）、`cx7 + direct`（两台 QSFP 直连）、`cx7 + vlan`（经交换机，按集群分 VLAN）；机器归属一个 fabric 并选一块集群网卡；集群成员必须同 fabric | 物理拓扑（哪几台插在同一根线 / 同一台交换机）平台探测不出来，必须人工登记；Agent 只负责报「我有哪些网卡、哪些有 RDMA」 |
| 2 | 集群数据面 | **容器加第二块网卡 `spc0`，由 Agent 在 `docker start` 之后自己建并移进容器 netns**（不用 Docker 的 macvlan / bridge 网络对象）；`lan` 用 veth + 宿主机 /32 路由 + iptables 链 `SPARK-CLU`；`cx7` 用 macvlan（直连口或 VLAN 子接口）| Docker 28+ 对 bridge 网络的直连访问有额外过滤、macvlan 网络对象一机一父接口一子网，行为随版本变；自己建网卡只用 `ip`/`nsenter`（Agent 已在用），每次 start / 恢复 / 60 秒巡检都能幂等重建 |
| 3 | 普通以太网怎么互通 | **三层路由（routed）**：每个成员容器得到集群 IP `10.210.C.(11+rank)/32`，网关为本机 veth 上的 `10.210.C.1`；宿主机为对端成员 IP 加 `/32 via <对端宿主机 IP>`；`SPARK-CLU` 只放行「本机成员 ↔ 对端成员」的 IP 对 | NAT 后面的 `10.200.0.x` 互相不可达且各机重复；二层方案在 WiFi 上不成立；路由方案的每个包都经宿主机 FORWARD，隔离由宿主机 iptables 执行，容器 root 改不了 |
| 4 | CX-7 / RDMA | **macvlan（bridge 模式）挂在 CX-7 口（direct）或 `scv<vlan>` VLAN 子接口（vlan）上**，RDMA 设备共享模式（kernel 默认 shared netns），容器 `--device /dev/infiniband/rdma_cm` + `uverbsN`；RoCE GID 随 macvlan 的 IP 自动生成，Agent 查出 GID 索引写进 `NCCL_IB_GID_INDEX` | RDMA 流量由网卡硬件收发、不经宿主机 netfilter，路由方案对 RDMA 无效，只能给容器一个挂在 CX-7 上的 netdev；SR-IOV / exclusive 模式需要 VF，GB10 上未验证（真机项）|
| 5 | 隔离 | 集群容器 `--cap-drop NET_RAW`（不能发伪造包、不能建 Raw Packet QP）、本来就没有 `NET_ADMIN`（不能改 IP / 路由 / 建 VLAN）；routed 模式 `INPUT -i sch+ -j SPARK-IN`（容器碰不到宿主机）+ `SPARK-CLU` 白名单；macvlan 模式父接口无 IP、关 IPv6，`vlan` fabric 的交换机端口只放行集群 VLAN（无 native VLAN），`direct` 物理隔离 | 见 §3.5 逐项分析；残余风险列为真机验收项 |
| 6 | 集群信息注入 | Agent 写 `/.spark/cluster.json` 与 `/.spark/cluster.env`（只读挂载内，实时可见），`init.sh` 每次启动建 `/etc/spark-cluster.json` 软链与 `/etc/profile.d/spark-cluster.sh`；登录 shell 自动有 `SPARK_CLUSTER_*`、`NCCL_SOCKET_IFNAME`、`MASTER_ADDR` 等 | 不需要平台经 SSH 往容器写文件；成员变更（换机）时 Agent 原子替换文件即生效 |
| 7 | 协议 | `Hello.net` / `StateReport.net`（网卡探测）、`InstanceState.cluster`（集群网络状态）、`CreateInstance.cluster`（ClusterSpec）、新指令 `ConfigureCluster`；`features` bit1 = `Cluster`，平台只把集群分到 bit1 的机器 | 向后兼容；旧 Agent 会静默忽略 `cluster` 字段，必须门控 |
| 8 | 实例集群实体 | `instance_clusters` 一行 + `instances.cluster_id / cluster_rank`；用户集群与 Token 商城节点组共用；集群状态（组建中 / 就绪 / 降级）由成员派生，表里只有 `active / released / failed` | 不复制一套「多机实例」；成员仍是普通实例，计费 / 接入 / 部署 / 对账全部复用 |
| 9 | 用户集群计费 | **N 台 = N 份整机价**，走现有 `PlaceBatch`（N 张订单、一个批次、全成或全败）；续费 / 自动续费 / 释放 / 后台延长**按集群整体**；到期时刻在全员 running 后对齐 | 与现有预付规则一致；集群成员到期时刻不一致会让集群「半截过期」 |
| 10 | 集群应用 | **在集群上一键部署 = 平台同时在 N 个成员上建 N 条部署**（rank0 = head、其余 = worker，同一 app 版本 / 变量，注入 `SPARK_CLUSTER_ROLE`），新表 `app_cluster_deployments` 聚合；整组重新部署 / 停 / 卸载 | 「各成员自己部署 worker」需要用户点 N 次且参数必须一致，容易错；Runner 仍按单条部署工作，零改动 |
| 11 | Token 商城节点组 | `llm_models.nodes_per_replica / tensor_parallel / pipeline_parallel`；N > 1 的副本 = 一个 `llm_nodes` 行（`instance_id` = head）+ `llm_node_members` N 行；用 `PlaceSystemCluster` + `CreateCluster(vllm-node 1.1.0)`；网关 / 路由 / 计费零改动（只认 head） | 计划 9 的状态机、探测、排空、后台页面全部保留，只在「实例」「部署」两处换成组 |
| 12 | 多机 vLLM 启动 | worker：`ray start --address=<head>:6379 --block`（失败每 10 秒重试，head 重建后自动重连）+ 回环健康检查小服务；head：`ray start --head` → 等 N 个节点（各 1 GPU）→ `vllm serve --distributed-executor-backend ray --tensor-parallel-size TP --pipeline-parallel-size PP`；`ray start --object-store-memory 2GB`；`VLLM_HOST_IP / NCCL_* / GLOO_*` 在 `ray start` 之前导出 | vLLM 0.10.1 的唯一多机路径；GB10 统一内存，Ray 默认按 30% 内存预留对象存储会挤占显存 |

### 1.2 不做的替代方案（评审时不必再提）
- 「集群容器用 `--network host`」：租户拿到宿主机网络命名空间 = 能连 Agent、Docker API、宿主机 sshd 与整个内网，隔离归零；Token 商城系统实例也不用（平台自己的代码同样会跑 `pip install` 来的第三方包、Ray 端口不鉴权），统一走 §3 的第二网卡。
- 「Docker `macvlan` / `ipvlan` 网络对象」：一个父接口只能有一个该类网络、`--ip` 依赖 Docker IPAM、`docker network connect` 在容器停止后的行为随版本变；改为 Agent 自建（§1.1 第 2 条）。
- 「普通以太网也用 macvlan」：WiFi 不支持；且 macvlan 包绕过宿主机 netfilter，在办公 / 机房内网上没有任何隔离（容器能直接打到别的宿主机）。
- 「Agent 起 VXLAN / WireGuard 覆盖网络」：多一层封装、要管密钥与 MTU，RDMA 无法走；routed 已经够用。
- 「vLLM `--nnodes/--node-rank` 多进程多机」：25.09 镜像的 vLLM 0.10.1 没有。
- 「每个成员各自在控制台部署 worker」：见 §1.1 第 10 条；同时保留「对单个成员部署普通应用」的能力。
- 「集群支持预订」：预订开通时的逐台改派（计划 6 §5.4）要变成「同 fabric 整组改派」，失败面翻倍；v1 只支持即时开通（§1.3 M2）。
- 「head 下载模型后经集群网分发给 worker」：省 ModelScope 带宽，但要做文件同步与校验；v1 每个成员各下一份（§1.3 M18）。

### 1.3 负责人裁定（默认，可推翻）

| # | 裁定 | 理由 |
|---|---|---|
| M1 | 集群规模产品上只开放 **2 台与 4 台**；代码与表约束按通用 N（`size BETWEEN 2 AND 8`），服务层校验 `{2, 4}` | 负责人要求 2 / 4；GB10 每机 1 GPU，TP 2 / 4 与常见模型的注意力头数整除 |
| M2 | 用户集群**只支持即时开通**，不支持预订；集群不能指定机器；可以选机组（机组内同 fabric） | 见 §1.2；指定机器与「平台保证同 fabric」冲突 |
| M3 | 价格 = 成员数 × 单机价，不加「互联费」；不同 fabric（RDMA / 以太网）同价，市场页标注网络类型 | 现有价格体系不动；高速网络溢价是运营决策（遗留） |
| M4 | 平台挑 fabric：同商品、可见机组 / 未分组（与计划 6 B9 一致）、Agent 支持集群、网卡就绪；排序 **RDMA 优先 → 剩余空闲最少（最贴合）→ fabric ID**；`direct` fabric 只能组 2 台 | 尽量给用户最好的网络，同时减少碎片 |
| M5 | 集群成员**不能单独续费 / 改自动续费 / 释放**（409 `cluster_member`，引导到集群页）；可以单独改密码、端口、登录方式、部署普通应用 | 成员到期不一致会让集群半截失效；访问类设置与集群无关 |
| M6 | **开通期任一成员失败 → 整个集群开通失败**：失败成员按现有 `failCreating`（机器 faulty、全额退），其余成员洗机并**全额退款**（机器不判故障），告警 `cluster_failed` 一次 | 用户买的是 N 台的整体；3/4 的集群对多机模型没用 |
| M7 | 运行中成员故障（机器失联 / faulty / 后台退款释放）→ 集群「降级」，**不自动换机**（与单机一致）；运维可「换机」（新机器必须同 fabric，保留 rank 与集群 IP，其余成员收到新的 ConfigureCluster）或退款释放该成员 | 复用现有处置手段；自动换机会在用户不知情时重启分布式任务 |
| M8 | 到期对齐：集群全员第一次 running（且未暂停）时，把所有成员 `expires_at` 拉到其中最晚的一个（平台让出开通先后差的几分钟）；**之后不再对齐**（失联暂停产生的差距不补，补就是白送其余成员的时长），续费 / 延长对全员加相同时长；自动续费允许有成员暂停；组建期 30 分钟未就绪整组中止全额退 | 让集群整体同时到期、同时停机、同时提醒，又不送时长 |
| M9 | 集群网段池 `10.210.0.0/16`（`SPARK_CLUSTER_SUBNET_POOL` 可改），每个活跃集群一个 `/24`（最多 256 个并存），成员 IP `.11 + rank`，routed 网关 `.1`；容器内集群网卡名固定 `spc0` | 与 `spark0` 的 `10.200.0.0/24` 不重叠；200 台机器最多 100 个集群 |
| M10 | `lan` fabric 允许用户租用，市场页 / 集群页标「普通网络（性能有限，适合测试与流水线并行）」；Token 商城对 `lan` fabric 同样可用（运维自己决定池里放什么） | 负责人要求「没有 CX-7 时退回普通以太网，性能告警但能跑」 |
| M11 | Token 商城：`nodes_per_replica ∈ {1, 2, 4}`、`tensor_parallel × pipeline_parallel = N`，默认 `TP = N, PP = 1`；`embedding` 模型只能 N = 1；N 只能在模型没有非终态节点时修改，TP / PP 随时可改（下次重建生效） | GB10 每机 1 GPU；改 N 会让存量节点形态与模型不符 |
| M12 | 节点组故障：任一成员实例没了 / 机器 faulty → **整组立即 failed**、释放全部成员，下一轮在别处另建一组；成员机器失联（实例暂停）→ 整组 unhealthy，**持续 10 分钟仍未恢复 → failed**；成员进程崩溃（部署 failed / 不健康）→ 整组 unhealthy，**2 分钟后整组重新部署**（`rebuild_count + 1`，3 次 → failed + 严重告警） | Ray 集群丢一个节点 vLLM 就不能自愈；组不能「换一个成员」继续跑 |
| M13 | Token 商城 N > 1 只在**池内同一 fabric** 的空闲机器上建组；不同 fabric 的零散空闲机器不拼组；N = 1 的节点优先用不属于任何 fabric 的池机器；缺组沿用告警 `llm_pool_short` | 避免单机节点把能组队的机器拆散 |
| M14 | 集群应用：在集群页一键部署（N 条部署同时建），整组重新部署 / 停止 / 启动 / 卸载；一个集群同时只能有一个集群应用；成员换机后集群应用显示「需要重新部署」，不自动重启分布式任务；首个用户集群应用 `vllm-cluster`（公开） | 见 §1.1 第 10 条 |
| M15 | RDMA 采用共享模式 + macvlan，不做 SR-IOV；fabric 标 `rdma=true` 但成员探测 RDMA 不可用、GID 找不到、或重启后 `uverbsN` 编号变化 → 退回 TCP（`NCCL_IB_DISABLE=1`），集群照常运行并告警，页面显示「RDMA 不可用」 | 「能跑优先」；真机验收确认 RDMA 路径 |
| M16 | 集群容器去掉 `NET_RAW`（显式 `--sysctl net.ipv4.ping_group_range=0 2147483647`，镜像里有 ping 时可用 ICMP 数据报套接字，但不保证——连通性验收一律用 curl / python）；macvlan 父接口由 Agent 持续清掉地址并关 IPv6、设为 NM unmanaged | §3.5 |
| M17 | Ray / vLLM 端口不鉴权，只在集群网内可达（只有同一租户的成员）；vLLM API 仍只监听 head 的 `127.0.0.1:8000`，经现有 SSH 上游访问 | 与计划 7 / 9 相同的暴露面 |
| M18 | 模型文件每个成员各下载一份到自己的数据盘（同一路径）；不做共享存储 / 分发 | 简单可靠；代价是 N 倍下载量（真机验收预留时间） |
| M19 | 集群成员之间**不提供 SSH 互信**（容器 sshd 只认平台上游密钥）；帮助页推荐 `torchrun` / Ray，需要 MPI 的用户自建 sshd | 不扩大密钥面 |
| M20 | 部署超时上限 `MaxTimeoutMinutes` 由 180 提到 **480**（只影响新版本 spec）；`vllm-node 1.1.0`、`vllm-cluster 1.0.0` 取 360 分钟；Token 商城节点 `ProvisionTimeout` 改为「该版本 `timeout_minutes` + 30 分钟」 | 4 台各下 200GB+ 模型 + 加载，180 分钟不够 |

## 2. 数据模型（迁移 `00026_multinode.sql`，带 `+goose Down`）

```sql
-- 集群网络（fabric）
CREATE TABLE cluster_fabrics (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,                       -- 1～32，后台展示
    kind        TEXT NOT NULL CHECK (kind IN ('lan', 'cx7')),
    isolation   TEXT NOT NULL CHECK (isolation IN ('routed', 'direct', 'vlan')),
    rdma        BOOLEAN NOT NULL DEFAULT false,             -- 运维意图：尝试 RDMA（还要成员探测通过）
    mtu         INT NOT NULL DEFAULT 0 CHECK (mtu = 0 OR mtu BETWEEN 1280 AND 9216),   -- 0 = 继承网卡
    vlan_min    INT,
    vlan_max    INT,
    description TEXT NOT NULL DEFAULT '',                   -- ≤ 200
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    CONSTRAINT cluster_fabrics_kind_isolation CHECK ((kind = 'lan') = (isolation = 'routed')),
    CONSTRAINT cluster_fabrics_rdma CHECK (kind = 'cx7' OR NOT rdma),
    CONSTRAINT cluster_fabrics_vlan CHECK (
        (isolation = 'vlan' AND vlan_min BETWEEN 2 AND 4094 AND vlan_max BETWEEN vlan_min AND 4094)
        OR (isolation <> 'vlan' AND vlan_min IS NULL AND vlan_max IS NULL))
);

-- 机器：fabric 归属、集群网卡、Agent 能力与网卡探测
ALTER TABLE machines
    ADD COLUMN fabric_id      BIGINT REFERENCES cluster_fabrics (id),
    ADD COLUMN cluster_iface  TEXT NOT NULL DEFAULT '',     -- 后台指定的集群网卡；'' = 自动（lan → lan_ip 所在网卡；cx7 → 第一个 up 的 mlx5 口）
    ADD COLUMN agent_features INT  NOT NULL DEFAULT 0,      -- Hello.features 原值（bit0 SharedSlots、bit1 Cluster）
    ADD COLUMN net_info       JSONB,                        -- 最近一次网卡探测（clusterspec.NetInfo）
    ADD COLUMN net_info_at    TIMESTAMPTZ,
    ADD COLUMN cluster_ready  BOOLEAN NOT NULL DEFAULT false,   -- fleet 按 fabric + 网卡 + 能力位计算（§3.2），分配只看它
    ADD COLUMN cluster_rdma   BOOLEAN NOT NULL DEFAULT false,   -- 本机在该 fabric 上 RDMA 可用
    ADD COLUMN cluster_net    TEXT NOT NULL DEFAULT '';         -- 集群网段：lan = 集群网卡 IPv4 前缀（如 192.168.1.0/24），cx7 = 'l2'，未就绪 = ''；分配按 (fabric_id, cluster_net) 分组
CREATE INDEX machines_fabric_idx ON machines (fabric_id, id) WHERE fabric_id IS NOT NULL;

-- 实例集群（用户集群与 Token 商城节点组共用）
CREATE TABLE instance_clusters (
    id           TEXT PRIMARY KEY,                          -- 'clu-' + 10 位 [a-z0-9]
    user_id      BIGINT NOT NULL REFERENCES users (id),
    system       BOOLEAN NOT NULL DEFAULT false,            -- 平台节点组（成员是系统实例）
    size         INT NOT NULL CHECK (size BETWEEN 2 AND 8),
    fabric_id    BIGINT NOT NULL REFERENCES cluster_fabrics (id),
    mode         TEXT NOT NULL CHECK (mode IN ('routed', 'macvlan')),
    vlan         INT CHECK (vlan BETWEEN 2 AND 4094),
    rdma         BOOLEAN NOT NULL,                          -- 开通时判定的有效 RDMA（fabric.rdma ∧ 全员 cluster_rdma）
    mtu          INT NOT NULL DEFAULT 0,
    subnet_index INT NOT NULL CHECK (subnet_index BETWEEN 0 AND 255),
    version      INT NOT NULL DEFAULT 1,                    -- ClusterSpec 版本，成员变更 +1
    state        TEXT NOT NULL CHECK (state IN ('active', 'released', 'failed')),
    batch_id     TEXT REFERENCES rental_batches (id),
    ready_at     TIMESTAMPTZ,                               -- 全员首次 running（到期对齐、开通期判定）
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    ended_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX instance_clusters_subnet_live_uniq ON instance_clusters (subnet_index) WHERE state = 'active';
CREATE UNIQUE INDEX instance_clusters_vlan_live_uniq ON instance_clusters (fabric_id, vlan) WHERE state = 'active' AND vlan IS NOT NULL;
CREATE INDEX instance_clusters_user_idx ON instance_clusters (user_id, created_at DESC);

ALTER TABLE instances
    ADD COLUMN cluster_id   TEXT REFERENCES instance_clusters (id),
    ADD COLUMN cluster_rank INT CHECK (cluster_rank BETWEEN 0 AND 7),
    ADD CONSTRAINT instances_cluster_pair CHECK ((cluster_id IS NULL) = (cluster_rank IS NULL));
CREATE INDEX instances_cluster_idx ON instances (cluster_id, cluster_rank) WHERE cluster_id IS NOT NULL;
-- 不建 (cluster_id, rank) 的唯一索引：换机时新旧成员短暂同时活动；rank 唯一由集群行锁保证、CheckClusterInvariants 核对

-- 集群部署（一次在全部成员上部署同一应用）
CREATE TABLE app_cluster_deployments (
    id             TEXT PRIMARY KEY,                        -- 'acd-' + 12 位
    cluster_id     TEXT NOT NULL REFERENCES instance_clusters (id),
    user_id        BIGINT NOT NULL REFERENCES users (id),
    app_id         BIGINT NOT NULL REFERENCES apps (id),
    app_version_id BIGINT NOT NULL REFERENCES app_versions (id),
    state          TEXT NOT NULL CHECK (state IN ('active', 'removed')),   -- 运行态由成员部署聚合（§6.4）
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX app_cluster_deployments_live_uniq ON app_cluster_deployments (cluster_id) WHERE state = 'active';
ALTER TABLE app_deployments
    ADD COLUMN cluster_deployment_id TEXT REFERENCES app_cluster_deployments (id),
    ADD COLUMN cluster_role          TEXT CHECK (cluster_role IN ('head', 'worker')),
    ADD CONSTRAINT app_deployments_cluster_pair CHECK ((cluster_deployment_id IS NULL) = (cluster_role IS NULL));
CREATE INDEX app_deployments_cluster_idx ON app_deployments (cluster_deployment_id) WHERE cluster_deployment_id IS NOT NULL;

-- Token 商城
ALTER TABLE llm_models
    ADD COLUMN nodes_per_replica INT NOT NULL DEFAULT 1 CHECK (nodes_per_replica IN (1, 2, 4)),
    ADD COLUMN tensor_parallel   INT NOT NULL DEFAULT 1 CHECK (tensor_parallel BETWEEN 1 AND 8),
    ADD COLUMN pipeline_parallel INT NOT NULL DEFAULT 1 CHECK (pipeline_parallel BETWEEN 1 AND 8),
    ADD CONSTRAINT llm_models_parallel CHECK (tensor_parallel * pipeline_parallel = nodes_per_replica);
ALTER TABLE llm_nodes
    ADD COLUMN size           INT NOT NULL DEFAULT 1 CHECK (size BETWEEN 1 AND 8),
    ADD COLUMN cluster_id     TEXT REFERENCES instance_clusters (id),
    ADD COLUMN app_cluster_id TEXT REFERENCES app_cluster_deployments (id),
    ADD CONSTRAINT llm_nodes_group CHECK ((size = 1) = (cluster_id IS NULL));
CREATE TABLE llm_node_members (
    node_id     TEXT NOT NULL REFERENCES llm_nodes (id),
    rank        INT  NOT NULL CHECK (rank BETWEEN 0 AND 7),
    machine_id  BIGINT NOT NULL REFERENCES machines (id),
    instance_id TEXT NOT NULL REFERENCES instances (id),
    live        BOOLEAN NOT NULL DEFAULT true,              -- 节点进入终态时同事务置 false
    PRIMARY KEY (node_id, rank)
);
CREATE UNIQUE INDEX llm_node_members_machine_live_uniq ON llm_node_members (machine_id) WHERE live;
```

说明：
- `machines.cluster_ready / cluster_rdma / cluster_net` 是**派生缓存**，由 `fleet` 在三处重算：`RecordNetInfo`（探测更新）、`RecordHello`（能力位）、`UpdateMachine`（改 fabric / 集群网卡）、`UpdateFabric`（改 kind / rdma，重算该 fabric 全部机器）。分配 SQL 只读这两列，不解析 JSON。
- 节点组的 `llm_nodes.machine_id / instance_id` 指向 rank0（head），`deployment_id` 指向 head 的成员部署；组的成员全部在 `llm_node_members`（含 rank0）。`size = 1` 的节点与计划 9 完全一样（不建成员行、`cluster_id` 为空）。
- `app_cluster_deployments.state` 只有 `active / removed`；对外的「安装中 / 运行中 / 失败 / 已停止 / 需要重新部署」由成员部署聚合（§6.4）。
- **Down**：`DO $$` 依次拒绝：仍有 `instance_clusters.state='active'`（「仍有运行中的集群，请先释放」）；仍有 `size > 1` 的非终态 `llm_nodes`；仍有 `app_cluster_deployments.state='active'`；然后按外键逆序删列 / 表（`llm_node_members`、`llm_nodes` 三列、`llm_models` 三列与约束、`app_deployments` 两列、`app_cluster_deployments`、`instances` 两列、`instance_clusters`、`machines` 八列、`cluster_fabrics`）。Down 不改历史实例（已释放成员的 `cluster_id` 随列删除）。

**存量数据**：无 fabric、无集群；存量机器 `cluster_ready=false`（新 Agent 上报后才可能为真）；存量模型 `nodes_per_replica=1, TP=PP=1`；存量节点 `size=1`。上线当天行为与今天一致。

## 3. 集群网络

### 3.1 fabric
| kind | isolation | 数据面 | RDMA | 可组规模 | 典型接线 |
|---|---|---|---|---|---|
| `lan` | `routed` | veth + 宿主机路由（§3.3） | 否 | 2 / 4 | 普通机房 / 办公网（有线或 WiFi） |
| `cx7` | `direct` | macvlan 挂 CX-7 口（§3.4） | 是（探测通过时） | 只 2 | 两台 QSFP 直连 |
| `cx7` | `vlan` | macvlan 挂 VLAN 子接口 `scv<vlan>`（§3.4） | 是 | 2 / 4 | CX-7 接交换机，端口 trunk |

- 后台「集群网络」页增删改；`direct` fabric 最多 2 台机器（`fleet` 加入时校验，`fabric_full`）；有机器或有活跃集群时不能删（`fabric_in_use`）；有活跃集群时不能改 `kind / isolation / vlan 范围`（`fabric_in_use`），`rdma / mtu / description / name` 可改（只影响之后的集群）。
- 机器编辑（`PUT /machines/{id}`）新增 `fabric_id`、`cluster_iface`；机器上有活跃集群成员实例时不能改这两项（`invalid_state`）。

### 3.2 机器网卡探测与就绪判定
- Agent 每次建流（`Hello.net`）与每份 `StateReport`（60 秒，`StateReport.net`）上报 `NetInfo`：物理网卡列表（排除 `lo`、`docker*`、`br-*`、`veth*`、`spark0`、`sch*`、`scm*`、`scv*`、虚拟设备——判定：`/sys/class/net/<if>/device` 存在，或持有 IPv4 默认路由）+ `lan_iface`（**仅供参考**：持有 `AgentConfig.lan_ip` 的网卡；Hello 与首份 StateReport 在收到 AgentConfig 之前发出，此时为空串）。每块网卡：`name / mac / up（operstate=up 且 carrier=1）/ speed_mbps（读不到为 0）/ mtu / ipv4[]（**CIDR 形式**，如 `192.168.1.221/24`）/ driver（device/driver 链接名）/ pci（device 链接的 PCI 地址）/ wireless（存在 /sys/class/net/<if>/wireless 或 phy80211）/ rdma_device（/sys/class/net/<if>/device/infiniband/ 下的名字，如 rocep1s0f1）/ rdma_active（/sys/class/infiniband/<dev>/ports/1/state 为 `4: ACTIVE` 且 link_layer 为 Ethernet）/ uverbs（/sys/class/infiniband_verbs/uverbs*/ibdev == rdma_device 的那个 uverbsN）/ link_layer`。全部从 sysfs 与 `ip -4 -o addr show` 读，不调用 `ibv_devinfo`（宿主机未必装 rdma-core 用户态）。
- **lan 网卡以平台为准**：`clusterspec.ResolveIface(net, kind, configured, lanIP)` 用 `machines.lan_ip` 在 `ifaces[].ipv4` 里找管理网卡，找不到才退回 `net.lan_iface`。所以重连时 Agent 不知道 lan IP 也不会让 `cluster_ready` 翻转。
- `fleet.evalClusterNet(machine, fabric, net)` 计算 `cluster_ready / cluster_rdma / cluster_net`：
  - fabric 为空、`agent_features & Cluster == 0`、`net_info` 为空 → ready = rdma = false、`cluster_net = ''`；
  - `lan`：集群网卡 = `cluster_iface` 或管理网卡；该网卡存在、up、有 IPv4 → ready；`host_ip` 取 `machines.lan_ip`（集群网卡就是管理网卡时）或该网卡第一个 IPv4；**`cluster_net` = 该 IPv4 的网络前缀（如 `192.168.1.0/24`）**；rdma = false；
  - `cx7`：集群网卡 = `cluster_iface`，为空则自动取第一个 `driver = mlx5_core` 且 up 的网卡；必须存在、up、非 wireless、≠ 管理网卡 → ready；`cluster_net = 'l2'`（由 fabric 的物理接线保证同一二层）；rdma = `fabric.rdma ∧ rdma_active ∧ uverbs ≠ ''`。
  - **routed 模式要求成员宿主机在同一 IP 网段**（对端路由 `via H_k` 必须 on-link）：分配按 `(fabric_id, cluster_net)` 分组，一个 `lan` fabric 跨了两个网段时只在各自网段内组队；后台机器页显示 `cluster_net`，`lan` fabric 里出现多个网段时 fabric 列表标黄提示。
  - `ResolveIface` 在 `fleet`（就绪判定）与 `rental`（构造 ClusterSpec）共用，保证两边口径一致。
- 后台机器详情显示网卡表（探测时间、每块网卡的状态与 RDMA）、当前判定（就绪 / 原因 / 网段），集群网卡下拉框只列探测到的网卡。

### 3.3 routed 模式（`lan`）
集群 `C`（`subnet_index`）的成员 `r`：容器 IP `A_r = 10.210.C.(11+r)`，网关 `G = 10.210.C.1`，对端宿主机 IP `H_k`（ClusterSpec 的 `members[k].host_ip`），宿主机集群网卡 `I`。Agent 在容器 `docker start` 之后（`startChecked` 末尾，限速之后）执行 `Apply`。**`Apply` 是差量、非破坏性的**：Agent 重启后 `Recover` 会对所有 running 实例再跑一遍 `startChecked`，正在跑的 NCCL / Ray 连接不能被打断。

```
# 0. 观察：host `ip -o link show sch<x>`；容器 `nsenter -t <pid> -n ip -o -4 addr show dev spc0`
# 1. 只有容器里没有 spc0 时才建网卡（spc0 已存在：绝不 del / add，直接跳到第 2 步）
#    此时若宿主机残留 sch<x>（上次半途失败）→ ip link del sch<x>
ip link add sch<x> mtu <mtu|1500> type veth peer name scc<x>
ip link set scc<x> netns <pid>
nsenter -t <pid> -n ip link set scc<x> name spc0
# 2. 以下全部是 replace / 幂等写，每次 Apply 都执行，不打断现有连接
nsenter -t <pid> -n ip addr replace A_r/32 dev spc0
nsenter -t <pid> -n sysctl -w net.ipv6.conf.spc0.disable_ipv6=1
nsenter -t <pid> -n ip link set spc0 up
nsenter -t <pid> -n ip route replace G/32 dev spc0 scope link
nsenter -t <pid> -n ip route replace 10.210.C.0/24 via G dev spc0
ip addr replace G/32 dev sch<x>; ip link set sch<x> up
ip route replace A_r/32 dev sch<x>
sysctl -w net.ipv4.conf.sch<x>.rp_filter=1 net.ipv6.conf.sch<x>.disable_ipv6=1
# 3. 对端路由差量：期望集合 = {A_k/32 via H_k dev I | k ≠ r}；`ip -4 route show 10.210.C.0/24 dev I` 里多出的删除、缺的 replace
```
- iptables：Agent 自己的链 `SPARK-CLU`（与 `SPARK-FWD` 同样的「`iptables -S` 比对 → `iptables-restore --noflush` 原子重建」做法）。期望规则**按 `iptables -S` 的打印顺序**（`-s -d -i -o -p -m … -j`）写，否则每 60 秒都会判漂移重建：
  ```
  -N SPARK-CLU
  -A SPARK-CLU -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
  -A SPARK-CLU -s A_r/32 -d A_k/32 -i sch<x> -o I -j ACCEPT        # 每个对端 k：出向
  -A SPARK-CLU -s A_k/32 -d A_r/32 -i I -o sch<x> -j ACCEPT        # 入向（同时限定入口网卡 I）
  -A SPARK-CLU -j DROP
  ```
- **跳转组**：`DOCKER-USER` 与 `INPUT` 的链首由 Agent 维护一组有序跳转（不再是「各自争第 1 位」）：`DOCKER-USER` 前 3 条恰为 `-i spark0 -j SPARK-FWD`、`-i sch+ -j SPARK-CLU`、`-o sch+ -j SPARK-CLU`；`INPUT` 前 2 条恰为 `-i spark0 -j SPARK-IN`、`-i sch+ -j SPARK-IN`（容器碰不到宿主机，包括网关 `G`）。由 `netfilter/iptables` 的 `ensureJumpGroup(chain, jumps)` 实现：前 k 条已按序相同 → 不动；否则先把缺的按位插入、再删掉别处的重复（任何时刻隔离跳转都在），`SPARK-CLU` 不存在时先 `-N` 并放一条 `-j DROP`（内容由 clusternet 的 `SyncRules` 管）。没有集群的机器上跳转组也在，`SPARK-CLU` 只有 `-j DROP`。
- 不做 NAT（Docker 的 MASQUERADE 只匹配 `-s 10.200.0.0/24`）；不限速（集群流量不计入实例带宽，§17 遗留）；docker 默认 `FORWARD` 策略 DROP 兜底——链缺失时集群流量是「不通」而不是「放开」。
- 对端宿主机收到 `dst = A_r` 的包 → 查 `/32 dev sch<x>` 转进容器；反向路径校验：`src = A_k` 的路由经 `I`，`rp_filter` 严格模式通过。
- 停容器 → 容器 netns 销毁 → veth 对与其上的地址、`/32` 路由随之消失；对端路由由 `Teardown`（洗机 / 退出集群 / create 失败的本地清理）删除。
- 前提（真机准备项）：成员宿主机之间的二层网络不能做客户端隔离 / IP 源地址防护（部分 WiFi AP、办公交换机会丢弃源地址为 `10.210.x` 的包）；优先用有线。

### 3.4 macvlan 模式（`cx7`）
```
# vlan fabric：缺则建 VLAN 子接口（名字 scv<vlan>，不能用 <iface>.<vlan>：CX-7 网卡名 13 字符 + 后缀超过 15）
ip link add link <iface> name scv<vlan> type vlan id <vlan>; ip link set scv<vlan> up
parent = scv<vlan>（vlan）或 <iface>（direct）
# 父接口卫生（每次 Apply 与每轮巡检都做）：nmcli device set <iface> managed no（有 nmcli 时，失败忽略）；
#   ip -4 addr flush dev <iface>/<parent>；sysctl -w net.ipv6.conf.<iface>.disable_ipv6=1（parent 同样）；mtu > 0 时 ip link set … mtu <mtu>
# 只有容器里没有 spc0 时才建网卡（spc0 已存在：绝不 del / add）；宿主机残留 scm<x> 先删
ip link add scm<x> link <parent> type macvlan mode bridge
ip link set scm<x> netns <pid>
nsenter -t <pid> -n ip link set scm<x> name spc0
# 幂等部分每次执行
nsenter -t <pid> -n ip addr replace A_r/24 dev spc0
nsenter -t <pid> -n sysctl -w net.ipv6.conf.spc0.disable_ipv6=1
nsenter -t <pid> -n ip link set spc0 up
```
- 同一二层内成员直接互通，不需要宿主机路由与 iptables（macvlan 帧不经宿主机 netfilter）；bridge 模式的 macvlan 子接口**与父接口宿主机不能互通**（内核语义），容器碰不到本机宿主。
- RDMA（`spec.rdma`）：`docker create` 时加 `--device /dev/infiniband/rdma_cm --device /dev/infiniband/uverbsN`（N 来自探测；设备只能在 create 时给，所以 RDMA 与否在开通时决定）。宿主机重启后 `uverbsN` 编号可能变：每次 Apply 复核 `/sys/class/infiniband_verbs/<create 时的 uverbsN>/ibdev == spec 的 HCA`，不符 → `NCCL_IB_DISABLE=1`、状态 `error: RDMA 设备编号变化，需重建实例`（仍可 TCP 运行）。`spc0` 配好地址后，Agent 在宿主机 sysfs 找 RoCE v2 GID：遍历 `/sys/class/infiniband/<hca>/ports/1/gids/<i>`，取等于 `::ffff:A_r` 且 `gid_attrs/types/<i>` 为 `RoCE v2` 的 `i`（最多等 5 秒，GID 表异步填充）→ 写进 `cluster.env` 的 `NCCL_IB_GID_INDEX`；找不到 → 状态 `error: 未找到 RoCE GID`，`NCCL_IB_DISABLE=1` 退回 TCP（仍可用）。
- 父接口卫生：NetworkManager / netplan 可能在链路变化时重新给 CX-7 口加 IPv4 或 IPv6 链路本地地址，那样对端容器就能直达本机宿主。所以卫生检查放进 60 秒巡检（`Check`）：发现地址 → 清除、状态 `error: 父接口出现地址，已清除`（平台告警 `cluster_net_error` 一次）。`deploy/agent/install.sh --cluster-iface <name>`（可多次）写 `/etc/NetworkManager/conf.d/99-spark-cluster.conf`（`unmanaged-devices=interface-name:<name>;…`）并 `nmcli general reload`；`selfcheck` 对受 NM 管理的 CX-7 口给 warn。Agent 只对 fabric 指定（或自动选中）的 CX-7 口做这些，绝不碰管理网卡（`ResolveIface` 已保证 cx7 网卡 ≠ 管理网卡，Agent 再按「持有本机 lan IP / 默认路由」校验一次）。
- 洗机 / 退出集群：macvlan 随 netns 销毁；VLAN 子接口在本机再没有使用它的集群实例时删除。

### 3.5 隔离分析（必须逐项真机验收，§16.4）
| 威胁 | routed（lan） | macvlan（cx7） |
|---|---|---|
| 容器改 IP / 路由 / 建 VLAN 冒充别人 | 没有 `NET_ADMIN`，做不到 | 同左 |
| 原始套接字伪造源地址 / 二层帧（含 802.1Q 标签） | `--cap-drop NET_RAW`；即使伪造，宿主机 `SPARK-CLU` 按 (源, 目的, 入口网卡) 放行 | `--cap-drop NET_RAW`；Raw Packet QP 也需要 `CAP_NET_RAW` |
| 访问本机宿主（Agent、sshd、Docker） | `INPUT -i sch+ -j SPARK-IN` 丢弃 | macvlan bridge 子接口与父接口不通；父接口无地址（巡检持续清除，NM 设为 unmanaged） |
| 访问对端宿主 | 对端宿主 IP 走 `spark0` → `SPARK-FWD` 私网丢弃；走 `spc0` 的只有 `10.210.C.0/24` 路由 | `direct`：对端宿主 CX-7 口无地址（同上）；`vlan`：宿主机不在集群 VLAN 上（父接口只有 macvlan） |
| 访问别的集群 / 普通实例 | 普通实例只有 `spark0`，私网全丢；别的集群的 IP 不在本机 `SPARK-CLU` 白名单 | `direct` 物理只有两台；`vlan` 按集群分 VLAN，交换机端口 trunk 只放行 fabric 的 VLAN 范围、**不配 native VLAN**（负责人准备项）|
| RDMA 选用别的 GID 发包（绕过 VLAN） | 无 RDMA | 内核对用户态 verbs / rdma_cm 做 netns 过滤：RoCE 的源 GID 只能选**本 netns 内网卡**的 GID（`rdma_check_gid_user_access`），容器只看得到 `spc0` 的 GID，用不了宿主机父接口的默认 GID 或别的 VLAN；`vlan` fabric 上未打标签帧再被交换机（无 native VLAN）兜底丢弃。真机验收用容器内 `ibv_devinfo -v` / `show_gids` 核对可见 GID |
| 机房内网其它设备向 `10.210.C.x` 注入 | 风险：能把包发到宿主机并伪造 `A_k` 源地址（`SPARK-CLU` 入向已限定 `-i I`，挡不住同网段伪造）。**`lan` 模式依赖机房内网可信**（与平台经内网 SSH 连容器同一假设），文档与后台页面标注 | 不适用 |
| Ray / NCCL 端口无鉴权 | 只有同一租户的成员可达 | 同左 |

### 3.6 集群信息注入
Agent 在应用 Spec（create、ConfigureCluster、每次 start）时原子写 `ssh/<id>/cluster.json` 与 `ssh/<id>/cluster.env`（容器内 `/.spark/cluster.{json,env}`）；`init.sh` 新增：
```sh
if [ -f /.spark/cluster.json ]; then
  ln -sfn /.spark/cluster.json /etc/spark-cluster.json
  printf '%s\n' '[ -f /.spark/cluster.env ] && . /.spark/cluster.env' > /etc/profile.d/spark-cluster.sh
else
  rm -f /etc/spark-cluster.json /etc/profile.d/spark-cluster.sh
fi
```
`/etc/spark-cluster.json`：
```json
{"cluster_id":"clu-7f3k2m9q1a","version":2,"size":4,"rank":1,"ifname":"spc0","subnet":"10.210.7.0/24",
 "ip":"10.210.7.12","head_ip":"10.210.7.11","network":"rdma",
 "members":[{"rank":0,"instance_id":"ins-a1b2c3d4","ip":"10.210.7.11"},{"rank":1,"instance_id":"ins-e5f6g7h8","ip":"10.210.7.12"}, …],
 "rdma":{"hca":"rocep1s0f1","gid_index":3}}
```
`network ∈ rdma / ethernet`；非 RDMA 时 `"rdma": null`。`/.spark/cluster.env` **每行都是 `export KEY='value'`**（单引号按 `'` → `'\''` 转义），所以任何 `. /.spark/cluster.env` 都会把变量导出给子进程（`ray start`、`vllm serve`、`torchrun` 读到的 `VLLM_HOST_IP / NCCL_* / GLOO_*` 才生效）：`SPARK_CLUSTER_ID / SIZE / RANK / IP / HEAD_IP / IPS（逗号分隔，按 rank）/ IFNAME=spc0 / NETWORK`、`NCCL_SOCKET_IFNAME=spc0`、`GLOO_SOCKET_IFNAME=spc0`、`TP_SOCKET_IFNAME=spc0`、`VLLM_HOST_IP=<ip>`、`MASTER_ADDR=<head_ip>`、`MASTER_PORT=29500`、`NNODES=<size>`、`NODE_RANK=<rank>`；RDMA 时 `NCCL_IB_HCA=<hca>`、`NCCL_IB_GID_INDEX=<i>`、`NCCL_IB_DISABLE=0`，否则 `NCCL_IB_DISABLE=1`。`ssh host cmd` 这类非登录会话不读 profile，帮助页与连接示例写明先 `. /.spark/cluster.env`。

## 4. Agent 协议与 Agent 改动

### 4.1 proto 变更（`api/proto/agent/v1/agent.proto`，`make proto` 重新生成）
```proto
message Hello { …; NetInfo net = 5; }                 // 计划 13：网卡探测
message StateReport { …; NetInfo net = 3; }           // 每 60 秒同步一次探测（链路变化可见）
message InstanceState { …; string cluster = 3; }      // "" 非集群 / "ok" / "pending"（容器未运行）/ "error: <原因>"
message CreateInstance { …; ClusterSpec cluster = 8; } // 非空 = 集群成员（create 时决定 RDMA 设备与 cap-drop）
message Command { oneof body { …; ConfigureCluster configure_cluster = 15; } }
message ConfigureCluster { ClusterSpec cluster = 1; } // cluster_id 为空 = 退出集群（清理网络与文件）

message ClusterSpec {
  string cluster_id = 1; uint32 version = 2; uint32 size = 3; uint32 rank = 4;
  string mode = 5;          // routed / macvlan
  string iface = 6;         // 宿主机集群网卡（routed 出口 / macvlan 父接口）
  uint32 vlan = 7;          // 0 = 不打标签
  uint32 mtu = 8;           // 0 = 继承
  string subnet = 9;        // 10.210.C.0/24
  bool rdma = 10;
  repeated ClusterMember members = 11;   // 含自己，按 rank
}
message ClusterMember { uint32 rank = 1; string instance_id = 2; string ip = 3; string host_ip = 4; }  // host_ip 只在 routed 有
message NetInfo { string lan_iface = 1; repeated NetIface ifaces = 2; }   // lan_iface 仅供参考：未收到 AgentConfig 时为空；平台按 machines.lan_ip 认管理网卡
message NetIface {
  string name = 1; string mac = 2; bool up = 3; int32 speed_mbps = 4; uint32 mtu = 5; repeated string ipv4 = 6;   // ipv4 为 CIDR，如 192.168.1.221/24
  string driver = 7; string pci = 8; bool wireless = 9; string rdma_device = 10; bool rdma_active = 11; string uverbs = 12; string link_layer = 13;
}
```
`internal/agent/features`：`Cluster uint32 = 1 << 1`（新 Agent 恒报 `SharedSlots | Cluster`）。

### 4.2 共享纯包 `internal/clusterspec`（平台与 Agent 共用，无依赖）
`Spec / Member / NetInfo / Iface / Runtime` Go 类型（JSON 字段与 proto 同名；`Iface.IPv4` 为 CIDR 字符串）、`Subnet(pool, index)`、`MemberIP(subnet, rank)`、`GatewayIP(subnet)`、`(Spec).Validate()`、`Self() Member / Head() (Member, bool) / Peers() []Member`、`ResolveIface(net, kind, configured, lanIP) (Iface, hostIP, error)`、`NetPrefix(cidr) string`（`cluster_net` 用）、`RenderJSON(spec, rt) / RenderEnv(spec, rt)`（env 每行 `export K='v'`）、接口名生成 `HostVeth(id) / TempName(id) / MacvlanName(id) / VLANDev(vlan)`、Agent 错误前缀 `ErrPrefix = "cluster-net: "`（Agent 因集群网络失败的 create 结果以它开头，平台据此不判机器故障，§5.5）。`fleet` 指令载荷、`agentgw` 转换、Agent 执行器都用它，保证一处定义。

### 4.3 Agent 执行器
- `executor.Command` 增 `Cluster *clusterspec.Spec`，类型 `configure_cluster`（实例指令队列，`opMu` 串行，时限 2 分钟）。`state.Instance` 增 `Cluster *clusterspec.Spec`、`ClusterRuntime *clusterspec.Runtime`（旧状态文件读出为 nil）。
- **探测**：`Net.Probe(ctx, lanIP)` 的 `lanIP` 可以为空（AgentConfig 尚未到达）——此时 `lan_iface` 为空串，其余字段照常；平台不依赖它（§3.2）。
- **create**：`Cluster != nil` 时先 `Spec.Validate()`、`Deps.Cluster == nil`（不支持）→ 失败；`runtime.Spec` 增 `CapDrop: ["NET_RAW"]`、`Sysctls: {"net.ipv4.ping_group_range": "0 2147483647"}`、RDMA 时 `Devices: Net.Devices(spec)`（`/dev/infiniband/rdma_cm` + `/dev/infiniband/uverbsN`；探测不到 → create 失败——平台只在 `cluster_rdma` 时才下发 `rdma=true`，这里是兜底）；写 `cluster.json / cluster.env` → 建容器 → `startChecked`。集群网络相关的 create 失败，错误消息一律以 `clusterspec.ErrPrefix` 开头，且返回前**在本机清理**：`Net.Teardown`（对端路由、VLAN 子接口）+ `Net.SyncRules`（去掉该实例的规则），容器停掉（容器与数据盘由平台随后的洗机删除）。
- **startChecked** 末尾（限速之后）：`Cluster != nil` → `Net.Apply(ctx, id, pid, spec)`（差量、非破坏性，§3.3 / §3.4）得到 `Runtime{HCA, GIDIndex, RDMAActive}` → 重写 `cluster.env`（带 GID）→ routed 时 `Net.SyncRules(全部本机 routed Spec)`。**create 路径**上 Apply 失败 → 上面的本地清理、create 失败；**之后的 start / 恢复**上 Apply 失败 → 容器保持运行、状态 `error: …`（集群网络不通但隔离不受影响：routed 缺规则时 FORWARD 默认丢弃，macvlan 不经宿主），由 60 秒巡检重试。
- **ConfigureCluster**：`version < 本地 version` → 忽略（成功）；`cluster_id == ""` → `Teardown` + `SyncRules` + 删文件 + 清 `state`；否则存 `state`、重写文件；容器在运行 → `Apply`（差量：只增删对端路由与规则，`spc0` 不重建）+ `SyncRules`；mode / iface 与 create 时不同 → 状态 `error: 集群网络模式变更需重建实例`（只更新文件）；`rdma` 由真变假 → `NCCL_IB_DISABLE=1`，由假变真不生效（设备只能 create 时给），状态 `error: RDMA 变更需重建实例`。
- **60 秒巡检**（`refreshRules` 内，与 `ensureLimits` 同模式 `opMu.TryLock`）：对每个运行中的集群实例 `Net.Check`——覆盖容器内 `spc0` 地址与路由、宿主机 `/32` 与对端路由集合、`SPARK-CLU` 与跳转组、VLAN 子接口存在且 up、父接口无地址 / IPv6 关闭 / NM unmanaged、RDMA 设备编号映射。不通过、或上一轮状态为 `error` → 重新 `Apply`；**每轮都回写状态**（`ok` / `error: …`），所以修复后状态会恢复为 `ok`。
- **wipe**：删容器之后 `Teardown`（对端路由、VLAN 子接口引用计数）→ `SyncRules`（去掉该实例的规则）→ 删 ssh 目录（文件随之删除）；失败计入洗机校验失败。
- **Report**：`InstanceState.cluster`：非集群 `""`；容器未运行 `pending`；最近一次 Apply / Check 失败 `error: <≤ 200 字节>`；否则 `ok`。
- **Recover**：与普通实例一样恢复；`startChecked` 里的 `Apply` 对已配好的 netns 不产生 `link add / del`，正在跑的分布式任务不受 Agent 重启影响。
- **init.sh**：§3.6 的片段（每次 start 重写，存量实例下次启动即生效）。

### 4.4 Agent 包结构
- `internal/agent/clusternet`：接口 `Net{Probe, Apply, Check, Teardown, SyncRules, Devices}`、`ErrNoGID` + `fake`（记录调用、可注入失败，平台集成测试与 e2e 用）。
- `internal/agent/clusternet/linux`：sysfs 探测（根目录可替换，测试用 `testdata/sysfs-*` 夹具：GB10 无 CX-7、标准 Spark 四个 CX-7 netdev、RoCE GID 表）+ `ip / nsenter / iptables / sysctl / nmcli` 命令（`cmdrun.Runner`，快照测试）。
- `internal/agent/netfilter/iptables`：`ensureFirstJump` 改为 `ensureJumpGroup`（§3.3 跳转组），确保 `SPARK-CLU` 链存在。
- `cmd/spark-agent`：装配 `clusternet/linux`；`selfcheck` 增「集群网卡」一节（列出网卡与 RDMA 状态、提示 CX-7 口上有地址会被清除、CX-7 口受 NM 管理时给出 `install.sh --cluster-iface` 用法、`ip_forward=1`、`10.210.0.0/16` 与本机地址 / 路由不冲突）。

## 5. rental：实例集群

### 5.1 用户下单
`BatchRequest` 增 `Cluster bool`。`validateBatch`：`Cluster` ⇒ `Count ∈ {2, 4}`（`cluster_size_invalid`）、`StartAt == nil`（`cluster_booking_unsupported`）、`MachineID == nil`、`AppID == nil`（`BadRequest("集群请在开通后到集群页部署应用")`——否则下单预装会给每台各装一个单机应用，绕过 §6.1 的 `required` 规则）。

`alloc.go` 把 `pickWhereSQL` 的 WHERE 条件抽成片段 `pickCondSQL`（`pickWhereSQL = "SELECT m.id FROM machines m LEFT JOIN machine_groups g … WHERE " + pickCondSQL`，行为不变），`pickSpec` 增 `FabricID *int64`、`ClusterNet *string`、`ClusterOnly bool`（`m.fabric_id IS NOT NULL AND m.cluster_ready`）。集群分支（在原 `placeOnMachines` 位置）用**单独的** fabric 候选 SQL（不能把只选 `m.id` 的 `pickWhereSQL` 当子查询）：

```
SELECT m.fabric_id, m.cluster_net, (f.rdma AND bool_and(m.cluster_rdma)) AS rdma, count(*) AS free
FROM machines m JOIN cluster_fabrics f ON f.id = m.fabric_id LEFT JOIN machine_groups g ON g.id = m.group_id
WHERE <pickCondSQL，即时分支参数> AND m.cluster_ready
GROUP BY m.fabric_id, m.cluster_net, f.rdma, f.isolation
HAVING count(*) >= N AND (f.isolation <> 'direct' OR N = 2)
ORDER BY rdma DESC, free ASC, m.fabric_id, m.cluster_net

for each (fabric, cluster_net):                    -- 每个候选一个 savepoint
    insts = insts[:0]                              -- 回调追加的实例列表每次尝试前清空
    placeOnMachines(spec{…, FabricID, ClusterNet, ClusterOnly: true}, N, 同原回调)   -- 原样复用：锁 / 排他冲突换机 / 补取
    成功 → break；no_capacity → 回滚 savepoint（放掉本次加的机器行锁），试下一个
都不行 → ClusterNoCapacity(N)「暂无可组成 N 台集群的空闲机器（需同一高速网络）」
createCluster(tx, user, fabric, instances)：
    pg_advisory_xact_lock(hashtext('cluster-alloc'))    -- 在机器行锁之后取，串行化子网 / VLAN 分配
    subnet_index = 最小的未被 active 集群占用的 0..255；vlan（vlan fabric）= [vlan_min, vlan_max] 中最小未占用
    mode = routed（lan）/ macvlan（cx7）；rdma = f.rdma ∧ 全员 cluster_rdma；mtu = f.mtu
    INSERT instance_clusters(state active, batch_id) ；成员按机器 ID 升序给 rank 0..N-1，UPDATE instances SET cluster_id, cluster_rank
然后（原流程）每台 charge → recordEvent → enqueueCreate（payload.Cluster = ClusterSpecFor(cluster, rank)）
```
- 非集群的即时分配 `pickOrderSQL` 首个排序键加 `(m.fabric_id IS NOT NULL)`（单机订单优先用不属于 fabric 的机器，给集群留整块；存量无 fabric，行为不变）。
- 可用性：`ClusterAvailability(productID, groupID)` → `{2: {available: Σ_(f,net) ⌊free/2⌋, rdma: 有 RDMA 候选能组}, 4: {…}}`（`direct` 只计入 2）；`GET /availability?cluster_size=` 返回其中一项。

### 5.2 平台节点组（`PlaceSystemCluster`）
`SystemClusterRequest{MachineIDs []int64; GroupID, ImageID int64; Note string}`：按机器 ID 升序 `FOR NO KEY UPDATE`（等待）→ 每台复核 `PlaceSystem` 的全部条件 + `cluster_ready` + 同一 `fabric_id` 且同一 `cluster_net` → `createCluster(system=true, user=系统账号)` → 每台按 `PlaceSystem` 插系统实例（`cluster_id / rank`）+ lease 10 年 + `MarkAllocated` + 事件 → Create（带 Spec）。任何一台不满足 → `ErrMachineUnavailable`，整个事务回滚。`ReleaseSystemCluster(clusterID)`：锁集群行 → 按实例 ID 升序锁成员 → 每个在位成员 `beginWipe`（与 `ReleaseSystem` 同链路）。

### 5.3 ClusterSpec 构造
`ClusterSpecFor(ctx, q, clusterID, rank)`：读集群行 + fabric + **在位成员**（§5.4 定义，按 rank）+ 成员机器 `net_info / cluster_iface / lan_ip` → `clusterspec.Spec{ClusterID, Version, Size, Rank, Mode, Iface: ResolveIface(…).Name, VLAN, MTU, Subnet, RDMA, Members[{rank, instance_id, ip, host_ip(routed)}]}` → `Validate()`。成员缺位（rank 被退款释放、或换机中旧成员已进入洗机）时 `Members` 只含在位成员，`Size` 不变。

### 5.4 成员口径与集群状态（派生，`ClusterView.Status`）
- **在位成员** = `cluster_id` 指向本集群且 `state IN (creating, running, stopped)` 的实例（`rental.ClusterMemberStates`）；ClusterSpec、rank 唯一、`CreateCluster` 的成员数校验都按它。**未结束成员** = 在位成员 ∪ `wiping`；集群是否结束（§5.5 第 4 步）按它。
- `state = failed / released` → 原样；
- `ready_at IS NULL` → `forming`（组建中）；
- `size` 个 rank 都有 running、未暂停、且 `last_state_report` 里该实例 `cluster = ok` 的在位成员 → `ready`（就绪）；
- 其余 → `degraded`（降级：有成员停机 / 暂停 / 缺位 / 网络异常 / 换机中）。

### 5.5 清扫任务 `SweepClusters`（`rental.Sweep`，排在 `FailTimedOutCommands` 之后）
候选：`state='active'` 的集群；每个集群独立事务，**锁序：集群行 `FOR UPDATE` → 成员实例按 ID 升序**：
1. **开通期中止**（非系统集群，`ready_at IS NULL`）：有成员 `failed / cancelled`，**或** `now ≥ created_at + ClusterFormTimeout(30m)` 仍未就绪（成员暂停、缺位、网络异常都会走到这里）→ 其余在位成员——`creating`：`CancelCommands` + `beginWipe` + `refundAll`；`running / stopped`：`beginWipe` + `refundAll`；事件 `"cluster aborted: <原因>"`；集群 `state='failed', ended_at`；`RaiseAlertOnce(warning, cluster_failed, "集群 <id> 开通失败，已全额退款")`。系统集群不在这里中止（llm Controller 负责）。
2. **集群网络导致的开通失败不判机器故障**：`HandleCommandResult` 对集群成员的 Create 失败，若消息以 `clusterspec.ErrPrefix` 开头 → 走 `failCreatingClusterNet`：实例 failed、`refundAll`、`CancelCommands`、告警 `cluster_net_error`，**不** `MarkFaulty`；容器由现有对账（failed 实例的容器 → `wipeOrphan`）洗掉，Agent 已在本地清理了路由与 VLAN（§4.3）。其它原因仍走 `failCreating`。
3. **就绪与对齐**：在位成员数 = size 且全员 running 未暂停、`ready_at` 为空 → 置 `ready_at = now`（系统集群同样置，只是不对齐）；非系统集群在这一刻把全员 `expires_at` 拉到其中最晚的一个（平台让出开通先后差的几分钟）、`refreshLease`、事件 `"cluster aligned to <t>"`。**之后不再对齐**：成员因失联暂停而产生的到期差不补（补就等于白送其余 N−1 台的时长），控制台在差距 > 60 秒时提示「成员到期时间不一致」。
4. **结束**：没有未结束成员（全部 `released / failed / cancelled`）→ `state = ready_at IS NULL ? 'failed' : 'released'`、`ended_at = now`（子网 / VLAN 随之释放）。

### 5.6 成员操作规则
| 操作 | 集群成员 | 说明 |
|---|---|---|
| 用户 `Renew / RenewQuantity / SetAutoRenew / Release` | `ErrClusterMember` | 改用 §5.7 的集群接口 |
| 用户改密码 / 端口 / 登录方式 / 部署普通应用 | 允许 | |
| 后台 `AdminExtend` | `ErrClusterMember` | 改用 `AdminExtendCluster` |
| 后台 `RefundRelease` | 允许 | 锁序 集群 → 实例；之后 `version + 1`，对其余在位成员 `ConfigureCluster`（成员列表去掉它）|
| 后台 `AdminMigrate` | 允许（新机器同 fabric 同 `cluster_net`、`cluster_ready`、`ClusterOnly` 候选） | 锁序 **集群 → 原实例** → 新机器 → 指令；新实例继承 `cluster_id / cluster_rank`；`version + 1`；新实例 Create 带新 Spec，其余在位成员 `ConfigureCluster`；集群部署见 §6.6 |
| 预订、指定机器、下单预装应用 | 不适用 / 拒绝 | 集群只即时开通，应用在集群页部署 |
| 到期停机 / 洗机 | 逐实例照常（首次就绪时已对齐，通常同一轮） | |
| 机器失联暂停 / 恢复 | 逐实例照常，**不重新对齐** | 见 §5.5 第 3 步 |
| `SendReminders` | 只给 rank 最小的在位成员发短信，其余成员同步置提醒标记 | 一个集群一条短信 |
| `AutoRenewDue` | 候选排除 `cluster_id IS NOT NULL` | 由 `AutoRenewClustersDue` 处理 |
| 对账 `Reconcile` | 不变；报告里 `cluster` 以 `error:` 开头 → `RaiseAlertOnce(warning, cluster_net_error, machine, instance, "实例 <id> 的集群网络异常")` | |

### 5.7 集群级接口
- `RenewCluster(ctx, userID, clusterID, d, quantity, key)`：幂等咨询锁 `idem:<user>:<key>` → 锁集群 → 在位成员（ID 升序）→ 每个成员 `renewFits` → 每个成员 `charge(kind=renew)`（键 `key#<rank>`）→ `addTime`（每个成员加同样的时长，保持原有差距）→ 全成或全败（余额不足 → `insufficient_balance` 带总价）。重放：命中 `key#*` 的续费单 → 返回它们。
- `SetClusterAutoRenew(ctx, userID, clusterID, enabled, d, ip)`：全员同设。
- `ReleaseCluster(ctx, userID, clusterID)`：全员走 `Release` 的同一链路（不退款）。
- `AutoRenewClustersDue`：候选 = 非系统、active、`ready_at` 非空、`auto_renew`、在位成员全是 running（**允许有成员暂停**，否则一台失联会让整集群到期停机）、`min(未暂停成员的 expires_at) ≤ now + AutoRenewLead`（全员暂停则跳过）、失败退避同 `AutoRenewDue`；锁集群 → 成员；档位按 `autoRenewDuration` 取全员都放得下的最大档；全员 `charge(auto=true)` + `addTime`（暂停中的成员按现有「running 暂停」分支加到冻结的剩余时长上）；任一失败 → 回滚，独立小事务给全员写 `auto_renew_failed_*`，首次失败对 rank 最小成员发一条短信。
- `AdminExtendCluster(ctx, actor, clusterID, seconds, reason)`：全员 `addTime`（受 `renewFits`）；审计 `cluster.extend`。
- `AdminResyncCluster(ctx, actor, clusterID)`：`version + 1` 并向全部在位成员下发 `ConfigureCluster`（排障用）；审计 `cluster.resync`。

### 5.8 指令
- `fleet.CmdConfigureCluster = "configure_cluster"`，载荷 `ConfigureClusterPayload{Cluster clusterspec.Spec}`；`CreateInstancePayload.Cluster *clusterspec.Spec`。
- `HandleCommandResult`：`configure_cluster` 成功无副作用；失败 → `RaiseAlertOnce(warning, cluster_net_error, …)`。`FailTimedOutCommands`：`ConfigureClusterCommandTimeout = 2m`。Create 失败的分流见 §5.5 第 2 步。
- 同一实例上 Create（旧版本）与 ConfigureCluster（新版本）都可能在途：Agent 按 `version` 取新，顺序无关。

### 5.9 计费
- 下单：N 张 `new` 订单（与「一次多台」完全相同），一个 `rental_batches` 行；控制台显示「集群 N 台 × 单价」。
- 续费 / 自动续费：N 张 `renew` 订单；后台延长不收费。
- 退款：逐实例（现有规则）；开通期中止全额退（§5.5 第 1、2 步）。
- 不变量（`rental.CheckClusterInvariants`，集成测试与真机验收后执行）：① 每个 active 集群：在位成员的 rank 互不相同且 `< size`、成员 `system = 集群.system`、成员都在 `fabric_id` 的机器上；② active 集群的 `subnet_index` 唯一、vlan fabric 上 `vlan` 唯一（索引保证，查询复核）；③ 非系统集群：`batch_id` 下 `kind='new'` 的订单恰有 `size` 张，每张订单的实例要么本身是本集群成员，要么经 `instance_transfers`（换机链，可多跳）连到一个本集群成员；④ 系统集群的成员没有任何订单；⑤ `failed` 的非系统集群：全部成员（含换机前的旧实例）的已付订单都已全额退款（`refunded_amount = amount`）。

## 6. 集群应用（appdeploy）

### 6.1 spec 扩展
```json
"cluster": {"mode": "required", "min_size": 2, "max_size": 4, "worker_ports": false}
```
`Spec.Cluster *ClusterSupport{Mode: optional|required, MinSize, MaxSize, WorkerPorts}`；`ValidateSpec`：`2 ≤ min ≤ max ≤ 8`，`mode` 二选一；不写 = 不支持集群（不改变既有版本的 `spec_hash`）。
- `required`：只能 `CreateCluster`；单实例 `Create` 与下单预装 `PrepareInstall` 都返回 `app_cluster_required`（集群下单本身也拒绝 `AppID`，§5.1）；
- `optional`：两种都行（`vllm-node 1.1.0`：N = 1 的节点仍用单实例部署）；
- 无：`CreateCluster` → `app_cluster_unsupported`。

### 6.2 `CreateCluster(ClusterCreateRequest{UserID, ClusterID, AppID, VersionID?, Env, IP})`
一个事务：锁集群行（用户须是 owner；系统集群只允许系统账号）→ 锁全部在位成员实例（ID 升序）→ 校验：集群 `active`、在位成员数 = size、全员 `creating / running / stopped`、size 在 `[min, max]`（`app_cluster_size`）、集群上没有 active 的集群部署（`app_cluster_exists`）、每个成员的镜像兼容、每个成员上该应用没有活动部署、端口规则（`checkPorts` 每个成员）、`ValidateEnv` 一次 → 插 `app_cluster_deployments(active)` → 每个成员插一条 `app_deployments`（`cluster_deployment_id`、`cluster_role = rank0 ? head : worker`，env 相同）→ 事件 / 审计 `cluster.app_deploy`（user actor 或系统）。Runner 之后按单条部署照常推进。

### 6.3 保留变量与端口
- `ReservedEnv`：`cluster_deployment_id` 非空的部署追加 `SPARK_CLUSTER_ROLE=<head|worker>`、`SPARK_CLUSTER_DEPLOYMENT=<acd-…>`（**只对集群部署**，其它部署的 env 逐字节不变，不触发存量重装）。集群拓扑（IP、rank、NCCL 变量）由 app.sh 读 `/.spark/cluster.env`（Agent 维护，换机后自动更新）。
- `worker_ports=false` 时 worker 部署 running 后**不登记** `instance_ports`（端口只在 head 实例上出现）；健康检查仍按 `Ports[0]`（worker 由应用自己的回环健康服务响应，§6.5）。

### 6.4 聚合状态 `ClusterDeployment.Status`
按成员部署：任一 `failed` → `failed`；任一 `removing` → `removing`；全部 `removed / lost` → `removed`；全部 `running` 且 health `ok` → `running`；全部 `stopped` → `stopped`；有成员所在实例已不是集群当前成员（换机）→ `stale`（「需要重新部署」）；其余 → `installing`（进度取成员最小值，阶段取 head）。`Health`：全员 ok → ok，有 unhealthy → unhealthy，否则 unknown。

### 6.5 操作
`RedeployCluster`（全员 `RedeployInTx`，可换版本 / 变量；`stale` 时先为新成员补建部署）、`StopCluster`、`StartCluster`、`RemoveCluster`（全员 remove，集群部署 `removed`）。锁序一律 集群行 → 成员实例（ID 升序）→ `app_deployments`。单个成员部署的用户接口（`/instances/{id}/apps/{dep}/…`）对 `cluster_deployment_id` 非空的部署返回 `app_cluster_managed`（409「该部署属于集群应用，请在集群页操作」），只读接口照常。

### 6.6 与实例生命周期
- `OnInstanceGone`（成员洗机 / 失败）：该成员部署照常 `lost / removed`；聚合进入 `failed / stale`。
- `OnInstanceMigrated`（成员换机）：沿用复制为新实例的 queued 部署（保留 `cluster_deployment_id / role`）；head 的分布式任务已失效 → 聚合 `stale`，等用户（或 llm Controller）`RedeployCluster`。

### 6.7 应用包
**`vllm-node 1.1.0`（内部，`cluster.mode = optional`，`max_size 4`，`worker_ports false`，`timeout_minutes 360`）**：在 1.0.0 基础上新增变量 `TENSOR_PARALLEL_SIZE`、`PIPELINE_PARALLEL_SIZE`（int 1～8，默认 1；平台在 N > 1 时填）。`app.sh`：
- `SPARK_CLUSTER_ROLE` 为空 → 与 1.0.0 相同（单机）。
- 共同：`install` 每个成员都下载模型（同一路径）；`start` 先 `. /.spark/cluster.env`（每行 `export`，`VLLM_HOST_IP / NCCL_* / GLOO_*` 随之导出给 `ray start` 及其子进程；文件缺失 → 失败「集群信息缺失」）、校验 `TP × PP == SPARK_CLUSTER_SIZE`；`PP > 1` 时先 `python3 -c 'import cupy, ray.dag'`（vLLM 0.10 的 Ray 流水线并行走 Ray Compiled Graph，需要 `ray[cgraph]` 的 cupy），失败 → 退出 2 并 `spark_status install 0 "流水线并行需要镜像内 cupy，请改用 TP = 台数"`；`ray stop --force`。默认 `TP = N, PP = 1`，不依赖 cupy。
- worker：后台 `until ray start --address="$SPARK_CLUSTER_HEAD_IP:6379" --node-ip-address="$SPARK_CLUSTER_IP" --num-gpus=1 --object-store-memory=2000000000 --disable-usage-stats --block; do sleep 10; done`（`service.pid`）+ 回环健康服务 `python3 health.py 127.0.0.1:$SPARK_PORT`（`/health` 在 `raylet` 进程存在时 200，否则 503）。
- head：`ray start --head --node-ip-address="$SPARK_CLUSTER_IP" --port=6379 --num-gpus=1 --object-store-memory=2000000000 --dashboard-host=127.0.0.1 --disable-usage-stats` → 后台：等待 `ray.nodes()` 中存活且带 GPU 的节点数 = `SPARK_CLUSTER_SIZE`（超时 = `SPARK_TIMEOUT_S`，超时退出 → 部署 failed「worker 未加入」）→ `export RAY_ADDRESS="$SPARK_CLUSTER_HEAD_IP:6379"`（容器有 eth0 与 spc0 两块网卡，显式指定地址，避免 driver 自动选到 `10.200.0.x`；driver 选 IP 的行为列为 CP-0 / CP-1 真机核对项）→ `vllm serve <dir> --host 127.0.0.1 --port $SPARK_PORT … --distributed-executor-backend ray --tensor-parallel-size $TP --pipeline-parallel-size $PP`（其余参数同 1.0.0）。
- `stop`：`ray stop --force` + 停健康服务。

**`vllm-cluster 1.0.0`（公开，category llm，`cluster.mode = required`，`min 2 / max 4`，`worker_ports false`，`timeout_minutes 360`，镜像 LLM Serving）**：变量 `MODEL_ID`（自由字符串，`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`，必填；帮助列推荐模型）、`MODEL_SOURCE`、`SERVED_MODEL_NAME`（默认 `model`）、`MAX_MODEL_LEN`、`TENSOR_PARALLEL_SIZE`（默认空 = 集群台数）、`PIPELINE_PARALLEL_SIZE`（默认 1）、`GPU_MEMORY_UTILIZATION`、`MAX_NUM_SEQS`、`QUANTIZATION`、`REASONING_PARSER`、`TOOL_CALL_PARSER`、`API_KEY`（secret，可选）、`MODEL_SIZE_GB`。端口 8000（`/docs`，健康 `/health`）。脚本与 `vllm-node 1.1.0` 的集群分支相同（两份文件，`apps_dir_test` 断言两者的 Ray 启动段一致）。

## 7. Token 商城多机节点

### 7.1 模型
`Model` / `ModelInput` 增 `NodesPerReplica, TensorParallel, PipelineParallel`（零值补 `1 / N / 1`）。校验（`llm_model_invalid`）：`N ∈ {1, 2, 4}`；`TP ≥ 1, PP ≥ 1, TP × PP = N`；`kind = embedding ⇒ N = 1`；`N > 1` 要求 `vllm-node` 版本 spec 的 `cluster` 支持且 `max_size ≥ N`；修改 `N` 要求模型没有非终态节点（「请先把副本数调为 0 并等节点全部释放，再修改每副本机器数」）。`NodeReservedEnv` 增 `TENSOR_PARALLEL_SIZE / PIPELINE_PARALLEL_SIZE`；`PlatformNodeEnv` 只在 `N > 1` 时填这两项（N = 1 的节点 env 与计划 9 逐字节相同）。`replicas` 仍是副本数（每副本 N 台）。

### 7.2 状态机（与计划 9 相同的状态，组语义见下）
```
provisioning ──(全员 running → 集群部署聚合 running/ok → 探测 head 成功)──▶ ready ⇄ unhealthy
   │  任一成员实例没了 / 机器 faulty / 部署无法恢复 / 超时                    │ 进程崩溃：2 分钟 → RedeployCluster（rebuild+1），3 次 → failed
   └──────────────────────────────────────────────────▶ failed ◀───────────┤ 成员失联 > 10 分钟 / 成员实例没了 / 机器 faulty：立即
                                                             draining ──(在途 0 或 60s)──▶ removing（ReleaseSystemCluster）──(全员洗完)──▶ removed
```
节点进入 `failed / removed` 的同一事务里 `llm_node_members.live = false`；`failed` 时随后（事务外）`ReleaseSystemCluster`。

### 7.3 Controller 改动（`internal/llm/controller.go`）
1. **对账**：缺额按模型 N 分开处理，**先处理 N 大的模型**；`N = 1` 走原 `provision`，但 `candidateMachines` 排序改为 `(m.fabric_id IS NOT NULL), m.id`（M13）；`N > 1` 走 `provisionGroups`：`candidateFabrics(poolID, now, N)` = 满足 `candidateCond ∧ cluster_ready ∧ (agent_features & 2) ≠ 0` 的池内机器按 fabric 分组、`count ≥ N`、`direct` 只给 N = 2，按 `count ASC, fabric_id` 排序；每组取 ID 最小的 N 台 → `createGroupNode`；`ErrMachineUnavailable` → 换下一个 fabric；都不行 → `llm_pool_short`（同一条告警）。`MaxProvisionPerRound` 按节点（组）计。
2. **`createGroupNode`**：`Rental.PlaceSystemCluster` → 一个事务插 `llm_nodes(size N, machine_id = 成员0机器, instance_id = 成员0实例, cluster_id)` + N 行 `llm_node_members` + 事件；插入失败 → `ReleaseSystemCluster`。
3. **推进 provisioning（组）**：读全部成员实例：任一不存在 / 结束 → `failProvisioning(不可重建)`；有成员非 running → 等（超时 `ProvisionTimeoutFor(version)`）；全员 running 且 `app_cluster_id` 为空 → `Apps.CreateCluster(系统账号, cluster_id, vllm-node, version, NodeEnv)`（`app_cluster_exists` → 认领集群上现有的 active 集群部署）→ 记 `app_cluster_id` 与 head 成员部署到 `deployment_id`；聚合 `failed` → `failProvisioning(可重建)`；`removed / stale` → 不可重建；`running ∧ ok` → `tryPromote`（探测 head）。
4. **unhealthy（组）**：任一成员实例不存在 / 非 running 非 suspended（stopped、wiping…）/ 成员机器 faulty → `failNode`（释放整组）；有成员暂停且 `unhealthy_since + GroupSuspendFailAfter(10m) ≤ now` → `failNode("成员机器失联")`；聚合 `removed / stale` → `failNode`；`unhealthy_since + GroupRebuildAfter(2m) ≤ now`：`rebuild_count ≥ MaxRebuilds` → `failNode` + 严重告警；否则 `Apps.RedeployCluster` → `rebuild_count + 1` → provisioning。
5. **draining / removing / 孤儿**：释放用 `ReleaseSystemCluster`；`removing → removed` 要求全部成员实例已结束；`releaseOrphans` 的「被引用」集合加入 `llm_node_members WHERE live` 的实例。
6. **失败**：`failNode` 对组调用 `ReleaseSystemCluster`。

### 7.4 Prober（组）
`observe` 对 `size > 1`：逐成员读实例——不存在 / 结束 → `vFailNow`；非 running 或已暂停 → `vFail`（provisioning 为 `vSkip`）；然后读集群部署聚合（`failed / removed / stale` → `vFailNow`，非 running / health 非 ok → `vFail`），最后探测 head（不变）。

### 7.5 Router
`Refresh` 的 SQL 增：`AND NOT EXISTS (SELECT 1 FROM llm_node_members mm JOIN instances mi ON mi.id = mm.instance_id WHERE mm.node_id = n.id AND mm.live AND (mi.state <> 'running' OR mi.suspended_at IS NOT NULL))`。网关、在途计数、粘滞、失败转移、计费不变。

### 7.6 后台操作
- 排空 / 移除：对组释放全部成员。
- 重建：`ready / unhealthy` 的组 → `RedeployCluster`（人工重建不计入自动上限）；`failed` 的组 → `llm_node_invalid_state`（「多机节点组失败后由系统在别处自动另建」）。
- `MachineHasLiveNode` 加入成员；池移出校验沿用。
- `PoolView` 增 `fabrics: [{fabric_id, name, kind, rdma, total, free, groups: {"2": n, "4": n}}]`，机器行增 `fabric`、`node: {id, model_slug, state, rank, size}`。

### 7.7 不变量（`llm.CheckInvariants` 新增）
⑧ 每个非终态且 `size > 1` 的节点：`live` 成员恰 `size` 行、rank 为 `0..size-1`、成员实例都是 `system` 且 `cluster_id = 节点.cluster_id`、`node.instance_id` = rank0 实例；⑨ 终态节点没有 `live` 成员；⑩ `ready` 的组：集群部署 active 且成员部署全 running。

## 8. 与现有行为的交互

| 场景 | 行为 |
|---|---|
| 单机下单 / 预订 / 换机 | 不变；单机即时分配优先非 fabric 机器 |
| 一次多台（非集群） | 不变：N 台可落在任意机器，不建集群 |
| 计划 10 共享机 | 搁置；集群成员恒为整机（`share_slots = 1`） |
| 机器维护 / 故障 | 现有规则；集群成员机器 faulty → 集群降级（用户）/ 节点组 failed（平台） |
| 机器改 fabric / 集群网卡 | 有活跃集群成员时拒绝 |
| 旧 Agent | `cluster_ready = false`，不参与集群；其余功能不变 |
| 接入层（SSH / Web / 端口） | 每个成员照常；集群应用的端口只在 head 实例上 |
| 计划 9 网关 | 不变（只拨 head） |
| 计划 12 AI 管家 | `renew_instance / release_instance` 对集群成员得到 `cluster_member` 错误，提示到集群页；不新增管家工具（遗留） |
| 仪表盘 | 新增卡片「集群：活跃 N（就绪 / 降级）」 |
| 洗机 | Agent 清理集群网络（Teardown + SyncRules）；集群在全员结束后 `released` |
| Agent 重启 / 宿主机重启 | `Recover` 重跑 `startChecked`，集群 `Apply` 差量补齐，不打断已有 `spc0` 与连接 |

## 9. 控制台页面
- **市场页下单对话框（`RentDialog.vue`）**：「租用方式」单机 / 集群 2 台 / 集群 4 台；选集群时：数量固定、隐藏预订与指定机器、显示「同一高速网络（RDMA）」或「普通网络（性能有限）」与「可开 X 个」（`/availability?cluster_size=`），总价 = N × 单价 × 数量；不可用时禁用并说明。
- **实例列表**：同一集群的成员折叠成一张集群卡（「集群 clu-xxxx · 4 台 · 就绪 / 组建中 / 降级」+ 成员行），点击进集群页。
- **集群详情 `/clusters/:id`**：状态、网络类型（RDMA / 以太网 / RDMA 不可用）、到期时间（对齐后）；成员表（rank、实例链接、集群 IP、状态、网络状态）；「连接信息」（head 地址、`/etc/spark-cluster.json` 说明、非登录会话先 `. /.spark/cluster.env`（文件每行 `export`）、`torchrun` / Ray 示例——Ray 示例里 head 用 `ray start --head --node-ip-address=$SPARK_CLUSTER_IP`、driver 先 `export RAY_ADDRESS=$SPARK_CLUSTER_HEAD_IP:6379`）；操作：续费（N 单总价确认）、自动续费、释放集群（二次确认）；「集群应用」卡：从支持集群的应用里选（`vllm-cluster`）→ 变量表单（复用 `AppEnvForm`，TP 默认 = 台数）→ 部署；显示聚合状态 / 进度 / 成员角色表（head / worker，各自状态）/ head 的访问地址；重新部署 / 停止 / 启动 / 卸载；`stale` 时显示「成员已更换，请重新部署」。
- **实例详情**：集群成员显示「属于集群 clu-xxxx（rank r / N）」横幅与链接；续费 / 自动续费 / 释放按钮置灰并说明；集群部署在成员的应用列表里显示「集群应用（head / worker）」只读。
- **帮助页**：新增「多机集群」一节（网络类型、集群 IP、环境变量、示例、计费与到期、故障说明、无 SSH 互信）。

## 10. API

错误码（`apperr`）新增：`cluster_not_found`（404「集群不存在」）、`cluster_member`（409「该实例属于集群，请在集群页统一续费、设置自动续费或释放」）、`cluster_size_invalid`（400「集群只支持 2 台或 4 台」）、`cluster_booking_unsupported`（400「集群暂不支持预订，请选择立即开通」）、`fabric_not_found`（404「集群网络不存在」）、`fabric_in_use`（409「该集群网络下还有机器或运行中的集群」）、`fabric_full`（409「直连网络最多 2 台机器」）、`fabric_invalid`（400，按字段）、`app_cluster_required`（400「该应用需要在集群上部署」）、`app_cluster_unsupported`（400「该应用不支持集群部署」）、`app_cluster_size`（400「该应用支持 X～Y 台的集群」）、`app_cluster_exists`（409「集群上已有集群应用，请先卸载」）、`app_cluster_managed`（409「该部署属于集群应用，请在集群页操作」）。`no_capacity` 集群文案「暂无可组成 N 台集群的空闲机器（需同一高速网络）」。

### 10.1 控制台（`/api/v1`）
| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/availability?…&cluster_size=2\|4` | 匿名组；带 `cluster_size` 时返回 `{available, rdma}`（可组成的集群数） |
| GET | `/products` | 每个商品增 `clusters: {"2": {available, rdma}, "4": {…}}` |
| POST | `/bookings` | body 增 `cluster: bool`；响应增 `cluster: ClusterView` |
| GET | `/clusters` | 我的集群（`active` + 最近 30 天结束的）`{items:[ClusterView]}` |
| GET | `/clusters/{id}` | `ClusterView{id, size, status, network: rdma\|ethernet\|rdma_degraded, fabric_kind, subnet, head_ip, expires_at, auto_renew, auto_renew_duration, renew_options, members:[{rank, instance_id, state, ip, machine_label, net_status: ok\|pending\|error, net_reason}]（net_status 为归一化值，error 时 net_reason 为固定文案「集群网络异常，平台已告警处理」；Agent 原始错误只在后台视图）, app?: ClusterAppView, created_at}` |
| POST | `/clusters/{id}/renew` | `{duration, quantity}` + `Idempotency-Key` → `{orders:[…], cluster}` |
| PUT | `/clusters/{id}/auto-renew` | `{enabled, duration}` |
| POST | `/clusters/{id}/release` | 全员释放 |
| GET | `/clusters/{id}/apps` | 当前集群应用（含成员部署）或 `null` |
| POST | `/clusters/{id}/apps` | `{app_id, version_id?, env}` → `ClusterAppView`；审计 `cluster.app_deploy` |
| POST | `/clusters/{id}/apps/{cd}/redeploy\|stop\|start` | `redeploy` body `{version_id?, env?, env_set}` |
| DELETE | `/clusters/{id}/apps/{cd}` | 卸载 |
| GET | `/apps` | 应用视图增 `cluster: {mode, min_size, max_size} \| null`；`required` 的应用不出现在单实例「添加应用」里 |
| GET | `/instances*` | 实例视图增 `cluster: {id, size, rank, status} \| null`；应用视图增 `cluster_role` |
| POST | `/instances/{id}/renew \| release`、`PUT …/auto-renew` | 成员 → 409 `cluster_member` |
| `/instances/{id}/apps/{dep}/redeploy\|stop\|start`、`DELETE` | 集群部署成员 → 409 `app_cluster_managed` |

### 10.2 后台（`/admin/api/v1`）
| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| GET | `/fabrics` | all | `{items:[{…fabric, machine_count, active_clusters}]}` |
| POST / PUT / DELETE | `/fabrics`、`/fabrics/{id}` | ops | 审计 `fabric.create / update / delete` |
| PUT | `/machines/{id}` | ops | 增 `fabric_id?`（null = 移出）、`cluster_iface?`；审计 `machine.update` 的 diff 带这两项 |
| GET | `/machines/{id}` | ops | 增 `fabric`、`cluster_iface`、`agent_features`、`net_info`、`net_info_at`、`cluster_ready`、`cluster_rdma`、`cluster_reason`（未就绪原因） |
| GET | `/machines` | all | 行增 `fabric_name`、`cluster_ready`、`cluster_rdma`；筛选 `fabric_id` |
| GET | `/clusters?state=&user_id=&fabric_id=&system=&page=` | all | 集群列表 |
| GET | `/clusters/{id}` | all | 详情 + 成员（含机器、实例状态、`last_state_report` 的网络状态）+ 版本 + 集群应用 + 最近事件 |
| POST | `/clusters/{id}/extend` | ops | `{seconds, reason}`；审计 `cluster.extend` |
| POST | `/clusters/{id}/resync` | ops | 重新下发 ConfigureCluster；审计 `cluster.resync` |
| POST | `/instances/{id}/migrate` | ops（现有） | 成员：候选限同 fabric |
| POST | `/instances/{id}/extend` | ops（现有） | 成员 → `cluster_member` |
| GET | `/instances/{id}` | all | 增 `cluster` 区块 |
| POST / PUT | `/llm/models*` | super | 增 `nodes_per_replica, tensor_parallel, pipeline_parallel` |
| GET | `/llm/pool` | ops | 增 `fabrics[]` 与机器行 `fabric / node.rank / node.size` |
| GET | `/llm/nodes*` | ops | 节点增 `size`、`cluster_id`、`members:[{rank, machine, instance_id, instance_state, suspended, deployment:{state, health, progress}}]`、`app_cluster:{id, status}` |
| GET | `/dashboard` | all | 增 `clusters: {active, ready, degraded}` |

## 11. 后台页面
- 菜单「机器」下新增「集群网络」（`/fabrics`，all 可看、ops 可改）与「集群」（`/clusters`，all）；`menu.spec.ts` 同步。
- `/fabrics`：表格（名称、类型、隔离、RDMA、MTU、VLAN 范围、机器数、活跃集群）+ 新建 / 编辑抽屉（`kind` 决定可选的 `isolation`，`vlan` 时填范围）+ 删除。
- `/machines/:id`：「集群网络」卡：fabric 选择、集群网卡下拉（探测到的网卡）、就绪 / RDMA 判定与原因、网卡表（名称、状态、速率、MTU、IPv4、驱动、无线、RDMA 设备 / 状态 / uverbs）、探测时间；Agent 能力位。
- `/clusters`：筛选 + 表（ID、用户 / 平台、规模、状态、网络、fabric、子网、创建时间）；`/clusters/:id`：成员表（rank、实例、机器、实例状态、网络状态）、版本、集群应用、事件；操作：延长（ops）、重新下发（ops）。
- `/instances/:id`：集群区块（链接、rank、成员）；集群成员的「延长」按钮引导到集群页，「换机」提示「只能换到同一集群网络的机器」。
- `/llm` 模型抽屉：「每副本机器数」（1 / 2 / 4）、TP / PP（联动校验）；机器池 Tab 按 fabric 分组（可组副本数）；节点 Tab：组节点显示「4 台」徽标，展开成员；节点详情抽屉成员表与集群部署聚合。
- 仪表盘：集群卡片。

## 12. 模块与接口

```go
// internal/clusterspec（纯包）
const (ModeRouted = "routed"; ModeMacvlan = "macvlan"; IfName = "spc0"; DefaultPool = "10.210.0.0/16"; MemberOffset = 11; MaxSize = 8; MasterPort = 29500)
type Member struct{ Rank int; InstanceID, IP, HostIP string }
type Spec struct{ ClusterID string; Version, Size, Rank int; Mode, Iface string; VLAN, MTU int; Subnet string; RDMA bool; Members []Member }
type Iface struct{ Name, MAC string; Up bool; SpeedMbps, MTU int; IPv4 []string /* CIDR */; Driver, PCI string; Wireless bool; RDMADevice string; RDMAActive bool; Uverbs, LinkLayer string }
type NetInfo struct{ LanIface string; Ifaces []Iface }
type Runtime struct{ HCA string; GIDIndex int; RDMAActive bool }
func Subnet(pool netip.Prefix, index int) (netip.Prefix, error)
func MemberIP(subnet netip.Prefix, rank int) netip.Addr
func GatewayIP(subnet netip.Prefix) netip.Addr
func (s Spec) Validate() error
func (s Spec) Self() Member; func (s Spec) Head() (Member, bool); func (s Spec) Peers() []Member
func NetPrefix(cidr string) string          // "192.168.1.221/24" → "192.168.1.0/24"；非法 → ""
const ErrPrefix = "cluster-net: "             // Agent 集群网络失败的 create 结果前缀（平台不判机器故障）
func ResolveIface(n NetInfo, kind, configured, lanIP string) (iface Iface, hostIP string, err error)
func RenderJSON(s Spec, rt Runtime) []byte
func RenderEnv(s Spec, rt Runtime) []byte
func HostVeth(instanceID string) string; func TempName(instanceID string) string; func MacvlanName(instanceID string) string; func VLANDev(vlan int) string

// internal/agent/clusternet
type Net interface {
    Probe(ctx context.Context, lanIP string) (clusterspec.NetInfo, error)
    Devices(s clusterspec.Spec) ([]string, error)
    Apply(ctx context.Context, instanceID string, pid int, s clusterspec.Spec) (clusterspec.Runtime, error)
    Check(ctx context.Context, instanceID string, pid int, s clusterspec.Spec) (bool, error)
    Teardown(ctx context.Context, instanceID string, s clusterspec.Spec, remaining []clusterspec.Spec) error
    SyncRules(ctx context.Context, routed map[string]clusterspec.Spec) error
}

// internal/fleet
const CmdConfigureCluster CommandType = "configure_cluster"
type ConfigureClusterPayload struct{ Cluster clusterspec.Spec }
// CreateInstancePayload 增 Cluster *clusterspec.Spec；InstanceState 增 Cluster string；Machine 增 FabricID, ClusterIface, AgentFeatures, NetInfo, NetInfoAt, ClusterReady, ClusterRDMA, ClusterNet
func (s *Service) RecordHello(ctx context.Context, machineID int64, features uint32, net *clusterspec.NetInfo) error
func (s *Service) RecordNetInfo(ctx context.Context, machineID int64, net clusterspec.NetInfo) error
func (s *Service) ListFabrics / GetFabric / CreateFabric / UpdateFabric / DeleteFabric
// MachineUpdate 增 SetFabric bool; FabricID *int64; ClusterIface *string

// internal/rental
type BatchRequest struct{ …; Cluster bool }; type BatchResult struct{ …; Cluster *ClusterView }
type SystemClusterRequest struct{ MachineIDs []int64; GroupID, ImageID int64; Note string }
type SystemClusterResult struct{ Cluster Cluster; Instances []Instance }
func (s *Service) PlaceSystemCluster(ctx, SystemClusterRequest) (SystemClusterResult, error)
func (s *Service) ReleaseSystemCluster(ctx, clusterID string) error
func (s *Service) GetClusterView(ctx, id string) (ClusterView, error)
func (s *Service) ClusterForUser(ctx, userID int64, id string) (ClusterView, error)
func (s *Service) ListClustersForUser(ctx, userID int64) ([]ClusterView, error)
func (s *Service) AdminListClusters(ctx, ClusterFilter, paging.Page) ([]ClusterView, int, error)
func (s *Service) ClusterSpecFor(ctx, q db.Querier, clusterID string, rank int) (clusterspec.Spec, error)
func (s *Service) ClusterAvailability(ctx, productID int64, groupID *int64) (map[int]ClusterAvail, error)
func (s *Service) RenewCluster / SetClusterAutoRenew / ReleaseCluster / AdminExtendCluster / AdminResyncCluster / SweepClusters / AutoRenewClustersDue
func CheckClusterInvariants(ctx, pool *pgxpool.Pool) error

// internal/appdeploy
type ClusterSupport struct{ Mode string; MinSize, MaxSize int; WorkerPorts bool }  // Spec.Cluster *ClusterSupport
type ClusterCreateRequest struct{ UserID int64; ClusterID string; AppID int64; VersionID *int64; Env map[string]string; IP string }
type ClusterRedeployRequest struct{ UserID int64; ClusterDeploymentID string; VersionID *int64; Env map[string]string; EnvSet bool; IP string }
type ClusterDeployment struct{ ID, ClusterID string; UserID, AppID, AppVersionID int64; State, Status, Health, Phase string; Progress int; Members []ClusterMemberDeployment; CreatedAt, UpdatedAt time.Time }
func (s *Service) CreateCluster / GetClusterDeployment / LiveClusterDeployment / RedeployCluster / StopCluster / StartCluster / RemoveCluster

// internal/llm
// RentalOps 增 PlaceSystemCluster / ReleaseSystemCluster；AppOps 增 CreateCluster / GetClusterDeployment / RedeployCluster
```

## 13. 配置与常量
| 项 | 值 | 说明 |
|---|---|---|
| `SPARK_CLUSTER_SUBNET_POOL` | `10.210.0.0/16` | 必须是 /16，与 `spark0` 子网不重叠（`config.Load` 校验） |
| `clusterspec.IfName` | `spc0` | 容器内集群网卡 |
| `clusterspec.MemberOffset` | 11 | 成员 IP `.11 + rank` |
| `rental.ClusterSizes` | `{2, 4}` | M1 |
| `rental.ClusterAlignSlack` | 60s | M8（只用于控制台「到期不一致」提示与不变量） |
| `rental.ClusterFormTimeout` | 30m | 组建期从 `created_at` 起未就绪 → 整组中止（≥ `CreateTimeout` 20m） |
| `rental.ConfigureClusterCommandTimeout` | 2m | |
| `executor.configureClusterTimeout` | 2m | 与平台一致 |
| `clusternet` GID 等待 | 5s | |
| `appdeploy.MaxTimeoutMinutes` | 480 | M20 |
| `llm.GroupRebuildAfter` | 2m | M12 |
| `llm.GroupSuspendFailAfter` | 10m | M12 |
| `llm.ProvisionTimeoutFor(ver)` | `timeout_minutes + 30m` | M20 |
| vLLM / Ray | `--object-store-memory 2000000000`、`--port 6379`、`MASTER_PORT 29500` | §6.7 |

## 14. 并发与正确性
- **锁序（追加到 `internal/rental/service.go` 头注释）**：
  | 路径 | 顺序 |
  |---|---|
  | `PlaceBatch` 集群分支 | idem 咨询锁 → machines(NO KEY UPDATE, SKIP LOCKED，按 fabric 逐个 savepoint) → `cluster-alloc` 咨询锁 → 插 `instance_clusters` → 实例 / lease → orders → wallet → agent_commands |
  | `PlaceSystemCluster` | machines(NO KEY UPDATE，按 ID 升序等待) → `cluster-alloc` → 插集群 → 实例 / lease → agent_commands |
  | 所有「多成员」路径（`SweepClusters`、`RenewCluster`、`ReleaseCluster`、`AdminExtendCluster`、`AdminResyncCluster`、`AutoRenewClustersDue`、`ReleaseSystemCluster`、成员 `AdminMigrate` / `RefundRelease`、appdeploy 集群操作） | **集群行 FOR UPDATE → 成员实例（ID 升序）** → 其后与原路径相同 |
  | 单实例路径（`HandleCommandResult`、`ExpireDue`、`Suspend/Resume`、`refreshLease`、`beginWipe`、用户访问设置…） | **不读写 `instance_clusters` 行**（只读 `instances.cluster_id` 列），因此不会与「集群 → 实例」形成环 |
- llm：Controller 对 rental / appdeploy 的调用仍在事务外；`mutateNode` 转终态时同事务 `UPDATE llm_node_members SET live = false`。
- 幂等：`ConfigureCluster` 带 `version`，Agent 只取不小于本地的；集群续费键 `key#<rank>`；`CreateCluster` 由 `app_cluster_deployments_live_uniq` 兜底并发。
- 时钟一律 `clock.Clock`。

## 15. 迁移、兼容与发布顺序
1. **先发 Agent**（新字段对旧平台无害：旧平台不发 `cluster`、不读 `net`），逐台升级；后台看到 `agent_features` 含 Cluster 位、网卡表有数据。
2. 再发 platform（迁移 00026）；`platform admin app import deploy/apps` 导入 `vllm-node 1.1.0`、`vllm-cluster 1.0.0` 并在后台发布。
3. 后台建 fabric、给机器分配 fabric / 集群网卡；需要 RDMA 的机器确认 CX-7 口无 IP。
4. 回滚：Down 在有活跃集群 / 组节点 / 集群部署时拒绝；先释放集群、把多机模型副本调 0。Agent 回滚到旧版前须没有集群实例（旧 Agent 不清理 `spc0` 路由与 `SPARK-CLU`，残留规则只会丢弃流量）。

## 16. 测试与验收

### 16.1 单元（Go，嵌入式 PG，假时钟）
- 迁移 00026 往返；Down 的三条拒绝；`llm_models_parallel`、`llm_nodes_group`、`cluster_fabrics_*` 约束；`llm_node_members_machine_live_uniq`。
- `clusterspec`：子网 / 成员 IP / 网关计算、`Validate` 矩阵、`ResolveIface`（lan / cx7 自动 / 指定 / 无线拒绝 / 等于 lan 拒绝）、`RenderJSON / RenderEnv` 黄金文件（每行 `export`、单引号转义、RDMA 与非 RDMA）；`ResolveIface` 以 `lanIP` 找管理网卡（`lan_iface` 为空时）；`NetPrefix`。
- agent `clusternet/linux`：sysfs 夹具（221 实况：只有 `enP7s7` 与 `wlP9s9`；标准 Spark：4 个 mlx5 netdev、2 个 ACTIVE、uverbs 映射；GID 表）→ `Probe` 结果（`lanIP` 为空也正常）；routed / macvlan / vlan 首次 `Apply` 命令序列快照；**对已配好的 netns 再次 `Apply` 不产生任何 `ip link add / del`**；对端路由差量增删；`SPARK-CLU` 期望规则按 `iptables -S` 打印顺序、相同时不重建；跳转组在 `DOCKER-USER` / `INPUT` 链首按序共存（与 `SPARK-FWD / SPARK-IN` 不互抢）；`Check` 覆盖对端路由 / 规则 / VLAN / 父接口卫生（含 NM 重加的地址被清除）/ uverbs 映射；`Teardown` 只删本集群的路由、VLAN 引用计数；GID 查找（找到 / 超时）；容器内 `spc0` 关 IPv6。
- agent executor（fake runtime + fake clusternet）：create 带集群 → `runtime.Spec` 含 `CapDrop NET_RAW`、sysctl、RDMA 设备；Apply 失败 create 失败（消息带 `cluster-net: ` 前缀）并本地 Teardown + SyncRules；start / Recover 时差量 Apply；ConfigureCluster 版本比较 / 清空 / 运行中差量 Apply；巡检每轮回写状态、上轮 error 则重跑 Apply；wipe 调 Teardown + SyncRules；Report 的 `cluster` 字段；`init.sh` 含集群片段；状态文件往返（旧文件兼容）。
- agent client：Hello / StateReport 带 `net`；`ConfigureCluster` 指令转换。
- `fleet`：fabric CRUD 校验（kind / isolation / vlan / direct 满 / 删除占用）；`evalClusterNet` 判定矩阵（含 `lan_iface` 为空时按 `machines.lan_ip` 判定、`cluster_net` 网段）；`RecordHello / RecordNetInfo` 重算；`UpdateMachine` fabric 变更受活跃成员约束。
- `agentgw`：Create 载荷带 Spec → proto；`configure_cluster` 转换；Hello features / net 落库；StateReport net 落库、`InstanceState.cluster` 进 `last_state_report`。
- `rental`：集群下单（同 fabric 原子；第一个 fabric 机器被并发锁走时换下一个；无 fabric 够 N → `no_capacity`；`direct` 只给 2；旧 Agent / `cluster_ready=false` 机器不入选；RDMA 优先与最贴合排序）；子网 / VLAN 分配并发唯一；Create 载荷 Spec 正确（routed host_ip、macvlan vlan、rdma 判定）；`PlaceSystemCluster` 原子与条件；`SweepClusters` 开通期中止（成员失败 / 30 分钟未就绪；退款金额、机器不判故障、告警一次）/ 首次就绪对齐（之后暂停恢复不再对齐）/ 系统集群也置 `ready_at` / 结束；`cluster-net:` 前缀的 Create 失败不判机器故障；集群下单拒绝 `AppID`；同 fabric 不同网段不组队；逐 fabric 重试时实例列表重置；成员守卫矩阵；`RenewCluster` 全成或全败与幂等；`AutoRenewClustersDue`（有成员暂停仍续全员）；提醒只发一条；`AdminMigrate` 成员同 fabric + 版本 + ConfigureCluster 下发；`RefundRelease` 成员；`configure_cluster` 结果与超时；`CheckClusterInvariants`；单机即时分配优先非 fabric 机器。
- `appdeploy`：`ValidateSpec` 的 `cluster` 规则；`CreateCluster` 校验矩阵与 N 条部署 / 角色；保留变量只对集群部署（存量部署 env 字节不变）；worker 不登记端口；聚合状态表驱动；整组 redeploy / stop / start / remove；成员接口 `app_cluster_managed`；`required` 应用单实例部署与下单预装（`PrepareInstall`）都被拒；换机后 `stale`。
- `llm`：模型校验（N、TP×PP、embedding、改 N 需无节点、版本支持集群）；`PlatformNodeEnv` N = 1 字节不变；对账先 N 大后 N 小、N = 1 优先非 fabric 机器；`provisionGroups` 选 fabric；`createGroupNode` 插入失败释放；组推进（等全员 running、建集群部署、认领、promote）；组 unhealthy（成员没了立即 failed、失联 10 分钟 failed、崩溃 2 分钟 Redeploy、3 次 failed）；排空 / 移除释放整组；孤儿识别成员；Prober 组观察；Router 排除成员暂停的组；后台操作；不变量 ⑧～⑩。
- 应用目录：`vllm-node 1.1.0`、`vllm-cluster 1.0.0` 过 `TestAppsDir`；两个脚本的 Ray 段一致；脚本 `. /.spark/cluster.env` 且 head 导出 `RAY_ADDRESS`、PP > 1 先检查 cupy；`sh -n`。

### 16.2 集成（Go，`internal/app`，进程内 fake Agent + fake clusternet + `sshtest` 假容器）
- **用户集群全链路**：fabric（lan）+ 4 台 fake 机器（features 含 Cluster、`NetInfo` 带 lan 网卡）→ `POST /bookings {cluster: true, count: 2}` → 两张订单、一个集群、两条 Create 的 Spec 互为对端 → fake Agent 完成 → `SweepClusters` 后集群 `ready`、到期对齐 → 控制台 `/clusters/{id}` 成员与 head_ip → 部署 `vllm-cluster`（`/hooks/apps/tick` 推进两条部署）→ 聚合 running、head 端口登记、worker 未登记 → `RenewCluster` 两单 → `ReleaseCluster` → 洗机完成后集群 `released`；`CheckClusterInvariants` 通过。
- **开通期中止**：fake Agent 让 rank1 的 Create 失败 → 下一轮清扫 rank0 洗机、两单全额退、告警 `cluster_failed` 一条。
- **成员换机**：后台对 rank1 `migrate` → 新实例同 fabric、rank 1、版本 2、rank0 收到 `ConfigureCluster(v2)`、集群应用 `stale` → 重新部署后 running。
- **Token 商城 2 台组**：池内 2 台同 fabric → 模型 `nodes_per_replica 2` online → tick：`PlaceSystemCluster` → 全员 running → `CreateCluster(vllm-node)` → 推进两条部署 → head 挂 `FakeVLLM` → ready → 网关请求成功并计费；fake Agent 让 rank1 实例 failed（模拟机器 faulty）→ 组 failed、两台释放、无空闲组 → `llm_pool_short`；补两台 → 新组 ready。成员部署 crashed → 2 分钟后整组 Redeploy。`llm.CheckInvariants` 通过。

### 16.3 端到端（Playwright）
钩子：`/hooks/machines` 增 `fabric_id`、`net`（`lan` / `cx7` / `cx7-rdma` 预设，决定 fake Agent 的 `NetInfo`）与 `old_agent`（features 不含 Cluster）；`/hooks/agent` 已能完成 `configure_cluster`；集群应用推进复用 `/hooks/apps/tick`。覆盖表新增：
- `C-CLU-01` 市场页集群选项、网络类型与可开数量；`C-CLU-02` 下单 2 台集群（总价 = 2 × 单价）并进入集群页；`C-CLU-03` 实例列表集群折叠与状态；`C-CLU-04` 集群详情成员 / 集群 IP / head 地址 / 连接说明；`C-CLU-05` 成员实例详情横幅，续费 / 释放 / 自动续费置灰并引导；`C-CLU-06` 集群续费（N 单）与自动续费开关；`C-CLU-07` 释放集群二次确认；`C-CLU-08` 部署集群应用（表单、TP 默认、成员角色进度、聚合状态）；`C-CLU-09` 集群应用重新部署 / 停止 / 卸载与成员页只读；`C-CLU-10` 无可用集群时禁用与「暂不支持预订」说明。
- `A-FAB-01` 集群网络新建 / 编辑（ops）与 finance 只读；`A-FAB-02` 删除占用与直连满 2 台；`A-MCH-11` 机器详情网卡表、fabric / 集群网卡选择与就绪判定；`A-MCH-12` 旧 Agent 机器显示「Agent 不支持集群」；`A-CLU-01` 集群列表筛选；`A-CLU-02` 集群详情成员 / 版本 / 重新下发；`A-CLU-03` 集群延长；`A-INS-17` 实例详情集群区块与成员换机提示；`A-LLM-10` 模型多机字段与 TP × PP 校验；`A-LLM-11` 机器池按 fabric 分组与可组副本数；`A-LLM-12` 节点组成员展开与整组操作。
- 现有用例保持全绿；`menu.spec.ts` 加两个菜单。

### 16.4 真机验收清单（全部通过才算完成；记录写 `docs/superpowers/acceptance-plan13.md`）
平台跑在开发 Mac（配置同计划 9 验收）；需要 root 的命令由协调人经 `sudo -S` 执行，密码不落盘；大文件（模型、镜像）只在 DGX 上下载 / 传输（镜像 `docker save | ssh <dgx> docker load` 走内网，不从开发 Mac 中转大文件、不访问 Docker Hub）；模型走 ModelScope。

**负责人准备清单（按检查点逐步到位即可）**
1. 机器：CP-1 / CP-2 需 2 台、CP-3 需 4 台 DGX Spark（标准版带 CX-7）；系统与 221 一致（Ubuntu 24.04 / DGX OS、Docker 29.x、`nvidia-ctk cdi generate` 已执行）；同一机房内网、互相能 ping 通 `lan_ip`；提供序列号 / `lan_ip` / 主机名，以及协调人的 sudo 方式（同 221）。
2. 每台装新版 `spark-agent`（协调人执行 `install.sh`，后台生成接入令牌）；每台 `docker load` 镜像 `spark/llm-serving:25.09`（协调人从 221 经内网传）。
3. 接线：CP-1 两台在**同一 IP 网段**、同一二层（优先有线；WiFi 也可但很慢，且 AP 不能开客户端隔离 / IP 源地址防护——这类功能会丢弃源地址为 `10.210.x` 的转发包；办公交换机的 DHCP snooping / IP Source Guard 同理需关闭）；CP-2 一根 QSFP 直连线（DAC 或 AOC，两台接同一编号的口）；CP-3 一台支持 VLAN trunk 的 QSFP 交换机（机器端口设 trunk，放行 VLAN 100～199，**不配 native VLAN**，MTU 9216），没有交换机时 CP-3 退化为 `lan` fabric 上的 4 台逻辑验收、RDMA 4 台列为遗留。
4. CX-7 口不要配 IP：协调人用 `install.sh --cluster-iface <口>` 把它设为 NetworkManager unmanaged（Agent 也会持续清除其地址）；提供每台的 `ip -br link`、`rdma link`、`ls /sys/class/infiniband` 输出（协调人也可自行采集）。
5. 后台：登记机器（商品 spark-1）、结束维护；fabric 由协调人代建，负责人确认命名（建议「机房 A 以太网」「Spark 直连 1」「交换机 A」）。
6. 磁盘：每台数据盘剩余 ≥ 验收模型大小 + 100GB（CP-3 大模型每台约 250～400GB）。
7. 时间：CP-3 每台各下载数百 GB，预留半天；期间这些机器不对外出租。
8. 账号：一个已实名测试用户（余额 ≥ ¥500，mock 充值）、后台 super / ops 账号与 TOTP。

**CP-0 单机回归（221，无需新硬件，Agent 与平台完成后立即做）**
1. 新 Agent：`spark-agent selfcheck` 列出 `enP7s7`（down）、`wlP9s9`（wireless）、无 RDMA，提示无 CX-7；后台机器详情网卡表一致、能力位含 Cluster；`systemctl restart spark-agent` 之后 `cluster_ready` 判定不翻转（Hello 在 AgentConfig 之前发出，平台按 `machines.lan_ip` 认管理网卡）。
2. 镜像前置：`docker run --rm spark/llm-serving:25.09 python3 -c "import cupy, ray.dag"` 结果记入验收（失败 → 流水线并行 `PP > 1` 不可用，CP-1 第 6 项只测 TP；是否在 DGX 上给镜像补 `ray[cgraph]` 由协调人决定并记 followups）；同镜像里 `which ping ibv_devinfo` 记录有无。
3. 普通实例下单 / 开通 / SSH / Web / 释放 / 洗机全流程照常；`iptables -S` 里 `SPARK-FWD / SPARK-IN` 内容不变，`DOCKER-USER` 前 3 条与 `INPUT` 前 2 条为跳转组、`SPARK-CLU` 只有 `-j DROP`（无集群）；连续 3 分钟内 Agent 日志没有 `iptables chain rebuilt`（规则不反复重建）。
4. Token 商城 N = 1 节点（Qwen3-4B）照常 ready、调用、计费（回归计划 9 §16.4 第 3～5 项抽查）。
5. 单台机器建 `lan` fabric → 市场页「集群 2 台」显示不可用；后台把 221 的 fabric 改为 `cx7` → 判定「未找到 CX-7 网卡」。

**CP-1 两台以太网（`lan` fabric）**
1. 建 fabric「测试以太网」（lan），两台加入 → 都「就绪」。
2. 控制台下单「集群 2 台」1 天 → 两实例 running、集群「就绪」、钱包两条扣款 = 2 × 单价、两实例到期时刻相同。
3. 进 rank0：`cat /etc/spark-cluster.json`、`env | grep -E 'SPARK_CLUSTER|NCCL|MASTER'`（登录 shell）、`ip -br addr show spc0` = `10.210.x.11/32`；连通性用 curl / python（镜像未必有 ping，去掉 NET_RAW 后 ping 也可能 EPERM）：rank1 上 `python3 -m http.server 34567 --bind 10.210.x.12`，rank0 `curl -m3 10.210.x.12:34567` 通（任意端口）；`bash -lc 'python3 -c "import os; print(os.environ[\"VLLM_HOST_IP\"], os.environ[\"NCCL_SOCKET_IFNAME\"])"'` 在子进程里能读到（`export` 生效）。
4. 隔离：rank0 容器 `curl -m3 <rank1 宿主机 lan_ip>:22` 不通、`curl -m3 10.210.x.1:22`（网关 = 本机宿主）不通；`python3 -c 'import socket; socket.socket(socket.AF_PACKET, socket.SOCK_RAW)'` 报权限错误（NET_RAW 已去）；宿主机 `iptables -S SPARK-CLU` 与 `ip route | grep 10.210` 符合 §3.3；开一个普通单机实例（或第三台机器）`curl -m3 10.210.x.11:34567` 不通。
5. NCCL：两成员 `torchrun --nnodes 2 --nproc-per-node 1 --node-rank $NODE_RANK --master-addr $MASTER_ADDR --master-port 29500 nccl_allreduce.py`（脚本随验收记录提交）→ 成功并记录 busbw；`NCCL_DEBUG=INFO` 显示 `NET/Socket` 与 `spc0`；让它长跑（`--iters` 足够大），期间对两台各 `sudo systemctl restart spark-agent` → 测试不中断、宿主机 `ip link` 里 `sch*` 的 ifindex 不变（Apply 差量、无 del / add）。
6. 集群应用：部署 `vllm-cluster`（`Qwen/Qwen3-8B`，TP = 2）→ 两条部署 running、head 端口私有地址 `/v1/models` 与一次 chat 成功；`ray status` 显示 2 节点 2 GPU；vLLM 日志里 Ray driver 的节点 IP 是 `10.210.x.11`（不是 `10.200.0.x`）；CP-0 第 2 项 cupy 可用时再以 `PP = 2, TP = 1` 重新部署一次成功；记录吞吐（tokens/s）。
7. 故障：rank1 宿主机 `sudo systemctl stop spark-agent` 5 分钟 → rank1 暂停、集群「降级」；恢复后 `ip addr show spc0` 自动恢复（巡检 / 恢复路径）、集群「就绪」；集群应用 failed 时「重新部署」成功。
8. 续费 / 自动续费 / 释放：集群续费 1 天 → 两张续费单、两实例到期同时后移；对单个成员调用续费接口 → 409 `cluster_member`；释放集群 → 两台洗机、机器 idle、宿主机 `ip route | grep 10.210` 为空、`SPARK-CLU` 只剩 DROP。
9. Token 商城：两台移入池（同 fabric）→ 模型 `nodes_per_replica 2`（`Qwen/Qwen3-8B`，TP 2）上线 → 节点组 ready（成员 2）→ 网关非流式 / 流式调用成功、计费正确；在 worker 容器里杀 raylet → 组 unhealthy、503 → 2 分钟后整组重新部署 → ready；worker 宿主机停 Agent 超过 10 分钟 → 组 failed、两台释放、`llm_pool_short`；恢复 Agent → 洗机完成回池 → 新组 ready；`llm.CheckInvariants`、`rental.CheckClusterInvariants` 通过。

**CP-2 两台 RDMA 直连（`cx7` + `direct` + `rdma`）**
1. 后台网卡表：直连口 `rdma_active = true`、有 `uverbs`；建 fabric（cx7 / direct / rdma），两台加入并选该口 → 就绪且 RDMA。
2. 下单「集群 2 台」→ 市场页显示「RDMA」→ running；`/etc/spark-cluster.json` 的 `network = rdma`、`rdma.hca / gid_index`；容器内 `ls /dev/infiniband` 有 `rdma_cm` 与 `uverbsN`；`ibv_devinfo` 显示 `PORT_ACTIVE`（镜像缺 rdma-core 用户态 → 记录并在 `deploy/image` 补 `rdma-core ibverbs-utils` 后重建镜像重测）；宿主机 `cat /sys/class/infiniband/<hca>/ports/1/gids/<idx>` 等于 `::ffff:<spc0 IP>`、类型 RoCE v2。
3. NCCL all-reduce：`NCCL_DEBUG=INFO` 显示 `NET/IB … RoCE`（而非 Socket），busbw 明显高于 CP-1（记录数值）。
4. 隔离：父接口宿主机 `ip addr show <iface>` 无 IPv4 / IPv6；手工 `sudo ip addr add 169.254.9.9/16 dev <iface>` 模拟 NM 重加地址 → 一分钟内被巡检清除、告警 `cluster_net_error` 一条；`nmcli device status` 显示该口 unmanaged；容器访问不到两台宿主机（lan_ip 走 spark0 被拒，CX-7 无地址）；容器内只看得到 `spc0` 的 GID（`ibv_devinfo -v` / `show_gids`）；raw socket 失败。
4b. 重启一台宿主机 → 实例恢复后 `/sys/class/infiniband_verbs/uverbsN/ibdev` 仍对应 create 时的 HCA（若编号变化，状态显示「RDMA 设备编号变化」、NCCL 退回 TCP 仍可运行——记录实际行为）。
5. `vllm-cluster`（`Qwen/Qwen3-32B` 或更大，TP 2）成功并对比 CP-1 吞吐；Token 商城 2 台组同样走 RDMA（vLLM 日志 NCCL IB）。
6. （可选）拔掉 QSFP → 下一份 StateReport `rdma_active=false`、集群「降级」/ 网络异常告警 → 插回 → 重新部署恢复。

**CP-3 四台（交换机 `cx7 + vlan`；无交换机时 `lan`）**
1. fabric（VLAN 100～199）四台加入；同时下两个「集群 2 台」→ 两组 VLAN 不同（宿主机 `ip -d link show scv1xx`）、组内互通、**组间不通**；释放后下「集群 4 台」→ 四台互通。
2. 4 台才能装下的模型：建议 `Qwen/Qwen3-235B-A22B-Instruct-2507-FP8`（约 236GB，2 台装不下）TP 4（或 TP 2 × PP 2）；负责人点名的 DeepSeek 用 4-bit 量化版（约 350～400GB）作为加分项，视 vLLM 0.10.1 在 GB10 上的支持情况；用 `vllm-cluster` 部署成功、chat 正确，记录下载 / 加载时间、首 token 延迟、吞吐。
3. Token 商城 `nodes_per_replica 4` 同模型 → 组 ready → 网关调用；停一个成员 → 整组 unhealthy → 失联 10 分钟后 failed → 释放。
4. 收尾：告警只有预期项（`machine_offline`、`llm_node_unhealthy`、`cluster_*` 预期项）；两个不变量函数通过；`make check` 全绿；`acceptance-plan13.md` 与 `followups-plan13.md`。

## 17. 范围与遗留
**本计划做**：§2～§16。

**明确不做（遗留清单，写入 `followups-plan13.md`）**
- 集群预订、指定机器组集群、成员自动换机、集群扩缩容（2 → 4）。
- 集群 / RDMA 溢价定价、按网络类型分商品。
- SR-IOV / RDMA exclusive 模式、多轨（CX-7 两个 PCIe 半口聚合 / 两个 QSFP 口同时用）、GPUDirect 调优、PFC / ECN 无损网络配置。
- 4 台直连环形拓扑（无交换机的 4 台 RDMA）。
- 集群内带宽限速 / 计量；`lan` 模式的内网防注入加固（MAC 绑定 / IPsec）。
- 模型文件在成员间分发 / 共享缓存（洗机保留模型缓存同属计划 7 遗留）。
- 容器间 SSH 互信、MPI 一键环境。
- AI 管家的集群工具；集群级 Prometheus 指标。
- vLLM 升级到支持 `--nnodes` 的版本后改用 mp 多机（去掉 Ray）。
- 镜像补 `ray[cgraph]`（cupy）以默认支持流水线并行 `PP > 1`（视 CP-0 结果）。
- 成员失联暂停造成的集群内到期差的自动补偿（现为不补、只提示）。
- `lan` fabric 跨网段（经路由器）的 routed 模式。

## 18. 任务拆分（实施计划 `docs/superpowers/plans/2026-10-26-plan13-multinode.md`）
1 基础（迁移、`clusterspec`、proto、错误码、共享类型）→ 并行组 G1：2 Agent 宿主机网络实现、3 Agent 执行器集群支持、5 fleet / agentgw、6 rental 集群分配、7 appdeploy 集群部署、8 应用包、9 llm 多机节点组 → G2：4 Agent 装配与自检、10 rental 集群生命周期 → G3：11 控制台 API、12 后台 API → 13 装配 + 钩子 + 集成测试 → G4：14 控制台页面、15 后台页面 → 16 Playwright → 17 真机 CP-0 → 18 CP-1 → 19 CP-2 → 20 CP-3 + 收尾。
