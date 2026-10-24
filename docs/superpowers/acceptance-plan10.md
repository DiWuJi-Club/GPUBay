# 计划 10（共享机）真机记录：PoC 与验收

- 机器：192.168.1.221 `spark-3479`，NVIDIA GB10（compute capability 12.1），驱动 580.178.04 / CUDA 13.0，内核 `7.0.0-1019-nvidia`，Docker 29.6.2（systemd cgroup 驱动、cgroup v2），`MemTotal = 130660974592` 字节，20 核。
- 时间：2026-10-25 01:10～01:32（机器时区 +08:00）。PoC 期间 `spark-agent` 已停止，结束后恢复（`active`，连回 192.168.1.218:9443）。
- 工具：`deploy/poc/shared/`（`alloc` / `burn` / `fault`，nvcc `-arch=sm_121 -cudart static`，不依赖 cuBLAS；镜像 `spark/poc-shared:1`，基于 `nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04`）。`burn` 是自写的 4096×4096 分块 SGEMM，单容器独跑约 3.6 TFLOPS（不是 GB10 的峰值，只用来比相对吞吐）。
- 容器参数与规格 §3.1 一致：`--gpus all --network spark0 --memory Xg --memory-swap Xg --shm-size X/2g --cpus 5 --pids-limit 8192 --oom-score-adj 500 --cap-drop SYS_NICE --cap-add IPC_LOCK --cap-add SYS_PTRACE --ulimit memlock=-1:-1`。第二轮另加 `-v /tmp/spark-mps/pipe:/tmp/nvidia-mps -e CUDA_MPS_PIPE_DIRECTORY=/tmp/nvidia-mps --ipc=host`（脚本的 `POC_EXTRA_ARGS`）。
- 原始日志存放在协调人的会话临时目录，没有入库；下文摘录的是原文数字。

## PoC 结论

**路线 C（cgroup）no-go → MPS no-go → 共享机商品搁置**（规格 §3.5 第 2 条）。
- 路线 C 没过 P1：在 `--memory 32g` 的容器里，`cudaMalloc` 加写入一共分配到 48 GiB，没有被拦下。容器 cgroup 的 `memory.current` 只涨到约 162 MiB，GB10 上 `cudaMalloc` 的页不计入容器 cgroup。
- MPS 字面上过了第二轮各项：pinned limit 在 27 GiB 处拦下分配，P2 / P5 通过，P3 致命错误连累邻居但不到 1 秒就恢复。但这个限额**按进程计、不按容器计**：同一容器起两个进程，每个都能拿到 27 GiB，合计 54 GiB，配额是 28 GiB。容器内 root 可以随意起进程，所以「按内存配额收费」的依据不成立（规格 §1.1 的第一条标准就是「限额强制、租户绕不过」）。另外，在默认计算模式下，租户只要不设 `CUDA_MPS_PIPE_DIRECTORY` 就能完全绕开 MPS；必须改成 `EXCLUSIVE_PROCESS` 才能堵住这条路。详见第二轮。
- 本分支因此只保留「Agent 多容器 + 按实例端口」的基础设施，规格 §2、§4～§12 不实现。`spark-shared-2 / spark-shared-4` 两个商品与迁移 00023 都不落库。

## 第一轮：路线 C（驱动时间分片 + cgroup v2），无 MPS

