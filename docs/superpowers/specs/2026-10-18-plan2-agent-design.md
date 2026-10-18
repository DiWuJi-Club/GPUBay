# Spark Cloud — 计划 2：Agent 网关与节点 Agent 设计文档

- 日期：2026-10-18
- 状态：已审定（Fable，2026-10-18）
- 上位文档：`2026-10-14-spark-cloud-mvp-design.md`（下称「总设计」）§1、§2、§3、§4、§10
- 范围：平台侧 gRPC Agent 网关（mTLS、机器接入、指令投递、心跳、对账）+ 机器侧 `spark-agent`（Docker 运行时、数据盘、网络隔离、限速、指标）+ 后台「机器接入」界面 + 真机验收。
- 不在范围：sshpiper 与 Traefik（计划 3）、实例 Web 端口 `UpdatePorts`（随计划 3）、档期预订（子项目 2）。

## 0. 现状与真机勘查（2026-10-17/18）

**平台现状**：实例状态机已经完整，但生产中没有任何东西驱动它。指令写入 `agent_commands` 后无人投递；`Fleet.Heartbeat` 与 `Rental.HandleCommandResult` 只被 e2e 测试钩子调用；`machines.cert_fingerprint` 字段存在但从未写入。计划 1 遗留的 6 条「计划 2 必须处理」见 §9 对照表。

与本设计相关的现有代码事实（实现时不必再查）：
- `rental.CreateTimeout = 10 分钟`、`OfflineTimeout = 5 分钟`、`RetainAfterStop = 6 小时`。
- `Fleet.Heartbeat` 对机器行 `FOR UPDATE`，返回「是否刚从离线恢复」（新机器首次心跳也返回 true）。
- `Fleet.CompleteCommand` 只在 `status IN ('pending','sent')` 时生效，否则返回 `applied=false`，`HandleCommandResult` 随即返回：迟到、重复、已取消指令的回报全部被忽略；`instance_id=''` 的指令在记录结果后也直接返回。
- `Rental.ResumeMachine(ctx, machineID)` 目前恢复该机器上**全部**暂停实例，没有按实例过滤的入口（本计划改签名，见 §3.2）。
- `agent_commands.type` 没有 CHECK 约束（只有 `status` 有）；`machines.state` 只有 `idle / allocated / maintenance / faulty`。
- 锁顺序（`rental/service.go`）：`instance → agent_commands → orders → wallet；machine → wallet`。`HandleCommandResult` 先 `CommandInstanceID`（不加锁）→ `lockInstance` → `CompleteCommand`。
- `Allocate` 要求 `agent_online`；`CancelCommands` 会把 `sent` 的指令也标为 failed（`beginWipe` / `failCreating` / `AdminRewipe` 调用）。
- e2e：`/hooks/agent` 对每台机器 `Heartbeat` 后把 `PendingCommands`（pending 与 sent）逐条 `HandleCommandResult`；`/hooks/sweep` 默认先全量心跳再 `Rental.Sweep + Billing.Sweep`；Playwright 通过 `web/e2e/helpers/hooks.ts` 调用。

**测试机 192.168.1.221 勘查结果**（只读勘查，未用 sudo）：

| 项 | 结果 | 对设计的影响 |
|---|---|---|
| 磁盘 | 单块 3.7T NVMe，只有 EFI + 一个 ext4 根分区 | **总设计「专用数据分区 + mkfs」不成立**，改为每实例一个 loop 镜像文件（§4.3） |
| Docker | 29.6.2，overlay2，cgroup v2，`/var/lib/docker` 在根分区 | 容器可写层也在根分区，无法限额（overlay2 on ext4 不支持 `--storage-opt size`），用监控告警兜底（§4.3） |
| GPU | `--gpus all` 可用；`nvidia-smi` 能读利用率、温度、功耗；**显存为 `[N/A]`**（统一内存） | 内存指标改读 `/proc/meminfo` |
| 工具 | docker CLI、iptables（nft 后端）、tc、losetup、mkfs.ext4 齐全；`ip_forward=1` | 全部直接 exec 系统命令（含 `docker`），不引入 netlink 库或 Docker SDK |
| 网络 | 当前走 WiFi（有线网卡 down），机房生产走 10GbE | 网卡名不写死，由 Docker 网桥名定位；端口发布地址由平台下发的 `lan_ip` 决定（§4.2） |
| 镜像 | nvcr.io 可达，Docker Hub 不可达；容器内 apt（ports.ubuntu.com）可用 | 测试镜像可在机器上 `docker build` |
| sshd | `nvcr.io/nvidia/cuda:13.0.1-base` 镜像**不含 sshd** | 定义平台镜像规范：必须自带 `/usr/sbin/sshd`（§4.2） |
| 权限 | `imac` 在 docker 组，sudo 需密码 | Agent 以 root 跑在 systemd 下；安装与真机验收中需要 root 的步骤由负责人执行 |

## 1. 架构

