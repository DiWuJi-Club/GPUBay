# Spark Cloud — 计划 10：共享机（一台 Spark 分给多位用户）设计文档

- 日期：2026-10-24
- 状态：**PoC no-go，共享机商品搁置**（2026-10-25，裁定 S13 / S14）；负责人已授权设计方裁定、不逐项询问；代负责人作出的裁定集中列在 §1.3，可事后推翻
- 上位文档：`2026-10-14-spark-cloud-mvp-design.md`（下称「总设计」）§4、路线图第 4 项「共享容器（k3s + HAMi，需 PoC）」、附录竞品调研；`2026-10-18-plan2-agent-design.md`（下称「计划 2」）§3、§4；`2026-10-18-plan3-access-design.md`（下称「计划 3」）§5.3；`2026-10-19-plan6-booking-design.md`（下称「计划 6」）§1.1、§4
- 范围：**先做真机 PoC 决定技术路线**（§3，含 go / no-go 标准与失败后的备选方案）；通过后实现「共享机」商品：一台 DGX Spark 同时出租给 2 或 4 位用户，每位用户得到一份内存（含 GPU 可用内存）/ CPU / 数据盘配额与共享的 GPU 算力，价格低于整机；共享实例与整机实例在下单、预订、续费、退款、接入层、后台处置上行为一致；Agent 支持一台机器多个容器；控制台与后台页面；测试与真机验收。
- 不在范围：见 §15。

## 0. 现状与约束（实现时不必再查）

**机器事实（计划 2 勘查与验收，192.168.1.221 `spark-3479`）**
- GB10，`MemTotal = 130660974592` 字节（127598608 kB，计划 10 PoC 实测）（约 121.7 GiB），**CPU 与 GPU 共用一池统一内存**；`nvidia-smi` 的显存列为 `[N/A]`（NVIDIA FAQ：只有独立显存时才报显存；NVML 的内存查询返回 `NOT_SUPPORTED`）。20 核 Arm（10×X925 + 10×A725）。
- Ubuntu 24.04，Docker 29.6.2（GPU 走 CDI，`--gpus all`），cgroup v2，驱动 580.x / **CUDA 13.0**，arm64。单块 3.7 TB NVMe，根分区 ext4，overlay2 可写层无法限额。
- 机器上 Docker Hub 不可达；nvcr.io、docker.m.daocloud.io、ports.ubuntu.com 可达。**没有 BMC**，机器重启靠 PDU。

**代码事实（`main` 上计划 1～6 已合并；计划 7、8 在 `../spark-cloud-p7`、`../spark-cloud-p8` 未合并；分支 `feat/plan10-hami-shared`）**
- 容器（`internal/agent/runtime/docker/docker.go`）**已经是非 privileged**：Docker 默认能力 + `IPC_LOCK / SYS_NICE / SYS_PTRACE`，`--gpus all`，`--memory = --memory-swap = 机器内存 − 8GiB`（`executor.HostReserveBytes`），`--shm-size = 内存一半`，`--pids-limit 32768`，`--oom-score-adj 500`，`--network spark0`，`--dns`，`--restart no`，`-p <lan_ip>:<ssh_port>:22/tcp`，`--entrypoint /.spark/init.sh`，`/root/data` bind 到 loop 数据盘。**任务书里的「整机 privileged 容器」与代码不符，本设计以代码为准。** `runtime.Spec` 已有 `MemoryBytes / ShmBytes / SSHPort`，没有 CPU 字段；`validateSpec` 拒绝空 `LanIP` 与 0 内存。
- 计划 2 §4 明文假设**每台机器同一时间只有一个实例**：`executor.DataMount()` 取 `insts[0]`（指标 `data_disk_used` 只有一个值）；SSH 端口 `2222` 由 `agentgw.Config.SSHPort`（`SPARK_UPSTREAM_SSH_PORT`）**对所有实例统一下发**（`agentgw/convert.go`），上游 SSH 连接池 `upstream.Target{InstanceID, LanIP, HostPubkey}` 与 `upstream.Config.Port` 也是全局一个端口（`internal/access/upstream/pool.go`、`sshgw`、`ingress`）。其余部分（`state.json` 多实例记录、`Report` 遍历容器、`Recover` 遍历实例、按 veth 限速、`SPARK-FWD / SPARK-IN` 按网桥而非按容器）天然支持多容器。`spark0` 网桥 `enable_icc=false`，同一机器上的容器之间**本来就不能互通**。
- `CreateInstance` 指令字段：`image_ref, upstream_pubkey, bandwidth_mbps, ssh_port, data_size_gib, host_key_pem`（proto 字段号 1～6）。`Heartbeat.Metrics` 是机器级指标（字段号 1～9）。计划 7 在 `StateReport` 加了 `repeated string images = 2`，不碰 `CreateInstance` 与 `Heartbeat`。
- 数据盘：每实例一个稀疏镜像 `data/<id>.img`，大小由 `AgentConfig.data_size_gib`（`SPARK_AGENT_DATA_SIZE_GIB`，默认 2048）统一决定，`CreateInstance.data_size_gib` 非零时优先（`executor.create`）。创建前检查根分区剩余 ≥ 数据盘 + 100 GiB。
- 库存：`machines.product_id NOT NULL`（一台机器只卖一种商品），`state ∈ {idle, allocated, maintenance, faulty}`；`fleet.MarkAllocated`（`idle AND agent_online → allocated`）与 `fleet.Release`（`allocated → idle`）由 rental 调用；`fleet.IdleCount` 只被机组页用；市场页空闲数走 `rental.AvailableNow`（lease-aware 计数）。后台没有修改机器 `product_id` 的接口（计划 4 遗留）。
- 占用时段：`machine_leases (machine_id, instance_id, period, status)`，`EXCLUDE USING gist (machine_id WITH =, period WITH &&) WHERE status IN ('reserved','active')`；`instances_machine_active_uniq ON instances (machine_id) WHERE state IN ('creating','running','stopped','wiping')`。分配统一走 `rental.pickMachines`（`pickWhereSQL`：商品 ∩ 机组 ∩ 状态 ∩ 无重叠时段，`FOR NO KEY UPDATE OF m SKIP LOCKED`，savepoint 逐台插实例 + lease，排他冲突换下一台）；`refreshLease` 在每次状态变化后按 `occupiedUntil` 更新上界并封顶于同机下一条 reserved 的下界（`updateLeaseCapped` 的 `cap` 子查询按 `machine_id`）；`DailyOccupancy` 按 lease 秒数 / 机器总数算出租率。
- 商品：`products (code, name, spec jsonb, status)`，`prices (product_id, duration)`；初始只有 `spark-1`「DGX Spark 整机」¥69 / ¥414 / ¥1599；后台只能改价、改镜像，不能建商品。`catalog.Product.Spec` 是 `map[string]any`，前端 `ProductSpec {gpu, memory_gb, disk_tb, cpu}`。
- 实例视图（`api/instances.go`）：`ssh_host / ssh_port` 是**网关**地址（用户永远经 `ssh.<domain>` 登录），`machine_label`、`product_name`。e2e 钩子 `/hooks/machines` 固定造 `spark-1` 机器、`lan_ip = 127.0.0.1`（拨进程内假 sshd）。
- 迁移编号：00020 计划 7、00021 计划 8、00022 计划 9；**本计划 `00023_shared.sql`**（带 `+goose Down`）。
- 真机：只有一台 Spark，**当前正用于计划 7 验收，本设计阶段不上机**；PoC 是本计划的第一批任务，等机器空出来后执行。

