# Spark Cloud 计划 10：共享机 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 先在真机上用一天证明「GB10 统一内存下 CUDA 分配受容器 cgroup `memory.max` 约束」（go / no-go），通过后把一台 DGX Spark 切成 2 或 4 份出租：每份独享内存 / CPU / 数据盘配额、共享 GPU 算力，价格低于整机；共享实例在下单、预订、续费、退款、换机、接入层、后台处置上与整机一致；Agent 支持一台机器多个容器；最后在 192.168.1.221 上完成验收。

**Architecture:** 不加新进程、不引入 Kubernetes / HAMi / MPS。「槽位」进入 `machine_leases` 与 `instances`（整机 slot 0，共享 1..N），排他约束改为 `(machine_id, slot, period)`，所有分配走槽位化的 `rental.pickMachines`；机器卖哪种商品由 `machines.product_id` 决定（`products.share_slots ∈ {1,2,4}`，不混卖）；Agent 按 `share_slots` 等分 `(MemTotal − 8GiB)`、`nproc`、数据盘，容器参数 `--memory --cpus --shm-size --pids-limit`，GPU 仍 `--gpus all`；SSH 端口 `基准 + slot`；心跳带每实例 cgroup 用量。

**Tech Stack:** Go 1.27（`~/.local/go/bin`）、pgx v5、chi v5、embedded-postgres（`dbtest.Main`）、gRPC / protobuf（`make proto`，protoc 在 `~/.local/bin`）；前端 Vue 3 + Element Plus + Vitest + Playwright。PoC 用 CUDA C（nvcc，在机器上的 `nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04` 容器内编译）。**不新增 Go 模块依赖、不新增 npm 依赖。**

**Spec:** `docs/superpowers/specs/2026-10-24-plan10-shared-machine-design.md`（下称「规格」）。上位：计划 2 规格 §3、§4；计划 3 规格 §5.3；计划 6 规格 §1.1、§4。

**分支：** `feat/plan10-hami-shared`（从 `main` 切出；计划 7、8、9 未合并，迁移编号 00020～00022 已预留，本计划用 **00023**）。

**本计划不包含（规格 §15）：** GPU 算力硬限额、混卖份数、份数 8、共享机按机器日历、可写层限额、磁盘 IO 限额、按份缩减带宽、超限短信、后台改机器其它字段、屏蔽邻居 GPU 进程列表。

## 执行须知（写给每个子代理）

- 任务**按编号顺序串行执行，一个任务一个子代理**。子代理只看得到自己任务的文字：每个任务的 Interfaces 必须完整（消费什么、产出什么、精确签名）。本计划给出签名、SQL 骨架与测试清单，不给整段实现；实现者按规格与现有代码风格补全。
- **Task 1～3 是 PoC，是整个计划的闸门。** Task 2 的结论为 no-go 时，Task 4 起**不执行**，改按规格 §3.5 走：先做 MPS 第二轮 PoC（Task 3 的 B 分支），仍 no-go 则只执行 Task 8 中「协议」与 Task 9、10（多容器 Agent 与按实例端口，share_slots 固定 1）并收尾。协调人在 Task 3 结束时把结论写进本文件顶部的「PoC 结论」一行。
- 测试机 192.168.1.221（`imac` 用户），**当前正用于计划 7 验收；Task 1～3 必须等协调人确认机器空出后才开始**。需要 root 的命令由协调人在运行时执行，密码由负责人在对话中提供，经 stdin 送给 `sudo -S`，**不写进任何文件、脚本、提交或记录**（做法见计划 6 实施计划「执行须知」的 `s()` 函数）。
- 机器访问不了 Docker Hub：基础镜像用 `nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04`；apt 走 ports.ubuntu.com（容器内可达）。
- 每个任务结束前必须实际运行并通过：`go vet ./... && go test ./...`；涉及 proto 的任务再加 `make proto && git diff --exit-code internal/agentpb`（生成物已提交且一致）；涉及前端的任务再加 `cd web && npm test && npm run build`；页面任务再加该任务列出的 Playwright 用例。证据是命令输出。
- 提交信息结尾空一行加 `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`。

**PoC 结论：** **`no-go → 搁置`**（2026-10-25，192.168.1.221；记录合并在 `docs/superpowers/acceptance-plan10.md` 而非单独的 `acceptance-plan10-poc.md`）。路线 C：`cudaMalloc` 不计入容器 cgroup（P1 在 32g 容器里分配到 48 GiB）；MPS：限额按进程计，一个容器两个进程合计 54 GiB（规格裁定 S13）。

**备选路线任务改写（搁置）：** 只执行 Task 1～3、Task 8 的协议部分（另加 `convert` 取指令端口、心跳每实例指标写入 `last_metrics.instances`，不做 `agent_features` 落库与内存压力告警）、Task 9（Agent 按份配额能力保留，平台只发 `share_slots = 1`）、Task 10（`Instance.SSHPort(base)` 暂恒为基准端口；不做「钩子造共享机」）、Task 19（收尾）；真机验收改为验证新 Agent 与整机实例行为不变、多容器能力（规格裁定 S14）。Task 4～7、11～18 不执行，迁移 00023 不落库。

## Global Constraints

- 仓库根目录 `/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud-p10`；所有命令前置 `export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH`。开发 Mac 没有 Docker、没有 GPU。
- 槽位：`instances.slot SMALLINT`、`machine_leases.slot SMALLINT`，整机 0，共享 1..`share_slots`。`instances_machine_slot_active_uniq (machine_id, slot) WHERE state IN ('creating','running','stopped','wiping')`。排他约束 `(machine_id WITH =, slot WITH =, period WITH &&) WHERE status IN ('reserved','active')`。
- 机器状态（规格 §1.2 第 3 条）：共享机有 ≥1 个未结束实例 → `allocated`，全空 → `idle`。
- 候选谓词（规格 §4.1）：整机商品沿用现有；共享商品 `state IN ('idle','allocated') AND agent_online AND (agent_features & 1) <> 0`，候选是 (机器 × 1..N 槽位) 中无重叠 lease 的对；排序「同机已占槽位数 DESC → 紧贴前一档期 DESC NULLS LAST → id → slot」；加锁 `FOR NO KEY UPDATE OF m SKIP LOCKED`（**不能用 `FOR UPDATE`**，理由见计划 6 实施计划）。
- 配额（规格 §6.2）全部在 Agent 侧按 `share_slots` 等分；平台只传 `share_slots` 与 `ssh_port = SPARK_UPSTREAM_SSH_PORT + slot`。
- 加锁顺序不变：`instance → agent_commands → orders → wallet`；`machine → wallet`。
- 稀疏数据盘大小 = `data_size_gib / share_slots`（整除，向下取整）。
- 所有新告警文案稳定（不带数值）：`instance_memory_pressure`「实例 {id} 内存接近配额上限」、`shared_agent_missing_feature`「机器 {serial} 的 Agent 不支持共享机，请升级」。
- 错误码（规格 §14）：`shared_no_machine_pick`、`shared_no_calendar`、`machine_product_locked`、`product_inactive`。
- e2e 覆盖表 ID：`C-MKT-09～11`、`C-DET-26～27`、`A-MCH-10～12`、`A-CAT-04～05`。

