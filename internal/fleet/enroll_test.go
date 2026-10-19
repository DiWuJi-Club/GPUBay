package fleet_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
)

func consume(t *testing.T, svc *fleet.Service, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, plain string) (fleet.EnrollTokenRow, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	row, err := svc.ConsumeEnrollToken(ctx, tx, plain)
	if err != nil {
		return row, err
	}
	return row, tx.Commit(ctx)
}

func TestEnrollTokenLifecycle(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN-A")
	actor := audit.Actor{AdminID: 7, IP: "10.0.0.1"}
	tok, err := svc.IssueEnrollToken(ctx, actor, id)
	if err != nil || !strings.HasPrefix(tok.Plain, "spk1_") || len(tok.Plain) != 5+43 || !tok.ExpiresAt.Equal(clk.Now().Add(24*time.Hour)) {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	if _, err := svc.IssueEnrollToken(ctx, actor, 9999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown machine = %v", err)
	}
	// 审计只记 token_id，不含明文
	var diff string
	_ = pool.QueryRow(ctx, `SELECT diff::text FROM audit_logs WHERE action = 'machine.enroll_token'`).Scan(&diff)
	if !strings.Contains(diff, `"token_id"`) || strings.Contains(diff, tok.Plain[5:]) {
		t.Fatalf("audit diff = %s", diff)
	}
	// 新令牌使旧令牌失效
	tok2, _ := svc.IssueEnrollToken(ctx, actor, id)
	if _, err := consume(t, svc, pool, tok.Plain); !errors.Is(err, fleet.ErrEnrollToken) {
		t.Fatalf("replaced token = %v", err)
	}
	row, err := consume(t, svc, pool, tok2.Plain)
	if err != nil || row.MachineID != id || row.IssuedBy != 7 || row.Serial != "SN-A" || row.ID != tok2.ID {
		t.Fatalf("consume = %+v, %v", row, err)
	}
	if _, err := consume(t, svc, pool, tok2.Plain); !errors.Is(err, fleet.ErrEnrollToken) {
		t.Fatalf("second use = %v", err)
	}
	// 过期
	tok3, _ := svc.IssueEnrollToken(ctx, actor, id)
	clk.Advance(24*time.Hour + time.Second)
	if _, err := consume(t, svc, pool, tok3.Plain); !errors.Is(err, fleet.ErrEnrollToken) {
		t.Fatalf("expired = %v", err)
	}
}

func TestConsumeEnrollTokenTrimsAndRequiresPrefix(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN-A")
	tok, _ := svc.IssueEnrollToken(ctx, audit.Actor{}, id)
	if _, err := consume(t, svc, pool, strings.TrimPrefix(tok.Plain, "spk1_")); !errors.Is(err, fleet.ErrEnrollToken) {
		t.Fatalf("no prefix = %v", err)
	}
	if _, err := consume(t, svc, pool, "spk1_"+strings.Repeat("x", 43)); !errors.Is(err, fleet.ErrEnrollToken) {
		t.Fatalf("garbage = %v", err)
	}
	if row, err := consume(t, svc, pool, "  "+tok.Plain+"\n"); err != nil || row.MachineID != id {
		t.Fatalf("trimmed = %+v, %v", row, err)
	}
}

func TestSetFingerprintAndRevoke(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN-A")
	if err := svc.RevokeEnrollment(ctx, audit.Actor{AdminID: 1}, id); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("revoke unenrolled = %v", err)
	}
	fp := strings.Repeat("ab", 32)
	if err := svc.AttachAgentConn(ctx, id, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := db.WithTx(ctx, pool, func(tx pgx.Tx) error { return svc.SetCertFingerprint(ctx, tx, id, fp) }); err != nil {
		t.Fatal(err)
	}
	m, _ := svc.Get(ctx, pool, id)
	if !m.Enrolled || m.FingerprintPrefix != fp[:16] || m.CertFingerprint != fp || m.Connected {
		t.Fatalf("after set = %+v", m)
	}
	if err := svc.AttachAgentConn(ctx, id, "c2"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeEnrollment(ctx, audit.Actor{AdminID: 1}, id); err != nil {
		t.Fatal(err)
	}
	m, _ = svc.Get(ctx, pool, id)
	if m.Enrolled || m.Connected || m.FingerprintPrefix != "" {
		t.Fatalf("after revoke = %+v", m)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'machine.revoke_enrollment' AND diff->>'fingerprint' = $1`, fp).Scan(&n)
	if n != 1 {
		t.Fatalf("revoke audit = %d", n)
	}
}

func TestRevokeEnrollmentInvalidatesUnusedTokens(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN-R")
	tok, err := svc.IssueEnrollToken(ctx, audit.Actor{AdminID: 1}, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WithTx(ctx, pool, func(tx pgx.Tx) error { return svc.SetCertFingerprint(ctx, tx, id, strings.Repeat("ab", 32)) }); err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeEnrollment(ctx, audit.Actor{AdminID: 1}, id); err != nil {
		t.Fatal(err)
	}
	if _, err := consume(t, svc, pool, tok.Plain); !errors.Is(err, fleet.ErrEnrollToken) {
		t.Fatalf("token issued before revoke still works: %v", err)
	}
	// 撤销后新签发的令牌可用
	tok2, _ := svc.IssueEnrollToken(ctx, audit.Actor{AdminID: 1}, id)
	if _, err := consume(t, svc, pool, tok2.Plain); err != nil {
		t.Fatalf("new token = %v", err)
	}
}

func TestConcurrentIssueLeavesOneValidToken(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN-C")
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.IssueEnrollToken(ctx, audit.Actor{AdminID: 1}, id); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var valid int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM machine_enroll_tokens WHERE machine_id = $1 AND used_at IS NULL AND expires_at > $2`,
		id, clk.Now()).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if valid != 1 {
		t.Fatalf("valid tokens = %d, want 1", valid)
	}
}

// 接入事务持有令牌与机器锁时，并发的撤销/签发必须等待而不是死锁，并看到接入后的状态。
func TestEnrollRacesWithRevokeAndIssueWithoutDeadlock(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN-D")
	tok, _ := svc.IssueEnrollToken(ctx, audit.Actor{AdminID: 1}, id)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := svc.ConsumeEnrollToken(ctx, tx, tok.Plain); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	go func() { errs <- svc.RevokeEnrollment(ctx, audit.Actor{AdminID: 1}, id) }()
	go func() { _, err := svc.IssueEnrollToken(ctx, audit.Actor{AdminID: 1}, id); errs <- err }()
	time.Sleep(300 * time.Millisecond) // 让两者阻塞在机器行锁上
	if err := svc.SetCertFingerprint(ctx, tx, id, strings.Repeat("cd", 32)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent op failed (deadlock?): %v", err)
		}
	}
	m, _ := svc.Get(ctx, pool, id)
	if m.Enrolled {
		t.Fatalf("revoke should have seen the enrolled state and cleared it: %+v", m)
	}
	var valid int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM machine_enroll_tokens WHERE machine_id = $1 AND used_at IS NULL AND expires_at > $2`, id, clk.Now()).Scan(&valid)
	if valid > 1 {
		t.Fatalf("valid tokens = %d", valid)
	}
}

func TestRevokeWithoutFingerprintExpiresUnusedTokens(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN-T")
	if err := svc.RevokeEnrollment(ctx, audit.Actor{AdminID: 1}, id); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("nothing to revoke = %v", err)
	}
	tok, _ := svc.IssueEnrollToken(ctx, audit.Actor{AdminID: 1}, id)
	if err := svc.RevokeEnrollment(ctx, audit.Actor{AdminID: 1}, id); err != nil {
		t.Fatalf("revoke with only token = %v", err)
	}
	if _, err := consume(t, svc, pool, tok.Plain); !errors.Is(err, fleet.ErrEnrollToken) {
		t.Fatalf("token still valid: %v", err)
	}
	if err := svc.RevokeEnrollment(ctx, audit.Actor{AdminID: 1}, id); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("second revoke = %v", err)
	}
}
