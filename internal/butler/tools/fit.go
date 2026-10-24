package tools

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"spark-cloud/internal/butler/guard"
)

// 结果超出大小上限时「降级字段，不截字节」：始终输出合法 JSON。
// 顺序：先把最长的长文本减半，再把最长的数组减半（所在对象加 <key>_truncated: true），最后才把短文本减半；
// 实在放不下（字段极多）时输出固定的 {"truncated": true, "message": …}。

const (
	degradeLongString  = 256 // 先处理的「长文本」阈值（字节）
	degradeShortString = 16  // 文本最短减到这么长
	degradeMaxRounds   = 2000
	truncMark          = "…（已截断）"
)

// fitJSON 包裹 v；超过 limit 时按上面的顺序降级。
func fitJSON(name string, c Caller, v any, limit int) (string, error) {
	s, err := guard.JSONWrap(name, userAttrs(c), v, 0)
	if err != nil || len(s) <= limit {
		return s, err
	}
	// 转成通用 JSON 值再降级（不改调用方的数据）
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	g, err := decodeValue(raw)
	if err != nil {
		return "", err
	}
	for range degradeMaxRounds {
		if !shrinkOnce(&g) {
			break
		}
		if s, err = guard.JSONWrap(name, userAttrs(c), g, 0); err != nil {
			return "", err
		}
		if len(s) <= limit {
			return s, nil
		}
	}
	return guard.JSONWrap(name, userAttrs(c), map[string]any{"truncated": true, "message": "结果过大，已省略；请缩小查询范围"}, 0)
}

type shrinkCand struct {
	size  int
	apply func()
}

// shrinkOnce 做一步降级；没有可降级的字段时返回 false。
func shrinkOnce(root *any) bool {
	var longStr, shortStr, arrays []shrinkCand
	var walk func(v any, set func(any), mark func())
	walk = func(v any, set func(any), mark func()) {
		switch x := v.(type) {
		case string:
			if h := halveString(x); len(x) > degradeShortString && len(h) < len(x) {
				c := shrinkCand{size: len(x), apply: func() { set(h) }}
				if len(x) > degradeLongString {
					longStr = append(longStr, c)
				} else {
					shortStr = append(shortStr, c)
				}
			}
		case []any:
			if len(x) > 1 {
				b, _ := json.Marshal(x)
				arrays = append(arrays, shrinkCand{size: len(b), apply: func() {
					set(x[:len(x)/2])
					if mark != nil {
						mark()
					}
				}})
			}
			for i := range x {
				walk(x[i], func(nv any) { x[i] = nv }, nil)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(x[k], func(nv any) { x[k] = nv }, func() { x[k+"_truncated"] = true })
			}
		}
	}
	walk(*root, func(nv any) { *root = nv }, nil)
	for _, group := range [][]shrinkCand{longStr, arrays, shortStr} {
		if len(group) == 0 {
			continue
		}
		best := group[0]
		for _, c := range group[1:] {
			if c.size > best.size {
				best = c
			}
		}
		best.apply()
		return true
	}
	return false
}

// halveString 保留前一半（rune 边界）并加截断标记；已带标记的不重复加。
func halveString(s string) string {
	base := strings.TrimSuffix(s, truncMark)
	cut := len(base) / 2
	for cut > 0 && !utf8.RuneStart(base[cut]) {
		cut--
	}
	return base[:cut] + truncMark
}
