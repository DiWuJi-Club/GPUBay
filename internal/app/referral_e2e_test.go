package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// X-Device-Id 只认 UUID 形态（identity.uuidRe / referral deviceIDRe），其余视同未提供（裁定 Q7）。
const (
	refDevA = "11111111-2222-4333-8444-555555555555"
	refDevB = "22222222-3333-4444-8555-666666666666"
	refDevC = "33333333-4444-4555-8666-777777777777"
)

type refEnv struct {
	t        *testing.T
	ctx      context.Context
	a        *app.App
	pool     *pgxpool.Pool
	clk      *clock.Fake
	ts       *httptest.Server
	machines []int64
	product  int64
	image    int64
}

func newRefEnv(t *testing.T, machines int) *refEnv {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", PublicBaseURL: "http://spark.test",
		SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock"}
	// 所有登录都来自 127.0.0.1：关掉按 IP 的验证码 / 登录限流（假时钟下窗口永不滑过）。
	a, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Clock: clk, DisableRateLimits: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(ts.Close)
	e := &refEnv{t: t, ctx: context.Background(), a: a, pool: pool, clk: clk, ts: ts, product: dbtest.ProductID(t, pool), image: dbtest.ImageID(t, pool)}
	for i := 0; i < machines; i++ {
		e.machines = append(e.machines, dbtest.CreateMachine(t, pool, fmt.Sprintf("SN%03d", i+1)))
	}
	return e
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// login 发码并登录（可带邀请码与 X-Device-Id 头）；返回已登录客户端与登录响应。
func (e *refEnv) login(phone, inviteCode, deviceID string) (*client, map[string]any) {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &client{t: e.t, base: e.ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	e.clk.Advance(61 * time.Second) // 同一手机号 60 秒内只能发一次码
	c.do("POST", "/auth/sms-code", map[string]string{"phone": phone})
	body := map[string]string{"phone": phone, "code": "000000"}
	if inviteCode != "" {
		body["invite_code"] = inviteCode
	}
	req, _ := http.NewRequest("POST", c.base+"/auth/login", jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	if deviceID != "" {
		req.Header.Set("X-Device-Id", deviceID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("login %s = %d %v", phone, resp.StatusCode, out)
	}
	return c, out
}

func (e *refEnv) verify(c *client, idNumber string) {
	e.t.Helper()
	if out := c.do("POST", "/kyc/personal", map[string]string{"name": "李四", "id_number": idNumber}); out["kyc_status"] != "verified" {
		e.t.Fatalf("kyc = %v", out)
	}
}

func (e *refEnv) recharge(c *client, amount int64) {
	e.t.Helper()
	rch := c.do("POST", "/recharges", map[string]any{"amount": amount, "channel": "wechat"})
	if paid := c.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{}); paid["status"] != "paid" {
		e.t.Fatalf("recharge = %v", paid)
	}
}

func (e *refEnv) rent(c *client) (orderID, instanceID string) {
	e.t.Helper()
	out := c.do("POST", "/orders", map[string]any{"product_id": e.product, "duration": "1d", "image_id": e.image})
	if out["order"] == nil {
		e.t.Fatalf("order = %v", out)
	}
	return out["order"].(map[string]any)["id"].(string), out["instance"].(map[string]any)["id"].(string)
}

// agent 让所有机器心跳并成功执行待处理指令。
func (e *refEnv) agent() {
	e.t.Helper()
	for _, mid := range e.machines {
		if _, err := e.a.Fleet.Heartbeat(e.ctx, mid); err != nil {
			e.t.Fatal(err)
		}
		cmds, err := e.a.Fleet.PendingCommands(e.ctx, mid)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, cmd := range cmds {
			if err := e.a.Rental.HandleCommandResult(e.ctx, cmd.ID, true, "ok"); err != nil {
				e.t.Fatal(err)
			}
		}
	}
}

// settle：Agent 执行指令 → 租用清扫 → 邀请返利清扫（发现 + 发放）。
func (e *refEnv) settle() {
	e.t.Helper()
	e.agent()
	if err := e.a.Rental.Sweep(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	if err := e.a.Referral.Settle(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *refEnv) balance(c *client) int64 {
	return int64(c.do("GET", "/wallet", nil)["balance"].(float64))
}

func (e *refEnv) invariants() {
	e.t.Helper()
	if err := e.a.Referral.CheckInvariants(e.ctx, e.pool); err != nil {
		e.t.Fatal(err)
	}
}

func (e *refEnv) userID(phone string) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM users WHERE phone = $1`, phone).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// scan 跑一条只返回一行的查询（失败即 Fatal）。
func (e *refEnv) scan(sql string, args []any, dest ...any) {
	e.t.Helper()
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(dest...); err != nil {
		e.t.Fatalf("%s %v: %v", sql, args, err)
	}
}

func (e *refEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *refEnv) rewardStatus(order string) (status string, reward int64) {
	e.t.Helper()
	e.scan(`SELECT status, reward FROM referral_rewards WHERE order_id = $1`, []any{order}, &status, &reward)
	return status, reward
}

func (e *refEnv) bindingStatus(inviteePhone string) string {
	e.t.Helper()
	var st string
	e.scan(`SELECT status FROM referral_bindings WHERE invitee_id = $1`, []any{e.userID(inviteePhone)}, &st)
	return st
}

// TestReferralFullChainHTTP：规格 §16.2「全链路」——带码注册绑定 → 实名 → 充值租用 → 计提 → 到期发放（流水备注）
// → 后台部分退款扣回 → 余额不足转欠账 → 下次发放先抵欠账。
func TestReferralFullChainHTTP(t *testing.T) {
	e := newRefEnv(t, 4) // 四笔租用期间洗机尚未释放机器，需要 4 台
	a, _ := e.login("13800000001", "", refDevA)
	e.verify(a, "110101199001011234")
	code := a.do("GET", "/referral/me", nil)["code"].(string)
	b, out := e.login("13900000001", code, refDevB)
	if inv, _ := out["invite"].(map[string]any); inv == nil || inv["bound"] != true || inv["inviter"] != "138****0001" {
		t.Fatalf("bind = %v", out)
	}
	if st := e.bindingStatus("13900000001"); st != "active" {
		t.Fatalf("binding = %s", st)
	}
	e.verify(b, "110101199001011235")
	e.recharge(b, 100_000)
	// 第一笔按默认 7 天延迟验证「待发放 → 到期发放」
	order, _ := e.rent(b)
	e.settle()
	rw := a.do("GET", "/referral/rewards", nil)["items"].([]any)[0].(map[string]any)
	if rw["order_id"] != order || rw["reward"] != float64(690) || rw["status"] != "pending" || rw["invitee_masked"] != "139****0001" {
		t.Fatalf("reward = %v", rw)
	}
	e.clk.Advance(7*24*time.Hour - time.Minute)
	e.settle()
	if e.balance(a) != 0 {
		t.Fatalf("released before hold_days: A balance = %d", e.balance(a))
	}
	e.clk.Advance(time.Minute)
	e.settle()
	if e.balance(a) != 690 {
		t.Fatalf("A balance = %d", e.balance(a))
	}
	led := a.do("GET", "/wallet/ledger", nil)["items"].([]any)[0].(map[string]any)
	if led["kind"] != "referral_rebate" || led["amount"] != float64(690) || led["remark"] != "邀请返利 139****0001 订单 "+order {
		t.Fatalf("ledger = %v", led)
	}
	if me := a.do("GET", "/referral/me", nil); me["stats"].(map[string]any)["invitees"] != float64(1) {
		t.Fatalf("me = %v", me)
	}
	// 之后的扣回场景要在实例仍运行时退款（RefundRelease 只接受 creating / running / stopped 且 amount ≤ 可退上限），
	// 所以把延迟改为 0 天：计提与发放在同一轮 Settle 完成，不再推进时钟。
	e.exec(`UPDATE referral_rate_versions SET hold_days = 0`)
	_, inst2 := e.rent(b)
	e.settle() // 计提 + 发放 → A 690 + 690 = 1380
	if e.balance(a) != 1380 {
		t.Fatalf("A after order2 = %d", e.balance(a))
	}
	// 后台对 B 的实例退 ¥30 → target 390，扣回 300
	if _, err := e.a.Rental.RefundRelease(e.ctx, audit.Actor{AdminID: 1}, inst2, 3000, "测试"); err != nil {
		t.Fatal(err)
	}
	e.settle()
	if e.balance(a) != 1080 {
		t.Fatalf("after clawback A = %d", e.balance(a))
	}
	// 欠账变体：A 先把余额花光（后台调账归零，等价于 A 把返利用于租用），再退 B 的另一台 ¥60 → target 90，excess 600，余额 0 → 欠账 600
	order3, inst3 := e.rent(b)
	e.settle() // +690 → 1770
	if e.balance(a) != 1770 {
		t.Fatalf("A after order3 = %d", e.balance(a))
	}
	uidA := e.userID("13800000001")
	if _, err := e.a.Wallet.AdminAdjust(e.ctx, 1, uidA, -1770, "test: spend all", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Rental.RefundRelease(e.ctx, audit.Actor{AdminID: 1}, inst3, 6000, "退款"); err != nil {
		t.Fatal(err)
	}
	e.settle()
	var debt, clawed int64
	e.scan(`SELECT debt FROM referral_accounts WHERE user_id = $1`, []any{uidA}, &debt)
	e.scan(`SELECT clawed_amount FROM referral_rewards WHERE order_id = $1`, []any{order3}, &clawed)
	if debt != 600 || clawed != 600 || e.balance(a) != 0 {
		t.Fatalf("debt = %d clawed = %d balance = %d", debt, clawed, e.balance(a))
	}
	// B 再租：发放 690 中 600 抵欠账、入账 90；欠账清零
	order4, _ := e.rent(b)
	e.settle()
	var status string
	var settled int64
	e.scan(`SELECT debt FROM referral_accounts WHERE user_id = $1`, []any{uidA}, &debt)
	e.scan(`SELECT status, settled_amount FROM referral_rewards WHERE order_id = $1`, []any{order4}, &status, &settled)
	if debt != 0 || status != "settled" || settled != 690 || e.balance(a) != 90 {
		t.Fatalf("debt offset: debt=%d status=%s settled=%d balance=%d", debt, status, settled, e.balance(a))
	}
	var rebate int64
	e.scan(`SELECT amount FROM wallet_ledger WHERE user_id = $1 AND kind = 'referral_rebate' AND ref_id = $2`, []any{uidA, order4}, &rebate)
	if rebate != 90 {
		t.Fatalf("order4 rebate ledger = %d, want 90 (600 offset debt)", rebate)
	}
	e.invariants()
}

// TestReferralBookingCancelAndCreateFailure：预订 → settle_after = 开始时刻 → 冷静期取消全退 → 到开始时刻 void、A 无流水；
// 立即开通失败 refundAll → reward 0 → 到期 void。
func TestReferralBookingCancelAndCreateFailure(t *testing.T) {
	e := newRefEnv(t, 2)
	a, _ := e.login("13800000001", "", "")
	e.verify(a, "110101199001011234")
	code := a.do("GET", "/referral/me", nil)["code"].(string)
	b, _ := e.login("13900000001", code, "")
	e.verify(b, "110101199001011235")
	e.recharge(b, 100_000)
	uidB := e.userID("13900000001")
	// settle_after = max(paid + hold_days(7d), start)：开始在 10 天后才会取到 start（裁定 Q8）
	start := e.clk.Now().Add(10 * 24 * time.Hour).Truncate(time.Hour)
	res, err := e.a.Rental.PlaceBatch(e.ctx, rental.BatchRequest{UserID: uidB, ProductID: e.product, ImageID: e.image, Count: 1, Duration: catalog.D1d, Quantity: 1, StartAt: &start})
	if err != nil {
		t.Fatal(err)
	}
	booked := res.Orders[0].ID
	e.settle()
	var settleAfter time.Time
	e.scan(`SELECT settle_after FROM referral_rewards WHERE order_id = $1`, []any{booked}, &settleAfter)
	if !settleAfter.Equal(start) {
		t.Fatalf("booking settle_after = %v want %v", settleAfter, start)
	}
	if st, rw := e.rewardStatus(booked); st != "pending" || rw != 690 {
		t.Fatalf("booking reward = %s %d", st, rw)
	}
	if _, err := e.a.Rental.Cancel(e.ctx, uidB, res.Instances[0].ID, "127.0.0.1"); err != nil { // 15 分钟冷静期内 100%
		t.Fatal(err)
	}
	e.settle()
	if st, rw := e.rewardStatus(booked); st != "pending" || rw != 0 {
		t.Fatalf("after cancel = %s %d, want pending 0", st, rw)
	}
	e.clk.Advance(10*24*time.Hour + time.Hour)
	e.settle()
	if st, _ := e.rewardStatus(booked); st != "void" || e.balance(a) != 0 {
		t.Fatalf("cancelled booking: status=%s balance=%d", st, e.balance(a))
	}
	var rebates int
	e.scan(`SELECT count(*) FROM wallet_ledger WHERE user_id = $1 AND kind IN ('referral_rebate', 'referral_clawback')`, []any{e.userID("13800000001")}, &rebates)
	if rebates != 0 {
		t.Fatalf("A has %d referral ledger rows", rebates)
	}
	// 开通失败：Create 回报失败 → refundAll（无后缀）
	b, _ = e.login("13900000001", "", "") // 会话仍有效，重登只为不依赖 30 天会话期
	order, inst := e.rent(b)
	failed := 0
	for _, mid := range e.machines {
		if _, err := e.a.Fleet.Heartbeat(e.ctx, mid); err != nil {
			t.Fatal(err)
		}
		cmds, err := e.a.Fleet.PendingCommands(e.ctx, mid)
		if err != nil {
			t.Fatal(err)
		}
		for _, cmd := range cmds {
			if cmd.Type != fleet.CmdCreateInstance || cmd.InstanceID != inst {
				continue
			}
			if err := e.a.Rental.HandleCommandResult(e.ctx, cmd.ID, false, "disk full"); err != nil {
				t.Fatal(err)
			}
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("create commands failed = %d", failed)
	}
	e.settle()
	if st, rw := e.rewardStatus(order); st != "pending" || rw != 0 {
		t.Fatalf("create failed reward = %s %d (instance %s)", st, rw, inst)
	}
	e.clk.Advance(8 * 24 * time.Hour)
	e.settle()
	if st, _ := e.rewardStatus(order); st != "void" || e.balance(a) != 0 {
		t.Fatalf("create failed status = %s balance = %d", st, e.balance(a))
	}
	e.invariants()
}

// TestReferralSelfDealingHeldAndRevoked：同设备 → held → 到期不发放 → approve → 发放；
// 同身份证 → revoked，其待发放返利 void（裁定 Q16）。
func TestReferralSelfDealingHeldAndRevoked(t *testing.T) {
	e := newRefEnv(t, 3)
	a, _ := e.login("13800000001", "", refDevA)
	e.verify(a, "110101199001011234")
	code := a.do("GET", "/referral/me", nil)["code"].(string)
	d, out := e.login("13900000004", code, refDevA) // 与 A 同设备
	if inv, _ := out["invite"].(map[string]any); inv == nil || inv["bound"] != true {
		t.Fatalf("same device must still bind: %v", out)
	}
	var hold string
	var flags []string
	e.scan(`SELECT status, hold_reason, risk_flags FROM referral_bindings WHERE invitee_id = $1`, []any{e.userID("13900000004")}, new(string), &hold, &flags)
	if st := e.bindingStatus("13900000004"); st != "held" || hold != "same_device" {
		t.Fatalf("binding = %s hold=%s flags=%v", st, hold, flags)
	}
	e.verify(d, "110101199001011236")
	e.recharge(d, 100_000)
	order, _ := e.rent(d)
	e.settle()
	e.clk.Advance(8 * 24 * time.Hour)
	e.settle()
	if st, rw := e.rewardStatus(order); st != "pending" || rw != 690 || e.balance(a) != 0 {
		t.Fatalf("held must not release: %s %d balance %d", st, rw, e.balance(a))
	}
	var bid int64
	e.scan(`SELECT id FROM referral_bindings WHERE invitee_id = $1`, []any{e.userID("13900000004")}, &bid)
	if err := e.a.Referral.Approve(e.ctx, audit.Actor{AdminID: 1}, bid, "ok"); err != nil {
		t.Fatal(err)
	}
	var reviewed bool
	e.scan(`SELECT reviewed_at IS NOT NULL FROM referral_bindings WHERE id = $1`, []any{bid}, &reviewed)
	if st := e.bindingStatus("13900000004"); st != "active" || !reviewed {
		t.Fatalf("after approve binding = %s reviewed=%v", st, reviewed)
	}
	e.settle()
	if e.balance(a) != 690 {
		t.Fatalf("after approve = %d", e.balance(a))
	}
	if st, _ := e.rewardStatus(order); st != "settled" {
		t.Fatalf("approved reward = %s", st)
	}
	// C 带 A 的码注册（另一台设备 → active），用 A 的身份证号实名 → revoked
	c, _ := e.login("13900000005", code, refDevC)
	if st := e.bindingStatus("13900000005"); st != "active" {
		t.Fatalf("C binding = %s", st)
	}
	e.verify(c, "110101199001011234") // 与 A 同一身份证号（mock 核验只看格式）
	if st := e.bindingStatus("13900000005"); st != "revoked" {
		t.Fatalf("same identity = %s", st)
	}
	// 撤销后 C 的消费不再计提：作废事件、无返利行
	e.recharge(c, 100_000)
	orderC, _ := e.rent(c)
	e.settle()
	var kind string
	e.scan(`SELECT kind || ':' || detail FROM referral_events WHERE order_id = $1`, []any{orderC}, &kind)
	if kind != "void:binding revoked" {
		t.Fatalf("revoked invitee order event = %s", kind)
	}
	// 同身份撤销作废已有的待发放返利（裁定 Q16）：下单要求实名，被邀请人实名前不可能有返利，
	// 所以走「被邀请人先实名、邀请人后实名」：E 未实名就拿到码，F 绑定、实名、租用 → 待发放；E 用 F 的身份证号实名 → F 撤销、待发放 void。
	ec, _ := e.login("13800000002", "", "")
	codeE := ec.do("GET", "/referral/me", nil)["code"].(string)
	f, _ := e.login("13900000006", codeE, "")
	e.verify(f, "110101199001011237")
	e.recharge(f, 100_000)
	orderF, _ := e.rent(f)
	e.settle()
	if st, rw := e.rewardStatus(orderF); st != "pending" || rw != 690 {
		t.Fatalf("F reward before inviter kyc = %s %d", st, rw)
	}
	e.verify(ec, "110101199001011237") // 邀请人与被邀请人同一身份证号
	if st := e.bindingStatus("13900000006"); st != "revoked" {
		t.Fatalf("inviter-side same identity = %s", st)
	}
	if st, _ := e.rewardStatus(orderF); st != "void" {
		t.Fatalf("same identity pending reward = %s, want void", st)
	}
	e.clk.Advance(8 * 24 * time.Hour)
	e.settle()
	if e.balance(a) != 690 || e.balance(ec) != 0 {
		t.Fatalf("revoked invitees paid out: A = %d, E = %d", e.balance(a), e.balance(ec))
	}
	e.invariants()
}

// TestReferralCapsAndPeriod：被邀请人上限 1000 → 690、310、0（capped）；返利期 1 天 → 第 2 天 capped(period_over)；
// 邀请人 30 天上限 1500 → 打满后 capped，推进 31 天后恢复（裁定 Q16）。
func TestReferralCapsAndPeriod(t *testing.T) {
	e := newRefEnv(t, 8)
	a, _ := e.login("13800000001", "", "")
	e.verify(a, "110101199001011234")
	code := a.do("GET", "/referral/me", nil)["code"].(string)
	// 绑定前把版本改成返利期 1 天、被邀请人上限 1000、邀请人 30 天上限 1500（返利期在绑定时写入 rebate_until）
	e.exec(`UPDATE referral_rate_versions SET period_days = 1, max_per_invitee = 1000, max_per_inviter_30d = 1500`)
	b, _ := e.login("13900000001", code, "")
	e.verify(b, "110101199001011235")
	e.recharge(b, 1_000_000)
	o1, _ := e.rent(b)
	o2, _ := e.rent(b)
	o3, _ := e.rent(b)
	e.settle()
	_, r1 := e.rewardStatus(o1)
	_, r2 := e.rewardStatus(o2)
	_, r3 := e.rewardStatus(o3)
	if r1 != 690 || r2 != 310 || r3 != 0 {
		t.Fatalf("caps = %d %d %d", r1, r2, r3)
	}
	var capped int
	e.scan(`SELECT count(*) FROM referral_events WHERE kind = 'capped' AND order_id IN ($1, $2)`, []any{o2, o3}, &capped)
	if capped != 2 {
		t.Fatalf("capped events = %d", capped)
	}
	// 返利期：第 2 天的消费 period_over
	e.clk.Advance(25 * time.Hour)
	e.settle()
	o4, _ := e.rent(b)
	e.settle()
	var kind string
	e.scan(`SELECT kind || ':' || detail FROM referral_events WHERE order_id = $1`, []any{o4}, &kind)
	if kind != "capped:period_over" {
		t.Fatalf("period over = %s", kind)
	}
	var n int
	e.scan(`SELECT count(*) FROM referral_rewards WHERE order_id = $1`, []any{o4}, &n)
	if n != 0 {
		t.Fatal("period_over must not create a reward row")
	}
	// B 的 690 + 310 到期发放（o3 = 0 → void）：邀请人 30 天内已发放 1000
	e.clk.Advance(7 * 24 * time.Hour)
	e.settle()
	a, _ = e.login("13800000001", "", "")
	if bal := e.balance(a); bal != 1000 {
		t.Fatalf("A after B's rewards = %d", bal)
	}
	if st, _ := e.rewardStatus(o3); st != "void" {
		t.Fatalf("o3 = %s", st)
	}
	// 30 天上限：C 绑定时返利期改回 60 天、被邀请人上限放开，只剩邀请人 30 天上限起作用：C 的第一笔只剩 500 的额度，第二笔 0
	e.exec(`UPDATE referral_rate_versions SET period_days = 60, max_per_invitee = 50000`)
	c, _ := e.login("13900000002", code, "")
	e.verify(c, "110101199001011236")
	e.recharge(c, 1_000_000)
	o5, _ := e.rent(c)
	o6, _ := e.rent(c)
	e.settle()
	_, r5 := e.rewardStatus(o5)
	_, r6 := e.rewardStatus(o6)
	if r5 != 500 || r6 != 0 {
		t.Fatalf("30d cap = %d %d", r5, r6)
	}
	var detail string
	e.scan(`SELECT detail FROM referral_events WHERE order_id = $1 AND kind = 'capped'`, []any{o6}, &detail)
	if detail != "cap" {
		t.Fatalf("o6 capped detail = %q", detail)
	}
	e.clk.Advance(7 * 24 * time.Hour)
	e.settle()
	a, _ = e.login("13800000001", "", "")
	if bal := e.balance(a); bal != 1500 {
		t.Fatalf("A after 30d cap = %d", bal)
	}
	// 31 天前一刻仍在窗口内：再租 → 0
	o7, _ := e.rent(c)
	e.settle()
	if _, r7 := e.rewardStatus(o7); r7 != 0 {
		t.Fatalf("inside 30d window reward = %d", r7)
	}
	// 推进 31 天：所有已发放都滑出 30 天窗口 → 恢复全额计提并发放
	e.clk.Advance(31 * 24 * time.Hour)
	e.settle()
	c, _ = e.login("13900000002", "", "") // 会话 30 天过期，重新登录
	o8, _ := e.rent(c)
	e.settle()
	if st, r8 := e.rewardStatus(o8); st != "pending" || r8 != 690 {
		t.Fatalf("after 31 days reward = %s %d, want pending 690", st, r8)
	}
	e.clk.Advance(7 * 24 * time.Hour)
	e.settle()
	a, _ = e.login("13800000001", "", "")
	if bal := e.balance(a); bal != 1500+690 {
		t.Fatalf("A after recovery = %d", bal)
	}
	e.invariants()
}

// isDeadlock 报告错误链里是否有 PG 死锁（40P01）/ 序列化失败（40001）。
func isDeadlock(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "40P01" || pe.Code == "40001")
}

// TestReferralConcurrentFundsConsistent：1 邀请人、5 被邀请人，50 个 goroutine 随机下单 / 续费 / 后台退款 / 预订 + 取消 /
// Agent 回报（含开通失败）/ 租用清扫 / 返利清扫 / 冻结解冻 / 推进时钟；每个种子 60 秒内完成。
// 静止后不变量（含第 6 条返利公式）全部成立、余额 ≥ 0、每条 order 流水恰处理一次，且没有任何死锁 / 非业务错误漏给调用方。
func TestReferralConcurrentFundsConsistent(t *testing.T) {
	for _, base := range []int64{1, 2, 3, 4, 5} {
		t.Run(fmt.Sprintf("seed%d", base), func(t *testing.T) { runConcurrentFunds(t, base) })
	}
}

func runConcurrentFunds(t *testing.T, base int64) {
	e := newRefEnv(t, 30)
	a, _ := e.login("13800000001", "", "")
	e.verify(a, "110101199001011234")
	code := a.do("GET", "/referral/me", nil)["code"].(string)
	uidA := e.userID("13800000001")
	e.recharge(a, 50_000)
	e.exec(`UPDATE referral_rate_versions SET hold_days = 1`)
	var invitees []int64
	for i := 1; i <= 5; i++ {
		phone := fmt.Sprintf("1390000000%d", i)
		c, _ := e.login(phone, code, "")
		e.verify(c, fmt.Sprintf("11010119900101%04d", 1300+i))
		e.recharge(c, 5_000_000)
		invitees = append(invitees, e.userID(phone))
	}
	ctx, cancel := context.WithTimeout(e.ctx, 60*time.Second)
	defer cancel()
	// 时钟门：业务操作持读锁，推进时钟持写锁——假时钟不会在一个事务读到 now 与提交之间跳几天
	// （真实时钟下这段间隔远小于发现的 ScanLag + 回看窗口），操作之间仍完全并发。
	var gate sync.RWMutex
	var agentMu sync.Mutex // 每台机器只有一个 Agent：回报串行
	// 租用清扫在生产里互斥（rental.SweeperLockKey 咨询锁）；两个 Sweep 并发时 MarkOfflineStale 的多行 UPDATE 会互相死锁（真实环境不会发生），
	// 所以这里同样互斥。返利清扫 Settle 不互斥：后台重扫 Rescan 会与清扫器里的 Settle 并发。
	var sweepMu sync.Mutex
	var mu sync.Mutex
	var instances []string // 被邀请人的立即开通实例
	pick := func(rng *rand.Rand) string {
		mu.Lock()
		defer mu.Unlock()
		if len(instances) == 0 {
			return ""
		}
		return instances[rng.Intn(len(instances))]
	}
	agent := func(rng *rand.Rand) error {
		agentMu.Lock()
		defer agentMu.Unlock()
		for _, mid := range e.machines {
			if _, err := e.a.Fleet.Heartbeat(ctx, mid); err != nil {
				return err
			}
			cmds, err := e.a.Fleet.PendingCommands(ctx, mid)
			if err != nil {
				return err
			}
			for _, cmd := range cmds {
				ok := cmd.Type != fleet.CmdCreateInstance || rng.Intn(5) != 0 // 20% 开通失败 → refundAll
				if err := e.a.Rental.HandleCommandResult(ctx, cmd.ID, ok, "concurrent"); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var errMu sync.Mutex
	var leaked []string
	var deadlocks int
	okOps, bizErrs := map[string]int{}, map[string]int{} // 覆盖面统计（日志）
	count := func(op string, err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if err == nil {
			okOps[op]++
		} else {
			bizErrs[op+":"+apperr.As(err).Code]++
		}
	}
	report := func(g int64, op string, err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if isDeadlock(err) {
			deadlocks++
			var pe *pgconn.PgError
			errors.As(err, &pe)
			err = fmt.Errorf("%w [detail: %s]", err, pe.Detail)
		}
		leaked = append(leaked, fmt.Sprintf("goroutine %d %s: %v", g, op, err))
	}
	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(g int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(base*1000 + g))
			for i := 0; i < 8; i++ {
				if ctx.Err() != nil {
					report(g, "timeout", ctx.Err())
					return
				}
				uid := invitees[rng.Intn(len(invitees))]
				op := rng.Intn(10)
				if op == 5 {
					gate.Lock()
					e.clk.Advance(time.Duration(rng.Intn(8*24)) * time.Hour)
					gate.Unlock()
					continue
				}
				var err error
				var name string
				gate.RLock()
				switch op {
				case 0, 1:
					name = "place"
					if rng.Intn(5) == 0 {
						// 邀请人把余额花光（后台调账归零，等价于把返利用于租用）：之后的扣回余额不足 → 欠账 → 由下次发放抵扣
						name = "inviter spend"
						var bal int64
						if err = e.pool.QueryRow(ctx, `SELECT balance FROM wallet_accounts WHERE user_id = $1`, uidA).Scan(&bal); err == nil && bal > 0 {
							_, err = e.a.Wallet.AdminAdjust(ctx, 1, uidA, -bal, fmt.Sprintf("spend %d-%d", g, i), "127.0.0.1")
						}
						break
					}
					var inst rental.Instance
					_, inst, err = e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: uid, ProductID: e.product, ImageID: e.image, Duration: catalog.D1d})
					if err == nil {
						mu.Lock()
						instances = append(instances, inst.ID)
						mu.Unlock()
					}
				case 2:
					name = "renew"
					if id := pick(rng); id != "" {
						var owner int64
						if qerr := e.pool.QueryRow(ctx, `SELECT user_id FROM instances WHERE id = $1`, id).Scan(&owner); qerr != nil {
							err = qerr
						} else {
							_, err = e.a.Rental.Renew(ctx, owner, id, catalog.D1d, "")
						}
					}
				case 3:
					name = "refund"
					if id := pick(rng); id != "" {
						_, err = e.a.Rental.RefundRelease(ctx, audit.Actor{AdminID: 1}, id, int64(100+rng.Intn(6800)), "并发测试")
					}
				case 4:
					name = "book"
					start := e.clk.Now().Add(time.Duration(3+rng.Intn(48)) * time.Hour).Truncate(time.Hour)
					var res rental.BatchResult
					res, err = e.a.Rental.PlaceBatch(ctx, rental.BatchRequest{UserID: uid, ProductID: e.product, ImageID: e.image, Count: 1, Duration: catalog.D1d, Quantity: 1, StartAt: &start})
					if err == nil && rng.Intn(2) == 0 {
						name = "cancel"
						_, err = e.a.Rental.Cancel(ctx, uid, res.Instances[0].ID, "127.0.0.1")
					}
				case 6:
					name = "referral settle"
					err = e.a.Referral.Settle(ctx)
				case 7:
					name = "freeze"
					if ferr := e.a.Referral.Freeze(ctx, audit.Actor{AdminID: 1}, uidA, "并发"); ferr == nil {
						name = "unfreeze"
						err = e.a.Referral.Unfreeze(ctx, audit.Actor{AdminID: 1}, uidA)
					} else {
						err = ferr
					}
				case 8:
					name = "agent"
					err = agent(rng)
				case 9:
					name = "rental sweep"
					if sweepMu.TryLock() { // 生产里 RunSweeper 用 pg_try_advisory_lock 保证同一时刻只有一个清扫：拿不到就跳过
						err = e.a.Rental.Sweep(ctx)
						sweepMu.Unlock()
					}
				}
				gate.RUnlock()
				if err != nil && apperr.As(err) == nil { // 业务错误（no_capacity / insufficient_balance / invalid_state / cancel_not_allowed …）可接受
					report(g, name, err)
					return
				}
				count(name, err)
			}
		}(int64(g))
	}
	wg.Wait()
	if len(leaked) > 0 {
		t.Fatalf("%d errors leaked to callers (%d deadlock / serialization):\n%v", len(leaked), deadlocks, leaked)
	}
	// 静止：确保解冻、推进 10 天、Agent + 清扫跑到没有待处理
	if err := e.a.Referral.Unfreeze(e.ctx, audit.Actor{AdminID: 1}, uidA); err != nil && apperr.As(err) == nil {
		t.Fatal(err)
	}
	e.clk.Advance(10 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		e.settle()
	}
	for i := 0; i < 3; i++ {
		if err := e.a.Referral.Settle(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	e.invariants()
	var pending, unprocessed, rewards, settledN, clawbacks int
	e.scan(`SELECT count(*) FROM referral_rewards WHERE status = 'pending' AND settle_after <= $1`, []any{e.clk.Now()}, &pending)
	e.scan(`SELECT count(*) FROM wallet_ledger l JOIN referral_bindings b ON b.invitee_id = l.user_id
		WHERE l.ref_type = 'order' AND NOT EXISTS (SELECT 1 FROM referral_events e WHERE e.source_ledger_id = l.id)`, nil, &unprocessed)
	if pending != 0 || unprocessed != 0 {
		t.Fatalf("not quiescent: due pending=%d unprocessed=%d", pending, unprocessed)
	}
	var negative int
	e.scan(`SELECT count(*) FROM wallet_accounts WHERE balance < 0`, nil, &negative)
	if negative != 0 {
		t.Fatal("negative balance")
	}
	// 覆盖面：并发轮确实产生了计提、发放与扣回（否则这个用例什么也没证明）
	e.scan(`SELECT count(*), count(*) FILTER (WHERE status = 'settled') FROM referral_rewards`, nil, &rewards, &settledN)
	var debtAdds, debtOffsets int
	e.scan(`SELECT count(*) FROM wallet_ledger WHERE kind = 'referral_clawback'`, nil, &clawbacks)
	e.scan(`SELECT count(*) FILTER (WHERE kind = 'debt_add'), count(*) FILTER (WHERE kind = 'debt_offset') FROM referral_events`, nil, &debtAdds, &debtOffsets)
	t.Logf("seed %d: rewards=%d settled=%d clawback ledger rows=%d debt_add=%d debt_offset=%d\nok ops %v\nbusiness errors %v",
		base, rewards, settledN, clawbacks, debtAdds, debtOffsets, okOps, bizErrs)
	if rewards == 0 || settledN == 0 || clawbacks+debtAdds == 0 {
		t.Fatalf("concurrent round did not exercise the money paths: rewards=%d settled=%d clawbacks=%d debt_add=%d", rewards, settledN, clawbacks, debtAdds)
	}
}
