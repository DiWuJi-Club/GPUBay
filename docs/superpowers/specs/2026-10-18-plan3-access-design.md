# Spark Cloud — 计划 3：接入层设计文档（SSH 网关 + Web 入口）

- 日期：2026-10-18
- 状态：已审定（Fable，2026-10-18；负责人已授权由 Fable 设计并确认）
- 上位文档：`2026-10-14-spark-cloud-mvp-design.md`（下称「总设计」）§5、§7.1；`2026-10-18-plan2-agent-design.md`（下称「计划 2」）§4.2
- 范围：用户经公网 SSH 登录实例（`ssh ins-xxxxxxxx@ssh.<domain>`，密码或公钥）、经公网 HTTPS 访问实例内的 Web 端口（`https://ins-xxxxxxxx-<port>.<domain>`，默认私有、可设公开）、控制台的 SSH 公钥 / 密码 / 端口管理、后台对应视图、TLS 终止、部署示例与真机验收。
- 不在范围：见 §12。

## 0. 现状与约束（实现时不必再查）

**代码事实**
- `GET /api/v1/instances/{id}` 的 `ssh_command` 为 `ssh <id>@ssh.<SPARK_DOMAIN>`（`internal/api/instances.go`），仅 running 时输出；`GET /instances/{id}/password` 解密 `instances.ssh_password_enc`（`rental.Password`，16 位随机密码，`secretbox` 加密）。
- 控制台会话 Cookie `sc_session`（host-only，`Path=/`，`SameSite=Lax`，`HttpOnly`，staging/prod 带 `Secure`）；后台 `sc_admin`（`Path=/admin/`，`Strict`）。`identity.Authenticate(token)` 查 `sessions` 表。两者都**没有 `Domain` 属性**，不会发到任何子域名。
- 容器 sshd 由 Agent 生成的 `/.spark/init.sh` 启动（`internal/agent/executor/sshdir.go`）：`PasswordAuthentication no`、`AuthorizedKeysFile /.spark/authorized_keys`（平台上游公钥）、主机密钥 `/.spark/hostkeys/ssh_host_ed25519_key`（Agent 在创建时生成，已存在则保留）。**配置里没有 `Subsystem sftp`**，所以现在 `sftp` 与 OpenSSH ≥ 9.0 的 `scp`（默认走 SFTP）都会失败，本计划修正（§5.2）。
- 容器 22 端口发布到 `<lan_ip>:2222`（`docker create -p`），端口发布在创建时固定（验收缺陷 B1/B2：绑定地址不存在时 Docker 29 会丢掉网络挂接）。上游私钥 `SPARK_UPSTREAM_SSH_KEY_FILE`（ed25519），公钥由网关投递 Create 指令时填入。
- 容器网络 `spark0`（10.200.0.0/24），`SPARK-FWD` / `SPARK-IN` 规则在真机上与 `ExpectedFwdRules` / `ExpectedInRules` 逐行相等；`SPARK-IN` 丢弃容器到宿主机的一切新连接。
- `agent_commands.type` 没有 CHECK；`fleet.CommandType` 只有 create / start / stop / wipe / pull_image；计划 2 把 `UpdatePorts` 留给本计划。
- 实例 ID 形如 `ins-` + 8 个 `[a-z0-9]`；同一机器同时只有一个实例；`instances_machine_active_uniq` 保证。
- `AdminMigrate`（换机）新建一个实例 ID；`instance_transfers` 记录迁移。
- `golang.org/x/crypto v0.58.0` 已在 `go.mod`（Agent 用它生成主机密钥），`x/crypto/ssh` 可直接用。
- 迁移编号到 `00017_agent.sql`；本计划是 `00018_access.sql`。
- e2e：Playwright 用 Chrome（`E2E_BROWSER_CHANNEL`），控制台 `http://127.0.0.1:18080`，钩子 `:18090`，fake Agent 步进模式；覆盖表 ID 维护在 `web/e2e/coverage.json`。已有 ID 到 `C-DET-10`、`C-INS-04`、`C-ACC-02`、`A-INS-09`、`A-USR-06`、`A-MCH-09`。
- `audit_logs.actor_type` 允许 `user`；`audit.Actor` 目前只有 `AdminID`，`AdminID=0` 记为 system。

**环境事实**
- 开发 Mac 无 Docker、无 brew；Go 1.27、Node 22。
- 测试机 192.168.1.221（Ubuntu 24.04、Docker 29.6.2、nf_tables），验收时控制面跑在 Mac（192.168.1.218）；局域网 WiFi DHCP，地址可能变。
- 生产：控制面 docker compose；公网只开 22 与 443/80；机器在内网。

## 1. 架构与核心决策

```
                  公网
     ┌─────────────┼──────────────────────────────┐
  SSH :22                                  HTTPS :443 / HTTP :80
     │                                             │
     ▼                                             ▼
┌───────────────────────── platform（Go 单体，1..N 副本）──────────────────────┐
│ sshgw（内置 SSH 网关）                hostmux：按 Host 分流                     │
│  用户名 = 实例 ID                      ├ <domain>            → 控制台 + /api  │
│  密码 / 公钥在此校验                    └ ins-<id>-<port>.<domain> → ingress   │
│  上游：平台私钥登录容器 sshd              （httputil.ReverseProxy，票据 Cookie）  │
│        │                                             │                       │
│        └──────── 一条 SSH 连接（lan_ip:2222）─────────┘  direct-tcpip 通道       │
└──────────────────────────────────────┬───────────────────────────────────────┘
                                       │ 机房内网
                              容器 sshd（10.200.0.x:22 → lan_ip:2222）
                              Web 端口只在容器内监听（127.0.0.1 或 0.0.0.0 均可）
```

三条总决策，理由与取舍见 §1.1～§1.3；其余决策在各节里。

### 1.1 决策 1：SSH 网关自研（`internal/sshgw`，内嵌 platform），不用 sshpiper

