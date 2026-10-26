package clusterspec

import (
	"net/netip"
	"strings"
	"testing"
)

func TestSubnetAndIPs(t *testing.T) {
	pool := netip.MustParsePrefix(DefaultPool)
	sub, err := Subnet(pool, 0)
	if err != nil || sub.String() != "10.210.0.0/24" {
		t.Fatalf("Subnet(0) = %v, %v", sub, err)
	}
	sub7, err := Subnet(pool, 7)
	if err != nil || sub7.String() != "10.210.7.0/24" {
		t.Fatalf("Subnet(7) = %v, %v", sub7, err)
	}
	last, err := Subnet(pool, 255)
	if err != nil || last.String() != "10.210.255.0/24" {
		t.Fatalf("Subnet(255) = %v, %v", last, err)
	}
	if got := MemberIP(sub, 3).String(); got != "10.210.0.14" {
		t.Fatalf("MemberIP(3) = %s", got)
	}
	if got := MemberIP(sub7, 0).String(); got != "10.210.7.11" {
		t.Fatalf("MemberIP(0) = %s", got)
	}
	if got := GatewayIP(sub7).String(); got != "10.210.7.1" {
		t.Fatalf("GatewayIP = %s", got)
	}
	// 未对齐的池按网络地址算
	if s, err := Subnet(netip.MustParsePrefix("10.210.3.4/16"), 1); err != nil || s.String() != "10.210.1.0/24" {
		t.Fatalf("unmasked pool: %v, %v", s, err)
	}
	for _, idx := range []int{-1, 256} {
		if _, err := Subnet(pool, idx); err == nil {
			t.Fatalf("Subnet(%d) accepted", idx)
		}
	}
	for _, p := range []string{"10.210.0.0/24", "10.0.0.0/8", "fd00::/16"} {
		if _, err := Subnet(netip.MustParsePrefix(p), 0); err == nil {
			t.Fatalf("pool %s accepted", p)
		}
	}
}

// validRouted 是 4 台 routed 集群的 rank 1。
func validRouted() Spec {
	return Spec{
		ClusterID: "clu-7f3k2m9q1a", Version: 2, Size: 4, Rank: 1, Mode: ModeRouted, Iface: "enP7s7",
		Subnet: "10.210.7.0/24",
		Members: []Member{
			{Rank: 0, InstanceID: "ins-a1b2c3d4", IP: "10.210.7.11", HostIP: "192.168.1.221"},
			{Rank: 1, InstanceID: "ins-e5f6g7h8", IP: "10.210.7.12", HostIP: "192.168.1.222"},
			{Rank: 2, InstanceID: "ins-i9j0k1l2", IP: "10.210.7.13", HostIP: "192.168.1.223"},
			{Rank: 3, InstanceID: "ins-m3n4o5p6", IP: "10.210.7.14", HostIP: "192.168.1.224"},
		},
	}
}

// validMacvlan 是 2 台 macvlan + VLAN + RDMA 集群的 rank 0。
func validMacvlan() Spec {
	return Spec{
		ClusterID: "clu-0000000001", Version: 1, Size: 2, Rank: 0, Mode: ModeMacvlan, Iface: "enp1s0f1np1",
		VLAN: 120, MTU: 9000, Subnet: "10.210.0.0/24", RDMA: true,
		Members: []Member{
			{Rank: 0, InstanceID: "ins-aaaaaaaa", IP: "10.210.0.11"},
			{Rank: 1, InstanceID: "ins-bbbbbbbb", IP: "10.210.0.12"},
		},
	}
}

