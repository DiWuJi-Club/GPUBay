# Spark Cloud — 计划 7：AI 应用一键部署设计文档

- 日期：2026-10-20
- 状态：待评审（设计方按负责人授权作出产品裁定，集中列在 §1.3，负责人可事后推翻）
- 上位文档：`2026-10-14-spark-cloud-mvp-design.md`（下称「总设计」）§2 `images`、§4、路线图第 2 项「一键部署模型」、附录竞品调研；`2026-10-18-plan2-agent-design.md`（下称「计划 2」）§3、§4.2、§5.2；`2026-10-18-plan3-access-design.md`（下称「计划 3」）§1.3、§4、§5.3；`2026-10-19-plan6-booking-design.md`（下称「计划 6」）§6、§8
- 范围：后台维护的应用目录（带版本）；用户在开通实例时或在运行中的实例上一键部署应用（vLLM 推理服务、Open WebUI、ComfyUI、JupyterLab）；部署进度、日志、失败处理、重新部署、停止 / 启动、卸载；经计划 3 的 Web 入口暴露并在控制台给出链接；应用镜像在 200 台机器上的分发与缓存；安全边界；后台与控制台页面；测试与真机验收。
- 不在范围：见 §17。

## 0. 现状与约束（实现时不必再查）

**代码事实（`main` 上计划 1～6 已合并，分支 `feat/plan7-deploy`）**
- 镜像目录：`images (id, name, ref, description, enabled, sort)`（`migrations/00005_catalog.sql`），后台 `GET /images`（all）、`POST/PUT /images`（super），`catalog.Service.{ListImages, GetImage, ImageRef, ImageName, CreateImage, UpdateImage}`。上架 / 启用时 `fleet.BroadcastPullImage(ref)` 向所有在线机器入队 `pull_image`（`adminapi/catalog.go`，失败只记日志）。`instances.image_id NOT NULL`，实例视图带 `image_name`。初始种子里的 `registry.spark.internal/...` 地址是占位，真机上目前用的是在机器上 `docker build` 出来的 `spark/cuda-ssh:13.0.1`。
- 容器（计划 2 §4.2，`internal/agent/runtime/docker/docker.go`）：**非 privileged**，Docker 默认能力 + `IPC_LOCK / SYS_NICE / SYS_PTRACE`，`--gpus all`，`--memory = 机器内存 − 8GiB`，`--shm-size` 一半内存，`--pids-limit 32768`，`--entrypoint /.spark/init.sh`（只起 sshd，**镜像的 ENTRYPOINT / CMD 不执行**），`--restart no`，用户是 `root`，数据盘 bind 到 `/root/data`（真机验收时 196G，生产默认 2TiB），可写层在根分区无限额。容器内没有 init / systemd、没有 Docker socket、不能再起容器。任务书里的「整机 privileged 容器」与代码不符，本设计以代码为准：应用必须作为**容器内的普通进程**运行。
- 容器网络（计划 2 §4.4）：可访问公网与 `SPARK_AGENT_ALLOW_CIDRS` 白名单，不可访问宿主机与机房内网；DNS `223.5.5.5 / 119.29.29.29`；默认限速 `SPARK_AGENT_BANDWIDTH_MBPS = 200`。
- 平台到容器的通道（计划 3 §5.3）：`internal/access/upstream.Pool` 对每个实例维持一条共享 `ssh.Client`（`lan_ip:2222`，上游私钥，固定主机密钥），目前只有 `Dial(ctx, Target{InstanceID, LanIP, HostPubkey}, port)` 打 `direct-tcpip` 通道；容器 sshd 配置 `MaxSessions 32`、`AllowTcpForwarding yes`、`Subsystem sftp internal-sftp`。**平台可以经这条连接在容器里 exec 命令**，这是本计划部署执行的通道，不需要 Agent 参与。
- 端口（计划 3 §4.4）：`instance_ports (instance_id, port, visibility, forced_private, note)`，每实例 ≤ 5 个，`rental.AddPort / UpdatePort / DeletePort`，URL `https://ins-<id>-<port>.<ingress_domain>/`，私有端口经票据跳转鉴权，ingress 路由表 2 秒刷新。
- Agent 协议（`api/proto/agent/v1/agent.proto`）：`StateReport { repeated InstanceState instances = 1 }`，每 60 秒一次；`Command` 有 `create / start / stop / wipe / pull`；`pull_image` 单独低优先级队列，超时 60 分钟（`PullCommandTimeout`）。发布顺序约定：**先升级全部 Agent，再发布 platform**。
- 实例状态机：`reserved / creating / running / stopped / wiping / released / failed / cancelled`；`startLease`（`results.go`）在 Create 成功与停机续费 Start 成功时进入 running；`beginWipe`（`sweeper.go`）进入 wiping；`AdminMigrate`（`migrate.go`）新建实例 ID、复制端口与 `password_auth`、**不复制数据盘**；停机（`ExpireDue`）下发 Stop，容器停止后里面的进程全部结束。
- 清扫器：`rental.RunSweeper(ctx, interval, extra...)` 每分钟一轮（咨询锁，附加任务超时 2 分钟）；`sweepEach` 模式。
- 下单：`POST /orders`（`{product_id, duration, image_id}`）与 `POST /bookings`（`{product_id, image_id, group_id?, machine_id?, count, duration, quantity, start_at?, auto_renew?, ...}`）都走 `rental.PlaceBatch`；控制台 `RentDialog.vue` 里镜像是一个 `el-select`。
- 加密：`secretbox`（AES-256-GCM，`SPARK_SECRET_KEY`）用于身份证号、实例密码、主机私钥。
- 后台角色：`super / ops / finance`，路由表 `allRoles / opsRoles / financeRoles / superOnly`；菜单 `web/admin/src/menu.ts`（`menu.spec.ts` 表驱动核对）；「价格与镜像」页 `CatalogView.vue` 为 `superOnly`。
- 控制台：`InstanceDetailView.vue` 卡片顺序：基本信息 → SSH 连接 → Web 访问 → 自动续费 → 动态；`MarketView.vue` + `RentDialog.vue`；类型在 `web/shared/src/types/`，中文映射 `labels.ts`。
- e2e：Playwright 单 worker、假时钟；e2e-server 内嵌 sshgw + `sshtest` 假容器 sshd（`internal/access/sshtest`，exec 目前只回显 `exec:<cmd>`，不真正执行）；钩子 `/hooks/{clock,sweep,agent,machines,groups,users,admins,kyc,ssh,ingress-upstream}`。覆盖表 ID 已到 `C-DET-25`、`C-INS-07`、`C-ING-06`、`C-MKT-08`、`A-INS-13`、`A-CAT-03`、`A-MCH-09`。
- 迁移编号到 `00019_booking.sql`；本计划是 `00020_apps.sql`（带 `+goose Down`）。
- 真机：一台 DGX Spark（192.168.1.221，`spark-3479`，GB10，128GB 统一内存，arm64，Ubuntu 24.04，Docker 29.6.2，CUDA 13 驱动），平台跑在 Mac 192.168.1.218。**机器上 Docker Hub、gcr.io、proxy.golang.org 不可达；nvcr.io 与 docker.m.daocloud.io 可达**；容器内 ports.ubuntu.com 可用（计划 2 勘查），公网 HTTPS 可用。机房没有 Harbor（总设计规划了，尚未搭）。

**竞品（总设计附录）**：「AI 管家一键部署」、ComfyUI 视频工作流、HuggingFace 热门模型库 + AI 匹配。用户视角的核心是：选一个应用、填几个参数、等进度条走完、点链接打开。

## 1. 架构与核心决策

```
 控制台：市场页「预装应用」/ 实例详情「应用」卡片         后台：应用目录（版本、脚本、镜像兼容）、部署排障
                         │                                             │
                         ▼                                             ▼
┌──────────────────────── appdeploy（平台侧部署编排，新包 internal/appdeploy）──────────────────────┐
│  apps / app_versions（后台维护，版本发布后不可变）                                              │
│  app_deployments：queued → installing → running ⇄ stopped；failed；removing → removed；lost      │
│  Runner（每 5 秒一轮，咨询锁，单副本工作）：                                                   │
│    经 upstream.Pool.Exec 在容器里：上传部署包 → setsid 后台执行 run.sh deploy → 轮询 status.json │
│    进度 / 日志尾部回写数据库；健康检查经 Pool.Dial 打 127.0.0.1:<port>                           │
│  与 rental 的衔接：实例 running 时启动 / 恢复应用；stopped 时跳过；wiping / released 时标记 removed  │
│  自动登记 instance_ports（私有），控制台显示链接                                                │
└──────────────────────────────────────────────────────────────────────────────────────────────┘
                         │ SSH（已有上游连接，不改 Agent 指令）
                         ▼
 容器内：/root/data/.spark-apps/<slug>/{run.sh, app.sh, env, status.json, log, service.pid}
         应用进程由 run.sh 以 setsid 启动；模型缓存 /root/data/models（ModelScope / HF 镜像站）
                         ▲
 镜像：每个应用版本钉一组「兼容镜像」（后台 images 表里的条目）；镜像由我们基于 NGC 的 arm64 基础镜像构建、
       推到内网 Harbor；上架 / 发布时预拉取到全部机器；Agent 的 StateReport 新增已缓存镜像列表（唯一的协议改动）
```

### 1.1 决策一览

| # | 决策 | 结论 | 理由 |
|---|---|---|---|
| 1 | 部署在哪里执行 | **在用户实例容器内、作为普通进程执行**，由平台经已有上游 SSH 连接驱动；不做「应用容器」 | 容器非 privileged、没有 Docker socket，容器里起不了容器；Agent 侧 `docker exec` 也能做，但要加指令类型、超时与结果流式回传，而上游 SSH 通道已经有连接池、主机密钥固定与 `direct-tcpip`，exec 只是多一种 session |
| 2 | 应用运行环境从哪来 | **镜像承载运行环境，脚本只做「下模型 + 起服务」**：每个应用版本声明一组兼容镜像（`image_ids`，第一个为默认），开通时选应用会锁定镜像；在已有实例上部署要求实例镜像在兼容列表里 | 在容器里现装 vLLM / PyTorch（arm64 + CUDA 13 的 wheel）既慢又不可靠；NGC 已有 arm64 的 vLLM / PyTorch 容器，构建一次、全机房缓存 |
| 3 | 两种入口 | **开通时预装**（市场页选应用 → 镜像锁定 → 实例 running 后自动部署）与**运行中实例上部署**（实例详情「应用」卡片）都做；两者产生同一种 `app_deployments` 行 | 预装是竞品的「一键」体验；运行中部署让用户在同一实例上加 Open WebUI / JupyterLab |
| 4 | 执行与进度 | 平台上传部署包后，容器内 `run.sh` 以 `setsid` **脱离 SSH 会话后台运行**，把状态写进 `status.json`、输出写进 `log`；平台每 5 秒轮询一次（短 exec），把阶段 / 百分比 / 日志尾部回写数据库 | 模型下载十几分钟到一小时，不能靠一条长 SSH 会话；platform 重启、副本切换后轮询照常继续，容器内的执行不受影响 |
| 5 | Agent 协议 | **只加 `StateReport.images`**（已缓存镜像的 tag 列表），其余不动；旧 Agent 不发该字段也兼容 | 后台要知道「这个应用镜像在 200 台里缓存了多少台」才能放心发布；部署本身不走 Agent |
| 6 | 镜像分发 | 内网 **Harbor**（`registry.spark.internal`，总设计已规划）：`proxy-nvcr` 项目代理 nvcr.io，`spark` 项目放我们构建的应用镜像；机器只从 Harbor 拉；发布应用版本时广播预拉取；后台显示缓存覆盖率。**真机验收阶段没有 Harbor，镜像在 221 上 `docker build` 并以本地 tag 登记**（验收记录注明） | Docker Hub 不可达，200 台各自拉 nvcr.io 既慢又不可控；Harbor 是总设计既定方案；预拉取已有（`pull_image`） |
| 7 | 定价 | **免费**，不单独计费 | 应用只是实例里的进程，算力已经按整机收费；收费要加订单种类、退款分摊，收益不抵复杂度 |
| 8 | 应用模板的可变部分 | 版本 = `spec`（端口、环境变量模式、模型、超时）+ `app.sh`（POSIX sh，三个子命令 `install / start / stop`），**发布后不可变**；用户只能提供 spec 里声明的环境变量，值经类型 / 正则校验后写进容器内 `env` 文件（单引号转义），**从不拼进脚本或命令行** | 脚本固定 + 变量走文件，不存在注入面；版本不可变才能回答「这台实例跑的是哪一版」 |
| 9 | 暴露 | 部署成功时自动在 `instance_ports` 登记应用端口（私有、`note = 应用名`、带 `deployment_id`），控制台链接 = 既有端口 URL；卸载删除该端口 | 沿用计划 3 的鉴权与路由，不新增入口；用户要公开时走现有「设为公开」确认 |
| 10 | 失败处理 | 部署失败（脚本非零退出、超时、健康检查不过）→ `failed`，保留日志尾部 64KB 与错误码，用户可「重新部署」或「卸载」；**不自动重试、不退款、不动实例** | 失败多半是模型名 / 令牌 / 磁盘的问题，用户看日志能自己处理；自动重试会反复下模型 |
| 11 | 实例停机 / 重启后 | 实例回到 running（续费恢复、换机后新实例开通）时，`desired = running` 的部署自动重新执行 `run.sh deploy`；`run.sh` 按 `spec_hash` 的安装标记跳过已完成的安装，只重新起服务 | 容器停了进程就没了，用户期望「续费后应用还在」；幂等的 deploy 比单独维护一条 start 路径简单 |
| 12 | 换机 | `AdminMigrate` 把旧实例的部署以 `queued` 复制到新实例（模型要重新下载，因为数据盘不迁移），旧实例的部署标 `lost` | 换机已是平台过失，不能再让用户手工重装 |
| 13 | 多副本 | Runner 用 `pg_try_advisory_lock` 保证只有一个副本在跑；exec 走该副本自己的 `upstream.Pool` | 与清扫器同一模式；不需要跨副本协调 |

