# Spark Cloud — 计划 11：ComfyUI 工作流市场设计文档

- 日期：2026-10-24
- 状态：待评审（负责人已授权设计方直接裁定、不再逐项询问；代负责人作出的裁定集中列在 §1.3，负责人可事后推翻）
- 上位文档：`2026-10-20-plan7-deploy-design.md`（下称「计划 7」）§1、§3、§5、§6、§8、§9、§10；`2026-10-14-spark-cloud-mvp-design.md`（下称「总设计」）附录竞品调研（器灵算力站「ComfyUI 工作流」）；`2026-10-18-plan3-access-design.md`（下称「计划 3」）§4、§5.3
- 范围：后台维护的 ComfyUI 工作流目录（带版本：工作流图、所需模型、所需自定义节点、可选的「快速出图」表单定义）；控制台工作流市场页；用户把工作流**一键安装**到自己实例上的 ComfyUI（模型与节点在实例容器内从 ModelScope / hf-mirror / Git 镜像下载）；开通实例时预装 ComfyUI + 工作流；已安装工作流的更新与移除；基于 ComfyUI API 的极简「快速出图」表单（不打开 ComfyUI 画布也能跑）；后台导入 / 版本 / 发布 / 统计；测试与真机验收。
- 不在范围：见 §17。
- 前置：**计划 7 已合并到 `main`**（本文按此假定写；分支 `feat/plan11-comfy-workflows` 从 `main` 切出，实施前先把它 rebase 到含计划 7 的 `main`）。迁移编号 00020～00023 已被计划 7～10 预留，**本计划用 `00024_comfy_workflows.sql`**。

## 0. 现状与约束（实现时不必再查）

**计划 7 已提供（`internal/appdeploy`，下文直接复用，不再重做）**
- 应用目录 `apps / app_versions`（版本 = `spec` + `app.sh`，发布后不可变，`spec_hash`），部署 `app_deployments`（`queued → installing → running ⇄ stopped / failed / removing → removed / lost`），事件表 `app_deploy_events`。`comfyui` 应用（slug `comfyui`，镜像 `spark/comfyui:25.09` = NGC PyTorch 25.09 + ComfyUI `v0.3.60` + modelscope + JupyterLab，有 `git`、`curl`、`tar`、`setsid`、`python3`，**没装 ComfyUI-Manager**）版本 1.0.0：端口 8188（健康路径 `/system_stats`）、变量 `MODEL_ID`（enum：SDXL base / FLUX.1-schnell / Qwen-Image）与 `MODEL_SOURCE`（`modelscope | hf-mirror`）、`timeout_minutes 90`、`min_data_free_gib 50`；`install` 下载单文件权重到 `/root/data/models/<来源>/<仓库>/` 并软链进 `/opt/ComfyUI/models/<子目录>`；`start` = `python3 main.py --listen 127.0.0.1 --port $SPARK_PORT --output-directory /root/data/comfyui-output`。
- 部署执行：Runner 每 5 秒一轮，经 `upstream.Pool.Exec` 把**部署包**（tar：`run.sh`、`app.sh`、`spec.json`、`env`、`image.env`；`appdeploy.Bundle(BundleInput, runSH)`，上限 `MaxBundleBytes = 262144`，`UploadCommand(dir)` = `mkdir -p … && tar -x -C <dir> -f - && chmod …`）传进容器 `/root/data/.spark-apps/<slug>/`，`sh run.sh deploy` 以 `setsid` 后台执行；`run.sh` 的 `install_key()` = `SPARK_SPEC_HASH + cksum(env)`，与 `.installed` 一致则跳过 `app.sh install`；`app.sh` 用 `spark_status <phase> <progress> <message>` 汇报；失败消息以 `disk:` 开头映射为错误码 `disk`。健康检查平台侧用 `http.Transport{DialContext: Pool.Dial(target, port)}`（`runner.go` 第 866 行一带）——**平台已经能对容器内 `127.0.0.1:8188` 发 HTTP 请求**，本计划的「快速出图」代理走同一条路。
- 用户操作：`Service.Create / Redeploy(RedeployRequest{VersionID *int64, Env, EnvSet}) / Stop / Start / Remove / Log / Events`，都在 `rental.LockOwned` 下进行；`Redeploy` 对 `queued` 的部署只改变量，对 `running / stopped / failed` 置回 `queued`（Runner 重新上传部署包并 `deploy`）。每实例 ≤ 3 个应用，每用户每小时 60 次写操作（`api/apps.go` 的 `userLimiter`）。
- 下单预装：`rental.AppHooks.PrepareInstall(ctx, q, appID, imageID, env) (InstallPlan, error)` / `QueueInstall(ctx, tx, inst, plan, now)`；`POST /orders` / `POST /bookings` 请求体 `app_id? / app_env?`（`api/booking.go` 第 277 行）。`OnInstanceRunning`（续费恢复：running / stopped → queued）、`OnInstanceGone`、`OnInstanceMigrated`（换机复制为 queued）。
- 保留变量（`appdeploy.ReservedEnv`）：`SPARK_APP / SPARK_APP_DIR / SPARK_DATA_DIR / SPARK_PORT / SPARK_MODELS_DIR / SPARK_SPEC_HASH / HF_ENDPOINT / MODELSCOPE_CACHE / HF_HOME / PIP_INDEX_URL` 等；配置 `SPARK_APP_HF_ENDPOINT`（默认 `https://hf-mirror.com`）、`SPARK_APP_PIP_INDEX_URL`（默认清华 PyPI）、`SPARK_APP_RUNNER`。
- 目录导入 `platform admin app import deploy/apps`（`cmd/platform/app_import.go`，`appdeploy.Catalog.ImportDir`）；`go test ./internal/appdeploy -run TestAppsDir` 校验 `deploy/apps/*`。
- 后台：菜单 `应用 /apps`、`应用部署 /deployments`（`web/admin/src/menu.ts`，`menu.spec.ts` 表驱动核对）；版本发布 super + TOTP，审计 `app.version.publish`；`AppVersionView.vue` 结构化 spec 表单。
- 控制台：`/market` 的「一键部署」应用卡片 → `RentDialog`（预装应用 + `AppEnvForm`）；实例详情 `AppsCard.vue`（每个部署一行：进度、打开、重新部署、停止 / 启动、日志、卸载）、`AppDeployDialog.vue`、`AppLogDrawer.vue`、`AppPicker.vue`；`logic/apps.ts`；类型 `web/shared/src/types/apps.ts`、文案 `labels.ts`。
- e2e：`sshtest` shell 模式（`exec` 真执行 `/bin/sh -c`），`/hooks/apps/seed`（fake 应用）、`/hooks/apps/tick`（Runner 轮次）；覆盖表到 `C-MKT-12`、`C-APP-10`、`C-ING-07`、`A-APP-08`、`A-INS-14`、`A-MCH-10`。
- 文件上传：`internal/storage`（`Service.Save(ctx, userID, purpose, r)`，目前只接受 `purpose = license`，按内容判断 JPG / PNG / PDF，≤ 10MB）；后台 `adminapi.Server.Uploads`。

**ComfyUI 事实（v0.3.60，实施时核实）**
- 启动参数：`--user-directory <dir>`（用户数据：设置、`default/workflows/*.json` 即前端侧栏「工作流」里的列表）、`--input-directory`、`--output-directory`、`--extra-model-paths-config <yaml>`（可声明 `base_path` 下的 `checkpoints / unet / diffusion_models / clip / text_encoders / vae / loras / upscale_models / controlnet / clip_vision / custom_nodes` 等目录；`custom_nodes` 条目会被当作附加的自定义节点目录加载）。
- HTTP API（监听 `127.0.0.1:8188`）：`POST /prompt {prompt, client_id}` → `{prompt_id, number, node_errors}`（`prompt` 是 **API 格式**，与前端保存的 UI 格式不同，由前端「Save (API Format)」导出）；`GET /history/{prompt_id}` → `{<id>: {status: {status_str, completed}, outputs: {<node_id>: {images: [{filename, subfolder, type}]}}}}`；`GET /view?filename=&subfolder=&type=output` 返回图片 / 视频文件；`POST /upload/image`（multipart `image`，可选 `subfolder`）→ `{name, subfolder, type}`；`GET /system_stats`、`GET /queue`、`GET /object_info`。
- 自定义节点 = `custom_nodes/<目录>` 下的 Python 包，启动时 import；多数带 `requirements.txt`。装完必须**重启 ComfyUI** 才生效。
- 国内网络：GitHub 直连不稳定，需要 Git 镜像前缀（如 `https://gh-proxy.com/https://github.com/...`）；模型走 ModelScope / hf-mirror（计划 7 已验证可达）。

**竞品**（器灵算力站）：工作流列表（封面图、分类、标题、所需模型大小）→ 一键部署到自己的 ComfyUI → 打开 ComfyUI 使用。用户视角的核心是：**挑一个工作流、点一下、等进度条、打开就能出图**，不用自己找模型和节点。

## 1. 架构与核心决策

```
 控制台：/workflows 工作流市场（卡片、分类、详情、一键安装）   实例详情「应用」卡片 ComfyUI 行 → 「工作流」子区块（已装 / 更新 / 移除 / 快速出图）
                          │                                                      │
                          ▼                                                      ▼
┌──────────────── comfywf（新包 internal/comfywf：目录 + 安装 + 快速出图代理）──────────────────────────────┐
│  comfy_workflows / comfy_workflow_versions（后台维护；版本 = graph(UI JSON) + manifest(模型、节点、表单) +  │
│      可选 api_prompt(API JSON)，发布后不可变）                                                                 │
│  deployment_workflows：某个 ComfyUI 部署上「应装」的工作流集合（pending → installed / failed）                │
│  安装 = 改集合 + appdeploy.Redeploy：部署包多带 workflows/manifest.json 与 workflows/<slug>.json；             │
│      进度 / 日志 / 失败 / 停机恢复 / 换机 全部沿用计划 7 的部署机制，不新增后台执行者                          │
│  快速出图 = 平台经 Pool.Dial 代理 ComfyUI 的 /prompt /history /view /upload/image（白名单四个端点）           │
└───────────────────────────────────────────────────────────────────────────────────────────────────────────┘
                          │  部署包（tar，经 Pool.Exec 上传，与计划 7 同一条路）
                          ▼
 容器内：/root/data/.spark-apps/comfyui/{run.sh, app.sh, env, workflows/manifest.json, workflows/<slug>.json}
         comfyui 应用 1.1.0 的 app.sh install：按 manifest 下模型（ModelScope / hf-mirror）→ 软链到 /root/data/comfyui/models/<子目录>
                                               → git clone 固定 commit 的自定义节点到 /root/data/comfyui/custom_nodes/<名> + pip install
                                               → 把工作流 JSON 放进 /root/data/comfyui/user/default/workflows/
         start：python3 main.py --extra-model-paths-config … --user-directory /root/data/comfyui/user …（重启即加载新节点）
```

