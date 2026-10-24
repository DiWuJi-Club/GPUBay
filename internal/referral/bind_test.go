package referral_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/referral"
)

func (e *env) doBind(req referral.BindRequest) referral.BindResult {
	e.t.Helper()
	var res referral.BindResult
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		res, err = e.svc.Bind(e.ctx, tx, req)
		return err
	}); err != nil {
		e.t.Fatalf("bind: %v", err)
	}
	return res
}

func (e *env) bindingRow(invitee int64) (status, hold string, flags []string) {
	e.t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT status, hold_reason, risk_flags FROM referral_bindings WHERE invitee_id = $1`, invitee).Scan(&status, &hold, &flags); err != nil {
		e.t.Fatalf("binding row: %v", err)
	}
	return
}

// bindExec / bindCode：bind 测试的前置写入，失败即 Fatal。
func (e *env) bindExec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *env) bindCode(userID int64) string {
	e.t.Helper()
	a, err := e.svc.EnsureCode(e.ctx, userID)
	if err != nil {
		e.t.Fatalf("EnsureCode: %v", err)
	}
	return a.Code
}

type bindAudit struct {
	ActorType, TargetType, TargetID, IP string
	ActorID                             int64
	Diff                                struct {
		InviterID int64    `json:"inviter_id"`
		Code      string   `json:"code"`
		Flags     []string `json:"flags"`
		Status    string   `json:"status"`
	}
}

// bindAuditOf 返回被邀请人那条绑定对应的 referral.bind 审计（按 target 精确定位）。
func (e *env) bindAuditOf(invitee int64) bindAudit {
	e.t.Helper()
	var a bindAudit
	var diff []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT l.actor_type, l.actor_id, l.target_type, l.target_id, l.ip, l.diff FROM audit_logs l
		JOIN referral_bindings b ON l.target_type = 'referral_binding' AND l.target_id = b.id::text
		WHERE l.action = 'referral.bind' AND b.invitee_id = $1`, invitee).Scan(&a.ActorType, &a.ActorID, &a.TargetType, &a.TargetID, &a.IP, &diff); err != nil {
		e.t.Fatalf("bind audit: %v", err)
	}
	if err := json.Unmarshal(diff, &a.Diff); err != nil {
		e.t.Fatalf("audit diff %s: %v", diff, err)
	}
	return a
}

