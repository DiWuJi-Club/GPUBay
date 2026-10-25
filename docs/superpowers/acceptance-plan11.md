# 计划 11 真机验收记录（192.168.1.221）

## 概况

- 日期：2026-10-25 01:32～04:27（时间均为 HKT / Asia/Shanghai）。
- 机器：DGX Spark `spark-3479`（GB10），后台「B 区」B-01（machine 1），与计划 2/3/6/7 验收是同一台。Agent `9f0e8f0`（计划 7 复测的产物，含 `image.env` 修复），`connected = true`，验收期间没有重装 Agent。
- 镜像：`spark/comfyui:25.09`（计划 7 Task 2 在 221 上构建，本地 tag），没有重建。镜像里有 `PIP_CONSTRAINT=/etc/pip/constraint.txt`、`/usr/local/bin/hf`、`/usr/local/bin/modelscope`。
- 平台：开发 Mac 192.168.1.218 上的 `.devdata/platform-p11`（验收工作树 `spark-cloud-p11-acc`，分支 `p11-acc`）。
  - 01:33 用 `c0f4a21`（集成分支 `feat/plan11-comfy-workflows` 在合入 `main` / 00021 之前的提交）构建：先 `make web`，再 `go build ./cmd/platform`，前端已嵌入。
  - 03:09 用 `f7f2cef` 重新构建并重启，即 `c0f4a21` + O-1 的修复（只改控制台前端）。
  - 04:12 第 8～10 项改用新实例，换成 `5c97796`：合入集成分支 HEAD `10dbe9b`（含计划 8 的 00021、goose `WithAllowOutofOrder`、快速出图与 app.sh 的评审修复）。
    - 启动时 00021 在 00024 之后补进 `spark_p11`，没有报错。
    - 集成分支 HEAD 的 comfyui `app.sh` 比 `c0f4a21` 多了「旧位置同名模型软链复用」和 Git 路径正则对齐。1.1.0 已在库里，同版本号不能再导入，所以以验收专用版本号 **`1.1.0-head`** 导入（目录拷到 scratchpad，只改 `app.json` 的 version / changelog）并发布，验收后已下线。第 8～10 项部署的是这份脚本。
  - 环境变量与计划 7 的平台（`platform-p7`，pid 94607）相同：`SPARK_ENV=dev`、`SPARK_DOMAIN=192-168-1-218.sslip.io`、`SPARK_AGENT_BANDWIDTH_MBPS=1000`、`SPARK_AGENT_DATA_SIZE_GIB=200`、`SPARK_SWEEP_INTERVAL=1m`、`SPARK_APP_PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple` 等，`SPARK_SECRET_KEY` 与上游 SSH 私钥也相同。只改了两个：
    - `SPARK_DATABASE_URL` 指向 `spark_p11`；
    - `SPARK_UPLOAD_DIR` 指向工作树的 `.devdata/uploads`。
  - `SPARK_APP_GIT_MIRROR` 未设置，取默认值 `https://gh-proxy.com/`。
- 数据库：先停 `platform-p7`（01:33:03 SIGTERM），再用 `CREATE DATABASE spark_p11 TEMPLATE spark` 复制开发库（:5433），`platform-p11` 启动时迁移到 **00024**。
  - 共享的 `spark` 库没有动，迁移版本仍是 20。计划 8～10 的 00021～00023 还要落到 `spark` 上，goose 当时不允许乱序，所以 00024 不能先落进去。
- 控制台测试用户：13800000221。
  - 余额 ¥448.00，下单（1 天 ¥69）后为 ¥379.00。
  - 登录走真实的 `POST /auth/sms-code` + `/auth/login`。开发环境的 mock 短信只存在内存里，所以把 `sms_codes` 最新一行的 `code_hash` 换成了已知验证码的哈希。
- 后台：账号 `acceptor`（super），用真实登录表单加动态码登录。
- 页面操作：Playwright 驱动真实 Chrome，用 `--host-resolver-rules` 把 `*.sslip.io` 指到 Mac。脚本与截图都在验收会话的 scratchpad 里（`…/scratchpad/p11acc/pw/`、`…/scratchpad/p11acc/shots/`，下文简写为 `shots/`），不入库。
- 模型：所有模型、自定义节点、pip 都在 Spark 上的实例容器里下载，开发 Mac 没有下载任何模型或大文件。验收用到的图片（第 3 项生成的 PNG）是从容器里取回的输出文件。
- 机器占用：与计划 9/10 共用这台机器，用 `spark.lock` 互斥。
  - 本计划持锁两段：01:32:17～03:08:38、04:11:57～04:26:53。
  - 中间计划 10 要用整机，按负责人安排于 03:22:19 释放了第一台实例（第 10 项的释放检查就是在这次完成的），03:22:38 停掉 `platform-p11`。等 `p11-spark-free` 标记被删、再拿到锁后，才重启平台、开新实例做第 8～10 项。
- 实例：
  - 第 2～7 项共用 `ins-o3xpibdh`（ComfyUI，1 天），部署 `dep-ketaw2yrx8s1`，最后装了 6 个工作流、约 78GB 模型。
  - 第 8～10 项用 `ins-9l2dpnzv`（ComfyUI，1 天，接口下单，预装 SDXL 文生图 1.0.1 + 图生图），部署 `dep-xwxypt7l5cf0`。
  - 两台都已释放。
- 余额：¥448.00 → ¥241.00（两次下单 + 一次续费，各 ¥69），没有充值。
- 结论：10 项中 **9 项通过、1 项部分通过**（第 5 项：FLUX 17GB 下载用了 13 分钟，超过「≤ 10 分钟」，原因是 ModelScope 的实际速度约 22MB/s，功能正常）。
  - 六个工作流都在真机 ComfyUI 上跑通：五个出图、一个出视频，`api.json` 全部被 ComfyUI 接受，没有 `node_errors`。
  - 没有发现需要阻塞合并的缺陷。记录 1 个低严重度缺陷 W-1（未修，列入遗留），以及 14 条观察。其中 O-1 已修并复测。

