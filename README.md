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

开发环境支付为 mock：充值后调用 `POST /api/v1/dev/recharges/{id}/pay` 模拟支付成功（生产环境不注册该接口）。PDU 为 mock，断电重启只记录调用。

### 管理后台

后台 API 在独立端口（默认 `127.0.0.1:8081`，前缀 `/admin/api/v1`），系统没有默认账号，先用命令行创建第一个超级管理员：

```bash
SPARK_DATABASE_URL=... SPARK_SECRET_KEY=... go run ./cmd/platform admin create --username root --role super
```

按提示输入两次密码（至少 12 位），用验证器 App 扫描终端里的二维码并输入一次动态码完成绑定，然后离线保存打印出的 8 个恢复码。
管理员丢失手机时用 `platform admin reset-totp --username <名称>` 重置，下次登录重新绑定。

## 配置（环境变量）

| 变量 | 说明 | 默认 |
|---|---|---|
| `SPARK_ENV` | dev / staging / prod | dev |
| `SPARK_HTTP_ADDR` | 监听地址 | :8080 |
| `SPARK_DATABASE_URL` | PostgreSQL 连接串 | 必填 |
| `SPARK_SECRET_KEY` | 32 字节密钥的 base64，加密身份证号与实例密码 | 必填 |
| `SPARK_DOMAIN` | 对外域名，用于生成 SSH 命令 | spark.local |
| `SPARK_ADMIN_ADDR` | 后台 API 监听地址（只应绑定内网地址） | 127.0.0.1:8081 |
| `SPARK_UPLOAD_DIR` | 上传文件（营业执照）保存目录，需定期备份 | ./.devdata/uploads |
| `SPARK_SMS_DRIVER` / `SPARK_KYC_DRIVER` | mock / aliyun（生产禁止 mock） | mock |
| `SPARK_PAY_DRIVER` | mock / real（生产禁止 mock；real 时启用已配置的微信和 / 或支付宝） | mock |
| `SPARK_PDU_DRIVER` | mock / snmp（生产禁止 mock；snmp 时按每台 PDU 的配置） | mock |
| `SPARK_PUBLIC_BASE_URL` | 对外 https 地址，支付回调为 `<地址>/api/v1/pay/notify/{wechat,alipay}`；real 支付必填 | 空 |
| `SPARK_ALIYUN_ACCESS_KEY_ID` / `SPARK_ALIYUN_ACCESS_KEY_SECRET` | 阿里云 AccessKey（短信与号码百科共用） | 空 |
| `SPARK_SMS_SIGN` / `SPARK_SMS_TPL_LOGIN` / `SPARK_SMS_TPL_EXPIRY` | 短信签名、登录验证码模板（参数 code）、到期提醒模板（参数 instance、hours） | 空 |
| `SPARK_KYC_AUTH_CODE` | 号码百科三要素核验授权码 | 空 |
| `SPARK_WECHAT_MCH_ID` / `_APP_ID` / `_API_V3_KEY` / `_CERT_SERIAL` / `_PRIVATE_KEY_PATH` | 微信支付商户号、AppID、APIv3 密钥、商户证书序列号、商户私钥文件 | 空 |
| `SPARK_WECHAT_PUBLIC_KEY_ID` / `SPARK_WECHAT_PUBLIC_KEY_PATH` | 微信支付公钥 ID 与公钥文件（商户平台「API 安全」中下载） | 空 |
| `SPARK_ALIPAY_APP_ID` / `_PRIVATE_KEY_PATH` / `_PUBLIC_KEY_PATH` / `_SANDBOX` | 支付宝应用 ID、应用私钥文件、支付宝公钥文件、是否沙箱 | 空 / false |
| `SPARK_SWEEP_INTERVAL` | 清扫间隔 | 1m |
| `SPARK_TRUST_PROXY` | 仅当 platform 部署在会改写 X-Forwarded-For 的反向代理之后才设为 true（只作用于用户 API；后台 API 始终以连接地址为准） | false |

## 部署注意

- 启动时自动执行数据库迁移（多副本同时启动时由 PostgreSQL 会话级咨询锁串行化）。
- 迁移 `00007_rental.sql` 会执行 `CREATE EXTENSION IF NOT EXISTS btree_gist`（机器占用时段的排他约束需要）。
  数据库角色需要有创建该扩展的权限；托管数据库上若无此权限，请由管理员预先在目标库执行
  `CREATE EXTENSION btree_gist;`。
- 后台端口不要暴露到公网；如必须经过反向代理，请限制来源 IP。
- `SPARK_UPLOAD_DIR` 存放用户上传的营业执照，需要纳入备份。
- 支付回调地址需在微信支付商户平台、支付宝开放平台中配置为 `SPARK_PUBLIC_BASE_URL` 下的对应路径。

## API 约定

- `POST /api/v1/orders` 与 `POST /api/v1/instances/{id}/renew` 支持可选请求头 `Idempotency-Key`（最长 64 字符）。
  同一用户用同一个键重复提交（双击、超时重试）只会扣款一次，并返回首次创建的订单（下单还会返回该实例的当前状态）；
  同一个键被用于不同类型的请求或续费不同实例时返回 409 `idempotency_conflict`。客户端应为每次「新操作」生成新的键（如 UUID）。
