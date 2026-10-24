# 计划 7 真机验收记录（192.168.1.221）

## 验收 A：vLLM / Open WebUI / JupyterLab、失败与日志、停机恢复、容器不可达、安全、对账（§16.4 第 1～8、11～13 项）

- 日期：2026-10-20（05:12～11:12；05:50～10:18 开发 Mac 休眠 / 会话暂停，期间无操作，见观察 O-9）。
- 机器：DGX Spark `spark-3479`（GB10），与计划 2/3/6 验收是同一台机器。后台显示为「B 区」B-01（machine 1）。machine 2 是离线占位机，本次没有用到。
- Agent：从 `feat/plan7-deploy` HEAD `748dec2` 用 `make agent` 构建 linux/arm64。SHA-256 为 `4582d8b622a73a07789c34321e5509621faf8206d3597ac9c2aae83798d983d4`，与 Spark 上的 `/usr/local/bin/spark-agent` 和 `.devdata/dl` 里的文件一致。后台 `agent_version = 748dec2`，`connected = true`。
- 平台：开发 Mac 192.168.1.218 上的 `.devdata/platform-p7`（同一 HEAD），05:12 启动，验收期间没有重启。
  - 环境变量：`SPARK_ENV=dev`、`SPARK_DOMAIN=192-168-1-218.sslip.io`、`SPARK_SWEEP_INTERVAL=1m`、`SPARK_AGENT_BANDWIDTH_MBPS=1000`、`SPARK_AGENT_DATA_SIZE_GIB=200`、`SPARK_APP_PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple`。
  - `SPARK_AGENT_BINARY_SHA256` 为上面的新值。
  - 开发库在 :5433。
- 镜像：Task 2 在 221 上构建的 `spark/llm-serving:25.09`、`spark/comfyui:25.09`、`spark/pytorch:25.09`，以本地 tag 登记。基础镜像为 `nvcr.io/nvidia/vllm:25.09-py3` 和 `nvcr.io/nvidia/pytorch:25.09-py3`。
- 规格：`docs/superpowers/specs/2026-10-20-plan7-deploy-design.md` §16.4。清单见 `.superpowers/sdd/2026-10-20-plan7-deploy/task-22-brief.md`。
- 说明：
  - 本验收先由一个验收代理做到第 5 步，该代理因额度中断。第 1～5 步的结论依据它留下的截图、库里的事件和审计、容器内日志，并在接手后复核了关键项（接口补全、JupyterLab 令牌登录、上限、脱敏）。第 6 步以后由接手方完成。
  - 只有一台机器，第 3～11 步共用实例 `ins-tqzyl1us`（镜像 LLM Serving，1 天）。第 11 步后已释放。
  - 控制台测试用户为 13800000221。余额 ¥362.00 → ¥224.00（下单 69 + 续费 69），没有充值。
  - 页面操作用 Playwright 驱动真实 Chrome（`--host-resolver-rules` 把 `*.sslip.io` 指到 Mac）。后台登录走真实表单和动态码。
  - 截图在验收会话的 scratchpad 里：`…/scratchpad/t22/shots/`，下文简写为 `shots/`。
  - 需要 root 的命令（`systemctl stop/start spark-agent`、`iptables -S`）在 Spark 上用 sudo 执行。
  - 所有模型下载都发生在 Spark 上的实例容器里，Mac 上没有下载任何模型或大文件。
- 结论：11 项中 **8 项通过、3 项部分通过**（第 3、4、11 项）。发现 2 个高严重度缺陷：
  - A-1：应用进程拿不到镜像的环境变量，导致 vLLM 1.0.0 无法启动。
  - A-2：Open WebUI 默认对话报 400。

  另有 1 个中等（A-3，规格清单与设计冲突）和 1 个低（A-4）。验收期间按规避方式继续，详见缺陷表。

### 版本编号的调整（与清单不同）

