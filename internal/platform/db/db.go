// Package db 提供连接池、迁移和事务辅助。
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // 注册 database/sql 驱动 "pgx"，供 goose 使用
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"spark-cloud/migrations"
)

// Querier 由 *pgxpool.Pool 与 pgx.Tx 共同满足，只读查询可以传任意一个。
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return pool, nil
}

func Migrate(ctx context.Context, url string) error {
	return MigrateFS(ctx, url, migrations.FS)
}

// MigrateFS 按 fsys 里的迁移执行 Up。允许乱序（WithAllowOutofOrder）：各计划在不同分支上预留迁移号
// （如 00022～00023 尚未合并时 00024 已上线），较小的编号后到时照常补跑，而不是让平台启动失败。
// 各计划的迁移只新增 / 修改各自的表，互不依赖执行顺序（规格计划 11 §15）。
func MigrateFS(ctx context.Context, url string, fsys fs.FS) error {
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		return fmt.Errorf("open migrate conn: %w", err)
	}
	defer sqlDB.Close()
	// 会话级咨询锁：多个副本同时启动时串行执行迁移。
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("goose locker: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys, goose.WithSessionLocker(locker), goose.WithAllowOutofOrder(true))
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// WithTx 在事务中执行 fn；fn 返回错误时回滚并原样返回该错误。
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // 提交后回滚是空操作
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// IsForeignKeyViolation 判断是否违反外键约束（引用了不存在的记录）。
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// IsExclusionViolation 判断是否违反排他约束（例如同一台机器的占用时段重叠）。
func IsExclusionViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}

// ConstraintName 返回违反的约束名（非数据库错误返回空串），用于区分同一张表上的多个约束。
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// Where 拼接带参数的 AND 条件；条件中的 ? 依次替换为 $1、$2……，用于后台列表的可选筛选。
type Where struct {
	conds []string
	Args  []any
}

func (w *Where) Add(cond string, args ...any) {
	var b strings.Builder
	i := 0
	for _, r := range cond {
		if r == '?' && i < len(args) {
			w.Args = append(w.Args, args[i])
			b.WriteString("$" + strconv.Itoa(len(w.Args)))
			i++
			continue
		}
		b.WriteRune(r)
	}
	w.conds = append(w.conds, b.String())
}

func (w *Where) SQL() string {
	if len(w.conds) == 0 {
		return "TRUE"
	}
	return strings.Join(w.conds, " AND ")
}