```
platform（Go 单体，新增）                         spark-agent（每台机器，root，systemd）
┌───────────────────────────────┐   gRPC 双向流   ┌──────────────────────────────┐
│ agentgw：TLS 监听 :9443         │◀──── mTLS ─────│ client：连接/重连/心跳/对账    │
│  ├ Enroll（一次性令牌 + CSR）   │                │ executor：按指令 ID 去重执行   │
│  ├ Connect（流）               │                │ runtime: docker(CLI) | fake  │
│  ├ 投递循环（每秒）             │                │ disk:    loop | fake         │
│  └ 对账、连接归属与指纹校验       │                │ netfilter: iptables+tc | fake│
│ pki：内置 CA、服务端证书         │                │ metrics: nvidia-smi + /proc  │
│ fleet / rental（已有，含指令超时）│                │ state：/var/lib/spark-agent   │
└───────────────────────────────┘                └──────────────────────────────┘
```

- 新端口 `SPARK_AGENT_ADDR`（默认 `:9443`），只对机房内网开放，与 8080/8081 分开监听。
- 协议定义放在 `api/proto/agent/v1/agent.proto`，生成代码 `internal/agentpb/` **提交进仓库**；`make proto` 重新生成（protoc 放在 `~/.local/bin`，版本写进 Makefile 注释）。日常构建和测试不需要 protoc。
- 新增依赖：`google.golang.org/grpc`、`google.golang.org/protobuf`。
- **Docker 通过 exec `docker` CLI 操作**（`docker ... --format '{{json .}}'` 解析输出），与 iptables / tc / losetup 的调用方式一致；不引入 Docker SDK，也不手写 Engine API 客户端。用到的子命令：`network create/inspect`、`create`、`start`、`stop`、`rm`、`inspect`、`ps --filter label=`、`pull`、`version`。
- 多副本：网关允许多个 platform 副本同时监听，正确性由 `machines.agent_conn_id`（§2.3、§3.1）保证；不做负载均衡。指令超时判定放在清扫器（已有咨询锁，§3.1）。

## 2. 身份与接入（PKI）

### 2.1 内置 CA
- 平台首次启动时生成 ECDSA P-256 根 CA（有效期 20 年）。私钥用现有 `secretbox`（`SPARK_SECRET_KEY`）加密，存表 `platform_pki`（单行）。多副本并发首次启动时，用 `INSERT ... ON CONFLICT DO NOTHING` 再读取，保证只有一套 CA。轮换 `SPARK_SECRET_KEY` 会使 CA 私钥不可解密，README 注明。
- 网关服务端证书：启动时由 CA 现签，SAN 取自 `SPARK_AGENT_HOSTS`（逗号分隔的域名或 IP，必填，prod 下缺失则启动失败；dev 默认 `localhost,127.0.0.1`），有效期 1 年，不落盘。通过 `tls.Config.GetCertificate` 提供，每日检查一次，剩余有效期 < 30 天时自动重签并热替换，平台长期不重启也不会过期。
- 机器证书：Subject CN = 机器序列号（由平台按 `machines.serial` 填写），有效期 5 年。**指纹 = 证书 DER 的 SHA-256 十六进制**。**吊销靠数据库**：网关建流时与每个投递 tick 都核对流上证书指纹与 `machines.cert_fingerprint`；`cert_fingerprint=''`（未接入）的机器一律拒绝；重新接入即作废旧证书；需要封禁一台机器时由管理员执行「撤销接入」（§5.2，清空指纹），其连接在下一个 tick 被断开。所以不需要 CRL。证书续期列入遗留事项。

### 2.2 接入流程（私钥不出机器）
1. 后台机器详情页点「生成接入命令」（super / ops，需动态码，写审计）。平台生成 32 字节随机令牌，只存哈希到 `machine_enroll_tokens`（machine_id、token_hash、expires_at = 24 小时、used_at、issued_by）。同一台机器生成新令牌时，旧的未用令牌立即失效。令牌明文不写日志、不写审计 diff（审计只记令牌记录 id）；展示形式 `spk1_<令牌>`，前缀便于识别。
2. 页面**只显示一次**安装命令：
   `curl ... | sudo sh -s -- --platform agent.example:9443 --token spk1_<令牌> --ca-sha256 <CA 证书指纹> --binary-sha256 <agent 二进制指纹>`
   MVP 阶段安装脚本和 agent 二进制由运维放到内网文件服务器，下载地址可配置（`SPARK_AGENT_DOWNLOAD_URL`，为空时页面只显示 `spark-agent enroll ...` 这一行）；`install.sh` 下载二进制后校验 `--binary-sha256`（平台从 `SPARK_AGENT_BINARY_SHA256` 读取并填进命令），不符则中止。
