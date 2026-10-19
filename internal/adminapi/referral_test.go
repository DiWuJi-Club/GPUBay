package adminapi_test

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/platform/dbtest"
)

func TestReferralAdminEndpoints(t *testing.T) {
	e := newEnv(t)
	fin := e.loginAs("fin", admin.RoleFinance)
	sup := e.loginAs("root", admin.RoleSuper)
	ops := e.loginAs("ops", admin.RoleOps)
	inviter := e.renter("13812345678")
	invitee := dbtest.CreateUser(t, e.pool, "13900000001", true)
	acc, err := e.srv.Referral.EnsureCode(e.ctx, inviter)
	if err != nil {
		t.Fatal(err)
	}
	var bid int64
	// device_id 以 = 开头：导出时必须按公式注入转义
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO referral_bindings (inviter_id, invitee_id, code, status, hold_reason, risk_flags, rate_version_id, bound_at, rebate_until, device_id, created_at, updated_at)
		VALUES ($1, $2, $3, 'held', 'same_device', '{same_device}', (SELECT min(id) FROM referral_rate_versions), $4::timestamptz, $4::timestamptz + interval '365 days', '=1+1', $4, $4) RETURNING id`,
		inviter, invitee, acc.Code, e.clk.Now()).Scan(&bid); err != nil {
		t.Fatal(err)
	}

	if code, out := fin.do("GET", "/referral/summary", nil); code != 200 || out["held_count"] != float64(1) {
		t.Fatalf("summary = %d %v", code, out)
	}
	if code, _ := ops.do("GET", "/referral/summary", nil); code != 403 {
		t.Fatal("ops must be forbidden")
	}
	// 配置：super + TOTP；参数在二次验证之前校验（不消耗动态码）；追溯生效被拒
	body := map[string]any{"rate_bp": 1500, "max_per_invitee": 50000, "max_per_inviter_30d": 500000, "hold_days": 7, "period_days": 365, "note": "活动"}
	if code, out := sup.do("POST", "/referral/config", body); code != 403 || out["code"] != "totp_required" {
		t.Fatalf("config without totp = %d %v", code, out)
	}
	totp := sup.totp()
	bad := map[string]any{"rate_bp": 9999, "max_per_invitee": 1, "max_per_inviter_30d": 1, "hold_days": 7, "period_days": 365, "totp": totp}
	if code, out := sup.do("POST", "/referral/config", bad); code != 400 || out["code"] != "referral_config_invalid" {
		t.Fatalf("invalid config = %d %v", code, out)
	}
	if code, out := sup.do("POST", "/referral/config", map[string]any{"rate_bp": 1000, "max_per_invitee": 1, "max_per_inviter_30d": 1, "hold_days": 7, "period_days": 365,
		"effective_from": "tomorrow", "totp": totp}); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("bad effective_from = %d %v", code, out)
	}
	body["totp"] = totp // 上面两次校验失败没有消耗这个动态码
	if code, out := sup.do("POST", "/referral/config", body); code != 200 || out["rate_bp"] != float64(1500) {
		t.Fatalf("config = %d %v", code, out)
	}
	if code, out := sup.do("POST", "/referral/config", body); code != 400 || out["code"] != "totp_used" {
		t.Fatalf("config with reused totp = %d %v", code, out)
	}
	past := map[string]any{"rate_bp": 1000, "max_per_invitee": 1, "max_per_inviter_30d": 1, "hold_days": 7, "period_days": 365, "effective_from": e.clk.Now().Add(-24 * time.Hour).Format(time.RFC3339)}
	if code, out := sup.do("POST", "/referral/config", past); code != 400 || out["code"] != "referral_config_invalid" {
		t.Fatalf("retroactive = %d %v", code, out)
	}
	if code, _ := fin.do("POST", "/referral/config", body); code != 403 {
		t.Fatal("finance cannot set config")
	}
	if code, out := fin.do("GET", "/referral/config", nil); code != 200 || len(out["versions"].([]any)) != 2 || out["current"].(map[string]any)["rate_bp"] != float64(1500) {
		t.Fatalf("get config = %d %v", code, out)
	}
	// 待审核队列
	if code, out := fin.do("GET", "/referral/bindings?status=held", nil); code != 200 || out["total"] != float64(1) ||
		out["items"].([]any)[0].(map[string]any)["kyc_check_unresolved"] != false {
		t.Fatalf("held queue = %d %v", code, out)
	}
	// 审核：approve（finance，无 TOTP）
	if code, _ := fin.do("POST", fmt.Sprintf("/referral/bindings/%d/approve", bid), map[string]string{"note": "ok"}); code != 200 {
		t.Fatal("approve")
	}
	if code, out := fin.do("POST", fmt.Sprintf("/referral/bindings/%d/approve", bid), map[string]string{"note": "ok"}); code != 409 || out["code"] != "referral_binding_state" {
		t.Fatalf("approve twice = %d %v", code, out)
	}
	// 撤销：finance + TOTP；缺原因在二次验证之前拒绝
	if code, out := fin.do("POST", fmt.Sprintf("/referral/bindings/%d/revoke", bid), map[string]string{"reason": "刷单"}); code != 403 || out["code"] != "totp_required" {
		t.Fatalf("revoke without totp = %d %v", code, out)
	}
	ftotp := fin.totp()
	if code, out := fin.do("POST", fmt.Sprintf("/referral/bindings/%d/revoke", bid), map[string]string{"reason": "  ", "totp": ftotp}); code != 400 {
		t.Fatalf("revoke without reason = %d %v", code, out)
	}
	if code, out := fin.do("POST", fmt.Sprintf("/referral/bindings/%d/revoke", bid), map[string]string{"reason": "刷单", "totp": ftotp}); code != 200 || out["voided_amount"] != float64(0) {
		t.Fatalf("revoke = %d %v", code, out)
	}
	// 冻结 / 解冻
	if code, _ := fin.do("POST", fmt.Sprintf("/referral/inviters/%d/freeze", inviter), map[string]string{"reason": "可疑"}); code != 200 {
		t.Fatal("freeze")
	}
	resp, out := fin.send("GET", fmt.Sprintf("/referral/inviters/%d", inviter), nil)
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || out["account"].(map[string]any)["status"] != "frozen" || len(out["bindings"].([]any)) != 1 ||
		out["bindings_total"] != float64(1) || out["rewards_total"] != float64(0) {
		t.Fatalf("detail = %d %v", resp.StatusCode, out)
	}
	if s := strings.ToLower(string(raw)); strings.Contains(s, "fingerprint") || strings.Contains(s, "id_number") {
		t.Fatalf("detail exposes identity data: %s", raw)
	}
	if code, _ := fin.do("POST", fmt.Sprintf("/referral/inviters/%d/unfreeze", inviter), nil); code != 200 {
		t.Fatal("unfreeze")
	}
	if code, _ := fin.do("GET", "/referral/inviters/999999", nil); code != 404 {
		t.Fatal("unknown inviter")
	}
	// 列表与导出
	if code, out := fin.do("GET", "/referral/inviters?q=5678&sort=invitees", nil); code != 200 || out["total"] != float64(1) {
		t.Fatalf("inviters = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/referral/inviters?sort=bogus", nil); code != 400 {
		t.Fatalf("bogus sort = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/referral/bindings?status=revoked", nil); code != 200 || out["total"] != float64(1) {
		t.Fatalf("bindings = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/referral/bindings?from=2026-01-02&to=2026-01-01", nil); code != 400 {
		t.Fatalf("bad range = %d %v", code, out)
	}
	resp, _ = fin.send("GET", "/referral/bindings/export?inviter_id="+fmt.Sprint(inviter), nil)
	raw, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") || !strings.Contains(string(raw), "绑定ID,邀请人ID,被邀请人ID,邀请码,状态,标记,绑定时间,返利截止,IP,设备") {
		t.Fatalf("bindings csv = %d %q", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), ",same_device,") || !strings.Contains(string(raw), ",'=1+1") {
		t.Fatalf("bindings csv row = %q", raw)
	}
	if code, out := fin.do("GET", "/referral/rewards?status=settled", nil); code != 200 || out["total"] != float64(0) {
		t.Fatalf("rewards = %d %v", code, out)
	}
	resp, _ = fin.send("GET", "/referral/rewards/export", nil)
	raw, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "订单号,邀请人ID,被邀请人ID,消费(元),退款(元),费率(%),应得(元),状态,发放时间,已扣回(元),费率版本") {
		t.Fatalf("rewards csv = %q", raw)
	}
	// 重扫：super + TOTP；days 范围在二次验证之前校验；写审计（水位拨回与重扫本身由 referral 的 Rescan 测试覆盖）
	if code, _ := fin.do("POST", "/referral/rescan", map[string]any{"days": 3}); code != 403 {
		t.Fatal("finance cannot rescan")
	}
	// 一条处理必然失败的流水（正数 order_pay）：重扫照常执行，200 + failed=1，验证窗口打开
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		VALUES ($1, 100, 100, 'order_pay', 'order', 'ord-bad', '', $2)`, invitee, e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	rtotp := sup.totp()
	if code, out := sup.do("POST", "/referral/rescan", map[string]any{"days": 99, "totp": rtotp}); code != 400 {
		t.Fatalf("rescan days range = %d %v", code, out)
	}
	if code, out := sup.do("POST", "/referral/rescan", map[string]any{"days": 3, "totp": rtotp}); code != 200 || out["failed"] != float64(1) || out["incomplete"] != false {
		t.Fatalf("rescan = %d %v", code, out)
	}
	if code, out := sup.do("POST", "/referral/rescan", map[string]any{"days": 2}); code != 200 || out["failed"] != float64(1) {
		t.Fatalf("rescan within step-up window = %d %v", code, out)
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = 'referral.rescan' AND actor_id = $1 AND diff->>'days' = '3'`, sup.admin.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rescan audit = %d %v", n, err)
	}
	// 用户详情 referral 块（all）、仪表盘字段
	if code, out := ops.do("GET", fmt.Sprintf("/users/%d", invitee), nil); code != 200 || out["referral"].(map[string]any)["inviter"].(map[string]any)["user_id"] != float64(inviter) {
		t.Fatalf("user detail = %d %v", code, out)
	}
	if code, out := ops.do("GET", fmt.Sprintf("/users/%d", inviter), nil); code != 200 || out["referral"].(map[string]any)["account"].(map[string]any)["code"] != acc.Code {
		t.Fatalf("inviter user detail = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/dashboard", nil); code != 200 || out["referral_month_settled"] != float64(0) {
		t.Fatalf("dashboard = %d %v", code, out)
	}
}

// 仪表盘与概览的本月返利：只算本月（北京时间）的 referral_rebate / referral_clawback 流水。
func TestReferralDashboardMonthSettled(t *testing.T) {
	e := newEnv(t)
	fin := e.loginAs("fin", admin.RoleFinance)
	uid := e.renter("13812345678")
	local := e.clk.Now().In(time.FixedZone("CST", 8*3600))
	monthStart := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, local.Location())
	for _, l := range []struct {
		kind   string
		amount int64
		at     time.Time
	}{
		{"referral_rebate", 690, e.clk.Now()},
		{"referral_rebate", 310, monthStart},
		{"referral_clawback", -100, e.clk.Now()},
		{"referral_rebate", 5000, monthStart.Add(-time.Second)}, // 上月
	} {
		if _, err := e.pool.Exec(e.ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
			VALUES ($1, $2, 0, $3, 'referral', $4, '', $5)`, uid, l.amount, l.kind, fmt.Sprintf("%s-%d", l.kind, l.amount), l.at); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := fin.do("GET", "/dashboard", nil); code != 200 || out["referral_month_settled"] != float64(900) {
		t.Fatalf("dashboard = %d %v", code, out["referral_month_settled"])
	}
	if code, out := fin.do("GET", "/referral/summary", nil); code != 200 || out["month_settled"] != float64(1000) {
		t.Fatalf("summary = %d %v", code, out)
	}
}