| | sshpiper + gRPC 插件 | 自研（x/crypto/ssh） |
|---|---|---|
| 开发机可测性 | 需要 sshpiper 二进制（无 brew；`go install` 可以），`go test` 得拉起外部进程并对齐其插件协议版本 | 纯进程内：测试里起网关 + 假「容器 sshd」+ ssh 客户端，三者都是 x/crypto/ssh |
| 转发特性 | 由 sshpiper 版本决定；哪些通道透传不受我们控制 | 通道与请求逐类透传，策略写在代码里（§2.4） |
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

## 2. SSH 网关（`internal/sshgw`）

### 2.1 监听与身份
- 监听 `SPARK_SSH_ADDR`（默认 `:2222`）；生产由 compose 把 22 映射到 2222。`ssh_command` 用 `SPARK_SSH_PUBLIC_PORT`（默认 22）：非 22 时输出 `ssh -p <port> <id>@ssh.<domain>`。主机名固定 `ssh.<SPARK_DOMAIN>`；用户名就是实例 ID，所以验收时直接 `ssh -p 2222 ins-xxxx@192.168.1.218` 也行，不需要 DNS。
- `ServerVersion = "SSH-2.0-SparkCloud"`；主机密钥 ed25519，首次启动生成，`secretbox` 加密存 `platform_pki.ssh_host_key_enc`（`UPDATE ... WHERE id = 1 AND ssh_host_key_enc IS NULL` 后重读，多副本只会留下一把）。`platform admin ssh-host-key` 子命令打印公钥与 SHA256 指纹，写进帮助页「首次连接请核对指纹」。
- 认证回调前的 `BannerCallback` 按用户名查实例并给出中文说明（见 §2.3 表），然后再决定是否允许认证。用户名不是 `^ins-[a-z0-9]{8}$`、实例不存在或不属于任何用户：Banner「用户名应为实例 ID，例如 ins-a1b2c3d4」，认证一律失败（不区分「不存在」与「密码错」，防枚举）。

### 2.2 认证
- `PasswordCallback`：实例 `password_auth = true` 且 `subtle.ConstantTimeCompare(解密密码, 输入)`；否则失败。关闭密码登录时失败原因只记日志。
- `PublicKeyCallback`：按 `ssh_keys WHERE user_id = <实例主人> AND fingerprint = SHA256(<key>)` 查；命中则在 `Permissions.Extensions` 记 `key_id`。会话建立后 `UPDATE ssh_keys SET last_used_at`。
- `MaxAuthTries = 6`；握手（含认证）总超时 30 秒。
- 暴力破解：进程内计数（与登录限流器同一实现，多副本各自计数，已接受）。来源 IP 10 分钟内认证失败 ≥ 10 次 → 该 IP 10 分钟内新连接直接断开；同一实例 10 分钟内失败 ≥ 30 次 → `RaiseAlertOnce(warning, ssh_bruteforce, machine, instance, "实例 <id> 的 SSH 登录失败次数超过阈值，详情见日志")`（文案稳定，不含次数与 IP）。e2e-server 的 `DisableRateLimits` 同样关闭这两项。
- 连接上限：每来源 IP 并发连接 ≤ 32，每实例并发会话 ≤ 16（`SPARK_SSH_MAX_SESSIONS_PER_INSTANCE`，默认 16），超限断开并提示「连接数过多」。

### 2.3 状态门控

| 实例状态 | Banner | 认证 |
|---|---|---|
| running 且未暂停 | 无 | 允许 |
| running 且暂停（机器失联） | 「实例所在机器暂时失联，计时已暂停，请稍后重试」 | 拒绝 |
| creating | 「实例正在开通，请稍后再试」 | 拒绝 |
| stopped | 「实例已到期停机，数据保留至 <retain_until>，续费后可继续使用」 | 拒绝 |
| wiping / released / failed | 「实例已释放」 | 拒绝 |
| 机器 `agent_online = false` 而实例仍 running（5 分钟判定窗口内） | 无 | 允许，上游连不上时按 §9 处理 |

网关每 10 秒扫描自己持有的会话：实例不再是「running 且未暂停」（到期停机、释放、换机后旧实例进 wiping）→ 向客户端发 disconnect「实例已停机 / 已释放，连接已断开」并关闭上游。续费恢复 running 后下一次连接自然放行，不需要额外动作。

### 2.4 中继
认证通过后：`net.DialTimeout(lan_ip:SPARK_UPSTREAM_SSH_PORT, 10s)` → `ssh.NewClientConn`（用户 `root`，上游私钥，`HostKeyCallback` 固定为 `instances.ssh_host_pubkey`，见 §5.1）。失败则 disconnect「无法连接到实例，请稍后重试」并按 §9 告警。成功后开始中继，每个用户会话一条独立上游连接（上游 sshd 的 `-R` 监听、agent 转发都按会话隔离，与 sshpiper 一致）：

| 方向 | 内容 | 策略 |
|---|---|---|
| 客户端 → 上游 `NewChannel` | `session`（shell / exec / subsystem sftp / pty / env / window-change / signal）、`direct-tcpip`（`-L`、`-D`） | 透传；上游拒绝原因原样回传 |
| 上游 → 客户端 `NewChannel` | `forwarded-tcpip`（`-R` 回连）、`auth-agent@openssh.com`（`-A`） | 透传 |
| 上游 → 客户端 `NewChannel` | `x11` | 拒绝（`Prohibited`）；sshd 配置也关闭 X11（§5.2） |
| 通道内请求 | 全部（含 `exit-status`、`exit-signal`、`eow@openssh.com`） | 透传，`WantReply` 结果回传 |
| 全局请求 | `tcpip-forward`、`cancel-tcpip-forward`、`keepalive@openssh.com`、`no-more-sessions@openssh.com` | 透传 |
| 全局请求 | `hostkeys-00@openssh.com`（上游公布容器主机密钥） | **丢弃**，否则客户端 known_hosts 会把容器密钥记到 `ssh.<domain>` 名下 |

