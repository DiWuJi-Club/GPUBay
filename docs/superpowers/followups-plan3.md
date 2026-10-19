# 计划 3 遗留事项

来源：计划 3 设计（2026-10-18）§1.4、§12 与评审附录，逐任务审查，以及真机验收（`docs/superpowers/acceptance-plan3.md`）。均不阻塞合并。

## 上线前需要核对
- 证书来源二选一：acme.sh DNS-01（需 DNS 服务商 API 密钥交给侧车）或商业泛域名证书（人工放进 certs 卷）；证书须同时覆盖 `<domain>` 与 `*.<ingress_domain>`。
- DNS：`<domain>`、`ssh.<domain>`、`*.<ingress_domain>` 三条记录都指向控制面。**不要加 AAAA 记录**，除非先给 compose 网络启用 IPv6：否则 IPv6 客户端经 docker-proxy 全部变成同一个源地址，按 IP 的暴力破解限额与限流失效。
- `SPARK_INGRESS_DOMAIN`：生产推荐与 `SPARK_DOMAIN` 不同的注册域（eTLD+1）；启动校验「不是子域」只防误配。同域时租户页面的 JS 仍能给父域写 Cookie（验收观察）。
- 控制面宿主机自己的 sshd 先挪到别的端口，compose 把 22 映射给网关。Ubuntu 22.10+ 是 `ssh.socket` 套接字激活：用 `sshd_config.d` 覆盖 + `daemon-reload` + 重启 `ssh.socket` 才换端口，先双端口验证新端口能登录再去掉 22（验收 B1）。
- 发布顺序：先升级全部 Agent，再发布 platform（旧 Agent 不认识 `host_key_pem`，新 platform 固定校验平台生成的主机密钥）。
- 本次升级把 staging / prod 的会话 Cookie 改名为 `__Host-sc_session` / `__Host-sc_admin`：升级后用户与管理员都要重新登录一次（Task 14）。
- platform 停机超时要大于 SSH drain 30 秒 + HTTP 关闭 10 秒：compose `stop_grace_period: 60s`；自己写 systemd unit 时 `TimeoutStopSec=60`（Task 11、18、23）。
- 监控不要用裸 TCP 连接探 SSH 端口（未认证连接会计入按 IP 的失败次数，监控机会把自己封掉）；改探 `https://ins-monitor0-80.<ingress_domain>/.spark/healthz`（Task 9、23）。
- 前置反向代理终止 TLS 时设 `SPARK_TRUST_PROXY=true`，多层代理把 `SPARK_TRUSTED_PROXY_HOPS` 设为层数（每层须追加对端地址）；platform 自己终止 TLS（默认 compose）时保持 `false`（Task 18）。
- 国内机房构建镜像：用 `GOPROXY` / `NPM_REGISTRY` / `DISTROLESS` 构建参数指向国内镜像，Docker 配置 registry mirror（验收 B2，`deploy/compose/README.md`）。
- `brand.sshHostKeyFingerprint` 填 `platform admin ssh-host-key` 的指纹并重新构建前端。
- VS Code Remote-SSH 的图形界面没有实测（开发 Mac 没装 VS Code）；它依赖的 exec + stdin、sftp、`-L` 已逐项通过，上线前请用图形界面补测一次（验收观察）。
- 公开端口是无鉴权的公网 HTTP 服务：使用条款 / 禁止事项由负责人决定（不进代码）；后台处置手段是强制私有、删端口、封禁用户。
- `SPARK_SECRET_KEY` 轮换：除 CA 外还会丢失网关主机密钥与实例主机私钥。
- 后台若经反向代理暴露到兄弟子域，设置 `SPARK_ADMIN_PUBLIC_BASE_URL`，否则浏览器带 `Origin` 的写请求全部被拒。
- prod 一定要显式设置 `SPARK_PUBLIC_BASE_URL`（https）：代码在为空时会按 `SPARK_HTTP_ADDR` 的端口推导（compose 下会得到 `:8443`，对外是 443），README 写的是「prod 必填」（Task 5 审查）。

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

