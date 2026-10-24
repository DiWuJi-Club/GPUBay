package appdeploy_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
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

// env 是 appdeploy 包数据库测试的公共环境：嵌入式 PG、假时钟、在线机器若干。
type env struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	clk       *clock.Fake
	fleet     *fleet.Service
	catalog   *catalog.Service
	box       *secretbox.Box
	cat       *appdeploy.Catalog
	rental    *rental.Service
	ids       *identity.Service
	svc       *appdeploy.Service
	imageID   int64
	userID    int64
	productID int64
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
	f := fleet.NewService(pool, clk)
	c := catalog.NewService(pool).WithClock(clk)
	e := &env{t: t, ctx: context.Background(), pool: pool, clk: clk, fleet: f, catalog: c, box: box,
		cat:     appdeploy.NewCatalog(pool, clk, f, c, slog.New(slog.NewTextHandler(io.Discard, nil))),
		imageID: dbtest.ImageID(t, pool), userID: dbtest.CreateUser(t, pool, "13800000000", true), productID: dbtest.ProductID(t, pool)}
	dbtest.Fund(t, pool, e.userID, 1_000_000)
	for i := 0; i < machines; i++ {
		e.machines = append(e.machines, dbtest.CreateMachine(t, pool, fmt.Sprintf("SN%03d", i)))
	}
	e.exec(`UPDATE machines SET agent_online = true`)
	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: &sms.Mock{}, KYC: kyc.Mock{}, Box: box})
	e.ids = ids
	e.setSSHBasePort(0)
	return e
}

// setSSHBasePort 以给定的容器 sshd 基准端口重建 rental 与部署服务（0 = 默认 2222）。
// 上游测试把 sshtest 端口放在这里、给上游池一个死端口：证明部署 Runner / 实时日志按实例端口拨号（与 sshgw / ingress 一致）。
func (e *env) setSSHBasePort(port int) {
	e.rental = rental.NewService(rental.Deps{Pool: e.pool, Clock: e.clk, Box: e.box, Users: e.ids, Wallet: wallet.NewService(e.pool, e.clk),
		Catalog: e.catalog, Fleet: e.fleet, SSHBasePort: port})
	e.svc = e.newService(nil, "")
}

// deadPort 返回一个刚关闭、无人监听的本机端口（上游池默认端口用它：没走实例端口就连不上）。
func deadPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// newService 用给定的上游连接池与 AppRoot 组装部署服务（nil / 空 = 无上游 / DefaultAppRoot）。
func (e *env) newService(up *upstream.Pool, appRoot string) *appdeploy.Service {
	return appdeploy.NewService(appdeploy.Deps{Pool: e.pool, Clock: e.clk, Box: e.box, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Catalog: e.cat, Images: e.catalog, Rental: e.rental, Fleet: e.fleet, Upstream: up, AppRoot: appRoot,
		PortURL: func(id string, port int) string { return fmt.Sprintf("https://%s-%d.apps.test/", id, port) }})
}

func (e *env) actor() audit.Actor { return audit.Actor{AdminID: 1} }

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

const goodScript = "#!/bin/sh\ncase \"$1\" in install) ;; start) sleep 1 & echo $! > \"$SPARK_APP_DIR/service.pid\" ;; esac\n"

func (e *env) goodVersionInput() appdeploy.VersionInput {
	return appdeploy.VersionInput{Version: "1.0.0", ImageIDs: []int64{e.imageID}, Spec: goodSpec(), Script: goodScript}
}

// app 建一个 llm 类应用（status 为空 = draft）。
func (e *env) app(slug, status string) appdeploy.App {
	e.t.Helper()
	a, err := e.cat.CreateApp(e.ctx, e.actor(), appdeploy.AppInput{Slug: slug, Name: slug, Category: "llm", Status: status})
	if err != nil {
		e.t.Fatalf("CreateApp %s: %v", slug, err)
	}
	return a
}

// version 在应用下建一个合法 draft 版本。
func (e *env) version(appID int64, ver string) appdeploy.Version {
	e.t.Helper()
	in := e.goodVersionInput()
	in.Version = ver
	v, err := e.cat.CreateVersion(e.ctx, e.actor(), appID, in)
	if err != nil {
		e.t.Fatalf("CreateVersion %s: %v", ver, err)
	}
	return v
}

func (e *env) imageRef(id int64) string {
	e.t.Helper()
	var ref string
	if err := e.pool.QueryRow(e.ctx, `SELECT ref FROM images WHERE id = $1`, id).Scan(&ref); err != nil {
		e.t.Fatal(err)
	}
	return ref
}