- vLLM `1.0.1` 已被第 3 步用作 A-1 的规避版本（`app.sh` 开头补回 NGC 镜像的 `CUDA_HOME / PATH / TRITON_*`），由前一个代理建立并发布。
- 因此清单第 7 步的「1.0.1」顺延为 **1.0.2**（`MAX_MODEL_LEN.max = 262144`）。
- 清单第 10 步的「1.0.2」顺延为 **1.0.3**（新增 `NOTE` 变量）。
- 1.0.2 和 1.0.3 是验收专用版本，验收结束后已下线（`retired`）。vLLM 当前最新的已发布版本是 1.0.1。
- 1.0.0 仍处于发布状态，按原样部署会触发 A-1。Task 23 若要部署 vLLM，请选 1.0.1。

### 1. 镜像登记与缓存上报（§16.4.1 后半）——通过

- 05:13，后台「价格与镜像」做了以下改动（审计 `image.update/create`）：
  - 新建 `LLM Serving → spark/llm-serving:25.09`（id 5）和 `ComfyUI → spark/comfyui:25.09`（id 6）。
  - 原占位条目 `PyTorch`（id 2）的 ref 改为 `spark/pytorch:25.09`。
  - 原 `vLLM` 占位（id 3）改名为「vLLM（旧占位）」并停用。
- 平台日志有三条 `pull_image broadcast`（image 2 / 5 / 6，machines 1）。
- 机器上的 `pull_image` 指令全部失败，原因是 Docker Hub 不可达（`registry-1.docker.io … context deadline exceeded`），只影响指令记录。之后每次发布版本的预拉取也同样失败，共 11 条 failed 记录，见观察 O-1。
- `GET /machines/1` 的 `cached_images` 包含三个 `spark/*:25.09`（共 16 个）。后台机器详情「已缓存镜像（16）」可见，上报时间每分钟刷新。
- 截图：`shots/01-machine-cached.png`。

### 2. 目录导入与发布（§16.4.2）——通过

- 05:13:15，`platform admin app import deploy/apps` 导入四个应用（审计 4 组 `app.create` + `app.version.create`），均为 draft 1.0.0。
- 05:14～05:16，在后台版本页逐个点「发布」并输入动态码：
  - 发布对话框显示「校验通过」，没有覆盖率不足提示。
  - 发布后状态为「已发布」。
  - 覆盖率显示 `已缓存 1 / 在线 1（未知 0）`。
- 05:17，四个应用改为 `published`（`app.update`）。控制台市场页「一键部署」出现 4 张卡片。
- 截图：`shots/publish-{1..4}-*-dialog.png`、`publish-*-after.png`、`app-4-detail.png`、`03-market.png`。

### 3. 预装 vLLM（§16.4.3）——部分通过（缺陷 A-1，规避后通过）

- **下单**：05:18:06 在市场页点「vLLM 推理服务」。
  - 对话框里镜像锁定为 LLM Serving，模型选 `Qwen/Qwen3-4B`，`API_KEY` 点「生成」。
  - 确认页显示「预装应用：vLLM 推理服务（Qwen/Qwen3-4B）」，下单后跳转到实例详情。
  - 截图：`shots/03-confirm.png`、`03-detail-queued.png`。
- **开通与部署进度**：
  - 实例约 4 秒后 running。05:18:10 部署进入「部署中」，满足 1 分钟内的要求。
  - 进度从 5% 推进到 67%，期间 phase 为「下载模型」，随后进入「健康检查」90%。截图：`03-detail-installing.png`。
  - Qwen3-4B（7.6GB）的下载在 05:18:10～05:20:50 之间完成，约 2.6 分钟，折合约 50MB/s。
- **第一次失败（缺陷 A-1）**：05:22:00 进入 `failed`，错误码 `health_failed`，事件为「健康检查未通过：服务进程已退出」。
  - 日志抽屉里能看到 vLLM 的报错：Triton JIT 调用 gcc 编译 `cuda_utils.c` 时报 `fatal error: cuda.h: No such file or directory`，随后出现 `RuntimeError: Engine core initialization failed`。截图：`03-log-failed.png`、`03-detail-failed.png`。
  - 根因：平台经实例 sshd 执行 `run.sh`。SSH 会话不继承镜像的 Docker `ENV`，app 进程的 `/proc/<pid>/environ` 里只有 `PATH=/root/data/.spark-apps/<slug>:…:/usr/bin:/bin`，没有 `CUDA_HOME`、`LD_LIBRARY_PATH`、`TRITON_*`、`NVIDIA_*`。`docker inspect` 能看到这些变量。