### 1.1 决策一览

| # | 决策 | 结论 | 理由 |
|---|---|---|---|
| 1 | 「安装工作流」如何执行 | **就是 ComfyUI 部署的一次重新部署**：平台把该部署「应装的工作流集合」连同每个工作流的 JSON 与模型 / 节点清单放进部署包，`app.sh install` 按清单补齐，`start` 重启 ComfyUI；没有第二套执行者 | 装自定义节点本来就要重启 ComfyUI；计划 7 的 Runner 已解决进度、日志、超时、失败、停机恢复、换机、乐观并发，再写一个「工作流安装器」是重复造轮子 |
| 2 | 清单在哪里解析 | 平台把 manifest 规范化成**容器内脚本易读的 JSON**随包下发；`app.sh` 用镜像自带的 `python3` 把它展开成每行一条的「模型 / 节点」列表再用 sh 处理（与 1.0.0 的 `model_files` 行格式一致） | 不新增容器内运行时文件；清单格式由平台定义与校验，脚本只消费 |
| 3 | 模型 / 节点存放 | 全部在**数据盘**：模型缓存仍是 `/root/data/models/<来源>/<仓库>/`（与计划 7 共用、卸载不删），软链到 `/root/data/comfyui/models/<子目录>/`；自定义节点 `/root/data/comfyui/custom_nodes/<名>/`；用户目录 `/root/data/comfyui/user/`；用 `extra_model_paths.yaml` 告诉 ComfyUI | `/opt/ComfyUI` 在镜像层，容器重建后丢；计划 7 的软链进 `/opt` 要每次 start 重做。放数据盘后停机 / 续费 / 重新部署都不丢，换机才需重下 |
| 4 | 自定义节点的来源 | 版本里**固定 Git 地址 + 40 位 commit**；容器内先经 `SPARK_GIT_MIRROR` 前缀（配置，默认 `https://gh-proxy.com/`）clone，失败再直连；`pip install -r requirements.txt`（PyPI 镜像已注入）；**不装 ComfyUI-Manager**、不允许用户自装 | 节点是第三方代码、在用户容器里以 root 执行；只允许 super 经 TOTP 发布的、钉死 commit 的节点 |
| 5 | 「快速出图」要不要做 | 做**极简版**：版本可附带 API 格式的 `api_prompt` 与 `inputs` 表单定义；平台代理 ComfyUI 的 4 个端点；一个抽屉：填表 → 运行 → 轮询 → 显示 / 下载输出图 | 竞品只有「打开 ComfyUI」；一个表单能把「不会用节点图」的用户留下。代理走已有的 `Pool.Dial`，不新开入口 |
| 6 | 快速出图为什么走平台代理而不是浏览器直连端口 URL | 端口 URL 是另一个子域（`ins-<id>-8188.<域>`），浏览器从控制台跨域 fetch 要 CORS + Cookie，且 ComfyUI 没有鉴权；平台代理只放行 4 个端点、限大小与频率、按实例归属鉴权 | 安全边界清楚，ComfyUI 本体仍只在私有端口后面 |
| 7 | 工作流和 ComfyUI 应用版本的关系 | 本计划发布 **comfyui 应用 1.1.0**（spec 增加 `supports_workflows: true`，`app.sh` 支持清单；保留 1.0.0 的 `MODEL_ID / MODEL_SOURCE` 变量并新增 `MODEL_ID = none`）；在 1.0.0 的部署上安装工作流会自动升到最新 published 版本（本来就是重新部署） | 用户不用理解「先升级应用再装工作流」 |
| 8 | 版本与更新 | 工作流版本发布后不可变；已装工作流显示「有新版本」，用户点「更新」= 换版本重新部署；**不自动更新** | 与计划 7 D10 一致：平台改图不能把用户正在用的流程改坏 |
| 9 | 预装 | `POST /orders` / `/bookings` 新增 `app_workflow_ids?: int64[]`（仅 `app_id = comfyui` 时有效）；市场页工作流卡片的「新开实例安装」= 打开 `RentDialog` 预选 ComfyUI 应用 + 该工作流 | 竞品的「一键」从市场页开始，不是从实例详情开始 |
| 10 | 定价 | **免费**（与计划 7 D1 一致） | 只是实例里的文件和进程 |
| 11 | 多副本 / 并发 | 没有新的后台循环；集合修改在 `LockOwned` 事务内；快速出图代理无状态（运行记录只做统计） | 不引入新的锁 |

### 1.2 不做的替代方案（评审时不必再提）
- 「平台直接 SSH 进容器写文件 / 跑下载」：绕开 Runner 就要自己做进度、超时、幂等、停机恢复；放弃。
- 「装 ComfyUI-Manager 让用户自己装节点」：Manager 会从任意 Git 地址拉代码并 pip install，与 §8 的边界冲突；用户想自由折腾可以 SSH 进去自己来，平台不负责。
- 「工作流 JSON 由容器从平台 / 对象存储下载」：容器不能访问机房内网（计划 2 §4.4），公网域名在验收环境不可达；随部署包下发最简单（放宽部署包上限即可）。
- 「快速出图用 WebSocket 推进度」：ComfyUI 的 `/ws` 要长连接代理；极简版用 2 秒轮询 `/history` 够用。
- 「把工作流做成独立的 `apps` 应用」：每个工作流一个应用会撞 D2 的「每实例 3 个应用」上限，且共用一个 ComfyUI 进程的事实对不上。
- 「用户上传自己的工作流到市场」：用户生成内容需要审核与节点来源审查；遗留。

### 1.3 负责人裁定（默认，可推翻）

| # | 裁定 | 理由 |
|---|---|---|
| D1 | 工作流安装 / 快速出图**免费** | §1.1 第 10 条 |
| D2 | 每个 ComfyUI 部署最多同时保留 **8** 个工作流；每个工作流 JSON（UI 格式）≤ **256KB**，`api_prompt` ≤ 256KB；部署包上限从 256KB 放宽到 **4MiB**（`appdeploy.MaxBundleBytes`） | 8 × 256KB + 脚本 < 4MiB；一条 exec 上传 4MiB 在 LAN 上 1 秒以内 |
| D3 | 工作流分类固定：`txt2img`（文生图）、`img2img`（图生图）、`video`（视频）、`upscale`（放大修复）、`other`；标签 ≤ 8 个 ≤ 16 字符 | 竞品分类 + 预留 |
| D4 | 模型只允许来自 **ModelScope** 与 **hf-mirror**（每个模型可同时给两处地址，容器内按部署的 `MODEL_SOURCE` 先选对应源，没有就用另一个），必须声明目标子目录与大小（GB，进度与磁盘预检用）；可选 `sha256`（有则下载后校验） | 与计划 7 D4 一致；不接 CivitAI（需登录、速率不稳） |
| D5 | 自定义节点 Git 主机白名单：`github.com`、`gitee.com`、`gitcode.com`；必须钉 40 位 commit；每个版本 ≤ 8 个节点；pip 依赖只装节点自带的 `requirements.txt`（不允许清单里写任意 pip 包） | 见 §1.1 第 4 条 |
| D6 | 发布工作流版本 = **super + TOTP**（与发布应用版本同级）；ops 可查看目录、统计、任意部署的工作流状态；finance 只读 | 节点代码在用户容器以 root 执行，权限等同发布脚本 |
| D7 | 安装时的磁盘预检：平台只在控制台展示「需下载约 X GB」，不替容器判断；容器内 `app.sh` 在每个模型下载前 `df`，剩余 < 模型大小 + 5GiB → 先 `spark_status download <pct> "disk: …"` 再非零退出；`run.sh` 对 install 失败的消息改为「status.json 最后一条消息以 `disk:` 开头时沿用它」（目前一律写「install 失败，退出码 N」），这样平台侧 `failCode` 映射为错误码 `disk` | 平台不知道容器实际剩余；`run.sh` 的 `min_data_free_gib`（50）是整包门槛 |
| D8 | 移除工作流：从集合删除并重新部署；容器内删掉 `user/default/workflows/<slug>.json`，**模型与自定义节点保留**（控制台提示占用与手工清理路径）；卸载 ComfyUI 应用同样保留 `/root/data/comfyui/` 与模型 | 与计划 7 D4「卸载不删模型」一致；节点目录很小 |
| D9 | 工作流安装 / 更新 / 移除都会**重启 ComfyUI**（正在生成的任务会中断）；控制台操作前确认文案写明 | 装节点必须重启；为一致性统一处理 |
| D10 | 快速出图：每个部署同时 ≤ **2** 个未完成的运行（超出 429 `workflow_busy`）；每用户每小时 ≤ **120** 次运行；单次运行等待上限 30 分钟（之后前端停止轮询、结果仍可在 ComfyUI 历史里看）；输出文件代理单个 ≤ **64MB**（视频）、上传图片 ≤ **16MB**；运行记录只存元数据（不存图） | 一台 GB10 同时跑两张图已经很慢；64MB 够 720p 短视频 |
| D11 | 快速出图的 `seed` 类型输入：用户填 `-1` 或留空 = 平台生成随机种子并回显在结果里 | ComfyUI 本身 `control_after_generate` 只在前端生效，API 侧要自己随机 |
| D12 | 首批工作流 **6 个**（§9）：SDXL 文生图、SDXL 图生图、FLUX.1-schnell 文生图（fp8）、Qwen-Image 文生图、Wan2.2 5B 文 / 图生视频、SDXL 放大修复（唯一带自定义节点 `ComfyUI_UltimateSDUpscale` 的，用来验证节点机制）；全部带 `api_prompt` + `inputs`（快速出图可用） | 覆盖四个分类、覆盖「只要模型」和「要节点」两条路径；都是 ComfyUI 官方示例里有的组合，GB10 128GB 统一内存放得下 |
| D13 | 封面图：后台上传（PNG / JPG / WebP，≤ 2MB，`storage` 新用途 `workflow_cover`，所有者 `user_id = 0`），控制台 `GET /api/v1/workflows/{slug}/cover` 匿名可取并带 `Cache-Control`；没有封面显示分类占位图 | 市场页没有封面像后台表格 |
| D14 | 换机（`OnInstanceMigrated`）复制部署时**连带复制工作流集合**（全部置 `pending`）；实例释放时集合随部署进入历史、不删行 | 与计划 7 §6.4 一致 |
| D15 | `comfyui` 1.1.0 的 `MODEL_ID` 新增选项 `none`（不下预置模型）并设为默认：装工作流的用户不需要 1.0.0 的「先选一个模型」；市场页「一键部署 ComfyUI」卡片（计划 7）行为不变（仍可选 SDXL 等） | 工作流清单已经带模型；默认再下 7GB SDXL 是浪费 |
| D16 | 工作流版本的 `graph` 引用到的模型文件名（`CheckpointLoaderSimple.ckpt_name`、`UNETLoader.unet_name`、`VAELoader.vae_name`、`CLIPLoader / DualCLIPLoader.clip_name*`、`LoraLoader.lora_name`、`UpscaleModelLoader.model_name`）必须出现在 `manifest.models[].name` 里，否则发布校验**报错**（`?validate=1` 同样返回）；`graph` 里出现的非内置节点类型（`properties.cnr_id` / `aux_id` 不是 `comfy-core`）必须对应 `manifest.nodes[]` 里的某一项（按节点声明的 `node_types` 列表匹配），否则报错 | 「装好打不开」是工作流市场最常见的差评；校验在发布时就把它堵住 |

