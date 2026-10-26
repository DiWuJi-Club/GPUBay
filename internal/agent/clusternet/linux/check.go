package linux

import (
	"context"
	"errors"
	"strings"

	"spark-cloud/internal/agent/netfilter/iptables"
	"spark-cloud/internal/clusterspec"
)

// Check 是 60 秒巡检（只读，唯一的写是父接口卫生：发现地址即清除）。ok = false 时 err 为可展示的问题描述（多条以「；」连接）：
//   - 容器内 spc0 存在、有 A_r、IPv6 关（routed 另查容器内子网路由）；
//   - routed：宿主机 A_r/32 dev sch<x>、对端路由集合与期望相同、iptables -S SPARK-CLU 与期望相同、跳转组在位；
//   - macvlan：VLAN 子接口存在且 up、父接口无 IPv4 / IPv6 地址（发现即清除并注明「父接口出现地址，已清除」）、IPv6 关；
//   - RDMA：uverbs 映射未变。
func (n *Net) Check(ctx context.Context, instanceID string, pid int, s clusterspec.Spec) (bool, error) {
	if err := s.Validate(); err != nil {
		return false, err
	}
	var probs []string
	add := func(p string) { probs = append(probs, p) }

	has, addrs, err := n.hasSpc0(ctx, pid)
	switch {
	case err != nil:
		add("无法检查容器网络：" + strings.TrimPrefix(err.Error(), clusterspec.ErrPrefix))
	case !has:
		add("容器内缺少 " + clusterspec.IfName)
	default:
		if !hasAddr(addrs, selfCIDR(s)) {
			add(clusterspec.IfName + " 缺少地址 " + selfCIDR(s))
		}
		if v, err := n.ns(ctx, pid, "sysctl", "-n", "net.ipv6.conf."+clusterspec.IfName+".disable_ipv6"); err != nil || strings.TrimSpace(string(v)) != "1" {
			add(clusterspec.IfName + " 未关闭 IPv6")
		}
		if s.Mode == clusterspec.ModeRouted {
			if out, err := n.ns(ctx, pid, "ip", "-4", "route", "show", s.Subnet, "dev", clusterspec.IfName); err != nil || strings.TrimSpace(string(out)) == "" {
				add("容器内缺少集群子网路由")
			}
		}
	}

	switch s.Mode {
	case clusterspec.ModeRouted:
		n.checkRouted(ctx, instanceID, s, add)
	case clusterspec.ModeMacvlan:
		n.checkMacvlan(ctx, s, add)
	}

	if s.RDMA {
		if hca, _ := n.rdmaOf(s.Iface); hca == "" || n.uverbsChanged(pid, hca) {
			add("RDMA 设备编号变化，需重建实例")
		}
	}
	if len(probs) > 0 {
		return false, errors.New(strings.Join(probs, "；"))
	}
	return true, nil
}

func (n *Net) checkRouted(ctx context.Context, id string, s clusterspec.Spec, add func(string)) {
	host, self := clusterspec.HostVeth(id), s.Self().IP
	if out, err := n.ip(ctx, "-4", "route", "show", self+"/32", "dev", host); err != nil || strings.TrimSpace(string(out)) == "" {
		add("缺少宿主机路由 " + self + "/32 dev " + host)
	}
	if cur, err := n.peerRoutes(ctx, s.Subnet, s.Iface); err != nil || !equalRoutes(cur, wantPeerRoutes(s)) {
		add("对端路由与期望不一致")
	}
	if out, err := n.run.Run(ctx, "iptables", "-w", "-S", ChainClu); err != nil || !n.cluRulesOK(string(out), id, s) {
		add(ChainClu + " 规则与期望不一致")
	}
	for _, g := range []struct {
		chain string
		jumps [][]string
	}{{"DOCKER-USER", iptables.DockerUserJumps}, {"INPUT", iptables.InputJumps}} {
		if out, err := n.run.Run(ctx, "iptables", "-w", "-S", g.chain); err != nil || !iptables.JumpGroupInPlace(string(out), g.chain, g.jumps) {
			add(g.chain + " 跳转组不在链首")
		}
	}
}

func (n *Net) checkMacvlan(ctx context.Context, s clusterspec.Spec, add func(string)) {
	if s.VLAN != 0 {
		dev := clusterspec.VLANDev(s.VLAN)
		ok, out, err := n.linkExists(ctx, dev)
		switch {
		case err != nil || !ok:
			add("VLAN 子接口 " + dev + " 不存在")
		case !linkUp(out):
			add("VLAN 子接口 " + dev + " 未启用")
		}
	}
	for _, dev := range hygieneDevs(s) {
		out, err := n.ip(ctx, "-o", "addr", "show", "dev", dev)
		if err != nil {
			continue // 网卡不存在：VLAN 子接口已在上面报告；CX-7 口不在由 Apply 报错
		}
		if hasAnyAddr(string(out)) {
			_, _ = n.ip(ctx, "-4", "addr", "flush", "dev", dev)
			_, _ = n.run.Run(ctx, "sysctl", "-w", "net.ipv6.conf."+dev+".disable_ipv6=1")
			add("父接口 " + dev + " 出现地址，已清除")
			continue
		}
		if v, err := n.run.Run(ctx, "sysctl", "-n", "net.ipv6.conf."+dev+".disable_ipv6"); err != nil || strings.TrimSpace(string(v)) != "1" {
			_, _ = n.run.Run(ctx, "sysctl", "-w", "net.ipv6.conf."+dev+".disable_ipv6=1")
			add("父接口 " + dev + " 未关闭 IPv6，已关闭")
		}
	}
}

// linkUp：`ip -o link show` 的标志位 <…,UP,…> 里有 UP（管理上启用）。
func linkUp(out string) bool {
	i, j := strings.Index(out, "<"), strings.Index(out, ">")
	if i < 0 || j < i {
		return false
	}
	for _, f := range strings.Split(out[i+1:j], ",") {
		if f == "UP" {
			return true
		}
	}
	return false
}

// hasAnyAddr：`ip -o addr show dev X` 里有任何 inet / inet6 地址。
func hasAnyAddr(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && (f[2] == "inet" || f[2] == "inet6") {
			return true
		}
	}
	return false
}
