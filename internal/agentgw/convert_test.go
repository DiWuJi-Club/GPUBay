package agentgw

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"

	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/fleet"
)

func createCmd(t *testing.T, p fleet.CreateInstancePayload) fleet.Command {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return fleet.Command{ID: "c1", InstanceID: "i1", Type: fleet.CmdCreateInstance, Payload: b}
}

func TestToProtoCommandCreatePortAndShareSlots(t *testing.T) {
	cfg := Config{SSHPort: 2222, BandwidthMbps: 100, DataSizeGiB: 2048, UpstreamPubKey: "ssh-ed25519 AAA"}
	cases := []struct {
		name      string
		payload   fleet.CreateInstancePayload
		wantPort  uint32
		wantSlots uint32
	}{
		{"指令带端口与份数", fleet.CreateInstancePayload{ImageRef: "img:1", SSHPort: 2224, ShareSlots: 4}, 2224, 4},
		{"整机指令", fleet.CreateInstancePayload{ImageRef: "img:1", SSHPort: 2222, ShareSlots: 1}, 2222, 1},
		{"存量指令无端口退回配置", fleet.CreateInstancePayload{ImageRef: "img:1"}, 2222, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := toProtoCommand(createCmd(t, tc.payload), cfg, "PEM")
			if err != nil {
				t.Fatal(err)
			}
			c := msg.GetCreate()
			if c == nil {
				t.Fatal("not a create command")
			}
			if c.GetSshPort() != tc.wantPort || c.GetShareSlots() != tc.wantSlots {
				t.Fatalf("port=%d slots=%d, want %d %d", c.GetSshPort(), c.GetShareSlots(), tc.wantPort, tc.wantSlots)
			}
			if c.GetImageRef() != "img:1" || c.GetHostKeyPem() != "PEM" || c.GetDataSizeGib() != 2048 || c.GetBandwidthMbps() != 100 {
				t.Fatalf("other fields changed: %+v", c)
			}
		})
	}
}

