// Package iptables 用 iptables（SPARK-FWD / SPARK-IN 链）与 tc 实现隔离与限速；所有操作幂等。
package iptables

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/netfilter"
)

var PrivateCIDRs = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "224.0.0.0/4"}

const (
	chainFwd = "SPARK-FWD"
	chainIn  = "SPARK-IN"
)

type Netfilter struct {
	Run cmdrun.Runner
	Log *slog.Logger
}

func (n *Netfilter) log() *slog.Logger {
	if n.Log == nil {
		return slog.Default()
	}
	return n.Log
}

// ExpectedFwdRules 是 `iptables -S SPARK-FWD` 应当输出的内容（含链头）。
// 注意 libxt_conntrack 按 INVALID,NEW,RELATED,ESTABLISHED 的固定顺序打印状态，所以这里写 RELATED,ESTABLISHED，
// 否则 syncChain 每 60 秒都会认为不一致而清空重建（中间有一个链为空的窗口）。
func ExpectedFwdRules(allow []string) []string {
	rules := []string{"-N " + chainFwd, "-A " + chainFwd + " -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT"}
	for _, c := range allow {
		rules = append(rules, fmt.Sprintf("-A %s -d %s -j ACCEPT", chainFwd, c))
	}
	for _, c := range PrivateCIDRs {
		rules = append(rules, fmt.Sprintf("-A %s -d %s -j DROP", chainFwd, c))
	}
	return append(rules, "-A "+chainFwd+" -j ACCEPT")
}

func ExpectedInRules() []string {
	return []string{"-N " + chainIn, "-A " + chainIn + " -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT", "-A " + chainIn + " -j DROP"}
}

func (n *Netfilter) ipt(ctx context.Context, args ...string) ([]byte, error) {
	return n.Run.Run(ctx, "iptables", append([]string{"-w"}, args...)...)
}

var _ netfilter.Netfilter = (*Netfilter)(nil)

// canonicalCIDRs 只接受 IPv4 CIDR，并规整为 iptables -S 会打印的形式（清掉主机位）。
func canonicalCIDRs(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, c := range in {
		p, err := netip.ParsePrefix(c)
		if err != nil || !p.Addr().Is4() || p.Bits() == 0 {
			return nil, fmt.Errorf("iptables: 无效的白名单网段 %q（需 IPv4 CIDR）", c)
		}
		out = append(out, p.Masked().String())
	}
	return out, nil
}

