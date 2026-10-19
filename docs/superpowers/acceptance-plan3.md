# 计划 3 真机验收记录（192.168.1.221）

- 机器：DGX Spark `spark-3479`（GB10），Ubuntu 24.04，Docker 29.6.2，与计划 2 验收同一台。
- 平台：开发 Mac 192.168.1.218，`go run ./cmd/platform`（`SPARK_SSH_ADDR=:2222`、`SPARK_SSH_PUBLIC_PORT=2222`、`SPARK_DOMAIN=192-168-1-218.sslip.io`），开发数据库 `make devdb`。
- 规格：`docs/superpowers/specs/2026-10-18-plan3-access-design.md` §10.4。
- **总结论：§10.4 全部项目在真机通过。** 检查点 A 发现 2 个缺陷（A1、A2），检查点 B 发现 3 个问题（B1～B3）。全部已修复、经审查，并在真机复测通过。剩余观察见文末「观察（列入遗留）」。
- 说明：Mac 上的 DNS 经代理把 `*.sslip.io` 解析为假地址（198.18.x.x），所以 SSH 客户端直接连 `192.168.1.218:2222`，用 `HostKeyAlias=ssh.192-168-1-218.sslip.io` 记录主机密钥，效果与按域名连接相同。测试密钥是临时生成的，未使用个人密钥。

## 检查点 A：SSH 网关与 Agent（§10.4 第 1～6、9～13 项）

- 日期：2026-10-19
- 结论：除第 4 项（密码重置接口在 Task 19 才有，放到检查点 B）外全部通过。发现 2 个缺陷（A1、A2），已修复（`9c3a4c4`，补充 `93232e4`），经审查并在真机复测通过。

### 0. 准备

- `make agent` 产出的 Agent 二进制 SHA-256 为 `308494a085c18c470f3b4d6e291e4c358ecd4068a1782df2a5a586c486d35299`（提交 `1f5d935`）。
- 按「先升级 Agent 再用新平台开通」的顺序操作：后台为机器 1 生成接入命令，在 Spark 上执行 `curl … install.sh | sudo sh -s -- …`。二进制校验和自检全部 ok，接入成功，`machines.agent_version = 1f5d935`。
- 平台网关主机密钥（`platform admin ssh-host-key`）：`SHA256:z9D/krcbX4NmfQPqw44OKBJouAuPUx+4HQJv0FIxxuo`，与 `ssh-keyscan -p 2222` 的结果一致。

### 1. 开通与主机密钥（§10.4.1）——通过

- 用户下单（镜像 `Spark CUDA SSH`，1 天）后，实例 `ins-pc1pb89o` 进入 `running`。
- 容器内 `/.spark/hostkeys/ssh_host_ed25519_key.pub` 与库里的 `instances.ssh_host_pubkey` 完全一致。私钥文件权限为 `-rw------- root`。
- `/.spark/init.sh` 包含以下配置：
  - `Subsystem sftp internal-sftp`、`MaxStartups 50:30:200`
  - `AllowTcpForwarding yes`、`AllowAgentForwarding yes`
  - `X11Forwarding no`、`PermitTunnel no`、`AllowStreamLocalForwarding no`
  - `PasswordAuthentication no`（容器只认平台上游密钥）

### 2. 密码登录（§10.4.2）——通过

- 控制台 `ssh_command` 为 `ssh ins-pc1pb89o@ssh.192-168-1-218.sslip.io`，**还没带 `-p 2222`**。计划里这一项由 Task 19 补上（`ssh_command` 在非 22 端口时输出 `-p`），这里只作记录。
- 首次连接时记录的主机密钥指纹与 `admin ssh-host-key` 一致。用密码登录后执行 `hostname`、`nvidia-smi`（NVIDIA GB10）、`whoami`（root）都正常。
- `ssh_sessions` 新增一行：`auth_method=password`、`client_ip=192.168.1.218`、`ended_at` 非空、`bytes_down=37`。`bytes_up=0` 符合预期：exec 命令没有 stdin 数据，上行由第 5 项的 scp 覆盖。

### 3. 公钥登录与关闭密码（§10.4.3）——通过

公钥页面和接口在 Task 19、20 才有，本项直接写开发库的 `ssh_keys`。