## 1. 导入与发布（§16.4.1）——通过

- **导入应用**（01:33:29）：`platform admin app import deploy/apps`。
  - 输出 `comfyui 1.1.0 version-added`。
  - 接着 `jupyterlab: duplicate: 版本号已存在`，命令以 rc=1 结束。导入器遇到第一个错误就停，open-webui / vllm 没有再尝试，见观察 O-7。
- **导入工作流**（01:33:29）：`platform admin workflow import deploy/workflows`。
  - 六个工作流都输出 `1.0.0 created`，rc=0。
  - 封面写进 `SPARK_UPLOAD_DIR`（与服务同一目录）。后台列表 `has_cover = true` ×6。
  - 审计：`app.version.create` ×1，`workflow.create` + `workflow.version.create` ×6，actor 均为 system。
- **发布**（后台页面，01:34～01:37）：
  - comfyui 1.1.0：版本页「发布」，显示「校验通过」，「确认发布」后输入动态码。审计 `app.version.publish`（01:35:00）。截图：`shots/01-comfyui-publish-dialog.png`。
  - 六个工作流 1.0.0：在版本页 `/workflows/:id/versions/:vid` 点「发布」，发布对话框都显示「校验通过」，下载量分别为 17.2 / 30.1 / 6.94 / 6.94 / 7.01 / 18.1 GB。
    - 放大修复额外提示「含 1 个自定义节点：节点代码会在用户容器内以 root 执行，发布前请确认已审查钉住的 commit」。
    - 审计 `workflow.version.publish` ×6，带 `manifest_hash` 与 `download_gb`。
    - 随后在详情页「编辑」把工作流状态改为「已发布」（`workflow.update` ×6）。
    - 截图：`shots/01-wf*-publish-dialog.png`、`01-admin-workflows.png`。
  - 动态码弹窗只在第一次高风险操作时出现（qwen 那次也弹过）。其余几次在 5 分钟的二次验证窗口内，没有再要动态码，符合 `admin.StepUp` 的设计（观察 O-9）。
- **市场页**（匿名访问 `/workflows`）：
  - 六张卡片，都有封面图、名称、简介、标签、「支持快速出图」和「需下载约 X GB」。只有放大修复显示「含 1 个自定义节点」。截图：`shots/01-market.png`。
  - 分类筛选结果：文生图 3（SDXL、FLUX、Qwen）、图生图 1、视频 1、放大修复 1、其它 0、全部 6。截图：`01-market-video.png`。
  - 详情页 `/workflows/sdxl-ultimate-upscale`：
    - 模型表：`sd_xl_base_1.0.safetensors → models/checkpoints 6.94 GB`、`RealESRGAN_x4plus.safetensors → models/upscale_models 0.07 GB`，来源同时列 ModelScope 和 HuggingFace 仓库。
    - 节点表：`ComfyUI_UltimateSDUpscale`、Git 地址、commit `a5547db9`。
    - 快速出图参数表（输入图片 / 提示词 / 重绘强度 0.05～1 默认 0.3 / 放大倍数 1～4 默认 2 / 种子留空随机）、更新说明、版本 `v1.0.0 · 2026-10-25 01:37（当前）`。
    - 截图：`shots/01-detail-upscale.png`。
  - Wan2.2 详情页的三个模型（diffusion_models 10 GB、text_encoders 6.74 GB、vae 1.41 GB）正确。截图：`01-detail-wan.png`。
- **接口**：
  - `GET /api/v1/workflows/sdxl-txt2img/cover` → 200 `image/png`，512×288，带 `Cache-Control: public, max-age=86400`、`ETag`、`X-Content-Type-Options: nosniff`。
  - `GET /api/v1/workflows` 六项，`installs` 都是 0；`?category=video` 只返回 `wan22-5b-ti2v`。

## 2. 预装 + 工作流（§16.4.2）——通过

- **下单**（01:39～01:40，已登录）：
  - 市场页 `sdxl-txt2img`「一键安装」打开安装弹窗，内容为「您的实例上还没有可用的 ComfyUI。可以新开一台实例，开通时预装 ComfyUI 与该工作流。」，以及确认文案「安装「SDXL 文生图」会重启实例上的 ComfyUI，正在生成的任务会中断；需下载约 6.94 GB 模型」。截图：`shots/02-install-dialog-empty.png`。
  - 点「新开一台实例」打开 `RentDialog`，预选情况：
    - 预装应用 ComfyUI 图像生成，镜像锁定为 ComfyUI；
    - 模型 `none`，下载源 modelscope；
    - 「预装工作流」里 SDXL 文生图已勾选，显示「已选 1/8 · 合计需下载约 6.94 GB」。
    - 截图：`02-rent-dialog.png`。
  - 确认页显示「预装工作流 SDXL 文生图（合计需下载约 6.94 GB）」，价格 ¥69.00。截图：`02-confirm.png`。
    - 当时确认页的「预装应用」显示为「ComfyUI 图像生成（none）」，见 O-1，已修。
  - 01:40:01 下单，跳转到 `/instances/ins-o3xpibdh?highlight=workflows`。实例 1 秒内 running，部署 `dep-ketaw2yrx8s1` 进入 queued。
- **部署进度**（`app_deploy_events` 与控制台轮询）：

  | 时间 | 状态 |
  |---|---|
  | 01:40:01 | queued「预装：ComfyUI 图像生成 1.1.0，实例开通后自动部署」「预装工作流 1 个」 |
  | 01:40:04 | installing（第 1 次） |
  | 01:40:23～01:44:54 | download 6%→62%「下载模型 sd_xl_base_1.0.safetensors（1/1，约 6.94GB）：nGB」 |
  | 01:45:04 | 70%「校验 sha256：sd_xl_base_1.0.safetensors」 |
  | 01:45:09 | **running**（「部署完成」），工作流 `sdxl-txt2img = installed` |

  - 从下单到可用 **5 分 08 秒**（要求 ≤ 15 分钟）。6.94GB 约 4.7 分钟下完，约 25MB/s。
  - 实例详情的「工作流」子区块：安装中显示「正在安装工作流…（随应用进度）」「SDXL 文生图 待安装」，完成后显示「已安装」。截图：`shots/02-detail-installing.png`、`02-detail-running.png`。
