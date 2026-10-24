// Package imageref 统一镜像引用的字符串形态：Agent 上报已缓存镜像（docker image ls）、
// 网关写 machines.cached_images、覆盖率统计比对 images.ref 都用这里的规则，三处才对得上。
package imageref

import (
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	MaxItems = 200 // 每台机器最多记录的镜像数
	MaxLen   = 256 // 每项最长字节数（与 images.ref 的长度上限一致）
)

// Familiar 返回 docker image ls 打印的「短名」形态（{{.Repository}}:{{.Tag}}）：
// Docker Hub 的 docker.io/、index.docker.io/ 前缀和官方镜像的 library/ 折叠掉，省略 tag 时补 :latest。
// 带 @sha256 摘要的引用原样返回（docker image ls 不打印摘要，永远对不上，覆盖率里算未缓存）。
func Familiar(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.Contains(ref, "@") {
		return ref
	}
	for _, p := range []string{"docker.io/", "index.docker.io/"} {
		if rest, ok := strings.CutPrefix(ref, p); ok {
			ref = rest
			break
		}
	}
	if rest, ok := strings.CutPrefix(ref, "library/"); ok && !strings.Contains(rest, "/") {
		ref = rest
	}
	if !strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":") {
		ref += ":latest"
	}
	return ref
}

// Key 是 cached_images 里存、覆盖率统计拿 images.ref 去比的形态：Familiar 后截到 MaxLen 字节（不切开多字节字符）。
// 两边都经过 Key，超长的引用也按同一规则截断，仍然对得上。
func Key(ref string) string { return truncate(Familiar(ref)) }

// Normalize 把 docker image ls 的输出行（或 Agent 上报的列表）整理成 cached_images：
// 去掉空行、含 <none> 的项与非法 UTF-8 的项（text[] 只收合法 UTF-8），逐项取 Key、去重、排序、截到 MaxItems 项。
// 结果总是非 nil（text[] 列 NOT NULL）。
func Normalize(lines []string) []string {
	seen := map[string]bool{}
	list := []string{}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.Contains(l, "<none>") || !utf8.ValidString(l) {
			continue
		}
		l = Key(l)
		if seen[l] {
			continue
		}
		seen[l] = true
		list = append(list, l)
	}
	sort.Strings(list)
	if len(list) > MaxItems {
		list = list[:MaxItems]
	}
	return list
}

func truncate(s string) string {
	if len(s) <= MaxLen {
		return s
	}
	cut := MaxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
