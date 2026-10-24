export PATH := $(HOME)/.local/go/bin:$(HOME)/.local/bin:$(PATH)

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

DEV_DB  := postgres://postgres:postgres@localhost:5433/spark?sslmode=disable
# 仅本地开发使用的固定密钥，生产环境必须用 openssl rand -base64 32 生成
DEV_KEY := MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=

.PHONY: test devdb run build web-install web web-test e2e check proto agent platform-image app-images

# 协议代码已入库，日常构建不需要 protoc。重新生成：protoc 36.2 + protoc-gen-go v1.36.12 + protoc-gen-go-grpc v1.6.2（都在 ~/.local/bin）
proto:
	protoc -I api/proto --go_out=. --go_opt=module=spark-cloud --go-grpc_out=. --go-grpc_opt=module=spark-cloud api/proto/agent/v1/agent.proto

test:
	go vet ./... && go test ./... && sh deploy/agent/install_test.sh && $(MAKE) web-test

devdb:
	go run ./cmd/devdb

run:
	SPARK_DATABASE_URL=$(DEV_DB) SPARK_SECRET_KEY=$(DEV_KEY) go run ./cmd/platform

# 前端：首次 make web-install；产物输出到 internal/web/dist/{console,admin}，由 Go embed
web-install:
	cd web && npm ci

web:
	cd web && npm run build

web-test:
	cd web && npm test

# 端到端：先构建前端（e2e-server 与生产一样从 embed 读产物），核对覆盖表，再跑 Playwright（自动拉起 cmd/e2e-server）
e2e: web
	cd web && npm run e2e:coverage && npx playwright test

# 节点 Agent：静态 linux/arm64 二进制（DGX Spark），输出 SHA-256 供 SPARK_AGENT_BINARY_SHA256 与 install.sh 校验
agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/spark-agent-linux-arm64 ./cmd/spark-agent
	shasum -a 256 bin/spark-agent-linux-arm64

# 发布产物先构建前端再编译后端，保证二进制里是最新页面
build: web
	GOOS=linux GOARCH=amd64 go build -o bin/platform-linux-amd64 ./cmd/platform
	GOOS=linux GOARCH=arm64 go build -o bin/platform-linux-arm64 ./cmd/platform
	$(MAKE) agent

# platform 容器镜像（前端 + 后端多阶段构建，部署见 deploy/compose/README.md）；只在本地打标签，不推送
# 大陆网络：GOPROXY=https://goproxy.cn,direct NPM_REGISTRY=https://registry.npmmirror.com/ make platform-image
# 为空时不传 --build-arg（传空值会覆盖 Dockerfile 里的默认值）；DISTROLESS 可换成镜像站全名（digest 不变），见 deploy/compose/README.md
GOPROXY ?=
NPM_REGISTRY ?=
DISTROLESS ?=
platform-image:
	docker build -f deploy/platform/Dockerfile --build-arg VERSION=$(VERSION) \
		$(if $(GOPROXY),--build-arg GOPROXY=$(GOPROXY)) $(if $(NPM_REGISTRY),--build-arg NPM_REGISTRY=$(NPM_REGISTRY)) $(if $(DISTROLESS),--build-arg DISTROLESS=$(DISTROLESS)) \
		-t spark-cloud/platform:$(VERSION) -t spark-cloud/platform:latest .

# 应用镜像（计划 7）：在一台 arm64 机器上构建；大陆网络传 PIP_INDEX_URL / COMFYUI_SRC，见 deploy/image/apps/README.md
APP_IMAGE_TAG ?= 25.09
PIP_INDEX_URL ?=
COMFYUI_SRC ?=
app-images:
	docker build -f deploy/image/apps/llm-serving/Dockerfile --build-arg BASE=nvcr.io/nvidia/vllm:$(APP_IMAGE_TAG)-py3 \
		$(if $(PIP_INDEX_URL),--build-arg PIP_INDEX_URL=$(PIP_INDEX_URL)) -t spark/llm-serving:$(APP_IMAGE_TAG) deploy/image/apps/llm-serving
	docker build -f deploy/image/apps/comfyui/Dockerfile --build-arg BASE=nvcr.io/nvidia/pytorch:$(APP_IMAGE_TAG)-py3 \
		$(if $(PIP_INDEX_URL),--build-arg PIP_INDEX_URL=$(PIP_INDEX_URL)) $(if $(COMFYUI_SRC),--build-arg COMFYUI_SRC=$(COMFYUI_SRC)) -t spark/comfyui:$(APP_IMAGE_TAG) deploy/image/apps/comfyui
	docker build -f deploy/image/apps/pytorch/Dockerfile --build-arg BASE=nvcr.io/nvidia/pytorch:$(APP_IMAGE_TAG)-py3 \
		$(if $(PIP_INDEX_URL),--build-arg PIP_INDEX_URL=$(PIP_INDEX_URL)) -t spark/pytorch:$(APP_IMAGE_TAG) deploy/image/apps/pytorch

check: test e2e
