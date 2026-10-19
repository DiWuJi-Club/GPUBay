package appdeploy

import (
	"sort"
	"strings"
)

type EnvKV struct{ Name, Value string }

// ShellQuote 单引号转义：' → '\”。值已由 ValidateEnv 保证不含换行 / NUL。
func ShellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

// RenderEnv 生成 env 文件：先平台保留变量（顺序固定），再用户变量；每行 export NAME='value'。
func RenderEnv(reserved, user []EnvKV) []byte {
	var b strings.Builder
	for _, kv := range reserved {
		b.WriteString("export " + kv.Name + "=" + ShellQuote(kv.Value) + "\n")
	}
	for _, kv := range user {
		b.WriteString("export " + kv.Name + "=" + ShellQuote(kv.Value) + "\n")
	}
	return []byte(b.String())
}

// sortedKV 把 map 变成按名字排序的切片（输出稳定）。
func sortedKV(m map[string]string) []EnvKV {
	out := make([]EnvKV, 0, len(m))
	for k, v := range m {
		out = append(out, EnvKV{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
