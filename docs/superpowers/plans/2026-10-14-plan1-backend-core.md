# Spark Cloud 计划 1：后端核心闭环 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付一个可运行的 Go 控制面（platform），实现手机号登录、实名、钱包、商品、机器库存、下单/续费/释放、实例状态机、到期清扫和用户 REST API。机器端指令写入 `agent_commands` 表，测试中由代码模拟 Agent 回报结果，跑通完整租赁生命周期。

**Architecture:** 模块化单体。`internal/` 下每个业务模块（identity、wallet、catalog、fleet、rental）是一个 Go 包，对外暴露 Service，只通过 Go 接口互相调用；涉及钱的操作由调用方开启事务并把 `pgx.Tx` 传给各模块，保证扣款、分配机器、写实例、写指令在同一事务内完成。时间一律从注入的 `clock.Clock` 取，测试用 `clock.Fake` 快进。

**Tech Stack:** Go 1.27（`~/.local/go/bin`），chi v5，pgx v5，goose v3，embedded-postgres（测试与本地开发），google/uuid，log/slog。

**Spec:** `docs/superpowers/specs/2026-10-14-spark-cloud-mvp-design.md`

**本计划不包含（后续计划）：** 计划 2 Agent 与 gRPC 网关；计划 3 sshpiper/Traefik 接入；计划 4 管理后台 API、阿里云短信/实名、微信/支付宝、PDU；计划 5 前端。

## Global Constraints

- Go 模块名 `spark-cloud`；Go 1.27；所有命令前置 `export PATH=$HOME/.local/go/bin:$PATH`。
- 金额一律 `int64`，单位「分」；数据库列 `BIGINT`。不允许出现浮点金额。
- 时间一律通过 `clock.Clock` 获取并以 UTC 写入数据库；SQL 中不使用 `now()`（迁移里的种子数据除外）。
- 面向用户的错误消息为中文，通过 `apperr.Error{Code, Message, Status}` 返回；未知错误统一返回 `internal` / 500，并记录日志。
- 租期只有 `1d`、`1w`、`1m`（不提供按小时），其中 `1m` = 30 天。初始价格 6900 / 41400 / 159900 分。
- 计时从实例进入 running 开始；到期停机后保留 6 小时；创建超时 10 分钟；Agent 失联判定 5 分钟。
- 主动释放不退款；创建失败全额退款；故障期间暂停计时。
- 生产环境（`SPARK_ENV=prod`）禁止使用 mock 插件。
- 每个模块不直接读写其他模块的表（测试夹具 `dbtest` 除外）。
- 机器占用用 `machine_leases` 时段表 + 排他约束表达；MVP 每个实例一条 active 时段，随实例状态同步上界（为子项目 2 的档期预订打基础）。
- 每个任务结束时 `go vet ./... && go test ./...` 必须全部通过再提交。

## Review Focus

1. **最后一台机器被并发抢购**：多个用户同时下单时只有一个能拿到机器，其余返回「暂无空闲机器」且余额不变 → Task 6 并发分配测试、Task 7 并发下单测试。
2. **Agent 重复或迟到的回报**：同一条指令回报两次、超时判失败后才回报成功，都不能导致重复退款或状态错乱 → Task 8 `TestDuplicateAckIsIgnored`、Task 9 `TestCreatingTimeoutRefundsAndIgnoresLateAck`。
3. **续费与 6 小时清扫撞车**：清扫已把实例转为 wiping 后用户再续费，必须拒绝且不扣钱 → Task 10 测试。
4. **租期中机器掉线**：掉线期间不计时，恢复后剩余时长补回，掉线期间不会被判到期 → Task 9 测试。
5. **验证码暴力尝试**：同一验证码最多试错 5 次，超过后正确验证码也失效 → Task 2 测试。

## 文件结构

```
spark-cloud/
├── go.mod / go.sum / Makefile / README.md / .gitignore
├── cmd/platform/main.go            # 控制面入口
├── cmd/devdb/main.go               # 本地开发用持久化嵌入式 PostgreSQL
├── migrations/
│   ├── embed.go                    # //go:embed *.sql
│   ├── 00001_audit.sql  00002_identity.sql  00003_kyc.sql
│   ├── 00004_wallet.sql 00005_catalog.sql   00006_fleet.sql  00007_rental.sql（含 machine_leases）
├── internal/
│   ├── platform/clock/             # Clock 接口、Real、Fake
│   ├── platform/apperr/            # 业务错误
│   ├── platform/db/                # Open、Migrate、WithTx、Querier
│   ├── platform/dbtest/            # 测试用临时 PostgreSQL + 夹具
│   ├── platform/secretbox/         # AES-GCM 加解密
│   ├── platform/randx/             # 验证码、token、ID、密码生成
│   ├── platform/config/            # 环境变量配置
│   ├── audit/                      # 审计日志
│   ├── plugins/sms/                # Sender 接口 + Mock
│   ├── plugins/kyc/                # Verifier 接口 + Mock
│   ├── identity/                   # 用户、短信登录、会话、实名
│   ├── wallet/                     # 余额与流水
│   ├── catalog/                    # 商品、价格、镜像
│   ├── fleet/                      # 机器、分配、心跳、指令队列、告警
│   ├── rental/                     # 订单、实例状态机、清扫器
│   ├── api/                        # 用户 REST API
│   └── app/                        # 组装所有服务（main 与端到端测试共用）
```

---

### Task 1: 项目骨架与基础设施

**Files:**
- Create: `go.mod`, `.gitignore`, `migrations/embed.go`, `migrations/00001_audit.sql`
- Create: `internal/platform/clock/clock.go`, `internal/platform/clock/clock_test.go`
- Create: `internal/platform/apperr/apperr.go`
- Create: `internal/platform/db/db.go`, `internal/platform/db/db_test.go`
- Create: `internal/platform/dbtest/dbtest.go`
- Create: `internal/platform/secretbox/secretbox.go`, `internal/platform/secretbox/secretbox_test.go`
- Create: `internal/platform/randx/randx.go`, `internal/platform/randx/randx_test.go`
- Create: `internal/audit/audit.go`

**Interfaces:**
- Produces:
  - `clock.Clock` 接口 `Now() time.Time`；`clock.Real{}`；`clock.NewFake(t time.Time) *clock.Fake`，`(*Fake).Advance(d time.Duration)`
  - `apperr.Error{Code, Message string; Status int}`，`apperr.New(code, msg string, status int) *Error`，`apperr.BadRequest(msg string) *Error`，`apperr.As(err error) *Error`，以及哨兵错误 `ErrNotFound ErrUnauthorized ErrForbidden ErrBanned ErrKYCRequired ErrInsufficientBalance ErrNoCapacity ErrInvalidState ErrDuplicate ErrRateLimited ErrInvalidCode`
  - `db.Querier`（`Exec/Query/QueryRow`，`*pgxpool.Pool` 与 `pgx.Tx` 都满足），`db.Open(ctx, url) (*pgxpool.Pool, error)`，`db.Migrate(ctx, url) error`，`db.WithTx(ctx, pool, func(pgx.Tx) error) error`，`db.IsUniqueViolation(err) bool`，`db.IsExclusionViolation(err) bool`，`db.IsNoRows(err) bool`
  - `dbtest.Main(m *testing.M)`，`dbtest.New(t) *pgxpool.Pool`，`dbtest.T0`（`2026-01-01T00:00:00Z`），`dbtest.Key`（32 字节测试密钥）
  - `secretbox.New(key []byte) (*Box, error)`，`(*Box).Seal([]byte) []byte`，`(*Box).Open([]byte) ([]byte, error)`
  - `randx.Digits(n int) string`，`randx.Token() string`，`randx.ID(prefix string, n int) string`，`randx.Password(n int) string`
  - `audit.Entry{ActorType string; ActorID int64; Action, TargetType, TargetID string; Diff any; IP string}`，`audit.Record(ctx, q db.Querier, at time.Time, e Entry) error`

- [ ] **Step 1: 初始化模块与依赖**

```bash
cd /Users/rifuyiri/Documents/claude/机房搭建/spark-cloud
export PATH=$HOME/.local/go/bin:$PATH
go mod init spark-cloud
go get github.com/jackc/pgx/v5@latest github.com/pressly/goose/v3@latest \
  github.com/fergusstrange/embedded-postgres@latest github.com/google/uuid@latest \
  github.com/go-chi/chi/v5@latest
```

`.gitignore`:

```gitignore
/bin/
/.devdata/
*.test
coverage.out
```

- [ ] **Step 2: 写 clock 的失败测试**

`internal/platform/clock/clock_test.go`:

```go
package clock_test

import (
	"testing"
	"time"

	"spark-cloud/internal/platform/clock"
)

func TestFakeAdvance(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := clock.NewFake(start)
	c.Advance(90 * time.Minute)
	if got, want := c.Now(), start.Add(90*time.Minute); !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
}

func TestRealIsUTCMicrosecond(t *testing.T) {
	now := clock.Real{}.Now()
	if now.Location() != time.UTC {
		t.Fatalf("location = %v, want UTC", now.Location())
	}
	if now.Nanosecond()%1000 != 0 {
		t.Fatalf("Real().Now() must be truncated to microseconds, got %d ns", now.Nanosecond())
	}
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/platform/clock/`
Expected: FAIL（`package spark-cloud/internal/platform/clock` 不存在）

- [ ] **Step 4: 实现 clock**

`internal/platform/clock/clock.go`:

```go
// Package clock 抽象当前时间。业务代码一律通过 Clock 取时间，测试时注入 Fake 快进。
package clock

import (
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
}

// Real 返回截断到微秒的 UTC 时间，与 PostgreSQL timestamptz 精度一致。
type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

type Fake struct {
	mu sync.Mutex
	t  time.Time
}

func NewFake(t time.Time) *Fake { return &Fake{t: t.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/platform/clock/`
Expected: PASS

- [ ] **Step 6: 实现 apperr**

`internal/platform/apperr/apperr.go`:

```go
// Package apperr 定义可以直接展示给用户的业务错误。
package apperr

import "errors"

type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func New(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

func BadRequest(message string) *Error { return New("bad_request", message, 400) }

var (
	ErrNotFound            = New("not_found", "资源不存在", 404)
	ErrUnauthorized        = New("unauthorized", "请先登录", 401)
	ErrForbidden           = New("forbidden", "无权执行该操作", 403)
	ErrBanned              = New("banned", "账号已被封禁", 403)
	ErrKYCRequired         = New("kyc_required", "请先完成实名认证", 403)
	ErrInsufficientBalance = New("insufficient_balance", "余额不足，请先充值", 402)
	ErrNoCapacity          = New("no_capacity", "暂无空闲机器，请稍后再试", 409)
	ErrInvalidState        = New("invalid_state", "当前状态不允许该操作", 409)
	ErrDuplicate           = New("duplicate", "重复操作", 409)
	ErrRateLimited         = New("rate_limited", "操作过于频繁，请稍后再试", 429)
	ErrInvalidCode         = New("invalid_code", "验证码错误或已过期", 400)
)

// As 取出错误链中的 *Error；不是业务错误时返回 nil。
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
```

- [ ] **Step 7: 写 secretbox 与 randx 的失败测试**

`internal/platform/secretbox/secretbox_test.go`:

```go
package secretbox_test

import (
	"bytes"
	"testing"

	"spark-cloud/internal/platform/secretbox"
)

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := secretbox.New(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed := box.Seal([]byte("110101199001011234"))
	if bytes.Contains(sealed, []byte("1101011990")) {
		t.Fatal("ciphertext contains plaintext")
	}
	plain, err := box.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "110101199001011234" {
		t.Fatalf("got %q", plain)
	}
}

func TestOpenRejectsTamperedData(t *testing.T) {
	box, _ := secretbox.New(bytes.Repeat([]byte("k"), 32))
	sealed := box.Seal([]byte("secret"))
	sealed[len(sealed)-1] ^= 0xff
	if _, err := box.Open(sealed); err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}

func TestNewRejectsWrongKeyLength(t *testing.T) {
	if _, err := secretbox.New([]byte("short")); err == nil {
		t.Fatal("expected error for short key")
	}
}
```

`internal/platform/randx/randx_test.go`:

```go
package randx_test

import (
	"regexp"
	"testing"

	"spark-cloud/internal/platform/randx"
)

func TestDigits(t *testing.T) {
	if got := randx.Digits(6); !regexp.MustCompile(`^\d{6}$`).MatchString(got) {
		t.Fatalf("Digits(6) = %q", got)
	}
}

func TestID(t *testing.T) {
	got := randx.ID("ins-", 8)
	if !regexp.MustCompile(`^ins-[a-z0-9]{8}$`).MatchString(got) {
		t.Fatalf("ID = %q", got)
	}
	if randx.ID("ins-", 8) == got {
		t.Fatal("two IDs should differ")
	}
}

func TestPasswordAndToken(t *testing.T) {
	if got := randx.Password(16); !regexp.MustCompile(`^[A-HJ-NP-Za-km-z2-9]{16}$`).MatchString(got) {
		t.Fatalf("Password = %q", got)
	}
	if got := randx.Token(); len(got) != 43 {
		t.Fatalf("Token length = %d, want 43", len(got))
	}
}
```

- [ ] **Step 8: 运行测试确认失败**

Run: `go test ./internal/platform/secretbox/ ./internal/platform/randx/`
Expected: FAIL（包不存在）

- [ ] **Step 9: 实现 secretbox 与 randx**

`internal/platform/secretbox/secretbox.go`:

```go
// Package secretbox 用 AES-256-GCM 加密敏感字段（身份证号、实例密码）。
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
)

type Box struct {
	aead cipher.AEAD
}

func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("secretbox: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal 返回 nonce || ciphertext。
func (b *Box) Seal(plain []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return b.aead.Seal(nonce, nonce, plain, nil)
}

func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("secretbox: ciphertext too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], nil)
}
```

`internal/platform/randx/randx.go`:

```go
// Package randx 生成验证码、会话 token、资源 ID 和密码，全部基于 crypto/rand。
package randx

import (
	"crypto/rand"
	"encoding/base64"
	"math/big"
)

const (
	digits       = "0123456789"
	idAlphabet   = "abcdefghijklmnopqrstuvwxyz0123456789"
	pwdAlphabet  = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789" // 去掉易混淆字符
)

func pick(alphabet string, n int) string {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		out[i] = alphabet[v.Int64()]
	}
	return string(out)
}

func Digits(n int) string { return pick(digits, n) }

func ID(prefix string, n int) string { return prefix + pick(idAlphabet, n) }

func Password(n int) string { return pick(pwdAlphabet, n) }

// Token 返回 32 字节随机数的 base64url（无填充，43 个字符）。
func Token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
```

- [ ] **Step 10: 运行测试确认通过**

Run: `go test ./internal/platform/secretbox/ ./internal/platform/randx/`
Expected: PASS

- [ ] **Step 11: 写迁移、db、dbtest、audit**

`migrations/embed.go`:

```go
// Package migrations 内嵌所有 goose SQL 迁移文件。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
```

`migrations/00001_audit.sql`:

```sql
-- +goose Up
CREATE TABLE audit_logs (
    id          BIGSERIAL PRIMARY KEY,
    actor_type  TEXT NOT NULL CHECK (actor_type IN ('admin', 'user', 'system')),
    actor_id    BIGINT NOT NULL DEFAULT 0,
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL DEFAULT '',
    target_id   TEXT NOT NULL DEFAULT '',
    diff        JSONB NOT NULL DEFAULT '{}',
    ip          TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX audit_logs_target_idx ON audit_logs (target_type, target_id);

-- +goose Down
DROP TABLE audit_logs;
```

`internal/platform/db/db.go`:

```go
// Package db 提供连接池、迁移和事务辅助。
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // 注册 database/sql 驱动 "pgx"，供 goose 使用
	"github.com/pressly/goose/v3"

	"spark-cloud/migrations"
)

// Querier 由 *pgxpool.Pool 与 pgx.Tx 共同满足，只读查询可以传任意一个。
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return pool, nil
}

func Migrate(ctx context.Context, url string) error {
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		return fmt.Errorf("open migrate conn: %w", err)
	}
	defer sqlDB.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// WithTx 在事务中执行 fn；fn 返回错误时回滚并原样返回该错误。
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // 提交后回滚是空操作
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// IsExclusionViolation 判断是否违反排他约束（例如同一台机器的占用时段重叠）。
func IsExclusionViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}
```

`internal/platform/dbtest/dbtest.go`:

```go
// Package dbtest 为测试提供临时 PostgreSQL：每个测试进程启动一个嵌入式实例，每个测试一个全新数据库。
package dbtest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/db"
)

// T0 是所有测试的起始时间。
var T0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Key 是测试用 32 字节加密密钥。
var Key = bytes.Repeat([]byte("k"), 32)

var (
	adminURL string
	seq      atomic.Int64
)

// Main 用在测试包的 TestMain 中：func TestMain(m *testing.M) { dbtest.Main(m) }
func Main(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "sparkpg-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	port := freePort()
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(port)).
		RuntimePath(filepath.Join(dir, "runtime")).
		DataPath(filepath.Join(dir, "data")).
		Logger(io.Discard))
	if err := pg.Start(); err != nil {
		panic(fmt.Sprintf("start embedded postgres: %v", err))
	}
	defer pg.Stop() //nolint:errcheck
	adminURL = fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
	return m.Run()
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// New 创建一个全新数据库、执行全部迁移并返回连接池；测试结束自动关闭。
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("t_%d_%d", os.Getpid(), seq.Add(1))
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	url := strings.Replace(adminURL, "/postgres?", "/"+name+"?", 1)
	if err := db.Migrate(ctx, url); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
```

`internal/audit/audit.go`:

```go
// Package audit 记录管理员与系统的关键操作。
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"spark-cloud/internal/platform/db"
)

type Entry struct {
	ActorType  string // admin / user / system
	ActorID    int64
	Action     string
	TargetType string
	TargetID   string
	Diff       any
	IP         string
}

func Record(ctx context.Context, q db.Querier, at time.Time, e Entry) error {
	diff := []byte("{}")
	if e.Diff != nil {
		b, err := json.Marshal(e.Diff)
		if err != nil {
			return fmt.Errorf("audit diff: %w", err)
		}
		diff = b
	}
	_, err := q.Exec(ctx, `INSERT INTO audit_logs (actor_type, actor_id, action, target_type, target_id, diff, ip, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.ActorType, e.ActorID, e.Action, e.TargetType, e.TargetID, diff, e.IP, at)
	return err
}
```

- [ ] **Step 12: 写 db 的测试（覆盖迁移、事务提交与回滚、审计写入）**

`internal/platform/db/db_test.go`:

```go
package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func TestWithTxCommitAndRollback(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	boom := errors.New("boom")

	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		if err := audit.Record(ctx, tx, dbtest.T0, audit.Entry{ActorType: "system", Action: "test.rollback"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}

	err = db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		return audit.Record(ctx, tx, dbtest.T0, audit.Entry{ActorType: "system", Action: "test.commit", Diff: map[string]int{"a": 1}})
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, _ := pool.Query(ctx, "SELECT action FROM audit_logs ORDER BY id")
	actions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0] != "test.commit" {
		t.Fatalf("actions = %v, want [test.commit]", actions)
	}
}
```

- [ ] **Step 13: 运行全部测试**

Run: `go mod tidy && go vet ./... && go test ./...`
Expected: PASS（首次运行 embedded-postgres 会下载 PostgreSQL 二进制，可能需要 1～2 分钟）

- [ ] **Step 14: 提交**

```bash
git add -A
git commit -m "feat: project skeleton with clock, db, migrations, secretbox, randx, audit

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: 用户与短信登录（identity）

**Files:**
- Create: `migrations/00002_identity.sql`
- Create: `internal/plugins/sms/sms.go`
- Create: `internal/identity/service.go`, `internal/identity/auth.go`, `internal/identity/auth_test.go`
- Create: `internal/platform/dbtest/fixtures.go`

**Interfaces:**
- Consumes: Task 1 全部
- Produces:
  - `sms.Sender` 接口 `Send(ctx, phone, template string, params map[string]string) error`；常量 `sms.TemplateLoginCode = "login_code"`、`sms.TemplateExpiryReminder = "expiry_reminder"`；`sms.Mock`（`Sent() []sms.Message`），`sms.Message{Phone, Template string; Params map[string]string}`
  - `identity.User{ID int64; Phone, Kind, KYCStatus, Status string; CreatedAt time.Time}`（JSON：`id phone kind kyc_status status created_at`）
  - `identity.Deps{Pool *pgxpool.Pool; Clock clock.Clock; SMS sms.Sender; KYC kyc.Verifier; Box *secretbox.Box; FixedCode string}`（`KYC`、`Box` 字段在 Task 3 加入）
  - `identity.NewService(d Deps) *Service`
  - `(*Service).SendLoginCode(ctx, phone, ip string) error`
  - `(*Service).Login(ctx, phone, code string) (token string, u User, err error)`
  - `(*Service).Authenticate(ctx, token string) (User, error)`
  - `(*Service).Logout(ctx, token string) error`
  - `(*Service).GetUser(ctx, id int64) (User, error)`
  - `(*Service).CanRent(ctx, userID int64) error`
  - `dbtest.CreateUser(t, pool, phone string, verified bool) int64`

- [ ] **Step 1: 写迁移**

`migrations/00002_identity.sql`:

```sql
-- +goose Up
CREATE TABLE users (
    id         BIGSERIAL PRIMARY KEY,
    phone      TEXT NOT NULL UNIQUE,
    kind       TEXT NOT NULL DEFAULT 'personal' CHECK (kind IN ('personal', 'enterprise')),
    kyc_status TEXT NOT NULL DEFAULT 'none' CHECK (kyc_status IN ('none', 'pending', 'verified', 'rejected')),
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'banned')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE sms_codes (
    id         BIGSERIAL PRIMARY KEY,
    phone      TEXT NOT NULL,
    code_hash  TEXT NOT NULL,
    ip         TEXT NOT NULL,
    attempts   INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);
CREATE INDEX sms_codes_phone_idx ON sms_codes (phone, created_at DESC);
CREATE INDEX sms_codes_ip_idx ON sms_codes (ip, created_at DESC);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

-- +goose Down
DROP TABLE sessions;
DROP TABLE sms_codes;
DROP TABLE users;
```

- [ ] **Step 2: 写 sms 插件与测试夹具**

`internal/plugins/sms/sms.go`:

```go
// Package sms 定义短信发送插件接口；生产实现（阿里云）在计划 4 中加入。
package sms

import (
	"context"
	"sync"
)

const (
	TemplateLoginCode      = "login_code"      // 参数：code
	TemplateExpiryReminder = "expiry_reminder" // 参数：instance、hours
)

type Sender interface {
	Send(ctx context.Context, phone, template string, params map[string]string) error
}

type Message struct {
	Phone    string
	Template string
	Params   map[string]string
}

// Mock 把短信记录在内存里，供开发与测试使用。
type Mock struct {
	mu   sync.Mutex
	sent []Message
}

func (m *Mock) Send(_ context.Context, phone, template string, params map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, Message{Phone: phone, Template: template, Params: params})
	return nil
}

func (m *Mock) Sent() []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Message(nil), m.sent...)
}
```

