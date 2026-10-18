package ingress_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"spark-cloud/internal/ingress"
	"spark-cloud/internal/platform/clock"
)

var secret = bytes.Repeat([]byte("s"), 32)

// flipSig 翻转签名中间一个字节（改末尾字符在 1/256 情况下不构成篡改）。
func flipSig(tok string) string {
	dot := strings.IndexByte(tok, '.')
	sig, _ := base64.RawURLEncoding.DecodeString(tok[dot+1:])
	sig[len(sig)/2] ^= 0xff
	return tok[:dot+1] + base64.RawURLEncoding.EncodeToString(sig)
}

func TestTicketSingleUse(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := ingress.NewSigner(secret, clk)
	c := ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7, SessionHash: "h"}
	tok := s.Ticket(c)
	got, err := s.VerifyTicket(tok)
	if err != nil || got.InstanceID != c.InstanceID || got.Port != 8888 || got.UserID != 7 || got.SessionHash != "h" || got.Nonce == "" {
		t.Fatalf("verify = %+v %v", got, err)
	}
	if _, err := s.VerifyTicket(tok); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("replay = %v", err)
	}
	tok2 := s.Ticket(c)
	clk.Advance(ingress.TicketTTL + time.Second)
	if _, err := s.VerifyTicket(tok2); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("expired = %v", err)
	}
	tok3 := s.Ticket(c)
	if _, err := s.VerifyTicket(flipSig(tok3)); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("tampered = %v", err)
	}
	if _, err := s.VerifyTicket(tok3); err != nil { // 篡改的那次不消耗原票据的 nonce
		t.Fatalf("untampered after tamper attempt = %v", err)
	}
	for _, bad := range []string{"garbage", "", ".", "a.b", tok3 + "x", "." + strings.SplitN(tok3, ".", 2)[1]} {
		if _, err := s.VerifyTicket(bad); !errors.Is(err, ingress.ErrToken) {
			t.Fatalf("%q = %v", bad, err)
		}
	}
	// 票据与 Cookie 用不同密钥：票据不能当 Cookie 用，Cookie 也不能当票据用
	if _, err := s.VerifyCookie(s.Ticket(c)); !errors.Is(err, ingress.ErrToken) {
		t.Fatal("ticket accepted as cookie")
	}
	if _, err := s.VerifyTicket(s.Cookie(c)); !errors.Is(err, ingress.ErrToken) {
		t.Fatal("cookie accepted as ticket")
	}
	other := ingress.NewSigner(bytes.Repeat([]byte("x"), 32), clk)
	if _, err := other.VerifyCookie(s.Cookie(c)); !errors.Is(err, ingress.ErrToken) {
		t.Fatal("cookie accepted with other secret")
	}
	if _, err := other.VerifyTicket(s.Ticket(c)); !errors.Is(err, ingress.ErrToken) {
		t.Fatal("ticket accepted with other secret")
	}
	// 缺会话哈希的票据（不该被签发）也不被接受
	if _, err := s.VerifyTicket(s.Ticket(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7})); !errors.Is(err, ingress.ErrToken) {
		t.Fatal("ticket without session hash accepted")
	}
}

// 同一票据被并发提交：恰好一次成功。
func TestTicketConcurrentReplay(t *testing.T) {
	s := ingress.NewSigner(secret, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	tok := s.Ticket(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7, SessionHash: "h"})
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.VerifyTicket(tok); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("successes = %d, want 1", ok.Load())
	}
}

// 已用 nonce 只保留到票据过期：nonce 表不会无限增长。
func TestTicketNonceStoreBounded(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := ingress.NewSigner(secret, clk)
	c := ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7, SessionHash: "h"}
	for round := 0; round < 5; round++ {
		for i := 0; i < 100; i++ {
			if _, err := s.VerifyTicket(s.Ticket(c)); err != nil {
				t.Fatal(err)
			}
		}
		if n := ingress.UsedNonces(s); n > 100 {
			t.Fatalf("round %d: nonce store = %d, want ≤ 100 (expired nonces not pruned)", round, n)
		}
		clk.Advance(ingress.TicketTTL + time.Second)
	}
	if _, err := s.VerifyTicket(s.Ticket(c)); err != nil {
		t.Fatal(err)
	}
	if n := ingress.UsedNonces(s); n != 1 {
		t.Fatalf("after expiry nonce store = %d, want 1", n)
	}
}

func TestCookieLifetime(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s := ingress.NewSigner(secret, clk)
	ck := s.Cookie(ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 80, UserID: 1, SessionHash: "h"})
	if strings.ContainsAny(ck, " ;,\"") {
		t.Fatalf("cookie value has separators: %q", ck)
	}
	for i := 0; i < 3; i++ { // Cookie 可重复验证
		if _, err := s.VerifyCookie(ck); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.VerifyCookie(flipSig(ck)); !errors.Is(err, ingress.ErrToken) {
		t.Fatal("tampered cookie accepted")
	}
	clk.Advance(ingress.CookieTTL + time.Second)
	if _, err := s.VerifyCookie(ck); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("expired cookie = %v", err)
	}
}

// 过期边界：exp 前一刻可用、到 exp 即拒；nonce 清理按秒节流，但重放判定不受清理时机影响。
func TestTicketExpiryEdgeAndPruneThrottle(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.NewFake(t0)
	s := ingress.NewSigner(secret, clk)
	c := ingress.Claims{InstanceID: "ins-a1b2c3d4", Port: 8888, UserID: 7, SessionHash: "h"}
	edge := s.Ticket(c) // exp = t0+60s
	late := s.Ticket(c)
	for i := 0; i < 10; i++ {
		if _, err := s.VerifyTicket(s.Ticket(c)); err != nil { // 10 个已用 nonce，exp = t0+60s
			t.Fatal(err)
		}
	}
	clk.Advance(ingress.TicketTTL - 500*time.Millisecond) // t0+59.5s：仍有效
	if _, err := s.VerifyTicket(edge); err != nil {
		t.Fatalf("just before exp = %v", err)
	}
	clk.Advance(400 * time.Millisecond) // t0+59.9s：距上次清理 < 1s，不清理；重放仍被拒
	if _, err := s.VerifyTicket(edge); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("replay before exp = %v", err)
	}
	clk.Advance(100 * time.Millisecond) // t0+60s：恰好到期
	if _, err := s.VerifyTicket(late); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("at exp = %v", err)
	}
	fresh := s.Ticket(c)
	if _, err := s.VerifyTicket(fresh); err != nil { // t0+60s，距上次清理 0.5s：已过期的 11 个 nonce 尚未清理
		t.Fatal(err)
	}
	if n := ingress.UsedNonces(s); n != 12 {
		t.Fatalf("within prune interval nonce store = %d, want 12", n)
	}
	clk.Advance(time.Second) // 距上次清理 ≥ 1s：清理过期 nonce，未过期的保留
	if _, err := s.VerifyTicket(s.Ticket(c)); err != nil {
		t.Fatal(err)
	}
	if n := ingress.UsedNonces(s); n != 2 {
		t.Fatalf("after prune nonce store = %d, want 2", n)
	}
	if _, err := s.VerifyTicket(fresh); !errors.Is(err, ingress.ErrToken) {
		t.Fatalf("replay after prune = %v", err)
	}
}