- 保活：网关每 60 秒向客户端与上游各发一次 `keepalive@openssh.com`，连续 3 次无回应即关闭。没有空闲超时（训练任务常挂几天）。
- 优雅关闭：收到 SIGTERM 后停止 accept，最多等 `SPARK_SSH_DRAIN_TIMEOUT`（默认 30s）再断开剩余会话（部署会断 SSH，README 注明）。
- 会话记录：建立时插入 `ssh_sessions`，结束时写 `ended_at`、`bytes_up`、`bytes_down`（中继时计数）。进程启动时 `UPDATE ssh_sessions SET ended_at = now() WHERE replica_id = $1 AND ended_at IS NULL`（`SPARK_REPLICA_ID`，默认主机名），崩溃残留的行由同名副本重启时收尾。日志（slog）：每个会话 open / close 各一条，字段 instance、user、method、ip、duration、bytes。

## 3. 凭据

### 3.1 实例密码（保留）
- 保留现有 16 位随机密码与查看接口。新增重置：`POST /instances/{id}/password/reset` → 新密码（同一长度），`UPDATE instances SET ssh_password_enc`，写 `audit_logs(actor_type='user', actor_id=user_id, action='instance.password_reset', target=instance)`。实例 released / failed 时 `invalid_state`。正在进行的会话不受影响（密码只在认证时用）。
- 每实例可关闭密码登录：`instances.password_auth`（默认 true）。`PUT /instances/{id}/access {password_auth}`；关闭时要求用户至少有一个公钥，否则 `password_auth_requires_key`。后台实例详情显示该开关，不能改（避免运维替用户降安全）。

### 3.2 用户级 SSH 公钥（新）
- 表 `ssh_keys`（§6），每用户最多 10 个（`identity.MaxSSHKeys = 10`）。
- 接受 `ssh-ed25519`、`ecdsa-sha2-nistp{256,384,521}`、`ssh-rsa`（≥ 2048 位）、`sk-*` 不接受（网关不做 FIDO）；`ssh-dss` 拒绝。解析用 `ssh.ParseAuthorizedKey`，只保留 `type + base64`，注释丢弃；指纹 `ssh.FingerprintSHA256`。同一用户同一指纹 → `ssh_key_exists`。名称 1～64 字符，缺省取公钥注释（没有则「密钥 N」）。
- 删除公钥不影响已建立的会话（与 OpenSSH 一致）。
- 控制台新页 `/account/ssh-keys`（「我的」页增加入口）：列表（名称、指纹、添加时间、最近使用）、添加（粘贴框 + 名称，前端用正则粗校验，后端为准）、删除（确认）。帮助页增加「用公钥登录」「关闭密码登录」「核对网关主机密钥指纹」三段。
- 后台用户详情显示公钥列表（名称、指纹前缀、添加时间、最近使用），只读。

## 4. Web 入口（`internal/ingress`）

### 4.1 路由
- 公网 HTTP 监听器前加 `hostmux`：Host（去端口、转小写）匹配 `^ins-([a-z0-9]{8})-([0-9]{1,5})\.<domain>$` → ingress；等于 `<domain>` 或其它 → 现有控制台处理器（行为不变，避免影响既有测试）。
- ingress 内存路由表每 2 秒从数据库刷新（`instance_ports JOIN instances JOIN machines`，200 台规模一条 SQL），字段：instance_id、user_id、port、visibility、state、suspended、machine lan_ip、ssh_host_pubkey。查不到 Host → 404 页「该端口未开放」；实例非 running 或暂停 → 503 页（文案同 §2.3 表，`<meta refresh=10>`）。
- 保留路径 `/.spark/*`（callback、logout、健康页），不转发到容器；帮助页注明。

### 4.2 私有端口的鉴权（票据跳转）
控制台 Cookie 不发子域（§0），而且**绝不能**把 `sc_session` 改成 `Domain=.<domain>`——那样每个租户的 Web 应用（以及其他用户的公开端口页面）都会收到控制台会话。所以每个实例域名单独发一枚 host-only Cookie：

1. 浏览器打开 `https://ins-a1b2c3d4-8888.<domain>/lab`，没有有效 `sc_ingress` → 302 `https://<domain>/api/v1/ingress/authorize?target=<原 URL>`。
2. `authorize`（控制台域，GET，顶级导航所以 Lax Cookie 会带上）：未登录 → 302 `/login?redirect=/api/v1/ingress/authorize?target=…`（登录页的 `safeRedirect` 对以 `/api/v1/ingress/authorize` 开头的路径做整页跳转 `location.assign`，而不是路由导航）；已登录但不是实例主人 → 403 HTML「无权访问该实例」；主人 → 签发票据（HMAC-SHA256，密钥 `HKDF(SPARK_SECRET_KEY, "ingress-ticket")`，内容 `instance_id|port|user_id|exp(60s)|nonce`）→ 302 `https://ins-…-8888.<domain>/.spark/callback?ticket=…&next=/lab`。`target` 必须是本平台的实例域名 URL，否则 400。
3. `/.spark/callback`：验签、未过期、nonce 未用过（进程内 60 秒集合；多副本下票据在另一副本可再用一次，已接受）→ `Set-Cookie: sc_ingress=<签名令牌>; Path=/; HttpOnly; SameSite=Lax; Secure(非 dev)`，有效 12 小时，内容 `instance_id|port|user_id|exp` → 302 `next`（只允许以 `/` 开头的路径）。
4. 之后每个请求：验证 Cookie、`user_id == 当前路由表里的实例主人`、`instance_id/port` 匹配 → 转发。实例换主人不可能发生；换机产生新实例 ID，旧 Cookie 自然失效。
5. `/.spark/logout` 清 Cookie。公开端口（`visibility = public`）跳过 1～4。

转发前**删除** `sc_ingress` Cookie 头（租户应用看不到它），其它 Cookie 原样透传（应用自己的会话）。

### 4.3 转发
- `httputil.ReverseProxy`：`Transport.DialContext` 返回 `upstream.Pool.Dial(instanceID, port)`（§5.3，一条 `direct-tcpip` 通道到容器内 `127.0.0.1:<port>`）；`ForceAttemptHTTP2 = false`、`DisableCompression = true`、`FlushInterval = -1`（SSE / 流式输出不缓冲）、`ResponseHeaderTimeout = 60s`、不限请求体。设置 `X-Forwarded-For / Proto / Host`，`Host` 头保持原值。WebSocket 由 ReverseProxy 的 101 透传处理（Jupyter、Gradio、VS Code Web 都依赖）。
- 上游拨号失败 → 502 页「无法连接到实例端口，请确认服务已在容器内监听 <port>」；并按 §9 告警一次。
- 公网监听器的 `http.Server`：`ReadHeaderTimeout 10s`，不设整体超时（长连接）。