3. `spark-agent enroll` 在本机生成 P-256 私钥（0600，`/etc/spark-agent/agent.key`），连网关时只信任指纹等于 `--ca-sha256` 的 CA，调用 `Enroll{token, csr}`。
4. 平台在一个事务里完成：校验令牌（存在、未用、未过期；机器任意状态均可接入）→ 校验 CSR 签名 → **只取 CSR 的公钥**，Subject 由平台按机器序列号生成，忽略 CSR 里的 Subject / SAN / 扩展 → 签发证书 → 写 `machines.cert_fingerprint`，并把 `machines.agent_conn_id` 置 NULL → 标记令牌已用 → 写审计（actor 为系统，带令牌签发人）。返回证书和 CA 证书。
5. 重复接入同一台机器会覆盖指纹并清空 `agent_conn_id`：旧 Agent 的新连接在建流时被拒；旧流所在副本在下一个投递 tick 发现 `agent_conn_id` 与指纹都不再匹配，关闭流并返回 `Aborted`（§2.3）。

`Enroll` 与 `Connect` 共用 9443 端口：TLS 配置为「客户端证书可选」（`VerifyClientCertIfGiven`，最低 TLS 1.3），`Enroll` 不要求客户端证书，`Connect` 必须有且通过校验，否则返回 `Unauthenticated`。`Enroll` 按来源 IP 限流（每分钟 10 次），失败只返回统一的「令牌无效」。

### 2.3 建流校验与连接归属
`Connect` 时：证书链由 CA 签发 → CN 对应一台机器 → 指纹匹配且非空 → 机器已登记。通过后：
1. 生成连接 ID（uuid），`UPDATE machines SET agent_conn_id = <conn_id>, agent_connected_at = now()`；再调用 `Fleet.Heartbeat`（Connect 本身算一次心跳，把 `agent_online` 置 true）。
2. 把该流登记到本副本的内存连接表，**同一台机器的新连接顶掉旧连接**（本副本持有的旧流收到 `Aborted`）。多副本时，后连上的副本赢。
3. 本副本每个投递 tick（§3.1）对自己持有的每条流校验 `machines.agent_conn_id` 仍是该流的 conn_id、且 `cert_fingerprint` 仍等于流上证书指纹；任一不符即关闭流（`Aborted`），不再向它投递。这一条同时实现了「跨副本顶替」与「重新接入 / 撤销接入后的吊销」。
4. 流关闭（主动或被动）时 `UPDATE machines SET agent_conn_id = NULL WHERE id = $1 AND agent_conn_id = $2`，只清自己的。

## 3. 协议

```proto
service AgentGateway {
  rpc Enroll(EnrollRequest) returns (EnrollResponse);
  rpc Connect(stream AgentMessage) returns (stream PlatformMessage);
}

message AgentMessage {
  oneof msg {
    Hello hello = 1;                  // 建流后第一条：agent 版本、主机名、本机 IP 列表
    StateReport state = 2;            // 建流后第二条，此后每 60 秒一次，以及每条指令执行完后
    Heartbeat heartbeat = 3;          // 每 10 秒，带指标
    CommandResult result = 4;         // 指令执行结果
  }
}
message PlatformMessage {
  oneof msg {
    Command command = 1;
    AgentConfig config = 2;           // 建流后平台发的第一条：lan_ip、心跳间隔、网络白名单 allow_cidrs、
                                      // dns_servers、默认限速、数据盘大小 data_size_gib
  }
}
message Command {
  string id = 1;                      // agent_commands.id（uuid）
  string instance_id = 2;
  oneof body {
    CreateInstance create = 10;       // image_ref, upstream_pubkey, bandwidth_mbps, ssh_port, data_size_gib
    StartInstance start = 11;
    StopInstance stop = 12;
    WipeInstance wipe = 13;
    PullImage pull = 14;              // image_ref
  }
}
message CommandResult { string command_id = 1; bool ok = 2; string message = 3; }
message StateReport { repeated InstanceState instances = 1; }
message InstanceState { string instance_id = 1; string phase = 2; } // running / stopped / missing_data / unknown
message Heartbeat { Metrics metrics = 1; }
message Metrics { double gpu_util; double gpu_temp_c; double gpu_power_w; uint64 mem_total; uint64 mem_used;
                  uint64 root_disk_total; uint64 root_disk_used; uint64 data_disk_used; double load1; }
```

（字段编号在实现中补全；以上为语义。）

`Hello` 带本机 IP 列表：若不含 `machines.lan_ip`，网关记 warning 告警 `lan_ip_mismatch`（§3.3 的稳定文案规则），登记错 IP 在接入时就暴露。

### 3.1 指令投递（补计划 1 遗留）
投递、连接归属、重推在同一条规则里，避免副本切换或重连时指令卡在 `sent`：

1. **认领与归属原子判定**。每个网关副本每秒一个 tick，用一条 SQL 认领 pending 指令：
   ```sql
   UPDATE agent_commands c SET status = 'sent', attempts = attempts + 1, sent_at = now(), updated_at = now()
   FROM machines m
   WHERE m.id = c.machine_id AND m.agent_conn_id = ANY($1)   -- $1 = 本副本当前活跃流的 conn_id 列表
     AND c.status = 'pending'
   RETURNING c.*, m.agent_conn_id
   ```
   按创建顺序推给对应流。若返回行的 `agent_conn_id` 已不是本副本持有的那条流（SQL 期间被顶掉），不推送、关闭该流，指令保持 `sent`，由规则 2 补推。
