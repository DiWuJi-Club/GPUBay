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

func TestSaveDailyLimitConcurrent(t *testing.T) {
	svc, pool, _ := newUploads(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000001", true)
	for i := 0; i < 19; i++ {
		if _, err := svc.Save(ctx, uid, storage.PurposeLicense, bytes.NewReader(pngHeader)); err != nil {
			t.Fatalf("upload %d: %v", i+1, err)
		}
	}
	const n = 5
	codes := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := svc.Save(ctx, uid, storage.PurposeLicense, bytes.NewReader(pngHeader))
			codes <- code(err)
		}()
	}
	ok, limited := 0, 0
	for i := 0; i < n; i++ {
		switch c := <-codes; c {
		case "":
			ok++
		case "rate_limited":
			limited++
		default:
			t.Fatalf("unexpected result %q", c)
		}
	}
	if ok != 1 || limited != n-1 {
		t.Fatalf("ok = %d, limited = %d", ok, limited)
	}
	var cnt int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM uploads`).Scan(&cnt)
	if cnt != 20 {
		t.Fatalf("uploads rows = %d, want 20", cnt)
	}
}

var webpHeader = []byte("RIFF\x24\x00\x00\x00WEBPVP8 ")

func TestSaveWorkflowCover(t *testing.T) {
	svc, pool, _ := newUploads(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13900000001", true)
	for header, ext := range map[string]string{string(pngHeader): "png", string(jpgHeader): "jpg", string(webpHeader): "webp"} {
		up, err := svc.Save(ctx, 0, storage.PurposeWorkflowCover, strings.NewReader(header+"payload"))
		if err != nil {
			t.Fatalf("%s: %v", ext, err)
		}
		if !regexp.MustCompile(`^2026/01/[a-z0-9]{32}\.`+ext+`$`).MatchString(up.Key) || up.Purpose != storage.PurposeWorkflowCover {
			t.Fatalf("upload = %+v", up)
		}
		got, rc, err := svc.Open(ctx, up.Key)
		if err != nil {
			t.Fatal(err)
		}
		rc.Close()
		if got.UserID != 0 || got.ContentType != up.ContentType {
			t.Fatalf("open = %+v", got)
		}
	}
	var nulls int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE user_id IS NULL AND purpose = 'workflow_cover'`).Scan(&nulls)
	if nulls != 3 {
		t.Fatalf("cover rows with NULL owner = %d", nulls)
	}
	cases := []struct {
		name   string
		uid    int64
		purp   string
		body   io.Reader
		code   string
		substr string
	}{
		{"pdf cover", 0, storage.PurposeWorkflowCover, bytes.NewReader(pdfHeader), "unsupported_file_type", "WebP"},
		{"webp license", uid, storage.PurposeLicense, bytes.NewReader(webpHeader), "unsupported_file_type", "PDF"},
		{"cover too large", 0, storage.PurposeWorkflowCover, io.MultiReader(bytes.NewReader(pngHeader), bytes.NewReader(make([]byte, storage.MaxCoverSize))), "file_too_large", "2MB"},
		{"cover with user owner", uid, storage.PurposeWorkflowCover, bytes.NewReader(pngHeader), "bad_request", ""},
		{"license without owner", 0, storage.PurposeLicense, bytes.NewReader(pngHeader), "bad_request", ""},
	}
	for _, c := range cases {
		_, err := svc.Save(ctx, c.uid, c.purp, c.body)
		if code(err) != c.code || !strings.Contains(err.Error(), c.substr) {
			t.Fatalf("%s: err = %v, want %s (%s)", c.name, err, c.code, c.substr)
		}
		if ae := apperr.As(err); c.code == "file_too_large" && ae.Status != 413 {
			t.Fatalf("%s: status = %d", c.name, ae.Status)
		}
	}
	exact := append(append([]byte{}, pngHeader...), make([]byte, storage.MaxCoverSize-len(pngHeader))...)
	if _, err := svc.Save(ctx, 0, storage.PurposeWorkflowCover, bytes.NewReader(exact)); err != nil {
		t.Fatalf("exactly 2MB must be accepted: %v", err)
	}
}

// 封面不计每日次数（所有者是平台）。
func TestSaveWorkflowCoverNoDailyLimit(t *testing.T) {
	svc, _, _ := newUploads(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		if _, err := svc.Save(ctx, 0, storage.PurposeWorkflowCover, bytes.NewReader(pngHeader)); err != nil {
			t.Fatalf("cover %d: %v", i+1, err)
		}
	}
}

func TestDeleteUpload(t *testing.T) {
	svc, pool, _ := newUploads(t)
	ctx := context.Background()
	up, err := svc.Save(ctx, 0, storage.PurposeWorkflowCover, bytes.NewReader(webpHeader))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, up.Key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Open(ctx, up.Key); code(err) != "not_found" {
		t.Fatalf("open after delete = %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM uploads`).Scan(&n)
	if n != 0 {
		t.Fatalf("rows after delete = %d", n)
	}
	if err := svc.Delete(ctx, up.Key); err != nil { // 幂等
		t.Fatalf("second delete = %v", err)
	}
	if err := svc.Delete(ctx, "../x.png"); code(err) != "bad_request" {
		t.Fatalf("bad key delete = %v", err)
	}
}
