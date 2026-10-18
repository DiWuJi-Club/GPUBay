# 参考平台镜像：CUDA 基础镜像 + sshd。
# Agent 以 /.spark/init.sh 覆盖入口，这里不设 ENTRYPOINT / CMD；不要在镜像里放密钥或口令。
FROM nvcr.io/nvidia/cuda:13.0.1-base-ubuntu24.04
LABEL spark.image.spec="1"
# openssh-server 的 postinst 会生成 /etc/ssh/ssh_host_* 并留在镜像层里，所有租户容器会共用同一把私钥，必须删除；
# 主机密钥由 Agent 按实例生成并放在 /.spark/hostkeys/。
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server ca-certificates curl iproute2 \
 && rm -rf /var/lib/apt/lists/* \
 && rm -f /etc/ssh/ssh_host_* \
 && mkdir -p /run/sshd