- **规避**：
  - 05:24 后台发布 1.0.1（`app.sh` 显式 export NGC 镜像的 CUDA / Triton 变量）。
  - 控制台出现「有新版本」，截图 `03-update-available.png`。
  - 「重新部署」选 1.0.1。05:25:00 开始，05:27:05 running（第 2 次，约 2 分钟，主要是 vLLM 编译和 CUDA graph）。从下单到可用共约 9 分钟；没有 A-1 的话预计约 5 分钟，满足 ≤ 20 分钟的要求。
- **打开**：
  - 「链接 → 接口文档」打开 `/docs`，未登录时 302 到 `/api/v1/ingress/authorize`，登录跳转后显示 FastAPI Swagger。截图：`03-docs.png`。
  - 主按钮「打开」指向根路径，vLLM 返回 `{"detail":"Not Found"}`（缺陷 A-4）。截图：`03-open-docs.png`。
- **接口**（复核于 05:37，带 `sc_ingress` Cookie）：

  | 请求 | 结果 |
  |---|---|
  | 不带 Cookie | 302 到 authorize |
  | 带 Cookie、不带密钥 | 401 `{"error":"Unauthorized"}` |
  | 带 `Authorization: Bearer <API_KEY>` 调 `/v1/chat/completions`（`max_tokens 32`） | 200，`model Qwen/Qwen3-4B`，content「你好！有什么我可以帮你的吗？」，`completion_tokens 14` |

- **机器侧**：
  - `nvidia-smi` 有 `VLLM::EngineCore`，占用约 82.7GB（`GPU_MEMORY_UTILIZATION 0.7`）。
  - 容器里有 `/root/data/models/modelscope/Qwen/Qwen3-4B`（7.6G，3 个 safetensors 分片）。
  - `env` 文件权限为 `-rw-------`。

### 4. 同实例加 Open WebUI（§16.4.4）——部分通过（缺陷 A-2，规避后对话成功）

- **部署**：05:29:48 在实例详情点「添加应用」→ Open WebUI。接口地址用默认的 `http://127.0.0.1:8000/v1`，接口密钥填第 3 步的 `API_KEY`。05:30:40 running，用时 52 秒，满足 ≤ 5 分钟。
  - 添加对话框里 ComfyUI 灰显，并提示「需要镜像：ComfyUI」。截图：`shots/04-add-openwebui.png`。
- **注册与模型**：打开链接经登录跳转进入 Open WebUI，注册了首个管理员。`/api/models` 返回 `["Qwen/Qwen3-4B"]`。
- **对话失败（缺陷 A-2）**：发消息后页面显示 `"auto" tool choice requires --enable-auto-tool-choice and --tool-call-parser to be set`。
  - vLLM 日志对应一条 `POST /v1/chat/completions 400`。
  - Open WebUI 日志：`Error processing chat payload: "auto" tool choice requires …`。
  - 原因：镜像里 Open WebUI 没有锁版本（实装 0.12.3），默认 `function_calling = native` 并带内置工具，请求里带 `tool_choice=auto`；vLLM 启动时没有开 auto tool choice。
  - 截图：`shots/04-webui-chat.png`。第 6 步换 8B 后复现，截图 `06-webui.png`。
- **规避**：经 Open WebUI 自己的接口把该用户的 `ui.params.function_calling` 设为 `legacy`，再发「用一句话介绍 DGX Spark」。
  - Qwen3-8B 正常回复（「DGX Spark 是 NVIDIA 推出的一款高性能 AI 计算平台……」）。
  - 页面经 ingress 建立 `ws://ins-tqzyl1us-3000.<域>:8080/ws/socket.io/?EIO=4&transport=websocket`，Playwright 捕获到 websocket 事件，流式回复正常。
  - 截图：`shots/07-webui-legacy.png`。

### 5. 加 JupyterLab（第 3 个）与上限（§16.4.5）——通过

- **部署**：05:32:16 添加 JupyterLab，令牌点「生成」。05:32:30 running，用时 14 秒。
- **打开**：经登录跳转到 `…/login?next=/lab`，输入令牌后进入 `/lab`。
  - 文件面板根目录为 `/root/data`，显示 `lost+found` 和 `models`；`.spark-apps` 是隐藏目录。
  - `/api/contents` 返回 200。
  - 截图：`shots/05-jupyter.png`。
