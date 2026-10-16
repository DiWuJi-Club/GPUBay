package admin_test

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/platform/apperr"
)

func TestHashAndVerifyPassword(t *testing.T) {
	h1 := admin.HashPassword("correct horse battery")
	h2 := admin.HashPassword("correct horse battery")
	if h1 == h2 {
		t.Fatal("hashes of the same password must differ (random salt)")
	}
	if !strings.HasPrefix(h1, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash format = %q", h1)
	}
	if !admin.VerifyPassword(h1, "correct horse battery") || !admin.VerifyPassword(h2, "correct horse battery") {
		t.Fatal("correct password rejected")
	}
	if admin.VerifyPassword(h1, "correct horse batterY") {
		t.Fatal("wrong password accepted")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA", "$argon2id$v=19$m=x,t=3,p=2$c2FsdA$aGFzaA"} {
		if admin.VerifyPassword(bad, "x") {
			t.Fatalf("malformed hash %q accepted", bad)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"12345678901": false, "123456789012": true, "十二个汉字的密码足够长了吧": true, strings.Repeat("a", 129): false,
	} {
		err := admin.ValidatePassword(pw)
		if ok != (err == nil) {
			t.Fatalf("ValidatePassword(%q) = %v, want ok=%v", pw, err, ok)
		}
		if err != nil && apperr.As(err).Code != "bad_request" {
			t.Fatalf("err code = %v", err)
		}
	}
}

// RFC 6238 附录 B 的 SHA1 测试向量（取 8 位结果的后 6 位）。
func TestTOTPCodeRFCVectors(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // ASCII "12345678901234567890"
	for unix, want := range map[int64]string{
		59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037",
	} {
		got, err := admin.TOTPCode(secret, admin.TOTPStep(time.Unix(unix, 0)))
		if err != nil || got != want {
			t.Fatalf("TOTPCode at %d = %q, %v; want %q", unix, got, err, want)
		}
	}
	if _, err := admin.TOTPCode("not base32!", 1); err == nil {
		t.Fatal("expected error for invalid secret")
	}
}

func TestMatchTOTPWindow(t *testing.T) {
	secret := admin.NewTOTPSecret()
	if len(secret) != 32 {
		t.Fatalf("secret length = %d, want 32", len(secret))
	}
	now := time.Date(2026, 1, 1, 0, 0, 15, 0, time.UTC)
	step := admin.TOTPStep(now)
	for offset, ok := range map[int64]bool{-2: false, -1: true, 0: true, 1: true, 2: false} {
		code, _ := admin.TOTPCode(secret, step+offset)
		got, matched := admin.MatchTOTP(secret, code, now)
		if matched != ok || (ok && got != step+offset) {
			t.Fatalf("offset %d: step=%d matched=%v", offset, got, matched)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := admin.MatchTOTP(secret, bad, now); ok {
			t.Fatalf("code %q matched", bad)
		}
	}
}

func TestMatchTOTPIgnoresSpaces(t *testing.T) {
	secret := admin.NewTOTPSecret()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	code, _ := admin.TOTPCode(secret, admin.TOTPStep(now))
	spaced := " " + code[:3] + " " + code[3:] + "\t"
	if _, ok := admin.MatchTOTP(secret, spaced, now); !ok {
		t.Fatalf("spaced code %q rejected", spaced)
	}
}

func TestOTPAuthURI(t *testing.T) {
	u, err := url.Parse(admin.OTPAuthURI("Spark Cloud", "root", "ABCDEF"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "otpauth" || u.Host != "totp" || u.Path != "/Spark Cloud:root" ||
		q.Get("secret") != "ABCDEF" || q.Get("issuer") != "Spark Cloud" || q.Get("digits") != "6" || q.Get("period") != "30" {
		t.Fatalf("uri = %s", u)
	}
}

func TestNewRecoveryCodes(t *testing.T) {
	codes := admin.NewRecoveryCodes()
	if len(codes) != admin.RecoveryCodeCount {
		t.Fatalf("len = %d", len(codes))
	}
	seen := map[string]bool{}
	re := regexp.MustCompile(`^[a-z0-9]{5}-[a-z0-9]{5}$`)
	for _, c := range codes {
		if !re.MatchString(c) || seen[c] {
			t.Fatalf("bad or duplicate code %q", c)
		}
		seen[c] = true
	}
}