### SSH 网关与上游
- 存量实例（计划 3 之前开通的）的容器主机密钥是首连信任（TOFU）：首次拨号若被冒充无从发现（规格接受）。可改为经 Agent 的 mTLS 通道取回容器主机公钥再固定（Task 8 审查）。
- 上游连接池的按实例锁 map 只增不减（量很小，Task 8 审查）。
- sshgw 的失败连接计数 / IP 封禁 / 并发上限都在进程内。
- `ssh_sessions` 只记录会话级字节数；按时间段统计需另建表。
- Agent 写主机密钥的日志限流用 `time.Now`（不走注入时钟）；写入中途崩溃会留下临时密钥文件（Task 12 审查）。
- `CountSSHKeys` 在关闭密码登录的事务外检查，并发删公钥时可能出现「没有公钥且密码关闭」，用户可自己重新打开密码登录（Task 3 审查，接受）。

### Web 入口
- 申请把 ingress 域加入公共后缀列表（PSL），作为独立注册域之外的另一种隔离（Task 16 审查）。
- `sc_ingress` Cookie 只签名、不加密：载荷里的 `user_id` 等对用户自己可见（Task 16 审查）。
- 入口的状态页（未登录、已停机、已释放、502 等）没有 CSP 头；`PageReleased` 的兜底文案（Task 15 审查）。
- 入口只删除转发头里的 `X-Forwarded-*` 与 `X-Real-IP`，客户端自带的 `True-Client-IP` 会原样转给租户应用（仅供参考、不用于鉴权，Task 18 审查）。
- 没有覆盖「TLS 下的 WebSocket」的测试（h2 下 WebSocket 客户端走 HTTP/1.1 连接，Task 18 审查）。
- TLS 告警：到期检查每 24 小时一次、提前 14 天告警，续期后 `tls_cert_expiring` 不会自动解除；热加载失败只在连续第 2 次失败时告警一次（`RaiseAlertOnce` 先查后插，多副本可能各报一条）（Task 18 审查）。
- ingress 会话校验缓存 2 秒按会话哈希键，登出后最多 2 秒内仍可访问。
- 票据 nonce 集合是进程内的：多副本下同一票据可在另一副本再用一次（60 秒内）。
- `upstream_unreachable` 告警在进程内按实例节流 5 分钟，多副本各自节流。
- 主域名与入口域名相同时，租户页面的 JS 可给父域写 Cookie（生产 `__Host-` Cookie 不会被覆盖，但可以塞满 Cookie 让控制台请求头超限）；代理只能改写容器响应头，根治靠独立注册域（Task 17 审查、验收观察）。

### 控制台 / 后台 API
- 控制台用户没有二次验证（step-up）：重置实例密码、改端口、删公钥只要登录态；重置已做归属校验 + 审计 + 密码只返回一次（Task 19 审查）。
- 后台 `GET /instances/{id}/ssh-sessions` 不检查实例是否存在，未知 ID 返回空列表而不是 404（Task 19，按计划）。
- `httpx.DecodeJSON` 只解析第一个 JSON 值，请求体末尾的多余数据（如 `{}{}`）不会被拒（审查发现，存量问题）。
- 公钥名称 `stripControl` 只去掉 Unicode Cc 类控制字符，不处理 Cf 类（如双向覆盖字符 U+202E）（Task 2 审查）。
- 后台对已释放实例的端口仍可强制私有 / 删除（有意保留，用于处置，Task 4 审查）。
- 生产 TLS 下 `SPARK_PUBLIC_BASE_URL` 为空时按 https 推导而不报错（见「上线前需要核对」）；`SPARK_HTTP_REDIRECT_ADDR` 不去首尾空白（失败即启动报错）；dev 下 TLS + 显式 http 基址被接受（Task 5 审查）。

### 部署
- `deploy/compose/.env.example` 没有列出全部 `SPARK_*`（如 `SPARK_TRUSTED_PROXY_HOPS`、`SPARK_UPSTREAM_SSH_PORT`、`SPARK_SSH_MAX_SESSIONS_PER_INSTANCE`、Agent 与支付 / 短信相关项），完整列表以 README 配置表为准（Task 25 复核）。
- distroless 镜像里没有 curl，platform 容器没有 compose healthcheck；外部监控探 HTTPS（Task 23）。

### 测试与开发
- e2e 钩子（`/hooks/*`，只在 e2e-server 注册）没有 `Origin` / `Host` 白名单，开发机上理论上可被 DNS rebinding 页面调用（仅开发环境，Task 22 审查）。
- 8200 位 RSA 公钥的拒绝用例生成密钥很慢（Task 2 审查）。
- 若干测试强度问题：迁移测试未断言行数（Task 1）、`Reap` 的接线没有测试（Task 3）、主机密钥未断言库内加密存储且竞态测试是概率性的（Task 6）、`ParseHost` 并发测试没有断言（Task 15）。
- e2e 的 `C-KEY-05` 用 Node 生成 ed25519 公钥的 wire 格式（SPKI 末 32 字节），若 Node 版本改变 SPKI 编码需调整。
- 少量重复代码（`lockOwned` 之外的实例加锁 / 查询片段）未合并（预检 D23）。

