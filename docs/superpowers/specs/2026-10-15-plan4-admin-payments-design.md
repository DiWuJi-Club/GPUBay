# Spark Cloud — 子项目：管理后台 API 与外部插件（计划 4）设计文档

- 日期：2026-10-15
- 状态：待评审
- 依赖：计划 1（后端核心闭环，分支 `feat/plan1-backend-core`）
- 总设计：`docs/superpowers/specs/2026-10-14-spark-cloud-mvp-design.md`

## 0. 目标与范围

让运营团队能用后台管理整个平台，并让真实的钱和短信流转起来：
- 管理后台 API（仅内网端口）：管理员与 TOTP、仪表盘、机器 / PDU、实例售后、用户与企业认证、订单流水导出、价格镜像、告警、审计。
- 用户在线充值：微信 Native、支付宝当面付扫码；后台手动入账。
- 阿里云短信、阿里云三要素实名替换模拟插件。
- PDU：通用 SNMP 驱动 + 模拟驱动。
- 文件上传（营业执照），存储在平台服务器本地磁盘。
- 收尾计划 1 遗留中归属本计划的事项（见 §9）。

**完成标准**：没有任何真实密钥时，所有功能可用模拟插件 / 沙箱跑通全部自动化测试；拿到密钥后只改配置即可上线。

**不包含**：前端界面（计划 5）、Agent 与机器指令投递（计划 2）、SSH / Web 接入（计划 3）、余额提现或原路退款、告警推送（企业微信 / 飞书 / 钉钉）。

### 0.1 已确认的决策

1. 管理员登录：账号密码 + TOTP 动态码。
2. 文件存储：平台服务器本地磁盘（代码保留存储接口，以后可换 OSS）。
3. 余额不可提现；特殊情况财务线下退款后在后台扣减余额。
4. 告警不推送，只在后台列表展示。
5. 后台 API 与用户 API 同一进程、不同端口：用户 `:8080`（公网），后台 `:8081`（只监听内网地址）。
6. 后台不提供「强制停机」，用「退款并释放」「换机」代替。

## 1. 架构

```
公网 :8080  ── 用户 API（计划 1）
            ── POST /api/v1/recharges、GET /api/v1/recharges/{id}、GET /api/v1/recharges
            ── POST /api/v1/uploads
            ── POST /api/v1/pay/notify/{wechat|alipay}（支付回调，无会话）
内网 :8081  ── /admin/api/v1/*（管理员会话 + TOTP + 角色）
platform 进程内：identity / wallet / catalog / fleet / rental（已有）
                + admin（管理员、会话、TOTP、权限）
                + billing（充值单、支付回调、补查、关单）
                + storage（本地磁盘文件）
                + plugins/{sms/aliyun, kyc/aliyun, pay/{wechat,alipay,mock}, pdu/{snmp,mock}}
```

新增包：
- `internal/admin`：管理员账号、登录、会话、TOTP、恢复码、角色校验。
- `internal/billing`：充值单与到账（依赖 wallet.Apply、pay.Provider）。
- `internal/storage`：`Store` 接口（Put / Open / Delete）+ `LocalDisk` 实现；`uploads` 表记录归属。
- `internal/adminapi`：后台 HTTP 路由与处理器（与用户端 `internal/api` 分开）。
- `internal/plugins/pay`、`internal/plugins/pdu`；`internal/plugins/sms/aliyun.go`、`internal/plugins/kyc/aliyun.go`。
- `cmd/platform` 增加子命令 `admin create` / `admin reset-totp`。

模块间仍只通过 Go 接口调用，不跨模块读写对方的表；后台查询需要的跨模块数据通过各模块新增的查询方法提供。

## 2. 管理员、TOTP 与权限