### 1.2 不做的替代方案（评审时不必再提）
- 「应用作为独立容器（docker-compose 风格）」：容器内无 Docker；在宿主机上起第二个容器要打破「每台机器一个实例」的 Agent 假设（计划 2 §4）；放弃。
- 「Agent 新增 `ExecInInstance` 指令 + 结果流式回传」：要改协议、指令超时、结果大小限制，和上游 SSH 重复；放弃。
- 「平台保持一条长 SSH 会话直到部署结束」：platform 重启就丢进度；放弃（改为容器内后台执行 + 轮询）。
- 「在容器里 pip 安装运行环境」：arm64 + CUDA 13 的 wheel 可用性与网络都不可控；放弃（改为镜像承载）。
- 「应用收费 / 按模型收费」：放弃（D1）。
- 「镜像内置 supervisor / systemd」：改容器入口会动计划 2 的 sshd 启动方式与镜像规范；放弃（`run.sh` + `setsid` 够用）。
- 「把 Harbor 搭建纳入本计划」：Harbor 是运维部署事项（compose 里加一个服务），不是平台代码；写进上线前核对，不占任务。

### 1.3 负责人裁定（默认，可推翻）

| # | 裁定 | 理由 |
|---|---|---|
| D1 | 应用部署**免费**，不产生订单与流水 | 见 §1.1 第 7 条 |
| D2 | 每个实例最多同时保留 **3** 个应用（含 failed / stopped，不含 removed / lost）；一个应用在同一实例上只能部署一份 | 每实例端口上限 5（计划 3），留两个给用户自己；一台 GB10 同时跑两个大模型服务也放不下 |
| D3 | 初始应用集 4 个：`vllm`（vLLM OpenAI 兼容推理服务，默认模型 Qwen3-8B）、`open-webui`（对话界面，默认接同实例的 vLLM）、`comfyui`（图像生成，默认模型 SDXL base）、`jupyterlab`；应用镜像 3 个：`spark/llm-serving`（vLLM + Open WebUI + JupyterLab）、`spark/comfyui`（PyTorch + ComfyUI + JupyterLab）、`spark/pytorch`（PyTorch + JupyterLab）；`jupyterlab` 兼容全部三个镜像 | 覆盖「推理 API + 聊天界面 + 文生图 + Notebook」四类需求；把 Open WebUI 装进 vLLM 镜像让「vLLM + WebUI」在一台实例上一键完成 |
| D4 | 模型默认从 **ModelScope** 下载，可选 **HF 镜像站（hf-mirror.com）**；模型缓存在 `/root/data/models/<来源>/<模型 ID>`，**卸载应用不删模型缓存**（控制台提示占用大小与手动删除路径） | 两个源在国内都可达；模型动辄 16GB，用户卸载再装不该重新下载 |
| D5 | 自动登记的端口默认**私有**；用户可设公开（现有二次确认）；Open WebUI 默认开启自身登录 | 与计划 3 的默认一致；公开端口风险已在帮助页说明 |
| D6 | 部署超时按版本 `spec.timeout_minutes`（默认 60，上限 180）；超时 → 停掉脚本、`failed(timeout)` | 16GB 模型在 200Mbps 限速下约 12 分钟，60 分钟留足余量；上限防止版本配错永远挂着 |
| D7 | 部署期间**不临时放开限速**；模型下载速度受实例限速约束，帮助页注明 | 临时改 tc 要新增 Agent 指令，本计划不碰 Agent 的实例指令；列入遗留 |
| D8 | 应用目录读写角色：**super** 创建 / 编辑 / 发布 / 退役版本（与「价格与镜像」一致）；**ops** 可查看目录、查看任意部署的状态与脱敏日志、「强制卸载」（填原因，审计）；finance 只读目录 | 脚本会在用户容器里以 root 执行，发布权限等同发布镜像；ops 处置滥用需要卸载权 |
| D9 | 敏感变量（`type = secret`）在数据库 secretbox 加密、控制台与后台只显示 `***`、不进审计 / 事件；回写数据库的日志尾部用已知敏感值做替换脱敏 | 用户的 HF 令牌、WebUI 密钥不应在后台可见 |
| D10 | 应用版本发布后不可改（spec / 脚本 / 兼容镜像）；要改就发新版本；**已有部署不自动升级**，用户在控制台看到「有新版本」后主动「重新部署」到新版本 | 不可变 + 用户主动，避免平台改脚本把用户正在用的服务改坏 |
| D11 | 版本缓存覆盖率 < 100% 也允许发布（后台显示警告与未缓存机器数）；市场页不按覆盖率隐藏应用；开通时镜像未缓存走现有 20 分钟 `CreateTimeout` | 新版本发布与 200 台预拉取不该互相卡；运维按后台数字决定何时发布 |
| D12 | `StateReport.images` 是本计划唯一的 Agent 协议改动，**旧 Agent 可继续工作**（后台显示「未知」）；发布顺序仍按「先 Agent 后 platform」 | 见 §1.1 第 5 条 |
| D13 | 实例被释放 / 洗机 / 失败 / 取消时部署标 `removed`，不发通知；换机时按 §1.1 第 12 条复制为 `queued` | 数据盘随实例消失，没有什么可保留的 |
| D14 | 应用脚本语言固定 **POSIX sh**（`/bin/sh`），镜像规范新增「必须有 `curl`、`tar`、`setsid`（util-linux）」 | 四个应用镜像都是 Ubuntu 基础，三者都在；不引入 bash 特性让脚本在 `sh -n` 下可静态检查 |
| D15 | 控制台「应用」卡片的进度显示在实例列表卡片上也有一行（「部署中 45%」/「运行中」），但**不发短信**；部署完成 / 失败只在控制台可见 | 与计划 6「开通成功不发短信」一致 |

## 2. 数据模型（迁移 `00020_apps.sql`，带 `+goose Down`）

```sql
CREATE TABLE apps (
    id          BIGSERIAL PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,          -- ^[a-z][a-z0-9-]{1,31}$；容器内目录名与端口备注都用它
    name        TEXT NOT NULL,                 -- 对用户展示，1～32 字符
    summary     TEXT NOT NULL DEFAULT '',      -- ≤ 80 字符，卡片副标题
    description TEXT NOT NULL DEFAULT '',      -- ≤ 4000 字符，Markdown 子集（段落、列表、代码、链接），控制台详情抽屉
    category    TEXT NOT NULL CHECK (category IN ('llm', 'image', 'notebook', 'tool')),
    icon        TEXT NOT NULL DEFAULT 'app',   -- 固定图标名集合（前端映射），不是 URL
    sort        INT  NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'retired')),
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);

CREATE TABLE app_versions (
    id           BIGSERIAL PRIMARY KEY,
    app_id       BIGINT NOT NULL REFERENCES apps (id),
    version      TEXT NOT NULL,                -- ^[0-9A-Za-z][0-9A-Za-z.-]{0,31}$
    status       TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'retired')),
    image_ids    BIGINT[] NOT NULL,            -- 兼容镜像，第一个为默认；每个元素都必须存在于 images
    spec         JSONB NOT NULL,               -- §3.2
    script       TEXT NOT NULL,                -- app.sh（POSIX sh），≤ 65536 字节
    spec_hash    TEXT NOT NULL,                -- sha256(canonical(spec) || "\n" || script) 的十六进制；发布时计算，容器内安装标记用它
    changelog    TEXT NOT NULL DEFAULT '',     -- ≤ 1000 字符
    published_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (app_id, version)
);
CREATE INDEX app_versions_app_idx ON app_versions (app_id, status, published_at DESC);

CREATE TABLE app_deployments (
    id              TEXT PRIMARY KEY,          -- 'dep-' + 12 位 [a-z0-9]
    instance_id     TEXT NOT NULL REFERENCES instances (id),
    user_id         BIGINT NOT NULL REFERENCES users (id),
    app_id          BIGINT NOT NULL REFERENCES apps (id),
    app_version_id  BIGINT NOT NULL REFERENCES app_versions (id),
    state           TEXT NOT NULL CHECK (state IN ('queued', 'installing', 'running', 'stopped', 'failed', 'removing', 'removed', 'lost')),
    desired         TEXT NOT NULL DEFAULT 'running' CHECK (desired IN ('running', 'stopped', 'removed')),
    phase           TEXT NOT NULL DEFAULT '',  -- prepare / download / install / start / health（来自 status.json）
    progress        INT  NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    message         TEXT NOT NULL DEFAULT '',  -- 最近一条可展示信息（≤ 200 字符，已脱敏）
    health          TEXT NOT NULL DEFAULT 'unknown' CHECK (health IN ('unknown', 'ok', 'unhealthy')),
    error_code      TEXT NOT NULL DEFAULT '',  -- script_failed / timeout / health_failed / upload_failed / unreachable / port_conflict / port_limit / instance_gone
    env             JSONB NOT NULL DEFAULT '{}',   -- 非敏感变量 {NAME: value}
    secret_env_enc  BYTEA,                     -- 敏感变量 JSON 的 secretbox 密文；为空表示没有
    port            INT  NOT NULL,             -- 主端口（spec.ports[0]）；登记到 instance_ports
    log_tail        TEXT NOT NULL DEFAULT '',  -- 容器内 log 末尾 ≤ 65536 字节，已脱敏
    status_seq      BIGINT NOT NULL DEFAULT 0, -- 上次读到的 status.json 序号（只增）
    attempt         INT  NOT NULL DEFAULT 0,   -- 第几次 deploy（重新部署 +1）
    spec_hash       TEXT NOT NULL,             -- 冗余自 app_versions，容器内安装标记比对
    next_poll_at    TIMESTAMPTZ NOT NULL,
    last_polled_at  TIMESTAMPTZ,
    cleanup_pending BOOLEAN NOT NULL DEFAULT false, -- 实例停机时卸载：目录留在数据盘，实例恢复后由 Runner 删除
    started_at      TIMESTAMPTZ,               -- 本次 deploy 开始（超时从这里算）
    finished_at     TIMESTAMPTZ,               -- 进入 running / failed / removed 的时刻
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX app_deployments_live_uniq ON app_deployments (instance_id, app_id) WHERE state NOT IN ('removed', 'lost');
CREATE INDEX app_deployments_poll_idx ON app_deployments (next_poll_at) WHERE state IN ('queued', 'installing', 'running', 'stopped', 'removing');
CREATE INDEX app_deployments_instance_idx ON app_deployments (instance_id, created_at);
CREATE INDEX app_deployments_user_idx ON app_deployments (user_id, created_at DESC);

CREATE TABLE app_deploy_events (
    id            BIGSERIAL PRIMARY KEY,
    deployment_id TEXT NOT NULL REFERENCES app_deployments (id),
    at            TIMESTAMPTZ NOT NULL,
    from_state    TEXT NOT NULL,
    to_state      TEXT NOT NULL,
    detail        TEXT NOT NULL DEFAULT ''     -- 可展示文案（已脱敏）
);
CREATE INDEX app_deploy_events_dep_idx ON app_deploy_events (deployment_id, id);

ALTER TABLE instance_ports ADD COLUMN deployment_id TEXT REFERENCES app_deployments (id);   -- 应用自动登记的端口
ALTER TABLE machines ADD COLUMN cached_images TEXT[] NOT NULL DEFAULT '{}', ADD COLUMN cached_images_at TIMESTAMPTZ;
```