## 已做出的裁定（规格未写明或与代码核对后确定的细节）

- `pickMachines` 返回类型从 `[]int64` 改为 `[]pickedSlot{MachineID int64; Slot int}`；`placeOnMachines` 的 `fn` 签名改为 `func(sp pgx.Tx, p pickedSlot) error`；`pickSpec.Exclude` 改为 `[]pickedSlot`（SQL 里用 `NOT EXISTS (SELECT 1 FROM unnest($6::bigint[], $7::int[]) AS x(mid, slot) WHERE x.mid = m.id AND x.slot = s.slot)`）。所有调用方（`batch.go`、`migrate.go`、`reassign.go`、`admin_bookings.go`）一并改。
- 整机商品（`Slots <= 1`）的候选 SQL 用同一条语句，`slot` 列由 `CASE` 给 0，不维护两条 SQL。
- `products.share_slots` 由 `catalog.Service.ShareSlots(ctx, q, productID)` 读取并在 `placeBatch` 开头取一次，放进 `pickSpec.Slots`；`AdminMigrate` / `reassign` 从实例的 `product_id` 读。
- `fleet.MarkAllocated` 的 SQL 改为 `WHERE id = $1 AND state IN ('idle','allocated') AND agent_online`；`fleet.Release` 改为带 `NOT EXISTS` 子查询（见 Task 5）。`IdleCount` 保留（机组页「现空闲」语义为「无租户的机器数」）。
- `rental.Instance` 新增 `Slot int`；方法 `(Instance) SSHPort(base int) int` 返回 `base + Slot`（base ≤ 0 时取 2222）。`upstream.Target.Port` 为 0 时沿用 `Config.Port`。
- `fleet.CreateInstancePayload` 新增 `SSHPort int \`json:"ssh_port"\``、`ShareSlots int \`json:"share_slots"\``；`agentgw/convert.go` 优先用 payload 的 `SSHPort`，为 0 时退回 `cfg.SSHPort`（兼容存量 pending 指令）。
- `Hello.features` 位图常量在 `internal/agentpb` 旁的 Go 包 `internal/agent/features`（`const SharedSlots uint32 = 1`），Agent 与网关共用。
- `last_metrics` jsonb 的结构：`{...机器级字段..., "instances": [{"instance_id","mem_current","mem_max","cpu_percent","data_disk_used"}]}`；没有实例时 `instances: []`。
- `instance_memory_pressure` 的连续计数放在网关每条连接的内存里（`map[instanceID]int`），心跳低于 95% 清零；连续 3 次 → `RaiseAlertOnce`。
- 商品上下架接口 `PUT /products/{id}/status`：`catalog.Service.SetProductStatus(ctx, actor, id, status)`，审计 `product.status`。
- 机器切换商品 `PUT /machines/{id}/product`：`fleet.SetProduct(ctx, q, actor, machineID, productID)` 检查 `state = 'maintenance'`，`rental.AdminSetMachineProduct` 在同一事务里先锁机器（`FOR NO KEY UPDATE`）再检查无 reserved / active lease，再调 fleet；审计 `machine.product`。stepUp（动态码）与「撤销接入」一致。
- e2e `/hooks/machines` 增加 `product_code?`（默认 `spark-1`）；共享机时钩子为该机器的每个槽位端口起一个假 sshd（`sshtest.Server.ListenAlso(port)`）。
- 控制台实例视图 `quota` 由平台按商品 spec 算：`memory_bytes = spec.memory_gb << 30`、`cpus = 解析 spec.cpu 的前导整数`、`data_disk_bytes = spec.disk_tb * 2^40`；只是展示值。

## 文件结构

```
spark-cloud-p10/
├── deploy/poc/shared/{alloc.cu,burn.cu,fault.cu,Dockerfile,run-poc.sh,README.md}        # Task 1
├── docs/superpowers/acceptance-plan10-poc.md                                            # Task 2、3
├── migrations/00023_shared.sql                                                          # Task 4
├── api/proto/agent/v1/agent.proto、internal/agentpb/                                     # Task 8
├── internal/
│   ├── catalog/catalog.go、admin.go                                                      # Task 4、11
│   ├── rental/service.go、alloc.go、leases.go、batch.go、migrate.go、reassign.go、provision.go、admin_bookings.go、admin_queries.go、booking.go、admin_ops.go  # Task 4～7
│   ├── fleet/machines.go、commands.go                                                    # Task 5、7、11
│   ├── agent/features/features.go、agent/executor/{executor,ops}.go、agent/runtime/{runtime.go,docker/docker.go,fake/fake.go}、agent/metrics/metrics.go、agent/client/client.go、agent/fakeagent/fakeagent.go、agent/state/store.go  # Task 9
│   ├── agentgw/{convert,connect,server}.go                                               # Task 8
│   ├── access/upstream/pool.go、access/sshtest/、sshgw/、ingress/                        # Task 10
│   ├── api/{market,instances,booking,server}.go                                          # Task 12
│   ├── adminapi/{machines,catalog,instances,views,server}.go                             # Task 11
│   ├── e2ehooks/hooks.go、app/shared_e2e_test.go、rental/shared_test.go、rental/concurrency_test.go  # Task 10、13
├── web/shared/src/types/{catalog,instance,admin}.ts、labels.ts                           # Task 15、16
├── web/console/src/{components/RentDialog.vue,views/{MarketView,InstanceDetailView,HelpView}.vue}   # Task 15
├── web/admin/src/views/{MachinesView,MachineDetailView,CatalogView,InstanceDetailView}.vue           # Task 16
├── web/e2e/{console/{market,instance-detail}.spec.ts,admin/{machines,catalog}.spec.ts,helpers/hooks.ts,coverage.json}  # Task 17
├── README.md、docs/image-spec.md、docs/superpowers/{acceptance-plan10.md,followups-plan10.md}       # Task 14、18、19
```

