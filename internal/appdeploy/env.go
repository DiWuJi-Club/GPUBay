package appdeploy

import (
	"fmt"
	"sort"
	"strings"
)

type EnvKV struct{ Name, Value string }

// ShellQuote 单引号转义：' → '\”。值已由 ValidateEnv 保证不含换行 / NUL。
func ShellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

// RenderEnv 生成 env 文件：先平台保留变量（顺序固定），再用户变量；每行 export NAME='value'。
// 纵深防御：env 会被 sh 加载，名字不合规的条目（保留区不符合 envNameRe；用户区不符合 validUserEnvName）不写出；
// Bundle 在调用前用 checkUserEnvNames 报错，这里只保证任何调用方都拿不到可注入的行。
func RenderEnv(reserved, user []EnvKV) []byte {
	var b strings.Builder
	for _, kv := range reserved {
		if envNameRe.MatchString(kv.Name) {
			b.WriteString("export " + kv.Name + "=" + ShellQuote(kv.Value) + "\n")
		}
	}
	for _, kv := range user {
		if validUserEnvName(kv.Name) {
			b.WriteString("export " + kv.Name + "=" + ShellQuote(kv.Value) + "\n")
		}
	}
	return []byte(b.String())
}

// checkUserEnvNames 用户变量名不合规时报错（Bundle 的纵深防御；正常路径上 ValidateSpec / ValidateEnv 已保证）。
func checkUserEnvNames(kvs []EnvKV) error {
	for _, kv := range kvs {
		if !validUserEnvName(kv.Name) {
			return fmt.Errorf("appdeploy: 变量名 %q 不合法", kv.Name)
		}
	}
	return nil
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
