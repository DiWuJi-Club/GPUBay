package app_test

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
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/sms"
)

const bookingPhone = "13800009999"

type bookingEnv struct {
	t        *testing.T
	ctx      context.Context
	a        *app.App
	pool     *pgxpool.Pool
	clk      *clock.Fake
	sms      *sms.Mock
	c        *client
	machines []int64
	products []any
	images   []any
}

func newBookingEnv(t *testing.T, machines int) *bookingEnv {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	smsMock := &sms.Mock{}
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock"}
	a, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Clock: clk, SMS: smsMock})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	e := &bookingEnv{t: t, ctx: ctx, a: a, pool: pool, clk: clk, sms: smsMock, c: c}
	for i := 0; i < machines; i++ {
		e.machines = append(e.machines, dbtest.CreateMachine(t, pool, fmt.Sprintf("SN%03d", i+1)))
	}
	e.login()
	c.do("POST", "/kyc/personal", map[string]string{"name": "李四", "id_number": "110101199001011234"})
	rch := c.do("POST", "/recharges", map[string]any{"amount": 1_000_000, "channel": "wechat"})
	c.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{})
	e.products = c.do("GET", "/products", nil)["items"].([]any)
	e.images = c.do("GET", "/images", nil)["items"].([]any)
	return e
}

// login 发验证码并登录（会话 30 天过期：假时钟推进超过它之后要重新登录）。
func (e *bookingEnv) login() {
	e.t.Helper()
	e.c.do("POST", "/auth/sms-code", map[string]string{"phone": bookingPhone})
	e.c.do("POST", "/auth/login", map[string]string{"phone": bookingPhone, "code": "000000"})
}

// smsCount 按模板统计已发短信条数。
func (e *bookingEnv) smsCount(template string) int {
	n := 0
	for _, m := range e.sms.Sent() {
		if m.Template == template {
			n++
		}
	}
	return n
}

// agent 模拟全部机器的 Agent：心跳并成功执行待处理指令。
func (e *bookingEnv) agent() {
	e.t.Helper()
	for _, mid := range e.machines {
		if _, err := e.a.Fleet.Heartbeat(e.ctx, mid); err != nil {
			e.t.Fatal(err)
		}
		e.ackOn(mid, true, "ok")
	}
}

