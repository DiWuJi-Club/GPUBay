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
