# 共享机 PoC 工具（计划 10 Task 1～2）

验证「GB10 统一内存下 CUDA 分配受容器 cgroup `memory.max` 约束」等前提（规格 `docs/superpowers/specs/2026-10-24-plan10-shared-machine-design.md` §3）。结果记录在 `docs/superpowers/acceptance-plan10.md`（原计划的 `acceptance-plan10-poc.md` 合并进了该文件）。

## 文件

| 文件 | 说明 |
|---|---|
| `alloc.cu` | `alloc <total_gib> <step_gib> [--managed] [--touch] [--hold <sec>]`：逐 `step_gib` 调 `cudaMalloc`（`--managed` 用 `cudaMallocManaged`），`--touch` 时每块 `cudaMemset` 全量写入并同步。每步打印 `allocated=<GiB> free=<bytes> total=<bytes> cg_current=<bytes> ts=<epoch>`（`free/total` 来自 `cudaMemGetInfo`，`cg_current` 是容器内 `/sys/fs/cgroup/memory.current`，读不到为 -1）。分配失败打印 `cudaMalloc failed at <GiB>: <错误名>`，**退出码 3**；其它 CUDA 错误（如 memset）`cuda error: <名>`，**退出码 4**；达到 total 打印 `reached total`，保持 `--hold` 秒后退出码 0。参数错误退出码 2。 |
| `burn.cu` | `burn <sec> [n]`：自带共享内存分块 SGEMM（默认 4096×4096，32×32 分块），连续 launch，cudaEvent 计时。每秒打印 `t=<s> gflops=<x> kernel_ms=<avg>`，结束打印 `avg_gflops=<x>`，退出码 0；CUDA 错误 `cuda error: <名>`，**退出码 4**。GPU 停顿时 `t=` 会跳号。 |
| `fault.cu` | `fault`：kernel 写地址 `0x10`，`cudaDeviceSynchronize` 后打印 `fault: <错误名>`（预期 `cudaErrorIllegalAddress`），**退出码 5**；同步竟成功则打印 `fault: no error`，退出码 1。 |
| `Dockerfile` | `nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04` + `stress procps`（apt 走 ports.ubuntu.com），`nvcc -O2 -arch=sm_121 -cudart static` 编出 `/poc/alloc`、`/poc/burn`、`/poc/fault`（`/poc` 在 PATH 里），无 ENTRYPOINT。 |
| `run-poc.sh` | 每个实验一个子命令，见下。 |

**计算能力：** GB10 实测 `nvidia-smi --query-gpu=compute_cap --format=csv` = **12.1** → `-arch=sm_121`（驱动 580.178，CUDA 13.0，arm64）。换机型时用 `docker build --build-arg CUDA_ARCH=sm_xxx` 覆盖。

**只依赖驱动：** 三个程序静态链接 cudart、不用 cuBLAS，可以直接 `docker cp` / `scp` 进只基于 CUDA base 镜像的用户容器运行（验收第 4 步用）。

## 使用

```sh
scp -r deploy/poc/shared imac@192.168.1.221:/tmp/poc-src
ssh imac@192.168.1.221
/tmp/poc-src/run-poc.sh build
docker run --rm --gpus all spark/poc-shared:1 alloc 1 1     # 冒烟：打印一步并 reached total
/tmp/poc-src/run-poc.sh p1     # 其余步骤同理
```

- 以 docker 组用户运行，脚本本身不需要 root。环境变量：`POC_IMAGE`（默认 `spark/poc-shared:1`）、`POC_NET`（默认 `spark0`，由 Agent 创建）、`POC_LOGDIR`（默认 `/tmp/poc`）。
- 每个容器的参数与 Agent 一致：`--gpus all --network spark0 --memory Xg --memory-swap Xg --shm-size X/2g --cpus 5 --pids-limit 8192 --oom-score-adj 500 --cap-drop SYS_NICE --cap-add IPC_LOCK --cap-add SYS_PTRACE --ulimit memlock=-1:-1`，名字 `poc-a` / `poc-b`。为了能看 OOM 状态不用 `--rm`：容器退出后把 `exit=<码>` 与 `inspect: OOMKilled=… ExitCode=…` 写进日志再 `docker rm`。
- 程序非零退出是预期结果之一（alloc 3、fault 5、被 OOM-kill 为 137），脚本只记录不中止。每步结束打印摘要（关键行、`t=` 行数 / 最大跳号 / 最小吞吐、solo 基线）。
- 日志：`/tmp/poc/<step>-<a|b>.log`；采样：`p1-sampler.log`、`p1b-sampler.log`（每 1 s）、`p5-sampler.log`（每 2 s），每行 `ts`、宿主机 `MemAvailable`、各容器 cgroup 的 `memory.current / memory.peak / oom_kill`（从 `/sys/fs/cgroup/system.slice/docker-<id>.scope/` 读）；`p2/p3/p4-nvsmi.log`（宿主机 `nvidia-smi`）、`p6-nvsmi.log / p6-ps.log / p6-ps-count.log`、`p7-stats.log`（每 5 s `docker stats`）。

