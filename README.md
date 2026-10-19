# Spark Cloud

DGX Spark 算力租赁平台控制面。设计文档见 `docs/superpowers/specs/`，实施计划见 `docs/superpowers/plans/`。

## 本地开发

需要 Go 1.27（本机安装在 `~/.local/go`）。不需要 Docker：测试和本地开发使用嵌入式 PostgreSQL。

```bash
make test     # 运行全部测试（首次会下载 PostgreSQL 二进制）
make devdb    # 终端 1：启动本地数据库（端口 5433）
make run      # 终端 2：启动 platform（http://localhost:8080）
```

开发环境短信为 mock，验证码固定为 `000000`；实名核验 mock 全部通过。

## 配置（环境变量）

| 变量 | 说明 | 默认 |
|---|---|---|
| `SPARK_ENV` | dev / staging / prod | dev |
| `SPARK_HTTP_ADDR` | 监听地址 | :8080 |
| `SPARK_DATABASE_URL` | PostgreSQL 连接串 | 必填 |
| `SPARK_SECRET_KEY` | 32 字节密钥的 base64，加密身份证号与实例密码 | 必填 |
| `SPARK_DOMAIN` | 对外域名，用于生成 SSH 命令 | spark.local |
| `SPARK_SMS_DRIVER` / `SPARK_KYC_DRIVER` | mock / aliyun（生产禁止 mock） | mock |
| `SPARK_SWEEP_INTERVAL` | 清扫间隔 | 1m |
| `SPARK_TRUST_PROXY` | 仅当 platform 部署在会改写 X-Forwarded-For 的反向代理之后才设为 true | false |

## 部署注意

- 启动时自动执行数据库迁移（多副本同时启动时由 PostgreSQL 会话级咨询锁串行化）。
- 迁移 `00007_rental.sql` 会执行 `CREATE EXTENSION IF NOT EXISTS btree_gist`（机器占用时段的排他约束需要）。
  数据库角色需要有创建该扩展的权限；托管数据库上若无此权限，请由管理员预先在目标库执行
  `CREATE EXTENSION btree_gist;`。

## API 约定

- `POST /api/v1/orders` 与 `POST /api/v1/instances/{id}/renew` 支持可选请求头 `Idempotency-Key`（最长 64 字符）。
  同一用户用同一个键重复提交（双击、超时重试）只会扣款一次，并返回首次创建的订单（下单还会返回该实例的当前状态）；
  同一个键被用于不同类型的请求或续费不同实例时返回 409 `idempotency_conflict`。客户端应为每次「新操作」生成新的键（如 UUID）。
