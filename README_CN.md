<div align="center">

# GPUBay

**开源的自建 GPU 算力租赁云控制面。**

在自己的机房里出租 GPU 整机（SSH 直连）、按 Token 售卖 OpenAI 兼容的大模型 API、一键部署 AI 应用，
全部由一个可自托管的平台完成。

[![CI](https://github.com/DiWuJi-Club/GPUBay/actions/workflows/ci.yml/badge.svg)](https://github.com/DiWuJi-Club/GPUBay/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8.svg)](go.mod)

[English](README.md) | 简体中文

</div>

---

GPUBay 是一个自建 GPU 机房背后的软件：把一排机器（目前是 NVIDIA DGX Spark / GB10，arm64）变成一朵云。
用户注册、充值钱包，然后租用独占的 GPU 实例或者申请 API Key；运营人员在独立的管理后台里管理机器、价格、支付和用户。

> **项目状态：** 1.0 之前，持续开发中。已经在真机上运行，但接口、配置和数据库结构仍可能变化。
> 短信、实名核验、支付等对接目前面向中国大陆服务商（阿里云、微信支付、支付宝），欢迎贡献其他服务商的实现。

## 功能

**GPU 实例租赁**
- 整机以隔离容器交付：root SSH、持久数据盘
- 内置 SSH 网关（`ssh <实例>@ssh.<你的域名>`）和按端口的 HTTPS Web 入口，机器无需公网 IP
- 按量、包时、档期预订、一次多台（机组）、自动续费
- 跨多台机器的多机模型部署

**Token 商城（大模型 API）**
- OpenAI 兼容的 `/v1` 接口，用户自助创建 API Key
- 按模型设置输入 / 输出单价，钱包预付扣费
- 运营维护的 vLLM 服务池：副本调度、健康检查、故障转移

**AI 应用一键部署**
- 应用目录（LLM Serving、ComfyUI、PyTorch 等）一键部署到已租实例
- ComfyUI 工作流市场
- 控制台内置 AI 管家，由平台自己的模型 API 驱动

**运营**
- 节点 Agent：mTLS gRPC、自检、镜像预拉取、cgroup v2 资源限制
- 管理后台：角色权限、强制 TOTP 二次验证、审计日志
- 钱包、充值（微信支付 / 支付宝）、退款、邀请返利
- 通过 SNMP PDU 远程断电重启

## 架构

```
             用户                                运营
   控制台 / SSH / HTTPS / API                管理后台（内网）
               │                                   │
   ┌───────────▼───────────────────────────────────▼──────────┐
   │                        platform                          │
   │  用户 API · 后台 API · SSH 网关 · Web 入口                │
   │  大模型网关 (/v1) · 计费 · 调度 · 应用部署 Runner          │
   └──────────────┬──────────────────────────┬────────────────┘
                  │ PostgreSQL               │ gRPC + mTLS（内网）
                  ▼                          ▼
             ┌─────────┐     ┌───────────┐ ┌───────────┐ ┌───────────┐
             │  数据库  │     │ Agent     │ │ Agent     │ │   ...     │
             └─────────┘     │ GPU 节点  │ │ GPU 节点  │ │           │
                             └───────────┘ └───────────┘ └───────────┘
```

| 组件 | 路径 | 技术栈 |
|---|---|---|
| 平台（控制面） | `cmd/platform`、`internal/` | Go、PostgreSQL |
| 节点 Agent | `cmd/spark-agent`、`internal/agent` | Go、Docker、cgroup v2 |
| 用户控制台与管理后台 | `web/console`、`web/admin` | Vue 3、TypeScript、Vite |
| 部署 | `deploy/compose`、`deploy/agent` | Docker Compose、systemd |

## 快速开始（本地开发）

需要 Go 1.27+、Node 22 / npm 10。**不需要** Docker：测试和本地开发使用嵌入式 PostgreSQL。

```bash
git clone https://github.com/DiWuJi-Club/GPUBay.git && cd GPUBay
make web-install   # 安装前端依赖
make web           # 构建控制台和后台，嵌入 Go 二进制
make devdb         # 终端 1：启动本地 PostgreSQL（端口 5433）
make run           # 终端 2：控制台 http://localhost:8080 ，后台 http://127.0.0.1:8081/admin/
```

开发环境下短信、实名核验、支付、PDU 都是 mock。控制台用任意手机号 + 验证码 `000000` 登录；
第一个管理员用 `go run ./cmd/platform admin create --username root --role super` 创建。

```bash
make test   # Go 测试 + 前端单测
make e2e    # Playwright 端到端测试（需要本机 Chrome）
```

## 文档

- [运维手册](docs/operations.md)：环境变量、部署、各功能发布注意事项
- [设计文档](docs/superpowers/specs/)与[实施计划](docs/superpowers/plans/)
- [实例镜像规范](docs/image-spec.md)

> 代码里仍沿用早期的工作名 `spark-cloud` / `SPARK_*`（模块路径、二进制名、环境变量），后续版本会统一改为 GPUBay。

## 参与贡献

欢迎提 Issue 和 Pull Request，提交前请先阅读 [CONTRIBUTING.md](CONTRIBUTING.md)；
安全问题请按 [SECURITY.md](SECURITY.md) 私下报告，不要公开提 Issue。

## 许可证

[Apache License 2.0](LICENSE)