说明：
- `apps.status`：`draft` 只在后台可见；`published` 且至少有一个 `published` 版本才出现在控制台；`retired` 不再可新部署，已有部署照常。
- `app_versions.status`：`draft` 可编辑；`published` 不可编辑（§3.4）；`retired` 不可新部署。一个应用的「当前版本」= `published` 中 `published_at` 最新的一条。
- `app_deployments.port` 冗余自 spec，供唯一性检查与卸载时删端口；spec 里的附加端口（§3.2 `ports[1:]`）也登记，`deployment_id` 相同。
- `instance_ports.deployment_id`：卸载时按它删；用户手动删了应用端口 → 控制台链接处显示「端口已删除，重新部署可恢复」；`AdminMigrate` 复制端口时 `deployment_id` 置 NULL（新实例的部署会重新登记，§6.4）。
- `machines.cached_images`：StateReport 上报的 `repository:tag` 列表（≤ 200 项），只用于后台覆盖率展示。
- **Down**：`DROP TABLE app_deploy_events, app_deployments, app_versions, apps` 前先 `ALTER TABLE instance_ports DROP COLUMN deployment_id`、`ALTER TABLE machines DROP COLUMN cached_images, cached_images_at`；容器内的应用进程不受影响（只是平台不再管理），迁移文件头注释写明。

**存量数据**：无；现有实例没有部署；`instance_ports.deployment_id` 全 NULL。

## 3. 应用目录

### 3.1 规则
- `slug` 创建后不可改（容器内目录名）。`name / summary / description / category / icon / sort / status` 可随时改（`app.update` 审计）。
- 删除应用：只允许 `draft` 且没有任何版本；其余用 `retired`。
- 版本号在应用内唯一；不要求 semver，但列表按 `published_at` 排序而不是按字符串。
- 发布版本的前置校验：spec 校验通过（§3.3）、脚本 `sh -n` 通过（平台用 `/bin/sh -n` 静态检查，dev Mac 与 Linux 都有）、`image_ids` 非空且全部 `enabled`、应用的每个 `published` 版本之间 `spec.ports` 可以不同（没有约束）。
- 发布时计算 `spec_hash` 并冻结；`PUT` 对 `published` 版本只允许改 `changelog` 与 `status → retired`。
- 「复制为新版本」：后台从任一版本复制 spec / 脚本 / 镜像到新 `draft`，是改脚本的常规路径（D10）。

### 3.2 版本 `spec`（JSON）

```json
{
  "ports": [
    {"port": 8000, "label": "OpenAI 兼容接口", "path": "/v1", "health_path": "/health"}
  ],
  "timeout_minutes": 60,
  "min_data_free_gib": 40,
  "env": [
    {"name": "MODEL_ID", "label": "模型", "type": "enum", "required": true,
     "options": ["Qwen/Qwen3-8B", "Qwen/Qwen3-4B", "Qwen/Qwen2.5-7B-Instruct"], "default": "Qwen/Qwen3-8B",
     "help": "ModelScope 模型 ID"},
    {"name": "MODEL_SOURCE", "label": "下载源", "type": "enum", "options": ["modelscope", "hf-mirror"], "default": "modelscope"},
    {"name": "MAX_MODEL_LEN", "label": "最大上下文", "type": "int", "min": 1024, "max": 131072, "default": 32768},
    {"name": "API_KEY", "label": "接口密钥", "type": "secret", "required": false, "pattern": "^[A-Za-z0-9_-]{8,128}$",
     "help": "留空则接口不鉴权（仅建议私有端口）"}
  ],
  "links": [{"label": "接口文档", "path": "/docs"}]
}
```

字段约束（`appdeploy.ValidateSpec`）：
- `ports`：1～2 项；`port ∈ [1024, 65535]`，≠ 22、≠ 2222，互不相同；`label` ≤ 32 字符；`path` 以 `/` 开头 ≤ 128 字符；`health_path` 只在第一项上有意义（可空 = 只做 TCP 连通检查）。
- `timeout_minutes`：1～180，默认 60（D6）。`min_data_free_gib`：0～2048，默认 0；部署前 `df /root/data` 剩余不足 → `failed(disk)`（在 `run.sh deploy` 的 prepare 阶段检查，消息带实际剩余）。
- `env`：≤ 16 项；`name ~ ^[A-Z][A-Z0-9_]{0,63}$`，不得以 `SPARK_` 开头（平台保留），不得是 `PATH / LD_PRELOAD / LD_LIBRARY_PATH / HOME / SHELL / IFS / ENV / BASH_ENV`；`type ∈ string | int | bool | enum | secret`；`string / secret` 可带 `pattern`（Go RE2，≤ 256 字符）与 `max_len`（默认 256，上限 1024）；`int` 带 `min / max`；`enum` 的 `options` 1～32 项、每项 ≤ 128 字符；`default` 必须自身通过校验；`required` 的变量没有 `default` 时用户必填；`label` ≤ 32、`help` ≤ 200。
- `links`：≤ 4 项，`path` 规则同上；控制台把它们拼到主端口 URL 后面显示。

### 3.3 用户提供的变量值（`appdeploy.ValidateEnv(spec, values)`）
- 只接受 spec 里声明的名字，多余的键 → `app_env_invalid`。
- 值：UTF-8、不含 `\n`、`\r`、`\0`，长度 ≤ `max_len`；按类型校验（`int` 为十进制整数且在区间；`bool` 为 `true / false`；`enum` 必须在 `options` 里；`pattern` 全匹配）。
- 缺省补 `default`；`required` 缺失 → `app_env_required`（消息带 `label`）。
- 校验后分拆：`type = secret` 的进 `secret_env_enc`（JSON 后 secretbox），其余进 `env`。
- 写入容器的 `env` 文件（0600）格式：每行 `export NAME='value'`，值里的 `'` 替换为 `'\''`；平台注入的保留变量先写：`SPARK_APP=<slug>`、`SPARK_APP_DIR=/root/data/.spark-apps/<slug>`、`SPARK_PORT=<主端口>`、`SPARK_PORT_2=<附加端口或空>`、`SPARK_INSTANCE_ID`、`SPARK_MODELS_DIR=/root/data/models`、`SPARK_SPEC_HASH`、`HF_ENDPOINT=https://hf-mirror.com`、`MODELSCOPE_CACHE=/root/data/models/modelscope`、`HF_HOME=/root/data/models/hf`、`PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple`。

### 3.4 版本不可变与「有新版本」
- 实例视图里每个部署带 `latest_version_id`、`update_available = 当前应用最新 published 版本 ≠ 部署版本`；控制台显示「有新版本 vX，重新部署即可升级」。
- 「重新部署」默认用**当前最新 published 版本**（请求体可指定 `app_version_id`，必须是同一应用的 published 版本）；换版本时 `spec_hash` 变化，容器内安装标记不匹配，`run.sh deploy` 重新执行 `install`。
- 退役版本上的部署照常运行与停启；「重新部署」时若原版本已退役且没有 published 版本 → `app_unavailable`。

## 4. 镜像分发与缓存

### 4.1 镜像来源与构建
- 内网 Harbor（`registry.spark.internal`，compose 里加 `goharbor/harbor-*` 服务是运维事项，写进上线前核对）：
  - 项目 `proxy-nvcr`：代理缓存 `nvcr.io`（Harbor proxy cache），机器拉 `registry.spark.internal/proxy-nvcr/nvidia/vllm:25.09-py3` 等价于拉 nvcr.io；
  - 项目 `spark`：我们构建的应用镜像与基础镜像。
- 应用镜像 Dockerfile 放在仓库 `deploy/image/apps/<name>/Dockerfile`，每个都满足 `docs/image-spec.md`（sshd、删主机密钥、无 ENTRYPOINT）并新增本计划要求：`curl`、`tar`、`setsid`、`python3`、`modelscope` CLI、`jupyterlab`，PyPI 走清华镜像（`ARG PIP_INDEX_URL`）。构建在一台 arm64 机器上执行（可以是进维护的 Spark），`docker build --build-arg BASE=<harbor 或 nvcr 地址>`，推到 Harbor。镜像清单与基础镜像（§9）。
- `SPARK_AGENT_ALLOW_CIDRS` 要含 Harbor 地址（已有机制），容器内不需要访问 Harbor（拉镜像是宿主机的事）。
- 真机验收：没有 Harbor，在 221 上 `docker build -t spark/llm-serving:25.09 ...`（基础镜像直接从 nvcr.io 拉；Open WebUI、ComfyUI 的依赖经 PyPI 镜像与 Gitee / ghproxy 镜像取得，Dockerfile 的 `ARG` 可切换），后台用本地 tag 登记（Docker 对本地已有 tag 的 `pull` 失败只会让 `pull_image` 指令失败，不影响 Create）。

### 4.2 预拉取与覆盖率
- 后台「发布版本」时对 `image_ids` 中每个镜像调用现有 `BroadcastPullImage`；后台应用版本页有「重新预拉取」按钮（ops）。
- Agent：`StateReport` 新增 `repeated string images = 2;`，取 `docker image ls --format '{{.Repository}}:{{.Tag}}' --filter dangling=false`，去掉 `<none>`，最多 200 项；网关 `handleReport` 写 `machines.cached_images / cached_images_at`。fake Agent 支持注入镜像列表（e2e 钩子 `/hooks/agent` 增加 `images` 参数）。
- 覆盖率 = `count(machines WHERE agent_online AND ref = ANY(cached_images))` / `count(agent_online)`；后台应用版本页对每个兼容镜像显示 `已缓存 X / 在线 Y`，缓存时间早于 5 分钟或旧 Agent 没上报的机器计入「未知」。发布不受覆盖率限制（D11）。
- 预拉取状态（`agent_commands` 里的 `pull_image` 结果）后台机器详情已有指令列表，不另做页面。

## 5. 部署机制

