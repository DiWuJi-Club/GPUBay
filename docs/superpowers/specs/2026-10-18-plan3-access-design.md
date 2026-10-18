# Spark Cloud — 计划 3：接入层设计文档（SSH 网关 + Web 入口）

- 日期：2026-10-18
- 状态：已批准（Fable 评审 2026-10-18；负责人已授权由 Fable 设计并确认）
- 上位文档：`2026-10-14-spark-cloud-mvp-design.md`（下称「总设计」）§5、§7.1；`2026-10-18-plan2-agent-design.md`（下称「计划 2」）§4.2
- 范围：用户经公网 SSH 登录实例（`ssh ins-xxxxxxxx@ssh.<domain>`，密码或公钥）、经公网 HTTPS 访问实例内的 Web 端口（`https://ins-xxxxxxxx-<port>.<ingress_domain>`，默认私有、可设公开）、控制台的 SSH 公钥 / 密码 / 端口管理、后台对应视图、TLS 终止、部署示例与真机验收。
- 不在范围：见 §12。

## 0. 现状与约束（实现时不必再查）

**代码事实**
- `GET /api/v1/instances/{id}` 的 `ssh_command` 为 `ssh <id>@ssh.<SPARK_DOMAIN>`（`internal/api/instances.go`），仅 running 时输出；`GET /instances/{id}/password` 解密 `instances.ssh_password_enc`（`rental.Password`，16 位随机密码，`secretbox` 加密）。
- 控制台会话 Cookie `sc_session`（host-only，`Path=/`，`SameSite=Lax`，`HttpOnly`，staging/prod 带 `Secure`）；后台 `sc_admin`（`Path=/admin/`，`Strict`）。`identity.Authenticate(token)` 查 `sessions` 表。两者都**没有 `Domain` 属性**，不会发到任何子域名。`internal/api/server.go` 的注释「上传接口依赖 SameSite=Lax 会话 Cookie 防跨站提交」在租户子域上线后不再成立（§4.2.1）。
- 公网 `http.Server` 由 `app.Servers()`（`internal/app/app.go:304`）创建，带 `ReadTimeout 30s / WriteTimeout 60s`（上传慢连接防护），本计划保留不改。
- 容器 sshd 由 Agent 生成的 `/.spark/init.sh` 启动（`internal/agent/executor/sshdir.go`）：`PasswordAuthentication no`、`AuthorizedKeysFile /.spark/authorized_keys`（平台上游公钥）、主机密钥 `/.spark/hostkeys/ssh_host_ed25519_key`（Agent 在创建时生成，已存在则保留）。**配置里没有 `Subsystem sftp`**，所以现在 `sftp` 与 OpenSSH ≥ 9.0 的 `scp`（默认走 SFTP）都会失败，本计划修正（§5.2）。
- 容器 22 端口发布到 `<lan_ip>:2222`（`docker create -p`），端口发布在创建时固定（验收缺陷 B1/B2：绑定地址不存在时 Docker 29 会丢掉网络挂接）。上游私钥 `SPARK_UPSTREAM_SSH_KEY_FILE`（ed25519），公钥由网关 `toProtoCommand` 投递 Create 指令时填入（`fleet.CreateInstancePayload` 只有 `image_ref`）。
- 容器网络 `spark0`（10.200.0.0/24），`SPARK-FWD` / `SPARK-IN` 规则在真机上与 `ExpectedFwdRules` / `ExpectedInRules` 逐行相等；`SPARK-IN` 丢弃容器到宿主机的一切新连接；容器 sshd 代用户拨出的连接（`-L`/`-D`）从 `spark0` 出去，同样受这两条链约束。
- `agent_commands.type` 没有 CHECK；`fleet.CommandType` 只有 create / start / stop / wipe / pull_image；计划 2 把 `UpdatePorts` 留给本计划。
- 实例 ID 形如 `ins-` + 8 个 `[a-z0-9]`；同一机器同时只有一个实例；`instances_machine_active_uniq` 保证。
- `AdminMigrate`（`internal/rental/migrate.go`，唯一的换机路径）新建一个实例 ID 并重新生成密码；`instance_transfers` 记录迁移。
- `golang.org/x/crypto v0.58.0` 已在 `go.mod`（Agent 用它生成主机密钥），`x/crypto/ssh` 可直接用。
- 迁移编号到 `00017_agent.sql`；本计划是 `00018_access.sql`（现有迁移都带 `+goose Down`，本迁移同样要带）。
- e2e：Playwright 用 Chrome（`E2E_BROWSER_CHANNEL`），控制台 `http://127.0.0.1:18080`，钩子 `:18090`，fake Agent 步进模式；覆盖表 ID 维护在 `web/e2e/coverage.json`。已有 ID 到 `C-DET-10`、`C-INS-04`、`C-ACC-02`、`A-INS-09`、`A-USR-06`、`A-MCH-09`。
- `web/console/src/router.ts` 第 38 行的守卫与 `LoginView` 都用 `safeRedirect(to.query.redirect)` 做**路由导航**；`safeRedirect` 规则：以 `/` 开头、不以 `//` 开头、不含 `\`。
- `audit_logs.actor_type` 允许 `user`；`audit.Actor` 目前只有 `AdminID`，`AdminID=0` 记为 system。

**环境事实**
- 开发 Mac 无 Docker、无 brew；Go 1.27、Node 22。
- 测试机 192.168.1.221（Ubuntu 24.04、Docker 29.6.2、nf_tables），验收时控制面跑在 Mac（192.168.1.218）；局域网 WiFi DHCP，地址可能变。
- 生产：控制面 docker compose；公网只开 22 与 443/80；机器在内网；compose 里容器主机名默认是容器 ID，每次重建都变。

## 1. 架构与核心决策

```
                  公网
     ┌─────────────┼──────────────────────────────┐
  SSH :22                                  HTTPS :443 / HTTP :80
     │                                             │
     ▼                                             ▼
┌───────────────────────── platform（Go 单体，1..N 副本）──────────────────────┐
│ sshgw（内置 SSH 网关）                hostmux：按 Host 分流                     │
│  用户名 = 实例 ID                      ├ <domain>                → 控制台 + /api│
│  密码 / 公钥在此校验                    └ ins-<id>-<port>.<ingress_domain>      │
│  上游：平台私钥登录容器 sshd                → ingress（ReverseProxy，票据 Cookie） │
│        │                                             │                       │
│        └──────── 一条 SSH 连接（lan_ip:2222）─────────┘  direct-tcpip 通道       │
└──────────────────────────────────────┬───────────────────────────────────────┘
                                       │ 机房内网
                              容器 sshd（10.200.0.x:22 → lan_ip:2222）
                              Web 端口只在容器内监听（127.0.0.1 或 0.0.0.0 均可）