`internal/platform/dbtest/fixtures.go`:

```go
package dbtest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CreateUser 直接插入一个用户；verified 为 true 时实名状态为 verified。
func CreateUser(t *testing.T, pool *pgxpool.Pool, phone string, verified bool) int64 {
	t.Helper()
	status := "none"
	if verified {
		status = "verified"
	}
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (phone, kyc_status, created_at, updated_at) VALUES ($1, $2, $3, $3) RETURNING id`,
		phone, status, T0).Scan(&id)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id
}
```

- [ ] **Step 3: 写 identity 的失败测试**

`internal/identity/auth_test.go`:

```go
package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/sms"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type env struct {
	svc   *identity.Service
	sms   *sms.Mock
	clock *clock.Fake
}

func newEnv(t *testing.T, fixedCode string) env {
	t.Helper()
	pool := dbtest.New(t)
	mock := &sms.Mock{}
	clk := clock.NewFake(dbtest.T0)
	svc := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: mock, FixedCode: fixedCode})
	return env{svc: svc, sms: mock, clock: clk}
}

func lastCode(t *testing.T, m *sms.Mock) string {
	t.Helper()
	sent := m.Sent()
	if len(sent) == 0 {
		t.Fatal("no sms sent")
	}
	return sent[len(sent)-1].Params["code"]
}

func TestLoginFlowCreatesUserAndSession(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000001", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	token, u, err := e.svc.Login(ctx, "13800000001", lastCode(t, e.sms))
	if err != nil {
		t.Fatal(err)
	}
	if u.Phone != "13800000001" || u.KYCStatus != "none" || u.Status != "active" {
		t.Fatalf("unexpected user %+v", u)
	}
	got, err := e.svc.Authenticate(ctx, token)
	if err != nil || got.ID != u.ID {
		t.Fatalf("Authenticate = %+v, %v", got, err)
	}

	// 同一手机号再次登录是同一个用户
	e.clock.Advance(time.Minute)
	if err := e.svc.SendLoginCode(ctx, "13800000001", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	_, u2, err := e.svc.Login(ctx, "13800000001", lastCode(t, e.sms))
	if err != nil || u2.ID != u.ID {
		t.Fatalf("second login = %+v, %v", u2, err)
	}

	if err := e.svc.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, token); !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("after logout want unauthorized, got %v", err)
	}
}

func TestSendCodeRejectsInvalidPhone(t *testing.T) {
	e := newEnv(t, "")
	err := e.svc.SendLoginCode(context.Background(), "12345", "1.1.1.1")
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
}

func TestSendCodeRateLimitedWithin60Seconds(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000002", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SendLoginCode(ctx, "13800000002", "1.1.1.1"); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("want rate limited, got %v", err)
	}
	e.clock.Advance(61 * time.Second)
	if err := e.svc.SendLoginCode(ctx, "13800000002", "1.1.1.1"); err != nil {
		t.Fatalf("after 61s want ok, got %v", err)
	}
}

func TestSendCodeDailyLimit(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := e.svc.SendLoginCode(ctx, "13800000003", "1.1.1.1"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		e.clock.Advance(61 * time.Second)
	}
	if err := e.svc.SendLoginCode(ctx, "13800000003", "1.1.1.1"); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("11th send want rate limited, got %v", err)
	}
}

func TestWrongCodeLocksAfterFiveAttempts(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000004", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code := lastCode(t, e.sms)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < 5; i++ {
		if _, _, err := e.svc.Login(ctx, "13800000004", wrong); !errors.Is(err, apperr.ErrInvalidCode) {
			t.Fatalf("attempt %d want invalid code, got %v", i, err)
		}
	}
	if _, _, err := e.svc.Login(ctx, "13800000004", code); !errors.Is(err, apperr.ErrInvalidCode) {
		t.Fatalf("correct code after 5 failures must be rejected, got %v", err)
	}
}

func TestCodeExpiresAfterFiveMinutes(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000005", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(5*time.Minute + time.Second)
	if _, _, err := e.svc.Login(ctx, "13800000005", lastCode(t, e.sms)); !errors.Is(err, apperr.ErrInvalidCode) {
		t.Fatalf("want invalid code, got %v", err)
	}
}

func TestCodeCannotBeReused(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000006", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code := lastCode(t, e.sms)
	if _, _, err := e.svc.Login(ctx, "13800000006", code); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Login(ctx, "13800000006", code); !errors.Is(err, apperr.ErrInvalidCode) {
		t.Fatalf("reuse want invalid code, got %v", err)
	}
}

func TestSessionExpiresAfter30Days(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	_ = e.svc.SendLoginCode(ctx, "13800000007", "1.1.1.1")
	token, _, err := e.svc.Login(ctx, "13800000007", lastCode(t, e.sms))
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(30*24*time.Hour + time.Second)
	if _, err := e.svc.Authenticate(ctx, token); !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("want unauthorized, got %v", err)
	}
}

func TestFixedCodeForDevelopment(t *testing.T) {
	e := newEnv(t, "000000")
	ctx := context.Background()
	_ = e.svc.SendLoginCode(ctx, "13800000008", "1.1.1.1")
	if got := lastCode(t, e.sms); got != "000000" {
		t.Fatalf("code = %q, want 000000", got)
	}
	if _, _, err := e.svc.Login(ctx, "13800000008", "000000"); err != nil {
		t.Fatal(err)
	}
}

func TestCanRentRequiresVerifiedKYC(t *testing.T) {
	e := newEnv(t, "000000")
	ctx := context.Background()
	_ = e.svc.SendLoginCode(ctx, "13800000009", "1.1.1.1")
	_, u, err := e.svc.Login(ctx, "13800000009", "000000")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CanRent(ctx, u.ID); !errors.Is(err, apperr.ErrKYCRequired) {
		t.Fatalf("want kyc required, got %v", err)
	}
}
```

- [ ] **Step 4: 运行测试确认失败**

Run: `go test ./internal/identity/`
Expected: FAIL（`identity` 包不存在）

- [ ] **Step 5: 实现 identity**

`internal/identity/service.go`:

```go
// Package identity 管理用户、短信登录、会话与实名认证。
package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/plugins/sms"
)

type User struct {
	ID        int64     `json:"id"`
	Phone     string    `json:"phone"`
	Kind      string    `json:"kind"`
	KYCStatus string    `json:"kyc_status"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type Deps struct {
	Pool      *pgxpool.Pool
	Clock     clock.Clock
	SMS       sms.Sender
	FixedCode string // 仅开发环境：非空时验证码固定为该值
}

type Service struct {
	d Deps
}

func NewService(d Deps) *Service { return &Service{d: d} }

const userCols = `id, phone, kind, kyc_status, status, created_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Phone, &u.Kind, &u.KYCStatus, &u.Status, &u.CreatedAt)
	if db.IsNoRows(err) {
		return User{}, apperr.ErrNotFound
	}
	return u, err
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (s *Service) GetUser(ctx context.Context, id int64) (User, error) {
	return scanUser(s.d.Pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

// CanRent 供 rental 模块在下单前校验：账号正常且已实名。
func (s *Service) CanRent(ctx context.Context, userID int64) error {
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.Status == "banned" {
		return apperr.ErrBanned
	}
	if u.KYCStatus != "verified" {
		return apperr.ErrKYCRequired
	}
	return nil
}
```

`internal/identity/auth.go`:

```go
package identity

import (
	"context"
	"crypto/subtle"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/plugins/sms"
)

const (
	codeTTL         = 5 * time.Minute
	codeInterval    = 60 * time.Second
	codeDailyLimit  = 10
	codeIPHourLimit = 30
	codeMaxAttempts = 5
	sessionTTL      = 30 * 24 * time.Hour
)

var phoneRe = regexp.MustCompile(`^1[3-9]\d{9}$`)

func (s *Service) SendLoginCode(ctx context.Context, phone, ip string) error {
	if !phoneRe.MatchString(phone) {
		return apperr.BadRequest("手机号格式不正确")
	}
	now := s.d.Clock.Now()
	var recent, daily, ipHour int
	err := s.d.Pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE phone = $1 AND created_at > $2),
			count(*) FILTER (WHERE phone = $1),
			count(*) FILTER (WHERE ip = $4 AND created_at > $5)
		FROM sms_codes WHERE created_at > $3`,
		phone, now.Add(-codeInterval), now.Add(-24*time.Hour), ip, now.Add(-time.Hour)).
		Scan(&recent, &daily, &ipHour)
	if err != nil {
		return err
	}
	if recent > 0 || daily >= codeDailyLimit || ipHour >= codeIPHourLimit {
		return apperr.ErrRateLimited
	}
	code := s.d.FixedCode
	if code == "" {
		code = randx.Digits(6)
	}
	if _, err := s.d.Pool.Exec(ctx, `INSERT INTO sms_codes (phone, code_hash, ip, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, phone, hashSecret(phone+":"+code), ip, now, now.Add(codeTTL)); err != nil {
		return err
	}
	return s.d.SMS.Send(ctx, phone, sms.TemplateLoginCode, map[string]string{"code": code})
}

// Login 校验验证码，不存在则创建用户，返回新的会话 token。
func (s *Service) Login(ctx context.Context, phone, code string) (string, User, error) {
	now := s.d.Clock.Now()
	var (
		bizErr error // 需要提交事务（记录试错次数）后再返回的业务错误
		user   User
		token  string
	)
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var id int64
		var hash string
		var attempts int
		err := tx.QueryRow(ctx, `SELECT id, code_hash, attempts FROM sms_codes
			WHERE phone = $1 AND used_at IS NULL AND expires_at > $2
			ORDER BY id DESC LIMIT 1 FOR UPDATE`, phone, now).Scan(&id, &hash, &attempts)
		if db.IsNoRows(err) {
			bizErr = apperr.ErrInvalidCode
			return nil
		}
		if err != nil {
			return err
		}
		if attempts >= codeMaxAttempts {
			bizErr = apperr.ErrInvalidCode
			return nil
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(hashSecret(phone+":"+code))) != 1 {
			bizErr = apperr.ErrInvalidCode
			_, err := tx.Exec(ctx, `UPDATE sms_codes SET attempts = attempts + 1 WHERE id = $1`, id)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sms_codes SET used_at = $2 WHERE id = $1`, id, now); err != nil {
			return err
		}
		user, err = scanUser(tx.QueryRow(ctx, `INSERT INTO users (phone, created_at, updated_at) VALUES ($1, $2, $2)
			ON CONFLICT (phone) DO UPDATE SET updated_at = EXCLUDED.updated_at
			RETURNING `+userCols, phone, now))
		if err != nil {
			return err
		}
		if user.Status == "banned" {
			bizErr = apperr.ErrBanned
			return nil
		}
		token = randx.Token()
		_, err = tx.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
			hashSecret(token), user.ID, now, now.Add(sessionTTL))
		return err
	})
	if err != nil {
		return "", User{}, err
	}
	if bizErr != nil {
		return "", User{}, bizErr
	}
	return token, user, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, apperr.ErrUnauthorized
	}
	u, err := scanUser(s.d.Pool.QueryRow(ctx, `SELECT u.id, u.phone, u.kind, u.kyc_status, u.status, u.created_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > $2`, hashSecret(token), s.d.Clock.Now()))
	if errors.Is(err, apperr.ErrNotFound) {
		return User{}, apperr.ErrUnauthorized
	}
	if err != nil {
		return User{}, err
	}
	if u.Status == "banned" {
		return User{}, apperr.ErrBanned
	}
	return u, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.d.Pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashSecret(token))
	return err
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/identity/`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add -A
git commit -m "feat(identity): phone sms login with rate limits and sessions

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: 实名认证（个人三要素 + 企业人工审核）

**Files:**
- Create: `migrations/00003_kyc.sql`
- Create: `internal/plugins/kyc/kyc.go`
- Modify: `internal/identity/service.go`（`Deps` 增加 `KYC`、`Box` 字段）
- Create: `internal/identity/kyc.go`, `internal/identity/kyc_test.go`

**Interfaces:**
- Consumes: Task 2 的 `identity.Service`、`audit.Record`、`secretbox.Box`
- Produces:
  - `kyc.Verifier` 接口 `VerifyPerson(ctx, name, idNumber, phone string) (kyc.Result, error)`；`kyc.Result{Passed bool; Reason string}`；`kyc.Mock{Reject bool}`
  - `(*identity.Service).SubmitPersonalKYC(ctx, userID int64, name, idNumber string) (User, error)`
  - `(*identity.Service).SubmitEnterpriseKYC(ctx, userID int64, companyName, licenseNo, licenseFile string) error`
  - `(*identity.Service).ReviewEnterpriseKYC(ctx, recordID, reviewerID int64, approve bool, reason string) error`

- [ ] **Step 1: 写迁移与插件**

`migrations/00003_kyc.sql`:

```sql
-- +goose Up
CREATE TABLE kyc_records (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users (id),
    kind          TEXT NOT NULL CHECK (kind IN ('personal', 'enterprise')),
    real_name     TEXT NOT NULL,             -- 个人姓名或企业名称
    id_number_enc BYTEA,                     -- 个人身份证号（AES-GCM 加密）
    license_no    TEXT NOT NULL DEFAULT '',  -- 统一社会信用代码
    license_file  TEXT NOT NULL DEFAULT '',  -- 营业执照文件地址
    result        TEXT NOT NULL CHECK (result IN ('pending', 'verified', 'rejected')),
    reason        TEXT NOT NULL DEFAULT '',
    reviewer_id   BIGINT,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX kyc_records_user_idx ON kyc_records (user_id, created_at DESC);
CREATE INDEX kyc_records_pending_idx ON kyc_records (created_at) WHERE result = 'pending';

-- +goose Down
DROP TABLE kyc_records;
```

`internal/plugins/kyc/kyc.go`:

```go
// Package kyc 定义实名核验插件接口；生产实现（阿里云三要素）在计划 4 中加入。
package kyc

import "context"

type Result struct {
	Passed bool
	Reason string
}

type Verifier interface {
	VerifyPerson(ctx context.Context, name, idNumber, phone string) (Result, error)
}

// Mock 默认全部通过；Reject 为 true 时全部不通过。
type Mock struct {
	Reject bool
}

func (m Mock) VerifyPerson(context.Context, string, string, string) (Result, error) {
	if m.Reject {
		return Result{Passed: false, Reason: "姓名、身份证号与手机号不一致"}, nil
	}
	return Result{Passed: true}, nil
}
```

- [ ] **Step 2: 修改 `identity.Deps`**

`internal/identity/service.go` 中的 `Deps` 改为：

```go
type Deps struct {
	Pool      *pgxpool.Pool
	Clock     clock.Clock
	SMS       sms.Sender
	KYC       kyc.Verifier
	Box       *secretbox.Box
	FixedCode string // 仅开发环境：非空时验证码固定为该值
}
```

并在 import 中加入 `"spark-cloud/internal/plugins/kyc"` 与 `"spark-cloud/internal/platform/secretbox"`。

- [ ] **Step 3: 写 KYC 的失败测试**

`internal/identity/kyc_test.go`:

```go
package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
)

func newKYCEnv(t *testing.T, verifier kyc.Verifier) (*identity.Service, *pgxpool.Pool, *secretbox.Box) {
	t.Helper()
	pool := dbtest.New(t)
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	svc := identity.NewService(identity.Deps{
		Pool: pool, Clock: clock.NewFake(dbtest.T0), SMS: &sms.Mock{}, KYC: verifier, Box: box,
	})
	return svc, pool, box
}

const validID = "11010119900101123X"

func TestPersonalKYCPassesAndEncryptsIDNumber(t *testing.T) {
	svc, pool, box := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000001", false)

	u, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID)
	if err != nil {
		t.Fatal(err)
	}
	if u.KYCStatus != "verified" || u.Kind != "personal" {
		t.Fatalf("user = %+v", u)
	}
	if err := svc.CanRent(ctx, uid); err != nil {
		t.Fatalf("CanRent after verify: %v", err)
	}

	var enc []byte
	if err := pool.QueryRow(ctx, `SELECT id_number_enc FROM kyc_records WHERE user_id = $1`, uid).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	plain, err := box.Open(enc)
	if err != nil || string(plain) != validID {
		t.Fatalf("decrypted id = %q, %v", plain, err)
	}
}

func TestPersonalKYCRejected(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{Reject: true})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000002", false)

	_, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID)
	if ae := apperr.As(err); ae == nil || ae.Code != "kyc_rejected" {
		t.Fatalf("want kyc_rejected, got %v", err)
	}
	u, _ := svc.GetUser(ctx, uid)
	if u.KYCStatus != "rejected" {
		t.Fatalf("status = %s, want rejected", u.KYCStatus)
	}
}

func TestPersonalKYCValidation(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000003", false)
	cases := []struct{ name, id string }{
		{"", validID},
		{"张三", "123"},
		{"张三", "1101011990010112345"},
	}
	for _, c := range cases {
		_, err := svc.SubmitPersonalKYC(ctx, uid, c.name, c.id)
		if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
			t.Fatalf("(%q,%q) want bad_request, got %v", c.name, c.id, err)
		}
	}
}

func TestPersonalKYCAlreadyVerified(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	uid := dbtest.CreateUser(t, pool, "13900000004", true)
	if _, err := svc.SubmitPersonalKYC(context.Background(), uid, "张三", validID); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("want invalid state, got %v", err)
	}
}

func TestPersonalKYCDailyLimit(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{Reject: true})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000005", false)
	for i := 0; i < 5; i++ {
		if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); apperr.As(err) == nil || apperr.As(err).Code != "kyc_rejected" {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("6th attempt want rate limited, got %v", err)
	}
}

func TestEnterpriseKYCReview(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000006", false)

	if err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", "https://files/license.png"); err != nil {
		t.Fatal(err)
	}
	u, _ := svc.GetUser(ctx, uid)
	if u.KYCStatus != "pending" {
		t.Fatalf("status = %s, want pending", u.KYCStatus)
	}
	var recordID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM kyc_records WHERE user_id = $1`, uid).Scan(&recordID); err != nil {
		t.Fatal(err)
	}

	if err := svc.ReviewEnterpriseKYC(ctx, recordID, 99, false, ""); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("reject without reason want bad_request, got %v", err)
	}
	if err := svc.ReviewEnterpriseKYC(ctx, recordID, 99, true, ""); err != nil {
		t.Fatal(err)
	}
	u, _ = svc.GetUser(ctx, uid)
	if u.KYCStatus != "verified" || u.Kind != "enterprise" {
		t.Fatalf("user = %+v", u)
	}
	if err := svc.ReviewEnterpriseKYC(ctx, recordID, 99, true, ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("second review want invalid state, got %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'kyc.review'`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
}

func TestEnterpriseKYCValidation(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	uid := dbtest.CreateUser(t, pool, "13900000007", false)
	err := svc.SubmitEnterpriseKYC(context.Background(), uid, "某某科技有限公司", "bad", "https://files/license.png")
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
}
```

- [ ] **Step 4: 运行测试确认失败**

Run: `go test ./internal/identity/ -run KYC`
Expected: FAIL（`SubmitPersonalKYC` 未定义）

- [ ] **Step 5: 实现 KYC**

`internal/identity/kyc.go`:

```go
package identity

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

const kycDailyLimit = 5

var (
	idNumberRe  = regexp.MustCompile(`^\d{17}[\dX]$`)
	licenseNoRe = regexp.MustCompile(`^[0-9A-HJ-NPQRTUWXY]{2}\d{6}[0-9A-HJ-NPQRTUWXY]{10}$`)
)

// SubmitPersonalKYC 调用三要素核验，结果立即生效。
func (s *Service) SubmitPersonalKYC(ctx context.Context, userID int64, name, idNumber string) (User, error) {
	name = strings.TrimSpace(name)
	idNumber = strings.ToUpper(strings.TrimSpace(idNumber))
	if name == "" || utf8.RuneCountInString(name) > 50 {
		return User{}, apperr.BadRequest("请填写真实姓名")
	}
	if !idNumberRe.MatchString(idNumber) {
		return User{}, apperr.BadRequest("身份证号格式不正确")
	}
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if u.KYCStatus == "verified" || u.KYCStatus == "pending" {
		return User{}, apperr.ErrInvalidState
	}
	now := s.d.Clock.Now()
	var attempts int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM kyc_records
		WHERE user_id = $1 AND kind = 'personal' AND created_at > $2`, userID, now.Add(-24*time.Hour)).Scan(&attempts); err != nil {
		return User{}, err
	}
	if attempts >= kycDailyLimit {
		return User{}, apperr.ErrRateLimited
	}

	res, err := s.d.KYC.VerifyPerson(ctx, name, idNumber, u.Phone)
	if err != nil {
		return User{}, fmt.Errorf("kyc verify: %w", err)
	}
	result := "rejected"
	if res.Passed {
		result = "verified"
	}
	err = db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, id_number_enc, result, reason, created_at, updated_at)
			VALUES ($1, 'personal', $2, $3, $4, $5, $6, $6)`,
			userID, name, s.d.Box.Seal([]byte(idNumber)), result, res.Reason, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE users SET kyc_status = $2, kind = 'personal', updated_at = $3
			WHERE id = $1 AND kyc_status <> 'verified'`, userID, result, now)
		return err
	})
	if err != nil {
		return User{}, err
	}
	if !res.Passed {
		return User{}, apperr.New("kyc_rejected", "实名核验未通过："+res.Reason, 400)
	}
	return s.GetUser(ctx, userID)
}

// SubmitEnterpriseKYC 提交企业认证，等待管理员审核。
func (s *Service) SubmitEnterpriseKYC(ctx context.Context, userID int64, companyName, licenseNo, licenseFile string) error {
	companyName = strings.TrimSpace(companyName)
	licenseNo = strings.ToUpper(strings.TrimSpace(licenseNo))
	if companyName == "" || utf8.RuneCountInString(companyName) > 100 {
		return apperr.BadRequest("请填写企业名称")
	}
	if !licenseNoRe.MatchString(licenseNo) {
		return apperr.BadRequest("统一社会信用代码格式不正确")
	}
	if strings.TrimSpace(licenseFile) == "" {
		return apperr.BadRequest("请上传营业执照")
	}
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.KYCStatus == "verified" || u.KYCStatus == "pending" {
		return apperr.ErrInvalidState
	}
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, license_no, license_file, result, created_at, updated_at)
			VALUES ($1, 'enterprise', $2, $3, $4, 'pending', $5, $5)`,
			userID, companyName, licenseNo, licenseFile, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE users SET kyc_status = 'pending', updated_at = $2 WHERE id = $1`, userID, now)
		return err
	})
}

// ReviewEnterpriseKYC 管理员审核企业认证；驳回必须填写原因。
func (s *Service) ReviewEnterpriseKYC(ctx context.Context, recordID, reviewerID int64, approve bool, reason string) error {
	reason = strings.TrimSpace(reason)
	if !approve && reason == "" {
		return apperr.BadRequest("请填写驳回原因")
	}
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var userID int64
		var kind, result string
		err := tx.QueryRow(ctx, `SELECT user_id, kind, result FROM kyc_records WHERE id = $1 FOR UPDATE`, recordID).
			Scan(&userID, &kind, &result)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if kind != "enterprise" || result != "pending" {
			return apperr.ErrInvalidState
		}
		newResult := "rejected"
		if approve {
			newResult = "verified"
		}
		if _, err := tx.Exec(ctx, `UPDATE kyc_records SET result = $2, reason = $3, reviewer_id = $4, updated_at = $5 WHERE id = $1`,
			recordID, newResult, reason, reviewerID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET kyc_status = $2,
				kind = CASE WHEN $2 = 'verified' THEN 'enterprise' ELSE kind END, updated_at = $3
			WHERE id = $1`, userID, newResult, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorType: "admin", ActorID: reviewerID, Action: "kyc.review",
			TargetType: "kyc_record", TargetID: strconv.FormatInt(recordID, 10),
			Diff: map[string]any{"approve": approve, "reason": reason, "user_id": userID},
		})
	})
}
```

注意：`idNumberRe` 只接受大写 X，输入在校验前已 `ToUpper`。

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/identity/`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add -A
git commit -m "feat(identity): personal and enterprise KYC with encrypted id numbers

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: 钱包（余额与流水）