- 免密登录成功，`auth_method=publickey`。
- `ssh-agent` 里装 5 把密钥（4 把无关的加 1 把已登记的），连续连接 3 次都成功，没有触发封禁。
- `password_auth=false` 后，密码被拒，公钥仍可登录。
- 删除公钥记录后，公钥被拒。
- `password_auth=true` 后，密码可以登录。
- `ssh -p 2222 foo@…` 显示 Banner「用户名应为实例 ID，例如 ins-a1b2c3d4」。

### 4. 密码重置（§10.4.4）——检查点 B 执行

### 5. scp / sftp / rsync（§10.4.5）——通过

- 1 GiB 随机文件 `scp` 上传 48 秒（约 21 MB/s），下载 57 秒（约 18 MB/s），`cmp` 一致。
- `sftp` 的 `cd`、`put`、`ls -l`、`get` 都正常，文件一致。
- 参考镜像不带 rsync。在容器里 `apt-get install rsync` 后，`rsync -e ssh -a` 传 50 MB 成功。
- VS Code Remote-SSH 的图形界面没有在本次操作。它走的是 exec 加 direct-tcpip，与本节和第 6 项的 `-L` 相同，已覆盖。检查点 B 由负责人在图形界面补测。

### 6. 转发策略（§10.4.6）——通过

| 操作 | 结果 |
|---|---|
| 容器内 `python3 -m http.server 8000`，`ssh -L 18000:127.0.0.1:8000` 后在 Mac 上 `curl 127.0.0.1:18000` | 返回 `container-hi` |
| Mac 起 `http.server 19000`，`ssh -R 9000:127.0.0.1:19000` 后在容器内 `curl 127.0.0.1:9000` | 返回 `mac-hi` |
| `ssh -A` 后在容器内 `ssh-add -l` | 列出 Mac 上 agent 的密钥 |
| `ssh -X … 'echo $DISPLAY'` | 空 |
| `ssh -L 18001:192.168.1.1:80` 后 `curl -m 3 127.0.0.1:18001` | 失败（000），容器 sshd 拨局域网被 `SPARK-FWD` 阻断 |

### 7. 状态门控（§10.4.9）——修复缺陷 A1 后通过

- 首次测试时保持一个 SSH 会话，把 `expires_at` 改为 1 分钟后。实例 08:33:52.9 停机，会话 08:33:54 断开（约 1 秒），但客户端只看到 `Connection … closed by remote host`，**没有收到断开提示**，记为**缺陷 A1**。
- 新连接显示 Banner「实例已到期停机，数据保留至 2026-10-19 14:33，续费后可继续使用」。续费后可以连接，`/root/data/1g` 仍在。
- 修复后复测（平台重启到 `9c3a4c4`）：
  - 会话中途停机，客户端收到「实例已停机 / 已释放，连接已断开」后断开，停机后约 1 秒。
  - 释放实例时同样收到这条提示，之后新连接显示 Banner「实例已释放」。

### 8. 机器失联（§10.4.10）——通过

- 保持一个会话（每 20 秒输出一次时间），然后 `sudo systemctl stop spark-agent`。
- 约 5 分钟后实例暂停（`suspended_at` 非空），**已有会话一直在输出**（裁定 5）。新连接显示 Banner「实例所在机器暂时失联，计时已暂停，请稍后重试」。
- `sudo systemctl start spark-agent` 后暂停解除，新连接恢复，旧会话仍在。
- 补记（终审 I-1）：本项最初只验证了 SSH 会话与 Web 新请求 503。当时 Web 入口会在暂停后撤销已有 WebSocket / SSE 并丢弃上游连接池，违反裁定 5；终审修复后由单元测试覆盖（`TestWebSocketSurvivesSuspend`、`TestPoolKeepIgnoresSuspend`）。
- 真机复测（终审 I-1 修复后）——通过：实例 `ins-2fa3pbns`，私有端口 8000 上保持一条 SSE 流。12:43:03 停止 `spark-agent`，12:48:58 实例暂停；SSE 流在暂停期间持续接收（一直到第 81 个 tick），0 错误。暂停期间对该端口的新请求返回 503「暂时无法访问」。重新启动 Agent 后实例恢复。

### 9. 隔离未削弱（§10.4.11）——通过

