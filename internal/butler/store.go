package butler

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
)

// Store 是管家的数据访问层：用量、设置等基础 SQL；Service 嵌入它。
type Store struct {
	pool *pgxpool.Pool
	clk  clock.Clock

	// 设置的进程内缓存（SettingsCacheTTL）；gen 在失效 / 更新时递增，防止并发读把旧值写回缓存。
	setMu  sync.Mutex
	setVal Settings
	setAt  time.Time
	setOK  bool
	setGen uint64
}

func NewStore(pool *pgxpool.Pool, clk clock.Clock) *Store {
	return &Store{pool: pool, clk: clk}
}

// withTx 在事务中执行 fn；fn 返回错误时回滚并原样返回。
func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return db.WithTx(ctx, s.pool, fn)
}
