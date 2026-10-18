# 参考平台镜像：CUDA 基础镜像 + sshd。
# Agent 以 /.spark/init.sh 覆盖入口，这里不设 ENTRYPOINT / CMD；不要在镜像里放密钥或口令。
FROM nvcr.io/nvidia/cuda:13.0.1-base-ubuntu24.04
LABEL spark.image.spec="1"
RUN apt-get update \
 && apt-get install -y --no-install-recommends openssh-server ca-certificates curl iproute2 \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /run/sshd
