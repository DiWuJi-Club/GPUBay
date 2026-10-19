package referral_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/referral"
	"spark-cloud/internal/wallet"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type fakeUsers struct {
	pool    *pgxpool.Pool
	devices map[int64]map[string]bool
	fps     map[int64][]string
}

func (f *fakeUsers) UserState(ctx context.Context, q db.Querier, id int64) (referral.UserState, error) {
	var u referral.UserState
	err := q.QueryRow(ctx, `SELECT status, kyc_status, phone, last_login_ip, last_login_at FROM users WHERE id = $1`, id).
		Scan(&u.Status, &u.KYCStatus, &u.Phone, &u.LastLoginIP, &u.LastLoginAt)
	if db.IsNoRows(err) {
		return u, apperr.ErrNotFound
	}
	return u, err
}
func (f *fakeUsers) HasDevice(_ context.Context, _ db.Querier, userID int64, deviceID string) (bool, error) {
	return f.devices[userID][deviceID], nil
}
func (f *fakeUsers) VerifiedFingerprints(_ context.Context, _ db.Querier, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	for _, id := range ids {
		if fps := f.fps[id]; len(fps) > 0 {
			out[id] = fps
		}
	}
	return out, nil
}

type fakeOrder struct {
	userID int64
	start  *time.Time
}
type fakeOrders struct {
	m    map[string]fakeOrder
	fail map[string]error // 查询该订单时返回的错误（模拟查询失败）
}

func (f *fakeOrders) OrderForReferral(_ context.Context, _ db.Querier, id string) (int64, *time.Time, error) {
	if err := f.fail[id]; err != nil {
		return 0, nil, err
	}
	o, ok := f.m[id]
	if !ok {
		return 0, nil, apperr.ErrNotFound
	}
	return o.userID, o.start, nil
}

type env struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	clk    *clock.Fake
	svc    *referral.Service
	wallet *wallet.Service
	users  *fakeUsers
	orders *fakeOrders
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	w := wallet.NewService(pool, clk)
	users := &fakeUsers{pool: pool, devices: map[int64]map[string]bool{}, fps: map[int64][]string{}}
	orders := &fakeOrders{m: map[string]fakeOrder{}, fail: map[string]error{}}
	svc := referral.NewService(referral.Deps{Pool: pool, Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Wallet: w, Users: users, Orders: orders})
	return &env{t: t, ctx: context.Background(), pool: pool, clk: clk, svc: svc, wallet: w, users: users, orders: orders}
}

func (e *env) user(phone string, verified bool) int64 {
	return dbtest.CreateUser(e.t, e.pool, phone, verified)
}

// pay 在当前假时刻写一条被邀请人的 order_pay 流水并登记订单；返回流水 id。
func (e *env) pay(userID int64, order string, amount int64) int64 {
	return e.payAt(userID, order, amount, nil)
}

func (e *env) payAt(userID int64, order string, amount int64, start *time.Time) int64 {
	e.t.Helper()
	dbtest.Fund(e.t, e.pool, userID, amount)
	var id int64
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := e.wallet.Apply(e.ctx, tx, wallet.Entry{UserID: userID, Amount: -amount, Kind: wallet.KindOrderPay, RefType: "order", RefID: order, Remark: "新购 ins-x"})
		if err != nil {
			return err
		}
		return tx.QueryRow(e.ctx, `SELECT id FROM wallet_ledger WHERE kind = 'order_pay' AND ref_id = $1`, order).Scan(&id)
	}); err != nil {
		e.t.Fatalf("pay: %v", err)
	}
	e.orders.m[order] = fakeOrder{userID: userID, start: start}
	return id
}

func (e *env) refund(userID int64, refID string, amount int64) int64 {
	e.t.Helper()
	var id int64
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := e.wallet.Apply(e.ctx, tx, wallet.Entry{UserID: userID, Amount: amount, Kind: wallet.KindRefund, RefType: "order", RefID: refID, Remark: "退款"})
		if err != nil {
			return err
		}
		return tx.QueryRow(e.ctx, `SELECT id FROM wallet_ledger WHERE kind = 'refund' AND ref_id = $1`, refID).Scan(&id)
	}); err != nil {
		e.t.Fatalf("refund: %v", err)
	}
	return id
}

// bind 直接按当前费率版本插入一条 active 绑定（Bind 的业务逻辑由 Task 5 单独测）。
func (e *env) bind(inviter, invitee int64) int64 {
	e.t.Helper()
	if _, err := e.svc.EnsureCode(e.ctx, inviter); err != nil {
		e.t.Fatal(err)
	}
	now := e.clk.Now()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO referral_bindings (inviter_id, invitee_id, code, status, rate_version_id, bound_at, rebate_until, created_at, updated_at)
		SELECT $1, $2, a.code, 'active', v.id, $3, $3 + make_interval(days => v.period_days), $3, $3
		FROM referral_accounts a, LATERAL (SELECT id, period_days FROM referral_rate_versions WHERE effective_from <= $3 ORDER BY effective_from DESC, id DESC LIMIT 1) v
		WHERE a.user_id = $1 RETURNING id`, inviter, invitee, now).Scan(&id); err != nil {
		e.t.Fatalf("bind: %v", err)
	}
	return id
}

func (e *env) ledgerSum(userID int64, kind string) int64 {
	e.t.Helper()
	var sum int64
	if err := e.pool.QueryRow(e.ctx, `SELECT COALESCE(sum(amount), 0) FROM wallet_ledger WHERE user_id = $1 AND kind = $2`, userID, kind).Scan(&sum); err != nil {
		e.t.Fatal(err)
	}
	return sum
}

func (e *env) events(order string) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT kind FROM referral_events WHERE order_id = $1 ORDER BY id`, order)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

func (e *env) reward(order string) map[string]any {
	e.t.Helper()
	var status string
	var reward, cap, settled, clawed, refunded int64
	var settleAfter time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT status, reward, reward_cap, settled_amount, clawed_amount, refunded_amount, settle_after FROM referral_rewards WHERE order_id = $1`, order).
		Scan(&status, &reward, &cap, &settled, &clawed, &refunded, &settleAfter); err != nil {
		e.t.Fatalf("reward %s: %v", order, err)
	}
	return map[string]any{"status": status, "reward": reward, "cap": cap, "settled": settled, "clawed": clawed, "refunded": refunded, "settle_after": settleAfter}
}

func (e *env) debt(userID int64) int64 {
	e.t.Helper()
	var d int64
	if err := e.pool.QueryRow(e.ctx, `SELECT debt FROM referral_accounts WHERE user_id = $1`, userID).Scan(&d); err != nil {
		e.t.Fatal(err)
	}
	return d
}