- **打开 ComfyUI**（经 ingress `ins-o3xpibdh-8188.<域>:8080`，带登录 Cookie）：
  - 侧栏「工作流 (w)」→「浏览」里只有 `spark-sdxl-txt2img.json`。截图：`shots/02-comfy-sidebar.png`。
  - 点它加载后，节点为 `CheckpointLoaderSimple[sd_xl_base_1.0.safetensors]`、`EmptyLatentImage[1024,1024,1]`、`KSampler[…,25,7,euler,normal,1]` 等。
  - 点「运行」，01:48:05 入队，01:48:27 `success`，输出 `ComfyUI_00001_.png`，**1024×1024**。截图：`02-comfy-loaded.png`、`02-comfy-image.png`。
- **容器内**（`docker exec ins-o3xpibdh …`）：
  - `/root/data/comfyui/models/checkpoints/sd_xl_base_1.0.safetensors -> /root/data/models/modelscope/AI-ModelScope/stable-diffusion-xl-base-1.0/sd_xl_base_1.0.safetensors`。
  - `/root/data/comfyui/user/default/workflows/spark-sdxl-txt2img.json` 存在（2681 字节）。它是平台规范化后的紧凑 JSON，解析后与仓库 `deploy/workflows/sdxl-txt2img/graph.json` 完全相等。
  - 部署包：`workflows/manifest.json`（带 sha256）+ `workflows/sdxl-txt2img.json`。`env` 里有 `SPARK_GIT_MIRROR='https://gh-proxy.com/'`、`MODEL_ID='none'`、`MODEL_SOURCE='modelscope'`，文件权限 `-rw-------`，带 `image.env`。
  - `extra_model_paths.yaml` 与规格 §5.2 一致：`base_path: '/root/data/comfyui/'`、`is_default: true`，13 个模型目录（`diffusion_models` / `text_encoders` 分别并入旧名 `unet` / `clip`），加上 `custom_nodes`。
  - 进程：`python3 main.py --listen 127.0.0.1 --port 8188 --extra-model-paths-config /root/data/.spark-apps/comfyui/extra_model_paths.yaml --user-directory /root/data/comfyui/user --input-directory /root/data/comfyui/input --output-directory /root/data/comfyui-output`。
  - `nvidia-smi` 有 `python3`，占用 7225MiB。

## 3. 快速出图（§16.4.3）——通过（并发上限经接口 + 页面验证，见 O-4）

- 实例详情 SDXL 文生图行点「快速出图」打开抽屉。表单默认值与 `api.json` 一致：提示词 / 反向提示词、宽高 1024（512～2048，步长 64）、步数 25、CFG 7、采样器 euler、种子「留空或 -1 = 随机」。
- 填提示词「a red fox sitting in a snowy forest at sunrise, photorealistic, highly detailed」，种子留空，点「运行」：

  | 时间 | 状态 |
  |---|---|
  | 01:50:06 | 点击 |
  | +1s | 排队中 |
  | +3s | 生成中 |
  | +11s | **完成（11 秒）** |

  - 页面显示 1024×1024 的图，「本次种子：2842278292826823」（平台生成）。截图：`shots/03-qr-result.png`。
  - `/root/data/comfyui-output/ComfyUI_00002_.png` 存在（1.7MB）。
- **文件代理响应头**（第 4 项的输出）：`Content-Type: image/png`、`Cache-Control: private, max-age=3600`、`Content-Disposition: inline; filename=ComfyUI_00005_.png`、`Content-Security-Policy: default-src 'none'; sandbox`、`Cross-Origin-Resource-Policy: same-origin`、`X-Content-Type-Options: nosniff`。
- **并发上限**（01:50:27）：
  - 连续三次 `POST …/workflows/4/runs`：前两次 201 queued（种子 6442974201833766、3952792740577698），第三次 **429 `workflow_busy`「ComfyUI 正在处理其它任务，请稍后再试」**。
  - 紧接着在页面抽屉点「运行」，抽屉里显示同一条错误（`qr-error`）。截图：`shots/03-busy-result.png`。
  - 两个接口运行随后都 `done`。
  - 清单写的「连续点 3 次『运行』」在同一个抽屉里做不到：运行中按钮是 loading，不能再点（观察 O-4）。所以前两个运行改用接口发起。

## 4. 添加带节点的工作流（§16.4.4）——通过

- 01:51:04 点「添加工作流」打开选择器。SDXL 文生图灰显（已装），选 SDXL 放大修复（2×）。
  - 确认文案「安装「SDXL 放大修复（2×）」会重启实例上的 ComfyUI，正在生成的任务会中断；需下载约 7.01 GB 模型」。下载量没有扣除已有的 SDXL，见 O-3。
  - 确认后子区块变为 2/8，新行「待安装」。截图：`shots/04-add-dialog.png`、`04-after-add.png`。
- 时间线：

  | 时间 | 状态 |
  |---|---|
  | 01:51:05 | queued，审计 `instance.workflow_add` + `instance.app_redeploy` |
  | 01:51:13 | deploy #2 |
  | 01:51:16 | 下载 `RealESRGAN_x4plus.safetensors`（modelscope `Comfy-Org/Real-ESRGAN_repackaged`），校验 sha256 |
  | 01:51:26 | 「安装节点 ComfyUI_UltimateSDUpscale（1/1）」 |
  | 01:51:36 | 健康检查 |
  | 01:51:46 | **running**（41 秒，要求 ≤ 10 分钟） |

  - 日志第一行是「模型 sd_xl_base_1.0.safetensors 已存在，跳过」。
  - 子模块的日志：`Submodule 'repositories/ultimate_sd_upscale' (https://github.com/Coyote-A/ultimate-upscale-for-automatic1111) registered`、`checked out '2322caa480535b1011a1f9c18126d85ea444f146'`。