func TestCreateInstancePayloadJSONCompat(t *testing.T) {
	// 存量 pending 指令只有 image_ref，解析后端口与份数为 0。
	var p fleet.CreateInstancePayload
	if err := json.Unmarshal([]byte(`{"image_ref":"img:1"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.SSHPort != 0 || p.ShareSlots != 0 {
		t.Fatalf("%+v", p)
	}
	b, _ := json.Marshal(fleet.CreateInstancePayload{ImageRef: "img:1", SSHPort: 2223, ShareSlots: 2})
	if string(b) != `{"image_ref":"img:1","ssh_port":2223,"share_slots":2}` {
		t.Fatalf("%s", b)
	}
}

// last_metrics 的结构快照：机器级字段 + instances 数组（无实例时为 []，不是 null）。
func TestMetricsJSONInstancesSnapshot(t *testing.T) {
	m := &agentpb.Metrics{GpuUtil: 12, GpuTempC: 55, GpuPowerW: 30, MemTotal: 100, MemUsed: 40, RootDiskTotal: 1000, RootDiskUsed: 500, DataDiskUsed: 7, Load1: 0.5}
	got, err := metricsJSON(m, []*agentpb.InstanceMetrics{
		{InstanceId: "ins-aaaaaaaa", MemCurrent: 1 << 30, MemMax: 2 << 30, CpuPercent: 150.5, DataDiskUsed: 4096},
		{InstanceId: "ins-bbbbbbbb", CpuPercent: math.NaN()},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"gpu_util":12,"gpu_temp_c":55,"gpu_power_w":30,"mem_total":100,"mem_used":40,"root_disk_total":1000,"root_disk_used":500,` +
		`"data_disk_used":7,"load1":0.5,"instances":[` +
		`{"instance_id":"ins-aaaaaaaa","mem_current":1073741824,"mem_max":2147483648,"cpu_percent":150.5,"data_disk_used":4096},` +
		`{"instance_id":"ins-bbbbbbbb","mem_current":0,"mem_max":0,"cpu_percent":0,"data_disk_used":0}]}`
	if string(got) != want {
		t.Fatalf("metrics json =\n%s\nwant\n%s", got, want)
	}
	// 旧 Agent（心跳不带 instances）→ "instances":[]
	got, err = metricsJSON(&agentpb.Metrics{Load1: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"gpu_util":0,"gpu_temp_c":0,"gpu_power_w":0,"mem_total":0,"mem_used":0,"root_disk_total":0,"root_disk_used":0,"data_disk_used":0,"load1":1,"instances":[]}`; string(got) != want {
		t.Fatalf("old agent metrics json = %s", got)
	}
}

// 心跳 instances 落库有界：实例 ID 不合法的条目跳过，最多 16 条。
func TestMetricsJSONInstancesBounded(t *testing.T) {
	in := []*agentpb.InstanceMetrics{{InstanceId: ""}, {InstanceId: "ins-ABCDEFGH"}, {InstanceId: "ins-abc"}, {InstanceId: "../etc/passwd"},
		{InstanceId: "ins-abcdefgh1"}, {InstanceId: "xins-abcdefgh"}}
	for i := 0; i < 20; i++ {
		in = append(in, &agentpb.InstanceMetrics{InstanceId: fmt.Sprintf("ins-%08d", i), MemCurrent: uint64(i)})
	}
	got, err := metricsJSON(&agentpb.Metrics{}, in)
	if err != nil {
		t.Fatal(err)
	}
	var doc metricsDoc
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Instances) != maxMetricsInstances || maxMetricsInstances != 16 {
		t.Fatalf("instances = %d, want 16", len(doc.Instances))
	}
	for i, x := range doc.Instances {
		if x.InstanceID != fmt.Sprintf("ins-%08d", i) || x.MemCurrent != uint64(i) {
			t.Fatalf("instances[%d] = %+v", i, x)
		}
	}
}

// fromProtoSpec / toProtoNet 是测试用的反向转换（Agent 侧另有自己的实现），只为往返断言。
func fromProtoSpec(p *agentpb.ClusterSpec) clusterspec.Spec {
	s := clusterspec.Spec{ClusterID: p.GetClusterId(), Version: int(p.GetVersion()), Size: int(p.GetSize()), Rank: int(p.GetRank()), Mode: p.GetMode(),
		Iface: p.GetIface(), VLAN: int(p.GetVlan()), MTU: int(p.GetMtu()), Subnet: p.GetSubnet(), RDMA: p.GetRdma()}
	for _, m := range p.GetMembers() {
		s.Members = append(s.Members, clusterspec.Member{Rank: int(m.GetRank()), InstanceID: m.GetInstanceId(), IP: m.GetIp(), HostIP: m.GetHostIp()})
	}
	return s
}

func toProtoNet(n clusterspec.NetInfo) *agentpb.NetInfo {
	out := &agentpb.NetInfo{LanIface: n.LanIface}
	for _, f := range n.Ifaces {
		out.Ifaces = append(out.Ifaces, &agentpb.NetIface{Name: f.Name, Mac: f.MAC, Up: f.Up, SpeedMbps: int32(f.SpeedMbps), Mtu: uint32(f.MTU), Ipv4: f.IPv4,
			Driver: f.Driver, Pci: f.PCI, Wireless: f.Wireless, RdmaDevice: f.RDMADevice, RdmaActive: f.RDMAActive, Uverbs: f.Uverbs, LinkLayer: f.LinkLayer})
	}
	return out
}

func TestConvertClusterSpecRoundTrip(t *testing.T) {
	specs := map[string]clusterspec.Spec{
		"routed 两台": {ClusterID: "clu-aaaaaaaaaa", Version: 3, Size: 2, Rank: 1, Mode: clusterspec.ModeRouted, Iface: "enP7s7", Subnet: "10.210.4.0/24",
			Members: []clusterspec.Member{{Rank: 0, InstanceID: "ins-aaaaaaaa", IP: "10.210.4.11", HostIP: "192.168.1.221"},
				{Rank: 1, InstanceID: "ins-bbbbbbbb", IP: "10.210.4.12", HostIP: "192.168.1.222"}}},
		"macvlan vlan + rdma": {ClusterID: "clu-bbbbbbbbbb", Version: 1, Size: 4, Rank: 0, Mode: clusterspec.ModeMacvlan, Iface: "enp1s0f0np0", VLAN: 120, MTU: 9000,
			Subnet: "10.210.9.0/24", RDMA: true, Members: []clusterspec.Member{{Rank: 0, InstanceID: "ins-cccccccc", IP: "10.210.9.11"},
				{Rank: 1, InstanceID: "ins-dddddddd", IP: "10.210.9.12"}, {Rank: 3, InstanceID: "ins-eeeeeeee", IP: "10.210.9.14"}}},
		"退出集群（空 Spec）": {},
	}
	for name, s := range specs {
		t.Run(name, func(t *testing.T) {
			if got := fromProtoSpec(toProtoSpec(s)); !reflect.DeepEqual(got, s) {
				t.Fatalf("round trip:\n got %+v\nwant %+v", got, s)
			}
		})
	}
	nets := map[string]clusterspec.NetInfo{
		"全字段": {LanIface: "enP7s7", Ifaces: []clusterspec.Iface{
			{Name: "enP7s7", MAC: "aa:bb:cc:dd:ee:01", Up: true, SpeedMbps: 10000, MTU: 1500, IPv4: []string{"192.168.1.221/24", "10.0.0.5/8"}, Driver: "r8127", PCI: "0007:01:00.0"},
			{Name: "wlP9s9", MAC: "aa:bb:cc:dd:ee:02", MTU: 1500, IPv4: []string{}, Driver: "mt7925e", PCI: "0009:01:00.0", Wireless: true},
			{Name: "enp1s0f0np0", MAC: "aa:bb:cc:dd:ee:03", Up: true, SpeedMbps: 200000, MTU: 9000, IPv4: []string{}, Driver: "mlx5_core", PCI: "0000:01:00.0",
				RDMADevice: "rocep1s0f0", RDMAActive: true, Uverbs: "uverbs0", LinkLayer: "Ethernet"}}},
		"无网卡（LanIface 为空）": {Ifaces: []clusterspec.Iface{}},
	}
	for name, n := range nets {
		t.Run(name, func(t *testing.T) {
			got := fromProtoNet(toProtoNet(n))
			if got == nil || !reflect.DeepEqual(*got, n) {
				t.Fatalf("net round trip:\n got %+v\nwant %+v", got, n)
			}
		})
	}
	if fromProtoNet(nil) != nil {
		t.Fatal("nil net must stay nil (old agent)")
	}
	if n := fromProtoNet(&agentpb.NetInfo{Ifaces: []*agentpb.NetIface{{Name: "x", SpeedMbps: -1}}}); n.Ifaces[0].SpeedMbps != 0 || n.Ifaces[0].IPv4 == nil {
		t.Fatalf("negative speed / nil ipv4 = %+v", n.Ifaces[0])
	}
}

func TestConfigureClusterCommandToProto(t *testing.T) {
	spec := clusterspec.Spec{ClusterID: "clu-aaaaaaaaaa", Version: 2, Size: 2, Rank: 0, Mode: clusterspec.ModeRouted, Iface: "enP7s7", Subnet: "10.210.0.0/24",
		Members: []clusterspec.Member{{Rank: 0, InstanceID: "ins-aaaaaaaa", IP: "10.210.0.11"}, {Rank: 1, InstanceID: "ins-bbbbbbbb", IP: "10.210.0.12", HostIP: "10.0.0.2"}}}
	cfg := Config{SSHPort: 2222}
	for name, payload := range map[string]fleet.ConfigureClusterPayload{"新版本": {Cluster: spec}, "退出集群": {}} {
		t.Run(name, func(t *testing.T) {
			b, _ := json.Marshal(payload)
			msg, err := toProtoCommand(fleet.Command{ID: "c9", InstanceID: "ins-aaaaaaaa", Type: fleet.CmdConfigureCluster, Payload: b}, cfg, "")
			if err != nil {
				t.Fatal(err)
			}
			cc := msg.GetConfigureCluster()
			if msg.GetId() != "c9" || msg.GetInstanceId() != "ins-aaaaaaaa" || cc == nil || cc.GetCluster() == nil {
				t.Fatalf("msg = %v", msg)
			}
			if got := fromProtoSpec(cc.GetCluster()); !reflect.DeepEqual(got, payload.Cluster) {
				t.Fatalf("spec = %+v", got)
			}
		})
	}
	// Create：集群成员带 cluster，非集群不带
	msg, err := toProtoCommand(createCmd(t, fleet.CreateInstancePayload{ImageRef: "img:1", SSHPort: 2222, Cluster: &spec}), cfg, "PEM")
	if err != nil || msg.GetCreate().GetCluster() == nil || !reflect.DeepEqual(fromProtoSpec(msg.GetCreate().GetCluster()), spec) {
		t.Fatalf("create with cluster = %v, %v", msg, err)
	}
	msg, _ = toProtoCommand(createCmd(t, fleet.CreateInstancePayload{ImageRef: "img:1", SSHPort: 2222}), cfg, "PEM")
	if msg.GetCreate().GetCluster() != nil {
		t.Fatalf("non-cluster create carries cluster: %v", msg.GetCreate())
	}
	if _, err := toProtoCommand(fleet.Command{ID: "c", Type: fleet.CmdConfigureCluster, Payload: []byte("{")}, cfg, ""); err == nil {
		t.Fatal("bad payload must fail")
	}
}

func TestToStateReportClusterAndNet(t *testing.T) {
	r := toStateReport(&agentpb.StateReport{Instances: []*agentpb.InstanceState{{InstanceId: "ins-aaaaaaaa", Phase: "running", Cluster: "ok"},
		{InstanceId: "ins-bbbbbbbb", Phase: "running"}}, Net: &agentpb.NetInfo{LanIface: "eth0"}})
	if r.Instances[0].Cluster != "ok" || r.Instances[1].Cluster != "" || r.Net == nil || r.Net.LanIface != "eth0" {
		t.Fatalf("report = %+v", r)
	}
	if old := toStateReport(&agentpb.StateReport{}); old.Net != nil {
		t.Fatal("old agent report must have nil Net")
	}
}
