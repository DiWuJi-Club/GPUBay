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
	"unicode"
	"unicode/utf8"
)

var (
	// 终端转义：OSC（ESC ] … BEL 或 ESC \）、CSI（ESC [ 参数 中间字节 终止字节）、
	// C1 形式的 CSI（U+009B 参数 …）、两字节转义（ESC + @～_）。顺序有意义：先长后短。
	ansiRe = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-9;?]*[ -/]*[@-~]|\x{9b}[0-9;?]*[ -/]*[@-~]|\x1b[@-Z\\-_]`)
	// 防止不可信文本提前闭合包裹标签：不区分大小写，容忍 < / 之间与 untrusted、data 之间的空白和下划线。
	// 替换结果 </untrusted-data 不再匹配，所以 Sanitize 幂等。
	closeRe = regexp.MustCompile(`(?i)<\s*/\s*untrusted[\s_]*data`)
	// 常见注入短语（只用于记日志与后台标记，不拦截）。
	suspiciousRe = regexp.MustCompile(`(?i)ignore\s+(all\s+)?((previous|above|prior)\s+)?instructions|disregard .* instructions|you are now|system prompt|忽略(以上|之前|上面)|现在你是|你现在是|无视.*指令|立即(释放|删除)`)
)

// Sanitize 净化不可信文本：
//   - 去掉终端转义（OSC / CSI / C1 CSI / 两字节转义）；
//   - 换行统一为 \n（CRLF、单独的 \r、U+2028 / U+2029 都变成 \n）；
//   - 去掉除 \n \t 外的 C0 / C1 控制字符与 DEL，去掉 Unicode 格式字符（Cf，如零宽空格、方向控制符；保留 U+200D 以免破坏 emoji）；
//   - 最后把 </untrusted_data 的各种变体改写为 </untrusted-data（在去字符之后做，防止插入控制 / 零宽字符绕过）。
//
// 结果幂等：Sanitize(Sanitize(s)) == Sanitize(s)。
func Sanitize(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\r' || r == ' ' || r == ' ':
			return '\n'
		case r == '\n' || r == '\t' || r == '‍':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	return closeRe.ReplaceAllString(s, "</untrusted-data")
}

// Truncate 超过 limit 字节时按 rune 边界截到 ≤ limit 字节，再追加「\n（已截断，原长 N 字节）」，
// 即输出最多是 limit 字节 + 标记；N 是传入 s 的字节数（经 Wrap 调用时即净化后的长度）。limit ≤ 0 表示不截断。
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

// JSONWrap 把 v 序列化为 JSON 后 Wrap：先对每个字符串叶子（含对象键）做 Sanitize，再重新编码（不转义 HTML 字符、数字原样保留）。
//
// limit 只是最后一道保险：JSON 超过 limit 时 Truncate 会切断 JSON 使其不再合法。
// 调用方应当在字段内部先截断（例如日志字段用 Truncate 截到上限），保证整体 JSON 不超过 limit。
func JSONWrap(source string, attrs map[string]string, v any, limit int) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("guard: marshal %s: %w", attrSafe(source), err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return "", fmt.Errorf("guard: decode %s: %w", attrSafe(source), err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(sanitizeTree(tree)); err != nil {
		return "", fmt.Errorf("guard: marshal %s: %w", attrSafe(source), err)
	}
	return Wrap(source, attrs, strings.TrimSuffix(buf.String(), "\n"), limit), nil
}

// sanitizeTree 对解码后的 JSON 树里所有字符串（值与对象键）做 Sanitize。
func sanitizeTree(x any) any {
	switch t := x.(type) {
	case string:
		return Sanitize(t)
	case []any:
		for i := range t {
			t[i] = sanitizeTree(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[Sanitize(k)] = sanitizeTree(v)
		}
		return out
	}
	return x // json.Number、bool、nil
}

// Suspicious 判断文本是否含常见提示注入短语（不区分大小写）。应对 Sanitize 之后的文本调用，
// 否则插入的控制 / 零宽字符可以绕过匹配。
func Suspicious(s string) bool { return suspiciousRe.MatchString(s) }
