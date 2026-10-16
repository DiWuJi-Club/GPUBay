package pay

import "testing"

func TestYuanToFen(t *testing.T) {
	for s, want := range map[string]int64{
		"100": 10000, "0.1": 10, "0.01": 1, "1.00": 100, "12.5": 1250, "1000000.00": 100_000_000, "0": 0,
	} {
		got, err := yuanToFen(s)
		if err != nil || got != want {
			t.Fatalf("yuanToFen(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "1.005", "-1.00", "1,00", ".5", "1.", "+1", " 1.00", "1e3", "12345678901.00"} {
		if got, err := yuanToFen(bad); err == nil {
			t.Fatalf("yuanToFen(%q) = %d, want error", bad, got)
		}
	}
}

func TestFenToYuan(t *testing.T) {
	for fen, want := range map[int64]string{1: "0.01", 10: "0.10", 5000: "50.00", 100_000_000: "1000000.00"} {
		if got := fenToYuan(fen); got != want {
			t.Fatalf("fenToYuan(%d) = %q, want %q", fen, got, want)
		}
	}
}
