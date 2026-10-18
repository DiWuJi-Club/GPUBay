package pki_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"net"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func newCA(t *testing.T) (*pki.CA, *clock.Fake) {
	t.Helper()
	pool := dbtest.New(t)
	box, _ := secretbox.New(dbtest.Key)
	clk := clock.NewFake(dbtest.T0)
	ca, err := pki.EnsureCA(context.Background(), pool, box, clk)
	if err != nil {
		t.Fatal(err)
	}
	return ca, clk
}

func csrFor(t *testing.T, key any, cn string) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn, Organization: []string{"evil"}}, DNSNames: []string{"evil.example"},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestEnsureCAIsSingletonAndPersistent(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	box, _ := secretbox.New(dbtest.Key)
	clk := clock.NewFake(dbtest.T0)
	var wg sync.WaitGroup
	fps := make([]string, 8)
	for i := range fps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ca, err := pki.EnsureCA(ctx, pool, box, clk)
			if err != nil {
				t.Error(err)
				return
			}
			fps[i] = ca.Fingerprint()
		}(i)
	}
	wg.Wait()
	for _, fp := range fps[1:] {
		if fp != fps[0] {
			t.Fatalf("fingerprints differ: %v", fps)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform_pki`).Scan(&n)
	if n != 1 {
		t.Fatalf("pki rows = %d", n)
	}
	ca, _ := pki.EnsureCA(ctx, pool, box, clk)
	cert, _ := x509.ParseCertificate(ca.CertDER())
	if !cert.IsCA || cert.PublicKeyAlgorithm != x509.ECDSA || cert.NotAfter.Sub(cert.NotBefore) < 19*365*24*time.Hour {
		t.Fatalf("ca cert = %+v", cert)
	}
	if len(ca.Fingerprint()) != 64 || ca.Fingerprint() != pki.Fingerprint(ca.CertDER()) {
		t.Fatalf("fingerprint = %s", ca.Fingerprint())
	}
	// 换一把 SPARK_SECRET_KEY 后 CA 私钥不可解密
	other, _ := secretbox.New([]byte("another-32-byte-key-for-testing!"))
	if _, err := pki.EnsureCA(ctx, pool, other, clk); err == nil {
		t.Fatal("wrong secret key must fail")
	}
}

func TestIssueMachineUsesSerialAndIgnoresCSR(t *testing.T) {
	ca, clk := newCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pub, err := pki.ParseCSR(csrFor(t, key, "evil"))
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.IssueMachine(pub, "SN-001", clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	if cert.Subject.CommonName != "SN-001" || len(cert.DNSNames) != 0 || len(cert.Subject.Organization) != 0 {
		t.Fatalf("subject not overridden: %+v", cert.Subject)
	}
	if cert.NotAfter.Sub(clk.Now()) < 5*365*24*time.Hour-time.Hour {
		t.Fatalf("validity = %v", cert.NotAfter)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: ca.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: clk.Now()}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := hex.DecodeString(pki.Fingerprint(der)); err != nil || len(pki.Fingerprint(der)) != 64 {
		t.Fatalf("fingerprint = %s", pki.Fingerprint(der))
	}
}

func TestParseCSRRejectsBadSignatureAndNonP256(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der := csrFor(t, key, "x")
	der[len(der)-1] ^= 0xff
	if _, err := pki.ParseCSR(der); err == nil {
		t.Fatal("tampered csr accepted")
	}
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := pki.ParseCSR(csrFor(t, rsaKey, "x")); err == nil {
		t.Fatal("rsa csr accepted")
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := pki.ParseCSR(csrFor(t, p384, "x")); err == nil {
		t.Fatal("p384 csr accepted")
	}
}

func TestServerCertSANsAndRotation(t *testing.T) {
	ca, clk := newCA(t)
	sc, err := pki.NewServerCerts(ca, []string{"agent.example", "192.168.1.5"}, clk)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := sc.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || cert.Leaf == nil || len(cert.Certificate) != 2 {
		t.Fatalf("cert = %+v, %v", cert, err)
	}
	if cert.Leaf.DNSNames[0] != "agent.example" || !cert.Leaf.IPAddresses[0].Equal(net.ParseIP("192.168.1.5")) {
		t.Fatalf("sans = %v %v", cert.Leaf.DNSNames, cert.Leaf.IPAddresses)
	}
	if err := cert.Leaf.VerifyHostname("192.168.1.5"); err != nil {
		t.Fatal(err)
	}
	if rotated, _ := sc.Rotate(); rotated {
		t.Fatal("fresh cert must not rotate")
	}
	clk.Advance(340 * 24 * time.Hour)
	rotated, err := sc.Rotate()
	if err != nil || !rotated {
		t.Fatalf("rotate = %v, %v", rotated, err)
	}
	fresh, _ := sc.GetCertificate(nil)
	if !fresh.Leaf.NotAfter.After(cert.Leaf.NotAfter) || fresh.Leaf.NotAfter.Sub(clk.Now()) < 364*24*time.Hour {
		t.Fatalf("not reissued: %v vs %v", fresh.Leaf.NotAfter, cert.Leaf.NotAfter)
	}
}