2. **sent 补推**。同一个 tick 里，对本副本在线机器再取 `status='sent'` 且**不在该流「已推送集合」**（内存，按连接维护）中的指令，推送并刷新 `sent_at`。Agent 重连后的「重推所有 sent」只是这条规则在集合为空时的特例，不单独实现。Agent 端按指令 ID 去重，重复推送无害。
3. **先对账再投递**。本副本在该流收到并处理完第一条 StateReport 之前，不向它推送任何指令，保证对账产生的指令（§3.2）与补推的 sent 顺序可控。
4. **执行顺序**。同一台机器上的实例指令按顺序执行（agent 单 goroutine 执行队列），避免 create 与 wipe 交错；`PullImage` 进单独的低优先级队列，不阻塞实例指令。
5. **超时判定在清扫器**（可用假时钟测试，多副本只有一个在跑）：新增 `Rental.FailTimedOutCommands`，每轮清扫执行
   `SELECT c.id, c.type FROM agent_commands c JOIN machines m ON m.id = c.machine_id WHERE c.status = 'sent' AND m.agent_online AND c.sent_at < now - timeout(c.type)`，
   逐条 `HandleCommandResult(id, false, "timeout after <时长>")` 交给状态机。超时值作为 `rental` 包常量：create 15 分钟（与现有 creating 超时的关系见 §5.3）、start / stop 5 分钟、wipe 30 分钟、pull 60 分钟。只对 `agent_online` 的机器计时；离线机器的指令保持 `sent`，重连后由规则 2 补推并刷新 `sent_at`，从头计时。
6. **迟到回报**。已取消或已超时的指令，Agent 的结果仍会被 `CompleteCommand` 忽略（已有逻辑）；它造成的实际副作用（例如容器其实建好了）由 §3.2 的对账清理。
7. 流断开后、清扫器判离线前的 5 分钟内，机器仍 `agent_online`、仍可被 `Allocate`，这是有意为之：这段时间下的单，create 停在 pending 等重连，20 分钟后由 `FailStuckCreating` 兜底（§5.3）。

### 3.2 对账（StateReport）
每次收到 StateReport（建流后第一条、每 60 秒、每条指令执行完后）都执行一次。网关先把报告原文写入 `machines.last_state_report jsonb` 与 `state_reported_at`，再按 `instances` 表算出这台机器「应有」的实例集合，与报告比对：

| Agent 报告 | 平台记录 | 处理 |
|---|---|---|
| 有容器 | 实例不在本机，或已 released / failed | 入队 `WipeInstance`（孤儿清理，`instance_id` 照填；状态机对非 wiping 实例的 wipe 结果本来就忽略），记 info 告警 |
| running | stopped | 入队 `StopInstance` |
| running | running 且暂停中 | 走下面的「容器正常才恢复计时」 |
| stopped 或 missing | running 且未暂停 | 记 critical 告警 `container_down`，**不自动处理**（避免误判时毁掉用户数据），计时继续，由管理员换机或退款 |
| missing | stopped | critical 告警 `container_missing`；之后的 Start 会失败并走现有 `start_failed` |
| missing_data（容器在、数据盘丢失） | 任意 | critical 告警 `data_disk_missing` |
| 无 | creating 且有 sent 的 create 指令 | 不处理（等待执行或超时） |
| 任意 | wiping | 不处理，等 wipe 结果 |

**对账动作的事务与互斥**：每个动作在独立事务内执行。有实例行的先 `lockInstance`，以锁内读到的状态重新判定（保持 `instance → agent_commands` 锁顺序）；只有当该实例**没有任何** `pending / sent` 指令（`HasOpenCommand` 扩展为不限类型的 `HasAnyOpenCommand`）时才入队 Stop / Wipe，避免与刚入队的 start、仍在执行的 stop 或上一轮入队的孤儿 wipe 打架。孤儿 wipe 的回报由网关单独处理：`HandleCommandResult` 之后若 `cmd.Type = wipe && !ok` 且实例不是 wiping，网关 `RaiseAlertOnce(critical, orphan_wipe_failed, machine, instance)`，否则运维不会知道机器上留着别人的容器与数据盘。

**容器正常才恢复计时**（补计划 1 遗留）：`ResumeMachine` 改签名为 `ResumeMachine(ctx, machineID int64, runningIDs []string)`，只恢复 `id = ANY(runningIDs)` 的暂停实例；其余暂停实例保持暂停并 `RaiseAlertOnce(container_down)`，由管理员换机或退款。网关每次 StateReport 都调用一次（不只首次，管理员手工拉起容器后下一次报告即恢复）。清扫器里现有的 `resumeOnline` 改为：对 `agent_online AND state_reported_at >= agent_connected_at` 的机器，用 `last_state_report` 中 phase=running 的实例列表调用同一个 `ResumeMachine`，作为网关事务失败时的兜底；两条路径共用一个实现。顺序要求见 §2.3：Connect 先 `Heartbeat` 置在线，再处理 StateReport，避免清扫器在两者之间把实例再次 `SuspendMachines`。

