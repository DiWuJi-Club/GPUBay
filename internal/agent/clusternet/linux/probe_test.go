package linux

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/clusterspec"
)

// 221 真机 `ip -4 -o addr show` / `ip -4 route show default` 输出（2026-10-26 采集；当前 DHCP 给的是 /23）。
const addr221 = `1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
7: wlP9s9    inet 192.168.1.221/23 brd 192.168.1.255 scope global dynamic noprefixroute wlP9s9\       valid_lft 470777sec preferred_lft 470777sec
8: spark0    inet 10.200.0.1/24 brd 10.200.0.255 scope global spark0\       valid_lft forever preferred_lft forever
9: docker0    inet 172.17.0.1/16 brd 172.17.255.255 scope global docker0\       valid_lft forever preferred_lft forever
`
const default221 = "default via 192.168.1.1 dev wlP9s9 proto dhcp src 192.168.1.221 metric 600 \n"

func want221(lan string) clusterspec.NetInfo {
	return clusterspec.NetInfo{LanIface: lan, Ifaces: []clusterspec.Iface{
		{Name: "enP7s7", MAC: "4c:bb:47:81:34:79", Up: false, SpeedMbps: 0, MTU: 1500, Driver: "r8127", PCI: "0007:01:00.0"},
		{Name: "wlP9s9", MAC: "50:2e:91:5b:0a:9c", Up: true, SpeedMbps: 0, MTU: 1500, IPv4: []string{"192.168.1.221/23"}, Driver: "mt7925e", PCI: "0009:01:00.0", Wireless: true},
	}}
}

func probe(t *testing.T, sys, addrs, defaults, lanIP string) clusterspec.NetInfo {
	t.Helper()
	r := &cmdrun.Recorder{}
	r.Stub("ip -4 -o addr show", addrs, nil)
	r.Stub("ip -4 route show default", defaults, nil)
	n := New(r, Options{SysRoot: sys, Log: quiet()})
	got, err := n.Probe(context.Background(), lanIP)
	if err != nil {
		t.Fatal(err)
	}
	if r.Lines() != "ip -4 -o addr show\nip -4 route show default" {
		t.Fatalf("probe must only read: %s", r.Lines())
	}
	return got
}

func TestProbe221(t *testing.T) {
	got := probe(t, "testdata/sysfs-221", addr221, default221, "192.168.1.221")
	if !reflect.DeepEqual(got, want221("wlP9s9")) {
		b, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("got %s", b)
	}
}

func TestProbeNoLanIP(t *testing.T) {
	got := probe(t, "testdata/sysfs-221", addr221, default221, "")
	if !reflect.DeepEqual(got, want221("")) {
		b, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("got %s", b)
	}
}

const addrSpark = `1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
2: enP7s7    inet 192.168.1.50/24 brd 192.168.1.255 scope global dynamic noprefixroute enP7s7\       valid_lft 86000sec preferred_lft 86000sec
8: spark0    inet 10.200.0.1/24 brd 10.200.0.255 scope global spark0\       valid_lft forever preferred_lft forever
12: schabcd1234    inet 10.210.3.1/32 scope global schabcd1234\       valid_lft forever preferred_lft forever
`

func TestProbeSparkCX7(t *testing.T) {
	got := probe(t, sparkSys, addrSpark, "default via 192.168.1.1 dev enP7s7 proto dhcp src 192.168.1.50 metric 100 \n", "192.168.1.50")
	want := clusterspec.NetInfo{LanIface: "enP7s7", Ifaces: []clusterspec.Iface{
		{Name: "enP2p1s0f0np0", MAC: "94:6d:ae:00:00:03", MTU: 1500, Driver: "mlx5_core", PCI: "0002:01:00.0", RDMADevice: "roceP2p1s0f0", Uverbs: "uverbs2", LinkLayer: "Ethernet"},
		{Name: "enP2p1s0f1np1", MAC: "94:6d:ae:00:00:04", Up: true, SpeedMbps: 200000, MTU: 9000, Driver: "mlx5_core", PCI: "0002:01:00.1", RDMADevice: "roceP2p1s0f1", RDMAActive: true, Uverbs: "uverbs3", LinkLayer: "Ethernet"},
		{Name: "enP7s7", MAC: "4c:bb:47:81:34:80", Up: true, SpeedMbps: 1000, MTU: 1500, IPv4: []string{"192.168.1.50/24"}, Driver: "r8127", PCI: "0007:01:00.0"},
		{Name: "enp1s0f0np0", MAC: "94:6d:ae:00:00:01", MTU: 1500, Driver: "mlx5_core", PCI: "0000:01:00.0", RDMADevice: "rocep1s0f0", Uverbs: "uverbs0", LinkLayer: "Ethernet"},
		{Name: "enp1s0f1np1", MAC: "94:6d:ae:00:00:02", Up: true, SpeedMbps: 200000, MTU: 9000, Driver: "mlx5_core", PCI: "0000:01:00.1", RDMADevice: "rocep1s0f1", RDMAActive: true, Uverbs: "uverbs1", LinkLayer: "Ethernet"},
		{Name: "wlP9s9", MAC: "50:2e:91:5b:0a:9d", MTU: 1500, Driver: "mt7925e", PCI: "0009:01:00.0", Wireless: true},
	}}
	if !reflect.DeepEqual(got, want) {
		b, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("got %s", b)
	}
	// 与平台口径一致：cx7 自动选中第一个已连接的 mlx5 口
	f, _, err := clusterspec.ResolveIface(got, "cx7", "", "192.168.1.50")
	if err != nil || f.Name != "enP2p1s0f1np1" {
		t.Fatalf("resolve = %v %v", f.Name, err)
	}
}

// 没有 device 链接的虚拟网卡只有持有默认路由时才算（例如 bond / 网桥做管理口）。
func TestProbeVirtualWithDefaultRoute(t *testing.T) {
	got := probe(t, "testdata/sysfs-221", addr221, "default via 10.200.0.254 dev spark0 \ndefault via 172.17.0.254 dev vethfcf0841 metric 900\n", "")
	if len(got.Ifaces) != 2 {
		t.Fatalf("excluded names must stay excluded even with default route: %+v", got.Ifaces)
	}
	sys := t.TempDir()
	for name, content := range map[string]string{"address": "aa:bb:cc:dd:ee:ff", "operstate": "up", "carrier": "1", "mtu": "1500", "speed": "2000"} {
		writeFile(t, filepath.Join(sys, "class", "net", "bond0", name), content+"\n")
		writeFile(t, filepath.Join(sys, "class", "net", "dummy0", name), content+"\n")
	}
	got = probe(t, sys, "3: bond0    inet 192.168.5.5/24 brd 192.168.5.255 scope global bond0\n", "default via 192.168.5.1 dev bond0 \n", "192.168.5.5")
	want := clusterspec.NetInfo{LanIface: "bond0", Ifaces: []clusterspec.Iface{{Name: "bond0", MAC: "aa:bb:cc:dd:ee:ff", Up: true, SpeedMbps: 2000, MTU: 1500, IPv4: []string{"192.168.5.5/24"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bond0: %+v", got)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