## 2. 数据模型（迁移 `00024_comfy_workflows.sql`，带 `+goose Down`）

```sql
CREATE TABLE comfy_workflows (
    id          BIGSERIAL PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,            -- ^[a-z][a-z0-9-]{1,47}$；容器内文件名 <slug>.json
    name        TEXT NOT NULL,                   -- 1～48 字符
    summary     TEXT NOT NULL DEFAULT '',        -- ≤ 120 字符，卡片副标题
    description TEXT NOT NULL DEFAULT '',        -- ≤ 8000 字符，Markdown 子集（同 apps.description）
    category    TEXT NOT NULL CHECK (category IN ('txt2img', 'img2img', 'video', 'upscale', 'other')),
    tags        TEXT[] NOT NULL DEFAULT '{}',    -- ≤ 8 项，每项 ≤ 16 字符
    cover_key   TEXT,                            -- storage 里的封面文件 key；NULL = 占位图
    sort        INT  NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'retired')),
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);

CREATE TABLE comfy_workflow_versions (
    id           BIGSERIAL PRIMARY KEY,
    workflow_id  BIGINT NOT NULL REFERENCES comfy_workflows (id),
    version      TEXT NOT NULL,                  -- ^[0-9A-Za-z][0-9A-Za-z.-]{0,31}$
    status       TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'retired')),
    graph        JSONB NOT NULL,                 -- ComfyUI 前端保存的 UI 格式工作流（≤ 256KB）
    api_prompt   JSONB,                          -- ComfyUI「Save (API Format)」导出（≤ 256KB）；NULL = 不支持快速出图
    manifest     JSONB NOT NULL,                 -- §3.2：models / nodes / inputs / outputs
    download_gb  NUMERIC(8,2) NOT NULL DEFAULT 0,-- 发布时由 manifest.models[].size_gb 求和，列表展示用
    manifest_hash TEXT NOT NULL,                 -- sha256(canonical(graph) || canonical(manifest) || canonical(api_prompt))，发布时冻结
    changelog    TEXT NOT NULL DEFAULT '',       -- ≤ 1000 字符
    published_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (workflow_id, version)
);
CREATE INDEX comfy_workflow_versions_wf_idx ON comfy_workflow_versions (workflow_id, status, published_at DESC);

-- 某个 ComfyUI 部署上「应装」的工作流集合；部署包按它生成。一个部署上一个工作流只有一行（换版本 = 改 version_id）。
CREATE TABLE deployment_workflows (
    deployment_id       TEXT   NOT NULL REFERENCES app_deployments (id),
    workflow_id         BIGINT NOT NULL REFERENCES comfy_workflows (id),
    workflow_version_id BIGINT NOT NULL REFERENCES comfy_workflow_versions (id),
    state               TEXT   NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'installed', 'failed')),
    added_at            TIMESTAMPTZ NOT NULL,
    installed_at        TIMESTAMPTZ,
    PRIMARY KEY (deployment_id, workflow_id)
);
CREATE INDEX deployment_workflows_wf_idx ON deployment_workflows (workflow_id);

-- 快速出图运行记录（只做统计与归属校验；图片不落库）
CREATE TABLE comfy_workflow_runs (
    id                  TEXT PRIMARY KEY,        -- 'run-' + 12 位 [a-z0-9]
    deployment_id       TEXT   NOT NULL REFERENCES app_deployments (id),
    user_id             BIGINT NOT NULL REFERENCES users (id),
    workflow_version_id BIGINT NOT NULL REFERENCES comfy_workflow_versions (id),
    prompt_id           TEXT   NOT NULL,         -- ComfyUI 返回的 prompt_id（UUID）
    seed                BIGINT,                  -- 平台生成或用户填写的种子（D11）
    state               TEXT   NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'done', 'error')),
    created_at          TIMESTAMPTZ NOT NULL,
    finished_at         TIMESTAMPTZ
);
CREATE INDEX comfy_workflow_runs_dep_idx ON comfy_workflow_runs (deployment_id, created_at DESC);
CREATE INDEX comfy_workflow_runs_user_idx ON comfy_workflow_runs (user_id, created_at DESC);
```

说明：
- `comfy_workflows.status`：`draft` 只在后台可见；`published` 且至少有一个 `published` 版本才进市场；`retired` 不可新装，已装的照常。「当前版本」= `published` 中 `published_at` 最新的一条。
- `deployment_workflows.state`：加入 / 换版本时 `pending`；该部署下一次进入 `running` 时全部 `pending → installed`（`installed_at = now`）；进入 `failed` 时 `pending → failed`（用户重新部署成功后再变 `installed`）。`removed / lost` 的部署的行保留（历史），不占任何唯一约束。
- 没有改 `app_versions` 表：`supports_workflows` 是 `spec` JSON 里的新字段（§3.1）。
- **Down**：`DROP TABLE comfy_workflow_runs, deployment_workflows, comfy_workflow_versions, comfy_workflows`；容器内已装的工作流与模型不受影响。
- **存量数据**：无。

## 3. 目录与版本

### 3.1 `comfyui` 应用 1.1.0（`deploy/apps/comfyui`，计划 7 的目录，升版本）
- `spec.json`：新增 `"supports_workflows": true`（`appdeploy.Spec` 新字段，`ValidateSpec` 只接受 bool；其它应用不写）；`MODEL_ID.options` 前面加 `none` 并设 `default = "none"`（D15）；其余不变（8188、`/system_stats`、90 分钟、50GiB）。
- `app.sh`：`install` = ①若 `MODEL_ID != none`，1.0.0 的预置模型下载（原逻辑，软链目标改为 `/root/data/comfyui/models/<子目录>/`）；②若存在 `$SPARK_APP_DIR/workflows/manifest.json`，按 §5.2 安装工作流；`start` = 生成 `extra_model_paths.yaml`、建目录、`python3 main.py --listen 127.0.0.1 --port $SPARK_PORT --extra-model-paths-config $SPARK_APP_DIR/extra_model_paths.yaml --user-directory /root/data/comfyui/user --input-directory /root/data/comfyui/input --output-directory /root/data/comfyui-output`；`stop` 不需要（`run.sh` 杀进程组）。
- `app.json`：`version = 1.1.0`，changelog「支持工作流市场；模型 / 节点 / 用户目录放到数据盘；MODEL_ID 可选 none」。导入后在后台发布；1.0.0 保持 published（已部署的用户不受影响，装工作流时自动升级）。

### 3.2 工作流版本 `manifest`（JSON）

```json
{
  "comfyui_min": "0.3.60",
  "models": [
    {"name": "sd_xl_base_1.0.safetensors", "dir": "checkpoints", "size_gb": 6.9,
     "modelscope": {"repo": "AI-ModelScope/stable-diffusion-xl-base-1.0", "file": "sd_xl_base_1.0.safetensors"},
     "hf":         {"repo": "stabilityai/stable-diffusion-xl-base-1.0",   "file": "sd_xl_base_1.0.safetensors"},
     "sha256": ""}
  ],
  "nodes": [
    {"name": "ComfyUI_UltimateSDUpscale", "git": "https://github.com/ssitu/ComfyUI_UltimateSDUpscale.git",
     "commit": "0123456789abcdef0123456789abcdef01234567", "node_types": ["UltimateSDUpscale", "UltimateSDUpscaleNoUpscale"]}
  ],
  "inputs": [
    {"key": "prompt",   "label": "提示词",   "type": "textarea", "node": "6", "input": "text", "default": "a photo of ...", "max_len": 2000},
    {"key": "negative", "label": "反向提示词", "type": "textarea", "node": "7", "input": "text", "default": "", "max_len": 2000},
    {"key": "width",    "label": "宽",      "type": "int",  "node": "5", "input": "width",  "default": 1024, "min": 512, "max": 2048, "step": 64},
    {"key": "height",   "label": "高",      "type": "int",  "node": "5", "input": "height", "default": 1024, "min": 512, "max": 2048, "step": 64},
    {"key": "steps",    "label": "步数",     "type": "int",  "node": "3", "input": "steps",  "default": 25, "min": 1, "max": 100},
    {"key": "cfg",      "label": "CFG",     "type": "float","node": "3", "input": "cfg",    "default": 7.0, "min": 1, "max": 30},
    {"key": "sampler",  "label": "采样器",   "type": "enum", "node": "3", "input": "sampler_name", "default": "euler", "options": ["euler", "dpmpp_2m", "dpmpp_2m_sde"]},
    {"key": "seed",     "label": "种子",     "type": "seed", "node": "3", "input": "seed"},
    {"key": "image",    "label": "参考图",   "type": "image","node": "10", "input": "image"}
  ],
  "outputs": [{"node": "9"}]
}
```

