package identity

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

const kycDailyLimit = 5

var (
	idNumberRe  = regexp.MustCompile(`^\d{17}[\dX]$`)
	licenseNoRe = regexp.MustCompile(`^[0-9A-HJ-NPQRTUWXY]{2}\d{6}[0-9A-HJ-NPQRTUWXY]{10}$`)
)

// SubmitPersonalKYC 调用三要素核验，结果立即生效。
func (s *Service) SubmitPersonalKYC(ctx context.Context, userID int64, name, idNumber string) (User, error) {
	name = strings.TrimSpace(name)
	idNumber = strings.ToUpper(strings.TrimSpace(idNumber))
	if name == "" || utf8.RuneCountInString(name) > 50 {
		return User{}, apperr.BadRequest("请填写真实姓名")
	}
	if !idNumberRe.MatchString(idNumber) {
		return User{}, apperr.BadRequest("身份证号格式不正确")
	}
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if u.KYCStatus == "verified" || u.KYCStatus == "pending" {
		return User{}, apperr.ErrInvalidState
	}
	now := s.d.Clock.Now()
	var attempts int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM kyc_records
		WHERE user_id = $1 AND kind = 'personal' AND created_at > $2`, userID, now.Add(-24*time.Hour)).Scan(&attempts); err != nil {
		return User{}, err
	}
	if attempts >= kycDailyLimit {
		return User{}, apperr.ErrRateLimited
	}

	res, err := s.d.KYC.VerifyPerson(ctx, name, idNumber, u.Phone)
	if err != nil {
		return User{}, fmt.Errorf("kyc verify: %w", err)
	}
	result := "rejected"
	if res.Passed {
		result = "verified"
	}
	err = db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, id_number_enc, result, reason, created_at, updated_at)
			VALUES ($1, 'personal', $2, $3, $4, $5, $6, $6)`,
			userID, name, s.d.Box.Seal([]byte(idNumber)), result, res.Reason, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE users SET kyc_status = $2, kind = 'personal', updated_at = $3
			WHERE id = $1 AND kyc_status <> 'verified'`, userID, result, now)
		return err
	})
	if err != nil {
		return User{}, err
	}
	if !res.Passed {
		return User{}, apperr.New("kyc_rejected", "实名核验未通过："+res.Reason, 400)
	}
	return s.GetUser(ctx, userID)
}

// SubmitEnterpriseKYC 提交企业认证，等待管理员审核。
func (s *Service) SubmitEnterpriseKYC(ctx context.Context, userID int64, companyName, licenseNo, licenseFile string) error {
	companyName = strings.TrimSpace(companyName)
	licenseNo = strings.ToUpper(strings.TrimSpace(licenseNo))
	if companyName == "" || utf8.RuneCountInString(companyName) > 100 {
		return apperr.BadRequest("请填写企业名称")
	}
	if !licenseNoRe.MatchString(licenseNo) {
		return apperr.BadRequest("统一社会信用代码格式不正确")
	}
	if strings.TrimSpace(licenseFile) == "" {
		return apperr.BadRequest("请上传营业执照")
	}
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.KYCStatus == "verified" || u.KYCStatus == "pending" {
		return apperr.ErrInvalidState
	}
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, license_no, license_file, result, created_at, updated_at)
			VALUES ($1, 'enterprise', $2, $3, $4, 'pending', $5, $5)`,
			userID, companyName, licenseNo, licenseFile, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE users SET kyc_status = 'pending', updated_at = $2 WHERE id = $1`, userID, now)
		return err
	})
}

// ReviewEnterpriseKYC 管理员审核企业认证；驳回必须填写原因。
func (s *Service) ReviewEnterpriseKYC(ctx context.Context, recordID, reviewerID int64, approve bool, reason string) error {
	reason = strings.TrimSpace(reason)
	if !approve && reason == "" {
		return apperr.BadRequest("请填写驳回原因")
	}
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var userID int64
		var kind, result string
		err := tx.QueryRow(ctx, `SELECT user_id, kind, result FROM kyc_records WHERE id = $1 FOR UPDATE`, recordID).
			Scan(&userID, &kind, &result)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if kind != "enterprise" || result != "pending" {
			return apperr.ErrInvalidState
		}
		newResult := "rejected"
		if approve {
			newResult = "verified"
		}
		if _, err := tx.Exec(ctx, `UPDATE kyc_records SET result = $2, reason = $3, reviewer_id = $4, updated_at = $5 WHERE id = $1`,
			recordID, newResult, reason, reviewerID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET kyc_status = $2,
				kind = CASE WHEN $2 = 'verified' THEN 'enterprise' ELSE kind END, updated_at = $3
			WHERE id = $1`, userID, newResult, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Entry{
			ActorType: "admin", ActorID: reviewerID, Action: "kyc.review",
			TargetType: "kyc_record", TargetID: strconv.FormatInt(recordID, 10),
			Diff: map[string]any{"approve": approve, "reason": reason, "user_id": userID},
		})
	})
}
