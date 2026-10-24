# 控制面 compose 部署

platform 镜像以 distroless nonroot（uid/gid 65532）运行，没有 shell。凡是挂进容器的文件（上游私钥、证书、支付密钥）都要让 65532 可读。

需要 Docker Compose ≥ v2.24（acme 服务的 `env_file` 用了带 `required: false` 的长格式）。

1. 宿主机：把自己的 OpenSSH 挪到别的端口，22 留给平台 SSH 网关。Ubuntu 22.10 及以后默认用 `ssh.socket` 套接字激活，只改配置再 `systemctl restart ssh` **不会**换端口，必须 `daemon-reload` 并重启 `ssh.socket`。为防把自己锁在外面，分两步：
   - 先同时监听两个端口：`printf 'Port 22\nPort 2200\n' | sudo tee /etc/ssh/sshd_config.d/10-ports.conf && sudo sshd -t && sudo systemctl daemon-reload && sudo systemctl restart ssh.socket`，另开终端 `ssh -p 2200 <用户>@<宿主机> true` 确认能登录；
   - 再只留新端口：`printf 'Port 2200\n' | sudo tee /etc/ssh/sshd_config.d/10-ports.conf && sudo sshd -t && sudo systemctl daemon-reload && sudo systemctl restart ssh.socket`，`ss -lnt | grep :22` 应为空。
   （未启用 `ssh.socket` 的系统改完配置 `systemctl restart ssh` 即可。）
2. DNS：`<domain>`、`ssh.<domain>`、`*.<ingress_domain>` 都指向控制面公网 **IPv4** 地址（A 记录）；`agent.<domain>`（或内网 IP）给 Agent 用。生产强烈建议 ingress 域用与 `<domain>` 不同的注册域（或已进 PSL 的域）：租户页面的 JS 能在父域上种 Cookie。
   **不要加 AAAA 记录**：compose 只把端口发布在 IPv4（`0.0.0.0:…`）。默认 compose 网络没有 IPv6，IPv6 客户端要经 docker-proxy 转发，到 platform 时全都变成同一个网桥地址，按 IP 的 SSH 暴力破解限额与限流会把所有 IPv6 用户当成一个人（一人触发、全体被封）。确需 IPv6 时先给 compose 网络启用 IPv6（`enable_ipv6: true` + dockerd 的 `ip6tables`），让源地址原样到达，再发布 `[::]` 端口、加 AAAA。
3. 配置：`cp .env.example .env` 填写（`POSTGRES_PASSWORD`、`SPARK_SECRET_KEY`、`AGENT_BIND_IP`、各插件变量）。`POSTGRES_PASSWORD` 只在 `pgdata` 卷首次初始化时生效，之后改它要先在库里 `ALTER ROLE spark PASSWORD ...`。
4. 镜像：在仓库根目录 `make platform-image`（或 `docker build -f deploy/platform/Dockerfile -t spark-cloud/platform:latest .`）。
5. 证书（acme.sh DNS-01）：`cp acme.env.example acme.env` 填阿里云 DNS 的 AccessKey（只给 acme 服务读，不进 platform 容器），`docker compose --profile acme up -d acme`，然后

   ```
   docker compose exec acme acme.sh --issue --server letsencrypt --dns dns_ali -d <domain> -d '*.<ingress_domain>' \
     --fullchain-file /certs/fullchain.pem --key-file /certs/privkey.pem \
     --reloadcmd 'chown 65532:65532 /certs/fullchain.pem /certs/privkey.pem'
   ```

   一张证书同时覆盖 `<domain>` 与 `*.<ingress_domain>`；`--fullchain-file` / `--key-file` / `--reloadcmd` 会记进续签配置，daemon 续签后自动重新写文件并 chown（让 nonroot 的 platform 能读私钥）。想用 ZeroSSL（acme.sh 默认 CA）：先 `docker compose exec acme acme.sh --register-account -m <邮箱> --server zerossl`，再把 `--server letsencrypt` 换成 `--server zerossl`。
   或用商业泛域名证书：把 `certs` 卷换成宿主机目录（`- /opt/spark/certs:/certs:ro`），放入同名文件并 `chown 65532:65532`。platform 每分钟检查文件变化，续签后不用重启。