| # | 实验 | 关键原始输出 | 判定 |
|---|---|---|---|
| P1 | A：`--memory 32g`，`alloc 48 1 --touch`；B：`--memory 56g`，同时跑 `burn 60` | A：`allocated=48 free=62521917440 total=130660974592 cg_current=170291200`，然后 `reached total`，`exit=0`，`OOMKilled=false`。开始时 `free=115016552448`，GPU 侧可用内存少了约 48.9 GiB，容器 `memory.current` 只从 67 MiB 涨到 162 MiB（每 GiB 约 2 MiB，是页表等元数据）。宿主机 `MemAvailable` 最低 72083588 kB（开始时 123724928 kB）。dmesg 里这一步没有任何 OOM 记录。B：`avg_gflops=3567.9`，`t_lines=60 max_t_gap=1` | **不通过**：GPU 分配不计入 cgroup |
| P1b | 同 P1，加 `--managed`（`cudaMallocManaged`） | A 在 `allocated=31 … cg_current=33419153408` 之后被杀：`exit=137`，`OOMKilled=true`。dmesg：`UVM GPU1 BH invoked oom-killer: gfp_mask=…__GFP_ACCOUNT`、`oom-kill:constraint=CONSTRAINT_MEMCG … task=alloc`、`Memory cgroup out of memory: Killed process 851618 (alloc) … file-rss:33461592kB`，随后是 `NVRM: Xid … 31 … MMU Fault`（属于被杀的进程本身）。B 跑完，但 A 分配期间吞吐下降：`avg_gflops=2528.4 min_gflops=1355.8` | 通过（托管内存计入 cgroup） |
| 基线 | 单容器 `burn 60` | `avg_gflops=3626.7`，`min_gflops=3612.5` | — |
| P2 | 两个容器各 `--memory 56g`，同时 `burn 60` | `1571.4 + 1575.2 = 3146.6`，是基线的 **86.8%**。宿主机 `nvidia-smi` 看到两个 `burn` 进程（PID 855027 / 855034），两边都 `exit=0` | 通过（≥ 85%） |
| P3 | A 跑 `burn 120`；20 秒后 B 跑 `fault` | B：`fault: cudaErrorIllegalAddress`，`exit=5`。A：`t_lines=120 max_t_gap=1 min_gflops=3269.0 avg_gflops=3595.8`，`exit=0`。之后宿主机 `nvidia-smi` 正常 | 通过 |
| P4 | 两个容器同时 `burn 60`（公平性，只记录） | 1568.8 / 1567.9，各占 **50.0%** | 记录 |
| P5 | 两个容器各 `--memory 56g`，同时 `alloc 60 1 --touch` | **未执行**。P1 已经证明 `cudaMalloc` 不受 cgroup 约束，两个容器合计 120 GiB 会超过整机 121.7 GiB，宿主机会被 OOM；机器没有 BMC，只能靠 PDU 重启 | 视为不通过（由 P1 推出） |
| P6 | P4 运行中，在 B 里跑 `nvidia-smi` 和 `ps aux` | 容器内 `nvidia-smi` 进程表**只有自己**（`1 C burn 362MiB`，`--query-compute-apps` 也只返回 `1, burn, 362 MiB`），看不到邻居。`ps aux` 只有 `burn 60` 和 `ps` 本身 | 记录：邻居的 GPU 进程不可见 |
| P7 | A：`--cpus 5` 跑 `stress --cpu 20 --timeout 60`；B 同时跑 `burn 60` | `docker stats` 里 A 一直在 496.0%～498.3%。B：`avg_gflops=3616.8 min_gflops=3596.6`，比基线低 0.3% | 通过 |

**第一轮判定：no-go**（P1 不通过，按规格 P5 也不成立）。

### §3.3 附加观察
- 不用 MPS 时，`cudaMemGetInfo` 返回的是整机数值（`total=130660974592`，`free` 等于整机可用量），与容器配额无关。用 MPS 时，`free` 反映的是 pinned limit：开始时 `free=30006898608`，约 27.9 GiB。
- 超限的表现：`cudaMallocManaged` 会被内核 memcg OOM 直接杀掉（`exit=137`，事件来自 `UVM GPU1 BH` 的缺页）；`cudaMalloc` 根本不会超限，因为它不计入 cgroup。
- `memory.current`：随托管内存同步增长（约 1 GiB/GiB），不随 `cudaMalloc` 增长（约 2 MiB/GiB）。所以规格 §1.2 第 13 条的 `instance_memory_pressure` 告警对 `cudaMalloc` 无效。
- 实际 cgroup 路径是 `/sys/fs/cgroup/system.slice/docker-<64 位 id>.scope`（dmesg 的 `oom_memcg=` 已证实），与 Agent 的 `Mounts()` 用的路径一致。