```

决策编号沿用任务书：决策 1（SSH 网关）、3（Web 入口）、4（流量路径）在本节；决策 2（凭据）在 §3，5（数据与接口）在 §6～§8，6（失败与安全）在 §9，7（测试）在 §10，8（范围）在 §12。

### 1.1 决策 1：SSH 网关自研（`internal/sshgw`，内嵌 platform），不用 sshpiper

| | sshpiper + gRPC 插件 | 自研（x/crypto/ssh） |
|---|---|---|
| 开发机可测性 | 需要 sshpiper 二进制（无 brew；`go install` 可以），`go test` 得拉起外部进程并对齐其插件协议版本 | 纯进程内：测试里起网关 + 假「容器 sshd」+ ssh 客户端，三者都是 x/crypto/ssh |
| 转发特性 | 由 sshpiper 版本决定；哪些通道透传不受我们控制 | 通道与请求白名单写在代码里（§2.4） |
| 限流 / 审计 / 会话上限 | 插件里只有认证回调，连接计数要另做 | 同一进程，直接用现有限流器模式、`alerts`、数据库 |
| 主机密钥、多副本 | 文件配置，每个副本各管一份 | 存数据库（与 CA 同法），副本一致 |
| 部署 | 多一个容器 + 插件进程 | 平台二进制多监听一个端口 |
| 风险 | 对第三方协议实现的依赖 | SSH 中继细节（窗口、请求回复、退出码）需自己做对——用真机验收清单兜底 |

选自研。SSH 中继（服务端握手 → 客户端连上游 → 双向转发通道与请求）正是 sshpiper 的内部结构，约 1000 行 Go，且和项目一贯做法一致（exec docker 而非 SDK）。网关作为 platform 的一个监听器（`SPARK_SSH_ADDR`），包内不依赖 HTTP，将来需要时可拆成独立二进制。

### 1.2 决策 3：Web 入口自研（`internal/ingress`），TLS 由 platform 按文件终止（可选），不用 Traefik

| | Traefik（HTTP provider + ForwardAuth） | 自研（httputil.ReverseProxy） |
|---|---|---|
| 开发机可测性 | 需要 Traefik 二进制，`go test` 无法覆盖路由 + ForwardAuth 链路 | 进程内 httptest 全覆盖，WebSocket 走 Go 1.20+ 的 101 透传 |
| 跨子域会话 | ForwardAuth 只能看到 Cookie，**控制台 Cookie 本来就不会发到子域**，仍需本设计 §4.2 的票据跳转 | 同样的票据流程，少一跳 |
| 证书 | 内置 ACME DNS-01（要配 DNS 服务商） | 平台读证书文件并热加载；证书由 acme.sh 侧车（DNS-01）或商业证书提供 |
| 上游 | 要求端口在宿主机可达（需 §1.3 的 DNAT 方案） | 直接复用 SSH 通道（§1.3） |

选自研。证书自动化不进 platform（lego 之类的依赖和 DNS 插件面太大），用 `acme.sh` 侧车写文件、platform 热加载（§4.5）。前面若已有反向代理也可以（`SPARK_TRUST_PROXY=true`，代理原样转发 Host）。

### 1.3 决策 4：Web 流量经 SSH 通道进容器，不发布端口、不做 DNAT、不需要 `UpdatePorts`

| | 创建时发布固定端口段 | Agent 管理 iptables DNAT | 经容器 sshd 的 `direct-tcpip` 通道 |
|---|---|---|---|
| 用户任意端口 | 不行（用户端口不可预知） | 可以 | 可以 |
| Agent / 协议改动 | Create 改 + 无法增删 | 新增 nat 链、`UpdatePorts` 指令、与 Docker 29 的 nat 链共存 | **无**（端口纯粹是平台数据） |
| 隔离 | 发布到 lan_ip，整个内网可达 | 要加只放行控制面来源的规则 | 机器上不多开任何端口；只有持有上游私钥的控制面能进 |
| 应用只监听 127.0.0.1（Jupyter 默认） | 不通 | 不通 | **通**（通道在容器网络命名空间里连 127.0.0.1） |
| 代价 | — | iptables 复杂度、Docker 重启漂移 | 加密开销；sshd 挂了 Web 也断（此时 SSH 本来也断） |

选 SSH 通道：ingress 对每个实例维持一条到 `lan_ip:2222` 的 SSH 连接（平台上游私钥），每个 HTTP 请求开一个 `direct-tcpip` 通道到容器内 `127.0.0.1:<port>`。**`UpdatePorts` 指令取消**，`SPARK-FWD` / `SPARK-IN` 与端口发布完全不变。吞吐对 Web UI 足够（x/crypto/ssh 单连接数百 MB/s 量级）；大文件请走 `scp`/`sftp`，同样经网关。

隔离成立的前提（硬性约束）：**网关与 ingress 从不替用户从控制面拨号**。sshgw 只把原始 `NewChannel` / `Request` 转给上游（§2.4）；ingress 自己发起的 `direct-tcpip` 目标固定为容器内 `127.0.0.1:<port>`。用户 `-L`/`-D` 的目标由容器 sshd 在容器网络命名空间里拨出，受 `SPARK-FWD`/`SPARK-IN` 约束，碰不到机房内网与宿主机。

### 1.4 负责人问题的裁定（评审提出，由负责人授权裁定）

| # | 裁定 | 理由 |
|---|---|---|
| 1 | 新增 `SPARK_INGRESS_DOMAIN`（默认等于 `SPARK_DOMAIN`）；两者不同时租户子域落在独立注册域上；生产部署说明**推荐独立域名**。无论是否独立，§4.2.1 的 `__Host-` / `Sec-Fetch-Site` / `Origin` 防护都保留 | 独立域名彻底消除同站风险（GitHub Pages、JupyterHub 通行做法），但不强制，避免上线被域名采购卡住 |
| 2 | 证书两条路都支持（文件热加载对 acme.sh DNS-01 侧车产物与商业证书一视同仁）；选哪条列入「上线前核对」 | 实现不区分来源，决定可推迟到部署时 |
| 3 | 控制面宿主机自己的 sshd 挪到其它端口（部署文档步骤）；compose 把 22 映射到网关 | 用户命令保持无 `-p` 最简；不需要第二个公网地址 |
| 4 | 公开端口默认允许，控制台设为公开时显示警告；后台可**强制设为私有**（接口 + 审计）；使用条款是负责人的后续事项，不进代码 | 保持 MVP 行为，处置手段齐备 |
| 5 | 机器失联（暂停计时）时**不主动断开**已有 SSH / Web 会话，上游断了自然结束；暂停计时规则沿用计划 2 | 断会话不会停掉 tmux 里的训练任务，只会打断用户 |

## 2. SSH 网关（`internal/sshgw`）

### 2.1 监听与身份
- 监听 `SPARK_SSH_ADDR`（默认 `:2222`）；生产由 compose 把 22 映射到 2222（宿主机 sshd 先挪走，§11）。`ssh_command` 用 `SPARK_SSH_PUBLIC_PORT`（默认 22）：非 22 时输出 `ssh -p <port> <id>@ssh.<domain>`。主机名固定 `ssh.<SPARK_DOMAIN>`；用户名就是实例 ID，所以验收时直接 `ssh -p 2222 ins-xxxx@192.168.1.218` 也行，不需要 DNS。
- `ServerVersion = "SSH-2.0-SparkCloud"`；主机密钥 ed25519，首次启动生成，`secretbox` 加密存 `platform_pki.ssh_host_key_enc`（`UPDATE ... WHERE id = 1 AND ssh_host_key_enc IS NULL` 后重读，多副本只会留下一把）。`platform admin ssh-host-key` 子命令打印公钥与 SHA256 指纹，写进帮助页「首次连接请核对指纹」。README 在 `SPARK_SECRET_KEY` 轮换提示处加一句：轮换会连带丢失网关主机密钥（用户会看到主机密钥变更警告）。
- 认证回调前的 `BannerCallback` 按用户名查实例并给出中文说明（§2.3 表）。**实例是否存在与其状态，对知道实例 ID 的人经 Banner 可见——接受**（ID 是 36^8 随机空间，不可枚举）；用户名不是 `^ins-[a-z0-9]{8}$` 或实例不存在 → Banner「用户名应为实例 ID，例如 ins-a1b2c3d4」，认证一律失败。**密码错误与公钥不匹配对客户端不可区分**（都是普通认证失败）。

### 2.2 认证
- `PasswordCallback`：实例 `password_auth = true` 且 `subtle.ConstantTimeCompare(解密密码, 输入)`；否则失败。关闭密码登录时失败原因只记日志。
- `PublicKeyCallback`：按 `ssh_keys WHERE user_id = <实例主人> AND fingerprint = SHA256(<key>)` 查；命中则在 `Permissions.Extensions` 记 `key_id`。会话建立后 `UPDATE ssh_keys SET last_used_at`。
- `MaxAuthTries = 6`（与 OpenSSH 一致）；握手（含认证）总超时 30 秒。
- 暴力破解计数**按连接而不是按认证尝试**：OpenSSH 客户端会把 agent 里的每把公钥逐个试探，每次不匹配都触发 `PublicKeyCallback`，若按尝试计数，一个 agent 里有 4 把密钥的用户连两次就会把自己锁掉。规则：一条连接关闭时仍未认证成功 → 计 1 次失败（公钥查询不计，密码错多次也只计 1 次）。来源 IP（IPv6 按 /64）10 分钟内失败连接 ≥ 10 → 该 IP 10 分钟内新连接直接断开；同一实例 10 分钟内失败连接 ≥ 30 → `RaiseAlertOnce(warning, ssh_bruteforce, machine, instance, "实例 <id> 的 SSH 登录失败次数超过阈值，详情见日志")`（文案稳定，不含次数与 IP）。进程内计数，多副本各自计数（已接受）；e2e-server 的 `DisableRateLimits` 同样关闭这两项。
- 连接上限：每来源 IP（IPv6 /64）并发连接 ≤ 32，每实例并发会话 ≤ 16（`SPARK_SSH_MAX_SESSIONS_PER_INSTANCE`，默认 16），超限断开并提示「连接数过多」。

### 2.3 状态门控（对新连接）

| 实例状态 | Banner | 认证 |
|---|---|---|
| running 且未暂停 | 无 | 允许 |
| running 且暂停（机器失联） | 「实例所在机器暂时失联，计时已暂停，请稍后重试」 | 拒绝 |
| creating | 「实例正在开通，请稍后再试」 | 拒绝 |
| stopped | 「实例已到期停机，数据保留至 <retain_until>，续费后可继续使用」 | 拒绝 |
| wiping / released / failed | 「实例已释放」 | 拒绝 |
| 机器 `agent_online = false` 而实例仍 running（5 分钟判定窗口内） | 无 | 允许，上游连不上时按 §9 处理 |

已有会话：网关每 10 秒扫描自己持有的会话，实例进入 stopped / wiping / released / failed（到期停机、释放、换机后旧实例）→ 向客户端发 disconnect「实例已停机 / 已释放，连接已断开」并关闭上游。**暂停（机器失联）不断已有会话**（裁定 5），上游断了会话自然结束。续费恢复 running 后下一次连接自然放行，不需要额外动作。

### 2.4 中继
认证通过后：`net.DialTimeout(lan_ip:SPARK_UPSTREAM_SSH_PORT, 10s)` → `ssh.NewClientConn`（用户 `root`，上游私钥，`HostKeyCallback` 固定为 `instances.ssh_host_pubkey`，见 §5.1）。失败则 disconnect「无法连接到实例，请稍后重试」并按 §9 告警。成功后开始中继，每个用户会话一条独立上游连接（上游 sshd 的 `-R` 监听、agent 转发都按会话隔离，与 sshpiper 一致）。

**策略是白名单 + 默认拒绝**；网关不解析、不拨号任何 `direct-tcpip` / `tcpip-forward` 的目标，只把原始 `NewChannel`（类型 + extra data 原样）与 `Request`（名称 + payload 原样）转给对端：

| 方向 | 白名单 | 未列出的处理 |
|---|---|---|
| 客户端 → 上游 `NewChannel` | `session`、`direct-tcpip` | `Reject(UnknownChannelType)`；明确含 `direct-streamlocal@openssh.com`、`tun@openssh.com`、`x11` |
| 上游 → 客户端 `NewChannel` | `forwarded-tcpip`、`auth-agent@openssh.com` | `Reject(Prohibited)`；明确含 `x11`、`forwarded-streamlocal@openssh.com` |
| 通道内请求（双向） | 全部透传（`pty-req`、`env`、`shell`、`exec`、`subsystem`、`window-change`、`signal`、`auth-agent-req@openssh.com`、`exit-status`、`exit-signal`、`eow@openssh.com` 等），`WantReply` 结果回传 | —（通道内请求不设黑名单，由上游 sshd 自行决定；sshd 配置已关 X11，§5.2） |
| 全局请求（双向） | `tcpip-forward`、`cancel-tcpip-forward`、`keepalive@openssh.com`、`no-more-sessions@openssh.com` | 回 `false` 不转发；明确含 `streamlocal-forward@openssh.com`、`cancel-streamlocal-forward@openssh.com` |
| 上游 → 客户端全局请求 `hostkeys-00@openssh.com` | — | **丢弃**（否则客户端 known_hosts 会把容器密钥记到 `ssh.<domain>` 名下） |

- 保活：网关每 60 秒向客户端与上游各发一次 `keepalive@openssh.com`，连续 3 次无回应即关闭。没有空闲超时（训练任务常挂几天）。
- 优雅关闭：收到 SIGTERM 后停止 accept，最多等 `SPARK_SSH_DRAIN_TIMEOUT`（默认 30s）再断开剩余会话（部署会断 SSH，README 注明）。
- 会话记录：建立时插入 `ssh_sessions`，结束时写 `ended_at`、`bytes_up`、`bytes_down`（中继时计数）。持有会话的副本每 60 秒批量 `UPDATE ssh_sessions SET last_seen_at = now() WHERE id = ANY($1)`；清扫器把 `ended_at IS NULL AND last_seen_at < now() - 5 分钟` 的行收尾（`ended_at = last_seen_at`）。这样副本崩溃、compose 重建改名都不会留残留行（不依赖主机名）。日志（slog）：每个会话 open / close 各一条，字段 instance、user、method、ip、duration、bytes。

## 3. 凭据（决策 2）

### 3.1 实例密码（保留）
- 保留现有 16 位随机密码与查看接口。新增重置：`POST /instances/{id}/password/reset` → 新密码（同一长度），`UPDATE instances SET ssh_password_enc`，写 `audit_logs(actor_type='user', actor_id=user_id, action='instance.password_reset', target=instance)`。实例 released / failed 时 `invalid_state`。正在进行的会话不受影响（密码只在认证时用）。
- 每实例可关闭密码登录：`instances.password_auth`（默认 true）。`PUT /instances/{id}/access {password_auth}`；关闭时要求用户至少有一个公钥，否则 `password_auth_requires_key`。后台实例详情显示该开关，不能改（避免运维替用户降安全）。
- 换机（`AdminMigrate`）在同一事务里把 `password_auth` 复制到新实例（§5.1），不出现换机后密码登录被重新打开的退化。

### 3.2 用户级 SSH 公钥（新）
- 表 `ssh_keys`（§6），每用户最多 10 个（`identity.MaxSSHKeys = 10`）。
- 接受 `ssh-ed25519`、`ecdsa-sha2-nistp{256,384,521}`、`ssh-rsa`（≥ 2048 位）；`sk-*` 不接受（网关不做 FIDO）；`ssh-dss` 拒绝。解析用 `ssh.ParseAuthorizedKey`，只保留 `type + base64`，注释丢弃；指纹 `ssh.FingerprintSHA256`。同一用户同一指纹 → `ssh_key_exists`。名称 1～64 字符，缺省取公钥注释（没有则「密钥 N」）。
- 删除公钥不影响已建立的会话（与 OpenSSH 一致）。
- 控制台新页 `/account/ssh-keys`（「我的」页增加入口）：列表（名称、指纹、添加时间、最近使用）、添加（粘贴框 + 名称，前端用正则粗校验，后端为准）、删除（确认）。帮助页增加：用公钥登录、关闭密码登录、核对网关主机密钥指纹、**`ssh-copy-id` 写进容器 `~/.ssh/authorized_keys` 不生效**（sshd 只读 `/.spark/authorized_keys`，公钥在控制台管理）。
- 后台用户详情显示公钥列表（名称、指纹前缀、添加时间、最近使用），只读。

## 4. Web 入口（`internal/ingress`）

### 4.1 路由
- 公网 HTTP 监听器前加 `hostmux`：Host（去端口、转小写）匹配 `^ins-([a-z0-9]{8})-([0-9]{1,5})\.<SPARK_INGRESS_DOMAIN>$` → ingress；其它（含 `<SPARK_DOMAIN>`）→ 现有控制台处理器（行为不变，避免影响既有测试）。
- ingress 内存路由表每 2 秒从数据库刷新（`instance_ports JOIN instances JOIN machines`，200 台规模一条 SQL），字段：instance_id、user_id、port、visibility、state、suspended、machine lan_ip、ssh_host_pubkey。查不到 Host → 404 页「该端口未开放」；实例非 running → 503 页（文案同 §2.3 表，`<meta refresh=10>`）；暂停中（机器失联）→ 新请求 503「机器暂时失联」，已建立的 WebSocket 不主动断（裁定 5）。
- 保留路径 `/.spark/*`（callback、logout、健康页），不转发到容器；帮助页注明。

### 4.2 私有端口的鉴权（票据跳转）
控制台 Cookie 不发子域（§0），而且**绝不能**把 `sc_session` 改成 `Domain=.<domain>`——那样每个租户的 Web 应用（以及其他用户的公开端口页面）都会收到控制台会话。所以每个实例域名单独发一枚 host-only Cookie：

1. 浏览器打开 `https://ins-a1b2c3d4-8888.<ingress_domain>/lab`，没有有效 `sc_ingress` → 302 `<SPARK_PUBLIC_BASE_URL>/api/v1/ingress/authorize?target=<原 URL>`。
2. `authorize`（控制台域，GET，顶级导航所以 Lax Cookie 会带上；**注册在 `requireUser` 组之外**，自己查会话，因为组内未登录返回 JSON 401 而这里要 302）：未登录 → 302 `/login?redirect=/api/v1/ingress/authorize?target=…`——`LoginView` 与 `router.ts` 第 38 行的已登录守卫对以 `/api/v1/ingress/authorize` 开头的 redirect 都改用整页跳转 `location.assign`，不走路由导航；已登录但不是实例主人 → 403 HTML「无权访问该实例」；主人 → 签发票据（HMAC-SHA256，密钥 `HKDF(SPARK_SECRET_KEY, "ingress-ticket")`，内容 `instance_id|port|user_id|session_hash|exp(60s)|nonce`）→ 302 `https://ins-…-8888.<ingress_domain>/.spark/callback?ticket=…&next=/lab`。`target` 校验：`url.Parse` 后 scheme 与端口等于 `SPARK_PUBLIC_BASE_URL` 推导值、host 匹配 §4.1 的实例域名正则、无用户名密码段，否则 400。
3. `/.spark/callback`：验签、未过期、nonce 未用过（进程内 60 秒集合；多副本下票据在另一副本可再用一次，已接受）→ `Set-Cookie: sc_ingress=<签名令牌>; Path=/; HttpOnly; SameSite=Lax; Secure(非 dev)`，有效 12 小时，内容 `instance_id|port|user_id|session_hash|exp` → 302 `next`。`next` 按 `safeRedirect` 同规则：以 `/` 开头、不以 `//` 开头、不含 `\`，且 `url.Parse` 后 `Scheme`、`Host` 为空；不合规则跳 `/`。
4. 之后每个请求：用 `r.CookiesNamed("sc_ingress")` **逐枚验证**（租户页面可用 Domain 属性塞一枚无效同名 Cookie，只取第一枚会把主人挡在门外），任一验签通过、`user_id == 路由表里的实例主人`、`instance_id/port` 匹配、且 `session_hash` 对应的控制台会话仍有效（ingress 按 `session_hash` 查 `sessions` 表，结果缓存 2 秒；控制台登出即失效）→ 转发。换机产生新实例 ID，旧 Cookie 自然失效。
5. `POST /.spark/logout` 清 Cookie（GET 不接受，避免任意页面触发登出）。公开端口（`visibility = public`）跳过 1～4。

转发前**删除**所有 `sc_ingress` Cookie（租户应用看不到它），其它 Cookie 原样透传（应用自己的会话）。

#### 4.2.1 同站（same-site）防护
租户页面 `ins-*.<ingress_domain>` 与控制台 `<domain>` 在默认配置下是同一注册域；浏览器 `SameSite` 只区分跨站不区分兄弟子域，所以控制台原来「`SameSite=Lax` 防跨站提交」的假设被打破：任一租户的公开端口页面能向 `/api/v1/uploads` 提交 multipart 表单并带上受害者 Cookie；租户页面还能 `document.cookie="sc_session=<攻击者令牌>; Domain=<domain>"` 做登录 CSRF（cookie tossing）。两道防线都做，且不因是否使用独立域名而省略：
- (a) `SecureCookie = true` 的环境（staging / prod）控制台会话 Cookie 改名 `__Host-sc_session`（浏览器拒绝子域用 Domain 属性覆写 `__Host-` 前缀 Cookie，且该前缀要求 `Secure; Path=/; 无 Domain`，与现状一致）；dev / e2e 走 http 无法用前缀，保持 `sc_session`，文档说明。读取时按环境取对应名字。
- (b) 控制台对非 GET / HEAD / OPTIONS 请求新增中间件，三段规则：有 `Sec-Fetch-Site` → 只放行 `same-origin` / `none`；没有 `Sec-Fetch-Site` 但有 `Origin` → 必须等于 `SPARK_PUBLIC_BASE_URL` 的 origin；两个头都没有 → 放行（非浏览器客户端：Go `api_test`、Playwright `request`、curl；浏览器在跨源 POST 时总会带 `Origin`，不会落到这一段）。支付回调 `/api/v1/pay/notify/*` 豁免。放在 `requireJSONForWrites` 旁边，`server.go` 的旧注释改掉。
- (c) 同一中间件也加到后台 `/admin/api/v1`（`Origin` 与后台对外地址比较：`SPARK_ADMIN_PUBLIC_BASE_URL`，默认空 = 只接受 `Sec-Fetch-Site` 规则与无头请求；若后台经反向代理挂在 `<domain>` 的兄弟子域上，租户页面与它同站，`sc_admin` 的 `SameSite=Strict` 同样挡不住兄弟子域的提交）。
- 租户 Web 应用自身的 CSRF 防护由应用负责（§9 安全要点、帮助页）。生产推荐把 `SPARK_INGRESS_DOMAIN` 设为独立注册域（裁定 1）。

### 4.3 转发
- `httputil.ReverseProxy`。**连接池按实例隔离**：`http.Transport` 以出站 URL 的 scheme+host 作为空闲连接池键，若所有实例的出站 URL 都是 `http://127.0.0.1:<port>`，实例 A 用过的空闲通道会被复用去服务实例 B 的同端口请求（跨租户返回数据）。因此 `Director` 把出站 URL 设为 `http://<instance_id>:<port>/`（host 唯一），`Transport.DialContext(ctx, network, addr)` 从 `addr` 解析出实例 ID 与端口再调 `upstream.Pool.Dial(instanceID, port)`（§5.3，一条 `direct-tcpip` 通道到容器内 `127.0.0.1:<port>`）；`MaxIdleConnsPerHost = 8`。WebSocket 101 透传走同一 Transport（ReverseProxy 对 101 用 `RoundTrip` 返回的 `ReadWriteCloser`）。单测必须包含「两个实例同端口并发请求不串」。
- 其余：`ForceAttemptHTTP2 = false`、`DisableCompression = true`、`FlushInterval = -1`（SSE / 流式输出不缓冲）、`ResponseHeaderTimeout = 60s`、不限请求体。设置 `X-Forwarded-For / Proto / Host`，`Host` 头保持原值。
- 超时：**不改 `app.Servers()` 的 `ReadTimeout 30s / WriteTimeout 60s`**（控制台上传防护保持），ingress 处理器对每个转发请求用 `http.ResponseController.SetReadDeadline / SetWriteDeadline(time.Time{})` 清除期限（Go 1.20+），长连接与 SSE 不受影响；单测覆盖「经 ingress 的 90 秒 SSE 流不被 WriteTimeout 掐断」。`http.Server.ReadHeaderTimeout 10s` 不变。
- 上游拨号失败 → 502 页「无法连接到实例端口，请确认服务已在容器内监听 <port>」；并按 §9 告警。

### 4.4 端口管理
- `instance_ports`（§6），每实例最多 5 条（`rental.MaxPortsPerInstance = 5`），端口 1～65535 且 ≠ 22；在实例行锁内计数。`visibility` 为 `private`（默认）/ `public`；`note` ≤ 32 字符（如「JupyterLab」）。
- 控制台实例详情新增「Web 访问」卡片：端口列表（端口、备注、私有/公开、URL + 复制 / 打开）、添加、修改可见性（设为公开需二次确认，文案含「任何人都能访问，请确认应用自带鉴权」）、删除。URL 由后端返回（`url` 字段，§7.1）。非 running 状态仍可管理端口，但 URL 旁提示「实例运行中才可访问」。换机后提示增加「Web 访问地址已变更为新实例 ID」。
- 后台实例详情显示端口列表；ops 可**强制设为私有**（`forced_private = true`，用户不能再改回公开，填原因，审计 `instance.port_force_private`）或删除某端口（填原因，审计 `instance.port_delete`），用于处理公开端口被滥用（裁定 4）。
- 换机：`AdminMigrate` 事务内 `INSERT INTO instance_ports (instance_id, port, visibility, forced_private, note, …) SELECT <new_id>, port, visibility, forced_private, note, … FROM instance_ports WHERE instance_id = <old_id>`。

### 4.5 TLS
- `SPARK_TLS_CERT_FILE` + `SPARK_TLS_KEY_FILE` 同时设置时，公网监听器（`SPARK_HTTP_ADDR`）改为 HTTPS（证书必须覆盖 `<domain>`、`*.<ingress_domain>`；两域不同则需要同时覆盖两者的证书或 SAN 合并），并可选在 `SPARK_HTTP_REDIRECT_ADDR`（默认空 = 关）起一个纯 HTTP 监听器把所有请求 301 到 https。HSTS（`max-age=31536000`）仅在 HTTPS 下发送。
- 热加载：`tls.Config.GetCertificate` 每分钟最多检查一次文件 mtime，变化即重新加载；加载失败保留旧证书并 `RaiseAlertOnce(critical, tls_cert_reload_failed, "TLS 证书文件重新加载失败，详情见日志")`。每日检查到期：剩余 < 14 天 → `RaiseAlertOnce(warning, tls_cert_expiring, "TLS 证书将在 14 天内到期")`。
- 没有配置证书文件时由前置代理终止 TLS（`SPARK_TRUST_PROXY=true`，代理须原样转发 Host，并把 `*.<ingress_domain>` 与 `<domain>` 都指到 platform）。prod 下两者都没有（无证书且 `TrustProxy=false`）→ 启动失败。
- 证书来源（裁定 2）：acme.sh 侧车 DNS-01（示例 `dns_ali`）写共享卷，或人工放商业泛域名证书文件；platform 不区分，热加载同一逻辑。选哪条列入上线前核对。

### 4.6 URL 生成
`SPARK_PUBLIC_BASE_URL` 成为所有环境的权威对外地址（dev 默认 `http://spark.local:8080`，即 `http://<domain>:<HTTP 端口>`；e2e `http://spark.test:18080`；prod 必填且须 https）。开发机本地测 ingress 时要把 `spark.local` 与用到的 `ins-<id>-<port>.spark.local` 加进 `/etc/hosts`（hosts 不支持泛域名），或改用 sslip.io 域（§10.4 的做法）；README「本地开发」注明。启动校验 `host(SPARK_PUBLIC_BASE_URL) == SPARK_DOMAIN`，不一致则启动失败（否则拼出的 URL 与证书 / DNS 对不上）。端口 URL = `scheme://ins-<id>-<port>.<ingress_domain>[:base 的端口]/`；`authorize` 与 callback 的跳转地址同样由它推导，不信任请求里的 Host 拼 URL。

## 5. 上游连接与 Agent 改动

### 5.1 实例主机密钥改由平台生成（防止上游被冒充）
- `rental.Place` 与 `AdminMigrate`（新建实例的两条路径）生成 ed25519 主机密钥：公钥（authorized_keys 一行）存 `instances.ssh_host_pubkey`，私钥 PEM 经 `secretbox` 存 `instances.ssh_host_key_enc`。实例 released / failed 时清空 `ssh_host_key_enc`（已无用途的私钥不留库）。
- **私钥不进 `agent_commands.payload`**（审计可见的 JSON 不放私钥）。`agentgw` 新增依赖接口 `HostKeys interface { PEM(ctx, instanceID string) (string, error) }`（由 `rental` 实现，内部解密），在 `push` 时读取填入 `CreateInstance.host_key_pem`（新字段 6，proto 注释标「敏感：仅在 mTLS 流上传输，不落日志」）。Agent `writeSSHDir`：收到非空 `host_key_pem` 且本地没有主机密钥文件时写入（0600）并派生 `.pub`；已存在则保留（步骤级幂等）；为空时沿用现在的本地生成（兼容旧平台）。
- 网关与 ingress 拨上游时 `HostKeyCallback = ssh.FixedHostKey(ssh_host_pubkey)`。`ssh_host_pubkey = ''`（计划 2 创建的存量实例）→ 首次连接信任并写回（`UPDATE … WHERE id = $1 AND ssh_host_pubkey = ''`），记 warning 日志；之后固定。
- **生产发布顺序：先升级全部 Agent，再发布 platform。** 旧 Agent 忽略未知字段 6 并本地生成主机密钥，而新 platform 已把平台生成的公钥固定为 `HostKeyCallback`，这段窗口里创建的实例会永久 `upstream_unreachable`；反过来（新 Agent + 旧 platform）兼容。README 与 `followups-plan3.md`「上线前核对」都写。
- 计划 2 的「上游密钥轮换需重建容器」不变。

### 5.2 容器 sshd 配置（`initScript`）
追加：`Subsystem sftp internal-sftp`（不依赖镜像里的 sftp-server 二进制）、`AllowTcpForwarding yes`、`GatewayPorts no`、`AllowAgentForwarding yes`、`X11Forwarding no`、`PermitTunnel no`、`ClientAliveInterval 60`、`ClientAliveCountMax 3`、`MaxStartups 50:30:200`（每实例最多 16 条用户连接 + 1 条池连接同时握手时默认 10 会随机丢弃）、`MaxSessions 32`（ControlMaster 复用）。Agent 在 create、start、重启恢复时都重写 `init.sh`（主机密钥与 authorized_keys 不动），存量实例下次启动即生效。`docs/image-spec.md` 增加「sftp 由 sshd 内置实现提供，镜像不需要 openssh-sftp-server」。

### 5.3 上游连接池（`internal/access/upstream`）
- `Pool.Dial(instanceID, port)`：按 `(instance_id, lan_ip)` 取（或建立）一条共享 `ssh.Client`（`lan_ip:SPARK_UPSTREAM_SSH_PORT`，上游私钥，固定主机密钥，`ClientConfig.Timeout 10s`），返回 `client.Dial("tcp", "127.0.0.1:<port>")` 的通道。键含 `lan_ip`：登记地址变化（`lan_ip_mismatch` 场景）时旧连接即被丢弃。连接错误、保活失败（每 30 秒 `keepalive@openssh.com`，2 次失败）或实例离开 running 时关闭并丢弃，下次请求重建。每实例最多 1 条共享连接；sshgw 的用户会话不走这个池（§2.4）。
- 配置 `SPARK_UPSTREAM_SSH_PORT`（默认 2222，同时作为 Create 指令的 `ssh_port`），测试里指向进程内的假容器 sshd。

### 5.4 协议与指令
- `agent.proto`：`CreateInstance` 增加 `string host_key_pem = 6;`（敏感），其余不变；`make proto` 重新生成并提交。
- **不新增** `UpdatePorts`；计划 2 文档里的「随计划 3」一条按本设计关闭。
- `AgentConfig` 不变。

## 6. 数据模型（迁移 `00018_access.sql`，带 `+goose Down`）

```sql
CREATE TABLE ssh_keys (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users (id),
    name         TEXT NOT NULL,
    public_key   TEXT NOT NULL,            -- "<type> <base64>"，无注释
    fingerprint  TEXT NOT NULL,            -- "SHA256:…"
    last_used_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (user_id, fingerprint)
);
CREATE INDEX ssh_keys_user_idx ON ssh_keys (user_id, created_at);

CREATE TABLE instance_ports (
    id             BIGSERIAL PRIMARY KEY,
    instance_id    TEXT NOT NULL REFERENCES instances (id),
    port           INT NOT NULL CHECK (port BETWEEN 1 AND 65535 AND port <> 22),
    visibility     TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'public')),
    forced_private BOOLEAN NOT NULL DEFAULT false,   -- 后台强制私有后用户不能改回公开
    note           TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    UNIQUE (instance_id, port)
);

CREATE TABLE ssh_sessions (
    id           BIGSERIAL PRIMARY KEY,
    instance_id  TEXT NOT NULL REFERENCES instances (id),
    user_id      BIGINT NOT NULL REFERENCES users (id),
    machine_id   BIGINT NOT NULL REFERENCES machines (id),
    auth_method  TEXT NOT NULL CHECK (auth_method IN ('password', 'publickey')),
    key_id       BIGINT REFERENCES ssh_keys (id) ON DELETE SET NULL,
    client_ip    TEXT NOT NULL,
    bytes_up     BIGINT NOT NULL DEFAULT 0,
    bytes_down   BIGINT NOT NULL DEFAULT 0,
    started_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,   -- 持有副本每 60 秒刷新；清扫器收尾超过 5 分钟未刷新的行
    ended_at     TIMESTAMPTZ
);
CREATE INDEX ssh_sessions_instance_idx ON ssh_sessions (instance_id, started_at DESC);
CREATE INDEX ssh_sessions_open_idx ON ssh_sessions (last_seen_at) WHERE ended_at IS NULL;

ALTER TABLE instances
    ADD COLUMN password_auth    BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN ssh_host_pubkey  TEXT NOT NULL DEFAULT '',
    ADD COLUMN ssh_host_key_enc BYTEA;                 -- 存量实例为 NULL；释放后清空

ALTER TABLE platform_pki ADD COLUMN ssh_host_key_enc BYTEA;  -- 网关主机密钥
```

清扫器新增：收尾 `last_seen_at` 过期的会话行；删除 `ended_at < now - 90 天` 的 `ssh_sessions`（`rental.SSHSessionRetention = 90d`）。释放实例时 `instance_ports` 行保留（历史），路由表只取 running 实例；用户视图里 released 实例不再显示端口卡片。

## 7. API

错误码（`apperr`）新增：`ssh_key_invalid`（400「公钥格式不正确或不受支持（支持 ed25519、ecdsa、rsa≥2048）」）、`ssh_key_limit`（409「最多添加 10 个公钥」）、`ssh_key_exists`（409「该公钥已添加」）、`password_auth_requires_key`（409「关闭密码登录前请先添加 SSH 公钥」）、`port_invalid`（400「端口必须是 1～65535 之间且不为 22 的整数」）、`port_limit`（409「每个实例最多开放 5 个端口」）、`port_exists`（409「该端口已添加」）、`port_forced_private`（409「该端口已被平台限制为私有」）。其余复用 `invalid_state` / `not_found` / `rate_limited` / `forbidden`（`Sec-Fetch-Site` 校验失败也返回 `forbidden`）。

### 7.1 控制台（`/api/v1`）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/ssh-keys` | `{items:[{id,name,fingerprint,created_at,last_used_at}]}` |
| POST | `/ssh-keys` | `{name?, public_key}` → 新记录 |
| DELETE | `/ssh-keys/{id}` | 只删自己的 |
| POST | `/instances/{id}/password/reset` | → `{password}` |
| PUT | `/instances/{id}/access` | `{password_auth}` → 实例视图 |
| GET | `/instances/{id}/ports` | `{items:[{port,visibility,forced_private,note,url,created_at}]}` |
| POST | `/instances/{id}/ports` | `{port, visibility?, note?}` → 记录（含 `url`） |
| PUT | `/instances/{id}/ports/{port}` | `{visibility?, note?}`；`forced_private` 时改公开 → `port_forced_private` |
| DELETE | `/instances/{id}/ports/{port}` | |
| GET | `/instances/{id}/ssh-sessions` | 最近 20 条 `{auth_method, client_ip, started_at, ended_at}`（用户自查异常登录） |
| GET | `/ingress/authorize?target=` | **`requireUser` 组之外**；HTML 跳转流程（§4.2）；按用户每分钟 60 次限流 |

以上除 `authorize` 外都在 `requireUser` 组内。`instanceView` 增加 `password_auth bool`、`ssh_host string`（`ssh.<domain>`）、`ssh_port int`（网关公网端口）、`ports_count int`；`ssh_command` 规则见 §2.1。

### 7.2 后台（`/admin/api/v1`）

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| GET | `/instances/{id}` | all | 响应增加 `password_auth`、`ports[]`（含 url、forced_private）、`ssh_host_pubkey_fingerprint` |
| GET | `/instances/{id}/ssh-sessions` | all | 分页，`ssh_sessions` 全字段 |
| POST | `/instances/{id}/ports/{port}/force-private` | ops | body `{reason}`，审计 `instance.port_force_private` |
| DELETE | `/instances/{id}/ports/{port}` | ops | body `{reason}`，审计 `instance.port_delete` |
| GET | `/users/{id}` | all | 响应增加 `ssh_keys[]`（id、name、fingerprint、created_at、last_used_at） |
| GET | `/dashboard` | all | 增加 `ssh_sessions_open`（`ended_at IS NULL` 计数） |

后台页面：实例详情加「访问」卡片（密码登录开关只读、端口表 + 强制私有 / 删除、最近 SSH 会话）；用户详情加「SSH 公钥」表；仪表盘加「在线 SSH 会话」数字。

## 8. 配置项

| 变量 | 说明 | 默认 |
|---|---|---|
| `SPARK_SSH_ADDR` | SSH 网关监听地址 | `:2222` |
| `SPARK_SSH_PUBLIC_PORT` | 写进 `ssh_command` 的公网端口 | `22` |
| `SPARK_SSH_MAX_SESSIONS_PER_INSTANCE` | 每实例并发 SSH 会话上限 | `16` |
| `SPARK_SSH_DRAIN_TIMEOUT` | 停机时等待会话结束的时间 | `30s` |
| `SPARK_UPSTREAM_SSH_PORT` | 容器 sshd 在机器上的端口（Create 的 `ssh_port` 与上游拨号共用） | `2222` |
| `SPARK_INGRESS_DOMAIN` | 租户子域的父域（`ins-<id>-<port>.<此域>`）；生产推荐与 `SPARK_DOMAIN` 不同的注册域（eTLD+1） | `= SPARK_DOMAIN` |
| `SPARK_ADMIN_PUBLIC_BASE_URL` | 后台对外地址（经反向代理暴露时填写），用于后台写请求的 `Origin` 校验 | 空 |
| `SPARK_PUBLIC_BASE_URL` | 对外地址（已有）；prod 必填且 https；host 必须等于 `SPARK_DOMAIN`；dev 默认 `http://<domain>:<HTTP 端口>` | 见左 |
| `SPARK_TLS_CERT_FILE` / `SPARK_TLS_KEY_FILE` | 证书与私钥（PEM，覆盖 `<domain>` 与 `*.<ingress_domain>`），设置后公网监听器为 HTTPS 并热加载 | 空 |
| `SPARK_HTTP_REDIRECT_ADDR` | 纯 HTTP 301 到 https 的监听地址（仅 TLS 开启时有效） | 空（关） |

常量（代码内，不做配置）：每 IP（IPv6 /64）并发 32、失败连接阈值 10/10 分钟（IP）与 30/10 分钟（实例）、票据 60 秒、`sc_ingress` 12 小时、路由表与会话校验缓存 2 秒、会话 `last_seen_at` 刷新 60 秒 / 收尾 5 分钟、`MaxSSHKeys 10`、`MaxPortsPerInstance 5`。

校验：prod 下 `SPARK_PUBLIC_BASE_URL` 必须 https；`host(SPARK_PUBLIC_BASE_URL) == SPARK_DOMAIN`；prod 下未配证书且 `SPARK_TRUST_PROXY=false` → 启动失败「生产环境必须配置 TLS 证书或在反向代理后设置 SPARK_TRUST_PROXY=true」；`SPARK_SSH_ADDR` 与 `SPARK_HTTP_ADDR` 等不得相同；`SPARK_INGRESS_DOMAIN` 须是合法域名且不是 `SPARK_DOMAIN` 的子域（相等允许）——这条只防误配；要真正脱离同站，必须是与 `SPARK_DOMAIN` **不同的注册域（eTLD+1）**，例如 `spark.example` 配 `sparkapp.example`，而不是 `app.spark.example`（README 与 `.env.example` 注明）。

## 9. 失败处理、安全与可观测（决策 6）

| 场景 | SSH 网关 | Web 入口 |
|---|---|---|
| 机器失联（实例暂停） | 新连接 Banner + 拒绝（§2.3）；已有会话不主动断，上游断了自然结束 | 新请求 503 页「机器暂时失联」；已有 WebSocket 不主动断 |
| 实例到期停机 / 释放 / 换机（旧 ID） | 新连接拒绝；已有会话 10 秒内断开 | 503 / 404 |
| 续费恢复 | 下次连接放行 | 路由表 2 秒内放行 |
| 上游拨号失败（容器 sshd 未起、lan_ip 未就绪） | disconnect「无法连接到实例，请稍后重试」 | 502 页 |
| 上游中途断开 | 透传 EOF，会话结束 | 当前请求 502，连接池重建 |
| 平台副本重启 | 会话断开（drain 30s） | 请求失败后浏览器重试即可；Cookie 无状态，仍有效（控制台会话未登出） |

- 告警（`RaiseAlertOnce`，文案稳定）：`ssh_bruteforce`（§2.2）、`upstream_unreachable`（warning，machine + instance，「机器 {serial} 上实例 {id} 的 SSH 上游无法连接」，SSH 与 Web 共用；`RaiseAlertOnce` 只按未处理同文案去重，额外在进程内按实例节流 5 分钟一次，避免高频拨号失败反复查库）、`tls_cert_reload_failed`、`tls_cert_expiring`。
- 审计：`instance.password_reset`、`instance.password_auth`（开/关）、`ssh_key.add` / `ssh_key.delete`、`instance.port_add` / `port_update` / `port_delete`（用户 actor）；后台强制私有 / 删端口为 admin actor。`audit.Actor` 增加 `UserID` 字段，`Entry` 按 AdminID / UserID 决定 `actor_type`。
- 日志：SSH 会话 open/close、认证失败（含 IP、用户名、方法）、ingress 上游错误与鉴权拒绝；正常转发请求不逐条记日志。
- 安全要点：票据与 Cookie 都用 `SPARK_SECRET_KEY` 派生密钥签名并绑定控制台会话；`sc_ingress` 不转发给容器；`hostkeys-00` 丢弃；通道 / 全局请求白名单且网关不自行拨号；`authorize` 的 `target` 与 `next` 严格校验；`__Host-` 前缀与 `Sec-Fetch-Site` / `Origin` 校验（§4.2.1）；**租户 Web 应用与控制台在默认配置下同站，租户应用自身的 CSRF 防护由应用负责**；公开端口是全网可达的明文 HTTP 应用（帮助页警告，建议应用自带密码；后台可强制私有）；`/.spark/*` 保留路径；实例存在性经 Banner 可见（接受），密码错与公钥不匹配不可区分。

## 10. 测试与验收（决策 7）

### 10.1 单元（Go，无 Docker）
- `sshgw`：用户名解析；密码 / 公钥 / 密码关闭 / 各状态 Banner（假时钟 + 嵌入式 PG）；中继——进程内假上游（`internal/access/sshtest`：x/crypto/ssh 服务端，支持 session exec / shell / pty、`subsystem sftp`（回固定字节）、`direct-tcpip`（拨本地地址）、`tcpip-forward` + `forwarded-tcpip` 回连、`auth-agent` 通道、发 `hostkeys-00`、按要求发 `x11` / `forwarded-streamlocal` 通道）：exec 退出码透传、`-L` 可用、`-R` 回连可用、agent 通道透传、x11 / streamlocal / tun 被拒、未知全局请求回 false、`hostkeys-00` 被丢弃、**网关从不自行拨号**（假上游断言收到的 `direct-tcpip` extra data 与客户端发送的字节一致，且测试里不存在任何网关侧出站拨号）、字节计数、实例停机后 10 秒内会话被断、暂停时不断、上游拒绝原因回传；失败连接计数（agent 多把公钥不计、密码错一次连接只计 1）、IPv6 /64 键、并发上限；主机密钥持久化（两次 `Ensure` 同一把、并发只留一把）；`last_seen_at` 刷新与清扫收尾；drain。
- `identity`：公钥解析矩阵（ed25519 / ecdsa / rsa-2048 / rsa-1024 拒 / dss 拒 / sk 拒 / 垃圾 / 带注释 / 重复 / 上限 10）。
- `rental`：密码重置、`password_auth` 规则、端口 CRUD（范围、22、上限 5、重复、非主人、released、forced_private）、主机密钥生成与加密、释放清空私钥、换机复制端口与 `password_auth`、`HostKeys.PEM`、会话清理。
- `api`：`__Host-` Cookie 名按环境切换；`Sec-Fetch-Site` / `Origin` 中间件矩阵（same-origin 放行、same-site 拒绝、cross-site 拒绝、无 `Sec-Fetch-Site` 且 `Origin` 匹配放行、`Origin` 不匹配拒绝、**两个头都没有放行**、支付回调豁免、GET 不检查；后台路由同一矩阵）；`authorize` 未登录 302、非主人 403、`target` 非法 400。
- `ingress`：Host 解析（大小写、带端口、非法、`SPARK_INGRESS_DOMAIN` 独立域）；票据签发 / 验签 / 过期 / 重放；Cookie 逐枚验证、会话失效即拒绝、剥离；`next` 开放重定向用例（`//evil`、`\`、带 host）；私有 / 公开 / forced_private；状态页；ReverseProxy 经 `sshtest` + httptest 上游：普通请求、流式响应、90 秒 SSE 不被 WriteTimeout 掐断、WebSocket 回显、502、**两个实例同端口并发请求不串**；`POST /.spark/logout`（GET 405）；TLS 热加载（写新证书文件 → `GetCertificate` 返回新证书；坏文件 → 保留旧的并告警）；`hostmux` 不影响控制台路径。
- `agent`：`init.sh` 快照含 sftp 等新行；`host_key_pem` 写入 / 已存在保留 / 为空时本地生成；start 时重写 `init.sh`。
- `config`：新变量与 prod 校验（含 base URL host 与 `SPARK_DOMAIN` 一致、`SPARK_INGRESS_DOMAIN` 规则）。

### 10.2 集成（Go，嵌入式 PG + 进程内 fake Agent + `sshtest` 上游）
机器 `lan_ip = 127.0.0.1`，`SPARK_UPSTREAM_SSH_PORT` 指向 `sshtest`。流程：下单 → running → 密码登录执行 `echo` → 添加公钥、关闭密码 → 密码拒绝、公钥通过 → 添加端口 8888（`sshtest` 把 8888 拨到 httptest）→ 私有：无 Cookie 302 → authorize → callback → 200；控制台登出 → 2 秒后 Cookie 失效；公开：直接 200 → 到期停机 → SSH 拒绝、Web 503 → 续费 → 恢复 → 换机 → 新实例保留端口与 `password_auth`、旧 ID 404 → 释放 → 拒绝、`ssh_host_key_enc` 为 NULL；主机密钥：`sshtest` 换一把密钥后拨号失败并告警 `upstream_unreachable`；存量实例（`ssh_host_pubkey=''`）首次连接写回；Create 指令 payload 的 JSON 不含 `host_key_pem`。

### 10.3 端到端（Playwright）
- e2e-server 内嵌 sshgw（`127.0.0.1:18022`）与一个 `sshtest` 上游；`/hooks/machines` 造的机器 `lan_ip=127.0.0.1`。新钩子 `POST /hooks/ssh {instance_id, password?|key_pem?, command}` 用进程内 ssh 客户端经网关执行并返回输出（验证页面上显示的密码 / 公钥真的能登录）；`POST /hooks/ingress-upstream {port, body}` 让 `sshtest` 在指定端口回固定内容。
- Chrome 加 `--host-resolver-rules=MAP *.spark.test 127.0.0.1`（`launchOptions.args`；`chromium` 通道同样支持）；ingress 用例在 `http://spark.test:18080` 登录。
- 覆盖表新增并登记到 `coverage.json`：
  - `C-KEY-01`～`05`：添加公钥（名称缺省取注释）、无效公钥提示、重复提示、删除、上限 10 提示。
  - `C-DET-11`～`16`：密码重置后新密码显示且 `/hooks/ssh` 能登录、关闭密码登录（无公钥时提示）、添加端口显示 URL、设为公开二次确认（含警告文案）、上限 5、删除端口。
  - `C-ING-01`～`06`：私有端口未登录 → 登录（整页跳转回 authorize）→ 回到目标页内容正确；非主人 403；公开端口免登录；实例停机 503；未开放端口 404；被后台强制私有后用户改公开被拒。
  - `A-INS-10`～`12`：后台实例详情显示端口与会话、强制私有需原因并写审计、删除端口需原因并写审计；`A-USR-07`：用户详情显示公钥。
- 注意事项：ingress 用例改变全局机器 / 实例状态的，按 `playwright.config.ts` 的顺序说明处理。

### 10.4 真机验收清单（192.168.1.221，全部通过才算完成）
平台跑在 Mac（`SPARK_DOMAIN=192-168-1-218.sslip.io`，`SPARK_PUBLIC_BASE_URL=http://192-168-1-218.sslip.io:8080`，`SPARK_SSH_ADDR=:2222`，`SPARK_SSH_PUBLIC_PORT=2222`；sslip.io 把 `*.192-168-1-218.sslip.io` 解析到 Mac，不需要改 hosts）。Agent 用本计划的新版本重新安装（先 Agent 后平台，§5.1）。需要 root 的步骤由负责人执行。**检查点 A**（SSH 网关 + Agent，任务 1～12 完成后先做）：第 1～6、9～13 项；**检查点 B**（ingress + 后台，全部任务完成后）：第 7、8、14～18 项。

1. **开通与主机密钥**：下单 → running；`docker exec <id> cat /.spark/hostkeys/ssh_host_ed25519_key.pub` 与后台实例详情的指纹一致；`/.spark/init.sh` 含 `Subsystem sftp internal-sftp`、`MaxStartups`。
2. **密码登录**：控制台复制命令 → `ssh -p 2222 ins-xxxx@ssh.192-168-1-218.sslip.io`，首次提示的主机密钥指纹与 `platform admin ssh-host-key` 一致；输入密码进入容器 root，`nvidia-smi` 正常；`ssh_sessions` 出现一行，退出后 `ended_at` 与字节数填上。
3. **公钥登录与关闭密码**：控制台添加 Mac 的 ed25519 公钥 → 免密登录；Mac agent 里装 4 把密钥连续连 3 次都成功（不触发封禁）；关闭密码登录 → 密码被拒、公钥仍通；删除公钥 → 公钥被拒，重新打开密码登录 → 密码通。错误用户名 `ssh foo@…` 显示 Banner「用户名应为实例 ID」。
4. **密码重置**：重置 → 旧密码被拒、新密码通；正在进行的会话不断。
5. **scp / sftp / rsync / VS Code**：`scp` 1GB 文件上传到 `/root/data` 与下载各一次（记录速率）；`sftp` 交互 `ls`/`put`/`get`；`rsync -e 'ssh -p 2222'`；Mac 上 VS Code Remote-SSH 连接 `ins-xxxx@ssh.…` 成功打开 `/root/data` 并运行终端。
6. **转发策略**：容器内 `python3 -m http.server 8000`（镜像无 python 时用 `nc -l`）→ `ssh -L 8000:127.0.0.1:8000` 本地可访问；`ssh -R 9000:127.0.0.1:9000` 容器内 `curl 127.0.0.1:9000` 打到 Mac；`ssh -A` 后容器内 `ssh-add -l` 列出 Mac 的密钥；`ssh -X` 不可用（`DISPLAY` 为空）；`ssh -L 8001:192.168.1.1:80` 后本地 `curl 127.0.0.1:8001` 失败（容器 sshd 拨出受 `SPARK-FWD` 阻断，证明网关没有替用户拨号）。
7. **Web 私有端口**：容器内 `jupyter lab --ip=127.0.0.1 --port=8888 --ServerApp.allow_remote_access=True`（镜像无 jupyter 时先 `pip install`；只监听 127.0.0.1，验证 §1.3 的优势）→ 控制台添加端口 8888 → 浏览器打开 URL → 跳转登录 → 回到 Jupyter，新建终端能用（WebSocket 透传）；不加 `allow_remote_access` 时预期 403「Host not allowed」（帮助页写明）；换另一个账号登录访问 → 403；隐身窗口未登录 → 登录页；控制台登出后租户页面 2 秒内变为跳转登录。
8. **Web 公开端口**：设为公开（看到警告）→ 隐身窗口直接打开；手机热点（非局域网）打开同一 URL 成功（需要 Mac 可被公网访问时做；否则用另一台局域网设备）；后台「强制私有」→ 隐身窗口变跳转登录，用户端改公开被拒并显示提示。
9. **状态门控**：把 `expires_at` 改到 1 分钟后 → 停机 → 已连接的 SSH 会话 10 秒内被断并显示「实例已停机」，新连接 Banner 显示保留截止时间，Web 显示 503 页；续费 → SSH 与 Web 恢复；释放 → 「实例已释放」/ 404。
10. **机器失联**：先开一个 SSH 会话跑 `top`，负责人 `sudo systemctl stop spark-agent` 超过 5 分钟 → 实例暂停 → 已有会话**仍在**（裁定 5），新连接 Banner「机器暂时失联」、Web 新请求 503；`start` 后恢复。
11. **隔离未削弱**：`iptables -S SPARK-FWD`、`iptables -S SPARK-IN` 与 `ExpectedFwdRules` / `ExpectedInRules` 逐行相等；`ss -lntp` 显示机器上除 2222 外没有新增任何由 Agent / Docker 发布的端口；从 Mac 直接 `curl 192.168.1.221:8888` 不通；容器内 `curl 192.168.1.218:8080`（控制面）仍被 `SPARK-FWD` 阻断（Mac 不在白名单时）。
12. **暴力破解**：脚本用错误密码连 10 次（每次连接试满 6 次）→ 第 11 次连接被立即断开；10 分钟后恢复；后台无告警（实例阈值 30 未到）；再错 30 次 → 告警 `ssh_bruteforce` 一条。
13. **重启**：负责人 `sudo reboot` → Agent 恢复容器后（计划 2 验收 §9 行为）SSH 与 Web 自动恢复，`/root/data` 文件仍在，主机密钥未变（`known_hosts` 不报警，ingress 不报 `upstream_unreachable`）。
14. **平台重启**：重启 platform 进程 → 已有 SSH 会话断开（drain）、残留的 `ssh_sessions` 行在 5 分钟内被清扫收尾；Web 的 `sc_ingress` Cookie 仍有效（无需重新登录）。
15. **TLS 热加载**：用 `openssl` 自签一张覆盖 `192-168-1-218.sslip.io` 与 `*.192-168-1-218.sslip.io` 的证书，配置 `SPARK_TLS_CERT_FILE/KEY_FILE` 与 `SPARK_PUBLIC_BASE_URL=https://…:8443`（`SPARK_HTTP_ADDR=:8443`），浏览器信任该 CA 后走 https 完成第 7 项，并确认控制台 Cookie 名为 `__Host-sc_session`（`SPARK_ENV=staging`）；替换为第二张证书文件 → 1 分钟内 `openssl s_client` 看到新证书，不重启。
16. **同站防护**：在租户页面（公开端口上放一个带表单的 HTML）向 `http://192-168-1-218.sslip.io:8080/api/v1/uploads` 提交 → 403 `forbidden`；控制台自身上传正常。
17. **独立域名**：把 `SPARK_INGRESS_DOMAIN` 设为 `192-168-1-218.sslip.io` 之外的另一个 sslip 域（如 `app-192-168-1-218.sslip.io`，仍解析到 Mac）→ 端口 URL 落在新域、私有端口鉴权流程照常。
18. **compose 冒烟**：在测试机上用 `deploy/compose` 起 `platform + postgres`（不接 Agent，`SPARK_ENV=staging`，自签证书卷）：从 Mac `ssh -p 22 ins-xxxx@192.168.1.221` 看到网关 Banner，platform 日志里的来源 IP 是 Mac 地址而不是 172.x 网桥地址（Docker DNAT 保留源地址；若看到网桥地址说明走了 userland-proxy，需改 `"userland-proxy": false` 或 `--net host`，记录结论）；替换证书卷里的文件后 `openssl s_client -connect 192.168.1.221:443` 1 分钟内看到新证书；`docker compose down` 清理。测试机宿主机 sshd 需临时挪到其它端口（负责人执行，验收后恢复）。

验收记录写到 `docs/superpowers/acceptance-plan3.md`，格式同计划 2。

## 11. 部署
- `deploy/compose/`：`docker-compose.yml`（`platform`：`SPARK_HTTP_ADDR=:8443`、`SPARK_HTTP_REDIRECT_ADDR=:8080`、`SPARK_SSH_ADDR=:2222`，端口映射 `22:2222`、`443:8443`、`80:8080`，`9443:9443` 仅内网；`postgres`；可选 `acme`（`neilpang/acme.sh`，`dns_ali`，证书卷共享给 platform）；`.env.example` 列出所有 `SPARK_*` 并与上述映射一一对应；`README`。platform 镜像 Dockerfile（多阶段，`make build` 产物）。
- 控制面宿主机步骤（README）：**先把宿主机自己的 OpenSSH 挪到其它端口**（例如 `Port 2200`，并先在新端口验证登录再重启 sshd），再 `docker compose up`（裁定 3）。
- README：新增配置表行；SSH 网关说明（端口映射、主机密钥指纹发布、部署会断 SSH、`SPARK_SECRET_KEY` 轮换连带丢失网关主机密钥）；Web 入口说明（DNS：`<domain>`、`ssh.<domain>`、`*.<ingress_domain>` 都指向控制面；**生产推荐 `SPARK_INGRESS_DOMAIN` 用与 `SPARK_DOMAIN` 不同的注册域（eTLD+1），子域不算**；本地开发测 ingress 需在 hosts 加 `spark.local` 与 `ins-*.spark.local` 条目或用 sslip.io；证书两种来源）；**发布顺序：先升级全部 Agent，再发布 platform**。
- 帮助页（`HelpView`）：SSH 登录（密码 / 公钥 / 指纹核对 / 端口转发 / scp、sftp / `ssh-copy-id` 不生效）、Web 端口（私有 / 公开、`/.spark/` 保留、公开端口风险、应用需监听 `127.0.0.1` 或 `0.0.0.0`——只监听容器 eth0 地址不通、Jupyter 需 `--ServerApp.allow_remote_access=True`、租户应用自己做 CSRF 防护）。

## 12. 范围与遗留（决策 8）

**本计划做**：§2～§11 全部；`UpdatePorts` 取消；`sftp` 修正；主机密钥平台化；compose 示例与冒烟。

**上线前核对（写入 `followups-plan3.md`）**：证书来源二选一（acme.sh DNS-01 需 DNS 服务商 API 密钥 / 商业证书）；`SPARK_INGRESS_DOMAIN` 是否用独立域；宿主机 sshd 改端口；发布顺序先 Agent 后 platform；公开端口使用条款（负责人事项）。

**明确不做（遗留清单）**
- X11 转发；FIDO/`sk-*` 公钥；SSH 证书认证；SSH 二次验证。
- platform 内置 ACME（用 acme.sh 侧车）；HTTP/3；自定义域名 CNAME 到实例端口。
- 公开端口的限流 / WAF / 基础认证分享链接；按实例的来源 IP 白名单；默认禁用公开端口按用户开通。
- 后台「断开某实例全部 SSH 会话」操作（处置滥用时可先强制私有 / 删端口 / 封禁用户）。
- 会话录像；按会话的流量计费；SSH 网关的 Prometheus 指标。
- 多副本共享的限流计数与票据一次性集合（目前各副本独立）。
- `direct-tcpip` originator 改写为真实客户端 IP（与「网关不解析通道载荷」冲突，见附录）。
- Web 终端 / JupyterLab 一键（子项目 2 应用层）。
- 原始 TCP/UDP 端口对外暴露（只提供 HTTP/WebSocket 入口，其它协议用 `ssh -L`）。
- 平台重启不断 SSH 会话（需独立网关进程 + 连接迁移）。

## 13. 任务拆分（供实施计划引用，24 项）

1. 迁移 00018（含 Down）+ `rental` 列与结构体、`audit.Actor.UserID`。
2. `identity` SSH 公钥解析、CRUD、上限；单测。
3. `rental` 密码重置、`password_auth`、主机密钥生成（Place / AdminMigrate）与 `HostKeys.PEM`、释放清空私钥、换机复制端口与开关、`ssh_sessions` 收尾与清理。
4. `rental` 端口 CRUD、`MaxPortsPerInstance`、`forced_private`。
5. `config` 新变量与校验（`SPARK_INGRESS_DOMAIN`、base URL host 一致性等）；README 配置表。
6. `pki`：网关主机密钥 `EnsureSSHHostKey`；`platform admin ssh-host-key`。
7. `internal/access/sshtest` 假上游（服务端）。
8. `internal/access/upstream` 连接池（键含 lan_ip、固定主机密钥、TOFU 写回、保活）。
9. `sshgw` 服务端配置、Banner、认证、按连接计数的限流、会话记录与 `last_seen_at`。
10. `sshgw` 中继（白名单 + 默认拒绝、不自行拨号、keepalive、字节计数）。
11. `sshgw` 状态扫描断开（不含暂停）、drain；集成进 `app` 与 `cmd/platform`。
12. `agent.proto` `host_key_pem`；Agent `writeSSHDir` 与 `init.sh` 修订；`agentgw` 经 `HostKeys` 投递填充。
    — **真机检查点 A**（§10.4 第 1～6、9～13 项）——
13. `api` / `adminapi`：`__Host-` Cookie 名、`Sec-Fetch-Site` / `Origin` 三段规则中间件（控制台与后台共用一份实现）、`authorize` 端点（组外）。
14. `ingress` Host 解析（含独立域）、路由表、状态页、404/502/503 页面。
15. `ingress` 票据、Cookie（逐枚验证、绑定会话）、callback / `POST logout`、`next` / `target` 校验。
16. `ingress` ReverseProxy（按实例隔离的连接池、SSH 通道拨号、WebSocket、Cookie 剥离、清除 Read/Write 期限）。
17. `hostmux` 与公网监听器整合；TLS 文件加载 / 热加载 / 重定向监听器 / HSTS；告警。
18. 控制台 API：`/ssh-keys`、`/password/reset`、`/access`、`/ports`、`/ssh-sessions`、`instanceView` 字段。
19. 后台 API：实例详情扩展、`ssh-sessions`、强制私有、删端口、用户详情公钥、仪表盘计数。
20. 控制台页面：SSH 公钥页、实例详情「SSH 连接」扩展与「Web 访问」卡片（公开警告）、登录页与路由守卫整页跳转、帮助页。
21. 后台页面：实例详情「访问」卡片、用户详情公钥、仪表盘。
22. Go 集成测试（§10.2）+ e2e-server 与钩子（sshgw、sshtest、`/hooks/ssh`、`/hooks/ingress-upstream`、host-resolver-rules）+ Playwright 用例 + `coverage.json`。
23. `deploy/compose` 与 platform Dockerfile、README 部署章节（宿主机 sshd 改端口、发布顺序、独立域名推荐）。
24. 真机验收检查点 B（§10.4 第 7、8、14～18 项）与 `acceptance-plan3.md`、`followups-plan3.md`。

## 附录：评审意见处理

必改 1～12 全部采纳（§4.3 连接池隔离；§4.2.1 `__Host-` + `Sec-Fetch-Site`；§4.2 `next`/`target` 校验与 `router.ts` 守卫；§2.2 按连接计数；§2.1 枚举措辞；§2.4 白名单 + 不自行拨号；§5.1 `HostKeys` 接口与发布顺序；§4.3 `ResponseController` 清期限；§4.4/§5.1 换机复制；§2.4/§6 `last_seen_at`；§10.4 第 5、7 项与帮助页；§10.4 第 18 项 compose 实跑与宿主机 sshd）。

建议项：
- 上游连接池键含 `lan_ip`：采纳（§5.3）。
- 暂停时不断已有会话：采纳（裁定 5，§2.3、§9）。
- `/.spark/logout` 用 POST：采纳（§4.2）。
- `sc_ingress` 绑定控制台会话：采纳（令牌带 `session_hash`，2 秒缓存校验，§4.2）。
- 同名 Cookie 逐枚验证：采纳（§4.2 第 4 步）。
- IPv6 限流键 /64：采纳（§2.2）。
- sshd `MaxStartups` / `MaxSessions`：采纳（§5.2）。
- 帮助页补 `ssh-copy-id` 与监听地址说明：采纳（§3.2、§11）。
- 释放时清空 `ssh_host_key_enc`、README 密钥轮换提示：采纳（§5.1、§2.1）。
- 启动校验 base URL host == `SPARK_DOMAIN`：采纳（§4.6、§8）。
- 迁移写 `+goose Down`：采纳（§6）。
- `authorize` 注册在 `requireUser` 组外：采纳（§4.2、§7.1、任务 13）。
- `direct-tcpip` originator 改写为真实客户端 IP：**不采纳**——需要解析并改写通道 extra data，与必改 6「网关不解析通道载荷、只原样转发」直接冲突，且来源 IP 已记录在 `ssh_sessions`；列入遗留。
- 真机检查点 + compose 拆分：检查点采纳（§10.4、§13）；compose **不拆**成独立计划——协调人要求保持单一计划（约 24 项），改为放在最后两项并有独立冒烟验收。
- 后台「断开某实例全部 SSH 会话」：**不采纳（延后）**——评审标注可延后，现有强制私有 / 删端口 / 封禁已覆盖处置需要；列入遗留。
- 文案 / 编号：采纳（§1 决策编号对照、§5.1「Place 与 AdminMigrate」、§11 `.env.example` 端口对应、§9 `upstream_unreachable` 节流说明）。