**Files:**
- Create: `migrations/00004_wallet.sql`
- Create: `internal/wallet/wallet.go`, `internal/wallet/wallet_test.go`
- Modify: `internal/platform/dbtest/fixtures.go`（增加 `Fund`）

**Interfaces:**
- Consumes: `db.WithTx`、`audit.Record`、`apperr`
- Produces:
  - `wallet.Kind`，常量 `KindRecharge KindAdminAdjust KindOrderPay KindRefund`
  - `wallet.Entry{UserID int64; Amount int64; Kind Kind; RefType, RefID string; OperatorID int64; Remark string}`
  - `wallet.LedgerRow{ID, Amount, BalanceAfter int64; Kind Kind; RefType, RefID, Remark string; CreatedAt time.Time}`（JSON：`id amount balance_after kind ref_type ref_id remark created_at`）
  - `wallet.NewService(pool *pgxpool.Pool, clk clock.Clock) *Service`
  - `(*Service).Apply(ctx, tx pgx.Tx, e Entry) (balanceAfter int64, err error)`
  - `(*Service).Balance(ctx, userID int64) (int64, error)`
  - `(*Service).ListLedger(ctx, userID, beforeID int64, limit int) ([]LedgerRow, error)`
  - `(*Service).AdminAdjust(ctx, operatorID, userID, amount int64, remark, ip string) (int64, error)`
  - `dbtest.Fund(t, pool, userID, amount int64)`

- [ ] **Step 1: 写迁移**

`migrations/00004_wallet.sql`:

```sql
-- +goose Up
CREATE TABLE wallet_accounts (
    user_id    BIGINT PRIMARY KEY REFERENCES users (id),
    balance    BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE wallet_ledger (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users (id),
    amount        BIGINT NOT NULL CHECK (amount <> 0),
    balance_after BIGINT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('recharge', 'admin_adjust', 'order_pay', 'refund')),
    ref_type      TEXT NOT NULL DEFAULT '',
    ref_id        TEXT NOT NULL DEFAULT '',
    operator_id   BIGINT,
    remark        TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX wallet_ledger_user_idx ON wallet_ledger (user_id, id DESC);
-- 同一业务单据同一类型只能记一次账（例如同一订单只能退款一次）
CREATE UNIQUE INDEX wallet_ledger_ref_uniq ON wallet_ledger (kind, ref_type, ref_id) WHERE ref_id <> '';

-- +goose Down
DROP TABLE wallet_ledger;
DROP TABLE wallet_accounts;
```

- [ ] **Step 2: 增加 `dbtest.Fund` 夹具**

在 `internal/platform/dbtest/fixtures.go` 末尾追加：

```go
// Fund 给用户直接加余额（同时写一条 admin_adjust 流水，保持账实一致）。
func Fund(t *testing.T, pool *pgxpool.Pool, userID, amount int64) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		WITH acc AS (
			INSERT INTO wallet_accounts (user_id, balance, updated_at) VALUES ($1, $2, $3)
			ON CONFLICT (user_id) DO UPDATE SET balance = wallet_accounts.balance + EXCLUDED.balance
			RETURNING balance
		)
		INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, remark, created_at)
		SELECT $1, $2, balance, 'admin_adjust', 'test fund', $3 FROM acc`, userID, amount, T0)
	if err != nil {
		t.Fatalf("Fund: %v", err)
	}
}
```

- [ ] **Step 3: 写钱包的失败测试**

`internal/wallet/wallet_test.go`:

```go
package wallet_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/wallet"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func newEnv(t *testing.T) (*wallet.Service, *pgxpool.Pool) {
	pool := dbtest.New(t)
	return wallet.NewService(pool, clock.NewFake(dbtest.T0)), pool
}

func apply(ctx context.Context, pool *pgxpool.Pool, svc *wallet.Service, e wallet.Entry) (int64, error) {
	var after int64
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		after, err = svc.Apply(ctx, tx, e)
		return err
	})
	return after, err
}

func TestCreditDebitAndLedger(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000001", true)

	if after, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: 10000, Kind: wallet.KindRecharge, RefType: "recharge", RefID: "r1"}); err != nil || after != 10000 {
		t.Fatalf("credit: %d, %v", after, err)
	}
	if after, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -3000, Kind: wallet.KindOrderPay, RefType: "order", RefID: "o1"}); err != nil || after != 7000 {
		t.Fatalf("debit: %d, %v", after, err)
	}
	bal, _ := svc.Balance(ctx, uid)
	if bal != 7000 {
		t.Fatalf("balance = %d", bal)
	}
	rows, err := svc.ListLedger(ctx, uid, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Amount != -3000 || rows[0].BalanceAfter != 7000 || rows[1].Amount != 10000 {
		t.Fatalf("ledger = %+v", rows)
	}
	older, _ := svc.ListLedger(ctx, uid, rows[0].ID, 10)
	if len(older) != 1 || older[0].ID != rows[1].ID {
		t.Fatalf("paging = %+v", older)
	}
}

func TestBalanceOfNewUserIsZero(t *testing.T) {
	svc, pool := newEnv(t)
	uid := dbtest.CreateUser(t, pool, "13700000002", true)
	if bal, err := svc.Balance(context.Background(), uid); err != nil || bal != 0 {
		t.Fatalf("balance = %d, %v", bal, err)
	}
}

func TestInsufficientBalanceLeavesNoTrace(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000003", true)
	dbtest.Fund(t, pool, uid, 500)

	_, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -501, Kind: wallet.KindOrderPay, RefType: "order", RefID: "o1"})
	if !errors.Is(err, apperr.ErrInsufficientBalance) {
		t.Fatalf("want insufficient balance, got %v", err)
	}
	bal, _ := svc.Balance(ctx, uid)
	rows, _ := svc.ListLedger(ctx, uid, 0, 10)
	if bal != 500 || len(rows) != 1 {
		t.Fatalf("balance = %d, ledger rows = %d", bal, len(rows))
	}
}

func TestDuplicateRefRejected(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000004", true)
	e := wallet.Entry{UserID: uid, Amount: 100, Kind: wallet.KindRefund, RefType: "order", RefID: "o1"}
	if _, err := apply(ctx, pool, svc, e); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(ctx, pool, svc, e); !errors.Is(err, apperr.ErrDuplicate) {
		t.Fatalf("want duplicate, got %v", err)
	}
	if bal, _ := svc.Balance(ctx, uid); bal != 100 {
		t.Fatalf("balance = %d, want 100", bal)
	}
}

func TestConcurrentDebitsNeverOverdraw(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000005", true)
	dbtest.Fund(t, pool, uid, 1000)

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -100, Kind: wallet.KindOrderPay, RefType: "order", RefID: string(rune('a' + i))})
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !errors.Is(err, apperr.ErrInsufficientBalance) {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	bal, _ := svc.Balance(ctx, uid)
	if ok != 10 || bal != 0 {
		t.Fatalf("succeeded = %d, balance = %d; want 10, 0", ok, bal)
	}
}

func TestAdminAdjustRequiresRemarkAndAudits(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13700000006", true)

	if _, err := svc.AdminAdjust(ctx, 7, uid, 5000, " ", "10.0.0.1"); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
	after, err := svc.AdminAdjust(ctx, 7, uid, 5000, "对公转账 #123", "10.0.0.1")
	if err != nil || after != 5000 {
		t.Fatalf("adjust = %d, %v", after, err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'wallet.adjust' AND actor_id = 7`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
}
```

- [ ] **Step 4: 运行测试确认失败**

Run: `go test ./internal/wallet/`
Expected: FAIL（`wallet` 包不存在）

- [ ] **Step 5: 实现钱包**

`internal/wallet/wallet.go`:

```go
// Package wallet 管理用户余额。所有变动先写只追加的流水，再更新余额。
package wallet

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
)

type Kind string

const (
	KindRecharge    Kind = "recharge"
	KindAdminAdjust Kind = "admin_adjust"
	KindOrderPay    Kind = "order_pay"
	KindRefund      Kind = "refund"
)

type Entry struct {
	UserID     int64
	Amount     int64 // 分；正数入账，负数扣款
	Kind       Kind
	RefType    string
	RefID      string
	OperatorID int64 // 管理员操作时填写
	Remark     string
}

type LedgerRow struct {
	ID           int64     `json:"id"`
	Amount       int64     `json:"amount"`
	BalanceAfter int64     `json:"balance_after"`
	Kind         Kind      `json:"kind"`
	RefType      string    `json:"ref_type"`
	RefID        string    `json:"ref_id"`
	Remark       string    `json:"remark"`
	CreatedAt    time.Time `json:"created_at"`
}

type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

func NewService(pool *pgxpool.Pool, clk clock.Clock) *Service {
	return &Service{pool: pool, clock: clk}
}

// Apply 在调用方事务内记一笔账并更新余额。
// 余额不足返回 ErrInsufficientBalance；同一 (kind, ref_type, ref_id) 重复记账返回 ErrDuplicate。
// 任何错误都要求调用方回滚事务。
func (s *Service) Apply(ctx context.Context, tx pgx.Tx, e Entry) (int64, error) {
	if e.Amount == 0 {
		return 0, apperr.BadRequest("金额不能为 0")
	}
	now := s.clock.Now()
	if _, err := tx.Exec(ctx, `INSERT INTO wallet_accounts (user_id, balance, updated_at) VALUES ($1, 0, $2)
		ON CONFLICT (user_id) DO NOTHING`, e.UserID, now); err != nil {
		return 0, err
	}
	var bal int64
	if err := tx.QueryRow(ctx, `SELECT balance FROM wallet_accounts WHERE user_id = $1 FOR UPDATE`, e.UserID).Scan(&bal); err != nil {
		return 0, err
	}
	after := bal + e.Amount
	if after < 0 {
		return 0, apperr.ErrInsufficientBalance
	}
	var operator *int64
	if e.OperatorID != 0 {
		operator = &e.OperatorID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, operator_id, remark, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.UserID, e.Amount, after, string(e.Kind), e.RefType, e.RefID, operator, e.Remark, now); err != nil {
		if db.IsUniqueViolation(err) {
			return 0, apperr.ErrDuplicate
		}
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE wallet_accounts SET balance = $2, updated_at = $3 WHERE user_id = $1`, e.UserID, after, now); err != nil {
		return 0, err
	}
	return after, nil
}

func (s *Service) Balance(ctx context.Context, userID int64) (int64, error) {
	var bal int64
	err := s.pool.QueryRow(ctx, `SELECT balance FROM wallet_accounts WHERE user_id = $1`, userID).Scan(&bal)
	if db.IsNoRows(err) {
		return 0, nil
	}
	return bal, err
}

// ListLedger 按时间倒序分页；beforeID 为 0 表示从最新开始。
func (s *Service) ListLedger(ctx context.Context, userID, beforeID int64, limit int) ([]LedgerRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if beforeID <= 0 {
		beforeID = 1<<63 - 1
	}
	rows, err := s.pool.Query(ctx, `SELECT id, amount, balance_after, kind, ref_type, ref_id, remark, created_at
		FROM wallet_ledger WHERE user_id = $1 AND id < $2 ORDER BY id DESC LIMIT $3`, userID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LedgerRow, error) {
		var l LedgerRow
		err := r.Scan(&l.ID, &l.Amount, &l.BalanceAfter, &l.Kind, &l.RefType, &l.RefID, &l.Remark, &l.CreatedAt)
		return l, err
	})
}

// AdminAdjust 管理员手动加减余额（线下转账入账、补偿、纠错），必须填写原因并写审计日志。
func (s *Service) AdminAdjust(ctx context.Context, operatorID, userID, amount int64, remark, ip string) (int64, error) {
	remark = strings.TrimSpace(remark)
	if remark == "" {
		return 0, apperr.BadRequest("请填写调整原因")
	}
	var after int64
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		after, err = s.Apply(ctx, tx, Entry{UserID: userID, Amount: amount, Kind: KindAdminAdjust, OperatorID: operatorID, Remark: remark})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.clock.Now(), audit.Entry{
			ActorType: "admin", ActorID: operatorID, Action: "wallet.adjust",
			TargetType: "user", TargetID: strconv.FormatInt(userID, 10),
			Diff: map[string]any{"amount": amount, "balance_after": after, "remark": remark}, IP: ip,
		})
	})
	return after, err
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/wallet/`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add -A
git commit -m "feat(wallet): append-only ledger with idempotent refs and admin adjust

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: 商品、价格与镜像（catalog）

**Files:**
- Create: `migrations/00005_catalog.sql`
- Create: `internal/catalog/catalog.go`, `internal/catalog/catalog_test.go`
- Modify: `internal/platform/dbtest/fixtures.go`（增加 `ProductID`、`ImageID`）

**Interfaces:**
- Consumes: `db.Querier`、`apperr`
- Produces:
  - `catalog.Duration`（`string`），常量 `D1d="1d" D1w="1w" D1m="1m"`；`(Duration).Valid() bool`；`(Duration).Length() time.Duration`（1m = 30 天）
  - `catalog.Product{ID int64; Code, Name string; Spec map[string]any}`，`catalog.Price{Duration Duration; Amount int64}`，`catalog.ProductWithPrices{Product; Prices []Price}`
  - `catalog.Image{ID int64; Name, Ref, Description string}`（`Ref` 不输出到 JSON）
  - `catalog.NewService(pool *pgxpool.Pool) *Service`
  - `(*Service).ListProducts(ctx) ([]ProductWithPrices, error)`
  - `(*Service).Quote(ctx, q db.Querier, productID int64, d Duration) (int64, error)`
  - `(*Service).GetImage(ctx, q db.Querier, id int64) (Image, error)`
  - `(*Service).ListImages(ctx) ([]Image, error)`
  - `dbtest.ProductID(t, pool) int64`（code = `spark-1`），`dbtest.ImageID(t, pool) int64`（排序最靠前的启用镜像）

- [ ] **Step 1: 写迁移（含种子数据）**

`migrations/00005_catalog.sql`:

```sql
-- +goose Up
CREATE TABLE products (
    id         BIGSERIAL PRIMARY KEY,
    code       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    spec       JSONB NOT NULL DEFAULT '{}',
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE prices (
    product_id BIGINT NOT NULL REFERENCES products (id),
    duration   TEXT NOT NULL CHECK (duration IN ('1d', '1w', '1m')),
    amount     BIGINT NOT NULL CHECK (amount > 0),
    enabled    BOOLEAN NOT NULL DEFAULT true,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (product_id, duration)
);

CREATE TABLE images (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    ref         TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    enabled     BOOLEAN NOT NULL DEFAULT true,
    sort        INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);

-- 初始商品与价格（单位：分），参考竞品 ¥69/天、¥414/周、¥1599/月，可在管理后台修改
INSERT INTO products (code, name, spec, created_at, updated_at) VALUES
    ('spark-1', 'DGX Spark 整机',
     '{"gpu": "NVIDIA GB10 Grace Blackwell", "memory_gb": 128, "disk_tb": 4, "cpu": "20 核 Arm"}', now(), now());
INSERT INTO prices (product_id, duration, amount, updated_at)
SELECT id, d.duration, d.amount, now()
FROM products, (VALUES ('1d', 6900), ('1w', 41400), ('1m', 159900)) AS d (duration, amount)
WHERE code = 'spark-1';

-- 镜像地址在计划 2 搭建内网 Harbor 后替换
INSERT INTO images (name, ref, description, sort, created_at, updated_at) VALUES
    ('Ubuntu 24.04 + CUDA', 'registry.spark.internal/base/cuda:ubuntu24.04', '基础 CUDA 开发环境', 10, now(), now()),
    ('PyTorch', 'registry.spark.internal/base/pytorch:latest', 'PyTorch + JupyterLab', 20, now(), now()),
    ('vLLM', 'registry.spark.internal/base/vllm:latest', 'vLLM 推理服务环境', 30, now(), now());

-- +goose Down
DROP TABLE images;
DROP TABLE prices;
DROP TABLE products;
```

- [ ] **Step 2: 增加夹具**

在 `internal/platform/dbtest/fixtures.go` 末尾追加：

```go
// ProductID 返回种子商品 spark-1 的 ID。
func ProductID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM products WHERE code = 'spark-1'`).Scan(&id); err != nil {
		t.Fatalf("ProductID: %v", err)
	}
	return id
}

// ImageID 返回排序最靠前的启用镜像 ID。
func ImageID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM images WHERE enabled ORDER BY sort, id LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("ImageID: %v", err)
	}
	return id
}
```

- [ ] **Step 3: 写 catalog 的失败测试**

`internal/catalog/catalog_test.go`:

```go
package catalog_test

import (
	"context"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func TestDurationLength(t *testing.T) {
	cases := map[catalog.Duration]time.Duration{
		catalog.D1d: 24 * time.Hour, catalog.D1w: 7 * 24 * time.Hour, catalog.D1m: 30 * 24 * time.Hour,
	}
	for d, want := range cases {
		if !d.Valid() || d.Length() != want {
			t.Fatalf("%s: valid=%v length=%v", d, d.Valid(), d.Length())
		}
	}
	for _, bad := range []catalog.Duration{"1h", "2d", ""} {
		if bad.Valid() {
			t.Fatalf("%q must be invalid", bad)
		}
	}
}

func TestListProductsWithSeededPrices(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	products, err := svc.ListProducts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(products) != 1 || products[0].Code != "spark-1" {
		t.Fatalf("products = %+v", products)
	}
	got := products[0].Prices
	want := []catalog.Price{{"1d", 6900}, {"1w", 41400}, {"1m", 159900}}
	if len(got) != len(want) {
		t.Fatalf("prices = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("prices[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if products[0].Spec["memory_gb"].(float64) != 128 {
		t.Fatalf("spec = %+v", products[0].Spec)
	}
}

func TestQuote(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)

	amount, err := svc.Quote(ctx, pool, pid, catalog.D1w)
	if err != nil || amount != 41400 {
		t.Fatalf("Quote = %d, %v", amount, err)
	}
	if _, err := svc.Quote(ctx, pool, pid, "1h"); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("invalid duration want bad_request, got %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE prices SET enabled = false WHERE duration = '1d'`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Quote(ctx, pool, pid, catalog.D1d); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("disabled price want bad_request, got %v", err)
	}
}

