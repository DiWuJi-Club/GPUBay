package identity_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/sms"
)

func keyLine(t *testing.T, pub any, comment string) string {
	t.Helper()
	p, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p)))
	if comment != "" {
		line += " " + comment
	}
	return line
}

func ed25519Line(t *testing.T, comment string) string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return keyLine(t, pub, comment)
}

func rsaLine(t *testing.T, bits int) string {
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return keyLine(t, &k.PublicKey, "")
}

func TestParseSSHPublicKeyMatrix(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ec384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	ec521, _ := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	ok := []struct{ name, raw, wantComment string }{
		{"ed25519 with comment", ed25519Line(t, "me@mac"), "me@mac"},
		{"ed25519 CRLF", ed25519Line(t, "win") + "\r\n", "win"},
		{"ed25519 leading/trailing spaces", "  " + ed25519Line(t, "") + "  \n", ""},
		{"ecdsa p256", keyLine(t, &ec.PublicKey, "ec"), "ec"},
		{"rsa 2048", rsaLine(t, 2048), ""},
		{"ecdsa p384", keyLine(t, &ec384.PublicKey, "ec384"), "ec384"},
		{"ecdsa p521", keyLine(t, &ec521.PublicKey, "ec521"), "ec521"},
		{"quoted options", `command="a b",no-pty ` + ed25519Line(t, "q"), "q"},
		{"options prefix", "no-pty " + ed25519Line(t, "opt"), "opt"},
	}
	for _, c := range ok {
		norm, fp, comment, err := identity.ParseSSHPublicKey(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.HasPrefix(fp, "SHA256:") || strings.Count(norm, " ") != 1 || strings.ContainsAny(norm, "\r\n") || comment != c.wantComment {
			t.Fatalf("%s: norm=%q fp=%q comment=%q", c.name, norm, fp, comment)
		}
	}
	bad := []struct{ name, raw string }{
		{"empty", "   "},
		{"junk", "ssh-ed25519 notbase64"},
		{"rsa 1024", rsaLine(t, 1024)},
		{"two lines", ed25519Line(t, "a") + "\n" + ed25519Line(t, "b")},
		{"private key", "-----BEGIN OPENSSH PRIVATE KEY-----"},
		{"sk ed25519", strings.Replace(ed25519Line(t, ""), "ssh-ed25519", "sk-ssh-ed25519@openssh.com", 1)},
		{"ed25519 cert", strings.Replace(ed25519Line(t, ""), "ssh-ed25519", "ssh-ed25519-cert-v01@openssh.com", 1)},
		{"rsa 8200", rsaLine(t, 8200)},
		{"over 16KiB", ed25519Line(t, strings.Repeat("a", 17*1024))},
		{"dss type", strings.Replace(ed25519Line(t, ""), "ssh-ed25519", "ssh-dss", 1)},
	}
	for _, c := range bad {
		if _, _, _, err := identity.ParseSSHPublicKey(c.raw); !errors.Is(err, apperr.ErrSSHKeyInvalid) {
			t.Fatalf("%s: err = %v, want ErrSSHKeyInvalid", c.name, err)
		}
	}
}

func TestSSHKeyCRUDLimitAndLookup(t *testing.T) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	e := struct {
		svc   *identity.Service
		clock *clock.Fake
	}{identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: &sms.Mock{}}), clk}
	uid := dbtest.CreateUser(t, pool, "13700000001", true)
	other := dbtest.CreateUser(t, pool, "13700000002", true)
	ctx := context.Background()

	line := ed25519Line(t, "laptop")
	k, err := e.svc.AddSSHKey(ctx, uid, "", line, "1.2.3.4")
	if err != nil || k.Name != "laptop" || k.UserID != uid || k.LastUsedAt != nil {
		t.Fatalf("add = %+v, %v", k, err)
	}
	if _, err := e.svc.AddSSHKey(ctx, uid, "dup", line+"\n", "1.2.3.4"); !errors.Is(err, apperr.ErrSSHKeyExists) {
		t.Fatalf("duplicate = %v", err)
	}
	if _, err := e.svc.AddSSHKey(ctx, other, "same key other user", line, ""); err != nil {
		t.Fatalf("other user same key = %v", err)
	}
	if _, err := e.svc.AddSSHKey(ctx, uid, strings.Repeat("长", 65), ed25519Line(t, ""), ""); err == nil {
		t.Fatal("65-char name accepted")
	}
	for i := 2; i <= identity.MaxSSHKeys; i++ {
		if _, err := e.svc.AddSSHKey(ctx, uid, "", ed25519Line(t, ""), ""); err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	if _, err := e.svc.AddSSHKey(ctx, uid, "", ed25519Line(t, ""), ""); !errors.Is(err, apperr.ErrSSHKeyLimit) {
		t.Fatalf("11th key = %v", err)
	}
	list, err := e.svc.ListSSHKeys(ctx, uid)
	if err != nil || len(list) != identity.MaxSSHKeys || list[0].ID != k.ID || list[1].Name != "密钥 2" {
		t.Fatalf("list = %d %v (%+v)", len(list), err, list)
	}
	found, err := e.svc.FindSSHKey(ctx, uid, k.Fingerprint)
	if err != nil || found.ID != k.ID || found.PublicKey != strings.Join(strings.Fields(line)[:2], " ") {
		t.Fatalf("find = %+v %v", found, err)
	}
	if _, err := e.svc.FindSSHKey(ctx, other, k.Fingerprint); err != nil { // 另一用户也添加了同一把钥匙
		t.Fatalf("find other = %v", err)
	}
	if _, err := e.svc.FindSSHKey(ctx, uid, "SHA256:nope"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("find missing = %v", err)
	}
	if err := e.svc.TouchSSHKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.svc.FindSSHKey(ctx, uid, k.Fingerprint); got.LastUsedAt == nil || !got.LastUsedAt.Equal(e.clock.Now()) {
		t.Fatalf("last_used_at = %v", got.LastUsedAt)
	}
	if err := e.svc.DeleteSSHKey(ctx, other, k.ID, ""); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("delete other's key = %v", err)
	}
	if err := e.svc.DeleteSSHKey(ctx, uid, k.ID, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.svc.CountSSHKeys(ctx, uid); n != identity.MaxSSHKeys-1 {
		t.Fatalf("count = %d", n)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor_type = 'user' AND actor_id = $1 AND action IN ('ssh_key.add', 'ssh_key.delete')`, uid).Scan(&audits); err != nil || audits != identity.MaxSSHKeys+1 {
		t.Fatalf("audit rows = %d %v", audits, err)
	}
}

func TestSSHKeyNameFromComment(t *testing.T) {
	pool := dbtest.New(t)
	svc := identity.NewService(identity.Deps{Pool: pool, Clock: clock.NewFake(dbtest.T0), SMS: &sms.Mock{}})
	uid := dbtest.CreateUser(t, pool, "13700000003", true)
	ctx := context.Background()
	k, err := svc.AddSSHKey(ctx, uid, "", ed25519Line(t, strings.Repeat("长", 100)), "")
	if err != nil || len([]rune(k.Name)) != 64 {
		t.Fatalf("long comment name = %q %v", k.Name, err)
	}
	k, err = svc.AddSSHKey(ctx, uid, "a\x01b\tc", ed25519Line(t, ""), "")
	if err != nil || k.Name != "abc" {
		t.Fatalf("control chars name = %q %v", k.Name, err)
	}
	k, err = svc.AddSSHKey(ctx, uid, "\x01", ed25519Line(t, ""), "")
	if err != nil || k.Name != "密钥 3" {
		t.Fatalf("default name = %q %v", k.Name, err)
	}
}

func TestSSHKeyConcurrentLimit(t *testing.T) {
	pool := dbtest.New(t)
	svc := identity.NewService(identity.Deps{Pool: pool, Clock: clock.NewFake(dbtest.T0), SMS: &sms.Mock{}})
	uid := dbtest.CreateUser(t, pool, "13700000004", true)
	ctx := context.Background()
	for i := 0; i < identity.MaxSSHKeys-1; i++ {
		if _, err := svc.AddSSHKey(ctx, uid, "", ed25519Line(t, ""), ""); err != nil {
			t.Fatal(err)
		}
	}
	const n = 6
	lines := make([]string, n)
	for i := range lines {
		lines[i] = ed25519Line(t, "")
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.AddSSHKey(ctx, uid, "", lines[i], "")
		}(i)
	}
	wg.Wait()
	okN, limN := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			okN++
		case errors.Is(err, apperr.ErrSSHKeyLimit):
			limN++
		default:
			t.Fatalf("unexpected err %v", err)
		}
	}
	if c, _ := svc.CountSSHKeys(ctx, uid); okN != 1 || limN != n-1 || c != identity.MaxSSHKeys {
		t.Fatalf("ok=%d limit=%d count=%d", okN, limN, c)
	}
}
