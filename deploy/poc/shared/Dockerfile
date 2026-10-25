# 共享机 PoC 镜像 spark/poc-shared:1（规格 §3.1）。
# 构建：docker build -t spark/poc-shared:1 deploy/poc/shared   （或 run-poc.sh build）
# 机器访问不了 Docker Hub，基础镜像用 nvcr.io；apt 走 ports.ubuntu.com（arm64 镜像默认源）。
# GB10 计算能力实测 12.1 → sm_121（nvidia-smi --query-gpu=compute_cap --format=csv）。
# cudart 静态链接、不用 cuBLAS：/poc/* 只依赖驱动，可直接 scp 进 CUDA base 镜像的容器运行。
FROM nvcr.io/nvidia/cuda:13.0.1-devel-ubuntu24.04

ARG CUDA_ARCH=sm_121

RUN apt-get update \
 && apt-get install -y --no-install-recommends stress procps \
 && rm -rf /var/lib/apt/lists/*

COPY alloc.cu burn.cu fault.cu /poc/src/

RUN set -e; \
    for p in alloc burn fault; do \
      nvcc -O2 -arch=${CUDA_ARCH} -cudart static -o /poc/$p /poc/src/$p.cu; \
    done; \
    ls -l /poc

ENV PATH=/poc:${PATH}
WORKDIR /poc