## 步骤与通过标准（规格 §3.2）

| 步骤 | 做什么 | 通过标准（go） | 失败含义 |
|---|---|---|---|
| `p1` P1 **GPU 分配计入 cgroup** | A `--memory 32g` 跑 `alloc 48 1 --touch`；B `--memory 56g` 同时 `burn 60` 作邻居；宿主机每秒采样 | 在累计 ≤ 32 GiB（±2 GiB）处被终止：`cudaMalloc` 返回 `cudaErrorMemoryAllocation` **或**进程被 OOM-kill（`dmesg` 可见 `memory.max` 的 oom 记录）；宿主机与另一容器不受影响 | 若能分配到 48 GiB 成功 → GPU 分配不计入 cgroup → **路线 C no-go**，转 §3.5 |
| `p1b` P1b 托管内存 | 同上，`alloc` 加 `--managed` | 同 P1 | 同 P1（任一变体不计入即 no-go） |
| `solo` P2 基线 | 单容器 `--memory 56g` 跑 `burn 60` | 记录独跑 `avg_gflops`，供 P2 / P7 对比 | — |
| `p2` P2 **并发可用** | 两个容器各 `--memory 56g`，同时 `burn 60`；15 s 时宿主机 `nvidia-smi` | 两者都正常完成，`nvidia-smi` 看到两个进程；吞吐之和 ≥ 单容器独跑的 85% | 两者不能同时建立上下文 → no-go |
| `p3` P3 **故障隔离** | A 跑 `burn 120`，20 s 后 B 跑 `fault`；B 退出后宿主机 `nvidia-smi` | B 退出、A 继续到 120 秒且吞吐无长时间中断（> 5 秒的中断视为失败，即摘要里 `max_t_gap` > 5）；宿主机 `nvidia-smi` 仍正常 | A 被连累或驱动需重置 → no-go |
| `p4` P4 **公平性（记录，不判定）** | 两个容器同时 `burn 60`（顺带做 P6 采集） | 记录各自吞吐占比；期望每个在 35%～65% 区间 | 严重不公平只写进产品文案与遗留，不阻塞 |
| `p5` P5 **宿主机安全** | 两容器各 `--memory 56g` 同时 `alloc 60 1 --touch --hold 20`；宿主机每 2 s 采样 | 两者都在配额处被限制；宿主机 `MemAvailable` 始终 > 6 GiB、Agent / sshd 不被 OOM；无内核 oops | 宿主机 OOM 或 GPU 驱动报错 → no-go |
| `p6` P6 **信息泄漏（记录）** | 同 p4 负载，运行中 `docker exec poc-b nvidia-smi`、`ps aux`、`ps aux \| wc -l` | 记录能否看到邻居的 GPU 进程（PID / 进程名）；`ps` 看不到邻居（pid ns） | 只记录，写进文案「同机用户可见 GPU 进程列表」或在镜像规范里建议 |
| `p7` P7 **CPU 配额** | A `--cpus 5` 跑 `stress --cpu 20 --timeout 60`；B 同时 `burn 60`；每 5 s `docker stats` | `docker stats` CPU ≈ 500%；邻居容器的 `burn` 吞吐波动 < 15% | 不通过只记录（CPU 配额是 cgroup 标准功能，预期通过） |
| `clean` | `docker rm -f poc-a poc-b`，删 `/tmp/poc` | — | — |

**go**：P1、P1b、P2、P3、P5 全部通过。**no-go**：任一不通过。

§3.3 附加记录：`alloc` 每行的 `free/total`（`cudaMemGetInfo` 是否返回整机值）；OOM 表现（退出码 3 = `cudaMalloc` 报错，137 + `OOMKilled=true` = 被杀，4 = memset 等其它 CUDA 错误）；`cg_current` 与 sampler 里的 `memory.current` 是否随 GPU 分配增长。

## 需要 root 的步骤（协调人用 `sudo -S` 执行，密码不落盘）

