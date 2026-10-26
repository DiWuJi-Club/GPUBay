package fleet

import (
	"strings"
	"testing"

	"spark-cloud/internal/agent/features"
	"spark-cloud/internal/clusterspec"
)

func sampleNet(mut func(*clusterspec.NetInfo)) *clusterspec.NetInfo {
	n := &clusterspec.NetInfo{LanIface: "enP7s7", Ifaces: []clusterspec.Iface{
		{Name: "enP7s7", Up: true, MTU: 1500, IPv4: []string{"192.168.1.221/24"}, Driver: "r8127"},
		{Name: "wlP9s9", Up: true, MTU: 1500, IPv4: []string{"10.8.0.5/16"}, Driver: "mt7925e", Wireless: true},
		{Name: "enp1s0f0np0", Up: true, MTU: 9000, Driver: clusterspec.MLX5Driver, RDMADevice: "rocep1s0f0", RDMAActive: true, Uverbs: "uverbs0", LinkLayer: "Ethernet"},
		{Name: "enp1s0f1np1", Up: false, MTU: 1500, Driver: clusterspec.MLX5Driver, RDMADevice: "rocep1s0f1"},
	}}
	if mut != nil {
		mut(n)
	}
	return n
}

func TestEvalClusterNetMatrix(t *testing.T) {
	lan := &Fabric{ID: 1, Kind: FabricKindLAN}
	cx7 := &Fabric{ID: 2, Kind: FabricKindCX7, RDMA: true}
	cx7NoRDMA := &Fabric{ID: 3, Kind: FabricKindCX7}
	base := Machine{LanIP: "192.168.1.221", AgentFeatures: features.SharedSlots | features.Cluster}
	with := func(mut func(*Machine)) Machine {
		m := base
		mut(&m)
		return m
	}
	cases := []struct {
		name      string
		m         Machine
		f         *Fabric
		n         *clusterspec.NetInfo
		ready     bool
		rdma      bool
		net       string
		reasonHas string
	}{
		{"无 fabric", base, nil, sampleNet(nil), false, false, "", "未加入集群网络"},
		{"旧 Agent", with(func(m *Machine) { m.AgentFeatures = features.SharedSlots }), lan, sampleNet(nil), false, false, "", "Agent 不支持集群（请升级）"},
		{"无网卡探测", base, lan, nil, false, false, "", "尚未上报网卡"},
		{"lan 正常", base, lan, sampleNet(nil), true, false, "192.168.1.0/24", ""},
		{"lan LanIface 为空但 lan_ip 能匹配", base, lan, sampleNet(func(n *clusterspec.NetInfo) { n.LanIface = "" }), true, false, "192.168.1.0/24", ""},
		{"lan lan_ip 匹配不到且 LanIface 为空", with(func(m *Machine) { m.LanIP = "10.9.9.9" }), lan,
			sampleNet(func(n *clusterspec.NetInfo) { n.LanIface = "" }), false, false, "", "未找到管理网卡"},
		{"lan 管理网卡 down", base, lan, sampleNet(func(n *clusterspec.NetInfo) { n.Ifaces[0].Up = false }), false, false, "", "网卡 enP7s7 未连接"},
		{"lan 指定无线网卡（另一网段）", with(func(m *Machine) { m.ClusterIface = "wlP9s9" }), lan, sampleNet(nil), true, false, "10.8.0.0/16", ""},
		{"lan 管理网卡多地址取 lan_ip 所在网段", base, lan,
			sampleNet(func(n *clusterspec.NetInfo) { n.Ifaces[0].IPv4 = []string{"172.16.5.1/16", "192.168.1.221/24"} }), true, false, "192.168.1.0/24", ""},
		{"lan 指定网卡没有 IPv4", with(func(m *Machine) { m.ClusterIface = "enp1s0f0np0" }), lan, sampleNet(nil), false, false, "", "没有 IPv4"},
		{"lan 不存在的网卡", with(func(m *Machine) { m.ClusterIface = "eth9" }), lan, sampleNet(nil), false, false, "", "未找到网卡 eth9"},
		{"cx7 自动 + RDMA", base, cx7, sampleNet(nil), true, true, "l2", ""},
		{"cx7 自动，fabric 不要 RDMA", base, cx7NoRDMA, sampleNet(nil), true, false, "l2", ""},
		{"cx7 RDMA 未激活", base, cx7, sampleNet(func(n *clusterspec.NetInfo) { n.Ifaces[2].RDMAActive = false }), true, false, "l2", ""},
		{"cx7 没有 uverbs", base, cx7, sampleNet(func(n *clusterspec.NetInfo) { n.Ifaces[2].Uverbs = "" }), true, false, "l2", ""},
		{"cx7 指定无线网卡", with(func(m *Machine) { m.ClusterIface = "wlP9s9" }), cx7, sampleNet(nil), false, false, "", "无线网卡"},
		{"cx7 指定管理网卡", with(func(m *Machine) { m.ClusterIface = "enP7s7" }), cx7, sampleNet(nil), false, false, "", "不能是管理网卡"},
		{"cx7 指定 down 的口", with(func(m *Machine) { m.ClusterIface = "enp1s0f1np1" }), cx7, sampleNet(nil), false, false, "", "未连接"},
		{"cx7 没有已连接的 CX-7 口", base, cx7, sampleNet(func(n *clusterspec.NetInfo) { n.Ifaces[2].Up = false }), false, false, "", "未找到已连接的 CX-7 网卡"},
		{"cx7 LanIface 为空也不会把管理网卡当集群网卡", with(func(m *Machine) { m.ClusterIface = "enP7s7" }), cx7,
			sampleNet(func(n *clusterspec.NetInfo) { n.LanIface = "" }), false, false, "", "不能是管理网卡"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ready, rdma, net, reason := evalClusterNet(tc.m, tc.f, tc.n)
			if ready != tc.ready || rdma != tc.rdma || net != tc.net {
				t.Fatalf("got ready=%v rdma=%v net=%q reason=%q", ready, rdma, net, reason)
			}
			if tc.ready && reason != "" || !tc.ready && !strings.Contains(reason, tc.reasonHas) {
				t.Fatalf("reason = %q, want contains %q", reason, tc.reasonHas)
			}
		})
	}
}

func TestSanitizeNetInfo(t *testing.T) {
	n := clusterspec.NetInfo{}
	for i := 0; i < maxNetIfaces+5; i++ {
		n.Ifaces = append(n.Ifaces, clusterspec.Iface{Name: "x", IPv4: make([]string, maxNetIfaceAddress+3)})
	}
	n.Ifaces[1].IPv4 = nil
	got := sanitizeNetInfo(n)
	if len(got.Ifaces) != maxNetIfaces || len(got.Ifaces[0].IPv4) != maxNetIfaceAddress || got.Ifaces[1].IPv4 == nil {
		t.Fatalf("sanitized = %d ifaces, %d addrs, %v", len(got.Ifaces), len(got.Ifaces[0].IPv4), got.Ifaces[1].IPv4)
	}
	if empty := sanitizeNetInfo(clusterspec.NetInfo{}); empty.Ifaces == nil {
		t.Fatal("nil ifaces must become empty slice")
	}
}
