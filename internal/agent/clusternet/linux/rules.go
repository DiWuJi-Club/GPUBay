package linux

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"spark-cloud/internal/agent/netfilter/iptables"
	"spark-cloud/internal/clusterspec"
)

// ChainClu 是 routed 模式的转发白名单链（DOCKER-USER 的 -i sch+ / -o sch+ 跳转指向它）。
const ChainClu = iptables.ChainClu

// ExpectedCluRules 是 `iptables -S SPARK-CLU` 的期望输出（含 -N 行）：按实例 ID、对端 rank 排序；
// 每条规则按 iptables -S 的打印顺序 -s -d -i -o … -j 书写（221 真机 iptables v1.8.10 (nf_tables) 实测），
// 否则每 60 秒都会判漂移重建。只收 routed 模式的 Spec。
func ExpectedCluRules(routed map[string]clusterspec.Spec) []string {
	rules := []string{"-N " + ChainClu, "-A " + ChainClu + " -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT"}
	for _, id := range sortedKeys(routed) {
		s := routed[id]
		if s.Mode != clusterspec.ModeRouted {
			continue
		}
		self, host := s.Self().IP, clusterspec.HostVeth(id)
		for _, p := range s.Peers() {
			rules = append(rules,
				fmt.Sprintf("-A %s -s %s/32 -d %s/32 -i %s -o %s -j ACCEPT", ChainClu, self, p.IP, host, s.Iface),
				fmt.Sprintf("-A %s -s %s/32 -d %s/32 -i %s -o %s -j ACCEPT", ChainClu, p.IP, self, s.Iface, host))
		}
	}
	return append(rules, "-A "+ChainClu+" -j DROP")
}

// validRouted 去掉校验不过或不是 routed 的 Spec（坏 Spec 会让整个 restore 事务失败，连带别的实例的规则）。
func (n *Net) validRouted(routed map[string]clusterspec.Spec) map[string]clusterspec.Spec {
	out := make(map[string]clusterspec.Spec, len(routed))
	for id, s := range routed {
		if s.Mode != clusterspec.ModeRouted {
			continue
		}
		if err := s.Validate(); err != nil {
			n.opt.Log.Warn("cluster spec skipped in SPARK-CLU", "instance", id, "err", err)
			continue
		}
		out[id] = s
	}
	return out
}

// SyncRules：`iptables -S SPARK-CLU` 与 ExpectedCluRules 比对，相同不写；不同（含链不存在）用一次
// `iptables-restore --noflush -w` 事务原子重建（声明链即清空，同 netfilter/iptables.syncChains）。之后保证跳转组在链首。
func (n *Net) SyncRules(ctx context.Context, routed map[string]clusterspec.Spec) error {
	routed = n.validRouted(routed)
	want := ExpectedCluRules(routed)
	n.mu.Lock()
	n.lastRouted = maps.Clone(routed)
	n.mu.Unlock()
	out, err := n.run.Run(ctx, "iptables", "-w", "-S", ChainClu)
	if err != nil || strings.TrimSpace(string(out)) != strings.Join(want, "\n") {
		var b strings.Builder
		b.WriteString("*filter\n:" + ChainClu + " - [0:0]\n")
		for _, r := range want[1:] {
			b.WriteString(r + "\n")
		}
		b.WriteString("COMMIT\n")
		n.opt.Log.Info("iptables chain rebuilt", "chain", ChainClu, "instances", len(routed))
		if _, err := n.run.RunInput(ctx, []byte(b.String()), "iptables-restore", "--noflush", "-w"); err != nil {
			return cerr("%w", err)
		}
	}
	if err := (&iptables.Netfilter{Run: n.run, Log: n.opt.Log}).EnsureJumpGroups(ctx, bridge); err != nil {
		return cerr("%w", err)
	}
	return nil
}

// cluRulesOK：SPARK-CLU 的实际内容是否符合期望。SyncRules 跑过 → 与其输入的全量期望比较（且须含本实例）；
// 尚未跑过（Agent 刚启动）→ 只要求本实例的规则都在。
func (n *Net) cluRulesOK(out, id string, s clusterspec.Spec) bool {
	n.mu.Lock()
	last := n.lastRouted
	n.mu.Unlock()
	got := strings.TrimSpace(out)
	if last != nil {
		if _, ok := last[id]; !ok {
			return false
		}
		return got == strings.Join(ExpectedCluRules(last), "\n")
	}
	lines := map[string]bool{}
	for _, l := range strings.Split(got, "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	for _, r := range ExpectedCluRules(map[string]clusterspec.Spec{id: s}) {
		if !lines[r] {
			return false
		}
	}
	return true
}
