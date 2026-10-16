package identity

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/storage"
)

const (
	kycDailyLimit     = 5
	kycPendingTimeout = 2 * time.Minute // 个人核验「进行中」的最长时间，超过视为中断
)

var (
	idNumberRe  = regexp.MustCompile(`^\d{17}[\dX]$`)
	licenseNoRe = regexp.MustCompile(`^[0-9A-HJ-NPQRTUWXY]{2}\d{6}[0-9A-HJ-NPQRTUWXY]{10}$`)

	errKYCInProgress  = apperr.New("kyc_in_progress", "实名核验进行中，请稍候", 409)
	errKYCUnavailable = apperr.New("kyc_unavailable", "实名核验服务暂时不可用，请稍后再试", 503)
)

// SubmitPersonalKYC 调用三要素核验，结果立即生效。
// 先在事务内锁用户行、检查次数并登记一条 pending 记录，提交后再调用付费核验接口，
// 因此同一用户的并发提交只会调用一次接口；接口报错记为 error 并计入当日次数。
func (s *Service) SubmitPersonalKYC(ctx context.Context, userID int64, name, idNumber string) (User, error) {
	name = strings.TrimSpace(name)
	idNumber = strings.ToUpper(strings.TrimSpace(idNumber))
	if name == "" || utf8.RuneCountInString(name) > 50 {
		return User{}, apperr.BadRequest("请填写真实姓名")
	}
	if !idNumberRe.MatchString(idNumber) {
		return User{}, apperr.BadRequest("身份证号格式不正确")
	}
	now := s.d.Clock.Now()
	var phone string
	var recordID int64
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1 FOR UPDATE`, userID))
		if err != nil {
			return err
		}
		if u.KYCStatus == "verified" || u.KYCStatus == "pending" {
			return apperr.ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `UPDATE kyc_records SET result = 'error', reason = '核验超时未完成', updated_at = $3
			WHERE user_id = $1 AND kind = 'personal' AND result = 'pending' AND created_at <= $2`,
			userID, now.Add(-kycPendingTimeout), now); err != nil {
			return err
		}
		var inProgress bool
		var attempts int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE result = 'pending') > 0, count(*)
			FROM kyc_records WHERE user_id = $1 AND kind = 'personal' AND created_at > $2`,
			userID, now.Add(-24*time.Hour)).Scan(&inProgress, &attempts); err != nil {
			return err
		}
		if inProgress {
			return errKYCInProgress
		}
		if attempts >= kycDailyLimit {
			return apperr.ErrRateLimited
		}
		phone = u.Phone
		return tx.QueryRow(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, id_number_enc, result, created_at, updated_at)
			VALUES ($1, 'personal', $2, $3, 'pending', $4, $4) RETURNING id`,
			userID, name, s.d.Box.Seal([]byte(idNumber)), now).Scan(&recordID)
	})
	if err != nil {
		return User{}, err
	}

	res, verr := s.d.KYC.VerifyPerson(ctx, name, idNumber, phone)
	result, reason := "rejected", res.Reason
	switch {
	case verr != nil:
		result, reason = "error", truncateRunes(verr.Error(), 200)
	case res.Passed:
		result, reason = "verified", ""
	}
	done := s.d.Clock.Now()
	wctx := context.WithoutCancel(ctx) // 已调用付费接口，结果必须落库，即使客户端已断开
	err = db.WithTx(wctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(wctx, `UPDATE kyc_records SET result = $2, reason = $3, updated_at = $4 WHERE id = $1`,
			recordID, result, reason, done); err != nil {
			return err
		}
		if result == "error" {
			return nil
		}
		_, err := tx.Exec(wctx, `UPDATE users SET kyc_status = $2, kind = 'personal', updated_at = $3
			WHERE id = $1 AND kyc_status <> 'verified'`, userID, result, done)
		return err
	})
	if err != nil {
		return User{}, err
	}
	switch result {
	case "error":
		return User{}, errKYCUnavailable
	case "rejected":
		return User{}, apperr.New("kyc_rejected", "实名核验未通过："+reason, 400)
	}
	return s.GetUser(ctx, userID)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// SubmitEnterpriseKYC 提交企业认证，等待管理员审核。licenseFile 必须是本人通过上传接口得到的营业执照文件 key。
func (s *Service) SubmitEnterpriseKYC(ctx context.Context, userID int64, companyName, licenseNo, licenseFile string) error {
	companyName = strings.TrimSpace(companyName)
	licenseNo = strings.ToUpper(strings.TrimSpace(licenseNo))
	licenseFile = strings.TrimSpace(licenseFile)
	if companyName == "" || utf8.RuneCountInString(companyName) > 100 {
		return apperr.BadRequest("请填写企业名称")
	}
	if !licenseNoRe.MatchString(licenseNo) {
		return apperr.BadRequest("统一社会信用代码格式不正确")
	}
	if licenseFile == "" {
		return apperr.BadRequest("请上传营业执照")
	}
	if s.d.Uploads == nil {
		return errors.New("identity: 未配置上传文件校验")
	}
	if err := s.d.Uploads.CheckOwned(ctx, licenseFile, userID, storage.PurposeLicense); err != nil {
		return err
	}
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1 FOR UPDATE`, userID))
		if err != nil {
			return err
		}
		if u.KYCStatus == "verified" || u.KYCStatus == "pending" {
			return apperr.ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, license_no, license_file, result, created_at, updated_at)
			VALUES ($1, 'enterprise', $2, $3, $4, 'pending', $5, $5)`,
			userID, companyName, licenseNo, licenseFile, now); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE users SET kyc_status = 'pending', updated_at = $2 WHERE id = $1`, userID, now)
		return err
	})
}

// ReviewEnterpriseKYC 管理员审核企业认证；驳回必须填写原因。
func (s *Service) ReviewEnterpriseKYC(ctx context.Context, actor audit.Actor, recordID int64, approve bool, reason string) error {
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
			recordID, newResult, reason, actor.AdminID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET kyc_status = $2,
				kind = CASE WHEN $2 = 'verified' THEN 'enterprise' ELSE kind END, updated_at = $3
			WHERE id = $1`, userID, newResult, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("kyc.review", "kyc_record", strconv.FormatInt(recordID, 10),
			map[string]any{"approve": approve, "reason": reason, "user_id": userID}))
	})
}
