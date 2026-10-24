package comfywf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
)

// ManifestHash = sha256(canonical(graph) + "\n" + canonical(manifest) + "\n" + canonical(api_prompt))，发布时冻结写列。
// canonical = 解析成 any 再 json.Marshal（对象键排序、去空白；数字保留原文，不经 float 往返）；空 / 无法解析 → "null"。
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
	out, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return out
}

// DownloadGB 清单里全部模型的大小之和（GB，两位小数），发布时写 download_gb 列。
func DownloadGB(m Manifest) float64 {
	var sum float64
	for _, md := range m.Models {
		sum += md.SizeGB
	}
	return math.Round(sum*100) / 100
}
