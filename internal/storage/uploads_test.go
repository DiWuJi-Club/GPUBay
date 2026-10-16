package storage_test

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/storage"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

var (
	pngHeader = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpgHeader = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	pdfHeader = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
)

func newUploads(t *testing.T) (*storage.Service, *pgxpool.Pool, *clock.Fake) {
	t.Helper()
	pool := dbtest.New(t)
	disk, err := storage.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(dbtest.T0)
	return storage.NewService(pool, clk, disk), pool, clk
}

func code(err error) string {
	if ae := apperr.As(err); ae != nil {
		return ae.Code
	}
	if err != nil {
		return "raw: " + err.Error()
	}
	return ""
}

func TestSaveDetectsTypeAndRecordsOwner(t *testing.T) {
	svc, pool, _ := newUploads(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000001", true)
	for header, ext := range map[string]string{string(pngHeader): "png", string(jpgHeader): "jpg", string(pdfHeader): "pdf"} {
		up, err := svc.Save(ctx, uid, storage.PurposeLicense, strings.NewReader(header+"payload"))
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^2026/01/[a-z0-9]{32}\.`+ext+`$`).MatchString(up.Key) || up.Size != int64(len(header)+7) {
			t.Fatalf("upload = %+v", up)
		}
		if err := svc.CheckOwned(ctx, up.Key, uid, storage.PurposeLicense); err != nil {
			t.Fatalf("CheckOwned own file: %v", err)
		}
		got, rc, err := svc.Open(ctx, up.Key)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if got.UserID != uid || !bytes.HasPrefix(b, []byte(header)) {
			t.Fatalf("open = %+v %q", got, b)
		}
	}
}

func TestSaveRejectsBadFiles(t *testing.T) {
	svc, pool, _ := newUploads(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000001", true)
	cases := []struct {
		name    string
		purpose string
		body    io.Reader
		want    string
	}{
		{"text disguised", storage.PurposeLicense, strings.NewReader("just text, not an image"), "unsupported_file_type"},
		{"html", storage.PurposeLicense, strings.NewReader("<html><script>alert(1)</script>"), "unsupported_file_type"},
		{"empty", storage.PurposeLicense, strings.NewReader(""), "bad_request"},
		{"bad purpose", "avatar", bytes.NewReader(pngHeader), "bad_request"},
		{"too large", storage.PurposeLicense, io.MultiReader(bytes.NewReader(pngHeader), bytes.NewReader(make([]byte, storage.MaxUploadSize))), "file_too_large"},
	}
	for _, c := range cases {
		if _, err := svc.Save(ctx, uid, c.purpose, c.body); code(err) != c.want {
			t.Fatalf("%s: err = %v, want %s", c.name, err, c.want)
		}
	}
	exact := append(append([]byte{}, pngHeader...), make([]byte, storage.MaxUploadSize-len(pngHeader))...)
	if _, err := svc.Save(ctx, uid, storage.PurposeLicense, bytes.NewReader(exact)); err != nil {
		t.Fatalf("exactly 10MB must be accepted: %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM uploads`).Scan(&n)
	if n != 1 {
		t.Fatalf("uploads rows = %d, want 1", n)
	}
}

func TestCheckOwnedRejectsOthers(t *testing.T) {
	svc, pool, _ := newUploads(t)
	ctx := context.Background()
	owner := dbtest.CreateUser(t, pool, "13900000001", true)
	other := dbtest.CreateUser(t, pool, "13900000002", true)
	up, err := svc.Save(ctx, owner, storage.PurposeLicense, bytes.NewReader(pngHeader))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		key string
		uid int64
	}{{up.Key, other}, {"2026/01/abcdefghijklmnopqrstuvwxyz012345.png", owner}, {"../x.png", owner}, {"https://evil/x.png", owner}} {
		if err := svc.CheckOwned(ctx, c.key, c.uid, storage.PurposeLicense); code(err) != "bad_request" {
			t.Fatalf("CheckOwned(%q, %d) = %v", c.key, c.uid, err)
		}
	}
	if _, _, err := svc.Open(ctx, "2026/01/abcdefghijklmnopqrstuvwxyz012345.png"); code(err) != "not_found" {
		t.Fatalf("open unknown = %v", err)
	}
}

func TestSaveDailyLimit(t *testing.T) {
	svc, pool, clk := newUploads(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000001", true)
	for i := 0; i < 20; i++ {
		if _, err := svc.Save(ctx, uid, storage.PurposeLicense, bytes.NewReader(pngHeader)); err != nil {
			t.Fatalf("upload %d: %v", i+1, err)
		}
	}
	if _, err := svc.Save(ctx, uid, storage.PurposeLicense, bytes.NewReader(pngHeader)); code(err) != "rate_limited" {
		t.Fatalf("21st upload = %v", err)
	}
	clk.Advance(24 * time.Hour)
	if _, err := svc.Save(ctx, uid, storage.PurposeLicense, bytes.NewReader(pngHeader)); err != nil {
		t.Fatalf("next day: %v", err)
	}
}
