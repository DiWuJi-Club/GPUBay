package llm

import (
	"errors"
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(""); got != 8 {
		t.Fatalf("empty = %d", got)
	}
	if got := EstimateTokens("你好世界"); got != 10 {
		t.Fatalf("4 runes = %d", got)
	}
	if got := EstimateTokens("abc"); got != 9 {
		t.Fatalf("3 runes = %d", got)
	}
}

func TestErrorStrings(t *testing.T) {
	cause := errors.New("boom")
	u := &UnavailableError{Cause: cause, Status: 502}
	if !strings.HasPrefix(u.Error(), "LLM 不可用: ") || !errors.Is(u, cause) {
		t.Fatalf("unavailable: %q", u.Error())
	}
	if (&UnavailableError{}).Error() != "LLM 不可用" {
		t.Fatal("empty unavailable")
	}
	r := &RequestError{Status: 401, Body: strings.Repeat("x", 500)}
	if !strings.Contains(r.Error(), "401") || len([]rune(r.Error())) > 260 {
		t.Fatalf("request: %q", r.Error())
	}
}
