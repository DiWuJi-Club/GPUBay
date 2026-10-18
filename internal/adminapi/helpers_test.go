package adminapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/adminapi"
	"spark-cloud/internal/agentgw"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/billing"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/pki"
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
	gw       *agentgw.Server
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// newEnvWith 在构建路由前允许调整 Server 字段（例如 LoginLimit）。
func newEnvWith(t *testing.T, tweak func(*adminapi.Server)) *env {
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
	c := catalog.NewService(pool).WithClock(clk)
	f := fleet.NewService(pool, clk)
	pduMock := &pdu.Mock{}
	f.ConfigurePDU(fleet.PDUConfig{Box: box, AllowMock: true, Open: func(fleet.PDUConn) (pdu.Driver, error) { return pduMock, nil }})
	r := rental.NewService(rental.Deps{Pool: pool, Clock: clk, Box: box, Log: log, Users: ids, Wallet: w, Catalog: c, Fleet: f,
		Notifier: noopNotifier{}})
	payMock := pay.NewMock("s")
	b := billing.NewService(billing.Deps{Pool: pool, Clock: clk, Log: log, Wallet: w, Users: ids, Alerts: f,
		Providers: map[string]pay.Provider{pay.ChannelWechat: payMock}, NotifyBaseURL: "https://spark.test"})
	admins := admin.NewService(admin.Deps{Pool: pool, Clock: clk, Box: box})
	ca, err := pki.EnsureCA(context.Background(), pool, box, clk)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := agentgw.New(agentgw.Config{AdvertiseAddr: "agent.test:9443", Hosts: []string{"127.0.0.1"}, DownloadURL: "http://files.test/agent", BinarySHA256: "beef"},
		agentgw.Deps{Pool: pool, Fleet: f, Rental: r, CA: ca, Clock: clk, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	srv := &adminapi.Server{Admins: admins, Identity: ids, Wallet: w, Catalog: c, Fleet: f, Rental: r, Billing: b,
		Uploads: uploads, Gateway: gw, Clock: clk, Log: log}
	if tweak != nil {
		tweak(srv)
	}
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return &env{t: t, ctx: context.Background(), pool: pool, clk: clk, srv: srv, ts: ts, admins: admins, identity: ids,
		wallet: w, catalog: c, fleet: f, rental: r, billing: b, uploads: uploads, pdu: pduMock, pay: payMock, gw: gw}
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

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
