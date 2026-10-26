# 计划 13 真机验收记录（多机集群）

## CP-0 单机回归（192.168.1.221，计划 Task 17，规格 §16.4 CP-0）

- 日期：2026-10-26 14:43～14:58（北京时间）。持有 `spark.lock` 的时段为 14:43:06～14:57:26。
- 机器：DGX Spark `spark-3479`（GB10，128 GB 统一内存），后台「B 区」B-01（machine 1）。没有 CX-7；有线口 `enP7s7`（r8127）未插线，管理网卡是 WiFi `wlP9s9`（mt7925e，192.168.1.221/23）。没有改 WiFi 配置。
- 用户实例：`ins-eckhaykk`（用户 1，整机，`spark/llm-serving:25.09`，vLLM 应用 `dep-bfld7w5oe3r8` 跑 `Qwen/Qwen3-8B`，端口 8000 私有）。全程没有释放、重建或重启它。
- 结论：**CP-0 通过（受限项见下）**。第 1、2、5 项通过；第 3 项「现有实例 / 规则 / SSH / 应用」部分通过，「新下单 → 开通 → 释放 → 洗机」与第 4 项 Token 商城 N = 1 抽查因 221 被用户实例占用、没有空闲机器而**未做**，挪到 CP-1（有空闲机器时首先补做）。发现 1 个低严重度缺陷 D-1，已修复（`63c138b`）；观察 O-1～O-7。

### 版本与升级步骤

| 项 | 升级前 | 升级后（验收结束时运行的版本） |
|---|---|---|
| 平台 | `.devdata/platform-main`（main 分支构建，10-25 22:53 启动，pid 51171） | `.devdata/platform-p13-1d25ff8`（`feat/plan13-multinode` @ `1d25ff8`，`make web` + `go build ./cmd/platform`），14:54:06 起运行 |
| 数据库 | 开发库 `spark`，迁移版本 25 | 迁移版本 26（`00026_multinode`，平台启动时 goose 自动执行） |
| Agent | `f5e6a66`，SHA-256 `1c82e218…7e6c` | `1d25ff8`，SHA-256 `462e4cb1176dbabe3bc44f3ee604e00f827778461a2385ab7eb993836e935f0a`（`make agent`） |

1. 14:44 记录升级前快照（容器、vLLM、iptables、路由，见「对照」）。
2. 14:46:36 `kill -TERM` 旧平台，确认退出后用 `CREATE DATABASE spark_pre_p13_20261026 TEMPLATE spark` 做库备份（同一 PostgreSQL 实例 :5433；开发机没有 `pg_dump`）。
3. 14:46:54 用同一份 `.devdata/main.env`（17 个 `SPARK_*` 变量，与旧进程逐一比对相同）、同一 cwd 启动 `3b7f143` 构建的平台，日志 `.devdata/platform-p13.log`。启动后 goose 版本 25 → 26，无 WARN / ERROR（除原有的「AI 管家未开启」）。旧 Agent 重连后后台显示 `agent_features = 1`、就绪原因「未加入集群网络」，`net_info = null`（旧 Agent 不上报）。
4. Agent：二进制 `scp` 到 221 的 `/tmp`，先用新二进制跑 `sudo spark-agent selfcheck`（exit 0），再 `cp -p` 备份旧二进制为 `/usr/local/bin/spark-agent.pre-p13-1c82e218`，`install -m 0755` 替换，`systemctl restart spark-agent`。没有重新接入（证书 / 令牌不变），所以不走 `install.sh`。14:48:05 装 `3b7f143`；修复 D-1 合并后 14:53:57 换成 `1d25ff8`。
5. 14:54:06 平台换成 `1d25ff8` 构建（只多了 D-1 与验收脚本，平台代码与 `3b7f143` 相同），同样 SIGTERM + 同 env 启动。

**回滚方法**（未使用）：停新平台 → `ALTER DATABASE spark RENAME TO spark_p13_failed; ALTER DATABASE spark_pre_p13_20261026 RENAME TO spark`（或在 `spark` 上执行 `00026` 的 `+goose Down`）→ 用 `.devdata/main.env` 启动 `.devdata/platform-main`；221 上 `sudo install -m 0755 /usr/local/bin/spark-agent.pre-p13-1c82e218 /usr/local/bin/spark-agent && sudo systemctl restart spark-agent`。旧 Agent 对 `SPARK-CLU` 链与 `sch+` 跳转不感知，留着无害。