- **上限**：已有 3 个应用时，「添加应用」按钮**不显示**（不是置灰），下方显示 `detail-app-limit`「每个实例最多部署 3 个应用」。截图：`shots/05-limit.png`。
- **接口**（`POST /instances/ins-tqzyl1us/apps`）：

  | 请求 | 结果 |
  |---|---|
  | `{"app_id":4}`（vllm 重复） | 409 `app_limit` |
  | `{"app_id":2}`、`{"app_id":3}` | 409 `app_limit` |
  | `{"app_id":1}`（ComfyUI） | 409 `app_image_mismatch`「该应用需要以下镜像之一：ComfyUI，当前实例镜像为 LLM Serving」 |

  上限检查先于「已存在」检查，所以重复添加 vllm 返回的是 `app_limit` 而不是 `app_exists`（按清单允许的方式记录）。

### 6. 重新部署换模型（§16.4.6）——通过

- 05:39:53 在 vLLM「操作 → 重新部署」：版本 v1.0.1（当前），模型改为 `Qwen/Qwen3-8B`；接口密钥显示「已设置，留空沿用」，留空。截图：`shots/06-redeploy-8b.png`。
- 时间线：

  | 时间 | 状态 |
  |---|---|
  | 05:39:55 | installing（第 3 次，旧服务被停） |
  | 05:40:31 | 下载 10% |
  | 05:43:33 | 下载 70% |
  | 05:45:55 | running |

  合计约 6 分钟。下载 16GB 约 3.5 分钟，约 75MB/s，按 1000Mbps 限速约 600Mbps。
- Open WebUI 的 `/api/models` 变为 `["Qwen/Qwen3-8B"]`。
- 容器里 `du -sh /root/data/models/modelscope/Qwen/*` 显示 `Qwen3-4B 7.6G`、`Qwen3-8B 16G`。`nvidia-smi` 中新的 EngineCore 占用 84.7GB。

### 7. 失败与日志、有新版本（§16.4.7）——通过

- ComfyUI 在本实例上灰显，并提示「需要镜像：ComfyUI」（见第 4 项截图）。
- **新版本**：
  - 后台以 1.0.1 为底建 draft **1.0.2**（`MAX_MODEL_LEN.max` 131072 → 262144），走接口 `copy_from` 建立。
  - 在版本页「发布」并输入动态码：「校验通过」，覆盖率 `已缓存 1 / 在线 1`。
  - 控制台 vLLM 行立即出现「有新版本 v1.0.2」，接口 `update_available: true`。截图：`shots/07-update-available.png`。
- **让它失败**：重新部署选 1.0.2，`MAX_MODEL_LEN = 262144`、`GPU_MEMORY_UTILIZATION = 0.3`。
  - 10:20:21 提交，约 40 秒后 `failed`，错误码 `health_failed`，说明「服务进程已退出」。
  - 日志抽屉里有 `pydantic_core._pydantic_core.ValidationError … User-specified max_model_len (262144) is greater than the derived max_model_len (max_position_embeddings=40960 …)`。
  - 失败后 GPU 上没有残留进程。
  - 截图：`07-redeploy-bad.png`、`07-log-failed.png`、`07-detail-failed.png`。
- **改回**：重新部署，改回 32768 / 0.7。10:21:57 开始，10:23:38 running。

### 8. 停止 / 启动 / 停机恢复（§16.4.8）——通过

- **停止**（10:23:49）：
  - 状态变为 `stopped / desired stopped`。
  - 「Web 访问」里 8000 端口的链接还在（`instance_ports` 三行，`deployment_id` 对应各部署）。
  - 带 Cookie 请求 `/v1/models` 返回 **502**，页面标题「无法连接」；平台日志 `ingress upstream port not listening port 8000`。
  - GPU 上没有进程。截图：`shots/08-stopped.png`。
