package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

func TestWithTxCommitAndRollback(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	boom := errors.New("boom")

	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		if err := audit.Record(ctx, tx, dbtest.T0, audit.Entry{ActorType: "system", Action: "test.rollback"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}

	err = db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		return audit.Record(ctx, tx, dbtest.T0, audit.Entry{ActorType: "system", Action: "test.commit", Diff: map[string]int{"a": 1}})
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, _ := pool.Query(ctx, "SELECT action FROM audit_logs ORDER BY id")
	actions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0] != "test.commit" {
		t.Fatalf("actions = %v, want [test.commit]", actions)
	}
}

// 多个副本同时启动时并发执行迁移，必须全部成功（由会话级咨询锁串行化）。
func TestConcurrentMigrate(t *testing.T) {
	url := dbtest.EmptyURL(t)
	ctx := context.Background()
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { errs <- db.Migrate(ctx, url) }()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}
}

func TestWhere(t *testing.T) {
	var w db.Where
	if w.SQL() != "TRUE" || len(w.Args) != 0 {
		t.Fatalf("empty = %q %v", w.SQL(), w.Args)
	}
	w.Add("state = ?", "idle")
	w.Add("created_at >= ? AND created_at < ?", 1, 2)
	if got := w.SQL(); got != "state = $1 AND created_at >= $2 AND created_at < $3" || len(w.Args) != 3 {
		t.Fatalf("sql = %q args = %v", got, w.Args)
	}
}

// README 建议在 SPARK_DATABASE_URL 里加 pool_max_conns 等连接池参数：迁移连接必须接受（不能把它当成服务器参数发给 PostgreSQL）。
func TestMigrateAcceptsPoolParams(t *testing.T) {
	url := dbtest.EmptyURL(t) + "&pool_max_conns=10&pool_min_conns=1"
	ctx := context.Background()
	if err := db.Migrate(ctx, url); err != nil {
		t.Fatalf("migrate with pool params: %v", err)
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open with pool params: %v", err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 10 {
		t.Fatalf("MaxConns = %d, want 10", got)
	}
}