**竞品（器灵算力站）**：机器 102 台里有「HAMi 共享 24 台」，与 DGX Spark 整机并列售卖，强调「共享显存 / 算力、价格低」；整机 ¥69 / 天。

**外部事实（2026-10-24 查证，决定技术路线）**
- HAMi-core（`libvgpu.so`）靠 `LD_PRELOAD` 挂钩 CUDA 驱动 API，环境变量 `CUDA_DEVICE_MEMORY_LIMIT / CUDA_DEVICE_SM_LIMIT` 限额，可以不依赖 Kubernetes 直接在 Docker 里用（README 示例 `-e CUDA_DEVICE_MEMORY_LIMIT=2g` + 挂 `libvgpu.so`）。但：① 第三方构建指南明确「不支持最新 CUDA 13.0，建议降到 12.8」，而 GB10 只有 CUDA 13 驱动；② 它的显存统计与算力节流依赖 NVML（`nvmlDeviceGetMemoryInfo`、进程利用率），GB10 上 NVML 内存查询 `NOT_SUPPORTED`；③ **`LD_PRELOAD` 对容器内 root 没有强制力**（`unset LD_PRELOAD`、静态链接、改 `/etc/ld.so.preload` 都能绕过），HAMi 在 k8s 里也只是「合作式」限额；④ 没有 arm64 预构建产物。
- CUDA MPS：`set_default_device_pinned_mem_limit` 由控制守护进程设定、客户端只能调低不能调高，`set_default_active_thread_percentage` 同理 → **服务端强制**。但：MPS 客户端必须与服务端同 UID、共享 `/tmp/nvidia-mps` 管道目录与（Volta 前）IPC 命名空间；一个客户端的致命 GPU 错误会连累同一 MPS 服务下的其他客户端（NVIDIA 文档「fatal exception」语义）；「pinned device memory」在统一内存上的语义未定义；MPS 在 GB10 上是否可用无公开资料。
- cgroup v2 `memory.max`：Docker `--memory` 直接落到容器 cgroup，内核强制、root 不能绕过（容器非 privileged、无 `CAP_SYS_ADMIN` 对宿主 cgroupfs 的写权）。**关键未知**：GB10 上 `cudaMalloc` 分配的是统一内存里的页，这些页是否计入分配进程的 cgroup（dGPU 上显存不计入；Jetson 一类 iGPU 上 nvmap 分配历史上**不**计入）。这正是 PoC 要测的第一件事。

## 1. 架构与核心决策

```
 控制台：市场页「共享机 1/4」「共享机 1/2」卡片 → RentDialog（与整机同一流程）        后台：机器页「商品 / 槽位」、价格页
                              │                                                        │
                              ▼                                                        ▼
┌──────────────────── rental（占用的唯一真相仍是 machine_leases，新增「槽位」维度）────────────────────┐
│  products.share_slots ∈ {1, 2, 4}：1 = 整机；机器卖哪种商品 = machines.product_id（不混卖）       │
│  machine_leases.slot / instances.slot：整机 0；共享 1..share_slots                               │
│  排他约束改为 (machine_id, slot, period)；pickMachines 改为挑 (machine, slot) 对                  │
│  下单 / 预订 / 续费 / 退款 / 改派 / 换机 / 封顶 / 清扫：全部按 (machine, slot) 复用现有逻辑          │
│  machines.state：共享机有 ≥1 个未结束实例时 allocated，全空 idle（MarkAllocated / Release 改语义）  │
└──────────────────────────────────────────────────────────────────────────────────────────────┘
                              │ CreateInstance 新增 share_slots、ssh_port = 2222 + slot
                              ▼
 spark-agent：按 share_slots 等分 (MemTotal − 8GiB) / nproc / 数据盘；--memory --cpus --shm-size --pids-limit；
              GPU 仍 --gpus all（算力由驱动时间分片，内存由 cgroup 兜底——PoC 验证）；每容器各自 veth 限速、
              同桥 enable_icc=false 互不可见；心跳新增每实例 memory.current / cpu 用量
```

### 1.1 技术路线评估与选择

三个候选按「限额是否**强制**（容器内 root 绕不过）」「在 GB10 / CUDA 13 / arm64 上可用的证据」「实现与运维代价」评估：

| 方案 | 内存限额 | 算力限额 | 强制力 | GB10 可用证据 | 代价与风险 | 结论 |
|---|---|---|---|---|---|---|
| A. HAMi-core `libvgpu.so` + Docker（无 k8s） | 有（挂钩 cuMemAlloc 计数） | 有（SM 利用率节流） | **无**：`LD_PRELOAD` 由租户进程自己加载，root 可去掉 | 负：不支持 CUDA 13；依赖 NVML 内存查询（GB10 `NOT_SUPPORTED`）；无 arm64 产物 | 要自己维护 CUDA 13 / arm64 补丁；每个镜像都要带 `.so` 与 `ld.so.preload`；限额可被绕过意味着**定价依据不成立** | 不作主路线；PoC 里只做 2 小时时间盒的「能否构建、能否拦到 cuMemAlloc」记录（§3.4） |
| B. CUDA MPS | 有（pinned mem limit，服务端强制） | 有（active thread %，服务端强制） | 中：服务端设定租户不能调高；但共享一个 MPS 服务 → 一个租户的致命错误连累全机，租户能看到 / 影响其他客户端 | 未知：统一内存上 pinned limit 语义未定义；需要同 UID、共享管道目录与 IPC | 新增宿主机守护进程（nvidia-cuda-mps-control）与生命周期管理；容器要挂 `/tmp/nvidia-mps` 并共享 IPC（计划 2 刚去掉 `--ipc=host`）；故障域扩大到整机 | **备选**：A 路线不成立时做第二轮 PoC（§3.5） |
| C. 驱动默认时间分片 + cgroup v2 限额 | 有（`memory.max`，**内核强制**，**前提是 GPU 分配计入 cgroup**） | 无硬限（驱动按上下文轮转时间分片，大致公平；`--cpus` 限 CPU） | **强**：内核 cgroup，与现有 `--memory` 同一机制 | 待验证：GB10 统一内存下 `cudaMalloc` 是否计入 cgroup —— 这是唯一未知 | 几乎零新增组件：只是把现有容器参数按份数缩小并允许多容器；没有新的守护进程、没有协议之外的东西 | **主路线**，PoC 第一轮只验它 |

**裁定：选 C 为主路线**。理由：(1) 只有它的限额是租户绕不过的，共享机定价「按内存配额收费」才站得住；(2) 它是现有容器参数的推广，Agent 改动最小、故障域不变（容器之间只共享内核与 GPU 驱动，不共享任何用户态守护进程）；(3) GPU 算力不做硬限额，产品上明说「算力共享、不保证独占」，这与竞品 HAMi 共享机的实际交付（HAMi 的 SM 限额也只是合作式）一致。A、B 的硬伤都不是实现量问题而是「在 GB10 上根本不成立或不可强制」。

**C 的唯一前提由 PoC 判定**：GB10 上 CUDA 设备内存分配是否计入进程 cgroup（§3.2 P1）。计入 → 直接进入实现；不计入 → 走 §3.5 备选链（MPS 第二轮 PoC → 都不行则共享机搁置，本计划只保留「多容器 Agent」这部分基础设施）。

