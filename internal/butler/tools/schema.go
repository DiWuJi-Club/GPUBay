package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 进程内极简 JSON Schema 校验器（规格 §4）：只支持工具参数用得到的子集，不引入依赖。
// 支持：type ∈ object|string|integer|number|boolean|array、properties、required、additionalProperties（false 或 schema）、
// enum、minimum / maximum、minLength / maxLength（按字符计）、pattern（RE2）、items（单 schema）、minItems / maxItems、default。
// 规范化：对象里值为 null 的字段视为缺失（去掉），缺失字段按 default 填入；integer 接受整数值的小数（3.0）与数字字符串（"3"），
// 统一输出为整数；number 接受数字字符串（"0.5"）；boolean 接受 "true" / "false"。未知关键字在编译时报错（工具 schema 写错立即暴露，而不是静默放行）。

// ValidationError 是参数校验失败；Error() 即回填给模型的文案「参数 <path>：<原因>」。
type ValidationError struct {
	Path   string // 如 env.MODEL_ID、tags[1]；根为空
	Reason string
}

func (e *ValidationError) Error() string {
	if e.Path == "" {
		return "参数：" + e.Reason
	}
	return "参数 " + e.Path + "：" + e.Reason
}

// Schema 是编译后的 schema 节点。
type Schema struct {
	Type       string
	Properties map[string]*Schema
	Required   []string
	// Additional 非 nil 时额外字段按它校验；Closed 为 true 时额外字段报错；两者都没有时额外字段原样保留。
	Additional *Schema
	Closed     bool
	Enum       []any
	Minimum    *float64
	Maximum    *float64
	MinLength  *int
	MaxLength  *int
	Pattern    *regexp.Regexp
	Items      *Schema
	MinItems   *int
	MaxItems   *int
	Default    any
	HasDefault bool
}

var knownKeywords = map[string]bool{
	"type": true, "description": true, "title": true, "properties": true, "required": true, "additionalProperties": true,
	"enum": true, "minimum": true, "maximum": true, "minLength": true, "maxLength": true, "pattern": true,
	"items": true, "minItems": true, "maxItems": true, "default": true,
}

var knownTypes = map[string]bool{"": true, "object": true, "string": true, "integer": true, "number": true, "boolean": true, "array": true}

// Compile 解析 schema；不支持的关键字、未知类型、坏正则、default 不满足自身约束都报错。
func Compile(raw json.RawMessage) (*Schema, error) {
	return compile(raw, "")
}

func compile(raw json.RawMessage, path string) (*Schema, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("schema %s: %w", path, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !knownKeywords[k] {
			return nil, fmt.Errorf("schema %s: unsupported keyword %q", path, k)
		}
	}
	s := &Schema{}
	dec := func(k string, v any) error {
		if r, ok := m[k]; ok {
			if err := json.Unmarshal(r, v); err != nil {
				return fmt.Errorf("schema %s.%s: %w", path, k, err)
			}
		}
		return nil
	}
	if err := dec("type", &s.Type); err != nil {
		return nil, err
	}
	if !knownTypes[s.Type] {
		return nil, fmt.Errorf("schema %s: unsupported type %q", path, s.Type)
	}
	if err := dec("required", &s.Required); err != nil {
		return nil, err
	}
	for _, k := range []struct {
		name string
		dst  **float64
	}{{"minimum", &s.Minimum}, {"maximum", &s.Maximum}} {
		if err := dec(k.name, k.dst); err != nil {
			return nil, err
		}
	}
	for _, k := range []struct {
		name string
		dst  **int
	}{{"minLength", &s.MinLength}, {"maxLength", &s.MaxLength}, {"minItems", &s.MinItems}, {"maxItems", &s.MaxItems}} {
		if err := dec(k.name, k.dst); err != nil {
			return nil, err
		}
	}
	if r, ok := m["pattern"]; ok {
		var p string
		if err := json.Unmarshal(r, &p); err != nil {
			return nil, fmt.Errorf("schema %s.pattern: %w", path, err)
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("schema %s.pattern: %w", path, err)
		}
		s.Pattern = re
	}
	if r, ok := m["enum"]; ok {
		v, err := decodeValue(r)
		if err != nil {
			return nil, fmt.Errorf("schema %s.enum: %w", path, err)
		}
		arr, ok := v.([]any)
		if !ok || len(arr) == 0 {
			return nil, fmt.Errorf("schema %s.enum: must be a non-empty array", path)
		}
		s.Enum = arr
	}
	if r, ok := m["properties"]; ok {
		var props map[string]json.RawMessage
		if err := json.Unmarshal(r, &props); err != nil {
			return nil, fmt.Errorf("schema %s.properties: %w", path, err)
		}
		s.Properties = map[string]*Schema{}
		for name, pr := range props {
			child, err := compile(pr, joinPath(path, name))
			if err != nil {
				return nil, err
			}
			s.Properties[name] = child
		}
	}
	if r, ok := m["additionalProperties"]; ok {
		var b bool
		if err := json.Unmarshal(r, &b); err == nil {
			s.Closed = !b
		} else {
			if bytes.TrimSpace(r)[0] != '{' {
				return nil, fmt.Errorf("schema %s.additionalProperties: must be boolean or schema", path)
			}
			child, err := compile(r, joinPath(path, "*"))
			if err != nil {
				return nil, err
			}
			s.Additional = child
		}
	}
	if r, ok := m["items"]; ok {
		child, err := compile(r, path+"[]")
		if err != nil {
			return nil, err
		}
		s.Items = child
	}
	if r, ok := m["default"]; ok {
		v, err := decodeValue(r)
		if err != nil {
			return nil, fmt.Errorf("schema %s.default: %w", path, err)
		}
		nv, err := s.validate(v, path)
		if err != nil {
			return nil, fmt.Errorf("schema %s.default: %w", path, err)
		}
		s.Default, s.HasDefault = nv, true
	}
	return s, nil
}

