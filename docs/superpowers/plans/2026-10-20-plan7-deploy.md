# Spark Cloud 计划 7：AI 应用一键部署 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 后台维护带版本的应用目录（vLLM 推理服务、Open WebUI、ComfyUI、JupyterLab）；用户在开通实例时「预装」或在运行中实例上「添加应用」，平台经已有上游 SSH 连接把部署包放进容器、在容器内以 `setsid` 后台执行固定的 `run.sh`，每 5 秒轮询进度 / 日志回写数据库，成功后自动登记私有端口并在控制台给出链接；支持重新部署（换版本 / 改变量）、停止 / 启动、卸载、失败日志；实例停机续费后自动恢复、换机后复制为 queued；Agent 只新增 `StateReport.images`（缓存覆盖率）；三个应用镜像的 Dockerfile 与四个应用的 `spec.json / app.sh`；最后在 192.168.1.221 单机完成真机验收。

**Architecture:** 不加新进程、不加「应用容器」。新包 `internal/appdeploy`：`apps / app_versions / app_deployments / app_deploy_events` 四张表；`Service`（目录 + 用户操作 + 视图）与 `Runner`（每 5 秒一轮、咨询锁单副本、`sweepEach` 模式：无锁选候选 → 独立事务 `FOR UPDATE SKIP LOCKED` 复核 → **exec 在事务外** → 回写时比对 `updated_at` 乐观并发）。exec 走新增的 `upstream.Pool.Exec`（共享 `ssh.Client` 上开 session）；健康检查走已有 `Pool.Dial`。容器内「运行时」是随 platform 二进制 embed 的固定 `run.sh`（POSIX sh），版本脚本 `app.sh` 只做 `install / start / stop`，用户变量只以 `export NAME='…'`（单引号转义）写进 `env` 文件，命令行里没有任何用户输入。与 `rental` 的衔接是 `rental.AppHooks` 接口（`OnInstanceRunning / OnInstanceGone / OnInstanceMigrated / PrepareInstall / QueueInstall`），在 rental 已持实例锁的事务里调用，`appdeploy` 实现、`app.go` 用 `SetAppHooks` 接上（避免包循环依赖）。

**Tech Stack:** Go 1.27（`~/.local/go/bin`）、pgx v5、chi v5、`golang.org/x/crypto/ssh`、embedded-postgres（`dbtest.Main`）、`embed`（`run.sh`）；前端 Vue 3 + Element Plus + Vitest + Playwright 1.64（Chrome 通道）；protoc 36.2 + protoc-gen-go v1.36.12 + protoc-gen-go-grpc v1.6.2（`make proto`，都在 `~/.local/bin`）。**不新增任何 Go 模块依赖（并发信号量用带缓冲 channel，不引入 `x/sync/semaphore`）、不新增 npm 依赖。**

**Spec:** `docs/superpowers/specs/2026-10-20-plan7-deploy-design.md`（已批准，提交 c0bb525；裁定 D1～D15 为负责人默认）。上位：总设计 §2 `images`、§4、路线图第 2 项；计划 2 §3 / §4.2 / §5.2；计划 3 §1.3 / §4 / §5.3；计划 6 §6 / §8。

**分支：** `feat/plan7-deploy`（worktree `/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud-p7`，规格提交 c0bb525）。

**本计划不包含（规格 §17）：** 应用收费 / 按模型收费；部署期间临时放开限速；机器级共享模型缓存；自动升级已有部署、灰度、版本回滚按钮；用户自定义应用 / 上传脚本；应用市场评分；多实例编排、应用依赖声明；日志全量持久化与搜索、Web 终端；脚本内容审查 / shellcheck / 签名；健康异常自动重启、短信通知；ComfyUI-Manager / 自定义节点、CivitAI、HF 热门模型搜索；reserved 实例开通前修改预装变量；镜像构建流水线（CI）、Harbor GC；**Harbor 搭建本身**（运维事项，写进上线前核对）。

## 执行须知（写给每个子代理）

- 任务**按编号顺序串行执行，一个任务一个子代理**。子代理只看得到自己任务的文字，看不到本文其它任务：每个任务的 Interfaces 必须完整（消费什么、产出什么、精确签名），代码步骤给完整代码，不写「同上」「类似 Task N」。
- 测试机 192.168.1.221（`imac` 用户，arm64，Docker 29.6.2，CUDA 13 驱动）。**需要 root 的命令由协调人（controller）在运行时执行，绝不写进任何脚本**；密码由负责人在对话中提供，协调人在**运行时**把它放进仅该条命令的环境变量，经 stdin 送给 `sudo -S`。密码不进任何持久化存储（不写文件、脚本、提交，也不用 Keychain）。远端用一个每次都从变量读密码的 shell 函数 `s`，形如：
  `SUDO_PW='<运行时提供>' sh -c 'printf "%s\n" "$SUDO_PW" | ssh -T imac@192.168.1.221 "read -r P; s() { printf \"%s\n\" \"\$P\" | sudo -S -p \"\" \"\$@\"; }; s systemctl stop spark-agent"'`
  （远端 `read -r P` 从 stdin 读走密码，之后每个 `s <cmd>` 都把它送给 `sudo -S`；`-p ""` 关掉提示符，`-T` 不分配 tty。）**密码绝不写进任何文件、记录、提交或本计划**；记录前核对输出不含密码（`sudo -S` 不回显，`set -x` 不得开启；`git diff --cached | grep -i sudo_pw` 应为空）。`docker build / docker run` 在 221 上 `imac` 已在 `docker` 组（计划 2 验收时加入），不需要 root。
- 开发 Mac 的 DNS 对 `*.sslip.io` 返回假地址（198.18.x.x）：curl 一律加 `--resolve <host>:<port>:<ip>`；Chrome / Playwright 用 `--host-resolver-rules`。
- **测试机访问不了 Docker Hub、gcr.io、proxy.golang.org；nvcr.io 与 docker.m.daocloud.io 可达。** 应用镜像的基础镜像直接从 nvcr.io 拉；需要 Docker Hub 的镜像用 `docker.m.daocloud.io/<repo>` 全名拉再 `docker tag` 回原名；Go 用 `GOPROXY=https://goproxy.cn,direct`，npm 用 `https://registry.npmmirror.com/`，pip 用 `https://mirrors.aliyun.com/pypi/simple`（或 `https://pypi.tuna.tsinghua.edu.cn/simple`），apt 用镜像自带的 ports.ubuntu.com（可达）。
- 开发环境短信验证码（mock，固定 `000000`）按手机号每 60 秒只能发一次：脚本化登录前先等 60 秒或换手机号。
- 每个任务结束前必须实际运行并通过：`go vet ./... && go test ./...`；涉及前端的任务再加 `cd web && npm test && npm run build`；页面任务再加该任务列出的 Playwright 用例。证据是命令输出。
- 提交信息结尾空一行加 `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`。

## Global Constraints

- 仓库根目录（worktree）`/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud-p7`；所有命令前置 `export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH`。Go 模块名 `spark-cloud`。开发 Mac 没有 Docker；Go 测试用 embedded-postgres（`dbtest.Main`）；`run.sh` 的测试用本机 `/bin/sh`（macOS 的 `/bin/sh` 是 bash 的 POSIX 模式，Linux 是 dash：脚本必须两边都过）。`make check` = `test`（`go vet && go test ./...` + `install_test.sh` + `web-test`）+ `e2e`（`make web` → `npm run e2e:coverage` → Playwright 三个 project），末尾会打印覆盖表核对结果。
- **迁移编号：本计划是 `migrations/00020_apps.sql`。计划 8（推荐 / referral）在另一分支并行开发、也占用了 00020：约定计划 7 先合并、保留 00020，计划 8 合并前自行改为 00021。** 本计划不得使用 00021，也不要为「给计划 8 让路」改号。`internal/platform/db/migrate_apps_test.go` 的往返测试按 00020 写。
- 数据模型（规格 §2）照抄进迁移；在规格之外**新增两列** `app_deployments.unreachable_count INT NOT NULL DEFAULT 0`、`app_deployments.crash_count INT NOT NULL DEFAULT 0`（规格 §5.3 的「连续 10 次 DialError」「连续 3 次 alive=0」需要持久计数，platform 重启不清零）。`app_deployments.id = 'dep-' + 12 位`（`randx.ID("dep-", 12)`）。Down 顺序：先 `ALTER TABLE instance_ports DROP COLUMN deployment_id`、`ALTER TABLE machines DROP COLUMN cached_images, DROP COLUMN cached_images_at`，再 `DROP TABLE app_deploy_events, app_deployments, app_versions, apps`；迁移头注释写明「回滚后容器内应用继续运行，只是平台不再管理」。
- 状态（规格 §6.1）：`queued → installing → running ⇄ stopped`；`failed`（重新部署 → queued）；`removing → removed`（终态）；`lost`（终态，换机）。`desired ∈ running | stopped | removed`。错误码 `error_code ∈ script_failed | timeout | health_failed | upload_failed | unreachable | port_conflict | port_limit | instance_gone | crashed | disk`。
- 常量（规格 §13，全在 `internal/appdeploy/consts.go`）：`MaxAppsPerInstance = 3`、`PollInterval = 5s`、`HealthInterval = 30s`、`ExecUploadTimeout = 60s`、`ExecStatusTimeout = 20s`、`DefaultTimeoutMinutes = 60`、`MaxTimeoutMinutes = 180`、`MaxLogTail = 65536`、`MaxBundleBytes = 262144`、`MaxScriptBytes = 65536`、`MaxStatusBytes = 16384`、`MaxExecPerRound = 16`、`UnreachableFailAfter = 10`、`CrashedAfter = 3`、`DefaultAppRoot = "/root/data"`、`AppDirName = ".spark-apps"`、`ModelsDirName = "models"`、`MaxCachedImages = 200`、`MaxCachedImageLen = 256`、`MinSecretLen = 8`、`UserOpsPerHour = 60`。配置项（`config.Config`）：`AppHFEndpoint`（`SPARK_APP_HF_ENDPOINT`，默认 `https://hf-mirror.com`）、`AppPipIndexURL`（`SPARK_APP_PIP_INDEX_URL`，默认 `https://pypi.tuna.tsinghua.edu.cn/simple`）、`AppRunner bool`（`SPARK_APP_RUNNER` `on|off`，默认 on；只影响 `cmd/platform` 是否启动 Runner 协程，`cmd/e2e-server` 从不启动，改由 `/hooks/apps/tick` 驱动）。
- 容器内布局（规格 §5.1）：`<AppRoot>/.spark-apps/<slug>/{run.sh, app.sh, spec.json, env, status.json, log, .installed, service.pid}`；`AppRoot` 生产固定 `/root/data`，测试经 `RunnerConfig.AppRoot` 指到临时目录（规格 §16.2 的 `SPARK_APP_ROOT` 覆盖以此实现，不设环境变量）。部署包 = tar（`run.sh`、`app.sh`、`spec.json`、`env`，≤ 256KB），上传命令固定为 `mkdir -p <dir> && chmod 700 <dir> && tar -x -C <dir> -f - && chmod 600 <dir>/env`，`<dir>` 只由 `AppRoot` 与 `slug`（`^[a-z][a-z0-9-]{1,31}$`）拼出。
- `env` 文件（规格 §3.3）：每行 `export NAME='value'`，`'` → `'\''`；平台保留变量先写（**比规格多三项，供 `run.sh` 免解析 JSON**）：`SPARK_APP`、`SPARK_APP_DIR`、`SPARK_DATA_DIR=<AppRoot>`、`SPARK_PORT`、`SPARK_PORT_2`、`SPARK_INSTANCE_ID`、`SPARK_MODELS_DIR=<AppRoot>/models`、`SPARK_SPEC_HASH`、`SPARK_TIMEOUT_S=<timeout_minutes×60>`、`SPARK_HEALTH_PATH=<ports[0].health_path>`、`SPARK_MIN_FREE_GIB=<min_data_free_gib>`、`SPARK_HEALTH_ADDR=<RunnerConfig.HealthAddr(port) 或 127.0.0.1:<port>>`（测试注入，生产恒为 `127.0.0.1:<port>`）、`HF_ENDPOINT`、`MODELSCOPE_CACHE=<models>/modelscope`、`HF_HOME=<models>/hf`、`PIP_INDEX_URL`。用户变量名 `^[A-Z][A-Z0-9_]{0,63}$`，不得以 `SPARK_` 开头，不得是 `PATH / LD_PRELOAD / LD_LIBRARY_PATH / HOME / SHELL / IFS / ENV / BASH_ENV`；值 UTF-8、不含 `\n \r \0`。
- `run.sh`（规格 §5.2）：子命令 `deploy | start | stop | status | remove`；`deploy` 前台做 prepare（`df` 剩余 ≥ `SPARK_MIN_FREE_GIB`、`sh -n app.sh`）后把 `_bg deploy` 脱离会话放后台并立即返回 0；`_bg` 写 `status.json`（原子：临时文件 + `mv`，`seq` 递增）；`.installed` 内容 ≠ `SPARK_SPEC_HASH` 时跑 `app.sh install`；`app.sh start` 必须写 `service.pid`；健康检查 `curl -sf http://$SPARK_HEALTH_ADDR$SPARK_HEALTH_PATH`（`SPARK_HEALTH_PATH` 为空时 TCP 连通探测）直到成功或 `SPARK_TIMEOUT_S` 用尽；`stop` 按 `status.json` 里的进程组 `kill -TERM -- -<pgid>`、10 秒后 `-KILL`；`status` 打印 `status.json` 一行 + `alive=<0|1>` + `log_size=<n>`。**脱离会话**：有 `setsid` 用 `setsid`（镜像规范要求 util-linux），没有（开发 Mac）用 `python3 -c 'import os,sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])'`，两者都没有时普通 `&`（只记日志）；`_bg` 把自己的 pgid（`ps -o pgid= -p $$`）写进 `status.json.pid`。`_bg` 必须 `</dev/null >>log 2>&1`，否则 `sshtest` shell 模式里 `cmd.Wait` 会等管道关闭。
- Runner（规格 §5.3）：每轮 `pg_try_advisory_lock(hashtext('appdeploy'))`（与清扫器一样每轮取锁、轮末释放；拿不到跳过本轮）；候选 `SELECT id FROM app_deployments WHERE next_poll_at <= $now AND (state IN ('queued','installing','running','stopped','removing') OR (state = 'removed' AND cleanup_pending)) ORDER BY next_poll_at LIMIT 50`；每个部署：事务 1 `FOR UPDATE SKIP LOCKED` 复核并把 `next_poll_at = now + PollInterval`、`last_polled_at = now`（`RETURNING updated_at` 作为快照）→ 事务外读实例目标（`instances ⋈ machines`：`state, suspended_at, lan_ip, ssh_host_pubkey`）与 exec → 事务 2 `FOR UPDATE` 比对 `updated_at = 快照` 否则丢弃本轮结果。每轮并发 exec ≤ 16（带缓冲 channel），同一实例一轮 ≤ 3 个部署。exec 超时：上传 60s、其它 20s；`DialError`（含 ctx 超时之外的连接层错误）→ `unreachable_count + 1`，`next_poll_at = now + 30s`，达到 10 → `failed(unreachable)` + `RaiseAlertOnce(warning, app_deploy_unreachable, …)`；成功 exec 后 `unreachable_count = 0`。实例非 running 或暂停 → 不 exec，`next_poll_at = now + 30s`；实例 `wiping / released / failed / cancelled` → `removed(instance_gone)`。
- 加锁顺序（规格 §14，写进 `appdeploy/service.go` 头注释）：rental 钩子在已持 `instances` 行锁的事务里锁 `app_deployments` 行（`instance → app_deployments → instance_ports`）；用户操作 `rental.LockOwned`（先实例后部署，同序）；Runner 回写**只锁部署行**，不锁实例；`instance_ports` 的插入 / 认领 / 删除都在持部署行锁之后。
- 暴露（规格 §7）：进入 `running` 时（回写事务内）对 `spec.ports` 每项：`UPDATE instance_ports SET deployment_id = $dep, note = $appName, updated_at = $now WHERE instance_id = $i AND port = $p AND deployment_id IS NULL`（认领换机复制的行）；0 行则 `INSERT (instance_id, port, visibility='private', note, deployment_id)`；撞唯一键（属于别的部署）→ `failed(port_conflict)`；总数 > 5 → `failed(port_limit)`。卸载 `DELETE FROM instance_ports WHERE deployment_id = $1` + 审计 `instance.port_delete`（user actor，detail「卸载应用 <name>」）。URL 用 `PortURL func(instanceID string, port int) string`（app 传 `ingressAuth.PortURL`）。
- 脱敏（规格 §8 / D9）：`secret` 类型最短 8；回写 `message / log_tail / 事件 detail` 前把全部 secret 值 `strings.ReplaceAll(…, "***")`；API 只返回 `secret_env: {NAME: true}`；重新部署不传沿用、传空串清除；后台强制卸载审计只记部署 ID。
- 错误码（规格 §10，`apperr`）：`app_not_found`（404「应用不存在或未上架」）、`app_unavailable`（409「该应用当前没有可用版本」）、`app_limit`（409「每个实例最多部署 3 个应用」）、`app_exists`（409「该应用已在此实例上部署」）、`app_invalid_state`（409「当前状态不允许该操作」）、`app_version_frozen`（409「已发布的版本不能修改」）为固定值；`app_image_mismatch`（409「该应用需要以下镜像之一：<名称列表>，当前实例镜像为 <名称>」）、`app_port_conflict`（409「应用端口 <port> 已被该实例的其它端口占用」）、`app_env_invalid`（400，带变量 label 与原因）、`app_env_required`（400「请填写 <label>」）、`app_version_invalid`（400，带字段路径）用 `apperr.New` 现场构造。端口上限复用 `port_limit`。
- 角色（D8）：后台 `GET /apps*`、`GET /deployments*`、`GET /apps/{id}/versions/{vid}/script` 为 `allRoles`；`POST/PUT/DELETE /apps*`、版本创建 / 修改 为 `superOnly`；`publish` 为 `superOnly` + TOTP（`withStepUp`）；`prepull`、`force-remove` 为 `opsRoles`。审计动作：`app.create / app.update / app.delete / app.version.create / app.version.update / app.version.publish（含 spec_hash）/ app.version.prepull / app.deployment.force_remove`；用户侧 `instance.app_deploy / instance.app_redeploy / instance.app_stop / instance.app_start / instance.app_remove`（actor = user，diff 只含部署 ID、应用 slug、版本 ID，不含变量值）。
- Agent 协议（D12）：`StateReport` 新增 `repeated string images = 2;`，**唯一的协议改动**；Agent 取 `docker image ls --format '{{.Repository}}:{{.Tag}}' --filter dangling=false`，过滤含 `<none>` 的行，≤ 200 项、每项 ≤ 256 字符；网关 `handleReport` 在 `RecordStateReport` 之外另调 `Fleet.RecordCachedImages`（旧 Agent 字段为空 → 不覆盖）。发布顺序「先 Agent 后 platform」，platform 先发也能工作（覆盖率显示「未知」）。
- 时间一律 `clock.Clock`（超时、`next_poll_at`、事件时间）；错误用 `apperr`；生产禁止 mock。
- 端到端规范沿用计划 5：用例标题以覆盖表 ID 开头、`data-testid` 命名 `页面-元素`、禁止 `waitForTimeout`、`workers: 1`；`web/e2e/coverage.json` 与覆盖表一致；现有 Playwright 用例必须保持全绿。本计划新增覆盖表 ID：`C-MKT-09～12`、`C-APP-01～10`、`C-ING-07`、`A-APP-01～08`、`A-INS-14`、`A-MCH-10`（共 25 个）。

## Review Focus

1. **用户在 exec 期间点了停止 / 卸载**：Runner 的回写事务必须比对 `updated_at` 丢弃本轮结果，部署按用户操作的状态在下一轮处理；不能出现「用户已卸载、Runner 又把它写回 running 并登记端口」→ Task 9 `TestRunnerDiscardsStaleWriteback`。
2. **变量注入面为零**：值 `'; touch /tmp/pwned; '`、含 `$()`、反引号、换行 → 写进 `env` 的是 `export NOTE=''\''; touch /tmp/pwned; '\'''`；换行 / NUL 在校验层拒绝；`sh -c` 执行 `. ./env` 后 `$NOTE` 原样等于输入；变量名 `PATH` / `SPARK_X` / `LD_PRELOAD` 在 spec 校验层拒绝 → Task 3 `TestRenderEnvEscapes`、`TestValidateSpecRejectsReservedNames`，Task 4 `TestRunShEnvIsNotEvaluated`。
3. **实例停机 → 续费恢复 → 应用自动回来且不重新下载模型**：`OnInstanceRunning` 把 running / stopped 的部署回 queued、failed 不动；`run.sh deploy` 的 `.installed` 匹配时跳过 install（日志 `skip install`）；旧 `service.pid` 已死不影响 → Task 10 `TestStartLeaseRequeuesDeployments`、Task 4 `TestRunShRedeploySkipsInstall`、Task 16 全链路。
4. **换机后端口认领不重复插入**：`AdminMigrate` 复制端口时 `deployment_id = NULL`，旧实例部署 `lost`、新实例复制 queued；新部署 running 时 `UPDATE … WHERE deployment_id IS NULL` 认领已有行，`instance_ports` 行数不变 → Task 10 `TestMigrateCopiesDeployments`、Task 9 `TestRunnerClaimsMigratedPort`。
5. **旧 Agent 不发 `images` 时平台不覆盖缓存列表，新 Agent 的列表被截断到 200 项 / 256 字符；`StateReport` 里只多一个字段，旧平台收到新 Agent 的消息照常解析**（proto3 未知字段忽略）→ Task 11 `TestReportImagesRecorded`、`TestReportWithoutImagesKeepsCache`。

## 已做出的裁定（规格未写明或与代码核对后确定的细节）

- **包依赖方向**：`appdeploy` 导入 `rental`（用 `rental.Instance`、`rental.LockOwned`、`rental.State*`）；`rental` 不导入 `appdeploy`，只定义 `AppHooks` 接口与 `InstallPlan`（不透明 `any`），`app.go` 组装顺序：`rental.NewService` → `appdeploy.NewService(…, r)` → `r.SetAppHooks(apps)`。钩子为 nil 时 rental 的所有调用点通过 `s.apps()` 取 noop 实现。
- **`rental.LockOwned` 导出**：现有 `lockOwned(ctx, tx, userID, id)` 改名导出（内部调用点同步改），供 `appdeploy` 用户操作按同一锁顺序锁实例并校验归属。
- **Runner 读实例目标不经 `rental.Service`**：`appdeploy/runner.go` 自己查 `SELECT i.state, i.suspended_at IS NOT NULL, m.lan_ip, i.ssh_host_pubkey, i.image_id FROM instances i JOIN machines m ON m.id = i.machine_id WHERE i.id = $1`（不加锁的快照，规格 §14）。
- **端口登记 / 删除的 SQL 写在 `appdeploy/ports.go`**（不经 `rental.AddPort`：那条路径会审计 `instance.port_add` 并校验用户归属；应用端口由平台登记，只在卸载时以 user actor 审计 `instance.port_delete`）。`rental.ListIngressRoutes` 不改。
- **Task 5 / 6 顺序与规格 §18 对调**：先做 `sshtest` shell 模式（Task 5），`Pool.Exec` 的超时 / stdin 测试（Task 6）需要 `sleep` 与 `cat` 真执行。
- **`sshtest.Options.Shell` 的语义**：`Shell: true` 时 `exec` 请求用 `/bin/sh -c <cmd>` 真执行，`Dir = Options.Root`，环境 `HOME=<Root>`、`PATH` 继承、`SPARK_TEST_ROOT=<Root>`；stdin / stdout / stderr / 退出码透传；进程在服务器 `Close` 时被杀（`exec.CommandContext(s.ctx, …)`）。`Root` 为空时自动建临时目录（`Close` 删除）。原回显模式不变（`Shell: false`）。
- **`Pool.Exec` 的超时由调用方 ctx 决定**（规格的 `ExecTimeout` 常量在 `appdeploy` 里用 `context.WithTimeout` 施加）：ctx 到期 → 关闭 session、返回 `ctx.Err()`（不是 `DialError`，共享连接保留）；`NewSession` / `Run` 的连接层错误 → 丢弃连接重试一次，再失败返回 `*DialError`；远端非零退出 → `exit = 退出码, err = nil`。
- **`status` 组合命令固定为** `cd <dir> && sh run.sh status; echo ---; tail -c 65536 log 2>/dev/null`；`cd` 失败（目录被用户删了）→ exec 非零 → `failed(script_failed, "状态文件缺失")`。
- **健康检查地址可注入**：`RunnerConfig.HealthAddr func(port int) string`（测试 / e2e 注入，nil = `127.0.0.1:<port>`）写进 `env` 的 `SPARK_HEALTH_ADDR`，`run.sh` 用它做容器内健康检查；平台侧健康检查仍经 `Pool.Dial(port)`（`sshtest` 用 `SetTarget` 把端口指到同一个 httptest）。e2e / 集成测试的 fake `app.sh start` 不监听端口（`sleep 100000 & echo $! > service.pid`），避免多个实例共用一台假容器时端口冲突。
- **下单预装的钩子**：`AppHooks.PrepareInstall(ctx, q, appID, imageID, env) (InstallPlan, error)` 在 `placeBatch` 取镜像之后、挑机器之前调用（校验失败整单不下）；`QueueInstall(ctx, tx, inst, plan, now)` 在每台 `insertInstance` 之后调用插入 queued 部署。`Place`（`POST /orders`）同样支持 `app_id`（`PlaceRequest` 加 `AppID *int64; AppEnv map[string]string`）。
- **`OnInstanceGone` 的调用点**：`beginWipe`（释放 / 洗机 / 换机旧实例，reason「实例已释放」）、`failCreating`（「开通失败」）、`cancelInstance`（「预订取消」）；`reassignAfterCreateFailure` 不调（实例 ID 不变，部署保持 queued）。`OnInstanceRunning` 在 `startLease` 与 `startBooked` 更新 `state = 'running'` 之后调用。`OnInstanceMigrated` 在 `AdminMigrate` 复制端口之后、`beginWipe(old)` 之前调用（先标 lost，`beginWipe` 的 `OnInstanceGone` 对 lost 行不再改）。
- **目录导入**：`platform admin app import <dir>` 读 `<dir>/<slug>/{app.json, spec.json, app.sh}`；`app.json` = `{"name","summary","description","category","icon","sort","version","images":["<images.name>", …],"changelog"}`，镜像按 `images.name` 精确匹配（验收时后台登记的本地 tag 镜像用同样的名字）；slug 不存在 → 建 draft 应用 + draft 版本；slug 已存在 → 只建新 draft 版本（版本号重复 → 报错）。导入不发布（发布需 TOTP，在后台做）。
- **脚本语法校验**（Task 7 修订裁定）：进程内 `mvdan.cc/sh/v3/syntax.NewParser(syntax.Variant(syntax.LangPOSIX))` 解析，不起子进程（生产镜像 distroless 无 shell；dash / bash 报错不一致）；错误消息 `第 N 行: <原因>`。容器内 `run.sh` 的 `sh -n app.sh` 不变。
- **覆盖率**：`coverage = {image_id, image_name, cached, online, unknown}`：`online = count(agent_online)`；`cached = count(agent_online AND ref = ANY(cached_images) AND cached_images_at >= now − 5m)`；`unknown = count(agent_online AND (cached_images_at IS NULL OR cached_images_at < now − 5m))`。
- **`apps_summary` 顺序**按 `created_at`；只含非终态（`removed / lost` 不显示）。
- **用户操作限流**：`api/apps.go` 里 `userLimiter`（结构同 `ipLimiter`，窗口 1 小时、上限 60，`DisableRateLimits` 时 1,000,000）只作用于写操作（创建 / 重新部署 / 停止 / 启动 / 卸载）。
- **后台「查看 JSON」与 `?validate=1`**：`PUT /apps/{id}/versions/{vid}?validate=1` 与 `POST /apps/{id}/versions?validate=1` 只校验不保存，返回 `{ok: true, spec_hash}`（草稿预览用）。
- **e2e 钩子** `POST /hooks/apps/seed` 幂等：已存在同 slug 的应用直接返回；返回 `{apps: [{id, slug, version_id, port}]}`；`POST /hooks/apps/tick {n?}`（默认 1）返回 `{rounds, processed}`；`/hooks/agent` 增加 `images?: string[]`（设置后该 fake Agent 之后每次 StateReport 都带这些镜像）。
- **真机验收顺序**：Task 22 做规格 §16.4 第 1～8、11～13 项（vLLM / Open WebUI / JupyterLab，含安全与对账），Task 23 释放实例后做第 9、10 项（ComfyUI）并写记录与遗留清单。

## 文件结构

```
spark-cloud-p7/
├── migrations/00020_apps.sql                                              # Task 1
├── api/proto/agent/v1/agent.proto、internal/agentpb/*.pb.go                # Task 11
├── deploy/image/apps/{llm-serving,comfyui,pytorch}/Dockerfile、docs/image-spec.md   # Task 2
├── deploy/apps/{vllm,open-webui,comfyui,jupyterlab}/{app.json,spec.json,app.sh}     # Task 15
├── internal/
│   ├── appdeploy/consts.go、types.go、spec.go、env.go、bundle.go、runtime/run.sh、runtime/run_test.go、
│   │             catalog.go、service.go、view.go、ports.go、runner.go、status.go、hooks.go、coverage.go、events.go   # Task 1、3、4、7～10
│   ├── access/sshtest/server.go（Shell 模式）、access/upstream/pool.go（Exec）                    # Task 5、6
│   ├── rental/service.go（AppHooks、SetAppHooks）、results.go、sweeper.go、cancel.go、migrate.go、batch.go、place.go、ports.go（LockOwned）  # Task 10
│   ├── agent/runtime/runtime.go、docker/docker.go、fake/fake.go、executor/ops.go、client/client.go、fakeagent/fakeagent.go  # Task 11
│   ├── agentgw/connect.go、convert.go、fleet/agent.go、fleet/machines.go                              # Task 11
│   ├── api/server.go、apps.go、instances.go、booking.go                                               # Task 12
│   ├── adminapi/server.go、apps.go、deployments.go、instances.go、machines.go、dashboard.go、permissions_test.go   # Task 13
│   ├── app/app.go、platform/config/config.go、platform/apperr/apperr.go、cmd/platform/{main,admin,app_import}.go   # Task 1、7、14
│   ├── app/apps_e2e_test.go、e2ehooks/apps.go、cmd/e2e-server/main.go                                 # Task 16、21
├── web/shared/src/types/apps.ts、labels.ts                                                            # Task 1、17
├── web/console/src/components/{AppEnvForm,AppPicker,RentDialog}.vue、logic/apps.ts、views/{MarketView,InstancesView,InstanceDetailView,HelpView}.vue   # Task 17、18
├── web/admin/src/{menu,router}.ts、views/{AppsView,AppDetailView,AppVersionView,DeploymentsView,InstanceDetailView,MachineDetailView,DashboardView}.vue、logic/appSpec.ts   # Task 19、20
├── web/e2e/{helpers/apps.ts,console/apps.spec.ts,console/market.spec.ts,console/ingress.spec.ts,admin/apps.spec.ts,admin/instances.spec.ts,admin/machines.spec.ts,coverage.json}、web/playwright.config.ts   # Task 21
├── README.md                                                              # Task 14
└── docs/superpowers/{acceptance-plan7.md,followups-plan7.md}             # Task 22、23
```

## 任务依赖

Task 1（迁移 + 类型 + 错误码）→ 2（应用镜像 Dockerfile + 221 构建）→ 3（spec / env / bundle）→ 4（`run.sh`）→ 5（`sshtest` shell 模式）→ 6（`Pool.Exec`）→ 7（目录服务 + 导入命令）→ 8（部署服务）→ 9（Runner）→ 10（rental 钩子 + 下单预装）→ 11（Agent `images`）→ 12（控制台 API）→ 13（后台 API）→ 14（组装 / 配置 / README）→ 15（四个应用脚本）→ 16（集成测试 + e2e 钩子）→ 17（控制台 RentDialog / 市场页）→ 18（控制台详情 / 列表 / 帮助）→ 19（后台应用目录页）→ 20（后台部署 / 实例 / 机器 / 仪表盘）→ 21（Playwright）→ **22 真机验收 A（vLLM / Open WebUI / JupyterLab）** → **23 真机验收 B（ComfyUI）+ 记录 + 遗留**。按编号顺序执行，共 23 个任务。

---
### Task 1: 迁移 00020、`appdeploy` 包骨架（类型、常量、状态）、错误码、前端类型与文案

**Files:**
- Create: `migrations/00020_apps.sql`、`internal/appdeploy/consts.go`、`internal/appdeploy/types.go`、`internal/platform/db/migrate_apps_test.go`、`web/shared/src/types/apps.ts`
- Modify: `internal/platform/apperr/apperr.go`、`web/shared/src/types/index.ts`、`web/shared/src/labels.ts`
- Test: `internal/platform/db/migrate_apps_test.go`（新建）、`web/shared/src/__tests__/labels.spec.ts`（若存在则追加；否则新建 `web/shared/src/__tests__/apps-labels.spec.ts`）

**Interfaces:**
- Produces:
  - 表 `apps`、`app_versions`、`app_deployments`（含 `unreachable_count`、`crash_count`）、`app_deploy_events`；`instance_ports.deployment_id TEXT NULL REFERENCES app_deployments(id)`；`machines.cached_images TEXT[] NOT NULL DEFAULT '{}'`、`machines.cached_images_at TIMESTAMPTZ`；索引同规格 §2。
  - `appdeploy` 常量：见 Global Constraints「常量」一条；状态常量 `StateQueued / StateInstalling / StateRunning / StateStopped / StateFailed / StateRemoving / StateRemoved / StateLost`，`DesiredRunning / DesiredStopped / DesiredRemoved`，错误码常量 `ErrCodeScriptFailed = "script_failed"` 等 10 个；`TerminalStates = []string{StateRemoved, StateLost}`。
  - 类型：`appdeploy.Spec{Ports []PortSpec; TimeoutMinutes int; MinDataFreeGiB int; Env []EnvSpec; Links []LinkSpec}`、`PortSpec{Port int; Label, Path, HealthPath string}`、`EnvSpec{Name, Label, Type string; Required bool; Options []string; Default, Pattern string; MaxLen int; Min, Max *int64; Help string}`（JSON 标签见代码）、`LinkSpec{Label, Path string}`；`App`、`Version`、`Deployment`、`Event` 结构体（字段 = 表列）。
  - `apperr`：`ErrAppNotFound`、`ErrAppUnavailable`、`ErrAppLimit`、`ErrAppExists`、`ErrAppInvalidState`、`ErrAppVersionFrozen`；函数 `AppImageMismatch(need []string, have string) *Error`、`AppPortConflict(port int) *Error`、`AppEnvInvalid(label, reason string) *Error`、`AppEnvRequired(label string) *Error`、`AppVersionInvalid(path, reason string) *Error`。
  - 前端：`types/apps.ts` 导出 `AppCategory`、`AppEnvType`、`AppEnvSpec`、`AppPortSpec`、`AppLinkSpec`、`AppSpec`、`AppVersionBrief`、`AppCatalogItem`、`DeploymentState`、`DeploymentHealth`、`Deployment`、`DeploymentEvent`、`AppsSummaryItem`（字段与 §10.1 Deployment 视图一致，见代码）；`labels.ts` 新增 `deploymentStateLabel / deploymentStateType / deploymentPhaseLabel / deploymentErrorLabel / appCategoryLabel / appStatusLabel`，`alertKindLabel` 加 `app_deploy_unreachable: '应用部署无法连接容器'`，`auditActions` 加本计划 13 个动作。

- [ ] **Step 1: 写失败的迁移往返测试**

`internal/platform/db/migrate_apps_test.go`（结构仿 `migrate_booking_test.go`）：

```go
package db_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"

	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/migrations"
)

// 迁移 00020：带存量数据 Up → Down → Up 往返；Down 后旧表 / 旧列不受影响。
func TestMigration00020RoundTrip(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", dbtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	if _, err := p.UpTo(ctx, 19); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO users (phone, kind, status, created_at, updated_at) VALUES ('13800000001', 'personal', 'active', now(), now())`)
	exec(`INSERT INTO machines (serial, rack, slot, lan_ip, product_id, state, created_at, updated_at) VALUES ('SN1', 'A', '1', '10.0.0.1', 1, 'idle', now(), now())`)
	exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ('ins-aaaaaaaa', 1, 1, 1, 1, 'running', 'x', now(), now())`)
	exec(`INSERT INTO instance_ports (instance_id, port, visibility, note, created_at, updated_at) VALUES ('ins-aaaaaaaa', 8888, 'private', 'old', now(), now())`)
	if _, err := p.UpByOne(ctx); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO apps (slug, name, category, status, created_at, updated_at) VALUES ('vllm', 'vLLM', 'llm', 'published', now(), now())`)
	exec(`INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, created_at, updated_at)
		VALUES (1, '1', 'published', '{1}', '{"ports":[{"port":8000}]}', 'true', 'h', now(), now())`)
	exec(`INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ('dep-000000000001', 'ins-aaaaaaaa', 1, 1, 1, 'queued', 8000, 'h', now(), now(), now())`)
	exec(`UPDATE instance_ports SET deployment_id = 'dep-000000000001' WHERE instance_id = 'ins-aaaaaaaa'`)
	exec(`UPDATE machines SET cached_images = '{"spark/llm-serving:25.09"}', cached_images_at = now() WHERE id = 1`)
	// 同一实例同一应用只能有一条非终态部署
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ('dep-000000000002', 'ins-aaaaaaaa', 1, 1, 1, 'running', 8000, 'h', now(), now(), now())`); err == nil {
		t.Fatal("second live deployment of same app accepted")
	}
	exec(`INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ('dep-000000000003', 'ins-aaaaaaaa', 1, 1, 1, 'removed', 8000, 'h', now(), now(), now())`)
	if n := count(`SELECT count(*) FROM app_deployments WHERE unreachable_count = 0 AND crash_count = 0`); n != 2 {
		t.Fatalf("counters default = %d rows", n)
	}
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM instance_ports WHERE instance_id = 'ins-aaaaaaaa'`); n != 1 {
		t.Fatalf("ports after down = %d", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('apps', 'app_versions', 'app_deployments', 'app_deploy_events')`); n != 0 {
		t.Fatalf("tables after down = %d", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'machines' AND column_name IN ('cached_images', 'cached_images_at')`); n != 0 {
		t.Fatalf("machine columns after down = %d", n)
	}
	if _, err := p.UpByOne(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM apps`); n != 0 {
		t.Fatalf("apps after re-up = %d", n)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/platform/db/ -run TestMigration00020RoundTrip 2>&1 | head -20`
Expected: `UpByOne` 失败（没有 00020 迁移）。

- [ ] **Step 3: 写迁移**

`migrations/00020_apps.sql`：

```sql
-- +goose Up
-- 计划 7：应用目录（apps / app_versions）、实例上的应用部署（app_deployments / app_deploy_events）、
-- 应用自动登记的端口（instance_ports.deployment_id）、机器已缓存镜像（machines.cached_images）。
-- Down 删表删列；容器内已部署的应用进程不受影响（只是平台不再登记端口、不再在实例恢复后拉起它们）。

CREATE TABLE apps (
    id          BIGSERIAL PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,          -- ^[a-z][a-z0-9-]{1,31}$；容器内目录名与端口备注都用它
    name        TEXT NOT NULL,                 -- 对用户展示，1～32 字符
    summary     TEXT NOT NULL DEFAULT '',      -- ≤ 80 字符
    description TEXT NOT NULL DEFAULT '',      -- ≤ 4000 字符，Markdown 子集
    category    TEXT NOT NULL CHECK (category IN ('llm', 'image', 'notebook', 'tool')),
    icon        TEXT NOT NULL DEFAULT 'app',   -- 固定图标名集合（前端映射）
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
    image_ids    BIGINT[] NOT NULL,            -- 兼容镜像，第一个为默认
    spec         JSONB NOT NULL,
    script       TEXT NOT NULL,                -- app.sh（POSIX sh），≤ 65536 字节
    spec_hash    TEXT NOT NULL,                -- sha256(canonical(spec) || "\n" || script) 十六进制
    changelog    TEXT NOT NULL DEFAULT '',
    published_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (app_id, version)
);
CREATE INDEX app_versions_app_idx ON app_versions (app_id, status, published_at DESC);

CREATE TABLE app_deployments (
    id                TEXT PRIMARY KEY,        -- 'dep-' + 12 位 [a-z0-9]
    instance_id       TEXT NOT NULL REFERENCES instances (id),
    user_id           BIGINT NOT NULL REFERENCES users (id),
    app_id            BIGINT NOT NULL REFERENCES apps (id),
    app_version_id    BIGINT NOT NULL REFERENCES app_versions (id),
    state             TEXT NOT NULL CHECK (state IN ('queued', 'installing', 'running', 'stopped', 'failed', 'removing', 'removed', 'lost')),
    desired           TEXT NOT NULL DEFAULT 'running' CHECK (desired IN ('running', 'stopped', 'removed')),
    phase             TEXT NOT NULL DEFAULT '',   -- prepare / download / install / start / health
    progress          INT  NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    message           TEXT NOT NULL DEFAULT '',   -- ≤ 200 字符，已脱敏
    health            TEXT NOT NULL DEFAULT 'unknown' CHECK (health IN ('unknown', 'ok', 'unhealthy')),
    error_code        TEXT NOT NULL DEFAULT '',   -- script_failed / timeout / health_failed / upload_failed / unreachable / port_conflict / port_limit / instance_gone / crashed / disk
    env               JSONB NOT NULL DEFAULT '{}',
    secret_env_enc    BYTEA,                      -- 敏感变量 JSON 的 secretbox 密文
    port              INT  NOT NULL,              -- 主端口（spec.ports[0]）
    log_tail          TEXT NOT NULL DEFAULT '',   -- ≤ 65536 字节，已脱敏
    status_seq        BIGINT NOT NULL DEFAULT 0,
    attempt           INT  NOT NULL DEFAULT 0,
    spec_hash         TEXT NOT NULL,
    next_poll_at      TIMESTAMPTZ NOT NULL,
    last_polled_at    TIMESTAMPTZ,
    cleanup_pending   BOOLEAN NOT NULL DEFAULT false,  -- 实例停机时卸载：目录留在数据盘，实例恢复后由 Runner 删除
    unreachable_count INT NOT NULL DEFAULT 0,          -- 连续 DialError 次数（10 次 → failed(unreachable)）
    crash_count       INT NOT NULL DEFAULT 0,          -- 连续 alive=0 次数（3 次 → failed(crashed)）
    started_at        TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX app_deployments_live_uniq ON app_deployments (instance_id, app_id) WHERE state NOT IN ('removed', 'lost');
CREATE INDEX app_deployments_poll_idx ON app_deployments (next_poll_at) WHERE state IN ('queued', 'installing', 'running', 'stopped', 'removing') OR cleanup_pending;
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

ALTER TABLE instance_ports ADD COLUMN deployment_id TEXT REFERENCES app_deployments (id);
CREATE INDEX instance_ports_deployment_idx ON instance_ports (deployment_id) WHERE deployment_id IS NOT NULL;
ALTER TABLE machines ADD COLUMN cached_images TEXT[] NOT NULL DEFAULT '{}', ADD COLUMN cached_images_at TIMESTAMPTZ;

-- +goose Down
DROP INDEX instance_ports_deployment_idx;
ALTER TABLE instance_ports DROP COLUMN deployment_id;
ALTER TABLE machines DROP COLUMN cached_images, DROP COLUMN cached_images_at;
DROP TABLE app_deploy_events;
DROP TABLE app_deployments;
DROP TABLE app_versions;
DROP TABLE apps;
```

- [ ] **Step 4: 常量与类型**

`internal/appdeploy/consts.go`：

```go
// Package appdeploy 实现 AI 应用一键部署（计划 7）：应用目录、部署记录、容器内运行时（run.sh）与平台侧 Runner。
package appdeploy

import "time"

const (
	MaxAppsPerInstance    = 3
	PollInterval          = 5 * time.Second
	HealthInterval        = 30 * time.Second
	ExecUploadTimeout     = 60 * time.Second
	ExecStatusTimeout     = 20 * time.Second
	DefaultTimeoutMinutes = 60
	MaxTimeoutMinutes     = 180
	MaxLogTail            = 65536
	MaxBundleBytes        = 262144
	MaxScriptBytes        = 65536
	MaxStatusBytes        = 16384
	MaxExecPerRound       = 16
	UnreachableFailAfter  = 10
	CrashedAfter          = 3
	DefaultAppRoot        = "/root/data"
	AppDirName            = ".spark-apps"
	ModelsDirName         = "models"
	MaxCachedImages       = 200
	MaxCachedImageLen     = 256
	MinSecretLen          = 8
	UserOpsPerHour        = 60
	MaxMessageLen         = 200
	NotRunningRetry       = 30 * time.Second
	UnreachableRetry      = 30 * time.Second
	advisoryLockName      = "appdeploy"
)

const (
	StateQueued     = "queued"
	StateInstalling = "installing"
	StateRunning    = "running"
	StateStopped    = "stopped"
	StateFailed     = "failed"
	StateRemoving   = "removing"
	StateRemoved    = "removed"
	StateLost       = "lost"

	DesiredRunning = "running"
	DesiredStopped = "stopped"
	DesiredRemoved = "removed"

	HealthUnknown   = "unknown"
	HealthOK        = "ok"
	HealthUnhealthy = "unhealthy"

	ErrCodeScriptFailed = "script_failed"
	ErrCodeTimeout      = "timeout"
	ErrCodeHealthFailed = "health_failed"
	ErrCodeUploadFailed = "upload_failed"
	ErrCodeUnreachable  = "unreachable"
	ErrCodePortConflict = "port_conflict"
	ErrCodePortLimit    = "port_limit"
	ErrCodeInstanceGone = "instance_gone"
	ErrCodeCrashed      = "crashed"
	ErrCodeDisk         = "disk"
)

// TerminalStates 不再被 Runner 处理、也不占 app_deployments_live_uniq。
var TerminalStates = []string{StateRemoved, StateLost}

// ErrorText 错误码的中文（事件 detail 与控制台）。
var ErrorText = map[string]string{
	ErrCodeScriptFailed: "脚本执行失败", ErrCodeTimeout: "部署超时", ErrCodeHealthFailed: "健康检查未通过", ErrCodeUploadFailed: "部署包上传失败",
	ErrCodeUnreachable: "无法连接容器", ErrCodePortConflict: "端口冲突", ErrCodePortLimit: "端口数量超限", ErrCodeInstanceGone: "实例已不存在",
	ErrCodeCrashed: "服务进程已退出", ErrCodeDisk: "数据盘空间不足",
}
```

`internal/appdeploy/types.go`：

```go
package appdeploy

import "time"

type PortSpec struct {
	Port       int    `json:"port"`
	Label      string `json:"label,omitempty"`
	Path       string `json:"path,omitempty"`
	HealthPath string `json:"health_path,omitempty"` // 只在 Ports[0] 上有意义；空 = TCP 连通检查
}

type EnvSpec struct {
	Name     string   `json:"name"`
	Label    string   `json:"label,omitempty"`
	Type     string   `json:"type"` // string / int / bool / enum / secret
	Required bool     `json:"required,omitempty"`
	Options  []string `json:"options,omitempty"`
	Default  string   `json:"default,omitempty"`
	Pattern  string   `json:"pattern,omitempty"`
	MaxLen   int      `json:"max_len,omitempty"` // 0 = 256
	Min      *int64   `json:"min,omitempty"`
	Max      *int64   `json:"max,omitempty"`
	Help     string   `json:"help,omitempty"`
}

type LinkSpec struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

// Spec 是版本的结构化描述（app_versions.spec），校验规则见 ValidateSpec。
type Spec struct {
	Ports          []PortSpec `json:"ports"`
	TimeoutMinutes int        `json:"timeout_minutes,omitempty"`
	MinDataFreeGiB int        `json:"min_data_free_gib,omitempty"`
	Env            []EnvSpec  `json:"env,omitempty"`
	Links          []LinkSpec `json:"links,omitempty"`
}

type App struct {
	ID          int64     `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Summary     string    `json:"summary"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	Icon        string    `json:"icon"`
	Sort        int       `json:"sort"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Version struct {
	ID          int64      `json:"id"`
	AppID       int64      `json:"app_id"`
	Version     string     `json:"version"`
	Status      string     `json:"status"`
	ImageIDs    []int64    `json:"image_ids"`
	Spec        Spec       `json:"spec"`
	Script      string     `json:"-"` // 单独接口返回
	SpecHash    string     `json:"spec_hash"`
	Changelog   string     `json:"changelog"`
	PublishedAt *time.Time `json:"published_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Deployment 是 app_deployments 一行；对外视图见 view.go（不直接序列化本结构）。
type Deployment struct {
	ID               string
	InstanceID       string
	UserID           int64
	AppID            int64
	AppVersionID     int64
	State            string
	Desired          string
	Phase            string
	Progress         int
	Message          string
	Health           string
	ErrorCode        string
	Env              map[string]string
	SecretEnvEnc     []byte
	Port             int
	LogTail          string
	StatusSeq        int64
	Attempt          int
	SpecHash         string
	NextPollAt       time.Time
	LastPolledAt     *time.Time
	CleanupPending   bool
	UnreachableCount int
	CrashCount       int
	StartedAt        *time.Time
	FinishedAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Live 表示部署仍占用 app_deployments_live_uniq（非终态）。
func (d Deployment) Live() bool { return d.State != StateRemoved && d.State != StateLost }

type Event struct {
	ID           int64     `json:"id"`
	DeploymentID string    `json:"deployment_id"`
	At           time.Time `json:"at"`
	FromState    string    `json:"from_state"`
	ToState      string    `json:"to_state"`
	Detail       string    `json:"detail"`
}
```

- [ ] **Step 5: 错误码**

`internal/platform/apperr/apperr.go` 的 `var (...)` 末尾追加：

```go
	// 计划 7
	ErrAppNotFound      = New("app_not_found", "应用不存在或未上架", 404)
	ErrAppUnavailable   = New("app_unavailable", "该应用当前没有可用版本", 409)
	ErrAppLimit         = New("app_limit", "每个实例最多部署 3 个应用", 409)
	ErrAppExists        = New("app_exists", "该应用已在此实例上部署", 409)
	ErrAppInvalidState  = New("app_invalid_state", "当前状态不允许该操作", 409)
	ErrAppVersionFrozen = New("app_version_frozen", "已发布的版本不能修改", 409)
```

文件末尾追加带动态消息的构造函数：

```go
// AppImageMismatch 该应用需要 need 中某个镜像，实例当前镜像为 have。
func AppImageMismatch(need []string, have string) *Error {
	return New("app_image_mismatch", "该应用需要以下镜像之一："+strings.Join(need, "、")+"，当前实例镜像为 "+have, 409)
}

func AppPortConflict(port int) *Error {
	return New("app_port_conflict", fmt.Sprintf("应用端口 %d 已被该实例的其它端口占用", port), 409)
}

func AppEnvInvalid(label, reason string) *Error {
	return New("app_env_invalid", label+"："+reason, 400)
}

func AppEnvRequired(label string) *Error { return New("app_env_required", "请填写 "+label, 400) }

func AppVersionInvalid(path, reason string) *Error {
	return New("app_version_invalid", path+"："+reason, 400)
}
```

（`import` 加 `fmt`、`strings`。）

- [ ] **Step 6: 前端类型与文案**

`web/shared/src/types/apps.ts`：

```ts
export type AppCategory = 'llm' | 'image' | 'notebook' | 'tool'
export type AppStatus = 'draft' | 'published' | 'retired'
export type AppEnvType = 'string' | 'int' | 'bool' | 'enum' | 'secret'

export interface AppEnvSpec {
  name: string
  label?: string
  type: AppEnvType
  required?: boolean
  options?: string[]
  default?: string
  pattern?: string
  max_len?: number
  min?: number
  max?: number
  help?: string
}
export interface AppPortSpec { port: number; label?: string; path?: string; health_path?: string }
export interface AppLinkSpec { label: string; path: string }
export interface AppSpec {
  ports: AppPortSpec[]
  timeout_minutes?: number
  min_data_free_gib?: number
  env?: AppEnvSpec[]
  links?: AppLinkSpec[]
}

/** GET /apps 的条目（市场页 / 添加应用弹窗）。 */
export interface AppCatalogItem {
  id: number
  slug: string
  name: string
  summary: string
  description?: string
  category: AppCategory
  icon: string
  version: { id: number; version: string; ports: AppPortSpec[]; env: AppEnvSpec[]; links: AppLinkSpec[]; timeout_minutes: number }
  images: { id: number; name: string }[]
}

export type DeploymentState = 'queued' | 'installing' | 'running' | 'stopped' | 'failed' | 'removing' | 'removed' | 'lost'
export type DeploymentHealth = 'unknown' | 'ok' | 'unhealthy'

/** GET /instances/{id}/apps 的条目。 */
export interface Deployment {
  id: string
  instance_id: string
  app_id: number
  app_slug: string
  app_name: string
  app_icon: string
  version_id: number
  version: string
  latest_version_id: number | null
  latest_version: string
  update_available: boolean
  state: DeploymentState
  desired: 'running' | 'stopped' | 'removed'
  phase: string
  progress: number
  message: string
  health: DeploymentHealth
  error_code: string
  env: Record<string, string>
  secret_env: Record<string, boolean>
  spec: AppSpec
  port: number
  url: string | null
  links: { label: string; url: string }[]
  attempt: number
  cleanup_pending: boolean
  started_at: string | null
  finished_at: string | null
  created_at: string
  updated_at: string
}

export interface DeploymentEvent { id: number; at: string; from_state: string; to_state: string; detail: string }

/** 实例列表 / 详情里的 apps_summary 条目。 */
export interface AppsSummaryItem { id: string; name: string; state: DeploymentState; progress: number; url: string | null }
```

`web/shared/src/types/index.ts` 加 `export * from './apps'`。`web/shared/src/labels.ts` 追加：

```ts
export const deploymentStateLabel: LabelMap = {
  queued: '等待部署', installing: '部署中', running: '运行中', stopped: '已停止', failed: '失败', removing: '卸载中', removed: '已卸载', lost: '已失效',
}
export const deploymentStateType: LabelMap = {
  queued: 'info', installing: 'primary', running: 'success', stopped: 'info', failed: 'danger', removing: 'warning', removed: 'info', lost: 'info',
}
export const deploymentPhaseLabel: LabelMap = { prepare: '准备', download: '下载模型', install: '安装', start: '启动', health: '健康检查' }
export const deploymentErrorLabel: LabelMap = {
  script_failed: '脚本执行失败', timeout: '部署超时', health_failed: '健康检查未通过', upload_failed: '部署包上传失败', unreachable: '无法连接容器',
  port_conflict: '端口冲突', port_limit: '端口数量超限', instance_gone: '实例已不存在', crashed: '服务进程已退出', disk: '数据盘空间不足',
}
export const appCategoryLabel: LabelMap = { llm: '大模型', image: '图像生成', notebook: 'Notebook', tool: '工具' }
export const appStatusLabel: LabelMap = { draft: '草稿', published: '已发布', retired: '已退役' }
```

`alertKindLabel` 加 `app_deploy_unreachable: '应用部署无法连接容器'`；`auditActions` 加 `'app.create', 'app.update', 'app.delete', 'app.version.create', 'app.version.update', 'app.version.publish', 'app.version.prepull', 'app.deployment.force_remove', 'instance.app_deploy', 'instance.app_redeploy', 'instance.app_stop', 'instance.app_start', 'instance.app_remove'`。若 `web/shared/src/__tests__/` 下有 labels 的表驱动测试（核对每个枚举值都有文案），把新映射加进去；没有则新建 `apps-labels.spec.ts` 断言 8 个部署状态、10 个错误码、5 个 phase 都有非空文案。

- [ ] **Step 7: 验证并提交**

Run: `go build ./... && go test ./internal/platform/db/ ./internal/platform/apperr/ && cd web && npm test && npm run build`
Expected: 全部通过；`TestMigration00020RoundTrip` 通过。

```bash
git add migrations/00020_apps.sql internal/appdeploy internal/platform/db/migrate_apps_test.go internal/platform/apperr/apperr.go web/shared/src
git commit -m "feat(appdeploy): migration 00020 (apps, versions, deployments, cached images), package skeleton, error codes, shared types

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 2: 应用镜像：三个 Dockerfile、镜像规范增补、在 221 上构建并验证

**Files:**
- Create: `deploy/image/apps/llm-serving/Dockerfile`、`deploy/image/apps/comfyui/Dockerfile`、`deploy/image/apps/pytorch/Dockerfile`、`deploy/image/apps/README.md`
- Modify: `docs/image-spec.md`、`Makefile`（`app-images` 目标）

**Interfaces:**
- Consumes: `deploy/image/Dockerfile`（参考镜像的 sshd 处理方式）、`docs/image-spec.md`。
- Produces: 三个镜像（验收 / 上线用本地 tag `spark/llm-serving:<TAG>`、`spark/comfyui:<TAG>`、`spark/pytorch:<TAG>`，`<TAG>` = 实际使用的 NGC 标签，首选 `25.09`），每个都满足镜像规范 + 本计划新增要求：`curl`、`tar`、`setsid`（`util-linux`）、`python3`、`git`、`modelscope` CLI、`jupyterlab`，`PIP_INDEX_URL` 为构建参数且写进镜像环境；`llm-serving` 另有 `/opt/open-webui`（独立 venv）与 `vllm`；`comfyui` 另有 `/opt/ComfyUI`；没有 `/etc/ssh/ssh_host_*`、没有 ENTRYPOINT / CMD。
- 构建参数：`BASE`（基础镜像全名）、`PIP_INDEX_URL`（默认 `https://pypi.tuna.tsinghua.edu.cn/simple`）、`COMFYUI_SRC`（默认 `https://github.com/comfyanonymous/ComfyUI.git`，大陆用 `https://gitee.com/mirrors/ComfyUI.git`）、`COMFYUI_REF`（默认 `v0.3.60`；实施时 `git ls-remote --tags` 核实并把实际值写进验收记录）。

- [ ] **Step 1: 核实 NGC 标签（在 221 上）**

```bash
ssh imac@192.168.1.221 'docker manifest inspect nvcr.io/nvidia/vllm:25.09-py3 >/dev/null && echo vllm-ok; docker manifest inspect nvcr.io/nvidia/pytorch:25.09-py3 >/dev/null && echo pytorch-ok'
```
Expected: 两行 `ok`。若 `vllm:25.09-py3` 不存在或 `docker run --rm --gpus all nvcr.io/nvidia/vllm:25.09-py3 python3 -c "import torch;print(torch.cuda.get_device_name())"` 不是 `NVIDIA GB10`：改用最近的可用标签（`docker manifest inspect nvcr.io/nvidia/vllm:25.10-py3` 等逐个试），把实际标签记进 `deploy/image/apps/README.md` 与本任务提交信息；仍不可用时走规格 §9 的退路（`llm-serving` 以 `nvcr.io/nvidia/pytorch:<TAG>-py3` 为基础 + `pip install vllm`，Dockerfile 里 `ARG VLLM_FROM_PIP=0` 切换）。

- [ ] **Step 2: Dockerfile**

`deploy/image/apps/llm-serving/Dockerfile`：

```dockerfile
# spark/llm-serving：vLLM（NGC arm64 容器自带）+ Open WebUI（独立 venv）+ JupyterLab + modelscope + sshd。
# Agent 以 /.spark/init.sh 覆盖入口（只起 sshd），这里不设 ENTRYPOINT / CMD；应用由平台经 run.sh 以普通进程启动。
ARG BASE=nvcr.io/nvidia/vllm:25.09-py3
FROM ${BASE}
ARG PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple
ARG VLLM_FROM_PIP=0
LABEL spark.image.spec="2" spark.app.image="llm-serving"
ENV PIP_INDEX_URL=${PIP_INDEX_URL} PIP_NO_CACHE_DIR=1 DEBIAN_FRONTEND=noninteractive
# openssh-server 的 postinst 会生成主机密钥，必须删除（所有租户容器会共用同一把私钥）；主机密钥由平台按实例下发
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server ca-certificates curl iproute2 util-linux tar git python3-venv \
 && rm -rf /var/lib/apt/lists/* \
 && rm -f /etc/ssh/ssh_host_* \
 && mkdir -p /run/sshd
RUN pip install -i ${PIP_INDEX_URL} modelscope jupyterlab \
 && if [ "${VLLM_FROM_PIP}" = "1" ]; then pip install -i ${PIP_INDEX_URL} vllm; fi \
 && vllm --version
# Open WebUI 依赖与 vLLM 的依赖互相冲突，放进独立 venv；app.sh 用 /opt/open-webui/bin/open-webui 启动
RUN python3 -m venv /opt/open-webui \
 && /opt/open-webui/bin/pip install -i ${PIP_INDEX_URL} --upgrade pip \
 && /opt/open-webui/bin/pip install -i ${PIP_INDEX_URL} open-webui \
 && /opt/open-webui/bin/open-webui --help >/dev/null
```

`deploy/image/apps/comfyui/Dockerfile`：

```dockerfile
# spark/comfyui：NGC PyTorch（arm64 + CUDA 13）+ ComfyUI（固定 ref）+ JupyterLab + modelscope + sshd。不装 ComfyUI-Manager。
ARG BASE=nvcr.io/nvidia/pytorch:25.09-py3
FROM ${BASE}
ARG PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple
ARG COMFYUI_SRC=https://github.com/comfyanonymous/ComfyUI.git
ARG COMFYUI_REF=v0.3.60
LABEL spark.image.spec="2" spark.app.image="comfyui"
ENV PIP_INDEX_URL=${PIP_INDEX_URL} PIP_NO_CACHE_DIR=1 DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server ca-certificates curl iproute2 util-linux tar git \
 && rm -rf /var/lib/apt/lists/* \
 && rm -f /etc/ssh/ssh_host_* \
 && mkdir -p /run/sshd
RUN pip install -i ${PIP_INDEX_URL} modelscope jupyterlab
# requirements 里的 torch / torchvision / torchaudio 由 NGC 镜像提供（arm64 + CUDA 13 的轮子 PyPI 上没有），过滤掉再装
RUN git clone --depth 1 --branch ${COMFYUI_REF} ${COMFYUI_SRC} /opt/ComfyUI \
 && grep -v -E '^(torch|torchvision|torchaudio|torchsde)([=<>!~ ]|$)' /opt/ComfyUI/requirements.txt > /tmp/req.txt \
 && pip install -i ${PIP_INDEX_URL} -r /tmp/req.txt torchsde \
 && python3 /opt/ComfyUI/main.py --help >/dev/null \
 && mkdir -p /opt/ComfyUI/models/checkpoints /opt/ComfyUI/models/unet /opt/ComfyUI/models/clip /opt/ComfyUI/models/vae
```

`deploy/image/apps/pytorch/Dockerfile`：

```dockerfile
# spark/pytorch：NGC PyTorch（arm64 + CUDA 13）+ JupyterLab + modelscope + sshd。替换 images 种子里的 PyTorch 占位条目。
ARG BASE=nvcr.io/nvidia/pytorch:25.09-py3
FROM ${BASE}
ARG PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple
LABEL spark.image.spec="2" spark.app.image="pytorch"
ENV PIP_INDEX_URL=${PIP_INDEX_URL} PIP_NO_CACHE_DIR=1 DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server ca-certificates curl iproute2 util-linux tar git \
 && rm -rf /var/lib/apt/lists/* \
 && rm -f /etc/ssh/ssh_host_* \
 && mkdir -p /run/sshd
RUN pip install -i ${PIP_INDEX_URL} modelscope jupyterlab && jupyter lab --version
```

`deploy/image/apps/README.md`：三个镜像的用途、构建参数表、大陆网络的构建命令（下一步的命令原文）、「验收 / 上线用的实际 NGC 标签」一栏（本任务填写）、推到 Harbor 的命令（`docker tag spark/llm-serving:25.09 registry.spark.internal/spark/llm-serving:25.09 && docker push …`，上线前核对项）。

`Makefile` 追加（放在 `platform-image` 之后）：

```makefile
# 应用镜像（计划 7）：在一台 arm64 机器上构建；大陆网络传 PIP_INDEX_URL / COMFYUI_SRC，见 deploy/image/apps/README.md
APP_IMAGE_TAG ?= 25.09
PIP_INDEX_URL ?=
COMFYUI_SRC ?=
app-images:
	docker build -f deploy/image/apps/llm-serving/Dockerfile --build-arg BASE=nvcr.io/nvidia/vllm:$(APP_IMAGE_TAG)-py3 \
		$(if $(PIP_INDEX_URL),--build-arg PIP_INDEX_URL=$(PIP_INDEX_URL)) -t spark/llm-serving:$(APP_IMAGE_TAG) deploy/image/apps/llm-serving
	docker build -f deploy/image/apps/comfyui/Dockerfile --build-arg BASE=nvcr.io/nvidia/pytorch:$(APP_IMAGE_TAG)-py3 \
		$(if $(PIP_INDEX_URL),--build-arg PIP_INDEX_URL=$(PIP_INDEX_URL)) $(if $(COMFYUI_SRC),--build-arg COMFYUI_SRC=$(COMFYUI_SRC)) -t spark/comfyui:$(APP_IMAGE_TAG) deploy/image/apps/comfyui
	docker build -f deploy/image/apps/pytorch/Dockerfile --build-arg BASE=nvcr.io/nvidia/pytorch:$(APP_IMAGE_TAG)-py3 \
		$(if $(PIP_INDEX_URL),--build-arg PIP_INDEX_URL=$(PIP_INDEX_URL)) -t spark/pytorch:$(APP_IMAGE_TAG) deploy/image/apps/pytorch
```

（`.PHONY` 加 `app-images`。）

- [ ] **Step 3: 镜像规范增补**

`docs/image-spec.md`「必须」一节追加一条：

```markdown
- **应用镜像（一键部署，计划 7）**另须自带 `curl`、`tar`、`setsid`（`util-linux`）、`python3`、`git`，以及 `modelscope` 命令与 `jupyterlab`；`PIP_INDEX_URL` 作为构建参数并写进镜像环境（脚本若需装小包走它）。平台把应用部署包放进 `/root/data/.spark-apps/<slug>/`，用 `/bin/sh` 执行固定的 `run.sh`，应用以普通进程运行（容器内没有 init / Docker）。模型缓存目录 `/root/data/models`。参考 Dockerfile：`deploy/image/apps/{llm-serving,comfyui,pytorch}`，`LABEL spark.image.spec="2"`。
```

「参考镜像」一节末尾加：`make app-images`（或 `deploy/image/apps/README.md` 的大陆构建命令）。

- [ ] **Step 4: 在 221 上构建（大陆网络）**

把仓库的 `deploy/image/apps` 同步到测试机并构建（三个镜像加起来约 30～60 分钟，视 nvcr.io 速度；`screen` 里跑）：

```bash
rsync -a deploy/image/apps/ imac@192.168.1.221:~/spark-apps-build/
ssh imac@192.168.1.221 'cd ~/spark-apps-build && \
  docker build -f llm-serving/Dockerfile --build-arg BASE=nvcr.io/nvidia/vllm:25.09-py3 --build-arg PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple -t spark/llm-serving:25.09 llm-serving 2>&1 | tail -5 && \
  docker build -f comfyui/Dockerfile --build-arg BASE=nvcr.io/nvidia/pytorch:25.09-py3 --build-arg PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple --build-arg COMFYUI_SRC=https://gitee.com/mirrors/ComfyUI.git -t spark/comfyui:25.09 comfyui 2>&1 | tail -5 && \
  docker build -f pytorch/Dockerfile --build-arg BASE=nvcr.io/nvidia/pytorch:25.09-py3 --build-arg PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple -t spark/pytorch:25.09 pytorch 2>&1 | tail -5'
```

若 `pip install open-webui` 在 arm64 上因某个依赖没有轮子而失败：先试 `--build-arg PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple`（两个镜像站同步时间不同），再在 Dockerfile 的 venv 段前加 `apt-get install -y build-essential python3-dev`（编译轮子）；仍失败则把 Open WebUI 改为 `/opt/open-webui/bin/pip install "open-webui==<上一个能装的版本>"` 并记录。ComfyUI 的 `gitee.com/mirrors/ComfyUI` 若没有目标 tag，改 `COMFYUI_REF` 为该镜像仓库里存在的最近 tag，记录。

- [ ] **Step 5: 验证镜像（规格 §16.4 第 1 项前半）**

```bash
ssh imac@192.168.1.221 'for img in spark/llm-serving:25.09 spark/comfyui:25.09 spark/pytorch:25.09; do
  echo "== $img";
  docker run --rm --gpus all $img python3 -c "import torch; print(torch.cuda.is_available(), torch.cuda.get_device_name())";
  docker run --rm $img sh -c "command -v curl tar setsid python3 git modelscope jupyter >/dev/null && echo tools-ok; ls /etc/ssh/ssh_host_* 2>/dev/null && echo HOSTKEY-LEAK; test -x /usr/sbin/sshd && echo sshd-ok";
  docker inspect --format "entry={{.Config.Entrypoint}} cmd={{.Config.Cmd}}" $img;
done;
docker run --rm spark/llm-serving:25.09 sh -c "vllm --version && /opt/open-webui/bin/open-webui --help | head -2 && jupyter lab --version";
docker run --rm spark/comfyui:25.09 sh -c "python3 /opt/ComfyUI/main.py --help | head -3 && jupyter lab --version"'
```
Expected：每个镜像 `True NVIDIA GB10`、`tools-ok`、`sshd-ok`、没有 `HOSTKEY-LEAK`；`entry=[]`（基础镜像若自带 ENTRYPOINT——NGC 的 `nvidia_entrypoint.sh`——需在 Dockerfile 末尾加 `ENTRYPOINT []` 与 `CMD []`，然后重建；Agent 会用 `--entrypoint` 覆盖，但规范要求镜像不设入口）；版本命令都能执行。

- [ ] **Step 6: 记录与提交**

把实际 NGC 标签、ComfyUI ref、构建耗时、镜像大小（`docker images spark/*`）写进 `deploy/image/apps/README.md`「实际构建记录」。

Run: `go vet ./... && go test ./internal/appdeploy/ ./internal/platform/...`（本任务不改 Go，确认仓库仍可构建）。

```bash
git add deploy/image/apps docs/image-spec.md Makefile
git commit -m "build(images): app images llm-serving / comfyui / pytorch (NGC arm64 base, mirror build args), image spec v2

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 3: `ValidateSpec` / `ValidateEnv` / `env` 文件渲染 / `spec_hash` / 部署包生成

**Files:**
- Create: `internal/appdeploy/spec.go`、`internal/appdeploy/env.go`、`internal/appdeploy/bundle.go`
- Test: `internal/appdeploy/spec_test.go`、`internal/appdeploy/env_test.go`、`internal/appdeploy/bundle_test.go`（新建）

**Interfaces:**
- Consumes: Task 1 的 `Spec / EnvSpec / PortSpec / LinkSpec`、`apperr.AppVersionInvalid / AppEnvInvalid / AppEnvRequired`。
- Produces:
  - `func ValidateSpec(s *Spec) error`：规范化（`TimeoutMinutes` 0 → 60、`MaxLen` 0 → 256、去首尾空白）并校验规格 §3.2 全部规则；错误是 `app_version_invalid`，消息前缀是字段路径（`ports[1].port`、`env[2].default`）。
  - `func ValidateEnv(s Spec, values map[string]string) (plain, secret map[string]string, err error)`：规格 §3.3；缺省补 `default`；`secret` 值最短 8；返回拆分后的两张表（不含未设置且无默认的可选变量）。
  - `func SpecHash(s Spec, script string) string`：`sha256(canonicalJSON(s) + "\n" + script)` 十六进制；`canonicalJSON` = `json.Marshal` 后 `json.Compact` 的结果（Go 的 `encoding/json` 对 struct 按字段顺序、对 map 按键排序输出，输入 JSON 的键序不影响）。
  - `type EnvFile struct{ Reserved []EnvKV; User []EnvKV }`、`type EnvKV struct{ Name, Value string }`；`func RenderEnv(reserved, user []EnvKV) []byte`（每行 `export NAME='…'`，`'` → `'\''`）；`func ShellQuote(v string) string`。
  - `type BundleInput struct{ Slug, InstanceID, AppRoot, SpecHash string; Spec Spec; Script string; Env, Secret map[string]string; HealthAddr, HFEndpoint, PipIndexURL string }`；`func Bundle(in BundleInput, runSH []byte) ([]byte, error)`（tar，条目顺序 `run.sh`(0755) `app.sh`(0644) `spec.json`(0644) `env`(0600)，mtime 固定为 0 使输出可快照；> `MaxBundleBytes` 报错）；`func AppDir(appRoot, slug string) string`；`func ReservedEnv(in BundleInput) []EnvKV`（顺序固定：`SPARK_APP, SPARK_APP_DIR, SPARK_DATA_DIR, SPARK_PORT, SPARK_PORT_2, SPARK_INSTANCE_ID, SPARK_MODELS_DIR, SPARK_SPEC_HASH, SPARK_TIMEOUT_S, SPARK_HEALTH_PATH, SPARK_MIN_FREE_GIB, SPARK_HEALTH_ADDR, HF_ENDPOINT, MODELSCOPE_CACHE, HF_HOME, PIP_INDEX_URL`）。
  - `var reservedNames = map[string]bool{"PATH":…, "LD_PRELOAD", "LD_LIBRARY_PATH", "HOME", "SHELL", "IFS", "ENV", "BASH_ENV"}`；`var envNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)`；`var slugRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)`；`var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.-]{0,31}$`)`。

- [ ] **Step 1: 写失败的测试**

`internal/appdeploy/spec_test.go`：

```go
package appdeploy_test

import (
	"strings"
	"testing"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/platform/apperr"
)

func i64(v int64) *int64 { return &v }

func goodSpec() appdeploy.Spec {
	return appdeploy.Spec{
		Ports: []appdeploy.PortSpec{{Port: 8000, Label: "API", Path: "/v1", HealthPath: "/health"}},
		Env: []appdeploy.EnvSpec{
			{Name: "MODEL_ID", Label: "模型", Type: "enum", Required: true, Options: []string{"Qwen/Qwen3-8B", "Qwen/Qwen3-4B"}, Default: "Qwen/Qwen3-8B"},
			{Name: "MAX_MODEL_LEN", Label: "最大上下文", Type: "int", Min: i64(1024), Max: i64(131072), Default: "32768"},
			{Name: "API_KEY", Label: "接口密钥", Type: "secret", Pattern: "^[A-Za-z0-9_-]{8,128}$"},
			{Name: "NOTE", Label: "备注", Type: "string"},
			{Name: "DEBUG", Label: "调试", Type: "bool", Default: "false"},
		},
		Links: []appdeploy.LinkSpec{{Label: "文档", Path: "/docs"}},
	}
}

func TestValidateSpecMatrix(t *testing.T) {
	ok := goodSpec()
	if err := appdeploy.ValidateSpec(&ok); err != nil {
		t.Fatalf("good spec rejected: %v", err)
	}
	if ok.TimeoutMinutes != 60 || ok.Env[3].MaxLen != 256 {
		t.Fatalf("defaults not applied: %+v", ok)
	}
	cases := map[string]struct {
		mut  func(s *appdeploy.Spec)
		path string
	}{
		"no ports":        {func(s *appdeploy.Spec) { s.Ports = nil }, "ports"},
		"three ports":     {func(s *appdeploy.Spec) { s.Ports = append(s.Ports, appdeploy.PortSpec{Port: 1}, appdeploy.PortSpec{Port: 2}) }, "ports"},
		"port 22":         {func(s *appdeploy.Spec) { s.Ports[0].Port = 22 }, "ports[0].port"},
		"port 2222":       {func(s *appdeploy.Spec) { s.Ports[0].Port = 2222 }, "ports[0].port"},
		"port low":        {func(s *appdeploy.Spec) { s.Ports[0].Port = 1023 }, "ports[0].port"},
		"port dup":        {func(s *appdeploy.Spec) { s.Ports = append(s.Ports, appdeploy.PortSpec{Port: 8000}) }, "ports[1].port"},
		"path no slash":   {func(s *appdeploy.Spec) { s.Ports[0].Path = "v1" }, "ports[0].path"},
		"label long":      {func(s *appdeploy.Spec) { s.Ports[0].Label = strings.Repeat("x", 33) }, "ports[0].label"},
		"timeout high":    {func(s *appdeploy.Spec) { s.TimeoutMinutes = 181 }, "timeout_minutes"},
		"timeout neg":     {func(s *appdeploy.Spec) { s.TimeoutMinutes = -1 }, "timeout_minutes"},
		"disk high":       {func(s *appdeploy.Spec) { s.MinDataFreeGiB = 2049 }, "min_data_free_gib"},
		"env name lower":  {func(s *appdeploy.Spec) { s.Env[0].Name = "model" }, "env[0].name"},
		"env name SPARK_": {func(s *appdeploy.Spec) { s.Env[0].Name = "SPARK_X" }, "env[0].name"},
		"env name PATH":   {func(s *appdeploy.Spec) { s.Env[0].Name = "PATH" }, "env[0].name"},
		"env name LD":     {func(s *appdeploy.Spec) { s.Env[0].Name = "LD_PRELOAD" }, "env[0].name"},
		"env dup":         {func(s *appdeploy.Spec) { s.Env[3].Name = "MODEL_ID" }, "env[3].name"},
		"env type":        {func(s *appdeploy.Spec) { s.Env[0].Type = "list" }, "env[0].type"},
		"enum no options": {func(s *appdeploy.Spec) { s.Env[0].Options = nil }, "env[0].options"},
		"enum 33 options": {func(s *appdeploy.Spec) { s.Env[0].Options = make([]string, 33) }, "env[0].options"},
		"enum bad default": {func(s *appdeploy.Spec) { s.Env[0].Default = "x" }, "env[0].default"},
		"int bad default": {func(s *appdeploy.Spec) { s.Env[1].Default = "99" }, "env[1].default"},
		"int min>max":     {func(s *appdeploy.Spec) { s.Env[1].Min, s.Env[1].Max = i64(5), i64(1) }, "env[1].min"},
		"bad RE2":         {func(s *appdeploy.Spec) { s.Env[2].Pattern = "(" }, "env[2].pattern"},
		"pattern long":    {func(s *appdeploy.Spec) { s.Env[2].Pattern = strings.Repeat("a", 257) }, "env[2].pattern"},
		"max_len high":    {func(s *appdeploy.Spec) { s.Env[3].MaxLen = 1025 }, "env[3].max_len"},
		"17 env":          {func(s *appdeploy.Spec) { for i := 0; i < 12; i++ { s.Env = append(s.Env, appdeploy.EnvSpec{Name: "V" + strings.Repeat("A", i+1), Type: "string"}) } }, "env"},
		"help long":       {func(s *appdeploy.Spec) { s.Env[0].Help = strings.Repeat("x", 201) }, "env[0].help"},
		"5 links":         {func(s *appdeploy.Spec) { s.Links = make([]appdeploy.LinkSpec, 5) }, "links"},
		"link path":       {func(s *appdeploy.Spec) { s.Links[0].Path = "docs" }, "links[0].path"},
		"secret pattern allows short": {func(s *appdeploy.Spec) { s.Env[2].Pattern = "^.{1,4}$" }, "env[2].pattern"},
	}
	for name, c := range cases {
		s := goodSpec()
		c.mut(&s)
		err := appdeploy.ValidateSpec(&s)
		ae := apperr.As(err)
		if ae == nil || ae.Code != "app_version_invalid" || !strings.HasPrefix(ae.Message, c.path+"：") {
			t.Errorf("%s: err = %v, want app_version_invalid at %s", name, err, c.path)
		}
	}
}

func TestSpecHashIgnoresKeyOrder(t *testing.T) {
	a := goodSpec()
	b := goodSpec()
	b.Env = append([]appdeploy.EnvSpec(nil), a.Env...) // 相同内容
	if appdeploy.SpecHash(a, "x") != appdeploy.SpecHash(b, "x") {
		t.Fatal("same spec different hash")
	}
	if appdeploy.SpecHash(a, "x") == appdeploy.SpecHash(a, "y") {
		t.Fatal("script change not in hash")
	}
	b.Env[0].Default = "Qwen/Qwen3-4B"
	if appdeploy.SpecHash(a, "x") == appdeploy.SpecHash(b, "x") {
		t.Fatal("spec change not in hash")
	}
	if h := appdeploy.SpecHash(a, "x"); len(h) != 64 {
		t.Fatalf("hash = %q", h)
	}
}
```

`internal/appdeploy/env_test.go`：

```go
package appdeploy_test

import (
	"os/exec"
	"path/filepath"
	"os"
	"strings"
	"testing"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/platform/apperr"
)

func TestValidateEnvMatrix(t *testing.T) {
	s := goodSpec()
	if err := appdeploy.ValidateSpec(&s); err != nil {
		t.Fatal(err)
	}
	plain, secret, err := appdeploy.ValidateEnv(s, map[string]string{"API_KEY": "abcdefgh", "MAX_MODEL_LEN": "4096"})
	if err != nil {
		t.Fatal(err)
	}
	if plain["MODEL_ID"] != "Qwen/Qwen3-8B" || plain["MAX_MODEL_LEN"] != "4096" || plain["DEBUG"] != "false" || plain["NOTE"] != "" {
		t.Fatalf("plain = %v", plain)
	}
	if _, has := plain["NOTE"]; has {
		t.Fatal("unset optional var without default must be absent")
	}
	if secret["API_KEY"] != "abcdefgh" || len(secret) != 1 {
		t.Fatalf("secret = %v", secret)
	}
	if _, has := plain["API_KEY"]; has {
		t.Fatal("secret leaked into plain")
	}
	bad := map[string]struct {
		in   map[string]string
		code string
		msg  string
	}{
		"extra key":      {map[string]string{"FOO": "1"}, "app_env_invalid", "FOO"},
		"newline":        {map[string]string{"NOTE": "a\nb"}, "app_env_invalid", "备注"},
		"nul":            {map[string]string{"NOTE": "a\x00b"}, "app_env_invalid", "备注"},
		"bad utf8":       {map[string]string{"NOTE": "\xff"}, "app_env_invalid", "备注"},
		"too long":       {map[string]string{"NOTE": strings.Repeat("x", 257)}, "app_env_invalid", "备注"},
		"int not number": {map[string]string{"MAX_MODEL_LEN": "big"}, "app_env_invalid", "最大上下文"},
		"int out of range": {map[string]string{"MAX_MODEL_LEN": "1"}, "app_env_invalid", "最大上下文"},
		"bool":           {map[string]string{"DEBUG": "yes"}, "app_env_invalid", "调试"},
		"enum":           {map[string]string{"MODEL_ID": "Qwen/Other"}, "app_env_invalid", "模型"},
		"pattern":        {map[string]string{"API_KEY": "has space!"}, "app_env_invalid", "接口密钥"},
		"secret short":   {map[string]string{"API_KEY": "abc"}, "app_env_invalid", "接口密钥"},
	}
	for name, c := range bad {
		_, _, err := appdeploy.ValidateEnv(s, c.in)
		ae := apperr.As(err)
		if ae == nil || ae.Code != c.code || !strings.Contains(ae.Message, c.msg) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// required 且无默认 → app_env_required
	s2 := goodSpec()
	s2.Env[0].Default = ""
	if err := appdeploy.ValidateSpec(&s2); err != nil {
		t.Fatal(err)
	}
	_, _, err = appdeploy.ValidateEnv(s2, nil)
	if ae := apperr.As(err); ae == nil || ae.Code != "app_env_required" || !strings.Contains(ae.Message, "模型") {
		t.Fatalf("required = %v", err)
	}
}

func TestRenderEnvEscapes(t *testing.T) {
	kvs := []appdeploy.EnvKV{{Name: "NOTE", Value: `'; touch /tmp/pwned; '`}, {Name: "B", Value: "a'b$(id)`id` \\ \"q\""}}
	got := string(appdeploy.RenderEnv([]appdeploy.EnvKV{{Name: "SPARK_APP", Value: "vllm"}}, kvs))
	want := "export SPARK_APP='vllm'\nexport NOTE=''\\''; touch /tmp/pwned; '\\'''\nexport B='a'\\''b$(id)`id` \\ \"q\"'\n"
	if got != want {
		t.Fatalf("render =\n%s\nwant\n%s", got, want)
	}
	// 真用 /bin/sh 加载：值原样回来，没有执行任何命令
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(got), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "pwned")
	kvs2 := []appdeploy.EnvKV{{Name: "NOTE", Value: "'; touch " + marker + "; '"}}
	os.WriteFile(filepath.Join(dir, "env2"), appdeploy.RenderEnv(nil, kvs2), 0o600)
	cmd := exec.Command("/bin/sh", "-c", `. ./env2 && printf '%s' "$NOTE"`)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil || string(out) != kvs2[0].Value {
		t.Fatalf("sh load = %q %v", out, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("injected command executed")
	}
}
```

`internal/appdeploy/bundle_test.go`：

```go
package appdeploy_test

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"

	"spark-cloud/internal/appdeploy"
)

func TestBundleContents(t *testing.T) {
	s := goodSpec()
	if err := appdeploy.ValidateSpec(&s); err != nil {
		t.Fatal(err)
	}
	in := appdeploy.BundleInput{Slug: "vllm", InstanceID: "ins-aaaaaaaa", AppRoot: "/root/data", SpecHash: "abc", Spec: s, Script: "echo hi\n",
		Env: map[string]string{"MODEL_ID": "Qwen/Qwen3-8B"}, Secret: map[string]string{"API_KEY": "abcdefgh"},
		HFEndpoint: "https://hf-mirror.com", PipIndexURL: "https://pypi.tuna.tsinghua.edu.cn/simple"}
	b, err := appdeploy.Bundle(in, []byte("#!/bin/sh\n"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	var names []string
	files := map[string]string{}
	modes := map[string]int64{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		files[h.Name] = string(body)
		modes[h.Name] = h.Mode
	}
	if strings.Join(names, ",") != "run.sh,app.sh,spec.json,env" {
		t.Fatalf("entries = %v", names)
	}
	if modes["run.sh"] != 0o755 || modes["env"] != 0o600 || modes["app.sh"] != 0o644 {
		t.Fatalf("modes = %v", modes)
	}
	env := files["env"]
	for _, line := range []string{
		"export SPARK_APP='vllm'", "export SPARK_APP_DIR='/root/data/.spark-apps/vllm'", "export SPARK_DATA_DIR='/root/data'", "export SPARK_PORT='8000'",
		"export SPARK_PORT_2=''", "export SPARK_INSTANCE_ID='ins-aaaaaaaa'", "export SPARK_MODELS_DIR='/root/data/models'", "export SPARK_SPEC_HASH='abc'",
		"export SPARK_TIMEOUT_S='3600'", "export SPARK_HEALTH_PATH='/health'", "export SPARK_MIN_FREE_GIB='0'", "export SPARK_HEALTH_ADDR='127.0.0.1:8000'",
		"export HF_ENDPOINT='https://hf-mirror.com'", "export MODELSCOPE_CACHE='/root/data/models/modelscope'", "export HF_HOME='/root/data/models/hf'",
		"export PIP_INDEX_URL='https://pypi.tuna.tsinghua.edu.cn/simple'", "export MODEL_ID='Qwen/Qwen3-8B'", "export API_KEY='abcdefgh'",
	} {
		if !strings.Contains(env, line+"\n") {
			t.Errorf("env missing %q:\n%s", line, env)
		}
	}
	if !strings.Contains(files["spec.json"], `"port":8000`) {
		t.Fatalf("spec.json = %s", files["spec.json"])
	}
	// 可重复：同样输入同样字节（mtime 固定）
	b2, _ := appdeploy.Bundle(in, []byte("#!/bin/sh\n"))
	if !bytes.Equal(b, b2) {
		t.Fatal("bundle not deterministic")
	}
	in.Script = strings.Repeat("x", appdeploy.MaxBundleBytes)
	if _, err := appdeploy.Bundle(in, []byte("#!/bin/sh\n")); err == nil {
		t.Fatal("oversized bundle accepted")
	}
	if got := appdeploy.AppDir("/tmp/x", "open-webui"); got != "/tmp/x/.spark-apps/open-webui" {
		t.Fatalf("AppDir = %q", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/appdeploy/ 2>&1 | head`
Expected: 编译错误（`ValidateSpec` 等未定义）。

- [ ] **Step 3: `spec.go`**

```go
package appdeploy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"spark-cloud/internal/platform/apperr"
)

var (
	slugRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.-]{0,31}$`)
	envNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	// reservedNames 会影响 run.sh 自身或 sh 的行为，不允许作为用户变量。
	reservedNames = map[string]bool{"PATH": true, "LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "HOME": true, "SHELL": true, "IFS": true, "ENV": true, "BASH_ENV": true}
	envTypes      = map[string]bool{"string": true, "int": true, "bool": true, "enum": true, "secret": true}
)

func ValidSlug(s string) bool    { return slugRe.MatchString(s) }
func ValidVersion(s string) bool { return versionRe.MatchString(s) }

func invalid(path, reason string) error { return apperr.AppVersionInvalid(path, reason) }

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func validPath(p string) bool {
	return strings.HasPrefix(p, "/") && len(p) <= 128 && strings.IndexFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == ' ' }) < 0
}

// ValidateSpec 规范化并校验版本 spec（规格 §3.2）。错误码 app_version_invalid，消息以字段路径开头。
func ValidateSpec(s *Spec) error {
	if len(s.Ports) < 1 || len(s.Ports) > 2 {
		return invalid("ports", "需要 1～2 个端口")
	}
	for i := range s.Ports {
		p := &s.Ports[i]
		path := fmt.Sprintf("ports[%d]", i)
		if p.Port < 1024 || p.Port > 65535 || p.Port == 2222 {
			return invalid(path+".port", "端口必须在 1024～65535 之间且不为 22、2222")
		}
		for j := 0; j < i; j++ {
			if s.Ports[j].Port == p.Port {
				return invalid(path+".port", "端口重复")
			}
		}
		p.Label = strings.TrimSpace(p.Label)
		if runeLen(p.Label) > 32 {
			return invalid(path+".label", "最多 32 个字符")
		}
		if p.Path != "" && !validPath(p.Path) {
			return invalid(path+".path", "必须以 / 开头、不含空白，最多 128 个字符")
		}
		if p.HealthPath != "" && !validPath(p.HealthPath) {
			return invalid(path+".health_path", "必须以 / 开头、不含空白，最多 128 个字符")
		}
		if i > 0 {
			p.HealthPath = "" // 只有主端口做 HTTP 健康检查
		}
	}
	if s.TimeoutMinutes == 0 {
		s.TimeoutMinutes = DefaultTimeoutMinutes
	}
	if s.TimeoutMinutes < 1 || s.TimeoutMinutes > MaxTimeoutMinutes {
		return invalid("timeout_minutes", "需在 1～180 之间")
	}
	if s.MinDataFreeGiB < 0 || s.MinDataFreeGiB > 2048 {
		return invalid("min_data_free_gib", "需在 0～2048 之间")
	}
	if len(s.Env) > 16 {
		return invalid("env", "最多 16 个变量")
	}
	for i := range s.Env {
		if err := validateEnvSpec(&s.Env[i], fmt.Sprintf("env[%d]", i)); err != nil {
			return err
		}
		for j := 0; j < i; j++ {
			if s.Env[j].Name == s.Env[i].Name {
				return invalid(fmt.Sprintf("env[%d].name", i), "变量名重复")
			}
		}
	}
	if len(s.Links) > 4 {
		return invalid("links", "最多 4 个链接")
	}
	for i := range s.Links {
		l := &s.Links[i]
		l.Label = strings.TrimSpace(l.Label)
		if l.Label == "" || runeLen(l.Label) > 32 {
			return invalid(fmt.Sprintf("links[%d].label", i), "1～32 个字符")
		}
		if !validPath(l.Path) {
			return invalid(fmt.Sprintf("links[%d].path", i), "必须以 / 开头、不含空白，最多 128 个字符")
		}
	}
	return nil
}

func validateEnvSpec(e *EnvSpec, path string) error {
	e.Name = strings.TrimSpace(e.Name)
	e.Label = strings.TrimSpace(e.Label)
	if !envNameRe.MatchString(e.Name) || strings.HasPrefix(e.Name, "SPARK_") || reservedNames[e.Name] {
		return invalid(path+".name", "变量名须为大写字母开头的 [A-Z0-9_]，不得以 SPARK_ 开头，不得是 PATH / HOME / LD_* 等保留名")
	}
	if e.Label == "" {
		e.Label = e.Name
	}
	if runeLen(e.Label) > 32 {
		return invalid(path+".label", "最多 32 个字符")
	}
	if runeLen(e.Help) > 200 {
		return invalid(path+".help", "最多 200 个字符")
	}
	if !envTypes[e.Type] {
		return invalid(path+".type", "类型只能是 string / int / bool / enum / secret")
	}
	if e.MaxLen == 0 {
		e.MaxLen = 256
	}
	if e.MaxLen < 1 || e.MaxLen > 1024 {
		return invalid(path+".max_len", "需在 1～1024 之间")
	}
	switch e.Type {
	case "enum":
		if len(e.Options) < 1 || len(e.Options) > 32 {
			return invalid(path+".options", "enum 需要 1～32 个选项")
		}
		for _, o := range e.Options {
			if o == "" || len(o) > 128 {
				return invalid(path+".options", "每个选项 1～128 字节")
			}
		}
	case "int":
		if e.Min != nil && e.Max != nil && *e.Min > *e.Max {
			return invalid(path+".min", "min 不能大于 max")
		}
	case "string", "secret":
		if len(e.Pattern) > 256 {
			return invalid(path+".pattern", "最多 256 个字符")
		}
		if e.Pattern != "" {
			re, err := regexp.Compile(e.Pattern)
			if err != nil {
				return invalid(path+".pattern", "不是合法的 RE2 正则："+err.Error())
			}
			// secret 最短 8：pattern 不能放行更短的值（校验层对 secret 另有长度下限，这里提前拒绝自相矛盾的配置）
			if e.Type == "secret" && re.MatchString("abcd") {
				return invalid(path+".pattern", "secret 的正则不得允许少于 8 个字符的值")
			}
		}
	}
	if e.Default != "" {
		if reason := checkValue(*e, e.Default); reason != "" {
			return invalid(path+".default", "默认值不符合自身规则："+reason)
		}
	}
	return nil
}

// checkValue 按类型校验单个值；返回空串表示合法，否则返回原因。
func checkValue(e EnvSpec, v string) string {
	if !utf8.ValidString(v) {
		return "不是合法的 UTF-8"
	}
	if strings.ContainsAny(v, "\n\r\x00") {
		return "不能包含换行或 NUL"
	}
	if len(v) > e.MaxLen {
		return fmt.Sprintf("最多 %d 字节", e.MaxLen)
	}
	switch e.Type {
	case "int":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return "必须是整数"
		}
		if (e.Min != nil && n < *e.Min) || (e.Max != nil && n > *e.Max) {
			return "超出允许范围"
		}
	case "bool":
		if v != "true" && v != "false" {
			return "只能是 true 或 false"
		}
	case "enum":
		found := false
		for _, o := range e.Options {
			found = found || o == v
		}
		if !found {
			return "不在可选项内"
		}
	case "secret":
		if len(v) < MinSecretLen {
			return fmt.Sprintf("至少 %d 个字符", MinSecretLen)
		}
		fallthrough
	case "string":
		if e.Pattern != "" && !regexp.MustCompile(e.Pattern).MatchString(v) {
			return "格式不正确"
		}
	}
	return ""
}

// ValidateEnv 校验用户提供的变量值（规格 §3.3），补默认值，按 type 拆成普通 / 敏感两张表。spec 须已通过 ValidateSpec。
func ValidateEnv(s Spec, values map[string]string) (plain, secret map[string]string, err error) {
	known := map[string]EnvSpec{}
	for _, e := range s.Env {
		known[e.Name] = e
	}
	for name := range values {
		if _, ok := known[name]; !ok {
			return nil, nil, apperr.AppEnvInvalid(name, "不是该应用声明的变量")
		}
	}
	plain, secret = map[string]string{}, map[string]string{}
	for _, e := range s.Env {
		v, set := values[e.Name]
		if !set || v == "" {
			if e.Default != "" {
				v, set = e.Default, true
			} else if e.Required {
				return nil, nil, apperr.AppEnvRequired(e.Label)
			} else {
				continue
			}
		}
		if reason := checkValue(e, v); reason != "" {
			return nil, nil, apperr.AppEnvInvalid(e.Label, reason)
		}
		if e.Type == "secret" {
			secret[e.Name] = v
		} else {
			plain[e.Name] = v
		}
	}
	return plain, secret, nil
}

// SpecHash = sha256(canonical(spec) || "\n" || script) 的十六进制。encoding/json 对 struct 按字段顺序、map 按键排序，输入键序不影响。
func SpecHash(s Spec, script string) string {
	b, _ := json.Marshal(s)
	h := sha256.New()
	h.Write(b)
	h.Write([]byte("\n"))
	h.Write([]byte(script))
	return hex.EncodeToString(h.Sum(nil))
}
```

- [ ] **Step 4: `env.go` 与 `bundle.go`

`internal/appdeploy/env.go`：

```go
package appdeploy

import (
	"sort"
	"strings"
)

type EnvKV struct{ Name, Value string }

// ShellQuote 单引号转义：' → '\''。值已由 ValidateEnv 保证不含换行 / NUL。
func ShellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

// RenderEnv 生成 env 文件：先平台保留变量（顺序固定），再用户变量；每行 export NAME='value'。
func RenderEnv(reserved, user []EnvKV) []byte {
	var b strings.Builder
	for _, kv := range reserved {
		b.WriteString("export " + kv.Name + "=" + ShellQuote(kv.Value) + "\n")
	}
	for _, kv := range user {
		b.WriteString("export " + kv.Name + "=" + ShellQuote(kv.Value) + "\n")
	}
	return []byte(b.String())
}

// sortedKV 把 map 变成按名字排序的切片（输出稳定）。
func sortedKV(m map[string]string) []EnvKV {
	out := make([]EnvKV, 0, len(m))
	for k, v := range m {
		out = append(out, EnvKV{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
```

`internal/appdeploy/bundle.go`：

```go
package appdeploy

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
)

type BundleInput struct {
	Slug, InstanceID, AppRoot, SpecHash string
	Spec                                Spec
	Script                              string
	Env, Secret                         map[string]string
	HealthAddr                          string // 空 = 127.0.0.1:<主端口>
	HFEndpoint, PipIndexURL             string
}

func AppDir(appRoot, slug string) string { return path.Join(appRoot, AppDirName, slug) }

// ReservedEnv 平台注入的保留变量（顺序固定，写在用户变量之前）。
func ReservedEnv(in BundleInput) []EnvKV {
	models := path.Join(in.AppRoot, ModelsDirName)
	port2 := ""
	if len(in.Spec.Ports) > 1 {
		port2 = strconv.Itoa(in.Spec.Ports[1].Port)
	}
	health := in.HealthAddr
	if health == "" {
		health = "127.0.0.1:" + strconv.Itoa(in.Spec.Ports[0].Port)
	}
	return []EnvKV{
		{"SPARK_APP", in.Slug}, {"SPARK_APP_DIR", AppDir(in.AppRoot, in.Slug)}, {"SPARK_DATA_DIR", in.AppRoot},
		{"SPARK_PORT", strconv.Itoa(in.Spec.Ports[0].Port)}, {"SPARK_PORT_2", port2}, {"SPARK_INSTANCE_ID", in.InstanceID},
		{"SPARK_MODELS_DIR", models}, {"SPARK_SPEC_HASH", in.SpecHash}, {"SPARK_TIMEOUT_S", strconv.Itoa(in.Spec.TimeoutMinutes * 60)},
		{"SPARK_HEALTH_PATH", in.Spec.Ports[0].HealthPath}, {"SPARK_MIN_FREE_GIB", strconv.Itoa(in.Spec.MinDataFreeGiB)}, {"SPARK_HEALTH_ADDR", health},
		{"HF_ENDPOINT", in.HFEndpoint}, {"MODELSCOPE_CACHE", path.Join(models, "modelscope")}, {"HF_HOME", path.Join(models, "hf")}, {"PIP_INDEX_URL", in.PipIndexURL},
	}
}

// Bundle 生成部署包 tar（run.sh 0755、app.sh 0644、spec.json 0644、env 0600；mtime 固定为 0，输出可重复）。
func Bundle(in BundleInput, runSH []byte) ([]byte, error) {
	if !ValidSlug(in.Slug) {
		return nil, errors.New("appdeploy: bad slug")
	}
	specJSON, err := json.Marshal(in.Spec)
	if err != nil {
		return nil, err
	}
	user := append(sortedKV(in.Env), sortedKV(in.Secret)...)
	env := RenderEnv(ReservedEnv(in), user)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		mode int64
		body []byte
	}{{"run.sh", 0o755, runSH}, {"app.sh", 0o644, []byte(in.Script)}, {"spec.json", 0o644, specJSON}, {"env", 0o600, env}} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Uid: 0, Gid: 0, Format: tar.FormatPAX}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > MaxBundleBytes {
		return nil, fmt.Errorf("appdeploy: 部署包 %d 字节超过上限 %d", buf.Len(), MaxBundleBytes)
	}
	return buf.Bytes(), nil
}

// UploadCommand 上传部署包的固定命令；dir 只由 AppRoot 与 slug 拼出，命令行里没有任何用户输入。
func UploadCommand(dir string) string {
	return "mkdir -p " + dir + " && chmod 700 " + dir + " && tar -x -C " + dir + " -f - && chmod 600 " + dir + "/env"
}
```

（`tar.Header` 不设 `ModTime` → 零值 → PAX 格式写 0；`FormatPAX` 保证两次输出字节一致。）

- [ ] **Step 5: 运行测试并提交**

Run: `go vet ./internal/appdeploy/ && go test ./internal/appdeploy/ -run 'TestValidateSpecMatrix|TestSpecHashIgnoresKeyOrder|TestValidateEnvMatrix|TestRenderEnvEscapes|TestBundleContents' -v 2>&1 | tail -15`
Expected: 5 个测试 PASS。

```bash
git add internal/appdeploy
git commit -m "feat(appdeploy): spec / env validation, single-quote escaping, spec hash, deploy bundle

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 4: 容器内运行时 `run.sh`（embed）+ `spark_status`，`sh` 测试

**Files:**
- Create: `internal/appdeploy/runtime/run.sh`、`internal/appdeploy/runtime/runtime.go`（`//go:embed run.sh`，`var RunSH []byte`）、`internal/appdeploy/runtime/run_test.go`、`internal/appdeploy/runtime/testdata/app_ok.sh`、`internal/appdeploy/runtime/testdata/app_fail.sh`

**Interfaces:**
- Consumes: Task 3 的 `env` 文件约定（`SPARK_APP_DIR / SPARK_DATA_DIR / SPARK_PORT / SPARK_SPEC_HASH / SPARK_TIMEOUT_S / SPARK_HEALTH_PATH / SPARK_HEALTH_ADDR / SPARK_MIN_FREE_GIB`）。
- Produces: `runtime.RunSH`；`run.sh` 子命令 `deploy | start | stop | status | remove | _bg deploy`；`status.json` 格式 `{"seq":N,"state":"installing|running|stopped|failed","phase":"...","progress":N,"message":"...","pid":<pgid>,"service_pid":N,"spec_hash":"...","exit_code":N,"at":"<ISO8601 UTC>"}`；`status` 输出三行：`status.json` 原文一行、`alive=<0|1>`、`log_size=<字节>`；`app.sh` 可调用的函数 `spark_status <phase> <progress> <message>`（`run.sh` 在执行 `app.sh` 前把它 `export` 成环境里的函数不可移植——改为写成可执行文件 `$SPARK_APP_DIR/spark_status`，`app.sh` 直接调用 `spark_status download 42 "…"`，`PATH` 前置 `$SPARK_APP_DIR`）。测试覆盖变量：`SPARK_TEST_DF_FREE_GIB`（覆盖 `df` 的剩余 GiB）。
- 退出码：`deploy` 前台 0 = 已后台启动；`2` = prepare 失败（磁盘 / `sh -n`），同时写 `status.json{state: failed, message}`；`status` 对缺失的 `status.json` 返回 3；`remove` 恒 0。

- [ ] **Step 1: 写失败的测试**

`internal/appdeploy/runtime/testdata/app_ok.sh`（fake 应用：install 汇报进度并写标记、start 起 `sleep` 写 `service.pid`）：

```sh
#!/bin/sh
set -e
case "$1" in
  install)
    spark_status download 50 "downloading $MODEL_ID"
    sleep "${INSTALL_SLEEP:-0}"
    echo "installed $MODEL_ID" > "$SPARK_APP_DIR/installed.marker"
    ;;
  start)
    sleep 100000 </dev/null >/dev/null 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    echo "started pid $!"
    ;;
  stop) ;;
esac
```

`testdata/app_fail.sh`：

```sh
#!/bin/sh
case "$1" in
  install) echo "boom: cannot download" >&2; exit 3 ;;
  start) echo "should not start"; exit 1 ;;
esac
```

`internal/appdeploy/runtime/run_test.go`：

```go
package runtime_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/appdeploy/runtime"
)

type box struct {
	t      *testing.T
	root   string // 充当容器 /root/data
	dir    string // <root>/.spark-apps/testapp
	health *httptest.Server
}

func newBox(t *testing.T, app string, extra map[string]string) *box {
	t.Helper()
	root := t.TempDir()
	dir := appdeploy.AppDir(root, "testapp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, "ok")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(hs.Close)
	os.WriteFile(filepath.Join(dir, "run.sh"), runtime.RunSH, 0o755)
	src, _ := os.ReadFile(filepath.Join("testdata", app))
	os.WriteFile(filepath.Join(dir, "app.sh"), src, 0o644)
	spec := appdeploy.Spec{Ports: []appdeploy.PortSpec{{Port: 18000, HealthPath: "/health"}}, TimeoutMinutes: 1}
	in := appdeploy.BundleInput{Slug: "testapp", InstanceID: "ins-test", AppRoot: root, SpecHash: "hash-1", Spec: spec,
		Env: map[string]string{"MODEL_ID": "Qwen/Qwen3-4B"}, HealthAddr: strings.TrimPrefix(hs.URL, "http://"), HFEndpoint: "https://hf-mirror.com", PipIndexURL: "https://pypi.tuna.tsinghua.edu.cn/simple"}
	for k, v := range extra {
		in.Env[k] = v
	}
	env := appdeploy.RenderEnv(appdeploy.ReservedEnv(in), []appdeploy.EnvKV{{Name: "MODEL_ID", Value: in.Env["MODEL_ID"]}})
	for k, v := range extra {
		env = append(env, []byte("export "+k+"="+appdeploy.ShellQuote(v)+"\n")...)
	}
	os.WriteFile(filepath.Join(dir, "env"), env, 0o600)
	b := &box{t: t, root: root, dir: dir, health: hs}
	t.Cleanup(func() { b.run("stop") })
	return b
}

func (b *box) setEnv(name, value string) {
	p := filepath.Join(b.dir, "env")
	data, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var out []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "export "+name+"=") {
			out = append(out, l)
		}
	}
	out = append(out, "export "+name+"="+appdeploy.ShellQuote(value))
	os.WriteFile(p, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

func (b *box) run(args ...string) (string, int) {
	b.t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"run.sh"}, args...)...)
	cmd.Dir = b.dir
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		b.t.Fatalf("run %v: %v", args, err)
	}
	return string(out), code
}

type status struct {
	Seq        int64  `json:"seq"`
	State      string `json:"state"`
	Phase      string `json:"phase"`
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
	Pid        int    `json:"pid"`
	ServicePid int    `json:"service_pid"`
	SpecHash   string `json:"spec_hash"`
	ExitCode   int    `json:"exit_code"`
}

func (b *box) status() (status, string) {
	b.t.Helper()
	out, _ := b.run("status")
	lines := strings.SplitN(out, "\n", 3)
	var st status
	if err := json.Unmarshal([]byte(lines[0]), &st); err != nil {
		b.t.Fatalf("status.json %q: %v", lines[0], err)
	}
	return st, out
}

func (b *box) waitState(want string) status {
	b.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, _ := b.status()
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("state = %+v, want %s; log:\n%s", st, want, b.log())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (b *box) log() string {
	data, _ := os.ReadFile(filepath.Join(b.dir, "log"))
	return string(data)
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func TestRunShSyntax(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-n")
	cmd.Stdin = strings.NewReader(string(runtime.RunSH))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	for _, f := range []string{"testdata/app_ok.sh", "testdata/app_fail.sh"} {
		if out, err := exec.Command("/bin/sh", "-n", f).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", f, err, out)
		}
	}
}

func TestRunShDeployInstallsStartsAndBecomesRunning(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	start := time.Now()
	out, code := b.run("deploy")
	if code != 0 || time.Since(start) > 3*time.Second {
		t.Fatalf("deploy = %d %q after %v (must return immediately)", code, out, time.Since(start))
	}
	st := b.waitState("running")
	if st.Phase != "health" || st.Progress != 100 || st.SpecHash != "hash-1" || st.Seq < 3 || !alive(st.ServicePid) {
		t.Fatalf("running status = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "installed.marker")); err != nil {
		t.Fatal("install not executed")
	}
	if got, _ := os.ReadFile(filepath.Join(b.dir, ".installed")); string(got) != "hash-1\n" {
		t.Fatalf(".installed = %q", got)
	}
	log := b.log()
	for _, s := range []string{"==== deploy #1 ", "downloading Qwen/Qwen3-4B", "started pid"} {
		if !strings.Contains(log, s) {
			t.Fatalf("log missing %q:\n%s", s, log)
		}
	}
	_, statusOut := b.status()
	if !strings.Contains(statusOut, "\nalive=1\n") || !strings.Contains(statusOut, "\nlog_size=") {
		t.Fatalf("status output = %q", statusOut)
	}
}

func TestRunShRedeploySkipsInstall(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	b.run("deploy")
	first := b.waitState("running")
	os.Remove(filepath.Join(b.dir, "installed.marker"))
	b.run("deploy")
	b.waitState("installing")
	second := b.waitState("running")
	if second.ServicePid == first.ServicePid || alive(first.ServicePid) {
		t.Fatalf("old service not replaced: first=%d second=%d", first.ServicePid, second.ServicePid)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "installed.marker")); err == nil {
		t.Fatal("install re-executed although .installed matches")
	}
	if !strings.Contains(b.log(), "skip install") {
		t.Fatalf("log:\n%s", b.log())
	}
	// spec_hash 变化 → 重跑 install
	b.setEnv("SPARK_SPEC_HASH", "hash-2")
	b.run("deploy")
	b.waitState("installing")
	third := b.waitState("running")
	if _, err := os.Stat(filepath.Join(b.dir, "installed.marker")); err != nil || third.SpecHash != "hash-2" {
		t.Fatalf("install not re-run for new hash: %+v", third)
	}
}

func TestRunShInstallFailure(t *testing.T) {
	b := newBox(t, "app_fail.sh", nil)
	b.run("deploy")
	st := b.waitState("failed")
	if st.ExitCode != 3 || !strings.Contains(b.log(), "boom: cannot download") {
		t.Fatalf("failed status = %+v log=%s", st, b.log())
	}
	if _, err := os.Stat(filepath.Join(b.dir, ".installed")); err == nil {
		t.Fatal(".installed written after failed install")
	}
}

func TestRunShTimeoutAndStop(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	b.setEnv("SPARK_HEALTH_ADDR", "127.0.0.1:1") // 健康检查永远不过
	b.setEnv("SPARK_TIMEOUT_S", "2")
	b.run("deploy")
	st := b.waitState("failed")
	if st.Phase != "health" || !strings.Contains(st.Message, "timeout") {
		t.Fatalf("timeout status = %+v", st)
	}
	for deadline := time.Now().Add(5 * time.Second); alive(st.ServicePid); { // 状态先写、服务随后被杀
		if time.Now().After(deadline) {
			t.Fatalf("service %d still alive after timeout", st.ServicePid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// stop 杀整个进程组：start 里起的 sleep 也没了
	b2 := newBox(t, "app_ok.sh", nil)
	b2.run("deploy")
	running := b2.waitState("running")
	if _, code := b2.run("stop"); code != 0 {
		t.Fatalf("stop = %d", code)
	}
	st2, out := b2.status()
	if st2.State != "stopped" || alive(running.ServicePid) || !strings.Contains(out, "\nalive=0\n") {
		t.Fatalf("after stop = %+v alive=%v", st2, alive(running.ServicePid))
	}
}

func TestRunShDiskCheckAndRemove(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	b.setEnv("SPARK_MIN_FREE_GIB", "100")
	b.setEnv("SPARK_TEST_DF_FREE_GIB", "5")
	out, code := b.run("deploy")
	st, _ := b.status()
	if code != 2 || st.State != "failed" || !strings.Contains(st.Message, "5") || !strings.Contains(out, "disk") {
		t.Fatalf("disk check: code=%d out=%q status=%+v", code, out, st)
	}
	if _, code := b.run("remove"); code != 0 {
		t.Fatalf("remove = %d", code)
	}
	if _, err := os.Stat(b.dir); err == nil {
		t.Fatal("dir still exists after remove")
	}
	// 对不存在的目录再 remove：仍 0
	cmd := exec.Command("/bin/sh", "-c", "sh run.sh remove")
	cmd.Dir = b.root
	_ = cmd.Run()
	if _, err := os.Stat(filepath.Join(b.root, appdeploy.AppDirName)); err == nil {
		// 父目录可存在，只要应用目录不在
	}
	// status 对缺失状态文件返回 3
	dir2 := appdeploy.AppDir(t.TempDir(), "x")
	os.MkdirAll(dir2, 0o700)
	os.WriteFile(filepath.Join(dir2, "run.sh"), runtime.RunSH, 0o755)
	cmd = exec.Command("/bin/sh", "run.sh", "status")
	cmd.Dir = dir2
	if err := cmd.Run(); err == nil || err.(*exec.ExitError).ExitCode() != 3 {
		t.Fatalf("status without file = %v", err)
	}
}

func TestRunShTCPHealthWhenNoPath(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	b.setEnv("SPARK_HEALTH_PATH", "")
	b.setEnv("SPARK_HEALTH_ADDR", "127.0.0.1:"+strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	b.run("deploy")
	b.waitState("running")
}

func TestRunShEnvIsNotEvaluated(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	b := newBox(t, "app_ok.sh", map[string]string{"NOTE": "'; touch " + marker + "; '"})
	b.run("deploy")
	b.waitState("running")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("env value was executed")
	}
}

func TestSparkStatusAtomicAndMonotonic(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	b.run("deploy")
	b.waitState("running")
	var last int64
	for i := 0; i < 20; i++ {
		cmd := exec.Command("/bin/sh", "-c", ". ./env; PATH=$SPARK_APP_DIR:$PATH spark_status install "+strconv.Itoa(i)+" \"step $i\"")
		cmd.Dir = b.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("spark_status: %v %s", err, out)
		}
		st, _ := b.status()
		if st.Seq <= last || st.Progress != i || st.Phase != "install" {
			t.Fatalf("seq not monotonic: %+v (last %d)", st, last)
		}
		last = st.Seq
	}
	if entries, _ := filepath.Glob(filepath.Join(b.dir, "status.json.*")); len(entries) != 0 {
		t.Fatalf("temp files left: %v", entries)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/appdeploy/runtime/ 2>&1 | head`
Expected: 编译错误（包不存在）。

- [ ] **Step 3: `run.sh`**

`internal/appdeploy/runtime/runtime.go`：

```go
// Package runtime 内嵌容器内的固定包装脚本 run.sh（随 platform 二进制发布，版本脚本 app.sh 只做 install / start / stop）。
package runtime

import _ "embed"

//go:embed run.sh
var RunSH []byte
```

`internal/appdeploy/runtime/run.sh`：

```sh
#!/bin/sh
# Spark Cloud 应用运行时（平台内置，随部署包下发；用户不要修改，重新部署会覆盖）。
# 用法：sh run.sh deploy|start|stop|status|remove
# 约定：本文件与 app.sh / spec.json / env 同目录；状态写 status.json（原子），输出追加到 log；
#       app.sh 的子命令 install / start / stop，start 必须把服务放后台并把 PID 写进 service.pid。
set -u
cd "$(dirname "$0")" || exit 1
DIR=$(pwd)
SEQ_FILE="$DIR/.seq"

load_env() {
  # shellcheck disable=SC1091
  [ -f "$DIR/env" ] && . "$DIR/env"
  : "${SPARK_APP_DIR:=$DIR}" "${SPARK_DATA_DIR:=/root/data}" "${SPARK_PORT:=0}" "${SPARK_SPEC_HASH:=}" "${SPARK_TIMEOUT_S:=3600}"
  : "${SPARK_HEALTH_PATH:=}" "${SPARK_HEALTH_ADDR:=127.0.0.1:$SPARK_PORT}" "${SPARK_MIN_FREE_GIB:=0}"
  export PATH="$DIR:$PATH"
}

now_iso() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# JSON 字符串转义：反斜杠、双引号；控制字符直接删掉（BSD sed 不认 \t，所以用 tr）
json_escape() { printf '%s' "$1" | tr -d '\n\r\t' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

# write_status state phase progress message [service_pid] [exit_code]
write_status() {
  _seq=$(cat "$SEQ_FILE" 2>/dev/null || echo 0)
  _seq=$((_seq + 1))
  echo "$_seq" > "$SEQ_FILE"
  _spid=${5:-$(cat "$DIR/service.pid" 2>/dev/null || echo 0)}
  _pgid=${SPARK_PGID:-$(cat "$DIR/.pgid" 2>/dev/null || echo 0)}
  _tmp="$DIR/status.json.$$"
  printf '{"seq":%s,"state":"%s","phase":"%s","progress":%s,"message":"%s","pid":%s,"service_pid":%s,"spec_hash":"%s","exit_code":%s,"at":"%s"}\n' \
    "$_seq" "$1" "$2" "$3" "$(json_escape "$4")" "$_pgid" "${_spid:-0}" "$(json_escape "$SPARK_SPEC_HASH")" "${6:-0}" "$(now_iso)" > "$_tmp"
  mv -f "$_tmp" "$DIR/status.json"
}

# 给 app.sh 用的进度汇报命令：spark_status <phase> <progress> <message>
install_spark_status() {
  cat > "$DIR/spark_status" <<'EOS'
#!/bin/sh
# 由 run.sh 生成：spark_status <phase> <progress> <message>，原子更新同目录 status.json（state 保持 installing）
DIR=$(dirname "$0"); [ -n "${SPARK_APP_DIR:-}" ] && DIR=$SPARK_APP_DIR
exec sh "$DIR/run.sh" _status "$1" "$2" "$3"
EOS
  chmod 755 "$DIR/spark_status"
}

free_gib() {
  if [ -n "${SPARK_TEST_DF_FREE_GIB:-}" ]; then echo "$SPARK_TEST_DF_FREE_GIB"; return; fi
  _kb=$(df -Pk "$SPARK_DATA_DIR" 2>/dev/null | awk 'NR==2 {print $4}')
  echo $(( ${_kb:-0} / 1024 / 1024 ))
}

detach() {
  # 脱离当前 SSH 会话：优先 setsid（util-linux），其次 python3 的 os.setsid，最后普通后台（会随会话结束被 HUP，只在无 setsid 的开发机出现）
  if command -v setsid >/dev/null 2>&1; then
    setsid "$@" </dev/null >>"$DIR/log" 2>&1 &
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c 'import os,sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' "$@" </dev/null >>"$DIR/log" 2>&1 &
  else
    echo "warn: no setsid/python3, running without session detach" >> "$DIR/log"
    nohup "$@" </dev/null >>"$DIR/log" 2>&1 &
  fi
  echo $!
}

kill_group() { # kill_group <pgid> ：TERM，10 秒后 KILL
  [ "${1:-0}" -gt 1 ] 2>/dev/null || return 0
  kill -TERM -- "-$1" 2>/dev/null || return 0
  _i=0
  while [ $_i -lt 100 ]; do
    kill -0 -- "-$1" 2>/dev/null || return 0
    sleep 0.1; _i=$((_i + 1))
  done
  kill -KILL -- "-$1" 2>/dev/null || true
}

tcp_probe() { # tcp_probe host:port
  _h=${1%:*}; _p=${1##*:}
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import socket,sys; s=socket.socket(); s.settimeout(2); s.connect((sys.argv[1], int(sys.argv[2]))); s.close()' "$_h" "$_p" 2>/dev/null
  else
    curl -s -o /dev/null --connect-timeout 2 --max-time 3 "telnet://$1" </dev/null; _rc=$?; [ $_rc -eq 0 ] || [ $_rc -eq 28 ]
  fi
}

health_ok() {
  if [ -n "$SPARK_HEALTH_PATH" ]; then
    curl -sf -o /dev/null --connect-timeout 3 --max-time 10 "http://$SPARK_HEALTH_ADDR$SPARK_HEALTH_PATH"
  else
    tcp_probe "$SPARK_HEALTH_ADDR"
  fi
}

cmd_prepare() {
  _free=$(free_gib)
  if [ "$_free" -lt "$SPARK_MIN_FREE_GIB" ]; then
    write_status failed prepare 0 "disk: 数据盘剩余 ${_free}GiB，需要 ${SPARK_MIN_FREE_GIB}GiB" 0 2
    echo "disk: free ${_free}GiB < ${SPARK_MIN_FREE_GIB}GiB" >&2
    return 2
  fi
  if ! _err=$(sh -n "$DIR/app.sh" 2>&1); then
    write_status failed prepare 0 "app.sh 语法错误: $_err" 0 2
    echo "syntax: $_err" >&2
    return 2
  fi
  install_spark_status
  return 0
}

cmd_deploy() {
  load_env
  cmd_prepare || return $?
  # 旧的部署 / 服务先停掉（幂等：没有也不报错）
  _old=$(cat "$DIR/.pgid" 2>/dev/null || echo 0)
  kill_group "$_old"
  _n=$(cat "$DIR/.attempt" 2>/dev/null || echo 0); _n=$((_n + 1)); echo "$_n" > "$DIR/.attempt"
  echo "==== deploy #$_n $(now_iso) ====" >> "$DIR/log"
  write_status installing prepare 0 "starting" 0 0
  _pid=$(detach sh "$DIR/run.sh" _bg deploy)
  echo "started background deploy pid $_pid"
  return 0
}

cmd_bg_deploy() {
  load_env
  SPARK_PGID=$(ps -o pgid= -p $$ 2>/dev/null | tr -d ' '); export SPARK_PGID
  echo "${SPARK_PGID:-0}" > "$DIR/.pgid"
  trap 'write_status failed "${PHASE:-prepare}" 0 "interrupted" 0 143; exit 143' TERM INT
  PHASE=prepare
  write_status installing prepare 1 "prepare"
  _installed=$(cat "$DIR/.installed" 2>/dev/null || echo "")
  if [ "$_installed" = "$SPARK_SPEC_HASH" ] && [ -n "$SPARK_SPEC_HASH" ]; then
    echo "skip install: spec_hash $SPARK_SPEC_HASH already installed"
  else
    PHASE=install
    write_status installing install 5 "install"
    rm -f "$DIR/.installed"
    if ! (cd "$DIR" && sh "$DIR/app.sh" install); then
      _rc=$?
      write_status failed "$(sed -n 's/.*"phase":"\([a-z]*\)".*/\1/p' "$DIR/status.json")" 0 "install 失败，退出码 $_rc" 0 "$_rc"
      exit "$_rc"
    fi
    echo "$SPARK_SPEC_HASH" > "$DIR/.installed"
  fi
  PHASE=start
  write_status installing start 80 "start"
  rm -f "$DIR/service.pid"
  if ! (cd "$DIR" && sh "$DIR/app.sh" start); then
    _rc=$?
    write_status failed start 80 "start 失败，退出码 $_rc" 0 "$_rc"
    exit "$_rc"
  fi
  _spid=$(cat "$DIR/service.pid" 2>/dev/null || echo 0)
  if [ "${_spid:-0}" -le 0 ] 2>/dev/null; then
    write_status failed start 80 "app.sh start 没有写 service.pid" 0 1
    exit 1
  fi
  PHASE=health
  write_status installing health 90 "health check" "$_spid"
  _deadline=$(( $(date +%s) + SPARK_TIMEOUT_S ))
  while :; do
    if health_ok; then
      write_status running health 100 "healthy" "$_spid"
      exit 0
    fi
    if ! kill -0 "$_spid" 2>/dev/null; then
      write_status failed health 90 "服务进程已退出" "$_spid" 1
      exit 1
    fi
    if [ "$(date +%s)" -ge "$_deadline" ]; then
      echo "health check timeout after ${SPARK_TIMEOUT_S}s" >> "$DIR/log"
      # 先写状态再杀服务：不能 kill 整个进程组（本进程也在组里，trap 会把状态改写成 interrupted）；
      # 同组里残留的子进程（tail 等）由平台随后下发的 run.sh stop 的 kill_group 收尾
      write_status failed health 90 "timeout: 健康检查 ${SPARK_TIMEOUT_S} 秒内未通过" "$_spid" 124
      trap - TERM INT
      kill -TERM "$_spid" 2>/dev/null || true
      sleep 1
      kill -KILL "$_spid" 2>/dev/null || true
      exit 124
    fi
    sleep 5
  done
}

cmd_stop() {
  load_env
  _pgid=$(cat "$DIR/.pgid" 2>/dev/null || echo 0)
  _spid=$(cat "$DIR/service.pid" 2>/dev/null || echo 0)
  if [ -f "$DIR/app.sh" ] && grep -q '^ *stop)' "$DIR/app.sh" 2>/dev/null; then
    (cd "$DIR" && sh "$DIR/app.sh" stop) >> "$DIR/log" 2>&1 || true
  fi
  kill_group "$_pgid"
  [ "${_spid:-0}" -gt 1 ] 2>/dev/null && kill -KILL "$_spid" 2>/dev/null
  write_status stopped "" 0 "stopped" "${_spid:-0}" 0
  return 0
}

cmd_status() {
  [ -f "$DIR/status.json" ] || { echo '{"error":"no status"}'; echo "alive=0"; echo "log_size=0"; return 3; }
  cat "$DIR/status.json"
  _spid=$(cat "$DIR/service.pid" 2>/dev/null || echo 0)
  if [ "${_spid:-0}" -gt 1 ] 2>/dev/null && kill -0 "$_spid" 2>/dev/null; then echo "alive=1"; else echo "alive=0"; fi
  echo "log_size=$(wc -c < "$DIR/log" 2>/dev/null | tr -d ' ' || echo 0)"
  return 0
}

cmd_remove() {
  cmd_stop >/dev/null 2>&1 || true
  cd / && rm -rf "$DIR"
  return 0
}

case "${1:-}" in
  deploy|start) cmd_deploy ;;
  _bg) shift; [ "${1:-}" = "deploy" ] && cmd_bg_deploy ;;
  _status) load_env; write_status installing "$2" "$3" "$4" ;;
  stop) cmd_stop ;;
  status) load_env; cmd_status ;;
  remove) load_env; cmd_remove ;;
  *) echo "usage: run.sh deploy|start|stop|status|remove" >&2; exit 64 ;;
esac
```

要点（实施时核对）：
- `set -e` 不在全局打开（`$(...)` 与条件表达式里会误杀），每条失败路径显式判断；`_bg` 用 `trap` 把 TERM 写成 `failed`。
- `sleep 0.1`：dash 与 macOS 的 `sleep` 都接受小数；Ubuntu 镜像的 `sleep` 来自 coreutils，同样接受。
- `status.json` 的 `pid` 是进程组 ID；`stop` 用 `kill -- -<pgid>`，所以 `_bg` 必须是会话 / 进程组首进程（`detach` 保证）。
- `.seq` 单独保存，`spark_status`（`_status`）与 `_bg` 共用，`seq` 只增。
- 测试断言 `deploy` 的退出码 2 带 `disk` 字样：`cmd_prepare` 往 stderr 写 `disk: ...`。

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/appdeploy/runtime/ -v 2>&1 | tail -25`
Expected: 9 个测试 PASS（在 macOS 上 `detach` 走 python3 分支；若本机没有 `python3`，`TestRunShTimeoutAndStop` 的进程组杀法会退化——此时安装 Xcode CLT 后重跑，不要改脚本绕过）。再跑一次 `-race -count=3` 确认没有偶发（后台进程 + 轮询）。

- [ ] **Step 5: 提交**

```bash
git add internal/appdeploy/runtime
git commit -m "feat(appdeploy): embedded run.sh runtime (detach, atomic status, install skip, health, stop/remove) with sh tests

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 5: `sshtest` shell 模式（exec 真执行）

**Files:**
- Modify: `internal/access/sshtest/server.go`（`Options.Shell / Root`、`Server.ctx / cancel / root`、`session.exec` 分支）
- Test: `internal/access/sshtest/server_test.go`（追加 `TestShellModeExec`）

**Interfaces:**
- Consumes: 现有 `sshtest.New / Options / session.exec / finish`。
- Produces: `Options.Shell bool`、`Options.Root string`（空 → `os.MkdirTemp("", "sshtest-root-")`，`Close` 时删除）；`func (s *Server) Root() string`；shell 模式下 `exec` 请求 = `/bin/sh -c <cmd>`，`Dir = Root`，`Env = os.Environ() + HOME=<Root> + SPARK_TEST_ROOT=<Root>`，stdin ← 通道、stdout / stderr → 通道、退出码 → `exit-status`；命令被 `exec.CommandContext(s.ctx, …)` 管理，`Close` 时全部杀掉；信号 `kill` 时退出码 128+信号。

- [ ] **Step 1: 写失败的测试**

`internal/access/sshtest/server_test.go` 追加：

```go
func TestShellModeExec(t *testing.T) {
	up := newSigner(t)
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey(), Shell: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	root := srv.Root()
	if root == "" {
		t.Fatal("no root")
	}
	c := dial(t, srv, up)
	// 真执行：工作目录是 Root，HOME 也是
	if out, err := output(t, c, `pwd; printf '%s\n' "$HOME"`); err != nil || out != root+"\n"+root+"\n" {
		t.Fatalf("pwd = %q %v", out, err)
	}
	// 退出码透传
	var exitErr *ssh.ExitError
	if _, err := output(t, c, "exit 7"); !errors.As(err, &exitErr) || exitErr.ExitStatus() != 7 {
		t.Fatalf("exit 7 = %v", err)
	}
	// stdin → 文件（部署包上传路径）
	sess, _ := c.NewSession()
	sess.Stdin = strings.NewReader("payload")
	if err := sess.Run("mkdir -p sub && cat > sub/f"); err != nil {
		t.Fatal(err)
	}
	sess.Close()
	if b, _ := os.ReadFile(filepath.Join(root, "sub", "f")); string(b) != "payload" {
		t.Fatalf("file = %q", b)
	}
	// stderr 分流
	sess, _ = c.NewSession()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	if err := sess.Run("echo out; echo err >&2"); err != nil || stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Fatalf("split = %q %q %v", stdout.String(), stderr.String(), err)
	}
	sess.Close()
	// 关闭服务器杀掉仍在跑的命令（sleep 不会残留）
	sess, _ = c.NewSession()
	done := make(chan error, 1)
	go func() { done <- sess.Run("sleep 30") }()
	time.Sleep(100 * time.Millisecond)
	srv.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("sleep finished normally")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command survived Close")
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("temp root not removed on Close")
	}
}
```

（测试文件 import 加 `os`、`path/filepath`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/access/sshtest/ -run TestShellModeExec 2>&1 | head -5`
Expected: 编译错误（`Shell`、`Root` 未定义）。

- [ ] **Step 3: 实现**

`internal/access/sshtest/server.go`：

`Options` 追加：

```go
	// Shell：exec 请求用本机 /bin/sh -c 真执行（工作目录 Root，HOME=Root）；false 时沿用回显模式（echo / exit / cat / …）。
	Shell bool
	// Root：Shell 模式的工作目录（充当容器的 /）；为空时自动建临时目录并在 Close 时删除。
	Root string
```

`Server` 追加字段 `shell bool; root string; rootTemp bool; ctx context.Context; cancel context.CancelFunc`（import `context`、`os`、`os/exec`）。`New` 里在建 `Server` 之前：

```go
	ctx, cancel := context.WithCancel(context.Background())
	root, rootTemp := o.Root, false
	if o.Shell && root == "" {
		var err error
		if root, err = os.MkdirTemp("", "sshtest-root-"); err != nil {
			cancel()
			return nil, err
		}
		rootTemp = true
	}
```

并把 `shell: o.Shell, root: root, rootTemp: rootTemp, ctx: ctx, cancel: cancel` 放进字面量。`Root()`：

```go
// Root 返回 Shell 模式的工作目录（非 Shell 模式为空）。
func (s *Server) Root() string { return s.root }
```

`Close` 在 `s.wg.Wait()` 之前加 `s.cancel()`（杀掉仍在跑的命令），之后：

```go
	if s.rootTemp {
		os.RemoveAll(s.root)
	}
```

`session.exec` 开头加分支：

```go
func (x *session) exec(ch ssh.Channel, cmd string) uint32 {
	if x.srv.shell {
		return x.srv.runShell(ch, cmd)
	}
	switch { // 原回显模式不变
```

新增：

```go
// runShell 用 /bin/sh -c 真执行命令：stdin / stdout / stderr 与通道对接，返回退出码；服务器 Close 时命令被杀（ctx）。
func (s *Server) runShell(ch ssh.Channel, cmd string) uint32 {
	c := exec.CommandContext(s.ctx, "/bin/sh", "-c", cmd)
	c.Dir = s.root
	c.Env = append(os.Environ(), "HOME="+s.root, "SPARK_TEST_ROOT="+s.root)
	c.Stdout, c.Stderr = ch, ch.Stderr()
	stdin, err := c.StdinPipe()
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "sshtest: %v\n", err)
		return 255
	}
	if err := c.Start(); err != nil {
		fmt.Fprintf(ch.Stderr(), "sshtest: %v\n", err)
		return 127
	}
	go func() { io.Copy(stdin, ch); stdin.Close() }()
	err = c.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ee.ExitCode() < 0 { // 被信号杀
			return 128 + 15
		}
		return uint32(ee.ExitCode())
	default:
		return 255
	}
}
```

注意 `c.Stdout = ch`：后台进程若继承了 stdout（没有重定向）会让 `Wait` 等到它结束——这正是 `run.sh` 的 `detach` 必须 `</dev/null >>log 2>&1` 的原因；`exec.Cmd` 对非 `*os.File` 的 Stdout 会起拷贝 goroutine 并等 EOF。`Wait` 卡住属于脚本错误，测试里会以超时暴露。

- [ ] **Step 4: 运行全部 sshtest / upstream / sshgw / ingress 测试并提交**

Run: `go vet ./internal/access/... && go test ./internal/access/... ./internal/sshgw/ ./internal/ingress/ 2>&1 | tail -8`
Expected: 全部通过（回显模式行为不变）。

```bash
git add internal/access/sshtest
git commit -m "test(sshtest): shell mode executes exec requests with /bin/sh -c under a temp root

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 6: `upstream.Pool.Exec`

**Files:**
- Modify: `internal/access/upstream/pool.go`
- Test: `internal/access/upstream/pool_test.go`（追加 `TestPoolExec`、`TestPoolExecTimeoutKeepsConnection`、`TestPoolExecRetriesDeadClient`）

**Interfaces:**
- Consumes: 现有 `Pool.client / dropClient / DialError / ErrClosed`、Task 5 的 shell 模式。
- Produces: `func (p *Pool) Exec(ctx context.Context, t Target, cmd string, stdin []byte, maxOutput int) (stdout, stderr []byte, exit int, err error)`：
  - 复用共享 `ssh.Client`，`NewSession` → `Run(cmd)`（不 `Setenv`，容器 sshd 默认不接受）；
  - 远端非零退出：`exit = 退出码, err = nil`；`exit-status` 缺失（`*ssh.ExitMissingError`）：`exit = -1, err` 非 nil 非 `DialError`；
  - `ctx` 到期 / 取消：关闭 session，返回已收到的输出与 `ctx.Err()`（共享连接保留）；
  - `NewSession` 失败或 `Run` 的连接层错误：丢弃连接重试一次，再失败返回 `*DialError`；池已关闭 `ErrClosed`；
  - stdout / stderr 各截断到 `maxOutput` 字节（超出部分丢弃，不报错；`maxOutput <= 0` 视为 1 MiB）。

- [ ] **Step 1: 写失败的测试**

`internal/access/upstream/pool_test.go` 追加：

```go
func shellServer(t *testing.T, up ssh.Signer) *sshtest.Server {
	t.Helper()
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey(), Shell: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func TestPoolExec(t *testing.T) {
	up := signer(t)
	srv := shellServer(t, up)
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: time.Second}, &memStore{pubs: map[string]string{}})
	defer p.Close()
	tg := upstream.Target{InstanceID: "ins-exec0001", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	ctx := context.Background()
	out, errb, exit, err := p.Exec(ctx, tg, "echo hi; echo oops >&2", nil, 1024)
	if err != nil || exit != 0 || string(out) != "hi\n" || string(errb) != "oops\n" {
		t.Fatalf("exec = %q %q %d %v", out, errb, exit, err)
	}
	if _, _, exit, err := p.Exec(ctx, tg, "exit 3", nil, 1024); err != nil || exit != 3 {
		t.Fatalf("exit 3 = %d %v", exit, err)
	}
	// stdin 传递（部署包上传就是这条路径）
	out, _, exit, err = p.Exec(ctx, tg, "cat > f && wc -c < f", []byte("12345"), 1024)
	if err != nil || exit != 0 || strings.TrimSpace(string(out)) != "5" {
		t.Fatalf("stdin = %q %d %v", out, exit, err)
	}
	// 截断
	out, errb, exit, err = p.Exec(ctx, tg, "head -c 5000 /dev/zero | tr '\\0' x; head -c 3000 /dev/zero | tr '\\0' y >&2", nil, 100)
	if err != nil || exit != 0 || len(out) != 100 || len(errb) != 100 {
		t.Fatalf("truncate = %d %d %d %v", len(out), len(errb), exit, err)
	}
	if p.Len() != 1 || srv.ConnCount() != 1 {
		t.Fatalf("pool=%d conns=%d, want one shared connection", p.Len(), srv.ConnCount())
	}
	// 不可达 → DialError
	var de *upstream.DialError
	if _, _, _, err := p.Exec(ctx, upstream.Target{InstanceID: "ins-exec0002", LanIP: "10.255.255.1", HostPubkey: "x"}, "true", nil, 10); !errors.As(err, &de) {
		t.Fatalf("unreachable = %v", err)
	}
	p.Close()
	if _, _, _, err := p.Exec(ctx, tg, "true", nil, 10); !errors.Is(err, upstream.ErrClosed) {
		t.Fatalf("closed = %v", err)
	}
}

func TestPoolExecTimeoutKeepsConnection(t *testing.T) {
	up := signer(t)
	srv := shellServer(t, up)
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: time.Second}, &memStore{pubs: map[string]string{}})
	defer p.Close()
	tg := upstream.Target{InstanceID: "ins-exec0003", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, _, exit, err := p.Exec(ctx, tg, "echo partial; sleep 10; echo late", nil, 1024)
	var de *upstream.DialError
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &de) || exit != -1 || string(out) != "partial\n" || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout = %q %d %v after %v", out, exit, err, time.Since(start))
	}
	// 共享连接仍可用
	if out, _, exit, err := p.Exec(context.Background(), tg, "echo again", nil, 64); err != nil || exit != 0 || string(out) != "again\n" {
		t.Fatalf("after timeout = %q %d %v", out, exit, err)
	}
	if p.Len() != 1 || srv.ConnCount() != 1 {
		t.Fatalf("pool=%d conns=%d", p.Len(), srv.ConnCount())
	}
}

func TestPoolExecRetriesDeadClient(t *testing.T) {
	up := signer(t)
	srv := shellServer(t, up)
	p := upstream.New(upstream.Config{Signer: up, Port: srv.Port(), DialTimeout: time.Second}, &memStore{pubs: map[string]string{}})
	defer p.Close()
	tg := upstream.Target{InstanceID: "ins-exec0004", LanIP: "127.0.0.1", HostPubkey: srv.HostPubLine()}
	for i := 0; i < 5; i++ {
		if out, _, exit, err := p.Exec(context.Background(), tg, "echo ok", nil, 64); err != nil || exit != 0 || string(out) != "ok\n" {
			t.Fatalf("round %d = %q %d %v", i, out, exit, err)
		}
		srv.DropAll() // 上游崩溃：下一次 Exec 发现连接已死，重建一次
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/access/upstream/ -run TestPoolExec 2>&1 | head -5`
Expected: 编译错误（`Exec` 未定义）。

- [ ] **Step 3: 实现**

`internal/access/upstream/pool.go` 追加（import 加 `bytes`）：

```go
// limitedBuf 只保留前 max 字节，其余丢弃（exec 输出截断，不报错）。
type limitedBuf struct {
	buf bytes.Buffer
	max int
}

func (l *limitedBuf) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		l.buf.Write(p)
	}
	return len(p), nil
}

// Exec 在容器里执行一条命令（复用共享 ssh.Client 上的一个 session）。
// 远端非零退出：exit = 退出码，err = nil。ctx 到期：关闭 session，返回已收到的输出与 ctx.Err()（共享连接保留）。
// NewSession / Run 的连接层错误：丢弃连接重试一次，再失败返回 *DialError。stdout / stderr 各截断到 maxOutput 字节。
func (p *Pool) Exec(ctx context.Context, t Target, cmd string, stdin []byte, maxOutput int) (stdout, stderr []byte, exit int, err error) {
	if maxOutput <= 0 {
		maxOutput = 1 << 20
	}
	for attempt := 0; attempt < 2; attempt++ {
		var c *ssh.Client
		if c, err = p.client(ctx, t); err != nil {
			return nil, nil, -1, err
		}
		sess, serr := c.NewSession()
		if serr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, nil, -1, ctxErr
			}
			p.dropClient(t.InstanceID, c)
			err = serr
			continue
		}
		out, errb := &limitedBuf{max: maxOutput}, &limitedBuf{max: maxOutput}
		sess.Stdin, sess.Stdout, sess.Stderr = bytes.NewReader(stdin), out, errb
		done := make(chan error, 1)
		go func() { done <- sess.Run(cmd) }()
		select {
		case rerr := <-done:
			sess.Close()
			var ee *ssh.ExitError
			var em *ssh.ExitMissingError
			switch {
			case rerr == nil:
				return out.buf.Bytes(), errb.buf.Bytes(), 0, nil
			case errors.As(rerr, &ee):
				return out.buf.Bytes(), errb.buf.Bytes(), ee.ExitStatus(), nil
			case errors.As(rerr, &em):
				return out.buf.Bytes(), errb.buf.Bytes(), -1, fmt.Errorf("远端未返回退出码: %w", rerr)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return out.buf.Bytes(), errb.buf.Bytes(), -1, ctxErr
			}
			p.dropClient(t.InstanceID, c) // 连接层错误：重建后再试一次
			err = rerr
		case <-ctx.Done():
			sess.Close() // 只关这个 session；共享连接保留
			<-done
			return out.buf.Bytes(), errb.buf.Bytes(), -1, ctx.Err()
		}
	}
	return nil, nil, -1, &DialError{InstanceID: t.InstanceID, Err: err}
}
```

`sess.Close()` 后 `Run` 很快以错误返回（通道关闭），`<-done` 不会久等；远端 `sleep` 进程由容器 sshd 在通道关闭时收到 SIGHUP（sshtest 的 shell 模式里 `exec.Cmd` 的 stdout 写失败后仍会跑完 `sleep`，由 `Close` 的 ctx 兜底杀掉）。

- [ ] **Step 4: 运行测试并提交**

Run: `go vet ./internal/access/... && go test -race ./internal/access/upstream/ 2>&1 | tail -5`
Expected: 全部通过（含原有 7 个 Pool 测试）。

```bash
git add internal/access/upstream
git commit -m "feat(upstream): Pool.Exec runs a command over the shared ssh client (stdin, exit code, truncation, ctx timeout, one retry)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 7: 目录服务（应用 / 版本 CRUD、发布、覆盖率）与 `platform admin app import`

**Files:**
- Create: `internal/appdeploy/catalog.go`、`internal/appdeploy/coverage.go`、`internal/appdeploy/shcheck.go`、`internal/appdeploy/importer.go`、`cmd/platform/app_import.go`
- Modify: `cmd/platform/admin.go`（`case "app"`、`adminUsage`）、`cmd/platform/main.go`（`adminMain` 为 `app` 子命令建 `appdeploy.NewCatalog`）
- Test: `internal/appdeploy/catalog_test.go`、`internal/appdeploy/helpers_test.go`（新建：`newEnv`，嵌入式 PG + 假时钟 + `dbtest.ImageID` + `fleet`）、`cmd/platform/app_import_test.go`

**Interfaces:**
- Consumes: Task 1 类型、Task 3 `ValidateSpec / ValidSlug / ValidVersion / SpecHash`、`fleet.Service.BroadcastPullImage / OnlineMachineIDs`、`catalog.Service.AdminImages`、`audit.Record / Actor`。
- Produces（`internal/appdeploy/catalog.go`）：
  - `type Catalog struct` + `func NewCatalog(pool *pgxpool.Pool, clk clock.Clock, fleet *fleet.Service, images *catalog.Service, log *slog.Logger) *Catalog`。
  - `type AppInput struct{ Slug, Name, Summary, Description, Category, Icon string; Sort int; Status string }`；`CreateApp(ctx, actor, in) (App, error)`（校验 slug / 长度 / 类别 / 图标名集合 `app, llm, chat, image, notebook, tool, code, audio`，重复 slug → `apperr.ErrDuplicate` 消息「slug 已存在」；审计 `app.create`）；`UpdateApp(ctx, actor, id, in) (App, error)`（忽略 `Slug`；审计 `app.update`）；`DeleteApp(ctx, actor, id) error`（仅 draft 且无版本，否则 `ErrInvalidState`；审计 `app.delete`）；`GetApp(ctx, id) (App, error)`；`AppBySlug(ctx, slug) (App, error)`；`type AppRow struct{ App; Versions int; DeploymentsLive int }`；`ListAppsAdmin(ctx) ([]AppRow, error)`。
  - `type VersionInput struct{ Version string; ImageIDs []int64; Spec Spec; Script string; Changelog string; CopyFrom *int64 }`；`CreateVersion(ctx, actor, appID, in) (Version, error)`（`CopyFrom` 非空时以该版本的 spec / 脚本 / 镜像为基底、`in` 中非零字段覆盖；**草稿也要过** `ValidateSpec` + 脚本 ≤ 64KB + `sh -n`（`shcheck.go`）+ `image_ids` 非空且都存在并 enabled；失败 `app_version_invalid`；审计 `app.version.create`）；`ValidateVersion(ctx, appID, in) (specHash string, err error)`（只校验，`?validate=1` 用）；`UpdateVersion(ctx, actor, appID, vid, in) (Version, error)`（draft 全改；published 只允许 `Changelog` 与 `Status = "retired"`，其它字段与当前值不同 → `ErrAppVersionFrozen`；审计 `app.version.update`）；`PublishVersion(ctx, actor, appID, vid) (Version, int, error)`（校验 → `published`、`published_at = now`、`spec_hash = SpecHash(spec, script)`；对 `image_ids` 每个 `BroadcastPullImage(ref)`，返回下发台数；审计 `app.version.publish` 含 `spec_hash`）；`Prepull(ctx, actor, appID, vid) (int, error)`（审计 `app.version.prepull`）；`GetVersion(ctx, id) (Version, error)`（含 `Script`）；`ListVersions(ctx, appID) ([]Version, error)`；`LatestPublished(ctx, q, appID) (Version, error)`（`published` 中 `published_at` 最新；无 → `apperr.ErrAppUnavailable`）。
  - `type CatalogItem struct{ App; Version Version; Images []ImageBrief }`、`type ImageBrief struct{ ID int64; Name string }`；`ListPublished(ctx) ([]CatalogItem, error)`（`apps.status = 'published'` 且有 published 版本，按 `sort, id`）。
  - `coverage.go`：`type Coverage struct{ ImageID int64; ImageName string; Cached, Online, Unknown int }`；`VersionCoverage(ctx, v Version) ([]Coverage, error)`（SQL 见裁定「覆盖率」；镜像 ref / 名称从 `images` 表取）。
  - `shcheck.go`：`func CheckShellSyntax(ctx, script string) error`（临时文件 + `/bin/sh -n`，5 秒超时；错误消息 = stderr 第一行去掉临时文件路径，形如 `line 12: Syntax error: ...`）。
  - `importer.go`：`type ImportResult struct{ Slug, Version string; Created bool }`；`func (c *Catalog) ImportDir(ctx, actor, dir string) ([]ImportResult, error)`（裁定「目录导入」；`app.json` 结构见裁定；一个目录失败整体返回错误、已导入的保留）。
  - `cmd/platform`：`platform admin app import <dir>` 打印每行 `slug version created|version-added`；`adminUsage` 增补。

- [ ] **Step 1: 测试辅助与失败测试**

`internal/appdeploy/helpers_test.go`：建 `env{t, ctx, pool, clk, fleet, catalog(*catalog.Service), box, cat(*appdeploy.Catalog), imageID, userID, productID, machines}`，`newEnv(t, machines int)`（`dbtest.New`、`clock.NewFake(dbtest.T0)`、`fleet.NewService`、`catalog.NewService(pool).WithClock(clk)`、`dbtest.ImageID`、`dbtest.CreateUser`、`dbtest.Fund`、`dbtest.CreateMachine` × n 并 `UPDATE machines SET agent_online = true`），`e.actor()` 返回 `audit.Actor{AdminID: 1}`，`e.goodVersionInput()` 返回 Task 3 `goodSpec()` + 脚本 `"#!/bin/sh\ncase \"$1\" in install) ;; start) sleep 1 & echo $! > \"$SPARK_APP_DIR/service.pid\" ;; esac\n"` + `ImageIDs: []int64{e.imageID}`。`TestMain` 用 `dbtest.Main`。

`internal/appdeploy/catalog_test.go` 用例（每个独立 `newEnv`）：
- `TestAppCRUDAndSlugImmutable`：创建（slug 校验失败 `bad_request`、重复 slug `duplicate`）；更新改 name 不改 slug（`in.Slug` 传别的值被忽略）；删除 draft 无版本 OK；有版本后删除 → `invalid_state`；审计 `app.create / app.update / app.delete` 各一条（`audit.List`）。
- `TestVersionDraftValidation`：spec 非法 → `app_version_invalid` 消息带 `ports[0].port`；脚本 `if then` 缺 `fi` → `app_version_invalid` 消息含 `line`；脚本 65537 字节 → 拒绝；`image_ids` 含不存在 / disabled 镜像 → 拒绝；合法 → draft，`SpecHash` 已算出（草稿也存 hash，发布时重算）。
- `TestPublishFreezesAndBroadcasts`：发布 → `published`、`published_at = clk.Now()`、`agent_commands` 里每台在线机器一条 `pull_image`（ref = 镜像 ref）、返回台数 = 在线数；再 `UpdateVersion` 改 spec → `app_version_frozen`；改 changelog OK；`Status = retired` OK 且 `LatestPublished` 变为 `app_unavailable`（只有一个版本时）；审计 `app.version.publish` 的 diff 含 `spec_hash`。
- `TestCopyFromAndLatest`：`CopyFrom` 新版本继承脚本与镜像、只覆盖 `Version`；两个 published 版本 `LatestPublished` 取 `published_at` 最新（假时钟推进 1 分钟再发第二个）而不是字符串最大。
- `TestListPublishedFiltersDrafts`：draft 应用不出现；published 应用无 published 版本不出现；`Images` 顺序 = `image_ids` 顺序。
- `TestCoverageCountsUnknownAndStale`：3 台在线机器：一台 `cached_images` 含 ref 且 `cached_images_at = now`，一台含 ref 但 `cached_images_at = now − 6m`，一台 `cached_images_at IS NULL`；一台离线也含 ref → `Coverage{Cached: 1, Online: 3, Unknown: 2}`。
- `TestCheckShellSyntax`：`echo ok` 通过；`if true; then` 失败且消息含 `line` 与 `Syntax error`（macOS 的 `sh` 消息是 `syntax error: unexpected end of file`——断言不区分大小写地含 `syntax error`）。
- `TestImportDir`：临时目录写两个 slug（含 `app.json`），镜像按名字匹配；第一次全部 `Created: true`；第二次（改 version）只加版本；`app.json` 的 `images` 引用不存在的名字 → 错误消息含该名字。

`cmd/platform/app_import_test.go`：对 `runAdmin(ctx, nil, []string{"app"}, …)` 返回用法错误；`app import` 的实际执行放在 `internal/appdeploy` 测过，这里只测参数解析（`app import` 缺目录 → 错误）。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/appdeploy/ -run 'TestApp|TestVersion|TestPublish|TestCopy|TestListPublished|TestCoverage|TestCheckShell|TestImport' 2>&1 | head -5`
Expected: 编译错误。

- [ ] **Step 3: 实现 `catalog.go`**

关键 SQL / 规则（实现时照此写，不要简化）：
- 列常量 `appCols = "id, slug, name, summary, description, category, icon, sort, status, created_at, updated_at"`、`versionCols = "id, app_id, version, status, image_ids, spec, script, spec_hash, changelog, published_at, created_at, updated_at"`；`scanApp / scanVersion`（`spec` 用 `json.Unmarshal` 到 `Spec`，`image_ids` 扫到 `[]int64`）。
- `CreateApp` 校验：`ValidSlug`；`name` 1～32 字符；`summary` ≤ 80；`description` ≤ 4000；`category` ∈ 四个；`icon` ∈ 集合（空 → `app`）；`sort` −10000～10000；`status` ∈ 三个（空 → `draft`）。唯一冲突用 `db.IsUniqueViolation`。
- `validateVersionInput(ctx, q, in) (Spec, string, error)`：`ValidVersion`；`len(in.Script) <= MaxScriptBytes` 且非空；`ValidateSpec(&spec)`；`CheckShellSyntax`（错误包成 `apperr.AppVersionInvalid("script", msg)`）；`image_ids` 去重、非空、`SELECT count(*) FROM images WHERE id = ANY($1) AND enabled` 等于个数（否则 `AppVersionInvalid("image_ids", "镜像不存在或未启用")`）；`changelog` ≤ 1000；返回规范化后的 spec 与 `SpecHash`。
- `UpdateVersion` 对 published：先读当前行，`in.Version != cur.Version || !slices.Equal(in.ImageIDs, cur.ImageIDs) || in.Script != cur.Script || canonical(in.Spec) != canonical(cur.Spec)` → `ErrAppVersionFrozen`；`in.Status` 只接受空（不改）或 `retired`。retired 版本任何改动 → `ErrAppVersionFrozen`。
- `PublishVersion`：`FOR UPDATE` 锁版本行；非 draft → `ErrInvalidState`；重新 `validateVersionInput`（镜像可能已被停用）；`UPDATE … status='published', published_at=$now, spec_hash=$hash`；提交后对每个镜像 `BroadcastPullImage`（失败只记日志，返回已下发的台数）。
- `DeleteApp`：`SELECT status, (SELECT count(*) FROM app_versions WHERE app_id = $1) FROM apps WHERE id = $1 FOR UPDATE`。
- `ListAppsAdmin`：`SELECT a.*, (SELECT count(*) FROM app_versions v WHERE v.app_id = a.id), (SELECT count(*) FROM app_deployments d WHERE d.app_id = a.id AND d.state NOT IN ('removed','lost')) FROM apps a ORDER BY a.sort, a.id`。
- `ListPublished`：`SELECT DISTINCT ON (a.id) … FROM apps a JOIN app_versions v ON v.app_id = a.id AND v.status = 'published' WHERE a.status = 'published' ORDER BY a.id, v.published_at DESC` 再按 `sort, id` 排序；`Images` 用 `SELECT id, name FROM images WHERE id = ANY($1)` 后按 `image_ids` 顺序重排。

`coverage.go` 的 SQL：

```sql
SELECT
  count(*) FILTER (WHERE agent_online) AS online,
  count(*) FILTER (WHERE agent_online AND cached_images_at >= $2 AND $1 = ANY(cached_images)) AS cached,
  count(*) FILTER (WHERE agent_online AND (cached_images_at IS NULL OR cached_images_at < $2)) AS unknown
FROM machines
```
（`$1` = 镜像 ref，`$2` = `now − 5m`。）

`shcheck.go`：

```go
func CheckShellSyntax(ctx context.Context, script string) error {
	f, err := os.CreateTemp("", "spark-app-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		return err
	}
	f.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/sh", "-n", f.Name()).CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	msg = strings.TrimPrefix(msg, f.Name()+": ")
	if msg == "" {
		msg = err.Error()
	}
	return apperr.AppVersionInvalid("script", msg)
}
```

`importer.go`：`appJSON struct{ Name, Summary, Description, Category, Icon string; Sort int; Version string; Images []string; Changelog string }`；对 `os.ReadDir(dir)` 的每个子目录读三份文件；镜像名 → id：`SELECT id FROM images WHERE name = $1`（不存在 → `fmt.Errorf("%s: 镜像 %q 未登记", slug, name)`）；slug 不存在 → `CreateApp(status draft)`；`CreateVersion`。目录按名字排序处理。

`cmd/platform/app_import.go`：

```go
// appImport 实现 `platform admin app import <dir>`：把 deploy/apps 风格的目录导入为 draft 应用 / 版本。
func appImport(ctx context.Context, cat *appdeploy.Catalog, args []string, out io.Writer) error {
	if len(args) != 2 || args[0] != "import" {
		return errors.New("用法：platform admin app import <目录>")
	}
	res, err := cat.ImportDir(ctx, audit.Actor{}, args[1])
	for _, r := range res {
		state := "version-added"
		if r.Created {
			state = "created"
		}
		fmt.Fprintf(out, "%s %s %s\n", r.Slug, r.Version, state)
	}
	return err
}
```

`admin.go` 的 `runAdmin` 加 `case "app": return appImport(ctx, cat, args[1:], out)`——`runAdmin` 签名加一个参数 `cat *appdeploy.Catalog`（现有三个调用点同步传 `nil` / 实例）；`main.go` 的 `adminMain` 在建 `admin.Service` 的同时建 `appdeploy.NewCatalog(pool, clock.Real{}, fleet.NewService(pool, clock.Real{}), catalog.NewService(pool), log)`。

- [ ] **Step 4: 运行测试并提交**

Run: `go vet ./... && go test ./internal/appdeploy/ ./cmd/platform/ 2>&1 | tail -5`
Expected: 全部通过。

```bash
git add internal/appdeploy cmd/platform
git commit -m "feat(appdeploy): app / version catalog (draft validation, sh -n, publish + prepull broadcast, coverage), platform admin app import

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 8: 部署服务（用户操作：创建 / 重新部署 / 停止 / 启动 / 卸载 / 日志 / 事件；限额与端口冲突）

**Files:**
- Create: `internal/appdeploy/service.go`、`internal/appdeploy/deployments.go`（列常量、scan、锁、事件）、`internal/appdeploy/secrets.go`、`internal/appdeploy/ports.go`
- Modify: `internal/rental/ports.go`（`lockOwned` → 导出 `LockOwned`，三个调用点 `AddPort / UpdatePort / DeletePort` 与 `access.go` / `renew.go` 等处的内部调用同步改名——用 `grep -rn 'lockOwned(' internal/rental` 找全）
- Test: `internal/appdeploy/service_test.go`

**Interfaces:**
- Consumes: Task 7 `Catalog`、Task 3 `ValidateEnv / SpecHash`、`rental.LockOwned(ctx, tx, userID, id) (rental.Instance, error)`、`rental.Instance.ImageID / State / SuspendedAt`、`secretbox.Box.Seal / Open`、`upstream.Pool.Exec`（日志实时读取）、`audit`。
- Produces（`internal/appdeploy/service.go`）：
  - `type Deps struct{ Pool *pgxpool.Pool; Clock clock.Clock; Box *secretbox.Box; Log *slog.Logger; Catalog *Catalog; Images *catalog.Service; Rental *rental.Service; Fleet *fleet.Service; Upstream *upstream.Pool; PortURL func(instanceID string, port int) string; AppRoot string; HFEndpoint, PipIndexURL string }`；`func NewService(d Deps) *Service`（`AppRoot` 空 → `DefaultAppRoot`）。
  - `type CreateRequest struct{ UserID int64; InstanceID string; AppID int64; VersionID *int64; Env map[string]string; IP string }`；`Create(ctx, req) (Deployment, error)`。
  - `type RedeployRequest struct{ UserID int64; InstanceID, DeploymentID string; VersionID *int64; Env map[string]string; EnvSet bool; IP string }`；`Redeploy(ctx, req) (Deployment, error)`（`EnvSet=false` 沿用全部；`true` 时：普通变量以请求为准，secret 未出现沿用、出现空串清除）。
  - `Stop / Start / Remove(ctx, userID int64, instanceID, depID, ip string) (Deployment, error)`。
  - `Get(ctx, depID) (Deployment, error)`、`GetForUser(ctx, userID int64, instanceID, depID string) (Deployment, error)`（非主人 → `ErrNotFound`）、`ListForInstance(ctx, instanceID string) ([]Deployment, error)`（按 `created_at`，含终态）、`ListLive(ctx, instanceID) ([]Deployment, error)`、`LiveByInstances(ctx, ids []string) (map[string][]Deployment, error)`（列表页摘要，一次查询）。
  - `type LogResult struct{ Log string; Live bool; At time.Time }`；`Log(ctx, userID, instanceID, depID) (LogResult, error)`。
  - `Events(ctx, depID string, limit int) ([]Event, error)`。
  - `AdminForceRemove(ctx, actor audit.Actor, depID, reason string) (Deployment, error)`（任何非终态 → 同 `Remove`，事件「平台卸载：<reason>」，审计 `app.deployment.force_remove` diff `{deployment, reason}`）。
  - `type ListFilter struct{ State string; AppID, UserID int64; InstanceID string }`；`AdminList(ctx, f ListFilter, p paging.Page) ([]Deployment, int, error)`。
  - `deployments.go`：`depCols`、`scanDeployment`、`lockDeployment(ctx, tx, id) (Deployment, error)`（`FOR UPDATE`）、`addEvent(ctx, q, depID, from, to, detail string, at time.Time) error`、`livePortsExcept(ctx, q, instanceID, exclude string) ([]int, error)`（实例上所有非终态部署（除 `exclude`）在 spec 里声明的端口：`SELECT (p->>'port')::int FROM app_deployments d JOIN app_versions v ON v.id = d.app_version_id, jsonb_array_elements(v.spec->'ports') p WHERE d.instance_id = $1 AND d.id <> $2 AND d.state NOT IN ('removed','lost')`）、`mustJSON(map[string]string) []byte`；内部方法（Task 9 / 10 复用，参数一律 `db.Querier`，`pgx.Tx` 满足它）：`resolveVersion(ctx, q, appID int64, versionID *int64) (App, Version, error)`、`checkImage(ctx, q, ver Version, imageID int64) error`、`insert(ctx, q, inst rental.Instance, userID int64, app App, ver Version, plain, secret map[string]string, now time.Time) (Deployment, error)`、`markFailed(ctx, tx, cur Deployment, code, msg string, now) error`（`state='failed', error_code, message, health='unknown', finished_at=now, next_poll_at=now+24h, updated_at=now` + 事件 `<cur.State> → failed`，detail = `ErrorText[code]+"："+msg`）、`markRemoved(ctx, tx, cur Deployment, code, detail string, now) error`（`state='removed', desired='removed', error_code=code, finished_at=now, cleanup_pending=false, updated_at=now` + `deletePorts(actor audit.Actor{UserID: cur.UserID})` + 事件）。
  - `secrets.go`：`sealSecrets(box, m map[string]string) []byte`（空 map → nil）、`openSecrets(box, enc []byte) (map[string]string, error)`、`Redact(s string, secrets map[string]string) string`（值长度 ≥ 8 才替换；按长度降序替换，避免短值是长值前缀时留下残片）。
  - `ports.go`：`registerPorts(ctx, tx, d Deployment, ports []PortSpec, appName string, now) (errCode string, err error)`（Global Constraints「暴露」）、`deletePorts(ctx, tx, d Deployment, appName string, now, actor audit.Actor) error`（`DELETE … WHERE deployment_id = $1` + 审计 `instance.port_delete`）、`PortURLFor(d) string`（端口行存在且 `PortURL != nil` 时返回 URL，否则空）。

- [ ] **Step 1: 写失败的测试**

`internal/appdeploy/service_test.go`（扩展 Task 7 的 `env`：加 `svc *appdeploy.Service`、`rental *rental.Service`、`e.publishedApp(slug, spec, script, imageIDs) (App, Version)`、`e.runningInstance() rental.Instance`（`rental.Place` + 假 Agent 结果：直接 `UPDATE instances SET state = 'running'` 并 `machines.state = 'allocated'`，与 `rental` 测试一致的最小方式）、`e.reservedInstance()`）。用例：
- `TestCreateQueuedDeployment`：running 实例 + published vllm → `Create` 返回 `state queued, desired running, attempt 0, port 8000, next_poll_at = now, spec_hash = version.SpecHash`，`env` 含默认值、`secret_env_enc` 非空且 `openSecrets` 还原 `API_KEY`；事件一条 `"" → queued`；审计 `instance.app_deploy` 的 diff 不含 `API_KEY` 的值。
- `TestCreateLimitsAndConflicts`：第 4 个应用 → `app_limit`；同一应用再建 → `app_exists`；实例已有用户端口 8000 → `app_port_conflict`；另一部署（installing）声明的附加端口与新应用主端口相同 → `app_port_conflict`；实例已有 4 个用户端口 + 应用声明 2 个端口 → `port_limit`；`removed` 的旧部署不占名额（卸载后可再建）。
- `TestCreateImageMismatch`：版本 `image_ids = [img2]`、实例镜像 img1 → `app_image_mismatch` 消息含两个镜像名；多镜像列表含 img1 → 通过。
- `TestCreateOnReservedAndNotOwner`：reserved 实例可建 queued；released 实例 → `invalid_state`；别人的实例 → `not_found`；draft / retired 应用 → `app_not_found`；应用 published 但版本全 retired → `app_unavailable`；指定 `VersionID` 属于别的应用 → `app_not_found`。
- `TestEnvValidationFailsCreate`：`required` 缺失 → `app_env_required`；多余键 → `app_env_invalid`。
- `TestStateMatrix`：对 `state ∈ {queued, installing, running, stopped, failed, removing, removed, lost}`（直接 `UPDATE` 造状态）逐个验证 `Redeploy / Stop / Start / Remove` 的允许表（规格 §6.2）：允许的返回新行，不允许的 `app_invalid_state`；`Remove` 对终态 `app_invalid_state`。
- `TestRedeployVersionAndSecrets`：failed 部署 `Redeploy(EnvSet=false)` → queued、`error_code = ''`、`attempt` 不变、secret 沿用；`Redeploy(EnvSet=true, Env{"MODEL_ID": "Qwen/Qwen3-4B"})` secret 仍沿用（未出现）；`Env{"API_KEY": ""}` → `secret_env_enc` 为 nil；发布 v2 后 `Redeploy(VersionID=nil)` → `app_version_id = v2`、`spec_hash = v2.SpecHash`、`port` 随 v2 变化且旧端口行（预先插一行 `deployment_id = dep`）被删；v2 的 `image_ids` 不含实例镜像 → `app_image_mismatch` 且行不变；对已是 queued 的部署再次 `Redeploy` → 返回当前行不报错。
- `TestRemoveWhenInstanceStopped`：实例 `stopped` + 部署 running（预先插端口行）→ `Remove` → `state removed, cleanup_pending true, finished_at = now`，端口行已删，审计 `instance.port_delete`；实例 running → `state removing, desired removed`，端口行保留（Runner 删）；reserved 实例上 queued 且 `attempt = 0` → 直接 `removed` 且 `cleanup_pending false`。
- `TestLogLiveAndFallback`：`Upstream` 用 shell 模式 sshtest（`AppRoot = <root>/root/data`）：目录里写 `log` 文件 → `Log` 返回 `Live true` 且内容脱敏（文件里写 secret 值 → `***`）；`srv.DropAll()` 后 `sshtest` 拒绝连接（`srv.Close()`）→ `Live false` 且返回 `log_tail`。
- `TestRedact`：`Redact("key=abcdefgh and abcdefghij", {"A":"abcdefgh","B":"abcdefghij"})` == `"key=*** and ***"`；短于 8 的不替换。
- `TestRateLimitNotHere`：（限流在 API 层，Task 12）——不写。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/appdeploy/ -run 'TestCreate|TestEnvValidationFails|TestStateMatrix|TestRedeploy|TestRemoveWhen|TestLogLive|TestRedact' 2>&1 | head -5`
Expected: 编译错误。

- [ ] **Step 3: 实现**

`service.go` 头注释写加锁顺序（Global Constraints「加锁顺序」原文）。关键实现：

```go
// Create 在实例上新建一条 queued 部署（规格 §6.2「部署（新）」）。
func (s *Service) Create(ctx context.Context, req CreateRequest) (Deployment, error) {
	now := s.d.Clock.Now()
	var out Deployment
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := rental.LockOwned(ctx, tx, req.UserID, req.InstanceID)
		if err != nil {
			return err
		}
		switch inst.State {
		case rental.StateReserved, rental.StateCreating, rental.StateRunning, rental.StateStopped:
		default:
			return apperr.ErrInvalidState
		}
		app, ver, err := s.resolveVersion(ctx, tx, req.AppID, req.VersionID) // published 应用 + 指定 / 最新 published 版本
		if err != nil {
			return err
		}
		if err := s.checkImage(ctx, tx, ver, inst.ImageID); err != nil {
			return err
		}
		var live int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM app_deployments WHERE instance_id = $1 AND state NOT IN ('removed', 'lost')`, inst.ID).Scan(&live); err != nil {
			return err
		}
		if live >= MaxAppsPerInstance {
			return apperr.ErrAppLimit
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_deployments WHERE instance_id = $1 AND app_id = $2 AND state NOT IN ('removed', 'lost'))`, inst.ID, app.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return apperr.ErrAppExists
		}
		if err := s.checkPorts(ctx, tx, inst.ID, ver.Spec.Ports, ""); err != nil {
			return err
		}
		plain, secret, err := ValidateEnv(ver.Spec, req.Env)
		if err != nil {
			return err
		}
		out, err = s.insert(ctx, tx, inst, req.UserID, app, ver, plain, secret, now)
		if err != nil {
			return err
		}
		if err := addEvent(ctx, tx, out.ID, "", StateQueued, "创建部署："+app.Name+" "+ver.Version, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: req.UserID, IP: req.IP}.Entry("instance.app_deploy", "instance", inst.ID,
			map[string]any{"deployment": out.ID, "app": app.Slug, "version_id": ver.ID}))
	})
	if db.IsUniqueViolation(err) { // 并发创建同一应用：app_deployments_live_uniq
		return Deployment{}, apperr.ErrAppExists
	}
	return out, err
}

// checkPorts：spec 端口不得与实例现有端口（用户端口与其它应用已登记的）或其它非终态部署声明的端口冲突；总数 ≤ 5。exclude 为重新部署时自己的 ID。
func (s *Service) checkPorts(ctx context.Context, tx pgx.Tx, instanceID string, ports []PortSpec, exclude string) error {
	rows, err := tx.Query(ctx, `SELECT port, deployment_id FROM instance_ports WHERE instance_id = $1`, instanceID)
	if err != nil {
		return err
	}
	taken := map[int]bool{}
	total := 0
	for rows.Next() {
		var port int
		var dep *string
		if err := rows.Scan(&port, &dep); err != nil {
			rows.Close()
			return err
		}
		total++
		if dep == nil || *dep != exclude {
			taken[port] = true
		}
	}
	rows.Close()
	declared, err := livePortsExcept(ctx, tx, instanceID, exclude)
	if err != nil {
		return err
	}
	for _, p := range declared {
		taken[p] = true
	}
	for _, p := range ports {
		if taken[p.Port] {
			return apperr.AppPortConflict(p.Port)
		}
	}
	if total+len(ports) > rental.MaxPortsPerInstance {
		return apperr.ErrPortLimit
	}
	return nil
}
```

`resolveVersion`：`SELECT appCols FROM apps WHERE id = $1 AND status = 'published'` → 无 → `ErrAppNotFound`；`VersionID` 给定 → `SELECT … WHERE id = $1 AND app_id = $2 AND status = 'published'` → 无 → `ErrAppNotFound`；否则 `Catalog.LatestPublished`。`checkImage`：`slices.Contains(ver.ImageIDs, imageID)` 否则取名字（`SELECT id, name FROM images WHERE id = ANY($1)` 与 `ImageName`）构造 `apperr.AppImageMismatch`。`insert`：

```go
	id := randx.ID("dep-", 12)
	return scanDeployment(tx.QueryRow(ctx, `INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, desired, env, secret_env_enc, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'queued', 'running', $6, $7, $8, $9, $10, $10, $10) RETURNING `+depCols,
		id, inst.ID, userID, app.ID, ver.ID, mustJSON(plain), sealSecrets(s.d.Box, secret), ver.Spec.Ports[0].Port, ver.SpecHash, now))
```

`Redeploy`：锁实例（`LockOwned`）→ `lockDeployment` 且 `instance_id` 匹配 → state ∈ `running / stopped / failed`（`queued` → 只更新变量返回）→ 版本：`VersionID` 给定（同应用 published）或最新 published（无 → `ErrAppUnavailable`）→ `checkImage` → 若端口集合与当前版本不同：`checkPorts(exclude = dep.ID)`，`DELETE FROM instance_ports WHERE deployment_id = $1` → 变量合并：`old := d.Env`（普通）与 `oldSecret := openSecrets`；`EnvSet` 时 `merged := req.Env` 补上未出现的 secret（`oldSecret` 的键若不在 `req.Env` 里则沿用；出现空串 → 不传给 `ValidateEnv`，即清除），`!EnvSet` 时 `merged = old ∪ oldSecret` → `ValidateEnv(ver.Spec, merged)` → `UPDATE app_deployments SET state='queued', desired='running', app_version_id, spec_hash, port, env, secret_env_enc, phase='', progress=0, message='', health='unknown', error_code='', crash_count=0, unreachable_count=0, next_poll_at=$now, finished_at=NULL, updated_at=$now` → 事件 `<from> → queued`「重新部署 vX」→ 审计 `instance.app_redeploy`。

`Stop`：state ∈ `installing / running` → `desired='stopped'`；`Start`：state `stopped` → `desired='running', state='queued', next_poll_at=now, error_code=''`；`Remove`：非终态 →
```go
	gone := inst.State != rental.StateRunning || inst.Suspended()
	fresh := d.State == StateQueued && d.Attempt == 0
	if gone || fresh {
		UPDATE … state='removed', desired='removed', cleanup_pending=$gone && !fresh, finished_at=now; deletePorts; event「已卸载（实例停机中，应用文件会在实例下次启动时清理）」或「已卸载」
	} else {
		UPDATE … state='removing', desired='removed'; event「卸载中」
	}
```
（`removing` 状态下再点卸载：返回当前行，不报错。）审计 `instance.app_remove`。

`Log`：读部署（主人校验）+ 实例目标（`SELECT i.state, i.suspended_at IS NOT NULL, m.lan_ip, i.ssh_host_pubkey FROM instances i JOIN machines m …`）；running 且未暂停且 `Upstream != nil` → `ctx 20s`、`Exec(tg, "tail -c 65536 "+AppDir(...)+"/log", nil, MaxLogTail)` → exit 0 → `Redact` → `Live true`；否则 `log_tail`。

`AdminList` 用 `db.Where` 拼条件，`ORDER BY created_at DESC`，分页用 `paging.Page.Limit / Offset`（与 `rental.AdminListInstances` 相同写法）。

- [ ] **Step 4: 运行测试并提交**

Run: `go vet ./... && go test ./internal/appdeploy/ ./internal/rental/ 2>&1 | tail -5`
Expected: 全部通过（`rental` 因 `LockOwned` 改名需全包编译通过）。

```bash
git add internal/appdeploy internal/rental
git commit -m "feat(appdeploy): deployment service (create / redeploy / stop / start / remove / log / events, limits, port conflicts, secrets)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 9: Runner（轮询、咨询锁、状态表、回写乐观并发、端口登记 / 认领、脱敏、告警）

**Files:**
- Create: `internal/appdeploy/runner.go`、`internal/appdeploy/status.go`
- Test: `internal/appdeploy/runner_test.go`、`internal/appdeploy/status_test.go`

**Interfaces:**
- Consumes: Task 8 `Service`（`lockDeployment / addEvent / registerPorts / deletePorts / openSecrets / Redact`）、Task 6 `Pool.Exec / Dial / DialError`、Task 4 `runtime.RunSH`、Task 3 `Bundle / UploadCommand / AppDir`、`fleet.RaiseAlertOnce`。
- Produces:
  - `type RunnerConfig struct{ PollInterval time.Duration; AppRoot string; HealthAddr func(port int) string; MaxConcurrent int }`（零值 → 5s / `DefaultAppRoot` / nil / 16）。
  - `func NewRunner(s *Service, up *upstream.Pool, cfg RunnerConfig) *Runner`；`func (r *Runner) Run(ctx context.Context)`（`Tick` 循环，错误只记日志）；`func (r *Runner) Tick(ctx context.Context) (processed int, locked bool, err error)`（一轮；拿不到咨询锁 `locked = false`）。
  - `status.go`：`type Status struct{ Seq int64; State, Phase string; Progress int; Message string; Pid, ServicePid int; SpecHash string; ExitCode int; At string }`；`func ParseStatusOutput(out []byte) (st Status, alive bool, logTail string, err error)`（三段：第一行 JSON（> 16KB 或非法 → err）、`alive=`、`log_size=`、`---` 之后全部为日志尾部）。
  - 告警 kind `app_deploy_unreachable`，文案 `rental.AlertAppDeployUnreachable(instanceID)`（在 `internal/rental/alerts.go` 加一行：`"实例 " + instanceID + " 的应用部署无法连接容器，详情见日志"`）。

- [ ] **Step 1: 写失败的测试**

`internal/appdeploy/status_test.go`：

```go
package appdeploy_test

import (
	"strings"
	"testing"

	"spark-cloud/internal/appdeploy"
)

func TestParseStatusOutput(t *testing.T) {
	out := []byte(`{"seq":7,"state":"installing","phase":"download","progress":42,"message":"downloading","pid":100,"service_pid":0,"spec_hash":"h","exit_code":0,"at":"2026-10-20T00:00:00Z"}` + "\nalive=0\nlog_size=12\n---\nline1\nline2\n")
	st, alive, tail, err := appdeploy.ParseStatusOutput(out)
	if err != nil || st.Seq != 7 || st.Phase != "download" || st.Progress != 42 || alive || tail != "line1\nline2\n" {
		t.Fatalf("parse = %+v %v %q %v", st, alive, tail, err)
	}
	if _, alive, _, err := appdeploy.ParseStatusOutput([]byte("{\"seq\":1,\"state\":\"running\"}\nalive=1\nlog_size=0\n---\n")); err != nil || !alive {
		t.Fatalf("alive = %v %v", alive, err)
	}
	if _, _, _, err := appdeploy.ParseStatusOutput([]byte("not json\nalive=1\n---\n")); err == nil {
		t.Fatal("bad json accepted")
	}
	if _, _, _, err := appdeploy.ParseStatusOutput([]byte("{\"seq\":1,\"message\":\"" + strings.Repeat("x", 20000) + "\"}\nalive=1\n---\n")); err == nil {
		t.Fatal("oversized status accepted")
	}
}
```

`internal/appdeploy/runner_test.go` 辅助：`newRunnerEnv(t)` = Task 8 的 `env` + shell 模式 `sshtest`（`Root = t.TempDir()`）+ `upstream.New(Config{Signer, Port: srv.Port()}, e.rental)` + `appdeploy.NewRunner(svc, pool, RunnerConfig{AppRoot: filepath.Join(srv.Root(), "root", "data"), HealthAddr: func(int) string { return healthAddr }})`，其中 `health` 是 `httptest.NewServer`（`/health` 与 `/system_stats` 200，`/fail` 500），`srv.SetTarget(8000, healthAddr)`；实例 `running`，`machines.lan_ip = 127.0.0.1`，`instances.ssh_host_pubkey = srv.HostPubLine()`。fake 版本脚本（与 Task 4 `app_ok.sh` 同款，`install` 里 `sleep "${INSTALL_SLEEP:-0}"`，变量 `INSTALL_SLEEP` 为 spec 里的 `int` 变量）。`tick()` = `r.Tick(ctx)` 并断言 `locked`；`until(state)` 循环 `tick` 直到部署 `state == want`（≤ 15 秒，每次 `clk.Advance(PollInterval)`，真实 `time.Sleep(50ms)` 让容器内脚本推进）。用例：
- `TestRunnerQueuedToRunningRegistersPort`：queued → 一轮后 `installing`、`attempt 1`、`started_at`、容器目录里有 `run.sh / app.sh / spec.json / env`（`env` 0600、含 `SPARK_HEALTH_ADDR`）；继续到 `running`：`health ok`、`progress 100`、`finished_at`、`log_tail` 含 `downloading`、`instance_ports` 出现 `(ins, 8000, private, note=应用名, deployment_id=dep)`；事件序列 `queued → installing → running`；`next_poll_at = now + 30s`。
- `TestRunnerInstallFailureAndRedeploy`：fake `app_fail.sh` 版本 → `failed(script_failed)`、`message` 含「退出码 3」、`log_tail` 含 `boom`、`finished_at`；`svc.Redeploy` 到好版本 → `running`。
- `TestRunnerUploadFailed`：`AppRoot` 指到一个只读目录（`chmod 500`）→ `mkdir` 失败 → `failed(upload_failed)`，`message` 含 stderr。
- `TestRunnerUnreachableTenTimes`：`srv.Close()`（拒绝连接）→ 每轮 `unreachable_count + 1`、`next_poll_at = now + 30s`、state 仍 queued；第 10 轮 → `failed(unreachable)` + `alerts` 一条 `app_deploy_unreachable`（再跑两轮不重复）。
- `TestRunnerTimeout`：版本 `timeout_minutes = 1`、`INSTALL_SLEEP = 30`：installing 后 `clk.Advance(61s)` → 下一轮 exec `stop`，`failed(timeout)`，容器内 `_bg` 已死（`status.json.state = failed`、`alive=0`）。
- `TestRunnerStopStartRemove`：running → `svc.Stop` → 一轮 → `stopped`，服务进程已死，端口行仍在；`svc.Start` → queued → running（`log` 含 `skip install`）；`svc.Remove` → `removing` → 一轮 → `removed`，目录消失，端口行删除，审计 `instance.port_delete`。
- `TestRunnerRemoveUnreachable`：removing + `srv.Close()` → 10 轮后仍 `removed`，事件 detail 含「容器不可达，目录未清理」。
- `TestRunnerCrashDetection`：running → 杀掉 `service.pid` 进程 → 三轮 `status`（每轮 `clk.Advance(HealthInterval)`）→ 第 1、2 轮 `health unhealthy, crash_count 1/2, state running`，第 3 轮 `failed(crashed)`。
- `TestRunnerHealthHTTP`：`health_path = /fail`（httptest 返回 500）→ running 后健康轮询 `health = unhealthy`，state 不变；改回 `/health` 路径的另一个部署 `health = ok`。
- `TestRunnerSkipsWhenInstanceNotRunning`：实例 `stopped` → 轮询不 exec（`srv.ConnCount() == 0`）、`next_poll_at = now + 30s`；实例 `released` → `removed(instance_gone)`。
- `TestRunnerDiscardsStaleWriteback`：installing 部署；在 Runner 的 exec 期间（用 `sshtest.HoldDirect`？——exec 不走 direct-tcpip；改为：fake 版本 `install` 里 `sleep 1`，Runner 的 `Tick` 在 goroutine 里跑，主 goroutine 等到容器目录出现后立刻 `svc.Remove`）→ Tick 结束后部署仍是 `removing`（Runner 的 installing 回写被丢弃），日志含 `stale`。
- `TestRunnerClaimsMigratedPort`：预先插 `instance_ports (ins, 8000, private, note='old', deployment_id NULL)` → 部署 running 后该行 `deployment_id = dep`、`note = 应用名`，行数仍 1；另一场景：行属于别的 `deployment_id` → `failed(port_conflict)`。
- `TestRunnerSeqRegressionIgnored`：installing 时把容器 `status.json` 的 `seq` 改小（直接写文件）→ 轮询后 `phase / progress` 不变。
- `TestRunnerRedactsSecrets`：secret `API_KEY = abcdefgh12`，fake `install` 里 `echo "key=$API_KEY"` → `log_tail` 与 `message` 里是 `***`。
- `TestRunnerAdvisoryLock`：两个 `Runner`（同一 pool）并发 `Tick` 各 20 次 → 每次恰好一个 `locked = true`（用一条长事务持有锁来验证「拿不到就跳过」：`conn.Exec("SELECT pg_advisory_lock(hashtext('appdeploy'))")` 后 `Tick` 返回 `locked false`，释放后 `true`）。
- `TestRunnerConcurrencyCap`：20 个实例各一个 queued（fake `install` `sleep 0.5`）→ 一轮内 `installing` 的数量 ≤ 16（`MaxConcurrent` 设 4 更易断言：`srv` 记录同时在跑的 exec 峰值 ≤ 4——`sshtest` 加 `MaxConcurrentExec()` 计数器，Task 5 已有 `opens` 记录；这里用 `ChannelOpens()` 的长度差不够，改为在 Runner 里暴露 `r.Stats().PeakConcurrent`（测试辅助，原子计数））。
- `TestRunnerCleanupPending`：`removed AND cleanup_pending` 且目录存在 → 一轮后目录消失、`cleanup_pending false`。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/appdeploy/ -run 'TestParseStatus|TestRunner' 2>&1 | head -5`
Expected: 编译错误。

- [ ] **Step 3: `status.go`**

```go
package appdeploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

type Status struct {
	Seq        int64  `json:"seq"`
	State      string `json:"state"`
	Phase      string `json:"phase"`
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
	Pid        int    `json:"pid"`
	ServicePid int    `json:"service_pid"`
	SpecHash   string `json:"spec_hash"`
	ExitCode   int    `json:"exit_code"`
	At         string `json:"at"`
}

// ParseStatusOutput 解析 `sh run.sh status; echo ---; tail -c 65536 log` 的组合输出。
func ParseStatusOutput(out []byte) (st Status, alive bool, logTail string, err error) {
	head, tail, _ := bytes.Cut(out, []byte("\n---\n"))
	lines := strings.Split(strings.TrimRight(string(head), "\n"), "\n")
	if len(lines) == 0 || len(lines[0]) > MaxStatusBytes {
		return st, false, "", errors.New("status.json 缺失或超过 16KB")
	}
	if err := json.Unmarshal([]byte(lines[0]), &st); err != nil {
		return st, false, "", errors.New("status.json 不是合法 JSON")
	}
	for _, l := range lines[1:] {
		if l == "alive=1" {
			alive = true
		}
	}
	if len(tail) > MaxLogTail {
		tail = tail[len(tail)-MaxLogTail:]
	}
	return st, alive, string(tail), nil
}
```

- [ ] **Step 4: `runner.go`**

```go
package appdeploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/appdeploy/runtime"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/rental"
)

type RunnerConfig struct {
	PollInterval  time.Duration
	AppRoot       string
	HealthAddr    func(port int) string // 测试注入；nil = 127.0.0.1:<port>
	MaxConcurrent int
}

type RunnerStats struct{ PeakConcurrent int32 }

type Runner struct {
	s    *Service
	up   *upstream.Pool
	cfg  RunnerConfig
	log  *slog.Logger
	cur  atomic.Int32
	peak atomic.Int32
}

func NewRunner(s *Service, up *upstream.Pool, cfg RunnerConfig) *Runner {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = PollInterval
	}
	if cfg.AppRoot == "" {
		cfg.AppRoot = DefaultAppRoot
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = MaxExecPerRound
	}
	return &Runner{s: s, up: up, cfg: cfg, log: s.d.Log}
}

func (r *Runner) Stats() RunnerStats { return RunnerStats{PeakConcurrent: r.peak.Load()} }

// Run 每 PollInterval 一轮直到 ctx 结束；错误只记日志。
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.PollInterval)
	defer t.Stop()
	for {
		if _, _, err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("appdeploy runner tick", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick 执行一轮：拿会话级咨询锁（拿不到 locked=false）→ 选候选 → 并发处理 → 释放锁。
func (r *Runner) Tick(ctx context.Context) (int, bool, error) {
	conn, err := r.s.d.Pool.Acquire(ctx)
	if err != nil {
		return 0, false, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, advisoryLockName).Scan(&locked); err != nil {
		return 0, false, err
	}
	if !locked {
		return 0, false, nil
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, advisoryLockName) //nolint:errcheck
	now := r.s.d.Clock.Now()
	rows, err := r.s.d.Pool.Query(ctx, `SELECT id, instance_id FROM app_deployments
		WHERE next_poll_at <= $1 AND (state IN ('queued', 'installing', 'running', 'removing') OR (state = 'removed' AND cleanup_pending))
		ORDER BY next_poll_at LIMIT 50`, now)
	if err != nil {
		return 0, true, err
	}
	type cand struct{ id, inst string }
	var cands []cand
	perInst := map[string]int{}
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.inst); err != nil {
			rows.Close()
			return 0, true, err
		}
		if perInst[c.inst] >= MaxAppsPerInstance {
			continue
		}
		perInst[c.inst]++
		cands = append(cands, c)
	}
	rows.Close()
	sem := make(chan struct{}, r.cfg.MaxConcurrent)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	processed := 0
	for _, c := range cands {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem; r.cur.Add(-1) }()
			if n := r.cur.Add(1); n > r.peak.Load() {
				r.peak.Store(n)
			}
			done, err := r.step(ctx, id, now)
			mu.Lock()
			if err != nil {
				errs = append(errs, fmt.Errorf("deployment %s: %w", id, err))
			} else if done {
				processed++
			}
			mu.Unlock()
		}(c.id)
	}
	wg.Wait()
	return processed, true, errors.Join(errs...)
}

// target 是实例的连接目标快照（不加锁；过期由下一轮纠正）。
type target struct {
	State      string
	Suspended  bool
	LanIP      string
	HostPubkey string
	MachineID  int64
}

func (t target) upstream(id string) upstream.Target {
	return upstream.Target{InstanceID: id, LanIP: t.LanIP, HostPubkey: t.HostPubkey}
}

func (r *Runner) target(ctx context.Context, instanceID string) (target, error) {
	var t target
	err := r.s.d.Pool.QueryRow(ctx, `SELECT i.state, i.suspended_at IS NOT NULL, m.lan_ip, i.ssh_host_pubkey, i.machine_id
		FROM instances i JOIN machines m ON m.id = i.machine_id WHERE i.id = $1`, instanceID).Scan(&t.State, &t.Suspended, &t.LanIP, &t.HostPubkey, &t.MachineID)
	if db.IsNoRows(err) {
		return target{State: rental.StateReleased}, nil
	}
	return t, err
}

func instanceGone(state string) bool {
	switch state {
	case rental.StateWiping, rental.StateReleased, rental.StateFailed, rental.StateCancelled:
		return true
	}
	return false
}

// step 处理一个部署：事务 1 复核占用 → exec（事务外）→ 事务 2 回写（比对 updated_at）。返回是否真的处理了。
func (r *Runner) step(ctx context.Context, id string, now time.Time) (bool, error) {
	var d Deployment
	err := db.WithTx(ctx, r.s.d.Pool, func(tx pgx.Tx) error {
		var err error
		d, err = scanDeployment(tx.QueryRow(ctx, `SELECT `+depCols+` FROM app_deployments WHERE id = $1 AND next_poll_at <= $2
			AND (state IN ('queued', 'installing', 'running', 'removing') OR (state = 'removed' AND cleanup_pending)) FOR UPDATE SKIP LOCKED`, id, now))
		if err != nil {
			return err
		}
		return scanInto(tx.QueryRow(ctx, `UPDATE app_deployments SET next_poll_at = $2, last_polled_at = $3, updated_at = $3 WHERE id = $1 RETURNING updated_at`,
			id, now.Add(r.cfg.PollInterval), now), &d.UpdatedAt)
	})
	if errors.Is(err, apperr.ErrNotFound) {
		return false, nil // 已被别的轮次处理或状态已变
	}
	if err != nil {
		return false, err
	}
	tg, err := r.target(ctx, d.InstanceID)
	if err != nil {
		return false, err
	}
	if instanceGone(tg.State) {
		return true, r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			return r.s.markRemoved(ctx, tx, cur, ErrCodeInstanceGone, "实例已不存在", now)
		})
	}
	if tg.State != rental.StateRunning || tg.Suspended {
		return true, r.writeback(ctx, d, func(tx pgx.Tx, _ Deployment) error {
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET next_poll_at = $2, health = CASE WHEN state = 'running' THEN 'unknown' ELSE health END WHERE id = $1`, d.ID, now.Add(NotRunningRetry))
			return err
		})
	}
	ut := tg.upstream(d.InstanceID)
	switch {
	case d.State == StateRemoved && d.CleanupPending:
		return true, r.stepCleanup(ctx, d, ut, now)
	case d.Desired == DesiredRemoved:
		return true, r.stepRemove(ctx, d, ut, tg, now)
	case d.State == StateQueued:
		return true, r.stepDeploy(ctx, d, ut, tg, now)
	case d.State == StateInstalling && d.Desired == DesiredStopped, d.State == StateRunning && d.Desired == DesiredStopped:
		return true, r.stepStop(ctx, d, ut, tg, now)
	case d.State == StateInstalling:
		return true, r.stepPoll(ctx, d, ut, tg, now)
	case d.State == StateRunning:
		return true, r.stepHealth(ctx, d, ut, tg, now)
	}
	return false, nil
}

// writeback 事务 2：锁部署行，updated_at 与本轮快照不一致（用户在 exec 期间操作过）则丢弃。
func (r *Runner) writeback(ctx context.Context, d Deployment, fn func(tx pgx.Tx, cur Deployment) error) error {
	return db.WithTx(ctx, r.s.d.Pool, func(tx pgx.Tx) error {
		cur, err := lockDeployment(ctx, tx, d.ID)
		if err != nil {
			return err
		}
		if !cur.UpdatedAt.Equal(d.UpdatedAt) {
			r.log.Info("appdeploy: stale writeback discarded", "deployment", d.ID, "state", cur.State, "desired", cur.Desired)
			return nil
		}
		return fn(tx, cur)
	})
}

type execResult struct {
	stdout, stderr []byte
	exit           int
}

// exec 在容器里执行；连接层错误返回 *upstream.DialError（调用方走 unreachable 分支）。
func (r *Runner) exec(ctx context.Context, ut upstream.Target, cmd string, stdin []byte, timeout time.Duration) (execResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, errb, exit, err := r.up.Exec(ctx, ut, cmd, stdin, MaxLogTail+MaxStatusBytes)
	return execResult{out, errb, exit}, err
}

func isDial(err error) bool {
	var de *upstream.DialError
	return errors.As(err, &de)
}

// unreachable 连续 DialError 计数；达到 UnreachableFailAfter → failed(unreachable) + 告警一次。
func (r *Runner) unreachable(ctx context.Context, d Deployment, tg target, now time.Time, err error) error {
	r.log.Warn("appdeploy: container unreachable", "deployment", d.ID, "instance", d.InstanceID, "count", d.UnreachableCount+1, "err", err)
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		n := cur.UnreachableCount + 1
		if n < UnreachableFailAfter {
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET unreachable_count = $2, next_poll_at = $3, updated_at = $4 WHERE id = $1`, cur.ID, n, now.Add(UnreachableRetry), now)
			return err
		}
		if cur.Desired == DesiredRemoved { // 卸载时容器不可达：仍标 removed，目录留在数据盘
			return r.s.markRemoved(ctx, tx, cur, ErrCodeUnreachable, "容器不可达，目录未清理", now)
		}
		if err := r.s.markFailed(ctx, tx, cur, ErrCodeUnreachable, "连续 10 次无法连接容器", now); err != nil {
			return err
		}
		return r.s.d.Fleet.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "warning", Kind: "app_deploy_unreachable", MachineID: tg.MachineID,
			InstanceID: cur.InstanceID, Message: rental.AlertAppDeployUnreachable(cur.InstanceID)})
	})
}

func (r *Runner) stepDeploy(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	bundle, app, err := r.s.bundleFor(ctx, d, r.cfg.AppRoot, r.cfg.HealthAddr)
	if err != nil {
		return err
	}
	dir := AppDir(r.cfg.AppRoot, app.Slug)
	res, err := r.exec(ctx, ut, UploadCommand(dir), bundle, ExecUploadTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err)
	}
	if err != nil {
		return err
	}
	secrets, _ := openSecrets(r.s.d.Box, d.SecretEnvEnc)
	if res.exit != 0 {
		msg := Redact(lastLine(res.stderr), secrets)
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			return r.s.markFailed(ctx, tx, cur, ErrCodeUploadFailed, "上传失败："+msg, now)
		})
	}
	res, err = r.exec(ctx, ut, "sh "+dir+"/run.sh deploy", nil, ExecStatusTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err)
	}
	if err != nil {
		return err
	}
	if res.exit != 0 {
		code, msg := ErrCodeScriptFailed, Redact(lastLine(res.stderr), secrets)
		if strings.HasPrefix(msg, "disk:") {
			code = ErrCodeDisk
		}
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error { return r.s.markFailed(ctx, tx, cur, code, msg, now) })
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'installing', phase = 'prepare', progress = 0, message = '', health = 'unknown', error_code = '',
			attempt = attempt + 1, started_at = $2, finished_at = NULL, status_seq = 0, unreachable_count = 0, crash_count = 0, next_poll_at = $3, updated_at = $2 WHERE id = $1`,
			cur.ID, now, now.Add(r.cfg.PollInterval)); err != nil {
			return err
		}
		return addEvent(ctx, tx, cur.ID, cur.State, StateInstalling, fmt.Sprintf("开始部署（第 %d 次）", cur.Attempt+1), now)
	})
}

func (r *Runner) statusCmd(slug string) string {
	dir := AppDir(r.cfg.AppRoot, slug)
	return "cd " + dir + " && sh run.sh status; echo ---; tail -c 65536 log 2>/dev/null"
}

func (r *Runner) stepPoll(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	app, ver, err := r.s.appAndVersion(ctx, d)
	if err != nil {
		return err
	}
	secrets, _ := openSecrets(r.s.d.Box, d.SecretEnvEnc)
	res, err := r.exec(ctx, ut, r.statusCmd(app.Slug), nil, ExecStatusTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err)
	}
	if err != nil {
		return err
	}
	if res.exit != 0 {
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			return r.s.markFailed(ctx, tx, cur, ErrCodeScriptFailed, "状态文件缺失", now)
		})
	}
	st, _, logTail, perr := ParseStatusOutput(res.stdout)
	if perr != nil {
		r.log.Warn("appdeploy: bad status output", "deployment", d.ID, "err", perr)
		return nil
	}
	if st.Seq < d.StatusSeq {
		return nil // 回放的旧结果
	}
	logTail, msg := Redact(logTail, secrets), truncate(Redact(st.Message, secrets), MaxMessageLen)
	timeout := time.Duration(ver.Spec.TimeoutMinutes) * time.Minute
	if d.StartedAt != nil && now.Sub(*d.StartedAt) > timeout && st.State == StateInstalling {
		r.exec(ctx, ut, "sh "+AppDir(r.cfg.AppRoot, app.Slug)+"/run.sh stop", nil, ExecStatusTimeout) //nolint:errcheck
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET log_tail = $2, status_seq = $3 WHERE id = $1`, cur.ID, logTail, st.Seq); err != nil {
				return err
			}
			return r.s.markFailed(ctx, tx, cur, ErrCodeTimeout, fmt.Sprintf("超过 %d 分钟未完成", ver.Spec.TimeoutMinutes), now)
		})
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		switch st.State {
		case StateInstalling:
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET phase = $2, progress = $3, message = $4, log_tail = $5, status_seq = $6, unreachable_count = 0, next_poll_at = $7, updated_at = $8 WHERE id = $1`,
				cur.ID, st.Phase, clampProgress(st.Progress), msg, logTail, st.Seq, now.Add(r.cfg.PollInterval), now)
			return err
		case StateRunning:
			if code, err := registerPorts(ctx, tx, cur, ver.Spec.Ports, app.Name, now); err != nil {
				return err
			} else if code != "" {
				return r.s.markFailed(ctx, tx, cur, code, ErrorText[code], now)
			}
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'running', phase = 'health', progress = 100, message = $2, health = 'ok', log_tail = $3, status_seq = $4,
				unreachable_count = 0, crash_count = 0, finished_at = $5, next_poll_at = $6, updated_at = $5 WHERE id = $1`, cur.ID, msg, logTail, st.Seq, now, now.Add(HealthInterval)); err != nil {
				return err
			}
			return addEvent(ctx, tx, cur.ID, StateInstalling, StateRunning, "部署完成", now)
		case StateFailed:
			code := ErrCodeScriptFailed
			switch {
			case strings.HasPrefix(st.Message, "disk:"):
				code = ErrCodeDisk
			case strings.HasPrefix(st.Message, "timeout"):
				code = ErrCodeTimeout
			case st.Phase == "health" && strings.Contains(st.Message, "进程已退出"):
				code = ErrCodeHealthFailed
			}
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET log_tail = $2, status_seq = $3, phase = $4 WHERE id = $1`, cur.ID, logTail, st.Seq, st.Phase); err != nil {
				return err
			}
			return r.s.markFailed(ctx, tx, cur, code, msg, now)
		case StateStopped: // 容器内已停（用户经别的途径停了）：对齐
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'stopped', health = 'unknown', log_tail = $2, status_seq = $3, next_poll_at = $4, updated_at = $5 WHERE id = $1`,
				cur.ID, logTail, st.Seq, now.Add(24*time.Hour), now)
			return err
		}
		return nil
	})
}

func (r *Runner) stepHealth(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	app, ver, err := r.s.appAndVersion(ctx, d)
	if err != nil {
		return err
	}
	secrets, _ := openSecrets(r.s.d.Box, d.SecretEnvEnc)
	res, err := r.exec(ctx, ut, r.statusCmd(app.Slug), nil, ExecStatusTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err)
	}
	if err != nil {
		return err
	}
	if res.exit != 0 {
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			return r.s.markFailed(ctx, tx, cur, ErrCodeScriptFailed, "状态文件缺失", now)
		})
	}
	st, alive, logTail, perr := ParseStatusOutput(res.stdout)
	if perr != nil {
		return nil
	}
	logTail = Redact(logTail, secrets)
	healthy := alive
	if alive && ver.Spec.Ports[0].HealthPath != "" {
		healthy = r.httpHealth(ctx, ut, ver.Spec.Ports[0].Port, ver.Spec.Ports[0].HealthPath)
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		switch {
		case !alive && cur.CrashCount+1 >= CrashedAfter:
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET log_tail = $2, status_seq = $3 WHERE id = $1`, cur.ID, logTail, st.Seq); err != nil {
				return err
			}
			return r.s.markFailed(ctx, tx, cur, ErrCodeCrashed, "服务进程已退出", now)
		case !alive:
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET health = 'unhealthy', crash_count = crash_count + 1, log_tail = $2, status_seq = $3, unreachable_count = 0, next_poll_at = $4, updated_at = $5 WHERE id = $1`,
				cur.ID, logTail, st.Seq, now.Add(HealthInterval), now)
			return err
		default:
			h := HealthUnhealthy
			if healthy {
				h = HealthOK
			}
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET health = $2, crash_count = 0, log_tail = $3, status_seq = $4, unreachable_count = 0, next_poll_at = $5, updated_at = $6 WHERE id = $1`,
				cur.ID, h, logTail, st.Seq, now.Add(HealthInterval), now)
			return err
		}
	})
}

// httpHealth 经 Pool.Dial 请求容器内 127.0.0.1:<port><path>，只看状态码 2xx；响应体最多读 4KB 后关闭。
func (r *Runner) httpHealth(ctx context.Context, ut upstream.Target, port int, path string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return r.up.Dial(ctx, ut, port) }}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1"+path, nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	resp.Body.Read(buf) //nolint:errcheck
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (r *Runner) stepStop(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	app, err := r.s.appOf(ctx, d)
	if err != nil {
		return err
	}
	res, err := r.exec(ctx, ut, "sh "+AppDir(r.cfg.AppRoot, app.Slug)+"/run.sh stop", nil, ExecStatusTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err)
	}
	if err != nil {
		return err
	}
	_ = res
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'stopped', health = 'unknown', phase = '', message = '', unreachable_count = 0, next_poll_at = $2, updated_at = $3 WHERE id = $1`,
			cur.ID, now.Add(24*time.Hour), now); err != nil {
			return err
		}
		return addEvent(ctx, tx, cur.ID, cur.State, StateStopped, "已停止", now)
	})
}

func (r *Runner) stepRemove(ctx context.Context, d Deployment, ut upstream.Target, tg target, now time.Time) error {
	app, err := r.s.appOf(ctx, d)
	if err != nil {
		return err
	}
	_, err = r.exec(ctx, ut, "sh "+AppDir(r.cfg.AppRoot, app.Slug)+"/run.sh remove", nil, ExecUploadTimeout)
	if isDial(err) {
		return r.unreachable(ctx, d, tg, now, err)
	}
	if err != nil {
		return err
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		return r.s.markRemoved(ctx, tx, cur, "", "已卸载", now)
	})
}

func (r *Runner) stepCleanup(ctx context.Context, d Deployment, ut upstream.Target, now time.Time) error {
	app, err := r.s.appOf(ctx, d)
	if err != nil {
		return err
	}
	_, err = r.exec(ctx, ut, "rm -rf "+AppDir(r.cfg.AppRoot, app.Slug), nil, ExecUploadTimeout)
	if err != nil {
		return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
			_, err := tx.Exec(ctx, `UPDATE app_deployments SET next_poll_at = $2 WHERE id = $1`, cur.ID, now.Add(UnreachableRetry))
			return err
		})
	}
	return r.writeback(ctx, d, func(tx pgx.Tx, cur Deployment) error {
		_, err := tx.Exec(ctx, `UPDATE app_deployments SET cleanup_pending = false, updated_at = $2 WHERE id = $1`, cur.ID, now)
		return err
	})
}

func lastLine(b []byte) string {
	s := strings.TrimRight(string(b), "\n")
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return truncate(s, MaxMessageLen)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func clampProgress(p int) int { return max(0, min(100, p)) }

func scanInto(row pgx.Row, dst ...any) error { return row.Scan(dst...) }
```

`Service` 侧配套（`markFailed / markRemoved` 已在 Task 8 的 `deployments.go`；本任务在同文件追加）：
- `bundleFor(ctx, d, appRoot string, healthAddr func(int) string) ([]byte, App, error)`：取 app / version / secrets，`BundleInput{Slug, InstanceID, AppRoot, SpecHash: d.SpecHash, Spec: ver.Spec, Script: ver.Script, Env: d.Env, Secret, HealthAddr: healthAddr != nil ? healthAddr(port) : "", HFEndpoint: s.d.HFEndpoint, PipIndexURL: s.d.PipIndexURL}`，`Bundle(in, runtime.RunSH)`。
- `appAndVersion(ctx, d) (App, Version, error)`、`appOf(ctx, d) (App, error)`。

`internal/rental/alerts.go` 加：

```go
func AlertAppDeployUnreachable(instanceID string) string {
	return "实例 " + instanceID + " 的应用部署无法连接容器，详情见日志"
}
```

- [ ] **Step 5: 运行测试并提交**

Run: `go vet ./... && go test -race ./internal/appdeploy/... 2>&1 | tail -8`
Expected: 全部通过；`TestRunnerDiscardsStaleWriteback` 与 `TestRunnerAdvisoryLock` 用 `-count=5` 再跑一次确认无偶发。

```bash
git add internal/appdeploy internal/rental/alerts.go
git commit -m "feat(appdeploy): runner (advisory lock, deploy / poll / health / stop / remove / cleanup steps, optimistic writeback, port claim, redaction, alert)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 10: rental 钩子（`AppHooks`）：实例 running / 消失 / 换机，下单预装（`Place` / `PlaceBatch` 带 `app_id`）

**Files:**
- Create: `internal/appdeploy/hooks.go`、`internal/rental/apphooks.go`
- Modify: `internal/rental/service.go`（`Service.apps` 字段、`SetAppHooks`）、`internal/rental/results.go`（`startLease / startBooked / failCreating`）、`internal/rental/sweeper.go`（`beginWipe`）、`internal/rental/cancel.go`（`cancelInstance`）、`internal/rental/migrate.go`（`AdminMigrate`）、`internal/rental/batch.go`（`BatchRequest.AppID / AppEnv`、`placeBatch`）、`internal/rental/place.go`（`PlaceRequest.AppID / AppEnv`）
- Test: `internal/appdeploy/hooks_test.go`、`internal/rental/apphooks_test.go`

**Interfaces:**
- Consumes: Task 8 `Service`（`insert`、`deletePorts`、`addEvent`、`resolveVersion`、`checkImage`、`ValidateEnv`）。
- Produces（`internal/rental/apphooks.go`）：

```go
package rental

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
)

// InstallPlan 是 PrepareInstall 返回、QueueInstall 消费的不透明预装计划（appdeploy 的类型，rental 不解释）。
type InstallPlan = any

// AppHooks 由 appdeploy 实现（计划 7）；所有方法在 rental 已持实例行锁的事务里调用，锁顺序 instance → app_deployments → instance_ports。
type AppHooks interface {
	// OnInstanceRunning：实例进入 running（首次开通、续费恢复、换机后新实例）；running / stopped 且 desired=running 的部署回 queued。
	OnInstanceRunning(ctx context.Context, tx pgx.Tx, instanceID string, now time.Time) error
	// OnInstanceGone：实例释放 / 洗机 / 开通失败 / 取消；非终态部署 → removed。reason 为中文短句。
	OnInstanceGone(ctx context.Context, tx pgx.Tx, instanceID, reason string, now time.Time) error
	// OnInstanceMigrated：换机；旧实例部署 → lost，复制为新实例的 queued。
	OnInstanceMigrated(ctx context.Context, tx pgx.Tx, oldID, newID string, now time.Time) error
	// PrepareInstall：下单预装校验（应用 / 镜像兼容 / 变量），失败整单不下。
	PrepareInstall(ctx context.Context, q db.Querier, appID, imageID int64, env map[string]string) (InstallPlan, error)
	// QueueInstall：为一台刚插入的实例写一条 queued 部署。
	QueueInstall(ctx context.Context, tx pgx.Tx, inst Instance, plan InstallPlan, now time.Time) error
}

type noopAppHooks struct{}

func (noopAppHooks) OnInstanceRunning(context.Context, pgx.Tx, string, time.Time) error        { return nil }
func (noopAppHooks) OnInstanceGone(context.Context, pgx.Tx, string, string, time.Time) error   { return nil }
func (noopAppHooks) OnInstanceMigrated(context.Context, pgx.Tx, string, string, time.Time) error { return nil }
func (noopAppHooks) PrepareInstall(context.Context, db.Querier, int64, int64, map[string]string) (InstallPlan, error) {
	return nil, apperr.ErrAppNotFound
}
func (noopAppHooks) QueueInstall(context.Context, pgx.Tx, Instance, InstallPlan, time.Time) error { return nil }

// SetAppHooks 接上应用部署钩子（app.go 在 appdeploy.NewService 之后调用）。
func (s *Service) SetAppHooks(h AppHooks) { s.appHooks = h }

func (s *Service) apps() AppHooks {
	if s.appHooks == nil {
		return noopAppHooks{}
	}
	return s.appHooks
}
```

  （`Service` 结构体加字段 `appHooks AppHooks`；`import "spark-cloud/internal/platform/apperr"`。）
  - `BatchRequest` / `PlaceRequest` 新字段 `AppID *int64; AppEnv map[string]string`；`PlaceRequest` → `BatchRequest` 的转换（`place.go`）带上它们。
  - `appdeploy.Service` 实现 `rental.AppHooks`（`hooks.go`），`var _ rental.AppHooks = (*Service)(nil)`。

- [ ] **Step 1: 写失败的测试**

`internal/rental/apphooks_test.go`（在 `rental_test` 包，用 `helpers_test.go` 的 `env`；加一个记录型 fake）：

```go
package rental_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/rental"
)

type recHooks struct {
	mu    sync.Mutex
	calls []string
	plan  string
}

func (h *recHooks) rec(s string) { h.mu.Lock(); h.calls = append(h.calls, s); h.mu.Unlock() }
func (h *recHooks) OnInstanceRunning(_ context.Context, _ pgx.Tx, id string, _ time.Time) error { h.rec("running:" + id); return nil }
func (h *recHooks) OnInstanceGone(_ context.Context, _ pgx.Tx, id, reason string, _ time.Time) error {
	h.rec("gone:" + id + ":" + reason)
	return nil
}
func (h *recHooks) OnInstanceMigrated(_ context.Context, _ pgx.Tx, oldID, newID string, _ time.Time) error {
	h.rec("migrated:" + oldID + "->" + newID)
	return nil
}
func (h *recHooks) PrepareInstall(_ context.Context, _ db.Querier, appID, imageID int64, env map[string]string) (rental.InstallPlan, error) {
	if h.plan == "fail" {
		return nil, fmt.Errorf("plan failed")
	}
	return fmt.Sprintf("plan:%d:%d:%s", appID, imageID, env["MODEL_ID"]), nil
}
func (h *recHooks) QueueInstall(_ context.Context, _ pgx.Tx, inst rental.Instance, plan rental.InstallPlan, _ time.Time) error {
	h.rec("queue:" + inst.ID + ":" + plan.(string))
	return nil
}

func TestAppHooksCalledOnLifecycle(t *testing.T) {
	e := newEnv(t, 2)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	_, inst := e.place(catalog.D1d)
	e.agentCreated(inst.ID) // Create 成功 → startLease
	if got := h.calls; len(got) != 1 || got[0] != "running:"+inst.ID {
		t.Fatalf("after create: %v", got)
	}
	// 到期停机再续费 Start 成功 → 再一次 running
	e.clock.Advance(25 * time.Hour)
	e.sweep()
	e.renew(inst.ID, catalog.D1d)
	e.agentStarted(inst.ID)
	if got := h.calls; len(got) != 2 || got[1] != "running:"+inst.ID {
		t.Fatalf("after renew start: %v", got)
	}
	// 释放 → beginWipe → gone
	e.release(inst.ID)
	if got := h.calls; len(got) != 3 || got[2] != "gone:"+inst.ID+":实例已释放" {
		t.Fatalf("after release: %v", got)
	}
	// 开通失败 → gone（开通失败）
	_, inst2 := e.place(catalog.D1d)
	e.agentCreateFailed(inst2.ID)
	if got := h.calls; got[len(got)-1] != "gone:"+inst2.ID+":开通失败" {
		t.Fatalf("after create failure: %v", got)
	}
}

func TestAppHooksMigrateOrder(t *testing.T) {
	e := newEnv(t, 2)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	_, inst := e.place(catalog.D1d)
	e.agentCreated(inst.ID)
	moved := e.migrate(inst.ID)
	want := []string{"running:" + inst.ID, "migrated:" + inst.ID + "->" + moved.ID, "gone:" + inst.ID + ":实例已释放"}
	if fmt.Sprint(h.calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", h.calls, want)
	}
	e.agentCreated(moved.ID)
	if h.calls[len(h.calls)-1] != "running:"+moved.ID {
		t.Fatalf("new instance running not hooked: %v", h.calls)
	}
}

func TestPlaceBatchWithApp(t *testing.T) {
	e := newEnv(t, 3)
	h := &recHooks{}
	e.svc.SetAppHooks(h)
	appID := int64(7)
	res, err := e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 2, Duration: catalog.D1d,
		AppID: &appID, AppEnv: map[string]string{"MODEL_ID": "Qwen/Qwen3-4B"}, IdempotencyKey: "k1"})
	if err != nil || len(res.Instances) != 2 {
		t.Fatalf("batch = %+v %v", res, err)
	}
	queued := 0
	for _, c := range h.calls {
		if len(c) > 6 && c[:6] == "queue:" {
			queued++
		}
	}
	if queued != 2 {
		t.Fatalf("queue calls = %v", h.calls)
	}
	// 幂等回放不再调钩子
	n := len(h.calls)
	if _, err := e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 2, Duration: catalog.D1d,
		AppID: &appID, AppEnv: map[string]string{"MODEL_ID": "Qwen/Qwen3-4B"}, IdempotencyKey: "k1"}); err != nil || len(h.calls) != n {
		t.Fatalf("replay = %v calls %d→%d", err, n, len(h.calls))
	}
	// 预装校验失败整单不下：没有实例、没有扣款
	h.plan = "fail"
	before := e.balance()
	if _, err := e.svc.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 1, Duration: catalog.D1d, AppID: &appID}); err == nil {
		t.Fatal("failed plan accepted")
	}
	if e.balance() != before || e.countInstances() != 2 {
		t.Fatalf("partial order: balance %d→%d instances %d", before, e.balance(), e.countInstances())
	}
	// 没有钩子时带 app_id → app_not_found
	e.svc.SetAppHooks(nil)
	if _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d, AppID: &appID}); err == nil || err.Error() != "app_not_found: 应用不存在或未上架" {
		t.Fatalf("no hooks = %v", err)
	}
}
```

（`e.agentCreated / agentStarted / agentCreateFailed / release / renew / migrate / sweep / balance / countInstances` 若 `helpers_test.go` 里尚无同名辅助，按现有 `results_test.go` / `migrate_test.go` 的写法补：`HandleCommandResult(ctx, cmdID, ok, msg)` 取最新 `agent_commands`；`Release` 为 `svc.Release(ctx, userID, id)`——以现有导出方法为准，`grep -n 'func (s \*Service) Release' internal/rental`。）

`internal/appdeploy/hooks_test.go`（在 Task 8 的 `env` 上）：
- `TestOnInstanceRunningRequeues`：三条部署 running / stopped(desired running) / failed → 调 `OnInstanceRunning` → 前两条 `queued`、`phase ''`、`next_poll_at = now`、各一条事件「实例已启动，正在恢复应用」；failed 不变；`removed AND cleanup_pending` 的 `next_poll_at = now`。
- `TestOnInstanceGoneRemovesAndKeepsTerminal`：running + queued + lost → `OnInstanceGone(…, "实例已释放")` → 前两条 `removed`、`error_code instance_gone`、`finished_at`，事件 detail = reason；端口行 `deployment_id` 指向它们的被删；lost 不变。
- `TestOnInstanceMigratedCopies`：旧实例 running(vllm) + failed(webui) + removed(jupyter)，端口行；新实例（直接插一行 `creating`）；`AdminMigrate` 那样先复制端口（`deployment_id NULL`）→ `OnInstanceMigrated` → 旧 vllm / webui → `lost`（事件「实例已更换机器」）；新实例只有 vllm 一条 `queued`（`attempt 0`、同 env / secret / port / version / spec_hash，事件「从 <旧> 迁移，模型需重新下载」）；failed 与 removed 不复制。
- `TestPrepareAndQueueInstall`：`PrepareInstall` 对 draft 应用 → `app_not_found`；镜像不兼容 → `app_image_mismatch`；变量错误 → `app_env_*`；合法 → `QueueInstall` 插 queued（`user_id = inst.UserID`）。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/rental/ -run 'TestAppHooks|TestPlaceBatchWithApp' 2>&1 | head -5; go test ./internal/appdeploy/ -run 'TestOnInstance|TestPrepareAndQueue' 2>&1 | head -5`
Expected: 编译错误。

- [ ] **Step 3: rental 侧改动**

`results.go` `startLease`（两处 UPDATE 之后、`refreshLease` 之前，`startBooked` 同样）：

```go
	if err := s.apps().OnInstanceRunning(ctx, tx, inst.ID, now); err != nil {
		return err
	}
```

`failCreating` 在 `recordEvent` 之后：`if err := s.apps().OnInstanceGone(ctx, tx, inst.ID, "开通失败", now); err != nil { return err }`。`beginWipe` 在 `recordEvent` 之后：`OnInstanceGone(…, "实例已释放", now)`。`cancelInstance` 在 `UPDATE instances SET state = 'cancelled'` 之后：`OnInstanceGone(…, "预订取消", now)`。`AdminMigrate` 在复制 `instance_ports` 之后、`beginWipe(old)` 之前：`OnInstanceMigrated(ctx, tx, old.ID, id, now)`。（`handleCommandResult` 的 wipe → released 分支不调：`beginWipe` 已做。）

`batch.go`：`BatchRequest` 加 `AppID *int64; AppEnv map[string]string`；`placeBatch` 在取镜像之后：

```go
			var plan InstallPlan
			if req.AppID != nil {
				if plan, err = s.apps().PrepareInstall(ctx, tx, *req.AppID, req.ImageID, req.AppEnv); err != nil {
					return err
				}
			}
```

`placeOnMachines` 回调里 `insertLease` 之后：

```go
				if req.AppID != nil {
					if err := s.apps().QueueInstall(ctx, sp, inst, plan, now); err != nil {
						return err
					}
				}
```

`place.go`：`PlaceRequest` 加同名字段并传给 `BatchRequest`。幂等回放路径（`replayByKey`）不经过这两处，自然不重复。

- [ ] **Step 4: `appdeploy/hooks.go`**

```go
package appdeploy

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/rental"
)

var _ rental.AppHooks = (*Service)(nil)

// OnInstanceRunning：running / stopped 且 desired=running 的部署统一回 queued（容器停过，进程必然不在；deploy 幂等）；failed 不动；
// 实例停机期间卸载的（removed AND cleanup_pending）排到下一轮清理目录。
func (s *Service) OnInstanceRunning(ctx context.Context, tx pgx.Tx, instanceID string, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id, state FROM app_deployments WHERE instance_id = $1 AND desired = 'running' AND state IN ('running', 'stopped') ORDER BY created_at FOR UPDATE`, instanceID)
	if err != nil {
		return err
	}
	type row struct{ id, from string }
	var changed []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.from); err != nil {
			rows.Close()
			return err
		}
		changed = append(changed, r)
	}
	rows.Close()
	for _, r := range changed {
		if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'queued', phase = '', progress = 0, message = '', health = 'unknown', error_code = '',
			unreachable_count = 0, crash_count = 0, next_poll_at = $2, updated_at = $2 WHERE id = $1`, r.id, now); err != nil {
			return err
		}
		if err := addEvent(ctx, tx, r.id, r.from, StateQueued, "实例已启动，正在恢复应用", now); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE app_deployments SET next_poll_at = $2 WHERE instance_id = $1 AND state = 'removed' AND cleanup_pending`, instanceID, now)
	return err
}
```

```go
// OnInstanceGone：非终态部署 → removed(instance_gone)；端口随实例失效，行按 deployment_id 删除（与卸载一致）。
func (s *Service) OnInstanceGone(ctx context.Context, tx pgx.Tx, instanceID, reason string, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT `+depCols+` FROM app_deployments WHERE instance_id = $1 AND state NOT IN ('removed', 'lost') ORDER BY created_at FOR UPDATE`, instanceID)
	if err != nil {
		return err
	}
	deps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Deployment, error) { return scanDeployment(r) })
	if err != nil {
		return err
	}
	for _, d := range deps {
		if err := s.markRemoved(ctx, tx, d, ErrCodeInstanceGone, reason, now); err != nil {
			return err
		}
	}
	return nil
}

// OnInstanceMigrated：旧实例非终态部署 → lost；state ∉ {failed} 且 desired ≠ removed 的复制为新实例的 queued（attempt 0，端口由 Runner 认领已复制的行）。
func (s *Service) OnInstanceMigrated(ctx context.Context, tx pgx.Tx, oldID, newID string, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT `+depCols+` FROM app_deployments WHERE instance_id = $1 AND state NOT IN ('removed', 'lost') ORDER BY created_at FOR UPDATE`, oldID)
	if err != nil {
		return err
	}
	deps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Deployment, error) { return scanDeployment(r) })
	if err != nil {
		return err
	}
	for _, d := range deps {
		if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'lost', finished_at = $2, updated_at = $2 WHERE id = $1`, d.ID, now); err != nil {
			return err
		}
		if err := addEvent(ctx, tx, d.ID, d.State, StateLost, "实例已更换机器", now); err != nil {
			return err
		}
		if d.State == StateFailed || d.Desired == DesiredRemoved {
			continue
		}
		var nd Deployment
		if nd, err = scanDeployment(tx.QueryRow(ctx, `INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, desired, env, secret_env_enc, port, spec_hash, next_poll_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'queued', 'running', $6, $7, $8, $9, $10, $10, $10) RETURNING `+depCols,
			randx.ID("dep-", 12), newID, d.UserID, d.AppID, d.AppVersionID, mustJSON(d.Env), d.SecretEnvEnc, d.Port, d.SpecHash, now)); err != nil {
			return err
		}
		if err := addEvent(ctx, tx, nd.ID, "", StateQueued, "从 "+oldID+" 迁移，模型需重新下载", now); err != nil {
			return err
		}
	}
	return nil
}

type installPlan struct {
	app    App
	ver    Version
	plain  map[string]string
	secret map[string]string
}

// PrepareInstall 下单预装校验：published 应用的最新 published 版本、镜像兼容、变量。
func (s *Service) PrepareInstall(ctx context.Context, q db.Querier, appID, imageID int64, env map[string]string) (rental.InstallPlan, error) {
	app, ver, err := s.resolveVersion(ctx, q, appID, nil)
	if err != nil {
		return nil, err
	}
	if err := s.checkImage(ctx, q, ver, imageID); err != nil {
		return nil, err
	}
	plain, secret, err := ValidateEnv(ver.Spec, env)
	if err != nil {
		return nil, err
	}
	return &installPlan{app: app, ver: ver, plain: plain, secret: secret}, nil
}

func (s *Service) QueueInstall(ctx context.Context, tx pgx.Tx, inst rental.Instance, plan rental.InstallPlan, now time.Time) error {
	p, ok := plan.(*installPlan)
	if !ok || p == nil {
		return nil
	}
	d, err := s.insert(ctx, tx, inst, inst.UserID, p.app, p.ver, p.plain, p.secret, now)
	if err != nil {
		return err
	}
	if err := addEvent(ctx, tx, d.ID, "", StateQueued, "预装："+p.app.Name+" "+p.ver.Version+"，实例开通后自动部署", now); err != nil {
		return err
	}
	return audit.Record(ctx, tx, now, audit.Actor{UserID: inst.UserID}.Entry("instance.app_deploy", "instance", inst.ID,
		map[string]any{"deployment": d.ID, "app": p.app.Slug, "version_id": p.ver.ID, "preinstall": true}))
}
```

（`resolveVersion / checkImage / insert / markRemoved / mustJSON` 来自 Task 8 的 `deployments.go`（参数 `db.Querier`）；import 加 `spark-cloud/internal/platform/randx`。）

- [ ] **Step 5: 运行测试并提交**

Run: `go vet ./... && go test ./internal/rental/ ./internal/appdeploy/ 2>&1 | tail -6`
Expected: 全部通过（`rental` 原有 ~60 个测试不受影响：钩子默认 noop）。

```bash
git add internal/rental internal/appdeploy
git commit -m "feat(rental): AppHooks (running / gone / migrated / preinstall) wired into startLease, beginWipe, failCreating, cancel, migrate, PlaceBatch

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 11: Agent：`StateReport.images`（proto、`docker image ls`、fake Agent 注入）；网关写 `machines.cached_images`

**Files:**
- Modify: `api/proto/agent/v1/agent.proto`、`internal/agentpb/agent.pb.go`（`make proto` 生成）、`internal/agent/runtime/runtime.go`（`Runtime.Images`）、`internal/agent/runtime/docker/docker.go`、`internal/agent/runtime/fake/fake.go`（`SetImages`）、`internal/agent/executor/ops.go`（`Images(ctx)`）、`internal/agent/client/client.go`（`stateReport`）、`internal/agent/fakeagent/fakeagent.go`（`Options.Images`、`Agent.SetImages`）、`internal/agentgw/convert.go`、`internal/agentgw/connect.go`（`handleReport`）、`internal/fleet/agent.go`（`StateReport.Images`、`RecordCachedImages`）、`internal/fleet/machines.go`（`Machine.CachedImages / CachedImagesAt`、`machineCols`、`scanMachine`）
- Test: `internal/agent/runtime/docker/docker_test.go`（追加）、`internal/agent/executor/executor_test.go`（追加）、`internal/agentgw/connect_test.go`（追加 `TestReportImagesRecorded`、`TestReportWithoutImagesKeepsCache`）、`internal/fleet/agent_test.go`（若无则新建）

**Interfaces:**
- Produces:
  - proto：`message StateReport { repeated InstanceState instances = 1; repeated string images = 2; }`（注释：已缓存镜像 `repository:tag`，≤ 200 项；旧 Agent 不发）。
  - `runtime.Runtime` 接口新增 `Images(ctx context.Context) ([]string, error)`；docker 实现 `docker image ls --format '{{.Repository}}:{{.Tag}}' --filter dangling=false`，过滤含 `<none>` 的行、去重、排序、截到 200 项、每项截到 256 字节；fake 实现返回 `SetImages` 设置的列表。
  - `executor.Executor.Images(ctx) []string`（`reportTimeout` 内；出错返回 nil 并记日志）。
  - `client.stateReport` 填 `Images: c.ex.Images(ctx)`。
  - `fakeagent.Options.Images []string`、`(*Agent).SetImages(imgs []string)`（设到 `Runtime`）。
  - `fleet.StateReport.Images []string \`json:"-"\``（不进 `last_state_report`）；`toStateReport` 填它；`func (s *Service) RecordCachedImages(ctx, machineID int64, images []string) error`（`images == nil` 不写；截断规则同上；`UPDATE machines SET cached_images = $2, cached_images_at = $3`）；`handleReport` 在 `RecordStateReport` 之后调用（`rep.Images != nil` 时；`RecordStateReport` 的 JSON 仍只含 `instances`——`last_state_report` 不存镜像列表，`fleet.StateReport.Images` 打 `json:"-"`）。
  - `fleet.Machine` 新字段 `CachedImages []string \`json:"cached_images"\``、`CachedImagesAt *time.Time \`json:"cached_images_at"\``（`machineCols` 与 `scanMachine` 同步）。

- [ ] **Step 1: proto 与生成**

`agent.proto` 的 `StateReport` 改为：

```proto
message StateReport {
  repeated InstanceState instances = 1;
  // 已缓存镜像的 repository:tag 列表（docker image ls，≤ 200 项）；计划 7 新增，旧 Agent 不发该字段，平台不覆盖已有记录。
  repeated string images = 2;
}
```

Run: `make proto && git diff --stat internal/agentpb`
Expected: `agent.pb.go` 变化只涉及 `StateReport`（新字段 + getter），`agent_grpc.pb.go` 无变化。

- [ ] **Step 2: 写失败的测试**

`internal/agent/runtime/docker/docker_test.go` 追加（按文件里现有的 fake `cmdrun` 记录器写法）：`TestImagesParsesAndFilters`：runner 对 `docker image ls --format {{.Repository}}:{{.Tag}} --filter dangling=false` 返回 `"spark/llm-serving:25.09\n<none>:<none>\nnvcr.io/nvidia/vllm:25.09-py3\nspark/llm-serving:25.09\n"` → `Images` 返回 `["nvcr.io/nvidia/vllm:25.09-py3", "spark/llm-serving:25.09"]`（去重、排序、无 `<none>`）；断言记录的命令行参数快照；250 行输入 → 200 项；一行 300 字节 → 截到 256。

`internal/agent/executor/executor_test.go` 追加 `TestImagesReportedFromRuntime`：fake runtime `SetImages([]string{"a:1"})` → `ex.Images(ctx) == ["a:1"]`；`FailNext("images", err)` → nil。

`internal/agentgw/connect_test.go` 追加：

```go
func TestReportImagesRecorded(t *testing.T) {
	e := newEnv(t) // 现有辅助：真实网关 + 嵌入式 PG + fake Agent
	ag := e.startAgent(t, fakeagent.Options{Images: []string{"spark/llm-serving:25.09", "spark/pytorch:25.09"}})
	e.waitReport(t, ag)
	m := e.machine(t, ag)
	if fmt.Sprint(m.CachedImages) != "[spark/llm-serving:25.09 spark/pytorch:25.09]" || m.CachedImagesAt == nil {
		t.Fatalf("cached = %v at %v", m.CachedImages, m.CachedImagesAt)
	}
	// 截断：201 项 → 200；超长项 → 256 字节
	many := make([]string, 201)
	for i := range many {
		many[i] = fmt.Sprintf("img%03d:%s", i, strings.Repeat("x", 300))
	}
	ag.SetImages(many)
	before := e.gw.ReportCount(m.ID)
	ag.Client.SendStateReport(e.ctx)
	e.waitUntil(t, func() bool { return e.gw.ReportCount(m.ID) > before })
	m = e.machine(t, ag)
	if len(m.CachedImages) != 200 || len(m.CachedImages[0]) != 256 {
		t.Fatalf("truncation: %d items, first len %d", len(m.CachedImages), len(m.CachedImages[0]))
	}
}

func TestReportWithoutImagesKeepsCache(t *testing.T) {
	e := newEnv(t)
	ag := e.startAgent(t, fakeagent.Options{Images: []string{"spark/comfyui:25.09"}})
	e.waitReport(t, ag)
	m := e.machine(t, ag)
	at := m.CachedImagesAt
	// 模拟旧 Agent：直接向流发一条没有 images 的 StateReport
	ag.SetImages(nil)
	before := e.gw.ReportCount(m.ID)
	ag.Client.SendStateReport(e.ctx)
	e.waitUntil(t, func() bool { return e.gw.ReportCount(m.ID) > before })
	m = e.machine(t, ag)
	if len(m.CachedImages) != 1 || !m.CachedImagesAt.Equal(*at) {
		t.Fatalf("cache overwritten by report without images: %v %v", m.CachedImages, m.CachedImagesAt)
	}
}
```

（`fakeagent` 的 `SetImages(nil)` 让 fake runtime 返回 nil → `stateReport` 不设 `Images` 字段 → 与旧 Agent 的消息一致。`newEnv / startAgent / waitReport / machine / waitUntil` 按 `connect_test.go` 现有辅助的真实名字调整。）

- [ ] **Step 3: 实现**

`runtime.go` 接口加：

```go
	// Images 返回本机已缓存的镜像 repository:tag（去重排序、≤ 200 项、每项 ≤ 256 字节；不含 <none>）。
	Images(ctx context.Context) ([]string, error)
```

`docker.go`：

```go
const (
	maxImages   = 200
	maxImageLen = 256
)

func (r *Runtime) Images(ctx context.Context) ([]string, error) {
	out, err := r.Run.Run(ctx, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "dangling=false")
	if err != nil {
		return nil, err
	}
	return NormalizeImages(strings.Split(string(out), "\n")), nil
}

// NormalizeImages 过滤 <none>、去重、排序、截断（Agent 与网关共用同一规则）。
func NormalizeImages(lines []string) []string {
	seen := map[string]bool{}
	var list []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.Contains(l, "<none>") || seen[l] {
			continue
		}
		if len(l) > maxImageLen {
			l = l[:maxImageLen]
		}
		seen[l] = true
		list = append(list, l)
	}
	sort.Strings(list)
	if len(list) > maxImages {
		list = list[:maxImages]
	}
	return list
}
```

（网关侧同样的规则在 `fleet.RecordCachedImages` 里再做一遍，用 `MaxCachedImages / MaxCachedImageLen` 常量——fleet 不能 import agent 包，复制这 15 行。）

`fake.go`：字段 `images []string`；`SetImages(imgs []string)`；`Images(ctx)`：`wait(ctx, "images")`、`fail("images")`、返回副本（nil 保持 nil）。

`executor/ops.go`：

```go
// Images 本机已缓存镜像（StateReport.images）；出错返回 nil（旧平台 / 失败时不发该字段）。
func (e *Executor) Images(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, e.reportTimeout)
	defer cancel()
	imgs, err := e.d.Runtime.Images(ctx)
	if err != nil {
		e.d.Log.Warn("list images", "err", err)
		return nil
	}
	return imgs
}
```

`client.go` `stateReport`：`State: &agentpb.StateReport{Instances: states, Images: c.ex.Images(ctx)}`。

`fakeagent.go`：`Options.Images []string`；`start` 里 `a.Runtime.SetImages(o.Images)`；`func (a *Agent) SetImages(imgs []string) { a.Runtime.SetImages(imgs) }`。

`fleet/agent.go`：

```go
type StateReport struct {
	Instances []InstanceState `json:"instances"`
	Images    []string        `json:"-"` // 计划 7：已缓存镜像；nil = 旧 Agent 未上报（不覆盖 machines.cached_images）
}

const (
	MaxCachedImages   = 200
	MaxCachedImageLen = 256
)

// RecordCachedImages 写 machines.cached_images / cached_images_at；images 为 nil 时不写（旧 Agent）。
func (s *Service) RecordCachedImages(ctx context.Context, machineID int64, images []string) error {
	if images == nil {
		return nil
	}
	list := normalizeImages(images)
	_, err := s.pool.Exec(ctx, `UPDATE machines SET cached_images = $2, cached_images_at = $3, updated_at = $3 WHERE id = $1`, machineID, list, s.clock.Now())
	return err
}
```

`agentgw/convert.go` `toStateReport`：`if r.Images != nil { out.Images = r.GetImages() }`——注意 proto3 的 `GetImages()` 对未设置字段返回 nil，对空列表（新 Agent 没有镜像）也返回 nil：**新 Agent 本机没有任何镜像时等同「未知」**，可接受（机器刚装好没有镜像的窗口极短）；为区分，Agent 侧 `Images()` 在列表为空时返回 `[]string{}`，`stateReport` 把空切片原样放进 proto——proto3 `repeated` 空列表在线上不编码，接收端仍是 nil。所以**不做区分**，在 `handleReport` 里按 nil 处理，并在 `followups-plan7.md` 记一条。`handleReport` 在 `RecordStateReport` 成功后：

```go
	if rep.Images != nil {
		if err := s.d.Fleet.RecordCachedImages(ctx, c.machineID, rep.Images); err != nil {
			s.d.Log.Warn("record cached images", "machine", c.machineID, "err", err)
		}
	}
```

`fleet/machines.go`：`machineCols` 加 `cached_images, cached_images_at`，`scanMachine` 扫进 `&m.CachedImages, &m.CachedImagesAt`（`[]string` 由 pgx 直接映射 `text[]`）。所有 `SELECT machineCols` 的地方自动带上；`INSERT` 不涉及。

- [ ] **Step 4: 运行测试并提交**

Run: `go vet ./... && go test ./internal/agent/... ./internal/agentgw/ ./internal/fleet/ ./cmd/spark-agent/ 2>&1 | tail -8`
Expected: 全部通过；`make agent` 能编译出 arm64 二进制（`shasum` 输出记下，Task 22 安装用）。

```bash
git add api/proto internal/agentpb internal/agent internal/agentgw internal/fleet
git commit -m "feat(agent): StateReport.images (docker image ls), gateway records machines.cached_images; old agents keep working

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 12: 控制台 API（`/apps*`、`/instances/{id}/apps*`、下单 `app_id`、`apps_summary`）

**Files:**
- Create: `internal/api/apps.go`、`internal/appdeploy/view.go`
- Modify: `internal/api/server.go`（`Apps *appdeploy.Service`、`AppsLimit int`、路由）、`internal/api/instances.go`（`instanceView.AppsSummary`、`views()` 批量填充、`handlePlaceOrder` 带 `app_id / app_env`）、`internal/api/booking.go`（`bookingRequest` 加 `app_id / app_env`，`handleCreateBooking` 传入）
- Test: `internal/api/apps_test.go`（新建；用 `api_test.go` 的 `newEnv`，加 `appdeploy.Service`）

**Interfaces:**
- Consumes: Task 8 / 10 `appdeploy.Service`、Task 7 `Catalog.ListPublished / AppBySlug`、`rental.PlaceRequest.AppID / AppEnv`、`BatchRequest.AppID / AppEnv`。
- Produces（`internal/appdeploy/view.go`）：
  - `type View struct` 与前端 `Deployment` 一一对应（字段与 JSON 标签见 Task 1 `apps.ts`：`id, instance_id, app_id, app_slug, app_name, app_icon, version_id, version, latest_version_id, latest_version, update_available, state, desired, phase, progress, message, health, error_code, env, secret_env, spec, port, url, links, attempt, cleanup_pending, started_at, finished_at, created_at, updated_at`）；`func (s *Service) Views(ctx, deps []Deployment) ([]View, error)`（应用 / 版本 / 最新 published 版本缓存；`url` = 端口行存在且 `PortURL != nil` 时 `PortURL(instanceID, port)`；`links[i].url = url + path`；`secret_env` 只给 `true`）。
  - `type Summary struct{ ID, Name, State string; Progress int; URL *string }`（JSON `id, name, state, progress, url`）；`func (s *Service) Summaries(ctx, instanceIDs []string) (map[string][]Summary, error)`。
  - `type AdminView struct{ View; LogTail string \`json:"log_tail"\`; UserID int64 \`json:"user_id"\` }`；`func (s *Service) AdminViews(ctx, deps []Deployment) ([]AdminView, error)`（后台实例详情 / 部署列表用，Task 13）。
  - `type CatalogView struct` = `AppCatalogItem`（`id, slug, name, summary, description?, category, icon, version{id, version, ports, env, links, timeout_minutes}, images[{id,name}]`）；`func (s *Service) CatalogViews(ctx, withDescription bool) ([]CatalogView, error)`；`CatalogViewBySlug(ctx, slug) (CatalogView, error)`。
- 路由（`api/server.go`）：组外、`httpx.NoStore + s.calendarRateLimit`（复用匿名 IP 限流器）：`GET /apps`、`GET /apps/{slug}`；`requireUser` 组内：`GET /instances/{id}/apps`、`POST /instances/{id}/apps`（201）、`GET /instances/{id}/apps/{dep}`、`POST /instances/{id}/apps/{dep}/redeploy`、`POST …/stop`、`POST …/start`、`DELETE /instances/{id}/apps/{dep}`、`GET …/log`、`GET …/events`。写操作经 `s.appsLimit.allow(userID)`（每小时 60；`AppsLimit` 零值 = 60，`DisableRateLimits` 时 app 传 `noRateLimit`）→ 超限 `rate_limited`。
- 请求体：`POST /instances/{id}/apps {app_id, app_version_id?, env}`；`redeploy {app_version_id?, env?}`（`env` 字段**出现**即 `EnvSet = true`，用 `json.RawMessage` 判断是否出现）；`POST /orders` 与 `POST /bookings` 的 body 新增 `app_id?: number`、`app_env?: {}`；`useIdempotencyKey` 的派生在前端（Task 17）。
- `instanceView` 新字段 `AppsSummary []appdeploy.Summary \`json:"apps_summary"\``（空数组非 null）。

- [ ] **Step 1: 写失败的测试**

`internal/api/apps_test.go` 用例（HTTP 级，`newEnv` + 已登录用户 + 一台 running 实例，published app 由 `appdeploy.Catalog` 直接造）：
- `TestAppsCatalogAnonymous`：未登录 `GET /apps` 200，含 `vllm`，每项有 `version.env`、`images`；`GET /apps/vllm` 带 `description`；`GET /apps/nope` 404 `app_not_found`；draft 应用不出现。
- `TestInstanceAppsFlow`：`POST /instances/{id}/apps {"app_id", "env": {"API_KEY": "abcdefgh"}}` → 201，`state queued`、`secret_env == {"API_KEY": true}`、`env` 无 `API_KEY`、`url == null`；`GET /instances/{id}/apps` 列表含它且 `apps_summary` 在 `GET /instances/{id}` 里出现 `[{id, name, state: "queued", progress: 0, url: null}]`；插入端口行后 `url == "https://ins-…-8000.spark.test/"`（`PortURL` 来自 `ingress.Auth`）；`links[0].url` 以 `/docs` 结尾。
- `TestInstanceAppsStateOps`：直接 `UPDATE` 造 `running` → `stop` 200 `desired stopped`；造 `stopped` → `start` → `queued`；造 `failed` → `redeploy {"env": {"MODEL_ID": "Qwen/Qwen3-4B"}}` → `queued`、`env.MODEL_ID` 更新、`secret_env.API_KEY` 仍 `true`；`redeploy {}`（无 env 字段）→ 变量不变；`DELETE` → `removing`（实例 running）；别人的实例 → 404；`redeploy` 在 `installing` → 409 `app_invalid_state`。
- `TestInstanceAppsLogAndEvents`：`GET …/log` 返回 `{log, live: false, at}`（测试里 `Upstream` 为 nil → 回落）；`GET …/events` 返回按 `id` 升序的事件 ≤ 50 条。
- `TestOrderWithApp`：`POST /orders {product_id, duration, image_id, app_id, app_env}` → 201 且 `instance.apps_summary` 有一条 queued；镜像不兼容 → 409 `app_image_mismatch` 且没有新实例、余额不变；`POST /bookings {…, app_id}` 同样；`app_env` 缺必填 → 400 `app_env_required`。
- `TestAppsWriteRateLimit`：`AppsLimit = 2` → 第 3 次写操作 429 `rate_limited`；读不受限。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/api/ -run 'TestApps|TestInstanceApps|TestOrderWithApp' 2>&1 | head -5`
Expected: 编译错误。

- [ ] **Step 3: 实现**

`view.go` 的 `Views`：一次查出涉及的 `apps`、`app_versions`（`id = ANY`）、每个 app 的最新 published 版本（`SELECT DISTINCT ON (app_id) id, version FROM app_versions WHERE app_id = ANY($1) AND status = 'published' ORDER BY app_id, published_at DESC`）、端口行（`SELECT deployment_id, port FROM instance_ports WHERE deployment_id = ANY($1)`）；`update_available = latest.ID != d.AppVersionID && latest 存在`；`env` 为 `d.Env`（空 map 而非 null）。

`api/apps.go` 处理器按 Task 8 的方法一一映射；`handleAppsRedeploy`：

```go
	var req struct {
		VersionID *int64          `json:"app_version_id"`
		Env       json.RawMessage `json:"env"`
	}
	…
	r2 := appdeploy.RedeployRequest{UserID: u.ID, InstanceID: id, DeploymentID: dep, VersionID: req.VersionID, IP: clientIP(r), EnvSet: len(req.Env) > 0 && string(req.Env) != "null"}
	if r2.EnvSet {
		if err := json.Unmarshal(req.Env, &r2.Env); err != nil { … bad_request }
	}
```

`instances.go`：`views()` 在 `PortCounts` 之后 `s.Apps.Summaries(ctx, ids)`（`s.Apps == nil` 时跳过——现有测试没有 Apps）；`handlePlaceOrder` 的 req 加 `AppID *int64 \`json:"app_id"\``、`AppEnv map[string]string \`json:"app_env"\`` 传给 `PlaceRequest`；`booking.go` 的 `bookingRequest` 同样。

`server.go` 字段 `Apps *appdeploy.Service`、`AppsLimit int`、`appsLim *userLimiter`；`userLimiter`：

```go
// userLimiter 进程内按用户的滑动一小时限流（应用部署类写操作，规格 §6.2：每用户每小时 60 次）。
type userLimiter struct {
	mu    sync.Mutex
	limit int
	hits  map[int64][]time.Time
}

func (l *userLimiter) allow(uid int64, now time.Time) bool { /* 同 ipLimiter，窗口 time.Hour，键 int64 */ }
```

- [ ] **Step 4: 运行测试并提交**

Run: `go vet ./... && go test ./internal/api/ 2>&1 | tail -5`
Expected: 全部通过（原有 api 测试因 `Apps == nil` 跳过摘要）。

```bash
git add internal/api internal/appdeploy/view.go
git commit -m "feat(api): app catalog, instance app deployments (create / redeploy / stop / start / remove / log / events), preinstall on order, apps_summary

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 13: 后台 API（应用目录、版本、发布 + TOTP、预拉取、部署列表 / 详情 / 强制卸载、实例 / 机器 / 仪表盘增量）+ 角色矩阵 + 审计

**Files:**
- Create: `internal/adminapi/apps.go`、`internal/adminapi/deployments.go`
- Modify: `internal/adminapi/server.go`（`Apps *appdeploy.Service`、`AppCatalog *appdeploy.Catalog`、`appRoutes(g)`）、`internal/adminapi/instances.go`（`handleInstance` 加 `apps`）、`internal/adminapi/machines.go`（`handleMachine` 响应已含 `Machine.CachedImages`——确认 JSON 里出现）、`internal/adminapi/dashboard.go`（`apps_installing / apps_running / apps_failed_24h`）、`internal/adminapi/permissions_test.go`（`expectedRoutes`）、`internal/appdeploy/service.go`（`Counts(ctx, now) (installing, running, failed24h int, err error)`）
- Test: `internal/adminapi/apps_test.go`

**Interfaces:**
- 路由与角色（Global Constraints「角色」）：

| 方法 | 路径 | 角色 |
|---|---|---|
| GET | `/apps` | all |
| POST | `/apps` | super |
| PUT | `/apps/{id}` | super |
| DELETE | `/apps/{id}` | super |
| GET | `/apps/{id}/versions` | all（每项带 `coverage`） |
| POST | `/apps/{id}/versions` | super（`?validate=1` 只校验） |
| PUT | `/apps/{id}/versions/{vid}` | super（`?validate=1` 只校验） |
| POST | `/apps/{id}/versions/{vid}/publish` | super + TOTP（body `{totp}`） |
| POST | `/apps/{id}/versions/{vid}/prepull` | ops → `{machines}` |
| GET | `/apps/{id}/versions/{vid}/script` | all → `{script}`（不审计） |
| GET | `/deployments?state=&app_id=&user_id=&instance_id=&page=` | all |
| GET | `/deployments/{dep}` | all → `{deployment: View, events, log_tail}` |
| POST | `/deployments/{dep}/force-remove` | ops（`{reason}` 必填） |

- `GET /instances/{id}` 响应新增 `apps: []View`（含 `log_tail` 字段：后台视图用 `AdminView = View + LogTail`）；`GET /machines/{id}` 的 `machine` 已含 `cached_images / cached_images_at`；`GET /dashboard` 新增三个数字。
- 审计动作见 Global Constraints；`publish` 用 `withStepUp`。

- [ ] **Step 1: 写失败的测试**

`permissions_test.go` 的 `expectedRoutes` 追加 13 行（按上表）。`internal/adminapi/apps_test.go` 用例：
- `TestAdminAppCRUD`：super 创建 / 更新 / 删除；ops `POST /apps` → 403；finance `GET /apps` 200。
- `TestAdminVersionValidateAndPublish`：`POST /apps/{id}/versions?validate=1` 非法 spec → 400 `app_version_invalid`（消息含 `ports[0].port`）且没有写入；合法 → `{ok: true, spec_hash}`；真正创建 → 201 draft；`publish` 无 TOTP → 401/403（按 `withStepUp` 现有错误码）；带 TOTP → 200 `status published`，`agent_commands` 有 `pull_image`，审计 `app.version.publish` diff 含 `spec_hash`；再 `PUT` 改 spec → 409 `app_version_frozen`；`GET /apps/{id}/versions` 每项 `coverage[0] = {image_id, image_name, cached, online, unknown}`。
- `TestAdminDeploymentsListAndForceRemove`：造两条部署（不同状态）→ `GET /deployments?state=running` 只一条；`GET /deployments/{dep}` 含 `events` 与脱敏 `log_tail`（`secret` 值不出现）且 `deployment.secret_env.API_KEY == true`；ops `force-remove` 无 reason → 400；带 reason → `state removing`（实例 running）/ `removed`（实例 stopped）+ 事件「平台卸载：<reason>」+ 审计 `app.deployment.force_remove`；finance → 403。
- `TestAdminInstanceAndDashboardIncludeApps`：`GET /instances/{id}` 的 `apps` 数组；`GET /dashboard` 的 `apps_installing / apps_running / apps_failed_24h`（failed 行 `finished_at = now − 25h` 不计）。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/adminapi/ -run 'TestRouteTable|TestAdminApp|TestAdminVersion|TestAdminDeployments|TestAdminInstanceAndDashboard' 2>&1 | head -8`
Expected: 权限矩阵测试失败（缺路由）+ 编译错误。

- [ ] **Step 3: 实现**

`apps.go`：`appRoutes(g)` 用 `s.handle(g, …)` 注册上表；请求体结构 `appRequest{slug, name, summary, description, category, icon, sort, status}` → `appdeploy.AppInput`；`versionRequest{version, image_ids, spec, script, changelog, copy_from}`；`validate=1` 分支调 `AppCatalog.ValidateVersion`；`publish`：

```go
	var out appdeploy.Version
	var machines int
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		out, machines, err = s.AppCatalog.PublishVersion(r.Context(), s.actor(r), id, vid)
		return err
	}); err != nil { … }
	httpx.WriteJSON(w, 200, map[string]any{"version": out, "prepull_machines": machines})
```

`deployments.go`：`handleListDeployments`（`paging`、`ListFilter`）→ `httpx.Page(views, total)`；`handleDeployment`；`handleForceRemove`。`instances.go` `handleInstance` 加 `"apps": adminViews`（`s.Apps.AdminViews(ctx, deps)`）。`dashboard.go` 加三个数字（`s.Apps.Counts(ctx, now)`）；`server.go` 的 `Router` 在 `catalogRoutes` 之后调 `s.appRoutes(g)`。

- [ ] **Step 4: 运行测试并提交**

Run: `go vet ./... && go test ./internal/adminapi/ 2>&1 | tail -5`
Expected: 全部通过（含权限矩阵）。

```bash
git add internal/adminapi internal/appdeploy
git commit -m "feat(adminapi): app catalog / versions (validate, publish + TOTP, prepull, coverage), deployments list / detail / force-remove, instance + dashboard apps

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 14: `app.go` 组装（`appdeploy.Service` / `Catalog` / Runner、钩子接线）、配置项、`cmd/platform` 启动 Runner、README

**Files:**
- Modify: `internal/app/app.go`（`App.Apps / AppCatalog / AppRunner`、`Options.AppHealthAddr func(int) string`、`Build` 组装）、`internal/platform/config/config.go`（`AppHFEndpoint / AppPipIndexURL / AppRunner` + 解析）、`cmd/platform/main.go`（`if cfg.AppRunner { go a.AppRunner.Run(ctx) }`，与 `RunSweeper` 同处）、`README.md`（配置表三行 + 「部署注意」一段 + 「节点 Agent」注明 `StateReport.images` 与发布顺序）
- Test: `internal/platform/config/config_test.go`（追加三项解析：默认值、`SPARK_APP_RUNNER=off`、非法值报错）、`internal/app/apps_build_test.go`（`Build` 后 `a.Apps != nil`、`a.AppRunner != nil`，`rental` 钩子已接：`Place` 带不存在的 `app_id` 返回 `app_not_found` 而不是 noop 的同一错误——两者相同，改为断言带 published app 时实例有 queued 部署，用 `appdeploy.Catalog` 直接造应用）

**Interfaces:**
- `config.Config` 新字段：`AppHFEndpoint string`（`SPARK_APP_HF_ENDPOINT`，默认 `https://hf-mirror.com`，须以 `http://` 或 `https://` 开头）、`AppPipIndexURL string`（`SPARK_APP_PIP_INDEX_URL`，默认 `https://pypi.tuna.tsinghua.edu.cn/simple`）、`AppRunner bool`（`SPARK_APP_RUNNER` ∈ `on | off`，默认 `on`，其它值报错）；放在 `FillAccessDefaults` 一类的默认填充里（`FillAppDefaults`，`Build` 调用）。
- `app.Options.AppHealthAddr func(port int) string`（e2e-server / 集成测试注入，生产 nil）。
- `App` 新字段 `Apps *appdeploy.Service; AppCatalog *appdeploy.Catalog; AppRunner *appdeploy.Runner`。
- 组装顺序：`r := rental.NewService(...)` → `cat := appdeploy.NewCatalog(pool, clk, f, c, log)` → `upstreamPool := upstream.New(...)`（现有位置，需要前移到 `ingressAuth` 之后、`srv` 之前）→ `apps := appdeploy.NewService(appdeploy.Deps{Pool, Clock: clk, Box: box, Log: log, Catalog: cat, Images: c, Rental: r, Fleet: f, Upstream: upstreamPool, PortURL: ingressAuth.PortURL, HFEndpoint: cfg.AppHFEndpoint, PipIndexURL: cfg.AppPipIndexURL})` → `r.SetAppHooks(apps)` → `runner := appdeploy.NewRunner(apps, upstreamPool, appdeploy.RunnerConfig{HealthAddr: opt.AppHealthAddr})` → `srv.Apps = apps; srv.AppsLimit = authorizeLimit`（`DisableRateLimits` 时 `noRateLimit`）→ `adminSrv.Apps = apps; adminSrv.AppCatalog = cat`。
- `cmd/platform/main.go`：在启动 `RunSweeper` 的 goroutine 旁：`if cfg.AppRunner { go a.AppRunner.Run(ctx) } else { log.Warn("SPARK_APP_RUNNER=off：应用部署不会推进") }`。

- [ ] **Step 1: 写失败的测试**（上面两处）

- [ ] **Step 2: 实现并跑全量**

README「配置（环境变量）」表追加：

```markdown
| `SPARK_APP_HF_ENDPOINT` | 注入应用容器的 `HF_ENDPOINT`（模型下载的 HuggingFace 镜像站） | https://hf-mirror.com |
| `SPARK_APP_PIP_INDEX_URL` | 注入应用容器的 `PIP_INDEX_URL`（应用脚本若需装小包） | https://pypi.tuna.tsinghua.edu.cn/simple |
| `SPARK_APP_RUNNER` | on / off：是否在本副本启动应用部署 Runner（多副本都 on，咨询锁保证只有一个在工作；e2e 用 off） | on |
```

「部署注意」追加一段「应用一键部署（计划 7）」：发布顺序先 Agent（`StateReport.images`）后 platform；Harbor 的 `proxy-nvcr` / `spark` 项目与 `SPARK_AGENT_ALLOW_CIDRS`；三个应用镜像构建与登记（`make app-images`、后台「价格与镜像」登记后创建应用、`platform admin app import deploy/apps`、后台发布）；回滚 00020 的后果；`run.sh` 在用户容器里以 root 跑、脚本发布权限等同发布镜像。

Run: `export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH && go vet ./... && go test ./... 2>&1 | tail -15`
Expected: 全部通过。

```bash
git add internal/app internal/platform/config cmd/platform README.md
git commit -m "feat(app): assemble appdeploy service / catalog / runner, SPARK_APP_* config, README

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 15: 四个应用的 `deploy/apps/<slug>/{app.json, spec.json, app.sh}`，校验进 `go test`

**Files:**
- Create: `deploy/apps/vllm/{app.json,spec.json,app.sh}`、`deploy/apps/open-webui/{…}`、`deploy/apps/comfyui/{…}`、`deploy/apps/jupyterlab/{…}`、`deploy/apps/README.md`、`internal/appdeploy/apps_dir_test.go`

**Interfaces:**
- Consumes: Task 3 `ValidateSpec`、Task 7 `CheckShellSyntax`、`run.sh` 约定（`spark_status`、`service.pid`、保留变量）、Task 2 镜像里的路径（`vllm` 命令、`/opt/open-webui/bin/open-webui`、`/opt/ComfyUI/main.py`、`jupyter lab`、`modelscope download`）。
- Produces: 规格 §9 的四个应用（镜像名、端口、变量、模型大小表、`min_data_free_gib`：vllm 40 / comfyui 50 / 其余 2；超时：vllm 90 / comfyui 90 / open-webui 15 / jupyterlab 10）。`app.json.images` 用镜像**名称**：`"LLM Serving"`（llm-serving）、`"ComfyUI"`（comfyui）、`"PyTorch"`（pytorch）——Task 22 在后台登记镜像时用这三个名字。
- `apps_dir_test.go`：遍历 `../../deploy/apps/*`，每个目录三份文件齐全、`app.json` 解析、`spec.json` 过 `ValidateSpec`、`app.sh` 过 `CheckShellSyntax`、脚本 ≤ 64KB、每个 `app.sh` 的 `start` 分支包含 `service.pid`、`env` 里引用的 `$VAR` 都在 spec 声明或是保留变量（正则 `\$\{?([A-Z][A-Z0-9_]*)` 扫描，白名单 `SPARK_*`、`HF_*`、`MODELSCOPE_CACHE`、`PIP_INDEX_URL`、`PATH`、`HOME`、`!`、`$`、`?`）。

- [ ] **Step 1: 写失败的测试**（`apps_dir_test.go`，先只有目录不存在的失败）

- [ ] **Step 2: 写四个应用**

`deploy/apps/vllm/app.json`：

```json
{"name": "vLLM 推理服务", "summary": "OpenAI 兼容的大模型推理接口（Qwen3 等）", "category": "llm", "icon": "llm", "sort": 10, "version": "1.0.0",
 "images": ["LLM Serving"], "changelog": "初始版本",
 "description": "在实例上启动 vLLM，提供 OpenAI 兼容接口（/v1/chat/completions）。模型从 ModelScope 下载并缓存在 /root/data/models，卸载应用不会删除模型。\n\n- 接口密钥留空时接口不鉴权：端口默认私有、经控制台票据保护；设为公开前请先填写密钥。\n- 16GB 模型在默认限速下约需 15 分钟下载。"}
```

`deploy/apps/vllm/spec.json`：

```json
{
  "ports": [{"port": 8000, "label": "OpenAI 兼容接口", "path": "/v1", "health_path": "/health"}],
  "timeout_minutes": 90,
  "min_data_free_gib": 40,
  "env": [
    {"name": "MODEL_ID", "label": "模型", "type": "enum", "required": true,
     "options": ["Qwen/Qwen3-8B", "Qwen/Qwen3-4B", "Qwen/Qwen2.5-7B-Instruct", "Qwen/Qwen3-14B"], "default": "Qwen/Qwen3-8B", "help": "ModelScope 模型 ID"},
    {"name": "MODEL_SOURCE", "label": "下载源", "type": "enum", "options": ["modelscope", "hf-mirror"], "default": "modelscope"},
    {"name": "MAX_MODEL_LEN", "label": "最大上下文", "type": "int", "min": 1024, "max": 131072, "default": "32768"},
    {"name": "GPU_MEMORY_UTILIZATION", "label": "显存占用比例", "type": "string", "pattern": "^0\\.[3-9][0-9]?$", "default": "0.7"},
    {"name": "API_KEY", "label": "接口密钥", "type": "secret", "required": false, "pattern": "^[A-Za-z0-9_-]{8,128}$", "help": "留空则接口不鉴权（仅建议私有端口）"}
  ],
  "links": [{"label": "接口文档", "path": "/docs"}, {"label": "模型列表", "path": "/v1/models"}]
}
```

`deploy/apps/vllm/app.sh`：

```sh
#!/bin/sh
# vLLM 推理服务：install = 下载模型到 $SPARK_MODELS_DIR；start = vllm serve 放后台并写 service.pid。
set -eu

# 模型大小（GB）供进度估算；spec 的 options 不带大小
model_size_gb() {
  case "$1" in
    Qwen/Qwen3-4B) echo 8 ;;
    Qwen/Qwen3-8B) echo 16 ;;
    Qwen/Qwen2.5-7B-Instruct) echo 15 ;;
    Qwen/Qwen3-14B) echo 29 ;;
    *) echo 16 ;;
  esac
}

model_dir() {
  case "${MODEL_SOURCE:-modelscope}" in
    hf-mirror) echo "$SPARK_MODELS_DIR/hf/$MODEL_ID" ;;
    *) echo "$SPARK_MODELS_DIR/modelscope/$MODEL_ID" ;;
  esac
}

# 后台每 10 秒按 du 汇报下载百分比（5～75）
progress_loop() {
  _dir=$1; _total_kb=$(( $(model_size_gb "$MODEL_ID") * 1024 * 1024 ))
  while :; do
    sleep 10
    _kb=$(du -sk "$_dir" 2>/dev/null | awk '{print $1}')
    _pct=$(( 5 + ${_kb:-0} * 70 / _total_kb ))
    [ "$_pct" -gt 75 ] && _pct=75
    spark_status download "$_pct" "下载模型 $MODEL_ID：$(( ${_kb:-0} / 1024 / 1024 ))GB / $(model_size_gb "$MODEL_ID")GB"
  done
}

download() {
  _dir=$(model_dir)
  mkdir -p "$_dir"
  spark_status download 5 "开始下载 $MODEL_ID（约 $(model_size_gb "$MODEL_ID")GB）"
  progress_loop "$_dir" &
  _pl=$!
  case "${MODEL_SOURCE:-modelscope}" in
    hf-mirror)
      # HF_ENDPOINT 已由平台注入；huggingface-cli 随 vllm 的依赖安装
      huggingface-cli download "$MODEL_ID" --local-dir "$_dir" ;;
    *)
      modelscope download --model "$MODEL_ID" --local_dir "$_dir" ;;
  esac
  kill "$_pl" 2>/dev/null || true
  spark_status install 78 "模型就绪"
}

case "${1:-}" in
  install)
    download
    ;;
  start)
    _dir=$(model_dir)
    set -- vllm serve "$_dir" --host 127.0.0.1 --port "$SPARK_PORT" --served-model-name "$MODEL_ID" \
      --max-model-len "${MAX_MODEL_LEN:-32768}" --gpu-memory-utilization "${GPU_MEMORY_UTILIZATION:-0.7}"
    if [ -n "${API_KEY:-}" ]; then set -- "$@" --api-key "$API_KEY"; fi
    "$@" </dev/null >>"$SPARK_APP_DIR/vllm.log" 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    echo "vllm serve started, pid $(cat "$SPARK_APP_DIR/service.pid"); log: $SPARK_APP_DIR/vllm.log"
    # 把 vLLM 自己的日志尾部也并进部署日志，方便控制台排障（失败时能看到报错）
    ( tail -n 200 -f "$SPARK_APP_DIR/vllm.log" 2>/dev/null & echo $! > "$SPARK_APP_DIR/tail.pid" )
    ;;
  stop)
    [ -f "$SPARK_APP_DIR/tail.pid" ] && kill "$(cat "$SPARK_APP_DIR/tail.pid")" 2>/dev/null || true
    ;;
esac
```

（`tail -f` 跟 `service` 在同一进程组，`run.sh stop` 的 `kill -- -pgid` 会一起清掉；`--api-key` 会出现在 `ps` 与日志里——日志回写前经平台脱敏，规格 §16.4 第 12 项验证。）

`deploy/apps/open-webui/`：`app.json`（`"images": ["LLM Serving"]`，category `llm`，icon `chat`，sort 20）；`spec.json` 端口 `3000`（`health_path /health`）、超时 15、`min_data_free_gib 2`、变量 `OPENAI_API_BASE_URL`（string，pattern `^https?://[^\s]{1,200}$`，默认 `http://127.0.0.1:8000/v1`）、`OPENAI_API_KEY`（secret，可空）、`WEBUI_SECRET_KEY`（secret，可空，help「留空则首次启动自动生成」）；`app.sh`：`install` 为空（`spark_status install 50 "无需下载"`）；`start`：`DATA_DIR=$SPARK_APP_DIR/data`，`WEBUI_SECRET_KEY` 为空时从 `$SPARK_APP_DIR/.secret` 读、没有则 `head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'` 生成并写入（0600）；`export DATA_DIR WEBUI_SECRET_KEY OPENAI_API_BASE_URL OPENAI_API_KEY WEBUI_AUTH=True`；`/opt/open-webui/bin/open-webui serve --host 127.0.0.1 --port "$SPARK_PORT" </dev/null >>"$SPARK_APP_DIR/webui.log" 2>&1 & echo $! > service.pid`。描述提醒「首个注册用户成为管理员，部署完成后先自己打开一次」。

`deploy/apps/comfyui/`：`app.json`（`"images": ["ComfyUI"]`，category `image`，icon `image`，sort 30）；`spec.json` 端口 `8188`（`health_path /system_stats`）、超时 90、`min_data_free_gib 50`、变量 `MODEL_ID`（enum：`AI-ModelScope/stable-diffusion-xl-base-1.0` 默认、`AI-ModelScope/FLUX.1-schnell`、`Qwen/Qwen-Image`）、`MODEL_SOURCE`；`app.sh`：大小表 SDXL 7 / FLUX 24 / Qwen-Image 41；`install` 下载后按模型软链：SDXL → `ln -sfn <dir>/sd_xl_base_1.0.safetensors /opt/ComfyUI/models/checkpoints/`；FLUX → `unet / clip / vae` 子目录下的文件分别软链到 `/opt/ComfyUI/models/{unet,clip,vae}/`；Qwen-Image 同样按其 `transformer / text_encoder / vae` 目录软链（文件名按下载结果 `find -name '*.safetensors'`，脚本用 `for f in "$dir"/unet/*.safetensors; do ln -sfn "$f" /opt/ComfyUI/models/unet/; done` 风格）；`start`：`cd /opt/ComfyUI && python3 main.py --listen 127.0.0.1 --port "$SPARK_PORT" --output-directory /root/data/comfyui-output </dev/null >>"$SPARK_APP_DIR/comfyui.log" 2>&1 & echo $! > service.pid`。

`deploy/apps/jupyterlab/`：`app.json`（`"images": ["LLM Serving", "ComfyUI", "PyTorch"]`，category `notebook`，icon `notebook`，sort 40）；`spec.json` 端口 `8888`（`health_path /api`）、超时 10、`min_data_free_gib 2`、变量 `JUPYTER_TOKEN`（secret，`required: true`，pattern `^[A-Za-z0-9]{16,64}$`，help「点「生成」得到随机令牌，打开页面时需要输入」）；`app.sh`：`install` 空；`start`：`jupyter lab --ip 127.0.0.1 --port "$SPARK_PORT" --no-browser --allow-root --notebook-dir /root/data --IdentityProvider.token="$JUPYTER_TOKEN" --ServerApp.allow_remote_access=True --ServerApp.base_url=/ </dev/null >>"$SPARK_APP_DIR/jupyter.log" 2>&1 & echo $! > service.pid`。

`deploy/apps/README.md`：目录格式（`app.json` 字段表）、`platform admin app import deploy/apps` 用法、镜像名称约定、「脚本会在用户容器里以 root 执行」提醒。

- [ ] **Step 3: 运行测试并提交**

Run: `go test ./internal/appdeploy/ -run TestAppsDir -v 2>&1 | tail -8`
Expected: 四个目录全部通过（`sh -n`、spec、变量引用）。

```bash
git add deploy/apps internal/appdeploy/apps_dir_test.go
git commit -m "feat(apps): vllm / open-webui / comfyui / jupyterlab app definitions (spec + app.sh), validated in go test

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 16: Go 集成测试（全链路、失败链路、换机、并发、多副本、一致性检查）+ e2e 钩子

**Files:**
- Create: `internal/app/apps_e2e_test.go`、`internal/e2ehooks/apps.go`
- Modify: `internal/e2ehooks/hooks.go`（路由 `POST /hooks/apps/seed`、`POST /hooks/apps/tick`；`/hooks/agent` 加 `images`）、`cmd/e2e-server/main.go`（`sshtest.Options{Shell: true}`、`app.Options.AppHealthAddr`、health httptest、`AppRoot`）、`internal/app/app.go`（`Options.AppRoot string` 透传到 `RunnerConfig.AppRoot` 与 `Deps.AppRoot`）
- Test: 本任务就是测试。

**Interfaces:**
- `app.Options` 新增 `AppRoot string`（测试 / e2e 用；空 = `/root/data`）。
- e2e 钩子：`POST /hooks/apps/seed {}` → `{apps: [{id, slug, version_id, port}]}`：用 `AppCatalog` 创建并发布（`PublishVersion` 用系统 actor，绕过 TOTP——钩子直接调服务）四个应用的 **fake 版本**（spec 同 `deploy/apps` 的端口 / 变量；脚本 = `install`：`spark_status download 50 "downloading $MODEL_ID"; sleep 1`，`start`：`sleep 100000 </dev/null >/dev/null 2>&1 & echo $! > "$SPARK_APP_DIR/service.pid"`；`comfyui` 的 `image_ids` 指向钩子另建的镜像「ComfyUI」，其余指向 `dbtest` 默认镜像 + 「LLM Serving」镜像（钩子创建两个 enabled 镜像：`LLM Serving` = 默认镜像改名、`ComfyUI` = 新建 ref `spark/comfyui:e2e`），`jupyterlab` 兼容三者）；对每个 app 端口 `Upstream.SetTarget(port, healthAddr)`；幂等（已存在同 slug 直接返回）。`POST /hooks/apps/tick {n?}` → 调 `App.AppRunner.Tick` n 次（每次之间 `Clock.Advance(PollInterval)` 并真实 `time.Sleep(200ms)` 让脚本推进）→ `{rounds, processed}`。`/hooks/agent` body 加 `images?: []string`：先 `ag.SetImages(images)` 再 `Step`。
- 一致性检查 `assertConsistent(t, pool)`（规格 §14）：`removed / lost` 的部署没有 `instance_ports.deployment_id` 指向它；非终态部署的实例非终态；每实例非终态部署 ≤ 3；`running` 部署的主端口在 `instance_ports` 里。每个集成测试 `t.Cleanup` 执行。

- [ ] **Step 1: e2e 钩子与 e2e-server 改动**

`cmd/e2e-server/main.go`：`sshtest.New(sshtest.Options{AuthorizedKey, HostKey, Shell: true})`；`health := httptest.NewServer(http.HandlerFunc(func(w, r) { w.Header().Set("Content-Type", "text/plain"); fmt.Fprintf(w, "spark-app-ok|path=%s", r.URL.Path) }))`；`app.Options{…, AppHealthAddr: func(int) string { return strings.TrimPrefix(health.URL, "http://") }, AppRoot: filepath.Join(upstream.Root(), "root", "data")}`。`e2ehooks.Server` 加 `HealthAddr string`。

`internal/e2ehooks/apps.go`：`seed` / `tick` 处理器（按接口描述）；fake 脚本常量 `fakeAppScript`。

- [ ] **Step 2: 集成测试**

`internal/app/apps_e2e_test.go`（基于 `booking_e2e_test.go` 的 `bookingEnv` 风格，新建 `appsEnv`：`app.Build` 带 `Options{Clock, SMS, HostKeyGen: 假上游主机密钥, AppHealthAddr, AppRoot}`，`sshtest` shell 模式、`fakeagent`（步进）、ingress 经 `a.Handler`）：
- `TestAppsFullLifecycle`：seed（与钩子相同的 fake 版本，共用 `e2ehooks.SeedApps(ctx, a, upstream, healthAddr)` 导出函数）→ `POST /orders {…, app_id: vllm, app_env: {API_KEY: "abcdefgh12"}}` → 实例 `creating` + 部署 `queued`（`apps_summary`）→ fake Agent `Step` → `running` → `tick` ×N → `installing`（断言 `phase download`、`progress 50`、`log_tail` 含 `downloading`）→ `running` → `instance_ports` 出现 8000 私有 + `deployment_id` → 经 ingress 用登录 Cookie 请求 `ins-<id>-8000.spark.test/docs` → 200 且 body 含 `spark-app-ok|path=/docs` → `POST …/stop` → tick → `stopped`，端口仍在、`url` 仍返回 → `start` → tick → `running` → 到期停机（`clk.Advance(25h)` + `Sweep` + Agent Step）→ tick 不 exec（`upstream.ConnCount()` 不变）、`health unknown` → 续费 + Agent Step（Start 成功）→ 部署自动 `queued` → tick → `running`，`log_tail` 含 `skip install` → `release` → 部署 `removed(instance_gone)`、`assertConsistent`。
- `TestAppsFailureAndRedeploy`：seed 一个 `install` 退出 3 的版本 → `failed(script_failed)`、`log_tail` 含脚本输出 → `redeploy` 到好版本 → `running`；超时：版本 `timeout_minutes 1` + `install` `sleep 30` → `clk.Advance(61s)` → tick → `failed(timeout)` 且容器内 `status.json.state == failed`；不可达：`upstream.Close()`（新建的 sshtest 拒绝）→ 10 轮 → `failed(unreachable)` + `alerts` 恰一条 `app_deploy_unreachable`。
- `TestAppsMigrate`：running 部署 → 后台 `migrate`（`AdminMigrate`）→ 旧 `lost`、新实例 `queued` → Agent Step（新实例 Create）→ tick → `running`，`instance_ports` 对新实例只有一行 8000 且 `deployment_id` = 新部署。
- `TestAppsConcurrentOnOneInstance`：三个应用同时 queued → 一轮 tick 全部 `installing`（`Runner.Stats().PeakConcurrent >= 2`）→ 都 `running`，端口 8000 / 3000 / 8888 互不冲突；第四个 `POST` → 409 `app_limit`。
- `TestAppsTwoRunnersOneWorks`：`r2 := appdeploy.NewRunner(a.Apps, a.Upstream, cfg)`；并发 `Tick` 20 轮，每轮恰一个 `locked`；用一条连接长持 `pg_advisory_lock(hashtext('appdeploy'))` 时两者都 `locked=false`，释放后恢复。
- 每个测试 `t.Cleanup(func() { assertConsistent(t, pool) })`。

- [ ] **Step 3: 运行并提交**

Run: `go vet ./... && go test -race ./internal/app/ -run 'TestApps' -v 2>&1 | tail -20 && go test ./... 2>&1 | tail -5 && go run ./cmd/e2e-server & sleep 15; curl -s -X POST 127.0.0.1:18090/hooks/apps/seed -d '{}' ; kill %1`
Expected: 五个集成测试通过；全量通过；e2e-server 的 seed 钩子返回四个应用。

```bash
git add internal/app internal/e2ehooks cmd/e2e-server
git commit -m "test(appdeploy): integration tests (lifecycle, failure / timeout / unreachable, migrate, concurrency, two runners) + e2e hooks (apps seed / tick, agent images)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 17: 控制台：`logic/apps.ts`、`AppEnvForm.vue`、`RentDialog` 预装应用、市场页应用卡片

**Files:**
- Create: `web/console/src/logic/apps.ts`、`web/console/src/logic/__tests__/apps.spec.ts`、`web/console/src/components/AppEnvForm.vue`、`web/console/src/components/AppPicker.vue`
- Modify: `web/console/src/components/RentDialog.vue`、`web/console/src/views/MarketView.vue`、`web/shared/src/idempotency.ts`（若派生参数在此；否则 `RentDialog` 内的 `useIdempotencyKey` 调用处）

**Interfaces:**
- `logic/apps.ts` 导出（纯函数，vitest 覆盖）：
  - `compatibleImages(app: AppCatalogItem, images: Image[]): Image[]`（按 `app.images` 的顺序筛选）。
  - `envDefaults(spec: AppEnvSpec[]): Record<string, string>`（`default` 填充；`bool` 无默认 → `'false'`）。
  - `validateEnvValue(e: AppEnvSpec, v: string): string | null`（前端预校验：required 空、int 区间、pattern（用 `new RegExp` 包 try）、secret ≥ 8、max_len；返回中文原因）。
  - `generateSecret(e: AppEnvSpec): string`（32 位 `[A-Za-z0-9]`，`crypto.getRandomValues`；若 `pattern` 存在且不匹配 32 位字母数字则返回 `''`，按钮隐藏）。
  - `canGenerate(e: AppEnvSpec): boolean`。
  - `stripEmpty(values: Record<string, string>): Record<string, string>`（空串去掉，提交用）。
  - `deploymentStatusText(d: Deployment, instanceState: InstanceState): { text: string; type: string }`（Task 18 也用：`installing` → `部署中 45%` + phase 中文；实例非 running 时 `实例已停机，续费后自动恢复`；failed → `deploymentErrorLabel`）。
- `AppEnvForm.vue`：`props: { spec: AppEnvSpec[]; modelValue: Record<string, string>; secretSet?: Record<string, boolean> }`（`secretSet` 用于重新部署：已设置的 secret 显示占位「已设置，留空沿用」）；按类型渲染：`enum → el-select`、`int → el-input-number`、`bool → el-switch`、`secret → el-input type=password show-password + 「生成」按钮（`canGenerate`）`、`string → el-input`；`help` 为 `el-form-item` 的提示；`required` 星号；暴露 `validate(): boolean`（逐项 `validateEnvValue`，错误显示在该项下）。`data-testid="app-env-<NAME>"`（输入）、`app-env-<NAME>-gen`（生成按钮）、`app-env-<NAME>-error`。
- `AppPicker.vue`：`props: { apps: AppCatalogItem[]; modelValue: number | null; disabled?: (app) => string | null }`（返回灰显原因）；小卡片列表（图标、名称、summary），`data-testid="app-pick-<slug>"`，灰显时 `aria-disabled` + 原因 tooltip `data-testid="app-pick-<slug>-reason"`。
- `RentDialog.vue`：props 加 `apps: AppCatalogItem[]`、`appId: number | null`（市场页卡片预选）；表单新增「预装应用」行（`el-select data-testid="rent-app"`：`不预装` + 应用列表 `名称 · 默认模型`，默认模型 = `env` 里第一个 `enum` 的 `default`）；选了应用后：镜像下拉只剩 `compatibleImages`、默认第一个、提示「该应用需要此镜像」`data-testid="rent-image-locked"`；下方 `<AppEnvForm>`；确认页多一行「预装应用」`data-testid="market-confirm-app"`（`<名称>（<模型>）`）；提交 body 加 `app_id`、`app_env: stripEmpty(values)`；幂等键派生参数加 `appId`、`appEnv`（JSON 字符串）；`app_image_mismatch / app_env_*` 错误显示后端文案并 `backToForm()`。
- `MarketView.vue`：加载 `GET /apps`（失败不阻塞商品列表）；每个商品卡片下方「一键部署」区：四个 `AppPicker` 风格的小卡片 `data-testid="market-app-<slug>"`，点击 = `rent(p, appId)` 打开 `RentDialog`（`mode='immediate'`，`appId` 预选）。

- [ ] **Step 1: 写失败的 vitest**（`logic/__tests__/apps.spec.ts`：`compatibleImages` 顺序；`envDefaults`；`validateEnvValue` 矩阵（required / int 区间 / pattern / secret 短 / max_len）；`generateSecret` 长度 32 且匹配 `^[A-Za-z0-9]{32}$`、pattern 不允许时 `''`；`stripEmpty`；`deploymentStatusText` 四种）

Run: `cd web && npm test -- apps 2>&1 | tail -5` → 失败（模块不存在）。

- [ ] **Step 2: 实现四个文件与两处修改**

`RentDialog` 的 `watch(selectedApp)`：切换应用时 `form.imageId = compatible[0]?.id ?? null`、`envValues = envDefaults(spec.env)`；取消预装时镜像列表恢复全部、`form.imageId` 恢复 props 的 `imageId`。`toConfirm()` 前先 `envForm.value?.validate()`。

- [ ] **Step 3: 验证并提交**

Run: `cd web && npm test && npm run build`
Expected: vitest 全绿（含新 `apps.spec.ts`），构建通过；现有 Playwright `console/market.spec.ts`、`console/booking.spec.ts` 用例在 Task 21 一并跑（`RentDialog` 默认「不预装」，`/orders` 请求体无 `app_id`）。

```bash
git add web/console web/shared
git commit -m "feat(console): app env form, app picker, preinstall app in RentDialog, market one-click deploy cards

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 18: 控制台：实例详情「应用」卡片（进度、日志抽屉、操作）、列表摘要、帮助页

**Files:**
- Create: `web/console/src/components/AppsCard.vue`、`web/console/src/components/AppLogDrawer.vue`、`web/console/src/components/AppDeployDialog.vue`
- Modify: `web/console/src/views/InstanceDetailView.vue`、`web/console/src/views/InstancesView.vue`、`web/console/src/views/HelpView.vue`、`web/console/src/logic/instance.ts`（刷新条件：有 `installing / queued` 部署的实例每 10 秒刷新）

**Interfaces:**
- `AppsCard.vue`（`data-testid="detail-apps-card"`，放在「Web 访问」卡片之前）：props `instance: Instance`、`apps: AppCatalogItem[]`；内部加载 `GET /instances/{id}/apps`；每个部署一行 `data-testid="detail-app-row" :data-dep="d.id" :data-state="d.state"`：图标 + 名称 `detail-app-name` + 版本 `detail-app-version`（`update_available` 时标签「有新版本 vX」`detail-app-update`）、状态标签 `detail-app-state`（`deploymentStatusText`）、进度条 `detail-app-progress`（installing，`:percentage="d.progress"`，文案 phase 中文 + `message` `detail-app-message`）、「打开」`detail-app-open`（running 且 `url`；`links` 为 `el-dropdown` 项 `detail-app-link`）、操作下拉 `detail-app-actions`：重新部署 `detail-app-redeploy`、停止 `detail-app-stop` / 启动 `detail-app-start`、查看日志 `detail-app-log`、卸载 `detail-app-remove`（按 §6.2 的状态表显示）；failed 行内红字 `detail-app-error`（错误码中文 + message）；实例非 running 时每行提示 `detail-app-instance-hint`（「实例已停机，续费后自动恢复」/ reserved / creating 时「将在开通后自动部署」）；端口被用户删了（running 且 `url == null`）→ `detail-app-port-deleted`「端口已删除，重新部署可恢复」；「添加应用」按钮 `detail-app-add`（≤ 3 个非终态时可用，否则提示 `detail-app-limit`「每个实例最多部署 3 个应用」）→ `AppDeployDialog`（模式 `add`：`AppPicker` 灰显不兼容（原因「需要镜像：<名称列表>」）与已部署（「已部署」）；选中后 `AppEnvForm` → `POST /instances/{id}/apps`）。
- `AppDeployDialog.vue`（`data-testid="app-deploy-dialog"`）：模式 `add | redeploy`；redeploy 时版本选择 `app-deploy-version`（当前 / 最新 published），`AppEnvForm` 预填 `env`、`secretSet`；确认 `app-deploy-ok`；错误 `app-deploy-error`。
- `AppLogDrawer.vue`（`data-testid="app-log-drawer"`）：`<pre data-testid="app-log-text">`，「刷新」`app-log-refresh` 调 `GET …/log`，显示 `live` 标记 `app-log-live`（「实时」/「数据库缓存」），自动滚到底。
- 卸载确认 `AppDialog kind="confirm" data-testid="app-remove-dialog"` 文案「卸载后应用目录会删除，模型缓存保留在 /root/data/models」；实例停机时附「实例停机中，应用文件会在实例下次启动时清理」。
- 「动态」卡片：合并 `GET …/events`（每个部署）与实例事件，按时间排序，部署事件 `detail-event` 的 `data-kind="app"`、文本 `<应用名>：<detail>`。
- `InstancesView.vue`：卡片 / 表格的时间文案下一行 `apps_summary` 标签 `data-testid="instance-app" :data-dep="a.id" :data-state="a.state"`（部署中 45% 蓝 / 运行中 绿可点（`a.url` 新窗口）/ 失败 红 / 已停止 灰）；`logic/instance.ts` 的刷新条件加「有 `queued / installing` 的部署」。
- `HelpView.vue`：新增「一键部署」一节（规格 §11.4 的十条，`data-testid="help-apps"`）。

- [ ] **Step 1: 实现**（无新纯逻辑则不加 vitest；`logic/instance.ts` 的刷新判断补一个 vitest 用例）

- [ ] **Step 2: 验证并提交**

Run: `cd web && npm test && npm run build`

```bash
git add web/console
git commit -m "feat(console): instance apps card (progress, actions, log drawer, add / redeploy dialog), list app summary, help section

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 19: 后台：应用列表 / 详情 / 版本编辑页（结构化 spec 表单、脚本框、校验、发布动态码、覆盖率、复制）

**Files:**
- Create: `web/admin/src/views/AppsView.vue`、`web/admin/src/views/AppDetailView.vue`、`web/admin/src/views/AppVersionView.vue`、`web/admin/src/logic/appSpec.ts`、`web/admin/src/logic/__tests__/appSpec.spec.ts`
- Modify: `web/admin/src/menu.ts`（`{ key: 'apps', path: '/apps', label: '应用', roles: ALL_ROLES }` 放在「价格与镜像」之前）、`web/admin/src/router.ts`（`/apps`、`/apps/:id`、`/apps/:id/versions/:vid`，`roles: ALL_ROLES`，写按钮按角色显示）、`web/admin/src/__tests__/menu.spec.ts`（表驱动加一行）、`web/shared/src/types/admin.ts`（`AdminApp`、`AdminAppVersion`、`AppCoverage`、`AdminDeployment`、`Dashboard` 三个字段、`Machine.cached_images / cached_images_at`）

**Interfaces:**
- `logic/appSpec.ts`：`emptySpec(): AppSpec`；`specToForm / formToSpec`（表格行 ↔ `AppSpec`，`int` 的 `min/max` 与 `enum` 的 `options`（textarea 一行一个）转换）；`fieldPathToRow(path: string): { table: 'ports'|'env'|'links'; index: number; field: string } | null`（把后端 `app_version_invalid` 的 `env[2].default` 定位到表格行高亮）；vitest 覆盖往返与路径解析。
- `AppsView.vue`（`/apps`，`data-testid="apps-table"`）：列 名称 / slug / 分类 / 状态 / 版本数 / 活跃部署 / 排序；`apps-create`（super）→ 弹窗 `apps-dialog`（slug 只在新建可填 `apps-slug`）；行点击进入详情。
- `AppDetailView.vue`（`/apps/:id`）：应用信息 + 编辑（super）+ 删除（draft 无版本）；版本表 `app-versions-table`：版本 / 状态 / 兼容镜像 / 覆盖率 `app-version-coverage`（`已缓存 X / 在线 Y（未知 Z）`，`cached < online` 时 warning 色）/ 发布时间 / 活跃部署；「新建版本」`app-version-create`（可选「从版本复制」`app-version-copy-from`）→ 进入编辑页。
- `AppVersionView.vue`（`/apps/:id/versions/:vid` 与 `/apps/:id/versions/new?copy_from=`）：左侧表单（版本号 `app-version-number`、兼容镜像多选 `app-version-images`、changelog）、spec 结构化表单（端口表 `app-spec-ports`、超时 `app-spec-timeout`、磁盘 `app-spec-disk`、变量表 `app-spec-env`（名 / 标签 / 类型 / 必填 / 默认 / 选项 / 正则 / 范围 / 帮助，行 `app-spec-env-row`）、链接表 `app-spec-links`）、「查看 JSON」`app-spec-json`（只读 `<pre>`）；右侧脚本 `<textarea data-testid="app-version-script">`（等宽、行号用左侧 `<pre>` 行号列）+「校验」`app-version-validate`（`?validate=1`，结果 `app-version-validate-result`，错误高亮对应行 / 字段 `app-version-field-error`）；「保存」`app-version-save`；「发布」`app-version-publish`（super；弹窗 `app-version-publish-dialog` 显示校验结果与覆盖率警告 `app-version-publish-coverage` + `TotpDialog`）；「重新预拉取」`app-version-prepull`（ops）；published 只读（changelog 与「退役」`app-version-retire` 除外）。

- [ ] **Step 1: vitest（`appSpec.spec.ts`）→ 实现 → 验证**

Run: `cd web && npm test && npm run build`

```bash
git add web/admin web/shared
git commit -m "feat(admin): app catalog pages (apps, versions with structured spec form, validate, publish with TOTP, coverage, copy)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 20: 后台：部署列表 / 详情 / 强制卸载、实例详情应用卡片、机器详情已缓存镜像、仪表盘

**Files:**
- Create: `web/admin/src/views/DeploymentsView.vue`
- Modify: `web/admin/src/menu.ts`（`{ key: 'deployments', path: '/deployments', label: '应用部署', roles: ALL_ROLES }` 紧跟「应用」）、`web/admin/src/router.ts`、`web/admin/src/__tests__/menu.spec.ts`、`web/admin/src/views/InstanceDetailView.vue`、`web/admin/src/views/MachineDetailView.vue`、`web/admin/src/views/DashboardView.vue`

**Interfaces:**
- `DeploymentsView.vue`（`/deployments`）：筛选 状态 `deployments-filter-state` / 应用 `deployments-filter-app` / 用户 ID / 实例 ID；表格 `deployments-table`（实例、用户、应用 / 版本、状态、进度、错误码、时间）；详情抽屉 `deployment-drawer`（状态、进度、错误、变量 `deployment-env`（secret 显示 `***`）、事件 `deployment-events`、日志 `<pre data-testid="deployment-log">`、「强制卸载」`deployment-force-remove`（ops）→ 原因弹窗 `deployment-force-remove-dialog`（原因必填 `deployment-force-remove-reason`））。
- `InstanceDetailView.vue`（后台）：新增「应用」卡片 `admin-instance-apps`（只读行 `admin-instance-app` + 强制卸载按钮）。
- `MachineDetailView.vue`：「已缓存镜像」卡片 `machine-cached-images`（列表 + `cached_images_at` 时间，空时「未上报（Agent 版本过旧或尚未上报）」）。
- `DashboardView.vue`：三个数字 `dash-apps-installing / dash-apps-running / dash-apps-failed`。

- [ ] **Step 1: 实现并验证**

Run: `cd web && npm test && npm run build`

```bash
git add web/admin web/shared
git commit -m "feat(admin): deployments list / detail / force-remove, instance apps card, machine cached images, dashboard app counters

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 21: Playwright 用例（控制台市场 / 应用 / 入口，后台应用 / 部署 / 实例 / 机器）、钩子、覆盖表

**Files:**
- Create: `web/e2e/helpers/apps.ts`、`web/e2e/console/apps.spec.ts`、`web/e2e/admin/apps.spec.ts`
- Modify: `web/e2e/helpers/hooks.ts`（`hooks.appsSeed()`、`hooks.appsTick(n)`、`hooks.agent({images})`）、`web/e2e/console/market.spec.ts`（C-MKT-09～12）、`web/e2e/console/ingress.spec.ts`（C-ING-07）、`web/e2e/admin/instances.spec.ts`（A-INS-14）、`web/e2e/admin/machines.spec.ts`（A-MCH-10）、`web/e2e/coverage.json`（25 个新 ID）、`web/playwright.config.ts`（注释：apps 用例的时钟推进与机器使用说明）

**Interfaces:**
- `helpers/apps.ts`：`seedApps()`（调钩子，返回 `{vllm, 'open-webui', comfyui, jupyterlab}` 的 `{id, version_id, port}`）；`tickUntil(api, instanceID, depID, state, maxRounds = 20)`（循环 `hooks.appsTick(1)` + `GET /instances/{id}/apps/{dep}` 直到状态）；`deployViaApi(api, instanceID, appId, env)`；`appRow(page, depId)`。
- 用例（标题以 ID 开头）：
  - `market.spec.ts`：**C-MKT-09** 市场页「一键部署」卡片 `market-app-vllm` 打开对话框且 `rent-app` 预选 vLLM；**C-MKT-10** 选应用后镜像锁定（`rent-image-locked` 可见、下拉只剩兼容镜像）、`app-env-API_KEY-gen` 生成 32 位、必填校验（清空 `MODEL_ID` → `app-env-MODEL_ID-error`）；**C-MKT-11** 确认页 `market-confirm-app` 显示「vLLM 推理服务（Qwen/Qwen3-8B）」；**C-MKT-12** 下单后实例详情出现 queued 应用（`detail-app-row[data-state=queued]`），`hooks.agent()` 开通 → `appsTick` → `data-state=installing` 且 `detail-app-progress` 可见 → 再 tick → `running`，`detail-app-open` 的 href 含 `ins-<id>-8000`。
  - `console/apps.spec.ts`：**C-APP-01** 添加应用弹窗：comfyui 灰显 `app-pick-comfyui-reason` 含「需要镜像」；**C-APP-02** 添加 open-webui → tick → running，两行；**C-APP-03** 第三个 jupyterlab 后 `detail-app-add` 禁用且 `detail-app-limit` 可见（API 第四个 409 `app_limit`）；**C-APP-04** 失败：后台（API）发布一个 install 失败的版本后部署 → `detail-app-error` 含「脚本执行失败」，`detail-app-log` 打开抽屉 `app-log-text` 含 `boom`；**C-APP-05** 重新部署：改 `MODEL_ID`、secret 留空 → 请求体 `env` 无 `API_KEY` → running 且 `secret_env.API_KEY` 仍 true（API 断言）；**C-APP-06** 停止 / 启动；**C-APP-07** 卸载确认文案含「模型缓存保留」→ tick → 行消失、「Web 访问」里 8000 端口消失；**C-APP-08** 实例停机（`advance(25h)` + sweep + agent）后每行 `detail-app-instance-hint` 含「实例已停机」；**C-APP-09** 后台发布新版本后 `detail-app-update` 出现；**C-APP-10** 列表页 `instance-app[data-state=running]` 标签可点。
  - `ingress.spec.ts`：**C-ING-07** 应用端口私有：未登录访问 `ins-<id>-8000.spark.test` 302 到登录；登录后拿到 `spark-app-ok|path=/docs`。
  - `admin/apps.spec.ts`：**A-APP-01** 应用新建 / 编辑；删除条件（有版本的「删除」禁用）；**A-APP-02** 版本新建用结构化表单，端口填 22 → 校验显示 `ports[0].port` 并高亮行；**A-APP-03** 脚本 `if true; then` → `app-version-validate-result` 含 `line`；**A-APP-04** 发布需动态码；覆盖率 `0 / 1（未知 1）` 警告 → `hooks.agent({images: [ref]})` 后刷新为 `1 / 1`；**A-APP-05** published 版本只读、改 changelog 可保存、退役；**A-APP-06** 复制为新版本；**A-APP-07** 部署列表筛选 + 详情 secret `***`；**A-APP-08** 强制卸载需原因、审计页出现 `app.deployment.force_remove`。
  - `admin/instances.spec.ts`：**A-INS-14** 实例详情应用卡片；`admin/machines.spec.ts`：**A-MCH-10** 机器详情已缓存镜像（`hooks.agent({machine_id, images})`）。
- 注意：apps 用例在各自实例（`runningInstance`）上操作；只有 C-APP-08 推进时钟（25 小时，与 C-INS-01 同量级；写进 `playwright.config.ts` 注释）；`seedApps` 幂等可在每个用例 `beforeAll` 调用。

- [ ] **Step 1: 写用例与钩子 → 登记覆盖表 → 跑**

Run: `cd web && npm run e2e:coverage && npx playwright test e2e/console/apps.spec.ts e2e/console/market.spec.ts e2e/console/ingress.spec.ts e2e/admin/apps.spec.ts e2e/admin/instances.spec.ts e2e/admin/machines.spec.ts --project=console-desktop --project=admin 2>&1 | tail -20`
Expected: 覆盖表核对通过（25 个新 ID 各恰好一次）；新用例全绿。再跑 `make e2e` 全量（三个 project）确认原有用例不受影响。

```bash
git add web/e2e web/playwright.config.ts
git commit -m "test(e2e): one-click deploy coverage (C-MKT-09~12, C-APP-01~10, C-ING-07, A-APP-01~08, A-INS-14, A-MCH-10)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 22: 真机验收 A（192.168.1.221）——规格 §16.4 第 1～8、11～13 项：镜像登记、目录导入、vLLM / Open WebUI / JupyterLab、失败与日志、停机恢复、容器不可达、安全、对账

**Files:**
- Create: `docs/superpowers/acceptance-plan7.md`（本任务写「验收 A」；Task 23 补齐）
- 可能修改：验收中发现的缺陷在对应包修复并补单测（单独提交，记录里写提交号）

**Interfaces:**
- Consumes: 全部任务；Task 2 已在 221 上构建好三个镜像（`docker images spark/*` 可见）；Task 11 的新 Agent（`make agent` 的 `bin/spark-agent-linux-arm64` 与 SHA-256）。
- 前提与分工：平台跑在开发 Mac（`MAC=$(ipconfig getifaddr en0)`，验收时 192.168.1.218），Agent 跑在 Spark。**先升级 Agent 再起 platform**（规格 §15）。需要 root 的命令由协调人按「执行须知」经 ssh 执行（远端 `read -r P; s() {...}`），密码只在运行时的环境变量里。DNS 对 `*.sslip.io` 返回假地址：curl 加 `--resolve`，Chrome 用 `--host-resolver-rules`。验收时机器限速临时设为 1000Mbps（`SPARK_AGENT_BANDWIDTH_MBPS=1000`，对新建实例生效）。只有一台机器：第 3～8 步共用一台实例，第 11 步后释放。

- [ ] **Step 0: 准备**

```bash
export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH
cd /Users/rifuyiri/Documents/claude/机房搭建/spark-cloud-p7
MAC=$(ipconfig getifaddr en0); DOM=$(echo $MAC | tr . -).sslip.io; echo $MAC $DOM
make agent   # 记下 SHA-256
# 1) 升级 Agent（需要 root：协调人执行）：把 bin/spark-agent-linux-arm64 scp 到 221 的 /tmp，然后
#    s install -m 755 /tmp/spark-agent-linux-arm64 /usr/local/bin/spark-agent && s systemctl restart spark-agent && s systemctl status spark-agent --no-pager | head -5
scp bin/spark-agent-linux-arm64 imac@192.168.1.221:/tmp/
# 2) 平台（新终端）
make devdb &
SPARK_DATABASE_URL=postgres://postgres:postgres@localhost:5433/spark?sslmode=disable \
SPARK_SECRET_KEY=MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE= \
SPARK_DOMAIN=$DOM SPARK_PUBLIC_BASE_URL=http://$DOM:8080 SPARK_SSH_ADDR=:2222 SPARK_SSH_PUBLIC_PORT=2222 \
SPARK_AGENT_ADDR=:9443 SPARK_AGENT_HOSTS=$MAC SPARK_UPSTREAM_SSH_KEY_FILE=$PWD/.devdata/upstream.key SPARK_AGENT_DATA_SIZE_GIB=196 \
SPARK_AGENT_BANDWIDTH_MBPS=1000 SPARK_SWEEP_INTERVAL=1m SPARK_APP_PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple go run ./cmd/platform
# 3) 快捷方式（另一终端）
PSQL=".devdata/runtime/bin/psql postgres://postgres:postgres@localhost:5433/spark"
API="curl -s --resolve $DOM:8080:$MAC -b .devdata/cj -c .devdata/cj -H 'Content-Type: application/json'"
ADM="curl -s -b .devdata/aj -c .devdata/aj -H 'Content-Type: application/json' http://127.0.0.1:8081/admin/api/v1"
```

后台机器 1 应 `connected=true` 且 `agent_version` 为新版本（`$ADM/machines/1 | jq .machine.agent_version`）；不在线时按计划 3 验收记录重新执行接入命令（需要 root）。用户登录（`/auth/sms-code` → `/auth/login` 填 `000000`，60 秒内不要重复发码）、实名、充值（mock）照 `internal/app/booking_e2e_test.go` 的顺序用 curl 做一遍；后台管理员用已有 super 账号登录 `http://127.0.0.1:8081/admin/`（TOTP 密钥在 Keychain 之外由负责人提供）。

- [ ] **Step 1: 镜像登记与缓存上报（§16.4 第 1 项后半）**

后台「价格与镜像」登记三个镜像（名称必须与 `deploy/apps/*/app.json` 的 `images` 一致）：`POST /images {"name":"LLM Serving","ref":"spark/llm-serving:25.09","enabled":true,"sort":10}`、`{"name":"ComfyUI","ref":"spark/comfyui:25.09",...}`、`{"name":"PyTorch","ref":"spark/pytorch:25.09",...}`（原 `vLLM` / `PyTorch` 占位条目改 ref 或停用，记录做法）。platform 日志出现 `pull_image broadcast`；机器上 `pull_image` 对本地 tag 会失败（Docker Hub 不可达）——只影响指令记录，后台机器详情指令列表显示失败，记录注明。2 分钟内 `$ADM/machines/1 | jq .machine.cached_images` 列出三个 `spark/*:25.09`（Agent 60 秒一次 StateReport），后台机器详情「已缓存镜像」可见。

- [ ] **Step 2: 目录导入与发布（§16.4 第 2 项）**

```bash
SPARK_DATABASE_URL=... SPARK_SECRET_KEY=... go run ./cmd/platform admin app import deploy/apps
```
Expected: 四行 `<slug> 1.0.0 created`。后台 `/apps` 四个 draft；逐个进入版本页「发布」（动态码）→ published，`prepull_machines 1`，覆盖率 `已缓存 1 / 在线 1`；`GET $DOM:8080/api/v1/apps` 返回四个应用。再把四个应用的 `status` 改为 `published`（`PUT /apps/{id}`）。

- [ ] **Step 3: 预装 vLLM（§16.4 第 3 项）**

浏览器（`open -na "Google Chrome" --args --host-resolver-rules="MAP *.$DOM $MAC, MAP $DOM $MAC" http://$DOM:8080/market`）：市场页「一键部署」点「vLLM 推理服务」→ 对话框预选、镜像锁定为 `LLM Serving`、模型选 `Qwen/Qwen3-4B`、`API_KEY` 点「生成」（记下）→ 确认页「预装应用：vLLM 推理服务（Qwen/Qwen3-4B）」→ 下单。实例 creating → running（约 1 分钟）；详情「应用」卡片 1 分钟内「部署中」，进度随下载推进（phase 下载模型 → 启动 → 健康检查），完成 ≤ 20 分钟（记录实际耗时）。「打开」`/docs` → 登录跳转 → Swagger 页。接口：

```bash
TK=$($API -X POST http://$DOM:8080/api/v1/instances/<id>/ports/8000/... )   # 不需要：直接用控制台 Cookie 走 ingress authorize
curl -s --resolve ins-<id>-8000.$DOM:8080:$MAC -b .devdata/ingress-cj -H "Authorization: Bearer <key>" -H 'Content-Type: application/json' \
  http://ins-<id>-8000.$DOM:8080/v1/chat/completions -d '{"model":"Qwen/Qwen3-4B","messages":[{"role":"user","content":"你好"}],"max_tokens":32}'
```
（`ingress-cj` 由浏览器登录跳转后的 `sc_ingress` Cookie 导出；或先把端口「设为公开」再 curl，记录做法。）返回补全；`ssh imac@192.168.1.221 'nvidia-smi | grep -i vllm; docker exec <容器> ls /root/data/models/modelscope/Qwen/Qwen3-4B | head'` 有进程与文件。

- [ ] **Step 4: 同实例加 Open WebUI（§16.4 第 4 项）**

实例详情「添加应用」→ Open WebUI（`OPENAI_API_BASE_URL` 默认、`OPENAI_API_KEY` 填上一步密钥）→ running ≤ 5 分钟 → 打开链接注册管理员 → 选择模型 `Qwen/Qwen3-4B` 对话成功（WebSocket 经 ingress；浏览器 Network 面板看到 `101`）。

- [ ] **Step 5: 加 JupyterLab（§16.4 第 5 项）**

添加 JupyterLab（「生成」令牌）→ running → 打开链接输入令牌 → 文件面板看到 `/root/data`；再点「添加应用」→ 按钮禁用 + `detail-app-limit`；`$API -X POST …/instances/<id>/apps -d '{"app_id":<comfyui>}'` → 409 `app_limit`（ComfyUI 本来也不兼容，先验证 limit：用 jupyterlab 的 id 再建 → `app_exists`，用一个临时 published 的第四个应用验证 `app_limit`，或直接看前端禁用 + API 对 vllm 重复 → `app_exists`；记录实际采用的方式）。

- [ ] **Step 6: 重新部署换模型（§16.4 第 6 项）**

vLLM「重新部署」→ `MODEL_ID = Qwen/Qwen3-8B`（约 16GB）→ 旧服务停止、下载新模型（进度）、running（记录耗时）；Open WebUI 模型列表变为 `Qwen/Qwen3-8B`；`ls /root/data/models/modelscope/Qwen/` 两个模型都在。

- [ ] **Step 7: 失败与日志、有新版本（§16.4 第 7 项）**

实例详情「添加应用」ComfyUI 灰显并提示「需要镜像：ComfyUI」。后台 vLLM「复制为新版本」1.0.1：`MAX_MODEL_LEN` 的 `max` 改为 262144，发布（动态码）→ 控制台 vLLM 行出现「有新版本 1.0.1」；重新部署选 1.0.1、`MAX_MODEL_LEN = 262144`、`GPU_MEMORY_UTILIZATION = 0.3` → `failed`（错误码「服务进程已退出」或「健康检查未通过」，记录实际），日志抽屉里能看到 vLLM 的报错（`ValueError` / `out of memory` 一类）；再重新部署改回 32768 / 0.7 → running。

- [ ] **Step 8: 停止 / 启动 / 停机恢复（§16.4 第 8 项）**

停止 vLLM → 端口链接仍在、`curl …/v1/models` → 502（ingress 的「端口未监听」页）；启动 → 恢复。`$PSQL -c "UPDATE instances SET expires_at = now() + interval '1 minute' WHERE id='<id>'"` → 2 分钟内停机 → 三个应用显示「实例已停机，续费后自动恢复」；续费（`POST …/renew {"duration":"1d"}`）→ 实例 running 后 3 分钟内三个应用自动回到运行中（记录各自耗时），日志里 `skip install`，`nvidia-smi` 看到 vLLM 进程。

- [ ] **Step 9: Agent 停机 / 容器不可达（§16.4 第 11 项）**

协调人：`s systemctl stop spark-agent`（不停容器）→ 应用健康轮询照常（后台部署详情 `last_polled_at` 持续更新，`health ok`）；`docker stop <容器>`（`imac` 在 docker 组，不需要 root）→ 约 5 分钟（10 轮 × 30 秒）后三个部署 `failed(unreachable)`，后台告警恰一条 `app_deploy_unreachable`；`docker start <容器>` → `s systemctl start spark-agent` → 三个应用「重新部署」→ running（`skip install`）。记录 Agent 停机期间平台日志没有 `agent` 相关报错以外的异常。

- [ ] **Step 10: 安全（§16.4 第 12 项）**

后台给 vLLM 再复制一个 draft 1.0.2，变量表加 `NOTE`（string、无 pattern），发布；重新部署 vLLM 到 1.0.2，`NOTE` 填 `'; touch /tmp/pwned; '` → running；`docker exec <容器> cat /root/data/.spark-apps/vllm/env | grep NOTE` 输出 `export NOTE=''\''; touch /tmp/pwned; '\'''`；`docker exec <容器> ls /tmp/pwned` → 不存在。后台部署详情里 `API_KEY` 显示 `***`，日志尾部里 `--api-key` 后的值是 `***`（vLLM 启动日志会打印参数）。协调人 `s iptables -S SPARK-FWD; s iptables -S SPARK-IN` 与计划 2/3 验收记录一致。

- [ ] **Step 11: 对账无噪声（§16.4 第 13 项）**

`$ADM/alerts?resolved=false`：只有第 9 步的一条 `app_deploy_unreachable`（处理掉）；`$PSQL -c "SELECT count(*) FROM ssh_sessions WHERE instance_id='<id>'"` 等于浏览器 / 手工 SSH 的次数（平台 exec 不产生会话行）。释放实例（为 Task 23 腾出唯一机器）→ 部署全部 `removed`，`instance_ports` 行随实例失效。

- [ ] **Step 12: 记录与提交**

把每项的命令、关键输出（部署耗时、模型下载速度、错误码）与结论写进 `docs/superpowers/acceptance-plan7.md`「验收 A」（格式同 `acceptance-plan6.md`：机器 / 平台 / 规格 / 说明 + 每项「——通过 / 未通过」+ 缺陷表）；发现的缺陷修复后单独提交并写提交号。写入前核对记录与提交里没有任何密码、`SUDO_PW`、TOTP 密钥或 API_KEY 的值。

```bash
git add docs/superpowers/acceptance-plan7.md
git commit -m "docs: plan 7 real-machine acceptance A (vLLM / Open WebUI / JupyterLab, failure, recovery, unreachable, security) on 192.168.1.221

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 23: 真机验收 B（ComfyUI，§16.4 第 9、10 项）+ 验收记录 + 遗留清单 + `make check` 全量

**Files:**
- Modify: `docs/superpowers/acceptance-plan7.md`（补「验收 B」与总结论）、`README.md`（如仍有遗漏）
- Create: `docs/superpowers/followups-plan7.md`

**Interfaces:**
- Consumes: Task 22 的环境（平台仍在跑、实例已释放、镜像 `spark/comfyui:25.09` 在机器上）。

- [ ] **Step 1: ComfyUI（§16.4 第 9 项）**

市场页「一键部署」点「ComfyUI 图像生成」→ 镜像锁定 `ComfyUI`、模型 SDXL base → 下单 → running ≤ 15 分钟（记录）→ 打开链接 → 加载默认工作流（checkpoint 下拉里有 `sd_xl_base_1.0.safetensors`）→ 512×512 生成一张 → `docker exec <容器> ls /root/data/comfyui-output` 有 PNG；`nvidia-smi` 看到 python 进程。

- [ ] **Step 2: 卸载与再部署（§16.4 第 10 项）**

卸载 ComfyUI（确认框文案）→ `docker exec <容器> ls /root/data/.spark-apps/` 没有 `comfyui`、「Web 访问」8188 消失、`/root/data/models/modelscope/AI-ModelScope/stable-diffusion-xl-base-1.0` 仍在；再添加 ComfyUI → `modelscope download` 对已存在文件秒过 → ≤ 3 分钟 running（记录）。释放实例。

- [ ] **Step 3: `followups-plan7.md`**

```markdown
# 计划 7 遗留事项

来源：计划 7 设计（2026-10-20）§17、实施与验收过程。均不阻塞合并。

## 上线前需要核对
- Harbor：compose 里加 goharbor 服务，建 `proxy-nvcr`（代理 nvcr.io）与 `spark` 项目；机器端 `SPARK_AGENT_ALLOW_CIDRS` 含 Harbor 地址；在 arm64 构建机上 `make app-images` 并 `docker push` 到 Harbor；后台「价格与镜像」把三个镜像的 ref 改为 Harbor 路径（替换占位条目）。
- 发布顺序：先全部 Agent（`StateReport.images`）后 platform；platform 先发只影响覆盖率显示「未知」。
- NGC 标签：验收用 `<实际标签>`；上线前复核是否有更新的 arm64 + CUDA 13 版本，vLLM 的 GB10 支持以 `torch.cuda.get_device_name()` 为准。
- 默认限速 200Mbps 下 16GB 模型约 12～15 分钟，给运营做预期管理（帮助页已注明）。
- Open WebUI「首访即管理员」的提醒文案；ModelScope 当前所选模型不需要登录令牌，新增模型时核对。
- 计划 8 合并前把它的迁移改为 00021（本计划占用 00020）。

## 规格列出的遗留（本计划不做）
- 应用收费、按模型收费、Token 商城。
- 部署期间临时放开限速（需要 Agent 指令）；机器级共享模型缓存。
- 自动升级已有部署、灰度发布、版本回滚按钮（用「复制为新版本」）。
- 用户自定义应用 / 上传脚本；应用市场评分与运营数据。
- 多实例编排；应用之间的依赖声明（Open WebUI 依赖 vLLM 只靠默认地址）。
- 日志全量持久化与搜索（只保留 64KB 尾部 + 容器内文件）；Web 终端。
- 后台脚本内容审查 / shellcheck；脚本签名。
- 健康异常自动重启（只标 unhealthy / crashed）；短信通知。
- ComfyUI-Manager / 自定义节点、CivitAI、HuggingFace 热门模型搜索。
- reserved 实例开通前修改预装变量（只能卸载再添加）。
- 镜像构建流水线（CI）；Harbor 镜像 GC。

## 可以继续延后
- 新 Agent 本机没有任何镜像时 `StateReport.images` 为空列表，在线上与「未上报」不可区分，后台显示「未知」；机器刚装好的窗口极短。
- `stopped` 的部署不在 Runner 的候选查询里（`next_poll_at` 推到 24 小时后），实例恢复 running 时由钩子重排；规格 §5.3 表里的 `stopped` 行等价。
- Runner 的咨询锁按轮取放（与清扫器一致），不是会话长持；多副本交替工作，效果相同。
- 本机没有 `setsid` 的开发 Mac 用 `python3 -c os.setsid` 脱离会话；镜像规范要求 `util-linux`，生产只走 `setsid`。
- 部署包上传用 `tar -x -f -`，依赖镜像里的 `tar`（规范已要求）；没有 `tar` 的镜像上传失败会显示 `upload_failed`。
- 后台版本编辑页的脚本框是 `<textarea>`，没有语法高亮。
- 覆盖率的「未知」= 5 分钟内没上报；Agent 60 秒一报，正常机器不会落入。
- `apps_summary` 让实例列表每次多一次查询（一次 `ANY($1)`，不是 N+1）。

## 实施中确定的产品行为（裁定）
- 应用端口由平台登记（不经 `rental.AddPort`），卸载时以用户身份审计 `instance.port_delete`。
- 卸载时实例停机：立即 `removed` + `cleanup_pending`，实例恢复后由 Runner 删目录。
- 重新部署换版本时旧端口行先删，新版本 running 时重新登记。
- `platform admin app import` 的镜像按 `images.name` 匹配；只导入为 draft，发布在后台做。
（实施中新增的裁定按实际补充。）
```

- [ ] **Step 4: 全量验证**

Run:
```bash
export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH
go vet ./... && go test -race ./... && make check
```
Expected: `go test -race` 全部通过；`make check` = `test`（Go + `web-test` + `install_test.sh`）+ `e2e`（`make web` → `npm run e2e:coverage` → Playwright 三个 project 全部通过，原有用例 + 本计划 25 个新用例）并打印覆盖表；失败项逐个修到绿，不允许 skip。

- [ ] **Step 5: 记录、结论、提交**

补齐 `acceptance-plan7.md`：「验收 B」每项命令 / 关键输出 / 结论，缺陷表（编号、现象、原因、修复提交），「观察（列入遗留）」，总结论「全部通过」。写入前核对记录与提交里没有任何密码、`SUDO_PW`、TOTP 密钥或 API_KEY 的值。

```bash
git add docs/superpowers/acceptance-plan7.md docs/superpowers/followups-plan7.md README.md
git commit -m "docs: plan 7 acceptance B (ComfyUI, uninstall / reinstall), followups, final make check

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## 上线前需要核对

见 Task 23 的 `followups-plan7.md`「上线前需要核对」：Harbor 搭建与镜像推送 / 登记、先 Agent 后 platform 的发布顺序、NGC 标签复核、默认限速下的下载时间预期、Open WebUI 首访提醒、ModelScope 令牌、**计划 8 的迁移改为 00021**。

## 规格列出的遗留（本计划不做）

规格 §17「明确不做」全文已抄进 `followups-plan7.md`（Task 23）。

## 可以继续延后

见 `followups-plan7.md`「可以继续延后」。

## 实施中确定的产品行为（裁定）

见本文「已做出的裁定」与 `followups-plan7.md`「实施中确定的产品行为」；实施中新增的裁定按实际补充到后者。

## 自检记录（写计划时完成）

**规格覆盖**（§ → 任务）：§0 代码事实（非 privileged 容器、`Pool` 只有 `Dial`、`sshtest` 只回显、`images.ref` 允许本地 tag、`BroadcastPullImage`、端口上限 5、迁移编号 00020）→ Global Constraints、Task 1、5、6、7；§1.1 第 1 条（容器内普通进程 + 上游 SSH exec）→ Task 4、6、9；第 2 条（镜像承载环境、兼容镜像锁定）→ Task 2、7、8、17；第 3 条（两种入口）→ Task 10（预装）、8 / 12 / 18（运行中实例）；第 4 条（`setsid` 后台 + 轮询）→ Task 4、9；第 5 条 / D12（只加 `StateReport.images`）→ Task 11；第 6 条 / D11（Harbor、预拉取、覆盖率）→ Task 7（coverage）、13、19、23（上线核对）；第 7 条 / D1（免费）→ 无订单改动，Task 10 只插部署；第 8 条 / D14（spec + POSIX sh、变量走文件）→ Task 3、4、15；第 9 条 / D5（自动登记私有端口）→ Task 8（`ports.go`）、9（`registerPorts`）；第 10 条 / D10（失败不重试、不升级）→ Task 9（`markFailed`）、8（`Redeploy`）、12（`update_available`）；第 11 条（恢复 running 回 queued、`.installed` 跳过）→ Task 10、4；第 12 条 / D13（换机复制 queued、旧 lost）→ Task 10；第 13 条（咨询锁）→ Task 9；D2（3 个 / 实例）→ Task 8；D3（四个应用三个镜像）→ Task 2、15；D4（ModelScope / hf-mirror、缓存不删）→ Task 15、3（保留变量）；D6（超时 60 / 180）→ Task 3、9；D7（不放开限速）→ 帮助页 Task 18、遗留；D8（角色）→ Task 13；D9（secret 加密、脱敏）→ Task 8（`secrets.go`）、9、12（`secret_env`）、13；D15（列表摘要、不发短信）→ Task 12（`apps_summary`）、18；§2 → Task 1；§3.1～3.4 → Task 3、7、8；§4.1 → Task 2；§4.2 → Task 7、11、13；§5.1 → Task 3（`Bundle / UploadCommand`）；§5.2 → Task 4；§5.3 状态表每一行 → Task 9（`stepDeploy / stepPoll / stepHealth / stepStop / stepRemove / stepCleanup / unreachable`）；§5.4 → Task 10；§6.1～6.2 → Task 8；§6.3 → Task 9（跳过 / instance_gone）、10；§6.4 → Task 10、9（认领）；§7 → Task 8 / 9 / 12（`url`）；§8 → Task 3（变量名黑名单 + 转义）、4（`sh` 真执行测试）、9（HTTP 健康只看状态码、4KB）、13（发布 TOTP）、22（真机安全项）；§9 → Task 15、2；§10 错误码 → Task 1；§10.1 → Task 12；§10.2 → Task 13；§11.1 → Task 17；§11.2～11.4 → Task 18；§12 → Task 19、20；§13 → Task 1（常量）、14（配置）；§14 → Task 8（锁顺序注释）、9（乐观并发、幂等、并发上限）、16（一致性检查）；§15 → Task 1（Down）、11（兼容）、14（README）、17（`RentDialog` 默认不预装）；§16.1 单元 → Task 3、4、6、7、8、9、10、11、12、13；§16.2 集成 → Task 5（shell 模式）、16；§16.3 e2e → Task 16（钩子）、21；§16.4 真机 13 项 → Task 2（第 1 项前半）、22（1 后半～8、11～13）、23（9、10）；§17 → Task 23；§18 的 23 项 → 本计划 23 项（5 / 6 对调，见裁定）。

**占位符扫描**：全文无 TBD / TODO / 「类似 Task N」；Task 3、4、6、9、10、11 的风险代码给出完整代码块；Task 5 给出完整的新增函数与改动点；Task 7、8、12、13 给出精确签名、SQL 与测试清单；前端任务给出文件、props、`data-testid` 与逻辑函数签名；Task 2 / 22 / 23 的真机步骤带精确命令与预期输出；NGC 标签与 ComfyUI ref 的「以实施时核实为准」在 Task 2 Step 1 给出了核实命令与退路，不是空白。

**类型一致性**：`appdeploy.Spec / PortSpec / EnvSpec / LinkSpec`（Task 1）↔ `ValidateSpec / ValidateEnv / SpecHash / BundleInput / ReservedEnv`（Task 3）↔ `run.sh` 读取的 `SPARK_*` 变量（Task 4）↔ `Catalog.VersionInput`（Task 7）↔ `Service.Create / Redeploy`（Task 8）↔ `Runner.stepPoll` 的 `ver.Spec.TimeoutMinutes / Ports[0].HealthPath`（Task 9）↔ 前端 `AppSpec`（Task 1 `apps.ts`）↔ 后台结构化表单 `specToForm / formToSpec`（Task 19）；`Deployment`（Task 1）↔ `depCols / scanDeployment / lockDeployment`（Task 8）↔ `writeback / markFailed / markRemoved`（Task 9）↔ `OnInstanceRunning / OnInstanceGone / OnInstanceMigrated`（Task 10）↔ `View / Summary`（Task 12）↔ 前端 `Deployment / AppsSummaryItem`（Task 1）↔ `detail-app-row` 等 testid（Task 18、21）；`upstream.Pool.Exec(ctx, Target, cmd, stdin, maxOutput) (stdout, stderr []byte, exit int, err error)`（Task 6）↔ `Runner.exec`（Task 9）↔ `Service.Log`（Task 8）；`sshtest.Options{Shell, Root}`（Task 5）↔ `TestPoolExec`（Task 6）↔ `runner_test` / `apps_e2e_test` / `cmd/e2e-server`（Task 9、16）；`rental.AppHooks / InstallPlan / SetAppHooks / LockOwned`（Task 8、10）↔ `appdeploy.Service` 实现（Task 10）↔ `app.Build` 接线（Task 14）；`BatchRequest.AppID / AppEnv`、`PlaceRequest.AppID / AppEnv`（Task 10）↔ `handlePlaceOrder / bookingRequest`（Task 12）↔ `RentDialog` 提交体（Task 17）↔ e2e `deployViaApi`（Task 21）；`fleet.StateReport.Images / RecordCachedImages / Machine.CachedImages`（Task 11）↔ `Catalog.VersionCoverage`（Task 7）↔ 后台 `coverage` / `machine-cached-images`（Task 13、19、20）↔ `hooks.agent({images})`（Task 16、21）；`RunnerConfig{PollInterval, AppRoot, HealthAddr, MaxConcurrent}` / `Runner.Tick`（Task 9）↔ `app.Options.AppHealthAddr / AppRoot`（Task 14、16）↔ `/hooks/apps/tick`（Task 16）；`apperr.ErrApp* / AppImageMismatch / AppPortConflict / AppEnvInvalid / AppEnvRequired / AppVersionInvalid`（Task 1）↔ 各服务返回 ↔ 前端错误文案显示（Task 17、18、19）。

**Review Focus 落点**：1 → Task 9 `TestRunnerDiscardsStaleWriteback`（exec 期间卸载 → 回写丢弃，部署仍 removing）；2 → Task 3 `TestRenderEnvEscapes`（`sh` 真加载、marker 不存在）+ `TestValidateSpecMatrix`（`PATH / SPARK_X / LD_PRELOAD` 拒绝）+ Task 4 `TestRunShEnvIsNotEvaluated` + Task 22 Step 10；3 → Task 10 `TestOnInstanceRunningRequeues` / `TestAppHooksCalledOnLifecycle` + Task 4 `TestRunShRedeploySkipsInstall` + Task 16 `TestAppsFullLifecycle`（续费恢复段）+ Task 22 Step 8；4 → Task 10 `TestOnInstanceMigratedCopies` / `TestAppHooksMigrateOrder` + Task 9 `TestRunnerClaimsMigratedPort` + Task 16 `TestAppsMigrate`；5 → Task 11 `TestReportImagesRecorded` / `TestReportWithoutImagesKeepsCache`（proto3 未知字段兼容由 `make proto` 生成代码保证，旧平台不解析 `images`）。