### 1.2 决策一览

| # | 决策 | 结论 | 理由 |
|---|---|---|---|
| 1 | 共享的粒度 | **份数固定**：`share_slots ∈ {1, 2, 4}`，整机 = 1；一台机器只卖一种商品（沿用 `machines.product_id`），不在同一台机器上混卖 1/2 与 1/4 | 固定份数让配额 = 等分，容量模型就是「槽位」；混卖要做装箱与碎片管理，收益小 |
| 2 | 占用模型 | **槽位 = `machine_leases.slot`**：排他约束从 `(machine_id, period)` 改为 `(machine_id, slot, period)`，整机 slot 0、共享 1..N；`pickMachines` 从「挑机器」改为「挑 (机器, 槽位)」 | 预订、改派、封顶、换机、出租率全部是 lease 上的逻辑，加一个维度后**一行代码路径都不分叉**；用数据库约束防超卖的做法不变 |
| 3 | 机器状态 | 共享机 `state`：有 ≥1 个未结束实例 → `allocated`，全空 → `idle`；`maintenance / faulty` 不变。候选谓词对共享商品放宽为 `state IN ('idle','allocated') AND agent_online` | `state` 继续表达「这台机器上有没有租户」，后台页面与现有告警 / 维护逻辑的语义不变；是否还有空槽由 lease 决定 |
| 4 | 配额如何算 | **Agent 按 `share_slots` 等分**：`memory = (MemTotal − 8GiB) / N`，`cpus = nproc / N`，`shm = memory / 2`，`pids = 32768 / N`，数据盘 = `AgentConfig.data_size_gib / N`（向下取整到 GiB）；平台只下发 N，不下发字节数 | 平台不需要知道每台机器的内存 / 核数；200 台机器配置一致时等分即正确；未来机型不同也不用改平台 |
| 5 | GPU | 共享容器仍 `--gpus all`；**不做算力硬限额**，产品文案「GPU 算力共享，不保证独占；GPU 可用内存受实例内存配额约束」 | 见 §1.1；时间分片由驱动完成，PoC 测公平性只作记录 |
| 6 | 共享容器的额外收紧 | 去掉 `SYS_NICE`（防止抬高自己优先级压制邻居）；`--pids-limit` 等分；`--oom-score-adj 500` 不变；`SYS_PTRACE / IPC_LOCK` 保留（pid 命名空间内只能 ptrace 自己的进程；memlock 受 `memory.max` 兜底） | 共享机的邻居是其他付费用户，CPU 调度优先级不能让租户自己改 |
| 7 | 网络 | 不改 iptables 链：`SPARK-FWD / SPARK-IN` 按 `spark0` 网桥生效，对每个容器一样；`enable_icc=false` 已禁止同机容器互通；限速按每个容器自己的 veth；SSH 端口 `2222 + slot`（整机 slot 0 仍 2222） | 现有隔离已经是「每容器」粒度，只需把端口从全局常量改成按实例 |
| 8 | 磁盘 | 数据盘按份等分（默认 2048 / N GiB）；可写层仍无限额（计划 2 已知风险）；共享机根分区 > 90% 的告警文案带机器序列号（已有） | 与整机一致；真正的可写层限额仍在遗留清单 |
| 9 | 商品与定价 | 新增两个商品：`spark-shared-2`「共享机 1/2」、`spark-shared-4`「共享机 1/4」，迁移里以 `status='inactive'` 落种子，**PoC 通过且验收后由 super 在后台启用**；价格见裁定 S1 | 商品不上架就不会出现在市场页；价格进 `prices` 表后台可改 |
| 10 | 预订与多台 | 共享实例支持预订、自动续费、一单多台（走同一 `PlaceBatch`，候选是 (机器, 槽位)），但**预订共享机时不允许指定机器**（只能选机组或不限） | 「机器日历」对共享机要画 N 条泳道，本计划不做；机组 / 不限的路径零改动 |
| 11 | 换机 / 改派 | `AdminMigrate`、预订改派都要求目标 (机器, 槽位) 的机器商品与实例商品相同（已有 `product_id` 匹配），共享实例只会落到共享机 | 配额由商品决定，换到整机会白送配额 |
| 12 | 后台切换机器商品 | 新增 `PUT /machines/{id}/product`（super，审计）：机器必须 `maintenance` 且没有 reserved / active lease；用于把整机转成共享机或反向 | 填补计划 4 遗留「不能改机器 product_id」中与本计划相关的一项；其余字段仍不改 |
| 13 | 指标与告警 | 心跳新增每实例 `memory.current / memory.max / cpu 使用`（Agent 读容器 cgroup）；平台对共享实例 `memory.current ≥ 95% memory.max` 持续 3 次心跳 → `warning` 告警 `instance_memory_pressure`（稳定文案）；用户在实例详情看到「内存用量」 | 共享机用户最常见的问题是「为什么我的程序被杀」；给用户和运维都看得见的数字 |
| 14 | Agent 协议 | `Hello` 新增 `uint32 features = 4`（能力位图，bit0 = 支持 share_slots）；`CreateInstance` 新增 `uint32 share_slots = 7`；`Heartbeat` 新增 `repeated InstanceMetrics instances = 2`。旧 Agent 收到 `share_slots > 1` 会忽略并按整机配额建容器 → 平台只把 `agent_features` 含该位的机器列为共享候选（裁定 S10） | 共享机必须由新 Agent 承载；用能力位而不是版本号比较；发布顺序仍是先 Agent 后 platform |
| 15 | PoC 的位置 | PoC 是本计划 Task 1～3，产出 `docs/superpowers/acceptance-plan10-poc.md` 与明确的 go / no-go；**no-go 时 Task 4 起的实现任务不执行**，改走 §3.5 | 先花一天验证唯一未知，再花两周写代码 |

### 1.3 代负责人裁定（负责人已授权，事后汇报）