字段约束（`comfywf.ValidateManifest(m *Manifest, graph, apiPrompt json.RawMessage)`，错误码 `workflow_version_invalid`，消息以字段路径开头）：
- `comfyui_min`：可空；形如 `^\d+\.\d+\.\d+$`，只做展示（镜像里 ComfyUI 版本固定，运行时不比对）。
- `models`：0～16 项。`name` ~ `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`（文件名，不含 `/`），同一版本内唯一；`dir ∈ {checkpoints, diffusion_models, unet, text_encoders, clip, vae, loras, upscale_models, controlnet, clip_vision, embeddings, style_models, vae_approx}`；`size_gb` 0.01～200（两位小数）；`modelscope` / `hf` 至少一个，`repo` ~ `^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`，`file` 是仓库内相对路径（每段 ~ `^[A-Za-z0-9][A-Za-z0-9._-]*$`、不含 `..`、≤ 256 字符）；`sha256` 可空或 64 位十六进制。
- `nodes`：0～8 项。`name` ~ `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`（目录名，唯一）；`git` 为 `https://` 且主机 ∈ D5 白名单，路径 ~ `^/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(\.git)?$`；`commit` 40 位十六进制；`node_types` 1～64 项，每项 ~ `^[A-Za-z0-9_.: -]{1,64}$`（用于 D16 的匹配）。
- `inputs`：0～24 项（`api_prompt` 为空时必须为空）。`key` ~ `^[a-z][a-z0-9_]{0,31}$` 唯一；`label` ≤ 32；`type ∈ text | textarea | int | float | bool | enum | seed | image`；`node` 必须是 `api_prompt` 的键，`input` 必须是该节点 `inputs` 里已有的键且**现值不是连线**（数组 `[node_id, slot]` 的是连线，不能当参数）；`int / float` 带 `min / max`（`step` 可选，`int` 用）、`default` 在区间内；`text / textarea` `max_len` 默认 1000、上限 4000；`enum` 的 `options` 1～32 项 ≤ 64 字符、`default` 在内；`seed` 对应的现值必须是整数；`image` ≤ 2 项（上传的图片名写进 `LoadImage.image`）；`bool` 对应布尔。
- `outputs`：0～4 项，`node` 是 `api_prompt` 里 `class_type ∈ {SaveImage, SaveAnimatedWEBP, SaveWEBM, VHS_VideoCombine, SaveVideo}` 的节点；为空时平台取 history 里**所有**带 `images` / `gifs` / `videos` 的输出。
- D16 的两条交叉校验在 `graph`（UI 格式）上做：遍历 `nodes[]`，`widgets_values` 中与模型加载节点对应位置的字符串须在 `models[].name` 里；`type` 不是内置节点（`properties.cnr_id` 存在且 ≠ `comfy-core`，或 `properties.aux_id` 存在）时须出现在某个 `nodes[].node_types`。内置节点类型表写在 `comfywf/builtin_nodes.go`（从 ComfyUI v0.3.60 的 `nodes.py` + `comfy_extras` 导出的 class 名列表，常量，几百项）。
- `graph` ≤ 256KB，须是对象且有 `nodes` 数组与 `links` 数组；`api_prompt` ≤ 256KB，须是「`<node_id>: {class_type, inputs}`」形状。
- `download_gb` = Σ `size_gb`（发布时计算写列）。

### 3.3 规则
- `slug` 创建后不可改（容器内文件名）。其余字段可改（审计 `workflow.update`）。删除工作流只允许 `draft` 且无版本；其余 `retired`。
- 版本 `draft` 可改；`published` 只允许改 `changelog` 与 `status → retired`（`workflow_version_frozen`）；draft 保存也要过全部校验（避免「能保存不能发布」，同计划 7）。「复制为新版本」从任一版本复制。
- 发布：校验 → `published`、`published_at`、`manifest_hash`、`download_gb`；审计 `workflow.version.publish`（含 `manifest_hash`）；super + TOTP。**不广播预拉取**（模型在容器内下，不是镜像）。
- 导入：`platform admin workflow import deploy/workflows`，每个子目录 = 一个 slug：`workflow.json`（必填，描述 + 版本信息，字段见 §9 的 README 表）、`graph.json`（UI 格式）、`api.json`（可选）、`manifest.json`、`cover.png|jpg|webp`（可选）；导入为 draft（slug 已存在只加版本），不发布。

### 3.4 兼容与「有新版本」
- 控制台已装工作流视图带 `latest_version_id / update_available`；「更新」= `PUT …/workflows/{wid} {version_id?}`（默认最新 published）。
- 安装 / 更新要求目标部署的应用是 `comfyui`（按 slug）且**本次部署将使用的应用版本** `spec.supports_workflows = true`：部署当前版本不支持 → 自动改用最新 published 且支持的版本（没有 → `workflow_app_unsupported`）。
- 已退役的工作流版本上的安装照常用；「更新」时没有 published 版本 → `workflow_unavailable`。

## 4. 控制台工作流市场（用户视角）

- `/workflows`（导航「工作流市场」，匿名可看、安装需登录）：顶部分类标签（全部 / 文生图 / 图生图 / 视频 / 放大修复 / 其它）与搜索框（前端过滤名称 / 标签）；卡片：封面（无则分类占位）、名称、summary、标签、`需下载约 X GB`、`含 N 个自定义节点`（有则显示）、`支持快速出图` 标记、「一键安装」按钮。
- `/workflows/:slug` 详情：描述（Markdown 子集）、模型表（名称、子目录、大小、来源）、节点表（名称、Git、commit 前 8 位）、快速出图参数表、版本与更新说明、「一键安装」。
- 「一键安装」弹窗（`WorkflowInstallDialog.vue`）：列出当前用户**可用的 ComfyUI 部署**（`GET /instances?with_apps` 已有 `apps_summary`；筛 `app_slug = comfyui` 且 state ∈ running / stopped / failed / queued）每项显示实例名、状态、已装 N/8；选择一个 → 「安装」= `POST /instances/{id}/apps/{dep}/workflows`；没有可用部署或用户选「新开一台实例」→ 打开 `RentDialog`（`mode = immediate`，预选应用 ComfyUI、`MODEL_ID = none`、预选该工作流）。确认文案：「安装会重启实例上的 ComfyUI，正在生成的任务会中断；需下载约 X GB 模型」。
- 实例详情「应用」卡片的 ComfyUI 行下方新增「工作流」子区块（`WorkflowsPanel.vue`）：每个已装工作流一行：名称、版本（`有新版本` 标签）、状态（待安装 / 已安装 / 安装失败）、按钮「打开 ComfyUI」（= 应用的 `url`）、「快速出图」（版本有 `api_prompt` 且部署 running）、「更新」、「移除」；底部「添加工作流」（打开市场页式选择器 `WorkflowPicker.vue`，灰显已装的）；`8/8` 时按钮隐藏并提示上限。部署 installing 时整个子区块显示「正在安装工作流…（随应用进度）」。
- 「快速出图」抽屉（`QuickRunDrawer.vue`）：左侧表单（按 `inputs` 渲染：`textarea / text → el-input`，`int / float → el-input-number`，`bool → switch`，`enum → select`，`seed → 数字框 + 「随机」默认`，`image → 上传框`（调 `POST …/uploads`，显示缩略图）），「运行」按钮；右侧：运行状态（排队中 / 生成中 / 完成 / 失败 + 耗时），完成后输出图网格（`<img>` 指向 `GET …/runs/{run}/files/{n}`，点开大图 / 下载；视频用 `<video controls>`），底部「本次种子：N」与「在 ComfyUI 里打开」。2 秒轮询 `GET …/runs/{run}`，30 分钟停止。错误（`node_errors`、`workflow_busy`、ComfyUI 执行错误）用后端文案。
- 市场页 `/market`「一键部署」区的 ComfyUI 卡片不变；`RentDialog` 选 ComfyUI 应用时新增多选「预装工作流」（最多 8 个，显示合计下载量）→ `app_workflow_ids`；确认页多一行「预装工作流：A、B」。
- 帮助页新增「工作流市场」一节：安装 = 下载模型 + 节点 + 重启 ComfyUI；目录（`/root/data/comfyui/{models,custom_nodes,user}`、`/root/data/models`）；移除不删模型；快速出图输出在 `/root/data/comfyui-output`；想自己装节点可以 SSH 进去放到 `/root/data/comfyui/custom_nodes/`（重新部署不会删它，但平台不负责）。

## 5. 安装机制

### 5.1 部署包的扩展（`internal/appdeploy`，小改）
- `Spec` 新字段 `SupportsWorkflows bool json:"supports_workflows,omitempty"`。
- `BundleInput` 新字段 `Extra []BundleFile{Name string; Mode int64; Body []byte}`：`Name` 相对路径（`^[a-z0-9][a-z0-9/._-]{0,127}$`，每段不为 `..`），由调用方给出；`Bundle` 追加写入 tar。`MaxBundleBytes = 4 << 20`。
- `UploadCommand(dir)` 在 `tar -x` 之前加 `rm -rf <dir>/workflows &&`：集合里被移除的工作流不会残留在包目录（`user/default/workflows/` 里的文件由 `app.sh install` 按 manifest 同步删除，见 §5.2）。
- `run.sh` 的 `install_key()` 追加 `workflows/manifest.json` 的 `cksum`（文件不存在时为空串；与旧的两段式 `.installed` 比较时忽略尾部空白，存量部署不会因升级而重跑 install）：集合变了就重跑 `install`（`install` 幂等：已下载的模型、已 clone 的节点秒过）。`cmd_bg_deploy` 里 install 非零退出时，若 `status.json` 最后一条 `message` 以 `disk:` 开头则沿用该消息（D7），否则仍是「install 失败，退出码 N」。
- `ReservedEnv` 新增 `SPARK_GIT_MIRROR`（来自配置 `SPARK_APP_GIT_MIRROR`，默认 `https://gh-proxy.com/`，允许空 = 直连）；`Deps` 加 `GitMirror string`。
- **扩展点**：`Deps.BundleExtra func(ctx, Deployment, Version) ([]BundleFile, error)`（nil 安全）；`bundleFor` 在 `ver.Spec.SupportsWorkflows` 为真时调用它。`comfywf` 实现该函数（读 `deployment_workflows` + 版本，生成 §5.2 的文件），`app.go` 接线。`appdeploy` **不 import `comfywf`**（避免循环）。
- **状态钩子**：`Deps.OnTransition func(ctx, tx pgx.Tx, d Deployment, to string, now time.Time) error`（nil 安全），Runner 在把部署写成 `running` / `failed` 的回写事务里调用（`stepPoll` 的 `StateRunning` 分支与 `markFailed`）。`comfywf` 用它把 `pending → installed / failed`。

