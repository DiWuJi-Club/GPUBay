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

// ProductID 返回种子商品 spark-1 的 ID。
func ProductID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM products WHERE code = 'spark-1'`).Scan(&id); err != nil {
		t.Fatalf("ProductID: %v", err)
	}
	return id
}

// ImageID 返回排序最靠前的启用镜像 ID。
func ImageID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM images WHERE enabled ORDER BY sort, id LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("ImageID: %v", err)
	}
	return id
}

// CreateMachine 插入一台属于 spark-1、空闲且在线的机器（last_seen_at = T0）。
func CreateMachine(t *testing.T, pool *pgxpool.Pool, serial string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `INSERT INTO machines
		(serial, lan_ip, product_id, state, agent_online, last_seen_at, created_at, updated_at)
		VALUES ($1, '10.0.0.10', (SELECT id FROM products WHERE code = 'spark-1'), 'idle', true, $2, $2, $2)
		RETURNING id`, serial, T0).Scan(&id)
	if err != nil {
		t.Fatalf("CreateMachine: %v", err)
	}
	return id
}
