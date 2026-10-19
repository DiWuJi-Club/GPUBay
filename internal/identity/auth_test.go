package identity_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/sms"
	"spark-cloud/internal/referral"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type env struct {
	pool  *pgxpool.Pool
	svc   *identity.Service
	sms   *sms.Mock
	clock *clock.Fake
}

func newEnv(t *testing.T, fixedCode string) env {
	t.Helper()
	pool := dbtest.New(t)
	mock := &sms.Mock{}
	clk := clock.NewFake(dbtest.T0)
	svc := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: mock, FixedCode: fixedCode})
	return env{pool: pool, svc: svc, sms: mock, clock: clk}
}

func lastCode(t *testing.T, m *sms.Mock) string {
	t.Helper()
	sent := m.Sent()
	if len(sent) == 0 {
		t.Fatal("no sms sent")
	}
	return sent[len(sent)-1].Params["code"]
}

func TestLoginFlowCreatesUserAndSession(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000001", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	token, u, err := e.svc.Login(ctx, "13800000001", lastCode(t, e.sms))
	if err != nil {
		t.Fatal(err)
	}
	if u.Phone != "13800000001" || u.KYCStatus != "none" || u.Status != "active" {
		t.Fatalf("unexpected user %+v", u)
	}
	got, err := e.svc.Authenticate(ctx, token)
	if err != nil || got.ID != u.ID {
		t.Fatalf("Authenticate = %+v, %v", got, err)
	}

	// 同一手机号再次登录是同一个用户
	e.clock.Advance(time.Minute)
	if err := e.svc.SendLoginCode(ctx, "13800000001", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	_, u2, err := e.svc.Login(ctx, "13800000001", lastCode(t, e.sms))
	if err != nil || u2.ID != u.ID {
		t.Fatalf("second login = %+v, %v", u2, err)
	}

	if err := e.svc.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, token); !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("after logout want unauthorized, got %v", err)
	}
}

func TestSendCodeRejectsInvalidPhone(t *testing.T) {
	e := newEnv(t, "")
	err := e.svc.SendLoginCode(context.Background(), "12345", "1.1.1.1")
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
}

func TestSendCodeRateLimitedWithin60Seconds(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000002", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SendLoginCode(ctx, "13800000002", "1.1.1.1"); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("want rate limited, got %v", err)
	}
	e.clock.Advance(61 * time.Second)
	if err := e.svc.SendLoginCode(ctx, "13800000002", "1.1.1.1"); err != nil {
		t.Fatalf("after 61s want ok, got %v", err)
	}
}

func TestSendCodeDailyLimit(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := e.svc.SendLoginCode(ctx, "13800000003", "1.1.1.1"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		e.clock.Advance(61 * time.Second)
	}
	if err := e.svc.SendLoginCode(ctx, "13800000003", "1.1.1.1"); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("11th send want rate limited, got %v", err)
	}
}

func TestWrongCodeLocksAfterFiveAttempts(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000004", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code := lastCode(t, e.sms)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < 5; i++ {
		if _, _, err := e.svc.Login(ctx, "13800000004", wrong); !errors.Is(err, apperr.ErrInvalidCode) {
			t.Fatalf("attempt %d want invalid code, got %v", i, err)
		}
	}
	if _, _, err := e.svc.Login(ctx, "13800000004", code); !errors.Is(err, apperr.ErrInvalidCode) {
		t.Fatalf("correct code after 5 failures must be rejected, got %v", err)
	}
}

func TestCodeExpiresAfterFiveMinutes(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000005", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(5*time.Minute + time.Second)
	if _, _, err := e.svc.Login(ctx, "13800000005", lastCode(t, e.sms)); !errors.Is(err, apperr.ErrInvalidCode) {
		t.Fatalf("want invalid code, got %v", err)
	}
}

func TestCodeCannotBeReused(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000006", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code := lastCode(t, e.sms)
	if _, _, err := e.svc.Login(ctx, "13800000006", code); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Login(ctx, "13800000006", code); !errors.Is(err, apperr.ErrInvalidCode) {
		t.Fatalf("reuse want invalid code, got %v", err)
	}
}

