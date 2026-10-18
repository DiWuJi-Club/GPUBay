# 计划 2 真机验收记录（192.168.1.221）

- 日期：2026-10-18
- 机器：DGX Spark `spark-3479`（GB10，内存 130660978688 字节，根分区 3.7T ext4），Ubuntu 24.04，Docker 29.6.2（GPU 走 CDI），iptables 1.8.10（nf_tables），iproute2 6.1.0，局域网地址经 WiFi DHCP 获得。
- 平台：开发 Mac 192.168.1.218，`go run ./cmd/platform`，开发数据库 `make devdb`；Agent 安装包与二进制由 Mac 上的 `python3 -m http.server 8000` 提供。
- 规格：`docs/superpowers/specs/2026-10-18-plan2-agent-design.md` §7.1。
- 结论：§7.1 十一项全部通过（第 7 项第二段按裁定调整）。验收中发现 6 个缺陷（A0～A3、B1、B2），均已修复、经审查并在真机复测通过。整分支审查后的修复版本见「最终版本复测」一节，全部通过。

## 1. 接入与指标（§7.1.1）——通过

- 预检（普通用户跑 `spark-agent selfcheck`）：除 `iptables DOCKER-USER` 需要 root 外全部 ok；安装时以 root 执行全部 ok（docker 29.6.2、CDI `/run/cdi/nvidia.yaml`、tc、losetup、所需命令、ufw、磁盘余量 3526 GiB）。
- 后台「生成接入命令」→ 在 Spark 上 `curl … install.sh | sudo sh -s -- …`：二进制 SHA-256 校验通过 → 自检 → 接入 → 安装 systemd 单元并启动。
- 后台机器详情：`enrolled=true`、`connected=true`、`agent_version=87353aa`；指标 `mem_total=130660978688`、`gpu_temp_c=38`、`gpu_power_w=3.32`、`root_disk_total=4031871553536`；告警页无 `lan_ip_mismatch`。

## 2. 镜像与开通（§7.1.2）——修复缺陷 A0 后通过

- 在 Spark 上 `docker build -t spark/cuda-ssh:13.0.1`（`deploy/image/Dockerfile`）。后台登记镜像并启用 → 广播 `pull_image`，本地标签无法从 Docker Hub 拉取，指令 `failed` 只记日志，符合预期。
- 首次下单失败 → **缺陷 A0**（见下）。失败路径表现正确：订单金额全额退回、机器转 `faulty`、`wipe_instance` 清理完成、产生 `create_failed` 告警。
- 修复后下单：实例 `running`；`docker ps` 见容器（`192.168.1.221:2222->22/tcp`），`mount` 见 `/dev/loopN on /var/lib/spark-agent/mnt/<id> type ext4`，`losetup -a` 见 `/var/lib/spark-agent/data/<id>.img`。

## 3. 容器内验证（§7.1.3）——通过

经 `ssh -i .devdata/upstream.key -p 2222 root@192.168.1.221`：

| 项 | 结果 |
|---|---|
| `nvidia-smi` | NVIDIA GB10，驱动 580.178.04，CUDA 13.0 |
| CUDA 运行库 | `/usr/local/cuda-13.0/lib64/libcudart.so.13` 存在（参考镜像不含 python/nvcc，未编译运行 CUDA 程序） |
| `/root/data` | `/dev/loopN 196G`，可写 |
| `memory.max` | 122071044096 = 机器内存 − 8GiB |
| `memory.swap.max` | 0（`free` 显示的 Swap 15G 是宿主机数值） |
| `pids.max` | 32768 |
| `/dev/shm` | 61G |
| `docker inspect` | Memory = MemorySwap = 122071044096，OomScoreAdj 500，IpcMode private，RestartPolicy no，Dns [223.5.5.5 119.29.29.29] |

## 4. 隔离（§7.1.4）——通过

容器内（bash `/dev/tcp`、`/dev/udp` 探测），并在宿主机上对同一目标做对照：