### 5.2 容器内文件与 `app.sh install` 的工作流部分
部署包里（`supports_workflows` 且集合非空时）：
```
workflows/manifest.json      平台生成（规范化、已校验）：
  {"workflows":[{"slug":"sdxl-txt2img","version":"1.0.0","file":"sdxl-txt2img.json",
                 "models":[{"name":"sd_xl_base_1.0.safetensors","dir":"checkpoints","size_gb":6.9,"sha256":"",
                            "modelscope":{"repo":"…","file":"…"},"hf":{"repo":"…","file":"…"}}],
                 "nodes":[{"name":"ComfyUI_UltimateSDUpscale","git":"https://github.com/…","commit":"…"}]}]}
workflows/<slug>.json        每个工作流的 UI 格式 graph（原文）
```
`app.sh install`（1.1.0）的工作流段，全部幂等：
1. `python3 -` 读 manifest，去重后按行输出 `model <name> <dir> <size_gb> <sha256|-> <ms_repo|-> <ms_file|-> <hf_repo|-> <hf_file|->` 与 `node <name> <git> <commit>` 与 `wf <slug> <file>`（字段都已被平台正则限定，不含空白与引号，sh 的 `read -r` 安全）。
2. 模型：目标 `/root/data/comfyui/models/<dir>/<name>` 已存在（软链有效或普通文件）→ 跳过；否则按 `MODEL_SOURCE` 选源（没有该源就用另一个），`df` 预检（D7），`modelscope download --model <repo> <file> --local_dir $SPARK_MODELS_DIR/modelscope/<repo>` 或 `hf download <repo> <file> --local-dir $SPARK_MODELS_DIR/hf/<repo>`，可选 `sha256sum` 校验（不符 → 删文件、失败），`ln -sfn` 到目标。进度：`spark_status download <5～70> "下载模型 <name>（第 i/n 个，约 X GB）"`，每 10 秒按 `du` 更新（沿用 1.0.0 的 `progress_loop`）。
3. 节点：`/root/data/comfyui/custom_nodes/<name>/.spark-commit` 内容 = commit → 跳过；否则 `git clone --filter=blob:none <mirror><git> <tmp>` 失败再直连 → `git -C <tmp> checkout <commit>`（失败 → 失败）→ 有 `requirements.txt` 则 `pip install -r`（失败 → 失败）→ `mv` 到位、写 `.spark-commit`。`spark_status install <70～78> "安装节点 <name>"`。**manifest 里不再有的节点不删**（D8）。
4. 工作流文件：`mkdir -p /root/data/comfyui/user/default/workflows`，把每个 `wf` 复制为 `<slug>.json`；目录里**以 `spark-` 开头**的旧文件不在 manifest 中则删除（平台放进去的文件名统一是 `spark-<slug>.json`，用户自己保存的不动）。
5. `spark_status install 79 "工作流就绪：N 个"`。

`start` 前生成 `$SPARK_APP_DIR/extra_model_paths.yaml`：
```yaml
spark:
  base_path: /root/data/comfyui/
  is_default: true
  checkpoints: models/checkpoints/
  diffusion_models: models/diffusion_models/
  unet: models/unet/
  text_encoders: models/text_encoders/
  clip: models/clip/
  vae: models/vae/
  loras: models/loras/
  upscale_models: models/upscale_models/
  controlnet: models/controlnet/
  clip_vision: models/clip_vision/
  embeddings: models/embeddings/
  custom_nodes: custom_nodes/
```
（`is_default: true` 让 ComfyUI 把保存类节点也默认落到这里；实施时在 v0.3.60 上核实键名，不支持的键删掉。）

### 5.3 平台侧安装流程（`comfywf.Installer`，复用 `appdeploy.Service`）
`Add(ctx, AddRequest{UserID, InstanceID, DeploymentID, WorkflowID, VersionID *int64, IP})`，在一个事务里：
1. `rental.LockOwned` → 锁部署行（`appdeploy.LockDeployment` 导出）→ 部署 `app` 必须是 `comfyui`、state ∈ `queued / running / stopped / failed`（`installing / removing` → `app_invalid_state`）。
2. 工作流 published、版本 = 指定或最新 published（`workflow_not_found / workflow_unavailable`）；集合里已有 → `workflow_exists`；集合 ≥ 8 → `workflow_limit`。
3. 决定应用版本：当前版本 `spec.supports_workflows` 为真则不换；否则取最新 published 且支持的版本（无 → `workflow_app_unsupported`）。
4. `INSERT deployment_workflows (…, 'pending')`；事件 `app_deploy_events`「添加工作流 <名> v<版本>」；审计 `instance.workflow_add`。
5. 部署 state = `queued` 时到此为止（Runner 下一轮生成的部署包就带上它）；否则调 `appdeploy.Service.RedeployInTx(ctx, tx, inst, d, RedeployRequest{VersionID: <第 3 步>, EnvSet: false})`（计划 7 的 `Redeploy` 拆出事务内版本，`Redeploy` 自身改为调它）。
`Update`（换版本）与 `Remove` 同构：`Remove` 删行 + 事件「移除工作流」+ 重新部署；对 `queued` 部署只删行。每用户写操作计入 `api/apps.go` 的 `userLimiter`（同一限流器，60/小时）。
`PrepareInstall` 扩展：`rental.AppHooks.PrepareInstall(ctx, q, appID, imageID, env, workflowIDs []int64)`，`appdeploy` 校验 `workflowIDs` 非空时应用是 comfyui 且版本支持（否则 `workflow_app_unsupported`），并经 `Deps.ResolveWorkflows func(ctx, q, ids) ([]WorkflowRef, error)`（nil 安全；`comfywf` 实现：published、去重、≤ 8、返回 `{WorkflowID, VersionID}`）把结果放进 `installPlan`；`QueueInstall` 插入 `deployment_workflows`。
`OnInstanceMigrated`：复制部署行后 `INSERT … SELECT` 复制集合为 `pending`（在 `appdeploy/hooks.go` 内做，不经 `comfywf`，列是固定的）。

### 5.4 状态同步
- Runner 回写 `running` → `OnTransition(…, StateRunning)`：`UPDATE deployment_workflows SET state='installed', installed_at=$now WHERE deployment_id=$1 AND state='pending'`。回写 `failed` → `pending → failed`。
- 控制台视图：部署 View 新增 `workflows: [{workflow_id, slug, name, category, version_id, version, state, latest_version_id, update_available, quick_run: bool}]`（只对 `comfyui` 部署非空；`appdeploy.Views` 经 `Deps.WorkflowViews func(ctx, depIDs) (map[string][]any, error)` 取，nil 安全）。

## 6. 快速出图（`comfywf.Runner`，无状态代理）

### 6.1 端点（控制台 `/api/v1`，`requireUser`，实例归属校验同 `/instances/{id}/apps*`）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/instances/{id}/apps/{dep}/workflows/{wid}/uploads` | multipart `file`（≤ 16MB，PNG / JPG / WebP，按内容判断）→ 代理 `POST 127.0.0.1:8188/upload/image`（`subfolder = spark-quickrun`）→ `{name: "spark-quickrun/<文件名>"}`。文件名由平台重命名为 `<run 风格随机 12 位>.<ext>`，不透传用户文件名 |
| POST | `/instances/{id}/apps/{dep}/workflows/{wid}/runs` | `{inputs: {key: value}}` → 校验（§3.2 `inputs` 规则；`image` 值须 ~ `^spark-quickrun/[a-z0-9]{12}\.(png\|jpg\|webp)$`）→ 深拷贝 `api_prompt`，逐项 `prompt[node].inputs[input] = value`（`seed` 空 / -1 → 平台 `rand.Int63n(2^53)`）→ `POST /prompt {prompt, client_id: run_id}` → 记 `comfy_workflow_runs` → 201 `{id, prompt_id, seed, state: queued}`。ComfyUI 返回 400（`node_errors`）→ 422 `workflow_prompt_rejected`，消息取第一条 `node_errors` 的 `message`（截 200 字符）。并发 / 频率限制见 D10 |
| GET | `/instances/{id}/apps/{dep}/workflows/{wid}/runs/{run}` | `GET /history/{prompt_id}`：空对象 → `{state: queued}`（再查 `GET /queue`：在 `queue_running` 里 → `state: running`）；`status.completed` → `state: done, files: [{index, filename, kind: image\|video, url}]`（按 `outputs` 过滤，为空取全部）；`status_str = error` → `state: error, message`（`status.messages` 里 `execution_error` 的 `exception_message`，截 500 字符）。同步更新 `comfy_workflow_runs.state / finished_at` |
| GET | `/instances/{id}/apps/{dep}/workflows/{wid}/runs/{run}/files/{index}` | 代理 `GET /view?filename=&subfolder=&type=output`（参数取自 history，**不接受客户端传文件名**）；透传 `Content-Type`、`Content-Length`；≤ 64MB（超出 413）；`Cache-Control: private, max-age=3600`；`Content-Disposition: inline; filename=…` |

- 代理客户端：`http.Client{Transport: &http.Transport{DialContext: Pool.Dial(target, 8188), DisableKeepAlives: true}}`，超时：`/prompt`、`/history`、`/queue`、`/upload/image` 30 秒；`/view` 120 秒。实例非 running / 暂停 → `app_invalid_state`「ComfyUI 未运行」；`DialError` → 新增 `apperr.ErrUpstreamUnreachable`（503 `upstream_unreachable`「无法连接实例内的 ComfyUI」；ingress 目前用自己的 502 页，不共用）。
- `run` 归属：`comfy_workflow_runs.user_id = 当前用户 AND deployment_id = {dep}`，否则 404。
- 所有响应体来自容器内进程，视为不可信：只解析需要的字段、总长 ≤ 1MB（`/view` 除外）。

### 6.2 不做的
- WebSocket 进度、队列位置、取消运行（用户可在 ComfyUI 界面取消）、多张批量、历史列表页（ComfyUI 自带）。

## 7. 暴露与链接
- 不新增端口；「打开 ComfyUI」= 部署的 `url`（计划 7）。工作流安装后在 ComfyUI 前端侧栏「工作流」→「浏览」里显示为 `spark-<slug>.json`；帮助页说明。

## 8. 安全
- **代码执行面**：自定义节点是第三方代码，只能由 super 经 TOTP 发布、Git 主机白名单、钉死 commit、pip 只装节点自带的 requirements；清单各字段都是平台正则限定的 token（无空白、无引号、无 `..`），`app.sh` 以 `read -r` 逐行读，所有变量加引号；`git clone` 的 URL 由平台拼接，用户不可控。
- **用户输入面**：用户能提供的只有 `inputs` 的值（走 JSON 进 `api_prompt`，不进 shell）与上传图片（平台重命名、按内容判类型、限 16MB）；`image` 值白名单正则；`/view` 的参数全部来自 history 不来自客户端。
- **代理白名单**：只有 §6.1 四个端点，路径固定；不是通用反向代理。响应体限长。
- **权限边界不变**：模型 / 节点都在用户容器内以 root 下载安装，与用户自己 SSH 进去做一样；容器能力、网络策略不变。
- **资源**：部署包 ≤ 4MiB；每部署 ≤ 8 工作流；每版本 ≤ 16 模型 / 8 节点；运行并发 2 / 部署、120 次 / 小时 / 用户；上传 16MB、下载 64MB。
- **审计**：`workflow.create / update / delete`、`workflow.version.create / update / publish`、`instance.workflow_add / update / remove`；快速出图运行不审计（只记 runs 表）。

