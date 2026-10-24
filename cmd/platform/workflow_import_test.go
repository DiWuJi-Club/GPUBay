package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/comfywf"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/storage"
)

func TestAdminWorkflowImportArgs(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	for _, args := range [][]string{{"workflow"}, {"workflow", "import"}, {"workflow", "export", "dir"}, {"workflow", "import", "a", "b"}} {
		if err := runAdmin(ctx, nil, nil, nil, args, &out, nil); err == nil || err.Error() != workflowImportUsage {
			t.Errorf("%v: want %q, got %v", args, workflowImportUsage, err)
		}
	}
	if !strings.Contains(adminUsage, "platform admin workflow import <目录>") {
		t.Fatalf("adminUsage missing workflow import:\n%s", adminUsage)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestAdminWorkflowImportPrintsResults(t *testing.T) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	disk, err := storage.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wf := comfywf.NewCatalog(pool, clk, storage.NewService(pool, clk, disk), slog.New(slog.NewTextHandler(io.Discard, nil)))
	src := filepath.Join("..", "..", "internal", "comfywf", "testdata", "import", "sdxl-demo")
	root := t.TempDir()
	write := func(slug, version string) {
		dir := filepath.Join(root, slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		ents, err := os.ReadDir(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, ent := range ents {
			b, err := os.ReadFile(filepath.Join(src, ent.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if ent.Name() == "workflow.json" {
				b = bytes.Replace(b, []byte(`"version": "1.0.0"`), []byte(`"version": "`+version+`"`), 1)
			}
			if err := os.WriteFile(filepath.Join(dir, ent.Name()), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	var out bytes.Buffer
	write("demo", "1.0.0")
	if err := runAdmin(context.Background(), nil, nil, wf, []string{"workflow", "import", root}, &out, nil); err != nil {
		t.Fatal(err)
	}
	write("demo", "1.0.1")
	write("other", "1.0.0")
	if err := os.WriteFile(filepath.Join(root, "other", "manifest.json"), []byte(`{"models":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "demo"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	err = runAdmin(context.Background(), nil, nil, wf, []string{"workflow", "import", root}, &out, nil)
	if err == nil || !strings.Contains(err.Error(), "1 个工作流目录导入失败") {
		t.Fatalf("second import err = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 || lines[0] != "demo 1.0.0 created" || lines[1] != "demo 1.0.1 version-added" ||
		lines[2] != "link skipped: link 是符号链接，已跳过" || !strings.HasPrefix(lines[3], "other error: workflow_version_invalid: ") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// 仓库里随平台发布的 deploy/workflows 经 CLI 全部导入为草稿（含封面），每个草稿版本都能发布（发布时按锁住的内容重新完整校验）。
func TestAdminWorkflowImportDeployDir(t *testing.T) {
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	disk, err := storage.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	wf := comfywf.NewCatalog(pool, clk, storage.NewService(pool, clk, disk), slog.New(slog.NewTextHandler(io.Discard, nil)))
	dir := filepath.Join("..", "..", "deploy", "workflows")
	var out bytes.Buffer
	if err := runAdmin(ctx, nil, nil, wf, []string{"workflow", "import", dir}, &out, nil); err != nil {
		t.Fatalf("import: %v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	dirs := 0
	for _, e := range ents {
		if e.IsDir() {
			dirs++
		}
	}
	if len(lines) != dirs || dirs == 0 {
		t.Fatalf("imported %d lines for %d dirs:\n%s", len(lines), dirs, out.String())
	}
	rows, err := wf.ListAdmin(ctx)
	if err != nil || len(rows) != dirs {
		t.Fatalf("workflows = %d %v", len(rows), err)
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, " created") {
			t.Errorf("line %q", l)
		}
		w, err := wf.WorkflowBySlug(ctx, strings.Fields(l)[0])
		if err != nil {
			t.Fatal(err)
		}
		vs, err := wf.ListVersions(ctx, w.ID)
		if err != nil || len(vs) != 1 {
			t.Fatalf("%s versions = %d %v", w.Slug, len(vs), err)
		}
		if _, err := wf.PublishVersion(ctx, audit.Actor{}, w.ID, vs[0].ID); err != nil {
			t.Errorf("publish %s %s: %v", w.Slug, vs[0].Version, err)
		}
	}
}
