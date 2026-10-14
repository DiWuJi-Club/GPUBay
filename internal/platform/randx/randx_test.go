package randx_test

import (
	"regexp"
	"testing"

	"spark-cloud/internal/platform/randx"
)

func TestDigits(t *testing.T) {
	if got := randx.Digits(6); !regexp.MustCompile(`^\d{6}$`).MatchString(got) {
		t.Fatalf("Digits(6) = %q", got)
	}
}

func TestID(t *testing.T) {
	got := randx.ID("ins-", 8)
	if !regexp.MustCompile(`^ins-[a-z0-9]{8}$`).MatchString(got) {
		t.Fatalf("ID = %q", got)
	}
	if randx.ID("ins-", 8) == got {
		t.Fatal("two IDs should differ")
	}
}

func TestPasswordAndToken(t *testing.T) {
	if got := randx.Password(16); !regexp.MustCompile(`^[A-HJ-NP-Za-km-z2-9]{16}$`).MatchString(got) {
		t.Fatalf("Password = %q", got)
	}
	if got := randx.Token(); len(got) != 43 {
		t.Fatalf("Token length = %d, want 43", len(got))
	}
}
