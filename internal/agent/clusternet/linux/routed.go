package linux

import (
	"context"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"spark-cloud/internal/clusterspec"
)

// applyRouted（规格 §3.3）：
//
//  0. 观察容器 spc0；没有时才建网卡（宿主机残留 sch<x> / 容器内残留 scc<x> 先删）
//  1. ip link add sch<x> mtu M type veth peer name scc<x> mtu M → 移进容器 → 改名 spc0
//  2. 幂等部分（每次都执行，不打断现有连接）：容器 A_r/32、关 IPv6、up、网关与子网路由；宿主机 G/32、up、A_r/32 dev sch<x>、rp_filter
//  3. 对端路由差量：期望 {A_k/32 via H_k dev I | k ≠ r}，多的删、缺的 replace
func (n *Net) applyRouted(ctx context.Context, id string, pid int, s clusterspec.Spec) error {
	sub := netip.MustParsePrefix(s.Subnet) // Validate 已保证
	gw := clusterspec.GatewayIP(sub).String()
	self := s.Self().IP
	host, tmp := clusterspec.HostVeth(id), clusterspec.TempName(id)
	has, _, err := n.hasSpc0(ctx, pid)
	if err != nil {
		return err
	}
	if !has {
		if err := n.clearLeftover(ctx, pid, host, tmp); err != nil {
			return err
		}
		mtu := strconv.Itoa(mtuOr1500(s.MTU))
		if err := n.steps(ctx,
			[]string{"ip", "link", "add", host, "mtu", mtu, "type", "veth", "peer", "name", tmp, "mtu", mtu},
			[]string{"ip", "link", "set", tmp, "netns", strconv.Itoa(pid)},
			nsCmd(pid, "ip", "link", "set", tmp, "name", clusterspec.IfName),
		); err != nil {
			return err
		}
	}
	if err := n.steps(ctx,
		nsCmd(pid, "ip", "addr", "replace", self+"/32", "dev", clusterspec.IfName),
		nsCmd(pid, "sysctl", "-w", "net.ipv6.conf."+clusterspec.IfName+".disable_ipv6=1"),
		nsCmd(pid, "ip", "link", "set", clusterspec.IfName, "up"),
		nsCmd(pid, "ip", "route", "replace", gw+"/32", "dev", clusterspec.IfName, "scope", "link"),
		nsCmd(pid, "ip", "route", "replace", sub.String(), "via", gw, "dev", clusterspec.IfName),
		[]string{"ip", "addr", "replace", gw + "/32", "dev", host},
		[]string{"ip", "link", "set", host, "up"},
		[]string{"ip", "route", "replace", self + "/32", "dev", host},
		[]string{"sysctl", "-w", "net.ipv4.conf." + host + ".rp_filter=1", "net.ipv6.conf." + host + ".disable_ipv6=1"},
	); err != nil {
		return err
	}
	return n.syncPeerRoutes(ctx, s)
}

func mtuOr1500(m int) int {
	if m > 0 {
		return m
	}
	return 1500
}

// clearLeftover：容器里还没有 spc0（首次或上次半途失败）时，删掉宿主机残留的 hostDev 与容器内残留的临时名 tmp。
func (n *Net) clearLeftover(ctx context.Context, pid int, hostDev, tmp string) error {
	ok, _, err := n.linkExists(ctx, hostDev)
	if err != nil {
		return err
	}
	if ok {
		if _, err := n.ip(ctx, "link", "del", hostDev); err != nil && !missing(err) {
			return cerr("%w", err)
		}
	}
	if _, err := n.ns(ctx, pid, "ip", "link", "del", tmp); err != nil && !missing(err) {
		return cerr("%w", err)
	}
	return nil
}

// wantPeerRoutes：对端成员 IP（/32）→ 对端宿主机 IP。
func wantPeerRoutes(s clusterspec.Spec) map[string]string {
	want := map[string]string{}
	for _, p := range s.Peers() {
		want[p.IP+"/32"] = p.HostIP
	}
	return want
}

// peerRoutes 列出 `ip -4 route show root <subnet> dev <iface>`（root：子网内所有更长前缀；不加 root 只精确匹配 /24）：
// 输出形如 "10.210.3.12 via 192.168.1.222 "（指定 dev 后不再打印 dev）。返回 目的（规整为 CIDR）→ via（没有 via 为 ""）。
func (n *Net) peerRoutes(ctx context.Context, subnet, iface string) (map[string]string, error) {
	out, err := n.ip(ctx, "-4", "route", "show", "root", subnet, "dev", iface)
	if err != nil {
		return nil, cerr("%w", err)
	}
	cur := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		dst := f[0]
		if !strings.Contains(dst, "/") {
			dst += "/32"
		}
		via := ""
		for i := 1; i+1 < len(f); i++ {
			if f[i] == "via" {
				via = f[i+1]
			}
		}
		cur[dst] = via
	}
	return cur, nil
}

// syncPeerRoutes：多出的对端路由删除、缺失或 via 不对的 replace；与期望相同时不写。
func (n *Net) syncPeerRoutes(ctx context.Context, s clusterspec.Spec) error {
	cur, err := n.peerRoutes(ctx, s.Subnet, s.Iface)
	if err != nil {
		return err
	}
	want := wantPeerRoutes(s)
	for _, dst := range sortedKeys(cur) {
		if _, ok := want[dst]; !ok {
			if _, err := n.ip(ctx, "route", "del", dst, "dev", s.Iface); err != nil && !missing(err) {
				return cerr("%w", err)
			}
		}
	}
	for _, p := range s.Peers() {
		dst := p.IP + "/32"
		if v, ok := cur[dst]; ok && v == p.HostIP {
			continue
		}
		if _, err := n.ip(ctx, "route", "replace", dst, "via", p.HostIP, "dev", s.Iface); err != nil {
			return cerr("%w", err)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// equalRoutes：当前对端路由集合与期望完全相同。
func equalRoutes(cur, want map[string]string) bool {
	if len(cur) != len(want) {
		return false
	}
	for k, v := range want {
		if cur[k] != v {
			return false
		}
	}
	return true
}