| 目标 | 容器内 | 宿主机对照 |
|---|---|---|
| 路由器 192.168.1.1:80 / :53（TCP） | 阻断 | 可达 |
| 路由器 192.168.1.1:53（UDP DNS 查询） | 无应答 | 有应答 |
| Mac 192.168.1.218:9443 / :8000 | 阻断 | 可达 |
| 宿主机 192.168.1.221:22 / :9443 | 阻断 | 可达 |
| 网关 10.200.0.1:22 / :2375 | 阻断 | — |
| 169.254.169.254:80 | 阻断 | — |
| 公网 ports.ubuntu.com:80、`getent hosts` | 可达 | — |
| 公网 DNS 223.5.5.5:53（TCP 与 UDP） | 可达 | — |
| IPv6 | 无全局地址 | — |

- `iptables -S SPARK-FWD`、`iptables -S SPARK-IN` 与 `iptables.ExpectedFwdRules(nil)`、`ExpectedInRules()` **逐行相等**（diff 为空）。`DOCKER-USER` 含 `-A DOCKER-USER -i spark0 -j SPARK-FWD`，`INPUT` 含 `-A INPUT -i spark0 -j SPARK-IN`，`FORWARD` 策略 DROP。
- `journalctl -u spark-agent | grep 'iptables chain rebuilt'`：Agent 首次启动后整个生命周期只出现一次（18:28:55），60 秒周期检查没有反复重建。

## 5. 限速（§7.1.5）——通过

平台以 `SPARK_AGENT_BANDWIDTH_MBPS=50`、`SPARK_AGENT_ALLOW_CIDRS=192.168.1.218/32` 重启（公网源速率波动大，宿主机从阿里云镜像仅 2.8MB/s，无法验证 50Mbps 上限；因此把 Mac 加入白名单，在局域网测速，同时验证白名单功能）。Mac 上起一个测速服务（GET 200MB / PUT 丢弃）。

| 方向 | 容器内 | 宿主机对照 |
|---|---|---|
| 下行 | 5864917 B/s ≈ 46.9 Mbps | 22862338 B/s ≈ 183 Mbps |
| 上行 | 5727248 B/s ≈ 45.8 Mbps | 23787055 B/s ≈ 190 Mbps |

- `tc qdisc show`：`qdisc tbf … dev vethXXXX root … rate 50Mbit`、`qdisc ingress ffff: dev vethXXXX`；`tc filter show dev vethXXXX parent ffff:` 含 `police 0x1 rate 50Mbit burst 256Kb … action drop`。
- 白名单：`-A SPARK-FWD -d 192.168.1.218/32 -j ACCEPT` 位于私网 DROP 之前；容器可达 Mac，路由器仍阻断。

## 6. 到期、续费、保留与洗机（§7.1.6、§7.1.7）——通过

- 把 `expires_at` 改到 30 秒后 → 清扫 → `stop_instance` done → 容器 `Exited`，数据盘仍挂载；实例 `stopped`，`retain_until = stopped_at + 6h`。
- 用户续费 1 天（扣 6900）→ `start_instance` done → `running`，`/root/data/ok` 内容保留。
- 再次到期停机后把 `stopped_at` 回拨 7 小时 → 实例 `released`、`wipe_instance` done；`docker ps -a`、`mount`、`losetup -a`、`/var/lib/spark-agent/{data,mnt}`、veth 上的 tc 均无残留；机器回到 `idle`。
- 用户主动释放：实例 `released`、`wipe_instance` done、无残留、机器 `idle`；不退款（与帮助页「主动释放立即清除且不退款」一致）。

## 7. 失联与恢复（§7.1.8）——通过（第二段按裁定调整）

