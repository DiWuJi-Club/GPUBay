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
