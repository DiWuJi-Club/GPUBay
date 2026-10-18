package pki_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/pki"
)

func TestUpstreamKeyGenerateAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream.key")
	if err := pki.GenerateUpstreamKey(path); err != nil {
		t.Fatal(err)
	}
	if err := pki.GenerateUpstreamKey(path); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("overwrite = %v", err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o", st.Mode().Perm())
	}
	pub, err := pki.LoadUpstreamPublicKey(path)
	if err != nil || !strings.HasPrefix(pub, "ssh-ed25519 AAAA") || strings.ContainsAny(pub, "\n ") && strings.Count(pub, " ") != 1 {
		t.Fatalf("pub = %q, %v", pub, err)
	}
	pubFile, _ := os.ReadFile(path + ".pub")
	if strings.TrimSpace(string(pubFile)) != pub {
		t.Fatalf(".pub = %q", pubFile)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := pki.LoadUpstreamPublicKey(path); err == nil || !strings.Contains(err.Error(), "权限") {
		t.Fatalf("loose perm = %v", err)
	}
	if _, err := pki.LoadUpstreamPublicKey(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestLoadUpstreamSigner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "up.key")
	if err := pki.GenerateUpstreamKey(path); err != nil {
		t.Fatal(err)
	}
	s, err := pki.LoadUpstreamSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := pki.LoadUpstreamPublicKey(path)
	if strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) != pub {
		t.Fatal("signer public key differs from LoadUpstreamPublicKey")
	}
	os.Chmod(path, 0o644)
	if _, err := pki.LoadUpstreamSigner(path); err == nil || !strings.Contains(err.Error(), "权限") {
		t.Fatalf("loose perm = %v", err)
	}
}