## 9. 首批工作流（D12，`deploy/workflows/<slug>/`）

| slug | 名称 | 分类 | 模型（来源：ModelScope 优先；大小约） | 节点 | 快速出图输入 |
|---|---|---|---|---|---|
| `sdxl-txt2img` | SDXL 文生图 | txt2img | `sd_xl_base_1.0.safetensors` → checkpoints（`AI-ModelScope/stable-diffusion-xl-base-1.0`；hf `stabilityai/…`）6.9GB | — | prompt、negative、width、height、steps、cfg、sampler、seed |
| `sdxl-img2img` | SDXL 图生图 | img2img | 同上 | — | image、prompt、negative、denoise(float 0.2～1)、steps、cfg、seed |
| `flux-schnell-txt2img` | FLUX.1-schnell 文生图（fp8） | txt2img | `flux1-schnell-fp8.safetensors` → checkpoints（`Comfy-Org/flux1-schnell`，ModelScope 与 hf 同名）17GB | — | prompt、width、height、steps(1～8，默认 4)、seed |
| `qwen-image-txt2img` | Qwen-Image 文生图 | txt2img | `qwen_image_fp8_e4m3fn.safetensors` → diffusion_models、`qwen_2.5_vl_7b_fp8_scaled.safetensors` → text_encoders、`qwen_image_vae.safetensors` → vae（`Comfy-Org/Qwen-Image_ComfyUI`，`split_files/…`）约 30GB | — | prompt、negative、width、height、steps、cfg、seed |
| `wan22-5b-ti2v` | Wan2.2 5B 文 / 图生视频 | video | `wan2.2_ti2v_5B_fp16.safetensors` → diffusion_models、`umt5_xxl_fp8_e4m3fn_scaled.safetensors` → text_encoders、`wan2.2_vae.safetensors` → vae（`Comfy-Org/Wan_2.2_ComfyUI_Repackaged`，`split_files/…`）约 17GB | — | prompt、negative、width(默认 1280)、height(704)、length(帧数 int 默认 121)、steps(默认 20)、seed、image(可选) |
| `sdxl-ultimate-upscale` | SDXL 放大修复（2×） | upscale | SDXL base（同上）+ `4x-UltraSharp.pth` → upscale_models（hf `Kim2091/UltraSharp`；ModelScope 镜像实施时核实，没有则只给 hf）约 7GB | `ComfyUI_UltimateSDUpscale`（`https://github.com/ssitu/ComfyUI_UltimateSDUpscale.git`，commit 实施时钉到当前 master；`node_types: [UltimateSDUpscale, UltimateSDUpscaleNoUpscale]`；无 requirements） | image、prompt、denoise(默认 0.3)、upscale_by(float 1～4，默认 2)、seed |

- 仓库 ID 与文件名以计划 7 `comfyui/app.sh` 已验证过的为准（SDXL、FLUX fp8、Qwen-Image）；Wan2.2 与 UltraSharp 的 ModelScope 可用性在实施任务（§18 第 7 项）里在 **Spark 上的实例容器内**用 `modelscope download … --dry-run` 式的列目录核实（不在开发 Mac 下载），不可用的只保留 hf 来源。
- `graph.json` / `api.json` 从 ComfyUI v0.3.60 官方模板（`templates` 里的 `default`、`image2image`、`flux_schnell`、`qwen_image`、`wan2.2_ti2v_5B`）导出并把模型文件名改成清单里的名字；放大修复的图由实施方在 Spark 上的 ComfyUI 里搭好导出。所有导出文件进仓库前过 `go test ./internal/comfywf -run TestWorkflowsDir`（§16.1）。
- `deploy/workflows/README.md`：目录格式、`workflow.json` 字段（`name / summary / description / category / tags / sort / version / changelog / comfyui_min`）、如何从 ComfyUI 导出两种 JSON、如何钉 commit、如何核实模型地址。

## 10. API

错误码（`apperr`）新增：`workflow_not_found`（404「工作流不存在或未上架」）、`workflow_unavailable`（409「该工作流当前没有可用版本」）、`workflow_exists`（409「该工作流已安装在此 ComfyUI 上」）、`workflow_limit`（409「每个 ComfyUI 最多安装 8 个工作流」）、`workflow_app_unsupported`（409「当前 ComfyUI 应用版本不支持工作流市场」）、`workflow_version_invalid`（400，字段路径）、`workflow_version_frozen`（409）、`workflow_busy`（429「ComfyUI 正在处理其它任务，请稍后再试」）、`workflow_prompt_rejected`（422，带 ComfyUI 的节点错误）、`workflow_input_invalid`（400，带 label 与原因）。复用：`app_invalid_state`、`app_not_found`、`rate_limited`、`upstream_unreachable`、`file_too_large`、`unsupported_file_type`。

### 10.1 控制台（`/api/v1`）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/workflows?category=` | 匿名（IP 限流同 `/apps`）：`{items:[{id, slug, name, summary, category, tags, cover_url, sort, version:{id, version, download_gb, models: N, nodes: N, quick_run: bool, comfyui_min}, installs: N}]}`，只含 published 且有 published 版本的；`installs` = `deployment_workflows` 里 `state = installed` 且部署非终态的数量 |
| GET | `/workflows/{slug}` | 多 `description`、`version.manifest`（去掉 `api_prompt` 本体，保留 `inputs / outputs / models / nodes`）、`version.changelog`、`versions:[{id, version, published_at}]` |
| GET | `/workflows/{slug}/cover` | 匿名，图片；无封面 404 |
| POST | `/orders` / `/bookings` | body 新增 `app_workflow_ids?: int64[]` |
| GET | `/instances/{id}/apps` | Deployment 视图新增 `workflows[]`（§5.4） |
| GET | `/instances/{id}/apps/{dep}/workflows` | `{items: [InstalledWorkflow]}` |
| POST | `/instances/{id}/apps/{dep}/workflows` | `{workflow_id, version_id?}` → 201 `{deployment: Deployment, workflows: [...]}` |
| PUT | `/instances/{id}/apps/{dep}/workflows/{wid}` | `{version_id?}` 更新 → 同上 |
| DELETE | `/instances/{id}/apps/{dep}/workflows/{wid}` | 移除 → 同上 |
| POST / GET | §6.1 的 uploads / runs | |

`instanceView.apps_summary` 不变。

### 10.2 后台（`/admin/api/v1`）

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| GET | `/workflows` | all | 全部（含 draft / retired），每项带版本数、`installs_live`、`runs_7d` |
| POST | `/workflows` | super | `{slug, name, summary, description, category, tags, sort}`；审计 `workflow.create` |
| PUT | `/workflows/{id}` | super | 除 slug；审计 `workflow.update` |
| DELETE | `/workflows/{id}` | super | 仅 draft 且无版本 |
| POST | `/workflows/{id}/cover` | super | multipart，D13；审计 `workflow.update`（detail `cover`）|
| GET | `/workflows/{id}/versions` | all | 版本列表（不含 graph 本体） |
| GET | `/workflows/{id}/versions/{vid}` | all | 含 `graph / api_prompt / manifest` |
| POST | `/workflows/{id}/versions[?validate=1]` | super | `{version, graph, api_prompt?, manifest, changelog, copy_from?}` → draft；校验失败 `workflow_version_invalid`；`validate=1` 只校验返回 `{ok, manifest_hash, download_gb, warnings: []}` |
| PUT | `/workflows/{id}/versions/{vid}[?validate=1]` | super | draft 全改；published 只 `changelog / status=retired` |
| POST | `/workflows/{id}/versions/{vid}/publish` | super + TOTP | 审计 `workflow.version.publish` |
| GET | `/deployments/{dep}` | all | 响应新增 `workflows[]`（含 state、版本） |
| GET | `/workflows/{id}/installs?page=` | all | 该工作流的安装列表（部署、实例、用户、版本、状态、时间） |
| GET | `/dashboard` | all | 新增 `workflow_installs`（installed 且部署非终态）、`workflow_runs_24h` |

## 11. 控制台页面
见 §4。文件：`views/WorkflowsView.vue`、`views/WorkflowDetailView.vue`、`components/WorkflowCard.vue`、`WorkflowInstallDialog.vue`、`WorkflowPicker.vue`、`WorkflowsPanel.vue`（嵌入 `AppsCard.vue` 的 comfyui 行）、`QuickRunDrawer.vue`、`QuickRunForm.vue`；`logic/workflows.ts`（纯函数：筛选 / 排序、输入校验与默认值、运行状态文案、下载量格式化）；`RentDialog.vue` 加「预装工作流」；路由 `/workflows`、`/workflows/:slug`；顶栏导航在「算力市场」后加「工作流市场」；类型 `web/shared/src/types/workflows.ts`；`labels.ts` 加分类 / 状态 / 错误码文案与审计动作。

## 12. 后台页面
- 菜单 `工作流 /workflows`（all；放在「应用部署」之后），`menu.spec.ts` 同步。
- `/workflows`：表格（封面缩略、名称、slug、分类、状态、版本数、安装数、7 日运行数、排序），新建 / 编辑弹窗（super，含封面上传）。
- `/workflows/:id`：信息 + 版本表（版本、状态、下载量、模型数、节点数、快速出图、发布时间、安装数）；「新建版本」（可复制）→ `/workflows/:id/versions/:vid`：左侧 `graph.json` / `api.json` 两个文件框（粘贴或选文件，显示大小与节点数）、manifest 用结构化表单（模型表、节点表、输入表、输出表）+「查看 JSON」；「校验」（`?validate=1`，显示错误 / 警告，含 D16 的缺失模型 / 节点清单）；「发布」（TOTP 弹窗）；published 只读。
- `/workflows/:id/installs`：安装列表。
- `/deployments/:dep` 抽屉新增「工作流」表。
- 仪表盘加两个数字。

## 13. 配置与常量

