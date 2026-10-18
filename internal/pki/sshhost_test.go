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
