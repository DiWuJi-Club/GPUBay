package clusterspec

import (
	"encoding/json"
	"testing"
)

// sparkNet：标准 Spark——有线管理网卡 enP7s7 + 两个 mlx5 口（一个 up 带 RDMA、一个 down）+ 一个 WiFi。
func sparkNet() NetInfo {
	return NetInfo{LanIface: "enP7s7", Ifaces: []Iface{
		{Name: "enP7s7", MAC: "aa:00:00:00:00:01", Up: true, SpeedMbps: 10000, MTU: 1500, IPv4: []string{"192.168.1.10/24"}, Driver: "r8169", PCI: "0007:01:00.0"},
		{Name: "enp1s0f0np0", MAC: "aa:00:00:00:00:02", Up: false, MTU: 1500, Driver: "mlx5_core", PCI: "0000:01:00.0", RDMADevice: "rocep1s0f0", Uverbs: "uverbs0", LinkLayer: "Ethernet"},
		{Name: "enp1s0f1np1", MAC: "aa:00:00:00:00:03", Up: true, SpeedMbps: 200000, MTU: 9000, Driver: "mlx5_core", PCI: "0000:01:00.1", RDMADevice: "rocep1s0f1", RDMAActive: true, Uverbs: "uverbs1", LinkLayer: "Ethernet"},
		{Name: "wlP9s9", MAC: "aa:00:00:00:00:04", Up: true, MTU: 1500, IPv4: []string{"192.168.2.20/24"}, Driver: "mt7925e", Wireless: true},
		{Name: "enp9s0", MAC: "aa:00:00:00:00:05", Up: true, MTU: 1500, IPv4: []string{"172.16.0.5/16", "172.16.0.6/16"}, Driver: "igc"},
	}}
}

// net221：测试机 221——有线未插线、WiFi 上网，无 CX-7。
func net221() NetInfo {
	return NetInfo{LanIface: "wlP9s9", Ifaces: []Iface{
		{Name: "enP7s7", Up: false, MTU: 1500, Driver: "r8169"},
		{Name: "wlP9s9", Up: true, MTU: 1500, IPv4: []string{"192.168.1.221/24"}, Driver: "mt7925e", Wireless: true},
	}}
}

// mlx5Mgmt：管理网卡恰好是一个 mlx5 口（LanIface 未报）——自动选择必须跳过它。
func mlx5Mgmt() NetInfo {
	return NetInfo{Ifaces: []Iface{
		{Name: "enp1s0f0np0", Up: true, IPv4: []string{"10.9.0.1/16"}, Driver: "mlx5_core"},
		{Name: "enp1s0f1np1", Up: true, Driver: "mlx5_core"},
	}}
}

func TestNetPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.221/24": "192.168.1.0/24",
		"172.16.0.5/16":    "172.16.0.0/16",
		"10.0.0.1/32":      "10.0.0.1/32",
		"10.210.7.0/24":    "10.210.7.0/24",
		"192.168.1.221":    "",
		"":                 "",
		"x/24":             "",
		"fe80::1/64":       "",
		"192.168.1.1/33":   "",
	} {
		if got := NetPrefix(in); got != want {
			t.Errorf("NetPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveIface(t *testing.T) {
	cases := []struct {
		name               string
		n                  NetInfo
		kind, conf, lanIP  string
		wantName, wantHost string
		wantErr            bool
	}{
		{"lan default", sparkNet(), "lan", "", "192.168.1.10", "enP7s7", "192.168.1.10", false},
		{"lan default uses lanIP", sparkNet(), "lan", "", "192.168.1.99", "enP7s7", "192.168.1.99", false},
		{"lan default empty lanIP falls back", sparkNet(), "lan", "", "", "enP7s7", "192.168.1.10", false},
		{"lan wifi ok", net221(), "lan", "", "192.168.1.221", "wlP9s9", "192.168.1.221", false},
		{"lan configured", sparkNet(), "lan", "enp9s0", "192.168.1.10", "enp9s0", "172.16.0.5", false},
		{"lan configured down", net221(), "lan", "enP7s7", "192.168.1.221", "", "", true},
		{"lan configured missing", sparkNet(), "lan", "eth9", "192.168.1.10", "", "", true},
		{"lan configured no ipv4", sparkNet(), "lan", "enp1s0f1np1", "192.168.1.10", "", "", true},
		{"lan no lan iface nor lanIP", NetInfo{Ifaces: sparkNet().Ifaces}, "lan", "", "", "", "", true},
		// Hello 在 AgentConfig 之前发出：LanIface 为空，靠 lanIP 在 CIDR 里认出管理网卡
		{"lan empty LanIface found by lanIP", NetInfo{Ifaces: sparkNet().Ifaces}, "lan", "", "192.168.1.10", "enP7s7", "192.168.1.10", false},
		{"lan empty LanIface wifi by lanIP", NetInfo{Ifaces: net221().Ifaces}, "lan", "", "192.168.1.221", "wlP9s9", "192.168.1.221", false},
		// lanIP 优先于 Agent 报的 LanIface
		{"lan lanIP overrides LanIface", NetInfo{LanIface: "wlP9s9", Ifaces: sparkNet().Ifaces}, "lan", "", "192.168.1.10", "enP7s7", "192.168.1.10", false},
		{"lan configured mgmt by lanIP", NetInfo{Ifaces: sparkNet().Ifaces}, "lan", "enP7s7", "192.168.1.10", "enP7s7", "192.168.1.10", false},
		{"lan second ipv4 not used", NetInfo{Ifaces: sparkNet().Ifaces}, "lan", "enp9s0", "172.16.0.6", "enp9s0", "172.16.0.6", false},
		{"cx7 auto picks first up mlx5", sparkNet(), "cx7", "", "192.168.1.10", "enp1s0f1np1", "", false},
		{"cx7 configured", sparkNet(), "cx7", "enp1s0f1np1", "192.168.1.10", "enp1s0f1np1", "", false},
		{"cx7 configured down", sparkNet(), "cx7", "enp1s0f0np0", "192.168.1.10", "", "", true},
		{"cx7 configured wireless", sparkNet(), "cx7", "wlP9s9", "192.168.1.10", "", "", true},
		{"cx7 configured lan iface", sparkNet(), "cx7", "enP7s7", "192.168.1.10", "", "", true},
		{"cx7 configured mgmt by lanIP", NetInfo{Ifaces: sparkNet().Ifaces}, "cx7", "enP7s7", "192.168.1.10", "", "", true},
		{"cx7 mgmt mlx5 by lanIP skipped", mlx5Mgmt(), "cx7", "", "10.9.0.1", "enp1s0f1np1", "", false},
		{"cx7 none on 221", net221(), "cx7", "", "192.168.1.221", "", "", true},
		{"unknown kind", sparkNet(), "ib", "", "192.168.1.10", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ifc, host, err := ResolveIface(c.n, c.kind, c.conf, c.lanIP)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ResolveIface = %+v, %q, nil; want error", ifc, host)
				}
				if err.Error() == "" {
					t.Fatal("empty error message")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ifc.Name != c.wantName || host != c.wantHost {
				t.Fatalf("ResolveIface = %q, %q; want %q, %q", ifc.Name, host, c.wantName, c.wantHost)
			}
		})
	}
}

// JSON 字段名与 proto 同名（machines.net_info 存这份 JSON）。
func TestNetInfoJSON(t *testing.T) {
	b, err := json.Marshal(NetInfo{LanIface: "eth0", Ifaces: []Iface{{Name: "eth0", Up: true, SpeedMbps: 1000, IPv4: []string{"10.0.0.1/24"}, RDMADevice: "x", RDMAActive: true, Uverbs: "uverbs0", LinkLayer: "Ethernet"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"lan_iface":"eth0","ifaces":[{"name":"eth0","mac":"","up":true,"speed_mbps":1000,"mtu":0,"ipv4":["10.0.0.1/24"],"driver":"","pci":"","wireless":false,"rdma_device":"x","rdma_active":true,"uverbs":"uverbs0","link_layer":"Ethernet"}]}`
	if string(b) != want {
		t.Fatalf("json = %s\nwant  %s", b, want)
	}
	var back NetInfo
	if err := json.Unmarshal(b, &back); err != nil || back.Ifaces[0].Uverbs != "uverbs0" {
		t.Fatalf("round trip: %+v, %v", back, err)
	}
}
