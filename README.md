# Spark Cloud

DGX Spark 算力租赁平台控制面。设计文档见 `docs/superpowers/specs/`，实施计划见 `docs/superpowers/plans/`。

## 本地开发

需要 Go 1.27（本机安装在 `~/.local/go`）。不需要 Docker：测试和本地开发使用嵌入式 PostgreSQL。

```bash
make test         # Go 测试 + 前端单测（首次会下载 PostgreSQL 二进制）
make devdb        # 终端 1：启动本地数据库（端口 5433）
make run          # 终端 2：启动 platform（控制台 http://localhost:8080 ，后台 http://127.0.0.1:8081/admin/ ，托管上次 make web 的产物）
make web-install  # 首次：安装前端依赖（Node 22 / npm 10，本机在 ~/.local/bin）
make web          # 构建前端到 internal/web/dist/{console,admin}（Go embed）
make e2e          # 端到端：构建前端 → 核对覆盖表 → Playwright（自动拉起 cmd/e2e-server，需要本机 Chrome）
make check        # test + e2e
```

开发环境短信为 mock，验证码固定为 `000000`；实名核验 mock 全部通过。

开发环境支付为 mock：充值后调用 `POST /api/v1/dev/recharges/{id}/pay` 模拟支付成功（生产环境不注册该接口）。PDU 为 mock，断电重启只记录调用。

### 管理后台

后台 API 在独立端口（默认 `127.0.0.1:8081`，前缀 `/admin/api/v1`），系统没有默认账号，先用命令行创建第一个超级管理员：

```bash
SPARK_DATABASE_URL=... SPARK_SECRET_KEY=... go run ./cmd/platform admin create --username root --role super
```

按提示输入两次密码（至少 12 位），用验证器 App 扫描终端里的二维码并输入一次动态码完成绑定，然后离线保存打印出的 8 个恢复码。
管理员丢失手机时用 `platform admin reset-totp --username <名称>` 重置，下次登录重新绑定。

### 前端开发

```bash
cd web && npm run dev:console   # http://127.0.0.1:5173 ，/api 代理到 8080
cd web && npm run dev:admin     # http://127.0.0.1:5174/admin/ ，/admin/api 代理到 8081
```

控制台登录：任意手机号 + 验证码 `000000`；后台管理员用 `platform admin create` 创建。
端到端测试全量约 10～15 分钟；只跑子集：`cd web && npx playwright test --project=admin` 或 `-g 'C-RCH'`；调试：`npx playwright test --headed --project=console-mobile -g 'C-RCH-04'`，失败的 trace 用 `cd web && npx playwright show-trace test-results/<目录>/trace.zip`（目录在 `web/test-results/` 下）。
直接运行 `npx playwright test` 不会先构建前端（e2e-server 托管的是 embed 进去的产物），改了页面后先 `make web`。
CI 机器没有 Chrome 时设 `E2E_BROWSER_CHANNEL=chromium` 并 `npx playwright install chromium`。

## 配置（环境变量）

