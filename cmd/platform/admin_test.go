package main

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
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
	if err := runAdmin(ctx, svc, []string{"create", "--username", "root"}, &out, scripted(t, &out, clk, pw, pw)); err != nil {
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
	if err := runAdmin(ctx, svc, []string{"reset-totp", "--username", "root"}, &out, nil); err != nil {
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
	if err := runAdmin(ctx, svc, nil, &out, nil); err == nil || !strings.Contains(err.Error(), "用法") {
		t.Fatalf("no args = %v", err)
	}
	if err := runAdmin(ctx, svc, []string{"create"}, &out, nil); err == nil || !strings.Contains(err.Error(), "--username") {
		t.Fatalf("missing username = %v", err)
	}
	err := runAdmin(ctx, svc, []string{"create", "--username", "root"}, &out, scripted(t, &out, clk, "correct horse battery", "different password!"))
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("mismatch = %v", err)
	}
	if _, err := svc.FindByUsername(ctx, "root"); err == nil {
		t.Fatal("admin must not be created when passwords differ")
	}
	if err := runAdmin(ctx, svc, []string{"reset-totp", "--username", "ghost"}, &out, nil); err == nil {
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
	if err := runAdmin(context.Background(), nil, []string{"gen-upstream-key", "--path", path}, &out, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".pub"); err != nil || !strings.Contains(out.String(), "ssh-ed25519 ") {
		t.Fatalf("output = %s, err = %v", out.String(), err)
	}
	if err := runAdmin(context.Background(), nil, []string{"gen-upstream-key"}, &out, nil); err == nil {
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