6. 上游密钥：`sudo mkdir -p /opt/spark && docker run --rm --user 0 -v /opt/spark:/out spark-cloud/platform:latest admin gen-upstream-key --path /out/upstream.key && sudo chown 65532:65532 /opt/spark/upstream.key*`（文件须 0600，platform 会拒绝权限过宽的私钥）；路径填进 `.env` 的 `SPARK_UPSTREAM_SSH_KEY_HOST_PATH`。
7. 支付（`SPARK_PAY_DRIVER=real`）：把微信 / 支付宝的密钥文件放到宿主机目录（例如 `/opt/spark/pay`，`chown -R 65532:65532`），打开 `docker-compose.yml` 里注释掉的 `/secrets/pay` 挂载，`.env` 里的 `*_PATH` 写容器内路径。缺这些时 prod 启动校验不通过。
8. 启动：`docker compose up -d`。
9. 首次：`docker compose exec platform /platform admin create --username admin`；`docker compose exec platform /platform admin ssh-host-key` 把指纹填进帮助页（`web/shared/src/brand.ts` 的 `sshHostKeyFingerprint`）。
10. 后台：只发布在宿主机 `127.0.0.1:8081`。运维 `ssh -p 2200 -L 8081:127.0.0.1:8081 <宿主机>` 后用浏览器打开 `http://localhost:8081/admin/`（用 `localhost` 而不是 `127.0.0.1`：staging / prod 的 `__Host-` Cookie 需要安全上下文）。
11. 发布顺序：**先升级全部 Agent，再更新 platform 镜像**（旧 Agent 不认识 `host_key_pem`）。platform 重启会断开 SSH 会话（最多等 `SPARK_SSH_DRAIN_TIMEOUT`，默认 30s；compose 的 `stop_grace_period` 为 60s；如果自己写 systemd unit 来管理 platform，设 `TimeoutStopSec=60`）。
12. 升级到带 AI 管家（计划 12）的版本：prod 下 `SPARK_BUTLER` 缺省为 `on`，openai 模式没配 `SPARK_LLM_BASE_URL`、或缺 `SPARK_LLM_API_KEY` 时 **platform 拒绝启动**。旧的 `.env` 先加一行 `SPARK_BUTLER=off`（`.env.example` 已带），模型端点就绪后再按 `.env.example` 的注释块配置并改为 `on`、`docker compose up -d` 重建容器。迁移 `00025` 只新增表，可直接滚动。停机时运行中的管家对话会被中断（用户看到「服务正在重启，本轮已中断，请重试」），已确认的写操作会执行完再退出。
13. 监控：探 HTTPS，例如 `https://<domain>/` 或 `https://ins-monitor0-80.<ingress_domain>/.spark/healthz`（返回 `ok`，不查库）。**不要对 22 端口做 TCP 探测**：未认证的连接计入按 IP 的暴力破解限额，探针会把监控机自己的 IP 封掉。
14. 冒烟：`ssh -p 22 ins-xxxxxxxx@ssh.<domain>` 看到 Banner；`docker compose logs platform | grep 'ssh auth failed'` 里的 `ip` 必须是真实客户端地址而不是 172.x（Docker 的 iptables DNAT 保留源地址；若看到网桥地址，检查 dockerd 是否关闭了 iptables 或启用了 userland-proxy，或客户端是否走了 IPv6）。

防火墙：Docker 发布的端口直接写 iptables 的 DOCKER 链，**不受 ufw 控制**；9443 靠绑定内网地址（`AGENT_BIND_IP`）、后台靠绑定 `127.0.0.1` 限制来源，不要指望 ufw 规则。

本 compose 由 platform 直接终止 TLS，所以 `SPARK_TRUST_PROXY=false`。若改为前置反向代理终止 TLS：去掉 `SPARK_TLS_*` 与 `SPARK_HTTP_REDIRECT_ADDR`，设 `SPARK_TRUST_PROXY=true`（多层代理再设 `SPARK_TRUSTED_PROXY_HOPS=N`，客户端 IP 取 `X-Forwarded-For` 自右数第 N 项），代理须原样转发 Host 头。

## 大陆网络

Docker Hub、gcr.io、proxy.golang.org 在大陆机房通常不可达；goproxy.cn、registry.npmmirror.com 与镜像站可用。

- 基础镜像：给 dockerd 配 `registry-mirrors`（`/etc/docker/daemon.json` 里 `{"registry-mirrors": ["https://docker.m.daocloud.io"]}` 后 `systemctl restart docker`，只对 Docker Hub 生效），或按全名从镜像站拉取再打回原标签：

  ```
  docker pull docker.m.daocloud.io/library/postgres:16.15-bookworm && docker tag docker.m.daocloud.io/library/postgres:16.15-bookworm postgres:16.15-bookworm
  docker pull docker.m.daocloud.io/library/node:22.23.1-bookworm-slim && docker tag docker.m.daocloud.io/library/node:22.23.1-bookworm-slim node:22.23.1-bookworm-slim
  docker pull docker.m.daocloud.io/library/golang:1.27.2-bookworm && docker tag docker.m.daocloud.io/library/golang:1.27.2-bookworm golang:1.27.2-bookworm
  docker pull docker.m.daocloud.io/neilpang/acme.sh:3.1.1 && docker tag docker.m.daocloud.io/neilpang/acme.sh:3.1.1 neilpang/acme.sh:3.1.1
  ```

  distroless 在 Dockerfile 里按 digest 引用，`registry-mirrors` 管不到 gcr.io，也不能把镜像打成带 digest 的标签：构建时直接用镜像站全名（digest 不变，内容照样校验），见下一条的 `DISTROLESS`。
- 构建（`DISTROLESS` 的 digest 与 `deploy/platform/Dockerfile` 里的保持一致）：

  ```
  GOPROXY=https://goproxy.cn,direct NPM_REGISTRY=https://registry.npmmirror.com/ \
  DISTROLESS=m.daocloud.io/gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab \
    make platform-image
  ```

  （Go 模块与 npm 依赖走国内源；`go.sum` 与 `package-lock.json` 的校验和照常生效）。