- **容器内**：
  - `custom_nodes/ComfyUI_UltimateSDUpscale/.spark-commit` = `a5547db9e1d07d3318bb21e9e9c474f4c1e9c8df`，与清单 commit 相同。`git rev-parse HEAD` 也是它。
  - `origin` 为 `https://gh-proxy.com/https://github.com/ssitu/ComfyUI_UltimateSDUpscale.git`，说明是经 Git 镜像 clone 的。
  - `git submodule status`：` 2322caa… repositories/ultimate_sd_upscale`，子模块已取到（目录里有 `scripts/`）。
  - `models/upscale_models/RealESRGAN_x4plus.safetensors` 软链到 `/root/data/models/modelscope/Comfy-Org/Real-ESRGAN_repackaged/…`。
  - `curl 127.0.0.1:8188/object_info`：共 478 个节点，含 `UltimateSDUpscale`、`UltimateSDUpscaleNoUpscale`、`UltimateSDUpscaleCustomSample`、`UltimateSDUpscaleGuider`。`UpscaleModelLoader.model_name` 的选项为 `['RealESRGAN_x4plus.safetensors']`。
  - comfyui.log：`Import times for custom nodes: 0.0 seconds: /root/data/comfyui/custom_nodes/ComfyUI_UltimateSDUpscale`，没有导入失败。
- **快速出图**（02:02:42）：
  - 上传第 3 项的输出图（1024×1024 PNG，从容器 `/root/data/comfyui-output` 取回），显示「已上传」。
  - 提示词「a red fox in a snowy forest, highly detailed fur」，denoise 0.3、放大倍数 2（默认）。
  - 运行 → 完成（56 秒），输出 **2048×2048**（`ComfyUI_00005_.png`，5.4MB），种子 2820514013487771。截图：`shots/04-qr-result.png`。
- 补充：第 7 项结束后，在 ComfyUI 界面从侧栏加载 `spark-sdxl-ultimate-upscale.json`，直接点「运行」（LoadImage 用默认的 `example.png`），输出 1536×1536。这说明 `start` 把镜像的 `example.png` 拷进数据盘输入目录这一步生效了，README 里「待真机核实」的那条成立。

## 5. img2img 与 FLUX（§16.4.5）——部分通过（FLUX 下载 13 分钟，超过 10 分钟的预期，环境带宽所致，见 O-8）

- **SDXL 图生图**：02:03:51 添加，02:03:58 deploy #3，**02:04:09 running（18 秒，要求 ≤ 2 分钟）**。
  - 日志：「模型 sd_xl_base_1.0.safetensors 已存在，跳过」「模型 RealESRGAN_x4plus.safetensors 已存在，跳过」「节点 ComfyUI_UltimateSDUpscale 已是 a5547db9…，跳过」「工作流就绪：3 个」。
  - 快速出图：上传第 3 项的输出图，提示词「… oil painting style」，denoise 0.75，完成（23 秒），输出 1024×1024。截图：`shots/05-img2img-result.png`。
- **FLUX.1-schnell**：02:04:49 添加，02:04:59 deploy #4，02:05:09 开始下载 `flux1-schnell-fp8.safetensors`（modelscope `Comfy-Org/flux1-schnell`），02:17:3x 校验 sha256，**02:17:52 running（13 分钟）**。
  - 17.24GB 约 12.5 分钟下完，约 22MB/s。
  - 下载期间进度条一直在 5%～26% 之间，见 O-2。
  - 快速出图：默认 1024×1024、4 步，提示词「a cozy wooden cabin by a mountain lake at dusk …」，完成（13 秒，含加载模型），输出 1024×1024，画面正常。截图：`shots/05-flux-result.png`。
- 判定：功能全部正常。「≤ 10 分钟」没有达到，原因是 ModelScope 到机房的实际速度约 22MB/s（与计划 7 验收 B 的 21MB/s 相同），不是平台问题。按此速度估算的各工作流下载时间写进了遗留。

## 6. 视频（§16.4.6）——通过

- 02:18:30 添加 Wan2.2 5B，02:18:3x deploy #5，依次下载三个文件（modelscope `Comfy-Org/Wan_2.2_ComfyUI_Repackaged`，`split_files/…`）：
  - `wan2.2_ti2v_5B_fp16`（10.0GB）
  - `umt5_xxl_fp8_e4m3fn_scaled`（6.74GB）
  - `wan2.2_vae`（1.41GB）
  - **02:30:56 running（12 分 25 秒，18.15GB）**。
- 软链：`models/diffusion_models/wan2.2_ti2v_5B_fp16.safetensors`、`models/text_encoders/umt5_xxl_fp8_e4m3fn_scaled.safetensors`、`models/vae/wan2.2_vae.safetensors`，都指向 `/root/data/models/modelscope/Comfy-Org/Wan_2.2_ComfyUI_Repackaged/split_files/…`。
- **快速出图**：用默认值（1280×704、121 帧、20 步，反向提示词为模板的中文默认），提示词「a red fox running through a snowy forest, cinematic tracking shot, golden hour light」：

  | 时间 | 状态 |
  |---|---|
  | 02:31:10 | 排队中 |
  | 02:31:12 | 生成中 |
  | 02:39:59 | **完成（8 分 50 秒）** |

  - 生成期间 `nvidia-smi` 的 GPU-Util 为 96%，`python3` 占用 8795MiB。
  - 页面 `<video controls>`：`videoWidth × videoHeight = 1280 × 704`，`duration = 5.04 s`，`error = null`，能播放（画面是雪林里奔跑的狐狸）。截图：`shots/06-wan-result.png`。
  - 文件代理：`Content-Type: video/mp4`，`Content-Length: 1615518`（1.6MB，远小于 64MB 上限）。容器内对应 `/root/data/comfyui-output/video/ComfyUI_00001_.mp4`。
- 快速出图只做文生视频（裁定 D19），表单里没有图片输入。