- **启动**（10:24:26）：10:27:05 running，带密钥请求 `/v1/models` 返回 200。
- **到期停机**：10:27:17 执行 `UPDATE instances SET expires_at = now() + interval '1 minute'`。
  - 10:28:17 到期，10:28:32 实例变为 `stopped`（已到期停机），容器 `Exited (0)`。
  - 三个应用的 `health` 变为 `unknown`，state 不变。每行显示「实例已停机，续费后自动恢复」，但状态标签仍是绿色「运行中」（观察 O-4）。
  - 截图：`shots/08-suspended.png`。
- **续费**：10:28:57 `POST /renew {"duration":"1d"}` 返回 200，10:29:03 实例 running。三个部署 10:28:58 回到 queued（「实例已启动，正在恢复应用」）。恢复耗时：

  | 应用 | 回到 running | 耗时 |
  |---|---|---|
  | JupyterLab | 10:29:10 | 约 13 秒 |
  | Open WebUI | 10:29:40 | 约 43 秒 |
  | vLLM | 10:31:05 | 约 2 分 8 秒 |

  都在 3 分钟内。
- 三个部署日志都有 `skip install: spec_hash … and env unchanged`，没有重新下载。`nvidia-smi` 有 `VLLM::EngineCore`。

### 9. Agent 停机 / 容器不可达（§16.4.11）——部分通过（缺陷 A-3：清单的组合场景到不了 unreachable）

**做法 1：按清单顺序**

- 10:38:28 执行 `systemctl stop spark-agent`，不停容器。
  - 10:38～10:40 部署照常轮询，`last_polled_at` 持续更新，`health ok`。轮询走上游 SSH，不经 Agent。
- 10:40:11 执行 `docker stop ins-tqzyl1us`。
  - 平台日志 `appdeploy: container unreachable … dial tcp 192.168.1.221:2222: connect: connection refused`，`unreachable_count` 每约 30 秒加 1。
  - 10:44:20 Agent 失联满 5 分钟，产生 `machine_offline` 告警（id 15），实例 `suspended_at` 被置位。按规格 §6.3「机器失联 → 跳过轮询」，Runner 不再轮询，计数停在 7～8，`health` 变为 `unknown`。
  - 一直到 10:50 都没有进入 `failed(unreachable)`，也没有 `app_deploy_unreachable` 告警。
- 10:50:41 执行 `systemctl start spark-agent`。
  - Agent 立刻把容器拉起，实例解除暂停。
  - 应用进程已经不在，`alive = 0` 连续 3 次后三个部署 `failed(crashed)`「服务进程已退出」。截图：`shots/09-crashed.png`。
  - 三个应用逐个「重新部署」后都 running，日志都是 `skip install`。
- 结论：清单要求的「Agent 停 + 容器停 → 10 轮后 unreachable + 一条告警」在设计上到不了：Agent 停满 5 分钟必然先触发暂停，见缺陷 A-3。

**做法 2：Agent 在线，只停容器（真实的「容器挂掉」）**

- 10:55:16 在 Agent 运行时执行 `docker stop`。Agent 没有自动拉起容器，容器保持 `exited`。
  - 10:55:42 产生 `container_down` 告警（id 16，计划 3 既有告警）。
  - `unreachable_count` 每 30 秒加 1。
  - 11:00:25 三个部署 `failed(unreachable)`「连续 10 次无法连接容器」，从停容器算起约 5 分 9 秒。
  - **恰好一条** `app_deploy_unreachable` 告警（id 17）。
  - 截图：`shots/09-unreachable.png`。
- 11:01 执行 `docker start`，三个应用重新部署。11:04:02 全部 running，日志都是 `skip install`。
- Agent 停机期间，平台日志除 `container unreachable` 的 WARN 外没有其他异常。
- 另注：计数期间 `health` 一直显示 `ok`（观察 O-5）。

### 10. 安全（§16.4.12）——通过

- **注入测试**：后台以 1.0.2 为底建 **1.0.3**，变量表加 `NOTE`（string，无 pattern，`max_len 256`），发布（动态码）。
  - vLLM 重新部署到 1.0.3，`NOTE` 填 `'; touch /tmp/pwned; '`。11:05:08 提交，11:07:40 running。
  - 容器里 `grep NOTE /root/data/.spark-apps/vllm/env` 输出 `export NOTE=''\''; touch /tmp/pwned; '\'''`，文件权限 `-rw-------`。
  - 容器和宿主机上都没有 `/tmp/pwned`。
  - 截图：`shots/10-note.png`。