## 任务依赖

**Task 1（PoC 工具）→ 2（PoC 第一轮 + 判定）→ 3（HAMi 时间盒 / MPS 分支）** ⇒ 闸门 ⇒ 4（迁移 + 类型）→ 5（候选与机器状态）→ 6（lease 槽位化）→ 7（下单 / 入队）→ 8（协议 + 网关）→ 9（Agent）→ 10（接入层 + 钩子）→ 11（后台 API）→ 12（控制台 API）→ 13（集成测试）→ **14 真机检查点 A** → 15（控制台页面）→ 16（后台页面）→ 17（Playwright）→ **18 真机验收 B** → 19（收尾）。共 19 个任务。

---
### Task 1: PoC 工具（CUDA 测试程序、镜像、脚本）

**Files:**
- Create: `deploy/poc/shared/alloc.cu`、`deploy/poc/shared/burn.cu`、`deploy/poc/shared/fault.cu`、`deploy/poc/shared/Dockerfile`、`deploy/poc/shared/run-poc.sh`、`deploy/poc/shared/README.md`

**Interfaces:**
- `alloc <total_gib> <step_gib> [--managed] [--touch] [--hold <sec>]`：循环 `cudaMalloc`（或 `cudaMallocManaged`）`step_gib`，`--touch` 时对每块 `cudaMemset` 全量写入；每步打印 `allocated=<GiB> free/total=<cudaMemGetInfo>`；分配失败打印 `cudaMalloc failed at <GiB>: <错误名>` 并以 **退出码 3** 退出；达到 total 后打印 `reached total` 并 `--hold` 秒后退出码 0。
- `burn <sec>`：在 GPU 上持续做 8192×8192 的 cuBLAS `sgemm`（镜像里有 cublas），每秒打印 `t=<s> gflops=<x>`，结束打印 `avg_gflops=<x>`，退出码 0；中途 CUDA 错误打印 `cuda error: <名>` 退出码 4。
- `fault`：启动一个对越界地址写入的 kernel，`cudaDeviceSynchronize` 得到 `cudaErrorIllegalAddress` 后打印并退出码 5。
- `Dockerfile`：`FROM nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04`；`apt-get install -y stress`（ports.ubuntu.com）；`nvcc -O2 -arch=sm_121`（GB10 的计算能力按机器上 `nvidia-smi --query-gpu=compute_cap --format=csv` 实测填写，README 注明）编译三个程序到 `/poc/`；不设 ENTRYPOINT。
- `run-poc.sh <step>`：`step ∈ {build, p1, p1b, p2, p3, p4, p5, p6, p7, clean}`。每步只打印将要执行的 `docker run` 命令并执行，日志写 `/tmp/poc/<step>-<容器>.log`；`docker run` 参数固定：`--rm --name poc-<a|b> --gpus all --network spark0 --memory <X>g --memory-swap <X>g --shm-size <X/2>g --cpus 5 --pids-limit 8192 --oom-score-adj 500 --cap-drop SYS_NICE --cap-add IPC_LOCK --cap-add SYS_PTRACE --ulimit memlock=-1:-1 spark/poc-shared:1 <程序>`。p1 的 X=32，其余 X=56。p3 / p4 / p5 用 `&` 并行起两个容器并 `wait`。p6 在 p4 运行中 `docker exec poc-b nvidia-smi` 与 `ps aux`。`clean` 删容器与 `/tmp/poc`。
- README：各步目的、通过标准（照抄规格 §3.2 表）、需要 root 的步骤（`dmesg`、`systemctl stop/start spark-agent`）。

- [ ] **Step 1: 写三个 CUDA 程序与 Dockerfile**（本地无法编译，只做 `nvcc` 语法级自查：无外部依赖、错误处理用宏 `CK(expr)` 打印 `cudaGetErrorName`）。
- [ ] **Step 2: 写 `run-poc.sh`**，`shellcheck`（若本机有）或 `sh -n` 通过；每一步的 docker 命令与规格 §3.2 一一对应，脚本顶部注释标明 P1～P7。
- [ ] **Step 3: 在 221 上构建**（协调人确认机器空闲后）：`scp -r deploy/poc/shared imac@192.168.1.221:/tmp/poc-src && ssh imac@192.168.1.221 'docker build -t spark/poc-shared:1 /tmp/poc-src'`。期望：镜像构建成功；`docker run --rm --gpus all spark/poc-shared:1 /poc/alloc 1 1` 打印一步并 `reached total`。
- [ ] **Step 4: 提交** `feat(poc): shared-machine PoC tools (alloc/burn/fault, Dockerfile, runner)`。

---
### Task 2: PoC 第一轮（P1～P7）与 go / no-go 判定

**Files:**
- Create: `docs/superpowers/acceptance-plan10-poc.md`

**Interfaces:**
- 消费：Task 1 的镜像与脚本；规格 §3.2 的表。
- 产出：记录文件，结构：机器与软件版本 → 每个实验的命令、关键输出（原文摘录）、是否通过 → §3.3 的三项附加观察 → **结论（go / no-go，一行）** → 观察与遗留。

- [ ] **Step 1: 准备**：协调人 `sudo systemctl stop spark-agent`（PoC 期间不让 Agent 对账 / 清理容器）；确认 `docker network inspect spark0` 存在（Agent 创建过）；`nvidia-smi` 正常。
- [ ] **Step 2: P1 / P1b**：`run-poc.sh p1`、`run-poc.sh p1b`。读日志里最后一行 `allocated=` 与退出码；协调人 `sudo dmesg | tail -50` 看是否有 `Memory cgroup out of memory` 记录。判定按规格：在 32 ± 2 GiB 处被 `cudaErrorMemoryAllocation` 拦下或被 OOM-kill 都算通过；**分配到 48 GiB 成功 = no-go**。
- [ ] **Step 3: P2、P3、P4、P5、P7**：逐步执行并记录。P3 判断 A 的 `t=` 序列是否连续（每秒一行，缺口 > 5 行视为中断）。P5 期间在宿主机每 5 秒记录 `grep MemAvailable /proc/meminfo`。
- [ ] **Step 4: P6**：记录容器内 `nvidia-smi` 的进程表（能否看到邻居 PID / 名称）与 `ps aux` 行数。
- [ ] **Step 5: §3.3 附加观察**：`cudaMemGetInfo` 的 free / total 数值；OOM 的表现形式；P1 过程中宿主机 `cat /sys/fs/cgroup/system.slice/docker-<id>.scope/memory.current` 是否随 GPU 分配增长（每 GiB 采样一次）。
- [ ] **Step 6: 恢复**：`run-poc.sh clean`；协调人 `sudo systemctl start spark-agent`；后台确认机器重新在线。
- [ ] **Step 7: 写记录并下结论**，提交 `docs: plan 10 PoC round 1 on 192.168.1.221 (<go|no-go>)`。把结论写进本计划「PoC 结论」一行。

