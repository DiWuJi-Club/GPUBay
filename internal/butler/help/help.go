// Package help 内嵌 AI 管家的帮助语料（get_help 工具的数据源）。内容抄录自控制台帮助页 HelpView.vue，
// butler.md 与帮助页「AI 管家」一节同文；两边手工保持一致。叶子包，只依赖标准库。
package help

import (
	"embed"
	"strings"
)

//go:embed *.md
var files embed.FS

// sections 是章节名（文件名去掉 .md），顺序与帮助页一致。
var sections = []string{"ssh", "ssh-keys", "forward", "web", "apps", "booking", "batch", "auto-renew", "retention", "billing", "butler"}

// Section 返回章节 Markdown 原文；不存在的章节返回 ok = false。
func Section(name string) (string, bool) {
	for _, s := range sections {
		if s == name {
			b, err := files.ReadFile(name + ".md")
			if err != nil {
				return "", false
			}
			return strings.TrimSpace(string(b)), true
		}
	}
	return "", false
}

// Sections 返回全部章节名（副本，顺序与帮助页一致）。
func Sections() []string { return append([]string(nil), sections...) }