// Validate 编译 schema 并校验 value，返回规范化后的 JSON（键按字母序，不转义 HTML 字符）。
func Validate(schema, value json.RawMessage) (json.RawMessage, error) {
	s, err := Compile(schema)
	if err != nil {
		return nil, err
	}
	return s.Validate(value)
}

// Validate 校验并规范化 value。空白或 null 的参数在根为 object 时视为 {}（模型对无参工具常发空参数）。
func (s *Schema) Validate(value json.RawMessage) (json.RawMessage, error) {
	var v any
	if len(bytes.TrimSpace(value)) > 0 {
		var err error
		if v, err = decodeValue(value); err != nil {
			return nil, &ValidationError{Reason: "不是合法的 JSON"}
		}
	}
	if v == nil && s.Type == "object" {
		v = map[string]any{}
	}
	out, err := s.validate(v, "")
	if err != nil {
		return nil, err
	}
	return encodeValue(out)
}

func (s *Schema) validate(v any, path string) (any, error) {
	if v == nil {
		return nil, &ValidationError{Path: path, Reason: "不能为 null"}
	}
	var out any
	switch s.Type {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return nil, &ValidationError{Path: path, Reason: "应为对象"}
		}
		res := map[string]any{}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val := m[k]
			if val == nil { // 去 null：视为缺失
				continue
			}
			child := s.Properties[k]
			if child == nil {
				switch {
				case s.Additional != nil:
					child = s.Additional
				case s.Closed:
					return nil, &ValidationError{Path: joinPath(path, k), Reason: "不允许的字段"}
				default:
					res[k] = val
					continue
				}
			}
			nv, err := child.validate(val, joinPath(path, k))
			if err != nil {
				return nil, err
			}
			res[k] = nv
		}
		for k, child := range s.Properties {
			if _, ok := res[k]; !ok && child.HasDefault {
				res[k] = cloneValue(child.Default)
			}
		}
		req := append([]string(nil), s.Required...)
		sort.Strings(req)
		for _, k := range req {
			if _, ok := res[k]; !ok {
				return nil, &ValidationError{Path: joinPath(path, k), Reason: "必填"}
			}
		}
		out = res
	case "string":
		str, ok := v.(string)
		if !ok {
			return nil, &ValidationError{Path: path, Reason: "应为字符串"}
		}
		n := utf8.RuneCountInString(str)
		if s.MinLength != nil && n < *s.MinLength {
			return nil, &ValidationError{Path: path, Reason: fmt.Sprintf("长度不能少于 %d 个字符", *s.MinLength)}
		}
		if s.MaxLength != nil && n > *s.MaxLength {
			return nil, &ValidationError{Path: path, Reason: fmt.Sprintf("长度不能超过 %d 个字符", *s.MaxLength)}
		}
		if s.Pattern != nil && !s.Pattern.MatchString(str) {
			return nil, &ValidationError{Path: path, Reason: "格式不正确"}
		}
		out = str
	case "integer":
		n, ok := toInteger(v)
		if !ok {
			return nil, &ValidationError{Path: path, Reason: "应为整数"}
		}
		if err := s.checkRange(float64(n), path); err != nil {
			return nil, err
		}
		out = n
	case "number":
		f, ok := toNumberArg(v)
		if !ok {
			return nil, &ValidationError{Path: path, Reason: "应为数字"}
		}
		if err := s.checkRange(f, path); err != nil {
			return nil, err
		}
		out = f
	case "boolean":
		b, ok := toBool(v)
		if !ok {
			return nil, &ValidationError{Path: path, Reason: "应为布尔值"}
		}
		out = b
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return nil, &ValidationError{Path: path, Reason: "应为数组"}
		}
		if s.MinItems != nil && len(arr) < *s.MinItems {
			return nil, &ValidationError{Path: path, Reason: fmt.Sprintf("至少 %d 项", *s.MinItems)}
		}
		if s.MaxItems != nil && len(arr) > *s.MaxItems {
			return nil, &ValidationError{Path: path, Reason: fmt.Sprintf("最多 %d 项", *s.MaxItems)}
		}
		res := make([]any, 0, len(arr))
		for i, it := range arr {
			ip := fmt.Sprintf("%s[%d]", path, i)
			if s.Items == nil {
				res = append(res, it)
				continue
			}
			nv, err := s.Items.validate(it, ip)
			if err != nil {
				return nil, err
			}
			res = append(res, nv)
		}
		out = res
	default: // 未声明类型：原样接受
		out = v
	}
	if len(s.Enum) > 0 && !enumHas(s.Enum, out) {
		parts := make([]string, 0, len(s.Enum))
		for _, e := range s.Enum {
			parts = append(parts, fmt.Sprint(e))
		}
		return nil, &ValidationError{Path: path, Reason: "只能是 " + strings.Join(parts, "、") + " 之一"}
	}
	return out, nil
}

