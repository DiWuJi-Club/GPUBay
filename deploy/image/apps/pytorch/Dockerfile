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
# NGC 基础镜像自带 ENTRYPOINT /opt/nvidia/nvidia_entrypoint.sh，规范要求应用镜像不设入口，清空
ENTRYPOINT []
CMD []