func TestSpecValidate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Spec)
		mac  bool // 基于 validMacvlan
		ok   bool
	}{
		{"routed ok", func(*Spec) {}, false, true},
		{"macvlan ok", func(*Spec) {}, true, true},
		{"cluster id bad", func(s *Spec) { s.ClusterID = "clu-ABC" }, false, false},
		{"cluster id empty", func(s *Spec) { s.ClusterID = "" }, false, false},
		{"version 1 ok", func(s *Spec) { s.Version = 1 }, false, true},
		{"version 0", func(s *Spec) { s.Version = 0 }, false, false},
		{"size 8 ok", func(s *Spec) { s.Size = 8 }, false, true},
		{"size 1", func(s *Spec) { s.Size = 1; s.Rank = 0; s.Members = s.Members[:1] }, false, false},
		{"size 9", func(s *Spec) { s.Size = 9 }, false, false},
		{"rank 3 ok", func(s *Spec) { s.Rank = 3 }, false, true},
		{"rank = size", func(s *Spec) { s.Rank = 4 }, false, false},
		{"rank negative", func(s *Spec) { s.Rank = -1 }, false, false},
		{"mode bad", func(s *Spec) { s.Mode = "bridge" }, false, false},
		{"iface 15 ok", func(s *Spec) { s.Iface = "enP2p1s0f1np1ab" }, false, true},
		{"iface 16", func(s *Spec) { s.Iface = "enP2p1s0f1np1abc" }, false, false},
		{"iface injection", func(s *Spec) { s.Iface = "eth0;reboot" }, false, false},
		{"iface empty", func(s *Spec) { s.Iface = "" }, false, false},
		{"vlan 0 ok", func(s *Spec) { s.VLAN = 0 }, true, true},
		{"vlan 4094 ok", func(s *Spec) { s.VLAN = 4094 }, true, true},
		{"vlan 1", func(s *Spec) { s.VLAN = 1 }, true, false},
		{"vlan 4095", func(s *Spec) { s.VLAN = 4095 }, true, false},
		{"mtu 0 ok", func(s *Spec) { s.MTU = 0 }, true, true},
		{"mtu 1280 ok", func(s *Spec) { s.MTU = 1280 }, true, true},
		{"mtu 1000", func(s *Spec) { s.MTU = 1000 }, true, false},
		{"mtu 9217", func(s *Spec) { s.MTU = 9217 }, true, false},
		{"subnet /16", func(s *Spec) { s.Subnet = "10.210.0.0/16" }, false, false},
		{"subnet unmasked", func(s *Spec) { s.Subnet = "10.210.7.5/24" }, false, false},
		{"subnet garbage", func(s *Spec) { s.Subnet = "x" }, false, false},
		{"subnet ipv6", func(s *Spec) { s.Subnet = "fd00::/120" }, false, false},
		{"members missing rank ok", func(s *Spec) { s.Members = append(s.Members[:2:2], s.Members[3]) }, false, true},
		{"members unsorted", func(s *Spec) { s.Members[0], s.Members[1] = s.Members[1], s.Members[0] }, false, false},
		{"members duplicate", func(s *Spec) { s.Members[2] = s.Members[1] }, false, false},
		{"member rank >= size", func(s *Spec) { s.Size = 3 }, false, false},
		{"member ip mismatch", func(s *Spec) { s.Members[2].IP = "10.210.7.20" }, false, false},
		{"member instance id bad", func(s *Spec) { s.Members[2].InstanceID = "x'; rm -rf /" }, false, false},
		{"self missing", func(s *Spec) { s.Members = append(s.Members[:1:1], s.Members[2:]...) }, false, false},
		{"routed peer host ip missing", func(s *Spec) { s.Members[0].HostIP = "" }, false, false},
		{"routed peer host ip v6", func(s *Spec) { s.Members[0].HostIP = "fe80::1" }, false, false},
		{"routed self host ip empty ok", func(s *Spec) { s.Members[1].HostIP = "" }, false, true},
		{"macvlan host ip set", func(s *Spec) { s.Members[1].HostIP = "192.168.1.2" }, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validRouted()
			if c.mac {
				s = validMacvlan()
			}
			c.mut(&s)
			err := s.Validate()
			if c.ok && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !c.ok && err == nil {
				t.Fatal("Validate() = nil, want error")
			}
		})
	}
}

func TestSpecMembers(t *testing.T) {
	s := validRouted()
	if self := s.Self(); self.Rank != 1 || self.InstanceID != "ins-e5f6g7h8" {
		t.Fatalf("Self = %+v", self)
	}
	if h, ok := s.Head(); !ok || h.IP != "10.210.7.11" {
		t.Fatalf("Head = %+v, %v", h, ok)
	}
	peers := s.Peers()
	if len(peers) != 3 || peers[0].Rank != 0 || peers[1].Rank != 2 || peers[2].Rank != 3 {
		t.Fatalf("Peers = %+v", peers)
	}
	s.Members = s.Members[1:]
	if _, ok := s.Head(); ok {
		t.Fatal("Head present without rank 0")
	}
	if p := s.Peers(); len(p) != 2 {
		t.Fatalf("Peers without head = %+v", p)
	}
}

func TestNames(t *testing.T) {
	id := "ins-a1b2c3d4"
	for _, c := range []struct{ got, want string }{
		{HostVeth(id), "scha1b2c3d4"},
		{TempName(id), "scca1b2c3d4"},
		{MacvlanName(id), "scma1b2c3d4"},
		{VLANDev(120), "scv120"},
		{VLANDev(4094), "scv4094"},
		{HostVeth("ab"), "schab"},
	} {
		if c.got != c.want {
			t.Fatalf("name = %q, want %q", c.got, c.want)
		}
		if len(c.got) > 15 {
			t.Fatalf("name %q longer than 15", c.got)
		}
	}
	if !strings.HasPrefix(HostVeth(id), "sch") || len(HostVeth("ins-"+strings.Repeat("z", 30))) > 15 {
		t.Fatal("long id not truncated")
	}
}
