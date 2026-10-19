package identity_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/referral"
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

func TestRejectEnterpriseKYCKeepsPersonalVerification(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000016", true)
	var recordID int64
	if err := pool.QueryRow(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, license_no, license_file, result, created_at, updated_at)
		VALUES ($1, 'enterprise', '某某科技有限公司', '91420100MA4KXXXX1X', 'f', 'pending', $2, $2) RETURNING id`, uid, dbtest.T0).Scan(&recordID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReviewEnterpriseKYC(ctx, audit.Actor{AdminID: 99}, recordID, false, "执照不清晰"); err != nil {
		t.Fatal(err)
	}
	u, _ := svc.GetUser(ctx, uid)
	if u.KYCStatus != "verified" || u.Kind == "enterprise" {
		t.Fatalf("user after rejected enterprise review = %+v, want still personally verified", u)
	}
	var result string
	_ = pool.QueryRow(ctx, `SELECT result FROM kyc_records WHERE id = $1`, recordID).Scan(&result)
	if result != "rejected" {
		t.Fatalf("record result = %s", result)
	}
}

func mustBox(t *testing.T) *secretbox.Box {
	t.Helper()
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestKYCWritesFingerprintAndCallsHook(t *testing.T) {
	e := newEnv(t, "000000")
	key, _ := referral.DeriveFingerprintKey(dbtest.Key)
	hook := &hookRec{}
	e.svc = identity.NewService(identity.Deps{Pool: e.pool, Clock: e.clock, SMS: e.sms, KYC: kyc.Mock{}, Box: mustBox(t), FixedCode: "000000", FingerprintKey: key})
	e.svc.SetReferral(hook)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, e.pool, "13800000021", false)
	if _, err := e.svc.SubmitPersonalKYC(ctx, uid, "张三", "110101199001011234"); err != nil {
		t.Fatal(err)
	}
	want := referral.Fingerprint(key, "kyc", "110101199001011234")
	var fp string
	if err := e.pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1 AND result = 'verified'`, uid).Scan(&fp); err != nil || fp != want {
		t.Fatalf("fingerprint = %q %v", fp, err)
	}
	if len(hook.kyc) != 1 || hook.kyc[0] != want {
		t.Fatalf("hook = %v", hook.kyc)
	}
	// 被驳回不写指纹、不调钩子
	e.svc = identity.NewService(identity.Deps{Pool: e.pool, Clock: e.clock, SMS: e.sms, KYC: kyc.Mock{Reject: true}, Box: mustBox(t), FixedCode: "000000", FingerprintKey: key})
	e.svc.SetReferral(hook)
	uid2 := dbtest.CreateUser(t, e.pool, "13800000022", false)
	e.svc.SubmitPersonalKYC(ctx, uid2, "李四", "110101199001011235")
	e.pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1`, uid2).Scan(&fp)
	if fp != "" || len(hook.kyc) != 1 {
		t.Fatalf("rejected: fp=%q hooks=%d", fp, len(hook.kyc))
	}
}

func TestEnterpriseKYCWritesFingerprintAndCallsHook(t *testing.T) {
	pool := dbtest.New(t)
	key, _ := referral.DeriveFingerprintKey(dbtest.Key)
	hook := &hookRec{}
	svc := identity.NewService(identity.Deps{Pool: pool, Clock: clock.NewFake(dbtest.T0), SMS: &sms.Mock{}, KYC: kyc.Mock{}, Box: mustBox(t), Uploads: fakeUploads{}, FingerprintKey: key})
	svc.SetReferral(hook)
	ctx := context.Background()
	review := func(phone string, approve bool) int64 {
		t.Helper()
		uid := dbtest.CreateUser(t, pool, phone, false)
		if err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", licenseKeyFor(uid)); err != nil {
			t.Fatal(err)
		}
		var recordID int64
		if err := pool.QueryRow(ctx, `SELECT id FROM kyc_records WHERE user_id = $1`, uid).Scan(&recordID); err != nil {
			t.Fatal(err)
		}
		if err := svc.ReviewEnterpriseKYC(ctx, audit.Actor{AdminID: 99}, recordID, approve, "执照不清晰"); err != nil {
			t.Fatal(err)
		}
		return uid
	}
	uid := review("13900000031", true)
	want := referral.Fingerprint(key, "lic", "91420100MA4KXXXX1X")
	var fp string
	if err := pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1`, uid).Scan(&fp); err != nil || fp != want {
		t.Fatalf("fingerprint = %q %v", fp, err)
	}
	if len(hook.kyc) != 1 || hook.kyc[0] != want {
		t.Fatalf("hook = %v", hook.kyc)
	}
	// 驳回不写指纹、不调钩子
	uid = review("13900000032", false)
	pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1`, uid).Scan(&fp)
	if fp != "" || len(hook.kyc) != 1 {
		t.Fatalf("rejected: fp=%q hooks=%d", fp, len(hook.kyc))
	}
}

// brokenHook 在事务里执行一条会出错的语句后返回错误：模拟 referral 侧的任何故障。
type brokenHook struct{ hookRec }

func (h *brokenHook) OnKYCVerified(ctx context.Context, tx pgx.Tx, _ int64, _ string) error {
	if _, err := tx.Exec(ctx, `SELECT 1 / 0`); err == nil {
		return errors.New("expected division by zero")
	}
	return errors.New("referral exploded")
}

// 邀请返利钩子出错不能让实名失败（个人实名已调用付费接口；企业审核是管理员操作）：钩子在保存点里执行，出错回滚到保存点并记日志。
func TestKYCHookErrorDoesNotFailVerification(t *testing.T) {
	pool := dbtest.New(t)
	key, _ := referral.DeriveFingerprintKey(dbtest.Key)
	svc := identity.NewService(identity.Deps{Pool: pool, Clock: clock.NewFake(dbtest.T0), SMS: &sms.Mock{}, KYC: kyc.Mock{}, Box: mustBox(t), Uploads: fakeUploads{}, FingerprintKey: key})
	svc.SetReferral(&brokenHook{})
	ctx := context.Background()

	uid := dbtest.CreateUser(t, pool, "13900000041", false)
	u, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID)
	if err != nil || u.KYCStatus != "verified" {
		t.Fatalf("personal = %+v %v", u, err)
	}
	var fp string
	if err := pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1 AND result = 'verified'`, uid).Scan(&fp); err != nil || fp != referral.Fingerprint(key, "kyc", validID) {
		t.Fatalf("personal fingerprint = %q %v", fp, err)
	}

	uid = dbtest.CreateUser(t, pool, "13900000042", false)
	if err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", licenseKeyFor(uid)); err != nil {
		t.Fatal(err)
	}
	var recordID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM kyc_records WHERE user_id = $1`, uid).Scan(&recordID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReviewEnterpriseKYC(ctx, audit.Actor{AdminID: 99}, recordID, true, ""); err != nil {
		t.Fatalf("review: %v", err)
	}
	if u, _ := svc.GetUser(ctx, uid); u.KYCStatus != "verified" || u.Kind != "enterprise" {
		t.Fatalf("enterprise user = %+v", u)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'kyc.review' AND target_id = $1`, fmt.Sprint(recordID)).Scan(&n)
	if n != 1 {
		t.Fatalf("kyc.review audit rows = %d", n)
	}
}

