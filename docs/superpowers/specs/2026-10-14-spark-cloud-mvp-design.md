# Spark Cloud — 子项目 1：核心闭环 MVP 设计文档

- 日期：2026-10-14
- 状态：待评审
- 范围：200 台 NVIDIA DGX Spark 算力租赁平台的第一个子项目——用户能实名、充值、租用整机、SSH 接入、到期自动洗机，运营能在后台管理机器/实例/用户/财务。

## 0. 背景与约束

| 项 | 内容 |
|---|---|
| 硬件 | 200 台 DGX Spark：GB10，arm64，128GB CPU/GPU 统一内存，4TB NVMe，10GbE + 2×200G CX-7，**无 BMC** |
| 系统 | DGX OS（Ubuntu 系），驱动、Docker、NVIDIA Container Toolkit 由宿主机提供 |
| 机房 | 自建，电力带宽充足；每台机器挂在智能 PDU 独立口上（型号未定） |
| 开发 | 平台自研，Go 后端 + Vue3 前端，由 Claude 持续开发 |
| 域名 | 未定，作为配置项（开发期 `spark.local`） |
| 参考产品 | 器灵算力站 spark.zhujihezi.com |

### 0.1 已确认的产品决策

1. 整机租赁形态：**用户拿到独占整机 GPU 的容器内 root**，宿主机归平台管理。
2. 充值：**在线支付（微信/支付宝）+ 后台手动充值**，支付做成插件。
3. 计费：**只做预付包时长**，规格为 1 小时 / 1 天 / 1 周 / 1 月；下单即扣款，可续费。
4. **计时从实例开通成功（进入 running）开始**，开机准备时间不计入。
5. 到期后**停机并保留数据 6 小时**，6 小时内续费可恢复，超时自动洗机。
6. 用户主动提前释放：**不退款**，直接洗机。
7. 机器故障（Agent 失联）：**暂停计时，恢复后补回**；不可恢复时管理员可按剩余时长退款或换机。
8. 注册：手机号 + 短信验证码；**租用前必须实名**（个人三要素自动核验，企业上传执照人工审核）。短信/实名用**阿里云**，均为插件，开发期用 mock。
9. PDU：通用 SNMP 驱动 + mock 驱动；**自动断电重启默认关闭**，仅管理员手动触发。

### 0.2 MVP 不包含

优惠券/返佣、发票、工单、站内信、双机互联、一键部署模型、Token 商城、共享容器（HAMi）、裸机 root、按量计费。

## 1. 架构

方案：**模块化单体 + 节点 Agent**。

```
                     公网
       ┌──────────────┼──────────────────────┐
       │              │                      │
  HTTPS:443      SSH:22                HTTPS:443 (*.domain)
  控制台/后台     sshpiper              Traefik ──▶ 实例 Web 端口
       │              │ gRPC 插件查路由        │ HTTP provider 拉路由
       ▼              ▼                      ▼
  ┌─────────────────────────────────────────────────┐
  │ platform (Go 单体)                               │
  │  REST API │ gRPC AgentGateway │ River 任务 │ 路由API│
  │  identity wallet catalog order instance fleet   │
  │  access admin plugins(sms/kyc/pay/pdu)          │
  └───────┬──────────────┬──────────────────────────┘
      PostgreSQL       Redis
          ▲
          │ gRPC 双向流 + mTLS（Agent 主动外连）
  ┌───────┴───────────────────────────────┐
  │ spark-agent ×200（systemd, 宿主机）     │
  │  Docker 管理 │ 数据盘 │ iptables │ tc │ 指标 │
  └───────────────────────────────────────┘
```

### 1.1 仓库结构

```
spark-cloud/
├── cmd/platform/          # 控制面入口
├── cmd/spark-agent/       # 节点 Agent 入口
├── api/proto/             # Agent ↔ platform、sshpiper 插件的 protobuf
├── api/openapi/           # REST 接口 OpenAPI 描述（前端代码生成）
├── internal/
│   ├── identity/  wallet/  catalog/  order/  instance/
│   ├── fleet/  access/  admin/
│   ├── plugins/{sms,kyc,pay,pdu}/{aliyun|wechat|alipay|snmp, mock}
│   ├── agent/             # Agent 侧实现（runtime、disk、netfilter、metrics）
│   └── platform/          # 公共：配置、数据库、日志、错误、鉴权中间件
├── migrations/            # SQL 迁移
├── web/console/  web/admin/
├── deploy/                # docker compose（控制面）、systemd（Agent）、Traefik/sshpiper 配置
└── docs/
```

