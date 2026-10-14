package dbtest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CreateUser 直接插入一个用户；verified 为 true 时实名状态为 verified。
func CreateUser(t *testing.T, pool *pgxpool.Pool, phone string, verified bool) int64 {
	t.Helper()
	status := "none"
	if verified {
		status = "verified"
	}
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (phone, kyc_status, created_at, updated_at) VALUES ($1, $2, $3, $3) RETURNING id`,
		phone, status, T0).Scan(&id)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id
}
