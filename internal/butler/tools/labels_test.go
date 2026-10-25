package tools

import (
	"os"
	"regexp"
	"testing"
)

// TestLabelsMatchFrontend：labels.go 与前端 butlerToolLabel 逐项一致，且恰好是规格 §4 的 23 个工具。
func TestLabelsMatchFrontend(t *testing.T) {
	src, err := os.ReadFile("../../../web/shared/src/labels.ts")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)export const butlerToolLabel: LabelMap = \{(.*?)\n\}`).FindSubmatch(src)
	if block == nil {
		t.Fatal("butlerToolLabel not found in labels.ts")
	}
	ts := map[string]string{}
	for _, m := range regexp.MustCompile(`([a-z_]+): '([^']*)'`).FindAllSubmatch(block[1], -1) {
		ts[string(m[1])] = string(m[2])
	}
	if len(ts) != 23 || len(Labels) != 23 {
		t.Fatalf("labels.ts has %d, Labels has %d; want 23", len(ts), len(Labels))
	}
	for k, v := range Labels {
		if ts[k] != v {
			t.Errorf("%s: go %q, ts %q", k, v, ts[k])
		}
	}
	if len(ReadToolNames) != 15 || len(WriteToolNames) != 8 {
		t.Fatalf("names: %d read, %d write", len(ReadToolNames), len(WriteToolNames))
	}
	for _, n := range append(append([]string{}, ReadToolNames...), WriteToolNames...) {
		if Labels[n] == "" {
			t.Errorf("no label for %s", n)
		}
	}
}
