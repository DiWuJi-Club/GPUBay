package appdeploy_test

import (
	"strings"
	"testing"

	"spark-cloud/internal/appdeploy"
)

func TestParseStatusOutput(t *testing.T) {
	out := []byte(`{"seq":7,"state":"installing","phase":"download","progress":42,"message":"downloading","pid":100,"service_pid":0,"spec_hash":"h","exit_code":0,"at":"2026-10-20T00:00:00Z"}` + "\nalive=0\nlog_size=12\n---\nline1\nline2\n")
	st, alive, tail, err := appdeploy.ParseStatusOutput(out)
	if err != nil || st.Seq != 7 || st.State != "installing" || st.Phase != "download" || st.Progress != 42 || st.Pid != 100 || st.SpecHash != "h" || alive || tail != "line1\nline2\n" {
		t.Fatalf("parse = %+v %v %q %v", st, alive, tail, err)
	}
	if _, alive, tail, err := appdeploy.ParseStatusOutput([]byte("{\"seq\":1,\"state\":\"running\"}\nalive=1\nlog_size=0\n---\n")); err != nil || !alive || tail != "" {
		t.Fatalf("alive = %v %q %v", alive, tail, err)
	}
	if _, _, _, err := appdeploy.ParseStatusOutput([]byte("not json\nalive=1\n---\n")); err == nil {
		t.Fatal("bad json accepted")
	}
	if _, _, _, err := appdeploy.ParseStatusOutput([]byte("{\"seq\":1,\"state\":\"installing\",\"message\":\"" + strings.Repeat("x", 20000) + "\"}\nalive=1\n---\n")); err == nil {
		t.Fatal("oversized status accepted")
	}
	// run.sh status 在 status.json 缺失时输出不带 state 的 JSON（退出码 3）：不能当成合法状态
	if _, _, _, err := appdeploy.ParseStatusOutput([]byte("{\"error\":\"no status\"}\nalive=0\nlog_size=13\n---\nsome log\n")); err == nil {
		t.Fatal("status without state accepted")
	}
	if _, _, _, err := appdeploy.ParseStatusOutput([]byte("")); err == nil {
		t.Fatal("empty output accepted")
	}
	// 日志尾部超过 64KB 只保留最后 MaxLogTail 字节
	big := strings.Repeat("a", appdeploy.MaxLogTail) + "END"
	_, _, tail, err = appdeploy.ParseStatusOutput([]byte("{\"seq\":1,\"state\":\"running\"}\nalive=1\n---\n" + big))
	if err != nil || len(tail) != appdeploy.MaxLogTail || !strings.HasSuffix(tail, "END") {
		t.Fatalf("tail len = %d %v", len(tail), err)
	}
}
