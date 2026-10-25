package help_test

import (
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"

	"spark-cloud/internal/butler/help"
	"spark-cloud/internal/llm"
)

func TestSections(t *testing.T) {
	want := []string{"ssh", "ssh-keys", "forward", "web", "apps", "workflows", "booking", "batch", "auto-renew", "retention", "tokens", "referral", "billing", "butler"}
	got := help.Sections()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Sections = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if help.Sections()[0] != "ssh" {
		t.Fatal("Sections must return a copy")
	}
	for _, s := range want {
		body, ok := help.Section(s)
		if !ok || !strings.HasPrefix(body, "# ") || len(body) < 40 {
			t.Errorf("Section(%q) = %q, %v", s, body, ok)
		}
	}
	for _, bad := range []string{"", "nope", "../help", "ssh.md", "SSH"} {
		if _, ok := help.Section(bad); ok {
			t.Errorf("Section(%q) should not exist", bad)
		}
	}
}

// 目录里的 .md 文件与章节表一一对应（新增文件必须登记，否则 get_help 读不到）。
func TestFilesMatchSections(t *testing.T) {
	entries, err := fs.Glob(os.DirFS("."), "*.md")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e, ".md"))
	}
	want := help.Sections()
	sort.Strings(names)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("md files %v != sections %v", names, want)
	}
}

// 抄录检查：帮助页的几处关键文案在语料里（帮助页改了文案时提醒同步）。
func TestCorpusMatchesHelpView(t *testing.T) {
	vue, err := os.ReadFile("../../../web/console/src/views/HelpView.vue")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		"ssh":        "点击「显示」查看 SSH 密码并复制",
		"apps":       "模型缓存在",
		"workflows":  "移除工作流只删工作流文件",
		"booking":    "可提前 2 小时～60 天预订",
		"batch":      "一单最多 8 台",
		"auto-renew": "到期前 1 小时自动扣款续 1 个单位",
		"retention":  "到期停机后数据保留 6 小时",
		"tokens":     "累计到 ¥1 或最早一笔满 10 分钟时从钱包余额扣款一次",
		"referral":   "好友退款时按比例扣回已发放的返利",
		"billing":    "预付包时长：1 天 / 1 周 / 1 月",
	}
	for sec, phrase := range checks {
		body, _ := help.Section(sec)
		if !strings.Contains(body, phrase) {
			t.Errorf("section %s missing %q", sec, phrase)
		}
		if !strings.Contains(string(vue), phrase) {
			t.Errorf("HelpView.vue no longer contains %q; resync help/%s.md", phrase, sec)
		}
	}
	// 邀请返利单独成节（与帮助页一致），不在计费规则里重复
	if body, _ := help.Section("billing"); strings.Contains(body, "返利") {
		t.Error("billing.md 不应再包含邀请返利")
	}
	// Token 商城一节的限额是帮助页里的变量（DEFAULT_LLM_LIMITS）：语料写死默认值，与 llm.DefaultRPM / TPM / Concurrency 一致
	limits := fmt.Sprintf("每分钟 %d 次请求、每分钟 %dK token、同时 %d 个并发请求", llm.DefaultRPM, llm.DefaultTPM/1000, llm.DefaultConcurrency)
	if body, _ := help.Section("tokens"); !strings.Contains(body, limits) {
		t.Errorf("tokens.md 的默认限额与 llm 包不一致，want %q", limits)
	}
	body, _ := help.Section("butler")
	for _, p := range []string{"确认卡", "90 天", "每日额度", "以下单页为准"} {
		if !strings.Contains(body, p) {
			t.Errorf("butler.md missing %q", p)
		}
	}
}