### 4.4 端口管理
- `instance_ports`（§6），每实例最多 5 条（`rental.MaxPortsPerInstance = 5`），端口 1～65535 且 ≠ 22；在实例行锁内计数。`visibility` 为 `private`（默认）/ `public`；`note` ≤ 32 字符（如「JupyterLab」）。
- 控制台实例详情新增「Web 访问」卡片：端口列表（端口、备注、私有/公开、URL + 复制 / 打开）、添加、修改可见性（设为公开需二次确认「任何人都能访问」）、删除。URL 由后端返回（`url` 字段，§7.1）。非 running 状态仍可管理端口，但 URL 旁提示「实例运行中才可访问」。
- 后台实例详情显示端口列表；ops 可删除某端口（填原因，审计 `instance.port_delete`），用于处理公开端口被滥用。

### 4.5 TLS
- `SPARK_TLS_CERT_FILE` + `SPARK_TLS_KEY_FILE` 同时设置时，公网监听器（`SPARK_HTTP_ADDR`）改为 HTTPS（证书必须覆盖 `<domain>` 与 `*.<domain>`），并可选在 `SPARK_HTTP_REDIRECT_ADDR`（默认空 = 关）起一个纯 HTTP 监听器把所有请求 301 到 https。HSTS（`max-age=31536000`）仅在 HTTPS 下发送。
- 热加载：`tls.Config.GetCertificate` 每分钟最多检查一次文件 mtime，变化即重新加载；加载失败保留旧证书并 `RaiseAlertOnce(critical, tls_cert_reload_failed, "TLS 证书文件重新加载失败，详情见日志")`。每日检查到期：剩余 < 14 天 → `RaiseAlertOnce(warning, tls_cert_expiring, "TLS 证书将在 14 天内到期")`。
- 没有配置证书文件时由前置代理终止 TLS（`SPARK_TRUST_PROXY=true`，代理须原样转发 Host，并把 `*.<domain>` 与 `<domain>` 都指到 platform）。prod 下两者都没有（无证书且 `TrustProxy=false`）→ 启动失败。
- 证书获取不进 platform：`deploy/compose` 提供 `acme.sh` 侧车（DNS-01，示例为阿里云 DNS，`dns_ali`）定期续签并写到共享卷，platform 热加载；或者直接放商业泛域名证书文件。

### 4.6 URL 生成
`SPARK_PUBLIC_BASE_URL` 成为所有环境的权威对外地址（dev 默认 `http://<domain>:<HTTP 端口>`，e2e `http://spark.test:18080`，prod 必填且须 https）。端口 URL = `scheme://ins-<id>-<port>.<domain>[:base 的端口]/`；`authorize` 与 callback 的跳转地址同样由它推导，不信任请求里的 Host 拼 URL。

## 5. 上游连接与 Agent 改动

### 5.1 实例主机密钥改由平台生成（防止上游被冒充）
- `rental.Place`（与 `AdminMigrate` 新建实例、`migrate.go` 旧路径）生成 ed25519 主机密钥：公钥（authorized_keys 一行）存 `instances.ssh_host_pubkey`，私钥 PEM 经 `secretbox` 存 `instances.ssh_host_key_enc`。
- 网关投递 Create 指令时填入 `CreateInstance.host_key_pem`（新字段 6）。Agent `writeSSHDir`：收到非空 `host_key_pem` 且本地没有主机密钥文件时写入（0600）并派生 `.pub`；已存在则保留（步骤级幂等）；为空时沿用现在的本地生成（兼容旧平台）。
- 网关与 ingress 拨上游时 `HostKeyCallback = ssh.FixedHostKey(ssh_host_pubkey)`。`ssh_host_pubkey = ''`（计划 2 创建的存量实例）→ 首次连接信任并写回（`UPDATE … WHERE id = $1 AND ssh_host_pubkey = ''`），记 warning 日志；之后固定。
- 计划 2 的「上游密钥轮换需重建容器」不变。

### 5.2 容器 sshd 配置（`initScript`）
追加：`Subsystem sftp internal-sftp`（不依赖镜像里的 sftp-server 二进制）、`AllowTcpForwarding yes`、`GatewayPorts no`、`AllowAgentForwarding yes`、`X11Forwarding no`、`PermitTunnel no`、`ClientAliveInterval 60`、`ClientAliveCountMax 3`。Agent 在 create、start、重启恢复时都重写 `init.sh`（主机密钥与 authorized_keys 不动），存量实例下次启动即生效。`docs/image-spec.md` 增加「sftp 由 sshd 内置实现提供，镜像不需要 openssh-sftp-server」。

### 5.3 上游连接池（`internal/access/upstream`）
- `Pool.Dial(instanceID, port)`：按实例取（或建立）一条共享 `ssh.Client`（`lan_ip:SPARK_UPSTREAM_SSH_PORT`，上游私钥，固定主机密钥，`ClientConfig.Timeout 10s`），返回 `client.Dial("tcp", "127.0.0.1:<port>")` 的通道。连接错误、保活失败（每 30 秒 `keepalive@openssh.com`，2 次失败）或实例离开 running 时关闭并丢弃，下次请求重建。每实例最多 1 条共享连接；sshgw 的用户会话不走这个池（§2.4）。
- 配置 `SPARK_UPSTREAM_SSH_PORT`（默认 2222，同时作为 Create 指令的 `ssh_port`），测试里指向进程内的假容器 sshd。

### 5.4 协议与指令
- `agent.proto`：`CreateInstance` 增加 `string host_key_pem = 6;`，其余不变；`make proto` 重新生成并提交。
- **不新增** `UpdatePorts`；计划 2 文档里的「随计划 3」一条按本设计关闭。
- `AgentConfig` 不变。