- `systemctl stop spark-agent`（19:16:10）→ 19:21～19:22 实例「计时已暂停」，`machine_offline` 告警 → `systemctl start` → StateReport 后恢复，`expires_at` 顺延约 98 秒（剩余时长在判定失联时冻结，判定前的 5 分钟照常计时，符合 MVP 规格「故障暂停」定义）。
- **裁定**：规格第二段「停 Agent + `docker stop` → 启动 Agent 后保持暂停并告警 `container_down`」与 §4.2「Agent 启动时按 `state.json` 启动应处于 running 的容器」（重启恢复所必需）矛盾，Agent 无法区分「宿主机重启」与「人工停了容器」。实测 Agent 启动即拉起容器并恢复计时，这是 §4.2 的预期行为。
- 改为验证 Agent 在线时的 `container_down`：`docker stop <id>`（19:30:14）→ 5 秒内 critical `container_down`「机器 spark-3479 上实例 … 的容器未在运行」，计时继续、不暂停（§3.2 对账表）；`docker start` 后 SSH 恢复。暂停中且容器未运行时保持暂停的分支由单元 / 集成测试覆盖。

## 8. 重新接入与撤销（§7.1.9）——修复缺陷 A3 后通过

- 备份旧证书（`/tmp/old.crt`，SHA-256 指纹 `CE:A6:1A:D6:78:20:DA:20…`）→ 后台重新生成接入命令 → 在 Spark 上重新安装（同时升级 Agent）→ 新连接正常，后台指纹前缀由 `cea61ad67820da20` 变为 `304cd80a17d88574`。
- 「撤销接入」→ 后台显示未接入、已断开；Agent 收到 `Aborted: connection superseded or enrollment revoked` 后重连被拒。修复 A3 后日志为
  `ERROR platform rejected agent certificate; re-enroll this machine err="rpc error: code = Unauthenticated desc = certificate not recognised"`，并按退避继续重连。
- 再次接入恢复（指纹前缀 `bcc1abc0a5ee3fc3`），运行中的实例全程未中断。

## 9. 重启（§7.1.10）——修复缺陷 B1、B2 后通过

- 第一次（Agent 51d4be6）：容器内写 `/root/data/reboot.txt` 后 `systemctl reboot`。启动后容器未被 Docker 自启（restart=no），Agent 先挂载数据盘（`/dev/loopN on /var/lib/spark-agent/mnt/<id>`）再启动容器——但 `docker start` 失败，发现**缺陷 B1**；随后 Agent 重启时容器虽然起来但没有网卡，发现**缺陷 B2**。平台侧：重启约 35 秒，未到 5 分钟失联判定，计时未暂停。
- 复测（Agent f0393ac）：

```
19:52:52 systemctl reboot
19:53:25 spark-agent 启动；iptables chain rebuilt
19:53:26 ERROR recover: instance ins-ib4ka4cc: 宿主机尚未配置地址 192.168.1.221，暂不启动
19:53:28 NetworkManager: wlP9s9 拿到 192.168.1.221
19:53:29 iptables chain rebuilt（收到 AgentConfig，立即刷新）
19:53:29 INFO recovered instance started on retry instance=ins-ib4ka4cc
```

  之后：容器 running、挂接 1 个网络、`docker port` 为 `22/tcp -> 192.168.1.221:2222`、veth 上 tbf 与 ingress 各 50Mbit、数据盘已挂载；SSH 进入容器 `/root/data/reboot.txt` 内容保留、eth0 `10.200.0.2/24`、`nvidia-smi -L` 正常；实例 `running`、未暂停。

## 10. 磁盘不足（§7.1.11）——通过

平台以 `SPARK_AGENT_DATA_SIZE_GIB=100000` 重启，机器空闲时下单：

- `create_instance` failed：「磁盘空间不足：剩余 3526 GiB，需要 100100 GiB（数据盘 100000 GiB + 预留 100 GiB）」。
- 实例 `failed`，余额不变（订单全额退回），机器转 `faulty`，critical 告警 `create_failed`，正文含「磁盘空间不足」。Spark 上没有留下容器与镜像文件。
- 恢复默认配置重启平台，后台开 / 关维护后机器回到 `idle`。

## 最终版本复测（Agent 367c6c2，整分支审查修复之后）