- `iptables -S SPARK-FWD` 与 `iptables -S SPARK-IN` 和 `iptables.ExpectedFwdRules(空白名单)` / `ExpectedInRules()` 逐行相等。
- `ss -lnt`：除 `192.168.1.221:2222`（容器 sshd 发布到局域网 IP，供平台拨入）外，只有系统原有端口（22、631、53，以及 NVIDIA dashboard 的 `127.0.0.1:11000`）。
- Mac 上 `curl -m 3 192.168.1.221:8888` 不通（rc=7）。
- 容器内 `curl -m 3 192.168.1.218:8080` 超时（rc=28，Mac 不在白名单），访问公网 200。

### 10. 暴力破解（§10.4.12）——修复缺陷 A2 后通过

- 每条连接试满 6 次错误密码。加上之前两条在实例停机或暂停时被拒的连接，第 10 条失败连接触发封禁（日志 `ip_blocked=true`）。第 11 条连接在握手前被断开（`Connection closed by 192.168.1.218 port 2222`）。
- 10 分钟后（08:53）自动解封，可以重新连接。
- 告警页没有 `ssh_bruteforce`：单一来源 IP 到不了实例阈值 30 次，实例阈值由 `TestInstanceBruteforceAlertOnce` 覆盖。
- **缺陷 A2**：实例停机、暂停或开通中时，网关只显示 Banner、不校验凭据就拒绝。这类拒绝也计入 IP 失败次数，自动重连的客户端（比如 IDE）会把自己的 IP 封掉。修复后：
  - 只有真正校验了凭据（实例可登录、或实例不存在）的失败才计数。
  - 已关闭密码登录时的密码尝试也不计数，因为根本不会校验密码。
  - 规格 §2.2 已同步改写。
  - 复测：对停机实例连续 15 条连接被拒后，续费再连接正常，没有封禁。

### 11. 重启（§10.4.13）——通过

- `sudo systemctl reboot` 后约 1.5 分钟，Agent 恢复容器，SSH 能连，`/root/data/1g` 仍在。
- 客户端 `known_hosts` 没有报主机密钥变化，平台固定的上游主机密钥也没变。
- 告警页有一条 `upstream_unreachable`（08:41:19），是我在重启期间探测连接、网关拨容器被拒触发的。此时机器尚未判定失联、实例没有暂停，这条告警符合设计。

### 缺陷汇总

| 编号 | 现象 | 修复 |
|---|---|---|
| A1 | 实例停机时 Agent 立即停掉容器，上游连接先于 10 秒状态扫描断开，用户收不到「实例已停机 / 已释放，连接已断开」 | `9c3a4c4`：上游连接异常断开时先保住客户端通道，查一次实例状态，不可登录就写入提示再断开 |
| A2 | 对停机、暂停、开通中实例的拒绝计入 IP 暴力破解次数 | `9c3a4c4`：只对真正校验过凭据的失败计数；规格 §2.2 同步修改 |

## 检查点 B：Web 入口、后台、TLS、同站防护、独立域名、compose 冒烟（§10.4 第 4、7、8、14～18 项）

- 日期：2026-10-19；平台版本 `b09e18d`（其后只改了部署产物与文档）；实例 `ins-kgfu2ti6`。
- 浏览器项用真实 Chrome（Playwright 驱动，`--host-resolver-rules` 把 `*.sslip.io` 指到 Mac，原因同上）。脚本在会话临时目录，不入库。
- 结论：全部通过。发现的问题 B1～B3 均为部署与文档问题，已修复。

### 1. 密码重置（§10.4.4）——通过

- 先用旧密码挂一个 SSH 会话，再调控制台 `POST /instances/{id}/password/reset`（200，新密码 16 位，只在响应里出现一次）。
- 旧密码 `Permission denied`，新密码登录成功；挂着的会话一直在输出，没有断开。
- 审计记录 `instance.password_reset`，`actor_type=user`。

### 2. Web 私有端口（§10.4.7）——通过

- 容器内装 JupyterLab：8888 带 `--ServerApp.allow_remote_access=True`，8889 不带。控制台添加两个私有端口，返回的 URL 为 `http://ins-kgfu2ti6-8888.192-168-1-218.sslip.io:8080/`。
- 未登录访问：跳到 `…/login?redirect=/api/v1/ingress/authorize?target=…`。
- 主人登录后访问：经授权回跳到 `/lab`，200，标题 `JupyterLab`。
  - 在页面里新建终端，经 WebSocket 执行 `echo ws-ok-$((40+2))`，收到 `ws-ok-42`。
