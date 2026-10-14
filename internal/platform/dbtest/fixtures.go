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

// Fund 给用户直接加余额（同时写一条 admin_adjust 流水，保持账实一致）。
func Fund(t *testing.T, pool *pgxpool.Pool, userID, amount int64) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		WITH acc AS (
			INSERT INTO wallet_accounts (user_id, balance, updated_at) VALUES ($1, $2, $3)
			ON CONFLICT (user_id) DO UPDATE SET balance = wallet_accounts.balance + EXCLUDED.balance
			RETURNING balance
		)
		INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, remark, created_at)
		SELECT $1, $2, balance, 'admin_adjust', 'test fund', $3 FROM acc`, userID, amount, T0)
	if err != nil {
		t.Fatalf("Fund: %v", err)
	}
}