// ackOn 只回报一台机器上的待处理指令。
func (e *bookingEnv) ackOn(mid int64, ok bool, result string) {
	e.t.Helper()
	cmds, err := e.a.Fleet.PendingCommands(e.ctx, mid)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, cmd := range cmds {
		if err := e.a.Rental.HandleCommandResult(e.ctx, cmd.ID, ok, result); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *bookingEnv) sweep() {
	e.t.Helper()
	for _, mid := range e.machines {
		if _, err := e.a.Fleet.Heartbeat(e.ctx, mid); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := e.a.Rental.Sweep(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *bookingEnv) bookingBody(start time.Time, extra map[string]any) map[string]any {
	body := map[string]any{"product_id": e.products[0].(map[string]any)["id"], "image_id": e.images[0].(map[string]any)["id"],
		"count": 1, "duration": "1d", "quantity": 1, "start_at": start.Format(time.RFC3339)}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (e *bookingEnv) state(id string) map[string]any { return e.c.do("GET", "/instances/"+id, nil) }

// TestBookingLifecycleHTTP：预订 → 到点开通 → 运行（到期 = 开始 + 1d）→ 自动续费 → 同机下一预订挡住自动续费（短信）→ 到期停机 → 保留期洗机 → 下一预订按时开通。
func TestBookingLifecycleHTTP(t *testing.T) {
	e := newBookingEnv(t, 1)
	g, err := e.a.Fleet.CreateGroup(e.ctx, audit.Actor{AdminID: 1}, fleet.GroupInput{Name: "A 区", Visible: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.Fleet.AssignMachines(e.ctx, audit.Actor{AdminID: 1}, g.ID, e.machines); err != nil {
		t.Fatal(err)
	}
	start := dbtest.T0.Add(3 * time.Hour)
	out := e.c.do("POST", "/bookings", e.bookingBody(start, map[string]any{"group_id": g.ID, "auto_renew": true, "auto_renew_duration": "1m"}))
	id := out["instances"].([]any)[0].(map[string]any)["id"].(string)
	if st := e.state(id); st["state"] != "reserved" || st["group_name"] != "A 区" {
		t.Fatalf("reserved view = %v", st)
	}
	e.clk.Advance(2 * time.Hour)
	e.sweep()
	if st := e.state(id); st["state"] != "creating" {
		t.Fatalf("after T-1h sweep = %v", st)
	}
	e.clk.Advance(5 * time.Minute)
	e.agent()
	st := e.state(id)
	if cmd, _ := st["ssh_command"].(string); st["state"] != "running" || st["remaining_seconds"].(float64) != 86400 || cmd == "" {
		t.Fatalf("after create = %v", st)
	}
	// 到 S：剩余仍是 24h（早开通不计费）
	e.clk.Advance(55 * time.Minute)
	if st := e.state(id); st["remaining_seconds"].(float64) != 86400 {
		t.Fatalf("at S = %v", st)
	}
	// 自动续费：到期前 1h，1m 放得下 → 到期 = S + 1d + 30d
	e.clk.Advance(23 * time.Hour)
	e.sweep()
	st = e.state(id)
	expires1, _ := time.Parse(time.RFC3339, st["expires_at"].(string))
	if !expires1.Equal(start.Add(31 * 24 * time.Hour)) {
		t.Fatalf("after auto renew expires = %v", expires1)
	}
	// 同机紧随其后的预订：S2 = expires1 + 8h → 可续至 = expires1；下一次自动续费全部被挡
	s2 := expires1.Add(8 * time.Hour)
	out2 := e.c.do("POST", "/bookings", e.bookingBody(s2, map[string]any{"group_id": g.ID}))
	id2 := out2["instances"].([]any)[0].(map[string]any)["id"].(string)
	if st := e.state(id); st["renewable_until"] == nil {
		t.Fatalf("renewable_until missing: %v", st)
	}
	e.clk.Advance(30 * 24 * time.Hour) // now = expires1 − 1h：自动续费到点
	e.sweep()
	e.login() // 会话 TTL 30 天，上面一共推进了 30 天多：重新登录（验证码限流窗口早已过去）
	st = e.state(id)
	if st["auto_renew_failed_reason"] != "renew_blocked" || e.smsCount(sms.TemplateAutoRenewFailed) != 1 { // 同一轮可能还有到期提醒短信，按模板计数
		t.Fatalf("blocked auto renew = %v sms=%+v", st, e.sms.Sent())
	}
	// 到期停机 → 6h 保留 → 洗机 → 释放；下一预订在 S2 − 1h 开通
	e.clk.Advance(time.Hour)
	e.sweep()
	e.agent()
	if e.smsCount(sms.TemplateAutoRenewFailed) != 1 { // 10 分钟重试间隔内到期：被挡的自动续费只发过一条短信（规格 S15）
		t.Fatalf("blocked auto renew must notify once, sms=%+v", e.sms.Sent())
	}
	if st := e.state(id); st["state"] != "stopped" {
		t.Fatalf("expired = %v", st)
	}
	e.clk.Advance(6 * time.Hour)
	e.sweep()
	e.agent()
	if st := e.state(id); st["state"] != "released" {
		t.Fatalf("wiped = %v", st)
	}
	e.clk.Advance(time.Hour) // = S2 − 1h
	e.sweep()
	if st := e.state(id2); st["state"] != "creating" {
		t.Fatalf("next booking at T-1h = %v", st)
	}
	e.agent()
	if st := e.state(id2); st["state"] != "running" {
		t.Fatalf("next booking running = %v", st)
	}
}

func TestImmediateOrderDoesNotTakeBookedMachine(t *testing.T) {
	e := newBookingEnv(t, 1)
	e.c.do("POST", "/bookings", e.bookingBody(dbtest.T0.Add(10*time.Hour), nil))
	body := map[string]any{"product_id": e.products[0].(map[string]any)["id"], "duration": "1d", "image_id": e.images[0].(map[string]any)["id"]}
	resp := e.c.raw("POST", "/orders", body)
	if resp["code"] != "no_capacity" {
		t.Fatalf("immediate order with near booking = %v", resp)
	}
	// 预订挪远（40h）后即时订单成功
	id := e.c.do("GET", "/instances", nil)["items"].([]any)[0].(map[string]any)["id"].(string)
	e.c.do("POST", "/instances/"+id+"/cancel", map[string]any{})
	e.c.do("POST", "/bookings", e.bookingBody(dbtest.T0.Add(40*time.Hour), nil))
	if out := e.c.do("POST", "/orders", body); out["instance"].(map[string]any)["state"] != "creating" {
		t.Fatalf("immediate order with far booking = %v", out)
	}
}

// TestBookingReassignChainHTTP（规格 §16.2 改派链路）：指定机器 A 的预订到点开通 → A 上 Create 失败改派 B → B 失败改派 C →
// C 也失败、候选全是 faulty → 回 reserved 钉在 C 上等待；其间 A 的 StateReport 报出该实例的残留容器：
// 实例还有未完成的 Create 时只告警、不入队清理，回到 reserved（无未完成指令）后对账入队孤儿清理，清理成功不改实例状态、A 仍是 faulty；
// 过宽限期 → 平台取消、全额退款、取消短信。
func TestBookingReassignChainHTTP(t *testing.T) {
	e := newBookingEnv(t, 3)
	g, err := e.a.Fleet.CreateGroup(e.ctx, audit.Actor{AdminID: 1}, fleet.GroupInput{Name: "B 区", Visible: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.Fleet.AssignMachines(e.ctx, audit.Actor{AdminID: 1}, g.ID, e.machines); err != nil {
		t.Fatal(err)
	}
	balance := e.c.do("GET", "/wallet", nil)["balance"].(float64)
	a := e.machines[0]
	start := dbtest.T0.Add(3 * time.Hour)
	out := e.c.do("POST", "/bookings", e.bookingBody(start, map[string]any{"group_id": g.ID, "machine_id": a}))
	id := out["instances"].([]any)[0].(map[string]any)["id"].(string)
	if got := e.c.do("GET", "/wallet", nil)["balance"].(float64); got != balance-6900 {
		t.Fatalf("balance after booking = %v", got)
	}
	machineOf := func() int64 {
		t.Helper()
		var mid int64
		if err := e.pool.QueryRow(e.ctx, `SELECT machine_id FROM instances WHERE id = $1`, id).Scan(&mid); err != nil {
			t.Fatal(err)
		}
		return mid
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	reportOrphan := func() {
		t.Helper()
		if err := e.a.Rental.Reconcile(e.ctx, a, fleet.StateReport{Instances: []fleet.InstanceState{{InstanceID: id, Phase: fleet.PhaseRunning}}}); err != nil {
			t.Fatal(err)
		}
	}
	openWipesOnA := func() int {
		return count(`SELECT count(*) FROM agent_commands WHERE machine_id = $1 AND instance_id = $2 AND type = 'wipe_instance' AND status IN ('pending', 'sent')`, a, id)
	}

	e.clk.Advance(2 * time.Hour) // S − 1h：开通
	e.sweep()
	if st := e.state(id); st["state"] != "creating" || machineOf() != a {
		t.Fatalf("provisioned = %v on %d", st, machineOf())
	}
	e.ackOn(a, false, "image pull failed")
	b := machineOf()
	if b == a {
		t.Fatal("create failure on A must reassign")
	}
	if st := e.state(id); st["state"] != "creating" {
		t.Fatalf("after A failure = %v", st)
	}
	// A 上报残留容器：实例在 B 上还有未完成的 Create → 只告警，不入队清理（不和进行中的指令打架）
	reportOrphan()
	if n := count(`SELECT count(*) FROM alerts WHERE kind = 'orphan_container' AND machine_id = $1 AND instance_id = $2`, a, id); n != 1 {
		t.Fatalf("orphan alerts = %d", n)
	}
	if n := openWipesOnA(); n != 0 {
		t.Fatalf("wipe must wait for the open create, got %d", n)
	}

	e.ackOn(b, false, "gpu error")
	c := machineOf()
	if c == a || c == b {
		t.Fatalf("second reassign went to %d (a=%d b=%d)", c, a, b)
	}
	e.ackOn(c, false, "disk error")
	if st := e.state(id); st["state"] != "reserved" || machineOf() != c {
		t.Fatalf("all candidates faulty: %v on %d", st, machineOf())
	}
	for _, mid := range e.machines {
		m, err := e.a.Fleet.Get(e.ctx, e.pool, mid)
		if err != nil || m.State != fleet.StateFaulty {
			t.Fatalf("machine %d = %s, %v", mid, m.State, err)
		}
	}
	if n := count(`SELECT count(*) FROM alerts WHERE kind = 'booking_at_risk' AND instance_id = $1`, id); n != 1 {
		t.Fatalf("at-risk alerts = %d", n)
	}
	if n := count(`SELECT reassign_count FROM instances WHERE id = $1`, id); n != 2 {
		t.Fatalf("reassign_count = %d", n)
	}

	// 回到 reserved 后实例没有未完成指令：A 再报残留容器 → 入队孤儿清理；清理成功不改实例、不释放 A
	reportOrphan()
	if n := openWipesOnA(); n != 1 {
		t.Fatalf("orphan wipe on A = %d", n)
	}
	e.ackOn(a, true, "wiped")
	if n := openWipesOnA(); n != 0 {
		t.Fatalf("orphan wipe still open = %d", n)
	}
	if st := e.state(id); st["state"] != "reserved" || machineOf() != c {
		t.Fatalf("orphan wipe must not touch the booking: %v on %d", st, machineOf())
	}
	if m, _ := e.a.Fleet.Get(e.ctx, e.pool, a); m.State != fleet.StateFaulty {
		t.Fatalf("A after orphan wipe = %s", m.State)
	}

	// 宽限期（S + 30m）内仍等待；过了宽限期 → 平台取消、全额退款、取消短信
	e.clk.Advance(time.Hour + 29*time.Minute)
	e.sweep()
	if st := e.state(id); st["state"] != "reserved" {
		t.Fatalf("within grace = %v", st)
	}
	e.clk.Advance(2 * time.Minute)
	e.sweep()
	if st := e.state(id); st["state"] != "cancelled" {
		t.Fatalf("after grace = %v", st)
	}
	if got := e.c.do("GET", "/wallet", nil)["balance"].(float64); got != balance {
		t.Fatalf("balance after platform cancel = %v, want %v", got, balance)
	}
	if n := e.smsCount(sms.TemplateBookingCancelled); n != 1 {
		t.Fatalf("booking cancelled sms = %d (%+v)", n, e.sms.Sent())
	}
	if n := count(`SELECT count(*) FROM machine_leases WHERE instance_id = $1 AND status IN ('reserved', 'active')`, id); n != 0 {
		t.Fatalf("live leases after cancel = %d", n)
	}
}

// raw 与 do 相同但不因非 2xx 失败，返回响应体。
func (c *client) raw(method, path string, body any) map[string]any {
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
	return out
}