**表**
- `admins`：id、username（唯一）、password_hash（argon2id）、totp_secret_enc（AES-GCM，用已有 secretbox）、totp_confirmed（bool）、role（super / ops / finance）、status（active / disabled）、failed_attempts、locked_until、last_login_at、last_login_ip、created_at、updated_at。
- `admin_recovery_codes`：admin_id、code_hash、used_at。
- `admin_sessions`：token_hash、admin_id、stage（password_ok / active）、created_at、last_seen_at、expires_at、ip。
- `admin_totp_used`：admin_id、step（TOTP 时间片编号），用于防止同一动态码重复使用（只保留最近 10 分钟）。

**初始化**：`platform admin create --username root --role super` 交互输入密码（≥12 位），终端输出 TOTP 的 otpauth URI（及终端二维码）和 8 个恢复码。系统不存在默认账号。

**登录**（两步）：
1. `POST /admin/api/v1/auth/login {username, password}` → 校验成功后得到 stage=password_ok 的临时会话（5 分钟有效）。若尚未绑定 TOTP，返回 `need_totp_setup` 并附 otpauth URI，必须先调用 `POST /auth/totp/setup {code}` 完成绑定。
2. `POST /admin/api/v1/auth/totp {code}`（或 `{recovery_code}`）→ 会话升为 active。

**规则**：
- 连续 5 次失败（密码或动态码）锁定 15 分钟；同一 IP 每分钟最多 20 次登录请求。
- 动态码 30 秒一步，允许前后各 1 步偏差，同一步不能重复使用。
- 会话：绝对有效期 12 小时，空闲 2 小时失效；Cookie 名 `sc_admin`，HttpOnly、SameSite=Strict，生产环境 Secure。
- **高风险操作需再次输入动态码**（请求体带 `totp` 字段，在 5 分钟内的同一会话中验证）：手动加减余额、手动入账、退款并释放、修改价格、增删管理员与改角色、重置他人 TOTP。
- 停用管理员时立即删除其全部会话。

**权限矩阵**

| 能力 | super | ops | finance |
|---|---|---|---|
| 仪表盘、告警（查看 / 标记处理）、审计日志查看 | ✅ | ✅ | ✅ |
| 机器、PDU、实例售后（延长、换机、重新洗机） | ✅ | ✅ | ❌ |
| 实例「退款并释放」 | ✅ | ❌ | ✅ |
| 用户查询 | ✅ | ✅ | ✅ |
| 封禁 / 解封、企业认证审核 | ✅ | ✅ | ❌ |
| 手动加减余额、手动入账、订单流水与充值导出 | ✅ | ❌ | ✅ |
| 价格与镜像 | ✅ | ❌ | ❌ |
| 管理员管理 | ✅ | ❌ | ❌ |

**审计**：所有后台修改操作写 `audit_logs`（actor_type=admin、动作、对象、前后差异、IP）。审计日志没有修改和删除接口。

## 3. 后台功能与接口（`/admin/api/v1`）

**仪表盘** `GET /dashboard`：机器按状态与在线统计、实例按状态统计、今日 / 本月充值与订单金额（分）、近 30 天每日出租率（按 machine_leases 中 active 时段与机器总数计算）。

**机器**
- `GET /machines?state=&online=&rack=&page=`、`GET /machines/{id}`（含当前实例、最近 20 个历史实例）。
- `POST /machines`（单台登记）、`POST /machines/import`（CSV：serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet；整批校验，任一行错误则整批不导入并返回逐行错误）。
- `POST /machines/{id}/maintenance {on}`：结束维护（→idle）前必须确认该机器上没有未结束的实例（creating/running/stopped/wiping），否则 `invalid_state`。
- `POST /machines/{id}/power-cycle`：调用 PDU 驱动断电重启，写审计；机器未配置 PDU 返回 `bad_request`。
- PDU：`GET/POST /pdus`、`PUT /pdus/{id}`（name、driver=snmp|mock、address、community（加密存储，列表不返回）、snmp 配置）。

