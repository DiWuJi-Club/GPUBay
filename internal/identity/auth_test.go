package identity_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/sms"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type env struct {
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
	return env{svc: svc, sms: mock, clock: clk}
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
	const n = 10
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
