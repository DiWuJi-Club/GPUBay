package tools

import (
	"errors"
	"strings"
	"testing"
)

const testSchema = `{
	"type": "object",
	"properties": {
		"instance_id": {"type": "string", "pattern": "^ins-[a-z0-9]{1,32}$", "description": "实例 ID"},
		"duration": {"type": "string", "enum": ["1d", "1w", "1m"]},
		"quantity": {"type": "integer", "minimum": 1, "maximum": 12, "default": 1},
		"ratio": {"type": "number", "minimum": 0, "maximum": 1},
		"auto_renew": {"type": "boolean"},
		"name": {"type": "string", "minLength": 2, "maxLength": 4},
		"tags": {"type": "array", "items": {"type": "string", "maxLength": 3}, "maxItems": 2},
		"env": {"type": "object", "additionalProperties": {"type": "string", "maxLength": 5}},
		"nested": {"type": "object", "properties": {"k": {"type": "integer", "enum": [1, 2]}}, "required": ["k"], "additionalProperties": false}
	},
	"required": ["instance_id", "duration"],
	"additionalProperties": false
}`

func TestValidateMatrix(t *testing.T) {
	cases := []struct {
		name, in string
		want     string // 规范化后的 JSON；err 非空时忽略
		err      string // 错误文案片段
	}{
		{"ok defaults filled", `{"instance_id":"ins-abc","duration":"1d"}`, `{"duration":"1d","instance_id":"ins-abc","quantity":1}`, ""},
		{"null removed then default", `{"instance_id":"ins-abc","duration":"1w","quantity":null,"auto_renew":null}`, `{"duration":"1w","instance_id":"ins-abc","quantity":1}`, ""},
		{"missing required", `{"instance_id":"ins-abc"}`, "", "参数 duration：必填"},
		{"required null counts as missing", `{"instance_id":"ins-abc","duration":null}`, "", "参数 duration：必填"},
		{"wrong type string", `{"instance_id":5,"duration":"1d"}`, "", "参数 instance_id：应为字符串"},
		{"wrong type bool", `{"instance_id":"ins-a","duration":"1d","auto_renew":"yes"}`, "", "参数 auto_renew：应为布尔值"},
		{"bool string true", `{"instance_id":"ins-a","duration":"1d","auto_renew":"true"}`, `{"auto_renew":true,"duration":"1d","instance_id":"ins-a","quantity":1}`, ""},
		{"bool string false", `{"instance_id":"ins-a","duration":"1d","auto_renew":" false "}`, `{"auto_renew":false,"duration":"1d","instance_id":"ins-a","quantity":1}`, ""},
		{"bool string other", `{"instance_id":"ins-a","duration":"1d","auto_renew":"True1"}`, "", "参数 auto_renew：应为布尔值"},
		{"bool number", `{"instance_id":"ins-a","duration":"1d","auto_renew":1}`, "", "参数 auto_renew：应为布尔值"},
		{"number string ok", `{"instance_id":"ins-a","duration":"1d","ratio":"0.25"}`, `{"duration":"1d","instance_id":"ins-a","quantity":1,"ratio":0.25}`, ""},
		{"number string range", `{"instance_id":"ins-a","duration":"1d","ratio":"2"}`, "", "参数 ratio：不能大于 1"},
		{"number string garbage", `{"instance_id":"ins-a","duration":"1d","ratio":"half"}`, "", "参数 ratio：应为数字"},
		{"number string NaN", `{"instance_id":"ins-a","duration":"1d","ratio":"NaN"}`, "", "参数 ratio：应为数字"},
		{"number bool", `{"instance_id":"ins-a","duration":"1d","ratio":true}`, "", "参数 ratio：应为数字"},
		{"integer garbage string", `{"instance_id":"ins-a","duration":"1d","quantity":"3 days"}`, "", "参数 quantity：应为整数"},
		{"enum", `{"instance_id":"ins-a","duration":"2d"}`, "", "参数 duration：只能是 1d、1w、1m 之一"},
		{"integer not whole", `{"instance_id":"ins-a","duration":"1d","quantity":1.5}`, "", "参数 quantity：应为整数"},
		{"integer whole float ok", `{"instance_id":"ins-a","duration":"1d","quantity":3.0}`, `{"duration":"1d","instance_id":"ins-a","quantity":3}`, ""},
		{"integer numeric string ok", `{"instance_id":"ins-a","duration":"1d","quantity":"4"}`, `{"duration":"1d","instance_id":"ins-a","quantity":4}`, ""},
		{"minimum", `{"instance_id":"ins-a","duration":"1d","quantity":0}`, "", "参数 quantity：不能小于 1"},
		{"maximum", `{"instance_id":"ins-a","duration":"1d","quantity":13}`, "", "参数 quantity：不能大于 12"},
		{"number range", `{"instance_id":"ins-a","duration":"1d","ratio":1.5}`, "", "参数 ratio：不能大于 1"},
		{"number ok", `{"instance_id":"ins-a","duration":"1d","ratio":0.5}`, `{"duration":"1d","instance_id":"ins-a","quantity":1,"ratio":0.5}`, ""},
		{"minLength", `{"instance_id":"ins-a","duration":"1d","name":"a"}`, "", "参数 name：长度不能少于 2 个字符"},
		{"maxLength runes", `{"instance_id":"ins-a","duration":"1d","name":"一二三四"}`, `{"duration":"1d","instance_id":"ins-a","name":"一二三四","quantity":1}`, ""},
		{"maxLength", `{"instance_id":"ins-a","duration":"1d","name":"abcde"}`, "", "参数 name：长度不能超过 4 个字符"},
		{"pattern", `{"instance_id":"INS-A","duration":"1d"}`, "", "参数 instance_id：格式不正确"},
		{"extra field", `{"instance_id":"ins-a","duration":"1d","evil":1}`, "", "参数 evil：不允许的字段"},
		{"array items", `{"instance_id":"ins-a","duration":"1d","tags":["ab","abcd"]}`, "", "参数 tags[1]：长度不能超过 3 个字符"},
		{"array maxItems", `{"instance_id":"ins-a","duration":"1d","tags":["a","b","c"]}`, "", "参数 tags：最多 2 项"},
		{"array not array", `{"instance_id":"ins-a","duration":"1d","tags":"a"}`, "", "参数 tags：应为数组"},
		{"nested env ok", `{"instance_id":"ins-a","duration":"1d","env":{"MODEL_ID":"qwen","X":"y"}}`, `{"duration":"1d","env":{"MODEL_ID":"qwen","X":"y"},"instance_id":"ins-a","quantity":1}`, ""},
		{"nested env too long", `{"instance_id":"ins-a","duration":"1d","env":{"MODEL_ID":"qwen-32b"}}`, "", "参数 env.MODEL_ID：长度不能超过 5 个字符"},
		{"nested env type", `{"instance_id":"ins-a","duration":"1d","env":{"N":3}}`, "", "参数 env.N：应为字符串"},
		{"nested env null dropped", `{"instance_id":"ins-a","duration":"1d","env":{"N":null}}`, `{"duration":"1d","env":{},"instance_id":"ins-a","quantity":1}`, ""},
		{"nested object required", `{"instance_id":"ins-a","duration":"1d","nested":{}}`, "", "参数 nested.k：必填"},
		{"nested object enum", `{"instance_id":"ins-a","duration":"1d","nested":{"k":3}}`, "", "参数 nested.k：只能是 1、2 之一"},
		{"nested object extra", `{"instance_id":"ins-a","duration":"1d","nested":{"k":1,"z":1}}`, "", "参数 nested.z：不允许的字段"},
		{"root not object", `[1]`, "", "参数：应为对象"},
		{"invalid json", `{"instance_id":`, "", "参数：不是合法的 JSON"},
		{"trailing garbage", `{"instance_id":"ins-a","duration":"1d"} {}`, "", "参数：不是合法的 JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := Validate([]byte(testSchema), []byte(c.in))
			if c.err != "" {
				if err == nil {
					t.Fatalf("want error %q, got %s", c.err, out)
				}
				if !strings.Contains(err.Error(), c.err) {
					t.Fatalf("error = %q, want contains %q", err, c.err)
				}
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("error type %T, want *ValidationError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(out) != c.want {
				t.Fatalf("out = %s, want %s", out, c.want)
			}
		})
	}
}