## 实施中确定的产品行为（裁定）

### 负责人授权的裁定（规格 §1.4）
- `SPARK_INGRESS_DOMAIN` 可配置（默认等于 `SPARK_DOMAIN`），生产推荐独立注册域：独立域彻底消除同站风险，但不强制，避免上线被域名采购卡住；同站防护无论如何都保留。
- 证书两种来源都支持（acme.sh DNS-01 侧车 / 商业证书）：platform 只按文件热加载，不区分来源，选哪条推迟到部署时。
- 控制面宿主机自己的 sshd 挪离 22，22 映射给网关：用户命令保持无 `-p`，不需要第二个公网地址。
- 公开端口默认允许（设为公开时显示警告），后台可强制设为私有（接口 + 审计）：保持 MVP 行为，处置手段齐备；使用条款不进代码。
- 机器失联（暂停计时）不断开已有 SSH / Web 会话：断会话不会停掉 tmux 里的训练，只会打断用户；上游断了自然结束。

### 执行中的裁定
- 暴力破解只对真正校验过凭据（运行了密码 / 公钥回调）的失败连接计数：裸 TCP 健康检查、对停机 / 暂停 / 开通中实例的 Banner 拒绝都不计，否则 IDE 自动重连和监控会把自己封掉（Task 9；验收 A2，`9c3a4c4`，规格 §2.2 同步修改）。
- 客户端 IP 取 `X-Forwarded-For` 自右数第 N 项（`SPARK_TRUSTED_PROXY_HOPS`，默认 1），只在 `SPARK_TRUST_PROXY=true` 时生效；控制台改用 `httpx.ProxyRealIP`，不再信任 `X-Real-IP` / `True-Client-IP`：最左项可由客户端伪造（Task 18）。
- 后台强制私有 / 删除端口不要求 TOTP 二次验证：不涉及资金、可撤销，规格 §7.2 已定接口（Task 21）。
- 被强制私有的端口在控制台只显示通用的锁定提示，后台填写的原因只进审计：避免把处置细节暴露给滥用者，代价是用户要联系客服才知道原因（Task 20）。
- 长连接 Web 流（WebSocket、SSE 等）在访问权限变化后约 10 秒内断开（路由消失、实例不在运行、端口改为私有后的匿名流、控制台会话失效），与 SSH 的 10 秒规则一致（Task 17）。
- 上游 `Set-Cookie` 的 `Domain` 属性被去掉（变成 host-only），上游下发的 `sc_ingress` 被丢弃：防止租户应用给兄弟实例或控制台种 Cookie；依赖父域 Cookie 的应用不受支持（Task 17）。
- 私有端口上 `Sec-Fetch-Site: same-site` 的非 GET/HEAD/OPTIONS 请求被拒，私有端口的 WebSocket 升级必须同源：入口子域彼此同站，防兄弟租户 CSRF（Task 17）。
- 公开端口的 authorize 直接 302、不发票据；非属主 / 不存在 / 未登记端口一律同一个 403，终态实例 410，属主访问未登记端口 404：不泄露实例是否存在（Task 16）。
- 平台下发的容器主机密钥是权威的：与机器上已有的不同时直接替换；取不到或解密失败时 Create 指令立即失败，瞬时错误按限速日志重试（Task 12）。
- 上游连接池拨号遇到非 `OpenChannelError` 时丢弃旧连接并重拨一次：容器重启后第一个 Web 请求不应 502（Task 8）。
- 上游连接被容器先断开（停机时 Agent 立即停容器）时，网关先查一次实例状态，不可登录则写入断开提示再断开（验收 A1，`9c3a4c4`）。
- 暂停（机器失联）不断已有 SSH / Web 会话；到期停机 / 释放 / 换机在 10 秒内断开已有 SSH 会话并提示。
- 用户名不合法与实例不存在显示同一条 Banner；密码错与公钥不匹配对客户端不可区分；实例存在与状态对知道 ID 的人可见（接受）。
- 换机后端口（含 forced_private、备注）与密码登录开关随新实例复制，新实例换新的容器主机密钥。
- 释放 / 开通失败时清空实例主机私钥；存量实例首连信任并写回公钥。
- 后台只读显示密码登录开关，不能替用户改。