## 6. 数据模型（迁移 `00018_access.sql`）

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
    id          BIGSERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances (id),
    port        INT NOT NULL CHECK (port BETWEEN 1 AND 65535 AND port <> 22),
    visibility  TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'public')),
    note        TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    UNIQUE (instance_id, port)
);

CREATE TABLE ssh_sessions (
    id          BIGSERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances (id),
    user_id     BIGINT NOT NULL REFERENCES users (id),
    machine_id  BIGINT NOT NULL REFERENCES machines (id),
    auth_method TEXT NOT NULL CHECK (auth_method IN ('password', 'publickey')),
    key_id      BIGINT REFERENCES ssh_keys (id) ON DELETE SET NULL,
    client_ip   TEXT NOT NULL,
    replica_id  TEXT NOT NULL,
    bytes_up    BIGINT NOT NULL DEFAULT 0,
    bytes_down  BIGINT NOT NULL DEFAULT 0,
    started_at  TIMESTAMPTZ NOT NULL,
    ended_at    TIMESTAMPTZ
);
CREATE INDEX ssh_sessions_instance_idx ON ssh_sessions (instance_id, started_at DESC);
CREATE INDEX ssh_sessions_open_idx ON ssh_sessions (replica_id) WHERE ended_at IS NULL;

ALTER TABLE instances
    ADD COLUMN password_auth    BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN ssh_host_pubkey  TEXT NOT NULL DEFAULT '',
    ADD COLUMN ssh_host_key_enc BYTEA;                 -- 存量实例为 NULL

ALTER TABLE platform_pki ADD COLUMN ssh_host_key_enc BYTEA;  -- 网关主机密钥
```

清扫器新增：删除 `ended_at < now - 90 天` 的 `ssh_sessions`（`rental.SSHSessionRetention = 90d`）。释放实例时 `instance_ports` 行保留（历史），路由表只取 running 实例；用户视图里 released 实例不再显示端口卡片。

## 7. API

错误码（`apperr`）新增：`ssh_key_invalid`（400「公钥格式不正确或不受支持（支持 ed25519、ecdsa、rsa≥2048）」）、`ssh_key_limit`（409「最多添加 10 个公钥」）、`ssh_key_exists`（409「该公钥已添加」）、`password_auth_requires_key`（409「关闭密码登录前请先添加 SSH 公钥」）、`port_invalid`（400「端口必须是 1～65535 之间且不为 22 的整数」）、`port_limit`（409「每个实例最多开放 5 个端口」）、`port_exists`（409「该端口已添加」）。其余复用 `invalid_state` / `not_found` / `rate_limited`。

### 7.1 控制台（`/api/v1`，requireUser）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/ssh-keys` | `{items:[{id,name,fingerprint,created_at,last_used_at}]}` |
| POST | `/ssh-keys` | `{name?, public_key}` → 新记录 |
| DELETE | `/ssh-keys/{id}` | 只删自己的 |
| POST | `/instances/{id}/password/reset` | → `{password}` |
| PUT | `/instances/{id}/access` | `{password_auth}` → 实例视图 |
| GET | `/instances/{id}/ports` | `{items:[{port,visibility,note,url,created_at}]}` |
| POST | `/instances/{id}/ports` | `{port, visibility?, note?}` → 记录（含 `url`） |
| PUT | `/instances/{id}/ports/{port}` | `{visibility?, note?}` |
| DELETE | `/instances/{id}/ports/{port}` | |
| GET | `/instances/{id}/ssh-sessions` | 最近 20 条 `{auth_method, client_ip, started_at, ended_at}`（用户自查异常登录） |
| GET | `/ingress/authorize?target=` | HTML 跳转流程（§4.2），不走 JSON 错误；按用户每分钟 60 次限流 |

`instanceView` 增加 `password_auth bool`、`ssh_host string`（`ssh.<domain>`）、`ssh_port int`、`ports_count int`；`ssh_command` 规则见 §2.1。

### 7.2 后台（`/admin/api/v1`）

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| GET | `/instances/{id}` | all | 响应增加 `password_auth`、`ports[]`（含 url）、`ssh_host_pubkey_fingerprint` |
| GET | `/instances/{id}/ssh-sessions` | all | 分页，`ssh_sessions` 全字段 |
| DELETE | `/instances/{id}/ports/{port}` | ops | body `{reason}`，审计 `instance.port_delete` |
| GET | `/users/{id}` | all | 响应增加 `ssh_keys[]`（id、name、fingerprint、created_at、last_used_at） |
| GET | `/dashboard` | all | 增加 `ssh_sessions_open`（`ended_at IS NULL` 计数） |

后台页面：实例详情加「访问」卡片（密码登录开关只读、端口表 + 删除、最近 SSH 会话）；用户详情加「SSH 公钥」表；仪表盘加「在线 SSH 会话」数字。

## 8. 配置项

| 变量 | 说明 | 默认 |
|---|---|---|
| `SPARK_SSH_ADDR` | SSH 网关监听地址 | `:2222` |
| `SPARK_SSH_PUBLIC_PORT` | 写进 `ssh_command` 的公网端口 | `22` |
| `SPARK_SSH_MAX_SESSIONS_PER_INSTANCE` | 每实例并发 SSH 会话上限 | `16` |
| `SPARK_SSH_DRAIN_TIMEOUT` | 停机时等待会话结束的时间 | `30s` |
| `SPARK_UPSTREAM_SSH_PORT` | 容器 sshd 在机器上的端口（Create 的 `ssh_port` 与上游拨号共用） | `2222` |
| `SPARK_PUBLIC_BASE_URL` | 对外地址（已有）；prod 必填且 https；dev 默认 `http://<domain>:<HTTP 端口>` | 见左 |
| `SPARK_TLS_CERT_FILE` / `SPARK_TLS_KEY_FILE` | 泛域名证书与私钥（PEM），设置后公网监听器为 HTTPS 并热加载 | 空 |
| `SPARK_HTTP_REDIRECT_ADDR` | 纯 HTTP 301 到 https 的监听地址（仅 TLS 开启时有效） | 空（关） |
| `SPARK_REPLICA_ID` | 副本标识（`ssh_sessions.replica_id`） | 主机名 |

