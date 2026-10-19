package pki_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
)

func TestGenerateInstanceHostKey(t *testing.T) {
	pub, pem, err := pki.GenerateInstanceHostKey()
	if err != nil || !strings.HasPrefix(pub, "ssh-ed25519 ") || strings.Contains(pub, "\n") || !strings.Contains(string(pem), "OPENSSH PRIVATE KEY") {
		t.Fatalf("pub=%q pem=%q err=%v", pub, pem, err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil || strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) != pub {
		t.Fatalf("pem/pub mismatch: %v", err)
	}
}

func TestEnsureSSHHostKeyPersistsAndIsUnique(t *testing.T) {
	pool := dbtest.New(t)
	box, _ := secretbox.New(dbtest.Key)
	ctx := context.Background()
	if _, err := pki.EnsureSSHHostKey(ctx, pool, box); err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("before CA: %v", err)
	}
	if _, err := pki.EnsureCA(ctx, pool, box, clock.NewFake(dbtest.T0)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	fps := make([]string, 8)
	for i := range fps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := pki.EnsureSSHHostKey(ctx, pool, box)
			if err != nil {
				t.Error(err)
				return
			}
			fps[i] = pki.SSHFingerprint(s.PublicKey())
		}(i)
	}
	wg.Wait()
	for _, fp := range fps[1:] {
		if fp != fps[0] || !strings.HasPrefix(fp, "SHA256:") {
			t.Fatalf("fingerprints differ: %v", fps)
		}
	}
	again, _ := pki.EnsureSSHHostKey(ctx, pool, box)
	if pki.SSHFingerprint(again.PublicKey()) != fps[0] || again.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Fatal("reload returned a different key")
	}
}

// SPARK_SECRET_KEY 轮换后不静默重新生成（会触发所有客户端的 known_hosts 警告），失败并给出可操作的恢复步骤。
func TestEnsureSSHHostKeyWrongSecretFailsClosed(t *testing.T) {
	pool := dbtest.New(t)
	box, _ := secretbox.New(dbtest.Key)
	ctx := context.Background()
	if _, err := pki.EnsureCA(ctx, pool, box, clock.NewFake(dbtest.T0)); err != nil {
		t.Fatal(err)
	}
	orig, err := pki.EnsureSSHHostKey(ctx, pool, box)
	if err != nil {
		t.Fatal(err)
	}
	other := make([]byte, len(dbtest.Key))
	copy(other, dbtest.Key)
	other[0] ^= 0xff
	box2, _ := secretbox.New(other)
	_, err = pki.EnsureSSHHostKey(ctx, pool, box2)
	if err == nil {
		t.Fatal("expected decrypt failure with a different SPARK_SECRET_KEY")
	}
	for _, want := range []string{"SPARK_SECRET_KEY", "platform_pki.ssh_host_key_enc", "UPDATE platform_pki SET ssh_host_key_enc = NULL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	again, err := pki.EnsureSSHHostKey(ctx, pool, box)
	if err != nil || pki.SSHFingerprint(again.PublicKey()) != pki.SSHFingerprint(orig.PublicKey()) {
		t.Fatalf("stored key changed after failed load: %v", err)
	}
}