| 变量 | 说明 | 默认 |
|---|---|---|
| `SPARK_ENV` | dev / staging / prod | dev |
| `SPARK_HTTP_ADDR` | 监听地址 | :8080 |
| `SPARK_DATABASE_URL` | PostgreSQL 连接串 | 必填 |
| `SPARK_SECRET_KEY` | 32 字节密钥的 base64，加密身份证号与实例密码 | 必填 |
| `SPARK_DOMAIN` | 对外域名，用于生成 SSH 命令 | spark.local |
| `SPARK_ADMIN_ADDR` | 后台 API 监听地址（只应绑定内网地址） | 127.0.0.1:8081 |
| `SPARK_UPLOAD_DIR` | 上传文件（营业执照）保存目录，需定期备份 | ./.devdata/uploads |
| `SPARK_SMS_DRIVER` / `SPARK_KYC_DRIVER` | mock / aliyun（生产禁止 mock） | mock |
| `SPARK_PAY_DRIVER` | mock / real（生产禁止 mock；real 时启用已配置的微信和 / 或支付宝） | mock |
| `SPARK_PDU_DRIVER` | mock / snmp（生产禁止 mock；snmp 时按每台 PDU 的配置） | mock |
| `SPARK_ALIYUN_ACCESS_KEY_ID` / `SPARK_ALIYUN_ACCESS_KEY_SECRET` | 阿里云 AccessKey（短信与号码百科共用） | 空 |
| `SPARK_SMS_SIGN` / `SPARK_SMS_TPL_LOGIN` / `SPARK_SMS_TPL_EXPIRY` | 短信签名、登录验证码模板（参数 code）、到期提醒模板（参数 instance、hours） | 空 |
| `SPARK_KYC_AUTH_CODE` | 号码百科三要素核验授权码 | 空 |
| `SPARK_WECHAT_MCH_ID` / `_APP_ID` / `_API_V3_KEY` / `_CERT_SERIAL` / `_PRIVATE_KEY_PATH` | 微信支付商户号、AppID、APIv3 密钥、商户证书序列号、商户私钥文件 | 空 |
| `SPARK_WECHAT_PUBLIC_KEY_ID` / `SPARK_WECHAT_PUBLIC_KEY_PATH` | 微信支付公钥 ID 与公钥文件（商户平台「API 安全」中下载） | 空 |
| `SPARK_ALIPAY_APP_ID` / `_PRIVATE_KEY_PATH` / `_PUBLIC_KEY_PATH` / `_SANDBOX` | 支付宝应用 ID、应用私钥文件、支付宝公钥文件、是否沙箱 | 空 / false |
| `SPARK_SWEEP_INTERVAL` | 清扫间隔 | 1m |
| `SPARK_AGENT_ADDR` | Agent 网关 gRPC 监听地址（mTLS，只对机房内网开放） | :9443 |
| `SPARK_AGENT_HOSTS` | 网关证书的域名 / IP（逗号分隔，每项须是主机名或 IP，不含端口），第一项写进安装命令；生产必填 | dev: localhost,127.0.0.1 |
| `SPARK_AGENT_DOWNLOAD_URL` | 内网文件服务器上 `install.sh` 与 `spark-agent-linux-arm64` 所在目录；为空时后台只显示 `spark-agent enroll` 一行 | 空 |
| `SPARK_AGENT_BINARY_SHA256` | agent 二进制 SHA-256（`make agent` 输出），填进安装命令供 install.sh 校验 | 空 |
| `SPARK_UPSTREAM_SSH_KEY_FILE` | 平台登录容器的 ed25519 私钥（`platform admin gen-upstream-key --path ...` 生成，0600）；生产必填，dev 缺省时自动生成在临时目录 | 空 |
| `SPARK_AGENT_DATA_SIZE_GIB` / `SPARK_AGENT_BANDWIDTH_MBPS` | 实例数据盘大小（GiB）与默认限速（Mbps） | 2048 / 200 |
| `SPARK_AGENT_ALLOW_CIDRS` / `SPARK_AGENT_DNS_SERVERS` | 容器可访问的内网白名单（Harbor、HF 镜像站等）与容器 DNS | 空 / 223.5.5.5,119.29.29.29 |
| `SPARK_TRUST_PROXY` | 仅当 platform 部署在反向代理之后才设为 true：用户 API 与租户 Web 入口的客户端 IP 取 X-Forwarded-For（不看 X-Real-IP / True-Client-IP）；后台 API 始终以连接地址为准 | false |
| `SPARK_TRUSTED_PROXY_HOPS` | `SPARK_TRUST_PROXY=true` 时自己的反向代理层数 N（≥ 1）：客户端 IP 取 X-Forwarded-For 自右数第 N 项（每层代理须追加对端地址）；该项缺失或不是合法 IP 时用连接地址 | 1 |
| `SPARK_SSH_ADDR` | SSH 网关监听地址（compose 把公网 22 映射到它） | :2222 |
| `SPARK_SSH_PUBLIC_PORT` | 写进用户 SSH 命令的公网端口；非 22 时命令带 `-p` | 22 |
| `SPARK_SSH_MAX_SESSIONS_PER_INSTANCE` / `SPARK_SSH_DRAIN_TIMEOUT` | 每实例并发 SSH 会话上限 / 停机时等待会话结束的时间 | 16 / 30s |
| `SPARK_UPSTREAM_SSH_PORT` | 容器 sshd 在机器上的端口（Create 指令的 `ssh_port` 与网关上游拨号共用） | 2222 |
| `SPARK_INGRESS_DOMAIN` | 租户 Web 子域的父域（`ins-<id>-<port>.<此域>`）；生产推荐用与 `SPARK_DOMAIN` **不同的注册域（eTLD+1）**，子域不算；「不是 SPARK_DOMAIN 子域」的启动校验只防误配 | = SPARK_DOMAIN |
| `SPARK_PUBLIC_BASE_URL` | 对外地址，支付回调为 `<地址>/api/v1/pay/notify/{wechat,alipay}`（real 支付必填，须 https）；域名必须等于 `SPARK_DOMAIN`；prod 必填且 https；dev 默认 `http://spark.local:8080`。本地测 ingress 需在 hosts 加 `spark.local` 与用到的 `ins-<id>-<port>.spark.local`（hosts 不支持泛域名），或改用 sslip.io 域 | 见左 |
| `SPARK_ADMIN_PUBLIC_BASE_URL` | 后台对外地址（经反向代理暴露时填写），用于后台写请求的 Origin 校验 | 空 |
| `SPARK_TLS_CERT_FILE` / `SPARK_TLS_KEY_FILE` | 证书与私钥（PEM，覆盖 `<domain>` 与 `*.<ingress_domain>`）；设置后公网监听器为 HTTPS 并每分钟检查文件变化热加载 | 空 |
| `SPARK_HTTP_REDIRECT_ADDR` | 纯 HTTP 301 到 https 的监听地址（仅 TLS 开启时有效） | 空 |

