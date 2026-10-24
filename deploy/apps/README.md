# 应用目录（一键部署，计划 7）

每个子目录是一个应用，目录名就是 slug（`^[a-z][a-z0-9-]{1,31}$`），里面三份文件：

| 文件 | 内容 |
|---|---|
| `app.json` | 应用信息 + 本次导入的版本号、兼容镜像、更新说明（字段见下表） |
| `spec.json` | 版本 spec：端口、健康检查路径、超时、数据盘最小剩余空间、用户变量、链接（规则见设计文档 §3.2，`appdeploy.ValidateSpec`） |
| `app.sh` | 版本脚本，POSIX sh，≤ 64KB（规则见下） |

`app.json` 字段：

| 字段 | 说明 |
|---|---|
| `name` | 应用名称，1～32 个字符 |
| `summary` | 一句话简介，≤ 80 个字符 |
| `description` | 详细说明，≤ 4000 个字符，可用换行与 `- ` 列表 |
| `category` | `llm` / `image` / `notebook` / `tool` |
| `icon` | `app` / `llm` / `chat` / `image` / `notebook` / `tool` / `code` / `audio` |
| `sort` | 市场页排序，小的在前 |
| `version` | 版本号（如 `1.0.0`）；同一应用重复导入同一版本号会报错，改脚本请升版本 |
| `images` | 兼容镜像的**名称**（`images.name`），导入时按名称找已登记的镜像 |
| `changelog` | 本版本的更新说明 |

镜像名称约定（后台「价格与镜像」登记时用这三个名字）：

| 名称 | 镜像 | 用到它的应用 |
|---|---|---|
| `LLM Serving` | `spark/llm-serving:25.09` | vllm、open-webui、jupyterlab |
| `ComfyUI` | `spark/comfyui:25.09` | comfyui、jupyterlab |
| `PyTorch` | `spark/pytorch:25.09` | jupyterlab |

## 导入

```bash
platform admin app import deploy/apps
```

每个目录导入为 draft 应用（slug 不存在时新建）+ draft 版本，输出 `slug version created|version-added`；导入不发布，到后台逐个发布。
引用的镜像必须先在后台登记，否则该应用导入失败（之前导入的保留）。

## `app.sh` 约定

由平台内置的 `run.sh`（`internal/appdeploy/runtime/run.sh`）在容器里以 `sh app.sh <子命令>` 调用，工作目录是 `$SPARK_APP_DIR`：

- `install`：下载模型等一次性准备，必须幂等（spec 或变量变了会重跑；已下载的文件要能跳过）。用 `spark_status <phase> <progress> <message>` 汇报进度（phase 一般是 `download` / `install`，progress 0～100）。
- `start`：把服务放后台启动、把服务进程 PID 写进 `$SPARK_APP_DIR/service.pid`，然后自己退出。服务只监听 `127.0.0.1:$SPARK_PORT`（平台经容器内回环访问）。
- `stop`（可选）：额外的清理；服务进程与整个进程组由 `run.sh` 负责杀掉。
- 镜像环境：镜像的 Docker `ENV`（`CUDA_HOME`、`LD_LIBRARY_PATH`、`PATH` 等）由平台写进 `image.env` 并先于 `env` 加载，脚本里不必再手工 export（需要 Agent 新版 init.sh；读不到时平台日志有 warn）。
- 变量：`env` 里的平台保留变量（`SPARK_APP_DIR`、`SPARK_DATA_DIR`、`SPARK_PORT`、`SPARK_MODELS_DIR`、`HF_ENDPOINT`、`MODELSCOPE_CACHE` 等）与 spec 里声明的用户变量都已导出；未填的可选变量不会出现，引用时写 `${NAME:-}`。
- 只用 POSIX sh（Ubuntu 的 `/bin/sh` 是 dash），不要写 bash 语法。

`spec.json` 的 `supports_workflows: true`（目前只有 comfyui 1.1.0 起）表示该版本能装工作流市场的工作流：平台在部署包里多放
`workflows/manifest.json` 与 `workflows/<slug>.json`（见下文「ComfyUI 与工作流市场」），工作流集合变了 `run.sh` 会重跑 `install`。
其它应用不写这个字段。

`go test ./internal/appdeploy/ -run TestAppsDir` 校验本目录：三份文件齐全、`app.json` 字段与镜像名、`spec.json` 过 `ValidateSpec`、`app.sh` 过平台同一个 POSIX 语法检查、`start` 分支写 `service.pid`、脚本里引用的大写变量都在 spec 里声明或是保留变量 / 脚本内赋值的变量。

**注意：脚本会在用户容器里以 root 执行。** 不要在脚本里拼接未加引号的用户变量，不要下载执行来路不明的脚本；密钥类变量用 `secret` 类型（平台会在日志里脱敏）。

## 模型

模型缓存在数据盘 `/root/data/models`（`$SPARK_MODELS_DIR`），卸载应用不删除：ModelScope 下到 `models/modelscope/<仓库>`，hf-mirror 下到 `models/hf/<仓库>`。