func TestSessionExpiresAfter30Days(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	_ = e.svc.SendLoginCode(ctx, "13800000007", "1.1.1.1")
	token, _, err := e.svc.Login(ctx, "13800000007", lastCode(t, e.sms))
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(30*24*time.Hour + time.Second)
	if _, err := e.svc.Authenticate(ctx, token); !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("want unauthorized, got %v", err)
	}
}

func TestFixedCodeForDevelopment(t *testing.T) {
	e := newEnv(t, "000000")
	ctx := context.Background()
	_ = e.svc.SendLoginCode(ctx, "13800000008", "1.1.1.1")
	if got := lastCode(t, e.sms); got != "000000" {
		t.Fatalf("code = %q, want 000000", got)
	}
	if _, _, err := e.svc.Login(ctx, "13800000008", "000000"); err != nil {
		t.Fatal(err)
	}
}

func TestCanRentRequiresVerifiedKYC(t *testing.T) {
	e := newEnv(t, "000000")
	ctx := context.Background()
	_ = e.svc.SendLoginCode(ctx, "13800000009", "1.1.1.1")
	_, u, err := e.svc.Login(ctx, "13800000009", "000000")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CanRent(ctx, u.ID); !errors.Is(err, apperr.ErrKYCRequired) {
		t.Fatalf("want kyc required, got %v", err)
	}
}

func TestConcurrentSendsSameInstantSendOnlyOnce(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	const n = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 同时放行，尽量让所有请求的计数与插入交错
			errs[i] = e.svc.SendLoginCode(ctx, "13800000010", "1.1.1.1")
		}(i)
	}
	close(start)
	wg.Wait()

	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, apperr.ErrRateLimited):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("successful sends = %d, want 1", ok)
	}
	if got := len(e.sms.Sent()); got != 1 {
		t.Fatalf("sms sent = %d, want 1", got)
	}
}

func TestIPHourlyLimit(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		phone := fmt.Sprintf("1390000%04d", i)
		if err := e.svc.SendLoginCode(ctx, phone, "2.2.2.2"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if err := e.svc.SendLoginCode(ctx, "13900009999", "2.2.2.2"); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("31st send from same IP want rate limited, got %v", err)
	}
	if err := e.svc.SendLoginCode(ctx, "13900009998", "3.3.3.3"); err != nil {
		t.Fatalf("other IP want ok, got %v", err)
	}
}

func TestFourWrongCodesThenCorrectSucceeds(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000011", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code := lastCode(t, e.sms)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 0; i < 4; i++ {
		if _, _, err := e.svc.Login(ctx, "13800000011", wrong); !errors.Is(err, apperr.ErrInvalidCode) {
			t.Fatalf("attempt %d want invalid code, got %v", i, err)
		}
	}
	if _, _, err := e.svc.Login(ctx, "13800000011", code); err != nil {
		t.Fatalf("correct code after 4 failures want ok, got %v", err)
	}
}

type flakySMS struct {
	fail bool
	sms.Mock
}

func (f *flakySMS) Send(ctx context.Context, phone, template string, params map[string]string) error {
	if f.fail {
		return errors.New("sms gateway down")
	}
	return f.Mock.Send(ctx, phone, template, params)
}

