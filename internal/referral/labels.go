package referral

import "strings"

// 导出 CSV 用的中文标签，与页面一致（web/shared/src/labels.ts 的 bindingStatusLabel / rewardStatusLabel、
// web/admin/src/logic/referral.ts 的 riskFlagLabel）。未知代码原样输出（同前端 labelOf）。
var (
	bindingStatusText = map[string]string{BindingActive: "有效", BindingHeld: "审核中", BindingRevoked: "已失效"}
	rewardStatusText  = map[string]string{StatusPending: "待发放", StatusSettled: "已发放", StatusVoid: "已作废"}
	flagText          = map[string]string{FlagSameIP: "同IP", FlagSameDevice: "同设备", FlagBurst: "爆发", FlagSameIdentity: "同身份"}
)

func textOf(m map[string]string, code string) string {
	if v, ok := m[code]; ok {
		return v
	}
	return code
}

func BindingStatusText(code string) string { return textOf(bindingStatusText, code) }
func RewardStatusText(code string) string  { return textOf(rewardStatusText, code) }

// FlagsText 风险标记的中文标签，空格分隔。
func FlagsText(flags []string) string {
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = textOf(flagText, f)
	}
	return strings.Join(out, " ")
}