---
### Task 3: HAMi-core 时间盒；no-go 时的 MPS 第二轮

**Files:**
- Modify: `docs/superpowers/acceptance-plan10-poc.md`（附录 A；no-go 时新增「第二轮：MPS」一节）
- Modify（仅 no-go）: 本计划文件（在「PoC 结论」后追加「备选路线任务改写」小节）

**A 分支（总是执行，≤ 2 小时）**
- [ ] 在 221 上 `docker run -it --rm --gpus all nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04` 内：`apt-get install -y git cmake build-essential`、`git clone https://github.com/Project-HAMi/HAMi-core`（机器能否到 github.com 先 `curl -I`，不可达则记录「无法获取源码」结束）、`./build.sh`。记录：能否编出 `libvgpu.so`；不能则记录前 50 行错误。
- [ ] 能编出时：挂 `libvgpu.so` 进 `spark/poc-shared:1` 容器，`LD_PRELOAD=/libvgpu.so CUDA_DEVICE_MEMORY_LIMIT=8g /poc/alloc 16 1`，记录在 8 GiB 处是否报错；再 `unset LD_PRELOAD` 跑一次，记录是否失效（预期失效）。
- [ ] 写附录 A（结论固定为「不作主路线，理由见规格 §1.1」加上实测事实）。

**B 分支（仅 Task 2 为 no-go 时，≤ 1 天）**
- [ ] 宿主机（协调人 root）：`nvidia-cuda-mps-control -d`（`CUDA_MPS_PIPE_DIRECTORY=/tmp/spark-mps/pipe`、`CUDA_MPS_LOG_DIRECTORY=/tmp/spark-mps/log`），`echo set_default_device_pinned_mem_limit 0 28G | nvidia-cuda-mps-control`、`echo set_default_active_thread_percentage 25 | nvidia-cuda-mps-control`。
- [ ] 容器以 `-v /tmp/spark-mps/pipe:/tmp/nvidia-mps -e CUDA_MPS_PIPE_DIRECTORY=/tmp/nvidia-mps --ipc=host`（PoC 先用 host IPC 验证可行性，实现时再收紧为 MPS 服务容器的 `--ipc=container:`）重跑 P1、P2、P3、P5。go 标准见规格 §3.5 第 1 条（P3 允许连累但须 30 秒内恢复）。
- [ ] 记录「第二轮：MPS」并下结论；MPS go 时在本计划追加「备选路线任务改写」：Task 9 增加「Agent 托管 `spark-mps` systemd 单元与管道目录、容器挂载与 IPC 参数、`mps_restarted` 告警」，其余任务不变；MPS no-go 时在本计划标注「只执行 Task 8（协议）、9、10（share_slots 固定 1）与 19」。
- [ ] `echo quit | nvidia-cuda-mps-control`，恢复 Agent。提交 `docs: plan 10 PoC appendix A (HAMi-core) [and round 2 (MPS)]`。

---
### Task 4: 迁移 00023、`catalog.share_slots`、`rental` 槽位字段与错误码

**Files:**
- Create: `migrations/00023_shared.sql`、`internal/rental/shared.go`、`internal/rental/shared_test.go`
- Modify: `internal/catalog/catalog.go`（`Product.ShareSlots`、`ListProducts`、`ProductIDByCode`、新增 `ShareSlots`）、`internal/catalog/admin.go`（`AdminProducts` 读新列）、`internal/rental/service.go`（`Instance.Slot`、`instanceCols`、`scanInstance`、`SSHPort`）、`internal/platform/apperr/apperr.go`

**Interfaces:**
- Produces:
  - 迁移：规格 §2 全部（`products.share_slots`、`instances.slot` 与新唯一索引、`machine_leases.slot` 与新排他约束 / 索引、`machines.agent_features`、两个 `inactive` 商品与六条价格）；Down 带 `DO $$ RAISE EXCEPTION` 保护（存在 `slot > 0` 的未结束实例或 reserved / active lease 时拒绝），恢复旧约束名（实现时先在 `dbtest` 里 `SELECT conname FROM pg_constraint WHERE conrelid = 'machine_leases'::regclass AND contype = 'x'` 取得旧名字写进迁移）。
  - `catalog.Product.ShareSlots int \`json:"share_slots"\``；`func (s *Service) ShareSlots(ctx context.Context, q db.Querier, productID int64) (int, error)`。
  - `rental.Instance.Slot int`；`func (i Instance) Shared() bool`（`Slot > 0`）；`func (i Instance) SSHPort(base int) int`。
  - `internal/rental/shared.go`：`const MaxShareSlots = 4`；`func validSlot(shareSlots, slot int) bool`。
  - `apperr`：`ErrSharedNoMachinePick`（400 `shared_no_machine_pick`）、`ErrSharedNoCalendar`（409 `shared_no_calendar`）、`ErrMachineProductLocked`（409 `machine_product_locked`）、`ErrProductInactive`（409 `product_inactive`）。

- [ ] **Step 1: 写失败的测试** `internal/rental/shared_test.go`：`TestSharedMigration`——① 同机 slot 1 与 slot 2 的重叠 active lease 可以同时存在；② 同机 slot 1 两条重叠 → 排他冲突（`db.IsExclusionViolation`）；③ 整机 slot 0 两条重叠仍冲突；④ `instances` 同机 slot 1 两个 running 实例 → 唯一冲突，slot 1 与 slot 2 可以共存；⑤ `products` 里 `spark-shared-2 / spark-shared-4` 存在、`inactive`、`share_slots` 为 2 / 4、各有三档价格（金额按裁定 S1）；⑥ `SSHPort(2222)` 对 slot 0 / 3 返回 2222 / 2225，`SSHPort(0)` 退回 2222 基准。
- [ ] **Step 2: 运行确认失败**：`go test ./internal/rental/ -run TestSharedMigration`（编译错误或列不存在）。
- [ ] **Step 3: 写迁移、改 catalog 与 rental 类型**。`instanceCols` 加 `slot`；`scanInstance` 同步；`catalog` 的三处查询加列。
- [ ] **Step 4: 全量测试**：`go vet ./... && go test ./...`。现有测试里直接 `INSERT INTO machine_leases` 的地方会因新列默认值 0 而无需改动；`TestBookingColumnsDefaults` 若断言唯一索引名，更新为新名。
- [ ] **Step 5: 提交** `feat(shared): migration 00023, product share_slots, instance/lease slot`。

