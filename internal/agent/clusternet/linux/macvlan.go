package linux

import (
	"context"
	"strconv"
	"strings"

	"spark-cloud/internal/clusterspec"
)

// refuseMgmt：macvlan 父接口卫生会清掉网卡上的地址——若 iface 持有本机 IPv4 默认路由或管理地址（Probe 记下的 lanIP；
// 尚未 Probe 时为 Options.LanIPs，即状态文件里实例记录的 LanIP），
// 那就是管理网卡，清掉会让机器失联。只读两条命令，不执行任何写操作。
func (n *Net) refuseMgmt(ctx context.Context, iface string) error {
	out, err := n.ip(ctx, "-4", "route", "show", "default")
	if err != nil {
		return cerr("%w", err)
	}
	for _, dev := range routeDevs(string(out)) {
		if dev == iface {
			return cerr("集群网卡不能是管理网卡（%s 持有默认路由）", iface)
		}
	}
	addrs, err := n.ip(ctx, "-4", "-o", "addr", "show", "dev", iface)
	if err != nil {
		if missing(err) {
			return cerr("集群网卡 %s 不存在", iface)
		}
		return cerr("%w", err)
	}
	for _, lan := range n.mgmtIPs() {
		if strings.Contains(string(addrs), " inet "+lan+"/") {
			return cerr("集群网卡不能是管理网卡（%s 持有管理地址 %s）", iface, lan)
		}
	}
	return nil
}

// mgmtIPs：Probe 记下的 lanIP；为空（Agent 重启后尚未 Probe）时退回 Options.LanIPs。
func (n *Net) mgmtIPs() []string {
	n.mu.Lock()
	lan := n.lanIP
	n.mu.Unlock()
	if lan != "" {
		return []string{lan}
	}
	if n.opt.LanIPs == nil {
		return nil
	}
	var out []string
	for _, ip := range n.opt.LanIPs() {
		if ip != "" {
			out = append(out, ip)
		}
	}
	return out
}

// routeDevs 取 `ip route` 输出里每行 "dev X" 的 X。
func routeDevs(out string) []string {
	var devs []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "dev" {
				devs = append(devs, f[i+1])
			}
		}
	}
	return devs
}

// macvlanParent：vlan fabric 挂在 scv<vlan> 上，direct 挂在 CX-7 口本身。
func macvlanParent(s clusterspec.Spec) string {
	if s.VLAN != 0 {
		return clusterspec.VLANDev(s.VLAN)
	}
	return s.Iface
}

// hygieneDevs：需要保持「无地址、关 IPv6」的宿主机网卡（CX-7 口，及 vlan fabric 的 VLAN 子接口）。
func hygieneDevs(s clusterspec.Spec) []string {
	if s.VLAN != 0 {
		return []string{s.Iface, clusterspec.VLANDev(s.VLAN)}
	}
	return []string{s.Iface}
}

// applyMacvlan（规格 §3.4）：VLAN 子接口缺则建（已存在不重建）→ 父接口卫生（每次）→ 容器没有 spc0 时才建 macvlan →
// 幂等部分（A_r/24、关 IPv6、up）。
func (n *Net) applyMacvlan(ctx context.Context, id string, pid int, s clusterspec.Spec) error {
	parent := macvlanParent(s)
	if s.VLAN != 0 {
		ok, _, err := n.linkExists(ctx, parent)
		if err != nil {
			return err
		}
		if !ok {
			if err := n.steps(ctx, []string{"ip", "link", "add", "link", s.Iface, "name", parent, "type", "vlan", "id", strconv.Itoa(s.VLAN)}); err != nil {
				return err
			}
		}
	}
	if err := n.hygiene(ctx, s); err != nil {
		return err
	}
	has, _, err := n.hasSpc0(ctx, pid)
	if err != nil {
		return err
	}
	if !has {
		mv := clusterspec.MacvlanName(id)
		if err := n.clearLeftover(ctx, pid, mv, mv); err != nil {
			return err
		}
		if err := n.steps(ctx,
			[]string{"ip", "link", "add", mv, "link", parent, "type", "macvlan", "mode", "bridge"},
			[]string{"ip", "link", "set", mv, "netns", strconv.Itoa(pid)},
			nsCmd(pid, "ip", "link", "set", mv, "name", clusterspec.IfName),
		); err != nil {
			return err
		}
	}
	return n.steps(ctx,
		nsCmd(pid, "ip", "addr", "replace", selfCIDR(s), "dev", clusterspec.IfName),
		nsCmd(pid, "sysctl", "-w", "net.ipv6.conf."+clusterspec.IfName+".disable_ipv6=1"),
		nsCmd(pid, "ip", "link", "set", clusterspec.IfName, "up"),
	)
}

// hygiene：nmcli device set <iface> managed no（nmcli 不存在或失败忽略）；每块卫生网卡 flush IPv4、关 IPv6、设 MTU、up。
// NetworkManager / netplan 可能在链路变化时重新加地址，那样对端容器就能直达本机宿主（§3.4）。
func (n *Net) hygiene(ctx context.Context, s clusterspec.Spec) error {
	if _, err := n.run.Run(ctx, "nmcli", "device", "set", s.Iface, "managed", "no"); err != nil {
		n.opt.Log.Debug("nmcli unmanaged failed (ignored)", "iface", s.Iface, "err", err)
	}
	for _, dev := range hygieneDevs(s) {
		cmds := [][]string{
			{"ip", "-4", "addr", "flush", "dev", dev},
			{"sysctl", "-w", "net.ipv6.conf." + dev + ".disable_ipv6=1"},
		}
		if s.MTU > 0 {
			cmds = append(cmds, []string{"ip", "link", "set", dev, "mtu", strconv.Itoa(s.MTU)})
		}
		cmds = append(cmds, []string{"ip", "link", "set", dev, "up"})
		if err := n.steps(ctx, cmds...); err != nil {
			return err
		}
	}
	return nil
}