模块之间只通过 Go 接口调用，不跨模块直接读写对方的表。

### 1.2 技术选型

| 用途 | 选型 |
|---|---|
| HTTP | chi |
| DB 访问 | pgx + sqlc（SQL 优先、类型安全） |
| 迁移 | goose |
| 后台任务 | River（PostgreSQL 队列，任务与业务同事务入队） |
| Agent 通信 | gRPC 双向流 + mTLS，platform 内置 CA 签发机器证书 |
| 会话 | 不透明 token 存 Redis，httpOnly Cookie；管理后台单独会话域 |
| 日志 | slog JSON |
| 前端 | Vue3 + TS + Vite + Element Plus，openapi-typescript 生成类型 |
| 接入 | sshpiper（自定义 gRPC 插件）、Traefik（HTTP provider） |
| 镜像仓库 | Harbor（内网，arm64 镜像） |

## 2. 数据模型（核心表）

金额一律为 `bigint` 分。所有表含 `created_at/updated_at`。

| 表 | 关键字段 |
|---|---|
| `users` | id, phone(唯一), kind(personal/enterprise), kyc_status(none/pending/verified/rejected), status(active/banned) |
| `kyc_records` | user_id, kind, real_name, id_number(加密存储), license_file, result, reviewer_id |
| `ssh_keys` | user_id, name, public_key, fingerprint |
| `wallet_accounts` | user_id, balance（冗余，带 version 乐观锁，以流水为准可重算） |
| `wallet_ledger` | id, user_id, amount(±), balance_after, kind(recharge/admin_adjust/order_pay/refund), ref_type, ref_id, operator_id, remark — **只追加** |
| `recharges` | id, user_id, channel(wechat/alipay/manual), amount, status, out_trade_no, paid_at |
| `products` | id, name, spec(json: GB10/128GB/4TB), status |
| `prices` | product_id, duration(1h/1d/1w/1m), amount, enabled |
| `images` | id, name, ref(harbor 地址), description, enabled, sort |
| `orders` | id, user_id, kind(new/renew), instance_id, product_id, duration, amount, status(paid/refunded/partially_refunded) |
| `instances` | id(`ins-xxxxxx`), user_id, machine_id, image_id, state, expires_at, paused_remaining(故障暂停时剩余秒数), stopped_at, ssh_password(加密), ports(json) |
| `instance_events` | instance_id, from_state, to_state, reason, at — 状态迁移全记录 |
| `machines` | id, serial(唯一), rack, slot, lan_ip, pdu_id, pdu_outlet, state(idle/allocated/maintenance/faulty), agent_online, last_seen_at, cert_fingerprint |
| `pdus` | id, name, driver(snmp/mock), address, community(加密) |
| `agent_commands` | id(uuid), machine_id, instance_id, type, payload, status(pending/sent/done/failed), result, attempts |
| `admins` | id, username, password_hash, role(super/ops/finance), status |
| `audit_logs` | actor_type, actor_id, action, target_type, target_id, diff(json), ip, at |

规则：
- 每笔扣费 → 一个 order → 一个 instance；每笔入账 → 一个 recharge 或一条 admin_adjust（必须带原因）。
- 机器分配：`SELECT ... FROM machines WHERE state='idle' AND agent_online ... FOR UPDATE SKIP LOCKED LIMIT 1`。
- 余额扣减在同一事务中：锁 `wallet_accounts` 行 → 校验余额 → 写 ledger → 更新 balance。

## 3. 实例生命周期

状态：`creating → running → stopped → wiping → released`，以及 `failed`。

| 迁移 | 触发 | 动作 |
|---|---|---|
| (下单) → creating | 用户下单 | 同一事务：扣款、建 order、分配机器(→allocated)、建 instance、入队 CreateInstance 指令 |
| creating → running | Agent 回报容器就绪 | `expires_at = now + duration`；写路由；短信通知（可选） |
| creating → failed | 无空闲机器 / Agent 报错 / 10 分钟超时 | 全额退款（ledger refund）；机器回 idle 或 faulty |
| running → running | 续费 | `expires_at += duration` |
| running → stopped | 到期（River 每分钟扫描） | 下发 StopInstance；撤销路由；`stopped_at = now` |
| stopped → running | 6 小时内续费 | 下发 StartInstance；成功后 `expires_at = now + duration` |
| stopped → wiping | stopped 超过 6 小时 | 下发 WipeInstance |
| running → wiping | 用户主动释放（不退款） | 撤销路由；下发 WipeInstance |
| wiping → released | Agent 回报洗机成功 | 机器 → idle |
| wiping → (保持) | 洗机失败 | 机器 → faulty，告警运维；实例保持 wiping 直到管理员处理 |

