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
# NGC 基础镜像自带 ENTRYPOINT /opt/nvidia/nvidia_entrypoint.sh，规范要求应用镜像不设入口，清空
ENTRYPOINT []
CMD []