---
### Task 5: 槽位化候选、`MarkAllocated / Release` 新语义、可用数

**Files:**
- Modify: `internal/rental/alloc.go`（`pickSpec.Slots`、`pickedSlot`、`pickWhereSQL`、`pickOrderSQL`、`pickArgs`、`pickMachines`、`countAvailable`、`placeOnMachines`、`pickLocked`、`AvailableNow`）、`internal/rental/batch.go`、`internal/rental/migrate.go`、`internal/rental/reassign.go`、`internal/rental/admin_bookings.go`、`internal/rental/provision.go`（`machineReady`）、`internal/fleet/machines.go`（`MarkAllocated`、`Release`）
- Test: `internal/rental/alloc_test.go`、`internal/rental/shared_test.go`、`internal/fleet/machines_test.go`

**Interfaces:**
- `type pickedSlot struct { MachineID int64; Slot int }`
- `pickSpec` 新增 `Slots int`（1 = 整机）与 `Exclude []pickedSlot`。
- `func pickMachines(ctx, q, spec, limit, lock) ([]pickedSlot, error)`；`func countAvailable(ctx, q, spec) (int, error)`（共享商品返回空槽数）；`func placeOnMachines(ctx, tx, spec, want, fn func(sp pgx.Tx, p pickedSlot) error) ([]pickedSlot, error)`。
- SQL 按规格 §4.1：`CROSS JOIN LATERAL generate_series`、`agent_features & 1` 过滤、排序前置「`(SELECT count(*) FROM instances i WHERE i.machine_id = m.id AND i.state IN ('creating','running','stopped','wiping')) DESC`」（仅 `Slots > 1` 时生效，整机该项恒 0 无影响）。
- `fleet.MarkAllocated`：`WHERE id = $1 AND state IN ('idle','allocated') AND agent_online`。
- `fleet.Release`：`UPDATE machines SET state = 'idle', updated_at = $2 WHERE id = $1 AND state = 'allocated' AND NOT EXISTS (SELECT 1 FROM instances WHERE machine_id = $1 AND state IN ('creating','running','stopped','wiping'))`。
- `machineReady(ctx, tx, machineID, slot)`：`state IN ('idle','allocated') AND agent_online`，且 `NOT EXISTS instances WHERE machine_id AND slot = $2 AND state IN (...)`（整机 slot 0 等价于原语义）。
- 调用方：`batch.go` 的插实例写 `slot = p.Slot`、`insertLease(..., p.Slot, ...)`（Task 6 加参数，本任务先传 0 并留 TODO 会导致测试失败——所以 **Task 5 与 6 由同一子代理连续完成也可以**；若分开，本任务 `insertLease` 暂加第二个签名 `insertLeaseSlot`，Task 6 合并）。

- [ ] **Step 1: 写失败的测试**：`TestPickSharedSlots`（一台 N=4 共享机、features=1：`pickMachines` limit 8 返回 4 对 (m,1..4)；占掉 slot 2 后返回 3 对；features=0 返回空；整机商品 N=1 返回 (m,0)）、`TestPickPrefersPartiallyUsedMachine`（两台共享机，一台已占 1 槽，先返回它）、`TestMarkAllocatedSharedTwice`、`TestReleaseKeepsAllocatedWhileOthersRemain`、`TestAvailableNowCountsSlots`。helpers：`e.newSharedMachine(serial, slots)`（登记机器、`product_id` 指向 `spark-shared-<slots>`、`agent_features = 1`、结束维护、在线）。
- [ ] **Step 2: 运行确认失败**。
- [ ] **Step 3: 实现**（SQL 与签名如上）。`noCapacity` 文案对共享商品改为「该时段只有 X 份可用」/「该时段暂无可用份额」。
- [ ] **Step 4: 全量测试、提交** `feat(shared): slot-aware candidate picking, machine state semantics`。

---
### Task 6: lease 槽位化、封顶、出租率加权、换机 / 改派带槽位

**Files:**
- Modify: `internal/rental/leases.go`（`leaseRow.Slot`、`lockLease`、`insertLease(..., slot, ...)`、`nextReservedLower(..., slot, ...)`、`updateLeaseCapped` 的 `cap` 子查询 `AND l.slot = own.slot`）、`internal/rental/admin_queries.go`（`DailyOccupancy` 乘 `1.0 / p.share_slots`）、`internal/rental/migrate.go`、`internal/rental/reassign.go`、`internal/rental/admin_bookings.go`（目标用 `pickedSlot`，`UPDATE instances SET machine_id, slot`）
- Test: `internal/rental/leases_test.go`、`internal/rental/shared_test.go`、`internal/rental/admin_queries_test.go`

- [ ] **Step 1: 写失败的测试**：`TestCapOnlySameSlot`（同机 slot 2 有 reserved，slot 1 的 running 实例续费不被封顶；slot 2 的被封顶 `renew_blocked`）、`TestDailyOccupancyWeightsSharedSlots`（一台 N=4 机器两个槽各占满一天 → 当日出租率 0.5）、`TestMigrateSharedKeepsProduct`（共享实例换机只落到共享机空槽，`slot` 更新，整机不在候选）、`TestReassignBookingToFreeSlot`。
- [ ] **Step 2: 运行确认失败。**
- [ ] **Step 3: 实现。** 所有 `insertLease` 调用点传实际槽位；`refreshLease` 不变。
- [ ] **Step 4: 全量测试、提交** `feat(shared): slot-scoped leases, cap and occupancy weighting, migrate/reassign by slot`。

---
### Task 7: 下单 / 预订校验、Create 入队带端口与份数、商品上架校验

**Files:**
- Modify: `internal/rental/batch.go`（`placeBatch`：读 `ShareSlots`，`MachineID != nil && Slots > 1 → ErrSharedNoMachinePick`；商品 `inactive → ErrProductInactive`）、`internal/rental/provision.go`（`startProvision`）、`internal/rental/results.go` / `renew.go`（Start 指令不带端口，不改）、`internal/rental/calendar.go`（机器日历对共享机 `ErrSharedNoCalendar`）、`internal/fleet/commands.go`（`CreateInstancePayload.SSHPort / ShareSlots`）、`internal/rental/service.go`（`Deps.SSHBasePort int`；`internal/app/app.go` 传 `cfg.UpstreamSSHPort`）
- Test: `internal/rental/batch_test.go`、`internal/rental/calendar_test.go`、`internal/rental/provision_test.go`

