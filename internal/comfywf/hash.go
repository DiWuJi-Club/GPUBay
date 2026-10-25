package comfywf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// ManifestHash = sha256(canonical(graph) + "\n" + canonical(manifest) + "\n" + canonical(api_prompt))，发布时冻结写列。
// canonical = 解析成 any 再 json.Marshal（对象键排序、去空白），数字按 normNumber 规范化（graph / api_prompt 存 JSONB，
// 往返会改写数字原文：1e-7 → 0.0000001、1E3 → 1000、1.0 → 1，哈希必须不变）；空 / 无法解析 → "null"。
func ManifestHash(graph, apiPrompt json.RawMessage, m Manifest) string {
	mb, err := json.Marshal(m)
	if err != nil { // Manifest 的字段都可序列化；保险起见
		mb = nil
	}
	h := sha256.New()
	h.Write(canonical(graph))
	h.Write([]byte("\n"))
	h.Write(canonical(mb))
	h.Write([]byte("\n"))
	h.Write(canonical(apiPrompt))
	return hex.EncodeToString(h.Sum(nil))
}

func canonical(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("null")
	}
	var v any
	if err := decodeNumber(raw, &v); err != nil {
		return []byte("null")
	}
	out, err := json.Marshal(normNumbers(v))
	if err != nil {
		return []byte("null")
	}
	return out
}

func normNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normNumbers(x)
		}
	case []any:
		for i, x := range t {
			t[i] = normNumbers(x)
		}
	case json.Number:
		return normNumber(t)
	}
	return v
}

// normNumber 数值相等的数字原文得到同一结果：整数值（含 1.0、1E3、超出 int64 的大整数）→ 十进制整数；其它 → float64。
func normNumber(n json.Number) any {
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	// 指数过大时不做精确展开（避免 1e999999 这类输入耗尽内存）；JSONB 也不接受这么大的数。
	if e := strings.IndexAny(s, "eE"); e >= 0 {
		if exp, err := strconv.Atoi(s[e+1:]); err != nil || exp > 400 || exp < -400 {
			if f, err := n.Float64(); err == nil {
				return f
			}
			return json.Number(s)
		}
	}
	if r, ok := new(big.Rat).SetString(s); ok && r.IsInt() {
		return json.Number(r.Num().String())
	}
	if f, err := n.Float64(); err == nil {
		return f
	}
	return json.Number(s)
}

// DownloadGB 清单里全部模型的大小之和（GB，两位小数），发布时写 download_gb 列。
func DownloadGB(m Manifest) float64 {
	var sum float64
	for _, md := range m.Models {
		sum += md.SizeGB
	}
	return math.Round(sum*100) / 100
}