## 第二轮：CUDA MPS（规格 §3.5 第 1 条）

宿主机用 root 执行：`nvidia-cuda-mps-control -d`（`CUDA_MPS_PIPE_DIRECTORY=/tmp/spark-mps/pipe`、`CUDA_MPS_LOG_DIRECTORY=/tmp/spark-mps/log`），`set_default_device_pinned_mem_limit 0 28G`，`set_default_active_thread_percentage 25`；P2-t50 及之后改为 50。控制守护进程版本 `13000`，服务进程命令行是 `nvidia-cuda-mps-server -force-tegra`。

| # | 实验 | 关键原始输出 | 判定 |
|---|---|---|---|
| P1 | `--memory 32g`，`alloc 48 1 --touch` | `cudaMalloc failed at 27: cudaErrorMemoryAllocation`，`exit=3`，`OOMKilled=false`。开始时 `free=30006898608`；失败时 `cg_current=102682624`（约 98 MiB）。邻居 B `burn 60`（25% 线程）：`avg_gflops=1418.3` | 通过（28G 限额，失败点在 27 GiB） |
| P1b | 同上，加 `--managed` | `cudaMalloc failed at 27: cudaErrorMemoryAllocation`，`cg_current=29092782080` | 通过 |
| 基线 | MPS 下单容器 `burn 60`（25% 线程） | `avg_gflops=1427.2`（不用 MPS 时是 3626.7） | — |
| P2-t25 | 两个容器同时 `burn 60`，各 25% 线程 | `1411.6 + 1411.5 = 2823.1`，是不用 MPS 时基线的 77.8%、MPS 下单容器的 198%。`nvidia-smi` 显示 `nvidia-cuda-mps-server` 加两个 `M+C burn` | 两边都跑完。25% × 2 只用了一半 SM，所以不用这组判吞吐 |
| P2-t50 | 两个容器同时 `burn 60`，各 50% 线程 | `1756.9 + 1754.2 = 3511.1`，是不用 MPS 时基线的 **96.8%** | 通过 |
| P3 | A `burn 120`；20 秒后 B 跑 `fault` | A 在 `t=19` 后报 `cuda error: cudaErrorIllegalAddress (cudaEventSynchronize…)`，`exit=4`，被连累。MPS 服务日志：`Client {PID: 867532…} encountered a fatal GPU error … All clients belonging to error trigging process … will be torn down … The following devices will be reset: 0`。控制进程与服务进程都没有退出（PID 866674 / 867092 不变）。恢复测试：`fault` 结束后立刻启动新客户端 `burn 10`，间隔不到 1 秒，跑出 `avg_gflops=2815.5`，`rc=0` | 通过（规格允许连累，要求 30 秒内恢复，实测不到 1 秒） |
| P5 | 两个容器各 `--memory 56g`，同时 `alloc 60 1 --touch --hold 20` | 两边都是 `cudaMalloc failed at 27`。宿主机 `MemAvailable` 最低 78041204 kB。dmesg 没有 OOM / Xid / oops，`ssh` 服务 `active` | 通过 |

**绕过测试**（规格 §1.1 把「限额是否强制」列为第一标准，所以加做这一组）：

