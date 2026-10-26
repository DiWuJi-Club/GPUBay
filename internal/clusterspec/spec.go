// Package clusterspec 是平台与 Agent 共用的集群网络纯包（计划 13 规格 §3、§4.2）：
// ClusterSpec（下发给每个成员的集群描述）、网卡探测结构、子网 / 成员 IP 规划、
// 容器内 /etc/spark-cluster.json 与 cluster.env 的渲染、宿主机侧网卡命名。
// 不依赖任何项目包。
package clusterspec

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
)

const (
	ModeRouted   = "routed"        // lan fabric：veth + 宿主机 /32 路由 + SPARK-CLU
	ModeMacvlan  = "macvlan"       // cx7 fabric：CX-7 口（direct）或 VLAN 子接口（vlan）上的 macvlan
	IfName       = "spc0"          // 容器内集群网卡名
	DefaultPool  = "10.210.0.0/16" // 集群网段池（SPARK_CLUSTER_SUBNET_POOL 可改，必须 /16）
	MemberOffset = 11              // 成员 IP = 子网起始 + MemberOffset + rank
	MaxSize      = 8               // 表约束上限；产品只开放 2 / 4（rental.ClusterSizes）
	MasterPort   = 29500           // MASTER_PORT

	// ErrPrefix：Agent 因集群网络失败的 create / Devices 错误消息前缀；平台据此走集群网络失败路径、不判机器故障。
	ErrPrefix = "cluster-net: "
)

// Member 是集群的一个成员。
type Member struct {
	Rank       int    `json:"rank"`
	InstanceID string `json:"instance_id"`
	IP         string `json:"ip"`
	HostIP     string `json:"host_ip,omitempty"` // routed：对端宿主机集群网卡 IPv4
}

// Spec 是下发给一个成员的集群描述（CreateInstance.cluster / ConfigureCluster.cluster）。
// ClusterID 为空的 Spec 只用于 ConfigureCluster，表示「退出集群」，不经 Validate。
type Spec struct {
	ClusterID string   `json:"cluster_id"`
	Version   int      `json:"version"`
	Size      int      `json:"size"`
	Rank      int      `json:"rank"`
	Mode      string   `json:"mode"`
	Iface     string   `json:"iface"` // 宿主机集群网卡（routed 出口 / macvlan 父接口）
	VLAN      int      `json:"vlan,omitempty"`
	MTU       int      `json:"mtu,omitempty"`
	Subnet    string   `json:"subnet"`
	RDMA      bool     `json:"rdma"`
	Members   []Member `json:"members"` // 含自己，按 rank 升序；缺位的 rank 不出现
}

// Runtime 是 Agent 配好 spc0 之后得到的运行时结果（RDMA 的 HCA 与 RoCE v2 GID 索引）。
type Runtime struct {
	HCA        string `json:"hca,omitempty"`
	GIDIndex   int    `json:"gid_index"`
	RDMAActive bool   `json:"rdma_active"`
}

var (
	clusterIDRe  = regexp.MustCompile(`^clu-[a-z0-9]{10}$`)
	ifaceRe      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)
	instanceIDRe = regexp.MustCompile(`^ins-[a-z0-9]{8}$`)
)

// Subnet 返回池里第 index 个 /24。pool 必须是 IPv4 /16，index 0..255。
func Subnet(pool netip.Prefix, index int) (netip.Prefix, error) {
	if !pool.IsValid() || !pool.Addr().Is4() || pool.Bits() != 16 {
		return netip.Prefix{}, fmt.Errorf("集群网段池 %s 必须是 IPv4 /16", pool)
	}
	if index < 0 || index > 255 {
		return netip.Prefix{}, fmt.Errorf("集群子网序号 %d 超出 0～255", index)
	}
	a := pool.Masked().Addr().As4()
	a[2] = byte(index)
	return netip.PrefixFrom(netip.AddrFrom4(a), 24), nil
}

// MemberIP 返回子网起始 + MemberOffset + rank。
func MemberIP(subnet netip.Prefix, rank int) netip.Addr {
	return offset(subnet, MemberOffset+rank)
}

