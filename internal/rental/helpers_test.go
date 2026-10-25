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
	"spark-cloud/internal/platform/randx"
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

func (n *fakeNotifier) NotifyAutoRenewFailed(_ context.Context, _ int64, instanceID, reason string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, "auto_renew_failed:"+instanceID+":"+reason)
	return nil
}

func (n *fakeNotifier) NotifyBookingCancelled(_ context.Context, _ int64, instanceID, reason string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, "booking_cancelled:"+instanceID+":"+reason)
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
	keys      *fakeKeys
	userID    int64
	systemID  int64 // 计划 9：迁移插入的系统账号（system:llm）
	productID int64
	imageID   int64
	machines  []int64
}

// testSSHBasePort 是测试环境的上游 SSH 基准端口；故意不用默认的 2222，验证 Create 指令按配置取端口。
const testSSHBasePort = 2300

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
	keys := &fakeKeys{}
	var systemID int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE system AND phone = 'system:llm'`).Scan(&systemID); err != nil {
		t.Fatalf("system user: %v", err)
	}
	svc := rental.NewService(rental.Deps{
		Pool: pool, Clock: clk, Box: box, Users: ids, Wallet: w,
		Catalog: catalog.NewService(pool), Fleet: f, Notifier: n, Keys: keys, SystemUserID: systemID, SSHBasePort: testSSHBasePort,
	})
	e := &env{t: t, ctx: context.Background(), pool: pool, clock: clk, svc: svc, wallet: w, fleet: f, notifier: n, keys: keys,
		systemID: systemID, productID: dbtest.ProductID(t, pool), imageID: dbtest.ImageID(t, pool)}
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

// reportRunning 模拟网关收到一份「该实例 running」的 StateReport（含本次连接归属）。
func (e *env) reportRunning(machineID int64, instanceID string) {
	e.t.Helper()
	if err := e.fleet.AttachAgentConn(e.ctx, machineID, "conn-test"); err != nil {
		e.t.Fatal(err)
	}
	if err := e.fleet.RecordStateReport(e.ctx, machineID, fleet.StateReport{Instances: []fleet.InstanceState{{InstanceID: instanceID, Phase: fleet.PhaseRunning}}}); err != nil {
		e.t.Fatal(err)
	}
}

// firstOf 把 (n, err) 压成 n，错误直接失败；用于 mustCount 之前已能断言 err 的场合。
func firstOf(n int, err error) int {
	if err != nil {
		panic(err)
	}
	return n
}

// fakeKeys 模拟 identity.CountSSHKeys：n 为用户当前公钥数。
type fakeKeys struct{ n int }

func (f *fakeKeys) CountSSHKeys(context.Context, int64) (int, error) { return f.n, nil }

// newMachine 再登记一台空闲在线机器并加入 e.machines。
func (e *env) newMachine(serial string) int64 {
	e.t.Helper()
	id := dbtest.CreateMachine(e.t, e.pool, serial)
	e.machines = append(e.machines, id)
	return id
}

// reserveRaw 直接写库造一个预订（Task 5 之前没有 PlaceBatch，之后也用于绕过参数校验）：
// reserved 实例 + reserved lease，period 按规格 §4.1 = [start − PrepLead, start + secs + Tail)。
// 第三参数是秒（int64），调用方写 86400 / 3600 这类字面量；不要用 time.Duration，无类型常量会被当成纳秒。
func (e *env) reserveRaw(machineID int64, start time.Time, secs int64) string {
	e.t.Helper()
	d := time.Duration(secs) * time.Second
	id := randx.ID("ins-", 8)
	now := e.clock.Now()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, pending_seconds,
			ssh_password_enc, scheduled_start_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'reserved', $6, $7, $8, $9, $9)`,
		id, e.userID, machineID, e.productID, e.imageID, int64(d/time.Second), []byte("x"), start, now); err != nil {
		e.t.Fatalf("reserveRaw instance: %v", err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'reserved', $5, $5)`, machineID, id, start.Add(-rental.PrepLead), start.Add(d+rental.Tail), now); err != nil {
		e.t.Fatalf("reserveRaw lease: %v", err)
	}
	return id
}