- 8889（不带 `allow_remote_access`）：403，Jupyter 自身因 Host 不在允许列表拒绝，符合预期并已记录。
- 另一个账号访问：403「无权访问」。
- 控制台登出后刷新租户页：0.7 秒内回到登录页。
- 加测（容器侧回显请求头，端口 8000）：
  - 容器收到的 Cookie 只有应用自己的 `app=1; evil=1`，控制台 `sc_session` 和 `sc_ingress` 都没有转发。
  - `X-Forwarded-For` 是真实客户端 IP。
  - 容器下发的 `Set-Cookie: evil=1; Domain=192-168-1-218.sslip.io` 被改成只属于该实例域名，控制台页面的 `document.cookie` 为空。

### 3. Web 公开端口（§10.4.8）——通过

- 端口 8000 改为公开后，不带任何 Cookie 访问返回 200。另一台局域网机器（Spark 本机）也能访问，`X-Forwarded-For=192.168.1.221`。
- 公开时的警告弹窗由 Playwright 用例 C-DET（控制台）覆盖，本次没有在图形界面另测。
- 后台「强制私有」（原因必填）后：
  - 匿名访问变成 302 跳授权；
  - 用户的端口列表显示 `forced_private=true`；
  - 用户再改公开返回 409「该端口已被平台限制为私有」；
  - 审计记录 `instance.port_force_private`，`actor_type=admin`。

### 4. 平台重启（§10.4.14）——通过

- 挂着 SSH 会话和一个已登录的 Jupyter 页面，向平台进程发 SIGTERM：
  - Web 长连接立即撤销（日志 `ingress stream revoked … reason=platform shutdown`）；
  - SSH 会话在 drain 满 30 秒时收到「网关正在重启，连接已断开」后断开；
  - 进程 31 秒退出。
- 重启后：租户页重新加载**不需要重新登录**（`sc_ingress` 仍有效），`ssh_sessions` 里 `ended_at IS NULL` 为 0。

### 5. TLS 与热加载（§10.4.15）——通过

- 第一次用 macOS 自带的 LibreSSL `-newkey ec` 生成的证书带显式曲线参数，平台启动时直接报错退出：「加载 TLS 证书（…）: x509: invalid ECDSA parameters」，明确失败，没有降级。改用 `openssl ecparam -name prime256v1` 生成后正常。
- `SPARK_ENV=staging` 加 TLS 启动后：
  - `http://…:8080/market?x=1` 301 到 `https://…:8443/market?x=1`；
  - 实例域名同样 301 到 https；
  - Host 为 `evil.com` 时返回 404，不做跳转；
  - https 响应带 `strict-transport-security: max-age=31536000`。
- https 下重做第 2 项全部通过：Cookie 名为 `__Host-sc_session`，`sc_ingress` 带 `Secure`，WebSocket 终端可用。
- 生成第二张证书覆盖文件：27 秒后对外服务的序列号由 `9D4C23A146E258D0` 变为 `DAD08353DC0F009D`，平台进程没有重启。

### 6. 同站防护（§10.4.16）——通过

- 租户公开端口 8001 上放了一个 multipart 表单，提交到 `https://192-168-1-218.sslip.io:8443/api/v1/uploads`。用已登录控制台的浏览器提交：
  - 请求带 `Sec-Fetch-Site: same-site`、`Origin: https://ins-kgfu2ti6-8001…`；
  - 响应 `403 {"code":"forbidden"}`。
- 控制台自身同源上传：201。

### 7. 独立域名（§10.4.17）——通过

- 加 `SPARK_INGRESS_DOMAIN=app-192-168-1-218.sslip.io`（证书 SAN 含 `*.app-…`）后重启：
  - 端口 URL 全部落在 `ins-…-<port>.app-192-168-1-218.sslip.io`；
  - 私有端口的跳转、回跳、WebSocket、他人 403、登出回登录页都正常；
  - 实例域名下看不到控制台 Cookie。
- 旧域名的 `ins-…` Host 改由控制台处理（返回控制台 index，标题 `Spark Cloud`）。

### 8. compose 冒烟（§10.4.18）——修复 B1～B3 后通过