// 短信发送失败时删除验证码记录：用户可立即重试，不会因为没收到的验证码被限流。
func TestSendCodeFailureDoesNotRateLimit(t *testing.T) {
	pool := dbtest.New(t)
	sender := &flakySMS{fail: true}
	svc := identity.NewService(identity.Deps{Pool: pool, Clock: clock.NewFake(dbtest.T0), SMS: sender})
	ctx := context.Background()
	if err := svc.SendLoginCode(ctx, "13800000001", "1.1.1.1"); err == nil {
		t.Fatal("want send error")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sms_codes`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sms_codes rows = %d, %v; want 0", n, err)
	}
	sender.fail = false
	if err := svc.SendLoginCode(ctx, "13800000001", "1.1.1.1"); err != nil {
		t.Fatalf("retry after failed send: %v", err)
	}
	if _, _, err := svc.Login(ctx, "13800000001", lastCode(t, &sender.Mock)); err != nil {
		t.Fatalf("login with resent code: %v", err)
	}
}

func TestSendCodeIPHourLimitConfigurable(t *testing.T) {
	ctx := context.Background()
	svc := identity.NewService(identity.Deps{Pool: dbtest.New(t), Clock: clock.NewFake(dbtest.T0), SMS: &sms.Mock{}, CodeIPHourLimit: 2})
	for i, phone := range []string{"13800000001", "13800000002"} {
		if err := svc.SendLoginCode(ctx, phone, "10.0.0.9"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if err := svc.SendLoginCode(ctx, "13800000003", "10.0.0.9"); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("third send from same ip = %v, want rate_limited", err)
	}
	// 零值保持默认 30 次
	def := identity.NewService(identity.Deps{Pool: dbtest.New(t), Clock: clock.NewFake(dbtest.T0), SMS: &sms.Mock{}})
	for i := 0; i < 30; i++ {
		if err := def.SendLoginCode(ctx, fmt.Sprintf("138000%05d", i), "10.0.0.8"); err != nil {
			t.Fatalf("default send %d: %v", i, err)
		}
	}
	if err := def.SendLoginCode(ctx, "13999999999", "10.0.0.8"); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("31st send = %v, want rate_limited", err)
	}
}

func TestSessionHashAndUserID(t *testing.T) {
	e := newEnv(t, "000000")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800001234", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	token, u, err := e.svc.Login(ctx, "13800001234", "000000")
	if err != nil {
		t.Fatal(err)
	}
	h := identity.SessionHash(token)
	if len(h) != 64 || h == token {
		t.Fatalf("hash = %q", h)
	}
	if id, err := e.svc.SessionUserID(ctx, h); err != nil || id != u.ID {
		t.Fatalf("session user = %d %v", id, err)
	}
	if _, err := e.svc.SessionUserID(ctx, identity.SessionHash("other")); !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("unknown = %v", err)
	}
	if err := e.svc.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SessionUserID(ctx, h); !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("after logout = %v", err)
	}
}

func TestSessionUserIDBannedAndExpired(t *testing.T) {
	e := newEnv(t, "000000")
	ctx := context.Background()
	login := func(phone string) (string, identity.User) {
		t.Helper()
		if err := e.svc.SendLoginCode(ctx, phone, "1.1.1.1"); err != nil {
			t.Fatal(err)
		}
		token, u, err := e.svc.Login(ctx, phone, "000000")
		if err != nil {
			t.Fatal(err)
		}
		return token, u
	}
	// 会话仍在而用户已被封禁（Ban 会删会话，这里直接改状态模拟封禁与会话清理之间的窗口）
	banned, bu := login("13800005555")
	if _, err := e.pool.Exec(ctx, `UPDATE users SET status = 'banned' WHERE id = $1`, bu.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SessionUserID(ctx, identity.SessionHash(banned)); !errors.Is(err, apperr.ErrBanned) {
		t.Fatalf("banned = %v", err)
	}
	// 过期：会话有效期 30 天
	token, u := login("13800005556")
	h := identity.SessionHash(token)
	e.clock.Advance(30*24*time.Hour - time.Second)
	if id, err := e.svc.SessionUserID(ctx, h); err != nil || id != u.ID {
		t.Fatalf("just before expiry = %d %v", id, err)
	}
	e.clock.Advance(time.Second)
	if _, err := e.svc.SessionUserID(ctx, h); !errors.Is(err, apperr.ErrUnauthorized) {
		t.Fatalf("expired = %v", err)
	}
}

type hookRec struct {
	mu    sync.Mutex
	calls []referral.BindRequest
	res   referral.BindResult
	err   error
	kyc   []string // fingerprints seen
}

func (h *hookRec) Bind(_ context.Context, _ pgx.Tx, req referral.BindRequest) (referral.BindResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, req)
	return h.res, h.err
}
func (h *hookRec) OnKYCVerified(_ context.Context, _ pgx.Tx, _ int64, fp string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kyc = append(h.kyc, fp)
	return nil
}

const devA = "11111111-2222-4333-8444-555555555555"

func TestLoginWithIsNewDeviceAndLastLogin(t *testing.T) {
	e := newEnv(t, "000000")
	hook := &hookRec{res: referral.BindResult{Bound: true, Reason: "ok", Inviter: "138****0000"}}
	e.svc.SetReferral(hook)
	ctx := context.Background()
	send := func(phone string) {
		t.Helper()
		// 本测试对同一手机号发 16 次验证码，超过每日 10 条上限，故每次推进一天多一点。
		e.clock.Advance(24*time.Hour + time.Second)
		if err := e.svc.SendLoginCode(ctx, phone, "9.9.9.9"); err != nil {
			t.Fatal(err)
		}
	}
	send("13800000011")
	_, r, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000011", Code: "000000", InviteCode: "ABCDEFGH", IP: "1.2.3.4", DeviceID: devA, UserAgent: "ua"})
	if err != nil || !r.IsNew || r.Invite == nil || !r.Invite.Bound || r.Invite.Inviter != "138****0000" {
		t.Fatalf("first login = %+v %v", r, err)
	}
	if len(hook.calls) != 1 || hook.calls[0].Code != "ABCDEFGH" || hook.calls[0].InviteeID != r.ID || hook.calls[0].IP != "1.2.3.4" || hook.calls[0].DeviceID != devA || !hook.calls[0].Now.Equal(e.clock.Now()) {
		t.Fatalf("bind req = %+v", hook.calls)
	}
	var ip string
	var at *time.Time
	if err := e.pool.QueryRow(ctx, `SELECT last_login_ip, last_login_at FROM users WHERE id = $1`, r.ID).Scan(&ip, &at); err != nil || ip != "1.2.3.4" || at == nil || !at.Equal(e.clock.Now()) {
		t.Fatalf("last login = %q %v %v", ip, at, err)
	}
	var devs int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM user_devices WHERE user_id = $1 AND device_id = $2`, r.ID, devA).Scan(&devs); err != nil || devs != 1 {
		t.Fatalf("devices = %d %v", devs, err)
	}
	// 老用户带码：不调用 Bind，reason existing_user；最近登录更新
	send("13800000011")
	_, r2, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000011", Code: "000000", InviteCode: "ABCDEFGH", IP: "5.6.7.8"})
	if err != nil || r2.IsNew || r2.ID != r.ID || r2.Invite == nil || r2.Invite.Bound || r2.Invite.Reason != "existing_user" || len(hook.calls) != 1 {
		t.Fatalf("second login = %+v %v calls=%d", r2, err, len(hook.calls))
	}
	if err := e.pool.QueryRow(ctx, `SELECT last_login_ip FROM users WHERE id = $1`, r.ID).Scan(&ip); err != nil || ip != "5.6.7.8" {
		t.Fatalf("last ip = %q", ip)
	}
	// 不带码：Invite 为 nil
	send("13800000011")
	if _, r3, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000011", Code: "000000"}); err != nil || r3.Invite != nil {
		t.Fatalf("no code = %+v %v", r3, err)
	}
	// 非 UUID 的设备 ID 不写；设备上限 10 台（删最旧）
	send("13800000011")
	if _, _, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000011", Code: "000000", DeviceID: "not-a-uuid"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 11; i++ {
		send("13800000011")
		if _, _, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000011", Code: "000000", DeviceID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	var oldest bool
	if err := e.pool.QueryRow(ctx, `SELECT count(*), bool_or(device_id = $2) FROM user_devices WHERE user_id = $1`, r.ID, devA).Scan(&n, &oldest); err != nil {
		t.Fatal(err)
	}
	if n != 10 || oldest {
		t.Fatalf("devices after 12 logins = %d (oldest kept=%v)", n, oldest)
	}
	// 旧签名 Login 仍可用
	send("13800000011")
	if _, u, err := e.svc.Login(ctx, "13800000011", "000000"); err != nil || u.ID != r.ID {
		t.Fatalf("Login = %+v %v", u, err)
	}
}