| # | 裁定 | 理由 |
|---|---|---|
| S1 | **定价**（分；后台可改）：共享机 1/4 ¥25 / 天、¥150 / 周、¥579 / 月；共享机 1/2 ¥39 / 天、¥234 / 周、¥899 / 月；整机不变 ¥69 / ¥414 / ¥1599 | 四分之一机合计 ¥100 / 天、二分之一机合计 ¥78 / 天，都高于整机 ¥69：共享机对平台单机收益更高、对用户门槛更低；1/2 比两份 1/4 便宜，鼓励需要更多内存的用户选 1/2 而不是买两份 1/4 |
| S2 | 份数只有 2 与 4，**不做 8**：1/8 只有约 14 GiB 内存、2.5 核，跑不了 GB10 的典型负载（8B 模型 bf16 就要 16 GiB） | 规格太小会带来大量「跑不动」的客诉 |
| S3 | 共享容器**不限 GPU 算力**，文案明示；也不做「忙时降级」之类的调度 | 见 §1.1 第 5 条 |
| S4 | 数据盘按份等分：1/2 → 1024 GiB、1/4 → 512 GiB（`SPARK_AGENT_DATA_SIZE_GIB=2048` 时）；创建前的「根分区剩余 ≥ 数据盘 + 100 GiB」检查不变 | 3.7 TB 盘放 4 个 512 GiB 稀疏镜像 + 可写层仍有余量 |
| S5 | 共享实例的 SSH 端口 `2222 + slot`（2223～2226）；整机仍 2222；`SPARK_UPSTREAM_SSH_PORT` 改名含义为「基准端口」 | 一台机器多个 sshd 必须不同端口；基准可配，偏移固定 |
| S6 | 预订共享机不能指定机器、机器日历页对共享机显示「共享机不提供按机器日历」；机组日历的「可用台数」对共享机组按**空槽数**统计 | 见 §1.2 第 10 条 |
| S7 | 后台出租率（`DailyOccupancy`）：共享槽位的 lease 按 `1/share_slots` 加权，分母仍是机器数 | 一台四分之三满的共享机算 0.75 台，而不是 3 台 |
| S8 | 共享机**不单独做机组**，运营自行把共享机放进「共享机」机组（已有机组功能） | 机组是展示与改派范围，已够用 |
| S9 | `instance_memory_pressure` 只告警不干预；不自动杀进程、不自动扩配额 | 杀租户进程的只能是内核 OOM；平台不替用户做决定 |
| S10 | 旧 Agent 承载共享机的保护：Agent 在 `Hello` 里上报能力位图 `features`（`FeatureSharedSlots = 1`），网关写 `machines.agent_features`；共享商品的候选谓词要求 `(agent_features & 1) <> 0`。不用 `agent_version` 比较（它是 git 短哈希，无法比大小） | 用能力位比版本号可靠；也让「机器切成共享机但 Agent 没升级」在下单时就被排除，而不是开通失败退款 |
| S11 | PoC 期间用的 CUDA 测试程序放 `deploy/poc/shared/`（C + nvcc，在 `nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04` 里编译），**提交进仓库**，验收时复用 | 验收第 3、4 项要重跑同样的测试 |
| S12 | HAMi-core 的构建尝试限 2 小时时间盒、只记录不阻塞；MPS 只在 C 路线 no-go 时才做 | 不为「评估完整性」烧时间 |
| S13 | **PoC 判定（2026-10-25 实测，记录见 `acceptance-plan10.md`）：路线 C no-go、MPS no-go → 共享机搁置**。C：`--memory 32g` 容器里 `cudaMalloc` 分配并写满 48 GiB 成功，cgroup `memory.current` 只涨约 100 MiB（托管内存则计入 cgroup 并被 OOM 杀）。MPS：§3.5 字面各项都过了（单进程 27 GiB 处被拦、P2 96.8%、P3 连累但 <1 秒恢复、P5 通过），但 pinned limit 是**每个客户端进程**一份，同一容器两个进程合计 54 GiB；且默认计算模式下不设 `CUDA_MPS_PIPE_DIRECTORY` 就能完全绕过（需 `EXCLUSIVE_PROCESS` 才能堵）。按 §1.1「限额强制、租户绕不过」的第一标准判为 no-go | 「每位租户的内存配额」是定价与宿主机安全的前提；按进程计的限额对容器内 root 不成立，4 位租户各起 2 个进程即可让宿主机 OOM。负责人可推翻（例如接受「每容器只允许一个 GPU 进程」的运营约束），推翻时从计划 Task 4 继续 |
| S14 | 搁置后保留的范围（§3.5 第 2 条的落地）：协议三处新增（Hello.features、CreateInstance.share_slots、Heartbeat.instances）；Agent 按份配额能力（平台目前只发 share_slots = 1）、多实例指标（`Mounts()` / 每实例 cgroup）、SSH 端口冲突兜底、selfcheck；平台侧 `rental.Instance.SSHPort(base)`（暂恒为基准端口）、Create 指令显式带 `ssh_port / share_slots = 1`、`upstream.Target.Port`、`sshtest` 多端口、心跳每实例指标写入 `last_metrics.instances`。**不做**：迁移 00023（编号保留未用）、槽位、两个共享商品、`agent_features` 列与告警、后台 / 控制台页面 | 只做对未来（共享机复活、双机互联、维护容器）都有用、对现有整机行为零变化的部分 |

### 1.4 不做的替代方案（评审时不必再提）
- 「k3s + HAMi（总设计路线图原文）」：200 台机器引入 Kubernetes 只为共享 GPU，与计划 2 自研 Agent 的架构完全冲突；HAMi 在 k8s 里的限额同样是 `LD_PRELOAD` 合作式；放弃。
- 「一台机器混卖多种份数（装箱）」：见 §1.2 第 1 条。
- 「独立的 `shared_slots` 表而不动 `machine_leases`」：预订、封顶、改派、出租率要全部分叉；放弃。
- 「按 GPU 利用率计费 / 按量计费」：总设计明确只做预付包时长。
- 「HAMi-core 作为软限额叠加在 C 之上」：CUDA 13 不支持，强制力为零，维护成本高；列入遗留观察。

## 2. 数据模型（迁移 `00023_shared.sql`，带 `+goose Down`）

```sql
-- +goose Up
-- 计划 10：共享机。槽位进入 machine_leases / instances；商品带份数；机器记录 Agent 能力位。
ALTER TABLE products ADD COLUMN share_slots INT NOT NULL DEFAULT 1 CHECK (share_slots IN (1, 2, 4));

ALTER TABLE instances ADD COLUMN slot SMALLINT NOT NULL DEFAULT 0 CHECK (slot BETWEEN 0 AND 4);   -- 0 = 整机
DROP INDEX instances_machine_active_uniq;
CREATE UNIQUE INDEX instances_machine_slot_active_uniq ON instances (machine_id, slot)
    WHERE state IN ('creating', 'running', 'stopped', 'wiping');

ALTER TABLE machine_leases ADD COLUMN slot SMALLINT NOT NULL DEFAULT 0 CHECK (slot BETWEEN 0 AND 4);
ALTER TABLE machine_leases DROP CONSTRAINT machine_leases_machine_id_period_excl;   -- 实现时用 \d 核对实际约束名
ALTER TABLE machine_leases ADD CONSTRAINT machine_leases_machine_slot_period_excl
    EXCLUDE USING gist (machine_id WITH =, slot WITH =, period WITH &&) WHERE (status IN ('reserved', 'active'));
-- 封顶与候选查询按 (machine_id, slot) 走索引
DROP INDEX machine_leases_machine_period_idx;
CREATE INDEX machine_leases_machine_slot_period_idx ON machine_leases (machine_id, slot, lower(period)) WHERE status IN ('reserved', 'active');

ALTER TABLE machines ADD COLUMN agent_features INT NOT NULL DEFAULT 0;   -- Hello.features 位图；bit0 = 支持 share_slots

INSERT INTO products (code, name, spec, status, share_slots, created_at, updated_at) VALUES
  ('spark-shared-2', 'DGX Spark 共享机 1/2',
   '{"gpu": "NVIDIA GB10（算力共享）", "memory_gb": 56, "disk_tb": 1, "cpu": "10 核 Arm", "shared": true}', 'inactive', 2, now(), now()),
  ('spark-shared-4', 'DGX Spark 共享机 1/4',
   '{"gpu": "NVIDIA GB10（算力共享）", "memory_gb": 28, "disk_tb": 0.5, "cpu": "5 核 Arm", "shared": true}', 'inactive', 4, now(), now());
INSERT INTO prices (product_id, duration, amount, updated_at)
SELECT p.id, d.duration, d.amount, now() FROM products p,
  (VALUES ('spark-shared-2', '1d', 3900), ('spark-shared-2', '1w', 23400), ('spark-shared-2', '1m', 89900),
          ('spark-shared-4', '1d', 2500), ('spark-shared-4', '1w', 15000), ('spark-shared-4', '1m', 57900)) AS d (code, duration, amount)
WHERE p.code = d.code;

-- +goose Down
-- 有损：仍有 slot > 0 的未结束实例或 reserved/active lease 时拒绝回滚（请先在后台释放 / 取消共享实例）。
```