**Interfaces:**
- 入队 Create 的 payload：`fleet.CreateInstancePayload{ImageRef, SSHPort: s.d.SSHBasePort + inst.Slot, ShareSlots: slots}`；三处入队点（即时下单 `placeBatch`、预订 `startProvision`、换机 `AdminMigrate`）统一经新 helper `func (s *Service) enqueueCreate(ctx, tx, inst Instance) error`（内部读 `ImageRef` 与 `ShareSlots`）。

- [ ] **Step 1: 写失败的测试**：`TestPlaceSharedRejectsMachinePick`、`TestPlaceInactiveProductRejected`、`TestCreatePayloadCarriesSlotPort`（slot 3 → `ssh_port 2225`、`share_slots 4`；整机 → 2222、1）、`TestMachineCalendarSharedRejected`。
- [ ] **Step 2～4**：运行失败 → 实现 → 全量测试 → 提交 `feat(shared): order validation, create payload with slot port and share_slots`。

---
### Task 8: 协议扩展与网关（features、心跳实例指标、内存压力告警）

**Files:**
- Modify: `api/proto/agent/v1/agent.proto`、`internal/agentpb/*`（`make proto`）、`internal/agentgw/convert.go`（`SshPort` 取 payload、`ShareSlots`）、`internal/agentgw/connect.go`（Hello.features → `machines.agent_features`；Heartbeat.instances → `last_metrics.instances`；压力计数与告警）、`internal/fleet/machines.go`（`Machine.AgentFeatures`、`machineCols`、`UpdateHeartbeatMetrics` 签名加实例指标）
- Create: `internal/agent/features/features.go`
- Test: `internal/agentgw/convert_test.go`、`internal/agentgw/connect_test.go`

**Interfaces:**
- proto（字段号固定）：`Hello.features = 4`、`CreateInstance.share_slots = 7`、`Heartbeat.instances = 2`、新消息 `InstanceMetrics{instance_id=1, mem_current=2, mem_max=3, cpu_percent=4, data_disk_used=5}`。
- `features.SharedSlots uint32 = 1 << 0`。
- 网关：`func (s *Server) handleHello(...)` 写 `agent_features`；机器商品 `share_slots > 1` 且 features 不含位 → `RaiseAlertOnce(warning, shared_agent_missing_feature)`。心跳：`metrics.instances` 原样进 jsonb；对 `mem_max > 0 && mem_current*100 >= mem_max*95` 的实例，连接内计数 +1，≥ 3 → `RaiseAlertOnce(warning, instance_memory_pressure, machine, instance)`；否则清零。
- 稳定文案函数：`rental.AlertInstanceMemoryPressure(id string) string`、`rental.AlertSharedAgentMissingFeature(serial string) string`（放 `internal/rental/alerts.go`）。

- [ ] **Step 1: 改 proto，`make proto`，提交生成物。**
- [ ] **Step 2: 写失败的测试**：`convert` 的 `SshPort` 取 payload（为 0 退回 cfg）、`ShareSlots` 透传；Hello features 落库；三次 ≥95% 才告警、第四次不重复、降到 90% 后清零再三次才再告警；`last_metrics.instances` 结构快照。
- [ ] **Step 3: 实现，全量测试（含 `git diff --exit-code internal/agentpb`），提交** `feat(agent-proto): hello features, create share_slots, per-instance heartbeat metrics; gateway pressure alert`。

---
### Task 9: Agent 多容器与按份配额

**Files:**
- Modify: `internal/agent/runtime/runtime.go`（`Spec.CPUs float64`、`Spec.PidsLimit int`、`Spec.DropNice bool`）、`internal/agent/runtime/docker/docker.go`（`Create` 追加 `--cpus`（`CPUs > 0` 时，格式 `strconv.FormatFloat(.., 'f', 2, 64)`）、`--pids-limit` 取 Spec、`SYS_NICE` 按 `DropNice`；`validateSpec`）、`internal/agent/runtime/fake/fake.go`、`internal/agent/executor/executor.go`（`Command.ShareSlots`、删除 `DataMount`，新增 `Mounts() []metrics.InstanceMount`）、`internal/agent/executor/ops.go`（`spec` 等分、数据盘等分、端口冲突检查）、`internal/agent/metrics/metrics.go`（`Collect(ctx, mounts []InstanceMount) (Metrics, []InstanceMetrics)`，读 cgroup）、`internal/agent/client/client.go`（Hello 带 `features.SharedSlots`、Create 解析 `share_slots`、心跳带实例指标）、`internal/agent/state/store.go`（`Instance.ShareSlots`）、`internal/agent/fakeagent/fakeagent.go`（注入实例指标）、`cmd/spark-agent` 的 `selfcheck`（cgroup v2 与 cpu/memory/pids 控制器、`nproc ≥ 4`）
- Test: `internal/agent/runtime/docker/docker_test.go`（快照：N=1 与 N=4）、`internal/agent/executor/executor_test.go`、`internal/agent/metrics/metrics_test.go`

**Interfaces:**
- `executor.spec(c, cfg)`：`N := max(1, c.ShareSlots)`；`memLimit = (MemTotal − HostReserveBytes) / N`；`shm = min(MemTotal/2, memLimit)`（N=1 与现状一致；N>1 时等于 `memLimit`，再除 2 → `shm = memLimit / 2`）；`CPUs = N > 1 ? float64(runtime.NumCPU()) / N : 0`（`Deps.NumCPU func() int` 可注入）；`PidsLimit = 32768 / N`；`DropNice = N > 1`。
- 数据盘：`c.DataSizeGiB`（或 cfg 默认）`/ N`；`create` 的余量检查用等分后的值。
- 端口冲突：`create` 前遍历 `State.Instances()`，有别的实例 `SSHPort == c.SSHPort` → `fmt.Errorf("SSH 端口冲突：%d 已被实例 %s 使用", ...)`。
- `metrics.InstanceMount{ID, DataDir, CgroupDir string}`；`CgroupDir` 由 executor 用 `docker inspect --format '{{.Id}}'` 的容器 ID 拼 `/sys/fs/cgroup/system.slice/docker-<id>.scope`（`Runtime.Inspect` 的 `Container` 加 `FullID` 字段）。读取 `memory.current`、`memory.max`（`max` 字面量 → 0）、`cpu.stat` 的 `usage_usec`；CPU 百分比 = 两次采样差 / 间隔 × 100，首个样本为 0。读不到的项为 0，永不失败。
- Hello：`Features: features.SharedSlots`。