// GatewayIP 返回子网起始 + 1（routed 模式宿主机 veth 上的网关）。
func GatewayIP(subnet netip.Prefix) netip.Addr {
	return offset(subnet, 1)
}

func offset(subnet netip.Prefix, n int) netip.Addr {
	a := subnet.Masked().Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v += uint32(n)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func invalid(format string, args ...any) error {
	return errors.New("集群参数无效：" + fmt.Sprintf(format, args...))
}

// Validate 校验 Spec 的全部字段（Agent 在 create / ConfigureCluster 时调用，平台构造后自检）。
func (s Spec) Validate() error {
	if !clusterIDRe.MatchString(s.ClusterID) {
		return invalid("cluster_id %q", s.ClusterID)
	}
	if s.Version < 1 {
		return invalid("version %d", s.Version)
	}
	if s.Size < 2 || s.Size > MaxSize {
		return invalid("size %d 不在 2～%d", s.Size, MaxSize)
	}
	if s.Rank < 0 || s.Rank >= s.Size {
		return invalid("rank %d 不在 0～%d", s.Rank, s.Size-1)
	}
	if s.Mode != ModeRouted && s.Mode != ModeMacvlan {
		return invalid("mode %q", s.Mode)
	}
	if !ifaceRe.MatchString(s.Iface) {
		return invalid("iface %q", s.Iface)
	}
	if s.VLAN != 0 && (s.VLAN < 2 || s.VLAN > 4094) {
		return invalid("vlan %d", s.VLAN)
	}
	if s.MTU != 0 && (s.MTU < 1280 || s.MTU > 9216) {
		return invalid("mtu %d", s.MTU)
	}
	sub, err := netip.ParsePrefix(s.Subnet)
	if err != nil || !sub.Addr().Is4() || sub.Bits() != 24 || sub.Masked() != sub {
		return invalid("subnet %q 必须是 IPv4 /24 网络地址", s.Subnet)
	}
	self := false
	for i, m := range s.Members {
		if m.Rank < 0 || m.Rank >= s.Size {
			return invalid("成员 rank %d 不在 0～%d", m.Rank, s.Size-1)
		}
		if i > 0 && m.Rank <= s.Members[i-1].Rank {
			return invalid("成员必须按 rank 升序且不重复")
		}
		if !instanceIDRe.MatchString(m.InstanceID) {
			return invalid("成员实例 ID %q", m.InstanceID)
		}
		if want := MemberIP(sub, m.Rank).String(); m.IP != want {
			return invalid("成员 rank %d 的 IP %q 应为 %s", m.Rank, m.IP, want)
		}
		if m.Rank == s.Rank {
			self = true
		}
		switch s.Mode {
		case ModeRouted:
			if m.Rank == s.Rank && m.HostIP == "" {
				continue
			}
			if a, err := netip.ParseAddr(m.HostIP); err != nil || !a.Is4() {
				return invalid("成员 rank %d 的 host_ip %q 必须是 IPv4", m.Rank, m.HostIP)
			}
		case ModeMacvlan:
			if m.HostIP != "" {
				return invalid("macvlan 模式成员不带 host_ip")
			}
		}
	}
	if !self {
		return invalid("成员列表不含自己（rank %d）", s.Rank)
	}
	return nil
}

// Self 返回 rank == s.Rank 的成员（不存在时为零值）。
func (s Spec) Self() Member {
	for _, m := range s.Members {
		if m.Rank == s.Rank {
			return m
		}
	}
	return Member{}
}

// Head 返回 rank 0 的成员；rank 0 缺位（被退款释放 / 换机中）时 ok = false。
func (s Spec) Head() (Member, bool) {
	for _, m := range s.Members {
		if m.Rank == 0 {
			return m, true
		}
	}
	return Member{}, false
}

// Peers 返回除自己以外的成员（按 rank 升序）。
func (s Spec) Peers() []Member {
	out := make([]Member, 0, len(s.Members))
	for _, m := range s.Members {
		if m.Rank != s.Rank {
			out = append(out, m)
		}
	}
	return out
}
