package agentgw_test

import (
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"spark-cloud/internal/agentgw"
	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/pki"
)

func TestEnrollIssuesCertBoundToSerial(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	if err := e.fleet.AttachAgentConn(e.ctx, mid, "stale"); err != nil {
		t.Fatal(err)
	}
	creds := e.enroll(mid)
	cert, err := x509.ParseCertificate(creds.certDER)
	if err != nil || cert.Subject.CommonName != "SN-001" || len(cert.DNSNames) != 0 {
		t.Fatalf("cert = %+v, %v", cert.Subject, err)
	}
	if pki.Fingerprint(creds.caDER) != e.ca.Fingerprint() {
		t.Fatal("ca der mismatch")
	}
	m, _ := e.fleet.Get(e.ctx, e.pool, mid)
	if !m.Enrolled || m.CertFingerprint != pki.Fingerprint(creds.certDER) || m.Connected {
		t.Fatalf("machine = %+v", m)
	}
	var actorType, diff string
	_ = e.pool.QueryRow(e.ctx, `SELECT actor_type, diff::text FROM audit_logs WHERE action = 'machine.enrolled'`).Scan(&actorType, &diff)
	if actorType != "system" || !strings.Contains(diff, `"issued_by": 1`) && !strings.Contains(diff, `"issued_by":1`) || !strings.Contains(diff, m.CertFingerprint) {
		t.Fatalf("audit = %s %s", actorType, diff)
	}
	// 重新接入：指纹被覆盖
	creds2 := e.enroll(mid)
	m, _ = e.fleet.Get(e.ctx, e.pool, mid)
	if m.CertFingerprint != pki.Fingerprint(creds2.certDER) {
		t.Fatal("re-enroll must replace fingerprint")
	}
}

func TestEnrollRejectsBadTokensAndCSR(t *testing.T) {
	e := newEnv(t)
	mid := e.machine("SN-001")
	tok, _ := e.fleet.IssueEnrollToken(e.ctx, audit.Actor{}, mid)
	c := e.dial(nil)
	key := newKey(t)
	for name, token := range map[string]string{"garbage": "spk1_nope", "no prefix": strings.TrimPrefix(tok.Plain, "spk1_")} {
		_, err := c.Enroll(e.ctx, &agentpb.EnrollRequest{Token: token, CsrDer: csrFor(t, key)})
		if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || st.Message() != "令牌无效" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := c.Enroll(e.ctx, &agentpb.EnrollRequest{Token: tok.Plain, CsrDer: []byte("not a csr")}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad csr = %v", err)
	}
	// 坏 CSR 没有消耗令牌；带空白的令牌可用；用过的不能再用
	if _, err := c.Enroll(e.ctx, &agentpb.EnrollRequest{Token: " " + tok.Plain + "\n", CsrDer: csrFor(t, key)}); err != nil {
		t.Fatalf("trimmed token: %v", err)
	}
	if _, err := c.Enroll(e.ctx, &agentpb.EnrollRequest{Token: tok.Plain, CsrDer: csrFor(t, key)}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("reuse = %v", err)
	}
}

func TestEnrollRateLimited(t *testing.T) {
	e := newEnvWith(t, func(c *agentgw.Config) { c.EnrollLimit = 2 })
	c := e.dial(nil)
	for i := 0; i < 2; i++ {
		if _, err := c.Enroll(e.ctx, &agentpb.EnrollRequest{Token: "spk1_x", CsrDer: nil}); status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.InvalidArgument {
			t.Fatalf("attempt %d = %v", i, err)
		}
	}
	if _, err := c.Enroll(e.ctx, &agentpb.EnrollRequest{Token: "spk1_x"}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("third = %v", err)
	}
	e.clk.Advance(time.Minute)
	if _, err := c.Enroll(e.ctx, &agentpb.EnrollRequest{Token: "spk1_x"}); status.Code(err) == codes.ResourceExhausted {
		t.Fatalf("after window = %v", err)
	}
}

func TestInstallCommand(t *testing.T) {
	got := agentgw.InstallCommand("agent.example:9443", "spk1_abc", "ff00", "http://files.local/agent", "beef")
	want := "curl -fsSL http://files.local/agent/install.sh | sudo sh -s -- --platform agent.example:9443 --token spk1_abc --ca-sha256 ff00 --binary-sha256 beef --download-url http://files.local/agent"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if got := agentgw.InstallCommand("agent.example:9443", "spk1_abc", "ff00", "", ""); got != "sudo spark-agent enroll --platform agent.example:9443 --token spk1_abc --ca-sha256 ff00" {
		t.Fatalf("bare = %q", got)
	}
	e := newEnv(t)
	if !strings.Contains(e.srv.InstallCommand("spk1_t"), "--platform agent.test:9443 --token spk1_t --ca-sha256 "+e.ca.Fingerprint()) {
		t.Fatalf("server command = %s", e.srv.InstallCommand("spk1_t"))
	}
}