### 5.1 容器内布局与部署包
```
/root/data/.spark-apps/<slug>/
  run.sh          平台内置的固定包装脚本（随 platform 二进制发布，embed），本计划的「运行时」
  app.sh          版本脚本：子命令 install / start / stop（stop 可选，缺省按 service.pid 杀进程组）
  spec.json       版本 spec（供 app.sh 读取端口等）
  env             变量文件（0600，§3.3）
  status.json     run.sh 写：{"seq":N,"state":"installing|running|stopped|failed","phase":"download","progress":42,"message":"...","pid":<run.sh 进程组>,"service_pid":<服务>,"spec_hash":"...","exit_code":0,"at":"<ISO8601>"}
  log             run.sh 与 app.sh 的合并输出（append；每次 deploy 开头写分隔行 "==== deploy #N <时间> ===="）
  .installed      安装标记：内容 = spec_hash（install 成功后写）
  service.pid     app.sh start 启动的服务进程 PID（start 子命令必须写）
```
数据盘放在 `/root/data` 下是有意的：可写层没有限额也不持久，模型与日志都该在数据盘上；实例停机再启动后文件仍在。

部署包 = tar（`run.sh`、`app.sh`、`spec.json`、`env`），平台在内存里生成，经一条 exec 上传：
`mkdir -p <dir> && chmod 700 <dir> && tar -x -C <dir> -f - && chmod 600 <dir>/env`（tar 内容走 stdin，≤ 256KB）。目录路径由 `slug` 推导，slug 已限定 `[a-z0-9-]`，命令行里没有任何用户输入。

### 5.2 `run.sh`（平台内置，固定）
子命令：
- `deploy`：前台做 prepare（检查 `df /root/data` ≥ `min_data_free_gib`、`sh -n app.sh`），然后 **`setsid sh run.sh _bg deploy >/dev/null 2>&1 &`** 后立即返回 0（输出新 pid）。`_bg deploy`：写 `status.json{state: installing, phase: prepare}` → 若 `.installed` 内容 ≠ `SPARK_SPEC_HASH` 则 `. ./env; sh app.sh install`（phase `download` / `install` 由 app.sh 通过 `spark_status` 函数汇报，见下）→ 成功写 `.installed` → `sh app.sh start`（必须把服务放后台并写 `service.pid`，自身退出）→ phase `health`：每 5 秒 `curl -sf 127.0.0.1:<port><health_path>`（无 health_path 时用 `/dev/tcp` 连通）直到成功或 `timeout_minutes` 用尽 → `state: running` / `failed(exit_code)`。全程 `set -e`、`trap` 把异常退出写成 `failed`。
- `start`：`deploy` 的别名（安装标记匹配时 deploy 本来就只重启服务），保留只为日志可读。
- `stop`：读 `service.pid` → `kill -TERM -<pgid>`，10 秒后仍在 → `kill -KILL`；同时杀仍在跑的 `_bg`；写 `state: stopped`。
- `status`：把 `status.json` 打印出来，并附一行 `alive=<0|1>`（`service.pid` 进程存在）与 `log_size=<字节>`；平台随后用一条 `tail -c 65536 log` 取日志尾部（两条 exec 合为一条：`sh run.sh status; echo ---; tail -c 65536 log`）。
- `remove`：`stop` → `rm -rf <dir>`（模型缓存不动，D4）→ 退出 0。

`spark_status <phase> <progress> <message>` 是 `run.sh` 导出给 `app.sh` 的函数（通过 `. ./run.sh` 的函数段，或 `run.sh` 把它写成 `spark_status` 可执行文件），原子更新 `status.json`（写临时文件再 `mv`），`seq` 递增。`app.sh` 的模型下载用 `modelscope download --model "$MODEL_ID" --local_dir "$SPARK_MODELS_DIR/modelscope/$MODEL_ID"`，并用一个后台子进程每 10 秒按 `du -s` / 预期大小（spec 的 `env.options` 不带大小；由 app.sh 里的常量表给出）汇报百分比。

幂等：`deploy` 重复执行只会（a）跳过已安装、（b）`stop` 掉旧服务再 `start`；`.installed` 不匹配时重跑 install（新版本）。`remove` 对不存在的目录返回 0。

### 5.3 平台 Runner（`internal/appdeploy`）
- goroutine `Runner.Run(ctx)`，每 `PollInterval = 5s` 一轮；每轮先 `pg_try_advisory_lock(hashtext('appdeploy'))`（会话级，拿不到就跳过本轮，与清扫器一样只有一个副本在工作）。
- 每轮：`SELECT ... FROM app_deployments WHERE next_poll_at <= now AND state IN (...) ORDER BY next_poll_at LIMIT 50 FOR UPDATE SKIP LOCKED`（先无锁选，再逐个独立事务 `FOR UPDATE SKIP LOCKED` 复核，`sweepEach` 模式），每个部署一个步骤，步骤之间不持有数据库事务：**exec 在事务外执行**，结果回写时再开事务并复核 `state / desired / updated_at` 未被用户操作改掉（乐观并发，改掉就丢弃本轮结果）。
- 实例前置：读实例（`state`、`suspended_at`、`machine lan_ip`、`ssh_host_pubkey`）：
  - 实例非 running 或暂停中 → 不 exec，`next_poll_at = now + 30s`（`queued` 的部署就是这样等开通）；
  - 实例 `wiping / released / failed / cancelled` → 直接 `removed(instance_gone)`（rental 的钩子也会做，这是兜底）。
- exec：新增 `upstream.Pool.Exec(ctx, Target, cmd string, stdin []byte, maxOutput int) (stdout, stderr []byte, exit int, err error)`：复用共享 `ssh.Client`，`NewSession` → `Setenv` 不用（容器 sshd 默认不接受）→ `Run(cmd)`；超时 `ExecTimeout = 60s`（上传包）/ `20s`（状态）；输出各截断到 `maxOutput`；连接层错误返回 `*DialError`（与 ingress 共用告警 `upstream_unreachable`）。
- 状态步骤（按 `state` / `desired`）：

| state | desired | 动作 | 结果 |
|---|---|---|---|
| queued | running | 上传部署包 → `sh run.sh deploy` | 成功 → `installing`（`started_at = now`，`attempt + 1`，事件）；上传 / 启动失败 → `failed(upload_failed)`（exec 非零）或保持 queued 且 `next_poll_at + 30s`（`DialError`，连续 10 次后 `failed(unreachable)`） |
| installing | running | `status` 组合命令 | 解析 `status.json`：`seq` 回退或解析失败 → 记日志、不改状态；`installing` → 更新 phase / progress / message / log_tail，`next_poll_at + 5s`；`running` → `running(health=ok)`、登记端口（§7）、`finished_at`、事件；`failed` → `failed(script_failed)`，消息取 `message`；`now − started_at > timeout` → exec `stop`，`failed(timeout)` |
| installing | stopped / removed | exec `stop`（removed 则 `remove`） | → `stopped` / `removed`（用户在部署中途点了停止 / 卸载） |
| running | running | 每 `HealthInterval = 30s` 一次 `status` 组合命令；`alive=1` 且（有 `health_path` 时）平台经 `Pool.Dial` 请求 `GET <health_path>` 2xx | ok → `health=ok`；否则 `health=unhealthy` 并更新 `log_tail`；**不改 state**（§1.1 第 10 条：由用户决定重新部署）；连续 3 次 `alive=0` → `failed(crashed)`，消息「服务进程已退出」 |
| running | stopped | exec `stop` | → `stopped` |
| running / stopped / failed | removed | exec `remove`（`DialError` 时重试到 10 次后仍标 `removed`，日志记「容器不可达，目录未清理」） | → `removed`，删 `deployment_id` 对应端口，事件 |
| stopped | running | exec `deploy`（用户点「启动」或实例恢复 running） | → `installing`（phase 从 `start` 开始，因为 `.installed` 匹配） |
| failed | running（用户点「重新部署」） | 由 API 把 state 置回 `queued` 并清 `error_code / progress / phase`，Runner 走 queued 分支 | — |
| removing | — | 与 `desired = removed` 同 | — |

- 日志尾部：每次 `status` 轮询把 `tail -c 65536` 的内容经**脱敏**（替换所有 secret 值为 `***`，再把平台上游公钥行去掉——它本来也不会出现）写进 `log_tail`；`installing` 期间每 5 秒 64KB 的写入量对 200 台可接受（实际同时部署的实例远少于 200）。
- 事件（`app_deploy_events`）：queued（创建 / 重新部署）、installing、running、stopped、failed（detail = 错误码中文 + message）、removed、lost；控制台「动态」只显示这些。
- 告警：`RaiseAlertOnce(warning, app_deploy_unreachable, machine, instance, "实例 <id> 的应用部署无法连接容器，详情见日志")`（连续 DialError 10 次时）；其它失败是用户可见的，不告警。
- 进程内节流：同一轮对同一实例最多处理 3 个部署（D2 的上限本来就是 3）；每轮总 exec 并发 ≤ 16（`semaphore`），超出的留到下一轮。

### 5.4 与 rental 的衔接（钩子接口）
`rental.Deps` 新增 `Apps AppHooks`（nil 安全）：

```go
type AppHooks interface {
    OnInstanceRunning(ctx, tx, instanceID)                 // startLease：running 时（含首次开通、续费恢复、换机后新实例）
    OnInstanceGone(ctx, tx, instanceID, reason)            // beginWipe / failCreating / 取消 / AdminMigrate 的旧实例：running/stopped/failed/installing/queued → removed 或 lost
    OnInstanceMigrated(ctx, tx, oldID, newID)              // AdminMigrate：旧实例部署 → lost；按 §6.4 复制 queued 到新实例
}
```
- `OnInstanceRunning`：把该实例 `desired = running` 且 `state IN ('running', 'stopped')` 的部署**统一置回 `state = 'queued'`**（phase 清空，`next_poll_at = now`，事件「实例已启动，正在恢复应用」）。容器停过，进程必然不在；走 queued 分支会重新上传部署包（覆盖同名文件，`.installed` 与模型缓存保留）并 `deploy`。`state = 'failed'` 的不动（用户决定）。首次开通（预装）本来就是 queued。同时对 `removed AND cleanup_pending` 的部署把 `next_poll_at = now`，Runner 下一轮 `rm -rf` 其目录后清掉标记（§6.2）。
- `OnInstanceGone`：`UPDATE app_deployments SET state = 'removed', finished_at = now WHERE instance_id = $1 AND state NOT IN ('removed','lost')`，事件 detail = reason（`实例已释放 / 开通失败 / 预订取消`）；对应端口随实例一起失效（路由表只取 running 实例，不删行）。
- 在同一事务内执行，保持 `instance → app_deployments` 的锁顺序（部署行只在这里与 Runner 的回写事务里被锁；Runner 回写先锁部署行不锁实例，两者不构成环：rental 的事务持实例锁后锁部署行，Runner 只锁部署行）。

## 6. 部署生命周期

### 6.1 状态
```
queued ──(Runner 上传+启动)──▶ installing ──(status: running, 健康)──▶ running ⇄ stopped
  ▲                              │                                       │  (用户停止 / 启动)
  │                              ├──(脚本失败 / 超时 / 进程退出)──▶ failed ──(重新部署)──▶ queued
  │                              │                                       │
  └──(实例恢复 running：running / stopped → queued)                       └──(卸载 / 实例消失)──▶ removed（终态）
                                                                 换机：旧实例 → lost（终态），新实例复制为 queued
```
- 用户可见字段：`state`、`phase`、`progress`、`message`、`health`、`error_code`（映射中文）、`url`（主端口 URL，端口存在时）、`links[]`、`update_available`、`version`、`attempt`、时间戳。

### 6.2 用户操作与允许状态