// EnsureRules 先落地链内容（SPARK-IN 的保护优先），再补跳转；每一步都尝试，错误合并返回。
// 链内容变更用一次 iptables-restore --noflush 事务原子完成：失败则链保持原样，不会出现空链或半截链。
// 若链内容没能落地，不再插入跳转（避免跳进一条未就绪的链），但 sysctl 仍会执行。
func (n *Netfilter) EnsureRules(ctx context.Context, r netfilter.Rules) error {
	allow, err := canonicalCIDRs(r.AllowCIDRs)
	if err != nil {
		return err
	}
	var errs []error
	if err := n.syncChains(ctx, ExpectedFwdRules(allow), ExpectedInRules()); err != nil {
		errs = append(errs, err)
	} else {
		for _, j := range [][]string{{"INPUT", "-i", r.Bridge, "-j", chainIn}, {"DOCKER-USER", "-i", r.Bridge, "-j", chainFwd}} {
			if err := n.ensureFirstJump(ctx, j); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if _, err := n.Run.Run(ctx, "sysctl", "-q", "-w", "net.ipv6.conf."+r.Bridge+".disable_ipv6=1"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ensureFirstJump 保证跳转规则 j（{链, 匹配…, -j, 目标}）是该链的第一条规则：只检查存在不够，
// 别的工具插在前面的 ACCEPT 会绕过隔离链。已在第一位不动；在别处则先删掉（含重复的）再插到第 1 位；不存在直接插入。
// 列不出链（如 DOCKER-USER 不存在）时返回错误，不盲目插入（fail closed，由调用方上报）。
func (n *Netfilter) ensureFirstJump(ctx context.Context, j []string) error {
	chain := j[0]
	out, err := n.ipt(ctx, "-S", chain)
	if err != nil {
		return fmt.Errorf("iptables: 无法列出链 %s：%w", chain, err)
	}
	rule := "-A " + strings.Join(j, " ")
	first, pos := "", 0
	var stale []int // 已有跳转在链中的序号（从 1 开始，按 -A 规则计数）
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-A ") {
			continue
		}
		pos++
		if first == "" {
			first = line
		}
		if line == rule {
			stale = append(stale, pos)
		}
	}
	if first == rule {
		return nil
	}
	if len(stale) > 0 {
		n.log().Warn("iptables jump not first in chain, moving it to position 1", "chain", chain, "first_rule", first)
	}
	// 先插到第 1 位再删旧的：插入失败时旧跳转仍在，删除期间新跳转已在最前，任何时刻都不会没有隔离。
	if _, err := n.ipt(ctx, append([]string{"-I", chain, "1"}, j[1:]...)...); err != nil {
		return err
	}
	// 插入后旧跳转的序号都加 1；从大到小按序号删，前面的删除不影响后面要删的序号。
	for i := len(stale) - 1; i >= 0; i-- {
		if _, err := n.ipt(ctx, "-D", chain, strconv.Itoa(stale[i]+1)); err != nil {
			return err
		}
	}
	return nil
}

func (n *Netfilter) chainMatches(ctx context.Context, chain string, want []string) bool {
	out, err := n.ipt(ctx, "-S", chain)
	return err == nil && strings.TrimSpace(string(out)) == strings.Join(want, "\n")
}

// syncChains 比较 `iptables -S` 与期望，有任何漂移就用一个 restore 事务重置两条链（声明链即清空，原子）。
func (n *Netfilter) syncChains(ctx context.Context, fwd, in []string) error {
	okIn := n.chainMatches(ctx, chainIn, in)
	okFwd := n.chainMatches(ctx, chainFwd, fwd)
	if okIn && okFwd {
		return nil
	}
	n.log().Info("iptables chain rebuilt", "chains", chainIn+","+chainFwd)
	var b strings.Builder
	b.WriteString("*filter\n")
	for _, rules := range [][]string{in, fwd} {
		b.WriteString(":" + strings.TrimPrefix(rules[0], "-N ") + " - [0:0]\n")
	}
	for _, rules := range [][]string{in, fwd} {
		for _, rule := range rules[1:] {
			b.WriteString(rule + "\n")
		}
	}
	b.WriteString("COMMIT\n")
	_, err := n.Run.RunInput(ctx, []byte(b.String()), "iptables-restore", "--noflush", "-w")
	return err
}

// Limit：下行（宿主机 veth 出口 = 流入容器）用 tbf，上行（veth 入口）用 ingress police。
func (n *Netfilter) Limit(ctx context.Context, veth string, mbps int) error {
	if mbps <= 0 {
		return fmt.Errorf("iptables: 限速必须为正数，当前 %d", mbps)
	}
	rate := fmt.Sprintf("%dmbit", mbps)
	if _, err := n.Run.Run(ctx, "tc", "qdisc", "replace", "dev", veth, "root", "tbf", "rate", rate, "burst", "256kbit", "latency", "50ms"); err != nil {
		return err
	}
	_, _ = n.Run.Run(ctx, "tc", "qdisc", "del", "dev", veth, "ingress") // 不存在时报错，忽略
	if _, err := n.Run.Run(ctx, "tc", "qdisc", "add", "dev", veth, "ingress"); err != nil {
		return err
	}
	_, err := n.Run.Run(ctx, "tc", "filter", "add", "dev", veth, "parent", "ffff:", "protocol", "all", "u32", "match", "u32", "0", "0",
		"police", "rate", rate, "burst", "256k", "drop", "flowid", ":1")
	return err
}

// LimitApplied：root 上有 tbf 且 ingress 上有 police 过滤器才算限速在位。
// 只判断存在性、不比较速率（tc 会把 1000mbit 打印成 1Gbit）；ingress 不存在直接返回 false，不去查 parent ffff:。
func (n *Netfilter) LimitApplied(ctx context.Context, veth string) (bool, error) {
	root, err := n.Run.Run(ctx, "tc", "qdisc", "show", "dev", veth, "root")
	if err != nil {
		return false, err
	}
	if !strings.Contains(string(root), " tbf ") {
		return false, nil
	}
	ing, err := n.Run.Run(ctx, "tc", "qdisc", "show", "dev", veth, "ingress")
	if err != nil {
		return false, err
	}
	if !strings.Contains(string(ing), "qdisc ingress") {
		return false, nil
	}
	flt, err := n.Run.Run(ctx, "tc", "filter", "show", "dev", veth, "parent", "ffff:")
	if err != nil {
		return false, err
	}
	return strings.Contains(string(flt), "police"), nil
}