| 项 | 值 | 说明 |
|---|---|---|
| `comfywf.MaxWorkflowsPerDeployment` | 8 | D2 |
| `MaxGraphBytes` / `MaxAPIPromptBytes` | 262144 | D2 |
| `MaxModelsPerVersion` / `MaxNodesPerVersion` / `MaxInputs` | 16 / 8 / 24 | §3.2 |
| `appdeploy.MaxBundleBytes` | 4 << 20（原 262144） | D2 |
| `MaxRunsInFlight` / `RunsPerHour` | 2 / 120 | D10 |
| `MaxUploadBytes` / `MaxViewBytes` | 16 << 20 / 64 << 20 | D10 |
| `ProxyTimeout` / `ViewTimeout` | 30s / 120s | §6.1 |
| `SPARK_APP_GIT_MIRROR` | 环境变量，默认 `https://gh-proxy.com/`，空 = 直连 | 注入 `SPARK_GIT_MIRROR` |
| 封面 | ≤ 2MB，PNG / JPG / WebP | D13 |

## 14. 并发与正确性
- 集合修改（Add / Update / Remove）在 `LockOwned → 锁部署行` 的事务里，与计划 7 用户操作同一锁顺序；Runner 回写在锁部署行后更新 `deployment_workflows`（同一事务），不新增锁对象。
- 部署包按**生成时刻**的集合内容打包；用户在 Runner exec 期间改集合会使 `row_version` 变化 → Runner 丢弃本轮结果 → 下一轮重新打包（计划 7 乐观并发已覆盖）。
- `install_key` 含 manifest 校验和：集合不变的恢复（续费 / 停机）走 `skip install`，不重下、不重 clone。
- 快速出图无共享状态；`runs` 行只追加 + 状态更新；并发上限用 `SELECT count(*) … WHERE deployment_id AND state = 'queued' AND created_at > now − 30m` 判断（软限制，允许极小概率超出）。
- 一致性检查（集成测试断言）：`installed` 的行所属部署当前 state ∈ running / stopped（failed 后仍 installed 是允许的：文件在）；`pending` 的行所属部署 state ∈ queued / installing / failed / stopped；每部署 ≤ 8 行。

## 15. 迁移与兼容
1. 迁移 00024：四张新表，无数据改写。`appdeploy.Spec` 加字段不需迁移（JSONB）。
2. 实施分支从 `main` 切出但**依赖计划 7 的代码**：计划 7 合并后把 `feat/plan11-comfy-workflows` rebase 到 `main`（迁移号已预留，不会冲突）。
3. 发布顺序：无 Agent 改动；platform 先发；再导入 comfyui 1.1.0 与六个工作流并在后台发布。1.0.0 的 ComfyUI 部署照常运行；用户装工作流时自动升级到 1.1.0。
4. `app.sh` 1.1.0 对 1.0.0 已下载的模型（`/root/data/models/...`）直接复用（软链到新目录），不重下；1.0.0 留在 `/opt/ComfyUI/models` 的旧软链无害。
5. 回滚：Down 删表；容器内文件不动；`apps` / `app_versions` 里 1.1.0 仍在（`supports_workflows` 字段被旧代码忽略，`Spec` 解析用非严格 JSON）。
6. e2e 既有用例：`RentDialog` 多出的「预装工作流」只在选 ComfyUI 时出现；部署视图多 `workflows` 字段；`AppsCard` 多子区块（新 `data-testid`），既有 testid 不动。

## 16. 测试与验收

### 16.1 单元（Go，嵌入式 PG，假时钟）
- `comfywf/manifest`：`ValidateManifest` 矩阵（模型名 / 目录 / 大小 / 来源缺失 / repo 与 file 正则 / sha256；节点主机白名单 / commit 长度 / node_types；inputs 的 node / input 存在性、连线值拒绝、类型与区间、enum、seed 现值、image 上限、api_prompt 为空时 inputs 必空；outputs 的 class_type；D16 两条交叉校验用三个 fixture（全对、缺模型、缺节点）；graph / api_prompt 大小与形状）；`manifest_hash` 对键序不敏感；`download_gb` 求和；容器内 manifest 生成（规范化输出快照、同一模型被两个工作流引用只出一次）。
- `comfywf/prompt`：`BuildPrompt(apiPrompt, inputs, values)` 深拷贝不改原值、每种类型的写入、seed 随机 / 指定、未知 key → `workflow_input_invalid`、必填（无 default）缺失、image 值白名单；`ParseHistory` 对 ComfyUI 三种状态的 fixture（排队空对象、完成含 images 与 gifs、error 含 exception_message）与 `outputs` 过滤。
- `comfywf/catalog`：CRUD、slug 不可改、删除条件、draft 校验、发布 TOTP（adminapi 层）、frozen、retired、复制为新版本、`ImportDir`（含 cover、缺 api.json、slug 重复只加版本）、`LatestPublished`。
- `comfywf/installer`：Add / Update / Remove 的允许状态矩阵、`workflow_exists / limit / unavailable / app_unsupported`、1.0.0 部署自动换到 1.1.0（`app_version_id` 变化、`spec_hash` 变化）、queued 部署只改集合不重排、running 部署变 queued、事件与审计、`OnTransition` 的 pending → installed / failed、`OnInstanceMigrated` 复制集合、`PrepareInstall` 带 workflow_ids（非 comfyui 应用 → `workflow_app_unsupported`、> 8 → `workflow_limit`）。
- `appdeploy`：`Bundle` 带 `Extra`（路径校验、4MiB 上限、tar 快照）、`UploadCommand` 含 `rm -rf <dir>/workflows`、`ReservedEnv` 含 `SPARK_GIT_MIRROR`、`ValidateSpec` 的 `supports_workflows`、`bundleFor` 调 `BundleExtra`、`Redeploy` 拆出 `RedeployInTx` 后既有测试全过；`run.sh`：`install_key` 随 `workflows/manifest.json` 变化、文件缺失时等于旧值（既有 `.installed` 不失效）。
- `deploy/apps/comfyui` 1.1.0：`TestAppsDir` 通过；`internal/appdeploy/runtime` 风格的 sh 测试：用假 `modelscope` / `hf` / `git` / `pip` 可执行文件（PATH 前置，记录调用参数）跑 `app.sh install`：manifest 两个工作流共用一个模型只下一次；已存在的模型 / `.spark-commit` 匹配的节点跳过；`MODEL_SOURCE = hf-mirror` 且模型只有 modelscope 来源时用 modelscope；镜像 clone 失败回退直连；checkout 失败 → 非零退出；`df` 不足 → stderr 含 `disk:` 且非零；`spark-` 前缀的多余工作流文件被删、用户文件不动；`extra_model_paths.yaml` 快照。
- `deploy/workflows`：`TestWorkflowsDir`：六个目录文件齐全、`workflow.json` 字段、manifest 过 `ValidateManifest`（含 D16 交叉校验）、`graph.json` 节点数 > 0、`api.json` 与 `inputs` 对得上、封面存在时类型与大小合法。
- `api` / `adminapi`：路由与角色矩阵（`permissions_test.go`）、匿名 `/workflows` 限流、视图字段（`api_prompt` 不外泄）、订单 `app_workflow_ids` 透传、快速出图四个端点对 fake ComfyUI（httptest 经 `sshtest.SetTarget` 指到 8188）的全流程、`workflow_busy`、上传类型 / 大小、`/view` 参数不可控、运行归属 404。
- 迁移 Up / Down 往返。

### 16.2 集成（Go，嵌入式 PG + fake Agent + `sshtest` shell 模式）
- fake comfyui 应用（e2e seed 版本，`spec.supports_workflows = true`）的 `app.sh install` 读 manifest 并把每个 `wf` 文件复制到 `$SPARK_TEST_ROOT/comfyui/user/default/workflows/`、每个模型 `touch` 到 `models/<dir>/<name>`、每个节点 `mkdir` + `.spark-commit`；`start` 写 `service.pid` 并挂起。
- 全链路：下单带 `app_id = comfyui`、`app_workflow_ids = [A]` → 实例 running → Runner → 部署 running → `deployment_workflows[A] = installed`、假容器目录里有 `spark-a.json`；添加 B（有节点）→ 部署回 queued → 包里有两个 json、manifest 两个条目 → running → B installed；更新 A 到新版本 → `version_id` 变、重装；移除 B → 文件消失、节点目录仍在；到期停机 → 续费恢复 → `skip install`（日志）且集合不变；换机 → 新实例集合复制为 pending → running 后 installed；释放 → 部署 removed、集合行保留。
- 失败链路：fake `install` 对某模型名返回 `disk:` → 部署 failed(disk)、集合 `failed`；重新部署成功 → installed。
- 快速出图：fake ComfyUI httptest（`/prompt` 记录收到的 prompt 并返回 prompt_id；`/history` 按脚本返回排队 → 完成；`/view` 返回 PNG 字节；`/upload/image` 返回名字）→ 断言 prompt 里 inputs 被正确写入、seed 随机、files 列表与字节透传、第 3 个并发运行 429。
- 一致性检查（§14）在每个集成测试结束时执行。

### 16.3 端到端（Playwright）
- e2e-server：`/hooks/apps/seed` 的 comfyui fake 版本升级为支持工作流；新钩子 `POST /hooks/workflows/seed` → 两个 fake 工作流（A 无节点、B 有节点、都带 api_prompt 与 inputs、A 有封面）返回 ids；`POST /hooks/comfy/history {run_id, state}` 控制 fake ComfyUI 的 `/history` 返回；fake ComfyUI 作为 e2e-server 内的 handler，经 `/hooks/ingress-upstream` 机制在 8188 上提供。
- 覆盖表新增：`C-WF-01`～`12`（市场页卡片 / 分类筛选 / 详情 / 匿名点安装跳登录 / 安装弹窗选部署 / 无部署时跳 RentDialog 预选 / 安装后应用卡片进度与已装状态 / 8 个上限 / 有新版本与更新 / 移除确认文案 / 快速出图表单校验 / 运行完成显示图片）、`C-MKT-13`（RentDialog 预装工作流与确认页）、`A-WF-01`～`07`（新建 / 编辑 / 封面上传 / 版本结构化表单与校验错误含 D16 / 发布 TOTP / published 只读 / 安装列表与仪表盘数字）、`A-INS-15`（部署详情工作流表）。
- `coverage.json` 登记。

### 16.4 真机验收清单（192.168.1.221，一台机器，全部通过才算完成）
前置：平台跑在 Mac（配置同计划 7 验收，`SPARK_AGENT_BANDWIDTH_MBPS=1000`），Agent 与镜像 `spark/comfyui:25.09` 沿用计划 7 验收的产物（无需重建镜像、无需升级 Agent）；**所有模型 / 节点下载都发生在 Spark 上的实例容器里，开发 Mac 不下载任何模型**；需要 root 的步骤由负责人 sudo。记录写到 `docs/superpowers/acceptance-plan11.md`，格式同计划 7。