## 节点 Agent

每台 DGX Spark（arm64，Ubuntu 24.04，Docker + NVIDIA CDI）跑一个 `spark-agent`，经 9443 端口（mTLS）连接平台。

1. 构建：`make agent`（`make build` 也会构建），生成静态二进制 `bin/spark-agent-linux-arm64` 并打印 SHA-256。
2. 把 `bin/spark-agent-linux-arm64` 与 `deploy/agent/install.sh` 放到内网文件服务器的同一目录；`SPARK_AGENT_DOWNLOAD_URL` 指向该目录，`SPARK_AGENT_BINARY_SHA256` 填 `make agent` 打印的值。
3. 后台机器详情「生成接入命令」，在机器上以 root 执行。脚本依次：下载并校验二进制 -> 用下载的临时二进制执行 `selfcheck`（失败则中止）-> 原子替换 `/usr/local/bin/spark-agent` -> 建 `/etc/spark-agent`、`/var/lib/spark-agent`（强制 0700，可重复执行）-> `enroll` -> 安装并启用 systemd 服务 `spark-agent`（`Restart=always`）。自检失败时令牌未被消耗，可原样重跑；`enroll` 之后的任何失败都需要在后台重新生成接入命令。令牌不会被脚本打印，但 `ps` 里在 enroll 运行期间可见，也会留在 shell 历史和 `sudo` 的 `/var/log/auth.log` 里；一次性令牌用后即失效，过期未用的令牌请在后台作废。
4. 9443 端口只对机房内网开放。
5. 镜像规范与参考 Dockerfile 见 `docs/image-spec.md`、`deploy/image/Dockerfile`。
6. 升级 Agent（**每次发布都先升级全部 Agent，再发布 platform**）：`make agent` 后更新文件服务器上的二进制与 `SPARK_AGENT_BINARY_SHA256`（平台重启生效），在后台重新生成接入命令并在机器上执行（会换新证书，运行中的实例不受影响）。
7. 机器网络：Agent 在宿主机重启后先挂载数据盘再拉起应运行的容器；若容器端口要绑定的局域网地址还没就绪，Agent 推迟启动并每 60 秒重试。业务网卡建议用有线 + 固定地址，并启用 `NetworkManager-wait-online`。
8. 本地无 Docker 时：`spark-agent run --fake --dir ./.devdata/agent --state-dir ./.devdata/agent/state`（`--fake` 仅用于开发，systemd unit 不使用）。
9. 脚本测试：`sh deploy/agent/install_test.sh`（已并入 `make test`）。

## 部署注意

- 启动时自动执行数据库迁移（多副本同时启动时由 PostgreSQL 会话级咨询锁串行化）。
- 迁移 `00007_rental.sql` 会执行 `CREATE EXTENSION IF NOT EXISTS btree_gist`（机器占用时段的排他约束需要）。
  数据库角色需要有创建该扩展的权限；托管数据库上若无此权限，请由管理员预先在目标库执行
  `CREATE EXTENSION btree_gist;`。
