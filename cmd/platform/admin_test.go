package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/referral"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func newAdminService(t *testing.T) (*admin.Service, *clock.Fake) {
	t.Helper()
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(dbtest.T0)
	return admin.NewService(admin.Deps{Pool: dbtest.New(t), Clock: clk, Box: box}), clk
}

var uriRe = regexp.MustCompile(`otpauth://\S+`)

// scripted 依次返回两次密码，遇到动态码提示时从已输出的 otpauth 链接计算当前动态码。
func scripted(t *testing.T, out *bytes.Buffer, clk *clock.Fake, pw1, pw2 string) prompter {
	answers := []string{pw1, pw2}
	return func(label string, secret bool) (string, error) {
		if strings.Contains(label, "动态码") {
			u, err := url.Parse(uriRe.FindString(out.String()))
			if err != nil {
				t.Fatal(err)
			}
			return admin.TOTPCode(u.Query().Get("secret"), admin.TOTPStep(clk.Now()))
		}
		if !secret {
			t.Fatalf("password prompt %q must not echo", label)
		}
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}
}

func TestAdminCreateBindsTOTPAndPrintsRecoveryCodes(t *testing.T) {
	svc, clk := newAdminService(t)
	ctx := context.Background()
	var out bytes.Buffer
	pw := "correct horse battery"
	if err := runAdmin(ctx, svc, nil, nil, []string{"create", "--username", "root"}, &out, scripted(t, &out, clk, pw, pw)); err != nil {
		t.Fatalf("create: %v\n%s", err, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "otpauth://totp/Spark%20Cloud:root?") || !strings.Contains(text, "恢复码") {
		t.Fatalf("output = %s", text)
	}
	if got := len(regexp.MustCompile(`(?m)^  [a-z0-9]{5}-[a-z0-9]{5}$`).FindAllString(text, -1)); got != admin.RecoveryCodeCount {
		t.Fatalf("printed %d recovery codes", got)
	}
	a, err := svc.FindByUsername(ctx, "root")
	if err != nil || a.Role != admin.RoleSuper || !a.TOTPConfirmed {
		t.Fatalf("admin = %+v, %v", a, err)
	}

	out.Reset()
	if err := runAdmin(ctx, svc, nil, nil, []string{"reset-totp", "--username", "root"}, &out, nil); err != nil {
		t.Fatal(err)
	}
	if a, _ := svc.FindByUsername(ctx, "root"); a.TOTPConfirmed {
		t.Fatal("TOTP not reset")
	}
	res, err := svc.Login(ctx, "root", pw, "127.0.0.1")
	if err != nil || !res.NeedTOTPSetup {
		t.Fatalf("login after reset = %+v, %v", res, err)
	}
}

func TestAdminCreateErrors(t *testing.T) {
	svc, clk := newAdminService(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := runAdmin(ctx, svc, nil, nil, nil, &out, nil); err == nil || !strings.Contains(err.Error(), "用法") {
		t.Fatalf("no args = %v", err)
	}
	if err := runAdmin(ctx, svc, nil, nil, []string{"create"}, &out, nil); err == nil || !strings.Contains(err.Error(), "--username") {
		t.Fatalf("missing username = %v", err)
	}
	err := runAdmin(ctx, svc, nil, nil, []string{"create", "--username", "root"}, &out, scripted(t, &out, clk, "correct horse battery", "different password!"))
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("mismatch = %v", err)
	}
	if _, err := svc.FindByUsername(ctx, "root"); err == nil {
		t.Fatal("admin must not be created when passwords differ")
	}
	if err := runAdmin(ctx, svc, nil, nil, []string{"reset-totp", "--username", "ghost"}, &out, nil); err == nil {
		t.Fatal("reset-totp for unknown admin must fail")
	}
}

func TestListensOnAllInterfaces(t *testing.T) {
	for addr, want := range map[string]bool{":8081": true, "0.0.0.0:8081": true, "[::]:8081": true, "127.0.0.1:8081": false, "10.0.0.5:8081": false} {
		if got := listensOnAllInterfaces(addr); got != want {
			t.Fatalf("%s = %v, want %v", addr, got, want)
		}
	}
}

func TestAdminGenUpstreamKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "up.key")
	var out bytes.Buffer
	if err := runAdmin(context.Background(), nil, nil, nil, []string{"gen-upstream-key", "--path", path}, &out, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".pub"); err != nil || !strings.Contains(out.String(), "ssh-ed25519 ") {
		t.Fatalf("output = %s, err = %v", out.String(), err)
	}
	if err := runAdmin(context.Background(), nil, nil, nil, []string{"gen-upstream-key"}, &out, nil); err == nil {
		t.Fatal("missing --path accepted")
	}
}