常量（代码内，不做配置）：每 IP 并发 32、认证失败阈值 10/10 分钟（IP）与 30/10 分钟（实例）、票据 60 秒、`sc_ingress` 12 小时、路由表刷新 2 秒、`MaxSSHKeys 10`、`MaxPortsPerInstance 5`。

校验：prod 下 `SPARK_PUBLIC_BASE_URL` 必须 https；prod 下未配证书且 `SPARK_TRUST_PROXY=false` → 启动失败「生产环境必须配置 TLS 证书或在反向代理后设置 SPARK_TRUST_PROXY=true」；`SPARK_SSH_ADDR` 与 `SPARK_HTTP_ADDR` 等不得相同。

## 9. 失败处理、安全与可观测

| 场景 | SSH 网关 | Web 入口 |
|---|---|---|
| 机器失联（实例暂停） | Banner + 拒绝（§2.3）；已有会话在 10 秒扫描中断开 | 503 页「机器暂时失联」 |
| 实例到期停机 / 释放 / 换机（旧 ID） | 同上 | 503 / 404 |
| 续费恢复 | 下次连接放行 | 路由表 2 秒内放行 |
| 上游拨号失败（容器 sshd 未起、lan_ip 未就绪） | disconnect「无法连接到实例，请稍后重试」 | 502 页 |
| 上游中途断开 | 透传 EOF，会话结束 | 当前请求 502，连接池重建 |
| 平台副本重启 | 会话断开（drain 30s） | 请求失败后浏览器重试即可；Cookie 无状态，仍有效 |

- 告警（`RaiseAlertOnce`，文案稳定）：`ssh_bruteforce`（§2.2）、`upstream_unreachable`（warning，machine + instance，「机器 {serial} 上实例 {id} 的 SSH 上游无法连接」，SSH 与 Web 共用，一次 5 分钟内不重复）、`tls_cert_reload_failed`、`tls_cert_expiring`。
- 审计：`instance.password_reset`、`instance.password_auth`（开/关）、`ssh_key.add` / `ssh_key.delete`、`instance.port_add` / `port_update` / `port_delete`（用户 actor）；后台删端口为 admin actor。`audit.Actor` 增加 `UserID` 字段，`Entry` 按 AdminID / UserID 决定 `actor_type`。
- 日志：SSH 会话 open/close、认证失败（含 IP、用户名、方法）、ingress 上游错误与鉴权拒绝；正常转发请求不逐条记日志。
- 安全要点：票据与 Cookie 都用 `SPARK_SECRET_KEY` 派生密钥签名；`sc_ingress` 不转发给容器；`hostkeys-00` 丢弃；`authorize` 的 `target` 与 `next` 白名单校验；公开端口是全网可达的明文 HTTP 应用（帮助页警告，建议应用自带密码）；`/.spark/*` 保留路径；SSH 用户名不存在与密码错误返回一致。

## 10. 测试与验收

### 10.1 单元（Go，无 Docker）
- `sshgw`：用户名解析与枚举防护；密码 / 公钥 / 密码关闭 / 各状态 Banner（假时钟 + 嵌入式 PG）；中继——进程内假上游（`internal/access/sshtest`：x/crypto/ssh 服务端，支持 session exec / shell / pty、`subsystem sftp`（回固定字节）、`direct-tcpip`（拨本地地址）、`tcpip-forward` + `forwarded-tcpip` 回连、`auth-agent` 通道、发 `hostkeys-00`）：exec 退出码透传、`-L` 可用、`-R` 回连可用、agent 通道透传、x11 被拒、`hostkeys-00` 被丢弃、字节计数、实例停机后 10 秒内会话被断、上游拒绝原因回传；限流与并发上限；主机密钥持久化（两次 `Ensure` 同一把、并发只留一把）；drain。
- `identity`：公钥解析矩阵（ed25519 / ecdsa / rsa-2048 / rsa-1024 拒 / dss 拒 / 垃圾 / 带注释 / 重复 / 上限 10）。
- `rental`：密码重置、`password_auth` 规则、端口 CRUD（范围、22、上限 5、重复、非主人、released）、主机密钥生成与加密、会话清理。
- `ingress`：Host 解析（大小写、带端口、非法）；票据签发 / 验签 / 过期 / 重放；Cookie 验证与剥离；私有 / 公开；状态页；`authorize` 的 `target` 校验与未登录跳转；ReverseProxy 经 `sshtest` + httptest 上游：普通请求、流式响应、WebSocket 回显、502；TLS 热加载（写新证书文件 → `GetCertificate` 返回新证书；坏文件 → 保留旧的并告警）；`hostmux` 不影响控制台路径。
- `agent`：`init.sh` 快照含 sftp 等新行；`host_key_pem` 写入 / 已存在保留 / 为空时本地生成；start 时重写 `init.sh`。
- `config`：新变量与 prod 校验。

### 10.2 集成（Go，嵌入式 PG + 进程内 fake Agent + `sshtest` 上游）
机器 `lan_ip = 127.0.0.1`，`SPARK_UPSTREAM_SSH_PORT` 指向 `sshtest`。流程：下单 → running → 密码登录执行 `echo` → 添加公钥、关闭密码 → 密码拒绝、公钥通过 → 添加端口 8888（`sshtest` 把 8888 拨到 httptest）→ 私有：无 Cookie 302 → authorize → callback → 200；公开：直接 200 → 到期停机 → SSH 拒绝、Web 503 → 续费 → 恢复 → 释放 → 拒绝；主机密钥：`sshtest` 换一把密钥后拨号失败并告警 `upstream_unreachable`；存量实例（`ssh_host_pubkey=''`）首次连接写回。