func TestImages(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	ctx := context.Background()
	images, err := svc.ListImages(ctx)
	if err != nil || len(images) != 3 || images[0].Name != "Ubuntu 24.04 + CUDA" {
		t.Fatalf("images = %+v, %v", images, err)
	}
	img, err := svc.GetImage(ctx, pool, images[1].ID)
	if err != nil || img.Ref != "registry.spark.internal/base/pytorch:latest" {
		t.Fatalf("GetImage = %+v, %v", img, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE images SET enabled = false WHERE id = $1`, img.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetImage(ctx, pool, img.ID); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("disabled image want bad_request, got %v", err)
	}
	if _, err := svc.GetImage(ctx, pool, 99999); err == nil {
		t.Fatal("missing image must error")
	}
}
```

- [ ] **Step 4: 运行测试确认失败**

Run: `go test ./internal/catalog/`
Expected: FAIL（`catalog` 包不存在）

- [ ] **Step 5: 实现 catalog**

`internal/catalog/catalog.go`:

```go
// Package catalog 管理可售商品、租期价格与系统镜像。
package catalog

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

type Duration string

const (
	D1d Duration = "1d"
	D1w Duration = "1w"
	D1m Duration = "1m"
)

func (d Duration) Valid() bool { return d.Length() > 0 }

// Length 返回租期时长；1m 按 30 天计。
func (d Duration) Length() time.Duration {
	switch d {
	case D1d:
		return 24 * time.Hour
	case D1w:
		return 7 * 24 * time.Hour
	case D1m:
		return 30 * 24 * time.Hour
	}
	return 0
}

type Product struct {
	ID   int64          `json:"id"`
	Code string         `json:"code"`
	Name string         `json:"name"`
	Spec map[string]any `json:"spec"`
}

type Price struct {
	Duration Duration `json:"duration"`
	Amount   int64    `json:"amount"`
}

type ProductWithPrices struct {
	Product
	Prices []Price `json:"prices"`
}

type Image struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Ref         string `json:"-"`
	Description string `json:"description"`
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) ListProducts(ctx context.Context) ([]ProductWithPrices, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, code, name, spec FROM products WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	products, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProductWithPrices, error) {
		var p ProductWithPrices
		err := r.Scan(&p.ID, &p.Code, &p.Name, &p.Spec)
		return p, err
	})
	if err != nil {
		return nil, err
	}
	for i := range products {
		rows, err := s.pool.Query(ctx, `SELECT duration, amount FROM prices
			WHERE product_id = $1 AND enabled
			ORDER BY array_position(ARRAY['1d', '1w', '1m'], duration)`, products[i].ID)
		if err != nil {
			return nil, err
		}
		products[i].Prices, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Price, error) {
			var p Price
			err := r.Scan(&p.Duration, &p.Amount)
			return p, err
		})
		if err != nil {
			return nil, err
		}
	}
	return products, nil
}

// Quote 返回指定商品与租期的当前价格（分）。
func (s *Service) Quote(ctx context.Context, q db.Querier, productID int64, d Duration) (int64, error) {
	if !d.Valid() {
		return 0, apperr.BadRequest("租期不正确")
	}
	var amount int64
	err := q.QueryRow(ctx, `SELECT p.amount FROM prices p JOIN products pr ON pr.id = p.product_id
		WHERE p.product_id = $1 AND p.duration = $2 AND p.enabled AND pr.status = 'active'`, productID, string(d)).Scan(&amount)
	if db.IsNoRows(err) {
		return 0, apperr.BadRequest("该规格暂不支持此租期")
	}
	return amount, err
}

func (s *Service) GetImage(ctx context.Context, q db.Querier, id int64) (Image, error) {
	var img Image
	err := q.QueryRow(ctx, `SELECT id, name, ref, description FROM images WHERE id = $1 AND enabled`, id).
		Scan(&img.ID, &img.Name, &img.Ref, &img.Description)
	if db.IsNoRows(err) {
		return Image{}, apperr.BadRequest("镜像不存在或已下架")
	}
	return img, err
}

func (s *Service) ListImages(ctx context.Context) ([]Image, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, ref, description FROM images WHERE enabled ORDER BY sort, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Image, error) {
		var img Image
		err := r.Scan(&img.ID, &img.Name, &img.Ref, &img.Description)
		return img, err
	})
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/catalog/`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add -A
git commit -m "feat(catalog): products, duration prices and images with seed data

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: 机器库存、心跳、指令队列与告警（fleet）

**Files:**
- Create: `migrations/00006_fleet.sql`
- Create: `internal/fleet/machines.go`, `internal/fleet/commands.go`, `internal/fleet/alerts.go`
- Create: `internal/fleet/machines_test.go`, `internal/fleet/commands_test.go`
- Modify: `internal/platform/dbtest/fixtures.go`（增加 `CreateMachine`）

**Interfaces:**
- Consumes: `db`、`apperr`、`clock`
- Produces:
  - 常量 `fleet.StateIdle="idle" StateAllocated="allocated" StateMaintenance="maintenance" StateFaulty="faulty"`
  - `fleet.Machine{ID int64; Serial, Rack, Slot, LanIP string; ProductID int64; State string; AgentOnline bool; LastSeenAt *time.Time; CreatedAt time.Time}`
  - `fleet.NewMachine{Serial, Rack, Slot, LanIP string; ProductID int64}`
  - `fleet.NewService(pool *pgxpool.Pool, clk clock.Clock) *Service`
  - `(*Service).Register(ctx, nm NewMachine) (Machine, error)`；`Get(ctx, q db.Querier, id int64) (Machine, error)`；`SetMaintenance(ctx, id int64, on bool) error`
  - `(*Service).Allocate(ctx, tx pgx.Tx, productID int64) (Machine, error)`（无机器返回 `ErrNoCapacity`）
  - `(*Service).Release(ctx, q db.Querier, machineID int64) error`；`MarkFaulty(ctx, q db.Querier, machineID int64) error`；`IdleCount(ctx, productID int64) (int, error)`
  - `(*Service).Heartbeat(ctx, machineID int64) (cameOnline bool, err error)`；`MarkOfflineStale(ctx, timeout time.Duration) ([]int64, error)`
  - `fleet.CommandType`，常量 `CmdCreateInstance="create_instance" CmdStartInstance="start_instance" CmdStopInstance="stop_instance" CmdWipeInstance="wipe_instance"`
  - `fleet.CreateInstancePayload{ImageRef string}`（JSON `image_ref`）
  - `fleet.Command{ID string; MachineID int64; InstanceID string; Type CommandType; Payload json.RawMessage; Status, Result string; Attempts int; CreatedAt time.Time}`
  - `(*Service).Enqueue(ctx, q db.Querier, machineID int64, instanceID string, t CommandType, payload any) (string, error)`
  - `(*Service).PendingCommands(ctx, machineID int64) ([]Command, error)`
  - `(*Service).CompleteCommand(ctx, q db.Querier, id string, ok bool, result string) (cmd Command, applied bool, err error)`
  - `(*Service).CancelCommands(ctx, q db.Querier, instanceID, reason string) error`
  - `fleet.Alert{Level, Kind string; MachineID int64; InstanceID, Message string}`；`(*Service).RaiseAlert(ctx, q db.Querier, a Alert) error`
  - `dbtest.CreateMachine(t, pool, serial string) int64`（spark-1、idle、在线、`last_seen_at = T0`）

- [ ] **Step 1: 写迁移**

`migrations/00006_fleet.sql`:

```sql
-- +goose Up
CREATE TABLE machines (
    id               BIGSERIAL PRIMARY KEY,
    serial           TEXT NOT NULL UNIQUE,
    rack             TEXT NOT NULL DEFAULT '',
    slot             TEXT NOT NULL DEFAULT '',
    lan_ip           TEXT NOT NULL,
    product_id       BIGINT NOT NULL REFERENCES products (id),
    state            TEXT NOT NULL DEFAULT 'maintenance'
                     CHECK (state IN ('idle', 'allocated', 'maintenance', 'faulty')),
    agent_online     BOOLEAN NOT NULL DEFAULT false,
    last_seen_at     TIMESTAMPTZ,
    cert_fingerprint TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX machines_alloc_idx ON machines (product_id, id) WHERE state = 'idle' AND agent_online;

CREATE TABLE agent_commands (
    id          TEXT PRIMARY KEY,
    machine_id  BIGINT NOT NULL REFERENCES machines (id),
    instance_id TEXT NOT NULL DEFAULT '',
    type        TEXT NOT NULL,
    payload     JSONB NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'done', 'failed')),
    result      TEXT NOT NULL DEFAULT '',
    attempts    INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX agent_commands_open_idx ON agent_commands (machine_id, created_at) WHERE status IN ('pending', 'sent');
CREATE INDEX agent_commands_instance_idx ON agent_commands (instance_id) WHERE status IN ('pending', 'sent');

CREATE TABLE alerts (
    id          BIGSERIAL PRIMARY KEY,
    level       TEXT NOT NULL CHECK (level IN ('warning', 'critical')),
    kind        TEXT NOT NULL,
    machine_id  BIGINT REFERENCES machines (id),
    instance_id TEXT NOT NULL DEFAULT '',
    message     TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ
);
CREATE INDEX alerts_open_idx ON alerts (created_at DESC) WHERE resolved_at IS NULL;

-- +goose Down
DROP TABLE alerts;
DROP TABLE agent_commands;
DROP TABLE machines;
```

- [ ] **Step 2: 增加 `CreateMachine` 夹具**

在 `internal/platform/dbtest/fixtures.go` 末尾追加：

```go
// CreateMachine 插入一台属于 spark-1、空闲且在线的机器（last_seen_at = T0）。
func CreateMachine(t *testing.T, pool *pgxpool.Pool, serial string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `INSERT INTO machines
		(serial, lan_ip, product_id, state, agent_online, last_seen_at, created_at, updated_at)
		VALUES ($1, '10.0.0.10', (SELECT id FROM products WHERE code = 'spark-1'), 'idle', true, $2, $2, $2)
		RETURNING id`, serial, T0).Scan(&id)
	if err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	return id
}
```

- [ ] **Step 3: 写机器部分的失败测试**

`internal/fleet/machines_test.go`:

```go
package fleet_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func newEnv(t *testing.T) (*fleet.Service, *pgxpool.Pool, *clock.Fake) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	return fleet.NewService(pool, clk), pool, clk
}

func allocate(ctx context.Context, pool *pgxpool.Pool, svc *fleet.Service, productID int64) (fleet.Machine, error) {
	var m fleet.Machine
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		m, err = svc.Allocate(ctx, tx, productID)
		return err
	})
	return m, err
}

func TestRegisterValidatesAndRejectsDuplicates(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)

	m, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN001", Rack: "A01", Slot: "03", LanIP: "10.0.1.3", ProductID: pid})
	if err != nil {
		t.Fatal(err)
	}
	if m.State != fleet.StateMaintenance || m.AgentOnline {
		t.Fatalf("new machine = %+v", m)
	}
	_, err = svc.Register(ctx, fleet.NewMachine{Serial: "SN001", LanIP: "10.0.1.4", ProductID: pid})
	if ae := apperr.As(err); ae == nil || ae.Code != "duplicate_serial" {
		t.Fatalf("want duplicate_serial, got %v", err)
	}
	_, err = svc.Register(ctx, fleet.NewMachine{Serial: "SN002", LanIP: "not-an-ip", ProductID: pid})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
}

func TestMaintenanceTransitions(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	m, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN001", LanIP: "10.0.1.3", ProductID: dbtest.ProductID(t, pool)})

	if err := svc.SetMaintenance(ctx, m.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, pool, m.ID); got.State != fleet.StateIdle {
		t.Fatalf("state = %s, want idle", got.State)
	}
	if err := svc.SetMaintenance(ctx, m.ID, false); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("idle->idle want invalid state, got %v", err)
	}
	if err := svc.SetMaintenance(ctx, m.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMaintenance(ctx, 9999, true); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing machine want not found, got %v", err)
	}
}

func TestAllocateRequiresIdleAndOnline(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	m, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN001", LanIP: "10.0.1.3", ProductID: pid})
	_ = svc.SetMaintenance(ctx, m.ID, false)

	if _, err := allocate(ctx, pool, svc, pid); !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("offline machine must not be allocated, got %v", err)
	}
	if _, err := svc.Heartbeat(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.IdleCount(ctx, pid); n != 1 {
		t.Fatalf("IdleCount = %d, want 1", n)
	}
	got, err := allocate(ctx, pool, svc, pid)
	if err != nil || got.ID != m.ID || got.State != fleet.StateAllocated {
		t.Fatalf("allocate = %+v, %v", got, err)
	}
	if _, err := allocate(ctx, pool, svc, pid); !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("second allocate want no capacity, got %v", err)
	}
	if err := svc.Release(ctx, pool, m.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.IdleCount(ctx, pid); n != 1 {
		t.Fatalf("after release IdleCount = %d, want 1", n)
	}
}

func TestConcurrentAllocationNeverDoubleAssigns(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	for i := 0; i < 10; i++ {
		dbtest.CreateMachine(t, pool, fmt.Sprintf("SN%03d", i))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[int64]int{}
	noCap := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := allocate(ctx, pool, svc, pid)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				got[m.ID]++
			case errors.Is(err, apperr.ErrNoCapacity):
				noCap++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(got) != 10 || noCap != 90 {
		t.Fatalf("distinct machines = %d, no capacity = %d", len(got), noCap)
	}
	for id, n := range got {
		if n != 1 {
			t.Fatalf("machine %d allocated %d times", id, n)
		}
	}
}

func TestHeartbeatAndOfflineDetection(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN001")

	if came, err := svc.Heartbeat(ctx, id); err != nil || came {
		t.Fatalf("heartbeat on online machine: came=%v err=%v", came, err)
	}
	clk.Advance(5*time.Minute + time.Second)
	offline, err := svc.MarkOfflineStale(ctx, 5*time.Minute)
	if err != nil || len(offline) != 1 || offline[0] != id {
		t.Fatalf("offline = %v, %v", offline, err)
	}
	if again, _ := svc.MarkOfflineStale(ctx, 5*time.Minute); len(again) != 0 {
		t.Fatalf("already offline machines reported again: %v", again)
	}
	if came, err := svc.Heartbeat(ctx, id); err != nil || !came {
		t.Fatalf("heartbeat after offline: came=%v err=%v", came, err)
	}
	if _, err := svc.Heartbeat(ctx, 9999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown machine want not found, got %v", err)
	}
}

func TestMarkFaultyAndAlert(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN001")
	if err := svc.MarkFaulty(ctx, pool, id); err != nil {
		t.Fatal(err)
	}
	if m, _ := svc.Get(ctx, pool, id); m.State != fleet.StateFaulty {
		t.Fatalf("state = %s", m.State)
	}
	if err := svc.RaiseAlert(ctx, pool, fleet.Alert{Level: "critical", Kind: "wipe_failed", MachineID: id, Message: "disk error"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RaiseAlert(ctx, pool, fleet.Alert{Level: "warning", Kind: "system", Message: "no machine"}); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alerts`).Scan(&n)
	if n != 2 {
		t.Fatalf("alerts = %d", n)
	}
}
```

- [ ] **Step 4: 写指令队列的失败测试**

`internal/fleet/commands_test.go`:

```go
package fleet_test

import (
	"context"
	"encoding/json"
	"testing"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
)

func TestCommandLifecycleIsIdempotent(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN001")

	id, err := svc.Enqueue(ctx, pool, mid, "ins-abc", fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: "img:1"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := svc.PendingCommands(ctx, mid)
	if err != nil || len(pending) != 1 || pending[0].ID != id || pending[0].Type != fleet.CmdCreateInstance {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	var p fleet.CreateInstancePayload
	if err := json.Unmarshal(pending[0].Payload, &p); err != nil || p.ImageRef != "img:1" {
		t.Fatalf("payload = %s, %v", pending[0].Payload, err)
	}

	cmd, applied, err := svc.CompleteCommand(ctx, pool, id, true, "ok")
	if err != nil || !applied || cmd.Status != "done" || cmd.InstanceID != "ins-abc" {
		t.Fatalf("complete = %+v applied=%v err=%v", cmd, applied, err)
	}
	cmd, applied, err = svc.CompleteCommand(ctx, pool, id, false, "late failure")
	if err != nil || applied || cmd.Status != "done" {
		t.Fatalf("second complete = %+v applied=%v err=%v", cmd, applied, err)
	}
	if pending, _ := svc.PendingCommands(ctx, mid); len(pending) != 0 {
		t.Fatalf("pending after done = %+v", pending)
	}
}

func TestCancelCommands(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN001")
	id, _ := svc.Enqueue(ctx, pool, mid, "ins-abc", fleet.CmdStartInstance, nil)
	_, _ = svc.Enqueue(ctx, pool, mid, "ins-other", fleet.CmdStopInstance, nil)

	if err := svc.CancelCommands(ctx, pool, "ins-abc", "superseded"); err != nil {
		t.Fatal(err)
	}
	pending, _ := svc.PendingCommands(ctx, mid)
	if len(pending) != 1 || pending[0].InstanceID != "ins-other" {
		t.Fatalf("pending = %+v", pending)
	}
	if _, applied, _ := svc.CompleteCommand(ctx, pool, id, true, "ok"); applied {
		t.Fatal("cancelled command must not be applied")
	}
}
```

- [ ] **Step 5: 运行测试确认失败**

Run: `go test ./internal/fleet/`
Expected: FAIL（`fleet` 包不存在）

- [ ] **Step 6: 实现 fleet**

`internal/fleet/machines.go`:

```go
// Package fleet 管理物理机器：库存分配、Agent 心跳、指令队列与告警。
package fleet

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
)

const (
	StateIdle        = "idle"
	StateAllocated   = "allocated"
	StateMaintenance = "maintenance"
	StateFaulty      = "faulty"
)

type Machine struct {
	ID          int64      `json:"id"`
	Serial      string     `json:"serial"`
	Rack        string     `json:"rack"`
	Slot        string     `json:"slot"`
	LanIP       string     `json:"lan_ip"`
	ProductID   int64      `json:"product_id"`
	State       string     `json:"state"`
	AgentOnline bool       `json:"agent_online"`
	LastSeenAt  *time.Time `json:"last_seen_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

type NewMachine struct {
	Serial    string
	Rack      string
	Slot      string
	LanIP     string
	ProductID int64
}

type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

func NewService(pool *pgxpool.Pool, clk clock.Clock) *Service {
	return &Service{pool: pool, clock: clk}
}

const machineCols = `id, serial, rack, slot, lan_ip, product_id, state, agent_online, last_seen_at, created_at`

func scanMachine(row pgx.Row) (Machine, error) {
	var m Machine
	err := row.Scan(&m.ID, &m.Serial, &m.Rack, &m.Slot, &m.LanIP, &m.ProductID, &m.State, &m.AgentOnline, &m.LastSeenAt, &m.CreatedAt)
	if db.IsNoRows(err) {
		return Machine{}, apperr.ErrNotFound
	}
	return m, err
}

// Register 登记一台新机器，初始状态为维护中，需管理员确认后上线。
func (s *Service) Register(ctx context.Context, nm NewMachine) (Machine, error) {
	nm.Serial = strings.TrimSpace(nm.Serial)
	if nm.Serial == "" {
		return Machine{}, apperr.BadRequest("请填写序列号")
	}
	if net.ParseIP(nm.LanIP) == nil {
		return Machine{}, apperr.BadRequest("内网 IP 格式不正确")
	}
	now := s.clock.Now()
	m, err := scanMachine(s.pool.QueryRow(ctx, `INSERT INTO machines (serial, rack, slot, lan_ip, product_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6) RETURNING `+machineCols,
		nm.Serial, nm.Rack, nm.Slot, nm.LanIP, nm.ProductID, now))
	if db.IsUniqueViolation(err) {
		return Machine{}, apperr.New("duplicate_serial", "序列号已存在", 409)
	}
	return m, err
}

func (s *Service) Get(ctx context.Context, q db.Querier, id int64) (Machine, error) {
	return scanMachine(q.QueryRow(ctx, `SELECT `+machineCols+` FROM machines WHERE id = $1`, id))
}

// SetMaintenance 进入维护（仅空闲或故障机器）或结束维护回到空闲（仅维护中或故障机器）。
func (s *Service) SetMaintenance(ctx context.Context, id int64, on bool) error {
	from, to := []string{StateIdle, StateFaulty}, StateMaintenance
	if !on {
		from, to = []string{StateMaintenance, StateFaulty}, StateIdle
	}
	tag, err := s.pool.Exec(ctx, `UPDATE machines SET state = $2, updated_at = $3 WHERE id = $1 AND state = ANY($4)`,
		id, to, s.clock.Now(), from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, s.pool, id); err != nil {
			return err
		}
		return apperr.ErrInvalidState
	}
	return nil
}

// Allocate 在调用方事务内锁定一台空闲且在线的机器并标记为已分配。
func (s *Service) Allocate(ctx context.Context, tx pgx.Tx, productID int64) (Machine, error) {
	m, err := scanMachine(tx.QueryRow(ctx, `UPDATE machines SET state = 'allocated', updated_at = $2
		WHERE id = (
			SELECT id FROM machines
			WHERE product_id = $1 AND state = 'idle' AND agent_online
			ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED
		) RETURNING `+machineCols, productID, s.clock.Now()))
	if errors.Is(err, apperr.ErrNotFound) {
		return Machine{}, apperr.ErrNoCapacity
	}
	return m, err
}

// Release 把已分配的机器放回空闲；故障或维护中的机器保持原状态。
func (s *Service) Release(ctx context.Context, q db.Querier, machineID int64) error {
	_, err := q.Exec(ctx, `UPDATE machines SET state = 'idle', updated_at = $2 WHERE id = $1 AND state = 'allocated'`,
		machineID, s.clock.Now())
	return err
}

func (s *Service) MarkFaulty(ctx context.Context, q db.Querier, machineID int64) error {
	_, err := q.Exec(ctx, `UPDATE machines SET state = 'faulty', updated_at = $2 WHERE id = $1`, machineID, s.clock.Now())
	return err
}

func (s *Service) IdleCount(ctx context.Context, productID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM machines WHERE product_id = $1 AND state = 'idle' AND agent_online`,
		productID).Scan(&n)
	return n, err
}

// Heartbeat 记录 Agent 心跳；返回值表示机器是否刚从离线恢复。
func (s *Service) Heartbeat(ctx context.Context, machineID int64) (bool, error) {
	now := s.clock.Now()
	var wasOnline bool
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT agent_online FROM machines WHERE id = $1 FOR UPDATE`, machineID).Scan(&wasOnline)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE machines SET agent_online = true, last_seen_at = $2, updated_at = $2 WHERE id = $1`, machineID, now)
		return err
	})
	if err != nil {
		return false, err
	}
	return !wasOnline, nil
}

// MarkOfflineStale 把超过 timeout 没有心跳的在线机器标记为离线，返回这些机器的 ID。
func (s *Service) MarkOfflineStale(ctx context.Context, timeout time.Duration) ([]int64, error) {
	now := s.clock.Now()
	rows, err := s.pool.Query(ctx, `UPDATE machines SET agent_online = false, updated_at = $1
		WHERE agent_online AND (last_seen_at IS NULL OR last_seen_at < $2) RETURNING id`, now, now.Add(-timeout))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}
```

`internal/fleet/commands.go`:

```go
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

type CommandType string

const (
	CmdCreateInstance CommandType = "create_instance"
	CmdStartInstance  CommandType = "start_instance"
	CmdStopInstance   CommandType = "stop_instance"
	CmdWipeInstance   CommandType = "wipe_instance"
)

type CreateInstancePayload struct {
	ImageRef string `json:"image_ref"`
}

type Command struct {
	ID         string
	MachineID  int64
	InstanceID string
	Type       CommandType
	Payload    json.RawMessage
	Status     string
	Result     string
	Attempts   int
	CreatedAt  time.Time
}

const commandCols = `id, machine_id, instance_id, type, payload, status, result, attempts, created_at`

func scanCommand(row pgx.Row) (Command, error) {
	var c Command
	var payload []byte
	err := row.Scan(&c.ID, &c.MachineID, &c.InstanceID, &c.Type, &payload, &c.Status, &c.Result, &c.Attempts, &c.CreatedAt)
	if db.IsNoRows(err) {
		return Command{}, apperr.ErrNotFound
	}
	c.Payload = payload
	return c, err
}

// Enqueue 写入一条待投递给 Agent 的指令；通常在业务事务内调用。
func (s *Service) Enqueue(ctx context.Context, q db.Querier, machineID int64, instanceID string, t CommandType, payload any) (string, error) {
	body := []byte("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return "", err
		}
		body = b
	}
	id := uuid.NewString()
	now := s.clock.Now()
	_, err := q.Exec(ctx, `INSERT INTO agent_commands (id, machine_id, instance_id, type, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, id, machineID, instanceID, string(t), body, now)
	return id, err
}

// PendingCommands 返回某台机器尚未完成的指令（按创建时间排序）。
func (s *Service) PendingCommands(ctx context.Context, machineID int64) ([]Command, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+commandCols+` FROM agent_commands
		WHERE machine_id = $1 AND status IN ('pending', 'sent') ORDER BY created_at, id`, machineID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Command, error) { return scanCommand(r) })
}

// CompleteCommand 记录指令结果。applied 为 false 表示该指令此前已完成或已取消，调用方应忽略本次回报。
func (s *Service) CompleteCommand(ctx context.Context, q db.Querier, id string, ok bool, result string) (Command, bool, error) {
	status := "failed"
	if ok {
		status = "done"
	}
	cmd, err := scanCommand(q.QueryRow(ctx, `UPDATE agent_commands SET status = $2, result = $3, updated_at = $4
		WHERE id = $1 AND status IN ('pending', 'sent') RETURNING `+commandCols, id, status, result, s.clock.Now()))
	if err == nil {
		return cmd, true, nil
	}
	if !errors.Is(err, apperr.ErrNotFound) {
		return Command{}, false, err
	}
	cmd, err = scanCommand(q.QueryRow(ctx, `SELECT `+commandCols+` FROM agent_commands WHERE id = $1`, id))
	if err != nil {
		return Command{}, false, err
	}
	return cmd, false, nil
}

// CancelCommands 把某实例所有未完成的指令标记为失败（被新指令取代时使用）。
func (s *Service) CancelCommands(ctx context.Context, q db.Querier, instanceID, reason string) error {
	_, err := q.Exec(ctx, `UPDATE agent_commands SET status = 'failed', result = $2, updated_at = $3
		WHERE instance_id = $1 AND status IN ('pending', 'sent')`, instanceID, "cancelled: "+reason, s.clock.Now())
	return err
}
```

`internal/fleet/alerts.go`:

```go
package fleet

import (
	"context"

	"spark-cloud/internal/platform/db"
)

type Alert struct {
	Level      string // warning / critical
	Kind       string
	MachineID  int64 // 0 表示与具体机器无关
	InstanceID string
	Message    string
}

func (s *Service) RaiseAlert(ctx context.Context, q db.Querier, a Alert) error {
	var machineID *int64
	if a.MachineID != 0 {
		machineID = &a.MachineID
	}
	_, err := q.Exec(ctx, `INSERT INTO alerts (level, kind, machine_id, instance_id, message, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, a.Level, a.Kind, machineID, a.InstanceID, a.Message, s.clock.Now())
	return err
}
```

- [ ] **Step 7: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/fleet/`
Expected: PASS