- `slot` 的取值由 `products.share_slots` 决定：整机商品只允许 0，共享商品 1..N；由 `rental` 在分配时保证，不加跨表 CHECK。
- `memory_gb` 写的是**实际配额**：`(121.7 − 8) / 2 ≈ 56.8`、`/ 4 ≈ 28.4`，向下取整展示。`disk_tb` 按 S4。
- Down 要先核查没有共享实例（`DO $$ ... RAISE EXCEPTION` 模式与 00019 相同），然后恢复旧排他约束与旧唯一索引、删列、删两个商品（`prices` 先删）。

## 3. 真机 PoC（Task 1～3，在 192.168.1.221 上，等计划 7 验收结束后）

### 3.1 准备
- `deploy/poc/shared/`：`alloc.cu`（参数：分配总量 GiB、步长、是否 `cudaMallocManaged`、是否触碰内存、持有秒数；每步打印累计量与 `cudaMemGetInfo`）、`burn.cu`（持续矩阵乘 N 秒，打印每秒完成的 GFLOP）、`fault.cu`（故意非法访存触发 `cudaErrorIllegalAddress`）、`Dockerfile`（`nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04` + nvcc 编译三个程序，产出 `spark/poc-shared:1`）、`run-poc.sh`（下面每一步对应一个子命令，输出写 `/tmp/poc/<step>.log`）。
- 容器统一用与 Agent 一致的参数手工 `docker run`（非 privileged、`--gpus all`、`--network spark0`、`--memory --memory-swap --cpus --shm-size --pids-limit --oom-score-adj 500 --cap-drop SYS_NICE`），**不经平台**，Agent 服务在 PoC 期间停掉（`systemctl stop spark-agent`，结束后恢复）。
- 需要 root 的步骤由协调人按计划 6 的 `sudo -S` 方式执行，密码不落盘。

### 3.2 第一轮：路线 C 的 go / no-go

| # | 实验 | 通过标准（go） | 失败含义 |
|---|---|---|---|
| P1 **GPU 分配计入 cgroup** | 容器 `--memory 32g`，运行 `alloc 48 1 --touch`（cudaMalloc 逐 GiB 分配并写入） | 在累计 ≤ 32 GiB（±2 GiB）处被终止：`cudaMalloc` 返回 `cudaErrorMemoryAllocation` **或**进程被 OOM-kill（`dmesg` 可见 `memory.max` 的 oom 记录）；宿主机与另一容器不受影响 | 若能分配到 48 GiB 成功 → GPU 分配不计入 cgroup → **路线 C no-go**，转 §3.5 |
| P1b 托管内存 | 同上用 `--managed` | 同 P1 | 同 P1（任一变体不计入即 no-go） |
| P2 **并发可用** | 两个容器各 `--memory 56g`，同时 `burn 60` | 两者都正常完成，`nvidia-smi` 看到两个进程；吞吐之和 ≥ 单容器独跑的 85% | 两者不能同时建立上下文 → no-go |
| P3 **故障隔离** | A 跑 `burn 120`，B 跑 `fault` | B 退出、A 继续到 120 秒且吞吐无长时间中断（> 5 秒的中断视为失败）；宿主机 `nvidia-smi` 仍正常 | A 被连累或驱动需重置 → no-go |
| P4 **公平性（记录，不判定）** | 两个容器同时 `burn 60` | 记录各自吞吐占比；期望每个在 35%～65% 区间 | 严重不公平只写进产品文案与遗留，不阻塞 |
| P5 **宿主机安全** | 两容器各 `--memory 56g` 同时 `alloc 60 1 --touch` | 两者都在配额处被限制；宿主机 `MemAvailable` 始终 > 6 GiB、Agent / sshd 不被 OOM；无内核 oops | 宿主机 OOM 或 GPU 驱动报错 → no-go |
| P6 **信息泄漏（记录）** | 容器内 `nvidia-smi`、`ps` | 记录能否看到邻居的 GPU 进程（PID / 进程名）；`ps` 看不到邻居（pid ns） | 只记录，写进文案「同机用户可见 GPU 进程列表」或在镜像规范里建议 |
| P7 **CPU 配额** | `--cpus 5` 跑 20 线程 `stress`（apt 装）60 秒 | `docker stats` CPU ≈ 500%；邻居容器的 `burn` 吞吐波动 < 15% | 不通过只记录（CPU 配额是 cgroup 标准功能，预期通过） |

**go**：P1、P1b、P2、P3、P5 全部通过。**no-go**：任一不通过。结果（含每条命令与原始输出摘要）写 `docs/superpowers/acceptance-plan10-poc.md`。

### 3.3 第一轮通过后额外记录（供实现与文案）
- `cudaMemGetInfo` 在容器内返回的 free / total 是什么（预计返回整机值，与配额无关 → 文案要写「程序看到的总内存不是你的配额」）。
- OOM 发生时的表现：`cudaMalloc` 报错还是进程被杀；写进帮助页。
- `memory.current` 在 GPU 分配后的读数是否随之增长（决定 §1.2 第 13 条的告警是否对 GPU 分配有效）。

### 3.4 HAMi-core 时间盒（2 小时，只记录）
在 `nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04` 里 `git clone` HAMi-core → `./build.sh` → 若能编出 `libvgpu.so`，用 `LD_PRELOAD` + `CUDA_DEVICE_MEMORY_LIMIT=8g` 跑 `alloc 16`，记录：是否能构建；是否拦截到分配（8 GiB 处报错）；`unset LD_PRELOAD` 后是否失效（预期是）。结论进 PoC 记录的「附录 A」。不改变路线。

### 3.5 备选链（第一轮 no-go 时）
1. **第二轮 PoC：MPS**（1 天）。宿主机起 `nvidia-cuda-mps-control -d`（`CUDA_MPS_PIPE_DIRECTORY=/var/lib/spark-agent/mps/pipe`），`set_default_device_pinned_mem_limit 0 28G`、`set_default_active_thread_percentage 25`；容器以 root、挂管道目录、`--ipc=shareable` 的 MPS 服务容器 / `--ipc=container:<mps>`。重跑 P1～P5。**go**：pinned limit 在统一内存上确实把 `cudaMalloc` 限制在 28 GiB、P2 / P5 通过、P3 **允许**「B 的致命错误使 A 的上下文失效」但宿主机与 MPS 守护进程在 30 秒内自动恢复（Agent 负责重启 MPS）。
   - 若 MPS go：实现路线改为「Agent 托管 MPS 守护进程（每台共享机一个，systemd 单元 `spark-mps`）+ 容器挂管道目录 + 共享 IPC 命名空间仅限 MPS 服务容器」；容器参数、槽位、商品、页面（§4～§12）**不变**；增加告警 `mps_restarted`；产品文案加「同机其他用户的程序崩溃可能导致你的 GPU 任务需要重启」。备选方案的具体任务在 PoC 记录里补写，不预写。
