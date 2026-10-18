// Package iptables 用 iptables（SPARK-FWD / SPARK-IN 链）与 tc 实现隔离与限速；所有操作幂等。
package iptables

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
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
			if _, err := n.ipt(ctx, append([]string{"-C"}, j...)...); err != nil {
				if _, err := n.ipt(ctx, append([]string{"-I", j[0], "1"}, j[1:]...)...); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	if _, err := n.Run.Run(ctx, "sysctl", "-q", "-w", "net.ipv6.conf."+r.Bridge+".disable_ipv6=1"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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