**实例**
- `GET /instances?state=&user_id=&machine_id=&page=`、`GET /instances/{id}`（含事件、订单、指令）。
- `POST /instances/{id}/extend {seconds, reason}`：免费补偿。running → expires_at（或暂停中的 paused_remaining）增加；stopped / creating → pending_seconds 增加（stopped 且无未完成启动指令时下发启动）。不产生订单；写实例事件与审计。seconds 范围 60～30 天。
- `POST /instances/{id}/refund-release {amount, reason, totp}`：creating / running / stopped 可用。建议金额由 `GET /instances/{id}/refund-quote` 给出 = 剩余可用秒数（running 用剩余时长，stopped / creating 用 pending_seconds）× 最近一笔订单单价（订单金额 ÷ 订单秒数），向下取整到分。实际退款金额 ≤ 该实例所有已付订单金额减已退金额。执行：在一个事务内按订单从新到旧扣减可退额度、记 refund 流水（ref = 订单号 + 退款序号，保证幂等），订单状态改为 refunded / partially_refunded，然后实例进入洗机。
- `POST /instances/{id}/migrate {reason}`：仅 running（含暂停中）可用。在同产品的另一台空闲机器上新建实例（同用户、同镜像，pending_seconds = 原实例剩余秒数，不扣费，不新建订单，原订单通过 `instance_transfers` 关联），原实例进入洗机。没有空闲机器返回 `no_capacity`。
- `POST /instances/{id}/rewipe`：仅 wiping 可用，取消该实例未完成指令后重新下发洗机。
- 机器为 faulty 时 rewipe 仍下发（由运维确认机器已修复后操作）。

**用户**
- `GET /users?q=&kyc_status=&status=&page=`（q 匹配手机号或用户 ID）、`GET /users/{id}`（余额、实例、订单、最近流水、实名记录；身份证号只显示前 3 后 4 位）。
- `POST /users/{id}/ban {reason}`、`POST /users/{id}/unban`：封禁时删除该用户全部会话；运行中实例不受影响。
- `GET /kyc/pending`、`GET /kyc/{id}`、`POST /kyc/{id}/review {approve, reason}`；营业执照文件通过 `GET /files/{key}` 下载（需管理员会话）。
- `POST /users/{id}/adjust {amount, remark, totp}`：调账（正负均可，记为 admin_adjust）。
- `POST /users/{id}/manual-recharge {amount, remark, totp}`：手动入账（记为 recharge，channel=manual）。

**订单 / 流水 / 充值**：`GET /orders`、`GET /ledger`、`GET /recharges`（按时间、用户、类型、状态筛选，分页）；`GET /orders/export`、`GET /ledger/export`、`GET /recharges/export` 导出 CSV（UTF-8 带 BOM，金额以元两位小数输出，单次最多 10 万行，超过要求缩小时间范围）。

**价格与镜像**：`GET /products`、`PUT /products/{id}/prices/{duration} {amount, enabled, totp}`；`GET/POST /images`、`PUT /images/{id}`（name、ref、description、enabled、sort）。

**告警**：`GET /alerts?resolved=&page=`、`POST /alerts/{id}/resolve`。

**审计**：`GET /audit?actor_id=&action=&target_type=&target_id=&from=&to=&page=`。

**管理员**：`GET /admins`、`POST /admins {username, password, role, totp}`、`PUT /admins/{id} {role, status, totp}`、`POST /admins/{id}/reset-totp {totp}`、`POST /me/password {old, new}`。不能停用或降级最后一个启用中的 super。

**分页**：统一 `page`（从 1 开始）+ `page_size`（默认 20，最大 100），响应 `{items, total}`。

## 4. 充值与支付回调

**表** `recharges`：id（`rch-` + 12 位）、user_id、channel（wechat / alipay / manual）、amount（分）、status（pending / paid / closed）、qr_code、channel_trade_no、operator_id（manual 时）、remark、expires_at、paid_at、closed_at、created_at、updated_at。

