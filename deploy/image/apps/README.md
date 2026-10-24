# 应用镜像（一键部署，计划 7）

三个镜像都以 NGC 容器（arm64 + CUDA 13，DGX Spark / GB10 可用）为基础，满足 `docs/image-spec.md` 的规范 v2（`LABEL spark.image.spec="2"`）：
自带 `sshd`（不含主机密钥）、`curl`、`tar`、`setsid`（`util-linux`）、`python3`、`git`、`modelscope` 命令、`jupyterlab`；
`PIP_INDEX_URL` 是构建参数并写进镜像环境；不设 ENTRYPOINT / CMD（NGC 自带的 `nvidia_entrypoint.sh` 在 Dockerfile 末尾清空）。
应用不随容器启动：平台把部署包放进 `/root/data/.spark-apps/<slug>/`，经 `run.sh` 以普通进程启动。

| 镜像 | 基础镜像 | 用途 / 额外内容 |
|---|---|---|
| `spark/llm-serving:<TAG>` | `nvcr.io/nvidia/vllm:<TAG>-py3` | 大模型推理：NGC 自带的 `vllm`；Open WebUI（锁 `0.12.3`，构建参数 `OPEN_WEBUI_VERSION`）装在独立 venv `/opt/open-webui`（依赖与 vLLM 冲突），用 `/opt/open-webui/bin/open-webui` 启动 |
| `spark/comfyui:<TAG>` | `nvcr.io/nvidia/pytorch:<TAG>-py3` | 文生图：ComfyUI 固定 ref 克隆到 `/opt/ComfyUI`（requirements 里的 torch 系过滤掉，用 NGC 的）；不装 ComfyUI-Manager |
| `spark/pytorch:<TAG>` | `nvcr.io/nvidia/pytorch:<TAG>-py3` | 通用 PyTorch 开发环境，替换 images 种子里的 PyTorch 占位条目 |

## 构建参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `BASE` | `nvcr.io/nvidia/vllm:25.09-py3`（llm-serving）/ `nvcr.io/nvidia/pytorch:25.09-py3`（其余） | 基础镜像全名 |
| `PIP_INDEX_URL` | `https://pypi.tuna.tsinghua.edu.cn/simple` | pip 源；同时写进镜像环境 |
| `VLLM_FROM_PIP`（llm-serving） | `0` | 规格 §9 退路：基础镜像不带 vLLM 时（`BASE` 换成 NGC PyTorch）设 `1`，`pip install vllm` |
| `COMFYUI_SRC`（comfyui） | `https://github.com/comfyanonymous/ComfyUI.git` | ComfyUI 仓库；大陆网络见下 |
| `COMFYUI_REF`（comfyui） | `v0.3.60` | ComfyUI tag |

`make app-images`（仓库根目录）按 `APP_IMAGE_TAG`（默认 `25.09`）构建三个镜像；`PIP_INDEX_URL` / `COMFYUI_SRC` 为空时不传，用 Dockerfile 默认值。

## 大陆网络构建

在一台 arm64 机器（DGX Spark）上，Docker Hub 不可达不影响（只用 nvcr.io）。`gitee.com/mirrors/ComfyUI` 不存在（访问要求登录），ComfyUI 走 GitHub 加速代理：

```bash
make app-images PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple \
  COMFYUI_SRC=https://ghfast.top/https://github.com/comfyanonymous/ComfyUI.git
```

或者把本目录同步到构建机后直接构建：

```bash
rsync -a deploy/image/apps/ imac@192.168.1.221:~/spark-build/apps/
cd ~/spark-build/apps
docker build -f llm-serving/Dockerfile --build-arg BASE=nvcr.io/nvidia/vllm:25.09-py3 --build-arg PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple -t spark/llm-serving:25.09 llm-serving
docker build -f comfyui/Dockerfile --build-arg BASE=nvcr.io/nvidia/pytorch:25.09-py3 --build-arg PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple --build-arg COMFYUI_SRC=https://ghfast.top/https://github.com/comfyanonymous/ComfyUI.git -t spark/comfyui:25.09 comfyui
docker build -f pytorch/Dockerfile --build-arg BASE=nvcr.io/nvidia/pytorch:25.09-py3 --build-arg PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple -t spark/pytorch:25.09 pytorch
```

