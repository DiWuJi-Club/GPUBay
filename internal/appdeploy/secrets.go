package appdeploy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"spark-cloud/internal/platform/secretbox"
)

// sealSecrets 把敏感变量（JSON）用 secretbox 加密；空 map → nil（列为 NULL）。
func sealSecrets(box *secretbox.Box, m map[string]string) []byte {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	return box.Seal(b)
}

// openSecrets 解密 secret_env_enc；nil / 空 → 空 map。
func openSecrets(box *secretbox.Box, enc []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(enc) == 0 {
		return out, nil
	}
	plain, err := box.Open(enc)
	if err != nil {
		return nil, fmt.Errorf("appdeploy: 解密敏感变量: %w", err)
	}
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, fmt.Errorf("appdeploy: 敏感变量格式: %w", err)
	}
	return out, nil
}

// Redact 把 s 里出现的全部 secret 值替换为 ***（规格 §8 / D9）。长度 < MinSecretLen 的值不替换（校验层已拒绝这种 secret）；
// 按长度降序替换：短值是长值前缀时，先替换短值会在长值处留下残片。
func Redact(s string, secrets map[string]string) string {
	vals := make([]string, 0, len(secrets))
	for _, v := range secrets {
		if len(v) >= MinSecretLen {
			vals = append(vals, v)
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}