func TestBindHappyPathAndReasons(t *testing.T) {
	e := newEnv(t)
	inviter := e.user("13812345678", true)
	code := e.bindCode(inviter)
	now := e.clk.Now()
	b := e.user("13900000001", false)
	res := e.doBind(referral.BindRequest{Code: " " + code + " ", InviteeID: b, IP: "1.1.1.1", UserAgent: "ua", Now: now})
	if !res.Bound || res.Reason != "ok" || res.Inviter != "138****5678" {
		t.Fatalf("res = %+v", res)
	}
	status, hold, flags := e.bindingRow(b)
	if status != "active" || hold != "" || len(flags) != 0 {
		t.Fatalf("row = %s %q %v", status, hold, flags)
	}
	var until time.Time
	var ver, bindingID int64
	if err := e.pool.QueryRow(e.ctx, `SELECT id, rebate_until, rate_version_id FROM referral_bindings WHERE invitee_id = $1`, b).Scan(&bindingID, &until, &ver); err != nil {
		t.Fatal(err)
	}
	if !until.Equal(now.Add(365*24*time.Hour)) || ver == 0 {
		t.Fatalf("rebate_until = %v ver = %d", until, ver)
	}
	au := e.bindAuditOf(b)
	if au.ActorType != "user" || au.ActorID != b || au.TargetType != "referral_binding" || au.TargetID != strconv.FormatInt(bindingID, 10) || au.IP != "1.1.1.1" ||
		au.Diff.InviterID != inviter || au.Diff.Code != code || au.Diff.Status != "active" || au.Diff.Flags == nil || len(au.Diff.Flags) != 0 {
		t.Fatalf("audit = %+v", au)
	}
	// 同一被邀请人再绑：invalid_code，不报错
	if r := e.doBind(referral.BindRequest{Code: code, InviteeID: b, Now: now}); r.Bound || r.Reason != "invalid_code" {
		t.Fatalf("rebind = %+v", r)
	}
	// 码主自己用自己的码：invalid_code
	if r := e.doBind(referral.BindRequest{Code: code, InviteeID: inviter, Now: now}); r.Bound || r.Reason != "invalid_code" {
		t.Fatalf("self = %+v", r)
	}
	// 无效码 / 垃圾码
	for i, c := range []string{"ZZZZZZZZ", "", "abc", "!!!!!!!!"} {
		if r := e.doBind(referral.BindRequest{Code: c, InviteeID: e.user(fmt.Sprintf("1390000%04d", 300+i), false), Now: now}); r.Bound || r.Reason != "invalid_code" {
			t.Fatalf("code %q = %+v", c, r)
		}
	}
	// 冻结 / 封禁 → inviter_frozen
	e.bindExec(`UPDATE referral_accounts SET status = 'frozen' WHERE user_id = $1`, inviter)
	if r := e.doBind(referral.BindRequest{Code: code, InviteeID: e.user("13900000002", false), Now: now}); r.Bound || r.Reason != "inviter_frozen" {
		t.Fatalf("frozen = %+v", r)
	}
	e.bindExec(`UPDATE referral_accounts SET status = 'active' WHERE user_id = $1`, inviter)
	e.bindExec(`UPDATE users SET status = 'banned' WHERE id = $1`, inviter)
	if r := e.doBind(referral.BindRequest{Code: code, InviteeID: e.user("13900000003", false), Now: now}); r.Bound || r.Reason != "inviter_frozen" {
		t.Fatalf("banned = %+v", r)
	}
	// 绑定用绑定时刻的版本：改版本后再绑的人 rebate_until 不同，旧绑定不变
	e.bindExec(`UPDATE users SET status = 'active' WHERE id = $1`, inviter)
	e.bindExec(`INSERT INTO referral_rate_versions (rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, created_at) VALUES (1000, 1, 1, 7, 30, $1, $1)`, now)
	c := e.user("13900000004", false)
	if r := e.doBind(referral.BindRequest{Code: code, InviteeID: c, Now: now}); !r.Bound {
		t.Fatalf("bind c = %+v", r)
	}
	var untilC time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT rebate_until FROM referral_bindings WHERE invitee_id = $1`, c).Scan(&untilC); err != nil {
		t.Fatal(err)
	}
	if !untilC.Equal(now.Add(30 * 24 * time.Hour)) {
		t.Fatalf("new version period = %v", untilC)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT rebate_until FROM referral_bindings WHERE invitee_id = $1`, b).Scan(&until); err != nil {
		t.Fatal(err)
	}
	if !until.Equal(now.Add(365 * 24 * time.Hour)) {
		t.Fatal("old binding period changed")
	}
}