| # | 做法 | 结果 |
|---|---|---|
| B1 | 客户端设 `CUDA_MPS_PINNED_DEVICE_MEM_LIMIT=0=60G` 试图调高限额 | 仍在 27 GiB 处失败，调不高 ✔ |
| B2 | 计算模式 `Default`，容器不挂管道、不设环境变量 | `alloc 40 1 --touch`：`reached total`，**绕过** |
| B3 | 计算模式 `Default`，挂了管道，但 `CUDA_MPS_PIPE_DIRECTORY=/nonexist` | `reached total`（40 GiB），**绕过** |
| X1 | `nvidia-smi -c EXCLUSIVE_PROCESS`（GB10 支持），正常的 MPS 客户端 | 在 27 GiB 处失败 ✔ |
| X2 / X3 | `EXCLUSIVE_PROCESS` 下，环境变量指向别处，或完全不用 MPS | `cuda error: cudaErrorDevicesUnavailable (cudaFree(0) …)`，堵住了 ✔ |
| X4 | `EXCLUSIVE_PROCESS` 下，两个 MPS 客户端同时 `burn 10` | 1775.7 / 1760.5，正常 |
| **M1** | `EXCLUSIVE_PROCESS` + 28G 限额，**同一个容器**（`--memory 32g`）里同时起两个 `alloc 30 1 --touch` | 两个进程都到了 `allocated=27`，然后才 `cudaMalloc failed at 27`。**同一容器合计 54 GiB**，`cg_current` 约 200 MB |

**第二轮判定：no-go。** pinned limit 只约束单个 MPS 客户端进程，约束不了一位租户（一个容器）。容器内 root 起 k 个 GPU 进程就能用 k × 28 GiB。四位 1/4 租户各起两个进程，就是 8 × 27 = 216 GiB，超过整机 121.7 GiB，会造成宿主机 OOM。这个情形没有实测：没有 BMC，太危险。规格 §3.5 字面列出的各项（单进程 P1、P2、P5、P3 放宽）都通过了，但这些项要保护的前提是「每位租户的 GPU 内存被限制在配额内」，而 M1 直接否定了这个前提。另外，任何一位租户的致命错误都会让全机所有租户的 GPU 上下文失效（P3），故障域扩大到整机。两条加在一起，共享机不能按「独享内存配额」出售。这个判定写进了规格「代负责人裁定」S13。

## 附录 A：HAMi-core 时间盒（只记录）

- 源码：`git clone --depth 1 https://github.com/Project-HAMi/HAMi-core`（机器能访问 github.com），commit `cdb5260760f16221c027ffeedb747c38a2f4deb5`（2026-10-23）。
- 构建：在 `nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04`（arm64）里装 `git cmake build-essential` 后执行 `./build.sh`，`build_rc=0`，得到 `build/libvgpu.so`（852552 字节）。**与规格 §0 依据的第三方说法「不支持 CUDA 13」相反：当前主干在 CUDA 13 / arm64 上能编出来。**
- H1：`LD_PRELOAD=/libvgpu.so CUDA_DEVICE_MEMORY_LIMIT=8g alloc 16 1 --touch` 的输出是 `start free=8411168768 total=8589934592`，然后 `cudaMalloc failed at 7: cudaErrorMemoryAllocation`，HAMi 日志 `Device 0 OOM 8768700416 / 8589934592`。能拦截，`cudaMemGetInfo` 也被改写成 8 GiB。
- H2：加 `--managed`，同样在 7 GiB 处失败。
- H3：容器内先 `unset LD_PRELOAD` 再跑 `alloc 16`，结果 `reached total`，**限额失效**（与预期一致）。
- 结论：不作主路线（规格 §1.1：`LD_PRELOAD` 对容器内 root 没有强制力）。它可以叠加为「软限额 + 让 `cudaMemGetInfo` 显示配额」，记入遗留。

## 机器复原（PoC 结束时）

`docker ps -a` 为空；`/sys/fs/cgroup/system.slice/` 下没有 `docker-*.scope`；MPS 已 `quit`，没有 `nvidia-cuda-mps*` 进程，`/tmp/spark-mps` 已删除；计算模式已恢复为 `Default`；`/tmp/poc`、`/tmp/poc-src` 已删除。保留了镜像 `spark/poc-shared:1` 和 `nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04`，验收时复用。`spark-agent` 为 `active`，版本 `9f0e8f0`。