| 操作 | 允许的 state | 行为 |
|---|---|---|
| 部署（新） | 实例 `reserved / creating / running / stopped`（非 running 时建 queued 等待） | 校验 D2 上限、`app_deployments_live_uniq`、镜像兼容、端口不与实例现有端口 / 其它部署冲突、端口总数 ≤ 5（算上将登记的）、变量校验；插入 queued |
| 重新部署 | `running / stopped / failed` | 可更新变量值（整套重新校验）、可指定版本；`state = queued`、`attempt` 不变（Runner +1）、清错误；运行中的服务由 `deploy` 内的 stop 处理 |
| 停止 | `installing / running` | `desired = stopped`，Runner 执行 |
| 启动 | `stopped` | `desired = running`，`state = queued` |
| 卸载 | 任何非终态 | `desired = removed`，`state = removing`；Runner 执行 `remove` 后 `removed`；实例非 running 时（容器停着，目录在数据盘上）→ 直接 `removed` 且 `cleanup_pending = true`，端口立即删除，提示「实例停机中，应用文件会在实例下次启动时清理」；实例恢复 running 后 Runner 对这些目录 `rm -rf` 并清标记（§5.4） |
| 查看日志 | 任何 | 返回 `log_tail` + 从容器实时取一次（实例 running 时，`tail -c 65536`，失败则只返回数据库里的） |

- 所有用户操作要求实例属于该用户（`lockOwned`）；对 `reserved` 实例允许新建 / 卸载 queued 的部署（预装选错了可以取消）。
- 操作限流：每用户每小时 60 次部署类写操作（进程内计数，与现有限流器一致）。

### 6.3 与实例状态的交互

| 实例事件 | 部署行为 |
|---|---|
| 开通成功（creating → running） | `OnInstanceRunning`：queued 的预装开始执行 |
| 到期停机（running → stopped） | Runner 轮询时跳过（容器已停，进程没了）；`health = unknown`，state 不变；控制台显示「实例已停机」 |
| 续费恢复（stopped → running） | `OnInstanceRunning`：running / stopped 的部署回 queued 重新 deploy（§5.4） |
| 机器失联（暂停） | 跳过轮询；已在容器里跑的部署继续跑，恢复后轮询接上（`status.json` 的 `seq` 单调） |
| 释放 / 洗机 / 开通失败 / 取消预订 | `OnInstanceGone` → removed |
| 换机 | §6.4 |
| 用户手动删了应用端口 | 控制台链接处提示，`url` 为空；重新部署时重新登记 |
| 用户把实例密码登录关了 / 改了公钥 | 无关（平台走上游密钥） |
| 用户在容器里自己杀了服务 | 健康检查 `alive = 0` 连续 3 次 → `failed(crashed)`，可重新部署 |
| 用户在容器里 `rm -rf /root/data/.spark-apps` | `status` exec 非零 → 当作 `failed(script_failed)`，消息「状态文件缺失」 |

### 6.4 换机（`AdminMigrate`）
- 旧实例的非终态部署 → `lost`（事件「实例已更换机器」）。
- 新实例：对每个旧部署（`state ∉ removed/lost/failed` 且 `desired ≠ removed`）插入一条 `queued`（同 app / version / env / secret / port、`attempt = 0`，事件「从 <旧实例> 迁移，模型需重新下载」）。`instance_ports` 的复制已有，`deployment_id` 置 NULL，新部署成功后以 `(instance_id, port)` 既有行**认领**（`UPDATE instance_ports SET deployment_id = $dep WHERE instance_id = $new AND port = $port AND deployment_id IS NULL`），不重复插入。
- 复制在 `AdminMigrate` 的事务内通过 `OnInstanceMigrated` 完成；新实例 running 时 `OnInstanceRunning` 不改 queued（它只处理 running / stopped）。

## 7. 暴露与链接
- 部署进入 `running` 时（Runner 回写事务内）对 `spec.ports` 每项：若 `(instance_id, port)` 已存在且 `deployment_id IS NULL` → 认领并把 `note` 改为应用名；已存在且属于别的部署 → 不可能（创建时已查冲突）；不存在 → `INSERT (visibility='private', note=<app.name>, deployment_id)`。端口数超过 5 → 不可能（创建时已算入），兜底 `failed(port_limit)`。
- 控制台 URL = 现有 `rental.PortURL`（`https://ins-<id>-<port>.<ingress_domain>/`），`links[]` = URL + `path`。
- 卸载：`DELETE FROM instance_ports WHERE deployment_id = $1`；审计 `instance.port_delete` 以 user actor 记录，detail「卸载应用 <name>」。
- 后台「强制私有」/「删端口」对应用端口照常生效；删除后部署的 `url` 为空，提示同上。
- `rental.ListIngressRoutes` 与 ingress 不需要改：应用端口只是普通端口。

## 8. 安全
- **脚本固定、变量走文件**：`app.sh` 是后台 super 发布的内容，用户不能改；用户变量只以 `export NAME='…'` 写入 `env`（单引号转义，值不含换行），`run.sh` 用 `. ./env` 加载；exec 的命令行只含 slug 推导的路径。命令注入面为零；变量名白名单（§3.2）防止覆盖 `PATH / LD_PRELOAD` 等影响 `run.sh` 自身。
- **权限边界不变**：脚本在用户容器里以 root 跑，这与用户自己 SSH 进去跑没有区别；容器的能力集、网络隔离、资源上限都不变。平台侧 exec 用的是已有的上游连接，没有新的信任关系。
- **敏感变量**（D9）：secretbox 加密；API 返回 `secret_env: {NAME: true}` 只表示「已设置」；重新部署时不传则沿用、传空串则清除；日志尾部与 `message` 回写前用全部 secret 值做 `strings.ReplaceAll(…, "***")`（值长度 ≥ 8 才替换，更短的 secret 在校验时就拒绝：`secret` 类型最短 8）；事件 detail 不含变量值；审计（后台强制卸载）只记部署 ID。
- **后台脚本发布**：super + 动态码（与「生成接入命令」同级，因为脚本会在所有用户容器里以 root 执行）；审计 `app.version.publish` 记 `spec_hash`。脚本 ≤ 64KB、`sh -n` 通过；不做内容审查（运维自己负责）。
- **健康检查**：平台经 `Pool.Dial` 请求容器内 `127.0.0.1:<port><health_path>`，与 ingress 同一路径，不信任响应内容（只看状态码）；响应体丢弃，≤ 4KB 读取后关闭。
- **资源**：`log_tail` 上限 64KB、`status.json` 解析上限 16KB（超过视为损坏）、部署包 ≤ 256KB；每轮 exec 并发 ≤ 16；每用户限流。
- **公开端口**：自动登记为私有；Open WebUI 的首个访问者注册为管理员——帮助页与应用描述都提醒「部署完成后先自己打开一次」。vLLM 的 `API_KEY` 为空时接口不鉴权，描述里提醒私有端口 + 票据已保护，设公开前先设密钥。
- **模型来源**：ModelScope / hf-mirror 是公网 HTTPS，走容器已有的出网策略；不加白名单也不需要（白名单是给内网地址用的）。
- **`cached_images`**：机器上报的字符串只用于展示，后台页面按文本渲染；每项 ≤ 256 字符、≤ 200 项，超出截断。

## 9. 初始应用集（D3）

| slug | 名称 | 镜像（`images.ref`，Harbor 路径；验收时本地 tag） | 基础镜像 | 端口 | 变量 | 说明 |
|---|---|---|---|---|---|---|
| `vllm` | vLLM 推理服务 | `registry.spark.internal/spark/llm-serving:25.09` | `nvcr.io/nvidia/vllm:25.09-py3`（NGC 的 arm64 vLLM 容器，含 CUDA 13 用户态与 GB10 支持） | 8000（`/health`，链接 `/docs`、`/v1`） | `MODEL_ID`（enum：`Qwen/Qwen3-8B` 默认、`Qwen/Qwen3-4B`、`Qwen/Qwen2.5-7B-Instruct`、`Qwen/Qwen3-14B`）、`MODEL_SOURCE`、`MAX_MODEL_LEN`（默认 32768）、`GPU_MEMORY_UTILIZATION`（string，pattern `^0\.[3-9][0-9]?$`，默认 `0.7`）、`API_KEY`（secret，可空） | `install` = 下模型；`start` = `vllm serve <本地目录> --host 127.0.0.1 --port $SPARK_PORT --served-model-name $MODEL_ID [--api-key]` 放后台写 `service.pid` |
| `open-webui` | Open WebUI 对话界面 | 同上 | 同上（镜像里 `/opt/open-webui` 独立 venv，`pip install open-webui`） | 3000（`/health`） | `OPENAI_API_BASE_URL`（string，默认 `http://127.0.0.1:8000/v1`，pattern 为 http(s) URL）、`OPENAI_API_KEY`（secret，可空）、`WEBUI_SECRET_KEY`（secret，默认由 `app.sh` 首次生成并存到 `$SPARK_APP_DIR/.secret`，用户可不填） | 数据目录 `/root/data/.spark-apps/open-webui/data`；`install` 为空（无模型）；`start` = `open-webui serve --host 127.0.0.1 --port 3000` |
| `comfyui` | ComfyUI 图像生成 | `registry.spark.internal/spark/comfyui:25.09` | `nvcr.io/nvidia/pytorch:25.09-py3` + ComfyUI（Dockerfile 里 `git clone` 固定 commit，`ARG COMFYUI_SRC` 可指向 Gitee 镜像）+ ComfyUI-Manager 不装 | 8188（`/system_stats`） | `MODEL_ID`（enum：`AI-ModelScope/stable-diffusion-xl-base-1.0` 默认、`AI-ModelScope/FLUX.1-schnell`、`Qwen/Qwen-Image`）、`MODEL_SOURCE` | `install` = 下模型并软链到 `ComfyUI/models/checkpoints`（FLUX / Qwen-Image 按其目录约定软链 `unet / clip / vae`）；`start` = `python main.py --listen 127.0.0.1 --port 8188 --output-directory /root/data/comfyui-output` |
| `jupyterlab` | JupyterLab | 三个镜像都兼容（`image_ids = [llm-serving, comfyui, pytorch]`） | — | 8888（`/api`） | `JUPYTER_TOKEN`（secret，**用户必填**，前端「生成」按钮填 32 位随机值；不由脚本生成，因为 `message` 会被脱敏、无法回显） | `start` = `jupyter lab --ip 127.0.0.1 --port 8888 --no-browser --allow-root --notebook-dir /root/data --IdentityProvider.token=$JUPYTER_TOKEN --ServerApp.allow_remote_access=True` |

第三个镜像 `registry.spark.internal/spark/pytorch:25.09`（`nvcr.io/nvidia/pytorch:25.09-py3` + sshd + jupyterlab）替换 `images` 种子里的 `PyTorch` 占位条目；`vLLM` 占位条目改指 `llm-serving`。镜像登记（`images` 表）由后台在验收 / 上线时手工完成，不写进迁移（种子地址是占位，和现在一样）。

模型大小常量（`app.sh` 内进度估算用）：Qwen3-4B 8GB、Qwen3-8B 16GB、Qwen2.5-7B 15GB、Qwen3-14B 29GB、SDXL base 7GB、FLUX.1-schnell 24GB、Qwen-Image 41GB。`min_data_free_gib`：vllm 40、comfyui 50、其余 2。超时：vllm 90、comfyui 90、open-webui 15、jupyterlab 10。

NGC 标签 `25.09` 以实施时能拉到的最近版本为准（实施任务 2 核实并把实际标签写进 Dockerfile 与验收记录）；若 NGC 的 vLLM 容器不含 GB10 所需的 CUDA 13 构建，退路是 `nvcr.io/nvidia/pytorch` + `pip install vllm`（NGC PyTorch 容器自带 arm64 CUDA 13 的 torch），在 Dockerfile 里切换。

