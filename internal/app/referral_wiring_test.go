package app_test

import (
	"context"
	"testing"

	"spark-cloud/internal/app"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/referral"
)

// app.Build 装配邀请返利：identity 用 cfg.SecretKey（与 backfill 子命令 referralKeys 相同的解码后字节）派生的密钥写实名指纹，
// 并把 referral.Service 注入为实名钩子（同身份 → 自动撤销绑定）；App.Referral.Settle 推进扫描水位。
func TestBuildWiresReferral(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock"}
	a, err := app.Build(cfg, pool, discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Referral == nil {
		t.Fatal("App.Referral must be set")
	}
	const idNumber = "11010119900101123X"
	inviter := dbtest.CreateUser(t, pool, "13800000001", false)
	invitee := dbtest.CreateUser(t, pool, "13800000002", false)
	if _, err := a.Identity.SubmitPersonalKYC(ctx, inviter, "张三", idNumber); err != nil {
		t.Fatal(err)
	}
	key, err := referral.DeriveFingerprintKey(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	var fp string
	if err := pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1 AND result = 'verified'`, inviter).Scan(&fp); err != nil || fp != referral.Fingerprint(key, "kyc", idNumber) {
		t.Fatalf("fingerprint = %q %v, want HMAC under DeriveFingerprintKey(cfg.SecretKey)", fp, err)
	}

	acc, err := a.Referral.EnsureCode(ctx, inviter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO referral_bindings (inviter_id, invitee_id, code, status, rate_version_id, bound_at, rebate_until, created_at, updated_at)
		SELECT $1, $2, $3, 'active', id, $4::timestamptz, $4::timestamptz + interval '365 days', $4, $4 FROM referral_rate_versions ORDER BY id LIMIT 1`, inviter, invitee, acc.Code, dbtest.T0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Identity.SubmitPersonalKYC(ctx, invitee, "张三", idNumber); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM referral_bindings WHERE invitee_id = $1`, invitee).Scan(&status); err != nil || status != referral.BindingRevoked {
		t.Fatalf("binding status = %q %v, want revoked by the KYC hook (ids.SetReferral)", status, err)
	}

	if err := a.Referral.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	var moved bool
	if err := pool.QueryRow(ctx, `SELECT watermark > '2000-01-01' FROM referral_state WHERE id = 1`).Scan(&moved); err != nil || !moved {
		t.Fatalf("watermark moved = %v %v", moved, err)
	}
}