- **控制台注入尝试**（`POST …/apps/<dep>/redeploy`，全部 400 `app_env_invalid`，部署没有被触发）：

  | 输入 | 提示 |
  |---|---|
  | 未声明变量 `LD_PRELOAD` / `SPARK_APP_DIR` / `PATH` / 小写 `note` | 「不是该应用声明的变量」 |
  | `NOTE` 含换行或 NUL | 「不能包含换行或 NUL」 |
  | `NOTE` 300 字节 | 「最多 256 字节」 |
  | enum 填 `$(id)` | 「不在可选项内」 |
  | int 填 `32768; reboot` | 「必须是整数」 |
  | pattern 字段填 ``0.7`id` `` | 「格式不正确」 |
  | secret 填 `a b;c` | 「至少 8 个字符」 |

- **后台版本校验**：变量名 `LD_PRELOAD`、`PATH`、`SPARK_PORT`、`BASH_ENV`、`A-B` 都被拒（`?validate=1`），返回 `app_version_invalid`「不得以 SPARK_ 开头，不得是 PATH / HOME / LD_* 等保留名」。
- **脱敏**：
  - 后台部署详情的变量表中 `API_KEY` 显示 `***`，`NOTE` 原样显示。截图：`shots/10-admin-dep.png`。
  - 后台日志尾部（60KB）里 vLLM 打印的 `api_key': ['***']` 出现 3 次，密钥原文出现 0 次。截图：`shots/10-admin-log.png`。
  - 控制台日志抽屉同样是 `api_key': ['***']`，原文 0 次。截图：`shots/10-console-log.png`。
  - 控制台和后台的接口只返回 `secret_env: {"API_KEY": true}`。
  - 在 `platform.log`、`app_deployments.env / log_tail / message`、`app_deploy_events`、`audit_logs`、`orders` 里搜索 `API_KEY` 和 `JUPYTER_TOKEN` 的原文：0 处。
  - 容器内 vLLM 自己的 `vllm.log` 有原文，属于用户容器内文件（观察 O-6）。
- **iptables**：`iptables -S SPARK-FWD` 和 `iptables -S SPARK-IN` 与 `iptables.ExpectedFwdRules(nil)`、`ExpectedInRules()` 逐行相等（conntrack ACCEPT、六个私网段 DROP、末尾 ACCEPT；SPARK-IN 为 conntrack ACCEPT 加 DROP），与计划 2/3 验收一致。

### 11. 对账无噪声（§16.4.13）与释放——通过

- **告警**：本次新增 4 条，10～13 是 10-19 已有的未处理告警，与本次无关。

  | id | 类型 | 来源 | 处理 |
  |---|---|---|---|
  | 14 | `machine_offline` | 09:04，开发 Mac 休眠期间（环境原因，O-9） | 保留 |
  | 15 | `machine_offline` | 第 9 步做法 1，Agent 停满 5 分钟 | 已处理 |
  | 16 | `container_down` | 第 9 步做法 2，计划 3 既有告警，模拟容器挂掉的预期结果 | 已处理 |
  | 17 | `app_deploy_unreachable` | 第 9 步做法 2 | 已处理 |

  `app_deploy_unreachable` 恰好一条。第 1～8、10 步没有产生任何告警。
- **ssh_sessions**：`SELECT count(*) FROM ssh_sessions WHERE instance_id='ins-tqzyl1us'` 结果为 **2**：
  - 05:23 前一个代理手工 SSH 一次。
  - 11:09:51 接手方手工 SSH 一次（经网关，`nvidia-smi` 看到 VLLM::EngineCore）。
  - 期间平台 exec 有数百次（部署、轮询、日志），没有产生会话行。
- **释放**：11:10:13 `POST /instances/ins-tqzyl1us/release` 返回 200。
  - 三个部署立即变为 `removed`（`instance_gone`）。
  - `instance_ports` 剩 0 行，审计里有 3 条 `instance.port_delete`。
  - 实例经 `wiping` 变为 `released`，机器 1 回到 `idle`，Agent `active`，二进制 SHA 不变。