**用户接口**（公网端口，需用户会话、需实名）：
- `POST /api/v1/recharges {amount, channel}`：amount 100～100,000,000 分；同一用户同时最多 5 张 pending；返回 `{id, qr_code, expires_at}`。二维码 15 分钟有效。
- `GET /api/v1/recharges/{id}`（仅本人）、`GET /api/v1/recharges?page=`。

**到账流程**（核心约束：不丢账、不重复入账）：
1. 回调 `POST /api/v1/pay/notify/{channel}`：插件验签（微信先解密 resource）→ 得到 out_trade_no、channel_trade_no、amount、是否成功。
2. 在一个事务内：锁定充值单；状态非 pending 且已 paid → 直接回复成功（幂等）；金额不一致 → 写告警，回复失败；否则 status=paid、记 channel_trade_no、paid_at，`wallet.Apply(kind=recharge, ref_type=recharge, ref_id=充值单号)`。
3. closed 状态的单收到成功回调（用户在关单前一刻付了款）：照常入账并改为 paid，写一条 warning 告警供核对。
4. 验签失败：不写库，回复失败，记日志（不写告警，避免被刷）。

**补查与关单**（加入清扫器，每分钟）：pending 且创建 1 分钟～2 小时的单调用 `Query`，已支付则走第 2 步；超过 2 小时的单调用 `Close` 并置 closed。

**插件接口**：
```go
type Provider interface {
    Create(ctx context.Context, r Recharge) (qrCode string, err error)
    ParseNotify(req *http.Request) (Notification, error)
    Query(ctx context.Context, outTradeNo string) (Notification, error)
    Close(ctx context.Context, outTradeNo string) error
    NotifyAck(w http.ResponseWriter, ok bool) // 按渠道要求的格式回复
}
type Notification struct { OutTradeNo, ChannelTradeNo string; Amount int64; Paid bool }
```
- wechat：APIv3 Native（`github.com/wechatpay-apiv3/wechatpay-go`）。
- alipay：当面付 precreate（`github.com/smartwalle/alipay/v3`）。
- mock：Create 返回 `mock://pay/{id}`；仅非生产环境注册 `POST /api/v1/dev/recharges/{id}/pay` 模拟支付成功（走与回调相同的入账函数）。

## 5. 阿里云短信与实名

- **短信**：阿里云短信服务 SendSms。配置：AccessKey ID / Secret、签名、模板编号（登录验证码 `login_code` 参数 code；到期提醒 `expiry_reminder` 参数 instance、hours）。接口返回非 OK 视为发送失败（计划 1 已处理失败时删除验证码记录）。
- **个人实名**：阿里云「运营商三要素核验」（号码百科 Dytnsapi ThreeElementsVerification）。配置：AccessKey、AuthCode。返回「一致」为通过，「不一致」为不通过并给出原因；其他（欠费、服务异常）视为调用失败。具体产品名称和返回码在拿到账号开通后核对，插件内部隔离。
- 两个插件都通过可配置的 endpoint 实现，测试用 httptest 模拟阿里云返回。
- **实名并发修正**（计划 1 遗留）：个人实名在事务中先 `SELECT … FOR UPDATE` 锁用户行，再检查状态与当日次数并插入一条 `pending` 记录占位，提交后调用核验接口，最后更新记录结果；核验接口报错时记录为 `error` 结果并计入当日次数。这样并发提交只会调用一次付费接口。

## 6. PDU

```go
type Driver interface {
    PowerCycle(ctx context.Context, outlet int) error
    Status(ctx context.Context, outlet int) (on bool, err error)
}
```
- snmp（`github.com/gosnmp/gosnmp`，SNMP v2c）：每台 PDU 配置控制 OID 前缀与取值，默认按 APC PowerNet MIB：`1.3.6.1.4.1.318.1.1.4.4.2.1.3.<outlet>`，重启值 3，状态读同一 OID（1=on）。其他品牌在 PDU 配置里改 OID 与取值。
- mock：记录调用，供测试与开发。
- 断电重启只由管理员手动触发；不自动执行。