// slowHook 委托真实的 referral.Service，但在同身份判定前停顿，拉长 KYC 事务持有 users(邀请人) 行锁的窗口，
// 让并发注册的 Bind 更容易插进来。记录判定本身的错误（KYC 侧会吞掉并记日志，这里要能看到）。
type slowHook struct {
	real    *referral.Service
	delay   time.Duration
	entered chan struct{} // 进入判定（KYC 事务已持有 users 行锁）时关闭；每轮重置
	mu      sync.Mutex
	errs    []error
}

func (h *slowHook) Bind(ctx context.Context, tx pgx.Tx, req referral.BindRequest) (referral.BindResult, error) {
	return h.real.Bind(ctx, tx, req)
}

func (h *slowHook) OnKYCVerified(ctx context.Context, tx pgx.Tx, userID int64, fp string) error {
	close(h.entered)
	time.Sleep(h.delay)
	err := h.real.OnKYCVerified(ctx, tx, userID, fp)
	if err != nil {
		h.mu.Lock()
		h.errs = append(h.errs, err)
		h.mu.Unlock()
	}
	return err
}

// 邀请人实名（users(A) FOR UPDATE → referral_accounts(A) FOR UPDATE）与多个用 A 的码注册（Bind：users(A) KEY SHARE / referral_accounts(A) FOR SHARE）
// 并发：不得死锁，注册全部成功并绑定，实名通过且同身份判定本身不出错。
func TestKYCConcurrentWithRegistrationsNoDeadlock(t *testing.T) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	key, _ := referral.DeriveFingerprintKey(dbtest.Key)
	svc := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: &sms.Mock{}, KYC: kyc.Mock{}, Box: mustBox(t), FixedCode: "000000", FingerprintKey: key, CodeIPHourLimit: 10000})
	ref := referral.NewService(referral.Deps{Pool: pool, Clock: clk, Users: svc})
	hook := &slowHook{real: ref, delay: 20 * time.Millisecond}
	svc.SetReferral(hook)
	ctx := context.Background()
	const rounds, n = 5, 8
	for round := 0; round < rounds; round++ {
		inviter := dbtest.CreateUser(t, pool, fmt.Sprintf("1370000%04d", round), false)
		acc, err := ref.EnsureCode(ctx, inviter)
		if err != nil {
			t.Fatal(err)
		}
		// 先有一个已绑定的被邀请人，使邀请人实名时一定走到 (b)：FOR UPDATE 自己的 referral_accounts
		first := fmt.Sprintf("1360000%02d99", round)
		if err := svc.SendLoginCode(ctx, first, "9.9.9.9"); err != nil {
			t.Fatal(err)
		}
		if _, r, err := svc.LoginWith(ctx, identity.LoginRequest{Phone: first, Code: "000000", InviteCode: acc.Code}); err != nil || r.Invite == nil || !r.Invite.Bound {
			t.Fatalf("round %d seed login = %+v %v", round, r, err)
		}
		phones := make([]string, n)
		for i := range phones {
			phones[i] = fmt.Sprintf("1350000%02d%02d", round, i)
			if err := svc.SendLoginCode(ctx, phones[i], "9.9.9.9"); err != nil {
				t.Fatal(err)
			}
		}
		hook.entered = make(chan struct{})
		start := make(chan struct{})
		var wg sync.WaitGroup
		loginErrs := make([]error, n)
		var kycErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, kycErr = svc.SubmitPersonalKYC(ctx, inviter, "张三", fmt.Sprintf("1101011990010112%02d", round))
		}()
		for i := range phones {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				if i%2 == 0 {
					<-hook.entered // 一半在 KYC 持有 users(A) 行锁期间注册（必然与同身份判定交错）
				} else {
					time.Sleep(time.Duration(i) * time.Millisecond) // 另一半自由竞争
				}
				_, r, err := svc.LoginWith(ctx, identity.LoginRequest{Phone: phones[i], Code: "000000", InviteCode: acc.Code})
				if err == nil && (r.Invite == nil || !r.Invite.Bound) {
					err = fmt.Errorf("not bound: %+v", r.Invite)
				}
				loginErrs[i] = err
			}(i)
		}
		close(start)
		wg.Wait()
		if kycErr != nil {
			t.Fatalf("round %d kyc: %v", round, kycErr)
		}
		for i, err := range loginErrs {
			if err != nil {
				t.Fatalf("round %d login %d: %v", round, i, err)
			}
		}
		hook.mu.Lock()
		herrs := hook.errs
		hook.mu.Unlock()
		if len(herrs) != 0 {
			t.Fatalf("round %d OnKYCVerified errors: %v", round, herrs)
		}
		var bound int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM referral_bindings WHERE inviter_id = $1`, inviter).Scan(&bound); err != nil || bound != n+1 {
			t.Fatalf("round %d bindings = %d %v", round, bound, err)
		}
	}
}
