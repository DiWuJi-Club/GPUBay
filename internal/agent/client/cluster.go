package client

// 计划 13：clusterspec ↔ agentpb 转换（Agent 侧；平台侧在 agentgw）。

import (
	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/clusterspec"
)

// specFromProto 把 CreateInstance.cluster / ConfigureCluster.cluster 转回 clusterspec.Spec；nil → nil。
func specFromProto(p *agentpb.ClusterSpec) *clusterspec.Spec {
	if p == nil {
		return nil
	}
	s := &clusterspec.Spec{ClusterID: p.GetClusterId(), Version: int(p.GetVersion()), Size: int(p.GetSize()), Rank: int(p.GetRank()),
		Mode: p.GetMode(), Iface: p.GetIface(), VLAN: int(p.GetVlan()), MTU: int(p.GetMtu()), Subnet: p.GetSubnet(), RDMA: p.GetRdma()}
	for _, m := range p.GetMembers() {
		s.Members = append(s.Members, clusterspec.Member{Rank: int(m.GetRank()), InstanceID: m.GetInstanceId(), IP: m.GetIp(), HostIP: m.GetHostIp()})
	}
	return s
}

// netInfoToProto 把网卡探测结果转成 Hello.net / StateReport.net；nil → nil（该消息不带 net）。
func netInfoToProto(n *clusterspec.NetInfo) *agentpb.NetInfo {
	if n == nil {
		return nil
	}
	out := &agentpb.NetInfo{LanIface: n.LanIface}
	for _, f := range n.Ifaces {
		out.Ifaces = append(out.Ifaces, &agentpb.NetIface{Name: f.Name, Mac: f.MAC, Up: f.Up, SpeedMbps: int32(f.SpeedMbps), Mtu: uint32(max(f.MTU, 0)),
			Ipv4: append([]string(nil), f.IPv4...), Driver: f.Driver, Pci: f.PCI, Wireless: f.Wireless,
			RdmaDevice: f.RDMADevice, RdmaActive: f.RDMAActive, Uverbs: f.Uverbs, LinkLayer: f.LinkLayer})
	}
	return out
}