- [ ] **Step 1: 写失败的测试**：docker 命令快照（N=4 含 `--cpus 5.00 --pids-limit 8192`、`--cap-add` 不含 `SYS_NICE`、`--memory` 为等分值；N=1 与现状完全一致）；`spec` 等分数值；数据盘等分（2048/4 = 512）；端口冲突报错；`metrics` 解析 cgroup 文件（用临时目录造 `memory.current` 等）；fake agent 心跳带实例指标。
- [ ] **Step 2～4**：失败 → 实现 → `go test ./internal/agent/...` 与全量 → 提交 `feat(agent): per-slot quotas, multi-container metrics, port conflict guard`。

---
### Task 10: 接入层按实例端口、`sshtest` 多端口、e2e 钩子造共享机

**Files:**
- Modify: `internal/access/upstream/pool.go`（`Target.Port`，`Dial` 用 `t.Port` 或 `cfg.Port`）、`internal/sshgw/*.go`（路由查实例时填 `Target.Port = inst.SSHPort(base)`）、`internal/ingress/*.go`（路由表项带端口）、`internal/access/sshtest/*.go`（`ListenAlso(port int) error`）、`internal/e2ehooks/hooks.go`（`/hooks/machines` 增加 `product_code`；共享机时为 `base+1..base+N` 起假 sshd）
- Test: `internal/access/upstream/pool_test.go`、`internal/sshgw/*_test.go`、`internal/ingress/*_test.go`

- [ ] **Step 1: 写失败的测试**：`Target.Port` 优先；sshgw 对 slot 2 的实例拨 `lan_ip:2224`；ingress 路由表对共享实例的端口；钩子造 N=4 共享机后 `GET /hooks/agent/status` 看到机器 `agent_features = 1`。
- [ ] **Step 2～4**：实现 → 全量 → 提交 `feat(access): per-instance upstream port; e2e hooks create shared machines`。

---
### Task 11: 后台 API

**Files:**
- Modify: `internal/adminapi/machines.go`（列表 `share_slots / used_slots / agent_features`；详情 `current_instances[]`；新增 `handleSetMachineProduct`）、`internal/adminapi/catalog.go`（`handleSetProductStatus`）、`internal/adminapi/instances.go`（`shared` 筛选、行 `slot`）、`internal/adminapi/server.go`（路由与角色：`PUT /machines/{id}/product` super + stepUp；`PUT /products/{id}/status` super）、`internal/adminapi/views.go`、`internal/fleet/machines.go`（`SetProduct`）、`internal/rental/admin_ops.go`（`AdminSetMachineProduct`）、`internal/catalog/admin.go`（`SetProductStatus`）、`internal/rental/admin_queries.go`（机器详情多实例、实例筛选）
- Test: `internal/adminapi/machines_test.go`、`internal/adminapi/catalog_test.go`、`internal/adminapi/permissions_test.go`（路由表）

**Interfaces:**
- `func (s *Service) AdminSetMachineProduct(ctx, actor audit.Actor, machineID, productID int64) error`：事务内锁机器 → `state != maintenance → ErrInvalidState` → 存在 reserved / active lease → `ErrMachineProductLocked` → `Fleet.SetProduct` → 审计 `machine.product {from, to}`。
- `func (s *Service) SetProductStatus(ctx, actor, id int64, status string) (AdminProduct, error)`，status ∈ {active, inactive}。
- `GET /machines/{id}` 响应：`current_instances: [{slot, instance_id, user_id, state, mem_current, mem_max}]`（保留 `current_instance` 为第一个元素以兼容现有前端，前端 Task 16 再切换）。

- [ ] **Step 1: 写失败的测试**（含权限矩阵：ops 调 `PUT /machines/{id}/product` 403；无动态码 428 / 对应现有 stepUp 码；维护外切换 409 `invalid_state`；有 lease 409 `machine_product_locked`；上架后 `GET /api/v1/products` 出现共享商品）。
- [ ] **Step 2～4**：实现 → 全量 → 提交 `feat(adminapi): machine slots, set machine product, product status`。

---
### Task 12: 控制台 API

**Files:**
- Modify: `internal/api/market.go`（`share_slots`、共享商品 `idle_count` = 空槽数——已由 `AvailableNow` 给出）、`internal/api/instances.go`（`shared / slot / share_slots / quota / usage`；`usage` 从 `machines.last_metrics.instances` 取本实例项，需要 `rental` 提供 `InstanceUsage(ctx, instanceID)`）、`internal/api/booking.go`（`machine_id` + 共享商品 → 400）、`internal/api/server.go`（机器日历共享机 409）
- Test: `internal/api/api_test.go`、`internal/api/booking_test.go`、`internal/api/view_internal_test.go`

- [ ] **Step 1: 写失败的测试**：产品视图字段；实例视图对共享实例的 `quota`（28 GiB / 5 / 0.5 TiB）与 `usage` 为空 / 有值两种；预订指定机器 400 `shared_no_machine_pick`；机器日历 409 `shared_no_calendar`。
- [ ] **Step 2～4**：实现 → 全量 → 提交 `feat(api): shared product and instance views, booking guards`。

---
### Task 13: 集成测试与并发测试

**Files:**
- Create: `internal/app/shared_e2e_test.go`
- Modify: `internal/rental/concurrency_test.go`、`internal/rental/shared_test.go`

- [ ] 用例（规格 §12「集成」一行逐条）：N=4 共享机 4 单落 4 槽、第 5 单 `no_capacity`；一单 4 台落同机；释放一槽 `state` 仍 allocated、全释放 idle；预订共享槽位到点开通；改派到另一台共享机空槽；同槽位下一预订挡住续费；`AdminMigrate` 保持商品；features=0 不进候选；每实例 Create 的 `ssh_port`；`TestConcurrentSharedSlotsNoOversell`（20 个 goroutine 抢 1 台 4 槽，恰好 4 成功，其余 `no_capacity`，无排他 / 唯一冲突泄漏到调用方）；fake Agent 对两个实例回报后对账不互相误判。
- [ ] 全量测试、提交 `test(shared): integration and concurrency coverage`。

---
### Task 14: 真机检查点 A（后端 + API，curl）