func TestBindRiskFlagsMatrix(t *testing.T) {
	const (
		devA     = "11111111-2222-4333-8444-555555555555" // 码主登录过的设备
		devOther = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" // 别人的设备
	)
	e := newEnv(t)
	inviter := e.user("13812345678", true)
	code := e.bindCode(inviter)
	now := e.clk.Now()
	e.bindExec(`UPDATE users SET last_login_ip = '8.8.8.8', last_login_at = $2 WHERE id = $1`, inviter, now.Add(-23*time.Hour))
	// "dev-a" 也放进假 UserLookup：证明是 Bind 自己把非 UUID 当作没带，而不是 HasDevice 恰好返回 false
	e.users.devices[inviter] = map[string]bool{devA: true, "dev-a": true}

	bindOK := func(req referral.BindRequest) {
		t.Helper()
		if r := e.doBind(req); !r.Bound {
			t.Fatalf("bind %+v = %+v", req, r)
		}
	}
	deviceOf := func(invitee int64) string {
		t.Helper()
		var d string
		if err := e.pool.QueryRow(e.ctx, `SELECT device_id FROM referral_bindings WHERE invitee_id = $1`, invitee).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	// same_ip（24h 内）→ 只标记，active
	b1 := e.user("13900000011", false)
	bindOK(referral.BindRequest{Code: code, InviteeID: b1, IP: "8.8.8.8", Now: now})
	if st, _, flags := e.bindingRow(b1); st != "active" || !slices.Equal(flags, []string{"same_ip"}) {
		t.Fatalf("same_ip = %s %v", st, flags)
	}
	// 同 IP 但超过 24h → 不标记
	e.bindExec(`UPDATE users SET last_login_at = $2 WHERE id = $1`, inviter, now.Add(-25*time.Hour))
	b2 := e.user("13900000012", false)
	bindOK(referral.BindRequest{Code: code, InviteeID: b2, IP: "8.8.8.8", Now: now})
	if _, _, flags := e.bindingRow(b2); len(flags) != 0 {
		t.Fatalf("stale ip = %v", flags)
	}
	// same_device（大写 UUID 也认）→ held，审计里 status / flags 一致
	b3 := e.user("13900000013", false)
	bindOK(referral.BindRequest{Code: code, InviteeID: b3, DeviceID: strings.ToUpper(devA), Now: now})
	if st, hold, flags := e.bindingRow(b3); st != "held" || hold != "same_device" || !slices.Equal(flags, []string{"same_device"}) || deviceOf(b3) != devA {
		t.Fatalf("same_device = %s %q %v %q", st, hold, flags, deviceOf(b3))
	}
	if au := e.bindAuditOf(b3); au.Diff.Status != "held" || !slices.Equal(au.Diff.Flags, []string{"same_device"}) {
		t.Fatalf("held audit = %+v", au)
	}
	// 带了设备 id 但不是码主的设备 → 不标记，照存
	b4 := e.user("13900000014", false)
	bindOK(referral.BindRequest{Code: code, InviteeID: b4, DeviceID: devOther, Now: now})
	if st, _, flags := e.bindingRow(b4); st != "active" || len(flags) != 0 || deviceOf(b4) != devOther {
		t.Fatalf("other device = %s %v %q", st, flags, deviceOf(b4))
	}
	// same_ip + same_device 同时命中 → 两个标记，held
	e.bindExec(`UPDATE users SET last_login_at = $2 WHERE id = $1`, inviter, now.Add(-time.Hour))
	b5 := e.user("13900000015", false)
	bindOK(referral.BindRequest{Code: code, InviteeID: b5, IP: "8.8.8.8", DeviceID: devA, Now: now})
	if st, hold, flags := e.bindingRow(b5); st != "held" || hold != "same_device" || !slices.Equal(flags, []string{"same_ip", "same_device"}) {
		t.Fatalf("ip+device = %s %q %v", st, hold, flags)
	}
	if au := e.bindAuditOf(b5); au.Diff.Status != "held" || !slices.Equal(au.Diff.Flags, []string{"same_ip", "same_device"}) {
		t.Fatalf("ip+device audit = %+v", au)
	}
	// 非 UUID 设备 id：视同没带（不查设备、不落库）
	b6 := e.user("13900000016", false)
	bindOK(referral.BindRequest{Code: code, InviteeID: b6, DeviceID: "dev-a", Now: now})
	if st, _, flags := e.bindingRow(b6); st != "active" || len(flags) != 0 || deviceOf(b6) != "" {
		t.Fatalf("non-uuid device = %s %v %q", st, flags, deviceOf(b6))
	}
	// burst：已有 6 条，再造到第 19 条不标记，第 20、21 条标记（计数有 LIMIT 封顶，超过阈值仍标记）
	for i := 7; i <= 19; i++ {
		u := e.user(fmt.Sprintf("1390000%04d", 100+i), false) // 与 b1..b6、b20..b22 不重号（preflight Q2）
		bindOK(referral.BindRequest{Code: code, InviteeID: u, Now: now})
		if _, _, flags := e.bindingRow(u); len(flags) != 0 {
			t.Fatalf("binding #%d flagged %v", i, flags)
		}
	}
	for _, phone := range []string{"13900000120", "13900000121"} {
		u := e.user(phone, false)
		bindOK(referral.BindRequest{Code: code, InviteeID: u, Now: now})
		if st, _, flags := e.bindingRow(u); st != "active" || !slices.Equal(flags, []string{"burst"}) {
			t.Fatalf("burst %s = %s %v", phone, st, flags)
		}
	}
	// 窗口之后的绑定不再算 burst
	e.clk.Advance(referral.BurstWindow + time.Hour)
	b22 := e.user("13900000122", false)
	bindOK(referral.BindRequest{Code: code, InviteeID: b22, Now: e.clk.Now()})
	if _, _, flags := e.bindingRow(b22); len(flags) != 0 {
		t.Fatalf("after window = %v", flags)
	}
}

func TestSanitizeUA(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"Mozilla/5.0", "Mozilla/5.0"},
		{"a\xffb\xc3", "ab"}, // 非法 UTF-8 字节丢弃
		{"a\x00b", "ab"},     // NUL：合法 UTF-8 但 PG TEXT 不收
		{strings.Repeat("a", 250), strings.Repeat("a", referral.MaxUserAgent)},
		{strings.Repeat("a", 199) + "中", strings.Repeat("a", 199)}, // 多字节字符跨截断点：整字丢弃
		{strings.Repeat("中", 100), strings.Repeat("中", 66)},        // 198 字节
	}
	for _, c := range cases {
		got := referral.SanitizeUA(c.in)
		if got != c.want || !utf8.ValidString(got) || len(got) > referral.MaxUserAgent {
			t.Errorf("SanitizeUA(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 坏 User-Agent / 坏邀请码都不能让注册事务报错（R7）。
func TestBindBadUserAgentAndGarbageCode(t *testing.T) {
	e := newEnv(t)
	inviter := e.user("13812345678", true)
	code := e.bindCode(inviter)
	now := e.clk.Now()

	// 非法 UTF-8 + NUL + 截断点上的多字节字符：照常绑定，存的是清洗后的值
	b := e.user("13900000001", false)
	ua := "\xff\x00" + strings.Repeat("a", 198) + "中\xfe"
	if r := e.doBind(referral.BindRequest{Code: code, InviteeID: b, UserAgent: ua, Now: now}); !r.Bound || r.Reason != referral.ReasonOK {
		t.Fatalf("bad ua = %+v", r)
	}
	var stored string
	if err := e.pool.QueryRow(e.ctx, `SELECT user_agent FROM referral_bindings WHERE invitee_id = $1`, b).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != strings.Repeat("a", 198) {
		t.Fatalf("stored ua = %q", stored)
	}

	// 非法 UTF-8 / 控制字符的邀请码：invalid_code，不报错（doBind 遇 error 会 Fatal）
	for i, c := range []string{"\xff\xfe\xfd\xfc\xfb\xfa\xf9\xf8", code[:7] + "\xff", "\x00\x00\x00\x00\x00\x00\x00\x00", code + "\xc3", "中文邀请码中文邀请"} {
		u := e.user(fmt.Sprintf("1390000%04d", 200+i), false)
		if r := e.doBind(referral.BindRequest{Code: c, InviteeID: u, UserAgent: "\xff", Now: now}); r.Bound || r.Reason != referral.ReasonInvalidCode {
			t.Fatalf("code %q = %+v", c, r)
		}
	}
}
