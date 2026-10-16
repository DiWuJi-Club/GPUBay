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
		Users:         identity.NewService(identity.Deps{Pool: pool, Clock: clk}),
		Alerts:        fleet.NewService(pool, clk),
		Providers:     map[string]pay.Provider{pay.ChannelWechat: wx, pay.ChannelAlipay: ali},
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