2. **MPS 也 no-go**：共享机商品**搁置**（两个商品保持 `inactive`，不在后台启用；路线图注明「等 NVIDIA 在 GB10 上支持 NVML 内存 / MIG 类功能后再评估」）。本计划仍完成 §6 的「Agent 多容器 + 按实例端口」改造（它对未来双机互联、后台维护容器都有用），其余 §4～§12 不实现。

## 4. 占用与分配（rental）

### 4.1 槽位候选
`pickSpec` 增加 `Slots int`（商品的 `share_slots`，由调用方从 `catalog` 读出），`pickMachines` 返回 `[]pickedSlot{MachineID, Slot}`：

```sql
-- 共享商品：候选 = 机器 × generate_series(1, N)，排除有重叠 lease 的 (机器, 槽位)；整机 N=1 时 slot 固定 0
SELECT m.id, s.slot FROM machines m
  LEFT JOIN machine_groups g ON g.id = m.group_id
  CROSS JOIN LATERAL (SELECT CASE WHEN $9::int <= 1 THEN 0 ELSE generate_series(1, $9::int) END AS slot) s
  WHERE m.product_id = $1 AND <机组条件> AND <指定机器条件> AND NOT (m.id = ANY($6))
    AND (CASE WHEN $7::boolean
         THEN (CASE WHEN $9 <= 1 THEN m.state = 'idle' ELSE m.state IN ('idle', 'allocated') END) AND m.agent_online
         ELSE m.state IN ('idle', 'allocated') END)
    AND ($9::int <= 1 OR (m.agent_features & 1) <> 0)                      -- 裁定 S10
    AND NOT EXISTS (SELECT 1 FROM machine_leases l WHERE l.machine_id = m.id AND l.slot = s.slot
                    AND l.status IN ('reserved', 'active') AND l.period && tstzrange($4, $5))
  ORDER BY <最紧贴前一档期（按 machine_id, slot）> DESC NULLS LAST, m.id, s.slot
  LIMIT $8 FOR NO KEY UPDATE OF m SKIP LOCKED
```

- 一台共享机的多个空槽对同一批次的多台是合法候选（一单 4 台可以落在同一台共享机的 4 个槽）。
- 排序增加「优先填已有租户的机器」（同机已占槽位数 DESC）放在「紧贴前一档期」之前：让空机器留给整机需求的切换与维护。
- `placeOnMachines` 的 savepoint 逻辑不变：`insertLease(..., slot, ...)`，排他冲突 → 剔除这对 (机器, 槽位) 再补取；`Exclude` 变为 `[]pickedSlot`。
- `countAvailable` 对共享商品数的是空槽数（市场页 `idle_count` 对共享商品显示「可用 X 份」）。

### 4.2 机器状态
- `fleet.MarkAllocated(ctx, q, id)` 改为 `idle → allocated`，**共享机已是 allocated 时视为成功**（`WHERE id = $1 AND state IN ('idle','allocated') AND agent_online RETURNING ...`；返回 `ErrNoCapacity` 仅当机器不在这两个状态或离线）。
- `fleet.Release(ctx, q, machineID)` 改为：仅当该机器没有其它未结束实例（`NOT EXISTS instances WHERE machine_id AND state IN (...)`）时 `allocated → idle`。调用方（`finishWipe`、`failCreating`、取消等）已在实例终态后调用，语义自然。
- `machineReady`（预订开通）对共享实例改为「机器 idle 或 allocated、在线、**该槽位**没有未结束实例」。

### 4.3 lease 与封顶
- `insertLease` 增加 `slot` 参数；`lockLease` 返回 `Slot`；`updateLeaseCapped` 的 `cap` 子查询加 `AND l.slot = own.slot`；`nextReservedLower` 加 `slot` 参数。所有 `refreshLease` 调用点不变。
- `occupiedUntil` 不变（配额不影响时长）。
- `reassign` / `AdminMigrate`：目标用 `pickMachines`（带 `Slots`）挑 (机器, 槽位)，实例的 `slot` 与 `machine_id` 一起更新；`agent_commands` 的 Create 带新槽位的 `ssh_port`。

### 4.4 出租率
`DailyOccupancy` 的 SQL 加 `JOIN machines m ON m.id = l.machine_id JOIN products p ON p.id = m.product_id`，秒数乘 `1.0 / p.share_slots`（裁定 S7）。注意机器切换商品后历史 lease 会按新份数加权，可接受（后台数字，非财务）。

## 5. 商品与定价（catalog）
- `catalog.Product` 增加 `ShareSlots int \`json:"share_slots"\``；`ListProducts / AdminProducts / ProductIDByCode` 读新列。
- 新增 `catalog.Service.ShareSlots(ctx, q, productID) (int, error)`（rental 分配时用）。
- 后台「价格与镜像」页商品表增加「份数」列（只读）；不提供建商品接口（两个共享商品由迁移落种子）；`PUT /products/{id}/status`（super，审计）新增：`active / inactive` 切换，用于 PoC 通过后上架。市场页只列 `active` 商品（已有）。
- 下单校验（`placeBatch`）：商品 `inactive` 时拒绝 `product_inactive`（现有逻辑若已拒绝则沿用）。

## 6. spark-agent

### 6.1 协议（`agent.proto`）
```proto
message Hello {  ...  uint32 features = 4; }                       // bit0 FeatureSharedSlots：支持 CreateInstance.share_slots
message CreateInstance { ... uint32 share_slots = 7; }             // 0 / 1 = 整机；2 / 4 = 按份等分配额
message Heartbeat { Metrics metrics = 1; repeated InstanceMetrics instances = 2; }
message InstanceMetrics { string instance_id = 1; uint64 mem_current = 2; uint64 mem_max = 3; double cpu_percent = 4; uint64 data_disk_used = 5; }
```
字段号与计划 7 的 `StateReport.images = 2` 不冲突（不同消息）。`make proto` 重新生成并提交。

### 6.2 容器规格（修订计划 2 §4.2，`executor.spec`）
```
N = max(1, share_slots)
memLimit = (MemTotal − 8GiB) / N          shm = memLimit / 2
cpus     = nproc / N（浮点，整机不传 --cpus）  pids = 32768 / N
dataGiB  = (指令 data_size_gib 或 AgentConfig.data_size_gib) / N   （整机 N=1 不变）
caps     = IPC_LOCK, SYS_PTRACE，(N == 1 时再加 SYS_NICE)
-p <lan_ip>:<ssh_port>:22/tcp               ssh_port 由指令给出（平台算 2222 + slot）
```
`runtime.Spec` 增加 `CPUs float64`（0 = 不限）、`PidsLimit int`、`DropNice bool`；`docker.Create` 据此追加 `--cpus`、`--pids-limit`、调整 `--cap-add`。`validateSpec` 对 `PidsLimit <= 0` 报错。单元测试对两种 N 的命令行做快照。

