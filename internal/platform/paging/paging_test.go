package paging_test

import (
	"net/url"
	"testing"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/paging"
)

func TestParse(t *testing.T) {
	p, err := paging.Parse(url.Values{})
	if err != nil || p.Number != 1 || p.Size != 20 || p.Offset() != 0 || p.Limit() != 20 {
		t.Fatalf("default = %+v, %v", p, err)
	}
	p, err = paging.Parse(url.Values{"page": {"3"}, "page_size": {"50"}})
	if err != nil || p.Offset() != 100 || p.Limit() != 50 {
		t.Fatalf("page 3 = %+v, %v", p, err)
	}
	for _, q := range []url.Values{
		{"page": {"0"}}, {"page": {"x"}}, {"page": {"1000001"}},
		{"page_size": {"0"}}, {"page_size": {"101"}}, {"page_size": {"-5"}},
	} {
		if _, err := paging.Parse(q); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
			t.Fatalf("Parse(%v) err = %v, want bad_request", q, err)
		}
	}
}

func TestNormalize(t *testing.T) {
	if p := (paging.Page{}).Normalize(); p.Number != 1 || p.Size != 20 {
		t.Fatalf("zero page = %+v", p)
	}
	if p := (paging.Page{Number: 2, Size: 500}).Normalize(); p.Size != 100 {
		t.Fatalf("oversized page = %+v", p)
	}
}
