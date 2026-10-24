package llm_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/llm"
)

func TestAuthorizeErrorMatrix(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name    string
		setup   func(userID, keyID int64)
		status  int
		typ     string
		code    string
		msgPart string
	}{
		{name: "ok"},
		{name: "revoked", setup: func(_, k int64) {
			e.exec(`UPDATE llm_api_keys SET status = 'revoked', revoked_at = $2 WHERE id = $1`, k, e.clk.Now())
		}, status: 401, typ: "authentication_error", code: "invalid_api_key"},
		{name: "disabled", setup: func(_, k int64) { e.exec(`UPDATE llm_api_keys SET status = 'disabled' WHERE id = $1`, k) },
			status: 401, typ: "authentication_error", code: "invalid_api_key"},
		{name: "expired", setup: func(_, k int64) { e.exec(`UPDATE llm_api_keys SET expires_at = $2 WHERE id = $1`, k, e.clk.Now()) },
			status: 401, typ: "authentication_error", code: "invalid_api_key"},
		{name: "banned beats kyc", setup: func(u, _ int64) {
			e.exec(`UPDATE users SET status = 'banned', kyc_status = 'none' WHERE id = $1`, u)
		}, status: 403, typ: "permission_error", code: "account_banned"},
		{name: "kyc", setup: func(u, _ int64) { e.exec(`UPDATE users SET kyc_status = 'rejected' WHERE id = $1`, u) },
			status: 403, typ: "permission_error", code: "kyc_required"},
		{name: "blocked", setup: func(u, _ int64) {
			e.exec(`UPDATE llm_accounts SET blocked = true, blocked_reason = '涉嫌滥用', debt = 5 WHERE user_id = $1`, u)
		}, status: 403, typ: "permission_error", code: "account_blocked", msgPart: "涉嫌滥用"},
		{name: "debt", setup: func(u, _ int64) { e.exec(`UPDATE llm_accounts SET debt = 1234 WHERE user_id = $1`, u) },
			status: 402, typ: "insufficient_quota", code: "insufficient_quota", msgPart: "账户有欠费 ¥12.34，请充值后再试"},
		{name: "debt one fen", setup: func(u, _ int64) { e.exec(`UPDATE llm_accounts SET debt = 1 WHERE user_id = $1`, u) },
			status: 402, typ: "insufficient_quota", code: "insufficient_quota", msgPart: "¥0.01"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := e.verifiedUser()
			plain, k := e.createKey(u)
			if c.setup != nil {
				c.setup(u, k.ID)
			}
			ac, err := e.svc.Authorize(e.ctx, llm.HashKey(plain), e.clk.Now())
			if c.status == 0 {
				if err != nil {
					t.Fatal(err)
				}
				if ac.UserID != u || ac.Key.ID != k.ID || ac.Account.UserID != u || ac.UserStatus != "active" || ac.KYCStatus != "verified" ||
					ac.Limits != (llm.Limits{RPM: llm.DefaultRPM, TPM: llm.DefaultTPM, Concurrency: llm.DefaultConcurrency}) {
					t.Fatalf("ctx = %+v", ac)
				}
				return
			}
			var ae *llm.AuthError
			if !errors.As(err, &ae) {
				t.Fatalf("err = %T %v", err, err)
			}
			if ae.Status != c.status || ae.Type != c.typ || ae.Code != c.code || !strings.Contains(ae.Message, c.msgPart) || ae.Message == "" {
				t.Fatalf("got %+v", ae)
			}
			if ae.Error() == "" {
				t.Fatal("empty Error()")
			}
		})
	}

	// 空哈希 / 未知哈希 / 未来 1 秒才过期
	for _, h := range []string{"", llm.HashKey("sk-spark-nope")} {
		if _, err := e.svc.Authorize(e.ctx, h, e.clk.Now()); err == nil || err.(*llm.AuthError).Code != "invalid_api_key" {
			t.Fatalf("hash %q: %v", h, err)
		}
	}
	u := e.verifiedUser()
	exp := e.clk.Now().Add(time.Second)
	plain, _ := e.createKey(u, func(in *llm.KeyInput) { in.ExpiresAt = &exp })
	if code, _ := e.authCode(e.svc, plain); code != 0 {
		t.Fatal("not yet expired")
	}
	e.clk.Advance(time.Second) // 缓存命中也按 now 判过期
	if code, c := e.authCode(e.svc, plain); code != 401 || c != "invalid_api_key" {
		t.Fatalf("expired from cache: %d %s", code, c)
	}

	// 账户限额覆盖；Key 行没有账户（直接插入的历史数据）时按全默认
	u2 := e.verifiedUser()
	plain2, _ := e.createKey(u2)
	e.exec(`UPDATE llm_accounts SET rpm_limit = 5, concurrency_limit = 2 WHERE user_id = $1`, u2)
	ac, err := e.svc.Authorize(e.ctx, llm.HashKey(plain2), e.clk.Now())
	if err != nil || ac.Limits != (llm.Limits{RPM: 5, TPM: llm.DefaultTPM, Concurrency: 2}) {
		t.Fatalf("limits = %+v %v", ac.Limits, err)
	}
	u3 := e.verifiedUser()
	p3, h3, hint3 := llm.GenerateKey()
	e.exec(`INSERT INTO llm_api_keys (user_id, name, key_hash, key_hint, created_at, updated_at) VALUES ($1, 'raw', $2, $3, $4, $4)`,
		u3, h3, hint3, e.clk.Now())
	ac, err = e.svc.Authorize(e.ctx, llm.HashKey(p3), e.clk.Now())
	if err != nil || ac.Account.UserID != u3 || ac.Account.Debt != 0 || ac.Limits.RPM != llm.DefaultRPM || ac.Key.ModelSlugs == nil {
		t.Fatalf("no account row: %+v %v", ac, err)
	}
}