## 7. 上限 / 更新 / 移除（§16.4.7）——通过

- **第 6 个工作流**：清单只要求「已装 6 个时添加按钮仍可用」，所以加装了 Qwen-Image，同时也在真机上验证了它的图。
  - 02:40:47 添加，下载 `qwen_image_fp8_e4m3fn`（20.43GB）、`qwen_2.5_vl_7b_fp8_scaled`（9.38GB）、`qwen_image_vae`（0.25GB），**03:01:55 running（21 分钟，30.06GB）**。
  - 快速出图：默认 1328×1328，提示词「一家名为「星火咖啡」的街角咖啡店招牌，木质门面，暖色灯光，写实摄影」，完成（2 分 53 秒）。输出 1328×1328，招牌上「星火咖啡」四个字正确。截图：`shots/07-qwen-result.png`。
  - 六个都装上后，子区块显示 **6/8**，「添加工作流」按钮可见且可用，没有上限提示（`wf-limit` 不存在）。选择器里六个工作流全部灰显。截图：`shots/07-update-panel.png`。8 个上限由集成测试覆盖。
- **新版本与更新**：
  - 02:40:58 后台以 1.0.0 为底建 `sdxl-txt2img` **1.0.1**：`copy_from` 后把 graph 的 KSampler 步数、`api.json` 的 `steps`、`manifest.inputs[steps].default` 都从 25 改为 20，更新说明「默认步数 25 → 20（验收第 7 项）」。
  - 在版本页「发布」：「校验通过」，输入动态码，02:41:30 审计 `workflow.version.publish`。截图：`shots/07-publish-dialog.png`。
  - 控制台接口 `sdxl-txt2img`：`latest_version 1.0.1`、`update_available true`。页面该行显示「有新版本 v1.0.1」（`shots/07-update-panel.png`）。
  - 03:05:23「更多 → 更新」，确认框「将「SDXL 文生图」从 v1.0.0 更新到 v1.0.1，会重启实例上的 ComfyUI，正在生成的任务会中断」，并附 v1.0.1 的更新说明。确认后行状态变为「v1.0.1 待安装」。
  - 03:05:29 deploy #7，日志里 9 个模型全部「已存在，跳过」，节点「已是 a5547db9…，跳过」，「工作流就绪：6 个」，**03:05:36 running（10 秒）**。
  - 六个 `spark-*.json` 都被重写（mtime 19:05:29 UTC）。`spark-sdxl-txt2img.json` 里 KSampler 的 `widgets_values` 变为 `[…,"randomize",20,…]`。
  - 快速出图的默认步数为 **20**（`shots/07-qr-defaults.png`）。用默认值运行完成（25 秒），ComfyUI `history` 里该 prompt 的 KSampler `steps = 20`。
- **移除**：03:06:38「更多 → 移除」SDXL 图生图。
  - 确认框「移除「SDXL 图生图」后模型与节点保留在 /root/data/comfyui（可手动删除释放空间）；会重启实例上的 ComfyUI，正在生成的任务会中断」。截图：`shots/07-remove-confirm.png`。
  - 确认后子区块变为 5/8。03:06:48 deploy #8，日志「移除工作流文件 spark-sdxl-img2img.json」「工作流就绪：5 个」，**03:07:06 running**。
  - 容器内 `user/default/workflows/` 剩 5 个文件。`models/checkpoints/sd_xl_base_1.0.safetensors` 软链仍在，模型缓存没有删。
  - ComfyUI 的 `/api/userdata?dir=workflows`（侧栏「浏览」的数据源）只剩 5 项，没有 `spark-sdxl-img2img.json`。截图：`shots/07-comfy-sidebar.png`。

### 第 8～10 项的新实例

- 04:12 重启平台：`5c97796`，comfyui 换成 `1.1.0-head`，见概况。
  - Agent 04:12:39 重新连上。
  - 平台停了约 50 分钟，启动时产生一条 `machine_offline` 告警（id 22），属于环境因素，见对账。
- 04:13 后台发布 `1.1.0-head`：真实登录、动态码，「校验通过」。截图：`shots/08-publish-head.png`。
- 04:14:02 经接口下单：`POST /orders`，`image_id 6`、`app_id 1`、`app_env {MODEL_ID: none, MODEL_SOURCE: modelscope}`、`app_workflow_ids [4, 3]`。页面下单路径已在第 2 项验证过。
  - 得到实例 `ins-9l2dpnzv`、部署 `dep-xwxypt7l5cf0`（版本 `1.1.0-head`），工作流为 `sdxl-txt2img 1.0.1`、`sdxl-img2img 1.0.0`。
- 04:14:06 开始部署，下载 SDXL（6.94GB），**04:19:21 running（5 分 19 秒）**，两个工作流 installed。
- 04:19:45 快速出图 SDXL 文生图：默认 20 步，19 秒出 1024×1024。

## 8. 停机恢复（§16.4.8）——通过

- 04:20:05 执行 `UPDATE instances SET expires_at = now() + interval '1 minute' WHERE id='ins-9l2dpnzv'`。约 04:21:15 实例 stopped，容器 `Exited (0)`。
  - 应用行与工作流子区块仍显示两个工作流「已安装」，提示「实例运行中才能连接」。截图：`shots/08-stopped.png`。
  - 接口返回部署 `running / unknown`，按 §6.3 state 不变；两个工作流仍是 `installed`。
- 04:21:38 `POST /instances/ins-9l2dpnzv/renew {"duration":"1d"}`，返回 200，订单 `renew` ¥69。时间线：

  | 时间 | 状态 |
  |---|---|
  | 04:21:39 | 部署 queued「实例已启动，正在恢复应用」 |
  | 04:21:41 | installing（第 2 次） |
  | 04:21:56 | **running**（续费后 18 秒，要求 ≤ 3 分钟） |

- 部署日志：`==== deploy #2 ====`，`skip install: spec_hash f0906ea2…, env and workflows unchanged`，接着 `comfyui started`。没有重新下载，也没有重新安装。
  - `.installed` 为三段式：`<spec_hash> <cksum(env)> <cksum(manifest)>`。