## 10. API

错误码（`apperr`）新增：`app_not_found`（404「应用不存在或未上架」）、`app_unavailable`（409「该应用当前没有可用版本」）、`app_image_mismatch`（409「该应用需要以下镜像之一：<名称列表>，当前实例镜像为 <名称>」）、`app_limit`（409「每个实例最多部署 3 个应用」）、`app_exists`（409「该应用已在此实例上部署」）、`app_port_conflict`（409「应用端口 <port> 已被该实例的其它端口占用」）、`app_env_invalid`（400，消息带变量 label 与原因）、`app_env_required`（400「请填写 <label>」）、`app_invalid_state`（409「当前状态不允许该操作」）、`app_version_invalid`（400，后台 spec / 脚本校验失败，消息带字段路径）、`app_version_frozen`（409「已发布的版本不能修改」）。端口上限复用 `port_limit`。

### 10.1 控制台（`/api/v1`，`requireUser` 组内；`GET /apps*` 在组外可匿名、按 IP 限流每分钟 120 次）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/apps` | `{items:[{id,slug,name,summary,category,icon,version:{id,version,ports,env:[spec.env],links,timeout_minutes},images:[{id,name}]}]}`，只含 published 应用的最新 published 版本；匿名可访问（市场页） |
| GET | `/apps/{slug}` | 单个，多 `description` |
| POST | `/orders` / `/bookings` | body 新增 `app_id?`、`app_env?: {NAME: value}`；有 `app_id` 时 `image_id` 必须在该应用最新版本的 `image_ids` 里（否则 `app_image_mismatch`），每台实例同一事务插入一条 queued 部署；变量校验失败整单不下 |
| GET | `/instances/{id}/apps` | `{items:[Deployment]}`，Deployment 字段见 §6.1（含 `env`、`secret_env:{NAME:true}`、`url`、`links`、`update_available`、`latest_version`） |
| POST | `/instances/{id}/apps` | `{app_id, app_version_id?, env}` → 201 Deployment |
| GET | `/instances/{id}/apps/{dep}` | 单个 |
| POST | `/instances/{id}/apps/{dep}/redeploy` | `{app_version_id?, env?}` → Deployment |
| POST | `/instances/{id}/apps/{dep}/stop` / `/start` | → Deployment |
| DELETE | `/instances/{id}/apps/{dep}` | 卸载 → Deployment（state removing / removed） |
| GET | `/instances/{id}/apps/{dep}/log` | `{log:string, live:bool, at}`：实例 running 时实时取一次（≤ 20s，失败回落到 `log_tail`，`live=false`） |
| GET | `/instances/{id}/apps/{dep}/events` | 最近 50 条 |

`instanceView` 新增 `apps_summary: [{id, name, state, progress, url}]`（列表页一行显示用，§11.2）。

### 10.2 后台（`/admin/api/v1`）

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| GET | `/apps` | all | 全部应用（含 draft / retired），每项带版本数、`deployments_live`（非终态部署数） |
| POST | `/apps` | super | `{slug,name,summary,description,category,icon,sort}`；审计 `app.create` |
| PUT | `/apps/{id}` | super | 除 slug；`status` 可改；审计 `app.update` |
| DELETE | `/apps/{id}` | super | 仅 draft 且无版本；审计 `app.delete` |
| GET | `/apps/{id}/versions` | all | 版本列表，每项带 `coverage:[{image_id,image_name,cached,online,unknown}]` |
| POST | `/apps/{id}/versions` | super | `{version,image_ids,spec,script,changelog, copy_from?}` → draft；**draft 也必须通过 spec 与 `sh -n` 校验**（失败 `app_version_invalid`，不保存），避免「能保存不能发布」；审计 `app.version.create` |
| PUT | `/apps/{id}/versions/{vid}` | super | draft 可改全部；published 只能改 `changelog`、`status=retired`（其余 → `app_version_frozen`）；审计 `app.version.update` |
| POST | `/apps/{id}/versions/{vid}/publish` | super + TOTP | 校验 → `published`、`published_at`、`spec_hash`；广播预拉取；审计 `app.version.publish`（含 `spec_hash`） |
| POST | `/apps/{id}/versions/{vid}/prepull` | ops | 重新广播预拉取 → `{machines}` |
| GET | `/apps/{id}/versions/{vid}/script` | all | 脚本原文（审计不记） |
| GET | `/deployments?state=&app_id=&user_id=&instance_id=&page=` | all | 部署列表（排障）：实例、用户、应用 / 版本、状态、进度、错误码、时间 |
| GET | `/deployments/{dep}` | all | 详情 + 事件 + `log_tail`（脱敏后的） |
| POST | `/deployments/{dep}/force-remove` | ops | `{reason}` → 卸载（`desired=removed`，事件「平台卸载：<reason>」）；审计 `app.deployment.force_remove` |
| GET | `/instances/{id}` | all | 响应新增 `apps:[Deployment]`（含 `log_tail`） |
| GET | `/machines/{id}` | ops | 新增 `cached_images[]`、`cached_images_at` |
| GET | `/dashboard` | all | 新增 `apps_installing`、`apps_running`、`apps_failed_24h` |

## 11. 控制台页面

### 11.1 市场页 `/market` 与 `RentDialog`
- 商品卡片下方新增「一键部署」区：4 个应用的小卡片（图标、名称、summary），点击 = 打开 `RentDialog` 并预选该应用（`mode='immediate'`）。
- `RentDialog` 镜像一行上方新增「预装应用」：`不预装` / 应用列表（`名称 · 默认模型`）；选了应用后：镜像下拉只剩兼容镜像并默认第一个（锁定提示「该应用需要此镜像」）；下方展开变量表单（按 spec.env 渲染：enum → select、int → input-number、bool → switch、secret → password 输入 + 「生成」按钮（当 pattern 允许时生成 32 位 `[A-Za-z0-9]`）、string → input；`help` 为提示文字；`required` 校验）；确认页多一行「预装应用：<名称>（<模型>）」。
- 提交：`app_id` 与 `app_env` 随 `/orders` 或 `/bookings` 一起发；`useIdempotencyKey` 的派生参数加上 `appId` 与 `appEnv`。
- 错误 `app_image_mismatch / app_env_*` 显示后端文案并回到表单。

### 11.2 我的实例 `/instances`
- 卡片在时间文案下加一行应用摘要：`apps_summary` 每项一个小标签：`部署中 45%`（蓝）/ `运行中`（绿，可点 = 打开 `url`）/ `失败`（红）/ `已停止`（灰）；无部署不显示。
- 有 `installing / queued` 部署的实例与 creating 一样每 10 秒刷新。

### 11.3 实例详情 `/instances/:id`
新增「应用」卡片，放在「Web 访问」之前（`data-testid="detail-apps-card"`）：
- 每个部署一行：图标 + 名称 + 版本（`update_available` 时显示「有新版本」标签）、状态标签、进度条（installing 时，带 phase 中文：准备 / 下载模型 / 安装 / 启动 / 健康检查 与 `message`）、「打开」按钮（running 且有 `url`；`links` 展开为下拉）、操作菜单：重新部署（弹窗：版本选择 + 变量表单预填、secret 显示「已设置，留空沿用」）、停止 / 启动、查看日志（抽屉，`<pre>` 显示 ≤ 64KB，「刷新」按钮调 `/log`，自动滚到底）、卸载（确认：「卸载后应用目录会删除，模型缓存保留在 /root/data/models」）。
- failed 时行内红色说明：错误码中文 + `message` + 「查看日志」。实例停机时所有行显示「实例已停机，续费后自动恢复」。
- 「添加应用」按钮：弹窗列出 published 应用，灰显不兼容的（提示需要的镜像）与已部署的；选中后变量表单 → `POST /instances/{id}/apps`。实例 reserved / creating 时允许添加（显示「将在开通后自动部署」）。
- 「动态」卡片合并部署事件（按时间排序，detail 前缀应用名）。

### 11.4 帮助页
新增「一键部署」一节：应用在实例里以进程运行、免费；部署需要下载模型（受实例限速，16GB 约 15 分钟）；模型缓存目录与清理方法；端口默认私有、打开链接要登录控制台；Open WebUI 首次访问者成为管理员；vLLM 的 `API_KEY`；JupyterLab 令牌；停机 / 续费后自动恢复；换机后需要重新下载模型；想自己改配置可以 SSH 进去看 `/root/data/.spark-apps/<slug>/`，但「重新部署」会覆盖 `app.sh / env`。

## 12. 后台页面
- 菜单新增 `应用 /apps`（all；写操作按钮按角色显示）；`menu.spec.ts` 同步。
- `/apps`：表格（名称、slug、分类、状态、版本数、活跃部署数、排序），新建 / 编辑弹窗（super），行内进入 `/apps/:id`。
- `/apps/:id`：应用信息 + 版本表（版本、状态、兼容镜像、覆盖率 `已缓存 X / 在线 Y（未知 Z）`、发布时间、活跃部署数）；「新建版本」（可从某版本复制）→ 版本编辑页 `/apps/:id/versions/:vid`：左侧表单（版本号、兼容镜像多选、changelog）、spec 用结构化表单（端口表、超时、磁盘、变量表（名 / 标签 / 类型 / 必填 / 默认 / 选项 / 正则 / 范围 / 帮助）、链接表）而不是裸 JSON，并提供「查看 JSON」只读；右侧脚本编辑框（等宽、行号，`<textarea>` 即可，不引入代码编辑器）+「校验」按钮（调 PUT 的 dry-run：`?validate=1` 只校验不保存）；「发布」（动态码弹窗，显示校验结果与覆盖率警告）；「重新预拉取」；published 版本编辑页只读（changelog 与退役除外）。
- `/deployments`：筛选（状态、应用、用户、实例）、表格、详情抽屉（状态、进度、错误、变量（secret 为 `***`）、事件、日志尾部 `<pre>`、「强制卸载」(ops，原因必填)）。
- `/instances/:id`：新增「应用」卡片（只读 + 强制卸载）。
- `/machines/:id`：新增「已缓存镜像」列表与上报时间。
- 仪表盘：新增「部署中 / 运行中 / 24 小时失败」三个数字。

## 13. 配置与常量

| 项 | 值 | 说明 |
|---|---|---|
| `MaxAppsPerInstance` | 3 | D2 |
| `PollInterval` / `HealthInterval` | 5s / 30s | Runner 轮询 |
| `ExecUploadTimeout` / `ExecStatusTimeout` | 60s / 20s | 单条 exec |
| `DefaultTimeoutMinutes` / `MaxTimeoutMinutes` | 60 / 180 | 版本 spec |
| `MaxLogTail` | 65536 字节 | |
| `MaxBundleBytes` / `MaxScriptBytes` | 262144 / 65536 | |
| `MaxExecPerRound` | 16 | 并发 exec |
| `UnreachableFailAfter` | 10 次 | 连续 DialError 后 failed / 告警 |
| `CrashedAfter` | 3 次 | 连续 `alive=0` |
| `AppDir` | `/root/data/.spark-apps` | 容器内 |
| `ModelsDir` | `/root/data/models` | |
| `SPARK_APP_HF_ENDPOINT` | 环境变量，默认 `https://hf-mirror.com` | 注入容器 `HF_ENDPOINT` |
| `SPARK_APP_PIP_INDEX_URL` | 环境变量，默认 `https://pypi.tuna.tsinghua.edu.cn/simple` | 注入 `PIP_INDEX_URL`（脚本若需装小包） |
| `SPARK_APP_RUNNER` | 环境变量 `on / off`，默认 on | e2e-server 与某些测试关闭 Runner，改由钩子驱动 |

其余常量在 `internal/appdeploy/consts.go`，不做配置。

