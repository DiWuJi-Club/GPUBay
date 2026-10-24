package identity_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/plugins/kyc"
)

var opsActor = audit.Actor{AdminID: 4, IP: "10.0.0.4"}

func TestAdminListUsers(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	a := dbtest.CreateUser(t, pool, "13900000001", true)
	dbtest.CreateUser(t, pool, "13900000002", false)
	dbtest.CreateUser(t, pool, "13800000003", false)
	all, total, err := svc.AdminListUsers(ctx, identity.UserFilter{}, paging.Page{})
	// 迁移 00022 种子：系统账号 system:llm（已实名）。后台列表默认隐藏系统账号在计划 9 Task 13（include_system）实现
	if err != nil || total != 4 || len(all) != 4 {
		t.Fatalf("all = %d, %v", total, err)
	}
	if list, total, _ := svc.AdminListUsers(ctx, identity.UserFilter{Q: "13900000002"}, paging.Page{}); total != 1 || list[0].Phone != "13900000002" {
		t.Fatalf("q=full phone = %+v", list)
	}
	if list, total, _ := svc.AdminListUsers(ctx, identity.UserFilter{Q: "0003"}, paging.Page{}); total != 1 || list[0].Phone != "13800000003" {
		t.Fatalf("q=phone tail = %+v", list)
	}
	if _, _, err := svc.AdminListUsers(ctx, identity.UserFilter{Q: "张三"}, paging.Page{}); apperr.As(err) == nil {
		t.Fatal("non-numeric q must be rejected")
	}
	// 数字 q 同时匹配 ID 与手机号尾号：系统账号占了 ID 1 后 a = 2，「2」也命中 13900000002
	if list, _, _ := svc.AdminListUsers(ctx, identity.UserFilter{Q: itoa(a)}, paging.Page{}); !slices.ContainsFunc(list, func(u identity.User) bool { return u.ID == a }) {
		t.Fatalf("q=id = %+v", list)
	}
	if _, total, _ := svc.AdminListUsers(ctx, identity.UserFilter{KYCStatus: "verified"}, paging.Page{}); total != 2 {
		t.Fatalf("verified = %d", total)
	}
	if _, _, err := svc.AdminListUsers(ctx, identity.UserFilter{Status: "zombie"}, paging.Page{}); apperr.As(err) == nil {
		t.Fatalf("bad status = %v", err)
	}
}

func TestKYCRecordsMaskIDNumber(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000010", false)
	if _, err := svc.SubmitPersonalKYC(ctx, uid, "张三", validID); err != nil {
		t.Fatal(err)
	}
	recs, err := svc.UserKYCRecords(ctx, uid)
	if err != nil || len(recs) != 1 || recs[0].IDNumberMasked != "110***********123X" || recs[0].RealName != "张三" {
		t.Fatalf("records = %+v, %v", recs, err)
	}
}

func TestPendingEnterpriseKYC(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000011", false)
	if err := svc.SubmitEnterpriseKYC(ctx, uid, "某某科技有限公司", "91420100MA4KXXXX1X", licenseKeyFor(uid)); err != nil {
		t.Fatal(err)
	}
	// 个人核验进行中的 pending 记录不能出现在企业审核列表里
	if _, err := pool.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, result, created_at, updated_at)
		VALUES ($1, 'personal', '李四', 'pending', $2, $2)`, dbtest.CreateUser(t, pool, "13900000012", false), dbtest.T0); err != nil {
		t.Fatal(err)
	}
	list, total, err := svc.PendingEnterpriseKYC(ctx, paging.Page{})
	if err != nil || total != 1 || list[0].Kind != "enterprise" || list[0].LicenseFile != licenseKeyFor(uid) {
		t.Fatalf("pending = %+v, %v", list, err)
	}
	rec, err := svc.GetKYCRecord(ctx, list[0].ID)
	if err != nil || rec.UserID != uid {
		t.Fatalf("get = %+v, %v", rec, err)
	}
	if _, err := svc.GetKYCRecord(ctx, 9999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestBanAndUnban(t *testing.T) {
	svc, pool, _ := newKYCEnv(t, kyc.Mock{})
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000013", true)
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ('h', $1, $2, $3)`,
		uid, dbtest.T0, dbtest.T0.AddDate(0, 1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Ban(ctx, opsActor, uid, " "); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("ban without reason = %v", err)
	}
	if err := svc.Ban(ctx, opsActor, uid, "恶意挖矿"); err != nil {
		t.Fatal(err)
	}
	var sessions int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, uid).Scan(&sessions)
	if u, _ := svc.GetUser(ctx, uid); u.Status != "banned" || sessions != 0 {
		t.Fatalf("status = %s sessions = %d", u.Status, sessions)
	}
	if err := svc.Ban(ctx, opsActor, uid, "again"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("ban twice = %v", err)
	}
	if err := svc.Unban(ctx, opsActor, uid); err != nil {
		t.Fatal(err)
	}
	if err := svc.Unban(ctx, opsActor, uid); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("unban twice = %v", err)
	}
	if err := svc.Ban(ctx, opsActor, 9999, "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("ban missing = %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action IN ('user.ban', 'user.unban') AND actor_id = 4`).Scan(&n)
	if n != 2 {
		t.Fatalf("audit rows = %d", n)
	}
}

func itoa(n int64) string { return fmt.Sprint(n) }