### 10.3 端到端（Playwright）
- e2e-server 内嵌 sshgw（`127.0.0.1:18022`）与一个 `sshtest` 上游；`/hooks/machines` 造的机器 `lan_ip=127.0.0.1`。新钩子 `POST /hooks/ssh {instance_id, password?|key_pem?, command}` 用进程内 ssh 客户端经网关执行并返回输出（验证页面上显示的密码 / 公钥真的能登录）；`POST /hooks/ingress-upstream {port, body}` 让 `sshtest` 在指定端口回固定内容。
- Chrome 加 `--host-resolver-rules=MAP *.spark.test 127.0.0.1`（`launchOptions.args`；`chromium` 通道同样支持）；ingress 用例在 `http://spark.test:18080` 登录。
- 覆盖表新增并登记到 `coverage.json`：
  - `C-KEY-01`～`05`：添加公钥（名称缺省取注释）、无效公钥提示、重复提示、删除、上限 10 提示。
  - `C-DET-11`～`16`：密码重置后新密码显示且 `/hooks/ssh` 能登录、关闭密码登录（无公钥时提示）、添加端口显示 URL、设为公开二次确认、上限 5、删除端口。
  - `C-ING-01`～`05`：私有端口未登录 → 登录 → 回到目标页内容正确；非主人 403；公开端口免登录；实例停机 503；未开放端口 404。
  - `A-INS-10`～`11`：后台实例详情显示端口与会话、删除端口需原因并写审计；`A-USR-07`：用户详情显示公钥。
- 注意事项：ingress 用例改变全局机器 / 实例状态的，按 `playwright.config.ts` 的顺序说明处理。

### 10.4 真机验收清单（192.168.1.221，全部通过才算完成）
平台跑在 Mac（`SPARK_DOMAIN=192-168-1-218.sslip.io`，`SPARK_PUBLIC_BASE_URL=http://192-168-1-218.sslip.io:8080`，`SPARK_SSH_ADDR=:2222`，`SPARK_SSH_PUBLIC_PORT=2222`；sslip.io 把 `*.192-168-1-218.sslip.io` 解析到 Mac，不需要改 hosts）。Agent 用本计划的新版本重新安装。需要 root 的步骤由负责人执行。

1. **开通与主机密钥**：下单 → running；`docker exec <id> cat /.spark/hostkeys/ssh_host_ed25519_key.pub` 与后台实例详情的指纹一致；`/.spark/init.sh` 含 `Subsystem sftp internal-sftp`。
2. **密码登录**：控制台复制命令 → `ssh -p 2222 ins-xxxx@ssh.192-168-1-218.sslip.io`，首次提示的主机密钥指纹与 `platform admin ssh-host-key` 一致；输入密码进入容器 root，`nvidia-smi` 正常；`ssh_sessions` 出现一行，退出后 `ended_at` 与字节数填上。
3. **公钥登录与关闭密码**：控制台添加 Mac 的 ed25519 公钥 → 免密登录；关闭密码登录 → 密码被拒、公钥仍通；删除公钥 → 公钥被拒，重新打开密码登录 → 密码通。错误用户名 `ssh foo@…` 显示 Banner「用户名应为实例 ID」。
4. **密码重置**：重置 → 旧密码被拒、新密码通；正在进行的会话不断。
5. **scp / sftp / rsync**：`scp` 1GB 文件上传到 `/root/data` 与下载各一次（记录速率）；`sftp` 交互 `ls`/`put`/`get`；`rsync -e 'ssh -p 2222'`。
6. **转发策略**：容器内起 `python3 -m http.server 8000`（镜像无 python 时用 `nc -l`）→ `ssh -L 8000:127.0.0.1:8000` 本地可访问；`ssh -R 9000:127.0.0.1:9000` 容器内 `curl 127.0.0.1:9000` 打到 Mac；`ssh -A` 后容器内 `ssh-add -l` 列出 Mac 的密钥；`ssh -X` 不可用（`DISPLAY` 为空）。
7. **Web 私有端口**：容器内 `python3 -m http.server 8888 --bind 127.0.0.1`（只监听 127.0.0.1，验证 §1.3 的优势）→ 控制台添加端口 8888 → 浏览器打开 URL → 跳转登录 → 回到目录页；换另一个账号登录访问 → 403；隐身窗口未登录 → 登录页。再起一个 WebSocket 回显（容器内 `websocat` 或 Jupyter）→ 在 `jupyter lab` 里新建终端能用（WebSocket 透传）。
8. **Web 公开端口**：设为公开 → 隐身窗口直接打开；手机热点（非局域网）打开同一 URL 成功（需要 Mac 可被公网访问时做；否则用另一台局域网设备）；改回私有 → 隐身窗口变跳转登录。
9. **状态门控**：把 `expires_at` 改到 1 分钟后 → 停机 → 已连接的 SSH 会话 10 秒内被断并显示「实例已停机」，新连接 Banner 显示保留截止时间，Web 显示 503 页；续费 → SSH 与 Web 恢复；释放 → 「实例已释放」/ 404。
10. **机器失联**：负责人 `sudo systemctl stop spark-agent` 超过 5 分钟 → 实例暂停 → SSH Banner「机器暂时失联」、Web 503；`start` 后恢复。期间已有 SSH 会话是否被断按 §2.3 规则（暂停即断）核对。
11. **隔离未削弱**：`iptables -S SPARK-FWD`、`iptables -S SPARK-IN` 与 `ExpectedFwdRules` / `ExpectedInRules` 逐行相等；`ss -lntp` 显示机器上除 2222 外没有新增任何由 Agent / Docker 发布的端口；从 Mac 直接 `curl 192.168.1.221:8888` 不通；容器内 `curl 192.168.1.218:8080`（控制面）仍被 `SPARK-FWD` 阻断（Mac 不在白名单时）。
12. **暴力破解**：脚本用错误密码连 10 次 → 第 11 次连接被立即断开；10 分钟后恢复；后台无告警（实例阈值 30 未到）；再错 30 次 → 告警 `ssh_bruteforce` 一条。
13. **重启**：负责人 `sudo reboot` → Agent 恢复容器后（计划 2 验收 §9 行为）SSH 与 Web 自动恢复，`/root/data` 文件仍在，主机密钥未变（`known_hosts` 不报警，ingress 不报 `upstream_unreachable`）。
14. **平台重启**：重启 platform 进程 → 已有 SSH 会话断开（drain）、`ssh_sessions` 残留行被同名副本启动时收尾；Web 的 `sc_ingress` Cookie 仍有效（无需重新登录）。
15. **TLS 热加载**：用 `openssl` 自签一张覆盖 `192-168-1-218.sslip.io` 与 `*.192-168-1-218.sslip.io` 的证书，配置 `SPARK_TLS_CERT_FILE/KEY_FILE` 与 `SPARK_PUBLIC_BASE_URL=https://…:8443`（`SPARK_HTTP_ADDR=:8443`），浏览器信任该 CA 后走 https 完成第 7 项；替换为第二张证书文件 → 1 分钟内 `openssl s_client` 看到新证书，不重启。
16. **compose 示例**：在 Spark 上 `docker compose -f deploy/compose/docker-compose.yml config` 通过（只校验配置，不在测试机上跑生产栈）。

