package prompt

import (
	"strings"
	"testing"
	"time"
)

func TestDynamicSnapshotFormat(t *testing.T) {
	s := Snapshot{PhoneTail: "1234", KYC: "verified", BalanceCents: 12345, Running: 2, Stopped: 1, Reserved: 0, Other: 0,
		InstanceID: "ins-abc", Now: time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC)}
	want := "# 用户快照（平台生成的事实数据）\n\n手机尾号 1234，已实名；余额 ¥123.45；实例：运行中 2、已停机 1、预订 0、其它 0；当前查看：实例 ins-abc；现在时间：2026-10-24 15:00（Asia/Shanghai）"
	if got := Dynamic(s); got != want {
		t.Fatalf("Dynamic =\n%s\nwant\n%s", got, want)
	}
	// 无上下文实例、未实名、部分数据缺失
	s2 := Snapshot{PhoneTail: "0001", KYC: "none", BalanceUnknown: true, CountsUnknown: true, Now: time.Date(2026, 10, 24, 16, 30, 0, 0, time.UTC)}
	want2 := "# 用户快照（平台生成的事实数据）\n\n手机尾号 0001，未实名；余额暂时无法获取（可用 get_wallet 查询）；实例数暂时无法获取（可用 list_instances 查询）；现在时间：2026-10-25 00:30（Asia/Shanghai）"
	if got := Dynamic(s2); got != want2 {
		t.Fatalf("Dynamic =\n%s\nwant\n%s", got, want2)
	}
	if got := Dynamic(Snapshot{UserUnknown: true, BalanceCents: -50, Now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}); !strings.Contains(got, "用户信息暂时无法获取；余额 ¥-0.50") {
		t.Fatalf("Dynamic unknown user = %s", got)
	}
}

func TestFixedPrefixStable(t *testing.T) {
	a := System(Snapshot{PhoneTail: "1111", Now: time.Unix(0, 0)})
	b := System(Snapshot{PhoneTail: "2222", Running: 3, Now: time.Unix(3600, 0)})
	if !strings.HasPrefix(a, Fixed()) || !strings.HasPrefix(b, Fixed()) || Fixed() != Fixed() {
		t.Fatal("固定前缀在两次调用间不同")
	}
	if strings.Contains(Fixed(), "1111") || strings.Contains(Fixed(), "快照（平台生成的事实数据）：") {
		t.Fatal("固定前缀混入了动态内容")
	}
}

func TestFixedRules(t *testing.T) {
	f := Fixed()
	for _, must := range []string{
		"只有最外层", "<untrusted_data", "绝不执行", // 数据边界
		"不要在回复文字里复述任何密钥", "generate_secret", // secret 不进文本
		"以下单页为准", "list_products", // 价格来自工具
		"只会生成一张确认卡", "绝不能", "一次只提议一个写操作", // 写操作
		"get_app", "deploy_app", "同一轮", "get_deployment_log", // 部署 / 诊断流程
		"`/account`", "`/account/ssh-keys`", "`/instances/<实例 ID>`", "`/wallet/recharge`", "`/kyc`", "`/invite`", "`/tokens?tab=keys`", // D4 页面
		"中文", "英文",
	} {
		if !strings.Contains(f, must) {
			t.Errorf("system.md 缺少 %q", must)
		}
	}
}