### 1. 新 Agent 自检、网卡上报、重启不翻转——通过（D-1 修复后）

- `sudo spark-agent selfcheck`（`1d25ff8`）集群网卡一节：

  ```
  ok: iface enP7s7 up=false speed=0 driver=r8127 rdma=- active=false
  ok: iface wlP9s9 up=true speed=0 driver=mt7925e rdma=- active=false wireless=true
  WARN: 未发现 ConnectX-7 网卡，集群将只能使用普通以太网
  ok: ip_forward 1
  ok: 集群网段 10.210.0.0/16 无冲突
  ```

  其余 20 项与旧版相同全为 ok，exit 0。`3b7f143` 版本的 `wlP9s9` 一行没有标出无线，见 D-1。
- Hello 上报（后台 `GET /machines/1`）：`agent_version = 1d25ff8`、`agent_features = 3`（共享实例 + 多机集群）、`net_info.ifaces` = `enP7s7`（down、r8127、wireless=false、无 IPv4）与 `wlP9s9`（up、mt7925e、**wireless=true**、`192.168.1.221/23`），`lan_iface = ""`（O-3），无 RDMA 字段。
- 后台机器详情「集群网络」卡（Playwright 1.64 无头浏览器，复用 API 登录的会话 cookie，截图在验收会话 scratchpad `t17/machine-net-card.png`，不入库）：所属集群网络「未加入」、集群网卡「自动（管理网卡 wlP9s9）」、就绪判定「未就绪：未加入集群网络」、Agent 能力「共享实例、多机集群」、管理网卡 `wlP9s9`；网卡表两行：`enP7s7` 未连接 / 1500 / r8127，`wlP9s9`「管理网卡」已连接 / 1500 / 192.168.1.221/23 / mt7925e / 无线「是」。与 Hello 一致。
- 重启不翻转：把 221 加入临时 `lan` fabric（见第 5 项，`cluster_ready = true`、`cluster_net = 192.168.0.0/23`）后，14:49:26 `sudo systemctl restart spark-agent`，同时每 0.5 秒查一次库（共 60 次，30 秒）：`cluster_ready / cluster_net / agent_online` 60 次全是 `true / 192.168.0.0/23 / true`，`net_info_at` 由 14:49:02 更新到 14:49:22（新 Hello），没有出现一次 `false`。

### 2. 镜像前置——记录完成（cupy 缺失，PP > 1 不可用）

- `docker run --rm spark/llm-serving:25.09 python3 -c "import cupy, ray.dag"` → `ModuleNotFoundError: No module named 'cupy'`。单独 `import ray.dag` 成功，Ray 2.49.2；vLLM `0.10.1.1+381074ae.nv25.09`，torch `2.9.0a0+50eac811a6.nv25.09`。
  - 结论：流水线并行 `PP > 1` 不可用（与 T8 结论一致，后台模型表单与服务端已拒绝 PP > 1）。CP-1 第 6 项只测 TP 2。是否在 DGX 上给镜像补 `cupy` / `ray[cgraph]` 记入 followups，由协调人决定。
- `which ping ibv_devinfo curl`：`/usr/bin/ibv_devinfo`、`/usr/bin/curl`，**没有 ping**（rc=1）。连通性验收一律用 curl / python。
- Ray 选 IP 基线（`ray.init(num_cpus=1, object_store_memory=100MB)` 后 `ray.util.get_node_ip_address()` 与 `ray.nodes()`；容器限 4 GB 内存，不碰 GPU，跑完即删）：

  | 容器网络 | 网卡 | 默认路由 | Ray 选中 |
  |---|---|---|---|
  | 只有 `spark0`（同普通实例） | `eth0 10.200.0.3/24` | `via 10.200.0.1 dev eth0` | `10.200.0.3` |
  | `spark0` + 第二张网卡（docker `bridge`） | `eth0 10.200.0.3/24`、`eth1 172.17.0.2/16` | `via 172.17.0.1 dev eth1` | `172.17.0.2` |

  Ray 默认取「默认路由所在网卡」的地址。集群容器的 `spc0` 只有 `10.210.C.0/24` 路由、默认路由仍在 `eth0`（spark0），所以**不显式指定就会选 `10.200.0.x`**；`vllm-node` / `vllm-cluster` 的 `app.sh` 已对 head 与 worker 都传 `ray start --node-ip-address="$SPARK_CLUSTER_IP"` 并导出 `VLLM_HOST_IP`。CP-1 对照：`ray status` 与 vLLM 日志里的节点 IP 应为 `10.210.x.11/12`。
