package identity_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
)

// fakeUploads 只认可 licenseKeyFor(userID) 生成的 key 属于该用户。
type fakeUploads struct{}

func licenseKeyFor(userID int64) string { return fmt.Sprintf("license-of-%d", userID) }

func (fakeUploads) CheckOwned(_ context.Context, key string, userID int64, purpose string) error {
	if key == licenseKeyFor(userID) && purpose == "license" {
		return nil
	}
	return apperr.BadRequest("营业执照文件无效，请重新上传")
}

func newKYCEnv(t *testing.T, verifier kyc.Verifier) (*identity.Service, *pgxpool.Pool, *secretbox.Box) {
	t.Helper()
	pool := dbtest.New(t)
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	svc := identity.NewService(identity.Deps{
		Pool: pool, Clock: clock.NewFake(dbtest.T0), SMS: &sms.Mock{}, KYC: verifier, Box: box, Uploads: fakeUploads{},
	})
	return svc, pool, box
}

const validID = "11010119900101123X"

func TestPersonalKYCPassesAndEncryptsIDNumber(t *testing.T) {
	svc, pool, box := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000001", false)

	u, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID)
	if err != nil {
		t.Fatal(err)
	}
	if u.KYCStatus != "verified" || u.Kind != "personal" {
		t.Fatalf("user = %+v", u)
	}
	if err := svc.CanRent(ctx, uid); err != nil {
		t.Fatalf("CanRent after verify: %v", err)
	}

	var enc []byte
	if err := pool.QueryRow(ctx, `SELECT id_number_enc FROM kyc_records WHERE user_id = $1`, uid).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	plain, err := box.Open(enc)
	if err != nil || string(plain) != validID {
		t.Fatalf("decrypted id = %q, %v", plain, err)
	}
}

func TestPersonalKYCRejected(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{Reject: true})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000002", false)

	_, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID)
	if ae := apperr.As(err); ae == nil || ae.Code != "kyc_rejected" {
		t.Fatalf("want kyc_rejected, got %v", err)
	}
	u, _ := svc.GetUser(ctx, uid)
	if u.KYCStatus != "rejected" {
		t.Fatalf("status = %s, want rejected", u.KYCStatus)
	}
}

func TestPersonalKYCValidation(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000003", false)
	cases := []struct{ name, id string }{
		{"", validID},
		{"张三", "123"},
		{"张三", "1101011990010112345"},
	}
	for _, c := range cases {
		_, err := svc.SubmitPersonalKYC(ctx, uid, c.name, c.id)
		if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
			t.Fatalf("(%q,%q) want bad_request, got %v", c.name, c.id, err)
		}
	}
}

func TestPersonalKYCAlreadyVerified(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	uid := dbtest.CreateUser(t, pool, "13900000004", true)
	if _, err := svc.SubmitPersonalKYC(context.Background(), uid, "张三", validID); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("want invalid state, got %v", err)
	}
}

func TestPersonalKYCDailyLimit(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{Reject: true})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000005", false)
	for i := 0; i < 5; i++ {
		if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); apperr.As(err) == nil || apperr.As(err).Code != "kyc_rejected" {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("6th attempt want rate limited, got %v", err)
	}
}

