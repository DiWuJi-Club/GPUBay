<div align="center">

# GPUBay

**Open-source control plane for running a GPU rental cloud on your own hardware.**

Rent out bare GPU machines over SSH, sell OpenAI-compatible model APIs by the token,
and deploy AI apps in one click, all from a single self-hosted platform.

[![CI](https://github.com/DiWuJi-Club/GPUBay/actions/workflows/ci.yml/badge.svg)](https://github.com/DiWuJi-Club/GPUBay/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8.svg)](go.mod)

English | [简体中文](README_CN.md)

</div>

---

GPUBay is the software behind a self-built GPU data center: it turns a rack of machines
(today: NVIDIA DGX Spark / GB10, arm64) into a cloud where customers sign up, top up a
wallet, and get either a dedicated GPU instance or an API key. Operators get a separate
admin console to manage the fleet, pricing, payments and users.

> **Status:** pre-1.0 and under active development. It runs on real hardware but APIs,
> config and schema may still change. Several integrations (SMS, identity verification,
> payments) currently target mainland-China providers; contributions for other providers
> are welcome.

## Features

**GPU instance rental**
- Dedicated machines delivered as isolated containers with root SSH and a persistent data disk
- Built-in SSH gateway (`ssh <instance>@ssh.<your-domain>`) and per-port HTTPS web ingress, no public IP per machine
- Pay-as-you-go, fixed-term and scheduled bookings, multi-machine groups, auto-renew
- Multi-node model serving across several machines

**Token mall (model API)**
- OpenAI-compatible `/v1` endpoint with per-user API keys
- Per-model input/output pricing, prepaid wallet billing
- Operator-managed vLLM serving pool with replica scheduling, health checks and failover

**One-click AI apps**
- App catalog (LLM serving, ComfyUI, PyTorch, ...) deployed into a rented instance
- ComfyUI workflow marketplace
- AI assistant ("butler") in the console, powered by the platform's own model API

**Operations**
- Node agent with mTLS gRPC, self-check, image pre-pull and cgroup v2 resource limits
- Admin console with role-based access, mandatory TOTP and audit log
- Wallet, recharge (WeChat Pay / Alipay), refunds, referral rebates
- Remote power control via SNMP PDUs

## Architecture

```
            customers                         operators
   console / SSH / HTTPS / API             admin console (LAN)
               │                                   │
   ┌───────────▼───────────────────────────────────▼──────────┐
   │                        platform                          │
   │  user API · admin API · SSH gateway · web ingress        │
   │  LLM gateway (/v1) · billing · scheduler · app runner    │
   └──────────────┬──────────────────────────┬────────────────┘
                  │ PostgreSQL               │ gRPC + mTLS (LAN)
                  ▼                          ▼
             ┌─────────┐     ┌───────────┐ ┌───────────┐ ┌───────────┐
             │   DB    │     │ agent     │ │ agent     │ │   ...     │
             └─────────┘     │ GPU node  │ │ GPU node  │ │           │
                             │ GPU node  │ │ GPU node  │ │           │
                             └───────────┘ └───────────┘ └───────────┘
```

| Component | Path | Stack |
|---|---|---|
| Platform (control plane) | `cmd/platform`, `internal/` | Go, PostgreSQL |
| Node agent | `cmd/spark-agent`, `internal/agent` | Go, Docker, cgroup v2 |
| User console & admin console | `web/console`, `web/admin` | Vue 3, TypeScript, Vite |
| Deployment | `deploy/compose`, `deploy/agent` | Docker Compose, systemd |

## Quick start (local development)

Requirements: Go 1.27+, Node 22 / npm 10. Docker is **not** required: tests and the dev
server use an embedded PostgreSQL.

```bash
git clone https://github.com/DiWuJi-Club/GPUBay.git && cd GPUBay
make web-install   # install frontend dependencies
make web           # build console + admin into the Go binary
make devdb         # terminal 1: local PostgreSQL on :5433
make run           # terminal 2: console http://localhost:8080, admin http://127.0.0.1:8081/admin/
```

In development, SMS, identity verification, payments and PDUs are mocked. Log in to the
console with any phone number and code `000000`. Create the first admin with
`go run ./cmd/platform admin create --username root --role super`.

```bash
make test   # Go tests + frontend unit tests
make e2e    # Playwright end-to-end tests (needs Chrome)
```

## Documentation

- [Operations manual (Chinese)](docs/operations.md): configuration reference, deployment, release notes per feature
- [Design specs](docs/superpowers/specs/) and [implementation plans](docs/superpowers/plans/)
- [Instance image spec](docs/image-spec.md)

> The codebase still uses the original working name `spark-cloud` / `SPARK_*` in module
> paths, binaries and environment variables. These will be renamed to GPUBay in a
> future release.

## Contributing

Issues and pull requests are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md) first,
and report security problems privately as described in [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE)
