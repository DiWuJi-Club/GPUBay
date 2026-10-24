package appdeploy

import (
	"encoding/json"
	"testing"
)

// 视图里的 spec：ports 没有 omitempty，nil 会序列化成 null；env / links 为空时省略（前端 AppSpec 里是可选字段），从不为 null。
func TestNormalizeSpecNeverNull(t *testing.T) {
	b, err := json.Marshal(normalizeSpec(Spec{}))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"ports":[]}` {
		t.Fatalf("normalized empty spec = %s", b)
	}
	in := Spec{Ports: []PortSpec{{Port: 8000}}}
	out := normalizeSpec(in)
	if in.Env != nil || out.Env == nil || out.Links == nil {
		t.Fatalf("normalizeSpec must copy: in %+v out %+v", in, out)
	}
}

// 「打开」= 主端口 URL + ports[0].path（A-4：vLLM 根路径 404，打开 /docs）；没有 path 时就是主端口 URL。
func TestOpenURL(t *testing.T) {
	for _, tc := range []struct {
		sp   Spec
		want string
	}{
		{Spec{Ports: []PortSpec{{Port: 8000, Path: "/docs"}}}, "https://ins-a-8000.x/docs"},
		{Spec{Ports: []PortSpec{{Port: 3000}}}, "https://ins-a-8000.x/"},
		{Spec{Ports: []PortSpec{{Port: 3000}, {Port: 3001, Path: "/admin"}}}, "https://ins-a-8000.x/"}, // 只看主端口
		{Spec{}, "https://ins-a-8000.x/"},
	} {
		if got := openURL("https://ins-a-8000.x/", tc.sp); got != tc.want {
			t.Errorf("openURL(%+v) = %q, want %q", tc.sp, got, tc.want)
		}
	}
}