**Files:**
- Create: `docs/superpowers/acceptance-plan10.md`（先写「检查点 A」一节）

- [ ] 构建 Agent（`make agent`），协调人在 221 上升级（后台重新生成接入命令 → 安装）；后台机器详情 `agent_features = 1`。
- [ ] 后台（curl，super + 动态码）：机器进维护 → `PUT /machines/{id}/product` 为 `spark-shared-4` → 结束维护；`PUT /products/{id}/status active`。
- [ ] 两个测试用户各下单 1/4（控制台 API curl）→ 规格 §12.1 第 2～7 项按顺序验证并记录（命令 + 输出摘录）。第 4 项用 S11 的 `/poc/alloc`（镜像 `spark/poc-shared:1` 已在机器上；经网关 SSH 进容器后 `docker cp` 不可用，改为在 Mac 上 `scp` 二进制经网关 sftp 上传到 `/root/data/`）。
- [ ] 发现的缺陷按计划 6 的格式记录（编号 A0…）、修复、复测后再继续；提交 `docs: plan 10 real-machine checkpoint A`。

---
### Task 15: 控制台页面

**Files:**
- Modify: `web/shared/src/types/catalog.ts`（`Product.share_slots`、`ProductSpec.shared?`）、`web/shared/src/types/instance.ts`（`shared, slot, share_slots, quota, usage`）、`web/shared/src/labels.ts`、`web/console/src/views/MarketView.vue`（共享标签与说明、`idle_count` 文案「可用 X 份」）、`web/console/src/components/RentDialog.vue`（共享商品隐藏指定机器）、`web/console/src/views/InstanceDetailView.vue`（规格行、内存用量进度条 `el-progress`，≥ 90% `status="warning"`）、`web/console/src/views/HelpView.vue`（「共享机」一节，文案按 PoC 记录 §3.3 / P6 结果）
- Test: `web/console/src/__tests__/*.spec.ts`（Vitest：市场卡片渲染共享标签；RentDialog 对共享商品不渲染机器选择；实例详情用量条阈值）

- [ ] 写 Vitest 失败用例 → 实现 → `cd web && npm test && npm run build` → 提交 `feat(console): shared machine cards, quota and usage, help section`。
- [ ] 文案清单（直接可用）：标签「共享」；说明「GPU 算力共享、不保证独占；内存 / CPU / 数据盘为独享配额」；规格「共享机 1/4 · 内存 28 GiB · 5 核 · 数据盘 512 GiB」；用量「内存 {used} / {max}」；帮助页要点：配额由容器 cgroup 强制，超限进程会被系统终止或 `cudaMalloc` 失败；`nvidia-smi`、`free`、`cudaMemGetInfo` 显示的是整机数值；同机其他用户可能看到你的 GPU 进程名（按 P6 实测决定是否保留）。

---
### Task 16: 后台页面

**Files:**
- Modify: `web/shared/src/types/admin.ts`、`web/admin/src/views/MachinesView.vue`（「商品 / 槽位」列：`共享机 1/4 · 2/4`）、`web/admin/src/views/MachineDetailView.vue`（槽位表替代「当前实例」；「切换商品」按钮：维护中可用，选择商品 → `TotpDialog` → `PUT /machines/{id}/product`；非维护时禁用并提示）、`web/admin/src/views/CatalogView.vue`（「份数」列、「上架 / 下架」开关）、`web/admin/src/views/InstanceDetailView.vue`（槽位、用量）、`web/admin/src/views/InstancesView.vue`（「共享」筛选）
- Test: Vitest 组件测试（槽位表渲染 4 行、切换按钮禁用逻辑、份数列）

- [ ] 失败用例 → 实现 → `npm test && npm run build` → 提交 `feat(admin): machine slots and product switch, product status toggle`。

---
### Task 17: Playwright 用例与覆盖表

**Files:**
- Modify: `web/e2e/helpers/hooks.ts`（`createMachines({count, product_code})`）、`web/e2e/console/market.spec.ts`（`C-MKT-09` 共享卡片与「可用 4 份」；`C-MKT-10` 下单共享机成功；`C-MKT-11` RentDialog 无「指定机器」）、`web/e2e/console/instance-detail.spec.ts`（`C-DET-26` 规格行；`C-DET-27` 用量条（钩子 `/hooks/agent` 注入 `instance_metrics`））、`web/e2e/admin/machines.spec.ts`（`A-MCH-10` 槽位表；`A-MCH-11` 维护中切换商品成功；`A-MCH-12` 有实例时切换 409 提示）、`web/e2e/admin/catalog.spec.ts`（`A-CAT-04` 份数列；`A-CAT-05` 上架开关）、`web/e2e/coverage.json`
- Note: 共享商品在 e2e 里需先上架：用 `A-CAT-05` 的接口或在 `/hooks/machines` 造共享机时顺带把该商品置 `active`（钩子加 `activate_product: true`）。

- [ ] 写用例 → `cd web && npx playwright test <files>` 通过 → `npm run e2e:coverage`（若有）核对覆盖表 → 提交 `test(e2e): shared machine console and admin coverage`。

---
### Task 18: 真机验收 B 与记录

**Files:**
- Modify: `docs/superpowers/acceptance-plan10.md`

- [ ] 规格 §12.1 全部 10 项（含页面）在 221 上执行；每项记录命令 / 页面观察、结果、缺陷编号。重启项（第 9 项）由协调人执行 `sudo systemctl reboot`。第 10 项切回整机后要再下单一次整机验证端口 2222 与内存上限。
- [ ] 发现的缺陷修复、审查、复测；最终版本复测段落。
- [ ] 提交 `docs: plan 10 real-machine acceptance record on 192.168.1.221`。

---
### Task 19: 收尾

**Files:**
- Modify: `README.md`（`SPARK_UPSTREAM_SSH_PORT` 改为「基准端口，共享实例为基准 + 槽位」；`SPARK_AGENT_DATA_SIZE_GIB` 加「共享机按份等分」；部署顺序提到 `agent_features`）、`docs/image-spec.md`（「共享机运行时环境」：内存 / CPU / pids 配额、无 `SYS_NICE`、`nvidia-smi` 数值含义）
- Create: `docs/superpowers/followups-plan10.md`（规格 §15 遗留 + PoC / 验收中的观察 + 「HAMi-core 待 CUDA 13 支持后再评估」）

- [ ] `make check`（或 `go vet ./... && go test ./... && cd web && npm test && npm run build`）全绿。
- [ ] 提交 `docs: plan 10 followups, README and image-spec updates`。
