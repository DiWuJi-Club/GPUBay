package identity

import (
	"context"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/referral"
)

// 计划 8：identity 作为 referral.UserLookup。

func (s *Service) UserState(ctx context.Context, q db.Querier, id int64) (referral.UserState, error) {
	var u referral.UserState
	err := q.QueryRow(ctx, `SELECT status, kyc_status, phone, last_login_ip, last_login_at FROM users WHERE id = $1`, id).
		Scan(&u.Status, &u.KYCStatus, &u.Phone, &u.LastLoginIP, &u.LastLoginAt)
	if db.IsNoRows(err) {
		return u, apperr.ErrNotFound
	}
	return u, err
}

func (s *Service) HasDevice(ctx context.Context, q db.Querier, userID int64, deviceID string) (bool, error) {
	var has bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_devices WHERE user_id = $1 AND device_id = $2)`, userID, deviceID).Scan(&has)
	return has, err
}

// VerifiedFingerprints 返回每个用户全部 verified 且指纹非空的实名指纹（去重，按首次出现的记录排序）。
func (s *Service) VerifiedFingerprints(ctx context.Context, q db.Querier, userIDs []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(userIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT user_id, fingerprint FROM kyc_records
		WHERE user_id = ANY($1) AND result = 'verified' AND fingerprint <> '' GROUP BY user_id, fingerprint ORDER BY user_id, min(id)`, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var fp string
		if err := rows.Scan(&id, &fp); err != nil {
			return nil, err
		}
		out[id] = append(out[id], fp)
	}
	return out, rows.Err()
}