## 14. 并发与正确性
- **部署行的写者**：用户 API（创建 / 重新部署 / 停止 / 启动 / 卸载）、Runner 回写、rental 钩子（实例状态变化）。三者都先 `SELECT … FOR UPDATE` 部署行；rental 钩子在已持实例锁的事务里锁部署行（顺序 `instance → app_deployments → instance_ports`），用户 API 走 `lockOwned`（先实例后部署，同序），Runner 回写**只锁部署行**（不锁实例；读实例状态用快照，过期由下一轮纠正）。`instance_ports` 的插入 / 删除都在持部署行锁之后。因此没有环。
- **exec 与事务分离**：Runner 的 exec 在事务外；回写时比较 `updated_at` 与轮次开始读到的值，不一致（用户在 exec 期间点了停止 / 卸载）→ 丢弃本轮结果，部署行按用户操作的状态在下一轮处理。`status.json` 的 `seq` 单调，回放旧结果无害。
- **幂等**：`deploy` / `remove` 容器内幂等（§5.2）；Runner 对同一部署同一轮只执行一次；用户「重新部署」连点两次等价于一次（API 对已是 queued 的部署只更新变量并返回当前行，不报错）。
- **一致性检查**（集成测试断言）：`removed / lost` 的部署没有 `instance_ports.deployment_id` 指向它；非终态部署的实例非终态；每个实例非终态部署 ≤ 3；`running` 部署的主端口在 `instance_ports` 里（或曾被用户删除——用 `events` 区分，测试里不允许该情形）。
- **时钟**：超时与 `next_poll_at` 用注入的 `clock.Clock`；e2e 的假时钟推进后 Runner 轮询按假时钟判超时。
- **多副本**：咨询锁；锁持有副本的 `upstream.Pool` 连接数会多一些（每实例一条，与 ingress 共用同一个 Pool 实例）。

## 15. 迁移与兼容
1. 迁移 00020：新表与两列，无数据改写。
2. 发布顺序：先升级 Agent（新 `StateReport.images`），再 platform；platform 先发也能工作（覆盖率显示「未知」）。本计划不改实例指令，老 Agent 的实例操作完全不受影响。
3. 镜像：上线前在 Harbor 里准备 §9 的三个镜像并在后台 `images` 登记（替换占位条目），再创建应用与版本（后台操作，不写迁移种子——脚本内容随仓库 `deploy/apps/<slug>/{spec.json,app.sh}` 提供，后台「从文件导入」不做，运维粘贴即可；实施任务里提供 `platform admin app import <dir>` 子命令批量导入为 draft，减少手工）。
4. `images.ref` 允许本地 tag（现状已允许）；`pull_image` 对本地 tag 失败只影响指令记录。
5. 回滚：Down 删表；容器内的应用继续运行，但不再有人登记端口 / 恢复；`instance_ports` 行保留（列删除后变普通端口）。
6. e2e 现有用例：`RentDialog` 新增字段默认「不预装」，`/orders` 请求体无 `app_id` 时行为不变；实例详情新增卡片不影响既有 `data-testid`。

## 16. 测试与验收

### 16.1 单元（Go，嵌入式 PG，假时钟；`run.sh` 用本机 `/bin/sh`）
- `appdeploy/spec`：`ValidateSpec` 矩阵（端口范围 / 22 / 2222 / 重复 / 数量、超时上下限、env 名正则 / 保留名 / 禁用名 / 类型 / default 自洽 / options 上限 / pattern 非法 RE2、links）；`ValidateEnv`（多余键、换行 / NUL、长度、int 区间、bool、enum、pattern、required 缺失、default 补齐、secret 最短 8、secret 分拆）；`env` 文件渲染（单引号注入 `a'b` → `'a'\''b'`，快照）；`spec_hash` 对 JSON 键序不敏感；部署包 tar 内容与权限快照（≤ 256KB，超出报错）。
- `run.sh`（`internal/appdeploy/runtime/run_test.go`，用临时目录当 `/root/data`，`SPARK_APP_DIR` 可覆盖）：`sh -n`；`deploy` 立即返回且后台写 `status.json`；fake `app.sh`（install 写标记、start 起 `sleep` 并写 `service.pid`）全程 → `running`；重复 `deploy` 跳过 install（`.installed` 匹配）且旧 `sleep` 被杀；`spec_hash` 变化重跑 install；install 非零 → `failed` 且 `exit_code`；`timeout_minutes` 用尽（测试用秒级覆盖变量 `SPARK_TEST_TIMEOUT_S`）→ `failed`；`stop` 杀进程组（start 里起的子孙进程也没了）；`remove` 删目录且对不存在目录返回 0；`status` 输出格式；`spark_status` 原子写与 `seq` 递增；磁盘不足（`SPARK_TEST_DF_FREE_GIB` 覆盖）→ `failed(disk)`；健康检查无 `health_path` 时走 TCP。
- `appdeploy/runner`（用 `sshtest` 的 **shell 模式**，见 §16.2）：状态表每一行（queued 上传失败 / DialError 10 次 / installing 进度回写 / running 登记端口 / 端口认领（换机场景）/ failed 三种错误码 / timeout 用假时钟 / stop / start / remove / removing 时容器不可达 / running 的 alive=0 三次 → crashed / health_path 2xx 与 500）；`seq` 回退忽略；回写时 `updated_at` 不一致丢弃；脱敏（日志里出现 secret 值被替换，短于 8 的 secret 在校验层已拒绝）；`status.json` 超 16KB 或非法 JSON 视为损坏；咨询锁拿不到跳过；每轮并发 ≤ 16。
- `appdeploy/service`（用户操作）：D2 上限、`app_exists`、镜像兼容（多镜像列表）、端口冲突（与用户端口 / 另一应用）、端口总数 5、reserved 实例可建 queued、非主人 `not_found`、各状态允许操作矩阵（§6.2）、redeploy 换版本更新 `spec_hash` / 变量沿用 secret / 清 secret、退役版本 `app_unavailable`、限流。
- `rental` 钩子：`startLease`（首次开通、续费 Start、换机新实例）→ running / stopped 部署回 queued 且 failed 不动；`beginWipe` / `failCreating` / 取消 → removed；`AdminMigrate` → 旧 lost、新 queued 复制、`instance_ports.deployment_id` 置 NULL 后被认领；`Place` / `PlaceBatch` 带 `app_id`：镜像不符整单失败、变量错误整单失败、N 台各一条 queued、幂等回放带部署。
- `catalog / adminapi`：应用 CRUD、slug 不可改、删除条件、版本 draft 校验（spec、`sh -n` 失败路径带行号）、发布需 TOTP、published 冻结（`app_version_frozen`）、retired、复制为新版本、覆盖率计算（含未知）、`force-remove` 审计、角色矩阵（`permissions_test.go`）。
- `agentgw / agent`：`StateReport.images` 解析、截断 200 项与 256 字符、旧 Agent 无字段不覆盖 `cached_images`；Agent `docker image ls` 命令快照与输出解析（过滤 `<none>`）；fake Agent 注入镜像。
- `upstream.Pool.Exec`：退出码、stdout / stderr 截断、超时取消、stdin 传递、连接坏掉时重建一次、`ErrClosed`。
- `api`：路由、匿名 `GET /apps` 限流、Deployment 视图字段（secret 只给 `true`）、`/log` 实时与回落、`apps_summary`。
- 迁移 Up / Down 往返。

### 16.2 集成（Go，嵌入式 PG + 进程内 fake Agent + `sshtest` shell 模式）
- `sshtest` 新增 `Options.Shell`：`exec` 请求改为在一个临时目录（充当容器的 `/`，`HOME` 与 `/root/data` 映射到它）里用本机 `/bin/sh -c` 真正执行，stdin / stdout / stderr / 退出码透传；`run.sh` 里的绝对路径 `/root/data` 通过 `SPARK_APP_ROOT` 覆盖（Runner 在测试配置下注入）；`direct-tcpip` 仍拨本地地址，健康检查打到测试起的 httptest 服务器（fake `app.sh start` 不真的起服务，由测试在 spec 端口上起 httptest 并把其 PID 写成 `service.pid`——Mac 与 Linux 都能跑）。
- 全链路：下单带 `app_id=vllm`（fake 版本，`install` 为 `sleep 2` + 进度汇报）→ fake Agent 完成 Create → running → Runner：queued → installing（断言进度 / phase / log_tail 变化）→ running → `instance_ports` 出现 8000（私有，`deployment_id`）→ 经 ingress 请求该端口打到 httptest → 停止 → 端口仍在、`url` 仍返回但 `state=stopped` → 启动 → running → 到期停机 → Runner 跳过 → 续费恢复 → 自动回 queued → running → 释放 → removed、端口随实例失效。
- 失败链路：`install` 退出 3 → failed(script_failed)，`log_tail` 含脚本输出；重新部署（修正变量）→ running；超时（假时钟推进）→ failed(timeout) 且容器里的 `_bg` 被 stop；容器 sshd 不可达（`sshtest.DropAll` + 拒绝）→ 10 轮后 failed(unreachable) + 告警一条。
- 换机：running 部署 → `AdminMigrate` → 旧 lost、新 queued → 新实例 running → 部署成功并认领复制过来的端口行（不重复插入）。
- 并发：同一实例三个应用同时 queued，一轮内并发执行，端口互不冲突，第四个 `app_limit`。
- 多副本：两个 Runner 实例只有一个在工作（另一个每轮跳过），停掉持锁者后另一个接管（锁随会话释放）。
- 一致性检查（§14）在每个集成测试结束时执行。

### 16.3 端到端（Playwright）
- e2e-server：`SPARK_APP_RUNNER=off`；`sshtest` 用 shell 模式。新增钩子 `POST /hooks/apps/seed` → 创建 §9 的四个应用的 **fake 版本**（快速脚本：`install` 只汇报 `spark_status download 50 …; sleep 1`，`start` 不起真服务、把 `$$` 写成 `service.pid` 后 `sleep` 挂住；健康检查打到 e2e-server 经 `/hooks/ingress-upstream` 在 spec 端口上提供的静态 handler）；`POST /hooks/apps/tick {n?}` → Runner 执行 n 轮（与 `/hooks/sweep` 同风格）；`/hooks/agent` 增加 `images` 参数。
- 覆盖表新增（登记到 `coverage.json`）：
  - `C-MKT-09`～`12`：市场页应用卡片打开对话框并预选；选应用后镜像锁定与变量表单（必填校验、生成密钥）；确认页显示预装应用；开通后实例详情出现 queued 的应用，`tick` 后变部署中并显示进度，再 `tick` 变运行中且「打开」链接指向 `ins-…-8000`。
  - `C-APP-01`～`10`：实例详情添加应用（不兼容灰显并提示镜像）；第二个应用部署成功；上限 3 提示；失败显示错误与日志抽屉；重新部署（改变量，secret 留空沿用）；停止 / 启动；卸载确认文案与端口消失；实例停机时的提示；有新版本标签（后台发布新版本后）；列表页应用摘要标签。
  - `C-ING-07`：应用端口私有，打开链接走登录跳转后拿到 httptest 内容。
  - `A-APP-01`～`08`：应用新建 / 编辑 / 删除条件；版本新建（结构化表单）→ 校验错误显示字段路径；脚本 `sh -n` 错误显示行号；发布需动态码并显示覆盖率警告（fake Agent 注入镜像后覆盖率变 100%）；published 版本只读；复制为新版本；部署列表筛选与详情（secret 显示 `***`）；强制卸载需原因并写审计。
  - `A-INS-14`：实例详情应用卡片；`A-MCH-10`：机器详情已缓存镜像。
- 注意：应用用例在各自实例上操作，不改全局机器状态；用到假时钟的只有超时用例（推进 ≤ 2 小时）。