func TestValidateEmptyArgs(t *testing.T) {
	schema := `{"type":"object","properties":{"limit":{"type":"integer","default":10}},"additionalProperties":false}`
	for _, in := range []string{``, `  `, `null`, `{}`} {
		out, err := Validate([]byte(schema), []byte(in))
		if err != nil || string(out) != `{"limit":10}` {
			t.Fatalf("Validate(%q) = %s, %v", in, out, err)
		}
	}
	// 没有任何属性的工具：空参数规范化为 {}
	out, err := Validate([]byte(`{"type":"object","properties":{},"additionalProperties":false}`), nil)
	if err != nil || string(out) != `{}` {
		t.Fatalf("empty = %s, %v", out, err)
	}
}

func TestValidateKeepsHTMLChars(t *testing.T) {
	schema := `{"type":"object","properties":{"s":{"type":"string"}},"additionalProperties":false}`
	out, err := Validate([]byte(schema), []byte(`{"s":"<a&b>"}`))
	if err != nil || string(out) != `{"s":"<a&b>"}` {
		t.Fatalf("out = %s, %v", out, err)
	}
}

func TestCompileRejectsBadSchemas(t *testing.T) {
	bad := []string{
		`{"type":"object","properties":{"a":{"type":"strng"}}}`,                  // 未知类型
		`{"type":"object","properties":{"a":{"type":"string","pattern":"("}}}`,   // 坏正则
		`{"type":"object","properties":{"a":{"type":"string","format":"date"}}}`, // 不支持的关键字
		`{"type":"object","additionalProperties":"x"}`,
		`{"type":"object","properties":{"a":{"type":"integer","default":"x"}}}`, // default 不满足自身 schema
		`not json`,
	}
	for _, s := range bad {
		if _, err := Compile([]byte(s)); err == nil {
			t.Errorf("Compile(%s) should fail", s)
		}
	}
}
