# 多阶段：前端（Node 22）→ 后端（Go 1.27，前端产物 embed）→ 运行镜像
# 基础镜像固定到具体版本，与仓库工具链一致（go.mod: go 1.27.2；本地 Node 22.23.1）；升级时同步改这里。
# 运行镜像：distroless static nonroot（uid/gid 65532，无 shell），按 digest 固定；更新：crane digest gcr.io/distroless/static-debian12:nonroot
# 大陆网络：--build-arg DISTROLESS=m.daocloud.io/gcr.io/distroless/static-debian12:nonroot@sha256:<同一 digest>
ARG DISTROLESS=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM node:22.23.1-bookworm-slim AS web
# 大陆网络：--build-arg NPM_REGISTRY=https://registry.npmmirror.com/
ARG NPM_REGISTRY=https://registry.npmjs.org/
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
COPY web/shared/package.json shared/
COPY web/console/package.json console/
COPY web/admin/package.json admin/
RUN npm ci --registry=$NPM_REGISTRY
COPY web/ ./
# vite 的 outDir 是 ../../internal/web/dist/{console,admin}，即 /src/internal/web/dist
RUN npm run build

FROM golang:1.27.2-bookworm AS build
# 大陆网络：--build-arg GOPROXY=https://goproxy.cn,direct
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/web/dist internal/web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/platform ./cmd/platform
# 上传目录预先建好并归 nonroot（65532）所有：compose 的命名卷首次挂载时继承这个属主，platform 才能写入
RUN mkdir -p /out/data/uploads

FROM ${DISTROLESS}
ARG VERSION=dev
LABEL org.opencontainers.image.title="spark-cloud platform" org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/platform /platform
COPY --from=build --chown=65532:65532 /out/data /data
# 容器内只监听非特权端口（:8443 / :8080 / :2222 / :9443），由 compose 映射到 443 / 80 / 22，不需要 NET_BIND_SERVICE
EXPOSE 2222 8080 8443 9443
USER nonroot
ENTRYPOINT ["/platform"]