- 工作流仍是「已安装」，`installed_at` 不变（04:19:21）。
- 快速出图可用：04:22:12 运行，20 秒完成，1024×1024。截图：`shots/08-qr-after-result.png`。

## 9. 失败与安全（§16.4.9）——通过

- **装不上的工作流**：
  - 后台建工作流 `acc-bad-model`，版本 1.0.0 的图与 `api.json` 取自 SDXL 文生图，checkpoint 改名为 `acc_missing_model.safetensors`。清单里该模型只给 modelscope 来源，仓库 `spark-acc/does-not-exist-p11`。
  - 04:22:47 在版本页发布：「校验通过」，需下载 1 GB，输入动态码。再把工作流设为「已发布」。
- 04:23:05 在实例详情「添加工作流」选它，确认后 04:23:08 部署 queued，deploy #3。
  - **04:23:18 部署 `failed`，错误码 `script_failed`**，message「install 失败，退出码 1」。
  - 工作流行显示 **「安装失败」**，以及「安装失败，可「重试安装」或在应用「查看日志」里看原因」。截图：`shots/09-failed.png`。
  - 库里 `deployment_workflows` 的 `acc-bad-model = failed`，另外两个仍 `installed`。
  - 部署日志里有 modelscope 的报错：

    ```
    下载模型 acc_missing_model.safetensors：modelscope spark-acc/does-not-exist-p11 acc_missing_model.safetensors
    error: [E3020] [404] 获取模型文件失败，文件内容为空 … code=10990101007
      Request: GET https://modelscope.cn/api/v1/models/spark-acc/does-not-exist-p11/repo?Revision=master&FilePath=acc_missing_model.safetensors
    模型 acc_missing_model.safetensors 下载失败（来源 modelscope：spark-acc/does-not-exist-p11 acc_missing_model.safetensors）
    ```

- **移除后恢复**：04:23:41「更多 → 移除」，确认后 deploy #4（日志「模型 sd_xl_base_1.0.safetensors 已存在，跳过」「工作流就绪：2 个」），**04:23:58 running**。`deployment_workflows` 里该行已删除。
- **后台校验**：以放大修复 1.0.0 为底，调 `POST /workflows/5/versions?validate=1`：

  | 改动 | 结果 |
  |---|---|
  | 原样（基线） | `ok: true`，附「含 1 个自定义节点…」提示 |
  | `nodes[0].git = https://evil.example/x/y.git` | 400 `workflow_version_invalid`「manifest.nodes[0].git：Git 主机只能是 github.com / gitee.com / gitcode.com」 |
  | `nodes[0].commit = a5547db`（短哈希） | 400「manifest.nodes[0].commit：必须是 40 位十六进制 commit」 |
  | graph 的 `CheckpointLoaderSimple` 改为 `foo.safetensors`，manifest 不变 | 400「graph.nodes[0]：节点 #4 CheckpointLoaderSimple 引用的模型 foo.safetensors 不在 manifest.models 里」 |

- **快速出图输入**（`ins-9l2dpnzv` 上的 SDXL 文生图 4 / 图生图 3）：

  | 请求 | 结果 |
  |---|---|
  | `steps = 999` | **400** `workflow_input_invalid`「步数：应在 1～100」 |
  | `sampler = "$(id)"` | 400「采样器：不是可选值」 |
  | 未声明的 `ckpt = foo.safetensors` | 400「ckpt：未知参数」 |
  | `seed = -5` | 400「种子：种子应为 -1（随机）或 0～9007199254740991 的整数」 |
  | 图生图 `image = "../../etc/passwd"` | **400**「输入图片：请先上传图片」 |
  | 图生图 `image = "spark-quickrun/../../x.png"` | 400（同上） |
  | 图生图不带 `image` | 400「输入图片：必填」 |
  | 上传 PDF（multipart 里声明 `image/png`、文件名 `x.png`） | **415** `unsupported_file_type`「仅支持 PNG、JPG、WebP 图片」（按内容判断） |
  | 上传 17MB 的 PNG（真实 PNG 头 + 填充） | 413 `file_too_large`「图片不能超过 16MB」 |
  | 上传 PNG，文件名 `../../evil name.png` | 201 `{"name":"spark-quickrun/s3bg3mjv3sr4.png"}`（平台重命名，不透传文件名） |
  | 用另一个部署的运行 id 查 `…/runs/run-qib5du11j32h` | 404 |
  | `…/files/9`（不存在的序号） | 404 |
  | `…/runs/run-2ja3qdv5re9v/files/0?filename=../../etc/passwd&type=input&subfolder=../..` | 200 `image/png`，`Content-Disposition: inline; filename=ComfyUI_00003_.png`（1024×1024 输出图），客户端参数被忽略 |

- **iptables**（Spark 上 sudo 执行）：`iptables -S SPARK-FWD` 与 `iptables -S SPARK-IN` 与计划 7 验收一致，没有变化。
  - SPARK-FWD：conntrack ESTABLISHED,RELATED ACCEPT；`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`、`100.64.0.0/10`、`169.254.0.0/16`、`224.0.0.0/4` 各一条 DROP；末尾 ACCEPT。
  - SPARK-IN：conntrack ACCEPT 加 DROP。

## 10. 对账无噪声（§16.4.10）——通过

- **告警**：01:00 之后共 2 条，都与计划 11 无关，都已处理。第 1～10 项的操作本身没有产生任何告警（第 9 项的部署失败按设计不告警）。

  | id | 类型 | 来源 | 处理 |
  |---|---|---|---|
  | 21 | `machine_offline` | 01:17:10，计划 10 的 PoC 期间 Agent 失联，复制开发库时带进了 `spark_p11` | 已处理 |
  | 22 | `machine_offline` | 04:12:16，`platform-p11` 为计划 10 让机停了约 50 分钟，重启时判定 Agent 失联超过 5 分钟（同计划 7 的告警 18，环境因素） | 已处理 |

