// Package iptables 用 iptables（SPARK-FWD / SPARK-IN 链）与 tc 实现隔离与限速；所有操作幂等。
package iptables

import (
	"context"
	"fmt"
	"log/slog"
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

func (n *Netfilter) EnsureRules(ctx context.Context, r netfilter.Rules) error {
	_, _ = n.ipt(ctx, "-N", chainFwd) // 已存在时报错，忽略
	_, _ = n.ipt(ctx, "-N", chainIn)
	for _, j := range [][]string{{"DOCKER-USER", "-i", r.Bridge, "-j", chainFwd}, {"INPUT", "-i", r.Bridge, "-j", chainIn}} {
		if _, err := n.ipt(ctx, append([]string{"-C"}, j...)...); err != nil {
			if _, err := n.ipt(ctx, append([]string{"-I", j[0], "1"}, j[1:]...)...); err != nil {
				return err
			}
		}
	}
	if err := n.syncChain(ctx, chainFwd, ExpectedFwdRules(r.AllowCIDRs)); err != nil {
		return err
	}
	if err := n.syncChain(ctx, chainIn, ExpectedInRules()); err != nil {
		return err
	}
	_, err := n.Run.Run(ctx, "sysctl", "-q", "-w", "net.ipv6.conf."+r.Bridge+".disable_ipv6=1")
	return err
}

// syncChain 比较 `iptables -S <chain>` 与期望，不同才清空重建（规则不随意抖动）；重建时记一条 Info 日志，真机验收据此确认稳定。
func (n *Netfilter) syncChain(ctx context.Context, chain string, want []string) error {
	out, err := n.ipt(ctx, "-S", chain)
	if err == nil && strings.TrimSpace(string(out)) == strings.Join(want, "\n") {
		return nil
	}
	n.log().Info("iptables chain rebuilt", "chain", chain)
	if _, err := n.ipt(ctx, "-F", chain); err != nil {
		return err
	}
	for _, rule := range want[1:] { // 跳过 -N 链头
		if _, err := n.ipt(ctx, strings.Fields(rule)...); err != nil {
			return err
		}
	}
	return nil
}

// Limit：下行（宿主机 veth 出口 = 流入容器）用 tbf，上行（veth 入口）用 ingress police。
func (n *Netfilter) Limit(ctx context.Context, veth string, mbps int) error {
	rate := fmt.Sprintf("%dmbit", mbps)
	if _, err := n.Run.Run(ctx, "tc", "qdisc", "replace", "dev", veth, "root", "tbf", "rate", rate, "burst", "256kbit", "latency", "50ms"); err != nil {
		return err
	}
	_, _ = n.Run.Run(ctx, "tc", "qdisc", "del", "dev", veth, "ingress") // 不存在时报错，忽略
	if _, err := n.Run.Run(ctx, "tc", "qdisc", "add", "dev", veth, "ingress"); err != nil {
		return err
	}
	_, err := n.Run.Run(ctx, "tc", "filter", "add", "dev", veth, "parent", "ffff:", "protocol", "ip", "u32", "match", "u32", "0", "0",
		"police", "rate", rate, "burst", "256k", "drop", "flowid", ":1")
	return err
}