| 应用 | 选项 | 实际下载 | 大小 |
|---|---|---|---|
| vllm | `Qwen/Qwen3-4B` / `Qwen3-8B` / `Qwen2.5-7B-Instruct` / `Qwen3-14B` | 整个仓库 | 8 / 16 / 15 / 29GB |
| comfyui | `AI-ModelScope/stable-diffusion-xl-base-1.0` | `sd_xl_base_1.0.safetensors` → `checkpoints` | 7GB |
| comfyui | `AI-ModelScope/FLUX.1-schnell`（ModelScope） | `flux1-schnell.safetensors` → `unet`、`ae.safetensors` → `vae`，`AI-ModelScope/flux_text_encoders` 的 `clip_l` + `t5xxl_fp16` → `clip` | 34GB |
| comfyui | `AI-ModelScope/FLUX.1-schnell`（hf-mirror） | `Comfy-Org/flux1-schnell` 的 fp8 单文件 checkpoint（HF 官方仓库需登录同意协议） | 17GB |
| comfyui | `Qwen/Qwen-Image` | `Comfy-Org/Qwen-Image_ComfyUI` 的 fp8 模型 → `unet`、`qwen_2.5_vl_7b_fp8_scaled` → `clip`、VAE → `vae` | 30GB |

comfyui 的 `MODEL_ID` 从 1.1.0 起默认 `none`（不下预置模型，只装工作流清单里的模型）。
ComfyUI 不下仓库里的 diffusers 分片格式（ComfyUI 读不了，体积也翻倍），只下它能直接加载的单文件权重，并软链进数据盘的
`/root/data/comfyui/models/<子目录>`（1.0.0 软链在镜像层的 `/opt/ComfyUI/models`，容器重建后要靠 `start` 重做）。

## ComfyUI 与工作流市场（comfyui 1.1.0，计划 11）

数据盘目录（卸载应用、移除工作流都不删）：

| 路径 | 内容 |
|---|---|
| `/root/data/comfyui/models/<dir>/<name>` | 模型软链（指向 `/root/data/models/{modelscope,hf}/<仓库>/<文件>`）；`<dir>` ∈ `checkpoints diffusion_models unet text_encoders clip vae loras upscale_models controlnet clip_vision embeddings style_models vae_approx` |
| `/root/data/comfyui/custom_nodes/<name>` | 自定义节点（`.spark-commit` 记录装的 commit） |
| `/root/data/comfyui/user` | ComfyUI 用户目录（`--user-directory`）；平台放的工作流是 `user/default/workflows/spark-<slug>.json`；从 1.0.0 升级后第一次启动把镜像层的 `/opt/ComfyUI/user` 拷过来一次 |
| `/root/data/comfyui/input` | 输入图片（`--input-directory`，首次启动拷入镜像自带的 `example.png`） |
| `/root/data/comfyui-output` | 生成结果（`--output-directory`） |

`start` 每次生成 `$SPARK_APP_DIR/extra_model_paths.yaml`（`base_path: /root/data/comfyui/`、`is_default: true`，上表每个模型目录一项；
`unet` / `clip` 是 ComfyUI 的旧名，分别并进 `diffusion_models` / `text_encoders` 的多行路径；另有 `custom_nodes`）。

部署包里的 `workflows/manifest.json` 由平台生成（`comfywf.BundleFiles`，已校验、按 slug 排序）：

```json
{"workflows":[{"slug":"sdxl-txt2img","version":"1.0.0","file":"sdxl-txt2img.json",
  "models":[{"name":"sd_xl_base_1.0.safetensors","dir":"checkpoints","size_gb":6.94,"sha256":"",
             "modelscope":{"repo":"…","file":"…"},"hf":{"repo":"…","file":"…"}}],
  "nodes":[{"name":"ComfyUI_UltimateSDUpscale","git":"https://github.com/…","commit":"<40 位>"}]}]}
```

`app.sh install` 用镜像的 `python3 -I` 读它，按平台规则再校验一遍（不合规整份拒绝），展开成按空白分隔的行（sh 用 `read -r` 读）：

```
model <name> <dir> <size_gb> <sha256|-> <ms_repo|-> <ms_file|-> <hf_repo|-> <hf_file|->   # 按目标 <dir>/<name> 去重
node <name> <git> <commit>                                                                # 按 name 去重
wf <slug> <file>
```

然后（全部幂等）：

1. 模型：目标已存在（有效软链或普通文件）→ 跳过；否则按 `MODEL_SOURCE` 选来源（清单里没有该来源就用另一个），下载前 `df` 预检
   （剩余 < 模型大小 + 5GiB → `spark_status download <pct> "disk: …"` 后退出 2，`run.sh` 沿用该消息，平台记错误码 `disk`），
   `modelscope download` / `hf download` 到 `/root/data/models/{modelscope,hf}/<仓库>`，`sha256` 非空时校验（不符删文件、失败），再 `ln -sfn`。进度 5～70。
2. 节点：`.spark-commit` 与清单 commit 相同 → 跳过；否则 `git clone --filter=blob:none` 到 `custom_nodes/.tmp-<name>`（GitHub 地址先经
   `SPARK_GIT_MIRROR`，失败再直连）→ `checkout <commit>` → 有 `.gitmodules` 时 `submodule update --init --recursive`（有镜像时先带
   `url.<镜像>https://github.com/.insteadOf=https://github.com/`，失败再直连）→ 有 `requirements.txt` 时 `pip install -r` → 替换旧目录、写 `.spark-commit`。
   任一步失败 → install 失败。清单里不再有的节点不删。进度 70～78。
3. 工作流文件：复制为 `user/default/workflows/spark-<slug>.json`，目录里不在清单中的 `spark-*.json` 删除（用户自己存的文件不动）；
   包里没有 manifest 时同样清理。最后 `spark_status install 79 "工作流就绪：N 个"`。

`go test ./internal/appdeploy/ -run TestComfyApp` 用假的 `modelscope / hf / git / pip / python3` 真跑 comfyui 的 `install` / `start`。