**故障暂停**：running 实例的机器 Agent 失联 > 5 分钟 → 实例标记 `suspended`（running 的子状态标志），记录 `paused_remaining = expires_at - now`，到期扫描跳过；Agent 恢复且容器正常 → `expires_at = now + paused_remaining`。管理员可对其执行「按剩余时长退款并释放」或「换机」（换机：新机器上新建容器，数据不迁移）。

**提醒**：到期前 24 小时、1 小时发短信（仅对剩余租期大于该阈值的实例）。

**幂等**：每条 agent_command 有 uuid，Agent 本地记录已执行 id；状态迁移用 `UPDATE ... WHERE state = <期望旧状态>` 条件更新，保证并发/重试安全。

## 4. Node Agent

### 4.1 通信
- Agent 作为 gRPC 客户端主动连 platform，`Connect(stream AgentMessage) returns (stream PlatformMessage)`。
- mTLS：机器上架时 admin 在后台为该序列号签发证书（platform 内置 CA），证书经安装脚本放到 `/etc/spark-agent/`。platform 校验证书 CN = 序列号 且指纹匹配 `machines.cert_fingerprint`。
- 心跳 10 秒；携带指标：GPU 利用率、GPU/系统内存、温度、功耗、磁盘使用、容器状态。
- 断线指数退避重连；重连后发送 `StateReport`（本机容器列表与状态），platform 对账修正。
- platform 侧：pending 指令在 Agent 连上后下发；每条指令有超时，超时视为失败交给状态机处理。

### 4.2 指令
`CreateInstance{instance_id, image_ref, upstream_pubkey, bandwidth_mbps, ports}` / `StartInstance` / `StopInstance` / `WipeInstance` / `UpdatePorts` / `PullImage`。

用户密码与公钥只在 sshpiper 层校验（见 5.1），容器内 sshd 只信任平台的上游密钥，因此重置密码、改公钥不需要下发到机器。

### 4.3 容器规格
- 每台机器最多 1 个用户容器，名称 `ins-xxxxxx`。
- `docker run --gpus all --ipc=host`，**非 privileged**，按需添加 capabilities；ulimit 放开 memlock。
- 容器内 sshd 端口映射到宿主机内网 IP 的固定端口（如 2222）；Web 端口映射到内网固定端口段。
- 数据盘：宿主机专用分区挂载到容器 `/root/data`。洗机 = 删容器 + 删除该实例的容器层 + `mkfs` 数据分区 + 抽样校验。
- 网络：iptables 禁止容器访问机房内网与管理网段，放行 Harbor、HF 镜像站等白名单；`tc` 限速（默认值后台可配，如 200Mbps）。
- 镜像预拉取：新镜像上架时 platform 广播 PullImage。

### 4.4 Runtime 抽象
Agent 内部 `Runtime` 接口（Create/Start/Stop/Wipe/Inspect），实现：
- `docker`：生产实现；
- `fake`：内存模拟，用于本地开发与集成测试（无 GPU、无 Docker 也可跑全链路）。

磁盘与网络操作同样走接口（`Disk`、`Netfilter`），fake 实现用于测试。

## 5. 接入层

### 5.1 SSH
- 用户连接：`ssh ins-a1b2c3@ssh.<domain>`（用户名即实例 ID，进入后为容器 root）。
- sshpiper 自定义 gRPC 插件 → platform `access` 模块：按实例 ID 查 `running` 实例的机器内网 IP 与 sshd 端口；非 running 返回拒绝。
- 认证：sshpiper 层校验用户密码或公钥（platform 判定），上游使用平台持有的内部密钥登录容器，用户凭据不直接透传到容器。
- 公网仅 sshpiper:22 与 Traefik:443/80 暴露。

### 5.2 Web
- `https://ins-a1b2c3-<port>.<domain>` → 实例机器内网对应端口。
- Traefik HTTP provider 每 5 秒从 platform 拉取动态路由；泛域名证书（DNS-01 或商业证书，配置项）。
- 默认私有：Traefik ForwardAuth 到 platform，校验控制台会话属于实例主人；用户可将端口设为公开。
- MVP 每实例最多开放 5 个端口。

## 6. 计费、支付与插件

