// Package guard 处理送进模型的不可信文本：净化、截断、包裹与可疑注入检测（规格 §6.1）。
// 叶子包，只依赖标准库。
package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var (
	// ANSI 转义：CSI 序列（ESC [ 参数 中间字节 终止字节）与两字节转义（ESC + @～_）。
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-Z\\-_]`)
	// 防止不可信文本提前闭合包裹标签（不区分大小写）。
	closeRe = regexp.MustCompile(`(?i)</untrusted_data`)
	// 常见注入短语（只用于记日志与后台标记，不拦截）。
	suspiciousRe = regexp.MustCompile(`(?i)ignore\s+(all\s+)?((previous|above|prior)\s+)?instructions|disregard .* instructions|you are now|system prompt|忽略(以上|之前|上面)|现在你是|你现在是|无视.*指令|立即(释放|删除)`)
)

// Sanitize 去掉 ANSI 转义与除 \n \t 外的 C0 / C1 控制字符，并把 </untrusted_data 改写为 </untrusted-data。
func Sanitize(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
	// 去控制字符之后再替换，避免 </untrusted\x00_data 这类拼接绕过
	return closeRe.ReplaceAllString(s, "</untrusted-data")
}

// Truncate 超过 limit 字节时按 rune 边界截到 ≤ limit 字节，末尾加「（已截断，原长 N 字节）」；limit ≤ 0 表示不截断。
func Truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n（已截断，原长 %d 字节）", len(s))
}

// attrSafe 只保留 [A-Za-z0-9_.:-]。
func attrSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == ':', r == '-':
			return r
		}
		return -1
	}, s)
}

// Wrap 净化并截断 text，包成 <untrusted_data source="…" k="v">\n…\n</untrusted_data>。
// source、attrs 的键与值都只保留 [A-Za-z0-9_.:-]；attrs 按键排序，净化后为空的键与名为 source 的键被丢弃。
func Wrap(source string, attrs map[string]string, text string, limit int) string {
	var b strings.Builder
	b.WriteString(`<untrusted_data source="`)
	b.WriteString(attrSafe(source))
	b.WriteByte('"')
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seen := map[string]bool{"source": true}
	for _, k := range keys {
		sk := attrSafe(k)
		if sk == "" || seen[sk] {
			continue
		}
		seen[sk] = true
		b.WriteString(" " + sk + `="` + attrSafe(attrs[k]) + `"`)
	}
	b.WriteString(">\n")
	b.WriteString(Truncate(Sanitize(text), limit))
	b.WriteString("\n</untrusted_data>")
	return b.String()
}

// JSONWrap 把 v 序列化为 JSON（不转义 HTML 字符）后 Wrap。
func JSONWrap(source string, attrs map[string]string, v any, limit int) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("guard: marshal %s: %w", attrSafe(source), err)
	}
	return Wrap(source, attrs, strings.TrimSuffix(buf.String(), "\n"), limit), nil
}

// Suspicious 判断文本是否含常见提示注入短语（不区分大小写）。
func Suspicious(s string) bool { return suspiciousRe.MatchString(s) }