1. **导入与发布**：`platform admin app import deploy/apps` 导入 comfyui 1.1.0；`platform admin workflow import deploy/workflows` 导入六个工作流（含封面）；后台逐个发布（TOTP）；市场页 `/workflows` 六张卡片、分类筛选、详情页模型 / 节点表正确；`GET /workflows/sdxl-txt2img/cover` 返回图片。
2. **预装 + 工作流**：市场页 `sdxl-txt2img`「一键安装」→ 无 ComfyUI 部署 → 跳到 `RentDialog`（预选 ComfyUI、`MODEL_ID = none`、预装工作流 sdxl-txt2img）→ 下单 → 实例 running 后部署进度显示「下载模型 sd_xl_base_1.0.safetensors」→ ≤ 15 分钟 running；工作流状态「已安装」；打开 ComfyUI → 侧栏「工作流」里有 `spark-sdxl-txt2img`，加载后点 Queue 出一张 1024×1024 图；容器内 `/root/data/comfyui/models/checkpoints/sd_xl_base_1.0.safetensors` 是指向 `/root/data/models/modelscope/...` 的软链；`/root/data/comfyui/user/default/workflows/spark-sdxl-txt2img.json` 存在；`nvidia-smi` 看到 python 进程。
3. **快速出图**：实例详情 → 工作流行「快速出图」→ 填提示词、种子留空 → 运行 → 2 秒轮询到完成（SDXL 25 步在 GB10 上约 10～20 秒）→ 页面显示图片、种子回显；`/root/data/comfyui-output` 里有该文件；连续点 3 次「运行」第 3 次提示「正在处理其它任务」。
4. **添加带节点的工作流**：添加 `sdxl-ultimate-upscale` → 确认文案含「重启 ComfyUI」→ 部署回「部署中」→ 日志显示 `安装节点 ComfyUI_UltimateSDUpscale`（经 Git 镜像）与下载 `4x-UltraSharp.pth` → running（≤ 10 分钟）→ 容器内 `/root/data/comfyui/custom_nodes/ComfyUI_UltimateSDUpscale/.spark-commit` 内容 = 清单 commit；ComfyUI `GET /object_info` 含 `UltimateSDUpscale`；快速出图上传一张第 3 步的输出图、`upscale_by = 2` → 完成，输出图尺寸 2048×2048。
5. **img2img 与 FLUX**：添加 `sdxl-img2img`（复用 SDXL，日志里模型「已存在，跳过」，≤ 2 分钟 running）；添加 `flux-schnell-txt2img`（17GB，≤ 10 分钟）→ 快速出图 4 步出图。
6. **视频**：添加 `wan22-5b-ti2v`（约 17GB）→ 快速出图（默认 1280×704×121 帧，GB10 预计数分钟）→ 页面 `<video>` 可播放、文件 ≤ 64MB；若超时 30 分钟则在 ComfyUI 界面确认输出存在并记录耗时（不算失败，记入观察）。
7. **上限 / 更新 / 移除**：添加到 8 个（可重复用 fake？不可——真机只有 6 个工作流：改为用后台把 `MaxWorkflowsPerDeployment` 验证留给集成测试，真机只验证「已装 6 个时添加按钮仍可用」）；后台以 `sdxl-txt2img` 1.0.0 为底建 1.0.1（改默认步数 20）并发布 → 控制台该行出现「有新版本」→ 「更新」→ 部署重装（日志 `skip`模型、文件替换）→ 快速出图默认步数变为 20；移除 `sdxl-img2img` → 文件消失、模型仍在、ComfyUI 侧栏少一项。
8. **停机恢复**：`UPDATE instances SET expires_at = now() + interval '1 minute'` → 停机 → 续费 → 实例 running 后 ≤ 3 分钟 ComfyUI running，日志 `skip install`，工作流状态仍「已安装」，快速出图可用。
9. **失败与安全**：后台建一个 draft 工作流版本：模型 `repo` 指向不存在的仓库 → 发布 → 安装 → 部署 `failed(script_failed)`，工作流状态「安装失败」，日志有 modelscope 的报错；移除后重新部署恢复 running。后台校验：节点 `git` 填 `https://evil.example/x/y.git` → `workflow_version_invalid`「主机不在白名单」；commit 填短哈希 → 拒；graph 引用 `foo.safetensors` 但 manifest 没有 → 拒并指出文件名。快速出图：`steps = 999` → 400 `workflow_input_invalid`；`image` 值填 `../../etc/passwd` → 400；上传一个 PDF → 415。`iptables -S SPARK-FWD / SPARK-IN` 未变。
10. **对账无噪声**：全过程无新告警；`ssh_sessions` 无平台产生的会话行；后台仪表盘 `workflow_installs` 与安装列表数字和实际一致；释放实例 → 部署 removed、`deployment_workflows` 行保留、`installs` 计数归零。

## 17. 范围与遗留

**本计划做**：§2～§16 全部；comfyui 应用 1.1.0；六个工作流；`appdeploy` 的扩展点（`Extra`、`BundleExtra`、`OnTransition`、`RedeployInTx`、`supports_workflows`、`SPARK_GIT_MIRROR`、4MiB）；`PrepareInstall` 带 `workflowIDs`。

**上线前核对（写入 `followups-plan11.md`）**：`SPARK_APP_GIT_MIRROR` 在机房出口的可达性（gh-proxy.com 不稳定时换 `https://ghfast.top/` 等）；六个工作流在生产镜像上各跑一次；Wan2.2 在默认限速 200Mbps 下的下载时间（17GB 约 12 分钟）给运营预期；封面图版权（用自己生成的图）。

**明确不做（遗留）**
- 用户上传 / 分享工作流；评分、评论；运营数据之外的推荐。
- ComfyUI-Manager、任意 Git 节点、CivitAI、LoRA 市场。
- 快速出图的 WebSocket 进度、取消、批量、历史页；多张输入图以上的复杂表单；参数预设保存。
- 机器级共享模型缓存 / 预热；按工作流计费。
- 工作流自动更新；安装时不重启 ComfyUI（热加载节点）。
- 后台可视化编辑工作流图（只接受导出的 JSON）。
- ComfyUI 版本升级（镜像里固定 v0.3.60；升级走计划 7 的镜像流程）。

## 18. 任务拆分（供实施计划引用，16 项）
1. 迁移 00024；`comfywf` 包骨架（类型、常量、错误码）；前端类型与文案。
2. `comfywf` 清单校验（`ValidateManifest`、D16 交叉校验、内置节点表）、`manifest_hash`、容器内 manifest 生成、`BuildPrompt` / `ParseHistory`；单测。
3. `appdeploy` 扩展点：`Spec.SupportsWorkflows`、`BundleInput.Extra`、4MiB、`UploadCommand` 清目录、`SPARK_GIT_MIRROR`、`Deps.BundleExtra / OnTransition / WorkflowViews / ResolveWorkflows`、`RedeployInTx`、`LockDeployment` 导出、`run.sh install_key`、`PrepareInstall` 带 `workflowIDs`、`OnInstanceMigrated` 复制集合；单测。
4. `comfywf` 目录服务（CRUD、版本、发布、封面、`ImportDir`、`platform admin workflow import`）；单测。
5. `comfywf` 安装服务（Add / Update / Remove、状态钩子、视图、`ResolveWorkflows`）；单测。
6. `comfywf` 快速出图代理（客户端、上传、运行、查询、文件）；fake ComfyUI 测试服务器；单测。
7. `deploy/apps/comfyui` 1.1.0（spec、app.sh 清单安装、extra_model_paths.yaml）；sh 测试；`TestAppsDir`。
8. `deploy/workflows/*` 六个工作流（在 Spark 的实例容器内核实模型地址、钉节点 commit、导出 graph / api）、README、`TestWorkflowsDir`。
9. 控制台 API（§10.1）+ 订单 `app_workflow_ids`；单测。
10. 后台 API（§10.2）+ 角色矩阵 + 审计；单测。
11. `app.go` 组装、配置、README；e2e 钩子（`/hooks/workflows/seed`、`/hooks/comfy/history`、fake ComfyUI handler、fake comfyui 应用升级）；集成测试（§16.2）。
12. 控制台：工作流市场页、详情页、安装弹窗、`RentDialog` 预装工作流、导航、帮助页。
13. 控制台：实例详情工作流子区块、快速出图抽屉。
14. 后台：工作流列表 / 详情 / 版本编辑页 / 安装列表、部署详情工作流表、仪表盘。
15. Playwright 用例与 `coverage.json`。
16. 真机验收（§16.4）与 `acceptance-plan11.md`、`followups-plan11.md`。

## 附录：自评（设计方，2026-10-24）
- 已核对的代码事实：`appdeploy.Bundle` 的 tar 写法与 `MaxBundleBytes` 常量位置；`UploadCommand` 只含 slug 推导路径（加 `rm -rf <dir>/workflows` 不引入用户输入）；`run.sh install_key` 的组成；Runner 健康检查已用 `Pool.Dial` 做 HTTP（§6 代理可复用）；`Redeploy` 目前是 `userOp` 闭包形态，拆出事务内版本可行；`AppHooks.PrepareInstall` 签名在 `rental/apphooks.go`，`batch.go` 两处调用点；`storage.Save` 只接受 `license` 用途，需要加 `workflow_cover`；comfyui 镜像里有 `git`。
- 自查对照代码发现并修正：`run.sh` 对 install 失败一律写「install 失败，退出码 N」，`app.sh` 的 `disk:` 消息到不了平台 → D7 / §5.1 增加「沿用 `disk:` 消息」的 run.sh 小改；`apperr` 没有 `upstream_unreachable`（ingress 用自己的 502 页）→ §6.1 改为新增；`markFailed` 是全部 failed 写入的唯一汇聚点（4 个调用点）、`bundleError` 已映射 `upload_failed`、`sshtest.ServeHTTP(port, h)` 可承载 fake ComfyUI、`allowedVar` 按 `SPARK_` 前缀放行——这些假定成立。
- 自查修正：原打算工作流 JSON 由容器从平台下载，改为随部署包下发（容器不能访问内网）；原打算把节点装进 `/opt/ComfyUI/custom_nodes`，改为数据盘 + `extra_model_paths.yaml`（容器重建 / 换机才需重装）；原打算用户选「模型来源」按工作流设置，改为沿用部署的 `MODEL_SOURCE`（少一个概念）。
- 待评审重点：§5.1 对 `appdeploy` 的扩展点是否够小；§3.2 的 D16 交叉校验是否会误报（内置节点表的完整性）；§9 Wan2.2 与 UltraSharp 的 ModelScope 可用性只能在真机核实；§16.4 第 7 项在只有 6 个工作流的真机上验证不了 8 的上限（交给集成测试）。
