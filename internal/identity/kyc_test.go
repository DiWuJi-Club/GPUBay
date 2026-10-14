package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/kyc"
	"spark-cloud/internal/plugins/sms"
)

func newKYCEnv(t *testing.T, verifier kyc.Verifier) (*identity.Service, *pgxpool.Pool, *secretbox.Box) {
	t.Helper()
	pool := dbtest.New(t)
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	svc := identity.NewService(identity.Deps{
		Pool: pool, Clock: clock.NewFake(dbtest.T0), SMS: &sms.Mock{}, KYC: verifier, Box: box,
	})
	return svc, pool, box
}

const validID = "11010119900101123X"

func TestPersonalKYCPassesAndEncryptsIDNumber(t *testing.T) {
	svc, pool, box := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000001", false)

	u, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID)
	if err != nil {
		t.Fatal(err)
	}
	if u.KYCStatus != "verified" || u.Kind != "personal" {
		t.Fatalf("user = %+v", u)
	}
	if err := svc.CanRent(ctx, uid); err != nil {
		t.Fatalf("CanRent after verify: %v", err)
	}

	var enc []byte
	if err := pool.QueryRow(ctx, `SELECT id_number_enc FROM kyc_records WHERE user_id = $1`, uid).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	plain, err := box.Open(enc)
	if err != nil || string(plain) != validID {
		t.Fatalf("decrypted id = %q, %v", plain, err)
	}
}

func TestPersonalKYCRejected(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{Reject: true})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000002", false)

	_, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID)
	if ae := apperr.As(err); ae == nil || ae.Code != "kyc_rejected" {
		t.Fatalf("want kyc_rejected, got %v", err)
	}
	u, _ := svc.GetUser(ctx, uid)
	if u.KYCStatus != "rejected" {
		t.Fatalf("status = %s, want rejected", u.KYCStatus)
	}
}

func TestPersonalKYCValidation(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000003", false)
	cases := []struct{ name, id string }{
		{"", validID},
		{"张三", "123"},
		{"张三", "1101011990010112345"},
	}
	for _, c := range cases {
		_, err := svc.SubmitPersonalKYC(ctx, uid, c.name, c.id)
		if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
			t.Fatalf("(%q,%q) want bad_request, got %v", c.name, c.id, err)
		}
	}
}

func TestPersonalKYCAlreadyVerified(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	uid := dbtest.CreateUser(t, pool, "13900000004", true)
	if _, err := svc.SubmitPersonalKYC(context.Background(), uid, "张三", validID); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("want invalid state, got %v", err)
	}
}

func TestPersonalKYCDailyLimit(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{Reject: true})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000005", false)
	for i := 0; i < 5; i++ {
		if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); apperr.As(err) == nil || apperr.As(err).Code != "kyc_rejected" {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("6th attempt want rate limited, got %v", err)
	}
}

func TestEnterpriseKYCReview(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000006", false)

	if err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", "https://files/license.png"); err != nil {
		t.Fatal(err)
	}
	u, _ := svc.GetUser(ctx, uid)
	if u.KYCStatus != "pending" {
		t.Fatalf("status = %s, want pending", u.KYCStatus)
	}
	var recordID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM kyc_records WHERE user_id = $1`, uid).Scan(&recordID); err != nil {
		t.Fatal(err)
	}

	if err := svc.ReviewEnterpriseKYC(ctx, recordID, 99, false, ""); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("reject without reason want bad_request, got %v", err)
	}
	if err := svc.ReviewEnterpriseKYC(ctx, recordID, 99, true, ""); err != nil {
		t.Fatal(err)
	}
	u, _ = svc.GetUser(ctx, uid)
	if u.KYCStatus != "verified" || u.Kind != "enterprise" {
		t.Fatalf("user = %+v", u)
	}
	if err := svc.ReviewEnterpriseKYC(ctx, recordID, 99, true, ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("second review want invalid state, got %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'kyc.review'`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
}

func TestEnterpriseKYCValidation(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	uid := dbtest.CreateUser(t, pool, "13900000007", false)
	err := svc.SubmitEnterpriseKYC(context.Background(), uid, "某某科技有限公司", "bad", "https://files/license.png")
	if ae := apperr.As(err); ae == nil || ae.Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
}
