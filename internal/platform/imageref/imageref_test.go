package imageref_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"spark-cloud/internal/platform/imageref"
)

func TestFamiliar(t *testing.T) {
	for in, want := range map[string]string{
		"spark/llm-serving:25.09":                       "spark/llm-serving:25.09",
		"nvcr.io/nvidia/vllm:25.09-py3":                 "nvcr.io/nvidia/vllm:25.09-py3",
		"registry.spark.internal/base/cuda:ubuntu24.04": "registry.spark.internal/base/cuda:ubuntu24.04",
		"localhost:5000/foo":                            "localhost:5000/foo:latest",
		"localhost:5000/foo:1":                          "localhost:5000/foo:1",
		"docker.io/library/ubuntu:24.04":                "ubuntu:24.04",
		"index.docker.io/library/ubuntu:24.04":          "ubuntu:24.04",
		"library/ubuntu":                                "ubuntu:latest",
		"docker.io/someone/tool:2":                      "someone/tool:2",
		"ubuntu":                                        "ubuntu:latest",
		"  spark/pytorch:25.09 ":                        "spark/pytorch:25.09",
		"nvcr.io/x@sha256:abc":                          "nvcr.io/x@sha256:abc", // 摘要引用原样保留（docker image ls 不会给出，不会匹配）
		"":                                              "",
	} {
		if got := imageref.Familiar(in); got != want {
			t.Errorf("Familiar(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeFiltersDedupesSortsAndCaps(t *testing.T) {
	got := imageref.Normalize([]string{"spark/llm-serving:25.09", "<none>:<none>", "nvcr.io/nvidia/vllm:25.09-py3", "", "spark/llm-serving:25.09", "foo:<none>", "docker.io/library/ubuntu:24.04", "ubuntu:24.04"})
	if want := []string{"nvcr.io/nvidia/vllm:25.09-py3", "spark/llm-serving:25.09", "ubuntu:24.04"}; !slices.Equal(got, want) {
		t.Fatalf("normalize = %v, want %v", got, want)
	}
	many := make([]string, 250)
	for i := range many {
		many[i] = fmt.Sprintf("img%03d:1", 249-i)
	}
	got = imageref.Normalize(many)
	if len(got) != imageref.MaxItems || got[0] != "img000:1" || got[199] != "img199:1" {
		t.Fatalf("cap: %d items, first %q last %q", len(got), got[0], got[len(got)-1])
	}
	got = imageref.Normalize([]string{"a:" + strings.Repeat("x", 300)})
	if len(got) != 1 || len(got[0]) != imageref.MaxLen {
		t.Fatalf("long item = %v", got)
	}
	// 截断不切开多字节字符（text[] 只收合法 UTF-8）
	got = imageref.Normalize([]string{"a:" + strings.Repeat("中", 100)})
	if len(got) != 1 || len(got[0]) > imageref.MaxLen || !utf8.ValidString(got[0]) {
		t.Fatalf("multibyte truncation = %d %q", len(got[0]), got[0])
	}
	if got := imageref.Normalize(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty input must give empty, non-nil list: %#v", got)
	}
}