- **收尾**：vLLM 1.0.2 / 1.0.3 下线，platform-p7 保持运行，供验收 B 使用。

### 缺陷

| 编号 | 现象 | 复现 | 原因（初判） | 严重度 | 修复 |
|---|---|---|---|---|---|
| A-1 | 平台起的应用进程里没有镜像的 Docker `ENV`。vLLM 1.0.0 启动时 Triton JIT 报 `cuda.h: No such file or directory`，部署 `failed(health_failed)`「服务进程已退出」。 | 在 `spark/llm-serving:25.09` 实例上部署 vllm 1.0.0（任意模型）→ 下载完成后约 1 分钟失败。对照 `docker exec <ctr> cat /proc/$(cat /root/data/.spark-apps/jupyterlab/service.pid)/environ`：只有精简的 `PATH`，没有 `CUDA_HOME / LD_LIBRARY_PATH / TRITON_* / NVIDIA_*`；`docker inspect` 里这些变量都在。 | `run.sh` 由平台经实例 sshd（上游 SSH exec）执行，sshd 会话不继承容器 ENV，`/.spark/init.sh` 也没有导出。Task 15 在真机上的验证大概率走的是 `docker exec`（继承 ENV），所以没发现。JupyterLab 和用户 SSH 会话同样缺这些变量，可能影响 Triton / torch.compile 等。 | 高（推理应用默认版本不可用） | 已修（`32fdfa3`），待真机复测：Agent init.sh 在 exec sshd 前把入口环境拷到 `/run/spark-image.environ`（`/proc/1/environ` 会被 sshd 的进程标题覆盖，不能用）；Runner 部署前读取、过滤后随包下发 `image.env`（0600），`run.sh` 先加载它再加载 `env`。需要新 Agent，且实例容器要在升级后创建或重启过。vLLM 1.0.4 不再带临时 export。原建议：建议：Agent / `init.sh` 启动时把容器 ENV 落成文件（例如 `/.spark/env`，0644，只放镜像 ENV），`run.sh` 在 source `env` 之前先 source 它；或在 sshd 配置 `SetEnv` / `PermitUserEnvironment`。修好后 vLLM 1.0.0 里的临时 export（1.0.1）即可去掉。 |
| A-2 | Open WebUI 默认设置下对话失败，页面显示 `"auto" tool choice requires --enable-auto-tool-choice and --tool-call-parser to be set`（vLLM 返回 400）。 | 同一实例部署 vllm（1.0.1）+ open-webui 1.0.0 → 注册 → 选 Qwen3-4B / 8B 发任意消息。 | `llm-serving` 镜像的 Open WebUI 没锁版本，实装 0.12.3，默认 `function_calling = native` 并带内置工具，请求里有 `tool_choice: auto`；vLLM 的 `app.sh` 没开 `--enable-auto-tool-choice --tool-call-parser hermes`。 | 高（§16.4 第 4 项的核心路径失败） | 已修（`5d07e8e`），待真机复测：vLLM 1.0.4 加 `--enable-auto-tool-choice --tool-call-parser hermes`，`llm-serving` Dockerfile 锁 `open-webui==0.12.3`。原建议：建议：vLLM `app.sh` 加 `--enable-auto-tool-choice --tool-call-parser hermes`（Qwen3 适用），同时在 Dockerfile 里锁定 Open WebUI 版本。验收中的规避：把用户设置 `function_calling` 设为 `legacy`，对话成功。 |
| A-3 | 规格 §16.4 第 11 项「Agent 停 + `docker stop` → 10 轮后 `failed(unreachable)`、告警一条」无法发生：Agent 失联满 5 分钟后实例被暂停，Runner 跳过轮询（§6.3），计数停在 7～8；Agent 恢复后会立刻拉起容器，三个部署走 `alive=0` 变成 `failed(crashed)`，需要逐个重新部署。 | 按 brief 第 9 步顺序：先 `systemctl stop spark-agent`，约 2 分钟后 `docker stop`，等 10 分钟。 | 验收清单与 §6.3「机器失联跳过轮询」冲突（10 轮 × 30 秒 ≈ Agent 失联判定的 5 分钟）。Agent 在线时只停容器，unreachable 路径和告警都正确（做法 2）。 | 中（规格 / 清单不一致；产品行为符合 §6.3） | 已修（规格 §16.4 第 11、13 项与计划 Task 22 第 9 步改为两个场景分开验证），待复测。原建议：建议改清单：先只 `docker stop`（Agent 在线）验证 unreachable，再单独验证 Agent 停机期间轮询照常。暂停期间应用进程被 Agent 重启容器后需要手工重新部署，已在 Task 10 的遗留里登记，可考虑恢复时把 crashed 自动重新排队。 |
| A-4 | vLLM 行主按钮「打开」打开的是端口根路径，vLLM 返回 `{"detail":"Not Found"}`。 | vLLM running → 实例详情点「打开」。 | 控制台把 `url`（主端口根）作为「打开」目标；spec 里端口的 `path: "/v1"` 没有用上，`links` 只放在「链接」下拉里。 | 低（「链接 → 接口文档」可用） | 已修（`4c6c998`、`5d07e8e`），待真机复测：部署视图的 `url`（「打开」、列表摘要）= 主端口 URL + `ports[0].path`，vLLM 的 `ports[0].path` 改为 `/docs`。原建议：建议：有 `ports[0].path` 时把它拼进 `url`，或在根路径 404 的应用上默认打开第一个 `links`（vLLM 填 `/docs`）。 |

