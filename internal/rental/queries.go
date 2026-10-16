package rental

import (
	"context"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

func (s *Service) Get(ctx context.Context, id string) (Instance, error) {
	return scanInstance(s.d.Pool.QueryRow(ctx, `SELECT `+instanceCols+` FROM instances WHERE id = $1`, id))
}

func (s *Service) GetForUser(ctx context.Context, userID int64, id string) (Instance, error) {
	return scanInstance(s.d.Pool.QueryRow(ctx, `SELECT `+instanceCols+` FROM instances WHERE id = $1 AND user_id = $2`, id, userID))
}

func (s *Service) ListForUser(ctx context.Context, userID int64) ([]Instance, error) {
	return collectInstances(s.d.Pool.Query(ctx, `SELECT `+instanceCols+` FROM instances
		WHERE user_id = $1 ORDER BY created_at DESC, id LIMIT 100`, userID))
}

// Password 解密返回实例的 SSH 密码。
func (s *Service) Password(ctx context.Context, userID int64, id string) (string, error) {
	var enc []byte
	if err := s.d.Pool.QueryRow(ctx, `SELECT ssh_password_enc FROM instances WHERE id = $1 AND user_id = $2`, id, userID).Scan(&enc); err != nil {
		if db.IsNoRows(err) {
			return "", apperr.ErrNotFound
		}
		return "", err
	}
	plain, err := s.d.Box.Open(enc)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ListInstancesByMachine 返回某台机器上最近的实例（新到旧）。
func (s *Service) ListInstancesByMachine(ctx context.Context, machineID int64, limit int) ([]Instance, error) {
	return collectInstances(s.d.Pool.Query(ctx, `SELECT `+instanceCols+` FROM instances
		WHERE machine_id = $1 ORDER BY created_at DESC, id LIMIT $2`, machineID, limit))
}
