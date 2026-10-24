# 计划 10（共享机）遗留与观察

计划 10 的真机 PoC 结论是**路线 C no-go、MPS no-go，共享机商品搁置**（`acceptance-plan10.md`，规格裁定 S13 / S14）。本分支只交付 Agent 多容器与按实例端口的基础设施。下面是搁置的内容、复活条件和实现中留下的小问题。

## 搁置的共享机商品（规格 §2、§4～§12 未实现）

- 没有实现的部分：槽位（`instances.slot` / `machine_leases.slot`、按槽位的排他约束）、`products.share_slots`、`spark-shared-2 / spark-shared-4` 两个商品、`machines.agent_features` 列与 `shared_agent_missing_feature` 告警、`instance_memory_pressure` 告警、后台「切换机器商品 / 商品上下架」、控制台共享卡片 / 配额 / 用量、e2e 钩子造共享机、覆盖表 `C-MKT-09～11`、`C-DET-26～27`、`A-MCH-10～12`、`A-CAT-04～05`。设计和实施计划仍然有效：复活时从计划 Task 4 往下执行。
- **迁移编号**：`00023` 保留未用。goose 不接受乱序迁移，计划 11 的 `00024` 一旦落库，`00023` 就再也不会执行。所以复活时要用当时最大编号之后的新号，并把规格 §2 和计划里的 `00023` 一并改掉。
- **复活条件**：租户级的 GPU 内存限额必须由内核或驱动强制执行，容器内 root 绕不过。候选做法：
  1. NVIDIA 在 GB10 上让 `cudaMalloc` 的页计入分配进程的 memcg。重测 PoC P1 即可判定，工具在 `deploy/poc/shared/`。
  2. MIG 或类似的硬件分区（GB10 目前不支持）。
  3. MPS 加「每容器只允许一个 GPU 进程」。这一条没有可强制的手段：`--pids-limit` 管不住同时有几个 GPU 上下文。若负责人接受「合作式」限额，就按裁定 S13 的「推翻」路径执行，并且必须同时：
     - 把计算模式设为 `EXCLUSIVE_PROCESS`（否则不设 `CUDA_MPS_PIPE_DIRECTORY` 就能绕过）；
     - 由 Agent 托管 `nvidia-cuda-mps-control`；
     - 增加 `mps_restarted` 告警；
     - 在文案里写明「同机任一用户的 GPU 致命错误会使全机 GPU 任务失效」（PoC P3：A 在 `t=19` 被连累，恢复不到 1 秒）。
- **HAMi-core**：当前主干（`cdb5260`，2026-10-23）在 CUDA 13 / arm64 上能编出来，`CUDA_DEVICE_MEMORY_LIMIT` 能拦住分配，还会把 `cudaMemGetInfo` 改写成配额值；但 `unset LD_PRELOAD` 就失效。它可以作为「软限额 + 让程序看到配额」叠加在将来的强制方案之上，不能单独用。
- **PoC 中可复用的事实**：
  - P2：并发两个上下文时，吞吐之和为独跑的 86.8%（无 MPS）和 96.8%（MPS 50% + 50%）。
  - P4：两个上下文各分到 50.0%。
  - P6：容器内 `nvidia-smi` 看不到邻居的 GPU 进程。
  - P7：`--cpus 5` 实测 496～498%，对邻居 GPU 吞吐的影响小于 1%。
  - 托管内存计入 cgroup，超限时被 memcg OOM 杀掉。

## 已交付的基础设施里的小问题

- 新 Agent 的 `Hello.features` 默认带 `SharedSlots`（表示 Agent 有按份配额的能力）。平台没有读取它（没有 `agent_features` 列）。复活时平台必须同时按商品上架状态把关，不能只看这一位（PoC 判定的是隔离不够，不是 Agent 不支持）。
- 平台目前在 Create 指令里只发 `share_slots = 1`。Agent 只接受 0 / 1 / 2 / 4，其它值报「不支持的份数」。按份配额的路径（`--cpus`、`--pids-limit` 等分、去掉 `SYS_NICE`、数据盘等分）只经过单元测试和真机工具 `deploy/poc/shared/multicheck` 验证，没有经过平台的端到端流程。
- 每实例指标里的 0 表示「未知」：重启后 cgroup 路径尚未解析、首个 CPU 样本、数据盘未挂载。以后有人消费这些数字（例如用量条、告警）时，需要加一个「有效」标记。
- `NumCPU` 默认取 Go 的 `runtime.NumCPU()`，即 Agent 进程可用的 CPU 数。若以后给 `spark-agent.service` 设 `CPUAffinity`，它会小于 `nproc`。目前只影响按份配额，而按份配额没有启用。
- 数据盘是否挂载，用「目录与父目录的 `st_dev` 不同」来判断。以后若改成 bind mount 或同设备挂载，会把已挂载的盘判成未挂载、报 0（只影响指标，不影响开通）。
- 端口冲突兜底在 `docker inspect` 出错（dockerd 短暂不可用）时会让开通失败，报错原文是「检查 SSH 端口 … 占用失败」。目前这样处理，是因为在这种状态下后续的 docker 步骤本来也会失败；若误报偏多，可以改成「出错时放行」。
- appdeploy 构造 `upstream.Target` 时没有带 `Port`，用的是连接池的默认端口。槽位上线时要跟 sshgw / ingress 一样从 rental 取实例端口。`ListIngressRoutes` 目前用裸的 `Instance{ID}` 算端口，届时也要带上槽位。