- PoC 开始前 `sudo systemctl stop spark-agent`（不让 Agent 对账 / 清理容器），全部结束并 `run-poc.sh clean` 后 `sudo systemctl start spark-agent`。
- 每个实验后 `sudo dmesg | tail -50`（P5 看 80 行）：`Memory cgroup out of memory`（P1 / P1b / P5）、`NVRM: Xid`（P3 / P5）、oops。脚本不读 dmesg。
- P5 后确认 `systemctl status sshd` 与宿主机仍可登录。

## 多容器 Agent 真机验收（`multicheck/`）

共享机产品已搁置（PoC no-go），但分支保留了 Agent 多容器与按实例端口的基础设施；平台不会把两个实例放到同一台机器上，所以用 `multicheck` 绕过平台、直接驱动真实执行器（与 `spark-agent run` 相同的 docker / loop 数据盘 / iptables+tc / 状态文件构造，步进模式）在真机上验收。

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/multicheck ./deploy/poc/shared/multicheck
scp /tmp/multicheck imac@192.168.1.221:/tmp/multicheck
# 机器上（root）：
sudo systemctl stop spark-agent
sudo /tmp/multicheck --lan-ip 192.168.1.221 2>/tmp/multicheck.log | tee /tmp/multicheck.out
sudo systemctl start spark-agent
```

- 参数：`--state-dir`（默认 `/var/tmp/spark-multicheck/<unix>`，与生产 `/var/lib/spark-agent` 隔离）、`--image`（默认 `spark/cuda-ssh:13.0.1`，须已在本机）、`--lan-ip`（默认默认路由的源地址）、`--data-gib`（默认 2048）、`--bridge`（默认 `spark0`）、`--allow`（出网白名单 CIDR，逗号分隔）、`--keep`（不洗机）、`--whole`（默认开，检查整机实例 D）、`--force`（`spark-agent` 服务在运行也继续；默认 `systemctl is-active --quiet spark-agent` 成功即拒绝启动，退出码 2）。
- 步骤：1 建 A（4 份，2223）、B（4 份，2224）→ 2 `docker inspect` 核对 Memory / MemorySwap = (MemTotal − 8GiB)/4、NanoCpus = nproc/4、PidsLimit 8192、CapAdd 无 SYS_NICE、ShmSize = Memory/2、端口发布、数据盘镜像 512 GiB 且已挂载 → 3 C 用 2223 必须报 `SSH 端口冲突` 且不留容器 / 数据盘 → 4 `Mounts()` 的 cgroup 目录存在，`metrics.Collector` 两次采样（间隔 3 s），mem_max = docker Memory；A 内两个空转循环时 A 的 cpu_percent 150～250、B < 20 → 5 容器内 `memory.max` / `cpu.max`（`500000 100000`）→ 6 A 连不到 B 的容器地址 :22 与宿主机 :2224，宿主机能连到两个 sshd（对照）→ 7 洗 A/B，建整机 D（share_slots 1，2225）核对 Memory = MemTotal − 8GiB、NanoCpus 0、PidsLimit 32768、有 SYS_NICE、数据盘 2048 GiB，再洗 D → 8 独立确认容器、镜像文件、loop 设备、挂载、ssh 目录都已消失。
- 输出：每步 `== step N …`，每项 `PASS` / `FAIL <项>: <实测值>`，最后 `RESULT: PASS` 或 `RESULT: FAIL (n checks failed)`（退出码 1）。执行器日志（Warn 以上）走 stderr。
- 超时：单条 create / wipe 指令 10 分钟、单条查询命令 2 分钟；清理（洗机 + 最终校验）整体 5 分钟，超时的项记 FAIL，残留按下一条手工处理。
- 中途失败或 Ctrl-C 也会洗掉已创建的实例（之后的 Ctrl-C 被忽略，等清理跑完）；`kill -9` 时按 `--state-dir` 与实例 ID（`ins-m<6 位>{a,b,c,d}`，输出里有）手工 `docker rm -f`、`umount`、`losetup -d`、删镜像。
- 副作用：会按 `--allow` 重写 `SPARK-FWD` 链（默认空白名单），启动时打印 `WARNING` 提示；正在跑的平台实例在真 Agent 重启并收到平台配置重新同步之前出网只剩公网（与 Agent 冷启动时相同），所以跑完务必 `systemctl start spark-agent`。需要 2048+100 GiB（D）与 2×(512+100) GiB 的根分区可用空间（稀疏文件，实际占用很小）；端口 2223～2225 须空闲。