### 3.3 心跳与在线
- 心跳调用现有 `Fleet.Heartbeat`；返回「刚从离线恢复」时不直接恢复计时，而是等 StateReport（上一节）。
- 指标写入 `machines.last_metrics`（jsonb）与 `agent_version`，只保留最新一份，不做时序库（时序监控列入遗留）。
- 阈值告警用 `RaiseAlertOnce`（按 kind + message 去重，同类未处理时不重复）：根分区使用率 > 90%、GPU 温度 > 90°C。**告警正文必须稳定**，不带百分比、温度等数值（否则每次心跳都是新告警），写成固定文案如「机器 {serial} 根分区使用率超过 90%」，数值到 `last_metrics` 里看。本文档所有 `RaiseAlertOnce` 告警都遵守这条。
- 流断开时不立刻标离线，仍由清扫器按 5 分钟无心跳判定（已有逻辑），避免网络抖动导致暂停计时（后果见 §3.1 第 7 条）。

## 4. spark-agent

本节所有机器侧逻辑假设**每台机器同一时间只有一个实例**（`instances_machine_active_uniq` 已保证）：`Metrics.data_disk_used` 只有一个值、`SPARK-FWD` 不区分容器、SSH 端口固定 2222。实现里不要出现多容器分支。

### 4.1 进程与目录
- 单个静态二进制（`CGO_ENABLED=0 GOOS=linux GOARCH=arm64`），子命令：`enroll`、`run`、`selfcheck`、`version`。
- systemd unit：`Restart=always`，`After=docker.service`，root 运行，`OOMScoreAdjust=-900`。
- `/etc/spark-agent/`：`agent.key`、`agent.crt`、`ca.crt`、`config.yaml`（平台地址）。
- `/var/lib/spark-agent/`：`state.json`（已执行指令 ID → 结果，保留最近 1000 条；实例本地记录：期望状态、数据盘路径、SSH 端口）、`data/<id>.img`、`mnt/<id>/`、`ssh/<id>/`。
- `selfcheck`：检查 Docker 可达、`nvidia` 运行时、iptables/tc/losetup 可用、ufw 未启用或不干扰 `DOCKER-USER`、磁盘剩余，安装脚本最后执行它。

### 4.2 容器规格（修订总设计 §4.3）
- 名称 `ins-xxxxxxxx`，标签 `spark.instance=<id>`；镜像来自指令。全部通过 `docker create/start/stop/rm/inspect` 命令完成（§1）。
- GPU：`--gpus all`。
- **不用 `--ipc=host`**：共享宿主机 IPC 命名空间会让租户看到宿主机进程的共享内存。改为 `--shm-size = 机器内存的一半`（GB10 上约 60GiB），PyTorch DataLoader 等场景够用。
- **资源上限**（防止租户把宿主机和 Agent 一起拖死）：`--memory = 机器内存 − 8GiB`（给宿主机、Docker 与 Agent 留余量），`--memory-swap` 同值，`--pids-limit 32768`，`--oom-score-adj 500`。不限 CPU。
- 非 privileged：Docker 默认能力集 + `IPC_LOCK`、`SYS_NICE`、`SYS_PTRACE`（调试器 / profiler 需要）；`--ulimit memlock=-1:-1 --ulimit stack=67108864:67108864`；`seccomp` 用 Docker 默认。
- 网络：Agent 创建专用网桥 `spark0`（`com.docker.network.bridge.name=spark0`，`enable_icc=false`，`enable_ipv6=false`，子网固定 `10.200.0.0/24`，可配置）。容器 22 端口发布到 `<lan_ip>:2222`：`lan_ip` 来自 `AgentConfig`（即 `machines.lan_ip`），本机没有该地址时 create 失败「内网 IP 不匹配」。SSH 端口字段由指令带来，本计划固定 2222。
- **DNS**：容器创建时显式指定 `--dns <AgentConfig.dns_servers>`（默认 `223.5.5.5, 119.29.29.29`，须在白名单内或为公网地址），`--dns-search` 置空。注意 dockerd 的内置 DNS（127.0.0.11）在**宿主机网络命名空间**代为查询，这些包不经过 `SPARK-FWD`，所以白名单对 DNS 的约束靠这项配置而不是 iptables。
- **重启策略为 no**：宿主机重启后由 Agent 先挂载数据盘、再启动应处于 running 的容器（以 `state.json` 的期望状态为准）。启动前必须通过 §4.3 的挂载校验，避免 Docker 在数据盘挂载前拉起容器、把数据写进空目录。
- **SSH 接入（镜像规范）**：平台镜像必须自带 `/usr/sbin/sshd`。Agent 把 `/var/lib/spark-agent/ssh/<id>/` 只读挂到容器 `/.spark/`，里面有 `authorized_keys`（平台上游公钥）、`hostkeys/`（由 Agent 在宿主机预先生成，只读挂载即可）和 `init.sh`；容器入口改为 `/.spark/init.sh`：写最小 sshd 配置（`HostKey /.spark/hostkeys/...`、`AuthorizedKeysFile /.spark/authorized_keys`、`StrictModes no`、`PasswordAuthentication no`、`PermitRootLogin prohibit-password`）、`exec /usr/sbin/sshd -D -e`。**镜像自身的 ENTRYPOINT / CMD 不会执行**，服务由用户通过 SSH 启动。镜像规范写进 `docs/image-spec.md`，并提供一个参考 Dockerfile（基于 `nvcr.io/nvidia/cuda` + openssh-server）。
- **上游 SSH 密钥**：平台配置 `SPARK_UPSTREAM_SSH_KEY_FILE` 指向 ed25519 私钥（0600），平台读取并派生公钥放进 Create 指令；计划 3 的 sshpiper 用同一把私钥登录容器。新增命令 `platform admin gen-upstream-key <path>` 生成密钥。prod 下未配置则启动失败；dev 下缺省时自动在临时目录生成并打印路径。轮换该密钥需要重建全部容器，列入遗留。