- 部署（替换二进制并重启服务）后：`DOCKER-USER` / `INPUT` 跳转在第 1 位，无重建、无告警；新实例开通正常。
- 隔离跳转位置（审查 M5）：手工 `iptables -I DOCKER-USER 1 -j RETURN`（20:33:30）→ 20:33:40 Agent 告警「iptables jump not first in chain, moving it to position 1」并把 `-i spark0 -j SPARK-FWD` 移回第 1 位。之后 5c1f785 把移动改为「先插入再按序号删除」，消除了中间无隔离的窗口（单元测试覆盖，未再上机）。
- 重启（20:34:47）：测试机重启后连到了另一个网段（Agent 经 192.168.0.129 连入平台，192.168.1.221 不再存在）。平台立即告警 `lan_ip_mismatch`；容器端口绑定的 192.168.1.221 不存在，Agent 不调用 `docker start`（避免缺陷 B2），实例在宽限期内上报 `unknown`、不误报；10 分钟后（20:45:20）上报 `stopped` 并产生 critical `container_down`。这些行为符合设计。
- 测试机切回原网络后（21:25 地址 192.168.1.221 重新出现）：期间 Agent 每 60 秒重试启动、因地址不存在共失败 50 次（始终未调用 `docker start`）；21:26:22「recovered instance started on retry」，容器挂接 1 个网络、`22/tcp -> 192.168.1.221:2222`、tbf + ingress 限速、数据盘挂载；容器内 `/root/data/final.txt` 保留、eth0 `10.200.0.2/24`、`nvidia-smi -L` 正常；Agent 从 192.168.1.221 重新建流，21:26:43 StateReport 报 running，实例 running、未暂停。最终版本重启恢复复测通过。

## 验收中发现并修复的缺陷

| 编号 | 现象 | 原因 | 修复 |
|---|---|---|---|
| A0 | 开通失败「查找容器网卡失败: nsenter -t PID -n cat /sys/class/net/eth0/iflink: No such file or directory」 | `nsenter -n` 只切换网络命名空间，`/sys` 仍是宿主机 sysfs | 5e1790f：在容器 netns 里解析 `ip -o link show dev eth0` 的 `eth0@ifN`（真机输出 `2: eth0@if26:`） |
| A1 | 容器被外部重启（管理员处理 `container_down` 的正式路径）后新 veth 没有限速，Agent 不补 | 只有 Recover / start 时设置限速 | 8ff6a8c：60 秒周期检查运行中实例的 tbf 与 ingress police，缺失即补并告警日志；真机验证 40 秒内补回 |
| A2 | Agent 启动后白名单最多 60 秒不生效 | Recover 先用空白名单建链，收到 AgentConfig 后要等下个周期 | 47c151e：白名单变化立即唤醒规则刷新；真机 108ms 内重建 |
| A3 | 撤销接入后 Agent 日志只有 `EOF` | gRPC 流被服务端终止时 Send 返回 io.EOF，真实状态需 Recv 取得 | 51d4be6：初次发送遇 EOF 时取 Recv 的状态；Unauthenticated 打 Error 并提示重新接入 |
| B1 | 宿主机重启后容器启动失败「failed to bind host port 192.168.1.221:2222/tcp: cannot assign requested address」，之后不再重试 | 局域网地址（WiFi DHCP）比 Agent 晚 2 秒就绪；该机未启用 *-wait-online，`network-online.target` 不保证有地址 | b9e57bc：Recover 启动失败的实例进入待重试集合，每个周期重试，平台指令接管时移出 |
| B2 | 上述失败之后再 `docker start` 成功，但容器只有 `lo`、没有 eth0 | Docker 29 在那次失败后清掉了容器的网络挂接（`NetworkSettings.Networks = {}`，`HostConfig` 仍在） | f0393ac：端口绑定地址不在本机时不调用 `docker start`（留给重试）；网络挂接丢失时先 `docker network connect`；启动后找不到 veth 或限速失败则停掉容器（fail closed） |

## 观察（不阻塞，列入遗留）

- 开通失败到洗机之间的 StateReport 会对已 failed 的实例产生一条 `orphan_container` 告警，属于噪声。
- `machine_offline` 告警在机器恢复在线后不会自动解除。
