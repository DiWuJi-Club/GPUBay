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
