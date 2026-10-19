# 计划 3 真机验收记录（192.168.1.221）

- 机器：DGX Spark `spark-3479`（GB10），Ubuntu 24.04，Docker 29.6.2，与计划 2 验收同一台。
- 平台：开发 Mac 192.168.1.218，`go run ./cmd/platform`（`SPARK_SSH_ADDR=:2222`、`SPARK_SSH_PUBLIC_PORT=2222`、`SPARK_DOMAIN=192-168-1-218.sslip.io`），开发数据库 `make devdb`。
- 规格：`docs/superpowers/specs/2026-10-18-plan3-access-design.md` §10.4。
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
