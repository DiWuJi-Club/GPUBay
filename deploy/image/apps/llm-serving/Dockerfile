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
# 构建时没有 GPU，`vllm --version` 会因推断不出设备类型而报错，这里只校验能导入；运行时用 `vllm --version`（需 --gpus all）
RUN pip install -i ${PIP_INDEX_URL} modelscope jupyterlab \
 && if [ "${VLLM_FROM_PIP}" = "1" ]; then pip install -i ${PIP_INDEX_URL} vllm; fi \
 && python3 -c "import vllm; print('vllm', vllm.__version__)"
# Open WebUI 依赖与 vLLM 的依赖互相冲突，放进独立 venv；app.sh 用 /opt/open-webui/bin/open-webui 启动
RUN python3 -m venv /opt/open-webui \
 && /opt/open-webui/bin/pip install -i ${PIP_INDEX_URL} --upgrade pip \
 && /opt/open-webui/bin/pip install -i ${PIP_INDEX_URL} open-webui \
 && /opt/open-webui/bin/open-webui --help >/dev/null
# NGC 基础镜像自带 ENTRYPOINT /opt/nvidia/nvidia_entrypoint.sh，规范要求应用镜像不设入口，清空
ENTRYPOINT []
CMD []
