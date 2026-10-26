// Package iptables 用 iptables（SPARK-FWD / SPARK-IN 链，以及 INPUT / DOCKER-USER 链首的跳转组）与 tc 实现隔离与限速；所有操作幂等。
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

// ChainClu 是集群网络（计划 13 routed 模式）的转发白名单链；内容由 clusternet/linux 的 SyncRules 维护，
// 这里只保证它存在（不存在时 -N 并放一条 -j DROP），以便跳转组能指向它。
const ChainClu = "SPARK-CLU"

// 跳转组（计划 13 §3.3）：链首必须恰为这几条、按此顺序。不再「各自争第 1 位」——SPARK-FWD / SPARK-CLU / SPARK-IN
// 的跳转作为一组由 ensureJumpGroup 维护，反复运行结果稳定。"spark0" 是默认网桥名，EnsureRules 按 Rules.Bridge 替换。
var (
	DockerUserJumps = [][]string{{"-i", "spark0", "-j", chainFwd}, {"-i", "sch+", "-j", ChainClu}, {"-o", "sch+", "-j", ChainClu}}
	InputJumps      = [][]string{{"-i", "spark0", "-j", chainIn}, {"-i", "sch+", "-j", chainIn}}
)

// jumpsFor 把跳转组里的默认网桥名换成 bridge；withClu = false 时去掉指向 SPARK-CLU 的跳转（该链没能建出来）。
func jumpsFor(group [][]string, bridge string, withClu bool) [][]string {
	out := make([][]string, 0, len(group))
	for _, j := range group {
		if !withClu && j[len(j)-1] == ChainClu {
			continue
		}
		c := append([]string(nil), j...)
		for i := range c {
			if c[i] == "spark0" {
				c[i] = bridge
			}
		}
		out = append(out, c)
	}
	return out
}

// EnsureRules 先落地链内容（SPARK-IN 的保护优先），再保证 SPARK-CLU 存在，最后维护跳转组；每一步都尝试，错误合并返回。
// 链内容变更用一次 iptables-restore --noflush 事务原子完成：失败则链保持原样，不会出现空链或半截链。
// 若链内容没能落地，不再插入跳转（避免跳进一条未就绪的链），但 sysctl 仍会执行。
// SPARK-CLU 建不出来时仍维护 SPARK-FWD / SPARK-IN 的跳转（隔离优先），只是不插指向 SPARK-CLU 的两条。
func (n *Netfilter) EnsureRules(ctx context.Context, r netfilter.Rules) error {
	allow, err := canonicalCIDRs(r.AllowCIDRs)
	if err != nil {
		return err
	}
	var errs []error
	if err := n.syncChains(ctx, ExpectedFwdRules(allow), ExpectedInRules()); err != nil {
		errs = append(errs, err)
	} else {
		cluOK := true
		if err := n.ensureCluChain(ctx); err != nil {
			errs = append(errs, err)
			cluOK = false
		}
		errs = append(errs, n.ensureJumps(ctx, r.Bridge, cluOK)...)
	}
	if _, err := n.Run.Run(ctx, "sysctl", "-q", "-w", "net.ipv6.conf."+r.Bridge+".disable_ipv6=1"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// EnsureJumpGroups 只维护 INPUT / DOCKER-USER 的跳转组（假定 SPARK-IN / SPARK-FWD / SPARK-CLU 已存在）；
// clusternet/linux 的 SyncRules 在重建 SPARK-CLU 后调用。
func (n *Netfilter) EnsureJumpGroups(ctx context.Context, bridge string) error {
	return errors.Join(n.ensureJumps(ctx, bridge, true)...)
}

func (n *Netfilter) ensureJumps(ctx context.Context, bridge string, withClu bool) []error {
	var errs []error
	if err := n.ensureJumpGroup(ctx, "INPUT", jumpsFor(InputJumps, bridge, withClu)); err != nil {
		errs = append(errs, err)
	}
	if err := n.ensureJumpGroup(ctx, "DOCKER-USER", jumpsFor(DockerUserJumps, bridge, withClu)); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// ensureCluChain：SPARK-CLU 能列出（存在）就不碰内容；列不出 → -N 并放一条 -j DROP（链缺失时集群流量是「不通」而不是「放开」）。
func (n *Netfilter) ensureCluChain(ctx context.Context) error {
	if _, err := n.ipt(ctx, "-S", ChainClu); err == nil {
		return nil
	}
	if _, err := n.ipt(ctx, "-N", ChainClu); err != nil {
		return fmt.Errorf("iptables: 无法创建链 %s：%w", ChainClu, err)
	}
	if _, err := n.ipt(ctx, "-A", ChainClu, "-j", "DROP"); err != nil {
		return fmt.Errorf("iptables: 无法初始化链 %s：%w", ChainClu, err)
	}
	return nil
}

// listRules 解析 `iptables -S <chain>` 的输出，返回规则（去掉 "-A <chain> " 前缀），按链内顺序。
func listRules(out, chain string) []string {
	var rules []string
	prefix := "-A " + chain + " "
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			rules = append(rules, strings.TrimPrefix(line, prefix))
		}
	}
	return rules
}

// JumpGroupInPlace：`iptables -S <chain>` 的输出 out 里，链的前 len(jumps) 条是否恰为 jumps（按序）。
func JumpGroupInPlace(out, chain string, jumps [][]string) bool {
	rules := listRules(out, chain)
	if len(rules) < len(jumps) {
		return false
	}
	for i, j := range jumps {
		if rules[i] != strings.Join(j, " ") {
			return false
		}
	}
	return true
}

// ensureJumpGroup 保证链的前 len(jumps) 条恰为 jumps（按序）。已在位 → 不动（只读一次 -S）；
// 否则逐条把不在应在位置上的跳转 -I 到该位置，再按序号从大到小删掉链里别处的重复（先插后删：任何时刻隔离跳转都在）。
// 别的工具插在前面的规则会被挤到跳转组之后（只检查存在不够：插在前面的 ACCEPT 会绕过隔离链）。
// 列不出链（如 DOCKER-USER 不存在）时返回错误，不盲目插入（fail closed，由调用方上报）。
func (n *Netfilter) ensureJumpGroup(ctx context.Context, chain string, jumps [][]string) error {
	out, err := n.ipt(ctx, "-S", chain)
	if err != nil {
		return fmt.Errorf("iptables: 无法列出链 %s：%w", chain, err)
	}
	cur := listRules(string(out), chain)
	want := make([]string, len(jumps))
	for i, j := range jumps {
		want[i] = strings.Join(j, " ")
	}
	if JumpGroupInPlace(string(out), chain, jumps) {
		return nil
	}
	n.log().Warn("iptables jump group not in place, repairing", "chain", chain, "rules", len(cur))
	for i, j := range jumps {
		if i < len(cur) && cur[i] == want[i] {
			continue
		}
		if _, err := n.ipt(ctx, append([]string{"-I", chain, strconv.Itoa(i + 1)}, j...)...); err != nil {
			return err
		}
		cur = append(cur[:i], append([]string{want[i]}, cur[i:]...)...)
	}
	// 前 len(jumps) 条已就位；跳转组之后再出现的同样跳转是旧的，从大到小按序号删（前面的删除不影响后面要删的序号）。
	for pos := len(cur); pos > len(jumps); pos-- {
		for _, w := range want {
			if cur[pos-1] == w {
				if _, err := n.ipt(ctx, "-D", chain, strconv.Itoa(pos)); err != nil {
					return err
				}
				break
			}
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
