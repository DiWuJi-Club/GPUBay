package referral_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
)

func (e *env) kycVerified(userID int64, fp string) {
	e.t.Helper()
	if fp != "" {
		e.users.fps[userID] = append(e.users.fps[userID], fp)
	}
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
			return err
		}
		return e.svc.OnKYCVerified(e.ctx, tx, userID, fp)
	}); err != nil {
		e.t.Fatalf("OnKYCVerified: %v", err)
	}
}

func TestOnKYCVerifiedRevokesSameIdentity(t *testing.T) {
	e := newEnv(t)
	a := e.user("13800000001", true)
	e.users.fps[a] = []string{"fp-A"}
	b := e.user("13800000002", false)
	c := e.user("13800000003", false)
	d := e.user("13800000004", false)
	bb := e.bind(a, b)
	e.bind(a, c)
	e.bind(a, d)
	// B 有待发放返利
	e.pay(b, "ord-b1", 6900)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO referral_rewards (order_id, binding_id, inviter_id, invitee_id, rate_version_id, rate_bp, order_amount, reward_cap, reward, status, settle_after, paid_at, created_at, updated_at)
		SELECT 'ord-b1', $1, $2, $3, (SELECT min(id) FROM referral_rate_versions), 1000, 6900, 690, 690, 'pending', $4, $4, $4, $4`, bb, a, b, e.clk.Now()); err != nil {
		t.Fatal(err)
	}

	// B 的指纹 == A → B 撤销，待发放 void，事件 void，审计 auto_revoke
	e.kycVerified(b, "fp-A")
	st, hold, flags := e.bindingRow(b)
	if st != "revoked" || hold != "same_identity" || flags[len(flags)-1] != "same_identity" {
		t.Fatalf("B = %s %q %v", st, hold, flags)
	}
	if r := e.reward("ord-b1"); r["status"] != "void" {
		t.Fatalf("reward = %v", r)
	}
	if ev := e.events("ord-b1"); len(ev) != 1 || ev[0] != "void" {
		t.Fatalf("events = %v", ev)
	}
	var action string
	if err := e.pool.QueryRow(e.ctx, `SELECT action FROM audit_logs WHERE action LIKE 'referral.%' ORDER BY id DESC LIMIT 1`).Scan(&action); err != nil || action != "referral.auto_revoke" {
		t.Fatalf("audit = %s", action)
	}
	// C 的指纹是新的 → 不动；D 的指纹 == C → D 撤销（同邀请人的其它被邀请人）
	e.kycVerified(c, "fp-C")
	if st, _, _ := e.bindingRow(c); st != "active" {
		t.Fatalf("C = %s", st)
	}
	e.kycVerified(d, "fp-C")
	if st, _, _ := e.bindingRow(d); st != "revoked" {
		t.Fatalf("D = %s", st)
	}
	// 已撤销的 B 再实名不再处理（幂等）
	e.kycVerified(b, "fp-A")
	// 空指纹什么都不做
	e.kycVerified(c, "")
	if st, _, _ := e.bindingRow(c); st != "active" {
		t.Fatal("empty fingerprint must be a no-op")
	}
}

func TestOnKYCVerifiedInviterLater(t *testing.T) {
	e := newEnv(t)
	a := e.user("13800000001", false) // 邀请人未实名
	b := e.user("13800000002", false)
	c := e.user("13800000003", false)
	e.bind(a, b)
	e.bind(a, c)
	e.kycVerified(b, "fp-X") // 此时 A 无指纹，不撤销
	e.kycVerified(c, "fp-Y")
	if st, _, _ := e.bindingRow(b); st != "active" {
		t.Fatal("B must stay active before inviter verifies")
	}
	e.kycVerified(a, "fp-X") // A 后实名，与 B 同身份 → B 撤销，C 不动
	if st, _, _ := e.bindingRow(b); st != "revoked" {
		t.Fatal("B must be revoked when inviter turns out to be the same identity")
	}
	if st, _, _ := e.bindingRow(c); st != "active" {
		t.Fatal("C must stay active")
	}
	// 没有账户行的用户实名：跳过
	e.kycVerified(e.user("13800000009", false), "fp-Z")
}

// 同一用户既有个人又有企业实名：比较全部指纹，而不只最新一条。
func TestOnKYCVerifiedMatchesAnyFingerprint(t *testing.T) {
	// (a) A 个人 X → A 企业 L → B（A 邀请）个人 X：B 撤销
	e := newEnv(t)
	a := e.user("13800000001", false)
	b := e.user("13800000002", false)
	e.bind(a, b)
	e.kycVerified(a, "fp-X")
	e.kycVerified(a, "fp-L")
	e.kycVerified(b, "fp-X")
	if st, hold, _ := e.bindingRow(b); st != "revoked" || hold != "same_identity" {
		t.Fatalf("(a) B = %s %q", st, hold)
	}
	// (b) B（A 邀请）个人 X → B 企业 L → A 个人 X：B 撤销
	e = newEnv(t)
	a = e.user("13800000001", false)
	b = e.user("13800000002", false)
	e.bind(a, b)
	e.kycVerified(b, "fp-X")
	e.kycVerified(b, "fp-L")
	if st, _, _ := e.bindingRow(b); st != "active" {
		t.Fatalf("(b) B before inviter verifies = %s", st)
	}
	e.kycVerified(a, "fp-X")
	if st, hold, _ := e.bindingRow(b); st != "revoked" || hold != "same_identity" {
		t.Fatalf("(b) B = %s %q", st, hold)
	}
}
