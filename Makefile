export PATH := $(HOME)/.local/go/bin:$(PATH)

DEV_DB  := postgres://postgres:postgres@localhost:5433/spark?sslmode=disable
# 仅本地开发使用的固定密钥，生产环境必须用 openssl rand -base64 32 生成
DEV_KEY := MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=

.PHONY: test devdb run build

test:
	go vet ./... && go test ./...

devdb:
	go run ./cmd/devdb

run:
	SPARK_DATABASE_URL=$(DEV_DB) SPARK_SECRET_KEY=$(DEV_KEY) go run ./cmd/platform

build:
	GOOS=linux GOARCH=amd64 go build -o bin/platform-linux-amd64 ./cmd/platform
	GOOS=linux GOARCH=arm64 go build -o bin/platform-linux-arm64 ./cmd/platform
