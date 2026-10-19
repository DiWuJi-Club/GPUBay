package referral_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"spark-cloud/internal/referral"
)

func TestGenerateCodeShapeAndNormalize(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := referral.GenerateCode()
		if len(c) != 8 || strings.Trim(c, referral.CodeAlphabet) != "" {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 199 {
		t.Fatalf("codes not random enough: %d distinct", len(seen))
	}
	cases := map[string]string{
		" ab0lio-z ": "", // 含 '-' 非法
		"abcdefgh":   "ABCDEFGH",
		" o1lI0Zy9 ": "01110ZY9",
		"ABCDEFG":    "", // 7 位
		"ABCDEFGHJ":  "", // 9 位
		"ABCD EFGH":  "ABCDEFGH",
		"abcdefgu":   "", // U 不在字母表
		"":           "",
	}
	for in, want := range cases {
		got, ok := referral.NormalizeCode(in)
		if (want == "") == ok || got != want {
			t.Errorf("NormalizeCode(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	if referral.MaskPhone("13812345678") != "138****5678" || referral.MaskPhone("12345") != "123****" {
		t.Fatal("MaskPhone")
	}
}

func TestEnsureCodeIdempotentUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	uid := e.user("13800000001", false)
	var wg sync.WaitGroup
	codes := make([]string, 10)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := e.svc.EnsureCode(context.Background(), uid)
			if err != nil {
				t.Error(err)
				return
			}
			codes[i] = a.Code
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		if c != codes[0] || len(c) != 8 {
			t.Fatalf("codes = %v", codes)
		}
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM referral_accounts WHERE user_id = $1`, uid).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d %v", n, err)
	}
	a, _ := e.svc.EnsureCode(e.ctx, uid)
	if a.Status != "active" || a.Debt != 0 || a.UserID != uid {
		t.Fatalf("account = %+v", a)
	}
}

func TestLookupCode(t *testing.T) {
	e := newEnv(t)
	inviter := e.user("13812345678", true)
	a, _ := e.svc.EnsureCode(e.ctx, inviter)
	valid, masked, err := e.svc.LookupCode(e.ctx, strings.ToLower(" "+a.Code+" "))
	if err != nil || !valid || masked != "138****5678" {
		t.Fatalf("lookup = %v %q %v", valid, masked, err)
	}
	if valid, _, err := e.svc.LookupCode(e.ctx, "ZZZZZZZZ"); err != nil || valid {
		t.Fatalf("unknown = %v %v", valid, err)
	}
	if valid, _, err := e.svc.LookupCode(e.ctx, "bad code!"); err != nil || valid {
		t.Fatalf("garbage = %v %v", valid, err)
	}
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET status = 'frozen' WHERE user_id = $1`, inviter)
	if valid, _, _ := e.svc.LookupCode(e.ctx, a.Code); valid {
		t.Fatal("frozen account must be invalid")
	}
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET status = 'active' WHERE user_id = $1`, inviter)
	e.pool.Exec(e.ctx, `UPDATE users SET status = 'banned' WHERE id = $1`, inviter)
	if valid, _, _ := e.svc.LookupCode(e.ctx, a.Code); valid {
		t.Fatal("banned owner must be invalid")
	}
}