### 4.3 数据盘（修订总设计「专用分区 + mkfs」）
- 每个实例一个**稀疏镜像文件** `/var/lib/spark-agent/data/<id>.img`，大小由 `AgentConfig.data_size_gib` 决定（默认 2048GiB，后台按机型可改）。`mkfs.ext4 -E lazy_itable_init=1,nodiscard` → `losetup` → 挂到 `mnt/<id>`（`nosuid,nodev`）→ bind 进容器 `/root/data`。
- 创建前检查根分区剩余空间 ≥ 数据盘大小 + 100GiB 余量，否则指令失败（「磁盘空间不足」），由状态机走开通失败退款。
- **挂载校验（数据安全）**：`mnt/<id>` 目录创建后 `chattr +i`（挂载后不影响挂载点内部写入，未挂载时任何写入都会失败）。`StartInstance`、重启恢复、以及 create 的最后一步，在 `docker start` 前执行 `mountpoint -q mnt/<id>` 且校验挂载源是该实例的 loop 设备；不满足则返回失败「数据盘未挂载」，并在 StateReport 里报 `missing_data`，**绝不以未挂载状态启动容器**。
- **洗机**：删除容器（含匿名卷）→ 卸载 → `losetup -d` → 删除镜像文件 → 删除 `ssh/<id>` → `fstrim /`（失败只记日志）→ 校验：容器、挂载点、loop 设备、镜像文件都已不存在，否则返回失败（机器转 faulty，已有逻辑）。
- 镜像文件删除后，块回到文件系统空闲池；下一位租户拿到的是新文件，非 privileged 容器读不到裸设备，所以不存在数据残留可读的问题。
- **已知风险**：容器可写层（`/` 下除 `/root/data` 外）在根分区上无法限额。缓解：根分区使用率 > 90% 告警；用户帮助文档写明大文件放 `/root/data`。真正的限额（XFS project quota 或独立分区）需要重装系统，列入遗留事项。

### 4.4 网络隔离与限速
- 基于 `iptables`，规则全部放在 Agent 自己的链里（`SPARK-FWD`、`SPARK-IN`），由 `DOCKER-USER`（`-i spark0 -j SPARK-FWD`）和 `INPUT`（`-i spark0 -j SPARK-IN`）跳转；Agent 启动时和每 60 秒检查一次，缺了就补，幂等。
- `SPARK-FWD`（来自 `spark0` 的转发流量）：已建立连接放行 → 白名单（`AgentConfig.allow_cidrs`，例如 Harbor、HF 镜像站、DNS）放行 → 目的为私网 / 保留地址（10/8、172.16/12、192.168/16、100.64/10、169.254/16、224/4）丢弃 → 其余（公网）放行。
- `SPARK-IN`（来自 `spark0`、目的为宿主机本身）：已建立连接放行，其余丢弃。容器访问不了宿主机上的任何服务（包括 Docker API、Agent、9443）。DNS 不经过这两条链，见 §4.2。
- **IPv6**：网桥 `enable_ipv6=false`，并 `sysctl net.ipv6.conf.spark0.disable_ipv6=1`；否则 ip6tables 没有任何规则，私网隔离只对 v4 生效。
- 限速：容器启动后找到它在宿主机上的 veth，下行用 `tc qdisc add dev <veth> root tbf rate <mbps>mbit burst 256kbit latency 50ms`，上行用 `tc qdisc add dev <veth> ingress` + `tc filter ... police rate <mbps>mbit burst 256k drop`，速率取指令里的 `bandwidth_mbps`（默认 200）。容器重启后重新设置。单元测试对生成的命令做快照断言。
- 宿主机自己的管理流量不受影响。

