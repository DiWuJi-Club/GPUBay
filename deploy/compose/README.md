# 控制面 compose 部署

platform 镜像以 distroless nonroot（uid/gid 65532）运行，没有 shell。凡是挂进容器的文件（上游私钥、证书、支付密钥）都要让 65532 可读。

1. 宿主机：把自己的 OpenSSH 挪到别的端口（例如 `/etc/ssh/sshd_config` 里 `Port 2200`，先在新端口验证登录再 `systemctl restart ssh`），22 留给平台 SSH 网关。
2. DNS：`<domain>`、`ssh.<domain>`、`*.<ingress_domain>` 都指向控制面公网地址；`agent.<domain>`（或内网 IP）给 Agent 用。生产强烈建议 ingress 域用与 `<domain>` 不同的注册域（或已进 PSL 的域）：租户页面的 JS 能在父域上种 Cookie。
3. 证书：`docker compose --profile acme up -d acme`，然后
   `docker compose exec acme acme.sh --issue --dns dns_ali -d <domain> -d '*.<ingress_domain>' --install-cert -d <domain> --fullchain-file /certs/fullchain.pem --key-file /certs/privkey.pem --reloadcmd 'chown 65532:65532 /certs/fullchain.pem /certs/privkey.pem'`
   （两个域不同时同一张证书同时包含两者；`--reloadcmd` 让 nonroot 的 platform 能读私钥，续签时也会执行）；或用商业泛域名证书：把 `certs` 卷换成宿主机目录（`- /opt/spark/certs:/certs:ro`），放入同名文件并 `chown 65532:65532`。platform 每分钟检查文件变化，续签后不用重启。
4. 上游密钥：`sudo mkdir -p /opt/spark && docker run --rm --user 0 -v /opt/spark:/out spark-cloud/platform:latest admin gen-upstream-key --path /out/upstream.key && sudo chown 65532:65532 /opt/spark/upstream.key*`（文件须 0600，platform 会拒绝权限过宽的私钥）；路径填进 `SPARK_UPSTREAM_SSH_KEY_HOST_PATH`。
5. `cp .env.example .env` 填写（`SPARK_SECRET_KEY`、`AGENT_BIND_IP`、各插件变量）；在仓库根目录 `make platform-image`（或 `docker build -f deploy/platform/Dockerfile -t spark-cloud/platform:latest .`）；回到本目录 `docker compose up -d`。
6. 首次：`docker compose exec platform /platform admin create --username admin`；`docker compose exec platform /platform admin ssh-host-key` 把指纹填进帮助页（`web/shared/src/brand.ts` 的 `sshHostKeyFingerprint`）。
7. 后台：只发布在宿主机 `127.0.0.1:8081`。运维 `ssh -p 2200 -L 8081:127.0.0.1:8081 <宿主机>` 后用浏览器打开 `http://localhost:8081/admin/`（用 `localhost` 而不是 `127.0.0.1`：staging / prod 的 `__Host-` Cookie 需要安全上下文）。
8. 发布顺序：**先升级全部 Agent，再更新 platform 镜像**（旧 Agent 不认识 `host_key_pem`）。platform 重启会断开 SSH 会话（最多等 `SPARK_SSH_DRAIN_TIMEOUT`，默认 30s；compose 的 `stop_grace_period` 为 60s，自己写 systemd unit 时 `TimeoutStopSec=60`）。
9. 监控：探 HTTPS，例如 `https://<domain>/` 或 `https://ins-monitor0-80.<ingress_domain>/.spark/healthz`（返回 `ok`，不查库）。**不要对 22 端口做 TCP 探测**：未认证的连接计入按 IP 的暴力破解限额，探针会把监控机自己的 IP 封掉。
10. 冒烟：`ssh -p 22 ins-xxxxxxxx@ssh.<domain>` 看到 Banner；`docker compose logs platform | grep 'ssh auth failed'` 里的 `ip` 必须是真实客户端地址而不是 172.x（Docker 的 iptables DNAT 保留源地址；若看到网桥地址，检查 dockerd 是否关闭了 iptables 或启用了 userland-proxy）。

本 compose 由 platform 直接终止 TLS，所以 `SPARK_TRUST_PROXY=false`。若改为前置反向代理终止 TLS：去掉 `SPARK_TLS_*` 与 `SPARK_HTTP_REDIRECT_ADDR`，设 `SPARK_TRUST_PROXY=true`（多层代理再设 `SPARK_TRUSTED_PROXY_HOPS=N`，客户端 IP 取 `X-Forwarded-For` 自右数第 N 项），代理须原样转发 Host 头。