// 裁定 Q15：非 UUID 的 X-Device-Id 视同未提供——不写 user_devices，也不转给 Bind；UUID 大写时按小写存与转发。
func TestLoginWithNonUUIDDeviceIgnored(t *testing.T) {
	e := newEnv(t, "000000")
	hook := &hookRec{res: referral.BindResult{Bound: true, Reason: "ok"}}
	e.svc.SetReferral(hook)
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000016", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	_, r, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000016", Code: "000000", InviteCode: "ABCDEFGH", DeviceID: "Not-A-UUID"})
	if err != nil || !r.IsNew || len(hook.calls) != 1 || hook.calls[0].DeviceID != "" {
		t.Fatalf("non-uuid login = %+v %v calls=%+v", r, err, hook.calls)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM user_devices WHERE user_id = $1`, r.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("devices = %d %v", n, err)
	}
	e.clock.Advance(61 * time.Second)
	if err := e.svc.SendLoginCode(ctx, "13800000017", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	upper := "AAAAAAAA-2222-4333-8444-555555555555"
	_, r2, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000017", Code: "000000", InviteCode: "ABCDEFGH", DeviceID: upper})
	if err != nil || len(hook.calls) != 2 || hook.calls[1].DeviceID != strings.ToLower(upper) {
		t.Fatalf("uuid login = %+v %v calls=%+v", r2, err, hook.calls)
	}
	if has, err := e.svc.HasDevice(ctx, e.pool, r2.ID, strings.ToLower(upper)); err != nil || !has {
		t.Fatalf("uuid device stored = %v %v", has, err)
	}
}

func TestLoginWithBindBusinessFailureKeepsRegistration(t *testing.T) {
	e := newEnv(t, "000000")
	hook := &hookRec{res: referral.BindResult{Bound: false, Reason: "invalid_code"}}
	e.svc.SetReferral(hook)
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000012", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	_, r, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000012", Code: "000000", InviteCode: "ZZZZZZZZ"})
	if err != nil || !r.IsNew || r.Invite == nil || r.Invite.Bound || r.Invite.Reason != "invalid_code" {
		t.Fatalf("= %+v %v", r, err)
	}
	if _, err := e.svc.GetUser(ctx, r.ID); err != nil {
		t.Fatalf("user must exist: %v", err)
	}
	// 数据库错误回滚注册
	hook.err = errors.New("boom")
	e.clock.Advance(61 * time.Second)
	if err := e.svc.SendLoginCode(ctx, "13800000013", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000013", Code: "000000", InviteCode: "ZZZZZZZZ"}); err == nil {
		t.Fatal("db error must propagate")
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE phone = '13800000013'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("registration must be rolled back")
	}
	// 没装 hook 时带码：invalid_code，注册成功
	e.svc.SetReferral(nil)
	e.clock.Advance(61 * time.Second)
	if err := e.svc.SendLoginCode(ctx, "13800000014", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if _, r, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000014", Code: "000000", InviteCode: "ABCDEFGH"}); err != nil || r.Invite == nil || r.Invite.Reason != "invalid_code" {
		t.Fatalf("no hook = %+v %v", r, err)
	}
}

func TestUserLookupImplementation(t *testing.T) {
	e := newEnv(t, "000000")
	ctx := context.Background()
	uid := dbtest.CreateUser(t, e.pool, "13800000015", true)
	if _, err := e.pool.Exec(ctx, `INSERT INTO user_devices (user_id, device_id, first_seen_at, last_seen_at) VALUES ($1, $2, $3, $3)`, uid, devA, dbtest.T0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, result, fingerprint, created_at, updated_at) VALUES ($1, 'personal', 'x', 'verified', 'fp-old', $2, $2), ($1, 'enterprise', 'x', 'verified', 'fp-new', $2, $2), ($1, 'personal', 'x', 'verified', 'fp-old', $2, $2), ($1, 'personal', 'x', 'rejected', 'fp-bad', $2, $2)`, uid, dbtest.T0); err != nil {
		t.Fatal(err)
	}
	var lookup referral.UserLookup = e.svc
	st, err := lookup.UserState(ctx, e.pool, uid)
	if err != nil || st.Status != "active" || st.KYCStatus != "verified" || st.Phone != "13800000015" || st.LastLoginAt != nil {
		t.Fatalf("state = %+v %v", st, err)
	}
	if _, err := lookup.UserState(ctx, e.pool, 999999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if has, err := lookup.HasDevice(ctx, e.pool, uid, devA); err != nil || !has {
		t.Fatalf("HasDevice = %v %v", has, err)
	}
	if has, err := lookup.HasDevice(ctx, e.pool, uid, "other"); err != nil || has {
		t.Fatalf("HasDevice other = %v %v", has, err)
	}
	fps, err := lookup.VerifiedFingerprints(ctx, e.pool, []int64{uid, 999999})
	if err != nil || len(fps) != 1 || len(fps[uid]) != 2 || fps[uid][0] != "fp-old" || fps[uid][1] != "fp-new" {
		t.Fatalf("fps = %v %v", fps, err)
	}
}

