package referral_test

import (
	"testing"

	"spark-cloud/internal/referral"
)

// 导出 CSV 的中文标签与页面（web/shared labels.ts、后台 riskFlagLabel）一致；未知代码原样输出。
func TestExportLabels(t *testing.T) {
	for in, want := range map[string]string{"active": "有效", "held": "审核中", "revoked": "已失效", "x": "x"} {
		if got := referral.BindingStatusText(in); got != want {
			t.Errorf("binding %q = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"pending": "待发放", "settled": "已发放", "void": "已作废", "": ""} {
		if got := referral.RewardStatusText(in); got != want {
			t.Errorf("reward %q = %q, want %q", in, got, want)
		}
	}
	if got := referral.FlagsText([]string{"same_ip", "same_device", "burst", "same_identity", "x"}); got != "同IP 同设备 爆发 同身份 x" {
		t.Errorf("flags = %q", got)
	}
	if got := referral.FlagsText(nil); got != "" {
		t.Errorf("no flags = %q", got)
	}
}
