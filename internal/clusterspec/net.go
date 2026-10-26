package clusterspec

import (
	"fmt"
	"net/netip"
	"strings"
)

// Iface 是 Agent 探测到的一块物理网卡（规格 §3.2；全部从 sysfs 读）。
type Iface struct {
	Name       string   `json:"name"`
	MAC        string   `json:"mac"`
	Up         bool     `json:"up"` // operstate=up 且 carrier=1
	SpeedMbps  int      `json:"speed_mbps"`
	MTU        int      `json:"mtu"`
	IPv4       []string `json:"ipv4"` // CIDR 形式，如 "192.168.1.221/24"
	Driver     string   `json:"driver"`
	PCI        string   `json:"pci"`
	Wireless   bool     `json:"wireless"`
	RDMADevice string   `json:"rdma_device,omitempty"`
	RDMAActive bool     `json:"rdma_active,omitempty"`
	Uverbs     string   `json:"uverbs,omitempty"`
	LinkLayer  string   `json:"link_layer,omitempty"`
}

// NetInfo 是一次网卡探测结果（Hello.net / StateReport.net；machines.net_info 存其 JSON）。
type NetInfo struct {
	LanIface string  `json:"lan_iface"` // 仅供参考：持有 AgentConfig.lan_ip 的网卡；收到 AgentConfig 之前为空（平台按 machines.lan_ip 认管理网卡）
	Ifaces   []Iface `json:"ifaces"`
}

// Find 按名字找网卡。
func (n NetInfo) Find(name string) (Iface, bool) {
	for _, f := range n.Ifaces {
		if f.Name == name {
			return f, true
		}
	}
	return Iface{}, false
}

// MLX5Driver 是 ConnectX-7 的内核驱动名。
const MLX5Driver = "mlx5_core"

// MgmtIface 返回管理网卡名：IPv4（CIDR）里含 lanIP 的网卡；lanIP 为空或找不到时退回 n.LanIface
// （Hello 与首份 StateReport 在收到 AgentConfig 之前发出，LanIface 可能为空——平台以 machines.lan_ip 为准）。
func MgmtIface(n NetInfo, lanIP string) string {
	if lanIP != "" {
		for _, f := range n.Ifaces {
			for _, c := range f.IPv4 {
				if bareIP(c) == lanIP {
					return f.Name
				}
			}
		}
	}
	return n.LanIface
}

// ResolveIface 按 fabric 类型解析本机集群网卡（规格 §3.2；fleet 就绪判定与 rental 构造 Spec 共用）。管理网卡见 MgmtIface。
//   - kind "lan"：configured 或管理网卡；须存在、Up、有 IPv4。hostIP = 该网卡是管理网卡且 lanIP 非空时取 lanIP，否则取其第一个 IPv4 的地址部分（不带前缀长度）。
//   - kind "cx7"：configured 或第一个 Driver == "mlx5_core"、Up、非无线、不是管理网卡的网卡；须存在、Up、非无线、不是管理网卡。hostIP 为空。
//
// 错误为中文可展示原因（后台「未就绪原因」）。
func ResolveIface(n NetInfo, kind, configured, lanIP string) (Iface, string, error) {
	mgmt := MgmtIface(n, lanIP)
	switch kind {
	case "lan":
		name := configured
		if name == "" {
			name = mgmt
		}
		if name == "" {
			return Iface{}, "", fmt.Errorf("未找到管理网卡（lan_ip 所在网卡）")
		}
		f, ok := n.Find(name)
		if !ok {
			return Iface{}, "", fmt.Errorf("未找到网卡 %s", name)
		}
		if !f.Up {
			return Iface{}, "", fmt.Errorf("网卡 %s 未连接", name)
		}
		if len(f.IPv4) == 0 {
			return Iface{}, "", fmt.Errorf("网卡 %s 没有 IPv4 地址", name)
		}
		host := bareIP(f.IPv4[0])
		if name == mgmt && lanIP != "" {
			host = lanIP
		}
		return f, host, nil
	case "cx7":
		var f Iface
		if configured != "" {
			var ok bool
			if f, ok = n.Find(configured); !ok {
				return Iface{}, "", fmt.Errorf("未找到网卡 %s", configured)
			}
		} else {
			found := false
			for _, c := range n.Ifaces {
				if c.Driver == MLX5Driver && c.Up && c.Name != mgmt && !c.Wireless {
					f, found = c, true
					break
				}
			}
			if !found {
				return Iface{}, "", fmt.Errorf("未找到已连接的 CX-7 网卡")
			}
		}
		if !f.Up {
			return Iface{}, "", fmt.Errorf("网卡 %s 未连接", f.Name)
		}
		if f.Wireless {
			return Iface{}, "", fmt.Errorf("网卡 %s 是无线网卡，不能用于 CX-7 集群网络", f.Name)
		}
		if f.Name == mgmt {
			return Iface{}, "", fmt.Errorf("集群网卡不能是管理网卡 %s", f.Name)
		}
		return f, "", nil
	default:
		return Iface{}, "", fmt.Errorf("未知的集群网络类型 %q", kind)
	}
}

// NetPrefix 返回 IPv4 CIDR 的网络前缀（"192.168.1.221/24" → "192.168.1.0/24"）；非法或非 IPv4 → ""。
// fleet 用它算 machines.cluster_net（routed 成员须在同一网段）。
func NetPrefix(cidr string) string {
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() {
		return ""
	}
	return p.Masked().String()
}

func bareIP(s string) string {
	ip, _, _ := strings.Cut(s, "/")
	return ip
}