### 4.5 幂等与执行
- 指令执行前查 `state.json`：已执行过的直接回放结果，不重复执行。
- 每个操作本身也幂等，并且**幂等到步骤级**（Agent 在 mkfs 与容器创建之间重启时 `state.json` 没有结果，会整条重跑）。create 的每一步自检：镜像文件存在且大小正确 → 跳过；`blkid` 已有 ext4 → 跳过 mkfs；已 `losetup` → 复用；已挂载 → 跳过；容器存在且 label / 镜像一致**且数据盘已挂载** → 视为成功，否则删掉重建。stop：「容器不存在」视为成功并按状态报告。这样 `state.json` 丢失也不会出错。
- 执行结果先写 `state.json`（fsync），再发送；发送失败等重连后在 Hello 之后补发。

### 4.6 fake 实现
`runtime/fake`、`disk/fake`、`netfilter/fake` 在内存中模拟，可注入「失败 / 延迟 / 容器意外退出 / 挂载缺失」。用于平台集成测试、e2e-server（步进模式，§7）以及 `spark-agent run --fake` 本地开发（不需要 Docker、GPU、root）。

## 5. 平台侧改动

### 5.1 数据库（迁移 00017）
- `platform_pki`：CA 证书、加密的 CA 私钥。
- `machine_enroll_tokens`：见 §2.2。
- `machines` 新增：`agent_version`、`last_metrics jsonb`、`last_state_report jsonb`、`agent_connected_at`、`state_reported_at`、`agent_conn_id`。
- `agent_commands` 新增：`sent_at`，以及部分索引 `(sent_at) WHERE status = 'sent'`（供 `FailTimedOutCommands`）。`type` 列没有 CHECK 约束，`pull_image` 只需 Go 侧新增 `CmdPullImage` 常量。

### 5.2 后台
- 机器详情：接入状态（未接入 / 已接入，证书指纹前 16 位）、Agent 版本、最近一次指标、「生成接入命令」与「撤销接入」按钮（都是 super / ops，动态码，审计；命令只显示一次，可复制；撤销接入清空 `cert_fingerprint` 与 `agent_conn_id`）。
- 机器列表：增加「GPU 温度」「根分区使用率」两列。
- 镜像上架或启用时，向所有在线机器广播 `PullImage`（已有镜像接口里加一步，失败不阻塞上架）。
- 接口按现有权限矩阵。

### 5.3 与现有状态机的衔接
- creating 超时：现有清扫器 10 分钟后 `FailStuckCreating`（从 `created_at` 计）。大镜像首次拉取可能超过 10 分钟，所以改为 **20 分钟**，并依靠上架时预拉取；`TestCreatingTimeoutRefundsAndIgnoresLateAck` 随之更新。create 指令超时 15 分钟从 `sent_at` 计，通常先于清扫器触发、给出更准确的失败原因；若指令在机器刚掉线的窗口里入队（§3.1 第 7 条），清扫器可能先到。两者都走 `failCreating`，先到的生效，后到的因状态已变而无效。
- `ResumeMachine` 改签名（§3.2），现有 `TestOfflineMachinePausesLeaseAndResumes`、`TestSweepResumesOnlineSuspended` 按新签名改。
- 计划 1 遗留「`occupiedUntil` 回退为 now」与「失联期间 lease 上界不推进」与档期预订相关，本计划不处理，继续保留在遗留清单。

## 6. 错误处理
- Agent 侧所有命令执行失败都带上可读原因（命令 + stderr 末尾 500 字节），进 `agent_commands.result` 与告警正文；用户只看到「开通失败，已退款」。
- 网关断流：Agent 指数退避重连（1s 起，上限 60s，带抖动）。
- 平台不可达期间 Agent 照常维持容器运行，不做任何自主停机（到期停机由平台决定）。

## 7. 测试与验收

| 层级 | 内容 |
|---|---|
| 单元 | PKI（签发、指纹、过期令牌、重复接入、CSR Subject 被忽略、空指纹拒绝）、协议转换、认领 SQL（`agent_conn_id` 不匹配时不返回行）、`FailTimedOutCommands`（假时钟）、对账表的每一行（含孤儿 wipe 失败告警、有未完成指令时不入队）、`ResumeMachine` 按实例过滤（running 的恢复、stopped 的保持暂停并告警、第二次报告变 running 时才恢复）、agent 指令去重与步骤级幂等、start 在挂载缺失时失败、iptables / tc / docker 命令生成快照（含 `--dns`、`--memory`、`--pids-limit`）、loop 磁盘命令序列、告警文案稳定性 |
| 集成（Go，无硬件） | 真实 TLS 网关 + 嵌入式 PostgreSQL + N 个进程内 fake Agent：接入 → 下单 → 开通 → 到期 → 停机 → 续费恢复 → 释放 → 洗机；断线重连补推；迟到回报；孤儿容器清理；离线恢复时容器不在则保持暂停；新连接顶掉旧连接；指纹不符被拒；并发 50 单抢 10 台不超卖。**两个网关实例**的变体：顶替、撤销接入后旧流被断、Agent 从 A 切到 B 时 A 在切换瞬间入队的指令须在 ≤2 个 tick 内由 B 投递 |
| 端到端（Playwright） | 后台生成接入命令（动态码、只显示一次）、撤销接入、机器详情显示指标与接入状态。e2e-server 接入真实网关 + 进程内 fake Agent，但 fake Agent 运行在**步进模式**、不自动执行：`/hooks/agent` 改为「让 fake Agent 对指定机器执行当前队列（按 `fail` / `result` 参数产生结果）并发送 StateReport」，`/hooks/sweep` 的 `heartbeat=true` 改为让 fake Agent 发一次心跳；`/hooks/machines` 造机器时同时完成接入（写指纹、建流）。这样既走真实投递 / 回报 / 对账链路，又与假时钟和现有用例保持确定性 |
| **真机验收（192.168.1.221）** | 见 §7.1，全部通过才算完成 |