func TestEnterpriseKYCReview(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000006", false)

	if err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", licenseKeyFor(uid)); err != nil {
		t.Fatal(err)
	}
	u, _ := svc.GetUser(ctx, uid)
	if u.KYCStatus != "pending" {
		t.Fatalf("status = %s, want pending", u.KYCStatus)
	}
	var recordID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM kyc_records WHERE user_id = $1`, uid).Scan(&recordID); err != nil {
		t.Fatal(err)
	}

	if err := svc.ReviewEnterpriseKYC(ctx, audit.Actor{AdminID: 99, IP: "10.0.0.1"}, recordID, false, ""); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("reject without reason want bad_request, got %v", err)
	}
	if err := svc.ReviewEnterpriseKYC(ctx, audit.Actor{AdminID: 99, IP: "10.0.0.1"}, recordID, true, ""); err != nil {
		t.Fatal(err)
	}
	u, _ = svc.GetUser(ctx, uid)
	if u.KYCStatus != "verified" || u.Kind != "enterprise" {
		t.Fatalf("user = %+v", u)
	}
	if err := svc.ReviewEnterpriseKYC(ctx, audit.Actor{AdminID: 99, IP: "10.0.0.1"}, recordID, true, ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("second review want invalid state, got %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'kyc.review' AND actor_id = 99 AND ip = '10.0.0.1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
}

func TestEnterpriseKYCValidation(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	uid := dbtest.CreateUser(t, pool, "13900000007", false)
	err := svc.SubmitEnterpriseKYC(context.Background(), uid, "某某科技有限公司", "bad", licenseKeyFor(uid))
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
}

func TestEnterpriseKYCRequiresOwnUpload(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000008", false)
	other := dbtest.CreateUser(t, pool, "13900000009", false)
	for _, key := range []string{"", "https://files/license.png", licenseKeyFor(other)} {
		err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", key)
		if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
			t.Fatalf("license %q: want bad_request, got %v", key, err)
		}
	}
	if err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", licenseKeyFor(uid)); err != nil {
		t.Fatal(err)
	}
	// 审核中不能再次提交
	if err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", licenseKeyFor(uid)); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("resubmit while pending = %v", err)
	}
	var file string
	_ = pool.QueryRow(ctx, `SELECT license_file FROM kyc_records WHERE user_id = $1`, uid).Scan(&file)
	if file != licenseKeyFor(uid) {
		t.Fatalf("license_file = %q", file)
	}
}

// blockingVerifier 在 release 关闭前阻塞，用于制造并发提交。
type blockingVerifier struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (b *blockingVerifier) VerifyPerson(context.Context, string, string, string) (kyc.Result, error) {
	b.calls.Add(1)
	b.entered <- struct{}{}
	<-b.release
	return kyc.Result{Passed: true}, nil
}

func TestPersonalKYCConcurrentSubmitCallsVerifierOnce(t *testing.T) {
	v := &blockingVerifier{entered: make(chan struct{}, 10), release: make(chan struct{})}
	svc, pool, _ := newKYCEnv(t, v)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000010", false)

	first := make(chan error, 1)
	go func() {
		_, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID)
		first <- err
	}()
	select { // 第一次提交已登记并正在调用核验接口
	case <-v.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("verifier not entered")
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); apperr.As(err) == nil || apperr.As(err).Code != "kyc_in_progress" {
			t.Fatalf("concurrent submit %d = %v", i, err)
		}
	}
	close(v.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if n := v.calls.Load(); n != 1 {
		t.Fatalf("verifier calls = %d, want 1", n)
	}
	u, _ := svc.GetUser(ctx, uid)
	if u.KYCStatus != "verified" {
		t.Fatalf("status = %s", u.KYCStatus)
	}
}

type failingVerifier struct{}

func (failingVerifier) VerifyPerson(context.Context, string, string, string) (kyc.Result, error) {
	return kyc.Result{}, errors.New("aliyun: 账户余额不足")
}

func TestPersonalKYCVerifierErrorCountsTowardLimit(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, failingVerifier{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000011", false)
	for i := 0; i < 5; i++ {
		if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); apperr.As(err) == nil || apperr.As(err).Code != "kyc_unavailable" {
			t.Fatalf("attempt %d = %v", i, err)
		}
	}
	if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("6th attempt = %v", err)
	}
	var errorsN int
	var reason string
	_ = pool.QueryRow(ctx, `SELECT count(*), max(reason) FROM kyc_records WHERE user_id = $1 AND result = 'error'`, uid).Scan(&errorsN, &reason)
	if errorsN != 5 || reason != "aliyun: 账户余额不足" {
		t.Fatalf("error records = %d %q", errorsN, reason)
	}
	if u, _ := svc.GetUser(ctx, uid); u.KYCStatus != "none" {
		t.Fatalf("status = %s, want none", u.KYCStatus)
	}
}

func TestPersonalKYCStalePendingIsReleased(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000012", false)
	var recordID int64
	if err := pool.QueryRow(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, result, created_at, updated_at)
		VALUES ($1, 'personal', '张三', 'pending', $2, $2) RETURNING id`, uid, dbtest.T0.Add(-time.Minute)).Scan(&recordID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); apperr.As(err) == nil || apperr.As(err).Code != "kyc_in_progress" {
		t.Fatalf("with fresh pending = %v", err)
	}
	// 模拟进程在核验途中重启：记录停在 pending 超过 2 分钟
	if _, err := pool.Exec(ctx, `UPDATE kyc_records SET created_at = $2 WHERE id = $1`, recordID, dbtest.T0.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); err != nil {
		t.Fatalf("after stale pending = %v", err)
	}
	var result string
	_ = pool.QueryRow(ctx, `SELECT result FROM kyc_records WHERE id = $1`, recordID).Scan(&result)
	if result != "error" {
		t.Fatalf("stale record result = %s, want error", result)
	}
}

func TestPersonalKYCLateResultAfterStaleReleaseIsDiscarded(t *testing.T) {
	v := &blockingVerifier{entered: make(chan struct{}, 10), release: make(chan struct{})}
	svc, pool, _ := newKYCEnv(t, v)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000013", false)

	first := make(chan error, 1)
	go func() {
		_, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID)
		first <- err
	}()
	select {
	case <-v.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("verifier not entered")
	}
	// 模拟超时清理已把该记录释放为 error
	if _, err := pool.Exec(ctx, `UPDATE kyc_records SET result = 'error', reason = '核验超时未完成' WHERE user_id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	close(v.release)
	select {
	case err := <-first:
		if ae := apperr.As(err); ae == nil || ae.Code != "kyc_unavailable" {
			t.Fatalf("late result = %v, want kyc_unavailable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first submit hung")
	}
	var result string
	_ = pool.QueryRow(ctx, `SELECT result FROM kyc_records WHERE user_id = $1`, uid).Scan(&result)
	if result != "error" {
		t.Fatalf("record result = %s, want error", result)
	}
	if u, _ := svc.GetUser(ctx, uid); u.KYCStatus != "none" {
		t.Fatalf("status = %s, want none", u.KYCStatus)
	}
}