// 封禁用户登录被拒：不更新 last_login_*，也不记设备。
func TestLoginWithBannedKeepsLastLogin(t *testing.T) {
	e := newEnv(t, "000000")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000018", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	_, r, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000018", Code: "000000", IP: "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	firstAt := e.clock.Now()
	if _, err := e.pool.Exec(ctx, `UPDATE users SET status = 'banned' WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(61 * time.Second)
	if err := e.svc.SendLoginCode(ctx, "13800000018", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000018", Code: "000000", IP: "5.6.7.8", DeviceID: devA}); !errors.Is(err, apperr.ErrBanned) {
		t.Fatalf("banned login = %v", err)
	}
	var ip string
	var at *time.Time
	var devs int
	if err := e.pool.QueryRow(ctx, `SELECT last_login_ip, last_login_at, (SELECT count(*) FROM user_devices WHERE user_id = $1) FROM users WHERE id = $1`, r.ID).Scan(&ip, &at, &devs); err != nil {
		t.Fatal(err)
	}
	if ip != "1.2.3.4" || at == nil || !at.Equal(firstAt) || devs != 0 {
		t.Fatalf("after banned login: ip=%q at=%v devices=%d", ip, at, devs)
	}
}

// User-Agent 原样交给 Bind（清洗与截断由 referral 负责）。
func TestLoginWithPassesRawUserAgent(t *testing.T) {
	e := newEnv(t, "000000")
	hook := &hookRec{res: referral.BindResult{Bound: true, Reason: "ok"}}
	e.svc.SetReferral(hook)
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000019", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	ua := strings.Repeat("中", referral.MaxUserAgent) + "\xff"
	if _, _, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000019", Code: "000000", InviteCode: "ABCDEFGH", UserAgent: ua}); err != nil {
		t.Fatal(err)
	}
	if len(hook.calls) != 1 || hook.calls[0].UserAgent != ua {
		t.Fatalf("bind ua len = %d, want raw %d", len(hook.calls[0].UserAgent), len(ua))
	}
}

// 同一新手机号并发首登：验证码行的 FOR UPDATE 把两次登录串行化，只有一次成功注册并调用 Bind。
func TestLoginWithConcurrentFirstLoginBindsOnce(t *testing.T) {
	e := newEnv(t, "000000")
	hook := &hookRec{res: referral.BindResult{Bound: true, Reason: "ok"}}
	e.svc.SetReferral(hook)
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000020", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]identity.LoginResult, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i], errs[i] = e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000020", Code: "000000", InviteCode: "ABCDEFGH"})
		}()
	}
	wg.Wait()
	ok, isNew := 0, 0
	for i := range 2 {
		if errs[i] == nil {
			ok++
			if results[i].IsNew {
				isNew++
			}
		} else if !errors.Is(errs[i], apperr.ErrInvalidCode) {
			t.Fatalf("unexpected error: %v", errs[i])
		}
	}
	var users int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE phone = '13800000020'`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if ok != 1 || isNew != 1 || len(hook.calls) != 1 || users != 1 {
		t.Fatalf("ok=%d isNew=%d binds=%d users=%d", ok, isNew, len(hook.calls), users)
	}
}

// 计划 8 终审 I1：控制台会话启动（GET /me）时补记设备与最近登录——同一设备 / 登录信息一小时内最多写一次；
// 设备 ID 规则（只认 UUID、按小写存）与 10 台上限同登录；封禁用户不记。
func TestTouchSessionRecordsDeviceAndLastLogin(t *testing.T) {
	e := newEnv(t, "000000")
	ctx := context.Background()
	if err := e.svc.SendLoginCode(ctx, "13800000031", "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	_, r, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000031", Code: "000000", IP: "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	state := func() (ip string, at time.Time, devSeen map[string]time.Time) {
		t.Helper()
		var atp *time.Time
		if err := e.pool.QueryRow(ctx, `SELECT last_login_ip, last_login_at FROM users WHERE id = $1`, r.ID).Scan(&ip, &atp); err != nil || atp == nil {
			t.Fatalf("users = %v %v", atp, err)
		}
		rows, err := e.pool.Query(ctx, `SELECT device_id, last_seen_at FROM user_devices WHERE user_id = $1`, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		devSeen = map[string]time.Time{}
		for rows.Next() {
			var d string
			var s time.Time
			if err := rows.Scan(&d, &s); err != nil {
				t.Fatal(err)
			}
			devSeen[d] = s
		}
		return ip, *atp, devSeen
	}
	loginAt := e.clock.Now()

	// 10 分钟后带新设备（大写）启动会话：设备按小写记下；同 IP 且一小时内登录过，last_login 不动
	e.clock.Advance(10 * time.Minute)
	t1 := e.clock.Now()
	if err := e.svc.TouchSession(ctx, r.ID, "1.2.3.4", strings.ToUpper(devA)); err != nil {
		t.Fatal(err)
	}
	if ip, at, devs := state(); ip != "1.2.3.4" || !at.Equal(loginAt) || len(devs) != 1 || !devs[devA].Equal(t1) {
		t.Fatalf("after first touch: ip=%q at=%v devs=%v", ip, at, devs)
	}
	if has, err := e.svc.HasDevice(ctx, e.pool, r.ID, devA); err != nil || !has {
		t.Fatalf("HasDevice = %v %v", has, err)
	}
	// 再过 10 分钟：一小时内不重复写设备
	e.clock.Advance(10 * time.Minute)
	if err := e.svc.TouchSession(ctx, r.ID, "1.2.3.4", devA); err != nil {
		t.Fatal(err)
	}
	if _, at, devs := state(); !at.Equal(loginAt) || !devs[devA].Equal(t1) {
		t.Fatalf("throttled touch wrote: at=%v devs=%v", at, devs)
	}
	// IP 变化：立即刷新 last_login_*
	t2 := e.clock.Now()
	if err := e.svc.TouchSession(ctx, r.ID, "5.6.7.8", devA); err != nil {
		t.Fatal(err)
	}
	if ip, at, devs := state(); ip != "5.6.7.8" || !at.Equal(t2) || !devs[devA].Equal(t1) {
		t.Fatalf("ip change: ip=%q at=%v devs=%v", ip, at, devs)
	}
	// 超过一小时：同 IP 同设备也刷新
	e.clock.Advance(61 * time.Minute)
	t3 := e.clock.Now()
	if err := e.svc.TouchSession(ctx, r.ID, "5.6.7.8", devA); err != nil {
		t.Fatal(err)
	}
	if ip, at, devs := state(); ip != "5.6.7.8" || !at.Equal(t3) || !devs[devA].Equal(t3) {
		t.Fatalf("after an hour: ip=%q at=%v devs=%v", ip, at, devs)
	}
	// 非 UUID 不记；空 IP 不覆盖
	e.clock.Advance(2 * time.Hour)
	if err := e.svc.TouchSession(ctx, r.ID, "", "not-a-uuid"); err != nil {
		t.Fatal(err)
	}
	if ip, at, devs := state(); ip != "5.6.7.8" || !at.Equal(t3) || len(devs) != 1 {
		t.Fatalf("garbage touch: ip=%q at=%v devs=%v", ip, at, devs)
	}
	// 10 台上限：再来 10 台新设备后，最旧的 devA 被删
	for i := 0; i < 10; i++ {
		e.clock.Advance(time.Second)
		if err := e.svc.TouchSession(ctx, r.ID, "5.6.7.8", fmt.Sprintf("00000000-0000-4000-8000-%012d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, devs := state(); len(devs) != 10 || !devs[devA].IsZero() {
		t.Fatalf("cap: %d devices, devA kept=%v", len(devs), !devs[devA].IsZero())
	}
	// 封禁用户：不更新 last_login_*，不记设备
	if _, err := e.pool.Exec(ctx, `UPDATE users SET status = 'banned' WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	ipB, atB, _ := state()
	e.clock.Advance(2 * time.Hour)
	if err := e.svc.TouchSession(ctx, r.ID, "9.9.9.9", "99999999-0000-4000-8000-000000000000"); err != nil {
		t.Fatal(err)
	}
	if ip, at, devs := state(); ip != ipB || !at.Equal(atB) || !devs["99999999-0000-4000-8000-000000000000"].IsZero() {
		t.Fatalf("banned touch: ip=%q at=%v devs=%v", ip, at, devs)
	}
}