- **镜像构建**：测试机访问不了 Docker Hub、gcr.io 和 proxy.golang.org。
  - 基础镜像经 `docker.m.daocloud.io` 拉取后改名；distroless 按固定摘要从 `m.daocloud.io/gcr.io/…@sha256:afa5…` 拉到，摘要真实存在。
  - 在 Spark 上 `GOPROXY=https://goproxy.cn,direct NPM_REGISTRY=https://registry.npmmirror.com/ DISTROLESS=… make platform-image VERSION=test` 构建成功，arm64 镜像 24.4 MB。
  - 这三个构建参数是本次发现问题后补的，见 **B2**。
- **宿主机 sshd 挪端口**：测试机是 `ssh.socket` 套接字激活，只改配置重启 `ssh` 不会换端口，见 **B1**。
  - 按「先双端口、新端口验证、再去掉 22」挪到 2200。
  - 结束后同样两步改回 22，并复测 `ssh -p 22` 连通。
  - 留下的 `/etc/ssh/sshd_config.d/10-spark-ports.conf` 内容为 `Port 22`，与默认等价。
- **启动**：staging `.env`（`192-168-1-221.sslip.io`，自签证书放进 `certs` 卷，属主 65532），上游密钥用镜像 `admin gen-upstream-key` 生成。`docker compose up -d platform postgres` 后：
  - postgres 健康检查通过，platform 冷启动迁移成功，各监听正常；
  - 后台「监听在所有网卡上」的提示按 `.env.example` 说明可忽略。
- **对外检查**：
  - 从 Mac `ssh -p 22 foo@192.168.1.221` 显示 Banner「用户名应为实例 ID」，容器日志 `ssh auth failed` 的 `ip` 为 `192.168.1.218`（真实来源，IPv4 发布没有经 docker-proxy 塌缩）；
  - `openssl s_client -connect 192.168.1.221:443` 拿到预期证书；
  - 80 端口 301 到 https；
  - 后台 8081 从局域网连不上，只在宿主机 `127.0.0.1` 可访问（200）；
  - 9443 只监听在 `192.168.1.221`；
  - postgres 没有发布端口。
- **证书热替换**：在 `certs` 卷里原子替换证书后 33 秒生效（序列号 `DC2B84F1…` 变为 `F8EA47C2…`），容器 `RestartCount=0`。
- **停止**：`docker compose down` 正常结束（没有 SSH 会话，不用等 drain）。
- **后台经隧道访问**：在 Chrome 里实测，`http://localhost` 会接受并回送 `Secure` / `__Host-` Cookie，所以 `ssh -L 8081:127.0.0.1:8081` 后用 `http://localhost:8081/admin/` 登录可行。

### 缺陷汇总（检查点 B）

| 编号 | 现象 | 修复 |
|---|---|---|
| B1 | 部署文档让运维改 `sshd_config` 再 `systemctl restart ssh`；Ubuntu 22.10+ 的 `ssh.socket` 套接字激活下端口不会变 | `0c0020f`：文档改为 `sshd_config.d` 覆盖 + `daemon-reload` + 重启 `ssh.socket`，并要求先双端口验证 |
| B2 | 镜像构建写死 proxy.golang.org、npmjs、gcr.io，国内机房无法构建 | `a74d732`：`GOPROXY` / `NPM_REGISTRY` / `DISTROLESS` 构建参数，以及国内网络章节 |
| B3 | 部署 README 的证书签发命令无效、步骤顺序错、IPv6 经 docker-proxy 塌缩成同一来源 IP（Task 23 评审发现，冒烟时确认 IPv4 发布下来源 IP 正确） | `305f16e` |

## 观察（列入遗留）

- VS Code Remote-SSH 的图形界面没有实测：开发 Mac 没装 VS Code。它依赖的 exec 加 stdin、sftp、`-L`/direct-tcpip 已逐项通过，请负责人上线前用图形界面补测一次。
- Playwright 的 `context.cookies(url)` 会把父域的 host-only Cookie 也列出来，容易误判为「控制台 Cookie 发到了实例域名」。以容器实际收到的请求头为准，见第 2 项加测。
- 控制台登录短信码在开发环境有 60 秒限流，连续跑浏览器脚本时需要等待，属于预期行为。
- 主域名与入口域名相同时，租户页面的 JS 仍可给父域写 Cookie（代理只能改写容器响应头）。生产环境请用独立注册域（第 7 项已验证可行），见 README「部署注意」。