### 7.1 真机验收清单
平台跑在开发 Mac 上（`SPARK_AGENT_HOSTS` 填 Mac 的局域网 IP），Agent 跑在 Spark 上。需要 root 的步骤（安装、iptables 检查、重启）由负责人执行 sudo，Claude 只给出命令并读取结果。
1. 后台生成接入命令 → 负责人在 Spark 上执行安装 → 机器显示已接入、指标正常（GPU 温度、内存 128GB）；登记的 `lan_ip` 与 Hello 上报一致，无 `lan_ip_mismatch` 告警。
2. 在 Spark 上 `docker build` 参考镜像，后台登记该镜像；用户下单 → 实例 running。
3. 从 Mac 用上游私钥 `ssh -p 2222 root@192.168.1.221` 进容器：`nvidia-smi` 看到 GB10；CUDA 小程序运行通过；`/root/data` 可写；`free -g` 看到约 128GB（容器内 `free` 读的是宿主机 `/proc/meminfo`），`cat /sys/fs/cgroup/memory.max` 约为 120GiB（§4.2 的内存上限）。
4. 隔离：容器内 `curl 192.168.1.1`、`curl 10.200.0.1:2375`、`curl 192.168.1.221:9443`、`nc 192.168.1.221 22` 均失败；`curl ports.ubuntu.com` 成功；`getent hosts ports.ubuntu.com` 成功而 `dig @192.168.1.1` 失败；`ip -6 addr` 无全局 IPv6 地址。
5. 限速：容器内下载与上传各测一次，接近配置值（WiFi 环境下用较低的限速值验证，例如 50Mbps）。
6. 到期：真机验收用真实时钟，不新增任何「立即到期」接口。在 Mac 上的开发数据库里用 SQL 把实例 `expires_at` 改到 1 分钟后，观察停机 → 续费 → 恢复，`/root/data` 中的文件仍在。6 小时保留后洗机同理，把 `stopped_at` 改早。
7. 释放 → 洗机 → 镜像文件、挂载、loop、容器均消失，机器回到空闲。
8. 在 Spark 上停掉 Agent（负责人 `sudo systemctl stop spark-agent`）超过 5 分钟 → 实例暂停计时 → 启动 Agent → 恢复计时；再试一次「停 Agent + docker stop 容器」→ 恢复后保持暂停并告警 `container_down`。
9. 重新生成接入命令再安装一次 → 旧证书连接被拒，新证书正常；后台「撤销接入」→ 连接断开、机器显示未接入。
10. 重启：负责人 `sudo reboot`。期望：Agent 起来前容器不自启；Agent 起来后先挂载再启动，`/root/data` 文件仍在；期间平台把机器判离线并暂停计时，StateReport 后恢复。
11. 磁盘不足：把 `AgentConfig.data_size_gib` 临时设成大于剩余空间的值，下单 → 实例 failed、退款、机器 faulty，告警正文含「磁盘空间不足」。

## 8. 部署
- `make build` 增加 `spark-agent` linux/arm64 产物并输出其 SHA-256；`deploy/agent/`：systemd unit、`install.sh`（下载并校验二进制、放置配置、执行 `enroll`、`selfcheck`、启用服务）。
- 控制面需开放 9443 给机房内网；`README` 增加 Agent 安装说明、`SPARK_AGENT_HOSTS` / `SPARK_AGENT_DOWNLOAD_URL` / `SPARK_AGENT_BINARY_SHA256` / `SPARK_UPSTREAM_SSH_KEY_FILE` 说明，以及 `SPARK_SECRET_KEY` 轮换会使 CA 不可用的提示。

## 9. 计划 1 遗留事项对照

| 遗留 | 本计划处理 |
|---|---|
| `sent` / `attempts` 无写入方 | §3.1 认领时写入 |
| 取消后迟到回报 | §3.1 忽略结果 + §3.2 对账清理副作用（孤儿 wipe 失败单独告警） |
| 心跳恢复时直接 `ResumeMachine` | §3.2 改为等 StateReport 后按容器状态恢复（`ResumeMachine` 增加实例列表参数） |
| 失联期间 lease 上界不推进 | 不处理（档期预订前处理），保留 |
| `occupiedUntil` 回退为 now | 不处理，保留 |
| 容器正常才恢复计时 | §3.2 |

新增遗留（本计划不做）：机器证书续期、上游 SSH 密钥轮换、容器可写层限额、时序监控、Agent 自动升级。
