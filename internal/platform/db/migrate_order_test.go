package db_test

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
)

// 各计划在不同分支预留迁移号：库里已经有 00003 之后，晚到的 00002 也要能补跑（计划 11 §15、followups）。
func TestMigrateFSAllowsOutOfOrder(t *testing.T) {
	ctx := context.Background()
	url := dbtest.EmptyURL(t)
	mig := func(name, table string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE " + table + " (id int);\n-- +goose Down\nDROP TABLE " + table + ";\n")}
	}
	first := fstest.MapFS{
		"00001_a.sql": mig("00001_a.sql", "t_a"),
		"00003_c.sql": mig("00003_c.sql", "t_c"),
	}
	if err := db.MigrateFS(ctx, url, first); err != nil {
		t.Fatal(err)
	}
	later := fstest.MapFS{
		"00001_a.sql": first["00001_a.sql"],
		"00002_b.sql": mig("00002_b.sql", "t_b"),
		"00003_c.sql": first["00003_c.sql"],
	}
	if err := db.MigrateFS(ctx, url, later); err != nil {
		t.Fatalf("out-of-order migration should apply: %v", err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, tbl := range []string{"t_a", "t_b", "t_c"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, tbl).Scan(&ok); err != nil || !ok {
			t.Fatalf("table %s missing (err %v)", tbl, err)
		}
	}
}