func TestAdminSSHHostKey(t *testing.T) {
	pool := dbtest.New(t)
	box, _ := secretbox.New(dbtest.Key)
	if _, err := pki.EnsureCA(context.Background(), pool, box, clock.NewFake(dbtest.T0)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := adminSSHHostKey(context.Background(), pool, box, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "公钥：ssh-ed25519 ") || !strings.Contains(out.String(), "指纹：SHA256:") {
		t.Fatalf("output = %s", out.String())
	}
}

func TestReferralBackfillFingerprints(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	box, _ := secretbox.New(dbtest.Key)
	key, _ := referral.DeriveFingerprintKey(dbtest.Key)
	u1 := dbtest.CreateUser(t, pool, "13800000031", true)
	u2 := dbtest.CreateUser(t, pool, "13800000032", true)
	u3 := dbtest.CreateUser(t, pool, "13800000033", true)
	u4 := dbtest.CreateUser(t, pool, "13800000034", true)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO kyc_records (user_id, kind, real_name, id_number_enc, result, created_at, updated_at) VALUES ($1, 'personal', 'a', $2, 'verified', $3, $3)`, []any{u1, box.Seal([]byte("110101199001011234")), dbtest.T0}},
		{`INSERT INTO kyc_records (user_id, kind, real_name, license_no, result, created_at, updated_at) VALUES ($1, 'enterprise', 'b', '91110000MA01ABCD2X', 'verified', $2, $2)`, []any{u2, dbtest.T0}},
		{`INSERT INTO kyc_records (user_id, kind, real_name, id_number_enc, result, created_at, updated_at) VALUES ($1, 'personal', 'c', '\x00', 'verified', $2, $2)`, []any{u3, dbtest.T0}}, // 解密失败
		{`INSERT INTO kyc_records (user_id, kind, real_name, result, created_at, updated_at) VALUES ($1, 'enterprise', 'd', 'verified', $2, $2)`, []any{u4, dbtest.T0}},                      // 无号码
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	var noNumber int64
	if err := pool.QueryRow(ctx, `SELECT id FROM kyc_records WHERE user_id = $1`, u4).Scan(&noNumber); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runReferral(ctx, pool, box, key, []string{"backfill-fingerprints"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "已回填 2 条，跳过 2 条") || !strings.Contains(out.String(), fmt.Sprintf("记录 %d：无号码，跳过", noNumber)) {
		t.Fatalf("out = %s", out.String())
	}
	var fp string
	if err := pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1`, u1).Scan(&fp); err != nil || fp != referral.Fingerprint(key, "kyc", "110101199001011234") {
		t.Fatalf("u1 fp = %q", fp)
	}
	if err := pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1`, u2).Scan(&fp); err != nil || fp != referral.Fingerprint(key, "lic", "91110000MA01ABCD2X") {
		t.Fatalf("u2 fp = %q", fp)
	}
	// 幂等：第二次 0 条
	out.Reset()
	if err := runReferral(ctx, pool, box, key, []string{"backfill-fingerprints"}, &out); err != nil || !strings.Contains(out.String(), "已回填 0 条") {
		t.Fatalf("second run = %s", out.String())
	}
	if err := runReferral(ctx, pool, box, key, []string{"bogus"}, &out); err == nil {
		t.Fatal("unknown subcommand must fail")
	}
}

// 子命令与服务（app.Build）必须用同一份密钥字节派生指纹密钥：config.Load 解出的 32 字节原始密钥，而不是 base64 文本。
// 否则回填写入的指纹与线上实名写入的指纹永远不相等，同身份判定对存量用户失效。
func TestReferralKeysUseDecodedSecret(t *testing.T) {
	raw := bytes.Repeat([]byte{0x5a}, 32)
	enc := base64.StdEncoding.EncodeToString(raw)
	cfg, err := config.Load(func(k string) string {
		switch k {
		case "SPARK_SECRET_KEY":
			return enc
		case "SPARK_DATABASE_URL":
			return "postgres://unused"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	box, key, err := referralKeys(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := referral.DeriveFingerprintKey(raw)
	if !bytes.Equal(key, want) {
		t.Fatal("fingerprint key must be derived from the decoded SPARK_SECRET_KEY bytes")
	}
	if wrong, _ := referral.DeriveFingerprintKey([]byte(enc)); bytes.Equal(key, wrong) {
		t.Fatal("fingerprint key must not be derived from the base64 text")
	}
	ref, _ := secretbox.New(raw)
	plain, err := box.Open(ref.Seal([]byte("110101199001011234")))
	if err != nil || string(plain) != "110101199001011234" {
		t.Fatalf("box must decrypt what the service encrypted: %q %v", plain, err)
	}
}