- [ ] **Step 8: 提交**

```bash
git add -A
git commit -m "feat(fleet): machine inventory, skip-locked allocation, heartbeats, command queue, alerts

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: 下单与实例查询（rental 第 1 部分）

**Files:**
- Create: `migrations/00007_rental.sql`
- Create: `internal/rental/service.go`, `internal/rental/orders.go`, `internal/rental/place.go`, `internal/rental/queries.go`, `internal/rental/leases.go`
- Create: `internal/rental/helpers_test.go`, `internal/rental/place_test.go`

**Interfaces:**
- Consumes: `identity.Service.CanRent`（通过 `rental.UserChecker` 接口）、`wallet.Service.Apply`、`catalog.Service.Quote/GetImage`、`fleet.Service.Allocate/Enqueue`、`secretbox.Box`、`randx`
- Produces:
  - 常量 `rental.StateCreating StateRunning StateStopped StateWiping StateReleased StateFailed`；`rental.CreateTimeout = 10*time.Minute`，`RetainAfterStop = 6*time.Hour`，`OfflineTimeout = 5*time.Minute`
  - `rental.UserChecker` 接口 `CanRent(ctx, userID int64) error`
  - `rental.Notifier` 接口 `NotifyExpiry(ctx, userID int64, instanceID string, remaining time.Duration) error`
  - `rental.Deps{Pool; Clock; Box; Log *slog.Logger; Users UserChecker; Wallet *wallet.Service; Catalog *catalog.Service; Fleet *fleet.Service; Notifier Notifier}`，`rental.NewService(d Deps) *Service`
  - `rental.Instance{ID string; UserID, MachineID, ProductID, ImageID int64; State string; PendingSeconds int64; ExpiresAt, StoppedAt, SuspendedAt *time.Time; PausedRemaining *int64; Reminded24h, Reminded1h bool; CreatedAt time.Time}`；`(Instance).Suspended() bool`；`(Instance).Remaining(now time.Time) time.Duration`
  - `rental.Order{ID string; UserID int64; Kind, InstanceID string; ProductID int64; Duration catalog.Duration; Amount int64; Status string; CreatedAt time.Time}`
  - `rental.PlaceRequest{UserID, ProductID, ImageID int64; Duration catalog.Duration}`
  - `(*Service).Place(ctx, req PlaceRequest) (Order, Instance, error)`
  - `(*Service).Get(ctx, id string) (Instance, error)`；`GetForUser(ctx, userID int64, id string) (Instance, error)`；`ListForUser(ctx, userID int64) ([]Instance, error)`；`Password(ctx, userID int64, id string) (string, error)`；`ListOrders(ctx, userID int64, limit int) ([]Order, error)`
  - `rental.WipeBuffer = time.Hour`；包内 `occupiedUntil(inst Instance, now time.Time) time.Time`，`openLease(ctx, tx, inst Instance, now time.Time) error`，`refreshLease(ctx, tx, id string, now time.Time) error`（每次状态变化后调用；实例 released/failed 时关闭时段）
  - 包内辅助（后续任务使用）：`lockInstance(ctx, tx, id) (Instance, error)`，`recordEvent(ctx, q, id, from, to, reason string, at time.Time) error`，`reminderFlags(remaining time.Duration) (r24, r1 bool)`，`(*Service).charge(ctx, tx, userID int64, kind, instanceID string, productID int64, d catalog.Duration, now time.Time) (Order, error)`，`seconds(d catalog.Duration) int64`

- [ ] **Step 1: 写迁移**

`migrations/00007_rental.sql`:

```sql
-- +goose Up
CREATE TABLE instances (
    id               TEXT PRIMARY KEY,
    user_id          BIGINT NOT NULL REFERENCES users (id),
    machine_id       BIGINT NOT NULL REFERENCES machines (id),
    product_id       BIGINT NOT NULL REFERENCES products (id),
    image_id         BIGINT NOT NULL REFERENCES images (id),
    state            TEXT NOT NULL
                     CHECK (state IN ('creating', 'running', 'stopped', 'wiping', 'released', 'failed')),
    pending_seconds  BIGINT NOT NULL DEFAULT 0,  -- 已付款但尚未开始计时的秒数
    expires_at       TIMESTAMPTZ,
    stopped_at       TIMESTAMPTZ,
    suspended_at     TIMESTAMPTZ,                -- 机器失联、暂停计时的开始时间
    paused_remaining BIGINT,                     -- 暂停时冻结的剩余秒数
    ssh_password_enc BYTEA NOT NULL,
    reminded_24h     BOOLEAN NOT NULL DEFAULT false,
    reminded_1h      BOOLEAN NOT NULL DEFAULT false,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX instances_user_idx ON instances (user_id, created_at DESC);
CREATE INDEX instances_state_idx ON instances (state, expires_at);
-- 一台机器同一时间只能被一个未结束的实例占用
CREATE UNIQUE INDEX instances_machine_active_uniq ON instances (machine_id)
    WHERE state IN ('creating', 'running', 'stopped', 'wiping');

CREATE TABLE instance_events (
    id          BIGSERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instances (id),
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    at          TIMESTAMPTZ NOT NULL
);
CREATE INDEX instance_events_instance_idx ON instance_events (instance_id, id);

CREATE TABLE orders (
    id          TEXT PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users (id),
    kind        TEXT NOT NULL CHECK (kind IN ('new', 'renew')),
    instance_id TEXT NOT NULL REFERENCES instances (id),
    product_id  BIGINT NOT NULL REFERENCES products (id),
    duration    TEXT NOT NULL CHECK (duration IN ('1d', '1w', '1m')),
    amount      BIGINT NOT NULL CHECK (amount > 0),
    status      TEXT NOT NULL CHECK (status IN ('paid', 'refunded', 'partially_refunded')),
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX orders_user_idx ON orders (user_id, created_at DESC);
CREATE INDEX orders_instance_idx ON orders (instance_id);

-- 机器占用时段：同一台机器的 reserved/active 时段不得重叠（子项目 2 的档期预订基于此表）
CREATE EXTENSION IF NOT EXISTS btree_gist;
CREATE TABLE machine_leases (
    id          BIGSERIAL PRIMARY KEY,
    machine_id  BIGINT NOT NULL REFERENCES machines (id),
    instance_id TEXT NOT NULL REFERENCES instances (id),
    period      TSTZRANGE NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('reserved', 'active', 'ended', 'cancelled')),
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    EXCLUDE USING gist (machine_id WITH =, period WITH &&) WHERE (status IN ('reserved', 'active'))
);
CREATE INDEX machine_leases_instance_idx ON machine_leases (instance_id) WHERE status = 'active';

-- +goose Down
DROP TABLE machine_leases;
DROP TABLE orders;
DROP TABLE instance_events;
DROP TABLE instances;
```

- [ ] **Step 2: 写测试辅助**

`internal/rental/helpers_test.go`:

```go
package rental_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

const startFunds = 1_000_000

type fakeNotifier struct {
	mu    sync.Mutex
	calls []string
}

func (n *fakeNotifier) NotifyExpiry(_ context.Context, _ int64, instanceID string, remaining time.Duration) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, fmt.Sprintf("%s:%s", instanceID, remaining.Round(time.Minute)))
	return nil
}

func (n *fakeNotifier) Calls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.calls...)
}

type env struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	clock     *clock.Fake
	svc       *rental.Service
	wallet    *wallet.Service
	fleet     *fleet.Service
	notifier  *fakeNotifier
	userID    int64
	productID int64
	imageID   int64
	machines  []int64
}

func newEnv(t *testing.T, machines int) *env {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: &sms.Mock{}, KYC: kyc.Mock{}, Box: box})
	w := wallet.NewService(pool, clk)
	f := fleet.NewService(pool, clk)
	n := &fakeNotifier{}
	svc := rental.NewService(rental.Deps{
		Pool: pool, Clock: clk, Box: box, Users: ids, Wallet: w,
		Catalog: catalog.NewService(pool), Fleet: f, Notifier: n,
	})
	e := &env{t: t, ctx: context.Background(), pool: pool, clock: clk, svc: svc, wallet: w, fleet: f, notifier: n,
		productID: dbtest.ProductID(t, pool), imageID: dbtest.ImageID(t, pool)}
	e.userID = e.newUser("13600000001", startFunds)
	for i := 0; i < machines; i++ {
		e.machines = append(e.machines, dbtest.CreateMachine(t, pool, fmt.Sprintf("SN%03d", i)))
	}
	return e
}

// newUser 创建一个已实名并充值的用户。
func (e *env) newUser(phone string, funds int64) int64 {
	uid := dbtest.CreateUser(e.t, e.pool, phone, true)
	if funds > 0 {
		dbtest.Fund(e.t, e.pool, uid, funds)
	}
	return uid
}

func (e *env) place(d catalog.Duration) (rental.Order, rental.Instance) {
	e.t.Helper()
	ord, inst, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: d})
	if err != nil {
		e.t.Fatalf("Place: %v", err)
	}
	return ord, inst
}

// ack 模拟 Agent：把所有机器上未完成的指令按 ok 回报，返回处理过的指令。
func (e *env) ack(ok bool) []fleet.Command {
	e.t.Helper()
	var done []fleet.Command
	for _, mid := range e.machines {
		cmds, err := e.fleet.PendingCommands(e.ctx, mid)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, c := range cmds {
			if err := e.svc.HandleCommandResult(e.ctx, c.ID, ok, "test"); err != nil {
				e.t.Fatalf("HandleCommandResult(%s): %v", c.Type, err)
			}
			done = append(done, c)
		}
	}
	return done
}

func (e *env) get(id string) rental.Instance {
	e.t.Helper()
	inst, err := e.svc.Get(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return inst
}

func (e *env) balance(uid int64) int64 {
	e.t.Helper()
	b, err := e.wallet.Balance(e.ctx, uid)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) machineState(id int64) string {
	e.t.Helper()
	m, err := e.fleet.Get(e.ctx, e.pool, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return m.State
}

// lease 返回实例最近一条占用时段的上下界与状态。
func (e *env) lease(instanceID string) (time.Time, time.Time, string) {
	e.t.Helper()
	var lower, upper time.Time
	var status string
	err := e.pool.QueryRow(e.ctx, `SELECT lower(period), upper(period), status FROM machine_leases
		WHERE instance_id = $1 ORDER BY id DESC LIMIT 1`, instanceID).Scan(&lower, &upper, &status)
	if err != nil {
		e.t.Fatalf("lease: %v", err)
	}
	return lower, upper, status
}

// heartbeatAll 让所有机器发送一次心跳（避免快进时间后被判离线）。
func (e *env) heartbeatAll() {
	e.t.Helper()
	for _, mid := range e.machines {
		if _, err := e.fleet.Heartbeat(e.ctx, mid); err != nil {
			e.t.Fatal(err)
		}
	}
}
```

注意：`HandleCommandResult` 在 Task 8 实现。为让本任务能编译，Step 5 会先放一个返回 `errors.New("not implemented")` 的占位，Task 8 第一步替换它。本任务的测试不调用 `ack`。

- [ ] **Step 3: 写下单的失败测试**

`internal/rental/place_test.go`:

```go
package rental_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func TestPlaceAllocatesChargesAndEnqueuesCreate(t *testing.T) {
	e := newEnv(t, 1)
	ord, inst := e.place(catalog.D1d)

	if ord.Amount != 6900 || ord.Kind != "new" || ord.Status != "paid" || ord.InstanceID != inst.ID {
		t.Fatalf("order = %+v", ord)
	}
	if inst.State != rental.StateCreating || inst.PendingSeconds != 86400 || inst.ExpiresAt != nil {
		t.Fatalf("instance = %+v", inst)
	}
	if got := e.balance(e.userID); got != startFunds-6900 {
		t.Fatalf("balance = %d", got)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateAllocated {
		t.Fatalf("machine state = %s", got)
	}
	cmds, _ := e.fleet.PendingCommands(e.ctx, e.machines[0])
	if len(cmds) != 1 || cmds[0].Type != fleet.CmdCreateInstance || cmds[0].InstanceID != inst.ID {
		t.Fatalf("commands = %+v", cmds)
	}
	var p fleet.CreateInstancePayload
	_ = json.Unmarshal(cmds[0].Payload, &p)
	if p.ImageRef != "registry.spark.internal/base/cuda:ubuntu24.04" {
		t.Fatalf("payload = %+v", p)
	}
	orders, _ := e.svc.ListOrders(e.ctx, e.userID, 10)
	if len(orders) != 1 || orders[0].ID != ord.ID {
		t.Fatalf("orders = %+v", orders)
	}
	pw, err := e.svc.Password(e.ctx, e.userID, inst.ID)
	if err != nil || len(pw) != 16 {
		t.Fatalf("password = %q, %v", pw, err)
	}
	// 占用时段：下单时刻起，至「开通超时 + 租期 + 6 小时保留 + 1 小时洗机」
	lower, upper, status := e.lease(inst.ID)
	wantUpper := dbtest.T0.Add(rental.CreateTimeout + 24*time.Hour + rental.RetainAfterStop + rental.WipeBuffer)
	if !lower.Equal(dbtest.T0) || !upper.Equal(wantUpper) || status != "active" {
		t.Fatalf("lease = [%v, %v) %s, want [%v, %v) active", lower, upper, status, dbtest.T0, wantUpper)
	}
}

func TestPlaceRequiresKYC(t *testing.T) {
	e := newEnv(t, 1)
	uid := dbtest.CreateUser(t, e.pool, "13600000099", false)
	dbtest.Fund(t, e.pool, uid, startFunds)
	_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: uid, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
	if !errors.Is(err, apperr.ErrKYCRequired) {
		t.Fatalf("want kyc required, got %v", err)
	}
}

func TestPlaceInsufficientBalanceKeepsMachineIdle(t *testing.T) {
	e := newEnv(t, 1)
	poor := e.newUser("13600000002", 100)
	_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: poor, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
	if !errors.Is(err, apperr.ErrInsufficientBalance) {
		t.Fatalf("want insufficient balance, got %v", err)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateIdle {
		t.Fatalf("machine state = %s, want idle", got)
	}
	if list, _ := e.svc.ListForUser(e.ctx, poor); len(list) != 0 {
		t.Fatalf("instances = %+v", list)
	}
	if got := e.balance(poor); got != 100 {
		t.Fatalf("balance = %d", got)
	}
}

func TestPlaceNoCapacityDoesNotCharge(t *testing.T) {
	e := newEnv(t, 1)
	e.place(catalog.D1d)
	_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
	if !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("want no capacity, got %v", err)
	}
	if got := e.balance(e.userID); got != startFunds-6900 {
		t.Fatalf("balance = %d, want charged once", got)
	}
}

func TestPlaceRejectsInvalidDurationAndImage(t *testing.T) {
	e := newEnv(t, 1)
	_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: "2h"})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("invalid duration want bad_request, got %v", err)
	}
	_, _, err = e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: 99999, Duration: catalog.D1d})
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("invalid image want bad_request, got %v", err)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateIdle {
		t.Fatalf("machine state = %s, want idle", got)
	}
}

func TestConcurrentPlaceForLastMachines(t *testing.T) {
	e := newEnv(t, 2)
	users := make([]int64, 6)
	for i := range users {
		users[i] = e.newUser(fmt.Sprintf("1350000000%d", i), startFunds)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := map[int64]bool{}
	for _, uid := range users {
		wg.Add(1)
		go func(uid int64) {
			defer wg.Done()
			_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: uid, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won[uid] = true
			} else if !errors.Is(err, apperr.ErrNoCapacity) {
				t.Errorf("unexpected error: %v", err)
			}
		}(uid)
	}
	wg.Wait()
	if len(won) != 2 {
		t.Fatalf("winners = %d, want 2", len(won))
	}
	for _, uid := range users {
		want := int64(startFunds)
		if won[uid] {
			want -= 6900
		}
		if got := e.balance(uid); got != want {
			t.Fatalf("user %d balance = %d, want %d", uid, got, want)
		}
	}
}

func TestInstancesAreIsolatedPerUser(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	other := e.newUser("13600000003", 0)
	if _, err := e.svc.GetForUser(e.ctx, other, inst.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("GetForUser other = %v", err)
	}
	if _, err := e.svc.Password(e.ctx, other, inst.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("Password other = %v", err)
	}
	if list, _ := e.svc.ListForUser(e.ctx, e.userID); len(list) != 1 || list[0].ID != inst.ID {
		t.Fatalf("ListForUser = %+v", list)
	}
}
```

- [ ] **Step 4: 运行测试确认失败**

Run: `go test ./internal/rental/`
Expected: FAIL（`rental` 包不存在）

- [ ] **Step 5: 实现 rental 第 1 部分**

`internal/rental/service.go`:

```go
// Package rental 管理订单与实例生命周期：下单、续费、释放、Agent 回报处理和到期清扫。
package rental

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/wallet"
)

const (
	StateCreating = "creating"
	StateRunning  = "running"
	StateStopped  = "stopped"
	StateWiping   = "wiping"
	StateReleased = "released"
	StateFailed   = "failed"

	CreateTimeout   = 10 * time.Minute // 开通超时，超时判失败并退款
	RetainAfterStop = 6 * time.Hour    // 到期停机后保留数据的时长
	OfflineTimeout  = 5 * time.Minute  // Agent 失联判定
)

type UserChecker interface {
	CanRent(ctx context.Context, userID int64) error
}

type Notifier interface {
	NotifyExpiry(ctx context.Context, userID int64, instanceID string, remaining time.Duration) error
}

type Deps struct {
	Pool     *pgxpool.Pool
	Clock    clock.Clock
	Box      *secretbox.Box
	Log      *slog.Logger
	Users    UserChecker
	Wallet   *wallet.Service
	Catalog  *catalog.Service
	Fleet    *fleet.Service
	Notifier Notifier
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

type Instance struct {
	ID              string
	UserID          int64
	MachineID       int64
	ProductID       int64
	ImageID         int64
	State           string
	PendingSeconds  int64
	ExpiresAt       *time.Time
	StoppedAt       *time.Time
	SuspendedAt     *time.Time
	PausedRemaining *int64
	Reminded24h     bool
	Reminded1h      bool
	CreatedAt       time.Time
}

func (i Instance) Suspended() bool { return i.SuspendedAt != nil }

// Remaining 返回剩余可用时长；暂停中返回冻结的剩余时长；非运行状态返回 0。
func (i Instance) Remaining(now time.Time) time.Duration {
	if i.State != StateRunning {
		return 0
	}
	if i.SuspendedAt != nil && i.PausedRemaining != nil {
		return time.Duration(*i.PausedRemaining) * time.Second
	}
	if i.ExpiresAt == nil {
		return 0
	}
	if r := i.ExpiresAt.Sub(now); r > 0 {
		return r
	}
	return 0
}

const instanceCols = `id, user_id, machine_id, product_id, image_id, state, pending_seconds, expires_at,
	stopped_at, suspended_at, paused_remaining, reminded_24h, reminded_1h, created_at`

func scanInstance(row pgx.Row) (Instance, error) {
	var i Instance
	err := row.Scan(&i.ID, &i.UserID, &i.MachineID, &i.ProductID, &i.ImageID, &i.State, &i.PendingSeconds, &i.ExpiresAt,
		&i.StoppedAt, &i.SuspendedAt, &i.PausedRemaining, &i.Reminded24h, &i.Reminded1h, &i.CreatedAt)
	if db.IsNoRows(err) {
		return Instance{}, apperr.ErrNotFound
	}
	return i, err
}

func collectInstances(rows pgx.Rows, err error) ([]Instance, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Instance, error) { return scanInstance(r) })
}

func lockInstance(ctx context.Context, tx pgx.Tx, id string) (Instance, error) {
	return scanInstance(tx.QueryRow(ctx, `SELECT `+instanceCols+` FROM instances WHERE id = $1 FOR UPDATE`, id))
}

func recordEvent(ctx context.Context, q db.Querier, id, from, to, reason string, at time.Time) error {
	_, err := q.Exec(ctx, `INSERT INTO instance_events (instance_id, from_state, to_state, reason, at) VALUES ($1, $2, $3, $4, $5)`,
		id, from, to, reason, at)
	return err
}

// reminderFlags 计算到期提醒标记：剩余时长已不超过阈值的提醒视为「已发送」，避免短租期一开通就收到提醒。
func reminderFlags(remaining time.Duration) (r24, r1 bool) {
	return remaining <= 24*time.Hour, remaining <= time.Hour
}

func seconds(d catalog.Duration) int64 { return int64(d.Length() / time.Second) }
```

`internal/rental/orders.go`:

```go
package rental

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/wallet"
)

type Order struct {
	ID         string           `json:"id"`
	UserID     int64            `json:"-"`
	Kind       string           `json:"kind"`
	InstanceID string           `json:"instance_id"`
	ProductID  int64            `json:"product_id"`
	Duration   catalog.Duration `json:"duration"`
	Amount     int64            `json:"amount"`
	Status     string           `json:"status"`
	CreatedAt  time.Time        `json:"created_at"`
}

const orderCols = `id, user_id, kind, instance_id, product_id, duration, amount, status, created_at`

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.UserID, &o.Kind, &o.InstanceID, &o.ProductID, &o.Duration, &o.Amount, &o.Status, &o.CreatedAt)
	if db.IsNoRows(err) {
		return Order{}, apperr.ErrNotFound
	}
	return o, err
}

// charge 在事务内按当前价格扣款并创建订单。实例行必须已存在（orders.instance_id 外键）。
func (s *Service) charge(ctx context.Context, tx pgx.Tx, userID int64, kind, instanceID string, productID int64, d catalog.Duration, now time.Time) (Order, error) {
	amount, err := s.d.Catalog.Quote(ctx, tx, productID, d)
	if err != nil {
		return Order{}, err
	}
	id := randx.ID("ord-", 12)
	remark := "新购 " + instanceID
	if kind == "renew" {
		remark = "续费 " + instanceID
	}
	if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{
		UserID: userID, Amount: -amount, Kind: wallet.KindOrderPay, RefType: "order", RefID: id, Remark: remark,
	}); err != nil {
		return Order{}, err
	}
	return scanOrder(tx.QueryRow(ctx, `INSERT INTO orders (id, user_id, kind, instance_id, product_id, duration, amount, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'paid', $8, $8) RETURNING `+orderCols,
		id, userID, kind, instanceID, productID, string(d), amount, now))
}