- **`ssh_sessions`**：`ins-o3xpibdh` 与 `ins-9l2dpnzv` 都是 **0 行**。期间平台 exec 有数百次（部署、轮询、快速出图代理），没有产生会话行；验收没有手工 SSH 进实例，容器检查都用 `docker exec`。
- **安装数**（释放前）：
  - 第一台：后台仪表盘 `workflow_installs = 5`，与 `deployment_workflows` 中 installed 且部署非终态的行数一致；工作流列表 `installs_live` 为 5 个 1、图生图 0（已移除），控制台 `/workflows` 的 `installs` 相同。
  - 第二台：仪表盘 `workflow_installs = 2`。
    - 后台工作流列表：文生图、图生图各 1，其余 0。
    - 「安装列表」`/workflows/4/installs`：共 2 行，`dep-xwxypt7l5cf0` running + `dep-ketaw2yrx8s1` removed，后者是历史。
    - 部署抽屉 `/deployments?dep=dep-xwxypt7l5cf0` 的「工作流」表有两行：SDXL 图生图 1.0.0、SDXL 文生图 1.0.1，均为「已安装」、支持快速出图。
    - `workflow_runs_24h = 12`，等于 `comfy_workflow_runs` 24 小时内的行数。
    - 截图：`shots/10-before-dashboard.png`、`10-before-dep-drawer.png`、`10-before-installs.png`、`10-before-workflows.png`。
- **释放**：
  - 第一台 `ins-o3xpibdh`：03:22:19 释放，8 秒内 `released`。
    - 机器 1 `idle`，容器删除（`docker ps -a` 为空）。
    - 部署 `removed`（`instance_gone`，事件「实例已释放」）。
    - `deployment_workflows` 的 5 行保留（installed）。
    - 仪表盘 `workflow_installs` 5 → **0**，控制台各工作流 `installs` 全部为 0。
  - 第二台 `ins-9l2dpnzv`：04:26:01 释放，5 秒内 `released`。
    - 部署 `removed`，2 行保留，`instance_ports` 0 行。
    - 仪表盘与后台 `installs_live`、控制台 `installs` 全部为 0。
    - 机器 1 `idle`，容器删除，GPU 上没有进程。
- **收尾**（`spark_p11` 库内）：验收专用的 `acc-bad-model` 工作流改为 `retired`，comfyui `1.1.0-head` 改为 `retired`。comfyui 1.0.0 / 1.1.0 与六个工作流（sdxl-txt2img 为 1.0.0 + 1.0.1）保持已发布。

## 缺陷

| 编号 | 现象 | 复现 | 原因 | 严重度 | 修复 |
|---|---|---|---|---|---|
| W-1 | ComfyUI（comfyui 1.1.0）每次启动的 `comfyui.log` 里都有 `Failed to initialize database … (sqlite3.OperationalError) unable to open database file`。 | 任意 1.1.0 部署启动后看 `/root/data/.spark-apps/comfyui/comfyui.log`。 | ComfyUI v0.3.60 的 `--database-url` 默认值是 `sqlite:///<ComfyUI 程序目录>/user/comfyui.db`，不随 `--user-directory` 变化。1.1.0 把用户目录换到数据盘后，镜像里没有 `/opt/ComfyUI/user`，SQLite 打不开文件。v0.3.60 只记日志、照常运行，验收中没有看到功能影响；ComfyUI 提示「in future the database will be required」。 | 低 | **未修**（列入遗留）。改 `app.sh` 会变 spec_hash，而且需要真机复测，验收为缩短占机时间没有做。建议在 `start` 的启动参数里加 `--database-url "sqlite:///$CDATA/user/comfyui.db"`，并在 sh 测试里核对启动参数。 |

除 W-1 外，验收中没有发现功能缺陷；O-1 是界面文案问题，已修。

## 观察（列入遗留）

- **O-1**（已修 `f7f2cef`，复测通过）：预装应用为 comfyui 1.1.0（`MODEL_ID` 默认 `none`）时，显示的是原始取值 `none`：
  - `RentDialog` 的预装应用下拉显示「ComfyUI 图像生成 · none」；
  - 确认页显示「预装应用：ComfyUI 图像生成（none）」。

  修法：`appOptionLabel` / `appConfirmText` 把 `none` 当作没有预置模型，加了单测。03:09 重建后复测，下拉与确认页都只显示「ComfyUI 图像生成」（`shots/02b-rent-dialog.png`、`02b-confirm-after-o1.png`，只看不下单）。
- **O-2**：工作流模型的下载进度不直观。
  - 进度条按清单里的模型**个数**平均分段，已存在的模型也占一段。例如 FLUX 是 3 个模型中唯一要下的，整整 12 分钟进度条都在 5%～26% 之间，然后直接跳到健康检查。
  - 消息里的「：nGB」是整个仓库目录的 `du`，并且向下取整到 GB。Wan 的 `umt5`（6.74GB）显示「：12GB」，因为同一仓库目录里已经有 10GB 的 diffusion 模型；SDXL 前 1GB 一直显示「：0GB」。
  - 建议在下一个 comfyui 版本里按 `size_gb` 加权、只计要下载的模型，消息改成「已下载 x.x / y.y GB」（用本文件的 `du` 减去起始值）。改 `app.sh` 要发新版本，验收没有改。
