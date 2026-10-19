package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/api"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

func TestReferralCodeLookupAndRateLimit(t *testing.T) {
	e := newTestEnvWith(t, func(s *api.Server) { s.ReferralLimit = 3 })
	owner := e.client()
	owner.login("13812345678")
	_, me := owner.do("GET", "/referral/me", nil)
	code := me["code"].(string)
	if me["link"] != "http://spark.test/?ic="+code || me["status"] != "active" || me["kyc_verified"] != false {
		t.Fatalf("me = %v", me)
	}
	if rebate, _ := me["rebate"].(map[string]any); rebate == nil || rebate["rate_bp"] != float64(1000) || rebate["created_by"] != nil || rebate["note"] != nil {
		t.Fatalf("me.rebate = %v", me["rebate"])
	}
	anon := e.client()
	for i := 0; i < 3; i++ {
		if st, out := anon.do("GET", "/referral/codes/"+strings.ToLower(code), nil); st != 200 || out["valid"] != true || out["inviter"] != "138****5678" {
			t.Fatalf("lookup %d = %d %v", i, st, out)
		}
	}
	if st, out := anon.do("GET", "/referral/codes/"+code, nil); st != 429 || out["code"] != "rate_limited" {
		t.Fatalf("4th = %d %v", st, out)
	}
	e.clk.Advance(time.Minute)
	if st, out := anon.do("GET", "/referral/codes/NOPE!!", nil); st != 200 || out["valid"] != false || out["inviter"] != "" {
		t.Fatalf("garbage = %d %v", st, out)
	}
	// 冻结的码与不存在的码响应完全一致（不区分原因）
	if _, err := e.pool.Exec(context.Background(), `UPDATE referral_accounts SET status = 'frozen' WHERE code = $1`, code); err != nil {
		t.Fatal(err)
	}
	_, frozen := anon.do("GET", "/referral/codes/"+code, nil)
	_, missing := anon.do("GET", "/referral/codes/ZZZZZZZZ", nil)
	if fj, mj := mustJSON(t, frozen), mustJSON(t, missing); fj != mj || frozen["valid"] != false {
		t.Fatalf("frozen = %s, missing = %s", fj, mj)
	}
	if st, _ := anon.do("GET", "/referral/me", nil); st != 401 {
		t.Fatal("me requires login")
	}
	for _, p := range []string{"/referral/invitees", "/referral/rewards"} {
		if st, _ := anon.do("GET", p, nil); st != 401 {
			t.Fatalf("%s requires login", p)
		}
	}
	if st, out := owner.do("GET", "/referral/rewards?status=bogus", nil); st != 400 || out["code"] != "bad_request" {
		t.Fatalf("bogus status = %d %v", st, out)
	}
	if st, out := owner.do("GET", "/referral/invitees?page_size=500", nil); st != 400 {
		t.Fatalf("bad page_size = %d %v", st, out)
	}
	if st, out := owner.do("GET", "/referral/rewards", nil); st != 200 || out["total"] != float64(0) {
		t.Fatalf("rewards = %d %v", st, out)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Review Focus 5 + 规格 §3.2：带码注册 / 老用户 / 垃圾码。
func TestLoginWithInviteCode(t *testing.T) {
	e := newTestEnv(t)
	owner := e.client()
	owner.login("13812345678")
	_, me := owner.do("GET", "/referral/me", nil)
	code := me["code"].(string)
	newbie := e.client()
	newbie.do("POST", "/auth/sms-code", map[string]string{"phone": "13900000001"})
	st, out := newbie.doH("POST", "/auth/login", map[string]string{"phone": "13900000001", "code": "000000", "invite_code": code}, map[string]string{"X-Device-Id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "User-Agent": "spark-test/1.0"})
	inv, _ := out["invite"].(map[string]any)
	if st != 200 || inv == nil || inv["bound"] != true || inv["inviter"] != "138****5678" || out["phone"] != "13900000001" {
		t.Fatalf("new user = %d %v", st, out)
	}
	// IP / 设备 / UA 都传到了 identity 与 Bind
	var device, ua, ip string
	if err := e.pool.QueryRow(context.Background(), `SELECT b.device_id, b.user_agent, b.ip FROM referral_bindings b JOIN users u ON u.id = b.invitee_id
		WHERE u.phone = '13900000001'`).Scan(&device, &ua, &ip); err != nil {
		t.Fatal(err)
	}
	if device != "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" || ua != "spark-test/1.0" || ip != "127.0.0.1" {
		t.Fatalf("binding device=%q ua=%q ip=%q", device, ua, ip)
	}
	_, invitees := owner.do("GET", "/referral/invitees", nil)
	items, _ := invitees["items"].([]any)
	if invitees["total"] != float64(1) || len(items) != 1 {
		t.Fatalf("invitees = %v", invitees)
	}
	if it := items[0].(map[string]any); it["phone_masked"] != "139****0001" || it["status"] != "active" || len(it) != 6 {
		t.Fatalf("invitee item = %v", it) // 只有 6 个字段：不暴露 id / 完整手机号 / 风险标记
	}
	// 老用户带码
	e.clk.Advance(time.Minute)
	newbie.do("POST", "/auth/sms-code", map[string]string{"phone": "13900000001"})
	_, out = newbie.do("POST", "/auth/login", map[string]string{"phone": "13900000001", "code": "000000", "invite_code": code})
	if inv, _ := out["invite"].(map[string]any); inv == nil || inv["bound"] != false || inv["reason"] != "existing_user" {
		t.Fatalf("existing = %v", out)
	}
	// 不带码：无 invite 字段
	e.clk.Advance(time.Minute)
	newbie.do("POST", "/auth/sms-code", map[string]string{"phone": "13900000001"})
	if _, out = newbie.do("POST", "/auth/login", map[string]string{"phone": "13900000001", "code": "000000"}); out["invite"] != nil || out["phone"] != "13900000001" {
		t.Fatalf("no code = %v", out)
	}
	// /me 的 referral_month_settled：0 时省略
	if _, me = owner.do("GET", "/me", nil); me["referral_month_settled"] != nil || me["phone"] != "13812345678" {
		t.Fatalf("me = %v", me)
	}
	// 非 0 时出现（直接写一条本月返利流水）
	ownerID := int64(me["id"].(float64))
	if err := db.WithTx(context.Background(), e.pool, func(tx pgx.Tx) error {
		_, err := e.wallet.Apply(context.Background(), tx, wallet.Entry{UserID: ownerID, Amount: 123, Kind: wallet.KindReferralRebate, RefType: "referral", RefID: "referral/x"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, me := owner.do("GET", "/me", nil); me["referral_month_settled"] != float64(123) {
		t.Fatalf("me = %v", me)
	}
}

// 规格 R7：邀请码 / 请求头再离谱也不能挡注册——注册成功、不绑定、不 500。
func TestLoginWithGarbageInviteCodeStillRegisters(t *testing.T) {
	e := newTestEnv(t)
	owner := e.client()
	owner.login("13812345678")
	_, me := owner.do("GET", "/referral/me", nil)
	code := me["code"].(string)
	cases := []struct {
		name, phone string
		invite      string // invite_code 的 JSON 原文（可含非法 UTF-8）
		headers     map[string]string
		reason      string // "" = 响应不应带 invite
	}{
		{"spaces and punctuation", "13900000011", `"   hello world!!! "`, nil, "invalid_code"},
		{"invalid utf-8", "13900000012", "\"\xff\xfe" + code[:6] + "\"", nil, "invalid_code"},
		{"overlong", "13900000013", `"` + strings.Repeat("A", 1<<16) + `"`, nil, "invalid_code"},
		// 超长但去空白后是有效码：按格式错误处理，不截断 / 规范化成有效码
		{"overlong padded valid code", "13900000014", `"` + code + strings.Repeat(" ", 200) + `"`, nil, "invalid_code"},
		{"garbage headers", "13900000015", `"!!"`, map[string]string{
			"X-Device-Id": "not-a-uuid\xff" + strings.Repeat("z", 4000), "User-Agent": "\xff\xfe" + strings.Repeat("u", 8000)}, "invalid_code"},
		{"empty code, garbage device", "13900000016", `""`, map[string]string{"X-Device-Id": strings.Repeat("-", 36)}, ""},
	}
	for _, tc := range cases {
		phone := tc.phone
		if st, out := e.client().do("POST", "/auth/sms-code", map[string]string{"phone": phone}); st != 200 {
			t.Fatalf("%s: sms = %d %v", tc.name, st, out)
		}
		body := []byte(`{"phone":"` + phone + `","code":"000000","invite_code":` + tc.invite + `}`)
		st, out := postRaw(t, e.ts.URL+"/api/v1/auth/login", body, tc.headers)
		if st != 200 || out["id"] == nil || out["phone"] != phone {
			t.Fatalf("%s: login = %d %v", tc.name, st, out)
		}
		inv, _ := out["invite"].(map[string]any)
		if tc.reason == "" {
			if inv != nil {
				t.Fatalf("%s: invite = %v, want absent", tc.name, inv)
			}
		} else if inv == nil || inv["bound"] != false || inv["reason"] != tc.reason {
			t.Fatalf("%s: invite = %v", tc.name, out["invite"])
		}
		var bindings, devices int
		if err := e.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM referral_bindings b JOIN users u ON u.id = b.invitee_id WHERE u.phone = $1),
			(SELECT count(*) FROM user_devices d JOIN users u ON u.id = d.user_id WHERE u.phone = $1)`, phone).Scan(&bindings, &devices); err != nil {
			t.Fatal(err)
		}
		if bindings != 0 || devices != 0 {
			t.Fatalf("%s: bindings = %d devices = %d", tc.name, bindings, devices)
		}
	}
	if _, invitees := owner.do("GET", "/referral/invitees", nil); invitees["total"] != float64(0) {
		t.Fatalf("invitees = %v", invitees)
	}
}

func postRaw(t *testing.T, url string, body []byte, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s response (status %d): %v", url, resp.StatusCode, err)
	}
	return resp.StatusCode, out
}

// /me 的返利小字查询失败不能拖垮 /me：记日志、省略字段、照常 200。
func TestMeSurvivesMonthSettledFailure(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	c.login("13812345678")
	// 让 MonthSettledFor 的查询报错（每个测试独占一个库）；会话校验不读 wallet_ledger
	if _, err := e.pool.Exec(context.Background(), `ALTER TABLE wallet_ledger RENAME COLUMN created_at TO created_at_broken`); err != nil {
		t.Fatal(err)
	}
	st, me := c.do("GET", "/me", nil)
	if st != 200 || me["phone"] != "13812345678" {
		t.Fatalf("me = %d %v", st, me)
	}
	if _, ok := me["referral_month_settled"]; ok {
		t.Fatalf("me = %v, want referral_month_settled omitted", me)
	}
}

// 计划 8 终审 I1：邀请人只有上线前的旧会话（登录时没带设备头）时，GET /me 会话启动补记设备；
// 之后同设备注册的被邀请人仍被搁置（same_device）。
func TestMeRecordsDeviceForSameDeviceHold(t *testing.T) {
	e := newTestEnv(t)
	const dev = "dddddddd-1111-4222-8333-444444444444"
	owner := e.client()
	owner.login("13812345678") // 旧会话：无 X-Device-Id
	_, me := owner.do("GET", "/referral/me", nil)
	code := me["code"].(string)
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM user_devices d JOIN users u ON u.id = d.user_id WHERE u.phone = '13812345678'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("devices before /me = %d %v", n, err)
	}
	if st, out := owner.doH("GET", "/me", nil, map[string]string{"X-Device-Id": dev}); st != 200 || out["phone"] != "13812345678" {
		t.Fatalf("/me = %d %v", st, out)
	}
	newbie := e.client() // 同一浏览器（同设备 ID）换号注册
	newbie.do("POST", "/auth/sms-code", map[string]string{"phone": "13900000003"})
	st, out := newbie.doH("POST", "/auth/login", map[string]string{"phone": "13900000003", "code": "000000", "invite_code": code}, map[string]string{"X-Device-Id": dev})
	if inv, _ := out["invite"].(map[string]any); st != 200 || inv == nil || inv["bound"] != true {
		t.Fatalf("register = %d %v", st, out)
	}
	var status, hold string
	if err := e.pool.QueryRow(context.Background(), `SELECT b.status, COALESCE(b.hold_reason, '') FROM referral_bindings b JOIN users u ON u.id = b.invitee_id
		WHERE u.phone = '13900000003'`).Scan(&status, &hold); err != nil {
		t.Fatal(err)
	}
	if status != "held" || hold != "same_device" {
		t.Fatalf("binding = %s / %s", status, hold)
	}
}

// 补记失败（这里让 user_devices 不可写）不影响 /me。
func TestMeTouchFailureDoesNotFailMe(t *testing.T) {
	e := newTestEnv(t)
	owner := e.client()
	owner.login("13812345678")
	if _, err := e.pool.Exec(context.Background(), `ALTER TABLE user_devices RENAME TO user_devices_gone`); err != nil {
		t.Fatal(err)
	}
	if st, out := owner.doH("GET", "/me", nil, map[string]string{"X-Device-Id": "dddddddd-1111-4222-8333-444444444444"}); st != 200 || out["phone"] != "13812345678" {
		t.Fatalf("/me = %d %v", st, out)
	}
}
