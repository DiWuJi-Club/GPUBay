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