### 16.4 真机验收清单（192.168.1.221，一台机器，全部通过才算完成）
平台跑在 Mac（配置同计划 3 验收：`SPARK_DOMAIN=192-168-1-218.sslip.io`，ingress 用 sslip 域）；Agent 用本计划的新版本重装（`先 Agent 后 platform`）。没有 Harbor：镜像在 221 上构建并以本地 tag 登记（验收记录注明实际使用的 NGC 标签）。模型下载受 200Mbps 限速，验收时把机器限速临时设为 1000（`SPARK_AGENT_BANDWIDTH_MBPS`，新实例生效）。需要 root 的步骤由负责人执行 sudo。

1. **镜像构建**：在 221 上 `docker build` 三个应用镜像（`deploy/image/apps/{llm-serving,comfyui,pytorch}`），基础镜像从 nvcr.io 拉；每个镜像 `docker run --rm --gpus all <img> python3 -c "import torch; print(torch.cuda.is_available(), torch.cuda.get_device_name())"` 输出 `True NVIDIA GB10`；`vllm --version`、`jupyter lab --version`、`python3 /opt/ComfyUI/main.py --help` 可执行；镜像里没有 `/etc/ssh/ssh_host_*`；后台登记三个镜像，机器详情「已缓存镜像」在 2 分钟内列出它们。
2. **目录导入**：`platform admin app import deploy/apps` 导入四个应用为 draft；后台逐个发布（动态码）；覆盖率显示 `1 / 1`。
3. **预装 vLLM**：市场页选「vLLM 推理服务」（Qwen3-4B，先用小模型，生成 `API_KEY`）→ 下单 → 实例 running 后控制台 1 分钟内显示「部署中」，进度随下载推进（phase 下载模型 → 启动 → 健康检查），完成 ≤ 20 分钟；「打开」链接 `/docs` 经登录跳转可见 Swagger；`curl -H "Authorization: Bearer <key>" https://ins-…-8000.<域>/v1/chat/completions -d '{...}'`（带 `sc_ingress` Cookie 或先设为公开）返回补全；`nvidia-smi` 看到 vLLM 进程；`ls /root/data/models/modelscope/Qwen/Qwen3-4B` 存在。
4. **同实例加 Open WebUI**：实例详情添加 `open-webui`（默认指向 127.0.0.1:8000，`OPENAI_API_KEY` 填上一步的密钥）→ running ≤ 5 分钟 → 打开链接注册管理员 → 选择模型 `Qwen/Qwen3-4B` 对话成功（WebSocket 经 ingress）。
5. **加 JupyterLab（第 3 个）**：添加 `jupyterlab`（生成令牌）→ running → 打开链接输入令牌进入，`/root/data` 可见；再添加任意第 4 个 → `app_limit` 提示。
6. **重新部署换模型**：vLLM 重新部署，`MODEL_ID = Qwen/Qwen3-8B`（约 16GB）→ 旧服务被停、下载新模型、running；Open WebUI 里模型列表随之变化；`/root/data/models` 下两个模型都在。
7. **失败与日志**：ComfyUI 不好在 llm-serving 上部署（灰显并提示镜像）；用后台把 vLLM 的某个 draft 新版本 `MAX_MODEL_LEN` 上限改大并发布，重新部署时填 131072 + `GPU_MEMORY_UTILIZATION=0.3` 让 vLLM 启动失败 → `failed`，日志抽屉里能看到 vLLM 的报错；再改回 → 成功；有新版本标签在发布后出现。
8. **停止 / 启动 / 停机恢复**：停止 vLLM → 端口链接仍在、`curl` 502；启动 → 恢复。`UPDATE instances SET expires_at = now() + interval '1 minute'` → 停机 → 三个应用显示「实例已停机」；续费 → 实例 running 后 3 分钟内三个应用自动回到运行中（不重新下载模型，日志里 `skip install`）。
9. **ComfyUI**：新开一台实例（spark/comfyui，预装 `comfyui`，SDXL base）→ running ≤ 15 分钟 → 打开链接，加载默认工作流生成一张 512×512 图片，输出在 `/root/data/comfyui-output`；`nvidia-smi` 看到进程。（只有一台机器：先释放第 3 步的实例。）
10. **卸载**：卸载 ComfyUI → 目录 `/root/data/.spark-apps/comfyui` 消失、端口 8188 从「Web 访问」消失、`/root/data/models` 仍在；再部署一次 → 跳过下载（`.installed` 没了但模型在，`modelscope download` 对已存在文件秒过），≤ 3 分钟 running。
11. **Agent 停机 / 容器不可达**：负责人 `sudo systemctl stop spark-agent`（不停容器）→ 应用轮询照常（走 SSH，不经 Agent）；`docker stop <容器>`（模拟容器挂掉）→ 10 轮后部署 `failed(unreachable)`，告警一条；`docker start` 后 `systemctl start spark-agent`，重新部署成功。
12. **安全**：在变量里填 `'; touch /tmp/pwned; '`（string 类型、无 pattern 的测试变量——用一个 draft 版本加一个 `NOTE` 变量）→ 部署成功、容器里 `env` 文件是 `export NOTE=''\''; touch /tmp/pwned; '\'''`，`/tmp/pwned` 不存在；后台部署详情里 secret 显示 `***`，日志尾部里 `API_KEY` 的值被替换为 `***`（vLLM 启动日志会打印 api-key 参数）；`iptables -S SPARK-FWD / SPARK-IN` 未变。
13. **对账无噪声**：全过程后台告警只有第 11 步预期的一条；`ssh_sessions` 没有因平台 exec 产生用户会话行（exec 走上游池，不经网关）。

验收记录写到 `docs/superpowers/acceptance-plan7.md`，格式同计划 6。

## 17. 范围与遗留

**本计划做**：§2～§16 全部；Agent 只改 `StateReport.images`；`sshtest` shell 模式；`platform admin app import`；三个应用镜像的 Dockerfile 与四个应用的 `spec.json / app.sh`。

**上线前核对（写入 `followups-plan7.md`）**：Harbor 搭建（compose 服务、`proxy-nvcr` 代理项目、`spark` 项目、机器端 `SPARK_AGENT_ALLOW_CIDRS` 含 Harbor）；在 arm64 构建机上构建并推送三个镜像、后台登记替换占位条目；NGC 标签核实；默认限速 200Mbps 下模型下载时间给运营做预期管理；Open WebUI 首访即管理员的提醒文案；ModelScope 是否需要登录令牌（当前所选模型均不需要）。

**明确不做（遗留清单）**
- 应用收费、按模型收费、Token 商城。
- 部署期间临时放开限速（需要 Agent 指令）；模型预热到机器本地缓存（机器级共享模型缓存）。
- 自动升级已有部署到新版本；灰度发布；版本回滚按钮（用「复制为新版本」）。
- 用户自定义应用 / 上传脚本；应用市场评分、使用统计以外的运营数据。
- 多实例编排（双机互联、分布式推理）；应用之间的依赖声明（Open WebUI 依赖 vLLM 只靠默认地址）。
- 日志全量持久化与搜索（只保留 64KB 尾部 + 容器内文件）；Web 终端。
- 后台脚本内容审查 / shellcheck；脚本签名。
- 应用健康异常的自动重启（只标 unhealthy / crashed）；短信通知。
- ComfyUI-Manager / 自定义节点、更多模型源（CivitAI）、HuggingFace 热门模型搜索（竞品「AI 匹配」）。
- 预订（reserved）实例的预装在开通前修改变量（只能卸载再添加）。
- 镜像构建流水线（CI）；Harbor 镜像 GC 策略。

## 18. 任务拆分（供实施计划引用，23 项）
1. 迁移 00020（含 `cleanup_pending`、Down）；`appdeploy` 包骨架、类型、常量、错误码；`labels.ts` / 类型。
2. 应用镜像：`deploy/image/apps/{llm-serving,comfyui,pytorch}/Dockerfile`（核实 NGC 标签、PyPI 镜像 ARG、ComfyUI 源 ARG）、`docs/image-spec.md` 增补（curl / tar / setsid / python3 / modelscope / jupyterlab）；在 221 上构建验证（§16.4 第 1 项前半）。
3. `ValidateSpec` / `ValidateEnv` / `env` 渲染 / `spec_hash` / 部署包生成；单测。
4. 容器内 `run.sh`（embed）+ `spark_status`；`sh` 测试（§16.1）。
5. `upstream.Pool.Exec`；单测。
6. `sshtest` shell 模式；单测。
7. 目录服务（`appdeploy/catalog.go`）：应用 / 版本 CRUD、发布（`sh -n`、hash、冻结、预拉取广播）、覆盖率、`platform admin app import`；单测。
8. 部署服务（`appdeploy/service.go`）：创建 / 重新部署 / 停止 / 启动 / 卸载 / 日志 / 事件，限额与端口冲突；单测。
9. Runner：轮询、咨询锁、状态表、回写乐观并发、端口登记 / 认领、脱敏、告警；单测（用 sshtest shell 模式）。
10. rental 钩子（`AppHooks`）：`startLease` / `beginWipe` / `failCreating` / 取消 / `AdminMigrate`；`Place` / `PlaceBatch` 带 `app_id`；单测。
11. Agent：`StateReport.images`（proto、`make proto`、`docker image ls`、fake Agent 注入）；网关写 `cached_images`；单测。
12. 控制台 API（§10.1）+ `instanceView.apps_summary`；单测。
13. 后台 API（§10.2）+ 角色矩阵 + 审计；单测。
14. `app.go` 组装（Runner 启动、`SPARK_APP_RUNNER`、配置项）、README 配置表与部署注意。
15. 四个应用的 `deploy/apps/<slug>/{spec.json, app.sh}`（含模型大小表、进度汇报、ModelScope / hf-mirror 切换）；`sh -n` 与 spec 校验进 `make test`。
16. 集成测试（§16.2）。
17. 控制台：`RentDialog` 预装应用与变量表单组件（`AppEnvForm.vue`，供添加 / 重新部署复用）、市场页应用卡片。
18. 控制台：实例详情「应用」卡片（进度、日志抽屉、操作）、列表摘要、帮助页。
19. 后台：应用列表 / 详情 / 版本编辑页（结构化 spec 表单、脚本框、校验、发布动态码、覆盖率）。
20. 后台：部署列表 / 详情 / 强制卸载、实例详情卡片、机器详情缓存镜像、仪表盘。
21. e2e：钩子（`/hooks/apps/seed`、`/hooks/apps/tick`、`/hooks/agent images`）、Playwright 用例、`coverage.json`。
22. 真机验收检查点 A（§16.4 第 1～8、11～13 项，vLLM / Open WebUI / JupyterLab）。
23. 真机验收检查点 B（第 9、10 项 ComfyUI）与 `acceptance-plan7.md`、`followups-plan7.md`。

## 附录：自评（设计方，2026-10-20）

- 已核对代码事实：容器非 privileged（任务书措辞有误，§0 注明）；`upstream.Pool` 只有 `Dial`，`Exec` 是新增；`sshtest` exec 只回显，需要 shell 模式才能跑 Runner 测试；`images.ref` 已允许本地 tag；`BroadcastPullImage` 可复用；`instance_ports` 每实例 5 个的上限决定了 D2 的 3。
- 自查发现并修正的点：JupyterLab 令牌原打算由脚本生成并经 `message` 回显，与 D9 的脱敏冲突，改为用户必填 + 前端生成；`OnInstanceRunning` 原想区分 start / deploy 两条路径，改为统一回 queued（§5.4）；卸载时实例停机的情况需要 `cleanup_pending`，已补进 §2 的表定义与 §5.4 的恢复逻辑。
- 待评审重点：§5.3 状态表、§5.4 钩子的锁顺序、§9 的 NGC 标签与 vLLM 退路、§16.4 在单机上的顺序安排（第 3～8 步与第 9～10 步共用唯一机器）。