- `deploy/image/tests/nccl_allreduce.py`（CP-1 起使用）在 221 单卡上冒烟：`torchrun --nnodes 1 --nproc-per-node 1 … --size-mb 16 --iters 5` → `correct=True`（单卡 busbw 为 0，符合定义）。

### 3. 普通实例与规则——现有实例部分通过；新开通 / 释放 / 洗机未做

对照（升级前 14:44:39 / 平台与 Agent 都换成 `1d25ff8` 后 14:56:51，同一脚本采集）：

| 项 | 升级前 | 升级后 |
|---|---|---|
| 容器 `ins-eckhaykk` | running，StartedAt `2026-10-25T12:32:24Z`，restarts 0，`10.200.0.2`，`22/tcp → 192.168.1.221:2222` | 完全相同（容器内 sshd 1024860、vLLM 1049947 进程号与 etime 连续） |
| vLLM `/health`（容器内 127.0.0.1:8000） | 200 | 200（三次 Agent / 平台切换后各测一次，均 200） |
| 一次 chat（`Qwen/Qwen3-8B`，`1+1=? /no_think`，API Key 在容器内从进程参数读取、未输出） | `2`，200，0.72 s | `2`，200，0.44 s |
| 应用部署 `dep-bfld7w5oe3r8` | running / ok | running / ok，新平台 14:51 起继续轮询（`last_polled_at` 前进、`unreachable_count = 0`） |
| 实例状态 | running，未暂停，到期 11-01 20:32 | 相同（后台实例视图 `cluster = null`、`cluster_id / cluster_rank = null`） |
| `iptables -S SPARK-FWD` / `SPARK-IN` | 8 条私网丢弃 + ACCEPT / ESTABLISHED + DROP | **逐字相同** |
| `DOCKER-USER` | `-i spark0 -j SPARK-FWD` | 前 3 条为跳转组：`-i spark0 -j SPARK-FWD`、`-i sch+ -j SPARK-CLU`、`-o sch+ -j SPARK-CLU` |
| `INPUT` | `-i spark0 -j SPARK-IN` | 前 2 条为跳转组：`-i spark0 -j SPARK-IN`、`-i sch+ -j SPARK-IN` |
| `SPARK-CLU` | 不存在 | `-m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT`、`-j DROP`（无集群；见 O-1） |
| nat / 路由 / `tc` | MASQUERADE `10.200.0.0/24`、DNAT 2222 → 10.200.0.2:22；无 `10.210` 路由 | 相同 |

- 规则不反复重建：Agent 日志里 `iptables chain rebuilt` 只出现一次（14:49:05，`SPARK-CLU instances=0`，首次巡检把建链时的单条 DROP 规范化，见 O-1）；之后 14:49:26～14:53:57（4.5 分钟）与 14:53:57～14:57:23（3 分 26 秒）两段都是 0 次，跳转组修复日志也只在第一次升级启动时出现一次（O-2）。
- SSH：网关主机密钥 `SHA256:z9D/krcbX4NmfQPqw44OKBJouAuPUx+4HQJv0FIxxuo` 不变（与计划 3 记录相同）；`ssh -p 2222 foo@网关` 仍显示 Banner「用户名应为实例 ID…」；平台上游密钥直连容器 sshd（`ssh -i .devdata/upstream.key -p 2222 root@192.168.1.221`）升级前后都成功，容器内 `/.spark/` 只有 `authorized_keys hostkeys init.sh`、没有 `/etc/spark-cluster.json`（非集群实例不注入）。新平台的应用轮询经 SSH 上游持续成功。**用户本人经网关登录没有测**：没有用户的密码 / 私钥（O-5）。
- 未做：新下单 → 开通 → SSH / Web → 释放 → 洗机全流程需要空闲机器，221 被 `ins-eckhaykk` 占用且不能释放，挪到 CP-1（新机器接入后、建 fabric 前先跑一遍普通实例全流程）。

### 4. Token 商城 N = 1 节点抽查——未做

开发库 `spark` 里没有 Token 商城模型与节点（`llm_models` 空），也没有空闲机器可建节点。新平台下后台 `GET /llm/models`、`/llm/pool`（`fabrics: []`）、`/llm/nodes`、`/clusters` 均 200 且为空，仪表盘 `clusters` 全 0。挪到 CP-1：两台新机器先用一台跑 N = 1（Qwen3-4B），再做 2 台组。集成测试 `TestLLMTwoNodeGroupE2E` 等已覆盖 N = 1 路径不变。