func (s *Schema) checkRange(f float64, path string) error {
	if s.Minimum != nil && f < *s.Minimum {
		return &ValidationError{Path: path, Reason: "不能小于 " + fmtNum(*s.Minimum)}
	}
	if s.Maximum != nil && f > *s.Maximum {
		return &ValidationError{Path: path, Reason: "不能大于 " + fmtNum(*s.Maximum)}
	}
	return nil
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// toInteger：JSON 整数、整数值的小数（3.0）、十进制整数字符串（"3"，小模型常把数字写成字符串）。
func toInteger(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		f, err := x.Float64()
		if err != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
			return 0, false
		}
		return int64(f), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	case int64:
		return x, true
	}
	return 0, false
}

// toNumberArg：JSON 数字或十进制数字字符串（"0.5"；与 integer 同样宽松）；NaN / Inf 不接受。
func toNumberArg(v any) (float64, bool) {
	if str, ok := v.(string); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	return toNumber(v)
}

// toBool：JSON 布尔或字符串 "true" / "false"（小模型常把布尔写成字符串）。
func toBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.TrimSpace(x) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

func toNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// enumHas 比较规范化后的值：数字按数值比较，其余按 JSON 相等。
func enumHas(enum []any, v any) bool {
	for _, e := range enum {
		if ef, ok := toNumber(e); ok {
			if vf, ok := toNumber(v); ok && ef == vf {
				return true
			}
			continue
		}
		if a, err := encodeValue(e); err == nil {
			if b, err := encodeValue(v); err == nil && bytes.Equal(a, b) {
				return true
			}
		}
	}
	return false
}

// decodeValue 解析单个 JSON 值（数字保留为 json.Number）；尾部多余内容报错。
func decodeValue(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

func encodeValue(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// cloneValue 深拷贝 default（object / array 默认值不能在多次调用间共享）。
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = cloneValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = cloneValue(val)
		}
		return out
	}
	return v
}
