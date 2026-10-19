# 平台镜像规范

Agent 创建容器时用 `--entrypoint /.spark/init.sh` 覆盖镜像入口：`init.sh` 写一份最小 sshd 配置（`/run/spark-sshd.conf`）并 `exec /usr/sbin/sshd -D -e -f /run/spark-sshd.conf`。
**镜像自身的 `ENTRYPOINT` / `CMD` 不会执行**，镜像里的服务（Jupyter、数据库等）不会自动启动，用户通过 SSH 登录后自行启动。镜像里唯一监听的进程应该只有这个 sshd。

## 必须
- 自带 `/usr/sbin/sshd`（Debian / Ubuntu 装 `openssh-server`）；`root` 用户存在；`/bin/sh` 可用；`/run/sshd` 目录由 `init.sh` 创建。
- 基于带 CUDA 用户态库的镜像，推荐 `nvcr.io/nvidia/cuda:13.0.1-base-ubuntu24.04`（机房内网可达 nvcr.io，不可达 Docker Hub）。容器以 `--gpus all` 启动（宿主机走 CDI），镜像内不装驱动。
- **不要在镜像里写入任何密钥、口令或 `authorized_keys`**。登录只靠平台注入的 `/.spark/authorized_keys`（平台上游公钥）；`PasswordAuthentication` 被关闭。主机密钥由平台按实例生成、Agent 写入 `/.spark/hostkeys/`，镜像里的 `/etc/ssh/ssh_host_*` 不会被使用，而且**镜像不得包含 SSH 主机密钥**（否则同一镜像的所有容器共用同一把私钥）：`openssh-server` 的安装脚本会生成它们，构建时必须 `rm -f /etc/ssh/ssh_host_*`（参考 Dockerfile 已处理）。
- 不依赖 `--ipc=host`、`--privileged`、宿主机网络；容器只额外获得 `IPC_LOCK`、`SYS_NICE`、`SYS_PTRACE`。
- 建议预装 `ca-certificates`、`curl`、`iproute2`。
- **应用镜像（一键部署，计划 7）**另须自带 `curl`、`tar`、`setsid`（`util-linux`）、`python3`、`git`，以及 `modelscope` 命令与 `jupyterlab`；`PIP_INDEX_URL` 作为构建参数并写进镜像环境（脚本若需装小包走它）。平台把应用部署包放进 `/root/data/.spark-apps/<slug>/`，用 `/bin/sh` 执行固定的 `run.sh`，应用以普通进程运行（容器内没有 init / Docker）。模型缓存目录 `/root/data/models`。参考 Dockerfile：`deploy/image/apps/{llm-serving,comfyui,pytorch}`，`LABEL spark.image.spec="2"`。

## 运行时环境（由 Agent 提供）
- `/root/data`：实例数据盘（ext4，大小见后台配置，默认 2TiB），持久化，用户大文件放这里；容器可写层（`/` 下其它路径）不持久，也没有限额，不要往里写大文件。
- `/.spark`（只读）：`authorized_keys`、`hostkeys/`、`init.sh`。
- 内存上限 = 机器内存 - 8GiB（`/sys/fs/cgroup/memory.max`），无 swap；`--shm-size` = 机器内存一半；`pids-limit 32768`。
- 网络：专用网桥 `spark0`，只能访问公网与白名单内网地址（Harbor、HF 镜像站等），不能访问宿主机与机房内网；DNS 由平台下发；没有 IPv6。
- SSH 端口 22 发布到机器内网 IP 的上游 SSH 端口（`SPARK_UPSTREAM_SSH_PORT`，默认 2222）；用户经平台 SSH 网关（`ssh ins-xxxxxxxx@ssh.<domain>`）登录，Web 端口经平台入口访问，都只由控制面持有的上游私钥连入。
- sftp 由 sshd 内置实现（`Subsystem sftp internal-sftp`）提供，镜像**不需要** `openssh-sftp-server`；`scp`（OpenSSH ≥ 9 默认走 SFTP）、`sftp`、`rsync` 都可用。
- 容器主机密钥由平台生成并随开通指令下发（`/.spark/hostkeys/`），平台登录容器时固定校验它；镜像里不得包含 `/etc/ssh/ssh_host_*`。

## 登记
后台「价格与镜像」里登记镜像地址（例如 `harbor.local/spark/cuda-ssh:13.0.1`）；上架或启用时平台会向所有在线机器广播预拉取。

## 参考镜像
`deploy/image/Dockerfile`：`nvcr.io/nvidia/cuda:13.0.1-base-ubuntu24.04` + `openssh-server`（`ca-certificates curl iproute2`），不设 ENTRYPOINT / CMD。在 Spark 上构建：

```bash
docker build -t spark/cuda-ssh:13.0.1 deploy/image
```

应用镜像：`make app-images`（或 `deploy/image/apps/README.md` 的大陆构建命令）。