func (s *Service) ListOrders(ctx context.Context, userID int64, limit int) ([]Order, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE user_id = $1 ORDER BY created_at DESC, id LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
}
```

`internal/rental/place.go`:

```go
package rental

import (
	"context"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

type PlaceRequest struct {
	UserID    int64
	ProductID int64
	ImageID   int64
	Duration  catalog.Duration
}

// Place 新购一台整机：同一事务内分配机器、创建实例、扣款建单、写入开通指令。
// 租期从实例进入 running 时开始计时，在此之前记为 pending_seconds。
func (s *Service) Place(ctx context.Context, req PlaceRequest) (Order, Instance, error) {
	if !req.Duration.Valid() {
		return Order{}, Instance{}, apperr.BadRequest("租期不正确")
	}
	if err := s.d.Users.CanRent(ctx, req.UserID); err != nil {
		return Order{}, Instance{}, err
	}
	now := s.d.Clock.Now()
	var ord Order
	var inst Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		img, err := s.d.Catalog.GetImage(ctx, tx, req.ImageID)
		if err != nil {
			return err
		}
		m, err := s.d.Fleet.Allocate(ctx, tx, req.ProductID)
		if err != nil {
			return err
		}
		id := randx.ID("ins-", 8)
		inst, err = scanInstance(tx.QueryRow(ctx, `INSERT INTO instances
			(id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'creating', $6, $7, $8, $8) RETURNING `+instanceCols,
			id, req.UserID, m.ID, req.ProductID, img.ID, seconds(req.Duration), s.d.Box.Seal([]byte(randx.Password(16))), now))
		if err != nil {
			return err
		}
		if err := openLease(ctx, tx, inst, now); err != nil {
			return err
		}
		if ord, err = s.charge(ctx, tx, req.UserID, "new", id, req.ProductID, req.Duration, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, id, "", StateCreating, "order "+ord.ID, now); err != nil {
			return err
		}
		_, err = s.d.Fleet.Enqueue(ctx, tx, m.ID, id, fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: img.Ref})
		return err
	})
	if err != nil {
		return Order{}, Instance{}, err
	}
	return ord, inst, nil
}
```

`internal/rental/leases.go`:

```go
package rental

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// WipeBuffer 是预留给洗机的时长，计入机器占用时段。
const WipeBuffer = time.Hour

// occupiedUntil 估算实例对机器的占用结束时间（machine_leases 的上界）。
// 子项目 2 的档期预订会据此判断机器何时空闲。
func occupiedUntil(inst Instance, now time.Time) time.Time {
	tail := RetainAfterStop + WipeBuffer
	pending := time.Duration(inst.PendingSeconds) * time.Second
	switch inst.State {
	case StateCreating:
		return now.Add(CreateTimeout + pending + tail)
	case StateRunning:
		if inst.Suspended() && inst.PausedRemaining != nil {
			return now.Add(time.Duration(*inst.PausedRemaining)*time.Second + tail)
		}
		if inst.ExpiresAt != nil {
			return inst.ExpiresAt.Add(tail)
		}
	case StateStopped:
		if pending > 0 {
			return now.Add(CreateTimeout + pending + tail)
		}
		if inst.StoppedAt != nil {
			return inst.StoppedAt.Add(tail)
		}
	case StateWiping:
		return now.Add(WipeBuffer)
	}
	return now
}

// openLease 为新实例写入一条 active 占用时段；与已有时段重叠视为无可用机器。
func openLease(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'active', $3, $3)`, inst.MachineID, inst.ID, now, occupiedUntil(inst, now))
	if db.IsExclusionViolation(err) {
		return apperr.ErrNoCapacity
	}
	return err
}

// refreshLease 在实例状态变化后同步占用时段：结束的实例关闭时段，其余按 occupiedUntil 更新上界。
func (s *Service) refreshLease(ctx context.Context, tx pgx.Tx, id string, now time.Time) error {
	inst, err := lockInstance(ctx, tx, id)
	if err != nil {
		return err
	}
	end, status := occupiedUntil(inst, now), "active"
	if inst.State == StateReleased || inst.State == StateFailed {
		end, status = now, "ended"
	}
	_, err = tx.Exec(ctx, `UPDATE machine_leases
		SET period = tstzrange(lower(period), GREATEST(lower(period), $2::timestamptz)), status = $3, updated_at = $4
		WHERE instance_id = $1 AND status = 'active'`, id, end, status, now)
	if db.IsExclusionViolation(err) {
		return apperr.ErrNoCapacity
	}
	return err
}
```

`internal/rental/queries.go`:

```go
package rental

import (
	"context"
	"errors"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

func (s *Service) Get(ctx context.Context, id string) (Instance, error) {
	return scanInstance(s.d.Pool.QueryRow(ctx, `SELECT `+instanceCols+` FROM instances WHERE id = $1`, id))
}

func (s *Service) GetForUser(ctx context.Context, userID int64, id string) (Instance, error) {
	return scanInstance(s.d.Pool.QueryRow(ctx, `SELECT `+instanceCols+` FROM instances WHERE id = $1 AND user_id = $2`, id, userID))
}

func (s *Service) ListForUser(ctx context.Context, userID int64) ([]Instance, error) {
	return collectInstances(s.d.Pool.Query(ctx, `SELECT `+instanceCols+` FROM instances
		WHERE user_id = $1 ORDER BY created_at DESC, id LIMIT 100`, userID))
}

// Password 解密返回实例的 SSH 密码。
func (s *Service) Password(ctx context.Context, userID int64, id string) (string, error) {
	var enc []byte
	if err := s.d.Pool.QueryRow(ctx, `SELECT ssh_password_enc FROM instances WHERE id = $1 AND user_id = $2`, id, userID).Scan(&enc); err != nil {
		if db.IsNoRows(err) {
			return "", apperr.ErrNotFound
		}
		return "", err
	}
	plain, err := s.d.Box.Open(enc)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// HandleCommandResult 在 Task 8 实现。
func (s *Service) HandleCommandResult(ctx context.Context, commandID string, ok bool, result string) error {
	return errors.New("not implemented")
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add -A
git commit -m "feat(rental): place orders atomically with allocation, charge and create command

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Agent 回报处理与开通失败退款（rental 第 2 部分）

**Files:**
- Create: `internal/rental/results.go`, `internal/rental/results_test.go`
- Modify: `internal/rental/queries.go`（删除 `HandleCommandResult` 占位）

**Interfaces:**
- Consumes: Task 6 `fleet.CompleteCommand/Release/MarkFaulty/CancelCommands/RaiseAlert`，Task 4 `wallet.Apply`，Task 7 包内辅助
- Produces:
  - `(*Service).HandleCommandResult(ctx, commandID string, ok bool, result string) error`：
    - create 成功（creating）→ running，`expires_at = now + pending_seconds`
    - create 失败（creating）→ failed，全额退款，机器标记故障，告警
    - start 成功（stopped）→ running，`expires_at = now + pending_seconds`；失败 → 告警
    - stop 失败 → 告警
    - wipe 成功（wiping）→ released，机器回空闲；失败 → 机器故障，告警
    - 重复或已取消的指令回报一律忽略
  - 包内：`(*Service).startLease(ctx, tx, inst Instance, from, reason string, now time.Time) error`，`(*Service).failCreating(ctx, tx, inst Instance, reason string, now time.Time) error`，`(*Service).refundAll(ctx, tx, inst Instance, now time.Time) error`

- [ ] **Step 1: 删除占位**

从 `internal/rental/queries.go` 删除 `HandleCommandResult` 占位函数，并删除不再使用的 `"errors"` import。

- [ ] **Step 2: 写失败测试**

`internal/rental/results_test.go`:

```go
package rental_test

import (
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/rental"
)

func TestCreateSuccessStartsLeaseAtRunningTime(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.clock.Advance(2 * time.Minute) // 开机耗时不计入租期
	e.ack(true)

	got := e.get(inst.ID)
	want := e.clock.Now().Add(24 * time.Hour)
	if got.State != rental.StateRunning || got.PendingSeconds != 0 || got.ExpiresAt == nil || !got.ExpiresAt.Equal(want) {
		t.Fatalf("instance = %+v, want running until %v", got, want)
	}
	if got.Remaining(e.clock.Now()) != 24*time.Hour {
		t.Fatalf("remaining = %v", got.Remaining(e.clock.Now()))
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(want.Add(rental.RetainAfterStop + rental.WipeBuffer)) {
		t.Fatalf("lease upper = %v, want %v", upper, want.Add(rental.RetainAfterStop+rental.WipeBuffer))
	}
}

func TestDuplicateAckIsIgnored(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	cmds := e.ack(true)
	if err := e.svc.HandleCommandResult(e.ctx, cmds[0].ID, false, "late failure"); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.State != rental.StateRunning {
		t.Fatalf("state = %s, want running", got.State)
	}
	if got := e.balance(e.userID); got != startFunds-6900 {
		t.Fatalf("balance = %d, no refund expected", got)
	}
}

func TestCreateFailureRefundsAndQuarantinesMachine(t *testing.T) {
	e := newEnv(t, 1)
	ord, inst := e.place(catalog.D1d)
	e.ack(false)

	if got := e.get(inst.ID); got.State != rental.StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	if got := e.balance(e.userID); got != startFunds {
		t.Fatalf("balance = %d, want full refund", got)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateFaulty {
		t.Fatalf("machine = %s, want faulty", got)
	}
	if _, _, status := e.lease(inst.ID); status != "ended" {
		t.Fatalf("lease status = %s, want ended", status)
	}
	orders, _ := e.svc.ListOrders(e.ctx, e.userID, 10)
	if len(orders) != 1 || orders[0].ID != ord.ID || orders[0].Status != "refunded" {
		t.Fatalf("orders = %+v", orders)
	}
	var alerts int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = 'create_failed'`).Scan(&alerts)
	if alerts != 1 {
		t.Fatalf("alerts = %d", alerts)
	}
}

func TestUnknownCommandID(t *testing.T) {
	e := newEnv(t, 1)
	if err := e.svc.HandleCommandResult(e.ctx, "00000000-0000-0000-0000-000000000000", true, ""); err == nil {
		t.Fatal("unknown command must return error")
	}
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'Create|Ack|Unknown'`
Expected: FAIL（`HandleCommandResult` 未定义，或占位返回 not implemented）

- [ ] **Step 4: 实现**

`internal/rental/results.go`:

```go
package rental

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

// HandleCommandResult 处理 Agent 对一条指令的回报。重复回报、已取消指令的回报、与当前状态不符的回报都会被忽略。
func (s *Service) HandleCommandResult(ctx context.Context, commandID string, ok bool, result string) error {
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		cmd, applied, err := s.d.Fleet.CompleteCommand(ctx, tx, commandID, ok, result)
		if err != nil {
			return err
		}
		if !applied || cmd.InstanceID == "" {
			return nil
		}
		inst, err := lockInstance(ctx, tx, cmd.InstanceID)
		if err != nil {
			return err
		}
		now := s.d.Clock.Now()
		switch cmd.Type {
		case fleet.CmdCreateInstance:
			if inst.State != StateCreating {
				return nil
			}
			if ok {
				return s.startLease(ctx, tx, inst, StateCreating, "agent: created", now)
			}
			return s.failCreating(ctx, tx, inst, "开通失败: "+result, now)
		case fleet.CmdStartInstance:
			if inst.State != StateStopped {
				return nil
			}
			if ok {
				return s.startLease(ctx, tx, inst, StateStopped, "agent: started", now)
			}
			return s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "start_failed",
				MachineID: inst.MachineID, InstanceID: inst.ID, Message: result})
		case fleet.CmdStopInstance:
			if !ok {
				return s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "stop_failed",
					MachineID: inst.MachineID, InstanceID: inst.ID, Message: result})
			}
		case fleet.CmdWipeInstance:
			if inst.State != StateWiping {
				return nil
			}
			if !ok {
				if err := s.d.Fleet.MarkFaulty(ctx, tx, inst.MachineID); err != nil {
					return err
				}
				return s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "wipe_failed",
					MachineID: inst.MachineID, InstanceID: inst.ID, Message: result})
			}
			if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'released', updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
				return err
			}
			if err := recordEvent(ctx, tx, inst.ID, StateWiping, StateReleased, "agent: wiped", now); err != nil {
				return err
			}
			if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
				return err
			}
			return s.d.Fleet.Release(ctx, tx, inst.MachineID)
		}
		return nil
	})
}

// startLease 把实例转为 running，并从此刻开始计时 pending_seconds。
func (s *Service) startLease(ctx context.Context, tx pgx.Tx, inst Instance, from, reason string, now time.Time) error {
	expires := now.Add(time.Duration(inst.PendingSeconds) * time.Second)
	r24, r1 := reminderFlags(expires.Sub(now))
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'running', expires_at = $2, pending_seconds = 0,
			stopped_at = NULL, reminded_24h = $3, reminded_1h = $4, updated_at = $5 WHERE id = $1`,
		inst.ID, expires, r24, r1, now); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, inst.ID, from, StateRunning, reason, now); err != nil {
		return err
	}
	return s.refreshLease(ctx, tx, inst.ID, now)
}

// failCreating 开通失败：实例失败、全额退款、机器隔离为故障、取消剩余指令并告警。
func (s *Service) failCreating(ctx context.Context, tx pgx.Tx, inst Instance, reason string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'failed', pending_seconds = 0, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, inst.ID, StateCreating, StateFailed, reason, now); err != nil {
		return err
	}
	if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
		return err
	}
	if err := s.refundAll(ctx, tx, inst, now); err != nil {
		return err
	}
	if err := s.d.Fleet.MarkFaulty(ctx, tx, inst.MachineID); err != nil {
		return err
	}
	if err := s.d.Fleet.CancelCommands(ctx, tx, inst.ID, "instance failed"); err != nil {
		return err
	}
	return s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "create_failed",
		MachineID: inst.MachineID, InstanceID: inst.ID, Message: reason})
}

// refundAll 退还该实例所有已支付订单（新购与续费）。
func (s *Service) refundAll(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id, amount FROM orders WHERE instance_id = $1 AND status = 'paid' FOR UPDATE`, inst.ID)
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
		if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{
			UserID: inst.UserID, Amount: o.amount, Kind: wallet.KindRefund, RefType: "order", RefID: o.id,
			Remark: "开通失败退款 " + inst.ID,
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET status = 'refunded', updated_at = $2 WHERE id = $1`, o.id, now); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add -A
git commit -m "feat(rental): handle agent results, start lease on running, refund failed creates

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: 到期清扫器（停机、6 小时洗机、开通超时、失联暂停、到期提醒）

**Files:**
- Create: `internal/rental/sweeper.go`, `internal/rental/sweeper_test.go`

**Interfaces:**
- Consumes: Task 6 `fleet.MarkOfflineStale/Enqueue/CancelCommands/RaiseAlert`，Task 7/8 包内辅助（`collectInstances`、`recordEvent`、`refreshLease`、`failCreating`、`reminderFlags`）
- Produces:
  - `(*Service).ExpireDue(ctx) (int, error)`：running 且未暂停、`expires_at <= now` → stopped，下发 stop
  - `(*Service).WipeStale(ctx) (int, error)`：stopped、`pending_seconds = 0`、`stopped_at <= now-6h` → wiping，下发 wipe
  - `(*Service).FailStuckCreating(ctx) (int, error)`：creating 超过 10 分钟 → 失败退款
  - `(*Service).SuspendMachines(ctx, machineIDs []int64) (int, error)`：冻结这些机器上运行中实例的剩余时长
  - `(*Service).ResumeMachine(ctx, machineID int64) (int, error)`：恢复计时，`expires_at = now + paused_remaining`
  - `(*Service).SendReminders(ctx) (int, error)`：到期前 24 小时、1 小时各提醒一次
  - `(*Service).Sweep(ctx) error`：按顺序执行以上全部（含离线检测与告警）
  - `(*Service).RunSweeper(ctx, interval time.Duration)`：定时调用 Sweep，用 advisory lock 保证多副本只有一个在跑
  - 包内：`(*Service).beginWipe(ctx, tx, inst Instance, reason string, now time.Time) error`（Task 10 的 Release 复用）

- [ ] **Step 1: 写失败测试**

`internal/rental/sweeper_test.go`:

```go
package rental_test

import (
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/rental"
)

func countPending(e *env, machineID int64, t fleet.CommandType) int {
	e.t.Helper()
	cmds, err := e.fleet.PendingCommands(e.ctx, machineID)
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, c := range cmds {
		if c.Type == t {
			n++
		}
	}
	return n
}

func mustCount(t *testing.T, n int, err error, want int, what string) {
	t.Helper()
	if err != nil || n != want {
		t.Fatalf("%s = %d, %v; want %d", what, n, err, want)
	}
}

func TestExpireDueStopsExactlyAtExpiry(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)

	e.clock.Advance(24*time.Hour - time.Minute)
	n, err := e.svc.ExpireDue(e.ctx)
	mustCount(t, n, err, 0, "expired before due")

	e.clock.Advance(time.Minute)
	n, err = e.svc.ExpireDue(e.ctx)
	mustCount(t, n, err, 1, "expired at due")
	got := e.get(inst.ID)
	if got.State != rental.StateStopped || got.StoppedAt == nil || !got.StoppedAt.Equal(e.clock.Now()) {
		t.Fatalf("instance = %+v", got)
	}
	if countPending(e, e.machines[0], fleet.CmdStopInstance) != 1 {
		t.Fatal("stop command not enqueued")
	}
	n, err = e.svc.ExpireDue(e.ctx)
	mustCount(t, n, err, 0, "expire twice")
}

func TestWipeStaleAfterSixHoursThenRelease(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(24 * time.Hour)
	_, _ = e.svc.ExpireDue(e.ctx)

	e.clock.Advance(6*time.Hour - time.Minute)
	n, err := e.svc.WipeStale(e.ctx)
	mustCount(t, n, err, 0, "wiped before 6h")

	e.clock.Advance(time.Minute)
	n, err = e.svc.WipeStale(e.ctx)
	mustCount(t, n, err, 1, "wiped at 6h")
	if got := e.get(inst.ID); got.State != rental.StateWiping {
		t.Fatalf("state = %s", got.State)
	}
	// 未回报的 stop 指令被取消，只剩 wipe
	if countPending(e, e.machines[0], fleet.CmdStopInstance) != 0 || countPending(e, e.machines[0], fleet.CmdWipeInstance) != 1 {
		t.Fatal("expected only a wipe command pending")
	}
	e.ack(true)
	if got := e.get(inst.ID); got.State != rental.StateReleased {
		t.Fatalf("state = %s, want released", got.State)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateIdle {
		t.Fatalf("machine = %s, want idle", got)
	}
	if _, _, status := e.lease(inst.ID); status != "ended" {
		t.Fatalf("lease status = %s", status)
	}
}

func TestCreatingTimeoutRefundsAndIgnoresLateAck(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	cmds, _ := e.fleet.PendingCommands(e.ctx, e.machines[0])

	e.clock.Advance(rental.CreateTimeout - time.Second)
	n, err := e.svc.FailStuckCreating(e.ctx)
	mustCount(t, n, err, 0, "failed before timeout")

	e.clock.Advance(time.Second)
	n, err = e.svc.FailStuckCreating(e.ctx)
	mustCount(t, n, err, 1, "failed at timeout")
	if got := e.get(inst.ID); got.State != rental.StateFailed {
		t.Fatalf("state = %s", got.State)
	}
	if got := e.balance(e.userID); got != startFunds {
		t.Fatalf("balance = %d, want refund", got)
	}
	// Agent 迟到的成功回报不能复活实例，也不能重复退款
	if err := e.svc.HandleCommandResult(e.ctx, cmds[0].ID, true, "late"); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.State != rental.StateFailed {
		t.Fatalf("after late ack state = %s", got.State)
	}
	if got := e.balance(e.userID); got != startFunds {
		t.Fatalf("after late ack balance = %d", got)
	}
}

func TestOfflineMachinePausesLeaseAndResumes(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // 计时从 T0 开始，到期 T0+24h

	e.clock.Advance(time.Hour)
	e.heartbeatAll()
	e.clock.Advance(10 * time.Minute) // 10 分钟无心跳
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.get(inst.ID)
	frozen := 24*time.Hour - 70*time.Minute
	if !got.Suspended() || got.Remaining(e.clock.Now()) != frozen {
		t.Fatalf("after offline: suspended=%v remaining=%v, want %v", got.Suspended(), got.Remaining(e.clock.Now()), frozen)
	}

	e.clock.Advance(48 * time.Hour) // 失联期间远超原到期时间，也不能被停机
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.State != rental.StateRunning {
		t.Fatalf("state during outage = %s, want running", got.State)
	}

	came, err := e.fleet.Heartbeat(e.ctx, e.machines[0])
	if err != nil || !came {
		t.Fatalf("heartbeat came=%v err=%v", came, err)
	}
	n, err := e.svc.ResumeMachine(e.ctx, e.machines[0])
	mustCount(t, n, err, 1, "resumed")
	got = e.get(inst.ID)
	if got.Suspended() || got.ExpiresAt == nil || !got.ExpiresAt.Equal(e.clock.Now().Add(frozen)) {
		t.Fatalf("after resume = %+v", got)
	}
	var alerts int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = 'machine_offline'`).Scan(&alerts)
	if alerts != 1 {
		t.Fatalf("machine_offline alerts = %d", alerts)
	}
}

func TestRemindersSentOncePerThreshold(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1w)
	e.ack(true)

	n, err := e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 0, "reminders at start")

	e.clock.Advance(6*24*time.Hour + time.Minute) // 剩余 23h59m
	n, err = e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 1, "24h reminder")
	n, err = e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 0, "24h reminder repeated")

	e.clock.Advance(22*time.Hour + 59*time.Minute) // 剩余 1h
	n, err = e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 1, "1h reminder")

	calls := e.notifier.Calls()
	if len(calls) != 2 || calls[0] != inst.ID+":23h59m0s" || calls[1] != inst.ID+":1h0m0s" {
		t.Fatalf("notifications = %v", calls)
	}
}

func TestDayLeaseOnlyGetsOneHourReminder(t *testing.T) {
	e := newEnv(t, 1)
	e.place(catalog.D1d)
	e.ack(true)
	n, err := e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 0, "reminder right after start")
	e.clock.Advance(23 * time.Hour)
	n, err = e.svc.SendReminders(e.ctx)
	mustCount(t, n, err, 1, "1h reminder")
}