- 后台端口不要暴露到公网；如必须经过反向代理，请限制来源 IP。
- `SPARK_UPLOAD_DIR` 存放用户上传的营业执照，需要纳入备份。
- 支付回调地址需在微信支付商户平台、支付宝开放平台中配置为 `SPARK_PUBLIC_BASE_URL` 下的对应路径。
- 用户控制台与用户 API 同在 `:8080`（`/` 为页面，`/api/*` 为接口），前面的反向代理不需要额外路由规则；管理后台与后台 API 同在 `:8081`（`/admin/`），仍只监听内网地址。
- `make build` 会先构建前端再编译后端，二进制内含页面；`index.html` 以 `Cache-Control: no-cache` 返回（浏览器每次加载都向服务器校验，发布后刷新即生效），`assets/` 文件名带内容哈希，缓存一年（`immutable`），其余不带哈希的文件（favicon、`brand/` 等）缓存 1 小时。
- `SPARK_ENV=staging` 与 `prod` 都会给 Cookie 加 `Secure`，所以 staging 必须走 HTTPS。
- `SPARK_SECRET_KEY` 同时加密内置 CA 的私钥：**轮换该密钥会使 CA 不可解密，所有机器都要重新接入**；轮换前先导出 `platform_pki` 表备份。
- 从备份恢复平台数据库前，先把全部机器置为维护或停止平台的 Agent 网关：网关对账会把数据库里不存在的实例容器当作孤儿洗掉。
- 容器化部署：`make platform-image` 构建 `spark-cloud/platform:<version>`（distroless nonroot），`deploy/compose/` 是 platform + postgres（+ 可选 acme.sh 证书侧车）的 compose，步骤见 `deploy/compose/README.md`。
- 接入层：用户 SSH 经 `ssh.<domain>:22`（平台自研网关，`SPARK_SSH_ADDR`），Web 端口经 `https://ins-<id>-<port>.<ingress_domain>`；DNS 需要 `<domain>`、`ssh.<domain>`、`*.<ingress_domain>` 三条记录都指向控制面。**生产推荐 `SPARK_INGRESS_DOMAIN` 用与 `SPARK_DOMAIN` 不同的注册域（eTLD+1），子域不算**（或让 ingress 域进入公共后缀列表 PSL）：租户页面的 JS 能在父域上设置 Cookie。
- 控制面宿主机自己的 sshd 要先挪到别的端口，22 留给网关（`deploy/compose/README.md`）。
- 公网端口（22 / 443 / 80）都是面向互联网、未认证即可连接的服务；9443 只对机房内网开放，后台端口不暴露。
- 不要用裸 TCP 连接对 SSH 端口做健康检查：未认证的连接计入按 IP 的暴力破解限额，会把监控机自己的 IP 封掉；监控请探 HTTPS（如 `https://ins-monitor0-80.<ingress_domain>/.spark/healthz`，返回 `ok`）。
- TLS 两种方式：`SPARK_TLS_CERT_FILE` / `SPARK_TLS_KEY_FILE`（需覆盖 `<domain>` 与 `*.<ingress_domain>` 的泛域名证书：acme.sh DNS-01 侧车或商业证书，平台每分钟热加载文件）；或前置反向代理终止 TLS 并 `SPARK_TRUST_PROXY=true`（代理须原样转发 Host，apex 与泛域名都指到 platform）。客户端 IP 取 `X-Forwarded-For` 自右数第 `SPARK_TRUSTED_PROXY_HOPS` 项（默认 1，多层代理设为层数）。platform 自己终止 TLS 时（默认 compose）保持 `SPARK_TRUST_PROXY=false`。
- 发布顺序：**先升级全部 Agent，再发布 platform**（新 platform 固定校验平台下发的容器主机密钥，旧 Agent 会自行生成另一把）。platform 重启会断开 SSH 会话（drain 最多 `SPARK_SSH_DRAIN_TIMEOUT`，默认 30s）；停机总耗时约 30～40s，compose 的 `stop_grace_period` 与 systemd 的 `TimeoutStopSec` 都设 60s。
- 本次升级把 staging / prod 的会话 Cookie 改名为 `__Host-…`：升级后用户与管理员都需要重新登录一次。
- `platform admin ssh-host-key` 打印网关主机密钥指纹，填进帮助页 `brand.sshHostKeyFingerprint`；`SPARK_SECRET_KEY` 轮换会连带丢失网关主机密钥与实例主机私钥（用户会看到主机密钥变更警告，存量实例首连重新信任）。
- 本地开发测 ingress：`SPARK_PUBLIC_BASE_URL` 默认 `http://spark.local:8080`，在 hosts 加 `spark.local` 与用到的 `ins-<id>-<port>.spark.local`（hosts 不支持泛域名），或用 sslip.io 域（如 `SPARK_DOMAIN=192-168-1-218.sslip.io`）。
- 控制台与后台的写请求有同站防护：经浏览器从其它子域 / 站点提交会得到 `forbidden`；curl 等不带 `Origin` 的客户端不受影响。

## API 约定

- `POST /api/v1/orders` 与 `POST /api/v1/instances/{id}/renew` 支持可选请求头 `Idempotency-Key`（最长 64 字符）。
  同一用户用同一个键重复提交（双击、超时重试）只会扣款一次，并返回首次创建的订单（下单还会返回该实例的当前状态）；
  同一个键被用于不同类型的请求或续费不同实例时返回 409 `idempotency_conflict`。客户端应为每次「新操作」生成新的键（如 UUID）。
- `GET /api/v1/kyc` 返回用户最近一条实名记录（含驳回原因），没有记录时 `record` 为 `null`；个人姓名只保留首字。
- 实例视图含 `product_name`、`image_name`（镜像停用后仍返回名称）；充值单视图含 `remaining_seconds`（服务器计算的剩余支付秒数）。
- 后台 `GET /admin/api/v1/products`、`GET /admin/api/v1/images` 对全部角色可读；`GET /admin/api/v1/audit` 每条含 `actor_name`。