func TestAuthorizeCacheTTL(t *testing.T) {
	e := newEnv(t)
	other := e.newService(nil) // 「另一个进程」：它的撤销不会让 e.svc 的缓存立即失效
	u := e.verifiedUser()
	plain, k := e.createKey(u)
	if code, _ := e.authCode(e.svc, plain); code != 0 {
		t.Fatal("fresh key must work immediately")
	}
	if err := other.RevokeKey(e.ctx, u, k.ID, ""); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(29 * time.Second)
	if code, _ := e.authCode(e.svc, plain); code != 0 {
		t.Fatal("cached for 29s")
	}
	if code, _ := e.authCode(other, plain); code != 401 {
		t.Fatal("other process sees revoke at once")
	}
	e.clk.Advance(2 * time.Second)
	if code, c := e.authCode(e.svc, plain); code != 401 || c != "invalid_api_key" {
		t.Fatalf("after 31s: %d %s", code, c)
	}

	// 封禁同样最多 30 秒生效
	u2 := e.verifiedUser()
	plain2, k2 := e.createKey(u2)
	if code, _ := e.authCode(e.svc, plain2); code != 0 {
		t.Fatal("precondition")
	}
	e.exec(`UPDATE users SET status = 'banned' WHERE id = $1`, u2)
	e.clk.Advance(10 * time.Second)
	if code, _ := e.authCode(e.svc, plain2); code != 0 {
		t.Fatal("ban cached")
	}
	e.clk.Advance(21 * time.Second)
	if code, c := e.authCode(e.svc, plain2); code != 403 || c != "account_banned" {
		t.Fatalf("ban after 31s: %d %s", code, c)
	}

	// InvalidateKey：本进程立即失效
	e.exec(`UPDATE users SET status = 'active' WHERE id = $1`, u2)
	e.svc.InvalidateKey(k2.KeyHash)
	if code, _ := e.authCode(e.svc, plain2); code != 0 {
		t.Fatal("unbanned after invalidate")
	}
	disabled := llm.KeyDisabled
	if _, err := other.UpdateKey(e.ctx, u2, k2.ID, llm.KeyUpdate{Status: &disabled}, ""); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.authCode(e.svc, plain2); code != 0 {
		t.Fatal("still cached")
	}
	e.svc.InvalidateKey(llm.HashKey(plain2))
	if code, c := e.authCode(e.svc, plain2); code != 401 || c != "invalid_api_key" {
		t.Fatalf("after InvalidateKey: %d %s", code, c)
	}
	// 同进程的启用 / 撤销调用自己失效缓存
	active := llm.KeyActive
	if _, err := e.svc.UpdateKey(e.ctx, u2, k2.ID, llm.KeyUpdate{Status: &active}, ""); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.authCode(e.svc, plain2); code != 0 {
		t.Fatal("re-enabled in-process")
	}
	if err := e.svc.RevokeKey(e.ctx, u2, k2.ID, ""); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.authCode(e.svc, plain2); code != 401 {
		t.Fatal("revoked in-process")
	}
}

// 查库在撤销提交前、写缓存在 InvalidateKey 之后：失效代数让这条旧数据不留在缓存里，下一次立即 401。
func TestAuthorizeInvalidateRace(t *testing.T) {
	e := newEnv(t)
	u := e.verifiedUser()
	plain, k := e.createKey(u)
	fired := false
	llm.SetAuthorizeAfterLoadHook(e.svc, func() {
		if fired {
			return
		}
		fired = true
		if err := e.svc.RevokeKey(e.ctx, u, k.ID, ""); err != nil {
			t.Error(err)
		}
	})
	// 在途的这一次读到的是撤销前的数据，放行
	if code, _ := e.authCode(e.svc, plain); code != 0 || !fired {
		t.Fatalf("in-flight lookup: %d fired=%v", code, fired)
	}
	if code, c := e.authCode(e.svc, plain); code != 401 || c != "invalid_api_key" {
		t.Fatalf("stale entry survived invalidate: %d %s", code, c)
	}
}

func TestInvalidateUser(t *testing.T) {
	e := newEnv(t)
	u, other := e.verifiedUser(), e.verifiedUser()
	p1, _ := e.createKey(u)
	p2, _ := e.createKey(u)
	p3, _ := e.createKey(other)
	for _, p := range []string{p1, p2, p3} {
		if code, _ := e.authCode(e.svc, p); code != 0 {
			t.Fatal("precondition")
		}
	}
	e.exec(`UPDATE llm_accounts SET debt = 50 WHERE user_id IN ($1, $2)`, u, other)
	e.svc.InvalidateUser(u)
	for _, p := range []string{p1, p2} {
		if code, c := e.authCode(e.svc, p); code != 402 || c != "insufficient_quota" {
			t.Fatalf("after InvalidateUser: %d %s", code, c)
		}
	}
	if code, _ := e.authCode(e.svc, p3); code != 0 {
		t.Fatal("other user's entry must stay cached")
	}
}
