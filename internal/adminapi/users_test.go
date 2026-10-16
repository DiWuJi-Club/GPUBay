package adminapi_test

import (
	"bytes"
	"io"
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/storage"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func TestUserListAndDetail(t *testing.T) {
	e := newEnv(t)
	e.machine("SN1")
	uid := e.renter("13600000001")
	e.place(uid)
	fin := e.loginAs("fin", admin.RoleFinance)
	if code, out := fin.do("GET", "/users?q=13600000001", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	code, out := fin.do("GET", "/users/"+itoa(uid), nil)
	if code != 200 || out["user"].(map[string]any)["phone"] != "13600000001" || out["balance"].(float64) != 1_000_000-6900 ||
		len(out["instances"].([]any)) != 1 || len(out["orders"].([]any)) != 1 || len(out["ledger"].([]any)) != 2 {
		t.Fatalf("detail = %d %v", code, out)
	}
	if code, _ := fin.do("GET", "/users/9999", nil); code != 404 {
		t.Fatalf("missing user = %d", code)
	}
}

func TestBanUnbanEndpoints(t *testing.T) {
	e := newEnv(t)
	uid := e.renter("13600000001")
	ops := e.loginAs("ops", admin.RoleOps)
	if code, out := ops.do("POST", "/users/"+itoa(uid)+"/ban", map[string]string{"reason": "违规"}); code != 200 {
		t.Fatalf("ban = %d %v", code, out)
	}
	if u, _ := e.identity.GetUser(e.ctx, uid); u.Status != "banned" {
		t.Fatalf("status = %s", u.Status)
	}
	if code, out := ops.do("POST", "/users/"+itoa(uid)+"/unban", map[string]any{}); code != 200 {
		t.Fatalf("unban = %d %v", code, out)
	}
	fin := e.loginAs("fin", admin.RoleFinance)
	if code, out := fin.do("POST", "/users/"+itoa(uid)+"/ban", map[string]string{"reason": "x"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance ban = %d %v", code, out)
	}
}

func TestEnterpriseReviewAndLicenseFile(t *testing.T) {
	e := newEnv(t)
	uid := e.renter("13600000001")
	if _, err := e.pool.Exec(e.ctx, `UPDATE users SET kyc_status = 'none' WHERE id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	up, err := e.uploads.Save(e.ctx, uid, storage.PurposeLicense, bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.identity.SubmitEnterpriseKYC(e.ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", up.Key); err != nil {
		t.Fatal(err)
	}
	ops := e.loginAs("ops", admin.RoleOps)
	code, out := ops.do("GET", "/kyc/pending", nil)
	if code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("pending = %d %v", code, out)
	}
	rec := out["items"].([]any)[0].(map[string]any)
	id := int64(rec["id"].(float64))
	if code, out := ops.do("GET", "/kyc/"+itoa(id), nil); code != 200 || out["license_file"] != up.Key {
		t.Fatalf("record = %d %v", code, out)
	}
	resp, _ := ops.send("GET", "/files/"+up.Key, nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		resp.Header.Get("Content-Disposition") != "inline" || !bytes.Equal(body, pngBytes) {
		t.Fatalf("file = %d %v", resp.StatusCode, resp.Header)
	}
	if code, _ := ops.do("GET", "/files/../../etc/passwd", nil); code != 404 {
		t.Fatalf("traversal = %d", code)
	}
	if code, out := ops.do("POST", "/kyc/"+itoa(id)+"/review", map[string]any{"approve": false}); code != 400 {
		t.Fatalf("reject without reason = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/kyc/"+itoa(id)+"/review", map[string]any{"approve": true}); code != 200 {
		t.Fatalf("approve = %d %v", code, out)
	}
	if u, _ := e.identity.GetUser(e.ctx, uid); u.KYCStatus != "verified" || u.Kind != "enterprise" {
		t.Fatalf("user = %+v", u)
	}
	fin := e.loginAs("fin", admin.RoleFinance)
	if code, _ := fin.do("GET", "/files/"+up.Key, nil); code != 403 {
		t.Fatalf("finance file = %d", code)
	}
}

func TestAdjustAndManualRecharge(t *testing.T) {
	e := newEnv(t)
	uid := e.renter("13600000001")
	fin := e.loginAs("fin", admin.RoleFinance)
	if code, out := fin.do("POST", "/users/"+itoa(uid)+"/adjust", map[string]any{"amount": 5000, "remark": "补偿"}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("adjust without totp = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/users/"+itoa(uid)+"/adjust", map[string]any{"amount": 5000, "remark": "补偿", "totp": fin.totp()}); code != 200 || out["balance"].(float64) != 1_005_000 {
		t.Fatalf("adjust = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/users/"+itoa(uid)+"/adjust", map[string]any{"amount": -2_000_000, "remark": "扣多了"}); code != 402 || codeOf(out) != "insufficient_balance" {
		t.Fatalf("over-deduct = %d %v", code, out)
	}
	code, out := fin.do("POST", "/users/"+itoa(uid)+"/manual-recharge", map[string]any{"amount": 20000, "remark": "对公转账 123"})
	if code != 200 || out["status"] != "paid" || out["channel"] != "manual" {
		t.Fatalf("manual recharge = %d %v", code, out)
	}
	if b, _ := e.wallet.Balance(e.ctx, uid); b != 1_025_000 {
		t.Fatalf("balance = %d", b)
	}
	if code, _ := fin.do("POST", "/users/9999/adjust", map[string]any{"amount": 100, "remark": "x"}); code != 404 {
		t.Fatalf("unknown user = %d", code)
	}
	ops := e.loginAs("ops", admin.RoleOps)
	if code, out := ops.do("POST", "/users/"+itoa(uid)+"/adjust", map[string]any{"amount": 1, "remark": "x"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops adjust = %d %v", code, out)
	}
}
