# Spark Cloud 计划 3：接入层（SSH 网关 + Web 入口）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让用户能经公网 SSH（`ssh ins-xxxxxxxx@ssh.<domain>`，密码或公钥）登录自己的实例、经公网 HTTPS（`https://ins-xxxxxxxx-<port>.<ingress_domain>`，默认私有、可设公开）访问实例内的 Web 端口；控制台提供 SSH 公钥 / 密码 / 端口管理，后台有对应视图；平台可按文件终止 TLS；最后在 LAN 上的 DGX Spark（192.168.1.221）完成真机验收。

**Architecture:** 平台仍是 Go 单体，新增三个进程内组件：`internal/sshgw`（自研 SSH 网关，`x/crypto/ssh` 服务端握手 → 用平台上游私钥连容器 sshd → 通道 / 请求白名单透传）、`internal/ingress`（按 Host 分流的反向代理，HTTP 请求经容器 sshd 的 `direct-tcpip` 通道进容器 `127.0.0.1:<port>`，私有端口用票据跳转 + 每实例 host-only Cookie 鉴权）、`internal/access/upstream`（按实例共享的上游 SSH 连接池，固定主机密钥）。不发布端口、不做 DNAT、**没有 `UpdatePorts` 指令**；Agent 只多接收一个 `host_key_pem` 字段并修正 `init.sh`（补 `Subsystem sftp`）。测试全部进程内：`internal/access/sshtest` 是用 `x/crypto/ssh` 写的假「容器 sshd」，单元 / 集成 / e2e-server 共用。

**Tech Stack:** Go 1.27（`~/.local/go/bin`）、pgx v5、chi v5、embedded-postgres；`golang.org/x/crypto v0.58.0`（已有：`ssh`、`ssh/agent`、`hkdf`）；`google.golang.org/protobuf`（已有，`make proto` 需要 `~/.local/bin` 的 protoc 36.2 + protoc-gen-go v1.36.12 + protoc-gen-go-grpc v1.6.2）；前端 Vue 3 + Element Plus + Vitest + Playwright 1.64（Chrome 通道，需要 `--host-resolver-rules`）。**不新增任何 Go 模块依赖。**

**Spec:** `docs/superpowers/specs/2026-10-18-plan3-access-design.md`（已批准，Fable 评审 2026-10-18）。上位：`docs/superpowers/specs/2026-10-14-spark-cloud-mvp-design.md` §5；计划 2 规格 `2026-10-18-plan2-agent-design.md` §4.2；e2e 规范与覆盖表见 `2026-10-17-plan5-frontend-design.md` §7.3、§7.5。

**分支：** `feat/plan3-gateway`（规格提交 1f905d5）。

**本计划不包含（规格 §12）：** X11、FIDO/`sk-*` 公钥、SSH 证书认证、SSH 二次验证、platform 内置 ACME、HTTP/3、自定义域名、公开端口限流 / WAF、按实例来源 IP 白名单、后台「断开某实例全部 SSH 会话」、会话录像、流量计费、Prometheus 指标、多副本共享限流、`direct-tcpip` originator 改写、Web 终端、原始 TCP/UDP 暴露、平台重启不断 SSH。

## Global Constraints

- 仓库根目录 `/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud`；所有命令前置 `export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH`。Go 模块名 `spark-cloud`。开发 Mac 没有 Docker；Go 测试用 embedded-postgres（`dbtest.Main`）。
- 决策（规格 §1.1～§1.3）：SSH 网关自研、Web 入口自研、Web 流量经容器 sshd 的 `direct-tcpip` 通道；**不新增 `UpdatePorts`**，`SPARK-FWD` / `SPARK-IN` 与 `-p <lan_ip>:2222:22` 完全不改。
- SSH 网关：监听 `SPARK_SSH_ADDR`（默认 `:2222`）；`ssh_command` 用 `SPARK_SSH_PUBLIC_PORT`（默认 22，非 22 时输出 `ssh -p <port> <id>@ssh.<domain>`）；`ServerVersion = "SSH-2.0-SparkCloud"`；主机密钥 ed25519，`secretbox` 加密存 `platform_pki.ssh_host_key_enc`（`UPDATE … WHERE id = 1 AND ssh_host_key_enc IS NULL` 后重读）；`MaxAuthTries = 6`；握手总超时 30 秒；用户名正则 `^ins-[a-z0-9]{8}$`。
- 暴力破解**按连接计数**：一条连接关闭时仍未认证成功 → 计 1 次（公钥查询不计、密码错多次只计 1）；来源 IP（IPv6 按 /64）10 分钟内 ≥ 10 → 该 IP 10 分钟内新连接直接断开；同一实例 10 分钟内 ≥ 30 → `RaiseAlertOnce(warning, ssh_bruteforce, …, "实例 <id> 的 SSH 登录失败次数超过阈值，详情见日志")`。每 IP 并发 ≤ 32，每实例并发会话 ≤ 16（`SPARK_SSH_MAX_SESSIONS_PER_INSTANCE`）。进程内计数；`DisableRateLimits` 关闭。
- 状态门控（规格 §2.3）：只有 `running 且未暂停` 允许认证；Banner 文案逐字：不存在 / 非法用户名「用户名应为实例 ID，例如 ins-a1b2c3d4」；暂停「实例所在机器暂时失联，计时已暂停，请稍后重试」；creating「实例正在开通，请稍后再试」；stopped「实例已到期停机，数据保留至 <retain_until>，续费后可继续使用」（时间格式 `2006-01-02 15:04`，北京时间）；wiping / released / failed「实例已释放」。已有会话：实例进入 stopped / wiping / released / failed 后 10 秒内断开并显示「实例已停机 / 已释放，连接已断开」；**暂停不断已有会话**（裁定 5）。
- 中继策略**白名单 + 默认拒绝**（规格 §2.4 表）：客户端 → 上游通道只允许 `session`、`direct-tcpip`；上游 → 客户端只允许 `forwarded-tcpip`、`auth-agent@openssh.com`；通道内请求全部透传；全局请求只允许 `tcpip-forward`、`cancel-tcpip-forward`、`keepalive@openssh.com`、`no-more-sessions@openssh.com`，其余回 false；上游的 `hostkeys-00@openssh.com` 丢弃。**网关不解析、不拨号任何 `direct-tcpip` / `tcpip-forward` 目标**，extra data 与 payload 原样转发。保活 60 秒 × 3；无空闲超时；drain `SPARK_SSH_DRAIN_TIMEOUT`（默认 30s）。
- 会话记录 `ssh_sessions`：建立时插入，持有副本每 60 秒批量刷新 `last_seen_at`，结束时写 `ended_at`、`bytes_up`、`bytes_down`；清扫器收尾 `ended_at IS NULL AND last_seen_at < now() - 5 分钟`（`ended_at = last_seen_at`），删除 `ended_at < now() - 90 天`。
- 凭据：密码 16 位（`randx.Password(16)`）；重置 `POST /instances/{id}/password/reset`；`instances.password_auth` 默认 true，关闭需 ≥ 1 个公钥（`password_auth_requires_key`）；`ssh_keys` 每用户 ≤ 10，接受 `ssh-ed25519`、`ecdsa-sha2-nistp{256,384,521}`、`ssh-rsa`（≥ 2048 位），拒绝 `sk-*`、`ssh-dss`；指纹 `ssh.FingerprintSHA256`；名称 1～64 字符，缺省取注释，否则「密钥 N」。
- Web 入口：Host 匹配 `^ins-([a-z0-9]{8})-([0-9]{1,5})\.<SPARK_INGRESS_DOMAIN>$`（去端口、小写）；路由表每 2 秒刷新；保留路径 `/.spark/*`；票据 HMAC-SHA256（`HKDF(SPARK_SECRET_KEY, "ingress-ticket")`，60 秒，nonce 进程内单次）；Cookie `sc_ingress`（`HKDF(SPARK_SECRET_KEY, "ingress-cookie")`，12 小时，`Path=/; HttpOnly; SameSite=Lax; Secure(非 dev)`，内容 `instance_id|port|user_id|session_hash|exp`），**逐枚验证**，绑定控制台会话（2 秒缓存）；转发前删除所有 `sc_ingress`；`POST /.spark/logout`。`next` 按 `safeRedirect` 规则（以 `/` 开头、不以 `//` 开头、不含 `\`、`url.Parse` 后无 Scheme/Host）；`target` 的 scheme / 端口等于 `SPARK_PUBLIC_BASE_URL` 推导值、host 匹配实例域名正则、无用户名密码段。
- 连接池按实例隔离：`Director` 出站 URL `http://<instance_id>:<port>/`，`Transport.DialContext` 从 addr 解析实例与端口再 `Pool.Dial`，`MaxIdleConnsPerHost = 8`；`ForceAttemptHTTP2 = false`、`DisableCompression = true`、`FlushInterval = -1`、`ResponseHeaderTimeout = 60s`；**不改 `app.Servers()` 的 Read/WriteTimeout**，ingress 用 `http.ResponseController` 清除期限。
- 同站防护（规格 §4.2.1）：`SecureCookie` 为 true 时控制台会话 Cookie 名 `__Host-sc_session`（dev 保持 `sc_session`）；控制台与后台对非 GET/HEAD/OPTIONS 请求三段规则：有 `Sec-Fetch-Site` → 只放 `same-origin` / `none`；无它但有 `Origin` → 等于对外地址 origin；两头都没有 → 放行；支付回调 `/api/v1/pay/notify/*` 豁免；拒绝返回 `forbidden`。
- TLS：`SPARK_TLS_CERT_FILE` + `SPARK_TLS_KEY_FILE` 都设置时公网监听器为 HTTPS，`GetCertificate` 每分钟最多检查一次 mtime 热加载，失败保留旧证书并 `RaiseAlertOnce(critical, tls_cert_reload_failed, "TLS 证书文件重新加载失败，详情见日志")`；剩余 < 14 天 → `RaiseAlertOnce(warning, tls_cert_expiring, "TLS 证书将在 14 天内到期")`；HSTS `max-age=31536000` 仅 HTTPS；`SPARK_HTTP_REDIRECT_ADDR` 301 到 https。prod 下无证书且 `SPARK_TRUST_PROXY=false` → 启动失败。
- 主机密钥：`rental.Place` / `AdminMigrate` 生成 ed25519，公钥存 `instances.ssh_host_pubkey`，私钥 `secretbox` 存 `instances.ssh_host_key_enc`，released / failed 时清空；**私钥不进 `agent_commands.payload`**，网关投递时经 `HostKeys.PEM` 读取填入 `CreateInstance.host_key_pem`（字段 6）；拨上游 `ssh.FixedHostKey`，`ssh_host_pubkey = ''` 时首连信任写回。**发布顺序先升级全部 Agent，再发布 platform。**
- 容器 sshd 配置追加（逐行）：`Subsystem sftp internal-sftp`、`AllowTcpForwarding yes`、`GatewayPorts no`、`AllowAgentForwarding yes`、`X11Forwarding no`、`PermitTunnel no`、`ClientAliveInterval 60`、`ClientAliveCountMax 3`、`MaxStartups 50:30:200`、`MaxSessions 32`；create / start / 重启恢复都重写 `init.sh`。
- 端口：`instance_ports` 每实例 ≤ 5（`rental.MaxPortsPerInstance = 5`），1～65535 且 ≠ 22；`visibility` private / public；`forced_private` 后用户不能改回公开（`port_forced_private`）；换机复制端口与 `password_auth`。
- 错误码新增：`ssh_key_invalid`（400「公钥格式不正确或不受支持（支持 ed25519、ecdsa、rsa≥2048）」）、`ssh_key_limit`（409「最多添加 10 个公钥」）、`ssh_key_exists`（409「该公钥已添加」）、`password_auth_requires_key`（409「关闭密码登录前请先添加 SSH 公钥」）、`port_invalid`（400「端口必须是 1～65535 之间且不为 22 的整数」）、`port_limit`（409「每个实例最多开放 5 个端口」）、`port_exists`（409「该端口已添加」）、`port_forced_private`（409「该端口已被平台限制为私有」）。
- 配置（规格 §8）：`SPARK_SSH_ADDR` `:2222`、`SPARK_SSH_PUBLIC_PORT` 22、`SPARK_SSH_MAX_SESSIONS_PER_INSTANCE` 16、`SPARK_SSH_DRAIN_TIMEOUT` 30s、`SPARK_UPSTREAM_SSH_PORT` 2222、`SPARK_INGRESS_DOMAIN` = `SPARK_DOMAIN`（须是合法域名且不是 `SPARK_DOMAIN` 的子域，相等允许）、`SPARK_ADMIN_PUBLIC_BASE_URL` 空、`SPARK_PUBLIC_BASE_URL` dev 默认 `http://<domain>:<HTTP 端口>`（prod 必填 https；host 必须等于 `SPARK_DOMAIN`）、`SPARK_TLS_CERT_FILE` / `SPARK_TLS_KEY_FILE` 空、`SPARK_HTTP_REDIRECT_ADDR` 空。
- 告警文案稳定（不含数值 / IP）：`ssh_bruteforce`、`upstream_unreachable`「机器 {serial} 上实例 {id} 的 SSH 上游无法连接」（warning，进程内按实例节流 5 分钟）、`tls_cert_reload_failed`、`tls_cert_expiring`。
- 审计动作：`instance.password_reset`、`instance.password_auth`、`ssh_key.add`、`ssh_key.delete`、`instance.port_add`、`instance.port_update`、`instance.port_delete`（用户 actor，`audit.Actor.UserID`）；后台 `instance.port_force_private`、`instance.port_delete`（admin actor）。
- 时间一律 `clock.Clock`；错误用 `apperr`；生产禁止 mock；加锁顺序沿用 `instance → agent_commands → orders → wallet；machine → wallet`，用户行锁只在 `ssh_keys` 计数时使用。
- 端到端规范沿用计划 5：用例标题以覆盖表 ID 开头、`data-testid` 命名 `页面-元素`、禁止 `waitForTimeout`、`workers: 1`；`web/e2e/coverage.json` 与覆盖表一致；现有 Playwright 用例必须保持全绿。
- 每个任务结束前必须实际运行并通过：`go vet ./... && go test ./...`；涉及前端的任务再加 `cd web && npm test && npm run build`；页面任务再加该任务列出的 Playwright 用例。证据是命令输出。
- 提交信息结尾空一行加 `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`。

## Review Focus

1. **用户名带大写、首尾空白或 `@` 后缀**（用户手敲 `INS-A1B2C3D4`、从网页复制带空格）：不能匹配到实例、不能 panic；按「用户名应为实例 ID」Banner 处理并拒绝认证 → Task 9 `TestBannerForUnknownUsernames`。
2. **公钥粘贴带 Windows 换行、多行文件或 `.pub` 整体内容（含注释与尾部空行）**：单行带 CRLF 接受并归一化；多行拒绝 `ssh_key_invalid`，不把第二行当注释 → Task 2 `TestParseSSHPublicKeyMatrix`。
3. **Host 头大小写混用、带尾点或带端口**（`INS-a1b2c3d4-8888.Spark.Test.:18080`）：归一化后仍能命中路由，而不是落到控制台 404 → Task 15 `TestParseHostNormalizes`。
4. **容器里的服务在请求中途退出**（用户 `Ctrl-C` 了 Jupyter，或上游通道被容器 sshd 关闭）：SSH 会话正常以 EOF 结束并记录字节数、不挂起；Web 请求返回 502 而不是空响应或 goroutine 泄漏 → Task 10 `TestUpstreamDropEndsSession`、Task 17 `TestUpstreamCloseMidResponseReturns502`。
5. **票据被重放或 `next` 指向外站**（用户把 callback 链接发给别人、`next=//evil.example`）：第二次使用票据 403，`next` 不合规则跳 `/` → Task 16 `TestTicketSingleUse`、`TestCallbackNextIsSafe`。

## 已做出的裁定（规格未写明或与代码核对后确定的细节）

- **sshgw / ingress 不直接依赖 `rental` 包的具体类型做鉴权数据源**：`rental` 新增 `AccessInfo`、`IngressRoute`、`SSHSessionStart` 等数据类型与方法，sshgw / ingress 通过自己声明的小接口（`sshgw.Store`、`sshgw.Keys`、`ingress.Source`、`ingress.SessionChecker`、`upstream.Store`）消费，单元测试用内存假实现，数据库实现在 `rental` / `identity` 自己的测试里覆盖。
- **实例主机密钥的生成函数可注入**：`rental.Deps.HostKeyGen func() (pub string, pem []byte, err error)`，零值用 `pki.GenerateInstanceHostKey`。e2e-server 与集成测试注入一个「总是返回 `sshtest` 假上游那把密钥」的生成器，这样所有实例的 `ssh_host_pubkey` 都等于假上游的主机公钥，`ssh.FixedHostKey` 校验在测试里同样严格生效，不需要给每台假机器一个独立地址。
- **e2e 的机器 `lan_ip`**：`/hooks/machines` 造的机器 `lan_ip = 127.0.0.1`（fake Agent 的 `IPs` 也传它），上游端口 `SPARK_UPSTREAM_SSH_PORT` = 进程内 `sshtest` 的监听端口；`machines.lan_ip` 没有唯一约束。
- **断开已有会话的提示方式**：`x/crypto/ssh` 没有「带消息的 disconnect」API。网关把消息写到该连接所有已打开 `session` 通道的 stderr（`\r\n` 结尾）再关闭连接；上游拨号失败时等客户端打开第一个 `session` 通道（最多 2 秒），写消息、发 `exit-status 1`、关闭。
- **SSH 会话计数与 `last_seen_at` 刷新**都在 sshgw 进程内（每 60 秒批量 `TouchSSHSessions`）；规格里的 `replica_id` 列不再需要（评审必改 10 已改为 `last_seen_at` 方案），迁移里不建该列。
- **`identity` 暴露会话哈希**：`identity.SessionHash(token) string` 与 `(*Service).SessionUserID(ctx, hash) (int64, error)`，`authorize` 用前者把当前会话哈希写进票据，ingress 用后者校验 Cookie 绑定的会话仍有效（结果缓存 2 秒）。
- **`authorize` 端点路径** `GET /api/v1/ingress/authorize`，注册在 `requireUser` 组外，自己读 Cookie；未登录 302 到 `<SPARK_PUBLIC_BASE_URL>/login?redirect=<本路径+query>`；返回 HTML（403 / 400 也是 HTML 页），不走 JSON 错误格式。
- **同站中间件**放在 `internal/platform/httpx`（`SameSiteGuard`），控制台与后台共用一份实现，由各自的 `Router()` 挂上；后台的对外 origin 来自 `SPARK_ADMIN_PUBLIC_BASE_URL`（空串 → 无 `Sec-Fetch-Site` 时只要有 `Origin` 就拒绝，因为无法比对）。
- **TLS 证书热加载**放在 `internal/platform/tlsfile`，与业务无关；到期 / 加载失败通过回调交给 `app` 转成 `fleet.RaiseAlertOnce`。
- **ingress 路由表只含非终态实例**（creating / running / stopped），查不到 → 404；查到但 `State != running` 或 `Suspended` → 503。released 实例的端口行保留但不进路由表。
- **`AdminMigrate` 复制端口**用 `INSERT … SELECT`（含 `forced_private`、`note`），`password_auth` 随 INSERT 列复制；新实例生成新的主机密钥（新容器）。
- **ingress 的 `Secure` Cookie 与 HSTS** 都由 `SecureCookie`（`cfg.Env != "dev"`）决定，与控制台一致；TLS 实际在前置代理终止时也成立（staging / prod 必须 https）。
- **后台 `/dashboard` 的 `ssh_sessions_open`** 直接 `count(*) WHERE ended_at IS NULL`（清扫器 5 分钟收尾保证不会无限增长）。
- **`platform admin ssh-host-key`** 在 `adminMain` 里单独处理（与 `gen-upstream-key` 一样不走 `runAdmin`），需要数据库：打印公钥行与 `SHA256:` 指纹。
- **`fakeagent` 不改**：Create 指令里的 `host_key_pem` 经 `client.toCommand` → `executor.Command.HostKeyPEM` → `writeSSHDir` 写入 fake Agent 的临时目录，与真实 Agent 完全同一条路径。
- **e2e 的 ingress 用例**在 `http://spark.test:18080` 登录（Chrome `--host-resolver-rules=MAP *.spark.test 127.0.0.1`）；其余用例继续用 `127.0.0.1:18080`；`SPARK_PUBLIC_BASE_URL=http://spark.test:18080`、`SPARK_DOMAIN=spark.test`、`SPARK_INGRESS_DOMAIN=spark.test`。
- **真机检查点 A（Task 13）在 ingress 之前**：SSH 网关与 Agent 改动完成后先上机，发现的中继缺陷在进入 Web 入口之前修掉。

## 文件结构

```
spark-cloud/
├── migrations/00018_access.sql                        # Task 1
├── api/proto/agent/v1/agent.proto、internal/agentpb/   # Task 12（host_key_pem）
├── internal/
│   ├── audit/audit.go                                 # Task 1：Actor.UserID
│   ├── rental/service.go、access.go、ports.go、alerts.go、sweeper.go、place.go、migrate.go、results.go  # Task 1、3、4
│   ├── identity/sshkeys.go、auth.go                   # Task 2（公钥）、Task 14（SessionHash / SessionUserID）
│   ├── platform/config/config.go                      # Task 5
│   ├── platform/httpx/sameorigin.go                   # Task 14
│   ├── platform/tlsfile/loader.go                     # Task 18
│   ├── pki/sshhost.go                                 # Task 6
│   ├── access/sshtest/server.go                       # Task 7：假容器 sshd
│   ├── access/upstream/pool.go、hostkey.go            # Task 8
│   ├── sshgw/{server,auth,limiter,relay,lifecycle}.go # Task 9～11
│   ├── ingress/{host,table,pages,token,auth,server,proxy,hostmux}.go  # Task 15～18
│   ├── api/{server,auth,access,ingress}.go            # Task 14、19
│   ├── adminapi/{server,access,users,instances,dashboard}.go  # Task 14、19
│   ├── agent/executor/{sshdir,ops,executor}.go、agent/client/client.go  # Task 12
│   ├── agentgw/{server,deliver,convert}.go            # Task 12
│   ├── app/app.go                                     # Task 11、18
│   └── e2ehooks/hooks.go                              # Task 22
├── cmd/platform/{main,admin}.go、cmd/e2e-server/main.go     # Task 6、11、18、22
├── web/shared/src/types/{instance,admin,access}.ts、labels.ts  # Task 20、21
├── web/console/src/views/{SshKeysView,InstanceDetailView,LoginView,HelpView,AccountView}.vue、router.ts  # Task 20
├── web/admin/src/views/{InstanceDetailView,UserDetailView,DashboardView}.vue  # Task 21
├── web/playwright.config.ts、web/e2e/{helpers/hooks.ts,console/*.spec.ts,admin/*.spec.ts,coverage.json}  # Task 22
├── deploy/compose/{docker-compose.yml,.env.example,README.md}、deploy/platform/Dockerfile  # Task 23
├── docs/image-spec.md、README.md                      # Task 12、23
└── docs/superpowers/{acceptance-plan3.md,followups-plan3.md}   # Task 24、25
```

## 任务依赖

Task 1 → 2、3 → 4（数据层）；5（配置）；6（pki，依赖 1）；7（sshtest，独立）；8（upstream，依赖 3、7）；9 → 10 → 11（sshgw，依赖 2、3、6、8；11 组装进 app / cmd）；12（proto + Agent + 网关投递，依赖 3）；**13 真机检查点 A**（依赖 11、12）；14（api 同站与 authorize，依赖 2）；15 → 16 → 17（ingress，依赖 4、8、14）；18（hostmux + TLS + 组装，依赖 17）；19（控制台 / 后台 API，依赖 3、4、18）；20、21（页面，依赖 19）；22（集成 + e2e，依赖 20、21）；23（部署产物）；**24 真机检查点 B + 验收记录**（依赖 22、23）；25（收尾）。按编号顺序执行，共 25 个任务。

---
### Task 1: 迁移 00018、`rental.Instance` 新字段、`audit.Actor.UserID`

**Files:**
- Create: `migrations/00018_access.sql`
- Modify: `internal/rental/service.go:75-122`（`Instance`、`instanceCols`、`scanInstance`）
- Modify: `internal/audit/audit.go:38-50`
- Test: `internal/rental/access_test.go`（新建）、`internal/audit/audit_test.go`（新建）

**Interfaces:**
- Produces:
  - 表 `ssh_keys`、`instance_ports`、`ssh_sessions`；`instances` 新列 `password_auth BOOLEAN NOT NULL DEFAULT true`、`ssh_host_pubkey TEXT NOT NULL DEFAULT ''`、`ssh_host_key_enc BYTEA`；`platform_pki.ssh_host_key_enc BYTEA`。
  - `rental.Instance` 新字段 `PasswordAuth bool`、`SSHHostPubkey string`（`instanceCols` 末尾追加 `password_auth, ssh_host_pubkey`）。
  - `audit.Actor{AdminID, UserID int64; IP string}`：`AdminID != 0` → `admin`；否则 `UserID != 0` → `user`（`ActorID = UserID`）；都为 0 → `system`。

- [ ] **Step 1: 写失败的测试**

`internal/audit/audit_test.go`：

```go
package audit_test

import (
	"testing"

	"spark-cloud/internal/audit"
)

func TestActorEntryResolvesActorType(t *testing.T) {
	cases := []struct {
		actor    audit.Actor
		wantType string
		wantID   int64
	}{
		{audit.Actor{AdminID: 7, IP: "1.1.1.1"}, "admin", 7},
		{audit.Actor{UserID: 42, IP: "2.2.2.2"}, "user", 42},
		{audit.Actor{AdminID: 7, UserID: 42}, "admin", 7},
		{audit.Actor{}, "system", 0},
	}
	for _, c := range cases {
		e := c.actor.Entry("x.y", "instance", "ins-1", nil)
		if e.ActorType != c.wantType || e.ActorID != c.wantID || e.IP != c.actor.IP || e.Action != "x.y" {
			t.Fatalf("%+v → %+v", c.actor, e)
		}
	}
}
```

`internal/rental/access_test.go`（本任务只放迁移与字段的测试；Task 3、4 往同一文件追加）：

```go
package rental_test

import (
	"testing"

	"spark-cloud/internal/catalog"
)

func TestInstanceAccessColumnsDefaults(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	got := e.get(inst.ID)
	if !got.PasswordAuth {
		t.Fatalf("password_auth default = %v, want true", got.PasswordAuth)
	}
	var portsCount, keysCount, sessCount int
	if err := e.pool.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM instance_ports), (SELECT count(*) FROM ssh_keys), (SELECT count(*) FROM ssh_sessions)`).
		Scan(&portsCount, &keysCount, &sessCount); err != nil {
		t.Fatalf("new tables missing: %v", err)
	}
	var hasCol bool
	if err := e.pool.QueryRow(e.ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'platform_pki' AND column_name = 'ssh_host_key_enc')`).Scan(&hasCol); err != nil || !hasCol {
		t.Fatalf("platform_pki.ssh_host_key_enc missing: %v", err)
	}
	// 端口范围与 22 由 CHECK 约束兜底（业务层另有校验）
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instance_ports (instance_id, port, created_at, updated_at) VALUES ($1, 22, now(), now())`, inst.ID); err == nil {
		t.Fatal("port 22 accepted by CHECK")
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instance_ports (instance_id, port, created_at, updated_at) VALUES ($1, 70000, now(), now())`, inst.ID); err == nil {
		t.Fatal("port 70000 accepted by CHECK")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/audit/ ./internal/rental/ -run 'TestActorEntryResolvesActorType|TestInstanceAccessColumnsDefaults' 2>&1 | head -20`
Expected: `audit` 编译错误（`UserID` 未定义）；`rental` 编译错误（`PasswordAuth` 未定义）。

- [ ] **Step 3: 写迁移**

`migrations/00018_access.sql`：

```sql
-- +goose Up
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

-- +goose Down
ALTER TABLE platform_pki DROP COLUMN ssh_host_key_enc;
ALTER TABLE instances DROP COLUMN ssh_host_key_enc, DROP COLUMN ssh_host_pubkey, DROP COLUMN password_auth;
DROP TABLE ssh_sessions;
DROP TABLE instance_ports;
DROP TABLE ssh_keys;
```

- [ ] **Step 4: `rental.Instance` 新字段**

`internal/rental/service.go` 把 `Instance` 结构体、`instanceCols`、`scanInstance` 改为：

```go
type Instance struct {
	ID              string
	UserID          int64
	MachineID       int64
	ProductID       int64
	ImageID         int64
	State           string
	PendingSeconds  int64
	ExpiresAt       *time.Time
	StoppedAt       *time.Time
	SuspendedAt     *time.Time
	PausedRemaining *int64
	Reminded24h     bool
	Reminded1h      bool
	CreatedAt       time.Time
	// 计划 3：密码登录开关与平台生成的容器主机公钥（authorized_keys 一行；存量实例为空，首连信任后写回）
	PasswordAuth  bool
	SSHHostPubkey string
}
```

```go
const instanceCols = `id, user_id, machine_id, product_id, image_id, state, pending_seconds, expires_at,
	stopped_at, suspended_at, paused_remaining, reminded_24h, reminded_1h, created_at, password_auth, ssh_host_pubkey`

func scanInstance(row pgx.Row) (Instance, error) {
	var i Instance
	err := row.Scan(&i.ID, &i.UserID, &i.MachineID, &i.ProductID, &i.ImageID, &i.State, &i.PendingSeconds, &i.ExpiresAt,
		&i.StoppedAt, &i.SuspendedAt, &i.PausedRemaining, &i.Reminded24h, &i.Reminded1h, &i.CreatedAt, &i.PasswordAuth, &i.SSHHostPubkey)
	if db.IsNoRows(err) {
		return Instance{}, apperr.ErrNotFound
	}
	return i, err
}
```

`RETURNING `+instanceCols 的各处 INSERT 不用改（新列都有默认值）。

- [ ] **Step 5: `audit.Actor.UserID`**

`internal/audit/audit.go` 把 `Actor` 与 `Entry` 改为：

```go
// Actor 标识操作主体：后台管理员（AdminID）、控制台用户（UserID）或系统（都为 0）。
type Actor struct {
	AdminID int64
	UserID  int64
	IP      string
}

// Entry 生成一条以该操作人为主体的审计记录。
func (a Actor) Entry(action, targetType, targetID string, diff any) Entry {
	actorType, actorID := "system", int64(0)
	switch {
	case a.AdminID != 0:
		actorType, actorID = "admin", a.AdminID
	case a.UserID != 0:
		actorType, actorID = "user", a.UserID
	}
	return Entry{ActorType: actorType, ActorID: actorID, Action: action, TargetType: targetType, TargetID: targetID, Diff: diff, IP: a.IP}
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/audit/ ./internal/rental/ ./internal/adminapi/ ./internal/api/ 2>&1 | tail -8`
Expected: 全部 `ok`（`rental` 的既有测试因 `RETURNING instanceCols` 增加两列而无需改动）。

- [ ] **Step 7: 提交**

```bash
git add migrations/00018_access.sql internal/rental/service.go internal/rental/access_test.go internal/audit/audit.go internal/audit/audit_test.go
git commit -m "feat(access): migration 00018 (ssh_keys, instance_ports, ssh_sessions), instance access columns, user audit actor

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 2: `identity` 用户 SSH 公钥：解析矩阵、增删查、上限、指纹查找

**Files:**
- Create: `internal/identity/sshkeys.go`、`internal/identity/sshkeys_test.go`
- Modify: `internal/platform/apperr/apperr.go`（新增三个错误）

**Interfaces:**
- Consumes: Task 1 的 `ssh_keys` 表、`audit.Actor{UserID}`。
- Produces:
  - `identity.SSHKey{ID int64; UserID int64; Name string; PublicKey string; Fingerprint string; LastUsedAt *time.Time; CreatedAt time.Time}`（JSON：`id,name,fingerprint,last_used_at,created_at`；`public_key`、`user_id` 不输出）。
  - `const identity.MaxSSHKeys = 10`。
  - `identity.ParseSSHPublicKey(raw string) (normalized, fingerprint, comment string, err error)`。
  - `(*Service).AddSSHKey(ctx, userID int64, name, raw, ip string) (SSHKey, error)`、`ListSSHKeys(ctx, userID) ([]SSHKey, error)`、`DeleteSSHKey(ctx, userID, id int64, ip string) error`、`FindSSHKey(ctx, userID int64, fingerprint string) (SSHKey, error)`、`TouchSSHKey(ctx, id int64) error`、`CountSSHKeys(ctx, userID int64) (int, error)`。
  - `apperr.ErrSSHKeyInvalid`（400 `ssh_key_invalid`）、`ErrSSHKeyLimit`（409 `ssh_key_limit`）、`ErrSSHKeyExists`（409 `ssh_key_exists`）。

- [ ] **Step 1: 写失败的测试**

`internal/identity/sshkeys_test.go`：

```go
package identity_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/sms"
)

func keyLine(t *testing.T, pub any, comment string) string {
	t.Helper()
	p, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p)))
	if comment != "" {
		line += " " + comment
	}
	return line
}

func ed25519Line(t *testing.T, comment string) string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return keyLine(t, pub, comment)
}

func rsaLine(t *testing.T, bits int) string {
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return keyLine(t, &k.PublicKey, "")
}

func TestParseSSHPublicKeyMatrix(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ok := []struct{ name, raw, wantComment string }{
		{"ed25519 with comment", ed25519Line(t, "me@mac"), "me@mac"},
		{"ed25519 CRLF", ed25519Line(t, "win") + "\r\n", "win"},
		{"ed25519 leading/trailing spaces", "  " + ed25519Line(t, "") + "  \n", ""},
		{"ecdsa p256", keyLine(t, &ec.PublicKey, "ec"), "ec"},
		{"rsa 2048", rsaLine(t, 2048), ""},
		{"options prefix", "no-pty " + ed25519Line(t, "opt"), "opt"},
	}
	for _, c := range ok {
		norm, fp, comment, err := identity.ParseSSHPublicKey(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.HasPrefix(fp, "SHA256:") || strings.Count(norm, " ") != 1 || strings.ContainsAny(norm, "\r\n") || comment != c.wantComment {
			t.Fatalf("%s: norm=%q fp=%q comment=%q", c.name, norm, fp, comment)
		}
	}
	bad := []struct{ name, raw string }{
		{"empty", "   "},
		{"junk", "ssh-ed25519 notbase64"},
		{"rsa 1024", rsaLine(t, 1024)},
		{"two lines", ed25519Line(t, "a") + "\n" + ed25519Line(t, "b")},
		{"private key", "-----BEGIN OPENSSH PRIVATE KEY-----"},
		{"dss type", strings.Replace(ed25519Line(t, ""), "ssh-ed25519", "ssh-dss", 1)},
	}
	for _, c := range bad {
		if _, _, _, err := identity.ParseSSHPublicKey(c.raw); !errors.Is(err, apperr.ErrSSHKeyInvalid) {
			t.Fatalf("%s: err = %v, want ErrSSHKeyInvalid", c.name, err)
		}
	}
}

func TestSSHKeyCRUDLimitAndLookup(t *testing.T) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	e := struct {
		svc   *identity.Service
		clock *clock.Fake
	}{identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: &sms.Mock{}}), clk}
	uid := dbtest.CreateUser(t, pool, "13700000001", true)
	other := dbtest.CreateUser(t, pool, "13700000002", true)
	ctx := context.Background()

	line := ed25519Line(t, "laptop")
	k, err := e.svc.AddSSHKey(ctx, uid, "", line, "1.2.3.4")
	if err != nil || k.Name != "laptop" || k.UserID != uid || k.LastUsedAt != nil {
		t.Fatalf("add = %+v, %v", k, err)
	}
	if _, err := e.svc.AddSSHKey(ctx, uid, "dup", line+"\n", "1.2.3.4"); !errors.Is(err, apperr.ErrSSHKeyExists) {
		t.Fatalf("duplicate = %v", err)
	}
	if _, err := e.svc.AddSSHKey(ctx, other, "same key other user", line, ""); err != nil {
		t.Fatalf("other user same key = %v", err)
	}
	if _, err := e.svc.AddSSHKey(ctx, uid, strings.Repeat("长", 65), ed25519Line(t, ""), ""); err == nil {
		t.Fatal("65-char name accepted")
	}
	for i := 2; i <= identity.MaxSSHKeys; i++ {
		if _, err := e.svc.AddSSHKey(ctx, uid, "", ed25519Line(t, ""), ""); err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	if _, err := e.svc.AddSSHKey(ctx, uid, "", ed25519Line(t, ""), ""); !errors.Is(err, apperr.ErrSSHKeyLimit) {
		t.Fatalf("11th key = %v", err)
	}
	list, err := e.svc.ListSSHKeys(ctx, uid)
	if err != nil || len(list) != identity.MaxSSHKeys || list[0].ID != k.ID || list[1].Name != "密钥 2" {
		t.Fatalf("list = %d %v (%+v)", len(list), err, list[:2])
	}
	found, err := e.svc.FindSSHKey(ctx, uid, k.Fingerprint)
	if err != nil || found.ID != k.ID || found.PublicKey != strings.Join(strings.Fields(line)[:2], " ") {
		t.Fatalf("find = %+v %v", found, err)
	}
	if _, err := e.svc.FindSSHKey(ctx, other, k.Fingerprint); err != nil { // 另一用户也添加了同一把钥匙
		t.Fatalf("find other = %v", err)
	}
	if _, err := e.svc.FindSSHKey(ctx, uid, "SHA256:nope"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("find missing = %v", err)
	}
	if err := e.svc.TouchSSHKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.svc.FindSSHKey(ctx, uid, k.Fingerprint); got.LastUsedAt == nil || !got.LastUsedAt.Equal(e.clock.Now()) {
		t.Fatalf("last_used_at = %v", got.LastUsedAt)
	}
	if err := e.svc.DeleteSSHKey(ctx, other, k.ID, ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("delete other's key = %v", err)
	}
	if err := e.svc.DeleteSSHKey(ctx, uid, k.ID, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.svc.CountSSHKeys(ctx, uid); n != identity.MaxSSHKeys-1 {
		t.Fatalf("count = %d", n)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor_type = 'user' AND actor_id = $1 AND action IN ('ssh_key.add', 'ssh_key.delete')`, uid).Scan(&audits); err != nil || audits != identity.MaxSSHKeys+1 {
		t.Fatalf("audit rows = %d %v", audits, err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/identity/ -run 'TestParseSSHPublicKeyMatrix|TestSSHKeyCRUDLimitAndLookup' 2>&1 | head`
Expected: 编译错误（`ParseSSHPublicKey` 等未定义）。

- [ ] **Step 3: 错误码**

`internal/platform/apperr/apperr.go` 的 `var (...)` 块追加：

```go
	ErrSSHKeyInvalid          = New("ssh_key_invalid", "公钥格式不正确或不受支持（支持 ed25519、ecdsa、rsa≥2048）", 400)
	ErrSSHKeyLimit            = New("ssh_key_limit", "最多添加 10 个公钥", 409)
	ErrSSHKeyExists           = New("ssh_key_exists", "该公钥已添加", 409)
	ErrPasswordAuthRequiresKey = New("password_auth_requires_key", "关闭密码登录前请先添加 SSH 公钥", 409)
	ErrPortInvalid            = New("port_invalid", "端口必须是 1～65535 之间且不为 22 的整数", 400)
	ErrPortLimit              = New("port_limit", "每个实例最多开放 5 个端口", 409)
	ErrPortExists             = New("port_exists", "该端口已添加", 409)
	ErrPortForcedPrivate      = New("port_forced_private", "该端口已被平台限制为私有", 409)
```

- [ ] **Step 4: 实现 `sshkeys.go`**

```go
package identity

import (
	"context"
	"crypto/rsa"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// MaxSSHKeys 每个用户最多保存的公钥数。
const MaxSSHKeys = 10

type SSHKey struct {
	ID          int64      `json:"id"`
	UserID      int64      `json:"-"`
	Name        string     `json:"name"`
	PublicKey   string     `json:"-"` // "<type> <base64>"，无注释
	Fingerprint string     `json:"fingerprint"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

var allowedKeyTypes = map[string]bool{
	ssh.KeyAlgoED25519: true, ssh.KeyAlgoECDSA256: true, ssh.KeyAlgoECDSA384: true, ssh.KeyAlgoECDSA521: true, ssh.KeyAlgoRSA: true,
}

// ParseSSHPublicKey 解析一行 authorized_keys 格式的公钥：去首尾空白与 CRLF，拒绝多行；只接受 ed25519 / ecdsa / rsa（≥ 2048 位）；
// 返回归一化的 "<type> <base64>"、SHA256 指纹与注释（可能为空）。
func ParseSSHPublicKey(raw string) (normalized, fingerprint, comment string, err error) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return "", "", "", apperr.ErrSSHKeyInvalid
	}
	pub, comment, _, _, perr := ssh.ParseAuthorizedKey([]byte(line))
	if perr != nil || !allowedKeyTypes[pub.Type()] {
		return "", "", "", apperr.ErrSSHKeyInvalid
	}
	if pub.Type() == ssh.KeyAlgoRSA {
		ck, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return "", "", "", apperr.ErrSSHKeyInvalid
		}
		rk, ok := ck.CryptoPublicKey().(*rsa.PublicKey)
		if !ok || rk.N.BitLen() < 2048 {
			return "", "", "", apperr.ErrSSHKeyInvalid
		}
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), ssh.FingerprintSHA256(pub), strings.TrimSpace(comment), nil
}

const sshKeyCols = `id, user_id, name, public_key, fingerprint, last_used_at, created_at`

func scanSSHKey(row pgx.Row) (SSHKey, error) {
	var k SSHKey
	err := row.Scan(&k.ID, &k.UserID, &k.Name, &k.PublicKey, &k.Fingerprint, &k.LastUsedAt, &k.CreatedAt)
	if db.IsNoRows(err) {
		return SSHKey{}, apperr.ErrNotFound
	}
	return k, err
}

// AddSSHKey 为用户添加公钥；name 为空取注释，仍为空取「密钥 N」；同指纹重复 → ErrSSHKeyExists；超过 MaxSSHKeys → ErrSSHKeyLimit。
func (s *Service) AddSSHKey(ctx context.Context, userID int64, name, raw, ip string) (SSHKey, error) {
	normalized, fp, comment, err := ParseSSHPublicKey(raw)
	if err != nil {
		return SSHKey{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = comment
	}
	if utf8.RuneCountInString(name) > 64 {
		return SSHKey{}, apperr.BadRequest("名称不能超过 64 个字符")
	}
	now := s.d.Clock.Now()
	var key SSHKey
	err = db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		// 锁用户行：并发添加不会越过上限
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ssh_keys WHERE user_id = (SELECT id FROM users WHERE id = $1 FOR UPDATE)`, userID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxSSHKeys {
			return apperr.ErrSSHKeyLimit
		}
		if name == "" {
			name = fmt.Sprintf("密钥 %d", n+1)
		}
		k, err := scanSSHKey(tx.QueryRow(ctx, `INSERT INTO ssh_keys (user_id, name, public_key, fingerprint, created_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+sshKeyCols, userID, name, normalized, fp, now))
		if db.IsUniqueViolation(err) {
			return apperr.ErrSSHKeyExists
		}
		if err != nil {
			return err
		}
		key = k
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("ssh_key.add", "ssh_key", fmt.Sprint(k.ID),
			map[string]any{"name": name, "fingerprint": fp}))
	})
	return key, err
}

func (s *Service) ListSSHKeys(ctx context.Context, userID int64) ([]SSHKey, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT `+sshKeyCols+` FROM ssh_keys WHERE user_id = $1 ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SSHKey, error) { return scanSSHKey(r) })
}

// DeleteSSHKey 删除用户自己的公钥；不属于该用户的 ID 返回 ErrNotFound。已建立的会话不受影响。
func (s *Service) DeleteSSHKey(ctx context.Context, userID, id int64, ip string) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		k, err := scanSSHKey(tx.QueryRow(ctx, `DELETE FROM ssh_keys WHERE id = $1 AND user_id = $2 RETURNING `+sshKeyCols, id, userID))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("ssh_key.delete", "ssh_key", fmt.Sprint(k.ID),
			map[string]any{"name": k.Name, "fingerprint": k.Fingerprint}))
	})
}

// FindSSHKey 按用户与指纹查找（SSH 网关公钥认证用）。
func (s *Service) FindSSHKey(ctx context.Context, userID int64, fingerprint string) (SSHKey, error) {
	return scanSSHKey(s.d.Pool.QueryRow(ctx, `SELECT `+sshKeyCols+` FROM ssh_keys WHERE user_id = $1 AND fingerprint = $2`, userID, fingerprint))
}

// TouchSSHKey 记录最近使用时间。
func (s *Service) TouchSSHKey(ctx context.Context, id int64) error {
	_, err := s.d.Pool.Exec(ctx, `UPDATE ssh_keys SET last_used_at = $2 WHERE id = $1`, id, s.d.Clock.Now())
	return err
}

func (s *Service) CountSSHKeys(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM ssh_keys WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/identity/ ./internal/platform/apperr/ 2>&1 | tail -4`
Expected: `ok`。

- [ ] **Step 6: 提交**

```bash
git add internal/identity/sshkeys.go internal/identity/sshkeys_test.go internal/platform/apperr/apperr.go
git commit -m "feat(identity): user SSH public keys (parse matrix, CRUD, limit, fingerprint lookup) and access error codes

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 3: `rental` 接入数据层：密码重置 / 开关、实例主机密钥、`AccessInfo`、SSH 会话记录、告警

**Files:**
- Create: `internal/rental/access.go`
- Modify: `internal/rental/service.go:52-62`（`Deps` 新增 `Keys`、`HostKeyGen`）、`internal/rental/place.go:249-253`、`internal/rental/migrate.go:342-346`、`internal/rental/results.go:84-86,118`、`internal/rental/alerts.go`、`internal/rental/sweeper.go:650-688`
- Modify: `internal/pki/sshhost.go`（本任务先只加 `GenerateInstanceHostKey`，网关主机密钥在 Task 6）
- Test: `internal/rental/access_test.go`（追加）、`internal/rental/helpers_test.go`（`newEnv` 传 `Keys`）

**Interfaces:**
- Consumes: Task 1 的列；`identity.(*Service).CountSSHKeys`。
- Produces:
  - `rental.KeyCounter interface { CountSSHKeys(ctx, userID int64) (int, error) }`；`rental.Deps.Keys KeyCounter`（nil 时视为 0 把钥匙）；`rental.Deps.HostKeyGen func() (pub string, pem []byte, err error)`（nil → `pki.GenerateInstanceHostKey`）。
  - `pki.GenerateInstanceHostKey() (pubLine string, privPEM []byte, err error)`：ed25519，公钥 authorized_keys 一行（无换行），私钥 OpenSSH PEM。
  - `(*Service).ResetPassword(ctx, userID int64, id, ip string) (string, error)`；`SetPasswordAuth(ctx, userID int64, id string, enabled bool, ip string) (Instance, error)`。
  - `(*Service).HostKeyPEM(ctx, instanceID string) (string, error)`（NULL → `""`；不存在 → `ErrNotFound`）；`SetSSHHostPubkeyIfEmpty(ctx, id, pub string) (bool, error)`。
  - `rental.AccessInfo{Instance Instance; Password string; MachineLanIP, MachineSerial string; AgentOnline bool; RetainUntil *time.Time}`；`(*Service).Access(ctx, instanceID string) (AccessInfo, error)`。
  - `rental.SSHSessionStart{InstanceID string; UserID, MachineID int64; AuthMethod string; KeyID *int64; ClientIP string}`；`rental.SSHSession{ID int64; InstanceID string; UserID, MachineID int64; AuthMethod string; KeyID *int64; ClientIP string; BytesUp, BytesDown int64; StartedAt, LastSeenAt time.Time; EndedAt *time.Time}`（JSON snake_case）；`OpenSSHSession(ctx, SSHSessionStart) (int64, error)`、`TouchSSHSessions(ctx, ids []int64) error`、`CloseSSHSession(ctx, id, up, down int64) error`、`ListSSHSessions(ctx, instanceID string, limit int) ([]SSHSession, error)`、`AdminListSSHSessions(ctx, instanceID string, p paging.Page) ([]SSHSession, int, error)`、`OpenSSHSessionCount(ctx) (int, error)`、`ReapSSHSessions(ctx) (closed, deleted int, err error)`（并入 `Sweep`）。
  - `rental.SSHSessionStaleAfter = 5 * time.Minute`、`rental.SSHSessionRetention = 90 * 24 * time.Hour`。
  - `rental.AlertSSHBruteforce(instanceID) string`、`AlertUpstreamUnreachable(serial, instanceID) string`；`(*Service).RaiseSSHBruteforce(ctx, instanceID string) error`、`RaiseUpstreamUnreachable(ctx, instanceID string) error`（都是 `RaiseAlertOnce`）。
  - `failCreating` 与 wipe → released 都把 `ssh_host_key_enc` 置 NULL；`Place` / `AdminMigrate` 写入主机密钥；`AdminMigrate` 复制 `password_auth`。

- [ ] **Step 1: 写失败的测试**

`internal/rental/access_test.go` 追加（import 与 Task 1 的合并成一个块）：

```go
import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/rental"
)

func TestPlaceGeneratesHostKeyAndResetPassword(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	got := e.get(inst.ID)
	if !strings.HasPrefix(got.SSHHostPubkey, "ssh-ed25519 ") || strings.ContainsAny(got.SSHHostPubkey, "\n") {
		t.Fatalf("host pubkey = %q", got.SSHHostPubkey)
	}
	pem, err := e.svc.HostKeyPEM(e.ctx, inst.ID)
	if err != nil || !strings.Contains(pem, "OPENSSH PRIVATE KEY") {
		t.Fatalf("pem = %q %v", pem, err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(pem))
	if err != nil || strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) != got.SSHHostPubkey {
		t.Fatalf("pem does not match pubkey: %v", err)
	}
	if _, err := e.svc.HostKeyPEM(e.ctx, "ins-nope0000"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	old, _ := e.svc.Password(e.ctx, e.userID, inst.ID)
	pw, err := e.svc.ResetPassword(e.ctx, e.userID, inst.ID, "9.9.9.9")
	if err != nil || len(pw) != 16 || pw == old {
		t.Fatalf("reset = %q %v (old %q)", pw, err, old)
	}
	if now, _ := e.svc.Password(e.ctx, e.userID, inst.ID); now != pw {
		t.Fatalf("stored password = %q, want %q", now, pw)
	}
	if _, err := e.svc.ResetPassword(e.ctx, e.userID+1, inst.ID, ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user reset = %v", err)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE actor_type = 'user' AND action = 'instance.password_reset' AND target_id = $1`, inst.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("audit = %d", n)
	}
}

func TestFailedAndReleasedClearHostKey(t *testing.T) {
	e := newEnv(t, 2)
	_, failed := e.place(catalog.D1d)
	e.ack(false) // create 失败 → failed
	if pem, err := e.svc.HostKeyPEM(e.ctx, failed.ID); err != nil || pem != "" {
		t.Fatalf("failed instance pem = %q %v", pem, err)
	}
	_, inst := e.place(catalog.D1d)
	e.ack(true) // running
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	e.ack(true) // wipe → released
	if st := e.get(inst.ID).State; st != rental.StateReleased {
		t.Fatalf("state = %s", st)
	}
	if pem, err := e.svc.HostKeyPEM(e.ctx, inst.ID); err != nil || pem != "" {
		t.Fatalf("released pem = %q %v", pem, err)
	}
	if _, err := e.svc.ResetPassword(e.ctx, e.userID, inst.ID, ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("reset released = %v", err)
	}
}

func TestPasswordAuthToggleRequiresKey(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if _, err := e.svc.SetPasswordAuth(e.ctx, e.userID, inst.ID, false, ""); !errors.Is(err, apperr.ErrPasswordAuthRequiresKey) {
		t.Fatalf("disable without key = %v", err)
	}
	e.keys.n = 1
	got, err := e.svc.SetPasswordAuth(e.ctx, e.userID, inst.ID, false, "1.1.1.1")
	if err != nil || got.PasswordAuth {
		t.Fatalf("disable = %+v %v", got, err)
	}
	e.keys.n = 0 // 删光钥匙后仍可重新打开密码登录
	if got, err = e.svc.SetPasswordAuth(e.ctx, e.userID, inst.ID, true, ""); err != nil || !got.PasswordAuth {
		t.Fatalf("enable = %+v %v", got, err)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = 'instance.password_auth' AND target_id = $1`, inst.ID).Scan(&n)
	if n != 2 {
		t.Fatalf("audit = %d", n)
	}
}

func TestMigrateCopiesPasswordAuthAndNewHostKey(t *testing.T) {
	e := newEnv(t, 2)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.keys.n = 1
	if _, err := e.svc.SetPasswordAuth(e.ctx, e.userID, inst.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	moved, err := e.svc.AdminMigrate(e.ctx, audit.Actor{AdminID: 1}, inst.ID, "bad gpu")
	if err != nil {
		t.Fatal(err)
	}
	got := e.get(moved.ID)
	if got.PasswordAuth || got.SSHHostPubkey == "" || got.SSHHostPubkey == e.get(inst.ID).SSHHostPubkey {
		t.Fatalf("moved = %+v", got)
	}
}

func TestAccessInfoAndTOFU(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	info, err := e.svc.Access(e.ctx, inst.ID)
	if err != nil || info.Instance.State != rental.StateRunning || len(info.Password) != 16 || info.MachineLanIP != "10.0.0.10" ||
		!info.AgentOnline || info.MachineSerial != "SN000" || info.RetainUntil != nil || info.Instance.SSHHostPubkey == "" {
		t.Fatalf("access = %+v %v", info, err)
	}
	if _, err := e.svc.Access(e.ctx, "ins-nope0000"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	// 存量实例：公钥为空时首连写回，之后不再覆盖
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET ssh_host_pubkey = '' WHERE id = $1`, inst.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.svc.SetSSHHostPubkeyIfEmpty(e.ctx, inst.ID, "ssh-ed25519 AAAAfirst"); err != nil || !ok {
		t.Fatalf("tofu first = %v %v", ok, err)
	}
	if ok, err := e.svc.SetSSHHostPubkeyIfEmpty(e.ctx, inst.ID, "ssh-ed25519 AAAAsecond"); err != nil || ok {
		t.Fatalf("tofu second = %v %v", ok, err)
	}
	if e.get(inst.ID).SSHHostPubkey != "ssh-ed25519 AAAAfirst" {
		t.Fatal("tofu overwritten")
	}
	// 到期停机后 RetainUntil 有值
	e.clock.Advance(25 * time.Hour)
	if _, err := e.svc.ExpireDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	info, _ = e.svc.Access(e.ctx, inst.ID)
	if info.Instance.State != rental.StateStopped || info.RetainUntil == nil || !info.RetainUntil.Equal(e.clock.Now().Add(rental.RetainAfterStop)) {
		t.Fatalf("stopped access = %+v", info)
	}
}

func TestSSHSessionsLifecycleAndReap(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	id, err := e.svc.OpenSSHSession(e.ctx, rental.SSHSessionStart{InstanceID: inst.ID, UserID: e.userID, MachineID: e.machines[0], AuthMethod: "password", ClientIP: "203.0.113.9"})
	if err != nil || id == 0 {
		t.Fatalf("open = %d %v", id, err)
	}
	if n, _ := e.svc.OpenSSHSessionCount(e.ctx); n != 1 {
		t.Fatalf("open count = %d", n)
	}
	e.clock.Advance(time.Minute)
	if err := e.svc.TouchSSHSessions(e.ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.ListSSHSessions(e.ctx, inst.ID, 20)
	if err != nil || len(list) != 1 || !list[0].LastSeenAt.Equal(e.clock.Now()) || list[0].EndedAt != nil {
		t.Fatalf("list = %+v %v", list, err)
	}
	if err := e.svc.CloseSSHSession(e.ctx, id, 100, 2000); err != nil {
		t.Fatal(err)
	}
	list, _ = e.svc.ListSSHSessions(e.ctx, inst.ID, 20)
	if list[0].EndedAt == nil || list[0].BytesUp != 100 || list[0].BytesDown != 2000 {
		t.Fatalf("closed = %+v", list[0])
	}
	// 崩溃残留：open 后 5 分钟没刷新 → 清扫收尾 ended_at = last_seen_at
	stale, _ := e.svc.OpenSSHSession(e.ctx, rental.SSHSessionStart{InstanceID: inst.ID, UserID: e.userID, MachineID: e.machines[0], AuthMethod: "publickey", ClientIP: "203.0.113.9"})
	seen := e.clock.Now()
	e.clock.Advance(rental.SSHSessionStaleAfter + time.Second)
	closed, deleted, err := e.svc.ReapSSHSessions(e.ctx)
	if err != nil || closed != 1 || deleted != 0 {
		t.Fatalf("reap = %d %d %v", closed, deleted, err)
	}
	page, total, err := e.svc.AdminListSSHSessions(e.ctx, inst.ID, paging.Page{Number: 1, Size: 10})
	if err != nil || total != 2 || page[0].ID != stale || page[0].EndedAt == nil || !page[0].EndedAt.Equal(seen) {
		t.Fatalf("admin list = %+v %d %v", page, total, err)
	}
	e.clock.Advance(rental.SSHSessionRetention)
	if _, deleted, _ := e.svc.ReapSSHSessions(e.ctx); deleted != 2 {
		t.Fatalf("retention deleted = %d", deleted)
	}
	if n, _ := e.svc.OpenSSHSessionCount(e.ctx); n != 0 {
		t.Fatalf("open count after reap = %d", n)
	}
}

func TestAccessAlertsAreStableAndOnce(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	for i := 0; i < 2; i++ {
		if err := e.svc.RaiseSSHBruteforce(e.ctx, inst.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.RaiseUpstreamUnreachable(e.ctx, inst.ID); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind IN ('ssh_bruteforce', 'upstream_unreachable')`).Scan(&n)
	if n != 2 {
		t.Fatalf("alerts = %d, want 2 (deduplicated)", n)
	}
	var msg string
	_ = e.pool.QueryRow(e.ctx, `SELECT message FROM alerts WHERE kind = 'upstream_unreachable'`).Scan(&msg)
	if msg != rental.AlertUpstreamUnreachable("SN000", inst.ID) || msg != "机器 SN000 上实例 "+inst.ID+" 的 SSH 上游无法连接" {
		t.Fatalf("message = %q", msg)
	}
}
```

`internal/rental/helpers_test.go`：`env` 结构体增加 `keys *fakeKeys`，`newEnv` 里 `n := &fakeNotifier{}` 之后加 `keys := &fakeKeys{}` 并传 `Keys: keys` 进 `rental.Deps`，`e` 字面量加 `keys: keys`；文件末尾追加：

```go
// fakeKeys 模拟 identity.CountSSHKeys：n 为用户当前公钥数。
type fakeKeys struct{ n int }

func (f *fakeKeys) CountSSHKeys(context.Context, int64) (int, error) { return f.n, nil }
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'TestPlaceGeneratesHostKey|TestFailedAndReleased|TestPasswordAuthToggle|TestMigrateCopies|TestAccessInfo|TestSSHSessions|TestAccessAlerts' 2>&1 | head`
Expected: 编译错误。

- [ ] **Step 3: `pki.GenerateInstanceHostKey`**

`internal/pki/sshhost.go`（新建，Task 6 再往里加网关主机密钥）：

```go
package pki

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"strings"

	"golang.org/x/crypto/ssh"
)

// GenerateInstanceHostKey 生成一把容器 sshd 主机密钥（ed25519）：返回 authorized_keys 一行的公钥与 OpenSSH PEM 私钥。
func GenerateInstanceHostKey() (pubLine string, privPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", nil, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), pem.EncodeToMemory(block), nil
}
```

- [ ] **Step 4: `Deps` 与插入路径**

`internal/rental/service.go` 的 `Deps` 追加两个字段，并在 `NewService` 里补默认值：

```go
type KeyCounter interface {
	CountSSHKeys(ctx context.Context, userID int64) (int, error)
}

type Deps struct {
	Pool     *pgxpool.Pool
	Clock    clock.Clock
	Box      *secretbox.Box
	Log      *slog.Logger
	Users    UserChecker
	Wallet   *wallet.Service
	Catalog  *catalog.Service
	Fleet    *fleet.Service
	Notifier Notifier
	// 计划 3
	Keys       KeyCounter                                  // 关闭密码登录前校验用户至少有一把公钥；nil 视为 0 把
	HostKeyGen func() (pub string, pem []byte, err error) // 实例主机密钥生成；nil → pki.GenerateInstanceHostKey
}

func NewService(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.HostKeyGen == nil {
		d.HostKeyGen = pki.GenerateInstanceHostKey
	}
	return &Service{d: d}
}
```

（import 追加 `"spark-cloud/internal/pki"`。）

`internal/rental/place.go` 的 INSERT 改为：

```go
		hostPub, hostPEM, err := s.d.HostKeyGen()
		if err != nil {
			return err
		}
		id := randx.ID("ins-", 8)
		inst, err = scanInstance(tx.QueryRow(ctx, `INSERT INTO instances
			(id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, ssh_host_pubkey, ssh_host_key_enc, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'creating', $6, $7, $8, $9, $10, $10) RETURNING `+instanceCols,
			id, req.UserID, m.ID, req.ProductID, img.ID, seconds(req.Duration), s.d.Box.Seal([]byte(randx.Password(16))), hostPub, s.d.Box.Seal(hostPEM), now))
```

`internal/rental/migrate.go` 的 INSERT 改为（复制 `password_auth`，新主机密钥）：

```go
		hostPub, hostPEM, err := s.d.HostKeyGen()
		if err != nil {
			return err
		}
		id := randx.ID("ins-", 8)
		moved, err = scanInstance(tx.QueryRow(ctx, `INSERT INTO instances
			(id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, password_auth, ssh_host_pubkey, ssh_host_key_enc, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'creating', $6, $7, $8, $9, $10, $11, $11) RETURNING `+instanceCols,
			id, old.UserID, m.ID, old.ProductID, old.ImageID, remaining, s.d.Box.Seal([]byte(randx.Password(16))), old.PasswordAuth, hostPub, s.d.Box.Seal(hostPEM), now))
```

`internal/rental/results.go`：第 118 行 `failCreating` 的 UPDATE 改为

```go
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'failed', pending_seconds = 0, ssh_host_key_enc = NULL, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
```

第 84 行 wipe 成功的 UPDATE 改为

```go
			if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'released', ssh_host_key_enc = NULL, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
```

- [ ] **Step 5: `access.go`**

```go
package rental

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/randx"
)

const (
	SSHSessionStaleAfter = 5 * time.Minute       // last_seen_at 超过此值仍未结束的会话由清扫器收尾
	SSHSessionRetention  = 90 * 24 * time.Hour   // 已结束会话的保留期
)

// ResetPassword 重新生成实例 SSH 密码（released / failed 不允许），返回明文。进行中的会话不受影响。
func (s *Service) ResetPassword(ctx context.Context, userID int64, id, ip string) (string, error) {
	now := s.d.Clock.Now()
	pw := randx.Password(16)
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.State == StateReleased || inst.State == StateFailed {
			return apperr.ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `UPDATE instances SET ssh_password_enc = $2, updated_at = $3 WHERE id = $1`, id, s.d.Box.Seal([]byte(pw)), now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.password_reset", "instance", id, nil))
	})
	if err != nil {
		return "", err
	}
	return pw, nil
}

// SetPasswordAuth 打开 / 关闭实例的密码登录；关闭要求用户至少有一把公钥。
func (s *Service) SetPasswordAuth(ctx context.Context, userID int64, id string, enabled bool, ip string) (Instance, error) {
	now := s.d.Clock.Now()
	var out Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.State == StateReleased || inst.State == StateFailed {
			return apperr.ErrInvalidState
		}
		if !enabled {
			n := 0
			if s.d.Keys != nil {
				if n, err = s.d.Keys.CountSSHKeys(ctx, userID); err != nil {
					return err
				}
			}
			if n == 0 {
				return apperr.ErrPasswordAuthRequiresKey
			}
		}
		out, err = scanInstance(tx.QueryRow(ctx, `UPDATE instances SET password_auth = $2, updated_at = $3 WHERE id = $1 RETURNING `+instanceCols, id, enabled, now))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.password_auth", "instance", id, map[string]any{"enabled": enabled}))
	})
	return out, err
}

// HostKeyPEM 解密返回实例容器的主机私钥（网关投递 Create 指令时填入 host_key_pem）；已清空返回空串。
func (s *Service) HostKeyPEM(ctx context.Context, instanceID string) (string, error) {
	var enc []byte
	err := s.d.Pool.QueryRow(ctx, `SELECT ssh_host_key_enc FROM instances WHERE id = $1`, instanceID).Scan(&enc)
	if db.IsNoRows(err) {
		return "", apperr.ErrNotFound
	}
	if err != nil || enc == nil {
		return "", err
	}
	plain, err := s.d.Box.Open(enc)
	if err != nil {
		return "", fmt.Errorf("解密实例 %s 主机密钥: %w", instanceID, err)
	}
	return string(plain), nil
}

// SetSSHHostPubkeyIfEmpty 存量实例首连信任：只在公钥为空时写入，返回是否写入。
func (s *Service) SetSSHHostPubkeyIfEmpty(ctx context.Context, id, pub string) (bool, error) {
	tag, err := s.d.Pool.Exec(ctx, `UPDATE instances SET ssh_host_pubkey = $2, updated_at = $3 WHERE id = $1 AND ssh_host_pubkey = ''`, id, pub, s.d.Clock.Now())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// AccessInfo 是 SSH 网关 / Web 入口判定与拨号所需的全部信息。
type AccessInfo struct {
	Instance      Instance
	Password      string // 明文（只在认证时比较）
	MachineLanIP  string
	MachineSerial string
	AgentOnline   bool
	RetainUntil   *time.Time // stopped 时的数据保留截止
}

func (s *Service) Access(ctx context.Context, instanceID string) (AccessInfo, error) {
	var info AccessInfo
	var enc []byte
	row := s.d.Pool.QueryRow(ctx, `SELECT `+instanceCols+`, ssh_password_enc, m.lan_ip, m.serial, m.agent_online
		FROM instances JOIN machines m ON m.id = instances.machine_id WHERE instances.id = $1`, instanceID)
	i := &info.Instance
	err := row.Scan(&i.ID, &i.UserID, &i.MachineID, &i.ProductID, &i.ImageID, &i.State, &i.PendingSeconds, &i.ExpiresAt,
		&i.StoppedAt, &i.SuspendedAt, &i.PausedRemaining, &i.Reminded24h, &i.Reminded1h, &i.CreatedAt, &i.PasswordAuth, &i.SSHHostPubkey,
		&enc, &info.MachineLanIP, &info.MachineSerial, &info.AgentOnline)
	if db.IsNoRows(err) {
		return AccessInfo{}, apperr.ErrNotFound
	}
	if err != nil {
		return AccessInfo{}, err
	}
	plain, err := s.d.Box.Open(enc)
	if err != nil {
		return AccessInfo{}, err
	}
	info.Password = string(plain)
	if i.State == StateStopped && i.StoppedAt != nil && i.PendingSeconds == 0 {
		t := i.StoppedAt.Add(RetainAfterStop)
		info.RetainUntil = &t
	}
	return info, nil
}

// ---- SSH 会话记录 ----

type SSHSessionStart struct {
	InstanceID string
	UserID     int64
	MachineID  int64
	AuthMethod string // password / publickey
	KeyID      *int64
	ClientIP   string
}

type SSHSession struct {
	ID         int64      `json:"id"`
	InstanceID string     `json:"instance_id"`
	UserID     int64      `json:"user_id"`
	MachineID  int64      `json:"machine_id"`
	AuthMethod string     `json:"auth_method"`
	KeyID      *int64     `json:"key_id"`
	ClientIP   string     `json:"client_ip"`
	BytesUp    int64      `json:"bytes_up"`
	BytesDown  int64      `json:"bytes_down"`
	StartedAt  time.Time  `json:"started_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	EndedAt    *time.Time `json:"ended_at"`
}

const sshSessionCols = `id, instance_id, user_id, machine_id, auth_method, key_id, client_ip, bytes_up, bytes_down, started_at, last_seen_at, ended_at`

func scanSSHSession(row pgx.Row) (SSHSession, error) {
	var x SSHSession
	err := row.Scan(&x.ID, &x.InstanceID, &x.UserID, &x.MachineID, &x.AuthMethod, &x.KeyID, &x.ClientIP, &x.BytesUp, &x.BytesDown, &x.StartedAt, &x.LastSeenAt, &x.EndedAt)
	return x, err
}

func (s *Service) OpenSSHSession(ctx context.Context, st SSHSessionStart) (int64, error) {
	now := s.d.Clock.Now()
	var id int64
	err := s.d.Pool.QueryRow(ctx, `INSERT INTO ssh_sessions (instance_id, user_id, machine_id, auth_method, key_id, client_ip, started_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7) RETURNING id`, st.InstanceID, st.UserID, st.MachineID, st.AuthMethod, st.KeyID, st.ClientIP, now).Scan(&id)
	return id, err
}

func (s *Service) TouchSSHSessions(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.d.Pool.Exec(ctx, `UPDATE ssh_sessions SET last_seen_at = $2 WHERE id = ANY($1) AND ended_at IS NULL`, ids, s.d.Clock.Now())
	return err
}

func (s *Service) CloseSSHSession(ctx context.Context, id, up, down int64) error {
	now := s.d.Clock.Now()
	_, err := s.d.Pool.Exec(ctx, `UPDATE ssh_sessions SET ended_at = $2, last_seen_at = $2, bytes_up = $3, bytes_down = $4 WHERE id = $1 AND ended_at IS NULL`, id, now, up, down)
	return err
}

func (s *Service) ListSSHSessions(ctx context.Context, instanceID string, limit int) ([]SSHSession, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT `+sshSessionCols+` FROM ssh_sessions WHERE instance_id = $1 ORDER BY started_at DESC, id DESC LIMIT $2`, instanceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SSHSession, error) { return scanSSHSession(r) })
}

func (s *Service) AdminListSSHSessions(ctx context.Context, instanceID string, p paging.Page) ([]SSHSession, int, error) {
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM ssh_sessions WHERE instance_id = $1`, instanceID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT `+sshSessionCols+` FROM ssh_sessions WHERE instance_id = $1 ORDER BY started_at DESC, id DESC LIMIT $2 OFFSET $3`,
		instanceID, p.Limit(), p.Offset())
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SSHSession, error) { return scanSSHSession(r) })
	return list, total, err
}

func (s *Service) OpenSSHSessionCount(ctx context.Context) (int, error) {
	var n int
	err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM ssh_sessions WHERE ended_at IS NULL`).Scan(&n)
	return n, err
}

// ReapSSHSessions：收尾超过 SSHSessionStaleAfter 未刷新的会话（副本崩溃 / 重建改名），删除超过保留期的已结束会话。
func (s *Service) ReapSSHSessions(ctx context.Context) (closed, deleted int, err error) {
	now := s.d.Clock.Now()
	tag, err := s.d.Pool.Exec(ctx, `UPDATE ssh_sessions SET ended_at = last_seen_at WHERE ended_at IS NULL AND last_seen_at < $1`, now.Add(-SSHSessionStaleAfter))
	if err != nil {
		return 0, 0, err
	}
	closed = int(tag.RowsAffected())
	tag, err = s.d.Pool.Exec(ctx, `DELETE FROM ssh_sessions WHERE ended_at IS NOT NULL AND ended_at < $1`, now.Add(-SSHSessionRetention))
	if err != nil {
		return closed, 0, err
	}
	return closed, int(tag.RowsAffected()), nil
}

// ---- 告警（文案在 alerts.go，稳定、无数值）----

func (s *Service) RaiseSSHBruteforce(ctx context.Context, instanceID string) error {
	inst, err := s.Get(ctx, instanceID)
	if err != nil {
		return err
	}
	return s.d.Fleet.RaiseAlertOnce(ctx, s.d.Pool, fleet.Alert{Level: "warning", Kind: "ssh_bruteforce",
		MachineID: inst.MachineID, InstanceID: instanceID, Message: AlertSSHBruteforce(instanceID)})
}

func (s *Service) RaiseUpstreamUnreachable(ctx context.Context, instanceID string) error {
	inst, err := s.Get(ctx, instanceID)
	if err != nil {
		return err
	}
	m, err := s.d.Fleet.Get(ctx, s.d.Pool, inst.MachineID)
	if err != nil {
		return err
	}
	return s.d.Fleet.RaiseAlertOnce(ctx, s.d.Pool, fleet.Alert{Level: "warning", Kind: "upstream_unreachable",
		MachineID: inst.MachineID, InstanceID: instanceID, Message: AlertUpstreamUnreachable(m.Serial, instanceID)})
}
```

`internal/rental/alerts.go` 追加：

```go
func AlertSSHBruteforce(instanceID string) string {
	return fmt.Sprintf("实例 %s 的 SSH 登录失败次数超过阈值，详情见日志", instanceID)
}
func AlertUpstreamUnreachable(serial, instanceID string) string {
	return fmt.Sprintf("机器 %s 上实例 %s 的 SSH 上游无法连接", serial, instanceID)
}
```

`internal/rental/sweeper.go` 的 `Sweep` 在 `SendReminders` 之前加：

```go
	if _, _, err := s.ReapSSHSessions(ctx); err != nil {
		errs = append(errs, fmt.Errorf("ssh sessions: %w", err))
	}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ ./internal/pki/ 2>&1 | tail -4`
Expected: `ok`。

- [ ] **Step 7: 提交**

```bash
git add internal/rental internal/pki/sshhost.go
git commit -m "feat(rental): instance host keys, password reset/toggle, access info, ssh session records, access alerts

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 4: `rental` 端口管理：CRUD、上限、`forced_private`、路由表查询、换机复制

**Files:**
- Create: `internal/rental/ports.go`
- Modify: `internal/rental/migrate.go`（复制端口）
- Test: `internal/rental/ports_test.go`

**Interfaces:**
- Produces:
  - `const rental.MaxPortsPerInstance = 5`。
  - `rental.Port{InstanceID string; Port int; Visibility string; ForcedPrivate bool; Note string; CreatedAt, UpdatedAt time.Time}`（JSON：`port,visibility,forced_private,note,created_at,updated_at`）。
  - `(*Service).ListPorts(ctx, instanceID string) ([]Port, error)`；`AddPort(ctx, userID int64, id string, port int, visibility, note, ip string) (Port, error)`；`UpdatePort(ctx, userID int64, id string, port int, visibility, note *string, ip string) (Port, error)`；`DeletePort(ctx, userID int64, id string, port int, ip string) error`；`AdminForcePortPrivate(ctx, actor audit.Actor, id string, port int, reason string) (Port, error)`；`AdminDeletePort(ctx, actor audit.Actor, id string, port int, reason string) error`。
  - `rental.IngressRoute{InstanceID string; UserID int64; Port int; Visibility string; State string; Suspended bool; LanIP string; HostPubkey string}`；`(*Service).ListIngressRoutes(ctx) ([]IngressRoute, error)`（只含 creating / running / stopped 实例的端口）。

- [ ] **Step 1: 写失败的测试**

`internal/rental/ports_test.go`：

```go
package rental_test

import (
	"errors"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

func str(s string) *string { return &s }

func TestPortCRUDAndLimits(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	p, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8888, "", "JupyterLab", "1.1.1.1")
	if err != nil || p.Port != 8888 || p.Visibility != "private" || p.ForcedPrivate || p.Note != "JupyterLab" {
		t.Fatalf("add = %+v %v", p, err)
	}
	for _, bad := range []int{0, 22, 65536, -1} {
		if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, bad, "private", "", ""); !errors.Is(err, apperr.ErrPortInvalid) {
			t.Fatalf("port %d = %v", bad, err)
		}
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8888, "public", "", ""); !errors.Is(err, apperr.ErrPortExists) {
		t.Fatalf("dup = %v", err)
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 80, "weird", "", ""); err == nil {
		t.Fatal("bad visibility accepted")
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 81, "private", string(make([]rune, 33)), ""); err == nil {
		t.Fatal("33-char note accepted")
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID+1, inst.ID, 82, "private", "", ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user = %v", err)
	}
	for port := 8001; port <= 8004; port++ {
		if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, port, "private", "", ""); err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8005, "private", "", ""); !errors.Is(err, apperr.ErrPortLimit) {
		t.Fatalf("6th = %v", err)
	}
	list, _ := e.svc.ListPorts(e.ctx, inst.ID)
	if len(list) != rental.MaxPortsPerInstance || list[0].Port != 8001 || list[4].Port != 8888 {
		t.Fatalf("list = %+v", list)
	}
	up, err := e.svc.UpdatePort(e.ctx, e.userID, inst.ID, 8888, str("public"), nil, "")
	if err != nil || up.Visibility != "public" || up.Note != "JupyterLab" {
		t.Fatalf("update = %+v %v", up, err)
	}
	if _, err := e.svc.UpdatePort(e.ctx, e.userID, inst.ID, 9999, str("public"), nil, ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("update missing = %v", err)
	}
	if err := e.svc.DeletePort(e.ctx, e.userID, inst.ID, 8001, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePort(e.ctx, e.userID, inst.ID, 8001, ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("delete twice = %v", err)
	}
	var audits int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE actor_type = 'user' AND action IN ('instance.port_add', 'instance.port_update', 'instance.port_delete')`).Scan(&audits)
	if audits != 7 {
		t.Fatalf("audit = %d", audits)
	}
}

func TestPortForcedPrivateAndAdminDelete(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8888, "public", "", ""); err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{AdminID: 3, IP: "10.0.0.1"}
	if _, err := e.svc.AdminForcePortPrivate(e.ctx, actor, inst.ID, 8888, ""); err == nil {
		t.Fatal("empty reason accepted")
	}
	p, err := e.svc.AdminForcePortPrivate(e.ctx, actor, inst.ID, 8888, "钓鱼页面")
	if err != nil || p.Visibility != "private" || !p.ForcedPrivate {
		t.Fatalf("force = %+v %v", p, err)
	}
	if _, err := e.svc.UpdatePort(e.ctx, e.userID, inst.ID, 8888, str("public"), nil, ""); !errors.Is(err, apperr.ErrPortForcedPrivate) {
		t.Fatalf("user re-public = %v", err)
	}
	if _, err := e.svc.UpdatePort(e.ctx, e.userID, inst.ID, 8888, nil, str("still mine"), ""); err != nil { // 改备注仍允许
		t.Fatal(err)
	}
	if err := e.svc.AdminDeletePort(e.ctx, actor, inst.ID, 8888, "abuse"); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.svc.ListPorts(e.ctx, inst.ID); len(list) != 0 {
		t.Fatalf("list = %+v", list)
	}
	var kinds []string
	rows, _ := e.pool.Query(e.ctx, `SELECT action FROM audit_logs WHERE actor_type = 'admin' AND actor_id = 3 ORDER BY id`)
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		kinds = append(kinds, a)
	}
	rows.Close()
	if len(kinds) != 2 || kinds[0] != "instance.port_force_private" || kinds[1] != "instance.port_delete" {
		t.Fatalf("audit = %v", kinds)
	}
}

func TestIngressRoutesAndMigrateCopy(t *testing.T) {
	e := newEnv(t, 2)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8888, "public", "lab", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 7860, "private", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminForcePortPrivate(e.ctx, audit.Actor{AdminID: 1}, inst.ID, 8888, "x"); err != nil {
		t.Fatal(err)
	}
	routes, err := e.svc.ListIngressRoutes(e.ctx)
	if err != nil || len(routes) != 2 {
		t.Fatalf("routes = %+v %v", routes, err)
	}
	r := routes[0]
	if r.InstanceID != inst.ID || r.UserID != e.userID || r.Port != 7860 || r.Visibility != "private" || r.State != rental.StateRunning || r.Suspended ||
		r.LanIP != "10.0.0.10" || r.HostPubkey == "" {
		t.Fatalf("route = %+v", r)
	}
	moved, err := e.svc.AdminMigrate(e.ctx, audit.Actor{AdminID: 1}, inst.ID, "move")
	if err != nil {
		t.Fatal(err)
	}
	copied, _ := e.svc.ListPorts(e.ctx, moved.ID)
	if len(copied) != 2 || copied[1].Port != 8888 || !copied[1].ForcedPrivate || copied[0].Note != "" || copied[1].Note != "lab" {
		t.Fatalf("copied = %+v", copied)
	}
	e.ack(true) // 旧实例 wipe → released，新实例 create → running
	routes, _ = e.svc.ListIngressRoutes(e.ctx)
	for _, r := range routes {
		if r.InstanceID == inst.ID {
			t.Fatalf("released instance still routed: %+v", r)
		}
	}
	if len(routes) != 2 || routes[0].InstanceID != moved.ID {
		t.Fatalf("routes after migrate = %+v", routes)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'TestPort|TestIngressRoutes' 2>&1 | head`
Expected: 编译错误。

- [ ] **Step 3: 实现 `ports.go`**

```go
package rental

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// MaxPortsPerInstance 每个实例最多开放的 Web 端口数。
const MaxPortsPerInstance = 5

type Port struct {
	InstanceID    string    `json:"-"`
	Port          int       `json:"port"`
	Visibility    string    `json:"visibility"` // private / public
	ForcedPrivate bool      `json:"forced_private"`
	Note          string    `json:"note"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

const portCols = `instance_id, port, visibility, forced_private, note, created_at, updated_at`

func scanPort(row pgx.Row) (Port, error) {
	var p Port
	err := row.Scan(&p.InstanceID, &p.Port, &p.Visibility, &p.ForcedPrivate, &p.Note, &p.CreatedAt, &p.UpdatedAt)
	if db.IsNoRows(err) {
		return Port{}, apperr.ErrNotFound
	}
	return p, err
}

func validPort(port int) bool { return port >= 1 && port <= 65535 && port != 22 }

func validVisibility(v string) bool { return v == "private" || v == "public" }

func checkNote(note string) error {
	if utf8.RuneCountInString(note) > 32 {
		return apperr.BadRequest("备注不能超过 32 个字符")
	}
	return nil
}

func (s *Service) ListPorts(ctx context.Context, instanceID string) ([]Port, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT `+portCols+` FROM instance_ports WHERE instance_id = $1 ORDER BY port`, instanceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Port, error) { return scanPort(r) })
}

// lockOwned 锁实例行并校验归属；released / failed 不允许管理端口。
func lockOwned(ctx context.Context, tx pgx.Tx, userID int64, id string) (Instance, error) {
	inst, err := lockInstance(ctx, tx, id)
	if err != nil {
		return Instance{}, err
	}
	if inst.UserID != userID {
		return Instance{}, apperr.ErrNotFound
	}
	if inst.State == StateReleased || inst.State == StateFailed {
		return Instance{}, apperr.ErrInvalidState
	}
	return inst, nil
}

func (s *Service) AddPort(ctx context.Context, userID int64, id string, port int, visibility, note, ip string) (Port, error) {
	if !validPort(port) {
		return Port{}, apperr.ErrPortInvalid
	}
	if visibility == "" {
		visibility = "private"
	}
	if !validVisibility(visibility) {
		return Port{}, apperr.BadRequest("可见性只能是 private 或 public")
	}
	note = strings.TrimSpace(note)
	if err := checkNote(note); err != nil {
		return Port{}, err
	}
	now := s.d.Clock.Now()
	var out Port
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockOwned(ctx, tx, userID, id); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM instance_ports WHERE instance_id = $1`, id).Scan(&n); err != nil {
			return err
		}
		if n >= MaxPortsPerInstance {
			return apperr.ErrPortLimit
		}
		p, err := scanPort(tx.QueryRow(ctx, `INSERT INTO instance_ports (instance_id, port, visibility, note, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $5) RETURNING `+portCols, id, port, visibility, note, now))
		if db.IsUniqueViolation(err) {
			return apperr.ErrPortExists
		}
		if err != nil {
			return err
		}
		out = p
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.port_add", "instance", id,
			map[string]any{"port": port, "visibility": visibility, "note": note}))
	})
	return out, err
}

// UpdatePort 修改可见性 / 备注（nil 表示不改）；forced_private 的端口不能改回 public。
func (s *Service) UpdatePort(ctx context.Context, userID int64, id string, port int, visibility, note *string, ip string) (Port, error) {
	if visibility != nil && !validVisibility(*visibility) {
		return Port{}, apperr.BadRequest("可见性只能是 private 或 public")
	}
	if note != nil {
		trimmed := strings.TrimSpace(*note)
		note = &trimmed
		if err := checkNote(trimmed); err != nil {
			return Port{}, err
		}
	}
	now := s.d.Clock.Now()
	var out Port
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockOwned(ctx, tx, userID, id); err != nil {
			return err
		}
		cur, err := scanPort(tx.QueryRow(ctx, `SELECT `+portCols+` FROM instance_ports WHERE instance_id = $1 AND port = $2 FOR UPDATE`, id, port))
		if err != nil {
			return err
		}
		if visibility != nil && *visibility == "public" && cur.ForcedPrivate {
			return apperr.ErrPortForcedPrivate
		}
		if visibility != nil {
			cur.Visibility = *visibility
		}
		if note != nil {
			cur.Note = *note
		}
		out, err = scanPort(tx.QueryRow(ctx, `UPDATE instance_ports SET visibility = $3, note = $4, updated_at = $5 WHERE instance_id = $1 AND port = $2 RETURNING `+portCols,
			id, port, cur.Visibility, cur.Note, now))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.port_update", "instance", id,
			map[string]any{"port": port, "visibility": cur.Visibility, "note": cur.Note}))
	})
	return out, err
}

func (s *Service) DeletePort(ctx context.Context, userID int64, id string, port int, ip string) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockOwned(ctx, tx, userID, id); err != nil {
			return err
		}
		if _, err := scanPort(tx.QueryRow(ctx, `DELETE FROM instance_ports WHERE instance_id = $1 AND port = $2 RETURNING `+portCols, id, port)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.port_delete", "instance", id, map[string]any{"port": port}))
	})
}

// AdminForcePortPrivate 后台处置：设为私有并禁止用户改回公开。
func (s *Service) AdminForcePortPrivate(ctx context.Context, actor audit.Actor, id string, port int, reason string) (Port, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Port{}, apperr.BadRequest("请填写原因")
	}
	now := s.d.Clock.Now()
	var out Port
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockInstance(ctx, tx, id); err != nil {
			return err
		}
		p, err := scanPort(tx.QueryRow(ctx, `UPDATE instance_ports SET visibility = 'private', forced_private = true, updated_at = $3
			WHERE instance_id = $1 AND port = $2 RETURNING `+portCols, id, port, now))
		if err != nil {
			return err
		}
		out = p
		return audit.Record(ctx, tx, now, actor.Entry("instance.port_force_private", "instance", id, map[string]any{"port": port, "reason": reason}))
	})
	return out, err
}

func (s *Service) AdminDeletePort(ctx context.Context, actor audit.Actor, id string, port int, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return apperr.BadRequest("请填写原因")
	}
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockInstance(ctx, tx, id); err != nil {
			return err
		}
		if _, err := scanPort(tx.QueryRow(ctx, `DELETE FROM instance_ports WHERE instance_id = $1 AND port = $2 RETURNING `+portCols, id, port)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("instance.port_delete", "instance", id, map[string]any{"port": port, "reason": reason}))
	})
}

// IngressRoute 是 Web 入口路由表的一行（非终态实例的每个端口）。
type IngressRoute struct {
	InstanceID string
	UserID     int64
	Port       int
	Visibility string
	State      string
	Suspended  bool
	LanIP      string
	HostPubkey string
}

func (s *Service) ListIngressRoutes(ctx context.Context) ([]IngressRoute, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT p.instance_id, i.user_id, p.port, p.visibility, i.state, i.suspended_at IS NOT NULL, m.lan_ip, i.ssh_host_pubkey
		FROM instance_ports p JOIN instances i ON i.id = p.instance_id JOIN machines m ON m.id = i.machine_id
		WHERE i.state IN ('creating', 'running', 'stopped') ORDER BY p.instance_id, p.port`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IngressRoute, error) {
		var x IngressRoute
		err := r.Scan(&x.InstanceID, &x.UserID, &x.Port, &x.Visibility, &x.State, &x.Suspended, &x.LanIP, &x.HostPubkey)
		return x, err
	})
}
```

`internal/rental/migrate.go` 在 `openLease(ctx, tx, moved, now)` 之后加：

```go
		if _, err := tx.Exec(ctx, `INSERT INTO instance_ports (instance_id, port, visibility, forced_private, note, created_at, updated_at)
			SELECT $2, port, visibility, forced_private, note, $3, $3 FROM instance_ports WHERE instance_id = $1 ORDER BY port`, old.ID, id, now); err != nil {
			return err
		}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ 2>&1 | tail -3`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/rental/ports.go internal/rental/ports_test.go internal/rental/migrate.go
git commit -m "feat(rental): instance web ports (CRUD, limit 5, forced private, ingress routes, copied on migrate)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 5: 配置项与校验、README 配置表

**Files:**
- Modify: `internal/platform/config/config.go`、`internal/platform/config/config_test.go`
- Modify: `README.md`（配置表追加行）

**Interfaces:**
- Produces（`config.Config` 新字段，全部由 `loadAccess` 读取）：
  - `SSHAddr string`（`SPARK_SSH_ADDR`，默认 `:2222`，须 host:port，不得与 `HTTPAddr` / `AdminAddr` / `AgentAddr` 相同）
  - `SSHPublicPort int`（`SPARK_SSH_PUBLIC_PORT`，默认 22，1～65535）
  - `SSHMaxSessions int`（`SPARK_SSH_MAX_SESSIONS_PER_INSTANCE`，默认 16，≥ 1）
  - `SSHDrainTimeout time.Duration`（`SPARK_SSH_DRAIN_TIMEOUT`，默认 30s，> 0）
  - `UpstreamSSHPort int`（`SPARK_UPSTREAM_SSH_PORT`，默认 2222）
  - `IngressDomain string`（`SPARK_INGRESS_DOMAIN`，默认 `= Domain`；合法域名；不得是 `Domain` 的子域）
  - `AdminPublicBaseURL string`（`SPARK_ADMIN_PUBLIC_BASE_URL`，默认空；非空须以 http(s):// 开头，去尾部 `/`）
  - `PublicBaseURL`：dev 默认 `http://<Domain>:<HTTPAddr 端口>`；任何环境非空时 `host == Domain`（忽略端口），prod 必须 `https://`
  - `TLSCertFile, TLSKeyFile string`（必须同时设置或同时为空）；`HTTPRedirectAddr string`（仅 TLS 开启时允许非空）
  - prod 且 `TLSCertFile == ""` 且 `!TrustProxy` → 错误「生产环境必须配置 TLS 证书或在反向代理后设置 SPARK_TRUST_PROXY=true」
  - `func (c Config) TLSEnabled() bool`；`func (c Config) PublicURL() *url.URL`（解析 `PublicBaseURL`）；`func (c Config) SSHHost() string`（`"ssh." + Domain`）；`func (c Config) FillAccessDefaults() Config`（给未经 `Load` 的字面量补接入层默认值：`app.Build` 首行调用，使 `internal/app/e2e_test.go` 与 e2e-server 的 `config.Config{…}` 字面量不必逐个填新字段）

- [ ] **Step 1: 写失败的测试**

`internal/platform/config/config_test.go` 追加：

```go
func TestLoadAccessDefaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SSHAddr != ":2222" || c.SSHPublicPort != 22 || c.SSHMaxSessions != 16 || c.SSHDrainTimeout != 30*time.Second || c.UpstreamSSHPort != 2222 ||
		c.IngressDomain != "spark.local" || c.AdminPublicBaseURL != "" || c.PublicBaseURL != "http://spark.local:8080" || c.TLSEnabled() || c.HTTPRedirectAddr != "" {
		t.Fatalf("access defaults = %+v", c)
	}
	if c.PublicURL().Host != "spark.local:8080" || c.SSHHost() != "ssh.spark.local" {
		t.Fatalf("derived = %s %s", c.PublicURL(), c.SSHHost())
	}
	// 字面量（测试 / e2e-server）补默认值
	lit := config.Config{Domain: "spark.test"}.FillAccessDefaults()
	if lit.UpstreamSSHPort != 2222 || lit.IngressDomain != "spark.test" || lit.PublicBaseURL != "http://spark.test:8080" || lit.SSHMaxSessions != 16 || lit.SSHDrainTimeout != 30*time.Second {
		t.Fatalf("literal defaults = %+v", lit)
	}
}

func TestLoadAccessValues(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey,
		"SPARK_DOMAIN": "spark.example", "SPARK_PUBLIC_BASE_URL": "https://spark.example/", "SPARK_INGRESS_DOMAIN": "sparkapp.example",
		"SPARK_SSH_ADDR": "0.0.0.0:2200", "SPARK_SSH_PUBLIC_PORT": "2200", "SPARK_SSH_MAX_SESSIONS_PER_INSTANCE": "4", "SPARK_SSH_DRAIN_TIMEOUT": "5s",
		"SPARK_UPSTREAM_SSH_PORT": "2223", "SPARK_ADMIN_PUBLIC_BASE_URL": "https://admin.spark.example/", "SPARK_TLS_CERT_FILE": "/etc/spark/tls.crt",
		"SPARK_TLS_KEY_FILE": "/etc/spark/tls.key", "SPARK_HTTP_REDIRECT_ADDR": ":8080"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SSHAddr != "0.0.0.0:2200" || c.SSHPublicPort != 2200 || c.SSHMaxSessions != 4 || c.SSHDrainTimeout != 5*time.Second || c.UpstreamSSHPort != 2223 ||
		c.IngressDomain != "sparkapp.example" || c.AdminPublicBaseURL != "https://admin.spark.example" || c.PublicBaseURL != "https://spark.example" ||
		!c.TLSEnabled() || c.HTTPRedirectAddr != ":8080" {
		t.Fatalf("access values = %+v", c)
	}
}

func TestLoadAccessErrors(t *testing.T) {
	base := func() map[string]string { return map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey} }
	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"ssh addr no port", map[string]string{"SPARK_SSH_ADDR": "2222"}, "SPARK_SSH_ADDR"},
		{"ssh addr equals http", map[string]string{"SPARK_SSH_ADDR": ":8080"}, "SPARK_SSH_ADDR"},
		{"public port 0", map[string]string{"SPARK_SSH_PUBLIC_PORT": "0"}, "SPARK_SSH_PUBLIC_PORT"},
		{"max sessions 0", map[string]string{"SPARK_SSH_MAX_SESSIONS_PER_INSTANCE": "0"}, "SPARK_SSH_MAX_SESSIONS_PER_INSTANCE"},
		{"drain bad", map[string]string{"SPARK_SSH_DRAIN_TIMEOUT": "soon"}, "SPARK_SSH_DRAIN_TIMEOUT"},
		{"upstream port", map[string]string{"SPARK_UPSTREAM_SSH_PORT": "99999"}, "SPARK_UPSTREAM_SSH_PORT"},
		{"ingress subdomain", map[string]string{"SPARK_INGRESS_DOMAIN": "app.spark.local"}, "SPARK_INGRESS_DOMAIN"},
		{"ingress invalid", map[string]string{"SPARK_INGRESS_DOMAIN": "bad domain"}, "SPARK_INGRESS_DOMAIN"},
		{"base url host mismatch", map[string]string{"SPARK_PUBLIC_BASE_URL": "http://other.local:8080"}, "SPARK_PUBLIC_BASE_URL"},
		{"base url no scheme", map[string]string{"SPARK_PUBLIC_BASE_URL": "spark.local"}, "SPARK_PUBLIC_BASE_URL"},
		{"admin url bad", map[string]string{"SPARK_ADMIN_PUBLIC_BASE_URL": "admin.local"}, "SPARK_ADMIN_PUBLIC_BASE_URL"},
		{"tls half", map[string]string{"SPARK_TLS_CERT_FILE": "/a.crt"}, "SPARK_TLS_KEY_FILE"},
		{"redirect without tls", map[string]string{"SPARK_HTTP_REDIRECT_ADDR": ":80"}, "SPARK_HTTP_REDIRECT_ADDR"},
	}
	for _, c := range cases {
		m := base()
		for k, v := range c.set {
			m[k] = v
		}
		if _, err := config.Load(env(m)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %s", c.name, err, c.want)
		}
	}
	// prod：无证书且不在代理后 → 拒绝；prodEnv 带 SPARK_TRUST_PROXY=true → 通过；base url 必须 https
	p := prodEnv()
	delete(p, "SPARK_TRUST_PROXY")
	if _, err := config.Load(env(p)); err == nil || !strings.Contains(err.Error(), "SPARK_TRUST_PROXY") {
		t.Fatalf("prod without tls = %v", err)
	}
	p = prodEnv()
	if _, err := config.Load(env(p)); err != nil {
		t.Fatalf("prod behind proxy = %v", err)
	}
	p["SPARK_PUBLIC_BASE_URL"] = "http://spark.example.com"
	if _, err := config.Load(env(p)); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("prod http base = %v", err)
	}
}
```

`prodEnv()` 的 map 增加 `"SPARK_DOMAIN": "spark.example.com", "SPARK_TRUST_PROXY": "true"`（现有的 `SPARK_PUBLIC_BASE_URL` 是 `https://spark.example.com/`，host 必须与域名一致；prod 没有证书时必须声明在代理后）。`config_test.go` import 追加 `"time"`（`strings` 已有）。现有 `TestLoadProdFullyConfigured`、`TestLoadPluginErrors`、`TestLoadPayRealWithOnlyAlipay` 用 `prodEnv()`，改完后照常通过。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/platform/config/ 2>&1 | head`
Expected: 编译错误。

- [ ] **Step 3: 实现**

`internal/platform/config/config.go` 的 `Config` 追加：

```go
	// 计划 3：接入层
	SSHAddr            string        // SSH 网关监听地址
	SSHPublicPort      int           // 写进 ssh_command 的公网端口
	SSHMaxSessions     int           // 每实例并发 SSH 会话上限
	SSHDrainTimeout    time.Duration // 停机时等待会话结束
	UpstreamSSHPort    int           // 容器 sshd 在机器上的端口（Create 的 ssh_port 与上游拨号共用）
	IngressDomain      string        // 租户子域的父域：ins-<id>-<port>.<IngressDomain>
	AdminPublicBaseURL string        // 后台对外地址（经反向代理暴露时），用于写请求的 Origin 校验；空表示不比对
	TLSCertFile        string
	TLSKeyFile         string
	HTTPRedirectAddr   string // 纯 HTTP 301 到 https 的监听地址；仅 TLS 开启时有效
```

`Load` 里把 `PublicBaseURL: strings.TrimRight(getenv("SPARK_PUBLIC_BASE_URL"), "/")` 保留，并在 `c.loadAgent` 之后、`c.checkPlugins()` 之前加 `if err := c.loadAccess(getenv, or); err != nil { return Config{}, err }`。`checkPlugins` 里对 `PayDriver == "real"` 的 https 校验保留。文件末尾追加：

```go
// loadAccess 读取并校验接入层（SSH 网关 / Web 入口 / TLS）配置。
func (c *Config) loadAccess(getenv func(string) string, or func(key, def string) string) error {
	var err error
	c.SSHAddr = or("SPARK_SSH_ADDR", ":2222")
	if _, _, err := net.SplitHostPort(c.SSHAddr); err != nil {
		return fmt.Errorf("SPARK_SSH_ADDR 必须是 host:port 形式，当前为 %q", c.SSHAddr)
	}
	for _, other := range []string{c.HTTPAddr, c.AdminAddr, c.AgentAddr} {
		if samePort(c.SSHAddr, other) {
			return fmt.Errorf("SPARK_SSH_ADDR %q 与其它监听地址冲突", c.SSHAddr)
		}
	}
	if c.SSHPublicPort, err = portValue("SPARK_SSH_PUBLIC_PORT", or("SPARK_SSH_PUBLIC_PORT", "22")); err != nil {
		return err
	}
	if c.SSHMaxSessions, err = positiveInt("SPARK_SSH_MAX_SESSIONS_PER_INSTANCE", or("SPARK_SSH_MAX_SESSIONS_PER_INSTANCE", "16")); err != nil {
		return err
	}
	c.SSHDrainTimeout = 30 * time.Second
	if v := getenv("SPARK_SSH_DRAIN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("SPARK_SSH_DRAIN_TIMEOUT 格式不正确：%q", v)
		}
		c.SSHDrainTimeout = d
	}
	if c.UpstreamSSHPort, err = portValue("SPARK_UPSTREAM_SSH_PORT", or("SPARK_UPSTREAM_SSH_PORT", "2222")); err != nil {
		return err
	}
	c.IngressDomain = strings.ToLower(or("SPARK_INGRESS_DOMAIN", c.Domain))
	if net.ParseIP(c.IngressDomain) != nil || !validHost(c.IngressDomain) {
		return fmt.Errorf("SPARK_INGRESS_DOMAIN 不是合法域名：%q", c.IngressDomain)
	}
	if c.IngressDomain != strings.ToLower(c.Domain) && strings.HasSuffix(c.IngressDomain, "."+strings.ToLower(c.Domain)) {
		return fmt.Errorf("SPARK_INGRESS_DOMAIN %q 不能是 SPARK_DOMAIN 的子域（请用不同的注册域，例如 sparkapp.example）", c.IngressDomain)
	}
	if c.PublicBaseURL == "" {
		_, port, _ := net.SplitHostPort(c.HTTPAddr)
		c.PublicBaseURL = "http://" + net.JoinHostPort(c.Domain, port)
	}
	u, err := url.Parse(c.PublicBaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return fmt.Errorf("SPARK_PUBLIC_BASE_URL 必须形如 https://域名[:端口]，当前为 %q", c.PublicBaseURL)
	}
	if !strings.EqualFold(u.Hostname(), c.Domain) {
		return fmt.Errorf("SPARK_PUBLIC_BASE_URL 的域名 %q 必须等于 SPARK_DOMAIN %q", u.Hostname(), c.Domain)
	}
	if c.Env == "prod" && u.Scheme != "https" {
		return errors.New("生产环境的 SPARK_PUBLIC_BASE_URL 必须以 https:// 开头")
	}
	c.AdminPublicBaseURL = strings.TrimRight(getenv("SPARK_ADMIN_PUBLIC_BASE_URL"), "/")
	if c.AdminPublicBaseURL != "" {
		au, err := url.Parse(c.AdminPublicBaseURL)
		if err != nil || (au.Scheme != "http" && au.Scheme != "https") || au.Hostname() == "" {
			return fmt.Errorf("SPARK_ADMIN_PUBLIC_BASE_URL 必须形如 https://域名[:端口]，当前为 %q", c.AdminPublicBaseURL)
		}
	}
	c.TLSCertFile, c.TLSKeyFile = getenv("SPARK_TLS_CERT_FILE"), getenv("SPARK_TLS_KEY_FILE")
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("SPARK_TLS_CERT_FILE 与 SPARK_TLS_KEY_FILE 必须同时设置")
	}
	c.HTTPRedirectAddr = getenv("SPARK_HTTP_REDIRECT_ADDR")
	if c.HTTPRedirectAddr != "" {
		if !c.TLSEnabled() {
			return errors.New("SPARK_HTTP_REDIRECT_ADDR 只在配置了 TLS 证书时有效")
		}
		if _, _, err := net.SplitHostPort(c.HTTPRedirectAddr); err != nil {
			return fmt.Errorf("SPARK_HTTP_REDIRECT_ADDR 必须是 host:port 形式，当前为 %q", c.HTTPRedirectAddr)
		}
	}
	if c.Env == "prod" && !c.TLSEnabled() && !c.TrustProxy {
		return errors.New("生产环境必须配置 TLS 证书或在反向代理后设置 SPARK_TRUST_PROXY=true")
	}
	return nil
}

// TLSEnabled 报告公网监听器是否由 platform 自己终止 TLS。
func (c Config) TLSEnabled() bool { return c.TLSCertFile != "" }

// FillAccessDefaults 给没有经过 Load 的 Config 字面量（测试、e2e-server）补上接入层零值字段的默认值；Load 之后调用无副作用。
func (c Config) FillAccessDefaults() Config {
	if c.SSHAddr == "" {
		c.SSHAddr = ":2222"
	}
	if c.SSHPublicPort == 0 {
		c.SSHPublicPort = 22
	}
	if c.SSHMaxSessions == 0 {
		c.SSHMaxSessions = 16
	}
	if c.SSHDrainTimeout == 0 {
		c.SSHDrainTimeout = 30 * time.Second
	}
	if c.UpstreamSSHPort == 0 {
		c.UpstreamSSHPort = 2222
	}
	if c.IngressDomain == "" {
		c.IngressDomain = strings.ToLower(c.Domain)
	}
	if c.PublicBaseURL == "" {
		_, port, _ := net.SplitHostPort(c.HTTPAddr)
		if port == "" {
			port = "8080"
		}
		c.PublicBaseURL = "http://" + net.JoinHostPort(c.Domain, port)
	}
	return c
}

// PublicURL 返回已校验的对外地址（Load 之后一定可解析）。
func (c Config) PublicURL() *url.URL {
	u, _ := url.Parse(c.PublicBaseURL)
	return u
}

// SSHHost 是用户 SSH 命令里的主机名。
func (c Config) SSHHost() string { return "ssh." + c.Domain }

func portValue(name, v string) (int, error) {
	n, err := positiveInt(name, v)
	if err != nil || n > 65535 {
		return 0, fmt.Errorf("%s 必须是 1～65535 的端口号，当前为 %q", name, v)
	}
	return n, nil
}

// samePort 判断两个监听地址是否会抢同一个端口（host 为空、0.0.0.0 或 :: 视为所有地址）。
func samePort(a, b string) bool {
	ha, pa, err1 := net.SplitHostPort(a)
	hb, pb, err2 := net.SplitHostPort(b)
	if err1 != nil || err2 != nil || pa != pb {
		return false
	}
	any := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }
	return any(ha) || any(hb) || ha == hb
}
```

import 追加 `"net/url"`。注意 `loadAccess` 必须在 `loadAgent` 之后（要用 `c.AgentAddr`），在 `checkPlugins` 之前（`PayDriver=real` 的 https 检查对 dev 默认值仍然成立：mock 支付不检查）。

- [ ] **Step 4: README 配置表追加**

`README.md` 配置表最后追加：

```markdown
| `SPARK_SSH_ADDR` | SSH 网关监听地址（compose 把公网 22 映射到它） | :2222 |
| `SPARK_SSH_PUBLIC_PORT` | 写进用户 SSH 命令的公网端口；非 22 时命令带 `-p` | 22 |
| `SPARK_SSH_MAX_SESSIONS_PER_INSTANCE` / `SPARK_SSH_DRAIN_TIMEOUT` | 每实例并发 SSH 会话上限 / 停机时等待会话结束的时间 | 16 / 30s |
| `SPARK_UPSTREAM_SSH_PORT` | 容器 sshd 在机器上的端口（Create 指令的 `ssh_port` 与网关上游拨号共用） | 2222 |
| `SPARK_INGRESS_DOMAIN` | 租户 Web 子域的父域（`ins-<id>-<port>.<此域>`）；生产推荐用与 `SPARK_DOMAIN` **不同的注册域（eTLD+1）**，子域不算；「不是 SPARK_DOMAIN 子域」的启动校验只防误配 | = SPARK_DOMAIN |
| `SPARK_PUBLIC_BASE_URL` | 对外地址；域名必须等于 `SPARK_DOMAIN`；prod 必填且 https；dev 默认 `http://spark.local:8080`。本地测 ingress 需在 hosts 加 `spark.local` 与用到的 `ins-<id>-<port>.spark.local`（hosts 不支持泛域名），或改用 sslip.io 域 | 见左 |
| `SPARK_ADMIN_PUBLIC_BASE_URL` | 后台对外地址（经反向代理暴露时填写），用于后台写请求的 Origin 校验 | 空 |
| `SPARK_TLS_CERT_FILE` / `SPARK_TLS_KEY_FILE` | 证书与私钥（PEM，覆盖 `<domain>` 与 `*.<ingress_domain>`）；设置后公网监听器为 HTTPS 并每分钟检查文件变化热加载 | 空 |
| `SPARK_HTTP_REDIRECT_ADDR` | 纯 HTTP 301 到 https 的监听地址（仅 TLS 开启时有效） | 空 |
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/platform/config/ ./internal/app/ ./cmd/... 2>&1 | tail -5`
Expected: `ok`。（`cmd/e2e-server` 构造的 `config.Config` 字面量不经过 `Load`，Task 22 再补齐新字段。）

- [ ] **Step 6: 提交**

```bash
git add internal/platform/config README.md
git commit -m "feat(config): access layer settings (ssh gateway, ingress domain, public base url, tls files) with validation

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 6: `pki` 网关主机密钥（存库）、`platform admin ssh-host-key`

**Files:**
- Modify: `internal/pki/sshhost.go`（追加）、`internal/pki/sshhost_test.go`（新建）
- Modify: `cmd/platform/main.go:363-387`、`cmd/platform/admin.go:493-496`、`cmd/platform/admin_test.go`

**Interfaces:**
- Produces:
  - `pki.EnsureSSHHostKey(ctx, pool *pgxpool.Pool, box *secretbox.Box) (ssh.Signer, error)`：读 `platform_pki.ssh_host_key_enc`；为 NULL 时生成 ed25519，`UPDATE platform_pki SET ssh_host_key_enc = $1 WHERE id = 1 AND ssh_host_key_enc IS NULL`，再重读（并发只留一把）。要求 `platform_pki` 行已存在（`EnsureCA` 先行），否则返回错误「内置 CA 尚未初始化」。
  - `pki.SSHFingerprint(pub ssh.PublicKey) string`（`ssh.FingerprintSHA256`）。
  - CLI：`platform admin ssh-host-key`（需要数据库）打印 `公钥：ssh-ed25519 AAAA…` 与 `指纹：SHA256:…`。

- [ ] **Step 1: 写失败的测试**

`internal/pki/sshhost_test.go`：

```go
package pki_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
)

func TestGenerateInstanceHostKey(t *testing.T) {
	pub, pem, err := pki.GenerateInstanceHostKey()
	if err != nil || !strings.HasPrefix(pub, "ssh-ed25519 ") || strings.Contains(pub, "\n") || !strings.Contains(string(pem), "OPENSSH PRIVATE KEY") {
		t.Fatalf("pub=%q pem=%q err=%v", pub, pem, err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil || strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) != pub {
		t.Fatalf("pem/pub mismatch: %v", err)
	}
}

func TestEnsureSSHHostKeyPersistsAndIsUnique(t *testing.T) {
	pool := dbtest.New(t)
	box, _ := secretbox.New(dbtest.Key)
	ctx := context.Background()
	if _, err := pki.EnsureSSHHostKey(ctx, pool, box); err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("before CA: %v", err)
	}
	if _, err := pki.EnsureCA(ctx, pool, box, clock.NewFake(dbtest.T0)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	fps := make([]string, 8)
	for i := range fps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := pki.EnsureSSHHostKey(ctx, pool, box)
			if err != nil {
				t.Error(err)
				return
			}
			fps[i] = pki.SSHFingerprint(s.PublicKey())
		}(i)
	}
	wg.Wait()
	for _, fp := range fps[1:] {
		if fp != fps[0] || !strings.HasPrefix(fp, "SHA256:") {
			t.Fatalf("fingerprints differ: %v", fps)
		}
	}
	again, _ := pki.EnsureSSHHostKey(ctx, pool, box)
	if pki.SSHFingerprint(again.PublicKey()) != fps[0] || again.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Fatal("reload returned a different key")
	}
}
```

`cmd/platform/admin_test.go` 追加（该文件已有 `bytes`、`context`、`strings`；追加 import `"spark-cloud/internal/pki"`、`"spark-cloud/internal/platform/clock"`、`"spark-cloud/internal/platform/dbtest"`、`"spark-cloud/internal/platform/secretbox"`；若文件尚无 `TestMain`，加 `func TestMain(m *testing.M) { dbtest.Main(m) }`）：

```go
func TestAdminSSHHostKey(t *testing.T) {
	pool := dbtest.New(t)
	box, _ := secretbox.New(dbtest.Key)
	if _, err := pki.EnsureCA(context.Background(), pool, box, clock.NewFake(dbtest.T0)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := adminSSHHostKey(context.Background(), pool, box, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "公钥：ssh-ed25519 ") || !strings.Contains(out.String(), "指纹：SHA256:") {
		t.Fatalf("output = %s", out.String())
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/pki/ ./cmd/platform/ 2>&1 | head`
Expected: 编译错误。

- [ ] **Step 3: 实现**

`internal/pki/sshhost.go` 追加：

```go
// EnsureSSHHostKey 读取或首次生成 SSH 网关的主机密钥（ed25519），secretbox 加密存 platform_pki.ssh_host_key_enc；
// 多副本并发首次启动时靠「只在 NULL 时写入」保证只有一把。platform_pki 行由 EnsureCA 创建，必须先行。
func EnsureSSHHostKey(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box) (ssh.Signer, error) {
	load := func() (ssh.Signer, bool, error) {
		var enc []byte
		err := pool.QueryRow(ctx, `SELECT ssh_host_key_enc FROM platform_pki WHERE id = 1`).Scan(&enc)
		if db.IsNoRows(err) {
			return nil, false, errors.New("pki: 内置 CA 尚未初始化（platform_pki 为空）")
		}
		if err != nil {
			return nil, false, err
		}
		if enc == nil {
			return nil, false, nil
		}
		raw, err := box.Open(enc)
		if err != nil {
			return nil, false, fmt.Errorf("解密 SSH 网关主机密钥失败（SPARK_SECRET_KEY 是否被更换？）: %w", err)
		}
		s, err := ssh.ParsePrivateKey(raw)
		return s, true, err
	}
	if s, ok, err := load(); err != nil || ok {
		return s, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "spark-cloud ssh gateway")
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `UPDATE platform_pki SET ssh_host_key_enc = $1 WHERE id = 1 AND ssh_host_key_enc IS NULL`, box.Seal(pem.EncodeToMemory(block))); err != nil {
		return nil, err
	}
	s, ok, err := load()
	if err == nil && !ok {
		return nil, errors.New("pki: ssh host key not persisted")
	}
	return s, err
}

// SSHFingerprint 返回 OpenSSH 风格的 SHA256 指纹。
func SSHFingerprint(pub ssh.PublicKey) string { return ssh.FingerprintSHA256(pub) }
```

`sshhost.go` 的 import 补齐为：

```go
import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
)
```

`cmd/platform/admin.go`：`adminUsage` 增加一行 `  platform admin ssh-host-key`，并追加：

```go
// adminSSHHostKey 打印 SSH 网关主机公钥与指纹（写进帮助页「首次连接请核对指纹」）。
func adminSSHHostKey(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box, out io.Writer) error {
	if _, err := pki.EnsureCA(ctx, pool, box, clock.Real{}); err != nil {
		return err
	}
	signer, err := pki.EnsureSSHHostKey(ctx, pool, box)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "公钥：%s\n指纹：%s\n", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), pki.SSHFingerprint(signer.PublicKey()))
	return nil
}
```

（import 追加 `"github.com/jackc/pgx/v5/pgxpool"`、`"golang.org/x/crypto/ssh"`、`"spark-cloud/internal/platform/clock"`、`"spark-cloud/internal/platform/secretbox"`。）

`cmd/platform/main.go` 的 `adminMain` 在 `svc := admin.NewService(...)` 之前加：

```go
	if len(args) > 0 && args[0] == "ssh-host-key" {
		return adminSSHHostKey(ctx, pool, box, os.Stdout)
	}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/pki/ ./cmd/platform/ 2>&1 | tail -3`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/pki cmd/platform
git commit -m "feat(pki): ssh gateway host key stored in platform_pki; platform admin ssh-host-key

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 7: `internal/access/sshtest`：进程内假「容器 sshd」

**Files:**
- Create: `internal/access/sshtest/server.go`、`internal/access/sshtest/server_test.go`

**Interfaces:**
- Produces（单元 / 集成 / e2e-server 共用）：
  - `sshtest.Options{AuthorizedKey ssh.PublicKey; HostKey ssh.Signer}`（`HostKey` nil → 自动生成 ed25519）。
  - `sshtest.New(o Options) (*Server, error)`：在 `127.0.0.1:0` 监听；`(*Server).Addr() string`、`Port() int`、`Signer() ssh.Signer`、`HostPubLine() string`（authorized_keys 一行）、`Close()`。
  - 行为：只接受 `root` + `AuthorizedKey` 的公钥认证。`session`：`exec echo X` → 输出 `X\n` 退出 0；`exec exit N` → 退出 N；`exec cat` → 回显 stdin 直到 EOF；`exec stderr X` → 把 X 写到 stderr；`exec agent` → 若收到过 `auth-agent-req@openssh.com`，向客户端打开 `auth-agent@openssh.com` 通道并 `agent.NewClient(ch).List()`，输出 `agent:<数量>\n`，否则 `agent:none\n`；`exec hostkeys` → 向客户端发全局请求 `hostkeys-00@openssh.com`（不要求回复）后输出 `sent\n`；其它 `exec` → `exec:<cmd>\n`；`shell` → 每读一行回 `shell:<line>\n`；`subsystem sftp` → 输出 `SFTP-FAKE\n` 退出 0；`pty-req` / `env` / `window-change` / `auth-agent-req@openssh.com` 回 true，其它请求回 false。`direct-tcpip`：记录 extra data（`LastDirectTCPIP() []byte`），按目标端口查 `SetTarget(port, addr)` 登记的地址拨号并转发，未登记 → `Reject(ConnectionFailed)`。全局请求：`tcpip-forward`（监听 `127.0.0.1:<port>`，0 为随机，回复实际端口；连接到来时向客户端打开 `forwarded-tcpip`）、`cancel-tcpip-forward`、`keepalive@openssh.com` 回 true；其它回 false。
  - 测试控制：`(*Server).ServeHTTP(port int, h http.Handler) (stop func())`（起 httptest 并 `SetTarget`）、`ForwardAddr() string`（最近一次 `tcpip-forward` 的实际监听地址）、`OpenToClient(chType string, extra []byte) error`（用最近一条连接向客户端发起 `NewChannel`，返回对端的拒绝错误）、`ConnCount() int`、`DropAll()`（关闭全部服务端连接，模拟上游崩溃）。

- [ ] **Step 1: 写失败的测试**

`internal/access/sshtest/server_test.go`：

```go
package sshtest_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"spark-cloud/internal/access/sshtest"
)

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dial(t *testing.T, srv *sshtest.Server, signer ssh.Signer) *ssh.Client {
	t.Helper()
	c, err := ssh.Dial("tcp", srv.Addr(), &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(srv.Signer().PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func output(t *testing.T, c *ssh.Client, cmd string) (string, error) {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.Output(cmd)
	return string(out), err
}

func TestSessionExecShellSftp(t *testing.T) {
	up := newSigner(t)
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if !strings.HasPrefix(srv.HostPubLine(), "ssh-ed25519 ") {
		t.Fatalf("host pub = %q", srv.HostPubLine())
	}
	// 错误的客户端密钥被拒
	if _, err := ssh.Dial("tcp", srv.Addr(), &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(newSigner(t))}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second}); err == nil {
		t.Fatal("wrong key accepted")
	}
	c := dial(t, srv, up)
	if out, err := output(t, c, "echo hi"); err != nil || out != "hi\n" {
		t.Fatalf("echo = %q %v", out, err)
	}
	var exitErr *ssh.ExitError
	if _, err := output(t, c, "exit 3"); !errors.As(err, &exitErr) || exitErr.ExitStatus() != 3 {
		t.Fatalf("exit 3 = %v", err)
	}
	sess, _ := c.NewSession()
	sess.Stdin = strings.NewReader("abc")
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	if err := sess.Run("cat"); err != nil || stdout.String() != "abc" {
		t.Fatalf("cat = %q %v", stdout.String(), err)
	}
	sess.Close()
	sess, _ = c.NewSession()
	sess.Stderr = &stderr
	if err := sess.Run("stderr oops"); err != nil || stderr.String() != "oops" {
		t.Fatalf("stderr = %q %v", stderr.String(), err)
	}
	sess.Close()
	sess, _ = c.NewSession()
	sess.Stdout = &stdout
	stdout.Reset()
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Wait(); err != nil || stdout.String() != "SFTP-FAKE\n" {
		t.Fatalf("sftp = %q %v", stdout.String(), err)
	}
	sess.Close()
	sess, _ = c.NewSession()
	in, _ := sess.StdinPipe()
	outp, _ := sess.StdoutPipe()
	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(in, "ls")
	buf := make([]byte, 64)
	n, _ := outp.Read(buf)
	if string(buf[:n]) != "shell:ls\n" {
		t.Fatalf("shell = %q", buf[:n])
	}
	in.Close()
	sess.Close()
	if srv.ConnCount() != 1 {
		t.Fatalf("conns = %d", srv.ConnCount())
	}
}

func TestDirectTCPIPAndForward(t *testing.T) {
	up := newSigner(t)
	srv, _ := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	defer srv.Close()
	stop := srv.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "hello from 8888") }))
	defer stop()
	c := dial(t, srv, up)
	conn, err := c.Dial("tcp", "127.0.0.1:8888")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(conn, "GET / HTTP/1.0\r\nHost: x\r\n\r\n")
	body, _ := io.ReadAll(conn)
	if !strings.HasSuffix(string(body), "hello from 8888") {
		t.Fatalf("body = %q", body)
	}
	if _, err := c.Dial("tcp", "127.0.0.1:9"); err == nil {
		t.Fatal("unregistered port accepted")
	}
	if extra := srv.LastDirectTCPIP(); len(extra) == 0 {
		t.Fatal("direct-tcpip extra data not recorded")
	}
	ln, err := c.Listen("tcp", "127.0.0.1:0") // -R
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(conn, conn)
		conn.Close()
	}()
	back, err := net.Dial("tcp", srv.ForwardAddr())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(back, "ping")
	buf := make([]byte, 4)
	io.ReadFull(back, buf)
	if string(buf) != "ping" {
		t.Fatalf("forwarded echo = %q", buf)
	}
	back.Close()
}

func TestAgentHostkeysAndControl(t *testing.T) {
	up := newSigner(t)
	srv, _ := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	defer srv.Close()
	c := dial(t, srv, up)
	keyring := agent.NewKeyring()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	if err := agent.ForwardToAgent(c, keyring); err != nil {
		t.Fatal(err)
	}
	sess, _ := c.NewSession()
	if err := agent.RequestAgentForwarding(sess); err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("agent")
	if err != nil || string(out) != "agent:1\n" {
		t.Fatalf("agent = %q %v", out, err)
	}
	sess.Close()
	if out, err := output(t, c, "agent"); err != nil || out != "agent:none\n" {
		t.Fatalf("agent without request = %q %v", out, err)
	}
	if out, err := output(t, c, "hostkeys"); err != nil || out != "sent\n" {
		t.Fatalf("hostkeys = %q %v", out, err)
	}
	// 服务端主动向客户端开通道：x/crypto 客户端对未注册类型回 UnknownChannelType
	err = srv.OpenToClient("x11", nil)
	var oce *ssh.OpenChannelError
	if !errors.As(err, &oce) || oce.Reason != ssh.UnknownChannelType {
		t.Fatalf("x11 to client = %v", err)
	}
	srv.DropAll()
	if _, err := output(t, c, "echo after-drop"); err == nil {
		t.Fatal("session survived DropAll")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/access/sshtest/ 2>&1 | head -5`
Expected: 编译错误（包不存在）。

- [ ] **Step 3: 实现**

`internal/access/sshtest/server.go`：

```go
// Package sshtest 是用 x/crypto/ssh 写的假「容器 sshd」：只接受平台上游公钥，支持 session（exec / shell / sftp）、
// direct-tcpip（按登记端口转发）、tcpip-forward 回连、agent 转发、hostkeys-00 推送，以及若干测试控制钩子。
// 供 sshgw / upstream / ingress 单元测试、Go 集成测试与 e2e-server 共用；生产二进制不引用它。
package sshtest

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type Options struct {
	AuthorizedKey ssh.PublicKey // 允许以 root 登录的公钥（平台上游公钥）
	HostKey       ssh.Signer    // nil → 生成 ed25519
}

type Server struct {
	lis    net.Listener
	signer ssh.Signer
	cfg    *ssh.ServerConfig

	mu          sync.Mutex
	targets     map[int]string // direct-tcpip 目标端口 → 实际拨号地址
	lastDirect  []byte
	forwardAddr string
	conns       []*ssh.ServerConn
	closed      bool
	wg          sync.WaitGroup
}

func New(o Options) (*Server, error) {
	if o.AuthorizedKey == nil {
		return nil, errors.New("sshtest: AuthorizedKey required")
	}
	signer := o.HostKey
	if signer == nil {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		if signer, err = ssh.NewSignerFromKey(priv); err != nil {
			return nil, err
		}
	}
	want := o.AuthorizedKey.Marshal()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == "root" && bytes.Equal(key.Marshal(), want) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("sshtest: key not authorized")
		},
	}
	cfg.AddHostKey(signer)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{lis: lis, signer: signer, cfg: cfg, targets: map[int]string{}}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

func (s *Server) Addr() string       { return s.lis.Addr().String() }
func (s *Server) Port() int          { return s.lis.Addr().(*net.TCPAddr).Port }
func (s *Server) Signer() ssh.Signer { return s.signer }
func (s *Server) HostPubLine() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.signer.PublicKey())))
}

// Close 停止监听并关闭全部连接。
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.lis.Close()
	s.DropAll()
	s.wg.Wait()
}

// SetTarget 让到容器内 127.0.0.1:<port> 的 direct-tcpip 转到 addr。
func (s *Server) SetTarget(port int, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets[port] = addr
}

// ServeHTTP 在随机地址起一个 httptest 服务并登记为 port 的目标；返回停止函数。
func (s *Server) ServeHTTP(port int, h http.Handler) (stop func()) {
	ts := httptest.NewServer(h)
	s.SetTarget(port, strings.TrimPrefix(ts.URL, "http://"))
	return func() {
		ts.Close()
		s.mu.Lock()
		delete(s.targets, port)
		s.mu.Unlock()
	}
}

func (s *Server) LastDirectTCPIP() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.lastDirect...)
}

func (s *Server) ForwardAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forwardAddr
}

func (s *Server) ConnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// OpenToClient 用最近一条连接向客户端发起 NewChannel（测试网关是否拒绝 x11 等类型）。
func (s *Server) OpenToClient(chType string, extra []byte) error {
	s.mu.Lock()
	var c *ssh.ServerConn
	if n := len(s.conns); n > 0 {
		c = s.conns[n-1]
	}
	s.mu.Unlock()
	if c == nil {
		return errors.New("sshtest: no connection")
	}
	ch, reqs, err := c.OpenChannel(chType, extra)
	if err != nil {
		return err
	}
	go ssh.DiscardRequests(reqs)
	return ch.Close()
}

// DropAll 关闭全部服务端连接（模拟容器 sshd 崩溃）。
func (s *Server) DropAll() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		nc, err := s.lis.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(nc)
		}()
	}
}

type session struct {
	srv            *Server
	conn           *ssh.ServerConn
	agentRequested bool
}

func (s *Server) handle(nc net.Conn) {
	sconn, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		nc.Close()
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		sconn.Close()
		return
	}
	s.conns = append(s.conns, sconn)
	s.mu.Unlock()
	defer func() {
		sconn.Close()
		s.mu.Lock()
		for i, c := range s.conns {
			if c == sconn {
				s.conns = append(s.conns[:i], s.conns[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()
	go s.globalRequests(sconn, reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			ch, chReqs, err := nc.Accept()
			if err != nil {
				continue
			}
			go (&session{srv: s, conn: sconn}).run(ch, chReqs)
		case "direct-tcpip":
			s.directTCPIP(nc)
		default:
			nc.Reject(ssh.UnknownChannelType, "sshtest: unsupported channel "+nc.ChannelType())
		}
	}
}

type channelOpenDirectMsg struct {
	Raddr string
	Rport uint32
	Laddr string
	Lport uint32
}

func (s *Server) directTCPIP(nc ssh.NewChannel) {
	extra := nc.ExtraData()
	s.mu.Lock()
	s.lastDirect = append([]byte(nil), extra...)
	s.mu.Unlock()
	var msg channelOpenDirectMsg
	if err := ssh.Unmarshal(extra, &msg); err != nil {
		nc.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
		return
	}
	s.mu.Lock()
	target, ok := s.targets[int(msg.Rport)]
	s.mu.Unlock()
	if !ok {
		nc.Reject(ssh.ConnectionFailed, fmt.Sprintf("connect to %s:%d refused", msg.Raddr, msg.Rport))
		return
	}
	dst, err := net.Dial("tcp", target)
	if err != nil {
		nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		dst.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go pipe(ch, dst)
}

// pipe 双向拷贝；任一方向结束即关闭两端。
func pipe(ch ssh.Channel, c net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(ch, c); ch.CloseWrite(); done <- struct{}{} }()
	go func() { io.Copy(c, ch); if tc, ok := c.(*net.TCPConn); ok { tc.CloseWrite() }; done <- struct{}{} }()
	<-done
	<-done
	ch.Close()
	c.Close()
}

type tcpipForwardMsg struct {
	Addr string
	Port uint32
}

type forwardedTCPIPMsg struct {
	Addr     string
	Port     uint32
	OrigAddr string
	OrigPort uint32
}

func (s *Server) globalRequests(sconn *ssh.ServerConn, reqs <-chan *ssh.Request) {
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	for req := range reqs {
		switch req.Type {
		case "tcpip-forward":
			var m tcpipForwardMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				req.Reply(false, nil)
				continue
			}
			l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(m.Port))))
			if err != nil {
				req.Reply(false, nil)
				continue
			}
			listeners = append(listeners, l)
			port := uint32(l.Addr().(*net.TCPAddr).Port)
			s.mu.Lock()
			s.forwardAddr = l.Addr().String()
			s.mu.Unlock()
			if m.Port == 0 {
				req.Reply(true, ssh.Marshal(struct{ Port uint32 }{port}))
			} else {
				req.Reply(true, nil)
			}
			go func(l net.Listener, bind string, port uint32) {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					ra := c.RemoteAddr().(*net.TCPAddr)
					ch, chReqs, err := sconn.OpenChannel("forwarded-tcpip", ssh.Marshal(forwardedTCPIPMsg{Addr: bind, Port: port, OrigAddr: ra.IP.String(), OrigPort: uint32(ra.Port)}))
					if err != nil {
						c.Close()
						continue
					}
					go ssh.DiscardRequests(chReqs)
					go pipe(ch, c)
				}
			}(l, m.Addr, port)
		case "cancel-tcpip-forward":
			for _, l := range listeners {
				l.Close()
			}
			listeners = nil
			req.Reply(true, nil)
		case "keepalive@openssh.com":
			req.Reply(true, nil)
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

type execMsg struct{ Command string }
type subsystemMsg struct{ Name string }

func (x *session) run(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "pty-req", "env", "window-change":
			req.Reply(true, nil)
		case "auth-agent-req@openssh.com":
			x.agentRequested = true
			req.Reply(true, nil)
		case "exec":
			var m execMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			x.exit(ch, x.exec(ch, m.Command))
			return
		case "shell":
			req.Reply(true, nil)
			sc := bufio.NewScanner(ch)
			for sc.Scan() {
				fmt.Fprintf(ch, "shell:%s\n", sc.Text())
			}
			x.exit(ch, 0)
			return
		case "subsystem":
			var m subsystemMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil || m.Name != "sftp" {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			fmt.Fprint(ch, "SFTP-FAKE\n")
			x.exit(ch, 0)
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (x *session) exit(ch ssh.Channel, status uint32) {
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
}

func (x *session) exec(ch ssh.Channel, cmd string) uint32 {
	switch {
	case strings.HasPrefix(cmd, "echo "):
		fmt.Fprint(ch, strings.TrimPrefix(cmd, "echo ")+"\n")
	case strings.HasPrefix(cmd, "exit "):
		n, _ := strconv.Atoi(strings.TrimPrefix(cmd, "exit "))
		return uint32(n)
	case cmd == "cat":
		io.Copy(ch, ch)
	case strings.HasPrefix(cmd, "stderr "):
		fmt.Fprint(ch.Stderr(), strings.TrimPrefix(cmd, "stderr "))
	case cmd == "agent":
		if !x.agentRequested {
			fmt.Fprint(ch, "agent:none\n")
			return 0
		}
		ach, areqs, err := x.conn.OpenChannel("auth-agent@openssh.com", nil)
		if err != nil {
			fmt.Fprintf(ch, "agent:error %v\n", err)
			return 1
		}
		go ssh.DiscardRequests(areqs)
		keys, err := agent.NewClient(ach).List()
		ach.Close()
		if err != nil {
			fmt.Fprintf(ch, "agent:error %v\n", err)
			return 1
		}
		fmt.Fprintf(ch, "agent:%d\n", len(keys))
	case cmd == "hostkeys":
		x.conn.SendRequest("hostkeys-00@openssh.com", false, x.srv.signer.PublicKey().Marshal())
		fmt.Fprint(ch, "sent\n")
	default:
		fmt.Fprintf(ch, "exec:%s\n", cmd)
	}
	return 0
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./internal/access/... && go test ./internal/access/sshtest/ 2>&1 | tail -3`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/access/sshtest
git commit -m "test(access): in-process fake container sshd (session/direct-tcpip/forward/agent/hostkeys) for gateway tests

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 8: `internal/access/upstream`：固定主机密钥回调与按实例共享的上游连接池

**Files:**
- Create: `internal/access/upstream/hostkey.go`、`internal/access/upstream/pool.go`、`internal/access/upstream/pool_test.go`

**Interfaces:**
- Consumes: Task 7 `sshtest`。
- Produces:
  - `upstream.HostKeyCallback(expected string, tofu func(pubLine string) error) ssh.HostKeyCallback`：`expected` 非空时逐字比对 authorized_keys 行；为空时调用 `tofu`（首连信任），`tofu` 返回错误则拒绝。
  - `upstream.ClientConfig(signer ssh.Signer, hostKey ssh.HostKeyCallback, timeout time.Duration) *ssh.ClientConfig`（`User: "root"`，`ClientVersion: "SSH-2.0-SparkCloud"`）。
  - `upstream.Target{InstanceID, LanIP, HostPubkey string}`。
  - `upstream.Store interface { SetSSHHostPubkeyIfEmpty(ctx, id, pub string) (bool, error) }`。
  - `upstream.Config{Signer ssh.Signer; Port int; DialTimeout time.Duration /*0→10s*/; Keepalive time.Duration /*0→30s*/; Log *slog.Logger}`。
  - `upstream.New(cfg Config, store Store) *Pool`；`(*Pool).Dial(ctx, t Target, port int) (net.Conn, error)`（一条 `direct-tcpip` 到容器内 `127.0.0.1:<port>`；连接按实例共享，`LanIP` 变化即丢弃重建）、`Drop(instanceID string)`、`Len() int`、`Close()`。
  - `upstream.DialError{InstanceID string; Err error}`（`Error()`、`Unwrap()`）：上游连不上 / 主机密钥不符时返回，调用方据此告警。

- [ ] **Step 1: 写失败的测试**

`internal/access/upstream/pool_test.go`：

```go
package upstream_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/access/upstream"
)

func signer(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type memStore struct {
	mu   sync.Mutex
	pubs map[string]string
}

func (m *memStore) SetSSHHostPubkeyIfEmpty(_ context.Context, id, pub string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pubs[id] != "" {
		return false, nil
	}
	m.pubs[id] = pub
	return true, nil
}

func httpGet(t *testing.T, p *upstream.Pool, tg upstream.Target, port int) (string, error) {
	t.Helper()
	conn, err := p.Dial(context.Background(), tg, port)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET / HTTP/1.0\r\nHost: x\r\n\r\n")
	b, _ := io.ReadAll(conn)
	parts := strings.SplitN(string(b), "\r\n\r\n", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("bad http response %q", b)
	}
	return parts[1], nil
}

func TestPoolSharesClientAndVerifiesHostKey(t *testing.T) {
	up := signer(t)
	srv, _ := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	defer srv.Close()
	stop := srv.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	defer stop()
	store := &memStore{pubs: map[string]string{}}
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: 2 * time.Second}, store)
	defer p.Close()
	good := upstream.Target{InstanceID: "ins-aaaaaaaa", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	for i := 0; i < 3; i++ {
		if body, err := httpGet(t, p, good, 8888); err != nil || body != "ok" {
			t.Fatalf("get %d = %q %v", i, body, err)
		}
	}
	if p.Len() != 1 || srv.ConnCount() != 1 {
		t.Fatalf("pool=%d upstream conns=%d, want 1 shared connection", p.Len(), srv.ConnCount())
	}
	// 主机密钥不符 → DialError，且不建立连接
	bad := upstream.Target{InstanceID: "ins-bbbbbbbb", LanIP: "127.0.0.1", HostPubkey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	var de *upstream.DialError
	if _, err := httpGet(t, p, bad, 8888); !errors.As(err, &de) || de.InstanceID != "ins-bbbbbbbb" || !strings.Contains(err.Error(), "主机密钥") {
		t.Fatalf("mismatch = %v", err)
	}
	// 存量实例（公钥为空）：首连信任并写回；之后用写回的公钥
	legacy := upstream.Target{InstanceID: "ins-cccccccc", LanIP: "127.0.0.1"}
	if body, err := httpGet(t, p, legacy, 8888); err != nil || body != "ok" {
		t.Fatalf("tofu = %q %v", body, err)
	}
	if store.pubs["ins-cccccccc"] != srv.HostPubLine() {
		t.Fatalf("tofu stored %q", store.pubs["ins-cccccccc"])
	}
	// 未登记端口：拨号失败但连接保留
	if _, err := p.Dial(context.Background(), good, 9); err == nil || p.Len() != 2 {
		t.Fatalf("port 9 = %v, len=%d", err, p.Len())
	}
	// lan_ip 变化 → 旧连接丢弃、重建
	moved := good
	moved.LanIP = "127.0.0.1"
	moved.LanIP = "127.000.000.001" // 字面量不同即视为变化（登记地址变了）
	if _, err := httpGet(t, p, moved, 8888); err == nil {
		t.Fatal("unparseable lan ip accepted")
	}
	p.Drop("ins-aaaaaaaa")
	if p.Len() != 1 {
		t.Fatalf("after drop len=%d", p.Len())
	}
	// 上游崩溃：下一次 Dial 重建连接
	srv.DropAll()
	if body, err := httpGet(t, p, good, 8888); err != nil || body != "ok" {
		t.Fatalf("after upstream drop = %q %v", body, err)
	}
}

func TestPoolUnreachableAndNotRunningTargets(t *testing.T) {
	up := signer(t)
	store := &memStore{pubs: map[string]string{}}
	p := upstream.New(upstream.Config{Signer: up, Port: 1, DialTimeout: 300 * time.Millisecond}, store)
	defer p.Close()
	var de *upstream.DialError
	_, err := p.Dial(context.Background(), upstream.Target{InstanceID: "ins-dddddddd", LanIP: "127.0.0.1", HostPubkey: "x"}, 80)
	if !errors.As(err, &de) {
		t.Fatalf("unreachable = %v", err)
	}
	if _, err := p.Dial(context.Background(), upstream.Target{InstanceID: "ins-eeeeeeee", LanIP: "", HostPubkey: "x"}, 80); err == nil {
		t.Fatal("empty lan ip accepted")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/access/upstream/ 2>&1 | head -5`
Expected: 编译错误。

- [ ] **Step 3: 实现**

`internal/access/upstream/hostkey.go`：

```go
// Package upstream 负责平台到容器 sshd 的连接：固定主机密钥校验（存量实例首连信任）与按实例共享的 SSH 连接池。
package upstream

import (
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const clientVersion = "SSH-2.0-SparkCloud"

// HostKeyCallback 按 instances.ssh_host_pubkey 校验上游主机密钥：expected 非空时必须逐字相等；
// 为空（计划 2 创建的存量实例）时调用 tofu 写回并信任本次。
func HostKeyCallback(expected string, tofu func(pubLine string) error) ssh.HostKeyCallback {
	expected = strings.TrimSpace(expected)
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		if expected == "" {
			if tofu == nil {
				return fmt.Errorf("上游主机密钥未登记且无法首连信任")
			}
			return tofu(line)
		}
		if line != expected {
			return fmt.Errorf("上游主机密钥不匹配：登记 %s，实际 %s", fingerprintOf(expected), ssh.FingerprintSHA256(key))
		}
		return nil
	}
}

func fingerprintOf(line string) string {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return "(无法解析)"
	}
	return ssh.FingerprintSHA256(pub)
}

// ClientConfig 是拨容器 sshd 用的客户端配置（用户 root，平台上游私钥）。
func ClientConfig(signer ssh.Signer, hostKey ssh.HostKeyCallback, timeout time.Duration) *ssh.ClientConfig {
	return &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: hostKey, Timeout: timeout, ClientVersion: clientVersion}
}
```

`internal/access/upstream/pool.go`：

```go
package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Store interface {
	SetSSHHostPubkeyIfEmpty(ctx context.Context, id, pub string) (bool, error)
}

type Config struct {
	Signer      ssh.Signer
	Port        int           // 容器 sshd 在机器上的端口
	DialTimeout time.Duration // 零值 10s
	Keepalive   time.Duration // 零值 30s；连续 2 次无回应即关闭
	Log         *slog.Logger
}

// Target 是路由表给出的拨号目标。
type Target struct {
	InstanceID string
	LanIP      string
	HostPubkey string // 为空 → 首连信任并写回
}

// DialError 表示到达不了容器 sshd（拨号失败、握手失败、主机密钥不符）；端口未监听不算。
type DialError struct {
	InstanceID string
	Err        error
}

func (e *DialError) Error() string { return "上游 " + e.InstanceID + ": " + e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

type entry struct {
	client *ssh.Client
	lanIP  string
	stop   chan struct{}
}

type Pool struct {
	cfg   Config
	store Store
	mu    sync.Mutex
	locks map[string]*sync.Mutex // 每实例一把：并发首次拨号只建一条
	conns map[string]*entry
}

func New(cfg Config, store Store) *Pool {
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.Keepalive <= 0 {
		cfg.Keepalive = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Pool{cfg: cfg, store: store, locks: map[string]*sync.Mutex{}, conns: map[string]*entry{}}
}

func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// Dial 返回一条到容器内 127.0.0.1:<port> 的通道（net.Conn）。
func (p *Pool) Dial(ctx context.Context, t Target, port int) (net.Conn, error) {
	c, err := p.client(ctx, t)
	if err != nil {
		return nil, err
	}
	conn, err := c.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		var oce *ssh.OpenChannelError
		if !errors.As(err, &oce) { // 不是上游拒绝通道，而是连接本身坏了：丢弃，下次重建
			p.Drop(t.InstanceID)
			return nil, &DialError{InstanceID: t.InstanceID, Err: err}
		}
		return nil, fmt.Errorf("容器内端口 %d 未监听: %w", port, err)
	}
	return conn, nil
}

func (p *Pool) client(ctx context.Context, t Target) (*ssh.Client, error) {
	if net.ParseIP(t.LanIP) == nil {
		return nil, &DialError{InstanceID: t.InstanceID, Err: fmt.Errorf("机器内网地址无效 %q", t.LanIP)}
	}
	p.mu.Lock()
	l := p.locks[t.InstanceID]
	if l == nil {
		l = &sync.Mutex{}
		p.locks[t.InstanceID] = l
	}
	p.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	p.mu.Lock()
	e := p.conns[t.InstanceID]
	p.mu.Unlock()
	if e != nil && e.lanIP == t.LanIP {
		return e.client, nil
	}
	if e != nil {
		p.Drop(t.InstanceID) // 登记地址变了
	}
	addr := net.JoinHostPort(t.LanIP, strconv.Itoa(p.cfg.Port))
	tofu := func(pub string) error {
		ok, err := p.store.SetSSHHostPubkeyIfEmpty(ctx, t.InstanceID, pub)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("主机公钥已由其它副本写回，请重试")
		}
		p.cfg.Log.Warn("upstream host key trusted on first use", "instance", t.InstanceID, "fingerprint", fingerprintOf(pub))
		return nil
	}
	d := net.Dialer{Timeout: p.cfg.DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, &DialError{InstanceID: t.InstanceID, Err: err}
	}
	cc, chans, reqs, err := ssh.NewClientConn(nc, addr, ClientConfig(p.cfg.Signer, HostKeyCallback(t.HostPubkey, tofu), p.cfg.DialTimeout))
	if err != nil {
		nc.Close()
		return nil, &DialError{InstanceID: t.InstanceID, Err: err}
	}
	client := ssh.NewClient(cc, chans, reqs)
	e = &entry{client: client, lanIP: t.LanIP, stop: make(chan struct{})}
	p.mu.Lock()
	p.conns[t.InstanceID] = e
	p.mu.Unlock()
	go p.keepalive(t.InstanceID, e)
	return client, nil
}

// keepalive 每 Keepalive 发一次 keepalive@openssh.com，连续 2 次无回应（或连接已关）即丢弃。
func (p *Pool) keepalive(id string, e *entry) {
	t := time.NewTicker(p.cfg.Keepalive)
	defer t.Stop()
	misses := 0
	closed := make(chan struct{})
	go func() { e.client.Wait(); close(closed) }()
	for {
		select {
		case <-e.stop:
			return
		case <-closed:
			p.dropEntry(id, e)
			return
		case <-t.C:
			res := make(chan bool, 1)
			go func() { _, _, err := e.client.SendRequest("keepalive@openssh.com", true, nil); res <- err == nil }()
			select {
			case ok := <-res:
				if ok {
					misses = 0
					continue
				}
			case <-time.After(10 * time.Second):
			}
			if misses++; misses >= 2 {
				p.cfg.Log.Warn("upstream keepalive failed, dropping connection", "instance", id)
				p.dropEntry(id, e)
				return
			}
		}
	}
}

// Drop 关闭并移除某实例的共享连接（实例离开 running、地址变化、连接损坏）。
func (p *Pool) Drop(instanceID string) {
	p.mu.Lock()
	e := p.conns[instanceID]
	p.mu.Unlock()
	if e != nil {
		p.dropEntry(instanceID, e)
	}
}

func (p *Pool) dropEntry(id string, e *entry) {
	p.mu.Lock()
	if p.conns[id] == e {
		delete(p.conns, id)
	}
	p.mu.Unlock()
	select {
	case <-e.stop:
	default:
		close(e.stop)
	}
	e.client.Close()
}

func (p *Pool) Close() {
	p.mu.Lock()
	all := p.conns
	p.conns = map[string]*entry{}
	p.mu.Unlock()
	for id, e := range all {
		p.dropEntry(id, e)
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./internal/access/... && go test ./internal/access/... 2>&1 | tail -3`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/access/upstream
git commit -m "feat(access): upstream ssh connection pool with pinned host keys and first-use trust for legacy instances

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 9: `internal/sshgw`：服务端配置、Banner、认证、按连接计数的限流、会话记录、上游中继骨架

**Files:**
- Create: `internal/sshgw/server.go`、`internal/sshgw/auth.go`、`internal/sshgw/limiter.go`、`internal/sshgw/relay.go`、`internal/sshgw/export_test.go`、`internal/sshgw/helpers_test.go`、`internal/sshgw/auth_test.go`

**Interfaces:**
- Consumes: Task 3 `rental.AccessInfo` / `SSHSessionStart`；Task 2 `identity.SSHKey`；Task 8 `upstream.HostKeyCallback` / `ClientConfig`。
- Produces:
  - `sshgw.Store interface { Access(ctx, instanceID string) (rental.AccessInfo, error); SetSSHHostPubkeyIfEmpty(ctx, id, pub string) (bool, error); OpenSSHSession(ctx, rental.SSHSessionStart) (int64, error); TouchSSHSessions(ctx, ids []int64) error; CloseSSHSession(ctx, id, up, down int64) error; RaiseSSHBruteforce(ctx, instanceID string) error; RaiseUpstreamUnreachable(ctx, instanceID string) error }`（`*rental.Service` 满足）。
  - `sshgw.Keys interface { FindSSHKey(ctx, userID int64, fingerprint string) (identity.SSHKey, error); TouchSSHKey(ctx, id int64) error }`（`*identity.Service` 满足）。
  - `sshgw.Config{HostKey ssh.Signer; Upstream ssh.Signer; UpstreamPort int; MaxSessionsPerInstance int /*0→16*/; MaxConnsPerIP int /*0→32*/; DrainTimeout time.Duration /*0→30s*/; DisableRateLimits bool}`；`sshgw.Deps{Store Store; Keys Keys; Clock clock.Clock; Log *slog.Logger}`。
  - `sshgw.New(cfg Config, d Deps) (*Server, error)`；`(*Server).Serve(ctx context.Context, lis net.Listener) error`（ctx 结束后停止 accept、drain、关闭全部会话；Task 11 补扫描 / 刷新 / drain 细节，本任务 Serve 已能 accept 并在 ctx 结束时关闭）；`(*Server).Sessions() int`。
  - Banner / 提示常量：`BannerUnknown = "用户名应为实例 ID，例如 ins-a1b2c3d4"`、`BannerSuspended = "实例所在机器暂时失联，计时已暂停，请稍后重试"`、`BannerCreating = "实例正在开通，请稍后再试"`、`BannerStoppedFmt = "实例已到期停机，数据保留至 %s，续费后可继续使用"`、`BannerReleased = "实例已释放"`、`MsgTooManySessions = "连接数过多"`、`MsgUpstreamUnreachable = "无法连接到实例，请稍后重试"`、`MsgDisconnected = "实例已停机 / 已释放，连接已断开"`。
  - 测试钩子（`export_test.go`）：`SetLimitsForTest(s, ipFails, instFails, maxConns int)`、`FailCountForTest(s, ip string) int`、`BlockedForTest(s, ip string) bool`。

- [ ] **Step 1: 写测试辅助与失败的测试**

`internal/sshgw/helpers_test.go`：

```go
package sshgw_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/sshgw"
)

const instA = "ins-aaaaaaaa"

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type fakeStore struct {
	mu       sync.Mutex
	infos    map[string]rental.AccessInfo
	nextID   int64
	opened   []rental.SSHSessionStart
	closed   map[int64][2]int64
	touched  [][]int64
	alerts   []string
	accessed int
}

func newFakeStore() *fakeStore {
	return &fakeStore{infos: map[string]rental.AccessInfo{}, closed: map[int64][2]int64{}}
}

func (f *fakeStore) set(id string, info rental.AccessInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infos[id] = info
}

func (f *fakeStore) Access(_ context.Context, id string) (rental.AccessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessed++
	info, ok := f.infos[id]
	if !ok {
		return rental.AccessInfo{}, apperr.ErrNotFound
	}
	return info, nil
}

func (f *fakeStore) SetSSHHostPubkeyIfEmpty(_ context.Context, id, pub string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.infos[id]
	if info.Instance.SSHHostPubkey != "" {
		return false, nil
	}
	info.Instance.SSHHostPubkey = pub
	f.infos[id] = info
	return true, nil
}

func (f *fakeStore) OpenSSHSession(_ context.Context, st rental.SSHSessionStart) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.opened = append(f.opened, st)
	return f.nextID, nil
}

func (f *fakeStore) TouchSSHSessions(_ context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, append([]int64(nil), ids...))
	return nil
}

func (f *fakeStore) CloseSSHSession(_ context.Context, id, up, down int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed[id] = [2]int64{up, down}
	return nil
}

func (f *fakeStore) RaiseSSHBruteforce(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, "bruteforce:"+id)
	return nil
}

func (f *fakeStore) RaiseUpstreamUnreachable(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, "unreachable:"+id)
	return nil
}

func (f *fakeStore) snapshot() (opened []rental.SSHSessionStart, closed map[int64][2]int64, alerts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := map[int64][2]int64{}
	for k, v := range f.closed {
		c[k] = v
	}
	return append([]rental.SSHSessionStart(nil), f.opened...), c, append([]string(nil), f.alerts...)
}

type fakeKeys struct {
	mu      sync.Mutex
	keys    map[int64]map[string]identity.SSHKey // user → fingerprint → key
	touched []int64
}

func (f *fakeKeys) add(userID int64, pub ssh.PublicKey) identity.SSHKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys == nil {
		f.keys = map[int64]map[string]identity.SSHKey{}
	}
	if f.keys[userID] == nil {
		f.keys[userID] = map[string]identity.SSHKey{}
	}
	k := identity.SSHKey{ID: int64(len(f.keys[userID]) + 1), UserID: userID, Fingerprint: ssh.FingerprintSHA256(pub), PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))}
	f.keys[userID][k.Fingerprint] = k
	return k
}

func (f *fakeKeys) FindSSHKey(_ context.Context, userID int64, fp string) (identity.SSHKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.keys[userID][fp]; ok {
		return k, nil
	}
	return identity.SSHKey{}, apperr.ErrNotFound
}

func (f *fakeKeys) TouchSSHKey(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, id)
	return nil
}

type env struct {
	t        *testing.T
	ctx      context.Context
	cancel   context.CancelFunc
	clk      *clock.Fake
	store    *fakeStore
	keys     *fakeKeys
	upstream *sshtest.Server
	srv      *sshgw.Server
	hostKey  ssh.Signer
	addr     string
	done     chan struct{}
}

func runningInfo(e *env, id string, userID int64, password string) rental.AccessInfo {
	return rental.AccessInfo{
		Instance:      rental.Instance{ID: id, UserID: userID, MachineID: 1, State: rental.StateRunning, PasswordAuth: true, SSHHostPubkey: e.upstream.HostPubLine()},
		Password:      password,
		MachineLanIP:  "127.0.0.1",
		MachineSerial: "SN-TEST",
		AgentOnline:   true,
	}
}

func newEnvWith(t *testing.T, tweak func(*sshgw.Config)) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	up := newSigner(t)
	us, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: ctx, cancel: cancel, clk: clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), store: newFakeStore(), keys: &fakeKeys{}, upstream: us, hostKey: newSigner(t), done: make(chan struct{})}
	cfg := sshgw.Config{HostKey: e.hostKey, Upstream: up, UpstreamPort: us.Port(), DrainTimeout: time.Second}
	if tweak != nil {
		tweak(&cfg)
	}
	e.srv, err = sshgw.New(cfg, sshgw.Deps{Store: e.store, Keys: e.keys, Clock: e.clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.addr = lis.Addr().String()
	go func() { defer close(e.done); _ = e.srv.Serve(ctx, lis) }()
	e.store.set(instA, runningInfo(e, instA, 7, "pw-correct-16chr"))
	t.Cleanup(func() { cancel(); <-e.done; us.Close() })
	return e
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// dial 以密码或公钥连接网关；返回 banner 文本与错误。
func (e *env) dial(user string, auth ssh.AuthMethod) (*ssh.Client, string, error) {
	e.t.Helper()
	var banner string
	c, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{auth},
		HostKeyCallback: ssh.FixedHostKey(e.hostKey.PublicKey()), Timeout: 5 * time.Second,
		BannerCallback: func(m string) error { banner += m; return nil }})
	if err == nil {
		e.t.Cleanup(func() { c.Close() })
	}
	return c, strings.TrimSpace(banner), err
}

func (e *env) mustDial(user string, auth ssh.AuthMethod) *ssh.Client {
	e.t.Helper()
	c, _, err := e.dial(user, auth)
	if err != nil {
		e.t.Fatalf("dial %s: %v", user, err)
	}
	return c
}

func run(t *testing.T, c *ssh.Client, cmd string) (stdout, stderr string, err error) {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		return "", "", err
	}
	defer sess.Close()
	var o, se strings.Builder
	sess.Stdout, sess.Stderr = &o, &se
	err = sess.Run(cmd)
	return o.String(), se.String(), err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
```

`internal/sshgw/auth_test.go`：

```go
package sshgw_test

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/rental"
	"spark-cloud/internal/sshgw"
)

func TestPasswordLoginRelaysExecAndRecordsSession(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	out, _, err := run(t, c, "echo hello")
	if err != nil || out != "hello\n" {
		t.Fatalf("exec = %q %v", out, err)
	}
	c.Close()
	waitFor(t, "session closed", func() bool { _, closed, _ := e.store.snapshot(); return len(closed) == 1 })
	opened, closed, _ := e.store.snapshot()
	if len(opened) != 1 || opened[0].InstanceID != instA || opened[0].UserID != 7 || opened[0].AuthMethod != "password" || opened[0].KeyID != nil || opened[0].ClientIP != "127.0.0.1" {
		t.Fatalf("opened = %+v", opened)
	}
	if b := closed[1]; b[1] < int64(len("hello\n")) {
		t.Fatalf("bytes = %v", b)
	}
	if e.srv.Sessions() != 0 {
		t.Fatalf("sessions = %d", e.srv.Sessions())
	}
}

func TestPublicKeyLoginAndPasswordDisabled(t *testing.T) {
	e := newEnv(t)
	userKey := newSigner(t)
	k := e.keys.add(7, userKey.PublicKey())
	c := e.mustDial(instA, ssh.PublicKeys(userKey))
	if out, _, err := run(t, c, "echo k"); err != nil || out != "k\n" {
		t.Fatalf("pubkey exec = %q %v", out, err)
	}
	waitFor(t, "key touched", func() bool { e.keys.mu.Lock(); defer e.keys.mu.Unlock(); return len(e.keys.touched) == 1 && e.keys.touched[0] == k.ID })
	opened, _, _ := e.store.snapshot()
	if opened[0].AuthMethod != "publickey" || opened[0].KeyID == nil || *opened[0].KeyID != k.ID {
		t.Fatalf("opened = %+v", opened[0])
	}
	// 其他用户的钥匙不行
	if _, _, err := e.dial(instA, ssh.PublicKeys(newSigner(t))); err == nil {
		t.Fatal("unknown key accepted")
	}
	// 关闭密码登录后密码被拒、公钥仍通
	info := runningInfo(e, instA, 7, "pw-correct-16chr")
	info.Instance.PasswordAuth = false
	e.store.set(instA, info)
	if _, _, err := e.dial(instA, ssh.Password("pw-correct-16chr")); err == nil {
		t.Fatal("password accepted while disabled")
	}
	e.mustDial(instA, ssh.PublicKeys(userKey))
}

func TestBannerForUnknownUsernames(t *testing.T) {
	e := newEnv(t)
	for _, user := range []string{"root", "INS-AAAAAAAA", " ins-aaaaaaaa", "ins-aaaaaaaa@ssh", "ins-zzzzzzzz", "ins-aaaaaaa", ""} {
		_, banner, err := e.dial(user, ssh.Password("pw-correct-16chr"))
		if err == nil || banner != sshgw.BannerUnknown {
			t.Fatalf("user %q: banner=%q err=%v", user, banner, err)
		}
	}
	if _, banner, err := e.dial(instA, ssh.Password("wrong")); err == nil || banner != "" {
		t.Fatalf("wrong password: banner=%q err=%v", banner, err)
	}
}

func TestBannerByState(t *testing.T) {
	e := newEnv(t)
	retain := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	cases := []struct {
		name   string
		mutate func(*rental.AccessInfo)
		want   string
	}{
		{"suspended", func(i *rental.AccessInfo) { now := e.clk.Now(); i.Instance.SuspendedAt = &now }, sshgw.BannerSuspended},
		{"creating", func(i *rental.AccessInfo) { i.Instance.State = rental.StateCreating }, sshgw.BannerCreating},
		{"stopped", func(i *rental.AccessInfo) { i.Instance.State = rental.StateStopped; i.RetainUntil = &retain }, "实例已到期停机，数据保留至 2026-03-04 13:06，续费后可继续使用"},
		{"wiping", func(i *rental.AccessInfo) { i.Instance.State = rental.StateWiping }, sshgw.BannerReleased},
		{"released", func(i *rental.AccessInfo) { i.Instance.State = rental.StateReleased }, sshgw.BannerReleased},
		{"failed", func(i *rental.AccessInfo) { i.Instance.State = rental.StateFailed }, sshgw.BannerReleased},
	}
	for _, c := range cases {
		info := runningInfo(e, instA, 7, "pw-correct-16chr")
		c.mutate(&info)
		e.store.set(instA, info)
		if _, banner, err := e.dial(instA, ssh.Password("pw-correct-16chr")); err == nil || banner != c.want {
			t.Fatalf("%s: banner=%q err=%v", c.name, banner, err)
		}
	}
	// agent_online=false 但实例仍 running（5 分钟判定窗口内）：允许
	info := runningInfo(e, instA, 7, "pw-correct-16chr")
	info.AgentOnline = false
	e.store.set(instA, info)
	e.mustDial(instA, ssh.Password("pw-correct-16chr"))
}

func TestFailureCountingIsPerConnection(t *testing.T) {
	e := newEnv(t)
	sshgw.SetLimitsForTest(e.srv, 3, 100, 32)
	// agent 里 4 把陌生钥匙 + 正确密码：PublicKeyCallback 失败 4 次，但连接最终成功 → 不计失败
	keys := []ssh.Signer{newSigner(t), newSigner(t), newSigner(t), newSigner(t)}
	c, _, err := e.dial(instA, ssh.PublicKeys(keys...))
	if err == nil {
		c.Close()
		t.Fatal("unknown keys accepted")
	}
	if n := sshgw.FailCountForTest(e.srv, "127.0.0.1"); n != 1 {
		t.Fatalf("one failed connection counted as %d", n)
	}
	c2, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{User: instA, Auth: []ssh.AuthMethod{ssh.PublicKeys(keys...), ssh.Password("pw-correct-16chr")},
		HostKeyCallback: ssh.FixedHostKey(e.hostKey.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
	if n := sshgw.FailCountForTest(e.srv, "127.0.0.1"); n != 1 {
		t.Fatalf("successful connection after key probes counted: %d", n)
	}
	// 密码错 5 次（同一连接内重试）只计 1
	e.dial(instA, ssh.RetryableAuthMethod(ssh.Password("wrong"), 5))
	if n := sshgw.FailCountForTest(e.srv, "127.0.0.1"); n != 2 {
		t.Fatalf("retries within one connection counted as %d", n)
	}
	e.dial(instA, ssh.Password("wrong"))
	if !sshgw.BlockedForTest(e.srv, "127.0.0.1") {
		t.Fatal("ip not blocked after 3 failed connections")
	}
	if _, _, err := e.dial(instA, ssh.Password("pw-correct-16chr")); err == nil {
		t.Fatal("blocked ip could connect")
	}
	e.clk.Advance(10*time.Minute + time.Second)
	e.mustDial(instA, ssh.Password("pw-correct-16chr"))
}

func TestInstanceBruteforceAlertOnce(t *testing.T) {
	e := newEnv(t)
	sshgw.SetLimitsForTest(e.srv, 100, 2, 32)
	for i := 0; i < 4; i++ {
		e.dial(instA, ssh.Password("wrong"))
	}
	_, _, alerts := e.store.snapshot()
	if len(alerts) != 1 || alerts[0] != "bruteforce:"+instA {
		t.Fatalf("alerts = %v", alerts)
	}
}

func TestSessionCapAndUpstreamUnreachable(t *testing.T) {
	e := newEnvWith(t, func(c *sshgw.Config) { c.MaxSessionsPerInstance = 1 })
	first := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if _, _, err := run(t, first, "echo one"); err != nil {
		t.Fatal(err)
	}
	second := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	_, stderr, err := run(t, second, "echo two")
	if err == nil || !strings.Contains(stderr, sshgw.MsgTooManySessions) {
		t.Fatalf("second session: err=%v stderr=%q", err, stderr)
	}
	first.Close()
	waitFor(t, "slot released", func() bool { return e.srv.Sessions() == 0 })
	// 上游拨不通（端口 1 没人监听）：提示 + 告警（进程内 5 分钟节流，连两次只告警一次）
	bad := newEnvWith(t, func(c *sshgw.Config) { c.UpstreamPort = 1 })
	for i := 0; i < 2; i++ {
		c := bad.mustDial(instA, ssh.Password("pw-correct-16chr"))
		_, stderr, err = run(t, c, "echo x")
		if err == nil || !strings.Contains(stderr, sshgw.MsgUpstreamUnreachable) {
			t.Fatalf("unreachable: err=%v stderr=%q", err, stderr)
		}
	}
	_, _, alerts := bad.store.snapshot()
	if len(alerts) != 1 || alerts[0] != "unreachable:"+instA {
		t.Fatalf("alerts = %v", alerts)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/sshgw/ 2>&1 | head -5`
Expected: 编译错误（包不存在）。

- [ ] **Step 3: `limiter.go`**

```go
package sshgw

import (
	"net"
	"sync"
	"time"

	"spark-cloud/internal/platform/clock"
)

const (
	failWindow     = 10 * time.Minute
	blockFor       = 10 * time.Minute
	defaultIPFails = 10
	defaultInstFails = 30
	defaultMaxConns  = 32
)

// limiter 进程内限流：按来源 IP（IPv6 取 /64）计并发连接与失败连接数，按实例计失败连接数。
type limiter struct {
	mu        sync.Mutex
	clk       clock.Clock
	disabled  bool
	ipFails   int
	instFails int
	maxConns  int
	conns     map[string]int
	fails     map[string][]time.Time
	instF     map[string][]time.Time
	blocked   map[string]time.Time
}

func newLimiter(clk clock.Clock, disabled bool, maxConns int) *limiter {
	if maxConns <= 0 {
		maxConns = defaultMaxConns
	}
	return &limiter{clk: clk, disabled: disabled, ipFails: defaultIPFails, instFails: defaultInstFails, maxConns: maxConns,
		conns: map[string]int{}, fails: map[string][]time.Time{}, instF: map[string][]time.Time{}, blocked: map[string]time.Time{}}
}

// ipKey：IPv4 原样；IPv6 取 /64 前缀（轮换地址绕不过封禁）。
func ipKey(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if p.To4() != nil {
		return p.String()
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func prune(ts []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > failWindow {
		i++
	}
	return ts[i:]
}

// acquireConn 在新连接到来时调用：被封禁或并发超限返回 false。
func (l *limiter) acquireConn(ip string) bool {
	if l.disabled {
		return true
	}
	k := ipKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	if until, ok := l.blocked[k]; ok {
		if now.Before(until) {
			return false
		}
		delete(l.blocked, k)
	}
	if l.conns[k] >= l.maxConns {
		return false
	}
	l.conns[k]++
	return true
}

func (l *limiter) releaseConn(ip string) {
	if l.disabled {
		return
	}
	k := ipKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conns[k] <= 1 {
		delete(l.conns, k)
	} else {
		l.conns[k]--
	}
}

// failed 记录一条「关闭时仍未认证成功」的连接；返回该 IP 是否因此被封禁、该实例是否刚达到告警阈值。
func (l *limiter) failed(ip, instance string) (blocked, alert bool) {
	if l.disabled {
		return false, false
	}
	k := ipKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	l.fails[k] = append(prune(l.fails[k], now), now)
	if len(l.fails[k]) >= l.ipFails {
		l.blocked[k] = now.Add(blockFor)
		l.fails[k] = nil
		blocked = true
	}
	if instance != "" {
		l.instF[instance] = append(prune(l.instF[instance], now), now)
		if len(l.instF[instance]) == l.instFails {
			alert = true
		}
	}
	return blocked, alert
}

func (l *limiter) failCount(ip string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(prune(l.fails[ipKey(ip)], l.clk.Now()))
}

func (l *limiter) isBlocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.blocked[ipKey(ip)]
	return ok && l.clk.Now().Before(until)
}
```

- [ ] **Step 4: `auth.go`**

```go
package sshgw

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

const (
	BannerUnknown          = "用户名应为实例 ID，例如 ins-a1b2c3d4"
	BannerSuspended        = "实例所在机器暂时失联，计时已暂停，请稍后重试"
	BannerCreating         = "实例正在开通，请稍后再试"
	BannerStoppedFmt       = "实例已到期停机，数据保留至 %s，续费后可继续使用"
	BannerReleased         = "实例已释放"
	MsgTooManySessions     = "连接数过多"
	MsgUpstreamUnreachable = "无法连接到实例，请稍后重试"
	MsgDisconnected        = "实例已停机 / 已释放，连接已断开"
)

var (
	instanceIDRe = regexp.MustCompile(`^ins-[a-z0-9]{8}$`)
	errAuth      = errors.New("authentication failed")
	cst          = time.FixedZone("CST", 8*3600)
)

const lookupTimeout = 5 * time.Second

// lookup 按用户名查实例；用户名不合法或实例不存在返回 ok=false（二者对客户端不可区分）。
func (s *Server) lookup(user string) (rental.AccessInfo, bool) {
	if !instanceIDRe.MatchString(user) {
		return rental.AccessInfo{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	info, err := s.d.Store.Access(ctx, user)
	if err != nil {
		if !errors.Is(err, apperr.ErrNotFound) {
			s.d.Log.Error("ssh access lookup", "instance", user, "err", err)
		}
		return rental.AccessInfo{}, false
	}
	return info, true
}

// allowed：只有 running 且未暂停的实例可以认证。
func allowed(info rental.AccessInfo) bool {
	return info.Instance.State == rental.StateRunning && !info.Instance.Suspended()
}

// stateBanner 返回认证前给用户看的说明；允许连接时为空。
func stateBanner(info rental.AccessInfo) string {
	inst := info.Instance
	switch {
	case inst.State == rental.StateRunning && inst.Suspended():
		return BannerSuspended
	case inst.State == rental.StateRunning:
		return ""
	case inst.State == rental.StateCreating:
		return BannerCreating
	case inst.State == rental.StateStopped:
		until := "—"
		if info.RetainUntil != nil {
			until = info.RetainUntil.In(cst).Format("2006-01-02 15:04")
		}
		return fmt.Sprintf(BannerStoppedFmt, until)
	default:
		return BannerReleased
	}
}

func (s *Server) banner(c ssh.ConnMetadata) string {
	s.noteUser(c)
	info, ok := s.lookup(c.User())
	if !ok {
		return BannerUnknown + "\r\n"
	}
	if b := stateBanner(info); b != "" {
		return b + "\r\n"
	}
	return ""
}

func perms(info rental.AccessInfo, method string, keyID int64) *ssh.Permissions {
	return &ssh.Permissions{Extensions: map[string]string{
		"instance": info.Instance.ID, "user_id": strconv.FormatInt(info.Instance.UserID, 10), "method": method, "key_id": strconv.FormatInt(keyID, 10),
	}}
}

func (s *Server) passwordCallback(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
	s.noteUser(c)
	info, ok := s.lookup(c.User())
	if !ok || !allowed(info) || !info.Instance.PasswordAuth {
		return nil, errAuth
	}
	if subtle.ConstantTimeCompare([]byte(info.Password), pw) != 1 {
		return nil, errAuth
	}
	return perms(info, "password", 0), nil
}

func (s *Server) publicKeyCallback(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	s.noteUser(c)
	info, ok := s.lookup(c.User())
	if !ok || !allowed(info) {
		return nil, errAuth
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	k, err := s.d.Keys.FindSSHKey(ctx, info.Instance.UserID, ssh.FingerprintSHA256(key))
	if err != nil {
		return nil, errAuth
	}
	return perms(info, "publickey", k.ID), nil
}
```

- [ ] **Step 5: `server.go`**

```go
// Package sshgw 是自研 SSH 网关：用户名 = 实例 ID，密码 / 公钥在此校验，认证通过后用平台上游私钥登录容器 sshd 并透传通道。
package sshgw

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
)

const (
	serverVersion    = "SSH-2.0-SparkCloud"
	maxAuthTries     = 6
	handshakeTimeout = 30 * time.Second
	upstreamTimeout  = 10 * time.Second
	rejectWait       = 2 * time.Second
)

type Store interface {
	Access(ctx context.Context, instanceID string) (rental.AccessInfo, error)
	SetSSHHostPubkeyIfEmpty(ctx context.Context, id, pub string) (bool, error)
	OpenSSHSession(ctx context.Context, st rental.SSHSessionStart) (int64, error)
	TouchSSHSessions(ctx context.Context, ids []int64) error
	CloseSSHSession(ctx context.Context, id, up, down int64) error
	RaiseSSHBruteforce(ctx context.Context, instanceID string) error
	RaiseUpstreamUnreachable(ctx context.Context, instanceID string) error
}

type Keys interface {
	FindSSHKey(ctx context.Context, userID int64, fingerprint string) (identity.SSHKey, error)
	TouchSSHKey(ctx context.Context, id int64) error
}

type Config struct {
	HostKey                ssh.Signer
	Upstream               ssh.Signer
	UpstreamPort           int
	MaxSessionsPerInstance int           // 0 → 16
	MaxConnsPerIP          int           // 0 → 32
	DrainTimeout           time.Duration // 0 → 30s
	DisableRateLimits      bool
}

type Deps struct {
	Store Store
	Keys  Keys
	Clock clock.Clock
	Log   *slog.Logger
}

// session 是一条已认证并连上上游的用户连接。
type session struct {
	id         int64
	instanceID string
	userID     int64
	ip         string
	down       *ssh.ServerConn
	up         ssh.Conn
	bytesUp    atomic.Int64 // 客户端 → 容器
	bytesDown  atomic.Int64 // 容器 → 客户端
	mu         sync.Mutex
	shells     []ssh.Channel // 已打开的 session 通道：强制断开时在这里写提示
}

func (x *session) addShell(ch ssh.Channel) {
	x.mu.Lock()
	x.shells = append(x.shells, ch)
	x.mu.Unlock()
}

// disconnect 向所有 session 通道写提示后关闭两端连接。
func (x *session) disconnect(msg string) {
	x.mu.Lock()
	shells := append([]ssh.Channel(nil), x.shells...)
	x.mu.Unlock()
	for _, ch := range shells {
		ch.Stderr().Write([]byte("\r\n" + msg + "\r\n"))
	}
	x.down.Close()
	x.up.Close()
}

type Server struct {
	cfg    Config
	d      Deps
	sshCfg *ssh.ServerConfig
	lim    *limiter

	pending sync.Map // 远端地址 → 握手期间最近一次出现的用户名（失败连接按实例计数用）

	mu          sync.Mutex
	sessions    map[*session]struct{}
	perInstance map[string]int
	lastUnreach map[string]time.Time // upstream_unreachable 告警的进程内节流（5 分钟）

	keepaliveInterval time.Duration
	wg                sync.WaitGroup
	hostkeysDropped   atomic.Int64
}

func New(cfg Config, d Deps) (*Server, error) {
	if cfg.HostKey == nil || cfg.Upstream == nil || cfg.UpstreamPort <= 0 {
		return nil, errors.New("sshgw: HostKey, Upstream and UpstreamPort are required")
	}
	if d.Store == nil || d.Keys == nil || d.Clock == nil {
		return nil, errors.New("sshgw: missing deps")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if cfg.MaxSessionsPerInstance <= 0 {
		cfg.MaxSessionsPerInstance = 16
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 30 * time.Second
	}
	s := &Server{cfg: cfg, d: d, sessions: map[*session]struct{}{}, perInstance: map[string]int{}, lastUnreach: map[string]time.Time{},
		keepaliveInterval: keepaliveInterval}
	s.lim = newLimiter(d.Clock, cfg.DisableRateLimits, cfg.MaxConnsPerIP)
	s.sshCfg = &ssh.ServerConfig{
		ServerVersion:     serverVersion,
		MaxAuthTries:      maxAuthTries,
		BannerCallback:    s.banner,
		PasswordCallback:  s.passwordCallback,
		PublicKeyCallback: s.publicKeyCallback,
	}
	s.sshCfg.AddHostKey(cfg.HostKey)
	return s, nil
}

// Sessions 返回当前已建立（已连上上游）的会话数。
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// Serve 接受连接直到 ctx 结束；随后停止 accept，最多等 DrainTimeout 让会话自然结束，再强制关闭。
// 每 10 秒扫描会话状态、每 60 秒刷新会话 last_seen_at（Task 11 的 runLoops）。
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	loopCtx, stopLoops := context.WithCancel(ctx)
	defer stopLoops()
	go s.runLoops(loopCtx)
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			nc, err := lis.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go s.handleConn(nc)
		}
	}()
	<-ctx.Done()
	lis.Close()
	<-accepted
	s.drain()
	return nil
}

func remoteIP(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

func (s *Server) noteUser(c ssh.ConnMetadata) { s.pending.Store(c.RemoteAddr().String(), c.User()) }

func (s *Server) handleConn(nc net.Conn) {
	defer s.wg.Done()
	ip := remoteIP(nc.RemoteAddr())
	if !s.lim.acquireConn(ip) {
		nc.Close()
		return
	}
	defer s.lim.releaseConn(ip)
	key := nc.RemoteAddr().String()
	s.pending.Store(key, "")
	defer s.pending.Delete(key)
	nc.SetDeadline(time.Now().Add(handshakeTimeout))
	down, chans, reqs, err := ssh.NewServerConn(nc, s.sshCfg)
	if err != nil {
		user, _ := s.pending.Load(key)
		s.authFailed(ip, user.(string))
		nc.Close()
		return
	}
	nc.SetDeadline(time.Time{})
	defer down.Close()
	s.serve(down, chans, reqs, ip)
}

// authFailed：一条连接关闭时仍未认证成功 → 按 IP 与实例各计 1 次。
func (s *Server) authFailed(ip, user string) {
	instance := ""
	if instanceIDRe.MatchString(user) {
		instance = user
	}
	blocked, alert := s.lim.failed(ip, instance)
	s.d.Log.Warn("ssh auth failed", "ip", ip, "user", user, "ip_blocked", blocked)
	if alert {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		if err := s.d.Store.RaiseSSHBruteforce(ctx, instance); err != nil {
			s.d.Log.Error("raise ssh_bruteforce", "instance", instance, "err", err)
		}
	}
}

func (s *Server) acquireInstance(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perInstance[id] >= s.cfg.MaxSessionsPerInstance {
		return false
	}
	s.perInstance[id]++
	return true
}

func (s *Server) releaseInstance(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perInstance[id] <= 1 {
		delete(s.perInstance, id)
	} else {
		s.perInstance[id]--
	}
}

// serve：认证通过后的一条连接。会话上限、再次状态校验、拨上游、记录会话、中继、收尾。
func (s *Server) serve(down *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, ip string) {
	ext := down.Permissions.Extensions
	instanceID := ext["instance"]
	userID, _ := strconv.ParseInt(ext["user_id"], 10, 64)
	keyID, _ := strconv.ParseInt(ext["key_id"], 10, 64)
	method := ext["method"]
	// 全局请求（reqs）不能在这里丢弃：它们由 relay 按白名单转发给上游
	if !s.acquireInstance(instanceID) {
		rejectWithMessage(chans, MsgTooManySessions)
		return
	}
	defer s.releaseInstance(instanceID)
	info, ok := s.lookup(instanceID)
	if !ok || !allowed(info) {
		msg := BannerReleased
		if ok {
			msg = stateBanner(info)
		}
		rejectWithMessage(chans, msg)
		return
	}
	up, upChans, upReqs, err := s.dialUpstream(info)
	if err != nil {
		s.d.Log.Warn("ssh upstream dial failed", "instance", instanceID, "err", err)
		s.alertUnreachable(instanceID)
		rejectWithMessage(chans, MsgUpstreamUnreachable)
		return
	}
	sess := &session{instanceID: instanceID, userID: userID, ip: ip, down: down, up: up}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	start := rental.SSHSessionStart{InstanceID: instanceID, UserID: userID, MachineID: info.Instance.MachineID, AuthMethod: method, ClientIP: ip}
	if method == "publickey" && keyID != 0 {
		start.KeyID = &keyID
		if err := s.d.Keys.TouchSSHKey(ctx, keyID); err != nil {
			s.d.Log.Error("touch ssh key", "key", keyID, "err", err)
		}
	}
	if sess.id, err = s.d.Store.OpenSSHSession(ctx, start); err != nil {
		s.d.Log.Error("open ssh session record", "instance", instanceID, "err", err)
	}
	cancel()
	s.mu.Lock()
	s.sessions[sess] = struct{}{}
	s.mu.Unlock()
	started := time.Now()
	s.d.Log.Info("ssh session open", "instance", instanceID, "user", userID, "method", method, "ip", ip)
	s.relay(sess, down, chans, reqs, up, upChans, upReqs)
	s.mu.Lock()
	delete(s.sessions, sess)
	s.mu.Unlock()
	if sess.id != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		if err := s.d.Store.CloseSSHSession(ctx, sess.id, sess.bytesUp.Load(), sess.bytesDown.Load()); err != nil {
			s.d.Log.Error("close ssh session record", "session", sess.id, "err", err)
		}
		cancel()
	}
	s.d.Log.Info("ssh session close", "instance", instanceID, "user", userID, "ip", ip, "duration", time.Since(started).Round(time.Second),
		"bytes_up", sess.bytesUp.Load(), "bytes_down", sess.bytesDown.Load())
}

// dialUpstream 用平台上游私钥连容器 sshd，主机密钥固定为 instances.ssh_host_pubkey（为空时首连信任写回）。
func (s *Server) dialUpstream(info rental.AccessInfo) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	addr := net.JoinHostPort(info.MachineLanIP, strconv.Itoa(s.cfg.UpstreamPort))
	nc, err := net.DialTimeout("tcp", addr, upstreamTimeout)
	if err != nil {
		return nil, nil, nil, err
	}
	tofu := func(pub string) error {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		ok, err := s.d.Store.SetSSHHostPubkeyIfEmpty(ctx, info.Instance.ID, pub)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("主机公钥已由其它副本写回，请重试")
		}
		s.d.Log.Warn("upstream host key trusted on first use", "instance", info.Instance.ID)
		return nil
	}
	cfg := upstream.ClientConfig(s.cfg.Upstream, upstream.HostKeyCallback(info.Instance.SSHHostPubkey, tofu), upstreamTimeout)
	up, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	if err != nil {
		nc.Close()
		return nil, nil, nil, err
	}
	return up, chans, reqs, nil
}

// alertUnreachable 进程内按实例 5 分钟节流后 RaiseAlertOnce。
func (s *Server) alertUnreachable(instanceID string) {
	now := s.d.Clock.Now()
	s.mu.Lock()
	if last, ok := s.lastUnreach[instanceID]; ok && now.Sub(last) < 5*time.Minute {
		s.mu.Unlock()
		return
	}
	s.lastUnreach[instanceID] = now
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	if err := s.d.Store.RaiseUpstreamUnreachable(ctx, instanceID); err != nil {
		s.d.Log.Error("raise upstream_unreachable", "instance", instanceID, "err", err)
	}
}

// rejectWithMessage：等客户端打开第一个 session 通道（最多 rejectWait），把提示写到 stderr，退出码 1，然后结束。
// 非 session 通道一律拒绝。
func rejectWithMessage(chans <-chan ssh.NewChannel, msg string) {
	timer := time.NewTimer(rejectWait)
	defer timer.Stop()
	for {
		select {
		case nc, ok := <-chans:
			if !ok {
				return
			}
			if nc.ChannelType() != "session" {
				nc.Reject(ssh.Prohibited, msg)
				continue
			}
			ch, reqs, err := nc.Accept()
			if err != nil {
				return
			}
			go func() {
				for req := range reqs {
					if req.WantReply {
						req.Reply(req.Type == "pty-req" || req.Type == "shell" || req.Type == "exec" || req.Type == "env", nil)
					}
				}
			}()
			ch.Stderr().Write([]byte(msg + "\r\n"))
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
			ch.Close()
			return
		case <-timer.C:
			return
		}
	}
}
```

- [ ] **Step 6: `relay.go`（策略与透传；保活在 Task 10 补）**

```go
package sshgw

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	keepaliveInterval = 60 * time.Second
	keepaliveMisses   = 3
	keepaliveTimeout  = 15 * time.Second
)

// 策略：白名单 + 默认拒绝。网关不解析任何通道 / 请求的载荷，原样转给对端。
func clientChannelAllowed(t string) bool   { return t == "session" || t == "direct-tcpip" }
func upstreamChannelAllowed(t string) bool { return t == "forwarded-tcpip" || t == "auth-agent@openssh.com" }

var globalAllowed = map[string]bool{"tcpip-forward": true, "cancel-tcpip-forward": true, "keepalive@openssh.com": true, "no-more-sessions@openssh.com": true}

// relay 双向转发直到任一侧连接结束。
func (s *Server) relay(sess *session, down *ssh.ServerConn, downChans <-chan ssh.NewChannel, downReqs <-chan *ssh.Request,
	up ssh.Conn, upChans <-chan ssh.NewChannel, upReqs <-chan *ssh.Request) {
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); forwardGlobal(downReqs, up, nil) }()
	go func() {
		defer wg.Done()
		forwardGlobal(upReqs, down, func(name string) {
			if name == "hostkeys-00@openssh.com" {
				s.hostkeysDropped.Add(1)
			}
		})
	}()
	go func() {
		defer wg.Done()
		for nc := range downChans {
			s.openOn(sess, nc, up, true)
		}
	}()
	go func() {
		defer wg.Done()
		for nc := range upChans {
			s.openOn(sess, nc, down, false)
		}
	}()
	stop := make(chan struct{})
	go s.keepalive(down, stop)
	go s.keepalive(up, stop)
	ended := make(chan struct{}, 2)
	go func() { down.Wait(); ended <- struct{}{} }()
	go func() { up.Wait(); ended <- struct{}{} }()
	<-ended
	close(stop)
	down.Close()
	up.Close()
	wg.Wait()
}

// forwardGlobal 转发全局请求；不在白名单的回 false（onDrop 用于计数）。
func forwardGlobal(in <-chan *ssh.Request, out ssh.Conn, onDrop func(name string)) {
	for req := range in {
		if !globalAllowed[req.Type] {
			if onDrop != nil {
				onDrop(req.Type)
			}
			if req.WantReply {
				req.Reply(false, nil)
			}
			continue
		}
		ok, payload, err := out.SendRequest(req.Type, req.WantReply, req.Payload)
		if req.WantReply {
			req.Reply(ok && err == nil, payload)
		}
	}
}

// openOn 把一侧的 NewChannel 原样开到另一侧；fromClient 决定白名单与字节计数方向。
func (s *Server) openOn(sess *session, nc ssh.NewChannel, other ssh.Conn, fromClient bool) {
	t := nc.ChannelType()
	if (fromClient && !clientChannelAllowed(t)) || (!fromClient && !upstreamChannelAllowed(t)) {
		reason := ssh.Prohibited
		if fromClient {
			reason = ssh.UnknownChannelType
		}
		nc.Reject(reason, "channel type not allowed: "+t)
		return
	}
	och, oreqs, err := other.OpenChannel(t, nc.ExtraData())
	if err != nil {
		var oce *ssh.OpenChannelError
		if errors.As(err, &oce) {
			nc.Reject(oce.Reason, oce.Message)
		} else {
			nc.Reject(ssh.ConnectionFailed, err.Error())
		}
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		och.Close()
		return
	}
	if fromClient && t == "session" {
		sess.addShell(ch)
	}
	if fromClient {
		go pipeChannels(ch, reqs, och, oreqs, &sess.bytesUp, &sess.bytesDown)
	} else {
		go pipeChannels(och, oreqs, ch, reqs, &sess.bytesUp, &sess.bytesDown)
	}
}

// pipeChannels：a 是客户端侧、b 是容器侧。数据与 stderr 双向拷贝，通道内请求双向转发（含 exit-status、eow）；
// 一侧的请求流结束（即该侧通道已关闭）就关闭另一侧。
func pipeChannels(a ssh.Channel, aReqs <-chan *ssh.Request, b ssh.Channel, bReqs <-chan *ssh.Request, up, down *atomic.Int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); forwardChannelRequests(aReqs, b); b.Close() }()
	go func() { defer wg.Done(); forwardChannelRequests(bReqs, a); a.Close() }()
	go func() { n, _ := io.Copy(b, a); up.Add(n); b.CloseWrite() }()
	go func() { n, _ := io.Copy(a, b); down.Add(n); a.CloseWrite() }()
	go func() { n, _ := io.Copy(b.Stderr(), a.Stderr()); up.Add(n) }()
	go func() { n, _ := io.Copy(a.Stderr(), b.Stderr()); down.Add(n) }()
	wg.Wait()
}

func forwardChannelRequests(in <-chan *ssh.Request, out ssh.Channel) {
	for req := range in {
		ok, err := out.SendRequest(req.Type, req.WantReply, req.Payload)
		if req.WantReply {
			req.Reply(ok && err == nil, nil)
		}
	}
}

// keepalive 每 keepaliveInterval 发一次 keepalive@openssh.com，连续 keepaliveMisses 次无回应即关闭连接。
// 对端回 false（OpenSSH 对未知请求的回复）也算活着；只有发送出错或超时才算未回应。
func (s *Server) keepalive(c ssh.Conn, stop <-chan struct{}) {
	t := time.NewTicker(s.keepaliveInterval)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			res := make(chan error, 1)
			go func() { _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); res <- err }()
			select {
			case err := <-res:
				if err == nil {
					misses = 0
					continue
				}
			case <-time.After(keepaliveTimeout):
			}
			if misses++; misses >= keepaliveMisses {
				c.Close()
				return
			}
		}
	}
}
```

`internal/sshgw/export_test.go`：

```go
package sshgw

import "time"

func SetLimitsForTest(s *Server, ipFails, instFails, maxConns int) {
	s.lim.mu.Lock()
	defer s.lim.mu.Unlock()
	s.lim.ipFails, s.lim.instFails, s.lim.maxConns = ipFails, instFails, maxConns
}

func FailCountForTest(s *Server, ip string) int { return s.lim.failCount(ip) }
func BlockedForTest(s *Server, ip string) bool  { return s.lim.isBlocked(ip) }

// SetKeepaliveForTest 调短保活间隔（必须在 Serve 之前调用）。
func SetKeepaliveForTest(s *Server, d time.Duration) { s.keepaliveInterval = d }

func HostkeysDroppedForTest(s *Server) int64 { return s.hostkeysDropped.Load() }
```

Task 11 之前 `runLoops` 与 `drain` 先按最简实现放在 `server.go` 末尾（Task 11 扩充成扫描 + 刷新）：

```go
// runLoops：Task 11 填充扫描与刷新；本任务只等待 ctx 结束。
func (s *Server) runLoops(ctx context.Context) { <-ctx.Done() }

// drain：最多等 DrainTimeout 让会话自然结束，然后强制断开剩余会话。
func (s *Server) drain() {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(s.cfg.DrainTimeout):
	}
	s.mu.Lock()
	all := make([]*session, 0, len(s.sessions))
	for x := range s.sessions {
		all = append(all, x)
	}
	s.mu.Unlock()
	for _, x := range all {
		x.disconnect("网关正在重启，连接已断开")
	}
	<-done
}
```

- [ ] **Step 7: 运行测试确认通过**

Run: `go vet ./internal/sshgw/ && go test ./internal/sshgw/ -race 2>&1 | tail -5`
Expected: 全部 `ok`。`TestFailureCountingIsPerConnection` 依赖 `BannerCallback` 在每次认证前都会被调用一次（`x/crypto/ssh` 在首次认证请求前调用 Banner），`noteUser` 在 Banner 与两种认证回调里都记录用户名，所以失败连接总能对应到实例。

- [ ] **Step 8: 提交**

```bash
git add internal/sshgw
git commit -m "feat(sshgw): ssh gateway core — banner by instance state, password/public key auth, per-connection brute-force counting, session records, channel relay

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 10: `sshgw` 中继矩阵：`-L` / `-R` / agent / sftp / 退出码 / 拒绝类型 / `hostkeys-00` 丢弃 / 原样透传 / 上游中断 / 保活

**Files:**
- Create: `internal/sshgw/relay_test.go`
- Modify: `internal/sshgw/export_test.go`（`SetKeepaliveForTest` 增加 timeout 参数）、`internal/sshgw/relay.go`（`keepaliveTimeout` 改为 Server 字段）

**Interfaces:**
- Consumes: Task 9 的全部；Task 7 `sshtest` 的 `ServeHTTP` / `ForwardAddr` / `OpenToClient` / `LastDirectTCPIP` / `DropAll`。
- Produces: `SetKeepaliveForTest(s *Server, interval, timeout time.Duration)`；`Server.keepaliveTimeout` 字段（默认 15s）。

- [ ] **Step 1: 写失败的测试**

`internal/sshgw/relay_test.go`：

```go
package sshgw_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"spark-cloud/internal/sshgw"
)

type directMsg struct {
	Raddr string
	Rport uint32
	Laddr string
	Lport uint32
}

func TestLocalForwardPassesExtraDataVerbatim(t *testing.T) {
	e := newEnv(t)
	stop := e.upstream.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "lab") }))
	defer stop()
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	payload := ssh.Marshal(directMsg{Raddr: "127.0.0.1", Rport: 8888, Laddr: "203.0.113.7", Lport: 51515})
	ch, reqs, err := c.OpenChannel("direct-tcpip", payload)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	fmt.Fprint(ch, "GET / HTTP/1.0\r\nHost: x\r\n\r\n")
	body, _ := io.ReadAll(ch)
	if !strings.HasSuffix(string(body), "lab") {
		t.Fatalf("body = %q", body)
	}
	if got := e.upstream.LastDirectTCPIP(); !bytes.Equal(got, payload) {
		t.Fatalf("extra data rewritten: got %x want %x", got, payload)
	}
	// 上游拒绝（未登记端口）的原因原样回传
	_, _, err = c.OpenChannel("direct-tcpip", ssh.Marshal(directMsg{Raddr: "127.0.0.1", Rport: 9, Laddr: "x", Lport: 1}))
	var oce *ssh.OpenChannelError
	if !errors.As(err, &oce) || oce.Reason != ssh.ConnectionFailed || !strings.Contains(oce.Message, "refused") {
		t.Fatalf("rejected = %v", err)
	}
}

func TestRemoteForwardAndAgentForwarding(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	ln, err := c.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(conn, conn)
		conn.Close()
	}()
	back, err := net.Dial("tcp", e.upstream.ForwardAddr())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(back, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(back, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("remote forward echo = %q %v", buf, err)
	}
	back.Close()

	keyring := agent.NewKeyring()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	keyring.Add(agent.AddedKey{PrivateKey: priv})
	if err := agent.ForwardToAgent(c, keyring); err != nil {
		t.Fatal(err)
	}
	sess, _ := c.NewSession()
	if err := agent.RequestAgentForwarding(sess); err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("agent")
	if err != nil || string(out) != "agent:1\n" {
		t.Fatalf("agent through gateway = %q %v", out, err)
	}
}

func TestSftpExitStatusAndStderr(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	sess, _ := c.NewSession()
	var out bytes.Buffer
	sess.Stdout = &out
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Wait(); err != nil || out.String() != "SFTP-FAKE\n" {
		t.Fatalf("sftp = %q %v", out.String(), err)
	}
	var exitErr *ssh.ExitError
	if _, _, err := run(t, c, "exit 3"); !errors.As(err, &exitErr) || exitErr.ExitStatus() != 3 {
		t.Fatalf("exit status = %v", err)
	}
	if _, stderr, err := run(t, c, "stderr oops"); err != nil || stderr != "oops" {
		t.Fatalf("stderr = %q %v", stderr, err)
	}
	sess2, _ := c.NewSession()
	sess2.Stdin = strings.NewReader(strings.Repeat("x", 100000))
	var big bytes.Buffer
	sess2.Stdout = &big
	if err := sess2.Run("cat"); err != nil || big.Len() != 100000 {
		t.Fatalf("cat 100k = %d %v", big.Len(), err)
	}
	c.Close()
	waitFor(t, "closed", func() bool { _, closed, _ := e.store.snapshot(); return len(closed) == 1 })
	_, closed, _ := e.store.snapshot()
	if b := closed[1]; b[0] < 100000 || b[1] < 100000 {
		t.Fatalf("bytes up/down = %v", b)
	}
}

func TestPolicyRejectsAndDropsHostkeys(t *testing.T) {
	e := newEnv(t)
	nc, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, chans, reqs, err := ssh.NewClientConn(nc, e.addr, &ssh.ClientConfig{User: instA, Auth: []ssh.AuthMethod{ssh.Password("pw-correct-16chr")},
		HostKeyCallback: ssh.FixedHostKey(e.hostKey.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var sawHostkeys atomic.Bool
	go func() {
		for r := range reqs {
			if r.Type == "hostkeys-00@openssh.com" {
				sawHostkeys.Store(true)
			}
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
	}()
	c := ssh.NewClient(conn, chans, nil)
	defer c.Close()
	var oce *ssh.OpenChannelError
	for _, typ := range []string{"direct-streamlocal@openssh.com", "tun@openssh.com", "x11", "session-bogus"} {
		if _, _, err := c.OpenChannel(typ, nil); !errors.As(err, &oce) || oce.Reason != ssh.UnknownChannelType {
			t.Fatalf("client %s = %v", typ, err)
		}
	}
	if ok, _, err := c.SendRequest("streamlocal-forward@openssh.com", true, nil); ok || err != nil {
		t.Fatalf("streamlocal-forward = %v %v", ok, err)
	}
	if ok, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("keepalive forwarded = %v %v", ok, err)
	}
	// 需要一条活的会话让上游有连接可用
	out, err := func() (string, error) {
		s, err := c.NewSession()
		if err != nil {
			return "", err
		}
		defer s.Close()
		b, err := s.Output("hostkeys")
		return string(b), err
	}()
	if err != nil || out != "sent\n" {
		t.Fatalf("hostkeys exec = %q %v", out, err)
	}
	for _, typ := range []string{"x11", "forwarded-streamlocal@openssh.com", "session"} {
		if err := e.upstream.OpenToClient(typ, nil); !errors.As(err, &oce) || oce.Reason != ssh.Prohibited {
			t.Fatalf("upstream %s = %v", typ, err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if sawHostkeys.Load() || sshgw.HostkeysDroppedForTest(e.srv) != 1 {
		t.Fatalf("hostkeys-00 leaked to client (saw=%v dropped=%d)", sawHostkeys.Load(), sshgw.HostkeysDroppedForTest(e.srv))
	}
}

func TestUpstreamDropEndsSession(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	sess, _ := c.NewSession()
	in, _ := sess.StdinPipe()
	out, _ := sess.StdoutPipe()
	if err := sess.Start("cat"); err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(in, "live")
	buf := make([]byte, 4)
	io.ReadFull(out, buf)
	e.upstream.DropAll()
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("session ended without error after upstream drop")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end after upstream drop")
	}
	waitFor(t, "session record closed", func() bool { _, closed, _ := e.store.snapshot(); return len(closed) == 1 && e.srv.Sessions() == 0 })
	_, closed, _ := e.store.snapshot()
	if closed[1][0] != 4 || closed[1][1] != 4 {
		t.Fatalf("bytes = %v", closed[1])
	}
}

// freezer 是客户端与网关之间的 TCP 中继，可随时停止转发（模拟客户端网络断掉但 TCP 未关闭）。
type freezer struct {
	lis    net.Listener
	frozen atomic.Bool
}

func newFreezer(t *testing.T, target string) *freezer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &freezer{lis: lis}
	go func() {
		for {
			a, err := lis.Accept()
			if err != nil {
				return
			}
			b, err := net.Dial("tcp", target)
			if err != nil {
				a.Close()
				continue
			}
			pump := func(dst, src net.Conn) {
				buf := make([]byte, 32*1024)
				for {
					n, err := src.Read(buf)
					if err != nil {
						return
					}
					for f.frozen.Load() {
						time.Sleep(10 * time.Millisecond)
					}
					if _, err := dst.Write(buf[:n]); err != nil {
						return
					}
				}
			}
			go pump(a, b)
			go pump(b, a)
		}
	}()
	t.Cleanup(func() { lis.Close() })
	return f
}

func TestKeepaliveClosesFrozenClient(t *testing.T) {
	e := newEnvWith(t, nil)
	sshgw.SetKeepaliveForTest(e.srv, 50*time.Millisecond, 100*time.Millisecond)
	fz := newFreezer(t, e.addr)
	c, err := ssh.Dial("tcp", fz.lis.Addr().String(), &ssh.ClientConfig{User: instA, Auth: []ssh.AuthMethod{ssh.Password("pw-correct-16chr")},
		HostKeyCallback: ssh.FixedHostKey(e.hostKey.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if out, _, err := run(t, c, "echo alive"); err != nil || out != "alive\n" {
		t.Fatalf("before freeze = %q %v", out, err)
	}
	time.Sleep(300 * time.Millisecond) // 多个保活周期，健康连接不被误杀
	if e.srv.Sessions() != 1 {
		t.Fatalf("healthy session closed by keepalive: %d", e.srv.Sessions())
	}
	fz.frozen.Store(true)
	waitFor(t, "frozen client dropped", func() bool { return e.srv.Sessions() == 0 })
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/sshgw/ -run 'TestKeepaliveClosesFrozenClient' 2>&1 | head -5`
Expected: 编译错误（`SetKeepaliveForTest` 参数不符）。其余用例应已通过或暴露中继缺陷——先跑一遍全部：`go test ./internal/sshgw/ 2>&1 | tail -15`，把失败的记下来逐个修。

- [ ] **Step 3: 保活超时可注入**

`internal/sshgw/relay.go`：删掉常量 `keepaliveTimeout`，`Server` 增加字段 `keepaliveTimeout time.Duration`（`New` 里设为 `15 * time.Second`），`keepalive` 里 `time.After(keepaliveTimeout)` 改为 `time.After(s.keepaliveTimeout)`。`export_test.go`：

```go
// SetKeepaliveForTest 调短保活间隔与等待回应的超时（必须在 Serve 之前调用）。
func SetKeepaliveForTest(s *Server, interval, timeout time.Duration) {
	s.keepaliveInterval, s.keepaliveTimeout = interval, timeout
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./internal/sshgw/ && go test ./internal/sshgw/ -race -count=2 2>&1 | tail -5`
Expected: 全部 `ok`，两次（排除偶发）。若 `TestUpstreamDropEndsSession` 偶发超时：检查 `pipeChannels` 里两个 `forwardChannelRequests` 结束后是否都执行了对侧 `Close()`（上游连接断开时 `oreqs` 关闭 → `a.Close()` 关闭客户端通道 → `sess.Wait` 返回）。

- [ ] **Step 5: 提交**

```bash
git add internal/sshgw
git commit -m "test(sshgw): relay matrix (local/remote forward, agent, sftp, exit status, policy rejects, hostkeys-00 drop, upstream drop, keepalive)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 11: `sshgw` 生命周期（状态扫描断开、`last_seen_at` 刷新、drain）+ 组装进 `app` / `cmd/platform`

**Files:**
- Create: `internal/sshgw/lifecycle.go`、`internal/sshgw/lifecycle_test.go`
- Modify: `internal/sshgw/server.go`（删除临时 `runLoops`，`drain` 移到 lifecycle.go）、`internal/sshgw/export_test.go`
- Modify: `internal/pki/upstream.go`（`LoadUpstreamSigner`）、`internal/pki/upstream_test.go`
- Modify: `internal/app/app.go`、`cmd/platform/main.go`

**Interfaces:**
- Produces:
  - `(*Server).ScanNow(ctx)`：对每条会话重新 `Access`；实例不存在或 `State ∈ {stopped, wiping, released, failed}` → `disconnect(MsgDisconnected)`；暂停不断。`(*Server).TouchNow(ctx)`：`TouchSSHSessions(全部会话 id)`。`runLoops` 每 10 秒 `ScanNow`、每 60 秒 `TouchNow`。
  - `pki.LoadUpstreamSigner(path string) (ssh.Signer, error)`（权限检查同 `LoadUpstreamPublicKey`）。
  - `app.App` 新字段 `SSH *sshgw.Server`、`SSHHostKey ssh.Signer`、`UpstreamSigner ssh.Signer`；`app.Build` 用 `pki.EnsureSSHHostKey` 与 `cfg.UpstreamSSHPort` 组装 `sshgw`（`DisableRateLimits` 透传；`rental.Deps.Keys = ids`）。
  - `cmd/platform`：在 `cfg.SSHAddr` 监听并 `a.SSH.Serve(ctx, lis)`；退出时先 `stop()` 再等它返回（内部按 `SSHDrainTimeout` drain）。

- [ ] **Step 1: 写失败的测试**

`internal/sshgw/lifecycle_test.go`：

```go
package sshgw_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/rental"
	"spark-cloud/internal/sshgw"
)

func TestScanDisconnectsStoppedButKeepsSuspended(t *testing.T) {
	e := newEnv(t)
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	sess, _ := c.NewSession()
	var stderr strings.Builder
	sess.Stderr = &stderr
	in, _ := sess.StdinPipe()
	if err := sess.Start("cat"); err != nil {
		t.Fatal(err)
	}
	// 暂停：不断
	info := runningInfo(e, instA, 7, "pw-correct-16chr")
	now := e.clk.Now()
	info.Instance.SuspendedAt = &now
	e.store.set(instA, info)
	e.srv.ScanNow(context.Background())
	time.Sleep(100 * time.Millisecond)
	if e.srv.Sessions() != 1 {
		t.Fatal("suspended instance's session was disconnected")
	}
	// 停机：10 秒扫描断开并提示
	info = runningInfo(e, instA, 7, "pw-correct-16chr")
	info.Instance.State = rental.StateStopped
	e.store.set(instA, info)
	e.srv.ScanNow(context.Background())
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("session not closed after stop")
	}
	in.Close()
	if !strings.Contains(stderr.String(), sshgw.MsgDisconnected) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	waitFor(t, "sessions 0", func() bool { return e.srv.Sessions() == 0 })
}

func TestTouchRefreshesAllSessions(t *testing.T) {
	e := newEnv(t)
	a := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	b := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	if _, _, err := run(t, a, "echo 1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, b, "echo 2"); err != nil {
		t.Fatal(err)
	}
	e.srv.TouchNow(context.Background())
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if len(e.store.touched) != 1 || len(e.store.touched[0]) != 2 {
		t.Fatalf("touched = %v", e.store.touched)
	}
}

func TestDrainClosesRemainingSessions(t *testing.T) {
	e := newEnvWith(t, func(c *sshgw.Config) { c.DrainTimeout = 200 * time.Millisecond })
	c := e.mustDial(instA, ssh.Password("pw-correct-16chr"))
	sess, _ := c.NewSession()
	var stderr strings.Builder
	sess.Stderr = &stderr
	if err := sess.Start("cat"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 3*time.Second {
		t.Fatalf("drain took %v", d)
	}
	if err := sess.Wait(); err == nil {
		t.Fatal("session survived drain")
	}
	if !strings.Contains(stderr.String(), "网关正在重启") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
```

`internal/pki/upstream_test.go` 追加：

```go
func TestLoadUpstreamSigner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "up.key")
	if err := pki.GenerateUpstreamKey(path); err != nil {
		t.Fatal(err)
	}
	s, err := pki.LoadUpstreamSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := pki.LoadUpstreamPublicKey(path)
	if strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) != pub {
		t.Fatal("signer public key differs from LoadUpstreamPublicKey")
	}
	os.Chmod(path, 0o644)
	if _, err := pki.LoadUpstreamSigner(path); err == nil || !strings.Contains(err.Error(), "权限") {
		t.Fatalf("loose perm = %v", err)
	}
}
```

（import 追加 `"golang.org/x/crypto/ssh"`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/sshgw/ ./internal/pki/ -run 'TestScan|TestTouch|TestDrain|TestLoadUpstreamSigner' 2>&1 | head`
Expected: 编译错误（`ScanNow` / `TouchNow` / `LoadUpstreamSigner` 未定义）。

- [ ] **Step 3: `lifecycle.go`**

把 `server.go` 末尾临时的 `runLoops` 与 `drain` 删掉，新建：

```go
package sshgw

import (
	"context"
	"time"

	"spark-cloud/internal/rental"
)

const (
	scanInterval  = 10 * time.Second
	touchInterval = 60 * time.Second
)

func (s *Server) snapshotSessions() []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*session, 0, len(s.sessions))
	for x := range s.sessions {
		out = append(out, x)
	}
	return out
}

// ScanNow：实例离开 running（到期停机、释放、换机后的旧实例）→ 提示并断开；暂停（机器失联）不断。
func (s *Server) ScanNow(ctx context.Context) {
	for _, x := range s.snapshotSessions() {
		info, err := s.d.Store.Access(ctx, x.instanceID)
		terminal := err != nil
		if err == nil {
			switch info.Instance.State {
			case rental.StateStopped, rental.StateWiping, rental.StateReleased, rental.StateFailed:
				terminal = true
			}
		}
		if terminal {
			s.d.Log.Info("ssh session disconnected: instance no longer running", "instance", x.instanceID, "user", x.userID)
			x.disconnect(MsgDisconnected)
		}
	}
}

// TouchNow 批量刷新会话 last_seen_at（清扫器据此收尾崩溃残留）。
func (s *Server) TouchNow(ctx context.Context) {
	var ids []int64
	for _, x := range s.snapshotSessions() {
		if x.id != 0 {
			ids = append(ids, x.id)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := s.d.Store.TouchSSHSessions(ctx, ids); err != nil {
		s.d.Log.Error("touch ssh sessions", "err", err)
	}
}

func (s *Server) runLoops(ctx context.Context) {
	scan := time.NewTicker(scanInterval)
	touch := time.NewTicker(touchInterval)
	defer scan.Stop()
	defer touch.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-scan.C:
			c, cancel := context.WithTimeout(context.Background(), lookupTimeout)
			s.ScanNow(c)
			cancel()
		case <-touch.C:
			c, cancel := context.WithTimeout(context.Background(), lookupTimeout)
			s.TouchNow(c)
			cancel()
		}
	}
}

// drain：最多等 DrainTimeout 让会话自然结束，然后强制断开剩余会话（部署会断 SSH，README 注明）。
func (s *Server) drain() {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(s.cfg.DrainTimeout):
	}
	for _, x := range s.snapshotSessions() {
		x.disconnect("网关正在重启，连接已断开")
	}
	<-done
}
```

- [ ] **Step 4: `pki.LoadUpstreamSigner`**

`internal/pki/upstream.go` 追加：

```go
// LoadUpstreamSigner 读取上游私钥供网关 / 入口登录容器 sshd（权限检查同 LoadUpstreamPublicKey）。
func LoadUpstreamSigner(path string) (ssh.Signer, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("上游 SSH 私钥 %s 权限过宽（%o），请 chmod 600", path, st.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("解析上游 SSH 私钥: %w", err)
	}
	if s.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return nil, errors.New("上游 SSH 私钥必须是 ed25519")
	}
	return s, nil
}
```

- [ ] **Step 5: 组装进 `app` 与 `cmd/platform`**

`internal/app/app.go`：
- `Build` 第一行加 `cfg = cfg.FillAccessDefaults()`（测试里的 `config.Config{…}` 字面量没有 `UpstreamSSHPort` 等字段，`sshgw.New` 会因 `UpstreamPort <= 0` 报错）。
- `App` 追加字段：

```go
	SSH            *sshgw.Server // SSH 网关；由 cmd/platform 在 SPARK_SSH_ADDR 上监听
	SSHHostKey     ssh.Signer
	UpstreamSigner ssh.Signer
```

- `upstreamPublicKey(cfg, log)` 改名为 `upstreamKeyPath(cfg, log) (string, error)`，只负责「缺省时生成临时密钥并返回路径」；`Build` 里：

```go
	keyPath, err := upstreamKeyPath(cfg, log) // 先于 EnsureCA：配置错误不应留下任何数据库写入
	if err != nil {
		return nil, err
	}
	upstreamPub, err := pki.LoadUpstreamPublicKey(keyPath)
	if err != nil {
		return nil, fmt.Errorf("读取上游 SSH 密钥: %w", err)
	}
	upstreamSigner, err := pki.LoadUpstreamSigner(keyPath)
	if err != nil {
		return nil, fmt.Errorf("读取上游 SSH 密钥: %w", err)
	}
```

- `rental.NewService(rental.Deps{...})` 增加 `Keys: ids`。
- `EnsureCA` 之后：

```go
	sshHostKey, err := pki.EnsureSSHHostKey(context.Background(), pool, box)
	if err != nil {
		return nil, fmt.Errorf("初始化 SSH 网关主机密钥: %w", err)
	}
	sshSrv, err := sshgw.New(sshgw.Config{HostKey: sshHostKey, Upstream: upstreamSigner, UpstreamPort: cfg.UpstreamSSHPort,
		MaxSessionsPerInstance: cfg.SSHMaxSessions, DrainTimeout: cfg.SSHDrainTimeout, DisableRateLimits: opt.DisableRateLimits},
		sshgw.Deps{Store: r, Keys: ids, Clock: clk, Log: log})
	if err != nil {
		return nil, err
	}
```

- 返回的 `App` 字面量加 `SSH: sshSrv, SSHHostKey: sshHostKey, UpstreamSigner: upstreamSigner`。import 追加 `"golang.org/x/crypto/ssh"`、`"spark-cloud/internal/sshgw"`。

`cmd/platform/main.go` 的 `run` 在网关 goroutine 之后加：

```go
	sshLis, err := net.Listen("tcp", cfg.SSHAddr)
	if err != nil {
		return fmt.Errorf("ssh gateway listen: %w", err)
	}
	sshErr := make(chan error, 1)
	go func() {
		log.Info("ssh gateway listening", "addr", cfg.SSHAddr, "public_port", cfg.SSHPublicPort)
		sshErr <- a.SSH.Serve(ctx, sshLis) // ctx 结束后按 SPARK_SSH_DRAIN_TIMEOUT drain
	}()
```

并在 `<-sweeperDone` 之后加 `<-sshErr`（Serve 总是返回 nil；等待它保证 drain 完成再退出进程）。

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/sshgw/ ./internal/pki/ ./internal/app/... ./cmd/... -race 2>&1 | tail -6 && go build ./...`
Expected: 全部 `ok`；`go build` 通过。

- [ ] **Step 7: 本地冒烟（开发库）**

```bash
export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH
make devdb &            # 已在跑则跳过
SPARK_DATABASE_URL=postgres://postgres:postgres@localhost:5433/spark?sslmode=disable \
SPARK_SECRET_KEY=MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE= go run ./cmd/platform &
sleep 3
ssh -p 2222 -o StrictHostKeyChecking=no root@127.0.0.1 ; echo "exit=$?"
go run ./cmd/platform admin ssh-host-key
kill %2
```
Expected: 第一条 ssh 显示 Banner「用户名应为实例 ID，例如 ins-a1b2c3d4」后 `Permission denied`；第二条打印公钥与指纹。

- [ ] **Step 8: 提交**

```bash
git add internal/sshgw internal/pki internal/app cmd/platform
git commit -m "feat(sshgw): session scan/disconnect, last_seen refresh, drain; wire ssh gateway into app and platform

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 12: 协议 `host_key_pem`、网关经 `HostKeys` 投递、Agent 写入主机密钥与新 `init.sh`、镜像规范

**Files:**
- Modify: `api/proto/agent/v1/agent.proto`、`internal/agentpb/agent.pb.go`（`make proto` 生成）
- Modify: `internal/agentgw/server.go:44-52`（`Deps.HostKeys`）、`internal/agentgw/deliver.go:120-135`（`push`）、`internal/agentgw/convert.go:48-58`、`internal/agentgw/deliver_test.go`、`internal/agentgw/helpers_test.go:90`
- Modify: `internal/agent/executor/executor.go:457-466`（`Command.HostKeyPEM`）、`internal/agent/executor/sshdir.go`、`internal/agent/executor/ops.go:84,194-209,322-363`、`internal/agent/executor/executor_test.go`
- Modify: `internal/agent/client/client.go:893-911`（`toCommand`）
- Modify: `docs/image-spec.md`

**Interfaces:**
- Produces:
  - proto：`message CreateInstance { … string host_key_pem = 6; }`（注释：敏感，仅在 mTLS 流上传输，不落日志、不进 `agent_commands.payload`）。
  - `agentgw.HostKeys interface { HostKeyPEM(ctx, instanceID string) (string, error) }`；`agentgw.Deps.HostKeys HostKeys`（`*rental.Service` 满足；nil → 不填）。`toProtoCommand(cmd fleet.Command, cfg Config, hostKeyPEM string)`。
  - `executor.Command.HostKeyPEM string`；`writeSSHDir(id, upstreamPubKey, hostKeyPEM string) error`（有 PEM 且本地无主机密钥 → 写入 0600 并派生 `.pub`；已存在保留；为空 → 本地生成）；`writeInitScript(id string) error`（只重写 `init.sh`，create / start / Recover 都调用）。
  - `initScript` 追加 10 行配置（见 Global Constraints）。

- [ ] **Step 1: 写失败的测试**

`internal/agentgw/deliver_test.go` 的 `TestDeliveryWaitsForFirstStateReport` 里 `cr.GetDataSizeGib() != 2048` 的断言之后追加：

```go
	if !strings.Contains(cr.GetHostKeyPem(), "OPENSSH PRIVATE KEY") {
		t.Fatalf("host_key_pem not delivered: %q", cr.GetHostKeyPem())
	}
	var payload string
	_ = e.pool.QueryRow(e.ctx, `SELECT payload::text FROM agent_commands WHERE id = $1`, cmdID).Scan(&payload)
	if strings.Contains(payload, "PRIVATE KEY") || strings.Contains(payload, "host_key") {
		t.Fatalf("private key leaked into agent_commands.payload: %s", payload)
	}
```

（import 追加 `"strings"`；`helpers_test.go` 的 `agentgw.Deps{...}` 增加 `HostKeys: r`。）

`internal/agent/executor/executor_test.go`：`TestCreateHappyPath` 的 `init.sh` 期望列表追加 `"Subsystem sftp internal-sftp"`, `"AllowTcpForwarding yes"`, `"GatewayPorts no"`, `"AllowAgentForwarding yes"`, `"X11Forwarding no"`, `"PermitTunnel no"`, `"ClientAliveInterval 60"`, `"ClientAliveCountMax 3"`, `"MaxStartups 50:30:200"`, `"MaxSessions 32"`；并新增：

```go
func TestHostKeyFromPlatformIsWrittenOnceAndInitScriptRewritten(t *testing.T) {
	e := newEnv(t)
	_, pem, err := pki.GenerateInstanceHostKey()
	if err != nil {
		t.Fatal(err)
	}
	cmd := create("ins-aaaaaaaa")
	cmd.HostKeyPEM = string(pem)
	if res := e.run(cmd); len(res) != 1 || !res[0].OK {
		t.Fatalf("create = %+v", res)
	}
	keyPath := filepath.Join(e.dir, "ssh", "ins-aaaaaaaa", "hostkeys", "ssh_host_ed25519_key")
	got, _ := os.ReadFile(keyPath)
	if string(got) != string(pem) {
		t.Fatalf("host key = %q, want platform pem", got)
	}
	if st, _ := os.Stat(keyPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o", st.Mode().Perm())
	}
	pub, _ := os.ReadFile(keyPath + ".pub")
	signer, _ := ssh.ParsePrivateKey(pem)
	if strings.TrimSpace(string(pub)) != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) {
		t.Fatalf(".pub = %q", pub)
	}
	// 重跑 create（步骤幂等）且平台换了一把钥匙：已存在的不被覆盖
	_, pem2, _ := pki.GenerateInstanceHostKey()
	cmd2 := cmd
	cmd2.ID, cmd2.HostKeyPEM = "cmd-again", string(pem2)
	e.run(cmd2)
	if got, _ := os.ReadFile(keyPath); string(got) != string(pem) {
		t.Fatal("existing host key overwritten")
	}
	// 旧版 init.sh（没有 sftp 行）在 start 时被重写
	initPath := filepath.Join(e.dir, "ssh", "ins-aaaaaaaa", "init.sh")
	os.WriteFile(initPath, []byte("#!/bin/sh\nexec /usr/sbin/sshd -D\n"), 0o755)
	e.run(executor.Command{ID: "cmd-stop", InstanceID: "ins-aaaaaaaa", Type: "stop_instance"})
	e.run(executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	init, _ := os.ReadFile(initPath)
	if !strings.Contains(string(init), "Subsystem sftp internal-sftp") {
		t.Fatalf("init.sh not rewritten on start:\n%s", init)
	}
	// 没有 PEM 的旧平台：本地生成
	e2 := newEnv(t)
	e2.run(create("ins-bbbbbbbb"))
	if _, err := os.Stat(filepath.Join(e2.dir, "ssh", "ins-bbbbbbbb", "hostkeys", "ssh_host_ed25519_key")); err != nil {
		t.Fatalf("locally generated host key missing: %v", err)
	}
}
```

（import 追加 `"golang.org/x/crypto/ssh"`、`"spark-cloud/internal/pki"`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/agentgw/ ./internal/agent/executor/ -run 'TestDeliveryWaitsForFirstStateReport|TestCreateHappyPath|TestHostKeyFromPlatform' 2>&1 | head`
Expected: 编译错误（`GetHostKeyPem`、`HostKeyPEM` 未定义）。

- [ ] **Step 3: proto 与生成代码**

`api/proto/agent/v1/agent.proto` 的 `CreateInstance` 改为：

```proto
message CreateInstance {
  string image_ref = 1;
  string upstream_pubkey = 2;   // authorized_keys 一行
  uint32 bandwidth_mbps = 3;
  uint32 ssh_port = 4;          // 容器 22 发布到 <lan_ip>:<ssh_port>
  uint32 data_size_gib = 5;
  // 平台生成的容器 sshd 主机私钥（OpenSSH PEM）。敏感：只在 mTLS 流上传输，不落日志，不进 agent_commands.payload；
  // 为空时 Agent 沿用本地生成（兼容旧平台）。
  string host_key_pem = 6;
}
```

Run: `export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH && make proto && git status --short internal/agentpb`
Expected: `internal/agentpb/agent.pb.go` 更新，含 `GetHostKeyPem()`。

- [ ] **Step 4: 网关经 `HostKeys` 填充**

`internal/agentgw/server.go`：

```go
// HostKeys 提供实例容器的主机私钥（由 rental 解密），只在投递 Create 指令时读取。
type HostKeys interface {
	HostKeyPEM(ctx context.Context, instanceID string) (string, error)
}
```

`Deps` 追加 `HostKeys HostKeys`。`convert.go` 的 `toProtoCommand` 签名改为 `toProtoCommand(cmd fleet.Command, cfg Config, hostKeyPEM string)`，`CreateInstance{...}` 增加 `HostKeyPem: hostKeyPEM`。`deliver.go`：`TickNow` 里 `s.push(c, cmd, n)` 改为 `s.push(ctx, c, cmd, n)`，`push` 改为：

```go
// push 把指令推给流；Create 指令在这里读实例主机私钥（不经过 agent_commands.payload）。发送失败则断流（指令保持 sent，重连后补推）。
func (s *Server) push(ctx context.Context, c *conn, cmd fleet.Command, tick int64) bool {
	hostKey := ""
	if cmd.Type == fleet.CmdCreateInstance && s.d.HostKeys != nil {
		pem, err := s.d.HostKeys.HostKeyPEM(ctx, cmd.InstanceID)
		if err != nil {
			s.d.Log.Error("tick: load instance host key", "command", cmd.ID, "instance", cmd.InstanceID, "err", err)
			return false
		}
		hostKey = pem
	}
	msg, err := toProtoCommand(cmd, s.cfg, hostKey)
	if err != nil {
		s.d.Log.Error("tick: convert command", "command", cmd.ID, "err", err)
		return false
	}
	if err := c.send(&agentpb.PlatformMessage{Msg: &agentpb.PlatformMessage_Command{Command: msg}}); err != nil {
		c.abort(err)
		return false
	}
	c.markPushed(cmd.ID)
	s.deliveries.Store(cmd.ID, tick)
	return true
}
```

`internal/app/app.go` 的 `agentgw.Deps{...}` 增加 `HostKeys: r`。

- [ ] **Step 5: Agent 侧**

`internal/agent/executor/executor.go` 的 `Command` 追加 `HostKeyPEM string // 平台生成的主机私钥；为空则本地生成`。

`internal/agent/client/client.go` 的 `toCommand` 在 `Command_Create` 分支追加 `out.HostKeyPEM = b.Create.GetHostKeyPem()`。

`internal/agent/executor/sshdir.go` 整体改为：

```go
package executor

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// initScript 是容器入口：写最小 sshd 配置并 exec sshd；镜像自身的 ENTRYPOINT / CMD 不会执行。
// sftp 用 sshd 内置实现（镜像不需要 openssh-sftp-server）；X11 关闭；转发与 agent 转发放开（网关侧另有白名单）。
const initScript = `#!/bin/sh
set -e
mkdir -p /run/sshd
cat > /run/spark-sshd.conf <<'EOF'
Port 22
HostKey /.spark/hostkeys/ssh_host_ed25519_key
AuthorizedKeysFile /.spark/authorized_keys
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
PidFile /run/spark-sshd.pid
Subsystem sftp internal-sftp
AllowTcpForwarding yes
GatewayPorts no
AllowAgentForwarding yes
X11Forwarding no
PermitTunnel no
ClientAliveInterval 60
ClientAliveCountMax 3
MaxStartups 50:30:200
MaxSessions 32
EOF
exec /usr/sbin/sshd -D -e -f /run/spark-sshd.conf
`

// writeSSHDir 准备 ssh/<id>/{authorized_keys,hostkeys/ssh_host_ed25519_key,init.sh}。
// 主机密钥：已存在则保留；否则有平台下发的 PEM 就写入，没有就本地生成（兼容旧平台）。
func (e *Executor) writeSSHDir(id, upstreamPubKey, hostKeyPEM string) error {
	dir := e.sshDir(id)
	if err := os.MkdirAll(filepath.Join(dir, "hostkeys"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "authorized_keys"), []byte(upstreamPubKey+"\n"), 0o644); err != nil {
		return err
	}
	if err := e.writeInitScript(id); err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "hostkeys", "ssh_host_ed25519_key")
	if _, err := os.Stat(keyPath); err == nil {
		return nil
	}
	if strings.TrimSpace(hostKeyPEM) == "" {
		return e.d.HostKeyGen(filepath.Join(dir, "hostkeys"))
	}
	return writeHostKey(filepath.Join(dir, "hostkeys"), []byte(hostKeyPEM))
}

// writeInitScript 只重写 init.sh（create / start / 重启恢复都调用，存量实例下次启动即用新配置）。
func (e *Executor) writeInitScript(id string) error {
	path := filepath.Join(e.sshDir(id), "init.sh")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// WriteFile 不会改已存在文件的权限，显式 chmod 保证可执行。
	if err := os.WriteFile(path, []byte(initScript), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

// writeHostKey 写入平台下发的主机私钥（0600）并派生 .pub。
func writeHostKey(dir string, pemBytes []byte) error {
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return errors.New("平台下发的主机密钥无法解析: " + err.Error())
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key"), pemBytes, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(dir, "ssh_host_ed25519_key"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key.pub"), ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644)
}

func generateHostKey(dir string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key"), pem.EncodeToMemory(block), 0o600); err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key.pub"), ssh.MarshalAuthorizedKey(sshPub), 0o644)
}
```

`internal/agent/executor/ops.go`：第 84 行 `e.writeSSHDir(c.InstanceID, c.UpstreamPubKey)` 改为 `e.writeSSHDir(c.InstanceID, c.UpstreamPubKey, c.HostKeyPEM)`；`start()` 在 `e.d.Disk.Mount(...)` 之前加

```go
	if err := e.writeInitScript(id); err != nil {
		return fmt.Errorf("更新 init.sh 失败：%w", err)
	}
```

`Recover()` 的实例循环里，`inst.Desired != "running"` 判断之前（挂载成功之后）加

```go
		if err := e.writeInitScript(inst.ID); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: 更新 init.sh 失败：%w", inst.ID, err))
		}
```

- [ ] **Step 6: 镜像规范**

`docs/image-spec.md`「运行时环境」一节追加：

```markdown
- sftp 由 sshd 内置实现（`Subsystem sftp internal-sftp`）提供，镜像**不需要** `openssh-sftp-server`；`scp`（OpenSSH ≥ 9 默认走 SFTP）、`sftp`、`rsync` 都可用。
- 容器主机密钥由平台生成并随开通指令下发（`/.spark/hostkeys/`），平台登录容器时固定校验它；镜像里不得包含 `/etc/ssh/ssh_host_*`。
```

并把「SSH 端口 22 发布到机器内网 IP 的实例专属端口（经 sshpiper 给用户，计划 3）」改为「SSH 端口 22 发布到机器内网 IP 的 2222；用户经平台 SSH 网关（`ssh ins-xxxxxxxx@ssh.<domain>`）登录，Web 端口经平台入口访问，都只由控制面持有的上游私钥连入」。

- [ ] **Step 7: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/agentgw/ ./internal/agent/... ./internal/app/... 2>&1 | tail -6 && make agent`
Expected: 全部 `ok`；`make agent` 输出新二进制与 SHA-256（Task 13 要用）。

- [ ] **Step 8: 提交**

```bash
git add api/proto internal/agentpb internal/agentgw internal/agent internal/app docs/image-spec.md
git commit -m "feat(agent): platform-issued container host key via CreateInstance.host_key_pem; sshd config with sftp/forwarding; init.sh rewritten on start

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 13: 真机检查点 A（192.168.1.221）——SSH 网关与 Agent：规格 §10.4 第 1～6、9～13 项

**Files:**
- Create: `docs/superpowers/acceptance-plan3.md`（本任务写「检查点 A」部分；Task 24 补齐）
- 可能修改：验收中发现的 sshgw / upstream / Agent 缺陷在对应包修复并补单测（单独提交）

**Interfaces:**
- Consumes: Task 11 的 platform（SSH 网关）、Task 12 的 Agent 二进制与 `make agent` 的 SHA-256。
- 前提与分工：平台跑在开发 Mac（局域网 IP `ipconfig getifaddr en0`，记为 `$MAC`，验收时是 192.168.1.218），Agent 跑在 Spark。**Claude 可以直接在测试机上执行 `sudo` 命令**（`ssh imac@192.168.1.221 'sudo -S ...'`，密码由负责人在终端输入或通过 `sudo -v` 预先缓存；**任何地方都不得写下或回显密码**，命令、日志、记录里也不能出现）。需要公网的第 8 项不在本检查点。

- [ ] **Step 0: 准备**

Mac 上：

```bash
export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH
MAC=$(ipconfig getifaddr en0); echo $MAC
make devdb &            # 终端 1（已在跑则跳过）
ls .devdata/upstream.key || go run ./cmd/platform admin gen-upstream-key --path .devdata/upstream.key
make agent              # 记下 SHA-256
mkdir -p .devdata/dl && cp bin/spark-agent-linux-arm64 deploy/agent/install.sh .devdata/dl/
(cd .devdata/dl && python3 -m http.server 8000) &   # 终端 2
SPARK_DATABASE_URL=postgres://postgres:postgres@localhost:5433/spark?sslmode=disable \
SPARK_SECRET_KEY=MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE= \
SPARK_DOMAIN=$(echo $MAC | tr . -).sslip.io SPARK_PUBLIC_BASE_URL=http://$(echo $MAC | tr . -).sslip.io:8080 \
SPARK_SSH_ADDR=:2222 SPARK_SSH_PUBLIC_PORT=2222 \
SPARK_AGENT_ADDR=:9443 SPARK_AGENT_HOSTS=$MAC \
SPARK_AGENT_DOWNLOAD_URL=http://$MAC:8000 SPARK_AGENT_BINARY_SHA256=<make agent 输出> \
SPARK_UPSTREAM_SSH_KEY_FILE=$PWD/.devdata/upstream.key SPARK_AGENT_DATA_SIZE_GIB=200 \
go run ./cmd/platform     # 终端 3
go run ./cmd/platform admin ssh-host-key   # 记下指纹（环境变量同上）
```

后台（已有管理员与机器登记则跳过）：机器 `lan_ip = 192.168.1.221`，商品 spark-1，结束维护。**先升级 Agent 再用新平台开通实例**：后台「生成接入命令」→ 在 Spark 上执行（`ssh imac@192.168.1.221 'curl -fsSL http://$MAC:8000/install.sh | sudo -S sh -s -- …'`，命令由后台原样复制）。

- [ ] **Step 1: 开通与主机密钥（§10.4.1）**

用户下单 → running。Spark 上：`ssh imac@192.168.1.221 'docker exec <id> cat /.spark/hostkeys/ssh_host_ed25519_key.pub; docker exec <id> cat /.spark/init.sh'`。核对：公钥与 `SELECT ssh_host_pubkey FROM instances WHERE id='<id>'`（`.devdata/runtime/bin/psql …`）一致；`init.sh` 含 `Subsystem sftp internal-sftp` 与 `MaxStartups`。

- [ ] **Step 2: 密码登录（§10.4.2）**

控制台复制命令（应为 `ssh -p 2222 <id>@ssh.<MAC>.sslip.io`）→ Mac 上执行，首次提示的指纹与 `admin ssh-host-key` 输出一致 → 输入密码 → `nvidia-smi`、`hostname`。退出后 `SELECT auth_method, client_ip, bytes_up, bytes_down, ended_at FROM ssh_sessions ORDER BY id DESC LIMIT 1`：一行、`password`、`client_ip` 是 Mac 地址、`ended_at` 非空、字节数 > 0。

- [ ] **Step 3: 公钥登录与关闭密码（§10.4.3）**

控制台公钥页面与接口在 Task 19 / 20 才有，本检查点直接写库（开发库）：`INSERT INTO ssh_keys (user_id, name, public_key, fingerprint, created_at) VALUES (<uid>, 'mac', '<~/.ssh/id_ed25519.pub 的前两段>', '<ssh-keygen -lf ~/.ssh/id_ed25519.pub 输出的 SHA256:…>', now())`，然后 `ssh -p 2222 -i ~/.ssh/id_ed25519 <id>@…` 免密登录。`ssh-agent` 里装 4 把其它密钥再连 3 次都成功（不触发封禁）；`UPDATE instances SET password_auth=false` → 密码被拒、公钥通；`DELETE FROM ssh_keys …` → 公钥被拒；`UPDATE … password_auth=true` → 密码通。`ssh -p 2222 foo@…` 显示 Banner「用户名应为实例 ID」。

- [ ] **Step 4: 密码重置（§10.4.4）——在检查点 B 执行**

重置接口 `POST /instances/{id}/password/reset` 在 Task 19 才有；本项在 Task 24 做，记录里写「A 阶段未做，B 阶段完成」。

- [ ] **Step 5: scp / sftp / rsync / VS Code（§10.4.5）**

`dd if=/dev/urandom of=/tmp/1g bs=1m count=1024`；`time scp -P 2222 /tmp/1g <id>@…:/root/data/`；`time scp -P 2222 <id>@…:/root/data/1g /tmp/1g.back && cmp /tmp/1g /tmp/1g.back`；`sftp -P 2222 <id>@…` 里 `ls`、`put`、`get`；`rsync -e 'ssh -p 2222' -a /tmp/1g <id>@…:/root/data/r/`；VS Code Remote-SSH（Host 配置 `Port 2222`）打开 `/root/data` 并开终端。记录速率。

- [ ] **Step 6: 转发策略（§10.4.6）**

容器内 `python3 -m http.server 8000`（无 python 时 `while true; do echo -e 'HTTP/1.0 200 OK\r\n\r\nhi' | nc -l -p 8000 -q 1; done`）：`ssh -p 2222 -L 8000:127.0.0.1:8000 <id>@…` 后 Mac `curl 127.0.0.1:8000` 成功；`ssh -R 9000:127.0.0.1:9000` 后容器内 `curl 127.0.0.1:9000` 打到 Mac 上的 `python3 -m http.server 9000`；`ssh -A` 后容器内 `ssh-add -l` 列出 Mac 的密钥；`ssh -X … 'echo $DISPLAY'` 为空；`ssh -L 8001:192.168.1.1:80` 后 `curl -m 3 127.0.0.1:8001` 失败（容器 sshd 拨出受 `SPARK-FWD` 阻断）。

- [ ] **Step 7: 状态门控（§10.4.9）**

挂一个 SSH 会话 → `UPDATE instances SET expires_at = now() + interval '1 minute'` → 停机后 10 秒内会话收到「实例已停机 / 已释放，连接已断开」并断开；新连接 Banner 显示保留截止时间；续费 → 能连；释放 → 「实例已释放」。

- [ ] **Step 8: 机器失联（§10.4.10）**

先开一个 SSH 会话跑 `top`；`ssh imac@192.168.1.221 'sudo -S systemctl stop spark-agent'`；> 5 分钟后实例暂停：已有会话**仍在**（裁定 5），新连接 Banner「机器暂时失联」；`sudo -S systemctl start spark-agent` → 新连接恢复。

- [ ] **Step 9: 隔离未削弱（§10.4.11）**

`ssh imac@192.168.1.221 'sudo -S iptables -S SPARK-FWD; sudo -S iptables -S SPARK-IN; sudo -S ss -lntp'`：两条链与 `iptables.ExpectedFwdRules(allow)` / `ExpectedInRules()` 逐行相等；监听端口除 2222 外无新增；Mac 上 `curl -m 3 192.168.1.221:8888` 不通；容器内 `curl -m 3 $MAC:8080` 被阻断（Mac 不在白名单）。

- [ ] **Step 10: 暴力破解（§10.4.12）**

Mac 上用 Go 写的小脚本（`go run` 一个只用 `x/crypto/ssh` 的临时程序，放在 `.devdata/` 不入库）对 `<id>@$MAC:2222` 发起 10 条密码错误的连接（每条试满 6 次）→ 第 11 条连接在握手前被断开（`ssh` 命令行看到 `kex_exchange_identification: Connection closed`）；10 分钟后恢复；后台告警页无 `ssh_bruteforce`（实例阈值 30 未到）。同一来源 IP 达不到 30 次（第 10 次就被封），实例阈值的告警由 `TestInstanceBruteforceAlertOnce` 覆盖，记录里注明。

- [ ] **Step 11: 重启（§10.4.13）**

`ssh imac@192.168.1.221 'sudo -S reboot'` → Agent 恢复容器后 SSH 能连、`/root/data/1g` 仍在、`known_hosts` 不报主机密钥变化、告警页无 `upstream_unreachable`。

- [ ] **Step 12: 记录与提交**

把每项的命令、关键输出与结论写进 `docs/superpowers/acceptance-plan3.md`「检查点 A」一节（格式同 `acceptance-plan2.md`）；发现的缺陷修复后单独提交并在记录里写明提交号。

```bash
git add docs/superpowers/acceptance-plan3.md
git commit -m "docs: plan 3 real-machine checkpoint A (ssh gateway + agent) on 192.168.1.221

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 14: 同站防护中间件（控制台 + 后台）、`__Host-` 会话 Cookie、`identity` 会话哈希

**Files:**
- Create: `internal/platform/httpx/sameorigin.go`、`internal/platform/httpx/sameorigin_test.go`
- Modify: `internal/api/server.go:25-43,49-53,101-137`、`internal/api/auth.go:9-15,49-53`、`internal/api/api_test.go`
- Modify: `internal/adminapi/server.go:38-60,78-86`、`internal/adminapi/auth_test.go`
- Modify: `internal/identity/auth.go`、`internal/identity/auth_test.go`
- Modify: `internal/app/app.go`（传 `PublicBaseURL` / `AdminPublicBaseURL`）、`internal/app/e2e_test.go:400-431`

**Interfaces:**
- Produces:
  - `httpx.SameSiteGuard(publicBaseURL string, exempt func(*http.Request) bool, log *slog.Logger) func(http.Handler) http.Handler`：三段规则（见 Global Constraints），拒绝时 `WriteError(apperr.ErrForbidden)`。
  - `api.Server.PublicBaseURL string`；会话 Cookie 名 `sc_session`（`SecureCookie=false`）/ `__Host-sc_session`（`SecureCookie=true`，`Secure; Path=/`，无 Domain）。
  - `adminapi.Server.PublicBaseURL string`（来自 `cfg.AdminPublicBaseURL`，可为空）。
  - `identity.SessionHash(token string) string`；`(*identity.Service).SessionUserID(ctx, hash string) (int64, error)`（会话不存在 / 过期 → `apperr.ErrUnauthorized`；用户封禁 → `apperr.ErrBanned`）。

- [ ] **Step 1: 写失败的测试**

`internal/platform/httpx/sameorigin_test.go`：

```go
package httpx_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spark-cloud/internal/platform/httpx"
)

func TestSameSiteGuardMatrix(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	exempt := func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/v1/pay/notify/") }
	h := httpx.SameSiteGuard("https://spark.example", exempt, slog.New(slog.NewTextHandler(io.Discard, nil)))(ok)
	cases := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		want    int
	}{
		{"get never checked", "GET", "/api/v1/me", map[string]string{"Sec-Fetch-Site": "cross-site"}, 204},
		{"same-origin", "POST", "/api/v1/orders", map[string]string{"Sec-Fetch-Site": "same-origin"}, 204},
		{"none (typed url)", "POST", "/api/v1/orders", map[string]string{"Sec-Fetch-Site": "none"}, 204},
		{"same-site sibling subdomain", "POST", "/api/v1/uploads", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://ins-a1b2c3d4-80.spark.example"}, 403},
		{"cross-site", "POST", "/api/v1/orders", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"no sfs, origin matches", "POST", "/api/v1/orders", map[string]string{"Origin": "https://spark.example"}, 204},
		{"no sfs, origin matches with trailing slash", "PUT", "/api/v1/x", map[string]string{"Origin": "https://spark.example/"}, 204},
		{"no sfs, origin differs", "POST", "/api/v1/orders", map[string]string{"Origin": "https://evil.example"}, 403},
		{"no sfs, origin scheme differs", "POST", "/api/v1/orders", map[string]string{"Origin": "http://spark.example"}, 403},
		{"neither header (curl / go client)", "POST", "/api/v1/orders", nil, 204},
		{"payment callback exempt", "POST", "/api/v1/pay/notify/wechat", map[string]string{"Sec-Fetch-Site": "cross-site"}, 204},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("%s: %d, want %d (body %s)", c.name, rec.Code, c.want, rec.Body.String())
		}
		if c.want == 403 && !strings.Contains(rec.Body.String(), `"forbidden"`) {
			t.Fatalf("%s: body = %s", c.name, rec.Body.String())
		}
	}
	// 没有对外地址（后台未配置 SPARK_ADMIN_PUBLIC_BASE_URL）：有 Origin 一律拒绝，没有则放行
	h2 := httpx.SameSiteGuard("", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(ok)
	for origin, want := range map[string]int{"https://admin.spark.example": 403, "": 204} {
		req := httptest.NewRequest("POST", "/admin/api/v1/x", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h2.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("no base url, origin %q: %d", origin, rec.Code)
		}
	}
}
```

`internal/api/api_test.go` 追加（`newTestEnvWith` 里把 `PublicBaseURL: "http://spark.test"` 加进 `api.Server` 字面量）：

```go
func TestSessionCookieNameAndSameSiteGuard(t *testing.T) {
	e := newTestEnvWith(t, func(s *api.Server) { s.SecureCookie = true })
	raw := &http.Client{}
	post := func(path, body string, headers map[string]string) *http.Response {
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := raw.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	post("/auth/sms-code", `{"phone":"13800009999"}`, nil)
	resp := post("/auth/login", `{"phone":"13800009999","code":"000000"}`, nil)
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "__Host-sc_session" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.Secure || cookie.Path != "/" || cookie.Domain != "" || !cookie.HttpOnly {
		t.Fatalf("cookie = %+v", resp.Cookies())
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/me", nil)
	req.AddCookie(cookie)
	if r, _ := raw.Do(req); r.StatusCode != 200 {
		t.Fatalf("me with __Host- cookie = %d", r.StatusCode)
	}
	// 同站（兄弟子域）写请求被拒；same-origin 放行；无头放行
	if r := post("/auth/logout", `{}`, map[string]string{"Sec-Fetch-Site": "same-site", "Cookie": cookie.String()}); r.StatusCode != 403 {
		t.Fatalf("same-site logout = %d", r.StatusCode)
	}
	if r := post("/auth/logout", `{}`, map[string]string{"Sec-Fetch-Site": "same-origin", "Cookie": cookie.String()}); r.StatusCode != 200 {
		t.Fatalf("same-origin logout = %d", r.StatusCode)
	}
	// 支付回调不受同站限制（这里渠道会因验签失败返回非 403）
	if r := post("/pay/notify/wechat", `{}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.StatusCode == 403 {
		t.Fatal("payment notify blocked by same-site guard")
	}
	// dev（SecureCookie=false）仍叫 sc_session
	dev := newTestEnv(t)
	c := dev.client()
	c.login("13800009998")
	for _, ck := range c.http.Jar.Cookies(mustURL(dev.ts.URL)) {
		if ck.Name == "sc_session" {
			return
		}
	}
	t.Fatal("dev cookie not named sc_session")
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
```

（import 追加 `"net/url"`。）

`internal/adminapi/auth_test.go` 追加：

```go
func TestAdminSameSiteGuard(t *testing.T) {
	e := newEnvWith(t, func(s *adminapi.Server) { s.PublicBaseURL = "https://admin.spark.test" })
	req, _ := http.NewRequest("POST", e.ts.URL+"/admin/api/v1/auth/login", strings.NewReader(`{"username":"x","password":"y"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("same-site admin login = %d", resp.StatusCode)
	}
	req2, _ := http.NewRequest("POST", e.ts.URL+"/admin/api/v1/auth/login", strings.NewReader(`{"username":"x","password":"y"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Origin", "https://admin.spark.test")
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode == 403 {
		t.Fatal("matching origin rejected")
	}
}
```

（import 补 `"net/http"`、`"strings"` 若缺。）

`internal/identity/auth_test.go` 追加：

```go
func TestSessionHashAndUserID(t *testing.T) {
	e := newEnv(t, "000000")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800001234", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	token, u, err := e.svc.Login(ctx, "13800001234", "000000")
	if err != nil {
		t.Fatal(err)
	}
	h := identity.SessionHash(token)
	if len(h) != 64 || h == token {
		t.Fatalf("hash = %q", h)
	}
	if id, err := e.svc.SessionUserID(ctx, h); err != nil || id != u.ID {
		t.Fatalf("session user = %d %v", id, err)
	}
	if _, err := e.svc.SessionUserID(ctx, identity.SessionHash("other")); !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("unknown = %v", err)
	}
	if err := e.svc.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SessionUserID(ctx, h); !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("after logout = %v", err)
	}
}
```

`internal/app/e2e_test.go` 的 `TestSecureCookieOutsideDev`：把 `map[string]bool{"dev": false, "staging": true}` 改为按环境期望 Cookie 名：

```go
	for env, want := range map[string]struct {
		name   string
		secure bool
	}{"dev": {"sc_session", false}, "staging": {"__Host-sc_session", true}} {
```

循环体里 `if c.Name == "sc_session"` 改为 `if c.Name == want.name`，断言改为 `secure != want.secure`（并把 `wantSecure` 的引用一并改掉）。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/platform/httpx/ ./internal/api/ ./internal/adminapi/ ./internal/identity/ -run 'SameSite|SessionCookieName|SessionHash' 2>&1 | head`
Expected: 编译错误 / 404。

- [ ] **Step 3: `httpx.SameSiteGuard`**

`internal/platform/httpx/sameorigin.go`：

```go
package httpx

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"spark-cloud/internal/platform/apperr"
)

// SameSiteGuard 防止同站（兄弟子域，例如租户 Web 页面）向控制台 / 后台提交写请求：SameSite Cookie 只区分跨站不区分兄弟子域。
// 规则（非 GET / HEAD / OPTIONS）：有 Sec-Fetch-Site → 只放行 same-origin / none；没有它但有 Origin → 必须等于 publicBaseURL 的 origin
// （publicBaseURL 为空时无法比对，一律拒绝）；两个头都没有 → 放行（非浏览器客户端；浏览器跨源 POST 一定带 Origin）。
func SameSiteGuard(publicBaseURL string, exempt func(*http.Request) bool, log *slog.Logger) func(http.Handler) http.Handler {
	origin := ""
	if u, err := url.Parse(publicBaseURL); err == nil && u.Scheme != "" && u.Host != "" {
		origin = strings.ToLower(u.Scheme + "://" + u.Host)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions || (exempt != nil && exempt(r)) {
				next.ServeHTTP(w, r)
				return
			}
			if sfs := strings.ToLower(r.Header.Get("Sec-Fetch-Site")); sfs != "" {
				if sfs == "same-origin" || sfs == "none" {
					next.ServeHTTP(w, r)
					return
				}
				log.Warn("same-site guard rejected request", "sec_fetch_site", sfs, "origin", r.Header.Get("Origin"), "path", r.URL.Path)
				WriteError(log, w, r, apperr.ErrForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				if origin != "" && strings.ToLower(strings.TrimSuffix(o, "/")) == origin {
					next.ServeHTTP(w, r)
					return
				}
				log.Warn("same-site guard rejected request", "origin", o, "path", r.URL.Path)
				WriteError(log, w, r, apperr.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

- [ ] **Step 4: 控制台 Cookie 名与中间件**

`internal/api/server.go`：`Server` 追加字段 `PublicBaseURL string // 对外地址，用于同站防护的 Origin 比对`；增加

```go
// cookieName：staging / prod 用 __Host- 前缀（浏览器拒绝子域用 Domain 属性覆写），dev 走 http 无法用前缀。
func (s *Server) cookieName() string {
	if s.SecureCookie {
		return "__Host-" + sessionCookie
	}
	return sessionCookie
}
```

`Router()` 里 `r.Use(httpx.Recoverer(s.Log), s.requireJSONForWrites)` 改为

```go
	r.Use(httpx.Recoverer(s.Log), s.requireJSONForWrites,
		httpx.SameSiteGuard(s.PublicBaseURL, func(req *http.Request) bool { return strings.HasPrefix(req.URL.Path, notifyPathPrefix) }, s.Log))
```

`requireUser` 与 `auth.go` 的 `setSession` / `handleLogout` 里的 `sessionCookie` 改为 `s.cookieName()`。`requireJSONForWrites` 上方注释「上传接口依赖 SameSite=Lax 会话 Cookie 防跨站提交」改为「同站（兄弟子域）与跨站的写请求由 SameSiteGuard 拦截（SameSite Cookie 对兄弟子域无效）」。

`internal/adminapi/server.go`：`Server` 追加 `PublicBaseURL string // 后台对外地址（SPARK_ADMIN_PUBLIC_BASE_URL），用于同站防护的 Origin 比对；空表示有 Origin 就拒绝`；`Router()` 里 `r.Use(httpx.Recoverer(s.Log), s.requireJSONForWrites)` 改为 `r.Use(httpx.Recoverer(s.Log), s.requireJSONForWrites, httpx.SameSiteGuard(s.PublicBaseURL, nil, s.Log))`。

`internal/app/app.go`：`api.Server{...}` 加 `PublicBaseURL: cfg.PublicBaseURL`；`adminapi.Server{...}` 加 `PublicBaseURL: cfg.AdminPublicBaseURL`。

- [ ] **Step 5: `identity` 会话哈希**

`internal/identity/auth.go` 追加：

```go
// SessionHash 返回会话 token 的存储哈希（与 sessions.token_hash 一致），供 Web 入口把 Cookie 绑定到控制台会话。
func SessionHash(token string) string { return hashSecret(token) }

// SessionUserID 按哈希校验会话仍有效并返回用户 ID；不存在 / 过期 → ErrUnauthorized，用户封禁 → ErrBanned。
func (s *Service) SessionUserID(ctx context.Context, hash string) (int64, error) {
	var userID int64
	var status string
	err := s.d.Pool.QueryRow(ctx, `SELECT u.id, u.status FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > $2`, hash, s.d.Clock.Now()).Scan(&userID, &status)
	if db.IsNoRows(err) {
		return 0, apperr.ErrUnauthorized
	}
	if err != nil {
		return 0, err
	}
	if status == "banned" {
		return 0, apperr.ErrBanned
	}
	return userID, nil
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/platform/httpx/ ./internal/api/ ./internal/adminapi/ ./internal/identity/ ./internal/app/ 2>&1 | tail -6`
Expected: 全部 `ok`。现有 api / adminapi 测试用 Go http client（无 `Origin` / `Sec-Fetch-Site`）不受影响。

- [ ] **Step 7: 提交**

```bash
git add internal/platform/httpx internal/api internal/adminapi internal/identity internal/app
git commit -m "feat(api): same-site guard for console and admin writes, __Host- session cookie outside dev, session hash lookup for ingress

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 15: `internal/ingress` 基础：Host 解析、路由表、状态页

**Files:**
- Create: `internal/ingress/host.go`、`internal/ingress/table.go`、`internal/ingress/pages.go`、`internal/ingress/host_test.go`、`internal/ingress/table_test.go`

**Interfaces:**
- Consumes: Task 4 `rental.IngressRoute` / `ListIngressRoutes`。
- Produces:
  - `ingress.ParseHost(host, ingressDomain string) (instanceID string, port int, ok bool)`：去端口、去尾点、小写后匹配 `^ins-([a-z0-9]{8})-([0-9]{1,5})\.<domain>$`，端口 1～65535。
  - `ingress.Source interface { ListIngressRoutes(ctx) ([]rental.IngressRoute, error) }`；`ingress.NewTable(src Source, log *slog.Logger) *Table`；`(*Table).Refresh(ctx) error`、`Run(ctx, interval)`、`Lookup(instanceID string, port int) (rental.IngressRoute, bool)`、`Routes() []rental.IngressRoute`。
  - 页面：`ingress.WritePage(w http.ResponseWriter, status int, title, body string, refreshSeconds int)`（内联 HTML，`Content-Type: text/html; charset=utf-8`，`Cache-Control: no-store`，`refreshSeconds > 0` 时带 `<meta http-equiv="refresh">`）；文案常量 `PageNotOpen = "该端口未开放"`、`PageNotRunningFmt`（按状态：creating「实例正在开通」、stopped「实例已到期停机，续费后可访问」、暂停「实例所在机器暂时失联」）、`PageUpstreamFmt = "无法连接到实例端口，请确认服务已在容器内监听 %d"`。

- [ ] **Step 1: 写失败的测试**

`internal/ingress/host_test.go`：

```go
package ingress_test

import (
	"testing"

	"spark-cloud/internal/ingress"
)

func TestParseHostNormalizes(t *testing.T) {
	ok := []struct {
		host string
		id   string
		port int
	}{
		{"ins-a1b2c3d4-8888.spark.test", "ins-a1b2c3d4", 8888},
		{"INS-a1b2c3d4-8888.Spark.Test.:18080", "ins-a1b2c3d4", 8888},
		{"ins-a1b2c3d4-80.spark.test:443", "ins-a1b2c3d4", 80},
		{"ins-zzzzzzzz-65535.spark.test", "ins-zzzzzzzz", 65535},
	}
	for _, c := range ok {
		id, port, found := ingress.ParseHost(c.host, "spark.test")
		if !found || id != c.id || port != c.port {
			t.Fatalf("%q → %q %d %v", c.host, id, port, found)
		}
	}
	for _, bad := range []string{"spark.test", "ins-a1b2c3d4.spark.test", "ins-a1b2c3d4-8888.other.test", "ins-a1b2c3d4-0.spark.test",
		"ins-a1b2c3d4-70000.spark.test", "ins-a1b2c3d-8888.spark.test", "x.ins-a1b2c3d4-8888.spark.test", "ins-a1b2c3d4-8888.spark.test.evil.com", ""} {
		if _, _, found := ingress.ParseHost(bad, "spark.test"); found {
			t.Fatalf("%q accepted", bad)
		}
	}
	// 独立注册域
	if id, port, found := ingress.ParseHost("ins-a1b2c3d4-7860.sparkapp.example", "sparkapp.example"); !found || id != "ins-a1b2c3d4" || port != 7860 {
		t.Fatal("separate ingress domain not matched")
	}
}
```

`internal/ingress/table_test.go`：

```go
package ingress_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"spark-cloud/internal/ingress"
	"spark-cloud/internal/rental"
)

type memSource struct {
	mu     sync.Mutex
	routes []rental.IngressRoute
	err    error
	calls  int
}

func (m *memSource) ListIngressRoutes(context.Context) ([]rental.IngressRoute, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return append([]rental.IngressRoute(nil), m.routes...), m.err
}

func TestTableRefreshAndLookup(t *testing.T) {
	src := &memSource{routes: []rental.IngressRoute{
		{InstanceID: "ins-a1b2c3d4", UserID: 7, Port: 8888, Visibility: "private", State: rental.StateRunning, LanIP: "10.0.0.10", HostPubkey: "k"},
		{InstanceID: "ins-a1b2c3d4", UserID: 7, Port: 7860, Visibility: "public", State: rental.StateRunning, LanIP: "10.0.0.10", HostPubkey: "k"},
	}}
	tb := ingress.NewTable(src, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, ok := tb.Lookup("ins-a1b2c3d4", 8888); ok {
		t.Fatal("lookup before refresh succeeded")
	}
	if err := tb.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, ok := tb.Lookup("ins-a1b2c3d4", 7860)
	if !ok || r.Visibility != "public" || r.UserID != 7 {
		t.Fatalf("lookup = %+v %v", r, ok)
	}
	if _, ok := tb.Lookup("ins-a1b2c3d4", 9999); ok {
		t.Fatal("unknown port found")
	}
	// 刷新失败保留旧表
	src.mu.Lock()
	src.err = errors.New("db down")
	src.mu.Unlock()
	if err := tb.Refresh(context.Background()); err == nil {
		t.Fatal("refresh error swallowed")
	}
	if _, ok := tb.Lookup("ins-a1b2c3d4", 8888); !ok {
		t.Fatal("old routes dropped on failed refresh")
	}
	src.mu.Lock()
	src.err, src.routes = nil, nil
	src.mu.Unlock()
	tb.Refresh(context.Background())
	if len(tb.Routes()) != 0 {
		t.Fatal("removed routes still present")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ingress/ 2>&1 | head -5`
Expected: 编译错误（包不存在）。

- [ ] **Step 3: 实现**

`internal/ingress/host.go`：

```go
// Package ingress 是租户 Web 端口的入口：按 Host（ins-<id>-<port>.<ingress_domain>）分流，私有端口用票据 + 每实例 Cookie 鉴权，
// 请求经容器 sshd 的 direct-tcpip 通道转发到容器内 127.0.0.1:<port>。
package ingress

import (
	"net"
	"regexp"
	"strconv"
	"strings"
)

var hostRe = regexp.MustCompile(`^ins-([a-z0-9]{8})-([0-9]{1,5})$`)

// ParseHost 从 Host 头解析实例 ID 与端口：去端口、去尾点、小写；父域必须精确等于 ingressDomain。
func ParseHost(host, ingressDomain string) (instanceID string, port int, ok bool) {
	h := strings.ToLower(strings.TrimSpace(host))
	if hp, _, err := net.SplitHostPort(h); err == nil {
		h = hp
	}
	h = strings.TrimSuffix(h, ".")
	suffix := "." + strings.ToLower(ingressDomain)
	if !strings.HasSuffix(h, suffix) {
		return "", 0, false
	}
	m := hostRe.FindStringSubmatch(strings.TrimSuffix(h, suffix))
	if m == nil {
		return "", 0, false
	}
	p, err := strconv.Atoi(m[2])
	if err != nil || p < 1 || p > 65535 {
		return "", 0, false
	}
	return "ins-" + m[1], p, true
}
```

`internal/ingress/table.go`：

```go
package ingress

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"spark-cloud/internal/rental"
)

type Source interface {
	ListIngressRoutes(ctx context.Context) ([]rental.IngressRoute, error)
}

type routeKey struct {
	instance string
	port     int
}

// Table 是内存路由表：每 2 秒从数据库整表刷新（200 台规模一条 SQL）；刷新失败保留旧表。
type Table struct {
	src Source
	log *slog.Logger
	mu  sync.RWMutex
	by  map[routeKey]rental.IngressRoute
	all []rental.IngressRoute
}

func NewTable(src Source, log *slog.Logger) *Table {
	if log == nil {
		log = slog.Default()
	}
	return &Table{src: src, log: log, by: map[routeKey]rental.IngressRoute{}}
}

func (t *Table) Refresh(ctx context.Context) error {
	routes, err := t.src.ListIngressRoutes(ctx)
	if err != nil {
		return err
	}
	by := make(map[routeKey]rental.IngressRoute, len(routes))
	for _, r := range routes {
		by[routeKey{r.InstanceID, r.Port}] = r
	}
	t.mu.Lock()
	t.by, t.all = by, routes
	t.mu.Unlock()
	return nil
}

// Run 按 interval 刷新直到 ctx 结束（先立即刷新一次）。
func (t *Table) Run(ctx context.Context, interval time.Duration) {
	refresh := func() {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := t.Refresh(c); err != nil {
			t.log.Error("ingress route refresh", "err", err)
		}
		cancel()
	}
	refresh()
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			refresh()
		}
	}
}

func (t *Table) Lookup(instanceID string, port int) (rental.IngressRoute, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.by[routeKey{instanceID, port}]
	return r, ok
}

func (t *Table) Routes() []rental.IngressRoute {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]rental.IngressRoute(nil), t.all...)
}
```

`internal/ingress/pages.go`：

```go
package ingress

import (
	"fmt"
	"html"
	"net/http"

	"spark-cloud/internal/rental"
)

const (
	PageNotOpen        = "该端口未开放"
	PageUpstreamFmt    = "无法连接到实例端口，请确认服务已在容器内监听 %d"
	PageForbidden      = "无权访问该实例"
	PageBadTarget      = "跳转目标不正确"
	PageSuspended      = "实例所在机器暂时失联，计时已暂停，请稍后重试"
	PageCreating       = "实例正在开通，请稍后再试"
	PageStopped        = "实例已到期停机，续费后可访问"
	PageReleased       = "实例已释放"
	PageTicketInvalid  = "登录票据无效或已过期，请重新访问"
)

// NotRunningPage 按实例状态给出 503 页文案。
func NotRunningPage(r rental.IngressRoute) string {
	switch {
	case r.State == rental.StateRunning && r.Suspended:
		return PageSuspended
	case r.State == rental.StateCreating:
		return PageCreating
	case r.State == rental.StateStopped:
		return PageStopped
	default:
		return PageReleased
	}
}

const pageTpl = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>%s</title>%s
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>body{font-family:system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center;background:#f5f7fa;color:#303133}
main{background:#fff;padding:32px 40px;border-radius:8px;box-shadow:0 2px 12px rgba(0,0,0,.08);max-width:520px}h1{font-size:18px;margin:0 0 12px}p{margin:0;color:#606266;line-height:1.7}</style></head>
<body><main><h1>%s</h1><p>%s</p></main></body></html>`

// WritePage 输出一个简单的状态页；refreshSeconds > 0 时自动刷新（停机 / 失联页用）。
func WritePage(w http.ResponseWriter, status int, title, body string, refreshSeconds int) {
	refresh := ""
	if refreshSeconds > 0 {
		refresh = fmt.Sprintf(`<meta http-equiv="refresh" content="%d">`, refreshSeconds)
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprintf(w, pageTpl, html.EscapeString(title), refresh, html.EscapeString(title), html.EscapeString(body))
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./internal/ingress/ && go test ./internal/ingress/ 2>&1 | tail -3`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/ingress
git commit -m "feat(ingress): host parsing, in-memory route table, status pages

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 16: `ingress` 鉴权：票据 / Cookie 签名、`Auth`（target / next 校验、URL 生成）、callback / logout、控制台 `authorize` 端点

**Files:**
- Create: `internal/ingress/token.go`、`internal/ingress/auth.go`、`internal/ingress/token_test.go`、`internal/ingress/auth_test.go`
- Create: `internal/api/ingress.go`、`internal/api/ingress_test.go`
- Modify: `internal/api/server.go`（`Ingress *ingress.Auth`、`AuthorizeLimit int`、路由）、`internal/api/api_test.go`（`newTestEnvWith` 传 `Ingress`）、`internal/app/app.go`

**Interfaces:**
- Produces:
  - `ingress.Claims{InstanceID string; Port int; UserID int64; SessionHash string; Exp int64; Nonce string}`；`ingress.TicketTTL = 60s`、`ingress.CookieTTL = 12h`；`ingress.NewSigner(secret []byte, clk clock.Clock) *Signer`；`(*Signer).Ticket(c Claims) string`、`VerifyTicket(tok string) (Claims, error)`（验签、未过期、nonce 进程内单次）、`Cookie(c Claims) string`、`VerifyCookie(tok string) (Claims, error)`；`ingress.ErrToken`。
  - `ingress.Target{InstanceID string; Port int; Path string}`（`Path` 含 query，以 `/` 开头）；`ingress.NewAuth(secret []byte, publicBaseURL, ingressDomain string, secureCookie bool, clk clock.Clock) (*Auth, error)`；`(*Auth).ParseTarget(raw string) (Target, error)`、`PortURL(instanceID string, port int) string`、`AuthorizeURL(target string) string`、`CallbackURL(t Target, ticket string) string`、`SafeNext(next string) string`、`ValidCookie(r *http.Request, instanceID string, port int) (Claims, bool)`（`r.CookiesNamed` 逐枚验证）、`HandleCallback(w, r)`、`HandleLogout(w, r)`；`ingress.CookieName = "sc_ingress"`；`(*Auth).Signer *Signer`、`IngressDomain string`、`BaseURL *url.URL`。
  - `api.Server.Ingress *ingress.Auth`、`api.Server.AuthorizeLimit int`（每用户每分钟，0 → 60）；`GET /api/v1/ingress/authorize?target=`（组外，HTML / 302）。

- [ ] **Step 1: 写失败的测试**

`internal/ingress/token_test.go`：

```go
package ingress_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/ingress"
	"spark-cloud/internal/platform/clock"
)

var secret = bytes.Repeat([]byte("s"), 32)

func TestTicketSingleUse(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := ingress.NewSigner(secret, clk)
	c := ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7, SessionHash: "h"}
	tok := s.Ticket(c)
	got, err := s.VerifyTicket(tok)
	if err != nil || got.InstanceID != c.InstanceID || got.Port != 8888 || got.UserID != 7 || got.SessionHash != "h" || got.Nonce == "" {
		t.Fatalf("verify = %+v %v", got, err)
	}
	if _, err := s.VerifyTicket(tok); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("replay = %v", err)
	}
	tok2 := s.Ticket(c)
	clk.Advance(ingress.TicketTTL + time.Second)
	if _, err := s.VerifyTicket(tok2); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("expired = %v", err)
	}
	tok3 := s.Ticket(c)
	tampered := tok3[:len(tok3)-2] + "AA"
	if _, err := s.VerifyTicket(tampered); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("tampered = %v", err)
	}
	if _, err := s.VerifyTicket("garbage"); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("garbage = %v", err)
	}
	// 票据与 Cookie 用不同密钥：票据不能当 Cookie 用
	if _, err := s.VerifyCookie(s.Ticket(c)); !errors.Is(err, ingress.ErrToken) {
		t.Fatal("ticket accepted as cookie")
	}
	other := ingress.NewSigner(bytes.Repeat([]byte("x"), 32), clk)
	if _, err := other.VerifyCookie(s.Cookie(c)); !errors.Is(err, ingress.ErrToken) {
		t.Fatal("cookie accepted with other secret")
	}
}

func TestCookieLifetime(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := ingress.NewSigner(secret, clk)
	ck := s.Cookie(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 80, UserID: 1, SessionHash: "h"})
	if strings.ContainsAny(ck, " ;,") {
		t.Fatalf("cookie value has separators: %q", ck)
	}
	for i := 0; i < 3; i++ { // Cookie 可重复验证
		if _, err := s.VerifyCookie(ck); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(ingress.CookieTTL + time.Second)
	if _, err := s.VerifyCookie(ck); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("expired cookie = %v", err)
	}
}
```

`internal/ingress/auth_test.go`：

```go
package ingress_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/ingress"
	"spark-cloud/internal/platform/clock"
)

func newAuth(t *testing.T, base, domain string, secure bool) *ingress.Auth {
	t.Helper()
	a, err := ingress.NewAuth(secret, base, domain, secure, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestParseTargetRejects(t *testing.T) {
	a := newAuth(t, "http://spark.test:18080", "spark.test", false)
	tg, err := a.ParseTarget("http://ins-a1b2c3d4-8888.spark.test:18080/lab/tree?x=1#frag")
	if err != nil || tg.InstanceID != "ins-a1b2c3d4" || tg.Port != 8888 || tg.Path != "/lab/tree?x=1" {
		t.Fatalf("target = %+v %v", tg, err)
	}
	if tg, _ := a.ParseTarget("http://ins-a1b2c3d4-8888.spark.test:18080"); tg.Path != "/" {
		t.Fatalf("empty path → %q", tg.Path)
	}
	for _, bad := range []string{
		"", "lab", "https://ins-a1b2c3d4-8888.spark.test:18080/", "http://ins-a1b2c3d4-8888.spark.test/", "http://ins-a1b2c3d4-8888.spark.test:1/",
		"http://user:pw@ins-a1b2c3d4-8888.spark.test:18080/", "http://spark.test:18080/market", "http://ins-a1b2c3d4-8888.evil.test:18080/",
		"http://ins-a1b2c3d4-8888.spark.test.evil.test:18080/", "javascript:alert(1)",
	} {
		if _, err := a.ParseTarget(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	prod := newAuth(t, "https://spark.example", "sparkapp.example", true)
	if tg, err := prod.ParseTarget("https://ins-a1b2c3d4-80.sparkapp.example/"); err != nil || tg.Port != 80 {
		t.Fatalf("prod target = %+v %v", tg, err)
	}
	if _, err := prod.ParseTarget("https://ins-a1b2c3d4-80.sparkapp.example:8443/"); err == nil {
		t.Fatal("non-default port accepted in prod")
	}
}

func TestURLsAndSafeNext(t *testing.T) {
	a := newAuth(t, "http://spark.test:18080", "spark.test", false)
	if got := a.PortURL("ins-a1b2c3d4", 8888); got != "http://ins-a1b2c3d4-8888.spark.test:18080/" {
		t.Fatalf("port url = %s", got)
	}
	if got := a.AuthorizeURL("http://ins-a1b2c3d4-8888.spark.test:18080/lab?x=1"); got != "http://spark.test:18080/api/v1/ingress/authorize?target="+url.QueryEscape("http://ins-a1b2c3d4-8888.spark.test:18080/lab?x=1") {
		t.Fatalf("authorize url = %s", got)
	}
	cb := a.CallbackURL(ingress.Target{InstanceID: "ins-a1b2c3d4", Port: 8888, Path: "/lab?x=1"}, "T.S")
	u, _ := url.Parse(cb)
	if u.Host != "ins-a1b2c3d4-8888.spark.test:18080" || u.Path != "/.spark/callback" || u.Query().Get("ticket") != "T.S" || u.Query().Get("next") != "/lab?x=1" {
		t.Fatalf("callback url = %s", cb)
	}
	prod := newAuth(t, "https://spark.example", "sparkapp.example", true)
	if got := prod.PortURL("ins-a1b2c3d4", 80); got != "https://ins-a1b2c3d4-80.sparkapp.example/" {
		t.Fatalf("prod port url = %s", got)
	}
	for in, want := range map[string]string{"/lab": "/lab", "/a?b=c": "/a?b=c", "": "/", "//evil.example/x": "/", "/\\evil.example": "/", "http://evil.example": "/", "lab": "/", "/ok#frag": "/ok#frag"} {
		if got := ingress.SafeNext(in); got != want {
			t.Fatalf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCallbackSetsCookieAndLogoutClearsIt(t *testing.T) {
	a := newAuth(t, "http://spark.test:18080", "spark.test", false)
	tk := a.Signer.Ticket(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7, SessionHash: "h"})
	req := httptest.NewRequest("GET", "http://ins-a1b2c3d4-8888.spark.test:18080/.spark/callback?ticket="+url.QueryEscape(tk)+"&next="+url.QueryEscape("//evil.example"), nil)
	rec := httptest.NewRecorder()
	a.HandleCallback(rec, req)
	if rec.Code != 302 || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	var ck *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == ingress.CookieName {
			ck = c
		}
	}
	if ck == nil || !ck.HttpOnly || ck.Path != "/" || ck.Domain != "" || ck.SameSite != http.SameSiteLaxMode || ck.Secure {
		t.Fatalf("cookie = %+v", ck)
	}
	claims, err := a.Signer.VerifyCookie(ck.Value)
	if err != nil || claims.UserID != 7 || claims.Port != 8888 || claims.SessionHash != "h" {
		t.Fatalf("cookie claims = %+v %v", claims, err)
	}
	// 票据与当前 Host 的实例 / 端口不一致 → 403 页
	tk2 := a.Signer.Ticket(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 7860, UserID: 7, SessionHash: "h"})
	req2 := httptest.NewRequest("GET", "http://ins-a1b2c3d4-8888.spark.test:18080/.spark/callback?ticket="+url.QueryEscape(tk2), nil)
	rec2 := httptest.NewRecorder()
	a.HandleCallback(rec2, req2)
	if rec2.Code != 403 || !strings.Contains(rec2.Body.String(), ingress.PageTicketInvalid) {
		t.Fatalf("mismatched ticket = %d %s", rec2.Code, rec2.Body.String())
	}
	// ValidCookie 逐枚验证：第一枚是租户塞的垃圾，第二枚有效
	req3 := httptest.NewRequest("GET", "http://ins-a1b2c3d4-8888.spark.test:18080/lab", nil)
	req3.AddCookie(&http.Cookie{Name: ingress.CookieName, Value: "junk"})
	req3.AddCookie(&http.Cookie{Name: ingress.CookieName, Value: ck.Value})
	if got, ok := a.ValidCookie(req3, "ins-a1b2c3d4", 8888); !ok || got.UserID != 7 {
		t.Fatalf("valid cookie not found among multiple: %+v %v", got, ok)
	}
	if _, ok := a.ValidCookie(req3, "ins-a1b2c3d4", 7860); ok {
		t.Fatal("cookie for other port accepted")
	}
	// logout：POST 清 Cookie，GET 405
	rec4 := httptest.NewRecorder()
	a.HandleLogout(rec4, httptest.NewRequest("POST", "http://ins-a1b2c3d4-8888.spark.test:18080/.spark/logout", nil))
	cleared := false
	for _, c := range rec4.Result().Cookies() {
		if c.Name == ingress.CookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if rec4.Code != 200 || !cleared {
		t.Fatalf("logout = %d cleared=%v", rec4.Code, cleared)
	}
	rec5 := httptest.NewRecorder()
	a.HandleLogout(rec5, httptest.NewRequest("GET", "http://ins-a1b2c3d4-8888.spark.test:18080/.spark/logout", nil))
	if rec5.Code != 405 {
		t.Fatalf("GET logout = %d", rec5.Code)
	}
	// Secure 环境 Cookie 带 Secure
	sec := newAuth(t, "https://spark.example", "spark.example", true)
	tk3 := sec.Signer.Ticket(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 80, UserID: 1, SessionHash: "h"})
	rec6 := httptest.NewRecorder()
	sec.HandleCallback(rec6, httptest.NewRequest("GET", "https://ins-a1b2c3d4-80.spark.example/.spark/callback?ticket="+url.QueryEscape(tk3)+"&next=/x", nil))
	if cs := rec6.Result().Cookies(); len(cs) != 1 || !cs[0].Secure || rec6.Header().Get("Location") != "/x" {
		t.Fatalf("secure callback = %+v %s", cs, rec6.Header().Get("Location"))
	}
}
```

`internal/api/ingress_test.go`：

```go
package api_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"spark-cloud/internal/platform/dbtest"
)

func TestIngressAuthorizeFlow(t *testing.T) {
	e := newTestEnv(t)
	dbtest.CreateMachine(t, e.pool, "SN001")
	c := e.client()
	user := c.login("13800000031")
	c.do("POST", "/kyc/personal", map[string]string{"name": "张三", "id_number": "11010119900101123X"})
	e.wallet.AdminAdjust(t.Context(), 1, int64(user["id"].(float64)), 100000, "fund", "")
	_, out := c.do("POST", "/orders", map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": dbtest.ImageID(t, e.pool)})
	id := out["instance"].(map[string]any)["id"].(string)
	target := "http://ins-" + strings.TrimPrefix(id, "ins-") + "-8888.spark.test/lab?x=1"
	noRedirect := &http.Client{Jar: c.http.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(q string) *http.Response {
		resp, err := noRedirect.Get(e.ts.URL + "/api/v1/ingress/authorize?" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	// 主人 → 302 到 callback，票据可验
	resp := get("target=" + url.QueryEscape(target))
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 302 || loc.Host != "ins-"+strings.TrimPrefix(id, "ins-")+"-8888.spark.test" || loc.Path != "/.spark/callback" || loc.Query().Get("next") != "/lab?x=1" {
		t.Fatalf("owner = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	claims, err := e.ingress.Signer.VerifyTicket(loc.Query().Get("ticket"))
	if err != nil || claims.InstanceID != id || claims.Port != 8888 || claims.UserID != int64(user["id"].(float64)) || claims.SessionHash == "" {
		t.Fatalf("ticket = %+v %v", claims, err)
	}
	// 非法 target → 400 页
	if resp := get("target=" + url.QueryEscape("http://evil.test/")); resp.StatusCode != 400 {
		t.Fatalf("bad target = %d", resp.StatusCode)
	}
	// 另一个用户 → 403 页
	other := e.client()
	other.login("13800000032")
	otherNoRedirect := &http.Client{Jar: other.http.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r2, _ := otherNoRedirect.Get(e.ts.URL + "/api/v1/ingress/authorize?target=" + url.QueryEscape(target))
	r2.Body.Close()
	if r2.StatusCode != 403 || !strings.HasPrefix(r2.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("non-owner = %d %s", r2.StatusCode, r2.Header.Get("Content-Type"))
	}
	// 未登录 → 302 到登录页，redirect 回本端点
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r3, _ := anon.Get(e.ts.URL + "/api/v1/ingress/authorize?target=" + url.QueryEscape(target))
	r3.Body.Close()
	want := "http://spark.test/login?redirect=" + url.QueryEscape("/api/v1/ingress/authorize?target="+url.QueryEscape(target))
	if r3.StatusCode != 302 || r3.Header.Get("Location") != want {
		t.Fatalf("anon = %d %s, want %s", r3.StatusCode, r3.Header.Get("Location"), want)
	}
}
```

`api_test.go` 的 `testEnv` 增加字段 `ingress *ingress.Auth`；`newTestEnvWith` 里构造 `auth, _ := ingress.NewAuth(dbtest.Key, "http://spark.test", "spark.test", false, clk)`，`api.Server{...}` 加 `Ingress: auth`，返回的 `testEnv` 带 `ingress: auth`（import `"spark-cloud/internal/ingress"`）。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ingress/ ./internal/api/ -run 'TestTicket|TestCookie|TestParseTarget|TestURLs|TestCallback|TestIngressAuthorize' 2>&1 | head`
Expected: 编译错误。

- [ ] **Step 3: `token.go`**

```go
package ingress

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/hkdf"

	"spark-cloud/internal/platform/clock"
)

const (
	TicketTTL = 60 * time.Second
	CookieTTL = 12 * time.Hour
)

// ErrToken：票据 / Cookie 验签失败、过期或已使用。
var ErrToken = errors.New("ingress: invalid token")

type Claims struct {
	InstanceID  string `json:"i"`
	Port        int    `json:"p"`
	UserID      int64  `json:"u"`
	SessionHash string `json:"s"`
	Exp         int64  `json:"e"`
	Nonce       string `json:"n,omitempty"`
}

// Signer 用 SPARK_SECRET_KEY 派生两把 HMAC 密钥：票据（60 秒、单次）与 Cookie（12 小时、可重复）。
type Signer struct {
	ticketKey []byte
	cookieKey []byte
	clk       clock.Clock
	mu        sync.Mutex
	used      map[string]time.Time // nonce → 过期时间（进程内，单次使用）
}

func derive(secret []byte, info string) []byte {
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, nil, []byte(info)), key); err != nil {
		panic(err)
	}
	return key
}

func NewSigner(secret []byte, clk clock.Clock) *Signer {
	return &Signer{ticketKey: derive(secret, "spark-ingress-ticket"), cookieKey: derive(secret, "spark-ingress-cookie"), clk: clk, used: map[string]time.Time{}}
}

func sign(key []byte, c Claims) string {
	body, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verify(key []byte, tok string, now time.Time) (Claims, error) {
	i := strings.IndexByte(tok, '.')
	if i <= 0 {
		return Claims{}, ErrToken
	}
	body, err := base64.RawURLEncoding.DecodeString(tok[:i])
	if err != nil {
		return Claims{}, ErrToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(tok[i+1:])
	if err != nil {
		return Claims{}, ErrToken
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return Claims{}, ErrToken
	}
	var c Claims
	if err := json.Unmarshal(body, &c); err != nil || c.InstanceID == "" || c.Port <= 0 || c.UserID <= 0 || now.Unix() >= c.Exp {
		return Claims{}, ErrToken
	}
	return c, nil
}

func (s *Signer) Ticket(c Claims) string {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	c.Exp = s.clk.Now().Add(TicketTTL).Unix()
	c.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
	return sign(s.ticketKey, c)
}

// VerifyTicket 验签 + 未过期 + nonce 未用过（用过的 nonce 保留到过期）。
func (s *Signer) VerifyTicket(tok string) (Claims, error) {
	now := s.clk.Now()
	c, err := verify(s.ticketKey, tok, now)
	if err != nil {
		return Claims{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for n, exp := range s.used {
		if now.After(exp) {
			delete(s.used, n)
		}
	}
	if _, dup := s.used[c.Nonce]; dup || c.Nonce == "" {
		return Claims{}, ErrToken
	}
	s.used[c.Nonce] = time.Unix(c.Exp, 0)
	return c, nil
}

func (s *Signer) Cookie(c Claims) string {
	c.Exp = s.clk.Now().Add(CookieTTL).Unix()
	c.Nonce = ""
	return sign(s.cookieKey, c)
}

func (s *Signer) VerifyCookie(tok string) (Claims, error) { return verify(s.cookieKey, tok, s.clk.Now()) }
```

- [ ] **Step 4: `auth.go`**

```go
package ingress

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"spark-cloud/internal/platform/clock"
)

const CookieName = "sc_ingress"

// Target 是用户最初想访问的实例地址。
type Target struct {
	InstanceID string
	Port       int
	Path       string // 含 query，以 / 开头
}

// Auth 负责票据跳转与每实例 Cookie；所有对外 URL 从 SPARK_PUBLIC_BASE_URL 推导，不信任请求里的 Host。
type Auth struct {
	Signer        *Signer
	BaseURL       *url.URL // scheme + host[:port]
	IngressDomain string
	SecureCookie  bool
}

func NewAuth(secret []byte, publicBaseURL, ingressDomain string, secureCookie bool, clk clock.Clock) (*Auth, error) {
	u, err := url.Parse(publicBaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("ingress: bad public base url %q", publicBaseURL)
	}
	return &Auth{Signer: NewSigner(secret, clk), BaseURL: &url.URL{Scheme: u.Scheme, Host: u.Host}, IngressDomain: strings.ToLower(ingressDomain), SecureCookie: secureCookie}, nil
}

// basePort 返回对外地址的端口（默认端口为空串）。
func (a *Auth) basePort() string { return a.BaseURL.Port() }

// PortURL：scheme://ins-<id>-<port>.<ingress_domain>[:base 端口]/
func (a *Auth) PortURL(instanceID string, port int) string {
	host := fmt.Sprintf("%s-%d.%s", instanceID, port, a.IngressDomain)
	if p := a.basePort(); p != "" {
		host = net.JoinHostPort(host, p)
	}
	return (&url.URL{Scheme: a.BaseURL.Scheme, Host: host, Path: "/"}).String()
}

func (a *Auth) AuthorizeURL(target string) string {
	return a.BaseURL.String() + "/api/v1/ingress/authorize?target=" + url.QueryEscape(target)
}

func (a *Auth) CallbackURL(t Target, ticket string) string {
	u, _ := url.Parse(a.PortURL(t.InstanceID, t.Port))
	u.Path = "/.spark/callback"
	q := url.Values{"ticket": {ticket}, "next": {t.Path}}
	u.RawQuery = q.Encode()
	return u.String()
}

// ParseTarget 校验 authorize 的 target：scheme 与端口等于对外地址推导值、host 是本平台的实例域名、无用户名密码段。
func (a *Auth) ParseTarget(raw string) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != a.BaseURL.Scheme || u.User != nil || u.Host == "" {
		return Target{}, errors.New("ingress: bad target")
	}
	if u.Port() != a.basePort() {
		return Target{}, errors.New("ingress: bad target port")
	}
	id, port, ok := ParseHost(u.Host, a.IngressDomain)
	if !ok {
		return Target{}, errors.New("ingress: target is not an instance host")
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return Target{InstanceID: id, Port: port, Path: path}, nil
}

// SafeNext 与控制台 safeRedirect 同规则：以 / 开头、不以 // 开头、不含反斜杠、解析后无 scheme / host；否则回 /。
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, `\`) {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

func (a *Auth) setCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: a.SecureCookie, SameSite: http.SameSiteLaxMode})
}

// ValidCookie 逐枚验证同名 Cookie（租户页面可用 Domain 属性塞一枚无效的同名 Cookie），任一通过且实例 / 端口匹配即可。
func (a *Auth) ValidCookie(r *http.Request, instanceID string, port int) (Claims, bool) {
	for _, c := range r.CookiesNamed(CookieName) {
		claims, err := a.Signer.VerifyCookie(c.Value)
		if err == nil && claims.InstanceID == instanceID && claims.Port == port {
			return claims, true
		}
	}
	return Claims{}, false
}

// HandleCallback：验票据（必须与当前 Host 的实例 / 端口一致）→ 发 Cookie → 302 到 next。
func (a *Auth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	id, port, ok := ParseHost(r.Host, a.IngressDomain)
	if !ok {
		WritePage(w, http.StatusNotFound, "页面不存在", PageNotOpen, 0)
		return
	}
	claims, err := a.Signer.VerifyTicket(r.URL.Query().Get("ticket"))
	if err != nil || claims.InstanceID != id || claims.Port != port {
		WritePage(w, http.StatusForbidden, "无法访问", PageTicketInvalid, 0)
		return
	}
	a.setCookie(w, a.Signer.Cookie(claims), int(CookieTTL/1e9))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, SafeNext(r.URL.Query().Get("next")), http.StatusFound)
}

// HandleLogout：POST 清 Cookie（GET 可被任意页面触发，拒绝）。
func (a *Auth) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		WritePage(w, http.StatusMethodNotAllowed, "方法不允许", "请用 POST 退出", 0)
		return
	}
	a.setCookie(w, "", -1)
	WritePage(w, http.StatusOK, "已退出", "已清除此实例的访问凭证", 0)
}
```

- [ ] **Step 5: 控制台 `authorize` 端点**

`internal/api/ingress.go`：

```go
package api

import (
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"spark-cloud/internal/identity"
	"spark-cloud/internal/ingress"
	"spark-cloud/internal/platform/apperr"
)

const defaultAuthorizeLimit = 60 // 每用户每分钟

// authorizeLimiter 进程内按用户限流 authorize（票据签发）。
type authorizeLimiter struct {
	mu    sync.Mutex
	limit int
	hits  map[int64][]time.Time
}

func (l *authorizeLimiter) allow(userID int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[int64][]time.Time{}
	}
	ts := l.hits[userID]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > time.Minute {
		i++
	}
	ts = ts[i:]
	if len(ts) >= l.limit {
		l.hits[userID] = ts
		return false
	}
	l.hits[userID] = append(ts, now)
	return true
}

// handleIngressAuthorize：控制台域上的票据签发。未登录 → 登录页（redirect 回本端点）；非主人 → 403 页；主人 → 302 到实例域名的 callback。
func (s *Server) handleIngressAuthorize(w http.ResponseWriter, r *http.Request) {
	rawTarget := r.URL.Query().Get("target")
	t, err := s.Ingress.ParseTarget(rawTarget)
	if err != nil {
		ingress.WritePage(w, http.StatusBadRequest, "跳转目标不正确", ingress.PageBadTarget, 0)
		return
	}
	token := ""
	if c, err := r.Cookie(s.cookieName()); err == nil {
		token = c.Value
	}
	u, err := s.Identity.Authenticate(r.Context(), token)
	if err != nil {
		if errors.Is(err, apperr.ErrUnauthorized) {
			self := "/api/v1/ingress/authorize?target=" + url.QueryEscape(rawTarget)
			http.Redirect(w, r, s.PublicBaseURL+"/login?redirect="+url.QueryEscape(self), http.StatusFound)
			return
		}
		ingress.WritePage(w, http.StatusForbidden, "无法访问", ingress.PageForbidden, 0)
		return
	}
	if !s.authorizeLim.allow(u.ID, s.Clock.Now()) {
		s.writeError(w, r, apperr.ErrRateLimited)
		return
	}
	if _, err := s.Rental.GetForUser(r.Context(), u.ID, t.InstanceID); err != nil {
		ingress.WritePage(w, http.StatusForbidden, "无法访问", ingress.PageForbidden, 0)
		return
	}
	ticket := s.Ingress.Signer.Ticket(ingress.Claims{InstanceID: t.InstanceID, Port: t.Port, UserID: u.ID, SessionHash: identity.SessionHash(token)})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, s.Ingress.CallbackURL(t, ticket), http.StatusFound)
}
```

`internal/api/server.go`：`Server` 追加 `Ingress *ingress.Auth`、`AuthorizeLimit int // authorize 每用户每分钟上限；零值 60`、私有字段 `authorizeLim *authorizeLimiter`；`Router()` 开头 `limit := s.AuthorizeLimit; if limit <= 0 { limit = defaultAuthorizeLimit }; s.authorizeLim = &authorizeLimiter{limit: limit}`；在 `/api/v1` 路由里 `r.Post("/pay/notify/{channel}", s.handlePayNotify)` 之后加

```go
		if s.Ingress != nil {
			r.With(httpx.NoStore).Get("/ingress/authorize", s.handleIngressAuthorize) // 组外：未登录要 302 到登录页而不是 JSON 401
		}
```

`internal/app/app.go`：`Build` 里 `api.Server{...}` 之前

```go
	ingressAuth, err := ingress.NewAuth(cfg.SecretKey, cfg.PublicBaseURL, cfg.IngressDomain, cfg.Env != "dev", clk)
	if err != nil {
		return nil, err
	}
```

`api.Server{...}` 加 `Ingress: ingressAuth`，`DisableRateLimits` 时 `AuthorizeLimit: noRateLimit`；`App` 增加字段 `IngressAuth *ingress.Auth`（Task 18 组装 ingress.Server 时复用）。

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/ingress/ ./internal/api/ ./internal/app/ 2>&1 | tail -5`
Expected: `ok`。

- [ ] **Step 7: 提交**

```bash
git add internal/ingress internal/api internal/app
git commit -m "feat(ingress): signed tickets and per-instance cookies, target/next validation, callback/logout, console authorize endpoint

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 17: `ingress.Server`：按实例隔离的反向代理、私有 / 公开判定、会话绑定、期限清除、WebSocket、502

**Files:**
- Create: `internal/ingress/server.go`、`internal/ingress/server_test.go`

**Interfaces:**
- Consumes: Task 15 / 16；Task 8 `upstream.Target` / `upstream.DialError`。
- Produces:
  - `ingress.SessionChecker interface { SessionUserID(ctx, hash string) (int64, error) }`（`*identity.Service` 满足）。
  - `ingress.Dialer interface { Dial(ctx, t upstream.Target, port int) (net.Conn, error) }`（`*upstream.Pool` 满足）。
  - `ingress.Alerts interface { RaiseUpstreamUnreachable(ctx, instanceID string) error }`（`*rental.Service` 满足）。
  - `ingress.NewServer(auth *Auth, routes *Table, sessions SessionChecker, dialer Dialer, alerts Alerts, clk clock.Clock, log *slog.Logger) *Server`；`(*Server).ServeHTTP`。
  - 行为：未知 Host → 404；`/.spark/callback`、`/.spark/logout`、`/.spark/healthz`；查不到端口 → 404 `PageNotOpen`；非 running / 暂停 → 503（`refresh=10`）；私有端口校验 Cookie（实例 / 端口 / 主人 / 会话仍有效，会话查询缓存 2 秒）否则 302 到 `AuthorizeURL(原始 URL)`；转发：出站 `http://<instance_id>:<port>`、`MaxIdleConnsPerHost 8`、剥离 `sc_ingress`、`X-Forwarded-*`、保留 Host、`FlushInterval -1`、清除 Read/Write 期限；上游拨号失败 → 502 + `upstream_unreachable`（进程内 5 分钟节流）。

- [ ] **Step 1: 写失败的测试**

`internal/ingress/server_test.go`：

```go
package ingress_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/ingress"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
)

type fakeDialer struct {
	mu      sync.Mutex
	targets map[string]string // instance → httptest 地址
	dials   []string
}

func (d *fakeDialer) Dial(_ context.Context, t upstream.Target, port int) (net.Conn, error) {
	d.mu.Lock()
	addr, ok := d.targets[t.InstanceID]
	d.dials = append(d.dials, fmt.Sprintf("%s:%d", t.InstanceID, port))
	d.mu.Unlock()
	if !ok {
		return nil, &upstream.DialError{InstanceID: t.InstanceID, Err: errors.New("dial tcp: connection refused")}
	}
	return net.Dial("tcp", addr)
}

type fakeSessions struct {
	mu    sync.Mutex
	users map[string]int64
	calls int
}

func (f *fakeSessions) SessionUserID(_ context.Context, hash string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if id, ok := f.users[hash]; ok {
		return id, nil
	}
	return 0, apperr.ErrUnauthorized
}

type fakeAlerts struct {
	mu    sync.Mutex
	raise []string
}

func (f *fakeAlerts) RaiseUpstreamUnreachable(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raise = append(f.raise, id)
	return nil
}

type env struct {
	t      *testing.T
	clk    *clock.Fake
	auth   *ingress.Auth
	src    *memSource
	table  *ingress.Table
	dialer *fakeDialer
	sess   *fakeSessions
	alerts *fakeAlerts
	ts     *httptest.Server
	host   string // ingress 监听的 host:port
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.WriteTimeout = time.Second
	ts.Config.ReadTimeout = 2 * time.Second
	host := ts.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(host)
	auth, err := ingress.NewAuth(secret, "http://spark.test:"+port, "spark.test", false, clk)
	if err != nil {
		t.Fatal(err)
	}
	src := &memSource{}
	table := ingress.NewTable(src, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e := &env{t: t, clk: clk, auth: auth, src: src, table: table, dialer: &fakeDialer{targets: map[string]string{}},
		sess: &fakeSessions{users: map[string]int64{"sess-7": 7, "sess-8": 8}}, alerts: &fakeAlerts{}, ts: ts, host: host}
	ts.Config.Handler = ingress.NewServer(auth, table, e.sess, e.dialer, e.alerts, clk, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts.Start()
	t.Cleanup(ts.Close)
	return e
}

// route 登记一个实例端口并指向 handler。
func (e *env) route(id string, userID int64, port int, visibility, state string, h http.Handler) {
	e.t.Helper()
	up := httptest.NewServer(h)
	e.t.Cleanup(up.Close)
	e.dialer.mu.Lock()
	e.dialer.targets[id] = strings.TrimPrefix(up.URL, "http://")
	e.dialer.mu.Unlock()
	e.src.mu.Lock()
	e.src.routes = append(e.src.routes, rental.IngressRoute{InstanceID: id, UserID: userID, Port: port, Visibility: visibility, State: state, LanIP: "127.0.0.1", HostPubkey: "k"})
	e.src.mu.Unlock()
	if err := e.table.Refresh(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// get 以给定 Host 头请求 ingress，不跟随重定向。
func (e *env) get(hostHeader, path string, cookies ...*http.Cookie) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest("GET", "http://"+e.host+path, nil)
	req.Host = hostHeader
	for _, c := range cookies {
		req.AddCookie(c)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func body(resp *http.Response) string {
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func (e *env) cookie(id string, port int, userID int64, sess string) *http.Cookie {
	return &http.Cookie{Name: ingress.CookieName, Value: e.auth.Signer.Cookie(ingress.Claims{InstanceID: id, Port: port, UserID: userID, SessionHash: sess})}
}

func echo(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "host=%s path=%s cookie=%s xff=%s", r.Host, r.URL.RequestURI(), r.Header.Get("Cookie"), r.Header.Get("X-Forwarded-For"))
}

func TestPublicPortProxiesAndStripsIngressCookie(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(echo))
	h := "ins-a1b2c3d4-8888.spark.test"
	resp := e.get(h, "/lab?x=1", &http.Cookie{Name: "sc_ingress", Value: "secret"}, &http.Cookie{Name: "app", Value: "keep"})
	b := body(resp)
	if resp.StatusCode != 200 || !strings.Contains(b, "host="+h) || !strings.Contains(b, "path=/lab?x=1") || !strings.Contains(b, "cookie=app=keep") || strings.Contains(b, "secret") || !strings.Contains(b, "xff=127.0.0.1") {
		t.Fatalf("proxied = %d %q", resp.StatusCode, b)
	}
	if r := e.get("INS-A1B2C3D4-8888.SPARK.TEST.", "/"); r.StatusCode != 200 {
		t.Fatalf("normalized host = %d", r.StatusCode)
	}
	if r := e.get("ins-a1b2c3d4-9999.spark.test", "/"); r.StatusCode != 404 || !strings.Contains(body(r), ingress.PageNotOpen) {
		t.Fatalf("unknown port = %d", r.StatusCode)
	}
	if r := e.get("other.test", "/"); r.StatusCode != 404 {
		t.Fatalf("unknown host = %d", r.StatusCode)
	}
	if r := e.get(h, "/.spark/healthz"); r.StatusCode != 200 || body(r) != "ok" {
		t.Fatal("healthz")
	}
	if r := e.get(h, "/.spark/unknown"); r.StatusCode != 404 {
		t.Fatal("reserved path leaked to tenant")
	}
}

func TestPrivatePortRequiresCookieBoundToOwnerAndSession(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "private", rental.StateRunning, http.HandlerFunc(echo))
	h := "ins-a1b2c3d4-8888.spark.test"
	resp := e.get(h, "/lab?x=1")
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 302 || loc != e.auth.AuthorizeURL("http://"+h+":"+e.auth.BaseURL.Port()+"/lab?x=1") {
		t.Fatalf("no cookie = %d %s", resp.StatusCode, loc)
	}
	if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")); r.StatusCode != 200 {
		t.Fatalf("owner cookie = %d", r.StatusCode)
	}
	if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 8888, 8, "sess-8")); r.StatusCode != 302 {
		t.Fatalf("other user's cookie = %d", r.StatusCode)
	}
	if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 7860, 7, "sess-7")); r.StatusCode != 302 {
		t.Fatalf("cookie for other port = %d", r.StatusCode)
	}
	if r := e.get(h, "/lab", &http.Cookie{Name: ingress.CookieName, Value: "junk"}, e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")); r.StatusCode != 200 {
		t.Fatalf("second cookie ignored = %d", r.StatusCode)
	}
	// 控制台登出：会话失效 → 2 秒缓存过后 302
	e.sess.mu.Lock()
	delete(e.sess.users, "sess-7")
	e.sess.mu.Unlock()
	if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")); r.StatusCode != 200 {
		t.Fatalf("within cache window = %d", r.StatusCode)
	}
	e.clk.Advance(3 * time.Second)
	if r := e.get(h, "/lab", e.cookie("ins-a1b2c3d4", 8888, 7, "sess-7")); r.StatusCode != 302 {
		t.Fatalf("after logout = %d", r.StatusCode)
	}
}

func TestStatePagesAndUpstreamErrors(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateStopped, http.HandlerFunc(echo))
	r := e.get("ins-a1b2c3d4-8888.spark.test", "/")
	if b := body(r); r.StatusCode != 503 || !strings.Contains(b, ingress.PageStopped) || !strings.Contains(b, `http-equiv="refresh" content="10"`) {
		t.Fatalf("stopped = %d %q", r.StatusCode, b)
	}
	e.src.mu.Lock()
	e.src.routes[0].State, e.src.routes[0].Suspended = rental.StateRunning, true
	e.src.mu.Unlock()
	e.table.Refresh(context.Background())
	if r := e.get("ins-a1b2c3d4-8888.spark.test", "/"); r.StatusCode != 503 || !strings.Contains(body(r), ingress.PageSuspended) {
		t.Fatalf("suspended = %d", r.StatusCode)
	}
	// 上游拨不通：502 + 告警一次（5 分钟节流）
	e.src.mu.Lock()
	e.src.routes[0].Suspended = false
	e.src.routes = append(e.src.routes, rental.IngressRoute{InstanceID: "ins-dead0000", UserID: 7, Port: 80, Visibility: "public", State: rental.StateRunning, LanIP: "127.0.0.1"})
	e.src.mu.Unlock()
	e.table.Refresh(context.Background())
	for i := 0; i < 2; i++ {
		if r := e.get("ins-dead0000-80.spark.test", "/"); r.StatusCode != 502 || !strings.Contains(body(r), "无法连接到实例端口") {
			t.Fatalf("unreachable = %d", r.StatusCode)
		}
	}
	e.alerts.mu.Lock()
	n := len(e.alerts.raise)
	e.alerts.mu.Unlock()
	if n != 1 {
		t.Fatalf("alerts = %d", n)
	}
}

func TestUpstreamCloseMidResponseReturns502OrTruncates(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial"))
		conn.Close()
	}))
	resp := e.get("ins-a1b2c3d4-8888.spark.test", "/")
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if resp.StatusCode == 200 && err == nil && len(b) == 100 {
		t.Fatal("truncated upstream response delivered as complete")
	}
	// 之后的请求照常
	e.route("ins-b2c3d4e5", 7, 80, "public", rental.StateRunning, http.HandlerFunc(echo))
	if r := e.get("ins-b2c3d4e5-80.spark.test", "/"); r.StatusCode != 200 {
		t.Fatalf("next request = %d", r.StatusCode)
	}
}

func TestTenantsDoNotShareConnections(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"ins-aaaaaaaa", "ins-bbbbbbbb"} {
		id := id
		e.route(id, 7, 8888, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, id) }))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 100; i++ {
		for _, id := range []string{"ins-aaaaaaaa", "ins-bbbbbbbb"} {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				r := e.get(id+"-8888.spark.test", "/")
				if b := body(r); b != id {
					errs <- fmt.Errorf("%s got %q", id, b)
				}
			}(id)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestStreamingNotCutByWriteTimeout(t *testing.T) {
	e := newEnv(t) // ingress http.Server 的 WriteTimeout 是 1 秒
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
			time.Sleep(400 * time.Millisecond)
		}
	}))
	resp := e.get("ins-a1b2c3d4-8888.spark.test", "/events")
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || strings.Count(string(b), "data:") != 5 {
		t.Fatalf("stream = %q %v", b, err)
	}
}

func TestWebSocketUpgradePassthrough(t *testing.T) {
	e := newEnv(t)
	e.route("ins-a1b2c3d4", 7, 8888, "public", rental.StateRunning, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			w.WriteHeader(400)
			return
		}
		hj, _ := w.(http.Hijacker)
		conn, rw, _ := hj.Hijack()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		io.Copy(conn, conn) // 原样回显
		conn.Close()
	}))
	nc, err := net.Dial("tcp", e.host)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	fmt.Fprint(nc, "GET /ws HTTP/1.1\r\nHost: ins-a1b2c3d4-8888.spark.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: x\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade = %v %v", resp, err)
	}
	fmt.Fprint(nc, "hello-ws")
	buf := make([]byte, 8)
	nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "hello-ws" {
		t.Fatalf("echo = %q %v", buf, err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ingress/ -run 'TestPublicPort|TestPrivatePort|TestStatePages|TestUpstreamClose|TestTenants|TestStreaming|TestWebSocket' 2>&1 | head`
Expected: 编译错误（`NewServer` 未定义）。

- [ ] **Step 3: 实现 `server.go`**

```go
package ingress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
)

const (
	sessionCacheTTL  = 2 * time.Second
	unreachThrottle  = 5 * time.Minute
	responseHeaderTO = 60 * time.Second
	maxIdlePerHost   = 8
)

type SessionChecker interface {
	SessionUserID(ctx context.Context, hash string) (int64, error)
}

type Dialer interface {
	Dial(ctx context.Context, t upstream.Target, port int) (net.Conn, error)
}

type Alerts interface {
	RaiseUpstreamUnreachable(ctx context.Context, instanceID string) error
}

type Server struct {
	auth   *Auth
	routes *Table
	sess   SessionChecker
	dialer Dialer
	alerts Alerts
	clk    clock.Clock
	log    *slog.Logger
	proxy  *httputil.ReverseProxy

	mu          sync.Mutex
	sessCache   map[string]cachedSession
	lastUnreach map[string]time.Time
}

type cachedSession struct {
	userID int64
	until  time.Time
}

type ctxKey struct{}

// requestRoute 随请求传给 Transport.DialContext / ErrorHandler。
type requestRoute struct {
	route rental.IngressRoute
}

func NewServer(auth *Auth, routes *Table, sessions SessionChecker, dialer Dialer, alerts Alerts, clk clock.Clock, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{auth: auth, routes: routes, sess: sessions, dialer: dialer, alerts: alerts, clk: clk, log: log,
		sessCache: map[string]cachedSession{}, lastUnreach: map[string]time.Time{}}
	tr := &http.Transport{
		// 出站地址是 http://<instance_id>:<port>：Transport 以 scheme+host 作为空闲连接池的键，实例之间绝不共用连接。
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			id, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, _ := strconv.Atoi(portStr)
			r, ok := s.routes.Lookup(id, port)
			if !ok {
				return nil, fmt.Errorf("route %s:%d vanished", id, port)
			}
			return s.dialer.Dial(ctx, upstream.Target{InstanceID: r.InstanceID, LanIP: r.LanIP, HostPubkey: r.HostPubkey}, port)
		},
		MaxIdleConnsPerHost:   maxIdlePerHost,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: responseHeaderTO,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
	}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rr := pr.In.Context().Value(ctxKey{}).(requestRoute)
			pr.SetXForwarded()
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort(rr.route.InstanceID, strconv.Itoa(rr.route.Port))
			pr.Out.Host = pr.In.Host // 租户应用看到用户访问的域名
			// 剥离 sc_ingress（租户应用不该拿到），其它 Cookie 原样
			pr.Out.Header.Del("Cookie")
			for _, c := range pr.In.Cookies() {
				if c.Name != CookieName {
					pr.Out.AddCookie(c)
				}
			}
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorHandler:  s.proxyError,
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, port, ok := ParseHost(r.Host, s.auth.IngressDomain)
	if !ok {
		WritePage(w, http.StatusNotFound, "页面不存在", PageNotOpen, 0)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/.spark/") {
		switch r.URL.Path {
		case "/.spark/callback":
			s.auth.HandleCallback(w, r)
		case "/.spark/logout":
			s.auth.HandleLogout(w, r)
		case "/.spark/healthz":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("ok"))
		default:
			WritePage(w, http.StatusNotFound, "页面不存在", "该路径由平台保留", 0)
		}
		return
	}
	route, ok := s.routes.Lookup(id, port)
	if !ok {
		WritePage(w, http.StatusNotFound, "端口未开放", PageNotOpen, 0)
		return
	}
	if route.State != rental.StateRunning || route.Suspended {
		WritePage(w, http.StatusServiceUnavailable, "暂时无法访问", NotRunningPage(route), 10)
		return
	}
	if route.Visibility != "public" {
		claims, ok := s.auth.ValidCookie(r, id, port)
		if !ok || claims.UserID != route.UserID || !s.sessionValid(r.Context(), claims.SessionHash, claims.UserID) {
			original := strings.TrimSuffix(s.auth.PortURL(id, port), "/") + r.URL.RequestURI()
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, s.auth.AuthorizeURL(original), http.StatusFound)
			return
		}
	}
	// 长连接 / SSE / WebSocket 不受 app.Servers() 的 Read/WriteTimeout 约束（控制台路径保持原有超时）
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Time{})
	rc.SetWriteDeadline(time.Time{})
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, requestRoute{route: route})))
}

// sessionValid：Cookie 绑定的控制台会话仍有效（登出即失效）；结果缓存 2 秒。
func (s *Server) sessionValid(ctx context.Context, hash string, userID int64) bool {
	now := s.clk.Now()
	s.mu.Lock()
	if c, ok := s.sessCache[hash]; ok && now.Before(c.until) {
		s.mu.Unlock()
		return c.userID == userID
	}
	s.mu.Unlock()
	uid, err := s.sess.SessionUserID(ctx, hash)
	if err != nil {
		uid = 0
	}
	s.mu.Lock()
	s.sessCache[hash] = cachedSession{userID: uid, until: now.Add(sessionCacheTTL)}
	if len(s.sessCache) > 10000 {
		for k, c := range s.sessCache {
			if !now.Before(c.until) {
				delete(s.sessCache, k)
			}
		}
	}
	s.mu.Unlock()
	return uid != 0 && uid == userID
}

func (s *Server) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	rr, _ := r.Context().Value(ctxKey{}).(requestRoute)
	var de *upstream.DialError
	if errors.As(err, &de) {
		s.log.Warn("ingress upstream unreachable", "instance", rr.route.InstanceID, "port", rr.route.Port, "err", err)
		s.alertUnreachable(rr.route.InstanceID)
	} else {
		s.log.Warn("ingress upstream error", "instance", rr.route.InstanceID, "port", rr.route.Port, "err", err)
	}
	WritePage(w, http.StatusBadGateway, "无法连接", fmt.Sprintf(PageUpstreamFmt, rr.route.Port), 0)
}

func (s *Server) alertUnreachable(instanceID string) {
	now := s.clk.Now()
	s.mu.Lock()
	if last, ok := s.lastUnreach[instanceID]; ok && now.Sub(last) < unreachThrottle {
		s.mu.Unlock()
		return
	}
	s.lastUnreach[instanceID] = now
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.alerts.RaiseUpstreamUnreachable(ctx, instanceID); err != nil {
		s.log.Error("raise upstream_unreachable", "instance", instanceID, "err", err)
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./internal/ingress/ && go test ./internal/ingress/ -race 2>&1 | tail -5`
Expected: 全部 `ok`。`TestUpstreamCloseMidResponseReturns502OrTruncates` 里上游已经发出 200 头再断开：ReverseProxy 无法再改状态码，客户端读到的 body 不足 100 字节且 `io.ReadAll` 返回 `unexpected EOF` 即通过（用例只拒绝「完整 100 字节」这一种错误结果）。

- [ ] **Step 5: 提交**

```bash
git add internal/ingress
git commit -m "feat(ingress): reverse proxy with per-instance connection pools, private/public ports, session-bound cookies, websocket/SSE passthrough

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 18: `HostMux`、TLS 文件热加载、HSTS、301 重定向、组装进 `app` / `cmd/platform`

**Files:**
- Create: `internal/ingress/hostmux.go`、`internal/ingress/hostmux_test.go`
- Create: `internal/platform/tlsfile/loader.go`、`internal/platform/tlsfile/loader_test.go`、`internal/platform/httpx/hsts.go`
- Modify: `internal/app/app.go`、`internal/app/access_test.go`（新建）、`cmd/platform/main.go`

**Interfaces:**
- Produces:
  - `ingress.HostMux(ingressDomain string, ingress, console http.Handler) http.Handler`。
  - `httpx.HSTS(next http.Handler) http.Handler`（`r.TLS != nil` 时加 `Strict-Transport-Security: max-age=31536000`）。
  - `tlsfile.Loader{CertFile, KeyFile string; Interval time.Duration /*0→1m*/; Now func() time.Time /*nil→time.Now*/; OnReloadError func(error); OnExpiring func(notAfter time.Time)}`；`(*Loader).Load() error`、`GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)`、`CheckNow()`、`NotAfter() time.Time`、`TLSConfig() *tls.Config`（`MinVersion: TLS1.2`）、`RunExpiryCheck(ctx, every, warnWithin time.Duration)`。
  - `app.App` 新字段 `Ingress *ingress.Server`、`IngressAuth *ingress.Auth`、`Routes *ingress.Table`、`Upstream *upstream.Pool`、`TLS *tlsfile.Loader`（无证书时 nil）；`App.Handler` 变为 `httpx.HSTS(ingress.HostMux(cfg.IngressDomain, ingressServer, consoleRouter))`；`(*App).Servers(cfg)` 在 `TLS != nil` 时给公网 `http.Server` 设 `TLSConfig`；`(*App).RedirectServer(cfg) *http.Server`（`HTTPRedirectAddr` 为空返回 nil；301 到 `https://<host 去端口><uri>`）；`(*App).RunAccessLoops(ctx)`（路由表每 2 秒刷新、离开 running 的实例从连接池丢弃、TLS 到期每日检查；ctx 结束后关闭连接池）。
  - `cmd/platform`：TLS 时 `ListenAndServeTLS("", "")`；启动重定向服务器（若配置）；`go a.RunAccessLoops(ctx)`。

- [ ] **Step 1: 写失败的测试**

`internal/ingress/hostmux_test.go`：

```go
package ingress_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"spark-cloud/internal/ingress"
)

func TestHostMuxSplitsByHost(t *testing.T) {
	tag := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(name)) })
	}
	h := ingress.HostMux("spark.test", tag("ingress"), tag("console"))
	for host, want := range map[string]string{
		"spark.test": "console", "127.0.0.1:18080": "console", "spark.test:18080": "console", "ssh.spark.test": "console",
		"ins-a1b2c3d4-8888.spark.test": "ingress", "INS-a1b2c3d4-80.spark.test:18080": "ingress", "ins-a1b2c3d4-8888.other.test": "console",
	} {
		req := httptest.NewRequest("GET", "http://x/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Body.String() != want {
			t.Fatalf("host %q → %q, want %q", host, rec.Body.String(), want)
		}
	}
}
```

`internal/platform/tlsfile/loader_test.go`：

```go
package tlsfile_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"spark-cloud/internal/platform/tlsfile"
)

func writeCert(t *testing.T, dir string, notAfter time.Time, serial int64) (certFile, keyFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "spark.test"}, DNSNames: []string{"spark.test", "*.spark.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile
}

func TestLoaderHotReloadKeepsOldOnError(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, time.Now().Add(90*24*time.Hour), 1)
	var reloadErrs int
	now := time.Now()
	l := &tlsfile.Loader{CertFile: certFile, KeyFile: keyFile, Interval: time.Minute, Now: func() time.Time { return now }, OnReloadError: func(error) { reloadErrs++ }}
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	c1, _ := l.GetCertificate(&tls.ClientHelloInfo{})
	if c1.Leaf == nil || c1.Leaf.SerialNumber.Int64() != 1 {
		t.Fatalf("initial cert = %+v", c1)
	}
	// 一分钟内不检查文件
	time.Sleep(20 * time.Millisecond) // 保证 mtime 不同
	writeCert(t, dir, time.Now().Add(90*24*time.Hour), 2)
	if c, _ := l.GetCertificate(&tls.ClientHelloInfo{}); c.Leaf.SerialNumber.Int64() != 1 {
		t.Fatal("reloaded before interval elapsed")
	}
	now = now.Add(2 * time.Minute)
	if c, _ := l.GetCertificate(&tls.ClientHelloInfo{}); c.Leaf.SerialNumber.Int64() != 2 {
		t.Fatalf("not reloaded after interval: serial %d", c.Leaf.SerialNumber.Int64())
	}
	// 坏文件：保留旧证书，回调一次
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(certFile, []byte("garbage"), 0o644)
	l.CheckNow()
	if c, _ := l.GetCertificate(&tls.ClientHelloInfo{}); c.Leaf.SerialNumber.Int64() != 2 || reloadErrs != 1 {
		t.Fatalf("bad reload: serial %d errs %d", c.Leaf.SerialNumber.Int64(), reloadErrs)
	}
	if l.TLSConfig().MinVersion != tls.VersionTLS12 || l.TLSConfig().GetCertificate == nil {
		t.Fatal("tls config")
	}
}

func TestLoaderExpiryCallback(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCert(t, dir, time.Now().Add(10*24*time.Hour), 3)
	var warned []time.Time
	l := &tlsfile.Loader{CertFile: certFile, KeyFile: keyFile, OnExpiring: func(na time.Time) { warned = append(warned, na) }}
	if err := l.Load(); err != nil {
		t.Fatal(err)
	}
	l.CheckExpiry(14 * 24 * time.Hour)
	if len(warned) != 1 || !warned[0].Equal(l.NotAfter()) {
		t.Fatalf("warned = %v", warned)
	}
	far, farKey := writeCert(t, t.TempDir(), time.Now().Add(60*24*time.Hour), 4)
	l2 := &tlsfile.Loader{CertFile: far, KeyFile: farKey, OnExpiring: func(time.Time) { t.Fatal("warned for far expiry") }}
	l2.Load()
	l2.CheckExpiry(14 * 24 * time.Hour)
	if _, err := (&tlsfile.Loader{CertFile: "/nonexistent.crt", KeyFile: keyFile}).GetCertificate(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("missing cert file accepted")
	}
}
```

（`CheckExpiry(warnWithin time.Duration)` 是 `RunExpiryCheck` 每日调用的单步，也作为公开方法供测试。）

`internal/app/access_test.go`：

```go
package app_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"spark-cloud/internal/app"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
)

func TestHandlerSplitsIngressAndConsole(t *testing.T) {
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
	a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)})
	if err != nil {
		t.Fatal(err)
	}
	if a.SSH == nil || a.Ingress == nil || a.Routes == nil || a.Upstream == nil || a.IngressAuth == nil || a.TLS != nil {
		t.Fatalf("access components missing: %+v", a)
	}
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	get := func(host, path string) *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := get("ins-a1b2c3d4-8888.spark.test", "/"); r.StatusCode != 404 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("ingress 404 page = %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := get("spark.test", "/api/v1/products"); r.StatusCode != 200 {
		t.Fatalf("console via apex = %d", r.StatusCode)
	}
	if r := get("127.0.0.1", "/api/v1/products"); r.StatusCode != 200 || r.Header.Get("Strict-Transport-Security") != "" {
		t.Fatalf("console via ip = %d hsts=%q", r.StatusCode, r.Header.Get("Strict-Transport-Security"))
	}
	if pub, _ := a.Servers(cfg.FillAccessDefaults()); pub.TLSConfig != nil || a.RedirectServer(cfg) != nil {
		t.Fatal("tls configured without cert files")
	}
}

func TestBuildWithTLSFilesAndRedirect(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeSelfSigned(t, dir)
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "staging", Domain: "spark.test", PublicBaseURL: "https://spark.test", SecretKey: dbtest.Key,
		SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir(), TLSCertFile: certFile, TLSKeyFile: keyFile, HTTPRedirectAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"}
	a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)})
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := a.Servers(cfg)
	if a.TLS == nil || pub.TLSConfig == nil || pub.TLSConfig.GetCertificate == nil {
		t.Fatal("tls not wired")
	}
	ts := httptest.NewUnstartedServer(a.Handler)
	ts.TLS = pub.TLSConfig
	ts.StartTLS()
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/products", nil)
	req.Host = "spark.test"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatalf("https = %d hsts=%q", resp.StatusCode, resp.Header.Get("Strict-Transport-Security"))
	}
	rs := a.RedirectServer(cfg)
	if rs == nil {
		t.Fatal("redirect server nil")
	}
	rt := httptest.NewServer(rs.Handler)
	defer rt.Close()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req2, _ := http.NewRequest("GET", rt.URL+"/lab?x=1", nil)
	req2.Host = "ins-a1b2c3d4-8888.spark.test:80"
	r2, _ := noFollow.Do(req2)
	r2.Body.Close()
	if r2.StatusCode != 301 || r2.Header.Get("Location") != "https://ins-a1b2c3d4-8888.spark.test/lab?x=1" {
		t.Fatalf("redirect = %d %s", r2.StatusCode, r2.Header.Get("Location"))
	}
	// 证书文件坏了：Build 失败而不是悄悄起一个没证书的服务
	os.WriteFile(certFile, []byte("x"), 0o644)
	if _, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)}); err == nil {
		t.Fatal("bad cert accepted")
	}
}
```

`writeSelfSigned(t, dir) (cert, key string)` 与 `tlsfile` 测试里的 `writeCert` 同实现（复制到 `access_test.go`：SAN `spark.test`、`*.spark.test`，`NotAfter = time.Now().Add(90*24*time.Hour)`，序列号 1；import `crypto/ecdsa`、`crypto/elliptic`、`crypto/rand`、`crypto/x509`、`crypto/x509/pkix`、`encoding/pem`、`math/big`、`path/filepath`、`time`）。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/ingress/ ./internal/platform/tlsfile/ ./internal/app/ -run 'TestHostMux|TestLoader|TestHandlerSplits|TestBuildWithTLS' 2>&1 | head`
Expected: 编译错误。

- [ ] **Step 3: `HostMux` 与 `HSTS`**

`internal/ingress/hostmux.go`：

```go
package ingress

import "net/http"

// HostMux 按 Host 分流：实例域名（ins-<id>-<port>.<ingress_domain>）→ ingress，其它（<domain>、IP、ssh.<domain> 等）→ 控制台。
func HostMux(ingressDomain string, ingress, console http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := ParseHost(r.Host, ingressDomain); ok {
			ingress.ServeHTTP(w, r)
			return
		}
		console.ServeHTTP(w, r)
	})
}
```

`internal/platform/httpx/hsts.go`：

```go
package httpx

import "net/http"

// HSTS 在 TLS 连接上加 Strict-Transport-Security（一年）；明文连接（dev、代理后）不加。
func HSTS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 4: `tlsfile.Loader`**

`internal/platform/tlsfile/loader.go`：

```go
// Package tlsfile 从文件加载 TLS 证书并按 mtime 热加载：acme.sh 侧车或人工替换证书文件后，platform 不用重启。
package tlsfile

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"sync"
	"time"
)

type Loader struct {
	CertFile string
	KeyFile  string
	Interval time.Duration     // 检查 mtime 的最小间隔，零值 1 分钟
	Now      func() time.Time  // 零值 time.Now
	// OnReloadError：文件变化但加载失败（保留旧证书）时回调；OnExpiring：CheckExpiry 发现剩余不足阈值时回调。
	OnReloadError func(err error)
	OnExpiring    func(notAfter time.Time)

	mu        sync.Mutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	lastCheck time.Time
}

func (l *Loader) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Loader) interval() time.Duration {
	if l.Interval > 0 {
		return l.Interval
	}
	return time.Minute
}

func mtimes(certFile, keyFile string) (time.Time, time.Time, error) {
	cs, err := os.Stat(certFile)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	ks, err := os.Stat(keyFile)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return cs.ModTime(), ks.ModTime(), nil
}

// Load 首次加载（失败即返回错误，启动时调用）。
func (l *Loader) Load() error {
	cm, km, err := mtimes(l.CertFile, l.KeyFile)
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(l.CertFile, l.KeyFile)
	if err != nil {
		return err
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return err
		}
	}
	l.mu.Lock()
	l.cert, l.certMod, l.keyMod, l.lastCheck = &cert, cm, km, l.now()
	l.mu.Unlock()
	return nil
}

// CheckNow 立即检查文件是否变化并重新加载；失败保留旧证书并回调 OnReloadError。
func (l *Loader) CheckNow() {
	l.mu.Lock()
	oldCM, oldKM := l.certMod, l.keyMod
	l.lastCheck = l.now()
	l.mu.Unlock()
	cm, km, err := mtimes(l.CertFile, l.KeyFile)
	if err != nil {
		l.reloadError(err)
		return
	}
	if cm.Equal(oldCM) && km.Equal(oldKM) {
		return
	}
	if err := l.Load(); err != nil {
		l.mu.Lock()
		l.certMod, l.keyMod = cm, km // 记住这次失败的版本，文件再变才重试
		l.mu.Unlock()
		l.reloadError(err)
	}
}

func (l *Loader) reloadError(err error) {
	if l.OnReloadError != nil {
		l.OnReloadError(err)
	}
}

// GetCertificate 供 tls.Config 使用；每 Interval 最多检查一次文件。
func (l *Loader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	l.mu.Lock()
	cert, last := l.cert, l.lastCheck
	l.mu.Unlock()
	if cert == nil {
		if err := l.Load(); err != nil {
			return nil, err
		}
		l.mu.Lock()
		cert = l.cert
		l.mu.Unlock()
		return cert, nil
	}
	if l.now().Sub(last) >= l.interval() {
		l.CheckNow()
		l.mu.Lock()
		cert = l.cert
		l.mu.Unlock()
	}
	return cert, nil
}

func (l *Loader) NotAfter() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cert == nil || l.cert.Leaf == nil {
		return time.Time{}
	}
	return l.cert.Leaf.NotAfter
}

func (l *Loader) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: l.GetCertificate}
}

// CheckExpiry：剩余有效期不足 warnWithin 时回调 OnExpiring。
func (l *Loader) CheckExpiry(warnWithin time.Duration) {
	na := l.NotAfter()
	if na.IsZero() || l.OnExpiring == nil {
		return
	}
	if na.Sub(l.now()) < warnWithin {
		l.OnExpiring(na)
	}
}

// RunExpiryCheck 每 every 检查一次到期（启动时先查一次），直到 ctx 结束。
func (l *Loader) RunExpiryCheck(ctx context.Context, every, warnWithin time.Duration) {
	l.CheckExpiry(warnWithin)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.CheckExpiry(warnWithin)
		}
	}
}
```

- [ ] **Step 5: 组装进 `app`**

`internal/app/app.go`：
- `App` 追加：

```go
	Ingress     *ingress.Server
	IngressAuth *ingress.Auth
	Routes      *ingress.Table
	Upstream    *upstream.Pool
	TLS         *tlsfile.Loader // 由 platform 终止 TLS 时非 nil
```

- `Build` 里，`srv := &api.Server{...}` 之后、`return` 之前：

```go
	pool := upstream.New(upstream.Config{Signer: upstreamSigner, Port: cfg.UpstreamSSHPort, Log: log}, r)
	routes := ingress.NewTable(r, log)
	ingSrv := ingress.NewServer(ingressAuth, routes, ids, pool, r, clk, log)
	handler := httpx.HSTS(ingress.HostMux(cfg.IngressDomain, ingSrv, srv.Router()))
	var loader *tlsfile.Loader
	if cfg.TLSEnabled() {
		loader = &tlsfile.Loader{CertFile: cfg.TLSCertFile, KeyFile: cfg.TLSKeyFile,
			OnReloadError: func(err error) {
				log.Error("tls certificate reload failed", "err", err)
				alertOnce(f, log, "critical", "tls_cert_reload_failed", "TLS 证书文件重新加载失败，详情见日志")
			},
			OnExpiring: func(na time.Time) {
				log.Warn("tls certificate expiring", "not_after", na)
				alertOnce(f, log, "warning", "tls_cert_expiring", "TLS 证书将在 14 天内到期")
			}}
		if err := loader.Load(); err != nil {
			return nil, fmt.Errorf("加载 TLS 证书: %w", err)
		}
	}
```

`App` 字面量用 `Handler: handler` 并加 `Ingress: ingSrv, IngressAuth: ingressAuth, Routes: routes, Upstream: pool, TLS: loader`。文件末尾追加：

```go
func alertOnce(f *fleet.Service, log *slog.Logger, level, kind, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.RaiseAlertOnce(ctx, f.Pool(), fleet.Alert{Level: level, Kind: kind, Message: message}); err != nil {
		log.Error("raise alert", "kind", kind, "err", err)
	}
}

// RedirectServer 返回把明文 HTTP 301 到 https 的服务器；未配置 SPARK_HTTP_REDIRECT_ADDR 时为 nil。
func (a *App) RedirectServer(cfg config.Config) *http.Server {
	if cfg.HTTPRedirectAddr == "" || a.TLS == nil {
		return nil
	}
	return &http.Server{
		Addr: cfg.HTTPRedirectAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
		}),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second,
	}
}

// RunAccessLoops：路由表每 2 秒刷新、离开 running 的实例从上游连接池丢弃、TLS 到期每日检查；ctx 结束后关闭连接池。
func (a *App) RunAccessLoops(ctx context.Context) {
	defer a.Upstream.Close()
	if a.TLS != nil {
		go a.TLS.RunExpiryCheck(ctx, 24*time.Hour, 14*24*time.Hour)
	}
	go a.Routes.Run(ctx, 2*time.Second)
	running := map[string]bool{}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := map[string]bool{}
			for _, r := range a.Routes.Routes() {
				if r.State == rental.StateRunning && !r.Suspended {
					now[r.InstanceID] = true
				}
			}
			for id := range running {
				if !now[id] {
					a.Upstream.Drop(id)
				}
			}
			running = now
		}
	}
}
```

`Servers(cfg)` 里 `public` 构造后加 `if a.TLS != nil { public.TLSConfig = a.TLS.TLSConfig() }`。import 追加 `"net"`、`"spark-cloud/internal/access/upstream"`、`"spark-cloud/internal/ingress"`、`"spark-cloud/internal/platform/httpx"`、`"spark-cloud/internal/platform/tlsfile"`。

- [ ] **Step 6: `cmd/platform`**

`run()`：
- 公网服务器 goroutine 改为

```go
	go func() {
		log.Info("platform listening", "addr", cfg.HTTPAddr, "env", cfg.Env, "tls", cfg.TLSEnabled(), "ingress_domain", cfg.IngressDomain)
		if cfg.TLSEnabled() {
			serverErr <- srv.ListenAndServeTLS("", "")
			return
		}
		serverErr <- srv.ListenAndServe()
	}()
```

- 之后：

```go
	redirect := a.RedirectServer(cfg)
	if redirect != nil {
		go func() {
			log.Info("http redirect listening", "addr", cfg.HTTPRedirectAddr)
			serverErr <- redirect.ListenAndServe()
		}()
	}
	accessDone := make(chan struct{})
	go func() { defer close(accessDone); a.RunAccessLoops(ctx) }()
```

`serverErr` 的容量改为 3；关闭阶段的 `for _, s := range []*http.Server{srv, adminSrv}` 里把 `redirect` 也加进去（nil 跳过），并在 `<-sweeperDone` 旁加 `<-accessDone`。

- [ ] **Step 7: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/ingress/ ./internal/platform/... ./internal/app/ ./cmd/... -race 2>&1 | tail -6 && go build ./...`
Expected: 全部 `ok`。

- [ ] **Step 8: 提交**

```bash
git add internal/ingress internal/platform/tlsfile internal/platform/httpx internal/app cmd/platform
git commit -m "feat(app): host-based mux for tenant ingress, tls file hot reload with hsts and http redirect, access loops

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 19: 控制台与后台 API：公钥、密码重置 / 开关、端口、SSH 会话、实例视图字段、仪表盘计数

**Files:**
- Create: `internal/api/access.go`、`internal/api/access_test.go`
- Modify: `internal/api/instances.go:19-48`（视图）、`internal/api/server.go`（路由、`SSHHost` / `SSHPort` 字段）
- Create: `internal/adminapi/access.go`、`internal/adminapi/access_test.go`
- Modify: `internal/adminapi/instances.go:14-22,70-95`、`internal/adminapi/users.go:43-80`、`internal/adminapi/views.go:12-30`、`internal/adminapi/dashboard.go`
- Modify: `internal/rental/ports.go`（`PortCounts`）、`internal/app/app.go`

**Interfaces:**
- Produces:
  - `(*rental.Service).PortCounts(ctx, instanceIDs []string) (map[string]int, error)`。
  - `api.Server.SSHHost string`、`SSHPort int`；`instanceView` 新字段 `password_auth bool`、`ssh_host string`、`ssh_port int`、`ports_count int`；`ssh_command` 非 22 端口时 `ssh -p <port> <id>@<host>`。
  - 控制台路由（`requireUser` 组内）：`GET/POST /ssh-keys`、`DELETE /ssh-keys/{id}`、`POST /instances/{id}/password/reset`、`PUT /instances/{id}/access`、`GET/POST /instances/{id}/ports`、`PUT/DELETE /instances/{id}/ports/{port}`、`GET /instances/{id}/ssh-sessions`。端口视图 `portView{port, visibility, forced_private, note, url, created_at}`（`url = Ingress.PortURL`）。
  - 后台：`instanceView.PasswordAuth bool json:"password_auth"`；`GET /instances/{id}` 响应增加 `ports []portView`、`ssh_host_pubkey_fingerprint string`；`GET /instances/{id}/ssh-sessions`（allRoles，分页）；`POST /instances/{id}/ports/{port}/force-private`（ops，`{reason}`）；`DELETE /instances/{id}/ports/{port}`（ops，`{reason}`）；`GET /users/{id}` 增加 `ssh_keys`；`GET /dashboard` 增加 `ssh_sessions_open`。
  - `adminapi.Server.PortURL func(instanceID string, port int) string`（app 传 `ingressAuth.PortURL`）。

- [ ] **Step 1: 写失败的测试**

`internal/api/access_test.go`：

```go
package api_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/platform/dbtest"
)

func pubLine(t *testing.T) string {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	p, _ := ssh.NewPublicKey(pub)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p))) + " test@mac"
}

func (c *client) runningInstance(t *testing.T, e *testEnv, phone string) string {
	t.Helper()
	dbtest.CreateMachine(t, e.pool, "SN-"+phone)
	user := c.login(phone)
	c.do("POST", "/kyc/personal", map[string]string{"name": "张三", "id_number": "11010119900101123X"})
	e.wallet.AdminAdjust(t.Context(), 1, int64(user["id"].(float64)), 100000, "fund", "")
	_, out := c.do("POST", "/orders", map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": dbtest.ImageID(t, e.pool)})
	return out["instance"].(map[string]any)["id"].(string)
}

func TestSSHKeysEndpoints(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	c.login("13800000041")
	code, out := c.do("POST", "/ssh-keys", map[string]string{"public_key": pubLine(t)})
	if code != 201 || out["name"] != "test@mac" || !strings.HasPrefix(out["fingerprint"].(string), "SHA256:") || out["public_key"] != nil {
		t.Fatalf("add = %d %v", code, out)
	}
	if code, out := c.do("POST", "/ssh-keys", map[string]string{"public_key": "nope"}); code != 400 || out["code"] != "ssh_key_invalid" {
		t.Fatalf("invalid = %d %v", code, out)
	}
	code, out = c.do("GET", "/ssh-keys", nil)
	items := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	id := int64(items[0].(map[string]any)["id"].(float64))
	other := e.client()
	other.login("13800000042")
	if code, _ := other.do("DELETE", "/ssh-keys/"+itoa(id), nil); code != 404 {
		t.Fatalf("delete other's = %d", code)
	}
	if code, _ := c.do("DELETE", "/ssh-keys/"+itoa(id), nil); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if _, out := c.do("GET", "/ssh-keys", nil); len(out["items"].([]any)) != 0 {
		t.Fatal("not deleted")
	}
}

func TestInstanceAccessEndpoints(t *testing.T) {
	e := newTestEnvWith(t, func(s *api.Server) { s.SSHPort = 2200 })
	c := e.client()
	id := c.runningInstance(t, e, "13800000043")
	_, inst := c.do("GET", "/instances/"+id, nil)
	if inst["ssh_command"] != "ssh -p 2200 "+id+"@ssh.spark.test" || inst["password_auth"] != true || inst["ssh_host"] != "ssh.spark.test" || inst["ssh_port"].(float64) != 2200 || inst["ports_count"].(float64) != 0 {
		t.Fatalf("view = %v", inst)
	}
	_, before := c.do("GET", "/instances/"+id+"/password", nil)
	code, out := c.do("POST", "/instances/"+id+"/password/reset", nil)
	if code != 200 || len(out["password"].(string)) != 16 || out["password"] == before["password"] {
		t.Fatalf("reset = %d %v", code, out)
	}
	if code, out := c.do("PUT", "/instances/"+id+"/access", map[string]bool{"password_auth": false}); code != 409 || out["code"] != "password_auth_requires_key" {
		t.Fatalf("disable without key = %d %v", code, out)
	}
	c.do("POST", "/ssh-keys", map[string]string{"public_key": pubLine(t)})
	if code, out := c.do("PUT", "/instances/"+id+"/access", map[string]bool{"password_auth": false}); code != 200 || out["password_auth"] != false {
		t.Fatalf("disable = %d %v", code, out)
	}
	// 端口
	code, out = c.do("POST", "/instances/"+id+"/ports", map[string]any{"port": 8888, "note": "lab"})
	if code != 201 || out["visibility"] != "private" || out["url"] != "http://"+id+"-8888.spark.test/" || out["note"] != "lab" {
		t.Fatalf("add port = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/ports", map[string]any{"port": 22}); code != 400 || out["code"] != "port_invalid" {
		t.Fatalf("port 22 = %d %v", code, out)
	}
	if code, out := c.do("PUT", "/instances/"+id+"/ports/8888", map[string]any{"visibility": "public"}); code != 200 || out["visibility"] != "public" {
		t.Fatalf("update = %d %v", code, out)
	}
	for p := 8001; p <= 8004; p++ {
		c.do("POST", "/instances/"+id+"/ports", map[string]any{"port": p})
	}
	if code, out := c.do("POST", "/instances/"+id+"/ports", map[string]any{"port": 8005}); code != 409 || out["code"] != "port_limit" {
		t.Fatalf("limit = %d %v", code, out)
	}
	if _, inst := c.do("GET", "/instances/"+id, nil); inst["ports_count"].(float64) != 5 {
		t.Fatalf("ports_count = %v", inst["ports_count"])
	}
	if code, _ := c.do("DELETE", "/instances/"+id+"/ports/8001", nil); code != 200 {
		t.Fatalf("delete port = %d", code)
	}
	code, out = c.do("GET", "/instances/"+id+"/ports", nil)
	if code != 200 || len(out["items"].([]any)) != 4 {
		t.Fatalf("list ports = %d %v", code, out)
	}
	if code, out := c.do("GET", "/instances/"+id+"/ssh-sessions", nil); code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("sessions = %d %v", code, out)
	}
	// 别人的实例一律 404
	other := e.client()
	other.login("13800000044")
	for _, req := range []struct{ m, p string }{{"POST", "/instances/" + id + "/password/reset"}, {"GET", "/instances/" + id + "/ports"}, {"PUT", "/instances/" + id + "/access"}} {
		var body any
		if req.m != "GET" {
			body = map[string]any{"password_auth": true}
		}
		if code, _ := other.do(req.m, req.p, body); code != 404 {
			t.Fatalf("%s %s by other = %d", req.m, req.p, code)
		}
	}
}
```

（`api_test.go` 若没有 `itoa`，追加 `func itoa(n int64) string { return strconv.FormatInt(n, 10) }` 与 import `strconv`；import `"spark-cloud/internal/api"` 已有。）

`internal/adminapi/access_test.go`：

```go
package adminapi_test

import (
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/rental"
)

func TestAdminAccessViewsAndPortActions(t *testing.T) {
	e := newEnv(t)
	e.machine("SN-ACC")
	uid := e.renter("13800000051")
	inst := e.place(uid)
	e.ack()
	if _, err := e.rental.AddPort(e.ctx, uid, inst.ID, 8888, "public", "lab", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.AddSSHKey(e.ctx, uid, "mac", pubLineAdmin(t), ""); err != nil {
		t.Fatal(err)
	}
	sid, _ := e.rental.OpenSSHSession(e.ctx, rental.SSHSessionStart{InstanceID: inst.ID, UserID: uid, MachineID: inst.MachineID, AuthMethod: "password", ClientIP: "203.0.113.1"})
	fin := e.loginAs("fin", admin.RoleFinance)
	code, out := fin.do("GET", "/instances/"+inst.ID, nil)
	ports := out["ports"].([]any)
	if code != 200 || out["instance"].(map[string]any)["password_auth"] != true || len(ports) != 1 || ports[0].(map[string]any)["url"] != "http://"+inst.ID+"-8888.spark.test/" ||
		out["ssh_host_pubkey_fingerprint"] == "" {
		t.Fatalf("detail = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/instances/"+inst.ID+"/ssh-sessions?page=1&page_size=10", nil); code != 200 || out["total"].(float64) != 1 || out["items"].([]any)[0].(map[string]any)["id"].(float64) != float64(sid) {
		t.Fatalf("sessions = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/users/"+itoa(uid), nil); code != 200 || len(out["ssh_keys"].([]any)) != 1 || out["ssh_keys"].([]any)[0].(map[string]any)["name"] != "mac" {
		t.Fatalf("user detail = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/dashboard", nil); code != 200 || out["ssh_sessions_open"].(float64) != 1 {
		t.Fatalf("dashboard = %d %v", code, out)
	}
	// 财务不能处置端口
	if code, _ := fin.do("POST", "/instances/"+inst.ID+"/ports/8888/force-private", map[string]string{"reason": "x"}); code != 403 {
		t.Fatalf("finance force-private = %d", code)
	}
	ops := e.loginAs("ops", admin.RoleOps)
	if code, _ := ops.do("POST", "/instances/"+inst.ID+"/ports/8888/force-private", map[string]string{"reason": ""}); code != 400 {
		t.Fatalf("empty reason = %d", code)
	}
	code, out = ops.do("POST", "/instances/"+inst.ID+"/ports/8888/force-private", map[string]string{"reason": "钓鱼"})
	if code != 200 || out["forced_private"] != true || out["visibility"] != "private" {
		t.Fatalf("force = %d %v", code, out)
	}
	if code, _ := ops.do("DELETE", "/instances/"+inst.ID+"/ports/8888", map[string]string{"reason": "abuse"}); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := ops.do("DELETE", "/instances/"+inst.ID+"/ports/8888", map[string]string{"reason": "abuse"}); code != 404 {
		t.Fatalf("delete twice = %d", code)
	}
	var n int
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action IN ('instance.port_force_private', 'instance.port_delete') AND actor_type = 'admin'`).Scan(&n)
	if n != 2 {
		t.Fatalf("audit = %d", n)
	}
}
```

`pubLineAdmin(t)` 与 `api` 测试的 `pubLine` 同实现（生成 ed25519 authorized_keys 行，放在本文件；import `crypto/ed25519`、`crypto/rand`、`strings`、`golang.org/x/crypto/ssh`）；文件 import 只需 `testing`、`admin`、`rental` 与上述四个。`helpers_test.go` 的 `adminapi.Server{...}` 增加 `PortURL: func(id string, port int) string { return fmt.Sprintf("http://%s-%d.spark.test/", id, port) }`。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/api/ ./internal/adminapi/ -run 'TestSSHKeys|TestInstanceAccess|TestAdminAccess' 2>&1 | head`
Expected: 编译错误 / 404。

- [ ] **Step 3: `rental.PortCounts`**

`internal/rental/ports.go` 追加：

```go
// PortCounts 批量返回实例的端口数（列表视图用，避免 N+1）。
func (s *Service) PortCounts(ctx context.Context, instanceIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(instanceIDs) == 0 {
		return out, nil
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT instance_id, count(*) FROM instance_ports WHERE instance_id = ANY($1) GROUP BY instance_id`, instanceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: 控制台视图与路由**

`internal/api/instances.go`：`instanceView` 追加

```go
	PasswordAuth bool   `json:"password_auth"`
	SSHHost      string `json:"ssh_host"`
	SSHPort      int    `json:"ssh_port"`
	PortsCount   int    `json:"ports_count"`
```

`view()` 里 `SSHCommand` 的分支改为：

```go
	v.PasswordAuth, v.SSHHost, v.SSHPort = i.PasswordAuth, s.SSHHost, s.sshPort()
	if i.State == rental.StateRunning {
		v.SSHCommand = sshCommand(i.ID, s.SSHHost, s.sshPort())
	}
```

并新增：

```go
func (s *Server) sshPort() int {
	if s.SSHPort <= 0 {
		return 22
	}
	return s.SSHPort
}

// sshCommand：公网端口 22 时省略 -p。
func sshCommand(id, host string, port int) string {
	if port == 22 {
		return fmt.Sprintf("ssh %s@%s", id, host)
	}
	return fmt.Sprintf("ssh -p %d %s@%s", port, id, host)
}
```

`viewNamed` 之后、`views()` 里批量填 `PortsCount`：`views()` 先收集 id 列表 → `s.Rental.PortCounts` → 赋值；`viewOne()` 用 `PortCounts(ctx, []string{i.ID})`。`Server` 追加字段 `SSHHost string // ssh.<domain>` 与 `SSHPort int // SPARK_SSH_PUBLIC_PORT`；`app.Build` 里传 `SSHHost: cfg.SSHHost(), SSHPort: cfg.SSHPublicPort`；`api_test.go` 的 `newTestEnvWith` 传 `SSHHost: "ssh.spark.test"`。

`internal/api/server.go` 的 `requireUser` 组内追加：

```go
			r.Get("/ssh-keys", s.handleListSSHKeys)
			r.Post("/ssh-keys", s.handleAddSSHKey)
			r.Delete("/ssh-keys/{id}", s.handleDeleteSSHKey)
			r.Post("/instances/{id}/password/reset", s.handleResetPassword)
			r.Put("/instances/{id}/access", s.handleSetAccess)
			r.Get("/instances/{id}/ports", s.handleListPorts)
			r.Post("/instances/{id}/ports", s.handleAddPort)
			r.Put("/instances/{id}/ports/{port}", s.handleUpdatePort)
			r.Delete("/instances/{id}/ports/{port}", s.handleDeletePort)
			r.Get("/instances/{id}/ssh-sessions", s.handleSSHSessions)
```

`internal/api/access.go`：

```go
package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

type portView struct {
	Port          int       `json:"port"`
	Visibility    string    `json:"visibility"`
	ForcedPrivate bool      `json:"forced_private"`
	Note          string    `json:"note"`
	URL           string    `json:"url"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Server) portView(instanceID string, p rental.Port) portView {
	return portView{Port: p.Port, Visibility: p.Visibility, ForcedPrivate: p.ForcedPrivate, Note: p.Note, URL: s.Ingress.PortURL(instanceID, p.Port), CreatedAt: p.CreatedAt}
}

func (s *Server) handleListSSHKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Identity.ListSSHKeys(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(keys))
}

func (s *Server) handleAddSSHKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	k, err := s.Identity.AddSSHKey(r.Context(), currentUser(r).ID, req.Name, req.PublicKey, clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, k)
}

func (s *Server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, r, apperr.BadRequest("ID 不正确"))
		return
	}
	if err := s.Identity.DeleteSSHKey(r.Context(), currentUser(r).ID, id, clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	pw, err := s.Rental.ResetPassword(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"password": pw})
}

func (s *Server) handleSetAccess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PasswordAuth bool `json:"password_auth"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	inst, err := s.Rental.SetPasswordAuth(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), req.PasswordAuth, clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.viewOne(r.Context(), inst)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ownedInstance 校验实例属于当前用户（只读接口用；写接口由 rental 在事务内校验）。
func (s *Server) ownedInstance(r *http.Request) (rental.Instance, error) {
	return s.Rental.GetForUser(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
}

func (s *Server) handleListPorts(w http.ResponseWriter, r *http.Request) {
	inst, err := s.ownedInstance(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ports, err := s.Rental.ListPorts(r.Context(), inst.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]portView, 0, len(ports))
	for _, p := range ports {
		out = append(out, s.portView(inst.ID, p))
	}
	writeJSON(w, http.StatusOK, list(out))
}

func pathPort(r *http.Request) (int, error) {
	n, err := strconv.Atoi(chi.URLParam(r, "port"))
	if err != nil {
		return 0, apperr.ErrPortInvalid
	}
	return n, nil
}

func (s *Server) handleAddPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port       int    `json:"port"`
		Visibility string `json:"visibility"`
		Note       string `json:"note"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	p, err := s.Rental.AddPort(r.Context(), currentUser(r).ID, id, req.Port, req.Visibility, req.Note, clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.portView(id, p))
}

func (s *Server) handleUpdatePort(w http.ResponseWriter, r *http.Request) {
	port, err := pathPort(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Visibility *string `json:"visibility"`
		Note       *string `json:"note"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	p, err := s.Rental.UpdatePort(r.Context(), currentUser(r).ID, id, port, req.Visibility, req.Note, clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.portView(id, p))
}

func (s *Server) handleDeletePort(w http.ResponseWriter, r *http.Request) {
	port, err := pathPort(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Rental.DeletePort(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), port, clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}

func (s *Server) handleSSHSessions(w http.ResponseWriter, r *http.Request) {
	inst, err := s.ownedInstance(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list_, err := s.Rental.ListSSHSessions(r.Context(), inst.ID, 20)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	type view struct {
		AuthMethod string `json:"auth_method"`
		ClientIP   string `json:"client_ip"`
		StartedAt  any    `json:"started_at"`
		EndedAt    any    `json:"ended_at"`
	}
	out := make([]view, 0, len(list_))
	for _, x := range list_ {
		out = append(out, view{AuthMethod: x.AuthMethod, ClientIP: x.ClientIP, StartedAt: x.StartedAt, EndedAt: x.EndedAt})
	}
	writeJSON(w, http.StatusOK, list(out))
}
```

`requireJSONForWrites` 对 `DELETE` 要求 JSON Content-Type（现有行为）；测试客户端 `do()` 对非 GET 都设置了该头，前端 `http.ts` 需要补 `delete`（Task 20）。

- [ ] **Step 5: 后台**

`internal/adminapi/views.go`：`instanceView` 追加 `PasswordAuth bool json:"password_auth"`（`instanceView()` 里 `PasswordAuth: i.PasswordAuth`）；新增

```go
type portView struct {
	Port          int       `json:"port"`
	Visibility    string    `json:"visibility"`
	ForcedPrivate bool      `json:"forced_private"`
	Note          string    `json:"note"`
	URL           string    `json:"url"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Server) portViews(instanceID string, list []rental.Port) []portView {
	out := make([]portView, 0, len(list))
	for _, p := range list {
		url := ""
		if s.PortURL != nil {
			url = s.PortURL(instanceID, p.Port)
		}
		out = append(out, portView{Port: p.Port, Visibility: p.Visibility, ForcedPrivate: p.ForcedPrivate, Note: p.Note, URL: url, CreatedAt: p.CreatedAt})
	}
	return out
}

// hostKeyFingerprint 返回容器主机公钥的 SHA256 指纹；为空（存量实例未首连）返回空串。
func hostKeyFingerprint(pubLine string) string {
	if pubLine == "" {
		return ""
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubLine))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(pub)
}
```

（import `"golang.org/x/crypto/ssh"`。）`Server` 追加 `PortURL func(instanceID string, port int) string`。

`internal/adminapi/instances.go`：`instanceRoutes` 追加

```go
	s.handle(g, http.MethodGet, "/instances/{id}/ssh-sessions", allRoles, s.handleInstanceSSHSessions)
	s.handle(g, http.MethodPost, "/instances/{id}/ports/{port}/force-private", opsRoles, s.handleForcePortPrivate)
	s.handle(g, http.MethodDelete, "/instances/{id}/ports/{port}", opsRoles, s.handleAdminDeletePort)
```

`handleInstance` 在 `cmds` 之后加 `ports, err := s.Rental.ListPorts(r.Context(), id)`（错误照常返回），响应 map 增加 `"ports": s.portViews(id, ports), "ssh_host_pubkey_fingerprint": hostKeyFingerprint(inst.SSHHostPubkey)`。

`internal/adminapi/access.go`：

```go
package adminapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
)

func (s *Server) handleInstanceSSHSessions(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Rental.AdminListSSHSessions(r.Context(), chi.URLParam(r, "id"), p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func adminPathPort(r *http.Request) (int, error) {
	n, err := strconv.Atoi(chi.URLParam(r, "port"))
	if err != nil {
		return 0, apperr.ErrPortInvalid
	}
	return n, nil
}

func (s *Server) handleForcePortPrivate(w http.ResponseWriter, r *http.Request) {
	port, err := adminPathPort(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	p, err := s.Rental.AdminForcePortPrivate(r.Context(), s.actor(r), id, port, req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.portViews(id, []rental.Port{p})[0])
}

func (s *Server) handleAdminDeletePort(w http.ResponseWriter, r *http.Request) {
	port, err := adminPathPort(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Rental.AdminDeletePort(r.Context(), s.actor(r), chi.URLParam(r, "id"), port, req.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}
```

（import 追加 `"spark-cloud/internal/rental"`。）

`internal/adminapi/users.go` 的 `handleUser`：加 `keys, err := s.Identity.ListSSHKeys(ctx, id)`，响应 map 增加 `"ssh_keys": httpx.List(keys)["items"]`。

`internal/adminapi/dashboard.go`：加 `sshOpen, err := s.Rental.OpenSSHSessionCount(ctx)`（错误照常返回），响应增加 `"ssh_sessions_open": sshOpen`。

`internal/app/app.go`：`adminapi.Server{...}` 加 `PortURL: ingressAuth.PortURL`。

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/api/ ./internal/adminapi/ ./internal/rental/ ./internal/app/ 2>&1 | tail -5`
Expected: 全部 `ok`（`adminapi` 的 `permissions_test.go` 按 `Routes()` 自动核对新接口的角色矩阵）。

- [ ] **Step 7: 提交**

```bash
git add internal/api internal/adminapi internal/rental/ports.go internal/app
git commit -m "feat(api): console ssh keys / password reset / access toggle / ports / sessions; admin access views, port actions, dashboard count

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 20: 控制台页面：SSH 公钥页、实例详情（密码重置 / 密码登录开关 / Web 访问卡片）、登录跳转、帮助页、共享类型

**Files:**
- Modify: `web/shared/src/http.ts`（`del`）、`web/shared/src/types/instance.ts`、`web/shared/src/types/admin.ts`、`web/shared/src/labels.ts`
- Create: `web/shared/src/types/access.ts`（并在 `types/index.ts` 导出）
- Create: `web/console/src/logic/access.ts`、`web/console/src/logic/__tests__/access.spec.ts`
- Create: `web/console/src/views/SshKeysView.vue`
- Modify: `web/console/src/router.ts`、`web/console/src/views/LoginView.vue:52`、`web/console/src/views/AccountView.vue`、`web/console/src/views/InstanceDetailView.vue`、`web/console/src/views/HelpView.vue`、`web/console/src/__tests__/router.spec.ts`

**Interfaces:**
- Consumes: Task 19 的接口与 JSON 字段。
- Produces:
  - `Http.del<T>(path, body?, opt?)`（`DELETE`，JSON 体，与 `post` 同样的错误处理）。
  - 类型：`Instance` 增加 `password_auth: boolean; ssh_host: string; ssh_port: number; ports_count: number`；`SshKey`、`InstancePort`、`PortVisibility`、`SshSessionBrief`、`AdminSshSession`；`AdminInstance.password_auth`；`AdminInstanceDetail.ports: InstancePort[]`、`.ssh_host_pubkey_fingerprint: string`；`UserDetail.ssh_keys: SshKey[]`；`Dashboard.ssh_sessions_open: number`。
  - `labels.ts`：`alertKindLabel` 增加 `ssh_bruteforce: 'SSH 暴力破解'`、`upstream_unreachable: '实例 SSH 上游不可达'`、`tls_cert_reload_failed: 'TLS 证书加载失败'`、`tls_cert_expiring: 'TLS 证书即将到期'`；`portVisibilityLabel = { private: '私有', public: '公开' }`；`auditActions` 增加 `'instance.password_reset', 'instance.password_auth', 'instance.port_add', 'instance.port_update', 'instance.port_delete', 'instance.port_force_private', 'ssh_key.add', 'ssh_key.delete'`。
  - `logic/access.ts`：`validatePort(input: string): { port: number } | { error: string }`、`looksLikePublicKey(s: string): boolean`、`isAuthorizeRedirect(v: string | null): boolean`、`MAX_PORTS = 5`、`MAX_KEYS = 10`。
  - 路由 `/account/ssh-keys`（`auth: true`，标题「SSH 公钥」）；已登录访问 `/login` 且 `redirect` 是 authorize 路径时 `location.assign`。
  - `data-testid`（供 Task 22 的用例使用）：公钥页 `keys-add-btn`、`keys-input`、`keys-name-input`、`keys-submit`、`keys-error`、`keys-row`、`keys-name`、`keys-fp`、`keys-delete`、`keys-delete-ok`、`keys-empty`、`keys-count`；实例详情 `detail-password-reset`、`detail-reset-ok`、`detail-password-auth`、`detail-ssh-host`、`detail-ports-card`、`detail-port-input`、`detail-port-note`、`detail-port-add`、`detail-port-row`、`detail-port-url`、`detail-port-open`、`detail-port-copy`、`detail-port-visibility`、`detail-port-forced`、`detail-port-toggle`、`detail-port-public-ok`、`detail-port-delete`、`detail-port-limit`、`detail-port-hint`、`detail-port-error`；账户页 `account-ssh-keys`；帮助页 `help-ssh-keys`、`help-forward`、`help-web`。

- [ ] **Step 1: 写失败的单元测试**

`web/console/src/logic/__tests__/access.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { isAuthorizeRedirect, looksLikePublicKey, validatePort } from '../access'

describe('validatePort', () => {
  it('1～65535 且不为 22 的整数', () => {
    expect(validatePort('8888')).toEqual({ port: 8888 })
    expect(validatePort(' 80 ')).toEqual({ port: 80 })
    for (const bad of ['', '0', '22', '65536', '-1', '8.8', 'abc', '８８８８']) expect(validatePort(bad)).toHaveProperty('error')
  })
})

describe('looksLikePublicKey', () => {
  it('前端粗校验：类型 + base64 两段，允许 CRLF 与注释', () => {
    expect(looksLikePublicKey('ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAbc me@mac\r\n')).toBe(true)
    expect(looksLikePublicKey('ssh-rsa AAAAB3NzaC1yc2E')).toBe(true)
    expect(looksLikePublicKey('ecdsa-sha2-nistp256 AAAAE2Vj')).toBe(true)
    expect(looksLikePublicKey('')).toBe(false)
    expect(looksLikePublicKey('ssh-ed25519')).toBe(false)
    expect(looksLikePublicKey('-----BEGIN OPENSSH PRIVATE KEY-----')).toBe(false)
    expect(looksLikePublicKey('ssh-dss AAAAB3')).toBe(false)
    expect(looksLikePublicKey('ssh-ed25519 AAAA\nssh-ed25519 BBBB')).toBe(false)
  })
})

describe('isAuthorizeRedirect', () => {
  it('只认 authorize 端点', () => {
    expect(isAuthorizeRedirect('/api/v1/ingress/authorize?target=x')).toBe(true)
    expect(isAuthorizeRedirect('/instances')).toBe(false)
    expect(isAuthorizeRedirect(null)).toBe(false)
    expect(isAuthorizeRedirect('/api/v1/ingress/authorizeX')).toBe(false)
  })
})
```

`web/shared/src/__tests__/http.spec.ts` 追加一个用例（文件已有 `createHttp` 的 fetch 注入写法，照其现有 `fetch` mock 方式）：

```ts
  it('del 发送 DELETE 与 JSON 体', async () => {
    const calls: { url: string; init: RequestInit }[] = []
    const http = createHttp({
      base: '/api/v1',
      onUnauthorized: () => {},
      fetch: async (input, init) => {
        calls.push({ url: String(input), init: init! })
        return new Response(JSON.stringify({ ok: true }), { status: 200, headers: { 'Content-Type': 'application/json' } })
      },
    })
    await http.del('/instances/ins-a/ports/8888')
    expect(calls[0].url).toBe('/api/v1/instances/ins-a/ports/8888')
    expect(calls[0].init.method).toBe('DELETE')
    expect((calls[0].init.headers as Record<string, string>)['Content-Type']).toBe('application/json')
    expect(calls[0].init.body).toBe('{}')
  })
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd web && npm test 2>&1 | tail -15`
Expected: `access.spec.ts` 找不到模块；`http.spec.ts` 的 `del` 不存在。

- [ ] **Step 3: 共享层**

`web/shared/src/http.ts`：`Http` 接口加 `del<T>(path: string, body?: unknown, opt?: HttpOptions): Promise<T>`，`createHttp` 返回对象加 `del: (path, body, opt) => request('DELETE', path, body ?? {}, opt),`。

`web/shared/src/types/access.ts`：

```ts
export interface SshKey {
  id: number
  name: string
  fingerprint: string
  last_used_at: string | null
  created_at: string
}

export type PortVisibility = 'private' | 'public'

export interface InstancePort {
  port: number
  visibility: PortVisibility
  forced_private: boolean
  note: string
  url: string
  created_at: string
}

export interface SshSessionBrief {
  auth_method: 'password' | 'publickey'
  client_ip: string
  started_at: string
  ended_at: string | null
}

export interface AdminSshSession {
  id: number
  instance_id: string
  user_id: number
  machine_id: number
  auth_method: 'password' | 'publickey'
  key_id: number | null
  client_ip: string
  bytes_up: number
  bytes_down: number
  started_at: string
  last_seen_at: string
  ended_at: string | null
}
```

`types/index.ts` 加 `export * from './access'`。`types/instance.ts` 的 `Instance` 追加 `password_auth: boolean`、`ssh_host: string`、`ssh_port: number`、`ports_count: number`。`types/admin.ts`：`AdminInstance` 追加 `password_auth: boolean`；`AdminInstanceDetail` 追加 `ports: InstancePort[]`、`ssh_host_pubkey_fingerprint: string`（import `InstancePort`、`SshKey` from `./access`）；`UserDetail` 追加 `ssh_keys: SshKey[]`；`Dashboard` 追加 `ssh_sessions_open: number`。`labels.ts` 按 Interfaces 追加。现有单测里构造 `Instance` 字面量的地方（`logic/__tests__/instance.spec.ts` 的 `base` 等）补上新字段：`password_auth: true, ssh_host: 'ssh.spark.test', ssh_port: 22, ports_count: 0`（`npm run build` 的 `vue-tsc` 会把缺字段的地方全部指出来，逐个补）。

- [ ] **Step 4: `logic/access.ts`**

```ts
export const MAX_PORTS = 5
export const MAX_KEYS = 10

/** 端口：半角整数 1～65535，且不能是 22（容器 sshd）。 */
export function validatePort(input: string): { port: number } | { error: string } {
  const s = input.trim()
  if (!/^\d{1,5}$/.test(s)) return { error: '端口必须是 1～65535 之间的整数' }
  const port = Number(s)
  if (port < 1 || port > 65535) return { error: '端口必须是 1～65535 之间的整数' }
  if (port === 22) return { error: '22 端口由 SSH 使用，不能作为 Web 端口' }
  return { port }
}

const KEY_TYPES = ['ssh-ed25519', 'ecdsa-sha2-nistp256', 'ecdsa-sha2-nistp384', 'ecdsa-sha2-nistp521', 'ssh-rsa']

/** 前端粗校验（后端为准）：单行、类型受支持、至少有 base64 段。 */
export function looksLikePublicKey(s: string): boolean {
  const line = s.trim()
  if (!line || /[\r\n]/.test(line)) return false
  const parts = line.split(/\s+/)
  return parts.length >= 2 && KEY_TYPES.includes(parts[0]) && /^[A-Za-z0-9+/=]+$/.test(parts[1])
}

/** 登录后的 redirect 若是实例 Web 访问的授权端点，必须整页跳转（它不是前端路由）。 */
export function isAuthorizeRedirect(v: string | null): boolean {
  return v !== null && (v === '/api/v1/ingress/authorize' || v.startsWith('/api/v1/ingress/authorize?'))
}
```

`web/console/src/router.ts`：routes 加 `{ path: '/account/ssh-keys', component: () => import('./views/SshKeysView.vue'), meta: { title: 'SSH 公钥', auth: true } }`（放在 `/account` 之后）；守卫里 `if (to.path === '/login' && session.status === 'authed')` 改为

```ts
  if (to.path === '/login' && session.status === 'authed') {
    const target = safeRedirect(to.query.redirect)
    if (target && isAuthorizeRedirect(target)) {
      window.location.assign(target) // authorize 是后端端点，不是前端路由
      return false
    }
    return target ?? '/market'
  }
```

（import `isAuthorizeRedirect` from `./logic/access`。）`LoginView.vue` 第 52 行改为：

```ts
    const target = safeRedirect(route.query.redirect)
    if (target && isAuthorizeRedirect(target)) {
      window.location.assign(target)
      return
    }
    await router.replace(target ?? '/market')
```

- [ ] **Step 5: `SshKeysView.vue`**

```vue
<script setup lang="ts">
import { fmtTime, isApiError, messageOf, type Listed, type SshKey } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, ref } from 'vue'
import { api } from '../api'
import AppDialog from '../components/AppDialog.vue'
import { looksLikePublicKey, MAX_KEYS } from '../logic/access'
import { notifyError } from '../logic/notify'

const keys = ref<SshKey[]>([])
const loadError = ref('')
const adding = ref(false)
const input = ref('')
const name = ref('')
const error = ref('')
const submitting = ref(false)
const deleting = ref<SshKey | null>(null)
const busyDelete = ref(false)

async function load() {
  try {
    keys.value = (await api.get<Listed<SshKey>>('/ssh-keys')).items
    loadError.value = ''
  } catch (e) {
    loadError.value = messageOf(e)
  }
}
onMounted(load)

function openAdd() {
  input.value = ''
  name.value = ''
  error.value = ''
  adding.value = true
}
async function submit() {
  if (submitting.value) return
  if (!looksLikePublicKey(input.value)) {
    error.value = '请粘贴一行公钥（以 ssh-ed25519 / ecdsa-sha2-… / ssh-rsa 开头）'
    return
  }
  error.value = ''
  submitting.value = true
  try {
    await api.post('/ssh-keys', { name: name.value.trim(), public_key: input.value.trim() })
    adding.value = false
    ElMessage.success('已添加')
    await load()
  } catch (e) {
    if (isApiError(e) && (e.code === 'ssh_key_invalid' || e.code === 'ssh_key_exists' || e.code === 'ssh_key_limit' || e.code === 'bad_request')) error.value = e.message
    else notifyError(e)
  } finally {
    submitting.value = false
  }
}
async function confirmDelete() {
  if (!deleting.value || busyDelete.value) return
  busyDelete.value = true
  try {
    await api.del(`/ssh-keys/${deleting.value.id}`)
    deleting.value = null
    ElMessage.success('已删除')
    await load()
  } catch (e) {
    notifyError(e)
  } finally {
    busyDelete.value = false
  }
}
</script>

<template>
  <div class="keys">
    <el-card>
      <template #header>
        <div class="head">
          <span>SSH 公钥 <span class="muted" data-testid="keys-count">{{ keys.length }} / {{ MAX_KEYS }}</span></span>
          <el-button type="primary" size="small" :disabled="keys.length >= MAX_KEYS" data-testid="keys-add-btn" @click="openAdd">添加公钥</el-button>
        </div>
      </template>
      <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" />
      <p class="muted">添加后可用公钥登录您的所有实例（`ssh -i 私钥 ins-xxxxxxxx@…`）；删除公钥不影响已建立的连接。在容器里 `ssh-copy-id` 不会生效，公钥只在这里管理。</p>
      <p v-if="!loadError && keys.length === 0" class="muted" data-testid="keys-empty">还没有公钥</p>
      <div v-for="k in keys" :key="k.id" class="row" data-testid="keys-row">
        <div class="info">
          <b data-testid="keys-name">{{ k.name }}</b>
          <code class="mono" data-testid="keys-fp">{{ k.fingerprint }}</code>
          <span class="muted">添加于 {{ fmtTime(k.created_at) }}<template v-if="k.last_used_at"> · 最近使用 {{ fmtTime(k.last_used_at) }}</template></span>
        </div>
        <el-button size="small" type="danger" plain data-testid="keys-delete" @click="deleting = k">删除</el-button>
      </div>
    </el-card>

    <AppDialog v-model="adding" kind="form" title="添加公钥" data-testid="keys-dialog">
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="公钥（一行，支持 ed25519 / ecdsa / rsa≥2048）">
          <el-input v-model="input" type="textarea" :rows="4" placeholder="ssh-ed25519 AAAA… you@laptop" data-testid="keys-input" />
        </el-form-item>
        <el-form-item label="名称（可选，缺省取公钥注释）"><el-input v-model="name" maxlength="64" data-testid="keys-name-input" /></el-form-item>
        <p v-if="error" class="danger" data-testid="keys-error">{{ error }}</p>
      </el-form>
      <template #footer>
        <el-button data-testid="keys-cancel" @click="adding = false">取消</el-button>
        <el-button type="primary" :loading="submitting" data-testid="keys-submit" @click="submit">添加</el-button>
      </template>
    </AppDialog>

    <AppDialog :model-value="!!deleting" kind="confirm" title="删除公钥" data-testid="keys-delete-dialog" @update:model-value="(v: boolean) => { if (!v) deleting = null }">
      <p>删除「{{ deleting?.name }}」后将无法再用它登录，确定删除？</p>
      <template #footer>
        <el-button data-testid="keys-delete-cancel" @click="deleting = null">取消</el-button>
        <el-button type="danger" :loading="busyDelete" data-testid="keys-delete-ok" @click="confirmDelete">确定删除</el-button>
      </template>
    </AppDialog>
  </div>
</template>

<style scoped>
.keys { display: grid; gap: 16px; max-width: 760px; }
.head { display: flex; justify-content: space-between; align-items: center; }
.row { display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 10px 0; border-bottom: 1px solid var(--el-border-color-lighter); }
.info { display: grid; gap: 4px; min-width: 0; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; font-size: 12px; }
.muted { color: #909399; font-size: 12px; }
.danger { color: var(--el-color-danger); font-size: 13px; }
</style>
```

`AccountView.vue` 的 `.actions` 里 `使用帮助` 链接之前加 `<router-link to="/account/ssh-keys" data-testid="account-ssh-keys">SSH 公钥</router-link>`。

- [ ] **Step 6: 实例详情**

`InstanceDetailView.vue` `<script setup>` 追加（import 补 `type InstancePort`、`isApiError`；`validatePort`、`MAX_PORTS` from `../logic/access`；`useRouter` 已有）：

```ts
// ---- 密码重置 / 密码登录开关 ----
const resetVisible = ref(false)
const resetting = ref(false)
async function confirmReset() {
  if (resetting.value) return
  resetting.value = true
  try {
    password.value = (await api.post<{ password: string }>(`/instances/${id.value}/password/reset`)).password
    resetVisible.value = false
    ElMessage.success('密码已重置，请复制保存')
  } catch (e) {
    resetVisible.value = false
    notifyError(e)
  } finally {
    resetting.value = false
  }
}
const togglingAuth = ref(false)
async function setPasswordAuth(enabled: boolean) {
  if (togglingAuth.value || !inst.value) return
  togglingAuth.value = true
  try {
    inst.value = { ...inst.value, ...(await api.put<Instance>(`/instances/${id.value}/access`, { password_auth: enabled })) }
    ElMessage.success(enabled ? '已打开密码登录' : '已关闭密码登录，只能用公钥登录')
  } catch (e) {
    if (isApiError(e) && e.code === 'password_auth_requires_key') ElMessage.warning('关闭密码登录前请先在「我的 → SSH 公钥」添加公钥')
    else notifyError(e)
  } finally {
    togglingAuth.value = false
  }
}

// ---- Web 访问（端口）----
const ports = ref<InstancePort[]>([])
const portInput = ref('')
const portNote = ref('')
const portError = ref('')
const addingPort = ref(false)
const publicTarget = ref<InstancePort | null>(null)
const togglingPort = ref(false)
async function loadPorts() {
  try {
    ports.value = (await api.get<Listed<InstancePort>>(`/instances/${id.value}/ports`)).items
  } catch (e) {
    notifyError(e)
  }
}
async function addPort() {
  if (addingPort.value) return
  const v = validatePort(portInput.value)
  if ('error' in v) {
    portError.value = v.error
    return
  }
  portError.value = ''
  addingPort.value = true
  try {
    await api.post(`/instances/${id.value}/ports`, { port: v.port, note: portNote.value.trim() })
    portInput.value = ''
    portNote.value = ''
    await loadPorts()
  } catch (e) {
    if (isApiError(e) && e.code.startsWith('port_')) portError.value = e.message
    else notifyError(e)
  } finally {
    addingPort.value = false
  }
}
async function setVisibility(p: InstancePort, visibility: 'private' | 'public') {
  if (togglingPort.value) return
  togglingPort.value = true
  try {
    await api.put(`/instances/${id.value}/ports/${p.port}`, { visibility })
    publicTarget.value = null
    await loadPorts()
  } catch (e) {
    publicTarget.value = null
    if (isApiError(e) && e.code === 'port_forced_private') ElMessage.warning(e.message)
    else notifyError(e)
  } finally {
    togglingPort.value = false
  }
}
function togglePort(p: InstancePort) {
  if (p.visibility === 'public') void setVisibility(p, 'private')
  else publicTarget.value = p // 设为公开需二次确认
}
async function deletePort(p: InstancePort) {
  try {
    await api.del(`/instances/${id.value}/ports/${p.port}`)
    await loadPorts()
  } catch (e) {
    notifyError(e)
  }
}
const canManagePorts = (s: string) => s === 'creating' || s === 'running' || s === 'stopped'
```

`load()` 成功分支末尾加 `if (canManagePorts(next.state)) void loadPorts()`；`watch(id, …)` 里加 `ports.value = []`。

模板：「SSH 连接」卡片改为

```vue
      <el-card header="SSH 连接">
        <template v-if="inst.state === 'running' && inst.ssh_command">
          <div class="row">
            <code class="mono" data-testid="detail-ssh-command">{{ inst.ssh_command }}</code>
            <el-button size="small" data-testid="detail-ssh-copy" @click="copy(inst.ssh_command)">复制</el-button>
          </div>
          <p class="muted">主机 <span data-testid="detail-ssh-host">{{ inst.ssh_host }}</span>，端口 {{ inst.ssh_port }}；首次连接请在<router-link to="/help">帮助页</router-link>核对主机密钥指纹。</p>
        </template>
        <p v-else class="muted" data-testid="detail-ssh-hint">实例运行中才能连接</p>
        <template v-if="canManagePorts(inst.state)">
          <div class="row">
            <span>密码：</span>
            <code class="mono" data-testid="detail-password">{{ password || '••••••••' }}</code>
            <el-button v-if="!password" size="small" :loading="loadingPassword" data-testid="detail-password-show" @click="showPassword">显示</el-button>
            <el-button v-else size="small" data-testid="detail-password-copy" @click="copy(password)">复制</el-button>
            <el-button size="small" plain data-testid="detail-password-reset" @click="resetVisible = true">重置密码</el-button>
          </div>
          <div class="row">
            <span>密码登录：</span>
            <el-switch :model-value="inst.password_auth" :loading="togglingAuth" data-testid="detail-password-auth" @change="(v: boolean) => setPasswordAuth(v)" />
            <span class="muted">关闭后只能用<router-link to="/account/ssh-keys">SSH 公钥</router-link>登录</span>
          </div>
        </template>
      </el-card>

      <el-card v-if="canManagePorts(inst.state)" header="Web 访问" data-testid="detail-ports-card">
        <p class="muted">把容器里监听的端口（如 JupyterLab 8888）开放为网址；默认只有您登录控制台后能访问，可设为公开。应用需监听 127.0.0.1 或 0.0.0.0。</p>
        <p v-if="inst.state !== 'running'" class="muted" data-testid="detail-port-hint">实例运行中才可访问，现在可以先添加端口</p>
        <div v-for="p in ports" :key="p.port" class="row port" data-testid="detail-port-row">
          <b>{{ p.port }}</b>
          <span v-if="p.note" class="muted">{{ p.note }}</span>
          <a :href="p.url" target="_blank" rel="noopener" class="mono" data-testid="detail-port-url">{{ p.url }}</a>
          <el-button size="small" data-testid="detail-port-copy" @click="copy(p.url)">复制</el-button>
          <el-tag :type="p.visibility === 'public' ? 'warning' : 'info'" data-testid="detail-port-visibility">{{ p.visibility === 'public' ? '公开' : '私有' }}</el-tag>
          <el-tag v-if="p.forced_private" type="danger" data-testid="detail-port-forced">平台限制为私有</el-tag>
          <el-button v-if="!p.forced_private" size="small" :loading="togglingPort" data-testid="detail-port-toggle" @click="togglePort(p)">{{ p.visibility === 'public' ? '设为私有' : '设为公开' }}</el-button>
          <el-button size="small" type="danger" plain data-testid="detail-port-delete" @click="deletePort(p)">删除</el-button>
        </div>
        <div v-if="ports.length < MAX_PORTS" class="row">
          <el-input v-model="portInput" placeholder="端口，如 8888" style="width: 140px" inputmode="numeric" data-testid="detail-port-input" @keyup.enter="addPort" />
          <el-input v-model="portNote" placeholder="备注（可选）" maxlength="32" style="width: 180px" data-testid="detail-port-note" />
          <el-button type="primary" size="small" :loading="addingPort" data-testid="detail-port-add" @click="addPort">添加端口</el-button>
        </div>
        <p v-else class="muted" data-testid="detail-port-limit">每个实例最多开放 {{ MAX_PORTS }} 个端口</p>
        <p v-if="portError" class="danger" data-testid="detail-port-error">{{ portError }}</p>
      </el-card>
```

并在 `GuideDialog` 之前加两个对话框：

```vue
    <AppDialog v-model="resetVisible" kind="confirm" title="重置 SSH 密码" data-testid="detail-reset-dialog">
      <p>将生成新的 16 位密码，旧密码立即失效（已连接的会话不受影响）。</p>
      <template #footer>
        <el-button data-testid="detail-reset-cancel" @click="resetVisible = false">取消</el-button>
        <el-button type="primary" :loading="resetting" data-testid="detail-reset-ok" @click="confirmReset">确定重置</el-button>
      </template>
    </AppDialog>

    <AppDialog :model-value="!!publicTarget" kind="confirm" title="设为公开" data-testid="detail-port-public-dialog" @update:model-value="(v: boolean) => { if (!v) publicTarget = null }">
      <p data-testid="detail-port-public-text">设为公开后，<b>任何人</b>都能通过网址访问端口 {{ publicTarget?.port }}，请确认应用自带鉴权（例如 Jupyter 的 token）。</p>
      <template #footer>
        <el-button data-testid="detail-port-public-cancel" @click="publicTarget = null">取消</el-button>
        <el-button type="warning" :loading="togglingPort" data-testid="detail-port-public-ok" @click="publicTarget && setVisibility(publicTarget, 'public')">确认公开</el-button>
      </template>
    </AppDialog>
```

`.port` 样式：`.port { flex-wrap: wrap; }`。

- [ ] **Step 7: 帮助页**

`HelpView.vue` 在「怎么用 SSH 连接」卡片之后插入三张卡片：

```vue
    <el-card>
      <h2 data-testid="help-ssh-keys">用公钥登录、关闭密码登录</h2>
      <ol>
        <li>在「我的 → SSH 公钥」粘贴您电脑的公钥（`~/.ssh/id_ed25519.pub` 的内容，一行），之后 `ssh -i ~/.ssh/id_ed25519 ins-xxxxxxxx@…` 免密登录。</li>
        <li>添加公钥后可在实例详情关闭密码登录；在容器里执行 `ssh-copy-id` 或修改 `~/.ssh/authorized_keys` 不会生效，公钥只在控制台管理。</li>
        <li>首次连接会提示主机密钥指纹，请与本页公布的网关指纹核对：<code data-testid="help-hostkey">{{ brand.sshHostKeyFingerprint || '（部署后由运维在此填写）' }}</code>。</li>
      </ol>
    </el-card>
    <el-card>
      <h2 data-testid="help-forward">端口转发与文件传输</h2>
      <ul>
        <li>`scp` / `sftp` / `rsync` 都可用（走同一条 SSH 连接）；大文件请放到 `/root/data`。</li>
        <li>本地转发 `ssh -L 8888:127.0.0.1:8888 ins-xxxxxxxx@…` 可在本机浏览器访问容器里的服务；`-R`、`-A`（agent 转发）也支持；不支持 X11。</li>
        <li>VS Code Remote-SSH：Host 填 `ssh.您的域名`，User 填实例 ID。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 data-testid="help-web">Web 访问（端口开放）</h2>
      <ul>
        <li>在实例详情「Web 访问」添加端口后，会得到形如 `https://ins-xxxxxxxx-8888.…/` 的网址；默认<b>私有</b>：只有您在本站登录后才能打开。</li>
        <li>设为<b>公开</b>后任何人都能访问，平台不做鉴权与限流，请确认应用自带密码；平台可在收到滥用举报后将端口限制为私有。</li>
        <li>应用需监听 `127.0.0.1` 或 `0.0.0.0`（只监听容器 eth0 地址不通）。Jupyter 请用 `jupyter lab --ip=127.0.0.1 --port=8888 --ServerApp.allow_remote_access=True`，否则会提示 Host not allowed。</li>
        <li>网址下的 `/.spark/` 路径由平台保留；应用自身的 CSRF 等安全防护由应用负责。</li>
      </ul>
    </el-card>
```

`web/shared/src/brand.ts` 增加字段 `sshHostKeyFingerprint: ''`（部署时由运维填 `platform admin ssh-host-key` 的指纹，与品牌名、客服文案放在一起）。

- [ ] **Step 8: 运行测试确认通过**

Run: `cd web && npm test 2>&1 | tail -8 && npm run build 2>&1 | tail -5`
Expected: Vitest 全绿（含 `access.spec.ts`、`http.spec.ts` 的 `del`、`router.spec.ts`）；`vue-tsc` 与 Vite 构建通过。再在 Go 侧 `go vet ./... && go test ./internal/web/` 确认 embed 正常。

- [ ] **Step 9: 提交**

```bash
git add web/shared web/console
git commit -m "feat(console): ssh keys page, password reset/toggle, web ports card, authorize redirect, help sections

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 21: 后台页面：实例详情「访问」卡片（端口处置、SSH 会话）、用户详情公钥、仪表盘在线会话

**Files:**
- Modify: `web/admin/src/views/InstanceDetailView.vue`、`web/admin/src/views/UserDetailView.vue`、`web/admin/src/views/DashboardView.vue`
- Modify: `web/admin/src/logic/instanceOps.ts`（`showPortActions`）、`web/admin/src/logic/__tests__/instanceOps.spec.ts`

**Interfaces:**
- Consumes: Task 19 后台接口；Task 20 类型与 `api.del`。
- Produces `data-testid`：`adetail-password-auth`、`adetail-hostkey`、`adetail-port-row`、`adetail-port-url`、`adetail-port-visibility`、`adetail-port-forced`、`adetail-port-force`、`adetail-port-delete`、`port-action-dialog`、`port-action-reason`、`port-action-error`、`port-action-submit`、`adetail-session-row`、`adetail-session-ip`、`adetail-session-method`、`adetail-sessions-empty`；`udetail-key-row`、`udetail-key-name`、`udetail-key-fp`；`dash-ssh-sessions`。
- `showPortActions(role: Role): boolean`（ops / super）。

- [ ] **Step 1: 写失败的单元测试**

`web/admin/src/logic/__tests__/instanceOps.spec.ts` 追加：

```ts
describe('showPortActions', () => {
  it('只有运维与超管能处置端口', () => {
    expect(showPortActions('ops')).toBe(true)
    expect(showPortActions('super')).toBe(true)
    expect(showPortActions('finance')).toBe(false)
  })
})
```

（import 里加 `showPortActions`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `cd web && npm test -- instanceOps 2>&1 | tail -5`
Expected: `showPortActions` 不存在。

- [ ] **Step 3: 实现**

`web/admin/src/logic/instanceOps.ts` 追加：

```ts
/** 端口处置（强制私有 / 删除）：运维与超管。 */
export const showPortActions = (role: Role): boolean => role === 'ops' || role === 'super'
```

（文件已 import `Role`；若没有，`import type { Role } from '@spark/shared'`。）

`InstanceDetailView.vue` `<script setup>` 追加（import 补 `type AdminSshSession`、`type InstancePort`、`type Paged`、`showPortActions`）：

```ts
// ---- 访问：端口处置与 SSH 会话 ----
const sessions = ref<AdminSshSession[]>([])
const sessionsTotal = ref(0)
async function loadSessions(target = id.value) {
  try {
    const p = await api.get<Paged<AdminSshSession>>(`/instances/${target}/ssh-sessions`, { page: 1, page_size: 20 })
    sessions.value = p.items
    sessionsTotal.value = p.total
  } catch (e) {
    notifyError(e)
  }
}
const portAction = reactive({ visible: false, kind: 'force' as 'force' | 'delete', port: 0, reason: '', error: '', busy: false })
function openPortAction(kind: 'force' | 'delete', p: InstancePort) {
  Object.assign(portAction, { visible: true, kind, port: p.port, reason: '', error: '' })
}
async function submitPortAction() {
  if (portAction.busy) return
  portAction.error = portAction.reason.trim() ? '' : '请填写原因'
  if (portAction.error) return
  portAction.busy = true
  try {
    if (portAction.kind === 'force') await api.post(`/instances/${id.value}/ports/${portAction.port}/force-private`, { reason: portAction.reason.trim() })
    else await api.del(`/instances/${id.value}/ports/${portAction.port}`, { reason: portAction.reason.trim() })
    ElMessage.success(portAction.kind === 'force' ? '已限制为私有' : '已删除端口')
    portAction.visible = false
    await load()
  } catch (e) {
    notifyError(e)
  } finally {
    portAction.busy = false
  }
}
```

`onMounted` 里加 `void loadSessions()`；`onBeforeRouteUpdate` 里加 `sessions.value = []`、`portAction.visible = false`、`void loadSessions(String(to.params.id))`。模板里「指令」卡片之后追加：

```vue
      <el-card header="访问" data-testid="adetail-access-card">
        <el-descriptions :column="2" border>
          <el-descriptions-item label="密码登录"><span data-testid="adetail-password-auth">{{ inst.password_auth ? '已开启' : '已关闭（仅公钥）' }}</span></el-descriptions-item>
          <el-descriptions-item label="容器主机密钥"><span class="mono" data-testid="adetail-hostkey">{{ data.ssh_host_pubkey_fingerprint || '（存量实例，首连后登记）' }}</span></el-descriptions-item>
        </el-descriptions>
        <h4>Web 端口</h4>
        <table class="grid">
          <thead><tr><th>端口</th><th>备注</th><th>网址</th><th>可见性</th><th v-if="showPortActions(role)">操作</th></tr></thead>
          <tbody>
            <tr v-for="p in data.ports" :key="p.port" data-testid="adetail-port-row">
              <td>{{ p.port }}</td>
              <td>{{ p.note }}</td>
              <td class="mono" data-testid="adetail-port-url">{{ p.url }}</td>
              <td>
                <span data-testid="adetail-port-visibility">{{ p.visibility === 'public' ? '公开' : '私有' }}</span>
                <el-tag v-if="p.forced_private" type="danger" size="small" data-testid="adetail-port-forced">平台限制</el-tag>
              </td>
              <td v-if="showPortActions(role)">
                <el-button v-if="!p.forced_private" size="small" data-testid="adetail-port-force" @click="openPortAction('force', p)">强制私有</el-button>
                <el-button size="small" type="danger" plain data-testid="adetail-port-delete" @click="openPortAction('delete', p)">删除</el-button>
              </td>
            </tr>
            <tr v-if="!data.ports.length"><td colspan="5" class="muted empty">无端口</td></tr>
          </tbody>
        </table>
        <h4>最近 SSH 会话（共 {{ sessionsTotal }}）</h4>
        <table class="grid">
          <thead><tr><th>方式</th><th>来源 IP</th><th>开始</th><th>结束</th><th>上行 / 下行</th></tr></thead>
          <tbody>
            <tr v-for="s in sessions" :key="s.id" data-testid="adetail-session-row">
              <td data-testid="adetail-session-method">{{ s.auth_method === 'password' ? '密码' : '公钥' }}</td>
              <td class="mono" data-testid="adetail-session-ip">{{ s.client_ip }}</td>
              <td>{{ fmtTime(s.started_at) }}</td>
              <td>{{ s.ended_at ? fmtTime(s.ended_at) : '进行中' }}</td>
              <td>{{ s.bytes_up }} / {{ s.bytes_down }}</td>
            </tr>
            <tr v-if="!sessions.length"><td colspan="5" class="muted empty" data-testid="adetail-sessions-empty">无会话</td></tr>
          </tbody>
        </table>
      </el-card>

      <el-dialog v-model="portAction.visible" :title="portAction.kind === 'force' ? `将端口 ${portAction.port} 限制为私有` : `删除端口 ${portAction.port}`" width="460" align-center data-testid="port-action-dialog">
        <p class="muted">{{ portAction.kind === 'force' ? '用户将无法再把该端口设为公开（可改备注、可删除）。' : '删除后用户需要重新添加。' }}</p>
        <el-form label-position="top" @submit.prevent="submitPortAction">
          <el-form-item label="原因（必填，写入审计）"><el-input v-model="portAction.reason" data-testid="port-action-reason" /></el-form-item>
          <p v-if="portAction.error" class="error" data-testid="port-action-error">{{ portAction.error }}</p>
          <el-button :type="portAction.kind === 'force' ? 'warning' : 'danger'" native-type="submit" :loading="portAction.busy" data-testid="port-action-submit">确定</el-button>
        </el-form>
      </el-dialog>
```

`UserDetailView.vue`：`el-tabs` 里「实名记录」之后加

```vue
          <el-tab-pane label="SSH 公钥" name="keys">
            <table class="grid">
              <thead><tr><th>名称</th><th>指纹</th><th>添加时间</th><th>最近使用</th></tr></thead>
              <tbody>
                <tr v-for="k in data.ssh_keys" :key="k.id" data-testid="udetail-key-row">
                  <td data-testid="udetail-key-name">{{ k.name }}</td>
                  <td class="mono" data-testid="udetail-key-fp">{{ k.fingerprint }}</td>
                  <td>{{ fmtTime(k.created_at) }}</td>
                  <td>{{ k.last_used_at ? fmtTime(k.last_used_at) : '—' }}</td>
                </tr>
                <tr v-if="!data.ssh_keys.length"><td colspan="4" class="muted empty">无公钥</td></tr>
              </tbody>
            </table>
          </el-tab-pane>
```

`DashboardView.vue`：「实例」卡片的 `<ul class="kv">` 末尾加 `<li><span>在线 SSH 会话</span><b data-testid="dash-ssh-sessions">{{ data.ssh_sessions_open }}</b></li>`。

- [ ] **Step 4: 运行测试确认通过**

Run: `cd web && npm test 2>&1 | tail -5 && npm run build 2>&1 | tail -3`
Expected: 通过（后台既有 Playwright 用例在 Task 22 全量运行）。

- [ ] **Step 5: 提交**

```bash
git add web/admin
git commit -m "feat(admin): instance access card (ports actions, ssh sessions), user ssh keys tab, open ssh sessions on dashboard

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 22: Go 集成测试 + e2e-server（SSH 网关、假上游、新钩子）+ Playwright 用例 + 覆盖表

**Files:**
- Create: `internal/app/access_lifecycle_test.go`
- Modify: `internal/app/app.go`（`Options.HostKeyGen`）、`cmd/e2e-server/main.go`、`internal/e2ehooks/hooks.go`、`internal/e2ehooks/hooks_test.go`
- Modify: `web/playwright.config.ts`、`web/e2e/helpers/hooks.ts`、`web/e2e/helpers/console.ts`、`web/e2e/coverage.json`
- Create: `web/e2e/console/ssh-keys.spec.ts`、`web/e2e/console/ingress.spec.ts`
- Modify: `web/e2e/console/instance-detail.spec.ts`、`web/e2e/admin/instances.spec.ts`、`web/e2e/admin/users.spec.ts`

**Interfaces:**
- Produces:
  - `app.Options.HostKeyGen func() (pub string, pem []byte, err error)`（透传给 `rental.Deps.HostKeyGen`；测试 / e2e 注入「总是返回假上游主机密钥」）。
  - e2e-server：`sshAddr = "127.0.0.1:18022"`；进程内 `sshtest` 假上游（`cfg.UpstreamSSHPort` = 其端口）；`cfg.Domain = cfg.IngressDomain = "spark.test"`、`PublicBaseURL = "http://spark.test:18080"`；`e2ehooks.Server` 新字段 `SSHAddr string`、`Upstream *sshtest.Server`。
  - 钩子：`POST /hooks/ssh {instance_id, password?, key_pem?, command}` → `{output, exit_status, error}`（进程内 `x/crypto/ssh` 客户端经网关执行，`error` 为连接 / 认证失败原因，banner 并入 `output`）；`POST /hooks/ingress-upstream {port, body}` → 假上游在该端口回 `body`；`/hooks/machines` 造的机器 `lan_ip = 127.0.0.1`。
  - `hooks.ts`：`hooks.ssh(opts)`、`hooks.ingressUpstream(port, body)`；`console.ts`：`loginConsoleAt(page, origin, phone, expectPath?)`（在指定 origin 登录，ingress 用例用 `http://spark.test:18080`）；`CONSOLE_SITE_URL = 'http://spark.test:18080'`。
  - Playwright：`use.launchOptions.args = ['--host-resolver-rules=MAP *.spark.test 127.0.0.1']`；覆盖表新增 `C-KEY-01`～`05`、`C-DET-11`～`16`、`C-ING-01`～`06`、`A-INS-10`～`12`、`A-USR-07`。

- [ ] **Step 1: Go 集成测试（先写，确认失败）**

`internal/app/access_lifecycle_test.go`：

```go
package app_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/app"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
)

// TestAccessLifecycle：下单 → 密码登录 → 公钥 → 关闭密码 → 端口私有 / 公开 → 登出失效 → 到期停机 → 续费 → 换机 → 释放。
func TestAccessLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	keyFile := upstreamKeyFile(t)
	upSigner, err := pki.LoadUpstreamSigner(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	hostPub, hostPEM, _ := pki.GenerateInstanceHostKey()
	hostSigner, _ := ssh.ParsePrivateKey(hostPEM)
	upstream, err := sshtest.New(sshtest.Options{AuthorizedKey: upSigner.PublicKey(), HostKey: hostSigner})
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	stop := upstream.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "lab:"+r.Host) }))
	defer stop()
	cfg := config.Config{UpstreamSSHKeyFile: keyFile, Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock",
		UpstreamSSHPort: upstream.Port(), SSHPublicPort: 2222}
	a, err := app.Build(cfg, pool, discardLog(), app.Options{Clock: clk, HostKeyGen: func() (string, []byte, error) { return hostPub, hostPEM, nil }})
	if err != nil {
		t.Fatal(err)
	}
	sshLis, _ := net.Listen("tcp", "127.0.0.1:0")
	gwCtx, stopGW := context.WithCancel(ctx)
	gwDone := make(chan struct{})
	go func() { defer close(gwDone); a.SSH.Serve(gwCtx, sshLis) }()
	defer func() { stopGW(); <-gwDone }()
	loopCtx, stopLoops := context.WithCancel(ctx)
	go a.RunAccessLoops(loopCtx)
	defer stopLoops()
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	tsPort := strings.TrimPrefix(ts.URL, "http://127.0.0.1:")
	// 对外地址按 FillAccessDefaults 是 http://spark.test:8080；Cookie / 跳转校验只看 scheme 与域名格式，这里用 Host 头模拟
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	mid := dbtest.CreateMachine(t, pool, "SN001")
	pool.Exec(ctx, `UPDATE machines SET lan_ip = '127.0.0.1' WHERE id = $1`, mid)
	agent := func() {
		t.Helper()
		a.Fleet.Heartbeat(ctx, mid)
		cmds, _ := a.Fleet.PendingCommands(ctx, mid)
		for _, cmd := range cmds {
			if err := a.Rental.HandleCommandResult(ctx, cmd.ID, true, "ok"); err != nil {
				t.Fatal(err)
			}
		}
	}
	c.do("POST", "/auth/sms-code", map[string]string{"phone": "13800005678"})
	c.do("POST", "/auth/login", map[string]string{"phone": "13800005678", "code": "000000"})
	c.do("POST", "/kyc/personal", map[string]string{"name": "王五", "id_number": "110101199001011234"})
	rch := c.do("POST", "/recharges", map[string]any{"amount": 100000, "channel": "wechat"})
	c.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{})
	products := c.do("GET", "/products", nil)["items"].([]any)
	images := c.do("GET", "/images", nil)["items"].([]any)
	order := c.do("POST", "/orders", map[string]any{"product_id": products[0].(map[string]any)["id"], "duration": "1d", "image_id": images[0].(map[string]any)["id"]})
	id := order["instance"].(map[string]any)["id"].(string)
	agent()
	inst := c.do("GET", "/instances/"+id, nil)
	if inst["state"] != "running" || inst["ssh_command"] != "ssh -p 2222 "+id+"@ssh.spark.test" {
		t.Fatalf("instance = %v", inst)
	}
	password := c.do("GET", "/instances/"+id+"/password", nil)["password"].(string)

	gwAddr := sshLis.Addr().String()
	sshExec := func(auth ssh.AuthMethod, cmd string) (string, error) {
		cl, err := ssh.Dial("tcp", gwAddr, &ssh.ClientConfig{User: id, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: ssh.FixedHostKey(a.SSHHostKey.PublicKey()), Timeout: 5 * time.Second})
		if err != nil {
			return "", err
		}
		defer cl.Close()
		s, err := cl.NewSession()
		if err != nil {
			return "", err
		}
		defer s.Close()
		out, err := s.Output(cmd)
		return string(out), err
	}
	if out, err := sshExec(ssh.Password(password), "echo hi"); err != nil || out != "hi\n" {
		t.Fatalf("password login = %q %v", out, err)
	}
	// 公钥 + 关闭密码
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	userSigner, _ := ssh.NewSignerFromKey(priv)
	sshPub, _ := ssh.NewPublicKey(pub)
	c.do("POST", "/ssh-keys", map[string]string{"public_key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))})
	if got := c.do("PUT", "/instances/"+id+"/access", map[string]bool{"password_auth": false}); got["password_auth"] != false {
		t.Fatalf("disable password = %v", got)
	}
	if _, err := sshExec(ssh.Password(password), "echo hi"); err == nil {
		t.Fatal("password accepted while disabled")
	}
	if out, err := sshExec(ssh.PublicKeys(userSigner), "echo key"); err != nil || out != "key\n" {
		t.Fatalf("key login = %q %v", out, err)
	}
	// 端口：私有 → 302 到 authorize → callback → 200；公开 → 直接 200
	c.do("POST", "/instances/"+id+"/ports", map[string]any{"port": 8888, "note": "lab"})
	time.Sleep(2500 * time.Millisecond) // 路由表 2 秒刷新
	host := id + "-8888.spark.test"
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	getHost := func(client *http.Client, h, path string) *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = h
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r1 := getHost(noFollow, host, "/lab")
	r1.Body.Close()
	if r1.StatusCode != 302 || !strings.Contains(r1.Header.Get("Location"), "/api/v1/ingress/authorize?target=") {
		t.Fatalf("private no cookie = %d %s", r1.StatusCode, r1.Header.Get("Location"))
	}
	target := url.QueryEscape("http://" + host + ":8080/lab")
	r2 := getHost(noFollow, "spark.test", "/api/v1/ingress/authorize?target="+target)
	r2.Body.Close()
	cb, _ := url.Parse(r2.Header.Get("Location"))
	if r2.StatusCode != 302 || cb.Path != "/.spark/callback" {
		t.Fatalf("authorize = %d %s", r2.StatusCode, r2.Header.Get("Location"))
	}
	r3 := getHost(noFollow, host, "/.spark/callback?"+cb.RawQuery)
	r3.Body.Close()
	var ingressCookie *http.Cookie
	for _, ck := range r3.Cookies() {
		if ck.Name == "sc_ingress" {
			ingressCookie = ck
		}
	}
	if r3.StatusCode != 302 || r3.Header.Get("Location") != "/lab" || ingressCookie == nil {
		t.Fatalf("callback = %d %s cookies=%v", r3.StatusCode, r3.Header.Get("Location"), r3.Cookies())
	}
	withCookie := func(h, path string) *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = h
		req.AddCookie(ingressCookie)
		resp, _ := noFollow.Do(req)
		return resp
	}
	r4 := withCookie(host, "/lab")
	b4, _ := io.ReadAll(r4.Body)
	r4.Body.Close()
	if r4.StatusCode != 200 || string(b4) != "lab:"+host {
		t.Fatalf("private with cookie = %d %q", r4.StatusCode, b4)
	}
	c.do("PUT", "/instances/"+id+"/ports/8888", map[string]string{"visibility": "public"})
	time.Sleep(2500 * time.Millisecond)
	if r := getHost(http.DefaultClient, host, "/"); r.StatusCode != 200 {
		t.Fatalf("public = %d", r.StatusCode)
	}
	// 登出控制台 → 私有端口 Cookie 失效
	c.do("PUT", "/instances/"+id+"/ports/8888", map[string]string{"visibility": "private"})
	c.do("POST", "/auth/logout", map[string]any{})
	clk.Advance(3 * time.Second)
	time.Sleep(2500 * time.Millisecond)
	if r := withCookie(host, "/lab"); r.StatusCode != 302 {
		t.Fatalf("after logout = %d", r.StatusCode)
	}
	c.do("POST", "/auth/sms-code", map[string]string{"phone": "13800005678"})
	c.do("POST", "/auth/login", map[string]string{"phone": "13800005678", "code": "000000"})
	// 到期停机 → SSH 拒绝、Web 503 → 续费恢复
	clk.Advance(25 * time.Hour)
	a.Fleet.Heartbeat(ctx, mid)
	a.Rental.Sweep(ctx)
	agent()
	if _, err := sshExec(ssh.PublicKeys(userSigner), "echo x"); err == nil {
		t.Fatal("ssh allowed while stopped")
	}
	time.Sleep(2500 * time.Millisecond)
	if r := getHost(http.DefaultClient, host, "/"); r.StatusCode != 503 {
		t.Fatalf("stopped web = %d", r.StatusCode)
	}
	c.do("POST", "/instances/"+id+"/renew", map[string]string{"duration": "1d"})
	agent()
	if out, err := sshExec(ssh.PublicKeys(userSigner), "echo back"); err != nil || out != "back\n" {
		t.Fatalf("after renew = %q %v", out, err)
	}
	// 释放 → 拒绝、主机私钥清空
	c.do("POST", "/instances/"+id+"/release", map[string]any{})
	agent()
	if _, err := sshExec(ssh.PublicKeys(userSigner), "echo x"); err == nil {
		t.Fatal("ssh allowed after release")
	}
	if pem, _ := a.Rental.HostKeyPEM(ctx, id); pem != "" {
		t.Fatal("host key not cleared after release")
	}
	var payload string
	pool.QueryRow(ctx, `SELECT payload::text FROM agent_commands WHERE instance_id = $1 AND type = 'create_instance'`, id).Scan(&payload)
	if strings.Contains(payload, "PRIVATE") {
		t.Fatal("host key leaked into command payload")
	}
}
```

Run: `go test ./internal/app/ -run TestAccessLifecycle 2>&1 | head -5`
Expected: 编译错误（`Options.HostKeyGen` 不存在）。

- [ ] **Step 2: `Options.HostKeyGen` 与 e2e-server**

`internal/app/app.go` 的 `Options` 追加 `HostKeyGen func() (pub string, pem []byte, err error) // 测试注入：实例主机密钥生成`，`rental.Deps{...}` 加 `HostKeyGen: opt.HostKeyGen`。

`cmd/e2e-server/main.go`：常量加 `sshAddr = "127.0.0.1:18022"`；生成上游密钥之后：

```go
	upSigner, err := pki.LoadUpstreamSigner(keyFile)
	if err != nil {
		return err
	}
	hostPub, hostPEM, err := pki.GenerateInstanceHostKey()
	if err != nil {
		return err
	}
	hostSigner, _ := ssh.ParsePrivateKey(hostPEM)
	upstream, err := sshtest.New(sshtest.Options{AuthorizedKey: upSigner.PublicKey(), HostKey: hostSigner}) // 所有 e2e 实例共用这把主机密钥
	if err != nil {
		return err
	}
	defer upstream.Close()
```

`cfg` 字面量加 `IngressDomain: "spark.test", PublicBaseURL: "http://spark.test:18080", SSHAddr: sshAddr, SSHPublicPort: 22, UpstreamSSHPort: upstream.Port()`；`app.Options{...}` 加 `HostKeyGen: func() (string, []byte, error) { return hostPub, hostPEM, nil }`；网关监听之后：

```go
	sshLis, err := net.Listen("tcp", sshAddr)
	if err != nil {
		return fmt.Errorf("ssh gateway: %w", err)
	}
	sshErr := make(chan error, 1)
	go func() { sshErr <- a.SSH.Serve(gwCtx, sshLis) }()
	defer func() { <-sshErr }()
	go a.RunAccessLoops(gwCtx)
```

（放在 `defer func() { stopGW(); <-gwErr }()` 之后，这样 defer 逆序先等 SSH 网关、再停网关 ctx。）`hooksSrv` 字面量加 `SSHAddr: sshAddr, Upstream: upstream`。import 追加 `"golang.org/x/crypto/ssh"`、`"spark-cloud/internal/access/sshtest"`。

- [ ] **Step 3: 钩子**

`internal/e2ehooks/hooks.go`：`Server` 加字段 `SSHAddr string`、`Upstream *sshtest.Server`；`Handler()` 加 `mux.HandleFunc("POST /hooks/ssh", s.handle(s.ssh))`、`mux.HandleFunc("POST /hooks/ingress-upstream", s.handle(s.ingressUpstream))`；`machines()` 里 `LanIP: fmt.Sprintf("10.99.%d.%d", …)` 改为 `LanIP: "127.0.0.1"`（`agentFor` 传给 fake Agent 的 `IPs` 已经取 `m.LanIP`）。追加：

```go
// ssh 用进程内 ssh 客户端经网关执行命令（验证页面上显示的密码 / 公钥真的能登录）。
func (s *Server) ssh(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		InstanceID string `json:"instance_id"`
		Password   string `json:"password"`
		KeyPEM     string `json:"key_pem"`
		Command    string `json:"command"`
	}](body)
	if err != nil {
		return nil, err
	}
	var auth ssh.AuthMethod
	switch {
	case req.KeyPEM != "":
		signer, err := ssh.ParsePrivateKey([]byte(req.KeyPEM))
		if err != nil {
			return nil, badRequest("key_pem 无法解析: " + err.Error())
		}
		auth = ssh.PublicKeys(signer)
	default:
		auth = ssh.Password(req.Password)
	}
	var banner strings.Builder
	cl, err := ssh.Dial("tcp", s.SSHAddr, &ssh.ClientConfig{User: req.InstanceID, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout: 5 * time.Second, BannerCallback: func(m string) error { banner.WriteString(m); return nil }})
	if err != nil {
		return map[string]any{"output": banner.String(), "exit_status": -1, "error": err.Error()}, nil
	}
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		return map[string]any{"output": banner.String(), "exit_status": -1, "error": err.Error()}, nil
	}
	defer sess.Close()
	var out, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &stderr
	err = sess.Run(req.Command)
	status := 0
	errText := ""
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		status = exitErr.ExitStatus()
	} else if err != nil {
		status, errText = -1, err.Error()
	}
	return map[string]any{"output": banner.String() + out.String() + stderr.String(), "exit_status": status, "error": errText}, nil
}

// ingressUpstream 让假上游在容器内 127.0.0.1:<port> 返回固定内容（带 Host 以便断言域名透传）。
func (s *Server) ingressUpstream(_ context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Port int    `json:"port"`
		Body string `json:"body"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Port <= 0 || req.Port > 65535 {
		return nil, badRequest("port 不正确")
	}
	s.Upstream.ServeHTTP(req.Port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%s|host=%s|path=%s", req.Body, r.Host, r.URL.RequestURI())
	}))
	return map[string]bool{"ok": true}, nil
}
```

（import 追加 `"bytes"`、`"strings"`、`"golang.org/x/crypto/ssh"`、`"spark-cloud/internal/access/sshtest"`。）`hooks_test.go` 追加一个用例：造机器后 `lan_ip` 为 `127.0.0.1`；`/hooks/ssh` 对不存在的实例返回 `error` 非空且 `output` 含「用户名应为实例 ID」（需要测试里起 sshgw：用 `app` 的 `a.SSH.Serve` 在 `127.0.0.1:0` 监听并把地址写进 `Server.SSHAddr`）。

- [ ] **Step 4: Playwright 配置与辅助**

`web/playwright.config.ts`：`use` 里加

```ts
    // 让 *.spark.test（控制台 spark.test 与租户子域 ins-*-<port>.spark.test）都解析到本机：Chrome 与 Playwright 自带 Chromium 都支持
    launchOptions: { args: ['--host-resolver-rules=MAP *.spark.test 127.0.0.1'] },
```

并导出 `export const CONSOLE_SITE_URL = 'http://spark.test:18080'`。`web/e2e/helpers/hooks.ts` 的 `hooks` 对象追加：

```ts
  /** 用进程内 ssh 客户端经网关执行命令：验证页面上显示的密码 / 公钥真的能登录。error 非空表示连接或认证失败，banner 在 output 里。 */
  ssh(opts: { instance_id: string; password?: string; key_pem?: string; command: string }) {
    return post<{ output: string; exit_status: number; error: string }>('/hooks/ssh', opts)
  },
  /** 让假上游在容器内 127.0.0.1:<port> 返回 body（响应形如 `body|host=…|path=…`）。 */
  async ingressUpstream(port: number, body: string): Promise<void> {
    await post('/hooks/ingress-upstream', { port, body })
  },
```

并 `export const CONSOLE_SITE_URL = 'http://spark.test:18080'`。`web/e2e/helpers/console.ts` 追加：

```ts
/** 在指定 origin 登录（ingress 用例要在 http://spark.test:18080 持有会话，Cookie 是 host-only 的）。 */
export async function loginConsoleAt(page: Page, origin: string, phone: string, expectPath = '/market') {
  await hooks.advance(61)
  await page.goto(`${origin}/login`)
  await page.getByTestId('login-phone').fill(phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(new RegExp(`${expectPath.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(\\?|$)`))
}
```

- [ ] **Step 5: 控制台用例**

`web/e2e/console/ssh-keys.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { loginConsole, nav, seedUser, toast } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { runningInstance } from '../helpers/instances'

// 固定的测试密钥对（仅 e2e 使用）：公钥注释 e2e@laptop
const PRIV = `-----BEGIN OPENSSH PRIVATE KEY-----
<用 ssh-keygen -t ed25519 -C e2e@laptop -N '' -f e2e_key 生成后把 e2e_key 的内容原样粘贴到这里>
-----END OPENSSH PRIVATE KEY-----
`
const PUB = '<e2e_key.pub 的一行内容，含注释 e2e@laptop>'

test('C-KEY-01 添加公钥：名称缺省取注释，列表显示指纹', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await nav(page, info, 'account')
  await page.getByTestId('account-ssh-keys').click()
  await expect(page).toHaveURL(/\/account\/ssh-keys$/)
  await expect(page.getByTestId('keys-empty')).toBeVisible()
  await page.getByTestId('keys-add-btn').click()
  await page.getByTestId('keys-input').fill(PUB + '\r\n')
  await page.getByTestId('keys-submit').click()
  await toast(page, '已添加')
  await expect(page.getByTestId('keys-row')).toHaveCount(1)
  await expect(page.getByTestId('keys-name')).toHaveText('e2e@laptop')
  await expect(page.getByTestId('keys-fp')).toHaveText(/^SHA256:/)
  await expect(page.getByTestId('keys-count')).toHaveText('1 / 10')
})

test('C-KEY-02 无效公钥：前端拦截与后端拒绝都显示原因', async ({ page }) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.goto('/account/ssh-keys')
  await page.getByTestId('keys-add-btn').click()
  await page.getByTestId('keys-input').fill('-----BEGIN OPENSSH PRIVATE KEY-----')
  await page.getByTestId('keys-submit').click()
  await expect(page.getByTestId('keys-error')).toContainText('请粘贴一行公钥')
  await page.getByTestId('keys-input').fill('ssh-ed25519 AAAAnotvalidbase64')
  await page.getByTestId('keys-submit').click()
  await expect(page.getByTestId('keys-error')).toContainText('公钥格式不正确')
})

test('C-KEY-03 重复公钥提示已添加', async ({ page }) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.goto('/account/ssh-keys')
  for (let i = 0; i < 2; i++) {
    await page.getByTestId('keys-add-btn').click()
    await page.getByTestId('keys-input').fill(PUB)
    await page.getByTestId('keys-submit').click()
  }
  await expect(page.getByTestId('keys-error')).toHaveText('该公钥已添加')
  await page.getByTestId('keys-cancel').click()
  await expect(page.getByTestId('keys-row')).toHaveCount(1)
})

test('C-KEY-04 公钥能登录实例；删除后不能', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto('/account/ssh-keys')
  await page.getByTestId('keys-add-btn').click()
  await page.getByTestId('keys-input').fill(PUB)
  await page.getByTestId('keys-submit').click()
  await expect(page.getByTestId('keys-row')).toHaveCount(1)
  const ok = await hooks.ssh({ instance_id: id, key_pem: PRIV, command: 'echo hi' })
  expect(ok.error).toBe('')
  expect(ok.output).toBe('hi\n')
  await page.getByTestId('keys-delete').click()
  await page.getByTestId('keys-delete-ok').click()
  await toast(page, '已删除')
  await expect(page.getByTestId('keys-empty')).toBeVisible()
  const denied = await hooks.ssh({ instance_id: id, key_pem: PRIV, command: 'echo hi' })
  expect(denied.error).not.toBe('')
})

test('C-KEY-05 公钥上限 10 个：按钮禁用', async ({ page }) => {
  const u = await seedUser()
  const { api } = await runningInstance(u.phone)
  // 用接口造 10 把不同的 ed25519 公钥（base64 段合法即可：真实生成）
  for (let i = 0; i < 10; i++) {
    const r = await api.post('/api/v1/ssh-keys', { data: { public_key: await freshPublicKey(), name: `k${i}` } })
    expect(r.status(), await r.text()).toBe(201)
  }
  await loginConsole(page, u.phone)
  await page.goto('/account/ssh-keys')
  await expect(page.getByTestId('keys-row')).toHaveCount(10)
  await expect(page.getByTestId('keys-count')).toHaveText('10 / 10')
  await expect(page.getByTestId('keys-add-btn')).toBeDisabled()
})

/** 在 Node 里生成一把 ed25519 公钥的 authorized_keys 行（node:crypto 导出 SPKI → 转 OpenSSH wire 格式）。 */
async function freshPublicKey(): Promise<string> {
  const { generateKeyPairSync } = await import('node:crypto')
  const { publicKey } = generateKeyPairSync('ed25519')
  const der = publicKey.export({ type: 'spki', format: 'der' }) as Buffer
  const raw = der.subarray(der.length - 32) // SPKI 的最后 32 字节是 ed25519 公钥
  const type = Buffer.from('ssh-ed25519')
  const len = (b: Buffer) => { const l = Buffer.alloc(4); l.writeUInt32BE(b.length); return l }
  return `ssh-ed25519 ${Buffer.concat([len(type), type, len(raw), raw]).toString('base64')}`
}
```

实施时用 `ssh-keygen -t ed25519 -C e2e@laptop -N '' -f /tmp/e2e_key` 生成一次，把 `/tmp/e2e_key` 与 `/tmp/e2e_key.pub` 的内容填进 `PRIV` / `PUB` 常量（这把密钥只在 e2e 里用，入库无妨；文件顶部注释写明）。

`web/e2e/console/instance-detail.spec.ts` 追加：

```ts
test('C-DET-11 重置密码后新密码显示且能登录，旧密码不能', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-password-show').click()
  const old = (await page.getByTestId('detail-password').textContent())!
  await page.getByTestId('detail-password-reset').click()
  await page.getByTestId('detail-reset-ok').click()
  await toast(page, '密码已重置')
  const fresh = (await page.getByTestId('detail-password').textContent())!
  expect(fresh).toMatch(/^[A-Za-z0-9]{16}$/)
  expect(fresh).not.toBe(old)
  expect((await hooks.ssh({ instance_id: id, password: fresh, command: 'echo ok' })).output).toBe('ok\n')
  expect((await hooks.ssh({ instance_id: id, password: old, command: 'echo ok' })).error).not.toBe('')
})

test('C-DET-12 关闭密码登录：无公钥时提示；有公钥后关闭成功且密码被拒', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-password-auth').click()
  await toast(page, '请先在「我的 → SSH 公钥」添加公钥')
  await expect(page.getByTestId('detail-password-auth')).toHaveAttribute('aria-checked', 'true')
  await api.post('/api/v1/ssh-keys', { data: { public_key: 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGt3oO2v5cS8m3m2Q4nE5x9P1r7xq0c8Wl3bT2uYhVZk e2e' } })
  await page.getByTestId('detail-password-show').click()
  const pw = (await page.getByTestId('detail-password').textContent())!
  await page.getByTestId('detail-password-auth').click()
  await toast(page, '已关闭密码登录')
  await expect(page.getByTestId('detail-password-auth')).toHaveAttribute('aria-checked', 'false')
  expect((await hooks.ssh({ instance_id: id, password: pw, command: 'echo ok' })).error).not.toBe('')
})

test('C-DET-13 添加端口显示网址；非法端口与 22 被拦截', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-port-input').fill('22')
  await page.getByTestId('detail-port-add').click()
  await expect(page.getByTestId('detail-port-error')).toContainText('22 端口')
  await page.getByTestId('detail-port-input').fill('8888')
  await page.getByTestId('detail-port-note').fill('JupyterLab')
  await page.getByTestId('detail-port-add').click()
  await expect(page.getByTestId('detail-port-row')).toHaveCount(1)
  await expect(page.getByTestId('detail-port-url')).toHaveText(`http://${id}-8888.spark.test:18080/`)
  await expect(page.getByTestId('detail-port-visibility')).toHaveText('私有')
  await expect(page.getByTestId('detail-port-row')).toContainText('JupyterLab')
})

test('C-DET-14 设为公开需二次确认（含警告文案）；取消不改', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888 } })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-port-toggle').click()
  await expect(page.getByTestId('detail-port-public-text')).toContainText('任何人')
  await page.getByTestId('detail-port-public-cancel').click()
  await expect(page.getByTestId('detail-port-visibility')).toHaveText('私有')
  await page.getByTestId('detail-port-toggle').click()
  await page.getByTestId('detail-port-public-ok').click()
  await expect(page.getByTestId('detail-port-visibility')).toHaveText('公开')
  await expect(page.getByTestId('detail-port-toggle')).toHaveText('设为私有')
})

test('C-DET-15 端口上限 5 个：输入框消失并提示', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  for (const p of [8001, 8002, 8003, 8004, 8005]) await api.post(`/api/v1/instances/${id}/ports`, { data: { port: p } })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-port-row')).toHaveCount(5)
  await expect(page.getByTestId('detail-port-input')).toHaveCount(0)
  await expect(page.getByTestId('detail-port-limit')).toHaveText('每个实例最多开放 5 个端口')
})

test('C-DET-16 删除端口', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888 } })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-port-delete').click()
  await expect(page.getByTestId('detail-port-row')).toHaveCount(0)
  const list = (await (await api.get(`/api/v1/instances/${id}/ports`)).json()) as { items: unknown[] }
  expect(list.items).toHaveLength(0)
})
```

`web/e2e/console/ingress.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { loginConsoleAt, seedUser } from '../helpers/console'
import { CONSOLE_SITE_URL, hooks } from '../helpers/hooks'
import { instanceOf, runningInstance } from '../helpers/instances'

const portUrl = (id: string, port: number) => `http://${id}-${port}.spark.test:18080`

test('C-ING-01 私有端口：未登录 → 登录页 → 回到目标页，内容来自容器端口', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888 } })
  await hooks.ingressUpstream(8888, 'hello-lab')
  await hooks.advance(61)
  await page.goto(`${portUrl(id, 8888)}/lab/tree?x=1`)
  await expect(page).toHaveURL(/spark\.test:18080\/login\?redirect=/)
  await page.getByTestId('login-phone').fill(u.phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(`${portUrl(id, 8888)}/lab/tree?x=1`)
  await expect(page.locator('body')).toHaveText(`hello-lab|host=${id}-8888.spark.test:18080|path=/lab/tree?x=1`)
  // 再次访问不再跳转
  await page.goto(`${portUrl(id, 8888)}/again`)
  await expect(page.locator('body')).toContainText('path=/again')
})

test('C-ING-02 非主人访问私有端口 → 无权访问', async ({ page }) => {
  const owner = await seedUser()
  const { api, id } = await runningInstance(owner.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888 } })
  await hooks.ingressUpstream(8888, 'secret')
  const other = await seedUser()
  await loginConsoleAt(page, CONSOLE_SITE_URL, other.phone)
  await page.goto(`${portUrl(id, 8888)}/`)
  await expect(page.locator('body')).toContainText('无权访问该实例')
  await expect(page.locator('body')).not.toContainText('secret')
})

test('C-ING-03 公开端口免登录直接访问', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 7860, visibility: 'public' } })
  await hooks.ingressUpstream(7860, 'gradio')
  await page.goto(`${portUrl(id, 7860)}/`)
  await expect(page.locator('body')).toContainText('gradio|host=')
})

test('C-ING-04 实例停机后显示 503 页', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 7860, visibility: 'public' } })
  await hooks.ingressUpstream(7860, 'x')
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent()
  expect((await instanceOf(api, id)).state).toBe('stopped')
  await expect.poll(async () => (await page.goto(`${portUrl(id, 7860)}/`))!.status(), { timeout: 10_000 }).toBe(503)
  await expect(page.locator('body')).toContainText('实例已到期停机')
})

test('C-ING-05 未开放的端口 404', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  const resp = await page.goto(`${portUrl(id, 9999)}/`)
  expect(resp!.status()).toBe(404)
  await expect(page.locator('body')).toContainText('该端口未开放')
})

test('C-ING-06 被后台强制私有后，用户改公开被拒并显示平台限制', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888, visibility: 'public' } })
  const { adminApi, adminPost, createAdmin } = await import('../helpers/admin')
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  await adminPost(ctx, admin, `/admin/api/v1/instances/${id}/ports/8888/force-private`, { reason: 'e2e' })
  await loginConsoleAt(page, CONSOLE_SITE_URL, u.phone)
  await page.goto(`${CONSOLE_SITE_URL}/instances/${id}`)
  await expect(page.getByTestId('detail-port-forced')).toBeVisible()
  await expect(page.getByTestId('detail-port-toggle')).toHaveCount(0)
  const r = await api.put(`/api/v1/instances/${id}/ports/8888`, { data: { visibility: 'public' } })
  expect(r.status()).toBe(409)
  expect(((await r.json()) as { code: string }).code).toBe('port_forced_private')
})
```

`web/e2e/admin/instances.spec.ts` 追加：

```ts
test('A-INS-10 实例详情「访问」卡片：密码登录、主机密钥、端口与 SSH 会话', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888, visibility: 'public', note: 'lab' } })
  const pw = ((await (await api.get(`/api/v1/instances/${id}/password`)).json()) as { password: string }).password
  expect((await hooks.ssh({ instance_id: id, password: pw, command: 'echo 1' })).output).toBe('1\n')
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await expect(page.getByTestId('adetail-password-auth')).toHaveText('已开启')
  await expect(page.getByTestId('adetail-hostkey')).toHaveText(/^SHA256:/)
  await expect(page.getByTestId('adetail-port-row')).toHaveCount(1)
  await expect(page.getByTestId('adetail-port-url')).toHaveText(`http://${id}-8888.spark.test:18080/`)
  await expect(page.getByTestId('adetail-port-visibility')).toHaveText('公开')
  await expect(page.getByTestId('adetail-port-force')).toHaveCount(0) // 财务无处置按钮
  await expect(page.getByTestId('adetail-session-row')).toHaveCount(1)
  await expect(page.getByTestId('adetail-session-method')).toHaveText('密码')
  await expect(page.getByTestId('adetail-session-ip')).toHaveText('127.0.0.1')
})

test('A-INS-11 强制私有：需原因、写审计、用户端显示平台限制', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888, visibility: 'public' } })
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await page.getByTestId('adetail-port-force').click()
  await page.getByTestId('port-action-submit').click()
  await expect(page.getByTestId('port-action-error')).toHaveText('请填写原因')
  await page.getByTestId('port-action-reason').fill('钓鱼页面')
  await page.getByTestId('port-action-submit').click()
  await toast(page, '已限制为私有')
  await expect(page.getByTestId('adetail-port-visibility')).toHaveText('私有')
  await expect(page.getByTestId('adetail-port-forced')).toBeVisible()
  const ctx = await adminApi(admin)
  const audit = (await getJSON<{ items: { action: string; target_id: string; diff: { reason: string } }[] }>(ctx, '/admin/api/v1/audit?action=instance.port_force_private&page_size=5')).items
  expect(audit.some((a) => a.target_id === id && a.diff.reason === '钓鱼页面')).toBe(true)
})

test('A-INS-12 删除端口：需原因、写审计、用户端消失', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/ports`, { data: { port: 8888 } })
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await page.getByTestId('adetail-port-delete').click()
  await page.getByTestId('port-action-reason').fill('abuse')
  await page.getByTestId('port-action-submit').click()
  await toast(page, '已删除端口')
  await expect(page.getByTestId('adetail-port-row')).toHaveCount(0)
  const list = (await (await api.get(`/api/v1/instances/${id}/ports`)).json()) as { items: unknown[] }
  expect(list.items).toHaveLength(0)
})
```

`web/e2e/admin/users.spec.ts` 追加：

```ts
test('A-USR-07 用户详情显示 SSH 公钥', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  await api.post('/api/v1/ssh-keys', { data: { public_key: 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGt3oO2v5cS8m3m2Q4nE5x9P1r7xq0c8Wl3bT2uYhVZk laptop' } })
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  await page.getByRole('tab', { name: 'SSH 公钥' }).click()
  await expect(page.getByTestId('udetail-key-row')).toHaveCount(1)
  await expect(page.getByTestId('udetail-key-name')).toHaveText('laptop')
  await expect(page.getByTestId('udetail-key-fp')).toHaveText(/^SHA256:/)
})
```

（C-DET-12 与 A-USR-07 里的固定公钥是一把合法的 ed25519 公钥；若 `ssh.ParseAuthorizedKey` 拒绝该字面量，用 `ssh-keygen` 另生成一行替换。）

`web/e2e/coverage.json`：`console` 追加 `"C-DET-11"`～`"C-DET-16"`、`"C-KEY-01"`～`"C-KEY-05"`、`"C-ING-01"`～`"C-ING-06"`；`admin` 追加 `"A-INS-10"`、`"A-INS-11"`、`"A-INS-12"`、`"A-USR-07"`。`playwright.config.ts` 顶部的顺序说明补一句：`C-ING-04` 推进 1 天；`C-ING` 用例在 `spark.test:18080` 登录，与其它用例的 `127.0.0.1` 会话互不影响。

- [ ] **Step 6: 运行全部测试**

Run:
```bash
export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH
go vet ./... && go test ./... 2>&1 | tail -15
cd web && npm test 2>&1 | tail -5 && npm run e2e:coverage && npm run build && npx playwright test 2>&1 | tail -20
```
Expected: Go 全部 `ok`（含 `TestAccessLifecycle`）；覆盖表核对通过（控制台 +17、后台 +4）；Playwright 三个 project 全部通过（原有用例全绿：机器 `lan_ip` 改为 127.0.0.1 不影响现有断言——A-MCH 用例用 `registerMachine` 自己登记的 10.96.x 地址）。

- [ ] **Step 7: 提交**

```bash
git add internal/app cmd/e2e-server internal/e2ehooks web/playwright.config.ts web/e2e
git commit -m "test(access): go access lifecycle test; e2e-server ssh gateway + fake upstream + hooks; playwright keys/ports/ingress/admin cases

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 23: 部署产物：`deploy/compose`、platform 镜像、README 部署章节

**Files:**
- Create: `deploy/compose/docker-compose.yml`、`deploy/compose/.env.example`、`deploy/compose/README.md`、`deploy/platform/Dockerfile`
- Modify: `Makefile`（`platform-image` 目标）、`README.md`（「部署注意」与「节点 Agent」章节）

**Interfaces:**
- Produces: compose 服务 `platform`（端口 `22:2222`、`443:8443`、`80:8080`、`9443:9443`）、`postgres`、可选 `acme`；`.env.example` 的变量与端口映射一一对应（`SPARK_HTTP_ADDR=:8443`、`SPARK_HTTP_REDIRECT_ADDR=:8080`、`SPARK_SSH_ADDR=:2222`、`SPARK_AGENT_ADDR=:9443`）；`make platform-image` 构建 `spark-cloud/platform:<version>`。

- [ ] **Step 1: 文件**

`deploy/platform/Dockerfile`：

```dockerfile
# 多阶段：前端（Node 22）→ 后端（Go 1.27，前端产物 embed）→ 运行镜像
FROM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
COPY web/shared/package.json shared/
COPY web/console/package.json console/
COPY web/admin/package.json admin/
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/web/dist internal/web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/platform ./cmd/platform

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/platform /platform
USER nonroot
ENTRYPOINT ["/platform"]
```

`deploy/compose/docker-compose.yml`：

```yaml
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_USER: spark
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in .env}
      POSTGRES_DB: spark
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U spark"]
      interval: 10s
      timeout: 5s
      retries: 10
    restart: unless-stopped

  platform:
    image: spark-cloud/platform:${PLATFORM_VERSION:-latest}
    env_file: .env
    environment:
      SPARK_DATABASE_URL: postgres://spark:${POSTGRES_PASSWORD}@postgres:5432/spark?sslmode=disable
    depends_on:
      postgres:
        condition: service_healthy
    ports:
      - "22:2222"      # SSH 网关（宿主机自己的 sshd 先挪到别的端口，见 README）
      - "443:8443"     # 控制台 + 租户 Web 入口（TLS 由 platform 终止，证书来自 certs 卷）
      - "80:8080"      # 301 到 https
      - "9443:9443"    # Agent 网关（只对机房内网开放，防火墙限制来源）
    volumes:
      - certs:/certs:ro
      - uploads:/data/uploads
      - ${SPARK_UPSTREAM_SSH_KEY_HOST_PATH:?set SPARK_UPSTREAM_SSH_KEY_HOST_PATH}:/secrets/upstream.key:ro
    stop_grace_period: 45s   # > SPARK_SSH_DRAIN_TIMEOUT，让 SSH 会话 drain 完
    restart: unless-stopped

  # 可选：acme.sh 侧车按 DNS-01 申请 / 续签泛域名证书并写进 certs 卷；用商业证书时删掉本服务，把文件放进 certs 卷即可
  acme:
    image: neilpang/acme.sh:latest
    profiles: ["acme"]
    command: daemon
    environment:
      Ali_Key: ${ALI_KEY:-}
      Ali_Secret: ${ALI_SECRET:-}
    volumes:
      - acme:/acme.sh
      - certs:/certs
    restart: unless-stopped

volumes:
  pgdata:
  certs:
  uploads:
  acme:
```

`deploy/compose/.env.example`：

```dotenv
# 复制为 .env 后填写。端口对应：22→SPARK_SSH_ADDR、443→SPARK_HTTP_ADDR、80→SPARK_HTTP_REDIRECT_ADDR、9443→SPARK_AGENT_ADDR
POSTGRES_PASSWORD=change-me
PLATFORM_VERSION=latest
SPARK_ENV=prod
SPARK_DOMAIN=spark.example
SPARK_INGRESS_DOMAIN=sparkapp.example        # 推荐与 SPARK_DOMAIN 不同的注册域；相同也可
SPARK_PUBLIC_BASE_URL=https://spark.example
SPARK_HTTP_ADDR=:8443
SPARK_HTTP_REDIRECT_ADDR=:8080
SPARK_ADMIN_ADDR=127.0.0.1:8081             # 后台只在容器内回环；需要时经 docker exec / 内网反向代理访问
SPARK_SSH_ADDR=:2222
SPARK_SSH_PUBLIC_PORT=22
SPARK_AGENT_ADDR=:9443
SPARK_AGENT_HOSTS=agent.spark.example       # Agent 连接的网关域名或 IP（证书 SAN）
SPARK_TLS_CERT_FILE=/certs/fullchain.pem
SPARK_TLS_KEY_FILE=/certs/privkey.pem
SPARK_SECRET_KEY=                            # openssl rand -base64 32
SPARK_UPSTREAM_SSH_KEY_FILE=/secrets/upstream.key
SPARK_UPSTREAM_SSH_KEY_HOST_PATH=/opt/spark/upstream.key
SPARK_UPLOAD_DIR=/data/uploads
SPARK_SMS_DRIVER=aliyun
SPARK_KYC_DRIVER=aliyun
SPARK_PAY_DRIVER=real
SPARK_PDU_DRIVER=snmp
# 其余阿里云 / 微信 / 支付宝变量见 README 配置表
ALI_KEY=
ALI_SECRET=
```

`deploy/compose/README.md`（部署步骤）：

```markdown
# 控制面 compose 部署

1. 宿主机：把自己的 OpenSSH 挪到别的端口（例如 `/etc/ssh/sshd_config` 里 `Port 2200`，先在新端口验证登录再 `systemctl restart ssh`），22 留给平台 SSH 网关。
2. DNS：`<domain>`、`ssh.<domain>`、`*.<ingress_domain>` 都指向控制面公网地址；`agent.<domain>`（或内网 IP）给 Agent 用。
3. 证书：`docker compose --profile acme up -d acme`，然后
   `docker compose exec acme acme.sh --issue --dns dns_ali -d <domain> -d '*.<ingress_domain>' --install-cert -d <domain> --fullchain-file /certs/fullchain.pem --key-file /certs/privkey.pem`
   （两个域不同时同一张证书同时包含两者）；或把商业证书放进 `certs` 卷的同名文件。platform 每分钟检查文件变化，续签后不用重启。
4. 上游密钥：在任意机器 `platform admin gen-upstream-key --path upstream.key`，放到 `SPARK_UPSTREAM_SSH_KEY_HOST_PATH`（0600）。
5. `cp .env.example .env` 填写；`make platform-image`（或 `docker build -f deploy/platform/Dockerfile -t spark-cloud/platform:latest .`）；`docker compose up -d`。
6. 首次：`docker compose exec platform /platform admin create --username admin`；`docker compose exec platform /platform admin ssh-host-key` 把指纹填进帮助页（`web/shared/src/brand.ts` 的 `sshHostKeyFingerprint`）。
7. 发布顺序：**先升级全部 Agent，再更新 platform 镜像**（旧 Agent 不认识 `host_key_pem`）。platform 重启会断开 SSH 会话（最多等 `SPARK_SSH_DRAIN_TIMEOUT`）。
8. 冒烟：`ssh -p 22 ins-xxxxxxxx@ssh.<domain>` 看到 Banner；`docker compose logs platform | grep 'ssh auth failed'` 里的 `ip` 必须是真实客户端地址而不是 172.x（Docker 的 iptables DNAT 保留源地址；若看到网桥地址，检查 dockerd 是否关闭了 iptables 或启用了 userland-proxy）。
```

`Makefile` 追加：

```make
# platform 容器镜像（前端 + 后端多阶段构建）
platform-image:
	docker build -f deploy/platform/Dockerfile --build-arg VERSION=$(VERSION) -t spark-cloud/platform:$(VERSION) -t spark-cloud/platform:latest .
```

（`.PHONY` 加 `platform-image`。）

- [ ] **Step 2: README**

「部署注意」追加：

```markdown
- 接入层：用户 SSH 经 `ssh.<domain>:22`（平台自研网关，`SPARK_SSH_ADDR`），Web 端口经 `https://ins-<id>-<port>.<ingress_domain>`；DNS 需要 `<domain>`、`ssh.<domain>`、`*.<ingress_domain>` 三条记录都指向控制面。**生产推荐 `SPARK_INGRESS_DOMAIN` 用与 `SPARK_DOMAIN` 不同的注册域（eTLD+1），子域不算**。
- 控制面宿主机自己的 sshd 要先挪到别的端口，22 留给网关（`deploy/compose/README.md`）。
- TLS 两种方式：`SPARK_TLS_CERT_FILE` / `SPARK_TLS_KEY_FILE`（acme.sh DNS-01 侧车或商业泛域名证书，平台每分钟热加载）；或前置反向代理终止 TLS 并 `SPARK_TRUST_PROXY=true`（代理须原样转发 Host，apex 与泛域名都指到 platform）。
- 发布顺序：**先升级全部 Agent，再发布 platform**（新 platform 固定校验平台下发的容器主机密钥，旧 Agent 会自行生成另一把）。platform 重启会断开 SSH 会话（drain 最多 `SPARK_SSH_DRAIN_TIMEOUT`）。
- `platform admin ssh-host-key` 打印网关主机密钥指纹，填进帮助页 `brand.sshHostKeyFingerprint`；`SPARK_SECRET_KEY` 轮换会连带丢失网关主机密钥与实例主机私钥（用户会看到主机密钥变更警告，存量实例首连重新信任）。
- 本地开发测 ingress：`SPARK_PUBLIC_BASE_URL` 默认 `http://spark.local:8080`，在 hosts 加 `spark.local` 与用到的 `ins-<id>-<port>.spark.local`（hosts 不支持泛域名），或用 sslip.io 域（如 `SPARK_DOMAIN=192-168-1-218.sslip.io`）。
- 控制台与后台的写请求有同站防护：经浏览器从其它子域 / 站点提交会得到 `forbidden`；curl 等不带 `Origin` 的客户端不受影响。
```

- [ ] **Step 3: 验证**

Run:
```bash
export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH
go vet ./... && go test ./... 2>&1 | tail -3
scp -r deploy/compose deploy/platform Dockerfile imac@192.168.1.221:/tmp/spark-deploy/ 2>/dev/null || true
ssh imac@192.168.1.221 'cd /tmp/spark-deploy/compose && cp .env.example .env && sed -i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=x/; s#^SPARK_UPSTREAM_SSH_KEY_HOST_PATH=.*#SPARK_UPSTREAM_SSH_KEY_HOST_PATH=/tmp/up.key#" .env && touch /tmp/up.key && docker compose config >/dev/null && echo CONFIG_OK'
```
Expected: `CONFIG_OK`（实跑冒烟在 Task 24 第 18 项）。

- [ ] **Step 4: 提交**

```bash
git add deploy Makefile README.md
git commit -m "deploy: compose stack (platform/postgres/optional acme), platform image, deployment notes for access layer

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 24: 真机检查点 B（192.168.1.221）——Web 入口、后台、TLS、同站、独立域名、compose 冒烟；验收记录

**Files:**
- Modify: `docs/superpowers/acceptance-plan3.md`（补「检查点 B」与总结论）
- 可能修改：验收中发现的缺陷在对应包修复并补单测（单独提交）

**Interfaces:**
- Consumes: 全部任务。前提同 Task 13（平台在 Mac，`SPARK_DOMAIN=192-168-1-218.sslip.io`、`SPARK_PUBLIC_BASE_URL=http://192-168-1-218.sslip.io:8080`，Agent 已是新版本）。Claude 可在测试机 `sudo`，**不得记录密码**。

- [ ] **Step 1: 密码重置（§10.4.4，A 阶段未做）**

开一个 SSH 会话挂着 → 控制台「重置密码」→ 旧密码被拒、新密码通、挂着的会话不断。

- [ ] **Step 2: Web 私有端口（§10.4.7）**

容器内 `pip install jupyterlab`（镜像无 python 时先 `apt-get install -y python3-pip`）→ `jupyter lab --ip=127.0.0.1 --port=8888 --ServerApp.allow_remote_access=True --ServerApp.token=''` → 控制台添加端口 8888 → 浏览器打开 URL → 跳登录 → 回到 Jupyter，新建终端能用（WebSocket）。再起一个不加 `allow_remote_access` 的 Jupyter 在 8889 → 预期 403「Host not allowed」（记录）。换另一个账号访问 → 「无权访问该实例」；隐身窗口未登录 → 登录页；控制台登出后租户页面刷新 2 秒内回到登录页。

- [ ] **Step 3: Web 公开端口（§10.4.8）**

设为公开（看到警告弹窗）→ 隐身窗口直接打开；手机热点（非局域网）打开同一 URL（Mac 需可被公网访问时做，否则用另一台局域网设备）；后台「强制私有」→ 隐身窗口变跳转登录，用户端显示「平台限制为私有」、改公开被拒。

- [ ] **Step 4: 平台重启（§10.4.14）**

重启 platform → 已有 SSH 会话在 30 秒内断开并显示「网关正在重启」；`SELECT count(*) FROM ssh_sessions WHERE ended_at IS NULL` 在 5 分钟内归零（清扫收尾）；浏览器里租户页面刷新不需要重新登录（`sc_ingress` 仍有效）。

- [ ] **Step 5: TLS 热加载（§10.4.15）**

`openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout .devdata/tls.key -out .devdata/tls.crt -days 30 -subj '/CN=192-168-1-218.sslip.io' -addext 'subjectAltName=DNS:192-168-1-218.sslip.io,DNS:*.192-168-1-218.sslip.io'`；平台以 `SPARK_ENV=staging SPARK_HTTP_ADDR=:8443 SPARK_PUBLIC_BASE_URL=https://192-168-1-218.sslip.io:8443 SPARK_TLS_CERT_FILE=… SPARK_TLS_KEY_FILE=… SPARK_HTTP_REDIRECT_ADDR=:8080` 重启；浏览器信任该证书后重做第 2 项；`curl -I http://…:8080/` 得到 301；登录后 DevTools 看到 Cookie 名 `__Host-sc_session`、响应带 HSTS；生成第二张证书覆盖文件 → 1 分钟内 `openssl s_client -connect 192.168.1.218:8443 -servername 192-168-1-218.sslip.io </dev/null | openssl x509 -noout -serial` 变化，平台未重启。

- [ ] **Step 6: 同站防护（§10.4.16）**

在公开端口上放一个 HTML：`<form method=post action="https://192-168-1-218.sslip.io:8443/api/v1/uploads" enctype="multipart/form-data"><input type=file name=file><button>go</button></form>`（容器内 `python3 -m http.server`），已登录控制台的浏览器提交 → 响应 `403 forbidden`；控制台自身上传营业执照正常。

- [ ] **Step 7: 独立域名（§10.4.17）**

平台加 `SPARK_INGRESS_DOMAIN=app-192-168-1-218.sslip.io`（证书 SAN 也加 `*.app-192-168-1-218.sslip.io`）重启 → 端口 URL 落在新域，私有端口的跳转 / 回跳 / Cookie 照常；旧域名的 `ins-…` Host 现在走控制台（404 页面是控制台的 index）。

- [ ] **Step 8: compose 冒烟（§10.4.18）**

测试机上：`make platform-image` 的产物需要 arm64 镜像——在 Spark 上 `docker build -f deploy/platform/Dockerfile -t spark-cloud/platform:test .`（代码经 `git archive` 或 rsync 到 `/tmp/spark-src`），`deploy/compose/.env` 用 staging 配置（自签证书放进 `certs` 卷：`docker run --rm -v spark-deploy_certs:/certs -v /tmp/tls:/src alpine cp /src/tls.crt /certs/fullchain.pem ...`），宿主机 sshd 临时挪到 2200（Claude 执行 `sudo -S sed -i 's/^#\?Port .*/Port 2200/' /etc/ssh/sshd_config && sudo -S systemctl restart ssh`，先用 2200 连通再继续）→ `docker compose up -d platform postgres` → 从 Mac `ssh -p 22 foo@192.168.1.221` 看到 Banner；`docker compose logs platform | grep 'ssh auth failed'` 的 `ip` 是 `192.168.1.218`；`openssl s_client -connect 192.168.1.221:443` 看到证书；替换 `certs` 卷里的证书 → 1 分钟内生效；`docker compose down`；宿主机 sshd 改回 22。

- [ ] **Step 9: 记录、结论、提交**

补齐 `acceptance-plan3.md`：每项命令 / 关键输出 / 结论，缺陷表（编号、现象、原因、修复提交），「观察（列入遗留）」。

```bash
git add docs/superpowers/acceptance-plan3.md
git commit -m "docs: plan 3 real-machine acceptance record (checkpoint B) on 192.168.1.221

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 25: 收尾：遗留清单、计划 2 遗留勾选、README 复核、`make check` 全量

**Files:**
- Create: `docs/superpowers/followups-plan3.md`
- Modify: `docs/superpowers/followups-plan2.md`（`UpdatePorts` 一条按本设计关闭）、`README.md`（如仍有遗漏）

**Interfaces:**
- Consumes: 全部任务（含 Task 13 / 24 验收记录里的缺陷修复提交与裁定）。
- Produces: 遗留清单与上线前核对项；`make check` 全绿是本计划的完成判据。

- [ ] **Step 1: `followups-plan2.md`**

「规格列出的遗留」上方加一行：`- ✅ 计划 3 裁定：实例 Web 端口 `UpdatePorts` 指令不再需要（Web 流量经容器 sshd 的 direct-tcpip 通道，端口只是平台数据）。`

- [ ] **Step 2: `followups-plan3.md`**

```markdown
# 计划 3 遗留事项

来源：计划 3 设计（2026-10-18）§12、评审附录与实施 / 验收过程。均不阻塞合并。

## 上线前需要核对
- 证书来源二选一：acme.sh DNS-01（需 DNS 服务商 API 密钥交给侧车）或商业泛域名证书（人工放进 certs 卷）；证书须同时覆盖 `<domain>` 与 `*.<ingress_domain>`。
- `SPARK_INGRESS_DOMAIN`：生产推荐与 `SPARK_DOMAIN` 不同的注册域（eTLD+1）；启动校验「不是子域」只防误配。
- 控制面宿主机自己的 sshd 先挪到别的端口，compose 把 22 映射给网关。
- 发布顺序：先升级全部 Agent，再发布 platform（旧 Agent 不认识 `host_key_pem`，新 platform 固定校验平台生成的主机密钥）。
- `brand.sshHostKeyFingerprint` 填 `platform admin ssh-host-key` 的指纹并重新构建前端。
- 公开端口是无鉴权的公网 HTTP 服务：使用条款 / 禁止事项由负责人决定（不进代码）；后台处置手段是强制私有、删端口、封禁用户。
- `SPARK_SECRET_KEY` 轮换：除 CA 外还会丢失网关主机密钥与实例主机私钥。
- 后台若经反向代理暴露到兄弟子域，设置 `SPARK_ADMIN_PUBLIC_BASE_URL`，否则浏览器带 `Origin` 的写请求全部被拒。

## 规格列出的遗留（本计划不做）
- X11 转发；FIDO/`sk-*` 公钥；SSH 证书认证；SSH 二次验证。
- platform 内置 ACME；HTTP/3；自定义域名 CNAME 到实例端口。
- 公开端口的限流 / WAF / 基础认证分享链接；按实例来源 IP 白名单；默认禁用公开端口按用户开通。
- 后台「断开某实例全部 SSH 会话」。
- 会话录像；按会话流量计费；SSH 网关 Prometheus 指标。
- 多副本共享的限流计数与票据一次性集合（各副本独立）。
- `direct-tcpip` originator 改写为真实客户端 IP（与「网关不解析通道载荷」冲突）。
- Web 终端 / JupyterLab 一键；原始 TCP/UDP 端口暴露；平台重启不断 SSH 会话。

## 可以继续延后
- `ssh_sessions` 只记录会话级字节数；按时间段统计需另建表。
- ingress 会话校验缓存 2 秒按会话哈希键，登出后最多 2 秒内仍可访问。
- 票据 nonce 集合是进程内的：多副本下同一票据可在另一副本再用一次（60 秒内）。
- `upstream_unreachable` 告警在进程内按实例节流 5 分钟，多副本各自节流。
- sshgw 的失败连接计数 / IP 封禁 / 并发上限都在进程内。
- e2e 的 `C-KEY-05` 用 Node 生成 ed25519 公钥的 wire 格式（SPKI 末 32 字节），若 Node 版本改变 SPKI 编码需调整。

## 实施中确定的产品行为（裁定）
- 暂停（机器失联）不断已有 SSH / Web 会话；到期停机 / 释放 / 换机在 10 秒内断开已有 SSH 会话并提示。
- 用户名不合法与实例不存在显示同一条 Banner；密码错与公钥不匹配对客户端不可区分；实例存在与状态对知道 ID 的人可见（接受）。
- 换机后端口（含 forced_private、备注）与密码登录开关随新实例复制，新实例换新的容器主机密钥。
- 释放 / 开通失败时清空实例主机私钥；存量实例首连信任并写回公钥。
- 后台只读显示密码登录开关，不能替用户改。
（实施中新增的裁定按实际补充。）
```

- [ ] **Step 3: 全量验证**

Run:
```bash
export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH
go vet ./... && go test -race ./... && make check
```
Expected: `go test -race` 全部通过；`make check` = `test`（Go + `web-test` + `install_test.sh`）+ `e2e`（`make web` → `npm run e2e:coverage` → Playwright 三个 project 全部通过，原 109 个用例 + 21 个新用例）。失败项逐个修到绿，不允许 skip。

- [ ] **Step 4: 提交**

```bash
git add docs/superpowers/followups-plan2.md docs/superpowers/followups-plan3.md README.md
git commit -m "docs: plan 3 followups, close UpdatePorts followup, final make check

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
## 自检记录（写计划时完成）

**规格覆盖**（§ → 任务）：§0 代码事实（`init.sh` 缺 sftp、Cookie 属性、`Servers()` 超时、`AdminMigrate` 新 ID、迁移编号）→ Task 1、12、14、18；§1.1 自研 SSH 网关 → Task 9～11；§1.2 自研 Web 入口与文件 TLS → Task 15～18；§1.3 经容器 sshd 通道、无 `UpdatePorts`、网关不自行拨号 → Task 8、10（`TestLocalForwardPassesExtraDataVerbatim`）、12、25；§1.4 裁定 1（`SPARK_INGRESS_DOMAIN`）→ Task 5、15、18、24 第 7 项；裁定 2（证书两种来源）→ Task 18、23；裁定 3（宿主机 sshd 改端口）→ Task 23、24 第 8 项；裁定 4（公开端口默认允许 + 警告 + 强制私有）→ Task 4、19、20、21；裁定 5（暂停不断会话）→ Task 11（`TestScanDisconnectsStoppedButKeepsSuspended`）、13 第 8 步；§2.1 监听 / 主机密钥存库 / CLI / Banner 措辞 → Task 5、6、9；§2.2 认证、按连接计数、IPv6 /64、并发上限、告警 → Task 9（`TestFailureCountingIsPerConnection`、`TestInstanceBruteforceAlertOnce`、`TestSessionCapAndUpstreamUnreachable`）；§2.3 门控表与 10 秒断开 → Task 9（`TestBannerByState`）、11；§2.4 白名单 + 默认拒绝、`hostkeys-00` 丢弃、保活、drain、会话记录与 `last_seen_at` → Task 3、10、11；§3.1 密码重置 / 开关 / 换机复制 → Task 3、19、20；§3.2 公钥（解析矩阵、上限、页面、后台只读、帮助页）→ Task 2、19、20、21；§4.1 Host 解析、路由表、`/.spark/*` → Task 15、17；§4.2 票据跳转全流程、`next` / `target` 校验、逐枚 Cookie、会话绑定、`POST logout`、整页跳转 → Task 16、17、20；§4.2.1 同站防护（`__Host-`、三段规则、后台、支付回调豁免）→ Task 14；§4.3 连接池隔离、超时清除、WebSocket、SSE、502 → Task 17（`TestTenantsDoNotShareConnections`、`TestStreamingNotCutByWriteTimeout`、`TestWebSocketUpgradePassthrough`）；§4.4 端口管理与后台处置 → Task 4、19、20、21；§4.5 TLS 热加载 / 到期告警 / 重定向 / HSTS / prod 校验 → Task 5、18；§4.6 URL 生成与 base URL 校验 → Task 5、16；§5.1 主机密钥平台生成、`HostKeys` 投递、不进 payload、TOFU、发布顺序 → Task 3、8、12、22（payload 断言）、23；§5.2 sshd 配置与 `init.sh` 重写 → Task 12；§5.3 连接池键含 lan_ip、保活 → Task 8；§5.4 proto → Task 12；§6 迁移（含 Down）→ Task 1；§7 错误码与全部接口 → Task 2、19；§8 配置与校验 → Task 5；§9 失败表、告警、审计、日志 → Task 3、9、11、17、18；§10.1 单元 → Task 2～18 各自测试；§10.2 集成 → Task 22 `TestAccessLifecycle`；§10.3 e2e（钩子、host-resolver-rules、覆盖表 ID）→ Task 22；§10.4 真机 18 项 → Task 13（1～6、9～13）、24（4、7、8、14～18）；§11 部署 / README / 帮助页 → Task 20、23；§12 遗留 → Task 25；§13 任务拆分 → 本计划 25 项（检查点 A 独立成 Task 13）。

**占位符扫描**：全文无 TBD / TODO / 「类似 Task N」；每个代码步骤都有代码块；Task 13 第 4 步明确写「在 Task 24 执行」而不是留空；e2e 固定密钥对由 `ssh-keygen` 生成后填入（步骤给出命令）。

**类型一致性**：`rental.AccessInfo{Instance, Password, MachineLanIP, MachineSerial, AgentOnline, RetainUntil}`（Task 3、9、11）；`rental.SSHSessionStart` / `SSHSession` / `OpenSSHSession` / `TouchSSHSessions` / `CloseSSHSession`（Task 3、9、11、19）；`rental.IngressRoute{InstanceID, UserID, Port, Visibility, State, Suspended, LanIP, HostPubkey}`（Task 4、15、17、18）；`rental.Port` / `AddPort(ctx, userID, id, port, visibility, note, ip)` / `UpdatePort(…, visibility, note *string, ip)` / `AdminForcePortPrivate` / `AdminDeletePort` / `PortCounts`（Task 4、19）；`rental.Deps.Keys` / `HostKeyGen`、`app.Options.HostKeyGen`（Task 3、11、22）；`identity.SSHKey` / `ParseSSHPublicKey` / `AddSSHKey(ctx, userID, name, raw, ip)` / `FindSSHKey` / `TouchSSHKey` / `CountSSHKeys` / `SessionHash` / `SessionUserID`（Task 2、9、14、17、19）；`pki.GenerateInstanceHostKey` / `EnsureSSHHostKey` / `SSHFingerprint` / `LoadUpstreamSigner`（Task 3、6、11、22）；`sshtest.New(Options{AuthorizedKey, HostKey})` / `ServeHTTP` / `ForwardAddr` / `OpenToClient` / `LastDirectTCPIP` / `DropAll` / `HostPubLine` / `Signer`（Task 7～10、22）；`upstream.Target` / `Pool.Dial(ctx, Target, port)` / `DialError` / `HostKeyCallback` / `ClientConfig`（Task 8、9、17、18）；`sshgw.Config` / `Deps` / `Store` / `Keys` / `Serve(ctx, lis)` / `ScanNow` / `TouchNow` / `Sessions` 与 Banner 常量（Task 9～11、22）；`ingress.ParseHost` / `Table` / `Signer.Ticket|VerifyTicket|Cookie|VerifyCookie` / `Claims` / `Auth.ParseTarget|PortURL|AuthorizeURL|CallbackURL|ValidCookie|HandleCallback|HandleLogout` / `SafeNext` / `NewServer(auth, routes, sessions, dialer, alerts, clk, log)` / `HostMux`（Task 15～19）；`httpx.SameSiteGuard` / `HSTS`（Task 14、18）；`tlsfile.Loader{CertFile, KeyFile, Interval, Now, OnReloadError, OnExpiring}` / `Load` / `GetCertificate` / `CheckNow` / `CheckExpiry` / `TLSConfig`（Task 18）；`config.Config` 新字段与 `FillAccessDefaults` / `TLSEnabled` / `PublicURL` / `SSHHost`（Task 5、11、18、22）；`api.Server.{PublicBaseURL, Ingress, AuthorizeLimit, SSHHost, SSHPort}`、`adminapi.Server.{PublicBaseURL, PortURL}`（Task 14、16、19）；`agentgw.Deps.HostKeys` / `toProtoCommand(cmd, cfg, hostKeyPEM)` / `push(ctx, …)`（Task 12）；`executor.Command.HostKeyPEM` / `writeSSHDir(id, pub, pem)` / `writeInitScript`（Task 12）；前端 `Http.del`、`SshKey` / `InstancePort` / `AdminSshSession` / `Instance.{password_auth, ssh_host, ssh_port, ports_count}`、`logic/access.ts`、`showPortActions`、`hooks.ssh` / `hooks.ingressUpstream` / `loginConsoleAt` / `CONSOLE_SITE_URL`（Task 20～22）。

**Review Focus 落点**：1 → Task 9 `TestBannerForUnknownUsernames`（大写、空白、`@` 后缀、短 ID、空串）；2 → Task 2 `TestParseSSHPublicKeyMatrix`（CRLF 接受、两行拒绝、私钥拒绝）；3 → Task 15 `TestParseHostNormalizes` + Task 17 `TestPublicPortProxiesAndStripsIngressCookie`（大写 / 尾点 Host 实际命中路由）；4 → Task 10 `TestUpstreamDropEndsSession` + Task 17 `TestUpstreamCloseMidResponseReturns502OrTruncates`；5 → Task 16 `TestTicketSingleUse`、`TestCallbackSetsCookieAndLogoutClearsIt`（`next=//evil.example` 回 `/`）。