### 5. 单台 fabric 不可用、cx7 判定——通过

- 14:48:58 `POST /fabrics {name:"CP0 单机以太网", kind:lan, isolation:routed}` → 201（id 1）；`PUT /machines/1 {fabric_id:1}` → 200，`cluster_ready = true`、`cluster_net = 192.168.0.0/23`、原因空。
- 控制台接口：`GET /api/v1/products` → `spark-1` 的 `clusters = {"2": {available: 0, rdma: false}, "4": {available: 0, rdma: false}}`；`GET /api/v1/availability?product_id=1&cluster_size=2` → `{"available":0,"rdma":false}`。市场页「集群 2 台」按 `available = 0` 禁用（前端逻辑见 T14，页面未在此登录用户点开）。
- `PUT /fabrics/1 {kind:cx7, isolation:direct}` → 200；机器 `cluster_ready = false`、原因「**未找到已连接的 CX-7 网卡**」（规格写「未找到 CX-7 网卡」，文案更具体，O-4）；products 仍 2 / 4 均 0。
- 收尾：`PUT /machines/1 {fabric_id:null}` → 原因恢复「未加入集群网络」；`DELETE /fabrics/1` → `{ok:true}`，`GET /fabrics` 为空。用户实例不受影响（fabric 只影响就绪判定，非集群实例不收 ConfigureCluster）。

### 缺陷

| # | 严重度 | 描述 | 修复 |
|---|---|---|---|
| D-1 | 低 | 规格 §16.4 CP-0 第 1 项要求自检「列出 `wlP9s9`（wireless）」，`selfcheck` 的网卡行没有无线标记（Hello 的 `net_info` 有 `wireless`，后台网卡表也显示「无线：是」，只有自检看不出） | `63c138b`：无线网卡行末尾加 `wireless=true`；`TestSelfcheckClusterSection`（221 真机桩数据）先改期望看到失败再修；`go test ./cmd/spark-agent` 通过；真机复测见第 1 项 |

### 观察

- O-1：`SPARK-CLU` 在 `EnsureJumpGroups` 建链时只有一条 `-j DROP`，第一次 `SyncRules`（Agent 启动约 60 秒后）按 `ExpectedCluRules` 重建为「ESTABLISHED ACCEPT + DROP」并记一次 `iptables chain rebuilt`。之后稳定、不再重建。规格 CP-0 第 3 项写「`SPARK-CLU` 只有 `-j DROP`」，稳态实际多一条 ESTABLISHED（无集群时功能等价）。不改代码，规格文字以本记录为准。
- O-2：从旧 Agent 升级后第一次启动记两条 WARN「iptables jump group not in place, repairing」（INPUT、DOCKER-USER 各 1 条，即补上 `sch+` 跳转），之后两次重启都没有再出现。属预期。
- O-3：Hello 的 `net_info.lan_iface` 为空（Hello 早于 AgentConfig，Agent 尚不知道 `lan_ip`）；平台按 `machines.lan_ip` 认出管理网卡 `wlP9s9`，后台显示正确。符合规格。
- O-4：cx7 判定文案是「未找到已连接的 CX-7 网卡」，比规格多「已连接」，更准确。
- O-5：SSH 网关的「用户登录」没有端到端测（没有用户凭据，也没有为验收往用户账号加密钥）；验证了网关主机密钥 / Banner、上游密钥直连与平台经上游轮询应用。网关代码本计划未改。验收时两次故意失败的认证（`ins-eckhaykk`、`foo`）记在平台日志 `ssh auth failed`，来源 192.168.1.218，`ip_blocked=false`。
- O-6：开发库还没有导入 `vllm-node 1.1.0` / `vllm-cluster 1.0.0`（只有 vllm 1.0.4 等计划 7 的应用）。CP-1 前协调人执行 `platform admin app import deploy/apps` 并发布（`vllm-cluster` 公开后控制台应用市场会多一张卡）。
- O-7：开发库备份 `spark_pre_p13_20261026` 留在 :5433，CP-3 收尾时确认后再删。221 上旧 Agent 备份 `/usr/local/bin/spark-agent.pre-p13-1c82e218` 同样保留到 CP-3。