## 7. 文件上传

- `POST /api/v1/uploads`（用户会话，multipart 字段 `file`）：≤10MB；按文件内容判断类型，仅允许 jpg / png / pdf；保存到 `SPARK_UPLOAD_DIR/yyyy/mm/<随机32位>.<ext>`；`uploads` 表记录 key、user_id、size、content_type、purpose（license）、created_at；返回 `{key}`。
- 企业认证提交时 `license_file` 必须是本人上传、purpose=license 的 key，否则 `bad_request`。
- 后台 `GET /admin/api/v1/files/{key}` 读取文件（需管理员会话），响应带 `Content-Disposition: inline` 与 `X-Content-Type-Options: nosniff`。
- 部署说明：上传目录需要定期备份。

## 8. 配置

新增环境变量：`SPARK_ADMIN_ADDR`（默认 `127.0.0.1:8081`）、`SPARK_UPLOAD_DIR`（默认 `./.devdata/uploads`）、`SPARK_PUBLIC_BASE_URL`（支付回调地址前缀，生产必填）、`SPARK_PAY_DRIVER`（mock / real，real 时按已配置的渠道启用微信和 / 或支付宝）、微信（`SPARK_WECHAT_MCH_ID`、`_APP_ID`、`_API_V3_KEY`、`_CERT_SERIAL`、`_PRIVATE_KEY_PATH`）、支付宝（`SPARK_ALIPAY_APP_ID`、`_PRIVATE_KEY_PATH`、`_PUBLIC_KEY_PATH`、`_SANDBOX`）、阿里云（`SPARK_ALIYUN_ACCESS_KEY_ID`、`_SECRET`、`SPARK_SMS_SIGN`、`SPARK_SMS_TPL_LOGIN`、`SPARK_SMS_TPL_EXPIRY`、`SPARK_KYC_AUTH_CODE`）、`SPARK_PDU_DRIVER`（mock / snmp）。生产环境禁止 mock 的规则扩展到支付与 PDU。

## 9. 本计划认领的计划 1 遗留事项

- 实名核验并发与失败计次（§5）。
- 洗机失败后的处理：后台「重新洗机」（§3）。
- 启动失败后的已付续费：后台「退款并释放」（§3）。
- 结束维护前检查机器上没有未结束实例（§3）。
- 登记机器时未知商品、写指令时未知机器返回业务错误而不是 500。
- 企业认证营业执照改为上传 key（§7）。

## 10. 测试

- admin：密码哈希、两步登录、TOTP 时间窗与防重放、锁定、恢复码一次性、空闲与绝对过期、高风险操作二次验证、权限矩阵（表驱动测试逐个接口 × 角色）、最后一个 super 保护。
- billing：创建充值单的校验与上限；回调验签失败 / 金额不一致 / 重复回调 / 并发回调只入账一次；关单后到账；补查入账；关单。
- 支付插件：微信用官方 SDK 的验签逻辑配合测试证书生成签名后的回调；支付宝用测试密钥对签名；均不访问外网。
- 阿里云插件：httptest 模拟成功、失败、异常返回。
- PDU：snmp 驱动对一个本地 SNMP 测试代理（gosnmp 提供的测试服务器或自建 UDP 应答）验证 OID 与取值。
- 后台售后：延长（各状态）、退款并释放（建议金额计算、上限、部分退款、幂等）、换机（时长转移、无空闲机器、原实例洗机）、重新洗机。
- 上传：大小与类型限制、伪造扩展名、他人 key 被拒。
- 端到端：管理员登录 → 登记机器 → 用户 mock 充值到账 → 下单 → 后台退款并释放 → 流水与审计正确。