### 6.3 多实例收尾
- `executor.DataMount()` 删除；`metrics.Collector.Collect` 改收 `[]InstanceMount{ID, Dir, CgroupDir}`：`data_disk_used` 为各实例之和（机器级字段语义不变），并为每个实例读 `memory.current / memory.max / cpu.stat usage_usec`（容器 cgroup 路径由 `docker inspect .HostConfig.CgroupParent` 与容器 ID 拼出，或直接读 `/sys/fs/cgroup/system.slice/docker-<id>.scope/`；实现时以 `docker inspect --format '{{.Id}}'` 为准）。CPU 百分比按两次采样差值计算。
- `create`：磁盘余量检查用等分后的大小；`state.Instance` 新增 `ShareSlots`，`Recover` 不需要它（容器参数已固化在 docker）。
- 端口冲突：`create` 前检查本机没有别的实例记录占用同一 `ssh_port`（`state.json`），有则失败「SSH 端口冲突」（平台 bug 的兜底）。
- `selfcheck` 增加：`nproc ≥ 4`、`docker info` 显示 `Cgroup Version: 2` 与 `cpu` / `memory` / `pids` 控制器可用。
- fake runtime / fake agent：记录 Spec 全部字段供集成测试断言；支持注入每实例指标。

### 6.4 网络与限速
不改 `SPARK-FWD / SPARK-IN`；每个容器启动后各自 `VethOf → Limit`（已有）。60 秒周期的「限速缺失补回」遍历全部运行中实例（已是如此）。

## 7. 网关与接入层
- `agentgw/convert.go`：`SshPort` 取 `payload.SSHPort`（`fleet.CreateInstancePayload` 新增 `SSHPort int`、`ShareSlots int`），平台在入队 Create 时填 `cfg.SSHPort + slot` 与商品份数。
- `agentgw` 处理 `Hello.features` → `machines.agent_features`；处理 `Heartbeat.instances` → 写入 `machines.last_metrics.instances`（jsonb 数组）并对共享实例做 `instance_memory_pressure` 判定（连续 3 次 ≥ 95%，内存计数器在网关内存里按连接维护；文案「实例 {id} 内存接近配额上限」）。
- `upstream.Target` 增加 `Port int`（0 → `Config.Port`）；`sshgw` 与 `ingress` 的路由表从实例取 `slot` 算端口（`rental.Instance.SSHPort(base)` 方法：`base + slot`）。连接池键仍是 `InstanceID`。
- e2e：进程内假 sshd 只监听一个端口；`/hooks/machines` 造共享机时 `lan_ip` 仍 `127.0.0.1`，钩子为每个槽位端口再起一个假 sshd 监听（`sshtest` 支持多端口），否则共享实例的 SSH / ingress 用例连不上。

## 8. 控制台 API 与页面
- `GET /products`：每项新增 `share_slots`、`spec.shared`；`idle_count` 对共享商品 = 空槽数。
- 实例视图新增 `shared bool`、`slot int`、`share_slots int`、`quota {memory_bytes, cpus, data_disk_bytes}`（平台按商品 spec 展示值，不是机器实时值）、`usage {mem_current, mem_max, cpu_percent, at}`（来自 `last_metrics.instances`，没有则 null）。
- `POST /bookings`：`machine_id` 非空且商品 `share_slots > 1` → `400 shared_no_machine_pick`「共享机不支持指定机器」。
- `GET /machines/{id}/calendar`：机器商品为共享机 → `409 shared_no_calendar`。
- 市场页：共享商品卡片加「共享」标签与一行说明「GPU 算力共享、内存 / CPU / 磁盘独享配额」，规格四项按 spec 显示；`RentDialog` 对共享商品隐藏「指定机器」。
- 实例详情：基本信息卡增加「规格：共享机 1/4 · 内存 28 GiB · 5 核 · 数据盘 512 GiB」与内存用量进度条（有 `usage` 时）；接近上限（≥ 90%）时黄色提示。
- 帮助页新增「共享机」一节：配额如何生效（cgroup，超限进程会被系统终止或 `cudaMalloc` 失败）、`nvidia-smi` / `cudaMemGetInfo` 看到的是整机数值、算力共享不保证、同机用户可能看到 GPU 进程列表（按 P6 结果写）。

## 9. 后台 API 与页面
| 方法 | 路径 | 角色 | 变化 |
|---|---|---|---|
| GET | `/machines` | ops | 列表新增 `share_slots`、`used_slots`（未结束实例数）、`agent_features` |
| GET | `/machines/{id}` | ops | `current_instance` 改为 `current_instances[]`（按 slot 排序，整机仍一个元素）；`last_metrics.instances` 原样返回 |
| PUT | `/machines/{id}/product` | super，动态码，审计 | `{product_id}`；机器须 `maintenance` 且无 reserved / active lease，否则 `invalid_state` |
| PUT | `/products/{id}/status` | super，审计 | `{status}`；见 §5 |
| GET | `/products` | all | 新增 `share_slots` |
| GET | `/instances` | ops | 筛选新增 `shared=true/false`；行新增 `slot` |

页面：机器列表加「商品 / 槽位 2/4」列；机器详情把「当前实例」改成槽位表（槽位、实例、用户、状态、内存用量）、加「切换商品」按钮（维护中可用）；价格页加「份数」列与「上架 / 下架」开关；实例详情显示槽位与用量。菜单不变（`menu.spec.ts` 不需要改）。

## 10. 清扫器、告警与对账
- 对账表（计划 2 §3.2）按实例逐条比对，天然支持多容器；`orphan` 判定不变。
- 新告警：`instance_memory_pressure`（warning，§7）、`shared_agent_missing_feature`（warning：共享机的 Agent 不支持 share_slots，机器切成共享商品时与 Hello 时各检查一次，文案「机器 {serial} 的 Agent 不支持共享机，请升级」）。
- `FailStuckCreating` 等超时不变。

## 11. 安全边界
- 共享容器与整机容器同样非 privileged、无 Docker socket、无宿主机网络；相互之间：pid / mnt / net / ipc / uts 命名空间独立，`enable_icc=false` 阻断二层互通，`SPARK-IN` 阻断访问宿主机；共享的只有内核、GPU 驱动与 GPU 本身。
- 已知残余风险（写进帮助页与遗留）：GPU 进程列表对同机用户可见（P6）；GPU 算力可被邻居占满；驱动级漏洞的横向风险高于整机（容器都有 `/dev/nvidia*`）。
- 容器内 root 不能修改自身 cgroup（cgroupfs 对容器只读挂载，Docker 默认）；`CAP_SYS_RESOURCE` 不在默认能力集里，`--oom-score-adj` 不能被降回。

## 12. 测试与验收

| 层级 | 内容 |
|---|---|
| 单元 | 迁移（排他约束按槽位：同机不同槽位重叠允许、同槽位拒绝；整机 slot 0 行为不变）；`pickMachines` 对 N=1 / 2 / 4 的候选集与排序、`agent_features` 过滤、「优先填已有租户的机器」；`MarkAllocated / Release` 新语义；`updateLeaseCapped` 按槽位封顶；`DailyOccupancy` 加权；`executor.spec` 两种 N 的 docker 命令快照（含 `--cpus`、`--pids-limit`、无 `SYS_NICE`）；端口冲突兜底；指标采集解析 cgroup 文件；`convert` 的 `SshPort / ShareSlots`；`Hello.features` 写入；`instance_memory_pressure` 连续 3 次判定与稳定文案 |
| 集成（Go，无硬件） | 一台共享机 N=4：4 单落同机不同槽位、第 5 单 `no_capacity`；一单 4 台落同一台共享机；释放一个槽后 `state` 仍 allocated、全释放后 idle；预订共享槽位、改派到另一台共享机的空槽、续费被同槽位下一预订挡住（`renew_blocked`）；换机（`AdminMigrate`）保持商品；旧 Agent（features=0）的共享机不进候选；每实例 Create 的 `ssh_port` 正确；并发 20 单抢 1 台 4 槽不超卖 |
| 端到端（Playwright） | 市场页共享卡片与 `idle_count`「可用 X 份」；下单共享机 → 实例详情显示规格与用量；RentDialog 对共享商品无「指定机器」；后台机器详情槽位表、切换商品（维护中 / 非维护中）、价格页上架开关；覆盖表新增 `C-MKT-09～11`、`C-DET-26～27`、`A-MCH-10～12`、`A-CAT-04～05` |
| **真机 PoC（Task 2～3）** | §3 |
| **真机验收（最后）** | §12.1 |