- **O-3**：安装确认文案与 `RentDialog` 的「需下载约 X GB」是工作流的全部模型，没有扣除这个 ComfyUI 上已经有的。例如放大修复显示 7.01GB，实际只下了 0.07GB；图生图显示 6.94GB，实际 0。可以按部署上已装工作流的模型名去重后再算。
- **O-4**：清单第 3 项写「连续点 3 次『运行』，第 3 次提示正在处理其它任务」，但抽屉在运行中把「运行」按钮置为 loading，同一个抽屉里点不出并发。验收改为两次接口运行加一次页面运行，页面显示了后端的 `workflow_busy` 文案。建议规格按这个写法修订。
- **O-5**：ComfyUI 重启（任何一次安装、更新、移除都会重启）后，`/history` 是空的：
  - 重启前的快速出图记录在控制台再取文件会 404（第 4 项取第 3 项的输出时遇到），文件仍在 `/root/data/comfyui-output`。
  - 没人轮询过的 queued 运行会被判为「任务已丢失（ComfyUI 可能已重启）」（error）。第 3 项并发测试的第一个接口运行（`run-0hhxbklo9ab8`）实际已经出图，但最后记为 error，所以后台统计里多了 1 个失败。
  - 这符合设计（无状态代理，D10）。可以在抽屉里提示「重新部署后之前的结果请到 /root/data/comfyui-output 查看」。
- **O-6**：`comfyui.log` 里有两次 `ModuleNotFoundError: No module named 'torchaudio'`。原因是 `spark/comfyui:25.09` 没装 torchaudio，ComfyUI 的音频类内置节点不可用。首批工作流不受影响；镜像下次重建时考虑装上与 torch 匹配的 torchaudio（计划 7 的镜像）。
- **O-7**：`platform admin app import deploy/apps` 遇到第一个错误就停止。本次 comfyui 在字母序第一个，导入成功；jupyterlab 重复，后面的 open-webui / vllm 没有再尝试。README「其它应用同版本号重复导入会报错、可忽略」没说清楚这一点。若以后某个排在后面的应用升了版本，会被前面的重复挡住。建议导入器像工作流导入那样逐个继续，或在 README 里写明「只拷要升级的应用目录再导入」。
- **O-8**：下载速度。验收环境 ModelScope 约 22～25MB/s，用时：SDXL 6.94GB 约 4.7 分钟、FLUX 17.24GB 约 12.5 分钟、Wan 18.15GB 约 12.4 分钟、Qwen 30.06GB 约 21 分钟。规格「FLUX ≤ 10 分钟」在这种速度下做不到（第 5 项部分通过的原因）。生成耗时（GB10）：
  - SDXL 25 步约 11 秒（换模型后首次约 20 秒）；
  - SDXL 放大 2× 约 56 秒；
  - FLUX 4 步约 13 秒；
  - Qwen-Image 1328² 约 2 分 53 秒；
  - Wan2.2 1280×704×121 帧约 8 分 50 秒。
- **O-9**：后台发布用动态码做二次验证，验证后有 5 分钟窗口（`admin.StepUp`），窗口内连续发布不再要码。本次 6 个工作流里只有两次弹了动态码框。符合设计；运营批量发布时会觉得「有时要码、有时不要」，帮助文案可以说明。
- **O-10**：后台页面在 `/admin/api/v1/dashboard` 请求途中关闭（Playwright 关浏览器），平台记了一条 ERROR `internal error … context canceled`。客户端断开不是服务端错误，可以降级为 debug 或忽略 `context.Canceled`。
- **O-11**：部署失败时的 message 只有「install 失败，退出码 1」。具体原因（「模型 … 下载失败（来源 modelscope：…）」、modelscope 的 404）只在日志里。控制台已提示「在应用『查看日志』里看原因」。可以让 `run.sh` 在 install 失败时取 stderr 最后一行作为 message（类似 D7 的 `disk:` 沿用）。
- **O-12**：机器与平台共享。
  - 本计划与计划 9 / 10 交替用这台机器。为计划 10 让机时释放了第一台实例（约 78GB 模型随数据盘清除），第 8～10 项在新实例上重下 SDXL（5 分钟）。
  - 告警 21、22 见第 10 项，不是本计划的问题。
- **O-13**：一次更新（第 7 项）会重写 `user/default/workflows/` 里**全部** `spark-*.json`（六个文件的 mtime 相同）。用户若直接修改并保存了平台放进去的文件，重新部署会被覆盖（帮助页已提示另存为其它名字）。已列入遗留。
- **O-14**：没有实测 `MODEL_SOURCE = hf-mirror`。全程用默认的 modelscope；计划 7 的 O-12 提到 hf-mirror 对 xet 仓库不可用。

## 未做的可选项

- 「在 ComfyUI 界面里加载每个 `spark-*.json`，比较『导出 (API)』与仓库 `api.json`」没有逐个做，原因是负责人要求缩短占机时间。替代证据：
  - 六个 `api.json` 都经快速出图被真机 ComfyUI 接受并成功执行（没有 `node_errors`），参数默认值与 `api.json` 现值一致（第 3～7 项的表单）；
  - `spark-sdxl-txt2img.json` 与 `spark-sdxl-ultimate-upscale.json` 在 ComfyUI 界面里从侧栏加载后，直接点「运行」都成功；
  - 容器内的 `spark-sdxl-txt2img.json` 解析后与仓库 `graph.json` 相同。

## 收尾状态

- **实例**：两台验收实例都已释放（`ins-o3xpibdh` 03:22:19，`ins-9l2dpnzv` 04:26:01）。机器 1 `idle`，没有容器、没有 GPU 进程；Agent `9f0e8f0` 在线，二进制 SHA-256 不变（`13bc98a7…1ef0`）。
- **平台**：04:26:28 停掉 `platform-p11`。04:26:36 按原样重启计划 7 的平台：
  - 二进制 `.devdata/platform-p7`，cwd 为主检出目录 `spark-cloud`，环境与停之前的进程相同（18 个 `SPARK_*`，取自 `ps eww`）；
  - 日志重定向到新文件 `.devdata/platform-p7-restored-20261025.log`；
  - 监听 :8080 / 127.0.0.1:8081 / :2222 / :9443，库为 `spark`（迁移版本仍是 20）。
- **机器锁**：04:26:53 释放 `spark.lock`。
- **数据库**：`spark_p11` 保留（迁移到 00024，并补了 00021），供需要时查看验收数据；可以 `DROP DATABASE spark_p11` 清理。
- **写入前核对**：本记录与提交里没有任何密码、sudo 口令、TOTP 密钥、`SPARK_SECRET_KEY` 或会话令牌的值。
