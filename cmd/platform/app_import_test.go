package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
)

const appImportUsage = "用法：platform admin app import <目录>"

func TestAdminAppImportArgs(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	for _, args := range [][]string{{"app"}, {"app", "import"}, {"app", "export", "dir"}, {"app", "import", "a", "b"}} {
		if err := runAdmin(ctx, nil, nil, args, &out, nil); err == nil || err.Error() != appImportUsage {
			t.Errorf("%v: want %q, got %v", args, appImportUsage, err)
		}
	}
	if !bytes.Contains([]byte(adminUsage), []byte("platform admin app import <目录>")) {
		t.Fatalf("adminUsage missing app import:\n%s", adminUsage)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestAdminAppImportPrintsResults(t *testing.T) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	cat := appdeploy.NewCatalog(pool, clk, fleet.NewService(pool, clk), catalog.NewService(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	root := t.TempDir()
	write := func(version string) {
		dir := filepath.Join(root, "demo")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			"app.json":  `{"name":"演示","category":"tool","version":"` + version + `","images":["PyTorch"]}`,
			"spec.json": `{"ports":[{"port":8000}]}`,
			"app.sh":    "#!/bin/sh\necho ok\n",
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	var out bytes.Buffer
	write("1.0.0")
	if err := runAdmin(context.Background(), nil, cat, []string{"app", "import", root}, &out, nil); err != nil {
		t.Fatal(err)
	}
	write("1.0.1")
	if err := runAdmin(context.Background(), nil, cat, []string{"app", "import", root}, &out, nil); err != nil {
		t.Fatal(err)
	}
	if want := "demo 1.0.0 created\ndemo 1.0.1 version-added\n"; out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
}
