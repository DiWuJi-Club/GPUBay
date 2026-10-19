# Spark Cloud 计划 4：管理后台 API、支付、阿里云、PDU 与上传 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在计划 1 的控制面上加入内网管理后台 API（管理员 + TOTP + 角色）、用户在线充值（微信 Native / 支付宝当面付 / 后台手动入账）、阿里云短信与三要素实名、SNMP PDU 断电重启、营业执照上传，并收尾计划 1 中归属本计划的遗留事项。没有任何真实密钥时，全部功能可用 mock 插件跑通自动化测试；拿到密钥后只改配置即可上线。

**Architecture:** 仍是模块化单体。新增 `admin`（管理员、会话、TOTP）、`billing`（充值单与到账）、`storage`（本地磁盘文件 + uploads 表）三个业务包，`adminapi`（后台 HTTP，独立端口 `:8081`）一个接口包，以及 `plugins/{aliyun,pay,pdu}` 插件包。所有后台修改操作在业务事务内写 `audit_logs`（通过 `audit.Actor`）。外部服务一律藏在插件接口后面：测试用 httptest / 本地 UDP 代理 / 官方 SDK 的验签逻辑配合测试密钥，不访问外网。

**Tech Stack:** Go 1.27（`~/.local/go/bin`），chi v5，pgx v5，goose v3，embedded-postgres；新增 `golang.org/x/crypto/argon2`、`golang.org/x/term`、`github.com/mdp/qrterminal/v3`、`github.com/wechatpay-apiv3/wechatpay-go`、`github.com/smartwalle/alipay/v3`、`github.com/smartwalle/nsign`（仅测试签名）、`github.com/gosnmp/gosnmp`。TOTP（RFC 6238）与阿里云 RPC 签名用标准库自行实现。

**Spec:** `docs/superpowers/specs/2026-10-15-plan4-admin-payments-design.md`（总设计：`docs/superpowers/specs/2026-10-14-spark-cloud-mvp-design.md`）

**分支：** `feat/plan4-admin-payments`（从 `feat/plan1-backend-core` 拉出，已存在，设计文档已提交 f0dc5b8）。

**本计划不包含：** 前端（计划 5）、Agent 与指令投递（计划 2）、SSH / Web 接入（计划 3）、提现与原路退款、告警推送。

## Global Constraints

- Go 模块名 `spark-cloud`；Go 1.27；所有命令前置 `export PATH=$HOME/.local/go/bin:$PATH`；在仓库根目录 `/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud` 执行。
- 金额一律 `int64`，单位「分」；不允许出现浮点金额（支付宝的元字符串用整数解析）。CSV 导出金额以「元、两位小数」输出。
- 时间一律通过 `clock.Clock` 获取并以 UTC 写入数据库；SQL 中不使用 `now()`。
- 面向用户 / 管理员的错误消息为中文，通过 `apperr.Error{Code, Message, Status}` 返回；未知错误统一 `internal` / 500 并记日志。
- 生产环境（`SPARK_ENV=prod`）禁止 mock 插件：短信、实名、支付、PDU 四类都适用（加载配置时与组装服务时都检查）。
- 系统不存在默认管理员；只能用 `platform admin create` 创建第一个。
- 敏感字段一律用已有 `secretbox` 加密存储：身份证号、TOTP 密钥、SNMP community。接口与日志不输出明文。
- 后台 API 与用户 API 同一进程、不同端口：用户 `SPARK_HTTP_ADDR`（默认 `:8080`），后台 `SPARK_ADMIN_ADDR`（默认 `127.0.0.1:8081`）。
- 后台所有修改操作写 `audit_logs`（`actor_type=admin`，CLI 操作为 `system`），与业务修改在同一事务内；审计日志没有修改和删除接口。
- 高风险操作（手动加减余额、手动入账、退款并释放、修改价格、新增管理员、修改管理员角色或状态、重置他人 TOTP）请求体带 `totp` 字段做二次验证。
- 后台分页统一 `page`（从 1 开始）+ `page_size`（默认 20，最大 100），响应 `{items, total}`。
- 模块不直接读写其他模块的表（测试夹具 `dbtest` 除外）；跨模块数据通过对方 Service 的方法获取。
- 加锁顺序沿用计划 1：`instance → agent_commands → orders → wallet`；`machine → wallet`。新增：`recharge → wallet`；`admins → admin_sessions`；`instance → machine`（换机分配新机器）。
- 每个任务结束时 `go vet ./... && go test ./...` 必须全部通过再提交；提交信息结尾加 `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`。

## Review Focus

1. **支付回调带着本系统不存在的商户单号**（例如在商户平台做回调测试、或同一商户号下其他系统的订单）：不入账、不崩溃、回复失败，并记日志 → Task 8 `TestSettleUnknownTradeIsRejected`。
2. **支付宝金额字符串的边界写法**（`"100"`、`"0.1"`、`"1.005"`、`"-1.00"`、`"1,00"`）：合法写法精确换算为分，非法写法拒绝而不是四舍五入 → Task 10 `TestYuanToFen`。
3. **管理员输入带空格的动态码**（验证器 App 显示为 `123 456`）：去掉空白后正常校验 → Task 2 `TestMatchTOTPIgnoresSpaces`。
4. **CSV 导出里以 `=`、`+`、`@` 开头的备注或用户输入**：在 Excel 中不能被当成公式执行；负数金额不受影响 → Task 1 `TestWriterEscapesFormulas`。
5. **对换过机的实例做退款并释放**：已付订单挂在被换掉的原实例上，建议金额与可退上限必须沿换机链路统计 → Task 14 `TestRefundAfterMigrationUsesOriginalOrders`。

## 已做出的裁定（规格未写明的细节）

- **微信支付验签方式**：使用「微信支付公钥」模式（新商户默认，启动时不需要联网下载平台证书）。因此在规格 §8 的变量之外新增 `SPARK_WECHAT_PUBLIC_KEY_ID`、`SPARK_WECHAT_PUBLIC_KEY_PATH`。
- **阿里云变量名**：AccessKey Secret 为 `SPARK_ALIYUN_ACCESS_KEY_SECRET`；另加可选 `SPARK_ALIYUN_SMS_ENDPOINT`（默认 `https://dysmsapi.aliyuncs.com`）、`SPARK_ALIYUN_KYC_ENDPOINT`（默认 `https://dytnsapi.aliyuncs.com`），测试时指向 httptest。
- **「同一用户最多 5 张 pending」**：只统计二维码尚未过期（15 分钟内）的 pending 单，避免用户放弃支付后被锁 2 小时。
- **高风险操作二次验证**：请求体 `totp` 非空时校验动态码（防重放），成功后记录 `admin_sessions.stepup_at`；同一会话 5 分钟内的后续高风险操作可以不带 `totp`。登录本身不算二次验证。
- **登录 IP 限流（每分钟 20 次）**：在 `adminapi` 进程内存中按 IP 计数（后台只有内网访问、单实例部署）。作用于 `/auth/login`、`/auth/totp`、`/auth/totp/setup`。
- **个人实名并发**：事务内锁用户行，2 分钟内存在 `pending` 个人记录则返回 `kyc_in_progress`；超过 2 分钟仍为 `pending` 的记录视为中断，改为 `error` 并计入当日次数。
- **管理员 TOTP 绑定时机**：所有管理员（CLI 创建或后台创建）都在首次登录时绑定 TOTP，恢复码在绑定成功时只展示给本人一次。CLI `admin create` 在终端里当场完成绑定（显示 otpauth URI 与二维码，要求输入一次动态码确认），然后打印 8 个恢复码。
- **finance 角色可查看实例列表与详情**（用于退款），但不能执行延长 / 换机 / 重新洗机。
- **`SPARK_PDU_DRIVER`**：`mock` 时所有 PDU 一律走 mock 驱动（开发默认）；`snmp` 时按每台 PDU 记录的 `driver` 字段选择，且生产环境不允许创建 `driver=mock` 的 PDU。
- **清扫器**：`rental.RunSweeper` 增加可选的附加任务参数，充值补查与关单作为附加任务在同一个咨询锁内每分钟执行。

## 文件结构

```
spark-cloud/
├── cmd/platform/main.go              # 修改：admin 子命令分发；同时启动用户与后台两个 HTTP 服务
├── cmd/platform/admin.go             # 新增：platform admin create / reset-totp
├── migrations/
│   ├── 00009_admin.sql               # admins、admin_recovery_codes、admin_sessions、admin_totp_used
│   ├── 00010_uploads.sql             # uploads
│   ├── 00011_kyc_error.sql           # kyc_records.result 增加 error
│   ├── 00012_recharges.sql           # recharges
│   ├── 00013_pdus.sql                # pdus；machines.pdu_id / pdu_outlet
│   ├── 00014_instance_transfers.sql  # 换机关联
│   ├── 00015_refunds.sql             # orders.refunded_amount / refund_count
│   └── 00016_admin_indexes.sql       # 后台列表查询用索引
├── internal/
│   ├── platform/config/              # 修改：新增后台、上传、支付、阿里云、PDU 配置
│   ├── platform/db/                  # 修改：IsForeignKeyViolation
│   ├── platform/paging/              # 新增：page / page_size 解析
│   ├── platform/csvx/                # 新增：带 BOM 的 CSV、元格式、防公式注入
│   ├── audit/                        # 修改：Actor；新增 List（Task 19）
│   ├── admin/                        # 新增：密码、TOTP、恢复码、登录、会话、二次验证、管理员管理
│   ├── storage/                      # 新增：Store 接口、LocalDisk、uploads 服务
│   ├── billing/                      # 新增：充值单、回调到账、补查关单、手动入账
│   ├── plugins/aliyun/               # 新增：阿里云 RPC 签名客户端
│   ├── plugins/sms/aliyun.go         # 新增：阿里云短信
│   ├── plugins/kyc/aliyun.go         # 新增：阿里云三要素
│   ├── plugins/pay/                  # 新增：Provider 接口、mock、wechat、alipay
│   ├── plugins/pdu/                  # 新增：Driver 接口、snmp、mock
│   ├── identity/                     # 修改：实名并发、营业执照 key、后台查询、封禁
│   ├── wallet/                       # 修改：后台流水查询与导出
│   ├── catalog/                      # 修改：后台价格与镜像管理
│   ├── fleet/                        # 修改：外键错误映射、PDU、机器后台查询与导入、告警查询
│   ├── rental/                       # 修改：延长、重新洗机、维护检查、换机、退款并释放、后台查询、清扫附加任务
│   ├── api/                          # 修改：上传、充值、支付回调、开发环境模拟支付
│   ├── adminapi/                     # 新增：后台 HTTP（认证、权限、各功能路由）
│   └── app/                          # 修改：组装新服务与插件，输出后台 Handler
```

## 任务依赖

Task 1 → 2 → 3 → 4（管理员）；5 → 6（上传 → 实名）；7（阿里云）；8 → 9 → 10（充值）；11（PDU）；12 → 13 → 14（实例售后）；15 → 16 → 17 → 18 → 19 → 20（后台 API，依赖前面全部业务方法）；21（组装、权限矩阵、端到端、文档）。按编号顺序执行，共 21 个任务。

---
### Task 1: 基础设施：配置、分页、CSV、审计操作人、外键错误映射

**Files:**
- Modify: `internal/platform/config/config.go`（整体替换）
- Modify: `internal/platform/config/config_test.go`（追加测试）
- Modify: `internal/platform/db/db.go`（追加 `IsForeignKeyViolation`）
- Create: `internal/platform/paging/paging.go`、`internal/platform/paging/paging_test.go`
- Create: `internal/platform/csvx/csvx.go`、`internal/platform/csvx/csvx_test.go`
- Modify: `internal/audit/audit.go`（追加 `Actor`）
- Create: `internal/audit/audit_test.go`
- Modify: `internal/fleet/machines.go`（`Register` 外键错误映射）
- Modify: `internal/fleet/commands.go`（`Enqueue` 外键错误映射）
- Modify: `internal/fleet/machines_test.go`、`internal/fleet/commands_test.go`（追加测试）

**Interfaces:**
- Consumes: 计划 1 的 `apperr`、`db`、`audit.Entry`、`fleet.Service`。
- Produces:
  - `config.Config` 新字段：`AdminAddr string`、`PublicBaseURL string`、`PayDriver string`（mock/real）、`PDUDriver string`（mock/snmp）、`UploadDir string`、`Aliyun AliyunConfig`、`Wechat WechatConfig`、`Alipay AlipayConfig`；`WechatConfig.Enabled() bool`、`AlipayConfig.Enabled() bool`。
  - `db.IsForeignKeyViolation(err error) bool`
  - `paging.Page{Number, Size int}`、`paging.Parse(q url.Values) (paging.Page, error)`、`Page.Normalize() Page`、`Page.Limit() int`、`Page.Offset() int`、常量 `paging.DefaultSize = 20`、`paging.MaxSize = 100`。
  - `csvx.MaxRows = 100_000`、`csvx.ErrTooLarge`、`csvx.NewWriter(w io.Writer) (*csvx.Writer, error)`、`(*Writer).Write([]string) error`、`(*Writer).Flush() error`、`csvx.Yuan(fen int64) string`、`csvx.Time(t time.Time) string`、`csvx.TimePtr(t *time.Time) string`。
  - `audit.Actor{AdminID int64; IP string}`、`(audit.Actor).Entry(action, targetType, targetID string, diff any) audit.Entry`（AdminID 为 0 时 actor_type=system）。
  - `fleet.Register` 对不存在的商品返回 `bad_request`「商品不存在」；`fleet.Enqueue` 对不存在的机器返回 `bad_request`「机器不存在」。

- [ ] **Step 1: 写配置的失败测试**

在 `internal/platform/config/config_test.go` 末尾追加：

```go
func TestLoadPlan4Defaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "postgres://x", "SPARK_SECRET_KEY": validKey}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminAddr != "127.0.0.1:8081" || c.UploadDir != "./.devdata/uploads" || c.PayDriver != "mock" || c.PDUDriver != "mock" ||
		c.PublicBaseURL != "" || c.Aliyun.SMSEndpoint != "https://dysmsapi.aliyuncs.com" ||
		c.Aliyun.KYCEndpoint != "https://dytnsapi.aliyuncs.com" || c.Wechat.Enabled() || c.Alipay.Enabled() {
		t.Fatalf("config = %+v", c)
	}
}

// prodEnv 返回一套完整、合法的生产配置；各测试在其上增删变量。
func prodEnv() map[string]string {
	return map[string]string{
		"SPARK_ENV": "prod", "SPARK_DATABASE_URL": "postgres://x", "SPARK_SECRET_KEY": validKey,
		"SPARK_SMS_DRIVER": "aliyun", "SPARK_KYC_DRIVER": "aliyun", "SPARK_PAY_DRIVER": "real", "SPARK_PDU_DRIVER": "snmp",
		"SPARK_ALIYUN_ACCESS_KEY_ID": "ak", "SPARK_ALIYUN_ACCESS_KEY_SECRET": "sk",
		"SPARK_SMS_SIGN": "Spark", "SPARK_SMS_TPL_LOGIN": "SMS_1", "SPARK_SMS_TPL_EXPIRY": "SMS_2", "SPARK_KYC_AUTH_CODE": "auth",
		"SPARK_PUBLIC_BASE_URL": "https://spark.example.com/",
		"SPARK_WECHAT_MCH_ID": "1900000001", "SPARK_WECHAT_APP_ID": "wx123", "SPARK_WECHAT_API_V3_KEY": "0123456789abcdef0123456789abcdef",
		"SPARK_WECHAT_CERT_SERIAL": "SERIAL", "SPARK_WECHAT_PRIVATE_KEY_PATH": "/etc/spark/wx.pem",
		"SPARK_WECHAT_PUBLIC_KEY_ID": "PUB_KEY_ID_1", "SPARK_WECHAT_PUBLIC_KEY_PATH": "/etc/spark/wxpub.pem",
		"SPARK_ALIPAY_APP_ID": "2021000000000001", "SPARK_ALIPAY_PRIVATE_KEY_PATH": "/etc/spark/ali.pem",
		"SPARK_ALIPAY_PUBLIC_KEY_PATH": "/etc/spark/alipub.pem", "SPARK_ALIPAY_SANDBOX": "false",
	}
}

func TestLoadProdFullyConfigured(t *testing.T) {
	c, err := config.Load(env(prodEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicBaseURL != "https://spark.example.com" || !c.Wechat.Enabled() || !c.Alipay.Enabled() || c.Alipay.Sandbox ||
		c.Wechat.PublicKeyID != "PUB_KEY_ID_1" || c.Aliyun.KYCAuthCode != "auth" {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadPluginErrors(t *testing.T) {
	cases := []struct {
		name   string
		change map[string]string // 空字符串表示删除该变量
		want   string
	}{
		{"prod pay mock", map[string]string{"SPARK_PAY_DRIVER": "mock"}, "mock"},
		{"prod pdu mock", map[string]string{"SPARK_PDU_DRIVER": "mock"}, "mock"},
		{"bad pay driver", map[string]string{"SPARK_PAY_DRIVER": "stripe"}, "SPARK_PAY_DRIVER"},
		{"bad pdu driver", map[string]string{"SPARK_PDU_DRIVER": "ipmi"}, "SPARK_PDU_DRIVER"},
		{"bad sms driver", map[string]string{"SPARK_SMS_DRIVER": "tencent"}, "SPARK_SMS_DRIVER"},
		{"sms missing sign", map[string]string{"SPARK_SMS_SIGN": ""}, "SPARK_SMS_SIGN"},
		{"kyc missing auth", map[string]string{"SPARK_KYC_AUTH_CODE": ""}, "SPARK_KYC_AUTH_CODE"},
		{"aliyun missing secret", map[string]string{"SPARK_ALIYUN_ACCESS_KEY_SECRET": ""}, "SPARK_ALIYUN_ACCESS_KEY_SECRET"},
		{"pay real http", map[string]string{"SPARK_PUBLIC_BASE_URL": "http://spark.example.com"}, "SPARK_PUBLIC_BASE_URL"},
		{"pay real no channel", map[string]string{"SPARK_WECHAT_MCH_ID": "", "SPARK_ALIPAY_APP_ID": ""}, "SPARK_WECHAT_MCH_ID"},
		{"wechat partial", map[string]string{"SPARK_WECHAT_PRIVATE_KEY_PATH": ""}, "SPARK_WECHAT_PRIVATE_KEY_PATH"},
		{"wechat bad v3 key", map[string]string{"SPARK_WECHAT_API_V3_KEY": "short"}, "SPARK_WECHAT_API_V3_KEY"},
		{"alipay partial", map[string]string{"SPARK_ALIPAY_PUBLIC_KEY_PATH": ""}, "SPARK_ALIPAY_PUBLIC_KEY_PATH"},
		{"alipay bad sandbox", map[string]string{"SPARK_ALIPAY_SANDBOX": "maybe"}, "SPARK_ALIPAY_SANDBOX"},
	}
	for _, c := range cases {
		m := prodEnv()
		for k, v := range c.change {
			if v == "" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		_, err := config.Load(env(m))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %s", c.name, err, c.want)
		}
	}
}

func TestLoadPayRealWithOnlyAlipay(t *testing.T) {
	m := prodEnv()
	for _, k := range []string{"SPARK_WECHAT_MCH_ID", "SPARK_WECHAT_APP_ID", "SPARK_WECHAT_API_V3_KEY", "SPARK_WECHAT_CERT_SERIAL",
		"SPARK_WECHAT_PRIVATE_KEY_PATH", "SPARK_WECHAT_PUBLIC_KEY_ID", "SPARK_WECHAT_PUBLIC_KEY_PATH"} {
		delete(m, k)
	}
	c, err := config.Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.Wechat.Enabled() || !c.Alipay.Enabled() {
		t.Fatalf("wechat=%v alipay=%v", c.Wechat.Enabled(), c.Alipay.Enabled())
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `export PATH=$HOME/.local/go/bin:$PATH && go test ./internal/platform/config/`
Expected: 编译失败，提示 `c.AdminAddr undefined` 等。

- [ ] **Step 3: 实现配置**

用以下内容整体替换 `internal/platform/config/config.go`：

```go
// Package config 从环境变量读取 platform 配置。
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Config struct {
	Env           string // dev / staging / prod
	HTTPAddr      string // 用户 API（公网）
	AdminAddr     string // 后台 API，只应绑定内网地址
	DatabaseURL   string
	Domain        string
	PublicBaseURL string // 支付回调地址前缀，例如 https://spark.example.com（无结尾斜杠）
	SecretKey     []byte // 32 字节，用于加密身份证号、实例密码、TOTP 密钥与 SNMP community
	SMSDriver     string // mock / aliyun
	KYCDriver     string // mock / aliyun
	PayDriver     string // mock / real
	PDUDriver     string // mock / snmp
	UploadDir     string
	SweepInterval time.Duration
	TrustProxy    bool // 仅当部署在会改写 X-Forwarded-For 的反向代理之后才设为 true
	Aliyun        AliyunConfig
	Wechat        WechatConfig
	Alipay        AlipayConfig
}

// AliyunConfig 阿里云短信与号码百科（三要素核验）共用一组 AccessKey。
type AliyunConfig struct {
	AccessKeyID     string
	AccessKeySecret string
	SMSSign         string
	SMSTplLogin     string
	SMSTplExpiry    string
	KYCAuthCode     string
	SMSEndpoint     string
	KYCEndpoint     string
}

// WechatConfig 微信支付 APIv3 Native 扫码，使用「微信支付公钥」验签。MchID 非空即视为启用。
type WechatConfig struct {
	MchID          string
	AppID          string
	APIv3Key       string
	CertSerial     string
	PrivateKeyPath string
	PublicKeyID    string
	PublicKeyPath  string
}

func (w WechatConfig) Enabled() bool { return w.MchID != "" }

// AlipayConfig 支付宝当面付。AppID 非空即视为启用。
type AlipayConfig struct {
	AppID          string
	PrivateKeyPath string // 应用私钥
	PublicKeyPath  string // 支付宝公钥
	Sandbox        bool
}

func (a AlipayConfig) Enabled() bool { return a.AppID != "" }

func Load(getenv func(string) string) (Config, error) {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Env:           or("SPARK_ENV", "dev"),
		HTTPAddr:      or("SPARK_HTTP_ADDR", ":8080"),
		AdminAddr:     or("SPARK_ADMIN_ADDR", "127.0.0.1:8081"),
		DatabaseURL:   getenv("SPARK_DATABASE_URL"),
		Domain:        or("SPARK_DOMAIN", "spark.local"),
		PublicBaseURL: strings.TrimRight(getenv("SPARK_PUBLIC_BASE_URL"), "/"),
		SMSDriver:     or("SPARK_SMS_DRIVER", "mock"),
		KYCDriver:     or("SPARK_KYC_DRIVER", "mock"),
		PayDriver:     or("SPARK_PAY_DRIVER", "mock"),
		PDUDriver:     or("SPARK_PDU_DRIVER", "mock"),
		UploadDir:     or("SPARK_UPLOAD_DIR", "./.devdata/uploads"),
		Aliyun: AliyunConfig{
			AccessKeyID:     getenv("SPARK_ALIYUN_ACCESS_KEY_ID"),
			AccessKeySecret: getenv("SPARK_ALIYUN_ACCESS_KEY_SECRET"),
			SMSSign:         getenv("SPARK_SMS_SIGN"),
			SMSTplLogin:     getenv("SPARK_SMS_TPL_LOGIN"),
			SMSTplExpiry:    getenv("SPARK_SMS_TPL_EXPIRY"),
			KYCAuthCode:     getenv("SPARK_KYC_AUTH_CODE"),
			SMSEndpoint:     or("SPARK_ALIYUN_SMS_ENDPOINT", "https://dysmsapi.aliyuncs.com"),
			KYCEndpoint:     or("SPARK_ALIYUN_KYC_ENDPOINT", "https://dytnsapi.aliyuncs.com"),
		},
		Wechat: WechatConfig{
			MchID:          getenv("SPARK_WECHAT_MCH_ID"),
			AppID:          getenv("SPARK_WECHAT_APP_ID"),
			APIv3Key:       getenv("SPARK_WECHAT_API_V3_KEY"),
			CertSerial:     getenv("SPARK_WECHAT_CERT_SERIAL"),
			PrivateKeyPath: getenv("SPARK_WECHAT_PRIVATE_KEY_PATH"),
			PublicKeyID:    getenv("SPARK_WECHAT_PUBLIC_KEY_ID"),
			PublicKeyPath:  getenv("SPARK_WECHAT_PUBLIC_KEY_PATH"),
		},
		Alipay: AlipayConfig{
			AppID:          getenv("SPARK_ALIPAY_APP_ID"),
			PrivateKeyPath: getenv("SPARK_ALIPAY_PRIVATE_KEY_PATH"),
			PublicKeyPath:  getenv("SPARK_ALIPAY_PUBLIC_KEY_PATH"),
		},
	}
	switch c.Env {
	case "dev", "staging", "prod":
	default:
		return Config{}, fmt.Errorf("SPARK_ENV 只能是 dev、staging 或 prod，当前为 %q", c.Env)
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("SPARK_DATABASE_URL 未设置")
	}
	key, err := base64.StdEncoding.DecodeString(getenv("SPARK_SECRET_KEY"))
	if err != nil || len(key) != 32 {
		return Config{}, errors.New("SPARK_SECRET_KEY 必须是 32 字节密钥的 base64 编码（可用 openssl rand -base64 32 生成）")
	}
	c.SecretKey = key
	for _, d := range []struct{ name, value, a, b string }{
		{"SPARK_SMS_DRIVER", c.SMSDriver, "mock", "aliyun"},
		{"SPARK_KYC_DRIVER", c.KYCDriver, "mock", "aliyun"},
		{"SPARK_PAY_DRIVER", c.PayDriver, "mock", "real"},
		{"SPARK_PDU_DRIVER", c.PDUDriver, "mock", "snmp"},
	} {
		if d.value != d.a && d.value != d.b {
			return Config{}, fmt.Errorf("%s 只能是 %s 或 %s，当前为 %q", d.name, d.a, d.b, d.value)
		}
	}
	if c.Env == "prod" && (c.SMSDriver == "mock" || c.KYCDriver == "mock" || c.PayDriver == "mock" || c.PDUDriver == "mock") {
		return Config{}, errors.New("生产环境禁止使用 mock 插件（SPARK_SMS_DRIVER / SPARK_KYC_DRIVER / SPARK_PAY_DRIVER / SPARK_PDU_DRIVER）")
	}
	if c.TrustProxy, err = parseBool("SPARK_TRUST_PROXY", getenv("SPARK_TRUST_PROXY")); err != nil {
		return Config{}, err
	}
	if c.Alipay.Sandbox, err = parseBool("SPARK_ALIPAY_SANDBOX", getenv("SPARK_ALIPAY_SANDBOX")); err != nil {
		return Config{}, err
	}
	c.SweepInterval = time.Minute
	if v := getenv("SPARK_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("SPARK_SWEEP_INTERVAL 格式不正确：%q", v)
		}
		c.SweepInterval = d
	}
	if err := c.checkPlugins(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func parseBool(name, v string) (bool, error) {
	switch v {
	case "", "false", "0":
		return false, nil
	case "true", "1":
		return true, nil
	}
	return false, fmt.Errorf("%s 只能是 true、false、1 或 0，当前为 %q", name, v)
}

// checkPlugins 校验启用的真实插件所需的变量是否齐全。
func (c Config) checkPlugins() error {
	a := c.Aliyun
	if c.SMSDriver == "aliyun" {
		if err := requireAll("SPARK_SMS_DRIVER=aliyun",
			"SPARK_ALIYUN_ACCESS_KEY_ID", a.AccessKeyID, "SPARK_ALIYUN_ACCESS_KEY_SECRET", a.AccessKeySecret,
			"SPARK_SMS_SIGN", a.SMSSign, "SPARK_SMS_TPL_LOGIN", a.SMSTplLogin, "SPARK_SMS_TPL_EXPIRY", a.SMSTplExpiry); err != nil {
			return err
		}
	}
	if c.KYCDriver == "aliyun" {
		if err := requireAll("SPARK_KYC_DRIVER=aliyun",
			"SPARK_ALIYUN_ACCESS_KEY_ID", a.AccessKeyID, "SPARK_ALIYUN_ACCESS_KEY_SECRET", a.AccessKeySecret,
			"SPARK_KYC_AUTH_CODE", a.KYCAuthCode); err != nil {
			return err
		}
	}
	if c.PayDriver == "real" {
		if !strings.HasPrefix(c.PublicBaseURL, "https://") {
			return errors.New("SPARK_PAY_DRIVER=real 需要设置以 https:// 开头的 SPARK_PUBLIC_BASE_URL（支付回调地址）")
		}
		if !c.Wechat.Enabled() && !c.Alipay.Enabled() {
			return errors.New("SPARK_PAY_DRIVER=real 需要至少配置微信支付（SPARK_WECHAT_MCH_ID）或支付宝（SPARK_ALIPAY_APP_ID）")
		}
		if w := c.Wechat; w.Enabled() {
			if err := requireAll("微信支付",
				"SPARK_WECHAT_APP_ID", w.AppID, "SPARK_WECHAT_API_V3_KEY", w.APIv3Key, "SPARK_WECHAT_CERT_SERIAL", w.CertSerial,
				"SPARK_WECHAT_PRIVATE_KEY_PATH", w.PrivateKeyPath, "SPARK_WECHAT_PUBLIC_KEY_ID", w.PublicKeyID,
				"SPARK_WECHAT_PUBLIC_KEY_PATH", w.PublicKeyPath); err != nil {
				return err
			}
			if len(w.APIv3Key) != 32 {
				return errors.New("SPARK_WECHAT_API_V3_KEY 必须是 32 个字符")
			}
		}
		if p := c.Alipay; p.Enabled() {
			if err := requireAll("支付宝",
				"SPARK_ALIPAY_PRIVATE_KEY_PATH", p.PrivateKeyPath, "SPARK_ALIPAY_PUBLIC_KEY_PATH", p.PublicKeyPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// requireAll 接收「变量名, 值」成对参数，列出所有为空的变量名。
func requireAll(what string, pairs ...string) error {
	var missing []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			missing = append(missing, pairs[i])
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s 需要设置 %s", what, strings.Join(missing, "、"))
	}
	return nil
}
```

- [ ] **Step 4: 运行配置测试确认通过**

Run: `go test ./internal/platform/config/`
Expected: PASS（含计划 1 的 `TestLoadDefaults`、`TestLoadTrustProxy`、`TestLoadErrors`）。

- [ ] **Step 5: 写分页、CSV、审计操作人的失败测试**

`internal/platform/paging/paging_test.go`：

```go
package paging_test

import (
	"net/url"
	"testing"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/paging"
)

func TestParse(t *testing.T) {
	p, err := paging.Parse(url.Values{})
	if err != nil || p.Number != 1 || p.Size != 20 || p.Offset() != 0 || p.Limit() != 20 {
		t.Fatalf("default = %+v, %v", p, err)
	}
	p, err = paging.Parse(url.Values{"page": {"3"}, "page_size": {"50"}})
	if err != nil || p.Offset() != 100 || p.Limit() != 50 {
		t.Fatalf("page 3 = %+v, %v", p, err)
	}
	for _, q := range []url.Values{
		{"page": {"0"}}, {"page": {"x"}}, {"page": {"1000001"}},
		{"page_size": {"0"}}, {"page_size": {"101"}}, {"page_size": {"-5"}},
	} {
		if _, err := paging.Parse(q); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
			t.Fatalf("Parse(%v) err = %v, want bad_request", q, err)
		}
	}
}

func TestNormalize(t *testing.T) {
	if p := (paging.Page{}).Normalize(); p.Number != 1 || p.Size != 20 {
		t.Fatalf("zero page = %+v", p)
	}
	if p := (paging.Page{Number: 2, Size: 500}).Normalize(); p.Size != 100 {
		t.Fatalf("oversized page = %+v", p)
	}
}
```

`internal/platform/csvx/csvx_test.go`：

```go
package csvx_test

import (
	"bytes"
	"testing"
	"time"

	"spark-cloud/internal/platform/csvx"
)

func TestYuan(t *testing.T) {
	for fen, want := range map[int64]string{0: "0.00", 5: "0.05", 100: "1.00", 6900: "69.00", 159901: "1599.01", -1234: "-12.34", -5: "-0.05"} {
		if got := csvx.Yuan(fen); got != want {
			t.Fatalf("Yuan(%d) = %q, want %q", fen, got, want)
		}
	}
}

func TestTimeUsesBeijing(t *testing.T) {
	if got := csvx.Time(time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC)); got != "2026-01-02 00:30:00" {
		t.Fatalf("Time = %q", got)
	}
	if got := csvx.TimePtr(nil); got != "" {
		t.Fatalf("TimePtr(nil) = %q", got)
	}
}

func TestWriterWritesBOMAndRows(t *testing.T) {
	var buf bytes.Buffer
	w, err := csvx.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write([]string{"金额", "备注"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Write([]string{"-12.34", "含,逗号"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "\xEF\xBB\xBF金额,备注\n-12.34,\"含,逗号\"\n"
	if buf.String() != want {
		t.Fatalf("csv = %q, want %q", buf.String(), want)
	}
}

func TestWriterEscapesFormulas(t *testing.T) {
	var buf bytes.Buffer
	w, _ := csvx.NewWriter(&buf)
	_ = w.Write([]string{"=HYPERLINK(\"x\")", "+1", "@SUM(A1)", "-cmd", "-0.50", "\tx"})
	_ = w.Flush()
	// 含双引号的单元格按 CSV 规则整体加引号并转义内部引号
	want := "\xEF\xBB\xBF\"'=HYPERLINK(\"\"x\"\")\",'+1,'@SUM(A1),'-cmd,-0.50,'\tx\n"
	if buf.String() != want {
		t.Fatalf("csv = %q, want %q", buf.String(), want)
	}
}
```

`internal/audit/audit_test.go`：

```go
package audit_test

import (
	"testing"

	"spark-cloud/internal/audit"
)

func TestActorEntry(t *testing.T) {
	e := audit.Actor{AdminID: 7, IP: "10.0.0.1"}.Entry("machine.power_cycle", "machine", "12", map[string]int{"outlet": 3})
	if e.ActorType != "admin" || e.ActorID != 7 || e.IP != "10.0.0.1" || e.Action != "machine.power_cycle" ||
		e.TargetType != "machine" || e.TargetID != "12" || e.Diff == nil {
		t.Fatalf("entry = %+v", e)
	}
	if e := (audit.Actor{}).Entry("admin.create", "admin", "1", nil); e.ActorType != "system" || e.ActorID != 0 {
		t.Fatalf("system entry = %+v", e)
	}
}
```

- [ ] **Step 6: 运行确认失败**

Run: `go test ./internal/platform/paging/ ./internal/platform/csvx/ ./internal/audit/`
Expected: 编译失败（包或 `Actor` 不存在）。

- [ ] **Step 7: 实现分页、CSV、审计操作人**

`internal/platform/paging/paging.go`：

```go
// Package paging 解析后台列表的 page / page_size 参数。
package paging

import (
	"net/url"
	"strconv"

	"spark-cloud/internal/platform/apperr"
)

const (
	DefaultSize = 20
	MaxSize     = 100
	maxNumber   = 1_000_000
)

// Page 是从 1 开始的页码与每页条数。
type Page struct {
	Number int
	Size   int
}

func Parse(q url.Values) (Page, error) {
	p := Page{Number: 1, Size: DefaultSize}
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxNumber {
			return Page{}, apperr.BadRequest("page 必须是 1～1000000 之间的整数")
		}
		p.Number = n
	}
	if v := q.Get("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxSize {
			return Page{}, apperr.BadRequest("page_size 必须是 1～100 之间的整数")
		}
		p.Size = n
	}
	return p, nil
}

// Normalize 把零值或越界的页码修正为默认值，供服务层直接调用时使用。
func (p Page) Normalize() Page {
	if p.Number < 1 || p.Number > maxNumber {
		p.Number = 1
	}
	if p.Size < 1 {
		p.Size = DefaultSize
	}
	if p.Size > MaxSize {
		p.Size = MaxSize
	}
	return p
}

func (p Page) Limit() int  { return p.Normalize().Size }
func (p Page) Offset() int { n := p.Normalize(); return (n.Number - 1) * n.Size }
```

`internal/platform/csvx/csvx.go`：

```go
// Package csvx 输出后台导出用的 CSV：UTF-8 带 BOM（Excel 直接打开不乱码），金额按元两位小数，时间按北京时间，并防止公式注入。
package csvx

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"time"

	"spark-cloud/internal/platform/apperr"
)

// MaxRows 是单次导出的最大行数。
const MaxRows = 100_000

var ErrTooLarge = apperr.New("export_too_large", "导出行数超过 10 万，请缩小时间范围", 400)

var beijing = time.FixedZone("CST", 8*3600)

type Writer struct {
	w *csv.Writer
}

func NewWriter(w io.Writer) (*Writer, error) {
	if _, err := io.WriteString(w, "\xEF\xBB\xBF"); err != nil {
		return nil, err
	}
	return &Writer{w: csv.NewWriter(w)}, nil
}

func (w *Writer) Write(record []string) error {
	out := make([]string, len(record))
	for i, v := range record {
		out[i] = escapeFormula(v)
	}
	return w.w.Write(out)
}

func (w *Writer) Flush() error {
	w.w.Flush()
	return w.w.Error()
}

// escapeFormula 给可能被表格软件当成公式的单元格加单引号前缀；负数金额保持原样。
func escapeFormula(v string) string {
	if v == "" {
		return v
	}
	switch v[0] {
	case '=', '+', '@', '\t', '\r':
		return "'" + v
	case '-':
		if _, err := strconv.ParseFloat(v, 64); err == nil {
			return v
		}
		return "'" + v
	}
	return v
}

// Yuan 把分格式化为元，保留两位小数。
func Yuan(fen int64) string {
	sign := ""
	if fen < 0 {
		sign, fen = "-", -fen
	}
	return fmt.Sprintf("%s%d.%02d", sign, fen/100, fen%100)
}

// Time 以北京时间格式化。
func Time(t time.Time) string { return t.In(beijing).Format("2006-01-02 15:04:05") }

func TimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return Time(*t)
}
```

在 `internal/audit/audit.go` 的 `Entry` 类型定义之后追加：

```go
// Actor 标识发起后台操作的管理员与来源 IP；AdminID 为 0 表示命令行等系统操作。
type Actor struct {
	AdminID int64
	IP      string
}

// Entry 生成一条以该操作人为主体的审计记录。
func (a Actor) Entry(action, targetType, targetID string, diff any) Entry {
	actorType := "admin"
	if a.AdminID == 0 {
		actorType = "system"
	}
	return Entry{ActorType: actorType, ActorID: a.AdminID, Action: action, TargetType: targetType, TargetID: targetID, Diff: diff, IP: a.IP}
}
```

- [ ] **Step 8: 运行确认通过**

Run: `go test ./internal/platform/paging/ ./internal/platform/csvx/ ./internal/audit/`
Expected: PASS

- [ ] **Step 9: 写外键错误映射的失败测试**

在 `internal/fleet/machines_test.go` 末尾追加：

```go
func TestRegisterUnknownProductIsBadRequest(t *testing.T) {
	svc, _, _ := newEnv(t)
	_, err := svc.Register(context.Background(), fleet.NewMachine{Serial: "SN009", LanIP: "10.0.1.9", ProductID: 9999})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" || ae.Message != "商品不存在" {
		t.Fatalf("want bad_request 商品不存在, got %v", err)
	}
}
```

在 `internal/fleet/commands_test.go` 末尾追加：

```go
func TestEnqueueUnknownMachineIsBadRequest(t *testing.T) {
	svc, pool, _ := newEnv(t)
	_, err := svc.Enqueue(context.Background(), pool, 9999, "", fleet.CmdWipeInstance, nil)
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" || ae.Message != "机器不存在" {
		t.Fatalf("want bad_request 机器不存在, got %v", err)
	}
}
```

- [ ] **Step 10: 运行确认失败**

Run: `go test ./internal/fleet/ -run 'Unknown'`
Expected: FAIL（返回的是原始 pg 错误）。

- [ ] **Step 11: 实现映射**

在 `internal/platform/db/db.go` 的 `IsUniqueViolation` 之后追加：

```go
// IsForeignKeyViolation 判断是否违反外键约束（引用了不存在的记录）。
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
```

`internal/fleet/machines.go` 的 `Register` 中，把

```go
	if db.IsUniqueViolation(err) {
		return Machine{}, apperr.New("duplicate_serial", "序列号已存在", 409)
	}
	return m, err
```

改为

```go
	if db.IsUniqueViolation(err) {
		return Machine{}, apperr.New("duplicate_serial", "序列号已存在", 409)
	}
	if db.IsForeignKeyViolation(err) {
		return Machine{}, apperr.BadRequest("商品不存在")
	}
	return m, err
```

`internal/fleet/commands.go` 的 `Enqueue` 末尾，把

```go
	_, err := q.Exec(ctx, `INSERT INTO agent_commands (id, machine_id, instance_id, type, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, id, machineID, instanceID, string(t), body, now)
	return id, err
```

改为

```go
	_, err := q.Exec(ctx, `INSERT INTO agent_commands (id, machine_id, instance_id, type, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, id, machineID, instanceID, string(t), body, now)
	if db.IsForeignKeyViolation(err) {
		return "", apperr.BadRequest("机器不存在")
	}
	return id, err
```

- [ ] **Step 12: 全量验证**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

- [ ] **Step 13: 提交**

```bash
git add internal/platform/config internal/platform/db/db.go internal/platform/paging internal/platform/csvx internal/audit internal/fleet
git commit -m "feat(platform): plan 4 config, paging, CSV export helpers, audit actor, FK error mapping

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: 管理员密码、TOTP 与恢复码（纯函数）

**Files:**
- Create: `internal/admin/password.go`、`internal/admin/totp.go`、`internal/admin/codes.go`
- Test: `internal/admin/crypto_test.go`（`package admin_test`）、`internal/admin/codes_internal_test.go`（`package admin`）
- Modify: `go.mod` / `go.sum`（新增 `golang.org/x/crypto`）

**Interfaces:**
- Consumes: `randx.ID`、`apperr.BadRequest`。
- Produces（Task 3、4 使用）：
  - `admin.HashPassword(password string) string`、`admin.VerifyPassword(encoded, password string) bool`、`admin.ValidatePassword(password string) error`、常量 `admin.MinPasswordLen = 12`。
  - `admin.NewTOTPSecret() string`（base32 无填充，20 字节）、`admin.TOTPStep(t time.Time) int64`、`admin.TOTPCode(secret string, step int64) (string, error)`、`admin.NormalizeCode(code string) string`、`admin.MatchTOTP(secret, code string, now time.Time) (int64, bool)`、`admin.OTPAuthURI(issuer, account, secret string) string`。
  - `admin.RecoveryCodeCount = 8`、`admin.NewRecoveryCodes() []string`；包内 `hashSecret(s string) string`、`hashRecoveryCode(code string) string`。

- [ ] **Step 1: 添加依赖**

Run: `export PATH=$HOME/.local/go/bin:$PATH && go get golang.org/x/crypto@v0.58.0`
Expected: `go.mod` 出现 `golang.org/x/crypto v0.58.0`。

- [ ] **Step 2: 写失败测试**

`internal/admin/crypto_test.go`：

```go
package admin_test

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/platform/apperr"
)

func TestHashAndVerifyPassword(t *testing.T) {
	h1 := admin.HashPassword("correct horse battery")
	h2 := admin.HashPassword("correct horse battery")
	if h1 == h2 {
		t.Fatal("hashes of the same password must differ (random salt)")
	}
	if !strings.HasPrefix(h1, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash format = %q", h1)
	}
	if !admin.VerifyPassword(h1, "correct horse battery") || !admin.VerifyPassword(h2, "correct horse battery") {
		t.Fatal("correct password rejected")
	}
	if admin.VerifyPassword(h1, "correct horse batterY") {
		t.Fatal("wrong password accepted")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA", "$argon2id$v=19$m=x,t=3,p=2$c2FsdA$aGFzaA"} {
		if admin.VerifyPassword(bad, "x") {
			t.Fatalf("malformed hash %q accepted", bad)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"12345678901": false, "123456789012": true, "十二个汉字的密码足够长了吧": true, strings.Repeat("a", 129): false,
	} {
		err := admin.ValidatePassword(pw)
		if ok != (err == nil) {
			t.Fatalf("ValidatePassword(%q) = %v, want ok=%v", pw, err, ok)
		}
		if err != nil && apperr.As(err).Code != "bad_request" {
			t.Fatalf("err code = %v", err)
		}
	}
}

// RFC 6238 附录 B 的 SHA1 测试向量（取 8 位结果的后 6 位）。
func TestTOTPCodeRFCVectors(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // ASCII "12345678901234567890"
	for unix, want := range map[int64]string{
		59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037",
	} {
		got, err := admin.TOTPCode(secret, admin.TOTPStep(time.Unix(unix, 0)))
		if err != nil || got != want {
			t.Fatalf("TOTPCode at %d = %q, %v; want %q", unix, got, err, want)
		}
	}
	if _, err := admin.TOTPCode("not base32!", 1); err == nil {
		t.Fatal("expected error for invalid secret")
	}
}

func TestMatchTOTPWindow(t *testing.T) {
	secret := admin.NewTOTPSecret()
	if len(secret) != 32 {
		t.Fatalf("secret length = %d, want 32", len(secret))
	}
	now := time.Date(2026, 1, 1, 0, 0, 15, 0, time.UTC)
	step := admin.TOTPStep(now)
	for offset, ok := range map[int64]bool{-2: false, -1: true, 0: true, 1: true, 2: false} {
		code, _ := admin.TOTPCode(secret, step+offset)
		got, matched := admin.MatchTOTP(secret, code, now)
		if matched != ok || (ok && got != step+offset) {
			t.Fatalf("offset %d: step=%d matched=%v", offset, got, matched)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := admin.MatchTOTP(secret, bad, now); ok {
			t.Fatalf("code %q matched", bad)
		}
	}
}

func TestMatchTOTPIgnoresSpaces(t *testing.T) {
	secret := admin.NewTOTPSecret()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	code, _ := admin.TOTPCode(secret, admin.TOTPStep(now))
	spaced := " " + code[:3] + " " + code[3:] + "\t"
	if _, ok := admin.MatchTOTP(secret, spaced, now); !ok {
		t.Fatalf("spaced code %q rejected", spaced)
	}
}

func TestOTPAuthURI(t *testing.T) {
	u, err := url.Parse(admin.OTPAuthURI("Spark Cloud", "root", "ABCDEF"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "otpauth" || u.Host != "totp" || u.Path != "/Spark Cloud:root" ||
		q.Get("secret") != "ABCDEF" || q.Get("issuer") != "Spark Cloud" || q.Get("digits") != "6" || q.Get("period") != "30" {
		t.Fatalf("uri = %s", u)
	}
}

func TestNewRecoveryCodes(t *testing.T) {
	codes := admin.NewRecoveryCodes()
	if len(codes) != admin.RecoveryCodeCount {
		t.Fatalf("len = %d", len(codes))
	}
	seen := map[string]bool{}
	re := regexp.MustCompile(`^[a-z0-9]{5}-[a-z0-9]{5}$`)
	for _, c := range codes {
		if !re.MatchString(c) || seen[c] {
			t.Fatalf("bad or duplicate code %q", c)
		}
		seen[c] = true
	}
}
```

`internal/admin/codes_internal_test.go`：

```go
package admin

import "testing"

func TestHashRecoveryCodeNormalizes(t *testing.T) {
	if hashRecoveryCode(" ABCDE-12345 ") != hashRecoveryCode("abcde12345") {
		t.Fatal("recovery code hash must ignore case, spaces and hyphens")
	}
	if hashRecoveryCode("abcde-12345") == hashRecoveryCode("abcde-12346") {
		t.Fatal("different codes must hash differently")
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `go test ./internal/admin/`
Expected: 编译失败（`admin.HashPassword` 等未定义）。

- [ ] **Step 4: 实现**

`internal/admin/password.go`：

```go
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"spark-cloud/internal/platform/apperr"
)

const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16

	MinPasswordLen = 12
	maxPasswordLen = 128
)

// HashPassword 用 argon2id 生成 PHC 格式字符串：$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>（base64 无填充）。
func HashPassword(password string) string {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key))
}

// VerifyPassword 校验密码；哈希格式不正确一律视为不匹配。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ValidatePassword 校验管理员密码长度（按字符计）。
func ValidatePassword(password string) error {
	if n := utf8.RuneCountInString(password); n < MinPasswordLen || n > maxPasswordLen {
		return apperr.BadRequest("密码长度需为 12～128 个字符")
	}
	return nil
}
```

`internal/admin/totp.go`：

```go
package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	totpPeriod = 30 // 秒
	totpDigits = 6
	totpSkew   = 1 // 允许前后各 1 个时间片的时钟偏差
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret 生成 20 字节随机密钥的 base32 编码（32 个字符）。
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// TOTPStep 返回 t 所在的 30 秒时间片编号。
func TOTPStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// TOTPCode 按 RFC 6238（HMAC-SHA1、6 位）计算指定时间片的动态码。
func TOTPCode(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("totp secret: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000), nil
}

// NormalizeCode 去掉动态码中的所有空白（验证器 App 常显示为「123 456」）。
func NormalizeCode(code string) string { return strings.Join(strings.Fields(code), "") }

// MatchTOTP 在 now 所在时间片前后各 1 个时间片内查找与 code 相同的动态码，返回匹配的时间片编号。
func MatchTOTP(secret, code string, now time.Time) (int64, bool) {
	code = NormalizeCode(code)
	if len(code) != totpDigits {
		return 0, false
	}
	cur := TOTPStep(now)
	for step := cur - totpSkew; step <= cur+totpSkew; step++ {
		want, err := TOTPCode(secret, step)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// OTPAuthURI 生成验证器 App 可扫描的 otpauth:// 链接。
func OTPAuthURI(issuer, account, secret string) string {
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + q.Encode()
}
```

`internal/admin/codes.go`：

```go
package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"spark-cloud/internal/platform/randx"
)

// RecoveryCodeCount 是每次绑定 TOTP 时生成的恢复码数量。
const RecoveryCodeCount = 8

// NewRecoveryCodes 生成形如 abcde-12345 的一次性恢复码。
func NewRecoveryCodes() []string {
	out := make([]string, RecoveryCodeCount)
	for i := range out {
		out[i] = randx.ID("", 5) + "-" + randx.ID("", 5)
	}
	return out
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hashRecoveryCode 忽略大小写、空白与连字符后再哈希，方便管理员照抄输入。
func hashRecoveryCode(code string) string {
	normalized := strings.ReplaceAll(strings.ToLower(NormalizeCode(code)), "-", "")
	return hashSecret("recovery:" + normalized)
}
```

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/admin/`
Expected: PASS

- [ ] **Step 6: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add go.mod go.sum internal/admin
git commit -m "feat(admin): argon2id passwords, RFC 6238 TOTP and recovery codes

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: 管理员登录、TOTP 绑定、会话与二次验证

**Files:**
- Create: `migrations/00009_admin.sql`
- Create: `internal/admin/service.go`、`internal/admin/auth.go`
- Test: `internal/admin/auth_test.go`、`internal/admin/helpers_test.go`（均为 `package admin_test`）

**Interfaces:**
- Consumes: Task 2 的 `HashPassword`、`VerifyPassword`、`ValidatePassword`、`NewTOTPSecret`、`MatchTOTP`、`OTPAuthURI`、`NewRecoveryCodes`、`hashSecret`、`hashRecoveryCode`；Task 1 的 `audit.Actor`；计划 1 的 `secretbox.Box`、`randx.Token`、`db.WithTx`。
- Produces（Task 4、15 使用）：
  - 类型 `admin.Role`（`RoleSuper`、`RoleOps`、`RoleFinance`，方法 `Valid() bool`），常量 `StatusActive = "active"`、`StatusDisabled = "disabled"`、`PendingTTL`、`SessionTTL`、`IdleTimeout`、`StepUpWindow`、`MaxFailures`、`LockDuration`、`Issuer = "Spark Cloud"`。
  - 错误 `ErrInvalidCredentials`（invalid_credentials/401）、`ErrLocked`（account_locked/423）、`ErrInvalidTOTP`（invalid_totp/400）、`ErrTOTPRequired`（totp_required/403）、`ErrTOTPSetupRequired`（totp_setup_required/403）。
  - `admin.Admin{ID, Username, Role, Status, TOTPConfirmed, LastLoginAt, LastLoginIP, CreatedAt}`（带 json 标签）、`admin.Session{Admin Admin; TokenHash string; StepUpAt *time.Time}`、`admin.LoginResult{Token string; NeedTOTPSetup bool; OTPAuthURI string}`。
  - `admin.Deps{Pool *pgxpool.Pool; Clock clock.Clock; Box *secretbox.Box}`、`admin.NewService(d Deps) *Service`。
  - `(*Service).Create(ctx, actor audit.Actor, username, password string, role Role) (Admin, error)`
  - `(*Service).Get(ctx, id int64) (Admin, error)`
  - `(*Service).Login(ctx, username, password, ip string) (LoginResult, error)`
  - `(*Service).ConfirmTOTPSetup(ctx, token, code, ip string) (Admin, []string, error)`
  - `(*Service).VerifyTOTP(ctx, token, code, ip string) (Admin, error)`
  - `(*Service).VerifyRecoveryCode(ctx, token, recoveryCode, ip string) (Admin, error)`
  - `(*Service).Authenticate(ctx, token string) (Session, error)`
  - `(*Service).StepUp(ctx, sess Session, code string) error`
  - `(*Service).Logout(ctx, token string) error`
  - 包内辅助（Task 4 复用）：`adminRow`、`lockAdmin`、`(*Service).ensureSecret`、`(*Service).checkTOTP`、`(*Service).confirmTOTP`、`recordFailure`。

- [ ] **Step 1: 写迁移**

`migrations/00009_admin.sql`：

```sql
-- +goose Up
CREATE TABLE admins (
    id              BIGSERIAL PRIMARY KEY,
    username        TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    totp_secret_enc BYTEA,                    -- AES-GCM 加密的 TOTP 密钥；未绑定或已重置时为 NULL
    totp_confirmed  BOOLEAN NOT NULL DEFAULT false,
    role            TEXT NOT NULL CHECK (role IN ('super', 'ops', 'finance')),
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    failed_attempts INT NOT NULL DEFAULT 0,
    locked_until    TIMESTAMPTZ,
    last_login_at   TIMESTAMPTZ,
    last_login_ip   TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE admin_recovery_codes (
    id        BIGSERIAL PRIMARY KEY,
    admin_id  BIGINT NOT NULL REFERENCES admins (id),
    code_hash TEXT NOT NULL,
    used_at   TIMESTAMPTZ
);
CREATE INDEX admin_recovery_codes_admin_idx ON admin_recovery_codes (admin_id);

-- stage=password_ok：密码已通过、等待动态码（5 分钟）；stage=active：已登录（绝对 12 小时，空闲 2 小时）
CREATE TABLE admin_sessions (
    token_hash   TEXT PRIMARY KEY,
    admin_id     BIGINT NOT NULL REFERENCES admins (id),
    stage        TEXT NOT NULL CHECK (stage IN ('password_ok', 'active')),
    ip           TEXT NOT NULL DEFAULT '',
    stepup_at    TIMESTAMPTZ,                 -- 最近一次高风险操作二次验证通过的时间
    created_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX admin_sessions_admin_idx ON admin_sessions (admin_id);

-- 已使用的动态码时间片，防止同一动态码被重复使用；只保留最近 10 分钟
CREATE TABLE admin_totp_used (
    admin_id BIGINT NOT NULL REFERENCES admins (id),
    step     BIGINT NOT NULL,
    used_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (admin_id, step)
);

-- +goose Down
DROP TABLE admin_totp_used;
DROP TABLE admin_sessions;
DROP TABLE admin_recovery_codes;
DROP TABLE admins;
```

- [ ] **Step 2: 写测试辅助与失败测试**

`internal/admin/helpers_test.go`：

```go
package admin_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

const (
	password = "correct horse battery"
	ip       = "10.0.0.8"
)

type env struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	clk  *clock.Fake
	svc  *admin.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(dbtest.T0)
	return &env{t: t, ctx: context.Background(), pool: pool, clk: clk,
		svc: admin.NewService(admin.Deps{Pool: pool, Clock: clk, Box: box})}
}

func (e *env) create(username string, role admin.Role) admin.Admin {
	e.t.Helper()
	a, err := e.svc.Create(e.ctx, audit.Actor{}, username, password, role)
	if err != nil {
		e.t.Fatalf("Create(%s): %v", username, err)
	}
	return a
}

// code 计算当前时间偏移 offset 个时间片的动态码。
func (e *env) code(secret string, offset int64) string {
	e.t.Helper()
	c, err := admin.TOTPCode(secret, admin.TOTPStep(e.clk.Now())+offset)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// enroll 完成首次登录与 TOTP 绑定，返回密钥、恢复码和已激活的会话 token。
func (e *env) enroll(username string) (string, []string, string) {
	e.t.Helper()
	res, err := e.svc.Login(e.ctx, username, password, ip)
	if err != nil || !res.NeedTOTPSetup {
		e.t.Fatalf("first login = %+v, %v", res, err)
	}
	u, err := url.Parse(res.OTPAuthURI)
	if err != nil {
		e.t.Fatal(err)
	}
	secret := u.Query().Get("secret")
	_, codes, err := e.svc.ConfirmTOTPSetup(e.ctx, res.Token, e.code(secret, 0), ip)
	if err != nil {
		e.t.Fatalf("ConfirmTOTPSetup: %v", err)
	}
	return secret, codes, res.Token
}

// login 用密码 + 动态码完成一次完整登录；先快进 30 秒，避免与上一次使用的动态码同一时间片。
func (e *env) login(username, secret string) string {
	e.t.Helper()
	e.clk.Advance(30 * time.Second)
	res, err := e.svc.Login(e.ctx, username, password, ip)
	if err != nil || res.NeedTOTPSetup {
		e.t.Fatalf("login = %+v, %v", res, err)
	}
	if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, e.code(secret, 0), ip); err != nil {
		e.t.Fatalf("VerifyTOTP: %v", err)
	}
	return res.Token
}

// codeOf 返回业务错误码；nil 返回空串；非业务错误返回 "raw: ..." 便于在失败信息中看到原始错误。
func codeOf(err error) string {
	if err == nil {
		return ""
	}
	if ae := apperr.As(err); ae != nil {
		return ae.Code
	}
	return "raw: " + err.Error()
}
```

`internal/admin/auth_test.go`：

```go
package admin_test

import (
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
)

func TestCreateValidatesAndAudits(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		username, password string
		role               admin.Role
		want               string
	}{
		{"ab", password, admin.RoleSuper, "bad_request"},
		{"1root", password, admin.RoleSuper, "bad_request"},
		{"root", "short", admin.RoleSuper, "bad_request"},
		{"root", password, admin.Role("god"), "bad_request"},
	}
	for _, c := range cases {
		if _, err := e.svc.Create(e.ctx, audit.Actor{}, c.username, c.password, c.role); codeOf(err) != c.want {
			t.Fatalf("Create(%q, role %q) = %v, want %s", c.username, c.role, err, c.want)
		}
	}
	a := e.create(" Root ", admin.RoleSuper)
	if a.Username != "root" || a.Role != admin.RoleSuper || a.Status != admin.StatusActive || a.TOTPConfirmed {
		t.Fatalf("admin = %+v", a)
	}
	if _, err := e.svc.Create(e.ctx, audit.Actor{}, "root", password, admin.RoleOps); codeOf(err) != "duplicate_username" {
		t.Fatalf("duplicate = %v", err)
	}
	var actorType, action string
	if err := e.pool.QueryRow(e.ctx, `SELECT actor_type, action FROM audit_logs WHERE target_type = 'admin'`).Scan(&actorType, &action); err != nil {
		t.Fatal(err)
	}
	if actorType != "system" || action != "admin.create" {
		t.Fatalf("audit = %s %s", actorType, action)
	}
	var hash string
	if err := e.pool.QueryRow(e.ctx, `SELECT password_hash FROM admins WHERE id = $1`, a.ID).Scan(&hash); err != nil || !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("password hash = %q, %v", hash, err)
	}
}

func TestFirstLoginRequiresTOTPSetup(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	res, err := e.svc.Login(e.ctx, "root", password, ip)
	if err != nil || !res.NeedTOTPSetup || !strings.HasPrefix(res.OTPAuthURI, "otpauth://totp/Spark%20Cloud:root?") || res.Token == "" {
		t.Fatalf("login = %+v, %v", res, err)
	}
	// 密码阶段的会话不能访问后台，也不能跳过绑定直接验证动态码
	if _, err := e.svc.Authenticate(e.ctx, res.Token); codeOf(err) != "unauthorized" {
		t.Fatalf("authenticate pending = %v", err)
	}
	if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, "123456", ip); codeOf(err) != "totp_setup_required" {
		t.Fatalf("verify before setup = %v", err)
	}
	// 再次登录拿到的是同一个待绑定密钥
	res2, err := e.svc.Login(e.ctx, "root", password, ip)
	if err != nil || res2.OTPAuthURI != res.OTPAuthURI {
		t.Fatalf("second login uri changed: %v", err)
	}
	if _, _, err := e.svc.ConfirmTOTPSetup(e.ctx, res.Token, "000000", ip); codeOf(err) != "invalid_totp" {
		t.Fatalf("wrong setup code = %v", err)
	}

	secret, codes, token := e.enroll("root")
	if len(codes) != admin.RecoveryCodeCount || secret == "" {
		t.Fatalf("codes = %v", codes)
	}
	sess, err := e.svc.Authenticate(e.ctx, token)
	if err != nil || sess.Admin.Username != "root" || !sess.Admin.TOTPConfirmed || sess.Admin.LastLoginIP != ip {
		t.Fatalf("session = %+v, %v", sess, err)
	}
	e.clk.Advance(30 * time.Second)
	if res, err := e.svc.Login(e.ctx, "root", password, ip); err != nil || res.NeedTOTPSetup || res.OTPAuthURI != "" {
		t.Fatalf("login after setup = %+v, %v", res, err)
	}
}

func TestVerifyTOTPWindowAndReplay(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, _ := e.enroll("root")
	// 绑定时用掉了时间片 S；快进到 S+2，使 S+1 仍在窗口内且未被使用
	e.clk.Advance(60 * time.Second)

	res, _ := e.svc.Login(e.ctx, "root", password, ip)
	if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, e.code(secret, -2), ip); codeOf(err) != "invalid_totp" {
		t.Fatalf("code two steps old = %v", err)
	}
	code := e.code(secret, -1)
	if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, code, ip); err != nil {
		t.Fatalf("code one step old = %v", err)
	}
	// 同一动态码不能再用于另一次登录
	res2, _ := e.svc.Login(e.ctx, "root", password, ip)
	if _, err := e.svc.VerifyTOTP(e.ctx, res2.Token, code, ip); codeOf(err) != "invalid_totp" {
		t.Fatalf("replayed code = %v", err)
	}
	if _, err := e.svc.VerifyTOTP(e.ctx, res2.Token, e.code(secret, 0), ip); err != nil {
		t.Fatalf("fresh code = %v", err)
	}
	// 已激活的会话不能再走第二步
	if _, err := e.svc.VerifyTOTP(e.ctx, res2.Token, e.code(secret, 1), ip); codeOf(err) != "unauthorized" {
		t.Fatalf("verify on active session = %v", err)
	}
}

func TestWrongPasswordLocksAccount(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	if _, err := e.svc.Login(e.ctx, "nobody", password, ip); codeOf(err) != "invalid_credentials" {
		t.Fatalf("unknown user = %v", err)
	}
	for i := 0; i < admin.MaxFailures; i++ {
		if _, err := e.svc.Login(e.ctx, "root", "wrong password!", ip); codeOf(err) != "invalid_credentials" {
			t.Fatalf("attempt %d = %v", i+1, err)
		}
	}
	if _, err := e.svc.Login(e.ctx, "root", password, ip); codeOf(err) != "account_locked" {
		t.Fatalf("after 5 failures = %v", err)
	}
	e.clk.Advance(admin.LockDuration)
	if _, err := e.svc.Login(e.ctx, "root", password, ip); err != nil {
		t.Fatalf("after lock expiry = %v", err)
	}
}

func TestTOTPFailuresCountTowardLock(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, _ := e.enroll("root")
	e.clk.Advance(30 * time.Second)
	res, _ := e.svc.Login(e.ctx, "root", password, ip)
	for i := 0; i < admin.MaxFailures; i++ {
		if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, "000000", ip); codeOf(err) != "invalid_totp" {
			t.Fatalf("attempt %d = %v", i+1, err)
		}
	}
	if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, e.code(secret, 0), ip); codeOf(err) != "account_locked" {
		t.Fatalf("correct code while locked = %v", err)
	}
	if _, err := e.svc.Login(e.ctx, "root", password, ip); codeOf(err) != "account_locked" {
		t.Fatalf("password while locked = %v", err)
	}
}

func TestSuccessfulLoginResetsFailures(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, _ := e.enroll("root")
	for i := 0; i < admin.MaxFailures-1; i++ {
		_, _ = e.svc.Login(e.ctx, "root", "wrong password!", ip)
	}
	e.login("root", secret)
	for i := 0; i < admin.MaxFailures-1; i++ {
		if _, err := e.svc.Login(e.ctx, "root", "wrong password!", ip); codeOf(err) != "invalid_credentials" {
			t.Fatalf("attempt %d after reset = %v", i+1, err)
		}
	}
}

func TestRecoveryCodeIsSingleUse(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	_, codes, _ := e.enroll("root")
	res, _ := e.svc.Login(e.ctx, "root", password, ip)
	if _, err := e.svc.VerifyRecoveryCode(e.ctx, res.Token, " "+strings.ToUpper(codes[0])+" ", ip); err != nil {
		t.Fatalf("recovery code = %v", err)
	}
	res2, _ := e.svc.Login(e.ctx, "root", password, ip)
	if _, err := e.svc.VerifyRecoveryCode(e.ctx, res2.Token, codes[0], ip); codeOf(err) != "invalid_totp" {
		t.Fatalf("reused recovery code = %v", err)
	}
	if _, err := e.svc.VerifyRecoveryCode(e.ctx, res2.Token, codes[1], ip); err != nil {
		t.Fatalf("second recovery code = %v", err)
	}
}

func TestPendingSessionExpires(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, _ := e.enroll("root")
	e.clk.Advance(30 * time.Second)
	res, _ := e.svc.Login(e.ctx, "root", password, ip)
	e.clk.Advance(admin.PendingTTL + time.Second)
	if _, err := e.svc.VerifyTOTP(e.ctx, res.Token, e.code(secret, 0), ip); codeOf(err) != "unauthorized" {
		t.Fatalf("expired pending session = %v", err)
	}
}

func TestSessionIdleTimeout(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	_, _, token := e.enroll("root")
	e.clk.Advance(admin.IdleTimeout - time.Minute)
	if _, err := e.svc.Authenticate(e.ctx, token); err != nil {
		t.Fatalf("before idle timeout = %v", err)
	}
	e.clk.Advance(admin.IdleTimeout - time.Minute) // 上一次访问刷新了空闲计时
	if _, err := e.svc.Authenticate(e.ctx, token); err != nil {
		t.Fatalf("activity should extend idle timeout: %v", err)
	}
	e.clk.Advance(admin.IdleTimeout + time.Second)
	if _, err := e.svc.Authenticate(e.ctx, token); codeOf(err) != "unauthorized" {
		t.Fatalf("after idle timeout = %v", err)
	}
}

func TestSessionAbsoluteExpiry(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	_, _, token := e.enroll("root")
	for elapsed := time.Duration(0); elapsed < admin.SessionTTL-time.Hour; elapsed += time.Hour {
		e.clk.Advance(time.Hour)
		if _, err := e.svc.Authenticate(e.ctx, token); err != nil {
			t.Fatalf("at %v: %v", elapsed+time.Hour, err)
		}
	}
	e.clk.Advance(time.Hour)
	if _, err := e.svc.Authenticate(e.ctx, token); codeOf(err) != "unauthorized" {
		t.Fatalf("after 12h = %v", err)
	}
}

func TestStepUp(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, token := e.enroll("root")
	sess, _ := e.svc.Authenticate(e.ctx, token)
	if err := e.svc.StepUp(e.ctx, sess, ""); codeOf(err) != "totp_required" {
		t.Fatalf("no code, no prior step-up = %v", err)
	}
	e.clk.Advance(30 * time.Second)
	if err := e.svc.StepUp(e.ctx, sess, "000000"); codeOf(err) != "invalid_totp" {
		t.Fatalf("wrong code = %v", err)
	}
	if err := e.svc.StepUp(e.ctx, sess, e.code(secret, 0)); err != nil {
		t.Fatalf("right code = %v", err)
	}
	sess, _ = e.svc.Authenticate(e.ctx, token)
	if sess.StepUpAt == nil {
		t.Fatal("StepUpAt not recorded")
	}
	e.clk.Advance(admin.StepUpWindow - time.Second)
	sess, _ = e.svc.Authenticate(e.ctx, token)
	if err := e.svc.StepUp(e.ctx, sess, ""); err != nil {
		t.Fatalf("within window = %v", err)
	}
	e.clk.Advance(2 * time.Second)
	sess, _ = e.svc.Authenticate(e.ctx, token)
	if err := e.svc.StepUp(e.ctx, sess, ""); codeOf(err) != "totp_required" {
		t.Fatalf("after window = %v", err)
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	_, _, token := e.enroll("root")
	if err := e.svc.Logout(e.ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(e.ctx, token); codeOf(err) != "unauthorized" {
		t.Fatalf("after logout = %v", err)
	}
	if _, err := e.svc.Authenticate(e.ctx, ""); codeOf(err) != "unauthorized" {
		t.Fatalf("empty token = %v", err)
	}
}

func TestTOTPSecretIsEncrypted(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, _ := e.enroll("root")
	var enc []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT totp_secret_enc FROM admins WHERE username = 'root'`).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc), secret) {
		t.Fatal("TOTP secret stored in plaintext")
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `go test ./internal/admin/`
Expected: 编译失败（`admin.NewService` 等未定义）。

- [ ] **Step 4: 实现服务骨架**

`internal/admin/service.go`：

```go
// Package admin 管理后台管理员：账号、密码、TOTP 动态码、恢复码、会话与高风险操作二次验证。
package admin

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
)

type Role string

const (
	RoleSuper   Role = "super"
	RoleOps     Role = "ops"
	RoleFinance Role = "finance"
)

func (r Role) Valid() bool { return r == RoleSuper || r == RoleOps || r == RoleFinance }

const (
	StatusActive   = "active"
	StatusDisabled = "disabled"

	PendingTTL        = 5 * time.Minute  // 密码通过后等待动态码的时长
	SessionTTL        = 12 * time.Hour   // 会话绝对有效期
	IdleTimeout       = 2 * time.Hour    // 会话空闲失效
	StepUpWindow      = 5 * time.Minute  // 高风险操作二次验证的有效期
	MaxFailures       = 5                // 连续失败次数上限（密码与动态码合计）
	LockDuration      = 15 * time.Minute // 锁定时长
	totpUsedRetention = 10 * time.Minute

	Issuer = "Spark Cloud"
)

var (
	ErrInvalidCredentials = apperr.New("invalid_credentials", "用户名或密码错误", 401)
	ErrLocked             = apperr.New("account_locked", "登录失败次数过多，请 15 分钟后再试", 423)
	ErrInvalidTOTP        = apperr.New("invalid_totp", "动态码错误或已使用过", 400)
	ErrTOTPRequired       = apperr.New("totp_required", "该操作需要输入动态码", 403)
	ErrTOTPSetupRequired  = apperr.New("totp_setup_required", "请先绑定动态码", 403)
)

var usernameRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{2,31}$`)

type Admin struct {
	ID            int64      `json:"id"`
	Username      string     `json:"username"`
	Role          Role       `json:"role"`
	Status        string     `json:"status"`
	TOTPConfirmed bool       `json:"totp_confirmed"`
	LastLoginAt   *time.Time `json:"last_login_at"`
	LastLoginIP   string     `json:"last_login_ip"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Session 是已登录的后台会话。
type Session struct {
	Admin     Admin
	TokenHash string
	StepUpAt  *time.Time
}

type Deps struct {
	Pool  *pgxpool.Pool
	Clock clock.Clock
	Box   *secretbox.Box
}

type Service struct {
	d Deps
}

func NewService(d Deps) *Service { return &Service{d: d} }

const adminCols = `id, username, role, status, totp_confirmed, last_login_at, last_login_ip, created_at`

func scanAdmin(row pgx.Row) (Admin, error) {
	var a Admin
	err := row.Scan(&a.ID, &a.Username, &a.Role, &a.Status, &a.TOTPConfirmed, &a.LastLoginAt, &a.LastLoginIP, &a.CreatedAt)
	if db.IsNoRows(err) {
		return Admin{}, apperr.ErrNotFound
	}
	return a, err
}

// adminRow 带有密码哈希、加密的 TOTP 密钥与失败计数，只在包内使用。
type adminRow struct {
	Admin
	passwordHash   string
	totpSecretEnc  []byte
	failedAttempts int
	lockedUntil    *time.Time
}

func (a adminRow) locked(now time.Time) bool { return a.lockedUntil != nil && now.Before(*a.lockedUntil) }

const adminRowCols = adminCols + `, password_hash, totp_secret_enc, failed_attempts, locked_until`

func scanAdminRow(row pgx.Row) (adminRow, error) {
	var a adminRow
	err := row.Scan(&a.ID, &a.Username, &a.Role, &a.Status, &a.TOTPConfirmed, &a.LastLoginAt, &a.LastLoginIP, &a.CreatedAt,
		&a.passwordHash, &a.totpSecretEnc, &a.failedAttempts, &a.lockedUntil)
	if db.IsNoRows(err) {
		return adminRow{}, apperr.ErrNotFound
	}
	return a, err
}

func lockAdmin(ctx context.Context, tx pgx.Tx, id int64) (adminRow, error) {
	return scanAdminRow(tx.QueryRow(ctx, `SELECT `+adminRowCols+` FROM admins WHERE id = $1 FOR UPDATE`, id))
}

func (s *Service) Get(ctx context.Context, id int64) (Admin, error) {
	return scanAdmin(s.d.Pool.QueryRow(ctx, `SELECT `+adminCols+` FROM admins WHERE id = $1`, id))
}

// Create 新建管理员（状态 active、未绑定 TOTP，首次登录时绑定）。actor 为零值表示命令行创建。
func (s *Service) Create(ctx context.Context, actor audit.Actor, username, password string, role Role) (Admin, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernameRe.MatchString(username) {
		return Admin{}, apperr.BadRequest("用户名需为 3～32 位小写字母、数字或 _ . -，并以字母开头")
	}
	if !role.Valid() {
		return Admin{}, apperr.BadRequest("角色只能是 super、ops 或 finance")
	}
	if err := ValidatePassword(password); err != nil {
		return Admin{}, err
	}
	hash := HashPassword(password)
	now := s.d.Clock.Now()
	var a Admin
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var err error
		a, err = scanAdmin(tx.QueryRow(ctx, `INSERT INTO admins (username, password_hash, role, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $4) RETURNING `+adminCols, username, hash, string(role), now))
		if db.IsUniqueViolation(err) {
			return apperr.New("duplicate_username", "用户名已存在", 409)
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("admin.create", "admin", strconv.FormatInt(a.ID, 10),
			map[string]any{"username": username, "role": role}))
	})
	return a, err
}

// secret 解密管理员的 TOTP 密钥。
func (s *Service) secret(a adminRow) (string, error) {
	if a.totpSecretEnc == nil {
		return "", ErrTOTPSetupRequired
	}
	plain, err := s.d.Box.Open(a.totpSecretEnc)
	if err != nil {
		return "", fmt.Errorf("open totp secret: %w", err)
	}
	return string(plain), nil
}

// ensureSecret 返回管理员待确认的 TOTP 密钥；还没有则生成并加密保存。
func (s *Service) ensureSecret(ctx context.Context, tx pgx.Tx, a adminRow, now time.Time) (string, error) {
	if a.totpSecretEnc != nil {
		return s.secret(a)
	}
	secret := NewTOTPSecret()
	_, err := tx.Exec(ctx, `UPDATE admins SET totp_secret_enc = $2, updated_at = $3 WHERE id = $1`, a.ID, s.d.Box.Seal([]byte(secret)), now)
	return secret, err
}

// recordFailure 累计一次失败；达到上限时锁定账号并清零计数。
func recordFailure(ctx context.Context, tx pgx.Tx, id int64, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE admins SET
			failed_attempts = CASE WHEN failed_attempts + 1 >= $2 THEN 0 ELSE failed_attempts + 1 END,
			locked_until = CASE WHEN failed_attempts + 1 >= $2 THEN $3::timestamptz ELSE locked_until END,
			updated_at = $4
		WHERE id = $1`, id, MaxFailures, now.Add(LockDuration), now)
	return err
}

// useStep 记录已使用的动态码时间片；同一时间片第二次使用返回 false（防重放）。
func useStep(ctx context.Context, tx pgx.Tx, adminID, step int64, now time.Time) (bool, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM admin_totp_used WHERE admin_id = $1 AND used_at < $2`,
		adminID, now.Add(-totpUsedRetention)); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO admin_totp_used (admin_id, step, used_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		adminID, step, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// checkTOTP 校验动态码并防重放；不通过时累计失败次数并返回 false（调用方应提交事务后返回 ErrInvalidTOTP）。
func (s *Service) checkTOTP(ctx context.Context, tx pgx.Tx, a adminRow, code string, now time.Time) (bool, error) {
	secret, err := s.secret(a)
	if err != nil {
		return false, err
	}
	step, ok := MatchTOTP(secret, code, now)
	if ok {
		if ok, err = useStep(ctx, tx, a.ID, step, now); err != nil {
			return false, err
		}
	}
	if !ok {
		return false, recordFailure(ctx, tx, a.ID, now)
	}
	return true, nil
}

// confirmTOTP 用动态码确认绑定，生成并保存新的恢复码。bizErr 非空时调用方应提交事务（保留失败计数）后返回它。
func (s *Service) confirmTOTP(ctx context.Context, tx pgx.Tx, a adminRow, code string, now time.Time) (codes []string, bizErr, err error) {
	ok, err := s.checkTOTP(ctx, tx, a, code, now)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, ErrInvalidTOTP, nil
	}
	codes = NewRecoveryCodes()
	if _, err := tx.Exec(ctx, `DELETE FROM admin_recovery_codes WHERE admin_id = $1`, a.ID); err != nil {
		return nil, nil, err
	}
	for _, c := range codes {
		if _, err := tx.Exec(ctx, `INSERT INTO admin_recovery_codes (admin_id, code_hash) VALUES ($1, $2)`, a.ID, hashRecoveryCode(c)); err != nil {
			return nil, nil, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE admins SET totp_confirmed = true, updated_at = $2 WHERE id = $1`, a.ID, now); err != nil {
		return nil, nil, err
	}
	return codes, nil, nil
}
```

注意：`checkTOTP` 中 `s.secret(a)` 在 `totpSecretEnc == nil` 时返回业务错误 `ErrTOTPSetupRequired`；调用方在调用前已检查绑定状态，这里只是兜底。

- [ ] **Step 5: 实现登录与会话**

`internal/admin/auth.go`：

```go
package admin

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

// LoginResult 是第一步（密码）登录的结果。NeedTOTPSetup 为 true 时必须先调用 ConfirmTOTPSetup 完成绑定。
type LoginResult struct {
	Token         string
	NeedTOTPSetup bool
	OTPAuthURI    string
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// burnPasswordCheck 对不存在的用户名也做一次同等开销的哈希，避免通过耗时差异探测用户名。
func burnPasswordCheck(password string) {
	dummyOnce.Do(func() { dummyHash = HashPassword("spark-cloud-dummy-password") })
	VerifyPassword(dummyHash, password)
}

// Login 校验用户名与密码，成功后返回 5 分钟内有效的「密码已通过」会话。
func (s *Service) Login(ctx context.Context, username, password, ip string) (LoginResult, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	now := s.d.Clock.Now()
	var res LoginResult
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		a, err := scanAdminRow(tx.QueryRow(ctx, `SELECT `+adminRowCols+` FROM admins WHERE username = $1 FOR UPDATE`, username))
		if errors.Is(err, apperr.ErrNotFound) {
			burnPasswordCheck(password)
			bizErr = ErrInvalidCredentials
			return nil
		}
		if err != nil {
			return err
		}
		if a.locked(now) {
			bizErr = ErrLocked
			return nil
		}
		if !VerifyPassword(a.passwordHash, password) || a.Status != StatusActive {
			bizErr = ErrInvalidCredentials
			return recordFailure(ctx, tx, a.ID, now)
		}
		token := randx.Token()
		if _, err := tx.Exec(ctx, `INSERT INTO admin_sessions (token_hash, admin_id, stage, ip, created_at, last_seen_at, expires_at)
			VALUES ($1, $2, 'password_ok', $3, $4, $4, $5)`, hashSecret(token), a.ID, ip, now, now.Add(PendingTTL)); err != nil {
			return err
		}
		res.Token = token
		if !a.TOTPConfirmed {
			secret, err := s.ensureSecret(ctx, tx, a, now)
			if err != nil {
				return err
			}
			res.NeedTOTPSetup = true
			res.OTPAuthURI = OTPAuthURI(Issuer, a.Username, secret)
		}
		return nil
	})
	if err != nil {
		return LoginResult{}, err
	}
	if bizErr != nil {
		return LoginResult{}, bizErr
	}
	return res, nil
}

// pendingSession 找到「密码已通过」阶段且未过期的会话，并锁定其管理员行。
func (s *Service) pendingSession(ctx context.Context, tx pgx.Tx, token string, now time.Time) (adminRow, error) {
	var adminID int64
	err := tx.QueryRow(ctx, `SELECT admin_id FROM admin_sessions
		WHERE token_hash = $1 AND stage = 'password_ok' AND expires_at > $2`, hashSecret(token), now).Scan(&adminID)
	if db.IsNoRows(err) {
		return adminRow{}, apperr.ErrUnauthorized
	}
	if err != nil {
		return adminRow{}, err
	}
	a, err := lockAdmin(ctx, tx, adminID)
	if err != nil {
		return adminRow{}, err
	}
	if a.Status != StatusActive {
		return adminRow{}, apperr.ErrUnauthorized
	}
	return a, nil
}

// promote 把会话升为已登录，并清零失败计数、记录登录时间与 IP。
func promote(ctx context.Context, tx pgx.Tx, token string, a adminRow, ip string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE admin_sessions SET stage = 'active', ip = $2, created_at = $3, last_seen_at = $3, expires_at = $4
		WHERE token_hash = $1`, hashSecret(token), ip, now, now.Add(SessionTTL)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE admins SET failed_attempts = 0, locked_until = NULL, last_login_at = $2, last_login_ip = $3, updated_at = $2
		WHERE id = $1`, a.ID, now, ip)
	return err
}

// ConfirmTOTPSetup 首次登录绑定 TOTP：校验动态码，生成恢复码（只返回这一次），会话升为已登录。
func (s *Service) ConfirmTOTPSetup(ctx context.Context, token, code, ip string) (Admin, []string, error) {
	now := s.d.Clock.Now()
	var a adminRow
	var codes []string
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var err error
		if a, err = s.pendingSession(ctx, tx, token, now); err != nil {
			return err
		}
		if a.locked(now) {
			bizErr = ErrLocked
			return nil
		}
		if a.TOTPConfirmed {
			return apperr.ErrInvalidState
		}
		if a.totpSecretEnc == nil {
			return ErrTOTPSetupRequired
		}
		if codes, bizErr, err = s.confirmTOTP(ctx, tx, a, code, now); err != nil || bizErr != nil {
			return err
		}
		if err := promote(ctx, tx, token, a, ip, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{AdminID: a.ID, IP: ip}.Entry("admin.totp_bind", "admin", strconv.FormatInt(a.ID, 10), nil))
	})
	if err != nil {
		return Admin{}, nil, err
	}
	if bizErr != nil {
		return Admin{}, nil, bizErr
	}
	a.TOTPConfirmed, a.LastLoginAt, a.LastLoginIP = true, &now, ip
	return a.Admin, codes, nil
}

// VerifyTOTP 第二步：用动态码完成登录。
func (s *Service) VerifyTOTP(ctx context.Context, token, code, ip string) (Admin, error) {
	return s.secondFactor(ctx, token, ip, "admin.login", func(tx pgx.Tx, a adminRow, now time.Time) (bool, error) {
		return s.checkTOTP(ctx, tx, a, code, now)
	})
}

// VerifyRecoveryCode 第二步：用一次性恢复码完成登录（手机丢失时使用）。
func (s *Service) VerifyRecoveryCode(ctx context.Context, token, recoveryCode, ip string) (Admin, error) {
	return s.secondFactor(ctx, token, ip, "admin.login_recovery_code", func(tx pgx.Tx, a adminRow, now time.Time) (bool, error) {
		tag, err := tx.Exec(ctx, `UPDATE admin_recovery_codes SET used_at = $3
			WHERE admin_id = $1 AND code_hash = $2 AND used_at IS NULL`, a.ID, hashRecoveryCode(recoveryCode), now)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() == 0 {
			return false, recordFailure(ctx, tx, a.ID, now)
		}
		return true, nil
	})
}

func (s *Service) secondFactor(ctx context.Context, token, ip, action string,
	check func(tx pgx.Tx, a adminRow, now time.Time) (bool, error)) (Admin, error) {
	now := s.d.Clock.Now()
	var a adminRow
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var err error
		if a, err = s.pendingSession(ctx, tx, token, now); err != nil {
			return err
		}
		if a.locked(now) {
			bizErr = ErrLocked
			return nil
		}
		if !a.TOTPConfirmed {
			bizErr = ErrTOTPSetupRequired
			return nil
		}
		ok, err := check(tx, a, now)
		if err != nil {
			return err
		}
		if !ok {
			bizErr = ErrInvalidTOTP
			return nil
		}
		if err := promote(ctx, tx, token, a, ip, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{AdminID: a.ID, IP: ip}.Entry(action, "admin", strconv.FormatInt(a.ID, 10), nil))
	})
	if err != nil {
		return Admin{}, err
	}
	if bizErr != nil {
		return Admin{}, bizErr
	}
	a.LastLoginAt, a.LastLoginIP = &now, ip
	return a.Admin, nil
}

const adminColsA = `a.id, a.username, a.role, a.status, a.totp_confirmed, a.last_login_at, a.last_login_ip, a.created_at`

// Authenticate 校验已登录会话（绝对 12 小时、空闲 2 小时），并刷新最近访问时间。
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, apperr.ErrUnauthorized
	}
	now := s.d.Clock.Now()
	h := hashSecret(token)
	var sess Session
	var stage string
	var lastSeen, expires time.Time
	a := &sess.Admin
	err := s.d.Pool.QueryRow(ctx, `SELECT s.stage, s.stepup_at, s.last_seen_at, s.expires_at, `+adminColsA+`
		FROM admin_sessions s JOIN admins a ON a.id = s.admin_id WHERE s.token_hash = $1`, h).
		Scan(&stage, &sess.StepUpAt, &lastSeen, &expires,
			&a.ID, &a.Username, &a.Role, &a.Status, &a.TOTPConfirmed, &a.LastLoginAt, &a.LastLoginIP, &a.CreatedAt)
	if db.IsNoRows(err) {
		return Session{}, apperr.ErrUnauthorized
	}
	if err != nil {
		return Session{}, err
	}
	if stage != "active" || !now.Before(expires) || !now.Before(lastSeen.Add(IdleTimeout)) || a.Status != StatusActive {
		return Session{}, apperr.ErrUnauthorized
	}
	if _, err := s.d.Pool.Exec(ctx, `UPDATE admin_sessions SET last_seen_at = $2 WHERE token_hash = $1`, h, now); err != nil {
		return Session{}, err
	}
	sess.TokenHash = h
	return sess, nil
}

// StepUp 高风险操作的二次验证：code 非空时校验动态码并记录验证时间；为空时要求 5 分钟内验证过。
func (s *Service) StepUp(ctx context.Context, sess Session, code string) error {
	now := s.d.Clock.Now()
	if NormalizeCode(code) == "" {
		if sess.StepUpAt != nil && now.Sub(*sess.StepUpAt) < StepUpWindow {
			return nil
		}
		return ErrTOTPRequired
	}
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		a, err := lockAdmin(ctx, tx, sess.Admin.ID)
		if err != nil {
			return err
		}
		if a.locked(now) {
			bizErr = ErrLocked
			return nil
		}
		ok, err := s.checkTOTP(ctx, tx, a, code, now)
		if err != nil {
			return err
		}
		if !ok {
			bizErr = ErrInvalidTOTP
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE admins SET failed_attempts = 0, updated_at = $2 WHERE id = $1`, a.ID, now); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE admin_sessions SET stepup_at = $2 WHERE token_hash = $1`, sess.TokenHash, now)
		return err
	})
	if err != nil {
		return err
	}
	return bizErr
}

func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.d.Pool.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash = $1`, hashSecret(token))
	return err
}
```

- [ ] **Step 6: 运行确认通过**

Run: `go test ./internal/admin/`
Expected: PASS

- [ ] **Step 7: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add migrations/00009_admin.sql internal/admin
git commit -m "feat(admin): two-step login with TOTP binding, recovery codes, lockout, sessions and step-up

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: 管理员管理与 `platform admin` 命令行

**Files:**
- Create: `internal/admin/manage.go`
- Test: `internal/admin/manage_test.go`（`package admin_test`，复用 Task 3 的 `helpers_test.go`）
- Create: `cmd/platform/admin.go`、`cmd/platform/admin_test.go`
- Modify: `cmd/platform/main.go`（`admin` 子命令分发）
- Modify: `go.mod` / `go.sum`（新增 `golang.org/x/term`、`github.com/mdp/qrterminal/v3`）

**Interfaces:**
- Consumes: Task 3 的 `Service`、`adminRow`、`lockAdmin`、`scanAdmin`、`adminCols`、`ensureSecret`、`confirmTOTP`、`recordFailure`、`OTPAuthURI`、`Issuer`、错误变量。
- Produces（Task 15 使用）：
  - `admin.ErrLastSuper`（last_super/409）。
  - `(*Service).List(ctx) ([]Admin, error)`
  - `(*Service).FindByUsername(ctx, username string) (Admin, error)`
  - `(*Service).Update(ctx, actor audit.Actor, id int64, role *Role, status *string) (Admin, error)`
  - `(*Service).ResetTOTP(ctx, actor audit.Actor, id int64) error`
  - `(*Service).ChangePassword(ctx, sess Session, oldPassword, newPassword, ip string) error`
  - `(*Service).EnrollTOTP(ctx, id int64) (string, error)`（返回 otpauth URI）
  - `(*Service).ConfirmEnrollment(ctx, id int64, code string) ([]string, error)`
  - `cmd/platform`：`runAdmin(ctx, svc *admin.Service, args []string, out io.Writer, prompt prompter) error`，`type prompter func(label string, secret bool) (string, error)`。

- [ ] **Step 1: 添加依赖**

Run: `go get golang.org/x/term@v0.47.0 github.com/mdp/qrterminal/v3@v3.2.1`
Expected: `go.mod` 出现两条依赖。

- [ ] **Step 2: 写管理功能的失败测试**

`internal/admin/manage_test.go`：

```go
package admin_test

import (
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
)

func ptrRole(r admin.Role) *admin.Role { return &r }
func ptrStr(s string) *string         { return &s }

func TestListAndFind(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	e.create("alice", admin.RoleOps)
	list, err := e.svc.List(e.ctx)
	if err != nil || len(list) != 2 || list[0].Username != "root" || list[1].Role != admin.RoleOps {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if a, err := e.svc.FindByUsername(e.ctx, " Alice "); err != nil || a.Username != "alice" {
		t.Fatalf("find = %+v, %v", a, err)
	}
	if _, err := e.svc.FindByUsername(e.ctx, "nobody"); codeOf(err) != "not_found" {
		t.Fatalf("find missing = %v", err)
	}
}

func TestUpdateProtectsLastSuper(t *testing.T) {
	e := newEnv(t)
	root := e.create("root", admin.RoleSuper)
	actor := audit.Actor{AdminID: root.ID, IP: ip}
	if _, err := e.svc.Update(e.ctx, actor, root.ID, ptrRole(admin.RoleOps), nil); codeOf(err) != "last_super" {
		t.Fatalf("demote last super = %v", err)
	}
	if _, err := e.svc.Update(e.ctx, actor, root.ID, nil, ptrStr(admin.StatusDisabled)); codeOf(err) != "last_super" {
		t.Fatalf("disable last super = %v", err)
	}
	second := e.create("boss", admin.RoleSuper)
	a, err := e.svc.Update(e.ctx, actor, root.ID, ptrRole(admin.RoleOps), nil)
	if err != nil || a.Role != admin.RoleOps {
		t.Fatalf("demote with another super = %+v, %v", a, err)
	}
	if _, err := e.svc.Update(e.ctx, actor, second.ID, nil, ptrStr(admin.StatusDisabled)); codeOf(err) != "last_super" {
		t.Fatalf("disable the remaining super = %v", err)
	}
	if _, err := e.svc.Update(e.ctx, actor, root.ID, ptrRole(admin.Role("god")), nil); codeOf(err) != "bad_request" {
		t.Fatalf("bad role = %v", err)
	}
	if _, err := e.svc.Update(e.ctx, actor, root.ID, nil, ptrStr("gone")); codeOf(err) != "bad_request" {
		t.Fatalf("bad status = %v", err)
	}
	if _, err := e.svc.Update(e.ctx, actor, 9999, ptrRole(admin.RoleOps), nil); codeOf(err) != "not_found" {
		t.Fatalf("missing admin = %v", err)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = 'admin.update' AND actor_id = $1`, root.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("admin.update audit rows = %d, want 1", n)
	}
}

// 两个超级管理员同时互相降级，只能成功一个。
func TestConcurrentDemotionKeepsOneSuper(t *testing.T) {
	e := newEnv(t)
	a := e.create("root", admin.RoleSuper)
	b := e.create("boss", admin.RoleSuper)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []int64{a.ID, b.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.svc.Update(e.ctx, audit.Actor{}, id, ptrRole(admin.RoleOps), nil)
		}()
	}
	wg.Wait()
	var supers int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM admins WHERE role = 'super' AND status = 'active'`).Scan(&supers)
	if supers != 1 || (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("supers = %d, errs = %v", supers, errs)
	}
}

func TestDisableKillsSessionsAndBlocksLogin(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	ops := e.create("alice", admin.RoleOps)
	_, _, token := e.enroll("alice")
	if _, err := e.svc.Update(e.ctx, audit.Actor{}, ops.ID, nil, ptrStr(admin.StatusDisabled)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(e.ctx, token); codeOf(err) != "unauthorized" {
		t.Fatalf("session after disable = %v", err)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM admin_sessions WHERE admin_id = $1`, ops.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("sessions left = %d", n)
	}
	if _, err := e.svc.Login(e.ctx, "alice", password, ip); codeOf(err) != "invalid_credentials" {
		t.Fatalf("login while disabled = %v", err)
	}
	if _, err := e.svc.Update(e.ctx, audit.Actor{}, ops.ID, nil, ptrStr(admin.StatusActive)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Login(e.ctx, "alice", password, ip); err != nil {
		t.Fatalf("login after re-enable = %v", err)
	}
}

func TestResetTOTPForcesRebinding(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	ops := e.create("alice", admin.RoleOps)
	oldSecret, _, token := e.enroll("alice")
	if err := e.svc.ResetTOTP(e.ctx, audit.Actor{AdminID: 1, IP: ip}, ops.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(e.ctx, token); codeOf(err) != "unauthorized" {
		t.Fatalf("session after reset = %v", err)
	}
	e.clk.Advance(30 * time.Second)
	res, err := e.svc.Login(e.ctx, "alice", password, ip)
	if err != nil || !res.NeedTOTPSetup {
		t.Fatalf("login after reset = %+v, %v", res, err)
	}
	if _, _, err := e.svc.ConfirmTOTPSetup(e.ctx, res.Token, e.code(oldSecret, 0), ip); codeOf(err) != "invalid_totp" {
		t.Fatalf("old secret still accepted: %v", err)
	}
	var codes int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM admin_recovery_codes WHERE admin_id = $1`, ops.ID).Scan(&codes)
	if codes != 0 {
		t.Fatalf("recovery codes left = %d", codes)
	}
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	e.create("root", admin.RoleSuper)
	secret, _, token := e.enroll("root")
	other := e.login("root", secret)
	sess, _ := e.svc.Authenticate(e.ctx, token)
	if err := e.svc.ChangePassword(e.ctx, sess, "wrong password!", "a brand new password", ip); codeOf(err) != "wrong_password" {
		t.Fatalf("wrong old password = %v", err)
	}
	if err := e.svc.ChangePassword(e.ctx, sess, password, "short", ip); codeOf(err) != "bad_request" {
		t.Fatalf("short new password = %v", err)
	}
	if err := e.svc.ChangePassword(e.ctx, sess, password, "a brand new password", ip); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(e.ctx, token); err != nil {
		t.Fatalf("current session must survive: %v", err)
	}
	if _, err := e.svc.Authenticate(e.ctx, other); codeOf(err) != "unauthorized" {
		t.Fatalf("other session must be revoked: %v", err)
	}
	if _, err := e.svc.Login(e.ctx, "root", password, ip); codeOf(err) != "invalid_credentials" {
		t.Fatalf("old password still works: %v", err)
	}
	if _, err := e.svc.Login(e.ctx, "root", "a brand new password", ip); err != nil {
		t.Fatalf("new password = %v", err)
	}
}

func TestEnrollmentWithoutSession(t *testing.T) {
	e := newEnv(t)
	a := e.create("root", admin.RoleSuper)
	uri, err := e.svc.EnrollTOTP(e.ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	uri2, _ := e.svc.EnrollTOTP(e.ctx, a.ID)
	if uri != uri2 {
		t.Fatal("EnrollTOTP must reuse the pending secret")
	}
	secret := secretFromURI(t, uri)
	if _, err := e.svc.ConfirmEnrollment(e.ctx, a.ID, "000000"); codeOf(err) != "invalid_totp" {
		t.Fatalf("wrong code = %v", err)
	}
	codes, err := e.svc.ConfirmEnrollment(e.ctx, a.ID, e.code(secret, 0))
	if err != nil || len(codes) != admin.RecoveryCodeCount {
		t.Fatalf("confirm = %v, %v", codes, err)
	}
	if _, err := e.svc.EnrollTOTP(e.ctx, a.ID); codeOf(err) != "invalid_state" {
		t.Fatalf("enroll after confirm = %v", err)
	}
	e.login("root", secret)
}
```

在 `internal/admin/helpers_test.go` 末尾追加（`url` 已在该文件的 import 中）：

```go
func secretFromURI(t *testing.T, uri string) string {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("secret")
}
```

- [ ] **Step 3: 运行确认失败**

Run: `go test ./internal/admin/`
Expected: 编译失败（`List`、`Update` 等未定义）。

- [ ] **Step 4: 实现管理功能**

`internal/admin/manage.go`：

```go
package admin

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

var (
	ErrLastSuper     = apperr.New("last_super", "不能停用或降级最后一个启用中的超级管理员", 409)
	errWrongPassword = apperr.New("wrong_password", "原密码不正确", 400)
)

func (s *Service) List(ctx context.Context) ([]Admin, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT `+adminCols+` FROM admins ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Admin, error) { return scanAdmin(r) })
}

func (s *Service) FindByUsername(ctx context.Context, username string) (Admin, error) {
	return scanAdmin(s.d.Pool.QueryRow(ctx, `SELECT `+adminCols+` FROM admins WHERE username = $1`,
		strings.ToLower(strings.TrimSpace(username))))
}

// Update 修改角色和 / 或状态；不能让系统失去最后一个启用中的超级管理员。停用时立即删除其全部会话。
func (s *Service) Update(ctx context.Context, actor audit.Actor, id int64, role *Role, status *string) (Admin, error) {
	if role != nil && !role.Valid() {
		return Admin{}, apperr.BadRequest("角色只能是 super、ops 或 finance")
	}
	if status != nil && *status != StatusActive && *status != StatusDisabled {
		return Admin{}, apperr.BadRequest("状态只能是 active 或 disabled")
	}
	now := s.d.Clock.Now()
	var out Admin
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		// 串行化所有角色与状态变更，保证「最后一个超级管理员」检查不会被并发绕过
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('admin:roles'))`); err != nil {
			return err
		}
		a, err := lockAdmin(ctx, tx, id)
		if err != nil {
			return err
		}
		newRole, newStatus := a.Role, a.Status
		if role != nil {
			newRole = *role
		}
		if status != nil {
			newStatus = *status
		}
		if a.Role == RoleSuper && a.Status == StatusActive && (newRole != RoleSuper || newStatus != StatusActive) {
			var others int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM admins WHERE role = 'super' AND status = 'active' AND id <> $1`, id).
				Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				return ErrLastSuper
			}
		}
		if out, err = scanAdmin(tx.QueryRow(ctx, `UPDATE admins SET role = $2, status = $3, updated_at = $4 WHERE id = $1
			RETURNING `+adminCols, id, string(newRole), newStatus, now)); err != nil {
			return err
		}
		if newStatus == StatusDisabled {
			if _, err := tx.Exec(ctx, `DELETE FROM admin_sessions WHERE admin_id = $1`, id); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, now, actor.Entry("admin.update", "admin", strconv.FormatInt(id, 10), map[string]any{
			"before": map[string]any{"role": a.Role, "status": a.Status},
			"after":  map[string]any{"role": newRole, "status": newStatus},
		}))
	})
	return out, err
}

// ResetTOTP 清除管理员的 TOTP 密钥、恢复码与全部会话；该管理员下次登录时重新绑定。
func (s *Service) ResetTOTP(ctx context.Context, actor audit.Actor, id int64) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockAdmin(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE admins SET totp_secret_enc = NULL, totp_confirmed = false, updated_at = $2 WHERE id = $1`,
			id, now); err != nil {
			return err
		}
		for _, table := range []string{"admin_recovery_codes", "admin_sessions", "admin_totp_used"} {
			if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE admin_id = $1`, id); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, now, actor.Entry("admin.reset_totp", "admin", strconv.FormatInt(id, 10), nil))
	})
}

// ChangePassword 修改自己的密码；原密码错误计入失败次数。成功后注销该管理员的其他会话。
func (s *Service) ChangePassword(ctx context.Context, sess Session, oldPassword, newPassword, ip string) error {
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	hash := HashPassword(newPassword)
	now := s.d.Clock.Now()
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		a, err := lockAdmin(ctx, tx, sess.Admin.ID)
		if err != nil {
			return err
		}
		if a.locked(now) {
			bizErr = ErrLocked
			return nil
		}
		if !VerifyPassword(a.passwordHash, oldPassword) {
			bizErr = errWrongPassword
			return recordFailure(ctx, tx, a.ID, now)
		}
		if _, err := tx.Exec(ctx, `UPDATE admins SET password_hash = $2, failed_attempts = 0, updated_at = $3 WHERE id = $1`,
			a.ID, hash, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM admin_sessions WHERE admin_id = $1 AND token_hash <> $2`, a.ID, sess.TokenHash); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{AdminID: a.ID, IP: ip}.Entry("admin.password_change", "admin", strconv.FormatInt(a.ID, 10), nil))
	})
	if err != nil {
		return err
	}
	return bizErr
}

// EnrollTOTP 供命令行在没有会话的情况下开始绑定：生成或复用待确认的密钥，返回 otpauth URI。
func (s *Service) EnrollTOTP(ctx context.Context, id int64) (string, error) {
	now := s.d.Clock.Now()
	var uri string
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		a, err := lockAdmin(ctx, tx, id)
		if err != nil {
			return err
		}
		if a.TOTPConfirmed {
			return apperr.ErrInvalidState
		}
		secret, err := s.ensureSecret(ctx, tx, a, now)
		if err != nil {
			return err
		}
		uri = OTPAuthURI(Issuer, a.Username, secret)
		return nil
	})
	return uri, err
}

// ConfirmEnrollment 供命令行确认绑定，返回恢复码（只显示这一次）。
func (s *Service) ConfirmEnrollment(ctx context.Context, id int64, code string) ([]string, error) {
	now := s.d.Clock.Now()
	var codes []string
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		a, err := lockAdmin(ctx, tx, id)
		if err != nil {
			return err
		}
		if a.TOTPConfirmed {
			return apperr.ErrInvalidState
		}
		if a.locked(now) {
			bizErr = ErrLocked
			return nil
		}
		if a.totpSecretEnc == nil {
			return ErrTOTPSetupRequired
		}
		if codes, bizErr, err = s.confirmTOTP(ctx, tx, a, code, now); err != nil || bizErr != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{}.Entry("admin.totp_bind", "admin", strconv.FormatInt(id, 10), nil))
	})
	if err != nil {
		return nil, err
	}
	if bizErr != nil {
		return nil, bizErr
	}
	return codes, nil
}
```

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/admin/`
Expected: PASS

- [ ] **Step 6: 写命令行的失败测试**

`cmd/platform/admin_test.go`：

```go
package main

import (
	"bytes"
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func newAdminService(t *testing.T) (*admin.Service, *clock.Fake) {
	t.Helper()
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(dbtest.T0)
	return admin.NewService(admin.Deps{Pool: dbtest.New(t), Clock: clk, Box: box}), clk
}

var uriRe = regexp.MustCompile(`otpauth://\S+`)

// scripted 依次返回两次密码，遇到动态码提示时从已输出的 otpauth 链接计算当前动态码。
func scripted(t *testing.T, out *bytes.Buffer, clk *clock.Fake, pw1, pw2 string) prompter {
	answers := []string{pw1, pw2}
	return func(label string, secret bool) (string, error) {
		if strings.Contains(label, "动态码") {
			u, err := url.Parse(uriRe.FindString(out.String()))
			if err != nil {
				t.Fatal(err)
			}
			return admin.TOTPCode(u.Query().Get("secret"), admin.TOTPStep(clk.Now()))
		}
		if !secret {
			t.Fatalf("password prompt %q must not echo", label)
		}
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}
}

func TestAdminCreateBindsTOTPAndPrintsRecoveryCodes(t *testing.T) {
	svc, clk := newAdminService(t)
	ctx := context.Background()
	var out bytes.Buffer
	pw := "correct horse battery"
	if err := runAdmin(ctx, svc, []string{"create", "--username", "root"}, &out, scripted(t, &out, clk, pw, pw)); err != nil {
		t.Fatalf("create: %v\n%s", err, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "otpauth://totp/Spark%20Cloud:root?") || !strings.Contains(text, "恢复码") {
		t.Fatalf("output = %s", text)
	}
	if got := len(regexp.MustCompile(`(?m)^  [a-z0-9]{5}-[a-z0-9]{5}$`).FindAllString(text, -1)); got != admin.RecoveryCodeCount {
		t.Fatalf("printed %d recovery codes", got)
	}
	a, err := svc.FindByUsername(ctx, "root")
	if err != nil || a.Role != admin.RoleSuper || !a.TOTPConfirmed {
		t.Fatalf("admin = %+v, %v", a, err)
	}

	out.Reset()
	if err := runAdmin(ctx, svc, []string{"reset-totp", "--username", "root"}, &out, nil); err != nil {
		t.Fatal(err)
	}
	if a, _ := svc.FindByUsername(ctx, "root"); a.TOTPConfirmed {
		t.Fatal("TOTP not reset")
	}
	res, err := svc.Login(ctx, "root", pw, "127.0.0.1")
	if err != nil || !res.NeedTOTPSetup {
		t.Fatalf("login after reset = %+v, %v", res, err)
	}
}

func TestAdminCreateErrors(t *testing.T) {
	svc, clk := newAdminService(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := runAdmin(ctx, svc, nil, &out, nil); err == nil || !strings.Contains(err.Error(), "用法") {
		t.Fatalf("no args = %v", err)
	}
	if err := runAdmin(ctx, svc, []string{"create"}, &out, nil); err == nil || !strings.Contains(err.Error(), "--username") {
		t.Fatalf("missing username = %v", err)
	}
	err := runAdmin(ctx, svc, []string{"create", "--username", "root"}, &out, scripted(t, &out, clk, "correct horse battery", "different password!"))
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("mismatch = %v", err)
	}
	if _, err := svc.FindByUsername(ctx, "root"); err == nil {
		t.Fatal("admin must not be created when passwords differ")
	}
	if err := runAdmin(ctx, svc, []string{"reset-totp", "--username", "ghost"}, &out, nil); err == nil {
		t.Fatal("reset-totp for unknown admin must fail")
	}
}
```

- [ ] **Step 7: 运行确认失败**

Run: `go test ./cmd/platform/`
Expected: 编译失败（`runAdmin`、`prompter` 未定义）。

- [ ] **Step 8: 实现命令行**

`cmd/platform/admin.go`：

```go
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mdp/qrterminal/v3"
	"golang.org/x/term"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
)

const adminUsage = `用法：
  platform admin create --username <用户名> [--role super|ops|finance]
  platform admin reset-totp --username <用户名>`

// prompter 读取一行输入；secret 为 true 时不回显（用于密码）。
type prompter func(label string, secret bool) (string, error)

// runAdmin 处理 `platform admin ...` 子命令。
func runAdmin(ctx context.Context, svc *admin.Service, args []string, out io.Writer, prompt prompter) error {
	if len(args) == 0 {
		return errors.New(adminUsage)
	}
	switch args[0] {
	case "create":
		return adminCreate(ctx, svc, args[1:], out, prompt)
	case "reset-totp":
		return adminResetTOTP(ctx, svc, args[1:], out)
	}
	return errors.New(adminUsage)
}

func adminCreate(ctx context.Context, svc *admin.Service, args []string, out io.Writer, prompt prompter) error {
	fs := flag.NewFlagSet("admin create", flag.ContinueOnError)
	fs.SetOutput(out)
	username := fs.String("username", "", "管理员用户名")
	role := fs.String("role", string(admin.RoleSuper), "角色：super / ops / finance")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" {
		return errors.New("请用 --username 指定用户名\n" + adminUsage)
	}
	pw, err := prompt("密码（至少 12 个字符）：", true)
	if err != nil {
		return err
	}
	pw2, err := prompt("再次输入密码：", true)
	if err != nil {
		return err
	}
	if pw != pw2 {
		return errors.New("两次输入的密码不一致")
	}
	a, err := svc.Create(ctx, audit.Actor{}, *username, pw, admin.Role(*role))
	if err != nil {
		return err
	}
	uri, err := svc.EnrollTOTP(ctx, a.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\n请用验证器 App（如 Google Authenticator、Microsoft Authenticator）扫描下方二维码，或手动添加以下链接：\n%s\n\n", uri)
	qrterminal.GenerateHalfBlock(uri, qrterminal.L, out)
	for attempt := 0; attempt < 3; attempt++ {
		code, err := prompt("\n输入验证器显示的 6 位动态码：", false)
		if err != nil {
			return err
		}
		codes, err := svc.ConfirmEnrollment(ctx, a.ID, code)
		if err == nil {
			fmt.Fprintf(out, "\n管理员 %s（%s）已创建并绑定动态码。\n以下 %d 个恢复码每个只能用一次，请立即离线保存，之后不会再显示：\n",
				a.Username, a.Role, len(codes))
			for _, c := range codes {
				fmt.Fprintln(out, "  "+c)
			}
			return nil
		}
		if ae := apperr.As(err); ae == nil || ae.Code != "invalid_totp" {
			return err
		}
		fmt.Fprintln(out, "动态码不正确，请等验证器刷新出下一个动态码后重试。")
	}
	return errors.New("动态码连续 3 次错误：管理员已创建但尚未绑定动态码，首次登录后台时会要求重新绑定")
}

func adminResetTOTP(ctx context.Context, svc *admin.Service, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("admin reset-totp", flag.ContinueOnError)
	fs.SetOutput(out)
	username := fs.String("username", "", "管理员用户名")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" {
		return errors.New("请用 --username 指定用户名\n" + adminUsage)
	}
	a, err := svc.FindByUsername(ctx, *username)
	if err != nil {
		return fmt.Errorf("找不到管理员 %s: %w", *username, err)
	}
	if err := svc.ResetTOTP(ctx, audit.Actor{}, a.ID); err != nil {
		return err
	}
	fmt.Fprintf(out, "已重置 %s 的动态码并注销其全部会话，该管理员下次登录时需要重新绑定。\n", a.Username)
	return nil
}

// terminalPrompter 从终端读取输入；密码不回显。输入不是终端（例如管道）时按行读取。
func terminalPrompter(in *os.File, out io.Writer) prompter {
	reader := bufio.NewReader(in)
	return func(label string, secret bool) (string, error) {
		fmt.Fprint(out, label)
		if secret && term.IsTerminal(int(in.Fd())) {
			b, err := term.ReadPassword(int(in.Fd()))
			fmt.Fprintln(out)
			return string(b), err
		}
		line, err := reader.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
}
```

- [ ] **Step 9: 在 main.go 分发子命令**

`cmd/platform/main.go`：

1. import 增加 `"fmt"`、`"spark-cloud/internal/admin"`、`"spark-cloud/internal/platform/clock"`、`"spark-cloud/internal/platform/secretbox"`。
2. 把 `main` 改为：

```go
func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		if err := adminMain(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("platform exited", "err", err)
		os.Exit(1)
	}
}

// adminMain 执行 `platform admin ...`：使用与服务相同的配置连接数据库（会先执行迁移）。
func adminMain(args []string) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	box, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		return err
	}
	svc := admin.NewService(admin.Deps{Pool: pool, Clock: clock.Real{}, Box: box})
	return runAdmin(ctx, svc, args, os.Stdout, terminalPrompter(os.Stdin, os.Stdout))
}
```

- [ ] **Step 10: 运行确认通过**

Run: `go test ./cmd/platform/ ./internal/admin/`
Expected: PASS

- [ ] **Step 11: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add go.mod go.sum internal/admin cmd/platform
git commit -m "feat(admin): admin management (last-super guard, reset TOTP, password change) and platform admin CLI

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: 文件存储与用户上传接口

**Files:**
- Create: `migrations/00010_uploads.sql`
- Create: `internal/storage/store.go`、`internal/storage/uploads.go`
- Test: `internal/storage/store_test.go`、`internal/storage/uploads_test.go`
- Create: `internal/api/uploads.go`、`internal/api/uploads_test.go`
- Modify: `internal/api/server.go`（`Uploads` 字段、路由、写请求类型检查）
- Modify: `internal/api/api_test.go`（`newTestEnv` 注入上传服务）
- Modify: `internal/app/app.go`、`internal/app/e2e_test.go`（组装上传服务）

**Interfaces:**
- Consumes: `randx.ID`、`apperr`、`clock.Clock`、`db.IsNoRows`。
- Produces（Task 6、18 使用）：
  - `storage.Store` 接口：`Put(ctx, key string, r io.Reader) (int64, error)`、`Open(ctx, key string) (io.ReadCloser, error)`、`Delete(ctx, key string) error`。
  - `storage.ValidKey(key string) bool`、`storage.NewLocalDisk(root string) (*storage.LocalDisk, error)`。
  - 常量 `storage.MaxUploadSize = 10 << 20`、`storage.PurposeLicense = "license"`；错误 `storage.ErrFileTooLarge`（file_too_large/413）、`storage.ErrUnsupportedType`（unsupported_file_type/415）。
  - `storage.Upload{Key, UserID, Size, ContentType, Purpose, CreatedAt}`（UserID 不输出 JSON）。
  - `storage.NewService(pool *pgxpool.Pool, clk clock.Clock, store Store) *storage.Service`
  - `(*Service).Save(ctx, userID int64, purpose string, r io.Reader) (Upload, error)`
  - `(*Service).CheckOwned(ctx, key string, userID int64, purpose string) error`（不属于该用户或用途不符返回 bad_request「营业执照文件无效，请重新上传」）
  - `(*Service).Open(ctx, key string) (Upload, io.ReadCloser, error)`
  - `api.Server.Uploads *storage.Service`；`POST /api/v1/uploads`（multipart，字段 `file`）→ 201 `{key, size, content_type, purpose, created_at}`。
  - `app.App.Uploads *storage.Service`；`app.Build` 在 `cfg.UploadDir` 为空时报错。

- [ ] **Step 1: 写迁移**

`migrations/00010_uploads.sql`：

```sql
-- +goose Up
CREATE TABLE uploads (
    key          TEXT PRIMARY KEY,               -- yyyy/mm/<32 位随机>.<ext>，即本地磁盘上的相对路径
    user_id      BIGINT NOT NULL REFERENCES users (id),
    size         BIGINT NOT NULL,
    content_type TEXT NOT NULL,
    purpose      TEXT NOT NULL CHECK (purpose IN ('license')),
    created_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX uploads_user_idx ON uploads (user_id, created_at DESC);

-- +goose Down
DROP TABLE uploads;
```

- [ ] **Step 2: 写存储层的失败测试**

`internal/storage/store_test.go`：

```go
package storage_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/storage"
)

const goodKey = "2026/01/abcdefghijklmnopqrstuvwxyz012345.png"

func TestValidKey(t *testing.T) {
	for key, ok := range map[string]bool{
		goodKey: true,
		"2026/01/abcdefghijklmnopqrstuvwxyz012345.pdf":    true,
		"2026/01/abcdefghijklmnopqrstuvwxyz012345.exe":    false,
		"2026/01/ABCDEFGHIJKLMNOPQRSTUVWXYZ012345.png":    false,
		"../01/abcdefghijklmnopqrstuvwxyz012345.png":      false,
		"2026/01/../abcdefghijklmnopqrstuvwxyz0123.png":   false,
		"/2026/01/abcdefghijklmnopqrstuvwxyz012345.png":   false,
		"2026/01/abcdefghijklmnopqrstuvwxyz012345.png/..": false,
		"": false,
	} {
		if storage.ValidKey(key) != ok {
			t.Fatalf("ValidKey(%q) = %v, want %v", key, !ok, ok)
		}
	}
}

func TestLocalDiskRoundTrip(t *testing.T) {
	root := t.TempDir()
	d, err := storage.NewLocalDisk(filepath.Join(root, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	n, err := d.Put(ctx, goodKey, strings.NewReader("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Put = %d, %v", n, err)
	}
	info, err := os.Stat(filepath.Join(root, "uploads", "2026", "01", "abcdefghijklmnopqrstuvwxyz012345.png"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("stored file = %v, %v", info, err)
	}
	rc, err := d.Open(ctx, goodKey)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}
	if err := d.Delete(ctx, goodKey); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Open(ctx, goodKey); apperr.As(err) == nil || apperr.As(err).Code != "not_found" {
		t.Fatalf("open after delete = %v", err)
	}
	if err := d.Delete(ctx, goodKey); err != nil {
		t.Fatalf("deleting a missing file must succeed: %v", err)
	}
	if _, err := d.Put(ctx, "../escape.png", strings.NewReader("x")); err == nil {
		t.Fatal("Put accepted a traversal key")
	}
	if _, err := d.Open(ctx, "../../etc/passwd"); apperr.As(err) == nil {
		t.Fatalf("Open traversal = %v", err)
	}
	if _, err := storage.NewLocalDisk(""); err == nil {
		t.Fatal("empty root must be rejected")
	}
}
```

`internal/storage/uploads_test.go`：

```go
package storage_test

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/storage"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

var (
	pngHeader = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpgHeader = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	pdfHeader = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
)

func newUploads(t *testing.T) (*storage.Service, *pgxpool.Pool, *clock.Fake) {
	t.Helper()
	pool := dbtest.New(t)
	disk, err := storage.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(dbtest.T0)
	return storage.NewService(pool, clk, disk), pool, clk
}

func code(err error) string {
	if ae := apperr.As(err); ae != nil {
		return ae.Code
	}
	if err != nil {
		return "raw: " + err.Error()
	}
	return ""
}

func TestSaveDetectsTypeAndRecordsOwner(t *testing.T) {
	svc, pool, _ := newUploads(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000001", true)
	for header, ext := range map[string]string{string(pngHeader): "png", string(jpgHeader): "jpg", string(pdfHeader): "pdf"} {
		up, err := svc.Save(ctx, uid, storage.PurposeLicense, strings.NewReader(header+"payload"))
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^2026/01/[a-z0-9]{32}\.` + ext + `$`).MatchString(up.Key) || up.Size != int64(len(header)+7) {
			t.Fatalf("upload = %+v", up)
		}
		if err := svc.CheckOwned(ctx, up.Key, uid, storage.PurposeLicense); err != nil {
			t.Fatalf("CheckOwned own file: %v", err)
		}
		got, rc, err := svc.Open(ctx, up.Key)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if got.UserID != uid || !bytes.HasPrefix(b, []byte(header)) {
			t.Fatalf("open = %+v %q", got, b)
		}
	}
}

func TestSaveRejectsBadFiles(t *testing.T) {
	svc, pool, _ := newUploads(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000001", true)
	cases := []struct {
		name    string
		purpose string
		body    io.Reader
		want    string
	}{
		{"text disguised", storage.PurposeLicense, strings.NewReader("just text, not an image"), "unsupported_file_type"},
		{"html", storage.PurposeLicense, strings.NewReader("<html><script>alert(1)</script>"), "unsupported_file_type"},
		{"empty", storage.PurposeLicense, strings.NewReader(""), "bad_request"},
		{"bad purpose", "avatar", bytes.NewReader(pngHeader), "bad_request"},
		{"too large", storage.PurposeLicense, io.MultiReader(bytes.NewReader(pngHeader), bytes.NewReader(make([]byte, storage.MaxUploadSize))), "file_too_large"},
	}
	for _, c := range cases {
		if _, err := svc.Save(ctx, uid, c.purpose, c.body); code(err) != c.want {
			t.Fatalf("%s: err = %v, want %s", c.name, err, c.want)
		}
	}
	exact := append(append([]byte{}, pngHeader...), make([]byte, storage.MaxUploadSize-len(pngHeader))...)
	if _, err := svc.Save(ctx, uid, storage.PurposeLicense, bytes.NewReader(exact)); err != nil {
		t.Fatalf("exactly 10MB must be accepted: %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM uploads`).Scan(&n)
	if n != 1 {
		t.Fatalf("uploads rows = %d, want 1", n)
	}
}

func TestCheckOwnedRejectsOthers(t *testing.T) {
	svc, pool, _ := newUploads(t)
	ctx := context.Background()
	owner := dbtest.CreateUser(t, pool, "13900000001", true)
	other := dbtest.CreateUser(t, pool, "13900000002", true)
	up, err := svc.Save(ctx, owner, storage.PurposeLicense, bytes.NewReader(pngHeader))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		key string
		uid int64
	}{{up.Key, other}, {"2026/01/abcdefghijklmnopqrstuvwxyz012345.png", owner}, {"../x.png", owner}, {"https://evil/x.png", owner}} {
		if err := svc.CheckOwned(ctx, c.key, c.uid, storage.PurposeLicense); code(err) != "bad_request" {
			t.Fatalf("CheckOwned(%q, %d) = %v", c.key, c.uid, err)
		}
	}
	if _, _, err := svc.Open(ctx, "2026/01/abcdefghijklmnopqrstuvwxyz012345.png"); code(err) != "not_found" {
		t.Fatalf("open unknown = %v", err)
	}
}

func TestSaveDailyLimit(t *testing.T) {
	svc, pool, clk := newUploads(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000001", true)
	for i := 0; i < 20; i++ {
		if _, err := svc.Save(ctx, uid, storage.PurposeLicense, bytes.NewReader(pngHeader)); err != nil {
			t.Fatalf("upload %d: %v", i+1, err)
		}
	}
	if _, err := svc.Save(ctx, uid, storage.PurposeLicense, bytes.NewReader(pngHeader)); code(err) != "rate_limited" {
		t.Fatalf("21st upload = %v", err)
	}
	clk.Advance(24 * time.Hour)
	if _, err := svc.Save(ctx, uid, storage.PurposeLicense, bytes.NewReader(pngHeader)); err != nil {
		t.Fatalf("next day: %v", err)
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `go test ./internal/storage/`
Expected: 编译失败（包不存在）。

- [ ] **Step 4: 实现存储层**

`internal/storage/store.go`：

```go
// Package storage 保存用户上传的文件（营业执照等）：Store 接口 + 本地磁盘实现，uploads 表记录归属。
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"spark-cloud/internal/platform/apperr"
)

// Store 是文件存储后端；以后可替换为对象存储。
type Store interface {
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

var keyRe = regexp.MustCompile(`^\d{4}/\d{2}/[a-z0-9]{32}\.(jpg|png|pdf)$`)

// ValidKey 报告 key 是否是服务端签发格式的文件键（同时防止路径穿越）。
func ValidKey(key string) bool { return keyRe.MatchString(key) }

var errBadKey = apperr.BadRequest("文件键无效")

// LocalDisk 把文件保存在本机目录下，key 即相对路径。
type LocalDisk struct {
	root string
}

func NewLocalDisk(root string) (*LocalDisk, error) {
	if root == "" {
		return nil, errors.New("storage: 上传目录未设置")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("storage: 创建上传目录: %w", err)
	}
	return &LocalDisk{root: abs}, nil
}

func (d *LocalDisk) path(key string) string { return filepath.Join(d.root, filepath.FromSlash(key)) }

// Put 先写临时文件再改名，避免读到写了一半的文件。
func (d *LocalDisk) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	if !ValidKey(key) {
		return 0, errBadKey
	}
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o640)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	return n, nil
}

func (d *LocalDisk) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if !ValidKey(key) {
		return nil, apperr.ErrNotFound
	}
	f, err := os.Open(d.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, apperr.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Delete 删除文件；文件不存在视为成功。
func (d *LocalDisk) Delete(_ context.Context, key string) error {
	if !ValidKey(key) {
		return errBadKey
	}
	if err := os.Remove(d.path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
```

`internal/storage/uploads.go`：

```go
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

const (
	MaxUploadSize  = 10 << 20 // 10MB
	PurposeLicense = "license"

	maxUploadsPerDay = 20 // 每个用户每 24 小时最多上传次数，防止占满磁盘
)

var (
	ErrFileTooLarge    = apperr.New("file_too_large", "文件不能超过 10MB", 413)
	ErrUnsupportedType = apperr.New("unsupported_file_type", "仅支持 JPG、PNG、PDF 文件", 415)
	errInvalidUpload   = apperr.BadRequest("营业执照文件无效，请重新上传")
)

// extByType 是允许的文件类型（按文件内容判断，不看扩展名）。
var extByType = map[string]string{"image/jpeg": "jpg", "image/png": "png", "application/pdf": "pdf"}

type Upload struct {
	Key         string    `json:"key"`
	UserID      int64     `json:"-"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	Purpose     string    `json:"purpose"`
	CreatedAt   time.Time `json:"created_at"`
}

type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
	store Store
}

func NewService(pool *pgxpool.Pool, clk clock.Clock, store Store) *Service {
	return &Service{pool: pool, clock: clk, store: store}
}

// Save 读取文件内容（最多 10MB），按内容判断类型后保存，并登记归属。
func (s *Service) Save(ctx context.Context, userID int64, purpose string, r io.Reader) (Upload, error) {
	if purpose != PurposeLicense {
		return Upload{}, apperr.BadRequest("不支持的上传用途")
	}
	now := s.clock.Now()
	var recent int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE user_id = $1 AND created_at > $2`,
		userID, now.Add(-24*time.Hour)).Scan(&recent); err != nil {
		return Upload{}, err
	}
	if recent >= maxUploadsPerDay {
		return Upload{}, apperr.ErrRateLimited
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxUploadSize+1))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || len(data) > MaxUploadSize {
		return Upload{}, ErrFileTooLarge
	}
	if err != nil {
		return Upload{}, apperr.BadRequest("读取上传文件失败")
	}
	if len(data) == 0 {
		return Upload{}, apperr.BadRequest("文件为空")
	}
	contentType := http.DetectContentType(data)
	ext, ok := extByType[contentType]
	if !ok {
		return Upload{}, ErrUnsupportedType
	}
	up := Upload{
		Key:    fmt.Sprintf("%04d/%02d/%s.%s", now.Year(), int(now.Month()), randx.ID("", 32), ext),
		UserID: userID, Size: int64(len(data)), ContentType: contentType, Purpose: purpose, CreatedAt: now,
	}
	if _, err := s.store.Put(ctx, up.Key, bytes.NewReader(data)); err != nil {
		return Upload{}, fmt.Errorf("store upload: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO uploads (key, user_id, size, content_type, purpose, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, up.Key, userID, up.Size, contentType, purpose, now); err != nil {
		_ = s.store.Delete(context.WithoutCancel(ctx), up.Key)
		return Upload{}, err
	}
	return up, nil
}

// CheckOwned 校验 key 是该用户上传的、指定用途的文件。
func (s *Service) CheckOwned(ctx context.Context, key string, userID int64, purpose string) error {
	if !ValidKey(key) {
		return errInvalidUpload
	}
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM uploads WHERE key = $1 AND user_id = $2 AND purpose = $3)`,
		key, userID, purpose).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errInvalidUpload
	}
	return nil
}

// Open 返回文件登记信息与内容（后台查看营业执照用），调用方负责关闭。
func (s *Service) Open(ctx context.Context, key string) (Upload, io.ReadCloser, error) {
	if !ValidKey(key) {
		return Upload{}, nil, apperr.ErrNotFound
	}
	var up Upload
	err := s.pool.QueryRow(ctx, `SELECT key, user_id, size, content_type, purpose, created_at FROM uploads WHERE key = $1`, key).
		Scan(&up.Key, &up.UserID, &up.Size, &up.ContentType, &up.Purpose, &up.CreatedAt)
	if db.IsNoRows(err) {
		return Upload{}, nil, apperr.ErrNotFound
	}
	if err != nil {
		return Upload{}, nil, err
	}
	rc, err := s.store.Open(ctx, key)
	if err != nil {
		return Upload{}, nil, err
	}
	return up, rc, nil
}
```

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/storage/`
Expected: PASS

- [ ] **Step 6: 写上传接口的失败测试**

`internal/api/api_test.go` 的 `newTestEnv` 中，在构造 `srv` 之前加入：

```go
	disk, err := storage.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uploads := storage.NewService(pool, clk, disk)
```

并在 `&api.Server{...}` 字面量中加上 `Uploads: uploads,`；import 增加 `"spark-cloud/internal/storage"`。

`internal/api/uploads_test.go`：

```go
package api_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"regexp"
	"testing"

	"spark-cloud/internal/storage"
)

// upload 以 multipart/form-data 上传一个文件字段。
func (c *client) upload(field string, content []byte) (int, map[string]any) {
	c.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(field, "license.png")
	if err != nil {
		c.t.Fatal(err)
	}
	_, _ = fw.Write(content)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", c.base+"/uploads", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func TestUploadLicense(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	if code, _ := c.upload("file", png); code != 401 {
		t.Fatalf("anonymous upload = %d", code)
	}
	c.login("13800000001")
	code, out := c.upload("file", png)
	if code != 201 || out["content_type"] != "image/png" || !regexp.MustCompile(`^\d{4}/\d{2}/[a-z0-9]{32}\.png$`).MatchString(out["key"].(string)) {
		t.Fatalf("upload = %d %v", code, out)
	}
	if _, ok := out["user_id"]; ok {
		t.Fatal("user_id must not be exposed")
	}
}

func TestUploadRejects(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	c.login("13800000001")
	if code, out := c.upload("file", []byte("plain text pretending to be png")); code != 415 || out["code"] != "unsupported_file_type" {
		t.Fatalf("fake png = %d %v", code, out)
	}
	if code, out := c.upload("document", png); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("wrong field = %d %v", code, out)
	}
	big := append(append([]byte{}, png...), make([]byte, storage.MaxUploadSize)...)
	if code, out := c.upload("file", big); code != 413 || out["code"] != "file_too_large" {
		t.Fatalf("too large = %d %v", code, out)
	}
	// JSON 请求体不能用于上传接口
	if code, out := c.do("POST", "/uploads", map[string]string{"file": "x"}); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("json upload = %d %v", code, out)
	}
	// 其他写接口仍然只接受 JSON
	req, _ := http.NewRequest("POST", c.base+"/orders", bytes.NewReader([]byte("--x--")))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("multipart to /orders = %d", resp.StatusCode)
	}
}
```

- [ ] **Step 7: 运行确认失败**

Run: `go test ./internal/api/ -run Upload`
Expected: 编译失败（`api.Server` 没有 `Uploads` 字段）或 404/400 断言失败。

- [ ] **Step 8: 实现上传接口**

`internal/api/uploads.go`：

```go
package api

import (
	"errors"
	"io"
	"net/http"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/storage"
)

const uploadPath = "/api/v1/uploads"

// handleUpload 接收 multipart/form-data 中名为 file 的文件（营业执照）。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, storage.MaxUploadSize+1<<20) // 额外 1MB 留给 multipart 头部
	mr, err := r.MultipartReader()
	if err != nil {
		s.writeError(w, r, apperr.BadRequest("请使用 multipart/form-data 上传，文件字段名为 file"))
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			s.writeError(w, r, apperr.BadRequest("缺少文件字段 file"))
			return
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, r, storage.ErrFileTooLarge)
			return
		}
		if err != nil {
			s.writeError(w, r, apperr.BadRequest("上传内容格式不正确"))
			return
		}
		if part.FormName() != "file" {
			continue
		}
		up, err := s.Uploads.Save(r.Context(), currentUser(r).ID, storage.PurposeLicense, part)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, up)
		return
	}
}
```

`internal/api/server.go`：

1. `Server` 结构体增加字段 `Uploads *storage.Service`（import `"spark-cloud/internal/storage"`）。
2. 在 `requireUser` 分组中加入 `r.Post("/uploads", s.handleUpload)`。
3. 把 `requireJSONForWrites` 替换为：

```go
// requireJSONForWrites 要求写请求使用 application/json（上传接口要求 multipart/form-data）。
// 浏览器跨站表单无法伪造 JSON 类型，兼作 CSRF 防护；上传接口依赖 SameSite=Lax 会话 Cookie 防跨站提交。
func (s *Server) requireJSONForWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		want, msg := "application/json", "请求格式必须为 JSON"
		if r.URL.Path == uploadPath {
			want, msg = "multipart/form-data", "请使用 multipart/form-data 上传文件"
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), want) {
			s.writeError(w, r, apperr.BadRequest(msg))
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 9: 在 app 中组装**

`internal/app/app.go`：

1. import 增加 `"spark-cloud/internal/storage"`。
2. `App` 结构体增加字段 `Uploads *storage.Service`。
3. 在 `fixedCode` 计算之后、`ids := identity.NewService(...)` 之前加入：

```go
	disk, err := storage.NewLocalDisk(cfg.UploadDir)
	if err != nil {
		return nil, err
	}
	uploads := storage.NewService(pool, clk, disk)
```

4. `api.Server` 字面量加 `Uploads: uploads,`；返回的 `App` 字面量加 `Uploads: uploads,`。

`internal/app/e2e_test.go` 的 `TestFullRentalLifecycle` 中，`cfg := config.Config{...}` 加上 `UploadDir: t.TempDir(),`。

- [ ] **Step 10: 运行确认通过**

Run: `go test ./internal/api/ ./internal/app/ ./internal/storage/`
Expected: PASS

- [ ] **Step 11: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add migrations/00010_uploads.sql internal/storage internal/api internal/app
git commit -m "feat(storage): local-disk uploads with content sniffing, size/type limits and owner tracking

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: 实名并发修正、失败计次与营业执照文件校验

**Files:**
- Create: `migrations/00011_kyc_error.sql`
- Modify: `internal/identity/service.go`（`Deps.Uploads`、`UploadChecker` 接口）
- Modify: `internal/identity/kyc.go`（`SubmitPersonalKYC`、`SubmitEnterpriseKYC`、`ReviewEnterpriseKYC` 改写）
- Modify: `internal/identity/kyc_test.go`
- Modify: `internal/app/app.go`（把上传服务注入 identity）

**Interfaces:**
- Consumes: Task 5 的 `storage.PurposeLicense`、`(*storage.Service).CheckOwned`；Task 1 的 `audit.Actor`。
- Produces（Task 18 使用）：
  - `identity.UploadChecker` 接口：`CheckOwned(ctx, key string, userID int64, purpose string) error`；`identity.Deps.Uploads UploadChecker`。
  - `(*identity.Service).ReviewEnterpriseKYC(ctx, actor audit.Actor, recordID int64, approve bool, reason string) error`（签名改为接收 `audit.Actor`，审计记录带 IP）。
  - 新错误码：`kyc_in_progress`（409）、`kyc_unavailable`（503）。
  - `kyc_records.result` 新增取值 `error`（核验接口报错或超时中断），计入每日 5 次上限，不改变用户的实名状态。

- [ ] **Step 1: 写迁移**

`migrations/00011_kyc_error.sql`：

```sql
-- +goose Up
-- error：核验接口报错或核验中断；计入每日次数，不改变用户实名状态
ALTER TABLE kyc_records DROP CONSTRAINT kyc_records_result_check;
ALTER TABLE kyc_records ADD CONSTRAINT kyc_records_result_check
    CHECK (result IN ('pending', 'verified', 'rejected', 'error'));
CREATE INDEX kyc_records_user_kind_idx ON kyc_records (user_id, kind, created_at DESC);

-- +goose Down
DROP INDEX kyc_records_user_kind_idx;
UPDATE kyc_records SET result = 'rejected' WHERE result = 'error';
ALTER TABLE kyc_records DROP CONSTRAINT kyc_records_result_check;
ALTER TABLE kyc_records ADD CONSTRAINT kyc_records_result_check
    CHECK (result IN ('pending', 'verified', 'rejected'));
```

- [ ] **Step 2: 改写并补充失败测试**

`internal/identity/kyc_test.go`：

1. import 增加 `"fmt"`、`"sync/atomic"`、`"time"`、`"spark-cloud/internal/audit"`。
2. 在 `newKYCEnv` 之前加入假上传校验器，并在 `newKYCEnv` 的 `identity.Deps{...}` 中加上 `Uploads: fakeUploads{},`：

```go
// fakeUploads 只认可 licenseKeyFor(userID) 生成的 key 属于该用户。
type fakeUploads struct{}

func licenseKeyFor(userID int64) string { return fmt.Sprintf("license-of-%d", userID) }

func (fakeUploads) CheckOwned(_ context.Context, key string, userID int64, purpose string) error {
	if key == licenseKeyFor(userID) && purpose == "license" {
		return nil
	}
	return apperr.BadRequest("营业执照文件无效，请重新上传")
}
```

3. `TestEnterpriseKYCReview` 中：
   - `svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", "https://files/license.png")` 改为 `svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", licenseKeyFor(uid))`；
   - 三处 `svc.ReviewEnterpriseKYC(ctx, recordID, 99, ...)` 改为 `svc.ReviewEnterpriseKYC(ctx, audit.Actor{AdminID: 99, IP: "10.0.0.1"}, recordID, ...)`；
   - 末尾审计断言改为同时检查 IP：

```go
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'kyc.review' AND actor_id = 99 AND ip = '10.0.0.1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
```

4. `TestEnterpriseKYCValidation` 中 `"https://files/license.png"` 改为 `licenseKeyFor(uid)`。
5. 在文件末尾追加：

```go
func TestEnterpriseKYCRequiresOwnUpload(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000008", false)
	other := dbtest.CreateUser(t, pool, "13900000009", false)
	for _, key := range []string{"", "https://files/license.png", licenseKeyFor(other)} {
		err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", key)
		if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
			t.Fatalf("license %q: want bad_request, got %v", key, err)
		}
	}
	if err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", licenseKeyFor(uid)); err != nil {
		t.Fatal(err)
	}
	// 审核中不能再次提交
	if err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", licenseKeyFor(uid)); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("resubmit while pending = %v", err)
	}
	var file string
	_ = pool.QueryRow(ctx, `SELECT license_file FROM kyc_records WHERE user_id = $1`, uid).Scan(&file)
	if file != licenseKeyFor(uid) {
		t.Fatalf("license_file = %q", file)
	}
}

// blockingVerifier 在 release 关闭前阻塞，用于制造并发提交。
type blockingVerifier struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (b *blockingVerifier) VerifyPerson(context.Context, string, string, string) (kyc.Result, error) {
	b.calls.Add(1)
	b.entered <- struct{}{}
	<-b.release
	return kyc.Result{Passed: true}, nil
}

func TestPersonalKYCConcurrentSubmitCallsVerifierOnce(t *testing.T) {
	v := &blockingVerifier{entered: make(chan struct{}, 10), release: make(chan struct{})}
	svc, pool, _ := newKYCEnv(t, v)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000010", false)

	first := make(chan error, 1)
	go func() {
		_, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID)
		first <- err
	}()
	<-v.entered // 第一次提交已登记并正在调用核验接口
	for i := 0; i < 3; i++ {
		if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); apperr.As(err) == nil || apperr.As(err).Code != "kyc_in_progress" {
			t.Fatalf("concurrent submit %d = %v", i, err)
		}
	}
	close(v.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if n := v.calls.Load(); n != 1 {
		t.Fatalf("verifier calls = %d, want 1", n)
	}
	u, _ := svc.GetUser(ctx, uid)
	if u.KYCStatus != "verified" {
		t.Fatalf("status = %s", u.KYCStatus)
	}
}

type failingVerifier struct{}

func (failingVerifier) VerifyPerson(context.Context, string, string, string) (kyc.Result, error) {
	return kyc.Result{}, errors.New("aliyun: 账户余额不足")
}

func TestPersonalKYCVerifierErrorCountsTowardLimit(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, failingVerifier{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000011", false)
	for i := 0; i < 5; i++ {
		if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); apperr.As(err) == nil || apperr.As(err).Code != "kyc_unavailable" {
			t.Fatalf("attempt %d = %v", i, err)
		}
	}
	if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("6th attempt = %v", err)
	}
	var errorsN int
	var reason string
	_ = pool.QueryRow(ctx, `SELECT count(*), max(reason) FROM kyc_records WHERE user_id = $1 AND result = 'error'`, uid).Scan(&errorsN, &reason)
	if errorsN != 5 || reason != "aliyun: 账户余额不足" {
		t.Fatalf("error records = %d %q", errorsN, reason)
	}
	if u, _ := svc.GetUser(ctx, uid); u.KYCStatus != "none" {
		t.Fatalf("status = %s, want none", u.KYCStatus)
	}
}

func TestPersonalKYCStalePendingIsReleased(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000012", false)
	var recordID int64
	if err := pool.QueryRow(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, result, created_at, updated_at)
		VALUES ($1, 'personal', '张三', 'pending', $2, $2) RETURNING id`, uid, dbtest.T0.Add(-time.Minute)).Scan(&recordID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); apperr.As(err) == nil || apperr.As(err).Code != "kyc_in_progress" {
		t.Fatalf("with fresh pending = %v", err)
	}
	// 模拟进程在核验途中重启：记录停在 pending 超过 2 分钟
	if _, err := pool.Exec(ctx, `UPDATE kyc_records SET created_at = $2 WHERE id = $1`, recordID, dbtest.T0.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); err != nil {
		t.Fatalf("after stale pending = %v", err)
	}
	var result string
	_ = pool.QueryRow(ctx, `SELECT result FROM kyc_records WHERE id = $1`, recordID).Scan(&result)
	if result != "error" {
		t.Fatalf("stale record result = %s, want error", result)
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `go test ./internal/identity/`
Expected: 编译失败（`Deps` 没有 `Uploads` 字段、`ReviewEnterpriseKYC` 签名不符）。

- [ ] **Step 4: 实现**

`internal/identity/service.go`：在 `Deps` 中 `Box` 字段之后加入 `Uploads UploadChecker // 校验营业执照文件归属（storage 模块实现）`，并在 `Deps` 定义之前加入：

```go
// UploadChecker 校验上传文件确实属于该用户且用途正确。
type UploadChecker interface {
	CheckOwned(ctx context.Context, key string, userID int64, purpose string) error
}
```

`internal/identity/kyc.go`：

1. import 改为：

```go
import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/storage"
)
```

2. 常量与错误改为：

```go
const (
	kycDailyLimit     = 5
	kycPendingTimeout = 2 * time.Minute // 个人核验「进行中」的最长时间，超过视为中断
)

var (
	idNumberRe  = regexp.MustCompile(`^\d{17}[\dX]$`)
	licenseNoRe = regexp.MustCompile(`^[0-9A-HJ-NPQRTUWXY]{2}\d{6}[0-9A-HJ-NPQRTUWXY]{10}$`)

	errKYCInProgress  = apperr.New("kyc_in_progress", "实名核验进行中，请稍候", 409)
	errKYCUnavailable = apperr.New("kyc_unavailable", "实名核验服务暂时不可用，请稍后再试", 503)
)
```

3. 用以下内容替换整个 `SubmitPersonalKYC`：

```go
// SubmitPersonalKYC 调用三要素核验，结果立即生效。
// 先在事务内锁用户行、检查次数并登记一条 pending 记录，提交后再调用付费核验接口，
// 因此同一用户的并发提交只会调用一次接口；接口报错记为 error 并计入当日次数。
func (s *Service) SubmitPersonalKYC(ctx context.Context, userID int64, name, idNumber string) (User, error) {
	name = strings.TrimSpace(name)
	idNumber = strings.ToUpper(strings.TrimSpace(idNumber))
	if name == "" || utf8.RuneCountInString(name) > 50 {
		return User{}, apperr.BadRequest("请填写真实姓名")
	}
	if !idNumberRe.MatchString(idNumber) {
		return User{}, apperr.BadRequest("身份证号格式不正确")
	}
	now := s.d.Clock.Now()
	var phone string
	var recordID int64
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1 FOR UPDATE`, userID))
		if err != nil {
			return err
		}
		if u.KYCStatus == "verified" || u.KYCStatus == "pending" {
			return apperr.ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `UPDATE kyc_records SET result = 'error', reason = '核验超时未完成', updated_at = $3
			WHERE user_id = $1 AND kind = 'personal' AND result = 'pending' AND created_at <= $2`,
			userID, now.Add(-kycPendingTimeout), now); err != nil {
			return err
		}
		var inProgress bool
		var attempts int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE result = 'pending') > 0, count(*)
			FROM kyc_records WHERE user_id = $1 AND kind = 'personal' AND created_at > $2`,
			userID, now.Add(-24*time.Hour)).Scan(&inProgress, &attempts); err != nil {
			return err
		}
		if inProgress {
			return errKYCInProgress
		}
		if attempts >= kycDailyLimit {
			return apperr.ErrRateLimited
		}
		phone = u.Phone
		return tx.QueryRow(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, id_number_enc, result, created_at, updated_at)
			VALUES ($1, 'personal', $2, $3, 'pending', $4, $4) RETURNING id`,
			userID, name, s.d.Box.Seal([]byte(idNumber)), now).Scan(&recordID)
	})
	if err != nil {
		return User{}, err
	}

	res, verr := s.d.KYC.VerifyPerson(ctx, name, idNumber, phone)
	result, reason := "rejected", res.Reason
	switch {
	case verr != nil:
		result, reason = "error", truncateRunes(verr.Error(), 200)
	case res.Passed:
		result, reason = "verified", ""
	}
	done := s.d.Clock.Now()
	wctx := context.WithoutCancel(ctx) // 已调用付费接口，结果必须落库，即使客户端已断开
	err = db.WithTx(wctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(wctx, `UPDATE kyc_records SET result = $2, reason = $3, updated_at = $4 WHERE id = $1`,
			recordID, result, reason, done); err != nil {
			return err
		}
		if result == "error" {
			return nil
		}
		_, err := tx.Exec(wctx, `UPDATE users SET kyc_status = $2, kind = 'personal', updated_at = $3
			WHERE id = $1 AND kyc_status <> 'verified'`, userID, result, done)
		return err
	})
	if err != nil {
		return User{}, err
	}
	switch result {
	case "error":
		return User{}, errKYCUnavailable
	case "rejected":
		return User{}, apperr.New("kyc_rejected", "实名核验未通过："+reason, 400)
	}
	return s.GetUser(ctx, userID)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
```

4. 用以下内容替换整个 `SubmitEnterpriseKYC`：

```go
// SubmitEnterpriseKYC 提交企业认证，等待管理员审核。licenseFile 必须是本人通过上传接口得到的营业执照文件 key。
func (s *Service) SubmitEnterpriseKYC(ctx context.Context, userID int64, companyName, licenseNo, licenseFile string) error {
	companyName = strings.TrimSpace(companyName)
	licenseNo = strings.ToUpper(strings.TrimSpace(licenseNo))
	licenseFile = strings.TrimSpace(licenseFile)
	if companyName == "" || utf8.RuneCountInString(companyName) > 100 {
		return apperr.BadRequest("请填写企业名称")
	}
	if !licenseNoRe.MatchString(licenseNo) {
		return apperr.BadRequest("统一社会信用代码格式不正确")
	}
	if licenseFile == "" {
		return apperr.BadRequest("请上传营业执照")
	}
	if s.d.Uploads == nil {
		return errors.New("identity: 未配置上传文件校验")
	}
	if err := s.d.Uploads.CheckOwned(ctx, licenseFile, userID, storage.PurposeLicense); err != nil {
		return err
	}
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1 FOR UPDATE`, userID))
		if err != nil {
			return err
		}
		if u.KYCStatus == "verified" || u.KYCStatus == "pending" {
			return apperr.ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, license_no, license_file, result, created_at, updated_at)
			VALUES ($1, 'enterprise', $2, $3, $4, 'pending', $5, $5)`,
			userID, companyName, licenseNo, licenseFile, now); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE users SET kyc_status = 'pending', updated_at = $2 WHERE id = $1`, userID, now)
		return err
	})
}
```

5. `ReviewEnterpriseKYC` 的签名与审计改为：

```go
// ReviewEnterpriseKYC 管理员审核企业认证；驳回必须填写原因。
func (s *Service) ReviewEnterpriseKYC(ctx context.Context, actor audit.Actor, recordID int64, approve bool, reason string) error {
```

函数体中把 `reviewerID` 全部替换为 `actor.AdminID`，并把最后的 `audit.Record(...)` 改为：

```go
		return audit.Record(ctx, tx, now, actor.Entry("kyc.review", "kyc_record", strconv.FormatInt(recordID, 10),
			map[string]any{"approve": approve, "reason": reason, "user_id": userID}))
```

（原文件中对 `"fmt"` 的引用随旧版 `SubmitPersonalKYC` 一起删除；若 `go vet` 报未使用的 import，删掉即可。）

`internal/app/app.go`：`identity.NewService(identity.Deps{...})` 中加上 `Uploads: uploads,`（Task 5 已在其之前创建 `uploads`）。

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/identity/ ./internal/app/`
Expected: PASS

- [ ] **Step 6: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add migrations/00011_kyc_error.sql internal/identity internal/app
git commit -m "fix(identity): serialize personal KYC per user, count verifier errors, require owned license upload

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: 阿里云短信与三要素实名插件

**Files:**
- Create: `internal/plugins/aliyun/client.go`、`internal/plugins/aliyun/client_test.go`
- Create: `internal/plugins/sms/aliyun.go`、`internal/plugins/sms/aliyun_test.go`
- Create: `internal/plugins/kyc/aliyun.go`、`internal/plugins/kyc/aliyun_test.go`
- Modify: `internal/app/app.go`（`aliyun` 驱动接入）
- Modify: `internal/app/e2e_test.go`（追加组装测试）

**Interfaces:**
- Consumes: Task 1 的 `config.AliyunConfig`；计划 1 的 `sms.Sender`、`sms.TemplateLoginCode`、`sms.TemplateExpiryReminder`、`kyc.Verifier`、`kyc.Result`。
- Produces：
  - `aliyun.Client{Endpoint, AccessKeyID, AccessKeySecret string; HTTP *http.Client; Now func() time.Time; Nonce func() string}`、`(*aliyun.Client).Call(ctx, action, version string, params map[string]string, out any) error`、`aliyun.Sign(secret, method string, params map[string]string) string`、`*aliyun.Error{Status int; Code, Message, RequestID string}`。
  - `sms.Aliyun{Client *aliyun.Client; SignName string; Templates map[string]string}` 实现 `sms.Sender`。
  - `kyc.Aliyun{Client *aliyun.Client; AuthCode string}` 实现 `kyc.Verifier`。
  - `app.Build` 支持 `SMSDriver = "aliyun"`、`KYCDriver = "aliyun"`。

阿里云产品与版本（拿到账号后如有出入，只改本任务的两个插件文件）：
- 短信：`SendSms`，版本 `2017-05-25`，默认 endpoint `https://dysmsapi.aliyuncs.com`；返回 `Code == "OK"` 为成功。
- 号码百科三要素核验：`ThreeElementsVerification`，版本 `2020-02-17`，默认 endpoint `https://dytnsapi.aliyuncs.com`；返回 `Code == "OK"` 时 `Data.IsConsistent`：1 一致、0 不一致、2 查无此号或非实名；其余视为调用失败。

- [ ] **Step 1: 写签名客户端的失败测试**

`internal/plugins/aliyun/client_test.go`：

```go
package aliyun_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"spark-cloud/internal/plugins/aliyun"
)

// 阿里云文档中 RPC 签名的示例（ECS DescribeRegions），用于校验签名算法。
func TestSignMatchesDocumentedExample(t *testing.T) {
	params := map[string]string{
		"AccessKeyId": "testid", "Action": "DescribeRegions", "Format": "XML", "SignatureMethod": "HMAC-SHA1",
		"SignatureNonce": "3ee8c1b8-83d3-44af-a94f-4e0ad82fd6cf", "SignatureVersion": "1.0",
		"Timestamp": "2016-02-23T12:46:24Z", "Version": "2014-05-26",
	}
	if got := aliyun.Sign("testsecret", "GET", params); got != "OLeaidS1JvxuMvnyHOwuJ+uX5qY=" {
		t.Fatalf("signature = %s", got)
	}
}

func TestCallSendsSignedRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		params := map[string]string{}
		for k := range q {
			if k != "Signature" {
				params[k] = q.Get(k)
			}
		}
		if q.Get("Signature") != aliyun.Sign("sk", "GET", params) {
			http.Error(w, `{"Code":"SignatureDoesNotMatch","Message":"bad sig","RequestId":"r0"}`, 400)
			return
		}
		if q.Get("Action") != "Ping" || q.Get("Version") != "2020-01-01" || q.Get("AccessKeyId") != "ak" ||
			q.Get("Format") != "JSON" || q.Get("Timestamp") != "2026-01-01T08:00:00Z" || q.Get("SignatureNonce") != "n-1" ||
			q.Get("Name") != "张 三+1" {
			http.Error(w, `{"Code":"BadParams","Message":"`+r.URL.RawQuery+`"}`, 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"Code": "OK", "Echo": q.Get("Name")})
	}))
	defer srv.Close()
	c := &aliyun.Client{Endpoint: srv.URL, AccessKeyID: "ak", AccessKeySecret: "sk",
		Now:   func() time.Time { return time.Date(2026, 1, 1, 16, 0, 0, 0, time.FixedZone("CST", 8*3600)) },
		Nonce: func() string { return "n-1" }}
	var out struct{ Code, Echo string }
	if err := c.Call(context.Background(), "Ping", "2020-01-01", map[string]string{"Name": "张 三+1"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Code != "OK" || out.Echo != "张 三+1" {
		t.Fatalf("out = %+v", out)
	}
}

func TestCallMapsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"Code":"InvalidAccessKeyId.NotFound","Message":"Specified access key is not found.","RequestId":"r1"}`))
	}))
	defer srv.Close()
	c := &aliyun.Client{Endpoint: srv.URL, AccessKeyID: "ak", AccessKeySecret: "sk"}
	err := c.Call(context.Background(), "Ping", "2020-01-01", nil, &struct{}{})
	var ae *aliyun.Error
	if !errors.As(err, &ae) || ae.Status != 404 || ae.Code != "InvalidAccessKeyId.NotFound" || ae.RequestID != "r1" {
		t.Fatalf("err = %v", err)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/plugins/aliyun/`
Expected: 编译失败（包不存在）。

- [ ] **Step 3: 实现签名客户端**

`internal/plugins/aliyun/client.go`：

```go
// Package aliyun 实现阿里云 RPC 风格 API（签名版本 1.0，HMAC-SHA1）的最小客户端，供短信与号码百科插件使用。
package aliyun

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Client struct {
	Endpoint        string // 例如 https://dysmsapi.aliyuncs.com
	AccessKeyID     string
	AccessKeySecret string
	HTTP            *http.Client     // 为空时使用 10 秒超时的默认客户端
	Now             func() time.Time // 测试可替换
	Nonce           func() string    // 测试可替换
}

// Error 是阿里云以非 200 状态返回的错误。
type Error struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *Error) Error() string {
	return fmt.Sprintf("aliyun %d %s: %s (request %s)", e.Status, e.Code, e.Message, e.RequestID)
}

var defaultHTTP = &http.Client{Timeout: 10 * time.Second}

// Call 以 GET 调用 action，并把 JSON 响应解码到 out。
func (c *Client) Call(ctx context.Context, action, version string, params map[string]string, out any) error {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	nonce := uuid.NewString
	if c.Nonce != nil {
		nonce = c.Nonce
	}
	q := map[string]string{
		"AccessKeyId":      c.AccessKeyID,
		"Action":           action,
		"Format":           "JSON",
		"RegionId":         "cn-hangzhou",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   nonce(),
		"SignatureVersion": "1.0",
		"Timestamp":        now().UTC().Format("2006-01-02T15:04:05Z"),
		"Version":          version,
	}
	for k, v := range params {
		q[k] = v
	}
	query := canonicalQuery(q) + "&Signature=" + percentEncode(Sign(c.AccessKeySecret, http.MethodGet, q))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.Endpoint, "/")+"/?"+query, nil)
	if err != nil {
		return err
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("aliyun %s: %w", action, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("aliyun %s: read response: %w", action, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Code, Message, RequestId string }
		_ = json.Unmarshal(body, &e)
		return &Error{Status: resp.StatusCode, Code: e.Code, Message: e.Message, RequestID: e.RequestId}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("aliyun %s: decode response: %w", action, err)
	}
	return nil
}

// percentEncode 按阿里云要求编码：空格为 %20，* 为 %2A，~ 不编码。
func percentEncode(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "*", "%2A")
	return strings.ReplaceAll(e, "%7E", "~")
}

func canonicalQuery(q map[string]string) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = percentEncode(k) + "=" + percentEncode(q[k])
	}
	return strings.Join(parts, "&")
}

// Sign 计算 RPC 签名：Base64(HMAC-SHA1(secret + "&", METHOD + "&%2F&" + percentEncode(规范化参数)))。
func Sign(secret, method string, params map[string]string) string {
	stringToSign := method + "&" + percentEncode("/") + "&" + percentEncode(canonicalQuery(params))
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/plugins/aliyun/`
Expected: PASS

- [ ] **Step 5: 写短信与实名插件的失败测试**

`internal/plugins/sms/aliyun_test.go`：

```go
package sms_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spark-cloud/internal/plugins/aliyun"
	"spark-cloud/internal/plugins/sms"
)

func newAliyunSMS(t *testing.T, handler http.HandlerFunc) *sms.Aliyun {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &sms.Aliyun{
		Client:    &aliyun.Client{Endpoint: srv.URL, AccessKeyID: "ak", AccessKeySecret: "sk"},
		SignName:  "Spark算力",
		Templates: map[string]string{sms.TemplateLoginCode: "SMS_100", sms.TemplateExpiryReminder: "SMS_200"},
	}
}

func TestAliyunSendSms(t *testing.T) {
	var got map[string]string
	s := newAliyunSMS(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		got = map[string]string{"Action": q.Get("Action"), "Version": q.Get("Version"), "PhoneNumbers": q.Get("PhoneNumbers"),
			"SignName": q.Get("SignName"), "TemplateCode": q.Get("TemplateCode"), "TemplateParam": q.Get("TemplateParam")}
		_, _ = w.Write([]byte(`{"Code":"OK","Message":"OK","BizId":"9","RequestId":"r"}`))
	})
	if err := s.Send(context.Background(), "13800001234", sms.TemplateExpiryReminder, map[string]string{"instance": "ins-1", "hours": "24"}); err != nil {
		t.Fatal(err)
	}
	var params map[string]string
	if err := json.Unmarshal([]byte(got["TemplateParam"]), &params); err != nil {
		t.Fatal(err)
	}
	if got["Action"] != "SendSms" || got["Version"] != "2017-05-25" || got["PhoneNumbers"] != "13800001234" ||
		got["SignName"] != "Spark算力" || got["TemplateCode"] != "SMS_200" || params["instance"] != "ins-1" || params["hours"] != "24" {
		t.Fatalf("request = %v", got)
	}
}

func TestAliyunSendSmsFailures(t *testing.T) {
	s := newAliyunSMS(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Code":"isv.BUSINESS_LIMIT_CONTROL","Message":"触发分钟级流控","RequestId":"r"}`))
	})
	err := s.Send(context.Background(), "13800001234", sms.TemplateLoginCode, map[string]string{"code": "123456"})
	if err == nil || !strings.Contains(err.Error(), "isv.BUSINESS_LIMIT_CONTROL") {
		t.Fatalf("business error = %v", err)
	}
	if err := s.Send(context.Background(), "13800001234", "unknown_template", nil); err == nil {
		t.Fatal("unknown template must fail")
	}
	down := newAliyunSMS(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "oops", 500) })
	if err := down.Send(context.Background(), "13800001234", sms.TemplateLoginCode, map[string]string{"code": "1"}); err == nil {
		t.Fatal("HTTP 500 must fail")
	}
}
```

`internal/plugins/kyc/aliyun_test.go`：

```go
package kyc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"spark-cloud/internal/plugins/aliyun"
	"spark-cloud/internal/plugins/kyc"
)

func newAliyunKYC(t *testing.T, body string, seen map[string]string) *kyc.Aliyun {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for _, k := range []string{"Action", "Version", "AuthCode", "InputNumber", "IdentifyNum", "UserName", "Mask"} {
			seen[k] = q.Get(k)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &kyc.Aliyun{Client: &aliyun.Client{Endpoint: srv.URL, AccessKeyID: "ak", AccessKeySecret: "sk"}, AuthCode: "auth-1"}
}

func TestAliyunThreeElements(t *testing.T) {
	cases := []struct {
		body    string
		passed  bool
		reason  string
		wantErr bool
	}{
		{`{"Code":"OK","Data":{"IsConsistent":1}}`, true, "", false},
		{`{"Code":"OK","Data":{"IsConsistent":0}}`, false, "姓名、身份证号与手机号不一致", false},
		{`{"Code":"OK","Data":{"IsConsistent":2}}`, false, "该手机号未查到实名信息，请使用本人实名登记的手机号", false},
		{`{"Code":"OK","Data":{"IsConsistent":9}}`, false, "", true},
		{`{"Code":"OK","Data":{}}`, false, "", true},
		{`{"Code":"Account.Arrears","Message":"欠费"}`, false, "", true},
	}
	for _, c := range cases {
		seen := map[string]string{}
		v := newAliyunKYC(t, c.body, seen)
		res, err := v.VerifyPerson(context.Background(), "张三", "11010119900101123X", "13800001234")
		if (err != nil) != c.wantErr || res.Passed != c.passed || res.Reason != c.reason {
			t.Fatalf("%s: res=%+v err=%v", c.body, res, err)
		}
		if seen["Action"] != "ThreeElementsVerification" || seen["Version"] != "2020-02-17" || seen["AuthCode"] != "auth-1" ||
			seen["InputNumber"] != "13800001234" || seen["IdentifyNum"] != "11010119900101123X" || seen["UserName"] != "张三" || seen["Mask"] != "NORMAL" {
			t.Fatalf("request = %v", seen)
		}
	}
}
```

- [ ] **Step 6: 运行确认失败**

Run: `go test ./internal/plugins/...`
Expected: 编译失败（`sms.Aliyun`、`kyc.Aliyun` 未定义）。

- [ ] **Step 7: 实现插件**

`internal/plugins/sms/aliyun.go`：

```go
package sms

import (
	"context"
	"encoding/json"
	"fmt"

	"spark-cloud/internal/plugins/aliyun"
)

// Aliyun 通过阿里云短信服务 SendSms 发送短信。
type Aliyun struct {
	Client    *aliyun.Client
	SignName  string
	Templates map[string]string // 业务模板名（TemplateLoginCode 等）→ 阿里云模板编号
}

func (a *Aliyun) Send(ctx context.Context, phone, template string, params map[string]string) error {
	code, ok := a.Templates[template]
	if !ok || code == "" {
		return fmt.Errorf("aliyun sms: 未配置模板 %s", template)
	}
	if params == nil {
		params = map[string]string{}
	}
	tp, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var resp struct {
		Code, Message, BizId, RequestId string
	}
	if err := a.Client.Call(ctx, "SendSms", "2017-05-25", map[string]string{
		"PhoneNumbers": phone, "SignName": a.SignName, "TemplateCode": code, "TemplateParam": string(tp),
	}, &resp); err != nil {
		return err
	}
	if resp.Code != "OK" {
		return fmt.Errorf("aliyun sms: %s %s (request %s)", resp.Code, resp.Message, resp.RequestId)
	}
	return nil
}
```

`internal/plugins/kyc/aliyun.go`：

```go
package kyc

import (
	"context"
	"fmt"

	"spark-cloud/internal/plugins/aliyun"
)

// Aliyun 调用阿里云号码百科「运营商三要素核验」（姓名 + 身份证号 + 手机号）。
type Aliyun struct {
	Client   *aliyun.Client
	AuthCode string // 号码百科控制台申请的授权码
}

func (a *Aliyun) VerifyPerson(ctx context.Context, name, idNumber, phone string) (Result, error) {
	var resp struct {
		Code, Message, RequestId string
		Data                     struct {
			IsConsistent *int
		}
	}
	if err := a.Client.Call(ctx, "ThreeElementsVerification", "2020-02-17", map[string]string{
		"AuthCode": a.AuthCode, "InputNumber": phone, "IdentifyNum": idNumber, "UserName": name, "Mask": "NORMAL",
	}, &resp); err != nil {
		return Result{}, err
	}
	if resp.Code != "OK" {
		return Result{}, fmt.Errorf("aliyun kyc: %s %s (request %s)", resp.Code, resp.Message, resp.RequestId)
	}
	if resp.Data.IsConsistent == nil {
		return Result{}, fmt.Errorf("aliyun kyc: 响应缺少 IsConsistent (request %s)", resp.RequestId)
	}
	switch *resp.Data.IsConsistent {
	case 1:
		return Result{Passed: true}, nil
	case 0:
		return Result{Passed: false, Reason: "姓名、身份证号与手机号不一致"}, nil
	case 2:
		return Result{Passed: false, Reason: "该手机号未查到实名信息，请使用本人实名登记的手机号"}, nil
	}
	return Result{}, fmt.Errorf("aliyun kyc: 未知的 IsConsistent=%d (request %s)", *resp.Data.IsConsistent, resp.RequestId)
}
```

- [ ] **Step 8: 运行确认通过**

Run: `go test ./internal/plugins/...`
Expected: PASS

- [ ] **Step 9: 写组装测试并接入 app**

在 `internal/app/e2e_test.go` 末尾追加（import 增加 `"sync"`、`"spark-cloud/internal/platform/config"` 已有）：

```go
// TestBuildWiresAliyunPlugins 验证 aliyun 驱动按配置的 endpoint 与凭证发出请求。
func TestBuildWiresAliyunPlugins(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	var mu sync.Mutex
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		actions = append(actions, q.Get("Action")+":"+q.Get("AccessKeyId"))
		mu.Unlock()
		switch q.Get("Action") {
		case "SendSms":
			_, _ = w.Write([]byte(`{"Code":"OK"}`))
		case "ThreeElementsVerification":
			_, _ = w.Write([]byte(`{"Code":"OK","Data":{"IsConsistent":1}}`))
		}
	}))
	defer srv.Close()
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "aliyun", KYCDriver: "aliyun",
		UploadDir: t.TempDir(), Aliyun: config.AliyunConfig{AccessKeyID: "ak", AccessKeySecret: "sk", SMSSign: "Spark",
			SMSTplLogin: "SMS_1", SMSTplExpiry: "SMS_2", KYCAuthCode: "auth", SMSEndpoint: srv.URL, KYCEndpoint: srv.URL}}
	a, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Clock: clock.NewFake(dbtest.T0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Identity.SendLoginCode(ctx, "13800001234", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	uid := dbtest.CreateUser(t, pool, "13800009999", false)
	if u, err := a.Identity.SubmitPersonalKYC(ctx, uid, "张三", "11010119900101123X"); err != nil || u.KYCStatus != "verified" {
		t.Fatalf("kyc = %+v, %v", u, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(actions) != 2 || actions[0] != "SendSms:ak" || actions[1] != "ThreeElementsVerification:ak" {
		t.Fatalf("actions = %v", actions)
	}
}
```

`internal/app/app.go`：

1. import 增加 `"spark-cloud/internal/plugins/aliyun"`。
2. 短信驱动 switch 增加：

```go
		case "aliyun":
			smsSender = &sms.Aliyun{
				Client:   aliyunClient(cfg, cfg.Aliyun.SMSEndpoint),
				SignName: cfg.Aliyun.SMSSign,
				Templates: map[string]string{
					sms.TemplateLoginCode:      cfg.Aliyun.SMSTplLogin,
					sms.TemplateExpiryReminder: cfg.Aliyun.SMSTplExpiry,
				},
			}
```

3. 实名驱动 switch 增加：

```go
		case "aliyun":
			verifier = &kyc.Aliyun{Client: aliyunClient(cfg, cfg.Aliyun.KYCEndpoint), AuthCode: cfg.Aliyun.KYCAuthCode}
```

4. 两个 switch 的 `default` 分支消息改为 `fmt.Errorf("未知的短信驱动 %q", cfg.SMSDriver)`、`fmt.Errorf("未知的实名驱动 %q", cfg.KYCDriver)`。
5. 文件末尾加入：

```go
func aliyunClient(cfg config.Config, endpoint string) *aliyun.Client {
	return &aliyun.Client{
		Endpoint: endpoint, AccessKeyID: cfg.Aliyun.AccessKeyID, AccessKeySecret: cfg.Aliyun.AccessKeySecret,
		HTTP: &http.Client{Timeout: 10 * time.Second},
	}
}
```

- [ ] **Step 10: 运行确认通过**

Run: `go test ./internal/app/ ./internal/plugins/...`
Expected: PASS

- [ ] **Step 11: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/plugins internal/app
git commit -m "feat(plugins): Aliyun RPC signer, SMS SendSms and three-element KYC drivers

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: 支付插件接口、mock 渠道与充值服务（billing）

**Files:**
- Create: `internal/plugins/pay/pay.go`、`internal/plugins/pay/mock.go`、`internal/plugins/pay/mock_test.go`
- Create: `migrations/00012_recharges.sql`
- Create: `internal/billing/billing.go`、`internal/billing/settle.go`
- Test: `internal/billing/helpers_test.go`、`internal/billing/billing_test.go`
- Modify: `internal/fleet/alerts.go`（新增 `RaiseAlertOnce`）、`internal/fleet/alerts_test.go`（新建）

**Interfaces:**
- Consumes: 计划 1 的 `wallet.Apply`（`KindRecharge`）、`identity.CanRent`、`fleet.Alert`；Task 1 的 `audit.Actor`、`paging.Page`。
- Produces（Task 9、10、18、19 使用）：
  - `pay.ChannelWechat = "wechat"`、`pay.ChannelAlipay = "alipay"`；`pay.Recharge{ID string; Amount int64; Subject string; ExpiresAt time.Time; NotifyURL string}`；`pay.Notification{OutTradeNo, ChannelTradeNo string; Amount int64; Paid bool}`；`pay.Provider` 接口（`Create`、`ParseNotify`、`Query`、`Close`、`NotifyAck`）。
  - `pay.NewMock(secret string) *pay.Mock`；`pay.MockSignatureHeader = "X-Mock-Signature"`；`(*Mock).FailCreate bool` 字段；`(*Mock).MarkPaid(id string, amount int64)`；`(*Mock).Closed(id string) bool`。
  - `(*fleet.Service).RaiseAlertOnce(ctx, q db.Querier, a fleet.Alert) error`：同 kind + message 已有未处理告警时不再新增。
  - `billing.Deps{Pool; Clock; Log *slog.Logger; Wallet *wallet.Service; Users billing.UserChecker; Alerts billing.AlertRaiser; Providers map[string]pay.Provider; NotifyBaseURL string}`、`billing.NewService(d Deps) *billing.Service`。
  - 常量 `billing.ChannelManual = "manual"`、`StatusPending/StatusPaid/StatusClosed`、`MinAmount = 100`、`MaxAmount = 100_000_000`、`MaxPending = 5`、`QRTTL = 15 * time.Minute`、`QueryAfter = time.Minute`、`CloseAfter = 2 * time.Hour`。
  - `billing.Recharge{ID, UserID, Channel, Amount, Status, QRCode, ChannelTradeNo, OperatorID *int64, Remark, ExpiresAt, PaidAt, ClosedAt, CreatedAt}`（带 json 标签）。
  - `(*Service).Channels() []string`、`Create(ctx, userID int64, channel string, amount int64) (Recharge, error)`、`Get(ctx, userID int64, id string) (Recharge, error)`、`ListForUser(ctx, userID int64, p paging.Page) ([]Recharge, int, error)`、`Settle(ctx, channel string, n pay.Notification) error`、`HandleNotify(ctx, channel string, w http.ResponseWriter, r *http.Request)`、`Sweep(ctx) error`、`ManualRecharge(ctx, actor audit.Actor, userID, amount int64, remark string) (Recharge, error)`。
  - 错误码：`too_many_pending`（409）、`pay_unavailable`（502）。

- [ ] **Step 1: 写支付接口与 mock 的失败测试**

`internal/plugins/pay/mock_test.go`：

```go
package pay_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/plugins/pay"
)

func TestMockLifecycle(t *testing.T) {
	m := pay.NewMock("s3cret")
	ctx := context.Background()
	qr, err := m.Create(ctx, pay.Recharge{ID: "rch-1", Amount: 100, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil || qr != "mock://pay/rch-1" {
		t.Fatalf("create = %q, %v", qr, err)
	}
	if n, err := m.Query(ctx, "rch-1"); err != nil || n.Paid || n.OutTradeNo != "rch-1" {
		t.Fatalf("query unpaid = %+v, %v", n, err)
	}
	m.MarkPaid("rch-1", 100)
	if n, _ := m.Query(ctx, "rch-1"); !n.Paid || n.Amount != 100 || n.ChannelTradeNo != "mock-rch-1" {
		t.Fatalf("query paid = %+v", n)
	}
	if err := m.Close(ctx, "rch-1"); err != nil || !m.Closed("rch-1") {
		t.Fatal("close not recorded")
	}
	m.FailCreate = true
	if _, err := m.Create(ctx, pay.Recharge{ID: "rch-2"}); err == nil {
		t.Fatal("FailCreate ignored")
	}
}

func TestMockParseNotifyChecksSignature(t *testing.T) {
	m := pay.NewMock("s3cret")
	body := `{"out_trade_no":"rch-1","channel_trade_no":"T1","amount":100,"paid":true}`
	req := httptest.NewRequest("POST", "/notify", strings.NewReader(body))
	req.Header.Set(pay.MockSignatureHeader, "s3cret")
	n, err := m.ParseNotify(req)
	if err != nil || n != (pay.Notification{OutTradeNo: "rch-1", ChannelTradeNo: "T1", Amount: 100, Paid: true}) {
		t.Fatalf("notify = %+v, %v", n, err)
	}
	bad := httptest.NewRequest("POST", "/notify", strings.NewReader(body))
	bad.Header.Set(pay.MockSignatureHeader, "guess")
	if _, err := m.ParseNotify(bad); err == nil {
		t.Fatal("bad signature accepted")
	}
	w := httptest.NewRecorder()
	m.NotifyAck(w, true)
	if w.Code != 200 || w.Body.String() != "success" {
		t.Fatalf("ack ok = %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	m.NotifyAck(w, false)
	if w.Code != 400 || w.Body.String() != "fail" {
		t.Fatalf("ack fail = %d %q", w.Code, w.Body.String())
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/plugins/pay/`
Expected: 编译失败（包不存在）。

- [ ] **Step 3: 实现支付接口与 mock**

`internal/plugins/pay/pay.go`：

```go
// Package pay 定义支付渠道插件：微信支付 Native、支付宝当面付，以及开发与测试用的 mock。
package pay

import (
	"context"
	"net/http"
	"time"
)

const (
	ChannelWechat = "wechat"
	ChannelAlipay = "alipay"
)

// Recharge 是发起扫码支付所需的充值单信息。
type Recharge struct {
	ID        string // 商户订单号（out_trade_no）
	Amount    int64  // 分
	Subject   string
	ExpiresAt time.Time // 二维码失效时间
	NotifyURL string
}

// Notification 是支付回调或主动查询得到的支付结果。
type Notification struct {
	OutTradeNo     string
	ChannelTradeNo string
	Amount         int64 // 分
	Paid           bool
}

type Provider interface {
	// Create 在渠道下单，返回二维码内容（前端据此生成二维码）。
	Create(ctx context.Context, r Recharge) (qrCode string, err error)
	// ParseNotify 验签并解析回调；验签失败返回错误。
	ParseNotify(req *http.Request) (Notification, error)
	// Query 主动查询订单；渠道上不存在的订单返回 Paid=false 而不是错误。
	Query(ctx context.Context, outTradeNo string) (Notification, error)
	// Close 关闭未支付的订单；订单不存在视为成功。
	Close(ctx context.Context, outTradeNo string) error
	// NotifyAck 按渠道要求的格式回复回调。
	NotifyAck(w http.ResponseWriter, ok bool)
}
```

`internal/plugins/pay/mock.go`：

```go
package pay

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
)

// MockSignatureHeader 是 mock 回调的「签名」请求头，值必须等于 Mock 的密钥。
const MockSignatureHeader = "X-Mock-Signature"

// Mock 是开发与测试用的支付渠道：二维码为 mock://pay/<单号>，回调为带共享密钥请求头的 JSON。
type Mock struct {
	FailCreate bool // 为 true 时 Create 返回错误，用于测试渠道故障

	secret string
	mu     sync.Mutex
	paid   map[string]Notification
	closed map[string]bool
}

func NewMock(secret string) *Mock {
	return &Mock{secret: secret, paid: map[string]Notification{}, closed: map[string]bool{}}
}

func (m *Mock) Create(_ context.Context, r Recharge) (string, error) {
	if m.FailCreate {
		return "", errors.New("mock pay: create failed")
	}
	return "mock://pay/" + r.ID, nil
}

func (m *Mock) ParseNotify(req *http.Request) (Notification, error) {
	sig := req.Header.Get(MockSignatureHeader)
	if m.secret == "" || subtle.ConstantTimeCompare([]byte(sig), []byte(m.secret)) != 1 {
		return Notification{}, errors.New("mock pay: bad signature")
	}
	var body struct {
		OutTradeNo     string `json:"out_trade_no"`
		ChannelTradeNo string `json:"channel_trade_no"`
		Amount         int64  `json:"amount"`
		Paid           bool   `json:"paid"`
	}
	if err := json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&body); err != nil {
		return Notification{}, err
	}
	return Notification{OutTradeNo: body.OutTradeNo, ChannelTradeNo: body.ChannelTradeNo, Amount: body.Amount, Paid: body.Paid}, nil
}

func (m *Mock) Query(_ context.Context, outTradeNo string) (Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.paid[outTradeNo]; ok {
		return n, nil
	}
	return Notification{OutTradeNo: outTradeNo}, nil
}

func (m *Mock) Close(_ context.Context, outTradeNo string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed[outTradeNo] = true
	return nil
}

func (m *Mock) NotifyAck(w http.ResponseWriter, ok bool) {
	if ok {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "success")
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, "fail")
}

// MarkPaid 让 Query 返回已支付（模拟用户已付款但回调丢失）。
func (m *Mock) MarkPaid(id string, amount int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paid[id] = Notification{OutTradeNo: id, ChannelTradeNo: "mock-" + id, Amount: amount, Paid: true}
}

func (m *Mock) Closed(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed[id]
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/plugins/pay/`
Expected: PASS

- [ ] **Step 5: 告警去重（fleet）**

新建 `internal/fleet/alerts_test.go`：

```go
package fleet_test

import (
	"context"
	"testing"

	"spark-cloud/internal/fleet"
)

func TestRaiseAlertOnce(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	a := fleet.Alert{Level: "critical", Kind: "recharge_amount_mismatch", Message: "充值单 rch-1 金额不符"}
	for i := 0; i < 3; i++ {
		if err := svc.RaiseAlertOnce(ctx, pool, a); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'recharge_amount_mismatch'`).Scan(&n)
	if n != 1 {
		t.Fatalf("alerts = %d, want 1", n)
	}
	if _, err := pool.Exec(ctx, `UPDATE alerts SET resolved_at = created_at`); err != nil {
		t.Fatal(err)
	}
	if err := svc.RaiseAlertOnce(ctx, pool, a); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'recharge_amount_mismatch'`).Scan(&n)
	if n != 2 {
		t.Fatalf("after resolve alerts = %d, want 2", n)
	}
}
```

在 `internal/fleet/alerts.go` 末尾追加：

```go
// RaiseAlertOnce 与 RaiseAlert 相同，但已存在同 kind、同内容且未处理的告警时不再新增（避免重试导致刷屏）。
func (s *Service) RaiseAlertOnce(ctx context.Context, q db.Querier, a Alert) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM alerts WHERE kind = $1 AND message = $2 AND resolved_at IS NULL)`,
		a.Kind, a.Message).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	return s.RaiseAlert(ctx, q, a)
}
```

Run: `go test ./internal/fleet/ -run RaiseAlertOnce`
Expected: PASS

- [ ] **Step 6: 写充值单迁移**

`migrations/00012_recharges.sql`：

```sql
-- +goose Up
CREATE TABLE recharges (
    id               TEXT PRIMARY KEY,                -- rch- + 12 位，同时作为渠道的商户订单号
    user_id          BIGINT NOT NULL REFERENCES users (id),
    channel          TEXT NOT NULL CHECK (channel IN ('wechat', 'alipay', 'manual')),
    amount           BIGINT NOT NULL CHECK (amount > 0),
    status           TEXT NOT NULL CHECK (status IN ('pending', 'paid', 'closed')),
    qr_code          TEXT NOT NULL DEFAULT '',
    channel_trade_no TEXT NOT NULL DEFAULT '',
    operator_id      BIGINT,                          -- 手动入账的管理员
    remark           TEXT NOT NULL DEFAULT '',
    expires_at       TIMESTAMPTZ NOT NULL,            -- 二维码失效时间
    paid_at          TIMESTAMPTZ,
    closed_at        TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX recharges_user_idx ON recharges (user_id, created_at DESC);
CREATE INDEX recharges_pending_idx ON recharges (created_at) WHERE status = 'pending';
CREATE INDEX recharges_created_idx ON recharges (created_at DESC);

-- +goose Down
DROP TABLE recharges;
```

- [ ] **Step 7: 写充值服务的失败测试**

`internal/billing/helpers_test.go`：

```go
package billing_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/billing"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/pay"
	"spark-cloud/internal/wallet"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type env struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	clk    *clock.Fake
	svc    *billing.Service
	wallet *wallet.Service
	wechat *pay.Mock
	alipay *pay.Mock
	userID int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	w := wallet.NewService(pool, clk)
	wx, ali := pay.NewMock("wx-secret"), pay.NewMock("ali-secret")
	svc := billing.NewService(billing.Deps{
		Pool: pool, Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Wallet: w,
		Users:     identity.NewService(identity.Deps{Pool: pool, Clock: clk}),
		Alerts:    fleet.NewService(pool, clk),
		Providers: map[string]pay.Provider{pay.ChannelWechat: wx, pay.ChannelAlipay: ali},
		NotifyBaseURL: "https://spark.test",
	})
	return &env{t: t, ctx: context.Background(), pool: pool, clk: clk, svc: svc, wallet: w, wechat: wx, alipay: ali,
		userID: dbtest.CreateUser(t, pool, "13700000001", true)}
}

func (e *env) create(channel string, amount int64) billing.Recharge {
	e.t.Helper()
	r, err := e.svc.Create(e.ctx, e.userID, channel, amount)
	if err != nil {
		e.t.Fatalf("Create: %v", err)
	}
	return r
}

func (e *env) balance() int64 {
	e.t.Helper()
	b, err := e.wallet.Balance(e.ctx, e.userID)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) status(id string) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT status FROM recharges WHERE id = $1`, id).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) alerts(kind string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, kind).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func codeOf(err error) string {
	if err == nil {
		return ""
	}
	if ae := apperr.As(err); ae != nil {
		return ae.Code
	}
	return "raw: " + err.Error()
}
```

`internal/billing/billing_test.go`：

```go
package billing_test

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/plugins/pay"
)

func paid(r billing.Recharge) pay.Notification {
	return pay.Notification{OutTradeNo: r.ID, ChannelTradeNo: "T-" + r.ID, Amount: r.Amount, Paid: true}
}

func TestCreateValidates(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		channel string
		amount  int64
		want    string
	}{
		{pay.ChannelWechat, 99, "bad_request"},
		{pay.ChannelWechat, 100_000_001, "bad_request"},
		{"paypal", 100, "bad_request"},
		{billing.ChannelManual, 100, "bad_request"},
	} {
		if _, err := e.svc.Create(e.ctx, e.userID, c.channel, c.amount); codeOf(err) != c.want {
			t.Fatalf("Create(%s, %d) = %v, want %s", c.channel, c.amount, err, c.want)
		}
	}
	unverified := dbtest.CreateUser(t, e.pool, "13700000002", false)
	if _, err := e.svc.Create(e.ctx, unverified, pay.ChannelWechat, 100); codeOf(err) != "kyc_required" {
		t.Fatalf("unverified = %v", err)
	}
	r := e.create(pay.ChannelAlipay, 5000)
	if !strings.HasPrefix(r.ID, "rch-") || len(r.ID) != 16 || r.Status != billing.StatusPending || r.QRCode != "mock://pay/"+r.ID ||
		!r.ExpiresAt.Equal(dbtest.T0.Add(billing.QRTTL)) || r.Channel != pay.ChannelAlipay {
		t.Fatalf("recharge = %+v", r)
	}
	if got := e.svc.Channels(); len(got) != 2 || got[0] != "alipay" || got[1] != "wechat" {
		t.Fatalf("channels = %v", got)
	}
}

func TestCreatePendingLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < billing.MaxPending; i++ {
		e.create(pay.ChannelWechat, 100)
	}
	if _, err := e.svc.Create(e.ctx, e.userID, pay.ChannelWechat, 100); codeOf(err) != "too_many_pending" {
		t.Fatalf("6th pending = %v", err)
	}
	e.clk.Advance(billing.QRTTL) // 二维码过期的单不再占名额
	e.create(pay.ChannelWechat, 100)
}

func TestCreateProviderFailureClosesRecharge(t *testing.T) {
	e := newEnv(t)
	e.wechat.FailCreate = true
	if _, err := e.svc.Create(e.ctx, e.userID, pay.ChannelWechat, 100); codeOf(err) != "pay_unavailable" {
		t.Fatalf("create = %v", err)
	}
	var status string
	_ = e.pool.QueryRow(e.ctx, `SELECT status FROM recharges WHERE user_id = $1`, e.userID).Scan(&status)
	if status != billing.StatusClosed {
		t.Fatalf("status = %s", status)
	}
}

func TestSettleCreditsOnceUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 12345)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.svc.Settle(e.ctx, pay.ChannelWechat, paid(r)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if e.balance() != 12345 || e.status(r.ID) != billing.StatusPaid {
		t.Fatalf("balance = %d status = %s", e.balance(), e.status(r.ID))
	}
	var rows int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM wallet_ledger WHERE ref_type = 'recharge' AND ref_id = $1`, r.ID).Scan(&rows)
	if rows != 1 {
		t.Fatalf("ledger rows = %d", rows)
	}
	got, err := e.svc.Get(e.ctx, e.userID, r.ID)
	if err != nil || got.ChannelTradeNo != "T-"+r.ID || got.PaidAt == nil {
		t.Fatalf("recharge = %+v, %v", got, err)
	}
}

func TestSettleIgnoresUnpaid(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 100)
	n := paid(r)
	n.Paid = false
	if err := e.svc.Settle(e.ctx, pay.ChannelWechat, n); err != nil || e.status(r.ID) != billing.StatusPending || e.balance() != 0 {
		t.Fatalf("err=%v status=%s balance=%d", err, e.status(r.ID), e.balance())
	}
}

func TestSettleAmountMismatchRaisesAlertOnce(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 100)
	n := paid(r)
	n.Amount = 1
	for i := 0; i < 3; i++ {
		if err := e.svc.Settle(e.ctx, pay.ChannelWechat, n); err == nil {
			t.Fatal("amount mismatch must fail")
		}
	}
	if e.balance() != 0 || e.status(r.ID) != billing.StatusPending || e.alerts("recharge_amount_mismatch") != 1 {
		t.Fatalf("balance=%d status=%s alerts=%d", e.balance(), e.status(r.ID), e.alerts("recharge_amount_mismatch"))
	}
}

func TestSettleUnknownTradeIsRejected(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 100)
	if err := e.svc.Settle(e.ctx, pay.ChannelWechat, pay.Notification{OutTradeNo: "rch-notmine00000", Amount: 100, Paid: true}); err == nil {
		t.Fatal("unknown out_trade_no must fail")
	}
	if err := e.svc.Settle(e.ctx, pay.ChannelAlipay, paid(r)); err == nil {
		t.Fatal("notification via another channel must fail")
	}
	if e.balance() != 0 || e.status(r.ID) != billing.StatusPending {
		t.Fatalf("balance=%d status=%s", e.balance(), e.status(r.ID))
	}
}

func TestSweepQueriesLostCallbacksAndCloses(t *testing.T) {
	e := newEnv(t)
	lost := e.create(pay.ChannelWechat, 300)
	abandoned := e.create(pay.ChannelAlipay, 500)
	e.wechat.MarkPaid(lost.ID, 300)

	e.clk.Advance(30 * time.Second) // 创建不足 1 分钟不查询
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(lost.ID) != billing.StatusPending {
		t.Fatal("queried too early")
	}
	e.clk.Advance(30 * time.Second)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(lost.ID) != billing.StatusPaid || e.balance() != 300 {
		t.Fatalf("lost callback not recovered: status=%s balance=%d", e.status(lost.ID), e.balance())
	}
	e.clk.Advance(billing.CloseAfter)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(abandoned.ID) != billing.StatusClosed || !e.alipay.Closed(abandoned.ID) {
		t.Fatalf("abandoned recharge: status=%s closedAtChannel=%v", e.status(abandoned.ID), e.alipay.Closed(abandoned.ID))
	}
}

func TestSweepSettlesInsteadOfClosingWhenPaidAtLastCheck(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelAlipay, 700)
	e.clk.Advance(billing.CloseAfter + time.Minute)
	e.alipay.MarkPaid(r.ID, 700)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(r.ID) != billing.StatusPaid || e.balance() != 700 || e.alipay.Closed(r.ID) {
		t.Fatalf("status=%s balance=%d closed=%v", e.status(r.ID), e.balance(), e.alipay.Closed(r.ID))
	}
}

func TestPaidAfterCloseIsCreditedWithWarning(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 900)
	e.clk.Advance(billing.CloseAfter + time.Minute)
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.status(r.ID) != billing.StatusClosed {
		t.Fatal("not closed")
	}
	if err := e.svc.Settle(e.ctx, pay.ChannelWechat, paid(r)); err != nil {
		t.Fatal(err)
	}
	if e.status(r.ID) != billing.StatusPaid || e.balance() != 900 || e.alerts("recharge_paid_after_close") != 1 {
		t.Fatalf("status=%s balance=%d alerts=%d", e.status(r.ID), e.balance(), e.alerts("recharge_paid_after_close"))
	}
}

func TestHandleNotify(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 100)
	body := `{"out_trade_no":"` + r.ID + `","channel_trade_no":"T1","amount":100,"paid":true}`

	req := httptest.NewRequest("POST", "/api/v1/pay/notify/wechat", strings.NewReader(body))
	req.Header.Set(pay.MockSignatureHeader, "forged")
	w := httptest.NewRecorder()
	e.svc.HandleNotify(e.ctx, pay.ChannelWechat, w, req)
	if w.Code != 400 || e.balance() != 0 {
		t.Fatalf("forged notify = %d balance %d", w.Code, e.balance())
	}

	req = httptest.NewRequest("POST", "/api/v1/pay/notify/wechat", strings.NewReader(body))
	req.Header.Set(pay.MockSignatureHeader, "wx-secret")
	w = httptest.NewRecorder()
	e.svc.HandleNotify(e.ctx, pay.ChannelWechat, w, req)
	if w.Code != 200 || w.Body.String() != "success" || e.balance() != 100 {
		t.Fatalf("notify = %d %q balance %d", w.Code, w.Body.String(), e.balance())
	}

	w = httptest.NewRecorder()
	e.svc.HandleNotify(e.ctx, "paypal", w, httptest.NewRequest("POST", "/api/v1/pay/notify/paypal", nil))
	if w.Code != 404 {
		t.Fatalf("unknown channel = %d", w.Code)
	}
}

func TestManualRecharge(t *testing.T) {
	e := newEnv(t)
	actor := audit.Actor{AdminID: 7, IP: "10.0.0.2"}
	if _, err := e.svc.ManualRecharge(e.ctx, actor, e.userID, 10000, " "); codeOf(err) != "bad_request" {
		t.Fatalf("empty remark = %v", err)
	}
	if _, err := e.svc.ManualRecharge(e.ctx, actor, e.userID, 0, "转账"); codeOf(err) != "bad_request" {
		t.Fatalf("zero amount = %v", err)
	}
	if _, err := e.svc.ManualRecharge(e.ctx, actor, 9999, 10000, "转账"); codeOf(err) != "not_found" {
		t.Fatalf("unknown user = %v", err)
	}
	r, err := e.svc.ManualRecharge(e.ctx, actor, e.userID, 10000, "对公转账 流水 123")
	if err != nil {
		t.Fatal(err)
	}
	if r.Channel != billing.ChannelManual || r.Status != billing.StatusPaid || r.OperatorID == nil || *r.OperatorID != 7 || e.balance() != 10000 {
		t.Fatalf("recharge = %+v balance=%d", r, e.balance())
	}
	var kind string
	var operator int64
	_ = e.pool.QueryRow(e.ctx, `SELECT kind, operator_id FROM wallet_ledger WHERE ref_id = $1`, r.ID).Scan(&kind, &operator)
	var auditN int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = 'wallet.manual_recharge' AND actor_id = 7 AND ip = '10.0.0.2'`).Scan(&auditN)
	if kind != "recharge" || operator != 7 || auditN != 1 {
		t.Fatalf("ledger kind=%s operator=%d audit=%d", kind, operator, auditN)
	}
}

func TestListAndGetForUser(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		e.create(pay.ChannelWechat, 100)
		e.clk.Advance(time.Second)
	}
	items, total, err := e.svc.ListForUser(e.ctx, e.userID, paging.Page{Number: 1, Size: 2})
	if err != nil || total != 3 || len(items) != 2 || !items[0].CreatedAt.After(items[1].CreatedAt) {
		t.Fatalf("list = %d items, total %d, %v", len(items), total, err)
	}
	other := dbtest.CreateUser(t, e.pool, "13700000003", true)
	if _, err := e.svc.Get(e.ctx, other, items[0].ID); codeOf(err) != "not_found" {
		t.Fatalf("get other's recharge = %v", err)
	}
}
```

- [ ] **Step 8: 运行确认失败**

Run: `go test ./internal/billing/`
Expected: 编译失败（包不存在）。

- [ ] **Step 9: 实现充值服务**

`internal/billing/billing.go`：

```go
// Package billing 管理用户充值：扫码充值单、支付回调与主动查询入账、超时关单、后台手动入账。
// 加锁顺序：recharge → wallet。
package billing

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/plugins/pay"
	"spark-cloud/internal/wallet"
)

const (
	ChannelManual = "manual"

	StatusPending = "pending"
	StatusPaid    = "paid"
	StatusClosed  = "closed"

	MinAmount  = 100         // 1 元
	MaxAmount  = 100_000_000 // 100 万元
	MaxPending = 5           // 同一用户同时有效的待支付单上限

	QRTTL      = 15 * time.Minute // 二维码有效期
	QueryAfter = time.Minute      // 创建满 1 分钟后开始主动查询
	CloseAfter = 2 * time.Hour    // 创建满 2 小时仍未支付则关单

	sweepBatch = 200
)

var (
	ErrTooManyPending = apperr.New("too_many_pending", "待支付的充值单过多，请先完成支付或 15 分钟后再试", 409)
	ErrPayUnavailable = apperr.New("pay_unavailable", "支付渠道暂时不可用，请稍后再试或换一种支付方式", 502)
)

type Recharge struct {
	ID             string     `json:"id"`
	UserID         int64      `json:"user_id"`
	Channel        string     `json:"channel"`
	Amount         int64      `json:"amount"`
	Status         string     `json:"status"`
	QRCode         string     `json:"qr_code,omitempty"`
	ChannelTradeNo string     `json:"channel_trade_no,omitempty"`
	OperatorID     *int64     `json:"operator_id,omitempty"`
	Remark         string     `json:"remark,omitempty"`
	ExpiresAt      time.Time  `json:"expires_at"`
	PaidAt         *time.Time `json:"paid_at"`
	ClosedAt       *time.Time `json:"closed_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// UserChecker 校验用户可以充值（账号正常且已实名）。
type UserChecker interface {
	CanRent(ctx context.Context, userID int64) error
}

// AlertRaiser 写后台告警（由 fleet 模块实现）。
type AlertRaiser interface {
	RaiseAlertOnce(ctx context.Context, q db.Querier, a fleet.Alert) error
}

type Deps struct {
	Pool          *pgxpool.Pool
	Clock         clock.Clock
	Log           *slog.Logger
	Wallet        *wallet.Service
	Users         UserChecker
	Alerts        AlertRaiser
	Providers     map[string]pay.Provider // 已启用的扫码渠道：wechat / alipay
	NotifyBaseURL string                  // 回调地址前缀，回调地址为 <base>/api/v1/pay/notify/<channel>
}

type Service struct {
	d Deps
}

func NewService(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Service{d: d}
}

const rechargeCols = `id, user_id, channel, amount, status, qr_code, channel_trade_no, operator_id, remark,
	expires_at, paid_at, closed_at, created_at`

func scanRecharge(row pgx.Row) (Recharge, error) {
	var r Recharge
	err := row.Scan(&r.ID, &r.UserID, &r.Channel, &r.Amount, &r.Status, &r.QRCode, &r.ChannelTradeNo, &r.OperatorID, &r.Remark,
		&r.ExpiresAt, &r.PaidAt, &r.ClosedAt, &r.CreatedAt)
	if db.IsNoRows(err) {
		return Recharge{}, apperr.ErrNotFound
	}
	return r, err
}

func collectRecharges(rows pgx.Rows, err error) ([]Recharge, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Recharge, error) { return scanRecharge(r) })
}

// Channels 返回已启用的扫码渠道（按名称排序）。
func (s *Service) Channels() []string {
	out := make([]string, 0, len(s.d.Providers))
	for name := range s.d.Providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func channelName(channel string) string {
	switch channel {
	case pay.ChannelWechat:
		return "微信"
	case pay.ChannelAlipay:
		return "支付宝"
	}
	return "手动"
}

// Create 创建扫码充值单：先落库（便于对账），再到渠道下单拿二维码。
func (s *Service) Create(ctx context.Context, userID int64, channel string, amount int64) (Recharge, error) {
	p, ok := s.d.Providers[channel]
	if !ok {
		return Recharge{}, apperr.BadRequest("不支持的支付方式")
	}
	if amount < MinAmount || amount > MaxAmount {
		return Recharge{}, apperr.BadRequest("充值金额需在 1 元到 100 万元之间")
	}
	if err := s.d.Users.CanRent(ctx, userID); err != nil {
		return Recharge{}, err
	}
	now := s.d.Clock.Now()
	var r Recharge
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		// 串行化同一用户的创建请求，保证待支付单上限不被并发绕过
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "recharge:"+strconv.FormatInt(userID, 10)); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM recharges WHERE user_id = $1 AND status = 'pending' AND expires_at > $2`,
			userID, now).Scan(&pending); err != nil {
			return err
		}
		if pending >= MaxPending {
			return ErrTooManyPending
		}
		var err error
		r, err = scanRecharge(tx.QueryRow(ctx, `INSERT INTO recharges (id, user_id, channel, amount, status, expires_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'pending', $5, $6, $6) RETURNING `+rechargeCols,
			randx.ID("rch-", 12), userID, channel, amount, now.Add(QRTTL), now))
		return err
	})
	if err != nil {
		return Recharge{}, err
	}
	qr, err := p.Create(ctx, pay.Recharge{ID: r.ID, Amount: amount, Subject: "Spark 算力余额充值", ExpiresAt: r.ExpiresAt,
		NotifyURL: s.d.NotifyBaseURL + "/api/v1/pay/notify/" + channel})
	if err != nil {
		s.d.Log.Error("pay create failed", "recharge", r.ID, "channel", channel, "err", err)
		closedAt := s.d.Clock.Now()
		if _, cerr := s.d.Pool.Exec(context.WithoutCancel(ctx), `UPDATE recharges SET status = 'closed', closed_at = $2,
				remark = '渠道下单失败', updated_at = $2 WHERE id = $1 AND status = 'pending'`, r.ID, closedAt); cerr != nil {
			s.d.Log.Error("close failed recharge", "recharge", r.ID, "err", cerr)
		}
		return Recharge{}, ErrPayUnavailable
	}
	if _, err := s.d.Pool.Exec(ctx, `UPDATE recharges SET qr_code = $2, updated_at = $3 WHERE id = $1`, r.ID, qr, now); err != nil {
		return Recharge{}, err
	}
	r.QRCode = qr
	return r, nil
}

// Get 返回用户自己的充值单。
func (s *Service) Get(ctx context.Context, userID int64, id string) (Recharge, error) {
	return scanRecharge(s.d.Pool.QueryRow(ctx, `SELECT `+rechargeCols+` FROM recharges WHERE id = $1 AND user_id = $2`, id, userID))
}

// ListForUser 按创建时间倒序分页返回用户的充值单。
func (s *Service) ListForUser(ctx context.Context, userID int64, p paging.Page) ([]Recharge, int, error) {
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM recharges WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	items, err := collectRecharges(s.d.Pool.Query(ctx, `SELECT `+rechargeCols+` FROM recharges WHERE user_id = $1
		ORDER BY created_at DESC, id LIMIT $2 OFFSET $3`, userID, p.Limit(), p.Offset()))
	return items, total, err
}

// ManualRecharge 后台手动入账（线下转账等），记为充值流水，渠道为 manual。
func (s *Service) ManualRecharge(ctx context.Context, actor audit.Actor, userID, amount int64, remark string) (Recharge, error) {
	remark = strings.TrimSpace(remark)
	if remark == "" {
		return Recharge{}, apperr.BadRequest("请填写入账说明（例如转账流水号）")
	}
	if amount < MinAmount || amount > MaxAmount {
		return Recharge{}, apperr.BadRequest("入账金额需在 1 元到 100 万元之间")
	}
	now := s.d.Clock.Now()
	var r Recharge
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var err error
		r, err = scanRecharge(tx.QueryRow(ctx, `INSERT INTO recharges (id, user_id, channel, amount, status, operator_id, remark,
				expires_at, paid_at, created_at, updated_at)
			VALUES ($1, $2, 'manual', $3, 'paid', $4, $5, $6, $6, $6, $6) RETURNING `+rechargeCols,
			randx.ID("rch-", 12), userID, amount, actor.AdminID, remark, now))
		if db.IsForeignKeyViolation(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{UserID: userID, Amount: amount, Kind: wallet.KindRecharge,
			RefType: "recharge", RefID: r.ID, OperatorID: actor.AdminID, Remark: "手动入账：" + remark}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("wallet.manual_recharge", "user", strconv.FormatInt(userID, 10),
			map[string]any{"recharge_id": r.ID, "amount": amount, "remark": remark}))
	})
	return r, err
}
```

`internal/billing/settle.go`：

```go
package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/plugins/pay"
	"spark-cloud/internal/wallet"
)

var (
	errUnknownTrade   = errors.New("billing: 未知的商户订单号或渠道不符")
	errAmountMismatch = errors.New("billing: 支付金额与充值单不符")
)

// Settle 按支付结果入账；回调、主动查询、开发环境模拟支付共用。重复调用只入账一次。
// 未支付的结果忽略；金额不符写告警且不入账；已关闭的单收到支付成功照常入账并写告警。
func (s *Service) Settle(ctx context.Context, channel string, n pay.Notification) error {
	if !n.Paid {
		return nil
	}
	now := s.d.Clock.Now()
	var bizErr error
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		r, err := scanRecharge(tx.QueryRow(ctx, `SELECT `+rechargeCols+` FROM recharges WHERE id = $1 FOR UPDATE`, n.OutTradeNo))
		if errors.Is(err, apperr.ErrNotFound) || (err == nil && r.Channel != channel) {
			bizErr = fmt.Errorf("%w: %s via %s", errUnknownTrade, n.OutTradeNo, channel)
			return nil
		}
		if err != nil {
			return err
		}
		if r.Status == StatusPaid {
			return nil
		}
		if n.Amount != r.Amount {
			bizErr = fmt.Errorf("%w: %s want %d got %d", errAmountMismatch, r.ID, r.Amount, n.Amount)
			return s.d.Alerts.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "critical", Kind: "recharge_amount_mismatch",
				Message: fmt.Sprintf("充值单 %s 金额 %d 分，渠道通知 %d 分（渠道单号 %s），未入账，请人工核对",
					r.ID, r.Amount, n.Amount, n.ChannelTradeNo)})
		}
		if _, err := tx.Exec(ctx, `UPDATE recharges SET status = 'paid', channel_trade_no = $2, paid_at = $3, updated_at = $3
			WHERE id = $1`, r.ID, n.ChannelTradeNo, now); err != nil {
			return err
		}
		if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{UserID: r.UserID, Amount: r.Amount, Kind: wallet.KindRecharge,
			RefType: "recharge", RefID: r.ID, Remark: channelName(r.Channel) + "充值"}); err != nil {
			return err
		}
		if r.Status == StatusClosed {
			return s.d.Alerts.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "warning", Kind: "recharge_paid_after_close",
				Message: fmt.Sprintf("充值单 %s 关闭后收到支付成功（%d 分，渠道单号 %s），已照常入账，请核对", r.ID, r.Amount, n.ChannelTradeNo)})
		}
		return nil
	})
	if err != nil {
		return err
	}
	return bizErr
}

// HandleNotify 处理渠道的异步回调：验签 → 入账 → 按渠道格式回复。验签失败只记日志（不写库、不写告警，避免被刷）。
func (s *Service) HandleNotify(ctx context.Context, channel string, w http.ResponseWriter, r *http.Request) {
	p, ok := s.d.Providers[channel]
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, err := p.ParseNotify(r)
	if err != nil {
		s.d.Log.Warn("pay notify rejected", "channel", channel, "err", err)
		p.NotifyAck(w, false)
		return
	}
	if err := s.Settle(ctx, channel, n); err != nil {
		s.d.Log.Error("pay notify settle failed", "channel", channel, "out_trade_no", n.OutTradeNo, "err", err)
		p.NotifyAck(w, false)
		return
	}
	p.NotifyAck(w, true)
}

type pendingRef struct {
	id      string
	channel string
}

func (s *Service) pending(ctx context.Context, pred string, args ...any) ([]pendingRef, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT id, channel FROM recharges
		WHERE status = 'pending' AND channel <> 'manual' AND `+pred+` ORDER BY created_at LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (pendingRef, error) {
		var p pendingRef
		err := r.Scan(&p.id, &p.channel)
		return p, err
	})
}

// Sweep 主动查询创建 1 分钟～2 小时仍未支付的单（回调可能丢失），关闭创建超过 2 小时的单。
func (s *Service) Sweep(ctx context.Context) error {
	now := s.d.Clock.Now()
	var errs []error
	due, err := s.pending(ctx, `created_at <= $1 AND created_at > $2`, now.Add(-QueryAfter), now.Add(-CloseAfter))
	if err != nil {
		errs = append(errs, fmt.Errorf("list due: %w", err))
	}
	for _, r := range due {
		if _, err := s.queryAndSettle(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	stale, err := s.pending(ctx, `created_at <= $1`, now.Add(-CloseAfter))
	if err != nil {
		errs = append(errs, fmt.Errorf("list stale: %w", err))
	}
	for _, r := range stale {
		// 关单前最后查一次，避免关掉刚付款的单
		settled, err := s.queryAndSettle(ctx, r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if settled {
			continue
		}
		if p := s.d.Providers[r.channel]; p != nil {
			if err := p.Close(ctx, r.id); err != nil {
				s.d.Log.Warn("pay close failed", "recharge", r.id, "channel", r.channel, "err", err)
			}
		}
		if _, err := s.d.Pool.Exec(ctx, `UPDATE recharges SET status = 'closed', closed_at = $2, updated_at = $2
			WHERE id = $1 AND status = 'pending'`, r.id, now); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", r.id, err))
		}
	}
	return errors.Join(errs...)
}

// queryAndSettle 向渠道查询一笔单，已支付则入账；渠道已下线（未配置）时返回 false。
func (s *Service) queryAndSettle(ctx context.Context, r pendingRef) (bool, error) {
	p := s.d.Providers[r.channel]
	if p == nil {
		return false, nil
	}
	n, err := p.Query(ctx, r.id)
	if err != nil {
		return false, fmt.Errorf("query %s: %w", r.id, err)
	}
	if !n.Paid {
		return false, nil
	}
	n.OutTradeNo = r.id
	if err := s.Settle(ctx, r.channel, n); err != nil {
		return false, fmt.Errorf("settle %s: %w", r.id, err)
	}
	return true, nil
}
```

- [ ] **Step 10: 运行确认通过**

Run: `go test ./internal/billing/ ./internal/plugins/pay/ ./internal/fleet/`
Expected: PASS

- [ ] **Step 11: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add migrations/00012_recharges.sql internal/plugins/pay internal/billing internal/fleet
git commit -m "feat(billing): recharge orders with idempotent settlement, lost-callback query, timeout close and manual credit

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: 充值的用户接口、支付回调路由、清扫附加任务与组装

**Files:**
- Create: `internal/api/recharges.go`、`internal/api/recharges_test.go`
- Modify: `internal/api/server.go`（`Billing`、`DevPay` 字段，路由，回调免 JSON 检查）
- Modify: `internal/api/api_test.go`（`newTestEnv` 注入充值服务）
- Modify: `internal/rental/sweeper.go`（`RunSweeper` 支持附加任务）、`internal/rental/sweeper_test.go`（追加测试）
- Modify: `internal/app/app.go`、`internal/app/e2e_test.go`
- Modify: `cmd/platform/main.go`（清扫器带上充值补查）

**Interfaces:**
- Consumes: Task 8 的 `billing.Service`（`Create`、`Get`、`ListForUser`、`Settle`、`HandleNotify`、`Sweep`、`Channels`）、`pay.Mock`、`pay.Notification`；Task 1 的 `paging.Parse`。
- Produces：
  - 用户接口：`GET /api/v1/pay/channels` → `{items: ["alipay","wechat"]}`（无需登录）；`POST /api/v1/recharges {amount, channel}` → 201 充值单；`GET /api/v1/recharges/{id}`；`GET /api/v1/recharges?page=&page_size=` → `{items, total}`；`POST /api/v1/pay/notify/{channel}`（无会话、不要求 JSON）；`POST /api/v1/dev/recharges/{id}/pay`（仅 `Server.DevPay` 为 true 时注册）。
  - 用户视图字段：`id, channel, amount, status, qr_code（仅 pending 时）, expires_at, paid_at, closed_at, created_at`。
  - `api.Server.Billing *billing.Service`、`api.Server.DevPay bool`。
  - `(*rental.Service).RunSweeper(ctx, interval time.Duration, extra ...func(context.Context) error)`。
  - `app.Options.Pay map[string]pay.Provider`（测试注入）；`app.App.Billing *billing.Service`。
  - `app.Build`：`PayDriver` 为 `""` 或 `"mock"` 时两个渠道都用 mock，`DevPay = Env != "prod"`；`"real"` 在 Task 10 接入（本任务先返回错误）；生产环境非 `real` 报错。

- [ ] **Step 1: 清扫器附加任务（失败测试）**

在 `internal/rental/sweeper_test.go` 末尾追加：

```go
func TestRunSweeperRunsExtraTasks(t *testing.T) {
	e := newEnv(t, 0)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	var calls []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.svc.RunSweeper(ctx, time.Hour,
			func(context.Context) error { calls = append(calls, "a"); return errors.New("boom") },
			func(context.Context) error { calls = append(calls, "b"); cancel(); return nil })
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunSweeper did not stop")
	}
	if len(calls) != 2 || calls[0] != "a" || calls[1] != "b" {
		t.Fatalf("calls = %v (a failing task must not stop later tasks)", calls)
	}
}
```

（若 `sweeper_test.go` 尚未 import `"errors"`，补上。）

Run: `go test ./internal/rental/ -run RunSweeperRunsExtraTasks`
Expected: 编译失败（`RunSweeper` 参数过多）。

- [ ] **Step 2: 实现附加任务**

`internal/rental/sweeper.go` 中把 `RunSweeper` 与 `sweepOnce` 替换为：

```go
// RunSweeper 每隔 interval 执行一次 Sweep 及附加任务（例如充值补查与关单），直到 ctx 结束。
// 多个 platform 副本中只有拿到咨询锁的一个会执行。附加任务出错只记日志，不影响后续任务。
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration, extra ...func(context.Context) error) {
	s.sweepOnce(ctx, extra)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOnce(ctx, extra)
		}
	}
}

func (s *Service) sweepOnce(ctx context.Context, extra []func(context.Context) error) {
	conn, err := s.d.Pool.Acquire(ctx)
	if err != nil {
		s.d.Log.Error("sweeper acquire conn", "err", err)
		return
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, sweeperLockKey).Scan(&locked); err != nil {
		s.d.Log.Error("sweeper advisory lock", "err", err)
		return
	}
	if !locked {
		return
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, sweeperLockKey) //nolint:errcheck
	if err := s.Sweep(ctx); err != nil {
		s.d.Log.Error("sweep failed", "err", err)
	}
	for i, task := range extra {
		if err := task(ctx); err != nil {
			s.d.Log.Error("sweep task failed", "task", i, "err", err)
		}
	}
}
```

Run: `go test ./internal/rental/`
Expected: PASS

- [ ] **Step 3: 写充值接口的失败测试**

`internal/api/api_test.go` 的 `newTestEnv`：

1. import 增加 `"spark-cloud/internal/billing"`、`"spark-cloud/internal/plugins/pay"`。
2. `testEnv` 结构体增加字段 `pay *pay.Mock`。
3. 在构造 `srv` 之前加入：

```go
	payMock := pay.NewMock("test-secret")
	bill := billing.NewService(billing.Deps{
		Pool: pool, Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Wallet: w, Users: ids, Alerts: f,
		Providers:     map[string]pay.Provider{pay.ChannelWechat: payMock, pay.ChannelAlipay: payMock},
		NotifyBaseURL: "https://spark.test",
	})
```

4. `&api.Server{...}` 加上 `Billing: bill, DevPay: true,`；返回值加上 `pay: payMock`。

`internal/api/recharges_test.go`：

```go
package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spark-cloud/internal/api"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/pay"
)

// verifiedClient 登录并完成实名，返回客户端与用户 ID。
func (e *testEnv) verifiedClient(phone string) (*client, int64) {
	c := e.client()
	u := c.login(phone)
	if code, out := c.do("POST", "/kyc/personal", map[string]string{"name": "张三", "id_number": "11010119900101123X"}); code != 200 {
		e.t.Fatalf("kyc = %d %v", code, out)
	}
	return c, int64(u["id"].(float64))
}

func TestRechargeFlowWithDevPay(t *testing.T) {
	e := newTestEnv(t)
	c, _ := e.verifiedClient("13800000011")
	if code, out := c.do("GET", "/pay/channels", nil); code != 200 || len(out["items"].([]any)) != 2 {
		t.Fatalf("channels = %d %v", code, out)
	}
	code, out := c.do("POST", "/recharges", map[string]any{"amount": 5000, "channel": "wechat"})
	if code != 201 || out["status"] != "pending" || !strings.HasPrefix(out["qr_code"].(string), "mock://pay/rch-") {
		t.Fatalf("create = %d %v", code, out)
	}
	if _, ok := out["user_id"]; ok {
		t.Fatal("user_id must not be exposed")
	}
	id := out["id"].(string)
	if code, out := c.do("POST", "/dev/recharges/"+id+"/pay", map[string]any{}); code != 200 || out["status"] != "paid" {
		t.Fatalf("dev pay = %d %v", code, out)
	}
	if code, out := c.do("GET", "/recharges/"+id, nil); code != 200 || out["status"] != "paid" || out["qr_code"] != nil {
		t.Fatalf("get = %d %v", code, out)
	}
	if code, out := c.do("GET", "/wallet", nil); code != 200 || out["balance"].(float64) != 5000 {
		t.Fatalf("wallet = %d %v", code, out)
	}
	if code, out := c.do("GET", "/recharges?page=1&page_size=10", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	if code, out := c.do("POST", "/dev/recharges/"+id+"/pay", map[string]any{}); code != 409 || out["code"] != "invalid_state" {
		t.Fatalf("dev pay twice = %d %v", code, out)
	}
	other, _ := e.verifiedClient("13800000012")
	if code, _ := other.do("GET", "/recharges/"+id, nil); code != 404 {
		t.Fatalf("other user's recharge = %d", code)
	}
	if code, _ := other.do("POST", "/dev/recharges/"+id+"/pay", map[string]any{}); code != 404 {
		t.Fatalf("other user dev pay = %d", code)
	}
}

func TestRechargeRequiresLoginAndKYC(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	if code, _ := c.do("POST", "/recharges", map[string]any{"amount": 5000, "channel": "wechat"}); code != 401 {
		t.Fatalf("anonymous = %d", code)
	}
	c.login("13800000013")
	if code, out := c.do("POST", "/recharges", map[string]any{"amount": 5000, "channel": "wechat"}); code != 403 || out["code"] != "kyc_required" {
		t.Fatalf("without kyc = %d %v", code, out)
	}
}

func TestPayNotifyEndpoint(t *testing.T) {
	e := newTestEnv(t)
	c, _ := e.verifiedClient("13800000014")
	_, out := c.do("POST", "/recharges", map[string]any{"amount": 800, "channel": "alipay"})
	id := out["id"].(string)
	notify := func(sig, body string) (int, string) {
		req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/pay/notify/alipay", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded") // 渠道回调不是 JSON，不能被统一的 JSON 检查拦下
		req.Header.Set(pay.MockSignatureHeader, sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	body := `{"out_trade_no":"` + id + `","channel_trade_no":"A1","amount":800,"paid":true}`
	if code, text := notify("forged", body); code != 400 || text != "fail" {
		t.Fatalf("forged = %d %q", code, text)
	}
	if code, text := notify("test-secret", body); code != 200 || text != "success" {
		t.Fatalf("notify = %d %q", code, text)
	}
	if code, text := notify("test-secret", body); code != 200 || text != "success" {
		t.Fatalf("duplicate notify = %d %q", code, text)
	}
	if _, out := c.do("GET", "/wallet", nil); out["balance"].(float64) != 800 {
		t.Fatalf("balance = %v", out["balance"])
	}
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/pay/notify/paypal", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown channel = %d", resp.StatusCode)
	}
}

func TestDevPayNotRegisteredWhenDisabled(t *testing.T) {
	srv := &api.Server{Clock: clock.NewFake(dbtest.T0), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DevPay: false}
	ts := httptest.NewServer(srv.Router())
	defer ts.Close()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL+"/api/v1/dev/recharges/rch-1/pay", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("dev pay route without DevPay = %d, want 404", resp.StatusCode)
	}
}
```

Run: `go test ./internal/api/ -run 'Recharge|PayNotify|DevPay'`
Expected: 编译失败（`Billing`、`DevPay` 字段不存在）。

- [ ] **Step 4: 实现充值接口**

`internal/api/recharges.go`：

```go
package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/billing"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/plugins/pay"
)

const notifyPathPrefix = "/api/v1/pay/notify/"

type rechargeView struct {
	ID        string     `json:"id"`
	Channel   string     `json:"channel"`
	Amount    int64      `json:"amount"`
	Status    string     `json:"status"`
	QRCode    string     `json:"qr_code,omitempty"` // 仅待支付时返回
	ExpiresAt time.Time  `json:"expires_at"`
	PaidAt    *time.Time `json:"paid_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	CreatedAt time.Time  `json:"created_at"`
}

func toRechargeView(r billing.Recharge) rechargeView {
	v := rechargeView{ID: r.ID, Channel: r.Channel, Amount: r.Amount, Status: r.Status, ExpiresAt: r.ExpiresAt,
		PaidAt: r.PaidAt, ClosedAt: r.ClosedAt, CreatedAt: r.CreatedAt}
	if r.Status == billing.StatusPending {
		v.QRCode = r.QRCode
	}
	return v
}

func (s *Server) handlePayChannels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, list(s.Billing.Channels()))
}

func (s *Server) handleCreateRecharge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Amount  int64  `json:"amount"`
		Channel string `json:"channel"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	rch, err := s.Billing.Create(r.Context(), currentUser(r).ID, req.Channel, req.Amount)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRechargeView(rch))
}

func (s *Server) handleRecharge(w http.ResponseWriter, r *http.Request) {
	rch, err := s.Billing.Get(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRechargeView(rch))
}

func (s *Server) handleRecharges(w http.ResponseWriter, r *http.Request) {
	p, err := paging.Parse(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items, total, err := s.Billing.ListForUser(r.Context(), currentUser(r).ID, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]rechargeView, 0, len(items))
	for _, it := range items {
		views = append(views, toRechargeView(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views, "total": total})
}

// handlePayNotify 接收支付渠道回调：无会话，由插件验签；响应格式由渠道决定。
func (s *Server) handlePayNotify(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	s.Billing.HandleNotify(r.Context(), chi.URLParam(r, "channel"), w, r)
}

// handleDevPay 仅开发环境：把自己的待支付充值单模拟为支付成功，走与回调相同的入账逻辑。
func (s *Server) handleDevPay(w http.ResponseWriter, r *http.Request) {
	uid := currentUser(r).ID
	rch, err := s.Billing.Get(r.Context(), uid, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if rch.Status != billing.StatusPending {
		s.writeError(w, r, apperr.ErrInvalidState)
		return
	}
	if err := s.Billing.Settle(r.Context(), rch.Channel, pay.Notification{
		OutTradeNo: rch.ID, ChannelTradeNo: "dev-" + rch.ID, Amount: rch.Amount, Paid: true,
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	if rch, err = s.Billing.Get(r.Context(), uid, rch.ID); err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRechargeView(rch))
}
```

`internal/api/server.go`：

1. `Server` 增加字段：

```go
	Billing      *billing.Service
	DevPay       bool // 仅开发环境（mock 支付）为 true：注册模拟支付成功接口
```

（import 增加 `"spark-cloud/internal/billing"`。）

2. 路由中，在 `r.Get("/images", s.handleImages)` 之后加入：

```go
		r.Get("/pay/channels", s.handlePayChannels)
		r.Post("/pay/notify/{channel}", s.handlePayNotify)
```

在 `requireUser` 分组末尾加入：

```go
			r.Get("/recharges", s.handleRecharges)
			r.Post("/recharges", s.handleCreateRecharge)
			r.Get("/recharges/{id}", s.handleRecharge)
			if s.DevPay {
				r.Post("/dev/recharges/{id}/pay", s.handleDevPay)
			}
```

3. `requireJSONForWrites` 中，在计算 `want, msg` 之前加入：

```go
		if strings.HasPrefix(r.URL.Path, notifyPathPrefix) { // 支付回调的格式由渠道决定，由插件自行验签解析
			next.ServeHTTP(w, r)
			return
		}
```

Run: `go test ./internal/api/`
Expected: PASS

- [ ] **Step 5: 组装与端到端测试**

`internal/app/app.go`：

1. import 增加 `"spark-cloud/internal/billing"`、`"spark-cloud/internal/plugins/pay"`、`"spark-cloud/internal/platform/randx"`。
2. `Options` 增加字段 `Pay map[string]pay.Provider // 测试注入的支付渠道`；`App` 增加字段 `Billing *billing.Service`。
3. 生产环境检查改为：

```go
	if cfg.Env == "prod" && ((opt.SMS == nil && cfg.SMSDriver == "mock") || (opt.KYC == nil && cfg.KYCDriver == "mock") ||
		(opt.Pay == nil && cfg.PayDriver != "real")) {
		return nil, errors.New("生产环境禁止使用 mock 插件")
	}
```

4. 在 `r := rental.NewService(...)` 之后加入：

```go
	providers := opt.Pay
	devPay := false
	if providers == nil {
		switch cfg.PayDriver {
		case "", "mock":
			// mock 回调密钥随机生成，外部无法伪造；开发环境通过 /api/v1/dev/recharges/{id}/pay 模拟支付
			m := pay.NewMock(randx.Token())
			providers = map[string]pay.Provider{pay.ChannelWechat: m, pay.ChannelAlipay: m}
			devPay = cfg.Env != "prod"
		case "real":
			return nil, errors.New("真实支付渠道尚未接入")
		default:
			return nil, fmt.Errorf("未知的支付驱动 %q", cfg.PayDriver)
		}
	}
	bill := billing.NewService(billing.Deps{
		Pool: pool, Clock: clk, Log: log, Wallet: w, Users: ids, Alerts: f,
		Providers: providers, NotifyBaseURL: cfg.PublicBaseURL,
	})
```

5. `api.Server` 字面量加 `Billing: bill, DevPay: devPay,`；返回的 `App` 加 `Billing: bill,`。

`cmd/platform/main.go`：`a.Rental.RunSweeper(ctx, cfg.SweepInterval)` 改为 `a.Rental.RunSweeper(ctx, cfg.SweepInterval, a.Billing.Sweep)`。

`internal/app/e2e_test.go` 的 `TestFullRentalLifecycle`：把

```go
	if _, err := a.Wallet.AdminAdjust(ctx, 1, int64(user["id"].(float64)), 100000, "线下转账", ""); err != nil {
		t.Fatal(err)
	}
```

替换为通过充值接口充值（开发环境模拟支付），并把上面的 `user := c.do("POST", "/auth/login", ...)` 改为 `c.do("POST", "/auth/login", ...)`（不再需要 user 变量）：

```go
	rch := c.do("POST", "/recharges", map[string]any{"amount": 100000, "channel": "wechat"})
	if paid := c.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{}); paid["status"] != "paid" {
		t.Fatalf("recharge = %v", paid)
	}
```

（流水条数断言保持 3：充值 1 条 + 两次扣款。）

并在文件末尾追加：

```go
func TestBuildRejectsMockPayInProd(t *testing.T) {
	cfg := config.Config{Env: "prod", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "aliyun", KYCDriver: "aliyun",
		PayDriver: "mock", UploadDir: t.TempDir()}
	if _, err := app.Build(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{}); err == nil {
		t.Fatal("expected error for mock pay in prod")
	}
}
```

- [ ] **Step 6: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/api internal/rental internal/app cmd/platform
git commit -m "feat(api): recharge endpoints, payment callback route, dev-only simulated payment, billing sweep

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: 微信支付 Native 与支付宝当面付插件

**Files:**
- Create: `internal/plugins/pay/amount.go`、`internal/plugins/pay/wechat.go`、`internal/plugins/pay/alipay.go`
- Test: `internal/plugins/pay/amount_test.go`、`internal/plugins/pay/wechat_test.go`、`internal/plugins/pay/alipay_test.go`（均为 `package pay`，需要访问未导出的构造函数）
- Modify: `internal/app/app.go`（`PayDriver = "real"`）、`internal/app/e2e_test.go`
- Modify: `go.mod` / `go.sum`（`github.com/wechatpay-apiv3/wechatpay-go v0.2.21`、`github.com/smartwalle/alipay/v3 v3.2.31`、`github.com/smartwalle/nsign v1.0.9`）

**Interfaces:**
- Consumes: Task 8 的 `pay.Provider`、`pay.Recharge`、`pay.Notification`；Task 1 的 `config.WechatConfig`、`config.AlipayConfig`。
- Produces：
  - `pay.WechatConfig{MchID, AppID, APIv3Key, CertSerial string; PrivateKey *rsa.PrivateKey; PublicKeyID string; PublicKey *rsa.PublicKey}`、`pay.NewWechat(ctx, cfg WechatConfig) (*pay.Wechat, error)`。
  - `pay.AlipayConfig{AppID, PrivateKeyPEM, AlipayPublicKeyPEM string; Production bool}`、`pay.NewAlipay(cfg AlipayConfig) (*pay.Alipay, error)`。
  - 包内 `yuanToFen(s string) (int64, error)`、`fenToYuan(fen int64) string`。
  - `app.Build` 在 `PayDriver = "real"` 时按配置读取密钥文件并启用已配置的渠道。

渠道约定：
- 微信：Native 下单返回 `code_url` 作为二维码内容；回调用「微信支付公钥」验签（请求头 `Wechatpay-Serial` 必须等于公钥 ID）并用 APIv3 密钥解密 `resource`；`trade_state == "SUCCESS"` 为已支付；查询或关单返回 `ORDER_NOT_EXIST` 视为未支付 / 已关闭；回调成功回复 200 空响应，失败回复 500 与 `{"code":"FAIL","message":"失败"}`。
- 支付宝：当面付 `alipay.trade.precreate` 返回 `qr_code`；`time_expire` 用北京时间 `yyyy-MM-dd HH:mm:ss`；回调为表单，验签后校验 `app_id`；`TRADE_SUCCESS`、`TRADE_FINISHED` 为已支付；`ACQ.TRADE_NOT_EXIST` 视为未支付 / 已关闭；回调成功回复 `success`，失败回复 `fail`。SDK 的业务失败可能出现在响应体（`IsFailure()`）或作为 `*alipay.Error` 返回，两种都要处理。

- [ ] **Step 1: 添加依赖**

Run: `go get github.com/wechatpay-apiv3/wechatpay-go@v0.2.21 github.com/smartwalle/alipay/v3@v3.2.31 github.com/smartwalle/nsign@v1.0.9`
Expected: `go.mod` 出现三条依赖。

- [ ] **Step 2: 写金额换算的失败测试**

`internal/plugins/pay/amount_test.go`：

```go
package pay

import "testing"

func TestYuanToFen(t *testing.T) {
	for s, want := range map[string]int64{
		"100": 10000, "0.1": 10, "0.01": 1, "1.00": 100, "12.5": 1250, "1000000.00": 100_000_000, "0": 0,
	} {
		got, err := yuanToFen(s)
		if err != nil || got != want {
			t.Fatalf("yuanToFen(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "1.005", "-1.00", "1,00", ".5", "1.", "+1", " 1.00", "1e3", "12345678901.00"} {
		if got, err := yuanToFen(bad); err == nil {
			t.Fatalf("yuanToFen(%q) = %d, want error", bad, got)
		}
	}
}

func TestFenToYuan(t *testing.T) {
	for fen, want := range map[int64]string{1: "0.01", 10: "0.10", 5000: "50.00", 100_000_000: "1000000.00"} {
		if got := fenToYuan(fen); got != want {
			t.Fatalf("fenToYuan(%d) = %q, want %q", fen, got, want)
		}
	}
}
```

`internal/plugins/pay/amount.go`：

```go
package pay

import (
	"fmt"
	"regexp"
	"strconv"
)

// 支付宝金额为「元」字符串，最多两位小数；用整数解析，不经过浮点。
var yuanRe = regexp.MustCompile(`^(\d{1,10})(?:\.(\d{1,2}))?$`)

func yuanToFen(s string) (int64, error) {
	m := yuanRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("pay: 金额格式不正确 %q", s)
	}
	yuan, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pay: 金额格式不正确 %q", s)
	}
	frac := m[2]
	for len(frac) < 2 {
		frac += "0"
	}
	fen, _ := strconv.ParseInt(frac, 10, 64)
	return yuan*100 + fen, nil
}

func fenToYuan(fen int64) string { return fmt.Sprintf("%d.%02d", fen/100, fen%100) }
```

Run: `go test ./internal/plugins/pay/ -run 'YuanToFen|FenToYuan'`
Expected: PASS

- [ ] **Step 3: 写微信插件的失败测试**

`internal/plugins/pay/wechat_test.go`：

```go
package pay

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/native"
)

const testV3Key = "0123456789abcdef0123456789abcdef"

type fakeWechatAPI struct {
	prepay    native.PrepayRequest
	codeURL   string
	prepayErr error
	queryTx   *payments.Transaction
	queryErr  error
	closeErr  error
	closed    []string
}

func (f *fakeWechatAPI) Prepay(_ context.Context, req native.PrepayRequest) (*native.PrepayResponse, *core.APIResult, error) {
	f.prepay = req
	if f.prepayErr != nil {
		return nil, nil, f.prepayErr
	}
	return &native.PrepayResponse{CodeUrl: core.String(f.codeURL)}, nil, nil
}

func (f *fakeWechatAPI) QueryOrderByOutTradeNo(context.Context, native.QueryOrderByOutTradeNoRequest) (*payments.Transaction, *core.APIResult, error) {
	return f.queryTx, nil, f.queryErr
}

func (f *fakeWechatAPI) CloseOrder(_ context.Context, req native.CloseOrderRequest) (*core.APIResult, error) {
	if f.closeErr != nil {
		return nil, f.closeErr
	}
	f.closed = append(f.closed, *req.OutTradeNo)
	return nil, nil
}

func newTestWechat(t *testing.T) (*Wechat, *rsa.PrivateKey, *fakeWechatAPI) {
	t.Helper()
	platformKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	merchantKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeWechatAPI{}
	w, err := newWechat(WechatConfig{MchID: "1900000001", AppID: "wx123", APIv3Key: testV3Key, CertSerial: "MCHSERIAL",
		PrivateKey: merchantKey, PublicKeyID: "PUB_KEY_ID_1", PublicKey: &platformKey.PublicKey}, fake)
	if err != nil {
		t.Fatal(err)
	}
	return w, platformKey, fake
}

// signedNotify 按微信支付 APIv3 规则加密 resource 并用平台私钥签名，得到一个回调请求。
func signedNotify(t *testing.T, key *rsa.PrivateKey, serial string, tx map[string]any) *http.Request {
	t.Helper()
	plain, _ := json.Marshal(tx)
	block, _ := aes.NewCipher([]byte(testV3Key))
	gcm, _ := cipher.NewGCM(block)
	nonce := "abcdefghijkl"
	ct := gcm.Seal(nil, []byte(nonce), plain, []byte("transaction"))
	body, _ := json.Marshal(map[string]any{
		"id": "EV-1", "event_type": "TRANSACTION.SUCCESS", "resource_type": "encrypt-resource", "summary": "支付成功",
		"resource": map[string]string{"algorithm": "AEAD_AES_256_GCM", "ciphertext": base64.StdEncoding.EncodeToString(ct),
			"associated_data": "transaction", "nonce": nonce, "original_type": "transaction"},
	})
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sum := sha256.Sum256([]byte(ts + "\nNONCE123\n" + string(body) + "\n"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/pay/notify/wechat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Wechatpay-Timestamp", ts)
	req.Header.Set("Wechatpay-Nonce", "NONCE123")
	req.Header.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))
	req.Header.Set("Wechatpay-Serial", serial)
	return req
}

func successTx(state string) map[string]any {
	return map[string]any{"mchid": "1900000001", "appid": "wx123", "out_trade_no": "rch-abc", "transaction_id": "4200001",
		"trade_state": state, "amount": map[string]any{"total": 5000, "currency": "CNY"}}
}

func TestWechatCreate(t *testing.T) {
	w, _, fake := newTestWechat(t)
	fake.codeURL = "weixin://wxpay/bizpayurl?pr=abc"
	exp := time.Date(2026, 1, 1, 0, 15, 0, 0, time.UTC)
	qr, err := w.Create(context.Background(), Recharge{ID: "rch-abc", Amount: 5000, Subject: "充值", ExpiresAt: exp, NotifyURL: "https://spark.test/api/v1/pay/notify/wechat"})
	if err != nil || qr != fake.codeURL {
		t.Fatalf("create = %q, %v", qr, err)
	}
	p := fake.prepay
	if *p.Appid != "wx123" || *p.Mchid != "1900000001" || *p.OutTradeNo != "rch-abc" || *p.Description != "充值" ||
		*p.Amount.Total != 5000 || *p.Amount.Currency != "CNY" || *p.NotifyUrl != "https://spark.test/api/v1/pay/notify/wechat" || !p.TimeExpire.Equal(exp) {
		t.Fatalf("prepay = %+v", p)
	}
	fake.codeURL = ""
	if _, err := w.Create(context.Background(), Recharge{ID: "rch-x", Amount: 1}); err == nil {
		t.Fatal("empty code_url must fail")
	}
	fake.prepayErr = errors.New("network down")
	if _, err := w.Create(context.Background(), Recharge{ID: "rch-y", Amount: 1}); err == nil {
		t.Fatal("prepay error must propagate")
	}
}

func TestWechatParseNotify(t *testing.T) {
	w, key, _ := newTestWechat(t)
	n, err := w.ParseNotify(signedNotify(t, key, "PUB_KEY_ID_1", successTx("SUCCESS")))
	if err != nil || n != (Notification{OutTradeNo: "rch-abc", ChannelTradeNo: "4200001", Amount: 5000, Paid: true}) {
		t.Fatalf("notify = %+v, %v", n, err)
	}
	if n, err := w.ParseNotify(signedNotify(t, key, "PUB_KEY_ID_1", successTx("NOTPAY"))); err != nil || n.Paid {
		t.Fatalf("NOTPAY = %+v, %v", n, err)
	}
	if _, err := w.ParseNotify(signedNotify(t, key, "OTHER_KEY", successTx("SUCCESS"))); err == nil {
		t.Fatal("wrong serial accepted")
	}
	forger, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := w.ParseNotify(signedNotify(t, forger, "PUB_KEY_ID_1", successTx("SUCCESS"))); err == nil {
		t.Fatal("forged signature accepted")
	}
	other := successTx("SUCCESS")
	other["mchid"] = "1900000999"
	if _, err := w.ParseNotify(signedNotify(t, key, "PUB_KEY_ID_1", other)); err == nil {
		t.Fatal("notification for another merchant accepted")
	}
}

func TestWechatQueryAndClose(t *testing.T) {
	w, _, fake := newTestWechat(t)
	ctx := context.Background()
	fake.queryErr = &core.APIError{StatusCode: 404, Code: "ORDER_NOT_EXIST", Message: "订单不存在"}
	if n, err := w.Query(ctx, "rch-abc"); err != nil || n.Paid || n.OutTradeNo != "rch-abc" {
		t.Fatalf("not exist = %+v, %v", n, err)
	}
	fake.queryErr = errors.New("timeout")
	if _, err := w.Query(ctx, "rch-abc"); err == nil {
		t.Fatal("query error must propagate")
	}
	fake.queryErr = nil
	fake.queryTx = &payments.Transaction{Mchid: core.String("1900000001"), OutTradeNo: core.String("rch-abc"),
		TransactionId: core.String("4200001"), TradeState: core.String("SUCCESS"), Amount: &payments.TransactionAmount{Total: core.Int64(5000)}}
	if n, err := w.Query(ctx, "rch-abc"); err != nil || !n.Paid || n.Amount != 5000 {
		t.Fatalf("paid = %+v, %v", n, err)
	}
	if err := w.Close(ctx, "rch-abc"); err != nil || len(fake.closed) != 1 {
		t.Fatalf("close = %v %v", err, fake.closed)
	}
	fake.closeErr = &core.APIError{StatusCode: 404, Code: "ORDER_NOT_EXIST"}
	if err := w.Close(ctx, "rch-gone"); err != nil {
		t.Fatalf("close missing = %v", err)
	}
	fake.closeErr = errors.New("timeout")
	if err := w.Close(ctx, "rch-abc"); err == nil {
		t.Fatal("close error must propagate")
	}
}

func TestWechatNotifyAck(t *testing.T) {
	w, _, _ := newTestWechat(t)
	rec := httptest.NewRecorder()
	w.NotifyAck(rec, true)
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("ok ack = %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	w.NotifyAck(rec, false)
	if rec.Code != 500 || rec.Body.String() != `{"code":"FAIL","message":"失败"}` {
		t.Fatalf("fail ack = %d %q", rec.Code, rec.Body.String())
	}
}
```

Run: `go test ./internal/plugins/pay/ -run Wechat`
Expected: 编译失败（`newWechat`、`Wechat` 未定义）。

- [ ] **Step 4: 实现微信插件**

`internal/plugins/pay/wechat.go`：

```go
package pay

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/wechatpay-apiv3/wechatpay-go/core"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/verifiers"
	"github.com/wechatpay-apiv3/wechatpay-go/core/notify"
	"github.com/wechatpay-apiv3/wechatpay-go/core/option"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments/native"
)

// WechatConfig 微信支付 APIv3 商户配置（使用「微信支付公钥」验签，启动时无需联网下载平台证书）。
type WechatConfig struct {
	MchID       string
	AppID       string
	APIv3Key    string
	CertSerial  string          // 商户 API 证书序列号
	PrivateKey  *rsa.PrivateKey // 商户 API 私钥
	PublicKeyID string          // 微信支付公钥 ID
	PublicKey   *rsa.PublicKey  // 微信支付公钥
}

// wechatAPI 是 Native 支付用到的 SDK 方法，测试时替换为假实现。
type wechatAPI interface {
	Prepay(ctx context.Context, req native.PrepayRequest) (*native.PrepayResponse, *core.APIResult, error)
	QueryOrderByOutTradeNo(ctx context.Context, req native.QueryOrderByOutTradeNoRequest) (*payments.Transaction, *core.APIResult, error)
	CloseOrder(ctx context.Context, req native.CloseOrderRequest) (*core.APIResult, error)
}

// Wechat 是微信支付 Native 扫码渠道。
type Wechat struct {
	cfg    WechatConfig
	api    wechatAPI
	notify *notify.Handler
}

func NewWechat(ctx context.Context, cfg WechatConfig) (*Wechat, error) {
	client, err := core.NewClient(ctx, option.WithWechatPayPublicKeyAuthCipher(
		cfg.MchID, cfg.CertSerial, cfg.PrivateKey, cfg.PublicKeyID, cfg.PublicKey))
	if err != nil {
		return nil, fmt.Errorf("wechat pay client: %w", err)
	}
	return newWechat(cfg, &native.NativeApiService{Client: client})
}

func newWechat(cfg WechatConfig, api wechatAPI) (*Wechat, error) {
	if cfg.PublicKey == nil || cfg.PrivateKey == nil {
		return nil, errors.New("wechat pay: 缺少商户私钥或微信支付公钥")
	}
	h, err := notify.NewRSANotifyHandler(cfg.APIv3Key, verifiers.NewSHA256WithRSAPubkeyVerifier(cfg.PublicKeyID, *cfg.PublicKey))
	if err != nil {
		return nil, fmt.Errorf("wechat pay notify handler: %w", err)
	}
	return &Wechat{cfg: cfg, api: api, notify: h}, nil
}

func (w *Wechat) Create(ctx context.Context, r Recharge) (string, error) {
	resp, _, err := w.api.Prepay(ctx, native.PrepayRequest{
		Appid:       core.String(w.cfg.AppID),
		Mchid:       core.String(w.cfg.MchID),
		Description: core.String(r.Subject),
		OutTradeNo:  core.String(r.ID),
		TimeExpire:  core.Time(r.ExpiresAt),
		NotifyUrl:   core.String(r.NotifyURL),
		Amount:      &native.Amount{Total: core.Int64(r.Amount), Currency: core.String("CNY")},
	})
	if err != nil {
		return "", fmt.Errorf("wechat prepay: %w", err)
	}
	if resp == nil || resp.CodeUrl == nil || *resp.CodeUrl == "" {
		return "", errors.New("wechat prepay: 响应缺少 code_url")
	}
	return *resp.CodeUrl, nil
}

func (w *Wechat) ParseNotify(req *http.Request) (Notification, error) {
	var tx payments.Transaction
	if _, err := w.notify.ParseNotifyRequest(req.Context(), req, &tx); err != nil {
		return Notification{}, fmt.Errorf("wechat notify: %w", err)
	}
	return w.toNotification(&tx)
}

func (w *Wechat) toNotification(tx *payments.Transaction) (Notification, error) {
	if tx == nil || tx.OutTradeNo == nil {
		return Notification{}, errors.New("wechat: 交易缺少 out_trade_no")
	}
	if tx.Mchid != nil && *tx.Mchid != w.cfg.MchID {
		return Notification{}, fmt.Errorf("wechat: 商户号不符 %s", *tx.Mchid)
	}
	n := Notification{OutTradeNo: *tx.OutTradeNo, Paid: tx.TradeState != nil && *tx.TradeState == "SUCCESS"}
	if tx.TransactionId != nil {
		n.ChannelTradeNo = *tx.TransactionId
	}
	if tx.Amount != nil && tx.Amount.Total != nil {
		n.Amount = *tx.Amount.Total
	}
	return n, nil
}

func (w *Wechat) Query(ctx context.Context, outTradeNo string) (Notification, error) {
	tx, _, err := w.api.QueryOrderByOutTradeNo(ctx, native.QueryOrderByOutTradeNoRequest{
		OutTradeNo: core.String(outTradeNo), Mchid: core.String(w.cfg.MchID)})
	if core.IsAPIError(err, "ORDER_NOT_EXIST") {
		return Notification{OutTradeNo: outTradeNo}, nil
	}
	if err != nil {
		return Notification{}, fmt.Errorf("wechat query: %w", err)
	}
	return w.toNotification(tx)
}

func (w *Wechat) Close(ctx context.Context, outTradeNo string) error {
	_, err := w.api.CloseOrder(ctx, native.CloseOrderRequest{OutTradeNo: core.String(outTradeNo), Mchid: core.String(w.cfg.MchID)})
	if err != nil && !core.IsAPIError(err, "ORDER_NOT_EXIST") {
		return fmt.Errorf("wechat close: %w", err)
	}
	return nil
}

// NotifyAck 成功回复 200 空响应；失败回复 5xx 与 FAIL，微信会按策略重试。
func (w *Wechat) NotifyAck(rw http.ResponseWriter, ok bool) {
	if ok {
		rw.WriteHeader(http.StatusOK)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(rw, `{"code":"FAIL","message":"失败"}`)
}
```

Run: `go test ./internal/plugins/pay/ -run Wechat`
Expected: PASS

- [ ] **Step 5: 写支付宝插件的失败测试**

`internal/plugins/pay/alipay_test.go`：

```go
package pay

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/smartwalle/alipay/v3"
	"github.com/smartwalle/nsign"
)

const testAlipayAppID = "2021000000000001"

func pemPrivate(k *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func pemPublic(t *testing.T, k *rsa.PublicKey) string {
	b, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b}))
}

// fakeAlipayAPI 用真实客户端验签回调，其余接口返回预设结果。
type fakeAlipayAPI struct {
	*alipay.Client
	precreate alipay.TradePreCreate
	preRsp    *alipay.TradePreCreateRsp
	preErr    error
	queryRsp  *alipay.TradeQueryRsp
	queryErr  error
	closeRsp  *alipay.TradeCloseRsp
	closeErr  error
}

func (f *fakeAlipayAPI) TradePreCreate(_ context.Context, p alipay.TradePreCreate) (*alipay.TradePreCreateRsp, error) {
	f.precreate = p
	return f.preRsp, f.preErr
}

func (f *fakeAlipayAPI) TradeQuery(context.Context, alipay.TradeQuery) (*alipay.TradeQueryRsp, error) {
	return f.queryRsp, f.queryErr
}

func (f *fakeAlipayAPI) TradeClose(context.Context, alipay.TradeClose) (*alipay.TradeCloseRsp, error) {
	return f.closeRsp, f.closeErr
}

// newTestAlipay 返回插件、假接口，以及模拟支付宝签名回调的客户端（用「支付宝私钥」签名）。
func newTestAlipay(t *testing.T) (*Alipay, *fakeAlipayAPI, *alipay.Client) {
	t.Helper()
	appKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	aliKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	a, err := NewAlipay(AlipayConfig{AppID: testAlipayAppID, PrivateKeyPEM: pemPrivate(appKey),
		AlipayPublicKeyPEM: pemPublic(t, &aliKey.PublicKey), Production: false})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeAlipayAPI{Client: a.api.(*alipay.Client)}
	a.api = fake
	signer, err := alipay.New(testAlipayAppID, pemPrivate(aliKey), false)
	if err != nil {
		t.Fatal(err)
	}
	return a, fake, signer
}

func signedAlipayNotify(t *testing.T, signer *alipay.Client, v url.Values) *http.Request {
	t.Helper()
	sig, err := signer.SignValues(v, nsign.WithIgnore("sign", "sign_type"))
	if err != nil {
		t.Fatal(err)
	}
	v.Set("sign", base64.StdEncoding.EncodeToString(sig))
	v.Set("sign_type", "RSA2")
	req := httptest.NewRequest("POST", "/api/v1/pay/notify/alipay", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func notifyValues(status, amount string) url.Values {
	return url.Values{"app_id": {testAlipayAppID}, "out_trade_no": {"rch-abc"}, "trade_no": {"2026010122001"},
		"trade_status": {status}, "total_amount": {amount}, "notify_id": {"n1"}}
}

func TestAlipayCreate(t *testing.T) {
	a, fake, _ := newTestAlipay(t)
	fake.preRsp = &alipay.TradePreCreateRsp{Error: alipay.Error{Code: "10000"}, QRCode: "https://qr.alipay.com/abc"}
	exp := time.Date(2026, 1, 1, 0, 15, 0, 0, time.UTC)
	qr, err := a.Create(context.Background(), Recharge{ID: "rch-abc", Amount: 5000, Subject: "充值", ExpiresAt: exp, NotifyURL: "https://spark.test/n"})
	if err != nil || qr != "https://qr.alipay.com/abc" {
		t.Fatalf("create = %q, %v", qr, err)
	}
	p := fake.precreate
	if p.OutTradeNo != "rch-abc" || p.TotalAmount != "50.00" || p.Subject != "充值" || p.NotifyURL != "https://spark.test/n" ||
		p.TimeExpire != "2026-01-01 08:15:00" {
		t.Fatalf("precreate = %+v", p)
	}
	fake.preRsp = &alipay.TradePreCreateRsp{Error: alipay.Error{Code: "40004", SubCode: "ACQ.INVALID_PARAMETER", SubMsg: "参数无效"}}
	if _, err := a.Create(context.Background(), Recharge{ID: "rch-x", Amount: 1, ExpiresAt: exp}); err == nil || !strings.Contains(err.Error(), "ACQ.INVALID_PARAMETER") {
		t.Fatalf("business failure = %v", err)
	}
	fake.preRsp, fake.preErr = nil, &alipay.Error{Code: "40002", SubCode: "isv.invalid-app-id"}
	if _, err := a.Create(context.Background(), Recharge{ID: "rch-y", Amount: 1, ExpiresAt: exp}); err == nil {
		t.Fatal("SDK error must propagate")
	}
}

func TestAlipayParseNotify(t *testing.T) {
	a, _, signer := newTestAlipay(t)
	n, err := a.ParseNotify(signedAlipayNotify(t, signer, notifyValues("TRADE_SUCCESS", "50.00")))
	if err != nil || n != (Notification{OutTradeNo: "rch-abc", ChannelTradeNo: "2026010122001", Amount: 5000, Paid: true}) {
		t.Fatalf("notify = %+v, %v", n, err)
	}
	if n, err := a.ParseNotify(signedAlipayNotify(t, signer, notifyValues("TRADE_FINISHED", "0.1"))); err != nil || !n.Paid || n.Amount != 10 {
		t.Fatalf("finished = %+v, %v", n, err)
	}
	if n, err := a.ParseNotify(signedAlipayNotify(t, signer, notifyValues("WAIT_BUYER_PAY", "50.00"))); err != nil || n.Paid {
		t.Fatalf("waiting = %+v, %v", n, err)
	}
	other := notifyValues("TRADE_SUCCESS", "50.00")
	other.Set("app_id", "2021999999999999")
	if _, err := a.ParseNotify(signedAlipayNotify(t, signer, other)); err == nil {
		t.Fatal("notification for another app accepted")
	}
	if _, err := a.ParseNotify(signedAlipayNotify(t, signer, notifyValues("TRADE_SUCCESS", "abc"))); err == nil {
		t.Fatal("malformed amount accepted")
	}
	tampered := signedAlipayNotify(t, signer, notifyValues("TRADE_SUCCESS", "50.00"))
	body := strings.Replace(mustRead(t, tampered), "total_amount=50.00", "total_amount=5000.00", 1)
	forged := httptest.NewRequest("POST", "/api/v1/pay/notify/alipay", strings.NewReader(body))
	forged.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := a.ParseNotify(forged); err == nil {
		t.Fatal("tampered amount accepted")
	}
}

func mustRead(t *testing.T, r *http.Request) string {
	t.Helper()
	b := new(strings.Builder)
	if _, err := io.Copy(b, r.Body); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestAlipayQueryAndClose(t *testing.T) {
	a, fake, _ := newTestAlipay(t)
	ctx := context.Background()
	fake.queryErr = &alipay.Error{Code: "40004", SubCode: "ACQ.TRADE_NOT_EXIST"}
	if n, err := a.Query(ctx, "rch-abc"); err != nil || n.Paid || n.OutTradeNo != "rch-abc" {
		t.Fatalf("not exist (error) = %+v, %v", n, err)
	}
	fake.queryErr = nil
	fake.queryRsp = &alipay.TradeQueryRsp{Error: alipay.Error{Code: "40004", SubCode: "ACQ.TRADE_NOT_EXIST"}}
	if n, err := a.Query(ctx, "rch-abc"); err != nil || n.Paid {
		t.Fatalf("not exist (body) = %+v, %v", n, err)
	}
	fake.queryRsp = &alipay.TradeQueryRsp{Error: alipay.Error{Code: "10000"}, OutTradeNo: "rch-abc", TradeNo: "T9",
		TradeStatus: alipay.TradeStatusSuccess, TotalAmount: "8.00"}
	if n, err := a.Query(ctx, "rch-abc"); err != nil || !n.Paid || n.Amount != 800 || n.ChannelTradeNo != "T9" {
		t.Fatalf("paid = %+v, %v", n, err)
	}
	fake.queryRsp = &alipay.TradeQueryRsp{Error: alipay.Error{Code: "20000", SubCode: "aop.ACQ.SYSTEM_ERROR"}}
	if _, err := a.Query(ctx, "rch-abc"); err == nil {
		t.Fatal("system error must propagate")
	}
	fake.closeRsp, fake.closeErr = nil, &alipay.Error{Code: "40004", SubCode: "ACQ.TRADE_NOT_EXIST"}
	if err := a.Close(ctx, "rch-abc"); err != nil {
		t.Fatalf("close missing = %v", err)
	}
	fake.closeRsp, fake.closeErr = &alipay.TradeCloseRsp{Error: alipay.Error{Code: "10000"}}, nil
	if err := a.Close(ctx, "rch-abc"); err != nil {
		t.Fatalf("close = %v", err)
	}
	fake.closeRsp = &alipay.TradeCloseRsp{Error: alipay.Error{Code: "40004", SubCode: "ACQ.REASON_TRADE_STATUS_INVALID"}}
	if err := a.Close(ctx, "rch-abc"); err == nil {
		t.Fatal("close failure must propagate")
	}
}

func TestAlipayNotifyAck(t *testing.T) {
	a, _, _ := newTestAlipay(t)
	rec := httptest.NewRecorder()
	a.NotifyAck(rec, true)
	if rec.Code != 200 || rec.Body.String() != "success" {
		t.Fatalf("ok = %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	a.NotifyAck(rec, false)
	if rec.Body.String() != "fail" {
		t.Fatalf("fail = %d %q", rec.Code, rec.Body.String())
	}
}
```

（该测试文件还需 import `"io"`，用于 `mustRead`。）

Run: `go test ./internal/plugins/pay/ -run Alipay`
Expected: 编译失败（`NewAlipay`、`Alipay` 未定义）。

- [ ] **Step 6: 实现支付宝插件**

`internal/plugins/pay/alipay.go`：

```go
package pay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/smartwalle/alipay/v3"
)

// AlipayConfig 支付宝开放平台应用配置（公钥模式）。
type AlipayConfig struct {
	AppID              string
	PrivateKeyPEM      string // 应用私钥
	AlipayPublicKeyPEM string // 支付宝公钥
	Production         bool   // false 时使用沙箱网关
}

// alipayAPI 是当面付用到的 SDK 方法，测试时替换为假实现。
type alipayAPI interface {
	TradePreCreate(ctx context.Context, p alipay.TradePreCreate) (*alipay.TradePreCreateRsp, error)
	TradeQuery(ctx context.Context, p alipay.TradeQuery) (*alipay.TradeQueryRsp, error)
	TradeClose(ctx context.Context, p alipay.TradeClose) (*alipay.TradeCloseRsp, error)
	DecodeNotification(ctx context.Context, values url.Values) (*alipay.Notification, error)
}

// Alipay 是支付宝当面付扫码渠道。
type Alipay struct {
	appID string
	api   alipayAPI
}

var beijing = time.FixedZone("CST", 8*3600)

func NewAlipay(cfg AlipayConfig) (*Alipay, error) {
	c, err := alipay.New(cfg.AppID, cfg.PrivateKeyPEM, cfg.Production)
	if err != nil {
		return nil, fmt.Errorf("alipay client: %w", err)
	}
	if err := c.LoadAliPayPublicKey(cfg.AlipayPublicKeyPEM); err != nil {
		return nil, fmt.Errorf("alipay public key: %w", err)
	}
	return &Alipay{appID: cfg.AppID, api: c}, nil
}

// alipayFailure 统一业务失败：SDK 可能把失败放在响应体里（IsFailure），也可能作为 *alipay.Error 返回。
func alipayFailure(e alipay.Error, err error) error {
	if err != nil {
		return err
	}
	if e.IsFailure() {
		return &e
	}
	return nil
}

func isTradeNotExist(err error) bool {
	var e *alipay.Error
	return errors.As(err, &e) && e.SubCode == "ACQ.TRADE_NOT_EXIST"
}

func (a *Alipay) Create(ctx context.Context, r Recharge) (string, error) {
	p := alipay.TradePreCreate{Trade: alipay.Trade{
		NotifyURL:   r.NotifyURL,
		Subject:     r.Subject,
		OutTradeNo:  r.ID,
		TotalAmount: fenToYuan(r.Amount),
		TimeExpire:  r.ExpiresAt.In(beijing).Format("2006-01-02 15:04:05"),
	}}
	rsp, err := a.api.TradePreCreate(ctx, p)
	var e alipay.Error
	if rsp != nil {
		e = rsp.Error
	}
	if err := alipayFailure(e, err); err != nil {
		return "", fmt.Errorf("alipay precreate: %w", err)
	}
	if rsp == nil || rsp.QRCode == "" {
		return "", errors.New("alipay precreate: 响应缺少 qr_code")
	}
	return rsp.QRCode, nil
}

func (a *Alipay) ParseNotify(req *http.Request) (Notification, error) {
	if err := req.ParseForm(); err != nil {
		return Notification{}, fmt.Errorf("alipay notify: %w", err)
	}
	n, err := a.api.DecodeNotification(req.Context(), req.PostForm)
	if err != nil {
		return Notification{}, fmt.Errorf("alipay notify: %w", err)
	}
	if n.AppId != a.appID {
		return Notification{}, fmt.Errorf("alipay notify: app_id 不符 %s", n.AppId)
	}
	amount, err := yuanToFen(n.TotalAmount)
	if err != nil {
		return Notification{}, err
	}
	return Notification{OutTradeNo: n.OutTradeNo, ChannelTradeNo: n.TradeNo, Amount: amount,
		Paid: n.TradeStatus == alipay.TradeStatusSuccess || n.TradeStatus == alipay.TradeStatusFinished}, nil
}

func (a *Alipay) Query(ctx context.Context, outTradeNo string) (Notification, error) {
	rsp, err := a.api.TradeQuery(ctx, alipay.TradeQuery{OutTradeNo: outTradeNo})
	var e alipay.Error
	if rsp != nil {
		e = rsp.Error
	}
	if err := alipayFailure(e, err); err != nil {
		if isTradeNotExist(err) {
			return Notification{OutTradeNo: outTradeNo}, nil
		}
		return Notification{}, fmt.Errorf("alipay query: %w", err)
	}
	paid := rsp.TradeStatus == alipay.TradeStatusSuccess || rsp.TradeStatus == alipay.TradeStatusFinished
	n := Notification{OutTradeNo: outTradeNo, ChannelTradeNo: rsp.TradeNo, Paid: paid}
	if paid {
		if n.Amount, err = yuanToFen(rsp.TotalAmount); err != nil {
			return Notification{}, err
		}
	}
	return n, nil
}

func (a *Alipay) Close(ctx context.Context, outTradeNo string) error {
	rsp, err := a.api.TradeClose(ctx, alipay.TradeClose{OutTradeNo: outTradeNo})
	var e alipay.Error
	if rsp != nil {
		e = rsp.Error
	}
	if err := alipayFailure(e, err); err != nil && !isTradeNotExist(err) {
		return fmt.Errorf("alipay close: %w", err)
	}
	return nil
}

// NotifyAck 支付宝要求成功时响应体为 success，其他内容都会触发重试。
func (a *Alipay) NotifyAck(w http.ResponseWriter, ok bool) {
	if ok {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "success")
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, "fail")
}
```

Run: `go test ./internal/plugins/pay/`
Expected: PASS

- [ ] **Step 7: 接入 app 并测试**

`internal/app/app.go`：

1. import 增加 `"os"`、`"github.com/wechatpay-apiv3/wechatpay-go/utils"`。
2. 把 Task 9 中 `case "real": return nil, errors.New("真实支付渠道尚未接入")` 改为：

```go
		case "real":
			var err error
			if providers, err = realProviders(cfg); err != nil {
				return nil, err
			}
```

3. 文件末尾加入：

```go
// realProviders 按配置读取密钥文件，创建已启用的真实支付渠道。
func realProviders(cfg config.Config) (map[string]pay.Provider, error) {
	out := map[string]pay.Provider{}
	if w := cfg.Wechat; w.Enabled() {
		priv, err := utils.LoadPrivateKeyWithPath(w.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取微信支付商户私钥: %w", err)
		}
		pub, err := utils.LoadPublicKeyWithPath(w.PublicKeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取微信支付公钥: %w", err)
		}
		p, err := pay.NewWechat(context.Background(), pay.WechatConfig{MchID: w.MchID, AppID: w.AppID, APIv3Key: w.APIv3Key,
			CertSerial: w.CertSerial, PrivateKey: priv, PublicKeyID: w.PublicKeyID, PublicKey: pub})
		if err != nil {
			return nil, err
		}
		out[pay.ChannelWechat] = p
	}
	if a := cfg.Alipay; a.Enabled() {
		priv, err := os.ReadFile(a.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取支付宝应用私钥: %w", err)
		}
		pub, err := os.ReadFile(a.PublicKeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取支付宝公钥: %w", err)
		}
		p, err := pay.NewAlipay(pay.AlipayConfig{AppID: a.AppID, PrivateKeyPEM: string(priv), AlipayPublicKeyPEM: string(pub), Production: !a.Sandbox})
		if err != nil {
			return nil, err
		}
		out[pay.ChannelAlipay] = p
	}
	return out, nil
}
```

在 `internal/app/e2e_test.go` 末尾追加（import 增加 `"crypto/rand"`、`"crypto/rsa"`、`"crypto/x509"`、`"encoding/pem"`、`"os"`、`"path/filepath"`）：

```go
// writeKeys 生成一对测试 RSA 密钥，按 PKCS#8 私钥与 PKIX 公钥写入 PEM 文件。
func writeKeys(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privDER, _ := x509.MarshalPKCS8PrivateKey(k)
	pubDER, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	priv, pub := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+"_pub.pem")
	if err := os.WriteFile(priv, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

func TestBuildWiresRealPayProviders(t *testing.T) {
	dir := t.TempDir()
	wxPriv, wxPub := writeKeys(t, dir, "wechat")
	aliPriv, aliPub := writeKeys(t, dir, "alipay")
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock",
		PayDriver: "real", PublicBaseURL: "https://spark.test", UploadDir: t.TempDir(),
		Wechat: config.WechatConfig{MchID: "1900000001", AppID: "wx123", APIv3Key: "0123456789abcdef0123456789abcdef",
			CertSerial: "SERIAL", PrivateKeyPath: wxPriv, PublicKeyID: "PUB_KEY_ID_1", PublicKeyPath: wxPub},
		Alipay: config.AlipayConfig{AppID: "2021000000000001", PrivateKeyPath: aliPriv, PublicKeyPath: aliPub, Sandbox: true}}
	a, err := app.Build(cfg, dbtest.New(t), slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Billing.Channels(); len(got) != 2 || got[0] != "alipay" || got[1] != "wechat" {
		t.Fatalf("channels = %v", got)
	}
	cfg.Wechat.PrivateKeyPath = filepath.Join(dir, "missing.pem")
	if _, err := app.Build(cfg, dbtest.New(t), slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{}); err == nil {
		t.Fatal("missing key file must fail")
	}
}
```

Run: `go test ./internal/app/`
Expected: PASS

- [ ] **Step 8: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add go.mod go.sum internal/plugins/pay internal/app
git commit -m "feat(pay): WeChat Pay Native (public-key mode) and Alipay face-to-face providers

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 11: PDU 插件、PDU 管理与断电重启

**Files:**
- Create: `internal/plugins/pdu/pdu.go`、`internal/plugins/pdu/snmp.go`、`internal/plugins/pdu/mock.go`
- Test: `internal/plugins/pdu/snmp_test.go`
- Create: `migrations/00013_pdus.sql`
- Create: `internal/fleet/pdus.go`、`internal/fleet/pdus_test.go`
- Modify: `internal/fleet/machines.go`（`Machine`/`NewMachine` 增加 PDU 字段，`Register` 改为可在事务内调用）
- Modify: `internal/platform/db/db.go`（`ConstraintName`）
- Modify: `internal/app/app.go`、`internal/app/e2e_test.go`
- Modify: `go.mod` / `go.sum`（`github.com/gosnmp/gosnmp v1.45.0`）

**Interfaces:**
- Consumes: Task 1 的 `audit.Actor`、`db.IsForeignKeyViolation`；计划 1 的 `secretbox.Box`。
- Produces（Task 16 使用）：
  - `pdu.Driver` 接口：`PowerCycle(ctx, outlet int) error`、`Status(ctx, outlet int) (bool, error)`；常量 `pdu.DefaultOIDPrefix = "1.3.6.1.4.1.318.1.1.4.4.2.1.3"`、`pdu.DefaultCycleValue = 3`、`pdu.DefaultOnValue = 1`。
  - `pdu.SNMPConfig{Address, Community, OIDPrefix string; CycleValue, OnValue int; Timeout time.Duration}`、`pdu.NewSNMP(cfg) *pdu.SNMP`。
  - `pdu.Mock{Fail error}`，方法 `Calls() []string`（记录 `"cycle:<outlet>"`）。
  - `db.ConstraintName(err error) string`。
  - `fleet.Machine` 新字段 `PDUID *int64`（json `pdu_id`）、`PDUOutlet *int`（json `pdu_outlet`）；`fleet.NewMachine` 新字段 `PDUID *int64`、`PDUOutlet *int`。
  - `fleet.PDU{ID, Name, Driver, Address, HasCommunity, OIDPrefix, CycleValue, OnValue, CreatedAt}`、`fleet.PDUInput{Name, Driver, Address, Community, OIDPrefix string; CycleValue, OnValue int}`、`fleet.PDUConn{Driver, Address, Community, OIDPrefix string; CycleValue, OnValue int}`、`type fleet.PDUOpener func(fleet.PDUConn) (pdu.Driver, error)`、`fleet.PDUConfig{Box *secretbox.Box; Open PDUOpener; AllowMock bool}`。
  - `(*fleet.Service).ConfigurePDU(c PDUConfig)`、`ListPDUs(ctx) ([]PDU, error)`、`CreatePDU(ctx, actor, in PDUInput) (PDU, error)`、`UpdatePDU(ctx, actor, id int64, in PDUInput) (PDU, error)`、`PowerCycle(ctx, actor, machineID int64) error`、`PDUIDByName(ctx, q db.Querier, name string) (int64, error)`。
  - 包内 `(*Service).register(ctx, q db.Querier, nm NewMachine) (Machine, error)`（Task 16 的批量导入在事务内复用）。
  - 错误码：`pdu_failed`（502）、`pdu_outlet_taken`（409）、`duplicate_name`（409）。
  - `app.Options.PDU pdu.Driver`（测试注入，所有 PDU 都用它）。

- [ ] **Step 1: 添加依赖并写 SNMP 驱动的失败测试**

Run: `go get github.com/gosnmp/gosnmp@v1.45.0`

`internal/plugins/pdu/snmp_test.go`：

```go
package pdu_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"

	"spark-cloud/internal/plugins/pdu"
)

// agent 是本地 UDP 上的极简 SNMP v2c 代理：只接受指定 community，Get 返回当前值，Set 写入并回显。
type agent struct {
	addr      string
	community string
	mu        sync.Mutex
	values    map[string]int
	sets      []string
}

func startAgent(t *testing.T, community string, values map[string]int) *agent {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	a := &agent{addr: pc.LocalAddr().String(), community: community, values: values}
	go func() {
		buf := make([]byte, 65535)
		dec := &gosnmp.GoSNMP{}
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			p, err := dec.SnmpDecodePacket(buf[:n])
			if err != nil || p.Community != a.community {
				continue // community 错误时不应答，客户端超时
			}
			resp := *p
			resp.PDUType = gosnmp.GetResponse
			var vars []gosnmp.SnmpPDU
			a.mu.Lock()
			for _, v := range p.Variables {
				cur, ok := a.values[v.Name]
				if p.PDUType == gosnmp.SetRequest && ok {
					cur = v.Value.(int)
					a.values[v.Name] = cur
					a.sets = append(a.sets, v.Name)
				}
				if ok {
					vars = append(vars, gosnmp.SnmpPDU{Name: v.Name, Type: gosnmp.Integer, Value: cur})
				} else {
					vars = append(vars, gosnmp.SnmpPDU{Name: v.Name, Type: gosnmp.NoSuchInstance})
				}
			}
			a.mu.Unlock()
			resp.Variables = vars
			if b, err := resp.MarshalMsg(); err == nil {
				_, _ = pc.WriteTo(b, from)
			}
		}
	}()
	return a
}

func TestSNMPPowerCycleAndStatus(t *testing.T) {
	oid := ".1.3.6.1.4.1.318.1.1.4.4.2.1.3.5"
	a := startAgent(t, "private", map[string]int{oid: 1})
	d := pdu.NewSNMP(pdu.SNMPConfig{Address: a.addr, Community: "private", Timeout: 500 * time.Millisecond})
	ctx := context.Background()
	on, err := d.Status(ctx, 5)
	if err != nil || !on {
		t.Fatalf("status = %v, %v", on, err)
	}
	if err := d.PowerCycle(ctx, 5); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	got, sets := a.values[oid], append([]string(nil), a.sets...)
	a.mu.Unlock()
	if got != pdu.DefaultCycleValue || len(sets) != 1 || sets[0] != oid {
		t.Fatalf("value = %d sets = %v", got, sets)
	}
	if on, err := d.Status(ctx, 5); err != nil || on {
		t.Fatalf("status after cycle command = %v, %v (value 3 is not 'on')", on, err)
	}
	if _, err := d.Status(ctx, 9); err == nil {
		t.Fatal("unknown outlet must fail")
	}
	if err := d.PowerCycle(ctx, 0); err == nil {
		t.Fatal("outlet 0 must be rejected")
	}
}

func TestSNMPCustomOIDAndValues(t *testing.T) {
	oid := ".1.3.6.1.4.1.99999.1.2.7"
	a := startAgent(t, "c0mm", map[string]int{oid: 2})
	d := pdu.NewSNMP(pdu.SNMPConfig{Address: a.addr, Community: "c0mm", OIDPrefix: "1.3.6.1.4.1.99999.1.2", CycleValue: 4, OnValue: 2,
		Timeout: 500 * time.Millisecond})
	if on, err := d.Status(context.Background(), 7); err != nil || !on {
		t.Fatalf("status = %v, %v", on, err)
	}
	if err := d.PowerCycle(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.values[oid] != 4 {
		t.Fatalf("value = %d, want 4", a.values[oid])
	}
}

func TestSNMPWrongCommunityTimesOut(t *testing.T) {
	a := startAgent(t, "private", map[string]int{".1.3.6.1.4.1.318.1.1.4.4.2.1.3.1": 1})
	d := pdu.NewSNMP(pdu.SNMPConfig{Address: a.addr, Community: "public", Timeout: 200 * time.Millisecond})
	if err := d.PowerCycle(context.Background(), 1); err == nil {
		t.Fatal("wrong community must fail")
	}
}

func TestMockRecordsCalls(t *testing.T) {
	m := &pdu.Mock{}
	if err := m.PowerCycle(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if calls := m.Calls(); len(calls) != 1 || calls[0] != "cycle:3" {
		t.Fatalf("calls = %v", calls)
	}
	if on, err := m.Status(context.Background(), 3); err != nil || !on {
		t.Fatalf("status = %v, %v", on, err)
	}
}
```

Run: `go test ./internal/plugins/pdu/`
Expected: 编译失败（包不存在）。

- [ ] **Step 2: 实现 PDU 插件**

`internal/plugins/pdu/pdu.go`：

```go
// Package pdu 定义智能 PDU 插件：按插座断电重启与查询通电状态。
// DGX Spark 没有 BMC，断电重启是唯一的远程硬恢复手段，只由管理员手动触发。
package pdu

import "context"

// 默认按 APC PowerNet MIB：sPDUOutletCtl.<插座号>，写 3 为重启，读到 1 为通电。其他品牌在 PDU 配置里改。
const (
	DefaultOIDPrefix  = "1.3.6.1.4.1.318.1.1.4.4.2.1.3"
	DefaultCycleValue = 3
	DefaultOnValue    = 1
)

type Driver interface {
	PowerCycle(ctx context.Context, outlet int) error
	Status(ctx context.Context, outlet int) (on bool, err error)
}
```

`internal/plugins/pdu/snmp.go`：

```go
package pdu

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

type SNMPConfig struct {
	Address    string // host 或 host:port（默认端口 161）
	Community  string // 写 community
	OIDPrefix  string // 插座控制 OID 前缀，完整 OID 为 <前缀>.<插座号>
	CycleValue int    // 写入即断电重启的值
	OnValue    int    // 读到该值表示通电
	Timeout    time.Duration
}

// SNMP 通过 SNMP v2c 控制 PDU 插座。
type SNMP struct {
	cfg SNMPConfig
}

func NewSNMP(cfg SNMPConfig) *SNMP {
	if cfg.OIDPrefix == "" {
		cfg.OIDPrefix = DefaultOIDPrefix
	}
	if cfg.CycleValue == 0 {
		cfg.CycleValue = DefaultCycleValue
	}
	if cfg.OnValue == 0 {
		cfg.OnValue = DefaultOnValue
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	cfg.OIDPrefix = strings.TrimSuffix(cfg.OIDPrefix, ".")
	return &SNMP{cfg: cfg}
}

func (s *SNMP) oid(outlet int) string { return fmt.Sprintf("%s.%d", s.cfg.OIDPrefix, outlet) }

func (s *SNMP) connect(ctx context.Context) (*gosnmp.GoSNMP, error) {
	host, port := s.cfg.Address, uint16(161)
	if h, p, err := net.SplitHostPort(s.cfg.Address); err == nil {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("pdu: 端口不正确 %q", s.cfg.Address)
		}
		host, port = h, uint16(n)
	}
	g := &gosnmp.GoSNMP{Target: host, Port: port, Community: s.cfg.Community, Version: gosnmp.Version2c,
		Timeout: s.cfg.Timeout, Retries: 1, Context: ctx}
	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("pdu: 连接 %s: %w", s.cfg.Address, err)
	}
	return g, nil
}

func (s *SNMP) PowerCycle(ctx context.Context, outlet int) error {
	if outlet < 1 {
		return fmt.Errorf("pdu: 插座号必须大于 0")
	}
	g, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer g.Conn.Close()
	oid := s.oid(outlet)
	res, err := g.Set([]gosnmp.SnmpPDU{{Name: oid, Type: gosnmp.Integer, Value: s.cfg.CycleValue}})
	if err != nil {
		return fmt.Errorf("pdu: set %s: %w", oid, err)
	}
	if res.Error != gosnmp.NoError {
		return fmt.Errorf("pdu: set %s: %s", oid, res.Error)
	}
	return nil
}

func (s *SNMP) Status(ctx context.Context, outlet int) (bool, error) {
	if outlet < 1 {
		return false, fmt.Errorf("pdu: 插座号必须大于 0")
	}
	g, err := s.connect(ctx)
	if err != nil {
		return false, err
	}
	defer g.Conn.Close()
	oid := s.oid(outlet)
	res, err := g.Get([]string{oid})
	if err != nil {
		return false, fmt.Errorf("pdu: get %s: %w", oid, err)
	}
	if res.Error != gosnmp.NoError || len(res.Variables) != 1 {
		return false, fmt.Errorf("pdu: get %s: %s", oid, res.Error)
	}
	v := res.Variables[0]
	if v.Type == gosnmp.NoSuchObject || v.Type == gosnmp.NoSuchInstance {
		return false, fmt.Errorf("pdu: 插座 %d 不存在（%s）", outlet, oid)
	}
	return gosnmp.ToBigInt(v.Value).Int64() == int64(s.cfg.OnValue), nil
}
```

`internal/plugins/pdu/mock.go`：

```go
package pdu

import (
	"context"
	"fmt"
	"sync"
)

// Mock 记录调用，供开发与测试使用；Fail 非空时所有操作返回该错误。
type Mock struct {
	Fail  error
	mu    sync.Mutex
	calls []string
}

func (m *Mock) PowerCycle(_ context.Context, outlet int) error {
	if m.Fail != nil {
		return m.Fail
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, fmt.Sprintf("cycle:%d", outlet))
	return nil
}

func (m *Mock) Status(context.Context, int) (bool, error) {
	if m.Fail != nil {
		return false, m.Fail
	}
	return true, nil
}

func (m *Mock) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}
```

Run: `go test ./internal/plugins/pdu/`
Expected: PASS

- [ ] **Step 3: 写迁移**

`migrations/00013_pdus.sql`：

```sql
-- +goose Up
CREATE TABLE pdus (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    driver        TEXT NOT NULL CHECK (driver IN ('snmp', 'mock')),
    address       TEXT NOT NULL,                        -- host 或 host:port
    community_enc BYTEA,                                -- AES-GCM 加密的 SNMP 写 community
    oid_prefix    TEXT NOT NULL DEFAULT '1.3.6.1.4.1.318.1.1.4.4.2.1.3',
    cycle_value   INT NOT NULL DEFAULT 3,
    on_value      INT NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);

ALTER TABLE machines
    ADD COLUMN pdu_id BIGINT REFERENCES pdus (id),
    ADD COLUMN pdu_outlet INT CHECK (pdu_outlet BETWEEN 1 AND 256),
    ADD CONSTRAINT machines_pdu_pair_chk CHECK ((pdu_id IS NULL) = (pdu_outlet IS NULL));
-- 一个 PDU 插座只能接一台机器
CREATE UNIQUE INDEX machines_pdu_outlet_uniq ON machines (pdu_id, pdu_outlet) WHERE pdu_id IS NOT NULL;

-- +goose Down
DROP INDEX machines_pdu_outlet_uniq;
ALTER TABLE machines DROP CONSTRAINT machines_pdu_pair_chk, DROP COLUMN pdu_outlet, DROP COLUMN pdu_id;
DROP TABLE pdus;
```

- [ ] **Step 4: 写 fleet 的失败测试**

`internal/fleet/pdus_test.go`：

```go
package fleet_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/pdu"
)

var ops = audit.Actor{AdminID: 3, IP: "10.0.0.3"}

func codeOf(err error) string {
	if ae := apperr.As(err); ae != nil {
		return ae.Code
	}
	if err != nil {
		return "raw: " + err.Error()
	}
	return ""
}

// configurePDU 让所有 PDU 都使用同一个 mock 驱动，并记录打开驱动时拿到的连接参数。
func configurePDU(t *testing.T, svc *fleet.Service, allowMock bool) (*pdu.Mock, *[]fleet.PDUConn) {
	t.Helper()
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	m := &pdu.Mock{}
	var conns []fleet.PDUConn
	svc.ConfigurePDU(fleet.PDUConfig{Box: box, AllowMock: allowMock,
		Open: func(c fleet.PDUConn) (pdu.Driver, error) { conns = append(conns, c); return m, nil }})
	return m, &conns
}

func snmpInput(name string) fleet.PDUInput {
	return fleet.PDUInput{Name: name, Driver: "snmp", Address: "10.0.9.1:161", Community: "s3cret-community"}
}

func TestCreatePDUValidatesAndEncrypts(t *testing.T) {
	svc, pool, _ := newEnv(t)
	configurePDU(t, svc, false)
	ctx := context.Background()
	for _, c := range []struct {
		in   fleet.PDUInput
		want string
	}{
		{fleet.PDUInput{Driver: "snmp", Address: "10.0.9.1", Community: "x"}, "bad_request"},
		{fleet.PDUInput{Name: "A", Driver: "ipmi", Address: "10.0.9.1", Community: "x"}, "bad_request"},
		{fleet.PDUInput{Name: "A", Driver: "snmp", Address: "", Community: "x"}, "bad_request"},
		{fleet.PDUInput{Name: "A", Driver: "snmp", Address: "10.0.9.1"}, "bad_request"},
		{fleet.PDUInput{Name: "A", Driver: "snmp", Address: "10.0.9.1", Community: "x", OIDPrefix: "1.3.x"}, "bad_request"},
		{fleet.PDUInput{Name: "A", Driver: "mock", Address: "10.0.9.1"}, "bad_request"}, // 不允许 mock 时
	} {
		if _, err := svc.CreatePDU(ctx, ops, c.in); codeOf(err) != c.want {
			t.Fatalf("CreatePDU(%+v) = %v, want %s", c.in, err, c.want)
		}
	}
	p, err := svc.CreatePDU(ctx, ops, snmpInput("PDU-A01"))
	if err != nil {
		t.Fatal(err)
	}
	if p.OIDPrefix != pdu.DefaultOIDPrefix || p.CycleValue != 3 || p.OnValue != 1 || !p.HasCommunity {
		t.Fatalf("pdu = %+v", p)
	}
	if _, err := svc.CreatePDU(ctx, ops, snmpInput("PDU-A01")); codeOf(err) != "duplicate_name" {
		t.Fatalf("duplicate = %v", err)
	}
	var enc []byte
	_ = pool.QueryRow(ctx, `SELECT community_enc FROM pdus WHERE id = $1`, p.ID).Scan(&enc)
	if len(enc) == 0 || strings.Contains(string(enc), "s3cret-community") {
		t.Fatal("community must be stored encrypted")
	}
	list, err := svc.ListPDUs(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "PDU-A01" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'pdu.create' AND diff::text NOT LIKE '%s3cret%'`).Scan(&n)
	if n != 1 {
		t.Fatalf("pdu.create audit rows without secret = %d", n)
	}
}

func TestUpdatePDUKeepsCommunityWhenEmpty(t *testing.T) {
	svc, pool, _ := newEnv(t)
	_, conns := configurePDU(t, svc, true)
	ctx := context.Background()
	p, _ := svc.CreatePDU(ctx, ops, snmpInput("PDU-A01"))
	one := 1
	m, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: dbtest.ProductID(t, pool), PDUID: &p.ID, PDUOutlet: &one})
	if err != nil {
		t.Fatal(err)
	}
	in := snmpInput("PDU-A01-new")
	in.Community = ""
	in.CycleValue = 4
	if _, err := svc.UpdatePDU(ctx, ops, p.ID, in); err != nil {
		t.Fatal(err)
	}
	if err := svc.PowerCycle(ctx, ops, m.ID); err != nil {
		t.Fatal(err)
	}
	in.Community = "rotated"
	if _, err := svc.UpdatePDU(ctx, ops, p.ID, in); err != nil {
		t.Fatal(err)
	}
	if err := svc.PowerCycle(ctx, ops, m.ID); err != nil {
		t.Fatal(err)
	}
	c := *conns
	if len(c) != 2 || c[0].Community != "s3cret-community" || c[0].CycleValue != 4 || c[1].Community != "rotated" {
		t.Fatalf("conns = %+v", c)
	}
	if _, err := svc.UpdatePDU(ctx, ops, 9999, in); codeOf(err) != "not_found" {
		t.Fatalf("update missing = %v", err)
	}
}

func TestRegisterWithPDU(t *testing.T) {
	svc, pool, _ := newEnv(t)
	configurePDU(t, svc, true)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	p, _ := svc.CreatePDU(ctx, ops, snmpInput("PDU-A01"))
	three, missing := 3, int64(9999)
	if _, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: pid, PDUOutlet: &three}); codeOf(err) != "bad_request" {
		t.Fatalf("outlet without pdu = %v", err)
	}
	if _, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: pid, PDUID: &missing, PDUOutlet: &three}); codeOf(err) != "bad_request" {
		t.Fatalf("unknown pdu = %v", err)
	}
	m, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: pid, PDUID: &p.ID, PDUOutlet: &three})
	if err != nil || m.PDUID == nil || *m.PDUID != p.ID || *m.PDUOutlet != 3 {
		t.Fatalf("machine = %+v, %v", m, err)
	}
	if _, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN2", LanIP: "10.0.1.2", ProductID: pid, PDUID: &p.ID, PDUOutlet: &three}); codeOf(err) != "pdu_outlet_taken" {
		t.Fatalf("taken outlet = %v", err)
	}
}

func TestPowerCycle(t *testing.T) {
	svc, pool, _ := newEnv(t)
	m, _ := configurePDU(t, svc, true)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	p, _ := svc.CreatePDU(ctx, ops, snmpInput("PDU-A01"))
	five := 5
	withPDU, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: pid, PDUID: &p.ID, PDUOutlet: &five})
	bare, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN2", LanIP: "10.0.1.2", ProductID: pid})

	if err := svc.PowerCycle(ctx, ops, bare.ID); codeOf(err) != "bad_request" {
		t.Fatalf("machine without pdu = %v", err)
	}
	if err := svc.PowerCycle(ctx, ops, 9999); codeOf(err) != "not_found" {
		t.Fatalf("unknown machine = %v", err)
	}
	if err := svc.PowerCycle(ctx, ops, withPDU.ID); err != nil {
		t.Fatal(err)
	}
	if calls := m.Calls(); len(calls) != 1 || calls[0] != "cycle:5" {
		t.Fatalf("calls = %v", calls)
	}
	m.Fail = errors.New("timeout")
	if err := svc.PowerCycle(ctx, ops, withPDU.ID); codeOf(err) != "pdu_failed" {
		t.Fatalf("driver failure = %v", err)
	}
	var okN, failN int
	_ = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE diff->>'ok' = 'true'), count(*) FILTER (WHERE diff->>'ok' = 'false')
		FROM audit_logs WHERE action = 'machine.power_cycle' AND actor_id = 3`).Scan(&okN, &failN)
	if okN != 1 || failN != 1 {
		t.Fatalf("audit ok=%d fail=%d", okN, failN)
	}
}

func TestPowerCycleRejectsMockPDUWhenNotAllowed(t *testing.T) {
	svc, pool, _ := newEnv(t)
	configurePDU(t, svc, true)
	ctx := context.Background()
	p, err := svc.CreatePDU(ctx, ops, fleet.PDUInput{Name: "dev", Driver: "mock", Address: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	one := 1
	m, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: dbtest.ProductID(t, pool), PDUID: &p.ID, PDUOutlet: &one})
	configurePDU(t, svc, false) // 换成生产配置
	if err := svc.PowerCycle(ctx, ops, m.ID); codeOf(err) != "bad_request" {
		t.Fatalf("mock pdu in prod = %v", err)
	}
}
```

Run: `go test ./internal/fleet/`
Expected: 编译失败（`ConfigurePDU`、`PDUInput` 等未定义）。

- [ ] **Step 5: 实现**

`internal/platform/db/db.go` 末尾追加：

```go
// ConstraintName 返回违反的约束名（非数据库错误返回空串），用于区分同一张表上的多个约束。
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}
```

`internal/fleet/machines.go`：

1. `Machine` 增加字段（放在 `LastSeenAt` 之后）：

```go
	PDUID       *int64     `json:"pdu_id"`
	PDUOutlet   *int       `json:"pdu_outlet"`
```

2. `NewMachine` 增加字段 `PDUID *int64` 与 `PDUOutlet *int`。
3. `Service` 结构体增加字段 `pdu PDUConfig`。
4. `machineCols` 与 `scanMachine` 改为：

```go
const machineCols = `id, serial, rack, slot, lan_ip, product_id, state, agent_online, last_seen_at, pdu_id, pdu_outlet, created_at`

func scanMachine(row pgx.Row) (Machine, error) {
	var m Machine
	err := row.Scan(&m.ID, &m.Serial, &m.Rack, &m.Slot, &m.LanIP, &m.ProductID, &m.State, &m.AgentOnline, &m.LastSeenAt,
		&m.PDUID, &m.PDUOutlet, &m.CreatedAt)
	if db.IsNoRows(err) {
		return Machine{}, apperr.ErrNotFound
	}
	return m, err
}
```

5. 把 `Register` 替换为：

```go
// Register 登记一台新机器，初始状态为维护中，需管理员确认后上线。
func (s *Service) Register(ctx context.Context, nm NewMachine) (Machine, error) {
	return s.register(ctx, s.pool, nm)
}

// register 校验并插入一台机器；q 可以是事务（批量导入时整批在一个事务内）。
func (s *Service) register(ctx context.Context, q db.Querier, nm NewMachine) (Machine, error) {
	nm.Serial = strings.TrimSpace(nm.Serial)
	if nm.Serial == "" {
		return Machine{}, apperr.BadRequest("请填写序列号")
	}
	if net.ParseIP(nm.LanIP) == nil {
		return Machine{}, apperr.BadRequest("内网 IP 格式不正确")
	}
	if (nm.PDUID == nil) != (nm.PDUOutlet == nil) {
		return Machine{}, apperr.BadRequest("PDU 与插座号需同时填写")
	}
	if nm.PDUOutlet != nil && (*nm.PDUOutlet < 1 || *nm.PDUOutlet > 256) {
		return Machine{}, apperr.BadRequest("PDU 插座号需在 1～256 之间")
	}
	now := s.clock.Now()
	m, err := scanMachine(q.QueryRow(ctx, `INSERT INTO machines (serial, rack, slot, lan_ip, product_id, pdu_id, pdu_outlet, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING `+machineCols,
		nm.Serial, nm.Rack, nm.Slot, nm.LanIP, nm.ProductID, nm.PDUID, nm.PDUOutlet, now))
	switch {
	case db.IsUniqueViolation(err) && db.ConstraintName(err) == "machines_pdu_outlet_uniq":
		return Machine{}, apperr.New("pdu_outlet_taken", "该 PDU 插座已被其他机器使用", 409)
	case db.IsUniqueViolation(err):
		return Machine{}, apperr.New("duplicate_serial", "序列号已存在", 409)
	case db.IsForeignKeyViolation(err) && db.ConstraintName(err) == "machines_pdu_id_fkey":
		return Machine{}, apperr.BadRequest("PDU 不存在")
	case db.IsForeignKeyViolation(err):
		return Machine{}, apperr.BadRequest("商品不存在")
	}
	return m, err
}
```

`internal/fleet/pdus.go`：

```go
package fleet

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/pdu"
)

type PDU struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Driver       string    `json:"driver"`
	Address      string    `json:"address"`
	HasCommunity bool      `json:"has_community"` // community 加密保存，接口只告诉是否已设置
	OIDPrefix    string    `json:"oid_prefix"`
	CycleValue   int       `json:"cycle_value"`
	OnValue      int       `json:"on_value"`
	CreatedAt    time.Time `json:"created_at"`
}

// PDUInput 是新建或修改 PDU 的参数；修改时 Community 为空表示保持原值。
type PDUInput struct {
	Name       string
	Driver     string // snmp / mock
	Address    string
	Community  string
	OIDPrefix  string
	CycleValue int
	OnValue    int
}

// PDUConn 是打开 PDU 驱动所需的参数（含解密后的 community）。
type PDUConn struct {
	Driver     string
	Address    string
	Community  string
	OIDPrefix  string
	CycleValue int
	OnValue    int
}

type PDUOpener func(PDUConn) (pdu.Driver, error)

type PDUConfig struct {
	Box       *secretbox.Box
	Open      PDUOpener
	AllowMock bool // 生产环境为 false：不允许创建或使用 driver=mock 的 PDU
}

// ConfigurePDU 设置 PDU 相关依赖；未配置时 PDU 操作返回错误。
func (s *Service) ConfigurePDU(c PDUConfig) { s.pdu = c }

var (
	oidRe       = regexp.MustCompile(`^\d+(\.\d+)+$`)
	errPDUSetup = fmt.Errorf("fleet: PDU 未配置")
)

const pduCols = `id, name, driver, address, community_enc IS NOT NULL, oid_prefix, cycle_value, on_value, created_at`

func scanPDU(row pgx.Row) (PDU, error) {
	var p PDU
	err := row.Scan(&p.ID, &p.Name, &p.Driver, &p.Address, &p.HasCommunity, &p.OIDPrefix, &p.CycleValue, &p.OnValue, &p.CreatedAt)
	if db.IsNoRows(err) {
		return PDU{}, apperr.ErrNotFound
	}
	return p, err
}

func (s *Service) normalizePDU(in PDUInput, creating bool) (PDUInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Address = strings.TrimSpace(in.Address)
	in.OIDPrefix = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(in.OIDPrefix), "."), ".")
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 64 {
		return in, apperr.BadRequest("请填写 PDU 名称（最多 64 个字符）")
	}
	switch in.Driver {
	case "snmp":
	case "mock":
		if !s.pdu.AllowMock {
			return in, apperr.BadRequest("生产环境不能使用 mock PDU")
		}
	default:
		return in, apperr.BadRequest("PDU 驱动只能是 snmp 或 mock")
	}
	host := in.Address
	if h, _, err := net.SplitHostPort(in.Address); err == nil {
		host = h
	}
	if host == "" || strings.ContainsAny(host, " /") {
		return in, apperr.BadRequest("PDU 地址格式不正确")
	}
	if creating && in.Driver == "snmp" && in.Community == "" {
		return in, apperr.BadRequest("请填写 SNMP community")
	}
	if in.OIDPrefix == "" {
		in.OIDPrefix = pdu.DefaultOIDPrefix
	}
	if !oidRe.MatchString(in.OIDPrefix) {
		return in, apperr.BadRequest("OID 前缀格式不正确")
	}
	if in.CycleValue == 0 {
		in.CycleValue = pdu.DefaultCycleValue
	}
	if in.OnValue == 0 {
		in.OnValue = pdu.DefaultOnValue
	}
	return in, nil
}

func pduDiff(in PDUInput) map[string]any {
	return map[string]any{"name": in.Name, "driver": in.Driver, "address": in.Address, "oid_prefix": in.OIDPrefix,
		"cycle_value": in.CycleValue, "on_value": in.OnValue, "community_changed": in.Community != ""}
}

func (s *Service) ListPDUs(ctx context.Context) ([]PDU, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pduCols+` FROM pdus ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PDU, error) { return scanPDU(r) })
}

func (s *Service) sealCommunity(c string) []byte {
	if c == "" {
		return nil
	}
	return s.pdu.Box.Seal([]byte(c))
}

func (s *Service) CreatePDU(ctx context.Context, actor audit.Actor, in PDUInput) (PDU, error) {
	if s.pdu.Box == nil {
		return PDU{}, errPDUSetup
	}
	in, err := s.normalizePDU(in, true)
	if err != nil {
		return PDU{}, err
	}
	now := s.clock.Now()
	var p PDU
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		p, err = scanPDU(tx.QueryRow(ctx, `INSERT INTO pdus (name, driver, address, community_enc, oid_prefix, cycle_value, on_value, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING `+pduCols,
			in.Name, in.Driver, in.Address, s.sealCommunity(in.Community), in.OIDPrefix, in.CycleValue, in.OnValue, now))
		if db.IsUniqueViolation(err) {
			return apperr.New("duplicate_name", "PDU 名称已存在", 409)
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("pdu.create", "pdu", strconv.FormatInt(p.ID, 10), pduDiff(in)))
	})
	return p, err
}

// UpdatePDU 修改 PDU；Community 为空表示保持原值。
func (s *Service) UpdatePDU(ctx context.Context, actor audit.Actor, id int64, in PDUInput) (PDU, error) {
	if s.pdu.Box == nil {
		return PDU{}, errPDUSetup
	}
	in, err := s.normalizePDU(in, false)
	if err != nil {
		return PDU{}, err
	}
	now := s.clock.Now()
	var p PDU
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		p, err = scanPDU(tx.QueryRow(ctx, `UPDATE pdus SET name = $2, driver = $3, address = $4,
				community_enc = COALESCE($5, community_enc), oid_prefix = $6, cycle_value = $7, on_value = $8, updated_at = $9
			WHERE id = $1 RETURNING `+pduCols,
			id, in.Name, in.Driver, in.Address, s.sealCommunity(in.Community), in.OIDPrefix, in.CycleValue, in.OnValue, now))
		if db.IsUniqueViolation(err) {
			return apperr.New("duplicate_name", "PDU 名称已存在", 409)
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("pdu.update", "pdu", strconv.FormatInt(id, 10), pduDiff(in)))
	})
	return p, err
}

// PDUIDByName 按名称查 PDU（批量导入机器时使用）。
func (s *Service) PDUIDByName(ctx context.Context, q db.Querier, name string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM pdus WHERE name = $1`, strings.TrimSpace(name)).Scan(&id)
	if db.IsNoRows(err) {
		return 0, apperr.ErrNotFound
	}
	return id, err
}

// PowerCycle 对机器所接的 PDU 插座断电重启。只由管理员手动触发；成功与失败都写审计。
func (s *Service) PowerCycle(ctx context.Context, actor audit.Actor, machineID int64) error {
	if s.pdu.Box == nil || s.pdu.Open == nil {
		return errPDUSetup
	}
	var pduID *int64
	var outlet *int
	err := s.pool.QueryRow(ctx, `SELECT pdu_id, pdu_outlet FROM machines WHERE id = $1`, machineID).Scan(&pduID, &outlet)
	if db.IsNoRows(err) {
		return apperr.ErrNotFound
	}
	if err != nil {
		return err
	}
	if pduID == nil || outlet == nil {
		return apperr.BadRequest("该机器未配置 PDU")
	}
	var conn PDUConn
	var enc []byte
	if err := s.pool.QueryRow(ctx, `SELECT driver, address, community_enc, oid_prefix, cycle_value, on_value FROM pdus WHERE id = $1`, *pduID).
		Scan(&conn.Driver, &conn.Address, &enc, &conn.OIDPrefix, &conn.CycleValue, &conn.OnValue); err != nil {
		return err
	}
	if conn.Driver == "mock" && !s.pdu.AllowMock {
		return apperr.BadRequest("生产环境不能使用 mock PDU，请先把该 PDU 改为 snmp")
	}
	if enc != nil {
		plain, err := s.pdu.Box.Open(enc)
		if err != nil {
			return fmt.Errorf("decrypt pdu community: %w", err)
		}
		conn.Community = string(plain)
	}
	drv, err := s.pdu.Open(conn)
	if err == nil {
		err = drv.PowerCycle(ctx, *outlet)
	}
	diff := map[string]any{"pdu_id": *pduID, "outlet": *outlet, "ok": err == nil}
	if err != nil {
		diff["error"] = err.Error()
	}
	if aerr := audit.Record(context.WithoutCancel(ctx), s.pool, s.clock.Now(),
		actor.Entry("machine.power_cycle", "machine", strconv.FormatInt(machineID, 10), diff)); aerr != nil {
		return aerr
	}
	if err != nil {
		return apperr.New("pdu_failed", "PDU 操作失败："+err.Error(), 502)
	}
	return nil
}
```

`internal/app/app.go`：

1. import 增加 `"spark-cloud/internal/plugins/pdu"`。
2. `Options` 增加字段 `PDU pdu.Driver // 测试注入：所有 PDU 都使用该驱动`。
3. 生产环境检查再加一项：`|| (opt.PDU == nil && cfg.PDUDriver != "snmp")`。
4. `f := fleet.NewService(pool, clk)` 之后加入：

```go
	f.ConfigurePDU(fleet.PDUConfig{Box: box, Open: pduOpener(cfg, opt.PDU), AllowMock: cfg.Env != "prod"})
```

5. 文件末尾加入：

```go
// pduOpener 按配置选择 PDU 驱动：mock 模式下所有 PDU 共用一个 mock；snmp 模式下按每台 PDU 记录的驱动选择。
func pduOpener(cfg config.Config, override pdu.Driver) fleet.PDUOpener {
	if override != nil {
		return func(fleet.PDUConn) (pdu.Driver, error) { return override, nil }
	}
	shared := &pdu.Mock{}
	if cfg.PDUDriver == "" || cfg.PDUDriver == "mock" {
		return func(fleet.PDUConn) (pdu.Driver, error) { return shared, nil }
	}
	return func(c fleet.PDUConn) (pdu.Driver, error) {
		switch c.Driver {
		case "snmp":
			return pdu.NewSNMP(pdu.SNMPConfig{Address: c.Address, Community: c.Community, OIDPrefix: c.OIDPrefix,
				CycleValue: c.CycleValue, OnValue: c.OnValue}), nil
		case "mock":
			return shared, nil
		}
		return nil, fmt.Errorf("未知的 PDU 驱动 %q", c.Driver)
	}
}
```

`internal/app/e2e_test.go`：Task 9 新增的 `TestBuildRejectsMockPayInProd` 中，cfg 已是 `PayDriver: "mock"`，无需修改；在文件末尾追加：

```go
func TestBuildRejectsMockPDUInProd(t *testing.T) {
	cfg := config.Config{Env: "prod", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "aliyun", KYCDriver: "aliyun",
		PayDriver: "real", PDUDriver: "mock", UploadDir: t.TempDir()}
	if _, err := app.Build(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{}); err == nil {
		t.Fatal("expected error for mock PDU in prod")
	}
}
```

- [ ] **Step 6: 运行确认通过**

Run: `go test ./internal/fleet/ ./internal/plugins/pdu/ ./internal/app/`
Expected: PASS

- [ ] **Step 7: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS（`rental`、`api` 测试里用到的 `fleet.Machine` 只增加了字段，不受影响）。

```bash
git add go.mod go.sum migrations/00013_pdus.sql internal/plugins/pdu internal/fleet internal/platform/db internal/app
git commit -m "feat(fleet): SNMP/mock PDU drivers, encrypted PDU config and audited manual power-cycle

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 12: 实例售后（一）：免费延长、重新洗机、结束维护前检查

**Files:**
- Create: `internal/rental/admin_ops.go`、`internal/rental/admin_ops_test.go`
- Modify: `internal/rental/renew.go`（抽出共用的 `addTime`）
- Modify: `internal/fleet/machines.go`（新增 `SetMaintenanceIn`）

**Interfaces:**
- Consumes: Task 1 的 `audit.Actor`；计划 1 的 `lockInstance`、`recordEvent`、`reminderFlags`、`refreshLease`、`fleet.HasOpenCommand`、`fleet.Enqueue`、`fleet.CancelCommands`。
- Produces（Task 17 使用）：
  - `rental.MinExtendSeconds = 60`、`rental.MaxExtendSeconds = 30 * 24 * 3600`。
  - `(*rental.Service).AdminExtend(ctx, actor audit.Actor, instanceID string, seconds int64, reason string) (Instance, error)`
  - `(*rental.Service).AdminRewipe(ctx, actor audit.Actor, instanceID string) error`
  - `(*rental.Service).SetMachineMaintenance(ctx, actor audit.Actor, machineID int64, on bool) error`（结束维护前要求机器上没有 creating/running/stopped/wiping 实例，否则 `invalid_state`「该机器上还有未结束的实例，不能结束维护」）
  - `(*fleet.Service).SetMaintenanceIn(ctx, q db.Querier, id int64, on bool) error`（`SetMaintenance` 改为调用它）
  - 包内 `(*Service).addTime(ctx, tx pgx.Tx, inst Instance, secs int64, now time.Time) error`（续费与免费延长共用）。

- [ ] **Step 1: 写失败测试**

`internal/rental/admin_ops_test.go`：

```go
package rental_test

import (
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

var opsActor = audit.Actor{AdminID: 5, IP: "10.0.0.5"}

func codeOf(err error) string {
	if ae := apperr.As(err); ae != nil {
		return ae.Code
	}
	if err != nil {
		return "raw: " + err.Error()
	}
	return ""
}

func (e *env) auditCount(action string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = $1 AND actor_id = $2`, action, opsActor.AdminID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestAdminExtendRunning(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(23*time.Hour + 30*time.Minute) // 剩余 30 分钟
	got, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 2*3600, "机器故障补偿")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.Equal(dbtest.T0.Add(26*time.Hour)) || got.Reminded1h || !got.Reminded24h {
		t.Fatalf("instance = %+v", got)
	}
	if b := e.balance(e.userID); b != startFunds-6900 {
		t.Fatalf("extend must be free, balance = %d", b)
	}
	var orders int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM orders WHERE instance_id = $1`, inst.ID).Scan(&orders)
	if orders != 1 || e.auditCount("instance.extend") != 1 {
		t.Fatalf("orders = %d audit = %d", orders, e.auditCount("instance.extend"))
	}
}

func TestAdminExtendSuspendedAddsToFrozenTime(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.svc.SuspendMachines(e.ctx, e.machines); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 600, "补偿")
	if err != nil || got.Remaining(e.clock.Now()) != 24*time.Hour+10*time.Minute {
		t.Fatalf("remaining = %v, %v", got.Remaining(e.clock.Now()), err)
	}
}

func TestAdminExtendCreatingAddsPending(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	got, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 3600, "补偿")
	if err != nil || got.PendingSeconds != 86400+3600 {
		t.Fatalf("pending = %d, %v", got.PendingSeconds, err)
	}
}

func TestAdminExtendStoppedRestarts(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	expireAndStop(e)
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 3600, "补偿"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 600, "再补偿"); err != nil {
		t.Fatal(err)
	}
	if n := countPending(e, e.machines[0], fleet.CmdStartInstance); n != 1 {
		t.Fatalf("start commands = %d, want 1", n)
	}
	e.ack(true)
	got := e.get(inst.ID)
	if got.State != rental.StateRunning || got.Remaining(e.clock.Now()) != 70*time.Minute {
		t.Fatalf("after restart: state=%s remaining=%v", got.State, got.Remaining(e.clock.Now()))
	}
}

func TestAdminExtendValidation(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	for _, c := range []struct {
		secs   int64
		reason string
	}{{59, "x"}, {rental.MaxExtendSeconds + 1, "x"}, {3600, "  "}} {
		if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, c.secs, c.reason); codeOf(err) != "bad_request" {
			t.Fatalf("extend(%d, %q) = %v", c.secs, c.reason, err)
		}
	}
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, "ins-missing", 3600, "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	e.ack(true)
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 3600, "x"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("wiping = %v", err)
	}
}

func TestAdminRewipeAfterWipeFailure(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if err := e.svc.AdminRewipe(e.ctx, opsActor, inst.ID); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("rewipe running = %v", err)
	}
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	e.ack(false) // 洗机失败：机器隔离为 faulty
	if e.machineState(e.machines[0]) != fleet.StateFaulty || e.get(inst.ID).State != rental.StateWiping {
		t.Fatalf("after wipe failure: machine=%s instance=%s", e.machineState(e.machines[0]), e.get(inst.ID).State)
	}
	// 结束维护前必须先处理完实例
	if err := e.svc.SetMachineMaintenance(e.ctx, opsActor, e.machines[0], false); codeOf(err) != "invalid_state" {
		t.Fatalf("end maintenance with wiping instance = %v", err)
	}
	if err := e.svc.AdminRewipe(e.ctx, opsActor, inst.ID); err != nil {
		t.Fatal(err)
	}
	if n := countPending(e, e.machines[0], fleet.CmdWipeInstance); n != 1 {
		t.Fatalf("wipe commands = %d", n)
	}
	e.ack(true)
	if got := e.get(inst.ID); got.State != rental.StateReleased {
		t.Fatalf("state = %s", got.State)
	}
	if e.machineState(e.machines[0]) != fleet.StateFaulty {
		t.Fatal("machine must stay faulty until ops ends maintenance")
	}
	if err := e.svc.SetMachineMaintenance(e.ctx, opsActor, e.machines[0], false); err != nil {
		t.Fatal(err)
	}
	if e.machineState(e.machines[0]) != fleet.StateIdle || e.auditCount("instance.rewipe") != 1 || e.auditCount("machine.maintenance") != 1 {
		t.Fatalf("machine=%s rewipe audit=%d maintenance audit=%d", e.machineState(e.machines[0]),
			e.auditCount("instance.rewipe"), e.auditCount("machine.maintenance"))
	}
}

func TestSetMachineMaintenanceOn(t *testing.T) {
	e := newEnv(t, 1)
	if err := e.svc.SetMachineMaintenance(e.ctx, opsActor, e.machines[0], true); err != nil {
		t.Fatal(err)
	}
	if e.machineState(e.machines[0]) != fleet.StateMaintenance {
		t.Fatal("not in maintenance")
	}
	if err := e.svc.SetMachineMaintenance(e.ctx, opsActor, 9999, true); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown machine = %v", err)
	}
}
```

Run: `go test ./internal/rental/ -run 'AdminExtend|AdminRewipe|SetMachineMaintenance'`
Expected: 编译失败（`AdminExtend` 等未定义）。

- [ ] **Step 2: 抽出 addTime 并让续费使用**

`internal/rental/renew.go`：在 `Renew` 中把从 `secs := seconds(d)` 开始、到 `if err != nil { return err }`（紧接 switch 之后的那一个）为止的整段替换为：

```go
		if err := s.addTime(ctx, tx, inst, seconds(d), now); err != nil {
			return err
		}
```

并在文件末尾加入：

```go
// addTime 按实例当前状态增加可用时长（续费与后台免费延长共用）：
// 运行中延长到期时间（已到期未清扫的从当前时刻起算；暂停中加到冻结的剩余时长）并重算提醒标记；
// 开通中或停机时计入待开始时长，停机且没有未完成的启动指令时下发启动。
func (s *Service) addTime(ctx context.Context, tx pgx.Tx, inst Instance, secs int64, now time.Time) error {
	var err error
	switch {
	case inst.State == StateCreating:
		_, err = tx.Exec(ctx, `UPDATE instances SET pending_seconds = pending_seconds + $2, updated_at = $3 WHERE id = $1`, inst.ID, secs, now)
	case inst.State == StateRunning && inst.Suspended():
		var paused int64
		if inst.PausedRemaining != nil {
			paused = *inst.PausedRemaining
		}
		r24, r1 := reminderFlags(time.Duration(paused+secs) * time.Second)
		_, err = tx.Exec(ctx, `UPDATE instances SET paused_remaining = $2, reminded_24h = $3, reminded_1h = $4, updated_at = $5 WHERE id = $1`,
			inst.ID, paused+secs, r24, r1, now)
	case inst.State == StateRunning:
		base := *inst.ExpiresAt
		if base.Before(now) {
			base = now
		}
		expires := base.Add(time.Duration(secs) * time.Second)
		r24, r1 := reminderFlags(expires.Sub(now))
		_, err = tx.Exec(ctx, `UPDATE instances SET expires_at = $2, reminded_24h = $3, reminded_1h = $4, updated_at = $5 WHERE id = $1`,
			inst.ID, expires, r24, r1, now)
	case inst.State == StateStopped:
		if _, err = tx.Exec(ctx, `UPDATE instances SET pending_seconds = pending_seconds + $2, updated_at = $3 WHERE id = $1`, inst.ID, secs, now); err != nil {
			return err
		}
		var open bool // 已有未完成的启动指令则不重复下发；启动失败后再次加时会重新下发
		if open, err = s.d.Fleet.HasOpenCommand(ctx, tx, inst.ID, fleet.CmdStartInstance); err == nil && !open {
			_, err = s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdStartInstance, nil)
		}
	default:
		return apperr.ErrInvalidState
	}
	return err
}
```

Run: `go test ./internal/rental/ -run Renew`
Expected: PASS（续费行为不变）。

- [ ] **Step 3: 实现售后操作**

`internal/fleet/machines.go`：把 `SetMaintenance` 替换为：

```go
// SetMaintenance 进入维护（仅空闲或故障机器）或结束维护回到空闲（仅维护中或故障机器）。
func (s *Service) SetMaintenance(ctx context.Context, id int64, on bool) error {
	return s.SetMaintenanceIn(ctx, s.pool, id, on)
}

// SetMaintenanceIn 与 SetMaintenance 相同，但在调用方给定的连接或事务内执行。
func (s *Service) SetMaintenanceIn(ctx context.Context, q db.Querier, id int64, on bool) error {
	from, to := []string{StateIdle, StateFaulty}, StateMaintenance
	if !on {
		from, to = []string{StateMaintenance, StateFaulty}, StateIdle
	}
	tag, err := q.Exec(ctx, `UPDATE machines SET state = $2, updated_at = $3 WHERE id = $1 AND state = ANY($4)`,
		id, to, s.clock.Now(), from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, q, id); err != nil {
			return err
		}
		return apperr.ErrInvalidState
	}
	return nil
}
```

`internal/rental/admin_ops.go`：

```go
package rental

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

const (
	MinExtendSeconds = 60
	MaxExtendSeconds = 30 * 24 * 3600
)

// AdminExtend 后台免费补偿时长：不产生订单，按实例状态加时，写实例事件与审计。
func (s *Service) AdminExtend(ctx context.Context, actor audit.Actor, instanceID string, seconds int64, reason string) (Instance, error) {
	reason = strings.TrimSpace(reason)
	if seconds < MinExtendSeconds || seconds > MaxExtendSeconds {
		return Instance{}, apperr.BadRequest("补偿时长需在 1 分钟到 30 天之间")
	}
	if reason == "" {
		return Instance{}, apperr.BadRequest("请填写补偿原因")
	}
	now := s.d.Clock.Now()
	var out Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if err := s.addTime(ctx, tx, inst, seconds, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, inst.ID, inst.State, inst.State, fmt.Sprintf("admin extend +%ds: %s", seconds, reason), now); err != nil {
			return err
		}
		if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, now, actor.Entry("instance.extend", "instance", inst.ID,
			map[string]any{"seconds": seconds, "reason": reason, "state": inst.State})); err != nil {
			return err
		}
		out, err = lockInstance(ctx, tx, inst.ID)
		return err
	})
	return out, err
}

// AdminRewipe 对洗机中（通常是洗机失败）的实例取消未完成的指令并重新下发洗机。机器为 faulty 时也会下发，
// 由运维确认机器已修复后操作；洗机成功后机器保持 faulty，需运维结束维护才回到库存。
func (s *Service) AdminRewipe(ctx context.Context, actor audit.Actor, instanceID string) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if inst.State != StateWiping {
			return apperr.ErrInvalidState
		}
		if err := s.d.Fleet.CancelCommands(ctx, tx, inst.ID, "superseded by admin rewipe"); err != nil {
			return err
		}
		if _, err := s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdWipeInstance, nil); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, inst.ID, StateWiping, StateWiping, "admin rewipe", now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("instance.rewipe", "instance", inst.ID,
			map[string]any{"machine_id": inst.MachineID}))
	})
}

// SetMachineMaintenance 让机器进入或结束维护；结束维护（回到库存）前要求机器上没有未结束的实例。
func (s *Service) SetMachineMaintenance(ctx context.Context, actor audit.Actor, machineID int64, on bool) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if !on {
			var busy bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instances
				WHERE machine_id = $1 AND state IN ('creating', 'running', 'stopped', 'wiping'))`, machineID).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return apperr.New("invalid_state", "该机器上还有未结束的实例，不能结束维护", 409)
			}
		}
		if err := s.d.Fleet.SetMaintenanceIn(ctx, tx, machineID, on); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("machine.maintenance", "machine", strconv.FormatInt(machineID, 10),
			map[string]any{"on": on}))
	})
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/rental/ ./internal/fleet/`
Expected: PASS

- [ ] **Step 5: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/rental internal/fleet
git commit -m "feat(rental): admin free extension, rewipe after wipe failure, guarded end-of-maintenance

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 13: 实例售后（二）：换机

**Files:**
- Create: `migrations/00014_instance_transfers.sql`
- Create: `internal/rental/migrate.go`、`internal/rental/migrate_test.go`
- Modify: `internal/catalog/catalog.go`（新增 `ImageRef`）、`internal/catalog/catalog_test.go`

**Interfaces:**
- Consumes: Task 12 的包内辅助；计划 1 的 `fleet.Allocate`、`openLease`、`beginWipe`、`recordEvent`、`randx`、`secretbox`。
- Produces（Task 14、17 使用）：
  - `(*catalog.Service).ImageRef(ctx, q db.Querier, id int64) (string, error)`（不要求镜像仍启用）。
  - `(*rental.Service).AdminMigrate(ctx, actor audit.Actor, instanceID, reason string) (Instance, error)`：返回新实例。
  - 表 `instance_transfers(from_instance_id, to_instance_id, seconds, reason, admin_id, created_at)`；原实例的订单通过它关联到新实例（Task 14 沿链路统计可退金额）。

规则（规格 §3）：仅 running（含暂停中）可换；在同产品另一台空闲机器上新建实例（同用户、同镜像，`pending_seconds` = 原实例剩余秒数，不扣费、不新建订单），原实例进入洗机；没有空闲机器返回 `no_capacity` 且什么都不改。

- [ ] **Step 1: 写迁移**

`migrations/00014_instance_transfers.sql`：

```sql
-- +goose Up
-- 换机记录：原实例剩余时长转移到新实例，原实例的已付订单通过此表关联到新实例
CREATE TABLE instance_transfers (
    id               BIGSERIAL PRIMARY KEY,
    from_instance_id TEXT NOT NULL UNIQUE REFERENCES instances (id),
    to_instance_id   TEXT NOT NULL UNIQUE REFERENCES instances (id),
    seconds          BIGINT NOT NULL,
    reason           TEXT NOT NULL,
    admin_id         BIGINT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE instance_transfers;
```

- [ ] **Step 2: 写失败测试**

在 `internal/catalog/catalog_test.go` 末尾追加：

```go
func TestImageRefIgnoresEnabledFlag(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	ctx := context.Background()
	id := dbtest.ImageID(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE images SET enabled = false WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	ref, err := svc.ImageRef(ctx, pool, id)
	if err != nil || ref == "" {
		t.Fatalf("ref = %q, %v", ref, err)
	}
	if _, err := svc.ImageRef(ctx, pool, 9999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing image = %v", err)
	}
}
```

（若 `catalog_test.go` 尚未 import `"errors"`、`"spark-cloud/internal/platform/apperr"`，补上。）

`internal/rental/migrate_test.go`：

```go
package rental_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

func TestAdminMigrateMovesRemainingTime(t *testing.T) {
	e := newEnv(t, 2)
	_, old := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(2 * time.Hour)
	balance := e.balance(e.userID)

	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "GPU 报错")
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID == old.ID || moved.MachineID != e.machines[1] || moved.State != rental.StateCreating ||
		moved.PendingSeconds != 22*3600 || moved.UserID != e.userID || moved.ImageID != old.ImageID {
		t.Fatalf("new instance = %+v", moved)
	}
	if got := e.get(old.ID); got.State != rental.StateWiping {
		t.Fatalf("old state = %s", got.State)
	}
	if e.balance(e.userID) != balance {
		t.Fatal("migration must be free")
	}
	var orders, transfers int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM orders WHERE user_id = $1`, e.userID).Scan(&orders)
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM instance_transfers WHERE from_instance_id = $1 AND to_instance_id = $2 AND seconds = 79200`,
		old.ID, moved.ID).Scan(&transfers)
	if orders != 1 || transfers != 1 || e.auditCount("instance.migrate") != 1 {
		t.Fatalf("orders=%d transfers=%d audit=%d", orders, transfers, e.auditCount("instance.migrate"))
	}
	cmds, _ := e.fleet.PendingCommands(e.ctx, e.machines[1])
	var p fleet.CreateInstancePayload
	if len(cmds) != 1 || cmds[0].Type != fleet.CmdCreateInstance || json.Unmarshal(cmds[0].Payload, &p) != nil || p.ImageRef == "" {
		t.Fatalf("create command = %+v", cmds)
	}

	e.clock.Advance(time.Minute)
	e.ack(true) // 新实例开通、原实例洗机完成
	if got := e.get(moved.ID); got.State != rental.StateRunning || got.Remaining(e.clock.Now()) != 22*time.Hour {
		t.Fatalf("moved after create: state=%s remaining=%v", got.State, got.Remaining(e.clock.Now()))
	}
	if e.get(old.ID).State != rental.StateReleased || e.machineState(e.machines[0]) != fleet.StateIdle {
		t.Fatalf("old not released: %s / %s", e.get(old.ID).State, e.machineState(e.machines[0]))
	}
}

func TestAdminMigrateSuspendedUsesFrozenTime(t *testing.T) {
	e := newEnv(t, 2)
	_, old := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(time.Hour)
	if _, err := e.svc.SuspendMachines(e.ctx, e.machines[:1]); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(5 * time.Hour) // 失联期间不计时
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "机器失联")
	if err != nil || moved.PendingSeconds != 23*3600 {
		t.Fatalf("pending = %d, %v", moved.PendingSeconds, err)
	}
}

func TestAdminMigrateNoCapacityChangesNothing(t *testing.T) {
	e := newEnv(t, 1)
	_, old := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "GPU 报错"); !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("no capacity = %v", err)
	}
	if got := e.get(old.ID); got.State != rental.StateRunning {
		t.Fatalf("old state = %s", got.State)
	}
	if e.auditCount("instance.migrate") != 0 {
		t.Fatal("failed migration must not be audited")
	}
}

func TestAdminMigrateValidation(t *testing.T) {
	e := newEnv(t, 2)
	_, inst := e.place(catalog.D1d)
	if _, err := e.svc.AdminMigrate(e.ctx, opsActor, inst.ID, "x"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("creating = %v", err)
	}
	e.ack(true)
	if _, err := e.svc.AdminMigrate(e.ctx, opsActor, inst.ID, " "); codeOf(err) != "bad_request" {
		t.Fatalf("empty reason = %v", err)
	}
	if _, err := e.svc.AdminMigrate(e.ctx, opsActor, "ins-missing", "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestAdminMigrateWorksWithDisabledImage(t *testing.T) {
	e := newEnv(t, 2)
	_, old := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.pool.Exec(e.ctx, `UPDATE images SET enabled = false`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "镜像已下架也要能换机"); err != nil {
		t.Fatal(err)
	}
}
```

Run: `go test ./internal/rental/ ./internal/catalog/ -run 'Migrate|ImageRef'`
Expected: 编译失败（`AdminMigrate`、`ImageRef` 未定义）。

- [ ] **Step 3: 实现**

`internal/catalog/catalog.go` 末尾追加：

```go
// ImageRef 返回镜像地址，不要求镜像仍在售（换机时沿用用户原来的镜像）。
func (s *Service) ImageRef(ctx context.Context, q db.Querier, id int64) (string, error) {
	var ref string
	err := q.QueryRow(ctx, `SELECT ref FROM images WHERE id = $1`, id).Scan(&ref)
	if db.IsNoRows(err) {
		return "", apperr.ErrNotFound
	}
	return ref, err
}
```

`internal/rental/migrate.go`：

```go
package rental

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

// AdminMigrate 把运行中实例的剩余时长转移到同产品的另一台空闲机器上新建的实例，原实例进入洗机。
// 不扣费、不新建订单；原订单通过 instance_transfers 关联。加锁顺序：原实例 → 新机器 → 指令。
func (s *Service) AdminMigrate(ctx context.Context, actor audit.Actor, instanceID, reason string) (Instance, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Instance{}, apperr.BadRequest("请填写换机原因")
	}
	now := s.d.Clock.Now()
	var moved Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		old, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if old.State != StateRunning {
			return apperr.ErrInvalidState
		}
		remaining := int64(old.Remaining(now) / time.Second)
		if remaining <= 0 {
			return apperr.New("invalid_state", "该实例已没有剩余时长，无需换机", 409)
		}
		ref, err := s.d.Catalog.ImageRef(ctx, tx, old.ImageID)
		if err != nil {
			return err
		}
		m, err := s.d.Fleet.Allocate(ctx, tx, old.ProductID)
		if err != nil {
			return err
		}
		id := randx.ID("ins-", 8)
		moved, err = scanInstance(tx.QueryRow(ctx, `INSERT INTO instances
			(id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'creating', $6, $7, $8, $8) RETURNING `+instanceCols,
			id, old.UserID, m.ID, old.ProductID, old.ImageID, remaining, s.d.Box.Seal([]byte(randx.Password(16))), now))
		if err != nil {
			return err
		}
		if err := openLease(ctx, tx, moved, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, id, "", StateCreating, "migrated from "+old.ID+": "+reason, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO instance_transfers (from_instance_id, to_instance_id, seconds, reason, admin_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, old.ID, id, remaining, reason, actor.AdminID, now); err != nil {
			return err
		}
		if _, err := s.d.Fleet.Enqueue(ctx, tx, m.ID, id, fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: ref}); err != nil {
			return err
		}
		if err := s.beginWipe(ctx, tx, old, "migrated to "+id+": "+reason, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("instance.migrate", "instance", old.ID, map[string]any{
			"to_instance": id, "from_machine": old.MachineID, "to_machine": m.ID, "seconds": remaining, "reason": reason,
		}))
	})
	return moved, err
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/rental/ ./internal/catalog/`
Expected: PASS

- [ ] **Step 5: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add migrations/00014_instance_transfers.sql internal/rental internal/catalog
git commit -m "feat(rental): admin machine migration that transfers remaining time to a new instance

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 14: 实例售后（三）：退款并释放

**Files:**
- Create: `migrations/00015_refunds.sql`
- Create: `internal/rental/refund.go`、`internal/rental/refund_test.go`
- Modify: `internal/rental/results.go`（`refundAll` 记录已退金额）
- Modify: `internal/rental/orders.go`（`Order` 增加 `RefundedAmount`）

**Interfaces:**
- Consumes: Task 13 的 `instance_transfers`；Task 12 的 `opsActor`/`codeOf`/`auditCount` 测试辅助；计划 1 的 `wallet.Apply`（`KindRefund`）、`beginWipe`、`catalog.Duration.Length`。
- Produces（Task 17、19 使用）：
  - `rental.Order` 新字段 `RefundedAmount int64`（json `refunded_amount`）；`orderCols` 包含 `refunded_amount`。
  - `rental.RefundQuote{InstanceID string; RemainingSeconds int64; UnitOrderID string; Suggested int64; Max int64}`（json：`instance_id, remaining_seconds, unit_order_id, suggested, max`）。
  - `(*rental.Service).RefundQuote(ctx, instanceID string) (RefundQuote, error)`
  - `(*rental.Service).RefundRelease(ctx, actor audit.Actor, instanceID string, amount int64, reason string) (int64, error)`：返回实际退款金额（分）。
  - 包内 `instanceChain(ctx, q db.Querier, id string) ([]string, error)`：该实例及其通过换机链路追溯到的全部前身实例。

规则（规格 §3）：
- 可退上限 = 换机链路上所有 `paid` / `partially_refunded` 订单的（金额 − 已退金额）之和。
- 建议金额 = 剩余可用秒数（running 用剩余时长，暂停中用冻结的剩余时长；creating / stopped 用 `pending_seconds`）× 最近一笔可退订单的单价（订单金额 ÷ 订单秒数），向下取整到分，且不超过可退上限。
- 执行：creating / running / stopped 可用；同一事务内先让实例进入洗机，再按订单从新到旧扣减可退额度、每笔订单记一条 refund 流水（`ref_id` = 订单号 + `-r` + 该订单的退款序号），订单状态改为 `refunded` 或 `partially_refunded`；退款金额可以为 0（只释放）。

- [ ] **Step 1: 写迁移**

`migrations/00015_refunds.sql`：

```sql
-- +goose Up
-- 订单已退金额与退款次数：支持「退款并释放」按订单部分退款
ALTER TABLE orders
    ADD COLUMN refunded_amount BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN refund_count INT NOT NULL DEFAULT 0;
UPDATE orders SET refunded_amount = amount, refund_count = 1 WHERE status = 'refunded';
ALTER TABLE orders ADD CONSTRAINT orders_refunded_amount_chk CHECK (refunded_amount >= 0 AND refunded_amount <= amount);

-- +goose Down
ALTER TABLE orders DROP CONSTRAINT orders_refunded_amount_chk, DROP COLUMN refund_count, DROP COLUMN refunded_amount;
```

- [ ] **Step 2: 写失败测试**

`internal/rental/refund_test.go`：

```go
package rental_test

import (
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

func (e *env) orderState(id string) (string, int64) {
	e.t.Helper()
	var status string
	var refunded int64
	if err := e.pool.QueryRow(e.ctx, `SELECT status, refunded_amount FROM orders WHERE id = $1`, id).Scan(&status, &refunded); err != nil {
		e.t.Fatal(err)
	}
	return status, refunded
}

func TestRefundQuoteRunning(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(6 * time.Hour)
	q, err := e.svc.RefundQuote(e.ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 剩余 18 小时 × 6900 分 / 86400 秒 = 5175 分
	if q.RemainingSeconds != 18*3600 || q.Suggested != 5175 || q.Max != 6900 {
		t.Fatalf("quote = %+v", q)
	}
}

func TestRefundQuoteUsesLatestOrderPrice(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	renew, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w, "")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(12 * time.Hour)
	q, _ := e.svc.RefundQuote(e.ctx, inst.ID)
	remaining := int64(8*24*3600 - 12*3600)
	if q.UnitOrderID != renew.ID || q.RemainingSeconds != remaining || q.Suggested != remaining*41400/(7*86400) || q.Max != 6900+41400 {
		t.Fatalf("quote = %+v", q)
	}
}

func TestRefundQuoteStoppedUsesPendingSeconds(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	expireAndStop(e)
	if q, _ := e.svc.RefundQuote(e.ctx, inst.ID); q.Suggested != 0 || q.Max != 6900 {
		t.Fatalf("stopped without renewal = %+v", q)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w, ""); err != nil {
		t.Fatal(err)
	}
	if q, _ := e.svc.RefundQuote(e.ctx, inst.ID); q.RemainingSeconds != 7*86400 || q.Suggested != 41400 {
		t.Fatalf("stopped with pending renewal = %+v", q)
	}
}

func TestRefundReleasePartialAcrossOrders(t *testing.T) {
	e := newEnv(t, 1)
	first, inst := e.place(catalog.D1d)
	e.ack(true)
	second, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, "")
	if err != nil {
		t.Fatal(err)
	}
	before := e.balance(e.userID)
	got, err := e.svc.RefundRelease(e.ctx, opsActor, inst.ID, 10000, "机器故障无法修复")
	if err != nil || got != 10000 {
		t.Fatalf("refund = %d, %v", got, err)
	}
	if e.balance(e.userID) != before+10000 || e.get(inst.ID).State != rental.StateWiping {
		t.Fatalf("balance=%d state=%s", e.balance(e.userID), e.get(inst.ID).State)
	}
	if s, r := e.orderState(second.ID); s != "refunded" || r != 6900 {
		t.Fatalf("newest order = %s %d", s, r)
	}
	if s, r := e.orderState(first.ID); s != "partially_refunded" || r != 3100 {
		t.Fatalf("older order = %s %d", s, r)
	}
	var refs []string
	rows, _ := e.pool.Query(e.ctx, `SELECT ref_id FROM wallet_ledger WHERE kind = 'refund' ORDER BY id`)
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		refs = append(refs, r)
	}
	rows.Close()
	if len(refs) != 2 || refs[0] != second.ID+"-r1" || refs[1] != first.ID+"-r1" {
		t.Fatalf("refund refs = %v", refs)
	}
	if e.auditCount("instance.refund_release") != 1 {
		t.Fatal("refund not audited")
	}
	e.ack(true)
	if e.get(inst.ID).State != rental.StateReleased {
		t.Fatal("not released after wipe")
	}
}

func TestRefundReleaseLimits(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	for _, c := range []struct {
		amount int64
		reason string
	}{{6901, "x"}, {-1, "x"}, {100, " "}} {
		if _, err := e.svc.RefundRelease(e.ctx, opsActor, inst.ID, c.amount, c.reason); codeOf(err) != "bad_request" {
			t.Fatalf("refund(%d, %q) = %v", c.amount, c.reason, err)
		}
	}
	if e.get(inst.ID).State != rental.StateRunning || e.balance(e.userID) != startFunds-6900 {
		t.Fatal("rejected refund must not change anything")
	}
	if _, err := e.svc.RefundRelease(e.ctx, opsActor, inst.ID, 0, "用户要求立即释放"); err != nil {
		t.Fatal(err)
	}
	if e.get(inst.ID).State != rental.StateWiping || e.balance(e.userID) != startFunds-6900 {
		t.Fatal("zero refund should only release")
	}
	if _, err := e.svc.RefundRelease(e.ctx, opsActor, inst.ID, 100, "again"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("refund while wiping = %v", err)
	}
}

func TestRefundReleaseCreating(t *testing.T) {
	e := newEnv(t, 1)
	ord, inst := e.place(catalog.D1d)
	if _, err := e.svc.RefundRelease(e.ctx, opsActor, inst.ID, 6900, "开通卡住，全额退"); err != nil {
		t.Fatal(err)
	}
	if s, r := e.orderState(ord.ID); s != "refunded" || r != 6900 || e.balance(e.userID) != startFunds {
		t.Fatalf("order=%s %d balance=%d", s, r, e.balance(e.userID))
	}
	if n := countPending(e, e.machines[0], fleet.CmdCreateInstance); n != 0 {
		t.Fatalf("create command still pending: %d", n)
	}
}

func TestRefundAfterMigrationUsesOriginalOrders(t *testing.T) {
	e := newEnv(t, 2)
	ord, old := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(12 * time.Hour)
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "换机")
	if err != nil {
		t.Fatal(err)
	}
	q, err := e.svc.RefundQuote(e.ctx, moved.ID)
	if err != nil || q.Max != 6900 || q.UnitOrderID != ord.ID || q.RemainingSeconds != 12*3600 || q.Suggested != 3450 {
		t.Fatalf("quote on migrated instance = %+v, %v", q, err)
	}
	if _, err := e.svc.RefundRelease(e.ctx, opsActor, moved.ID, 3450, "换机后仍故障"); err != nil {
		t.Fatal(err)
	}
	if s, r := e.orderState(ord.ID); s != "partially_refunded" || r != 3450 {
		t.Fatalf("original order = %s %d", s, r)
	}
}

func TestCreateFailureRecordsRefundedAmount(t *testing.T) {
	e := newEnv(t, 1)
	ord, _ := e.place(catalog.D1d)
	e.ack(false)
	if s, r := e.orderState(ord.ID); s != "refunded" || r != 6900 {
		t.Fatalf("order = %s %d", s, r)
	}
}
```

Run: `go test ./internal/rental/ -run 'Refund|CreateFailureRecords'`
Expected: 编译失败（`RefundQuote` 等未定义）。

- [ ] **Step 3: 订单增加已退金额**

`internal/rental/orders.go`：

1. `Order` 结构体在 `Status` 之后增加 `RefundedAmount int64 `json:"refunded_amount"``。
2. `orderCols` 改为 `id, user_id, kind, instance_id, product_id, duration, amount, status, refunded_amount, created_at`，`scanOrder` 中对应位置加 `&o.RefundedAmount`：

```go
const orderCols = `id, user_id, kind, instance_id, product_id, duration, amount, status, refunded_amount, created_at`

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.UserID, &o.Kind, &o.InstanceID, &o.ProductID, &o.Duration, &o.Amount, &o.Status, &o.RefundedAmount, &o.CreatedAt)
	if db.IsNoRows(err) {
		return Order{}, apperr.ErrNotFound
	}
	return o, err
}
```

`internal/rental/results.go` 的 `refundAll` 替换为（退还每笔订单尚未退的部分，并记录已退金额）：

```go
// refundAll 退还该实例所有已支付订单（新购与续费）尚未退还的部分。
func (s *Service) refundAll(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id, amount - refunded_amount FROM orders
		WHERE instance_id = $1 AND status IN ('paid', 'partially_refunded') ORDER BY created_at, id FOR UPDATE`, inst.ID)
	if err != nil {
		return err
	}
	type paid struct {
		id     string
		amount int64
	}
	orders, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (paid, error) {
		var p paid
		err := r.Scan(&p.id, &p.amount)
		return p, err
	})
	if err != nil {
		return err
	}
	for _, o := range orders {
		if o.amount > 0 {
			if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{
				UserID: inst.UserID, Amount: o.amount, Kind: wallet.KindRefund, RefType: "order", RefID: o.id,
				Remark: "开通失败退款 " + inst.ID,
			}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET status = 'refunded', refunded_amount = amount, refund_count = refund_count + 1,
			updated_at = $2 WHERE id = $1`, o.id, now); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: 实现退款并释放**

`internal/rental/refund.go`：

```go
package rental

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

type RefundQuote struct {
	InstanceID       string `json:"instance_id"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	UnitOrderID      string `json:"unit_order_id"` // 计算单价所用的订单（最近一笔可退订单）
	Suggested        int64  `json:"suggested"`     // 建议退款金额（分）
	Max              int64  `json:"max"`           // 可退上限（分）
}

type refundable struct {
	id          string
	amount      int64
	refunded    int64
	refundCount int
	duration    catalog.Duration
}

// instanceChain 返回该实例以及通过换机链路追溯到的全部前身实例 ID。
func instanceChain(ctx context.Context, q db.Querier, id string) ([]string, error) {
	rows, err := q.Query(ctx, `WITH RECURSIVE chain (id) AS (
			SELECT $1::text
			UNION
			SELECT t.from_instance_id FROM instance_transfers t JOIN chain c ON t.to_instance_id = c.id
		) SELECT id FROM chain`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// refundableOrders 返回链路上仍可退的订单，从新到旧；lock 为 true 时加行锁（调用方须已锁实例）。
func refundableOrders(ctx context.Context, q db.Querier, chain []string, lock bool) ([]refundable, error) {
	sql := `SELECT id, amount, refunded_amount, refund_count, duration FROM orders
		WHERE instance_id = ANY($1) AND status IN ('paid', 'partially_refunded') ORDER BY created_at DESC, id DESC`
	if lock {
		sql += ` FOR UPDATE`
	}
	rows, err := q.Query(ctx, sql, chain)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (refundable, error) {
		var o refundable
		err := r.Scan(&o.id, &o.amount, &o.refunded, &o.refundCount, &o.duration)
		return o, err
	})
}

func remainingSeconds(inst Instance, now time.Time) int64 {
	switch inst.State {
	case StateRunning:
		return int64(inst.Remaining(now) / time.Second)
	case StateCreating, StateStopped:
		return inst.PendingSeconds
	}
	return 0
}

func quote(inst Instance, orders []refundable, now time.Time) RefundQuote {
	q := RefundQuote{InstanceID: inst.ID, RemainingSeconds: remainingSeconds(inst, now)}
	for _, o := range orders {
		q.Max += o.amount - o.refunded
	}
	if len(orders) > 0 {
		latest := orders[0]
		q.UnitOrderID = latest.id
		if secs := seconds(latest.duration); secs > 0 {
			q.Suggested = q.RemainingSeconds * latest.amount / secs
		}
	}
	if q.Suggested > q.Max {
		q.Suggested = q.Max
	}
	return q
}

// RefundQuote 计算退款建议金额与可退上限（不修改任何数据）。
func (s *Service) RefundQuote(ctx context.Context, instanceID string) (RefundQuote, error) {
	inst, err := s.Get(ctx, instanceID)
	if err != nil {
		return RefundQuote{}, err
	}
	chain, err := instanceChain(ctx, s.d.Pool, inst.ID)
	if err != nil {
		return RefundQuote{}, err
	}
	orders, err := refundableOrders(ctx, s.d.Pool, chain, false)
	if err != nil {
		return RefundQuote{}, err
	}
	return quote(inst, orders, s.d.Clock.Now()), nil
}

// RefundRelease 后台「退款并释放」：实例进入洗机，并按订单从新到旧退还 amount（分）到余额。返回实际退款金额。
// 加锁顺序：instance → agent_commands（beginWipe）→ orders → wallet。
func (s *Service) RefundRelease(ctx context.Context, actor audit.Actor, instanceID string, amount int64, reason string) (int64, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return 0, apperr.BadRequest("请填写退款原因")
	}
	if amount < 0 {
		return 0, apperr.BadRequest("退款金额不能为负数")
	}
	now := s.d.Clock.Now()
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if inst.State != StateCreating && inst.State != StateRunning && inst.State != StateStopped {
			return apperr.ErrInvalidState
		}
		chain, err := instanceChain(ctx, tx, inst.ID)
		if err != nil {
			return err
		}
		if err := s.beginWipe(ctx, tx, inst, "admin refund-release: "+reason, now); err != nil {
			return err
		}
		orders, err := refundableOrders(ctx, tx, chain, true)
		if err != nil {
			return err
		}
		q := quote(inst, orders, now) // inst 是加锁时读到的状态，beginWipe 不会改变它
		if amount > q.Max {
			return apperr.BadRequest(fmt.Sprintf("退款金额不能超过可退金额 %d.%02d 元", q.Max/100, q.Max%100))
		}
		left := amount
		var parts []map[string]any
		for _, o := range orders {
			if left == 0 {
				break
			}
			take := min(left, o.amount-o.refunded)
			if take <= 0 {
				continue
			}
			seq := o.refundCount + 1
			if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{
				UserID: inst.UserID, Amount: take, Kind: wallet.KindRefund, RefType: "order", RefID: fmt.Sprintf("%s-r%d", o.id, seq),
				OperatorID: actor.AdminID, Remark: "退款并释放 " + inst.ID + "：" + reason,
			}); err != nil {
				return err
			}
			status := "partially_refunded"
			if o.refunded+take == o.amount {
				status = "refunded"
			}
			if _, err := tx.Exec(ctx, `UPDATE orders SET refunded_amount = refunded_amount + $2, refund_count = $3, status = $4, updated_at = $5
				WHERE id = $1`, o.id, take, seq, status, now); err != nil {
				return err
			}
			parts = append(parts, map[string]any{"order_id": o.id, "amount": take})
			left -= take
		}
		return audit.Record(ctx, tx, now, actor.Entry("instance.refund_release", "instance", inst.ID, map[string]any{
			"amount": amount, "reason": reason, "state": inst.State, "suggested": q.Suggested, "max": q.Max, "orders": parts,
		}))
	})
	if err != nil {
		return 0, err
	}
	return amount, nil
}
```

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/rental/`
Expected: PASS（含计划 1 的开通失败退款测试）。

- [ ] **Step 6: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add migrations/00015_refunds.sql internal/rental
git commit -m "feat(rental): refund-and-release with quote, partial per-order refunds across migrations

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 15: 后台 API 骨架：共用 HTTP 辅助、登录与 TOTP、角色校验、管理员管理

**Files:**
- Create: `internal/platform/httpx/httpx.go`、`internal/platform/httpx/httpx_test.go`
- Modify: `internal/api/httpx.go`（改为委托 `httpx`）、`internal/api/server.go`（使用 `httpx.Recoverer` / `httpx.NoStore`）
- Delete: `internal/api/recover_test.go`（测试迁移到 `httpx_test.go`）
- Create: `internal/adminapi/server.go`、`internal/adminapi/ratelimit.go`、`internal/adminapi/auth.go`、`internal/adminapi/admins.go`
- Test: `internal/adminapi/helpers_test.go`、`internal/adminapi/auth_test.go`

**Interfaces:**
- Consumes: Task 3、4 的 `admin.Service` 全部方法与错误；Task 1 的 `audit.Actor`；其余各模块 Service（只在测试环境中组装）。
- Produces（Task 16～21 使用）：
  - `httpx.ErrorBody`、`httpx.WriteJSON(w, status, v)`、`httpx.OK(w)`、`httpx.List[T](items []T) map[string]any`、`httpx.Page[T](items []T, total int) map[string]any`（`{items, total}`，空列表输出 `[]`）、`httpx.WriteError(log, w, r, err)`、`httpx.WriteInternal(w, rid)`、`httpx.DecodeJSON(r, v) error`、`httpx.ClientIP(r) string`、`httpx.Recoverer(log) func(http.Handler) http.Handler`、`httpx.NoStore(next) http.Handler`。
  - `adminapi.Server{Admins, Identity, Wallet, Catalog, Fleet, Rental, Billing, Uploads, Clock, Log, SecureCookie, TrustProxy}`、`(*Server).Router() http.Handler`、`(*Server).Routes() []adminapi.Route`、`adminapi.Route{Method, Pattern string; Roles []admin.Role}`。
  - 包内：`prefix = "/admin/api/v1"`、`cookieName = "sc_admin"`、角色集合 `allRoles`、`opsRoles`、`financeRoles`、`superOnly`、`(*Server).handle(g chi.Router, method, pattern string, roles []admin.Role, h http.HandlerFunc)`、`currentSession(r) admin.Session`、`(*Server).actor(r) audit.Actor`、`(*Server).stepUp(r, code string) error`、`(*Server).writeError(w, r, err)`、`pathID(r, name string) (int64, error)`、`pageOf(r) (paging.Page, error)`。
  - 接口：`POST /auth/login {username, password}` → `{next: "totp"|"totp_setup", otpauth_uri}` 并设置 Cookie；`POST /auth/totp/setup {code}` → `{admin, recovery_codes}`；`POST /auth/totp {code}` 或 `{recovery_code}` → `{admin}`；`POST /auth/logout`；`GET /me`；`POST /me/password {old, new}`；`GET /admins`；`POST /admins {username, password, role, totp}`；`PUT /admins/{id} {role, status, totp}`；`POST /admins/{id}/reset-totp {totp}`。
  - 测试辅助（`package adminapi_test`）：`newEnv(t) *env`（组装全部服务与后台 httptest 服务器）、`(*env).anon() *client`、`(*env).loginAs(username string, role admin.Role) *client`、`(*client).do(method, path string, body any) (int, map[string]any)`、`(*client).send(...) (*http.Response, map[string]any)`、`(*client).totp() string`、`codeOf(out map[string]any) string`。

后台 Cookie：名称 `sc_admin`，`Path=/admin/`，`HttpOnly`，`SameSite=Strict`，生产环境 `Secure`。所有写请求必须是 `application/json`。登录、`/auth/totp`、`/auth/totp/setup` 三个接口同一 IP 每分钟最多 20 次（进程内计数）。

- [ ] **Step 1: 抽出共用 HTTP 辅助（失败测试）**

`internal/platform/httpx/httpx_test.go`：

```go
package httpx_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
)

func TestRecovererWritesJSONErrorAndLogs(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	h := middleware.RequestID(httpx.Recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusInternalServerError || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status = %d, content-type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if body.Code != "internal" || body.Message != "服务器内部错误，请稍后重试" || body.RequestID == "" {
		t.Fatalf("body = %+v", body)
	}
	if out := logBuf.String(); !strings.Contains(out, "boom") || !strings.Contains(out, "goroutine") {
		t.Fatalf("log missing panic value or stack: %s", out)
	}
}

func TestWriteErrorMapsBusinessErrors(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := httptest.NewRecorder()
	httpx.WriteError(log, rec, httptest.NewRequest("GET", "/", nil), apperr.ErrNotFound)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
		t.Fatalf("business error = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	httpx.WriteError(log, rec, httptest.NewRequest("GET", "/", nil), errors.New("db down"))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("internal error = %d %s", rec.Code, rec.Body.String())
	}
}

func TestPageAndListNeverNull(t *testing.T) {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(httpx.Page[int](nil, 0))
	if strings.TrimSpace(buf.String()) != `{"items":[],"total":0}` {
		t.Fatalf("page = %s", buf.String())
	}
	buf.Reset()
	_ = json.NewEncoder(&buf).Encode(httpx.List[string](nil))
	if strings.TrimSpace(buf.String()) != `{"items":[]}` {
		t.Fatalf("list = %s", buf.String())
	}
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"a":1,"b":2}`))
	if err := httpx.DecodeJSON(req, &v); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("unknown field = %v", err)
	}
}
```

Run: `go test ./internal/platform/httpx/`
Expected: 编译失败（包不存在）。

- [ ] **Step 2: 实现 httpx 并让用户 API 使用它**

`internal/platform/httpx/httpx.go`：

```go
// Package httpx 是用户 API 与后台 API 共用的 HTTP 辅助：JSON 读写、统一错误格式、panic 恢复、缓存控制与客户端 IP。
package httpx

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"

	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/platform/apperr"
)

type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func OK(w http.ResponseWriter) { WriteJSON(w, http.StatusOK, map[string]bool{"ok": true}) }

// List 包装列表响应，保证空列表输出 [] 而不是 null。
func List[T any](items []T) map[string]any {
	if items == nil {
		items = []T{}
	}
	return map[string]any{"items": items}
}

// Page 包装分页列表响应 {items, total}。
func Page[T any](items []T, total int) map[string]any {
	if items == nil {
		items = []T{}
	}
	return map[string]any{"items": items, "total": total}
}

// WriteError 输出业务错误；未知错误记日志并统一返回 500，不暴露内部细节。
func WriteError(log *slog.Logger, w http.ResponseWriter, r *http.Request, err error) {
	rid := middleware.GetReqID(r.Context())
	if e := apperr.As(err); e != nil {
		WriteJSON(w, e.Status, ErrorBody{Code: e.Code, Message: e.Message, RequestID: rid})
		return
	}
	log.Error("internal error", "err", err, "request_id", rid, "method", r.Method, "path", r.URL.Path)
	WriteInternal(w, rid)
}

func WriteInternal(w http.ResponseWriter, rid string) {
	WriteJSON(w, http.StatusInternalServerError, ErrorBody{Code: "internal", Message: "服务器内部错误，请稍后重试", RequestID: rid})
}

// DecodeJSON 解析请求体（上限 1MB），拒绝未知字段。
func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.BadRequest("请求参数格式不正确")
	}
	return nil
}

func ClientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// Recoverer 捕获处理器中的 panic：记录日志（含堆栈）并按统一错误格式返回 500。
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler { //nolint:errorlint // 与 net/http 约定一致：继续向上抛出以中断连接
					panic(rec)
				}
				rid := middleware.GetReqID(r.Context())
				log.Error("panic", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()),
					"request_id", rid, "method", r.Method, "path", r.URL.Path)
				WriteInternal(w, rid)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// NoStore 禁止缓存响应（含密码、余额、个人信息等敏感数据）。
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
```

用以下内容整体替换 `internal/api/httpx.go`：

```go
package api

import (
	"net/http"

	"spark-cloud/internal/platform/httpx"
)

func writeJSON(w http.ResponseWriter, status int, v any) { httpx.WriteJSON(w, status, v) }

func ok(w http.ResponseWriter) { httpx.OK(w) }

func list[T any](items []T) map[string]any { return httpx.List(items) }

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(s.Log, w, r, err)
}

func decodeJSON(r *http.Request, v any) error { return httpx.DecodeJSON(r, v) }

func clientIP(r *http.Request) string { return httpx.ClientIP(r) }
```

`internal/api/server.go`：

1. 删除 `recoverer` 方法与 `noStore` 函数，import 中删去 `"fmt"`、`"runtime/debug"`，加入 `"spark-cloud/internal/platform/httpx"`。
2. `r.Use(s.recoverer, s.requireJSONForWrites)` 改为 `r.Use(httpx.Recoverer(s.Log), s.requireJSONForWrites)`。
3. `r.Use(noStore, s.requireUser)` 改为 `r.Use(httpx.NoStore, s.requireUser)`。

删除 `internal/api/recover_test.go`（同样的断言已在 `httpx_test.go` 中）。

Run: `go test ./internal/platform/httpx/ ./internal/api/`
Expected: PASS

- [ ] **Step 3: 写后台 API 的测试辅助与失败测试**

`internal/adminapi/helpers_test.go`：

```go
package adminapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/adminapi"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/pay"
	"spark-cloud/internal/plugins/pdu"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/storage"
	"spark-cloud/internal/wallet"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

const adminPassword = "correct horse battery"

type noopNotifier struct{}

func (noopNotifier) NotifyExpiry(context.Context, int64, string, time.Duration) error { return nil }

type env struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	clk      *clock.Fake
	srv      *adminapi.Server
	ts       *httptest.Server
	admins   *admin.Service
	identity *identity.Service
	wallet   *wallet.Service
	catalog  *catalog.Service
	fleet    *fleet.Service
	rental   *rental.Service
	billing  *billing.Service
	uploads  *storage.Service
	pdu      *pdu.Mock
	pay      *pay.Mock
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	disk, err := storage.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uploads := storage.NewService(pool, clk, disk)
	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: &sms.Mock{}, KYC: kyc.Mock{}, Box: box, Uploads: uploads})
	w := wallet.NewService(pool, clk)
	c := catalog.NewService(pool)
	f := fleet.NewService(pool, clk)
	pduMock := &pdu.Mock{}
	f.ConfigurePDU(fleet.PDUConfig{Box: box, AllowMock: true, Open: func(fleet.PDUConn) (pdu.Driver, error) { return pduMock, nil }})
	r := rental.NewService(rental.Deps{Pool: pool, Clock: clk, Box: box, Log: log, Users: ids, Wallet: w, Catalog: c, Fleet: f,
		Notifier: noopNotifier{}})
	payMock := pay.NewMock("s")
	b := billing.NewService(billing.Deps{Pool: pool, Clock: clk, Log: log, Wallet: w, Users: ids, Alerts: f,
		Providers: map[string]pay.Provider{pay.ChannelWechat: payMock}, NotifyBaseURL: "https://spark.test"})
	admins := admin.NewService(admin.Deps{Pool: pool, Clock: clk, Box: box})
	srv := &adminapi.Server{Admins: admins, Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Billing: b,
		Uploads: uploads, Clock: clk, Log: log}
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return &env{t: t, ctx: context.Background(), pool: pool, clk: clk, srv: srv, ts: ts, admins: admins, identity: ids,
		wallet: w, catalog: c, fleet: f, rental: r, billing: b, uploads: uploads, pdu: pduMock, pay: payMock}
}

type client struct {
	e      *env
	base   string
	http   *http.Client
	secret string
	admin  admin.Admin
}

func (e *env) anon() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, base: e.ts.URL + "/admin/api/v1", http: &http.Client{Jar: jar}}
}

// loginAs 创建一个指定角色的管理员，通过接口完成首次登录与 TOTP 绑定，返回已登录的客户端。
func (e *env) loginAs(username string, role admin.Role) *client {
	e.t.Helper()
	a, err := e.admins.Create(e.ctx, audit.Actor{}, username, adminPassword, role)
	if err != nil {
		e.t.Fatal(err)
	}
	c := e.anon()
	c.admin = a
	code, out := c.do("POST", "/auth/login", map[string]string{"username": username, "password": adminPassword})
	if code != 200 || out["next"] != "totp_setup" {
		e.t.Fatalf("login %s = %d %v", username, code, out)
	}
	u, err := url.Parse(out["otpauth_uri"].(string))
	if err != nil {
		e.t.Fatal(err)
	}
	c.secret = u.Query().Get("secret")
	if code, out := c.do("POST", "/auth/totp/setup", map[string]string{"code": c.totp()}); code != 200 {
		e.t.Fatalf("totp setup %s = %d %v", username, code, out)
	}
	return c
}

// totp 快进 30 秒后返回当前动态码（同一时间片的动态码只能使用一次）。
func (c *client) totp() string {
	c.e.clk.Advance(30 * time.Second)
	code, err := admin.TOTPCode(c.secret, admin.TOTPStep(c.e.clk.Now()))
	if err != nil {
		c.e.t.Fatal(err)
	}
	return code
}

func (c *client) send(method, path string, body any) (*http.Response, map[string]any) {
	c.e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, out
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.e.t.Helper()
	resp, out := c.send(method, path, body)
	return resp.StatusCode, out
}

func codeOf(out map[string]any) string {
	s, _ := out["code"].(string)
	return s
}
```

`internal/adminapi/auth_test.go`：

```go
package adminapi_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
)

func TestLoginAndTOTPSetupOverHTTP(t *testing.T) {
	e := newEnv(t)
	if _, err := e.admins.Create(e.ctx, audit.Actor{}, "root", adminPassword, admin.RoleSuper); err != nil {
		t.Fatal(err)
	}
	c := e.anon()
	c.admin.Username = "root"
	if code, out := c.do("GET", "/me", nil); code != 401 || codeOf(out) != "unauthorized" {
		t.Fatalf("anonymous me = %d %v", code, out)
	}
	if code, out := c.do("POST", "/auth/login", map[string]string{"username": "root", "password": "wrong password!"}); code != 401 || codeOf(out) != "invalid_credentials" {
		t.Fatalf("wrong password = %d %v", code, out)
	}
	resp, out := c.send("POST", "/auth/login", map[string]string{"username": "root", "password": adminPassword})
	cookie := resp.Header.Get("Set-Cookie")
	if resp.StatusCode != 200 || out["next"] != "totp_setup" || !strings.Contains(cookie, "sc_admin=") || !strings.Contains(cookie, "HttpOnly") ||
		!strings.Contains(cookie, "SameSite=Strict") || !strings.Contains(cookie, "Path=/admin/") {
		t.Fatalf("login = %d %v cookie=%q", resp.StatusCode, out, cookie)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("admin responses must not be cached")
	}
	u, _ := url.Parse(out["otpauth_uri"].(string))
	c.secret = u.Query().Get("secret")
	if code, _ := c.do("GET", "/me", nil); code != 401 {
		t.Fatalf("me before totp setup = %d", code)
	}
	if code, out := c.do("POST", "/auth/totp/setup", map[string]string{"code": "000000"}); code != 400 || codeOf(out) != "invalid_totp" {
		t.Fatalf("wrong setup code = %d %v", code, out)
	}
	code, out := c.do("POST", "/auth/totp/setup", map[string]string{"code": c.totp()})
	if code != 200 || len(out["recovery_codes"].([]any)) != admin.RecoveryCodeCount || out["admin"].(map[string]any)["totp_confirmed"] != true {
		t.Fatalf("setup = %d %v", code, out)
	}
	codes := out["recovery_codes"].([]any)
	if code, out := c.do("GET", "/me", nil); code != 200 || out["username"] != "root" || out["role"] != "super" {
		t.Fatalf("me = %d %v", code, out)
	}
	if code, _ := c.do("POST", "/auth/logout", map[string]any{}); code != 200 {
		t.Fatal("logout failed")
	}
	if code, _ := c.do("GET", "/me", nil); code != 401 {
		t.Fatalf("me after logout = %d", code)
	}

	if code, out := c.do("POST", "/auth/login", map[string]string{"username": "root", "password": adminPassword}); code != 200 || out["next"] != "totp" {
		t.Fatalf("second login = %d %v", code, out)
	}
	for _, body := range []map[string]string{{}, {"code": "123456", "recovery_code": "abcde-12345"}} {
		if code, out := c.do("POST", "/auth/totp", body); code != 400 || codeOf(out) != "bad_request" {
			t.Fatalf("totp body %v = %d %v", body, code, out)
		}
	}
	if code, out := c.do("POST", "/auth/totp", map[string]string{"recovery_code": codes[0].(string)}); code != 200 {
		t.Fatalf("recovery code login = %d %v", code, out)
	}
	if code, _ := c.do("GET", "/me", nil); code != 200 {
		t.Fatalf("me after recovery login = %d", code)
	}
}

func TestLoginRateLimitPerIP(t *testing.T) {
	e := newEnv(t)
	c := e.anon()
	body := map[string]string{"username": "nobody", "password": "whatever password"}
	for i := 0; i < 20; i++ {
		if code, out := c.do("POST", "/auth/login", body); code != 401 {
			t.Fatalf("attempt %d = %d %v", i+1, code, out)
		}
	}
	if code, out := c.do("POST", "/auth/login", body); code != 429 || codeOf(out) != "rate_limited" {
		t.Fatalf("21st attempt = %d %v", code, out)
	}
	e.clk.Advance(time.Minute)
	if code, _ := c.do("POST", "/auth/login", body); code != 401 {
		t.Fatalf("after a minute = %d", code)
	}
}

func TestAdminManagementRequiresSuperAndStepUp(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	newAdmin := map[string]any{"username": "alice", "password": adminPassword, "role": "ops"}
	if code, out := root.do("POST", "/admins", newAdmin); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("create without totp = %d %v", code, out)
	}
	newAdmin["totp"] = root.totp()
	code, out := root.do("POST", "/admins", newAdmin)
	if code != 201 || out["role"] != "ops" {
		t.Fatalf("create = %d %v", code, out)
	}
	aliceID := int64(out["id"].(float64))
	// 5 分钟内的后续高风险操作可以不带动态码
	if code, out := root.do("PUT", fmt.Sprintf("/admins/%d", aliceID), map[string]any{"status": "disabled"}); code != 200 || out["status"] != "disabled" {
		t.Fatalf("disable = %d %v", code, out)
	}
	if code, out := root.do("PUT", fmt.Sprintf("/admins/%d", root.admin.ID), map[string]any{"role": "ops"}); code != 409 || codeOf(out) != "last_super" {
		t.Fatalf("demote last super = %d %v", code, out)
	}
	if code, out := root.do("POST", fmt.Sprintf("/admins/%d/reset-totp", aliceID), map[string]any{}); code != 200 {
		t.Fatalf("reset totp = %d %v", code, out)
	}
	if code, out := root.do("GET", "/admins", nil); code != 200 || len(out["items"].([]any)) != 2 {
		t.Fatalf("list = %d %v", code, out)
	}
	if code, out := root.do("PUT", "/admins/abc", map[string]any{"role": "ops"}); code != 400 {
		t.Fatalf("bad id = %d %v", code, out)
	}

	ops := e.loginAs("bob", admin.RoleOps)
	if code, out := ops.do("GET", "/admins", nil); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops list admins = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/me", nil); code != 200 || out["role"] != "ops" {
		t.Fatalf("ops me = %d %v", code, out)
	}
	e.clk.Advance(admin.StepUpWindow)
	if code, out := root.do("PUT", fmt.Sprintf("/admins/%d", aliceID), map[string]any{"status": "active"}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("after step-up window = %d %v", code, out)
	}
}

func TestChangeOwnPassword(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	if code, out := root.do("POST", "/me/password", map[string]string{"old": "wrong password!", "new": "a brand new password"}); code != 400 || codeOf(out) != "wrong_password" {
		t.Fatalf("wrong old = %d %v", code, out)
	}
	if code, out := root.do("POST", "/me/password", map[string]string{"old": adminPassword, "new": "a brand new password"}); code != 200 {
		t.Fatalf("change = %d %v", code, out)
	}
	if code, _ := e.anon().do("POST", "/auth/login", map[string]string{"username": "root", "password": "a brand new password"}); code != 200 {
		t.Fatalf("login with new password = %d", code)
	}
}

func TestAdminWritesRequireJSON(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Post(e.ts.URL+"/admin/api/v1/auth/login", "text/plain", strings.NewReader(`{"username":"root"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("text/plain login = %d", resp.StatusCode)
	}
}
```

Run: `go test ./internal/adminapi/`
Expected: 编译失败（包不存在）。

- [ ] **Step 4: 实现后台 API 骨架**

`internal/adminapi/server.go`：

```go
// Package adminapi 提供运营后台 REST API。与用户 API 同一进程、不同端口，只应监听内网地址。
package adminapi

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/storage"
	"spark-cloud/internal/wallet"
)

const (
	prefix     = "/admin/api/v1"
	cookieName = "sc_admin"
)

type Server struct {
	Admins       *admin.Service
	Identity     *identity.Service
	Wallet       *wallet.Service
	Catalog      *catalog.Service
	Fleet        *fleet.Service
	Rental       *rental.Service
	Billing      *billing.Service
	Uploads      *storage.Service
	Clock        clock.Clock
	Log          *slog.Logger
	SecureCookie bool // 生产环境为 true：Cookie 仅经 HTTPS 发送
	TrustProxy   bool // 仅当部署在会改写转发头的反向代理之后才设为 true

	limiter *ipLimiter
	routes  []Route
}

// Route 是一个需要登录的后台接口及其允许的角色，供权限矩阵测试核对。
type Route struct {
	Method  string
	Pattern string
	Roles   []admin.Role
}

var (
	allRoles     = []admin.Role{admin.RoleSuper, admin.RoleOps, admin.RoleFinance}
	opsRoles     = []admin.Role{admin.RoleSuper, admin.RoleOps}
	financeRoles = []admin.Role{admin.RoleSuper, admin.RoleFinance}
	superOnly    = []admin.Role{admin.RoleSuper}
)

func (s *Server) Router() http.Handler {
	s.limiter = newIPLimiter(loginLimit, time.Minute, s.Clock)
	s.routes = nil
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	if s.TrustProxy {
		r.Use(middleware.RealIP)
	}
	r.Use(httpx.Recoverer(s.Log), httpx.NoStore, s.requireJSONForWrites)
	r.Route(prefix, func(r chi.Router) {
		r.With(s.rateLimit).Post("/auth/login", s.handleLogin)
		r.With(s.rateLimit).Post("/auth/totp/setup", s.handleTOTPSetup)
		r.With(s.rateLimit).Post("/auth/totp", s.handleTOTP)
		r.Post("/auth/logout", s.handleLogout)
		r.Group(func(g chi.Router) {
			g.Use(s.requireAdmin)
			s.accountRoutes(g)
		})
	})
	return r
}

// Routes 返回 Router 注册的全部需要登录的接口。
func (s *Server) Routes() []Route { return append([]Route(nil), s.routes...) }

// handle 注册一个需要登录的接口，并在处理器之前校验角色。
func (s *Server) handle(g chi.Router, method, pattern string, roles []admin.Role, h http.HandlerFunc) {
	s.routes = append(s.routes, Route{Method: method, Pattern: prefix + pattern, Roles: roles})
	g.With(s.requireRole(roles)).MethodFunc(method, pattern, h)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(s.Log, w, r, err)
}

// requireJSONForWrites 要求写请求使用 application/json；配合 SameSite=Strict Cookie 防止跨站请求。
func (s *Server) requireJSONForWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions &&
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			s.writeError(w, r, apperr.BadRequest("请求格式必须为 JSON"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type sessionKey struct{}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.Admins.Authenticate(r.Context(), sessionToken(r))
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, sess)))
	})
}

func (s *Server) requireRole(roles []admin.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !slices.Contains(roles, currentSession(r).Admin.Role) {
				s.writeError(w, r, apperr.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func currentSession(r *http.Request) admin.Session {
	return r.Context().Value(sessionKey{}).(admin.Session)
}

func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

// actor 返回当前请求的审计操作人。
func (s *Server) actor(r *http.Request) audit.Actor {
	return audit.Actor{AdminID: currentSession(r).Admin.ID, IP: httpx.ClientIP(r)}
}

// stepUp 校验高风险操作的动态码（请求体 totp 字段；5 分钟内验证过可留空）。
func (s *Server) stepUp(r *http.Request, code string) error {
	return s.Admins.StepUp(r.Context(), currentSession(r), code)
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.BadRequest("ID 不正确")
	}
	return id, nil
}

func pageOf(r *http.Request) (paging.Page, error) { return paging.Parse(r.URL.Query()) }
```

`internal/adminapi/ratelimit.go`：

```go
package adminapi

import (
	"net/http"
	"sync"
	"time"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/httpx"
)

const loginLimit = 20 // 同一 IP 每分钟最多 20 次登录相关请求

// ipLimiter 是进程内按 IP 的滑动窗口计数器（后台只在内网单实例部署）。
type ipLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	clock  clock.Clock
	hits   map[string][]time.Time
}

func newIPLimiter(limit int, window time.Duration, clk clock.Clock) *ipLimiter {
	return &ipLimiter{limit: limit, window: window, clock: clk, hits: map[string][]time.Time{}}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	cutoff := now.Add(-l.window)
	recent := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.limit {
		l.hits[ip] = recent
		return false
	}
	l.hits[ip] = append(recent, now)
	if len(l.hits) > 10_000 { // 防止大量不同来源撑大内存：清掉窗口外的记录
		for k, v := range l.hits {
			if len(v) == 0 || !v[len(v)-1].After(cutoff) {
				delete(l.hits, k)
			}
		}
	}
	return true
}

func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(httpx.ClientIP(r)) {
			s.writeError(w, r, apperr.ErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

`internal/adminapi/auth.go`：

```go
package adminapi

import (
	"net/http"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
)

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/admin/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.SecureCookie, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	res, err := s.Admins.Login(r.Context(), req.Username, req.Password, httpx.ClientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.setSessionCookie(w, res.Token, int(admin.SessionTTL/time.Second))
	next := "totp"
	if res.NeedTOTPSetup {
		next = "totp_setup"
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"next": next, "otpauth_uri": res.OTPAuthURI})
}

func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	a, codes, err := s.Admins.ConfirmTOTPSetup(r.Context(), sessionToken(r), req.Code, httpx.ClientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"admin": a, "recovery_codes": codes})
}

func (s *Server) handleTOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code         string `json:"code"`
		RecoveryCode string `json:"recovery_code"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if (req.Code == "") == (req.RecoveryCode == "") {
		s.writeError(w, r, apperr.BadRequest("请填写动态码或恢复码（二选一）"))
		return
	}
	var a admin.Admin
	var err error
	if req.Code != "" {
		a, err = s.Admins.VerifyTOTP(r.Context(), sessionToken(r), req.Code, httpx.ClientIP(r))
	} else {
		a, err = s.Admins.VerifyRecoveryCode(r.Context(), sessionToken(r), req.RecoveryCode, httpx.ClientIP(r))
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"admin": a})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := sessionToken(r); token != "" {
		if err := s.Admins.Logout(r.Context(), token); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	s.setSessionCookie(w, "", -1)
	httpx.OK(w)
}
```

`internal/adminapi/admins.go`：

```go
package adminapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/platform/httpx"
)

func (s *Server) accountRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/me", allRoles, s.handleMe)
	s.handle(g, http.MethodPost, "/me/password", allRoles, s.handleChangePassword)
	s.handle(g, http.MethodGet, "/admins", superOnly, s.handleListAdmins)
	s.handle(g, http.MethodPost, "/admins", superOnly, s.handleCreateAdmin)
	s.handle(g, http.MethodPut, "/admins/{id}", superOnly, s.handleUpdateAdmin)
	s.handle(g, http.MethodPost, "/admins/{id}/reset-totp", superOnly, s.handleResetTOTP)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, currentSession(r).Admin)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Admins.ChangePassword(r.Context(), currentSession(r), req.Old, req.New, httpx.ClientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleListAdmins(w http.ResponseWriter, r *http.Request) {
	list, err := s.Admins.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleCreateAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
		TOTP     string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.stepUp(r, req.TOTP); err != nil {
		s.writeError(w, r, err)
		return
	}
	a, err := s.Admins.Create(r.Context(), s.actor(r), req.Username, req.Password, admin.Role(req.Role))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, a)
}

func (s *Server) handleUpdateAdmin(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Role   *string `json:"role"`
		Status *string `json:"status"`
		TOTP   string  `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.stepUp(r, req.TOTP); err != nil {
		s.writeError(w, r, err)
		return
	}
	var role *admin.Role
	if req.Role != nil {
		v := admin.Role(*req.Role)
		role = &v
	}
	a, err := s.Admins.Update(r.Context(), s.actor(r), id, role, req.Status)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}

func (s *Server) handleResetTOTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		TOTP string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.stepUp(r, req.TOTP); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Admins.ResetTOTP(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}
```

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/adminapi/ ./internal/api/ ./internal/platform/httpx/`
Expected: PASS

- [ ] **Step 6: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/platform/httpx internal/api internal/adminapi
git rm internal/api/recover_test.go
git commit -m "feat(adminapi): admin API skeleton with TOTP login, role checks, step-up and admin management

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 16: 后台：机器、PDU、告警（含 CSV 批量导入）

**Files:**
- Create: `migrations/00016_admin_indexes.sql`
- Modify: `internal/platform/db/db.go`（新增 `Where`）、`internal/platform/db/db_test.go`
- Create: `internal/fleet/admin.go`、`internal/fleet/admin_test.go`
- Modify: `internal/catalog/catalog.go`（`ProductIDByCode`）
- Modify: `internal/rental/queries.go`（`ListInstancesByMachine`）
- Create: `internal/adminapi/views.go`、`internal/adminapi/machines.go`、`internal/adminapi/machines_test.go`
- Modify: `internal/adminapi/server.go`（注册路由）、`internal/adminapi/helpers_test.go`（追加租用辅助）

**Interfaces:**
- Consumes: Task 11 的 `fleet.register`、`PDUInput`、`CreatePDU`、`UpdatePDU`、`ListPDUs`、`PowerCycle`、`PDUIDByName`；Task 12 的 `rental.SetMachineMaintenance`；Task 15 的 `handle`、`actor`、`pathID`、`pageOf`、`writeError`。
- Produces（Task 17～21 使用）：
  - `db.Where`：`(*Where).Add(cond string, args ...any)`（`cond` 中的 `?` 依次替换为 `$n`）、`(*Where).SQL() string`（无条件时为 `TRUE`）、字段 `Args []any`。
  - `fleet.MachineFilter{State string; Online *bool; Rack string}`、`(*fleet.Service).ListMachines(ctx, f, p paging.Page) ([]Machine, int, error)`、`RegisterAudited(ctx, actor, nm NewMachine) (Machine, error)`、`fleet.ImportRow{Line int; Machine NewMachine}`、`fleet.RowError{Line int; Message string}`、`*fleet.ImportError{Rows []RowError}`、`fleet.MaxImportRows = 1000`、`Import(ctx, actor, rows []ImportRow, preErrors []RowError) ([]Machine, error)`、`fleet.AlertRow`、`ListAlerts(ctx, resolved *bool, p paging.Page) ([]AlertRow, int, error)`、`ResolveAlert(ctx, actor, id int64) error`、`fleet.MachineStats{ByState map[string]int; Online int; Total int}`、`Stats(ctx) (MachineStats, error)`。
  - `(*catalog.Service).ProductIDByCode(ctx, code string) (int64, error)`。
  - `(*rental.Service).ListInstancesByMachine(ctx, machineID int64, limit int) ([]Instance, error)`。
  - adminapi 包内：`instanceView`、`(*Server).instanceView(rental.Instance) instanceView`、`(*Server).instanceViews([]rental.Instance) []instanceView`、`(*Server).machineRoutes(g)`。
  - 接口：`GET /machines?state=&online=&rack=&page=&page_size=`、`POST /machines`、`POST /machines/import {csv}`、`GET /machines/{id}` → `{machine, current_instance, history}`、`POST /machines/{id}/maintenance {on}`、`POST /machines/{id}/power-cycle`、`GET /pdus`、`POST /pdus`、`PUT /pdus/{id}`、`GET /alerts?resolved=&page=`、`POST /alerts/{id}/resolve`。
  - 测试辅助：`(*env).renter(phone string) int64`（已实名、余额 100 万分的用户）、`(*env).machine(serial string) int64`（空闲在线机器）、`(*env).place(uid int64) rental.Instance`、`(*env).ack()`（模拟 Agent 成功执行所有待处理指令）。

CSV 导入格式（UTF-8，可带 BOM，首行必须是表头）：`serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet`；`pdu_name` 与 `pdu_outlet` 可同时留空。整批校验：任何一行有错就整批不导入，返回 400 `{code: "import_invalid", message, request_id, errors: [{line, message}]}`（行号从 1 开始，表头为第 1 行）。

- [ ] **Step 1: 迁移与 SQL 条件拼接**

`migrations/00016_admin_indexes.sql`：

```sql
-- +goose Up
ALTER TABLE alerts ADD COLUMN resolved_by BIGINT;  -- 处理告警的管理员
CREATE INDEX alerts_created_idx ON alerts (created_at DESC);
CREATE INDEX machines_state_idx ON machines (state, rack, slot);
CREATE INDEX instances_machine_idx ON instances (machine_id, created_at DESC);
CREATE INDEX instances_created_idx ON instances (created_at DESC);
CREATE INDEX orders_created_idx ON orders (created_at DESC);
CREATE INDEX wallet_ledger_created_idx ON wallet_ledger (created_at DESC);
CREATE INDEX audit_logs_created_idx ON audit_logs (created_at DESC);
CREATE INDEX audit_logs_actor_idx ON audit_logs (actor_id, created_at DESC);
CREATE INDEX users_created_idx ON users (created_at DESC);

-- +goose Down
DROP INDEX users_created_idx;
DROP INDEX audit_logs_actor_idx;
DROP INDEX audit_logs_created_idx;
DROP INDEX wallet_ledger_created_idx;
DROP INDEX orders_created_idx;
DROP INDEX instances_created_idx;
DROP INDEX instances_machine_idx;
DROP INDEX machines_state_idx;
DROP INDEX alerts_created_idx;
ALTER TABLE alerts DROP COLUMN resolved_by;
```

在 `internal/platform/db/db_test.go` 末尾追加：

```go
func TestWhere(t *testing.T) {
	var w db.Where
	if w.SQL() != "TRUE" || len(w.Args) != 0 {
		t.Fatalf("empty = %q %v", w.SQL(), w.Args)
	}
	w.Add("state = ?", "idle")
	w.Add("created_at >= ? AND created_at < ?", 1, 2)
	if got := w.SQL(); got != "state = $1 AND created_at >= $2 AND created_at < $3" || len(w.Args) != 3 {
		t.Fatalf("sql = %q args = %v", got, w.Args)
	}
}
```

在 `internal/platform/db/db.go` 末尾追加（import 增加 `"strconv"`、`"strings"`）：

```go
// Where 拼接带参数的 AND 条件；条件中的 ? 依次替换为 $1、$2……，用于后台列表的可选筛选。
type Where struct {
	conds []string
	Args  []any
}

func (w *Where) Add(cond string, args ...any) {
	var b strings.Builder
	i := 0
	for _, r := range cond {
		if r == '?' && i < len(args) {
			w.Args = append(w.Args, args[i])
			b.WriteString("$" + strconv.Itoa(len(w.Args)))
			i++
			continue
		}
		b.WriteRune(r)
	}
	w.conds = append(w.conds, b.String())
}

func (w *Where) SQL() string {
	if len(w.conds) == 0 {
		return "TRUE"
	}
	return strings.Join(w.conds, " AND ")
}
```

Run: `go test ./internal/platform/db/ -run Where`
Expected: PASS

- [ ] **Step 2: 写 fleet 后台查询的失败测试**

`internal/fleet/admin_test.go`：

```go
package fleet_test

import (
	"context"
	"errors"
	"testing"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
)

func TestListMachinesFilters(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	dbtest.CreateMachine(t, pool, "SN-IDLE") // idle、在线、rack 为空
	if _, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN-A1", Rack: "A01", LanIP: "10.0.1.1", ProductID: pid}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN-A2", Rack: "A01", LanIP: "10.0.1.2", ProductID: pid}); err != nil {
		t.Fatal(err)
	}
	all, total, err := svc.ListMachines(ctx, fleet.MachineFilter{}, paging.Page{Number: 1, Size: 2})
	if err != nil || total != 3 || len(all) != 2 {
		t.Fatalf("all = %d/%d, %v", len(all), total, err)
	}
	on := true
	idle, total, _ := svc.ListMachines(ctx, fleet.MachineFilter{State: "idle", Online: &on}, paging.Page{})
	if total != 1 || idle[0].Serial != "SN-IDLE" {
		t.Fatalf("idle online = %+v", idle)
	}
	rack, total, _ := svc.ListMachines(ctx, fleet.MachineFilter{Rack: "A01", State: "maintenance"}, paging.Page{})
	if total != 2 || rack[0].Serial != "SN-A1" {
		t.Fatalf("rack = %+v", rack)
	}
	if _, _, err := svc.ListMachines(ctx, fleet.MachineFilter{State: "broken"}, paging.Page{}); codeOf(err) != "bad_request" {
		t.Fatalf("bad state = %v", err)
	}
	stats, err := svc.Stats(ctx)
	if err != nil || stats.Total != 3 || stats.Online != 1 || stats.ByState["maintenance"] != 2 || stats.ByState["idle"] != 1 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
}

func TestImportIsAllOrNothing(t *testing.T) {
	svc, pool, _ := newEnv(t)
	configurePDU(t, svc, true)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	p, _ := svc.CreatePDU(ctx, ops, snmpInput("PDU-A"))
	two := 2
	rows := []fleet.ImportRow{
		{Line: 2, Machine: fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: pid}},
		{Line: 3, Machine: fleet.NewMachine{Serial: "SN2", LanIP: "10.0.1.2", ProductID: pid, PDUID: &p.ID, PDUOutlet: &two}},
	}
	bad := append(rows,
		fleet.ImportRow{Line: 4, Machine: fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.3", ProductID: pid}},                          // 同批重复
		fleet.ImportRow{Line: 5, Machine: fleet.NewMachine{Serial: "SN3", LanIP: "10.0.1.4", ProductID: pid, PDUID: &p.ID, PDUOutlet: &two}}, // 插座已占用
		fleet.ImportRow{Line: 6, Machine: fleet.NewMachine{Serial: "SN4", LanIP: "bad", ProductID: pid}},
	)
	_, err := svc.Import(ctx, ops, bad, []fleet.RowError{{Line: 7, Message: "商品 spark-9 不存在"}})
	var ie *fleet.ImportError
	if !errors.As(err, &ie) || len(ie.Rows) != 4 || ie.Rows[0].Line != 4 || ie.Rows[3].Line != 7 {
		t.Fatalf("import errors = %+v (%v)", ie, err)
	}
	if _, total, _ := svc.ListMachines(ctx, fleet.MachineFilter{}, paging.Page{}); total != 0 {
		t.Fatalf("partial import: %d machines", total)
	}
	got, err := svc.Import(ctx, ops, rows, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("import = %d, %v", len(got), err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'machine.import' AND (diff->>'count')::int = 2`).Scan(&n)
	if n != 1 {
		t.Fatalf("import audit rows = %d", n)
	}
	if _, err := svc.Import(ctx, ops, nil, nil); codeOf(err) != "bad_request" {
		t.Fatalf("empty import = %v", err)
	}
}

func TestRegisterAudited(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	m, err := svc.RegisterAudited(ctx, ops, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: dbtest.ProductID(t, pool)})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'machine.register' AND target_id = $1::text`, m.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
}

func TestAlertsListAndResolve(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN1")
	for _, kind := range []string{"a", "b", "c"} {
		if err := svc.RaiseAlert(ctx, pool, fleet.Alert{Level: "warning", Kind: kind, MachineID: mid, Message: kind}); err != nil {
			t.Fatal(err)
		}
	}
	open := false
	list, total, err := svc.ListAlerts(ctx, &open, paging.Page{})
	if err != nil || total != 3 || list[0].MachineID == nil || *list[0].MachineID != mid {
		t.Fatalf("open alerts = %+v, %d, %v", list, total, err)
	}
	if err := svc.ResolveAlert(ctx, ops, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResolveAlert(ctx, ops, list[0].ID); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("resolve twice = %v", err)
	}
	if err := svc.ResolveAlert(ctx, ops, 9999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("resolve missing = %v", err)
	}
	done := true
	resolved, total, _ := svc.ListAlerts(ctx, &done, paging.Page{})
	if total != 1 || resolved[0].ResolvedBy == nil || *resolved[0].ResolvedBy != ops.AdminID || resolved[0].ResolvedAt == nil {
		t.Fatalf("resolved = %+v", resolved)
	}
	if _, total, _ := svc.ListAlerts(ctx, nil, paging.Page{}); total != 3 {
		t.Fatalf("all alerts = %d", total)
	}
}
```

Run: `go test ./internal/fleet/ -run 'ListMachines|Import|RegisterAudited|Alerts'`
Expected: 编译失败（`ListMachines` 等未定义）。

- [ ] **Step 3: 实现 fleet 后台查询**

`internal/fleet/admin.go`：

```go
package fleet

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type MachineFilter struct {
	State  string
	Online *bool
	Rack   string
}

var machineStates = []string{StateIdle, StateAllocated, StateMaintenance, StateFaulty}

// ListMachines 按机柜、槽位排序分页返回机器。
func (s *Service) ListMachines(ctx context.Context, f MachineFilter, p paging.Page) ([]Machine, int, error) {
	var w db.Where
	if f.State != "" {
		if !slices.Contains(machineStates, f.State) {
			return nil, 0, apperr.BadRequest("机器状态不正确")
		}
		w.Add("state = ?", f.State)
	}
	if f.Online != nil {
		w.Add("agent_online = ?", *f.Online)
	}
	if f.Rack != "" {
		w.Add("rack = ?", f.Rack)
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM machines WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.pool.Query(ctx, `SELECT `+machineCols+` FROM machines WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY rack, slot, id LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Machine, error) { return scanMachine(r) })
	return list, total, err
}

// RegisterAudited 后台单台登记：登记并写审计。
func (s *Service) RegisterAudited(ctx context.Context, actor audit.Actor, nm NewMachine) (Machine, error) {
	var m Machine
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if m, err = s.register(ctx, tx, nm); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.clock.Now(), actor.Entry("machine.register", "machine", strconv.FormatInt(m.ID, 10),
			map[string]any{"serial": m.Serial, "rack": m.Rack, "slot": m.Slot, "lan_ip": m.LanIP}))
	})
	return m, err
}

const MaxImportRows = 1000

type ImportRow struct {
	Line    int
	Machine NewMachine
}

type RowError struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// ImportError 表示批量导入中有行出错（整批未导入）。
type ImportError struct {
	Rows []RowError
}

func (e *ImportError) Error() string { return fmt.Sprintf("导入数据有 %d 行错误", len(e.Rows)) }

// Import 整批登记机器：每行在保存点内插入以收集全部行错误；任一行出错（含调用方预先发现的 preErrors）则整批回滚。
func (s *Service) Import(ctx context.Context, actor audit.Actor, rows []ImportRow, preErrors []RowError) ([]Machine, error) {
	if len(rows)+len(preErrors) == 0 {
		return nil, apperr.BadRequest("没有可导入的机器")
	}
	if len(rows)+len(preErrors) > MaxImportRows {
		return nil, apperr.BadRequest(fmt.Sprintf("单次最多导入 %d 台", MaxImportRows))
	}
	var out []Machine
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		rowErrs := append([]RowError(nil), preErrors...)
		for _, row := range rows {
			sp, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			m, err := s.register(ctx, sp, row.Machine)
			if err != nil {
				_ = sp.Rollback(ctx)
				ae := apperr.As(err)
				if ae == nil {
					return err
				}
				rowErrs = append(rowErrs, RowError{Line: row.Line, Message: ae.Message})
				continue
			}
			if err := sp.Commit(ctx); err != nil {
				return err
			}
			out = append(out, m)
		}
		if len(rowErrs) > 0 {
			sort.SliceStable(rowErrs, func(i, j int) bool { return rowErrs[i].Line < rowErrs[j].Line })
			return &ImportError{Rows: rowErrs}
		}
		serials := make([]string, len(out))
		for i, m := range out {
			serials[i] = m.Serial
		}
		return audit.Record(ctx, tx, s.clock.Now(), actor.Entry("machine.import", "machine", "",
			map[string]any{"count": len(out), "serials": serials}))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type MachineStats struct {
	ByState map[string]int `json:"by_state"`
	Online  int            `json:"online"`
	Total   int            `json:"total"`
}

func (s *Service) Stats(ctx context.Context) (MachineStats, error) {
	st := MachineStats{ByState: map[string]int{}}
	rows, err := s.pool.Query(ctx, `SELECT state, count(*), count(*) FILTER (WHERE agent_online) FROM machines GROUP BY state`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n, online int
		if err := rows.Scan(&state, &n, &online); err != nil {
			return st, err
		}
		st.ByState[state] = n
		st.Total += n
		st.Online += online
	}
	return st, rows.Err()
}

type AlertRow struct {
	ID         int64      `json:"id"`
	Level      string     `json:"level"`
	Kind       string     `json:"kind"`
	MachineID  *int64     `json:"machine_id"`
	InstanceID string     `json:"instance_id"`
	Message    string     `json:"message"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
	ResolvedBy *int64     `json:"resolved_by"`
}

// ListAlerts 按时间倒序分页返回告警；resolved 为 nil 表示不按处理状态筛选。
func (s *Service) ListAlerts(ctx context.Context, resolved *bool, p paging.Page) ([]AlertRow, int, error) {
	cond := "TRUE"
	if resolved != nil && *resolved {
		cond = "resolved_at IS NOT NULL"
	} else if resolved != nil {
		cond = "resolved_at IS NULL"
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE `+cond).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, level, kind, machine_id, instance_id, message, created_at, resolved_at, resolved_by
		FROM alerts WHERE `+cond+` ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`, p.Limit(), p.Offset())
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlertRow, error) {
		var a AlertRow
		err := r.Scan(&a.ID, &a.Level, &a.Kind, &a.MachineID, &a.InstanceID, &a.Message, &a.CreatedAt, &a.ResolvedAt, &a.ResolvedBy)
		return a, err
	})
	return list, total, err
}

// ResolveAlert 把告警标记为已处理；已处理过返回 invalid_state。
func (s *Service) ResolveAlert(ctx context.Context, actor audit.Actor, id int64) error {
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var resolvedAt *time.Time
		err := tx.QueryRow(ctx, `SELECT resolved_at FROM alerts WHERE id = $1 FOR UPDATE`, id).Scan(&resolvedAt)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if resolvedAt != nil {
			return apperr.ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `UPDATE alerts SET resolved_at = $2, resolved_by = $3 WHERE id = $1`, id, now, actor.AdminID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("alert.resolve", "alert", strconv.FormatInt(id, 10), nil))
	})
}
```

`internal/catalog/catalog.go` 末尾追加：

```go
// ProductIDByCode 按商品编码查 ID（批量导入机器时使用）。
func (s *Service) ProductIDByCode(ctx context.Context, code string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM products WHERE code = $1`, code).Scan(&id)
	if db.IsNoRows(err) {
		return 0, apperr.ErrNotFound
	}
	return id, err
}
```

`internal/rental/queries.go` 末尾追加：

```go
// ListInstancesByMachine 返回某台机器上最近的实例（新到旧）。
func (s *Service) ListInstancesByMachine(ctx context.Context, machineID int64, limit int) ([]Instance, error) {
	return collectInstances(s.d.Pool.Query(ctx, `SELECT `+instanceCols+` FROM instances
		WHERE machine_id = $1 ORDER BY created_at DESC, id LIMIT $2`, machineID, limit))
}
```

Run: `go test ./internal/fleet/ ./internal/catalog/ ./internal/rental/`
Expected: PASS

- [ ] **Step 4: 写后台接口的失败测试**

在 `internal/adminapi/helpers_test.go` 末尾追加（import 增加 `"fmt"`、`"spark-cloud/internal/catalog"` 已有）：

```go
// renter 创建一个已实名、余额 100 万分的用户。
func (e *env) renter(phone string) int64 {
	e.t.Helper()
	uid := dbtest.CreateUser(e.t, e.pool, phone, true)
	dbtest.Fund(e.t, e.pool, uid, 1_000_000)
	return uid
}

// machine 创建一台空闲、在线的机器。
func (e *env) machine(serial string) int64 {
	e.t.Helper()
	return dbtest.CreateMachine(e.t, e.pool, serial)
}

func (e *env) place(uid int64) rental.Instance {
	e.t.Helper()
	_, inst, err := e.rental.Place(e.ctx, rental.PlaceRequest{UserID: uid, ProductID: dbtest.ProductID(e.t, e.pool),
		ImageID: dbtest.ImageID(e.t, e.pool), Duration: catalog.D1d})
	if err != nil {
		e.t.Fatalf("place: %v", err)
	}
	return inst
}

// ack 模拟 Agent：成功执行所有机器上待处理的指令。
func (e *env) ack() {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT id FROM machines ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, mid := range ids {
		cmds, err := e.fleet.PendingCommands(e.ctx, mid)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, c := range cmds {
			if err := e.rental.HandleCommandResult(e.ctx, c.ID, true, "ok"); err != nil {
				e.t.Fatal(fmt.Errorf("ack %s: %w", c.Type, err))
			}
		}
	}
}
```

`internal/adminapi/machines_test.go`：

```go
package adminapi_test

import (
	"fmt"
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
)

func TestRegisterAndImportMachines(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	pid := dbtest.ProductID(t, e.pool)
	code, out := ops.do("POST", "/machines", map[string]any{"serial": "SN1", "rack": "A01", "slot": "01", "lan_ip": "10.0.1.1", "product_id": pid})
	if code != 201 || out["state"] != "maintenance" {
		t.Fatalf("register = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/machines", map[string]any{"serial": "SN1", "lan_ip": "10.0.1.9", "product_id": pid}); code != 409 || codeOf(out) != "duplicate_serial" {
		t.Fatalf("duplicate = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/pdus", map[string]any{"name": "PDU-A", "driver": "snmp", "address": "10.0.9.1", "community": "private"}); code != 201 || out["has_community"] != true {
		t.Fatalf("create pdu = %d %v", code, out)
	}

	bad := "﻿serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet\n" +
		"SN4,A01,04,10.0.1.4,spark-9,,\n" +
		"SN5,A01,05,bad-ip,spark-1,,\n" +
		"SN1,A01,06,10.0.1.6,spark-1,,\n" +
		"SN7,A01,08,10.0.1.8,spark-1,PDU-X,1\n" +
		"SN8,A01,09,10.0.1.9,spark-1,PDU-A,x\n" +
		"SN9,A01,10,10.0.1.10,spark-1,PDU-A,3\n"
	code, out = ops.do("POST", "/machines/import", map[string]string{"csv": bad})
	if code != 400 || codeOf(out) != "import_invalid" {
		t.Fatalf("bad import = %d %v", code, out)
	}
	var lines []int
	for _, it := range out["errors"].([]any) {
		lines = append(lines, int(it.(map[string]any)["line"].(float64)))
	}
	if fmt.Sprint(lines) != "[2 3 4 5 6]" {
		t.Fatalf("error lines = %v (%v)", lines, out["errors"])
	}
	if _, out := ops.do("GET", "/machines", nil); out["total"].(float64) != 1 {
		t.Fatalf("bad import must not import anything: %v", out)
	}

	good := "serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet\n" +
		"SN2,A01,02,10.0.1.2,spark-1,PDU-A,2\n" +
		"SN3,A01,03,10.0.1.3,spark-1,,\n"
	if code, out := ops.do("POST", "/machines/import", map[string]string{"csv": good}); code != 201 || len(out["items"].([]any)) != 2 {
		t.Fatalf("good import = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/machines/import", map[string]string{"csv": "sn,rack\nSN1,A"}); code != 400 || codeOf(out) != "bad_request" {
		t.Fatalf("bad header = %d %v", code, out)
	}
}

func TestMachineListDetailMaintenanceAndPowerCycle(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	busy := e.machine("SN-BUSY")
	e.machine("SN-IDLE")
	inst := e.place(e.renter("13600000001"))
	e.ack()

	if code, out := ops.do("GET", "/machines?state=allocated", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("allocated = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/machines?online=maybe", nil); code != 400 {
		t.Fatalf("bad online filter = %d %v", code, out)
	}
	code, out := ops.do("GET", fmt.Sprintf("/machines/%d", busy), nil)
	if code != 200 || out["current_instance"].(map[string]any)["id"] != inst.ID || out["machine"].(map[string]any)["serial"] != "SN-BUSY" {
		t.Fatalf("detail = %d %v", code, out)
	}
	if code, _ := ops.do("GET", "/machines/9999", nil); code != 404 {
		t.Fatalf("missing machine = %d", code)
	}
	idle := int64(0)
	_ = e.pool.QueryRow(e.ctx, `SELECT id FROM machines WHERE serial = 'SN-IDLE'`).Scan(&idle)
	if code, out := ops.do("POST", fmt.Sprintf("/machines/%d/maintenance", idle), map[string]bool{"on": true}); code != 200 || out["state"] != "maintenance" {
		t.Fatalf("maintenance on = %d %v", code, out)
	}
	if code, out := ops.do("POST", fmt.Sprintf("/machines/%d/maintenance", idle), map[string]bool{"on": false}); code != 200 || out["state"] != "idle" {
		t.Fatalf("maintenance off = %d %v", code, out)
	}
	if code, out := ops.do("POST", fmt.Sprintf("/machines/%d/power-cycle", busy), map[string]any{}); code != 400 {
		t.Fatalf("power-cycle without pdu = %d %v", code, out)
	}
	_, pdu := ops.do("POST", "/pdus", map[string]any{"name": "PDU-A", "driver": "mock", "address": "127.0.0.1"})
	pduID := int64(pdu["id"].(float64))
	if code, out := ops.do("PUT", fmt.Sprintf("/pdus/%d", pduID), map[string]any{"name": "PDU-A1", "driver": "mock", "address": "127.0.0.1"}); code != 200 || out["name"] != "PDU-A1" {
		t.Fatalf("update pdu = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/pdus", nil); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("list pdus = %d %v", code, out)
	}
	five := 5
	m, err := e.fleet.Register(e.ctx, fleet.NewMachine{Serial: "SN-PDU", LanIP: "10.0.1.50", ProductID: dbtest.ProductID(t, e.pool), PDUID: &pduID, PDUOutlet: &five})
	if err != nil {
		t.Fatal(err)
	}
	if code, out := ops.do("POST", fmt.Sprintf("/machines/%d/power-cycle", m.ID), map[string]any{}); code != 200 {
		t.Fatalf("power-cycle = %d %v", code, out)
	}
	if calls := e.pdu.Calls(); len(calls) != 1 || calls[0] != "cycle:5" {
		t.Fatalf("pdu calls = %v", calls)
	}

	fin := e.loginAs("fin", admin.RoleFinance)
	if code, out := fin.do("GET", "/machines", nil); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance machines = %d %v", code, out)
	}
}

func TestAlertsEndpoints(t *testing.T) {
	e := newEnv(t)
	fin := e.loginAs("fin", admin.RoleFinance)
	mid := e.machine("SN1")
	if err := e.fleet.RaiseAlert(e.ctx, e.pool, fleet.Alert{Level: "critical", Kind: "wipe_failed", MachineID: mid, Message: "x"}); err != nil {
		t.Fatal(err)
	}
	code, out := fin.do("GET", "/alerts?resolved=false", nil)
	if code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("alerts = %d %v", code, out)
	}
	id := int64(out["items"].([]any)[0].(map[string]any)["id"].(float64))
	if code, _ := fin.do("POST", fmt.Sprintf("/alerts/%d/resolve", id), map[string]any{}); code != 200 {
		t.Fatalf("resolve = %d", code)
	}
	if code, out := fin.do("POST", fmt.Sprintf("/alerts/%d/resolve", id), map[string]any{}); code != 409 {
		t.Fatalf("resolve twice = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/alerts?resolved=true", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("resolved alerts = %d %v", code, out)
	}
}
```

Run: `go test ./internal/adminapi/ -run 'Machines|MachineList|Alerts'`
Expected: 失败（路由不存在，返回 404）。

- [ ] **Step 5: 实现后台接口**

`internal/adminapi/views.go`：

```go
package adminapi

import (
	"time"

	"spark-cloud/internal/rental"
)

// instanceView 是后台展示的实例信息（不含 SSH 密码）。
type instanceView struct {
	ID               string     `json:"id"`
	UserID           int64      `json:"user_id"`
	MachineID        int64      `json:"machine_id"`
	ProductID        int64      `json:"product_id"`
	ImageID          int64      `json:"image_id"`
	State            string     `json:"state"`
	Suspended        bool       `json:"suspended"`
	PendingSeconds   int64      `json:"pending_seconds"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	ExpiresAt        *time.Time `json:"expires_at"`
	StoppedAt        *time.Time `json:"stopped_at"`
	CreatedAt        time.Time  `json:"created_at"`
}

func (s *Server) instanceView(i rental.Instance) instanceView {
	return instanceView{ID: i.ID, UserID: i.UserID, MachineID: i.MachineID, ProductID: i.ProductID, ImageID: i.ImageID,
		State: i.State, Suspended: i.Suspended(), PendingSeconds: i.PendingSeconds,
		RemainingSeconds: int64(i.Remaining(s.Clock.Now()) / time.Second), ExpiresAt: i.ExpiresAt, StoppedAt: i.StoppedAt, CreatedAt: i.CreatedAt}
}

func (s *Server) instanceViews(list []rental.Instance) []instanceView {
	out := make([]instanceView, 0, len(list))
	for _, i := range list {
		out = append(out, s.instanceView(i))
	}
	return out
}
```

`internal/adminapi/machines.go`：

```go
package adminapi

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/rental"
)

func (s *Server) machineRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/machines", opsRoles, s.handleListMachines)
	s.handle(g, http.MethodPost, "/machines", opsRoles, s.handleRegisterMachine)
	s.handle(g, http.MethodPost, "/machines/import", opsRoles, s.handleImportMachines)
	s.handle(g, http.MethodGet, "/machines/{id}", opsRoles, s.handleMachine)
	s.handle(g, http.MethodPost, "/machines/{id}/maintenance", opsRoles, s.handleMaintenance)
	s.handle(g, http.MethodPost, "/machines/{id}/power-cycle", opsRoles, s.handlePowerCycle)
	s.handle(g, http.MethodGet, "/pdus", opsRoles, s.handleListPDUs)
	s.handle(g, http.MethodPost, "/pdus", opsRoles, s.handleCreatePDU)
	s.handle(g, http.MethodPut, "/pdus/{id}", opsRoles, s.handleUpdatePDU)
	s.handle(g, http.MethodGet, "/alerts", allRoles, s.handleListAlerts)
	s.handle(g, http.MethodPost, "/alerts/{id}/resolve", allRoles, s.handleResolveAlert)
}

// parseBoolParam 解析可选的 true / false 查询参数。
func parseBoolParam(v, name string) (*bool, error) {
	switch v {
	case "":
		return nil, nil
	case "true":
		b := true
		return &b, nil
	case "false":
		b := false
		return &b, nil
	}
	return nil, apperr.BadRequest(name + " 只能是 true 或 false")
}

func (s *Server) handleListMachines(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	online, err := parseBoolParam(q.Get("online"), "online")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Fleet.ListMachines(r.Context(), fleet.MachineFilter{State: q.Get("state"), Online: online, Rack: q.Get("rack")}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

type machineRequest struct {
	Serial    string `json:"serial"`
	Rack      string `json:"rack"`
	Slot      string `json:"slot"`
	LanIP     string `json:"lan_ip"`
	ProductID int64  `json:"product_id"`
	PDUID     *int64 `json:"pdu_id"`
	PDUOutlet *int   `json:"pdu_outlet"`
}

func (s *Server) handleRegisterMachine(w http.ResponseWriter, r *http.Request) {
	var req machineRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.Fleet.RegisterAudited(r.Context(), s.actor(r), fleet.NewMachine{Serial: req.Serial, Rack: req.Rack, Slot: req.Slot,
		LanIP: req.LanIP, ProductID: req.ProductID, PDUID: req.PDUID, PDUOutlet: req.PDUOutlet})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, m)
}

var importHeader = []string{"serial", "rack", "slot", "lan_ip", "product_code", "pdu_name", "pdu_outlet"}

// parseMachineCSV 解析导入文件；能在解析阶段发现的错误（未知商品、未知 PDU、插座号格式）作为行错误返回。
func (s *Server) parseMachineCSV(ctx context.Context, text string) ([]fleet.ImportRow, []fleet.RowError, error) {
	rd := csv.NewReader(strings.NewReader(strings.TrimPrefix(text, "﻿")))
	rd.FieldsPerRecord = -1
	rd.TrimLeadingSpace = true
	header, err := rd.Read()
	if err != nil || len(header) != len(importHeader) {
		return nil, nil, apperr.BadRequest("表头必须是 " + strings.Join(importHeader, ","))
	}
	for i, h := range header {
		if strings.ToLower(strings.TrimSpace(h)) != importHeader[i] {
			return nil, nil, apperr.BadRequest("表头必须是 " + strings.Join(importHeader, ","))
		}
	}
	products := map[string]int64{}
	pdus := map[string]int64{}
	var rows []fleet.ImportRow
	var rowErrs []fleet.RowError
	for line := 2; ; line++ {
		rec, err := rd.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		fail := func(msg string) { rowErrs = append(rowErrs, fleet.RowError{Line: line, Message: msg}) }
		if err != nil || len(rec) != len(importHeader) {
			fail("列数不正确或格式错误")
			continue
		}
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}
		code := rec[4]
		pid, ok := products[code]
		if !ok {
			pid, err = s.Catalog.ProductIDByCode(ctx, code)
			if errors.Is(err, apperr.ErrNotFound) {
				fail("商品 " + code + " 不存在")
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			products[code] = pid
		}
		nm := fleet.NewMachine{Serial: rec[0], Rack: rec[1], Slot: rec[2], LanIP: rec[3], ProductID: pid}
		if rec[5] != "" || rec[6] != "" {
			id, ok := pdus[rec[5]]
			if !ok {
				id, err = s.Fleet.PDUIDByName(ctx, s.Fleet.Pool(), rec[5])
				if errors.Is(err, apperr.ErrNotFound) {
					fail("PDU " + rec[5] + " 不存在")
					continue
				}
				if err != nil {
					return nil, nil, err
				}
				pdus[rec[5]] = id
			}
			outlet, err := strconv.Atoi(rec[6])
			if err != nil {
				fail("PDU 插座号必须是数字")
				continue
			}
			nm.PDUID, nm.PDUOutlet = &id, &outlet
		}
		rows = append(rows, fleet.ImportRow{Line: line, Machine: nm})
	}
	return rows, rowErrs, nil
}

func (s *Server) handleImportMachines(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CSV string `json:"csv"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	rows, rowErrs, err := s.parseMachineCSV(r.Context(), req.CSV)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	machines, err := s.Fleet.Import(r.Context(), s.actor(r), rows, rowErrs)
	var ie *fleet.ImportError
	if errors.As(err, &ie) {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"code": "import_invalid", "message": "导入数据有误，未导入任何机器", "request_id": middleware.GetReqID(r.Context()),
			"errors": ie.Rows,
		})
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, httpx.List(machines))
}

func (s *Server) handleMachine(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.Fleet.Get(r.Context(), s.Fleet.Pool(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	insts, err := s.Rental.ListInstancesByMachine(r.Context(), id, 21)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var current *instanceView
	history := make([]instanceView, 0, len(insts))
	for _, i := range insts {
		v := s.instanceView(i)
		switch {
		case current == nil && i.State != rental.StateReleased && i.State != rental.StateFailed:
			current = &v
		case len(history) < 20:
			history = append(history, v)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"machine": m, "current_instance": current, "history": history})
}

func (s *Server) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Rental.SetMachineMaintenance(r.Context(), s.actor(r), id, req.On); err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.Fleet.Get(r.Context(), s.Fleet.Pool(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

func (s *Server) handlePowerCycle(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Fleet.PowerCycle(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

type pduRequest struct {
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	Address    string `json:"address"`
	Community  string `json:"community"`
	OIDPrefix  string `json:"oid_prefix"`
	CycleValue int    `json:"cycle_value"`
	OnValue    int    `json:"on_value"`
}

func (p pduRequest) input() fleet.PDUInput {
	return fleet.PDUInput{Name: p.Name, Driver: p.Driver, Address: p.Address, Community: p.Community,
		OIDPrefix: p.OIDPrefix, CycleValue: p.CycleValue, OnValue: p.OnValue}
}

func (s *Server) handleListPDUs(w http.ResponseWriter, r *http.Request) {
	list, err := s.Fleet.ListPDUs(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleCreatePDU(w http.ResponseWriter, r *http.Request) {
	var req pduRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.Fleet.CreatePDU(r.Context(), s.actor(r), req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

func (s *Server) handleUpdatePDU(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req pduRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.Fleet.UpdatePDU(r.Context(), s.actor(r), id, req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	resolved, err := parseBoolParam(r.URL.Query().Get("resolved"), "resolved")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Fleet.ListAlerts(r.Context(), resolved, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleResolveAlert(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Fleet.ResolveAlert(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}
```

上面用到了 `s.Fleet.Pool()`：在 `internal/fleet/machines.go` 中加入

```go
// Pool 返回连接池，供需要传入 db.Querier 的只读调用使用。
func (s *Service) Pool() *pgxpool.Pool { return s.pool }
```

（`machines.go` 已 import `pgxpool`。）

`internal/adminapi/server.go` 的 `Router` 中，在 `s.accountRoutes(g)` 之后加一行 `s.machineRoutes(g)`。

- [ ] **Step 6: 运行确认通过**

Run: `go test ./internal/adminapi/ ./internal/fleet/`
Expected: PASS

- [ ] **Step 7: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add migrations/00016_admin_indexes.sql internal/platform/db internal/fleet internal/catalog internal/rental internal/adminapi
git commit -m "feat(adminapi): machines, CSV import, maintenance, power-cycle, PDUs and alerts

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 17: 后台：实例查询与售后接口

**Files:**
- Create: `internal/rental/admin_queries.go`、`internal/rental/admin_queries_test.go`
- Modify: `internal/fleet/commands.go`（`CommandsForInstance`）
- Modify: `internal/adminapi/views.go`（`orderView`、`commandView`）
- Create: `internal/adminapi/instances.go`、`internal/adminapi/instances_test.go`
- Modify: `internal/adminapi/server.go`（注册路由）

**Interfaces:**
- Consumes: Task 12～14 的 `AdminExtend`、`AdminRewipe`、`AdminMigrate`、`RefundQuote`、`RefundRelease`、`instanceChain`；Task 16 的 `db.Where`、`instanceView`、测试辅助 `renter`/`machine`/`place`/`ack`。
- Produces（Task 18、19 使用）：
  - `rental.InstanceFilter{State string; UserID, MachineID int64}`、`(*rental.Service).AdminListInstances(ctx, f, p paging.Page) ([]Instance, int, error)`。
  - `rental.Event{ID int64; FromState, ToState, Reason string; At time.Time}`（json：`id, from_state, to_state, reason, at`）、`(*rental.Service).InstanceEvents(ctx, id string) ([]Event, error)`。
  - `(*rental.Service).InstanceOrders(ctx, id string) ([]Order, error)`：沿换机链路的全部订单（新到旧）。
  - `(*fleet.Service).CommandsForInstance(ctx, instanceID string, limit int) ([]Command, error)`。
  - adminapi 包内：`orderView`（含 `user_id`、`refunded_amount`）与 `toOrderView(rental.Order) orderView`、`orderViews([]rental.Order) []orderView`；`commandView`；`queryInt(r, name string) (int64, error)`。
  - 接口：`GET /instances?state=&user_id=&machine_id=&page=`、`GET /instances/{id}` → `{instance, events, orders, commands}`、`GET /instances/{id}/refund-quote`（以上 all）；`POST /instances/{id}/extend {seconds, reason}`、`POST /instances/{id}/migrate {reason}` → `{instance}`（新实例）、`POST /instances/{id}/rewipe`（以上 ops）；`POST /instances/{id}/refund-release {amount, reason, totp}` → `{refunded}`（finance，需二次验证）。

- [ ] **Step 1: 写 rental 查询的失败测试**

`internal/rental/admin_queries_test.go`：

```go
package rental_test

import (
	"testing"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/rental"
)

func TestAdminListInstancesFilters(t *testing.T) {
	e := newEnv(t, 3)
	_, a := e.place(catalog.D1d)
	_, b := e.place(catalog.D1d)
	other := e.newUser("13600000099", startFunds)
	if _, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: other, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d}); err != nil {
		t.Fatal(err)
	}
	e.ack(true)
	if err := e.svc.Release(e.ctx, e.userID, a.ID); err != nil {
		t.Fatal(err)
	}
	all, total, err := e.svc.AdminListInstances(e.ctx, rental.InstanceFilter{}, paging.Page{})
	if err != nil || total != 3 || len(all) != 3 {
		t.Fatalf("all = %d, %v", total, err)
	}
	mine, total, _ := e.svc.AdminListInstances(e.ctx, rental.InstanceFilter{UserID: e.userID, State: rental.StateRunning}, paging.Page{})
	if total != 1 || mine[0].ID != b.ID {
		t.Fatalf("mine running = %+v", mine)
	}
	onMachine, total, _ := e.svc.AdminListInstances(e.ctx, rental.InstanceFilter{MachineID: a.MachineID}, paging.Page{})
	if total != 1 || onMachine[0].ID != a.ID {
		t.Fatalf("by machine = %+v", onMachine)
	}
	if _, _, err := e.svc.AdminListInstances(e.ctx, rental.InstanceFilter{State: "zombie"}, paging.Page{}); codeOf(err) != "bad_request" {
		t.Fatalf("bad state = %v", err)
	}
}

func TestInstanceEventsAndOrdersFollowMigration(t *testing.T) {
	e := newEnv(t, 2)
	ord, old := e.place(catalog.D1d)
	e.ack(true)
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "换机")
	if err != nil {
		t.Fatal(err)
	}
	events, err := e.svc.InstanceEvents(e.ctx, old.ID)
	if err != nil || len(events) != 3 || events[0].ToState != rental.StateCreating || events[2].ToState != rental.StateWiping {
		t.Fatalf("events = %+v, %v", events, err)
	}
	orders, err := e.svc.InstanceOrders(e.ctx, moved.ID)
	if err != nil || len(orders) != 1 || orders[0].ID != ord.ID {
		t.Fatalf("orders of migrated instance = %+v, %v", orders, err)
	}
	cmds, err := e.fleet.CommandsForInstance(e.ctx, old.ID, 10)
	if err != nil || len(cmds) != 2 || cmds[0].Type != "wipe_instance" {
		t.Fatalf("commands = %+v, %v", cmds, err)
	}
}
```

Run: `go test ./internal/rental/ -run 'AdminListInstances|InstanceEventsAndOrders'`
Expected: 编译失败。

- [ ] **Step 2: 实现 rental 与 fleet 查询**

`internal/rental/admin_queries.go`：

```go
package rental

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type InstanceFilter struct {
	State     string
	UserID    int64
	MachineID int64
}

var instanceStates = []string{StateCreating, StateRunning, StateStopped, StateWiping, StateReleased, StateFailed}

// AdminListInstances 按创建时间倒序分页返回实例。
func (s *Service) AdminListInstances(ctx context.Context, f InstanceFilter, p paging.Page) ([]Instance, int, error) {
	var w db.Where
	if f.State != "" {
		if !slices.Contains(instanceStates, f.State) {
			return nil, 0, apperr.BadRequest("实例状态不正确")
		}
		w.Add("state = ?", f.State)
	}
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.MachineID > 0 {
		w.Add("machine_id = ?", f.MachineID)
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM instances WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	list, err := collectInstances(s.d.Pool.Query(ctx, `SELECT `+instanceCols+` FROM instances WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...))
	return list, total, err
}

type Event struct {
	ID        int64     `json:"id"`
	FromState string    `json:"from_state"`
	ToState   string    `json:"to_state"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

// InstanceEvents 返回实例的状态变化记录（旧到新）。
func (s *Service) InstanceEvents(ctx context.Context, id string) ([]Event, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT id, from_state, to_state, reason, at FROM instance_events
		WHERE instance_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.FromState, &e.ToState, &e.Reason, &e.At)
		return e, err
	})
}

// InstanceOrders 返回实例及其换机前身的全部订单（新到旧）。
func (s *Service) InstanceOrders(ctx context.Context, id string) ([]Order, error) {
	chain, err := instanceChain(ctx, s.d.Pool, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE instance_id = ANY($1) ORDER BY created_at DESC, id`, chain)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
}
```

`internal/fleet/commands.go` 末尾追加：

```go
// CommandsForInstance 返回实例的指令记录（新到旧，含已完成与失败的）。
func (s *Service) CommandsForInstance(ctx context.Context, instanceID string, limit int) ([]Command, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+commandCols+` FROM agent_commands
		WHERE instance_id = $1 ORDER BY created_at DESC, id LIMIT $2`, instanceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Command, error) { return scanCommand(r) })
}
```

Run: `go test ./internal/rental/ ./internal/fleet/`
Expected: PASS

- [ ] **Step 3: 写后台接口的失败测试**

`internal/adminapi/instances_test.go`：

```go
package adminapi_test

import (
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/rental"
)

func TestInstanceListAndDetail(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	uid := e.renter("13600000001")
	inst := e.place(uid)
	e.ack()
	fin := e.loginAs("fin", admin.RoleFinance)
	code, out := fin.do("GET", "/instances?state=running&user_id="+itoa(uid), nil)
	if code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/instances?user_id=abc", nil); code != 400 {
		t.Fatalf("bad user_id = %d %v", code, out)
	}
	code, out = fin.do("GET", "/instances/"+inst.ID, nil)
	if code != 200 || out["instance"].(map[string]any)["state"] != "running" || len(out["events"].([]any)) != 2 ||
		len(out["commands"].([]any)) != 1 {
		t.Fatalf("detail = %d %v", code, out)
	}
	order := out["orders"].([]any)[0].(map[string]any)
	if order["user_id"].(float64) != float64(uid) || order["amount"].(float64) != 6900 {
		t.Fatalf("order view = %v", order)
	}
	if code, _ := fin.do("GET", "/instances/ins-missing", nil); code != 404 {
		t.Fatalf("missing = %d", code)
	}
}

func TestInstanceAfterSales(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	e.machine("SN2")
	uid := e.renter("13600000001")
	inst := e.place(uid)
	e.ack()
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)

	if code, out := ops.do("POST", "/instances/"+inst.ID+"/extend", map[string]any{"seconds": 3600, "reason": "补偿"}); code != 200 || out["state"] != "running" {
		t.Fatalf("extend = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/instances/"+inst.ID+"/extend", map[string]any{"seconds": 3600, "reason": "补偿"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance extend = %d %v", code, out)
	}
	code, out := ops.do("POST", "/instances/"+inst.ID+"/migrate", map[string]any{"reason": "GPU 报错"})
	if code != 200 {
		t.Fatalf("migrate = %d %v", code, out)
	}
	moved := out["instance"].(map[string]any)["id"].(string)
	e.ack()

	if code, out := ops.do("POST", "/instances/"+moved+"/refund-release", map[string]any{"amount": 100, "reason": "x"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops refund = %d %v", code, out)
	}
	code, out = fin.do("GET", "/instances/"+moved+"/refund-quote", nil)
	if code != 200 || out["max"].(float64) != 6900 || out["suggested"].(float64) <= 0 {
		t.Fatalf("quote = %d %v", code, out)
	}
	suggested := out["suggested"]
	if code, out := fin.do("POST", "/instances/"+moved+"/refund-release", map[string]any{"amount": suggested, "reason": "故障"}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("refund without totp = %d %v", code, out)
	}
	code, out = fin.do("POST", "/instances/"+moved+"/refund-release", map[string]any{"amount": suggested, "reason": "故障", "totp": fin.totp()})
	if code != 200 || out["refunded"] != suggested {
		t.Fatalf("refund = %d %v", code, out)
	}
	if got, _ := e.rental.Get(e.ctx, moved); got.State != rental.StateWiping {
		t.Fatalf("state after refund = %s", got.State)
	}
	if code, out := ops.do("POST", "/instances/"+moved+"/rewipe", map[string]any{}); code != 200 {
		t.Fatalf("rewipe = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/instances/"+inst.ID+"/rewipe", map[string]any{}); code != 409 {
		t.Fatalf("rewipe released instance = %d %v", code, out)
	}
}
```

在 `internal/adminapi/helpers_test.go` 末尾追加（import 增加 `"strconv"`）：

```go
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
```

Run: `go test ./internal/adminapi/ -run Instance`
Expected: 失败（404）。

- [ ] **Step 4: 实现后台接口**

在 `internal/adminapi/views.go` 末尾追加（import 增加 `"spark-cloud/internal/catalog"`、`"spark-cloud/internal/fleet"`）：

```go
// orderView 是后台展示的订单（比用户端多 user_id）。
type orderView struct {
	ID             string           `json:"id"`
	UserID         int64            `json:"user_id"`
	Kind           string           `json:"kind"`
	InstanceID     string           `json:"instance_id"`
	ProductID      int64            `json:"product_id"`
	Duration       catalog.Duration `json:"duration"`
	Amount         int64            `json:"amount"`
	RefundedAmount int64            `json:"refunded_amount"`
	Status         string           `json:"status"`
	CreatedAt      time.Time        `json:"created_at"`
}

func toOrderView(o rental.Order) orderView {
	return orderView{ID: o.ID, UserID: o.UserID, Kind: o.Kind, InstanceID: o.InstanceID, ProductID: o.ProductID, Duration: o.Duration,
		Amount: o.Amount, RefundedAmount: o.RefundedAmount, Status: o.Status, CreatedAt: o.CreatedAt}
}

func orderViews(list []rental.Order) []orderView {
	out := make([]orderView, 0, len(list))
	for _, o := range list {
		out = append(out, toOrderView(o))
	}
	return out
}

type commandView struct {
	ID        string    `json:"id"`
	MachineID int64     `json:"machine_id"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	Result    string    `json:"result"`
	Attempts  int       `json:"attempts"`
	CreatedAt time.Time `json:"created_at"`
}

func commandViews(list []fleet.Command) []commandView {
	out := make([]commandView, 0, len(list))
	for _, c := range list {
		out = append(out, commandView{ID: c.ID, MachineID: c.MachineID, Type: string(c.Type), Status: c.Status, Result: c.Result,
			Attempts: c.Attempts, CreatedAt: c.CreatedAt})
	}
	return out
}
```

`internal/adminapi/instances.go`：

```go
package adminapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/rental"
)

func (s *Server) instanceRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/instances", allRoles, s.handleListInstances)
	s.handle(g, http.MethodGet, "/instances/{id}", allRoles, s.handleInstance)
	s.handle(g, http.MethodGet, "/instances/{id}/refund-quote", allRoles, s.handleRefundQuote)
	s.handle(g, http.MethodPost, "/instances/{id}/extend", opsRoles, s.handleExtend)
	s.handle(g, http.MethodPost, "/instances/{id}/migrate", opsRoles, s.handleMigrate)
	s.handle(g, http.MethodPost, "/instances/{id}/rewipe", opsRoles, s.handleRewipe)
	s.handle(g, http.MethodPost, "/instances/{id}/refund-release", financeRoles, s.handleRefundRelease)
}

// queryInt 解析可选的正整数查询参数；缺省返回 0。
func queryInt(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, apperr.BadRequest(name + " 必须是正整数")
	}
	return n, nil
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	uid, err := queryInt(r, "user_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	mid, err := queryInt(r, "machine_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Rental.AdminListInstances(r.Context(), rental.InstanceFilter{State: r.URL.Query().Get("state"), UserID: uid, MachineID: mid}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(s.instanceViews(list), total))
}

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	inst, err := s.Rental.Get(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	events, err := s.Rental.InstanceEvents(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	orders, err := s.Rental.InstanceOrders(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	cmds, err := s.Fleet.CommandsForInstance(r.Context(), id, 50)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if events == nil {
		events = []rental.Event{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"instance": s.instanceView(inst), "events": events, "orders": orderViews(orders), "commands": commandViews(cmds),
	})
}

func (s *Server) handleRefundQuote(w http.ResponseWriter, r *http.Request) {
	q, err := s.Rental.RefundQuote(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, q)
}

func (s *Server) handleExtend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seconds int64  `json:"seconds"`
		Reason  string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	inst, err := s.Rental.AdminExtend(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.Seconds, req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.instanceView(inst))
}

func (s *Server) handleMigrate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	inst, err := s.Rental.AdminMigrate(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"instance": s.instanceView(inst)})
}

func (s *Server) handleRewipe(w http.ResponseWriter, r *http.Request) {
	if err := s.Rental.AdminRewipe(r.Context(), s.actor(r), chi.URLParam(r, "id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleRefundRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Amount int64  `json:"amount"`
		Reason string `json:"reason"`
		TOTP   string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.stepUp(r, req.TOTP); err != nil {
		s.writeError(w, r, err)
		return
	}
	refunded, err := s.Rental.RefundRelease(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.Amount, req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"refunded": refunded})
}
```

`internal/adminapi/server.go` 的 `Router` 中，在 `s.machineRoutes(g)` 之后加一行 `s.instanceRoutes(g)`。

注意：`rewipe` 接口没有请求体字段，前端仍需发送 `{}`（JSON）。

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/adminapi/ ./internal/rental/ ./internal/fleet/`
Expected: PASS

- [ ] **Step 6: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/rental internal/fleet internal/adminapi
git commit -m "feat(adminapi): instance list/detail and after-sales endpoints (extend, migrate, rewipe, refund-release)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 18: 后台：用户、封禁、企业认证审核、营业执照查看、调账与手动入账

**Files:**
- Create: `internal/identity/admin.go`、`internal/identity/admin_test.go`
- Modify: `internal/wallet/wallet.go`（`AdminAdjust` 增加金额上限与未知用户映射）、`internal/wallet/wallet_test.go`
- Create: `internal/adminapi/users.go`、`internal/adminapi/users_test.go`
- Modify: `internal/adminapi/server.go`（注册路由）

**Interfaces:**
- Consumes: Task 5 的 `storage.Service.Open`；Task 6 的 `ReviewEnterpriseKYC(ctx, actor, ...)`；Task 8 的 `billing.ManualRecharge`；Task 16 的 `db.Where`；Task 17 的 `AdminListInstances`、`orderViews`、`queryInt`；计划 1 的 `rental.ListOrders`、`wallet.ListLedger`、`wallet.Balance`。
- Produces（Task 21 使用）：
  - `identity.UserFilter{Q, KYCStatus, Status string}`、`(*identity.Service).AdminListUsers(ctx, f, p paging.Page) ([]User, int, error)`。
  - `identity.KYCRecord{ID, UserID, Kind, RealName, IDNumberMasked, LicenseNo, LicenseFile, Result, Reason, ReviewerID *int64, CreatedAt, UpdatedAt}`（json：`id, user_id, kind, real_name, id_number_masked, license_no, license_file, result, reason, reviewer_id, created_at, updated_at`）。
  - `(*identity.Service).UserKYCRecords(ctx, userID) ([]KYCRecord, error)`、`PendingEnterpriseKYC(ctx, p) ([]KYCRecord, int, error)`、`GetKYCRecord(ctx, id int64) (KYCRecord, error)`、`Ban(ctx, actor, userID int64, reason string) error`、`Unban(ctx, actor, userID int64) error`。
  - `wallet.MaxAdjust = 100_000_000`；`AdminAdjust` 对未知用户返回 `not_found`。
  - 接口：`GET /users?q=&kyc_status=&status=&page=`（q 为完整手机号、手机尾号或用户 ID）、`GET /users/{id}` → `{user, balance, instances, orders, ledger, kyc}`（all）；`POST /users/{id}/ban {reason}`、`POST /users/{id}/unban`、`GET /kyc/pending?page=`、`GET /kyc/{id}`、`POST /kyc/{id}/review {approve, reason}`、`GET /files/*`（ops）；`POST /users/{id}/adjust {amount, remark, totp}`、`POST /users/{id}/manual-recharge {amount, remark, totp}`（finance，需二次验证）。

身份证号在后台只显示前 3 位与后 4 位（中间用 `*`）；文件接口响应带 `Content-Disposition: inline` 与 `X-Content-Type-Options: nosniff`。

- [ ] **Step 1: 写 identity 与 wallet 的失败测试**

`internal/identity/admin_test.go`：

```go
package identity_test

import (
	"context"
	"errors"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/plugins/kyc"
)

var opsActor = audit.Actor{AdminID: 4, IP: "10.0.0.4"}

func TestAdminListUsers(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	a := dbtest.CreateUser(t, pool, "13900000001", true)
	dbtest.CreateUser(t, pool, "13900000002", false)
	dbtest.CreateUser(t, pool, "13800000003", false)
	all, total, err := svc.AdminListUsers(ctx, identity.UserFilter{}, paging.Page{})
	if err != nil || total != 3 || len(all) != 3 {
		t.Fatalf("all = %d, %v", total, err)
	}
	if list, total, _ := svc.AdminListUsers(ctx, identity.UserFilter{Q: "13900000002"}, paging.Page{}); total != 1 || list[0].Phone != "13900000002" {
		t.Fatalf("q=full phone = %+v", list)
	}
	if list, total, _ := svc.AdminListUsers(ctx, identity.UserFilter{Q: "0003"}, paging.Page{}); total != 1 || list[0].Phone != "13800000003" {
		t.Fatalf("q=phone tail = %+v", list)
	}
	if _, _, err := svc.AdminListUsers(ctx, identity.UserFilter{Q: "张三"}, paging.Page{}); apperr.As(err) == nil {
		t.Fatal("non-numeric q must be rejected")
	}
	if list, total, _ := svc.AdminListUsers(ctx, identity.UserFilter{Q: itoa(a)}, paging.Page{}); total != 1 || list[0].ID != a {
		t.Fatalf("q=id = %+v", list)
	}
	if _, total, _ := svc.AdminListUsers(ctx, identity.UserFilter{KYCStatus: "verified"}, paging.Page{}); total != 1 {
		t.Fatalf("verified = %d", total)
	}
	if _, _, err := svc.AdminListUsers(ctx, identity.UserFilter{Status: "zombie"}, paging.Page{}); apperr.As(err) == nil {
		t.Fatalf("bad status = %v", err)
	}
}

func TestKYCRecordsMaskIDNumber(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000010", false)
	if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); err != nil {
		t.Fatal(err)
	}
	recs, err := svc.UserKYCRecords(ctx, uid)
	if err != nil || len(recs) != 1 || recs[0].IDNumberMasked != "110***********123X" || recs[0].RealName != "张三" {
		t.Fatalf("records = %+v, %v", recs, err)
	}
}

func TestPendingEnterpriseKYC(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000011", false)
	if err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", licenseKeyFor(uid)); err != nil {
		t.Fatal(err)
	}
	// 个人核验进行中的 pending 记录不能出现在企业审核列表里
	if _, err := pool.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, result, created_at, updated_at)
		VALUES ($1, 'personal', '李四', 'pending', $2, $2)`, dbtest.CreateUser(t, pool, "13900000012", false), dbtest.T0); err != nil {
		t.Fatal(err)
	}
	list, total, err := svc.PendingEnterpriseKYC(ctx, paging.Page{})
	if err != nil || total != 1 || list[0].Kind != "enterprise" || list[0].LicenseFile != licenseKeyFor(uid) {
		t.Fatalf("pending = %+v, %v", list, err)
	}
	rec, err := svc.GetKYCRecord(ctx, list[0].ID)
	if err != nil || rec.UserID != uid {
		t.Fatalf("get = %+v, %v", rec, err)
	}
	if _, err := svc.GetKYCRecord(ctx, 9999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestBanAndUnban(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000013", true)
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ('h', $1, $2, $3)`,
		uid, dbtest.T0, dbtest.T0.AddDate(0, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Ban(ctx, opsActor, uid, " "); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("ban without reason = %v", err)
	}
	if err := svc.Ban(ctx, opsActor, uid, "恶意挖矿"); err != nil {
		t.Fatal(err)
	}
	var sessions int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, uid).Scan(&sessions)
	if u, _ := svc.GetUser(ctx, uid); u.Status != "banned" || sessions != 0 {
		t.Fatalf("status = %s sessions = %d", u.Status, sessions)
	}
	if err := svc.Ban(ctx, opsActor, uid, "again"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("ban twice = %v", err)
	}
	if err := svc.Unban(ctx, opsActor, uid); err != nil {
		t.Fatal(err)
	}
	if err := svc.Unban(ctx, opsActor, uid); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("unban twice = %v", err)
	}
	if err := svc.Ban(ctx, opsActor, 9999, "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("ban missing = %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action IN ('user.ban', 'user.unban') AND actor_id = 4`).Scan(&n)
	if n != 2 {
		t.Fatalf("audit rows = %d", n)
	}
}
```

在同一文件末尾加入：

```go
func itoa(n int64) string { return fmt.Sprint(n) }
```

（import 增加 `"fmt"`。）

在 `internal/wallet/wallet_test.go` 末尾追加（如未 import `apperr`、`dbtest`、`context` 则补上）：

```go
func TestAdminAdjustLimitsAndUnknownUser(t *testing.T) {
	pool := dbtest.New(t)
	svc := wallet.NewService(pool, clock.NewFake(dbtest.T0))
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000001", false)
	for _, amount := range []int64{0, wallet.MaxAdjust + 1, -wallet.MaxAdjust - 1} {
		if _, err := svc.AdminAdjust(ctx, 1, uid, amount, "x", ""); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
			t.Fatalf("amount %d = %v", amount, err)
		}
	}
	if _, err := svc.AdminAdjust(ctx, 1, 9999, 100, "x", ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown user = %v", err)
	}
}
```

（若 `wallet_test.go` 尚未 import `"errors"` 或 `clock`，补上。）

Run: `go test ./internal/identity/ ./internal/wallet/`
Expected: 编译失败。

- [ ] **Step 2: 实现 identity 后台方法与调账限制**

`internal/identity/admin.go`：

```go
package identity

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type UserFilter struct {
	Q         string // 完整手机号、手机尾号或用户 ID
	KYCStatus string
	Status    string
}

var (
	kycStatuses  = []string{"none", "pending", "verified", "rejected"}
	userStatuses = []string{"active", "banned"}
)

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func (s *Service) AdminListUsers(ctx context.Context, f UserFilter, p paging.Page) ([]User, int, error) {
	var w db.Where
	switch q := strings.TrimSpace(f.Q); {
	case q == "":
	case isDigits(q) && len(q) == 11: // 完整手机号
		w.Add("phone = ?", q)
	case isDigits(q): // 用户 ID 或手机尾号
		w.Add("(id::text = ? OR phone LIKE ?)", q, "%"+q)
	default:
		return nil, 0, apperr.BadRequest("q 只能是手机号、手机尾号或用户 ID")
	}
	if f.KYCStatus != "" {
		if !slices.Contains(kycStatuses, f.KYCStatus) {
			return nil, 0, apperr.BadRequest("实名状态不正确")
		}
		w.Add("kyc_status = ?", f.KYCStatus)
	}
	if f.Status != "" {
		if !slices.Contains(userStatuses, f.Status) {
			return nil, 0, apperr.BadRequest("账号状态不正确")
		}
		w.Add("status = ?", f.Status)
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.d.Pool.Query(ctx, `SELECT `+userCols+` FROM users WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (User, error) { return scanUser(r) })
	return list, total, err
}

type KYCRecord struct {
	ID             int64     `json:"id"`
	UserID         int64     `json:"user_id"`
	Kind           string    `json:"kind"`
	RealName       string    `json:"real_name"`
	IDNumberMasked string    `json:"id_number_masked,omitempty"`
	LicenseNo      string    `json:"license_no,omitempty"`
	LicenseFile    string    `json:"license_file,omitempty"`
	Result         string    `json:"result"`
	Reason         string    `json:"reason"`
	ReviewerID     *int64    `json:"reviewer_id"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

const kycCols = `id, user_id, kind, real_name, id_number_enc, license_no, license_file, result, reason, reviewer_id, created_at, updated_at`

// maskIDNumber 只保留身份证号前 3 位与后 4 位。
func maskIDNumber(id string) string {
	if len(id) <= 7 {
		return strings.Repeat("*", len(id))
	}
	return id[:3] + strings.Repeat("*", len(id)-7) + id[len(id)-4:]
}

func (s *Service) scanKYC(row pgx.Row) (KYCRecord, error) {
	var k KYCRecord
	var enc []byte
	err := row.Scan(&k.ID, &k.UserID, &k.Kind, &k.RealName, &enc, &k.LicenseNo, &k.LicenseFile, &k.Result, &k.Reason,
		&k.ReviewerID, &k.CreatedAt, &k.UpdatedAt)
	if db.IsNoRows(err) {
		return KYCRecord{}, apperr.ErrNotFound
	}
	if err != nil {
		return KYCRecord{}, err
	}
	if enc != nil {
		plain, err := s.d.Box.Open(enc)
		if err != nil {
			return KYCRecord{}, fmt.Errorf("decrypt id number: %w", err)
		}
		k.IDNumberMasked = maskIDNumber(string(plain))
	}
	return k, nil
}

func (s *Service) collectKYC(rows pgx.Rows, err error) ([]KYCRecord, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (KYCRecord, error) { return s.scanKYC(r) })
}

func (s *Service) UserKYCRecords(ctx context.Context, userID int64) ([]KYCRecord, error) {
	return s.collectKYC(s.d.Pool.Query(ctx, `SELECT `+kycCols+` FROM kyc_records WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, userID))
}

// PendingEnterpriseKYC 返回待人工审核的企业认证（先提交的在前）。
func (s *Service) PendingEnterpriseKYC(ctx context.Context, p paging.Page) ([]KYCRecord, int, error) {
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM kyc_records WHERE kind = 'enterprise' AND result = 'pending'`).Scan(&total); err != nil {
		return nil, 0, err
	}
	list, err := s.collectKYC(s.d.Pool.Query(ctx, `SELECT `+kycCols+` FROM kyc_records WHERE kind = 'enterprise' AND result = 'pending'
		ORDER BY created_at, id LIMIT $1 OFFSET $2`, p.Limit(), p.Offset()))
	return list, total, err
}

func (s *Service) GetKYCRecord(ctx context.Context, id int64) (KYCRecord, error) {
	return s.scanKYC(s.d.Pool.QueryRow(ctx, `SELECT `+kycCols+` FROM kyc_records WHERE id = $1`, id))
}

// Ban 封禁用户并删除其全部会话；运行中的实例不受影响。
func (s *Service) Ban(ctx context.Context, actor audit.Actor, userID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return apperr.BadRequest("请填写封禁原因")
	}
	return s.setStatus(ctx, actor, userID, "active", "banned", "user.ban", map[string]any{"reason": reason})
}

func (s *Service) Unban(ctx context.Context, actor audit.Actor, userID int64) error {
	return s.setStatus(ctx, actor, userID, "banned", "active", "user.unban", nil)
}

func (s *Service) setStatus(ctx context.Context, actor audit.Actor, userID int64, from, to, action string, diff any) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&status)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != from {
			return apperr.ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET status = $2, updated_at = $3 WHERE id = $1`, userID, to, now); err != nil {
			return err
		}
		if to == "banned" {
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, now, actor.Entry(action, "user", strconv.FormatInt(userID, 10), diff))
	})
}
```

`internal/wallet/wallet.go`：

1. 在 `Kind` 常量块之后加入 `const MaxAdjust = 100_000_000 // 单次调账上限（分）`。
2. `AdminAdjust` 开头（`remark` 校验之后）加入：

```go
	if amount == 0 || amount > MaxAdjust || amount < -MaxAdjust {
		return 0, apperr.BadRequest("调整金额需在 ±100 万元之间且不能为 0")
	}
```

3. `AdminAdjust` 末尾的 `return after, err` 改为：

```go
	if db.IsForeignKeyViolation(err) {
		return 0, apperr.ErrNotFound
	}
	return after, err
```

Run: `go test ./internal/identity/ ./internal/wallet/`
Expected: PASS

- [ ] **Step 3: 写后台接口的失败测试**

`internal/adminapi/users_test.go`：

```go
package adminapi_test

import (
	"bytes"
	"io"
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/storage"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func TestUserListAndDetail(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	uid := e.renter("13600000001")
	e.place(uid)
	fin := e.loginAs("fin", admin.RoleFinance)
	if code, out := fin.do("GET", "/users?q=13600000001", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	code, out := fin.do("GET", "/users/"+itoa(uid), nil)
	if code != 200 || out["user"].(map[string]any)["phone"] != "13600000001" || out["balance"].(float64) != 1_000_000-6900 ||
		len(out["instances"].([]any)) != 1 || len(out["orders"].([]any)) != 1 || len(out["ledger"].([]any)) != 2 {
		t.Fatalf("detail = %d %v", code, out)
	}
	if code, _ := fin.do("GET", "/users/9999", nil); code != 404 {
		t.Fatalf("missing user = %d", code)
	}
}

func TestBanUnbanEndpoints(t *testing.T) {
	e := newEnv(t)
	uid := e.renter("13600000001")
	ops := e.loginAs("ops", admin.RoleOps)
	if code, out := ops.do("POST", "/users/"+itoa(uid)+"/ban", map[string]string{"reason": "违规"}); code != 200 {
		t.Fatalf("ban = %d %v", code, out)
	}
	if u, _ := e.identity.GetUser(e.ctx, uid); u.Status != "banned" {
		t.Fatalf("status = %s", u.Status)
	}
	if code, out := ops.do("POST", "/users/"+itoa(uid)+"/unban", map[string]any{}); code != 200 {
		t.Fatalf("unban = %d %v", code, out)
	}
	fin := e.loginAs("fin", admin.RoleFinance)
	if code, out := fin.do("POST", "/users/"+itoa(uid)+"/ban", map[string]string{"reason": "x"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance ban = %d %v", code, out)
	}
}

func TestEnterpriseReviewAndLicenseFile(t *testing.T) {
	e := newEnv(t)
	uid := e.renter("13600000001")
	if _, err := e.pool.Exec(e.ctx, `UPDATE users SET kyc_status = 'none' WHERE id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	up, err := e.uploads.Save(e.ctx, uid, storage.PurposeLicense, bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.identity.SubmitEnterpriseKYC(e.ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", up.Key); err != nil {
		t.Fatal(err)
	}
	ops := e.loginAs("ops", admin.RoleOps)
	code, out := ops.do("GET", "/kyc/pending", nil)
	if code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("pending = %d %v", code, out)
	}
	rec := out["items"].([]any)[0].(map[string]any)
	id := int64(rec["id"].(float64))
	if code, out := ops.do("GET", "/kyc/"+itoa(id), nil); code != 200 || out["license_file"] != up.Key {
		t.Fatalf("record = %d %v", code, out)
	}
	resp, _ := ops.send("GET", "/files/"+up.Key, nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		resp.Header.Get("Content-Disposition") != "inline" || !bytes.Equal(body, pngBytes) {
		t.Fatalf("file = %d %v", resp.StatusCode, resp.Header)
	}
	if code, _ := ops.do("GET", "/files/../../etc/passwd", nil); code != 404 {
		t.Fatalf("traversal = %d", code)
	}
	if code, out := ops.do("POST", "/kyc/"+itoa(id)+"/review", map[string]any{"approve": false}); code != 400 {
		t.Fatalf("reject without reason = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/kyc/"+itoa(id)+"/review", map[string]any{"approve": true}); code != 200 {
		t.Fatalf("approve = %d %v", code, out)
	}
	if u, _ := e.identity.GetUser(e.ctx, uid); u.KYCStatus != "verified" || u.Kind != "enterprise" {
		t.Fatalf("user = %+v", u)
	}
	fin := e.loginAs("fin", admin.RoleFinance)
	if code, _ := fin.do("GET", "/files/"+up.Key, nil); code != 403 {
		t.Fatalf("finance file = %d", code)
	}
}

func TestAdjustAndManualRecharge(t *testing.T) {
	e := newEnv(t)
	uid := e.renter("13600000001")
	fin := e.loginAs("fin", admin.RoleFinance)
	if code, out := fin.do("POST", "/users/"+itoa(uid)+"/adjust", map[string]any{"amount": 5000, "remark": "补偿"}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("adjust without totp = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/users/"+itoa(uid)+"/adjust", map[string]any{"amount": 5000, "remark": "补偿", "totp": fin.totp()}); code != 200 || out["balance"].(float64) != 1_005_000 {
		t.Fatalf("adjust = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/users/"+itoa(uid)+"/adjust", map[string]any{"amount": -2_000_000, "remark": "扣多了"}); code != 402 || codeOf(out) != "insufficient_balance" {
		t.Fatalf("over-deduct = %d %v", code, out)
	}
	code, out := fin.do("POST", "/users/"+itoa(uid)+"/manual-recharge", map[string]any{"amount": 20000, "remark": "对公转账 123"})
	if code != 200 || out["status"] != "paid" || out["channel"] != "manual" {
		t.Fatalf("manual recharge = %d %v", code, out)
	}
	if b, _ := e.wallet.Balance(e.ctx, uid); b != 1_025_000 {
		t.Fatalf("balance = %d", b)
	}
	if code, _ := fin.do("POST", "/users/9999/adjust", map[string]any{"amount": 100, "remark": "x"}); code != 404 {
		t.Fatalf("unknown user = %d", code)
	}
	ops := e.loginAs("ops", admin.RoleOps)
	if code, out := ops.do("POST", "/users/"+itoa(uid)+"/adjust", map[string]any{"amount": 1, "remark": "x"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops adjust = %d %v", code, out)
	}
}
```

Run: `go test ./internal/adminapi/ -run 'User|Ban|Enterprise|Adjust'`
Expected: 失败（404）。

- [ ] **Step 4: 实现后台接口**

`internal/adminapi/users.go`：

```go
package adminapi

import (
	"io"
	"net/http"
	"path"
	"strconv"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/rental"
)

func (s *Server) userRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/users", allRoles, s.handleListUsers)
	s.handle(g, http.MethodGet, "/users/{id}", allRoles, s.handleUser)
	s.handle(g, http.MethodPost, "/users/{id}/ban", opsRoles, s.handleBan)
	s.handle(g, http.MethodPost, "/users/{id}/unban", opsRoles, s.handleUnban)
	s.handle(g, http.MethodGet, "/kyc/pending", opsRoles, s.handlePendingKYC)
	s.handle(g, http.MethodGet, "/kyc/{id}", opsRoles, s.handleKYCRecord)
	s.handle(g, http.MethodPost, "/kyc/{id}/review", opsRoles, s.handleReviewKYC)
	s.handle(g, http.MethodGet, "/files/*", opsRoles, s.handleFile)
	s.handle(g, http.MethodPost, "/users/{id}/adjust", financeRoles, s.handleAdjust)
	s.handle(g, http.MethodPost, "/users/{id}/manual-recharge", financeRoles, s.handleManualRecharge)
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	list, total, err := s.Identity.AdminListUsers(r.Context(),
		identity.UserFilter{Q: q.Get("q"), KYCStatus: q.Get("kyc_status"), Status: q.Get("status")}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ctx := r.Context()
	u, err := s.Identity.GetUser(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	balance, err := s.Wallet.Balance(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	insts, _, err := s.Rental.AdminListInstances(ctx, rental.InstanceFilter{UserID: id}, paging.Page{Number: 1, Size: 20})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	orders, err := s.Rental.ListOrders(ctx, id, 20)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ledger, err := s.Wallet.ListLedger(ctx, id, 0, 20)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	kyc, err := s.Identity.UserKYCRecords(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"user": u, "balance": balance, "instances": s.instanceViews(insts), "orders": orderViews(orders),
		"ledger": httpx.List(ledger)["items"], "kyc": httpx.List(kyc)["items"],
	})
}

func (s *Server) handleBan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Identity.Ban(r.Context(), s.actor(r), id, req.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleUnban(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Identity.Unban(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handlePendingKYC(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Identity.PendingEnterpriseKYC(r.Context(), p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleKYCRecord(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rec, err := s.Identity.GetKYCRecord(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rec)
}

func (s *Server) handleReviewKYC(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Identity.ReviewEnterpriseKYC(r.Context(), s.actor(r), id, req.Approve, req.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

// handleFile 输出上传的文件（营业执照），供审核查看。
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	up, rc, err := s.Uploads.Open(r.Context(), chi.URLParam(r, "*"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer rc.Close()
	h := w.Header()
	h.Set("Content-Type", up.ContentType)
	h.Set("Content-Length", strconv.FormatInt(up.Size, 10))
	h.Set("Content-Disposition", "inline")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		s.Log.Warn("serve upload aborted", "key", path.Clean(up.Key), "err", err)
	}
}

type moneyRequest struct {
	Amount int64  `json:"amount"`
	Remark string `json:"remark"`
	TOTP   string `json:"totp"`
}

// decodeMoney 解析调账 / 入账请求并完成二次验证。
func (s *Server) decodeMoney(w http.ResponseWriter, r *http.Request) (int64, moneyRequest, bool) {
	id, err := pathID(r, "id")
	if err == nil {
		var req moneyRequest
		if err = httpx.DecodeJSON(r, &req); err == nil {
			if err = s.stepUp(r, req.TOTP); err == nil {
				return id, req, true
			}
		}
	}
	s.writeError(w, r, err)
	return 0, moneyRequest{}, false
}

func (s *Server) handleAdjust(w http.ResponseWriter, r *http.Request) {
	id, req, ok := s.decodeMoney(w, r)
	if !ok {
		return
	}
	after, err := s.Wallet.AdminAdjust(r.Context(), currentSession(r).Admin.ID, id, req.Amount, req.Remark, s.actor(r).IP)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"balance": after})
}

func (s *Server) handleManualRecharge(w http.ResponseWriter, r *http.Request) {
	id, req, ok := s.decodeMoney(w, r)
	if !ok {
		return
	}
	rch, err := s.Billing.ManualRecharge(r.Context(), s.actor(r), id, req.Amount, req.Remark)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rch)
}
```

`internal/adminapi/server.go` 的 `Router` 中，在 `s.instanceRoutes(g)` 之后加一行 `s.userRoutes(g)`。

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/adminapi/ ./internal/identity/ ./internal/wallet/`
Expected: PASS

- [ ] **Step 6: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/identity internal/wallet internal/adminapi
git commit -m "feat(adminapi): users, bans, enterprise KYC review with license files, adjustments and manual recharge

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 19: 后台：订单、流水、充值单列表与 CSV 导出

**Files:**
- Modify: `internal/rental/admin_queries.go`、`internal/rental/admin_queries_test.go`
- Create: `internal/wallet/admin.go`、`internal/wallet/admin_test.go`
- Create: `internal/billing/admin.go`、`internal/billing/admin_test.go`
- Create: `internal/adminapi/finance.go`、`internal/adminapi/finance_test.go`
- Modify: `internal/adminapi/server.go`（注册路由）

**Interfaces:**
- Consumes: Task 1 的 `csvx`；Task 16 的 `db.Where`；Task 17 的 `orderView`、`queryInt`。
- Produces（Task 21 使用）：
  - `rental.OrderFilter{UserID int64; Kind, Status string; From, To *time.Time}`、`(*rental.Service).AdminListOrders(ctx, f, p) ([]Order, int, error)`、`ExportOrders(ctx, f, fn func(Order) error) error`。
  - `wallet.LedgerFilter{UserID int64; Kind string; From, To *time.Time}`、`wallet.AdminLedgerRow{LedgerRow; UserID int64; OperatorID *int64}`、`(*wallet.Service).AdminListLedger(ctx, f, p) ([]AdminLedgerRow, int, error)`、`ExportLedger(ctx, f, fn func(AdminLedgerRow) error) error`。
  - `billing.RechargeFilter{UserID int64; Channel, Status string; From, To *time.Time}`、`(*billing.Service).AdminList(ctx, f, p) ([]Recharge, int, error)`、`Export(ctx, f, fn func(Recharge) error) error`。
  - 所有 `ExportX` 先统计行数，超过 `csvx.MaxRows` 时在调用 `fn` 之前返回 `csvx.ErrTooLarge`；导出按时间正序，列表按时间倒序。
  - adminapi 包内：`cst`（北京时间）、`parseRange(r) (from, to *time.Time, err error)`（`from`/`to` 接受 `2006-01-02`（北京时间，`to` 含当天）或 RFC3339）、`(*Server).csvExport(w, r, name string, header []string, run func(write func([]string) error) error)`。
  - 接口（finance）：`GET /orders`、`GET /orders/export`、`GET /ledger`、`GET /ledger/export`、`GET /recharges`、`GET /recharges/export`，共同参数 `user_id`、`from`、`to`；订单另有 `kind`、`status`；流水另有 `kind`；充值另有 `channel`、`status`。导出为 `text/csv; charset=utf-8`，`Content-Disposition: attachment; filename="<名称>-<北京时间 yyyymmdd-hhmmss>.csv"`。

- [ ] **Step 1: 写服务层的失败测试**

在 `internal/rental/admin_queries_test.go` 末尾追加（import 增加 `"errors"`、`"time"`、`"spark-cloud/internal/platform/csvx"`）：

```go
func TestAdminListOrdersAndExport(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(48 * time.Hour)
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w, ""); err != nil {
		t.Fatal(err)
	}
	list, total, err := e.svc.AdminListOrders(e.ctx, rental.OrderFilter{}, paging.Page{})
	if err != nil || total != 2 || list[0].Kind != "renew" || list[0].UserID != e.userID {
		t.Fatalf("all = %+v, %d, %v", list, total, err)
	}
	from := e.clock.Now().Add(-time.Hour)
	if _, total, _ := e.svc.AdminListOrders(e.ctx, rental.OrderFilter{From: &from}, paging.Page{}); total != 1 {
		t.Fatalf("from filter = %d", total)
	}
	if _, total, _ := e.svc.AdminListOrders(e.ctx, rental.OrderFilter{To: &from, Kind: "new", Status: "paid"}, paging.Page{}); total != 1 {
		t.Fatalf("to+kind+status filter = %d", total)
	}
	if _, _, err := e.svc.AdminListOrders(e.ctx, rental.OrderFilter{Kind: "gift"}, paging.Page{}); codeOf(err) != "bad_request" {
		t.Fatalf("bad kind = %v", err)
	}
	var kinds []string
	if err := e.svc.ExportOrders(e.ctx, rental.OrderFilter{}, func(o rental.Order) error { kinds = append(kinds, o.Kind); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 2 || kinds[0] != "new" {
		t.Fatalf("export order = %v (must be oldest first)", kinds)
	}
}

func TestExportOrdersTooLarge(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO orders (id, user_id, kind, instance_id, product_id, duration, amount, status, created_at, updated_at)
		SELECT 'ord-bulk-' || g, $1, 'renew', $2, $3, '1d', 1, 'paid', $4, $4 FROM generate_series(1, $5::int) g`,
		e.userID, inst.ID, e.productID, dbtest.T0, csvx.MaxRows); err != nil {
		t.Fatal(err)
	}
	called := false
	err := e.svc.ExportOrders(e.ctx, rental.OrderFilter{}, func(rental.Order) error { called = true; return nil })
	if !errors.Is(err, csvx.ErrTooLarge) || called {
		t.Fatalf("export = %v, called = %v", err, called)
	}
}
```

（同一文件若尚未 import `dbtest`，补上 `"spark-cloud/internal/platform/dbtest"`。）

`internal/wallet/admin_test.go`：

```go
package wallet_test

import (
	"context"
	"testing"
	"time"

	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/wallet"
)

func TestAdminListLedgerAndExport(t *testing.T) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	svc := wallet.NewService(pool, clk)
	ctx := context.Background()
	a := dbtest.CreateUser(t, pool, "13700000001", false)
	b := dbtest.CreateUser(t, pool, "13700000002", false)
	if _, err := svc.AdminAdjust(ctx, 7, a, 1000, "补偿", ""); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	if _, err := svc.AdminAdjust(ctx, 7, b, 2000, "补偿", ""); err != nil {
		t.Fatal(err)
	}
	list, total, err := svc.AdminListLedger(ctx, wallet.LedgerFilter{}, paging.Page{})
	if err != nil || total != 2 || list[0].UserID != b || list[0].OperatorID == nil || *list[0].OperatorID != 7 {
		t.Fatalf("all = %+v, %d, %v", list, total, err)
	}
	if _, total, _ := svc.AdminListLedger(ctx, wallet.LedgerFilter{UserID: a, Kind: "admin_adjust"}, paging.Page{}); total != 1 {
		t.Fatalf("user filter = %d", total)
	}
	if _, _, err := svc.AdminListLedger(ctx, wallet.LedgerFilter{Kind: "gift"}, paging.Page{}); err == nil {
		t.Fatal("bad kind accepted")
	}
	var users []int64
	if err := svc.ExportLedger(ctx, wallet.LedgerFilter{}, func(r wallet.AdminLedgerRow) error { users = append(users, r.UserID); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0] != a {
		t.Fatalf("export = %v", users)
	}
}
```

`internal/billing/admin_test.go`：

```go
package billing_test

import (
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/plugins/pay"
)

func TestAdminListAndExportRecharges(t *testing.T) {
	e := newEnv(t)
	e.create(pay.ChannelWechat, 100)
	e.clk.Advance(time.Minute)
	if _, err := e.svc.ManualRecharge(e.ctx, audit.Actor{AdminID: 1}, e.userID, 500, "转账"); err != nil {
		t.Fatal(err)
	}
	list, total, err := e.svc.AdminList(e.ctx, billing.RechargeFilter{}, paging.Page{})
	if err != nil || total != 2 || list[0].Channel != billing.ChannelManual {
		t.Fatalf("all = %+v, %d, %v", list, total, err)
	}
	if _, total, _ := e.svc.AdminList(e.ctx, billing.RechargeFilter{Status: billing.StatusPending, UserID: e.userID}, paging.Page{}); total != 1 {
		t.Fatalf("pending = %d", total)
	}
	if _, _, err := e.svc.AdminList(e.ctx, billing.RechargeFilter{Channel: "paypal"}, paging.Page{}); codeOf(err) != "bad_request" {
		t.Fatalf("bad channel = %v", err)
	}
	var channels []string
	if err := e.svc.Export(e.ctx, billing.RechargeFilter{}, func(r billing.Recharge) error { channels = append(channels, r.Channel); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 || channels[0] != pay.ChannelWechat {
		t.Fatalf("export = %v", channels)
	}
}
```

Run: `go test ./internal/rental/ ./internal/wallet/ ./internal/billing/`
Expected: 编译失败。

- [ ] **Step 2: 实现服务层**

在 `internal/rental/admin_queries.go` 末尾追加（import 增加 `"spark-cloud/internal/platform/csvx"`）：

```go
type OrderFilter struct {
	UserID int64
	Kind   string // new / renew
	Status string // paid / refunded / partially_refunded
	From   *time.Time
	To     *time.Time // 不含
}

func (f OrderFilter) where() (db.Where, error) {
	var w db.Where
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.Kind != "" {
		if f.Kind != "new" && f.Kind != "renew" {
			return w, apperr.BadRequest("订单类型不正确")
		}
		w.Add("kind = ?", f.Kind)
	}
	if f.Status != "" {
		if !slices.Contains([]string{"paid", "refunded", "partially_refunded"}, f.Status) {
			return w, apperr.BadRequest("订单状态不正确")
		}
		w.Add("status = ?", f.Status)
	}
	if f.From != nil {
		w.Add("created_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("created_at < ?", *f.To)
	}
	return w, nil
}

// AdminListOrders 按下单时间倒序分页返回订单。
func (s *Service) AdminListOrders(ctx context.Context, f OrderFilter, p paging.Page) ([]Order, int, error) {
	w, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.d.Pool.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
	return list, total, err
}

// ExportOrders 按下单时间正序逐行回调；超过导出上限时在回调前返回 csvx.ErrTooLarge。
func (s *Service) ExportOrders(ctx context.Context, f OrderFilter, fn func(Order) error) error {
	w, err := f.where()
	if err != nil {
		return err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return err
	}
	if total > csvx.MaxRows {
		return csvx.ErrTooLarge
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE `+w.SQL()+` ORDER BY created_at, id`, w.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return err
		}
		if err := fn(o); err != nil {
			return err
		}
	}
	return rows.Err()
}
```

`internal/wallet/admin.go`：

```go
package wallet

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type LedgerFilter struct {
	UserID int64
	Kind   string
	From   *time.Time
	To     *time.Time // 不含
}

// AdminLedgerRow 是后台看到的流水（比用户端多用户与操作人）。
type AdminLedgerRow struct {
	LedgerRow
	UserID     int64  `json:"user_id"`
	OperatorID *int64 `json:"operator_id"`
}

var ledgerKinds = []string{string(KindRecharge), string(KindAdminAdjust), string(KindOrderPay), string(KindRefund)}

func (f LedgerFilter) where() (db.Where, error) {
	var w db.Where
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.Kind != "" {
		if !slices.Contains(ledgerKinds, f.Kind) {
			return w, apperr.BadRequest("流水类型不正确")
		}
		w.Add("kind = ?", f.Kind)
	}
	if f.From != nil {
		w.Add("created_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("created_at < ?", *f.To)
	}
	return w, nil
}

const adminLedgerCols = `id, amount, balance_after, kind, ref_type, ref_id, remark, created_at, user_id, operator_id`

func scanAdminLedger(row pgx.Row) (AdminLedgerRow, error) {
	var l AdminLedgerRow
	err := row.Scan(&l.ID, &l.Amount, &l.BalanceAfter, &l.Kind, &l.RefType, &l.RefID, &l.Remark, &l.CreatedAt, &l.UserID, &l.OperatorID)
	return l, err
}

func (s *Service) AdminListLedger(ctx context.Context, f LedgerFilter, p paging.Page) ([]AdminLedgerRow, int, error) {
	w, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.pool.Query(ctx, `SELECT `+adminLedgerCols+` FROM wallet_ledger WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY id DESC LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminLedgerRow, error) { return scanAdminLedger(r) })
	return list, total, err
}

// ExportLedger 按时间正序逐行回调；超过导出上限时在回调前返回 csvx.ErrTooLarge。
func (s *Service) ExportLedger(ctx context.Context, f LedgerFilter, fn func(AdminLedgerRow) error) error {
	w, err := f.where()
	if err != nil {
		return err
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM wallet_ledger WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return err
	}
	if total > csvx.MaxRows {
		return csvx.ErrTooLarge
	}
	rows, err := s.pool.Query(ctx, `SELECT `+adminLedgerCols+` FROM wallet_ledger WHERE `+w.SQL()+` ORDER BY id`, w.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		l, err := scanAdminLedger(rows)
		if err != nil {
			return err
		}
		if err := fn(l); err != nil {
			return err
		}
	}
	return rows.Err()
}
```

`internal/billing/admin.go`：

```go
package billing

import (
	"context"
	"fmt"
	"slices"
	"time"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/plugins/pay"
)

type RechargeFilter struct {
	UserID  int64
	Channel string
	Status  string
	From    *time.Time
	To      *time.Time // 不含
}

func (f RechargeFilter) where() (db.Where, error) {
	var w db.Where
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.Channel != "" {
		if !slices.Contains([]string{pay.ChannelWechat, pay.ChannelAlipay, ChannelManual}, f.Channel) {
			return w, apperr.BadRequest("充值渠道不正确")
		}
		w.Add("channel = ?", f.Channel)
	}
	if f.Status != "" {
		if !slices.Contains([]string{StatusPending, StatusPaid, StatusClosed}, f.Status) {
			return w, apperr.BadRequest("充值状态不正确")
		}
		w.Add("status = ?", f.Status)
	}
	if f.From != nil {
		w.Add("created_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("created_at < ?", *f.To)
	}
	return w, nil
}

func (s *Service) AdminList(ctx context.Context, f RechargeFilter, p paging.Page) ([]Recharge, int, error) {
	w, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM recharges WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	list, err := collectRecharges(s.d.Pool.Query(ctx, `SELECT `+rechargeCols+` FROM recharges WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...))
	return list, total, err
}

// Export 按创建时间正序逐行回调；超过导出上限时在回调前返回 csvx.ErrTooLarge。
func (s *Service) Export(ctx context.Context, f RechargeFilter, fn func(Recharge) error) error {
	w, err := f.where()
	if err != nil {
		return err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM recharges WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return err
	}
	if total > csvx.MaxRows {
		return csvx.ErrTooLarge
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT `+rechargeCols+` FROM recharges WHERE `+w.SQL()+` ORDER BY created_at, id`, w.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRecharge(rows)
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}
```

Run: `go test ./internal/rental/ ./internal/wallet/ ./internal/billing/`
Expected: PASS

- [ ] **Step 3: 写后台接口的失败测试**

`internal/adminapi/finance_test.go`：

```go
package adminapi_test

import (
	"io"
	"strings"
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
)

func TestFinanceListsAndExports(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	uid := e.renter("13600000001")
	e.place(uid)
	if _, err := e.billing.ManualRecharge(e.ctx, audit.Actor{AdminID: 1}, uid, 10000, "=HYPERLINK(\"x\")"); err != nil {
		t.Fatal(err)
	}
	fin := e.loginAs("fin", admin.RoleFinance)
	code, out := fin.do("GET", "/orders?user_id="+itoa(uid), nil)
	if code != 200 || out["total"].(float64) != 1 || out["items"].([]any)[0].(map[string]any)["user_id"].(float64) != float64(uid) {
		t.Fatalf("orders = %d %v", code, out)
	}
	if code, _ := fin.do("GET", "/orders?kind=gift", nil); code != 400 {
		t.Fatalf("bad kind = %d", code)
	}
	if code, out := fin.do("GET", "/orders?from=2026-01-02", nil); code != 200 || out["total"].(float64) != 0 {
		t.Fatalf("from next day = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/orders?to=2026-01-01", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("to same day (inclusive) = %d %v", code, out)
	}
	if code, _ := fin.do("GET", "/orders?from=2026-01-02&to=2026-01-01", nil); code != 400 {
		t.Fatalf("inverted range = %d", code)
	}
	if code, _ := fin.do("GET", "/orders?from=yesterday", nil); code != 400 {
		t.Fatalf("bad time = %d", code)
	}
	if code, out := fin.do("GET", "/ledger?user_id="+itoa(uid)+"&kind=order_pay", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("ledger = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/recharges?channel=manual", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("recharges = %d %v", code, out)
	}

	resp, _ := fin.send("GET", "/orders/export", nil)
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), `attachment; filename="orders-`) ||
		!strings.HasPrefix(text, "\xEF\xBB\xBF订单号,") || !strings.Contains(text, ",69.00,") || strings.Count(text, "\n") != 2 {
		t.Fatalf("orders csv = %d %v\n%s", resp.StatusCode, resp.Header, text)
	}
	resp, _ = fin.send("GET", "/recharges/export", nil)
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `'=HYPERLINK(`) || !strings.Contains(string(body), ",100.00,") {
		t.Fatalf("recharges csv must escape formulas and format yuan:\n%s", body)
	}
	resp, _ = fin.send("GET", "/ledger/export?kind=refund", nil)
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.Count(string(body), "\n") != 1 {
		t.Fatalf("empty ledger csv must still have a header:\n%s", body)
	}

	ops := e.loginAs("ops", admin.RoleOps)
	if code, out := ops.do("GET", "/orders", nil); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops orders = %d %v", code, out)
	}
}
```

Run: `go test ./internal/adminapi/ -run Finance`
Expected: 失败（404）。

- [ ] **Step 4: 实现后台接口**

`internal/adminapi/finance.go`：

```go
package adminapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/billing"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

var cst = time.FixedZone("CST", 8*3600)

func (s *Server) financeRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/orders", financeRoles, s.handleListOrders)
	s.handle(g, http.MethodGet, "/orders/export", financeRoles, s.handleExportOrders)
	s.handle(g, http.MethodGet, "/ledger", financeRoles, s.handleListLedger)
	s.handle(g, http.MethodGet, "/ledger/export", financeRoles, s.handleExportLedger)
	s.handle(g, http.MethodGet, "/recharges", financeRoles, s.handleListRecharges)
	s.handle(g, http.MethodGet, "/recharges/export", financeRoles, s.handleExportRecharges)
}

func parseTimeParam(name, v string, endOfDay bool) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", v, cst); err == nil {
		if endOfDay {
			t = t.AddDate(0, 0, 1)
		}
		t = t.UTC()
		return &t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		t = t.UTC()
		return &t, nil
	}
	return nil, apperr.BadRequest(name + " 的格式应为 2006-01-02 或 RFC3339")
}

// parseRange 解析 from / to：日期按北京时间，to 为日期时包含当天。
func parseRange(r *http.Request) (*time.Time, *time.Time, error) {
	q := r.URL.Query()
	from, err := parseTimeParam("from", q.Get("from"), false)
	if err != nil {
		return nil, nil, err
	}
	to, err := parseTimeParam("to", q.Get("to"), true)
	if err != nil {
		return nil, nil, err
	}
	if from != nil && to != nil && !from.Before(*to) {
		return nil, nil, apperr.BadRequest("from 必须早于 to")
	}
	return from, to, nil
}

// csvExport 以附件形式输出 CSV。run 必须在写出第一行之前完成行数检查（超过上限直接返回错误，此时仍可返回 JSON 错误）。
func (s *Server) csvExport(w http.ResponseWriter, r *http.Request, name string, header []string,
	run func(write func([]string) error) error) {
	var cw *csvx.Writer
	start := func() error {
		if cw != nil {
			return nil
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.csv"`, name, s.Clock.Now().In(cst).Format("20060102-150405")))
		var err error
		if cw, err = csvx.NewWriter(w); err != nil {
			return err
		}
		return cw.Write(header)
	}
	err := run(func(rec []string) error {
		if err := start(); err != nil {
			return err
		}
		return cw.Write(rec)
	})
	if err == nil {
		err = start()
	}
	if err != nil {
		if cw == nil {
			s.writeError(w, r, err)
			return
		}
		s.Log.Error("csv export aborted", "name", name, "err", err)
		return
	}
	if err := cw.Flush(); err != nil {
		s.Log.Error("csv export flush", "name", name, "err", err)
	}
}

func idText(id *int64) string {
	if id == nil {
		return ""
	}
	return strconv.FormatInt(*id, 10)
}

func (s *Server) orderFilter(r *http.Request) (rental.OrderFilter, error) {
	uid, err := queryInt(r, "user_id")
	if err != nil {
		return rental.OrderFilter{}, err
	}
	from, to, err := parseRange(r)
	if err != nil {
		return rental.OrderFilter{}, err
	}
	q := r.URL.Query()
	return rental.OrderFilter{UserID: uid, Kind: q.Get("kind"), Status: q.Get("status"), From: from, To: to}, nil
}

func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.orderFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Rental.AdminListOrders(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(orderViews(list), total))
}

func (s *Server) handleExportOrders(w http.ResponseWriter, r *http.Request) {
	f, err := s.orderFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.csvExport(w, r, "orders", []string{"订单号", "用户ID", "类型", "实例", "租期", "金额(元)", "已退(元)", "状态", "下单时间"},
		func(write func([]string) error) error {
			return s.Rental.ExportOrders(r.Context(), f, func(o rental.Order) error {
				return write([]string{o.ID, strconv.FormatInt(o.UserID, 10), o.Kind, o.InstanceID, string(o.Duration),
					csvx.Yuan(o.Amount), csvx.Yuan(o.RefundedAmount), o.Status, csvx.Time(o.CreatedAt)})
			})
		})
}

func (s *Server) ledgerFilter(r *http.Request) (wallet.LedgerFilter, error) {
	uid, err := queryInt(r, "user_id")
	if err != nil {
		return wallet.LedgerFilter{}, err
	}
	from, to, err := parseRange(r)
	if err != nil {
		return wallet.LedgerFilter{}, err
	}
	return wallet.LedgerFilter{UserID: uid, Kind: r.URL.Query().Get("kind"), From: from, To: to}, nil
}

func (s *Server) handleListLedger(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.ledgerFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Wallet.AdminListLedger(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleExportLedger(w http.ResponseWriter, r *http.Request) {
	f, err := s.ledgerFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.csvExport(w, r, "ledger", []string{"流水ID", "用户ID", "类型", "金额(元)", "变动后余额(元)", "关联类型", "关联单号", "操作人", "备注", "时间"},
		func(write func([]string) error) error {
			return s.Wallet.ExportLedger(r.Context(), f, func(l wallet.AdminLedgerRow) error {
				return write([]string{strconv.FormatInt(l.ID, 10), strconv.FormatInt(l.UserID, 10), string(l.Kind),
					csvx.Yuan(l.Amount), csvx.Yuan(l.BalanceAfter), l.RefType, l.RefID, idText(l.OperatorID), l.Remark, csvx.Time(l.CreatedAt)})
			})
		})
}

func (s *Server) rechargeFilter(r *http.Request) (billing.RechargeFilter, error) {
	uid, err := queryInt(r, "user_id")
	if err != nil {
		return billing.RechargeFilter{}, err
	}
	from, to, err := parseRange(r)
	if err != nil {
		return billing.RechargeFilter{}, err
	}
	q := r.URL.Query()
	return billing.RechargeFilter{UserID: uid, Channel: q.Get("channel"), Status: q.Get("status"), From: from, To: to}, nil
}

func (s *Server) handleListRecharges(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.rechargeFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Billing.AdminList(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleExportRecharges(w http.ResponseWriter, r *http.Request) {
	f, err := s.rechargeFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.csvExport(w, r, "recharges", []string{"充值单号", "用户ID", "渠道", "金额(元)", "状态", "渠道单号", "操作人", "备注", "创建时间", "支付时间", "关闭时间"},
		func(write func([]string) error) error {
			return s.Billing.Export(r.Context(), f, func(c billing.Recharge) error {
				return write([]string{c.ID, strconv.FormatInt(c.UserID, 10), c.Channel, csvx.Yuan(c.Amount), c.Status, c.ChannelTradeNo,
					idText(c.OperatorID), c.Remark, csvx.Time(c.CreatedAt), csvx.TimePtr(c.PaidAt), csvx.TimePtr(c.ClosedAt)})
			})
		})
}
```

`internal/adminapi/server.go` 的 `Router` 中，在 `s.userRoutes(g)` 之后加一行 `s.financeRoutes(g)`。

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/adminapi/`
Expected: PASS

- [ ] **Step 6: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/rental internal/wallet internal/billing internal/adminapi
git commit -m "feat(adminapi): order, ledger and recharge lists with CSV export (BOM, yuan, formula-safe)

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 20: 后台：价格与镜像、审计日志、仪表盘

**Files:**
- Create: `internal/catalog/admin.go`、`internal/catalog/admin_test.go`
- Create: `internal/audit/list.go`、`internal/audit/list_test.go`
- Modify: `internal/rental/admin_queries.go`、`internal/rental/admin_queries_test.go`（统计）
- Modify: `internal/billing/admin.go`、`internal/billing/admin_test.go`（到账金额）
- Create: `internal/adminapi/catalog.go`、`internal/adminapi/dashboard.go`、`internal/adminapi/catalog_test.go`、`internal/adminapi/dashboard_test.go`
- Modify: `internal/adminapi/server.go`（注册路由）、`internal/adminapi/helpers_test.go`（catalog 注入时钟）
- Modify: `internal/catalog/catalog.go`（`Service` 增加时钟）、`internal/app/app.go`

**Interfaces:**
- Consumes: Task 16 的 `fleet.Stats`、`db.Where`；Task 19 的 `cst`、`parseRange`、`queryInt`；Task 1 的 `audit.Actor`、`paging`。
- Produces（Task 21 使用）：
  - `catalog.MaxPrice = 100_000_000`、`catalog.AdminPrice{Duration, Amount, Enabled, UpdatedAt}`、`catalog.AdminProduct{Product; Status string; Prices []AdminPrice}`、`(*catalog.Service).AdminProducts(ctx) ([]AdminProduct, error)`、`SetPrice(ctx, actor, productID int64, d Duration, amount int64, enabled bool) (AdminPrice, error)`、`catalog.AdminImage{ID, Name, Ref, Description, Enabled, Sort, CreatedAt, UpdatedAt}`、`catalog.ImageInput{Name, Ref, Description string; Enabled bool; Sort int}`、`AdminImages(ctx) ([]AdminImage, error)`、`CreateImage(ctx, actor, in) (AdminImage, error)`、`UpdateImage(ctx, actor, id int64, in) (AdminImage, error)`。
  - `audit.Log{ID, ActorType, ActorID, Action, TargetType, TargetID, Diff json.RawMessage, IP, CreatedAt}`、`audit.Filter{ActorID int64; Action, TargetType, TargetID string; From, To *time.Time}`、`audit.List(ctx, q db.Querier, f Filter, p paging.Page) ([]Log, int, error)`。
  - `(*rental.Service).InstanceStateCounts(ctx) (map[string]int, error)`、`OrderAmount(ctx, from, to time.Time) (int64, error)`、`DailyOccupancy(ctx, from time.Time, days int, now time.Time) ([]float64, error)`（每天被租用的机器秒数，`machine_leases` 中 active / ended 时段与当天的交集，截止到 now）。
  - `(*billing.Service).PaidAmount(ctx, from, to time.Time) (int64, error)`（`paid_at` 落在区间内的充值金额之和，含手动入账）。
  - 接口：`GET /products`、`PUT /products/{id}/prices/{duration} {amount, enabled, totp}`、`GET /images`、`POST /images`、`PUT /images/{id}`（super；改价需二次验证）；`GET /audit?actor_id=&action=&target_type=&target_id=&from=&to=&page=`、`GET /dashboard`（all）。
  - 仪表盘响应：`{machines: {by_state, online, total}, instances: {<state>: n}, recharge_today, recharge_month, order_today, order_month, utilization: [{date: "2006-01-02", rate}]}`，金额单位为分，日期按北京时间，`utilization` 为近 30 天（含今天，今天按已过去的时长计算），`rate` 保留 4 位小数，最大为 1。

- [ ] **Step 1: 写服务层的失败测试**

`internal/catalog/admin_test.go`：

```go
package catalog_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
)

var superActor = audit.Actor{AdminID: 1, IP: "10.0.0.1"}

func TestSetPrice(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	for _, c := range []struct {
		d      catalog.Duration
		amount int64
	}{{"1h", 100}, {catalog.D1d, 0}, {catalog.D1d, catalog.MaxPrice + 1}} {
		if _, err := svc.SetPrice(ctx, superActor, pid, c.d, c.amount, true); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
			t.Fatalf("SetPrice(%s, %d) = %v", c.d, c.amount, err)
		}
	}
	if _, err := svc.SetPrice(ctx, superActor, 9999, catalog.D1d, 100, true); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown product = %v", err)
	}
	p, err := svc.SetPrice(ctx, superActor, pid, catalog.D1d, 7900, true)
	if err != nil || p.Amount != 7900 || !p.Enabled {
		t.Fatalf("set = %+v, %v", p, err)
	}
	if amount, _ := svc.Quote(ctx, pool, pid, catalog.D1d); amount != 7900 {
		t.Fatalf("quote = %d", amount)
	}
	if _, err := svc.SetPrice(ctx, superActor, pid, catalog.D1w, 41400, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Quote(ctx, pool, pid, catalog.D1w); err == nil {
		t.Fatal("disabled price must not be quotable")
	}
	products, err := svc.AdminProducts(ctx)
	if err != nil || len(products) != 1 || len(products[0].Prices) != 3 || products[0].Prices[1].Enabled {
		t.Fatalf("admin products = %+v, %v", products, err)
	}
	var diff string
	_ = pool.QueryRow(ctx, `SELECT diff::text FROM audit_logs WHERE action = 'price.update' ORDER BY id LIMIT 1`).Scan(&diff)
	if diff == "" || !strings.Contains(diff, `"before"`) || !strings.Contains(diff, "6900") || !strings.Contains(diff, "7900") {
		t.Fatalf("price audit diff = %s", diff)
	}
}

func TestImagesAdmin(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	ctx := context.Background()
	for _, in := range []catalog.ImageInput{
		{Name: "", Ref: "registry/x:1"}, {Name: "x", Ref: ""}, {Name: "x", Ref: "has space:1"},
	} {
		if _, err := svc.CreateImage(ctx, superActor, in); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
			t.Fatalf("CreateImage(%+v) = %v", in, err)
		}
	}
	img, err := svc.CreateImage(ctx, superActor, catalog.ImageInput{Name: "SGLang", Ref: "registry.spark.internal/base/sglang:0.5", Enabled: true, Sort: 40})
	if err != nil || img.ID == 0 || img.Ref != "registry.spark.internal/base/sglang:0.5" {
		t.Fatalf("create = %+v, %v", img, err)
	}
	if _, err := svc.UpdateImage(ctx, superActor, img.ID, catalog.ImageInput{Name: "SGLang", Ref: img.Ref, Enabled: false, Sort: 40}); err != nil {
		t.Fatal(err)
	}
	public, _ := svc.ListImages(ctx)
	for _, p := range public {
		if p.ID == img.ID {
			t.Fatal("disabled image must be hidden from users")
		}
	}
	all, err := svc.AdminImages(ctx)
	if err != nil || len(all) != 4 {
		t.Fatalf("admin images = %d, %v", len(all), err)
	}
	if _, err := svc.UpdateImage(ctx, superActor, 9999, catalog.ImageInput{Name: "x", Ref: "y"}); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("update missing = %v", err)
	}
}

```

`internal/audit/list_test.go`：

```go
package audit_test

import (
	"context"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func TestList(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	entries := []struct {
		at time.Time
		e  audit.Entry
	}{
		{dbtest.T0, audit.Actor{AdminID: 1, IP: "a"}.Entry("machine.register", "machine", "1", map[string]int{"x": 1})},
		{dbtest.T0.Add(time.Hour), audit.Actor{AdminID: 2, IP: "b"}.Entry("instance.extend", "instance", "ins-1", nil)},
		{dbtest.T0.Add(2 * time.Hour), audit.Actor{AdminID: 1, IP: "a"}.Entry("instance.extend", "instance", "ins-2", nil)},
	}
	for _, it := range entries {
		if err := audit.Record(ctx, pool, it.at, it.e); err != nil {
			t.Fatal(err)
		}
	}
	all, total, err := audit.List(ctx, pool, audit.Filter{}, paging.Page{})
	if err != nil || total != 3 || all[0].TargetID != "ins-2" || all[2].Action != "machine.register" || string(all[2].Diff) != `{"x": 1}` {
		t.Fatalf("all = %+v, %d, %v", all, total, err)
	}
	if list, total, _ := audit.List(ctx, pool, audit.Filter{ActorID: 1, Action: "instance.extend"}, paging.Page{}); total != 1 || list[0].TargetID != "ins-2" {
		t.Fatalf("actor+action = %+v", list)
	}
	from, to := dbtest.T0.Add(30*time.Minute), dbtest.T0.Add(90*time.Minute)
	if list, total, _ := audit.List(ctx, pool, audit.Filter{From: &from, To: &to}, paging.Page{}); total != 1 || list[0].TargetID != "ins-1" {
		t.Fatalf("time range = %+v", list)
	}
	if _, total, _ := audit.List(ctx, pool, audit.Filter{TargetType: "instance", TargetID: "ins-1"}, paging.Page{}); total != 1 {
		t.Fatalf("target = %d", total)
	}
}
```

在 `internal/rental/admin_queries_test.go` 末尾追加：

```go
func TestDashboardStats(t *testing.T) {
	e := newEnv(t, 2)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // 计时从 T0 开始，到期 T0+24h，占用时段上界为 T0+31h（含 6 小时保留与 1 小时洗机缓冲）
	e.place(catalog.D1w)
	counts, err := e.svc.InstanceStateCounts(e.ctx)
	if err != nil || counts[rental.StateRunning] != 1 || counts[rental.StateCreating] != 1 {
		t.Fatalf("counts = %v, %v", counts, err)
	}
	amount, err := e.svc.OrderAmount(e.ctx, dbtest.T0, dbtest.T0.Add(time.Hour))
	if err != nil || amount != 6900+41400 {
		t.Fatalf("order amount = %d, %v", amount, err)
	}
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	e.ack(true)
	now := dbtest.T0.Add(36 * time.Hour)
	occ, err := e.svc.DailyOccupancy(e.ctx, dbtest.T0, 2, now)
	if err != nil || len(occ) != 2 {
		t.Fatalf("occupancy = %v, %v", occ, err)
	}
	// 第一个实例在 T0 释放（时段为空）；第二个实例（1 周）在 e.ack 后开始运行，两天都被占用，第二天截止到 now
	if occ[0] != 86400 || occ[1] != 12*3600 {
		t.Fatalf("occupancy = %v, want [86400 43200]", occ)
	}
}
```

（若尚未 import `"spark-cloud/internal/platform/dbtest"`、`"time"`，补上。）

在 `internal/billing/admin_test.go` 末尾追加：

```go
func TestPaidAmount(t *testing.T) {
	e := newEnv(t)
	r := e.create(pay.ChannelWechat, 300)
	if err := e.svc.Settle(e.ctx, pay.ChannelWechat, paid(r)); err != nil {
		t.Fatal(err)
	}
	e.create(pay.ChannelWechat, 999) // 未支付不计入
	if _, err := e.svc.ManualRecharge(e.ctx, audit.Actor{AdminID: 1}, e.userID, 700, "转账"); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.PaidAmount(e.ctx, e.clk.Now().Add(-time.Hour), e.clk.Now().Add(time.Hour))
	if err != nil || got != 1000 {
		t.Fatalf("paid = %d, %v", got, err)
	}
}
```

Run: `go test ./internal/catalog/ ./internal/audit/ ./internal/rental/ ./internal/billing/`
Expected: 编译失败。

- [ ] **Step 2: 实现服务层**

`internal/catalog/admin.go`：

```go
package catalog

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

const MaxPrice = 100_000_000 // 单个租期价格上限（分）

type AdminPrice struct {
	Duration  Duration  `json:"duration"`
	Amount    int64     `json:"amount"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

type AdminProduct struct {
	Product
	Status string       `json:"status"`
	Prices []AdminPrice `json:"prices"`
}

// AdminProducts 返回全部商品及全部租期价格（含已停用的）。
func (s *Service) AdminProducts(ctx context.Context) ([]AdminProduct, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, code, name, spec, status FROM products ORDER BY id`)
	if err != nil {
		return nil, err
	}
	products, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminProduct, error) {
		var p AdminProduct
		err := r.Scan(&p.ID, &p.Code, &p.Name, &p.Spec, &p.Status)
		return p, err
	})
	if err != nil {
		return nil, err
	}
	for i := range products {
		rows, err := s.pool.Query(ctx, `SELECT duration, amount, enabled, updated_at FROM prices WHERE product_id = $1
			ORDER BY array_position(ARRAY['1d', '1w', '1m'], duration)`, products[i].ID)
		if err != nil {
			return nil, err
		}
		if products[i].Prices, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminPrice, error) {
			var p AdminPrice
			err := r.Scan(&p.Duration, &p.Amount, &p.Enabled, &p.UpdatedAt)
			return p, err
		}); err != nil {
			return nil, err
		}
	}
	return products, nil
}

// SetPrice 设置某商品某租期的价格与是否在售；价格变动写审计（含修改前后）。
func (s *Service) SetPrice(ctx context.Context, actor audit.Actor, productID int64, d Duration, amount int64, enabled bool) (AdminPrice, error) {
	if !d.Valid() {
		return AdminPrice{}, apperr.BadRequest("租期只能是 1d、1w 或 1m")
	}
	if amount <= 0 || amount > MaxPrice {
		return AdminPrice{}, apperr.BadRequest("价格需在 0.01 元到 100 万元之间")
	}
	var out AdminPrice
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var before map[string]any
		var oldAmount int64
		var oldEnabled bool
		err := tx.QueryRow(ctx, `SELECT amount, enabled FROM prices WHERE product_id = $1 AND duration = $2 FOR UPDATE`,
			productID, string(d)).Scan(&oldAmount, &oldEnabled)
		switch {
		case err == nil:
			before = map[string]any{"amount": oldAmount, "enabled": oldEnabled}
		case !db.IsNoRows(err):
			return err
		}
		now := s.clock.Now()
		err = tx.QueryRow(ctx, `INSERT INTO prices (product_id, duration, amount, enabled, updated_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (product_id, duration) DO UPDATE SET amount = EXCLUDED.amount, enabled = EXCLUDED.enabled, updated_at = EXCLUDED.updated_at
			RETURNING duration, amount, enabled, updated_at`, productID, string(d), amount, enabled, now).
			Scan(&out.Duration, &out.Amount, &out.Enabled, &out.UpdatedAt)
		if db.IsForeignKeyViolation(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("price.update", "product", strconv.FormatInt(productID, 10), map[string]any{
			"duration": d, "before": before, "after": map[string]any{"amount": amount, "enabled": enabled},
		}))
	})
	return out, err
}

type AdminImage struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Ref         string    `json:"ref"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	Sort        int       `json:"sort"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ImageInput struct {
	Name        string
	Ref         string
	Description string
	Enabled     bool
	Sort        int
}

func normalizeImage(in ImageInput) (ImageInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Ref = strings.TrimSpace(in.Ref)
	in.Description = strings.TrimSpace(in.Description)
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 64 {
		return in, apperr.BadRequest("请填写镜像名称（最多 64 个字符）")
	}
	if in.Ref == "" || len(in.Ref) > 256 || strings.IndexFunc(in.Ref, unicode.IsSpace) >= 0 {
		return in, apperr.BadRequest("镜像地址不能为空且不能包含空白字符")
	}
	if utf8.RuneCountInString(in.Description) > 500 {
		return in, apperr.BadRequest("镜像说明最多 500 个字符")
	}
	if in.Sort < -10000 || in.Sort > 10000 {
		return in, apperr.BadRequest("排序值需在 -10000～10000 之间")
	}
	return in, nil
}

const adminImageCols = `id, name, ref, description, enabled, sort, created_at, updated_at`

func scanAdminImage(row pgx.Row) (AdminImage, error) {
	var i AdminImage
	err := row.Scan(&i.ID, &i.Name, &i.Ref, &i.Description, &i.Enabled, &i.Sort, &i.CreatedAt, &i.UpdatedAt)
	if db.IsNoRows(err) {
		return AdminImage{}, apperr.ErrNotFound
	}
	return i, err
}

func (s *Service) AdminImages(ctx context.Context) ([]AdminImage, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+adminImageCols+` FROM images ORDER BY sort, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminImage, error) { return scanAdminImage(r) })
}

func (s *Service) CreateImage(ctx context.Context, actor audit.Actor, in ImageInput) (AdminImage, error) {
	in, err := normalizeImage(in)
	if err != nil {
		return AdminImage{}, err
	}
	var img AdminImage
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		now := s.clock.Now()
		var err error
		if img, err = scanAdminImage(tx.QueryRow(ctx, `INSERT INTO images (name, ref, description, enabled, sort, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6) RETURNING `+adminImageCols, in.Name, in.Ref, in.Description, in.Enabled, in.Sort, now)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("image.create", "image", strconv.FormatInt(img.ID, 10), in))
	})
	return img, err
}

func (s *Service) UpdateImage(ctx context.Context, actor audit.Actor, id int64, in ImageInput) (AdminImage, error) {
	in, err := normalizeImage(in)
	if err != nil {
		return AdminImage{}, err
	}
	var img AdminImage
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		now := s.clock.Now()
		var err error
		if img, err = scanAdminImage(tx.QueryRow(ctx, `UPDATE images SET name = $2, ref = $3, description = $4, enabled = $5, sort = $6, updated_at = $7
			WHERE id = $1 RETURNING `+adminImageCols, id, in.Name, in.Ref, in.Description, in.Enabled, in.Sort, now)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("image.update", "image", strconv.FormatInt(id, 10), in))
	})
	return img, err
}
```

`internal/catalog/catalog.go`：给 `Service` 增加时钟（改价与镜像的时间戳也要来自 `clock.Clock`），不改动现有构造函数的调用方式：

```go
type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool, clock: clock.Real{}} }

// WithClock 替换时钟（测试与 app 组装时注入同一个时钟）。
func (s *Service) WithClock(c clock.Clock) *Service {
	s.clock = c
	return s
}
```

（import 增加 `"spark-cloud/internal/platform/clock"`。）`internal/app/app.go` 中 `c := catalog.NewService(pool)` 改为 `c := catalog.NewService(pool).WithClock(clk)`；`internal/adminapi/helpers_test.go` 中同样改为 `catalog.NewService(pool).WithClock(clk)`。

`internal/audit/list.go`：

```go
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type Log struct {
	ID         int64           `json:"id"`
	ActorType  string          `json:"actor_type"`
	ActorID    int64           `json:"actor_id"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Diff       json.RawMessage `json:"diff"`
	IP         string          `json:"ip"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Filter struct {
	ActorID    int64
	Action     string
	TargetType string
	TargetID   string
	From       *time.Time
	To         *time.Time // 不含
}

// List 按时间倒序分页查询审计日志（只读；审计日志没有修改与删除接口）。
func List(ctx context.Context, q db.Querier, f Filter, p paging.Page) ([]Log, int, error) {
	var w db.Where
	if f.ActorID > 0 {
		w.Add("actor_id = ?", f.ActorID)
	}
	if f.Action != "" {
		w.Add("action = ?", f.Action)
	}
	if f.TargetType != "" {
		w.Add("target_type = ?", f.TargetType)
	}
	if f.TargetID != "" {
		w.Add("target_id = ?", f.TargetID)
	}
	if f.From != nil {
		w.Add("created_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("created_at < ?", *f.To)
	}
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := q.Query(ctx, `SELECT id, actor_type, actor_id, action, target_type, target_id, diff, ip, created_at
		FROM audit_logs WHERE `+w.SQL()+fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, n+1, n+2),
		append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Log, error) {
		var l Log
		var diff []byte
		err := r.Scan(&l.ID, &l.ActorType, &l.ActorID, &l.Action, &l.TargetType, &l.TargetID, &diff, &l.IP, &l.CreatedAt)
		l.Diff = diff
		return l, err
	})
	return list, total, err
}
```

在 `internal/rental/admin_queries.go` 末尾追加：

```go
// InstanceStateCounts 按状态统计实例数量。
func (s *Service) InstanceStateCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT state, count(*) FROM instances GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

// OrderAmount 返回 [from, to) 内下单金额之和（分，未扣除退款）。
func (s *Service) OrderAmount(ctx context.Context, from, to time.Time) (int64, error) {
	var sum int64
	err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0) FROM orders WHERE created_at >= $1 AND created_at < $2`, from, to).Scan(&sum)
	return sum, err
}

// DailyOccupancy 返回从 from 起连续 days 天、每天被租用的机器总秒数（machine_leases 中 active / ended 时段与当天的交集，截止到 now）。
func (s *Service) DailyOccupancy(ctx context.Context, from time.Time, days int, now time.Time) ([]float64, error) {
	rows, err := s.d.Pool.Query(ctx, `
		SELECT COALESCE(sum(GREATEST(EXTRACT(EPOCH FROM
				LEAST(upper(l.period), d.day + interval '24 hours', $3::timestamptz) - GREATEST(lower(l.period), d.day)), 0)), 0)::float8
		FROM generate_series($1::timestamptz, $1::timestamptz + ($2::int - 1) * interval '24 hours', interval '24 hours') AS d(day)
		LEFT JOIN machine_leases l ON l.status IN ('active', 'ended') AND l.period && tstzrange(d.day, d.day + interval '24 hours')
		GROUP BY d.day ORDER BY d.day`, from, days, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[float64])
}
```

在 `internal/billing/admin.go` 末尾追加：

```go
// PaidAmount 返回 paid_at 落在 [from, to) 内的充值金额之和（分，含手动入账）。
func (s *Service) PaidAmount(ctx context.Context, from, to time.Time) (int64, error) {
	var sum int64
	err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0) FROM recharges
		WHERE status = 'paid' AND paid_at >= $1 AND paid_at < $2`, from, to).Scan(&sum)
	return sum, err
}
```

Run: `go test ./internal/catalog/ ./internal/audit/ ./internal/rental/ ./internal/billing/`
Expected: PASS

- [ ] **Step 3: 写后台接口的失败测试**

`internal/adminapi/catalog_test.go`：

```go
package adminapi_test

import (
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/platform/dbtest"
)

func TestPricesAndImagesEndpoints(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	pid := itoa(dbtest.ProductID(t, e.pool))
	if code, out := root.do("GET", "/products", nil); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("products = %d %v", code, out)
	}
	if code, out := root.do("PUT", "/products/"+pid+"/prices/1d", map[string]any{"amount": 7900, "enabled": true}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("price without totp = %d %v", code, out)
	}
	if code, out := root.do("PUT", "/products/"+pid+"/prices/1d", map[string]any{"amount": 7900, "enabled": true, "totp": root.totp()}); code != 200 || out["amount"].(float64) != 7900 {
		t.Fatalf("price = %d %v", code, out)
	}
	if code, out := root.do("PUT", "/products/"+pid+"/prices/2h", map[string]any{"amount": 100, "enabled": true}); code != 400 {
		t.Fatalf("bad duration = %d %v", code, out)
	}
	code, out := root.do("POST", "/images", map[string]any{"name": "SGLang", "ref": "registry.spark.internal/base/sglang:0.5", "enabled": true, "sort": 40})
	if code != 201 {
		t.Fatalf("create image = %d %v", code, out)
	}
	id := itoa(int64(out["id"].(float64)))
	if code, out := root.do("PUT", "/images/"+id, map[string]any{"name": "SGLang", "ref": "registry.spark.internal/base/sglang:0.6", "enabled": false}); code != 200 || out["enabled"] != false {
		t.Fatalf("update image = %d %v", code, out)
	}
	if code, out := root.do("GET", "/images", nil); code != 200 || len(out["items"].([]any)) != 4 {
		t.Fatalf("images = %d %v", code, out)
	}
	ops := e.loginAs("ops", admin.RoleOps)
	if code, out := ops.do("GET", "/products", nil); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops products = %d %v", code, out)
	}
}

func TestAuditEndpoint(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	fin := e.loginAs("fin", admin.RoleFinance)
	if code, out := fin.do("GET", "/audit?action=admin.totp_bind", nil); code != 200 || out["total"].(float64) != 2 {
		t.Fatalf("audit = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/audit?actor_id="+itoa(root.admin.ID)+"&action=admin.totp_bind", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("audit by actor = %d %v", code, out)
	}
	if code, _ := fin.do("GET", "/audit?from=bad", nil); code != 400 {
		t.Fatalf("bad from = %d", code)
	}
}
```

`internal/adminapi/dashboard_test.go`：

```go
package adminapi_test

import (
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/dbtest"
)

func TestDashboard(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	e.machine("SN2")
	uid := e.renter("13600000001")
	e.place(uid)
	e.ack()
	if _, err := e.billing.ManualRecharge(e.ctx, audit.Actor{AdminID: 1}, uid, 5000, "转账"); err != nil {
		t.Fatal(err)
	}
	ops := e.loginAs("ops", admin.RoleOps)
	code, out := ops.do("GET", "/dashboard", nil)
	if code != 200 {
		t.Fatalf("dashboard = %d %v", code, out)
	}
	machines := out["machines"].(map[string]any)
	instances := out["instances"].(map[string]any)
	util := out["utilization"].([]any)
	if machines["total"].(float64) != 2 || instances["running"].(float64) != 1 ||
		out["order_today"].(float64) != 6900 || out["order_month"].(float64) != 6900 ||
		out["recharge_today"].(float64) != 5000 || out["recharge_month"].(float64) != 5000 || len(util) != 30 {
		t.Fatalf("dashboard = %v", out)
	}
	today := util[29].(map[string]any)
	// T0 = 北京时间 2026-01-01 08:00；登录等操作把时钟推进了 1 分钟，今天已过去约 8 小时，2 台机器中 1 台在租
	if today["date"] != dbtest.T0.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02") || today["rate"].(float64) <= 0 || today["rate"].(float64) > 0.5 {
		t.Fatalf("today utilization = %v", today)
	}
}
```

Run: `go test ./internal/adminapi/ -run 'Prices|Audit|Dashboard'`
Expected: 失败（404）。

- [ ] **Step 4: 实现后台接口**

`internal/adminapi/catalog.go`：

```go
package adminapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/httpx"
)

func (s *Server) catalogRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/products", superOnly, s.handleAdminProducts)
	s.handle(g, http.MethodPut, "/products/{id}/prices/{duration}", superOnly, s.handleSetPrice)
	s.handle(g, http.MethodGet, "/images", superOnly, s.handleAdminImages)
	s.handle(g, http.MethodPost, "/images", superOnly, s.handleCreateImage)
	s.handle(g, http.MethodPut, "/images/{id}", superOnly, s.handleUpdateImage)
	s.handle(g, http.MethodGet, "/audit", allRoles, s.handleAudit)
	s.handle(g, http.MethodGet, "/dashboard", allRoles, s.handleDashboard)
}

func (s *Server) handleAdminProducts(w http.ResponseWriter, r *http.Request) {
	list, err := s.Catalog.AdminProducts(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleSetPrice(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Amount  int64  `json:"amount"`
		Enabled bool   `json:"enabled"`
		TOTP    string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.stepUp(r, req.TOTP); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.Catalog.SetPrice(r.Context(), s.actor(r), id, catalog.Duration(chi.URLParam(r, "duration")), req.Amount, req.Enabled)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (s *Server) handleAdminImages(w http.ResponseWriter, r *http.Request) {
	list, err := s.Catalog.AdminImages(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

type imageRequest struct {
	Name        string `json:"name"`
	Ref         string `json:"ref"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Sort        int    `json:"sort"`
}

func (q imageRequest) input() catalog.ImageInput {
	return catalog.ImageInput{Name: q.Name, Ref: q.Ref, Description: q.Description, Enabled: q.Enabled, Sort: q.Sort}
}

func (s *Server) handleCreateImage(w http.ResponseWriter, r *http.Request) {
	var req imageRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	img, err := s.Catalog.CreateImage(r.Context(), s.actor(r), req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, img)
}

func (s *Server) handleUpdateImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req imageRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	img, err := s.Catalog.UpdateImage(r.Context(), s.actor(r), id, req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, img)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	actorID, err := queryInt(r, "actor_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	from, to, err := parseRange(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	list, total, err := audit.List(r.Context(), s.Fleet.Pool(), audit.Filter{ActorID: actorID, Action: q.Get("action"),
		TargetType: q.Get("target_type"), TargetID: q.Get("target_id"), From: from, To: to}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}
```

`internal/adminapi/dashboard.go`：

```go
package adminapi

import (
	"context"
	"math"
	"net/http"
	"time"

	"spark-cloud/internal/platform/httpx"
)

type dayRate struct {
	Date string  `json:"date"`
	Rate float64 `json:"rate"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.Clock.Now()
	local := now.In(cst)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, cst)
	month := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, cst)
	tomorrow, nextMonth := today.AddDate(0, 0, 1), month.AddDate(0, 1, 0)

	machines, err := s.Fleet.Stats(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	instances, err := s.Rental.InstanceStateCounts(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var firstErr error
	sum := func(f func(context.Context, time.Time, time.Time) (int64, error), from, to time.Time) int64 {
		if firstErr != nil {
			return 0
		}
		v, err := f(ctx, from, to)
		if err != nil {
			firstErr = err
		}
		return v
	}
	rechargeToday := sum(s.Billing.PaidAmount, today, tomorrow)
	rechargeMonth := sum(s.Billing.PaidAmount, month, nextMonth)
	orderToday := sum(s.Rental.OrderAmount, today, tomorrow)
	orderMonth := sum(s.Rental.OrderAmount, month, nextMonth)
	if firstErr != nil {
		s.writeError(w, r, firstErr)
		return
	}
	const days = 30
	from := today.AddDate(0, 0, -(days - 1))
	occ, err := s.Rental.DailyOccupancy(ctx, from, days, now)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	util := make([]dayRate, len(occ))
	for i, secs := range occ {
		day := from.AddDate(0, 0, i)
		span := 86400.0
		if i == len(occ)-1 {
			span = now.Sub(day).Seconds()
		}
		rate := 0.0
		if machines.Total > 0 && span > 0 {
			rate = math.Min(1, secs/(float64(machines.Total)*span))
		}
		util[i] = dayRate{Date: day.Format("2006-01-02"), Rate: math.Round(rate*10000) / 10000}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"machines": machines, "instances": instances,
		"recharge_today": rechargeToday, "recharge_month": rechargeMonth, "order_today": orderToday, "order_month": orderMonth,
		"utilization": util,
	})
}
```

`internal/adminapi/server.go` 的 `Router` 中，在 `s.financeRoutes(g)` 之后加一行 `s.catalogRoutes(g)`。

- [ ] **Step 5: 运行确认通过**

Run: `go test ./internal/adminapi/`
Expected: PASS

- [ ] **Step 6: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/catalog internal/audit internal/rental internal/billing internal/adminapi
git commit -m "feat(adminapi): price and image management, audit log query, dashboard with utilization

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 21: 组装后台服务、权限矩阵测试、端到端测试与文档

**Files:**
- Modify: `internal/app/app.go`（创建 `admin.Service` 与后台 Handler）
- Modify: `cmd/platform/main.go`（同时启动用户与后台两个 HTTP 服务）
- Create: `internal/adminapi/permissions_test.go`
- Modify: `internal/app/e2e_test.go`（端到端：管理员登录 → 登记机器 → 用户充值下单 → 后台退款并释放）
- Modify: `README.md`
- Create: `docs/superpowers/followups-plan4.md`
- Modify: `docs/superpowers/followups-plan1.md`（标记本计划已处理的事项）

**Interfaces:**
- Consumes: Task 15～20 的 `adminapi.Server` 与全部路由；Task 4 的 `admin.Service`。
- Produces：`app.App.Admins *admin.Service`、`app.App.AdminHandler http.Handler`；`platform` 进程监听 `SPARK_HTTP_ADDR`（用户）与 `SPARK_ADMIN_ADDR`（后台）。

- [ ] **Step 1: 写权限矩阵测试（失败）**

`internal/adminapi/permissions_test.go`：

```go
package adminapi_test

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"spark-cloud/internal/admin"
)

var (
	all     = []admin.Role{admin.RoleSuper, admin.RoleOps, admin.RoleFinance}
	ops     = []admin.Role{admin.RoleSuper, admin.RoleOps}
	finance = []admin.Role{admin.RoleSuper, admin.RoleFinance}
	super   = []admin.Role{admin.RoleSuper}
)

// expectedRoutes 是规格 §2 权限矩阵展开到每个接口后的期望（不从实现读取，防止实现改错而测试跟着错）。
var expectedRoutes = []struct {
	method, path string
	roles        []admin.Role
}{
	{"GET", "/me", all}, {"POST", "/me/password", all},
	{"GET", "/admins", super}, {"POST", "/admins", super}, {"PUT", "/admins/{id}", super}, {"POST", "/admins/{id}/reset-totp", super},

	{"GET", "/machines", ops}, {"POST", "/machines", ops}, {"POST", "/machines/import", ops}, {"GET", "/machines/{id}", ops},
	{"POST", "/machines/{id}/maintenance", ops}, {"POST", "/machines/{id}/power-cycle", ops},
	{"GET", "/pdus", ops}, {"POST", "/pdus", ops}, {"PUT", "/pdus/{id}", ops},
	{"GET", "/alerts", all}, {"POST", "/alerts/{id}/resolve", all},

	{"GET", "/instances", all}, {"GET", "/instances/{id}", all}, {"GET", "/instances/{id}/refund-quote", all},
	{"POST", "/instances/{id}/extend", ops}, {"POST", "/instances/{id}/migrate", ops}, {"POST", "/instances/{id}/rewipe", ops},
	{"POST", "/instances/{id}/refund-release", finance},

	{"GET", "/users", all}, {"GET", "/users/{id}", all},
	{"POST", "/users/{id}/ban", ops}, {"POST", "/users/{id}/unban", ops},
	{"GET", "/kyc/pending", ops}, {"GET", "/kyc/{id}", ops}, {"POST", "/kyc/{id}/review", ops}, {"GET", "/files/*", ops},
	{"POST", "/users/{id}/adjust", finance}, {"POST", "/users/{id}/manual-recharge", finance},

	{"GET", "/orders", finance}, {"GET", "/orders/export", finance}, {"GET", "/ledger", finance}, {"GET", "/ledger/export", finance},
	{"GET", "/recharges", finance}, {"GET", "/recharges/export", finance},

	{"GET", "/products", super}, {"PUT", "/products/{id}/prices/{duration}", super},
	{"GET", "/images", super}, {"POST", "/images", super}, {"PUT", "/images/{id}", super},
	{"GET", "/audit", all}, {"GET", "/dashboard", all},
}

func roleKey(roles []admin.Role) string {
	s := make([]string, len(roles))
	for i, r := range roles {
		s[i] = string(r)
	}
	sort.Strings(s)
	return strings.Join(s, "+")
}

func TestRouteTableMatchesPermissionMatrix(t *testing.T) {
	e := newEnv(t)
	got := map[string]string{}
	for _, r := range e.srv.Routes() {
		got[r.Method+" "+r.Pattern] = roleKey(r.Roles)
	}
	want := map[string]string{}
	for _, r := range expectedRoutes {
		want[r.method+" /admin/api/v1"+r.path] = roleKey(r.roles)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: roles = %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected protected route %s (add it to the permission matrix)", k)
		}
	}
}

// TestPermissionMatrix 用三个角色逐个请求每个接口：无权限必须是 403 forbidden，有权限则不能是 forbidden
// （参数不全导致的 400 / 404、需要二次验证的 403 totp_required 都算放行）。
func TestPermissionMatrix(t *testing.T) {
	e := newEnv(t)
	clients := map[admin.Role]*client{
		admin.RoleSuper:   e.loginAs("root", admin.RoleSuper),
		admin.RoleOps:     e.loginAs("ops", admin.RoleOps),
		admin.RoleFinance: e.loginAs("fin", admin.RoleFinance),
	}
	fill := strings.NewReplacer("{id}", "999999", "{duration}", "1d", "*", "2026/01/abcdefghijklmnopqrstuvwxyz012345.png")
	for _, r := range expectedRoutes {
		path := fill.Replace(r.path)
		for role, c := range clients {
			var body any
			if r.method != "GET" {
				body = map[string]any{}
			}
			code, out := c.do(r.method, path, body)
			allowed := slices.Contains(r.roles, role)
			name := fmt.Sprintf("%s %s as %s", r.method, path, role)
			switch {
			case allowed && codeOf(out) == "forbidden":
				t.Errorf("%s: allowed role got forbidden", name)
			case !allowed && (code != 403 || codeOf(out) != "forbidden"):
				t.Errorf("%s: want 403 forbidden, got %d %v", name, code, out)
			case code == 401 || code >= 500:
				t.Errorf("%s: unexpected %d %v", name, code, out)
			}
		}
	}
	if code, _ := e.anon().do("GET", "/dashboard", nil); code != 401 {
		t.Fatalf("anonymous dashboard = %d", code)
	}
}
```

Run: `go test ./internal/adminapi/ -run 'RouteTable|PermissionMatrix'`
Expected: PASS（若 Task 15～20 的角色与规格一致）。若失败，按规格 §2 修正对应路由的角色集合，而不是修改测试。

- [ ] **Step 2: 写端到端测试（失败）**

`internal/app/e2e_test.go` 末尾追加（import 增加 `"fmt"`、`"net/url"`、`"spark-cloud/internal/admin"`、`"spark-cloud/internal/audit"`）：

```go
// TestAdminRefundEndToEnd 走通：命令行创建管理员 → 后台登录绑定 TOTP → 登记机器并上线 →
// 用户登录、实名、充值、下单 → 开通 → 后台退款并释放 → 洗机 → 余额、流水、审计正确。
func TestAdminRefundEndToEnd(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
	a, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	userTS := httptest.NewServer(a.Handler)
	defer userTS.Close()
	adminTS := httptest.NewServer(a.AdminHandler)
	defer adminTS.Close()

	if _, err := a.Admins.Create(ctx, audit.Actor{}, "root", "correct horse battery", admin.RoleSuper); err != nil {
		t.Fatal(err)
	}
	adminJar, _ := cookiejar.New(nil)
	ac := &client{t: t, base: adminTS.URL + "/admin/api/v1", http: &http.Client{Jar: adminJar}}
	login := ac.do("POST", "/auth/login", map[string]string{"username": "root", "password": "correct horse battery"})
	u, _ := url.Parse(login["otpauth_uri"].(string))
	secret := u.Query().Get("secret")
	code := func() string {
		clk.Advance(30 * time.Second)
		c, _ := admin.TOTPCode(secret, admin.TOTPStep(clk.Now()))
		return c
	}
	ac.do("POST", "/auth/totp/setup", map[string]string{"code": code()})

	products := ac.do("GET", "/products", nil)["items"].([]any)
	pid := products[0].(map[string]any)["id"]
	m := ac.do("POST", "/machines", map[string]any{"serial": "SN-E2E", "rack": "A01", "slot": "01", "lan_ip": "10.0.1.1", "product_id": pid})
	mid := int64(m["id"].(float64))
	if _, err := a.Fleet.Heartbeat(ctx, mid); err != nil { // Agent 上线
		t.Fatal(err)
	}
	ac.do("POST", fmt.Sprintf("/machines/%d/maintenance", mid), map[string]bool{"on": false})

	agent := func() {
		t.Helper()
		if _, err := a.Fleet.Heartbeat(ctx, mid); err != nil {
			t.Fatal(err)
		}
		cmds, err := a.Fleet.PendingCommands(ctx, mid)
		if err != nil {
			t.Fatal(err)
		}
		for _, cmd := range cmds {
			if err := a.Rental.HandleCommandResult(ctx, cmd.ID, true, "ok"); err != nil {
				t.Fatal(err)
			}
		}
	}

	userJar, _ := cookiejar.New(nil)
	uc := &client{t: t, base: userTS.URL + "/api/v1", http: &http.Client{Jar: userJar}}
	uc.do("POST", "/auth/sms-code", map[string]string{"phone": "13800005678"})
	user := uc.do("POST", "/auth/login", map[string]string{"phone": "13800005678", "code": "000000"})
	uc.do("POST", "/kyc/personal", map[string]string{"name": "王五", "id_number": "110101199001011234"})
	rch := uc.do("POST", "/recharges", map[string]any{"amount": 100000, "channel": "alipay"})
	uc.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{})
	images := uc.do("GET", "/images", nil)["items"].([]any)
	order := uc.do("POST", "/orders", map[string]any{"product_id": pid, "duration": "1d", "image_id": images[0].(map[string]any)["id"]})
	id := order["instance"].(map[string]any)["id"].(string)
	agent()

	quote := ac.do("GET", "/instances/"+id+"/refund-quote", nil)
	refund := ac.do("POST", "/instances/"+id+"/refund-release", map[string]any{"amount": quote["suggested"], "reason": "机器故障", "totp": code()})
	agent()
	if inst := uc.do("GET", "/instances/"+id, nil); inst["state"] != "released" {
		t.Fatalf("instance after refund-release = %v", inst)
	}
	refunded := int64(refund["refunded"].(float64))
	if refunded <= 0 || refunded > 6900 {
		t.Fatalf("refunded = %d", refunded)
	}
	if bal := uc.do("GET", "/wallet", nil)["balance"].(float64); int64(bal) != 100000-6900+refunded {
		t.Fatalf("balance = %v", bal)
	}
	kinds := map[string]int{}
	for _, row := range uc.do("GET", "/wallet/ledger", nil)["items"].([]any) {
		kinds[row.(map[string]any)["kind"].(string)]++
	}
	if kinds["recharge"] != 1 || kinds["order_pay"] != 1 || kinds["refund"] != 1 {
		t.Fatalf("ledger kinds = %v", kinds)
	}
	uid := int64(user["id"].(float64))
	detail := ac.do("GET", fmt.Sprintf("/users/%d", uid), nil)
	if int64(detail["balance"].(float64)) != 100000-6900+refunded {
		t.Fatalf("admin user detail = %v", detail)
	}
	for _, action := range []string{"machine.register", "machine.maintenance", "instance.refund_release"} {
		if got := ac.do("GET", "/audit?action="+action, nil); got["total"].(float64) != 1 {
			t.Fatalf("audit %s = %v", action, got)
		}
	}
}
```

Run: `go test ./internal/app/ -run AdminRefundEndToEnd`
Expected: 编译失败（`a.Admins`、`a.AdminHandler` 不存在）。

- [ ] **Step 3: 组装后台服务**

`internal/app/app.go`：

1. import 增加 `"spark-cloud/internal/admin"`、`"spark-cloud/internal/adminapi"`。
2. `App` 结构体增加字段：

```go
	Admins       *admin.Service
	AdminHandler http.Handler // 后台 API，只应监听内网地址
```

3. 在构造 `srv := &api.Server{...}` 之后加入：

```go
	admins := admin.NewService(admin.Deps{Pool: pool, Clock: clk, Box: box})
	adminSrv := &adminapi.Server{
		Admins: admins, Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Billing: bill, Uploads: uploads,
		Clock: clk, Log: log, SecureCookie: cfg.Env == "prod", TrustProxy: cfg.TrustProxy,
	}
```

4. 返回的 `App` 字面量加 `Admins: admins, AdminHandler: adminSrv.Router(),`。

`cmd/platform/main.go` 的 `run` 中，把创建与运行 HTTP 服务的部分替换为（保留前面的配置、迁移、连接池、`app.Build` 与清扫器）：

```go
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           a.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	adminSrv := &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           a.AdminHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second, // CSV 导出最多 10 万行
		IdleTimeout:       120 * time.Second,
	}
	if listensOnAllInterfaces(cfg.AdminAddr) {
		log.Warn("后台 API 监听在所有网卡上，请确认防火墙只对内网开放该端口", "addr", cfg.AdminAddr)
	}
	serverErr := make(chan error, 2)
	go func() {
		log.Info("platform listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
		serverErr <- srv.ListenAndServe()
	}()
	go func() {
		log.Info("admin api listening", "addr", cfg.AdminAddr)
		serverErr <- adminSrv.ListenAndServe()
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	stop() // 确保清扫器退出
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range []*http.Server{srv, adminSrv} {
		if err := s.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	<-sweeperDone
	return runErr
}

// listensOnAllInterfaces 报告监听地址是否绑定了所有网卡（:8081、0.0.0.0:8081、[::]:8081）。
func listensOnAllInterfaces(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && (host == "" || host == "0.0.0.0" || host == "::")
}
```

（import 增加 `"net"`。）

在 `cmd/platform/admin_test.go` 末尾追加：

```go
func TestListensOnAllInterfaces(t *testing.T) {
	for addr, want := range map[string]bool{":8081": true, "0.0.0.0:8081": true, "[::]:8081": true, "127.0.0.1:8081": false, "10.0.0.5:8081": false} {
		if got := listensOnAllInterfaces(addr); got != want {
			t.Fatalf("%s = %v, want %v", addr, got, want)
		}
	}
}
```

Run: `go test ./internal/app/ ./internal/adminapi/ ./cmd/platform/`
Expected: PASS

- [ ] **Step 4: 更新文档**

`README.md`：

1. 「本地开发」一节末尾追加：

````markdown
开发环境支付为 mock：充值后调用 `POST /api/v1/dev/recharges/{id}/pay` 模拟支付成功（生产环境不注册该接口）。PDU 为 mock，断电重启只记录调用。

### 管理后台

后台 API 在独立端口（默认 `127.0.0.1:8081`，前缀 `/admin/api/v1`），系统没有默认账号，先用命令行创建第一个超级管理员：

```bash
SPARK_DATABASE_URL=... SPARK_SECRET_KEY=... go run ./cmd/platform admin create --username root --role super
```

按提示输入两次密码（至少 12 位），用验证器 App 扫描终端里的二维码并输入一次动态码完成绑定，然后离线保存打印出的 8 个恢复码。
管理员丢失手机时用 `platform admin reset-totp --username <名称>` 重置，下次登录重新绑定。
````

2. 「配置（环境变量）」表格中把 `SPARK_SMS_DRIVER` 一行替换为以下各行：

```markdown
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
```

3. 「部署注意」末尾追加：

```markdown
- 后台端口不要暴露到公网；如必须经过反向代理，请限制来源 IP。
- `SPARK_UPLOAD_DIR` 存放用户上传的营业执照，需要纳入备份。
- 支付回调地址需在微信支付商户平台、支付宝开放平台中配置为 `SPARK_PUBLIC_BASE_URL` 下的对应路径。
```

`docs/superpowers/followups-plan4.md`：

```markdown
# 计划 4 遗留事项

来源：计划 4 实施过程中的裁定与评审（2026-10-16 起）。均不阻塞合并。

## 上线前需要核对
- 阿里云号码百科三要素核验的产品名、版本号与 `IsConsistent` 取值以开通后的文档为准（只需改 `internal/plugins/kyc/aliyun.go`）。
- 微信支付按「微信支付公钥」模式接入；若商户号仍是平台证书模式，需要改用自动下载平台证书的初始化方式。
- PDU 默认按 APC PowerNet MIB；实际采购的品牌需在后台 PDU 配置里核对 OID 与取值，并在一台机器上实测断电重启。

## 可以继续延后
- 登录限流计数在进程内，多副本部署时每个副本各自计数。
- 换机后新实例的 SSH 密码与原实例不同，需要在前端提示用户（计划 5）。
- 后台没有修改已登记机器（机柜、IP、PDU 插座）的接口，目前只能通过导入新机器或直接改库。
- 支付金额不符的告警按「同类型同内容未处理」去重；同一笔单多次不同金额的通知仍会各产生一条。
- 后台列表的总数用 count(*)，数据量很大时可改为估算值。
```

`docs/superpowers/followups-plan1.md` 的「计划 4（管理后台 / 阿里云插件）必须处理」一节，每一条前面加上「✅ 计划 4 已处理：」。

- [ ] **Step 5: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/app internal/adminapi cmd/platform README.md docs/superpowers/followups-plan4.md docs/superpowers/followups-plan1.md
git commit -m "feat: serve admin API on its own port; permission matrix and admin end-to-end tests; docs

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

## 自检记录（写计划时完成）

**规格覆盖：** §1 新增包与 CLI → Task 2～4、5、8、15、21；§2 管理员、TOTP、权限、审计 → Task 2～4、15、21；§3 后台功能 → Task 16（机器 / PDU / 告警）、17（实例售后）、18（用户 / 认证 / 调账 / 入账）、19（订单流水导出）、20（价格镜像 / 告警 / 审计 / 仪表盘）；§4 充值与回调 → Task 8～10；§5 阿里云与实名并发 → Task 6、7；§6 PDU → Task 11；§7 上传 → Task 5、6、18；§8 配置 → Task 1、7、9、10、11、21；§9 计划 1 遗留 → Task 1（外键映射）、6（实名并发 / 营业执照 key）、12（重新洗机 / 结束维护检查）、14（启动失败后退款并释放）；§10 测试 → 各任务测试与 Task 21 权限矩阵、端到端。

**类型一致性：** 跨任务使用的名称以各任务 Interfaces 段为准；`audit.Actor`、`paging.Page`、`db.Where`、`fleet.PDUConn`、`rental.RefundQuote`、`billing.Recharge`、`adminapi.Route` 在定义任务与使用任务中签名一致。