验收记录写到 `docs/superpowers/acceptance-plan3.md`，格式同计划 2。

## 11. 部署
- `deploy/compose/`：`docker-compose.yml`（`platform`：映射 `22:2222`、`443:8443`、`80:8080` 重定向、`9443:9443` 仅内网；`postgres`；可选 `acme`（`neilpang/acme.sh`，`dns_ali`，证书卷共享给 platform）、`.env.example`（所有 `SPARK_*`）、`README`。platform 镜像 Dockerfile（多阶段，`make build` 产物）。
- `README`：新增配置表行、SSH 网关说明（端口映射、主机密钥指纹发布、部署会断 SSH）、Web 入口说明（泛域名 DNS `*.<domain>` 与 `<domain>` 都指向控制面；`ssh.<domain>` 也指向控制面）、证书两种方式。
- 帮助页（`HelpView`）：SSH 登录（密码 / 公钥 / 指纹核对 / 端口转发 / scp、sftp）、Web 端口（私有 / 公开、`/.spark/` 保留、公开端口风险、建议应用监听 127.0.0.1 也可）。

## 12. 范围与遗留

**本计划做**：§2～§11 全部；`UpdatePorts` 取消；`sftp` 修正；主机密钥平台化；compose 示例。

**明确不做（遗留清单，写入 `followups-plan3.md`）**
- X11 转发；FIDO/`sk-*` 公钥；SSH 证书认证；SSH 二次验证。
- platform 内置 ACME（用 acme.sh 侧车）；HTTP/3；自定义域名 CNAME 到实例端口。
- 公开端口的限流 / WAF / 基础认证分享链接；按实例的来源 IP 白名单。
- 会话录像；按会话的流量计费；SSH 网关的 Prometheus 指标。
- 多副本共享的限流计数与票据一次性集合（目前各副本独立）。
- Web 终端 / JupyterLab 一键（子项目 2 应用层）。
- 原始 TCP/UDP 端口对外暴露（只提供 HTTP/WebSocket 入口，其它协议用 `ssh -L`）。
- 平台重启不断 SSH 会话（需独立网关进程 + 连接迁移）。

## 13. 任务拆分（供实施计划引用，约 24 项）

1. 迁移 00018 + `rental` 列与结构体、`audit.Actor.UserID`。
2. `identity` SSH 公钥解析、CRUD、上限；单测。
3. `rental` 密码重置、`password_auth`、主机密钥生成（Place / Migrate / 旧迁移）、`ssh_sessions` 清理。
4. `rental` 端口 CRUD 与 `MaxPortsPerInstance`。
5. `config` 新变量与校验；README 配置表。
6. `pki`：网关主机密钥 `EnsureSSHHostKey`；`platform admin ssh-host-key`。
7. `internal/access/sshtest` 假上游（服务端）。
8. `internal/access/upstream` 连接池（固定主机密钥、TOFU 写回、保活）。
9. `sshgw` 服务端配置、Banner、认证、限流、会话记录。
10. `sshgw` 中继（通道 / 请求 / 全局请求策略、keepalive、字节计数）。
11. `sshgw` 状态扫描断开、drain、启动收尾；集成进 `app` 与 `cmd/platform`。
12. `agent.proto` `host_key_pem`；Agent `writeSSHDir` 与 `init.sh` 修订；`agentgw` 投递填充。
13. `ingress` Host 解析、路由表、状态页、404/502/503 页面。
14. `ingress` 票据、Cookie、`authorize` / callback / logout。
15. `ingress` ReverseProxy（SSH 通道拨号、WebSocket、Cookie 剥离）。
16. `hostmux` 与公网监听器整合；TLS 文件加载 / 热加载 / 重定向监听器 / HSTS；告警。
17. 控制台 API：`/ssh-keys`、`/password/reset`、`/access`、`/ports`、`/ssh-sessions`、`instanceView` 字段。
18. 后台 API：实例详情扩展、`ssh-sessions`、删端口、用户详情公钥、仪表盘计数。
19. 控制台页面：SSH 公钥页、实例详情「SSH 连接」扩展与「Web 访问」卡片、帮助页。
20. 后台页面：实例详情「访问」卡片、用户详情公钥、仪表盘。
21. Go 集成测试（§10.2）。
22. e2e-server 与钩子（sshgw、sshtest、`/hooks/ssh`、`/hooks/ingress-upstream`、host-resolver-rules）+ Playwright 用例 + `coverage.json`。
23. `deploy/compose` 与 platform Dockerfile、README 部署章节。
24. 真机验收（§10.4）与 `acceptance-plan3.md`、`followups-plan3.md`。

## 14. 由 Fable 裁定、负责人可随时推翻的事项
- 不用 sshpiper / Traefik（§1.1、§1.2）：若后续希望用现成组件，SSH 的认证与路由数据模型不变，可以替换网关层。
- 公网 SSH 端口默认 22（compose 映射），生产域名下 `ssh.<domain>` 与 `*.<domain>` 都解析到控制面公网地址——上线前要在 DNS 配置这两条记录，且**泛域名证书需要 DNS-01**（阿里云 DNS API 密钥）或购买商业泛域名证书。
- 公开端口是无鉴权的公网 HTTP 服务；平台不做限流与内容审查，仅提供后台删端口的处置手段。
- 平台重启会断开 SSH 会话（drain 30 秒）；发布窗口需避开用户高峰或提前公告。