- 价格表 `prices` 由后台维护；下单时快照金额写入 order。
- 续费与新购同一套扣款逻辑。
- 支付插件接口：`CreatePayment(recharge) → 支付参数/二维码`，`HandleNotify(req) → 结果`，`Query(out_trade_no)`。实现：wechat（Native 扫码）、alipay（当面付/电脑网站）、mock。回调验签后在事务内入账；River 定时任务对未完成订单主动查单。
- 手动充值/扣款：财务或超级管理员，必须填写原因，写 audit_log。
- 短信插件：`Send(phone, template, params)`；实现 aliyun、mock（验证码固定 `000000`，仅非生产环境可用）。验证码 Redis 存 5 分钟，单号 60 秒限 1 次、每日上限 10 次，IP 限频。
- 实名插件：`VerifyPerson(name, idNumber, phone)`；实现 aliyun（三要素）、mock（总是通过）。身份证号加密存储（AES-GCM，密钥来自配置）。
- PDU 插件：`PowerCycle(outlet)`、`PowerOff`、`PowerOn`、`Status`；实现 snmp（通用 OID 可配置）、mock。

## 7. 前端

### 7.1 用户控制台
登录、实名认证、算力市场（规格、空闲数量、选租期与镜像下单）、我的实例（状态、到期倒计时、SSH 命令复制、密码查看/重置、端口管理、续费、释放、近 1 小时 GPU/内存小图）、钱包（余额、充值、流水）、SSH 公钥、文档。

### 7.2 管理后台
仪表盘、机器管理（录入/CSV 批量导入、签发证书、维护上下线、PDU 重启、实时指标）、实例管理（强制停机/洗机、延长时长、按剩余时长退款、换机）、用户管理（查询、封禁、企业认证审核、手动充值/扣款）、订单与流水（查询、导出 CSV）、价格与镜像、管理员与角色（super/ops/finance）、审计日志。

指标小图数据：platform 将每台机器最近 1 小时心跳指标存 Redis 环形缓冲。

## 8. 错误处理

- 统一错误码 + 中文消息；API 返回 `{code, message, request_id}`。
- 涉及钱的操作全部在数据库事务内完成，失败整体回滚；外部调用（支付、短信、Agent）不放在事务内，通过 River 任务异步重试。
- Agent 指令失败：按状态机转换（creating 失败退款、wiping 失败标记 faulty）。
- 所有状态异常（机器失联、洗机失败、支付回调验签失败）写日志并触发告警（MVP：后台告警列表 + 可选 Webhook 推送到企业微信/飞书）。

## 9. 部署

- **控制面**：一台（或两台主备）x86/arm 服务器，`docker compose` 部署 platform、PostgreSQL、Redis、sshpiper、Traefik、Harbor。PostgreSQL 每日备份。
- **Agent**：单个静态二进制（linux/arm64）+ systemd unit；安装脚本负责放置证书、配置、数据分区初始化、iptables 基础规则。
- 配置：YAML + 环境变量覆盖；环境 `dev/staging/prod`，prod 禁用全部 mock 插件。

## 10. 测试与验证

| 层级 | 方式 |
|---|---|
| 单元测试 | 状态机迁移、计费与余额、幂等、价格计算、路由解析；TDD 编写 |
| 集成测试 | 真实 PostgreSQL（docker / 本地实例）跑 wallet、order、instance 事务与并发分配（并发 100 单抢 10 台机器不重复不超卖） |
| 全链路（无硬件） | platform + N 个 `fake runtime` Agent 进程：下单 → 开通 → 到期 → 停机 → 6 小时 → 洗机 → 释放；时间通过可注入的 Clock 加速 |
| 真机验证 | 用户提供的 DGX Spark：docker runtime 实际开容器、GPU 可见（`nvidia-smi`、PyTorch 跑 CUDA）、128GB 内存可用、数据盘洗净校验、iptables 隔离、sshpiper/Traefik 链路、PDU 断电重启 |
| 前端 | 关键流程 Playwright 冒烟测试 |

开发环境注意：当前开发机未安装 Go 与 Docker，实施第一步需安装 Go 工具链；PostgreSQL/Redis 可用 Homebrew 或 Docker 运行。

## 11. 后续子项目（不在本文档范围）

2. 应用层：镜像市场扩充、JupyterLab / Web 终端、一键部署模型、双机互联
3. Token 商城：推理池 + OpenAI 兼容 API（LiteLLM/Higress 改造）+ 统一余额
4. 共享容器（k3s + HAMi，需 PoC）、AI 管家、ComfyUI 工作流、企业功能（发票、子账号）
