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

ComfyUI 不下仓库里的 diffusers 分片格式（ComfyUI 读不了，体积也翻倍），只下它能直接加载的单文件权重，并软链进 `/opt/ComfyUI/models/<子目录>`（`start` 时重做软链，容器重建后也在）。