### 12.1 真机验收清单（192.168.1.221，平台跑在 Mac）
1. 升级 Agent（新 Hello 带 features）→ 后台机器详情显示「支持共享机」；机器进维护 → 「切换商品」为「共享机 1/4」→ 结束维护。
2. 两个不同用户各下单 1/4 → 两个实例 running；`docker ps` 两个容器，端口 `:2223`、`:2224`；`docker inspect` 的 Memory ≈ (MemTotal − 8GiB)/4、NanoCpus = 5 核、PidsLimit 8192、CapAdd 不含 SYS_NICE；两个 loop 数据盘各 512 GiB。
3. 分别经网关 SSH 进两个容器：`nvidia-smi` 都看到 GB10；`/sys/fs/cgroup/memory.max` ≈ 28 GiB；`nproc` 显示 20 但 `cat /sys/fs/cgroup/cpu.max` 为 `500000 100000`；`/root/data` 512G 可写。
4. 复跑 PoC 的 P1（`alloc 40 1 --touch`，用 S11 提交的程序）：在 ≈28 GiB 处被限制，邻居容器的 `burn` 不受影响；控制台实例详情内存用量曲线上升到 95% 以上后，后台出现 `instance_memory_pressure` 告警一条且不重复。
5. 隔离：容器 A 无法访问容器 B 的 `10.200.0.x:22`（`enable_icc=false`）；A 无法访问宿主机 `:2224`；公网可达；两个容器各自限速生效（`tc qdisc show` 两个 veth 各有 tbf + ingress）。
6. 第 3、4 位用户下单 → 4 个槽满；第 5 单 `no_capacity`；市场页「可用 0 份」。
7. 到期 / 续费 / 释放 / 洗机对其中一个槽位：只影响该容器与该数据盘；机器 `state` 保持 allocated；全部释放后回 idle。
8. 后台「换机」一个共享实例（只有一台机器 → 预期 `no_capacity`，验证商品匹配的错误路径）；预订一份 1/4（机组方式）→ 到点开通到空槽。
9. 重启机器（PDU 或 `reboot`）→ Agent 恢复后 4 个容器按 `state.json` 依次挂盘、启动、限速；端口各自正确。
10. 机器切回整机商品：须先释放全部共享实例并进维护；切换后下单整机 → 端口 2222、内存上限回到 MemTotal − 8GiB。

## 13. 部署与配置
- 新增配置项：无（`SPARK_UPSTREAM_SSH_PORT` 语义改为基准端口，README 更新；`SPARK_AGENT_DATA_SIZE_GIB` 的说明加「共享机按份等分」）。
- 发布顺序：先升级全部 Agent（新 Hello / Heartbeat / CreateInstance 字段），再发布 platform；旧 Agent 的机器因 `agent_features = 0` 不会被分配共享实例。
- 迁移 00023 在计划 7、8、9 之后执行（编号已预留）。

## 14. 错误码与文案（新增）
`shared_no_machine_pick`「共享机不支持指定机器」、`shared_no_calendar`「共享机不提供按机器日历」、`machine_product_locked`「机器有未结束的占用，不能切换商品」（409）、`product_inactive`「该商品暂未上架」。Agent 侧：「Agent 版本不支持共享机」「SSH 端口冲突」。

## 15. 范围与遗留

**本计划做**：§3 PoC；§2、§4～§12（go 时）；§3.5 第 2 条（no-go 时只做 §6 的多容器与按实例端口）。

**明确不做（遗留清单）**
- GPU 算力硬限额（MPS / HAMi-core / 未来 MIG 类功能）；忙时调度；按量计费。
- 一台机器混卖多种份数；份数 8；共享机的按机器日历（泳道图）。
- 容器可写层限额（计划 2 遗留，共享机上更值得做：XFS project quota 需重装系统）。
- 磁盘 IO 限额（loop 设备上的 `--device-read-bps` 需验证）、网络带宽按份数缩减（目前每容器都用默认 200 Mbps）。
- 共享机专属的告警自动处置、租户内存超限时的通知短信。
- 后台修改机器 `rack / slot / lan_ip / PDU`（计划 4 遗留，本计划只补 `product_id`）。
- `nvidia-smi` 进程列表对邻居可见的屏蔽（需要 `NVIDIA_VISIBLE_DEVICES` 以外的驱动能力，目前没有）。

## 16. 任务拆分（供实施计划引用，19 项）
1. PoC 工具：`deploy/poc/shared/`（alloc / burn / fault、Dockerfile、run-poc.sh），在 221 上构建镜像。
2. PoC 第一轮（P1～P7）+ 记录 + go / no-go 判定。
3. HAMi-core 时间盒记录（附录 A）；no-go 时 MPS 第二轮并改写后续任务。
4. 迁移 00023；`catalog` share_slots；`rental` slot 字段、`Instance.SSHPort(base)`、错误码。
5. `pickMachines` 槽位化、`MarkAllocated / Release` 新语义、`countAvailable / AvailableNow`、`machineReady`。
6. lease 槽位化（`insertLease / lockLease / updateLeaseCapped / nextReservedLower`）、`DailyOccupancy` 加权、换机 / 改派带槽位。
7. 下单 / 预订校验（共享不指定机器、商品未上架）、Create 入队带 `ssh_port / share_slots`、`agent_features` 过滤。
8. 协议：proto 三处新增、`make proto`、`convert`、`Hello.features` 落库、心跳实例指标落库与 `instance_memory_pressure`。
9. Agent：`spec` 等分、`runtime.Spec` 新字段与 docker 命令、`DataMount` 删除与多实例指标、端口冲突兜底、`selfcheck`、fake 实现。
10. 接入层：`upstream.Target.Port`、sshgw / ingress 按实例端口、`sshtest` 多端口、e2e 钩子造共享机。
11. 后台 API：机器列表 / 详情槽位、切换商品、商品上下架、实例筛选。
12. 控制台 API：产品 `share_slots`、实例视图 quota / usage、预订与日历的拒绝分支。
13. 集成测试 + 并发测试。
14. **真机检查点 A**：后端 + API 在 221 上跑 §12.1 第 1～7 项（curl）。
15. 控制台页面：市场卡片、RentDialog、实例详情规格与用量、帮助页。
16. 后台页面：机器列表 / 详情槽位表与切换商品、价格页份数与上架开关。
17. Playwright 用例与覆盖表。
18. **真机验收 B**（§12.1 全部）+ `acceptance-plan10.md`。
19. 收尾：README、`docs/image-spec.md`（共享机运行时环境一节）、`followups-plan10.md`、`make check`。