func TestConcurrentExpireDoesNotDoubleStop(t *testing.T) {
	e := newEnv(t, 1)
	e.place(catalog.D1d)
	e.ack(true)
	e.clock.Advance(24 * time.Hour)

	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := e.svc.ExpireDue(e.ctx)
			if err != nil {
				t.Errorf("ExpireDue: %v", err)
			}
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != 1 || countPending(e, e.machines[0], fleet.CmdStopInstance) != 1 {
		t.Fatalf("total stopped = %d", total)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'Expire|Wipe|Timeout|Offline|Reminder'`
Expected: FAIL（`ExpireDue` 等未定义）

- [ ] **Step 3: 实现清扫器**

`internal/rental/sweeper.go`:

```go
package rental

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/db"
)

const (
	sweepBatch     = 200
	sweeperLockKey = 7301001 // pg_try_advisory_lock 的键，保证多个 platform 副本只有一个在清扫
)

// ExpireDue 把已到期、未暂停的运行中实例转为停机并下发停止指令。
func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	n := 0
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE state = 'running' AND suspended_at IS NULL AND expires_at <= $1
			ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now, sweepBatch))
		if err != nil {
			return err
		}
		for _, inst := range list {
			if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'stopped', stopped_at = $2, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
				return err
			}
			if err := recordEvent(ctx, tx, inst.ID, StateRunning, StateStopped, "expired", now); err != nil {
				return err
			}
			if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
				return err
			}
			if _, err := s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdStopInstance, nil); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
}

// WipeStale 洗掉停机超过保留期且没有待开始续费的实例。
func (s *Service) WipeStale(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	n := 0
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE state = 'stopped' AND pending_seconds = 0 AND stopped_at <= $1
			ORDER BY stopped_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now.Add(-RetainAfterStop), sweepBatch))
		if err != nil {
			return err
		}
		for _, inst := range list {
			if err := s.beginWipe(ctx, tx, inst, "retention expired", now); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
}

// beginWipe 把实例转为 wiping：取消该实例未完成的指令，再下发洗机指令。
func (s *Service) beginWipe(ctx context.Context, tx pgx.Tx, inst Instance, reason string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'wiping', suspended_at = NULL, paused_remaining = NULL, updated_at = $2
		WHERE id = $1`, inst.ID, now); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, inst.ID, inst.State, StateWiping, reason, now); err != nil {
		return err
	}
	if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
		return err
	}
	if err := s.d.Fleet.CancelCommands(ctx, tx, inst.ID, "superseded by wipe"); err != nil {
		return err
	}
	_, err := s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdWipeInstance, nil)
	return err
}

// FailStuckCreating 把开通超时的实例判为失败并全额退款。
func (s *Service) FailStuckCreating(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	n := 0
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE state = 'creating' AND created_at <= $1
			ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now.Add(-CreateTimeout), sweepBatch))
		if err != nil {
			return err
		}
		for _, inst := range list {
			if err := s.failCreating(ctx, tx, inst, "开通超时", now); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
}

// SuspendMachines 冻结失联机器上运行中实例的剩余时长，暂停期间不计时、不会被判到期。
func (s *Service) SuspendMachines(ctx context.Context, machineIDs []int64) (int, error) {
	if len(machineIDs) == 0 {
		return 0, nil
	}
	now := s.d.Clock.Now()
	n := 0
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE machine_id = ANY($1) AND state = 'running' AND suspended_at IS NULL FOR UPDATE`, machineIDs))
		if err != nil {
			return err
		}
		for _, inst := range list {
			remaining := int64(inst.Remaining(now) / time.Second)
			if _, err := tx.Exec(ctx, `UPDATE instances SET suspended_at = $2, paused_remaining = $3, updated_at = $2 WHERE id = $1`,
				inst.ID, now, remaining); err != nil {
				return err
			}
			if err := recordEvent(ctx, tx, inst.ID, StateRunning, StateRunning, "machine offline: lease paused", now); err != nil {
				return err
			}
			if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
}

// ResumeMachine 机器恢复在线后补回暂停的时长。
func (s *Service) ResumeMachine(ctx context.Context, machineID int64) (int, error) {
	now := s.d.Clock.Now()
	n := 0
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE machine_id = $1 AND state = 'running' AND suspended_at IS NOT NULL FOR UPDATE`, machineID))
		if err != nil {
			return err
		}
		for _, inst := range list {
			var remaining time.Duration
			if inst.PausedRemaining != nil {
				remaining = time.Duration(*inst.PausedRemaining) * time.Second
			}
			r24, r1 := reminderFlags(remaining)
			if _, err := tx.Exec(ctx, `UPDATE instances SET expires_at = $2, suspended_at = NULL, paused_remaining = NULL,
					reminded_24h = reminded_24h OR $3, reminded_1h = reminded_1h OR $4, updated_at = $5 WHERE id = $1`,
				inst.ID, now.Add(remaining), r24, r1, now); err != nil {
				return err
			}
			if err := recordEvent(ctx, tx, inst.ID, StateRunning, StateRunning, "machine online: lease resumed", now); err != nil {
				return err
			}
			if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
}

type reminder struct {
	userID    int64
	id        string
	remaining time.Duration
}

// SendReminders 对剩余不足 24 小时、1 小时的实例各发一次短信提醒（短信在事务提交后发送）。
func (s *Service) SendReminders(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	var due []reminder
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		list, err := collectInstances(tx.Query(ctx, `SELECT `+instanceCols+` FROM instances
			WHERE state = 'running' AND suspended_at IS NULL AND (
				(NOT reminded_24h AND expires_at <= $1) OR (NOT reminded_1h AND expires_at <= $2))
			ORDER BY expires_at LIMIT $3 FOR UPDATE SKIP LOCKED`, now.Add(24*time.Hour), now.Add(time.Hour), sweepBatch))
		if err != nil {
			return err
		}
		for _, inst := range list {
			remaining := inst.ExpiresAt.Sub(now)
			if _, err := tx.Exec(ctx, `UPDATE instances SET reminded_24h = true, reminded_1h = reminded_1h OR $2, updated_at = $3
				WHERE id = $1`, inst.ID, remaining <= time.Hour, now); err != nil {
				return err
			}
			due = append(due, reminder{userID: inst.UserID, id: inst.ID, remaining: remaining})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, r := range due {
		if err := s.d.Notifier.NotifyExpiry(ctx, r.userID, r.id, r.remaining); err != nil {
			s.d.Log.Warn("expiry reminder failed", "instance", r.id, "err", err)
		}
	}
	return len(due), nil
}

// Sweep 执行一轮完整清扫：离线检测与暂停、到期停机、保留期洗机、开通超时、到期提醒。
func (s *Service) Sweep(ctx context.Context) error {
	var errs []error
	offline, err := s.d.Fleet.MarkOfflineStale(ctx, OfflineTimeout)
	if err != nil {
		errs = append(errs, fmt.Errorf("mark offline: %w", err))
	}
	for _, id := range offline {
		if err := s.d.Fleet.RaiseAlert(ctx, s.d.Pool, fleet.Alert{Level: "warning", Kind: "machine_offline",
			MachineID: id, Message: "Agent 失联超过 5 分钟"}); err != nil {
			errs = append(errs, fmt.Errorf("alert offline: %w", err))
		}
	}
	if _, err := s.SuspendMachines(ctx, offline); err != nil {
		errs = append(errs, fmt.Errorf("suspend: %w", err))
	}
	if _, err := s.ExpireDue(ctx); err != nil {
		errs = append(errs, fmt.Errorf("expire: %w", err))
	}
	if _, err := s.WipeStale(ctx); err != nil {
		errs = append(errs, fmt.Errorf("wipe stale: %w", err))
	}
	if _, err := s.FailStuckCreating(ctx); err != nil {
		errs = append(errs, fmt.Errorf("creating timeout: %w", err))
	}
	if _, err := s.SendReminders(ctx); err != nil {
		errs = append(errs, fmt.Errorf("reminders: %w", err))
	}
	return errors.Join(errs...)
}

// RunSweeper 每隔 interval 执行一次 Sweep，直到 ctx 结束。
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

func (s *Service) sweepOnce(ctx context.Context) {
	conn, err := s.d.Pool.Acquire(ctx)
	if err != nil {
		s.d.Log.Error("sweeper acquire conn", "err", err)
		return
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, sweeperLockKey).Scan(&locked); err != nil || !locked {
		return
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, sweeperLockKey) //nolint:errcheck
	if err := s.Sweep(ctx); err != nil {
		s.d.Log.Error("sweep failed", "err", err)
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add -A
git commit -m "feat(rental): sweeper for expiry, retention wipe, create timeout, offline pause and reminders

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: 续费与主动释放

**Files:**
- Create: `internal/rental/renew.go`, `internal/rental/renew_test.go`

**Interfaces:**
- Consumes: Task 7 `charge/lockInstance/recordEvent/refreshLease/seconds/reminderFlags`，Task 9 `beginWipe/SuspendMachines/ExpireDue/WipeStale`
- Produces:
  - `(*Service).Renew(ctx, userID int64, instanceID string, d catalog.Duration) (Order, error)`：
    - creating：`pending_seconds += d`
    - running（未暂停）：`expires_at += d`
    - running（暂停中）：`paused_remaining += d`
    - stopped：`pending_seconds += d`，首次续费时下发 StartInstance
    - 其他状态返回 `ErrInvalidState`，不扣款
  - `(*Service).Release(ctx, userID int64, instanceID string) error`：running/stopped → wiping，不退款

- [ ] **Step 1: 写失败测试**

`internal/rental/renew_test.go`:

```go
package rental_test

import (
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// expireAndStop 让实例到期并模拟 Agent 完成停机。
func expireAndStop(e *env) {
	e.t.Helper()
	e.clock.Advance(24 * time.Hour)
	if _, err := e.svc.ExpireDue(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	e.ack(true)
}

func TestRenewRunningExtendsExpiry(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	ord, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w)
	if err != nil || ord.Kind != "renew" || ord.Amount != 41400 {
		t.Fatalf("renew = %+v, %v", ord, err)
	}
	got := e.get(inst.ID)
	want := dbtest.T0.Add(8 * 24 * time.Hour)
	if !got.ExpiresAt.Equal(want) {
		t.Fatalf("expires = %v, want %v", got.ExpiresAt, want)
	}
	if b := e.balance(e.userID); b != startFunds-6900-41400 {
		t.Fatalf("balance = %d", b)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(want.Add(rental.RetainAfterStop + rental.WipeBuffer)) {
		t.Fatalf("lease upper = %v", upper)
	}
}

func TestRenewWhileCreatingAddsPendingTime(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.PendingSeconds != 2*86400 {
		t.Fatalf("pending = %d", got.PendingSeconds)
	}
	e.clock.Advance(3 * time.Minute)
	e.ack(true)
	if got := e.get(inst.ID); !got.ExpiresAt.Equal(e.clock.Now().Add(48 * time.Hour)) {
		t.Fatalf("expires = %v", got.ExpiresAt)
	}
}

func TestCreateFailureRefundsRenewalsToo(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w); err != nil {
		t.Fatal(err)
	}
	e.ack(false)
	if b := e.balance(e.userID); b != startFunds {
		t.Fatalf("balance = %d, want full refund of both orders", b)
	}
}

func TestRenewStoppedRestartsWithFreshLease(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	expireAndStop(e)
	e.clock.Advance(2 * time.Hour)

	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d); err != nil {
		t.Fatal(err)
	}
	got := e.get(inst.ID)
	if got.State != rental.StateStopped || got.PendingSeconds != 2*86400 {
		t.Fatalf("after renew = %+v", got)
	}
	if n := countPending(e, e.machines[0], fleet.CmdStartInstance); n != 1 {
		t.Fatalf("start commands = %d, want 1", n)
	}

	e.clock.Advance(7 * time.Hour) // 有待开始的续费，不能被 6 小时清扫洗掉
	if n, err := e.svc.WipeStale(e.ctx); err != nil || n != 0 {
		t.Fatalf("WipeStale = %d, %v", n, err)
	}

	e.ack(true)
	got = e.get(inst.ID)
	if got.State != rental.StateRunning || !got.ExpiresAt.Equal(e.clock.Now().Add(48*time.Hour)) || got.StoppedAt != nil {
		t.Fatalf("after start = %+v", got)
	}
}

func TestRenewAfterWipeStartedIsRejectedWithoutCharge(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	expireAndStop(e)
	e.clock.Advance(rental.RetainAfterStop)
	if _, err := e.svc.WipeStale(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("want invalid state, got %v", err)
	}
	if b := e.balance(e.userID); b != startFunds-6900 {
		t.Fatalf("balance = %d, renewal must not be charged", b)
	}
}

func TestRenewWhileSuspendedExtendsFrozenTime(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.svc.SuspendMachines(e.ctx, e.machines); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.Remaining(e.clock.Now()) != 48*time.Hour {
		t.Fatalf("remaining = %v", got.Remaining(e.clock.Now()))
	}
}

func TestRenewValidation(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	other := e.newUser("13600000004", startFunds)
	if _, err := e.svc.Renew(e.ctx, other, inst.ID, catalog.D1d); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user want not found, got %v", err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, "1h"); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("bad duration want bad_request, got %v", err)
	}
	poor := e.newUser("13600000005", 0)
	_, _ = e.pool.Exec(e.ctx, `UPDATE instances SET user_id = $1 WHERE id = $2`, poor, inst.ID)
	before := e.get(inst.ID).ExpiresAt
	if _, err := e.svc.Renew(e.ctx, poor, inst.ID, catalog.D1d); !errors.Is(err, apperr.ErrInsufficientBalance) {
		t.Fatalf("want insufficient balance, got %v", err)
	}
	if after := e.get(inst.ID).ExpiresAt; !after.Equal(*before) {
		t.Fatalf("expires changed %v -> %v", before, after)
	}
}

func TestReleaseRunningWipesWithoutRefund(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.State != rental.StateWiping {
		t.Fatalf("state = %s", got.State)
	}
	if b := e.balance(e.userID); b != startFunds-6900 {
		t.Fatalf("balance = %d, release must not refund", b)
	}
	e.ack(true)
	if got := e.get(inst.ID); got.State != rental.StateReleased {
		t.Fatalf("state = %s", got.State)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateIdle {
		t.Fatalf("machine = %s", got)
	}
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("release twice want invalid state, got %v", err)
	}
	// 机器回到空闲后可以被再次租用
	if _, inst2 := e.place(catalog.D1d); inst2.MachineID != inst.MachineID {
		t.Fatalf("expected machine reuse")
	}
}

func TestReleaseCreatingRejected(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("want invalid state, got %v", err)
	}
}

func TestWipeFailureQuarantinesMachine(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	_ = e.svc.Release(e.ctx, e.userID, inst.ID)
	e.ack(false)
	if got := e.get(inst.ID); got.State != rental.StateWiping {
		t.Fatalf("state = %s, want wiping", got.State)
	}
	if got := e.machineState(e.machines[0]); got != fleet.StateFaulty {
		t.Fatalf("machine = %s, want faulty", got)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = 'wipe_failed'`).Scan(&n)
	if n != 1 {
		t.Fatalf("alerts = %d", n)
	}
}

func TestLeaseExclusionRejectsOverlap(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	_, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'reserved', $3, $3)`,
		inst.MachineID, inst.ID, dbtest.T0.Add(time.Hour), dbtest.T0.Add(2*time.Hour))
	if err == nil {
		t.Fatal("overlapping lease must be rejected")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'Renew|Release|Wipe|Lease'`
Expected: FAIL（`Renew`、`Release` 未定义）

- [ ] **Step 3: 实现**

`internal/rental/renew.go`:

```go
package rental

import (
	"context"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// Renew 续费：扣款建单后按实例当前状态延长时长。wiping/released/failed 状态不允许续费且不扣款。
func (s *Service) Renew(ctx context.Context, userID int64, instanceID string, d catalog.Duration) (Order, error) {
	if !d.Valid() {
		return Order{}, apperr.BadRequest("租期不正确")
	}
	now := s.d.Clock.Now()
	var ord Order
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.State != StateCreating && inst.State != StateRunning && inst.State != StateStopped {
			return apperr.ErrInvalidState
		}
		if ord, err = s.charge(ctx, tx, userID, "renew", inst.ID, inst.ProductID, d, now); err != nil {
			return err
		}
		secs := seconds(d)
		switch {
		case inst.State == StateCreating:
			_, err = tx.Exec(ctx, `UPDATE instances SET pending_seconds = pending_seconds + $2, updated_at = $3 WHERE id = $1`, inst.ID, secs, now)
		case inst.State == StateRunning && inst.Suspended():
			_, err = tx.Exec(ctx, `UPDATE instances SET paused_remaining = paused_remaining + $2, updated_at = $3 WHERE id = $1`, inst.ID, secs, now)
		case inst.State == StateRunning:
			expires := inst.ExpiresAt.Add(d.Length())
			r24, r1 := reminderFlags(expires.Sub(now))
			_, err = tx.Exec(ctx, `UPDATE instances SET expires_at = $2, reminded_24h = $3, reminded_1h = $4, updated_at = $5 WHERE id = $1`,
				inst.ID, expires, r24, r1, now)
		case inst.State == StateStopped:
			if _, err = tx.Exec(ctx, `UPDATE instances SET pending_seconds = pending_seconds + $2, updated_at = $3 WHERE id = $1`, inst.ID, secs, now); err != nil {
				return err
			}
			if inst.PendingSeconds == 0 { // 首次续费才下发启动；已有待开始的续费说明启动指令已在队列中
				_, err = s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdStartInstance, nil)
			}
		}
		if err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, inst.ID, inst.State, inst.State, "renew "+ord.ID, now); err != nil {
			return err
		}
		return s.refreshLease(ctx, tx, inst.ID, now)
	})
	if err != nil {
		return Order{}, err
	}
	return ord, nil
}

// Release 用户主动释放：立即进入洗机，已付费用不退。
func (s *Service) Release(ctx context.Context, userID int64, instanceID string) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.State != StateRunning && inst.State != StateStopped {
			return apperr.ErrInvalidState
		}
		return s.beginWipe(ctx, tx, inst, "user released", now)
	})
}

```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add -A
git commit -m "feat(rental): renew in every live state and user release without refund

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 11: 用户 REST API

**Files:**
- Create: `internal/api/server.go`, `internal/api/httpx.go`, `internal/api/auth.go`, `internal/api/market.go`, `internal/api/wallet.go`, `internal/api/instances.go`
- Create: `internal/api/api_test.go`

**Interfaces:**
- Consumes: Task 2～10 全部 Service
- Produces:
  - `api.Server{Identity *identity.Service; Wallet *wallet.Service; Catalog *catalog.Service; Fleet *fleet.Service; Rental *rental.Service; Clock clock.Clock; Log *slog.Logger; Domain string; SecureCookie bool}`
  - `(*Server).Router() http.Handler`
  - 路由（前缀 `/api/v1`；所有非 GET 请求必须 `Content-Type: application/json`）：

| 方法 | 路径 | 鉴权 | 请求 | 响应 |
|---|---|---|---|---|
| POST | `/auth/sms-code` | 否 | `{phone}` | `{ok:true}` |
| POST | `/auth/login` | 否 | `{phone, code}` | User，设置 Cookie `sc_session` |
| POST | `/auth/logout` | 否 | `{}` | `{ok:true}`，清除 Cookie |
| GET | `/products` | 否 | | `{items:[{id,code,name,spec,prices:[{duration,amount}],idle_count}]}` |
| GET | `/images` | 否 | | `{items:[{id,name,description}]}` |
| GET | `/me` | 是 | | User |
| POST | `/kyc/personal` | 是 | `{name, id_number}` | User |
| POST | `/kyc/enterprise` | 是 | `{company_name, license_no, license_file}` | `{ok:true}` |
| GET | `/wallet` | 是 | | `{balance}` |
| GET | `/wallet/ledger?before_id=&limit=` | 是 | | `{items:[LedgerRow]}` |
| GET | `/orders` | 是 | | `{items:[Order]}` |
| POST | `/orders` | 是 | `{product_id, duration, image_id}` | 201 `{order, instance}` |
| GET | `/instances` | 是 | | `{items:[InstanceView]}` |
| GET | `/instances/{id}` | 是 | | InstanceView |
| GET | `/instances/{id}/password` | 是 | | `{password}` |
| POST | `/instances/{id}/renew` | 是 | `{duration}` | `{order}` |
| POST | `/instances/{id}/release` | 是 | `{}` | `{ok:true}` |

  - 错误响应：`{code, message, request_id}`，HTTP 状态取自 `apperr.Error.Status`；未知错误 500 `internal`
  - `InstanceView`：`id state product_id image_id suspended expires_at remaining_seconds pending_seconds retain_until ssh_command created_at`

- [ ] **Step 1: 写失败测试**

`internal/api/api_test.go`:

```go
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/api"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type rentalNoop struct{}

func (rentalNoop) NotifyExpiry(context.Context, int64, string, time.Duration) error { return nil }

type testEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	ts     *httptest.Server
	wallet *wallet.Service
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	box, _ := secretbox.New(dbtest.Key)
	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: &sms.Mock{}, KYC: kyc.Mock{}, Box: box, FixedCode: "000000"})
	w := wallet.NewService(pool, clk)
	c := catalog.NewService(pool)
	f := fleet.NewService(pool, clk)
	r := rental.NewService(rental.Deps{Pool: pool, Clock: clk, Box: box, Users: ids, Wallet: w, Catalog: c, Fleet: f, Notifier: rentalNoop{}})
	srv := &api.Server{Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Clock: clk,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Domain: "spark.test"}
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return &testEnv{t: t, pool: pool, ts: ts, wallet: w}
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func (e *testEnv) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: e.t, base: e.ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
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
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (c *client) login(phone string) map[string]any {
	c.t.Helper()
	if code, out := c.do("POST", "/auth/sms-code", map[string]string{"phone": phone}); code != 200 {
		c.t.Fatalf("sms-code: %d %v", code, out)
	}
	code, out := c.do("POST", "/auth/login", map[string]string{"phone": phone, "code": "000000"})
	if code != 200 {
		c.t.Fatalf("login: %d %v", code, out)
	}
	return out
}

func TestLoginSessionAndLogout(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	if code, out := c.do("GET", "/me", nil); code != 401 || out["code"] != "unauthorized" {
		t.Fatalf("anonymous /me = %d %v", code, out)
	}
	user := c.login("13800000001")
	if user["phone"] != "13800000001" {
		t.Fatalf("login user = %v", user)
	}
	if code, out := c.do("GET", "/me", nil); code != 200 || out["phone"] != "13800000001" {
		t.Fatalf("/me = %d %v", code, out)
	}
	if code, _ := c.do("POST", "/auth/logout", map[string]any{}); code != 200 {
		t.Fatalf("logout = %d", code)
	}
	if code, _ := c.do("GET", "/me", nil); code != 401 {
		t.Fatalf("/me after logout = %d", code)
	}
}

