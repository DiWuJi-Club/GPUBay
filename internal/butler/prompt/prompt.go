// Package prompt 内嵌 AI 管家的系统提示（规格 §3.5）：固定前缀 system.md（可被提供方缓存）+ 每轮重算的用户快照段（D10）。
// 叶子包，只依赖标准库与 platform/tz。
package prompt

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"spark-cloud/internal/platform/tz"
)

//go:embed system.md
var system string

// Fixed 返回固定前缀（llm.Request.System）；进程内字节恒定，是提示缓存命中的前提。
func Fixed() string { return system }

// Snapshot 是系统提示里的用户快照（D10：只含手机尾号、实名状态、余额、实例数与各状态数、当前上下文实例、当前时间）。
// Partial = true 表示部分数据没取到（对应字段不展示，提示模型用工具查询）。
type Snapshot struct {
	PhoneTail    string // 手机号后 4 位
	KYC          string // users.kyc_status：none / pending / verified / rejected
	BalanceCents int64
	Running      int
	Stopped      int
	Reserved     int
	Other        int // creating / wiping / failed 等（不含 released / cancelled）
	InstanceID   string
	Now          time.Time

	UserUnknown    bool // 用户信息没取到
	BalanceUnknown bool // 余额没取到
	CountsUnknown  bool // 实例数没取到
}

var kycText = map[string]string{"none": "未实名", "pending": "实名审核中", "verified": "已实名", "rejected": "实名未通过"}

func yuan(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("¥%s%d.%02d", sign, cents/100, cents%100)
}

// Dynamic 返回用户快照段（llm.Request.SystemDynamic），固定格式、不含自由文本：
// 「用户快照（平台生成的事实数据）：手机尾号 1234，已实名；余额 ¥123.45；实例：运行中 2、已停机 1、预订 0、其它 0；当前查看：实例 ins-xxxx；现在时间：2026-10-24 15:00（Asia/Shanghai）」
func Dynamic(s Snapshot) string {
	var parts []string
	if s.UserUnknown {
		parts = append(parts, "用户信息暂时无法获取")
	} else {
		k := kycText[s.KYC]
		if k == "" {
			k = "实名状态未知"
		}
		parts = append(parts, fmt.Sprintf("手机尾号 %s，%s", s.PhoneTail, k))
	}
	if s.BalanceUnknown {
		parts = append(parts, "余额暂时无法获取（可用 get_wallet 查询）")
	} else {
		parts = append(parts, "余额 "+yuan(s.BalanceCents))
	}
	if s.CountsUnknown {
		parts = append(parts, "实例数暂时无法获取（可用 list_instances 查询）")
	} else {
		parts = append(parts, fmt.Sprintf("实例：运行中 %d、已停机 %d、预订 %d、其它 %d", s.Running, s.Stopped, s.Reserved, s.Other))
	}
	if s.InstanceID != "" {
		parts = append(parts, "当前查看：实例 "+s.InstanceID)
	}
	parts = append(parts, "现在时间："+tz.Format(s.Now)+"（Asia/Shanghai）")
	return "# 用户快照（平台生成的事实数据）\n\n" + strings.Join(parts, "；")
}

// System 返回完整系统提示（固定前缀 + 空行 + 快照段），供不区分两段的场景与测试使用。
func System(s Snapshot) string { return Fixed() + "\n\n" + Dynamic(s) }
