package storage_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/storage"
)

const goodKey = "2026/01/abcdefghijklmnopqrstuvwxyz012345.png"

func TestValidKey(t *testing.T) {
	for key, ok := range map[string]bool{
		goodKey: true,
		"2026/01/abcdefghijklmnopqrstuvwxyz012345.pdf":    true,
		"2026/01/abcdefghijklmnopqrstuvwxyz012345.webp":   true,
		"2026/01/abcdefghijklmnopqrstuvwxyz012345.exe":    false,
		"2026/01/ABCDEFGHIJKLMNOPQRSTUVWXYZ012345.png":    false,
		"../01/abcdefghijklmnopqrstuvwxyz012345.png":      false,
		"2026/01/../abcdefghijklmnopqrstuvwxyz0123.png":   false,
		"/2026/01/abcdefghijklmnopqrstuvwxyz012345.png":   false,
		"2026/01/abcdefghijklmnopqrstuvwxyz012345.png/..": false,
		"": false,
	} {
		if storage.ValidKey(key) != ok {
			t.Fatalf("ValidKey(%q) = %v, want %v", key, !ok, ok)
		}
	}
}

func TestLocalDiskRoundTrip(t *testing.T) {
	root := t.TempDir()
	d, err := storage.NewLocalDisk(filepath.Join(root, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	n, err := d.Put(ctx, goodKey, strings.NewReader("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Put = %d, %v", n, err)
	}
	info, err := os.Stat(filepath.Join(root, "uploads", "2026", "01", "abcdefghijklmnopqrstuvwxyz012345.png"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("stored file = %v, %v", info, err)
	}
	rc, err := d.Open(ctx, goodKey)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}
	if err := d.Delete(ctx, goodKey); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Open(ctx, goodKey); apperr.As(err) == nil || apperr.As(err).Code != "not_found" {
		t.Fatalf("open after delete = %v", err)
	}
	if err := d.Delete(ctx, goodKey); err != nil {
		t.Fatalf("deleting a missing file must succeed: %v", err)
	}
	if _, err := d.Put(ctx, "../escape.png", strings.NewReader("x")); err == nil {
		t.Fatal("Put accepted a traversal key")
	}
	if _, err := d.Open(ctx, "../../etc/passwd"); apperr.As(err) == nil {
		t.Fatalf("Open traversal = %v", err)
	}
	if _, err := storage.NewLocalDisk(""); err == nil {
		t.Fatal("empty root must be rejected")
	}
}
