package csvx_test

import (
	"bytes"
	"testing"
	"time"

	"spark-cloud/internal/platform/csvx"
)

func TestYuan(t *testing.T) {
	for fen, want := range map[int64]string{0: "0.00", 5: "0.05", 100: "1.00", 6900: "69.00", 159901: "1599.01", -1234: "-12.34", -5: "-0.05"} {
		if got := csvx.Yuan(fen); got != want {
			t.Fatalf("Yuan(%d) = %q, want %q", fen, got, want)
		}
	}
}

func TestTimeUsesBeijing(t *testing.T) {
	if got := csvx.Time(time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC)); got != "2026-01-02 00:30:00" {
		t.Fatalf("Time = %q", got)
	}
	if got := csvx.TimePtr(nil); got != "" {
		t.Fatalf("TimePtr(nil) = %q", got)
	}
}

func TestWriterWritesBOMAndRows(t *testing.T) {
	var buf bytes.Buffer
	w, err := csvx.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write([]string{"金额", "备注"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Write([]string{"-12.34", "含,逗号"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "\xEF\xBB\xBF金额,备注\n-12.34,\"含,逗号\"\n"
	if buf.String() != want {
		t.Fatalf("csv = %q, want %q", buf.String(), want)
	}
}

func TestWriterEscapesFormulas(t *testing.T) {
	var buf bytes.Buffer
	w, _ := csvx.NewWriter(&buf)
	_ = w.Write([]string{"=HYPERLINK(\"x\")", "+1", "@SUM(A1)", "-cmd", "-0.50", "\tx"})
	_ = w.Flush()
	// 含双引号的单元格按 CSV 规则整体加引号并转义内部引号
	want := "\xEF\xBB\xBF\"'=HYPERLINK(\"\"x\"\")\",'+1,'@SUM(A1),'-cmd,-0.50,'\tx\n"
	if buf.String() != want {
		t.Fatalf("csv = %q, want %q", buf.String(), want)
	}
}