func TestWritesRequireJSONAndRejectUnknownFields(t *testing.T) {
	e := newTestEnv(t)
	resp, err := http.Post(e.ts.URL+"/api/v1/auth/sms-code", "text/plain", bytes.NewReader([]byte(`{"phone":"13800000001"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("text/plain status = %d, want 400", resp.StatusCode)
	}
	c := e.client()
	if code, out := c.do("POST", "/auth/sms-code", map[string]string{"phone": "13800000001", "x": "1"}); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("unknown field = %d %v", code, out)
	}
}

func TestOrderRequiresKYC(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	c.login("13800000002")
	code, out := c.do("POST", "/orders", map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": dbtest.ImageID(t, e.pool)})
	if code != 403 || out["code"] != "kyc_required" {
		t.Fatalf("order without kyc = %d %v", code, out)
	}
}

func TestOrderAndInstanceEndpoints(t *testing.T) {
	e := newTestEnv(t)
	dbtest.CreateMachine(t, e.pool, "SN001")
	c := e.client()
	user := c.login("13800000003")
	if code, out := c.do("POST", "/kyc/personal", map[string]string{"name": "张三", "id_number": "11010119900101123X"}); code != 200 || out["kyc_status"] != "verified" {
		t.Fatalf("kyc = %d %v", code, out)
	}
	uid := int64(user["id"].(float64))
	if _, err := e.wallet.AdminAdjust(context.Background(), 1, uid, 100000, "测试充值", ""); err != nil {
		t.Fatal(err)
	}

	code, out := c.do("GET", "/products", nil)
	items := out["items"].([]any)
	p := items[0].(map[string]any)
	if code != 200 || p["idle_count"].(float64) != 1 || len(p["prices"].([]any)) != 3 {
		t.Fatalf("products = %d %v", code, out)
	}

	code, out = c.do("POST", "/orders", map[string]any{"product_id": p["id"], "duration": "1d", "image_id": dbtest.ImageID(t, e.pool)})
	if code != 201 {
		t.Fatalf("order = %d %v", code, out)
	}
	inst := out["instance"].(map[string]any)
	id := inst["id"].(string)
	if inst["state"] != "creating" || inst["pending_seconds"].(float64) != 86400 {
		t.Fatalf("instance = %v", inst)
	}

	if code, out := c.do("GET", "/instances", nil); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("instances = %d %v", code, out)
	}
	if code, out := c.do("GET", "/instances/"+id+"/password", nil); code != 200 || len(out["password"].(string)) != 16 {
		t.Fatalf("password = %d %v", code, out)
	}
	if code, out := c.do("GET", "/wallet", nil); code != 200 || out["balance"].(float64) != 100000-6900 {
		t.Fatalf("wallet = %d %v", code, out)
	}
	if code, out := c.do("GET", "/wallet/ledger?limit=10", nil); code != 200 || len(out["items"].([]any)) != 2 {
		t.Fatalf("ledger = %d %v", code, out)
	}
	if code, out := c.do("GET", "/orders", nil); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("orders = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/release", map[string]any{}); code != 409 || out["code"] != "invalid_state" {
		t.Fatalf("release while creating = %d %v", code, out)
	}

	other := e.client()
	other.login("13800000004")
	if code, _ := other.do("GET", "/instances/"+id, nil); code != 404 {
		t.Fatalf("other user's instance = %d, want 404", code)
	}
	if code, out := other.do("GET", "/instances", nil); code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("other user's list = %d %v (items must be [] not null)", code, out)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/api/`
Expected: FAIL（`api` 包不存在）

- [ ] **Step 3: 实现 API**

`internal/api/server.go`:

```go
// Package api 提供用户控制台使用的 REST API。
package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

type Server struct {
	Identity     *identity.Service
	Wallet       *wallet.Service
	Catalog      *catalog.Service
	Fleet        *fleet.Service
	Rental       *rental.Service
	Clock        clock.Clock
	Log          *slog.Logger
	Domain       string // 用于生成 SSH 命令，例如 spark.example.com
	SecureCookie bool   // 生产环境为 true：Cookie 仅经 HTTPS 发送
}

const sessionCookie = "sc_session"

type userCtxKey struct{}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, s.requireJSONForWrites)
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/sms-code", s.handleSendCode)
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/logout", s.handleLogout)
		r.Get("/products", s.handleProducts)
		r.Get("/images", s.handleImages)

		r.Group(func(r chi.Router) {
			r.Use(s.requireUser)
			r.Get("/me", s.handleMe)
			r.Post("/kyc/personal", s.handleKYCPersonal)
			r.Post("/kyc/enterprise", s.handleKYCEnterprise)
			r.Get("/wallet", s.handleWallet)
			r.Get("/wallet/ledger", s.handleLedger)
			r.Get("/orders", s.handleOrders)
			r.Post("/orders", s.handlePlaceOrder)
			r.Get("/instances", s.handleInstances)
			r.Get("/instances/{id}", s.handleInstance)
			r.Get("/instances/{id}/password", s.handlePassword)
			r.Post("/instances/{id}/renew", s.handleRenew)
			r.Post("/instances/{id}/release", s.handleRelease)
		})
	})
	return r
}

// requireJSONForWrites 要求所有写请求使用 application/json；浏览器跨站表单无法伪造该类型，兼作 CSRF 防护。
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

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie(sessionCookie); err == nil {
			token = c.Value
		}
		u, err := s.Identity.Authenticate(r.Context(), token)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey{}, u)))
	})
}

func currentUser(r *http.Request) identity.User {
	return r.Context().Value(userCtxKey{}).(identity.User)
}
```

`internal/api/httpx.go`:

```go
package api

import (
	"encoding/json"
	"io"
	"net"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/platform/apperr"
)

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) }

// list 包装列表响应，保证空列表输出 [] 而不是 null。
func list[T any](items []T) map[string]any {
	if items == nil {
		items = []T{}
	}
	return map[string]any{"items": items}
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	rid := middleware.GetReqID(r.Context())
	if e := apperr.As(err); e != nil {
		writeJSON(w, e.Status, errorBody{Code: e.Code, Message: e.Message, RequestID: rid})
		return
	}
	s.Log.Error("internal error", "err", err, "request_id", rid, "method", r.Method, "path", r.URL.Path)
	writeJSON(w, http.StatusInternalServerError, errorBody{Code: "internal", Message: "服务器内部错误，请稍后重试", RequestID: rid})
}

// decodeJSON 解析请求体（上限 1MB），拒绝未知字段。
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.BadRequest("请求参数格式不正确")
	}
	return nil
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
```

`internal/api/auth.go`:

```go
package api

import "net/http"

func (s *Server) setSession(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) handleSendCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Identity.SendLoginCode(r.Context(), req.Phone, clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	token, u, err := s.Identity.Login(r.Context(), req.Phone, req.Code)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.setSession(w, token, 30*24*3600)
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.Identity.Logout(r.Context(), c.Value); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	s.setSession(w, "", -1)
	ok(w)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUser(r))
}

func (s *Server) handleKYCPersonal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		IDNumber string `json:"id_number"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	u, err := s.Identity.SubmitPersonalKYC(r.Context(), currentUser(r).ID, req.Name, req.IDNumber)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleKYCEnterprise(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CompanyName string `json:"company_name"`
		LicenseNo   string `json:"license_no"`
		LicenseFile string `json:"license_file"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Identity.SubmitEnterpriseKYC(r.Context(), currentUser(r).ID, req.CompanyName, req.LicenseNo, req.LicenseFile); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}
```

`internal/api/market.go`:

```go
package api

import (
	"net/http"

	"spark-cloud/internal/catalog"
)

type productView struct {
	catalog.ProductWithPrices
	IdleCount int `json:"idle_count"`
}

func (s *Server) handleProducts(w http.ResponseWriter, r *http.Request) {
	products, err := s.Catalog.ListProducts(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]productView, 0, len(products))
	for _, p := range products {
		n, err := s.Fleet.IdleCount(r.Context(), p.ID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		views = append(views, productView{ProductWithPrices: p, IdleCount: n})
	}
	writeJSON(w, http.StatusOK, list(views))
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	images, err := s.Catalog.ListImages(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(images))
}
```

`internal/api/wallet.go`:

```go
package api

import (
	"net/http"
	"strconv"
)

func (s *Server) handleWallet(w http.ResponseWriter, r *http.Request) {
	bal, err := s.Wallet.Balance(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"balance": bal})
}

func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before_id"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.Wallet.ListLedger(r.Context(), currentUser(r).ID, before, limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(rows))
}
```

`internal/api/instances.go`:

```go
package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/rental"
)

type instanceView struct {
	ID               string     `json:"id"`
	State            string     `json:"state"`
	ProductID        int64      `json:"product_id"`
	ImageID          int64      `json:"image_id"`
	Suspended        bool       `json:"suspended"`
	ExpiresAt        *time.Time `json:"expires_at"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	PendingSeconds   int64      `json:"pending_seconds"`
	RetainUntil      *time.Time `json:"retain_until"` // 停机后数据保留截止时间
	SSHCommand       string     `json:"ssh_command"`
	CreatedAt        time.Time  `json:"created_at"`
}

func (s *Server) view(i rental.Instance) instanceView {
	now := s.Clock.Now()
	v := instanceView{
		ID: i.ID, State: i.State, ProductID: i.ProductID, ImageID: i.ImageID, Suspended: i.Suspended(),
		ExpiresAt: i.ExpiresAt, RemainingSeconds: int64(i.Remaining(now) / time.Second),
		PendingSeconds: i.PendingSeconds, CreatedAt: i.CreatedAt,
	}
	if i.State == rental.StateRunning {
		v.SSHCommand = fmt.Sprintf("ssh %s@ssh.%s", i.ID, s.Domain)
	}
	if i.State == rental.StateStopped && i.StoppedAt != nil && i.PendingSeconds == 0 {
		t := i.StoppedAt.Add(rental.RetainAfterStop)
		v.RetainUntil = &t
	}
	return v
}

func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProductID int64  `json:"product_id"`
		Duration  string `json:"duration"`
		ImageID   int64  `json:"image_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	ord, inst, err := s.Rental.Place(r.Context(), rental.PlaceRequest{
		UserID: currentUser(r).ID, ProductID: req.ProductID, ImageID: req.ImageID, Duration: catalog.Duration(req.Duration),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"order": ord, "instance": s.view(inst)})
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := s.Rental.ListOrders(r.Context(), currentUser(r).ID, 50)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(orders))
}

func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := s.Rental.ListForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]instanceView, 0, len(insts))
	for _, i := range insts {
		views = append(views, s.view(i))
	}
	writeJSON(w, http.StatusOK, list(views))
}

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := s.Rental.GetForUser(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(inst))
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	pw, err := s.Rental.Password(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"password": pw})
}

func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Duration string `json:"duration"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	ord, err := s.Rental.Renew(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), catalog.Duration(req.Duration))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": ord})
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	if err := s.Rental.Release(r.Context(), currentUser(r).ID, chi.URLParam(r, "id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/api/`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add -A
git commit -m "feat(api): user REST API for auth, kyc, market, wallet, orders and instances

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 12: 配置、服务组装、启动入口与端到端测试

**Files:**
- Create: `internal/platform/config/config.go`, `internal/platform/config/config_test.go`
- Create: `internal/app/app.go`, `internal/app/e2e_test.go`
- Create: `cmd/platform/main.go`, `cmd/devdb/main.go`
- Create: `Makefile`, `README.md`

**Interfaces:**
- Consumes: 全部
- Produces:
  - `config.Config{Env, HTTPAddr, DatabaseURL, Domain string; SecretKey []byte; SMSDriver, KYCDriver string; SweepInterval time.Duration}`；`config.Load(getenv func(string) string) (Config, error)`
  - 环境变量：`SPARK_ENV`（dev/staging/prod，默认 dev）、`SPARK_HTTP_ADDR`（默认 `:8080`）、`SPARK_DATABASE_URL`（必填）、`SPARK_DOMAIN`（默认 `spark.local`）、`SPARK_SECRET_KEY`（必填，32 字节 base64）、`SPARK_SMS_DRIVER`/`SPARK_KYC_DRIVER`（默认 mock）、`SPARK_SWEEP_INTERVAL`（默认 `1m`）
  - `app.Options{Clock clock.Clock; SMS sms.Sender; KYC kyc.Verifier}`；`app.App{Identity, Wallet, Catalog, Fleet, Rental; Handler http.Handler}`；`app.Build(cfg, pool, log, opt) (*App, error)`

- [ ] **Step 1: 写配置的失败测试**

`internal/platform/config/config_test.go`:

```go
package config_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/platform/config"
)

var validKey = base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"SPARK_DATABASE_URL": "postgres://x", "SPARK_SECRET_KEY": validKey}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != "dev" || c.HTTPAddr != ":8080" || c.Domain != "spark.local" || c.SMSDriver != "mock" ||
		c.KYCDriver != "mock" || c.SweepInterval != time.Minute || len(c.SecretKey) != 32 {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing db", map[string]string{"SPARK_SECRET_KEY": validKey}, "SPARK_DATABASE_URL"},
		{"bad key", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": "short"}, "SPARK_SECRET_KEY"},
		{"bad env", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_ENV": "test"}, "SPARK_ENV"},
		{"prod mock", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_ENV": "prod"}, "mock"},
		{"bad interval", map[string]string{"SPARK_DATABASE_URL": "x", "SPARK_SECRET_KEY": validKey, "SPARK_SWEEP_INTERVAL": "soon"}, "SPARK_SWEEP_INTERVAL"},
	}
	for _, c := range cases {
		_, err := config.Load(env(c.env))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want mention of %s", c.name, err, c.want)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/platform/config/`
Expected: FAIL（包不存在）

- [ ] **Step 3: 实现配置**

`internal/platform/config/config.go`:

```go
// Package config 从环境变量读取 platform 配置。
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

type Config struct {
	Env           string // dev / staging / prod
	HTTPAddr      string
	DatabaseURL   string
	Domain        string
	SecretKey     []byte // 32 字节，用于加密身份证号与实例密码
	SMSDriver     string // mock / aliyun（计划 4）
	KYCDriver     string // mock / aliyun（计划 4）
	SweepInterval time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Env:         or("SPARK_ENV", "dev"),
		HTTPAddr:    or("SPARK_HTTP_ADDR", ":8080"),
		DatabaseURL: getenv("SPARK_DATABASE_URL"),
		Domain:      or("SPARK_DOMAIN", "spark.local"),
		SMSDriver:   or("SPARK_SMS_DRIVER", "mock"),
		KYCDriver:   or("SPARK_KYC_DRIVER", "mock"),
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
	if c.Env == "prod" && (c.SMSDriver == "mock" || c.KYCDriver == "mock") {
		return Config{}, errors.New("生产环境禁止使用 mock 插件（SPARK_SMS_DRIVER / SPARK_KYC_DRIVER）")
	}
	c.SweepInterval = time.Minute
	if v := getenv("SPARK_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("SPARK_SWEEP_INTERVAL 格式不正确：%q", v)
		}
		c.SweepInterval = d
	}
	return c, nil
}
```

Run: `go test ./internal/platform/config/`
Expected: PASS

- [ ] **Step 4: 写端到端失败测试**

`internal/app/e2e_test.go`:

```go
package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"spark-cloud/internal/app"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/sms"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func (c *client) do(method, path string, body any) map[string]any {
	c.t.Helper()
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
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 {
		c.t.Fatalf("%s %s = %d %v", method, path, resp.StatusCode, out)
	}
	return out
}

// TestFullRentalLifecycle 走通：登录 → 实名 → 充值 → 下单 → 开通 → 到期提醒 → 到期停机 → 续费 → 重新启动 → 释放 → 洗机 → 机器回到库存。
func TestFullRentalLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	smsMock := &sms.Mock{}
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock"}
	a, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Clock: clk, SMS: smsMock})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	mid := dbtest.CreateMachine(t, pool, "SN001")

	// agent 模拟 Agent：心跳并成功执行全部待处理指令
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
	sweep := func() {
		t.Helper()
		if _, err := a.Fleet.Heartbeat(ctx, mid); err != nil {
			t.Fatal(err)
		}
		if err := a.Rental.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}

	c.do("POST", "/auth/sms-code", map[string]string{"phone": "13800001234"})
	user := c.do("POST", "/auth/login", map[string]string{"phone": "13800001234", "code": "000000"})
	c.do("POST", "/kyc/personal", map[string]string{"name": "李四", "id_number": "110101199001011234"})
	if _, err := a.Wallet.AdminAdjust(ctx, 1, int64(user["id"].(float64)), 100000, "线下转账", ""); err != nil {
		t.Fatal(err)
	}

	products := c.do("GET", "/products", nil)["items"].([]any)
	images := c.do("GET", "/images", nil)["items"].([]any)
	order := c.do("POST", "/orders", map[string]any{
		"product_id": products[0].(map[string]any)["id"], "duration": "1d", "image_id": images[0].(map[string]any)["id"],
	})
	id := order["instance"].(map[string]any)["id"].(string)

	clk.Advance(90 * time.Second)
	agent()
	inst := c.do("GET", "/instances/"+id, nil)
	if inst["state"] != "running" || inst["remaining_seconds"].(float64) != 86400 || inst["ssh_command"] != "ssh "+id+"@ssh.spark.test" {
		t.Fatalf("after create = %v", inst)
	}

	clk.Advance(23 * time.Hour)
	sweep()
	sent := smsMock.Sent()
	last := sent[len(sent)-1]
	if last.Template != sms.TemplateExpiryReminder || last.Params["instance"] != id || last.Params["hours"] != "1" {
		t.Fatalf("reminder sms = %+v", last)
	}

	clk.Advance(time.Hour)
	sweep()
	agent()
	inst = c.do("GET", "/instances/"+id, nil)
	if inst["state"] != "stopped" || inst["retain_until"] == nil {
		t.Fatalf("after expiry = %v", inst)
	}

	clk.Advance(time.Hour)
	c.do("POST", "/instances/"+id+"/renew", map[string]string{"duration": "1w"})
	agent()
	inst = c.do("GET", "/instances/"+id, nil)
	if inst["state"] != "running" || inst["remaining_seconds"].(float64) != 7*86400 {
		t.Fatalf("after renew = %v", inst)
	}

	c.do("POST", "/instances/"+id+"/release", map[string]any{})
	agent()
	if inst = c.do("GET", "/instances/"+id, nil); inst["state"] != "released" {
		t.Fatalf("after release = %v", inst)
	}
	if p := c.do("GET", "/products", nil)["items"].([]any)[0].(map[string]any); p["idle_count"].(float64) != 1 {
		t.Fatalf("machine not back in stock: %v", p)
	}
	if bal := c.do("GET", "/wallet", nil)["balance"].(float64); bal != 100000-6900-41400 {
		t.Fatalf("balance = %v", bal)
	}
	if rows := c.do("GET", "/wallet/ledger", nil)["items"].([]any); len(rows) != 3 {
		t.Fatalf("ledger rows = %d", len(rows))
	}
}
```

Run: `go test ./internal/app/`
Expected: FAIL（`app` 包不存在）

- [ ] **Step 5: 实现服务组装**

`internal/app/app.go`:

```go
// Package app 按配置组装所有服务，供 cmd/platform 与端到端测试共用。
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/api"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

// Options 用于测试时替换时钟与插件；零值表示按配置创建。
type Options struct {
	Clock clock.Clock
	SMS   sms.Sender
	KYC   kyc.Verifier
}

type App struct {
	Identity *identity.Service
	Wallet   *wallet.Service
	Catalog  *catalog.Service
	Fleet    *fleet.Service
	Rental   *rental.Service
	Handler  http.Handler
}

func Build(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger, opt Options) (*App, error) {
	clk := opt.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	box, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	smsSender := opt.SMS
	if smsSender == nil {
		switch cfg.SMSDriver {
		case "mock":
			smsSender = &sms.Mock{}
		default:
			return nil, fmt.Errorf("短信驱动 %q 尚未实现（计划 4 接入阿里云）", cfg.SMSDriver)
		}
	}
	verifier := opt.KYC
	if verifier == nil {
		switch cfg.KYCDriver {
		case "mock":
			verifier = kyc.Mock{}
		default:
			return nil, fmt.Errorf("实名驱动 %q 尚未实现（计划 4 接入阿里云）", cfg.KYCDriver)
		}
	}
	fixedCode := ""
	if cfg.SMSDriver == "mock" && cfg.Env != "prod" {
		fixedCode = "000000"
	}

	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: smsSender, KYC: verifier, Box: box, FixedCode: fixedCode})
	w := wallet.NewService(pool, clk)
	c := catalog.NewService(pool)
	f := fleet.NewService(pool, clk)
	r := rental.NewService(rental.Deps{
		Pool: pool, Clock: clk, Box: box, Log: log, Users: ids, Wallet: w, Catalog: c, Fleet: f,
		Notifier: expiryNotifier{ids: ids, sms: smsSender},
	})
	srv := &api.Server{
		Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Clock: clk, Log: log,
		Domain: cfg.Domain, SecureCookie: cfg.Env == "prod",
	}
	return &App{Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Handler: srv.Router()}, nil
}

// expiryNotifier 通过短信发送到期提醒。
type expiryNotifier struct {
	ids *identity.Service
	sms sms.Sender
}

func (n expiryNotifier) NotifyExpiry(ctx context.Context, userID int64, instanceID string, remaining time.Duration) error {
	u, err := n.ids.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	hours := int(remaining.Round(time.Hour) / time.Hour)
	if hours < 1 {
		hours = 1
	}
	return n.sms.Send(ctx, u.Phone, sms.TemplateExpiryReminder, map[string]string{"instance": instanceID, "hours": strconv.Itoa(hours)})
}
```

Run: `go vet ./... && go test ./internal/app/`
Expected: PASS

- [ ] **Step 6: 启动入口、本地数据库、Makefile、README**

`cmd/platform/main.go`:

```go
// platform 是 Spark Cloud 控制面服务。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"spark-cloud/internal/app"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/db"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("platform exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	a, err := app.Build(cfg, pool, log, app.Options{})
	if err != nil {
		return err
	}
	go a.Rental.RunSweeper(ctx, cfg.SweepInterval)

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: a.Handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Info("platform listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
```

`cmd/devdb/main.go`:

```go
// devdb 启动一个持久化的本地 PostgreSQL，供本地开发使用（端口 5433，数据在 .devdata/）。Ctrl+C 停止。
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

func main() {
	dir, err := filepath.Abs(".devdata")
	if err != nil {
		log.Fatal(err)
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(5433).
		Database("spark").
		DataPath(filepath.Join(dir, "pg")).
		RuntimePath(filepath.Join(dir, "runtime")))
	if err := pg.Start(); err != nil {
		log.Fatal(err)
	}
	fmt.Println("PostgreSQL 已启动：postgres://postgres:postgres@localhost:5433/spark?sslmode=disable")
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	if err := pg.Stop(); err != nil {
		log.Fatal(err)
	}
}
```

`Makefile`（缩进必须是 Tab）:

```makefile
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
```

实现时先用 `printf '%s' 01234567890123456789012345678901 | base64` 核对 `DEV_KEY`。

`README.md`:

````markdown
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
````

- [ ] **Step 7: 冒烟运行**

```bash
make devdb &   # 后台启动数据库
sleep 15
make run &     # 后台启动 platform
sleep 10
curl -s localhost:8080/api/v1/products
curl -s -X POST -H 'Content-Type: application/json' -d '{"phone":"13800000000"}' localhost:8080/api/v1/auth/sms-code
```

Expected: `/products` 返回包含 `DGX Spark 整机`、三档价格、`idle_count: 0` 的 JSON；`sms-code` 返回 `{"ok":true}`。验证后结束两个后台进程。

- [ ] **Step 8: 全量测试并提交**

Run: `make test`
Expected: 全部 PASS

```bash
git add -A
git commit -m "feat: config, app wiring, platform entrypoint, devdb and end-to-end lifecycle test

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```