### 观察（列入遗留）

O-3、O-4、O-5、O-7 已修（`5d07e8e`、`ee03e67`、`042bb40`），其余见 `followups-plan7.md`。

- **O-1**：本地 tag 镜像（`spark/*:25.09`）每次登记或发布版本都会广播 `pull_image`，Docker Hub 不可达导致每次都失败。本次共 11 条 failed 指令，后台机器详情的指令列表一片红。可考虑「机器已缓存该 ref 时不下发 pull」。
- **O-2**：平台日志没有部署生命周期的 INFO 行（只有 unreachable 的 WARN）。排障依赖库里的事件表和后台页面。
- **O-3**：vLLM 的部署日志用 `tail -n 200 -f vllm.log` 并入，而 `vllm.log` 是追加写的，所以每次部署的日志开头会混入上一次运行的 200 行（例如第 8 步恢复后的日志里还有 10:21、10:24 的启动输出）。可在 start 时轮转 `vllm.log`。
- **O-4**：实例到期停机时，应用行状态标签仍是绿色「运行中」（state 不变，按规格 §6.3），只靠下方的灰字「实例已停机，续费后自动恢复」提示。建议停机时把标签改为灰色「已停机」。
- **O-5**：容器不可达计数期间（`unreachable_count` 1～9），部署的 `health` 一直显示 `ok`，直到 failed 才变化。可在第一次 DialError 时置为 `unknown`。
- **O-6**：容器内 `/root/data/.spark-apps/vllm/vllm.log` 和 `log` 里有 `api_key` 原文（vLLM 自己打印的参数）。只限用户自己的容器，平台侧展示和存储都已脱敏。若用户把数据盘内容分享出去会带出密钥，帮助页可以提醒一句。
- **O-7**：失败原因的展示有重复：`crashed` 显示为「服务进程已退出：服务进程已退出」；`health_failed` 的 message 写的是「服务进程已退出」（进程在健康检查阶段退出时），错误码却是「健康检查未通过」，用户容易困惑。
- **O-8**：换版本（spec_hash 变化）后的重新部署会重新跑 install。`modelscope download` 对已存在的文件秒过，实际耗时与 skip install 相近，符合预期。
- **O-9**：验收中途开发 Mac 休眠 / 会话暂停（05:50～10:18）。醒来时平台出现 `upstream keepalive failed` 和 `appdeploy runner tick … context deadline exceeded`，以及一条 `machine_offline` 告警（id 14）。之后自愈，部署状态没有被误判。属于环境因素，仅记录。
- **O-10**：释放实例会洗掉数据盘，模型缓存随之清空。Task 23 的 ComfyUI 和卸载重装需要在新实例上重新下载（在 Spark 上的实例容器里下载）。
- **O-11**：清单写「添加按钮禁用」，实际是达到上限后按钮隐藏、显示上限文字；API 上 `app_limit` 先于 `app_exists`。两者都能达到提示目的。