`gh-proxy.com`（`https://gh-proxy.com/https://github.com/comfyanonymous/ComfyUI.git`）同样可用，二选一。

## 验收 / 上线用的实际 NGC 标签

| 镜像 | 实际基础镜像 | 其它 |
|---|---|---|
| `spark/llm-serving:25.09` | `nvcr.io/nvidia/vllm:25.09-py3`（vLLM `0.10.1.1+381074ae.nv25.09`，torch `2.9.0a0+50eac811a6.nv25.09`，CUDA 13.0.1） | Open WebUI `0.12.3`（独立 venv） |
| `spark/comfyui:25.09` | `nvcr.io/nvidia/pytorch:25.09-py3`（torch `2.9.0a0+50eac811a6.nv25.09`，CUDA 13.0.1） | ComfyUI `v0.3.60`（`b8730510db30c8858e1e5d8e126ef19eac395560`） |
| `spark/pytorch:25.09` | `nvcr.io/nvidia/pytorch:25.09-py3` | — |

## 推到 Harbor（上线前核对项）

```bash
for app in llm-serving comfyui pytorch; do
  docker tag spark/$app:25.09 registry.spark.internal/spark/$app:25.09 && docker push registry.spark.internal/spark/$app:25.09
done
```

推送后在后台「价格与镜像」登记 `registry.spark.internal/spark/<app>:25.09`，上架时平台会广播预拉取（llm-serving 约 20GB+，首次预拉取耗时较长）。

## 实际构建记录

2026-10-20，DGX Spark 测试机 `192.168.1.221`（arm64 GB10，驱动 580，Docker 29.6.2），`PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple`，
`COMFYUI_SRC=https://ghfast.top/https://github.com/comfyanonymous/ComfyUI.git`，`COMFYUI_REF=v0.3.60`（upstream `git ls-remote --tags` 核实存在，提交 `b8730510db30`）。

| 镜像 | 镜像 ID | 大小（`docker images`） | 构建耗时（不含拉基础镜像） | 主要内容 |
|---|---|---|---|---|
| `spark/llm-serving:25.09` | `3f3a4d0068bf` | 28.9GB | 2206s（约 37 分钟，几乎全在 Open WebUI venv：PyPI 的 arm64 CUDA torch 2.14.1 + nvidia-* 轮子，约 1–3MB/s） | vLLM `0.10.1.1+381074ae.nv25.9.cu130`、Open WebUI `0.12.3`（venv 7.0G）、modelscope `1.40.2`、JupyterLab `4.4.7`（NGC 自带） |
| `spark/comfyui:25.09` | `fb3ac0fa75ba` | 18.7GB | 147s | ComfyUI `v0.3.60`、modelscope `1.40.2`、JupyterLab `4.4.7` |
| `spark/pytorch:25.09` | `07fedfaef0ed` | 18.2GB | 37s | modelscope `1.40.2`、JupyterLab `4.4.7` |

基础镜像拉取（两个并行）：`vllm:25.09-py3`（21.6GB）约 8.5 分钟，`pytorch:25.09-py3`（18.1GB）约 7.5 分钟。
`make app-images`（同样的参数）在缓存命中时 1 秒内产出同样的三个镜像 ID。

验证（`--gpus all`）：三个镜像都是 `True NVIDIA GB10`、`tools-ok`、`sshd-ok`、无 `/etc/ssh/ssh_host_*`、`entry=[] cmd=[]`、`spark.image.spec=2`；
`vllm --version`、`/opt/open-webui/bin/open-webui --help`、`python3 /opt/ComfyUI/main.py --help`、`jupyter lab --version` 都能执行。

注意：
- `vllm --version` 需要 GPU（没有 `--gpus all` 时报 `Failed to infer device type`），所以构建期只校验 `import vllm`。
- NGC PyTorch 25.09 不带 `torchaudio`，ComfyUI 启动时提示 `torchaudio missing, ACE model will be broken`（只影响 ACE 音频模型）。
- `gitee.com/mirrors/ComfyUI` 不存在（要求登录），大陆用 `ghfast.top` / `gh-proxy.com` 代理 GitHub。
