export PATH := $(HOME)/.local/go/bin:$(HOME)/.local/bin:$(PATH)

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

DEV_DB  := postgres://postgres:postgres@localhost:5433/spark?sslmode=disable
# 仅本地开发使用的固定密钥，生产环境必须用 openssl rand -base64 32 生成
DEV_KEY := MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=

.PHONY: test devdb run build web-install web web-test e2e check proto agent

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

check: test e2e
